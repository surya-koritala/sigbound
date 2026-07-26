package main

// Metering from the CLI drive path (issue #159): `sig run` records the same
// usage.json `sig serve` does, for the same run. The property under test is
// EQUIVALENCE, not existence — a record with the right shape and the wrong
// numbers is what a "the file is there" test would happily pass.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/surya-koritala/sigbound/cell"
)

// costAgentCmd is one agent command usable from both drive paths: it writes the
// optional SIGBOUND_USAGE_FILE blob FIRST (so the failing task reports its cost
// too — a failed agent still burned tokens), then fails for task "bad" and runs
// the real deterministic agent for everything else.
func costAgentCmd(agent string) string {
	return `printf '{"inputTokens":600,"outputTokens":170,"costUsd":0.375,"vendorThing":"x"}' > "$SIGBOUND_USAGE_FILE"; ` +
		`if [ "$SIGBOUND_TASK_ID" = "bad" ]; then exit 1; fi; ` + agent
}

// costTasks is the shared two-task batch: one that lands a file, one that fails.
func costTasks(t *testing.T) []taskSpec {
	t.Helper()
	return []taskSpec{
		taskWrite(t, "ok", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}),
		{ID: "bad", Prompt: "x"},
	}
}

// measured zeroes the five fields that CANNOT be equal across two runs of the
// same work — wall clocks and report.json's size — so the rest can be compared
// exactly. Every caller asserts the zeroed ones are populated separately;
// without that this would happily accept an all-zero record.
func measured(u UsageJSON) UsageJSON {
	u.TotalWallMs, u.IntegrateWallMs, u.VerifyWallMs, u.AgentWallMs, u.ReportBytes = 0, 0, 0, 0, 0
	return u
}

// cliRunDir resolves repo's durable directory for runID, the same way `sig ack`
// does — proof in itself that a CLI run's record lands where every other reader
// already looks.
func cliRunDir(t *testing.T, repo, runID string) string {
	t.Helper()
	c, err := cell.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := cellRunsDir(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(runs, runID)
}

// cliRunUsage reads the usage record `sig run` left for runID in repo.
func cliRunUsage(t *testing.T, repo, runID string) *UsageJSON {
	t.Helper()
	u, err := readRunUsage(cliRunDir(t, repo, runID))
	if err != nil {
		t.Fatalf("no usage record for `sig run` %s: %v", runID, err)
	}
	return u
}

// TestCLIAndServeUsageRecordsAreEquivalent drives the SAME tasks, agent and
// -verify through both entry points against two identical repos and asserts the
// two usage.json records agree — field for field, against one expected literal
// so neither side can be vacuously "equal" by both being empty. The agent writes
// SIGBOUND_USAGE_FILE, so the ingested cost fields are part of that equality:
// they are 0 on the CLI side unless `sig run` exports the variable at all.
func TestCLIAndServeUsageRecordsAreEquivalent(t *testing.T) {
	requirePOSIXShell(t)
	agent := costAgentCmd(buildTestAgent(t))
	tasks := costTasks(t)

	_, cliRepo := makeGoRepo(t)
	cliRep, code, out := runRunJSON(t, cliRepo, agent, tasks, "-verify", "true")
	if code != exitOK {
		t.Fatalf("`sig run` exit %d, want %d\n%s", code, exitOK, out)
	}
	cliU := cliRunUsage(t, cliRepo, cliRep.RunID)

	_, serveRepo := makeGoRepo(t)
	srv, ts := newTestServer(t, "", serveRepo)
	var created struct {
		RunID string `json:"runId"`
	}
	if c := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell: serveRepo, Base: "main", Tasks: tasks, Agent: agent, Verify: "true",
	}, &created); c != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", c)
	}
	if final := pollRun(t, ts, "", created.RunID); final.Status != "done" {
		t.Fatalf("serve run status %q: %s", final.Status, final.Error)
	}
	serveU, err := readRunUsage(filepath.Join(srv.cells[0].runsDir, created.RunID))
	if err != nil {
		t.Fatalf("no usage record for the serve run: %v", err)
	}

	// One agent landed, one failed; -verify ran once and passed; both agents
	// reported cost. Both paths must say exactly this.
	want := UsageJSON{
		AgentsTotal:    2,
		AgentsOK:       1,
		AgentsFailed:   1,
		VerifyAttempts: 1,
		Landed:         true,
		InputTokens:    1200,
		OutputTokens:   340,
		CostUSD:        0.75,
		CostAgents:     2,
	}
	if got := measured(*cliU); got != want {
		t.Fatalf("`sig run` usage = %+v, want %+v", got, want)
	}
	if got := measured(*serveU); got != want {
		t.Fatalf("`sig serve` usage = %+v, want %+v", got, want)
	}
	if measured(*cliU) != measured(*serveU) {
		t.Fatalf("the two drive paths disagree:\n  sig run:   %+v\n  sig serve: %+v", *cliU, *serveU)
	}

	// The fields measured() zeroed are real on the CLI side too — otherwise the
	// comparison above would pass on a record that measured nothing.
	if cliU.TotalWallMs <= 0 || cliU.AgentWallMs <= 0 || cliU.ReportBytes <= 0 {
		t.Fatalf("`sig run` usage measured nothing: totalWallMs=%d agentWallMs=%d reportBytes=%d",
			cliU.TotalWallMs, cliU.AgentWallMs, cliU.ReportBytes)
	}
	// And the seam's files landed in the run dir, outside every worktree, for
	// the failing agent as much as the successful one.
	for _, id := range []string{"ok", "bad"} {
		if _, err := os.Stat(filepath.Join(cliRunDir(t, cliRepo, cliRep.RunID), agentUsagePrefix+id+".json")); err != nil {
			t.Fatalf("SIGBOUND_USAGE_FILE was not usable by agent %q under `sig run`: %v", id, err)
		}
	}
}

// TestCLIUsageOnRefusedLandingRecordsNotLanded is the error path, and the one
// case where computeUsage's report-field heuristic is actively WRONG: the base
// moves under the run, landRef's compare-and-swap refuses, and driveRun returns
// an error with finalSHA set to the integrated tree and -verify green. The
// heuristic reads that as a landing. Nothing landed, so the record must say so.
//
// The interleaving is forced by program order, not raced: the -verify command
// itself lands the competing commit, and verify runs immediately before the swap
// (same construction as TestDriveRunRefusesToLandOverAnInterveningLanding).
func TestCLIUsageOnRefusedLandingRecordsNotLanded(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	baseSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "intervening.go"), []byte("package main\n\nfunc intervening() int { return 8 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	intervening, err := g.CommitAll(ctx, "landing that arrives while the run is verifying")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateRef(ctx, "refs/heads/main", baseSHA); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tasks := []taskSpec{taskWrite(t, "ok", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"})}
	_, runErr := runRun(&buf, []string{
		"-repo", repo, "-tasks", tasksFileFor(t, tasks), "-agent", agent, "-json",
		"-verify", fmt.Sprintf("git -C %q update-ref refs/heads/main %s", repo, intervening),
	})
	if runErr == nil {
		t.Fatalf("`sig run` succeeded; a run whose base moved under it must refuse to land\n%s", buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("no partial report to recover the run id from: %v\n%s", err, buf.String())
	}
	// Guard against this fixture quietly ceasing to exercise the override: the
	// heuristic must genuinely disagree with the truth here.
	if !(rep.Integrate.FinalSHA != rep.BaseSHA && rep.Verify.OK) {
		t.Fatalf("fixture no longer produces the heuristic's false positive: finalSHA=%s baseSHA=%s verifyOK=%v",
			short(rep.Integrate.FinalSHA), short(rep.BaseSHA), rep.Verify.OK)
	}

	u := cliRunUsage(t, repo, rep.RunID)
	if u.Landed {
		t.Fatal("usage says the run landed; the compare-and-swap refused and the base still carries somebody else's commit")
	}
	if u.AgentsTotal != 1 || u.AgentsOK != 1 {
		t.Fatalf("usage = %+v, want the one agent that really ran recorded", *u)
	}
	if u.TotalWallMs <= 0 {
		t.Fatalf("totalWallMs=%d on a failed run that really took time", u.TotalWallMs)
	}
}

// TestCLIUsageRedVerifyRecordsNotLanded: a red -verify is NOT a driveRun error —
// it returns cleanly having landed nothing — so this run goes down the happy
// path and the heuristic is the thing being trusted. It must still say landed
// false, and it must still record the verify it paid for.
func TestCLIUsageRedVerifyRecordsNotLanded(t *testing.T) {
	requirePOSIXShell(t)
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	tasks := []taskSpec{taskWrite(t, "ok", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"})}
	rep, code, out := runRunJSON(t, repo, agent, tasks, "-verify", "false")
	if code != exitVerifyFailed {
		t.Fatalf("exit %d, want %d (verify failed)\n%s", code, exitVerifyFailed, out)
	}

	u := cliRunUsage(t, repo, rep.RunID)
	if u.Landed {
		t.Fatal("usage says a red-verify run landed")
	}
	if u.AgentsTotal != 1 || u.AgentsOK != 1 || u.VerifyAttempts != 1 {
		t.Fatalf("usage = %+v, want 1 agent ok and 1 verify attempt", *u)
	}
}

// TestCLIUsageTotalWallIncludesPlanning pins the SPAN, not just the number:
// totalWallMs brackets the same thing serve's does, which for a -goal run means
// planning time driveRun itself never sees. The planner sleeps a known second,
// so a bracket that started after the plan cannot reach 1000ms — no timing luck
// involved, only a floor the run provably cannot dip under.
func TestCLIUsageTotalWallIncludesPlanning(t *testing.T) {
	requirePOSIXShell(t)
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	plan := []taskSpec{{ID: "p1", Files: []string{"alpha.go"}, Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"},
	})}}
	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-goal", "add an alpha helper",
		"-planner", "sleep 1; " + planFileCmd(t, mustJSON(t, plan)),
		"-n", "1",
		"-agent", agent,
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("exit %d, want %d\n%s", code, exitOK, buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}

	u := cliRunUsage(t, repo, rep.RunID)
	if u.TotalWallMs < 1000 {
		t.Fatalf("totalWallMs=%d for a run whose planner alone took a second; the wall-clock bracket starts too late to see planning", u.TotalWallMs)
	}
}

// TestCLIUsageParkedRunRecords: a park is a landing plus a held group, not a
// failure — the clean group's ref move is real and the record must show it.
// Reuses the CLI park fixture so this is the same parked run every other
// issue-#137 test acts on.
func TestCLIUsageParkedRunRecords(t *testing.T) {
	f := newCLIParkFixture(t)
	u, err := readRunUsage(f.dir)
	if err != nil {
		t.Fatalf("no usage record for a parked `sig run`: %v", err)
	}
	if !u.Landed {
		t.Fatal("usage says nothing landed, but the fixture already proved the clean group moved the base ref")
	}
	if u.AgentsTotal != 2 || u.AgentsOK != 2 {
		t.Fatalf("usage = %+v, want both agents recorded ok (one landed, one parked)", *u)
	}
	if u.TotalWallMs <= 0 || u.ReportBytes <= 0 {
		t.Fatalf("usage measured nothing: %+v", *u)
	}
}
