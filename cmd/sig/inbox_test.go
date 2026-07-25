package main

// Coverage for GET /inbox and the review UI's Inbox tab (issue #109).
//
// The `parked` entry is always a REAL parked run (newParkFixture) — it is the
// only actionable type, so its record has to be the one the daemon actually
// wrote. The other four types are seeded as run directories with a hand-written
// report.json: what is under test here is the AGGREGATION (which reports raise
// which entries, and how they filter), not how a report comes to carry a
// droppedByBisect list, which the bisect/repair tests already establish.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// seedRun writes a synthetic completed run directory into runsDir and returns
// its id.
func seedRun(t *testing.T, runsDir string, rep runReport) string {
	t.Helper()
	id := newRunID()
	dir := filepath.Join(runsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if rep.StartedAt == "" {
		rep.StartedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	}
	writeRunReport(dir, rep)
	writeRunStatus(dir, "done", "")
	return id
}

// getInbox issues GET /inbox with the given query and returns the entries.
func getInbox(t *testing.T, url, query string) []inboxEntry {
	t.Helper()
	var body struct {
		Entries []inboxEntry `json:"entries"`
	}
	if code := doJSON(t, "GET", url+"/inbox"+query, "", nil, &body); code != http.StatusOK {
		t.Fatalf("GET /inbox%s: status %d", query, code)
	}
	return body.Entries
}

func typesOf(entries []inboxEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if !seen[e.Type] {
			seen[e.Type] = true
			out = append(out, e.Type)
		}
	}
	sort.Strings(out)
	return out
}

// TestInboxListsEveryEntryTypeAndFilters is acceptance #6: one cell holding one
// of each of the five entry types, all five listed, and both filters honored.
func TestInboxListsEveryEntryTypeAndFilters(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	runsDir := f.srv.cells[0].runsDir

	flaggedID := seedRun(t, runsDir, runReport{
		Integrate: integrateJSON{Flagged: []flaggedJSON{{Branch: "agent/conflicted", Paths: []string{"shared.txt"}}}},
	})
	droppedID := seedRun(t, runsDir, runReport{
		Integrate: integrateJSON{DroppedByBisect: []string{"agent/breaks-the-build"}},
	})
	repairID := seedRun(t, runsDir, runReport{
		Verify: verifyJSON{Ran: true, OK: false, Repairs: []repairAttemptJSON{{N: 1}, {N: 2}}},
	})
	auditID := seedRun(t, runsDir, runReport{
		Audit:     true,
		Integrate: integrateJSON{FinalSHA: "0123456789abcdef0123456789abcdef01234567"},
	})

	entries := getInbox(t, f.ts.URL, "")
	got := typesOf(entries)
	want := []string{inboxAudit, inboxDropped, inboxFlagged, inboxParked, inboxRepairFailed}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("inbox types = %v, want all five %v", got, want)
	}

	// Every entry carries the common shape.
	for _, e := range entries {
		if e.CellID == "" || e.RunID == "" || e.Age == "" || e.Summary == "" {
			t.Fatalf("entry is missing a required field: %+v", e)
		}
		if e.Links["run"] != "/runs/"+e.RunID {
			t.Fatalf("entry %s has no run link: %+v", e.RunID, e.Links)
		}
	}

	// Newest first: run ids are timestamp-prefixed, so descending id order is
	// chronological.
	for i := 1; i < len(entries); i++ {
		if entries[i-1].RunID < entries[i].RunID {
			t.Fatalf("inbox is not newest-first: %s before %s", entries[i-1].RunID, entries[i].RunID)
		}
	}

	// ?type= filters to exactly that type, and names the right run.
	for _, tc := range []struct{ typ, runID string }{
		{inboxParked, f.runID},
		{inboxFlagged, flaggedID},
		{inboxDropped, droppedID},
		{inboxRepairFailed, repairID},
		{inboxAudit, auditID},
	} {
		filtered := getInbox(t, f.ts.URL, "?type="+tc.typ)
		if len(filtered) != 1 {
			t.Fatalf("?type=%s returned %d entries, want 1: %+v", tc.typ, len(filtered), filtered)
		}
		if filtered[0].Type != tc.typ || filtered[0].RunID != tc.runID {
			t.Fatalf("?type=%s returned %+v, want run %s", tc.typ, filtered[0], tc.runID)
		}
	}

	// ?limit= caps the list; an unknown ?type= is a loud 400, not an empty list
	// that looks like "nothing to do".
	if got := getInbox(t, f.ts.URL, "?limit=2"); len(got) != 2 {
		t.Fatalf("?limit=2 returned %d entries", len(got))
	}
	if code := doJSON(t, "GET", f.ts.URL+"/inbox?type=nonsense", "", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("?type=nonsense: status %d, want 400", code)
	}
	if code := doJSON(t, "GET", f.ts.URL+"/inbox?limit=-1", "", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("?limit=-1: status %d, want 400", code)
	}
}

// TestInboxParkedEntryCarriesActionableDetail: the one actionable type carries
// what a human needs before deciding — why it parked, which paths triggered it
// under which glob, how many verify cycles it has been through — plus the two
// mutating links. No other type gets them.
func TestInboxParkedEntryCarriesActionableDetail(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths+"ack-timeout = 72h\n")
	runsDir := f.srv.cells[0].runsDir
	seedRun(t, runsDir, runReport{
		Integrate: integrateJSON{Flagged: []flaggedJSON{{Branch: "agent/conflicted", Paths: []string{"shared.txt"}}}},
	})

	for _, e := range getInbox(t, f.ts.URL, "") {
		if e.Type != inboxParked {
			if e.Links["ack"] != "" || e.Links["reject"] != "" {
				t.Fatalf("%s entry offered a mutating link: %+v", e.Type, e.Links)
			}
			continue
		}
		if e.Reason != parkReasonAckPaths {
			t.Fatalf("parked reason %q, want %q", e.Reason, parkReasonAckPaths)
		}
		if e.MatchedPaths["auth/token.go"] != "auth/**" {
			t.Fatalf("matchedPaths %v, want auth/token.go -> auth/**", e.MatchedPaths)
		}
		if e.Attempts != 1 {
			t.Fatalf("attempts %d, want 1 (the park's own verify)", e.Attempts)
		}
		if e.ExpiresAt == "" {
			t.Fatal("a park with an ack-timeout reported no expiry")
		}
		if e.Links["ack"] != "/runs/"+f.runID+"/ack" || e.Links["reject"] != "/runs/"+f.runID+"/reject" {
			t.Fatalf("parked entry links %+v", e.Links)
		}
		// The three-pane diff of the matched paths is the EXISTING flagged
		// endpoint — a parked branch is in the run's flagged set, so no second
		// viewer exists to drift from it.
		if e.Links["flagged"] != "/runs/"+f.runID+"/flagged" {
			t.Fatalf("parked entry does not link to the diff: %+v", e.Links)
		}
		var detail flaggedDetailResponse
		if c := doJSON(t, "GET", f.ts.URL+"/runs/"+f.runID+"/flagged/agent/held/auth/token.go", "", nil, &detail); c != http.StatusOK {
			t.Fatalf("three-pane diff of a matched path: status %d", c)
		}
		if detail.Theirs == nil || !strings.Contains(*detail.Theirs, "func Token()") {
			t.Fatalf("the parked path's diff has no `theirs` side: %+v", detail)
		}
	}
}

// TestInboxDoesNotDoubleListAParkedBranch: a parked group is also in the run's
// flagged set (that is how the diff viewer reaches it), but it must appear as
// ONE actionable parked entry, not additionally as an unactionable flagged one.
func TestInboxDoesNotDoubleListAParkedBranch(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	rep, err := readRunReport(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Integrate.Flagged) == 0 {
		t.Fatal("a parked run recorded no flagged entry; the diff viewer needs one")
	}
	for _, e := range getInbox(t, f.ts.URL, "") {
		if e.RunID == f.runID && e.Type == inboxFlagged {
			t.Fatalf("the parked run is also listed as flagged: %+v", e)
		}
	}
}

// ---- acceptance #7: the UI ----

// TestUIInboxControlsAreParkedOnlyAndInert covers the review page's half of the
// contract: the two mutating controls are gated on the parked type, the CSP is
// byte-identical to what it was before parking existed, and crafted content
// (HTML in a branch name and in a triggering path) reaches the browser as data.
func TestUIInboxControlsAreParkedOnlyAndInert(t *testing.T) {
	page := string(uiHTML)

	// The ONLY sink for server data stays textContent — an inbox summary is just
	// as agent-influenced as a conflicted file's bytes.
	if strings.Contains(page, ".innerHTML") || strings.Contains(page, "insertAdjacentHTML") ||
		strings.Contains(page, "document.write") {
		t.Fatal("ui.html reaches for an HTML sink; inbox data must render via textContent only")
	}
	// Ack/Reject are built inside the parked branch and nowhere else.
	if !strings.Contains(page, `e.type === "parked"`) {
		t.Fatal("ui.html does not gate its mutating controls on the parked entry type")
	}
	before, _, ok := strings.Cut(page, `e.type === "parked"`)
	if !ok {
		t.Fatal("could not split ui.html on the parked gate")
	}
	if strings.Contains(before, "apiPost(") && !strings.Contains(before, "async function apiPost") {
		t.Fatal("ui.html calls apiPost before the parked gate; only a parked entry may mutate")
	}
	// Exactly one POST helper, and the page never POSTs anywhere else.
	if n := strings.Count(page, `method: "POST"`); n != 1 {
		t.Fatalf("ui.html issues %d POSTs; there must be exactly one helper", n)
	}

	// The CSP is unchanged by this feature: connect-src 'self' already covers a
	// same-origin POST, so adding the Inbox tab needed no relaxation at all.
	requirePOSIXShell(t)
	_, repo := makeGoRepo(t)
	_, tsrv := newTestServer(t, "", repo)
	resp, _ := rawGet(t, tsrv.URL+"/ui", "")
	const wantCSP = "default-src 'none'; connect-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'"
	if got := resp.Header.Get("Content-Security-Policy"); got != wantCSP {
		t.Fatalf("CSP changed:\n got %q\nwant %q", got, wantCSP)
	}
}

// TestInboxRendersCraftedContentAsData drives a real run whose branch name and
// ack-path both carry HTML, and asserts the daemon hands them to the page as
// JSON string data — never as markup, and never anywhere but a textContent sink.
func TestInboxRendersCraftedContentAsData(t *testing.T) {
	requirePOSIXShell(t)
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	commitPolicy(t, g, repo, "ack-paths = auth/**\n")

	// A git-legal branch component and a git-legal path, both crafted: `<`/`>`
	// are perfectly valid in a ref name and a filename, so this is content a real
	// agent could produce, not a contrived string injected at the API.
	const craftedID = `t1<img/src=x/onerror=alert(1)>`
	const craftedPath = `auth/<script>alert(1)</script>.txt`
	_, tsrv := newTestServer(t, "", repo)
	var created struct {
		RunID string `json:"runId"`
	}
	if c := doJSON(t, "POST", tsrv.URL+"/runs", "", runRequest{
		Cell:   repo,
		Base:   "main",
		Tasks:  []taskSpec{taskWrite(t, craftedID, map[string]string{craftedPath: "secrets\n"})},
		Agent:  agent,
		Verify: "go build ./...",
	}, &created); c != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", c)
	}
	final := pollRunStatus(t, tsrv, "", created.RunID, statusAwaitingAck)
	if final.Park == nil {
		t.Fatalf("crafted run did not park: %+v", final)
	}

	entries := getInbox(t, tsrv.URL, "?type=parked")
	if len(entries) != 1 {
		t.Fatalf("crafted parked run not in the inbox: %+v", entries)
	}
	e := entries[0]
	if e.MatchedPaths[craftedPath] != "auth/**" {
		t.Fatalf("crafted path not carried verbatim: %v", e.MatchedPaths)
	}
	if !slicesContains(e.Branches, "agent/"+craftedID) {
		t.Fatalf("crafted branch not carried verbatim: %v", e.Branches)
	}
	// On the wire it is a JSON string: the angle brackets survive as data (JSON
	// has no markup), which is exactly why the page can render them inertly with
	// textContent.
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var round inboxEntry
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("inbox entry does not round-trip: %v", err)
	}
	if round.MatchedPaths[craftedPath] == "" {
		t.Fatal("crafted path lost in the JSON round trip")
	}
	// And the response is served as JSON, so a browser never parses it as HTML.
	req, _ := http.NewRequest("GET", tsrv.URL+"/inbox", nil)
	got, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	if ct := got.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("GET /inbox content-type %q, want application/json", ct)
	}
}

// TestInboxEmptyWhenNothingWaits: a daemon with no runs answers with an empty
// list, not null — a caller should never have to distinguish the two.
func TestInboxEmptyWhenNothingWaits(t *testing.T) {
	requirePOSIXShell(t)
	_, repo := makeGoRepo(t)
	_, tsrv := newTestServer(t, "", repo)
	var body struct {
		Entries []inboxEntry `json:"entries"`
	}
	if code := doJSON(t, "GET", tsrv.URL+"/inbox", "", nil, &body); code != http.StatusOK {
		t.Fatalf("GET /inbox status %d", code)
	}
	if body.Entries == nil || len(body.Entries) != 0 {
		t.Fatalf("empty inbox = %+v, want []", body.Entries)
	}
}
