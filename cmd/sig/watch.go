// `sig serve -watch` (issue #111): continuous integration cycles. Serve is
// otherwise request/response — one POST /runs, one run. With -watch, each
// registered cell also gets a loop that observes its designated sources
// (local agent/* arrivals, imported/<worker>/* refs from `sig import`, and
// explicit POST /queue enqueues), batches what has ARRIVED since the last
// cycle, and drives an ordinary run over it on a cadence.
//
// A cycle adds NO landing path. It builds runParams and goes through the same
// acquireRun + execRun + driveRun path a POSTed run does: same per-cell busy
// lock, same crash journal, same policy resolution at the current base, same
// verify gate, same parking, same usage record. The only thing that tells the
// two apart is the manifest's "source": "watch" (see runReport.Source) — a
// cycle run is otherwise indistinguishable, deliberately.
//
// THE ARRIVAL INVARIANT. A cycle integrates branches it did not create, which
// is a sharper problem than it looks: the engine may only integrate a branch
// that CONTAINS the base it is landing onto (see adoptBranch, which enforces
// it, and adoptableAgainst, which decides it). A branch that has fallen behind
// the base cannot land and is not offered again until it is re-pushed; an
// already-landed branch is retired outright. That is what makes a LOST
// watch-seen set safe: every branch re-qualifies, and every already-landed one
// is recognized and retired without a run.
//
// IDEMPOTENCE. watch-seen.json (per cell, alongside the run journal) maps
// branch -> the SHA this daemon last reached a decision about, so an unchanged
// branch is never processed twice and a re-pushed one (new SHA) always
// re-qualifies. It is a CACHE, not a ledger: losing or corrupting it costs
// re-examination — a reset backoff count, a landing parked a second time — and
// never a landing under a bar that did not judge it. That holds because nothing
// UNSAFE to re-examine is kept here: a scheduled fire's own branch, the one kind
// of branch an ordinary cycle must never adopt, is excluded durably from
// intent-fired.json instead (see intentFiredEntry.Branches).
//
// STARVATION. A branch whose cycle keeps failing to land it is retried, and
// after -watch-max-red consecutive such cycles it is excluded and raised as a
// red-branch inbox entry for a human. A re-push clears the count.
//
// SCHEDULED INTENTS (issue #113). The same tick is also the heartbeat for an
// intent's `schedule`: a due tick asks which of the repo's intents are due and
// fires one as an ordinary run (see watchIntentCycle). There is no second
// scheduler and no timer per intent. Intents get the tick FIRST but not EVERY
// tick — a due tick that follows one that fired goes to arrivals — so neither
// side can starve the other however short the schedules are; see watchCycle.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

const (
	// watchSeenFileName is the per-cell seen-set, written beside the run
	// journal under <git-common-dir>/sigbound/.
	watchSeenFileName = "watch-seen.json"
	// redBranchFileName marks the branches a cycle EXCLUDED via backoff. It is
	// written into that cycle's run dir, so GET /inbox surfaces it the same way
	// it surfaces every other attention item: by reading what a run wrote.
	redBranchFileName = "red-branches.json"
	// watchSource is runReport.Source's value for a cycle run.
	watchSource = "watch"
	// watchRefPrefixes are the ref namespaces a cycle observes: agent/* (local
	// arrivals) and imported/*/* (bundles landed by `sig import`). Same two
	// namespaces `sig gc` sweeps.
	watchRefPrefixes = "refs/heads/agent/,refs/heads/imported/"
	// watchPollInterval is how often the loop LOOKS when -watch-batch is set: a
	// batch trigger that only checked once per -watch-interval could not fire
	// early, which is the whole point of it. Without -watch-batch the loop looks
	// exactly once per interval and this is unused.
	watchPollInterval = time.Second
)

// watchConfig is one cell's watch cadence, resolved once at startup from the
// server flags and the cell's sigbound.policy (see resolveWatchConfig).
type watchConfig struct {
	base     string
	interval time.Duration
	batch    int // fire early once this many branches are pending (0 = interval only)
	maxRed   int // exclude a branch after this many consecutive cycles that failed to land it
	// verify is -watch-verify: the verify command every cycle runs, composed
	// with the cell policy's own battery by resolvePolicy exactly as a POSTed
	// request's verify is. A cycle has no requester to supply one, and a loop
	// that lands unattended must not be the one path with no gate — so watch
	// REFUSES to start when neither this nor a policy verify line exists (see
	// resolveWatchConfig). `-watch-verify true` is the explicit way to say that
	// landing unverified is what you meant.
	verify string
	// agent is -watch-agent: the agent command a due SCHEDULED INTENT's run
	// invokes (issue #113). Empty is the ordinary case and costs nothing — a
	// branch cycle runs no agent at all (see watchParams) — so only a cell whose
	// intents declare a schedule needs it, and a due intent with no agent is
	// reported per occurrence rather than refused at startup: intents live in the
	// WORKING TREE and change under a running daemon, so startup is the wrong
	// place to decide whether this cell will ever need one.
	agent string
	// tick replaces the internal ticker when non-nil: each receive is one poll.
	// Tests drive cycles through it so a cadence test never depends on a sleep
	// producing the interleaving it hoped for. With interval 0 every poll is
	// due, which makes "one send = one cycle" exactly true.
	tick <-chan time.Time
}

// poll is how often the loop looks for arrivals: once per interval normally,
// but at watchPollInterval when a batch trigger is armed (it must be able to
// fire before the interval elapses). Never zero — a zero-interval config (what
// tests use to make every poll due) still needs a real ticker period.
func (c watchConfig) poll() time.Duration {
	if c.batch > 0 && (c.interval <= 0 || c.interval > watchPollInterval) {
		return watchPollInterval
	}
	if c.interval <= 0 {
		return watchPollInterval
	}
	return c.interval
}

// watchSeenEntry is what one branch's last decision was, at the SHA it was
// decided at. Any entry whose SHA no longer matches the branch's head is
// ignored outright — that is what makes a re-push re-qualify, and what makes
// Red a count of consecutive cycles over the SAME content rather than a
// permanent mark against a branch name.
type watchSeenEntry struct {
	SHA string `json:"sha"`
	// Done: this SHA reached a decision — it landed, it parked awaiting a human,
	// or it was already in the base. Never offered again at this SHA.
	Done bool `json:"done,omitempty"`
	// Stale: this SHA does not contain the base, so integrating it would revert
	// work (see adoptBranch). Permanent for this SHA — the base only moves
	// forward — so the branch must be rebased and re-pushed to qualify.
	Stale bool `json:"stale,omitempty"`
	// Red counts consecutive cycles that took this SHA into a batch and failed
	// to land it. At -watch-max-red the branch is excluded; see watchCycle.
	Red int `json:"red,omitempty"`
}

// watchSeen is watch-seen.json's shape.
type watchSeen struct {
	Branches map[string]watchSeenEntry `json:"branches"`
}

// readWatchSeen reads a cell's seen-set. EVERY failure — missing, unreadable,
// truncated, malformed, wrong shape — yields an EMPTY set, never an error: the
// seen-set is a cache whose worst-case loss is re-examining branches that have
// already been decided, and the arrival invariant (see adoptBranch) makes that
// re-examination a no-op rather than a re-landing. Failing closed to empty is
// therefore the safe direction, and the only one that keeps a corrupt file from
// wedging a daemon's watch loop forever.
func readWatchSeen(path string) watchSeen {
	empty := watchSeen{Branches: map[string]watchSeenEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	var s watchSeen
	if err := json.Unmarshal(data, &s); err != nil || s.Branches == nil {
		return empty
	}
	for b, e := range s.Branches {
		// A record with no SHA can never match a branch head, so it would sit
		// there forever doing nothing; drop it rather than carry it.
		if b == "" || e.SHA == "" || e.Red < 0 {
			delete(s.Branches, b)
		}
	}
	return s
}

// writeWatchSeen persists the seen-set atomically (write-then-rename, the same
// pattern writeRunStatus and writePark use), so a daemon killed mid-write
// leaves either the old file or the new one — never a torn one that the reader
// above would have to discard. Best-effort: losing a write costs a
// re-examination, so it must never fail a cycle.
func writeWatchSeen(path string, s watchSeen) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
	}
}

// watchSeenPath is this cell's seen-set, beside its runs dir.
func (rc *registeredCell) watchSeenPath() string {
	return filepath.Join(filepath.Dir(rc.runsDir), watchSeenFileName)
}

// redBranchJSON is one excluded branch in a cycle's red-branches.json.
type redBranchJSON struct {
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
	Cycles int    `json:"cycles"`
}

// ---- collection ----

// watchArrival is one branch a cycle may take, with the SHA it was seen at.
type watchArrival struct {
	Branch string
	SHA    string
}

// watchCollect enumerates this cell's sources and splits them into the branches
// that QUALIFY for a cycle at baseSHA and the seen-set updates that classifying
// them produced (retiring an already-landed branch, marking one stale). Those
// updates are returned rather than applied so the caller persists exactly once.
// The third return is every branch name that EXISTS right now, which is what
// lets the caller drop seen-set entries for branches that are gone (nil when the
// enumeration itself failed, so a git error prunes nothing).
//
// A branch qualifies when it is not a scheduled fire's own branch, its head
// differs from what the seen-set decided about (a first sighting, or a re-push),
// it is not excluded by backoff, and it can actually be integrated onto baseSHA.
// An ancestry check that ERRORS leaves the branch alone entirely — unrecorded
// and un-offered — so a transient git failure neither retires a branch nor feeds
// it to a run.
func watchCollect(ctx context.Context, g *gitx.Git, seen watchSeen, baseSHA string, queued []string, cfg watchConfig, ev *eventEmitter, cellID string, fired intentFired) (pending []watchArrival, updates map[string]watchSeenEntry, live map[string]bool) {
	updates = map[string]watchSeenEntry{}
	refs, err := g.ForEachRefCommit(ctx, strings.Split(watchRefPrefixes, ",")...)
	if err != nil {
		ev.emit("watch_error", map[string]any{"cell": cellID, "at": "list-refs", "error": err.Error()})
		return nil, updates, nil
	}
	fireBranches := fired.fireBranches()
	arrivals := make([]watchArrival, 0, len(refs)+len(queued))
	for _, r := range refs {
		arrivals = append(arrivals, watchArrival{Branch: r.Name, SHA: r.SHA})
	}
	// Explicitly enqueued branches (POST /queue) may live outside the watched
	// namespaces, so they are resolved individually. One that has since vanished
	// is simply dropped: the enqueue is a hint, not a promise.
	for _, b := range queued {
		sha, err := g.RevParse(ctx, b)
		if err != nil {
			continue
		}
		arrivals = append(arrivals, watchArrival{Branch: b, SHA: sha})
	}
	sort.Slice(arrivals, func(i, j int) bool { return arrivals[i].Branch < arrivals[j].Branch })

	seenBranch := map[string]bool{}
	for _, a := range arrivals {
		if seenBranch[a.Branch] { // a queued branch that is also under a watched prefix
			continue
		}
		seenBranch[a.Branch] = true
		if fireBranches[a.Branch] {
			// A scheduled fire's own branch (issue #113), named durably by the fire
			// that created it BEFORE that run started. It was judged by that
			// intent's `acceptance`; a cycle's bar is the repo policy plus
			// -watch-verify and does NOT include the acceptance, so adopting it
			// here could land exactly the work the acceptance rejected. Skipped
			// before the ancestry check, and skipped whatever the seen-set says —
			// the seen-set is a cache, and this exclusion has to survive losing it.
			continue
		}
		if e, ok := seen.Branches[a.Branch]; ok && e.SHA == a.SHA {
			if e.Done || e.Stale || (cfg.maxRed > 0 && e.Red >= cfg.maxRed) {
				continue
			}
		}
		verdict, err := adoptableAgainst(ctx, g, baseSHA, a.SHA)
		if err != nil {
			ev.emit("watch_error", map[string]any{"cell": cellID, "at": "classify", "branch": a.Branch, "error": err.Error()})
			continue
		}
		switch verdict {
		case adoptMerged:
			// Already in the base: finished work, not a cycle's business. Retiring
			// it here is what keeps a lost seen-set cheap AND silent.
			updates[a.Branch] = watchSeenEntry{SHA: a.SHA, Done: true}
		case adoptStale:
			// Recorded so the operator is told ONCE per pushed SHA rather than
			// every tick forever.
			if e, ok := seen.Branches[a.Branch]; !ok || e.SHA != a.SHA || !e.Stale {
				ev.emit("watch_stale", map[string]any{"cell": cellID, "branch": a.Branch, "sha": a.SHA, "baseSHA": baseSHA})
			}
			updates[a.Branch] = watchSeenEntry{SHA: a.SHA, Stale: true}
		default:
			pending = append(pending, a)
		}
	}
	return pending, updates, seenBranch
}

// ---- the cycle ----

// watchCycle runs at most one integration cycle for rc. due says the interval
// has elapsed; when it has not, a cycle only fires if -watch-batch is armed and
// that many branches are already pending. Returns whether a run was driven.
//
// It BLOCKS until the run it starts has finished. That is what makes the loop
// serial by construction: there is never a second cycle to queue, because the
// tick that would have started one finds this one still in flight.
func (s *server) watchCycle(ctx context.Context, rc *registeredCell, cfg watchConfig, due bool) bool {
	cellID := rc.cell.ID()
	g := rc.cell.Git()
	// Scheduled intents (issue #113) get the tick FIRST, but not EVERY tick.
	//
	// First, because an intent has a deadline and an arrival does not: a branch
	// that waits one more tick is merely later, while a schedule that keeps
	// losing the cell to a busy arrival stream is a schedule that does not work.
	// Only on a DUE tick, so the check runs once per -watch-interval however fast
	// a batch trigger is polling.
	//
	// Not every tick, because "first" alone is starvation with extra steps in the
	// other direction: an intent scheduled at or under the interval — or a fire
	// that simply runs longer than one — is due again on every tick, and a cycle
	// that returns as soon as it fires would then never classify an arrival at
	// all. So a due tick that FOLLOWS a tick that fired goes to arrivals, which
	// bounds each side's wait at two due ticks and makes a `schedule` honored to
	// within two -watch-intervals rather than one.
	if due {
		fired := s.intentTurn(cellID) && s.watchIntentCycle(ctx, rc, cfg)
		s.noteIntentFire(cellID, fired)
		if fired {
			return true
		}
	}
	seenPath := rc.watchSeenPath()
	seen := readWatchSeen(seenPath)

	baseSHA, err := g.RevParse(ctx, cfg.base)
	if err != nil {
		s.watchEvents.emit("watch_error", map[string]any{"cell": cellID, "at": "resolve-base", "base": cfg.base, "error": err.Error()})
		return false
	}
	pending, updates, live := watchCollect(ctx, g, seen, baseSHA, s.queuedBranches(cellID), cfg, s.watchEvents, cellID, readIntentFired(rc.intentFiredPath()))
	changed := len(updates) > 0
	for b, e := range updates {
		seen.Branches[b] = e
	}
	// An entry for a branch that no longer exists can never match a head again,
	// so it would sit in the file forever: every fire, every deleted branch and
	// every retired arrival would be permanent growth. live is exactly what the
	// enumeration above saw, and is nil when that enumeration failed — a git
	// error must not read as "every branch is gone".
	for b := range seen.Branches {
		if live != nil && !live[b] {
			delete(seen.Branches, b)
			changed = true
		}
	}
	if changed {
		writeWatchSeen(seenPath, seen)
	}
	if len(pending) == 0 {
		return false
	}
	if !due && !(cfg.batch > 0 && len(pending) >= cfg.batch) {
		return false // waiting for either the interval or a full batch
	}

	// Quota, per cycle: -max-agents-per-run is a REJECT for a POST (its caller
	// asked for a run this server will not do), but rejecting a CYCLE would mean
	// a server whose quota is below its arrival rate lands nothing, ever, while
	// counting every branch toward exclusion. A cycle SPLITS instead: it takes
	// the first N in branch-name order (deterministic, so every branch is
	// eventually reached) and DEFERS the rest to the next cycle.
	//
	// Deferral has a consequence worth naming: this cycle is about to move the
	// base, so a deferred branch will no longer contain it and must be rebased
	// before it can land (it is reported as such — see watchCollect's stale
	// case). That is not a quirk of the quota, it is the arrival invariant every
	// branch lives under — anything that arrives while a cycle is landing is in
	// the same position. Splitting merely makes it visible sooner.
	//
	// The repo policy's own max-agents keeps its reject semantics untouched: it
	// is resolved inside driveRun, the one place policy is resolved, and an
	// over-count there fails the cycle run exactly as it fails a POSTed one.
	split := 0
	if s.maxAgentsPerRun > 0 && len(pending) > s.maxAgentsPerRun {
		split = len(pending) - s.maxAgentsPerRun
		pending = pending[:s.maxAgentsPerRun]
	}

	tasks := make([]taskSpec, 0, len(pending))
	adopt := make(map[string]string, len(pending))
	branches := make([]string, 0, len(pending))
	for _, a := range pending {
		// The branch name IS the task id: unique by construction, and it makes
		// perAgent[].id in the report name the thing that actually arrived.
		tasks = append(tasks, taskSpec{ID: a.Branch})
		adopt[a.Branch] = a.Branch
		branches = append(branches, a.Branch)
	}

	p := s.watchParams(rc, cfg, adopt)
	req := runRequest{Cell: cellID, Base: cfg.base, Tasks: tasks}
	rec, err := s.acquireRun(rc, req, &p)
	if err != nil {
		// A manual POST /runs (or an ack) holds the cell. Skip this tick entirely
		// rather than queue a second cycle: the branches are still pending, and
		// the next tick picks them up unchanged.
		s.watchEvents.emit("watch_skip", map[string]any{"cell": cellID, "pending": len(branches), "reason": err.Error()})
		return false
	}
	s.watchEvents.emit("watch_tick", map[string]any{
		"cell": cellID, "runId": rec.id, "baseSHA": baseSHA, "branches": branches, "deferred": split,
	})
	s.execRun(rec, p, tasks, planSpec{}, false)

	s.watchSettle(ctx, rc, cfg, rec, pending, seenPath)
	return true
}

// ---- scheduled intents (issue #113) ----

// intentFiredPath is this cell's last-fired record, beside its seen-set.
func (rc *registeredCell) intentFiredPath() string {
	return filepath.Join(filepath.Dir(rc.runsDir), intentFiredFileName)
}

// watchIntentCycle fires at most ONE due scheduled intent as an ordinary run,
// and reports whether it did. It is the whole recurring runtime: there is no
// second scheduler, no timer per intent and no queue — the watch tick is the
// heartbeat, and every tick simply asks which intents are due.
//
// A fire is an ordinary run in every way a cycle over arrivals is (same
// acquireRun slot, same journal, same policy resolution, same verify gate, same
// parking) plus the two things `sig run -intent` gives an intent run: the goal
// as the task prompt with the intent's files as its lane, and the intent's
// acceptance APPENDED to the verify battery — tighten-only, never replacing a
// policy member.
//
// WHY THE TASK ID IS STAMPED. A task id becomes the branch agent/<id>, and a
// worktree add refuses to reuse an existing branch (loudly, by design — see
// runAgent). A recurring intent runs under the same id forever, so a plain
// intent id would collide with the branch its own previous fire left behind and
// every fire after the first would die at worktree add. The stamp is newRunID's
// UTC-timestamp-plus-random, so each fire gets its own branch and the intent id
// still reaches `sig log` where it belongs: runParams.Intent.
//
// EXACTLY-ONCE, AND ITS LIMIT. Two concurrent ticks cannot both fire one intent:
// acquireRun takes the cell's single run slot, so the loser is refused before it
// records anything, and the winner stamps the fired record BEFORE execRun. That
// ordering is also why a crash mid-run does not re-fire on restart — the record
// is written when the work STARTS. The limit, stated because it is real: the run
// slot is per-PROCESS, so this holds within one daemon, exactly as the per-cell
// busy lock does. Two daemons watching one repo is outside this design (which is
// why newServer refuses two cells over one git directory).
//
// THE SAME WRITE NAMES THE BRANCH. The stamp carries agent/<task id>, the branch
// this fire is about to create, because that is the only moment at which the
// name is known AND nothing has run yet: watchCollect refuses to adopt a branch
// the record names, so a fire that crashes, or whose seen-set is later lost,
// still cannot have its work re-judged under the cycle's bar instead of the
// intent's acceptance.
func (s *server) watchIntentCycle(ctx context.Context, rc *registeredCell, cfg watchConfig) bool {
	cellID := rc.cell.ID()
	g := rc.cell.Git()
	// The working tree, like every other intent read (see intent.go): an intent
	// is input, not a gate. A malformed intents/ dir is reported and skipped —
	// the daemon is not the validator of files it does not own, and `sig intent
	// list` is where that error belongs.
	intents, err := listIntents(rc.cell.Repo())
	if err != nil {
		s.watchEvents.emit("watch_error", map[string]any{"cell": cellID, "at": "intents", "error": err.Error()})
		return false
	}
	// An id that cannot be a branch component is an intent that can never fire:
	// the stamp would be written and the worktree add would then fail, so `sig
	// intent show` would claim it fired while nothing ever ran. Refused BEFORE
	// anything is recorded, and taken out of the running entirely rather than
	// merely skipped — an intent that is permanently due would otherwise starve
	// every intent behind it (see dueIntent).
	fireable := make([]intent, 0, len(intents))
	for _, it := range intents {
		if it.Schedule > 0 && !refComponentSafe(it.ID) {
			s.watchEvents.emit("watch_error", map[string]any{"cell": cellID, "at": "intent-id", "intent": it.ID,
				"error": fmt.Sprintf("this intent has a schedule but its id cannot be a git branch component (a fire runs on agent/%s-<stamp>): rename %s", it.ID, intentPath(rc.cell.Repo(), it.ID))})
			continue
		}
		fireable = append(fireable, it)
	}
	firedPath := rc.intentFiredPath()
	fired := readIntentFired(firedPath)
	now := time.Now()
	it, ok := dueIntent(fireable, fired, now)
	if !ok {
		return false
	}
	// A last-fire in the future is not a fire (see effectiveFiredAt): it is
	// treated as never-fired, which is the documented fail-open direction, and
	// said out loud here because the alternative is a schedule that appears to
	// have stopped. The fire below overwrites the stamp with a real one, so this
	// is not a per-tick alarm.
	if last := fired.Intents[it.ID].FiredAt; !last.IsZero() && effectiveFiredAt(last, now).IsZero() {
		s.watchEvents.emit("watch_error", map[string]any{"cell": cellID, "at": "intent-fired-future", "intent": it.ID,
			"error": fmt.Sprintf("last fire recorded at %s, which is ahead of now (%s): ignoring it and firing, and this fire re-stamps the record",
				last.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))})
	}
	if strings.TrimSpace(cfg.agent) == "" {
		s.watchEvents.emit("watch_error", map[string]any{"cell": cellID, "at": "intent-agent", "intent": it.ID,
			"error": "this intent has a schedule but the daemon has no agent to run it with: start `sig serve -watch` with -watch-agent"})
		return false
	}

	p := s.watchParams(rc, cfg, nil)
	p.AgentCmd = cfg.agent
	p.Intent = it.ID
	// Same composition `sig run -intent` performs, for the same reason: the
	// acceptance command is a member of the battery, not a replacement for one.
	// resolvePolicy then composes the repo's own battery over this inside
	// driveRun, where policy is resolved for every run.
	if acc := strings.TrimSpace(it.Acceptance); acc != "" {
		battery := []string{acc}
		if strings.TrimSpace(p.VerifyCmd) != "" {
			battery = append(battery, p.VerifyCmd)
		}
		p.VerifyCmd = joinVerifyBattery(battery)
	}
	tasks := []taskSpec{{ID: it.ID + "-" + newRunID(), Prompt: it.Goal, Files: it.Files}}
	req := runRequest{Cell: cellID, Base: cfg.base, Tasks: tasks}
	rec, err := s.acquireRun(rc, req, &p)
	if err != nil {
		// A manual POST /runs (or an ack) holds the cell, or a quota is full.
		// Nothing is recorded, so the intent stays due and the next tick fires it.
		s.watchEvents.emit("watch_skip", map[string]any{"cell": cellID, "intent": it.ID, "reason": err.Error()})
		return false
	}
	// Recorded the instant the slot is taken, before any agent runs — the stamp
	// AND the branch this fire will produce, in one atomic write, because the
	// exclusion is worthless if it can be missing while the branch exists. A
	// write failure here is loud: the fire happened, and a fire this daemon could
	// not record is one the next tick will repeat with nothing keeping this run's
	// branch out of an ordinary cycle.
	branch := "agent/" + tasks[0].ID
	fired.Intents[it.ID] = intentFiredEntry{FiredAt: now, Branches: append(liveBranches(ctx, g, fired.Intents[it.ID].Branches), branch)}
	if werr := writeIntentFired(firedPath, fired); werr != nil {
		s.watchEvents.emit("watch_error", map[string]any{"cell": cellID, "at": "intent-fired", "intent": it.ID,
			"error": fmt.Sprintf("%s: %v (this fire may repeat on the next tick, and %s is not excluded from an ordinary cycle)", firedPath, werr, branch)})
	}
	s.watchEvents.emit("watch_intent", map[string]any{
		"cell": cellID, "runId": rec.id, "intent": it.ID, "schedule": it.Schedule.String(), "task": tasks[0].ID,
	})
	s.execRun(rec, p, tasks, planSpec{}, false)
	s.retireIntentBranch(ctx, rc, rec, branch)
	// No backoff and no retry: a fire that failed to land is an ordinary red run,
	// its report and inbox entry say so, and the intent tries again at its next
	// window. The schedule IS the retry cadence.
	return true
}

// liveBranches drops the names that no longer resolve. It is what keeps the
// fired record's branch list bounded rather than a log of every fire this repo
// has ever had: a landed fire deletes its own branch (see retireIntentBranch),
// so the next fire forgets it, and what survives is exactly the fire branches
// still on disk — the red ones, and any a human or `sig gc` has not swept.
//
// It prunes off ONE enumeration of the watched namespaces rather than a probe
// per name, because the two failure directions are not equal: keeping a name
// that is gone excludes a branch that does not exist (harmless), while dropping
// a name that is NOT gone offers a red fire's work back to an ordinary cycle. A
// per-name rev-parse cannot tell "no such branch" from a cancelled context or a
// transient git failure; an enumeration that fails is one answer for all of
// them, and that answer is "keep everything".
func liveBranches(ctx context.Context, g *gitx.Git, branches []string) []string {
	if len(branches) == 0 {
		return nil
	}
	refs, err := g.ForEachRefCommit(ctx, strings.Split(watchRefPrefixes, ",")...)
	if err != nil {
		return branches
	}
	live := make(map[string]bool, len(refs))
	for _, r := range refs {
		live[r.Name] = true
	}
	var out []string
	for _, b := range branches {
		if live[b] {
			out = append(out, b)
		}
	}
	return out
}

// retireIntentBranch closes out the branch a fire produced. The part that
// MATTERS already happened before the run started — the branch is named in
// intent-fired.json, which watchCollect refuses to adopt — so this is only the
// two things that can be decided once the run is over:
//
//   - A fire that LANDED cleanly has its branch deleted, the same way the run
//     that landed it makes it redundant: its content is the base now. That is
//     also what bounds the fired record, since the next fire drops names that no
//     longer resolve. A fire that did NOT land keeps its branch — work is never
//     destroyed here, and `sig gc` is the one thing that sweeps refs.
//   - Anything else is recorded Done in the seen-set: a cache on top of the
//     durable exclusion, so an ordinary cycle does not spend an ancestry check
//     per tick on a branch it will refuse anyway.
func (s *server) retireIntentBranch(ctx context.Context, rc *registeredCell, rec *runRecord, branch string) {
	// WithoutCancel: a shutdown that cut the fire short must still get here — the
	// durable record already holds, but leaving the branch AND a stale seen entry
	// behind on every drain is growth nothing later cleans up.
	ctx = context.WithoutCancel(ctx)
	if rep, err := readRunReport(rec.dir); err == nil && landed(rep) && hasString(rep.Integrate.Landed, branch) {
		if err := rc.cell.DeleteBranch(ctx, branch); err == nil {
			return
		}
		// A delete that failed is not an error worth failing a fire over: the
		// branch is still excluded durably, and the seen entry below is written as
		// if it had not been attempted.
	}
	sha, err := rc.cell.Git().RevParse(ctx, branch)
	if err != nil {
		return // the agent created no branch: nothing to record
	}
	seenPath := rc.watchSeenPath()
	seen := readWatchSeen(seenPath)
	seen.Branches[branch] = watchSeenEntry{SHA: sha, Done: true}
	writeWatchSeen(seenPath, seen)
}

// watchSettle records what the finished cycle decided about each branch it took.
// Landed and parked branches are DONE at that SHA (a park is a decision pending
// with a human, not a failure — re-offering it would park the same landing
// twice). Anything else is a red cycle for that branch, and the count crossing
// -watch-max-red excludes it and raises a red-branch inbox entry.
//
// A cycle the daemon ABORTED (shutdown cancelling baseCtx mid-run) records no
// red counts at all: the branches were never judged, and holding a shutdown
// against them would walk them toward exclusion for an operational reason.
func (s *server) watchSettle(ctx context.Context, rc *registeredCell, cfg watchConfig, rec *runRecord, batch []watchArrival, seenPath string) {
	seen := readWatchSeen(seenPath)
	defer func() { writeWatchSeen(seenPath, seen) }()

	if ctx.Err() != nil {
		s.watchEvents.emit("watch_drain", map[string]any{"cell": rc.cell.ID(), "runId": rec.id, "branches": len(batch)})
		return
	}
	done := map[string]bool{}
	if rep, err := readRunReport(rec.dir); err == nil {
		// integrate.landed names the branches that FOLDED cleanly, which is not
		// the same as landed: the base ref only advances after verify goes green
		// (that is the whole gate). Use the same predicate `sig log` does, so a
		// red cycle's branches stay pending and keep counting toward backoff
		// instead of being retired as decided.
		if landed(rep) {
			for _, b := range rep.Integrate.Landed {
				done[b] = true
			}
		}
		if rep.Park != nil {
			for _, b := range rep.Park.branches() {
				done[b] = true
			}
		}
	}
	var excluded []redBranchJSON
	for _, a := range batch {
		if done[a.Branch] {
			seen.Branches[a.Branch] = watchSeenEntry{SHA: a.SHA, Done: true}
			continue
		}
		red := 1
		if e, ok := seen.Branches[a.Branch]; ok && e.SHA == a.SHA {
			red = e.Red + 1
		}
		seen.Branches[a.Branch] = watchSeenEntry{SHA: a.SHA, Red: red}
		s.watchEvents.emit("watch_backoff", map[string]any{"cell": rc.cell.ID(), "branch": a.Branch, "red": red, "max": cfg.maxRed})
		if cfg.maxRed > 0 && red >= cfg.maxRed {
			excluded = append(excluded, redBranchJSON{Branch: a.Branch, SHA: a.SHA, Cycles: red})
		}
	}
	if len(excluded) > 0 {
		writeRedBranches(rec.dir, excluded)
	}
}

// writeRedBranches records the branches this cycle excluded, for GET /inbox.
// Best-effort like the other run-dir writers: the seen-set already holds the
// exclusion itself, so a lost marker costs the inbox entry, never the behavior.
func writeRedBranches(dir string, red []redBranchJSON) {
	data, err := json.MarshalIndent(map[string]any{"branches": red}, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, redBranchFileName), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "sig serve: write red-branch marker %s: %v\n", dir, err)
	}
}

// readRedBranches reads a run dir's red-branch marker; nil when there is none.
func readRedBranches(dir string) []redBranchJSON {
	data, err := os.ReadFile(filepath.Join(dir, redBranchFileName))
	if err != nil {
		return nil
	}
	var f struct {
		Branches []redBranchJSON `json:"branches"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil
	}
	return f.Branches
}

// watchParams builds a cycle's runParams. Everything a POSTed run gets from the
// SERVER (environment policy, the quota clamps) is applied identically here;
// everything a POSTed run gets from its REQUEST has no analogue in a cycle —
// there is no caller to ask — so those stay at their defaults and the repo's
// sigbound.policy, resolved inside driveRun like always, is what actually sets
// this cell's bar. Notably AgentCmd stays EMPTY: a cycle runs no agents, and
// adoptBranch never falls back to running one.
func (s *server) watchParams(rc *registeredCell, cfg watchConfig, adopt map[string]string) runParams {
	p := runParams{
		Repo:          rc.cell.Repo(),
		Base:          cfg.base,
		Strategy:      cell.StrategyOverlay,
		Semantic:      semanticOff,
		LaneMode:      laneWarn,
		VerifyCmd:     cfg.verify,
		Autocommit:    true,
		AdoptBranches: adopt,
		Source:        watchSource,
		EnvMode:       s.envMode,
		EnvAgent:      s.envAgent,
		EnvResolver:   s.envResolver,
		EnvVerify:     s.envVerify,
		EnvRepair:     s.envRepair,
		EnvPublish:    s.envPublish,
		RepairMax:     2,
	}
	if s.maxRunTime > 0 {
		p.Budget = s.maxRunTime
	}
	if s.maxParallelAgents > 0 {
		p.ParallelAgents = s.maxParallelAgents
	}
	return p
}

// ---- the loop ----

// watchLoop is one cell's ticker. It stops at the first tick after ctx is
// cancelled; a cycle already in flight is not interrupted here (it runs
// synchronously, and driveRun observes the same cancelled ctx), so shutdown
// DRAINS it and its seen-set write happens before this returns.
func (s *server) watchLoop(ctx context.Context, rc *registeredCell, cfg watchConfig) {
	defer s.wg.Done()
	tick := cfg.tick
	if tick == nil {
		t := time.NewTicker(cfg.poll())
		defer t.Stop()
		tick = t.C
	}
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		// Re-checked after the tick: a tick that arrived at the same moment as the
		// shutdown must not start a cycle whose ctx is already dead.
		if ctx.Err() != nil {
			return
		}
		last = s.watchStep(ctx, rc, cfg, last)
	}
}

// watchStep is one poll: it decides whether this poll is DUE (a -watch-interval
// has elapsed since the last due one), drives the cycle, and returns the time
// the next poll measures against.
//
// The reset depends on DUE-NESS ALONE, never on what the cycle did. A due poll
// that found nothing pending has still spent this interval, and treating it as
// if the clock had never started is what made "once per -watch-interval however
// fast a batch trigger is polling" false: after one such poll, every
// watchPollInterval poll was due, so an intent's schedule was checked 30x more
// often than the interval says and a red branch's backoff was counted in polls.
// A non-due (batch-triggered) cycle deliberately does NOT reset it — an early
// batch is extra work, not a replacement for the interval's own cycle.
//
// Split out of watchLoop because a cadence is exactly the thing a test must be
// able to drive without waiting for one: the caller owns `last`.
func (s *server) watchStep(ctx context.Context, rc *registeredCell, cfg watchConfig, last time.Time) time.Time {
	due := time.Since(last) >= cfg.interval
	s.watchCycle(ctx, rc, cfg, due)
	if due {
		return time.Now()
	}
	return last
}

// startWatch launches one loop per registered cell, resolving each cell's
// cadence against its own sigbound.policy first. Every failure here is a
// STARTUP error: a watch daemon that silently fell back to a default cadence
// the repo did not ask for would be worse than one that refused to start.
func (s *server) startWatch(ctx context.Context, cfg watchConfig, explicit map[string]bool) error {
	if len(s.cells) == 0 {
		return errors.New("-watch: no cells registered; -repos must name at least one repository to watch")
	}
	if cfg.interval < 0 {
		return fmt.Errorf("-watch-interval must not be negative, got %s", cfg.interval)
	}
	if cfg.batch < 0 {
		return fmt.Errorf("-watch-batch must not be negative, got %d", cfg.batch)
	}
	if cfg.maxRed < 1 {
		return fmt.Errorf("-watch-max-red must be at least 1, got %d", cfg.maxRed)
	}
	resolved := make([]watchConfig, len(s.cells))
	for i, rc := range s.cells {
		c, err := resolveWatchConfig(ctx, rc.cell.Git(), cfg, explicit)
		if err != nil {
			return fmt.Errorf("cell %s: %w", rc.cell.ID(), err)
		}
		resolved[i] = c
	}
	for i, rc := range s.cells {
		s.wg.Add(1)
		go s.watchLoop(ctx, rc, resolved[i])
	}
	return nil
}

// resolveWatchConfig applies the cell's sigbound.policy cadence keys over the
// server flags. Precedence is #108's, in the shape a cadence allows: the POLICY
// sets the value when it declares one, and the flags are DEFAULTS beneath it.
// Tightening cannot be defined here the way it can for a landing bar — a shorter
// interval is not a stricter one, it is merely a different one — so a flag that
// was set EXPLICITLY against a policy that also declares that key is a loud
// error naming both sources, rather than a silent win for either.
//
// Read ONCE, at startup, from the policy at the cell's watch base: a cadence
// that changed under a running loop would make the daemon's own behavior
// unexplainable from its logs. A policy edit takes effect on restart.
func resolveWatchConfig(ctx context.Context, g *gitx.Git, cfg watchConfig, explicit map[string]bool) (watchConfig, error) {
	// The watch base must resolve NOW: every cycle pins it, and a daemon that
	// only discovered a misspelled -watch-base on its first tick would report the
	// failure to a log nobody is reading rather than to whoever started it.
	if _, err := g.RevParse(ctx, cfg.base); err != nil {
		return cfg, fmt.Errorf("-watch-base %q does not resolve: %w", cfg.base, err)
	}
	pol, err := loadPolicy(ctx, g, cfg.base)
	if err != nil {
		return cfg, err
	}
	// The one thing a cycle cannot be allowed to do quietly: land unattended
	// work that nothing checked. A POSTed run gets its verify from its caller
	// and `sig run` from its flags; a cycle has neither, so the verify must come
	// from the repo's policy or from -watch-verify, and a cell with neither is a
	// startup refusal rather than a loop that lands whatever arrives.
	if len(pol.verify) == 0 && strings.TrimSpace(cfg.verify) == "" {
		return cfg, fmt.Errorf("-watch: no verify command for this cell — add a verify line to %s at %s, or pass -watch-verify (use `-watch-verify true` to accept landing every cycle unverified)",
			policyFileName, cfg.base)
	}
	conflict := func(flagName, polKey, polVal, flagVal string) error {
		return fmt.Errorf("policy %s: %s=%s; flag -%s=%s — the policy sets the watch cadence, so remove the flag (or the policy line)",
			policyFileName, polKey, polVal, flagName, flagVal)
	}
	if pol.watchInterval > 0 {
		if explicit["watch-interval"] {
			return cfg, conflict("watch-interval", "watch-interval", pol.watchInterval.String(), cfg.interval.String())
		}
		cfg.interval = pol.watchInterval
	}
	if pol.watchBatch > 0 {
		if explicit["watch-batch"] {
			return cfg, conflict("watch-batch", "watch-batch", fmt.Sprint(pol.watchBatch), fmt.Sprint(cfg.batch))
		}
		cfg.batch = pol.watchBatch
	}
	if pol.watchMaxRed > 0 {
		if explicit["watch-max-red"] {
			return cfg, conflict("watch-max-red", "watch-max-red", fmt.Sprint(pol.watchMaxRed), fmt.Sprint(cfg.maxRed))
		}
		cfg.maxRed = pol.watchMaxRed
	}
	return cfg, nil
}

// ---- POST /queue ----

// queueRequest is POST /queue's body: name branches for a cell's next cycle
// that its watched namespaces would not pick up on their own.
type queueRequest struct {
	Cell     string   `json:"cell"`
	Branches []string `json:"branches"`
}

// intentTurn reports whether this due tick may look at intents at all: it may
// unless the PREVIOUS due tick fired one. noteIntentFire records the answer.
// Together they are the whole of the bounded fairness watchCycle documents —
// one bool per cell, so it is deterministic (no clock, no counter to skew) and a
// test can drive it by driving ticks.
func (s *server) intentTurn(cellID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.intentTick[cellID]
}

func (s *server) noteIntentFire(cellID string, fired bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intentTick[cellID] = fired
}

// queuedBranches drains nothing — it returns a snapshot of what is enqueued for
// a cell. Entries stay until a cycle DECIDES them (the seen-set is what stops
// them being offered again), so an enqueue survives a cycle that skipped.
func (s *server) queuedBranches(cellID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queued[cellID]...)
}

// handleQueue serves POST /queue. Every named branch must ALREADY resolve in
// that cell — an enqueue is a pointer at existing work, never a request to
// create any — so an unknown ref is a 400 naming it rather than an entry that
// silently never fires.
//
// ponytail: the queue is in memory only. A restart drops it, which costs
// nothing for the agent/* and imported/* namespaces (they are re-enumerated
// from refs every cycle) and costs an operator a re-POST for anything outside
// them. Persist it beside the seen-set if that ever bites.
func (s *server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json", codeUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, serveMaxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req queueRequest
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), codeBadRequest)
		return
	}
	if !s.watchOn {
		writeErr(w, http.StatusBadRequest, "this server is not watching: start it with -watch to enqueue branches for a cycle", codeWatchDisabled)
		return
	}
	rc := s.resolveCell(req.Cell)
	if rc == nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown cell %q; known cells: %s", req.Cell, strings.Join(s.cellKeys(), ", ")), codeCellNotFound)
		return
	}
	if len(req.Branches) == 0 {
		writeErr(w, http.StatusBadRequest, "branches is required (at least one existing branch name)", codeBadRequest)
		return
	}
	for _, b := range req.Branches {
		if strings.TrimSpace(b) == "" {
			writeErr(w, http.StatusBadRequest, "branches contains an empty name", codeBadRequest)
			return
		}
		if _, err := rc.cell.Git().RevParse(r.Context(), b); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("branch %q does not exist in cell %s", b, rc.cell.ID()), codeBadRequest)
			return
		}
	}

	cellID := rc.cell.ID()
	s.mu.Lock()
	have := map[string]bool{}
	for _, b := range s.queued[cellID] {
		have[b] = true
	}
	for _, b := range req.Branches {
		if !have[b] {
			have[b] = true
			s.queued[cellID] = append(s.queued[cellID], b)
		}
	}
	total := len(s.queued[cellID])
	s.mu.Unlock()

	writeJSON(w, http.StatusAccepted, map[string]any{"cell": cellID, "queued": total})
}
