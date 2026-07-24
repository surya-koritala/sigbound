package cell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// assertPopulated checks a worktree dir is a COMPLETE checkout of base on
// branch: every base file present, the tree clean (index+worktree == HEAD), and
// the branch ref still at base (the populate must not move it). This is the
// property the --no-checkout add + reset --hard populate split must preserve
// byte-for-byte against the old single `worktree add` (which checked out inline).
func assertPopulated(t *testing.T, c *Cell, dir, branch, base string, numFiles int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < numFiles; i++ {
		f := filepath.Join(dir, fmt.Sprintf("f%03d.txt", i))
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("worktree %s missing base file f%03d.txt after populate: %v", dir, i, err)
		}
	}
	dirty, err := c.Git().At(dir).HasUncommittedChanges(ctx)
	if err != nil {
		t.Fatalf("status %s: %v", dir, err)
	}
	if dirty {
		t.Fatalf("worktree %s is dirty after populate — reset --hard should leave it clean", dir)
	}
	head, err := c.Git().RevParse(ctx, branch)
	if err != nil {
		t.Fatalf("rev-parse %s: %v", branch, err)
	}
	if head != base {
		t.Fatalf("branch %s at %s, want base %s (populate moved the ref)", branch, head, base)
	}
}

func tracked(c *Cell, dir string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.created[dir]
	return ok
}

// TestAddWorktreePopulatesFully proves the two-phase add returns a fully
// materialized worktree, not the empty --no-checkout shell (the failure mode a
// missing populate would produce).
func TestAddWorktreePopulatesFully(t *testing.T) {
	ctx := context.Background()
	g, _, base := scenario(t, 12)
	c, err := Open(g.Dir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "wt")
	if err := c.AddWorktree(ctx, dir, "agent/full", base, false); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	assertPopulated(t, c, dir, "agent/full", base, 12)
	if !tracked(c, dir) {
		t.Fatal("populated worktree not registered in created")
	}
}

// TestAddWorktreeLoudFailAndReset is acceptance criterion #2: the no-checkout
// split preserves -b loud-fail-on-collision and -B reset semantics identically.
func TestAddWorktreeLoudFailAndReset(t *testing.T) {
	ctx := context.Background()
	g, _, base := scenario(t, 4)
	c, err := Open(g.Dir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()

	dirA := filepath.Join(root, "wtA")
	if err := c.AddWorktree(ctx, dirA, "agent/x", base, false); err != nil {
		t.Fatalf("first add: %v", err)
	}

	// -b (reset=false) MUST loud-fail on the pre-existing branch, never silently
	// reset it. The add fails in phase 1, so nothing is registered or left on disk.
	dirB := filepath.Join(root, "wtB")
	if err := c.AddWorktree(ctx, dirB, "agent/x", base, false); err == nil {
		t.Fatal("second -b add on existing branch: want loud failure, got nil")
	}
	if tracked(c, dirB) {
		t.Fatal("failed loud-fail add left dirB registered in created")
	}
	if _, err := os.Stat(dirB); !os.IsNotExist(err) {
		t.Fatalf("failed loud-fail add left dirB on disk (stat err=%v)", err)
	}

	// -B (reset=true) re-creates the branch's worktree once the prior one is gone
	// (the real -agent-retries shape: tear down attempt N, reset attempt N+1).
	if err := c.RemoveWorktree(ctx, dirA); err != nil {
		t.Fatalf("remove dirA: %v", err)
	}
	dirC := filepath.Join(root, "wtC")
	if err := c.AddWorktree(ctx, dirC, "agent/x", base, true); err != nil {
		t.Fatalf("reset (-B) add after removal: %v", err)
	}
	assertPopulated(t, c, dirC, "agent/x", base, 4)
}

// resetFailingGit writes a shell shim named `git` that passes every subcommand
// through to the real git EXCEPT `reset`, which it fails. Bound onto a cell's
// git handle it lets the locked --no-checkout add succeed but the out-of-lock
// reset --hard populate fail deterministically — the cleanest injection of a
// populate failure without a production seam.
func resetFailingGit(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	shim := filepath.Join(t.TempDir(), "git")
	// gitx always invokes `git -C <dir> <subcommand> ...` (see runWith), so the
	// subcommand is $3 — fail exactly `reset`, pass everything else through.
	script := "#!/bin/sh\nif [ \"$3\" = reset ]; then echo 'injected populate failure' >&2; exit 1; fi\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return shim
}

// TestAddWorktreePopulateFailureCleansUp is acceptance criterion #3: when the
// populate fails, the half-made worktree is torn down, the error is surfaced,
// and the created map is left consistent (dir not tracked) — no half-populated
// tree survives as "created OK".
func TestAddWorktreePopulateFailureCleansUp(t *testing.T) {
	ctx := context.Background()
	g, _, base := scenario(t, 6)
	c, err := Open(g.Dir())
	if err != nil {
		t.Fatal(err)
	}
	// Swap in the reset-failing shim AFTER Open's real-git preflight ran.
	c.git = c.git.WithBinary(resetFailingGit(t))

	dir := filepath.Join(t.TempDir(), "wt")
	err = c.AddWorktree(ctx, dir, "agent/broken", base, false)
	if err == nil {
		t.Fatal("AddWorktree with a failing populate: want error, got nil")
	}

	// Cleanup: the dir is gone, the registration is gone, and git no longer lists
	// the worktree — so a later Close/gc never trips over a ghost.
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("half-made worktree left on disk after populate failure (stat err=%v)", statErr)
	}
	if tracked(c, dir) {
		t.Fatal("half-made worktree still registered in created after populate failure")
	}
	c.mu.Lock()
	n := len(c.created)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("created map not empty after a lone failed add: %d entries", n)
	}
	if list := listWorktrees(t, g.Dir()); strings.Contains(list, dir) {
		t.Fatalf("git still lists the torn-down worktree %s:\n%s", dir, list)
	}
}

// TestAddWorktreeConcurrent is acceptance criterion #4: many goroutines add
// worktrees on ONE cell (distinct dirs/branches) at once. Under -race this
// proves the split's out-of-lock populate is data-race-free and that every
// worktree still ends fully populated with its branch at base.
func TestAddWorktreeConcurrent(t *testing.T) {
	ctx := context.Background()
	const n = 24
	g, _, base := scenario(t, 20)
	c, err := Open(g.Dir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()

	type made struct {
		dir, branch string
	}
	results := make([]made, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dir := filepath.Join(root, fmt.Sprintf("wt-%02d", i))
			branch := fmt.Sprintf("agent/c%02d", i)
			errs[i] = c.AddWorktree(ctx, dir, branch, base, false)
			results[i] = made{dir, branch}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent AddWorktree %d: %v", i, errs[i])
		}
		assertPopulated(t, c, results[i].dir, results[i].branch, base, 20)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestAddWorktreeCloseInterleave is the other half of criterion #4: AddWorktree
// racing Close on one cell. The point is no panic, no deadlock, and a consistent
// created map — a worktree caught mid-add either lands and gets torn down or
// fails its populate and cleans up, never leaking a tracked ghost. Individual
// add results are legitimately either nil or a populate error, so only the end
// state is asserted.
func TestAddWorktreeCloseInterleave(t *testing.T) {
	ctx := context.Background()
	const n = 24
	g, _, base := scenario(t, 8)
	c, err := Open(g.Dir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dir := filepath.Join(root, fmt.Sprintf("wt-%02d", i))
			_ = c.AddWorktree(ctx, dir, fmt.Sprintf("agent/i%02d", i), base, false)
		}(i)
	}
	// Hammer Close concurrently with the adds.
	var closeWG sync.WaitGroup
	for k := 0; k < 4; k++ {
		closeWG.Add(1)
		go func() {
			defer closeWG.Done()
			for j := 0; j < 8; j++ {
				_ = c.Close(ctx)
			}
		}()
	}
	wg.Wait()
	closeWG.Wait()

	// A final Close sweeps up any worktree an add registered after the last
	// concurrent Close passed it, and must leave created empty and error-free.
	if err := c.Close(ctx); err != nil {
		t.Fatalf("final Close: %v", err)
	}
	c.mu.Lock()
	n2 := len(c.created)
	c.mu.Unlock()
	if n2 != 0 {
		t.Fatalf("created map not empty after final Close: %d entries", n2)
	}
}
