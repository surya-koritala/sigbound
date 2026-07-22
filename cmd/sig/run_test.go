package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// buildTestAgent compiles the deterministic sig-testagent helper and returns
// its binary path, so the driver runs a real subprocess agent (no live LLM).
func buildTestAgent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "testagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/surya-koritala/sigbound/cmd/sig-testagent")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sig-testagent: %v\n%s", err, out)
	}
	return bin
}

// makeGoRepo initializes a temp module with a couple of Go files plus a data
// file agents will edit. `go build ./...` on the base tree passes.
func makeGoRepo(t *testing.T) (*gitx.Git, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	g := gitx.New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/e2e\n\ngo 1.21\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	// shared.txt: many distinct lines so an edit targets an unambiguous line.
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString("shared base line ")
		sb.WriteByte(byte('0' + i%10))
		sb.WriteByte('\n')
	}
	write("shared.txt", sb.String())
	if _, err := g.CommitAll(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	return g, dir
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDriveRunEndToEnd exercises the whole driver on a real repo with the
// deterministic agent: two disjoint tasks and one that overlaps a third on the
// same line of a shared file, resolved by a trivial union resolver, then
// verified with `go build ./...`.
func TestDriveRunEndToEnd(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	// t1: disjoint (own new file). t2: new file + edits shared.txt line 5.
	// t3: edits the SAME shared.txt line 5 -> real conflict with t2.
	tasks := []taskSpec{
		{ID: "t1", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"},
		})},
		{ID: "t2", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"beta.go": "package main\n\nfunc beta() int { return 2 }\n"},
			"edit":  map[string]any{"file": "shared.txt", "line": 5, "text": "t2-was-here"},
		})},
		{ID: "t3", Prompt: mustJSON(t, map[string]any{
			"edit": map[string]any{"file": "shared.txt", "line": 5, "text": "t3-was-here"},
		})},
	}

	p := runParams{
		Repo:     repo,
		Base:     "main",
		Strategy: "overlay",
		AgentCmd: agent, // sh -c execs the binary; it reads SIGBOUND_TASK
		// Trivial union resolver: emit ours then theirs (keeps both agents' work).
		ResolverCmd:     `cat "$SIGBOUND_OURS" "$SIGBOUND_THEIRS"`,
		ResolverTimeout: 10 * time.Second,
		VerifyCmd:       "go build ./...",
	}

	rep, err := driveRun(ctx, p, tasks)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}

	// ---- every agent committed on its own branch ----
	if len(rep.PerAgent) != 3 {
		t.Fatalf("perAgent=%d, want 3", len(rep.PerAgent))
	}
	byID := map[string]perAgentJSON{}
	for _, a := range rep.PerAgent {
		byID[a.ID] = a
		if !a.OK {
			t.Fatalf("agent %s not ok: exit=%d stderr=%q", a.ID, a.Exit, a.Stderr)
		}
		if a.Branch != "agent/"+a.ID {
			t.Fatalf("agent %s branch=%q, want agent/%s", a.ID, a.Branch, a.ID)
		}
		if a.SHA == "" || a.SHA == rep.BaseSHA {
			t.Fatalf("agent %s did not advance its branch (sha=%q)", a.ID, a.SHA)
		}
	}
	// write-sets: t1 disjoint, t2 touches beta.go + shared.txt, t3 touches shared.txt.
	if got := byID["t1"].Files; len(got) != 1 || got[0] != "alpha.go" {
		t.Fatalf("t1 files=%v, want [alpha.go]", got)
	}
	if !contains(byID["t2"].Files, "shared.txt") || !contains(byID["t2"].Files, "beta.go") {
		t.Fatalf("t2 files=%v, want beta.go+shared.txt", byID["t2"].Files)
	}

	// ---- integration: t1 disjoint (own group) parallel to the {t2,t3} group ----
	ig := rep.Integrate
	if ig.Groups != 2 {
		t.Fatalf("integrate groups=%d, want 2 (disjoint t1 || {t2,t3})", ig.Groups)
	}
	if len(ig.Landed) != 3 {
		t.Fatalf("landed=%d (%v), want 3", len(ig.Landed), ig.Landed)
	}
	if len(ig.Flagged) != 0 {
		t.Fatalf("flagged=%v, want none (resolver resolves the overlap)", ig.Flagged)
	}
	if ig.Resolved < 1 {
		t.Fatalf("resolved=%d, want >=1 (t2/t3 overlap resolved)", ig.Resolved)
	}
	if ig.FinalSHA == "" {
		t.Fatal("empty finalSHA")
	}

	// ---- final tree contains all disjoint work AND both overlap markers ----
	paths, err := g.LsTree(ctx, ig.FinalSHA)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alpha.go", "beta.go", "shared.txt", "main.go", "go.mod"} {
		if !contains(paths, want) {
			t.Fatalf("final tree %s missing %s (have %v)", ig.FinalSHA, want, paths)
		}
	}
	shared, err := g.ShowFile(ctx, ig.FinalSHA, "shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shared, "t2-was-here") || !strings.Contains(shared, "t3-was-here") {
		t.Fatalf("resolved shared.txt missing a marker:\n%s", shared)
	}

	// ---- base branch advanced to the integrated commit ----
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != ig.FinalSHA {
		t.Fatalf("main=%s not advanced to finalSHA=%s", mainSHA, ig.FinalSHA)
	}

	// ---- verify (`go build ./...`) passed on the integrated tree ----
	if !rep.Verify.Ran || !rep.Verify.OK {
		t.Fatalf("verify ran=%v ok=%v output=%q", rep.Verify.Ran, rep.Verify.OK, rep.Verify.Output)
	}
}

// TestDriveRunFlagsWithoutResolver runs the same overlap with NO resolver: the
// overlapping branch must be flagged (not silently dropped, not landed).
func TestDriveRunFlagsWithoutResolver(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	tasks := []taskSpec{
		{ID: "a", Prompt: mustJSON(t, map[string]any{
			"edit": map[string]any{"file": "shared.txt", "line": 5, "text": "a-here"},
		})},
		{ID: "b", Prompt: mustJSON(t, map[string]any{
			"edit": map[string]any{"file": "shared.txt", "line": 5, "text": "b-here"},
		})},
	}
	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent}

	rep, err := driveRun(ctx, p, tasks)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.Integrate.Landed) != 1 || len(rep.Integrate.Flagged) != 1 {
		t.Fatalf("landed=%d flagged=%d, want 1/1", len(rep.Integrate.Landed), len(rep.Integrate.Flagged))
	}
	if got := rep.Integrate.Flagged[0].Paths; len(got) != 1 || got[0] != "shared.txt" {
		t.Fatalf("flagged paths=%v, want [shared.txt]", got)
	}
}

// TestDriveRunAssertHealthy wires -assert (runParams.Assert -> integrateBranches
// -> cell.WithAssert) end to end on a healthy disjoint batch: it must still
// land everything normally, proving the paranoid cross-check doesn't change
// the outcome when there's nothing wrong to catch.
func TestDriveRunAssertHealthy(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	tasks := []taskSpec{
		{ID: "a", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
		})},
		{ID: "b", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"b.go": "package main\n\nfunc b() int { return 2 }\n"},
		})},
	}
	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", Assert: true, AgentCmd: agent}

	rep, err := driveRun(ctx, p, tasks)
	if err != nil {
		t.Fatalf("driveRun with -assert: %v", err)
	}
	if len(rep.Integrate.Landed) != 2 || len(rep.Integrate.Flagged) != 0 {
		t.Fatalf("landed=%d flagged=%d, want 2/0", len(rep.Integrate.Landed), len(rep.Integrate.Flagged))
	}
}

// TestIntegrateBranchesReusesPrecomputedWriteSets is the correctness anchor for
// the writeSets param. The write-set only drives OCC PARTITIONING (which
// branches are treated as overlapping); actual landed content always comes
// from the branch's real tree, so the only observable signal that a supplied
// write-set was TRUSTED (not silently re-diffed and corrected) is the
// resulting group count. Two branches touch disjoint real files (x.txt,
// y.txt — an accurate diff partitions them into 2 independent groups), but a
// deliberately WRONG writeSets claims they both touched the same path: if
// integrateBranches actually used that lie for partitioning, they land forced
// into ONE group instead of two. The complementary nil-writeSets call proves
// the batched fallback (gitx.DiffNameOnlyBatch) still computes the correct,
// disjoint real write-sets when nothing is precomputed.
func TestIntegrateBranchesReusesPrecomputedWriteSets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := gitx.New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("k\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}

	mkBranch := func(branch, file string) {
		wt := filepath.Join(t.TempDir(), branch)
		if err := g.WorktreeAdd(ctx, wt, branch, base); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, file), []byte("new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := g.At(wt).CommitAll(ctx, branch); err != nil {
			t.Fatal(err)
		}
		if err := g.WorktreeRemove(ctx, wt); err != nil {
			t.Fatal(err)
		}
	}
	mkBranch("agent/x", "x.txt")
	mkBranch("agent/y", "y.txt")
	branches := []string{"agent/x", "agent/y"}

	// Lie: both branches supposedly touched the same path, though their real
	// diffs (x.txt vs y.txt) are disjoint.
	lying := map[string][]string{
		"agent/x": {"fake-shared.txt"},
		"agent/y": {"fake-shared.txt"},
	}
	res, err := integrateBranches(ctx, g, "main", base, branches, lying, "overlay", "", 0, false, false)
	if err != nil {
		t.Fatalf("integrateBranches (lying writeSets): %v", err)
	}
	if res.Groups != 1 {
		t.Fatalf("groups=%d with a lying shared-path writeSets, want 1 (forced together): integrateBranches did not trust the supplied write-sets", res.Groups)
	}
	if len(res.Landed) != 2 {
		t.Fatalf("landed=%v, want both branches (folding two non-conflicting real diffs together must still succeed)", res.Landed)
	}

	// No precomputed data at all: the batched fallback must compute the real,
	// disjoint write-sets and partition the same two branches into 2 groups.
	res, err = integrateBranches(ctx, g, "main", base, branches, nil, "overlay", "", 0, false, false)
	if err != nil {
		t.Fatalf("integrateBranches (nil writeSets): %v", err)
	}
	if res.Groups != 2 {
		t.Fatalf("groups=%d with nil writeSets, want 2 (real diffs are disjoint): the batched fallback computed the wrong write-sets", res.Groups)
	}
	if len(res.Landed) != 2 {
		t.Fatalf("landed=%v, want both branches", res.Landed)
	}
}

// TestDriveRunLaneEnforcement: an agent that declares Files=[a.go] but also
// writes b.go is "out of lane". In warn mode it still lands but the report
// records strayed=[b.go]; in strict mode it is treated as a failed agent — not
// landed — with the reason recorded.
func TestDriveRunLaneEnforcement(t *testing.T) {
	ctx := context.Background()
	agent := buildTestAgent(t)

	// Declares only a.go, but the agent writes a.go AND b.go.
	prompt := mustJSON(t, map[string]any{
		"write": map[string]string{
			"a.go": "package main\n\nfunc a() int { return 1 }\n",
			"b.go": "package main\n\nfunc b() int { return 2 }\n",
		},
	})
	task := taskSpec{ID: "lane1", Prompt: prompt, Files: []string{"a.go"}}

	// ---- warn (default): lands with strayed=[b.go], inLane=false ----
	_, repoWarn := makeGoRepo(t)
	warn := runParams{Repo: repoWarn, Base: "main", Strategy: "overlay", AgentCmd: agent, LaneMode: laneWarn}
	repW, err := driveRun(ctx, warn, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun warn: %v", err)
	}
	aw := repW.PerAgent[0]
	if !aw.OK {
		t.Fatalf("warn: out-of-lane agent should still land, got not OK: %q", aw.Stderr)
	}
	if aw.InLane {
		t.Fatal("warn: agent should be marked out-of-lane (inLane=false)")
	}
	if !contains(aw.Strayed, "b.go") || contains(aw.Strayed, "a.go") {
		t.Fatalf("warn: strayed=%v, want exactly [b.go]", aw.Strayed)
	}
	if !contains(aw.DeclaredFiles, "a.go") {
		t.Fatalf("warn: declaredFiles=%v, want a.go", aw.DeclaredFiles)
	}
	if len(repW.Integrate.Landed) != 1 {
		t.Fatalf("warn: landed=%d, want 1 (still lands)", len(repW.Integrate.Landed))
	}

	// ---- strict: NOT landed, agent not OK, reason recorded ----
	_, repoStrict := makeGoRepo(t)
	strict := runParams{Repo: repoStrict, Base: "main", Strategy: "overlay", AgentCmd: agent, LaneMode: laneStrict}
	repS, err := driveRun(ctx, strict, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun strict: %v", err)
	}
	as := repS.PerAgent[0]
	if as.OK {
		t.Fatal("strict: out-of-lane agent must not be OK")
	}
	if as.InLane {
		t.Fatal("strict: agent should be out-of-lane")
	}
	if !contains(as.Strayed, "b.go") {
		t.Fatalf("strict: strayed=%v, want b.go", as.Strayed)
	}
	if !strings.Contains(as.Stderr, "out-of-lane") {
		t.Fatalf("strict: out-of-lane reason not recorded: %q", as.Stderr)
	}
	if len(repS.Integrate.Landed) != 0 {
		t.Fatalf("strict: landed=%d, want 0 (out-of-lane agent not landed)", len(repS.Integrate.Landed))
	}

	// ---- off: no lane accounting even though it strayed; still lands ----
	_, repoOff := makeGoRepo(t)
	off := runParams{Repo: repoOff, Base: "main", Strategy: "overlay", AgentCmd: agent, LaneMode: laneOff}
	repO, err := driveRun(ctx, off, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun off: %v", err)
	}
	ao := repO.PerAgent[0]
	if !ao.OK || !ao.InLane || len(ao.Strayed) != 0 {
		t.Fatalf("off: want OK, inLane=true, no strayed; got ok=%v inLane=%v strayed=%v", ao.OK, ao.InLane, ao.Strayed)
	}
	if len(repO.Integrate.Landed) != 1 {
		t.Fatalf("off: landed=%d, want 1", len(repO.Integrate.Landed))
	}
}

// TestDriveRunKeepFailed: -keep-failed retains a FAILED agent's worktree on
// disk (and names it in the report) instead of tearing it down; without the
// flag the worktree is removed as before. A successful agent's worktree is
// always removed, flag or not — -keep-failed only affects failures.
func TestDriveRunKeepFailed(t *testing.T) {
	ctx := context.Background()
	task := taskSpec{ID: "bad", Prompt: "x"} // sig-testagent would fail on this prompt too, but "exit 1" is simpler and direct

	// ---- failing agent, -keep-failed: worktree survives, path reported ----
	_, repo := makeGoRepo(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // pin driveRun's worktree root under a dir this test controls
	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: "exit 1", KeepFailed: true}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.PerAgent) != 1 || rep.PerAgent[0].OK {
		t.Fatalf("want one failed agent, got %+v", rep.PerAgent)
	}
	kept := rep.PerAgent[0].WorktreeKept
	if kept == "" {
		t.Fatal("want worktreeKept set for a failed agent run with -keep-failed")
	}
	if fi, err := os.Stat(kept); err != nil || !fi.IsDir() {
		t.Fatalf("kept worktree %s should still exist as a dir: %v", kept, err)
	}

	// ---- same failure, no -keep-failed: worktree (and its root) cleaned up ----
	_, repo2 := makeGoRepo(t)
	tmp2 := t.TempDir()
	t.Setenv("TMPDIR", tmp2)
	p2 := runParams{Repo: repo2, Base: "main", Strategy: "overlay", AgentCmd: "exit 1"}
	rep2, err := driveRun(ctx, p2, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if got := rep2.PerAgent[0].WorktreeKept; got != "" {
		t.Fatalf("want no worktreeKept without -keep-failed, got %q", got)
	}
	entries, err := os.ReadDir(tmp2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("want the run's worktree root cleaned up under %s, found %v", tmp2, entries)
	}

	// ---- successful agent, -keep-failed set: still removed (not a failure) ----
	_, repo3 := makeGoRepo(t)
	tmp3 := t.TempDir()
	t.Setenv("TMPDIR", tmp3)
	agent := buildTestAgent(t)
	goodTask := taskSpec{ID: "good", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"ok.go": "package main\n\nfunc ok() int { return 1 }\n"},
	})}
	p3 := runParams{Repo: repo3, Base: "main", Strategy: "overlay", AgentCmd: agent, KeepFailed: true}
	rep3, err := driveRun(ctx, p3, []taskSpec{goodTask})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep3.PerAgent[0].OK {
		t.Fatalf("want the agent to succeed, got %+v", rep3.PerAgent[0])
	}
	if got := rep3.PerAgent[0].WorktreeKept; got != "" {
		t.Fatalf("want no worktreeKept for a successful agent even with -keep-failed, got %q", got)
	}
	entries3, err := os.ReadDir(tmp3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries3) != 0 {
		t.Fatalf("want the successful agent's worktree root cleaned up under %s, found %v", tmp3, entries3)
	}

	// ---- out-of-lane under -lanes strict, -keep-failed: strict turns the stray
	// into a failure, so it must be kept too, same as any other failed agent ----
	_, repo4 := makeGoRepo(t)
	tmp4 := t.TempDir()
	t.Setenv("TMPDIR", tmp4)
	strayAgent := buildTestAgent(t)
	strayTask := taskSpec{ID: "stray", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{
			"a.go": "package main\n\nfunc a() int { return 1 }\n",
			"b.go": "package main\n\nfunc b() int { return 2 }\n",
		},
	}), Files: []string{"a.go"}}
	p4 := runParams{Repo: repo4, Base: "main", Strategy: "overlay", AgentCmd: strayAgent, LaneMode: laneStrict, KeepFailed: true}
	rep4, err := driveRun(ctx, p4, []taskSpec{strayTask})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	a4 := rep4.PerAgent[0]
	if a4.OK {
		t.Fatal("strict+keep-failed: out-of-lane agent must not be OK")
	}
	kept4 := a4.WorktreeKept
	if kept4 == "" {
		t.Fatal("strict+keep-failed: want worktreeKept set for an out-of-lane agent under -lanes strict")
	}
	if fi, err := os.Stat(kept4); err != nil || !fi.IsDir() {
		t.Fatalf("kept worktree %s should still exist as a dir: %v", kept4, err)
	}
}

// TestDriveRunAgentTimeout: -agent-timeout bounds a single hung agent instead
// of letting it block the whole run. The agent sleeps far longer than the
// timeout; a driveRun call that actually enforces -agent-timeout returns well
// before the sleep would finish on its own (asserted below), and the failed
// attempt is reported exit=-1, timedOut=true, with the reason in stderr.
func TestDriveRunAgentTimeout(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	task := taskSpec{ID: "slow", Prompt: "x"}

	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: "sleep 5", AgentTimeout: 200 * time.Millisecond}
	start := time.Now()
	rep, err := driveRun(ctx, p, []taskSpec{task})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("driveRun took %s; -agent-timeout 200ms should have cut the 5s sleep short", elapsed)
	}
	if len(rep.PerAgent) != 1 {
		t.Fatalf("perAgent=%d, want 1", len(rep.PerAgent))
	}
	a := rep.PerAgent[0]
	if a.OK {
		t.Fatal("timed-out agent must not be OK")
	}
	if !a.TimedOut {
		t.Fatal("want timedOut=true")
	}
	if a.Exit != -1 {
		t.Fatalf("exit=%d, want -1", a.Exit)
	}
	if !strings.Contains(a.Stderr, "agent-timeout") {
		t.Fatalf("stderr should note the timeout: %q", a.Stderr)
	}
	if a.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1 (no -agent-retries set)", a.Attempts)
	}
}

// TestDriveRunAgentRetries: -agent-retries re-runs a FAILED agent in a fresh
// worktree off the same base, up to N more times, and Attempts reports the
// total number of tries made. Cases: succeeds on the retry; the existing
// no-retries behavior is unchanged; a lane-strict out-of-lane failure is
// never retried (it's a plan violation, not a timing fluke a retry could
// fix) — both alone and combined with -keep-failed, where the stray's
// worktree must still be kept even though it stops on attempt 1 (the attempt
// that ends the loop, not the one -agent-retries would have predicted as
// "last"); and -keep-failed keeps only the LAST failed attempt's worktree,
// tearing every earlier one down.
func TestDriveRunAgentRetries(t *testing.T) {
	ctx := context.Background()
	badTask := taskSpec{ID: "bad", Prompt: "x"}

	// ---- fails on the first invocation, succeeds on the second ----
	_, repo := makeGoRepo(t)
	marker := filepath.Join(t.TempDir(), "attempt-marker")
	flakyAgent := "test -f '" + marker + "' && { echo done > out.txt; exit 0; } || { touch '" + marker + "'; exit 1; }"
	flakyTask := taskSpec{ID: "flaky", Prompt: "x"}
	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: flakyAgent,
		AgentRetries: 1, Autocommit: true,
	}
	rep, err := driveRun(ctx, p, []taskSpec{flakyTask})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	a := rep.PerAgent[0]
	if !a.OK {
		t.Fatalf("want the retried agent to succeed, got %+v", a)
	}
	if a.Attempts != 2 {
		t.Fatalf("attempts=%d, want 2 (failed once, succeeded on retry)", a.Attempts)
	}
	if !a.Autocommitted || !contains(a.Files, "out.txt") {
		t.Fatalf("want the successful retry's edit autocommitted: %+v", a)
	}

	// ---- -agent-retries 0 (default): today's behavior — a single failing
	// attempt fails outright, never retried ----
	_, repo0 := makeGoRepo(t)
	p0 := runParams{Repo: repo0, Base: "main", Strategy: "overlay", AgentCmd: "exit 1"}
	rep0, err := driveRun(ctx, p0, []taskSpec{badTask})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	a0 := rep0.PerAgent[0]
	if a0.OK {
		t.Fatal("want the agent to fail with no retries configured")
	}
	if a0.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1 (no -agent-retries set)", a0.Attempts)
	}

	// ---- lane-strict stray: a plan violation, never retried even though
	// -agent-retries is set ----
	_, repoStray := makeGoRepo(t)
	strayAgent := buildTestAgent(t)
	strayTask := taskSpec{ID: "stray", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{
			"a.go": "package main\n\nfunc a() int { return 1 }\n",
			"b.go": "package main\n\nfunc b() int { return 2 }\n",
		},
	}), Files: []string{"a.go"}}
	pStray := runParams{Repo: repoStray, Base: "main", Strategy: "overlay", AgentCmd: strayAgent, LaneMode: laneStrict, AgentRetries: 3}
	repStray, err := driveRun(ctx, pStray, []taskSpec{strayTask})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	aStray := repStray.PerAgent[0]
	if aStray.OK {
		t.Fatal("strict: out-of-lane agent must not be OK")
	}
	if aStray.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1 (a lane-strict stray is never retried)", aStray.Attempts)
	}

	// ---- -keep-failed + -agent-retries>0 + lane-strict stray: the stray stops
	// the retry loop on attempt 1, which IS the attempt that ends the loop, so
	// -keep-failed must still keep its worktree — even though a naive
	// "attempt > AgentRetries" check would (wrongly) call attempt 1 "not the
	// last attempt" since AgentRetries is 3 ----
	_, repoStrayKF := makeGoRepo(t)
	tmpStrayKF := t.TempDir()
	t.Setenv("TMPDIR", tmpStrayKF)
	strayAgentKF := buildTestAgent(t)
	strayTaskKF := taskSpec{ID: "stray", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{
			"a.go": "package main\n\nfunc a() int { return 1 }\n",
			"b.go": "package main\n\nfunc b() int { return 2 }\n",
		},
	}), Files: []string{"a.go"}}
	pStrayKF := runParams{Repo: repoStrayKF, Base: "main", Strategy: "overlay", AgentCmd: strayAgentKF, LaneMode: laneStrict, AgentRetries: 3, KeepFailed: true}
	repStrayKF, err := driveRun(ctx, pStrayKF, []taskSpec{strayTaskKF})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	aStrayKF := repStrayKF.PerAgent[0]
	if aStrayKF.OK {
		t.Fatal("strict+keep-failed: out-of-lane agent must not be OK")
	}
	if aStrayKF.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1 (a lane-strict stray is never retried)", aStrayKF.Attempts)
	}
	if aStrayKF.WorktreeKept == "" {
		t.Fatal("strict+keep-failed+agent-retries: want worktreeKept set on the attempt that actually ends the loop")
	}
	if fi, err := os.Stat(aStrayKF.WorktreeKept); err != nil || !fi.IsDir() {
		t.Fatalf("kept worktree %s should still exist as a dir: %v", aStrayKF.WorktreeKept, err)
	}

	// ---- -keep-failed + -agent-retries: only the LAST failed attempt's
	// worktree survives; earlier ones are torn down as each attempt fails ----
	_, repoKF := makeGoRepo(t)
	tmpKF := t.TempDir()
	t.Setenv("TMPDIR", tmpKF)
	pKF := runParams{Repo: repoKF, Base: "main", Strategy: "overlay", AgentCmd: "exit 1", AgentRetries: 2, KeepFailed: true}
	repKF, err := driveRun(ctx, pKF, []taskSpec{badTask})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	aKF := repKF.PerAgent[0]
	if aKF.OK {
		t.Fatal("want the agent to still fail after exhausting retries")
	}
	if aKF.Attempts != 3 {
		t.Fatalf("attempts=%d, want 3 (1 + 2 retries)", aKF.Attempts)
	}
	if aKF.WorktreeKept == "" {
		t.Fatal("want worktreeKept set for the final failed attempt")
	}
	if fi, err := os.Stat(aKF.WorktreeKept); err != nil || !fi.IsDir() {
		t.Fatalf("kept worktree %s should still exist as a dir: %v", aKF.WorktreeKept, err)
	}
	runRootEntries, err := os.ReadDir(tmpKF)
	if err != nil {
		t.Fatal(err)
	}
	if len(runRootEntries) != 1 {
		t.Fatalf("want exactly one sig-run-* root under %s, found %v", tmpKF, runRootEntries)
	}
	wtEntries, err := os.ReadDir(filepath.Join(tmpKF, runRootEntries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if len(wtEntries) != 1 {
		t.Fatalf("want exactly one surviving worktree dir (only the last failed attempt kept), found %v", wtEntries)
	}
}

// gitOut runs a git plumbing command directly against repo (bypassing gitx,
// which has no exported branch-creation call) and returns trimmed stdout.
func gitOut(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestDriveRunAgentRetriesPreExistingBranchSurvives: a foreign agent/<id>
// branch left over from a PRIOR run (holding a real, committed user commit)
// must survive a run that reuses that same task ID with -agent-retries set.
// Attempt 1's WorktreeAdd loud-fails on the collision (see WorktreeAdd's
// doc comment); that must be terminal — never retried into a
// WorktreeAddReset that would -B-reset the foreign branch and silently
// destroy the prior commit. This is the regression case for the bug where
// the retry gate was keyed off `attempt >= 2` instead of "did THIS run
// create the branch": that condition can't tell a foreign pre-existing
// branch apart from one this run made itself on an earlier attempt.
func TestDriveRunAgentRetriesPreExistingBranchSurvives(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)

	baseSHA := gitOut(t, repo, "rev-parse", "HEAD")
	tree := gitOut(t, repo, "rev-parse", baseSHA+"^{tree}")
	// A distinct commit (new SHA, same tree) simulating a prior run's landed
	// work, reachable only through agent/collide.
	userSHA := gitOut(t, repo, "commit-tree", tree, "-p", baseSHA, "-m", "user work from a prior run")
	gitOut(t, repo, "update-ref", "refs/heads/agent/collide", userSHA)

	task := taskSpec{ID: "collide", Prompt: "x"}
	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: "exit 0", AgentRetries: 2}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	a := rep.PerAgent[0]
	if a.OK {
		t.Fatal("want the agent to fail: agent/collide collides with a foreign branch")
	}
	if a.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1 (a WorktreeAdd collision is terminal, never retried)", a.Attempts)
	}
	if !strings.Contains(a.Stderr, "worktree add") {
		t.Fatalf("want the worktree-add failure reported in stderr, got %q", a.Stderr)
	}
	if got := gitOut(t, repo, "rev-parse", "refs/heads/agent/collide"); got != userSHA {
		t.Fatalf("agent/collide moved from %s to %s: the foreign branch's commit was destroyed", userSHA, got)
	}
}

// TestDriveRunBudgetExhausted: -budget caps the whole run (agent phase +
// integrate + verify) with one wall-clock ceiling. Two tasks share one -agent
// command that routes on SIGBOUND_TASK_ID: "fast" commits immediately via the
// deterministic test agent, "slow" sleeps far longer than the budget. Once
// the budget fires, the slow agent is cancelled (fails) and ctx is already
// expired by the time driveRun reaches integrate/land, so those git calls
// can't complete either — driveRun returns an honest operational error
// naming the budget rather than landing a partial tree. (The alternative —
// completing integration inside a leftover grace period — would need a
// second, ungated context and contradicts what -budget promises; see
// driveRun's -budget comment. This is the "pick the achievable assertion"
// case called out in the -budget design.)
func TestDriveRunBudgetExhausted(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	tasks := []taskSpec{
		{ID: "fast", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"fast.go": "package main\n\nfunc fast() int { return 1 }\n"},
		})},
		{ID: "slow", Prompt: "x"},
	}
	// -agent is one command for every task; route on SIGBOUND_TASK_ID so "slow"
	// sleeps while "fast" runs the real committing test agent.
	agentCmd := `if [ "$SIGBOUND_TASK_ID" = "slow" ]; then sleep 6; else ` + agent + `; fi`

	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agentCmd, Budget: 900 * time.Millisecond}
	start := time.Now()
	rep, err := driveRun(ctx, p, tasks)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("driveRun took %s; -budget 900ms should have cut the 6s sleep short", elapsed)
	}
	if err == nil {
		t.Fatalf("want an honest operational error once the budget is exhausted, got a clean report: %+v", rep)
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("error should name the exhausted budget: %v", err)
	}

	byID := map[string]perAgentJSON{}
	for _, a := range rep.PerAgent {
		byID[a.ID] = a
	}
	if !byID["fast"].OK {
		t.Fatalf("fast agent should have finished well inside the budget: %+v", byID["fast"])
	}
	if byID["slow"].OK {
		t.Fatal("slow agent should have been cancelled by the budget")
	}

	// The budget's honest failure must never leave a partial tree landed.
	mainSHA, err2 := g.RevParse(ctx, "main")
	if err2 != nil {
		t.Fatal(err2)
	}
	if mainSHA != rep.BaseSHA {
		t.Fatalf("main=%s advanced past baseSHA=%s despite the budget failing the run", mainSHA, rep.BaseSHA)
	}
}

// TestDriveRunVerifyKilledByBudget: when -budget expires while -verify itself
// is running (agent phase + integrate already finished, unlike
// TestDriveRunBudgetExhausted above where the budget kills an agent), verify
// naturally fails because its command gets killed along with ctx — but
// without help that reads as an ordinary verify failure, hiding the real
// cause. driveRun must name the exhausted budget in the verify output.
func TestDriveRunVerifyKilledByBudget(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	task := taskSpec{ID: "noop", Prompt: "x"} // agent does nothing; nothing needs to land for verify to run

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: "exit 0",
		VerifyCmd: "sleep 5", Budget: 700 * time.Millisecond,
	}
	start := time.Now()
	rep, err := driveRun(ctx, p, []taskSpec{task})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("driveRun: %v (verify failing is a report-level outcome, not an operational error)", err)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("driveRun took %s; -budget 700ms should have cut the 5s verify sleep short", elapsed)
	}
	if !rep.Verify.Ran || rep.Verify.OK {
		t.Fatalf("want verify to have run and failed once the budget killed it, got %+v", rep.Verify)
	}
	if !strings.Contains(rep.Verify.Output, "budget") {
		t.Fatalf("verify output should name the exhausted budget instead of reading as a plain failure, got: %q", rep.Verify.Output)
	}
}

// TestRunExitCode exercises the outcome->exit-code mapping directly (the
// seam runRun uses), including the override precedence when a report matches
// more than one failure class: verify-failed > no-agent-succeeded > flagged.
func TestRunExitCode(t *testing.T) {
	cases := []struct {
		name string
		rep  runReport
		want int
	}{
		{
			name: "clean landed, verify unset",
			rep:  runReport{PerAgent: []perAgentJSON{{OK: true}}},
			want: exitOK,
		},
		{
			name: "clean landed, verify passed",
			rep: runReport{
				PerAgent: []perAgentJSON{{OK: true}},
				Verify:   verifyJSON{Ran: true, OK: true},
			},
			want: exitOK,
		},
		{
			name: "verify failed wins over flagged",
			rep: runReport{
				PerAgent:  []perAgentJSON{{OK: true}},
				Integrate: integrateJSON{Flagged: []flaggedJSON{{Branch: "agent/x"}}},
				Verify:    verifyJSON{Ran: true, OK: false},
			},
			want: exitVerifyFailed,
		},
		{
			name: "verify failed wins over no-agent-succeeded",
			rep: runReport{
				PerAgent: []perAgentJSON{{OK: false}},
				Verify:   verifyJSON{Ran: true, OK: false},
			},
			want: exitVerifyFailed,
		},
		{
			name: "no agent succeeded, verify never ran",
			rep:  runReport{PerAgent: []perAgentJSON{{OK: false}, {OK: false}}},
			want: exitNoAgentSucceeded,
		},
		{
			name: "no agent succeeded, verify trivially passed",
			rep: runReport{
				PerAgent: []perAgentJSON{{OK: false}},
				Verify:   verifyJSON{Ran: true, OK: true},
			},
			want: exitNoAgentSucceeded,
		},
		{
			name: "no-agent-succeeded wins over flagged",
			rep: runReport{
				PerAgent:  []perAgentJSON{{OK: false}},
				Integrate: integrateJSON{Flagged: []flaggedJSON{{Branch: "agent/x"}}},
			},
			want: exitNoAgentSucceeded,
		},
		{
			name: "flagged only",
			rep: runReport{
				PerAgent:  []perAgentJSON{{OK: true}, {OK: true}},
				Integrate: integrateJSON{Flagged: []flaggedJSON{{Branch: "agent/x"}}},
			},
			want: exitFlagged,
		},
	}
	for _, c := range cases {
		if got := runExitCode(c.rep); got != c.want {
			t.Errorf("%s: runExitCode=%d, want %d", c.name, got, c.want)
		}
	}
}

// TestDriveRunIntegrateFailureKeepsPerAgent: a REAL integrate failure (an
// unknown strategy name — driveRun itself doesn't validate it, unlike runRun's
// flag parsing) after two agents have genuinely committed. driveRun must
// return the error AND a report whose PerAgent already names the branches
// those agents landed — the exact data runRun needs to still emit on a
// mid-run failure. Regression test for issue #5: this data used to be
// silently discarded by runRun's caller.
func TestDriveRunIntegrateFailureKeepsPerAgent(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	tasks := []taskSpec{
		{ID: "t1", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"},
		})},
		{ID: "t2", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"beta.go": "package main\n\nfunc beta() int { return 2 }\n"},
		})},
	}
	p := runParams{Repo: repo, Base: "main", Strategy: "not-a-real-strategy", AgentCmd: agent}

	rep, err := driveRun(ctx, p, tasks)
	if err == nil {
		t.Fatal("expected an integrate error for an unknown strategy")
	}
	if !strings.Contains(err.Error(), "integrate:") {
		t.Fatalf("err=%q, want it to name the integrate stage", err)
	}

	if len(rep.PerAgent) != 2 {
		t.Fatalf("perAgent=%d, want 2 (both agents ran before the integrate error)", len(rep.PerAgent))
	}
	for _, a := range rep.PerAgent {
		if !a.OK {
			t.Fatalf("agent %s not ok: exit=%d stderr=%q", a.ID, a.Exit, a.Stderr)
		}
		if a.SHA == "" || a.SHA == rep.BaseSHA {
			t.Fatalf("agent %s did not advance its branch (sha=%q)", a.ID, a.SHA)
		}
		if a.Branch != "agent/"+a.ID {
			t.Fatalf("agent %s branch=%q, want agent/%s", a.ID, a.Branch, a.ID)
		}
	}
	// Nothing integrated: the report's integrate section stays zero-valued.
	if len(rep.Integrate.Landed) != 0 {
		t.Fatalf("landed=%v, want none (integrate itself errored)", rep.Integrate.Landed)
	}
}

// TestEmitReport is a direct test of the seam runRun calls to render the
// report (on both the success path and the new mid-run-error path): given a
// report with PerAgent already filled in, it must write valid JSON in -json
// mode and the terse summary otherwise — the exact two code paths runRun used
// to have inlined before driveRun's error path needed the same rendering.
func TestEmitReport(t *testing.T) {
	rep := runReport{
		Repo: "/tmp/repo", Base: "main", BaseSHA: "abc123",
		PerAgent: []perAgentJSON{
			{ID: "t1", Branch: "agent/t1", SHA: "deadbeef", OK: true, Files: []string{"alpha.go"}},
		},
	}

	var jsonBuf bytes.Buffer
	if err := emitReport(&jsonBuf, rep, true); err != nil {
		t.Fatalf("emitReport(json): %v", err)
	}
	var got runReport
	if err := json.Unmarshal(jsonBuf.Bytes(), &got); err != nil {
		t.Fatalf("parse emitted JSON: %v\n%s", err, jsonBuf.String())
	}
	if len(got.PerAgent) != 1 || got.PerAgent[0].Branch != "agent/t1" || got.PerAgent[0].SHA != "deadbeef" {
		t.Fatalf("round-tripped report perAgent=%+v, want the agent/t1 branch preserved", got.PerAgent)
	}

	var sumBuf bytes.Buffer
	if err := emitReport(&sumBuf, rep, false); err != nil {
		t.Fatalf("emitReport(summary): %v", err)
	}
	summary := sumBuf.String()
	if !strings.Contains(summary, "agent/t1") || !strings.Contains(summary, "t1") {
		t.Fatalf("summary missing the surviving agent branch:\n%s", summary)
	}
}

// TestRunRunNoReportWhenNoAgentsRan: when driveRun fails before any agent ran
// (here: -base names a branch that doesn't exist, so RevParse fails first),
// runRun must print nothing — an empty PerAgent means there is nothing worth
// recovering, same as before this change.
func TestRunRunNoReportWhenNoAgentsRan(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	tasksFile := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(tasksFile, []byte(`[{"id":"a","prompt":"x"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-base", "does-not-exist",
		"-tasks", tasksFile,
		"-agent", agent,
	})
	if err == nil {
		t.Fatal("expected an error resolving a nonexistent base")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("no report should be written when no agents ran, got:\n%s", buf.String())
	}
}

// TestDriveRunVerifyRetries covers -verify-retries against a verify command
// that fails on its first invocation and passes on every one after. Each
// invocation runs in a FRESH detached worktree (see runVerify), so the
// fail-once state has to live outside the checkout: a marker file at a fixed
// path, created by the command itself on its first (failing) run.
func TestDriveRunVerifyRetries(t *testing.T) {
	ctx := context.Background()
	agent := buildTestAgent(t)
	task := taskSpec{ID: "ok", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"good.go": "package main\n\nfunc good() int { return 1 }\n"},
	})}
	flakyVerify := func(marker string) string {
		return "test -f '" + marker + "' || { touch '" + marker + "'; exit 1; }"
	}

	// ---- -verify-retries 1: fails once, retry passes -> ok=true, flaky=true ----
	_, repo1 := makeGoRepo(t)
	p1 := runParams{
		Repo: repo1, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:     flakyVerify(filepath.Join(t.TempDir(), "marker")),
		VerifyRetries: 1,
	}
	rep1, err := driveRun(ctx, p1, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep1.Verify.OK {
		t.Fatalf("verify.ok=false, want true after a retry: %q", rep1.Verify.Output)
	}
	if !rep1.Verify.Flaky {
		t.Fatal("verify.flaky=false, want true (passed only on the 2nd invocation)")
	}
	if mainSHA, err := gitx.New(repo1).RevParse(ctx, "main"); err != nil || mainSHA != rep1.Integrate.FinalSHA {
		t.Fatalf("flaky-but-green run must still land: main=%s finalSHA=%s err=%v", mainSHA, rep1.Integrate.FinalSHA, err)
	}

	// ---- -verify-retries 0 (fresh marker, same command): today's behavior — a
	// single failing invocation fails verify outright, never flaky ----
	_, repo0 := makeGoRepo(t)
	p0 := runParams{
		Repo: repo0, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:     flakyVerify(filepath.Join(t.TempDir(), "marker")),
		VerifyRetries: 0,
	}
	rep0, err := driveRun(ctx, p0, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep0.Verify.OK {
		t.Fatal("verify.ok=true with -verify-retries 0, want false (no retry to save a failing first invocation)")
	}
	if rep0.Verify.Flaky {
		t.Fatal("verify.flaky=true with -verify-retries 0, want false")
	}
	if code := runExitCode(rep0); code != exitVerifyFailed {
		t.Fatalf("runExitCode=%d, want exitVerifyFailed", code)
	}
}

// TestDriveRunRepairUsesVerifyRetries: the repair-loop interplay is untouched
// at the default -verify-retries=0 — same fixture, same assertions as
// TestDriveRunRepairSucceeds, just with VerifyRetries set explicitly to prove
// the new seam doesn't change repair behavior when retries are off.
func TestDriveRunRepairUsesVerifyRetries(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	p := runParams{
		Repo:          repo,
		Base:          "main",
		Strategy:      "overlay",
		AgentCmd:      agent,
		VerifyCmd:     "go build ./...",
		VerifyRetries: 0,
		RepairCmd:     `printf 'package main\n\nfunc helper() int { return helperX() }\n' > repair_fix.go`,
		RepairMax:     2,
	}

	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	v := rep.Verify
	if !v.Ran || !v.OK {
		t.Fatalf("verify ran=%v ok=%v output=%q", v.Ran, v.OK, v.Output)
	}
	if !v.Repaired {
		t.Fatal("repaired=false; expected the repair loop to have fixed an initial failure")
	}
	if v.Flaky {
		t.Fatal("flaky=true with -verify-retries 0; repair loop behavior must be unaffected")
	}
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != rep.Integrate.FinalSHA {
		t.Fatalf("main=%s not advanced to repaired finalSHA=%s", mainSHA, rep.Integrate.FinalSHA)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// brokenBuildTasks are two DISJOINT tasks whose combined tree does NOT compile:
// `caller.go` calls helper(), but the other task only defines helperX() — so the
// integrated tree fails `go build ./...` with "undefined: helper". Both write
// their own file, so integration itself is clean (the break is semantic, not a
// merge conflict) — exactly the case the self-healing repair loop targets.
func brokenBuildTasks(t *testing.T) []taskSpec {
	t.Helper()
	return []taskSpec{
		{ID: "caller", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"caller.go": "package main\n\nfunc caller() int { return helper() }\n"},
		})},
		{ID: "helper", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"helper.go": "package main\n\nfunc helperX() int { return 42 }\n"},
		})},
	}
}

// TestDriveRunRepairSucceeds: integration yields a tree that fails `go build`,
// -repair supplies the missing helper() (a deterministic, LLM-free fixer), and
// re-verify PASSES. The report must show repaired=true, attempts>=1, the fix
// commit's files, and the fix must LAND on the base branch.
func TestDriveRunRepairSucceeds(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		VerifyCmd: "go build ./...",
		// Deterministic fixer: define the missing helper() in a new file. The
		// driver auto-commits it (the fixer only edits, never runs git).
		RepairCmd: `printf 'package main\n\nfunc helper() int { return helperX() }\n' > repair_fix.go`,
		RepairMax: 2,
	}

	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}

	// Both agents committed disjoint files, integration landed both cleanly.
	if len(rep.Integrate.Landed) != 2 {
		t.Fatalf("landed=%v, want both agent branches", rep.Integrate.Landed)
	}

	v := rep.Verify
	if !v.Ran {
		t.Fatal("verify did not run")
	}
	if !v.OK {
		t.Fatalf("verify.ok=false after repair; output=%q", v.Output)
	}
	if !v.Repaired {
		t.Fatal("repaired=false; expected the repair loop to have fixed an initial failure")
	}
	if v.Attempts < 1 {
		t.Fatalf("attempts=%d, want >=1", v.Attempts)
	}
	if len(v.Repairs) == 0 {
		t.Fatal("no per-attempt repair records")
	}
	last := v.Repairs[len(v.Repairs)-1]
	if !last.VerifyOK {
		t.Fatalf("last repair attempt verifyOk=false: %+v", last)
	}
	if !contains(last.FilesTouched, "repair_fix.go") {
		t.Fatalf("repair filesTouched=%v, want repair_fix.go", last.FilesTouched)
	}

	// The fix must LAND: base branch advanced to the repaired head, whose tree
	// contains the fix file and actually compiles.
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != rep.Integrate.FinalSHA {
		t.Fatalf("main=%s not advanced to repaired finalSHA=%s", mainSHA, rep.Integrate.FinalSHA)
	}
	paths, err := g.LsTree(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"caller.go", "helper.go", "repair_fix.go"} {
		if !contains(paths, want) {
			t.Fatalf("landed tree missing %s (have %v)", want, paths)
		}
	}
}

// TestDriveRunVerifyFailLandsNothing: when -verify fails and no -repair is set,
// the base branch must NOT advance — a red run lands nothing, per the documented
// guarantee. Regression test for landing before verify (base ref advanced while
// verify.ok=false).
func TestDriveRunVerifyFailLandsNothing(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	before, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}

	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		VerifyCmd: "go build ./...", // fails: brokenBuildTasks references undefined helper()
	}
	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep.Verify.OK {
		t.Fatal("verify.ok=true on a broken build")
	}
	after, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("base ref advanced to %s on verify failure; must stay at %s (land nothing)", after, before)
	}
	// This is the exact bug issue #4 fixes: verify.ok=false must not exit 0.
	if code := runExitCode(rep); code != exitVerifyFailed {
		t.Fatalf("runExitCode=%d, want exitVerifyFailed(%d) on a verify failure", code, exitVerifyFailed)
	}
}

// TestDriveRunRepairFailsHonestly: the same broken build, but the fixer does NOT
// fix it and -repair-max=1. The loop must run exactly once and then report
// verify.ok=false HONESTLY (no false green), repaired=false, with the per-attempt
// record showing the fixer touched a file yet verify still failed.
func TestDriveRunRepairFailsHonestly(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		VerifyCmd: "go build ./...",
		// A fixer that edits a file but does NOT define helper() — the build stays
		// broken, so the loop cannot make verify pass.
		RepairCmd: `printf 'package main\n\nfunc noop() int { return 7 }\n' > repair_noop.go`,
		RepairMax: 1,
	}

	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}

	v := rep.Verify
	if !v.Ran {
		t.Fatal("verify did not run")
	}
	if v.OK {
		t.Fatal("verify.ok=true — must NOT claim green when the repair never fixed the build")
	}
	if v.Repaired {
		t.Fatal("repaired=true — nothing was actually repaired")
	}
	if v.Attempts != 1 {
		t.Fatalf("attempts=%d, want exactly 1 (repair-max=1)", v.Attempts)
	}
	if len(v.Repairs) != 1 {
		t.Fatalf("repairs=%d records, want 1", len(v.Repairs))
	}
	if v.Repairs[0].VerifyOK {
		t.Fatal("repair attempt verifyOk=true, but the build is still broken")
	}
	if !contains(v.Repairs[0].FilesTouched, "repair_noop.go") {
		t.Fatalf("attempt filesTouched=%v, want repair_noop.go", v.Repairs[0].FilesTouched)
	}
	if strings.TrimSpace(v.Output) == "" {
		t.Fatal("empty verify output; the honest failure output must be reported")
	}
}

// TestDriveRunLogDirCapturesFullAgentOutput: with -logdir, an agent's FULL
// stdout+stderr (well beyond the 800-byte in-memory tail) lands in
// agent-<id>.log, while a.Stderr stays bounded exactly as it does without
// -logdir. The agent fails on purpose (exit 1) — the log path must work
// regardless of whether the agent ultimately lands.
func TestDriveRunLogDirCapturesFullAgentOutput(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	logDir := t.TempDir()

	// 3000 bytes of stderr (well past the 800-byte tail) plus a stdout marker
	// (stdout is normally discarded entirely; -logdir must still capture it).
	agentCmd := "printf 'stdout-marker\\n'; head -c 3000 /dev/zero | tr '\\0' 'X' >&2; exit 1"
	task := taskSpec{ID: "logtest", Prompt: "n/a"}

	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agentCmd, LogDir: logDir}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.PerAgent) != 1 || rep.PerAgent[0].OK {
		t.Fatalf("want one failed agent (exit 1), got %+v", rep.PerAgent)
	}
	a := rep.PerAgent[0]

	// In-memory capture stays bounded, same as without -logdir.
	if len(a.Stderr) > 803 { // 800 + "..." prefix
		t.Fatalf("a.Stderr not bounded: len=%d", len(a.Stderr))
	}

	logPath := filepath.Join(logDir, "agent-logtest.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read %s: %v", logPath, err)
	}
	if !strings.Contains(string(data), "stdout-marker") {
		t.Fatalf("%s missing stdout content (stdout is discarded without -logdir, must be captured with it):\n%.200s", logPath, data)
	}
	if got := strings.Count(string(data), "X"); got != 3000 {
		t.Fatalf("%s has %d X's, want the full 3000-byte stderr (not truncated)", logPath, got)
	}

	if rep.LogDir != logDir {
		t.Fatalf("rep.LogDir=%q, want %q", rep.LogDir, logDir)
	}
	var sumBuf bytes.Buffer
	if err := emitReport(&sumBuf, rep, false); err != nil {
		t.Fatalf("emitReport: %v", err)
	}
	if !strings.Contains(sumBuf.String(), "logs: "+logDir) {
		t.Fatalf("summary missing 'logs: %s' line:\n%s", logDir, sumBuf.String())
	}
}

// TestDriveRunLogDirVerifyAndRepairFiles exercises the full verify/repair loop
// with -logdir: an initial FAILING verify (verify-0.log), a repair attempt
// (repair-1.log), and the post-repair verify that passes (verify-1.log) must
// all appear under -logdir with the expected names, alongside each agent's log.
func TestDriveRunLogDirVerifyAndRepairFiles(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	logDir := t.TempDir()

	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		VerifyCmd: "go build ./...",
		RepairCmd: `printf 'package main\n\nfunc helper() int { return helperX() }\n' > repair_fix.go`,
		RepairMax: 2,
		LogDir:    logDir,
	}

	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep.Verify.OK || !rep.Verify.Repaired {
		t.Fatalf("verify ok=%v repaired=%v, want the repair loop to fix it: %q", rep.Verify.OK, rep.Verify.Repaired, rep.Verify.Output)
	}

	// sig-testagent and the repair fixer here are both silent on success (they
	// only write/commit files), so only existence is asserted for their logs;
	// verify's failing output is checked for real content separately below.
	for _, name := range []string{
		"agent-caller.log", "agent-helper.log",
		"verify-0.log", // initial verify: FAILS (undefined: helper)
		"repair-1.log", // the fixer's own log
		"verify-1.log", // post-repair verify: PASSES
	} {
		path := filepath.Join(logDir, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected log file %s: %v", path, err)
		}
	}
	v0, err := os.ReadFile(filepath.Join(logDir, "verify-0.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(v0), "helper") {
		t.Fatalf("verify-0.log missing the build failure output:\n%s", v0)
	}
}

// TestRunRunLogDirUnwritableFailsBeforeAgents: an unwritable/uncreatable
// -logdir must fail the run BEFORE any agent runs — never silently drop the
// requested logs. Here -logdir names a path that is already a regular FILE,
// so os.MkdirAll cannot turn it into a directory; a chmod-based test would be
// unreliable when tests run as root.
func TestRunRunLogDirUnwritableFailsBeforeAgents(t *testing.T) {
	_, repo := makeGoRepo(t)
	marker := filepath.Join(t.TempDir(), "agent-ran")
	agentCmd := "touch " + marker

	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(blocked, "logs") // parent "blocked" is a file, not a dir

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-base", "main",
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", agentCmd,
		"-logdir", logDir,
	})
	if err == nil {
		t.Fatal("unwritable -logdir: want error, got nil")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("no report should be written when -logdir fails before any agent runs, got:\n%s", buf.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("agent ran despite an unwritable -logdir; must fail before any agent runs")
	}
}

// tasksFileFor writes tasks as a JSON -tasks file and returns its path.
func tasksFileFor(t *testing.T, tasks []taskSpec) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(f, []byte(mustJSON(t, tasks)), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestDriveRunRepairSkippedWhenVerifyPasses: when -verify passes on the FIRST
// try, the repair loop must never run even though -repair is configured.
func TestDriveRunRepairSkippedWhenVerifyPasses(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	// A single benign task whose integrated tree compiles fine.
	tasks := []taskSpec{
		{ID: "ok", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"good.go": "package main\n\nfunc good() int { return 1 }\n"},
		})},
	}
	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		VerifyCmd: "go build ./...",
		RepairCmd: `printf 'package main\n\nfunc never() {}\n' > should_not_exist.go`,
		RepairMax: 3,
	}

	rep, err := driveRun(ctx, p, tasks)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	v := rep.Verify
	if !v.Ran || !v.OK {
		t.Fatalf("verify ran=%v ok=%v, want first-try pass", v.Ran, v.OK)
	}
	if v.Attempts != 0 || v.Repaired || len(v.Repairs) != 0 {
		t.Fatalf("repair loop ran on a first-try pass: attempts=%d repaired=%v repairs=%d", v.Attempts, v.Repaired, len(v.Repairs))
	}
	// The fixer must never have executed, so its file is absent from the tree.
	paths, err := g.LsTree(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if contains(paths, "should_not_exist.go") {
		t.Fatal("fixer ran despite verify passing on the first try")
	}
}

// openReadOnly creates an empty file at path and reopens it O_RDONLY, so every
// Write() to the returned handle fails immediately (EBADF) — a real,
// portable, permission-independent way to make an already-successfully-opened
// file fail every write, used below to reproduce the exact -logdir failure
// mode bestEffortWriter guards against.
func openReadOnly(t *testing.T, path string) *os.File {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// TestBestEffortWriterSwallowsErrors is the direct unit test of the seam: a
// real failing writer (a file opened O_RDONLY) must never make
// bestEffortWriter.Write return an error or a short count.
func TestBestEffortWriterSwallowsErrors(t *testing.T) {
	f := openReadOnly(t, filepath.Join(t.TempDir(), "readonly.log"))

	// Confirm the premise: writing to an O_RDONLY file really does fail.
	if _, err := f.Write([]byte("x")); err == nil {
		t.Fatal("premise broken: write to an O_RDONLY file succeeded")
	}

	w := bestEffortWriter{f}
	n, err := w.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("bestEffortWriter.Write returned error %v, want nil (must swallow)", err)
	}
	if n != len("hello") {
		t.Fatalf("bestEffortWriter.Write returned n=%d, want %d", n, len("hello"))
	}
}

// TestBestEffortWriterKeepsCommandRunCleanOnLogWriteFailure reproduces the
// exact os/exec behavior the CRITICAL finding is about: as soon as
// cmd.Stdout is anything other than a bare *os.File, exec services it via a
// pipe + copy-goroutine, and that goroutine's write error is promoted into
// cmd.Run()'s returned error even though the child process itself exited 0.
// The "control" subtest confirms the bug is real without the fix; the
// "fixed" subtest confirms bestEffortWriter neutralizes it while still
// capturing the real output.
func TestBestEffortWriterKeepsCommandRunCleanOnLogWriteFailure(t *testing.T) {
	t.Run("control: unwrapped failing writer fails a clean exit", func(t *testing.T) {
		f := openReadOnly(t, filepath.Join(t.TempDir(), "readonly.log"))
		var buf bytes.Buffer
		cmd := exec.Command("sh", "-c", "echo hello")
		cmd.Stdout = io.MultiWriter(&buf, f)
		if err := cmd.Run(); err == nil {
			t.Fatal("premise broken: expected the log-write failure to surface as a Run() error " +
				"(the os/exec behavior bestEffortWriter works around) — got nil")
		}
	})

	t.Run("fixed: bestEffortWriter keeps a clean exit clean", func(t *testing.T) {
		f := openReadOnly(t, filepath.Join(t.TempDir(), "readonly.log"))
		var buf bytes.Buffer
		cmd := exec.Command("sh", "-c", "echo hello")
		cmd.Stdout = io.MultiWriter(&buf, bestEffortWriter{f})
		if err := cmd.Run(); err != nil {
			t.Fatalf("Run() = %v, want nil — a failing -logdir write must never fail the command", err)
		}
		if got := strings.TrimSpace(buf.String()); got != "hello" {
			t.Fatalf("captured output = %q, want %q (real output must still land)", got, "hello")
		}
	})
}

// TestDriveRunAgentSurvivesLogWriteFailure is the agent-level counterpart:
// with -logdir wired to a log file that fails every write (forced via
// openLogFile, the seam openLog uses), a real agent that lands a commit must
// still be reported OK — not just the bestEffortWriter primitive in
// isolation, but the actual runAgent call site that wraps it.
func TestDriveRunAgentSurvivesLogWriteFailure(t *testing.T) {
	orig := openLogFile
	openLogFile = func(name string, _ int, perm os.FileMode) (*os.File, error) {
		if err := os.WriteFile(name, nil, perm); err != nil {
			return nil, err
		}
		return os.OpenFile(name, os.O_RDONLY, 0) // every Write() to this fails
	}
	t.Cleanup(func() { openLogFile = orig })

	ctx := context.Background()
	_, repo := makeGoRepo(t)
	logDir := t.TempDir()
	agentBin := buildTestAgent(t)

	task := taskSpec{ID: "survives", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"landed.go": "package main\n\nfunc landed() int { return 1 }\n"},
	})}
	// Real output on both streams so the log-write path is actually exercised
	// (not skipped because the child produced nothing to write).
	agentCmd := "printf 'stdout-line\\n'; printf 'stderr-line\\n' >&2; " + agentBin

	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agentCmd, LogDir: logDir}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.PerAgent) != 1 {
		t.Fatalf("want 1 agent result, got %d", len(rep.PerAgent))
	}
	a := rep.PerAgent[0]
	if !a.OK {
		t.Fatalf("agent OK=false (exit=%d stderr=%q); a failing -logdir write must never fail the agent", a.Exit, a.Stderr)
	}
	if a.SHA == "" || a.SHA == rep.BaseSHA {
		t.Fatalf("agent did not land its commit: sha=%q base=%q", a.SHA, rep.BaseSHA)
	}
	if len(a.Files) != 1 || a.Files[0] != "landed.go" {
		t.Fatalf("agent.Files=%v, want [landed.go]", a.Files)
	}
}
