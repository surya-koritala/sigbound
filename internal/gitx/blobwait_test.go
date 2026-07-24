package gitx

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBatchBlobReaderCloseBoundsStuckProcess pins the teardown bound: Close must
// return promptly even against a git that ignores stdin-EOF and never exits on
// its own. The daemon runs on its own cancellable context, and Close arms a
// timer that cancels it (SIGKILL) so cmd.Wait cannot block forever — WaitDelay
// alone is inert on a context that is never cancelled.
//
// Shim: `exec sleep 8` ignores stdin entirely and stays alive 8s. If teardown is
// bounded, Close returns ~2s; a regression (cancel not wired) blocks until the
// child exits ~8s.
func TestBatchBlobReaderCloseBoundsStuckProcess(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "git")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec sleep 8\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := New(dir).WithBinary(shim)
	br, err := g.NewBatchBlobReader()
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan struct{})
	start := time.Now()
	go func() {
		_ = br.Close()
		close(done)
	}()

	select {
	case <-done:
		if el := time.Since(start); el > 4*time.Second {
			t.Fatalf("Close took %v against a stuck git; teardown is not bounded (~2s expected)", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5s against a stuck git — teardown is unbounded")
	}
}
