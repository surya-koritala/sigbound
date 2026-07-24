package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestServer builds a server over repos (no signal loop, background ctx) and
// wraps its handler in an httptest.Server the tests drive over real HTTP.
func newTestServer(t *testing.T, token string, repos ...string) (*server, *httptest.Server) {
	t.Helper()
	s, err := newServer(context.Background(), serverConfig{repos: repos, token: token, envMode: envModeInherit})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// doJSON issues one request with an optional bearer token and decodes the JSON
// body into out (when non-nil), returning the status code.
func doJSON(t *testing.T, method, url, token string, body any, out any) int {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s %s: %v", method, url, err)
		}
	}
	return resp.StatusCode
}

// pollRun polls GET /runs/{id} until the status is terminal (done|error) or the
// deadline passes, returning the final response.
func pollRun(t *testing.T, ts *httptest.Server, token, id string) runStatusResponse {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var resp runStatusResponse
		code := doJSON(t, "GET", ts.URL+"/runs/"+id, token, nil, &resp)
		if code != http.StatusOK {
			t.Fatalf("GET /runs/%s: status %d", id, code)
		}
		if resp.Status == "done" || resp.Status == "error" {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish (last status %q)", id, resp.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeFileAgent is a "printf agent": it writes one file in its worktree and
// lets the driver autocommit it. No build, no git identity needed.
func writeFileAgent(name string) string {
	return "printf 'hello\\n' > " + name
}

func TestServeHealth(t *testing.T) {
	_, repo := makeGoRepo(t)
	s, ts := newTestServer(t, "", repo)

	var resp struct {
		OK      bool `json:"ok"`
		Version string
		Cells   []struct{ ID, Repo string }
	}
	code := doJSON(t, "GET", ts.URL+"/health", "", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("health status %d", code)
	}
	if !resp.OK || resp.Version != Version {
		t.Fatalf("health = %+v, want ok + version %s", resp, Version)
	}
	if len(resp.Cells) != 1 || resp.Cells[0].ID != s.cells[0].cell.ID() || resp.Cells[0].Repo != repo {
		t.Fatalf("health cells = %+v, want [{%s %s}]", resp.Cells, s.cells[0].cell.ID(), repo)
	}
}

func TestServeAuthMatrix(t *testing.T) {
	_, repo := makeGoRepo(t)

	// Token set: every request needs a matching bearer token.
	const tok = "s3cr3t-token-value"
	_, ts := newTestServer(t, tok, repo)
	cases := []struct {
		name string
		send string // token to send ("" = no header)
		want int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong same length", "s3cr3t-token-valuX", http.StatusUnauthorized}, // same len -> reaches ConstantTimeCompare
		{"wrong diff length", "nope", http.StatusUnauthorized},
		{"correct", tok, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// health's 200 body doesn't decode into map[string]string (it has
			// non-string fields), so only the 401 cases decode a body.
			var body map[string]string
			var out any
			if c.want == http.StatusUnauthorized {
				out = &body
			}
			code := doJSON(t, "GET", ts.URL+"/health", c.send, nil, out)
			if code != c.want {
				t.Fatalf("auth %q: status %d, want %d", c.name, code, c.want)
			}
			if c.want == http.StatusUnauthorized && body["code"] != codeUnauthorized {
				t.Fatalf("auth %q: code %q, want %q", c.name, body["code"], codeUnauthorized)
			}
		})
	}

	// Token unset (dev mode on loopback): auth is off, any request passes.
	_, ts2 := newTestServer(t, "", repo)
	if code := doJSON(t, "GET", ts2.URL+"/health", "", nil, nil); code != http.StatusOK {
		t.Fatalf("dev-mode health without token: status %d, want 200", code)
	}
}

func TestServeStartupCheck(t *testing.T) {
	loop := []string{"127.0.0.1:7777", "localhost:7777", "[::1]:7777"}
	for _, a := range loop {
		if ok, err := addrIsLoopback(a); err != nil || !ok {
			t.Fatalf("addrIsLoopback(%q) = %v,%v; want true,nil", a, ok, err)
		}
	}
	pub := []string{"0.0.0.0:7777", ":7777", "192.168.1.5:7777"}
	for _, a := range pub {
		if ok, err := addrIsLoopback(a); err != nil || ok {
			t.Fatalf("addrIsLoopback(%q) = %v,%v; want false,nil", a, ok, err)
		}
	}

	// Loopback binds are fine with or without a token / -allow-remote.
	if err := serveStartupCheck("127.0.0.1:7777", false, false, "T"); err != nil {
		t.Fatalf("loopback startup check: %v", err)
	}
	// Non-loopback without -allow-remote is refused.
	if err := serveStartupCheck("0.0.0.0:7777", false, false, "T"); err == nil {
		t.Fatal("non-loopback without -allow-remote should be refused")
	}
	// Non-loopback with -allow-remote but no token is refused.
	if err := serveStartupCheck("0.0.0.0:7777", true, false, "T"); err == nil {
		t.Fatal("non-loopback without a token should be refused")
	}
	// Non-loopback with both is allowed.
	if err := serveStartupCheck("0.0.0.0:7777", true, true, "T"); err != nil {
		t.Fatalf("non-loopback with -allow-remote + token: %v", err)
	}
}

func TestServeCreateRunHappyPath(t *testing.T) {
	g, repo := makeGoRepo(t)
	ctx := context.Background()
	baseBefore, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	s, ts := newTestServer(t, "", repo)

	var created struct{ RunID, Cell, Status string }
	code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:   repo,
		Base:   "main",
		Tasks:  []taskSpec{{ID: "t1", Prompt: "ignored"}},
		Agent:  writeFileAgent("served.txt"),
		Verify: "true",
	}, &created)
	if code != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", code)
	}
	if created.RunID == "" || created.Status != "queued" {
		t.Fatalf("create resp = %+v", created)
	}

	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "done" {
		t.Fatalf("run status %q (error=%q), want done", final.Status, final.Error)
	}
	if final.Report == nil {
		t.Fatal("done run has no report")
	}
	if !final.Report.Verify.Ran || !final.Report.Verify.OK {
		t.Fatalf("verify ran=%v ok=%v", final.Report.Verify.Ran, final.Report.Verify.OK)
	}

	// Base ref advanced to the integrated commit.
	baseAfter, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if baseAfter == baseBefore {
		t.Fatal("base ref did not advance")
	}
	if final.Report.Integrate.FinalSHA != baseAfter {
		t.Fatalf("report finalSHA %s != main %s", final.Report.Integrate.FinalSHA, baseAfter)
	}

	// The report the API returns matches the durable report.json on disk.
	_, dir, ok := s.findRunDir(created.RunID)
	if !ok {
		t.Fatal("run dir not found on disk")
	}
	disk, err := readRunReport(dir)
	if err != nil {
		t.Fatalf("read report.json: %v", err)
	}
	if disk.Integrate.FinalSHA != final.Report.Integrate.FinalSHA {
		t.Fatalf("disk finalSHA %s != api %s", disk.Integrate.FinalSHA, final.Report.Integrate.FinalSHA)
	}
	if disk.BaseSHA != baseBefore {
		t.Fatalf("disk baseSHA %s != base before %s", disk.BaseSHA, baseBefore)
	}
}

func TestServeConcurrentSameCell409(t *testing.T) {
	_, repo := makeGoRepo(t)
	_, ts := newTestServer(t, "", repo)

	// First run's agent sleeps, so it holds the cell's slot while we race a
	// second request in.
	var first struct{ RunID string }
	code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Tasks: []taskSpec{{ID: "t1"}},
		Agent: "sleep 1 && " + writeFileAgent("slow.txt"),
	}, &first)
	if code != http.StatusAccepted {
		t.Fatalf("first POST status %d, want 202", code)
	}

	var second map[string]string
	code = doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Tasks: []taskSpec{{ID: "t2"}},
		Agent: writeFileAgent("other.txt"),
	}, &second)
	if code != http.StatusConflict {
		t.Fatalf("concurrent same-cell POST status %d, want 409 (body %v)", code, second)
	}
	if !strings.Contains(second["error"], "already in progress") {
		t.Fatalf("409 body = %v, want an 'already in progress' message", second)
	}
	if second["code"] != codeCellBusy {
		t.Fatalf("409 code %q, want %q", second["code"], codeCellBusy)
	}

	// Let the first run finish; the slot frees and a new run is accepted.
	pollRun(t, ts, "", first.RunID)
	var third struct{ RunID string }
	code = doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Tasks: []taskSpec{{ID: "t3"}},
		Agent: writeFileAgent("after.txt"),
	}, &third)
	if code != http.StatusAccepted {
		t.Fatalf("post-completion POST status %d, want 202", code)
	}
	// Drain it to a terminal state before returning: otherwise this run
	// outlives the test, and its goroutine can still be writing report.json
	// into the makeGoRepo temp dir after t.TempDir()'s cleanup removes it —
	// a stderr write (see writeRunReport) racing the test's own teardown.
	pollRun(t, ts, "", third.RunID)
}

func TestServeTwoCellsRunConcurrently(t *testing.T) {
	gA, repoA := makeGoRepo(t)
	gB, repoB := makeGoRepo(t)
	ctx := context.Background()
	beforeA, _ := gA.RevParse(ctx, "main")
	beforeB, _ := gB.RevParse(ctx, "main")

	_, ts := newTestServer(t, "", repoA, repoB)

	// Start a slow run on A, then — while A is still busy — start one on B.
	// B must NOT 409 (different cell), proving cells run in parallel.
	var runA struct{ RunID string }
	if code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repoA,
		Tasks: []taskSpec{{ID: "a1"}},
		Agent: "sleep 1 && " + writeFileAgent("a.txt"),
	}, &runA); code != http.StatusAccepted {
		t.Fatalf("run A status %d", code)
	}
	var runB struct{ RunID string }
	if code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repoB,
		Tasks: []taskSpec{{ID: "b1"}},
		Agent: writeFileAgent("b.txt"),
	}, &runB); code != http.StatusAccepted {
		t.Fatalf("run B while A busy: status %d, want 202 (cross-cell concurrency)", code)
	}

	finA := pollRun(t, ts, "", runA.RunID)
	finB := pollRun(t, ts, "", runB.RunID)
	if finA.Status != "done" || finB.Status != "done" {
		t.Fatalf("A=%q B=%q, want both done", finA.Status, finB.Status)
	}
	afterA, _ := gA.RevParse(ctx, "main")
	afterB, _ := gB.RevParse(ctx, "main")
	if afterA == beforeA || afterB == beforeB {
		t.Fatalf("both cells should land: A %s->%s B %s->%s", beforeA, afterA, beforeB, afterB)
	}
}

func TestServeEventsNDJSON(t *testing.T) {
	_, repo := makeGoRepo(t)
	_, ts := newTestServer(t, "", repo)

	var created struct{ RunID string }
	doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:   repo,
		Tasks:  []taskSpec{{ID: "t1"}},
		Agent:  writeFileAgent("ev.txt"),
		Verify: "true",
	}, &created)
	pollRun(t, ts, "", created.RunID)

	req, _ := http.NewRequest("GET", ts.URL+"/runs/"+created.RunID+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("events content-type %q, want application/x-ndjson", ct)
	}
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("event line not valid JSON: %q: %v", line, err)
		}
		if name, ok := rec["event"].(string); ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 || names[0] != "run_start" || names[len(names)-1] != "run_done" {
		t.Fatalf("events = %v, want run_start ... run_done", names)
	}
}

func TestServeBadRequests(t *testing.T) {
	_, repo := makeGoRepo(t)
	_, ts := newTestServer(t, "", repo)

	cases := []struct {
		name     string
		body     any
		want     string // substring the error must contain
		wantCode string
	}{
		{"unknown cell", runRequest{Cell: "/no/such/repo", Tasks: []taskSpec{{ID: "t1"}}, Agent: "true"}, "unknown cell", codeCellNotFound},
		{"tasks and goal", runRequest{Cell: repo, Tasks: []taskSpec{{ID: "t1"}}, Goal: "do it", Agent: "true"}, "mutually exclusive", codeBadRequest},
		{"neither tasks nor goal", runRequest{Cell: repo, Agent: "true"}, "one of tasks or goal", codeBadRequest},
		{"missing agent", runRequest{Cell: repo, Tasks: []taskSpec{{ID: "t1"}}}, "agent is required", codeBadRequest},
		{"empty task id", runRequest{Cell: repo, Tasks: []taskSpec{{ID: ""}}, Agent: "true"}, "empty id", codeBadRequest},
		{"verifyImpact without verify", runRequest{Cell: repo, Tasks: []taskSpec{{ID: "t1"}}, Agent: "true", VerifyImpact: "go test ./..."}, "verifyImpact requires verify", codeBadRequest},
		{"bad duration", runRequest{Cell: repo, Tasks: []taskSpec{{ID: "t1"}}, Agent: "true", Budget: "notaduration"}, "budget", codeBadRequest},
		{"goal without planner", runRequest{Cell: repo, Goal: "split it", Agent: "true"}, "planner is required", codeBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var body map[string]string
			code := doJSON(t, "POST", ts.URL+"/runs", "", c.body, &body)
			if code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 (body %v)", code, body)
			}
			if !strings.Contains(body["error"], c.want) {
				t.Fatalf("error %q, want it to contain %q", body["error"], c.want)
			}
			if body["code"] != c.wantCode {
				t.Fatalf("code %q, want %q", body["code"], c.wantCode)
			}
		})
	}

	// Malformed JSON is a 400 too.
	req, _ := http.NewRequest("POST", ts.URL+"/runs", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON status %d, want 400", resp.StatusCode)
	}
	var malformedBody map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&malformedBody); err != nil {
		t.Fatal(err)
	}
	if malformedBody["code"] != codeBadRequest {
		t.Fatalf("malformed JSON code %q, want %q", malformedBody["code"], codeBadRequest)
	}

	// A non-JSON Content-Type is 415, before the body is even parsed.
	req2, _ := http.NewRequest("POST", ts.URL+"/runs", strings.NewReader("{}"))
	req2.Header.Set("Content-Type", "text/plain")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong Content-Type status %d, want 415", resp2.StatusCode)
	}
	var mediaBody map[string]string
	if err := json.NewDecoder(resp2.Body).Decode(&mediaBody); err != nil {
		t.Fatal(err)
	}
	if mediaBody["code"] != codeUnsupportedMediaType {
		t.Fatalf("wrong Content-Type code %q, want %q", mediaBody["code"], codeUnsupportedMediaType)
	}

	// An unknown run id is 404.
	var unknownRunBody map[string]string
	if code := doJSON(t, "GET", ts.URL+"/runs/does-not-exist", "", nil, &unknownRunBody); code != http.StatusNotFound {
		t.Fatalf("unknown run status %d, want 404", code)
	}
	if unknownRunBody["code"] != codeRunNotFound {
		t.Fatalf("unknown run code %q, want %q", unknownRunBody["code"], codeRunNotFound)
	}
}

func TestServeHistorySurvivesRestart(t *testing.T) {
	g, repo := makeGoRepo(t)
	ctx := context.Background()

	// First "process": run one job to completion.
	_, ts1 := newTestServer(t, "", repo)
	var created struct{ RunID string }
	doJSON(t, "POST", ts1.URL+"/runs", "", runRequest{
		Cell:   repo,
		Tasks:  []taskSpec{{ID: "t1"}},
		Agent:  writeFileAgent("hist.txt"),
		Verify: "true",
	}, &created)
	final := pollRun(t, ts1, "", created.RunID)
	if final.Status != "done" {
		t.Fatalf("first-process run status %q", final.Status)
	}
	landed, _ := g.RevParse(ctx, "main")
	ts1.Close()

	// Second "process": a fresh server over the same repo, no in-memory state.
	// It must see the prior run from disk alone.
	_, ts2 := newTestServer(t, "", repo)

	var list struct {
		Runs []runListEntry
	}
	if code := doJSON(t, "GET", ts2.URL+"/runs", "", nil, &list); code != http.StatusOK {
		t.Fatalf("list status %d", code)
	}
	var found *runListEntry
	for i := range list.Runs {
		if list.Runs[i].ID == created.RunID {
			found = &list.Runs[i]
		}
	}
	if found == nil {
		t.Fatalf("run %s not in list after restart: %+v", created.RunID, list.Runs)
	}
	if found.Status != "done" || found.FinalSHA != landed {
		t.Fatalf("restart list entry = %+v, want done + finalSHA %s", *found, landed)
	}

	// And GET /runs/{id} rehydrates the full report from disk.
	var got runStatusResponse
	if code := doJSON(t, "GET", ts2.URL+"/runs/"+created.RunID, "", nil, &got); code != http.StatusOK {
		t.Fatalf("get after restart status %d", code)
	}
	if got.Status != "done" || got.Report == nil || got.Report.Integrate.FinalSHA != landed {
		t.Fatalf("get after restart = %+v (report nil? %v)", got, got.Report == nil)
	}
}

// TestServeVerifyGateHolds proves serve adds no new landing path: a run whose
// -verify fails lands nothing (base ref unchanged), status done, report shows
// the red verify.
func TestServeVerifyGateHolds(t *testing.T) {
	g, repo := makeGoRepo(t)
	ctx := context.Background()
	before, _ := g.RevParse(ctx, "main")
	_, ts := newTestServer(t, "", repo)

	var created struct{ RunID string }
	doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:   repo,
		Tasks:  []taskSpec{{ID: "t1"}},
		Agent:  writeFileAgent("nope.txt"),
		Verify: "false", // always fails
	}, &created)
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "done" {
		t.Fatalf("status %q, want done (a red verify is a completed run, not an error)", final.Status)
	}
	if final.Report.Verify.OK {
		t.Fatal("verify should have failed")
	}
	after, _ := g.RevParse(ctx, "main")
	if after != before {
		t.Fatalf("base ref advanced despite failing verify: %s -> %s", before, after)
	}
}

func TestServeStartupErrors(t *testing.T) {
	_, repo := makeGoRepo(t)
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"missing repos", []string{}, "-repos is required"},
		{"bad repo", []string{"-repos", "/no/such/repo/here"}, "open cell"},
		{"non-loopback without allow-remote", []string{"-repos", repo, "-addr", "0.0.0.0:0"}, "without -allow-remote"},
		{"non-loopback without token", []string{"-repos", repo, "-addr", "0.0.0.0:0", "-allow-remote"}, "without an auth token"},
		{"bad env-mode", []string{"-repos", repo, "-env-mode", "bogus"}, "unknown -env-mode"},
		{"bare star env allow", []string{"-repos", repo, "-env-agent", "*"}, "bare '*'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, err := runServe(io.Discard, c.argv)
			if err == nil {
				t.Fatalf("expected an error, got code %d nil", code)
			}
			if code != exitOperationalError {
				t.Fatalf("code %d, want exitOperationalError", code)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q, want it to contain %q", err, c.want)
			}
		})
	}
}

// TestServeRealListenerGracefulShutdown drives the actual net.Listener +
// serve() shutdown path (not httptest): it starts a slow run, cancels the base
// context the way a signal would, and asserts serve returns cleanly AND the
// in-flight run was drained to a terminal on-disk marker.
func TestServeRealListenerGracefulShutdown(t *testing.T) {
	_, repo := makeGoRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	s, err := newServer(ctx, serverConfig{repos: []string{repo}, envMode: envModeInherit})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := s.serve(io.Discard, ln, func() {})
		done <- code
	}()

	base := "http://" + ln.Addr().String()
	var created struct{ RunID string }
	if code := doJSON(t, "POST", base+"/runs", "", runRequest{
		Cell:  repo,
		Tasks: []taskSpec{{ID: "t1"}},
		Agent: "sleep 5 && " + writeFileAgent("slow.txt"),
	}, &created); code != http.StatusAccepted {
		t.Fatalf("POST status %d", code)
	}

	// Simulate SIGINT: cancel the base context. serve must drain and return.
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("serve returned code %d, want exitOK", code)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("serve did not shut down")
	}

	// The interrupted run left a terminal marker on disk (final report written).
	_, dir, ok := s.findRunDir(created.RunID)
	if !ok {
		t.Fatal("run dir missing after shutdown")
	}
	if st := diskStatus(dir); st == "" {
		t.Fatal("interrupted run wrote no terminal report/error marker")
	}
}

// TestServeScopedEnvIsRealDefault is the regression guard for issue #60: a
// server built with the REAL scoped default (not newTestServer's hardcoded
// envMode: inherit) must (a) reject a request that tries to widen envMode to
// inherit, and (b) never let a normal scoped run's agent see a daemon-only
// secret env var.
func TestServeScopedEnvIsRealDefault(t *testing.T) {
	_, repo := makeGoRepo(t)

	const secretName = "SIGBOUND_TEST_DAEMON_SECRET"
	t.Setenv(secretName, "daemon-secret-value")

	dumpFile := filepath.Join(t.TempDir(), "env.txt")
	agent := "env > " + dumpFile + " && " + writeFileAgent("served.txt")

	s, err := newServer(context.Background(), serverConfig{repos: []string{repo}, envMode: envModeScoped})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)

	// (a) a request cannot widen the server's scoped default to inherit.
	var body map[string]string
	code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:    repo,
		Tasks:   []taskSpec{{ID: "widen"}},
		Agent:   agent,
		EnvMode: envModeInherit,
	}, &body)
	if code != http.StatusBadRequest {
		t.Fatalf("widen envMode: status %d, want 400 (body %v)", code, body)
	}
	if !strings.Contains(body["error"], "cannot widen") {
		t.Fatalf("widen envMode error %q, want it to mention widening", body["error"])
	}
	if body["code"] != codeEnvWidenRefused {
		t.Fatalf("widen envMode code %q, want %q", body["code"], codeEnvWidenRefused)
	}

	// (b) a normal scoped run's agent does not see the daemon's secret.
	var created struct{ RunID string }
	code = doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Tasks: []taskSpec{{ID: "scoped"}},
		Agent: agent,
	}, &created)
	if code != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", code)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "done" {
		t.Fatalf("run status %q (error=%q), want done", final.Status, final.Error)
	}
	data, err := os.ReadFile(dumpFile)
	if err != nil {
		t.Fatalf("read agent env dump: %v", err)
	}
	if strings.Contains(string(data), secretName) {
		t.Fatalf("scoped agent's env leaked the daemon secret:\n%s", data)
	}
}

// ---- Quotas and metering (issue #61) ----

// perTaskAgent is an agent command that writes one file named after its own
// task id ($SIGBOUND_TASK_ID.txt), so N tasks in one run never collide on
// the same path the way a shared writeFileAgent(name) call would.
const perTaskAgent = `printf 'hi\n' > "$SIGBOUND_TASK_ID.txt"`

func manyTasks(n int) []taskSpec {
	tasks := make([]taskSpec, n)
	for i := range tasks {
		tasks[i] = taskSpec{ID: fmt.Sprintf("t%d", i)}
	}
	return tasks
}

// TestServeMaxAgentsPerRunRejects: a request whose agent count exceeds
// -max-agents-per-run is rejected 400 before any run starts (no run dir
// created); a request within the cap still succeeds normally.
func TestServeMaxAgentsPerRunRejects(t *testing.T) {
	_, repo := makeGoRepo(t)
	s, err := newServer(context.Background(), serverConfig{repos: []string{repo}, envMode: envModeInherit, maxAgentsPerRun: 1})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)

	var body map[string]string
	code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Tasks: manyTasks(2),
		Agent: perTaskAgent,
	}, &body)
	if code != http.StatusBadRequest {
		t.Fatalf("over-cap POST status %d, want 400 (body %v)", code, body)
	}
	if !strings.Contains(body["error"], "max-agents-per-run") {
		t.Fatalf("error %q, want it to name max-agents-per-run", body["error"])
	}
	if body["code"] != codeQuotaAgents {
		t.Fatalf("over-cap code %q, want %q", body["code"], codeQuotaAgents)
	}
	// No run was started: the cell's runs dir has no entries at all.
	names, _ := os.ReadDir(s.cells[0].runsDir)
	if len(names) != 0 {
		t.Fatalf("run dir(s) created despite the quota rejection: %v", names)
	}

	// A request AT the cap still succeeds normally.
	var created struct{ RunID string }
	code = doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Tasks: manyTasks(1),
		Agent: perTaskAgent,
	}, &created)
	if code != http.StatusAccepted {
		t.Fatalf("at-cap POST status %d, want 202", code)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "done" {
		t.Fatalf("at-cap run status %q, want done", final.Status)
	}
}

// TestServeMaxConcurrentRuns429 proves -max-concurrent-runs is a GLOBAL
// ceiling across ALL cells, distinct from the existing per-cell 409: cell B
// is completely idle, yet a POST to it is rejected 429 while the global cap
// (met by cell A's in-flight run) is exhausted. Once A finishes, the slot
// frees and a new run — even on a different cell — is accepted again.
func TestServeMaxConcurrentRuns429(t *testing.T) {
	_, repoA := makeGoRepo(t)
	_, repoB := makeGoRepo(t)
	s, err := newServer(context.Background(), serverConfig{repos: []string{repoA, repoB}, envMode: envModeInherit, maxConcurrentRuns: 1})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)

	var runA struct{ RunID string }
	code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repoA,
		Tasks: []taskSpec{{ID: "a1"}},
		Agent: "sleep 1 && " + writeFileAgent("a.txt"),
	}, &runA)
	if code != http.StatusAccepted {
		t.Fatalf("run A status %d, want 202", code)
	}

	var body map[string]string
	code = doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repoB,
		Tasks: []taskSpec{{ID: "b1"}},
		Agent: writeFileAgent("b.txt"),
	}, &body)
	if code != http.StatusTooManyRequests {
		t.Fatalf("cell-B POST while A in flight: status %d, want 429 (body %v)", code, body)
	}
	if !strings.Contains(body["error"], "max-concurrent-runs") {
		t.Fatalf("error %q, want it to name max-concurrent-runs", body["error"])
	}
	if body["code"] != codeQuotaConcurrency {
		t.Fatalf("429 code %q, want %q", body["code"], codeQuotaConcurrency)
	}
	// The rejected request never started a run on cell B either.
	bCell := s.byKey[repoB]
	names, _ := os.ReadDir(bCell.runsDir)
	if len(names) != 0 {
		t.Fatalf("cell B run dir(s) created despite the 429: %v", names)
	}

	pollRun(t, ts, "", runA.RunID)

	var runB struct{ RunID string }
	code = doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repoB,
		Tasks: []taskSpec{{ID: "b2"}},
		Agent: writeFileAgent("b2.txt"),
	}, &runB)
	if code != http.StatusAccepted {
		t.Fatalf("post-completion cell-B POST status %d, want 202 (slot should have freed)", code)
	}
	final := pollRun(t, ts, "", runB.RunID)
	if final.Status != "done" {
		t.Fatalf("run B status %q, want done", final.Status)
	}
}

// TestServeMaxConcurrentRunsReleasedOnError proves the global counter is
// released even when a run ends in an OPERATIONAL ERROR (not just a normal
// completion) — driveRun errors out immediately on an unresolvable -base,
// well before any agent runs, so this exercises the defer in execRun, not
// the ordinary success tail.
func TestServeMaxConcurrentRunsReleasedOnError(t *testing.T) {
	_, repo := makeGoRepo(t)
	s, err := newServer(context.Background(), serverConfig{repos: []string{repo}, envMode: envModeInherit, maxConcurrentRuns: 1})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)

	var created struct{ RunID string }
	code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Base:  "no-such-branch-at-all",
		Tasks: []taskSpec{{ID: "t1"}},
		Agent: writeFileAgent("x.txt"),
	}, &created)
	if code != http.StatusAccepted {
		t.Fatalf("POST status %d, want 202", code)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "error" {
		t.Fatalf("status %q, want error (an unresolvable -base fails driveRun operationally)", final.Status)
	}

	// If the counter weren't released on that error path, this would 429.
	var created2 struct{ RunID string }
	code = doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Tasks: []taskSpec{{ID: "t2"}},
		Agent: writeFileAgent("y.txt"),
	}, &created2)
	if code != http.StatusAccepted {
		t.Fatalf("post-error POST status %d, want 202 (concurrency counter should have been released)", code)
	}
	final2 := pollRun(t, ts, "", created2.RunID)
	if final2.Status != "done" {
		t.Fatalf("recovery run status %q, want done", final2.Status)
	}
}

// TestServeMaxRunTimeClampsBudget exercises buildParams' min() directly:
// a request asking for MORE than the server ceiling is clamped down to it; a
// request asking for LESS keeps its own, stricter budget; a request setting
// no budget at all inherits the ceiling.
func TestServeMaxRunTimeClampsBudget(t *testing.T) {
	_, repo := makeGoRepo(t)
	s, err := newServer(context.Background(), serverConfig{repos: []string{repo}, envMode: envModeInherit, maxRunTime: 30 * time.Second})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	rc := s.byKey[repo]

	cases := []struct {
		name   string
		budget string
		want   time.Duration
	}{
		{"request asks for more than the ceiling", "60s", 30 * time.Second},
		{"request asks for less: its own budget wins", "10s", 10 * time.Second},
		{"request sets no budget: the ceiling applies", "", 30 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _, err := s.buildParams(runRequest{
				Cell: repo, Tasks: []taskSpec{{ID: "t1"}}, Agent: "true", Budget: c.budget,
			}, rc.cell.Repo(), false)
			if err != nil {
				t.Fatalf("buildParams: %v", err)
			}
			if p.Budget != c.want {
				t.Fatalf("budget=%s, want %s", p.Budget, c.want)
			}
		})
	}
}

// TestServeMaxRunTimeAppliesEndToEnd proves the clamp actually reaches
// driveRun: a request sets no -budget at all, the server's 1s ceiling alone
// must cut a 5s -verify sleep short, exactly like TestDriveRunVerifyKilledByBudget
// at the driveRun layer.
func TestServeMaxRunTimeAppliesEndToEnd(t *testing.T) {
	_, repo := makeGoRepo(t)
	s, err := newServer(context.Background(), serverConfig{repos: []string{repo}, envMode: envModeInherit, maxRunTime: 1 * time.Second})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)

	var created struct{ RunID string }
	code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:   repo,
		Tasks:  []taskSpec{{ID: "t1"}},
		Agent:  writeFileAgent("x.txt"),
		Verify: "sleep 5",
	}, &created)
	if code != http.StatusAccepted {
		t.Fatalf("POST status %d, want 202", code)
	}

	start := time.Now()
	final := pollRun(t, ts, "", created.RunID)
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Fatalf("run took %s; -max-run-time 1s should have cut the 5s verify sleep short", elapsed)
	}
	if final.Report == nil || !strings.Contains(final.Report.Verify.Output, "budget") {
		t.Fatalf("verify output should name the exhausted budget, got report=%+v", final.Report)
	}
}

// TestServeMaxParallelAgentsClampsRequest exercises buildParams' min()
// directly, mirroring TestServeMaxRunTimeClampsBudget above: a request
// asking for MORE than the server ceiling is clamped down to it; a request
// asking for LESS keeps its own, stricter value; a request setting nothing
// (<=0, "use the default") inherits the ceiling.
func TestServeMaxParallelAgentsClampsRequest(t *testing.T) {
	_, repo := makeGoRepo(t)
	s, err := newServer(context.Background(), serverConfig{repos: []string{repo}, envMode: envModeInherit, maxParallelAgents: 4})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	rc := s.byKey[repo]

	cases := []struct {
		name           string
		parallelAgents int
		want           int
	}{
		{"request asks for more than the ceiling", 10, 4},
		{"request asks for less: its own value wins", 2, 2},
		{"request sets nothing (0): the ceiling applies", 0, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _, err := s.buildParams(runRequest{
				Cell: repo, Tasks: []taskSpec{{ID: "t1"}}, Agent: "true", ParallelAgents: c.parallelAgents,
			}, rc.cell.Repo(), false)
			if err != nil {
				t.Fatalf("buildParams: %v", err)
			}
			if p.ParallelAgents != c.want {
				t.Fatalf("parallelAgents=%d, want %d", p.ParallelAgents, c.want)
			}
		})
	}
}

// TestServeMaxParallelAgentsAppliesEndToEnd proves the clamp actually reaches
// driveRun, mirroring TestServeMaxRunTimeAppliesEndToEnd: a request asks for
// -parallel-agents 4 (an over-ask, more than the server allows) across four
// tasks, but the server's ceiling of 1 must still force them to run strictly
// one at a time. Reuses overlapDetectorAgent (run_test.go) to observe real
// concurrency rather than just the clamped number buildParams computed.
func TestServeMaxParallelAgentsAppliesEndToEnd(t *testing.T) {
	_, repo := makeGoRepo(t)
	s, err := newServer(context.Background(), serverConfig{repos: []string{repo}, envMode: envModeInherit, maxParallelAgents: 1})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)

	agentCmd, overlapFile := overlapDetectorAgent(t)
	var created struct{ RunID string }
	code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:           repo,
		Tasks:          []taskSpec{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}},
		Agent:          agentCmd,
		ParallelAgents: 4, // over-ask: the server's ceiling of 1 must still win
	}, &created)
	if code != http.StatusAccepted {
		t.Fatalf("POST status %d, want 202", code)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "done" {
		t.Fatalf("status %q, want done: %+v", final.Status, final)
	}
	if _, err := os.Stat(overlapFile); err == nil {
		t.Fatal("overlap detected despite -max-parallel-agents 1; the server ceiling did not reach driveRun")
	}
}

// TestServeQuotasOffIsUnlimited: with every quota left at its zero value
// (unlimited), a run whose agent count would trip any of the caps tested
// above still succeeds exactly as it did before quotas existed (#60) — the
// byte-identical-when-off guarantee.
func TestServeQuotasOffIsUnlimited(t *testing.T) {
	_, repo := makeGoRepo(t)
	s, err := newServer(context.Background(), serverConfig{
		repos: []string{repo}, envMode: envModeInherit,
		maxAgentsPerRun: 0, maxRunTime: 0, maxConcurrentRuns: 0, maxParallelAgents: 0,
	})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)

	var created struct{ RunID string }
	code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:   repo,
		Tasks:  manyTasks(5),
		Agent:  perTaskAgent,
		Verify: "true",
	}, &created)
	if code != http.StatusAccepted {
		t.Fatalf("POST status %d, want 202", code)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "done" || final.Report == nil || !final.Report.Verify.OK || len(final.Report.PerAgent) != 5 {
		t.Fatalf("run = %+v, want a clean 5-agent landed run", final)
	}
}

// TestServeUsageRecordMatchesKnownRun builds a run with a KNOWN shape (one
// agent that succeeds, one that fails outright, and a -verify that fails
// once then passes under -verify-retries) and checks the usage record's
// numbers against it exactly, both from GET /runs/{id}/usage and embedded in
// GET /runs/{id}.
func TestServeUsageRecordMatchesKnownRun(t *testing.T) {
	_, repo := makeGoRepo(t)
	_, ts := newTestServer(t, "", repo)

	marker := filepath.Join(t.TempDir(), "verify-marker")
	flakyVerify := "test -f '" + marker + "' || { touch '" + marker + "'; exit 1; }"
	agent := `if [ "$SIGBOUND_TASK_ID" = "ok1" ]; then printf 'hi\n' > ok.txt; else exit 1; fi`

	var created struct{ RunID string }
	code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:          repo,
		Tasks:         []taskSpec{{ID: "ok1"}, {ID: "bad1"}},
		Agent:         agent,
		Verify:        flakyVerify,
		VerifyRetries: 1,
	}, &created)
	if code != http.StatusAccepted {
		t.Fatalf("POST status %d, want 202", code)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "done" {
		t.Fatalf("status %q (error=%q), want done", final.Status, final.Error)
	}
	if !final.Report.Verify.OK || !final.Report.Verify.Flaky {
		t.Fatalf("expected a flaky-but-green verify, got %+v", final.Report.Verify)
	}

	var u UsageJSON
	if code := doJSON(t, "GET", ts.URL+"/runs/"+created.RunID+"/usage", "", nil, &u); code != http.StatusOK {
		t.Fatalf("GET usage status %d", code)
	}
	want := UsageJSON{
		AgentsTotal:    2,
		AgentsOK:       1,
		AgentsFailed:   1,
		VerifyAttempts: 2, // the failing first attempt + the retry that passed
		RepairAttempts: 0, // no -repair configured
		Landed:         true,
	}
	if u.AgentsTotal != want.AgentsTotal || u.AgentsOK != want.AgentsOK || u.AgentsFailed != want.AgentsFailed {
		t.Fatalf("agent counts = %+v, want total=%d ok=%d failed=%d", u, want.AgentsTotal, want.AgentsOK, want.AgentsFailed)
	}
	if u.VerifyAttempts != want.VerifyAttempts {
		t.Fatalf("verifyAttempts=%d, want %d", u.VerifyAttempts, want.VerifyAttempts)
	}
	if u.RepairAttempts != want.RepairAttempts {
		t.Fatalf("repairAttempts=%d, want %d", u.RepairAttempts, want.RepairAttempts)
	}
	if u.Landed != want.Landed {
		t.Fatalf("landed=%v, want %v", u.Landed, want.Landed)
	}
	if u.VerifyWallMs <= 0 {
		t.Fatalf("verifyWallMs=%d, want > 0", u.VerifyWallMs)
	}
	if u.TotalWallMs <= 0 {
		t.Fatalf("totalWallMs=%d, want > 0", u.TotalWallMs)
	}
	if u.ReportBytes <= 0 {
		t.Fatalf("reportBytes=%d, want > 0", u.ReportBytes)
	}

	// GET /runs/{id} embeds the identical record.
	if final.Usage == nil || *final.Usage != u {
		t.Fatalf("GET /runs/{id} usage = %+v, want it to match GET /runs/{id}/usage %+v", final.Usage, u)
	}
}

// TestServeUsageAggregateSumsAcrossRuns runs two separate jobs and checks
// GET /usage's totals equal the per-run usage records summed, plus a
// correct single-cell rollup.
func TestServeUsageAggregateSumsAcrossRuns(t *testing.T) {
	_, repo := makeGoRepo(t)
	s, ts := newTestServer(t, "", repo)

	var run1, run2 struct{ RunID string }
	doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell: repo, Tasks: []taskSpec{{ID: "a1"}}, Agent: writeFileAgent("a.txt"),
	}, &run1)
	pollRun(t, ts, "", run1.RunID)
	doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell: repo, Tasks: manyTasks(2), Agent: perTaskAgent,
	}, &run2)
	pollRun(t, ts, "", run2.RunID)

	var u1, u2 UsageJSON
	if code := doJSON(t, "GET", ts.URL+"/runs/"+run1.RunID+"/usage", "", nil, &u1); code != http.StatusOK {
		t.Fatalf("GET usage 1 status %d", code)
	}
	if code := doJSON(t, "GET", ts.URL+"/runs/"+run2.RunID+"/usage", "", nil, &u2); code != http.StatusOK {
		t.Fatalf("GET usage 2 status %d", code)
	}

	var agg struct {
		Totals usageTotals `json:"totals"`
		Cells  []struct {
			Cell  string      `json:"cell"`
			Repo  string      `json:"repo"`
			Usage usageTotals `json:"usage"`
		} `json:"cells"`
	}
	if code := doJSON(t, "GET", ts.URL+"/usage", "", nil, &agg); code != http.StatusOK {
		t.Fatalf("GET /usage status %d", code)
	}

	if agg.Totals.Runs != 2 {
		t.Fatalf("totals.runs=%d, want 2", agg.Totals.Runs)
	}
	if want := u1.AgentsTotal + u2.AgentsTotal; agg.Totals.AgentsTotal != want {
		t.Fatalf("totals.agentsTotal=%d, want %d", agg.Totals.AgentsTotal, want)
	}
	if want := u1.TotalWallMs + u2.TotalWallMs; agg.Totals.TotalWallMs != want {
		t.Fatalf("totals.totalWallMs=%d, want %d", agg.Totals.TotalWallMs, want)
	}
	if want := u1.ReportBytes + u2.ReportBytes; agg.Totals.ReportBytes != want {
		t.Fatalf("totals.reportBytes=%d, want %d", agg.Totals.ReportBytes, want)
	}
	wantLanded := 0
	if u1.Landed {
		wantLanded++
	}
	if u2.Landed {
		wantLanded++
	}
	if agg.Totals.Landed != wantLanded {
		t.Fatalf("totals.landed=%d, want %d", agg.Totals.Landed, wantLanded)
	}

	if len(agg.Cells) != 1 {
		t.Fatalf("cells=%v, want exactly 1", agg.Cells)
	}
	if agg.Cells[0].Cell != s.cells[0].cell.ID() || agg.Cells[0].Repo != repo {
		t.Fatalf("cell rollup = %+v, want cell %s repo %s", agg.Cells[0], s.cells[0].cell.ID(), repo)
	}
	if agg.Cells[0].Usage.Runs != 2 || agg.Cells[0].Usage.AgentsTotal != agg.Totals.AgentsTotal {
		t.Fatalf("cell rollup usage = %+v, want it to equal the (single-cell) grand total", agg.Cells[0].Usage)
	}
}
