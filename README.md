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
metadata only, typically a few seconds even on a multi-gigabyte file — see
Limitations). Every byte of actual disk content is read from the `.vma` file
exactly when `guestmount`'s filesystem probe, or your own reads inside the
mounted directory, ask for it.

### Anatomy of a single `ls`

Running `ls` inside the mountpoint crosses two separate "machines" (the host
container and a small inner VM) and two Unix sockets before a single byte of
the `.vma` file is actually touched:

```
ls (your shell, in the container)
  │ getdents64()/statx() syscalls against a path under /mnt/<mountpoint>
  ▼
Linux VFS → kernel FUSE module → /dev/fuse
  │ (this is what makes the mountpoint "a filesystem" at all)
  ▼
guestmount (control process, running in the container)
  │ translates the FUSE READDIR/GETATTR request into libguestfs API calls,
  │ sent over a virtio-serial channel (confirmed in this container's own
  │ qemu command line: "-device virtserialport,...guestfsd.sock")
  ▼
guestfsd (daemon inside the libguestfs "appliance" — a real small Linux
kernel + its own QEMU process, booted by guestmount; TCG-emulated here, no
/dev/kvm)
  │ runs ordinary ext4/xfs/NTFS/... syscalls against the filesystem IT
  │ mounted from the guest's virtual disk; the appliance's filesystem driver
  │ issues a block read when it needs a directory/inode block it doesn't
  │ already have cached
  ▼
appliance's own QEMU block layer: a local QCOW2 overlay file, whose backing
file is the NBD Unix socket (confirmed via `qemu-img info` on a live mount:
"backing file: nbd:unix:/run/vma-nbd-mounts/.../nbd.sock") — reads not yet
present in the overlay fall through to the backing NBD connection; any
writes the appliance's own kernel does internally (e.g. journal replay,
access-time updates) land in the overlay only, never in the backing file
  ▼
Unix socket, NBD protocol (NBD_CMD_READ(offset, length))
  ▼
nbdkit (in the container, outside the appliance)
  │ parses the NBD request, dispatches to our plugin's callback
  ▼
nbdkit-vma-plugin's PRead(buf, offset, length)
  │ offset / 65536 → cluster number → one array lookup in the in-memory
  │ ClusterIndex (built once, at mount time, from the extent-header scan)
  │
  ├─ cluster never seen in the stream (fully sparse) → synthesize zeroes,
  │  no file I/O at all
  │
  └─ cluster present → for each 4K sub-block the read touches: present
     (mask bit set) → pread() the real bytes at their exact byte offset in
     the .vma file; absent (mask bit clear — a sparse write inside an
     otherwise-present cluster) → zero-fill, again no file I/O
  ▼
a plain read() against the .vma file — a bind-mounted file, so this is the
point where the request actually leaves the container and is served by the
real file on the host
```

...and the result travels back up the exact same chain: nbdkit → NBD reply
→ appliance's block layer → guest filesystem driver → guestfsd → virtio-
serial → guestmount → FUSE reply → kernel → `ls`'s syscall returns, and `ls`
prints what it got.

A few things worth knowing when reasoning about this:

- **Two independent "brains", not one.** The appliance is the only thing
  that understands ext4/NTFS/LVM/etc; our plugin and nbdkit know nothing
  about guest filesystems, only about where VMA clusters live inside the
  `.vma` file. The appliance is the filesystem logic, our plugin is just its
  block-level data source.
- **Plain `ls` vs `ls -l`/`-la`.** A bare `ls` mostly needs the directory's
  entries (one READDIR-shaped round trip through the whole chain above);
  `ls -l`/`-la` additionally issues a GETATTR (stat) per entry, each one a
  separate round trip, unless already cached (see below).
- **Caching happens at layers our plugin has no part in**: the appliance's
  own page cache (it's a real, if small, Linux kernel), and the kernel FUSE
  attribute/entry cache on the host side (`--dir-cache-timeout`, 5s by
  default in `guestmount`). Nothing is cached on nbdkit's or our plugin's
  side — every `PRead` that isn't absorbed by one of those caches really
  does run the cluster-index lookup above, but we never cache block
  contents ourselves.
- **Metadata reads mostly hit the real-data path, not the zero-fill path.**
  The directories/inodes a filesystem driver actually walks for `ls` are, by
  definition, allocated data, so they're "present" clusters. The zero-fill
  path mostly matters for the much larger unallocated regions of a
  thin-provisioned disk, which nothing touches unless something does a
  full-disk scan.

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

`drive-scsi0` is just an example device name — use whatever
`list-vma-resources.sh` prints for your file. Works for any `.vma` file and
any of its devices. One disk at a time per invocation; run it again with a
different mount dir and device name to inspect another disk.

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
- **In-memory cluster index size** scales with the device's *nominal* virtual
  size, not its actual stored data — thin provisioning doesn't shrink it.
  Each cluster (64KB) costs one 16-byte `clusterEntry` (an `int64` file
  offset + a `uint16` presence mask, padded to 8-byte alignment), held in a
  flat array for the life of the `nbdkit` process. Concretely: a 250GiB
  virtual disk costs ~62MiB of index (built in a few seconds), 1TB → 256MiB,
  4TB → 1GiB, 16TB → 4GiB. This is still far smaller than the disk itself and
  nothing is ever written to storage for it, but at multi-TB scale the memory
  cost is real and the current design has no cap or on-disk/streaming
  fallback — it's an eager, full, in-memory array sized up front.
- **No `/dev/kvm` on Docker Desktop for Apple Silicon** (confirmed: the
  Linux VM backing it exposes no KVM device). `guestmount`'s internal
  libguestfs appliance falls back to QEMU's software (TCG) emulation via
  `LIBGUESTFS_BACKEND_SETTINGS=force_tcg` (a known aarch64-specific
  workaround for a `gic-version=host` bug when KVM is absent). Since the
  appliance runs natively on aarch64 (matching the container, not the
  guest's original x86-64 architecture), this is same-architecture
  emulation, not cross-arch — slower than KVM but not worst-case. In testing,
  a single mount took on the order of tens of seconds with a warm appliance
  cache, regardless of the backed-up disk's actual size (this cost is
  appliance boot time, not proportional to the `.vma` file). On a host with
  `/dev/kvm` (e.g. a Linux machine, or `docker run --device /dev/kvm`), this
  should be several times faster.
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
