// Command vma-fuse is a FUSE filesystem that exposes exactly one device from
// a VMA file as a single read-only virtual raw file, decoding clusters on
// demand straight out of the .vma file. Nothing is ever extracted or cached
// to a temp file: main builds an in-memory cluster index (one linear scan of
// the extent headers), and every Read after that is a handful of pread(2)
// calls against the original file.
//
// Usage:
//
//	vma-fuse <file.vma> <device-name> <mountpoint>
//
// The mountpoint must already exist and not be in use. vma-fuse mounts a
// single file, <mountpoint>/disk.raw, and runs in the foreground until the
// mount is torn down (SIGINT/SIGTERM, or an external unmount of mountpoint);
// see mount-vma-disk.sh for how it's started and stopped.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"vma-fuse-poc/internal/vma"
)

// diskFileName is the single file exposed inside the FUSE mountpoint;
// mount-vma-disk.sh points guestmount at <mountpoint>/<diskFileName>.
const diskFileName = "disk.raw"

func main() {
	log.SetFlags(0)
	log.SetPrefix("vma-fuse: ")

	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "usage: %s <file.vma> <device-name> <mountpoint>\n", os.Args[0])
		os.Exit(2)
	}
	vmaPath, deviceName, mountpoint := os.Args[1], os.Args[2], os.Args[3]

	file, idx, dev, err := openIndex(vmaPath, deviceName)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	root := &rootNode{disk: &diskFile{file: file, idx: idx, size: dev.Size}}
	server, err := fs.Mount(mountpoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:     "vma-fuse",
			Name:       "vma",
			AllowOther: false,
			Options:    []string{"ro"},
		},
	})
	if err != nil {
		log.Fatalf("mount %s: %v", mountpoint, err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		// Whatever last had disk.raw open (guestmount's backing qemu
		// process) may take a moment to close it after guestunmount
		// returns, which makes an immediate Unmount fail with EBUSY;
		// retry until it succeeds rather than leaving mountpoint stuck
		// in "Transport endpoint is not connected".
		for server.Unmount() != nil {
			time.Sleep(100 * time.Millisecond)
		}
	}()

	server.Wait()
}

func openIndex(vmaPath, deviceName string) (*os.File, *vma.ClusterIndex, vma.Device, error) {
	f, err := os.Open(vmaPath)
	if err != nil {
		return nil, nil, vma.Device{}, fmt.Errorf("open %s: %w", vmaPath, err)
	}

	hdr, err := vma.ParseHeader(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, vma.Device{}, fmt.Errorf("parse %s: %w", vmaPath, err)
	}

	dev, ok := hdr.DeviceByName(deviceName)
	if !ok {
		_ = f.Close()
		return nil, nil, vma.Device{}, fmt.Errorf("device %q not found in %s", deviceName, vmaPath)
	}

	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, vma.Device{}, fmt.Errorf("stat %s: %w", vmaPath, err)
	}

	idx, err := vma.BuildClusterIndex(f, fi.Size(), hdr, dev.ID, dev.Size)
	if err != nil {
		_ = f.Close()
		return nil, nil, vma.Device{}, fmt.Errorf("index device %q in %s: %w", deviceName, vmaPath, err)
	}

	return f, idx, dev, nil
}

// rootNode is the FUSE mountpoint's root directory; it holds exactly one
// child, diskFileName, created once up front.
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
