package cell

import "sort"

// WriteSet is the set of repository paths a branch changed relative to its base
// commit. It is the atom the OCC engine reasons about: two branches can only
// conflict if their write-sets share a path, so disjoint write-sets are provably
// safe to land in parallel.
type WriteSet struct {
	paths map[string]struct{}
}

// NewWriteSet builds a WriteSet from the given paths.
func NewWriteSet(paths ...string) *WriteSet {
	w := &WriteSet{paths: make(map[string]struct{}, len(paths))}
	for _, p := range paths {
		w.paths[p] = struct{}{}
	}
	return w
}

// Add inserts a path.
func (w *WriteSet) Add(p string) { w.paths[p] = struct{}{} }

// Len reports how many distinct paths the set holds.
func (w *WriteSet) Len() int { return len(w.paths) }

// Contains reports whether p is in the set.
func (w *WriteSet) Contains(p string) bool {
	_, ok := w.paths[p]
	return ok
}

// Paths returns the paths in sorted order (deterministic for tests / display).
func (w *WriteSet) Paths() []string {
	out := make([]string, 0, len(w.paths))
	for p := range w.paths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Overlaps reports whether the two write-sets share at least one path. It walks
// the smaller set for O(min(|a|,|b|)) work — this is the hot comparison the
// partitioner leans on, so it stays allocation-free.
func (w *WriteSet) Overlaps(other *WriteSet) bool {
	a, b := w, other
	if len(a.paths) > len(b.paths) {
		a, b = b, a
	}
	for p := range a.paths {
		if _, ok := b.paths[p]; ok {
			return true
		}
	}
	return false
}
