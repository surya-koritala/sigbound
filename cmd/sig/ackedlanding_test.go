package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// validParkRecord is a park.json that passes parkJSON.validate — every required
// field populated with something structurally real. The tests below vary only
// resolvedAt/landedSHA and the run status, which are the two things
// ackedLandedSHA actually reads.
func validParkRecord(sha string) *parkJSON {
	const zero = "1111111111111111111111111111111111111111"
	return &parkJSON{
		VerifiedSHA:  sha,
		VerifiedTree: "3333333333333333333333333333333333333333",
		BaseSHA:      zero,
		ForkSHA:      zero,
		Base:         "main",
		Groups:       []parkGroupJSON{{Branches: []string{"agent/a"}, Reason: parkReasonAckPaths}},
		Reason:       parkReasonAckPaths,
		CreatedAt:    "2026-07-26T00:00:00Z",
	}
}

// parkedRunDir builds a run directory holding a park record that has been
// RESOLVED — resolvedAt and landedSHA both written — at the given run status.
// That combination is the whole subject: it is what both ack paths leave on
// disk in the window between recording the release and the ref actually moving.
func parkedRunDir(t *testing.T, status, landedSHA string) string {
	t.Helper()
	dir := t.TempDir()
	pk := validParkRecord(landedSHA)
	pk.ResolvedAt = "2026-07-26T00:05:00Z"
	pk.LandedSHA = landedSHA
	if err := writePark(dir, pk); err != nil {
		t.Fatal(err)
	}
	writeRunStatus(dir, status, "")
	return dir
}

// TestAckedLandedSHAGates pins BOTH of ackedLandedSHA's guards. Before this,
// either could be deleted with the whole suite staying green (issue #165) — the
// behaviour was correct and nothing held it there.
//
// The status gate is the one that matters, and the crashed-mid-ack shape is why.
// ackRun and ackReverify write resolvedAt and landedSHA BEFORE moving the ref,
// and only ErrRefMoved rewinds them. Every other landRef failure — a stale
// refs/heads/X.lock, a reference-transaction hook refusing the update, ENOSPC —
// returns with the record still claiming a landing that never happened. Reading
// resolvedAt alone would report a landing for a ref that provably did not move.
//
// That false positive is not academic: the helper has four consumers, and in
// blastRadius it names an innocent run as entangling a revert.
func TestAckedLandedSHAGates(t *testing.T) {
	const landed = "2222222222222222222222222222222222222222"

	t.Run("a released park that actually landed reports its SHA", func(t *testing.T) {
		// The only shape that counts: record resolved AND status done, which is
		// written only after landRef succeeded.
		if got := ackedLandedSHA(parkedRunDir(t, "done", landed)); got != landed {
			t.Fatalf("ackedLandedSHA=%q, want %q — a real acked landing is invisible", got, landed)
		}
	})

	t.Run("crashed mid-ack: record written, ref never moved", func(t *testing.T) {
		// The record claims a landing; the status says the run never finished.
		// This is exactly what a stale index.lock or a rejecting hook leaves.
		if got := ackedLandedSHA(parkedRunDir(t, statusAwaitingAck, landed)); got != "" {
			t.Fatalf("ackedLandedSHA=%q for a run whose ref provably never moved; every consumer now believes a landing that did not happen", got)
		}
	})

	t.Run("a run left running reports nothing", func(t *testing.T) {
		if got := ackedLandedSHA(parkedRunDir(t, "running", landed)); got != "" {
			t.Fatalf("ackedLandedSHA=%q for a still-running run", got)
		}
	})

	t.Run("an errored run reports nothing", func(t *testing.T) {
		if got := ackedLandedSHA(parkedRunDir(t, "error", landed)); got != "" {
			t.Fatalf("ackedLandedSHA=%q for a run that errored", got)
		}
	})

	t.Run("an unresolved park reports nothing even at status done", func(t *testing.T) {
		// The resolvedAt gate, isolated: a park nobody has acked. Status is forced
		// to done so the status gate cannot be what produces the empty answer —
		// otherwise this case would pass with the resolvedAt check deleted.
		dir := t.TempDir()
		pk := validParkRecord(landed)
		pk.LandedSHA = landed // present but never released
		if err := writePark(dir, pk); err != nil {
			t.Fatal(err)
		}
		writeRunStatus(dir, "done", "")
		if got := ackedLandedSHA(dir); got != "" {
			t.Fatalf("ackedLandedSHA=%q for a park nobody acked; an unreleased landing is being counted as released", got)
		}
	})

	t.Run("a directory with no park record reports nothing", func(t *testing.T) {
		dir := t.TempDir()
		writeRunStatus(dir, "done", "")
		if got := ackedLandedSHA(dir); got != "" {
			t.Fatalf("ackedLandedSHA=%q for a run that never parked", got)
		}
	})
}

// TestBlastRadiusSeesBothLandingsOfAMixedRun is issue #164. A multi-task run
// under ack-paths lands its clean groups IMMEDIATELY — so landed(rep) is already
// true — and parks the gated one. Acking that park makes the run contribute a
// SECOND landing, from a different fork point.
//
// Consulting park.json only when the report shows nothing landed sees the first
// and never the second. Safety was never at risk (the conflict gate refuses
// either way), but the operator was told to "unland those runs first" while the
// list named none of them — an instruction that cannot be followed.
//
// The assertion is on the SHAs, not the count: a count alone would pass against
// an implementation that reported the same landing twice.
func TestBlastRadiusSeesBothLandingsOfAMixedRun(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)

	// base -> first (the run's own landing) -> second (what its ack landed).
	// Both touch shared.txt, so both entangle a revert of anything that owns it.
	write := func(content string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		sha, err := g.CommitAll(ctx, "touch shared")
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	base, err := g.RevParse(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	first := write("first landing\n")
	second := write("second landing, released by an ack\n")

	runsDir := filepath.Join(t.TempDir(), "runs")
	mkRun := func(id string) string {
		dir := filepath.Join(runsDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	// The run being reverted. Older id, so the walk stops at it.
	mkRun("20260101T000000Z-target")

	// The mixed run: report records the clean group's landing (base -> first),
	// park.json records the acked one (first -> second).
	later := mkRun("20260102T000000Z-mixed")
	writeRunReport(later, runReport{
		RunID:     "20260102T000000Z-mixed",
		BaseSHA:   base,
		Integrate: integrateJSON{FinalSHA: first, Landed: []string{"agent/clean"}},
	})
	pk := validParkRecord(second)
	pk.BaseSHA, pk.ForkSHA = first, base
	pk.ResolvedAt, pk.LandedSHA = "2026-07-26T00:05:00Z", second
	if err := writePark(later, pk); err != nil {
		t.Fatal(err)
	}
	writeRunStatus(later, "done", "")

	ent, _, err := blastRadius(ctx, g, runsDir, "20260101T000000Z-target", []string{"shared.txt"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, e := range ent {
		got[e.FinalSHA] = true
	}
	if !got[first] {
		t.Fatalf("the run's own landing %s is missing from the entanglement scan: %+v", short(first), ent)
	}
	if !got[second] {
		t.Fatalf("the ACK-released landing %s is missing: the run landed twice and the scan only saw the first, so the block message names no run to unland. entangled=%+v", short(second), ent)
	}

	// And the advisory list a human reads names the run exactly once — unlanding
	// takes back everything a run contributed, so "unland X, then unland X" is
	// not an instruction anyone can follow.
	ids := entangledIDs(ent)
	if len(ids) != 1 || ids[0] != "20260102T000000Z-mixed" {
		t.Fatalf("entangledIDs=%v, want the run named exactly once", ids)
	}
}
