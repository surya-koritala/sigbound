package gitx

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// branchFrom builds a branch off base in a throwaway worktree, runs edit against
// its directory, commits, removes the worktree, and returns the commit SHA.
func branchFrom(t *testing.T, g *Git, base, branch string, edit func(dir string)) string {
	t.Helper()
	ctx := context.Background()
	wt := filepath.Join(t.TempDir(), branch)
	if err := g.WorktreeAdd(ctx, wt, branch, base); err != nil {
		t.Fatal(err)
	}
	edit(wt)
	sha, err := g.At(wt).CommitAll(ctx, branch)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.WorktreeRemove(ctx, wt); err != nil {
		t.Fatal(err)
	}
	return sha
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestOverlayTreesEqualsMergeTree is the correctness anchor for the overlay fast
// path: for path-DISJOINT branches, overlaying their changed entries onto base
// must yield the exact same tree OID git's 3-way merge-tree produces. Covers add,
// modify, delete, nested paths and the executable bit.
func TestOverlayTreesEqualsMergeTree(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "shared.txt", "shared\n")
	write(t, dir, "base_only.txt", "orig\n")
	write(t, dir, "dir/n.txt", "nested\n")
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}

	// ours: add a.txt, modify base_only.txt, delete dir/n.txt.
	ours := branchFrom(t, g, base, "ours", func(d string) {
		write(t, d, "a.txt", "a-content\n")
		write(t, d, "base_only.txt", "modified-by-ours\n")
		_ = os.Remove(filepath.Join(d, "dir", "n.txt"))
	})
	// theirs: add executable b.txt and c.txt — disjoint paths from ours.
	theirs := branchFrom(t, g, base, "theirs", func(d string) {
		write(t, d, "b.txt", "b-content\n")
		_ = os.Chmod(filepath.Join(d, "b.txt"), 0o755)
		write(t, d, "c.txt", "c-content\n")
	})

	mt, err := g.MergeTree(ctx, base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if !mt.OK {
		t.Fatalf("disjoint merge-tree unexpectedly conflicted: %v", mt.Conflicts)
	}

	overlay, err := g.OverlayTrees(ctx, base, []string{ours, theirs})
	if err != nil {
		t.Fatal(err)
	}
	if overlay != mt.Tree {
		t.Fatalf("overlay tree %s != merge-tree %s", overlay, mt.Tree)
	}

	// Sanity: the union really contains adds, the modify, the exec bit, and the
	// deletion is gone.
	files, err := g.LsTree(ctx, overlay)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	for _, w := range []string{"a.txt", "b.txt", "c.txt", "base_only.txt", "shared.txt"} {
		if !got[w] {
			t.Fatalf("overlay tree missing %s (have %v)", w, files)
		}
	}
	if got["dir/n.txt"] {
		t.Fatalf("overlay tree still has deleted dir/n.txt")
	}
}

func TestDiffRawAddModifyDelete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "keep.txt", "k\n")
	write(t, dir, "gone.txt", "g\n")
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	b := branchFrom(t, g, base, "b", func(d string) {
		write(t, d, "added.txt", "new\n")
		write(t, d, "keep.txt", "changed\n")
		_ = os.Remove(filepath.Join(d, "gone.txt"))
	})

	ents, err := g.DiffRaw(ctx, base, b)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]DiffRawEntry{}
	for _, e := range ents {
		byPath[e.Path] = e
	}
	if e, ok := byPath["added.txt"]; !ok || e.Deleted() || e.Mode != "100644" {
		t.Fatalf("added.txt entry wrong: %+v", e)
	}
	if e, ok := byPath["keep.txt"]; !ok || e.Deleted() {
		t.Fatalf("keep.txt should be a modify: %+v", e)
	}
	if e, ok := byPath["gone.txt"]; !ok || !e.Deleted() {
		t.Fatalf("gone.txt should be a deletion: %+v", e)
	}
}

// TestDiffTreeUnionMatchesDiffRaw locks the batched overlay diff: the single
// `git diff-tree --stdin` union must carry the exact same destination-side
// (mode, oid, path) entries as running DiffRaw(base, head) per head. This guards
// the subtle two-tree stdin direction (git diffs the second arg to the first, so
// the union writes "<head> <base>"); a git change to that order would flip adds
// into deletes and is caught here rather than as a silently wrong tree.
func TestDiffTreeUnionMatchesDiffRaw(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "keep.txt", "k\n")
	write(t, dir, "mod.txt", "orig\n")
	write(t, dir, "gone.txt", "g\n")
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	h1 := branchFrom(t, g, base, "h1", func(d string) { write(t, d, "added.txt", "new\n") })
	h2 := branchFrom(t, g, base, "h2", func(d string) {
		write(t, d, "mod.txt", "changed\n")
		_ = os.Remove(filepath.Join(d, "gone.txt"))
	})

	want := map[string]DiffRawEntry{}
	for _, h := range []string{h1, h2} {
		ents, err := g.DiffRaw(ctx, base, h)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			want[e.Path] = e
		}
	}

	got, err := g.diffTreeUnion(ctx, base, []string{h1, h2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("union entries=%d, want %d (%+v)", len(got), len(want), got)
	}
	for _, e := range got {
		w, ok := want[e.Path]
		if !ok {
			t.Fatalf("union has unexpected path %q", e.Path)
		}
		if e.Mode != w.Mode || e.OID != w.OID {
			t.Fatalf("union %q = (%s,%s), DiffRaw = (%s,%s)", e.Path, e.Mode, e.OID, w.Mode, w.OID)
		}
	}
}

func TestBatchReaderResolve(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	br, err := g.NewBatchReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer br.Close()

	// Same object resolved twice through the one long-running process.
	for i := 0; i < 2; i++ {
		oid, typ, size, ok, err := br.Resolve(base)
		if err != nil || !ok {
			t.Fatalf("resolve base: ok=%v err=%v", ok, err)
		}
		if oid != base || typ != "commit" || size <= 0 {
			t.Fatalf("resolve base = %s/%s/%d", oid, typ, size)
		}
	}
	commit, err := br.ResolveCommit("main")
	if err != nil || commit != base {
		t.Fatalf("ResolveCommit(main) = %s err=%v, want %s", commit, err, base)
	}
	if _, _, _, ok, err := br.Resolve("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err != nil || ok {
		t.Fatalf("missing object should report exists=false, got ok=%v err=%v", ok, err)
	}
}

func TestUpdateRefsBatch(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	// Two commits to point new refs at.
	c1 := branchFrom(t, g, base, "src1", func(d string) { write(t, d, "one.txt", "1\n") })
	c2 := branchFrom(t, g, base, "src2", func(d string) { write(t, d, "two.txt", "2\n") })

	if err := g.UpdateRefs(ctx, map[string]string{
		"refs/heads/landed-a": c1,
		"refs/heads/landed-b": c2,
	}); err != nil {
		t.Fatal(err)
	}
	gotA, err := g.RevParse(ctx, "refs/heads/landed-a")
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := g.RevParse(ctx, "refs/heads/landed-b")
	if err != nil {
		t.Fatal(err)
	}
	if gotA != c1 || gotB != c2 {
		t.Fatalf("update-refs: a=%s(want %s) b=%s(want %s)", gotA, c1, gotB, c2)
	}

	// Single-ref convenience also works.
	if err := g.UpdateRef(ctx, "refs/heads/main", c1); err != nil {
		t.Fatal(err)
	}
	head, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if head != c1 {
		t.Fatalf("main moved to %s, want %s", head, c1)
	}
}

// TestOverlayManyDisjoint scales the overlay to many singleton branches and
// checks it still matches a sequential merge-tree fold — the disjoint combine the
// integrator relies on.
func TestOverlayManyDisjoint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "root.txt", "root\n")
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	const n = 16
	trees := make([]string, n)
	for i := 0; i < n; i++ {
		name := "b" + itoa(i)
		trees[i] = branchFrom(t, g, base, name, func(d string) {
			write(t, d, name+".txt", name+"\n")
		})
	}
	// Reference: sequential merge-tree fold.
	acc := base
	for _, tr := range trees {
		mt, err := g.MergeTree(ctx, base, acc, tr)
		if err != nil || !mt.OK {
			t.Fatalf("fold merge-tree: ok=%v err=%v", mt.OK, err)
		}
		acc, err = g.CommitTree(ctx, mt.Tree, []string{acc, tr}, "fold")
		if err != nil {
			t.Fatal(err)
		}
	}
	wantPaths, err := g.LsTree(ctx, acc)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(wantPaths)

	overlay, err := g.OverlayTrees(ctx, base, trees)
	if err != nil {
		t.Fatal(err)
	}
	gotPaths, err := g.LsTree(ctx, overlay)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(gotPaths)
	if len(gotPaths) != len(wantPaths) {
		t.Fatalf("overlay paths %d != fold paths %d", len(gotPaths), len(wantPaths))
	}
	for i := range gotPaths {
		if gotPaths[i] != wantPaths[i] {
			t.Fatalf("overlay path %q != fold %q", gotPaths[i], wantPaths[i])
		}
	}
	// The fold's TREE and the overlay tree must be byte-identical.
	foldTree, err := g.TreeOID(ctx, acc)
	if err != nil {
		t.Fatal(err)
	}
	if overlay != foldTree {
		t.Fatalf("overlay tree %s != fold tree %s", overlay, foldTree)
	}
}

// TestDiffNameOnlyBatchMatchesPerBranch is the correctness anchor for
// DiffNameOnlyBatch: its result for every branch must equal what a per-branch
// DiffNameOnly loop (the code path it replaces) would have produced, including
// a branch with NO changes vs base (contributes no diff-tree block at all) and
// a path containing spaces (must survive the -z NUL-delimited decode intact).
func TestDiffNameOnlyBatchMatchesPerBranch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "keep.txt", "k\n")
	write(t, dir, "mod.txt", "orig\n")
	write(t, dir, "gone.txt", "g\n")
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}

	branchFrom(t, g, base, "h1", func(d string) {
		write(t, d, "mod.txt", "changed\n")
		write(t, d, "new dir/file with spaces.txt", "space\n")
	})
	branchFrom(t, g, base, "h2", func(d string) {
		_ = os.Remove(filepath.Join(d, "gone.txt"))
		write(t, d, "added.txt", "new\n")
	})
	branchFrom(t, g, base, "h3-nochange", func(d string) {})
	// h4 spans TWO commits, so its write-set only matches base...head (not
	// head's single most recent commit vs its immediate parent).
	h4wt := filepath.Join(t.TempDir(), "h4-multi")
	if err := g.WorktreeAdd(ctx, h4wt, "h4-multi", base); err != nil {
		t.Fatal(err)
	}
	write(t, h4wt, "c1.txt", "c1\n")
	if _, err := g.At(h4wt).CommitAll(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	write(t, h4wt, "c2.txt", "c2\n")
	if _, err := g.At(h4wt).CommitAll(ctx, "c2"); err != nil {
		t.Fatal(err)
	}
	if err := g.WorktreeRemove(ctx, h4wt); err != nil {
		t.Fatal(err)
	}

	branches := []string{"h1", "h2", "h3-nochange", "h4-multi"}

	want := map[string][]string{}
	for _, b := range branches {
		paths, err := g.DiffNameOnly(ctx, base, b)
		if err != nil {
			t.Fatalf("DiffNameOnly(%s): %v", b, err)
		}
		sort.Strings(paths)
		want[b] = paths
	}

	got, err := g.DiffNameOnlyBatch(ctx, base, branches)
	if err != nil {
		t.Fatalf("DiffNameOnlyBatch: %v", err)
	}
	if len(got) != len(branches) {
		t.Fatalf("DiffNameOnlyBatch returned %d entries, want %d", len(got), len(branches))
	}
	for _, b := range branches {
		gotPaths := append([]string(nil), got[b]...)
		sort.Strings(gotPaths)
		wantPaths := want[b]
		if len(gotPaths) != len(wantPaths) {
			t.Fatalf("branch %q: batched=%v, per-branch=%v", b, gotPaths, wantPaths)
		}
		for i := range gotPaths {
			if gotPaths[i] != wantPaths[i] {
				t.Fatalf("branch %q: batched=%v, per-branch=%v", b, gotPaths, wantPaths)
			}
		}
	}
	// The no-change branch must come back empty, not merely "absent".
	if len(got["h3-nochange"]) != 0 {
		t.Fatalf("h3-nochange write-set = %v, want empty", got["h3-nochange"])
	}
}

// TestDiffNameOnlyBatchSupersetWhenBaseAdvanced covers the case
// integrateBranches relies on: baseSHA has moved PAST a branch's fork point
// (e.g. other branches already landed onto base before this one is diffed).
// DiffNameOnlyBatch's two-tree diff (base tip vs branch tip) necessarily picks
// up base's own post-fork changes too, so it must be a SUPERSET of the
// three-dot DiffNameOnly(base, branch) result, which only shows the branch's
// changes since the merge-base — the extra paths are exactly the conservative,
// partition-safe behavior callers depend on. A path genuinely changed on both
// sides must appear in both results.
func TestDiffNameOnlyBatchSupersetWhenBaseAdvanced(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.txt", "1\n")
	write(t, dir, "b.txt", "1\n")
	write(t, dir, "c.txt", "1\n")
	fork, err := g.CommitAll(ctx, "fork")
	if err != nil {
		t.Fatal(err)
	}

	// Branch forks here, changing a.txt (its own) and c.txt (base will ALSO
	// change this one below — the genuine overlap).
	branch := branchFrom(t, g, fork, "b1", func(d string) {
		write(t, d, "a.txt", "branch-a\n")
		write(t, d, "c.txt", "branch-c\n")
	})

	// Base advances past the fork point without the branch: b.txt is a
	// base-only change (branch never touched it); c.txt is the genuine
	// overlap (base changed it independently of the branch).
	write(t, dir, "b.txt", "base-b\n")
	write(t, dir, "c.txt", "base-c\n")
	baseAdvanced, err := g.CommitAll(ctx, "base-advances")
	if err != nil {
		t.Fatal(err)
	}

	threeDot, err := g.DiffNameOnly(ctx, baseAdvanced, branch)
	if err != nil {
		t.Fatalf("DiffNameOnly: %v", err)
	}
	batch, err := g.DiffNameOnlyBatch(ctx, baseAdvanced, []string{branch})
	if err != nil {
		t.Fatalf("DiffNameOnlyBatch: %v", err)
	}
	twoTree := batch[branch]

	got := map[string]bool{}
	for _, p := range twoTree {
		got[p] = true
	}
	for _, p := range threeDot {
		if !got[p] {
			t.Fatalf("two-tree result %v is missing three-dot path %q — not a superset", twoTree, p)
		}
	}
	// The genuinely-overlapping path (changed on both sides) must be caught.
	if !got["c.txt"] {
		t.Fatalf("two-tree result %v missing genuinely-overlapping path c.txt", twoTree)
	}
	// The base-only path is exactly the extra conservatism: present in
	// two-tree, absent from three-dot (which only looks at the branch's own
	// changes since the merge-base).
	if !got["b.txt"] {
		t.Fatalf("two-tree result %v missing base-only path b.txt (conservatism)", twoTree)
	}
	for _, p := range threeDot {
		if p == "b.txt" {
			t.Fatalf("three-dot unexpectedly included base-only path b.txt: %v", threeDot)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
