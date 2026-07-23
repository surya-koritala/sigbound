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

// AddWorktree creates a worktree at dir on branch off base, serialized against
// this cell's other admin mutations and tracked so Close can tear it down. When
// reset is false it uses the loud-fail `git worktree add -b` (never silently
// resets a pre-existing branch); reset true uses `-B` and is ONLY safe for a
// branch this same run already created (see gitx.WorktreeAddReset).
func (c *Cell) AddWorktree(ctx context.Context, dir, branch, base string, reset bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	if reset {
		err = c.git.WorktreeAddReset(ctx, dir, branch, base)
	} else {
		err = c.git.WorktreeAdd(ctx, dir, branch, base)
	}
	if err != nil {
		return err
	}
	c.created[dir] = struct{}{}
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
	return in.Integrate(ctx, base, changes, strategy)
}

// Close tears down every worktree this cell created and has not yet removed,
// leaving branches intact. Idempotent: a second call (or one after the caller
// already removed everything) finds nothing to do. It returns the first removal
// error, if any, but always finishes forgetting every tracked worktree so a
// retry never re-touches a half-removed one.
func (c *Cell) Close(ctx context.Context) error {
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
