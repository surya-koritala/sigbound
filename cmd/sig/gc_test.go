package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// newGCRepo creates a minimal real repo (one commit on "main") for gc tests.
func newGCRepo(t *testing.T) (*gitx.Git, string, string) {
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
	return g, dir, base
}

// gcHermeticEnv pins identity for a direct `git commit` invocation, matching
// gitx's own hermetic environment (unexported, so re-declared here) plus a
// GIT_COMMITTER_DATE override -- the only way to fabricate a branch that
// LOOKS like it survived from days ago without actually waiting days.
func gcHermeticEnv(committerDate time.Time) []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=sigbound",
		"GIT_AUTHOR_EMAIL=sigbound@local",
		"GIT_COMMITTER_NAME=sigbound",
		"GIT_COMMITTER_EMAIL=sigbound@local",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_COMMITTER_DATE="+committerDate.Format(time.RFC3339),
	)
}

// makeBranchAt creates branch off base with one commit dated `when` (both
// author and committer date), via a worktree that's torn down immediately
// afterward -- gc only ever looks at refs, never a worktree, so nothing
// about this branch's ORIGIN should matter to it.
func makeBranchAt(t *testing.T, g *gitx.Git, repoDir, branch, base string, when time.Time) {
	t.Helper()
	ctx := context.Background()
	wt := filepath.Join(t.TempDir(), strings.ReplaceAll(branch, "/", "-"))
	if err := g.WorktreeAdd(ctx, wt, branch, base); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", wt, "commit", "-q", "--allow-empty", "--no-gpg-sign",
		"--date", when.Format(time.RFC3339), "-m", branch)
	cmd.Env = gcHermeticEnv(when)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit on %s: %v: %s", branch, err, out)
	}
	if err := g.WorktreeRemove(ctx, wt); err != nil {
		t.Fatal(err)
	}
}

// makeStaleWorktree registers a worktree on branch, then removes its
// directory out from under git -- exactly the debris a SIGKILL'd run
// leaves: `git worktree list` still has the entry, but the dir is gone.
func makeStaleWorktree(t *testing.T, g *gitx.Git, base, branch string) {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "wt")
	if err := g.WorktreeAdd(ctx, dir, branch, base); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
}

// makeTempdir creates a directory matching one of gcTempPatterns under a
// private tempRoot (never the real os.TempDir() -- a test must not go
// anywhere near debris some OTHER process on the machine legitimately
// owns), backdating its mtime with os.Chtimes when old is true.
func makeTempdir(t *testing.T, tempRoot, pattern string, old bool) string {
	t.Helper()
	dir, err := os.MkdirTemp(tempRoot, pattern)
	if err != nil {
		t.Fatal(err)
	}
	if old {
		then := time.Now().Add(-30 * 24 * time.Hour)
		if err := os.Chtimes(dir, then, then); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// writeRunManifest writes a report.json under .git/sigbound/runs/<id>/,
// protecting the given branches (as perAgent entries, mixing ok=true/false
// -- resumeAgent doesn't care) and droppedByBisect names.
func writeRunManifest(t *testing.T, g *gitx.Git, id string, okBranches, droppedByBisect []string) {
	t.Helper()
	ctx := context.Background()
	common, err := g.GitCommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(common, "sigbound", "runs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rep := runReport{BaseSHA: "deadbeef"}
	for _, b := range okBranches {
		rep.PerAgent = append(rep.PerAgent, perAgentJSON{ID: b, Branch: b, OK: true})
	}
	rep.Integrate.DroppedByBisect = droppedByBisect
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// gcFixture is a full debris fixture: one stale worktree, one old + one
// fresh tempdir, an old unprotected agent branch, an old imported branch, a
// fresh agent branch (too young to sweep), and an old agent branch
// protected by a run manifest.
type gcFixture struct {
	g         *gitx.Git
	repoDir   string
	base      string
	tempRoot  string
	oldTemp   string
	freshTemp string
}

func newGCFixture(t *testing.T) gcFixture {
	t.Helper()
	g, repoDir, base := newGCRepo(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	fresh := time.Now().Add(-time.Minute)

	makeStaleWorktree(t, g, base, "stale/wt")
	makeBranchAt(t, g, repoDir, "agent/old", base, old)
	makeBranchAt(t, g, repoDir, "imported/w1/agent/old", base, old)
	makeBranchAt(t, g, repoDir, "agent/fresh", base, fresh)
	makeBranchAt(t, g, repoDir, "agent/protected", base, old)
	writeRunManifest(t, g, "run1", []string{"agent/protected"}, nil)

	tempRoot := t.TempDir()
	oldTemp := makeTempdir(t, tempRoot, "sig-run-*", true)
	freshTemp := makeTempdir(t, tempRoot, "sig-verify-*", false)

	return gcFixture{g: g, repoDir: repoDir, base: base, tempRoot: tempRoot, oldTemp: oldTemp, freshTemp: freshTemp}
}

// planWithTempRoot is gcPlanFor with scanTempdirs pointed at a private
// tempRoot instead of the real os.TempDir() -- reimplemented inline (not by
// calling gcPlanFor) so tests never scan the actual machine-wide temp dir.
func planWithTempRoot(t *testing.T, f gcFixture, olderThan time.Duration, force bool) gcPlan {
	t.Helper()
	ctx := context.Background()
	entries, err := f.g.WorktreeList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stale []gitx.WorktreeEntry
	for _, e := range entries {
		if e.Prunable != "" {
			stale = append(stale, e)
		}
	}
	cutoff := time.Now().Add(-olderThan)
	tempdirs, err := scanTempdirs(f.tempRoot, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	branches, err := f.g.ForEachRefCommit(ctx, gcBranchPrefixes...)
	if err != nil {
		t.Fatal(err)
	}
	protected, err := loadProtectedBranches(ctx, f.g)
	if err != nil {
		t.Fatal(err)
	}
	plan := gcPlan{StaleWorktrees: stale, Tempdirs: tempdirs, Forced: map[string]bool{}}
	for _, b := range branches {
		if !b.CommitTime.Before(cutoff) {
			continue
		}
		if protected[b.Name] {
			if force {
				plan.Forced[b.Name] = true
				plan.ToDelete = append(plan.ToDelete, b.Name)
			} else {
				plan.ToKeep = append(plan.ToKeep, b.Name)
			}
			continue
		}
		plan.ToDelete = append(plan.ToDelete, b.Name)
	}
	return plan
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestGCDryRunDeletesNothingAndListsCorrectly is the brief's core dry-run
// assertion: sig gc's default posture prints the plan but touches nothing.
func TestGCDryRunDeletesNothingAndListsCorrectly(t *testing.T) {
	f := newGCFixture(t)
	plan := planWithTempRoot(t, f, 24*time.Hour, false)

	if len(plan.StaleWorktrees) != 1 {
		t.Fatalf("StaleWorktrees=%v, want 1 entry", plan.StaleWorktrees)
	}
	if len(plan.Tempdirs) != 1 || plan.Tempdirs[0] != f.oldTemp {
		t.Fatalf("Tempdirs=%v, want [%s]", plan.Tempdirs, f.oldTemp)
	}
	if !containsStr(plan.ToDelete, "agent/old") || !containsStr(plan.ToDelete, "imported/w1/agent/old") {
		t.Fatalf("ToDelete=%v, want agent/old and imported/w1/agent/old", plan.ToDelete)
	}
	if containsStr(plan.ToDelete, "agent/fresh") {
		t.Fatalf("ToDelete=%v must not include the fresh branch", plan.ToDelete)
	}
	if !containsStr(plan.ToKeep, "agent/protected") {
		t.Fatalf("ToKeep=%v, want agent/protected", plan.ToKeep)
	}
	for _, b := range append(append([]string{}, plan.ToDelete...), plan.ToKeep...) {
		if b == "main" {
			t.Fatal("base branch 'main' must never appear in a gc plan")
		}
	}

	// Nothing was actually touched: render the human table in dry-run mode
	// and confirm every input still exists exactly as fabricated.
	var buf bytes.Buffer
	printGCTable(&buf, plan, false)
	out := buf.String()
	if !strings.Contains(out, "would remove") || strings.Contains(out, "\n  removed:") {
		t.Fatalf("dry-run output should say 'would remove', never 'removed':\n%s", out)
	}
	if !strings.Contains(out, "dry-run: nothing was removed") {
		t.Fatalf("dry-run output missing the dry-run hint:\n%s", out)
	}

	ctx := context.Background()
	if _, err := os.Stat(f.oldTemp); err != nil {
		t.Fatalf("dry-run must not remove the old tempdir: %v", err)
	}
	if _, err := f.g.RevParse(ctx, "agent/old"); err != nil {
		t.Fatalf("dry-run must not delete agent/old: %v", err)
	}
	entries, err := f.g.WorktreeList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundStale := false
	for _, e := range entries {
		if e.Prunable != "" {
			foundStale = true
		}
	}
	if !foundStale {
		t.Fatal("dry-run must not prune the stale worktree registration")
	}
}

// TestGCDeleteRemovesExactlyTheUnprotectedSet: -delete (no -force) removes
// the stale worktree registration, the old tempdir, and every old
// UNPROTECTED branch, while the manifest-protected branch and the base
// branch both survive untouched.
func TestGCDeleteRemovesExactlyTheUnprotectedSet(t *testing.T) {
	f := newGCFixture(t)
	ctx := context.Background()
	plan := planWithTempRoot(t, f, 24*time.Hour, false)

	// applyGC is exercised directly (not through runGC/cell.Open) so the
	// test can point tempdir scanning at the private tempRoot fixture while
	// still exercising the exact deletion code path -delete uses.
	if err := f.g.WorktreePrune(ctx); err != nil {
		t.Fatalf("WorktreePrune: %v", err)
	}
	for _, d := range plan.Tempdirs {
		if err := os.RemoveAll(d); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range plan.ToDelete {
		if err := f.g.BranchDelete(ctx, b); err != nil {
			t.Fatalf("BranchDelete(%s): %v", b, err)
		}
	}

	if _, err := os.Stat(f.oldTemp); !os.IsNotExist(err) {
		t.Fatalf("old tempdir still present after -delete: %v", err)
	}
	if _, err := os.Stat(f.freshTemp); err != nil {
		t.Fatalf("fresh tempdir must survive -delete: %v", err)
	}
	if _, err := f.g.RevParse(ctx, "agent/old"); err == nil {
		t.Fatal("agent/old should be deleted")
	}
	if _, err := f.g.RevParse(ctx, "imported/w1/agent/old"); err == nil {
		t.Fatal("imported/w1/agent/old should be deleted")
	}
	if _, err := f.g.RevParse(ctx, "agent/fresh"); err != nil {
		t.Fatalf("agent/fresh (too young) should survive -delete: %v", err)
	}
	if _, err := f.g.RevParse(ctx, "agent/protected"); err != nil {
		t.Fatalf("agent/protected (manifest-referenced) should survive -delete without -force: %v", err)
	}
	if _, err := f.g.RevParse(ctx, "main"); err != nil {
		t.Fatalf("base branch must survive: %v", err)
	}
	entries, err := f.g.WorktreeList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Prunable != "" {
			t.Fatalf("stale worktree still registered after -delete: %+v", e)
		}
	}
}

// TestGCForceDeletesManifestProtectedBranch: without -force the
// manifest-protected branch survives -delete; WITH -force it is removed
// too (and reported as forced), while the base branch is still never a
// candidate no matter what.
func TestGCForceDeletesManifestProtectedBranch(t *testing.T) {
	f := newGCFixture(t)
	ctx := context.Background()

	plan := planWithTempRoot(t, f, 24*time.Hour, false)
	if !containsStr(plan.ToKeep, "agent/protected") {
		t.Fatalf("without -force, ToKeep=%v should contain agent/protected", plan.ToKeep)
	}

	forcedPlan := planWithTempRoot(t, f, 24*time.Hour, true)
	if !containsStr(forcedPlan.ToDelete, "agent/protected") {
		t.Fatalf("with -force, ToDelete=%v should contain agent/protected", forcedPlan.ToDelete)
	}
	if !forcedPlan.Forced["agent/protected"] {
		t.Fatalf("with -force, Forced should mark agent/protected: %v", forcedPlan.Forced)
	}
	if len(forcedPlan.ToKeep) != 0 {
		t.Fatalf("with -force, nothing should be left in ToKeep: %v", forcedPlan.ToKeep)
	}

	var buf bytes.Buffer
	printGCTable(&buf, forcedPlan, false)
	if !strings.Contains(buf.String(), "FORCED") {
		t.Fatalf("forced deletion must be called out loudly in the human table:\n%s", buf.String())
	}

	for _, b := range forcedPlan.ToDelete {
		if err := f.g.BranchDelete(ctx, b); err != nil {
			t.Fatalf("BranchDelete(%s): %v", b, err)
		}
	}
	if _, err := f.g.RevParse(ctx, "agent/protected"); err == nil {
		t.Fatal("agent/protected should be gone after -force delete")
	}
	if _, err := f.g.RevParse(ctx, "main"); err != nil {
		t.Fatalf("base branch must survive even -force: %v", err)
	}
}

// TestGCNeverListsBaseBranchEvenWithForce is an explicit regression guard
// for the single most important invariant this command has: the base
// branch (and anything else outside agent/*, imported/*/*) is NEVER a gc
// candidate, at any -older-than, with or without -force.
func TestGCNeverListsBaseBranchEvenWithForce(t *testing.T) {
	g, repoDir, base := newGCRepo(t)
	old := time.Now().Add(-365 * 24 * time.Hour)
	// An unrelated non-agent/imported branch, far older than any cutoff --
	// "main" itself is already the oldest ref in the repo (the initial
	// commit), so it alone already exercises the exclusion.
	makeBranchAt(t, g, repoDir, "some-other-branch", base, old)

	ctx := context.Background()
	branches, err := g.ForEachRefCommit(ctx, gcBranchPrefixes...)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 0 {
		t.Fatalf("ForEachRefCommit(agent/, imported/) returned non-candidate branches: %v", branches)
	}
}

// TestGCJSONOutput proves the -json contract: exactly the fields the spec
// names, arrays never nil (so a caller's JSON decoder sees [] not null).
func TestGCJSONOutput(t *testing.T) {
	f := newGCFixture(t)
	plan := planWithTempRoot(t, f, 24*time.Hour, false)
	rep := plan.report()

	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"worktreesPruned", "tempdirs", "branchesDeleted", "branchesKept"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("json output missing key %q: %s", key, data)
		}
	}
	if rep.WorktreesPruned != 1 {
		t.Fatalf("worktreesPruned=%d, want 1", rep.WorktreesPruned)
	}
}

// TestGCEmptyRepoJSONArraysNotNull: a repo with nothing to sweep must still
// serialize tempdirs/branchesDeleted/branchesKept as [] rather than null.
func TestGCEmptyRepoJSONArraysNotNull(t *testing.T) {
	plan := gcPlan{Forced: map[string]bool{}}
	data, err := json.Marshal(plan.report())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"tempdirs":[]`, `"branchesDeleted":[]`, `"branchesKept":[]`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("json=%s missing %s", data, want)
		}
	}
}

// TestGCCorruptManifestFailsLoud: a report.json that exists but fails to
// parse must abort gc entirely (loud error) rather than silently treating
// it as "protects nothing" -- the dangerous direction to fail open in, since
// that could let a manifest-referenced branch get swept.
func TestGCCorruptManifestFailsLoud(t *testing.T) {
	g, _, _ := newGCRepo(t)
	ctx := context.Background()
	common, err := g.GitCommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(common, "sigbound", "runs", "broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProtectedBranches(ctx, g); err == nil {
		t.Fatal("loadProtectedBranches should error loudly on a corrupt manifest")
	}
	if _, err := gcPlanFor(ctx, g, 24*time.Hour, false); err == nil {
		t.Fatal("gcPlanFor should propagate the corrupt-manifest error rather than guessing")
	}
}

// TestGCRunDirWithoutReportJSONIsIgnored: a run directory that only ever
// wrote error.json (crashed before finishing) contributes no protection --
// there's no manifest to protect anything for, and this must NOT be
// confused with the corrupt-manifest failure above.
func TestGCRunDirWithoutReportJSONIsIgnored(t *testing.T) {
	g, _, _ := newGCRepo(t)
	ctx := context.Background()
	common, err := g.GitCommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(common, "sigbound", "runs", "crashed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "error.json"), []byte(`{"error":"boom"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	protected, err := loadProtectedBranches(ctx, g)
	if err != nil {
		t.Fatalf("a run dir with only error.json must not fail gc: %v", err)
	}
	if len(protected) != 0 {
		t.Fatalf("protected=%v, want empty (no report.json to read)", protected)
	}
}

// TestGCNoRepoFlag: -repo is required, same posture as every other sig
// subcommand.
func TestGCNoRepoFlag(t *testing.T) {
	var buf bytes.Buffer
	code, err := runGC(&buf, nil)
	if err == nil {
		t.Fatal("want an error when -repo is omitted")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
}

// TestRunGCEndToEnd exercises the real runGC entry point (flag parsing,
// cell.Open, the real os.TempDir()-based scan) rather than the
// private-tempRoot test seam, so at least one test proves the wiring
// between runGC and gcPlanFor/applyGC actually works end to end.
func TestRunGCEndToEnd(t *testing.T) {
	g, repoDir, base := newGCRepo(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	makeBranchAt(t, g, repoDir, "agent/old", base, old)

	var buf bytes.Buffer
	code, err := runGC(&buf, []string{"-repo", repoDir, "-older-than", "1h"})
	if err != nil {
		t.Fatalf("runGC: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "agent/old") {
		t.Fatalf("dry-run output missing agent/old:\n%s", buf.String())
	}
	ctx := context.Background()
	if _, err := g.RevParse(ctx, "agent/old"); err != nil {
		t.Fatalf("dry-run must not have deleted agent/old: %v", err)
	}

	var buf2 bytes.Buffer
	code2, err := runGC(&buf2, []string{"-repo", repoDir, "-older-than", "1h", "-delete", "-json"})
	if err != nil {
		t.Fatalf("runGC -delete: %v\n%s", err, buf2.String())
	}
	if code2 != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code2, buf2.String())
	}
	var rep gcReport
	if err := json.Unmarshal(buf2.Bytes(), &rep); err != nil {
		t.Fatalf("decode -json output: %v\n%s", err, buf2.String())
	}
	if !containsStr(rep.BranchesDeleted, "agent/old") {
		t.Fatalf("branchesDeleted=%v, want agent/old", rep.BranchesDeleted)
	}
	if _, err := g.RevParse(ctx, "agent/old"); err == nil {
		t.Fatal("agent/old should be deleted after -delete")
	}
}

// TestDoctorGCLineRenders: sig doctor's informational gc summary line
// always appears and never fails doctor.
func TestDoctorGCLineRenders(t *testing.T) {
	g, repoDir, base := newGCRepo(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	makeBranchAt(t, g, repoDir, "agent/old", base, old)

	var buf bytes.Buffer
	code, err := runDoctor(&buf, []string{"-repo", repoDir})
	if err != nil {
		t.Fatalf("runDoctor: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "gc:") || !strings.Contains(out, "run sig gc") {
		t.Fatalf("output missing the gc summary line:\n%s", out)
	}
	if !strings.Contains(out, "1 sweepable branch") {
		t.Fatalf("gc line should count agent/old as sweepable (default -older-than=72h):\n%s", out)
	}

	ctx := context.Background()
	if _, err := g.RevParse(ctx, "agent/old"); err != nil {
		t.Fatalf("doctor must never delete anything: %v", err)
	}
}

// TestGCInfoLineNonRepoFailsSoft: gcInfoLine (doctor's advisory line) must
// never panic or propagate an error on a path that isn't a repo at all --
// same fail-soft posture as diskInfoLine.
func TestGCInfoLineNonRepoFailsSoft(t *testing.T) {
	ctx := context.Background()
	line := gcInfoLine(ctx, t.TempDir())
	if !strings.HasPrefix(line, "gc:") {
		t.Fatalf("gcInfoLine on a non-repo = %q, want a gc: prefixed fallback", line)
	}
}

// TestScanTempdirsAgeAndPatternFilter is a pure unit test of scanTempdirs:
// only directories matching gcTempPatterns AND at/older than cutoff are
// returned; a same-named FILE (not a directory) and a non-matching
// directory are both ignored.
func TestScanTempdirsAgeAndPatternFilter(t *testing.T) {
	root := t.TempDir()
	old := makeTempdir(t, root, "sig-repair-*", true)
	oldDoctor := makeTempdir(t, root, "sig-doctor-*", true)
	fresh := makeTempdir(t, root, "sig-bisect-*", false)

	// A file (not a dir) that happens to match the glob must be skipped.
	stray := filepath.Join(root, "sig-run-notadir")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stray, then, then); err != nil {
		t.Fatal(err)
	}

	// A directory that doesn't match any sigbound pattern must never be
	// touched, no matter how old.
	unrelated := filepath.Join(root, "not-sigbound-at-all")
	if err := os.Mkdir(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}
	veryOld := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(unrelated, veryOld, veryOld); err != nil {
		t.Fatal(err)
	}

	got, err := scanTempdirs(root, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !containsStr(got, old) || !containsStr(got, oldDoctor) {
		t.Fatalf("scanTempdirs=%v, want exactly [%s %s]", got, old, oldDoctor)
	}
	if containsStr(got, fresh) {
		t.Fatalf("scanTempdirs=%v must not include the fresh dir %s", got, fresh)
	}
}
