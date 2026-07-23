package main

import (
	"bytes"
	"context"
	"encoding/json"
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
			code := doJSON(t, "GET", ts.URL+"/health", c.send, nil, nil)
			if code != c.want {
				t.Fatalf("auth %q: status %d, want %d", c.name, code, c.want)
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
	if created.RunID == "" || created.Status != "running" {
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

	// Let the first run finish; the slot frees and a new run is accepted.
	pollRun(t, ts, "", first.RunID)
	code = doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Tasks: []taskSpec{{ID: "t3"}},
		Agent: writeFileAgent("after.txt"),
	}, nil)
	if code != http.StatusAccepted {
		t.Fatalf("post-completion POST status %d, want 202", code)
	}
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
		name string
		body any
		want string // substring the error must contain
	}{
		{"unknown cell", runRequest{Cell: "/no/such/repo", Tasks: []taskSpec{{ID: "t1"}}, Agent: "true"}, "unknown cell"},
		{"tasks and goal", runRequest{Cell: repo, Tasks: []taskSpec{{ID: "t1"}}, Goal: "do it", Agent: "true"}, "mutually exclusive"},
		{"neither tasks nor goal", runRequest{Cell: repo, Agent: "true"}, "one of tasks or goal"},
		{"missing agent", runRequest{Cell: repo, Tasks: []taskSpec{{ID: "t1"}}}, "agent is required"},
		{"empty task id", runRequest{Cell: repo, Tasks: []taskSpec{{ID: ""}}, Agent: "true"}, "empty id"},
		{"verifyImpact without verify", runRequest{Cell: repo, Tasks: []taskSpec{{ID: "t1"}}, Agent: "true", VerifyImpact: "go test ./..."}, "verifyImpact requires verify"},
		{"bad duration", runRequest{Cell: repo, Tasks: []taskSpec{{ID: "t1"}}, Agent: "true", Budget: "notaduration"}, "budget"},
		{"goal without planner", runRequest{Cell: repo, Goal: "split it", Agent: "true"}, "planner is required"},
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

	// An unknown run id is 404.
	if code := doJSON(t, "GET", ts.URL+"/runs/does-not-exist", "", nil, nil); code != http.StatusNotFound {
		t.Fatalf("unknown run status %d, want 404", code)
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
