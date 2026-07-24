package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// plantRunStatus writes status.json directly (bypassing writeRunStatus'
// atomic rename, and its "always os.Getpid()" pid) so a test can simulate a
// specific process's phase marker, including a foreign/dead pid.
func plantRunStatus(t *testing.T, dir, status string, pid int) {
	t.Helper()
	data, err := json.MarshalIndent(runStatusFile{
		Status:    status,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		PID:       pid,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestServeStatusLifecycleOnDisk drives one run to completion and asserts
// status.json (not just the in-memory record) visibly transitions
// queued/running -> done, and that request.json was journaled at accept time
// with exactly the fields the caller POSTed (issue #90).
func TestServeStatusLifecycleOnDisk(t *testing.T) {
	_, repo := makeGoRepo(t)
	s, ts := newTestServer(t, "", repo)

	req := runRequest{
		Cell:   repo,
		Base:   "main",
		Tasks:  []taskSpec{{ID: "t1", Prompt: "hi"}},
		Agent:  "sleep 1 && " + writeFileAgent("journal.txt"),
		Verify: "true",
	}
	var created struct{ RunID, Status string }
	code := doJSON(t, "POST", ts.URL+"/runs", "", req, &created)
	if code != http.StatusAccepted {
		t.Fatalf("POST status %d, want 202", code)
	}
	if created.Status != "queued" {
		t.Fatalf("accept-time status %q, want queued", created.Status)
	}

	_, dir, ok := s.findRunDir(created.RunID)
	if !ok {
		t.Fatal("run dir not found immediately after accept")
	}

	// status.json exists the instant accept returns (written before the HTTP
	// response, under the same lock as the run-dir creation) -- queued or
	// already running, if the goroutine won the race, but never missing.
	sf, err := readRunStatus(dir)
	if err != nil {
		t.Fatalf("read status.json right after accept: %v", err)
	}
	if sf.Status != "queued" && sf.Status != "running" {
		t.Fatalf("status.json right after accept = %q, want queued or running", sf.Status)
	}
	if sf.PID != os.Getpid() {
		t.Fatalf("status.json pid = %d, want this process's %d", sf.PID, os.Getpid())
	}

	// request.json was journaled at accept, with exactly the posted fields.
	reqData, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		t.Fatalf("read request.json: %v", err)
	}
	var gotReq runRequest
	if err := json.Unmarshal(reqData, &gotReq); err != nil {
		t.Fatalf("unmarshal request.json: %v", err)
	}
	if gotReq.Agent != req.Agent || gotReq.Base != req.Base || len(gotReq.Tasks) != 1 || gotReq.Tasks[0].ID != "t1" {
		t.Fatalf("request.json = %+v, want it to match the POSTed body %+v", gotReq, req)
	}

	// The sleep-1 agent gives us a window to observe "running" land on disk
	// before the run finishes.
	deadline := time.Now().Add(5 * time.Second)
	sawRunning := false
	for time.Now().Before(deadline) {
		if sf, err := readRunStatus(dir); err == nil && sf.Status == "running" {
			sawRunning = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawRunning {
		t.Fatal("status.json never showed \"running\" on disk")
	}

	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "done" {
		t.Fatalf("run status %q, want done", final.Status)
	}
	if sf, err := readRunStatus(dir); err != nil || sf.Status != "done" {
		t.Fatalf("status.json after completion = %+v, err %v, want done", sf, err)
	}
	// The atomic write-then-rename never leaves its scratch file behind.
	if _, err := os.Stat(filepath.Join(dir, ".status.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("status.json write left a stray tmp file: err=%v", err)
	}
}

// TestServeStartupRecoveryMarksDeadRunInterrupted simulates a daemon killed
// mid-run: a run directory whose status.json still says "running" but whose
// recorded pid provably belongs to no process anymore. A fresh server
// instance over the same repo (a real restart) must recover it to
// "interrupted" — on disk, not just in the API response — before it serves
// its first request, and both GET /runs/{id} and the /runs listing must
// report that.
func TestServeStartupRecoveryMarksDeadRunInterrupted(t *testing.T) {
	_, repo := makeGoRepo(t)

	// A known-dead pid: spawn a short-lived process and wait it out, so its
	// pid is guaranteed to no longer belong to any running process (no
	// pid-reuse race within the test's own lifetime).
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn+wait short-lived process: %v", err)
	}
	deadPID := cmd.Process.Pid

	// "First process": open the cell just to resolve its runs dir, then plant
	// a run directory as if that process crashed mid-run.
	s0, err := newServer(context.Background(), serverConfig{repos: []string{repo}, envMode: envModeInherit})
	if err != nil {
		t.Fatal(err)
	}
	runID := "20260101T000000Z-deadbeef"
	dir := filepath.Join(s0.cells[0].runsDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	plantRunStatus(t, dir, "running", deadPID)

	// "Restart": a brand-new server instance over the same repo. newServer
	// runs the recovery scan before returning, i.e. before any request is
	// served.
	s1, err := newServer(context.Background(), serverConfig{repos: []string{repo}, envMode: envModeInherit})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s1.handler())
	t.Cleanup(ts.Close)

	var got runStatusResponse
	if code := doJSON(t, "GET", ts.URL+"/runs/"+runID, "", nil, &got); code != http.StatusOK {
		t.Fatalf("GET status %d", code)
	}
	if got.Status != "interrupted" {
		t.Fatalf("status %q, want interrupted", got.Status)
	}
	if got.Error == "" {
		t.Fatal("interrupted run should carry an explanatory note")
	}
	if got.Report != nil {
		t.Fatalf("interrupted run should have no report, got %+v", got.Report)
	}
	if got.Usage != nil {
		t.Fatalf("interrupted run should have no usage (none was ever written), got %+v", got.Usage)
	}

	var list struct{ Runs []runListEntry }
	if code := doJSON(t, "GET", ts.URL+"/runs", "", nil, &list); code != http.StatusOK {
		t.Fatalf("list status %d", code)
	}
	found := false
	for _, e := range list.Runs {
		if e.ID == runID {
			found = true
			if e.Status != "interrupted" {
				t.Fatalf("listing status %q, want interrupted", e.Status)
			}
		}
	}
	if !found {
		t.Fatalf("run %s missing from listing", runID)
	}

	// The rewrite actually landed on disk, not just in an API-layer
	// interpretation — a SECOND restart must see it as already-terminal.
	sf, err := readRunStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sf.Status != "interrupted" {
		t.Fatalf("status.json on disk = %q, want interrupted", sf.Status)
	}
}

// TestServeRecoveryProtectsLiveRun is the direct unit-level check on
// recoverStaleRuns: a status.json still "running" under THIS process's own
// pid must never be rewritten. This is what distinguishes "a run this
// process is still doing" (skip it) from "a run some now-gone process left
// behind" (recover it) — get this backwards and a startup could stomp on its
// own in-flight run.
func TestServeRecoveryProtectsLiveRun(t *testing.T) {
	runsDir := t.TempDir()
	runDir := filepath.Join(runsDir, "run1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plantRunStatus(t, runDir, "running", os.Getpid())

	recoverStaleRuns(runsDir, os.Getpid())

	sf, err := readRunStatus(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if sf.Status != "running" {
		t.Fatalf("status = %q, want running (a live run of THIS process must not be touched)", sf.Status)
	}
}

// TestServeRecoveryMarksForeignPidEvenIfAlive checks the OTHER half of
// recoverStaleRuns' condition: a recorded pid that differs from ourPID is
// treated as a prior process's leftover regardless of whether that pid
// happens to still resolve to something alive (here, it's os.Getpid() —
// definitely alive — but recoverStaleRuns is told a DIFFERENT pid is "us").
func TestServeRecoveryMarksForeignPidEvenIfAlive(t *testing.T) {
	runsDir := t.TempDir()
	runDir := filepath.Join(runsDir, "run1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plantRunStatus(t, runDir, "running", os.Getpid())

	recoverStaleRuns(runsDir, os.Getpid()+1)

	sf, err := readRunStatus(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if sf.Status != "interrupted" {
		t.Fatalf("status = %q, want interrupted (recorded pid != our own pid)", sf.Status)
	}
}

// TestServeRecoveryLeavesTerminalRunsAlone guards against the recovery scan
// over-reaching: a done/error run under an obviously foreign pid is already
// finished and must be left exactly as it is.
func TestServeRecoveryLeavesTerminalRunsAlone(t *testing.T) {
	runsDir := t.TempDir()
	for _, status := range []string{"done", "error"} {
		runDir := filepath.Join(runsDir, status)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		plantRunStatus(t, runDir, status, 999999999)
	}
	recoverStaleRuns(runsDir, os.Getpid())
	for _, status := range []string{"done", "error"} {
		sf, err := readRunStatus(filepath.Join(runsDir, status))
		if err != nil {
			t.Fatal(err)
		}
		if sf.Status != status {
			t.Fatalf("terminal run %q became %q, want unchanged", status, sf.Status)
		}
	}
}

// TestServeRequestJournalCarriesNoEnvValues: request.json is the exact
// POSTed body — it must never carry the server's OWN env policy (auth token,
// -env-* allowlisted secret values). Nothing in the runRequest schema
// currently has a slot for one (env values live only in the daemon's own
// process env plus its -env-* flags, never in a request), so this guards the
// invariant against a future field accidentally reintroducing one, not just
// today's shape.
func TestServeRequestJournalCarriesNoEnvValues(t *testing.T) {
	_, repo := makeGoRepo(t)
	const secretToken = "super-secret-serve-token-value"
	const secretEnvVal = "sk-totally-secret-agent-key"
	t.Setenv("SIGBOUND_TEST_SECRET_XYZ", secretEnvVal)

	s, err := newServer(context.Background(), serverConfig{
		repos:    []string{repo},
		token:    secretToken,
		envMode:  envModeScoped,
		envAgent: []string{"SIGBOUND_TEST_SECRET_XYZ"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)

	var created struct{ RunID string }
	code := doJSON(t, "POST", ts.URL+"/runs", secretToken, runRequest{
		Cell:  repo,
		Tasks: []taskSpec{{ID: "t1"}},
		Agent: writeFileAgent("j.txt"),
	}, &created)
	if code != http.StatusAccepted {
		t.Fatalf("POST status %d, want 202", code)
	}
	pollRun(t, ts, secretToken, created.RunID)

	_, dir, ok := s.findRunDir(created.RunID)
	if !ok {
		t.Fatal("run dir not found")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		t.Fatalf("read request.json: %v", err)
	}
	for _, secret := range []string{secretToken, secretEnvVal} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("request.json leaked a server secret %q: %s", secret, raw)
		}
	}
}
