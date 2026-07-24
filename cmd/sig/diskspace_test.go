package main

import (
	"context"
	"strings"
	"testing"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// TestDiskShortfall locks the pure arithmetic behind the preflight verdict:
// estimate is treeBytes x nTasks, margin pads it 10%, and refuse fires only
// once the PADDED estimate exceeds free — not the raw one.
func TestDiskShortfall(t *testing.T) {
	cases := []struct {
		name         string
		treeBytes    int64
		nTasks       int
		free         uint64
		wantEstimate int64
		wantMargin   int64
		wantRefuse   bool
	}{
		{"comfortably fits", 100, 4, 1000, 400, 440, false},
		{"raw estimate under free, but margin tips it over", 100, 9, 990, 900, 990, false}, // margin == free: not GREATER than, so no refuse
		{"margin exceeds free by one byte", 100, 9, 989, 900, 990, true},
		{"exactly at the margin refuses only when strictly exceeded", 1000, 1, 1100, 1000, 1100, false},
		{"one byte short refuses", 1000, 1, 1099, 1000, 1100, true},
		{"zero tree size never refuses", 0, 512, 0, 0, 0, false},
		{"zero tasks never refuses", 500, 0, 0, 0, 0, false},
		{"large repo, large fan-out, plenty of free space", 5 * 1024 * 1024, 512, 100 * 1024 * 1024 * 1024, 5 * 1024 * 1024 * 512, 5*1024*1024*512 + (5*1024*1024*512)/10, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			estimate, margin, refuse := diskShortfall(c.treeBytes, c.nTasks, c.free)
			if estimate != c.wantEstimate {
				t.Errorf("estimate = %d, want %d", estimate, c.wantEstimate)
			}
			if margin != c.wantMargin {
				t.Errorf("margin = %d, want %d", margin, c.wantMargin)
			}
			if refuse != c.wantRefuse {
				t.Errorf("refuse = %v, want %v", refuse, c.wantRefuse)
			}
		})
	}
}

// TestDiskPreflightFailsOpenOnUnreadableTree: TreeSize erroring (a bad rev)
// must never block a run — diskPreflight fails OPEN rather than refusing on
// an estimate it couldn't even form.
func TestDiskPreflightFailsOpenOnUnreadableTree(t *testing.T) {
	orig := diskFreeBytes
	diskFreeBytes = func(string) (uint64, bool) { return 1, true } // would refuse if reached
	t.Cleanup(func() { diskFreeBytes = orig })

	ctx := context.Background()
	dir := t.TempDir()
	g := gitx.New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := diskPreflight(ctx, g, "no-such-rev", dir, 512); err != nil {
		t.Fatalf("diskPreflight = %v, want nil (fail open on an unreadable tree)", err)
	}
}

// TestDiskPreflightFailsOpenWhenFreeSpaceUnknown: a diskFreeBytes reading of
// ok=false (unsupported platform, or unreadable filesystem) must also never
// block a run.
func TestDiskPreflightFailsOpenWhenFreeSpaceUnknown(t *testing.T) {
	orig := diskFreeBytes
	diskFreeBytes = func(string) (uint64, bool) { return 0, false }
	t.Cleanup(func() { diskFreeBytes = orig })

	ctx := context.Background()
	g, base := newDoctorRepo(t)
	if err := diskPreflight(ctx, g, base, g.Dir(), 999999); err != nil {
		t.Fatalf("diskPreflight = %v, want nil (fail open when free space is unknown)", err)
	}
}

// TestDiskPreflightSkipsOnZeroTasks: nTasks<=0 is a no-op — never even reads
// the tree size or free space (whatever diskFreeBytes is set to).
func TestDiskPreflightSkipsOnZeroTasks(t *testing.T) {
	orig := diskFreeBytes
	called := false
	diskFreeBytes = func(string) (uint64, bool) { called = true; return 0, false }
	t.Cleanup(func() { diskFreeBytes = orig })

	ctx := context.Background()
	g, base := newDoctorRepo(t)
	if err := diskPreflight(ctx, g, base, g.Dir(), 0); err != nil {
		t.Fatalf("diskPreflight = %v, want nil for nTasks=0", err)
	}
	if called {
		t.Fatal("diskPreflight read free space for nTasks=0, want it to short-circuit before that")
	}
}

// TestDiskInfoLineRendersForRealRepo: the happy path renders all three
// pieces (tree size, free space, the reference-run estimate) for a real repo.
func TestDiskInfoLineRendersForRealRepo(t *testing.T) {
	ctx := context.Background()
	g, _ := newDoctorRepo(t)
	line := diskInfoLine(ctx, g.Dir())
	if !strings.HasPrefix(line, "disk: repo tree ~") {
		t.Fatalf("diskInfoLine = %q, want it to start with the repo-tree summary", line)
	}
	if !strings.Contains(line, "512-agent run needs") {
		t.Fatalf("diskInfoLine = %q, want the reference-run estimate", line)
	}
}

// TestDiskInfoLineHandlesUnreadableRepo: a -repo that isn't a git repository
// at all still renders a line (never panics, never returns empty) — doctor
// never fails on this, so the message must degrade gracefully instead of
// crashing runDoctor.
func TestDiskInfoLineHandlesUnreadableRepo(t *testing.T) {
	ctx := context.Background()
	line := diskInfoLine(ctx, t.TempDir())
	if !strings.HasPrefix(line, "disk: unable to determine") {
		t.Fatalf("diskInfoLine = %q, want the unreadable-tree fallback wording", line)
	}
}
