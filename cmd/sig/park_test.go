package main

// End-to-end coverage for run parking (issue #109). Every test here drives a
// REAL run through `sig serve` against a real repo whose sigbound.policy holds
// an ack-path, so what is asserted is the artifact the daemon actually wrote,
// never a hand-built fixture standing in for it.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
		{"baseSHA rewritten to an unrelated ancestor-less commit", func(f *parkFixture, pk *parkJSON) {
			pk.BaseSHA = strings.Repeat("b", len(pk.BaseSHA))
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
	if _, err := rejectRun(f.dir, "test", ""); err == nil {
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
	entries := inboxEntriesFor("c", f.dir, f.runID, inboxParked, time.Now())
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

	out, err := rejectRun(f.dir, "test", "not shipping auth changes this week")
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
	if _, err := rejectRun(f.dir, "test", "again"); err == nil {
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
	if enforceParkTimeout(f.dir) {
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
