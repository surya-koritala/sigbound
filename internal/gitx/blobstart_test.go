package gitx

import (
	"os"
	"runtime"
	"testing"
)

func openFDs(t *testing.T) int {
	t.Helper()
	f, err := os.Open("/dev/fd") // macOS/Linux: this process's own fds
	if err != nil {
		t.Fatalf("open /dev/fd: %v", err)
	}
	defer f.Close()
	names, err := f.Readdirnames(-1) // names only — no per-entry stat (macOS-safe)
	if err != nil {
		t.Fatalf("readdirnames /dev/fd: %v", err)
	}
	return len(names)
}

// TestNewBatchBlobReaderStartFailureNoFDLeak stresses the start-failure path: a
// git binary that cannot even Start. NewBatchBlobReader creates a stdin pipe and
// a stdout pipe BEFORE Start; if Start then fails it returns (nil, err) — this
// test proves it does not leak the parent-side pipe fds on that path (which would
// eventually exhaust the process's fd table on a repo with a persistently broken
// git).
func TestNewBatchBlobReaderStartFailureNoFDLeak(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fd counting reads /dev/fd, which does not exist on Windows")
	}
	g := New(t.TempDir()).WithBinary("/nonexistent/git-binary-xyzzy")
	// Prime once so any one-time fd allocation is already counted.
	if _, err := g.NewBatchBlobReader(); err == nil {
		t.Fatal("expected Start to fail for a nonexistent binary")
	}
	before := openFDs(t)
	const n = 300
	for i := 0; i < n; i++ {
		if br, err := g.NewBatchBlobReader(); err == nil {
			_ = br.Close()
			t.Fatal("expected Start failure")
		}
	}
	after := openFDs(t)
	// Zero growth is the pass; allow a tiny slack for runtime bookkeeping.
	if after-before > 5 {
		t.Fatalf("fd leak on Start failure: before=%d after=%d (grew %d over %d calls)", before, after, after-before, n)
	}
	t.Logf("fd count before=%d after=%d over %d failed starts", before, after, n)
}
