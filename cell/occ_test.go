package cell

import (
	"fmt"
	"reflect"
	"testing"
)

// groupBranches renders Partition output as branch-name groups for comparison,
// preserving order (Partition is documented as deterministic).
func groupBranches(groups [][]BranchChange) [][]string {
	out := make([][]string, 0, len(groups))
	for _, g := range groups {
		names := make([]string, 0, len(g))
		for _, bc := range g {
			names = append(names, bc.Branch)
		}
		out = append(out, names)
	}
	return out
}

func bc(name string, paths ...string) BranchChange {
	return BranchChange{Branch: name, WriteSet: NewWriteSet(paths...)}
}

func TestPartitionEmpty(t *testing.T) {
	if got := Partition(nil); got != nil {
		t.Fatalf("Partition(nil) = %v, want nil", got)
	}
}

func TestPartitionFullyDisjoint(t *testing.T) {
	// Every branch touches a unique file -> N singleton groups, max parallelism.
	in := []BranchChange{
		bc("a", "f1"),
		bc("b", "f2"),
		bc("c", "f3"),
	}
	got := groupBranches(Partition(in))
	want := [][]string{{"a"}, {"b"}, {"c"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPartitionAllOverlap(t *testing.T) {
	// All branches touch one shared hot file -> a single group.
	in := []BranchChange{
		bc("a", "hot", "a1"),
		bc("b", "hot", "b1"),
		bc("c", "hot", "c1"),
	}
	got := groupBranches(Partition(in))
	want := [][]string{{"a", "b", "c"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPartitionTransitiveChain(t *testing.T) {
	// A shares p1 with B, B shares p2 with C, A and C are disjoint. Transitivity
	// must still fuse all three into one group.
	in := []BranchChange{
		bc("A", "p1"),
		bc("B", "p1", "p2"),
		bc("C", "p2"),
	}
	got := groupBranches(Partition(in))
	want := [][]string{{"A", "B", "C"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPartitionMixedGroups(t *testing.T) {
	// Two overlap clusters + one loner. Groups ordered by smallest member index;
	// members in input order.
	in := []BranchChange{
		bc("a", "hotX", "pa"), // 0
		bc("b", "pb"),         // 1 loner
		bc("c", "hotX", "pc"), // 2 joins a
		bc("d", "hotY", "pd"), // 3
		bc("e", "hotY", "pe"), // 4 joins d
	}
	got := groupBranches(Partition(in))
	want := [][]string{{"a", "c"}, {"b"}, {"d", "e"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPartitionDisjointnessInvariant(t *testing.T) {
	// Property: across the returned groups, no path is shared between two groups
	// (the guarantee that lets disjoint groups land in parallel).
	in := []BranchChange{
		bc("a", "f1", "shared"),
		bc("b", "shared", "f2"),
		bc("c", "f3"),
		bc("d", "f4", "f5"),
		bc("e", "f5"),
		bc("f", "f6"),
	}
	groups := Partition(in)
	pathToGroup := map[string]int{}
	for gi, g := range groups {
		for _, bcv := range g {
			for _, p := range bcv.WriteSet.Paths() {
				if prev, ok := pathToGroup[p]; ok && prev != gi {
					t.Fatalf("path %q appears in groups %d and %d", p, prev, gi)
				}
				pathToGroup[p] = gi
			}
		}
	}
	// Sanity: every input branch is present exactly once.
	seen := map[string]int{}
	for _, g := range groups {
		for _, bcv := range g {
			seen[bcv.Branch]++
		}
	}
	if len(seen) != len(in) {
		t.Fatalf("expected %d branches across groups, got %d", len(in), len(seen))
	}
	for name, c := range seen {
		if c != 1 {
			t.Fatalf("branch %q appears %d times", name, c)
		}
	}
}

func TestPartitionLargeDeterministic(t *testing.T) {
	// Same input twice must yield identical grouping.
	var in []BranchChange
	for i := 0; i < 50; i++ {
		// even branches share "hot", odd branches are disjoint
		if i%2 == 0 {
			in = append(in, bc(fmt.Sprintf("e%d", i), "hot", fmt.Sprintf("p%d", i)))
		} else {
			in = append(in, bc(fmt.Sprintf("o%d", i), fmt.Sprintf("p%d", i)))
		}
	}
	a := groupBranches(Partition(in))
	b := groupBranches(Partition(in))
	if !reflect.DeepEqual(a, b) {
		t.Fatal("Partition not deterministic")
	}
	// The 25 even branches all share "hot" -> one big group; 25 odd singletons.
	if len(a) != 26 {
		t.Fatalf("groups = %d, want 26", len(a))
	}
}
