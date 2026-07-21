package cell

import (
	"context"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	runID, err := s.StartRun(ctx, "basesha", "occ")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordBranch(ctx, runID, "agent/wt-1", "sha1", []string{"a.txt", "b.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordBranch(ctx, runID, "agent/wt-2", "sha2", []string{"c.txt"}); err != nil {
		t.Fatal(err)
	}

	res := IntegrationResult{
		FinalSHA: "finalsha",
		Duration: 3 * time.Millisecond,
		Groups:   2,
		MaxBatch: 2,
		Landed:   []string{"agent/wt-1"},
		Flagged:  []BranchResult{{Branch: "agent/wt-2", Conflicts: []string{"c.txt"}}},
	}
	if err := s.FinishRun(ctx, runID, res); err != nil {
		t.Fatal(err)
	}

	counts, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"run": 1, "branch": 2, "change": 3, "queue": 2, "conflict": 1}
	for k, v := range want {
		if counts[k] != v {
			t.Fatalf("count[%s]=%d, want %d (all=%v)", k, counts[k], v, counts)
		}
	}
}
