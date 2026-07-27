# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Before 1.0.0, minor versions may add features and patch versions carry fixes.

## [Unreleased]

## [2.2.0] - 2026-07-27

The trustworthiness milestone. 2.1 made the loop something you could point at a
codebase and leave running; 2.2 is about whether you can believe what it tells
you afterwards.

The headline is a hole in the middle of this product's central claim. `-repair`
could edit the tests that had just failed it, and whatever made the tree green
landed carrying a green verify. That is fixed, and the rest of the release is
the same theme from other angles: a landing's evidence now travels with it
atomically instead of being left behind, the note that carries that evidence is
versioned so a reader can tell what it is looking at, an interrupted run leaves a
record instead of vanishing, an ack can say who approved it, and the rules a
landing is judged by are published as a library so a second implementation of
them can never disagree with the first.

Nothing about what lands changed. The verify gate still decides, the tree that
lands is still byte-for-byte the tree that passed, and every path that moves a
ref still does it with a compare-and-swap.

### Fixed

- **The `-repair` fixer can no longer edit what judged it.** This is the one
  place the product's central claim was false. The fixer is the only agent in a
  run with no brief — it is handed a failure and rewarded for making it stop,
  which makes weakening whatever judged it the shortest path from red to green —
  and nothing stopped it. Lane enforcement runs on the agent path only, and the
  landing holdback is decided from the **agents'** write-sets before the fixer
  has run at all, so the fixer was the single code path that walked straight
  through the ack bar. A fixer that deleted a failing assertion had its edit
  auto-committed, re-verified against a suite with nothing left in it, and landed.

  The fixer's write-set is now checked before its commit is handed back, and a
  match **refuses** the attempt: the commit stays in the throwaway worktree and is
  never referenced, the base does not move, and the repair loop stops — a fixer
  that reached for the bar will reach for it again. Four rules, most to least
  specific: `sigbound.policy` itself, the new `repair-deny` globs, the repo's
  existing `ack-paths`, and an unreadable diff (the write-set is the only thing
  the rules can reason about, so an unknowable one fails closed). The refusal is
  recorded on the attempt as `refused` / `refusedPaths` and emitted as
  `repair_refused`.

  Two asymmetries with the landing rules are deliberate. `sigbound.policy` is
  barred **unconditionally**, including creating one where none existed and
  including repos with no policy at all — a landing may create a first policy
  because that can only raise the bar, but a fixer writing a governance file is
  not fixing a verify failure under any reading. And an `ack-paths` match is
  **refused, not parked**: a park exists to put work in front of a person, and a
  repair is work nobody asked for, so there is nothing to hold.

- **A landing pushed to a remote now arrives with its evidence.** The provenance
  note was not pushed at all, and `-notes` ran *after* `-publish`, so at push time
  there was no note to send. That mattered more than it sounds: "did this run
  already land?" is answered by fetching the base and looking for that run's note,
  so a caller recovering from an interrupted run read *no*, re-ran work that had
  already landed, and paid for it twice — reachable through a plain network
  failure. The order is reversed and the receipt presets now push the branch and
  `refs/notes/sigbound` in one `git push --atomic` transaction: either both refs
  move or neither does. The notes ref is included only when one exists locally,
  `--porcelain` makes the per-ref outcome machine-readable so a refusal names
  *which* ref was refused, and a server without `--atomic` support fails loudly
  rather than degrading into two pushes and silently reopening the window. What
  the note gives up is the publish outcome, which was never its job — whether we
  reached a remote is a fact about the machine and stays in the run report.

- **`sig unland`'s entanglement scan sees both landings of a run that landed and
  parked.** A multi-task run under `ack-paths` lands its clean groups immediately
  and parks the gated one; acking that park makes the run contribute a *second*
  landing from a different fork point. The scan consulted the parking record only
  when the report showed nothing landed, so it saw the first and never the second.
  Safety was never at risk — the conflict gate refused either way — but the
  operator was told to "unland those runs first" while the list named none of
  them. Landings are now collected rather than chosen between, one entry per
  landing, with the run id deduplicated where it is displayed.

- **A CI flake that turned passing tests red in cleanup.** `t.TempDir` removal
  failed with `.git: directory not empty` — something creating a file inside
  `.git` after the foreground git command returned. The suspect was auto-gc, and
  it was not: repos made through `gitx.Init` have had `gc.auto=0` since it was
  written. The unhandled mechanism was **`maintenance.auto`**, a separate switch,
  enabled by default, gating `git maintenance run --auto`, which `gc.auto` does
  nothing about and which daemonizes the same way. Both are now set on every repo
  sigbound makes. **Honest limit:** this eliminates a mechanism, it does not prove
  the flake gone — it appeared in one of two runs on an identical commit and
  cannot be reproduced on demand, so nothing here can assert its absence.

### Added

- **`sig run -run-id ID` — the caller names the run.** `sig run` minted its own
  id, so anything queueing work had to start the run and *then* discover what it
  chose; a run that died before printing left no id at all and no way to find the
  directory it had already made. `-run-id` sets the **existing** id — the run
  directory, the events, the report, and the handle `sig ack` / `sig reject` /
  `sig log` take — rather than adding a second identity the ledger would carry
  alongside. The id becomes both a path component and a git branch component, so
  both are checked, and checked *above* the `-logdir` / `-manifest` preflights,
  because those create directories and the promise is "before any directory is
  created". Reusing an existing id is refused rather than merged into. The
  `run_start` event now carries `runId`, so a caller streaming `-events` sees its
  own handle in line one.

- **`sig run` handles `SIGINT` and `SIGTERM`, and exits `7` when interrupted.**
  There was no handler at all: a stop killed the process outright and the run lost
  everything it had already paid for — no report, no park record, no usage — and
  the caller could not tell an interrupted run from a crashed one. Cancelling the
  run's context does the work, since every agent, verify, repair and publish child
  runs under it, so they are killed rather than orphaned and the existing error
  path salvages the partial record. The run directory is marked `interrupted`, the
  same status the crash-recovery sweep heals a killed run to; the difference is
  that this run wrote its own record instead of being reconstructed from outside.
  A **second** signal exits immediately — the handler unregisters itself on the
  first, so a stuck shutdown can never be un-killable.

  The classification runs on **both** paths out of the driver, which is the part
  worth knowing: cancelling kills the agents, and a run whose every agent was
  killed completes perfectly normally and would otherwise exit "no agent
  succeeded" — a code that is a lie about what happened. It is asked of the
  *signal* context specifically, never a phase's own, so `-budget` exhaustion and
  `-agent-timeout` keep their own codes. Two limits stated rather than left to be
  discovered: an agent's own background children are not put in a process group,
  and Windows gets `Ctrl-C` only — `SIGTERM` is accepted there but never
  delivered, and `SIGKILL` cannot be handled anywhere.

- **`sig ack -by WHO` / `sig reject -by WHO` — who approved this landing.**
  Parking exists because some changes need a person to decide, and the ledger
  could not say which person. Anything driving `sig` from outside knows the
  identity and had nowhere to put it; an approval recorded only in that system
  does not travel with the commit and cannot be checked from a clone. `-by` is
  written into the parking record and, through the record the ack note carries,
  onto the released commit — so `sig log -sha <commit>` answers from a clone that
  has only `refs/notes/sigbound` and no run directory. Also accepted on both HTTP
  verbs as `{"by": "…"}`.

  **It is not authentication.** Sigbound has no user model and is not getting one:
  the string is opaque, uninterpreted, and trusted exactly as far as whoever
  passed it — a daemon's token says a caller *may* act, never *who they are*. A
  value read back from a note is a claim, so it renders as "recorded as approved
  by" and rides inside the ledger-versus-note marking every other provenance
  answer carries. Sanitized once on the way in: control bytes dropped (a newline
  in a note is how a payload forges structure around itself) and the length
  bounded, with dropping before the bound so an all-newline value cannot fill the
  budget and push real characters out.

- **`repair-deny` policy key.** Globs the `-repair` fixer may never write —
  typically a repo's tests and CI config. A separate key from `ack-paths` because
  the two answer different questions: `ack-paths` says *a human must approve this
  change*, which is right for an agent doing requested work and wrong for an
  automatic fixer. A repo that wants its tests off-limits to the fixer while
  ordinary agents keep writing them can only say so here.

- **`noteFormat` — the provenance note is versioned, and its stable fields are
  documented.** The note is deliberately the half of the record that travels: it
  rides on the commit, survives `gc`, and is readable from a clone that never had
  the run directory. Anything outside this repository answering "which run landed
  this commit" has to parse it — and it was a struct that could change shape in
  any release, with no way to tell which shape you were looking at, so a reader
  written against it silently got worse answers after a change instead of failing
  loudly.

  Every note now carries `noteFormat` as its first key, `docs/USAGE.md` documents
  the stable field set and the compatibility rule (adding a field does not bump
  it; removing one or changing what one means does), and a note from a **newer**
  sigbound is **refused rather than guessed at** — the reader falls through to the
  local run ledger, silently and non-fatally. A note with **no** version reads
  exactly as before, so existing repositories keep their history. The version
  belongs to the note, not the product, and never appears in `-json` output or the
  on-disk manifest. It is a statement about **shape, never authenticity**: a note
  is user-writable, and `noteFormat` buys its payload no trust at all.

- **`pkg/policy` and `pkg/attest` — the rules, as an importable library.** The
  landing rules lived in `package main`, which no other module can import, so
  anything enforcing the same bar elsewhere had to reimplement them — and two
  implementations of a gate eventually disagree about the same bytes. `pkg/policy`
  owns the `sigbound.policy` type, parser and evaluators (`HoldReason`,
  `RepairRefusal`, `GlobMatch`); `pkg/attest` reads the provenance note (`Parse`,
  `Landed`, `LandedSHA`, `Concerns`).

  **Neither does any I/O** — no file read, no git, no subprocess, no network — so
  a caller holding only the bytes of a push can use them. `cmd/sig` consumes them
  rather than keeping a copy: its `policy` type is an alias, its helpers are
  re-exports, and the note version gate *is* `pkg/attest`'s, so the whole engine
  suite exercises the published library on every run.

  `pkg/attest` exposes only the documented stable fields, and carries the two
  things a caller gets wrong on its own. `Landed()` exists because
  `integrate.finalSHA` is populated with the integrated tree **even when verify
  went red** and nothing reached the base ref — reading it and concluding "landed"
  accepts a tree that failed — and because an ack-released landing lives in
  `park.landedSHA`, since the report predates the ack. `Concerns(sha)` exists
  because a note is user-writable and arrives with the commit: parsing says what
  the bytes *say*, and a note lifted onto an unrelated commit parses perfectly and
  concerns nothing.

  Still zero dependencies; `go.sum` stays empty.

### Changed

- **`sig policy init` drafts only from a job it can affirmatively confirm gates a
  merge.** It used to draft from every workflow job and then refuse the constructs
  known to break one — a default that needed four rounds of additions at four
  different scopes, each found the same way: a construct nobody had enumerated
  produced a live `verify` member that exits 0 on broken code. The enumeration was
  the problem, because a deny-list of things that break a landing bar is a list of
  everything GitHub Actions will ever add.

  Job scope is now an allow-list: every key a job carries must be one the scanner
  has reasoned about, and anything else refuses the job and is noted by name. That
  covers `container`, `services`, `environment`, `env`, `defaults`, `if` and
  `continue-on-error` — the four rounds' worth — and every key nobody has thought
  about yet. `runs-on` became an affirmative requirement rather than a
  not-contradicted one; a missing value used to be accepted on the grounds that
  workflows rely on a default, and that is precisely the unrecognized input this
  stops trusting, since the battery is drafted *for* the machine verify runs on.
  It costs nothing real — Actions requires `runs-on` for every job.

  The asymmetry is what makes over-refusing cheap: a refused workflow falls
  through to the Makefile and manifest sources, which draft a real battery. Over-
  refusing costs a less specific draft; under-refusing costs a landing bar that
  reports green on failing code. **Scope:** trigger-level checks are still a
  deny-list — job scope is where all four rounds of churn happened.

- **`-notes` now runs before `-publish`.** A consequence of the atomic-push fix
  above: the note no longer records the publish outcome, because it has not
  happened yet. That outcome is still in the run report (`publish`).

- **`refs/notes/sigbound` should be pushed with `--atomic`, not separately.**
  `docs/USAGE.md`'s guidance changed from `git push origin refs/notes/sigbound` to
  `git push --atomic origin main refs/notes/sigbound`, with the token-permission
  trap named: one that can move branches but not `refs/notes/*` now fails the
  whole publish rather than landing a branch without its evidence.

### Internal

- **`ackedLandedSHA`'s two guards are now pinned by tests.** Either could be
  deleted with the whole suite staying green. The status gate is the one that
  matters: both ack paths write `resolvedAt` and `landedSHA` *before* moving the
  ref and only a moved-base refusal rewinds them, so every other landing failure —
  a stale `refs/heads/X.lock`, a rejecting `reference-transaction` hook, ENOSPC —
  leaves a record claiming a landing that never happened. The helper has four
  consumers, and in the entanglement scan that false positive names an innocent
  run as blocking a revert.

## [2.1.0] - 2026-07-26

The agent-operation milestone. 2.0 gave a repository a landing bar and a way to
hold work for a human; 2.1 makes the loop something you can point at a codebase,
leave running, answer for afterwards, and take back when it was wrong.

Nothing about what lands changed. The verify gate still decides, the tree that
lands is still byte-for-byte the tree that passed, and every path that moves a
ref still does it with a compare-and-swap.

### Added

- **`sig policy init` -- a starting `sigbound.policy` drafted from the repo's own
  configuration.** Reads GitHub Actions workflows, Makefile targets, language
  manifests and CODEOWNERS, and writes a policy with every emitted key commented
  with the file and line it came from. It is deliberately conservative: anything
  it cannot justify from the repo becomes a `# unmapped:` note rather than a
  guess, because a `verify` member that is subtly wrong is a bar that gates
  nothing while reporting green. Workflow reading is a line-oriented heuristic,
  not a YAML parser (this binary has no dependencies and is keeping it), and an
  unrecognised shape yields fewer suggestions, never a wrong one. It never
  overwrites an existing policy -- it prints the lines it would have added and
  exits non-zero. Known ceilings, stated rather than guessed: `paths:` /
  `paths-ignore:` are not read, and `merge_group:` is not treated as a merge gate.

- **`sig unland` -- take a landing back out, through the gate.** Reverts what a
  run landed as a new commit on the base, never a history rewrite. An unland is
  itself a landing: the reverted tree is built, the policy's verify battery runs
  against it, and the ref advances only through the same compare-and-swap. A
  reverted tree that fails verify lands nothing; a base whose revert conflicts is
  refused with the conflicting paths named; `ack-paths` park an unland like any
  other landing. It is recorded in the ledger attributed to the run it reverses,
  so `sig log` shows both halves and the reverse edge.

- **`sig log -release FROM..TO` -- release notes from the ledger.** Assembles a
  release document from what actually landed -- each landing's intent, goal,
  agents, verify verdict and landed SHA -- rather than from commit subjects, with
  `-json` for tooling. Commits sigbound did not land are listed as unattributed
  rather than silently omitted: the document never claims a completeness it does
  not have. Untrusted text is confined to its line so a run's goal cannot forge
  sections into a paste-ready document, and a landing recovered from a commit
  note renders in a separate, marked bucket -- a note is user-writable, so its
  payload is never republished as policy or acceptance.

- **Recurring intents and templates.** An intent may declare a `schedule`, which
  `sig serve -watch` makes live: a due intent fires as an ordinary run, through
  the same policy-gated path a hand-started one takes. Firing is bounded -- one
  intent per due tick, alternating with branch collection so a schedule cannot
  starve arrivals, and least-recently-fired first among due intents. The branch a
  fire produces is recorded durably before the agent runs, so losing the watch
  cache costs a re-examination and never a landing judged without the intent's
  own `acceptance`. Templates under `intents/templates/` are instantiated by
  `sig intent new`.

- **Intents -- in-repo statements of work.** One file per intent under `intents/`,
  in the same flat KEY=VALUE dialect as `sig.conf` and `sigbound.policy`, runnable
  with `sig run -intent ID` and attributable in the ledger. An intent's
  `acceptance` composes exactly as a `-verify` flag does -- appended to the policy
  battery, never replacing it -- so an intent can only make a landing bar
  stricter. `sig intent import-github` turns an issue into one.

- **A board and delivery metrics on `sig serve`.** `GET /board` derives intents x
  runs x parks into columns, with delivery metrics beside them, rendered in the
  UI. Read-only and fully derived from the journal -- there is no state to drag a
  card into, and the board cannot disagree with `sig log` about what happened.

- **Publish presets `github-receipt` and `gitlab-receipt`.** After a landing,
  push the landed base and open a pull/merge request whose body is the run's
  receipt -- run id, goal, verify verdict, landed SHA, agent tally, and what did
  *not* land: parked branches awaiting an ack, conflict-flagged branches, and
  bisect-dropped members. On later runs onto the same branch the receipt is
  appended to the open request rather than lost. Every failure path says the
  landing already happened and still stands.

- **Security verify presets.** `govulncheck`, `gitleaks` and `codeql` as
  `-verify-preset` values, each failing loudly when the tool is absent rather
  than passing vacuously.

- **Run event push.** `sig serve` can push its NDJSON run events to an HTTP
  receiver, with the drop counter reporting honestly what it dropped.

- **Metering from `sig run`.** The CLI path now records the same `usage.json`
  `sig serve` records -- agent wall time, token and cost totals, and the per-agent
  cost seam via `SIGBOUND_USAGE_FILE`. Both paths share one writer, so they cannot
  diverge on the rule that matters: a run that failed before landing records
  `landed: false` rather than inheriting a heuristic that is only accurate for a
  completed run.

- **Provenance for acked landings.** A landing released by a human `sig ack` now
  carries a `refs/notes/sigbound` note and is recognised by `sig log -sha` with a
  role that distinguishes it from an automatic landing. These are the landings
  most worth auditing, and the note is the half that travels: it rides on the
  commit and is readable from a clone that never had the run directory.

- **A run directory for `sig run`**, so a park created from the CLI can be acked.

### Changed

- **`cell.Integrate` carries its own stale-branch guard.** The ancestry check
  that prevents a branch forked before the current base from silently deleting
  everything the base gained now lives inside the exported API rather than in the
  CLI that wrapped it. A caller importing the package no longer has to know to
  add it.

- **A run's owner is a process id *and* the scope that id is meaningful in.** A
  bare PID answers wrongly in both directions once runs execute in separate
  namespaces over a shared clone -- a live run reclaimed underneath itself, or a
  dead one never reclaimed because an unrelated process holds the number. A
  record whose scope does not match reads as reclaimable rather than trusted.

- **Every landing path lands with a compare-and-swap.** The ref advances only
  while the base still holds the commit the landing was computed against, so a
  landing that arrived in between is refused rather than reset away.

### Fixed

- **`GET /runs` reported what was integrated, not what landed.** A run that
  integrated a tree and then failed verify showed a SHA as though it had landed,
  and an acked parked landing showed none at all. Both surfaces now share one
  rule, so `/runs` and `/log` cannot disagree.

- **A forged commit note could dress its claim in a real run's identity.** The
  marker that says an answer came from a user-writable note was keyed on a field
  happening to be empty; it is now keyed on where the answer actually came from,
  and a note's payload can no longer supply a run id.

- **The policy battery gates every package the repo's changes land in**, and the
  event vocabulary is held to its documentation in both directions.

- **README figures** corrected: test and fuzz-target counts and coverage had
  drifted a full major version behind.


## [2.0.0] - 2026-07-25

The workflow milestone: a repository can now declare its own landing bar, work
that needs human judgment waits in an inbox instead of blocking a queue, every
landing is answerable, and the daemon can run the whole loop continuously. The
engine is unchanged in what it lands -- the verify gate still decides, and what
lands is still byte-for-byte the tree that passed.

The major version marks a change in what the tool *is*, not a break in how it is
called: every flag, environment variable, and JSON field from 1.x behaves as it
did, and a repository with no `sigbound.policy` runs exactly as it did in 1.1.
Two behaviours changed shape and are called out under Changed.

### Added

- **`sigbound.policy` -- a repo-owned landing bar.** A flat KEY=VALUE file
  committed to the repository declares what a landing requires: a `verify`
  battery (repeatable, ANDed), `lanes`, `semantic`, `assert`, `ack-paths`,
  `audit-sample`, `ack-timeout`, and quota ceilings. It is read from the base
  commit's tree, so the bar is versioned and landing-gated like any other file,
  and it is resolved at one call site shared by `sig run` and `sig serve`, so
  the two cannot drift. Flags may only tighten it: verify commands append to the
  battery, booleans are floors, quotas take the minimum, and an explicitly
  weaker flag is a loud error naming both sources. Unknown keys and malformed
  values are errors naming file, line, and key -- a typo cannot silently weaken
  a bar. The resolved policy's SHA-256 is recorded in the run manifest.

- **Run parking -- ack and reject for landings that need a human.** A run whose
  verified landing touches an `ack-paths` match, or whose own changes modify
  `sigbound.policy`, completes verification and then parks instead of advancing
  the ref: branches kept, reason recorded, `awaiting-ack` status durable across
  daemon restarts. `POST /runs/{id}/ack` and `sig ack` release it through one
  shared function; when the base has not moved the recorded commit is re-checked
  against the object store and landed byte-for-byte, and when the base has moved
  the recorded tree is discarded and the parked branches are re-integrated
  against their own fork point and re-verified under the policy at the new head.
  `POST /runs/{id}/reject` and `sig reject` are terminal and land nothing.
  Resolution is claimed atomically, so a reject that wins the claim guarantees
  nothing lands. The parked commit is pinned by a keep-alive ref under
  `refs/sigbound/park/` and released when the park resolves.

- **`GET /inbox` and an Inbox tab -- everything waiting on a human, in one
  place.** Parked landings, flagged conflicts, bisect-dropped groups, repair
  failures, and audit samples, newest first, filterable by type. Ack and reject
  are the only mutating controls in the review UI, shown only on parked entries,
  under the existing auth and CSP posture.

- **Spot audits.** `audit-sample = N%` surfaces a deterministic sample of clean
  landings as non-blocking inbox entries -- `sha256(runID) mod 100 < N`, so
  selection is replayable with no RNG. Parked and flagged runs are never sampled.

- **`sig log` -- the run ledger.** `sig log` lists runs newest-first with their
  goal, agents, verdict, landed SHA, and policy hash; `sig log -sha <commit>`
  answers which run landed a commit, from which task, by which agent, correctly
  for overlay, octopus, and bisect-salvaged landings, and attributes commits a
  bisect dropped rather than reporting them unknown; `sig log -task <id>` follows
  a task across runs and resumes. `-json` is stable, and `GET /log` and
  `GET /log/sha/{sha}` mirror the CLI through the same reader. A landing note is
  trusted only when it concerns the commit being asked about; otherwise
  resolution falls through to the local manifests.

- **`sig serve -watch` -- continuous integration cycles.** The daemon observes
  local `agent/*` arrivals, `imported/<worker>/*` bundle refs, and `POST /queue`
  enqueues, batches them on `-watch-interval` or `-watch-batch`, and drives
  normal policy-gated runs through the same path a POSTed run takes -- same busy
  lock, same quotas, same journal, same policy resolution, differing only by
  `"source": "watch"` in the manifest. A persisted per-cell seen-set makes cycles
  idempotent; a branch red for `-watch-max-red` consecutive cycles is excluded
  and posted to the inbox until it is re-pushed; shutdown drains the in-flight
  cycle.

### Changed

- **`sig integrate` refuses a branch that does not contain the base.** Such a
  branch's overlay contribution is the *deletion* of everything the base gained
  since it forked, and the write-set partitioner cannot catch it because that
  branch's `base...head` diff is empty -- so it previously landed silently and
  reported success. Every branch is now checked against the base before anything
  reaches the integrator, for every strategy; one refused branch refuses the
  whole batch and nothing is written. Branches from `sig import` routinely
  predate the coordinator's base and must be rebased. See [#130].

- **`sig gc` protects parked runs and reclaims parking refs.** An open park's
  branches are never candidates regardless of age or `-force`, an unreadable
  park record aborts the sweep rather than guessing, and parking refs stranded
  by a crash are reclaimed once their run has resolved.

### Fixed

- Every atomic write -- the park record, the run journal, and the verify cache
  -- now uses a unique temporary file. A fixed name meant two concurrent writers
  could tear a record, and a corrupt park record could neither be acked nor
  rejected and failed `sig gc` closed for the whole repository. On Windows the
  publishing rename retries briefly when another handle holds the destination.

- Test-only: `sig gc`'s sweep root is injectable, so tests no longer sweep the
  machine-wide temporary directory and delete the working state of a concurrent
  test binary.


## [1.1.0] - 2026-07-24

The faster-and-more-reliable milestone: the run pipeline's serial phases go
parallel (worktree creation, sparse checkouts, bisect probes, a persistent
blob reader), crashes and leftovers become recoverable (`sig gc`, the serve
crash journal, disk preflight), Windows gets a CI-tested build, extended-scale
behavior is documented to 4096 agents with a nightly guard, and installing
`sig` no longer requires a Go toolchain. Interface changes are additive only;
zero module dependencies, and the verify gate is untouched on every landing
path.

### Added

- **`-parallel-agents`** — an explicit cap on concurrently running agents,
  shaped for model-backed agents (rate limits, memory) instead of the
  GOMAXPROCS default that fit CPU-bound work. Serve enforces org quotas by
  clamping it; the effective value is recorded in the report.

- **`-sparse-worktrees`** (opt-in) — when a task declares `files`, its agent
  worktree materializes only that lane plus `go.mod`/`go.sum` on disk while
  the git index stays complete, so commits still produce whole, correct
  trees. Worktree disk drops ~99% on a 500-file repo (the win grows with tree
  size) and setup time drops ~30% further. Lane paths are anchored and
  glob-escaped before `sparse-checkout`; lane enforcement is unchanged;
  no-lane tasks loudly fall back to a full checkout.

- **`sig gc`** — sweeps stale worktrees, temp dirs, and old `agent/*` /
  `imported/*` branches left by crashed or interrupted runs. Dry-run by
  default, age-gated, and refuses to touch anything a live run's manifest
  still references; a corrupt manifest fails closed.

- **Crash-safe `sig serve` run journal** — every run directory now carries a
  `status.json` phase marker (`queued`/`running`/`done`/`error`/`interrupted`,
  plus `pid` and an explanatory note) written atomically at each transition,
  and a `request.json` snapshot of the exact POSTed body, journaled the
  instant `POST /runs` accepts the request — before its goroutine even
  starts. On startup, `sig serve` scans every registered cell's runs
  directory and rewrites any run left `queued`/`running` by a now-dead owning
  process to `interrupted`, so `GET /runs/{id}` and the `/runs` listing
  report reality instead of `running` forever after a `kill -9`. See
  [docs/USAGE.md](docs/USAGE.md) "Crash recovery".

- **Disk-space preflight** — `sig run` estimates the space an N-agent run
  needs on the temp filesystem before starting and warns when it does not
  fit (fail-open: a warning, never a refusal); `sig doctor` reports free
  space on the filesystem runs actually fill.

- **Machine-readable serve errors** — every serve API error body is now
  `{"error": ..., "code": ...}` with a stable code vocabulary, so clients can
  branch on codes instead of parsing prose.

- **Windows support, honestly stated** — a `windows-latest` CI job builds,
  vets, and unit-tests every push; unix-only tests carry explicit, justified
  skips. Windows binaries remain not-yet-battle-tested and the README says
  exactly that.

- **`docs/SCALE.md`** — extended-scale measurements to 4096 agents on two
  platforms with the observed scaling exponent (linear through 4096; no
  super-linear bottleneck yet), plus a nightly 2048-agent correctness-checked
  scale smoke so a scale regression is caught within a day.

- **Install without Go** — the Homebrew tap is live
  (`brew install surya-koritala/tap/sig`) and `install.sh` fetches the right
  prebuilt binary for macOS/Linux and verifies its SHA-256 against the
  release checksums before installing. The only runtime requirement is
  `git` >= 2.38.

### Changed

- **Agent worktree creation is no longer fully serial.** The cell lock now
  covers only the cheap `git worktree add --no-checkout` admin step (branch
  ref + worktree metadata); the expensive file population runs outside the
  lock, in parallel across agents, bounded by `-parallel-agents`. Real-run
  wall time improves ~1.7× at 64–256 agents and worst-case single-worktree
  setup latency stays flat as fan-out widens. Per-agent setup cost is now
  visible as `setupMs` on `agent_done` events plus one `worktree_setup`
  aggregate event per run. Loud-fail semantics on pre-existing branches are
  unchanged, and a worktree whose population fails is torn down rather than
  surviving half-made.

- **`-verify-bisect`'s each-alone candidate probes now run concurrently**
  (bounded at 3), each in its own detached worktree from a serially-created
  pool, cutting red-batch salvage time from k+1 sequential verifies toward
  the longest single verify. The union re-verify — the landing gate — stays
  serial and last, and the salvaged subset is identical to the serial scan's.

- **Blob reads go through one persistent `git cat-file --batch` process per
  cell** instead of spawning a process per operation — the semantic-analysis
  phase drops ~84% (708 ms → 113 ms at 50 agents) and the review UI's
  three-pane endpoint responds without a process spawn per request. Strictly
  fail-open: any daemon error falls back to the old spawn path (a kill
  mid-response can never surface truncated content), teardown is bounded at
  2 s even against a wedged child, and reads honor context cancellation.

- **`POST /runs`'s immediate `202` body now reports `status: "queued"`**
  (previously `"running"`) — it reflects the phase actually recorded at
  accept time, before the run's goroutine has started. The status vocabulary
  gains `queued` and `interrupted` alongside the existing `running`/`done`/
  `error`; this is an additive extension of the lifecycle, not a change to
  any terminal state.

### Fixed

- A flaky serve test that raced its third concurrent run's drain
  (`TestServeConcurrentSameCell409`) and a class of Windows-only test
  failures (8.3 short-name path comparisons, unix-only `/dev/fd` and
  shell-shim assumptions) surfaced by the new Windows CI job.

## [1.0.0] - 2026-07-23

The distributed and service milestone: Sigbound gains its first network daemon
(`sig serve`), multi-machine transport (`sig export`/`sig import`), a first-class
`Cell` as the unit of horizontal scale, per-slot environment scoping, and
symbol-level Go semantic conflict detection — all still on top of plain git, with
zero module dependencies, and with the verify gate holding on every landing path.
`sig serve` runs and lets you inspect work over a repo you already host; it is
not a git host and does not replace your forge.

With 1.0.0 the public interface — the `sig` subcommands and their flags, and the
`SIGBOUND_*` environment variables — is now covered by Semantic Versioning:
breaking changes to it require a 2.0.0.

### Added

- **`sig serve` conflict-review surface** — a read-only, self-contained web UI
  and JSON endpoints for inspecting the branches a run flagged. When a run flags a
  branch (a real conflict a `-resolver` declined, or none was set), it doesn't
  land — a human decides. `GET /runs/{id}/flagged` lists the flagged branches and
  their conflicted paths; `GET /runs/{id}/flagged/{branch}/{path...}` returns the
  three sides of one path (`base` | `ours`, the landed tree | `theirs`, the
  flagged branch), each read as a blob from the object store, with a `null` side
  for an add/delete conflict. The path is validated against the run's own flagged
  set (an allowlist), so the endpoint can only ever read a path that was actually
  flagged — a traversal, an absolute path, or any non-flagged file is `404` and
  reads nothing. `GET /ui` serves a single embedded HTML page (vanilla HTML/CSS/
  JS, no framework, no CDN, no external asset — CSP-safe and works offline on an
  air-gapped daemon) that renders the listing and a three-pane diff; file contents
  render via `textContent` only, so agent-generated code can't inject anything.
  This surface is strictly read-only — it never resolves, merges, or lands from
  the browser; that stays `sig run` / `sig integrate` on the CLI. It composes with
  the existing auth/loopback posture: the data-free `/ui` shell is served
  unauthenticated, while the `/runs` data endpoints it fetches stay gated by the
  token. See [docs/USAGE.md](docs/USAGE.md) "Conflict review UI".
- **`sig serve` quotas and metering** — a managed-layer feature on `serve`
  only (`sig run` stays uncapped), entirely opt-in via server flags (`0` =
  unlimited, byte-identical to before). Quotas are hosted-side ceilings
  enforced at `POST /runs` before a run starts: `-max-agents-per-run`
  rejects an over-cap agent count with `400`; `-max-run-time` caps every
  run's `-budget` via `min(request, server)` — a request can only make its
  own budget stricter, never laxer; `-max-concurrent-runs` rejects with
  `429` once N runs are in flight across ALL cells, on top of the existing
  per-cell `409`. A rejected request starts no run: no run directory, no
  cell slot held. Metering is a per-run usage record, always on and derived
  from data `driveRun`'s report already tracks (agent counts, integrate/
  verify wall time, repair rounds) plus the run's total wall clock `serve`
  itself brackets; written as `usage.json` alongside `report.json` so it
  survives a restart, exposed via `GET /runs/{id}/usage`, embedded in `GET
  /runs/{id}`, and aggregated fleet-wide via `GET /usage`. This is NOT a
  biller: no price, currency, or external metering call — it's the data
  layer a hosted product would meter on. See
  [docs/USAGE.md](docs/USAGE.md) "Quotas and metering".
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
- **`-semantic go`** — an opt-in, Go-only, best-effort symbol-level conflict
  detector. It runs after the agents finish and before integration, parsing
  (stdlib `go/parser`/`go/ast`, no type-checking) each changed `.go` file to
  find branches that declared-changed or referenced the same symbol by name;
  any such pair is unioned into the same partition group so it serializes
  through the normal overlap path instead of landing independently. Fails
  open on any parse or git-read error (that one branch just contributes no
  semantic edges); the default `-semantic off` leaves today's path-only
  partitioning byte-for-byte unchanged. See
  [docs/USAGE.md](docs/USAGE.md) "Semantic conflicts (Go)".

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

[Unreleased]: https://github.com/surya-koritala/sigbound/compare/v2.2.0...HEAD
[2.2.0]: https://github.com/surya-koritala/sigbound/compare/v2.1.0...v2.2.0
[2.1.0]: https://github.com/surya-koritala/sigbound/compare/v2.0.0...v2.1.0
[2.0.0]: https://github.com/surya-koritala/sigbound/compare/v1.1.0...v2.0.0
[1.1.0]: https://github.com/surya-koritala/sigbound/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/surya-koritala/sigbound/compare/v0.3.0...v1.0.0
[0.3.0]: https://github.com/surya-koritala/sigbound/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/surya-koritala/sigbound/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/surya-koritala/sigbound/releases/tag/v0.1.0
