// Command vma lists or mounts the contents of a Proxmox .vma backup,
// without ever extracting a disk to a separate file:
//
//	vma <file.vma> -l [--json]
//	vma <file.vma> -m <device-name> -o <mountpoint>
//
// -l reads only the fixed-size header (devices, sizes, embedded config
// blobs), so it runs instantly regardless of the file's size. -m/-o mount
// the named device at mountpoint and block until interrupted (SIGINT or
// SIGTERM), at which point everything they mounted is cleanly unmounted
// before vma exits; see internal/mount for how that's done.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"vma-fuse-poc/internal/mount"
	"vma-fuse-poc/internal/vma"
)

// errUsage marks a parseArgs failure that should just print usage, with no
// extra explanation (e.g. no arguments at all).
var errUsage = errors.New("usage")

type config struct {
	vmaPath  string
	list     bool
	json     bool
	device   string
	mountDir string
}

func parseArgs(args []string) (*config, error) {
	if len(args) == 0 {
		return nil, errUsage
	}
	vmaPath := args[0]
	if strings.HasPrefix(vmaPath, "-") {
		return nil, errUsage
	}

	fs := flag.NewFlagSet("vma", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	list := fs.Bool("l", false, "list devices and embedded config blobs")
	jsonOut := fs.Bool("json", false, "with -l, print machine-readable JSON instead of a table")
	device := fs.String("m", "", "device name to mount, e.g. drive-scsi0 (see -l)")
	mountDir := fs.String("o", "", "directory to mount the device at (with -m)")
	if err := fs.Parse(args[1:]); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("unexpected argument(s): %v", fs.Args())
	}

	mountMode := *device != "" || *mountDir != ""
	switch {
	case *list && mountMode:
		return nil, errors.New("-l and -m/-o are mutually exclusive")
	case !*list && !mountMode:
		return nil, errUsage
	case mountMode && (*device == "" || *mountDir == ""):
		return nil, errors.New("-m and -o must be given together")
	case *jsonOut && !*list:
		return nil, errors.New("--json only applies with -l")
	}

	return &config{vmaPath: vmaPath, list: *list, json: *jsonOut, device: *device, mountDir: *mountDir}, nil
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  %[1]s <file.vma> -l [--json]
  %[1]s <file.vma> -m <device-name> -o <mountpoint>
`, os.Args[0])
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("vma: ")

	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		if !errors.Is(err, errUsage) {
			fmt.Fprintf(os.Stderr, "error: %v\n\n", err)
		}
		usage()
		os.Exit(2)
	}

	if cfg.list {
		runList(cfg)
		return
	}
	runMount(cfg)
}

func runList(cfg *config) {
	f, err := os.Open(cfg.vmaPath)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	hdr, err := vma.ParseHeader(f)
	if err != nil {
		log.Fatalf("parse %s: %v", cfg.vmaPath, err)
	}

	if cfg.json {
		printJSON(hdr)
		return
	}
	printHuman(cfg.vmaPath, hdr)
}

func runMount(cfg *config) {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()

	if err := mount.Run(ctx, cfg.vmaPath, cfg.device, cfg.mountDir); err != nil {
		log.Fatal(err)
	}
}
