package main

// Recurring intents and intent templates (issue #113). The runtime half lives on
// `sig serve -watch`'s existing cycle, so these drive watchCycle directly through
// the watch fixture — the test IS the cadence, and no test here waits on a clock
// to produce an interleaving it needs.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- due-ness (pure) ----

func TestIntentDueSemantics(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	daily := intent{ID: "d", Goal: "g", Schedule: 24 * time.Hour}
	for _, tc := range []struct {
		name string
		it   intent
		last time.Time
		want bool
	}{
		{"no schedule is never due", intent{ID: "x", Goal: "g"}, time.Time{}, false},
		{"no schedule stays not due however long ago it ran", intent{ID: "x", Goal: "g"}, now.Add(-99 * time.Hour), false},
		{"never fired is due now", daily, time.Time{}, true},
		{"inside the window is not due", daily, now.Add(-23 * time.Hour), false},
		{"exactly one window is due", daily, now.Add(-24 * time.Hour), true},
		{"past the window is due", daily, now.Add(-25 * time.Hour), true},
		{"ten missed windows is still just due", daily, now.Add(-240 * time.Hour), true},
		{"a fire re-bases the window", daily, now, false},
	} {
		if got := intentDue(tc.it, tc.last, now); got != tc.want {
			t.Errorf("%s: intentDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDueIntentPicksOneInPriorityOrder: a tick fires ONE intent, and it is the
// one `sig intent list` puts first — priority desc, then id asc.
func TestDueIntentPicksOneInPriorityOrder(t *testing.T) {
	now := time.Now()
	intents := []intent{
		{ID: "high", Goal: "g", Schedule: time.Hour, Priority: 9},
		{ID: "abc", Goal: "g", Schedule: time.Hour},
		{ID: "unscheduled", Goal: "g", Priority: 99},
	}
	got, ok := dueIntent(intents, intentFired{Intents: map[string]intentFiredEntry{}}, now)
	if !ok || got.ID != "high" {
		t.Fatalf("dueIntent = %q, %v; want the highest-priority due intent", got.ID, ok)
	}
	// With the leader already fired inside its window, the next due one is taken
	// — and an intent with no schedule is never taken at all.
	fired := intentFired{Intents: map[string]intentFiredEntry{"high": {FiredAt: now}}}
	got, ok = dueIntent(intents, fired, now)
	if !ok || got.ID != "abc" {
		t.Fatalf("dueIntent = %q, %v; want abc", got.ID, ok)
	}
	fired.Intents["abc"] = intentFiredEntry{FiredAt: now}
	if got, ok := dueIntent(intents, fired, now); ok {
		t.Fatalf("dueIntent = %q; an intent with no schedule must never be due", got.ID)
	}
}

// ---- the durable record ----

func TestIntentFiredRecordRoundTripAndDegradation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", intentFiredFileName)
	if got := readIntentFired(path); len(got.Intents) != 0 {
		t.Fatalf("missing file read as %+v, want an empty record", got)
	}
	if got := readIntentFired(""); len(got.Intents) != 0 {
		t.Fatalf("empty path read as %+v, want an empty record", got)
	}
	fired := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	rec := intentFired{Intents: map[string]intentFiredEntry{"deps": {FiredAt: fired}}}
	if err := writeIntentFired(path, rec); err != nil {
		t.Fatalf("writeIntentFired: %v", err)
	}
	if got := readIntentFired(path).Intents["deps"].FiredAt; !got.Equal(fired) {
		t.Fatalf("round trip = %s, want %s", got, fired)
	}
	// No temp file is left beside it: a dot-file in .git/sigbound is one more
	// thing every reader has to know to ignore.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != intentFiredFileName {
		t.Fatalf("directory holds %d entries, want only %s", len(entries), intentFiredFileName)
	}
	// Corrupt, and entries that can only mean "never fired", degrade to empty
	// rather than wedging the loop. The cost is documented: one extra fire.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readIntentFired(path); len(got.Intents) != 0 {
		t.Fatalf("corrupt file read as %+v, want an empty record", got)
	}
	if err := os.WriteFile(path, []byte(`{"intents":{"a":{"firedAt":"0001-01-01T00:00:00Z"},"":{"firedAt":"2026-07-25T09:00:00Z"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readIntentFired(path); len(got.Intents) != 0 {
		t.Fatalf("degenerate entries survived as %+v", got)
	}
	// But an unusable fire time must NOT take the branch names with it. The two
	// fields fail open in opposite directions: forgetting WHEN it fired costs
	// one extra fire, while forgetting WHICH branch it produced puts that branch
	// back in front of an ordinary cycle, to be judged without the intent's
	// acceptance. Dropping the entry wholesale would do the second to do the
	// first.
	if err := os.WriteFile(path, []byte(`{"intents":{"a":{"firedAt":"0001-01-01T00:00:00Z","branches":["agent/a-1"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readIntentFired(path)
	if !got.fireBranches()["agent/a-1"] {
		t.Fatalf("an entry with no usable fire time dropped its branches: %+v — that branch is now adoptable", got)
	}
	if !got.Intents["a"].FiredAt.IsZero() {
		t.Fatalf("the unusable fire time survived as %v, want it to read as never fired", got.Intents["a"].FiredAt)
	}
}

// ---- the fixture's scheduled-intent helpers ----

// scheduledAgent writes ONE file named after the task id, so every fire of a
// recurring intent produces a genuinely new tree (and the landed tree names the
// fire that produced it). runAgent sets SIGBOUND_TASK_ID for every agent.
const scheduledAgent = `printf 'fired\n' > "fire-$SIGBOUND_TASK_ID.txt"`

// scheduleIntent writes a scheduled intent into the fixture's repo working tree.
func (f *watchFixture) scheduleIntent(id, body string) {
	f.t.Helper()
	writeIntent(f.t, f.repo, id, body)
}

// firedRecord reads this cell's durable last-fired record.
func (f *watchFixture) firedRecord() intentFired {
	f.t.Helper()
	return readIntentFired(f.rc.intentFiredPath())
}

// setFired backdates an intent's last fire, which is how a test reaches a state
// that would otherwise need a real day to arrive at. It touches the TIME only:
// the branches the entry records are the fire's own bookkeeping, and a helper
// that quietly cleared them would hide exactly what the tests below check.
func (f *watchFixture) setFired(id string, at time.Time) {
	f.t.Helper()
	rec := f.firedRecord()
	e := rec.Intents[id]
	e.FiredAt = at
	rec.Intents[id] = e
	if err := writeIntentFired(f.rc.intentFiredPath(), rec); err != nil {
		f.t.Fatal(err)
	}
}

// ---- AC: a scheduled intent fires on the watch loop and lands like any run ----

func TestWatchFiresScheduledIntentAndLands(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	marker := filepath.Join(t.TempDir(), "acceptance-ran")
	f.scheduleIntent("deps-current", "goal = update dependencies\nacceptance = printf ok > "+marker+"\nschedule = 24h\n")
	f.cfg.agent = scheduledAgent
	base0 := f.mainSHA()

	if !f.cycle() {
		t.Fatal("a due scheduled intent did not fire")
	}
	rep := f.lastReport()
	if rep.Intent != "deps-current" {
		t.Fatalf("report intent = %q, want the intent id (this is what `sig log` attributes the landing to)", rep.Intent)
	}
	if rep.Source != watchSource {
		t.Fatalf("report source = %q, want %q", rep.Source, watchSource)
	}
	if len(rep.Integrate.Landed) != 1 || !landed(rep) {
		t.Fatalf("scheduled fire did not land: integrate=%+v verify=%+v", rep.Integrate, rep.Verify)
	}
	if f.mainSHA() == base0 {
		t.Fatal("the base did not move: a fire must land like any other run")
	}
	// The intent's own lane/prompt reached the run as ONE task, under a stamped
	// id (see watchIntentCycle) whose prefix is still the intent.
	if len(rep.Tasks) != 1 || !strings.HasPrefix(rep.Tasks[0].ID, "deps-current-") {
		t.Fatalf("tasks = %+v, want one task stamped from the intent id", rep.Tasks)
	}
	// acceptance composed INTO the battery rather than replacing the policy's:
	// both members are in the effective verify, and the acceptance really ran.
	if !strings.Contains(rep.VerifyCmd, marker) || !strings.Contains(rep.VerifyCmd, "true") {
		t.Fatalf("effective verify = %q, want the intent's acceptance AND the policy's verify", rep.VerifyCmd)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the intent's acceptance command never ran: %v", err)
	}
	ev := f.ev.lastEvent("watch_intent")
	if ev == nil || ev["intent"] != "deps-current" || ev["schedule"] != "24h0m0s" {
		t.Fatalf("watch_intent event = %+v", ev)
	}
	// And it is recorded durably, at the moment it fired.
	if f.firedRecord().Intents["deps-current"].FiredAt.IsZero() {
		t.Fatal("the fire was not recorded: the next tick would fire it again")
	}
	// The window has been re-based, so the very next tick does not re-fire.
	if f.cycle() {
		t.Fatal("the intent fired twice inside one window")
	}
	if n := f.ev.countEvent("watch_intent"); n != 1 {
		t.Fatalf("watch_intent events = %d, want 1", n)
	}
}

// TestWatchScheduledIntentSurvivesRestart is the durability statement: a fresh
// daemon over the same repo reads the same record and does NOT re-fire — and
// deleting the record is what brings the intent back, which also proves the
// record (not some other effect of the first cycle) is what suppressed it.
func TestWatchScheduledIntentSurvivesRestart(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("deps-current", "goal = update dependencies\nschedule = 24h\n")
	f.cfg.agent = scheduledAgent

	if !f.cycle() {
		t.Fatal("first fire did not happen")
	}
	runs := len(f.runDirs())

	// "Restart": a second server process over the same repo, with its own
	// in-memory state and no knowledge of the first beyond what is on disk.
	restarted := newWatchFixtureAt(t, f.g, f.repo)
	restarted.cfg = f.cfg
	for i := 0; i < 3; i++ {
		if restarted.cycle() {
			t.Fatal("a restarted daemon re-fired an intent that is inside its window")
		}
	}
	if n := len(f.runDirs()); n != runs {
		t.Fatalf("runs %d -> %d across a restart: something re-fired", runs, n)
	}
	if n := restarted.ev.countEvent("watch_intent"); n != 0 {
		t.Fatalf("restarted daemon emitted %d watch_intent events, want 0", n)
	}

	// Lose the record (the documented degradation) and it fires exactly once
	// more — and that fire LANDS, which is the second thing being pinned here:
	// the branch the first fire left behind must not collide with the second's.
	if err := os.Remove(restarted.rc.intentFiredPath()); err != nil {
		t.Fatal(err)
	}
	if !restarted.cycle() {
		t.Fatal("with the record gone, an unknown last-fire must read as never fired")
	}
	rep := restarted.lastReport()
	if len(rep.Integrate.Landed) != 1 || !landed(rep) {
		t.Fatalf("the second fire did not land: perAgent=%+v integrate=%+v", rep.PerAgent, rep.Integrate)
	}
}

// TestWatchScheduledIntentConcurrentTicksFireOnce forces the interleaving rather
// than hoping for it: the first fire is pinned mid-run by a gate its agent
// blocks on, so the second tick provably happens WHILE the first is in flight.
// Exactly one run exists, and exactly one fire is recorded — the record is
// stamped before the run starts, so the second tick sees an intent inside its
// window rather than a race.
func TestWatchScheduledIntentConcurrentTicksFireOnce(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("deps-current", "goal = update dependencies\nschedule = 24h\n")
	g := newGate(t)
	f.cfg.agent = g.shell(scheduledAgent)

	var wg sync.WaitGroup
	wg.Add(1)
	var first bool
	go func() {
		defer wg.Done()
		first = f.s.watchCycle(f.ctx, f.rc, f.cfg, true)
	}()
	waitFor(t, "the first fire's agent to start", g.started)

	// The fire above is provably in flight; this tick must not start a second.
	if f.s.watchCycle(f.ctx, f.rc, f.cfg, true) {
		t.Fatal("a concurrent tick fired the same intent a second time")
	}
	g.release(t)
	wg.Wait()
	if !first {
		t.Fatal("the first tick did not fire")
	}
	if n := len(f.runDirs()); n != 1 {
		t.Fatalf("%d runs, want exactly 1 for one due intent", n)
	}
	if n := f.ev.countEvent("watch_intent"); n != 1 {
		t.Fatalf("watch_intent events = %d, want 1", n)
	}
	if len(f.firedRecord().Intents) != 1 {
		t.Fatalf("fired record = %+v, want exactly one entry", f.firedRecord().Intents)
	}
}

// TestWatchScheduledIntentYieldsToABusyCell covers the other half of "fires
// once": the tight window BEFORE the record is stamped, where two ticks have
// both decided an intent is due. The cell's single run slot is what serializes
// them, so this forces exactly that state — the slot taken, the way a manual
// POST /runs or an ack takes it — instead of trying to hit the window. Nothing
// is recorded, so the intent is still due when the slot frees.
func TestWatchScheduledIntentYieldsToABusyCell(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("deps-current", "goal = update dependencies\nschedule = 24h\n")
	f.cfg.agent = scheduledAgent

	cellID := f.rc.cell.ID()
	f.s.mu.Lock()
	f.s.busy[cellID] = true
	f.s.mu.Unlock()

	if f.cycle() {
		t.Fatal("an intent fired while the cell's run slot was held")
	}
	if n := len(f.runDirs()); n != 0 {
		t.Fatalf("%d run dirs on a skipped tick, want 0", n)
	}
	if len(f.firedRecord().Intents) != 0 {
		t.Fatalf("fired record = %+v, want nothing recorded for a fire that never happened", f.firedRecord().Intents)
	}
	ev := f.ev.lastEvent("watch_skip")
	if ev == nil || ev["intent"] != "deps-current" {
		t.Fatalf("watch_skip = %+v, want one naming the intent it left due", ev)
	}

	f.s.mu.Lock()
	f.s.busy[cellID] = false
	f.s.mu.Unlock()
	if !f.cycle() {
		t.Fatal("the intent did not fire once the slot was free")
	}
	if n := f.ev.countEvent("watch_intent"); n != 1 {
		t.Fatalf("watch_intent events = %d, want 1", n)
	}
}

// TestWatchScheduledIntentMissedWindowsFireOnce: a daemon that was down for ten
// windows fires ONCE on return, not once per window missed.
func TestWatchScheduledIntentMissedWindowsFireOnce(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("deps-current", "goal = update dependencies\nschedule = 1h\n")
	f.cfg.agent = scheduledAgent
	f.setFired("deps-current", time.Now().Add(-10*time.Hour)) // ten windows missed

	if !f.cycle() {
		t.Fatal("an overdue intent did not fire on return")
	}
	for i := 0; i < 3; i++ {
		if f.cycle() {
			t.Fatal("a missed window fired more than once: the fire must re-base the window on NOW, not walk the backlog")
		}
	}
	if n := f.ev.countEvent("watch_intent"); n != 1 {
		t.Fatalf("watch_intent events = %d after 10 missed windows, want 1", n)
	}
	if got := f.firedRecord().Intents["deps-current"].FiredAt; time.Since(got) > time.Hour {
		t.Fatalf("last fired = %s: the record must be stamped with the fire, not with the window it was owed", got)
	}
}

// TestWatchScheduledIntentAcceptanceGatesTheLanding: a red acceptance fails the
// fire's verify like any other battery member — nothing lands — and the fire is
// still recorded, so the INTENT waits for its next window instead of re-firing
// every tick.
//
// It also pins what happens to the branch a red fire leaves behind: it is
// recorded as DECIDED, so no later cycle re-judges it under the cycle bar (the
// repo policy plus -watch-verify), which does not include this intent's
// acceptance. Without that, the very work the acceptance rejected would land on
// the next tick.
func TestWatchScheduledIntentAcceptanceGatesTheLanding(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("deps-current", "goal = update dependencies\nacceptance = false\nschedule = 24h\n")
	f.cfg.agent = scheduledAgent
	base0 := f.mainSHA()

	if !f.cycle() {
		t.Fatal("the intent did not fire")
	}
	rep := f.lastReport()
	if rep.Verify.OK {
		t.Fatal("verify passed with a red acceptance member")
	}
	if len(rep.Integrate.Landed) == 0 {
		t.Fatal("the agent's work never folded, so this proves nothing about the gate")
	}
	if f.mainSHA() != base0 {
		t.Fatal("the base moved past a red verify")
	}
	fired := f.firedRecord().Intents["deps-current"].FiredAt
	if fired.IsZero() {
		t.Fatal("a red fire must still be recorded, or it repeats every tick")
	}
	// The branch it left behind is retired, so no later tick lands it under a bar
	// that does not include the acceptance — durably in the fired record (which is
	// what survives losing the seen-set: see TestWatchRedFireDoesNotLandAfterSeenSetLoss)
	// and as a cache in the seen-set. And the intent does not fire again inside its
	// window either. The branch itself SURVIVES: its work never landed.
	branch := "agent/" + rep.Tasks[0].ID
	if got := f.firedRecord().Intents["deps-current"].Branches; len(got) != 1 || got[0] != branch {
		t.Fatalf("fired record branches = %v, want [%s] recorded by the fire itself", got, branch)
	}
	if e := f.seen().Branches[branch]; !e.Done {
		t.Fatalf("seen[%s] = %+v, want it decided by the fire that produced it", branch, e)
	}
	if _, err := f.g.RevParse(context.Background(), branch); err != nil {
		t.Fatalf("a red fire's branch was deleted (%v): nothing here may destroy work that did not land", err)
	}
	for i := 0; i < 2; i++ {
		if f.cycle() {
			t.Fatal("a later cycle re-judged the branch a red fire left behind")
		}
	}
	if n := f.ev.countEvent("watch_intent"); n != 1 {
		t.Fatalf("watch_intent events = %d, want 1: a red fire must not retry before its next window", n)
	}
	if got := f.firedRecord().Intents["deps-current"].FiredAt; !got.Equal(fired) {
		t.Fatalf("last fired moved from %s to %s without a second fire", fired, got)
	}
	if f.mainSHA() != base0 {
		t.Fatal("the base moved: work the intent's acceptance rejected must not land by another path")
	}
}

// TestWatchScheduledIntentNeedsAnAgent: a schedule with no -watch-agent is
// reported per occurrence and fires nothing — and nothing is recorded, so the
// intent is still due the moment an agent is configured.
func TestWatchScheduledIntentNeedsAnAgent(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("deps-current", "goal = update dependencies\nschedule = 24h\n")

	if f.cycle() {
		t.Fatal("an intent fired with no agent command to run it with")
	}
	ev := f.ev.lastEvent("watch_error")
	if ev == nil || ev["at"] != "intent-agent" || !strings.Contains(ev["error"].(string), "-watch-agent") {
		t.Fatalf("watch_error = %+v, want one naming -watch-agent", ev)
	}
	if len(f.firedRecord().Intents) != 0 {
		t.Fatalf("fired record = %+v, want nothing recorded for a fire that never happened", f.firedRecord().Intents)
	}
	f.cfg.agent = scheduledAgent
	if !f.cycle() {
		t.Fatal("the intent must fire once an agent is configured")
	}
}

// TestWatchMalformedIntentsDoNotStopTheBranchCycle: the daemon is not the
// validator of files it does not own. A broken intents/ dir is reported and the
// cycle goes on to integrate arrivals — `sig intent list` is where that error
// belongs.
func TestWatchMalformedIntentsDoNotStopTheBranchCycle(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("broken", "goal = g\nnope = 1\n")
	f.arrive("agent/a", "a.txt", "a\n")

	if !f.cycle() {
		t.Fatal("a malformed intent file stopped an ordinary branch cycle")
	}
	if got := f.lastReport().Integrate.Landed; len(got) != 1 || got[0] != "agent/a" {
		t.Fatalf("landed %v, want the arrival", got)
	}
	ev := f.ev.lastEvent("watch_error")
	if ev == nil || ev["at"] != "intents" {
		t.Fatalf("watch_error = %+v, want one naming the intents read", ev)
	}
}

// TestWatchUnscheduledIntentNeverFires: `schedule` is what makes an intent
// recurring. Without it an intent is a standing ask for whoever runs it, and a
// watch loop must never start work nobody asked it to start.
func TestWatchUnscheduledIntentNeverFires(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("manual", "goal = only when asked\n")
	f.cfg.agent = scheduledAgent

	for i := 0; i < 3; i++ {
		if f.cycle() {
			t.Fatal("an intent with no schedule fired on the watch loop")
		}
	}
	if n := len(f.runDirs()); n != 0 {
		t.Fatalf("%d runs for an unscheduled intent, want 0", n)
	}
}

// ---- fire retirement is durable, not cached ----

// TestWatchRedFireDoesNotLandAfterSeenSetLoss is the safety statement the whole
// acceptance rule rests on. A red fire's branch is adoptable work that one bar
// REJECTED; a cycle's bar is the repo policy plus -watch-verify and does not
// include that acceptance. So if the only thing keeping that branch out of a
// cycle were the seen-set — a documented CACHE that degrades to empty on any
// read failure — then deleting or corrupting one file would land exactly the
// work the acceptance rejected. The exclusion is therefore recorded in
// intent-fired.json by the fire itself, and this pins that losing the cache
// changes nothing about what lands.
func TestWatchRedFireDoesNotLandAfterSeenSetLoss(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	f := newWatchFixture(t, "true") // the CYCLE bar: green on anything
	f.scheduleIntent("deps-current", "goal = update dependencies\nacceptance = false\nschedule = 24h\n")
	f.cfg.agent = scheduledAgent
	base0 := f.mainSHA()

	if !f.cycle() {
		t.Fatal("the intent did not fire")
	}
	rep := f.lastReport()
	branch := "agent/" + rep.Tasks[0].ID
	if len(rep.Integrate.Landed) == 0 {
		t.Fatal("the agent's work never folded, so this proves nothing about the gate")
	}
	if f.mainSHA() != base0 {
		t.Fatal("the base moved past a red acceptance")
	}
	if _, err := f.g.RevParse(ctx, branch); err != nil {
		t.Fatalf("the red fire left no branch to re-offer (%v): the attack this guards has no target", err)
	}
	runsAfterFire := len(f.runDirs())

	for _, loss := range []struct {
		name  string
		write func(path string)
	}{
		{"deleted", func(p string) { os.Remove(p) }},                                    //nolint:errcheck // the point is that it is gone
		{"corrupt", func(p string) { os.WriteFile(p, []byte("\x00 not json"), 0o644) }}, //nolint:errcheck // best-effort
		{"wrong shape", func(p string) { os.WriteFile(p, []byte(`[1,2,3]`), 0o644) }},   //nolint:errcheck // best-effort
		{"empty object", func(p string) { os.WriteFile(p, []byte(`{}`), 0o644) }},       //nolint:errcheck // best-effort
	} {
		t.Run(loss.name, func(t *testing.T) {
			loss.write(f.rc.watchSeenPath())
			// The intent is inside its window, so this tick has no fire to make: what
			// happens next is the ORDINARY arrival path meeting the fire's branch.
			if f.cycle() {
				t.Error("a cycle ran over the branch a red fire left behind")
			}
			if f.mainSHA() != base0 {
				t.Fatalf("the base moved to %s: work the intent's acceptance rejected landed under the cycle's bar",
					short(f.mainSHA()))
			}
			if n := len(f.runDirs()); n != runsAfterFire {
				t.Fatalf("runs = %d, want %d: no run may be driven over a fire's own branch", n, runsAfterFire)
			}
		})
	}
}

// TestWatchFireBranchExcludedAfterACrashMidRun covers the window the seen-set
// could never have covered at all: the fire's branch exists, the run never
// finished, so nothing was ever written about it AFTER the run. The record
// written BEFORE the run is what holds — and the negative control (the same
// branch, the same seen-set, the record simply not naming it) proves that record
// is what stopped the landing rather than some other property of the branch.
func TestWatchFireBranchExcludedAfterACrashMidRun(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("deps-current", "goal = update dependencies\nschedule = 24h\n")
	f.cfg.agent = scheduledAgent

	// Exactly what a daemon killed mid-fire leaves on disk.
	branch := "agent/deps-current-20260726T000000Z-4f2a"
	f.arrive(branch, "half-done.txt", "the agent got this far\n")
	firedAt := time.Now()
	if err := writeIntentFired(f.rc.intentFiredPath(), intentFired{Intents: map[string]intentFiredEntry{
		"deps-current": {FiredAt: firedAt, Branches: []string{branch}},
	}}); err != nil {
		t.Fatal(err)
	}
	if got := readWatchSeen(f.rc.watchSeenPath()).Branches; len(got) != 0 {
		t.Fatalf("seen-set = %+v, want the empty set a run that never finished leaves", got)
	}
	base0 := f.mainSHA()

	if f.cycle() {
		t.Error("a cycle adopted the branch of a fire that never finished")
	}
	if f.mainSHA() != base0 {
		t.Fatal("the base moved: a crashed fire's work landed without its intent's acceptance ever passing")
	}

	// Negative control: drop the branch from the record and it is an ordinary
	// arrival again, which lands. The exclusion is the only difference.
	if err := writeIntentFired(f.rc.intentFiredPath(), intentFired{Intents: map[string]intentFiredEntry{
		"deps-current": {FiredAt: firedAt},
	}}); err != nil {
		t.Fatal(err)
	}
	if !f.cycle() {
		t.Fatal("with the branch un-recorded it must be an ordinary arrival")
	}
	if f.mainSHA() == base0 {
		t.Fatal("the control did not land, so the case above proves nothing")
	}
}

// TestWatchLandedFireBranchIsDeleted: a fire that landed cleanly deletes its own
// branch, which is what keeps both the fired record and the ref namespace from
// growing by one entry per fire forever. A fire that did NOT land keeps its
// branch and its record entry — that work exists nowhere else.
func TestWatchLandedFireBranchIsDeleted(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	f := newWatchFixture(t, "true")
	f.scheduleIntent("deps-current", "goal = update dependencies\nschedule = 24h\n")
	f.cfg.agent = scheduledAgent

	var branches []string
	for i := 0; i < 10 && f.ev.countEvent("watch_intent") < 3; i++ {
		f.setFired("deps-current", time.Now().Add(-48*time.Hour)) // due again
		if f.cycle() {
			branches = append(branches, "agent/"+f.lastReport().Tasks[0].ID)
		}
	}
	if n := len(branches); n != 3 {
		t.Fatalf("%d fires landed, want 3", n)
	}
	for _, b := range branches {
		if _, err := f.g.RevParse(ctx, b); err == nil {
			t.Fatalf("%s still exists: a landed fire's branch is redundant with the base and must not accumulate", b)
		}
	}
	// The record names only the newest fire, and stops naming even that one as
	// soon as a later fire notices it is gone.
	if got := f.firedRecord().Intents["deps-current"].Branches; len(got) != 1 || got[0] != branches[2] {
		t.Fatalf("fired record branches = %v after 3 landed fires, want only the newest (%s)", got, branches[2])
	}
	if got := f.seen().Branches; len(got) != 0 {
		t.Fatalf("seen-set = %+v after 3 landed fires, want no entries for branches that no longer exist", got)
	}
}

// ---- fairness: intents get the tick first, never every tick ----

// TestWatchScheduledIntentDoesNotStarveArrivals is the bound. An intent
// scheduled at or under the interval (or a fire that outlasts one) is due on
// EVERY tick, so a cycle that returned as soon as it fired would never classify
// an arrival again — the branch stream would simply stop being watched.
func TestWatchScheduledIntentDoesNotStarveArrivals(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("always", "goal = keep the cell busy\nschedule = 1ns\n") // due on every tick
	f.cfg.agent = scheduledAgent

	if !f.cycle() {
		t.Fatal("the due intent did not fire on the first tick")
	}
	// The arrival forks from the base that fire just landed, the way a real one
	// pushed after a landing does.
	f.arrive("agent/real", "real.txt", "arrived\n")

	if !f.cycle() {
		t.Fatal("the tick after a fire did not run a cycle over arrivals: a perpetually-due intent owns the loop")
	}
	rep := f.lastReport()
	if len(rep.Integrate.Landed) != 1 || rep.Integrate.Landed[0] != "agent/real" {
		t.Fatalf("second due tick landed %v, want the arrival", rep.Integrate.Landed)
	}
	if e := f.seen().Branches["agent/real"]; !e.Done {
		t.Fatalf("seen[agent/real] = %+v: the arrival was never classified", e)
	}
	if n := f.ev.countEvent("watch_intent"); n != 1 {
		t.Fatalf("watch_intent events = %d after two ticks, want 1: the arrivals tick fired an intent too", n)
	}
	// The alternation is not one-sided: the intent gets the tick after that.
	if !f.cycle() {
		t.Fatal("the intent never got another tick")
	}
	if n := f.ev.countEvent("watch_intent"); n != 2 {
		t.Fatalf("watch_intent events = %d, want 2: arrivals now own the loop instead", n)
	}
}

// TestDueIntentRotatesLeastRecentlyFiredFirst: with several intents due at once,
// the one that has waited longest goes first. Fixed priority order starved
// everything behind a short-scheduled leader.
func TestDueIntentRotatesLeastRecentlyFiredFirst(t *testing.T) {
	now := time.Now()
	intents := []intent{ // listIntents order: priority desc, then id asc
		{ID: "high", Goal: "g", Schedule: time.Hour, Priority: 9},
		{ID: "low", Goal: "g", Schedule: time.Hour},
	}
	fired := intentFired{Intents: map[string]intentFiredEntry{
		"high": {FiredAt: now.Add(-2 * time.Hour)},
		"low":  {FiredAt: now.Add(-10 * time.Hour)},
	}}
	if got, ok := dueIntent(intents, fired, now); !ok || got.ID != "low" {
		t.Fatalf("dueIntent = %q, %v; want the intent that has waited longest", got.ID, ok)
	}
	// Firing re-bases its window, so the other is now the oldest.
	fired.Intents["low"] = intentFiredEntry{FiredAt: now}
	if got, ok := dueIntent(intents, fired, now); !ok || got.ID != "high" {
		t.Fatalf("dueIntent = %q, %v; want high", got.ID, ok)
	}
}

// ---- the due gate ----

// TestWatchNonDueTickNeverFiresAnIntent pins the `due` gate itself. A non-due
// tick is what a batch trigger produces (watchPollInterval, once a second): it
// exists to look at ARRIVALS early, and a `schedule` is honored to the interval,
// not to the poll — without the gate every batch poll would fire intents.
func TestWatchNonDueTickNeverFiresAnIntent(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("deps-current", "goal = update dependencies\nschedule = 24h\n")
	f.cfg.agent = scheduledAgent

	for i := 0; i < 3; i++ {
		if f.s.watchCycle(f.ctx, f.rc, f.cfg, false) {
			t.Fatal("a non-due tick drove a cycle")
		}
	}
	if n := f.ev.countEvent("watch_intent"); n != 0 {
		t.Fatalf("watch_intent events = %d on non-due ticks, want 0", n)
	}
	if len(f.firedRecord().Intents) != 0 {
		t.Fatalf("fired record = %+v, want nothing recorded by a non-due tick", f.firedRecord().Intents)
	}
	if n := len(f.runDirs()); n != 0 {
		t.Fatalf("%d run dirs from non-due ticks, want 0", n)
	}
	// Positive control: the same state, one due tick.
	if !f.cycle() {
		t.Fatal("the intent did not fire on a due tick, so the case above proves nothing")
	}
}

// ---- a fired record from the future ----

// TestWatchFutureFiredAtIsIgnoredAndReported: a stamp far ahead of now cannot be
// a fire this daemon made (a stepped clock, a record from a machine an hour or a
// year out), and believing it is a schedule that silently stops for that long.
// It reads as never-fired — the same fail-open direction losing the record has —
// and is said out loud rather than swallowed.
func TestWatchFutureFiredAtIsIgnoredAndReported(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("deps-current", "goal = update dependencies\nschedule = 24h\n")
	f.cfg.agent = scheduledAgent
	f.setFired("deps-current", time.Now().Add(365*24*time.Hour))

	if !f.cycle() {
		t.Fatal("a last-fire a year in the future wedged the schedule for a year")
	}
	ev := f.ev.lastEvent("watch_error")
	if ev == nil || ev["at"] != "intent-fired-future" || ev["intent"] != "deps-current" {
		t.Fatalf("watch_error = %+v, want one naming the future stamp it ignored", ev)
	}
	// The fire overwrote it with a real time, so this is not a per-tick alarm and
	// the intent is back inside an ordinary window.
	if got := f.firedRecord().Intents["deps-current"].FiredAt; time.Until(got) > time.Minute {
		t.Fatalf("last fired = %s: the fire must re-stamp the record with its own time", got)
	}

	// Negative control: a stamp INSIDE the skew allowance is believed. A clock a
	// few minutes out must not turn every scheduled intent into a permanent
	// re-fire, which is the other way to be wrong here.
	f.setFired("deps-current", time.Now().Add(intentFiredSkew/2))
	errors := f.ev.countEvent("watch_error")
	f.cycle() // arrivals' turn after a fire; drive the intent turn as well
	if f.cycle() {
		t.Fatal("a stamp inside the skew allowance was treated as never fired")
	}
	if n := f.ev.countEvent("watch_error"); n != errors {
		t.Fatalf("watch_error events %d -> %d: a believable stamp must not be reported", errors, n)
	}
}

// TestIntentShowFlagsAFutureFiredAt: the same record, from the CLI. `sig intent
// show` says the intent is due; without the flag, "due" next to a lastFired a
// year out is unexplainable.
func TestIntentShowFlagsAFutureFiredAt(t *testing.T) {
	_, repo := makeGoRepo(t)
	writeIntent(t, repo, "deps-current", "goal = update dependencies\nschedule = 24h\n")
	future := time.Now().Add(365 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := writeIntentFired(intentFiredPathFor(context.Background(), repo),
		intentFired{Intents: map[string]intentFiredEntry{"deps-current": {FiredAt: future}}}); err != nil {
		t.Fatal(err)
	}

	var asJSON, text bytes.Buffer
	if code, err := runIntent(&asJSON, []string{"show", "deps-current", "-repo", repo, "-json"}); err != nil || code != exitOK {
		t.Fatalf("intent show -json: code=%d err=%v", code, err)
	}
	var one intentJSON
	if err := json.Unmarshal(asJSON.Bytes(), &one); err != nil {
		t.Fatalf("parse show JSON: %v\n%s", err, asJSON.String())
	}
	if !one.FiredInFuture || !one.Due || one.LastFired != future.Format(time.RFC3339) {
		t.Fatalf("show = %+v, want due, flagged, and still reporting the stamp it ignored", one)
	}
	if code, err := runIntent(&text, []string{"show", "deps-current", "-repo", repo}); err != nil || code != exitOK {
		t.Fatalf("intent show: code=%d err=%v", code, err)
	}
	if !strings.Contains(text.String(), "in the FUTURE") {
		t.Fatalf("show text = %q, want it to say why the stamp is ignored", text.String())
	}

	// Negative control: a stamp inside the allowance is neither flagged nor due.
	recent := time.Now().Add(intentFiredSkew / 2).UTC().Truncate(time.Second)
	if err := writeIntentFired(intentFiredPathFor(context.Background(), repo),
		intentFired{Intents: map[string]intentFiredEntry{"deps-current": {FiredAt: recent}}}); err != nil {
		t.Fatal(err)
	}
	asJSON.Reset()
	if code, err := runIntent(&asJSON, []string{"show", "deps-current", "-repo", repo, "-json"}); err != nil || code != exitOK {
		t.Fatalf("intent show -json: code=%d err=%v", code, err)
	}
	one = intentJSON{}
	if err := json.Unmarshal(asJSON.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if one.FiredInFuture || one.Due {
		t.Fatalf("show = %+v, want an ordinary in-window intent", one)
	}
}

// ---- an id that is slug-safe but cannot be a branch ----

// TestWatchRefusesAnIntentIdThatCannotBeABranch: "a..b" is a legal filename and
// a legal slug, and `agent/a..b-<stamp>` is not a legal ref. Stamping the fire
// and THEN failing at worktree add would leave `sig intent show` claiming a fire
// that never happened, once per window, forever.
func TestWatchRefusesAnIntentIdThatCannotBeABranch(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.scheduleIntent("a..b", "goal = update dependencies\nschedule = 24h\n")
	f.cfg.agent = scheduledAgent

	if f.cycle() {
		t.Fatal("an intent whose id cannot be a git branch component fired")
	}
	ev := f.ev.lastEvent("watch_error")
	if ev == nil || ev["at"] != "intent-id" || ev["intent"] != "a..b" {
		t.Fatalf("watch_error = %+v, want one naming the unusable id", ev)
	}
	if len(f.firedRecord().Intents) != 0 {
		t.Fatalf("fired record = %+v: a fire that cannot happen must not be stamped", f.firedRecord().Intents)
	}
	if n := len(f.runDirs()); n != 0 {
		t.Fatalf("%d run dirs, want 0", n)
	}

	// Positive control, and the starvation half: an intent that CAN fire is not
	// held up by the one that cannot, however permanently due that one is.
	f.scheduleIntent("ok-id", "goal = update dependencies\nschedule = 24h\n")
	if !f.cycle() {
		t.Fatal("a valid intent never fired while an unusable one was due")
	}
	if got := f.lastReport().Intent; got != "ok-id" {
		t.Fatalf("fired intent = %q, want ok-id", got)
	}
}

// TestIntentNewRefusesAnIdThatCannotBeABranch: creation is the friendlier place
// to say so — in front of whoever typed it, instead of on an unattended tick.
func TestIntentNewRefusesAnIdThatCannotBeABranch(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(intentTemplateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentTemplateDir(repo), "ok"+intentFileExt), []byte("goal = g\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// A leading "-" is deliberately NOT in this list: `sig intent new -dash` is
	// eaten by flag parsing long before any id check, so asserting on it here
	// would pass whether or not the check exists. refComponentSafe covers it.
	for _, id := range []string{"a..b", "...", ".lead", "trail.", "x.lock"} {
		out.Reset()
		if _, err := runIntent(&out, []string{"new", id, "-repo", repo, "-template", "ok"}); err == nil {
			t.Errorf("intent id %q was accepted; it cannot be a git branch component", id)
		}
		if _, err := os.Stat(intentPath(repo, id)); err == nil {
			t.Errorf("a refused id left %s behind", intentPath(repo, id))
		}
	}
	// Negative control: the ordinary shapes still work.
	for _, id := range []string{"deps-current", "issue-113", "a.b", "_x"} {
		out.Reset()
		if code, err := runIntent(&out, []string{"new", id, "-repo", repo, "-template", "ok"}); err != nil || code != exitOK {
			t.Errorf("intent id %q refused: code=%d err=%v", id, code, err)
		}
	}
}

func TestRefComponentSafe(t *testing.T) {
	for _, s := range []string{"a..b", "...", ".lead", "trail.", "x.lock", "-dash", "", ".", ".."} {
		if refComponentSafe(s) {
			t.Errorf("refComponentSafe(%q) = true, want false", s)
		}
	}
	for _, s := range []string{"deps-current", "issue-113", "a.b", "_x", "A1", "x.locked"} {
		if !refComponentSafe(s) {
			t.Errorf("refComponentSafe(%q) = false, want true", s)
		}
	}
}

// ---- templates ----

func TestIntentNewFromTemplate(t *testing.T) {
	repo := t.TempDir()
	tplDir := intentTemplateDir(repo)
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# a maintenance skeleton\ngoal = update dependencies\nacceptance = go build ./... && go test ./...\nschedule = 168h\n"
	if err := os.WriteFile(filepath.Join(tplDir, "deps-current"+intentFileExt), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code, err := runIntent(&out, []string{"new", "weekly-deps", "-repo", repo, "-template", "deps-current"}); err != nil || code != exitOK {
		t.Fatalf("intent new: code=%d err=%v", code, err)
	}
	got, err := os.ReadFile(intentPath(repo, "weekly-deps"))
	if err != nil {
		t.Fatalf("instantiated intent: %v", err)
	}
	if string(got) != body {
		t.Fatalf("instantiation is not a byte-for-byte copy:\n%s", got)
	}
	// It is a real intent under its NEW id, and the template it came from is not
	// one: listIntents never recurses, so intents/templates/ stays invisible.
	it, err := loadIntent(repo, "weekly-deps")
	if err != nil {
		t.Fatalf("loadIntent: %v", err)
	}
	if it.Schedule != 168*time.Hour || it.Acceptance == "" {
		t.Fatalf("parsed = %+v", it)
	}
	all, err := listIntents(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != "weekly-deps" {
		t.Fatalf("listIntents = %+v, want only the instantiated intent", all)
	}

	// Never clobbers: the same instantiation twice leaves the first file exactly
	// as it is, the way a re-import does.
	if err := os.WriteFile(intentPath(repo, "weekly-deps"), []byte("goal = edited by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	code, err := runIntent(&out, []string{"new", "weekly-deps", "-repo", repo, "-template", "deps-current"})
	if err == nil || code == exitOK {
		t.Fatal("instantiating over an existing intent must fail")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %q", err)
	}
	if b, _ := os.ReadFile(intentPath(repo, "weekly-deps")); string(b) != "goal = edited by hand\n" {
		t.Fatalf("the hand edit was overwritten: %s", b)
	}
}

func TestIntentNewFailsClosed(t *testing.T) {
	repo := t.TempDir()
	tplDir := intentTemplateDir(repo)
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A template that would not parse under the new id is refused BEFORE
	// anything is written: a file this binary cannot read back is never left
	// behind (the same pre-write validation the GitHub import does).
	if err := os.WriteFile(filepath.Join(tplDir, "broken"+intentFileExt), []byte("goal = g\nnope = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "ok"+intentFileExt), []byte("goal = g\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := runIntent(&out, []string{"new", "x", "-repo", repo, "-template", "broken"}); err == nil {
		t.Fatal("a template that does not parse must be refused")
	}
	if _, err := os.Stat(intentPath(repo, "x")); err == nil {
		t.Fatal("a refused instantiation left a file behind")
	}
	// A missing template names the ones that DO exist — templates have no
	// listing command of their own.
	_, err := runIntent(&out, []string{"new", "x", "-repo", repo, "-template", "nosuch"})
	if err == nil {
		t.Fatal("an unknown template must fail")
	}
	for _, want := range []string{intentTemplatePath(repo, "nosuch"), "available:", "ok", "broken"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	// An unsafe id is refused for the same reason loadIntent refuses one: it
	// becomes a task id and a branch component.
	if _, err := runIntent(&out, []string{"new", "../escape", "-repo", repo, "-template", "ok"}); err == nil {
		t.Fatal("an unsafe intent id must be refused")
	}
	if _, err := runIntent(&out, []string{"new", "x", "-repo", repo}); err == nil {
		t.Fatal("-template is required")
	}
	if _, err := runIntent(&out, []string{"new", "-repo", repo, "-template", "ok"}); err == nil {
		t.Fatal("an ID is required")
	}
	// Flags-before-positional works too, the way `sig intent show` accepts both.
	if code, err := runIntent(&out, []string{"new", "-repo", repo, "-template", "ok", "flagsfirst"}); err != nil || code != exitOK {
		t.Fatalf("flags-first form: code=%d err=%v", code, err)
	}
}

// ---- `sig intent show` / `list` report next-due ----

func TestIntentShowReportsNextDue(t *testing.T) {
	requirePOSIXShell(t) // makeGoRepo: the fired record lives under .git
	_, repo := makeGoRepo(t)
	writeIntent(t, repo, "deps-current", "goal = update dependencies\nschedule = 24h\n")
	writeIntent(t, repo, "manual", "goal = no schedule here\n")

	show := func(id string) (intentJSON, string) {
		t.Helper()
		var asJSON, text bytes.Buffer
		if code, err := runIntent(&asJSON, []string{"show", id, "-repo", repo, "-json"}); err != nil || code != exitOK {
			t.Fatalf("intent show -json: code=%d err=%v", code, err)
		}
		var one intentJSON
		if err := json.Unmarshal(asJSON.Bytes(), &one); err != nil {
			t.Fatalf("parse show JSON: %v\n%s", err, asJSON.String())
		}
		if code, err := runIntent(&text, []string{"show", id, "-repo", repo}); err != nil || code != exitOK {
			t.Fatalf("intent show: code=%d err=%v", code, err)
		}
		return one, text.String()
	}

	// Never fired: due now, and no next-due timestamp to report yet.
	one, text := show("deps-current")
	if !one.Due || one.LastFired != "" || one.NextDue != "" {
		t.Fatalf("never-fired scheduled intent = %+v, want due with no timestamps", one)
	}
	if !strings.Contains(text, "next due:   now (never fired)") {
		t.Fatalf("show text = %q", text)
	}
	// An intent with no schedule reports no schedule state at all.
	if none, _ := show("manual"); none.Due || none.Schedule != "" || none.NextDue != "" {
		t.Fatalf("unscheduled intent = %+v, want no schedule state", none)
	}

	// Fired inside its window: not due, and the next due time is last + schedule.
	last := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	firedPath := intentFiredPathFor(context.Background(), repo)
	if firedPath == "" {
		t.Fatal("a git repo must resolve a fired-record path")
	}
	if err := writeIntentFired(firedPath, intentFired{Intents: map[string]intentFiredEntry{"deps-current": {FiredAt: last}}}); err != nil {
		t.Fatal(err)
	}
	one, text = show("deps-current")
	if one.Due {
		t.Fatalf("an intent fired 2h ago on a 24h schedule reports due: %+v", one)
	}
	if one.LastFired != last.Format(time.RFC3339) || one.NextDue != last.Add(24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("show = %+v, want lastFired %s and nextDue +24h", one, last.Format(time.RFC3339))
	}
	if !strings.Contains(text, "next due:   "+last.Add(24*time.Hour).Format(time.RFC3339)) {
		t.Fatalf("show text = %q", text)
	}

	// Overdue: due now, and the time it was owed is still reported.
	overdue := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	if err := writeIntentFired(firedPath, intentFired{Intents: map[string]intentFiredEntry{"deps-current": {FiredAt: overdue}}}); err != nil {
		t.Fatal(err)
	}
	one, text = show("deps-current")
	if !one.Due || one.NextDue != overdue.Add(24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("overdue intent = %+v", one)
	}
	if !strings.Contains(text, "next due:   now (was due") {
		t.Fatalf("show text = %q", text)
	}

	// `sig intent list` reports the same verdict, against one clock.
	var list bytes.Buffer
	if code, err := runIntent(&list, []string{"list", "-repo", repo, "-json"}); err != nil || code != exitOK {
		t.Fatalf("intent list: code=%d err=%v", code, err)
	}
	var rows []intentJSON
	if err := json.Unmarshal(list.Bytes(), &rows); err != nil {
		t.Fatalf("parse list JSON: %v\n%s", err, list.String())
	}
	for _, r := range rows {
		switch r.ID {
		case "deps-current":
			if !r.Due || r.NextDue == "" {
				t.Fatalf("list row = %+v, want the same due verdict show gives", r)
			}
		case "manual":
			if r.Due || r.Schedule != "" {
				t.Fatalf("list row = %+v, want no schedule state", r)
			}
		}
	}
}
