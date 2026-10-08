#!/usr/bin/env bash
# Mount one disk out of a Proxmox .vma backup, read-only, without ever
# extracting it from the .vma file.
#
# How it works:
#   1. vma-fuse (go/cmd/vma-fuse) mounts a single virtual file, disk.raw,
#      backed by the selected device's cluster index -- the same on-demand
#      decoding logic used everywhere else in this repo (see
#      go/internal/vma), now served through a small Go FUSE filesystem
#      instead of a block-device protocol.
#   2. guestmount opens that file directly (--format=raw) and mounts the
#      guest filesystem it finds inside (auto-inspecting partitions/LVM/fs
#      type).
#
# Only the bytes guestmount's filesystem probe and your subsequent reads
# actually touch are ever read from the .vma file. Nothing is extracted.
#
# Usage:
#   mount-vma-disk.sh <file.vma> <device-name> <mount-dir>
#   mount-vma-disk.sh --umount <mount-dir>
#
# <device-name> is one of the names printed by list-vma-resources.sh (e.g.
# "drive-scsi0").
set -euo pipefail

STATE_ROOT=/run/vma-fuse-mounts

usage() {
    cat >&2 <<EOF
usage:
  $(basename "$0") <file.vma> <device-name> <mount-dir>
  $(basename "$0") --umount <mount-dir>
EOF
    exit 2
}

# Derive a stable per-mount-dir state directory (vma-fuse's pidfile + its own
# hidden FUSE mountpoint) from the mount dir's absolute path, so a later
# --umount invocation (a separate process) can find them again.
state_dir_for() {
    local mountdir_abs=$1
    local key
    key=$(printf '%s' "$mountdir_abs" | sha256sum | cut -c1-16)
    echo "$STATE_ROOT/$key"
}

check_deps() {
    local missing=()
    command -v vma-fuse >/dev/null 2>&1 || missing+=("vma-fuse (built from go/cmd/vma-fuse)")
    command -v guestmount >/dev/null 2>&1 || missing+=("guestmount (libguestfs-tools)")
    command -v guestunmount >/dev/null 2>&1 || missing+=("guestunmount (libguestfs-tools)")
    command -v vma-info >/dev/null 2>&1 || missing+=("vma-info")

    if ((${#missing[@]} > 0)); then
        echo "error: missing dependencies:" >&2
        printf '  - %s\n' "${missing[@]}" >&2
        echo "(rebuild the Docker image, or run this inside the container it describes)" >&2
        exit 1
    fi

    if [[ ! -c /dev/fuse ]]; then
        cat >&2 <<'EOF'
error: /dev/fuse is not available in this container.

Both vma-fuse and guestmount need FUSE to present their mountpoints. Run the
container with:  --device /dev/fuse --cap-add SYS_ADMIN
(SYS_ADMIN is needed for the mount(2) syscall FUSE and guestmount's internal
appliance perform; it is scoped to this one capability rather than
--privileged -- see README.md for the full explanation.)
EOF
        exit 1
    fi
}

# unmount_fuse_dir unmounts vma-fuse's own hidden mountpoint, falling back to
# a plain fusermount if it's no longer responding (e.g. we had to SIGKILL it).
unmount_fuse_dir() {
    local fuse_dir=$1
    mountpoint -q "$fuse_dir" 2>/dev/null || return 0
    fusermount3 -u "$fuse_dir" 2>/dev/null || fusermount -u "$fuse_dir" 2>/dev/null || true
}

do_umount() {
    local mountdir=$1
    local mountdir_abs
    mountdir_abs=$(readlink -f "$mountdir" 2>/dev/null || echo "$mountdir")
    local state_dir
    state_dir=$(state_dir_for "$mountdir_abs")
    local fuse_dir="$state_dir/fuse"

    local had_error=0

    if mountpoint -q "$mountdir" 2>/dev/null; then
        echo "unmounting $mountdir..."
        if ! guestunmount "$mountdir"; then
            echo "warning: guestunmount failed, trying fusermount3 -u as a fallback" >&2
            fusermount3 -u "$mountdir" || fusermount -u "$mountdir" || had_error=1
        fi
    else
        echo "note: $mountdir is not currently mounted"
    fi

    if [[ -f "$state_dir/vma-fuse.pid" ]]; then
        local pid
        pid=$(cat "$state_dir/vma-fuse.pid")
        if kill -0 "$pid" 2>/dev/null; then
            # SIGTERM asks vma-fuse to unmount fuse_dir itself before
            # exiting; only fall back to SIGKILL (and unmounting fuse_dir
            # ourselves) if it doesn't respond.
            echo "stopping vma-fuse (pid $pid)..."
            kill "$pid" 2>/dev/null || true
            for _ in $(seq 1 50); do
                kill -0 "$pid" 2>/dev/null || break
                sleep 0.1
            done
            if kill -0 "$pid" 2>/dev/null; then
                kill -9 "$pid" 2>/dev/null || true
                unmount_fuse_dir "$fuse_dir"
            fi
        fi
    fi

    rm -rf "$state_dir"

    if ((had_error)); then
        echo "error: cleanup finished with warnings, check manually" >&2
        exit 1
    fi
    echo "done."
}

do_mount() {
    local vma_file=$1 device_name=$2 mountdir=$3

    [[ -f "$vma_file" ]] || { echo "error: no such file: $vma_file" >&2; exit 1; }
    mkdir -p "$mountdir"
    if mountpoint -q "$mountdir" 2>/dev/null; then
        echo "error: $mountdir is already a mountpoint (unmount it first with --umount)" >&2
        exit 1
    fi

    check_deps

    # Fail fast with a clear message (and the list of real device names) if
    # the requested device doesn't exist, before starting any processes.
    local resolved size_bytes dev_id
    if ! resolved=$(vma-info --resolve-device "$device_name" "$vma_file"); then
        exit 1
    fi
    dev_id=${resolved%%$'\t'*}
    size_bytes=${resolved##*$'\t'}
    echo "device '$device_name' (dev_id=$dev_id, $size_bytes bytes) found in $vma_file"

    local mountdir_abs state_dir fuse_dir pidfile
    mountdir_abs=$(readlink -f "$mountdir")
    state_dir=$(state_dir_for "$mountdir_abs")
    fuse_dir="$state_dir/fuse"
    pidfile="$state_dir/vma-fuse.pid"

    mkdir -p "$fuse_dir"

    # Cleanup on any failure between here and the final success return; a
    # successful mount clears this trap before the script exits, because
    # vma-fuse must keep running in the background after we return.
    local vma_fuse_pid=""
    cleanup_on_failure() {
        echo "mount failed, cleaning up..." >&2
        [[ -n "$vma_fuse_pid" ]] && kill "$vma_fuse_pid" 2>/dev/null || true
        sleep 0.2
        unmount_fuse_dir "$fuse_dir"
        rm -rf "$state_dir"
    }
    trap cleanup_on_failure EXIT

    echo "starting vma-fuse..."
    vma-fuse "$vma_file" "$device_name" "$fuse_dir" &
    vma_fuse_pid=$!
    echo "$vma_fuse_pid" > "$pidfile"

    for _ in $(seq 1 100); do
        [[ -e "$fuse_dir/disk.raw" ]] && break
        if ! kill -0 "$vma_fuse_pid" 2>/dev/null; then
            echo "error: vma-fuse exited before mounting its virtual file (see its output above)" >&2
            exit 1
        fi
        sleep 0.1
    done
    if [[ ! -e "$fuse_dir/disk.raw" ]]; then
        echo "error: timed out waiting for vma-fuse to mount $fuse_dir/disk.raw" >&2
        exit 1
    fi

    # Known issue on hosts without /dev/kvm (e.g. Docker Desktop on Apple
    # Silicon): libguestfs's appliance needs this to fall back to QEMU's
    # software (TCG) emulation on aarch64. See README.md "Known limitations".
    echo "starting guestmount (this can take a while without /dev/kvm)..."
    # -o default_permissions routes FUSE permission checks through the kernel's
    # normal generic_permission(), which honours root's CAP_DAC_OVERRIDE.
    # Without it, guestmount's own partial check on chdir(2) denies access
    # based on the guest's raw UID/GID bits even when running as root here
    # (confirmed: plain reads/ls still worked, only chdir was affected) --
    # those guest UIDs don't correspond to anything meaningful on the host.
    if ! LIBGUESTFS_BACKEND_SETTINGS=force_tcg guestmount \
        --format=raw -a "$fuse_dir/disk.raw" \
        -i --ro -o default_permissions \
        "$mountdir"; then
        echo "error: guestmount failed" >&2
        exit 1
    fi

    trap - EXIT
    echo "mounted '$device_name' from $vma_file at $mountdir (read-only)"
    echo "unmount with: $(basename "$0") --umount $mountdir"
}

if [[ "${1:-}" == "--umount" ]]; then
    [[ $# -eq 2 ]] || usage
    do_umount "$2"
elif [[ $# -eq 3 ]]; then
    do_mount "$1" "$2" "$3"
else
    usage
fi
