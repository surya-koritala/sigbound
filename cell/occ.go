package cell

import "sort"

// BranchChange pairs a committed branch with the write-set it produced against
// the shared base commit. It is the input unit to the OCC engine.
type BranchChange struct {
	Branch   string
	WriteSet *WriteSet
}

// Partition groups branches by write-set overlap using union-find. Two branches
// land in the same group iff their write-sets share a path, transitively: a
// chain A–B–C (A shares with B, B shares with C, A and C disjoint) forms one
// group of three.
//
// The returned groups are mutually DISJOINT — no path appears in two groups — so
// distinct groups can be landed in parallel with zero conflict risk. Branches
// within a group may overlap and must be serialized by the integrator.
//
// The engine is pure and in-memory: an inverted path->branch index feeds a
// union-find, nothing touches git or the store. This is the measured hot path.
//
// Output is deterministic: groups are ordered by their smallest member index,
// and members within a group keep the input order.
func Partition(changes []BranchChange) [][]BranchChange {
	n := len(changes)
	if n == 0 {
		return nil
	}

	uf := newUnionFind(n)

	// Inverted index: first branch index seen to touch a given path. When a
	// later branch touches the same path we union the two. Transitivity is
	// handled by union-find, so we only need the first owner per path.
	owner := make(map[string]int)
	for i := range changes {
		ws := changes[i].WriteSet
		if ws == nil {
			continue
		}
		for p := range ws.paths {
			if j, seen := owner[p]; seen {
				uf.union(i, j)
			} else {
				owner[p] = i
			}
		}
	}

	// Bucket branch indices by their union-find root.
	buckets := make(map[int][]int)
	for i := 0; i < n; i++ {
		r := uf.find(i)
		buckets[r] = append(buckets[r], i)
	}

	// Deterministic ordering: sort groups by smallest member index.
	roots := make([]int, 0, len(buckets))
	for r := range buckets {
		roots = append(roots, r)
	}
	sort.Slice(roots, func(a, b int) bool {
		return minInt(buckets[roots[a]]) < minInt(buckets[roots[b]])
	})

	groups := make([][]BranchChange, 0, len(roots))
	for _, r := range roots {
		idxs := buckets[r]
		sort.Ints(idxs) // preserve input order within a group
		g := make([]BranchChange, 0, len(idxs))
		for _, i := range idxs {
			g = append(g, changes[i])
		}
		groups = append(groups, g)
	}
	return groups
}

func minInt(xs []int) int {
	m := xs[0]
	for _, x := range xs[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

// unionFind is a classic disjoint-set with path compression and union by rank.
type unionFind struct {
	parent []int
	rank   []int
}

func newUnionFind(n int) *unionFind {
	uf := &unionFind{parent: make([]int, n), rank: make([]int, n)}
	for i := range uf.parent {
		uf.parent[i] = i
	}
	return uf
}

func (uf *unionFind) find(x int) int {
	for uf.parent[x] != x {
		uf.parent[x] = uf.parent[uf.parent[x]] // path halving
		x = uf.parent[x]
	}
	return x
}

func (uf *unionFind) union(a, b int) {
	ra, rb := uf.find(a), uf.find(b)
	if ra == rb {
		return
	}
	if uf.rank[ra] < uf.rank[rb] {
		ra, rb = rb, ra
	}
	uf.parent[rb] = ra
	if uf.rank[ra] == uf.rank[rb] {
		uf.rank[ra]++
	}
}
