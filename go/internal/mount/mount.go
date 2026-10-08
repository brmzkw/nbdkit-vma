// Package mount mounts one device from a VMA file at a directory, blocking
// until the context passed to Run is cancelled. It is the seam between the
// CLI (cmd/vma) and whatever ends up embedding this as a goroutine instead
// of a separate process.
//
// Nothing is ever extracted or cached to a temp file: Run builds an
// in-memory cluster index (one linear scan of the extent headers), exposes
// it as a single virtual file through a small FUSE filesystem of its own
// (disk.raw, under a private temp directory), and points guestmount's
// "file" block driver straight at that file. Every read after that is a
// handful of pread(2) calls against the original .vma file.
package mount

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"vma-fuse-poc/internal/vma"
)

// diskFileName is the single file exposed inside vma-fuse's own (hidden)
// mountpoint; guestmount is pointed at <fuseDir>/<diskFileName>.
const diskFileName = "disk.raw"

// unmountGrace bounds how long Run waits for a clean unmount (of either its
// own hidden FUSE mount, or guestmount's) before giving up and returning
// anyway; cleanup is best-effort once the caller has asked to stop.
const unmountGrace = 10 * time.Second

// Run mounts device from the vma file at mountDir, blocking until ctx is
// cancelled or guestmount exits on its own (crash, or someone unmounting
// mountDir directly). Either way, Run unmounts everything it mounted before
// returning. mountDir is created if it doesn't exist, and must not already
// be a mountpoint.
func Run(ctx context.Context, vmaPath, device, mountDir string) error {
	if err := checkDeps(); err != nil {
		return err
	}

	file, idx, dev, err := openIndex(vmaPath, device)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", mountDir, err)
	}
	if isMountpoint(mountDir) {
		return fmt.Errorf("%s is already a mountpoint", mountDir)
	}

	fuseDir, err := os.MkdirTemp("", "vma-fuse-*")
	if err != nil {
		return fmt.Errorf("create fuse mount dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(fuseDir) }()

	root := &rootNode{disk: &diskFile{file: file, idx: idx, size: dev.Size}}
	server, err := fs.Mount(fuseDir, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:  "vma-fuse",
			Name:    "vma",
			Options: []string{"ro"},
		},
	})
	if err != nil {
		return fmt.Errorf("mount fuse at %s: %w", fuseDir, err)
	}
	defer unmountRetry(server)

	cmd := exec.Command("guestmount", guestmountArgs(filepath.Join(fuseDir, diskFileName), mountDir)...)
	cmd.Env = append(os.Environ(), "LIBGUESTFS_BACKEND_SETTINGS=force_tcg")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start guestmount: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		unmountGuestmount(mountDir)
		waitOrKill(cmd, done)
		return nil
	case err := <-done:
		// guestmount exited on its own; make sure its mount is really
		// gone (e.g. it crashed without unmounting cleanly).
		unmountGuestmount(mountDir)
		return err
	}
}

// guestmountArgs builds guestmount's argument list. --no-fork keeps it in
// the foreground as our direct child (so cmd.Wait above actually brackets
// its lifetime) rather than letting it daemonize itself.
func guestmountArgs(diskPath, mountDir string) []string {
	return []string{
		"--format=raw", "-a", diskPath,
		"-i", "--ro", "-o", "default_permissions",
		"--no-fork", mountDir,
	}
}

func unmountGuestmount(mountDir string) {
	if err := exec.Command("guestunmount", mountDir).Run(); err != nil {
		_ = exec.Command("fusermount3", "-u", mountDir).Run()
	}
}

func waitOrKill(cmd *exec.Cmd, done <-chan error) {
	select {
	case <-done:
	case <-time.After(unmountGrace):
		_ = cmd.Process.Kill()
		<-done
	}
}

// unmountRetry unmounts vma-fuse's own hidden mountpoint, retrying for a
// while: whatever last had disk.raw open (guestmount's backing qemu
// process) may take a moment to close it after guestunmount returns, which
// makes an immediate Unmount fail with EBUSY. Without the retry, the
// mountpoint is left stuck as "Transport endpoint is not connected".
func unmountRetry(server *fuse.Server) {
	deadline := time.Now().Add(unmountGrace)
	for server.Unmount() != nil {
		if time.Now().After(deadline) {
			return // best-effort; os.RemoveAll(fuseDir) may still fail, that's fine
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func checkDeps() error {
	var missing []string
	for _, bin := range []string{"guestmount", "guestunmount", "mountpoint"} {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing dependencies: %v (install libguestfs-tools, util-linux)", missing)
	}
	if fi, err := os.Stat("/dev/fuse"); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("/dev/fuse is not available (run the container with --device /dev/fuse --cap-add SYS_ADMIN)")
	}
	return nil
}

func isMountpoint(dir string) bool {
	return exec.Command("mountpoint", "-q", dir).Run() == nil
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
		return nil, nil, vma.Device{}, fmt.Errorf("device %q not found in %s (available: %v)", deviceName, vmaPath, deviceNames(hdr))
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

func deviceNames(hdr *vma.Header) []string {
	names := make([]string, len(hdr.Devices))
	for i, d := range hdr.Devices {
		names[i] = d.Name
	}
	return names
}
