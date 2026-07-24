package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// TestDoctorRealGit is the e2e test: run the actual doctor probes against
// whatever git is on PATH. CI runs a modern git, so every check must pass
// and the process must report exit 0.
func TestDoctorRealGit(t *testing.T) {
	var buf bytes.Buffer
	code, err := runDoctor(&buf, nil)
	if err != nil {
		t.Fatalf("runDoctor: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("runDoctor code=%d, want exitOK\noutput:\n%s", code, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"git on PATH: ok", "git version >= 2.38: ok", "live probe: merge-tree + overlay plumbing: ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "FAIL") {
		t.Fatalf("no check should FAIL against a modern git; got:\n%s", out)
	}
}

// TestDoctorRepoFlagRunsInPlaceWithoutMutatingIt: -repo points the live probe
// at a real, pre-existing repo. The probe must pass AND leave that repo's
// visible state (HEAD, branches, working tree) completely untouched — it
// only builds unreferenced objects via plumbing, never a worktree/branch/ref.
func TestDoctorRepoFlagRunsInPlaceWithoutMutatingIt(t *testing.T) {
	ctx := context.Background()
	g, base := newDoctorRepo(t)

	filesBefore, err := g.LsTree(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	code, err := runDoctor(&buf, []string{"-repo", g.Dir()})
	if err != nil {
		t.Fatalf("runDoctor -repo: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("runDoctor -repo code=%d, want exitOK\noutput:\n%s", code, buf.String())
	}

	headAfter, err := g.HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if headAfter != base {
		t.Fatalf("HEAD moved from %s to %s: -repo probe must not mutate the repo", base, headAfter)
	}
	dirty, err := g.HasUncommittedChanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("-repo probe left uncommitted changes in the working tree")
	}
	filesAfter, err := g.LsTree(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(filesAfter) != len(filesBefore) {
		t.Fatalf("HEAD tree changed: before=%v after=%v", filesBefore, filesAfter)
	}
}

// TestDoctorBadRepoFailsSafe: a -repo that isn't a git repository at all
// makes the live probe FAIL (not panic), and runDoctor reports exit 1.
func TestDoctorBadRepoFailsSafe(t *testing.T) {
	var buf bytes.Buffer
	code, err := runDoctor(&buf, []string{"-repo", t.TempDir()})
	if err != nil {
		t.Fatalf("runDoctor: %v\n%s", err, buf.String())
	}
	if code != exitOperationalError {
		t.Fatalf("runDoctor code=%d, want exitOperationalError (1) for a non-repo -repo", code)
	}
	if !strings.Contains(buf.String(), "FAIL") {
		t.Fatalf("expected a FAIL line for a non-repo -repo, got:\n%s", buf.String())
	}
}

// TestDoctorDiskLineRenders: the informational disk-space line always
// appears — on the default (no -repo, falls back to the current directory)
// AND the -repo path — and never contributes a "FAIL": it's advisory, not
// one of doctor's pass/fail checks (see diskInfoLine's doc comment).
func TestDoctorDiskLineRenders(t *testing.T) {
	var buf bytes.Buffer
	code, err := runDoctor(&buf, nil)
	if err != nil {
		t.Fatalf("runDoctor: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "disk:") {
		t.Fatalf("output missing the disk line:\n%s", buf.String())
	}

	g, _ := newDoctorRepo(t)
	var buf2 bytes.Buffer
	code2, err := runDoctor(&buf2, []string{"-repo", g.Dir()})
	if err != nil {
		t.Fatalf("runDoctor -repo: %v\n%s", err, buf2.String())
	}
	if code2 != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code2, buf2.String())
	}
	if !strings.Contains(buf2.String(), "disk: repo tree") {
		t.Fatalf("output missing the disk tree-size line for a real -repo:\n%s", buf2.String())
	}
}

// newDoctorRepo creates a minimal real repo (one commit) for -repo tests.
func newDoctorRepo(t *testing.T) (*gitx.Git, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	g := gitx.New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	base, err := g.CommitTree(ctx, emptySHA1Tree, nil, "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateRef(ctx, "refs/heads/main", base); err != nil {
		t.Fatal(err)
	}
	head, err := g.HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head != base {
		t.Fatalf("HEAD=%s, want %s", head, base)
	}
	return g, base
}
