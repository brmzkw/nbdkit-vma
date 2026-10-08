# VMA browsing POC: everything needed to list and mount the contents of a
# Proxmox .vma backup lives in this image. Nothing that touches the .vma
# file (nbdkit, guestmount, guestfish, the Proxmox tooling we stood in for
# with our own parser) needs to be installed on the host.
FROM ubuntu:24.04

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    # nbdkit itself, plus the headers/pkg-config file needed to build our Go
    # plugin against it (nbdkit-plugin.h, nbdkit.pc).
    nbdkit \
    nbdkit-plugin-dev \
    pkg-config \
    # Go toolchain + gcc + libc headers, needed for `go build
    # -buildmode=c-shared` (cgo) -- --no-install-recommends above means gcc
    # alone doesn't pull in libc6-dev's headers (stdlib.h etc).
    golang-go \
    gcc \
    libc6-dev \
    # guestmount/guestfish and the appliance (qemu-system + supermin) they
    # launch internally to introspect the guest filesystem.
    libguestfs-tools \
    # supermin builds its appliance from a real kernel + initrd; a plain
    # container has neither, so we need a kernel package purely to give it
    # something to boot (it is never used to run anything else).
    linux-image-generic \
    # process/mount utilities used by mount-vma-disk-via-nbdkit.sh.
    psmisc \
    fuse3 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Build the Go binaries (vma-info CLI + the nbdkit plugin .so) at image build
# time, so the running container never needs network access or a Go
# toolchain detour to produce them.
COPY go/ /app/go/
RUN cd /app/go \
    && go build -o /usr/local/bin/vma-info ./cmd/vma-info \
    && go build -buildmode=c-shared -o /usr/local/lib/nbdkit-vma-plugin.so ./cmd/nbdkit-vma-plugin

COPY list-vma-resources.sh mount-vma-disk-via-nbdkit.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/list-vma-resources.sh /usr/local/bin/mount-vma-disk-via-nbdkit.sh
