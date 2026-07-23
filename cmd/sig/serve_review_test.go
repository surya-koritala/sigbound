package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// modifyConflictAgent rewrites shared.txt with the task id, so two tasks produce
// distinct whole-file rewrites — a real modify/modify conflict git can't
// auto-resolve. With no resolver the SECOND task's branch (agent/t2) is flagged
// (fold lands branches in request order; the first to touch a path wins).
const modifyConflictAgent = `printf '%s\n' "$SIGBOUND_TASK_ID" > shared.txt`

// driveFlaggedRun runs two tasks (t1, t2) with agent against a fresh repo over
// serve, waits for the run to finish, and asserts it flagged at least one
// branch. Returns the httptest server and the completed run response.
func driveFlaggedRun(t *testing.T, token, agent string) (*httptest.Server, runStatusResponse) {
	t.Helper()
	_, repo := makeGoRepo(t)
	_, ts := newTestServer(t, token, repo)
	var created struct {
		RunID string `json:"runId"`
	}
	code := doJSON(t, "POST", ts.URL+"/runs", token, runRequest{
		Cell:  repo,
		Base:  "main",
		Tasks: []taskSpec{{ID: "t1", Prompt: "x"}, {ID: "t2", Prompt: "x"}},
		Agent: agent,
	}, &created)
	if code != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", code)
	}
	final := pollRun(t, ts, token, created.RunID)
	if final.Status != "done" {
		t.Fatalf("run status %q (error=%q), want done", final.Status, final.Error)
	}
	if final.Report == nil || len(final.Report.Integrate.Flagged) == 0 {
		t.Fatalf("expected a flagged branch, got report %+v", final.Report)
	}
	return ts, final
}

// rawGet issues a GET WITHOUT following redirects, returning the response and
// its body verbatim — so a ServeMux path-clean redirect shows as its real 3xx
// instead of the followed destination, and a leak can be checked against the raw
// bytes.
func rawGet(t *testing.T, url, token string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func TestServeFlaggedListsBranchesAndPaths(t *testing.T) {
	ts, final := driveFlaggedRun(t, "", modifyConflictAgent)
	var resp flaggedListResponse
	code := doJSON(t, "GET", ts.URL+"/runs/"+final.ID+"/flagged", "", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("GET flagged status %d, want 200", code)
	}
	if resp.RunID != final.ID {
		t.Fatalf("runId %q, want %q", resp.RunID, final.ID)
	}
	if len(resp.Flagged) != 1 {
		t.Fatalf("flagged branches = %d, want 1: %+v", len(resp.Flagged), resp.Flagged)
	}
	f := resp.Flagged[0]
	if f.Branch != "agent/t2" {
		t.Fatalf("flagged branch %q, want agent/t2", f.Branch)
	}
	if len(f.Paths) != 1 || f.Paths[0] != "shared.txt" {
		t.Fatalf("conflict paths %v, want [shared.txt]", f.Paths)
	}
}

func TestServeFlaggedThreeSides(t *testing.T) {
	ts, final := driveFlaggedRun(t, "", modifyConflictAgent)
	var d flaggedDetailResponse
	code := doJSON(t, "GET", ts.URL+"/runs/"+final.ID+"/flagged/agent/t2/shared.txt", "", nil, &d)
	if code != http.StatusOK {
		t.Fatalf("GET three-sides status %d, want 200", code)
	}
	if d.Path != "shared.txt" {
		t.Fatalf("path %q, want shared.txt", d.Path)
	}
	if d.Base == nil || d.Ours == nil || d.Theirs == nil {
		t.Fatalf("no side should be null for a modify/modify conflict: %+v", d)
	}
	// theirs = the flagged branch's content; ours = the landed (t1) content;
	// base = the original file the run forked from.
	if *d.Theirs != "t2\n" {
		t.Fatalf("theirs = %q, want t2", *d.Theirs)
	}
	if *d.Ours != "t1\n" {
		t.Fatalf("ours = %q, want the landed t1 content", *d.Ours)
	}
	if !strings.Contains(*d.Base, "shared base line") {
		t.Fatalf("base = %q, want the original shared.txt", *d.Base)
	}
	if d.BaseSHA == "" || d.BaseSHA != final.Report.BaseSHA {
		t.Fatalf("baseSHA %q, want report baseSHA %q", d.BaseSHA, final.Report.BaseSHA)
	}
}

func TestServeFlaggedNullSide(t *testing.T) {
	// t1 modifies shared.txt, t2 deletes it: a delete/modify conflict. t2 is
	// flagged, so its side (theirs) has no blob for the path => JSON null.
	agent := `if [ "$SIGBOUND_TASK_ID" = t2 ]; then rm -f shared.txt; else printf 'v1\n' > shared.txt; fi`
	ts, final := driveFlaggedRun(t, "", agent)
	var d flaggedDetailResponse
	code := doJSON(t, "GET", ts.URL+"/runs/"+final.ID+"/flagged/agent/t2/shared.txt", "", nil, &d)
	if code != http.StatusOK {
		t.Fatalf("GET three-sides status %d, want 200", code)
	}
	if d.Theirs != nil {
		t.Fatalf("theirs should be null (branch deleted the path), got %q", *d.Theirs)
	}
	if d.Base == nil || d.Ours == nil {
		t.Fatalf("base and ours should be present: %+v", d)
	}
	if *d.Ours != "v1\n" {
		t.Fatalf("ours = %q, want the landed v1 content", *d.Ours)
	}
}

// TestServeFlaggedPathAllowlistSafety is the key safety property: the detail
// endpoint reads ONLY (branch, path) pairs the run actually flagged. Anything
// else — a real repo file that wasn't flagged, a landed (non-flagged) branch's
// path, a traversal, an absolute-looking path, an unknown branch — must be
// refused and read NOTHING.
func TestServeFlaggedPathAllowlistSafety(t *testing.T) {
	ts, final := driveFlaggedRun(t, "", modifyConflictAgent)
	base := ts.URL + "/runs/" + final.ID + "/flagged/"
	cases := []string{
		"agent/t2/go.mod",              // real, readable repo file — but NOT flagged
		"agent/t1/shared.txt",          // real branch+path, but t1 landed (not flagged)
		"agent/t2/../../../etc/passwd", // traversal
		"agent/t2//etc/passwd",         // absolute-ish (leading slash after branch)
		"agent/t2/../../../../../../../../etc/passwd",
		"agent/nope/shared.txt", // unknown branch
	}
	for _, c := range cases {
		resp, body := rawGet(t, base+c, "")
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("path %q returned 200 — the allowlist must refuse it", c)
		}
		for _, secret := range []string{"module example.com/e2e", "root:", "shared base line"} {
			if strings.Contains(body, secret) {
				t.Fatalf("path %q leaked file content %q: %s", c, secret, body)
			}
		}
	}
}

func TestServeFlaggedXSSReturnedVerbatim(t *testing.T) {
	// A conflicted file whose contents try to break out of a <script> context.
	// The endpoint returns it verbatim as JSON data; escaping is the client's
	// job. Assert the payload round-trips and that the embedded page renders
	// file data via textContent, never innerHTML.
	xssAgent := `printf '</script><img src=x onerror=alert(1)>%s\n' "$SIGBOUND_TASK_ID" > shared.txt`
	ts, final := driveFlaggedRun(t, "", xssAgent)
	var d flaggedDetailResponse
	code := doJSON(t, "GET", ts.URL+"/runs/"+final.ID+"/flagged/agent/t2/shared.txt", "", nil, &d)
	if code != http.StatusOK {
		t.Fatalf("GET three-sides status %d, want 200", code)
	}
	const payload = "</script><img src=x onerror=alert(1)>"
	if d.Theirs == nil || !strings.Contains(*d.Theirs, payload) {
		t.Fatalf("theirs should carry the XSS payload verbatim, got %v", d.Theirs)
	}

	page := string(uiHTML)
	if strings.Contains(page, ".innerHTML") {
		t.Fatal("ui.html uses innerHTML — agent-generated file data must render via textContent only")
	}
	if !strings.Contains(page, "textContent") {
		t.Fatal("ui.html does not use textContent to render file data")
	}
}

func TestServeUIPageIsSelfContained(t *testing.T) {
	_, repo := makeGoRepo(t)
	_, ts := newTestServer(t, "", repo)
	for _, path := range []string{"/ui", "/ui/"} {
		resp, body := rawGet(t, ts.URL+path, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status %d, want 200", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("GET %s content-type %q, want text/html", path, ct)
		}
		if !strings.Contains(body, "<title>Sigbound") {
			t.Fatalf("GET %s is not the review page", path)
		}
		// CSP-friendly: a strict policy AND no external asset of any kind.
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
			t.Fatalf("GET %s missing strict CSP, got %q", path, csp)
		}
		low := strings.ToLower(body)
		for _, ext := range []string{`src="http`, `src='http`, `href="http`, `href='http`, "//cdn", "cdnjs", "unpkg", "googleapis", "http://", "https://"} {
			if strings.Contains(low, ext) {
				t.Fatalf("GET %s references external asset %q — must be fully self-contained/air-gapped", path, ext)
			}
		}
	}
}

func TestServeReviewAuthApplies(t *testing.T) {
	const tok = "review-secret-token-value"
	ts, final := driveFlaggedRun(t, tok, modifyConflictAgent)
	routes := []string{
		"/ui",
		"/ui/",
		"/runs/" + final.ID + "/flagged",
		"/runs/" + final.ID + "/flagged/agent/t2/shared.txt",
	}
	for _, path := range routes {
		if code := doJSON(t, "GET", ts.URL+path, "", nil, nil); code != http.StatusUnauthorized {
			t.Fatalf("GET %s without token: status %d, want 401", path, code)
		}
		if code := doJSON(t, "GET", ts.URL+path, tok, nil, nil); code != http.StatusOK {
			t.Fatalf("GET %s with token: status %d, want 200", path, code)
		}
	}
}
