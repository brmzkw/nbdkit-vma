package vma

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
)

const (
	clusterSize       = 65536 // bytes per cluster, 16 * blockSize
	blockSize         = 4096  // bytes per block; presence is tracked per block
	blocksPerCluster  = clusterSize / blockSize
	extentMagic       = "VMAE"
	extentHeaderSize  = 512
	blockinfoCount    = 59 // (512 - 40) / 8
	blockinfoSize     = 8
	blockinfoTableOff = 40
)

// clusterEntry records where one cluster's present blocks start in the vma
// file. A zero fileOffset (impossible for a real extent, since extents never
// start at byte 0) means the cluster was never seen in the stream, i.e. it is
// entirely sparse/zero.
type clusterEntry struct {
	fileOffset int64
	mask       uint16
}

// ClusterIndex maps a single device's clusters to their location in the vma
// file. Building one requires exactly one linear scan of the file's extent
// headers (see BuildClusterIndex); after that, reads are O(1) random access
// with no further scanning.
type ClusterIndex struct {
	deviceID int
	entries  []clusterEntry
}

// BuildClusterIndex scans the vma file once, from the end of the header to
// EOF, recording where each cluster belonging to deviceID can be found. It
// reads only the 512-byte extent headers in full; any block data belonging
// to other devices (or to zero/absent blocks) is skipped by computing its
// size from the blockinfo mask, never read. Nothing is written to disk and
// memory use is proportional to the device's cluster count (one small struct
// per cluster), not to the file size.
func BuildClusterIndex(r io.ReaderAt, fileSize int64, hdr *Header, deviceID int, deviceSize uint64) (*ClusterIndex, error) {
	numClusters := (deviceSize + clusterSize - 1) / clusterSize
	idx := &ClusterIndex{
		deviceID: deviceID,
		entries:  make([]clusterEntry, numClusters),
	}

	var eh [extentHeaderSize]byte
	pos := int64(hdr.HeaderSize)
	for pos < fileSize {
		if _, err := io.ReadFull(io.NewSectionReader(r, pos, extentHeaderSize), eh[:]); err != nil {
			return nil, fmt.Errorf("read extent header at offset %d: %w", pos, err)
		}
		if string(eh[0:4]) != extentMagic {
			return nil, fmt.Errorf("corrupt vma stream: expected extent magic %q at offset %d, got %q", extentMagic, pos, eh[0:4])
		}

		dataPos := pos + extentHeaderSize
		for i := range blockinfoCount {
			b := eh[blockinfoTableOff+i*blockinfoSize : blockinfoTableOff+(i+1)*blockinfoSize]
			mask := binary.BigEndian.Uint16(b[0:2])
			devID := int(b[3])
			clusterNum := binary.BigEndian.Uint32(b[4:8])
			dataLen := int64(bits.OnesCount16(mask)) * blockSize

			if mask != 0 && devID == deviceID && uint64(clusterNum) < numClusters {
				idx.entries[clusterNum] = clusterEntry{fileOffset: dataPos, mask: mask}
			}
			dataPos += dataLen
		}
		pos = dataPos
	}

	return idx, nil
}

// Size returns the number of bytes this index covers (the device size,
// rounded up to a whole number of clusters).
func (idx *ClusterIndex) Size() int64 {
	return int64(len(idx.entries)) * clusterSize
}

// ReadAt serves a read of len(p) bytes at byte offset off against the
// device's virtual address space, pulling real bytes from r for present
// blocks and synthesizing zeroes for absent ones. r must be the same vma
// file BuildClusterIndex was run against.
func (idx *ClusterIndex) ReadAt(r io.ReaderAt, p []byte, off int64) (int, error) {
	total := len(p)
	done := 0

	for done < total {
		abs := off + int64(done)
		clusterNum := abs / clusterSize
		clusterOff := abs % clusterSize

		if clusterNum < 0 || clusterNum >= int64(len(idx.entries)) {
			return done, fmt.Errorf("read offset %d is past the end of the device (%d bytes)", abs, idx.Size())
		}

		entry := idx.entries[clusterNum]
		chunk := int64(total - done)
		if remain := int64(clusterSize) - clusterOff; chunk > remain {
			chunk = remain
		}

		if entry.fileOffset == 0 {
			zero(p[done : done+int(chunk)])
			done += int(chunk)
			continue
		}

		n, err := readWithinCluster(r, p[done:done+int(chunk)], entry, clusterOff)
		done += n
		if err != nil {
			return done, err
		}
	}

	return done, nil
}

// readWithinCluster reads `len(dst)` bytes starting at clusterOff (0..65535)
// inside one cluster whose present 4K blocks start at entry.fileOffset and
// are stored back-to-back in ascending block-index order (absent blocks
// contribute no bytes to the file at all).
func readWithinCluster(r io.ReaderAt, dst []byte, entry clusterEntry, clusterOff int64) (int, error) {
	done := 0
	for done < len(dst) {
		blockIdx := uint((clusterOff + int64(done)) / blockSize)
		blockOff := (clusterOff + int64(done)) % blockSize
		take := int64(len(dst) - done)
		if remain := int64(blockSize) - blockOff; take > remain {
			take = remain
		}

		if entry.mask&(1<<blockIdx) == 0 {
			zero(dst[done : done+int(take)])
			done += int(take)
			continue
		}

		precedingPresent := bits.OnesCount16(entry.mask & (1<<blockIdx - 1))
		blockFileOff := entry.fileOffset + int64(precedingPresent)*blockSize + blockOff
		n, err := r.ReadAt(dst[done:done+int(take)], blockFileOff)
		done += n
		if err != nil {
			return done, fmt.Errorf("read device data at vma offset %d: %w", blockFileOff, err)
		}
	}
	return done, nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
