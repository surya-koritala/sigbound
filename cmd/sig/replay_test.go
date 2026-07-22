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
)

// TestReplayReproducesGreenRun: replaying a -manifest from a green run (two
// disjoint agents, -verify passing) re-integrates the recorded SHAs and
// reports REPRODUCED, exit 0 — the whole point of deterministic replay.
func TestReplayReproducesGreenRun(t *testing.T) {
	agent := buildTestAgent(t)
	_, repo := makeGoRepo(t)
	tasks := []taskSpec{
		{ID: "a", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
		})},
		{ID: "b", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"b.go": "package main\n\nfunc b() int { return 2 }\n"},
		})},
	}
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")

	var runBuf bytes.Buffer
	code, err := runRun(&runBuf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", agent,
		"-verify", "go build ./...",
		"-manifest", manifestPath,
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, runBuf.String())
	}
	if code != exitOK {
		t.Fatalf("runRun code=%d, want exitOK\n%s", code, runBuf.String())
	}

	var replayBuf bytes.Buffer
	rcode, rerr := runReplay(&replayBuf, []string{"-manifest", manifestPath})
	if rerr != nil {
		t.Fatalf("runReplay: %v\n%s", rerr, replayBuf.String())
	}
	if rcode != exitReplayReproduced {
		t.Fatalf("code=%d, want exitReplayReproduced\n%s", rcode, replayBuf.String())
	}
	if !strings.Contains(replayBuf.String(), "REPRODUCED") {
		t.Fatalf("output=%q, want it to report REPRODUCED", replayBuf.String())
	}
	if !strings.Contains(replayBuf.String(), "verify (replayed): PASS") {
		t.Fatalf("output=%q, want the replayed -verify to report PASS", replayBuf.String())
	}
}

// TestReplayErrorsWhenAgentSHANoLongerExists: two agents conflict on the same
// line with no -resolver, so ONE lands and the other is flagged — the
// flagged one's commit is reachable ONLY via its own agent/<id> branch (it
// never becomes an ancestor of the landed commit, see cell.fold). Deleting
// that branch and forcing an immediate `git gc` makes the recorded SHA
// genuinely unreachable, simulating a manifest replayed after the branch was
// cleaned up and the commit garbage collected — replay must error clearly
// (exit 2), never crash or silently treat it as reproduced/diverged.
func TestReplayErrorsWhenAgentSHANoLongerExists(t *testing.T) {
	agent := buildTestAgent(t)
	_, repo := makeGoRepo(t)
	tasks := []taskSpec{
		{ID: "a", Prompt: mustJSON(t, map[string]any{
			"edit": map[string]any{"file": "shared.txt", "line": 5, "text": "a-here"},
		})},
		{ID: "b", Prompt: mustJSON(t, map[string]any{
			"edit": map[string]any{"file": "shared.txt", "line": 5, "text": "b-here"},
		})},
	}
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")

	var runBuf bytes.Buffer
	code, err := runRun(&runBuf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", agent,
		"-manifest", manifestPath,
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, runBuf.String())
	}
	if code != exitFlagged {
		t.Fatalf("runRun code=%d, want exitFlagged (one branch should conflict with no -resolver)\n%s", code, runBuf.String())
	}
	var rep runReport
	if err := json.Unmarshal(runBuf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if len(rep.Integrate.Flagged) != 1 {
		t.Fatalf("flagged=%v, want exactly one branch flagged", rep.Integrate.Flagged)
	}
	flaggedBranch := rep.Integrate.Flagged[0].Branch // "agent/a" or "agent/b" — never landed onto main

	// Delete the flagged branch's only ref, expire its reflog, and force an
	// immediate prune — the deterministic way to make an unreachable commit
	// actually vanish from the object store without waiting out git's normal
	// 2-week grace period.
	for _, args := range [][]string{
		{"branch", "-D", flaggedBranch},
		{"reflog", "expire", "--expire=now", "--all"},
		{"gc", "--prune=now"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	var replayBuf bytes.Buffer
	rcode, rerr := runReplay(&replayBuf, []string{"-manifest", manifestPath})
	if rerr == nil {
		t.Fatalf("want an error: the flagged agent's commit was garbage collected, got output:\n%s", replayBuf.String())
	}
	if rcode != exitReplayRepoState {
		t.Fatalf("code=%d, want exitReplayRepoState; error=%v", rcode, rerr)
	}
	if !strings.Contains(rerr.Error(), "no longer resolves") {
		t.Fatalf("error=%q, want it to explain the recorded sha no longer resolves", rerr.Error())
	}
}

// TestReplayDetectsTamperedManifest: editing the recorded integrate.finalSHA
// to point at a different (but still perfectly resolvable) commit — the base
// commit itself, which lacks the agent's change — must make replay report
// DIVERGED, exit 1, naming both tree OIDs. This is the fail-safe half of
// deterministic replay: a manifest that disagrees with what the repo
// actually produced is caught, not rubber-stamped.
func TestReplayDetectsTamperedManifest(t *testing.T) {
	agent := buildTestAgent(t)
	g, repo := makeGoRepo(t)
	tasks := []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}}
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")

	var runBuf bytes.Buffer
	code, err := runRun(&runBuf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", agent,
		"-manifest", manifestPath,
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, runBuf.String())
	}
	if code != exitOK {
		t.Fatalf("runRun code=%d, want exitOK\n%s", code, runBuf.String())
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var rep runReport
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	realFinal := rep.Integrate.FinalSHA
	// Tamper: point the recorded finalSHA at the base commit instead of the
	// real landed one — still a real, resolvable commit, just the WRONG tree.
	rep.Integrate.FinalSHA = rep.BaseSHA
	tampered, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}

	var replayBuf bytes.Buffer
	rcode, rerr := runReplay(&replayBuf, []string{"-manifest", manifestPath})
	if rerr != nil {
		t.Fatalf("runReplay: %v\n%s", rerr, replayBuf.String())
	}
	if rcode != exitReplayDiverged {
		t.Fatalf("code=%d, want exitReplayDiverged\n%s", rcode, replayBuf.String())
	}
	if !strings.Contains(replayBuf.String(), "DIVERGED") {
		t.Fatalf("output=%q, want it to report DIVERGED", replayBuf.String())
	}
	realTree, err := g.TreeOID(context.Background(), realFinal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replayBuf.String(), realTree) {
		t.Fatalf("output=%q, want it to name the recomputed tree %s", replayBuf.String(), realTree)
	}
}

// TestReplayManifestFlagRequired: `sig replay` with no -manifest at all is a
// usage error, exit exitReplayRepoState, before touching git.
func TestReplayManifestFlagRequired(t *testing.T) {
	var buf bytes.Buffer
	code, err := runReplay(&buf, nil)
	if err == nil {
		t.Fatal("want an error: -manifest is required")
	}
	if code != exitReplayRepoState {
		t.Fatalf("code=%d, want exitReplayRepoState", code)
	}
}
