package main

// End-to-end coverage for issue #137: a park created by `sig run` is resolvable
// through exactly the same surfaces a `sig serve` park is — `sig ack`, `sig
// reject`, the moved-base re-verify and its refusal, `sig gc`'s unconditional
// protection, and a daemon's inbox. Every test here drives a REAL `sig run`
// (runRun, the CLI's own front door, with real flag parsing) against a real repo
// whose sigbound.policy holds an ack-path, and resolves it through the CLI's own
// runAck/runReject — the run-id-to-run-dir resolution those do is precisely what
// had nothing to resolve before.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

// cliParkFixture is one real parked `sig run` plus the handles a test acts on it
// with. It is deliberately the CLI twin of newParkFixture: same policy, same two
// tasks, same assertion that the clean group landed and the held one did not.
type cliParkFixture struct {
	t     *testing.T
	repo  string
	g     *gitx.Git
	cell  *cell.Cell
	runID string
	dir   string
	rep   runReport
	park  *parkJSON
}

// newCLIParkFixture drives a two-task `sig run`: `clean` writes alpha.go (lands)
// and `held` writes auth/token.go (parks under the ack-path). The run's own
// -verify is real, so what parks is genuinely a verified tree and the moved-base
// re-verify has a command that can go red.
func newCLIParkFixture(t *testing.T) *cliParkFixture {
	t.Helper()
	requirePOSIXShell(t)
	// gcPlanFor with a negative -older-than puts the cutoff in the future, which
	// against the real os.TempDir() would sweep another sigbound test binary's
	// live worktrees. Give every fixture its own root (see newParkFixture).
	origTempRoot := gcTempRoot
	gcRoot := t.TempDir()
	gcTempRoot = func() string { return gcRoot }
	t.Cleanup(func() { gcTempRoot = origTempRoot })

	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	commitPolicy(t, g, repo, parkPolicyAckPaths)

	rep, code, out := runRunJSON(t, repo, agent, []taskSpec{
		taskWrite(t, "clean", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}),
		taskWrite(t, "held", map[string]string{"auth/token.go": "package auth\n\nfunc Token() string { return \"t\" }\n"}),
	}, "-verify", "go build ./...")
	if code != exitFlagged {
		t.Fatalf("exit code %d, want exitFlagged (%d)\n%s", code, exitFlagged, out)
	}
	if rep.Park == nil {
		t.Fatalf("`sig run` reported no park\n%s", out)
	}
	if rep.RunID == "" {
		t.Fatalf("`sig run` reported a park with no runId — nothing can name it to `sig ack`\n%s", out)
	}
	c, err := cell.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := cellRunsDir(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	f := &cliParkFixture{t: t, repo: repo, g: g, cell: c, runID: rep.RunID, dir: filepath.Join(dir, rep.RunID), rep: rep}
	if st := f.status(); st != statusAwaitingAck {
		t.Fatalf("run status %q on disk, want %s", st, statusAwaitingAck)
	}
	f.park = f.reread()
	// The clean group must actually have landed: the park holds ONE group, never
	// the run, and the ack has to be verified against the base as it now stands.
	if head := f.head(); head != f.park.BaseSHA {
		t.Fatalf("park baseSHA %s but main is at %s", short(f.park.BaseSHA), short(head))
	}
	if f.head() == f.park.ForkSHA {
		t.Fatalf("main never advanced past the fork point %s — the clean group did not land", short(f.park.ForkSHA))
	}
	return f
}

func (f *cliParkFixture) reread() *parkJSON {
	f.t.Helper()
	pk, err := readPark(f.dir)
	if err != nil {
		f.t.Fatalf("readPark: %v", err)
	}
	return pk
}

func (f *cliParkFixture) head() string {
	f.t.Helper()
	sha, err := f.g.RevParse(context.Background(), "main")
	if err != nil {
		f.t.Fatal(err)
	}
	return sha
}

func (f *cliParkFixture) status() string {
	f.t.Helper()
	st, _ := diskRunStatus(f.dir)
	return st
}

// treeOf is the byte-for-byte handle: two commits carrying the same tree OID
// carry identical content, which is what "what lands is the tree that passed
// verify" has to be asserted against.
func (f *cliParkFixture) treeOf(rev string) string {
	f.t.Helper()
	tree, err := f.g.TreeOID(context.Background(), rev)
	if err != nil {
		f.t.Fatalf("tree of %s: %v", short(rev), err)
	}
	return tree
}

// moveBase lands extra.go on main so an ack finds a base that has moved. The
// reset is load-bearing: sigbound advances refs in the object store and never
// touches the checkout, so committing without it would revert what the run
// landed (see parkFixture.moveBase).
func (f *cliParkFixture) moveBase(body string) string {
	f.t.Helper()
	ctx := context.Background()
	if err := f.g.ResetHard(ctx); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "extra.go"), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	sha, err := f.g.CommitAll(ctx, "move the base while a run is parked")
	if err != nil {
		f.t.Fatal(err)
	}
	return sha
}

// ack/reject go through the CLI front doors, positional RUN_ID and all — the
// resolution from a run id to a run dir is the thing under test.
func (f *cliParkFixture) ack() (ackOutcome, error) {
	f.t.Helper()
	var buf bytes.Buffer
	_, err := runAck(&buf, []string{f.runID, "-repo", f.repo, "-json"})
	if err != nil {
		return ackOutcome{}, err
	}
	var out ackOutcome
	if jerr := json.Unmarshal(buf.Bytes(), &out); jerr != nil {
		f.t.Fatalf("parse ack outcome: %v\n%s", jerr, buf.String())
	}
	return out, nil
}

func (f *cliParkFixture) reject(reason string) (ackOutcome, error) {
	f.t.Helper()
	var buf bytes.Buffer
	_, err := runReject(&buf, []string{f.runID, "-repo", f.repo, "-reason", reason, "-json"})
	if err != nil {
		return ackOutcome{}, err
	}
	var out ackOutcome
	if jerr := json.Unmarshal(buf.Bytes(), &out); jerr != nil {
		f.t.Fatalf("parse reject outcome: %v\n%s", jerr, buf.String())
	}
	return out, nil
}

// ---- acceptance #1: sig ack releases a CLI park, byte-for-byte ----

func TestCLIParkAckLandsTheVerifiedTree(t *testing.T) {
	f := newCLIParkFixture(t)
	ctx := context.Background()

	// Nothing landed yet, and the held file is in the parked tree only.
	if _, present, err := f.g.BlobAt(ctx, "main", "auth/token.go"); err != nil || present {
		t.Fatalf("auth/token.go landed without an ack (err=%v)", err)
	}
	if _, present, err := f.g.BlobAt(ctx, f.park.VerifiedSHA, "auth/token.go"); err != nil || !present {
		t.Fatalf("auth/token.go missing from the parked tree (err=%v)", err)
	}
	wantTree := f.treeOf(f.park.VerifiedSHA)

	out, err := f.ack()
	if err != nil {
		t.Fatalf("sig ack %s: %v", f.runID, err)
	}
	if out.Status != "done" || out.Reverified {
		t.Fatalf("ack outcome %+v, want status=done with no re-verify", out)
	}
	if out.LandedSHA != f.park.VerifiedSHA {
		t.Fatalf("ack landed %s, want the recorded verifiedSHA %s", short(out.LandedSHA), short(f.park.VerifiedSHA))
	}
	// THE assertion: the tree on the base is the tree that passed verify, not a
	// recomputation that happens to contain the same files.
	if got := f.treeOf("main"); got != wantTree {
		t.Fatalf("main's tree is %s, want the verified tree %s", short(got), short(wantTree))
	}
	if got := f.head(); got != f.park.VerifiedSHA {
		t.Fatalf("main is at %s, want the verified commit %s", short(got), short(f.park.VerifiedSHA))
	}
	if st := f.status(); st != "done" {
		t.Fatalf("status %q after ack, want done", st)
	}
	after := f.reread()
	if after.LandedSHA != f.park.VerifiedSHA || after.ResolvedAt == "" {
		t.Fatalf("parking record not closed out: %+v", after)
	}
	// The keep-alive ref is released once the commit is reachable from the base.
	if _, err := f.g.RevParse(ctx, f.park.KeepRef); err == nil {
		t.Fatalf("keep-alive ref %s still exists after the landing", f.park.KeepRef)
	}
	if _, err := f.ack(); err == nil {
		t.Fatal("a second ack succeeded; a run resolves exactly once")
	}
	assertEvent(t, f.dir, "ack")

	// The CLI run is in the ledger `sig log` reads, which is the smaller gap the
	// run dir also closes.
	var logBuf bytes.Buffer
	if _, err := runLog(&logBuf, []string{"-repo", f.repo, "-json"}); err != nil {
		t.Fatalf("sig log: %v", err)
	}
	var rows []logRow
	if err := json.Unmarshal(logBuf.Bytes(), &rows); err != nil {
		t.Fatalf("parse sig log: %v\n%s", err, logBuf.String())
	}
	found := false
	for _, r := range rows {
		if r.ID == f.runID {
			found = true
			if r.Incomplete {
				t.Fatalf("sig log reports run %s incomplete: %+v", f.runID, r)
			}
			if r.Agents != 2 {
				t.Fatalf("sig log row %+v, want the run's 2 agents", r)
			}
		}
	}
	if !found {
		t.Fatalf("sig log does not list the CLI run %s:\n%s", f.runID, logBuf.String())
	}
}

func TestCLIParkRejectLandsNothingAndKeepsBranches(t *testing.T) {
	f := newCLIParkFixture(t)
	ctx := context.Background()
	before := f.head()

	out, err := f.reject("not this week")
	if err != nil {
		t.Fatalf("sig reject %s: %v", f.runID, err)
	}
	if out.Status != statusRejected {
		t.Fatalf("reject outcome %+v, want status=%s", out, statusRejected)
	}
	if got := f.head(); got != before {
		t.Fatalf("main moved to %s on a reject (was %s); a rejection lands nothing", short(got), short(before))
	}
	if st := f.status(); st != statusRejected {
		t.Fatalf("status %q after reject, want %s", st, statusRejected)
	}
	after := f.reread()
	if after.RejectReason != "not this week" || after.ResolvedAt == "" {
		t.Fatalf("rejection not recorded: %+v", after)
	}
	// A rejection is a decision not to land, never a decision to destroy work.
	for _, b := range f.park.branches() {
		if _, err := f.g.RevParse(ctx, b); err != nil {
			t.Fatalf("branch %s was destroyed by a reject: %v", b, err)
		}
	}
	if _, err := f.ack(); err == nil {
		t.Fatal("an ack succeeded after a reject; rejected is terminal")
	}
	assertEvent(t, f.dir, "reject")
}

// ---- acceptance #2: the moved-base path is identical for a CLI park ----

func TestCLIParkAckReverifiesWhenTheBaseMoved(t *testing.T) {
	f := newCLIParkFixture(t)
	ctx := context.Background()
	moved := f.moveBase("package main\n\nfunc extra() int { return 7 }\n")

	out, err := f.ack()
	if err != nil {
		t.Fatalf("sig ack %s: %v", f.runID, err)
	}
	if !out.Reverified || out.Attempts != 2 {
		t.Fatalf("ack outcome %+v, want a re-verify recorded as attempt 2", out)
	}
	if out.LandedSHA == f.park.VerifiedSHA {
		t.Fatalf("ack landed the stale commit %s; a moved base must rebuild from the fork point", short(out.LandedSHA))
	}
	if got := f.head(); got != out.LandedSHA {
		t.Fatalf("main is at %s, want the re-verified commit %s", short(got), short(out.LandedSHA))
	}
	// The landed tree carries BOTH the held change and what moved the base —
	// re-integration merges each branch against its own fork point, so it never
	// reverts what landed meanwhile.
	if _, present, err := f.g.BlobAt(ctx, "main", "auth/token.go"); err != nil || !present {
		t.Fatalf("the re-verified landing lost auth/token.go (err=%v)", err)
	}
	if _, present, err := f.g.BlobAt(ctx, "main", "extra.go"); err != nil || !present {
		t.Fatalf("the re-verified landing reverted the commit that moved the base (err=%v)", err)
	}
	if anc, err := f.g.IsAncestor(ctx, moved, out.LandedSHA); err != nil || !anc {
		t.Fatalf("the landing does not descend from the moved base %s (err=%v)", short(moved), err)
	}
	after := f.reread()
	if len(after.Attempts) != 2 || !after.Attempts[1].VerifyOK || after.LandedSHA != out.LandedSHA {
		t.Fatalf("re-verify attempt not recorded: %+v", after.Attempts)
	}
	assertEvent(t, f.dir, "repark")
}

// TestCLIParkAckStaysParkedWhenTheReverifyIsRed is the refusal half: the base
// moved to a tree the parked branches cannot verify against, so nothing lands
// and the park stays open with the failure attached — never the stale tree.
func TestCLIParkAckStaysParkedWhenTheReverifyIsRed(t *testing.T) {
	f := newCLIParkFixture(t)
	// A commit that cannot compile is a CONSTRUCTED red, not a timing-dependent
	// one: `go build ./...` fails on this tree every time, on every machine.
	broken := f.moveBase("package main\n\nthis is not go\n")

	out, err := f.ack()
	if err != nil {
		t.Fatalf("sig ack %s: %v", f.runID, err)
	}
	if out.Status != statusAwaitingAck || !out.Reverified {
		t.Fatalf("ack outcome %+v, want a red re-verify leaving the run %s", out, statusAwaitingAck)
	}
	if out.LandedSHA != "" {
		t.Fatalf("a red re-verify landed %s; it must land nothing", short(out.LandedSHA))
	}
	if got := f.head(); got != broken {
		t.Fatalf("main is at %s, want the moved base %s untouched", short(got), short(broken))
	}
	if st := f.status(); st != statusAwaitingAck {
		t.Fatalf("status %q after a red re-verify, want %s", st, statusAwaitingAck)
	}
	after := f.reread()
	if len(after.Attempts) != 2 || after.Attempts[1].VerifyOK {
		t.Fatalf("red attempt not recorded: %+v", after.Attempts)
	}
	if after.ResolvedAt != "" || after.LandedSHA != "" {
		t.Fatalf("a red re-verify resolved the park: %+v", after)
	}
	// The park is still ackable, and its keep-alive ref still pins the tree.
	if _, err := f.g.RevParse(context.Background(), after.KeepRef); err != nil {
		t.Fatalf("keep-alive ref %s released while the park is still open: %v", after.KeepRef, err)
	}
}

// ---- acceptance #3: sig gc protects a CLI park like a serve park ----

func TestCLIParkSurvivesGCEvenForced(t *testing.T) {
	f := newCLIParkFixture(t)
	ctx := context.Background()
	// -older-than in the NEGATIVE puts the cutoff in the future, so every branch
	// and every ref is past the age gate: the attack the protection must survive.
	plan, err := gcPlanFor(ctx, f.g, -time.Hour, true)
	if err != nil {
		t.Fatalf("gcPlanFor: %v", err)
	}
	held := f.park.branches()
	if len(held) == 0 {
		t.Fatal("the fixture parked no branches")
	}
	for _, b := range held {
		if !plan.Parked[b] {
			t.Fatalf("parked branch %s is not recorded as park-protected: %+v", b, plan)
		}
		if contains(plan.ToDelete, b) {
			t.Fatalf("gc -force would delete the parked branch %s: %+v", b, plan.ToDelete)
		}
		if !contains(plan.ToKeep, b) {
			t.Fatalf("parked branch %s is not in the keep set: %+v", b, plan.ToKeep)
		}
	}
	// The keep-alive ref pins the only copy of the verified landing, so it must
	// not read as stranded debris while the park is open.
	if contains(plan.ParkRefs, f.park.KeepRef) {
		t.Fatalf("gc would sweep the open park's keep-alive ref %s: %+v", f.park.KeepRef, plan.ParkRefs)
	}
	// Contrast, and the proof the run dir carries a report: this run is
	// UNRESOLVED (it is parked), so its report.json still manifest-protects the
	// branch its clean group landed, and -force is what reaches that one —
	// exactly the protection a parked branch does not get and does not need.
	if !plan.Forced["agent/clean"] {
		t.Fatalf("agent/clean was not manifest-protected by the run dir's report.json: %+v", plan)
	}
}

// TestCompletedCLIRunDoesNotPinItsBranches: giving `sig run` a run directory
// made it journal report.json into the ledger the way `sig serve` always has,
// and manifest protection keys on that file. It must not follow that every CLI
// run anyone has ever completed pins its agent branches forever — that would
// leave `sig gc -delete` with nothing to sweep and make -force mandatory, which
// is the opposite of a tool for cleaning up after completed and crashed runs.
// A run that FINISHED protects nothing; the negative control is the same repo
// with nothing changed but the recorded phase.
func TestCompletedCLIRunDoesNotPinItsBranches(t *testing.T) {
	requirePOSIXShell(t)
	// Negative -older-than puts the cutoff in the future so the just-created
	// branch is past the age gate; that also means never scanning the real
	// os.TempDir() (see newCLIParkFixture).
	origTempRoot := gcTempRoot
	gcRoot := t.TempDir()
	gcTempRoot = func() string { return gcRoot }
	t.Cleanup(func() { gcTempRoot = origTempRoot })

	ctx := context.Background()
	g, repo := makeGoRepo(t)
	rep, code, out := runRunJSON(t, repo, buildTestAgent(t), []taskSpec{
		taskWrite(t, "clean", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}),
	})
	if code != exitOK {
		t.Fatalf("exit code %d, want exitOK (%d)\n%s", code, exitOK, out)
	}
	if len(rep.PerAgent) != 1 || rep.PerAgent[0].Branch == "" {
		t.Fatalf("report names no agent branch to sweep: %+v", rep.PerAgent)
	}
	branch := rep.PerAgent[0].Branch
	if _, err := g.RevParse(ctx, branch); err != nil {
		t.Fatalf("%s does not exist, so this test would pass vacuously: %v", branch, err)
	}
	if st, _ := diskRunStatus(runDirOf(t, g, rep.RunID)); st != "done" {
		t.Fatalf("a clean `sig run` recorded phase %q, want done", st)
	}

	plan, err := gcPlanFor(ctx, g, -time.Hour, false)
	if err != nil {
		t.Fatalf("gcPlanFor: %v", err)
	}
	if !contains(plan.ToDelete, branch) {
		t.Fatalf("`sig gc -delete` (no -force) would keep %s from a completed run: ToDelete=%v ToKeep=%v", branch, plan.ToDelete, plan.ToKeep)
	}

	// Negative control: nothing about the repo changes except the recorded
	// phase. An unresolved run is exactly what -resume reads, so the same
	// branch must flip back to kept, and back to reachable only with -force.
	writeRunStatus(runDirOf(t, g, rep.RunID), "interrupted", "")
	plan, err = gcPlanFor(ctx, g, -time.Hour, false)
	if err != nil {
		t.Fatalf("gcPlanFor: %v", err)
	}
	if !contains(plan.ToKeep, branch) || contains(plan.ToDelete, branch) {
		t.Fatalf("an interrupted run stopped protecting %s: ToDelete=%v ToKeep=%v", branch, plan.ToDelete, plan.ToKeep)
	}
	if plan, err = gcPlanFor(ctx, g, -time.Hour, true); err != nil || !plan.Forced[branch] {
		t.Fatalf("-force must still reach %s (err=%v): %+v", branch, err, plan)
	}
}

// ---- acceptance #4: a CLI park is in a daemon's inbox ----

func TestCLIParkAppearsInDaemonInbox(t *testing.T) {
	f := newCLIParkFixture(t)
	_, ts := newTestServer(t, "", f.repo)

	var inbox struct {
		Entries []inboxEntry `json:"entries"`
	}
	if code := doJSON(t, "GET", ts.URL+"/inbox?type="+inboxParked, "", nil, &inbox); code != http.StatusOK {
		t.Fatalf("GET /inbox status %d", code)
	}
	var entry *inboxEntry
	for i := range inbox.Entries {
		if inbox.Entries[i].RunID == f.runID {
			entry = &inbox.Entries[i]
		}
	}
	if entry == nil {
		t.Fatalf("the CLI park %s is not in the daemon's inbox: %+v", f.runID, inbox.Entries)
	}
	if entry.Reason != parkReasonAckPaths || len(entry.MatchedPaths) == 0 {
		t.Fatalf("inbox entry %+v, want an ack-paths park with its triggering paths", *entry)
	}
	if !contains(entry.Branches, "agent/held") {
		t.Fatalf("inbox entry names branches %v, want agent/held", entry.Branches)
	}
	// `parked` is the inbox's only ACTIONABLE type, so the entry has to be
	// actionable: the daemon acks a run it never started.
	var out ackOutcome
	if code := doJSON(t, "POST", ts.URL+entry.Links["ack"], "", nil, &out); code != http.StatusOK {
		t.Fatalf("POST %s status %d", entry.Links["ack"], code)
	}
	if out.LandedSHA != f.park.VerifiedSHA {
		t.Fatalf("the daemon's ack landed %s, want the CLI park's verifiedSHA %s", short(out.LandedSHA), short(f.park.VerifiedSHA))
	}
	if got := f.head(); got != f.park.VerifiedSHA {
		t.Fatalf("main is at %s, want %s", short(got), short(f.park.VerifiedSHA))
	}
}

// ---- crash recovery, without a daemon ----

// TestCLIRunHealsAStaleRunDir: a run dir left saying "running" by a killed
// process is healed at the start of the next `sig run`. `sig serve` does this at
// newServer, but a repo that never runs a daemon has no other moment to do it —
// and every `sig run` now leaves a dir that a Ctrl-C could strand that way.
func TestCLIRunHealsAStaleRunDir(t *testing.T) {
	requireUnixProcessSemantics(t)
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	c, err := cell.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	runsDir, err := cellRunsDir(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(runsDir, "20200101T000000Z-000000000000")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunStatusAsPID(t, stale, "running", deadPID(t))

	runRunJSON(t, repo, agent, []taskSpec{
		taskWrite(t, "t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}),
	})

	st, note := diskRunStatus(stale)
	if st != "interrupted" {
		t.Fatalf("stale run dir still reads %q (note %q), want interrupted", st, note)
	}
}

// ---- the human handle ----

// TestRunSummaryNamesTheAckCommand pins the one line that makes a parked CLI run
// releasable by a person: the human summary must name the run id and both
// commands. Without it the terse output of a parked run is indistinguishable
// from an ordinary flagged one, and the run id lives only in -json.
func TestRunSummaryNamesTheAckCommand(t *testing.T) {
	rep := runReport{
		RunID: "20260101T000000Z-abcdef012345",
		Repo:  "/tmp/repo",
		Base:  "main",
		Park: &parkJSON{
			VerifiedSHA: strings.Repeat("a", 40),
			BaseSHA:     strings.Repeat("b", 40),
			Reason:      parkReasonAckPaths,
			Groups:      []parkGroupJSON{{Branches: []string{"agent/held"}}},
		},
	}
	var buf bytes.Buffer
	if err := writeRunSummary(&buf, rep); err != nil {
		t.Fatalf("writeRunSummary: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"parked", "agent/held", "sig ack " + rep.RunID, "sig reject " + rep.RunID, rep.Repo} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary does not mention %q:\n%s", want, got)
		}
	}
	// A run that did NOT park says nothing about acking.
	var plain bytes.Buffer
	if err := writeRunSummary(&plain, runReport{RunID: rep.RunID, Repo: rep.Repo, Base: "main"}); err != nil {
		t.Fatalf("writeRunSummary: %v", err)
	}
	if strings.Contains(plain.String(), "sig ack") {
		t.Fatalf("an unparked run's summary offers an ack:\n%s", plain.String())
	}
}
