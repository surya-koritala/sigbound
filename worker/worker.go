// Package worker runs a unit of agent work inside an isolated worktree and turns
// it into a committed branch. This is the plug point where a real AI agent (or
// any process) attaches: give it a worktree directory, let it edit files, then
// commit. For the benchmark the "agent" is a deterministic file-editing func,
// but ShellTask shows the same shape works for an arbitrary external command.
package worker

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

// Task performs edits inside a worktree directory. Returning an error aborts the
// worker before it commits. This is intentionally just `func(dir) error` so a
// real agent, a script, or a test can all satisfy it.
type Task func(ctx context.Context, dir string) error

// ShellTask adapts a shell command line into a Task. The command runs with the
// worktree as its working directory. (Provided to demonstrate pluggability; the
// benchmark uses in-process Tasks to keep timing about git, not shells.)
func ShellTask(cmdline string) Task {
	return func(ctx context.Context, dir string) error {
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("shell task %q: %w: %s", cmdline, err, out)
		}
		return nil
	}
}

// Result is what one worker produced.
type Result struct {
	Branch string
	SHA    string
}

// Worker owns an identity and the shared pool.
type Worker struct {
	ID   int
	Pool *cell.WorktreePool
}

// Run acquires a worktree off base, runs the task, commits the result to the
// worker's own branch, then releases the worktree (keeping the branch). It
// returns the branch name and commit SHA so the integrator can later compute a
// write-set and land it.
func (w *Worker) Run(ctx context.Context, base string, task Task, commitMsg string) (Result, error) {
	wt, err := w.Pool.Acquire(ctx, base)
	if err != nil {
		return Result{}, err
	}
	// Best-effort release; the branch persists regardless.
	defer func() { _ = w.Pool.Release(ctx, wt) }()

	if err := task(ctx, wt.Dir); err != nil {
		return Result{}, fmt.Errorf("worker %d task: %w", w.ID, err)
	}
	sha, err := wt.Git().CommitAll(ctx, commitMsg)
	if err != nil {
		return Result{}, fmt.Errorf("worker %d commit: %w", w.ID, err)
	}
	return Result{Branch: wt.Branch, SHA: sha}, nil
}

// WriteSetFor computes the branch's write-set against base. Kept here so callers
// have one place to turn a worker Result into OCC input.
func WriteSetFor(ctx context.Context, g *gitx.Git, base, branch string) (*cell.WriteSet, error) {
	paths, err := g.DiffNameOnly(ctx, base, branch)
	if err != nil {
		return nil, err
	}
	return cell.NewWriteSet(paths...), nil
}
