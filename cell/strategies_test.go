package cell

import (
	"context"
	"strings"
	"testing"
)

// treeOID is the content-addressed tree OID of a commit: two commits are
// byte-for-byte tree-equal iff these match.
func treeOID(t *testing.T, in *Integrator, commit string) string {
	t.Helper()
	oid, err := in.g.TreeOID(context.Background(), commit)
	if err != nil {
		t.Fatalf("tree oid of %s: %v", commit, err)
	}
	return oid
}

// TestOverlayMatchesMergeTreeStrategy is the strategy-level equivalence the spec
// requires: for a fully-disjoint batch the overlay fast path must produce the
// SAME final tree as the merge-tree engine (proof the "no merge at all" combine
// is sound end-to-end, not just at the gitx primitive level).
func TestOverlayMatchesMergeTreeStrategy(t *testing.T) {
	ctx := context.Background()
	_, pool, base := scenario(t, 16)
	const n = 12
	changes := spawnAgents(t, pool, base, n, nil) // fully disjoint

	in := NewIntegrator(pool.Git())

	mt, err := in.IntegrateOCC(ctx, base, changes) // merge-tree combine
	if err != nil {
		t.Fatal(err)
	}
	ov, err := in.IntegrateOverlay(ctx, base, changes) // overlay combine
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Landed) != n || len(ov.Flagged) != 0 {
		t.Fatalf("overlay landed=%d flagged=%d, want %d/0", len(ov.Landed), len(ov.Flagged), n)
	}
	if ov.Groups != n {
		t.Fatalf("overlay groups=%d, want %d singletons", ov.Groups, n)
	}
	if got, want := treeOID(t, in, ov.FinalSHA), treeOID(t, in, mt.FinalSHA); got != want {
		t.Fatalf("overlay tree %s != mergetree tree %s", got, want)
	}
}

// TestIntegrateOverlayAssertHealthy verifies -assert (WithAssert) is a pure
// paranoia check: on a healthy fully-disjoint batch it must not change the
// outcome. The union tree it computes must equal what the SAME batch produces
// with assert off.
func TestIntegrateOverlayAssertHealthy(t *testing.T) {
	ctx := context.Background()
	_, pool, base := scenario(t, 16)
	const n = 10
	changes := spawnAgents(t, pool, base, n, nil) // fully disjoint

	plain := NewIntegrator(pool.Git())
	want, err := plain.IntegrateOverlay(ctx, base, changes)
	if err != nil {
		t.Fatal(err)
	}

	asserted := NewIntegrator(pool.Git()).WithAssert()
	got, err := asserted.IntegrateOverlay(ctx, base, changes)
	if err != nil {
		t.Fatalf("assert-on overlay: %v", err)
	}
	if len(got.Landed) != n || len(got.Flagged) != 0 {
		t.Fatalf("assert-on landed=%d flagged=%d, want %d/0", len(got.Landed), len(got.Flagged), n)
	}
	if gotTree, wantTree := treeOID(t, asserted, got.FinalSHA), treeOID(t, plain, want.FinalSHA); gotTree != wantTree {
		t.Fatalf("assert-on tree %s != assert-off tree %s", gotTree, wantTree)
	}
}

// TestOverlayAssertMismatchErrors exercises the comparison seam directly: a
// real overlay/merge-tree divergence would mean the partition invariant is
// broken (a bug that, by construction, doesn't exist in this codebase), so
// there's no honest way to trigger it end-to-end without faking a bug that
// isn't there. Testing overlayAssertErr itself proves the check actually
// fires on a mismatch and stays silent on a match.
func TestOverlayAssertMismatchErrors(t *testing.T) {
	if err := overlayAssertErr("aaaa", "aaaa"); err != nil {
		t.Fatalf("equal OIDs: unexpected error: %v", err)
	}
	err := overlayAssertErr("aaaa", "bbbb")
	if err == nil {
		t.Fatal("expected error on mismatched tree OIDs")
	}
	if !strings.Contains(err.Error(), "aaaa") || !strings.Contains(err.Error(), "bbbb") {
		t.Fatalf("error %q must name both OIDs", err.Error())
	}
}

// TestAllStrategiesAgreeClean runs every strategy on the same auto-merge batch
// (overlapping distinct-line edits + disjoint files) and asserts they all land
// everything and yield an identical final tree. This is the A/B soundness
// guarantee: the strategies differ only in speed, never in result.
func TestAllStrategiesAgreeClean(t *testing.T) {
	ctx := context.Background()
	_, pool, base := scenario(t, 12)
	const n = 10
	// Agents 0..5 edit the same hot file on distinct spaced lines (one group,
	// all auto-merge); 6..9 are disjoint.
	overlap := map[int]hotEdit{}
	for i := 0; i < 6; i++ {
		overlap[i] = hotEdit{file: "f000.txt", line: i*5 + 1}
	}
	changes := spawnAgents(t, pool, base, n, overlap)

	in := NewIntegrator(pool.Git())
	var wantTree string
	for _, strat := range AvailableStrategies() {
		res, err := in.Integrate(ctx, base, changes, strat)
		if err != nil {
			t.Fatalf("%s: %v", strat, err)
		}
		if len(res.Landed) != n || len(res.Flagged) != 0 {
			t.Fatalf("%s landed=%d flagged=%d, want %d/0", strat, len(res.Landed), len(res.Flagged), n)
		}
		tree := treeOID(t, in, res.FinalSHA)
		if wantTree == "" {
			wantTree = tree
			continue
		}
		if tree != wantTree {
			t.Fatalf("%s tree %s != %s (first strategy)", strat, tree, wantTree)
		}
	}
}

// TestAllStrategiesAgreeConflict asserts every strategy handles a real conflict
// identically: land the first, flag the rest on the exact conflicting path — a
// real conflict is never silently dropped or auto-resolved by any strategy.
func TestAllStrategiesAgreeConflict(t *testing.T) {
	ctx := context.Background()
	_, pool, base := scenario(t, 6)
	// Three agents fight over the same line of the same file.
	const n = 3
	overlap := map[int]hotEdit{}
	for i := 0; i < n; i++ {
		overlap[i] = hotEdit{file: "f000.txt", line: 0} // all line 0 -> conflict
	}
	changes := spawnAgents(t, pool, base, n, overlap)

	in := NewIntegrator(pool.Git())
	for _, strat := range AvailableStrategies() {
		res, err := in.Integrate(ctx, base, changes, strat)
		if err != nil {
			t.Fatalf("%s: %v", strat, err)
		}
		if len(res.Landed) != 1 || len(res.Flagged) != 2 {
			t.Fatalf("%s landed=%d flagged=%d, want 1/2", strat, len(res.Landed), len(res.Flagged))
		}
		for _, f := range res.Flagged {
			if len(f.Conflicts) != 1 || f.Conflicts[0] != "f000.txt" {
				t.Fatalf("%s flagged %s conflicts=%v, want [f000.txt]", strat, f.Branch, f.Conflicts)
			}
		}
	}
}

func TestIntegrateUnknownStrategy(t *testing.T) {
	ctx := context.Background()
	_, pool, base := scenario(t, 2)
	in := NewIntegrator(pool.Git())
	if _, err := in.Integrate(ctx, base, nil, "bogus"); err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

// TestLandRefPublishesFinal verifies the single serialized landing step: with a
// land ref configured, the integrated commit is published there via update-ref,
// for every strategy.
func TestLandRefPublishesFinal(t *testing.T) {
	ctx := context.Background()
	for _, strat := range AvailableStrategies() {
		g, pool, base := scenario(t, 6)
		const n = 5
		changes := spawnAgents(t, pool, base, n, nil)
		in := NewIntegrator(pool.Git()).WithLandRef("refs/heads/main")

		res, err := in.Integrate(ctx, base, changes, strat)
		if err != nil {
			t.Fatalf("%s: %v", strat, err)
		}
		mainSHA, err := g.RevParse(ctx, "refs/heads/main")
		if err != nil {
			t.Fatalf("%s: rev-parse main: %v", strat, err)
		}
		if mainSHA != res.FinalSHA {
			t.Fatalf("%s: main=%s, want FinalSHA=%s", strat, mainSHA, res.FinalSHA)
		}
		if mainSHA == base {
			t.Fatalf("%s: main did not advance past base", strat)
		}
	}
}
