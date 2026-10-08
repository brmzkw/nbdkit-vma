Vendored from https://gitlab.com/nbdkit/nbdkit tag `v1.36.3` (`plugins/golang/src/libguestfs.org/nbdkit`),
matching the `nbdkit-plugin-dev` version shipped by Ubuntu 24.04 (`1.36.3-1ubuntu10`). BSD-3-Clause, Copyright Red Hat.

Vendored (rather than fetched as a module at build time) because this package's cgo bridge
(`wrappers.h`/`wrappers.go`) must match the ABI of the `nbdkit-plugin.h` header that `nbdkit-plugin-dev`
installs — pinning both to the same release avoids a version skew that `go get` against the `libguestfs.org/nbdkit`
vanity import path can't guarantee. Referenced from `go.mod` via a `replace` directive.

If you bump the Dockerfile's Ubuntu/nbdkit version, re-fetch these files from the matching tag.
