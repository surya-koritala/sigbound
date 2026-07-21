package cell

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"testing"
)

// bruteComponents computes connected components of the overlap graph the slow,
// obviously-correct way: O(n^2) pairwise write-set overlap + BFS. Used as an
// independent oracle for Partition's union-find.
func bruteComponents(changes []BranchChange) [][]string {
	n := len(changes)
	adj := make([][]int, n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if changes[i].WriteSet.Overlaps(changes[j].WriteSet) {
				adj[i] = append(adj[i], j)
				adj[j] = append(adj[j], i)
			}
		}
	}
	seen := make([]bool, n)
	var comps [][]string
	for i := 0; i < n; i++ {
		if seen[i] {
			continue
		}
		var stack, comp []int
		stack = append(stack, i)
		seen[i] = true
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			comp = append(comp, x)
			for _, y := range adj[x] {
				if !seen[y] {
					seen[y] = true
					stack = append(stack, y)
				}
			}
		}
		names := make([]string, 0, len(comp))
		for _, x := range comp {
			names = append(names, changes[x].Branch)
		}
		sort.Strings(names)
		comps = append(comps, names)
	}
	return comps
}

func normalizeGroups(groups [][]string) [][]string {
	out := make([][]string, 0, len(groups))
	for _, g := range groups {
		gg := append([]string(nil), g...)
		sort.Strings(gg)
		out = append(out, gg)
	}
	sort.Slice(out, func(a, b int) bool { return strings.Join(out[a], ",") < strings.Join(out[b], ",") })
	return out
}

// TestPartitionMatchesBruteForce fuzzes Partition against an independent
// connected-components oracle over many random write-set configurations. If
// union-find ever mis-groups (splits a connected cluster or fuses disjoint
// ones), this fails.
func TestPartitionMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(12345))
	for trial := 0; trial < 400; trial++ {
		n := 1 + rng.Intn(40)
		pathPool := 1 + rng.Intn(30) // small pool => forced overlaps
		changes := make([]BranchChange, n)
		for i := 0; i < n; i++ {
			k := 1 + rng.Intn(4)
			ws := NewWriteSet()
			for j := 0; j < k; j++ {
				ws.Add(fmt.Sprintf("p%d", rng.Intn(pathPool)))
			}
			changes[i] = BranchChange{Branch: fmt.Sprintf("b%d", i), WriteSet: ws}
		}
		got := normalizeGroups(groupBranches(Partition(changes)))
		want := normalizeGroups(bruteComponents(changes))
		if len(got) != len(want) {
			t.Fatalf("trial %d: group count got=%d want=%d\ngot=%v\nwant=%v", trial, len(got), len(want), got, want)
		}
		for i := range got {
			if strings.Join(got[i], ",") != strings.Join(want[i], ",") {
				t.Fatalf("trial %d: mismatch\ngot=%v\nwant=%v", trial, got, want)
			}
		}
	}
}

// TestPartitionDisjointGroupsShareNoPath asserts the load-bearing invariant on
// fuzzed input: no path is claimed by two different groups. This is exactly what
// makes "disjoint groups land in parallel" sound.
func TestPartitionDisjointGroupsShareNoPath(t *testing.T) {
	rng := rand.New(rand.NewSource(999))
	for trial := 0; trial < 200; trial++ {
		n := 1 + rng.Intn(30)
		changes := make([]BranchChange, n)
		for i := 0; i < n; i++ {
			ws := NewWriteSet()
			for j := 0; j < 1+rng.Intn(3); j++ {
				ws.Add(fmt.Sprintf("p%d", rng.Intn(20)))
			}
			changes[i] = BranchChange{Branch: fmt.Sprintf("b%d", i), WriteSet: ws}
		}
		groups := Partition(changes)
		owner := map[string]int{}
		for gi, g := range groups {
			for _, b := range g {
				for _, p := range b.WriteSet.Paths() {
					if prev, ok := owner[p]; ok && prev != gi {
						t.Fatalf("trial %d: path %q in groups %d and %d (partition unsound)", trial, p, prev, gi)
					}
					owner[p] = gi
				}
			}
		}
	}
}

// disjointModifiers spawns n agents, each MODIFYING a distinct EXISTING file's
// content (not just adding a private file). Their write-sets are pairwise
// disjoint, so OCC lands them via the parallel combine reduction. Returns the
// changes plus the (file -> unique marker) each agent wrote.
func disjointModifiers(t *testing.T, pool *WorktreePool, base string, n int) ([]BranchChange, map[string]string) {
	t.Helper()
	ctx := context.Background()
	out := make([]BranchChange, n)
	markers := make(map[string]string, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		file := fmt.Sprintf("f%03d.txt", i)
		marker := fmt.Sprintf("AGENT-%d-CONTENT-MARKER", i)
		mu.Lock()
		markers[file] = marker
		mu.Unlock()
		wg.Add(1)
		go func(i int, file, marker string) {
			defer wg.Done()
			wt, err := pool.Acquire(ctx, base)
			if err != nil {
				errs[i] = err
				return
			}
			// Modify an existing base file's content (line 10) -> the change is a
			// content edit, so a dropped merge shows as a missing marker, not a
			// missing file.
			editDistinctLine(file, marker, 10)(wt.Dir)
			if _, err := wt.Git().CommitAll(ctx, fmt.Sprintf("agent %d", i)); err != nil {
				errs[i] = err
				return
			}
			ws, err := WriteSetFor(ctx, pool.Git(), base, wt.Branch)
			if err != nil {
				errs[i] = err
				return
			}
			out[i] = BranchChange{Branch: wt.Branch, WriteSet: ws}
			_ = pool.Release(ctx, wt)
		}(i, file, marker)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("agent %d: %v", i, e)
		}
	}
	return out, markers
}

// TestOCCDisjointCombinePreservesContent is the adversarial "dropped edit" test.
// N agents each modify a DISTINCT existing file. Every write-set is a singleton
// group, so the whole batch is landed by the OCC parallel combine reduction (the
// log-depth pairwise merge). If that reduction ever dropped one side of a merge,
// that agent's content marker would be missing from the final tree. The existing
// disjoint test only checks file *presence*; this checks *content*, and it also
// asserts the OCC tree is byte-identical to the naive fold.
func TestOCCDisjointCombinePreservesContent(t *testing.T) {
	ctx := context.Background()
	g, pool, base := scenario(t, 32)
	const n = 16 // odd/even splits exercise the "carry up" branch in the reduction
	changes, markers := disjointModifiers(t, pool, base, n)

	// Sanity: the batch really is fully disjoint (n singleton groups).
	if got := len(Partition(changes)); got != n {
		t.Fatalf("expected %d singleton groups, got %d", n, got)
	}

	in := NewIntegrator(pool.Git())
	occ, err := in.IntegrateOCC(ctx, base, changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(occ.Landed) != n || len(occ.Flagged) != 0 {
		t.Fatalf("occ landed=%d flagged=%d, want %d/0", len(occ.Landed), len(occ.Flagged), n)
	}

	// Every agent's content edit must survive the combine reduction.
	for file, marker := range markers {
		content, err := g.ShowFile(ctx, occ.FinalSHA, file)
		if err != nil {
			t.Fatalf("show %s: %v", file, err)
		}
		if !strings.Contains(content, marker) {
			t.Fatalf("OCC combine DROPPED an edit: %s missing %q in final tree %s", file, marker, occ.FinalSHA)
		}
	}

	// And the OCC tree must equal the naive sequential fold byte-for-byte.
	naive, err := in.IntegrateNaive(ctx, base, changes)
	if err != nil {
		t.Fatal(err)
	}
	nt, err := g.TreeOID(ctx, naive.FinalSHA)
	if err != nil {
		t.Fatal(err)
	}
	ot, err := g.TreeOID(ctx, occ.FinalSHA)
	if err != nil {
		t.Fatal(err)
	}
	if nt != ot {
		t.Fatalf("disjoint OCC tree %s != naive tree %s", ot, nt)
	}
}

// TestOCCConflictNotFalselyAutoMerged guards the flagging tier: three agents
// fight over the SAME line of the SAME file. Exactly one may land; the other two
// MUST be flagged (never silently dropped, never falsely counted as auto-merged).
func TestOCCConflictNotFalselyAutoMerged(t *testing.T) {
	ctx := context.Background()
	_, pool, base := scenario(t, 6)
	const n = 3
	changes := make([]BranchChange, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wt, _ := pool.Acquire(ctx, base)
			editDistinctLine("f000.txt", fmt.Sprintf("agent-%d-claims-line-5", i), 5)(wt.Dir)
			_, _ = wt.Git().CommitAll(ctx, fmt.Sprintf("agent %d", i))
			ws, _ := WriteSetFor(ctx, pool.Git(), base, wt.Branch)
			changes[i] = BranchChange{Branch: wt.Branch, WriteSet: ws}
			_ = pool.Release(ctx, wt)
		}(i)
	}
	wg.Wait()

	in := NewIntegrator(pool.Git())
	occ, err := in.IntegrateOCC(ctx, base, changes)
	if err != nil {
		t.Fatal(err)
	}
	if occ.Groups != 1 {
		t.Fatalf("groups=%d, want 1 (same file)", occ.Groups)
	}
	if len(occ.Landed) != 1 {
		t.Fatalf("landed=%d, want exactly 1", len(occ.Landed))
	}
	if len(occ.Flagged) != n-1 {
		t.Fatalf("flagged=%d, want %d", len(occ.Flagged), n-1)
	}
	if occ.AutoMerged != 0 {
		t.Fatalf("autoMerged=%d, want 0 (real conflicts must NOT be auto-merged)", occ.AutoMerged)
	}
	// Landed + flagged must account for every branch (nothing silently dropped).
	if len(occ.Landed)+len(occ.Flagged) != n {
		t.Fatalf("landed+flagged=%d, want %d — a branch vanished", len(occ.Landed)+len(occ.Flagged), n)
	}
}
