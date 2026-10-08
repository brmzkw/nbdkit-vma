#!/usr/bin/env bash
# List the devices and embedded config blobs in a Proxmox .vma file, without
# extracting anything: it reads only the fixed-size VMA header (a few
# kilobytes), never the extent/data stream, so it runs instantly no matter
# how large the backup is.
#
# Usage:
#   list-vma-resources.sh [--json] <file.vma>
#
# The device names printed here (e.g. "drive-scsi0") are what you pass as
# the disk identifier to mount-vma-disk.sh.
set -euo pipefail

usage() {
    echo "usage: $(basename "$0") [--json] <file.vma>" >&2
    exit 2
}

json_flag=()
if [[ "${1:-}" == "--json" ]]; then
    json_flag=(--json)
    shift
fi

[[ $# -eq 1 ]] || usage
vma_file=$1

if [[ ! -f "$vma_file" ]]; then
    echo "error: no such file: $vma_file" >&2
    exit 1
fi

if ! command -v vma-info >/dev/null 2>&1; then
    cat >&2 <<'EOF'
error: the 'vma-info' helper is missing.

This script relies on a small Go program (built into this Docker image from
go/cmd/vma-info) to parse the VMA header, rather than the Proxmox 'vma' CLI:
that tool is bundled inside Proxmox's patched QEMU package (pve-qemu-kvm) and
is impractical to build standalone outside a full PVE install (it depends on
QEMU's own block layer and several Proxmox-only libraries). If you are seeing
this error, the image was not built correctly -- rebuild it.
EOF
    exit 1
fi

exec vma-info "${json_flag[@]}" "$vma_file"
