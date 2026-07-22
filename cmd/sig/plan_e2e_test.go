package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planFileCmd writes planJSON to a temp file and returns a `cat FILE` planner
// command — avoids all shell-quoting of the nested JSON prompts.
func planFileCmd(t *testing.T, planJSON string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(f, []byte(planJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return "cat " + f
}

// TestRunGoalEndToEnd drives the whole goal->plan->agents->integrate->verify path
// through the CLI entry point (runRun). A mock planner emits two DISJOINT tasks
// whose prompts ARE sig-testagent JSON specs; the agents commit, the cell
// integrates both cleanly (two groups), and `go build ./...` verifies the tree.
func TestRunGoalEndToEnd(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	plan := []taskSpec{
		{ID: "p1", Files: []string{"alpha.go"}, Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"},
		})},
		{ID: "p2", Files: []string{"beta.go"}, Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"beta.go": "package main\n\nfunc beta() int { return 2 }\n"},
		})},
	}
	planner := planFileCmd(t, mustJSON(t, plan))

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-goal", "add alpha and beta helpers in their own files",
		"-planner", planner,
		"-n", "2",
		"-agent", agent,
		"-verify", "go build ./...",
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("runRun code=%d, want exitOK on a clean landed+verified run", code)
	}

	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}

	if len(rep.Tasks) != 2 {
		t.Fatalf("planned tasks=%d, want 2", len(rep.Tasks))
	}
	if len(rep.PerAgent) != 2 {
		t.Fatalf("perAgent=%d, want 2", len(rep.PerAgent))
	}
	for _, a := range rep.PerAgent {
		if !a.OK {
			t.Fatalf("agent %s not ok: exit=%d stderr=%q", a.ID, a.Exit, a.Stderr)
		}
	}
	// Disjoint plan -> two independent integration groups, both land, none flagged.
	if rep.Integrate.Groups != 2 {
		t.Fatalf("integrate groups=%d, want 2 (disjoint plan)", rep.Integrate.Groups)
	}
	if len(rep.Integrate.Landed) != 2 {
		t.Fatalf("landed=%d (%v), want 2", len(rep.Integrate.Landed), rep.Integrate.Landed)
	}
	if len(rep.Integrate.Flagged) != 0 {
		t.Fatalf("flagged=%v, want none", rep.Integrate.Flagged)
	}
	if !rep.Verify.Ran || !rep.Verify.OK {
		t.Fatalf("verify ran=%v ok=%v output=%q", rep.Verify.Ran, rep.Verify.OK, rep.Verify.Output)
	}
}

// TestRunGoalLogDirWritesPlannerLog: with -logdir, the planner command's full
// stdout+stderr lands in <logdir>/planner.log, alongside each agent's log —
// the same -goal->plan->agents->verify path as TestRunGoalEndToEnd, plus -logdir.
func TestRunGoalLogDirWritesPlannerLog(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	logDir := t.TempDir()

	plan := []taskSpec{
		{ID: "p1", Files: []string{"alpha.go"}, Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"},
		})},
	}
	// A planner that also writes to stderr, so planner.log has content beyond
	// the plan JSON itself.
	plannerCmd := "echo planner-stderr-marker >&2; " + planFileCmd(t, mustJSON(t, plan))

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-goal", "add alpha helper",
		"-planner", plannerCmd,
		"-n", "1",
		"-agent", agent,
		"-logdir", logDir,
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("runRun code=%d, want exitOK", code)
	}

	plannerLog, err := os.ReadFile(filepath.Join(logDir, "planner.log"))
	if err != nil {
		t.Fatalf("read planner.log: %v", err)
	}
	if !strings.Contains(string(plannerLog), "planner-stderr-marker") {
		t.Fatalf("planner.log missing stderr content:\n%s", plannerLog)
	}
	if !strings.Contains(string(plannerLog), "alpha.go") {
		t.Fatalf("planner.log missing stdout (the plan JSON):\n%s", plannerLog)
	}
	if _, err := os.Stat(filepath.Join(logDir, "agent-p1.log")); err != nil {
		t.Fatalf("expected agent-p1.log alongside planner.log: %v", err)
	}
}

// TestRunGoalBadPlanFailsSafe: a planner that emits garbage makes runRun return
// an error and start NO run — the base ref must not move and no report is written.
func TestRunGoalBadPlanFailsSafe(t *testing.T) {
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	before, err := g.RevParse(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-goal", "whatever",
		"-planner", "echo not-json",
		"-agent", agent,
		"-json",
	})
	if err == nil {
		t.Fatal("expected an error on a bad plan, got nil")
	}
	if code != exitOperationalError {
		t.Fatalf("runRun code=%d, want exitOperationalError on a bad plan", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("no report should be written for a bad plan, got:\n%s", buf.String())
	}
	after, err := g.RevParse(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("main advanced (%s -> %s) despite a bad plan; run must not start", before, after)
	}
}

// TestRunGoalRequiresPlanner: -goal without -planner is an error (can't plan
// without a model), and -tasks + -goal together is an error.
func TestRunGoalFlagValidation(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	var buf bytes.Buffer
	if _, err := runRun(&buf, []string{"-repo", repo, "-goal", "x", "-agent", agent}); err == nil {
		t.Error("-goal without -planner: want error, got nil")
	}

	tasksFile := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(tasksFile, []byte(`[{"id":"a","prompt":"x"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if _, err := runRun(&buf, []string{"-repo", repo, "-tasks", tasksFile, "-goal", "x", "-planner", "echo", "-agent", agent}); err == nil {
		t.Error("-tasks + -goal together: want error, got nil")
	}
}

// TestDriveRunAutocommit: an edit-only agent (writes a file, never runs git)
// still lands its work because the driver auto-commits — and does NOT land when
// auto-commit is disabled.
func TestDriveRunAutocommit(t *testing.T) {
	ctx := context.Background()
	// Edit-only agent: writes a valid Go file in its worktree, never commits.
	editOnly := `printf 'package main\n\nfunc gamma() int { return 3 }\n' > gamma.go`
	tasks := []taskSpec{{ID: "g1", Prompt: "n/a"}}

	// ---- auto-commit ON (default): the driver commits the edit; it lands ----
	_, repo := makeGoRepo(t)
	on := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: editOnly, Autocommit: true, VerifyCmd: "go build ./..."}
	rep, err := driveRun(ctx, on, tasks)
	if err != nil {
		t.Fatalf("driveRun (autocommit on): %v", err)
	}
	a := rep.PerAgent[0]
	if !a.OK {
		t.Fatalf("edit-only agent not ok: exit=%d stderr=%q", a.Exit, a.Stderr)
	}
	if !a.Autocommitted {
		t.Fatal("expected Autocommitted=true for an edit-only agent")
	}
	if !contains(a.Files, "gamma.go") {
		t.Fatalf("files=%v, want gamma.go", a.Files)
	}
	if len(rep.Integrate.Landed) != 1 {
		t.Fatalf("landed=%d, want 1", len(rep.Integrate.Landed))
	}
	if !rep.Verify.OK {
		t.Fatalf("verify failed on auto-committed tree: %q", rep.Verify.Output)
	}

	// ---- auto-commit OFF: the same agent's edit is left uncommitted, so nothing
	// lands and the agent is not OK ----
	_, repo2 := makeGoRepo(t)
	off := runParams{Repo: repo2, Base: "main", Strategy: "overlay", AgentCmd: editOnly, Autocommit: false}
	rep2, err := driveRun(ctx, off, tasks)
	if err != nil {
		t.Fatalf("driveRun (autocommit off): %v", err)
	}
	if rep2.PerAgent[0].OK {
		t.Fatal("with -no-autocommit an edit-only agent must not be OK")
	}
	if rep2.PerAgent[0].Autocommitted {
		t.Fatal("with -no-autocommit Autocommitted must be false")
	}
	if len(rep2.Integrate.Landed) != 0 {
		t.Fatalf("landed=%d, want 0", len(rep2.Integrate.Landed))
	}
}

// alphaBetaPlan returns a two-task, pairwise-disjoint plan (own new file each)
// suitable for a real driveRun through the CLI entry point.
func alphaBetaPlan(t *testing.T) []taskSpec {
	t.Helper()
	return []taskSpec{
		{ID: "p1", Files: []string{"alpha.go"}, Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"},
		})},
		{ID: "p2", Files: []string{"beta.go"}, Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"beta.go": "package main\n\nfunc beta() int { return 2 }\n"},
		})},
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns
// whatever was written to it. Used to observe the -min-tasks warning, which
// (per design) goes straight to os.Stderr rather than through runRun's report
// writer.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestRunGoalMinTasksFailsBeforeAnyAgent: a planner that returns fewer tasks
// than -min-tasks makes runRun fail BEFORE any agent runs (fail-safe, like
// every other plan validation) and names got vs. want. The base ref must not
// move and no report is written.
func TestRunGoalMinTasksFailsBeforeAnyAgent(t *testing.T) {
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	before, err := g.RevParse(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}

	planner := planFileCmd(t, mustJSON(t, alphaBetaPlan(t))) // 2 tasks

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-goal", "add alpha and beta helpers",
		"-planner", planner,
		"-n", "4",
		"-min-tasks", "3",
		"-agent", agent,
	})
	if err == nil {
		t.Fatal("plan has fewer tasks than -min-tasks: want error, got nil")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	if !strings.Contains(err.Error(), "planner produced 2 tasks, -min-tasks 3") {
		t.Fatalf("error should name got vs want (2 vs 3): %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("no report should be written when -min-tasks fails the plan, got:\n%s", buf.String())
	}
	after, err := g.RevParse(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("main advanced (%s -> %s) despite a -min-tasks failure; no agent should run", before, after)
	}
}

// TestRunMinTasksExceedsNFailsAtFlagValidation: -min-tasks > -n is rejected at
// flag-validation time — before the planner command even runs (its command
// here would fail the test if invoked).
func TestRunMinTasksExceedsNFailsAtFlagValidation(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	var buf bytes.Buffer
	_, err := runRun(&buf, []string{
		"-repo", repo,
		"-goal", "x",
		"-planner", "echo 'planner ran: should not happen' >&2; exit 1",
		"-n", "2",
		"-min-tasks", "3",
		"-agent", agent,
	})
	if err == nil {
		t.Fatal("-min-tasks > -n: want error, got nil")
	}
	if !strings.Contains(err.Error(), "-min-tasks 3 exceeds -n 2") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRunGoalFewerThanNWarnsOnStderr: when the planner returns fewer tasks
// than -n and -min-tasks doesn't fail it (0 = no floor, the default), the run
// still proceeds but a warning naming got vs. requested is written to
// stderr — surfacing under-parallelization instead of silently swallowing it.
func TestRunGoalFewerThanNWarnsOnStderr(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	plan := []taskSpec{alphaBetaPlan(t)[0]} // 1 task, but -n 4 requested
	planner := planFileCmd(t, mustJSON(t, plan))

	var buf bytes.Buffer
	var code int
	var runErr error
	stderr := captureStderr(t, func() {
		code, runErr = runRun(&buf, []string{
			"-repo", repo,
			"-goal", "add alpha",
			"-planner", planner,
			"-n", "4",
			"-agent", agent,
		})
	})
	if runErr != nil {
		t.Fatalf("runRun: %v\n%s", runErr, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK (fewer tasks than -n is a warning, not a failure)", code)
	}
	if !strings.Contains(stderr, "1") || !strings.Contains(stderr, "4") {
		t.Fatalf("warning should name got (1) vs requested (4), got stderr=%q", stderr)
	}
}

// TestRunGoalDefaultsToStrictLanes: a planned run (-goal/-planner) with -lanes
// not set explicitly defaults to strict, not warn — the planner already
// promised a pairwise-disjoint split, and strict is the only mode that
// actually preserves it on land.
func TestRunGoalDefaultsToStrictLanes(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	planner := planFileCmd(t, mustJSON(t, alphaBetaPlan(t)))

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-goal", "add alpha and beta helpers",
		"-planner", planner,
		"-n", "2",
		"-agent", agent,
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK", code)
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}
	if rep.LaneMode != laneStrict {
		t.Fatalf("laneMode=%q, want %q (planned runs default to strict)", rep.LaneMode, laneStrict)
	}
}

// TestRunGoalExplicitLanesWarnStaysWarn: an explicit -lanes ALWAYS wins over
// the planned-run strict default.
func TestRunGoalExplicitLanesWarnStaysWarn(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	planner := planFileCmd(t, mustJSON(t, alphaBetaPlan(t)))

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-goal", "add alpha and beta helpers",
		"-planner", planner,
		"-n", "2",
		"-agent", agent,
		"-lanes", "warn",
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK", code)
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}
	if rep.LaneMode != laneWarn {
		t.Fatalf("laneMode=%q, want %q (explicit -lanes must win over the planned-run default)", rep.LaneMode, laneWarn)
	}
}

// TestRunTasksFileDefaultsWarnLanes: a -tasks run (not planned) keeps the
// laneWarn default as today — only -goal runs get the strict default.
func TestRunTasksFileDefaultsWarnLanes(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	tasks := []taskSpec{alphaBetaPlan(t)[0]}
	tasksFile := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(tasksFile, []byte(mustJSON(t, tasks)), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFile,
		"-agent", agent,
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK", code)
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}
	if rep.LaneMode != laneWarn {
		t.Fatalf("laneMode=%q, want %q (-tasks runs keep the warn default)", rep.LaneMode, laneWarn)
	}
}

// TestRunGoalDryRunPreviewsWithoutAgents: -dry-run -goal runs the planner
// (the whole point: see the plan before spending any agent calls) and prints
// the preview, but never spawns the agent. Uses a fake planner script
// emitting a fixed JSON plan, same pattern as TestRunGoalEndToEnd.
func TestRunGoalDryRunPreviewsWithoutAgents(t *testing.T) {
	_, repo := makeGoRepo(t)
	marker := filepath.Join(t.TempDir(), "agent-ran")
	planner := planFileCmd(t, mustJSON(t, alphaBetaPlan(t))) // 2 disjoint tasks

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-goal", "add alpha and beta helpers",
		"-planner", planner,
		"-n", "2",
		"-agent", "touch " + marker,
		"-dry-run",
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK", code)
	}

	var rep dryRunReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}
	if len(rep.Tasks) != 2 {
		t.Fatalf("tasks=%d, want 2 (the planner ran and produced the plan)", len(rep.Tasks))
	}
	if len(rep.Groups) != 2 {
		t.Fatalf("groups=%d, want 2 (disjoint plan)", len(rep.Groups))
	}
	// -goal defaults -lanes to strict even under -dry-run, same as a real run.
	if rep.LaneMode != laneStrict {
		t.Fatalf("laneMode=%q, want %q", rep.LaneMode, laneStrict)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("agent marker exists: -dry-run must never run the agent, even after a real planner run")
	}
}
