package cell

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// WorktreePool hands out isolated git worktrees that all share one object store.
//
// This is the substrate that makes parallel agents cheap: `git worktree add`
// gives each agent its own working directory on its own branch off a base
// commit, while the packed/loose objects live once in the main repo. Agents
// never contend on the working tree, and because every agent commits to a
// distinct branch ref there is no shared-ref write contention on the commit path
// either — the property the benchmark measures.
type WorktreePool struct {
	git  *gitx.Git // handle on the main repo (owns the object store)
	root string    // directory under which worktree dirs are created

	// git serializes worktree-admin mutations internally, but we also guard
	// Acquire/Release so concurrent callers get distinct dirs/branch names and
	// never race on the pool's own bookkeeping. This lock covers only the
	// (fast) add/remove admin step — the agent's edit+commit work happens
	// outside it, fully in parallel.
	mu  sync.Mutex
	seq int64
}

// NewWorktreePool builds a pool. mainRepoDir must be an initialized git repo;
// worktreeRoot is where per-agent working directories are created.
func NewWorktreePool(mainRepoDir, worktreeRoot string) *WorktreePool {
	return &WorktreePool{git: gitx.New(mainRepoDir), root: worktreeRoot}
}

// Git returns the main-repo handle (for diffs, integration, etc.).
func (p *WorktreePool) Git() *gitx.Git { return p.git }

// Worktree is one leased working directory on its own branch.
type Worktree struct {
	Dir    string
	Branch string
	pool   *WorktreePool
}

// Git returns a handle scoped to this worktree's directory.
func (wt *Worktree) Git() *gitx.Git { return wt.pool.git.At(wt.Dir) }

// Acquire creates a fresh worktree on a new branch off base, with a full
// checkout of the base tree (the spec-faithful, agent-realistic path). Branch/
// dir names are unique per pool via an atomic counter.
func (p *WorktreePool) Acquire(ctx context.Context, base string) (*Worktree, error) {
	return p.acquire(ctx, base, false)
}

// AcquireSparse is like Acquire but leaves the working directory empty (index
// still fully populated from base). Agents materialize only the files they touch
// via Worktree.Git().CheckoutPaths. This keeps benchmark setup O(1) per agent
// instead of O(files), so timings reflect git plumbing rather than checkout I/O.
func (p *WorktreePool) AcquireSparse(ctx context.Context, base string) (*Worktree, error) {
	return p.acquire(ctx, base, true)
}

func (p *WorktreePool) acquire(ctx context.Context, base string, sparse bool) (*Worktree, error) {
	n := atomic.AddInt64(&p.seq, 1)
	branch := fmt.Sprintf("agent/wt-%d", n)
	dir := filepath.Join(p.root, fmt.Sprintf("wt-%d", n))

	p.mu.Lock()
	var err error
	if sparse {
		err = p.git.WorktreeAddSparse(ctx, dir, branch, base)
	} else {
		err = p.git.WorktreeAdd(ctx, dir, branch, base)
	}
	p.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("acquire worktree: %w", err)
	}
	return &Worktree{Dir: dir, Branch: branch, pool: p}, nil
}

// Release removes the worktree's directory. The branch it created is preserved
// in the shared store so its commits remain integratable.
func (p *WorktreePool) Release(ctx context.Context, wt *Worktree) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.git.WorktreeRemove(ctx, wt.Dir)
}
