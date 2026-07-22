package cell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// scenario builds a temp repo with numFiles base files and a base commit, and
// returns the main git handle, a worktree pool, and the base SHA.
func scenario(t *testing.T, numFiles int) (*gitx.Git, *WorktreePool, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	wtRoot := filepath.Join(root, "wts")
	if err := os.MkdirAll(wtRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	g := gitx.New(repoDir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < numFiles; i++ {
		// Unique line content: otherwise git can't tell which of N identical
		// lines an agent changed, and distinct-line edits conflict ambiguously.
		var sb strings.Builder
		for ln := 0; ln < 40; ln++ {
			fmt.Fprintf(&sb, "f%03d base line %02d\n", i, ln)
		}
		if err := os.WriteFile(filepath.Join(repoDir, fmt.Sprintf("f%03d.txt", i)), []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	return g, NewWorktreePool(repoDir, wtRoot), base
}

// editDistinctLine rewrites file `path` so that line `line` carries a unique
// marker. Distinct lines across agents => git auto-merges (no human conflict).
func editDistinctLine(path, marker string, line int) func(dir string) {
	return func(dir string) {
		full := filepath.Join(dir, path)
		data, _ := os.ReadFile(full)
		lines := strings.Split(string(data), "\n")
		if line < len(lines) {
			lines[line] = marker
		}
		_ = os.WriteFile(full, []byte(strings.Join(lines, "\n")), 0o644)
	}
}

// hotEdit is a well-separated single-line edit to a shared file. Lines must be
// spaced apart: git treats adjacent-line changes as overlapping hunks and would
// conflict, so callers assign distinct, spaced line numbers to get clean 3-way
// auto-merges.
type hotEdit struct {
	file string
	line int
}

// spawnAgents runs N agents in parallel. Each writes a unique private file and,
// if it's in the overlapping set, also edits a distinct, spaced line of a shared
// hot file. Returns their BranchChanges (branch + write-set).
func spawnAgents(t *testing.T, pool *WorktreePool, base string, n int, overlapping map[int]hotEdit) []BranchChange {
	t.Helper()
	ctx := context.Background()
	out := make([]BranchChange, n)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wt, err := pool.Acquire(ctx, base)
			if err != nil {
				errs[i] = err
				return
			}
			// private file
			priv := fmt.Sprintf("agent_%03d.txt", i)
			if err := os.WriteFile(filepath.Join(wt.Dir, priv), []byte(fmt.Sprintf("agent %d\n", i)), 0o644); err != nil {
				errs[i] = err
				return
			}
			// optional hot-file edit on a distinct, spaced line
			if hot, ok := overlapping[i]; ok {
				editDistinctLine(hot.file, fmt.Sprintf("agent-%d-was-here", i), hot.line)(wt.Dir)
			}
			if _, err := wt.Git().CommitAll(ctx, fmt.Sprintf("agent %d", i)); err != nil {
				errs[i] = err
				return
			}
			ws, err := WriteSetFor(ctx, pool.Git(), base, wt.Branch)
			if err != nil {
				errs[i] = err
				return
			}
			out[i] = BranchChange{Branch: wt.Branch, WriteSet: ws}
			_ = pool.Release(ctx, wt)
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("agent %d: %v", i, e)
		}
	}
	return out
}

// WriteSetFor is duplicated from worker to avoid an import cycle in tests.
func WriteSetFor(ctx context.Context, g *gitx.Git, base, branch string) (*WriteSet, error) {
	paths, err := g.DiffNameOnly(ctx, base, branch)
	if err != nil {
		return nil, err
	}
	return NewWriteSet(paths...), nil
}

// assertAllLanded checks the final tree contains every agent's private file
// (structural correctness: no landed change is lost).
func assertAllLanded(t *testing.T, g *gitx.Git, finalSHA string, n int) {
	t.Helper()
	ctx := context.Background()
	paths, err := g.LsTree(ctx, finalSHA)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, p := range paths {
		have[p] = true
	}
	for i := 0; i < n; i++ {
		if !have[fmt.Sprintf("agent_%03d.txt", i)] {
			t.Fatalf("final tree %s missing agent_%03d.txt", finalSHA, i)
		}
	}
}

func TestIntegrateDisjointBothStrategies(t *testing.T) {
	ctx := context.Background()
	g, pool, base := scenario(t, 12)
	const n = 8
	changes := spawnAgents(t, pool, base, n, nil) // fully disjoint

	in := NewIntegrator(pool.Git())

	naive, err := in.IntegrateNaive(ctx, base, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(naive.Landed) != n || len(naive.Flagged) != 0 {
		t.Fatalf("naive landed=%d flagged=%d, want %d/0", len(naive.Landed), len(naive.Flagged), n)
	}
	assertAllLanded(t, g, naive.FinalSHA, n)

	occ, err := in.IntegrateOCC(ctx, base, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(occ.Landed) != n || len(occ.Flagged) != 0 {
		t.Fatalf("occ landed=%d flagged=%d, want %d/0", len(occ.Landed), len(occ.Flagged), n)
	}
	if occ.Groups != n {
		t.Fatalf("occ groups=%d, want %d singleton groups", occ.Groups, n)
	}
	assertAllLanded(t, g, occ.FinalSHA, n)

	// Both strategies must produce the SAME tree for a fully-disjoint batch.
	naiveTree, _ := g.RevParse(ctx, naive.FinalSHA+"^{tree}")
	occTree, _ := g.RevParse(ctx, occ.FinalSHA+"^{tree}")
	// RevParse verifies commits; compare trees via ls-tree hash instead.
	nt := treeHash(t, g, naive.FinalSHA)
	ot := treeHash(t, g, occ.FinalSHA)
	if nt != ot {
		t.Fatalf("disjoint trees differ: naive=%s occ=%s", nt, ot)
	}
	_ = naiveTree
	_ = occTree
}

func TestIntegrateOverlapAutoMerges(t *testing.T) {
	ctx := context.Background()
	g, pool, base := scenario(t, 12)
	const n = 8
	// Agents 0..5 all touch the SAME hot file on DISTINCT, SPACED lines -> one
	// group, serialized, git auto-merges every one. Agents 6,7 are disjoint.
	overlap := map[int]hotEdit{}
	for i := 0; i < 6; i++ {
		overlap[i] = hotEdit{file: "f000.txt", line: i*5 + 1} // lines 1,6,11,16,21,26
	}
	changes := spawnAgents(t, pool, base, n, overlap)

	in := NewIntegrator(pool.Git())
	occ, err := in.IntegrateOCC(ctx, base, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(occ.Flagged) != 0 {
		t.Fatalf("expected 0 flagged (distinct lines auto-merge), got %d", len(occ.Flagged))
	}
	if len(occ.Landed) != n {
		t.Fatalf("landed=%d, want %d", len(occ.Landed), n)
	}
	// 6 agents share the hot file -> one group of 6 + two singletons = 3 groups.
	if occ.Groups != 3 {
		t.Fatalf("groups=%d, want 3", occ.Groups)
	}
	if occ.LargestGroup != 6 {
		t.Fatalf("largestGroup=%d, want 6", occ.LargestGroup)
	}
	// 5 of the 6 in the hot group are auto-merged overlaps (first lands clean).
	if occ.AutoMerged != 5 {
		t.Fatalf("autoMerged=%d, want 5", occ.AutoMerged)
	}
	assertAllLanded(t, g, occ.FinalSHA, n)

	// And every agent's hot-file marker survived the auto-merge.
	content, err := g.ShowFile(ctx, occ.FinalSHA, "f000.txt")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if !strings.Contains(content, fmt.Sprintf("agent-%d-was-here", i)) {
			t.Fatalf("hot file missing agent %d marker after auto-merge", i)
		}
	}
}

func TestIntegrateConflictFlagged(t *testing.T) {
	ctx := context.Background()
	_, pool, base := scenario(t, 6)
	// Two agents edit the SAME line of the SAME hot file -> real conflict.
	const n = 2
	changes := make([]BranchChange, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wt, _ := pool.Acquire(ctx, base)
			editDistinctLine("f000.txt", fmt.Sprintf("agent-%d-CLAIMS-line-5", i), 5)(wt.Dir)
			sha, _ := wt.Git().CommitAll(ctx, fmt.Sprintf("agent %d", i))
			_ = sha
			ws, _ := WriteSetFor(ctx, pool.Git(), base, wt.Branch)
			changes[i] = BranchChange{Branch: wt.Branch, WriteSet: ws}
			_ = pool.Release(ctx, wt)
		}(i)
	}
	wg.Wait()

	in := NewIntegrator(pool.Git())
	occ, err := in.IntegrateOCC(ctx, base, changes)
	if err != nil {
		t.Fatal(err)
	}
	// Same file => one group. First lands, second flagged for a human.
	if occ.Groups != 1 {
		t.Fatalf("groups=%d, want 1", occ.Groups)
	}
	if len(occ.Landed) != 1 || len(occ.Flagged) != 1 {
		t.Fatalf("landed=%d flagged=%d, want 1/1", len(occ.Landed), len(occ.Flagged))
	}
	if len(occ.Flagged[0].Conflicts) != 1 || occ.Flagged[0].Conflicts[0] != "f000.txt" {
		t.Fatalf("conflict paths=%v, want [f000.txt]", occ.Flagged[0].Conflicts)
	}
}

// TestFoldGroupHeadIsOneCommit is the seam test for the tree-OID fold (issue
// #23): a multi-branch overlapping group must still fold to the SAME tree as
// porcelain, and it must do so via exactly ONE new commit — the group head,
// with acc and every landed branch as its parents — not one commit per landed
// branch. It calls fold directly since IntegrationResult doesn't expose a
// group head on its own (IntegrateOCC's FinalSHA is the disjoint combine of
// group heads, not a group head itself).
func TestFoldGroupHeadIsOneCommit(t *testing.T) {
	ctx := context.Background()
	g, pool, base := scenario(t, 4)
	const n = 5
	// All n agents edit the SAME hot file on distinct, spaced lines -> one
	// overlapping group, every branch auto-merges clean.
	overlap := map[int]hotEdit{}
	for i := 0; i < n; i++ {
		overlap[i] = hotEdit{file: "f000.txt", line: i*5 + 1}
	}
	changes := spawnAgents(t, pool, base, n, overlap)

	in := NewIntegrator(pool.Git())
	head, landed, flagged, _, err := in.fold(ctx, base, base, changes, "sigbound: test")
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if len(landed) != n || len(flagged) != 0 {
		t.Fatalf("landed=%d flagged=%d, want %d/0", len(landed), len(flagged), n)
	}

	// Tree must match what the porcelain baseline produces for the same batch.
	porc, err := in.IntegratePorcelain(ctx, base, changes)
	if err != nil {
		t.Fatalf("porcelain: %v", err)
	}
	if got, want := treeHash(t, g, head), treeHash(t, g, porc.FinalSHA); got != want {
		t.Fatalf("fold tree != porcelain tree")
	}

	// Exactly one octopus commit: parent 1 is acc (base), parents 2..n+1 are
	// the landed branches in order, and there is no parent n+2 — i.e. head is
	// ONE commit, not a chain of n.
	if p, err := g.RevParse(ctx, head+"^1"); err != nil || p != base {
		t.Fatalf("head^1 = %q (err %v), want base %q", p, err, base)
	}
	for i, branch := range landed {
		want, err := g.RevParse(ctx, branch)
		if err != nil {
			t.Fatalf("resolve %s: %v", branch, err)
		}
		p, err := g.RevParse(ctx, fmt.Sprintf("%s^%d", head, i+2))
		if err != nil || p != want {
			t.Fatalf("head^%d = %q (err %v), want landed branch %s = %q", i+2, p, err, branch, want)
		}
	}
	if _, err := g.RevParse(ctx, fmt.Sprintf("%s^%d", head, n+2)); err == nil {
		t.Fatalf("head has an extra parent beyond acc + %d landed branches", n)
	}
}

func treeHash(t *testing.T, g *gitx.Git, rev string) string {
	t.Helper()
	ctx := context.Background()
	paths, err := g.LsTree(ctx, rev)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	// Hash the sorted (path -> content) pairs so two commits with identical
	// trees but different history compare equal.
	var b strings.Builder
	for _, p := range paths {
		c, err := g.ShowFile(ctx, rev, p)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s\x00%s\x00", p, c)
	}
	return b.String()
}
