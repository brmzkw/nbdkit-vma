package mount

import (
	"context"
	"os"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"vma-fuse-poc/internal/vma"
)

// rootNode is vma-fuse's own hidden mountpoint's root directory; it holds
// exactly one child, diskFileName, created once up front.
type rootNode struct {
	fs.Inode
	disk *diskFile
}

func (r *rootNode) OnAdd(ctx context.Context) {
	child := r.NewPersistentInode(ctx, r.disk, fs.StableAttr{Mode: syscall.S_IFREG})
	r.AddChild(diskFileName, child, false)
}

var _ fs.NodeOnAdder = (*rootNode)(nil)

// diskFile is the virtual read-only file backing one VMA device: reads are
// served directly from the vma file via idx, nothing is buffered or cached
// here (the kernel's own page cache handles that).
type diskFile struct {
	fs.Inode
	file *os.File
	idx  *vma.ClusterIndex
	size uint64
}

// Open grants every open request: diskFile is immutable and holds all the
// state a read needs already, so there's nothing to allocate per file
// handle. Without this, go-fuse's default (no NodeOpener) rejects every
// open(2) with ENOTSUP -- there is no "optional, skip Open entirely" path
// for a real userspace open() call.
func (f *diskFile) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	return nil, 0, 0
}

func (f *diskFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFREG | 0o444
	out.Size = f.size
	return 0
}

func (f *diskFile) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := f.idx.ReadAt(f.file, dest, off)
	if err != nil && n == 0 {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

var (
	_ fs.NodeOpener    = (*diskFile)(nil)
	_ fs.NodeGetattrer = (*diskFile)(nil)
	_ fs.NodeReader    = (*diskFile)(nil)
)
