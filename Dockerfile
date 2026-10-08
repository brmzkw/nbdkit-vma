# VMA browsing POC: everything needed to list and mount the contents of a
# Proxmox .vma backup lives in this image. Nothing that touches the .vma
# file (vma, guestmount, guestfish, the Proxmox tooling we stood in for
# with our own parser) needs to be installed on the host.
FROM ubuntu:24.04

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    # Go toolchain to build vma. It's pure Go (the go-fuse library needs no
    # cgo), so no gcc/libc headers are needed here.
    golang-go \
    # guestmount/guestfish and the appliance (qemu-system + supermin) they
    # launch internally to introspect the guest filesystem.
    libguestfs-tools \
    # supermin builds its appliance from a real kernel + initrd; a plain
    # container has neither, so we need a kernel package purely to give it
    # something to boot (it is never used to run anything else).
    linux-image-generic \
    fuse3 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Build the vma binary at image build time, so the running container never
# needs network access or a Go toolchain detour to produce it.
COPY go/ /app/go/
RUN cd /app/go && go build -o /usr/local/bin/vma ./cmd/vma
