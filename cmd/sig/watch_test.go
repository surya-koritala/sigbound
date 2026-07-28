package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/surya-koritala/sigbound/v2/cell"
	"github.com/surya-koritala/sigbound/v2/internal/gitx"
)

// ---- fixture ----

// syncBuffer is an io.Writer a test can read while a watch loop writes to it.
// The event emitter serializes its own writes, but a test READING the stream
// concurrently is a second party the emitter knows nothing about.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// events decodes every watch_* line emitted so far.
func (s *syncBuffer) events() []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(s.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// countEvent counts emitted events of one name.
func (s *syncBuffer) countEvent(name string) int {
	n := 0
	for _, e := range s.events() {
		if e["event"] == name {
			n++
		}
	}
	return n
}

// lastEvent returns the most recent event of one name, or nil.
func (s *syncBuffer) lastEvent(name string) map[string]any {
	var last map[string]any
	for _, e := range s.events() {
		if e["event"] == name {
			last = e
		}
	}
	return last
}

type watchFixture struct {
	t    *testing.T
	repo string
	g    *gitx.Git
	s    *server
	rc   *registeredCell
	cfg  watchConfig
	ev   *syncBuffer
	// lastDir is the run dir the most recent cycle created; see cycle().
	lastDir string
	ctx     context.Context
	stop    context.CancelFunc
}

// newWatchFixture builds a repo whose policy declares a verify battery, plus a
// server over it with watch enabled. verify is the policy's verify line.
func newWatchFixture(t *testing.T, verify string) *watchFixture {
	t.Helper()
	g, repo := makeGoRepo(t)
	commitPolicy(t, g, repo, "verify = "+verify+"\n")
	return newWatchFixtureAt(t, g, repo)
}

func newWatchFixtureAt(t *testing.T, g *gitx.Git, repo string) *watchFixture {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	s, err := newServer(ctx, serverConfig{repos: []string{repo}, envMode: envModeInherit})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ev := &syncBuffer{}
	s.watchOn = true
	s.watchEvents = &eventEmitter{enc: json.NewEncoder(ev)}
	f := &watchFixture{
		t: t, repo: repo, g: g, s: s, rc: s.cells[0], ev: ev, ctx: ctx, stop: stop,
		cfg: watchConfig{base: "main", maxRed: 3},
	}
	return f
}

// arrive pushes a new branch holding one file, forked from the CURRENT main —
// the shape of a real arrival, which by construction contains the base.
func (f *watchFixture) arrive(branch, file, content string) {
	f.t.Helper()
	mkBranchFrom(f.t, f.g, branch, "main", map[string]string{file: content})
}

// cycle drives exactly one cycle synchronously, as due. No loop, no ticker, no
// clock: the test IS the cadence. It also remembers the run dir the cycle
// created, because run ids carry only SECOND precision plus a random suffix —
// two cycles inside one second sort by that suffix, so "the newest directory" is
// not a reliable way to find the run that just happened.
func (f *watchFixture) cycle() bool {
	f.t.Helper()
	before := map[string]bool{}
	for _, d := range f.runDirs() {
		before[d] = true
	}
	ran := f.s.watchCycle(f.ctx, f.rc, f.cfg, true)
	for _, d := range f.runDirs() {
		if !before[d] {
			f.lastDir = filepath.Join(f.rc.runsDir, d)
		}
	}
	return ran
}

func (f *watchFixture) seen() watchSeen {
	f.t.Helper()
	return readWatchSeen(f.rc.watchSeenPath())
}

func (f *watchFixture) mainSHA() string {
	f.t.Helper()
	sha, err := f.g.RevParse(context.Background(), "main")
	if err != nil {
		f.t.Fatal(err)
	}
	return sha
}

func (f *watchFixture) mainTree() string {
	f.t.Helper()
	tree, err := f.g.TreeOID(context.Background(), "main")
	if err != nil {
		f.t.Fatal(err)
	}
	return tree
}

// runDirs lists this cell's run directories, oldest first.
func (f *watchFixture) runDirs() []string {
	f.t.Helper()
	entries, err := os.ReadDir(f.rc.runsDir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, de := range entries {
		if de.IsDir() {
			ids = append(ids, de.Name())
		}
	}
	sort.Strings(ids)
	return ids
}

// lastReport reads the manifest of the run the most recent cycle drove.
func (f *watchFixture) lastReport() *runReport {
	f.t.Helper()
	if f.lastDir == "" {
		f.t.Fatal("no cycle has driven a run yet")
	}
	rep, err := readRunReport(f.lastDir)
	if err != nil {
		f.t.Fatalf("read %s: %v", f.lastDir, err)
	}
	return rep
}

// waitFor blocks until cond holds, failing the test if it never does. It waits
// on a CONDITION the test itself will cause, never on a duration hoping an
// interleaving happens — the deadline exists only to turn a hang into a failure.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// gate is a filesystem rendezvous between the test and a command a run
// executes: the command announces it has started and then blocks until the test
// releases it. It is what makes "the tick happened WHILE a run was in flight" a
// fact rather than a hope.
type gate struct {
	dir string
}

func newGate(t *testing.T) *gate {
	t.Helper()
	g := &gate{dir: t.TempDir()}
	// Release on the way out no matter how the test ended. A gated command is a
	// spin loop, and the tests that never release it deliberately (the drain
	// case) rely on ctx cancellation killing it; if that ever failed to reach the
	// shell, the leftover would burn CPU under every test that ran after it.
	t.Cleanup(func() { os.WriteFile(filepath.Join(g.dir, "go"), nil, 0o644) }) //nolint:errcheck // best-effort cleanup
	return g
}

// shell is a command that touches the started marker, then spins until release.
func (g *gate) shell(then string) string {
	return fmt.Sprintf("touch %q; while [ ! -f %q ]; do sleep 0.01; done; %s",
		filepath.Join(g.dir, "started"), filepath.Join(g.dir, "go"), then)
}

func (g *gate) started() bool {
	_, err := os.Stat(filepath.Join(g.dir, "started"))
	return err == nil
}

func (g *gate) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(g.dir, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- AC2 (safety): the arrival invariant ----

// TestAdoptBranchRefusesAlreadyLandedAndStale is the safety assertion the whole
// corrupt-seen-set degradation rests on, made at the level that enforces it.
// Re-offering an already-landed branch must NOT integrate it: overlay computes a
// branch's contribution as the two-tree diff from base to tip, so a branch that
// no longer contains the base carries stale content for everything the base
// gained since — and integrating it would REVERT that work past a green verify,
// which is exactly what an unguarded re-qualification would do.
func TestAdoptBranchRefusesAlreadyLandedAndStale(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	c, err := cell.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	base0, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	mkBranchFrom(t, g, "agent/a", base0, map[string]string{"a.txt": "a\n"})
	mkBranchFrom(t, g, "agent/b", base0, map[string]string{"b.txt": "b\n"})

	// Land agent/a the ordinary way: a run that adopts it.
	rep, err := driveRun(ctx, runParams{
		Repo: repo, Base: "main", Strategy: "overlay", VerifyCmd: "true",
		AdoptBranches: map[string]string{"agent/a": "agent/a"},
	}, []taskSpec{{ID: "agent/a"}})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.Integrate.Landed) != 1 {
		t.Fatalf("landed = %v, want [agent/a]", rep.Integrate.Landed)
	}
	if rep.Source != "" {
		t.Fatalf("source = %q on a non-watch run, want empty", rep.Source)
	}
	base1, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}

	// agent/a is now IN the base: re-offering it must be refused, not integrated.
	landed := adoptBranch(ctx, c, "agent/a", base1, runParams{}, taskSpec{ID: "agent/a"})
	if landed.OK {
		t.Fatalf("adopted an already-landed branch (ok=true): re-offering it would revert work; stderr=%q", landed.Stderr)
	}
	if !strings.Contains(landed.Stderr, "already contained in base") {
		t.Fatalf("already-landed rejection = %q, want it to name the reason", landed.Stderr)
	}

	// agent/b forked from base0 and never landed: it does NOT contain base1, so
	// integrating it would revert agent/a. Also refused.
	stale := adoptBranch(ctx, c, "agent/b", base1, runParams{}, taskSpec{ID: "agent/b"})
	if stale.OK {
		t.Fatalf("adopted a branch that does not contain the base (ok=true): stderr=%q", stale.Stderr)
	}
	if !strings.Contains(stale.Stderr, "rebase") {
		t.Fatalf("stale rejection = %q, want it to say the branch must be rebased", stale.Stderr)
	}

	// A branch that DOES contain base1 is still adoptable — the guard rejects
	// staleness, not adoption.
	mkBranchFrom(t, g, "agent/c", "main", map[string]string{"c.txt": "c\n"})
	ok := adoptBranch(ctx, c, "agent/c", base1, runParams{}, taskSpec{ID: "agent/c"})
	if !ok.OK || len(ok.Files) != 1 || ok.Files[0] != "c.txt" {
		t.Fatalf("fresh arrival not adopted: ok=%v files=%v stderr=%q", ok.OK, ok.Files, ok.Stderr)
	}
}

// TestWatchLostSeenSetIsANoOp is the composed statement: with the seen-set gone
// (deleted, or corrupt — both read as empty), EVERY branch re-qualifies, and the
// cycle that follows must change nothing at all. No run is driven, the base does
// not move, and the already-landed branches are retired so the next cycle is
// silent too.
func TestWatchLostSeenSetIsANoOp(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.arrive("agent/a", "a.txt", "a\n")
	f.arrive("agent/b", "b.txt", "b\n")
	if !f.cycle() {
		t.Fatal("first cycle did not run")
	}
	landedTree := f.mainTree()
	landedSHA := f.mainSHA()
	if got := f.lastReport().Integrate.Landed; len(got) != 2 {
		t.Fatalf("cycle 1 landed %v, want both branches", got)
	}
	runsAfterLanding := len(f.runDirs())

	for _, corrupt := range []struct {
		name  string
		write func(path string)
	}{
		{"deleted", func(p string) { os.Remove(p) }},
		{"truncated", func(p string) { os.WriteFile(p, []byte(`{"branches":{"agent/a":`), 0o644) }},
		{"garbage", func(p string) { os.WriteFile(p, []byte("\x00\xff not json at all"), 0o644) }},
		{"wrong shape", func(p string) { os.WriteFile(p, []byte(`[1,2,3]`), 0o644) }},
	} {
		t.Run(corrupt.name, func(t *testing.T) {
			corrupt.write(f.rc.watchSeenPath())
			if readWatchSeen(f.rc.watchSeenPath()).Branches == nil {
				t.Fatal("reader must fail closed to an empty (non-nil) set")
			}
			if f.cycle() {
				t.Error("a cycle ran over already-landed branches; it must retire them without a run")
			}
			if f.mainTree() != landedTree || f.mainSHA() != landedSHA {
				t.Fatalf("base moved on a re-qualification: tree %s->%s. Re-integrating a landed branch REVERTS work",
					short(landedTree), short(f.mainTree()))
			}
			if n := len(f.runDirs()); n != runsAfterLanding {
				t.Fatalf("runs = %d, want %d: no run may be driven for already-landed branches", n, runsAfterLanding)
			}
			// Retired, so the NEXT cycle needs no ancestry check at all.
			for _, b := range []string{"agent/a", "agent/b"} {
				if e := f.seen().Branches[b]; !e.Done {
					t.Fatalf("%s not retired in the seen-set after re-qualifying: %+v", b, e)
				}
			}
		})
	}
}

// ---- AC1: continuous cycles ----

// TestWatchLandsArrivalsAcrossCycles is the stream case: branches arrive over
// three cycles and each lands, with the seen-set advancing to exactly the SHAs
// decided so far and nothing being processed twice.
func TestWatchLandsArrivalsAcrossCycles(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "test -f go.mod")

	want := map[string]string{}
	for i, name := range []string{"one", "two", "three"} {
		branch := "agent/" + name
		f.arrive(branch, name+".txt", name+"\n")
		sha, err := f.g.RevParse(context.Background(), branch)
		if err != nil {
			t.Fatal(err)
		}
		want[branch] = sha

		before := f.mainSHA()
		if !f.cycle() {
			t.Fatalf("cycle %d did not run", i+1)
		}
		rep := f.lastReport()
		if rep.Source != watchSource {
			t.Fatalf("cycle %d source = %q, want %q", i+1, rep.Source, watchSource)
		}
		if len(rep.Tasks) != 1 || rep.Tasks[0].ID != branch {
			t.Fatalf("cycle %d took %v, want only the newly arrived %s", i+1, rep.Tasks, branch)
		}
		if !rep.Verify.Ran || !rep.Verify.OK {
			t.Fatalf("cycle %d verify: ran=%v ok=%v", i+1, rep.Verify.Ran, rep.Verify.OK)
		}
		if len(rep.Integrate.Landed) != 1 || rep.Integrate.Landed[0] != branch {
			t.Fatalf("cycle %d landed %v, want [%s]", i+1, rep.Integrate.Landed, branch)
		}
		if f.mainSHA() == before {
			t.Fatalf("cycle %d did not advance the base", i+1)
		}
		// The file landed, and so did every earlier one: nothing was reverted.
		for j := 0; j <= i; j++ {
			path := filepath.Join(f.repo, []string{"one", "two", "three"}[j]+".txt")
			if content, _, err := f.g.BlobAt(context.Background(), "main", filepath.Base(path)); err != nil || content == "" {
				t.Fatalf("after cycle %d, %s is not in the base (err %v)", i+1, filepath.Base(path), err)
			}
		}
		// Seen-set: exactly the branches decided so far, each at its own SHA.
		seen := f.seen()
		if len(seen.Branches) != i+1 {
			t.Fatalf("after cycle %d the seen-set holds %d branches, want %d: %+v", i+1, len(seen.Branches), i+1, seen.Branches)
		}
		for b, sha := range want {
			e, ok := seen.Branches[b]
			if !ok || e.SHA != sha || !e.Done || e.Red != 0 {
				t.Fatalf("seen[%s] = %+v, want done at %s", b, e, short(sha))
			}
		}
	}
	if n := len(f.runDirs()); n != 3 {
		t.Fatalf("%d runs recorded, want exactly 3 (one per cycle)", n)
	}
}

// ---- AC2: idempotence ----

// TestWatchSeenSetIdempotence: an unchanged branch is never processed twice, and
// a re-push (new SHA) re-qualifies it.
func TestWatchSeenSetIdempotence(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.arrive("agent/a", "a.txt", "a\n")

	if !f.cycle() {
		t.Fatal("first cycle did not run")
	}
	firstRuns := f.runDirs()

	// Same branch, same SHA: no cycle at all, however many times we look.
	for i := 0; i < 3; i++ {
		if f.cycle() {
			t.Fatalf("cycle %d ran again over an unchanged branch", i+2)
		}
	}
	if got := f.runDirs(); len(got) != len(firstRuns) {
		t.Fatalf("runs went %d -> %d over unchanged branches", len(firstRuns), len(got))
	}

	// Re-push: a NEW commit on the same branch name, forked from the base the
	// first cycle landed. It must re-qualify.
	mkBranchFrom(t, f.g, "agent/a2", "main", map[string]string{"a.txt": "a again\n"})
	sha2, err := f.g.RevParse(context.Background(), "agent/a2")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.g.UpdateRef(context.Background(), "refs/heads/agent/a", sha2); err != nil {
		t.Fatal(err)
	}
	if err := f.g.BranchDelete(context.Background(), "agent/a2"); err != nil {
		t.Fatal(err)
	}

	if !f.cycle() {
		t.Fatal("re-pushed branch did not re-qualify")
	}
	if e := f.seen().Branches["agent/a"]; e.SHA != sha2 || !e.Done {
		t.Fatalf("seen[agent/a] = %+v, want done at the re-pushed %s", e, short(sha2))
	}
}

// ---- AC3: per-branch backoff ----

// TestWatchBackoffExcludesRedBranch drives a branch that can never land (its
// verify is red) and asserts it is retried exactly K times, then excluded, with
// a red-branch inbox entry — and that a re-push clears the count.
func TestWatchBackoffExcludesRedBranch(t *testing.T) {
	requirePOSIXShell(t)
	// The policy's verify fails whenever the offending file is present, so the
	// branch's own content is what keeps the cycle red.
	f := newWatchFixture(t, "test ! -f red.txt")
	f.cfg.maxRed = 2 // K=2, per the acceptance criterion
	f.arrive("agent/red", "red.txt", "boom\n")
	sha, err := f.g.RevParse(context.Background(), "agent/red")
	if err != nil {
		t.Fatal(err)
	}
	baseBefore := f.mainSHA()

	for i := 1; i <= f.cfg.maxRed; i++ {
		if !f.cycle() {
			t.Fatalf("cycle %d did not run; a red branch must be RETRIED until it is excluded", i)
		}
		rep := f.lastReport()
		if rep.Verify.OK {
			t.Fatalf("cycle %d verify passed; the fixture needs a genuinely red branch", i)
		}
		if e := f.seen().Branches["agent/red"]; e.Red != i || e.Done {
			t.Fatalf("after cycle %d seen[agent/red] = %+v, want red=%d", i, e, i)
		}
		if f.mainSHA() != baseBefore {
			t.Fatal("a red cycle moved the base: the verify gate must hold")
		}
	}
	// K reached: excluded from now on, whatever we do.
	for i := 0; i < 3; i++ {
		if f.cycle() {
			t.Fatal("a cycle ran for a branch already excluded by backoff")
		}
	}
	if n := len(f.runDirs()); n != f.cfg.maxRed {
		t.Fatalf("%d runs, want exactly K=%d before exclusion", n, f.cfg.maxRed)
	}
	if e := f.ev.lastEvent("watch_backoff"); e == nil || e["branch"] != "agent/red" {
		t.Fatalf("no watch_backoff event naming the branch: %v", e)
	}

	// The inbox raises it for a human, exactly once, as an attention item.
	ts := httptest.NewServer(f.s.handler())
	defer ts.Close()
	var inbox struct{ Entries []inboxEntry }
	if code := doJSON(t, "GET", ts.URL+"/inbox?type=red-branch", "", nil, &inbox); code != http.StatusOK {
		t.Fatalf("GET /inbox: %d", code)
	}
	if len(inbox.Entries) != 1 {
		t.Fatalf("inbox red-branch entries = %d, want 1: %+v", len(inbox.Entries), inbox.Entries)
	}
	if got := inbox.Entries[0].Branches; len(got) != 1 || got[0] != "agent/red" {
		t.Fatalf("red-branch entry branches = %v, want [agent/red]", got)
	}
	if !strings.Contains(inbox.Entries[0].Summary, "re-push") {
		t.Fatalf("red-branch summary should say how to clear it: %q", inbox.Entries[0].Summary)
	}

	// A re-push clears the count and re-qualifies the branch.
	mkBranchFrom(t, f.g, "agent/fixed", "main", map[string]string{"fine.txt": "ok\n"})
	newSHA, err := f.g.RevParse(context.Background(), "agent/fixed")
	if err != nil {
		t.Fatal(err)
	}
	if newSHA == sha {
		t.Fatal("re-push produced the same SHA")
	}
	if err := f.g.UpdateRef(context.Background(), "refs/heads/agent/red", newSHA); err != nil {
		t.Fatal(err)
	}
	if err := f.g.BranchDelete(context.Background(), "agent/fixed"); err != nil {
		t.Fatal(err)
	}
	if !f.cycle() {
		t.Fatal("a re-pushed branch must re-qualify after backoff excluded the old SHA")
	}
	if e := f.seen().Branches["agent/red"]; !e.Done || e.Red != 0 {
		t.Fatalf("seen[agent/red] = %+v, want a clean done entry after the re-push landed", e)
	}
}

// ---- AC4: the per-cell busy lock ----

// TestWatchSkipsBusyCell forces the exact state a manual run creates — the
// cell's busy slot taken — and asserts the tick SKIPS rather than queueing a
// second cycle, then resumes once the slot is free. Taking the lock directly is
// what makes this deterministic: there is no window to hit.
func TestWatchSkipsBusyCell(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.arrive("agent/a", "a.txt", "a\n")

	cellID := f.rc.cell.ID()
	f.s.mu.Lock()
	f.s.busy[cellID] = true
	f.s.mu.Unlock()

	if f.cycle() {
		t.Fatal("a cycle ran while the cell was busy")
	}
	if n := len(f.runDirs()); n != 0 {
		t.Fatalf("%d run dirs created on a skipped tick, want 0", n)
	}
	if f.ev.countEvent("watch_skip") != 1 {
		t.Fatalf("watch_skip events = %d, want 1", f.ev.countEvent("watch_skip"))
	}
	if e := f.ev.lastEvent("watch_skip"); e == nil || e["pending"] != float64(1) {
		t.Fatalf("watch_skip should report what it left pending: %v", e)
	}
	// The branch was NOT consumed: it is still pending for the next tick.
	if e, ok := f.seen().Branches["agent/a"]; ok && (e.Done || e.Red > 0) {
		t.Fatalf("a skipped tick recorded a decision: %+v", e)
	}

	f.s.mu.Lock()
	f.s.busy[cellID] = false
	f.s.mu.Unlock()

	if !f.cycle() {
		t.Fatal("the cycle did not resume on the next tick")
	}
	if got := f.lastReport().Integrate.Landed; len(got) != 1 {
		t.Fatalf("resumed cycle landed %v, want the pending branch", got)
	}
}

// TestManualRunDuringCycleGets409 is the other direction: a POST that arrives
// while a CYCLE holds the cell gets the existing 409, not a second concurrent
// run. The cycle is pinned in flight by a gate its verify command blocks on, so
// the POST provably lands inside the cycle rather than hopefully.
func TestManualRunDuringCycleGets409(t *testing.T) {
	requirePOSIXShell(t)
	g := newGate(t)
	f := newWatchFixture(t, g.shell("true"))
	f.arrive("agent/a", "a.txt", "a\n")

	ts := httptest.NewServer(f.s.handler())
	defer ts.Close()

	done := make(chan bool, 1)
	go func() { done <- f.cycle() }()
	waitFor(t, "the cycle's verify to start", g.started)

	// The cycle is now provably mid-run and holding the cell.
	var errBody struct{ Error, Code string }
	code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell: f.rc.cell.ID(), Agent: writeFileAgent("x.txt"),
		Tasks: []taskSpec{{ID: "manual", Prompt: "x"}},
	}, &errBody)
	if code != http.StatusConflict {
		t.Fatalf("POST /runs during a cycle = %d, want 409", code)
	}
	if errBody.Code != codeCellBusy {
		t.Fatalf("error code = %q, want %q", errBody.Code, codeCellBusy)
	}

	g.release(t)
	if !<-done {
		t.Fatal("the cycle did not complete")
	}
	if n := len(f.runDirs()); n != 1 {
		t.Fatalf("%d run dirs, want exactly 1: the refused POST must start no run", n)
	}
}

// ---- AC5: parks from a cycle ----

// TestWatchCycleParksToInbox: a cycle whose landing touches an ack-path parks
// exactly like a POSTed run's would — it appears in the inbox and acks normally.
func TestWatchCycleParksToInbox(t *testing.T) {
	requirePOSIXShell(t)
	g, repo := makeGoRepo(t)
	commitPolicy(t, g, repo, "verify = true\nack-paths = docs/**\n")
	f := newWatchFixtureAt(t, g, repo)
	f.arrive("agent/docs", "docs/guide.md", "hello\n")

	baseBefore := f.mainSHA()
	if !f.cycle() {
		t.Fatal("cycle did not run")
	}
	if f.mainSHA() != baseBefore {
		t.Fatal("a parked landing moved the base; the ack is what releases it")
	}
	rep := f.lastReport()
	if rep.Park == nil {
		t.Fatalf("cycle did not park an ack-path landing: %+v", rep.Integrate)
	}
	// Parked is a DECISION: the branch must not be re-offered next cycle.
	if e := f.seen().Branches["agent/docs"]; !e.Done {
		t.Fatalf("parked branch not marked done: %+v", e)
	}
	if f.cycle() {
		t.Fatal("a second cycle ran over a branch already parked awaiting ack")
	}

	ts := httptest.NewServer(f.s.handler())
	defer ts.Close()
	var inbox struct{ Entries []inboxEntry }
	if code := doJSON(t, "GET", ts.URL+"/inbox?type=parked", "", nil, &inbox); code != http.StatusOK {
		t.Fatalf("GET /inbox: %d", code)
	}
	if len(inbox.Entries) != 1 {
		t.Fatalf("parked inbox entries = %d, want 1", len(inbox.Entries))
	}
	runID := inbox.Entries[0].RunID
	var out ackOutcome
	if code := doJSON(t, "POST", ts.URL+"/runs/"+runID+"/ack", "", struct{}{}, &out); code != http.StatusOK {
		t.Fatalf("POST ack = %d", code)
	}
	if out.LandedSHA == "" || f.mainSHA() != out.LandedSHA {
		t.Fatalf("ack did not land: outcome=%+v main=%s", out, short(f.mainSHA()))
	}
}

// ---- AC6: drain on shutdown ----

// TestWatchDrainsOnShutdown cancels the daemon's context while a cycle is
// provably mid-run and asserts the loop drains: the cycle reaches a TERMINAL
// journal state (never "interrupted", which is what a crashed run looks like),
// the seen-set is persisted, and the drain is not counted against the branch.
func TestWatchDrainsOnShutdown(t *testing.T) {
	requirePOSIXShell(t)
	gt := newGate(t)
	f := newWatchFixture(t, gt.shell("true"))
	f.arrive("agent/a", "a.txt", "a\n")

	tick := make(chan time.Time)
	f.cfg.tick = tick
	f.cfg.interval = 0 // every tick is due
	f.s.wg.Add(1)
	go f.s.watchLoop(f.ctx, f.rc, f.cfg)

	tick <- time.Now()
	waitFor(t, "the cycle's verify to start", gt.started)

	// Kill the daemon mid-cycle, exactly as a SIGTERM does.
	f.stop()

	drained := make(chan struct{})
	go func() { f.s.wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(60 * time.Second):
		t.Fatal("shutdown did not drain: wg.Wait blocked")
	}

	dirs := f.runDirs()
	if len(dirs) != 1 {
		t.Fatalf("%d run dirs, want 1", len(dirs))
	}
	status, note := diskRunStatus(filepath.Join(f.rc.runsDir, dirs[0]))
	if status == "interrupted" || status == "running" || status == "queued" {
		t.Fatalf("drained cycle left a non-terminal journal entry: status=%q note=%q", status, note)
	}
	if _, err := os.Stat(f.rc.watchSeenPath()); err != nil {
		t.Fatalf("seen-set not persisted across shutdown: %v", err)
	}
	if e, ok := f.seen().Branches["agent/a"]; ok && e.Red != 0 {
		t.Fatalf("the drain was counted against the branch: %+v", e)
	}
	if f.ev.countEvent("watch_drain") != 1 {
		t.Fatalf("watch_drain events = %d, want 1", f.ev.countEvent("watch_drain"))
	}
}

// ---- AC7: indistinguishable from a POSTed run ----

// TestWatchRunRecordedLikePostedRun compares a cycle's manifest against a POSTed
// run's: same recorded shape, same run-dir artifacts, same `sig log` row shape.
// The ONLY key either manifest has that the other does not is source.
func TestWatchRunRecordedLikePostedRun(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")

	// A POSTed run, driven the ordinary way.
	ts := httptest.NewServer(f.s.handler())
	defer ts.Close()
	var created struct{ RunID string }
	if code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell: f.rc.cell.ID(), Agent: writeFileAgent("posted.txt"), Verify: "true",
		Tasks: []taskSpec{{ID: "posted", Prompt: "x"}},
	}, &created); code != http.StatusAccepted {
		t.Fatalf("POST /runs = %d", code)
	}
	if got := pollRun(t, ts, "", created.RunID); got.Status != "done" {
		t.Fatalf("posted run status = %q: %s", got.Status, got.Error)
	}
	postedDir := filepath.Join(f.rc.runsDir, created.RunID)

	// A cycle, over a branch that arrived on its own.
	f.arrive("agent/a", "watched.txt", "a\n")
	if !f.cycle() {
		t.Fatal("cycle did not run")
	}
	var cycleDir string
	for _, id := range f.runDirs() {
		if id != created.RunID {
			cycleDir = filepath.Join(f.rc.runsDir, id)
		}
	}

	keys := func(dir string) map[string]bool {
		data, err := os.ReadFile(filepath.Join(dir, "report.json"))
		if err != nil {
			t.Fatalf("read manifest %s: %v", dir, err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for k := range m {
			out[k] = true
		}
		return out
	}
	posted, cycle := keys(postedDir), keys(cycleDir)
	if !cycle["source"] {
		t.Fatal("the cycle's manifest has no source field")
	}
	if posted["source"] {
		t.Fatal("a POSTed run recorded a source field; it must stay byte-identical to before -watch existed")
	}
	delete(cycle, "source")
	if diff := symmetricDiff(posted, cycle); len(diff) > 0 {
		t.Fatalf("manifests differ in keys %v; a cycle must be recorded exactly like a POSTed run", diff)
	}

	// Same durable artifacts on disk.
	files := func(dir string) map[string]bool {
		out := map[string]bool{}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, de := range entries {
			out[de.Name()] = true
		}
		return out
	}
	if diff := symmetricDiff(files(postedDir), files(cycleDir)); len(diff) > 0 {
		t.Fatalf("run dirs differ in %v; a cycle must leave the same journal", diff)
	}

	// Same `sig log` row shape: both done, both landed one branch by one agent.
	rows, _ := scanRuns(f.rc.runsDir, 10)
	if len(rows) != 2 {
		t.Fatalf("sig log rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Status != "done" || r.Tasks != 1 || r.Agents != 1 || r.Landed != 1 || r.LandedSHA == "" || r.Incomplete {
			t.Fatalf("row %+v: a cycle and a POSTed run must render identically in sig log", r)
		}
	}
	// And the manifest's own verdict fields agree.
	repCycle, err := readRunReport(cycleDir)
	if err != nil {
		t.Fatal(err)
	}
	if repCycle.Source != watchSource {
		t.Fatalf("cycle source = %q, want %q", repCycle.Source, watchSource)
	}
}

// symmetricDiff names the keys present in exactly one of the two sets.
func symmetricDiff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, "-"+k)
		}
	}
	for k := range b {
		if !a[k] {
			out = append(out, "+"+k)
		}
	}
	sort.Strings(out)
	return out
}

// ---- AC8: quotas apply per cycle ----

// TestWatchQuotaSplitsOversizedBatch documents and proves the choice: a batch
// larger than -max-agents-per-run is SPLIT across cycles, not rejected. A POST
// that exceeds the quota is refused because its caller asked for something the
// server will not do; refusing a CYCLE would instead mean a server whose quota
// sits below its arrival rate lands nothing at all, ever, so a cycle takes the
// first N in branch-name order and defers the rest.
//
// It also pins the deferral's consequence, which is the arrival invariant rather
// than anything about quotas: the cycle moves the base, so a deferred branch no
// longer contains it and must be rebased. Deferred work is reported, never
// silently dropped, and a rebase re-qualifies it.
func TestWatchQuotaSplitsOversizedBatch(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.s.maxAgentsPerRun = 1
	f.arrive("agent/a", "a.txt", "a\n")
	f.arrive("agent/b", "b.txt", "b\n")
	f.arrive("agent/c", "c.txt", "c\n")

	if !f.cycle() {
		t.Fatal("cycle 1 did not run")
	}
	rep := f.lastReport()
	if len(rep.Tasks) != 1 || rep.Tasks[0].ID != "agent/a" {
		t.Fatalf("cycle 1 took %v, want exactly [agent/a] (quota 1, branch-name order)", rep.Tasks)
	}
	if got := rep.Integrate.Landed; len(got) != 1 || got[0] != "agent/a" {
		t.Fatalf("cycle 1 landed %v", got)
	}
	if e := f.ev.lastEvent("watch_tick"); e == nil || e["deferred"] != float64(2) {
		t.Fatalf("watch_tick = %v, want deferred=2", e)
	}

	// The deferred two are behind the base this cycle just moved: reported, not
	// run, and not counted against them as red.
	if f.cycle() {
		t.Fatal("a cycle ran for branches the landing left behind")
	}
	if got := f.ev.countEvent("watch_stale"); got != 2 {
		t.Fatalf("watch_stale = %d, want one per deferred branch", got)
	}
	for _, b := range []string{"agent/b", "agent/c"} {
		if e := f.seen().Branches[b]; !e.Stale || e.Red != 0 {
			t.Fatalf("seen[%s] = %+v, want stale with no red count", b, e)
		}
	}
	if n := len(f.runDirs()); n != 1 {
		t.Fatalf("%d runs, want 1", n)
	}

	// Rebasing one onto the new base re-qualifies it, and the quota still applies.
	mkBranchFrom(t, f.g, "agent/b2", "main", map[string]string{"b.txt": "b\n"})
	sha, err := f.g.RevParse(context.Background(), "agent/b2")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.g.UpdateRef(context.Background(), "refs/heads/agent/b", sha); err != nil {
		t.Fatal(err)
	}
	if err := f.g.BranchDelete(context.Background(), "agent/b2"); err != nil {
		t.Fatal(err)
	}
	if !f.cycle() {
		t.Fatal("a rebased deferred branch must re-qualify")
	}
	if got := f.lastReport().Integrate.Landed; len(got) != 1 || got[0] != "agent/b" {
		t.Fatalf("landed %v, want [agent/b]", got)
	}
}

// TestWatchPolicyMaxAgentsStillRejects: the policy's own max-agents is NOT
// clamped by the cycle — it fails the run exactly as it fails a POSTed one, and
// the branches then walk into backoff rather than silently landing in pieces.
func TestWatchPolicyMaxAgentsStillRejects(t *testing.T) {
	requirePOSIXShell(t)
	g, repo := makeGoRepo(t)
	commitPolicy(t, g, repo, "verify = true\nmax-agents = 1\n")
	f := newWatchFixtureAt(t, g, repo)
	f.arrive("agent/a", "a.txt", "a\n")
	f.arrive("agent/b", "b.txt", "b\n")

	if !f.cycle() {
		t.Fatal("cycle did not run")
	}
	status, _ := diskRunStatus(f.lastDir)
	if status != "error" {
		t.Fatalf("cycle status = %q, want error: the policy's max-agents rejects the run", status)
	}
	if msg := readRunErrorMsg(f.lastDir); !strings.Contains(msg, "max-agents") {
		t.Fatalf("error = %q, want it to name the policy's max-agents", msg)
	}
	for _, b := range []string{"agent/a", "agent/b"} {
		if e := f.seen().Branches[b]; e.Red != 1 || e.Done {
			t.Fatalf("seen[%s] = %+v, want one red cycle", b, e)
		}
	}
}

// ---- the loop, cadence, and POST /queue ----

// TestWatchLoopBatchTriggerFiresEarly: with an interval that will never elapse
// during the test, only the batch trigger can start a cycle — so the loop must
// wait for the Nth branch and then fire. Every tick is delivered by the test, so
// "did not fire yet" is a fact about the loop, not about timing.
func TestWatchLoopBatchTriggerFiresEarly(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	tick := make(chan time.Time)
	f.cfg.tick = tick
	f.cfg.interval = time.Hour // never due
	f.cfg.batch = 2

	f.s.wg.Add(1)
	go f.s.watchLoop(f.ctx, f.rc, f.cfg)
	defer func() { f.stop(); f.s.wg.Wait() }()

	f.arrive("agent/a", "a.txt", "a\n")
	tick <- time.Now()
	tick <- time.Now() // the second send only returns once the first was consumed
	if n := len(f.runDirs()); n != 0 {
		t.Fatalf("%d runs with 1 pending and batch=2: the trigger fired early", n)
	}

	f.arrive("agent/b", "b.txt", "b\n")
	tick <- time.Now()
	waitFor(t, "the batch cycle to finish", func() bool { return len(f.runDirs()) == 1 })
	var rep *runReport
	waitFor(t, "the batch cycle's manifest", func() bool {
		var err error
		rep, err = readRunReport(filepath.Join(f.rc.runsDir, f.runDirs()[0]))
		return err == nil
	})
	if len(rep.Tasks) != 2 {
		t.Fatalf("batch cycle took %d branches, want both", len(rep.Tasks))
	}
}

func TestWatchConfigPoll(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  watchConfig
		want time.Duration
	}{
		{"interval only", watchConfig{interval: 30 * time.Second}, 30 * time.Second},
		{"batch armed polls faster", watchConfig{interval: 30 * time.Second, batch: 3}, watchPollInterval},
		{"short interval kept", watchConfig{interval: 200 * time.Millisecond, batch: 3}, 200 * time.Millisecond},
		{"zero interval is not a zero ticker", watchConfig{}, watchPollInterval},
	} {
		if got := tc.cfg.poll(); got != tc.want {
			t.Errorf("%s: poll() = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestWatchStaleBranchIsReportedOnce: a branch that does not contain the base
// cannot land, is never fed to a run, and is announced exactly once per pushed
// SHA rather than every tick forever.
func TestWatchStaleBranchIsReportedOnce(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	base0 := f.mainSHA()
	// Land something first, THEN fork from the old base: that fork is behind.
	f.arrive("agent/fresh", "fresh.txt", "f\n")
	if !f.cycle() {
		t.Fatal("first cycle did not run")
	}
	if f.mainSHA() == base0 {
		t.Fatal("nothing landed, so nothing can be behind the base")
	}
	mkBranchFrom(t, f.g, "agent/stale", base0, map[string]string{"stale.txt": "s\n"})

	before := len(f.runDirs())
	for i := 0; i < 3; i++ {
		if f.cycle() {
			t.Fatal("a cycle ran for a branch that cannot be integrated onto the base")
		}
	}
	if n := len(f.runDirs()); n != before {
		t.Fatalf("runs %d -> %d over a stale branch", before, n)
	}
	if got := f.ev.countEvent("watch_stale"); got != 1 {
		t.Fatalf("watch_stale events = %d, want exactly 1 for one pushed SHA", got)
	}
	if e := f.seen().Branches["agent/stale"]; !e.Stale {
		t.Fatalf("seen[agent/stale] = %+v, want stale", e)
	}
	// Rebasing it (a new SHA that DOES contain the base) re-qualifies it.
	mkBranchFrom(t, f.g, "agent/rebased", "main", map[string]string{"stale.txt": "s\n"})
	sha, err := f.g.RevParse(context.Background(), "agent/rebased")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.g.UpdateRef(context.Background(), "refs/heads/agent/stale", sha); err != nil {
		t.Fatal(err)
	}
	if err := f.g.BranchDelete(context.Background(), "agent/rebased"); err != nil {
		t.Fatal(err)
	}
	if !f.cycle() {
		t.Fatal("a rebased branch must re-qualify")
	}
}

// TestWatchQueueEndpoint covers POST /queue: it validates against existing refs,
// and an enqueued branch outside the watched namespaces reaches the next cycle.
func TestWatchQueueEndpoint(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	mkBranchFrom(t, f.g, "feature/x", "main", map[string]string{"x.txt": "x\n"})

	ts := httptest.NewServer(f.s.handler())
	defer ts.Close()
	cellID := f.rc.cell.ID()

	// Not watched by prefix: no cycle sees it until it is enqueued.
	if f.cycle() {
		t.Fatal("a cycle ran for a branch outside the watched namespaces")
	}

	var errBody struct{ Error, Code string }
	if code := doJSON(t, "POST", ts.URL+"/queue", "", queueRequest{Cell: "nope", Branches: []string{"feature/x"}}, &errBody); code != http.StatusBadRequest {
		t.Fatalf("unknown cell = %d, want 400", code)
	}
	if errBody.Code != codeCellNotFound {
		t.Fatalf("unknown cell code = %q", errBody.Code)
	}
	if code := doJSON(t, "POST", ts.URL+"/queue", "", queueRequest{Cell: cellID, Branches: []string{"no/such/branch"}}, &errBody); code != http.StatusBadRequest {
		t.Fatalf("unknown branch = %d, want 400", code)
	}
	if !strings.Contains(errBody.Error, "does not exist") {
		t.Fatalf("unknown branch error = %q", errBody.Error)
	}
	if code := doJSON(t, "POST", ts.URL+"/queue", "", queueRequest{Cell: cellID}, &errBody); code != http.StatusBadRequest {
		t.Fatalf("no branches = %d, want 400", code)
	}

	var ok struct {
		Cell   string
		Queued int
	}
	if code := doJSON(t, "POST", ts.URL+"/queue", "", queueRequest{Cell: cellID, Branches: []string{"feature/x"}}, &ok); code != http.StatusAccepted {
		t.Fatalf("valid enqueue = %d, want 202", code)
	}
	if ok.Queued != 1 {
		t.Fatalf("queued = %d, want 1", ok.Queued)
	}
	// Idempotent: enqueueing twice does not double it.
	doJSON(t, "POST", ts.URL+"/queue", "", queueRequest{Cell: cellID, Branches: []string{"feature/x"}}, &ok)
	if ok.Queued != 1 {
		t.Fatalf("re-queued = %d, want 1", ok.Queued)
	}

	if !f.cycle() {
		t.Fatal("the enqueued branch did not reach a cycle")
	}
	if got := f.lastReport().Integrate.Landed; len(got) != 1 || got[0] != "feature/x" {
		t.Fatalf("landed %v, want [feature/x]", got)
	}

	// With watch off the endpoint refuses rather than silently accepting.
	f.s.watchOn = false
	if code := doJSON(t, "POST", ts.URL+"/queue", "", queueRequest{Cell: cellID, Branches: []string{"feature/x"}}, &errBody); code != http.StatusBadRequest {
		t.Fatalf("queue without -watch = %d, want 400", code)
	}
	if errBody.Code != codeWatchDisabled {
		t.Fatalf("code = %q, want %q", errBody.Code, codeWatchDisabled)
	}
}

// TestWatchImportedRefsAreASource: `sig import` lands bundles under
// imported/<worker>/, which a cycle picks up with no enqueue at all.
func TestWatchImportedRefsAreASource(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	mkBranchFrom(t, f.g, "imported/w1/task", "main", map[string]string{"w1.txt": "w\n"})

	if !f.cycle() {
		t.Fatal("cycle did not run for an imported ref")
	}
	if got := f.lastReport().Integrate.Landed; len(got) != 1 || got[0] != "imported/w1/task" {
		t.Fatalf("landed %v, want the imported branch", got)
	}
}

// ---- startup: config resolution ----

func TestResolveWatchConfigPolicyPrecedence(t *testing.T) {
	ctx := context.Background()
	base := watchConfig{base: "main", interval: 30 * time.Second, batch: 0, maxRed: 3, verify: "true"}

	t.Run("policy sets the cadence when the flags are defaults", func(t *testing.T) {
		g, repo := makeGoRepo(t)
		commitPolicy(t, g, repo, "verify = true\nwatch-interval = 5m\nwatch-batch = 7\nwatch-max-red = 9\n")
		_ = repo
		got, err := resolveWatchConfig(ctx, g, base, map[string]bool{})
		if err != nil {
			t.Fatal(err)
		}
		if got.interval != 5*time.Minute || got.batch != 7 || got.maxRed != 9 {
			t.Fatalf("resolved = %+v, want the policy's cadence", got)
		}
	})

	t.Run("an explicit flag against a policy value is a loud error", func(t *testing.T) {
		g, repo := makeGoRepo(t)
		commitPolicy(t, g, repo, "verify = true\nwatch-interval = 5m\n")
		_ = repo
		_, err := resolveWatchConfig(ctx, g, base, map[string]bool{"watch-interval": true})
		if err == nil {
			t.Fatal("an explicit -watch-interval against a policy watch-interval must error")
		}
		for _, want := range []string{policyFileName, "watch-interval", "5m", "30s"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q should name both sources and both values (missing %q)", err, want)
			}
		}
	})

	t.Run("no policy leaves the flags alone", func(t *testing.T) {
		g, _ := makeGoRepo(t)
		got, err := resolveWatchConfig(ctx, g, base, map[string]bool{"watch-interval": true})
		if err != nil {
			t.Fatal(err)
		}
		if got.interval != 30*time.Second || got.maxRed != 3 {
			t.Fatalf("resolved = %+v, want the flags", got)
		}
	})

	t.Run("no verify anywhere refuses to start", func(t *testing.T) {
		g, _ := makeGoRepo(t)
		_, err := resolveWatchConfig(ctx, g, watchConfig{base: "main", maxRed: 3}, map[string]bool{})
		if err == nil || !strings.Contains(err.Error(), "-watch-verify") {
			t.Fatalf("err = %v, want a refusal naming the escape hatch", err)
		}
		if _, err := resolveWatchConfig(ctx, g, watchConfig{base: "main", maxRed: 3, verify: "true"}, map[string]bool{}); err != nil {
			t.Fatalf("-watch-verify true must be accepted: %v", err)
		}
	})
}

func TestStartWatchValidatesConfig(t *testing.T) {
	f := newWatchFixture(t, "true")
	for _, tc := range []struct {
		name string
		cfg  watchConfig
		want string
	}{
		{"negative interval", watchConfig{base: "main", interval: -time.Second, maxRed: 3}, "-watch-interval"},
		{"negative batch", watchConfig{base: "main", batch: -1, maxRed: 3}, "-watch-batch"},
		{"zero max-red", watchConfig{base: "main", maxRed: 0}, "-watch-max-red"},
		{"unknown base", watchConfig{base: "no-such-branch", maxRed: 3, verify: "true"}, "no-such-branch"},
	} {
		err := f.s.startWatch(f.ctx, tc.cfg, map[string]bool{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.want)
		}
	}
	// And with no cells at all there is nothing to watch.
	empty := &server{}
	if err := empty.startWatch(context.Background(), watchConfig{base: "main", maxRed: 3, verify: "true"}, nil); err == nil {
		t.Error("-watch with zero registered cells must be a startup error")
	}
}

// TestPolicyWatchKeys covers the three cadence keys parsePolicy must know: a
// repo that declares one would otherwise fail EVERY run with "unknown key".
func TestPolicyWatchKeys(t *testing.T) {
	pol, err := parsePolicy([]byte("watch-interval = 90s\nwatch-batch = 4\nwatch-max-red = 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if pol.WatchInterval != 90*time.Second || pol.WatchBatch != 4 || pol.WatchMaxRed != 5 {
		t.Fatalf("parsed = %+v", pol)
	}
	// They gate nothing, so they must not touch a run's resolved params.
	p := runParams{}
	if err := resolvePolicy(pol, &p, 1); err != nil {
		t.Fatal(err)
	}
	if p.Budget != 0 || p.ParallelAgents != 0 || p.VerifyCmd != "" {
		t.Fatalf("watch keys changed the run's params: %+v", p)
	}
	if rep := policyReport(pol); rep == nil || len(rep.Verify) != 0 {
		t.Fatalf("watch keys should not appear on the report: %+v", rep)
	}
	for _, bad := range []string{
		"watch-interval = never\n", "watch-interval = 0s\n", "watch-interval = -5s\n",
		"watch-batch = 0\n", "watch-batch = -1\n", "watch-batch = many\n",
		"watch-max-red = 0\n", "watch-max-red = x\n",
		"watch-interval = 1s\nwatch-interval = 2s\n",
	} {
		if _, err := parsePolicy([]byte(bad)); err == nil {
			t.Errorf("parsePolicy(%q) accepted an invalid value", bad)
		}
	}
}

// ---- the loop's cadence ----

// TestWatchStepResetsTheIntervalOnEveryDueCycle pins what -watch-interval
// actually bounds. The loop only ever reset its clock when a cycle DROVE a run,
// so one due poll that found nothing left every later poll due — and with a
// batch trigger armed that is a poll a second: an intent's schedule checked 30x
// per interval, and a red branch's backoff counted in polls.
//
// Driven through watchStep with the caller owning `last`, so a cadence is proven
// without waiting out an interval.
func TestWatchStepResetsTheIntervalOnEveryDueCycle(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.cfg.interval = time.Hour
	// A due intent with no -watch-agent reports itself on every DUE poll and
	// drives no run: that is what makes "was this poll due?" observable.
	writeIntent(t, f.repo, "deps-current", "goal = update dependencies\nschedule = 24h\n")

	last := f.s.watchStep(f.ctx, f.rc, f.cfg, time.Now().Add(-2*time.Hour))
	if n := f.ev.countEvent("watch_error"); n != 1 {
		t.Fatalf("watch_error events = %d after an overdue poll, want 1 (the poll was not treated as due)", n)
	}
	if time.Since(last) > time.Minute {
		t.Fatal("a due poll that drove no run left the interval clock where it was: every later poll is due")
	}
	// The next poll is inside the interval, so it must NOT be due.
	next := f.s.watchStep(f.ctx, f.rc, f.cfg, last)
	if !next.Equal(last) {
		t.Fatalf("a non-due poll moved the clock: %s -> %s", last, next)
	}
	if n := f.ev.countEvent("watch_error"); n != 1 {
		t.Fatalf("watch_error events = %d, want 1: a poll inside the interval was treated as due", n)
	}
}

// TestWatchPrunesSeenEntriesForBranchesThatAreGone: an entry whose branch no
// longer exists can never match a head again, so keeping it is permanent growth
// — one line per branch this daemon has ever decided about, forever.
func TestWatchPrunesSeenEntriesForBranchesThatAreGone(t *testing.T) {
	requirePOSIXShell(t)
	f := newWatchFixture(t, "true")
	f.arrive("agent/a", "a.txt", "a\n")
	f.arrive("agent/b", "b.txt", "b\n")
	if !f.cycle() {
		t.Fatal("the first cycle did not run")
	}
	if n := len(f.seen().Branches); n != 2 {
		t.Fatalf("seen-set holds %d entries, want both landed branches", n)
	}
	if err := f.g.BranchDelete(context.Background(), "agent/a"); err != nil {
		t.Fatal(err)
	}
	if f.cycle() {
		t.Fatal("a cycle ran over already-decided branches")
	}
	seen := f.seen()
	if _, ok := seen.Branches["agent/a"]; ok {
		t.Fatal("the seen-set kept an entry for a branch that no longer exists")
	}
	// Negative control: the branch that DOES still exist keeps its decision, or
	// pruning would just be re-examining everything every cycle.
	if e := seen.Branches["agent/b"]; !e.Done {
		t.Fatalf("seen[agent/b] = %+v, want the decision it already reached", e)
	}
}

// ---- one cell per git directory ----

// TestServerRefusesTwoCellsOverOneGitDir: a repo and its own linked worktree are
// two paths to ONE git directory. Everything durable — the run journal, the
// watch seen-set, intent-fired.json — lives under that shared directory, while
// the busy lock that serializes writers is per cell, so registering both would
// give two "cells" that race each other's read-modify-writes and double-fire one
// scheduled intent. It is the same rule one-run-per-cell already states.
func TestServerRefusesTwoCellsOverOneGitDir(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	if err := g.WorktreeAdd(ctx, linked, "linked-wt", "main"); err != nil {
		t.Fatal(err)
	}
	_, err := newServer(ctx, serverConfig{repos: []string{repo, linked}, envMode: envModeInherit})
	if err == nil {
		t.Fatal("a repo and its own linked worktree were registered as two cells")
	}
	for _, want := range []string{repo, linked, "share one git directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	// Negative control: two independent repos are still two cells.
	_, other := makeGoRepo(t)
	if _, err := newServer(ctx, serverConfig{repos: []string{repo, other}, envMode: envModeInherit}); err != nil {
		t.Fatalf("two independent repos refused: %v", err)
	}
}

// ---- seen-set reader ----

func TestReadWatchSeenFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, watchSeenFileName)
	for _, tc := range []struct {
		name string
		data string
		want int
	}{
		{"missing", "", 0},
		{"empty file", "", 0},
		{"truncated", `{"branches":{"a":{"sha":"x"`, 0},
		{"null branches", `{"branches":null}`, 0},
		{"array", `[]`, 0},
		{"entry without sha", `{"branches":{"a":{"done":true}}}`, 0},
		{"negative red", `{"branches":{"a":{"sha":"x","red":-3}}}`, 0},
		{"empty branch name", `{"branches":{"":{"sha":"x"}}}`, 0},
		{"good", `{"branches":{"agent/a":{"sha":"x","done":true}}}`, 1},
		{"good among bad", `{"branches":{"agent/a":{"sha":"x"},"b":{"done":true}}}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			os.Remove(path)
			if tc.name != "missing" {
				if err := os.WriteFile(path, []byte(tc.data), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := readWatchSeen(path)
			if got.Branches == nil {
				t.Fatal("Branches must never be nil: every caller writes into it")
			}
			if len(got.Branches) != tc.want {
				t.Fatalf("read %d entries, want %d: %+v", len(got.Branches), tc.want, got.Branches)
			}
		})
	}
}

func TestWriteWatchSeenIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", watchSeenFileName)
	s := watchSeen{Branches: map[string]watchSeenEntry{"agent/a": {SHA: "abc", Done: true, Red: 2}}}
	writeWatchSeen(path, s)
	got := readWatchSeen(path)
	if e := got.Branches["agent/a"]; e.SHA != "abc" || !e.Done || e.Red != 2 {
		t.Fatalf("round-trip = %+v", got.Branches)
	}
	// No temp file left behind for a reader (or `sig gc`) to trip over.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != watchSeenFileName {
		t.Fatalf("dir holds %d entries, want just the seen-set", len(entries))
	}
}

func FuzzReadWatchSeen(f *testing.F) {
	f.Add(`{"branches":{"agent/a":{"sha":"deadbeef","done":true,"red":1}}}`)
	f.Add(`{"branches":{}}`)
	f.Add(`{"branches":null}`)
	f.Add(`{"branches":{"a":{"sha":"x","red":-2147483648}}}`)
	f.Add(`{"branches":{"":{"sha":""}}}`)
	f.Add(`[]`)
	f.Add(`{`)
	f.Add("")
	f.Add("\x00\xff\xfe")
	f.Fuzz(func(t *testing.T, data string) {
		// Per-case dir: fuzz workers run in parallel, and a shared path would
		// have them reading each other's bytes.
		path := filepath.Join(t.TempDir(), watchSeenFileName)
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Skip()
		}
		got := readWatchSeen(path)
		// The contract the whole degradation rests on: never panics, always
		// returns a usable map, and never invents an entry a cycle could act on
		// without a SHA to compare against a branch head.
		if got.Branches == nil {
			t.Fatal("nil map from a corrupt seen-set")
		}
		for b, e := range got.Branches {
			if b == "" || e.SHA == "" || e.Red < 0 {
				t.Fatalf("unusable entry survived: %q -> %+v", b, e)
			}
		}
	})
}
