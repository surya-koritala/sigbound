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
// atomic rename, and its "always os.Getpid()/ownerScope()" owner) so a test can
// simulate a specific process's phase marker: a foreign/dead pid, a pid scope
// from another host, or the absent scope an older binary wrote.
func plantRunStatus(t *testing.T, dir, status string, pid int, scope string) {
	t.Helper()
	data, err := json.MarshalIndent(runStatusFile{
		Status:    status,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		PID:       pid,
		PIDScope:  scope,
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
	plantRunStatus(t, dir, "running", deadPID, ownerScope())

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
	requireUnixProcessSemantics(t)
	runsDir := t.TempDir()
	runDir := filepath.Join(runsDir, "run1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plantRunStatus(t, runDir, "running", os.Getpid(), ownerScope())

	recoverStaleRuns(runsDir, os.Getpid())

	sf, err := readRunStatus(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if sf.Status != "running" {
		t.Fatalf("status = %q, want running (a live run of THIS process must not be touched)", sf.Status)
	}
}

// TestServeRecoveryProtectsAliveForeignPid checks the OTHER half of
// recoverStaleRuns' condition: a recorded pid that differs from ourPID but
// still resolves to a live process (a sibling `sig serve` daemon sharing the
// same runs dir, for instance) must be left alone. Only a dead recorded pid
// is a prior process's leftover; a live one, ours or not, is still owned by
// somebody and recovery must never stomp it.
func TestServeRecoveryProtectsAliveForeignPid(t *testing.T) {
	requireUnixProcessSemantics(t)
	runsDir := t.TempDir()
	runDir := filepath.Join(runsDir, "run1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// os.Getpid() is definitely alive (it's us), but recoverStaleRuns is told
	// a DIFFERENT pid is "us" -- simulating a sibling daemon's live run.
	plantRunStatus(t, runDir, "running", os.Getpid(), ownerScope())

	recoverStaleRuns(runsDir, os.Getpid()+1)

	sf, err := readRunStatus(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if sf.Status != "running" {
		t.Fatalf("status = %q, want running (recorded pid is alive, so it's not ours to touch)", sf.Status)
	}
}

// TestServeRecoveryFlipsDeadForeignPid is the direct unit-level counterpart
// of TestServeRecoveryProtectsAliveForeignPid: a recorded pid that differs
// from ourPID AND is genuinely dead (spawned, run to completion, and waited
// on, so its pid is guaranteed to belong to no process by the time recovery
// runs) must be recovered to "interrupted".
func TestServeRecoveryFlipsDeadForeignPid(t *testing.T) {
	requireUnixProcessSemantics(t)
	runsDir := t.TempDir()
	runDir := filepath.Join(runsDir, "run1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn+wait short-lived process: %v", err)
	}
	deadPID := cmd.Process.Pid
	plantRunStatus(t, runDir, "running", deadPID, ownerScope())

	recoverStaleRuns(runsDir, os.Getpid())

	sf, err := readRunStatus(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if sf.Status != "interrupted" {
		t.Fatalf("status = %q, want interrupted (recorded pid is dead)", sf.Status)
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
		plantRunStatus(t, runDir, status, 999999999, ownerScope())
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

// TestServeRecoveryReclaimsForeignHostRecord is the fail-safe direction of
// issue #162, and the one a bare pid gets WRONG: the recorded pid is
// os.Getpid(), which is unarguably alive in this namespace, but the record was
// written somewhere else. Under a container per run over a shared clone that is
// the routine case — a stranger's pid number resolves here perfectly well — and
// trusting it wedges the cell in "running" with no process behind it. A record
// this host cannot vouch for must read NOT alive, i.e. reclaimable.
//
// Deliberately NOT gated on unix process semantics: the whole point is that the
// decision is reached before pidAlive is ever consulted, so it must hold on
// Windows too.
func TestServeRecoveryReclaimsForeignHostRecord(t *testing.T) {
	runsDir := t.TempDir()
	runDir := filepath.Join(runsDir, "run1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plantRunStatus(t, runDir, "running", os.Getpid(), "some-other-host-entirely")

	recoverStaleRuns(runsDir, os.Getpid())

	sf, err := readRunStatus(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if sf.Status != "interrupted" {
		t.Fatalf("status = %q, want interrupted: a live LOCAL pid under a FOREIGN pid scope must not read as alive", sf.Status)
	}
	// The note has to say why, or an operator debugging a wedged cell is told
	// "that process is gone" about a process that is very much running.
	if !strings.Contains(sf.Note, "another host or boot") {
		t.Fatalf("note = %q, want it to name the foreign host/boot rather than claiming the pid is gone", sf.Note)
	}
}

// TestServeRecoveryReclaimsUnscopedRecord pins the documented behaviour change
// for status.json files already on disk from a binary that predates pidScope:
// with no identity recorded, the run cannot be shown to belong to a live
// process here, so it is reclaimed rather than trusted. Trusting it would leave
// the guard opt-out-able by deleting one field.
//
// Not gated on unix process semantics, for the same reason as the foreign-host
// case: the decision precedes pidAlive.
func TestServeRecoveryReclaimsUnscopedRecord(t *testing.T) {
	runsDir := t.TempDir()
	runDir := filepath.Join(runsDir, "run1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Exactly what the older binary wrote: a live pid, no pidScope at all.
	plantRunStatus(t, runDir, "running", os.Getpid(), "")
	raw, err := os.ReadFile(filepath.Join(runDir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "pidScope") {
		t.Fatalf("the pre-upgrade fixture must have NO pidScope key at all, got: %s", raw)
	}

	recoverStaleRuns(runsDir, os.Getpid())

	sf, err := readRunStatus(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if sf.Status != "interrupted" {
		t.Fatalf("status = %q, want interrupted: a record with no recorded host identity must not read as alive", sf.Status)
	}
	if !strings.Contains(sf.Note, "no host identity") {
		t.Fatalf("note = %q, want it to say the record carried no host identity", sf.Note)
	}
}

// TestServeRecoveryProtectsOwnHostRecord is the other direction, end to end
// through the REAL writer: writeRunStatus stamps the scope, recoverStaleRuns
// reads it, and a live run of this process must survive the sweep untouched.
// Written this way on purpose — a test that planted ownerScope() by hand would
// still pass if writeRunStatus stopped recording the field at all, and every
// live run on every host would then be reclaimed at the next startup.
func TestServeRecoveryProtectsOwnHostRecord(t *testing.T) {
	requireUnixProcessSemantics(t)
	runsDir := t.TempDir()
	runDir := filepath.Join(runsDir, "run1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunStatus(runDir, "running", "")

	recoverStaleRuns(runsDir, os.Getpid())

	sf, err := readRunStatus(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if sf.Status != "running" {
		t.Fatalf("status = %q, want running: this process wrote it and is still alive", sf.Status)
	}
	if sf.PIDScope == "" {
		t.Fatal("writeRunStatus recorded no pidScope; every record it writes would then be reclaimable")
	}
}

// TestOwnedByLiveProcess pins the decision itself, on a pid that is
// unarguably alive (this test's own), so the only variable left is the
// recorded host identity. The row that matters most is the last: when THIS
// host cannot name itself either, an unscoped record must still not match —
// two empty strings comparing equal is precisely how the false-alive would
// come back.
//
// Unix-gated because pidAlive is unimplemented on Windows and answers false
// for every pid: every row would pass there for a reason that has nothing to
// do with what is being tested. The two fail-safe directions that must hold on
// Windows are covered ungated by the recoverStaleRuns tests above.
func TestOwnedByLiveProcess(t *testing.T) {
	requireUnixProcessSemantics(t)
	live := os.Getpid()
	for _, tc := range []struct {
		name  string
		scope string
		ours  string
		want  bool
	}{
		{"same host, live pid", "host-a", "host-a", true},
		{"another host, live pid", "host-b", "host-a", false},
		{"no recorded identity", "", "host-a", false},
		{"no recorded identity and this host unknown", "", "", false},
	} {
		if got := ownedByLiveProcess(&runStatusFile{PID: live, PIDScope: tc.scope}, tc.ours); got != tc.want {
			t.Errorf("%s: ownedByLiveProcess(scope=%q, ours=%q) = %v, want %v", tc.name, tc.scope, tc.ours, got, tc.want)
		}
	}

	// And a dead pid is dead however well the host matches -- the scope check
	// narrows "alive", it never substitutes for it.
	if ownedByLiveProcess(&runStatusFile{PID: deadPID(t), PIDScope: "host-a"}, "host-a") {
		t.Error("a dead pid under a matching host scope read as alive")
	}
}

// TestOwnerScopeIsStableAndNonEmpty: the scope is compared across processes, so
// a value that varies between two calls in the same process would make a live
// run reclaim itself, and an empty one would make every record reclaimable.
// Neither is a hypothetical -- both are silent, and both look like recovery
// working.
func TestOwnerScopeIsStableAndNonEmpty(t *testing.T) {
	first := ownerScope()
	if first == "" {
		t.Fatal("ownerScope() is empty; every status.json this host writes would then be reclaimable by its own next startup")
	}
	if second := ownerScope(); second != first {
		t.Fatalf("ownerScope() returned %q then %q; it must not vary within a process", first, second)
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
