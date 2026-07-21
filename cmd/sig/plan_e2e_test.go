package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	err := runRun(&buf, []string{
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
	err = runRun(&buf, []string{
		"-repo", repo,
		"-goal", "whatever",
		"-planner", "echo not-json",
		"-agent", agent,
		"-json",
	})
	if err == nil {
		t.Fatal("expected an error on a bad plan, got nil")
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
	if err := runRun(&buf, []string{"-repo", repo, "-goal", "x", "-agent", agent}); err == nil {
		t.Error("-goal without -planner: want error, got nil")
	}

	tasksFile := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(tasksFile, []byte(`[{"id":"a","prompt":"x"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := runRun(&buf, []string{"-repo", repo, "-tasks", tasksFile, "-goal", "x", "-planner", "echo", "-agent", agent}); err == nil {
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
