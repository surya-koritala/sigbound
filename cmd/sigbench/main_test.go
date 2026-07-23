package main

import (
	"errors"
	"testing"

	"github.com/surya-koritala/sigbound/bench"
)

// TestGateOK covers the pure correctness decision runSweep and the non-sweep
// path both gate on. It's the honest testable seam here: runSweep itself
// drives bench.RunAB, which builds and diffs real git repos per sweep point,
// so an exit-code-level test (the "go run ./cmd/sigbench -sweep" smoke run)
// is the practical coverage for the wiring; this test covers the decision
// logic that wiring depends on.
func TestGateOK(t *testing.T) {
	ok := func() *bench.StratStat {
		return &bench.StratStat{Correct: true, TreeComparable: true, TreeMatchesRef: true}
	}

	tests := []struct {
		name string
		ab   bench.ABResult
		want bool
	}{
		{
			name: "all correct and trees match",
			ab:   bench.ABResult{Stats: map[string]*bench.StratStat{"a": ok(), "b": ok()}},
			want: true,
		},
		{
			name: "strategy errored",
			ab: bench.ABResult{Stats: map[string]*bench.StratStat{"a": {
				Correct: true, TreeComparable: true, TreeMatchesRef: true, Err: errors.New("boom"),
			}}},
			want: false,
		},
		{
			name: "strategy not correct",
			ab: bench.ABResult{Stats: map[string]*bench.StratStat{"a": {
				Correct: false, TreeComparable: true, TreeMatchesRef: true,
			}}},
			want: false,
		},
		{
			name: "tree comparable but doesn't match ref",
			ab: bench.ABResult{Stats: map[string]*bench.StratStat{"a": {
				Correct: true, TreeComparable: true, TreeMatchesRef: false,
			}}},
			want: false,
		},
		{
			name: "not tree-comparable is not a failure",
			ab: bench.ABResult{Stats: map[string]*bench.StratStat{"a": {
				Correct: true, TreeComparable: false, TreeMatchesRef: false,
			}}},
			want: true,
		},
		{
			name: "missing stat for a requested strategy",
			ab:   bench.ABResult{Stats: map[string]*bench.StratStat{}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gateOK(tt.ab, []string{"a"}); got != tt.want {
				t.Errorf("gateOK() = %v, want %v", got, tt.want)
			}
		})
	}
}
