# Usage

Complete reference for the `sig` CLI. For a five-minute walkthrough on your own
repository, see [`examples/`](../examples/). For the shape of the whole
pipeline, see the [README](../README.md#how-it-works).

`sig` has four subcommands:

```
sig run        run agents on a repo and integrate their work (the driver)
sig integrate  integrate a set of existing branches (the engine, standalone)
sig doctor     check that git is new enough and its plumbing actually works
sig version    print the version, git commit, and build date
```

Everything runs on top of the `git` binary. `sig` never starts a server and
never modifies your repository except to advance the base branch to the final,
verified commit (unless you tell it not to).

---

## `sig run`

Splits work into tasks, runs an agent on each in its own worktree, integrates
the results, and optionally verifies and repairs the merged tree.

```
sig run -repo PATH -base BRANCH
        (-tasks FILE | -goal STRING -planner CMD [-n N] [-min-tasks N])
        -agent CMD [-agent-timeout D] [-agent-retries N]
        [-strategy overlay] [-assert]
        [-resolver CMD] [-resolver-timeout D]
        [-verify CMD [-verify-retries N] [-repair CMD [-repair-max N]]]
        [-lanes off|warn|strict]
        [-no-autocommit]
        [-keep-failed]
        [-budget D]
        [-logdir DIR]
        [-events FILE]
        [-json]
```

### Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `-repo` | *(required)* | Path to the target git repository. |
| `-base` | `main` | Branch the agents fork from and the result lands onto. |
| `-tasks` | — | JSON file describing the tasks (see below). Mutually exclusive with `-goal`. |
| `-goal` | — | Natural-language goal; the `-planner` turns it into disjoint tasks. Mutually exclusive with `-tasks`. |
| `-planner` | — | Planner command (run via `sh -c`). Required with `-goal`. |
| `-n` | `4` | Number of parallel tasks the planner should produce from `-goal`. |
| `-min-tasks` | `0` | Minimum tasks a `-goal` plan must produce; fewer fails **before any agent runs** (`0` = no floor). Must be `<= -n`. |
| `-planner-timeout` | `120s` | Timeout for the planner command (`0` = none). |
| `-agent` | *(required)* | Command (run once per task, via `sh -c`) that edits files in the task's worktree. |
| `-agent-timeout` | `0` | Timeout for each `-agent` command (`0` = none). On expiry the agent fails (`exit=-1`) and the report marks `timedOut=true`. |
| `-agent-retries` | `0` | Retry a FAILED agent (bad exit or `-agent-timeout`) up to N more times, each in a fresh worktree off the same base. A lane-strict out-of-lane failure is never retried — that's a plan violation, not a timing fluke. |
| `-strategy` | `overlay` | Integration strategy: `overlay`, `mergetree`, `naive`, `porcelain` (see [Strategies](#strategies)). |
| `-assert` | `false` | Paranoid cross-check for `-strategy overlay`: independently recompute the combine via `merge-tree` and error out (nothing lands) on any tree mismatch. Roughly doubles integration cost (it re-merges everything); for paranoia/CI, not routine use. |
| `-resolver` | — | Conflict-resolver command; low-confidence cases are flagged, never guessed. |
| `-resolver-timeout` | `30s` | Per-conflict timeout for `-resolver` (`0` = none). |
| `-verify` | — | Command run in a detached checkout of the integrated tree; non-zero exit = merge fails and does not land. |
| `-verify-retries` | `0` | After a FAILING `-verify` invocation, re-run it up to N more times on the same tree; passes on any green. A pass on a retry marks the report `flaky=true`. `0` = today's behavior. |
| `-repair` | — | Fixer command invoked when `-verify` fails; edits are committed and `-verify` re-runs. |
| `-repair-max` | `2` | Max repair attempts before reporting `verify.ok=false` honestly. |
| `-lanes` | `warn`* | Lane enforcement: `off`, `warn`, or `strict` (see [File lanes](#file-lanes)). *`-goal` runs default to `strict` instead unless `-lanes` is set explicitly. |
| `-no-autocommit` | `false` | Do **not** commit edits an agent left uncommitted. By default the driver stages and commits them, so edit-only agents still land. |
| `-keep-failed` | `false` | Keep a FAILED agent's worktree on disk instead of removing it, so it can be inspected. The path is printed and recorded in the report. Successful agents' worktrees are always removed. A kept worktree stays registered with git until you remove it: `git worktree remove <path>` (or `git worktree prune` after deleting the directory yourself). With `-agent-retries`, only the LAST failed attempt's worktree is kept — every earlier attempt is torn down as it fails. |
| `-budget` | `0` | Wall-clock ceiling for the whole run: the agent phase, integrate, and verify combined (`0` = none). On expiry, outstanding agents are cancelled and fail; integrate/verify then run against whatever's left of that same deadline, and if they can't complete, the run reports an operational error naming the budget instead of landing anything partial. |
| `-logdir` | — | Write each agent/repair/verify/planner command's **full** stdout+stderr to `<logdir>/<name>.log` (`agent-<id>.log`, `repair-<n>.log`, `verify-<n>.log`, `planner.log`), on top of the truncated tails the report keeps in memory. The directory is created if needed and must be writable — checked before any agent runs, so a bad `-logdir` fails the whole run rather than silently dropping logs partway through. Repeated runs against the same `-logdir` **append** to the same files; there is no per-run rotation. A task's `id` is sanitized for use in the filename (non-alphanumeric characters become `-`), so two exotic ids that sanitize to the same string share one log file. |
| `-events` | — | Stream one JSON object per line to FILE as the run progresses (see [Events](#events)); `-` writes to stderr. The file is truncated fresh each run. Opening it is checked before any agent runs, same fail-fast policy as `-logdir`; a write failure afterward is best-effort and never fails the run. |
| `-json` | `false` | Emit the full JSON report instead of a terse human summary. |

### Timeouts, retries, and budget

`-agent-timeout`, `-agent-retries`, and `-budget` compose. `-agent-timeout`
bounds a single agent command so one hung agent can't hang the whole run —
on expiry it's reported `exit=-1, timedOut=true`. `-agent-retries` then
decides whether to try that same task again, in a fresh worktree off the
same base; a bad exit or an `-agent-timeout` expiry is retried, but a
lane-strict out-of-lane failure never is (`-lanes strict`), since that's a
plan violation no retry fixes. `-budget` caps the ENTIRE run — the agent
phase, integrate, and verify — with one wall-clock ceiling that sits above
both: once it expires, every outstanding agent is cancelled and fails
(consuming no further retries, since a cancelled run can't usefully retry
anything), and integrate/verify are attempted against whatever's left of
that same expired context. If they can't complete, `sig run` reports an
honest operational error naming the budget instead of ever landing a
partial tree — the same `-verify` gate applies as on any other run.

### Determinism

`-verify` **must be deterministic**: the same tree should produce the same
verdict every time. `-verify-retries` is a mitigation for flaky test suites —
a transient failure re-runs on the exact same commit, never a different one —
not a license to ship a nondeterministic check. Every retried pass is surfaced
honestly: the report's `verify.flaky` is `true` whenever a passing run needed
a retry, so a flaky suite stays visible even though the run goes green.
`verify-bisect` and `verify-cache` (planned) both assume `-verify` is
deterministic; an undocumented flaky command will confuse them.

### Planning

The prompt the planner receives explicitly allows it to return **fewer** than
`-n` tasks when the goal doesn't split cleanly, so a degenerate plan (e.g. one
task for the whole goal) is not itself an error. Two safeguards keep that from
going unnoticed:

- If the plan has fewer tasks than `-n`, a warning naming the actual vs.
  requested count is printed to stderr — the run still proceeds.
- `-min-tasks` sets a hard floor: a plan with fewer tasks than `-min-tasks`
  fails **before any agent runs**, naming got vs. want (fail-safe, same as
  every other plan validation). `-min-tasks` must be `<= -n`, checked at
  flag-parse time.

A planned run (`-goal`) also changes the [`-lanes`](#file-lanes) default to
`strict`; see below.

### Tasks file

`-tasks` points at a JSON array. Each task has an `id`, a `prompt`, and an
optional `files` list — the lane the task is allowed to touch:

```json
[
  {"id": "csv-export", "prompt": "Add CSV export to the report command.", "files": ["report/csv.go"]},
  {"id": "due-dates",  "prompt": "Add due dates to tasks.",              "files": ["model/task.go"]}
]
```

Ids must be unique and non-empty. Tasks with disjoint `files` integrate in
parallel; overlapping ones are serialized. When `files` is omitted, the task's
write-set is computed from what the agent actually changed.

### Exit codes

`sig run` exits with a code that reflects the actual outcome, so CI can gate
on it instead of parsing stdout:

| Code | Meaning |
|------|---------|
| `0` | Landed and verified (or `-verify` was not set). |
| `1` | Operational error (bad flags, a git/integrate failure, etc.). |
| `2` | Usage error (bad top-level `sig` invocation). |
| `3` | `-verify` failed; nothing landed. |
| `4` | One or more branches flagged as conflicts (the rest landed). |
| `5` | No agent succeeded. |

When a run matches more than one of these, the most severe wins, in this
order: `1` > `3` > `5` > `4`.

---

## `sig integrate`

The integration engine on its own: given a set of existing branches, fold them
into one tree. This is the same code path `sig run` uses after the agents
finish — useful for integrating branches you produced some other way, or for
benchmarking.

```
sig integrate -repo PATH -base BRANCH -branches b1,b2,.. [-strategy overlay] [-assert] [-resolver CMD] [-no-land]
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-repo` | *(required)* | Path to the target git repository. |
| `-base` | `main` | Branch to land the integrated result onto. |
| `-branches` | *(required)* | Comma-separated branch names to integrate. |
| `-strategy` | `overlay` | Integration strategy (see below). |
| `-assert` | `false` | Paranoid cross-check for `-strategy overlay`: independently recompute the combine via `merge-tree` and error out (nothing lands) on any tree mismatch. Roughly doubles integration cost (it re-merges everything); for paranoia/CI, not routine use. |
| `-resolver` | — | Per-conflict resolver command. |
| `-resolver-timeout` | `30s` | Per-conflict timeout for `-resolver` (`0` = none). |
| `-no-land` | `false` | Integrate without moving the base ref; leave the result as a detached commit. |

---

## `sig doctor`

Checks that `git` is new enough and that the plumbing sigbound depends on —
`git merge-tree --write-tree -z --name-only --merge-base=` and the overlay
index plumbing (`read-tree`/`update-index`/`write-tree`) — actually works,
instead of trusting the version string. Both require git >= 2.38; without
this check, an older git fails deep inside a run with a bare "merge-tree exit
N" instead of a clear message.

```
sig doctor [-repo PATH]
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-repo` | — | Run the live probe inside this existing repo instead of a throwaway temp repo. The probe only builds objects via plumbing (no worktree, branch, or ref), so it never mutates the repo. |

`sig doctor` prints one line per check (`ok` or `FAIL: <reason>`) and exits
`0` if every check passes, `1` if any fails:

```
$ sig doctor
git on PATH: ok
git version >= 2.38: ok
live probe: merge-tree + overlay plumbing: ok
```

`sig run` and `sig integrate` also run the cheap part of this (git present +
version check) automatically before doing anything, so a too-old git is
caught before any agent runs; they do **not** run the live probe (it's
overkill to pay for on every invocation) — run `sig doctor` directly for the
full picture.

---

## Strategies

All strategies produce the same final tree for a conflict-free batch and flag
the same real conflicts — they differ only in how they get there, so they
benchmark cleanly against each other.

| Strategy | How it merges |
|----------|---------------|
| `overlay` *(default)* | OCC with a tree-overlay fast path: disjoint groups are unioned in the object store with **no merge at all**; only genuinely overlapping groups run `merge-tree`. Fastest. |
| `mergetree` | OCC on `git merge-tree` everywhere: partition into disjoint groups, fold each in parallel, combine group heads with `merge-tree`. |
| `naive` | Serial fold with in-object-store `merge-tree`, no worktree. |
| `porcelain` | The working-tree baseline: `git merge` on an integration worktree. Correct, but pays the working-tree, index-lock, and per-merge process cost this project exists to eliminate. |

`overlay`'s combine has no runtime cross-check by default (unlike `mergetree`'s, which self-guards); pass `-assert` to have it independently recompute the combine via `merge-tree` and error out on any tree mismatch.

---

## File lanes

Each task can declare the files it is allowed to touch (`files` in the tasks
file, or a plan produced by the planner). `-lanes` controls enforcement:

- `off` — ignore declared file-sets.
- `warn` *(default for `-tasks` runs)* — record out-of-lane writes but still
  land them.
- `strict` *(default for planned `-goal` runs, unless `-lanes` is set
  explicitly)* — an out-of-lane write makes the agent a failed agent; its work is
  not landed. This is the real disjointness guarantee.

---

## Environment variables

Every model slot (`-planner`, `-agent`, `-resolver`, `-repair`) is a shell
command **you** supply, run via `sh -c`. Sigbound passes context through
`SIGBOUND_*` environment variables:

| Variable | Given to | Meaning |
|----------|----------|---------|
| `SIGBOUND_GOAL` | `-planner` | The natural-language goal. |
| `SIGBOUND_REPOMAP` | `-planner` | A short map of the repository. |
| `SIGBOUND_N` | `-planner` | The requested number of tasks. |
| `SIGBOUND_PROMPT` | `-planner` | A ready-to-use prompt combining the above; write a JSON task array to stdout. |
| `SIGBOUND_TASK` | `-agent` | The task prompt. cwd is the task's worktree. |
| `SIGBOUND_TASK_ID` | `-agent` | The task id. |
| `SIGBOUND_REPO` | `-agent`, `-repair` | Path to the repository. |
| `SIGBOUND_BRANCH` | `-agent` | The task's branch name. |
| `SIGBOUND_BASE` | `-resolver` | Path to the base (common-ancestor) version of a conflicted file. |
| `SIGBOUND_OURS` | `-resolver` | Path to the "ours" version. |
| `SIGBOUND_THEIRS` | `-resolver` | Path to the "theirs" version. |
| `SIGBOUND_PATH` | `-resolver` | Repo-relative path of the conflicted file. Write the resolved body to stdout; empty output, a non-zero exit, or a timeout flags the conflict for a human. |
| `SIGBOUND_FAILURE` | `-repair` | The captured `-verify` output to fix. Edit files to fix it; the driver commits the edits and re-runs `-verify`. |

---

## JSON report

With `-json`, `sig run` prints a full report. Top-level shape:

```jsonc
{
  "repo": "…", "base": "main", "baseSHA": "…",
  "laneMode": "warn",
  "tasks":    [ { "id": "…", "prompt": "…", "files": ["…"] } ],
  "perAgent": [ {
    "id": "…", "branch": "…", "sha": "…", "files": ["…"],
    "ok": true, "exit": 0, "autocommitted": false,
    "declaredFiles": ["…"], "actualFiles": ["…"],
    "inLane": true, "strayed": [], "stderr": "",
    "worktreeKept": "", "timedOut": false, "attempts": 1
  } ],
  "integrate": {
    "strategy": "overlay", "groups": 3,
    "landed": ["…"], "flagged": [], "resolved": 0,
    "finalSHA": "…", "wallMs": 12
  },
  "verify": {
    "ran": true, "ok": true, "attempts": 1, "repaired": false, "flaky": false,
    "output": "…",
    "repairs": [ { "n": 1, "filesTouched": ["…"], "verifyOk": true } ]
  },
  "logDir": "…"
}
```

- `integrate.landed` / `integrate.flagged` — branches that merged vs. branches
  set aside for a human (real conflicts).
- `integrate.resolved` — overlapping branches that still landed (auto-merged or
  resolver-resolved).
- `verify.ok` is the bottom line: `false` means nothing was landed onto `-base`.
- `logDir` is present iff `-logdir` was set; it names the directory holding
  each command's full stdout+stderr log (see `-logdir` above).

Without `-json`, the same run prints a short human summary.

---

## Events

With `-events FILE` (`-` for stderr), `sig run` streams one JSON object per
line as the run progresses — `{"event":"...","ts":"<RFC3339Nano>",...fields}`.
This is a **progress feed, not a second report**: it lets you watch a long or
highly parallel run live (which agent is the long pole, when integrate/verify
start and finish), but the [JSON report](#json-report) printed at the end
remains the source of truth for the actual outcome. Lines are written through
a single mutex-guarded writer, so concurrent agents never interleave a
partial line; a write failure is best-effort and never fails the run, but a
FILE that can't be opened at all fails the run before any agent runs, same as
`-logdir`.

| Event | Fields | When |
|-------|--------|------|
| `run_start` | `repo`, `base`, `baseSHA`, `tasks` | Once, right after the base ref resolves. |
| `agent_start` | `id`, `branch` | Once per task, right before that agent's worktree/command starts. |
| `agent_done` | `id`, `ok`, `exit`, `attempts`, `wallMs` | Once per task, after all of that task's attempts (including `-agent-retries`) finish. |
| `integrate_start` | `branches` | Once, before the successfully-committed branches are folded together. |
| `integrate_done` | `landed`, `flagged`, `resolved`, `finalSHA`, `wallMs` | Once, after integration (before landing). |
| `verify_start` | `attempt` | Before each `-verify` invocation. `attempt` is `0` pre-repair, `N` after repair round `N` (matches `-logdir`'s `verify-<n>.log`). |
| `verify_done` | `ok`, `flaky`, `attempt`, `wallMs` | After each `-verify` invocation (including `-verify-retries`). |
| `repair_start` | `attempt` | Before each `-repair` fixer invocation. |
| `repair_done` | `attempt`, `verifyOk`, `wallMs` | After that round's fixer AND its follow-up `-verify` both finish; `wallMs` covers the fixer only. |
| `land` | `sha` | Once, right after the base ref advances (never emitted when nothing lands). |
| `run_done` | `ok`, `exitCode`, `wallMs` | Once, always last — even on a mid-run operational error. |

`-events` off (the default, empty `-events`) emits nothing at all.

---

## Benchmark

`sigbench` measures parallel integration against a sequential `git merge`
baseline, verifying correctness on every run:

```bash
go run ./cmd/sigbench -sweep -runs 5 -warmup 1
```

`-sweep` runs the full agent-count sweep; `-runs` sets repetitions per point and
`-warmup` the discarded warmup runs. See the
[Benchmarks](../README.md#benchmarks) section of the README for results.
