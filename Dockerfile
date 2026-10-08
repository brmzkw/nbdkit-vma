# VMA browsing POC: everything needed to list and mount the contents of a
# Proxmox .vma backup lives in this image. Nothing that touches the .vma
# file (vma-fuse, guestmount, guestfish, the Proxmox tooling we stood in for
# with our own parser) needs to be installed on the host.
FROM ubuntu:24.04

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    # Go toolchain to build vma-info and vma-fuse. Both are pure Go (the
    # go-fuse library needs no cgo), so no gcc/libc headers are needed here.
    golang-go \
    # guestmount/guestfish and the appliance (qemu-system + supermin) they
    # launch internally to introspect the guest filesystem.
    libguestfs-tools \
    # supermin builds its appliance from a real kernel + initrd; a plain
    # container has neither, so we need a kernel package purely to give it
    # something to boot (it is never used to run anything else).
    linux-image-generic \
    # process/mount utilities used by mount-vma-disk.sh.
    psmisc \
    fuse3 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Build the Go binaries (vma-info, vma-fuse) at image build time, so the
# running container never needs network access or a Go toolchain detour to
# produce them.
COPY go/ /app/go/
RUN cd /app/go \
    && go build -o /usr/local/bin/vma-info ./cmd/vma-info \
    && go build -o /usr/local/bin/vma-fuse ./cmd/vma-fuse

COPY list-vma-resources.sh mount-vma-disk.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/list-vma-resources.sh /usr/local/bin/mount-vma-disk.sh
