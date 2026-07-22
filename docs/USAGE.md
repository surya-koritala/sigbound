# Usage

Complete reference for the `sig` CLI. For a five-minute walkthrough on your own
repository, see [`examples/`](../examples/). For the shape of the whole
pipeline, see the [README](../README.md#how-it-works).

`sig` has five subcommands:

```
sig run        run agents on a repo and integrate their work (the driver)
sig integrate  integrate a set of existing branches (the engine, standalone)
sig replay     deterministically re-integrate a prior run's recorded inputs
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
sig run [-config PATH]
        -repo PATH -base BRANCH
        (-tasks FILE | -goal STRING (-planner CMD | -planner-preset NAME) [-n N] [-min-tasks N] | -resume -manifest FILE)
        (-agent CMD | -agent-preset NAME) [-agent-timeout D] [-agent-retries N]
        [-strategy overlay] [-assert]
        [-resolver CMD] [-resolver-timeout D]
        [(-verify CMD | -verify-preset NAME) [-verify-retries N] [-verify-impact CMD]
          [-verify-cache]
          [(-repair CMD | -repair-preset NAME) [-repair-max N]]]
        [-lanes off|warn|strict]
        [-no-autocommit]
        [-keep-failed]
        [-budget D]
        [-logdir DIR]
        [-events FILE]
        [-manifest FILE]
        [-notes]
        [-dry-run]
        [-json]
```

### Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `-config` | — | Path to a flags file supplying defaults for the other flags below (see [Config file](#config-file)). `""` (unset): look for `./sig.conf` in the current directory. `none`: disable that discovery. Anything else: read that exact file. |
| `-repo` | *(required)* | Path to the target git repository. |
| `-base` | `main` | Branch the agents fork from and the result lands onto. |
| `-tasks` | — | JSON file describing the tasks (see below). Mutually exclusive with `-goal`. |
| `-goal` | — | Natural-language goal; the `-planner` turns it into disjoint tasks. Mutually exclusive with `-tasks`. |
| `-planner` | — | Planner command (run via `sh -c`). Required with `-goal`, unless `-planner-preset` supplies one. |
| `-planner-preset` | — | Expand a named planner-harness preset (`claude`, `codex`, `aider`) into `-planner`'s command (see [Presets](#presets)). An explicit `-planner` always overrides its preset. |
| `-n` | `4` | Number of parallel tasks the planner should produce from `-goal`. |
| `-min-tasks` | `0` | Minimum tasks a `-goal` plan must produce; fewer fails **before any agent runs** (`0` = no floor). Must be `<= -n`. |
| `-planner-timeout` | `120s` | Timeout for the planner command (`0` = none). |
| `-agent` | *(required, unless `-agent-preset` supplies one)* | Command (run once per task, via `sh -c`) that edits files in the task's worktree. |
| `-agent-preset` | — | Expand a named agent-harness preset (`claude`, `codex`, `aider`) into `-agent`'s command (see [Presets](#presets)). An explicit `-agent` always overrides its preset. |
| `-agent-timeout` | `0` | Timeout for each `-agent` command (`0` = none). On expiry the agent fails (`exit=-1`) and the report marks `timedOut=true`. |
| `-agent-retries` | `0` | Retry a FAILED agent (bad exit or `-agent-timeout`) up to N more times, each in a fresh worktree off the same base. A lane-strict out-of-lane failure is never retried — that's a plan violation, not a timing fluke. |
| `-strategy` | `overlay` | Integration strategy: `overlay`, `mergetree`, `naive`, `porcelain` (see [Strategies](#strategies)). |
| `-assert` | `false` | Paranoid cross-check for `-strategy overlay`: independently recompute the combine via `merge-tree` and error out (nothing lands) on any tree mismatch. Roughly doubles integration cost (it re-merges everything); for paranoia/CI, not routine use. |
| `-resolver` | — | Conflict-resolver command; low-confidence cases are flagged, never guessed. |
| `-resolver-timeout` | `30s` | Per-conflict timeout for `-resolver` (`0` = none). |
| `-verify` | — | Command run in a detached checkout of the integrated tree; non-zero exit = merge fails and does not land. |
| `-verify-preset` | — | Expand a named per-language build+test preset (`go`, `node`, `python`, `rust`) into `-verify`'s command (see [Presets](#presets)). An explicit `-verify` always overrides its preset. |
| `-verify-retries` | `0` | After a FAILING `-verify` invocation, re-run it up to N more times on the same tree; passes on any green. A pass on a retry marks the report `flaky=true`. `0` = today's behavior. |
| `-verify-impact` | — | Command run INSTEAD of `-verify` when Sigbound can confidently scope it to the impacted Go packages (see [Scoped verification](#scoped-verification)). Requires `-verify` (or `-verify-preset`), which stays the fallback on any doubt. |
| `-verify-cache` | `false` | Cache a PASSING verify verdict and skip re-running the command on an exact repeat (see [Cache](#cache)). A FAILING verdict is never cached. Off by default. |
| `-repair` | — | Fixer command invoked when `-verify` fails; edits are committed and `-verify` re-runs. |
| `-repair-preset` | — | Expand a named repair-harness preset (`claude`, `codex`, `aider`) into `-repair`'s command (see [Presets](#presets)). An explicit `-repair` always overrides its preset. |
| `-repair-max` | `2` | Max repair attempts before reporting `verify.ok=false` honestly. |
| `-lanes` | `warn`* | Lane enforcement: `off`, `warn`, or `strict` (see [File lanes](#file-lanes)). *`-goal` runs default to `strict` instead unless `-lanes` is set explicitly. |
| `-no-autocommit` | `false` | Do **not** commit edits an agent left uncommitted. By default the driver stages and commits them, so edit-only agents still land. |
| `-keep-failed` | `false` | Keep a FAILED agent's worktree on disk instead of removing it, so it can be inspected. The path is printed and recorded in the report. Successful agents' worktrees are always removed. A kept worktree stays registered with git until you remove it: `git worktree remove <path>` (or `git worktree prune` after deleting the directory yourself). With `-agent-retries`, only the LAST failed attempt's worktree is kept — every earlier attempt is torn down as it fails. |
| `-budget` | `0` | Wall-clock ceiling for the whole run: the agent phase, integrate, and verify combined (`0` = none). On expiry, outstanding agents are cancelled and fail; integrate/verify then run against whatever's left of that same deadline, and if they can't complete, the run reports an operational error naming the budget instead of landing anything partial. |
| `-logdir` | — | Write each agent/repair/verify/planner command's **full** stdout+stderr to `<logdir>/<name>.log` (`agent-<id>.log`, `repair-<n>.log`, `verify-<n>.log`, `planner.log`), on top of the truncated tails the report keeps in memory. The directory is created if needed and must be writable — checked before any agent runs, so a bad `-logdir` fails the whole run rather than silently dropping logs partway through. Repeated runs against the same `-logdir` **append** to the same files; there is no per-run rotation. A task's `id` is sanitized for use in the filename (non-alphanumeric characters become `-`), so two exotic ids that sanitize to the same string share one log file. |
| `-events` | — | Stream one JSON object per line to FILE as the run progresses (see [Events](#events)); `-` writes to stderr. The file is truncated fresh each run. Opening it is checked before any agent runs, same fail-fast policy as `-logdir`; a write failure afterward is best-effort and never fails the run. |
| `-manifest` | — | Write the full JSON report to FILE at the end of the run, independent of `-json` (see [Provenance](#provenance)). FILE's directory is created if needed and checked writable before any agent runs, same fail-fast policy as `-logdir`; a write failure AFTER the run completes is best-effort and warned on stderr — by then real work may already be landed. With `-resume`, this SAME flag also names the prior run's manifest to resume FROM (see [Resume](#resume)). |
| `-resume` | `false` | Resume a prior run instead of planning/loading tasks fresh (see [Resume](#resume)). Requires `-manifest`; `-tasks`/`-goal` must not be passed. |
| `-notes` | `false` | When the run LANDS, attach the full JSON report as a git note on the landed commit under the namespaced `refs/notes/sigbound` (see [Provenance](#provenance)). Best-effort: a failure is warned on stderr but never fails the run, since landing already happened. |
| `-dry-run` | `false` | Load or plan the tasks, print them plus the predicted OCC partition, then exit — no worktree, agent, verify, or repair ever runs (see [Dry run](#dry-run)). |
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

### Scoped verification

`-verify` runs the whole suite once, on the whole tree, every time — for a
large repo that's usually the dominant cost of a run. `-verify-impact CMD`
lets you point Sigbound at a command that runs ONLY the tests for the Go
packages actually affected by this run's changes, using the write-set the
integrator already computed (no extra diffing):

```bash
sig run ... -verify 'go test ./...' -verify-impact 'go test $SIGBOUND_IMPACTED_PKGS'
```

When it decides scoping is safe, Sigbound runs `go list -json ./...` in the
same detached checkout `-verify` would use, builds the reverse import graph
(including test-only imports — a change to package A can break the tests of
anything whose _test.go files import A, even if A's own sources don't reach
it), and expands the changed packages to every transitive reverse dependent.
The result is exported to `-verify-impact` as two environment variables:

| Variable | Given to | Meaning |
|----------|----------|---------|
| `SIGBOUND_IMPACTED_PKGS` | `-verify-impact` | Space-separated `./relative` package paths: the changed packages plus every package that (transitively) imports one of them. |
| `SIGBOUND_CHANGED_FILES` | `-verify-impact` | Space-separated repo-relative paths of every file this run's landed write-set touched. |

**`-verify-impact` only ever runs INSTEAD of `-verify` on high confidence;
`-verify` is required and remains the fallback — and the source of truth —
on ANY doubt.** Impact scoping trades confidence for speed: it can only ever
narrow which tests run, never which changes get checked. The full `-verify`
command runs instead whenever:

- a changed path is anything other than a `.go` file inside the module
  (a `go.mod`/`go.sum` change, a doc, a config file, ...);
- a changed path sits under a directory named `testdata/` (the Go tool never
  treats those as package source, so impact analysis can't reason about them
  safely);
- `go list` fails to run or its output can't be parsed;
- the resulting impact set is empty, or a changed `.go` file doesn't map to
  any package `go list` reported.

The report's `verify.scope` names which command actually ran (`"impact"` or
`"full"`) and `verify.impactedPkgs` lists the packages, when scoped — both
fields are absent entirely when `-verify-impact` isn't set, so a run without
it is unaffected. `-verify-impact` failing is an ordinary verify failure,
gated and repaired exactly like `-verify` failing — `-repair`'s re-verify
runs through the same seam, RECOMPUTING impact after each repair attempt (a
fixer's own edits are new changes on top of the original write-set, so the
scope decision is re-made from scratch every time, never memoized).

### Cache

Integration is deterministic and `-verify` is usually the dominant per-run
cost, yet by default every run re-verifies from scratch even when it lands a
tree that was already proven to pass — e.g. a `-resume`/replay that
reproduces an earlier result exactly. `-verify-cache` lets a run skip that
redundant work:

```bash
sig run ... -verify 'go test ./...' -verify-cache
```

**Key composition.** An entry is keyed on three things together:

1. The **tree OID** of the verified commit (`HEAD^{tree}` in the verify
   checkout) — NOT the commit SHA. Git trees are content-addressed, so two
   different commits (a fresh integration and a later resume/replay) that
   land byte-identical content share one entry; two commits with different
   content never collide.
2. A hash of the **exact command that would run** — the resolved `-verify`
   or (when scoped) `-verify-impact` command plus its impacted-package list.
   A full run and a scoped run over the same tree, or two different scoped
   runs with different impacted packages, are different keys, never the
   same entry.
3. The running **sigbound version**, so a rebuilt or upgraded binary never
   trusts a verdict computed under different semantics.

**Only a PASS is ever cached.** A failing `-verify` invocation is never
written to the cache: a flaky environment must never pin a red as
permanently cached, and any doubt about whether a cached NO-verdict is still
accurate costs nothing to re-check for real. A cache hit therefore always
means "this exact tree, with this exact command, already passed" — never a
weaker guarantee — and the report marks it `verify.cached: true`
(`verify.ok` stays `true`, and the command is not spawned at all). The
human summary notes it too: `verify: PASS  cached  ...`.

**Storage.** Entries live at `.git/sigbound/verify-cache/<key>` under the
TARGET repo's own git directory (resolved via `git rev-parse
--git-common-dir`, so it's correct even when `-repo` is itself a linked
worktree) — never inside the working tree, so it never shows up in `git
status` and survives worktrees/clones being reused. One small JSON file per
entry, no eviction (entries are a few hundred bytes each). Reset the whole
cache with:

```bash
rm -rf .git/sigbound
```

**The trade-off.** `-verify` is the gate that decides what lands — its
rawness (a real command running against a real checked-out tree, every
time) is the whole trust anchor. `-verify-cache` is therefore off by
default: turning it on is a deliberate choice to trust "this exact tree
already passed this exact command" instead of re-proving it, in exchange for
skipping genuinely redundant work.

### Determinism

`-verify` **must be deterministic**: the same tree should produce the same
verdict every time. `-verify-retries` is a mitigation for flaky test suites —
a transient failure re-runs on the exact same commit, never a different one —
not a license to ship a nondeterministic check. Every retried pass is surfaced
honestly: the report's `verify.flaky` is `true` whenever a passing run needed
a retry, so a flaky suite stays visible even though the run goes green.
`verify-bisect` (planned) and `-verify-cache` (see [Cache](#cache)) both
assume `-verify` is
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

#### Dry run

`-dry-run` loads the tasks (`-tasks`) or runs the planner (`-goal` — the
planner still runs; that's the point, seeing the split costs nothing further
once you've paid for it) and prints them plus the **predicted** partition,
then exits before creating any worktree or running any agent, `-verify`, or
`-repair` command. The prediction reuses the same `cell.Partition` grouping
`sig integrate` runs for real, fed each task's *declared* `files` in place of
a real `git diff` — a task with no declared `files` is unknown to the
partitioner and lands alone in its own group. `-agent` is still required (no
validation changes) but is never invoked. The existing plan-validation
failures (a bad plan, an unmet `-min-tasks` floor) fail exactly as they would
without `-dry-run` — that failure IS the preview's value. With `-json`, the
report is `{"tasks":[...], "groups":[{"tasks":["…"],"files":["…"]}], "parallelism":N, "laneMode":"…"}`,
where `parallelism` is the number of groups (independent branches that could
land in parallel; tasks sharing a group would be serialized by the
integrator).

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

### Presets

Hand-writing the `sh -c` wiring for `-agent`/`-repair`/`-planner` (the
`SIGBOUND_TASK`/`SIGBOUND_FAILURE`/`SIGBOUND_PROMPT` env plumbing) and the
idiomatic build+test command for `-verify` is the fiddliest part of a first
`sig run` — especially off Go, where there's no worked example to copy.
`-agent-preset`, `-repair-preset`, and `-planner-preset` (`claude` | `codex` |
`aider`) and `-verify-preset` (`go` | `node` | `python` | `rust`) expand a
short name into that exact command, so you start from a known-good invocation
instead of typing it by hand.

A preset encodes only the harness's CLI shape (how to invoke it
non-interactively) or the ecosystem's build+test command — never the model —
so bring-your-own-model is unaffected: `claude`/`codex`/`aider` here are just
those CLIs invoked in their standard non-interactive mode, using whatever
model each is configured for elsewhere.

**Precedence.** An explicit `-agent`/`-repair`/`-planner`/`-verify` always
overrides its preset — raw wins, unconditionally, even when a preset name is
also set (an overridden preset name is never looked up, so a bogus one there
is not an error). Every expansion is printed once to stderr, so you can see
and copy exactly what will run.

**From `sig.conf` too.** Preset flags are ordinary flags: they're read from a
`-config` file exactly like `-agent` or `-verify` (see [Config
file](#config-file) below) — nothing about them is special-cased.

There is no `-resolver-preset`: the `-resolver` wiring in the [Config
file](#config-file) example below (`git merge-file -p --union ...`) is a
plain git command with no harness involved, and a real conflict resolver
worth presetting is repo-specific — out of scope here.

Exact expansions:

| Flag | Name | Expands to |
|------|------|------------|
| `-agent-preset` | `claude` | `claude -p --permission-mode acceptEdits "$SIGBOUND_TASK"` |
| `-agent-preset` | `codex` | `codex exec --full-auto "$SIGBOUND_TASK"` |
| `-agent-preset` | `aider` | `aider --yes --message "$SIGBOUND_TASK"` |
| `-repair-preset` | `claude` | `claude -p --permission-mode acceptEdits "Fix this build failure: $SIGBOUND_FAILURE"` |
| `-repair-preset` | `codex` | `codex exec --full-auto "Fix this build failure: $SIGBOUND_FAILURE"` |
| `-repair-preset` | `aider` | `aider --yes --message "Fix this build failure: $SIGBOUND_FAILURE"` |
| `-planner-preset` | `claude` | `claude -p "$SIGBOUND_PROMPT"` |
| `-planner-preset` | `codex` | `codex exec "$SIGBOUND_PROMPT"` |
| `-planner-preset` | `aider` | `aider --yes --message "$SIGBOUND_PROMPT"` |
| `-verify-preset` | `go` | `go build ./... && go test ./...` |
| `-verify-preset` | `node` | `npm test` |
| `-verify-preset` | `python` | `python -m pytest` |
| `-verify-preset` | `rust` | `cargo build && cargo test` |

For example, this long-form invocation (see [Usage](../README.md#usage) in
the README):

```bash
./sig run -repo /path/to/your/repo -tasks examples/tasks.json \
  -agent  'claude -p --permission-mode acceptEdits "$SIGBOUND_TASK"' \
  -verify 'go build ./... && go test ./...'
```

is equivalent to:

```bash
./sig run -repo /path/to/your/repo -tasks examples/tasks.json -agent-preset claude -verify-preset go
```

### Config file

A real project's `sig run` invocation is long and stable from one run to the
next — the same `-agent`/`-verify`/`-repair`/`-lanes` every time. `-config`
lets you park those standing flags in a file instead of retyping them.

**This is a flags file, not TOML**, despite `sig.toml` being issue #13's
working title: sigbound is stdlib-only (no third-party dependencies, ever),
and the standard library doesn't include a TOML parser. `sig.conf` is instead
the simplest thing that actually works — one flag per line:

```
# sig.conf — standing flags for this project. Comments start with '#'.
# key = the flag's name, without its leading dash.
# value = exactly what you'd type after that flag on the command line.

repo     = .
strategy = overlay
agent    = claude -p --permission-mode acceptEdits "$SIGBOUND_TASK"
resolver = git merge-file -p --union "$SIGBOUND_OURS" "$SIGBOUND_BASE" "$SIGBOUND_THEIRS"
verify   = go build ./... && go test ./...
repair   = claude -p --permission-mode acceptEdits "Fix this build failure: $SIGBOUND_FAILURE"
lanes    = strict
json     = true
```

With that file next to where you run `sig` from:

```bash
sig run -config sig.conf -tasks examples/tasks.json
```

`-tasks` here is still required on the command line (or could equally live in
`sig.conf` itself) — `-config` supplies *defaults*, it doesn't replace normal
flag validation. Every other run flag is allowed in the file **except**
`-config` itself (self-referential, so it's a hard error, not silently
ignored).

**Discovery.** Leave `-config` unset and `sig run` looks for `./sig.conf` in
the directory you ran it from — no home-directory fallback, no walking up
toward a repo root. Nothing there is not an error; the run just proceeds on
flags and defaults alone. Pass `-config /path/to/file` to use a specific file
by name (it must exist and be readable), or `-config none` to turn discovery
off even when a `sig.conf` is sitting right there.

**Precedence.** Command-line flag > config file > flag's built-in default. A
flag you pass on the command line always wins over the same key in the config
file; a key the config file sets always wins over the flag's plain default.
Concretely: `-config sig.conf -strategy overlay` uses `overlay` even if
`sig.conf` says `strategy = mergetree`, but drop `-strategy` from the command
line and the config file's `mergetree` takes over.

One consequence of that precedence is worth calling out: [`-lanes`'s
strict-by-default rule for planned (`-goal`) runs](#file-lanes) treats a
config-file `lanes = ...` the same as a command-line `-lanes` — both count as
"the caller chose this explicitly," so either one overrides the strict
default, not just a command-line flag.

**Raw vs. preset resolves first.** The precedence above is per-flag (`-agent`
vs. `agent = ...`); [raw-vs-preset resolution](#presets) — a raw command
always wins over its preset name — happens afterward, on whichever value each
flag ended up with, so a `sig.conf` `agent = ...` raw command still overrides
an `-agent-preset` passed on the command line.

**Format.** Blank lines and lines starting with `#` (after leading
whitespace) are ignored. Otherwise a line must be `key=value`; only the
*first* `=` on the line is the delimiter, so a value may itself contain `=`
(a shell command like `verify = FOO=bar go test ./...` works as expected).
Whitespace around the key and around the value is trimmed; whitespace inside
the value is not. A line with no `=` at all, or an empty key, is a hard
parse error naming the file and the 1-based line number, same as an unknown
key (`fs.Set` rejects it) or a value the flag's own type can't parse (e.g. a
non-boolean for `-assert`). `sig run` fails before touching git or spawning
any agent in every one of these cases — a bad config file behaves like any
other bad flag.

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

### Provenance

`sig run` prints a report and advances a branch, but by default nothing else
survives the process exiting — the exact inputs (which commit SHAs landed,
what `-agent`/`-verify`/`-resolver` actually ran) live only in that one
stdout capture. `-manifest` and `-notes` persist that same report as durable
provenance, in two complementary places:

- **`-manifest FILE`** writes the full JSON report (identical shape to
  `-json`) to `FILE` at the end of the run — independent of `-json`, which
  still only controls what's printed to *stdout*. `FILE`'s directory is
  created if needed and checked writable **before any agent runs**, the same
  fail-fast policy as `-logdir`: a manifest path that can never be written is
  caught up front, not discovered after paying for the whole run. A write
  failure *after* the run completes is different — by then real work may
  already be landed on `-base`, so losing the manifest must never look like
  losing the run itself. That failure is best-effort: a loud warning on
  stderr, no change to the exit code.
- **`-notes`** attaches that same report as a git note on the landed commit,
  under the **namespaced** ref `refs/notes/sigbound` — never git's default
  `refs/notes/commits`, so sigbound's provenance never collides with a
  repo's own note usage. It only ever fires once the base ref has actually
  advanced (a run that lands nothing attaches nothing); like a late
  `-manifest` write, a note failure is best-effort and warned on stderr,
  never a run failure — the landing already happened by the time it runs.

The report's new top-level fields carry that provenance: `strategy` (the
integration strategy, duplicated here even though `integrate.strategy`
already has it, so it's readable without integrate having run at all),
`agentCmd`/`resolverCmd`/`verifyCmd`/`repairCmd`/`plannerCmd` (the exact,
RESOLVED command strings this run executed — after `-*-preset` expansion and
`-config` merging), `version` (the sigbound version that produced this
report), and `startedAt` (an RFC3339 timestamp for when the run began).

**These commands are redacted NOTHING.** They're your own shell commands,
recorded verbatim — if you baked a secret (an API key, a token) into
`-verify` or `-resolver`, that secret is now sitting in the manifest file and
in the git note on the landed commit. Treat both accordingly: don't commit a
`-manifest` file to a public repo, and remember `-notes` writes into the
target repo's own object store.

**Reading a note back:**

```bash
git notes --ref=sigbound show <sha>
```

**Notes don't push by default.** `git push` never sends `refs/notes/*`
unless you ask it to; push the namespace explicitly when you want the
provenance to travel with the branch:

```bash
git push origin refs/notes/sigbound
```

See [`sig replay`](#sig-replay) below for what a `-manifest` file (or a
`-notes` note, since the shape is identical) is actually *for*: feeding it
back in to deterministically reproduce the integration it recorded.

### Resume

`runAgent` deliberately never cleans up an `agent/<id>` branch — it tears
down the worktree but the branch (and whatever the agent committed to it)
survives the run, win or lose. `-resume` is the glue that turns that
leftover work into a cheap restart: given a prior run's `-manifest`, it skips
re-running the (expensive) agent for every task whose branch already holds
real work, and only runs fresh what's actually missing.

```
sig run -repo PATH -resume -manifest FILE [-agent CMD] [-verify CMD] ...
```

**What gets reused.** For each task in the manifest:

- Its `agent/<id>` branch exists and its head **differs** from the
  manifest's recorded `baseSHA` → reused outright. The agent never runs
  again; the report marks that task `resumed: true` with the SAME `sha` the
  original run recorded.
- Its branch is **missing**, or exists but its head **equals** `baseSHA`
  (the agent ran last time but committed nothing) → runs fresh, exactly like
  an ordinary (non-`-resume`) task. A stale no-op branch is deleted first —
  its content is byte-identical to base, so nothing is lost — clearing the
  way for the fresh run's normal worktree setup.

Integration and verification then proceed over the union of reused and
freshly-run branches exactly as in an ordinary run: nothing about landing,
`-verify`, or `-repair` changes once the agent phase decides what ran.

**The moved-base refusal.** `-resume` fails loudly, before anything runs, if
`-base`'s CURRENT head is no longer exactly the manifest's recorded
`baseSHA`:

```
-resume: base "main" is now at <sha> but the manifest recorded <sha> — it has
moved since that run; re-run fresh instead of resuming onto a different base
```

This is not a technicality: every reused branch forked from `baseSHA`, so
integrating it against a DIFFERENT current base would silently combine work
against a tree it was never actually written for. A run that already landed
something has, by definition, moved `-base` past its own manifest's
`baseSHA` — resuming from that same manifest again is exactly this case.
`-resume` is for restarting a run that DIDN'T land (an interrupted run, a
`-verify` failure, a mid-run operational error) and left agent branches
behind with the base untouched — not for continuing after a partial landing.
This makes `-resume` safe, not magical: it never guesses which base you
meant, it insists on the exact one the prior run recorded.

**The manifest requirement.** `-resume` never re-plans, so `-tasks`/`-goal`
must not be passed alongside it (a loud error, not a silent ignore) — the
task list always comes from `-manifest`, which is REQUIRED with `-resume`.
The same `-manifest` flag doubles as both input (the prior run's file) and
output (this run's own manifest is written back to that same path at the
end), so a chain of resumes just keeps reading and overwriting one file.

**Flag-over-manifest precedence.** `-base`, `-strategy`, `-agent`,
`-resolver`, `-verify`, and `-repair` are all read from the manifest, but
any of them set EXPLICITLY on this command line (directly or via its
`-*-preset`) overrides the manifest's recorded value — the same
flag-beats-file precedence `-config` gives a command-line flag over a config
file. Leave them unset to reuse the prior run's commands verbatim.

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

## `sig replay`

Deterministic replay: given a `-manifest` file (or any `-json` report — same
shape), re-run **only** the integration + verify portion of that recorded
run and check whether the repo still reproduces it.

```
sig replay -manifest FILE
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-manifest` | *(required)* | Path to a JSON report written by `sig run`'s `-manifest` flag (or `-json`). |

**Why this works at all.** Integration is already deterministic —
`cell.Partition` is order-stable and `combineDisjoint` is a fixed reduction —
so capturing a run's inputs and re-feeding them is the *only* missing piece
for reproducible debugging. `sig replay`:

1. Resolves the recorded `baseSHA` and, for every agent the manifest marked
   `ok: true`, the recorded per-agent `sha` — **the exact commit, never the
   branch's current tip**, since a branch can move or be deleted after the
   run that produced the manifest. If any of these no longer resolve in the
   repo (a branch was deleted and the commit was eventually garbage
   collected, say), replay fails clearly instead of guessing.
2. Re-integrates those exact SHAs with the recorded `strategy` and
   `resolverCmd`, via the same `integrateBranches` path `sig run`/`sig
   integrate` use — with `land=false` (like `sig integrate -no-land`).
   Replay is **read-only**: it never moves any ref.
3. If the manifest recorded a `verifyCmd`, re-runs it against the recomputed
   tree and prints whether it still passes — informational only; see below.
4. Compares the recomputed tree's OID against the recorded
   `integrate.finalSHA`'s tree OID (not the raw commit SHA — commit
   timestamps differ between the original run and replay even when the tree
   is byte-identical, so tree equality is the actual claim being checked).

```bash
sig run   -repo . -base main -tasks tasks.json -agent '...' -manifest run.json
sig replay -manifest run.json
```

```
REPRODUCED tree=3f9a2c…
```

or, when the repo no longer produces the recorded result:

```
DIVERGED recorded=3f9a2c… recomputed=a01de4…
```

**Exit codes** are their own scale, distinct from `sig run`'s:

| Code | Meaning |
|------|---------|
| `0` | `REPRODUCED` — the recomputed tree is byte-identical to the recorded one. |
| `1` | `DIVERGED` — both recomputed cleanly, but the trees differ; both OIDs are printed. |
| `2` | Replay itself could not run: a bad/unreadable `-manifest`, a recorded SHA no longer resolvable, or an integrate/checkout failure. |

**What replay does NOT do.** It never re-runs `-agent` or `-repair` — those
are non-deterministic by nature (an LLM, a fixer), so "replaying" them
wouldn't prove anything about the DETERMINISTIC part of the pipeline. If the
original run's `integrate.finalSHA` reflects a `-repair` round, replay's
pure re-integration (no repair) will legitimately `DIVERGE` from it — that's
an honest signal that the recorded result depended on a repair step, not a
replay bug. The recorded `-verify` command IS re-run (step 3 above), but its
pass/fail is reported alongside the result, not folded into
`REPRODUCED`/`DIVERGED`: `-verify` is only deterministic *by convention* (see
[Determinism](#determinism)), while the tree comparison is the one claim
integration's own determinism actually backs.

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
| `SIGBOUND_IMPACTED_PKGS` | `-verify-impact` | Space-separated `./relative` package paths: the changed packages plus every transitive reverse dependent (see [Scoped verification](#scoped-verification)). |
| `SIGBOUND_CHANGED_FILES` | `-verify-impact` | Space-separated repo-relative paths this run's landed write-set touched. |

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
    "worktreeKept": "", "timedOut": false, "attempts": 1, "resumed": false
  } ],
  "strategy": "overlay",
  "integrate": {
    "strategy": "overlay", "groups": 3,
    "landed": ["…"], "flagged": [], "resolved": 0,
    "finalSHA": "…", "wallMs": 12
  },
  "verify": {
    "ran": true, "ok": true, "attempts": 1, "repaired": false, "flaky": false,
    "scope": "impact", "impactedPkgs": ["./a", "./b"], "cached": false,
    "output": "…",
    "repairs": [ { "n": 1, "filesTouched": ["…"], "verifyOk": true } ]
  },
  "logDir": "…",
  "agentCmd": "…", "resolverCmd": "…", "verifyCmd": "…", "repairCmd": "…", "plannerCmd": "…",
  "version": "0.2.0",
  "startedAt": "2025-01-01T00:00:00Z"
}
```

- `integrate.landed` / `integrate.flagged` — branches that merged vs. branches
  set aside for a human (real conflicts).
- `perAgent[].resumed` is `true` iff `-resume` reused that task's `agent/<id>`
  branch outright instead of running its agent again (see
  [Resume](#resume)); always `false` on a run without `-resume`, and false
  for any task `-resume` still ran fresh (a missing or stale no-op branch).
- `integrate.resolved` — overlapping branches that still landed (auto-merged or
  resolver-resolved).
- `verify.ok` is the bottom line: `false` means nothing was landed onto `-base`.
- `verify.scope`/`verify.impactedPkgs` are present iff `-verify-impact` was
  set (see [Scoped verification](#scoped-verification)); `scope` is `"full"`
  on any doubt, `"impact"` when the scoped command ran.
- `verify.cached` is `true` iff `-verify-cache` served this verdict from a
  prior pass instead of actually running the command (see [Cache](#cache));
  always `false` when `-verify-cache` isn't set, and never `true` for a
  failing verdict.
- `logDir` is present iff `-logdir` was set; it names the directory holding
  each command's full stdout+stderr log (see `-logdir` above).
- `strategy`, `agentCmd`/`resolverCmd`/`verifyCmd`/`repairCmd`/`plannerCmd`,
  `version`, and `startedAt` are the [provenance](#provenance) fields —
  always populated (`resolverCmd`/`verifyCmd`/`repairCmd`/`plannerCmd` are
  `omitempty` and simply absent when that slot wasn't configured for this
  run), whether or not `-manifest`/`-notes` were passed: they're part of the
  ordinary report: `-manifest`/`-notes` just persist it. This is also the
  exact shape `sig replay -manifest FILE` expects.

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
| `agent_done` | `id`, `ok`, `exit`, `attempts`, `files`, `inLane`, `wallMs`, `resumed`* | Once per task, after all of that task's attempts (including `-agent-retries`) finish. *`resumed` is present (`true`) only for a task `-resume` reused outright — see [Resume](#resume) — with `wallMs=0` since no agent command ran; absent for every ordinary task. |
| `integrate_start` | `branches` | Once, before the successfully-committed branches are folded together. |
| `integrate_done` | `landed`, `flagged`, `resolved`, `finalSHA`, `wallMs` | Once, after integration (before landing). |
| `verify_start` | `attempt` | Before each `-verify` invocation. `attempt` is `0` pre-repair, `N` after repair round `N` (matches `-logdir`'s `verify-<n>.log`). |
| `verify_done` | `ok`, `flaky`, `cached`, `attempt`, `wallMs` | After each `-verify` invocation (including `-verify-retries`). `cached` is `true` on a `-verify-cache` hit, and `wallMs` is near-zero since the command never ran. |
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
