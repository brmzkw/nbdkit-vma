module vma-nbd-poc

go 1.22

require libguestfs.org/nbdkit v0.0.0-00010101000000-000000000000

replace libguestfs.org/nbdkit => ./third_party/nbdkit-golang
