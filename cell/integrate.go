package cell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// Strategy names selectable at the benchmark boundary. Every strategy produces
// the same correctness guarantees (all non-conflicting changes land; real
// conflicts are flagged, never dropped) and, for a conflict-free batch, the same
// final tree — so they A/B cleanly.
const (
	// StrategyPorcelain is the working-tree baseline: fold each branch onto an
	// integration worktree with `git merge`. Correct, but pays the working-tree +
	// index-lock + per-merge process cost this project exists to eliminate.
	StrategyPorcelain = "porcelain"
	// StrategyNaive folds serially with in-object-store merge-tree (no worktree).
	StrategyNaive = "naive"
	// StrategyMergeTree is the OCC engine on merge-tree everywhere: partition,
	// fold groups in parallel, combine disjoint group heads by merge-tree.
	StrategyMergeTree = "mergetree"
	// StrategyOverlay is the OCC engine with the tree-overlay fast path: disjoint
	// groups are unioned with NO merge at all (object-store index overlay); only
	// genuinely overlapping groups run merge-tree.
	StrategyOverlay = "overlay"
)

// AvailableStrategies lists the strategy names Integrate accepts.
func AvailableStrategies() []string {
	return []string{StrategyPorcelain, StrategyNaive, StrategyMergeTree, StrategyOverlay}
}

// BranchResult records what happened to one branch during integration.
type BranchResult struct {
	Branch    string
	Landed    bool
	Conflicts []string // paths a human must resolve (only when !Landed)
}

// IntegrationResult is the outcome of landing a batch of branches.
type IntegrationResult struct {
	Strategy   string         // "naive" or "occ"
	FinalSHA   string         // commit OID of the integrated tree
	Landed     []string       // branches whose changes are in FinalSHA
	Flagged    []BranchResult // branches set aside for a human (real conflicts)
	AutoMerged int            // landed branches where git 3-way auto-resolved an overlap

	// OCC partition stats (naive reports the degenerate values).
	Groups       int // number of mutually-disjoint groups
	MaxBatch     int // branches landable in parallel == number of groups
	LargestGroup int // serialization depth == largest group size

	// GroupHeads/GroupBranches expose the per-group folded heads for the groups
	// that LANDED at least one branch — exactly the disjoint heads combined into
	// FinalSHA. GroupHeads[i] is that group's head commit OID (or tip, for a
	// singleton), and GroupBranches[i] names the branches it landed. They let a
	// caller cheaply re-overlay a SUBSET of the heads (OverlayTrees) without
	// re-folding — the seam -verify-bisect uses to land a passing subset when
	// the full combined tree fails verify. Populated only by the OCC strategies
	// (overlay, mergetree); the serial baselines (naive, porcelain) leave them
	// nil, since they never partition.
	GroupHeads    []string
	GroupBranches [][]string

	Duration time.Duration
}

// Integrator lands committed agent branches onto a base commit.
//
// Both strategies operate ENTIRELY in the git object store via `git merge-tree`
// + `git commit-tree` — no worktrees, no checkouts, no working-tree locks. This
// is the MVCC-style engine: a merge is a pure function base×ours×theirs -> tree,
// so disjoint merges are embarrassingly parallel. Because both strategies use
// the same fast primitive, the only thing the benchmark compares is the OCC
// idea itself — partition, parallelize, serialize only within a group.
type Integrator struct {
	g        *gitx.Git
	parallel int
	landRef  string   // if set, the final commit is published here via update-ref
	seq      int64    // unique suffix for scratch porcelain worktrees
	resolver Resolver // optional conflict resolver; nil => flag on conflict (default)
	assert   bool     // opt-in overlay-vs-merge-tree cross-check; see WithAssert
	// blobs reads conflict-side blob content (attemptResolve). It defaults to g
	// (a per-call cat-file --batch spawn); cell.Integrate points it at the cell so
	// the reads route through the cell's long-lived daemon instead. Any type with
	// gitx.BlobsBatch's signature satisfies it, so the default and the daemon are
	// interchangeable and the fail-open behavior lives entirely in the cell.
	blobs blobBatcher
	// semanticEdges are extra cross-branch union edges fed into PartitionSemantic
	// on top of path overlap; see WithSemanticEdges. Nil (the default) leaves
	// partitioning exactly path-based.
	semanticEdges [][2]string
}

// blobBatcher is the seam attemptResolve reads conflict blobs through: either a
// raw gitx.Git (a spawn per call) or a cell (its reused cat-file --batch daemon).
type blobBatcher interface {
	BlobsBatch(ctx context.Context, specs []string) (map[string]string, error)
}

// NewIntegrator builds an Integrator over the main repo's git handle. Blob reads
// default to g's per-call spawn; cell.Integrate overrides blobs with the cell so
// they route through its daemon.
func NewIntegrator(g *gitx.Git) *Integrator {
	return &Integrator{g: g, blobs: g, parallel: max(1, runtime.GOMAXPROCS(0))}
}

// WithLandRef makes every strategy publish its final commit to ref (e.g.
// "refs/heads/main") with a single `git update-ref` after integration — the one
// serialized write in an otherwise lock-free, object-only pipeline. Empty ref
// (the default) integrates without moving any ref, leaving FinalSHA a detached
// commit the caller can inspect.
func (in *Integrator) WithLandRef(ref string) *Integrator {
	in.landRef = ref
	return in
}

// WithResolver installs a conflict Resolver. When merge-tree reports a real
// conflict for a branch in an overlapping group, the integrator asks the
// resolver for a body for every conflicted path; if it resolves them ALL, the
// merged tree is rebuilt with the resolved blobs and the branch LANDS. If the
// resolver declines/errors/times out on any path, the branch is FLAGGED exactly
// as before (fail-safe: a partial or garbage resolution is never landed). A nil
// resolver (the default) leaves the flag-on-conflict behavior unchanged.
func (in *Integrator) WithResolver(r Resolver) *Integrator {
	in.resolver = r
	return in
}

// WithAssert turns on the overlay-vs-merge-tree cross-check for
// IntegrateOverlay: after the overlay fast path builds its union tree, the
// SAME group heads are independently recombined via the merge-tree path
// (combineDisjoint — the exact combiner the mergetree strategy uses) and the
// two tree OIDs are compared. A mismatch means the overlay path unioned trees
// it should never have unioned (a partition-invariant bug), and is reported
// as an error naming both OIDs; nothing lands. This roughly doubles
// integration cost (every disjoint combine now runs twice), so it is opt-in —
// paranoia/CI, not the default. combineDisjoint already self-guards for the
// mergetree/occ strategies, so only overlay needs this.
func (in *Integrator) WithAssert() *Integrator {
	in.assert = true
	return in
}

// WithSemanticEdges feeds extra cross-branch grouping edges into the OCC
// strategies' partition step (see PartitionSemantic): pairs of branch names
// a caller's own analysis determined overlap despite disjoint write-sets
// (e.g. cmd/sig's opt-in Go symbol-level semantic conflict detector, -semantic
// go), so they still serialize through the normal overlap path (fold +
// merge-tree + resolver) instead of landing in independent parallel groups.
// Ignored by IntegrateNaive/IntegratePorcelain, which never partition. Nil
// (the default) leaves partitioning exactly path-based, as before.
func (in *Integrator) WithSemanticEdges(edges [][2]string) *Integrator {
	in.semanticEdges = edges
	return in
}

// Integrate dispatches to a named strategy so the benchmark can A/B them. Unknown
// names are an error listing the valid set.
func (in *Integrator) Integrate(ctx context.Context, base string, changes []BranchChange, strategy string) (IntegrationResult, error) {
	switch strategy {
	case StrategyPorcelain:
		return in.IntegratePorcelain(ctx, base, changes)
	case StrategyNaive:
		return in.IntegrateNaive(ctx, base, changes)
	case StrategyMergeTree, "occ":
		res, err := in.IntegrateOCC(ctx, base, changes)
		res.Strategy = strategy
		return res, err
	case StrategyOverlay:
		return in.IntegrateOverlay(ctx, base, changes)
	default:
		return IntegrationResult{}, fmt.Errorf("unknown strategy %q (have %v)", strategy, AvailableStrategies())
	}
}

// land publishes commit to the configured ref (no-op when unset/empty). This is
// the single serialized step: merges/overlays only write objects, so everything
// upstream of here is parallel and lock-free.
func (in *Integrator) land(ctx context.Context, commit string) error {
	if in.landRef == "" || commit == "" {
		return nil
	}
	return in.g.UpdateRef(ctx, in.landRef, commit)
}

// IntegrateNaive is the obvious approach with no OCC: fold every branch onto the
// base one after another. Correct, but strictly serial — it never exploits the
// fact that most branches can never conflict.
func (in *Integrator) IntegrateNaive(ctx context.Context, base string, changes []BranchChange) (IntegrationResult, error) {
	start := time.Now()
	res := IntegrationResult{Strategy: "naive", Groups: 1, MaxBatch: 1, LargestGroup: len(changes)}

	head, landed, flagged, auto, err := in.fold(ctx, base, base, changes, "sigbound: naive")
	if err != nil {
		return res, err
	}
	res.FinalSHA = head
	res.Landed = landed
	res.Flagged = flagged
	res.AutoMerged = auto
	if err := in.land(ctx, res.FinalSHA); err != nil {
		return res, err
	}
	res.Duration = time.Since(start)
	return res, nil
}

// IntegrateOCC is the OCC engine:
//
//   - Partition the branches by write-set overlap into mutually-disjoint groups.
//   - Parallel phase: fold each group independently (goroutines bounded by
//     GOMAXPROCS). Overlaps inside a group are serialized and tiered — git
//     auto-resolves what it can, the rest are flagged for a human. Singleton
//     groups need no merge; their branch head is already the group head.
//   - Combine phase: the group heads are provably disjoint, so they are folded
//     into the final tree with a parallel pairwise reduction (log-depth), every
//     step guaranteed conflict-free.
func (in *Integrator) IntegrateOCC(ctx context.Context, base string, changes []BranchChange) (IntegrationResult, error) {
	start := time.Now()
	groups := PartitionSemantic(changes, in.semanticEdges)

	res := IntegrationResult{Strategy: "occ", Groups: len(groups), MaxBatch: len(groups)}
	for _, g := range groups {
		if len(g) > res.LargestGroup {
			res.LargestGroup = len(g)
		}
	}

	// Resolve every branch tip to a commit OID once through a single batched
	// process (the same helper IntegrateOverlay uses). GroupHeads must carry a
	// real commit OID even for a singleton group, per IntegrationResult's doc
	// comment: -verify-bisect's overlayCandidate re-overlays a SUBSET of these
	// heads via OverlayTrees, whose diff-tree --stdin batching requires a full
	// OID on both sides of its two-tree lines — an unresolved ref name (what
	// used to land here) silently breaks that parse instead of erroring
	// loudly, so this can't be left to the ref-accepting call sites inside
	// this function (MergeTree/CommitTree, which tolerate refs fine).
	tips, err := in.resolveTips(ctx, changes)
	if err != nil {
		return res, err
	}

	// ---- parallel phase: fold each group -> a group head ----
	heads := make([]string, len(groups))
	landedByGroup := make([][]string, len(groups))
	flaggedByGroup := make([][]BranchResult, len(groups))
	autoByGroup := make([]int, len(groups))

	sem := make(chan struct{}, in.parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for gi := range groups {
		g := groups[gi]
		if len(g) == 1 {
			// Singleton: the branch tip is already the group head. No merge.
			heads[gi] = tips[g[0].Branch]
			landedByGroup[gi] = []string{g[0].Branch}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(gi int, g []BranchChange) {
			defer wg.Done()
			defer func() { <-sem }()
			head, landed, flagged, auto, err := in.fold(ctx, base, base, g, "sigbound: occ")
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			heads[gi] = head
			landedByGroup[gi] = landed
			flaggedByGroup[gi] = flagged
			autoByGroup[gi] = auto
		}(gi, g)
	}
	wg.Wait()
	if firstErr != nil {
		return res, firstErr
	}

	for gi := range groups {
		res.Landed = append(res.Landed, landedByGroup[gi]...)
		res.Flagged = append(res.Flagged, flaggedByGroup[gi]...)
		res.AutoMerged += autoByGroup[gi]
		if len(landedByGroup[gi]) > 0 {
			res.GroupHeads = append(res.GroupHeads, heads[gi])
			res.GroupBranches = append(res.GroupBranches, landedByGroup[gi])
		}
	}

	// ---- combine phase: disjoint parallel reduction of the group heads ----
	final, err := in.combineDisjoint(ctx, base, heads)
	if err != nil {
		return res, err
	}
	res.FinalSHA = final
	if err := in.land(ctx, res.FinalSHA); err != nil {
		return res, err
	}
	res.Duration = time.Since(start)
	return res, nil
}

// fold sequentially merges changes onto acc (starting from acc, with mergeBase
// as the common ancestor) using in-memory merge-tree. The accumulator is kept
// as a bare TREE OID for the whole loop — merge-tree accepts tree-ish for both
// sides, only --merge-base needs a commit, and that's mergeBase itself, which
// is fixed for every step here (never recomputed) — so no intermediate
// commit-tree is needed to carry it forward. Exactly ONE commit is created, at
// the end, wrapping the fully-folded tree with acc and every landed branch as
// parents: the group head. When acc == mergeBase (true for every caller today)
// the very first landed branch is a no-op merge — ours has no divergence from
// base, so the result is trivially theirs' tree and can never conflict — so
// that redundant merge-tree call is skipped in favor of a plain tree lookup.
// Conflicts are flagged and skipped, never folded into the accumulator.
// Returns the head commit plus per-branch outcomes.
func (in *Integrator) fold(ctx context.Context, mergeBase, acc string, changes []BranchChange, msgPrefix string) (head string, landed []string, flagged []BranchResult, autoMerged int, err error) {
	if len(changes) == 0 {
		return acc, nil, nil, 0, nil
	}
	landedPaths := NewWriteSet()
	tree := acc // tree accumulator; valid once the first branch lands
	for _, bc := range changes {
		var mt gitx.MergeTreeResult
		if len(landed) == 0 && acc == mergeBase {
			// ours == merge-base: no divergence on our side, so the merge is
			// trivially theirs' tree and cannot conflict. Skip merge-tree.
			t, terr := in.g.TreeOID(ctx, bc.Branch)
			if terr != nil {
				return "", nil, nil, 0, fmt.Errorf("%s tree of %s: %w", msgPrefix, bc.Branch, terr)
			}
			mt = gitx.MergeTreeResult{Tree: t, OK: true}
		} else {
			mt, err = in.g.MergeTree(ctx, mergeBase, tree, bc.Branch)
			if err != nil {
				return "", nil, nil, 0, fmt.Errorf("%s merge-tree %s: %w", msgPrefix, bc.Branch, err)
			}
		}
		outTree := mt.Tree
		if !mt.OK {
			// Real conflict. With no resolver (or a resolver that declines/errors
			// on any path) this branch is flagged for a human — main is never
			// touched. Only a resolution covering EVERY conflicted path lands.
			resolvedTree, ok, rerr := in.attemptResolve(ctx, mergeBase, tree, bc.Branch, mt)
			if rerr != nil {
				return "", nil, nil, 0, fmt.Errorf("%s resolve %s: %w", msgPrefix, bc.Branch, rerr)
			}
			if !ok {
				flagged = append(flagged, BranchResult{Branch: bc.Branch, Conflicts: mt.Conflicts})
				continue
			}
			outTree = resolvedTree
		}
		if bc.WriteSet.Overlaps(landedPaths) {
			autoMerged++
		}
		for _, p := range bc.WriteSet.Paths() {
			landedPaths.Add(p)
		}
		tree = outTree
		landed = append(landed, bc.Branch)
	}
	if len(landed) == 0 {
		return acc, landed, flagged, autoMerged, nil
	}
	head, err = in.g.CommitTree(ctx, tree, append([]string{acc}, landed...), msgPrefix+": "+strings.Join(landed, ", "))
	if err != nil {
		return "", nil, nil, 0, fmt.Errorf("%s commit-tree: %w", msgPrefix, err)
	}
	return head, landed, flagged, autoMerged, nil
}

// attemptResolve tries to turn a conflicted merge-tree result into a clean,
// resolved tree using the configured Resolver. For every conflicted path it
// gathers the three sides' CONTENTS — Base from the merge base, Ours from the
// current landed accumulator, Theirs from the branch — and calls the resolver.
// A path missing on a side (add/add, delete/modify) is passed as empty content.
//
// All base/ours/theirs content for every conflicted path is fetched in ONE
// `git cat-file --batch` process via BlobsBatch (gitx), instead of the 3
// `git cat-file blob` forks per path this used to spawn — 3K processes for K
// conflicts, serialized in fold's loop, collapsed to one. The resolver may
// still decline on the FIRST conflicted path and never see the rest (same
// fail-fast short-circuit as before); only the blob reads are batched ahead
// of that, since they're cheap regardless of how many paths the resolver ends
// up actually needing.
//
// It returns resolved=true only when the resolver resolves EVERY conflicted path;
// then it rebuilds the tree by splicing the resolved blobs onto the conflicted
// merge-tree output (which already carries git's clean auto-merges elsewhere). If
// the resolver is nil, or declines/errors on ANY path, it returns resolved=false
// so the caller flags the branch — main is never touched. err is reserved for a
// genuine git-plumbing failure (blob read, hash-object, write-tree), which the
// caller surfaces loudly rather than landing anything.
func (in *Integrator) attemptResolve(ctx context.Context, mergeBase, ours, theirs string, mt gitx.MergeTreeResult) (tree string, resolved bool, err error) {
	if in.resolver == nil {
		return "", false, nil
	}
	specs := make([]string, 0, len(mt.Conflicts)*3)
	for _, path := range mt.Conflicts {
		specs = append(specs, mergeBase+":"+path, ours+":"+path, theirs+":"+path)
	}
	contents, err := in.blobs.BlobsBatch(ctx, specs)
	if err != nil {
		return "", false, err
	}

	blobs := make([]gitx.ResolvedBlob, 0, len(mt.Conflicts))
	for _, path := range mt.Conflicts {
		// A path absent on a side (add/add, delete/modify) is simply missing
		// from the map — the zero value ("") matches BlobAt's old present=false
		// => empty-content contract.
		baseC := contents[mergeBase+":"+path]
		oursC := contents[ours+":"+path]
		theirsC := contents[theirs+":"+path]
		out, ok, rerr := in.resolver.Resolve(ctx, Conflict{Path: path, Base: baseC, Ours: oursC, Theirs: theirsC})
		if rerr != nil || !ok {
			// Fail-safe: any decline or error on ANY path flags the whole branch.
			return "", false, nil
		}
		blobs = append(blobs, gitx.ResolvedBlob{Path: path, Content: out})
	}
	tree, err = in.g.SpliceBlobs(ctx, mt.Tree, blobs)
	if err != nil {
		return "", false, err
	}
	return tree, true, nil
}

// combineDisjoint folds provably-disjoint heads into a single commit using a
// parallel pairwise reduction. Every pair merges cleanly (disjoint paths, common
// ancestor = base), so the depth is O(log n) and each level is fully parallel.
func (in *Integrator) combineDisjoint(ctx context.Context, base string, heads []string) (string, error) {
	cur := heads
	for len(cur) > 1 {
		next := make([]string, (len(cur)+1)/2)
		sem := make(chan struct{}, in.parallel)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var firstErr error

		for i := 0; i < len(cur); i += 2 {
			if i+1 == len(cur) {
				next[i/2] = cur[i] // odd one out carries up unchanged
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(dst, a, b int) {
				defer wg.Done()
				defer func() { <-sem }()
				mt, err := in.g.MergeTree(ctx, base, cur[a], cur[b])
				if err != nil {
					recordErr(&mu, &firstErr, err)
					return
				}
				if !mt.OK {
					// Must not happen: OCC guarantees group heads are disjoint.
					recordErr(&mu, &firstErr, fmt.Errorf("combine conflict on %v (partition invariant violated)", mt.Conflicts))
					return
				}
				m, err := in.g.CommitTree(ctx, mt.Tree, []string{cur[a], cur[b]}, "sigbound: occ combine")
				if err != nil {
					recordErr(&mu, &firstErr, err)
					return
				}
				next[dst] = m
			}(i/2, i, i+1)
		}
		wg.Wait()
		if firstErr != nil {
			return "", firstErr
		}
		cur = next
	}
	if len(cur) == 0 {
		return base, nil
	}
	return cur[0], nil
}

// overlayAssertErr is the -assert comparison itself, pulled out as a small
// pure function so the mismatch path is directly testable (see WithAssert):
// nil when the overlay combine's tree OID matches the independently
// recomputed merge-tree combine's tree OID, a loud error naming both
// otherwise.
func overlayAssertErr(overlayTree, mergeTreeTree string) error {
	if overlayTree == mergeTreeTree {
		return nil
	}
	return fmt.Errorf("overlay assert: tree mismatch — overlay=%s mergetree=%s (partition invariant violated)", overlayTree, mergeTreeTree)
}

func recordErr(mu *sync.Mutex, dst *error, err error) {
	mu.Lock()
	if *dst == nil {
		*dst = err
	}
	mu.Unlock()
}

// IntegrateOverlay is the OCC engine with the tree-overlay fast path.
//
//   - Partition, then resolve every branch tip to a commit OID through ONE
//     long-running `git cat-file --batch-check` (no per-branch rev-parse spawn).
//   - Parallel phase: singleton groups need NO work — their branch tip IS the
//     group head. Multi-branch groups fold via merge-tree (auto-resolving what
//     git can, flagging real conflicts).
//   - Combine phase: the group heads are provably path-disjoint, so instead of
//     merging them the union tree is built by OVERLAYING each head's changed
//     entries onto base in a scratch object-store index — no 3-way merge at all.
//     This is byte-for-byte identical to the merge-tree combine for disjoint
//     inputs (proven in gitx.TestOverlayTreesEqualsMergeTree and
//     TestOverlayMatchesMergeTreeStrategy). Unlike combineDisjoint (mergetree's
//     combiner), this path has no runtime cross-check by default — WithAssert
//     opts into one, at roughly double the cost.
func (in *Integrator) IntegrateOverlay(ctx context.Context, base string, changes []BranchChange) (IntegrationResult, error) {
	start := time.Now()
	groups := PartitionSemantic(changes, in.semanticEdges)
	res := IntegrationResult{Strategy: StrategyOverlay, Groups: len(groups), MaxBatch: len(groups)}
	for _, g := range groups {
		if len(g) > res.LargestGroup {
			res.LargestGroup = len(g)
		}
	}
	if len(changes) == 0 {
		res.FinalSHA = base
		res.Duration = time.Since(start)
		return res, nil
	}

	// Resolve all tips once through a single batched process.
	tips, err := in.resolveTips(ctx, changes)
	if err != nil {
		return res, err
	}

	heads := make([]string, len(groups))
	landedByGroup := make([][]string, len(groups))
	flaggedByGroup := make([][]BranchResult, len(groups))
	autoByGroup := make([]int, len(groups))

	sem := make(chan struct{}, in.parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for gi := range groups {
		g := groups[gi]
		if len(g) == 1 {
			// Singleton: the branch tip is already the group head. No merge.
			heads[gi] = tips[g[0].Branch]
			landedByGroup[gi] = []string{g[0].Branch}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(gi int, g []BranchChange) {
			defer wg.Done()
			defer func() { <-sem }()
			head, landed, flagged, auto, err := in.fold(ctx, base, base, g, "sigbound: overlay")
			if err != nil {
				recordErr(&mu, &firstErr, err)
				return
			}
			heads[gi] = head
			landedByGroup[gi] = landed
			flaggedByGroup[gi] = flagged
			autoByGroup[gi] = auto
		}(gi, g)
	}
	wg.Wait()
	if firstErr != nil {
		return res, firstErr
	}

	var parents []string
	parents = append(parents, base)
	for gi := range groups {
		res.Landed = append(res.Landed, landedByGroup[gi]...)
		res.Flagged = append(res.Flagged, flaggedByGroup[gi]...)
		res.AutoMerged += autoByGroup[gi]
		if len(landedByGroup[gi]) > 0 {
			parents = append(parents, heads[gi])
			res.GroupHeads = append(res.GroupHeads, heads[gi])
			res.GroupBranches = append(res.GroupBranches, landedByGroup[gi])
		}
	}

	// ---- combine phase: overlay the disjoint group heads onto base ----
	overlayHeads := parents[1:] // group heads that landed something
	unionTree, err := in.g.OverlayTrees(ctx, base, overlayHeads)
	if err != nil {
		return res, fmt.Errorf("overlay combine: %w", err)
	}

	// Opt-in paranoia check (see WithAssert): independently recompute the SAME
	// group heads' combine via merge-tree and require byte-for-byte the same
	// tree. Checked before the final commit-tree below, so a mismatch never
	// lands anything.
	if in.assert {
		mtHead, err := in.combineDisjoint(ctx, base, overlayHeads)
		if err != nil {
			return res, fmt.Errorf("overlay assert: merge-tree recompute: %w", err)
		}
		mtTree, err := in.g.TreeOID(ctx, mtHead)
		if err != nil {
			return res, fmt.Errorf("overlay assert: tree oid of %s: %w", mtHead, err)
		}
		if aerr := overlayAssertErr(unionTree, mtTree); aerr != nil {
			return res, aerr
		}
	}

	final, err := in.g.CommitTree(ctx, unionTree, parents, "sigbound: overlay combine")
	if err != nil {
		return res, err
	}
	res.FinalSHA = final
	if err := in.land(ctx, res.FinalSHA); err != nil {
		return res, err
	}
	res.Duration = time.Since(start)
	return res, nil
}

// resolveTips maps every branch to its commit OID using one long-running
// `git cat-file --batch-check`, so N branches cost one process instead of N
// `git rev-parse` forks.
func (in *Integrator) resolveTips(ctx context.Context, changes []BranchChange) (map[string]string, error) {
	br, err := in.g.NewBatchReader(ctx)
	if err != nil {
		return nil, err
	}
	defer br.Close()
	tips := make(map[string]string, len(changes))
	for _, bc := range changes {
		if _, ok := tips[bc.Branch]; ok {
			continue
		}
		oid, err := br.ResolveCommit(bc.Branch)
		if err != nil {
			return nil, fmt.Errorf("resolve tip %s: %w", bc.Branch, err)
		}
		tips[bc.Branch] = oid
	}
	return tips, nil
}

// IntegratePorcelain is the WORKING-TREE baseline the fast paths are measured
// against: fold every branch onto a scratch integration worktree with `git
// merge`. It is correct (auto-merges what git can, flags real conflicts) but pays
// exactly the costs this engine avoids — a checkout per merge, index locks, and a
// separate merge process per branch. Serial by construction; the partition stats
// are reported degenerate.
func (in *Integrator) IntegratePorcelain(ctx context.Context, base string, changes []BranchChange) (IntegrationResult, error) {
	start := time.Now()
	res := IntegrationResult{Strategy: StrategyPorcelain, Groups: 1, MaxBatch: 1, LargestGroup: len(changes)}
	if len(changes) == 0 {
		res.FinalSHA = base
		res.Duration = time.Since(start)
		return res, nil
	}

	root, err := os.MkdirTemp("", "sig-int-*")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(root)
	intDir := filepath.Join(root, "wt")
	branch := fmt.Sprintf("sig-int-%d", atomic.AddInt64(&in.seq, 1))
	if err := in.g.WorktreeAdd(ctx, intDir, branch, base); err != nil {
		return res, fmt.Errorf("porcelain worktree: %w", err)
	}
	defer func() { _ = in.g.WorktreeRemove(ctx, intDir) }()
	ig := in.g.At(intDir)

	landedPaths := NewWriteSet()
	for _, bc := range changes {
		mr, err := ig.Merge(ctx, "sigbound: porcelain: "+bc.Branch, bc.Branch)
		if err != nil {
			return res, fmt.Errorf("porcelain merge %s: %w", bc.Branch, err)
		}
		if !mr.OK {
			res.Flagged = append(res.Flagged, BranchResult{Branch: bc.Branch, Conflicts: mr.Conflicts})
			continue
		}
		if bc.WriteSet.Overlaps(landedPaths) {
			res.AutoMerged++
		}
		for _, p := range bc.WriteSet.Paths() {
			landedPaths.Add(p)
		}
		res.Landed = append(res.Landed, bc.Branch)
	}

	head, err := ig.HeadSHA(ctx)
	if err != nil {
		return res, err
	}
	res.FinalSHA = head
	if err := in.land(ctx, res.FinalSHA); err != nil {
		return res, err
	}
	res.Duration = time.Since(start)
	return res, nil
}
