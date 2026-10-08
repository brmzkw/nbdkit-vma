package mount

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"

	"vma-fuse-poc/internal/vma"
)

// buildTestVMA writes a minimal synthetic 3-cluster, 1-device VMA file (no
// Proxmox tooling involved) covering the three read paths diskFile.Read must
// handle: a fully-present cluster, a fully-sparse cluster never mentioned in
// the extent stream, and a cluster with only some of its blocks present.
//
// Layout mirrors the real format (see internal/vma doc comments): a
// 12288-byte fixed header, immediately followed by a small blob buffer
// holding just the device name, immediately followed by one extent.
//
// Cluster 0: all 16 blocks present, filled with byte(i).
// Cluster 1: never mentioned in the extent stream -> fully sparse (zero).
// Cluster 2: only blocks 4-7 present, filled with 0x5A.
func buildTestVMA(t *testing.T) (path string, deviceSize int64) {
	t.Helper()

	const (
		headerSize       = 12288
		devInfoOff       = 4096
		clusterSize      = 65536
		blockSize        = 4096
		extentHeaderSize = 512
		blockinfoTabOff  = 40
	)

	buf := make([]byte, headerSize)
	copy(buf[0:4], "VMA\x00")
	binary.BigEndian.PutUint32(buf[4:8], 1) // version

	blobOffset := uint32(headerSize)
	blobData := []byte("testdev")
	blob := make([]byte, 1+2+len(blobData)) // leading pad byte + length prefix + data
	binary.LittleEndian.PutUint16(blob[1:3], uint16(len(blobData)))
	copy(blob[3:], blobData)
	blobSize := uint32(len(blob))

	binary.BigEndian.PutUint32(buf[48:52], blobOffset)
	binary.BigEndian.PutUint32(buf[52:56], blobSize)
	binary.BigEndian.PutUint32(buf[56:60], headerSize+blobSize) // hdr.HeaderSize: where extents start

	deviceSize = 3 * clusterSize
	binary.BigEndian.PutUint32(buf[devInfoOff:devInfoOff+4], 1) // name blob at pos 1
	binary.BigEndian.PutUint64(buf[devInfoOff+8:devInfoOff+16], uint64(deviceSize))

	eh := make([]byte, extentHeaderSize)
	copy(eh[0:4], "VMAE")
	entry := func(i int) []byte { return eh[blockinfoTabOff+i*8 : blockinfoTabOff+(i+1)*8] }

	// blockinfo[0]: cluster 0, all 16 blocks present.
	e0 := entry(0)
	binary.BigEndian.PutUint16(e0[0:2], 0xFFFF)
	e0[3] = 0
	binary.BigEndian.PutUint32(e0[4:8], 0)

	// blockinfo[1]: cluster 2, only blocks 4-7 present (cluster 1 is never
	// mentioned at all, i.e. fully sparse). Remaining entries stay zeroed
	// (mask=0 -> skipped, contribute no data bytes).
	e1 := entry(1)
	binary.BigEndian.PutUint16(e1[0:2], 0x00F0)
	e1[3] = 0
	binary.BigEndian.PutUint32(e1[4:8], 2)

	data0 := make([]byte, clusterSize)
	for i := range data0 {
		data0[i] = byte(i)
	}
	data2 := bytes.Repeat([]byte{0x5A}, 4*blockSize)

	var file bytes.Buffer
	file.Write(buf)
	file.Write(blob)
	file.Write(eh)
	file.Write(data0)
	file.Write(data2)

	dir := t.TempDir()
	path = filepath.Join(dir, "test.vma")
	if err := os.WriteFile(path, file.Bytes(), 0o644); err != nil {
		t.Fatalf("write synthetic vma file: %v", err)
	}
	return path, deviceSize
}

func openTestDiskFile(t *testing.T) (*diskFile, int64) {
	t.Helper()
	path, deviceSize := buildTestVMA(t)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open synthetic vma file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	hdr, err := vma.ParseHeader(f)
	if err != nil {
		t.Fatalf("parse synthetic vma header: %v", err)
	}
	dev, ok := hdr.DeviceByName("testdev")
	if !ok {
		t.Fatalf("testdev not found in synthetic vma header")
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat synthetic vma file: %v", err)
	}
	idx, err := vma.BuildClusterIndex(f, fi.Size(), hdr, dev.ID, dev.Size)
	if err != nil {
		t.Fatalf("build cluster index: %v", err)
	}
	return &diskFile{file: f, idx: idx, size: dev.Size}, deviceSize
}

func TestDiskFileOpen(t *testing.T) {
	df, _ := openTestDiskFile(t)

	// Regression test: without an explicit NodeOpener, go-fuse's default
	// rejects every open(2) with ENOTSUP, which is exactly how this surfaced
	// against a real guestmount/qemu-img run.
	_, _, errno := df.Open(context.TODO(), 0)
	if errno != 0 {
		t.Fatalf("Open returned errno %v", errno)
	}
}

func TestDiskFileGetattr(t *testing.T) {
	df, deviceSize := openTestDiskFile(t)

	var out fuse.AttrOut
	errno := df.Getattr(context.TODO(), nil, &out)
	if errno != 0 {
		t.Fatalf("Getattr returned errno %v", errno)
	}
	if got := out.Size; got != uint64(deviceSize) {
		t.Errorf("Getattr size = %d, want %d", got, deviceSize)
	}
}

func TestDiskFileReadPresentCluster(t *testing.T) {
	df, _ := openTestDiskFile(t)

	dest := make([]byte, 65536)
	res, errno := df.Read(context.TODO(), nil, dest, 0)
	if errno != 0 {
		t.Fatalf("Read returned errno %v", errno)
	}
	got, status := res.Bytes(dest)
	if status != 0 {
		t.Fatalf("ReadResult.Bytes status %v", status)
	}
	for i, b := range got {
		if b != byte(i) {
			t.Fatalf("cluster 0 byte %d = %#x, want %#x", i, b, byte(i))
		}
	}
}

func TestDiskFileReadFullySparseCluster(t *testing.T) {
	df, _ := openTestDiskFile(t)

	dest := make([]byte, 65536)
	res, errno := df.Read(context.TODO(), nil, dest, 65536) // cluster 1, never mentioned
	if errno != 0 {
		t.Fatalf("Read returned errno %v", errno)
	}
	got, status := res.Bytes(dest)
	if status != 0 {
		t.Fatalf("ReadResult.Bytes status %v", status)
	}
	for i, b := range got {
		if b != 0 {
			t.Fatalf("sparse cluster byte %d = %#x, want 0", i, b)
		}
	}
}

func TestDiskFileReadPartiallyPresentCluster(t *testing.T) {
	df, _ := openTestDiskFile(t)

	// Cluster 2 starts at offset 2*65536. Block 0 (first 4096 bytes) is
	// absent -> zero. Blocks 4-7 are present -> our 0x5A pattern.
	dest := make([]byte, 4096)
	res, errno := df.Read(context.TODO(), nil, dest, 2*65536)
	if errno != 0 {
		t.Fatalf("Read returned errno %v", errno)
	}
	got, _ := res.Bytes(dest)
	for i, b := range got {
		if b != 0 {
			t.Fatalf("absent block byte %d = %#x, want 0", i, b)
		}
	}

	dest2 := make([]byte, 4096)
	res2, errno := df.Read(context.TODO(), nil, dest2, 2*65536+4*4096)
	if errno != 0 {
		t.Fatalf("Read returned errno %v", errno)
	}
	got2, _ := res2.Bytes(dest2)
	for i, b := range got2 {
		if b != 0x5A {
			t.Fatalf("present block byte %d = %#x, want 0x5a", i, b)
		}
	}
}
