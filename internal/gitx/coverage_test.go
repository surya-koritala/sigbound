package gitx

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// --- trivial accessors + binary override ------------------------------------

func TestWithBinaryAndDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if g.Dir() != dir {
		t.Fatalf("Dir() = %q, want %q", g.Dir(), dir)
	}
	// WithBinary keeps the directory and yields a working handle.
	g2 := g.WithBinary("git")
	if g2.Dir() != dir {
		t.Fatalf("WithBinary lost dir: %q", g2.Dir())
	}
	if err := g2.Init(ctx); err != nil {
		t.Fatalf("Init via WithBinary handle: %v", err)
	}
	// At() also preserves the (overridden) binary.
	if got := g2.At("/other").Dir(); got != "/other" {
		t.Fatalf("At().Dir() = %q", got)
	}
}

// --- HasUncommittedChanges: clean, dirty, and error -------------------------

func TestHasUncommittedChanges(t *testing.T) {
	ctx := context.Background()
	g, _ := newRepo(t)

	dirty, err := g.HasUncommittedChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("fresh commit should leave a clean tree")
	}

	// Untracked file makes it dirty.
	writeFile(t, g.Dir(), "u.txt", "scratch\n")
	dirty, err = g.HasUncommittedChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("untracked file should mark tree dirty")
	}

	// Error path: a directory that is not a git repo.
	bad := New(filepath.Join(t.TempDir(), "not-a-repo"))
	if _, err := bad.HasUncommittedChanges(ctx); err == nil {
		t.Fatal("expected error on non-existent repo")
	}
}

// --- Add: empty no-op, targeted staging, pathspec error ---------------------

func TestAddSpecificPaths(t *testing.T) {
	ctx := context.Background()
	g, _ := newRepo(t)

	// No paths => no-op, no error, nothing staged.
	if err := g.Add(ctx); err != nil {
		t.Fatalf("empty Add should be a no-op: %v", err)
	}

	writeFile(t, g.Dir(), "a.txt", "a\n")
	writeFile(t, g.Dir(), "b.txt", "b\n")
	if err := g.Add(ctx, "a.txt"); err != nil {
		t.Fatal(err)
	}
	sha, err := g.Commit(ctx, "add only a")
	if err != nil {
		t.Fatal(err)
	}
	files, err := g.LsTree(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	if !got["a.txt"] {
		t.Fatalf("a.txt should be committed, tree=%v", files)
	}
	if got["b.txt"] {
		t.Fatalf("b.txt should NOT be committed (only a.txt staged), tree=%v", files)
	}

	// Error path: staging a path that does not exist.
	if err := g.Add(ctx, "does-not-exist.txt"); err == nil {
		t.Fatal("expected pathspec error for missing file")
	}
}

// --- WorktreeAddSparse + CheckoutPaths --------------------------------------

func TestWorktreeAddSparseAndCheckoutPaths(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	// Add a second tracked file so the index carries more than the edited path.
	writeFile(t, g.Dir(), "extra.txt", "extra\n")
	base2, err := g.CommitAll(ctx, "add extra")
	if err != nil {
		t.Fatal(err)
	}
	_ = base

	wt := filepath.Join(t.TempDir(), "sparse")
	if err := g.WorktreeAddSparse(ctx, wt, "sp", base2); err != nil {
		t.Fatal(err)
	}
	wg := g.At(wt)

	// No checkout: base.txt must NOT be on disk, yet the index knows it.
	if _, err := os.Stat(filepath.Join(wt, "base.txt")); !os.IsNotExist(err) {
		t.Fatalf("sparse worktree should not materialize base.txt: %v", err)
	}
	idx, err := wg.run(ctx, "ls-files")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(idx, "base.txt") || !contains(idx, "extra.txt") {
		t.Fatalf("index should hold base paths, got %q", idx)
	}

	// Empty CheckoutPaths => no-op.
	if err := wg.CheckoutPaths(ctx); err != nil {
		t.Fatalf("empty CheckoutPaths should be a no-op: %v", err)
	}
	// Materialize just base.txt and edit it.
	if err := wg.CheckoutPaths(ctx, "base.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "base.txt")); err != nil {
		t.Fatalf("base.txt not materialized: %v", err)
	}
	writeFile(t, wt, "base.txt", "base\nedited\n")
	if err := wg.Add(ctx, "base.txt"); err != nil {
		t.Fatal(err)
	}
	sha, err := wg.Commit(ctx, "sparse edit")
	if err != nil {
		t.Fatal(err)
	}
	// The commit's tree is complete: extra.txt survives from the index even
	// though it never touched disk (the O(1) index trick).
	files, err := g.LsTree(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	if !got["base.txt"] || !got["extra.txt"] {
		t.Fatalf("sparse commit lost a path, tree=%v", files)
	}
	body, _, err := g.BlobAt(ctx, sha, "base.txt")
	if err != nil {
		t.Fatal(err)
	}
	if body != "base\nedited\n" {
		t.Fatalf("base.txt not the edited content: %q", body)
	}

	// Error path: bad base cannot seed a worktree.
	if err := g.WorktreeAddSparse(ctx, filepath.Join(t.TempDir(), "x"), "bad",
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err == nil {
		t.Fatal("expected error for bad base")
	}
}

// --- WorktreePopulateSparse + AddAllSparse (issue #86) ----------------------

// TestWorktreePopulateSparse proves the #86 sparse populate: a --no-checkout
// worktree materialized with WorktreePopulateSparse puts ONLY the named paths
// on disk while the index stays complete, and — the property lane enforcement
// rides on — an out-of-lane write staged with AddAllSparse still lands in the
// commit (so a base...head diff surfaces it) WITHOUT the untouched
// skip-worktree files being dropped as spurious deletions. A pattern that
// matches nothing in the tree (here go.sum, which this repo lacks) is not an
// error — it simply materializes nothing.
func TestWorktreePopulateSparse(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t) // base.txt
	// A tracked file the lane will NOT include, plus a go.mod, so we can prove
	// the out-of-lane file stays off disk yet survives in the committed tree.
	writeFile(t, g.Dir(), "other.txt", "other\n")
	writeFile(t, g.Dir(), "go.mod", "module x\n")
	base2, err := g.CommitAll(ctx, "add other + go.mod")
	if err != nil {
		t.Fatal(err)
	}
	_ = base

	wt := filepath.Join(t.TempDir(), "sparse")
	if err := g.WorktreeAddNoCheckout(ctx, wt, "sp", base2); err != nil {
		t.Fatal(err)
	}
	wg := g.At(wt)

	// Empty paths is a caller bug, never a silent empty checkout.
	if err := wg.WorktreePopulateSparse(ctx, nil); err == nil {
		t.Fatal("WorktreePopulateSparse with no paths: want error, got nil")
	}

	// Lane = base.txt + module metadata (go.sum absent from this repo).
	if err := wg.WorktreePopulateSparse(ctx, []string{"base.txt", "go.mod", "go.sum"}); err != nil {
		t.Fatalf("WorktreePopulateSparse: %v", err)
	}
	// Only the lane is on disk...
	if _, err := os.Stat(filepath.Join(wt, "base.txt")); err != nil {
		t.Fatalf("lane file base.txt not materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "other.txt")); !os.IsNotExist(err) {
		t.Fatalf("out-of-lane other.txt should not be on disk: %v", err)
	}
	// ...but the index is complete (skip-worktree hides the rest).
	idx, err := wg.run(ctx, "ls-files")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(idx, "base.txt") || !contains(idx, "other.txt") || !contains(idx, "go.mod") {
		t.Fatalf("sparse index should hold every base path, got %q", idx)
	}

	// An out-of-lane stray write: AddAllSparse must stage it (so it reaches the
	// commit and a base...head diff surfaces it) while leaving the untouched
	// skip-worktree files intact — never staged as deletions.
	writeFile(t, wt, "base.txt", "base\nedited\n")
	writeFile(t, wt, "stray.txt", "out of lane\n")
	if err := wg.AddAllSparse(ctx); err != nil {
		t.Fatalf("AddAllSparse: %v", err)
	}
	sha, err := wg.Commit(ctx, "sparse edit + stray")
	if err != nil {
		t.Fatal(err)
	}
	// The stray is in the diff (so lane enforcement can catch it), and the edit
	// too.
	changed, err := g.DiffNameOnly(ctx, base2, sha)
	if err != nil {
		t.Fatal(err)
	}
	cs := map[string]bool{}
	for _, f := range changed {
		cs[f] = true
	}
	if !cs["stray.txt"] || !cs["base.txt"] {
		t.Fatalf("sparse diff must surface the stray + edit, got %v", changed)
	}
	// The committed tree is COMPLETE: other.txt never touched disk yet survives,
	// go.mod too, and the stray was added — nothing dropped by the sparse add.
	files, err := g.LsTree(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	for _, want := range []string{"base.txt", "other.txt", "go.mod", "stray.txt"} {
		if !got[want] {
			t.Fatalf("sparse commit lost/dropped %q, tree=%v", want, files)
		}
	}
	body, _, err := g.BlobAt(ctx, sha, "other.txt")
	if err != nil {
		t.Fatal(err)
	}
	if body != "other\n" {
		t.Fatalf("out-of-lane other.txt corrupted in tree: %q", body)
	}
}

// mkfile writes content to dir/rel, creating parent directories (writeFile
// does not) — needed for the nested-path sparse-pattern fixtures below.
func mkfile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWorktreePopulateSparseAnchorsToRoot is issue #86 review FINDING 1: a
// --no-cone pattern is an UNANCHORED gitignore glob, so a bare "go.mod"/"main.go"
// materializes copies at EVERY depth (sub/mod/go.mod, sub/main.go). sparsePattern
// anchors each path to the repo root, so only the declared root paths land.
func TestWorktreePopulateSparseAnchorsToRoot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	mkfile(t, dir, "go.mod", "module root\n")
	mkfile(t, dir, "sub/mod/go.mod", "module nested\n") // a NESTED go.mod
	mkfile(t, dir, "main.go", "package main\n")
	mkfile(t, dir, "sub/main.go", "package sub\n") // a deeper main.go
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(t.TempDir(), "sparse")
	if err := g.WorktreeAddNoCheckout(ctx, wt, "anchor", base); err != nil {
		t.Fatal(err)
	}
	// Lane = root main.go, plus the go.mod/go.sum the driver always appends.
	if err := g.At(wt).WorktreePopulateSparse(ctx, []string{"main.go", "go.mod", "go.sum"}); err != nil {
		t.Fatalf("WorktreePopulateSparse: %v", err)
	}
	onDisk := func(rel string) bool {
		_, err := os.Stat(filepath.Join(wt, rel))
		return err == nil
	}
	if !onDisk("main.go") || !onDisk("go.mod") {
		t.Fatal("declared root main.go/go.mod not materialized")
	}
	if onDisk("sub/main.go") {
		t.Fatal("over-match: unanchored pattern materialized nested sub/main.go")
	}
	if onDisk("sub/mod/go.mod") {
		t.Fatal("over-match: unanchored pattern materialized nested sub/mod/go.mod")
	}
}

// TestWorktreePopulateSparseEscapesGlobChars is issue #86 review FINDING 2: a
// lane file whose name contains gitignore metacharacters (here "[") is read as a
// character class and matches NOTHING, so the agent never sees its own declared
// file. sparsePattern escapes the metacharacters, so the literal file is
// materialized and editable.
func TestWorktreePopulateSparseEscapesGlobChars(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	const lane = "pkg/fixture[1].json"
	mkfile(t, dir, lane, "orig\n")
	mkfile(t, dir, "pkg/other.json", "other\n") // a sibling the glob "[1]" must NOT pull in
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(t.TempDir(), "sparse")
	if err := g.WorktreeAddNoCheckout(ctx, wt, "escape", base); err != nil {
		t.Fatal(err)
	}
	wg := g.At(wt)
	if err := wg.WorktreePopulateSparse(ctx, []string{lane}); err != nil {
		t.Fatalf("WorktreePopulateSparse: %v", err)
	}
	// The declared file — glob metachars and all — is on disk and editable.
	if _, err := os.Stat(filepath.Join(wt, lane)); err != nil {
		t.Fatalf("under-match: declared lane file %q not materialized: %v", lane, err)
	}
	// "[1]" is a literal, not a class: pkg/other.json must NOT come along.
	if _, err := os.Stat(filepath.Join(wt, "pkg/other.json")); err == nil {
		t.Fatal("escaped pattern still matched a sibling (pkg/other.json materialized)")
	}
	// Edit it and commit: the agent can actually work in its lane.
	if err := os.WriteFile(filepath.Join(wt, lane), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wg.AddAllSparse(ctx); err != nil {
		t.Fatal(err)
	}
	sha, err := wg.Commit(ctx, "edit fixture")
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := g.BlobAt(ctx, sha, lane)
	if err != nil {
		t.Fatal(err)
	}
	if body != "edited\n" {
		t.Fatalf("declared file not editable in sparse worktree: blob=%q", body)
	}
}

// --- WorktreeAddDetached ----------------------------------------------------

func TestWorktreeAddDetached(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	c := branchFrom(t, g, base, "feat", func(d string) { write(t, d, "f.txt", "f\n") })

	wt := filepath.Join(t.TempDir(), "det")
	if err := g.WorktreeAddDetached(ctx, wt, c); err != nil {
		t.Fatal(err)
	}
	wg := g.At(wt)

	head, err := wg.HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head != c {
		t.Fatalf("detached HEAD = %s, want %s", head, c)
	}
	// Detached: HEAD is not a symbolic ref (no branch).
	if _, err := wg.run(ctx, "symbolic-ref", "-q", "HEAD"); err == nil {
		t.Fatal("expected detached HEAD (symbolic-ref should fail)")
	}
	// The commit's tree is checked out.
	if _, err := os.Stat(filepath.Join(wt, "f.txt")); err != nil {
		t.Fatalf("detached worktree missing f.txt: %v", err)
	}

	// Error path: bad commit.
	if err := g.WorktreeAddDetached(ctx, filepath.Join(t.TempDir(), "x"),
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err == nil {
		t.Fatal("expected error for bad commit")
	}
}

// --- CheckoutDetach + Clean + ResetHard: reusing one detached worktree -----

// TestCheckoutDetachCleanAndResetHard covers the three primitives
// -verify/-repair use to reuse ONE detached worktree across attempts instead
// of a fresh WorktreeAddDetached + WorktreeRemove per attempt: CheckoutDetach
// advances the SAME worktree onto a new commit in place, Clean removes
// whatever untracked an invocation left behind, and ResetHard reverts
// whatever tracked-file edit it left behind — Clean and ResetHard together
// are what let the next reuse start hermetic (Clean alone only covers half of
// that; see gitx.go's docs on both).
func TestCheckoutDetachCleanAndResetHard(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	c := branchFrom(t, g, base, "feat", func(d string) { write(t, d, "f.txt", "f\n") })

	wt := filepath.Join(t.TempDir(), "det")
	if err := g.WorktreeAddDetached(ctx, wt, base); err != nil {
		t.Fatal(err)
	}
	wg := g.At(wt)

	// CheckoutDetach advances the SAME worktree onto a different commit —
	// no fresh worktree add.
	if err := wg.CheckoutDetach(ctx, c); err != nil {
		t.Fatal(err)
	}
	head, err := wg.HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head != c {
		t.Fatalf("HEAD after CheckoutDetach = %s, want %s", head, c)
	}
	if _, err := os.Stat(filepath.Join(wt, "f.txt")); err != nil {
		t.Fatalf("checked-out file missing after CheckoutDetach: %v", err)
	}
	// Still detached (no branch).
	if _, err := wg.run(ctx, "symbolic-ref", "-q", "HEAD"); err == nil {
		t.Fatal("expected detached HEAD after CheckoutDetach")
	}

	// Clean removes an untracked file (e.g. a build artifact a verify command
	// left behind) but leaves tracked files alone.
	writeFile(t, wt, "leftover.txt", "artifact\n")
	if err := wg.Clean(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "leftover.txt")); !os.IsNotExist(err) {
		t.Fatalf("Clean did not remove the untracked leftover: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "f.txt")); err != nil {
		t.Fatalf("Clean removed a tracked file: %v", err)
	}

	// ResetHard reverts a MODIFICATION to a tracked file — the case Clean
	// explicitly does not cover (Clean only deletes untracked/ignored paths).
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("mutated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wg.ResetHard(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "f\n" {
		t.Fatalf("f.txt after ResetHard = %q, want original tracked content %q", got, "f\n")
	}

	// Error paths.
	if err := wg.CheckoutDetach(ctx, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err == nil {
		t.Fatal("expected error for bad commit")
	}
	bad := New(filepath.Join(t.TempDir(), "not-a-repo"))
	if err := bad.Clean(ctx); err == nil {
		t.Fatal("expected error cleaning a non-repo directory")
	}
	if err := bad.ResetHard(ctx); err == nil {
		t.Fatal("expected error resetting a non-repo directory")
	}
}

// --- CheckoutB --------------------------------------------------------------

func TestCheckoutB(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	b1 := branchFrom(t, g, base, "one", func(d string) { write(t, d, "one.txt", "1\n") })

	intDir := filepath.Join(t.TempDir(), "int")
	if err := g.WorktreeAdd(ctx, intDir, "integration", base); err != nil {
		t.Fatal(err)
	}
	ig := g.At(intDir)

	// Reset the branch forward onto b1.
	if err := ig.CheckoutB(ctx, "integration", b1); err != nil {
		t.Fatal(err)
	}
	if head, _ := ig.HeadSHA(ctx); head != b1 {
		t.Fatalf("after CheckoutB HEAD=%s, want %s", head, b1)
	}
	// Rewind back to base.
	if err := ig.CheckoutB(ctx, "integration", base); err != nil {
		t.Fatal(err)
	}
	if head, _ := ig.HeadSHA(ctx); head != base {
		t.Fatalf("after rewind HEAD=%s, want %s", head, base)
	}

	// Error path: bad start point.
	if err := ig.CheckoutB(ctx, "integration",
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err == nil {
		t.Fatal("expected error for bad start")
	}
}

// --- BranchDelete -------------------------------------------------------

func TestBranchDelete(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	commit := branchFrom(t, g, base, "throwaway", func(d string) { write(t, d, "x.txt", "1\n") })

	if _, err := g.run(ctx, "rev-parse", "--verify", "refs/heads/throwaway"); err != nil {
		t.Fatalf("branch should exist before delete: %v", err)
	}
	if err := g.BranchDelete(ctx, "throwaway"); err != nil {
		t.Fatalf("BranchDelete: %v", err)
	}
	if _, err := g.run(ctx, "rev-parse", "--verify", "refs/heads/throwaway"); err == nil {
		t.Fatal("branch still resolves after BranchDelete")
	}
	// The commit itself survives (unreachable, not deleted) — BranchDelete only
	// removes the ref, matching its doc comment.
	if _, err := g.RevParse(ctx, commit); err != nil {
		t.Fatalf("commit %s should still resolve after its branch ref was deleted: %v", commit, err)
	}

	// Error path: deleting a branch that never existed.
	if err := g.BranchDelete(ctx, "never-existed"); err == nil {
		t.Fatal("want an error deleting a nonexistent branch")
	}
}

// --- Init / Commit / AddAll error paths -------------------------------------

func TestInitMkdirError(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// Create a regular file, then try to Init at a path *below* it: MkdirAll
	// cannot create a directory under a file.
	filePath := filepath.Join(dir, "afile")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := New(filepath.Join(filePath, "sub"))
	if err := g.Init(ctx); err == nil {
		t.Fatal("expected MkdirAll error initializing under a file")
	}
}

func TestCommitAndAddAllInNonRepo(t *testing.T) {
	ctx := context.Background()
	g := New(t.TempDir()) // never Init'd
	if err := g.AddAll(ctx); err == nil {
		t.Fatal("AddAll in non-repo should error")
	}
	if _, err := g.Commit(ctx, "x"); err == nil {
		t.Fatal("Commit in non-repo should error")
	}
	if _, err := g.CommitAll(ctx, "x"); err == nil {
		t.Fatal("CommitAll in non-repo should error")
	}
}

// --- run: process-cannot-run (err) branch, via a bogus binary ---------------

func TestRunProcessCannotStart(t *testing.T) {
	ctx := context.Background()
	g := New(t.TempDir()).WithBinary("git-nonexistent-binary-xyzzy")
	if _, err := g.HeadSHA(ctx); err == nil {
		t.Fatal("expected exec error for missing binary")
	}
	// BlobAt's err (not just !present) branch runs through the same failure.
	if _, _, err := g.BlobAt(ctx, "HEAD", "x"); err == nil {
		t.Fatal("expected exec error from BlobAt with missing binary")
	}
	// DiffRaw's err branch too.
	if _, err := g.DiffRaw(ctx, "a", "b"); err == nil {
		t.Fatal("expected exec error from DiffRaw with missing binary")
	}
}

// --- read helpers: bad-revision error paths ---------------------------------

func TestReadHelpersBadRevision(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)

	if _, err := g.RevParse(ctx, "no-such-ref"); err == nil {
		t.Fatal("RevParse bad ref")
	}
	if _, err := g.TreeOID(ctx, "no-such-ref"); err == nil {
		t.Fatal("TreeOID bad ref")
	}
	if _, err := g.DiffNameOnly(ctx, "no-such", "main"); err == nil {
		t.Fatal("DiffNameOnly bad base")
	}
	if _, err := g.LsTree(ctx, "no-such-ref"); err == nil {
		t.Fatal("LsTree bad ref")
	}
	if _, err := g.ShowFile(ctx, "HEAD", "no-such-path"); err == nil {
		t.Fatal("ShowFile missing path")
	}
	if _, err := g.DiffRaw(ctx, "no-such", "main"); err == nil {
		t.Fatal("DiffRaw bad rev")
	}
	// Identical revisions => empty diff (exercises splitLines empty branch).
	paths, err := g.DiffNameOnly(ctx, base, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("DiffNameOnly(base,base) = %v, want empty", paths)
	}
}

// --- MergeTree: conflict (OID + paths) and hard error -----------------------

func TestMergeTreeConflictAndError(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "c.txt", "base\n")
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	ours := branchFrom(t, g, base, "ours", func(d string) { write(t, d, "c.txt", "ours\n") })
	theirs := branchFrom(t, g, base, "theirs", func(d string) { write(t, d, "c.txt", "theirs\n") })

	mt, err := g.MergeTree(ctx, base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if mt.OK {
		t.Fatal("expected a conflict on overlapping edits")
	}
	if mt.Tree == "" {
		t.Fatal("conflicted merge-tree must still return a tree OID")
	}
	if len(mt.Conflicts) != 1 || mt.Conflicts[0] != "c.txt" {
		t.Fatalf("conflicts = %v, want [c.txt]", mt.Conflicts)
	}
	// The written tree holds a conflicted blob with merge markers.
	body, present, err := g.BlobAt(ctx, mt.Tree, "c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !present || !contains(body, "<<<<<<<") || !contains(body, ">>>>>>>") {
		t.Fatalf("expected conflict markers in merged blob:\n%s", body)
	}

	// Hard error (exit != 0/1): a bogus merge-base.
	if _, err := g.MergeTree(ctx,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", ours, theirs); err == nil {
		t.Fatal("expected error for bad merge-base")
	}
}

// --- Merge: bad ref -> error (no recorded conflicts) ------------------------

func TestMergeBadRefError(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	intDir := filepath.Join(t.TempDir(), "int")
	if err := g.WorktreeAdd(ctx, intDir, "integration", base); err != nil {
		t.Fatal(err)
	}
	ig := g.At(intDir)
	_, err := ig.Merge(ctx, "m", "no-such-branch")
	if err == nil {
		t.Fatal("merging a non-existent ref should surface an error")
	}
	// The "failed without recorded conflicts" error must carry git's actual
	// stderr, not just that path — otherwise an operational failure (bad
	// ref, index.lock, octopus refusal) is unfixable when it flakes: there's
	// nothing to read (see issue #69).
	if !contains(err.Error(), "not something we can merge") {
		t.Fatalf("error should surface git's stderr, got: %v", err)
	}
}

// --- HashObject + BlobAt (present, absent, roundtrip) -----------------------

func TestHashObjectAndBlobAt(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)

	oid, err := g.HashObject(ctx, []byte("hello world\n"))
	if err != nil || oid == "" {
		t.Fatalf("HashObject: oid=%q err=%v", oid, err)
	}
	// Content-addressed: identical bytes hash to the same OID.
	oid2, err := g.HashObject(ctx, []byte("hello world\n"))
	if err != nil || oid2 != oid {
		t.Fatalf("HashObject not deterministic: %q vs %q (err %v)", oid, oid2, err)
	}
	// The blob is now readable through cat-file --batch (type + size).
	br, err := g.NewBatchReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer br.Close()
	got, typ, size, ok, err := br.Resolve(oid)
	if err != nil || !ok {
		t.Fatalf("resolve hashed blob: ok=%v err=%v", ok, err)
	}
	if got != oid || typ != "blob" || size != 12 {
		t.Fatalf("hashed blob = %s/%s/%d, want %s/blob/12", got, typ, size, oid)
	}

	// BlobAt present.
	body, present, err := g.BlobAt(ctx, base, "base.txt")
	if err != nil || !present {
		t.Fatalf("BlobAt present: present=%v err=%v", present, err)
	}
	if body != "base\n" {
		t.Fatalf("BlobAt content = %q, want %q", body, "base\n")
	}
	// BlobAt absent => present=false, empty, NO error.
	body, present, err = g.BlobAt(ctx, base, "no-such.txt")
	if err != nil {
		t.Fatalf("BlobAt absent must not error: %v", err)
	}
	if present || body != "" {
		t.Fatalf("absent path should be empty/false, got %q present=%v", body, present)
	}
}

// --- BlobsBatch: equality vs BlobAt, binary content, empty blob, non-blob ---

// TestBlobsBatch proves the batched resolver-blob-read path (one `git
// cat-file --batch` for every conflicted path's base/ours/theirs specs)
// returns byte-identical content to the per-spec BlobAt calls it replaces —
// including a binary-ish body (embedded NUL and a non-UTF8 byte, no trailing
// newline) and an empty blob, both hashed directly so they never touch a
// working tree.
func TestBlobsBatch(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)

	binBody := "bin\x00ary\xffdata"
	binOID, err := g.HashObject(ctx, []byte(binBody))
	if err != nil {
		t.Fatal(err)
	}
	emptyOID, err := g.HashObject(ctx, []byte(""))
	if err != nil {
		t.Fatal(err)
	}

	specs := []string{
		base + ":base.txt",    // present, ordinary text, rev:path spec
		base + ":no-such.txt", // absent path
		binOID,                // present, binary content, bare OID
		emptyOID,              // present, empty content, bare OID
		base,                  // present but NOT a blob (a commit) -> excluded
	}
	got, err := g.BlobsBatch(ctx, specs)
	if err != nil {
		t.Fatal(err)
	}

	// Equality against BlobAt for the rev:path spec — the exact contract
	// attemptResolve relied on before it moved to this batched call.
	wantBase, present, err := g.BlobAt(ctx, base, "base.txt")
	if err != nil || !present {
		t.Fatalf("BlobAt base.txt: present=%v err=%v", present, err)
	}
	if got[base+":base.txt"] != wantBase {
		t.Fatalf("BlobsBatch base.txt = %q, want %q", got[base+":base.txt"], wantBase)
	}
	if c, ok := got[base+":no-such.txt"]; ok {
		t.Fatalf("BlobsBatch: absent path present in map: %q", c)
	}
	if got[binOID] != binBody {
		t.Fatalf("BlobsBatch binary content = %q, want %q", got[binOID], binBody)
	}
	if c, ok := got[emptyOID]; !ok || c != "" {
		t.Fatalf("BlobsBatch empty blob = %q ok=%v, want \"\"/true", c, ok)
	}
	if _, ok := got[base]; ok {
		t.Fatal("BlobsBatch: a commit OID (non-blob) must be excluded from the map")
	}

	// Empty input -> empty, non-nil map, no process spawned.
	if m, err := g.BlobsBatch(ctx, nil); err != nil || len(m) != 0 {
		t.Fatalf("BlobsBatch(nil) = %v err=%v, want empty", m, err)
	}

	// Error path: a spec containing a newline would desync the request stream
	// from the response stream (one spec per line).
	if _, err := g.BlobsBatch(ctx, []string{"bad\nspec"}); err == nil {
		t.Fatal("expected error for newline in spec")
	}
}

// --- SpliceBlobs: mode-preserving resolution + errors -----------------------

func TestSpliceBlobs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "reg.txt", "regular-original\n")
	write(t, dir, "exec.sh", "echo hi\n")
	if err := os.Chmod(filepath.Join(dir, "exec.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	baseTree, err := g.TreeOID(ctx, base)
	if err != nil {
		t.Fatal(err)
	}

	// No blobs => baseTree returned unchanged.
	if same, err := g.SpliceBlobs(ctx, baseTree, nil); err != nil || same != baseTree {
		t.Fatalf("empty splice: got %s err=%v, want %s", same, err, baseTree)
	}

	// Splice: regular file, executable file (mode must survive), and a brand-new
	// path absent in baseTree (added as a regular file).
	newTree, err := g.SpliceBlobs(ctx, baseTree, []ResolvedBlob{
		{Path: "reg.txt", Content: "regular-RESOLVED\n"},
		{Path: "exec.sh", Content: "echo resolved\n"},
		{Path: "added.txt", Content: "added-body\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if newTree == baseTree {
		t.Fatal("splice should produce a new tree")
	}
	for path, want := range map[string]string{
		"reg.txt":   "regular-RESOLVED\n",
		"exec.sh":   "echo resolved\n",
		"added.txt": "added-body\n",
	} {
		body, present, err := g.BlobAt(ctx, newTree, path)
		if err != nil || !present {
			t.Fatalf("BlobAt %s: present=%v err=%v", path, present, err)
		}
		if body != want {
			t.Fatalf("%s spliced content = %q, want %q", path, body, want)
		}
	}
	// Modes: regular stays 100644, executable stays 100755, new is 100644.
	// One entryModesBatch call resolves all three paths' modes.
	modes, err := g.entryModesBatch(ctx, newTree, []string{"reg.txt", "exec.sh", "added.txt"})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"reg.txt":   "100644",
		"exec.sh":   "100755",
		"added.txt": "100644",
	} {
		if got := modes[path]; got != want {
			t.Fatalf("%s mode = %q, want %q", path, got, want)
		}
	}
	// entryModesBatch: empty input -> empty map, no process spawned.
	if m, err := g.entryModesBatch(ctx, baseTree, nil); err != nil || len(m) != 0 {
		t.Fatalf("entryModesBatch(nil) = %v err=%v, want empty", m, err)
	}
	// entryModesBatch: an absent path is simply missing from the map (not an
	// error, no zero-value entry).
	absentModes, err := g.entryModesBatch(ctx, baseTree, []string{"totally-absent.xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if mode, ok := absentModes["totally-absent.xyz"]; ok {
		t.Fatalf("entryModesBatch absent path present in map: %q", mode)
	}

	// Error: a path containing a tab is rejected (update-index --index-info is
	// line-oriented).
	if _, err := g.SpliceBlobs(ctx, baseTree, []ResolvedBlob{
		{Path: "bad\tpath", Content: "x"},
	}); err == nil {
		t.Fatal("expected error for tab in path")
	}
	// Error: a bad baseTree fails read-tree.
	if _, err := g.SpliceBlobs(ctx,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		[]ResolvedBlob{{Path: "x", Content: "y"}}); err == nil {
		t.Fatal("expected error for bad baseTree")
	}
}

// --- parseDiffRawZ: malformed inputs (unit) ---------------------------------

func TestParseDiffRawZErrors(t *testing.T) {
	// Dangling record: metadata with no following NUL-separated path.
	if _, err := parseDiffRawZ(":100644 100644 aaaa bbbb M"); err == nil {
		t.Fatal("expected dangling-record error")
	}
	// Malformed: fewer than 4 metadata fields.
	if _, err := parseDiffRawZ(":100644 100644\x00some/path\x00"); err == nil {
		t.Fatal("expected malformed-record error")
	}
	// Valid input parses cleanly (trailing empty field tolerated).
	ents, err := parseDiffRawZ(":100644 100755 aaaa bbbb M\x00file.txt\x00")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Mode != "100755" || ents[0].Path != "file.txt" {
		t.Fatalf("parsed = %+v", ents)
	}
}

// --- OverlayTrees: error paths + empty/no-op branches -----------------------

func TestOverlayTreesErrorsAndEmpty(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	good := branchFrom(t, g, base, "g", func(d string) { write(t, d, "x.txt", "x\n") })

	// Bad base => read-tree fails.
	if _, err := g.OverlayTrees(ctx,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", []string{good}); err == nil {
		t.Fatal("expected error for bad base")
	}
	// Bad tree in the list => ensureTreeish surfaces the error.
	if _, err := g.OverlayTrees(ctx, base,
		[]string{"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}); err == nil {
		t.Fatal("expected error for bad tree in list")
	}
	// Empty tree list => just the base tree (no update-index step).
	tree, err := g.OverlayTrees(ctx, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseTree, err := g.TreeOID(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if tree != baseTree {
		t.Fatalf("overlay of nothing = %s, want base tree %s", tree, baseTree)
	}
}

// --- UpdateRefs: no-op, whitespace guard, and failing update ----------------

func TestUpdateRefsErrors(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)

	// Empty map => no-op, no error.
	if err := g.UpdateRefs(ctx, nil); err != nil {
		t.Fatalf("empty UpdateRefs should be a no-op: %v", err)
	}
	// Whitespace in a ref name is rejected before touching git.
	if err := g.UpdateRef(ctx, "refs/heads/bad ref", base); err == nil {
		t.Fatal("expected whitespace-in-ref error")
	}
	// Whitespace in an oid is rejected too.
	if err := g.UpdateRefs(ctx, map[string]string{"refs/heads/x": "bad oid"}); err == nil {
		t.Fatal("expected whitespace-in-oid error")
	}
	// Well-formed but non-existent oid => update-ref exits non-zero.
	if err := g.UpdateRef(ctx, "refs/heads/x",
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err == nil {
		t.Fatal("expected update-ref failure for missing object")
	}
}

// --- BatchReader: closed reader, rev:path, ResolveCommit error, double Close -

func TestBatchReaderClosedAndResolveCommitError(t *testing.T) {
	ctx := context.Background()
	g, _ := newRepo(t)
	br, err := g.NewBatchReader(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// ResolveCommit on a non-existent ref => error.
	if _, err := br.ResolveCommit("no-such-ref"); err == nil {
		t.Fatal("ResolveCommit should fail on missing ref")
	}
	// A rev:path spec resolves to a blob.
	_, typ, _, ok, err := br.Resolve("HEAD:base.txt")
	if err != nil || !ok {
		t.Fatalf("resolve HEAD:base.txt: ok=%v err=%v", ok, err)
	}
	if typ != "blob" {
		t.Fatalf("HEAD:base.txt type = %q, want blob", typ)
	}

	// Close, then double-Close is a no-op nil.
	if err := br.Close(); err != nil {
		t.Fatal(err)
	}
	if err := br.Close(); err != nil {
		t.Fatalf("double Close should be nil: %v", err)
	}
	// Resolve after Close => error.
	if _, _, _, _, err := br.Resolve("HEAD"); err == nil {
		t.Fatal("Resolve after Close should error")
	}
}

// --- CheckoutB + DiffNameOnly sort sanity kept trivially green --------------

func TestDiffNameOnlySorted(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	b := branchFrom(t, g, base, "multi", func(d string) {
		write(t, d, "z.txt", "z\n")
		write(t, d, "a.txt", "a\n")
	})
	paths, err := g.DiffNameOnly(ctx, base, b)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	if len(paths) != 2 || paths[0] != "a.txt" || paths[1] != "z.txt" {
		t.Fatalf("write-set = %v, want [a.txt z.txt]", paths)
	}
}
