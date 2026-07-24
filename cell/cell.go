package cell

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// Cell is one repo's unit of work: it owns that repo's git handle, serializes
// the repo's worktree-admin mutations, and runs integrations against it under a
// stable id. One repo = one Cell = the unit of horizontal scale — N repos are N
// cells side by side in one process, and because independent repos have fully
// disjoint object stores and refs there is zero cross-cell coordination. The
// upcoming bundles transport (#59) and `sig serve` (#60) address cells by id;
// `sig run` drives exactly one.
//
// Concurrency: a Cell is safe to use from many goroutines, and DIFFERENT cells
// run fully in parallel (the point of repo-level sharding). A single cell
// serializes ITS OWN worktree-admin mutations — AddWorktree/RemoveWorktree/
// DeleteBranch/Close — behind one mutex, exactly as `sig run`'s driveRun used
// to serialize its ad-hoc admin steps; the agents' edit+commit work and every
// integration happen outside that lock, in parallel. Integrate holds no
// cross-call state, so concurrent integrations on one cell don't corrupt each
// other.
//
// Two cells on the SAME repo path are allowed but pointless: they don't share
// this in-process mutex, so their worktree-admin steps can interleave and race
// on git's own worktree/ref locking. The sharding unit is the whole repo — one
// cell per repo — so this package builds no coordination for that case (git's
// on-disk locking is the only backstop). Just don't do it.
type Cell struct {
	id   string
	repo string // resolved absolute repo path
	git  *gitx.Git

	// mu serializes this cell's worktree-admin mutations and guards created.
	// Distinct cells never share it, so N cells on N repos never contend.
	mu      sync.Mutex
	created map[string]struct{} // worktree dirs this cell added and hasn't removed

	// blobMu guards the lazy-started, reused cat-file --batch daemon (see
	// BlobsBatch). It is SEPARATE from mu so blob reads and worktree-admin never
	// block each other; it also serializes the daemon's strictly-sequential wire
	// protocol (only one Read may be in flight). blob is nil until first use and
	// after any error (which resets it so the next call restarts it).
	blobMu sync.Mutex
	blob   *gitx.BatchBlobReader
}

// Option configures a Cell at Open time.
type Option func(*Cell)

// WithID sets an explicit cell id instead of the path-derived default.
func WithID(id string) Option { return func(c *Cell) { c.id = id } }

// Open resolves repoPath, runs the cheap git preflight (see
// gitx.CheckMinVersion), confirms the path is a git repository, and derives the
// cell id (a short hash of the absolute repo path unless WithID overrides it).
// It creates no worktrees — the caller drives those through AddWorktree.
func Open(repoPath string, opts ...Option) (*Cell, error) {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path %q: %w", repoPath, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("open cell: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("open cell: %s is not a directory", abs)
	}
	ctx := context.Background()
	// Same cheap preflight sig run/integrate do before touching a repo: git
	// present and new enough for the merge-tree/overlay plumbing the engine
	// hard-depends on. Cheap enough to run per Open.
	if err := gitx.CheckMinVersion(ctx, "git"); err != nil {
		return nil, err
	}
	g := gitx.New(abs)
	// Confirm abs actually is a git repo (rev-parse fails outside one), so a
	// non-repo is rejected loudly at Open rather than deep inside the first
	// worktree add or integration.
	if _, err := g.GitCommonDir(ctx); err != nil {
		return nil, fmt.Errorf("open cell: %s is not a git repository: %w", abs, err)
	}
	c := &Cell{repo: abs, git: g, created: map[string]struct{}{}}
	for _, o := range opts {
		o(c)
	}
	if c.id == "" {
		c.id = deriveID(abs)
	}
	return c, nil
}

// deriveID is the default cell id: a short, stable hash of the repo's absolute
// path, so the same repo always maps to the same cell id across processes and
// two different repos never collide.
func deriveID(absRepo string) string {
	sum := sha256.Sum256([]byte(absRepo))
	return hex.EncodeToString(sum[:])[:12]
}

// ID returns the cell's stable id.
func (c *Cell) ID() string { return c.id }

// Repo returns the cell's resolved absolute repo path.
func (c *Cell) Repo() string { return c.repo }

// Git returns the cell's main-repo git handle (for diffs, rev-parse, the
// detached verify/repair worktrees driveRun manages itself, etc.).
func (c *Cell) Git() *gitx.Git { return c.git }

// BlobsBatch resolves object specs to blob content (the same map contract as
// gitx.BlobsBatch) through this cell's ONE long-lived `git cat-file --batch`
// process, lazily started and reused across every call so a busy cell — semantic
// analysis reading two blobs per branch, the review three-sides endpoint hit
// repeatedly, a resolver's per-conflict reads — resolves blobs WITHOUT a git
// process per operation (measured ~15ms of spawn overhead per call on macOS,
// which the daemon collapses to a sub-millisecond pipe round-trip).
//
// It is FAIL-OPEN: any daemon failure — it won't start, or a read desyncs its
// wire stream — discards the wedged process and satisfies the call with a fresh
// per-call spawn (gitx.BlobsBatch), then lets the next call restart the daemon.
// A broken daemon therefore only forfeits the speedup; it can never fail a run.
// The daemon is closed in Close.
//
// blobMu serializes the whole operation, which the strictly-sequential --batch
// protocol requires anyway; concurrent callers (e.g. parallel integrations on
// one cell) block here rather than corrupt each other's records.
func (c *Cell) BlobsBatch(ctx context.Context, specs []string) (map[string]string, error) {
	if len(specs) == 0 {
		return map[string]string{}, nil
	}
	c.blobMu.Lock()
	defer c.blobMu.Unlock()
	if c.blob == nil {
		br, err := c.git.NewBatchBlobReader()
		if err != nil {
			// Couldn't even start it: fall back to a spawn and stay nil so the next
			// call retries the start.
			return c.git.BlobsBatch(ctx, specs)
		}
		c.blob = br
	}
	// Bound the read by ctx. An alive-but-silent daemon (accepts the request, never
	// answers) would otherwise block here forever WHILE HOLDING blobMu, stalling
	// Close and every other blob read on the cell. AfterFunc Kills the process on
	// ctx cancellation, which makes the blocked Read error out so the fallback
	// below engages (and the spawn fallback then honors the cancelled ctx by
	// failing fast); stop() disarms it on the happy path so a completed call never
	// kills a healthy daemon.
	stop := context.AfterFunc(ctx, c.blob.Cancel)
	m, err := c.blob.Read(specs)
	stop()
	if err == nil {
		return m, nil
	}
	// The daemon desynced or was cancelled mid-stream — its position/health is now
	// unknown. Discard it (next call restarts a clean one) and satisfy THIS call
	// with a fresh spawn (which fails fast if ctx is the thing that was cancelled).
	_ = c.blob.Close()
	c.blob = nil
	return c.git.BlobsBatch(ctx, specs)
}

// AddWorktree creates a worktree at dir on branch off base and returns only once
// its working tree is fully populated. It runs in two phases so the O(tree size)
// checkout no longer serializes every agent behind this cell's mutex:
//
//  1. UNDER c.mu (the SAME serialization the whole add used to hold): `git
//     worktree add --no-checkout` — the half that mutates shared git state, the
//     branch ref + packed-refs + .git/worktrees/<name> metadata. When reset is
//     false it uses the loud-fail `-b` (never silently resets a pre-existing
//     branch); reset true uses `-B` and is ONLY safe for a branch this same run
//     already created (see gitx.WorktreeAddResetNoCheckout). dir is registered in
//     created here, still under the lock, so Close/gc can tear down even a
//     worktree whose populate below fails or is cut short.
//  2. OUTSIDE the lock: gitx.WorktreePopulate (`git reset --hard`) fills this
//     worktree's OWN index + files from HEAD. It reads only the shared object
//     store and moves no ref, so distinct worktrees populate in parallel with no
//     new race surface — that parallelism is the whole point of the split.
//
// The caller's contract is unchanged: a nil return is a fully-populated worktree
// at dir on branch; a non-nil return leaves NO usable worktree. A populate
// failure (or a ctx cancelled between the phases) tears the half-made worktree
// down — best-effort, uncancellable — and forgets it from created before
// returning the error, so no half-populated tree ever survives as "created OK".
func (c *Cell) AddWorktree(ctx context.Context, dir, branch, base string, reset bool) error {
	c.mu.Lock()
	var err error
	if reset {
		err = c.git.WorktreeAddResetNoCheckout(ctx, dir, branch, base)
	} else {
		err = c.git.WorktreeAddNoCheckout(ctx, dir, branch, base)
	}
	if err != nil {
		c.mu.Unlock()
		return err
	}
	c.created[dir] = struct{}{}
	c.mu.Unlock()

	if err := c.git.At(dir).WorktreePopulate(ctx); err != nil {
		// Half-made worktree: tear it down and forget it (RemoveWorktree drops it
		// from created too). WithoutCancel so cleanup still runs when ctx is the
		// very thing that failed the populate between the two phases.
		_ = c.RemoveWorktree(context.WithoutCancel(ctx), dir)
		return fmt.Errorf("populate worktree %s on %s: %w", dir, branch, err)
	}
	return nil
}

// RemoveWorktree tears down a worktree dir this cell created (the branch it
// pointed at survives in the shared store). Serialized against this cell's
// other admin mutations. The dir is forgotten from Close's tracking even if the
// underlying removal errors — a caller that asked to remove it does not want it
// retried by Close.
func (c *Cell) RemoveWorktree(ctx context.Context, dir string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.created, dir)
	return c.git.WorktreeRemove(ctx, dir)
}

// DeleteBranch force-deletes a local branch ref, serialized against this cell's
// worktree-admin mutations (a ref mutation that must not race a concurrent
// worktree add on the same repo). Only the ref is removed; any commit it pointed
// at stays in the object store. See gitx.BranchDelete.
func (c *Cell) DeleteBranch(ctx context.Context, branch string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.git.BranchDelete(ctx, branch)
}

// Integrate runs strategy over changes on base through this cell's git handle —
// the SAME engine sig integrate uses (see Integrator), now addressable per-cell.
// opts apply the optional land-ref / assert / resolver knobs via the Integrator
// With* builders. Holds no cross-call state, so it is safe to run concurrently
// (on one cell or across cells).
func (c *Cell) Integrate(ctx context.Context, base string, changes []BranchChange, strategy string, opts ...func(*Integrator)) (IntegrationResult, error) {
	in := NewIntegrator(c.git)
	for _, o := range opts {
		o(in)
	}
	in.blobs = c // resolver blob reads route through this cell's daemon (fail-open)
	return in.Integrate(ctx, base, changes, strategy)
}

// Close tears down every worktree this cell created and has not yet removed,
// leaving branches intact. Idempotent: a second call (or one after the caller
// already removed everything) finds nothing to do. It returns the first removal
// error, if any, but always finishes forgetting every tracked worktree so a
// retry never re-touches a half-removed one.
func (c *Cell) Close(ctx context.Context) error {
	// Reap the cat-file --batch daemon (if ever started) under its own lock —
	// distinct from mu, so this never blocks on or behind worktree-admin.
	c.blobMu.Lock()
	if c.blob != nil {
		_ = c.blob.Close()
		c.blob = nil
	}
	c.blobMu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	var firstErr error
	for dir := range c.created {
		if err := c.git.WorktreeRemove(ctx, dir); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(c.created, dir)
	}
	return firstErr
}
