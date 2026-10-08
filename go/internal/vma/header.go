// Package vma parses the Proxmox VMA backup container format well enough to
// list its contents and serve one device's data on demand, without ever
// extracting a disk to a separate file.
//
// Format reference: verified byte-for-byte against a real vzdump-produced
// .vma file, cross-checked against the reference C implementation's layout
// (git.proxmox.com pve-qemu.git) and against jancc/vma-extractor, an
// independent working Python reimplementation.
//
// Layout summary:
//
//	offset 0..12287   fixed header (all integers big-endian, except blob
//	                  buffer entries below)
//	offset 2044       config_names[256] uint32, offsets into the blob buffer
//	offset 3068       config_data[256]  uint32, offsets into the blob buffer
//	offset 4096       dev_info[256], 32 bytes each: {name_offset u32, _, size u64, _}
//	offset header_size..EOF   a stream of extents (see index.go)
//
// The blob buffer (at blob_buffer_offset, blob_buffer_size bytes) holds one
// leading padding byte, then a sequence of [2-byte little-endian length][that
// many bytes] blobs. Every "offset into the blob buffer" field in the header
// points at the start of one such length prefix. This mixed endianness (header
// big-endian, blob length prefixes little-endian) is a real quirk of the
// format, not a bug: it comes straight from the original C reader, which
// reads the two length bytes individually (low byte, then high byte) rather
// than as a single big-endian field.
package vma

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	headerSize       = 12288
	configCount      = 256
	devInfoCount     = 256
	devInfoEntrySize = 32
	configNamesOff   = 2044
	configDataOff    = 3068
	devInfoOff       = 4096
)

// Device describes one disk (or the special "vmstate" blob) contained in a
// VMA file.
type Device struct {
	ID   int    // dev_id: index into the file's dev_info table, used by extents
	Name string // e.g. "drive-scsi0", or "vmstate" for a live-memory snapshot
	Size uint64 // nominal device size in bytes, as recorded in the header
}

// ConfigBlob is a named configuration blob embedded in the VMA file, e.g.
// the VM's "qemu-server.conf" or "qemu-server.fw".
type ConfigBlob struct {
	Name string
	Data []byte
}

// Header holds everything we parse out of a VMA file's fixed header and blob
// buffer, except the extent/cluster data itself (see BuildClusterIndex).
type Header struct {
	UUID       [16]byte
	CtimeUnix  uint64
	HeaderSize uint32

	Devices []Device
	Configs []ConfigBlob

	devByID map[int]Device
}

// DeviceByName looks up a device by its VMA device name (as shown by
// ParseHeader's Devices list), e.g. "drive-scsi0".
func (h *Header) DeviceByName(name string) (Device, bool) {
	for _, d := range h.Devices {
		if d.Name == name {
			return d, true
		}
	}
	return Device{}, false
}

// DeviceByID looks up a device by its dev_id.
func (h *Header) DeviceByID(id int) (Device, bool) {
	d, ok := h.devByID[id]
	return d, ok
}

// ParseHeader reads and parses the fixed-size VMA header and its associated
// blob buffer. It never reads past header_size, so it is fast and safe to
// call even on a multi-gigabyte file.
func ParseHeader(r io.ReaderAt) (*Header, error) {
	buf := make([]byte, headerSize)
	if _, err := io.ReadFull(io.NewSectionReader(r, 0, headerSize), buf); err != nil {
		return nil, fmt.Errorf("read vma header: %w", err)
	}

	if string(buf[0:4]) != "VMA\x00" {
		return nil, fmt.Errorf("not a vma file: bad magic %q", buf[0:4])
	}
	if version := binary.BigEndian.Uint32(buf[4:8]); version != 1 {
		return nil, fmt.Errorf("unsupported vma version %d (only version 1 is known)", version)
	}

	h := &Header{devByID: map[int]Device{}}
	copy(h.UUID[:], buf[8:24])
	h.CtimeUnix = binary.BigEndian.Uint64(buf[24:32])
	blobOffset := binary.BigEndian.Uint32(buf[48:52])
	blobSize := binary.BigEndian.Uint32(buf[52:56])
	h.HeaderSize = binary.BigEndian.Uint32(buf[56:60])

	var configNames, configData [configCount]uint32
	for i := range configCount {
		configNames[i] = binary.BigEndian.Uint32(buf[configNamesOff+i*4 : configNamesOff+i*4+4])
	}
	for i := range configCount {
		configData[i] = binary.BigEndian.Uint32(buf[configDataOff+i*4 : configDataOff+i*4+4])
	}

	type rawDevInfo struct {
		nameOffset uint32
		size       uint64
	}
	var rawDev [devInfoCount]rawDevInfo
	for i := range devInfoCount {
		off := devInfoOff + i*devInfoEntrySize
		rawDev[i] = rawDevInfo{
			nameOffset: binary.BigEndian.Uint32(buf[off : off+4]),
			size:       binary.BigEndian.Uint64(buf[off+8 : off+16]),
		}
	}

	blobs, err := parseBlobBuffer(r, blobOffset, blobSize)
	if err != nil {
		return nil, fmt.Errorf("parse blob buffer: %w", err)
	}

	for id, raw := range rawDev {
		if raw.size == 0 {
			continue // unused dev_id slot
		}
		name := cString(blobs[raw.nameOffset])
		d := Device{ID: id, Name: name, Size: raw.size}
		h.Devices = append(h.Devices, d)
		h.devByID[id] = d
	}

	for i := range configCount {
		if configNames[i] == 0 {
			continue // unused config slot
		}
		h.Configs = append(h.Configs, ConfigBlob{
			Name: cString(blobs[configNames[i]]),
			Data: blobs[configData[i]],
		})
	}

	return h, nil
}

// parseBlobBuffer reads the blob buffer and splits it into blobs keyed by
// their byte offset within the buffer, matching the offsets used by
// dev_info/config_names/config_data. See the package doc comment for the
// leading-padding-byte and little-endian-length quirks.
func parseBlobBuffer(r io.ReaderAt, offset, size uint32) (map[uint32][]byte, error) {
	buf := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(r, int64(offset), int64(size)), buf); err != nil {
		return nil, err
	}

	blobs := map[uint32][]byte{}
	pos := uint32(1) // one leading padding byte before the first blob
	for pos+2 <= size {
		blobLen := uint32(buf[pos]) | uint32(buf[pos+1])<<8 // little-endian
		dataStart := pos + 2
		if dataStart+blobLen > size {
			break // truncated/corrupt trailing entry; stop rather than panic
		}
		blobs[pos] = buf[dataStart : dataStart+blobLen]
		pos = dataStart + blobLen
	}
	return blobs, nil
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
