# VMA browsing POC

Demonstrates reading and mounting the contents of a Proxmox `.vma` backup
without ever extracting a disk to a separate file, entirely inside Docker.
Meant to inform a future `plakar mount` integration for browsing VMA backups.

## How it works

A `.vma` file is a container: a 12KB header (device list, sizes, embedded VM
config) followed by a stream of "extents" holding the actual disk data,
interleaved across devices in no particular order. There is no way to seek
directly to "byte N of disk X" — you first need an index mapping that disk's
clusters to file offsets, built by one lightweight pass over the extent
headers (not the data itself).

Nothing here uses the Proxmox `vma` CLI: it's bundled inside Proxmox's
patched QEMU (`pve-qemu-kvm`) and isn't practical to build standalone outside
a full PVE install. Instead, `go/internal/vma` is a from-scratch VMA parser
(verified byte-for-byte against a real backup file and cross-checked against
the reference C implementation and an independent Python reimplementation,
[jancc/vma-extractor](https://github.com/jancc/vma-extractor)).

Two Go programs are built from it at image build time:

- **`vma-info`** (`go/cmd/vma-info`): parses just the header to list devices,
  sizes, and embedded config blobs. Used by `list-vma-resources.sh`.
- **`nbdkit-vma-plugin.so`** (`go/cmd/nbdkit-vma-plugin`): an
  [nbdkit](https://gitlab.com/nbdkit/nbdkit) Go plugin. On load it builds an
  in-memory cluster index for one selected device (one scan of the extent
  headers), then serves arbitrary byte-range reads against it by seeking
  directly into the `.vma` file — decoding clusters on demand, never
  extracting the disk. nbdkit has no plugin for VMA, so this plugin is itself
  the "point critique" this POC had to resolve; it's novel as far as we found.

`mount-vma-disk-via-nbdkit.sh` starts `nbdkit` with that plugin, serving the
selected device over a Unix-socket NBD export, then points `guestmount`
(libguestfs) at it. `guestmount -i` auto-inspects the guest (partition table,
LVM, filesystem type) and mounts the real root filesystem read-only.

**What's read on demand vs. extracted: nothing is ever extracted.** The only
non-trivial read ahead of time is the one-time cluster-index scan (header
metadata only, a few seconds even on an ~12GB file — see Limitations). Every
byte of actual disk content is read from the `.vma` file exactly when
`guestmount`'s filesystem probe, or your own reads inside the mounted
directory, ask for it.

## Host dependencies

Docker. That's it — `nbdkit`, `guestmount`/`guestfish`, `qemu-nbd`-equivalent
tooling, and anything Proxmox-specific all live inside the image, per the
project's constraint.

## Building and running

```sh
make run
```

equivalent to:

```sh
docker build -t test-proxmox .
docker run --rm -ti --init -v .:/app --device /dev/fuse --cap-add SYS_ADMIN test-proxmox bash
```

This drops you into a long-lived container with this directory bind-mounted
at `/app`. From there, or via `docker exec` from another terminal against a
container started the same way (see `Makefile`), run the two scripts as many
times as you like.

### List the disks in a `.vma` file

```sh
list-vma-resources.sh /app/your-backup.vma
list-vma-resources.sh --json /app/your-backup.vma   # machine-readable
```

Instant regardless of file size — it only reads the fixed header.

### Mount a disk

```sh
mkdir -p /mnt/vma-disk
mount-vma-disk-via-nbdkit.sh /app/your-backup.vma drive-scsi0 /mnt/vma-disk
```

`drive-scsi0` is the device name from `list-vma-resources.sh`'s output — this
works for any `.vma` file and any of its devices, not just the sample backup
used to build this POC. One disk at a time per invocation; run it again with
a different mount dir and device name to inspect another disk.

### Unmount

```sh
mount-vma-disk-via-nbdkit.sh --umount /mnt/vma-disk
```

Unmounts, stops `nbdkit`, and removes its socket/state directory
(`/run/vma-nbd-mounts/<hash of the mount dir>` inside the container).

## Docker privileges, explained

- **`--device /dev/fuse`**: `guestmount` presents the guest filesystem via
  FUSE; without this device node, FUSE mounts are unavailable in the
  container.
- **`--cap-add SYS_ADMIN`**: `mount(2)` (which FUSE mounting goes through)
  requires this capability. We did not need `--privileged` or `/dev/nbd*` —
  the NBD export in this design is a Unix socket consumed directly by
  libguestfs/qemu's own NBD client, never the kernel's `/dev/nbd` block
  device, so no block-device access or kernel NBD module is involved at all.
- **`--init`**: not strictly about guestmount, but needed for cleanliness —
  without a real init as PID 1, a container has nothing to reap `nbdkit`
  once `--umount` kills it, leaving a zombie process entry for the life of
  the container. `docker run --init` (Docker's built-in `tini`) fixes this.

## Known limitations

- **Read-only.** No write path exists or is planned for this POC.
- **One disk mounted at a time** (per design decision — see conversation
  history). Mount a second disk by running the script again with a
  different mount dir; each gets its own `nbdkit` process and socket.
- **In-memory cluster index size** scales with the device's *nominal* size,
  not its actual stored data (one small struct per 64KB cluster — e.g. a
  250GiB disk is ~4M clusters, a few dozen MB of index, built in a few
  seconds). A multi-TB thin-provisioned disk would have a proportionally
  larger index; still far smaller than the disk itself, and nothing is
  written to disk for it.
- **No `/dev/kvm` on Docker Desktop for Apple Silicon** (confirmed: the
  Linux VM backing it exposes no KVM device). `guestmount`'s internal
  libguestfs appliance falls back to QEMU's software (TCG) emulation via
  `LIBGUESTFS_BACKEND_SETTINGS=force_tcg` (a known aarch64-specific
  workaround for a `gic-version=host` bug when KVM is absent). Since the
  appliance runs natively on aarch64 (matching the container, not the
  guest's original x86-64 architecture), this is same-architecture
  emulation, not cross-arch — slower than KVM but not worst-case. Measured
  on the 11.84GB sample file: ~40s per mount with a warm appliance cache.
  On a host with `/dev/kvm` (e.g. a Linux machine, or `docker run
  --device /dev/kvm`), this should be several times faster.
- **First mount in a fresh container is slower**: libguestfs builds and
  caches a ~450MB "supermin appliance" (a minimal kernel+initrd it uses
  internally) on first use, under `/var/tmp/.guestfs-*` in the container's
  writable layer. This is a one-time cost per container lifetime, unrelated
  to the size of any particular `.vma` file, and is not part of what gets
  read from the backup.
- **Guest filesystem type/layout is only known once mounted.**
  `list-vma-resources.sh` reports what's in the VMA header (devices, sizes,
  VM config); it cannot tell you whether a given disk is ext4, NTFS, uses
  LVM, etc., since that requires reading guest data — which is exactly what
  `guestmount -i`'s auto-inspection does.
- **Guest file ownership/permissions are preserved as-is.** A file owned by
  UID 1000 inside the guest is still reported as UID 1000 under the mount —
  these guest UIDs don't correspond to anything on the host. `-o
  default_permissions` is passed to `guestmount` so the container's root
  (which has `CAP_DAC_OVERRIDE`) can still read/traverse everything
  regardless; without that flag, `chdir(2)` into a guest directory your
  (host) UID doesn't own is denied even as root, while plain file reads and
  `ls` oddly still work — an inconsistency in how libguestfs/FUSE partially
  enforce permissions without `default_permissions`. A non-root user inside
  the container would still be bound by the guest's real permission bits.
- **nbdkit Go plugins can't daemonize** (an nbdkit-golang-plugin constraint,
  not ours); `mount-vma-disk-via-nbdkit.sh` runs `nbdkit -f` and backgrounds
  it itself, tracking the PID for `--umount`.
- The nbdkit Go SDK (`go/third_party/nbdkit-golang`) is vendored from the
  exact nbdkit release (`v1.36.3`) that Ubuntu 24.04 ships, rather than
  fetched as a module at build time — its cgo bridge must match the
  `nbdkit-plugin.h` ABI of whatever `nbdkit-plugin-dev` installs. Bumping
  the Ubuntu/nbdkit version in the `Dockerfile` means re-vendoring from the
  matching tag (see `go/third_party/nbdkit-golang/VENDORED.md`).
