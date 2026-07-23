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

// listWorktrees returns `git worktree list --porcelain` for repo (read-only, so
// a plain exec is fine — no hermetic env needed).
func listWorktrees(t *testing.T, repo string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v: %s", err, out)
	}
	return string(out)
}

func TestOpenValidation(t *testing.T) {
	// Bad path: does not exist.
	if _, err := Open(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("Open on a nonexistent path: want error")
	}
	// Non-repo: an existing directory that is not a git repository.
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("Open on a non-git directory: want error")
	}

	g, _, _ := scenario(t, 1)
	repo := g.Dir() // already absolute (t.TempDir)

	c, err := Open(repo)
	if err != nil {
		t.Fatalf("Open real repo: %v", err)
	}
	if c.Repo() != repo {
		t.Fatalf("Repo()=%q, want %q", c.Repo(), repo)
	}
	if c.ID() == "" {
		t.Fatal("derived id is empty")
	}
	// Deterministic: same repo path always derives the same id.
	c2, err := Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID() != c2.ID() {
		t.Fatalf("id not deterministic: %q vs %q", c.ID(), c2.ID())
	}
	// Distinct repos derive distinct ids.
	g2, _, _ := scenario(t, 1)
	c3, err := Open(g2.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if c.ID() == c3.ID() {
		t.Fatalf("distinct repos share id %q", c.ID())
	}
	// Custom id via WithID overrides the derived default.
	cc, err := Open(repo, WithID("custom-id"))
	if err != nil {
		t.Fatal(err)
	}
	if cc.ID() != "custom-id" {
		t.Fatalf("WithID ignored: got %q", cc.ID())
	}
}

func TestCellCloseCleansWorktrees(t *testing.T) {
	ctx := context.Background()
	g, _, base := scenario(t, 2)
	repo := g.Dir()
	c, err := Open(repo)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	var dirs []string
	for i := 0; i < 3; i++ {
		dir := filepath.Join(root, fmt.Sprintf("wt-%d", i))
		if err := c.AddWorktree(ctx, dir, fmt.Sprintf("cell/wt-%d", i), base, false); err != nil {
			t.Fatalf("AddWorktree %d: %v", i, err)
		}
		dirs = append(dirs, dir)
	}

	// Every created worktree is present on disk and in git's worktree list.
	before := listWorktrees(t, repo)
	for _, d := range dirs {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("worktree dir %s should exist before Close: %v", d, err)
		}
		if !strings.Contains(before, d) {
			t.Fatalf("worktree %s missing from `git worktree list` before Close:\n%s", d, before)
		}
	}

	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Close tore them all down: gone from disk and from git's worktree list.
	after := listWorktrees(t, repo)
	for _, d := range dirs {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("worktree dir %s should be gone after Close, stat err=%v", d, err)
		}
		if strings.Contains(after, d) {
			t.Fatalf("worktree %s still in `git worktree list` after Close:\n%s", d, after)
		}
	}

	// Idempotent: a second Close finds nothing to do and does not error.
	if err := c.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestConcurrentCellsIntegrateDifferentRepos is the sharding property: two cells
// on two independent repos integrate (and land onto their own base ref) at the
// same time with no cross-contamination. Run under -race, this proves a cell
// only serializes its OWN repo and different cells never share mutable state.
func TestConcurrentCellsIntegrateDifferentRepos(t *testing.T) {
	const n = 6
	// Distinct base sizes so the two repos have genuinely different trees: their
	// integrated commits then differ by content, deterministically, which lets
	// the no-crosstalk check below be exact. (Byte-identical repos would produce
	// the SAME content-addressed commit SHA — correct git behavior, not shared
	// stores — so equal content could never prove disjointness.)
	g1, pool1, base1 := scenario(t, 10)
	changes1 := spawnAgents(t, pool1, base1, n, nil) // fully disjoint
	g2, pool2, base2 := scenario(t, 14)
	changes2 := spawnAgents(t, pool2, base2, n, nil)

	c1, err := Open(g1.Dir())
	if err != nil {
		t.Fatal(err)
	}
	c2, err := Open(g2.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if c1.ID() == c2.ID() {
		t.Fatalf("two repos share cell id %q", c1.ID())
	}

	ctx := context.Background()
	landMain := func(in *Integrator) { in.WithLandRef("refs/heads/main") }
	var (
		wg     sync.WaitGroup
		r1, r2 IntegrationResult
		e1, e2 error
	)
	wg.Add(2)
	go func() { defer wg.Done(); r1, e1 = c1.Integrate(ctx, base1, changes1, StrategyOverlay, landMain) }()
	go func() { defer wg.Done(); r2, e2 = c2.Integrate(ctx, base2, changes2, StrategyOverlay, landMain) }()
	wg.Wait()

	if e1 != nil {
		t.Fatalf("cell1 integrate: %v", e1)
	}
	if e2 != nil {
		t.Fatalf("cell2 integrate: %v", e2)
	}
	if len(r1.Landed) != n || len(r1.Flagged) != 0 {
		t.Fatalf("cell1 landed=%d flagged=%d, want %d/0", len(r1.Landed), len(r1.Flagged), n)
	}
	if len(r2.Landed) != n || len(r2.Flagged) != 0 {
		t.Fatalf("cell2 landed=%d flagged=%d, want %d/0", len(r2.Landed), len(r2.Flagged), n)
	}
	assertAllLanded(t, g1, r1.FinalSHA, n)
	assertAllLanded(t, g2, r2.FinalSHA, n)

	// Each cell advanced its OWN base ref to its OWN final commit — no crosstalk.
	head1, err := g1.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if head1 != r1.FinalSHA {
		t.Fatalf("cell1 main=%s, want landed %s", head1, r1.FinalSHA)
	}
	head2, err := g2.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if head2 != r2.FinalSHA {
		t.Fatalf("cell2 main=%s, want landed %s", head2, r2.FinalSHA)
	}
	// Different-content repos must land different commits: an equal SHA here
	// would mean one cell's integration leaked into the other's store.
	if r1.FinalSHA == r2.FinalSHA {
		t.Fatal("different-content repos produced the same final commit — cells crosstalked")
	}
}
