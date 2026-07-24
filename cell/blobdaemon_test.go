package cell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// blobRepo builds a temp repo whose base commit carries a text blob, an empty
// blob, and a binary blob, then Opens a cell on it. It returns the cell, the
// base SHA, and the file contents so tests can assert byte-equality.
func blobRepo(t *testing.T) (c *Cell, base string, want map[string]string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	g := gitx.New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	want = map[string]string{
		"text.txt":  "hello\nworld\n",
		"empty.txt": "",
		"bin.dat":   string([]byte{0, 1, 2, 0, 255, 10, 0}), // NULs + a newline
	}
	for name, body := range want {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	base, err = g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	c, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c, base, want
}

// TestCellBlobsBatchEqualsSpawn proves the daemon path returns byte-identical
// results to the per-call spawn it replaces — for present blobs (text, empty,
// binary), a missing spec, and a non-blob spec (all absent from the map) — and
// that repeated calls reuse ONE process rather than restarting it.
func TestCellBlobsBatchEqualsSpawn(t *testing.T) {
	ctx := context.Background()
	c, base, want := blobRepo(t)

	specs := []string{
		base + ":text.txt",
		base + ":empty.txt",
		base + ":bin.dat",
		base + ":does-not-exist.txt", // missing => absent
		base,                         // a commit, not a blob => absent
		base + "^{tree}",             // a tree, not a blob => absent
	}

	viaSpawn, err := c.git.BlobsBatch(ctx, specs) // the fallback path, directly
	if err != nil {
		t.Fatalf("spawn BlobsBatch: %v", err)
	}
	viaDaemon, err := c.BlobsBatch(ctx, specs) // through the cell daemon
	if err != nil {
		t.Fatalf("daemon BlobsBatch: %v", err)
	}

	// Present blobs match the on-disk bodies exactly.
	for name, body := range want {
		if got := viaDaemon[base+":"+name]; got != body {
			t.Fatalf("daemon %s = %q, want %q", name, got, body)
		}
	}
	// Empty blob is PRESENT with "" (a real zero-length object), not absent.
	if _, ok := viaDaemon[base+":empty.txt"]; !ok {
		t.Fatal("empty blob must be present in the map, not absent")
	}
	// Missing / non-blob specs are absent from both maps.
	for _, spec := range []string{base + ":does-not-exist.txt", base, base + "^{tree}"} {
		if _, ok := viaDaemon[spec]; ok {
			t.Fatalf("non-blob/missing spec %q must be absent from the map", spec)
		}
	}
	// Daemon and spawn agree map-for-map.
	if len(viaDaemon) != len(viaSpawn) {
		t.Fatalf("map sizes differ: daemon=%d spawn=%d", len(viaDaemon), len(viaSpawn))
	}
	for k, v := range viaSpawn {
		if viaDaemon[k] != v {
			t.Fatalf("daemon[%q]=%q, spawn[%q]=%q", k, viaDaemon[k], k, v)
		}
	}

	// The daemon is reused: the same process backs a second call.
	if c.blob == nil {
		t.Fatal("expected a live daemon after the first call")
	}
	first := c.blob
	if _, err := c.BlobsBatch(ctx, specs); err != nil {
		t.Fatalf("second daemon call: %v", err)
	}
	if c.blob != first {
		t.Fatal("daemon was restarted between calls; it should be reused")
	}
}

// TestCellBlobsBatchConcurrent drives many goroutines through the ONE daemon at
// once. The -race build proves blobMu correctly serializes the sequential wire
// protocol; the content checks prove no goroutine ever gets another's record.
func TestCellBlobsBatchConcurrent(t *testing.T) {
	ctx := context.Background()
	c, base, want := blobRepo(t)

	const goroutines, iters = 16, 40
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				m, err := c.BlobsBatch(ctx, []string{base + ":text.txt", base + ":bin.dat"})
				if err != nil {
					errs <- err
					return
				}
				if m[base+":text.txt"] != want["text.txt"] || m[base+":bin.dat"] != want["bin.dat"] {
					errs <- fmt.Errorf("content mismatch: got text=%q bin=%q", m[base+":text.txt"], m[base+":bin.dat"])
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestCellBlobsBatchFailOpen kills the daemon out from under a live cell (Close
// reaps the process, simulating a wedged/dead daemon) and proves the next call
// still returns correct content via the per-call spawn fallback, then restarts a
// clean daemon on the call after that. A broken daemon must never fail a call.
func TestCellBlobsBatchFailOpen(t *testing.T) {
	ctx := context.Background()
	c, base, want := blobRepo(t)
	specs := []string{base + ":text.txt"}

	// First call starts the daemon.
	if _, err := c.BlobsBatch(ctx, specs); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if c.blob == nil {
		t.Fatal("daemon should be live after the first call")
	}

	// Kill it: reap the process so its pipes are dead. The next Read will error.
	_ = c.blob.Close()

	// Fail-open: the call still succeeds (spawn fallback) with correct content...
	m, err := c.BlobsBatch(ctx, specs)
	if err != nil {
		t.Fatalf("fail-open call errored instead of falling back: %v", err)
	}
	if m[base+":text.txt"] != want["text.txt"] {
		t.Fatalf("fallback content = %q, want %q", m[base+":text.txt"], want["text.txt"])
	}
	// ...and the wedged daemon was discarded.
	if c.blob != nil {
		t.Fatal("a desynced daemon must be discarded, leaving blob nil for restart")
	}

	// The next call restarts a clean daemon and serves from it.
	m, err = c.BlobsBatch(ctx, specs)
	if err != nil {
		t.Fatalf("restart call: %v", err)
	}
	if m[base+":text.txt"] != want["text.txt"] {
		t.Fatalf("restarted daemon content = %q, want %q", m[base+":text.txt"], want["text.txt"])
	}
	if c.blob == nil {
		t.Fatal("daemon should be live again after restart")
	}
}
