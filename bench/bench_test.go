package bench

import (
	"context"
	"testing"
)

// TestRunCorrectness is a fast functional check of the whole harness: a small
// disjoint run must land every branch in both strategies and yield equal trees.
func TestRunCorrectness(t *testing.T) {
	ctx := context.Background()
	res, err := Run(ctx, Config{Agents: 8, Files: 50, Overlap: 0.0, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !res.NaiveCorrect || !res.OCCCorrect {
		t.Fatalf("correctness: naive=%v occ=%v", res.NaiveCorrect, res.OCCCorrect)
	}
	if len(res.OCC.Landed) != 8 || len(res.Naive.Landed) != 8 {
		t.Fatalf("landed: naive=%d occ=%d, want 8/8", len(res.Naive.Landed), len(res.OCC.Landed))
	}
	if !res.TreesEqual {
		t.Fatal("disjoint run: naive and OCC trees must be identical")
	}
	if res.OCC.Groups != 8 {
		t.Fatalf("occ groups=%d, want 8 singletons", res.OCC.Groups)
	}
}

// TestRunWithConflicts exercises the flag-for-human tier.
func TestRunWithConflicts(t *testing.T) {
	ctx := context.Background()
	res, err := Run(ctx, Config{Agents: 12, Files: 50, Overlap: 1.0, Conflict: 1.0, HotFiles: 2, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	// All 12 agents fight over 2 hot files on line 0 -> 2 groups, each lands one
	// and flags the rest. Expect 10 flagged, 2 landed.
	if res.OCC.Groups != 2 {
		t.Fatalf("groups=%d, want 2", res.OCC.Groups)
	}
	if len(res.OCC.Flagged) != 10 || len(res.OCC.Landed) != 2 {
		t.Fatalf("landed=%d flagged=%d, want 2/10", len(res.OCC.Landed), len(res.OCC.Flagged))
	}
	if !res.OCCCorrect {
		t.Fatal("occ correctness failed with conflicts")
	}
}

// BenchmarkIntegrate compares naive vs OCC integration wall-clock at a couple of
// N values via the standard `go test -bench` harness. Reported as ns/op for each
// strategy so the speedup is visible directly.
func BenchmarkIntegrate(b *testing.B) {
	for _, n := range []int{8, 32} {
		cfg := Config{Agents: n, Files: 200, Overlap: 0.1, Seed: 1}
		b.Run(strat("naive", n), func(b *testing.B) { benchOne(b, cfg, false) })
		b.Run(strat("occ", n), func(b *testing.B) { benchOne(b, cfg, true) })
	}
}

func strat(name string, n int) string {
	return name + "/N=" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// benchOne times just the requested integration strategy, rebuilding the branch
// set fresh each iteration (setup excluded from the reported time).
func benchOne(b *testing.B, cfg Config, occ bool) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		// Run does commit + both integrations; we report the strategy's own
		// measured Duration rather than the whole harness, to isolate landing.
		res, err := Run(ctx, cfg)
		if err != nil {
			b.Fatal(err)
		}
		if occ {
			b.ReportMetric(float64(res.OCC.Duration.Nanoseconds()), "integrate-ns/op")
		} else {
			b.ReportMetric(float64(res.Naive.Duration.Nanoseconds()), "integrate-ns/op")
		}
	}
}
