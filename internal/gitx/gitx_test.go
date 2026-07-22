package gitx

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo creates an initialized repo with one base commit and returns the Git
// handle plus the base SHA.
func newRepo(t *testing.T) (*Git, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "base.txt", "base\n")
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	if base == "" {
		t.Fatal("empty base sha")
	}
	return g, base
}

func TestInitCommitHead(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	head, err := g.HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head != base {
		t.Fatalf("head %s != base %s", head, base)
	}
}

// TestGitCommonDir proves the property -verify-cache storage relies on: a
// linked worktree's GitCommonDir resolves to the SAME shared .git as the
// main repo it was created from, not the worktree's own per-worktree
// admin dir — so cache entries written from either location land in one
// shared place.
func TestGitCommonDir(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	// EvalSymlinks: on macOS t.TempDir() lives under a symlinked /var, and
	// --path-format=absolute resolves it — compare against the same
	// resolved form rather than asserting byte-identical strings, which
	// would be a host quirk, not a real behavior difference.
	want, err := filepath.EvalSymlinks(filepath.Join(g.Dir(), ".git"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.GitCommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("GitCommonDir=%s, want %s", got, want)
	}

	wtDir := filepath.Join(t.TempDir(), "wt")
	if err := g.WorktreeAdd(ctx, wtDir, "feature", base); err != nil {
		t.Fatal(err)
	}
	gotWT, err := g.At(wtDir).GitCommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gotWT != want {
		t.Fatalf("worktree GitCommonDir=%s, want %s (same as main repo)", gotWT, want)
	}
}

func TestWorktreeAddDiffRemove(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)

	wtDir := filepath.Join(t.TempDir(), "wt")
	if err := g.WorktreeAdd(ctx, wtDir, "agent1", base); err != nil {
		t.Fatal(err)
	}
	wg := g.At(wtDir)

	writeFile(t, wtDir, "new.txt", "hello\n")
	writeFile(t, wtDir, "base.txt", "base\nmore\n")
	if _, err := wg.CommitAll(ctx, "agent1 edit"); err != nil {
		t.Fatal(err)
	}

	paths, err := g.DiffNameOnly(ctx, base, "agent1")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	want := []string{"base.txt", "new.txt"}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("write-set = %v, want %v", paths, want)
	}

	if err := g.WorktreeRemove(ctx, wtDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still present: %v", err)
	}
	// Branch survives removal.
	if _, err := g.RevParse(ctx, "agent1"); err != nil {
		t.Fatalf("branch gone after worktree remove: %v", err)
	}
}

func TestMergeClean(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)

	// Two branches touching different files -> clean merge.
	mk := func(branch, file string) {
		wt := filepath.Join(t.TempDir(), branch)
		if err := g.WorktreeAdd(ctx, wt, branch, base); err != nil {
			t.Fatal(err)
		}
		writeFile(t, wt, file, "content of "+file+"\n")
		if _, err := g.At(wt).CommitAll(ctx, branch); err != nil {
			t.Fatal(err)
		}
		if err := g.WorktreeRemove(ctx, wt); err != nil {
			t.Fatal(err)
		}
	}
	mk("b1", "one.txt")
	mk("b2", "two.txt")

	intDir := filepath.Join(t.TempDir(), "int")
	if err := g.WorktreeAdd(ctx, intDir, "integration", base); err != nil {
		t.Fatal(err)
	}
	ig := g.At(intDir)

	res, err := ig.Merge(ctx, "merge b1", "b1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("b1 merge not OK: %+v", res)
	}
	res, err = ig.Merge(ctx, "merge b2", "b2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("b2 merge not OK: %+v", res)
	}

	// Both files present in the merged tree.
	files, err := ig.LsTree(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	for _, want := range []string{"one.txt", "two.txt", "base.txt"} {
		if !got[want] {
			t.Fatalf("merged tree missing %s (have %v)", want, files)
		}
	}
}

func TestMergeAutoResolvesDistinctLines(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	// A file with many lines; two branches edit far-apart lines -> git 3-way
	// auto-merges (tier (a), no human).
	lines := ""
	for i := 0; i < 20; i++ {
		lines += "line\n"
	}
	writeFile(t, dir, "shared.txt", lines)
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}

	edit := func(branch string, lineIdx int) {
		wt := filepath.Join(t.TempDir(), branch)
		if err := g.WorktreeAdd(ctx, wt, branch, base); err != nil {
			t.Fatal(err)
		}
		data := ""
		for i := 0; i < 20; i++ {
			if i == lineIdx {
				data += branch + "-edit\n"
			} else {
				data += "line\n"
			}
		}
		writeFile(t, wt, "shared.txt", data)
		if _, err := g.At(wt).CommitAll(ctx, branch); err != nil {
			t.Fatal(err)
		}
		if err := g.WorktreeRemove(ctx, wt); err != nil {
			t.Fatal(err)
		}
	}
	edit("x", 1)
	edit("y", 18)

	intDir := filepath.Join(t.TempDir(), "int")
	if err := g.WorktreeAdd(ctx, intDir, "integration", base); err != nil {
		t.Fatal(err)
	}
	ig := g.At(intDir)
	if r, err := ig.Merge(ctx, "x", "x"); err != nil || !r.OK {
		t.Fatalf("merge x: %+v %v", r, err)
	}
	r, err := ig.Merge(ctx, "y", "y")
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Fatalf("expected auto-merge of distinct lines, got conflicts %v", r.Conflicts)
	}
	content, err := ig.ShowFile(ctx, "HEAD", "shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(content, "x-edit") || !contains(content, "y-edit") {
		t.Fatalf("merged file missing an edit:\n%s", content)
	}
}

func TestMergeConflictFlagged(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "c.txt", "original\n")
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}

	edit := func(branch, content string) {
		wt := filepath.Join(t.TempDir(), branch)
		if err := g.WorktreeAdd(ctx, wt, branch, base); err != nil {
			t.Fatal(err)
		}
		writeFile(t, wt, "c.txt", content)
		if _, err := g.At(wt).CommitAll(ctx, branch); err != nil {
			t.Fatal(err)
		}
		if err := g.WorktreeRemove(ctx, wt); err != nil {
			t.Fatal(err)
		}
	}
	edit("p", "changed by p\n")
	edit("q", "changed by q\n")

	intDir := filepath.Join(t.TempDir(), "int")
	if err := g.WorktreeAdd(ctx, intDir, "integration", base); err != nil {
		t.Fatal(err)
	}
	ig := g.At(intDir)
	if r, err := ig.Merge(ctx, "p", "p"); err != nil || !r.OK {
		t.Fatalf("merge p: %+v %v", r, err)
	}
	r, err := ig.Merge(ctx, "q", "q")
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("expected conflict, got clean merge")
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0] != "c.txt" {
		t.Fatalf("conflicts = %v, want [c.txt]", r.Conflicts)
	}
	// After a flagged conflict the worktree must be clean (merge aborted), so a
	// subsequent clean merge still works.
	head, err := ig.HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pSHA, _ := ig.RevParse(ctx, "p")
	if head != pSHA {
		t.Fatalf("after abort HEAD=%s, want p=%s (clean state)", head, pSHA)
	}
}

func TestOctopusMergeDisjoint(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)
	var branches []string
	for _, name := range []string{"o1", "o2", "o3", "o4"} {
		wt := filepath.Join(t.TempDir(), name)
		if err := g.WorktreeAdd(ctx, wt, name, base); err != nil {
			t.Fatal(err)
		}
		writeFile(t, wt, name+".txt", name+"\n")
		if _, err := g.At(wt).CommitAll(ctx, name); err != nil {
			t.Fatal(err)
		}
		if err := g.WorktreeRemove(ctx, wt); err != nil {
			t.Fatal(err)
		}
		branches = append(branches, name)
	}
	intDir := filepath.Join(t.TempDir(), "int")
	if err := g.WorktreeAdd(ctx, intDir, "integration", base); err != nil {
		t.Fatal(err)
	}
	ig := g.At(intDir)
	// Single octopus merge of all disjoint branches.
	r, err := ig.Merge(ctx, "octopus", branches...)
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Fatalf("octopus not OK: %+v", r)
	}
	files, err := ig.LsTree(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	for _, want := range []string{"o1.txt", "o2.txt", "o3.txt", "o4.txt"} {
		if !got[want] {
			t.Fatalf("octopus tree missing %s", want)
		}
	}
}

// TestNoteAdd proves NoteAdd attaches content under the NAMESPACED ref (never
// git's default refs/notes/commits), that it's readable back via the ordinary
// `git notes --ref=... show` porcelain, and that a second call on the same
// commit OVERWRITES rather than erroring or appending (the `-f` flag).
func TestNoteAdd(t *testing.T) {
	ctx := context.Background()
	g, base := newRepo(t)

	if err := g.NoteAdd(ctx, "sigbound", base, []byte(`{"v":1}`)); err != nil {
		t.Fatalf("NoteAdd: %v", err)
	}
	got, err := g.run(ctx, "notes", "--ref=sigbound", "show", base)
	if err != nil {
		t.Fatalf("notes show: %v", err)
	}
	if got != `{"v":1}` {
		t.Fatalf("note body=%q, want {\"v\":1}", got)
	}

	// A note under git's DEFAULT namespace must be untouched — NoteAdd is
	// namespaced, never refs/notes/commits.
	if _, _, code, _ := g.runRaw(ctx, "notes", "show", base); code == 0 {
		t.Fatal("a note exists under the default refs/notes/commits namespace; NoteAdd must never touch it")
	}

	// Overwrite: a second NoteAdd on the SAME commit replaces the note body
	// instead of erroring (git notes add without -f refuses when one exists).
	if err := g.NoteAdd(ctx, "sigbound", base, []byte(`{"v":2}`)); err != nil {
		t.Fatalf("NoteAdd (overwrite): %v", err)
	}
	got, err = g.run(ctx, "notes", "--ref=sigbound", "show", base)
	if err != nil {
		t.Fatalf("notes show after overwrite: %v", err)
	}
	if got != `{"v":2}` {
		t.Fatalf("note body after overwrite=%q, want {\"v\":2}", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
