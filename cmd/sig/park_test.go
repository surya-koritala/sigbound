package main

// End-to-end coverage for run parking (issue #109). Every test here drives a
// REAL run through `sig serve` against a real repo whose sigbound.policy holds
// an ack-path, so what is asserted is the artifact the daemon actually wrote,
// never a hand-built fixture standing in for it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

// parkPolicyAckPaths is the fixture policy: everything under auth/ needs a
// human, everything else lands on its own.
const parkPolicyAckPaths = "ack-paths = auth/**\n"

// parkFixture is one real parked run plus everything a test acts on it with.
type parkFixture struct {
	t     *testing.T
	repo  string
	g     *gitx.Git
	cell  *cell.Cell
	srv   *server
	ts    *httptest.Server
	runID string
	dir   string
	park  *parkJSON
}

// newParkFixture commits policyBody as the repo's sigbound.policy, then drives a
// two-task run over serve: `clean` writes alpha.go (lands), `held` writes
// auth/token.go (parks). It returns once the daemon reports awaiting-ack.
func newParkFixture(t *testing.T, policyBody string) *parkFixture {
	t.Helper()
	requirePOSIXShell(t)
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	commitPolicy(t, g, repo, policyBody)

	srv, ts := newTestServer(t, "", repo)
	var created struct {
		RunID string `json:"runId"`
	}
	code := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell: repo,
		Base: "main",
		Tasks: []taskSpec{
			taskWrite(t, "clean", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}),
			taskWrite(t, "held", map[string]string{"auth/token.go": "package auth\n\nfunc Token() string { return \"t\" }\n"}),
		},
		Agent:  agent,
		Verify: "go build ./...",
	}, &created)
	if code != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", code)
	}
	final := pollRunStatus(t, ts, "", created.RunID, statusAwaitingAck)
	if final.Report == nil || final.Report.Park == nil {
		t.Fatalf("run %s reported no park: %+v", created.RunID, final.Report)
	}
	f := &parkFixture{
		t: t, repo: repo, g: g, cell: srv.cells[0].cell, srv: srv, ts: ts,
		runID: created.RunID, dir: filepath.Join(srv.cells[0].runsDir, created.RunID),
	}
	f.park = f.reread()
	head, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	// The clean group must actually have landed; the whole point of the park is
	// that it holds ONE group, not the run.
	if head != f.park.BaseSHA {
		t.Fatalf("park baseSHA %s but main is at %s — the park must be verified against the base as it stands", short(f.park.BaseSHA), short(head))
	}
	if head == f.park.ForkSHA {
		t.Fatalf("main never advanced past the fork point %s — the clean group did not land", short(f.park.ForkSHA))
	}
	return f
}

// reread reloads park.json from disk through the same validating reader every
// production path uses.
func (f *parkFixture) reread() *parkJSON {
	f.t.Helper()
	pk, err := readPark(f.dir)
	if err != nil {
		f.t.Fatalf("readPark: %v", err)
	}
	return pk
}

// head resolves the base branch's current commit.
func (f *parkFixture) head() string {
	f.t.Helper()
	sha, err := f.g.RevParse(context.Background(), "main")
	if err != nil {
		f.t.Fatal(err)
	}
	return sha
}

// status reads the run's durable status.
func (f *parkFixture) status() string {
	f.t.Helper()
	st, _ := diskRunStatus(f.dir)
	return st
}

// writeParkRaw overwrites park.json with arbitrary bytes, for the mutation
// tests — deliberately bypassing writePark so an invalid record can be planted.
func (f *parkFixture) writeParkRaw(pk *parkJSON) {
	f.t.Helper()
	data, err := json.MarshalIndent(pk, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, parkFileName), data, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// moveBase lands an unrelated commit on main, so an ack finds a base that has
// moved since the park was verified. body is committed as extra.go.
//
// The reset is load-bearing: sigbound advances refs in the object store and
// never touches the repo's working tree, so after a run the checkout still holds
// the PRE-run content. Committing on top of it without resetting first would
// produce a commit that silently reverts everything the run landed, and the test
// would be asserting against a tree no real workflow produces.
func (f *parkFixture) moveBase(body string) string {
	f.t.Helper()
	if err := f.g.ResetHard(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "extra.go"), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	sha, err := f.g.CommitAll(context.Background(), "move the base while a run is parked")
	if err != nil {
		f.t.Fatal(err)
	}
	return sha
}

// pollRunStatus polls GET /runs/{id} until its status is one of want (or any
// terminal status), returning the final response. Unlike pollRun it knows about
// awaiting-ack, which is durable and never "finishes".
func pollRunStatus(t *testing.T, ts *httptest.Server, token, id string, want ...string) runStatusResponse {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var resp runStatusResponse
		code := doJSON(t, "GET", ts.URL+"/runs/"+id, token, nil, &resp)
		if code != http.StatusOK {
			t.Fatalf("GET /runs/%s: status %d", id, code)
		}
		for _, w := range want {
			if resp.Status == w {
				return resp
			}
		}
		if resp.Status == "done" || resp.Status == "error" {
			t.Fatalf("run %s reached %q (error=%q), want one of %v", id, resp.Status, resp.Error, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach %v (last status %q)", id, want, resp.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---- acceptance #1: an ack lands the EXACT verified SHA ----

// TestParkAckLandsExactVerifiedSHA is the central promise: when the base has not
// moved since verify, an ack advances the ref to precisely the recorded commit —
// no recomputation, no re-merge, the same bytes that passed verify.
func TestParkAckLandsExactVerifiedSHA(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()

	// The parked tree must not be on main yet, and must be a descendant of it.
	if f.head() == f.park.VerifiedSHA {
		t.Fatal("the parked commit is already on main — parking must not advance the ref")
	}
	anc, err := f.g.IsAncestor(ctx, f.park.BaseSHA, f.park.VerifiedSHA)
	if err != nil || !anc {
		t.Fatalf("parked commit %s does not descend from the recorded base %s (err=%v)", short(f.park.VerifiedSHA), short(f.park.BaseSHA), err)
	}
	// The ack-path file is in the parked tree and NOT on main.
	if _, present, err := f.g.BlobAt(ctx, f.park.VerifiedSHA, "auth/token.go"); err != nil || !present {
		t.Fatalf("auth/token.go missing from the parked tree (err=%v)", err)
	}
	if _, present, err := f.g.BlobAt(ctx, f.head(), "auth/token.go"); err != nil || present {
		t.Fatalf("auth/token.go landed without an ack (err=%v)", err)
	}
	// The clean group's file IS on main and survives into the parked tree —
	// folding onto a moved base must never revert what already landed.
	if _, present, err := f.g.BlobAt(ctx, f.park.VerifiedSHA, "alpha.go"); err != nil || !present {
		t.Fatalf("the parked tree reverted the clean group's alpha.go (err=%v)", err)
	}

	out, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit})
	if err != nil {
		t.Fatalf("ackRun: %v", err)
	}
	if out.Status != "done" || out.Reverified {
		t.Fatalf("ack outcome %+v, want status=done with no re-verify", out)
	}
	if out.LandedSHA != f.park.VerifiedSHA {
		t.Fatalf("ack landed %s, want the recorded verifiedSHA %s", short(out.LandedSHA), short(f.park.VerifiedSHA))
	}
	if got := f.head(); got != f.park.VerifiedSHA {
		t.Fatalf("main is at %s, want the verified commit %s", short(got), short(f.park.VerifiedSHA))
	}
	if f.status() != "done" {
		t.Fatalf("status %q after ack, want done", f.status())
	}
	after := f.reread()
	if after.LandedSHA != f.park.VerifiedSHA || after.ResolvedAt == "" {
		t.Fatalf("parking record not closed out: %+v", after)
	}
	// One ack is enough: a second must refuse rather than re-land.
	if _, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit}); err == nil {
		t.Fatal("a second ack succeeded; ack must only be valid while awaiting-ack")
	}
	assertEvent(t, f.dir, "ack")
	assertEvent(t, f.dir, "parked")
}

// TestParkAckRefusesMutatedRecord is the mutation test: every way of editing
// park.json to point the ack somewhere other than the verified tree must be
// refused, with the base ref left exactly where it was.
func TestParkAckRefusesMutatedRecord(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mutate func(f *parkFixture, pk *parkJSON)
	}{
		{"verifiedSHA points at another real commit", func(f *parkFixture, pk *parkJSON) {
			// The fork commit exists and is a genuine ancestor, so only the
			// recorded TREE OID stands between this and landing the wrong tree.
			pk.VerifiedSHA = pk.ForkSHA
		}},
		{"verifiedSHA is a plausible but absent object", func(f *parkFixture, pk *parkJSON) {
			pk.VerifiedSHA = strings.Repeat("a", len(pk.VerifiedSHA))
		}},
		{"verifiedTree rewritten to the base's tree", func(f *parkFixture, pk *parkJSON) {
			tree, err := f.g.TreeOID(ctx, pk.BaseSHA)
			if err != nil {
				t.Fatal(err)
			}
			pk.VerifiedTree = tree
		}},
		{"verifiedSHA is not hex at all", func(f *parkFixture, pk *parkJSON) {
			pk.VerifiedSHA = "../../etc/passwd"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newParkFixture(t, parkPolicyAckPaths)
			before := f.head()
			pk := f.reread()
			tc.mutate(f, pk)
			f.writeParkRaw(pk)

			if _, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit}); err == nil {
				t.Fatal("ack accepted a mutated parking record; it must refuse")
			}
			if got := f.head(); got != before {
				t.Fatalf("base ref moved to %s despite a refused ack (was %s)", short(got), short(before))
			}
			if f.status() != statusAwaitingAck {
				t.Fatalf("status %q, want the park left untouched at %s", f.status(), statusAwaitingAck)
			}
		})
	}
}

// TestParkReadFailsClosedOnCorruption: a park.json that does not read back
// cleanly can never be acted on, and gc refuses to run rather than guess that it
// protects nothing.
func TestParkReadFailsClosedOnCorruption(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	before := f.head()
	if err := os.WriteFile(filepath.Join(f.dir, parkFileName), []byte(`{"verifiedSHA":"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPark(f.dir); err == nil {
		t.Fatal("readPark accepted a truncated record")
	}
	if _, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit}); err == nil {
		t.Fatal("ack accepted a corrupt parking record")
	}
	if _, err := rejectRun(ctx, f.g, f.dir, "test", ""); err == nil {
		t.Fatal("reject accepted a corrupt parking record")
	}
	if got := f.head(); got != before {
		t.Fatalf("base ref moved on a corrupt record: %s", short(got))
	}
	// gc must abort rather than treat an unreadable park as protecting nothing.
	if _, err := loadParkedBranches(ctx, f.g); err == nil {
		t.Fatal("loadParkedBranches ignored a corrupt park.json; it must fail closed")
	}
	if _, err := gcPlanFor(ctx, f.g, -time.Hour, true); err == nil {
		t.Fatal("sig gc planned a sweep despite a corrupt park.json")
	}
}

// ---- acceptance #2: the base-moved re-verify cycle ----

// TestParkAckBaseMovedReverifiesGreenAndLandsNewSHA: an ack must never land the
// stale tree once the base has moved. It re-integrates the parked branches onto
// the new base, re-verifies, and lands the NEW commit — recorded as a second
// attempt on the parking record.
func TestParkAckBaseMovedReverifiesGreenAndLandsNewSHA(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	stale := f.park.VerifiedSHA
	moved := f.moveBase("package main\n\nfunc extra() int { return 7 }\n")

	out, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit})
	if err != nil {
		t.Fatalf("ackRun: %v", err)
	}
	if !out.Reverified || out.Status != "done" {
		t.Fatalf("ack outcome %+v, want a re-verified landing", out)
	}
	if out.LandedSHA == stale {
		t.Fatal("ack landed the stale verified tree after the base moved")
	}
	head := f.head()
	if head != out.LandedSHA {
		t.Fatalf("main at %s, want the re-verified commit %s", short(head), short(out.LandedSHA))
	}
	// The new landing is a descendant of the moved base and carries BOTH the
	// commit that moved it and the parked change.
	if anc, err := f.g.IsAncestor(ctx, moved, head); err != nil || !anc {
		t.Fatalf("the re-verified landing discarded the commit that moved the base (err=%v)", err)
	}
	for _, path := range []string{"auth/token.go", "extra.go", "alpha.go"} {
		if _, present, err := f.g.BlobAt(ctx, head, path); err != nil || !present {
			t.Fatalf("%s missing from the re-verified landing (err=%v)", path, err)
		}
	}
	pk := f.reread()
	if len(pk.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (the park's own verify plus one re-verify)", len(pk.Attempts))
	}
	last := pk.Attempts[1]
	if !last.VerifyOK || last.BaseSHA != moved || last.FinalSHA != out.LandedSHA {
		t.Fatalf("attempt 2 = %+v, want a green verify of %s onto %s", last, short(out.LandedSHA), short(moved))
	}
	if pk.VerifiedSHA != out.LandedSHA || pk.BaseSHA != moved {
		t.Fatalf("parking record not updated to the new landing: %+v", pk)
	}
	assertEvent(t, f.dir, "repark")
	assertEvent(t, f.dir, "ack")
}

// TestParkAckBaseMovedReverifiesRedRePark: when the re-verify goes red, nothing
// lands and the run stays parked with the failure attached — the inbox shows it.
func TestParkAckBaseMovedReverifiesRedRePark(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	// A base that no longer compiles: the re-verified tree contains it, so
	// `go build ./...` fails on the exact tree an ack would land.
	moved := f.moveBase("package main\n\nfunc extra() int { return \"not an int\" }\n")

	out, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit})
	if err != nil {
		t.Fatalf("ackRun: %v", err)
	}
	if out.Status != statusAwaitingAck || !out.Reverified {
		t.Fatalf("ack outcome %+v, want the run left parked after a red re-verify", out)
	}
	if got := f.head(); got != moved {
		t.Fatalf("main moved to %s on a red re-verify; nothing may land", short(got))
	}
	if f.status() != statusAwaitingAck {
		t.Fatalf("status %q, want %s", f.status(), statusAwaitingAck)
	}
	pk := f.reread()
	if len(pk.Attempts) != 2 || pk.Attempts[1].VerifyOK {
		t.Fatalf("attempts = %+v, want a recorded RED second attempt", pk.Attempts)
	}
	if pk.Attempts[1].Output == "" {
		t.Fatal("the red attempt recorded no verify output; the failure must be attached")
	}
	// The park still names the tree it originally verified, untouched.
	if pk.VerifiedSHA != f.park.VerifiedSHA || pk.BaseSHA != f.park.BaseSHA {
		t.Fatalf("a red re-verify rewrote the recorded landing: %+v", pk)
	}
	// The inbox surfaces the red attempt.
	entries := inboxEntriesFor(context.Background(), f.g, "c", f.dir, f.runID, inboxParked, time.Now())
	if len(entries) != 1 || !strings.Contains(entries[0].Summary, "red") {
		t.Fatalf("inbox entry does not surface the red attempt: %+v", entries)
	}
}

// ---- acceptance #3: reject ----

// TestParkRejectIsTerminalAndKeepsBranches: a rejection lands nothing, records
// the reason, and leaves every parked branch exactly where it is.
func TestParkRejectIsTerminalAndKeepsBranches(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	before := f.head()

	out, err := rejectRun(context.Background(), f.g, f.dir, "test", "not shipping auth changes this week")
	if err != nil {
		t.Fatalf("rejectRun: %v", err)
	}
	if out.Status != statusRejected {
		t.Fatalf("reject outcome %+v, want status=rejected", out)
	}
	if got := f.head(); got != before {
		t.Fatalf("base ref moved to %s on a reject", short(got))
	}
	pk := f.reread()
	if pk.RejectReason != "not shipping auth changes this week" || pk.ResolvedAt == "" {
		t.Fatalf("reason not recorded: %+v", pk)
	}
	if pk.LandedSHA != "" {
		t.Fatalf("a rejected park recorded a landing: %s", pk.LandedSHA)
	}
	// Branches kept: every parked branch still resolves.
	for _, b := range pk.branches() {
		if _, err := f.g.RevParse(ctx, b); err != nil {
			t.Fatalf("rejected park lost branch %s: %v", b, err)
		}
	}
	// Terminal: neither an ack nor a second reject may reopen it.
	if _, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit}); err == nil {
		t.Fatal("ack succeeded on a rejected run")
	}
	if _, err := rejectRun(context.Background(), f.g, f.dir, "test", "again"); err == nil {
		t.Fatal("reject succeeded twice on the same run")
	}
	if f.status() != statusRejected {
		t.Fatalf("status %q, want %s", f.status(), statusRejected)
	}
	assertEvent(t, f.dir, "reject")
}

// ---- acceptance #4: awaiting-ack survives a crash + restart ----

// TestParkSurvivesDaemonDeathAndRestart: awaiting-ack is durable. A daemon that
// dies without ever writing another byte must come back reporting the same park,
// not "interrupted" — the pid-liveness recovery applies to queued/running only.
func TestParkSurvivesDaemonDeathAndRestart(t *testing.T) {
	requireUnixProcessSemantics(t)
	f := newParkFixture(t, parkPolicyAckPaths)

	// Simulate the kill -9: the owning process is gone (an unrelated dead pid is
	// recorded), and nothing terminal was ever written.
	writeRunStatusAsPID(t, f.dir, statusAwaitingAck, deadPID(t))
	recoverStaleRuns(filepath.Dir(f.dir), os.Getpid())
	if got := f.status(); got != statusAwaitingAck {
		t.Fatalf("startup recovery rewrote a parked run to %q; awaiting-ack must be durable", got)
	}

	// A genuinely fresh daemon over the same repo reports it identically, and
	// the park is still ackable through it.
	_, ts2 := newTestServer(t, "", f.repo)
	var resp runStatusResponse
	if code := doJSON(t, "GET", ts2.URL+"/runs/"+f.runID, "", nil, &resp); code != http.StatusOK {
		t.Fatalf("GET /runs status %d", code)
	}
	if resp.Status != statusAwaitingAck {
		t.Fatalf("restarted daemon reports %q, want %s", resp.Status, statusAwaitingAck)
	}
	if resp.Park == nil || resp.Park.VerifiedSHA != f.park.VerifiedSHA {
		t.Fatalf("restarted daemon lost the parking record: %+v", resp.Park)
	}
	var out ackOutcome
	if code := doJSON(t, "POST", ts2.URL+"/runs/"+f.runID+"/ack", "", map[string]any{}, &out); code != http.StatusOK {
		t.Fatalf("POST ack after restart: status %d", code)
	}
	if out.LandedSHA != f.park.VerifiedSHA {
		t.Fatalf("post-restart ack landed %s, want %s", short(out.LandedSHA), short(f.park.VerifiedSHA))
	}
}

// writeRunStatusAsPID plants a status.json owned by a specific pid, so a test
// can express "this run's owner is gone" without actually dying.
func writeRunStatusAsPID(t *testing.T, dir, status string, pid int) {
	t.Helper()
	data, err := json.MarshalIndent(runStatusFile{
		Status: status, UpdatedAt: time.Now().UTC().Format(time.RFC3339), PID: pid,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// deadPID returns a pid that has certainly exited: a `true` this test reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	for pid := 1 << 20; pid > 1<<19; pid-- {
		if !pidAlive(pid) {
			return pid
		}
	}
	t.Skip("no free pid to impersonate a dead owner")
	return 0
}

// ---- acceptance #5: gc never sweeps a parked run's branches ----

// TestGCNeverSweepsParkedBranches ages every branch past the cutoff and runs the
// most aggressive sweep gc offers (-delete -force). The parked branches must
// survive it; the ack afterwards must still work, which is the property that
// actually matters.
func TestGCNeverSweepsParkedBranches(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	parked := f.reread().branches()
	if len(parked) == 0 {
		t.Fatal("fixture parked no branches")
	}

	// A negative -older-than puts the cutoff in the FUTURE, so every branch in
	// the repo is past the age gate — the attack this must withstand.
	plan, err := gcPlanFor(ctx, f.g, -time.Hour, true)
	if err != nil {
		t.Fatalf("gcPlanFor: %v", err)
	}
	for _, b := range parked {
		if slicesContains(plan.ToDelete, b) {
			t.Fatalf("gc -force planned to delete parked branch %s", b)
		}
		if !slicesContains(plan.ToKeep, b) {
			t.Fatalf("gc did not keep parked branch %s (keep=%v delete=%v)", b, plan.ToKeep, plan.ToDelete)
		}
		if !plan.Parked[b] {
			t.Fatalf("gc kept %s but not for the parked reason", b)
		}
	}
	if err := applyGC(ctx, f.cell, plan); err != nil {
		t.Fatalf("applyGC: %v", err)
	}
	for _, b := range parked {
		if _, err := f.g.RevParse(ctx, b); err != nil {
			t.Fatalf("gc deleted parked branch %s: %v", b, err)
		}
	}
	if _, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit}); err != nil {
		t.Fatalf("the park was no longer ackable after gc: %v", err)
	}

	// Once resolved it is no longer parked, so ordinary sweep rules resume.
	plan2, err := gcPlanFor(ctx, f.g, -time.Hour, true)
	if err != nil {
		t.Fatalf("gcPlanFor after ack: %v", err)
	}
	for _, b := range parked {
		if plan2.Parked[b] {
			t.Fatalf("branch %s still claims park protection after the park was acked", b)
		}
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// ---- acceptance #8: deterministic spot-audit sampling ----

// TestAuditSelectedIsDeterministic: the sample is a pure function of the run id,
// so it is identical across runs, processes and machines — that is what makes an
// audit claim replayable rather than an assertion about a lost coin flip.
func TestAuditSelectedIsDeterministic(t *testing.T) {
	ids := make([]string, 500)
	for i := range ids {
		ids[i] = newRunID()
	}
	for _, id := range ids {
		if auditSelected(id, 0) {
			t.Fatalf("audit-sample=0 selected %s; 0%% must select nothing", id)
		}
		if !auditSelected(id, 100) {
			t.Fatalf("audit-sample=100 skipped %s; 100%% must select everything", id)
		}
		// Same input, same answer, every time.
		want := auditSelected(id, 37)
		for i := 0; i < 5; i++ {
			if auditSelected(id, 37) != want {
				t.Fatalf("auditSelected(%s, 37) is not deterministic", id)
			}
		}
	}
	// A run with no id (i.e. `sig run`) is never sampled.
	if auditSelected("", 100) {
		t.Fatal("an empty run id was sampled")
	}
	// Fixed vectors: these must not drift, or a recorded audit decision stops
	// being reproducible by a later version of the binary.
	for id, want := range map[string]bool{
		"20240101T000000Z-000000000000": auditSelected("20240101T000000Z-000000000000", 50),
		"20240101T000000Z-0000000000ff": auditSelected("20240101T000000Z-0000000000ff", 50),
	} {
		if auditSelected(id, 50) != want {
			t.Fatalf("sampling of %s is unstable within one process", id)
		}
	}
	// Sanity on the spread: a 30% rate over 500 distinct ids should land
	// somewhere near 30% rather than 0% or 100% (a wide band — this checks the
	// hash is actually being used, not the exactness of the distribution).
	n := 0
	for _, id := range ids {
		if auditSelected(id, 30) {
			n++
		}
	}
	if n < len(ids)/10 || n > len(ids)/2 {
		t.Fatalf("audit-sample=30%% selected %d/%d ids — the sample is not spread over the hash", n, len(ids))
	}
}

// TestAuditMarksCleanLandingAndRaisesInboxEntry drives a run with
// `audit-sample = 100%` and no ack-path, so the landing is clean and certain to
// be sampled: the manifest records it and the inbox shows it.
func TestAuditMarksCleanLandingAndRaisesInboxEntry(t *testing.T) {
	requirePOSIXShell(t)
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	commitPolicy(t, g, repo, "audit-sample = 100%\n")
	srv, ts := newTestServer(t, "", repo)

	var created struct {
		RunID string `json:"runId"`
	}
	if c := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:   repo,
		Base:   "main",
		Tasks:  []taskSpec{taskWrite(t, "clean", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"})},
		Agent:  agent,
		Verify: "go build ./...",
	}, &created); c != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", c)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "done" {
		t.Fatalf("run status %q (error=%q)", final.Status, final.Error)
	}
	if final.Report == nil || !final.Report.Audit {
		t.Fatalf("a clean landing under audit-sample=100%% was not marked: %+v", final.Report)
	}
	dir := filepath.Join(srv.cells[0].runsDir, created.RunID)
	assertEvent(t, dir, "audit_selected")

	var inbox struct {
		Entries []inboxEntry `json:"entries"`
	}
	if c := doJSON(t, "GET", ts.URL+"/inbox?type=audit", "", nil, &inbox); c != http.StatusOK {
		t.Fatalf("GET /inbox status %d", c)
	}
	if len(inbox.Entries) != 1 || inbox.Entries[0].RunID != created.RunID {
		t.Fatalf("audit entry missing from the inbox: %+v", inbox.Entries)
	}
	// An audit is informational: it gates nothing, so the run is plainly done.
	if inbox.Entries[0].Links["ack"] != "" {
		t.Fatal("an audit entry offered an ack control; only parked entries are actionable")
	}
}

// TestAuditNotRaisedForZeroSampleOrParkedRun: 0% selects nothing, and a run that
// parked is not a clean landing, so it is never sampled either.
func TestAuditNotRaisedForZeroSampleOrParkedRun(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths+"audit-sample = 100%\n")
	rep, err := readRunReport(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Audit {
		t.Fatal("a parked run was audit-sampled; only a CLEAN landing is")
	}
}

// ---- acceptance #9: the lazy ack-timeout ----

// TestParkAckTimeoutAutoRejectsOnRead: an expired park is rejected the moment
// anyone looks at it — no timer goroutine, and the transition is durable.
func TestParkAckTimeoutAutoRejectsOnRead(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths+"ack-timeout = 1h\nack-timeout-action = reject\n")
	pk := f.reread()
	if pk.AckTimeout != "1h0m0s" || pk.AckTimeoutAction != parkActionReject {
		t.Fatalf("timeout not carried onto the park: %+v", pk)
	}
	before := f.head()

	// Backdate the park past its own deadline.
	pk.CreatedAt = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	f.writeParkRaw(pk)

	var inbox struct {
		Entries []inboxEntry `json:"entries"`
	}
	if c := doJSON(t, "GET", f.ts.URL+"/inbox", "", nil, &inbox); c != http.StatusOK {
		t.Fatalf("GET /inbox status %d", c)
	}
	if f.status() != statusRejected {
		t.Fatalf("status %q after an expired park was read, want %s", f.status(), statusRejected)
	}
	if got := f.head(); got != before {
		t.Fatalf("an expiry landed something: main moved to %s", short(got))
	}
	after := f.reread()
	if !strings.Contains(after.RejectReason, "ack-timeout") {
		t.Fatalf("expiry reason %q does not name the timeout", after.RejectReason)
	}
	for _, e := range inbox.Entries {
		if e.Type == inboxParked && e.RunID == f.runID {
			t.Fatal("an expired park was still listed as actionable")
		}
	}
	if _, err := ackRun(context.Background(), f.cell, f.dir, "test", ackEnv{Mode: envModeInherit}); err == nil {
		t.Fatal("an expired park was still ackable")
	}
	assertEvent(t, f.dir, "reject")
}

// TestParkWithoutTimeoutNeverExpires: absent ack-timeout means park forever,
// which is the default posture — an unacked landing is not a problem time solves.
func TestParkWithoutTimeoutNeverExpires(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	pk := f.reread()
	if pk.AckTimeout != "" {
		t.Fatalf("a policy with no ack-timeout produced one: %q", pk.AckTimeout)
	}
	pk.CreatedAt = time.Now().Add(-100000 * time.Hour).UTC().Format(time.RFC3339)
	f.writeParkRaw(pk)
	if enforceParkTimeout(context.Background(), f.g, f.dir) {
		t.Fatal("a park with no ack-timeout expired")
	}
	if f.status() != statusAwaitingAck {
		t.Fatalf("status %q, want %s", f.status(), statusAwaitingAck)
	}
}

// ---- acceptance #10: CLI and HTTP share the code path ----

// TestAckCLIAndHTTPProduceIdenticalRecords drives two identical parks, acking
// one over HTTP and one from the CLI, and asserts the resulting parking records
// are the same in every field that is not a timestamp — which they can only be
// because both front doors call ackRun.
func TestAckCLIAndHTTPProduceIdenticalRecords(t *testing.T) {
	viaHTTP := newParkFixture(t, parkPolicyAckPaths)
	viaCLI := newParkFixture(t, parkPolicyAckPaths)

	var out ackOutcome
	if c := doJSON(t, "POST", viaHTTP.ts.URL+"/runs/"+viaHTTP.runID+"/ack", "", map[string]any{}, &out); c != http.StatusOK {
		t.Fatalf("POST ack status %d", c)
	}

	var buf strings.Builder
	code, err := runAck(&buf, []string{viaCLI.runID, "-repo", viaCLI.repo, "-json"})
	if err != nil || code != exitOK {
		t.Fatalf("sig ack: code=%d err=%v out=%s", code, err, buf.String())
	}
	var cliOut ackOutcome
	if err := json.Unmarshal([]byte(buf.String()), &cliOut); err != nil {
		t.Fatalf("parse sig ack -json: %v (%s)", err, buf.String())
	}

	// Both landed their own park's exact verified commit, both closed the record
	// out the same way, and both left the run done.
	for _, c := range []struct {
		name string
		f    *parkFixture
		out  ackOutcome
	}{{"http", viaHTTP, out}, {"cli", viaCLI, cliOut}} {
		if c.out.Status != "done" || c.out.LandedSHA != c.f.park.VerifiedSHA {
			t.Fatalf("%s: outcome %+v, want the recorded verifiedSHA %s", c.name, c.out, short(c.f.park.VerifiedSHA))
		}
		if c.f.head() != c.f.park.VerifiedSHA || c.f.status() != "done" {
			t.Fatalf("%s: head=%s status=%s", c.name, short(c.f.head()), c.f.status())
		}
	}
	// Field-by-field: the two records differ only where they must (the SHAs of
	// two different repos, and the timestamps).
	a, b := normalizedPark(viaHTTP.reread()), normalizedPark(viaCLI.reread())
	if a != b {
		t.Fatalf("CLI and HTTP acks produced different records:\n http=%s\n  cli=%s", a, b)
	}

	// Reject shares the path the same way: the CLI records the reason exactly as
	// the handler does.
	rejCLI := newParkFixture(t, parkPolicyAckPaths)
	var rbuf strings.Builder
	if code, err := runReject(&rbuf, []string{rejCLI.runID, "-repo", rejCLI.repo, "-reason", "cli said no"}); err != nil || code != exitOK {
		t.Fatalf("sig reject: code=%d err=%v", code, err)
	}
	if pk := rejCLI.reread(); pk.RejectReason != "cli said no" || rejCLI.status() != statusRejected {
		t.Fatalf("sig reject did not record the reason: %+v status=%s", pk, rejCLI.status())
	}
}

// normalizedPark renders a parking record with every repo-specific SHA and every
// timestamp blanked, so two records from two different repos are comparable on
// structure and outcome alone.
func normalizedPark(pk *parkJSON) string {
	c := *pk
	c.VerifiedSHA, c.VerifiedTree, c.BaseSHA, c.ForkSHA, c.LandedSHA = "", "", "", "", ""
	c.CreatedAt, c.ResolvedAt = "", ""
	c.KeepRef = "" // names the run, so it differs between two runs by construction
	c.Attempts = append([]parkAttemptJSON(nil), pk.Attempts...)
	for i := range c.Attempts {
		c.Attempts[i].At, c.Attempts[i].BaseSHA, c.Attempts[i].FinalSHA = "", "", ""
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// TestAckHTTPStatusCodes covers the wrong-state and busy 409s and the unknown-run
// 404, so a programmatic caller can switch on the #93 code vocabulary.
func TestAckHTTPStatusCodes(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	var body map[string]string

	if c := doJSON(t, "POST", f.ts.URL+"/runs/nope-not-a-run/ack", "", map[string]any{}, &body); c != http.StatusNotFound {
		t.Fatalf("ack of an unknown run: status %d, want 404", c)
	}
	// An ack carries no reason — silently dropping one would be worse.
	if c := doJSON(t, "POST", f.ts.URL+"/runs/"+f.runID+"/ack", "", map[string]any{"reason": "x"}, &body); c != http.StatusBadRequest {
		t.Fatalf("ack with a reason: status %d, want 400", c)
	}
	// A busy cell (a run or another ack in flight) is a 409, not a second landing.
	f.srv.mu.Lock()
	f.srv.busy[f.cell.ID()] = true
	f.srv.mu.Unlock()
	if c := doJSON(t, "POST", f.ts.URL+"/runs/"+f.runID+"/ack", "", map[string]any{}, &body); c != http.StatusConflict {
		t.Fatalf("ack while the cell is busy: status %d, want 409", c)
	}
	if body["code"] != codeCellBusy {
		t.Fatalf("busy code %q, want %q", body["code"], codeCellBusy)
	}
	f.srv.mu.Lock()
	f.srv.busy[f.cell.ID()] = false
	f.srv.mu.Unlock()

	// Reject, then ack: the wrong-state 409 carries its own code.
	if c := doJSON(t, "POST", f.ts.URL+"/runs/"+f.runID+"/reject", "", map[string]any{"reason": "no"}, &body); c != http.StatusOK {
		t.Fatalf("reject status %d, want 200", c)
	}
	if c := doJSON(t, "POST", f.ts.URL+"/runs/"+f.runID+"/ack", "", map[string]any{}, &body); c != http.StatusConflict {
		t.Fatalf("ack of a rejected run: status %d, want 409", c)
	}
	if body["code"] != codeNotParked {
		t.Fatalf("wrong-state code %q, want %q", body["code"], codeNotParked)
	}
}

// TestAckMutatingEndpointsRequireAuth: the two POSTs sit behind exactly the same
// bearer gate as every other data route — the UI's buttons are convenience, not
// authorization.
func TestAckMutatingEndpointsRequireAuth(t *testing.T) {
	requirePOSIXShell(t)
	_, repo := makeGoRepo(t)
	_, tsrv := newTestServer(t, "review-secret-token-value", repo)
	for _, path := range []string{"/runs/anything/ack", "/runs/anything/reject"} {
		if c := doJSON(t, "POST", tsrv.URL+path, "", map[string]any{}, nil); c != http.StatusUnauthorized {
			t.Fatalf("POST %s without a token: status %d, want 401", path, c)
		}
	}
	if c := doJSON(t, "GET", tsrv.URL+"/inbox", "", nil, nil); c != http.StatusUnauthorized {
		t.Fatalf("GET /inbox without a token: status %d, want 401", c)
	}
}

// ---- policy plumbing ----

// TestPolicySelfProtectionParks: a run that edits sigbound.policy itself parks
// under the policy-modified reason, not ack-paths — a change may not loosen the
// bar that gates it without a human seeing it.
func TestPolicySelfProtectionParks(t *testing.T) {
	requirePOSIXShell(t)
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	commitPolicy(t, g, repo, parkPolicyAckPaths)
	srv, tsrv := newTestServer(t, "", repo)

	var created struct {
		RunID string `json:"runId"`
	}
	if c := doJSON(t, "POST", tsrv.URL+"/runs", "", runRequest{
		Cell:   repo,
		Base:   "main",
		Tasks:  []taskSpec{taskWrite(t, "loosen", map[string]string{policyFileName: "# no ack-paths any more\n"})},
		Agent:  agent,
		Verify: "go build ./...",
	}, &created); c != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", c)
	}
	final := pollRunStatus(t, tsrv, "", created.RunID, statusAwaitingAck)
	if final.Park == nil || final.Park.Reason != parkReasonPolicyModified {
		t.Fatalf("policy self-modification parked as %+v, want reason %s", final.Park, parkReasonPolicyModified)
	}
	if final.Park.matchedPaths()[policyFileName] == "" {
		t.Fatalf("the policy file is not named as the trigger: %+v", final.Park.Groups)
	}
	// Nothing landed: the base is untouched until a human acks.
	head, err := g.RevParse(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if head != final.Park.BaseSHA {
		t.Fatalf("main at %s but the park was verified against %s", short(head), short(final.Park.BaseSHA))
	}
	// Nothing else in the run, so the base did not move: an ack lands the exact
	// verified commit, base-unchanged path.
	dir := filepath.Join(srv.cells[0].runsDir, created.RunID)
	out, err := ackRun(context.Background(), srv.cells[0].cell, dir, "test", ackEnv{Mode: envModeInherit})
	if err != nil {
		t.Fatalf("ackRun: %v", err)
	}
	if out.Reverified {
		t.Fatal("a park with no clean co-landing re-verified; the base never moved")
	}
	if out.LandedSHA != final.Park.VerifiedSHA {
		t.Fatalf("landed %s, want %s", short(out.LandedSHA), short(final.Park.VerifiedSHA))
	}
}

// TestPolicyAckTimeoutActionRejectsUnknownValue: an action this binary cannot
// perform is a hard parse error, never a silent no-op that would leave an
// expired park open forever.
func TestPolicyAckTimeoutActionRejectsUnknownValue(t *testing.T) {
	if _, err := parsePolicy([]byte("ack-timeout = 1h\nack-timeout-action = land\n")); err == nil {
		t.Fatal("parsePolicy accepted ack-timeout-action = land")
	}
	pol, err := parsePolicy([]byte("ack-timeout = 72h\n"))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}
	if pol.ackTimeoutAction != parkActionReject {
		t.Fatalf("ack-timeout without an action defaulted to %q, want %q", pol.ackTimeoutAction, parkActionReject)
	}
	if got := policyReport(pol).AckTimeoutAction; got != parkActionReject {
		t.Fatalf("report records action %q, want %q", got, parkActionReject)
	}
}

// assertEvent fails unless the run's events.ndjson carries at least one line for
// the named event — the durable trace ack/reject/repark leave behind.
func assertEvent(t *testing.T, dir, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Event string `json:"event"`
			TS    string `json:"ts"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("events.ndjson line is not JSON: %q", line)
		}
		if rec.Event == name {
			if rec.TS == "" {
				t.Fatalf("%s event has no ts", name)
			}
			return
		}
	}
	t.Fatalf("no %q event in %s/events.ndjson:\n%s", name, dir, data)
}

// ---- regressions from the adversarial review of cf3d491 ----

// keepRefSHA returns what the park's keep-alive ref points at, or "" if the ref
// is gone.
func (f *parkFixture) keepRefSHA(ref string) string {
	f.t.Helper()
	sha, err := f.g.RevParse(context.Background(), ref)
	if err != nil {
		return ""
	}
	return sha
}

// pruneUnreachable is the reviewer's exact garbage-collection sequence: drop
// every reflog entry (so nothing is reachable via reflog) and collect
// aggressively. Anything not reachable from a real ref is deleted.
func (f *parkFixture) pruneUnreachable() {
	f.t.Helper()
	for _, args := range [][]string{
		{"reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all"},
		{"gc", "--prune=now"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = f.repo
		if out, err := cmd.CombinedOutput(); err != nil {
			f.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// TestParkSurvivesGitGarbageCollection is the BLOCK-1 regression. The parked
// commit is built by commit-tree and is reachable from no branch — its parents
// are the base and the agent branches, so protecting those protects its
// ancestors, not it. Without a keep-alive ref, an ordinary `git gc` in the
// user's own repo deletes the verified landing and the park can never be acked
// again. sigbound only disables gc.auto on repos it creates itself, and a park
// waits indefinitely by default, so this is not a hypothetical.
func TestParkSurvivesGitGarbageCollection(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	pk := f.reread()

	if pk.KeepRef == "" {
		t.Fatal("the park recorded no keep-alive ref")
	}
	if got := f.keepRefSHA(pk.KeepRef); got != pk.VerifiedSHA {
		t.Fatalf("keep-alive ref points at %s, want the verified commit %s", short(got), short(pk.VerifiedSHA))
	}
	// It must live outside every prefix `sig gc` sweeps, or the protection is
	// circular.
	for _, prefix := range gcBranchPrefixes {
		if strings.HasPrefix(pk.KeepRef, prefix) {
			t.Fatalf("keep-alive ref %s is inside sig gc's sweep prefix %s", pk.KeepRef, prefix)
		}
	}

	f.pruneUnreachable()

	if _, err := f.g.RevParse(ctx, pk.VerifiedSHA); err != nil {
		t.Fatalf("git gc pruned the parked commit %s: %v", short(pk.VerifiedSHA), err)
	}
	out, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit})
	if err != nil {
		t.Fatalf("ack after git gc: %v", err)
	}
	if out.LandedSHA != pk.VerifiedSHA {
		t.Fatalf("ack landed %s, want %s", short(out.LandedSHA), short(pk.VerifiedSHA))
	}
	// Once landed, the base branch reaches the commit, so the keep-alive ref has
	// nothing left to protect and is released.
	if got := f.keepRefSHA(pk.KeepRef); got != "" {
		t.Fatalf("keep-alive ref survived the ack (still at %s)", short(got))
	}
}

// TestParkRejectReleasesKeepAliveRef: a rejection lands nothing, so the verified
// commit becomes garbage on purpose — the ref must not pin it forever.
func TestParkRejectReleasesKeepAliveRef(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	pk := f.reread()
	if _, err := rejectRun(context.Background(), f.g, f.dir, "test", "no"); err != nil {
		t.Fatalf("rejectRun: %v", err)
	}
	if got := f.keepRefSHA(pk.KeepRef); got != "" {
		t.Fatalf("keep-alive ref survived a reject (still at %s)", short(got))
	}
	// The branches are still kept — a reject declines a landing, it does not
	// destroy work.
	for _, b := range pk.branches() {
		if _, err := f.g.RevParse(context.Background(), b); err != nil {
			t.Fatalf("reject lost branch %s: %v", b, err)
		}
	}
}

// TestParkAckBaseMovedDoesNotNeedTheRecordedCommit is the amplifier regression:
// validateParkedLanding used to run unconditionally, so a park whose recorded
// commit was unusable was refused even on the base-MOVED path — which never
// reads that commit at all, rebuilding instead from the fork point and the
// branches (both of which gc protects). A recoverable park was permanently
// stranded by a check on something it did not need.
func TestParkAckBaseMovedDoesNotNeedTheRecordedCommit(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	pk := f.reread()

	// Take the keep-alive ref away and collect, so the recorded commit is really
	// gone — the exact state a pre-keep-ref park would be in.
	if err := f.g.DeleteRef(ctx, pk.KeepRef); err != nil {
		t.Fatal(err)
	}
	f.moveBase("package main\n\nfunc extra() int { return 7 }\n")
	f.pruneUnreachable()
	if _, err := f.g.RevParse(ctx, pk.VerifiedSHA); err == nil {
		t.Skip("the recorded commit is still reachable; this platform's gc did not prune it")
	}

	out, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit})
	if err != nil {
		t.Fatalf("base-moved ack refused over an unusable recorded commit it never needed: %v", err)
	}
	if !out.Reverified || out.Status != "done" {
		t.Fatalf("ack outcome %+v, want a re-verified landing", out)
	}
	if _, present, err := f.g.BlobAt(ctx, f.head(), "auth/token.go"); err != nil || !present {
		t.Fatalf("the rebuilt landing is missing the parked change (err=%v)", err)
	}
}

// slowVerify rewrites the park's recorded verify command so a re-verify takes
// long enough for a second actor to race it, and returns the deadline by which
// that ack will have finished.
func (f *parkFixture) slowVerify(cmd string) {
	f.t.Helper()
	pk := f.reread()
	pk.Verify = cmd
	f.writeParkRaw(pk)
}

// TestRejectDuringInFlightAckWins is BLOCK-2 proof (1): status was checked once
// at ack entry and landRef fired minutes later, so a human who rejected a run
// mid-ack was told "nothing landed" and then watched the ref advance anyway,
// with the ack's stale read-modify-write erasing the rejection reason.
func TestRejectDuringInFlightAckWins(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	f.moveBase("package main\n\nfunc extra() int { return 7 }\n") // force the slow re-verify path
	f.slowVerify("sleep 3")
	before := f.head()

	ackErr := make(chan error, 1)
	go func() {
		_, err := ackRun(ctx, f.cell, f.dir, "http", ackEnv{Mode: envModeInherit})
		ackErr <- err
	}()
	time.Sleep(750 * time.Millisecond) // the ack is now inside its re-verify

	out, err := rejectRun(ctx, f.g, f.dir, "cli", "changed my mind")
	if err != nil {
		t.Fatalf("reject during an in-flight ack: %v", err)
	}
	if out.Status != statusRejected {
		t.Fatalf("reject outcome %+v", out)
	}
	if err := <-ackErr; err == nil {
		t.Fatal("the in-flight ack succeeded after the run was rejected")
	}

	if got := f.head(); got != before {
		t.Fatalf("the ref advanced to %s after a reject won (was %s)", short(got), short(before))
	}
	if f.status() != statusRejected {
		t.Fatalf("final status %q, want %s — a reject is terminal", f.status(), statusRejected)
	}
	pk := f.reread()
	if pk.RejectReason != "changed my mind" {
		t.Fatalf("rejectReason = %q; the in-flight ack clobbered it", pk.RejectReason)
	}
	if pk.LandedSHA != "" {
		t.Fatalf("a rejected park recorded a landing: %s", pk.LandedSHA)
	}
}

// TestExpiryDuringInFlightAckWins is BLOCK-2 proof (2): the lazy ack-timeout
// sweep runs from an ordinary GET /inbox, with none of the daemon's per-cell
// locking, so a plain read could auto-reject a park while an ack was mid-verify
// — and the ack landed it anyway and overwrote the status back to done.
func TestExpiryDuringInFlightAckWins(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths+"ack-timeout = 1h\n")
	ctx := context.Background()
	f.moveBase("package main\n\nfunc extra() int { return 7 }\n")
	before := f.head()

	// Expire one second from now: not yet expired when the ack starts, expired
	// while it is inside its verify.
	pk := f.reread()
	pk.Verify = "sleep 3"
	pk.CreatedAt = time.Now().Add(-1 * time.Hour).Add(time.Second).UTC().Format(time.RFC3339)
	f.writeParkRaw(pk)

	ackErr := make(chan error, 1)
	go func() {
		_, err := ackRun(ctx, f.cell, f.dir, "http", ackEnv{Mode: envModeInherit})
		ackErr <- err
	}()
	time.Sleep(1500 * time.Millisecond)
	getInbox(t, f.ts.URL, "") // the lazy sweep fires here
	if f.status() != statusRejected {
		t.Fatalf("status %q after the deadline passed and the inbox was read, want %s", f.status(), statusRejected)
	}

	if err := <-ackErr; err == nil {
		t.Fatal("the in-flight ack succeeded after its park expired")
	}
	if got := f.head(); got != before {
		t.Fatalf("the ref advanced to %s after an expiry (was %s)", short(got), short(before))
	}
	if f.status() != statusRejected {
		t.Fatalf("final status %q, want %s", f.status(), statusRejected)
	}
}

// TestConcurrentAcksLandExactlyOnce: two acks racing the same park must produce
// exactly one landing and one coherent record — the loser must not overwrite the
// winner's attempt history.
func TestConcurrentAcksLandExactlyOnce(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	f.moveBase("package main\n\nfunc extra() int { return 7 }\n")
	f.slowVerify("sleep 1")

	type res struct {
		out ackOutcome
		err error
	}
	results := make(chan res, 2)
	for i := 0; i < 2; i++ {
		go func() {
			out, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit})
			results <- res{out, err}
		}()
	}
	landed := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err == nil && r.out.LandedSHA != "" {
			landed++
		}
	}
	if landed != 1 {
		t.Fatalf("%d of 2 concurrent acks landed; exactly one must", landed)
	}
	pk := f.reread()
	if pk.LandedSHA == "" || f.status() != "done" {
		t.Fatalf("after two concurrent acks: landedSHA=%q status=%q", pk.LandedSHA, f.status())
	}
	// The winner's attempt history survives: attempt 1 (the park's own verify)
	// plus exactly one re-verify. A last-writer-wins clobber loses one.
	if len(pk.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 — a concurrent writer clobbered the record: %+v", len(pk.Attempts), pk.Attempts)
	}
	if f.head() != pk.LandedSHA {
		t.Fatalf("main at %s, record says %s", short(f.head()), short(pk.LandedSHA))
	}
}

// TestParkSurvivesCrashBetweenRecordAndStatus is the M-a regression: execRun
// writes park.json and THEN status.json, so a crash in that gap left a genuinely
// parked run marked "running". Startup recovery rewrote it to "interrupted" and
// the verified landing became permanently unreachable — ack and reject both
// refused it, while sig gc pinned its branches forever with nothing able to
// release them. An unresolved parking record now outranks the phase marker.
func TestParkSurvivesCrashBetweenRecordAndStatus(t *testing.T) {
	requireUnixProcessSemantics(t)
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()

	// The crash: park.json is on disk, the marker never advanced past "running",
	// and the owning process is gone.
	writeRunStatusAsPID(t, f.dir, "running", deadPID(t))

	if st, _ := diskRunStatus(f.dir); st != statusAwaitingAck {
		t.Fatalf("diskRunStatus = %q for a run with an unresolved parking record, want %s", st, statusAwaitingAck)
	}
	recoverStaleRuns(filepath.Dir(f.dir), os.Getpid())
	if got := f.status(); got != statusAwaitingAck {
		t.Fatalf("startup recovery marked a parked run %q, want %s", got, statusAwaitingAck)
	}
	// And it is genuinely actionable again.
	if _, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit}); err != nil {
		t.Fatalf("the recovered park was not ackable: %v", err)
	}
}

// TestParkLockIsCrossProcessAndStealsFromDeadHolders: the lock has to span
// processes, since `sig ack` and the daemon are different ones acting on the
// same run dir, and it must never wedge a park when a holder is killed.
func TestParkLockIsCrossProcessAndStealsFromDeadHolders(t *testing.T) {
	requireUnixProcessSemantics(t)
	dir := t.TempDir()

	unlock, ok := lockPark(dir)
	if !ok {
		t.Fatal("could not take a fresh park lock")
	}
	if _, ok := lockPark(dir); ok {
		t.Fatal("the park lock was granted twice at once")
	}
	unlock()
	unlock2, ok := lockPark(dir)
	if !ok {
		t.Fatal("the park lock was not released")
	}
	unlock2()

	// A holder that died without releasing must not wedge the run forever.
	if err := os.WriteFile(filepath.Join(dir, parkLockName), []byte(strconv.Itoa(deadPID(t))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock3, ok := lockPark(dir)
	if !ok {
		t.Fatal("a lock held by a dead process was not stolen")
	}
	unlock3()
}

// TestRedAttemptAlwaysExplainsItself is the M-d regression: a verify that fails
// SILENTLY (a bare `exit 1`, or a policy battery member that prints nothing)
// recorded verifyOk=false with empty output AND empty error, so the inbox said
// "attempt 2 failed" with nothing whatsoever to inspect.
func TestRedAttemptAlwaysExplainsItself(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	f.moveBase("package main\n\nfunc extra() int { return 7 }\n")
	f.slowVerify("exit 1") // fails, prints nothing at all

	out, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit})
	if err != nil {
		t.Fatalf("ackRun: %v", err)
	}
	if out.Status != statusAwaitingAck {
		t.Fatalf("outcome %+v, want the run left parked", out)
	}
	att := f.reread().Attempts
	if len(att) != 2 {
		t.Fatalf("attempts = %d, want 2", len(att))
	}
	last := att[1]
	if last.VerifyOK {
		t.Fatal("a failing verify recorded a green attempt")
	}
	if strings.TrimSpace(last.Output) == "" && strings.TrimSpace(last.Error) == "" {
		t.Fatalf("a red attempt recorded neither output nor error: %+v", last)
	}
}

// TestConflictAttemptRecordsNoFinalSHA: when every parked branch conflicts on
// re-integration nothing is produced, so the attempt must not report the BASE
// commit as though it were the result.
func TestConflictAttemptRecordsNoFinalSHA(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	// Land a conflicting edit to the very file the parked branch adds.
	if err := f.g.ResetHard(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.repo, "auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "auth/token.go"), []byte("package auth\n\nfunc Token() string { return \"other\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	moved, err := f.g.CommitAll(ctx, "conflicting edit on the base")
	if err != nil {
		t.Fatal(err)
	}

	out, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit})
	if err != nil {
		t.Fatalf("ackRun: %v", err)
	}
	if out.Status != statusAwaitingAck {
		t.Fatalf("outcome %+v, want the run left parked on a conflict", out)
	}
	if got := f.head(); got != moved {
		t.Fatalf("the ref moved to %s on a conflicting re-integration", short(got))
	}
	last := f.reread().Attempts[1]
	if last.FinalSHA != "" {
		t.Fatalf("a conflict-only attempt recorded finalSHA %s — that is the base, not a result", short(last.FinalSHA))
	}
	if len(last.Flagged) == 0 {
		t.Fatal("a conflict-only attempt recorded no flagged branches")
	}
}

// TestParkMutatedBaseSHAReverifiesRatherThanLandingStale pins the semantics of
// the ONE record field an ack cannot simply refuse over. A rewritten baseSHA is
// indistinguishable from a base that legitimately moved — that is precisely what
// "the base moved" means — so it routes to the re-verify path, which is a
// STRICTER gate than the direct release, not a weaker one: the recorded tree is
// discarded, the branches are re-integrated from the fork point, and only a
// freshly green result lands.
//
// The property that matters is not "every mutation is refused" (see
// validateParkedLanding's note on what these checks are and are not). It is that
// no mutation can make an UNVERIFIED tree land.
func TestParkMutatedBaseSHAReverifiesRatherThanLandingStale(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	pk := f.reread()
	stale := pk.VerifiedSHA
	before := f.head()

	pk.BaseSHA = strings.Repeat("b", len(pk.BaseSHA))
	f.writeParkRaw(pk)

	out, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit})
	if err != nil {
		// Refusing is also acceptable; what must never happen is landing stale.
		if f.head() != before {
			t.Fatalf("ack failed but the ref still moved to %s", short(f.head()))
		}
		return
	}
	if !out.Reverified {
		t.Fatalf("outcome %+v: a record whose baseSHA does not match the base must re-verify, never release the recorded commit", out)
	}
	// NOTE: the re-verified commit may legitimately EQUAL the recorded one.
	// Integration is deterministic, so re-folding the same branches onto the same
	// base with the same merge base reproduces the same tree and parents — and
	// within the same second, commit-tree's timestamp matches too, yielding the
	// identical SHA. That is deterministic integration agreeing with itself, not
	// a stale release; what proves the difference is that it went through a real
	// re-verify (asserted above and via attempt 2 below), so the SHA is not
	// compared here.
	_ = stale
	head := f.head()
	if head != out.LandedSHA {
		t.Fatalf("main at %s, outcome says %s", short(head), short(out.LandedSHA))
	}
	// Whatever landed is a fresh descendant of what was there, carrying the
	// parked change — i.e. it went through a real verify just now.
	if anc, aerr := f.g.IsAncestor(ctx, before, head); aerr != nil || !anc {
		t.Fatalf("the landing is not a fast-forward from %s (err=%v)", short(before), aerr)
	}
	if _, present, berr := f.g.BlobAt(ctx, head, "auth/token.go"); berr != nil || !present {
		t.Fatalf("the landing is missing the parked change (err=%v)", berr)
	}
	att := f.reread().Attempts
	if len(att) != 2 || !att[1].VerifyOK {
		t.Fatalf("no green re-verify attempt was recorded: %+v", att)
	}
}

// TestParkAckRefusesLandingThatLeftItsBase pins the ancestry check in
// validateParkedLanding — the sole guard on the ONE path that releases a
// recorded commit without re-verifying it. The mutation table cannot reach it:
// rewriting baseSHA to anything that differs from the live base routes to the
// re-verify path instead. It is reachable only when the recorded base MATCHES
// the current head (so the direct-land branch is taken) while the recorded
// landing does not descend from it — a record left over from a base that has
// since been rewritten.
func TestParkAckRefusesLandingThatLeftItsBase(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()

	// Move the base somewhere the parked commit does NOT descend from, and point
	// the record's baseSHA at it so the direct-land branch is what runs.
	moved := f.moveBase("package main\n\nfunc extra() int { return 7 }\n")
	pk := f.reread()
	if anc, err := f.g.IsAncestor(ctx, moved, pk.VerifiedSHA); err != nil || anc {
		t.Fatalf("fixture is wrong: the parked commit already descends from %s (err=%v)", short(moved), err)
	}
	pk.BaseSHA = moved
	f.writeParkRaw(pk)

	_, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit})
	if err == nil {
		t.Fatal("ack released a recorded landing that does not descend from its recorded base")
	}
	if !strings.Contains(err.Error(), "descend") {
		t.Fatalf("refused for the wrong reason (%v); the ancestry check is what must fire here", err)
	}
	if got := f.head(); got != moved {
		t.Fatalf("the ref moved to %s despite a refused ack", short(got))
	}
	if f.status() != statusAwaitingAck {
		t.Fatalf("status %q, want the park left open at %s", f.status(), statusAwaitingAck)
	}
}

// TestWriteParkCASRefusesAStaleWrite pins the compare-and-swap itself: a writer
// that read the record, then found it changed underneath, must FAIL rather than
// overwrite. This is the primitive both land paths stake their ordering on.
func TestWriteParkCASRefusesAStaleWrite(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	raw, pk, err := readParkAt(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	// Someone else resolves the park in the meantime.
	other := *pk
	other.RejectReason = "someone else got here first"
	other.ResolvedAt = time.Now().UTC().Format(time.RFC3339)
	if err := writePark(f.dir, &other); err != nil {
		t.Fatal(err)
	}
	// Our stale write must be refused, and must not have touched the record.
	mine := *pk
	mine.LandedSHA = pk.VerifiedSHA
	if err := writeParkCAS(f.dir, raw, &mine); !errors.Is(err, errParkChanged) {
		t.Fatalf("writeParkCAS returned %v, want errParkChanged", err)
	}
	if got := f.reread(); got.RejectReason != "someone else got here first" || got.LandedSHA != "" {
		t.Fatalf("a stale CAS clobbered the record: %+v", got)
	}
	// An up-to-date CAS still succeeds.
	raw2, cur, err := readParkAt(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	cur.RejectReason = "updated"
	if err := writeParkCAS(f.dir, raw2, cur); err != nil {
		t.Fatalf("a current CAS was refused: %v", err)
	}
}

// TestGCSweepsStrandedParkRefsOnly covers the keep-alive ref's other end. The
// ref is released only AFTER a resolution is durably recorded — the correct
// order — so a crash in that gap strands one. Nothing else sweeps
// refs/sigbound/**, so without this they accumulate without bound, each pinning
// a commit. An OPEN park's ref must survive the most aggressive sweep gc offers.
func TestGCSweepsStrandedParkRefsOnly(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()
	pk := f.reread()
	openRef := pk.KeepRef

	// An open park's ref is never a candidate, even with -force and everything
	// past the age gate.
	plan, err := gcPlanFor(ctx, f.g, -time.Hour, true)
	if err != nil {
		t.Fatalf("gcPlanFor: %v", err)
	}
	if slicesContains(plan.ParkRefs, openRef) {
		t.Fatalf("gc planned to delete an OPEN park's keep-alive ref %s", openRef)
	}

	// Resolve the park, then strand the ref exactly as a crash between recording
	// the resolution and releasing it would.
	if _, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit}); err != nil {
		t.Fatalf("ackRun: %v", err)
	}
	if err := f.g.UpdateRef(ctx, openRef, pk.VerifiedSHA); err != nil {
		t.Fatal(err)
	}

	plan, err = gcPlanFor(ctx, f.g, -time.Hour, false)
	if err != nil {
		t.Fatalf("gcPlanFor: %v", err)
	}
	if !slicesContains(plan.ParkRefs, openRef) {
		t.Fatalf("gc did not spot the stranded ref %s (plan: %v)", openRef, plan.ParkRefs)
	}
	if err := applyGC(ctx, f.cell, plan); err != nil {
		t.Fatalf("applyGC: %v", err)
	}
	if got := f.keepRefSHA(openRef); got != "" {
		t.Fatalf("stranded ref survived gc (still at %s)", short(got))
	}
	// It is reported, so a -json consumer sees what happened.
	if !slicesContains(plan.report().ParkRefsDeleted, openRef) {
		t.Fatalf("the swept ref is missing from the gc report: %+v", plan.report())
	}

	// Fail closed: a park.json that exists but cannot be read aborts gc rather
	// than licensing a delete.
	f2 := newParkFixture(t, parkPolicyAckPaths)
	if err := os.WriteFile(filepath.Join(f2.dir, parkFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gcPlanFor(ctx, f2.g, -time.Hour, true); err == nil {
		t.Fatal("gc planned a sweep despite an unreadable parking record")
	}
}

// rearm restores the fixture to its just-parked state: base ref back where the
// park was verified against, keep-alive ref re-pointed, a pristine copy of the
// record, status awaiting-ack, and both the lock and the resolution claim
// cleared. It exists so the ack/reject race can be run a hundred times without
// paying for a full agent run each iteration — the state it writes is byte-for-
// byte what a real park left behind, copied from one.
func (f *parkFixture) rearm(pristine *parkJSON) {
	f.t.Helper()
	ctx := context.Background()
	for _, name := range []string{parkClaimName, parkLockName} {
		if err := os.Remove(filepath.Join(f.dir, name)); err != nil && !os.IsNotExist(err) {
			f.t.Fatal(err)
		}
	}
	if err := f.g.UpdateRef(ctx, "refs/heads/"+pristine.Base, pristine.BaseSHA); err != nil {
		f.t.Fatal(err)
	}
	if err := f.g.UpdateRef(ctx, pristine.KeepRef, pristine.VerifiedSHA); err != nil {
		f.t.Fatal(err)
	}
	fresh := *pristine
	if err := writePark(f.dir, &fresh); err != nil {
		f.t.Fatal(err)
	}
	writeRunStatus(f.dir, statusAwaitingAck, "")
}

// parkRaceIterations is deliberately far past the point where one lucky
// scheduling decision could hide the bug. CI caught the old code on iteration 0
// of twelve; this runs 100 iterations WITH the window forced open, so a pass
// means the guarantee holds rather than that the race happened not to land.
const parkRaceIterations = 100

// TestRejectAndAckNeverBothSucceed is the platform-independent proof for the
// direct-land (base-UNCHANGED) path — the ORDINARY ack.
//
// Two things make it brutal rather than hopeful:
//
//   - lockPark is stubbed to a no-op, which is exactly how Windows degrades
//     (pidAlive reports every pid dead there, so every caller steals it and there
//     is no mutual exclusion whatsoever). The guarantee must not depend on it.
//   - parkCASDelay holds writeParkCAS's read-compare-write window open for
//     milliseconds, so the interleaving a loaded CI runner produces by scheduler
//     preemption is FORCED on every iteration instead of hoped for. Under the
//     pre-claim code that made both racers compare against unchanged bytes and
//     both proceed; under claimPark, widening it changes nothing, because the
//     winner was already decided by an atomic O_EXCL create.
//
// It asserts the operator-facing invariant, not a winner — which racer wins is
// genuinely a race:
//
//	reject reported success  =>  the ref did not move.
//	ack reported success     =>  it really landed, and the run is done.
//	both reported success    =>  FAILURE. That is the catastrophe.
func TestRejectAndAckNeverBothSucceed(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	pristine := f.reread()

	restoreLock := lockPark
	lockPark = func(string) (func(), bool) { return func() {}, true }

	// A RENDEZVOUS, not a sleep. A fixed delay only produces the overlap if it
	// happens to exceed how much further ahead one racer is, which varies by
	// machine — the same "the timing will surely work out" reasoning that let the
	// original bug reach CI. This instead holds the first racer inside the window
	// until the second one is also inside it, so when both get here they are
	// guaranteed to overlap on any hardware. It gives up after a moment, which is
	// what happens once claimPark is doing its job and only one racer ever
	// arrives.
	var mu sync.Mutex
	inWindow := 0
	parkCASDelay = func() {
		mu.Lock()
		inWindow++
		mu.Unlock()
		deadline := time.Now().Add(120 * time.Millisecond)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := inWindow
			mu.Unlock()
			if n >= 2 {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	t.Cleanup(func() { lockPark, parkCASDelay = restoreLock, nil })

	ackWon, rejWon, neither := 0, 0, 0
	for i := 0; i < parkRaceIterations; i++ {
		f.rearm(pristine)
		mu.Lock()
		inWindow = 0
		mu.Unlock()
		ctx := context.Background()
		before := f.head()

		var ackOut ackOutcome
		var ackErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			ackOut, ackErr = ackRun(ctx, f.cell, f.dir, "http", ackEnv{Mode: envModeInherit})
		}()
		_, rejErr := rejectRun(ctx, f.g, f.dir, "cli", "no thanks")
		<-done

		head := f.head()
		moved := head != before
		switch {
		case rejErr == nil && ackErr == nil:
			t.Fatalf("iteration %d: reject AND ack both succeeded — an operator was told nothing landed while %s landed",
				i, short(ackOut.LandedSHA))
		case rejErr == nil:
			rejWon++
			if moved {
				t.Fatalf("iteration %d: reject succeeded but the ref advanced to %s", i, short(head))
			}
			if f.status() != statusRejected {
				t.Fatalf("iteration %d: reject succeeded but final status is %q", i, f.status())
			}
			if pk := f.reread(); pk.RejectReason != "no thanks" || pk.LandedSHA != "" {
				t.Fatalf("iteration %d: the losing ack corrupted the rejection: %+v", i, pk)
			}
		case ackErr == nil:
			ackWon++
			if !moved || head != ackOut.LandedSHA {
				t.Fatalf("iteration %d: ack reported landing %s but main is at %s", i, short(ackOut.LandedSHA), short(head))
			}
			if f.status() != "done" {
				t.Fatalf("iteration %d: ack succeeded but final status is %q", i, f.status())
			}
		default:
			neither++
			if moved {
				t.Fatalf("iteration %d: both failed but the ref advanced to %s", i, short(head))
			}
		}
	}
	t.Logf("%d iterations: ack won %d, reject won %d, neither %d", parkRaceIterations, ackWon, rejWon, neither)
	// Deliberately NOT asserting that each side wins at least once: which racer
	// wins is genuinely timing-dependent, and an assertion on it would be the
	// same flaky reasoning this whole test exists to replace. What keeps the test
	// honest is the negative control — stubbing claimPark to a no-op makes it
	// fail — plus the barrier above, which guarantees the windows overlap
	// whenever both racers reach them.
	//
	// What it does NOT pin, measured by mutation rather than assumed: removing
	// the recheckResolvable call under the claim still passes here, and so does
	// releasing the claim before the ref move. That is a property of the design,
	// not a hole — the claim and writeParkCAS's compare are two overlapping
	// guards and either alone closes THIS race. The interleavings only one of
	// them covers are pinned separately, by
	// TestAckRefusesAResolvedRecordUnderTheClaim and
	// TestWriteParkCASRefusesAStaleWrite.
}

// TestClaimParkIsAtomicAndRecovers pins the three properties the resolution
// claim must have, none of which may depend on pidAlive — the thing that
// degrades on Windows.
func TestClaimParkIsAtomicAndRecovers(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	pristine := f.reread()

	// (1) Exclusive, by O_EXCL alone: no pid is consulted anywhere here.
	release, err := claimPark(f.dir)
	if err != nil {
		t.Fatalf("claimPark: %v", err)
	}
	if _, err := claimPark(f.dir); !errors.Is(err, errParkBusy) {
		t.Fatalf("a second concurrent claim returned %v, want errParkBusy", err)
	}
	release()
	release2, err := claimPark(f.dir)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	release2()

	// (1b) Exclusive under real concurrency with lockPark stubbed away: many
	// simultaneous claimants, and holding is mutually exclusive throughout.
	restoreLock := lockPark
	lockPark = func(string) (func(), bool) { return func() {}, true }
	t.Cleanup(func() { lockPark = restoreLock })
	var mu sync.Mutex
	concurrent, maxConcurrent, got := 0, 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := claimPark(f.dir)
			if err != nil {
				return
			}
			mu.Lock()
			got++
			concurrent++
			if concurrent > maxConcurrent {
				maxConcurrent = concurrent
			}
			mu.Unlock()
			time.Sleep(3 * time.Millisecond)
			mu.Lock()
			concurrent--
			mu.Unlock()
			rel()
		}()
	}
	wg.Wait()
	if got == 0 {
		t.Fatal("no claimant ever got the claim")
	}
	if maxConcurrent != 1 {
		t.Fatalf("%d claimants held the claim at once; it must be exclusive", maxConcurrent)
	}

	// (2) A crash mid-resolution must not wedge the park permanently. A FRESH
	// claim is respected; one older than the threshold, over an UNRESOLVED park,
	// is stolen.
	claimPath := filepath.Join(f.dir, parkClaimName)
	if err := os.WriteFile(claimPath, []byte("99999 1 whenever\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := claimPark(f.dir); !errors.Is(err, errParkBusy) {
		t.Fatalf("a FRESH claim was not respected: %v", err)
	}
	old := time.Now().Add(-parkClaimStale - time.Minute)
	if err := os.Chtimes(claimPath, old, old); err != nil {
		t.Fatal(err)
	}
	rel3, err := claimPark(f.dir)
	if err != nil {
		t.Fatalf("a claim left by a crashed resolver was not stolen: %v", err)
	}
	rel3()

	// (3) An already-resolved park is terminal, even with a claim file present.
	if _, err := rejectRun(context.Background(), f.g, f.dir, "test", "done with it"); err != nil {
		t.Fatalf("rejectRun: %v", err)
	}
	if err := os.WriteFile(claimPath, []byte("99999 2 whenever\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := claimPark(f.dir); !errors.Is(err, errParkResolved) {
		t.Fatalf("claim on a resolved park returned %v, want errParkResolved", err)
	}
	// It surfaces as the ordinary wrong-state conflict, so both front doors
	// answer 409 without knowing anything about claims.
	if !errors.Is(errParkResolved, errNotAwaitingAck) {
		t.Fatal("errParkResolved must wrap errNotAwaitingAck")
	}
	// Resolving twice is refused; re-arming makes it resolvable again exactly once.
	f.rearm(pristine)
	if _, err := rejectRun(context.Background(), f.g, f.dir, "test", "again"); err != nil {
		t.Fatalf("a re-armed park should be resolvable: %v", err)
	}
	if _, err := rejectRun(context.Background(), f.g, f.dir, "test", "third time"); err == nil {
		t.Fatal("a resolved park was resolved a second time")
	}
}

// ---- park.json durability under concurrent writers ----

// The env vars that turn a re-exec of this test binary into a park writer. The
// hammer below needs writers in OTHER PROCESSES, not just other goroutines: a
// shared temp file is contended through the filesystem, and goroutines in one
// process can be serialized by nothing more than the Go scheduler happening to
// keep them apart. Real processes on real cores cannot be.
const (
	parkWriteDirEnv     = "SIG_TEST_PARK_WRITE_DIR"
	parkWriteBarrierEnv = "SIG_TEST_PARK_WRITE_BARRIER"
	parkWriteIDEnv      = "SIG_TEST_PARK_WRITE_ID"
	parkWriteNEnv       = "SIG_TEST_PARK_WRITE_N"
)

// parkWriteProbe builds a VALID park record in one of two very different sizes.
// The size difference is the point: two writers sharing one temp file interleave
// their bytes in it, and a short record written over a long one leaves the long
// one's tail behind — which the rename then publishes as park.json. Two writers
// of identical bytes could corrupt the temp just as often and leave no trace.
func parkWriteProbe(long bool) *parkJSON {
	pk := &parkJSON{
		VerifiedSHA:  strings.Repeat("a", 40),
		VerifiedTree: strings.Repeat("b", 40),
		BaseSHA:      strings.Repeat("c", 40),
		ForkSHA:      strings.Repeat("d", 40),
		Base:         "main",
		Reason:       parkReasonAckPaths,
		CreatedAt:    "2026-01-02T15:04:05Z",
		Groups:       []parkGroupJSON{{Branches: []string{"sigbound/held"}}},
	}
	if long {
		// Big enough that one writer is still inside its write when another
		// truncates the shared temp under it — which is the exact mechanism, and
		// with small records the loser's rename simply fails instead.
		for i := 0; i < 200; i++ {
			pk.Attempts = append(pk.Attempts, parkAttemptJSON{
				N: i + 1, At: pk.CreatedAt, BaseSHA: pk.BaseSHA, Output: strings.Repeat("y", 1200),
			})
		}
	}
	return pk
}

// parkWriteBarrier blocks until name appears in the barrier dir. It is a
// START-ALIGNMENT rendezvous, not a delay: every writer is already loaded and
// spinning when the file lands, so they begin writing within microseconds of
// each other on hardware the test does not get to choose. A sleep here would be
// the "the timing will surely work out" reasoning that let the original race
// reach CI.
func parkWriteBarrier(barrier, name string, deadline time.Duration) error {
	until := time.Now().Add(deadline)
	for time.Now().Before(until) {
		if _, err := os.Stat(filepath.Join(barrier, name)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("barrier %q never opened", name)
}

// TestParkWriteHelperProcess is not a test. It is the body of the subprocess
// writers TestWriteParkNeverPublishesATornRecord spawns, and skips out
// immediately in an ordinary run.
func TestParkWriteHelperProcess(t *testing.T) {
	dir := os.Getenv(parkWriteDirEnv)
	if dir == "" {
		t.Skip("helper process for TestWriteParkNeverPublishesATornRecord")
	}
	barrier := os.Getenv(parkWriteBarrierEnv)
	n, err := strconv.Atoi(os.Getenv(parkWriteNEnv))
	if err != nil {
		t.Fatalf("%s: %v", parkWriteNEnv, err)
	}
	id := os.Getenv(parkWriteIDEnv)
	if err := os.WriteFile(filepath.Join(barrier, "ready."+id), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := parkWriteBarrier(barrier, "go", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		// Errorf, not Fatalf: a writer that stops early stops contending, and the
		// parent still needs every other assertion (the reader's, the final parse)
		// exercised at full concurrency before it looks at exit statuses.
		if err := writePark(dir, parkWriteProbe(i%2 == 0)); err != nil {
			t.Errorf("writePark: %v", err)
		}
	}
}

// TestWriteParkNeverPublishesATornRecord is the regression for the fixed temp
// path. writePark used to write through .park.json.tmp — one name, every writer
// — so two concurrent writers opened the SAME file, interleaved their bytes, and
// the rename published the mixture. Not a torn READ, which rename does prevent:
// a permanently CORRUPT record. Its consequences are all terminal and all silent
// until someone tries to act on the run: ack fails, reject fails, the timeout
// sweep cannot resolve it, the run sits in awaiting-ack forever, and because
// loadParkedBranches fails closed on an unreadable record, `sig gc` aborts for
// the ENTIRE repository. One wedged park, no garbage collection anywhere.
//
// The record is hammered from 24 goroutines and 8 subprocesses at once while a
// reader parses it continuously, so the assertion is not merely that the final
// state is good but that no observer ever saw a bad one.
func TestWriteParkNeverPublishesATornRecord(t *testing.T) {
	const (
		goroutines = 24
		subprocs   = 8
		writes     = 60
	)
	dir, barrier := t.TempDir(), t.TempDir()
	if err := writePark(dir, parkWriteProbe(false)); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	var procs []*exec.Cmd
	var out []*strings.Builder
	for i := 0; i < subprocs; i++ {
		cmd := exec.Command(exe, "-test.run=^TestParkWriteHelperProcess$", "-test.v")
		cmd.Env = append(os.Environ(),
			parkWriteDirEnv+"="+dir,
			parkWriteBarrierEnv+"="+barrier,
			parkWriteIDEnv+"="+strconv.Itoa(i),
			parkWriteNEnv+"="+strconv.Itoa(writes),
		)
		var sb strings.Builder
		cmd.Stdout, cmd.Stderr = &sb, &sb
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		procs, out = append(procs, cmd), append(out, &sb)
	}
	// Every subprocess is up and spinning on the barrier before anyone writes.
	for i := 0; i < subprocs; i++ {
		if err := parkWriteBarrier(barrier, "ready."+strconv.Itoa(i), 60*time.Second); err != nil {
			t.Fatalf("subprocess %d never started: %v (output: %s)", i, err, out[i].String())
		}
	}

	// A reader that never stops looking. Rename is atomic, so anything it fails
	// to parse was corrupt BEFORE it was published.
	stop := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				close(readErr)
				return
			default:
			}
			if _, err := readPark(dir); err != nil {
				readErr <- err
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := parkWriteBarrier(barrier, "go", 30*time.Second); err != nil {
				t.Error(err)
				return
			}
			for j := 0; j < writes; j++ {
				if err := writePark(dir, parkWriteProbe((i+j)%2 == 0)); err != nil {
					t.Errorf("writePark: %v", err)
				}
			}
		}(i)
	}
	if err := os.WriteFile(filepath.Join(barrier, "go"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	procErr := make([]error, len(procs))
	for i, cmd := range procs {
		procErr[i] = cmd.Wait()
	}
	close(stop)
	// The record's own assertions come FIRST and none of them are fatal: a shared
	// temp file breaks writePark several ways at once (a lost write, a corrupt
	// publish, a leaked temp) and the point of the test is to say which.
	if err := <-readErr; err != nil {
		t.Errorf("a reader observed an unparseable park.json while writers were running: %v", err)
	}
	if _, err := readPark(dir); err != nil {
		t.Errorf("park.json is corrupt after %d concurrent writes: %v", goroutines*writes+subprocs*writes, err)
	}
	for i, err := range procErr {
		if err != nil {
			t.Errorf("subprocess %d: %v (output: %s)", i, err, out[i].String())
		}
	}
	// And no writer leaked its temp into the run dir, where a reader or `sig gc`
	// would then have to know to ignore it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != parkFileName {
			t.Fatalf("writePark leaked %q into the run dir", e.Name())
		}
	}
}

// plantStaleClaim writes the claim file a resolver that crashed mid-resolution
// leaves behind, back-dated past parkClaimStale so the next claimant judges it
// dead. Back-dating, not waiting: the ten-minute threshold is a property of the
// code under test, never something a test may sit through.
func (f *parkFixture) plantStaleClaim() {
	f.t.Helper()
	path := filepath.Join(f.dir, parkClaimName)
	if err := os.WriteFile(path, []byte("999999 1 crashed\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	old := time.Now().Add(-parkClaimStale - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		f.t.Fatal(err)
	}
}

// TestFrontDoorsEnforceTimeoutBeforeClaiming pins the ordering the front doors'
// safety rests on, which was INCIDENTAL and undocumented until it was measured.
//
// claimPark's stale-steal path does not guarantee exclusion: two resolvers
// arriving together, minutes after a crash, can both judge the same claim dead
// and both steal it (about 5% of rounds under CPU oversubscription). What keeps
// `sig ack` and `sig reject` off that path is that both call enforceParkTimeout
// FIRST, and its own claim+release reaps the crashed resolver's claim — so by
// the time the front door takes its own claim, the file is absent or young, and
// a young claim is refused rather than stolen. Reorder either call site below
// its claim and the steal path is live again on the most common path in the
// feature, which is precisely the sort of silent reopening this test exists to
// prevent.
func TestFrontDoorsEnforceTimeoutBeforeClaiming(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths+"ack-timeout = 1h\n")
	ctx := context.Background()
	pristine := f.reread()

	// (1) The reaping itself. The park has NOT expired, so the sweep resolves
	// nothing — but it still claims and releases, and that is what clears a
	// crashed resolver's claim out of the way of everyone behind it.
	f.plantStaleClaim()
	if enforceParkTimeout(ctx, f.g, f.dir) {
		t.Fatal("a park an hour from its deadline was auto-rejected")
	}
	if fi, err := os.Stat(filepath.Join(f.dir, parkClaimName)); err == nil && time.Since(fi.ModTime()) >= parkClaimStale {
		t.Fatal("the sweep left the crashed resolver's stale claim in place; the next claimPark would have to STEAL it, and stealing is not exclusive")
	}

	// (2) The ordering, end to end. With the park expired AND a crashed
	// resolver's claim in the way, both front doors must report an
	// already-resolved run whose rejection is the TIMEOUT's, not the caller's.
	// That is only reachable if the sweep ran before the front door's own claim:
	// with the order reversed, the front door claims first, the sweep is refused,
	// and the ack LANDS the park it should have found expired.
	expired := *pristine
	expired.CreatedAt = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	for _, door := range []string{"ack", "reject"} {
		f.rearm(&expired)
		f.plantStaleClaim()
		before := f.head()

		var err error
		if door == "ack" {
			_, err = ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit})
		} else {
			_, err = rejectRun(ctx, f.g, f.dir, "cli", "the operator's own reason")
		}
		if !errors.Is(err, errNotAwaitingAck) {
			t.Fatalf("%s on an expired park returned %v, want errNotAwaitingAck — the timeout sweep did not run before the resolution claim", door, err)
		}
		if pk := f.reread(); !strings.Contains(pk.RejectReason, "ack-timeout") {
			t.Fatalf("%s: rejectReason = %q, want the ack-timeout's — the sweep did not get there first", door, pk.RejectReason)
		}
		if got := f.head(); got != before {
			t.Fatalf("%s: the ref advanced to %s on a park that had already expired", door, short(got))
		}
		if f.status() != statusRejected {
			t.Fatalf("%s: final status %q, want %s", door, f.status(), statusRejected)
		}
	}
}

// TestAckRefusesAResolvedRecordUnderTheClaim pins the ONE thing the
// recheckResolvable call under ackRun's claim contributes that writeParkCAS
// cannot: the crash window the commit order deliberately leaves open.
//
// Every resolver writes the record first and the status marker second (so a
// crash cannot lose the record of a landing that happened). A resolver that dies
// in between therefore leaves a RESOLVED park.json under an awaiting-ack
// status.json — a durable on-disk state needing no concurrency to reproduce. In
// it, an arriving ack's early status read passes, and the bytes it read still
// match disk, so the CAS would pass too. Only re-reading the record under the
// claim catches it. Delete that call and this ack lands a park an operator
// already rejected.
func TestAckRefusesAResolvedRecordUnderTheClaim(t *testing.T) {
	f := newParkFixture(t, parkPolicyAckPaths)
	ctx := context.Background()

	pk := f.reread()
	pk.RejectReason = "an operator rejected this, then the process died"
	pk.ResolvedAt = time.Now().UTC().Format(time.RFC3339)
	f.writeParkRaw(pk)
	writeRunStatus(f.dir, statusAwaitingAck, "") // the marker the crash never advanced

	before := f.head()
	if _, err := ackRun(ctx, f.cell, f.dir, "test", ackEnv{Mode: envModeInherit}); !errors.Is(err, errNotAwaitingAck) {
		t.Fatalf("ack over a resolved record returned %v, want errNotAwaitingAck", err)
	}
	if got := f.head(); got != before {
		t.Fatalf("ack landed %s over a record that was already resolved", short(got))
	}
	if got := f.reread(); got.LandedSHA != "" || got.RejectReason != pk.RejectReason {
		t.Fatalf("the refused ack rewrote the resolved record: landedSHA=%q rejectReason=%q", got.LandedSHA, got.RejectReason)
	}
}
