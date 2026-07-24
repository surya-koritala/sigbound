package cell

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// catFileProcsFor counts live `git cat-file --batch` processes bound to dir
// (the daemon launches `git -C <dir> cat-file --batch`, so dir appears in argv).
func catFileProcsFor(t *testing.T, dir string) int {
	t.Helper()
	out, err := exec.Command("ps", "-ax", "-o", "command").Output()
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "cat-file") && strings.Contains(line, "--batch") && strings.Contains(line, dir) {
			n++
		}
	}
	return n
}

// TestCloseReapsDaemonNoZombie: after a real daemon has been started, Close must
// leave no surviving `git cat-file --batch` child, and a second Close must be a
// harmless no-op.
func TestCloseReapsDaemonNoZombie(t *testing.T) {
	ctx := context.Background()
	c, base, _ := blobRepo(t) // registers its own Close cleanup — idempotent, fine
	dir := c.Repo()

	if _, err := c.BlobsBatch(ctx, []string{base + ":text.txt"}); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	if c.blob == nil {
		t.Fatal("daemon should be live")
	}
	if got := catFileProcsFor(t, dir); got < 1 {
		t.Fatalf("expected a live cat-file --batch child for %s, ps saw %d", dir, got)
	}

	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c.blob != nil {
		t.Fatal("Close must nil out the daemon handle")
	}
	if got := catFileProcsFor(t, dir); got != 0 {
		t.Fatalf("zombie/leaked cat-file child after Close: ps saw %d for %s", got, dir)
	}

	// Idempotent: a second Close does nothing and does not panic.
	if err := c.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestCloseWithoutDaemonIsNoOp: closing a cell whose daemon was never started
// (no BlobsBatch call) must not panic and must return nil.
func TestCloseWithoutDaemonIsNoOp(t *testing.T) {
	c, _, _ := blobRepo(t)
	if c.blob != nil {
		t.Fatal("no daemon should exist before any BlobsBatch call")
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close on a never-used daemon: %v", err)
	}
}
