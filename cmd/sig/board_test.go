package main

// Coverage for GET /board (issue #114). The board is DERIVED, so the tests
// assert the derivation against the journal that produced it: one leg drives a
// real parked run through `sig serve` and watches the card move when the journal
// moves, the metrics leg reconciles every counter against fixtures written
// through the production writers, and the UI leg drives crafted content all the
// way to the page's data.

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// getBoard fetches GET /board (optionally with a query) and fails on anything
// but 200.
func getBoard(t *testing.T, base, token, query string) boardResponse {
	t.Helper()
	var b boardResponse
	url := base + "/board"
	if query != "" {
		url += "?" + query
	}
	if code := doJSON(t, "GET", url, token, nil, &b); code != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", url, code)
	}
	return b
}

// cardFor returns the board card with the given intent id ("" is the no-intent
// bucket), or fails naming what was actually on the board.
func cardFor(t *testing.T, bc boardCell, id string) boardIntent {
	t.Helper()
	var have []string
	for _, it := range bc.Intents {
		if it.ID == id {
			return it
		}
		have = append(have, it.ID)
	}
	t.Fatalf("no card for intent %q; board has %v", id, have)
	return boardIntent{}
}

// TestBoardReflectsRealJournal drives a REAL run that parks, then acks it, and
// asserts the card moves because the journal moved — never because anything set
// a column. This is the whole "you cannot drag a card" property.
func TestBoardReflectsRealJournal(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	writeIntent(t, f.repo, "ship-auth", "goal = wire up the auth token\npriority = 3\nissue = 7\nschedule = 24h\n")

	b := getBoard(t, f.ts.URL, "", "")
	if len(b.Cells) != 1 {
		t.Fatalf("board has %d cells, want 1", len(b.Cells))
	}
	// An intent with no runs is open — the intents directory is read, and it
	// contributes a card even though the journal knows nothing about it.
	open := cardFor(t, b.Cells[0], "ship-auth")
	if open.Column != boardOpen || len(open.Runs) != 0 {
		t.Fatalf("intent with no runs: column %q with %d runs, want %q/0", open.Column, len(open.Runs), boardOpen)
	}
	if open.Goal != "wire up the auth token" || open.Priority != 3 || open.Issue != 7 || open.Schedule != "24h0m0s" {
		t.Fatalf("intent card lost its file's fields: %+v", open)
	}
	// The real parked run: no request field can attribute a serve run to an
	// intent today (see board.go), so it lands in the no-intent bucket — and the
	// park is what puts that card in the actionable column.
	parked := cardFor(t, b.Cells[0], "")
	if parked.Column != boardAwaitingAck {
		t.Fatalf("a parked run put the card in %q, want %q", parked.Column, boardAwaitingAck)
	}
	if len(parked.Runs) != 1 || parked.Runs[0].ID != f.runID {
		t.Fatalf("no-intent card runs = %+v, want the parked run %s", parked.Runs, f.runID)
	}
	if parked.Runs[0].Status != statusAwaitingAck {
		t.Fatalf("run status %q, want %q", parked.Runs[0].Status, statusAwaitingAck)
	}

	// Move the journal: ack releases the verified landing. Nothing tells the
	// board about it; it re-derives.
	if code := doJSON(t, "POST", f.ts.URL+"/runs/"+f.runID+"/ack", "", map[string]any{}, nil); code != http.StatusOK {
		t.Fatalf("POST ack: status %d, want 200", code)
	}
	after := cardFor(t, getBoard(t, f.ts.URL, "", "").Cells[0], "")
	if after.Column != boardLanded {
		t.Fatalf("after ack the card is in %q, want %q", after.Column, boardLanded)
	}
	if after.Runs[0].LandedSHA == "" {
		t.Fatal("acked run shows no landed sha on the board")
	}
}

// newAllParkedFixture drives a REAL run whose ONLY task touches an ack-path, so
// EVERY group parks and the run's own report records no landing at all
// (finalSHA == baseSHA). newParkFixture cannot stand in for this: its clean
// group lands on its own, which moves finalSHA before anybody acks, so the
// report already says "landed" and an ack the board cannot see stays invisible.
func newAllParkedFixture(t *testing.T) *parkFixture {
	t.Helper()
	requirePOSIXShell(t)
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	commitPolicy(t, g, repo, parkPolicyAckPaths)
	srv, ts := newTestServer(t, "", repo)
	var created struct {
		RunID string `json:"runId"`
	}
	if code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:   repo,
		Base:   "main",
		Tasks:  []taskSpec{taskWrite(t, "held", map[string]string{"auth/token.go": "package auth\n\nfunc Token() string { return \"t\" }\n"})},
		Agent:  agent,
		Verify: "go build ./...",
	}, &created); code != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", code)
	}
	pollRunStatus(t, ts, "", created.RunID, statusAwaitingAck)
	f := &parkFixture{
		t: t, repo: repo, g: g, cell: srv.cells[0].cell, srv: srv, ts: ts,
		runID: created.RunID, dir: filepath.Join(srv.cells[0].runsDir, created.RunID),
	}
	f.park = f.reread()
	// The premise this fixture exists for: the run itself landed NOTHING, so its
	// report can never be what tells a reader this run landed.
	rep, err := readRunReport(f.dir)
	if err != nil {
		t.Fatalf("read the parked run's report: %v", err)
	}
	if landed(rep) {
		t.Fatalf("fixture is not all-parked: the report already records a landing (finalSHA %s, baseSHA %s)",
			short(rep.Integrate.FinalSHA), short(rep.BaseSHA))
	}
	return f
}

// TestBoardCountsAnAckedLanding is the ALL-PARKED ack: a run that parked every
// group lands only when a human acks it, and that landing is recorded in
// park.json — never in the report, which was written before the ack existed. A
// board that reads landedness out of the report alone shows this run as needing
// attention with no landed sha, and counts landed=0, forever, for a landing that
// happened.
func TestBoardCountsAnAckedLanding(t *testing.T) {
	f := newAllParkedFixture(t)

	before := getBoard(t, f.ts.URL, "", "limit=0")
	card := cardFor(t, before.Cells[0], "")
	if card.Column != boardAwaitingAck {
		t.Fatalf("before the ack the card is in %q, want %q", card.Column, boardAwaitingAck)
	}
	if before.Metrics.Landed != 0 {
		t.Fatalf("before the ack metrics.landed = %d, want 0 — nothing has landed yet", before.Metrics.Landed)
	}

	var out ackOutcome
	if code := doJSON(t, "POST", f.ts.URL+"/runs/"+f.runID+"/ack", "", map[string]any{}, &out); code != http.StatusOK {
		t.Fatalf("POST ack: status %d, want 200", code)
	}
	if out.LandedSHA == "" {
		t.Fatal("the ack reported no landed sha")
	}
	// Ground truth, independent of anything the board reads: the base ref moved.
	if head := f.head(); head != out.LandedSHA {
		t.Fatalf("main is at %s, the ack claims it landed %s", short(head), short(out.LandedSHA))
	}

	after := getBoard(t, f.ts.URL, "", "limit=0")
	acked := cardFor(t, after.Cells[0], "")
	if acked.Column != boardLanded {
		t.Fatalf("after the ack the card is in %q, want %q", acked.Column, boardLanded)
	}
	if acked.Runs[0].LandedSHA != short(out.LandedSHA) {
		t.Fatalf("the card shows landed sha %q, want the acked commit %q", acked.Runs[0].LandedSHA, short(out.LandedSHA))
	}
	if after.Metrics.Landed != 1 {
		t.Fatalf("after the ack metrics.landed = %d, want 1", after.Metrics.Landed)
	}
	// The landed-only populations agree with that count: this run has a metering
	// record, and it landed, so it is in both of them.
	if after.Metrics.LandedWallRuns != 1 || after.Metrics.AgentWallLanded != 1 {
		t.Fatalf("landedWallRuns=%d agentWallLanded=%d, want 1/1 — an acked landing is a landing in every metric",
			after.Metrics.LandedWallRuns, after.Metrics.AgentWallLanded)
	}
	// GET /usage reads the same landing through its own aggregate. The two
	// endpoints answering differently about one landing is the disagreement
	// this whole derivation exists to prevent, so pin it here rather than
	// trusting that the shared helper is called on both paths.
	var agg struct {
		Totals usageTotals `json:"totals"`
	}
	if code := doJSON(t, "GET", f.ts.URL+"/usage", "", nil, &agg); code != http.StatusOK {
		t.Fatalf("GET /usage: status %d, want 200", code)
	}
	if agg.Totals.Landed != 1 {
		t.Fatalf("GET /usage reports landed=%d, want 1 — /board and /usage disagree about the same acked landing", agg.Totals.Landed)
	}
}

// TestBoardDoesNotCountAnAckWhoseRefNeverMoved is the fail-closed half of the
// rule above. Both ack paths write resolvedAt and landedSHA BEFORE they move the
// ref, and only ErrRefMoved rewinds them -- any other landRef failure returns
// with the record already claiming a landing. A stale refs/heads/main.lock, the
// thing a crashed git leaves behind, is enough to produce that state without any
// crash of our own.
//
// Reading the park record alone would then report a landing for a ref that
// provably never moved, which is worse than the run report's own conservative
// answer. The run status is the on-disk fact that means the ref moved -- it is
// written only after landRef succeeds -- so the derivation gates on it.
func TestBoardDoesNotCountAnAckWhoseRefNeverMoved(t *testing.T) {
	f := newAllParkedFixture(t)
	head := f.head()

	// Make landRef fail for a reason that is NOT ErrRefMoved.
	lock := filepath.Join(f.repo, ".git", "refs", "heads", "main.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatalf("plant a stale ref lock: %v", err)
	}
	if code := doJSON(t, "POST", f.ts.URL+"/runs/"+f.runID+"/ack", "", map[string]any{}, nil); code == http.StatusOK {
		t.Fatal("the ack succeeded with a stale ref lock in place; this test cannot prove anything")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatalf("remove the stale ref lock: %v", err)
	}

	// Ground truth: nothing landed.
	if now := f.head(); now != head {
		t.Fatalf("main moved from %s to %s despite the failed ack", short(head), short(now))
	}
	// The record nevertheless claims one -- that is the state under test.
	if pk := f.reread(); pk.LandedSHA == "" {
		t.Skip("the failed ack left no landedSHA in the park record, so the rule under test cannot fire")
	}

	board := getBoard(t, f.ts.URL, "", "limit=0")
	if card := cardFor(t, board.Cells[0], ""); card.Column == boardLanded {
		t.Fatalf("the card is in %q for a ref that never moved", card.Column)
	}
	if board.Metrics.Landed != 0 {
		t.Fatalf("metrics.landed = %d, want 0 — the base ref is still at %s", board.Metrics.Landed, short(head))
	}
	var agg struct {
		Totals usageTotals `json:"totals"`
	}
	if code := doJSON(t, "GET", f.ts.URL+"/usage", "", nil, &agg); code != http.StatusOK {
		t.Fatalf("GET /usage: status %d, want 200", code)
	}
	if agg.Totals.Landed != 0 {
		t.Fatalf("GET /usage reports landed=%d, want 0 — nothing landed", agg.Totals.Landed)
	}
}

// TestBoardRejectedParkIsNotLanded drives a REAL park to a real rejection. Its
// report records a green, fully verified landing — the exact commit an ack would
// have released — so a card that keyed on the recorded sha alone would claim the
// work landed when a human refused it.
func TestBoardRejectedParkIsNotLanded(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	if code := doJSON(t, "POST", f.ts.URL+"/runs/"+f.runID+"/reject", "", map[string]any{"reason": "not now"}, nil); code != http.StatusOK {
		t.Fatalf("POST reject: status %d, want 200", code)
	}
	card := cardFor(t, getBoard(t, f.ts.URL, "", "").Cells[0], "")
	if card.Runs[0].LandedSHA == "" {
		t.Skip("the rejected run's report records no landed sha, so the rule under test cannot fire")
	}
	if card.Column != boardAttention {
		t.Fatalf("a rejected park put the card in %q, want %q — nothing landed", card.Column, boardAttention)
	}
}

// TestBoardColumnPrecedence pins the derivation rules that are NOT "whatever the
// newest run says": a park outranks a newer landing (verified work is still
// being held, and a newer green run must not hide it), and a run in flight
// outranks a finished one.
func TestBoardColumnPrecedence(t *testing.T) {
	// scanRuns order: newest first.
	cases := []struct {
		name string
		rows []logRow
		want string
	}{
		{"no runs", nil, boardOpen},
		{"landed", []logRow{{ID: "b", Status: "done", LandedSHA: "abc"}}, boardLanded},
		{"done but nothing landed", []logRow{{ID: "b", Status: "done"}}, boardAttention},
		// A rejected park's report carries the sha it WOULD have landed; a human
		// refused it, so nothing landed and the card must not claim otherwise.
		{"rejected park", []logRow{{ID: "b", Status: statusRejected, LandedSHA: "abc"}}, boardAttention},
		{"interrupted after computing a sha", []logRow{{ID: "b", Status: "interrupted", LandedSHA: "abc"}}, boardAttention},
		{"errored", []logRow{{ID: "b", Status: "error"}}, boardAttention},
		{"interrupted", []logRow{{ID: "b", Status: "interrupted"}}, boardAttention},
		{"park under a newer landing", []logRow{
			{ID: "c", Status: "done", LandedSHA: "abc"},
			{ID: "b", Status: statusAwaitingAck},
		}, boardAwaitingAck},
		{"in flight under a newer landing", []logRow{
			{ID: "c", Status: "done", LandedSHA: "abc"},
			{ID: "b", Status: "running"},
		}, boardRunning},
		{"park outranks in flight", []logRow{
			{ID: "c", Status: "running"},
			{ID: "b", Status: statusAwaitingAck},
		}, boardAwaitingAck},
	}
	for _, c := range cases {
		if got := boardColumn(c.rows); got != c.want {
			t.Errorf("%s: column %q, want %q", c.name, got, c.want)
		}
	}
}

// TestBoardKeepsRunsWhoseIntentFileIsGone: the journal wins. A run recorded
// against an intent whose file has since been deleted still reaches the board,
// flagged as missing rather than silently dropped.
func TestBoardKeepsRunsWhoseIntentFileIsGone(t *testing.T) {
	intents := []intent{{ID: "alive", Goal: "still on disk"}}
	rows := []logRow{
		{ID: "r3", Status: "done", LandedSHA: "sha3", Intent: "deleted"},
		{ID: "r2", Status: "done", LandedSHA: "sha2", Intent: "alive"},
		{ID: "r1", Status: "error"},
	}
	cards := buildBoard(intents, rows)
	byID := map[string]boardIntent{}
	for _, c := range cards {
		byID[c.ID] = c
	}
	if len(cards) != 3 {
		t.Fatalf("got %d cards, want 3 (alive, deleted, no-intent): %+v", len(cards), cards)
	}
	if c := byID["deleted"]; !c.Missing || len(c.Runs) != 1 || c.Column != boardLanded {
		t.Fatalf("deleted intent card = %+v; want missing, 1 run, landed", c)
	}
	if c := byID["alive"]; c.Missing || len(c.Runs) != 1 {
		t.Fatalf("alive intent card = %+v", c)
	}
	if c := byID[""]; c.Missing || len(c.Runs) != 1 || c.Column != boardAttention {
		t.Fatalf("no-intent card = %+v; want 1 run, attention", c)
	}
	// Every run reached exactly one card — nothing was dropped or duplicated.
	total := 0
	for _, c := range cards {
		total += len(c.Runs)
	}
	if total != len(rows) {
		t.Fatalf("%d rows reached the board, want %d", total, len(rows))
	}
}

// fixtureRun writes one run directory through the PRODUCTION writers, so the
// metrics read exactly the bytes a real run would leave.
func fixtureRun(t *testing.T, runsDir, id, status string, rep runReport, u *UsageJSON) {
	t.Helper()
	dir := filepath.Join(runsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunStatus(dir, status, "")
	writeRunReport(dir, rep)
	if u != nil {
		writeRunUsage(dir, *u)
	}
}

// TestBoardMetricsReconcile builds a run history whose every metric input is
// known by construction, then asserts GET /board's counters equal the numbers
// re-derived from those inputs — a reconciliation, not an eyeball. The fixtures
// deliberately include the awkward cases: a run with no metering record, a run
// whose report will not read back, a pre-v2.1 run with no agent wall time, and a
// run that reported cost.
func TestBoardMetricsReconcile(t *testing.T) {
	_, repo := makeGoRepo(t)
	srv, ts := newTestServer(t, "", repo)
	runsDir := srv.cells[0].runsDir

	landedRep := func(agentCmd string, wallMs int64) runReport {
		return runReport{
			BaseSHA:   "base0",
			AgentCmd:  agentCmd,
			PerAgent:  []perAgentJSON{{ID: "t1", Branch: "agent/t1", SHA: "s1", OK: true, WallMs: wallMs}},
			Integrate: integrateJSON{Landed: []string{"agent/t1"}, FinalSHA: "final1"},
			Verify:    verifyJSON{Ran: true, OK: true, Invocations: 1},
		}
	}

	// 1: landed, verify green, claude preset, 4000ms agent time, cost reported.
	fixtureRun(t, runsDir, "20260101T000001Z-aaaa", "done",
		landedRep(agentPresets["claude"], 4000),
		&UsageJSON{Landed: true, TotalWallMs: 10000, AgentWallMs: 4000, CostUSD: 1.50, CostAgents: 1, InputTokens: 100, OutputTokens: 20})
	// 2: landed, verify green, claude preset, pre-v2.1 (no agentWallMs), no cost.
	fixtureRun(t, runsDir, "20260101T000002Z-bbbb", "done",
		landedRep(agentPresets["claude"], 0),
		&UsageJSON{Landed: true, TotalWallMs: 20000})
	// 3: custom agent, verify RED, one flagged branch, nothing landed.
	fixtureRun(t, runsDir, "20260101T000003Z-cccc", "done", runReport{
		BaseSHA:   "base0",
		AgentCmd:  "./my-own-agent",
		PerAgent:  []perAgentJSON{{ID: "t1", Branch: "agent/t1", OK: true, WallMs: 900}},
		Integrate: integrateJSON{FinalSHA: "final3", Flagged: []flaggedJSON{{Branch: "agent/t2", Paths: []string{"a.txt"}}}},
		Verify:    verifyJSON{Ran: true, OK: false, Invocations: 2},
	}, &UsageJSON{Landed: false, TotalWallMs: 30000, AgentWallMs: 900})
	// 4: custom agent, bisect ran and SALVAGED a subset; landed; no usage record
	// at all (so it is absent from every wall-clock and cost number).
	fixtureRun(t, runsDir, "20260101T000004Z-dddd", "done", runReport{
		BaseSHA:   "base0",
		AgentCmd:  "./my-own-agent",
		PerAgent:  []perAgentJSON{{ID: "t1", Branch: "agent/t1", OK: true, WallMs: 700}},
		Integrate: integrateJSON{Landed: []string{"agent/t1"}, FinalSHA: "final4", DroppedByBisect: []string{"agent/t2"}},
		Verify: verifyJSON{Ran: true, OK: true, Invocations: 3,
			Bisect: &bisectJSON{Ran: true, Attempts: 2, LandedGroups: [][]string{{"agent/t1"}}, DroppedGroups: [][]string{{"agent/t2"}}}},
	}, nil)
	// 5: bisect ran and salvaged NOTHING.
	fixtureRun(t, runsDir, "20260101T000005Z-eeee", "done", runReport{
		BaseSHA:   "base0",
		AgentCmd:  "./my-own-agent",
		PerAgent:  []perAgentJSON{{ID: "t1", Branch: "agent/t1", OK: true, WallMs: 600}},
		Integrate: integrateJSON{FinalSHA: "final5"},
		Verify: verifyJSON{Ran: true, OK: false, Invocations: 3,
			Bisect: &bisectJSON{Ran: true, Attempts: 2, LandedGroups: [][]string{}, DroppedGroups: [][]string{{"agent/t1"}}}},
	}, &UsageJSON{Landed: false, TotalWallMs: 5000, AgentWallMs: 600})
	// 6: a torn run dir — report.json present but unparseable. It has no verdict,
	// so it must contribute NOTHING, not a zero.
	torn := filepath.Join(runsDir, "20260101T000006Z-ffff")
	if err := os.MkdirAll(torn, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunStatus(torn, "done", "")
	if err := os.WriteFile(filepath.Join(torn, "report.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	want := boardMetrics{
		Runs: 5, Landed: 3,
		VerifyRan: 5, VerifyPassed: 3,
		BisectRan: 2, BisectSalvaged: 1,
		FlaggedRuns: 1,
		// Runs 1+2 landed AND carry usage; run 4 landed with no usage record.
		LandedWallMs: 30000, LandedWallRuns: 2,
		// Runs 1, 3 and 5 recorded agent wall time, but only run 1 of those
		// LANDED — and this is a per-landed-change mean, so 3 and 5 are in
		// neither number. Summing all 5500ms over the single landing would
		// report 5500ms per landed change for a run that took 4000.
		AgentWallMs: 4000, AgentWallLanded: 1,
		CostUSD: 1.50, CostRuns: 1, CostLanded: 1, InputTokens: 100, OutputTokens: 20,
		Presets: []boardPreset{
			{Preset: "claude", Runs: 2, Landed: 2},
			{Preset: "custom", Runs: 3, Landed: 1},
		},
	}
	got := getBoard(t, ts.URL, "", "limit=0").Metrics
	if !jsonEqual(t, got, want) {
		t.Fatalf("metrics mismatch\n got %s\nwant %s", mustJSON(t, got), mustJSON(t, want))
	}

	// Independent re-derivation: walk the same directories with the test's own
	// summation and confirm the daemon's totals are those files' totals, not a
	// number this test copied out of the implementation.
	var sumTotal, sumAgent int64
	var sumCost float64
	for _, id := range runDirNames(runsDir) {
		u, err := readRunUsage(filepath.Join(runsDir, id))
		if err != nil {
			continue
		}
		if u.Landed {
			sumTotal += u.TotalWallMs
			// Same population as the denominator: landed runs only.
			sumAgent += u.AgentWallMs
		}
		sumCost += u.CostUSD
	}
	if sumTotal != got.LandedWallMs || sumAgent != got.AgentWallMs || sumCost != got.CostUSD {
		t.Fatalf("re-derived from usage.json: landedWall=%d agentWall=%d cost=%v; board reported %d/%d/%v",
			sumTotal, sumAgent, sumCost, got.LandedWallMs, got.AgentWallMs, got.CostUSD)
	}
}

// jsonEqual compares two values by their JSON encoding — the shape the client
// actually receives.
func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	return mustJSON(t, a) == mustJSON(t, b)
}

// TestBoardLimitBoundsTheWindow: ?limit is the same bounded window /log uses, and
// the metrics describe exactly that window rather than all history.
func TestBoardLimitBoundsTheWindow(t *testing.T) {
	_, repo := makeGoRepo(t)
	srv, ts := newTestServer(t, "", repo)
	rep := runReport{BaseSHA: "b", AgentCmd: "x", Integrate: integrateJSON{FinalSHA: "f"}, Verify: verifyJSON{Ran: true, OK: true}}
	for _, id := range []string{"20260101T000001Z-a", "20260101T000002Z-b", "20260101T000003Z-c"} {
		fixtureRun(t, srv.cells[0].runsDir, id, "done", rep, nil)
	}
	if n := getBoard(t, ts.URL, "", "limit=0").Metrics.Runs; n != 3 {
		t.Fatalf("limit=0 counted %d runs, want 3", n)
	}
	b := getBoard(t, ts.URL, "", "limit=2")
	if b.Metrics.Runs != 2 {
		t.Fatalf("limit=2 counted %d runs, want 2", b.Metrics.Runs)
	}
	// Newest-first: the window keeps the two newest ids.
	card := cardFor(t, b.Cells[0], "")
	if len(card.Runs) != 2 || card.Runs[0].ID != "20260101T000003Z-c" {
		t.Fatalf("limit=2 window = %+v, want the two newest runs", card.Runs)
	}
	for _, bad := range []string{"limit=-1", "limit=abc"} {
		if code := doJSON(t, "GET", ts.URL+"/board?"+bad, "", nil, nil); code != http.StatusBadRequest {
			t.Fatalf("GET /board?%s: status %d, want 400", bad, code)
		}
	}
}

// TestBoardAddsNoMutatingEndpoint: the board is read-only and gated exactly like
// every other data route. Both halves of the pinned "no new mutating endpoint"
// promise — the route rejects every write method, and the page it feeds still
// has exactly one POST helper, reachable only from a parked inbox entry.
func TestBoardAddsNoMutatingEndpoint(t *testing.T) {
	const tok = "board-token-value"
	_, repo := makeGoRepo(t)
	_, ts := newTestServer(t, tok, repo)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		code := doJSON(t, m, ts.URL+"/board", tok, map[string]any{"column": "landed"}, nil)
		if code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /board: status %d, want 405 — the board must not accept a write", m, code)
		}
	}
	if code := doJSON(t, "GET", ts.URL+"/board", "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("GET /board without the token: status %d, want 401", code)
	}
	if code := doJSON(t, "GET", ts.URL+"/board", tok, nil, nil); code != http.StatusOK {
		t.Fatalf("GET /board with the token: status %d, want 200", code)
	}
	if n := strings.Count(string(uiHTML), `method: "POST"`); n != 1 {
		t.Fatalf("ui.html issues %d POSTs; the board tab must add none", n)
	}
}

// TestBoardUIRendersCraftedContentAsData drives content an attacker controls —
// an imported GitHub issue's body becomes an intent goal verbatim, and a task id
// becomes a branch name — through GET /board, and asserts the daemon hands both
// to the page as JSON string data with no HTML sink anywhere on the page to put
// them in. The CSP is asserted byte-identical: the board tab needed no
// relaxation, because same-origin GETs were already covered.
func TestBoardUIRendersCraftedContentAsData(t *testing.T) {
	requirePOSIXShell(t)
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	_, ts := newTestServer(t, "", repo)

	const craftedGoal = `</script><img src=x onerror=alert(1)>`
	const craftedID = `t1<img/src=x/onerror=alert(1)>`
	writeIntent(t, repo, "crafted", "goal = "+craftedGoal+"\n")

	var created struct {
		RunID string `json:"runId"`
	}
	if c := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Base:  "main",
		Tasks: []taskSpec{taskWrite(t, craftedID, map[string]string{"alpha.txt": "a\n"})},
		Agent: agent,
	}, &created); c != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", c)
	}
	pollRun(t, ts, "", created.RunID)

	// The crafted strings survive to the client verbatim, as JSON data.
	raw, _ := rawGet(t, ts.URL+"/board", "")
	if raw.StatusCode != http.StatusOK {
		t.Fatalf("GET /board status %d", raw.StatusCode)
	}
	b := getBoard(t, ts.URL, "", "")
	if got := cardFor(t, b.Cells[0], "crafted").Goal; got != craftedGoal {
		t.Fatalf("intent goal reached the client as %q, want the crafted text verbatim", got)
	}
	if ct := raw.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("/board content-type %q — crafted content must never be served as HTML", ct)
	}

	// The page has no HTML sink at all, so there is nowhere for that text to be
	// parsed as markup.
	page := string(uiHTML)
	for _, sink := range []string{".innerHTML", "insertAdjacentHTML", "outerHTML", "document.write"} {
		if strings.Contains(page, sink) {
			t.Fatalf("ui.html reaches for %s; board data must render via textContent only", sink)
		}
	}
	if !strings.Contains(page, `function renderCard`) || !strings.Contains(page, `el("div", "goal", it.goal)`) {
		t.Fatal("ui.html no longer renders an intent goal through el()'s textContent")
	}

	// CSP byte-identical to what it was before this feature: a same-origin GET
	// was already permitted by connect-src 'self'.
	resp, _ := rawGet(t, ts.URL+"/ui", "")
	const wantCSP = "default-src 'none'; connect-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'"
	if got := resp.Header.Get("Content-Security-Policy"); got != wantCSP {
		t.Fatalf("CSP changed:\n got %q\nwant %q", got, wantCSP)
	}
}

// TestBoardSurvivesUnparseableIntents: an intents directory that will not parse
// costs the intent cards and says so — it never costs the runs, which come from
// the journal and do not depend on that directory at all.
func TestBoardSurvivesUnparseableIntents(t *testing.T) {
	_, repo := makeGoRepo(t)
	srv, ts := newTestServer(t, "", repo)
	fixtureRun(t, srv.cells[0].runsDir, "20260101T000001Z-a", "done",
		runReport{BaseSHA: "b", AgentCmd: "x", Integrate: integrateJSON{FinalSHA: "f"}}, nil)
	writeIntent(t, repo, "broken", "nosuchkey = 1\n")

	b := getBoard(t, ts.URL, "", "")
	if b.Cells[0].IntentsError == "" {
		t.Fatal("a malformed intents/ directory was swallowed; the board must report why the cards are missing")
	}
	if c := cardFor(t, b.Cells[0], ""); len(c.Runs) != 1 {
		t.Fatalf("the journal's runs vanished with the intents error: %+v", c)
	}
}

// TestAgentUsageFileIngested drives a REAL run whose agent writes the optional
// SIGBOUND_USAGE_FILE, and asserts the numbers reach usage.json and the board's
// cost metric. It also covers the two ways the seam must stay silent: a garbage
// file and a negative-cost file are both skipped without failing anything.
func TestAgentUsageFileIngested(t *testing.T) {
	requirePOSIXShell(t)
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	srv, ts := newTestServer(t, "", repo)

	// The agent writes its usage blob first, then does its real work. The blob
	// carries an unknown vendor field, which must be ignored rather than reject
	// the record.
	usageAgent := `printf '{"inputTokens":1200,"outputTokens":340,"costUsd":0.75,"vendorThing":"x"}' > "$SIGBOUND_USAGE_FILE"; ` + agent
	var created struct {
		RunID string `json:"runId"`
	}
	if c := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Base:  "main",
		Tasks: []taskSpec{taskWrite(t, "cost1", map[string]string{"alpha.txt": "a\n"})},
		Agent: usageAgent,
	}, &created); c != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", c)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "done" {
		t.Fatalf("run status %q: %s", final.Status, final.Error)
	}
	if final.Usage == nil {
		t.Fatal("no usage record for a completed run")
	}
	u := final.Usage
	if u.InputTokens != 1200 || u.OutputTokens != 340 || u.CostUSD != 0.75 || u.CostAgents != 1 {
		t.Fatalf("ingested usage = %+v, want 1200/340/0.75 from 1 agent", u)
	}
	// The file itself is where the seam said it would be, inside the run dir and
	// outside the agent's worktree (so it can never be a stray in-lane write).
	dir := filepath.Join(srv.cells[0].runsDir, created.RunID)
	if _, err := os.Stat(filepath.Join(dir, agentUsagePrefix+"cost1.json")); err != nil {
		t.Fatalf("agent usage file not in the run dir: %v", err)
	}
	// Agent wall time is measured, not ingested: a real agent that ran took a
	// non-zero number of milliseconds.
	if u.AgentWallMs <= 0 {
		t.Fatalf("agentWallMs = %d for a run whose agent really ran", u.AgentWallMs)
	}

	m := getBoard(t, ts.URL, "", "limit=0").Metrics
	if m.CostRuns != 1 || m.CostUSD != 0.75 || m.CostLanded != 1 {
		t.Fatalf("board cost metrics = %+v, want 1 run / 0.75 / 1 landed", m)
	}

	// Unusable files are skipped silently: they change no counter, and
	// CostAgents is what makes the absence visible.
	for _, body := range []string{"{not json", `{"costUsd":-5}`, `{"inputTokens":-1}`} {
		var got UsageJSON
		fixture := t.TempDir()
		if err := os.WriteFile(filepath.Join(fixture, agentUsagePrefix+"x.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		ingestAgentUsage(fixture, &got)
		if got.CostAgents != 0 || got.CostUSD != 0 || got.InputTokens != 0 {
			t.Fatalf("unusable usage file %q was ingested as %+v", body, got)
		}
	}
	// And an absent file is the normal case: nothing recorded, no error.
	var none UsageJSON
	ingestAgentUsage(t.TempDir(), &none)
	if none != (UsageJSON{}) {
		t.Fatalf("no usage file at all produced %+v, want a zero record", none)
	}
}

// TestAgentUsageSumsSaturate: the per-file guard rejects an infinite cost, but
// two FINITE costs can still sum to +Inf — and +Inf is not encodable JSON, so
// the sum would take out `usage.json` at the per-run layer and the whole /board
// response (200, empty body) at the cross-run layer. Both layers must saturate.
func TestAgentUsageSumsSaturate(t *testing.T) {
	// Per-run layer: two files whose finite values sum past both limits.
	dir := t.TempDir()
	for _, name := range []string{"a", "b"} {
		body := `{"costUsd":1e308,"inputTokens":9223372036854775807,"outputTokens":9223372036854775807}`
		if err := os.WriteFile(filepath.Join(dir, agentUsagePrefix+name+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var u UsageJSON
	ingestAgentUsage(dir, &u)
	if u.CostAgents != 2 {
		t.Fatalf("costAgents = %d, want 2 — both files are individually valid", u.CostAgents)
	}
	if math.IsInf(u.CostUSD, 0) || math.IsNaN(u.CostUSD) {
		t.Fatalf("per-run cost sum = %v; an unencodable number must never reach usage.json", u.CostUSD)
	}
	if u.InputTokens != math.MaxInt64 || u.OutputTokens != math.MaxInt64 {
		t.Fatalf("token sums = %d/%d, want saturation at %d — a wrapped counter reads as a negative token count",
			u.InputTokens, u.OutputTokens, int64(math.MaxInt64))
	}
	if _, err := json.Marshal(u); err != nil {
		t.Fatalf("the summed record does not encode: %v", err)
	}

	// Cross-run layer: two runs each carrying the clamped maximum.
	_, repo := makeGoRepo(t)
	srv, ts := newTestServer(t, "", repo)
	rep := runReport{BaseSHA: "base0", AgentCmd: "x", Integrate: integrateJSON{FinalSHA: "final"}}
	for _, id := range []string{"20260101T000001Z-a", "20260101T000002Z-b"} {
		fixtureRun(t, srv.cells[0].runsDir, id, "done", rep,
			&UsageJSON{Landed: true, CostUSD: math.MaxFloat64, CostAgents: 1,
				InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64, AgentWallMs: math.MaxInt64})
	}
	// getBoard fails the test if the body will not decode, which is exactly the
	// symptom: the 200 header is already on the wire when Encode gives up.
	m := getBoard(t, ts.URL, "", "limit=0").Metrics
	if math.IsInf(m.CostUSD, 0) || math.IsNaN(m.CostUSD) || m.CostUSD <= 0 {
		t.Fatalf("board cost = %v, want a huge but finite number", m.CostUSD)
	}
	if m.InputTokens < 0 || m.AgentWallMs < 0 {
		t.Fatalf("wrapped counters reached the board: inputTokens=%d agentWallMs=%d", m.InputTokens, m.AgentWallMs)
	}
}

// TestAgentUsageFileIsBounded: the file is written by the AGENT, so its size is
// the agent's choice and the daemon's allocation must not be. An oversized file
// is refused loudly — silence there hides why a run's cost vanished — and the
// well-formed file beside it is still ingested.
func TestAgentUsageFileIsBounded(t *testing.T) {
	dir := t.TempDir()
	big := `{"costUsd":9,"pad":"` + strings.Repeat("a", agentUsageMaxBytes) + `"}`
	if err := os.WriteFile(filepath.Join(dir, agentUsagePrefix+"big.json"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, agentUsagePrefix+"ok.json"), []byte(`{"costUsd":0.25,"inputTokens":7}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var u UsageJSON
	log := captureStderr(t, func() { ingestAgentUsage(dir, &u) })
	if u.CostAgents != 1 || u.CostUSD != 0.25 || u.InputTokens != 7 {
		t.Fatalf("ingested %+v, want only the small file (1 agent / 0.25 / 7 tokens)", u)
	}
	if !strings.Contains(log, agentUsagePrefix+"big.json") {
		t.Fatalf("the oversized file was dropped silently; stderr said %q", log)
	}
}

// TestPresetOfAgentCmd: the report records the RESOLVED command, so the preset
// name can only be recovered by an exact reverse match — and anything else must
// say "custom" rather than guess.
func TestPresetOfAgentCmd(t *testing.T) {
	for name, expanded := range agentPresets {
		if got := presetOfAgentCmd(expanded); got != name {
			t.Errorf("presetOfAgentCmd(%q) = %q, want %q", expanded, got, name)
		}
	}
	for _, cmd := range []string{"", "./my-agent", agentPresets["claude"] + " --extra"} {
		if got := presetOfAgentCmd(cmd); got != "custom" {
			t.Errorf("presetOfAgentCmd(%q) = %q, want custom", cmd, got)
		}
	}
}

// TestBoardRunsMirrorSigLog: the board's rows ARE `sig log`'s rows — same reader,
// so the two surfaces cannot drift.
func TestBoardRunsMirrorSigLog(t *testing.T) {
	_, repo := makeGoRepo(t)
	srv, ts := newTestServer(t, "", repo)
	fixtureRun(t, srv.cells[0].runsDir, "20260101T000001Z-a", "done", runReport{
		BaseSHA:   "base0",
		AgentCmd:  "./a",
		StartedAt: "2026-01-01T00:00:01Z",
		PerAgent:  []perAgentJSON{{ID: "t1", Branch: "agent/t1", OK: true}},
		Integrate: integrateJSON{Landed: []string{"agent/t1"}, FinalSHA: "final1"},
		Verify:    verifyJSON{Ran: true, OK: true},
	}, nil)
	want, _ := scanRuns(srv.cells[0].runsDir, 50)
	got := cardFor(t, getBoard(t, ts.URL, "", "").Cells[0], "").Runs
	if mustJSON(t, got) != mustJSON(t, want) {
		t.Fatalf("board rows differ from sig log's\n got %s\nwant %s", mustJSON(t, got), mustJSON(t, want))
	}
	// And the same rows are what GET /log serves.
	var lg struct {
		Cells []logCellRuns `json:"cells"`
	}
	if code := doJSON(t, "GET", ts.URL+"/log", "", nil, &lg); code != http.StatusOK {
		t.Fatalf("GET /log: %d", code)
	}
	if mustJSON(t, lg.Cells[0].Runs) != mustJSON(t, got) {
		t.Fatal("GET /board and GET /log disagree about the same journal")
	}
}

// TestBoardJSONShapeIsStable guards the field names the UI and any curl consumer
// read. A rename here is a breaking change to a documented surface.
func TestBoardJSONShapeIsStable(t *testing.T) {
	data, err := json.Marshal(boardResponse{Cells: []boardCell{{Cell: "c", Repo: "r"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"cells"`, `"metrics"`, `"runs"`, `"landed"`, `"verifyRan"`,
		`"verifyPassed"`, `"bisectRan"`, `"bisectSalvaged"`, `"flaggedRuns"`, `"landedWallMs"`,
		`"landedWallRuns"`, `"agentWallMs"`, `"agentWallLanded"`, `"costUsd"`, `"presets"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("GET /board response is missing documented key %s: %s", key, data)
		}
	}
}
