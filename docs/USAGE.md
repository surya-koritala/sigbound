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
        [-strategy overlay] [-assert] [-semantic go|off]
        [-resolver CMD] [-resolver-timeout D]
        [(-verify CMD | -verify-preset NAME) [-verify-retries N] [-verify-impact CMD]
          [-verify-cache] [-verify-bisect]
          [(-repair CMD | -repair-preset NAME) [-repair-max N]]]
        [-lanes off|warn|strict]
        [-no-autocommit]
        [-keep-failed]
        [-parallel-agents N]
        [-budget D]
        [-logdir DIR]
        [-events FILE]
        [-manifest FILE]
        [-notes]
        [-publish CMD [-publish-timeout D]]
        [-env-mode inherit|scoped]
        [-env-agent NAMES] [-env-resolver NAMES] [-env-verify NAMES] [-env-repair NAMES] [-env-planner NAMES] [-env-publish NAMES]
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
| `-semantic` | `off` | Opt-in, Go-only, best-effort semantic conflict detector (see [Semantic conflicts (Go)](#semantic-conflicts-go)). `go`: parse each changed `.go` file and merge any branch pair a conservative name-based overlap rule flags into the same partition group. `off`: today's path-only partitioning, unchanged. |
| `-resolver` | — | Conflict-resolver command; low-confidence cases are flagged, never guessed. |
| `-resolver-timeout` | `30s` | Per-conflict timeout for `-resolver` (`0` = none). |
| `-verify` | — | Command run in a detached checkout of the integrated tree; non-zero exit = merge fails and does not land. |
| `-verify-preset` | — | Expand a named per-language build+test preset (`go`, `node`, `python`, `rust`) into `-verify`'s command (see [Presets](#presets)). An explicit `-verify` always overrides its preset. |
| `-verify-retries` | `0` | After a FAILING `-verify` invocation, re-run it up to N more times on the same tree; passes on any green. A pass on a retry marks the report `flaky=true`. `0` = today's behavior. |
| `-verify-impact` | — | Command run INSTEAD of `-verify` when Sigbound can confidently scope it to the impacted Go packages (see [Scoped verification](#scoped-verification)). Requires `-verify` (or `-verify-preset`), which stays the fallback on any doubt. |
| `-verify-cache` | `false` | Cache a PASSING verify verdict and skip re-running the command on an exact repeat (see [Cache](#cache)). A FAILING verdict is never cached. Off by default. |
| `-verify-bisect` | `false` | When the FULL combined tree fails verify (after `-repair` exhausts, if set), bisect the integration groups and land the largest subset whose combined tree still verifies GREEN (see [Verify bisect](#verify-bisect)). Dropped groups go to `integrate.droppedByBisect`, never `flagged`. Requires `-verify`. Off by default. |
| `-repair` | — | Fixer command invoked when `-verify` fails; edits are committed and `-verify` re-runs. |
| `-repair-preset` | — | Expand a named repair-harness preset (`claude`, `codex`, `aider`) into `-repair`'s command (see [Presets](#presets)). An explicit `-repair` always overrides its preset. |
| `-repair-max` | `2` | Max repair attempts before reporting `verify.ok=false` honestly. |
| `-lanes` | `warn`* | Lane enforcement: `off`, `warn`, or `strict` (see [File lanes](#file-lanes)). *`-goal` runs default to `strict` instead unless `-lanes` is set explicitly. |
| `-no-autocommit` | `false` | Do **not** commit edits an agent left uncommitted. By default the driver stages and commits them, so edit-only agents still land. |
| `-keep-failed` | `false` | Keep a FAILED agent's worktree on disk instead of removing it, so it can be inspected. The path is printed and recorded in the report. Successful agents' worktrees are always removed. A kept worktree stays registered with git until you remove it: `git worktree remove <path>` (or `git worktree prune` after deleting the directory yourself). With `-agent-retries`, only the LAST failed attempt's worktree is kept — every earlier attempt is torn down as it fails. |
| `-parallel-agents` | `0` | Cap how many agents run concurrently — the fan-out semaphore every agent goes through (see [Parallelism](#parallelism)). `0` (default): today's behavior, unchanged — `runtime.GOMAXPROCS(0)`. Must be `>= 0`. |
| `-budget` | `0` | Wall-clock ceiling for the whole run: the agent phase, integrate, and verify combined (`0` = none). On expiry, outstanding agents are cancelled and fail; integrate/verify then run against whatever's left of that same deadline, and if they can't complete, the run reports an operational error naming the budget instead of landing anything partial. |
| `-no-disk-check` | `false` | Skip the disk-space preflight. By default, before any agent runs, the driver refuses the run (exit 1, no agent started) when task count x checked-out tree size, padded by a 10% safety margin, clearly won't fit free space on the worktree root's filesystem; the check fails OPEN (never blocks the run) whenever the estimate can't be confidently formed, e.g. an unreadable tree or an unsupported platform. |
| `-logdir` | — | Write each agent/repair/verify/planner command's **full** stdout+stderr to `<logdir>/<name>.log` (`agent-<id>.log`, `repair-<n>.log`, `verify-<n>.log`, `planner.log`), on top of the truncated tails the report keeps in memory. The directory is created if needed and must be writable — checked before any agent runs, so a bad `-logdir` fails the whole run rather than silently dropping logs partway through. Repeated runs against the same `-logdir` **append** to the same files; there is no per-run rotation. A task's `id` is sanitized for use in the filename (non-alphanumeric characters become `-`), so two exotic ids that sanitize to the same string share one log file. |
| `-events` | — | Stream one JSON object per line to FILE as the run progresses (see [Events](#events)); `-` writes to stderr. The file is truncated fresh each run. Opening it is checked before any agent runs, same fail-fast policy as `-logdir`; a write failure afterward is best-effort and never fails the run. |
| `-manifest` | — | Write the full JSON report to FILE at the end of the run, independent of `-json` (see [Provenance](#provenance)). FILE's directory is created if needed and checked writable before any agent runs, same fail-fast policy as `-logdir`; a write failure AFTER the run completes is best-effort and warned on stderr — by then real work may already be landed. With `-resume`, this SAME flag also names the prior run's manifest to resume FROM (see [Resume](#resume)). |
| `-resume` | `false` | Resume a prior run instead of planning/loading tasks fresh (see [Resume](#resume)). Requires `-manifest`; `-tasks`/`-goal` must not be passed. |
| `-notes` | `false` | When the run LANDS, attach the full JSON report as a git note on the landed commit under the namespaced `refs/notes/sigbound` (see [Provenance](#provenance)). Best-effort: a failure is warned on stderr but never fails the run, since landing already happened. |
| `-publish` | — | Command (run via `sh -c`, cwd = the TARGET REPO) run once, after the run LANDS (see [Publish](#publish)). A failure doesn't unland the work; it's reported in `publish` and the run exits `6`. Default `""`: off. |
| `-publish-timeout` | `120s` | Timeout for the `-publish` command (`0` = none). |
| `-env-mode` | `inherit` | Environment given to every `-agent`/`-resolver`/`-verify`/`-repair`/`-planner`/`-publish` command (see [Scoped environments](#scoped-environments)). `inherit`: the full parent environment plus that slot's own `SIGBOUND_*` vars — today's behavior, byte-identical. `scoped`: only a minimal base environment (`PATH`, `HOME`, `USER`, `SHELL`, `TMPDIR`, `LANG`, `LC_*`, `TERM`, `GIT_*`) plus that slot's `SIGBOUND_*` vars plus whatever its own `-env-*` flag allowlists through — nothing else from the parent. |
| `-env-agent` | — | `-env-mode scoped` only: comma-separated extra parent-env variable NAMES (or `NAME_*` globs) passed through to `-agent`, e.g. `ANTHROPIC_API_KEY`. A name unset in the parent is silently skipped. Ignored in `-env-mode inherit`. |
| `-env-resolver` | — | Same as `-env-agent`, for `-resolver`. |
| `-env-verify` | — | Same as `-env-agent`, for `-verify`. |
| `-env-repair` | — | Same as `-env-agent`, for `-repair`. |
| `-env-planner` | — | Same as `-env-agent`, for `-planner`. |
| `-env-publish` | — | Same as `-env-agent`, for `-publish`. |
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

### Parallelism

`-parallel-agents` caps the number of agents `driveRun` runs at once — the
fan-out semaphore every task's agent command queues on. The default (`0`)
keeps today's behavior: `runtime.GOMAXPROCS(0)`, a cap sized for CPU-bound
work. Model agents are not CPU-bound — each one spends almost all its wall
clock waiting on a network call, so `GOMAXPROCS` under-uses a batch far
larger than your core count (on a 10-core laptop, only 10 of 512 agents run
at a time no matter how idle the network is).

**Raise it** (well past `GOMAXPROCS` — `-parallel-agents 64` is a legitimate
ask for I/O-bound calls) to trade more concurrent API usage for a shorter
wall clock, up to whatever your model provider's rate limits allow.
**Lower it** — down to `-parallel-agents 1`, fully serial — when a
rate-limited key can't sustain many concurrent requests: better one agent at
a time than a batch that trips 429s and burns retries. There's no wrong
default to override; pick the number your provider's limits and your
patience for wall clock actually allow.

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

A cache hit always reports `verify.flaky: false`, even if the ORIGINAL run
that populated the entry only passed after a `-verify-retries` retry — only
the PASS itself is stored, not whether it was flaky getting there.

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
`-verify-bisect` (see [Verify bisect](#verify-bisect)) and `-verify-cache`
(see [Cache](#cache)) both assume `-verify` is deterministic; an undocumented
flaky command will confuse them.

### Verify bisect

`-verify` is all-or-nothing on the fully-merged tree: one broken agent turns
the whole batch red and lands nothing. `-verify-bisect` turns that into an
honest **partial land** — when the full tree fails, it finds the largest subset
of the run's work that still verifies green and lands just that, dropping the
rest.

```bash
sig run ... -verify 'go build ./... && go test ./...' -verify-bisect
```

**The atomic unit is the integration GROUP, not the branch.** The OCC
partition already splits branches into mutually-disjoint groups by write-set
overlap (see [Strategies](#strategies)); branches within a group are entangled
(they touch shared paths), so bisect keeps or drops a whole group at a time,
never an individual branch inside one. Recombining a subset is cheap: the
per-group folded heads were already computed during integration, so a candidate
subset is just an object-store overlay of those heads onto the base — no
re-folding, no worktree churn beyond the verify checkout itself.

**Strategy.** With **≤ 6 groups** each group is verified ALONE (at most `k`
runs — precise, and it finds every individually-green group). With **more than
6**, bisect binary-splits instead, to keep the probe count low on a batch with
only a handful of bad groups.

**The union-must-verify rule.** After the individually-green groups are
identified, their **combined tree is verified once more** — and only if THAT
passes does it land. This is the whole safety contract: the `-verify` gate
applies to the *exact tree that lands*, never to a subset merely *assumed* good
from probing its parts in isolation. So two groups that each pass alone but
fail *together* (an interaction failure) land **NOTHING** — honestly reported,
never a subset that was never verified as a whole. Likewise a batch where no
group passes alone lands nothing.

**Interplay with the other verify knobs:**

- **`-repair` gets first shot.** Bisect is the *last* resort: the repair loop
  runs against the whole combined tree first, and only if it can't make the
  full tree green does bisect start carving. (Repair's throwaway fix commits are
  discarded when bisect falls back to the original group heads.)
- **`-verify-retries`** apply per candidate verify — a flaky probe re-runs on
  its own candidate tree, same as a normal verify.
- **`-verify-cache`** composes naturally and is a big win here: every candidate
  tree is its own cache key, so repeated bisects (or a group that reappears
  unchanged across runs) skip re-verifying trees already proven green.
- **`-verify-impact`** scopes per candidate: each subset's impacted-package set
  is computed from just that subset's branches' changed files.
- **`-budget`** still bounds the whole thing; a budget-cancelled bisect lands
  nothing and reports the exhausted budget, same as any other cancelled phase.

**Cost warning.** Bisect trades verify runs for salvaged work. The `≤ 6`
path is at most `k + 1` verifies; the binary-split path is bounded but, on a
large batch where nearly every group is broken, can approach ~2k verifies
(each of which may itself be a full build+test). Reserve `-verify-bisect` for
batches where salvaging the good work is worth the extra verify time —
`-verify-cache` blunts the cost sharply on repeated runs.

**Exit code.** A bisect that lands a nonempty green subset exits **`0`** — but
the summary and JSON report show every dropped group, so a partial land is
never silent. A bisect that salvages nothing keeps exit **`3`** (verify
failed), exactly like an un-bisected red run.

### Semantic conflicts (Go)

`-strategy`'s partition (see [Strategies](#strategies)) only reasons about
**paths** — two branches land in independent parallel groups whenever their
write-sets are disjoint, even when they touch completely different files. That
is usually right, but not always: a branch that changes function `F`'s
signature in `a.go` and a branch that adds a brand-new call to `F` in `b.go`
are path-disjoint and merge with **zero textual conflict** — then the build
breaks. `-verify` still catches this (that is its job), but only after the
fact, once the whole batch has already landed together.

```bash
sig run ... -semantic go
```

`-semantic go` is an OPT-IN, Go-only, best-effort pass that runs **after** the
agents finish and **before** integration. For every branch, it parses (via
`go/parser`/`go/ast` — stdlib only, no type-checking) the base and branch
versions of every changed `.go` file and extracts two symbol sets, both
qualified by directory (Go's own package-per-directory convention stands in
for a real package identity, since nothing here loads the module):

- **Declared** — top-level funcs (including receiver-qualified methods, e.g.
  `(*Cell).Integrate`), types, consts, and vars that were ADDED, REMOVED, or
  had their signature/underlying type change.
- **Referenced** — identifiers newly called or selected that resolve, by
  NAME, to a symbol elsewhere in this module: a bare call `F(...)` is
  attributed to the calling file's own package; a selector call `pkg.F(...)`
  is attributed to `pkg`'s package only when its import path is inside this
  module (an external or stdlib import never matches, so it never creates an
  edge).

**The rule.** If branch A declared-changed symbol `S` and branch B newly
references `S` — or also declared-changed `S` — the two branches are
semantically overlapping: they are unioned into the SAME partition group,
exactly the union-find `-strategy` already builds from path overlap, so they
serialize through the normal overlap path (fold + `merge-tree` + `-resolver`)
instead of landing in independent parallel groups.
`report.integrate.semanticEdges` lists every branch pair the analysis merged
this way; `report.perAgent[].semanticNote` records `"analyzed"` or why a
branch was skipped (see [JSON report](#json-report)).

**Precision limits — read this before trusting it.** There is no
type-checking (`go/types` is deliberately not used: loading a whole module's
type information is slow, and the entire point of this pass is a cheap,
best-effort signal, not a second compiler) and no scope resolution, so
matching is by NAME alone. A local variable or parameter that happens to
share a name with a package-level symbol elsewhere can produce a false edge
(over-serializing — never a correctness risk, just a missed parallelism
opportunity); a method call through a receiver variable (`t.M()`) is **not**
resolved at all, since telling `t`'s type apart from an arbitrary identifier
needs type information this pass does not have — only bare calls and
package-qualified selectors are. This trades recall for cost and honesty:
**`-verify` remains the source of truth either way** — a symbol-level
collision this pass misses is still caught exactly as it is today, by
`-verify` going red on the combined tree.

**Fails open, always.** A parse failure, a non-Go file in a branch's
write-set, or a git read error means that ONE branch contributes NO semantic
edges — never a reason to fail the run, and never a reason to keep that
branch from integrating on its path-based merits alone. `-semantic off`
(the default) skips the analysis entirely: partitioning stays exactly today's
path-only behavior, byte-for-byte.

**`-dry-run` composes, with one caveat.** `-dry-run` previews the predicted
partition before any agent has run, so there is no branch content yet to
analyze — its preview stays PATH-ONLY even with `-semantic go` set (see
[Dry run](#dry-run)). The semantic pass only ever runs as part of an actual
`sig run`.

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

`-dry-run` composes with `-semantic go`, but the preview stays **path-only**:
`-semantic go`'s analysis needs each branch's actual committed content (see
[Semantic conflicts (Go)](#semantic-conflicts-go)), and `-dry-run` exits
before any agent — hence any branch — exists. The predicted `groups` above
reflect `cell.Partition` alone, never `cell.PartitionSemantic`.

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
| `0` | Landed and verified (or `-verify` was not set); `-publish` succeeded or was not set. |
| `1` | Operational error (bad flags, a git/integrate failure, etc.). |
| `2` | Usage error (bad top-level `sig` invocation). |
| `3` | `-verify` failed; nothing landed. With `-verify-bisect`, this means bisect salvaged no green subset either. |
| `4` | One or more branches flagged as conflicts (the rest landed). |
| `5` | No agent succeeded. |
| `6` | Landed (and verified), but `-publish` failed — see `publish` in the [JSON report](#json-report). |

When a run matches more than one of these, the most severe wins, in this
order: `1` > `3` > `5` > `4` > `6`.

With `-verify-bisect`, a run that lands a nonempty green subset counts as
"landed and verified" and exits `0` — the dropped groups appear in the report
(`verify.bisect` and `integrate.droppedByBisect`) but don't raise the code (a
dropped group is not a flagged conflict). Only a bisect that salvages nothing
keeps exit `3`. See [Verify bisect](#verify-bisect).

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
`-config` merging), `envMode` (`-env-mode`'s value, `inherit` or `scoped` —
see [Scoped environments](#scoped-environments)), `version` (the sigbound
version that produced this report), and `startedAt` (an RFC3339 timestamp
for when the run began).

**These commands are redacted NOTHING.** They're your own shell commands,
recorded verbatim — if you baked a secret (an API key, a token) into
`-verify` or `-resolver`, that secret is now sitting in the manifest file and
in the git note on the landed commit. Treat both accordingly: don't commit a
`-manifest` file to a public repo, and remember `-notes` writes into the
target repo's own object store.

**`envMode` is the one exception.** Everything above (`agentCmd`, etc.) is
recorded verbatim; `envMode` deliberately is not extended the same way — the
`-env-*` allowlists (which variable NAMES each slot was permitted to see)
and, above all, the actual resolved environment each command ran with
(parent vars passed through, `SIGBOUND_*` values) are NEVER written to the
manifest, the note, or anywhere else. Only the mode name (`inherit` or
`scoped`) is. See [Scoped environments](#scoped-environments).

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

### Publish

`sig run` advances a LOCAL branch and stops there — the integrated commit
never leaves the repo it ran against. `-publish CMD` is the seam that gets it
somewhere a team actually reviews: it runs `CMD` via `sh -c`, cwd = the
**target repo itself** (not a throwaway worktree, unlike `-agent`/`-verify`/
`-repair` — publishing acts *on* the repo, e.g. `git push`, rather than
editing its content), exactly **once**, and only after the run has genuinely
**LANDED**: the base ref actually advanced, which already implies `-verify`
was green or never set. It never runs on a run that failed verify, had no
agent succeed, or landed nothing (every branch flagged as a conflict).

Sigbound stays host-agnostic on purpose — it never calls `gh`, `glab`, or any
other host CLI itself, and the `sig run`/`-publish` path itself never starts a
server or acts as a git host (see the top-level [How it works](../README.md#how-it-works)
overview). `sig serve` is a separate, explicit opt-in daemon (see [`sig
serve`](#sig-serve)) — running it is a deliberate choice, never a side effect
of `sig run` or `-publish`. `-publish` shells out to whatever your repo
already uses instead.

The **full JSON run report** (the same document `-json` prints and `-manifest`
writes to disk) is piped to the command's **stdin** — its own `publish` field
is absent there, since this call is what fills it in. This is the delivery
mechanism for the report's contents; read whatever you need from it with
`jq`, e.g. `jq -r .integrate.finalSHA` to pull the landed commit off stdin.

It also receives the same `SIGBOUND_*` env-var pattern as every other slot
(see [Environment variables](#environment-variables)): `SIGBOUND_FINAL_SHA`
(the landed commit), `SIGBOUND_BASE_BRANCH` (the base branch name),
`SIGBOUND_BASE_SHA` (the base commit BEFORE this run), `SIGBOUND_REPO`,
`SIGBOUND_LANDED` (space-separated names of the branches that actually
landed), and `SIGBOUND_MANIFEST` (the `-manifest` path when set, else empty —
just a *pointer* to where the manifest will live, not a promise the file
already exists: `-manifest` itself is written back to disk only after
`driveRun` returns, same as always — use stdin for the report's actual
contents).

Two common patterns, depending on whether `-base` is the branch you actually
review against or a throwaway integration branch:

```bash
# Direct push: -base IS the branch reviewers look at.
sig run ... -publish 'git push origin "$SIGBOUND_BASE_BRANCH"'

# Push to a fresh branch + open a PR/MR against an upstream base — for when
# -base is an internal integration branch, not what you review against.
sig run ... -publish 'git push origin "$SIGBOUND_BASE_BRANCH:refs/heads/sigbound/$SIGBOUND_FINAL_SHA" \
  && gh pr create --base main --head "sigbound/$SIGBOUND_FINAL_SHA" \
       --title "sigbound: $SIGBOUND_LANDED" --fill'

# GitLab equivalent of the second pattern:
sig run ... -publish 'git push origin "$SIGBOUND_BASE_BRANCH:refs/heads/sigbound/$SIGBOUND_FINAL_SHA" \
  && glab mr create --source-branch "sigbound/$SIGBOUND_FINAL_SHA" --target-branch main --fill'
```

**A `-publish` failure never unlands the work or flips `verify`'s verdict** —
by the time `-publish` runs, the base ref has already advanced and the work
is landed and good. It's recorded honestly in its own report field,
`publish: {ran, ok, exit, output}` (a `null`/absent field when `-publish`
was never set, or the run never landed — a run without it reports
byte-identical to before `-publish` existed), and the process exits `6`
instead of `0` (see [Exit codes](#exit-codes)) so CI can tell "landed but
didn't publish" apart from a clean run without parsing output.

`-publish-timeout D` (default `120s`, `0` = none) bounds the command the same
way every other timeout in this tool does; with `-logdir`, its full output
streams to `publish.log` alongside the truncated tail the report keeps in
memory.

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
2. Excludes any agent whose branch is listed in the manifest's
   `integrate.droppedByBisect` (see [Verify bisect](#verify-bisect)) from
   that set. A bisect run that salvaged a subset recorded
   `integrate.finalSHA` for the LANDED SUBSET only, not the full agent set —
   re-integrating the same subset reproduces it byte-identically (folding is
   deterministic), while including the dropped branches would recompute a
   different, larger tree and falsely `DIVERGE`.
3. Re-integrates the remaining exact SHAs with the recorded `strategy` and
   `resolverCmd`, via the same `integrateBranches` path `sig run`/`sig
   integrate` use — with `land=false` (like `sig integrate -no-land`).
   Replay is **read-only**: it never moves any ref.
4. If the manifest recorded a `verifyCmd`, re-runs it against the recomputed
   tree and prints whether it still passes — informational only; see below.
5. Compares the recomputed tree's OID against the recorded
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

## Distributed workflow (bundles)

Everything above runs on one machine: agents commit branches into one repo and
`sig integrate` folds them there. `sig export` / `sig import` split that across
machines using git's own **bundles** — the on-top-of-git, host-free way to move
objects. A bundle is one ordinary file; there is no server and no custom
protocol.

The pattern is **worker → coordinator**:

- A **worker** machine runs its agents against its own clone and `sig export`s
  the resulting branches into a bundle file.
- A **coordinator** machine `sig import`s that bundle and then folds and lands
  the imported branches centrally with `sig integrate` — same engine, same
  conflict safety (real conflicts flagged, never guessed) as a single-machine
  run.

**No networking here.** sigbound builds the two ends; the file moves by whatever
means you already have — `scp`, a shared/NFS directory, an S3 or CI-artifact
`get`. If you can copy a file between the two machines, you have the transport.

### `sig export`

```
sig export -repo PATH -bundle FILE -branches b1,b2,.. [-json]
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-repo` | — | The worker's git repository. |
| `-bundle` | — | Path of the bundle file to write. |
| `-branches` | — | Comma-separated branch names to export. Each is bundled with its complete history, so the file imports into any repo with nothing to pre-satisfy. |
| `-json` | off | Emit the result as JSON. |

Branches are validated to exist **before** anything is written — a typo is a
clean error naming the missing branch, not a half-written bundle.

### `sig import`

```
sig import -repo PATH -bundle FILE [-from WORKER_ID] [-json]
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-repo` | — | The coordinator's git repository. |
| `-bundle` | — | Path of the bundle file to import. |
| `-from` | bundle filename stem | Worker id; namespaces the imported branches. |
| `-json` | off | Emit the result as JSON. |

**The namespace is the safety property.** Every imported branch lands as
`imported/<worker-id>/<original-branch>` — a bundle carrying `main` or
`agent/foo` can therefore **never** move the coordinator's `main` or clobber a
local `agent/*` branch. Import writes refs *only* under
`refs/heads/imported/<worker-id>/` and nowhere else. (`git bundle unbundle`
imports objects but writes no refs of its own, so the only refs that ever appear
are the namespaced ones sigbound creates.)

The bundle is **verified before** it is unbundled, so a corrupt or truncated
bundle fails loudly and imports nothing. Re-importing the same bundle under the
same worker id is idempotent (the objects are already present and the namespaced
refs are set to the same commits).

Imported branches are just branch names, so they feed straight into the ordinary
`sig integrate` — there is no separate "integrate a bundle" path:

```
sig integrate -repo COORD -base main -branches imported/w1/agent/t1,imported/w1/agent/t2
```

### End-to-end example

`sig integrate` on its own has no verify gate — unlike `sig run -verify`, it
folds and lands (or, with `-no-land`, folds and stops) without ever running a
build or test command. On a coordinator that's importing branches it didn't
build itself, land only what you've verified: integrate detached with
`-no-land`, test the printed `finalSHA`, and land only on green.

```sh
#!/bin/sh
set -eu

# --- on the WORKER: run agents, then bundle their branches ---
sig run -repo /work/clone -base main -tasks tasks.json -agent ./agent -no-land
sig export -repo /work/clone -bundle /tmp/worker-a.bundle \
  -branches agent/t1,agent/t2,agent/t3

# --- move the file by any means you like (no server involved) ---
scp /tmp/worker-a.bundle coordinator:/tmp/worker-a.bundle

# --- on the COORDINATOR: import under a namespace ---
sig import -repo /srv/main -bundle /tmp/worker-a.bundle -from worker-a -json
# imported/worker-a/agent/t1, imported/worker-a/agent/t2, imported/worker-a/agent/t3

# --- integrate WITHOUT landing: fold the branches, leave the result detached ---
sig integrate -repo /srv/main -base main -no-land -json \
  -branches imported/worker-a/agent/t1,imported/worker-a/agent/t2,imported/worker-a/agent/t3 \
  > /tmp/integrate.json
finalSHA=$(jq -r .finalSHA /tmp/integrate.json)

# --- verify the integrated tree BEFORE it ever touches main ---
git -C /srv/main worktree add /tmp/verify-worktree "$finalSHA"
( cd /tmp/verify-worktree && ./run-tests.sh )   # your build/test command
git -C /srv/main worktree remove /tmp/verify-worktree

# --- land only on green: re-run integrate now that it's verified ---
sig integrate -repo /srv/main -base main \
  -branches imported/worker-a/agent/t1,imported/worker-a/agent/t2,imported/worker-a/agent/t3
# ...or, since you already have the exact tree tested, advance the ref directly:
git -C /srv/main update-ref refs/heads/main "$finalSHA"
```

The result is identical to integrating those branches on the worker itself —
the transport is lossless (the imported trees are byte-for-byte the worker's).
`sig integrate` folds with the same in-object-store engine as a
single-machine run: non-conflicting branches land, real conflicts are flagged
rather than guessed.

Re-running `sig integrate` a second time (without `-no-land`) recomputes and
lands the identical tree because folding is deterministic — the second run
isn't redoing the test, just publishing the SHA you already verified;
advancing the ref by hand with `git update-ref` does the same thing without
paying for a second fold. Either way, nothing lands on `main` until the
`finalSHA` you tested is the `finalSHA` you land. Centrally-gated landing —
the coordinator itself running the `-verify` command, the way `sig run` does
on a single machine — is the serve layer's job (issue #60, not yet built);
until then, this `-no-land` → verify → land pattern is how you keep
sigbound's verify-gated-landing promise on a coordinator.

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
disk: repo tree ~2.1MB, free 41.2GB on /tmp (a 512-agent run needs ~1.0GB)
gc: 1 stale worktree(s), 2 sweepable branch(es), 0 old tempdir(s) (run sig gc)
```

The `disk:` line is unconditional (informational, not a pass/fail check — it
never contributes to doctor's exit code) and reports free space on the OS
temp directory, since that's the filesystem `sig run` actually creates
worktrees under (`os.MkdirTemp`), not necessarily the repo's own. When the
repo lives on a different filesystem than the temp directory (a common trap
in CI, where `/tmp` is a small tmpfs), the line shows both readings instead:
`free on temp: X (path) · free on repo fs: Y (path)`.

The `gc:` line is the same posture: unconditional and informational, never a
pass/fail check. It's [`sig gc`](#sig-gc)'s own dry-run plan (default
`-older-than`, never `-force`), so the counts match exactly what a bare `sig
gc -repo ... -delete` would remove right now — run `sig gc` to see and act on
the detail.

`sig run` and `sig integrate` also run the cheap part of this (git present +
version check) automatically before doing anything, so a too-old git is
caught before any agent runs; they do **not** run the live probe (it's
overkill to pay for on every invocation) — run `sig doctor` directly for the
full picture.

---

## `sig gc`

Sweeps debris a killed or crashed `sig run` (or `sig integrate`/`sig
verify`/`sig repair`/`sig doctor`) leaves behind: stale git worktree
registrations, sigbound's own tempdirs under the OS temp directory, and old
`agent/*`/`imported/*` branches that outlived their run.

```
sig gc -repo PATH [-older-than 72h] [-delete] [-force] [-json]
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-repo` | *(required)* | Path to the target git repository. |
| `-older-than` | `72h` | Age cutoff for tempdirs (mtime) and `agent`/`imported` branches (commit date). Ignored by worktree-registration pruning, which is always safe regardless of age. |
| `-delete` | `false` | Actually remove debris. Without it, `sig gc` only prints what it would remove and changes nothing. |
| `-force` | `false` | Also delete `agent`/`imported` branches a run manifest under `.git/sigbound/runs` still references (printed as a loud per-branch warning — that run's `-resume` can no longer reuse it). Never bypasses `-older-than`, and has no effect on worktree pruning or tempdirs. |
| `-json` | `false` | Emit the result as JSON: `{worktreesPruned, tempdirs, branchesDeleted, branchesKept}`. |

**Dry-run by default.** Nothing is ever deleted without `-delete` — a bare
`sig gc -repo PATH` only prints the plan and exits `0`. `-delete` executes
the exact same plan it would otherwise have printed, so what you see in a
dry run is what a follow-up `-delete` run does, never an approximation.

**The deletion boundary.** `sig gc` only ever considers three kinds of
debris:

1. **Stale worktree registrations** — entries `git worktree list` reports as
   prunable (their directory is gone). No age gate: a dangling registration
   is safe to prune the moment it's detected.
2. **Sigbound tempdirs** under `os.TempDir()` whose mtime is at or before the
   `-older-than` cutoff — see [Tempdir liveness](#tempdir-liveness-caveat)
   below for exactly what "old enough" means here.
3. **`agent/*` and `imported/*` branches** whose commit date is at or before
   the `-older-than` cutoff **and** aren't referenced by a run manifest under
   `.git/sigbound/runs` (see below).

Nothing else is ever a candidate. In particular, `sig gc` **never touches**
the base branch or any branch outside `agent/` and `imported/<worker>/`,
`refs/notes/sigbound`, or the run history itself under
`.git/sigbound/runs` — deliberately out of scope, see
[Run history and notes are out of scope](#run-history-and-notes-are-out-of-scope)
below.

**Manifest protection and `-force`.** Every `.git/sigbound/runs/*/report.json`
names the branches `sig run -resume` might still reuse (see
[Resume](#resume)): each agent's branch, plus any branch a `-verify-bisect`
run dropped. An old, otherwise-sweepable branch that a manifest still names
is **kept**, not deleted — deleting it would silently break that run's
`-resume`. `-force` overrides this protection and deletes it anyway, printed
with a loud per-branch `FORCED` warning so it's never a silent loss. `-force`
is **manifest-only**: it does not, and cannot, bypass `-older-than` — a
branch younger than the cutoff is never a candidate at all, `-force` or not
— and it has no effect on worktree pruning or tempdirs, which have no
manifest-protection concept to override.

### Tempdir liveness caveat

A sigbound tempdir (`sig-run-*`, `sig-verify-*`, `sig-bisect-*`,
`sig-repair-*`, `sig-replay-verify-*`, `sig-int-*`, `sig-resolve-*`,
`sig-doctor-*` under `os.TempDir()`) is normally removed by its own creator
(`defer os.RemoveAll`) when the command that made it finishes. What
`sig gc` finds is what's left after a hard kill (SIGKILL, an OOM kill, a
crashed machine) skipped that cleanup.

There's no PID file or lock to check, so the directory's mtime is the
**only** liveness signal `sig gc` has: a tempdir newer than `-older-than` is
left alone on the theory that its owning process might still be running.
Be honest about what that does and doesn't prove: **a nested file write
inside a tempdir does NOT refresh the tempdir's own mtime** — only a change
to the directory entry itself (creating or removing a direct child) does.
A long-running agent invocation that's actively writing deep inside its
worktree, without adding or removing anything directly under the tempdir
root, can look idle by mtime alone.

In practice this is a narrow, largely theoretical hole: the default
`-older-than` (`72h`) dwarfs any real run's duration, and the common crash
case (kill, then immediately investigate) leaves an mtime from seconds ago.
It only matters if you deliberately pass a tiny `-older-than` (minutes) while
a run is still actively live — don't do that against a repo with a run in
flight, or double-check for a live `sig`/agent process first.

### Cross-repo tempdir note

Tempdir scanning is **not scoped to `-repo`**. `sig gc` scans the whole OS
temp directory (`os.TempDir()`) for anything matching a sigbound pattern,
regardless of which repo created it — tempdirs carry no repo identity of
their own (no per-repo subdirectory, no marker file). Running `sig gc -repo
A -delete` sweeps stale `sig-*` tempdirs left behind by runs against repo B,
C, or any other repo on the same machine, right alongside `A`'s own. The
worktree and branch stages, by contrast, only ever look at `-repo`'s own git
metadata.

### Run history and notes are out of scope

`sig gc` never removes anything under `.git/sigbound/runs` (the durable
`report.json`/`events.ndjson` history `sig serve` and `-manifest` write —
see [`sig serve`](#sig-serve) and [Provenance](#provenance)) or the
`refs/notes/sigbound` namespace `-notes` writes to. Both are the durable
record of what actually happened; a sweep tool guessing which of those
records are "old enough to matter" is exactly the kind of judgment call this
project doesn't make silently. Manifests already double as `sig gc`'s own
input (they're what makes a branch protected in the first place), so
deleting them via the same tool that reads them would be circular. If you
want to prune run history, do it explicitly — `rm -rf .git/sigbound/runs`
resets it (see [One run per cell](#one-run-per-cell)) — that's a deliberate,
manual act, not something `sig gc` decides on your behalf.

---

## `sig serve`

A thin HTTP wrapper over the **same** `driveRun` orchestration `sig run` uses —
it forks no engine. Point it at one or more repos and it opens each as a cell at
startup; `POST /runs` drives a run through the exact same machinery, so the
verify gate ("nothing lands unless `-verify` passes") holds by construction:
serve adds **no new landing path**. Every run's full report and event stream are
written under the target repo's `.git/sigbound/runs/<runId>/`, so history
survives a restart — the `GET` endpoints read from disk.

```
sig serve -repos /path/a,/path/b [-addr HOST:PORT] [-token-env NAME] [-allow-remote] [-env-mode inherit|scoped] [-env-agent NAMES ...]
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-repos` | — | Comma-separated repo paths, one cell each (required). Every repo is opened at startup; a bad repo fails the whole daemon before it binds. |
| `-addr` | `127.0.0.1:7777` | Host:port to bind. **Loopback by default.** A non-loopback value (`0.0.0.0:…`, a LAN IP, or an empty host like `:7777`) is refused unless `-allow-remote` is also set. |
| `-token-env` | `SIGBOUND_SERVE_TOKEN` | Name of the env var holding the shared bearer token. When that var is set, every request must send `Authorization: Bearer <token>`. |
| `-allow-remote` | `false` | Permit a non-loopback `-addr`. Serve ships no TLS and no user model — you accept responsibility for a TLS-terminating, access-controlled proxy in front of it. |
| `-env-mode` | `scoped` | Environment given to every run's commands. Defaults to **scoped** here (a daemon must not leak its environment by default), unlike `sig run`'s `inherit`. Set once by the operator; a request may narrow it per run but never widen the allowlists. See [Scoped environments](#scoped-environments). |
| `-env-agent` … `-env-publish` | — | Per-slot allowlists for `-env-mode scoped`, exactly as on `sig run`. Operator-set: a request never chooses what env the daemon's commands see. |
| `-max-agents-per-run` | `0` (unlimited) | Reject (`400`) a `POST /runs` whose agent count exceeds N, before any run starts. See [Quotas and metering](#quotas-and-metering). |
| `-max-run-time` | `0` (unlimited) | Cap every run's `-budget` at this duration (e.g. `10m`). See [Quotas and metering](#quotas-and-metering). |
| `-max-parallel-agents` | `0` (unlimited) | Cap every run's `-parallel-agents` (fan-out concurrency) at N. See [Quotas and metering](#quotas-and-metering). |
| `-max-concurrent-runs` | `0` (unlimited) | Reject (`429`) a `POST /runs` once N runs are in flight across ALL cells. See [Quotas and metering](#quotas-and-metering). |

### Posture — single-user, single-process

`sig serve` is a **single-user, single-process daemon, not a multi-tenant
service.** It has no TLS, no accounts, and one shared bearer token at most. If
you expose it past localhost, putting TLS and network access control in front of
it (a reverse proxy) is **your** job. The `-allow-remote` flag exists precisely
so binding a public interface is a deliberate, acknowledged act.

### Auth model

- **Token set** (the `-token-env` var has a value): every request must carry
  `Authorization: Bearer <token>`, constant-time compared.
- **Token unset + loopback bind**: auth is off — dev mode on your own machine.
- **Non-loopback bind**: a token is **required**; serve refuses to start without
  one, even with `-allow-remote`.

### Endpoints

All requests and responses are JSON (events are NDJSON).

| Method & path | Purpose |
|---------------|---------|
| `GET /health` | `{ok, version, cells:[{id, repo}]}` |
| `POST /runs` | Start a run. Returns **202** `{runId, cell, status}` immediately; the run executes asynchronously. |
| `GET /runs` | `{runs:[{id, cell, status, startedAt, finalSHA?}]}`, newest first. |
| `GET /runs/{id}` | `{id, cell, status, startedAt, error?, report?, usage?}` — `status` is `queued`, `running`, `done`, `error`, or `interrupted` (see [Crash recovery](#crash-recovery)); the full run report is present once `done`, and `usage` alongside it (see [Quotas and metering](#quotas-and-metering)). |
| `GET /runs/{id}/events` | The run's `events.ndjson` as written so far (`Content-Type: application/x-ndjson`) — the same lifecycle events `sig run -events` emits. |
| `GET /runs/{id}/usage` | That run's metering record. `404` until the run has written a report. |
| `GET /usage` | Usage totals across all run history, plus a per-cell rollup. |
| `GET /runs/{id}/flagged` | `{runId, cell, flagged:[{branch, paths}]}` — the branches this run flagged and each one's conflicted paths (see [Conflict review UI](#conflict-review-ui)). `404` until the run has a report. |
| `GET /runs/{id}/flagged/{branch}/{path...}` | `{path, base, ours, theirs, baseSHA}` — the three sides of one conflicted path. A side is `null` when the path is absent there (an add/delete conflict). `{branch}/{path...}` must **exactly** match a flagged pair from the listing above; anything else is `404` and reads nothing. |
| `GET /ui` (and `/ui/`) | The read-only conflict-review HTML page (see [Conflict review UI](#conflict-review-ui)). |

The `POST /runs` body mirrors `sig run`'s flags by name (camelCased); durations
are Go duration strings (`"30s"`, `"2m"`). `cell` is a cell id (from `/health`)
or a repo path. Provide either `tasks` (an array of `{id, prompt, files?}`) or
`goal` + `planner` (+ optional `n`, `minTasks`), never both. `agent` is required.
Unknown fields are rejected, so a typo'd knob fails loudly with a 400.

Environment policy is deliberately **not** a request field (it is the operator's,
set on the server) except `envMode`, which a request may set to override the
server default for that run.

### Error responses

Every non-2xx response is `{error, code}`: `error` is a human-readable
message (its wording may change between releases), `code` is a stable,
machine-readable string a programmatic caller (a retry loop, an SDK) can
switch on instead of parsing `error`. `code` is **additive** — it was added
after `error` and existing text is unchanged, so this is SemVer-safe post-1.0.
The vocabulary:

| `code` | HTTP status | Meaning |
|--------|-------------|---------|
| `unauthorized` | 401 | Missing or wrong `Authorization: Bearer` token. |
| `bad_request` | 400 | Request validation failed (bad JSON, a missing/conflicting field, an invalid duration, etc.). |
| `cell_not_found` | 400 | `POST /runs`' `cell` doesn't match any registered cell id or repo path. |
| `env_widen_refused` | 400 | The request's `envMode` tried to widen the server's `scoped` default to `inherit`. |
| `quota_agents` | 400 | `-max-agents-per-run` exceeded. |
| `unsupported_media_type` | 415 | `Content-Type` isn't `application/json`. |
| `cell_busy` | 409 | That cell already has a run in flight (one run per cell at a time). |
| `quota_concurrency` | 429 | `-max-concurrent-runs` exceeded. |
| `run_not_found` | 404 | The run id in the path doesn't resolve to any run. |
| `not_found` | 404 | A known run has no report/usage yet, or a `flagged` path/branch isn't in that run's allowlist — not a run- or cell-lookup failure. |
| `internal_error` | 500 | An operational failure on the server's side (disk I/O, git), not a caller error. |

### One run per cell

A run moves the base ref, so a cell runs **one at a time**: a second `POST` for a
cell whose run is still in flight gets **409 Conflict**. Different cells run
fully in parallel — that repo-level sharding is the whole point of registering
several. Run history is durable per cell under `.git/sigbound/runs/<runId>/`
(`status.json`, `request.json`, `report.json`, `events.ndjson`, `usage.json`),
the same `.git/sigbound` storage `-verify-cache` uses; `rm -rf .git/sigbound`
resets it and it never shows up in `git status`.

### Crash recovery

Every run directory carries a tiny `status.json` phase marker —
`{status, updatedAt, pid}` — written atomically (write-then-rename, same
pattern `-verify-cache` uses) at each transition: `queued` the instant
`POST /runs` accepts the request, `running` once its goroutine actually
starts, and `done`/`error` at the end. Alongside it, `request.json` journals
the exact POSTed request body (no server-side env values are ever in a
request — those live only in the daemon's own `-env-*` flags — so there's
nothing to redact) the moment the run is accepted, before anything runs.

If the daemon is killed mid-run (`kill -9`, an OOM, a host reboot), the run
directory is left with a `status.json` still saying `queued` or `running` —
nobody is ever coming back to finish it. On the **next startup**, `sig serve`
scans every registered cell's runs directory and rewrites any such entry to
`interrupted` (with a note recording that the owning process is gone) before
it accepts its first request. `GET /runs/{id}` (and the `/runs` listing) then
report that run as `{status: "interrupted"}` — plus whatever `events.ndjson`
it managed to stream before dying — instead of `running` forever, which is
what happened before this existed. `usage.json` is never fabricated: if the
run never got far enough to write one, `usage` is simply absent from the
response. Any run whose recorded pid is still alive is left alone by that
startup scan entirely — that covers both a run this same process is still
actively doing and a sibling `sig serve` daemon's live run sharing the same
runs directory; only a run whose recorded pid no longer belongs to any
process gets flipped to `interrupted`. (On a system that recycles pids fast
enough, a dead run's pid could already belong to some unrelated process by
the time recovery runs, in which case it's left alone rather than recovered
— a narrow, best-effort window, consistent with this feature's posture
elsewhere, not a correctness guarantee.)

`sig serve` does **not** auto-resume an interrupted run — that would mean
guessing what to do with partial work, which cuts against the fail-safe
posture (flag, never guess) the whole project holds elsewhere. The journal
exists so a human can: read `request.json` out of the interrupted run's
directory and `POST` that same body back to `/runs` to start it fresh. Any
`agent/<id>` branches the dead run's agents already committed are untouched
on disk (`runAgent` never cleans them up, see [Resume](#resume)) — a re-POST
runs the same tasks again from scratch, agent included, since `sig serve`
does not thread `-manifest`/`-resume` through its own runs today.

### Conflict review UI

When a run flags branches — a real conflict a `-resolver` declined, or none was
set — Sigbound's fail-safe **flags, never guesses**: those branches don't land,
and a human decides. `sig serve` surfaces exactly that, so you can **see** what
was flagged without leaving the daemon or digging blobs out of git by hand.

Open **`GET /ui`** (e.g. `http://127.0.0.1:7777/ui`) in a browser. The page lists
runs; pick one to see its flagged branches, then a branch's path to see the
**three sides side by side — `base` | `ours` (the landed tree) | `theirs` (the
flagged branch)**. It's a single self-contained HTML page: vanilla HTML/CSS/JS,
**no framework, no CDN, no external asset of any kind**, so it works offline on an
air-gapped daemon. File contents are rendered with `textContent` only (never
`innerHTML`), so agent-generated code in a conflicted file can't inject anything.

This surface is **strictly read-only.** It does not resolve, merge, or land
anything from the browser — that would be a new landing path, which Sigbound
deliberately does not have. Resolving a flagged branch stays a CLI act: re-run
with a `-resolver`, or land it yourself with `sig run` / `sig integrate`. The UI
just shows you the data so you can make that call.

The same data is available as JSON — `GET /runs/{id}/flagged` for the listing and
`GET /runs/{id}/flagged/{branch}/{path...}` for one path's three sides (see
[Endpoints](#endpoints)). The path is validated against the run's own flagged set
(an allowlist), so the endpoint can only ever read a path that was actually
flagged — never an arbitrary file.

**Auth in the browser.** The `/ui` shell is served unauthenticated even when a
token is set — it is data-free (all run data comes from the gated `/runs`
endpoints), and a browser navigation cannot carry a bearer token, so gating the
page itself would only make it unreachable. In the default posture — loopback
bind, **no token** — everything just works. If the daemon sets a token, the page
has a field to paste it: it's kept in `sessionStorage` and sent as
`Authorization: Bearer` on every data fetch (never in a URL). This is a
localhost single-user tool, not a multi-tenant auth system; for a token-protected
or exposed daemon, the reverse proxy you already put in front of it (see above) is
the right place to handle browser auth.

### Graceful shutdown

`SIGINT`/`SIGTERM` stops accepting new requests, cancels every in-flight run's
context (they honor it via the same `-budget` machinery), lets them write their
final reports, and exits. A second `Ctrl-C` hard-kills.

### Quotas and metering

A **managed-layer** feature on `serve` only — `sig run` is untouched and stays
uncapped. Everything here is **opt-in** via server flags; leave them all at
their default (`0`) and behavior is byte-identical to before this existed.

**Quotas** are hosted-side ceilings, enforced at `POST /runs` **before a run
starts** (no run directory created, no cell slot held) and, for wall clock,
via the same `-budget` machinery that already bounds a run in flight:

| Flag | Enforcement |
|------|-------------|
| `-max-agents-per-run` | The request's agent count exceeds N → `400`. For an explicit `tasks` request the count is exact; for a `goal` (planner) request the true count isn't known until planning runs asynchronously, so the only synchronously-available number — `n`, the planner's already-validated/defaulted target — is checked instead. A BYO planner that ignores `n` is outside what serve can check before starting. |
| `-max-run-time` | Folded into the run's `-budget` via `min(request budget, server ceiling)` — a request can only make its own budget **stricter**, never laxer than the server's ceiling. An unset/zero request budget (unlimited) becomes exactly the ceiling. |
| `-max-parallel-agents` | Folded into the run's `-parallel-agents` via the same `min(request value, server ceiling)` as `-max-run-time` above — a request can only narrow its own fan-out concurrency, never exceed the server's. An unset/non-positive request value (today's `GOMAXPROCS` default) becomes exactly the ceiling. An over-ask is silently capped, **not** rejected with a `400` — same posture as `-max-run-time`. |
| `-max-concurrent-runs` | N runs already in flight **across ALL cells** → `429 Too Many Requests`, naming the limit. This is on top of the existing per-cell `409` (one run per cell) — different cells can still each hold a slot up to N total. The counter is released on every run's completion, success or operational error alike (a `defer`, so it can't leak even on a panic). |

A quota rejection composes cleanly with everything else: it happens strictly
before the per-cell busy check and the run directory is created, so a
rejected request leaves **zero** trace on disk and never touches a cell's
run slot.

**Metering** is a per-run usage record, always on (it costs nothing — every
number is derived from data `driveRun`'s report already tracks, plus the one
wall-clock bracket only `serve` itself can measure). It's written as
`usage.json` alongside `report.json` under the run's directory, so it
survives a restart the same way. Fields:

| Field | Meaning |
|-------|---------|
| `agentsTotal` / `agentsOk` / `agentsFailed` | Per-agent outcome counts. |
| `integrateWallMs` | `report.integrate.wallMs`, unchanged. |
| `verifyAttempts` | Total `-verify` command invocations: the initial attempt's `-verify-retries` loop, plus one per repair round that reached a re-verify. |
| `repairAttempts` | Repair rounds actually run (`report.verify.attempts`). |
| `verifyWallMs` | Summed wall time of every invocation counted in `verifyAttempts`. |
| `totalWallMs` | The run's full wall clock as `serve` saw it — `POST /runs` acceptance to the run's terminal write. For a `goal` run this includes planning time, which `driveRun`'s own report never sees. |
| `landed` | The base ref actually advanced. **Not** the same as `report.integrate.finalSHA != report.baseSHA` — `finalSHA` is populated with the *integrated* tree even when `-verify` fails and nothing is ever written to the ref. |
| `reportBytes` | Size of `report.json` on disk. |

`GET /runs/{id}/usage` returns one run's record (`404` until it exists — a
still-running run, or one that errored before any agent ran, has none). It's
also embedded in `GET /runs/{id}` as `usage`. `GET /usage` aggregates every
run in history into `{totals, cells: [{cell, repo, usage}]}` — sums of every
field above (`landed`/`runs` as counts) — for the fleet-wide "how much did
this cost to run" story.

**This is not a biller.** There is no price, currency, unit cost, or external
metering call anywhere in `sig serve` — `usage.json` is the DATA layer a
hosted product would meter on (engine work: agents run, integrate/verify wall
time, repair rounds — the honest billable unit for a BYO-model engine that
never sees a token), not a bill itself.

```sh
# Cap agent count, wall clock, and total concurrency:
sig serve -repos /work/api -max-agents-per-run 20 -max-run-time 15m -max-concurrent-runs 4

# Over the cap: 400, no run started.
curl -s localhost:7777/runs -d '{"cell":"/work/api","tasks":[...25 tasks...],"agent":"..."}'
# -> 400 {"error":"agent count 25 exceeds this server's max-agents-per-run 20","code":"quota_agents"}

# One run's usage record:
curl -s localhost:7777/runs/20260722T164530Z-a1b2c3d4e5f6/usage | jq

# Fleet-wide totals:
curl -s localhost:7777/usage | jq '.totals'
```

### curl examples

```sh
# Start the daemon over two repos (dev mode, loopback, no token):
sig serve -repos /work/api,/work/web &

# What's registered?
curl -s localhost:7777/health | jq

# Start a run on the /work/api cell (addressed by path here):
curl -s localhost:7777/runs -d '{
  "cell":  "/work/api",
  "base":  "main",
  "tasks": [{"id":"t1","prompt":"add a healthcheck endpoint","files":["health.go"]}],
  "agent": "claude -p \"$SIGBOUND_TASK\"",
  "verify":"go build ./... && go test ./..."
}'
# -> {"runId":"20260722T164530Z-a1b2c3d4e5f6","cell":"...","status":"queued"}

# Poll it, then read the landed report:
curl -s localhost:7777/runs/20260722T164530Z-a1b2c3d4e5f6 | jq '.status, .report.integrate.finalSHA'

# Follow the event stream:
curl -s localhost:7777/runs/20260722T164530Z-a1b2c3d4e5f6/events

# List history (survives restarts — it's read from disk):
curl -s localhost:7777/runs | jq

# With a token (any non-loopback bind requires one):
export SIGBOUND_SERVE_TOKEN=$(openssl rand -hex 32)
sig serve -repos /work/api -addr 0.0.0.0:7777 -allow-remote &
curl -s -H "Authorization: Bearer $SIGBOUND_SERVE_TOKEN" localhost:7777/health
```

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
| `SIGBOUND_REPO` | `-agent`, `-repair`, `-publish` | Path to the repository. cwd for `-publish` too (unlike `-agent`/`-repair`, which run in a throwaway worktree). |
| `SIGBOUND_BRANCH` | `-agent` | The task's branch name. |
| `SIGBOUND_BASE` | `-resolver` | Path to the base (common-ancestor) version of a conflicted file. |
| `SIGBOUND_OURS` | `-resolver` | Path to the "ours" version. |
| `SIGBOUND_THEIRS` | `-resolver` | Path to the "theirs" version. |
| `SIGBOUND_PATH` | `-resolver` | Repo-relative path of the conflicted file. Write the resolved body to stdout; empty output, a non-zero exit, or a timeout flags the conflict for a human. |
| `SIGBOUND_FAILURE` | `-repair` | The captured `-verify` output to fix. Edit files to fix it; the driver commits the edits and re-runs `-verify`. |
| `SIGBOUND_IMPACTED_PKGS` | `-verify-impact` | Space-separated `./relative` package paths: the changed packages plus every transitive reverse dependent (see [Scoped verification](#scoped-verification)). |
| `SIGBOUND_CHANGED_FILES` | `-verify-impact` | Space-separated repo-relative paths this run's landed write-set touched. |
| `SIGBOUND_FINAL_SHA` | `-publish` | The commit `-base` was advanced to (see [Publish](#publish)). |
| `SIGBOUND_BASE_BRANCH` | `-publish` | The base branch NAME (e.g. `main`). |
| `SIGBOUND_BASE_SHA` | `-publish` | The base commit BEFORE this run (i.e. this run's `baseSHA`). |
| `SIGBOUND_LANDED` | `-publish` | Space-separated `agent/<id>` branch names that actually landed. |
| `SIGBOUND_MANIFEST` | `-publish` | The `-manifest` path, when set; empty otherwise. Just a path pointer — `-manifest` itself is written after the run returns; the full report arrives on **stdin** instead (see [Publish](#publish)). |

---

## Scoped environments

**Threat model.** Every slot above is a shell command **you** supply, and by
default (`-env-mode inherit`) it gets the FULL environment `sig run` itself
was started with — every variable, whether that slot needs it or not. On a
laptop, where you run `sig` in your own shell to drive your own commands,
that's harmless: it's your shell either way. It stops being harmless the
moment one of those commands is LLM-driven (which `-agent`/`-repair`/
`-planner` typically are) or the process is driving several tenants' work at
once (a hosted layer built on `sig run`): an agent prompt is untrusted input
by construction, and a command that can read `os.Environ()` can exfiltrate
anything in it — a secret meant for `-verify` becomes visible to `-agent`
too, and in a multi-tenant setting, one tenant's key becomes visible to
every other tenant's slot. `-env-mode scoped` is least privilege for exactly
this: each slot gets only what it's told to get.

**What `scoped` hands each command:**

1. A minimal, fixed base: `PATH`, `HOME`, `USER`, `SHELL`, `TMPDIR`, `LANG`,
   `LC_*`, `TERM`, `GIT_*` — whichever of these are actually set in the
   parent process (nothing here is invented; an unset one just stays unset).
   Enough to find an interpreter, resolve paths, behave sanely under a
   locale/terminal, and let the command's own `git` calls work normally.
2. That slot's own `SIGBOUND_*` vars (see [Environment
   variables](#environment-variables) above) — unaffected either way.
3. Whatever that slot's `-env-*` flag allowlists through: extra parent-env
   variable NAMES, or `NAME_*` globs for a family of vars (model CLIs often
   read several, e.g. `ANTHROPIC_API_KEY` plus `ANTHROPIC_*` config knobs).
   A name that isn't actually set in the parent is silently skipped — an
   allowlist says what's PERMITTED, not what's REQUIRED. Nothing else from
   the parent environment reaches the command.

**The `GIT_*` family is broader than strict least-privilege**: it passes
through as part of the base env on every slot, unconditionally, so that
ordinary `git` usage keeps working — which also means any git credential
helper configured via `GIT_ASKPASS`, `GIT_SSH_COMMAND`, etc. rides along too.
If a slot must not have git credentials, don't rely on `-env-mode scoped`
alone to withhold them.

**It's per slot**, not global: an allowlisted name reaches only the flag it
was given on. Giving `-agent` a key never exposes it to `-verify`, `-repair`,
or any other slot unless that slot's OWN `-env-*` flag names it too.

```
sig run -repo . -base main -tasks tasks.json \
  -agent 'claude -p --permission-mode acceptEdits "$SIGBOUND_TASK"' \
  -verify 'go build ./... && go test ./...' \
  -env-mode scoped \
  -env-agent ANTHROPIC_API_KEY
```

Here `-agent` sees `ANTHROPIC_API_KEY` (needed to call the model) plus the
base env plus its own `SIGBOUND_TASK`/`SIGBOUND_TASK_ID`/`SIGBOUND_REPO`/
`SIGBOUND_BRANCH`; `-verify` — a plain `go build`/`go test`, no key needed —
sees only the base env. Neither sees anything else from the shell `sig run`
itself was launched in.

`-env-mode inherit` (the default) is unaffected by any of this — every
`-env-*` flag is simply ignored, and behavior is byte-identical to before
`-env-mode` existed. Turning `scoped` on for a command that DOES need
something from the parent (a proxy `HTTP_PROXY`, an ssh agent socket, a tool
config file path) and forgetting its `-env-*` entry fails the SAME way a
missing/wrong credential always would — the command errors out, same as if
you'd never set that variable in your shell at all; there is nothing
`sig run` does differently or worse. See `-env-mode`/`-env-agent`/
`-env-resolver`/`-env-verify`/`-env-repair`/`-env-planner`/`-env-publish` in
[Flags](#flags) above.

---

## JSON report

With `-json`, `sig run` prints a full report. Top-level shape:

```jsonc
{
  "repo": "…", "base": "main", "baseSHA": "…",
  "laneMode": "warn",
  "parallelAgents": 4,
  "tasks":    [ { "id": "…", "prompt": "…", "files": ["…"] } ],
  "perAgent": [ {
    "id": "…", "branch": "…", "sha": "…", "files": ["…"],
    "ok": true, "exit": 0, "autocommitted": false,
    "declaredFiles": ["…"], "actualFiles": ["…"],
    "inLane": true, "strayed": [], "stderr": "",
    "worktreeKept": "", "timedOut": false, "attempts": 1, "setupMs": 3, "resumed": false,
    "semanticNote": "analyzed"
  } ],
  "strategy": "overlay",
  "integrate": {
    "strategy": "overlay", "groups": 3,
    "landed": ["…"], "flagged": [], "resolved": 0,
    "finalSHA": "…", "wallMs": 12,
    "droppedByBisect": ["agent/…"],
    "semanticEdges": [ ["agent/a", "agent/b"] ]
  },
  "verify": {
    "ran": true, "ok": true, "attempts": 1, "repaired": false, "flaky": false,
    "scope": "impact", "impactedPkgs": ["./a", "./b"], "cached": false,
    "output": "…", "invocations": 2, "wallMs": 340,
    "repairs": [ { "n": 1, "filesTouched": ["…"], "verifyOk": true } ],
    "bisect": {
      "ran": true, "attempts": 4,
      "landedGroups": [ ["agent/g0"], ["agent/g1"] ],
      "droppedGroups": [ ["agent/g2"] ],
      "output": "…"
    }
  },
  "logDir": "…",
  "agentCmd": "…", "resolverCmd": "…", "verifyCmd": "…", "repairCmd": "…", "plannerCmd": "…",
  "envMode": "inherit",
  "publish": { "ran": true, "ok": true, "exit": 0, "output": "…" },
  "version": "0.2.0",
  "startedAt": "2025-01-01T00:00:00Z"
}
```

- `integrate.landed` / `integrate.flagged` — branches that merged vs. branches
  set aside for a human (real conflicts).
- `parallelAgents` is the CONFIGURED `-parallel-agents` value (see
  [Parallelism](#parallelism)), not the resolved `GOMAXPROCS`-derived cap —
  omitted at the default (`0`), so a run without `-parallel-agents` reports
  byte-identical to before the flag existed. The resolved numeric cap that
  was actually applied is in the `run_start` event instead (see
  [Events](#events)).
- `perAgent[].resumed` is `true` iff `-resume` reused that task's `agent/<id>`
  branch outright instead of running its agent again (see
  [Resume](#resume)); always `false` on a run without `-resume`, and false
  for any task `-resume` still ran fresh (a missing or stale no-op branch).
- `perAgent[].setupMs` is that task's worktree-setup wall time in milliseconds —
  the cell's locked `--no-checkout` add plus its out-of-lock `reset --hard`
  populate, which run in parallel across agents. Omitted (`0`) for a task
  `-resume` reused, which set up no worktree. The `worktree_setup`
  [event](#events) rolls these up per run.
- `integrate.resolved` — overlapping branches that still landed (auto-merged or
  resolver-resolved).
- `integrate.semanticEdges` and `perAgent[].semanticNote` are present iff
  `-semantic go` was set (see [Semantic conflicts (Go)](#semantic-conflicts-go)).
  `semanticEdges` lists the branch pairs the analysis merged into one
  partition group; `semanticNote` is `"analyzed"` or a `"skipped: …"` reason
  for that one branch. Both are empty/omitted on a run without `-semantic go`.
- `verify.ok` is the bottom line: `false` means nothing was landed onto `-base`.
- `verify.scope`/`verify.impactedPkgs` are present iff `-verify-impact` was
  set (see [Scoped verification](#scoped-verification)); `scope` is `"full"`
  on any doubt, `"impact"` when the scoped command ran.
- `verify.cached` is `true` iff `-verify-cache` served this verdict from a
  prior pass instead of actually running the command (see [Cache](#cache));
  always `false` when `-verify-cache` isn't set, and never `true` for a
  failing verdict.
- `verify.invocations`/`verify.wallMs` count and time every actual `-verify`
  command run: the initial attempt's `-verify-retries` loop, plus one more
  per repair round that reached a re-verify. Both are `0` when `-verify`
  wasn't set. `-verify-bisect`'s own probes are **not** folded in here — they
  have their own count in `verify.bisect.attempts`. `sig serve`'s per-run
  usage record (see [Quotas and metering](#quotas-and-metering)) is built
  from these.
- `verify.bisect` is present iff `-verify-bisect` ran (the full tree failed
  and there were ≥ 2 groups to bisect — see [Verify bisect](#verify-bisect)).
  `landedGroups`/`droppedGroups` list the branch names per group that landed
  vs. were dropped; `attempts` is the number of candidate verifies bisect made.
  When bisect lands a subset, `verify.ok` is `true` and `integrate.landed` is
  narrowed to that subset while the dropped groups' branches appear in
  `integrate.droppedByBisect` (they are **not** conflicts, so never in
  `integrate.flagged`). When it salvages nothing, `verify.ok` stays `false`,
  `landedGroups` is empty, and every group is in `droppedGroups`.
- `logDir` is present iff `-logdir` was set; it names the directory holding
  each command's full stdout+stderr log (see `-logdir` above).
- `publish` is present iff `-publish` was set AND the run LANDED (see
  [Publish](#publish)); absent (not `null`, just omitted) on any run without
  `-publish`, or one that never landed — so a run without `-publish` reports
  byte-identical to before it existed. `publish.ok=false` means the run still
  landed successfully; it's `publish` that failed, reflected in the exit code
  (`6`) rather than `verify.ok`.
- `strategy`, `agentCmd`/`resolverCmd`/`verifyCmd`/`repairCmd`/`plannerCmd`,
  `envMode`, `version`, and `startedAt` are the [provenance](#provenance)
  fields — always populated (`resolverCmd`/`verifyCmd`/`repairCmd`/
  `plannerCmd` are `omitempty` and simply absent when that slot wasn't
  configured for this run), whether or not `-manifest`/`-notes` were passed:
  they're part of the ordinary report: `-manifest`/`-notes` just persist it.
  This is also the exact shape `sig replay -manifest FILE` expects.
  `envMode` is `-env-mode`'s value (`inherit` or `scoped`) — the `-env-*`
  allowlists and the actual resolved environment are deliberately NOT part
  of this report (see [Scoped environments](#scoped-environments)).

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
| `run_start` | `repo`, `base`, `baseSHA`, `tasks`, `parallelAgents` | Once, right after the base ref resolves. `parallelAgents` is the RESOLVED fan-out cap actually applied (see [Parallelism](#parallelism)) — `-parallel-agents`'s own value when set, else the `GOMAXPROCS`-derived default; always a concrete number here even when `-parallel-agents` wasn't passed. |
| `agent_start` | `id`, `branch` | Once per task, right before that agent's worktree/command starts. |
| `agent_done` | `id`, `ok`, `exit`, `attempts`, `files`, `inLane`, `setupMs`, `wallMs`, `resumed`* | Once per task, after all of that task's attempts (including `-agent-retries`) finish. `setupMs` is that task's worktree-setup wall (the locked `--no-checkout` add plus its parallel `reset --hard` populate) for the attempt that ended the loop. *`resumed` is present (`true`) only for a task `-resume` reused outright — see [Resume](#resume) — with `wallMs=0`/`setupMs=0` since no agent command ran and no worktree was set up; absent for every ordinary task. |
| `worktree_setup` | `count`, `totalMs`, `maxMs` | Once, right after the agent fan-out finishes. The run-level rollup of per-agent worktree setup — `count` worktrees set up, `totalMs` the summed setup work, `maxMs` the single long pole — so the setup phase's cost is visible at any scale without a per-agent line. Only fresh (non-`-resume`) agents are counted; omitted when every task was resumed. |
| `semantic_done` | `edges`, `notes` | Once, only when `-semantic go` is set (see [Semantic conflicts (Go)](#semantic-conflicts-go)) — after every agent finishes, before integration starts. |
| `integrate_start` | `branches` | Once, before the successfully-committed branches are folded together. |
| `integrate_done` | `landed`, `flagged`, `resolved`, `finalSHA`, `wallMs` | Once, after integration (before landing). |
| `verify_start` | `attempt` | Before each `-verify` invocation. `attempt` is `0` pre-repair, `N` after repair round `N` (matches `-logdir`'s `verify-<n>.log`). |
| `verify_done` | `ok`, `flaky`, `cached`, `attempt`, `wallMs` | After each `-verify` invocation (including `-verify-retries`). `cached` is `true` on a `-verify-cache` hit, and `wallMs` is near-zero since the command never ran. |
| `repair_start` | `attempt` | Before each `-repair` fixer invocation. |
| `repair_done` | `attempt`, `verifyOk`, `wallMs` | After that round's fixer AND its follow-up `-verify` both finish; `wallMs` covers the fixer only. |
| `bisect_start` | `groups` | Once, when `-verify-bisect` starts (the full tree failed and there are ≥ 2 groups). `groups` is the group count being bisected. |
| `bisect_attempt` | `groups`, `ok` | After each candidate-subset verify. `groups` is the branch names per group in that candidate; `ok` is its verdict. |
| `bisect_done` | `landed`, `dropped` | Once, when bisect finishes. `landed`/`dropped` are the branch names per group that landed vs. were dropped (`landed` empty when nothing was salvaged). |
| `land` | `sha` | Once, right after the base ref advances (never emitted when nothing lands). |
| `publish_start` | — | Once, right before the `-publish` command runs. Only emitted when `-publish` is set AND the run landed. |
| `publish_done` | `ok`, `exit`, `wallMs` | Once, right after the `-publish` command finishes. |
| `run_done` | `ok`, `exitCode`, `wallMs` | Once, always last — even on a mid-run operational error. |

`-events` off (the default, empty `-events`) emits nothing at all.

---

## GitHub Action

`surya-koritala/sigbound` is also a composite GitHub Action ([`action.yml`](../action.yml)
at the repo root), so a workflow can run `sig run` without hand-rolling the
install + invocation itself:

```yaml
- uses: actions/checkout@v4
  with:
    fetch-depth: 0   # sig run needs real history, not a shallow clone

- uses: surya-koritala/sigbound@v0.3.0
  id: sigbound
  with:
    repo-path: .
    tasks: examples/tasks.json
    agent:  'claude -p --permission-mode acceptEdits "$SIGBOUND_TASK"'
    verify: 'go build ./... && go test ./...'

- run: echo "landed ${{ steps.sigbound.outputs.landed-count }} branch(es) at ${{ steps.sigbound.outputs.final-sha }}"
```

See [`examples/sigbound-action.yml`](../examples/sigbound-action.yml) for a
complete workflow, including a `-goal`/planner run. That example — like any
real use of this action — needs your own agent CLI and its auth (an API key
secret, typically) already set up on the runner; sigbound never bundles a
model, in CI or anywhere else, so there's no way to test a *real* agent
inside this repo's own CI.

**What it does.** Three steps, all shell (no third-party actions):

1. **Install.** Downloads the Linux `sig` binary matching `version` (default
   `latest`, resolved via the GitHub API) from this repo's
   [releases](https://github.com/surya-koritala/sigbound/releases), verifies
   it against that release's `checksums.txt`, and puts it on `PATH` for the
   rest of the job. Linux runners only (`ubuntu-latest` or a self-hosted
   Linux box) — the same constraint every step below inherits.
2. **`sig doctor`.** Runs before anything else so a runner with too old a
   `git` fails fast with an actionable message instead of failing deep
   inside the agent run.
3. **`sig run`.** Assembled from this action's inputs — only the flags whose
   input is actually set are passed — with `-json` captured to a report
   file. See [Inputs](#inputs) below for the full mapping.

> **Secrets:** this step logs the assembled `sig run` command line (every
> flag, including `agent`/`verify`/`resolver`/`repair`/`extra-args`) to the
> job log, so pass secret values via the step's own `env:` and reference
> the environment variable from your command — never embed a secret
> literal in an input string (the example above already does this right).

**The exit code is never swallowed.** `sig run`'s [exit code](#exit-codes) is
always published as the `exit-code` output, but a non-zero exit ALSO fails
the step (and so the job) with a message naming what that code means — a
`-verify` failure fails your CI run, honestly, by default. If you want to
inspect the outputs on a failing run instead of stopping the job, add
`continue-on-error: true` to the action's own step and check `exit-code`
yourself in a later step.

### Inputs

| Input | Default | Maps to |
|-------|---------|---------|
| `version` | `latest` | Release to install (`latest`, or e.g. `0.3.0` / `v0.3.0`). |
| `repo-path` | `.` | `-repo` |
| `base` | — | `-base` (sig run's own `main` default applies when unset) |
| `tasks` | — | `-tasks` (mutually exclusive with `goal`) |
| `goal` | — | `-goal` (mutually exclusive with `tasks`) |
| `planner` | — | `-planner` |
| `n` | — | `-n` |
| `agent` | *(required)* | `-agent` |
| `resolver` | — | `-resolver` |
| `verify` | — | `-verify` |
| `repair` | — | `-repair` |
| `strategy` | — | `-strategy` |
| `extra-args` | — | Appended verbatim after every flag above — the escape hatch for anything without a dedicated input (e.g. `-agent-timeout 300s -lanes strict -verify-retries 2`). |

### Outputs

| Output | From the report |
|--------|------------------|
| `exit-code` | `sig run`'s own exit code (0-6; see [Exit codes](#exit-codes)). |
| `final-sha` | `integrate.finalSHA` — empty if nothing landed. |
| `report` | Path to the full JSON report file this run wrote. |
| `landed-count` | `integrate.landed`'s length. |

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
