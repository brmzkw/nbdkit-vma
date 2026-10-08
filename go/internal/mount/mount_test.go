package mount

import "testing"

func TestGuestmountArgs(t *testing.T) {
	args := guestmountArgs("/tmp/fuse/disk.raw", "/mnt/vma-disk")

	want := []string{
		"--format=raw", "-a", "/tmp/fuse/disk.raw",
		"-i", "--ro", "-o", "default_permissions",
		"--no-fork", "/mnt/vma-disk",
	}
	if len(args) != len(want) {
		t.Fatalf("guestmountArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("guestmountArgs[%d] = %q, want %q (full: %v)", i, args[i], want[i], args)
		}
	}

	// --no-fork is what keeps guestmount as our direct child instead of
	// letting it daemonize itself -- Run's cmd.Wait() depends on this.
	found := false
	for _, a := range args {
		if a == "--no-fork" {
			found = true
		}
	}
	if !found {
		t.Errorf("guestmountArgs = %v, missing --no-fork (foreground)", args)
	}
}
