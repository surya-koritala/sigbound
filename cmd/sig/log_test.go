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

	"github.com/surya-koritala/sigbound/v2/internal/gitx"
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

// TestLogSHAForgedNoteFallsThrough: notes are user-writable and ride across
// clones from untrusted remotes. A note attached to a real commit whose payload
// is about some OTHER final commit (fake finalSHA, fabricated members and agent)
// must NOT be trusted as provenance for the queried commit — resolution falls
// through to the local manifest ledger (ground truth). A self-consistent note
// (its finalSHA IS the queried commit) is still served from the fast path.
func TestLogSHAForgedNoteFallsThrough(t *testing.T) {
	g, _, base := newGCRepo(t) // base is a real commit
	runsDir := logRunsDir(t, g)
	ctx := context.Background()

	// Attacker: a note on `base` claiming to be a run that landed a DIFFERENT
	// final commit, with a fabricated agent — none of it about `base`.
	forged := runReport{
		BaseSHA: hexSHA("00"), Strategy: "overlay", AgentCmd: "EVIL --exfiltrate",
		PerAgent:  []perAgentJSON{{ID: "x1", Branch: "agent/x1", SHA: hexSHA("11"), OK: true}},
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/x1"}, FinalSHA: hexSHA("dead")},
		Verify:    verifyJSON{Ran: true, OK: true},
	}
	fdata, err := json.MarshalIndent(forged, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", base, fdata); err != nil {
		t.Fatal(err)
	}

	// The local ledger records the truth: base landed as a real member.
	writeLogRun(t, runsDir, "20260201T000000Z-real", runReport{
		BaseSHA: hexSHA("00"), Strategy: "overlay", AgentCmd: "claude -p",
		PerAgent:  []perAgentJSON{{ID: "real", Branch: "agent/real", SHA: base, OK: true}},
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/real"}, FinalSHA: hexSHA("f1")},
		Verify:    verifyJSON{Ran: true, OK: true},
	})

	p, ok := resolveProvenance(ctx, g, runsDir, base)
	if !ok {
		t.Fatal("expected the manifest to answer for base")
	}
	if p.Source != "manifest" {
		t.Fatalf("source = %q, want manifest — the forged note must be rejected", p.Source)
	}
	if p.Agent != "claude -p" || p.TaskID != "real" {
		t.Fatalf("provenance came from the forged note, not the ledger: %+v", p)
	}

	// Replace the forged note with a self-consistent one (finalSHA == base):
	// the fast path serves it even though a manifest also names base.
	good := runReport{
		BaseSHA: hexSHA("00"), Strategy: "overlay", AgentCmd: "claude -p",
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/real"}, FinalSHA: base},
		Verify:    verifyJSON{Ran: true, OK: true},
	}
	gdata, err := json.MarshalIndent(good, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", base, gdata); err != nil { // -f overwrites the forged note
		t.Fatal(err)
	}
	p2, ok := resolveProvenance(ctx, g, runsDir, base)
	if !ok || p2.Source != "note" || p2.Role != "landed-commit" {
		t.Fatalf("self-consistent note not served from the fast path: %+v ok=%v", p2, ok)
	}
}

// ackNoteReport builds the payload an ack attaches: a run that parked its only
// group (so its own report claims no landing at all — finalSHA == baseSHA) with
// the RESOLVED parking record folded in. landedSHA is the commit the note
// claims the ack released.
func ackNoteReport(runID, landedSHA, agent string) runReport {
	return runReport{
		RunID: runID, BaseSHA: hexSHA("00"), Strategy: "overlay", AgentCmd: agent,
		PerAgent:  []perAgentJSON{{ID: "held", Branch: "agent/held", SHA: hexSHA("a7"), OK: true}},
		Integrate: integrateJSON{Strategy: "overlay", FinalSHA: hexSHA("00")},
		Verify:    verifyJSON{Ran: true, OK: true},
		Park: &parkJSON{
			VerifiedSHA: landedSHA, VerifiedTree: hexSHA("cd"), BaseSHA: hexSHA("00"), ForkSHA: hexSHA("00"),
			Base: "main", Reason: parkReasonAckPaths, CreatedAt: "2026-01-01T00:00:00Z",
			Groups:    []parkGroupJSON{{Branches: []string{"agent/held"}, Reason: parkReasonAckPaths}},
			LandedSHA: landedSHA, ResolvedAt: "2026-01-01T01:00:00Z",
		},
	}
}

// TestLogSHAForgedAckNoteFallsThrough is issue #160's half of the #110 lesson.
// The ack arm of matchProvenance is a new way for a note to claim a commit, so
// it gets the same adversarial test: a note in the ACK shape, hand-attached to
// an unrelated commit, whose park record says some OTHER commit was the acked
// landing. It must not be attributed — resolution falls through to the local
// ledger, which is ground truth. A note that genuinely names the queried commit
// as its ack's landing is still served from the fast path.
func TestLogSHAForgedAckNoteFallsThrough(t *testing.T) {
	g, _, base := newGCRepo(t) // base is a real commit
	runsDir := logRunsDir(t, g)
	ctx := context.Background()

	// Attacker: an ack-shaped note ON base, claiming a human released some other
	// commit entirely — a real ack note lifted onto a commit it says nothing about.
	forged := ackNoteReport("20260101T000000Z-evil", hexSHA("dead"), "EVIL --exfiltrate")
	fdata, err := json.MarshalIndent(forged, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", base, fdata); err != nil {
		t.Fatal(err)
	}

	// The local ledger records the truth about base: an ordinary landed member.
	writeLogRun(t, runsDir, "20260201T000000Z-real", runReport{
		BaseSHA: hexSHA("00"), Strategy: "overlay", AgentCmd: "claude -p",
		PerAgent:  []perAgentJSON{{ID: "real", Branch: "agent/real", SHA: base, OK: true}},
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/real"}, FinalSHA: hexSHA("f1")},
		Verify:    verifyJSON{Ran: true, OK: true},
	})

	p, ok := resolveProvenance(ctx, g, runsDir, base)
	if !ok {
		t.Fatal("expected the manifest to answer for base")
	}
	if p.Source != "manifest" || p.Role == roleAckLanded {
		t.Fatalf("provenance = %+v, want the manifest's answer — a forged ack note must not be attributed", p)
	}
	if p.Agent != "claude -p" || p.TaskID != "real" || p.RunID != "20260201T000000Z-real" {
		t.Fatalf("provenance came from the forged ack note, not the ledger: %+v", p)
	}

	// Self-consistent: the note's ack really did land THIS commit. Served from the
	// fast path — and served as what it is, a note's claim.
	good := ackNoteReport("20260301T000000Z-good", base, "claude -p")
	gdata, err := json.MarshalIndent(good, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", base, gdata); err != nil { // -f overwrites the forged note
		t.Fatal(err)
	}
	p2, ok := resolveProvenance(ctx, g, runsDir, base)
	if !ok || p2.Source != "note" || p2.Role != roleAckLanded || !p2.Landed {
		t.Fatalf("a self-consistent ack note was not served from the fast path: %+v ok=%v", p2, ok)
	}
	if p2.SHA != base || p2.Members != 1 {
		t.Fatalf("ack provenance = %+v, want the queried commit and its one parked branch", p2)
	}
	if line := provenanceLine(p2); !strings.Contains(line, "human ACK") {
		t.Fatalf("rendered as %q — a reader cannot tell a human approved this landing", line)
	}
}

// TestLogSHAForgedAckNoteCannotBorrowARunID is the sharper half of the same
// lesson, and the one falling through does not cover. A note whose park record
// names the QUERIED commit matches on purpose — that is the arm — so the
// question is not whether it matches but what a match buys. It must buy no more
// than any other note-sourced answer: an attacker can write an ack-shaped note
// on their OWN commit naming any real local run id, and if that id reached the
// answer the output would be byte-identical to the ledger's own, sending an
// auditor to a run dir holding a genuine human's ack for work nobody ever saw.
// So a note-sourced answer names no run and says where it came from.
func TestLogSHAForgedAckNoteCannotBorrowARunID(t *testing.T) {
	g, _, attacker := newGCRepo(t) // a commit the attacker controls
	runsDir := logRunsDir(t, g)
	ctx := context.Background()

	// The identity the forgery wants to wear: a real, innocent local run.
	const realRun = "20260201T000000Z-real"
	writeLogRun(t, runsDir, realRun, runReport{
		RunID: realRun, BaseSHA: hexSHA("00"), Strategy: "overlay", AgentCmd: "claude -p",
		PerAgent:  []perAgentJSON{{ID: "real", Branch: "agent/real", SHA: hexSHA("aa"), OK: true}},
		Integrate: integrateJSON{Strategy: "overlay", Landed: []string{"agent/real"}, FinalSHA: hexSHA("f1")},
		Verify:    verifyJSON{Ran: true, OK: true},
	})

	// Self-consistent by construction — the note sits on the very commit its park
	// record calls the acked landing, so the arm matches — and its runId is the
	// real run's.
	data, err := json.MarshalIndent(ackNoteReport(realRun, attacker, "claude -p"), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.NoteAdd(ctx, "sigbound", attacker, data); err != nil {
		t.Fatal(err)
	}

	p, ok := resolveProvenance(ctx, g, runsDir, attacker)
	if !ok || p.Source != "note" {
		t.Fatalf("provenance = %+v ok=%v, want the note's own answer", p, ok)
	}
	if p.RunID != "" {
		t.Fatalf("a note-sourced answer carries run id %q — a forged note just borrowed a real local run's identity", p.RunID)
	}
	line := provenanceLine(p)
	if !strings.Contains(line, "from commit note") || strings.Contains(line, realRun) {
		t.Fatalf("rendered as %q — a note-sourced answer must say so, and must not name a local run", line)
	}
}

// TestProvenanceLineMarksANoteSourcedAnswer pins the rendering half of that
// fence on its own. resolveProvenance clears RunID on the note path, so the two
// signals normally agree — and that agreement is exactly how this broke once
// already: the marker keyed on "no run id", one new arm carried a payload's run
// id out, and a note-sourced line silently started reading like the ledger's.
// The marker is keyed on Source, so an answer that carries both still says
// where it came from and still names no run.
func TestProvenanceLineMarksANoteSourcedAnswer(t *testing.T) {
	line := provenanceLine(&provenance{
		SHA: hexSHA("ab"), Landed: true, Role: roleAckLanded, Source: "note",
		RunID: "20260201T000000Z-real", StartedAt: "2026-02-01T00:00:00Z",
		Strategy: "overlay", Verify: "pass", Members: 1,
	})
	if !strings.Contains(line, "from commit note") || strings.Contains(line, "20260201T000000Z-real") {
		t.Fatalf("rendered as %q — a note-sourced answer must be marked as one and must not name a run", line)
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

// --- issue #161: GET /runs and GET /log cannot disagree about what landed ---

// fixturePark writes the park.json + status marker a park leaves behind. out
// carries only the OUTCOME fields (landedSHA / rejectReason / resolvedAt, all
// empty for a park still awaiting a human); everything else is fixed, so the
// cases below differ in exactly the one thing under test.
func fixturePark(t *testing.T, dir, status string, out parkJSON) {
	t.Helper()
	out.VerifiedSHA, out.VerifiedTree = hexSHA("ab"), hexSHA("cd")
	out.BaseSHA, out.ForkSHA = hexSHA("ef"), hexSHA("12")
	out.Base, out.Reason, out.CreatedAt = "main", parkReasonAckPaths, "2026-01-09T00:00:00Z"
	out.Groups = []parkGroupJSON{{Branches: []string{"agent/t2"}, Reason: parkReasonAckPaths}}
	out.Attempts = []parkAttemptJSON{{N: 1, At: "2026-01-09T00:00:00Z", VerifyOK: true}}
	if err := writePark(dir, &out); err != nil {
		t.Fatal(err)
	}
	writeRunStatus(dir, status, "")
}

// resolvedPark is the outcome an ack or a reject records: both stamp resolvedAt,
// and which of the other two fields is set is what tells them apart.
func resolvedPark(landedSHA, rejectReason string) parkJSON {
	return parkJSON{LandedSHA: landedSHA, RejectReason: rejectReason, ResolvedAt: "2026-01-09T01:00:00Z"}
}

// TestRunsListAndLogAgreeOnLandedSHA builds one history holding every shape a
// landed sha can take — a clean landing, a run whose verify went red, an acked
// park, a rejected park, an ack whose ref move failed, and a run that landed its
// clean groups and THEN parked a held one — and asserts GET /runs and GET /log
// agree across the whole set. Agreement alone would be vacuous (two surfaces can
// be wrong the same way), so each case also pins the sha that actually reached
// the base ref.
//
// The last two cases are the ones that keep a status gate off GET /runs: they
// are the only cells where landed(rep) is true while the run's status is
// something other than "done", so anything narrower than "read the run dir"
// erases a landing GET /log reports.
func TestRunsListAndLogAgreeOnLandedSHA(t *testing.T) {
	_, repo := makeGoRepo(t)
	srv, ts := newTestServer(t, "", repo)
	runsDir := srv.cells[0].runsDir

	base := hexSHA("b0")
	rep := func(finalSHA string, verifyOK bool) runReport {
		r := runReport{
			BaseSHA: base, StartedAt: "2026-01-09T00:00:00Z", Strategy: "overlay", AgentCmd: "claude -p",
			PerAgent:  []perAgentJSON{{ID: "t1", Branch: "agent/t1", SHA: hexSHA("a1"), OK: true}},
			Integrate: integrateJSON{Strategy: "overlay", FinalSHA: finalSHA},
			Verify:    verifyJSON{Ran: true, OK: verifyOK},
		}
		if finalSHA != base {
			r.Integrate.Landed = []string{"agent/t1"}
		}
		return r
	}
	const (
		cleanID       = "20260109T000001Z-aaaa"
		redID         = "20260109T000002Z-bbbb"
		ackedID       = "20260109T000003Z-cccc"
		rejectedID    = "20260109T000004Z-dddd"
		wedgedID      = "20260109T000005Z-eeee"
		partialHeldID = "20260109T000006Z-ffff"
		partialNoID   = "20260109T000007Z-9999"
	)
	// A clean landing: verify green, the ref moved to f1.
	fixtureRun(t, runsDir, cleanID, "done", rep(hexSHA("f1"), true), nil)
	// Verify went RED. integrate.finalSHA holds the integrated tree the run built,
	// but landRef was never reached and the base ref never moved.
	fixtureRun(t, runsDir, redID, "done", rep(hexSHA("f2"), false), nil)
	// Every group parked, so the run itself landed nothing (finalSHA == baseSHA);
	// a human ACKED it afterwards and f3 landed, recorded only in park.json.
	fixtureRun(t, runsDir, ackedID, "done", rep(base, true), nil)
	fixturePark(t, filepath.Join(runsDir, ackedID), "done", resolvedPark(hexSHA("f3"), ""))
	// Parked and REJECTED: a verified landing a human refused. Nothing moved.
	fixtureRun(t, runsDir, rejectedID, "done", rep(base, true), nil)
	fixturePark(t, filepath.Join(runsDir, rejectedID), statusRejected, resolvedPark("", "not this week"))
	// An ack that wrote its outcome and then failed to move the ref for a reason
	// other than ErrRefMoved (a stale .lock, ENOSPC): park.json claims f5, but the
	// "done" marker ackRun writes only AFTER landRef succeeds never appeared. Fail
	// closed — the ref provably did not move, so this is a landing on no surface.
	fixtureRun(t, runsDir, wedgedID, "done", rep(base, true), nil)
	fixturePark(t, filepath.Join(runsDir, wedgedID), statusAwaitingAck, resolvedPark(hexSHA("f5"), ""))
	// A PARTIAL landing (issue #109's normal shape, and what newParkFixture drives
	// end to end): driveRun landed the clean groups at f6 and only THEN parked a
	// held one, so finishRunDir marked the run awaiting-ack. The ref really moved
	// to f6 while the status is not "done".
	fixtureRun(t, runsDir, partialHeldID, statusAwaitingAck, rep(hexSHA("f6"), true), nil)
	fixturePark(t, filepath.Join(runsDir, partialHeldID), statusAwaitingAck, parkJSON{})
	// The same partial landing after the human REJECTED the held group: f7 stays
	// landed — the reject refused the park, not the commit already on the ref.
	fixtureRun(t, runsDir, partialNoID, statusRejected, rep(hexSHA("f7"), true), nil)
	fixturePark(t, filepath.Join(runsDir, partialNoID), statusRejected, resolvedPark("", "not this week"))

	var list struct{ Runs []runListEntry }
	if code := doJSON(t, "GET", ts.URL+"/runs", "", nil, &list); code != http.StatusOK {
		t.Fatalf("GET /runs: %d", code)
	}
	var lg struct {
		Cells []struct {
			Runs []logRow `json:"runs"`
		} `json:"cells"`
	}
	if code := doJSON(t, "GET", ts.URL+"/log?limit=50", "", nil, &lg); code != http.StatusOK {
		t.Fatalf("GET /log: %d", code)
	}
	fromRuns := map[string]string{}
	for _, e := range list.Runs {
		fromRuns[e.ID] = e.FinalSHA
	}
	fromLog := map[string]string{}
	for _, c := range lg.Cells {
		for _, row := range c.Runs {
			fromLog[row.ID] = row.LandedSHA
		}
	}

	cases := []struct{ name, id, want string }{
		{"clean landing", cleanID, hexSHA("f1")},
		{"red verify landed nothing", redID, ""},
		{"acked park landed the ack commit", ackedID, hexSHA("f3")},
		{"rejected park landed nothing", rejectedID, ""},
		{"ack whose ref move failed landed nothing", wedgedID, ""},
		{"partial landing still awaiting ack", partialHeldID, hexSHA("f6")},
		{"partial landing whose park was rejected", partialNoID, hexSHA("f7")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runsSHA, ok := fromRuns[c.id]
			if !ok {
				t.Fatalf("%s missing from GET /runs", c.id)
			}
			logSHA, ok := fromLog[c.id]
			if !ok {
				t.Fatalf("%s missing from GET /log", c.id)
			}
			// /runs serves the full object name, /log the abbreviation its human
			// column shows, so the two are compared through short().
			if short(runsSHA) != logSHA {
				t.Fatalf("/runs and /log disagree: /runs %q, /log %q", runsSHA, logSHA)
			}
			if runsSHA != c.want {
				t.Fatalf("landed sha = %q, want %q", runsSHA, c.want)
			}
		})
	}
}

// TestRunsListAndLogAgreeOnRealPartialLanding is the same agreement assertion
// against a REAL run instead of fixtures. newParkFixture drives a two-task run
// over serve whose clean group LANDS and whose held group parks, so the daemon
// itself produces the cell that matters: the base ref genuinely moved and the
// run's status is awaiting-ack rather than done. No fixture can prove a ref
// moved; `git rev-parse main` can, and both surfaces have to name that commit.
func TestRunsListAndLogAgreeOnRealPartialLanding(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	head := f.head() // where the clean group put main
	// Without this the test would pass against a status-gated /runs the moment
	// the fixture's run came back "done" — the gate would simply never be hit.
	if st := f.status(); st != statusAwaitingAck {
		t.Fatalf("run status %q, want %q: the landed-but-not-done cell is what this asserts", st, statusAwaitingAck)
	}

	var list struct{ Runs []runListEntry }
	if code := doJSON(t, "GET", f.ts.URL+"/runs", "", nil, &list); code != http.StatusOK {
		t.Fatalf("GET /runs: %d", code)
	}
	var runsSHA string
	found := false
	for _, e := range list.Runs {
		if e.ID == f.runID {
			runsSHA, found = e.FinalSHA, true
		}
	}
	if !found {
		t.Fatalf("run %s missing from GET /runs: %+v", f.runID, list.Runs)
	}
	var lg struct {
		Cells []struct {
			Runs []logRow `json:"runs"`
		} `json:"cells"`
	}
	if code := doJSON(t, "GET", f.ts.URL+"/log?limit=50", "", nil, &lg); code != http.StatusOK {
		t.Fatalf("GET /log: %d", code)
	}
	var logSHA string
	found = false
	for _, c := range lg.Cells {
		for _, row := range c.Runs {
			if row.ID == f.runID {
				logSHA, found = row.LandedSHA, true
			}
		}
	}
	if !found {
		t.Fatalf("run %s missing from GET /log", f.runID)
	}
	if short(runsSHA) != logSHA {
		t.Fatalf("/runs and /log disagree: /runs %q, /log %q", runsSHA, logSHA)
	}
	if runsSHA != head {
		t.Fatalf("landed sha = %q, want %q — main IS at that commit", runsSHA, head)
	}
}

// TestRunsStartedAtPrefersTheReport pins the other half of the agreement: both
// /runs surfaces take startedAt from the run's own REPORT, never from the
// in-memory record. They are different instants — the record is stamped when
// POST /runs accepted the run, the report when driveRun actually began, with a
// -goal run's entire planner call in between — so reading the record would make
// this field change the moment a daemon restart dropped it from memory. The
// gap is planted an hour wide on purpose: RFC3339 is second-resolution, and a
// test fast enough to finish inside one second would otherwise assert nothing.
func TestRunsStartedAtPrefersTheReport(t *testing.T) {
	_, repo := makeGoRepo(t)
	srv, ts := newTestServer(t, "", repo)
	runsDir := srv.cells[0].runsDir

	const id = "20260109T000009Z-8888"
	const reportStart = "2026-01-09T00:00:09Z"
	fixtureRun(t, runsDir, id, "done", runReport{
		BaseSHA: hexSHA("b0"), StartedAt: reportStart, AgentCmd: "claude -p",
		Integrate: integrateJSON{FinalSHA: hexSHA("b0")},
	}, nil)
	accepted, err := time.Parse(time.RFC3339, "2026-01-08T23:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	// The live record a restart would drop, holding the accept time.
	srv.mu.Lock()
	srv.runs[id] = &runRecord{
		id: id, cellID: srv.cells[0].cell.ID(), repo: repo,
		dir: filepath.Join(runsDir, id), status: "done", startedAt: accepted,
	}
	srv.mu.Unlock()

	var list struct{ Runs []runListEntry }
	if code := doJSON(t, "GET", ts.URL+"/runs", "", nil, &list); code != http.StatusOK {
		t.Fatalf("GET /runs: %d", code)
	}
	got := ""
	for _, e := range list.Runs {
		if e.ID == id {
			got = e.StartedAt
		}
	}
	if got != reportStart {
		t.Fatalf("GET /runs startedAt = %q, want the report's %q", got, reportStart)
	}
	var one struct {
		StartedAt string `json:"startedAt"`
	}
	if code := doJSON(t, "GET", ts.URL+"/runs/"+id, "", nil, &one); code != http.StatusOK {
		t.Fatalf("GET /runs/%s: %d", id, code)
	}
	if one.StartedAt != reportStart {
		t.Fatalf("GET /runs/%s startedAt = %q, want the report's %q", id, one.StartedAt, reportStart)
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
