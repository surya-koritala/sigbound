package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
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

// TestUpdateRefCASRefusesAMovedRef pins the primitive every landing path now
// swaps through: the update applies only while the ref still holds the value the
// caller computed against, a lost race is ErrRefMoved (and writes NOTHING), and a
// genuine update-ref failure is NOT reported as one — the two answers send a
// caller in opposite directions, so conflating them is the bug this prevents.
func TestUpdateRefCASRefusesAMovedRef(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	c1 := branchFrom(t, g, base, "src1", func(d string) { write(t, d, "one.txt", "1\n") })
	c2 := branchFrom(t, g, base, "src2", func(d string) { write(t, d, "two.txt", "2\n") })

	// The ref is where the caller left it: the swap applies.
	if err := g.UpdateRefCAS(ctx, "refs/heads/main", c1, base); err != nil {
		t.Fatalf("CAS against the current value: %v", err)
	}
	if head, err := g.RevParse(ctx, "main"); err != nil || head != c1 {
		t.Fatalf("main = %s (err %v), want %s", head, err, c1)
	}

	// Somebody else got there first: refused, and c1 survives untouched.
	err := g.UpdateRefCAS(ctx, "refs/heads/main", c2, base)
	if !errors.Is(err, ErrRefMoved) {
		t.Fatalf("CAS against a stale value: err = %v, want ErrRefMoved", err)
	}
	if head, rerr := g.RevParse(ctx, "main"); rerr != nil || head != c1 {
		t.Fatalf("a refused swap moved main to %s (err %v); it must still be %s", head, rerr, c1)
	}
	// git's message names both values, so an operator can see who won.
	if !strings.Contains(err.Error(), c1) || !strings.Contains(err.Error(), base) {
		t.Fatalf("refusal %q names neither the value found (%s) nor the one expected (%s)", err, c1, base)
	}

	// A ref that does not exist at all is a broken caller, not a lost race.
	if err := g.UpdateRefCAS(ctx, "refs/heads/absent", c2, base); err == nil || errors.Is(err, ErrRefMoved) {
		t.Fatalf("updating a missing ref: err = %v, want a plain failure (never ErrRefMoved)", err)
	}
	// Empty/whitespace fields never reach git: they would silently change the
	// stdin directive's arity, turning a compare-and-swap into a plain write.
	for _, bad := range [][3]string{{"refs/heads/main", c2, ""}, {"refs/heads/main", "", base}, {"refs/heads/a b", c2, base}} {
		if err := g.UpdateRefCAS(ctx, bad[0], bad[1], bad[2]); err == nil {
			t.Fatalf("UpdateRefCAS%v = nil, want a rejection", bad)
		}
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

// TestUpdateRefCASPinsTheMessageLocale holds the LC_ALL=C pin, which nothing
// else can: hermeticEnv passes os.Environ() straight through, so on a git with
// translations installed the caller's own locale reaches the child and git's
// refusal comes back translated. The discriminant is that message, and the cost
// of missing it is NOT symmetric with the cost of a false positive — a lost race
// read as a plain error is what leaves an ack terminally resolved with nothing
// landed (writeParkCAS has already written resolvedAt/landedSHA by then, and
// refuseAck never runs to take them back). A real git cannot demonstrate this
// unless the machine has translations installed, so the git here is a shim that
// speaks German to anyone who did not pin the locale.
//
// Remove the LC_ALL=C from UpdateRefCAS and this fails: the shim takes its
// translated branch, "but expected" misses, and the refusal comes back as a
// plain error. LC_ALL is set to a non-C value first so the pin is what makes the
// difference and never the developer's ambient environment.
func TestUpdateRefCASPinsTheMessageLocale(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell-script git shim, not executable on Windows")
	}
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	dir := t.TempDir()
	shim := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"if [ \"$LC_ALL\" = C ]; then\n" +
		"  echo \"error: cannot lock ref 'refs/heads/main': is at 1111111111111111111111111111111111111111 but expected 2222222222222222222222222222222222222222\" >&2\n" +
		"else\n" +
		"  echo 'Fehler: Sperren der Referenz refs/heads/main nicht moeglich: steht auf 1111111111111111111111111111111111111111, erwartet wurde 2222222222222222222222222222222222222222' >&2\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	g := New(dir).WithBinary(shim)
	err := g.UpdateRefCAS(context.Background(), "refs/heads/main", strings.Repeat("1", 40), strings.Repeat("2", 40))
	if !errors.Is(err, ErrRefMoved) {
		t.Fatalf("refusal from a localized git = %v, want ErrRefMoved — LC_ALL=C did not reach the child, so the discriminant read a translated message", err)
	}
}

// TestUpdateRefCASRejectsANameAsTheExpectedValue pins the field that must be an
// OID. git resolves a NAME here at compare time, so passing the ref being
// updated would compare it against whatever it holds right now and pass always —
// a compare-and-swap silently degraded into the plain write it replaced. That
// failure is invisible at every call site, so it is refused at this one.
func TestUpdateRefCASRejectsANameAsTheExpectedValue(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	c1 := branchFrom(t, g, base, "src1", func(d string) { write(t, d, "one.txt", "1\n") })

	for _, name := range []string{"main", "refs/heads/main", "HEAD", base[:8]} {
		if err := g.UpdateRefCAS(ctx, "refs/heads/main", c1, name); err == nil {
			t.Fatalf("UpdateRefCAS accepted %q as the expected old value: a name resolves at compare time, so the swap can never refuse", name)
		}
	}
	if head, err := g.RevParse(ctx, "main"); err != nil || head != base {
		t.Fatalf("main = %s (err %v), want %s untouched — a rejected call must write nothing", head, err, base)
	}
	// The OID form of the very same value still works: the guard is on the
	// SHAPE of the field, not on comparing a ref to itself.
	if err := g.UpdateRefCAS(ctx, "refs/heads/main", c1, base); err != nil {
		t.Fatalf("CAS with the base's own OID: %v", err)
	}
}
