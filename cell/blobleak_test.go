package cell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

func fdCount(t *testing.T) int {
	t.Helper()
	f, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatalf("open /dev/fd: %v", err)
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatalf("readdirnames: %v", err)
	}
	return len(names)
}

// writeShim drops an executable shell script and returns its path.
func writeShim(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// countStrayShims counts live processes whose command line mentions shim — any
// survivor after teardown is a leaked/zombie child.
func countStrayShims(t *testing.T, shim string) int {
	t.Helper()
	out, err := exec.Command("ps", "-ax", "-o", "command").Output()
	if err != nil {
		t.Logf("ps failed (skipping stray check): %v", err)
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if line != "" && strings.Contains(line, shim) {
			count++
		}
	}
	return count
}

// TestBlobsBatchFailOpenBrokenGitNoLeak: a git that Start()s fine but exits
// immediately every time (daemon Read EOFs, spawn fallback also fails) must not
// accumulate fds, goroutines, or zombie processes over many calls — a broken
// daemon may only forfeit the speedup, never leak.
func TestBlobsBatchFailOpenBrokenGitNoLeak(t *testing.T) {
	c, base, _ := blobRepo(t)
	shim := writeShim(t, "#!/bin/sh\nexit 0\n")
	c.git = c.git.WithBinary(shim)

	specs := []string{base + ":text.txt"}
	_, _ = c.BlobsBatch(context.Background(), specs) // warm up allocations

	runtime.GC()
	fd0, g0 := fdCount(t), runtime.NumGoroutine()
	const n = 100
	for i := 0; i < n; i++ {
		if _, err := c.BlobsBatch(context.Background(), specs); err == nil {
			t.Fatalf("call %d: expected an error from a wholly broken git", i)
		}
		if c.blob != nil {
			t.Fatalf("call %d: a broken daemon must be discarded, leaving blob nil", i)
		}
	}
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	fd1, g1 := fdCount(t), runtime.NumGoroutine()

	if fd1-fd0 > 5 {
		t.Errorf("fd leak: %d -> %d over %d broken calls", fd0, fd1, n)
	}
	if g1-g0 > 5 {
		t.Errorf("goroutine leak: %d -> %d over %d broken calls", g0, g1, n)
	}
	if stray := countStrayShims(t, shim); stray > 0 {
		t.Errorf("%d stray shim process(es) survived", stray)
	}
}

// wedgeCell builds a repo+cell WITHOUT a t.Cleanup Close (the test drives Close
// explicitly, timing it).
func wedgeCell(t *testing.T) (*Cell, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	g := gitx.New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "text.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c, base
}

// TestBlobsBatchWedgedDaemonHonorsCtx pins the fail-open bound against an
// alive-but-silent daemon: a git that accepts the request and NEVER responds must
// not hang the run. Cancelling the call's ctx must (a) make BlobsBatch return
// promptly — AfterFunc kills the wedged process, its Read errors, the spawn
// fallback fails fast on the cancelled ctx — and (b) release blobMu so a
// concurrent Close is not stalled behind the wedged read.
func TestBlobsBatchWedgedDaemonHonorsCtx(t *testing.T) {
	c, base := wedgeCell(t)
	c.git = c.git.WithBinary(writeShim(t, "#!/bin/sh\ncat >/dev/null\n")) // swallow request, never answer

	ctx, cancel := context.WithCancel(context.Background())
	readDone := make(chan struct{})
	go func() {
		_, _ = c.BlobsBatch(ctx, []string{base + ":text.txt"})
		close(readDone)
	}()

	time.Sleep(300 * time.Millisecond) // let the daemon start and Read block
	cancel()                           // ask the in-flight call to stop

	select {
	case <-readDone:
	case <-time.After(3 * time.Second):
		t.Fatal("BlobsBatch did not return within 3s after ctx cancel — a wedged daemon hangs the run")
	}

	closeDone := make(chan struct{})
	go func() {
		_ = c.Close(context.Background())
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked >3s after a wedged read — blobMu was held across the read")
	}
}
