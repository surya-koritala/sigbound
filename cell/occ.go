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
	return partition(changes, nil)
}

// PartitionSemantic is Partition PLUS extra union edges: pairs of branch
// names, named by BranchChange.Branch, that some caller-supplied analysis
// determined overlap despite disjoint write-sets (e.g. cmd/sig's opt-in Go
// symbol-level semantic conflict detector — see -semantic go). Every edge
// just feeds the SAME union-find Partition already builds from path overlap;
// the path-overlap logic itself is untouched. An edge naming a branch not
// present in changes is silently ignored (fail-open — a caller's analysis
// may be stale or partial); edges is nil or empty, this is byte-identical to
// Partition.
func PartitionSemantic(changes []BranchChange, edges [][2]string) [][]BranchChange {
	return partition(changes, edges)
}

// partition is the shared implementation behind Partition/PartitionSemantic:
// union by path overlap, THEN union by the extra edges (if any), so semantic
// edges can only ever COARSEN the path-based grouping, never split it.
func partition(changes []BranchChange, edges [][2]string) [][]BranchChange {
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

	if len(edges) > 0 {
		byName := make(map[string]int, n)
		for i := range changes {
			byName[changes[i].Branch] = i
		}
		for _, e := range edges {
			i, iok := byName[e[0]]
			j, jok := byName[e[1]]
			if iok && jok {
				uf.union(i, j)
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
