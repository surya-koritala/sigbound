//go:build unix

package main

import "testing"

// TestStatfsFreeRealFilesystem is the sanity check for statfsFree itself
// (diskspace_unix.go): against a real, existing directory it must report
// ok=true and a plausible (nonzero) free-byte count.
func TestStatfsFreeRealFilesystem(t *testing.T) {
	free, ok := statfsFree(t.TempDir())
	if !ok {
		t.Fatal("statfsFree: want ok=true for a real, existing directory")
	}
	if free == 0 {
		t.Fatal("statfsFree: want free > 0 bytes on a real filesystem (a genuinely full one is vanishingly unlikely in CI)")
	}
}

// TestStatfsFreeMissingPath: a path that does not exist must fail cleanly
// (ok=false), never panic.
func TestStatfsFreeMissingPath(t *testing.T) {
	_, ok := statfsFree("/no/such/path/sigbound-disk-preflight-test")
	if ok {
		t.Fatal("statfsFree: want ok=false for a nonexistent path")
	}
}
