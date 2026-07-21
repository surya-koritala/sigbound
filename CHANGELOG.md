# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Before 1.0.0, minor versions may add features and patch versions carry fixes.

## [Unreleased]

## [0.1.0] - 2026-07-21

Initial public release.

### Added

- **Parallel-agent orchestration** (`sig run`) — split work into independent
  tasks and run an agent on each in its own git worktree, driven either from a
  tasks file (`-tasks`) or from a goal (`-goal`) that a planner command expands.
- **Integration engine** (`sig integrate`) with two strategies: `overlay` (a
  tree-overlay fast path in git's object database) and `mergetree` (a.k.a.
  `occ`, optimistic concurrency via `git merge-tree`). Non-conflicting branches
  are combined in parallel, partitioned by each branch's write-set.
- **Fail-safe AI conflict resolver** (`-resolver`) — a command resolves
  overlapping changes; empty output, a non-zero exit, or a timeout leaves the
  branch flagged for a human rather than guessing.
- **Verify-gated merge** (`-verify`) — nothing lands unless the combined result
  passes the build and test command you supply.
- **Self-healing repair loop** (`-repair`, `-repair-max`) — a merge that fails
  verification is routed back to an agent to fix, then re-checked.
- **File-lane enforcement** (`-lanes off|warn|strict`) — each task declares the
  files it may touch; an agent that writes outside its lane is rejected.
- **Auto-planner** — a goal plus a `-planner` command produces the task list,
  so a single objective can fan out into parallel work.
- **`sigbench` A/B benchmark** — measures parallel integration against a
  sequential `git merge` baseline, verifying correctness on every run.
- **`sig version`** — reports the version, and the git commit and build date
  when built from a checkout.

[Unreleased]: https://github.com/surya-koritala/sigbound/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/surya-koritala/sigbound/releases/tag/v0.1.0
