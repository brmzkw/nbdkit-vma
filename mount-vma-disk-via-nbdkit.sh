#!/usr/bin/env bash
# Mount one disk out of a Proxmox .vma backup, read-only, without ever
# extracting it from the .vma file.
#
# How it works:
#   1. nbdkit serves the selected device over a Unix-socket NBD export,
#      using a Go plugin (go/cmd/nbdkit-vma-plugin) that decodes VMA clusters
#      on demand straight out of the .vma file (see go/internal/vma).
#   2. guestmount connects to that NBD export and mounts the guest
#      filesystem it finds inside (auto-inspecting partitions/LVM/fs type).
#
# Only the bytes guestmount's filesystem probe and your subsequent reads
# actually touch are ever read from the .vma file. Nothing is extracted.
#
# Usage:
#   mount-vma-disk-via-nbdkit.sh <file.vma> <device-name> <mount-dir>
#   mount-vma-disk-via-nbdkit.sh --umount <mount-dir>
#
# <device-name> is one of the names printed by list-vma-resources.sh (e.g.
# "drive-scsi0").
set -euo pipefail

PLUGIN_SO=/usr/local/lib/nbdkit-vma-plugin.so
STATE_ROOT=/run/vma-nbd-mounts

usage() {
    cat >&2 <<EOF
usage:
  $(basename "$0") <file.vma> <device-name> <mount-dir>
  $(basename "$0") --umount <mount-dir>
EOF
    exit 2
}

# Derive a stable per-mount-dir state directory (nbdkit's pidfile + socket)
# from the mount dir's absolute path, so a later --umount invocation (a
# separate process) can find them again.
state_dir_for() {
    local mountdir_abs=$1
    local key
    key=$(printf '%s' "$mountdir_abs" | sha256sum | cut -c1-16)
    echo "$STATE_ROOT/$key"
}

check_deps() {
    local missing=()
    command -v nbdkit >/dev/null 2>&1 || missing+=("nbdkit")
    command -v guestmount >/dev/null 2>&1 || missing+=("guestmount (libguestfs-tools)")
    command -v guestunmount >/dev/null 2>&1 || missing+=("guestunmount (libguestfs-tools)")
    command -v vma-info >/dev/null 2>&1 || missing+=("vma-info")
    [[ -f "$PLUGIN_SO" ]] || missing+=("$PLUGIN_SO (nbdkit Go plugin, built from go/cmd/nbdkit-vma-plugin)")

    if ((${#missing[@]} > 0)); then
        echo "error: missing dependencies:" >&2
        printf '  - %s\n' "${missing[@]}" >&2
        echo "(rebuild the Docker image, or run this inside the container it describes)" >&2
        exit 1
    fi

    if [[ ! -c /dev/fuse ]]; then
        cat >&2 <<'EOF'
error: /dev/fuse is not available in this container.

guestmount needs FUSE to present the guest filesystem as a mountpoint. Run
the container with:  --device /dev/fuse --cap-add SYS_ADMIN
(SYS_ADMIN is needed for the mount(2) syscall FUSE and guestmount's internal
appliance perform; it is scoped to this one capability rather than
--privileged -- see README.md for the full explanation.)
EOF
        exit 1
    fi
}

do_umount() {
    local mountdir=$1
    local mountdir_abs
    mountdir_abs=$(readlink -f "$mountdir" 2>/dev/null || echo "$mountdir")
    local state_dir
    state_dir=$(state_dir_for "$mountdir_abs")

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

    if [[ -f "$state_dir/nbdkit.pid" ]]; then
        local pid
        pid=$(cat "$state_dir/nbdkit.pid")
        if kill -0 "$pid" 2>/dev/null; then
            echo "stopping nbdkit (pid $pid)..."
            kill "$pid" 2>/dev/null || true
            for _ in $(seq 1 50); do
                kill -0 "$pid" 2>/dev/null || break
                sleep 0.1
            done
            kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
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

    local mountdir_abs state_dir sock pidfile
    mountdir_abs=$(readlink -f "$mountdir")
    state_dir=$(state_dir_for "$mountdir_abs")
    sock="$state_dir/nbd.sock"
    pidfile="$state_dir/nbdkit.pid"

    mkdir -p "$state_dir"
    rm -f "$sock"

    # Cleanup on any failure between here and the final success return; a
    # successful mount clears this trap before the script exits, because
    # nbdkit must keep running in the background after we return.
    local nbdkit_pid=""
    cleanup_on_failure() {
        echo "mount failed, cleaning up..." >&2
        [[ -n "$nbdkit_pid" ]] && kill "$nbdkit_pid" 2>/dev/null || true
        rm -rf "$state_dir"
    }
    trap cleanup_on_failure EXIT

    # nbdkit's golang plugin support can't daemonize (see
    # nbdkit-golang-plugin(3)), so we run nbdkit in the foreground (-f) and
    # background it ourselves. -r enforces read-only at the NBD level, on
    # top of the plugin's own CanWrite=false.
    echo "starting nbdkit..."
    nbdkit -f -r -U "$sock" "$PLUGIN_SO" "vma=$vma_file" "device=$device_name" &
    nbdkit_pid=$!
    echo "$nbdkit_pid" > "$pidfile"

    for _ in $(seq 1 100); do
        [[ -S "$sock" ]] && break
        if ! kill -0 "$nbdkit_pid" 2>/dev/null; then
            echo "error: nbdkit exited before creating its socket (see its output above)" >&2
            exit 1
        fi
        sleep 0.1
    done
    if [[ ! -S "$sock" ]]; then
        echo "error: timed out waiting for nbdkit's socket at $sock" >&2
        exit 1
    fi

    # Known issue on hosts without /dev/kvm (e.g. Docker Desktop on Apple
    # Silicon): libguestfs's appliance needs this to fall back to QEMU's
    # software (TCG) emulation on aarch64. See README.md "Known limitations".
    echo "starting guestmount (this can take a while without /dev/kvm)..."
    if ! LIBGUESTFS_BACKEND_SETTINGS=force_tcg guestmount \
        --format=raw -a "nbd://?socket=$sock" \
        -i --ro \
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
