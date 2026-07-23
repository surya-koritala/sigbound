# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Before 1.0.0, minor versions may add features and patch versions carry fixes.

## [Unreleased]

### Added

- **`sig serve`** — a thin, single-process HTTP run API over the same `driveRun`
  orchestration `sig run` uses (no engine fork), so the verify gate holds by
  construction: serve adds no new landing path. `sig serve -repos a,b` opens each
  repo as a cell; `POST /runs` starts a run asynchronously and returns `202
  {runId}`, `GET /runs/{id}` returns its status and full report once done, `GET
  /runs` lists history, and `GET /runs/{id}/events` streams the run's NDJSON
  events. Each run's report and event stream are written under the target repo's
  `.git/sigbound/runs/<runId>/` (the same `.git/sigbound` storage `-verify-cache`
  uses), so history survives a restart — the GET endpoints read from disk. One
  run per cell at a time (a second concurrent run for a cell is `409 Conflict`);
  different cells run fully in parallel. Binds loopback by default and refuses a
  non-loopback `-addr` without `-allow-remote`; a non-loopback bind also requires
  the shared bearer token (`-token-env`, constant-time compared). It ships no TLS
  and no user model — a single-user daemon, not a multi-tenant service. Runs
  default to `-env-mode scoped` (a daemon must not leak its environment). See
  [docs/USAGE.md](docs/USAGE.md) "`sig serve`".
- **`sig export` / `sig import`** — git-bundle object transport for distributed
  runs. A worker `sig export -bundle FILE -branches a,b,c` packs branches into
  one bundle file (git's native, server-free offline transport); a coordinator
  `sig import -bundle FILE [-from WORKER_ID]` verifies and unbundles it, landing
  every carried branch under an isolated `imported/<worker>/<branch>` namespace —
  so a bundle can never move the coordinator's `main` or clobber a local
  `agent/*` ref. Imported branches feed the existing `sig integrate` unchanged.
  The bundle is verified before unbundling (a corrupt bundle imports nothing),
  and the round-trip is lossless (imported trees are byte-for-byte the worker's).
  Moving the file is out of scope — use scp/NFS/artifacts. See
  [docs/USAGE.md](docs/USAGE.md) "Distributed workflow (bundles)".
- **`-env-mode scoped`** — per-slot environment scoping: each command slot gets
  a minimal base environment plus its own `SIGBOUND_*` vars, with per-slot
  `-env-*` allowlists (exact names and `NAME_*` families) for anything extra.
  Default `inherit` is unchanged.

### Fixed

- `-verify-bisect` under `-strategy mergetree` salvaged nothing on
  fully-disjoint batches: singleton group heads were left as branch ref names
  instead of commit OIDs, so every bisect candidate failed to build (fail-safe
  — nothing wrong ever landed, but green subsets were never salvaged).

## [0.3.0] - 2026-07-22

The differentiators: salvage landing, provenance and replay, resumable runs,
publishing, a reusable GitHub Action, and engine speedups — every landing
still gated on a verify of the exact tree that lands.

### Added

- **`-verify-bisect`** — when the combined tree fails verify (after the repair
  loop has had first shot), bisect over the integration groups and land the
  union of the green ones, but only after that exact union tree passes its own
  verify; interaction failures land nothing. Dropped groups are reported as
  `droppedByBisect`, distinct from conflicts.
- **`-verify-cache`** — opt-in cache of green verify verdicts keyed by
  (tree OID, resolved command + impact scope, sigbound version), stored under
  the repo's git dir; only passes are ever cached, failures always re-run.
- **Run manifest and provenance** — the report now records the resolved
  commands, version, and start time; `-manifest FILE` writes it, `-notes`
  attaches it as a git note under `refs/notes/sigbound` on the landed commit.
- **`sig replay`** — re-integrates a manifest's recorded base and branches
  (excluding any dropped by bisect) and compares tree OIDs: REPRODUCED,
  DIVERGED, or a repo-state error; read-only, never moves refs.
- **`-resume`** — reuse surviving `agent/<id>` branches from a prior run's
  manifest, re-running only failed or no-op tasks; refuses loudly if the base
  has moved past the recorded baseSHA.
- **`-publish`** — a bring-your-own command that runs once after a landed run,
  receiving the JSON report on stdin plus `SIGBOUND_FINAL_SHA` and friends;
  publish failure never unlands (new exit code 6).
- **GitHub Action** — a composite action at the repo root installs a released
  `sig` (checksum-verified), runs `sig doctor`, assembles `sig run` from typed
  inputs, and surfaces exit code, final SHA, and report as outputs.
- **Differential engine fuzzer** — `FuzzStrategiesAgree` drives random bounded
  scenarios through all four strategies and fails on any tree or
  landed/flagged disagreement with porcelain; wired into CI's fuzz smoke.

### Changed

- **Fold on tree OIDs** — the overlapping-group fold keeps its accumulator as
  a tree and emits one octopus commit per group instead of a commit per
  branch; the pure-fold path is 33–53% faster at 256 agents, with byte-
  identical trees.
- **Batched resolver reads and a reused verify worktree** — conflicted-path
  contents come from one `cat-file --batch` per batch, and verify retries and
  repair re-verifies share one worktree, reset hard and cleaned between every
  use so neither tracked nor untracked state can leak across attempts.

## [0.2.0] - 2026-07-22

Hardening of the single-machine CLI: CI-friendly exit codes and events,
robustness knobs for flaky suites and hung agents, preflight checks, presets,
and a release pipeline. Zero module dependencies as of this release.

### Added

- **Exit codes for CI** — `sig run` returns distinct codes: 0 landed+verified,
  1 operational error, 2 usage, 3 verify failed (nothing landed), 4 conflicts
  flagged, 5 no agent succeeded.
- **`sig doctor`** — preflight that validates the git version (>= 2.38) and
  live-probes the exact `merge-tree --write-tree` and overlay plumbing the
  engine depends on; `sig run`/`sig integrate` now do the cheap version check
  up front.
- **`-verify-retries`** — re-run a failing verify on the same tree and pass on
  any green; a flaky pass is surfaced as `flaky` in the report. `-verify` is
  documented as requiring determinism.
- **`-agent-timeout`, `-agent-retries`, `-budget`** — per-agent wall clock,
  retries in fresh worktrees (lane strays and branch collisions are terminal),
  and a hard run-wide ceiling; nothing partial ever lands on exhaustion.
- **`-verify-impact`** — scoped verification: maps the landed write-set to Go
  packages, expands to reverse dependents (including test-only importers), and
  runs a narrower command with `SIGBOUND_IMPACTED_PKGS`; any doubt (non-Go
  changes, `go.mod`, testdata, unmapped dirs, `go list` errors) falls back to
  the full `-verify`.
- **`-events`** — an NDJSON lifecycle stream (run/agent/integrate/verify/
  repair/land start+done, per-phase wall times) for driving CI and dashboards.
- **`-logdir`** — full per-command stdout+stderr streamed to per-agent/verify/
  repair/planner files; a failing log file can never fail the run.
- **`-dry-run`** — print the plan and the predicted partition (computed by the
  real partitioner from declared file-sets) without running any agent.
- **`sig.conf` config file** (`-config`) — a flat `KEY=VALUE` flags file with
  CLI > config > default precedence and loud unknown-key errors.
- **Presets** — `-agent-preset`/`-repair-preset`/`-planner-preset`
  (`claude|codex|aider`) and `-verify-preset` (`go|node|python|rust`) expand to
  known-good command strings; a raw flag always wins.
- **`-keep-failed`** — keep a failed agent's worktree for inspection.
- **`-min-tasks`** and strict-lane default for planned runs — a planner that
  under-delivers fails before any agent runs; planned tasks get `-lanes strict`
  unless overridden.
- **`-assert`** — opt-in cross-check that recombines the overlay result via
  merge-tree and refuses to land on any tree mismatch.
- **Partial report on error** — a mid-run failure still emits the report for
  completed agents, so their branches are recoverable.
- **Release pipeline** — GoReleaser config and a tag-triggered workflow
  producing versioned binaries for linux/darwin (amd64+arm64) and windows
  (amd64), with checksums and a Homebrew tap hook.

### Changed

- **Write-set reuse and batched diffs** — `sig run` reuses each agent's
  computed write-set for partitioning; `sig integrate` computes all branches'
  write-sets in one batched `git diff-tree --stdin` pass.
- **Zero dependencies** — the unused SQLite-backed bench store was removed;
  `go.mod` now has no requires and the CLI links stdlib only.

### Fixed

- An agent whose write-set diff fails is now failed loudly and excluded from
  integration (a wrong empty write-set could previously let overlapping
  content land silently).
- Retry branch resets are gated on this-run creation, so a leftover
  `agent/<id>` branch from a prior run is never silently reset.
- External commands are bounded with `WaitDelay` uniformly, so a hung
  grandchild holding an inherited pipe cannot defeat timeouts.

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

[Unreleased]: https://github.com/surya-koritala/sigbound/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/surya-koritala/sigbound/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/surya-koritala/sigbound/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/surya-koritala/sigbound/releases/tag/v0.1.0
