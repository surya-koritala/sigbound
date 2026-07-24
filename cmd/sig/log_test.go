package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// logRunsDir is the run-history root for a test repo: <git-common-dir>/sigbound/runs.
func logRunsDir(t *testing.T, g *gitx.Git) string {
	t.Helper()
	common, err := g.GitCommonDir(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(common, "sigbound", "runs")
}

// writeLogRun writes rep as report.json under runsDir/<id>/ — the minimal
// fixture sig log reads back. Fields left zero simply render absent.
func writeLogRun(t *testing.T, runsDir, id string, rep runReport) {
	t.Helper()
	dir := filepath.Join(runsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// hexSHA pads prefix (hex) to a full 40-char object name so a fixture can use
// distinct, valid-looking commit shas without minting real commits.
func hexSHA(prefix string) string {
	return prefix + strings.Repeat("0", 40-len(prefix))
}

// --- AC #1: -sha provenance, one test per landing shape ---

// TestLogSHAOverlayLanding: an overlay run's member commit resolves to its task
// and agent (member-landed); its final integration commit resolves to
// landed-commit naming how many branches combined.
func TestLogSHAOverlayLanding(t *testing.T) {
	g, _, _ := newGCRepo(t)
	runsDir := logRunsDir(t, g)
	ctx := context.Background()
	final, m1, m2 := hexSHA("f1"), hexSHA("a1"), hexSHA("a2")
	writeLogRun(t, runsDir, "20260101T000000Z-aaaa", runReport{
		BaseSHA:  hexSHA("00"),
		Strategy: "overlay",
		AgentCmd: "claude -p",
		PerAgent: []perAgentJSON{
			{ID: "t1", Branch: "agent/t1", SHA: m1, OK: true},
			{ID: "t2", Branch: "agent/t2", SHA: m2, OK: true},
		},
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1", "agent/t2"}, FinalSHA: final},
		Verify:    verifyJSON{Ran: true, OK: true},
	})

	p, ok := resolveProvenance(ctx, g, runsDir, m1)
	if !ok {
		t.Fatal("member commit not resolved")
	}
	if p.Role != "member-landed" || p.TaskID != "t1" || p.Branch != "agent/t1" || p.Agent != "claude -p" || !p.Landed {
		t.Fatalf("member provenance = %+v", p)
	}
	if p.RunID != "20260101T000000Z-aaaa" || p.Source != "manifest" {
		t.Fatalf("member run/source = %q/%q", p.RunID, p.Source)
	}

	pf, ok := resolveProvenance(ctx, g, runsDir, final)
	if !ok || pf.Role != "landed-commit" || !pf.Landed || pf.Members != 2 || pf.Strategy != "overlay" {
		t.Fatalf("final provenance = %+v ok=%v", pf, ok)
	}
}

// TestLogSHAOctopusLanding: a landing whose final commit is a multi-parent merge
// of three agent branches — every member resolves, and the merge commit itself
// resolves to landed-commit. The reader keys off report fields, not topology, so
// this differs from overlay only in the strategy string and branch count.
func TestLogSHAOctopusLanding(t *testing.T) {
	g, _, _ := newGCRepo(t)
	runsDir := logRunsDir(t, g)
	ctx := context.Background()
	merge := hexSHA("de")
	members := map[string]string{"t1": hexSHA("b1"), "t2": hexSHA("b2"), "t3": hexSHA("b3")}
	rep := runReport{
		BaseSHA:   hexSHA("00"),
		Strategy:  "naive",
		AgentCmd:  "codex exec",
		Integrate: integrateJSON{Strategy: "naive", FinalSHA: merge},
		Verify:    verifyJSON{Ran: true, OK: true},
	}
	for id, sha := range members {
		rep.PerAgent = append(rep.PerAgent, perAgentJSON{ID: id, Branch: "agent/" + id, SHA: sha, OK: true})
		rep.Integrate.Landed = append(rep.Integrate.Landed, "agent/"+id)
	}
	writeLogRun(t, runsDir, "20260102T000000Z-bbbb", rep)

	for id, sha := range members {
		p, ok := resolveProvenance(ctx, g, runsDir, sha)
		if !ok || p.Role != "member-landed" || p.TaskID != id || !p.Landed {
			t.Fatalf("member %s provenance = %+v ok=%v", id, p, ok)
		}
	}
	pm, ok := resolveProvenance(ctx, g, runsDir, merge)
	if !ok || pm.Role != "landed-commit" || pm.Members != 3 {
		t.Fatalf("merge provenance = %+v ok=%v", pm, ok)
	}
}

// TestLogSHABisectSalvagedSubset: a bisect run landed one member and dropped
// another. The landed member resolves to member-landed; the DROPPED member is
// fully attributed as member-dropped-by-bisect (its task, agent, run) — never
// "unknown", even though it never landed.
func TestLogSHABisectSalvagedSubset(t *testing.T) {
	g, _, _ := newGCRepo(t)
	runsDir := logRunsDir(t, g)
	ctx := context.Background()
	final, kept, dropped := hexSHA("f9"), hexSHA("c1"), hexSHA("c2")
	writeLogRun(t, runsDir, "20260103T000000Z-cccc", runReport{
		BaseSHA:  hexSHA("00"),
		Strategy: "overlay",
		AgentCmd: "aider",
		PerAgent: []perAgentJSON{
			{ID: "keep", Branch: "agent/keep", SHA: kept, OK: true},
			{ID: "drop", Branch: "agent/drop", SHA: dropped, OK: true},
		},
		Integrate: integrateJSON{
			Strategy: "overlay", Landed: []string{"agent/keep"},
			DroppedByBisect: []string{"agent/drop"}, FinalSHA: final,
		},
		Verify: verifyJSON{Ran: true, OK: true, Bisect: &bisectJSON{Ran: true, LandedGroups: [][]string{{"agent/keep"}}, DroppedGroups: [][]string{{"agent/drop"}}}},
	})

	pk, ok := resolveProvenance(ctx, g, runsDir, kept)
	if !ok || pk.Role != "member-landed" || pk.TaskID != "keep" || !pk.Landed {
		t.Fatalf("kept provenance = %+v ok=%v", pk, ok)
	}
	pd, ok := resolveProvenance(ctx, g, runsDir, dropped)
	if !ok {
		t.Fatal("dropped member not resolved — must be attributed, not unknown")
	}
	if pd.Role != "member-dropped-by-bisect" || pd.TaskID != "drop" || pd.Agent != "aider" || pd.Landed {
		t.Fatalf("dropped provenance = %+v", pd)
	}
}

// TestLogSHAUnknownCommit: a commit sigbound never landed resolves to nothing,
// and `sig log -sha` exits 1 with a clear "not landed by sigbound" line.
func TestLogSHAUnknownCommit(t *testing.T) {
	g, repo, _ := newGCRepo(t)
	runsDir := logRunsDir(t, g)
	writeLogRun(t, runsDir, "20260104T000000Z-dddd", runReport{
		BaseSHA:   hexSHA("00"),
		Integrate: integrateJSON{FinalSHA: hexSHA("f1"), Landed: []string{"agent/t1"}},
		PerAgent:  []perAgentJSON{{ID: "t1", Branch: "agent/t1", SHA: hexSHA("a1"), OK: true}},
		Verify:    verifyJSON{Ran: true, OK: true},
	})

	if _, ok := resolveProvenance(context.Background(), g, runsDir, hexSHA("ee")); ok {
		t.Fatal("unknown commit resolved to a provenance")
	}

	var buf bytes.Buffer
	code, err := runLog(&buf, []string{"-repo", repo, "-sha", hexSHA("ee")})
	if err != nil {
		t.Fatalf("runLog: %v", err)
	}
	if code != exitOperationalError {
		t.Fatalf("exit code = %d, want %d (not landed)", code, exitOperationalError)
	}
	if !strings.Contains(buf.String(), "not landed by sigbound") {
		t.Fatalf("output = %q, want a 'not landed by sigbound' line", buf.String())
	}
}

// TestLogSHANotesFirst: a landing note on a real commit answers provenance even
// when the local run ledger has NO manifest for it (the portable, cross-clone
// path). resolveProvenance must reach the note first and mark source "note".
func TestLogSHANotesFirst(t *testing.T) {
	g, _, base := newGCRepo(t)  // base is a real commit
	runsDir := logRunsDir(t, g) // deliberately empty: no manifests on disk
	ctx := context.Background()
	rep := runReport{
		BaseSHA:   hexSHA("00"),
		Strategy:  "overlay",
		AgentCmd:  "claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: base},
		Verify:    verifyJSON{Ran: true, OK: true},
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", base, data); err != nil {
		t.Fatal(err)
	}

	p, ok := resolveProvenance(ctx, g, runsDir, base)
	if !ok {
		t.Fatal("note-backed commit not resolved")
	}
	if p.Source != "note" || p.Role != "landed-commit" || !p.Landed || p.Strategy != "overlay" {
		t.Fatalf("note provenance = %+v", p)
	}
	if p.RunID != "" {
		t.Fatalf("note provenance RunID = %q, want empty (note is portable, no local dir)", p.RunID)
	}
}

// --- AC #2: newest-first ordering, -limit, laziness ---

func TestLogListNewestFirst(t *testing.T) {
	g, _, _ := newGCRepo(t)
	runsDir := logRunsDir(t, g)
	for _, id := range []string{"20260101T000000Z-a", "20260103T000000Z-c", "20260102T000000Z-b"} {
		writeLogRun(t, runsDir, id, runReport{Integrate: integrateJSON{}})
	}
	rows, _ := scanRuns(runsDir, 0)
	got := []string{rows[0].ID, rows[1].ID, rows[2].ID}
	want := []string{"20260103T000000Z-c", "20260102T000000Z-b", "20260101T000000Z-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want newest-first %v", got, want)
	}
}

// TestLogListLimitLazy: with 200 runs and -limit 5, only the 5 newest dirs are
// read. Proof (file-access proxy): the OLDEST dir carries an unparseable
// report.json — if the scan read all 200 it would count as incomplete; with
// -limit 5 it is never opened, so incomplete stays 0 and only the 5 newest rows
// come back.
func TestLogListLimitLazy(t *testing.T) {
	g, _, _ := newGCRepo(t)
	runsDir := logRunsDir(t, g)
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("id%04d", i)
		dir := filepath.Join(runsDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := []byte("{}")
		if i == 0 { // oldest: corrupt. Reading it would bump the incomplete count.
			body = []byte("{ this is not json")
		}
		if err := os.WriteFile(filepath.Join(dir, "report.json"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rows, incomplete := scanRuns(runsDir, 5)
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	if rows[0].ID != "id0199" {
		t.Fatalf("newest = %q, want id0199", rows[0].ID)
	}
	if incomplete != 0 {
		t.Fatalf("incomplete = %d, want 0 — the corrupt oldest dir must never be read for -limit 5", incomplete)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].ID <= rows[i].ID {
			t.Fatalf("rows not strictly descending at %d: %q then %q", i, rows[i-1].ID, rows[i].ID)
		}
	}
}

// --- AC #3: -json shape pins stable field names ---

func TestLogListJSONShape(t *testing.T) {
	g, repo, _ := newGCRepo(t)
	runsDir := logRunsDir(t, g)
	writeLogRun(t, runsDir, "20260105T120000Z-eeee", runReport{
		BaseSHA:   hexSHA("00"),
		StartedAt: "2026-01-05T12:00:00Z",
		Strategy:  "overlay",
		AgentCmd:  "claude -p",
		Tasks:     []taskSpec{{ID: "t1"}, {ID: "t2"}},
		PerAgent:  []perAgentJSON{{ID: "t1", Branch: "agent/t1", SHA: hexSHA("a1"), OK: true}},
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, Flagged: []flaggedJSON{{Branch: "agent/t2"}}, FinalSHA: hexSHA("f1")},
		Verify:    verifyJSON{Ran: true, OK: true},
	})

	var buf bytes.Buffer
	if code, err := runLog(&buf, []string{"-repo", repo, "-json"}); err != nil || code != exitOK {
		t.Fatalf("runLog -json: code=%d err=%v", code, err)
	}

	// -json is a bare array of run objects (documented shape).
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &arr); err != nil {
		t.Fatalf("output is not a JSON array of run objects: %v\n%s", err, buf.String())
	}
	if len(arr) != 1 {
		t.Fatalf("array len = %d, want 1", len(arr))
	}
	row := arr[0]
	// Stable field names present for a completed landed run.
	for _, k := range []string{"id", "startedAt", "status", "tasks", "agents", "agentCmd", "strategy", "landed", "flagged", "dropped", "verify", "landedSHA"} {
		if _, ok := row[k]; !ok {
			t.Fatalf("field %q missing from -json row: keys=%v", k, keysOf(row))
		}
	}
	// omitempty fields absent when zero-valued.
	for _, k := range []string{"goal", "policyHash", "error", "incomplete"} {
		if _, ok := row[k]; ok {
			t.Fatalf("field %q should be omitted when empty", k)
		}
	}
	// A couple of values, to pin meaning as well as names.
	if string(row["tasks"]) != "2" || string(row["landed"]) != "1" || string(row["flagged"]) != "1" {
		t.Fatalf("tasks/landed/flagged = %s/%s/%s", row["tasks"], row["landed"], row["flagged"])
	}
	var verify string
	_ = json.Unmarshal(row["verify"], &verify)
	if verify != "pass" {
		t.Fatalf("verify = %q, want pass", verify)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// --- AC #4: serve returns the same data as the CLI for the same cell ---

func TestLogServeMatchesCLI(t *testing.T) {
	g, repo, _ := newGCRepo(t)
	runsDir := logRunsDir(t, g)
	writeLogRun(t, runsDir, "20260106T000000Z-aaaa", runReport{
		BaseSHA: hexSHA("00"), StartedAt: "2026-01-06T00:00:00Z", Strategy: "overlay", AgentCmd: "claude -p",
		PerAgent:  []perAgentJSON{{ID: "t1", Branch: "agent/t1", SHA: hexSHA("a1"), OK: true}},
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: hexSHA("f1")},
		Verify:    verifyJSON{Ran: true, OK: true},
	})
	writeLogRun(t, runsDir, "20260107T000000Z-bbbb", runReport{
		BaseSHA: hexSHA("00"), StartedAt: "2026-01-07T00:00:00Z", Strategy: "overlay", AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay"},
	})

	// CLI shape: a bare array of logRow.
	var buf bytes.Buffer
	if code, err := runLog(&buf, []string{"-repo", repo, "-json"}); err != nil || code != exitOK {
		t.Fatalf("runLog: code=%d err=%v", code, err)
	}
	var cliRows []logRow
	if err := json.Unmarshal(buf.Bytes(), &cliRows); err != nil {
		t.Fatalf("decode CLI rows: %v", err)
	}

	// Serve shape: {cells:[{cell,repo,runs:[logRow]}]}.
	_, ts := newTestServer(t, "", repo)
	var srv struct {
		Cells []struct {
			Cell string   `json:"cell"`
			Repo string   `json:"repo"`
			Runs []logRow `json:"runs"`
		} `json:"cells"`
	}
	if code := doJSON(t, "GET", ts.URL+"/log?limit=50", "", nil, &srv); code != http.StatusOK {
		t.Fatalf("GET /log status %d", code)
	}
	if len(srv.Cells) != 1 {
		t.Fatalf("serve cells = %d, want 1", len(srv.Cells))
	}
	if !reflect.DeepEqual(srv.Cells[0].Runs, cliRows) {
		t.Fatalf("serve rows != CLI rows\nserve: %+v\ncli:   %+v", srv.Cells[0].Runs, cliRows)
	}

	// And -sha provenance matches across both surfaces.
	var cliBuf bytes.Buffer
	if code, err := runLog(&cliBuf, []string{"-repo", repo, "-sha", hexSHA("a1"), "-json"}); err != nil || code != exitOK {
		t.Fatalf("runLog -sha: code=%d err=%v", code, err)
	}
	var cliProv provenance
	if err := json.Unmarshal(cliBuf.Bytes(), &cliProv); err != nil {
		t.Fatal(err)
	}
	var srvProv struct {
		Provenance provenance `json:"provenance"`
	}
	if code := doJSON(t, "GET", ts.URL+"/log/sha/"+hexSHA("a1"), "", nil, &srvProv); code != http.StatusOK {
		t.Fatalf("GET /log/sha status %d", code)
	}
	if !reflect.DeepEqual(srvProv.Provenance, cliProv) {
		t.Fatalf("serve provenance != CLI\nserve: %+v\ncli:   %+v", srvProv.Provenance, cliProv)
	}

	// A commit no cell landed is a 404 (the HTTP analogue of exit 1).
	if code := doJSON(t, "GET", ts.URL+"/log/sha/"+hexSHA("ee"), "", nil, nil); code != http.StatusNotFound {
		t.Fatalf("GET /log/sha unknown = %d, want 404", code)
	}
}

// --- AC #5: the ledger is independent of refs (deleted branches still render) ---

func TestLogListLedgerIndependentOfRefs(t *testing.T) {
	g, repoDir, base := newGCRepo(t)
	runsDir := logRunsDir(t, g)
	ctx := context.Background()

	// A real landed branch, recorded in a manifest.
	makeBranchAt(t, g, repoDir, "agent/gone", base, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	tip, err := g.RevParse(ctx, "agent/gone")
	if err != nil {
		t.Fatal(err)
	}
	writeLogRun(t, runsDir, "20260108T000000Z-aaaa", runReport{
		BaseSHA: base, StartedAt: "2026-01-08T00:00:00Z", Strategy: "overlay", AgentCmd: "claude -p",
		PerAgent:  []perAgentJSON{{ID: "gone", Branch: "agent/gone", SHA: tip, OK: true}},
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/gone"}, FinalSHA: hexSHA("f1")},
		Verify:    verifyJSON{Ran: true, OK: true},
	})

	before, _ := scanRuns(runsDir, 0)

	// Delete the branch the run landed, then re-scan: identical.
	if err := g.BranchDelete(ctx, "agent/gone"); err != nil {
		t.Fatal(err)
	}
	after, _ := scanRuns(runsDir, 0)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("row changed when its landed branch was deleted\nbefore: %+v\nafter:  %+v", before, after)
	}

	// Provenance for the (now-danging) tip still resolves from the ledger.
	if p, ok := resolveProvenance(ctx, g, runsDir, tip); !ok || p.TaskID != "gone" {
		t.Fatalf("provenance after branch delete = %+v ok=%v", p, ok)
	}
}

// --- AC #6: corrupt/partial run dirs render an incomplete row, never crash ---

func TestLogListIncompleteRows(t *testing.T) {
	g, repo, _ := newGCRepo(t)
	runsDir := logRunsDir(t, g)

	// One good run.
	writeLogRun(t, runsDir, "20260109T000000Z-good", runReport{
		BaseSHA: hexSHA("00"), Strategy: "overlay", Integrate: integrateJSON{Strategy: "overlay"},
	})
	// One with an unparseable report (crash mid-write).
	torn := filepath.Join(runsDir, "20260109T000001Z-torn")
	if err := os.MkdirAll(torn, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torn, "report.json"), []byte("{ half-written"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One with no files at all (interrupted before any terminal write).
	if err := os.MkdirAll(filepath.Join(runsDir, "20260109T000002Z-empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	rows, incomplete := scanRuns(runsDir, 0)
	if incomplete != 2 {
		t.Fatalf("incomplete = %d, want 2", incomplete)
	}
	inc := map[string]bool{}
	for _, r := range rows {
		inc[r.ID] = r.Incomplete
	}
	if inc["20260109T000000Z-good"] {
		t.Fatal("good run marked incomplete")
	}
	if !inc["20260109T000001Z-torn"] || !inc["20260109T000002Z-empty"] {
		t.Fatalf("corrupt/partial dirs not marked incomplete: %+v", inc)
	}

	// Human list exits 0 and surfaces the count.
	var buf bytes.Buffer
	code, err := runLog(&buf, []string{"-repo", repo})
	if err != nil || code != exitOK {
		t.Fatalf("runLog list: code=%d err=%v", code, err)
	}
	if !strings.Contains(buf.String(), "incomplete") {
		t.Fatalf("list output does not surface the incomplete count:\n%s", buf.String())
	}
}

// --- AC #7: -notes default flips on when a sigbound.policy file is at base ---

// commitPolicyFile writes sigbound.policy into repo and commits it, so the base
// tree (main) carries it.
func commitPolicyFile(t *testing.T, g *gitx.Git, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "sigbound.policy"), []byte("# landing policy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CommitAll(context.Background(), "add landing policy"); err != nil {
		t.Fatal(err)
	}
}

func hasSigboundNote(t *testing.T, repo, sha string) bool {
	t.Helper()
	_, err := exec.Command("git", "-C", repo, "notes", "--ref=sigbound", "show", sha).CombinedOutput()
	return err == nil
}

// TestNotesFlipPolicyPresent: with sigbound.policy at base and NO -notes flag,
// the run attaches a landing note anyway — the policy-present default.
func TestNotesFlipPolicyPresent(t *testing.T) {
	agent := buildTestAgent(t)
	g, repo := makeGoRepo(t)
	commitPolicyFile(t, g, repo)
	tasks := []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{"-repo", repo, "-tasks", tasksFileFor(t, tasks), "-agent", agent, "-json"})
	if err != nil || code != exitOK {
		t.Fatalf("runRun: code=%d err=%v\n%s", code, err, buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !hasSigboundNote(t, repo, rep.Integrate.FinalSHA) {
		t.Fatal("no sigbound note attached despite sigbound.policy present at base (flip should default -notes on)")
	}
}

// TestNotesFlipPolicyAbsent: without a policy file and no -notes, the default is
// unchanged — no note.
func TestNotesFlipPolicyAbsent(t *testing.T) {
	agent := buildTestAgent(t)
	_, repo := makeGoRepo(t)
	tasks := []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{"-repo", repo, "-tasks", tasksFileFor(t, tasks), "-agent", agent, "-json"})
	if err != nil || code != exitOK {
		t.Fatalf("runRun: code=%d err=%v\n%s", code, err, buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if hasSigboundNote(t, repo, rep.Integrate.FinalSHA) {
		t.Fatal("a sigbound note exists with neither -notes nor a policy file present")
	}
}

// TestNotesFlipExplicitFalseWins: an explicit -notes=false beats the
// policy-present default — no note.
func TestNotesFlipExplicitFalseWins(t *testing.T) {
	agent := buildTestAgent(t)
	g, repo := makeGoRepo(t)
	commitPolicyFile(t, g, repo)
	tasks := []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{"-repo", repo, "-tasks", tasksFileFor(t, tasks), "-agent", agent, "-notes=false", "-json"})
	if err != nil || code != exitOK {
		t.Fatalf("runRun: code=%d err=%v\n%s", code, err, buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if hasSigboundNote(t, repo, rep.Integrate.FinalSHA) {
		t.Fatal("explicit -notes=false did not win over the policy-present default")
	}
}

// --- -task view ---

func TestLogTaskAcrossRuns(t *testing.T) {
	g, repo, _ := newGCRepo(t)
	runsDir := logRunsDir(t, g)
	// Same task id "feat" in two runs; the second reused it under -resume.
	writeLogRun(t, runsDir, "20260110T000000Z-aaaa", runReport{
		BaseSHA: hexSHA("00"), StartedAt: "2026-01-10T00:00:00Z",
		PerAgent:  []perAgentJSON{{ID: "feat", Branch: "agent/feat", SHA: hexSHA("a1"), OK: true}},
		Integrate: integrateJSON{Landed: []string{"agent/feat"}, FinalSHA: hexSHA("f1")},
		Verify:    verifyJSON{Ran: true, OK: true},
	})
	writeLogRun(t, runsDir, "20260111T000000Z-bbbb", runReport{
		BaseSHA: hexSHA("f1"), StartedAt: "2026-01-11T00:00:00Z",
		PerAgent:  []perAgentJSON{{ID: "feat", Branch: "agent/feat", SHA: hexSHA("a2"), OK: true, Resumed: true}},
		Integrate: integrateJSON{Landed: []string{"agent/feat"}, FinalSHA: hexSHA("f2")},
		Verify:    verifyJSON{Ran: true, OK: true},
	})

	rows := scanTask(runsDir, "feat")
	if len(rows) != 2 {
		t.Fatalf("task rows = %d, want 2", len(rows))
	}
	// Oldest-first.
	if rows[0].RunID != "20260110T000000Z-aaaa" || rows[1].RunID != "20260111T000000Z-bbbb" {
		t.Fatalf("task order = %q,%q, want oldest-first", rows[0].RunID, rows[1].RunID)
	}
	if !rows[0].Landed || !rows[1].Landed || !rows[1].Resumed {
		t.Fatalf("task rows = %+v", rows)
	}

	var buf bytes.Buffer
	if code, err := runLog(&buf, []string{"-repo", repo, "-task", "feat"}); err != nil || code != exitOK {
		t.Fatalf("runLog -task: code=%d err=%v", code, err)
	}
	if !strings.Contains(buf.String(), "feat") || !strings.Contains(buf.String(), "resumed") {
		t.Fatalf("task output = %q", buf.String())
	}
}

func TestLogSHAAndTaskMutuallyExclusive(t *testing.T) {
	_, repo, _ := newGCRepo(t)
	if _, err := runLog(&bytes.Buffer{}, []string{"-repo", repo, "-sha", hexSHA("a1"), "-task", "x"}); err == nil {
		t.Fatal("expected an error for -sha with -task")
	}
}
