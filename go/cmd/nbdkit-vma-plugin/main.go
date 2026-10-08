// Command nbdkit-vma-plugin is an nbdkit plugin (built with
// -buildmode=c-shared into a .so) that exposes exactly one device from a
// VMA file as a read-only NBD export, decoding clusters on demand straight
// out of the .vma file. Nothing is ever extracted or cached to a temp file:
// GetReady builds an in-memory cluster index (one linear scan of the extent
// headers), and every PRead after that is a handful of pread(2) calls
// against the original file.
//
// Config parameters:
//
//	vma=PATH      path to the .vma file (required)
//	device=NAME   VMA device name to expose, e.g. "drive-scsi0" (required)
//
// Go plugins can't daemonize (see nbdkit-golang-plugin(3)), so the caller
// must run nbdkit with -f and manage backgrounding itself; see
// mount-vma-disk-via-nbdkit.sh.
package main

import (
	"C"
	"fmt"
	"os"
	"unsafe"

	"libguestfs.org/nbdkit"

	"vma-nbd-poc/internal/vma"
)

var pluginName = "vma"

// Plugin-wide state, populated once in GetReady and read-only afterwards, so
// concurrent connections (nbdkit's golang thread model is always PARALLEL)
// can share it without locking.
var (
	vmaPath    string
	deviceName string

	file   *os.File
	device vma.Device
	index  *vma.ClusterIndex
)

type VMAPlugin struct {
	nbdkit.Plugin
}

type VMAConnection struct {
	nbdkit.Connection
}

func (p *VMAPlugin) Version() string { return "0.1" }

func (p *VMAPlugin) ConfigHelp() string {
	return "vma=FILE      Path to the .vma file (required)\n" +
		"device=NAME   VMA device name to expose, e.g. drive-scsi0 (required)"
}

func (p *VMAPlugin) Config(key, value string) error {
	switch key {
	case "vma":
		vmaPath = value
	case "device":
		deviceName = value
	default:
		return nbdkit.PluginError{Errmsg: fmt.Sprintf("unknown parameter %q", key)}
	}
	return nil
}

func (p *VMAPlugin) ConfigComplete() error {
	if vmaPath == "" {
		return nbdkit.PluginError{Errmsg: "vma parameter is required"}
	}
	if deviceName == "" {
		return nbdkit.PluginError{Errmsg: "device parameter is required"}
	}
	return nil
}

// GetReady opens the vma file, parses its header, resolves the requested
// device, and builds that device's cluster index. This is the only pass
// nbdkit-vma-plugin makes over the file; it runs once at startup, not per
// connection and not per read.
func (p *VMAPlugin) GetReady() error {
	f, err := os.Open(vmaPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", vmaPath, err)
	}

	hdr, err := vma.ParseHeader(f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("parse %s: %w", vmaPath, err)
	}

	dev, ok := hdr.DeviceByName(deviceName)
	if !ok {
		_ = f.Close()
		return fmt.Errorf("device %q not found in %s", deviceName, vmaPath)
	}

	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat %s: %w", vmaPath, err)
	}

	idx, err := vma.BuildClusterIndex(f, fi.Size(), hdr, dev.ID, dev.Size)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("index device %q in %s: %w", deviceName, vmaPath, err)
	}

	file, device, index = f, dev, idx
	return nil
}

func (p *VMAPlugin) Open(readonly bool) (nbdkit.ConnectionInterface, error) {
	return &VMAConnection{}, nil
}

func (c *VMAConnection) GetSize() (uint64, error) {
	return device.Size, nil
}

func (c *VMAConnection) CanMultiConn() (bool, error) {
	return true, nil
}

// CanWrite must be implemented and return false: this plugin only ever reads
// from the backing .vma file, and writing to a device's index would corrupt
// a shared backup archive. nbdkit never calls PWrite when this returns false.
func (c *VMAConnection) CanWrite() (bool, error) {
	return false, nil
}

func (c *VMAConnection) PRead(buf []byte, offset uint64, flags uint32) error {
	_, err := index.ReadAt(file, buf, int64(offset))
	return err
}

//----------------------------------------------------------------------
// Boilerplate required by all nbdkit golang plugins.

//export plugin_init
func plugin_init() unsafe.Pointer {
	return nbdkit.PluginInitialize(pluginName, &VMAPlugin{})
}

func main() {}
