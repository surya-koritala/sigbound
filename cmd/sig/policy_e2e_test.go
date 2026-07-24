package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commitPolicy writes content to repo's sigbound.policy and commits it, returning
// the new commit SHA. The working tree is clean afterward (no run has advanced
// the ref yet), so this is safe to call before driving a run.
func commitPolicy(t *testing.T, g gitCommitter, repo, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, policyFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := g.CommitAll(context.Background(), "add landing policy")
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// gitCommitter is the sliver of *gitx.Git commitPolicy needs, so the helper
// doesn't drag the whole type into its signature.
type gitCommitter interface {
	CommitAll(ctx context.Context, msg string) (string, error)
}

// taskWrite builds a testagent task that writes files (path->content) and
// declares exactly those paths as its lane, so it stays in-lane under -lanes
// strict (which a policy may impose).
func taskWrite(t *testing.T, id string, files map[string]string) taskSpec {
	t.Helper()
	decl := make([]string, 0, len(files))
	for p := range files {
		decl = append(decl, p)
	}
	return taskSpec{ID: id, Prompt: mustJSON(t, map[string]any{"write": files}), Files: decl}
}

// runRunJSON drives a full `sig run` and returns the decoded report, the exit
// code, and the raw JSON. It fails on an OPERATIONAL error (non-nil err); a
// report-level code (flagged, verify-failed) is returned for the caller to
// assert on.
func runRunJSON(t *testing.T, repo, agent string, tasks []taskSpec, extra ...string) (runReport, int, string) {
	t.Helper()
	args := append([]string{"-repo", repo, "-tasks", tasksFileFor(t, tasks), "-agent", agent, "-json"}, extra...)
	var buf bytes.Buffer
	code, err := runRun(&buf, args)
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}
	return rep, code, buf.String()
}

// TestPolicyLoadedFromBaseSHANotWorkingTree is acceptance #3: the policy that
// gates a landing is the one COMMITTED at the base SHA, never the working-tree
// copy. A working-tree edit (uncommitted) does not change what loadPolicy reads
// at that SHA; committing it, which moves the SHA, does.
func TestPolicyLoadedFromBaseSHANotWorkingTree(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	polPath := filepath.Join(repo, policyFileName)

	if err := os.WriteFile(polPath, []byte("lanes = strict\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shaA, err := g.CommitAll(ctx, "policy A")
	if err != nil {
		t.Fatal(err)
	}
	// Working-tree edit to a WEAKER policy, left UNCOMMITTED.
	if err := os.WriteFile(polPath, []byte("lanes = off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantA, _ := parsePolicy([]byte("lanes = strict\n"))
	got, err := loadPolicy(ctx, g, shaA)
	if err != nil {
		t.Fatal(err)
	}
	if got.hash != wantA.hash || got.lanes != laneStrict {
		t.Fatalf("at shaA: hash=%q lanes=%q, want the COMMITTED strict policy (working-tree edit must be ignored)", got.hash, got.lanes)
	}
	// Commit the edit: NOW the base tree carries the weaker policy.
	shaB, err := g.CommitAll(ctx, "policy B")
	if err != nil {
		t.Fatal(err)
	}
	if shaA == shaB {
		t.Fatal("committing the edit should have moved the SHA")
	}
	wantB, _ := parsePolicy([]byte("lanes = off\n"))
	got, err = loadPolicy(ctx, g, shaB)
	if err != nil {
		t.Fatal(err)
	}
	if got.hash != wantB.hash || got.lanes != laneOff {
		t.Fatalf("at shaB: hash=%q lanes=%q, want the committed weaker policy", got.hash, got.lanes)
	}
}

// TestPolicyAbsentLoadsToZero: no policy file at the base is NOT an error — it
// resolves to a zero policy (present=false), the no-migration default.
func TestPolicyAbsentLoadsToZero(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	g, _ := makeGoRepo(t)
	sha, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	pol, err := loadPolicy(ctx, g, sha)
	if err != nil {
		t.Fatalf("absent policy must not error: %v", err)
	}
	if pol.present {
		t.Fatalf("no policy file, but present=%v", pol.present)
	}
}

// TestPolicyHashInManifest is acceptance #5: a run against a repo WITH a policy
// records the policy's sha256 in the manifest; a run against a repo with NO
// policy records no policy block at all (byte-identical to before the feature —
// acceptance #8's field-absence half).
func TestPolicyHashInManifest(t *testing.T) {
	agent := buildTestAgent(t)

	// With a policy: manifest carries policyHash matching the committed bytes.
	g, repo := makeGoRepo(t)
	src := "verify = echo ok\n"
	commitPolicy(t, g, repo, src)
	want, _ := parsePolicy([]byte(src))
	manifest := filepath.Join(t.TempDir(), "m.json")
	rep, code, _ := runRunJSON(t, repo, agent, []taskSpec{taskWrite(t, "a", map[string]string{"a.txt": "x\n"})}, "-manifest", manifest)
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK", code)
	}
	if rep.Policy == nil || rep.Policy.Hash != want.hash {
		t.Fatalf("report policy=%+v, want hash %q", rep.Policy, want.hash)
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var fromDisk runReport
	if err := json.Unmarshal(data, &fromDisk); err != nil {
		t.Fatal(err)
	}
	if fromDisk.Policy == nil || fromDisk.Policy.Hash != want.hash {
		t.Fatalf("manifest policy=%+v, want hash %q", fromDisk.Policy, want.hash)
	}

	// With NO policy: no policy block, and the raw JSON names neither the policy
	// field nor a flagged reason — byte-identical shape to pre-feature.
	_, repoBare := makeGoRepo(t)
	repBare, codeBare, raw := runRunJSON(t, repoBare, agent, []taskSpec{taskWrite(t, "a", map[string]string{"a.txt": "x\n"})})
	if codeBare != exitOK {
		t.Fatalf("bare code=%d, want exitOK", codeBare)
	}
	if repBare.Policy != nil {
		t.Fatalf("no policy file, but report has policy=%+v", repBare.Policy)
	}
	if strings.Contains(raw, `"policy"`) || strings.Contains(raw, `"reason"`) {
		t.Fatalf("no-policy report must not contain policy/reason fields:\n%s", raw)
	}
}

// TestPolicySharedResolverRunAndServe is acceptance #1: the SAME repo+policy
// resolved through `sig run` and through `sig serve` yields IDENTICAL effective
// parameters (lane floor raised, verify battery composed, quota clamped) and
// identical outcomes (both land). One resolver, reached by both via driveRun.
func TestPolicySharedResolverRunAndServe(t *testing.T) {
	agent := buildTestAgent(t)
	g, repo := makeGoRepo(t)
	// A policy that TIGHTENS all three governed dimensions: a verify battery, a
	// lane floor, and a parallel-agents ceiling. Neither invocation sets any of
	// them, so both must be tightened to policy identically.
	commitPolicy(t, g, repo, "verify = echo pol-verify\nlanes = strict\nparallel-agents = 2\n")
	want, _ := parsePolicy([]byte("verify = echo pol-verify\nlanes = strict\nparallel-agents = 2\n"))

	// Path 1: sig run (writes a.txt).
	repRun, code, _ := runRunJSON(t, repo, agent, []taskSpec{taskWrite(t, "a", map[string]string{"a.txt": "x\n"})})
	if code != exitOK {
		t.Fatalf("sig run code=%d, want exitOK", code)
	}

	// Path 2: sig serve on the SAME repo (writes b.txt), a fresh run off the now
	// advanced main — same committed policy, so same effective resolution.
	_, ts := newTestServer(t, "", repo)
	var created struct {
		RunID string `json:"runId"`
	}
	if c := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Base:  "main",
		Tasks: []taskSpec{taskWrite(t, "b", map[string]string{"b.txt": "x\n"})},
		Agent: agent,
	}, &created); c != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", c)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "done" || final.Report == nil {
		t.Fatalf("serve run status %q err=%q report=%v", final.Status, final.Error, final.Report)
	}
	repServe := *final.Report

	// Effective params must match across both paths.
	if repRun.LaneMode != laneStrict || repServe.LaneMode != laneStrict {
		t.Fatalf("lane floor not applied identically: run=%q serve=%q", repRun.LaneMode, repServe.LaneMode)
	}
	if repRun.ParallelAgents != 2 || repServe.ParallelAgents != 2 {
		t.Fatalf("parallel clamp not applied identically: run=%d serve=%d", repRun.ParallelAgents, repServe.ParallelAgents)
	}
	if !strings.Contains(repRun.VerifyCmd, "echo pol-verify") || !strings.Contains(repServe.VerifyCmd, "echo pol-verify") {
		t.Fatalf("policy verify not composed identically: run=%q serve=%q", repRun.VerifyCmd, repServe.VerifyCmd)
	}
	if repRun.Policy == nil || repServe.Policy == nil || repRun.Policy.Hash != want.hash || repServe.Policy.Hash != want.hash {
		t.Fatalf("policyHash mismatch: run=%v serve=%v want=%q", repRun.Policy, repServe.Policy, want.hash)
	}
	// Identical outcome: both landed.
	if len(repRun.Integrate.Landed) == 0 || len(repServe.Integrate.Landed) == 0 {
		t.Fatalf("both paths should land: run.landed=%v serve.landed=%v", repRun.Integrate.Landed, repServe.Integrate.Landed)
	}
}

// TestPolicyAckPathHeldCleanLands is acceptance #4 (ack-paths half): a group
// touching an ack-path is flagged with its reason and the ref is NOT advanced
// for it, while a disjoint clean group still integrates, verifies, and lands.
func TestPolicyAckPathHeldCleanLands(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	agent := buildTestAgent(t)
	g, repo := makeGoRepo(t)
	commitPolicy(t, g, repo, "ack-paths = auth/**\nverify = echo ok\n")

	rep, code, _ := runRunJSON(t, repo, agent, []taskSpec{
		taskWrite(t, "auth", map[string]string{"auth/login.txt": "secret\n"}),
		taskWrite(t, "clean", map[string]string{"clean.txt": "ok\n"}),
	})
	if code != exitFlagged {
		t.Fatalf("code=%d, want exitFlagged (%d)", code, exitFlagged)
	}
	// The ack group is flagged with the ack reason; the clean group landed.
	var authFlag *flaggedJSON
	for i := range rep.Integrate.Flagged {
		if rep.Integrate.Flagged[i].Branch == "agent/auth" {
			authFlag = &rep.Integrate.Flagged[i]
		}
	}
	if authFlag == nil || !strings.Contains(authFlag.Reason, "ack required for auth/login.txt") {
		t.Fatalf("flagged=%+v, want agent/auth held with an ack reason", rep.Integrate.Flagged)
	}
	landed := strings.Join(rep.Integrate.Landed, ",")
	if !strings.Contains(landed, "agent/clean") || strings.Contains(landed, "agent/auth") {
		t.Fatalf("landed=%v, want clean landed and auth held", rep.Integrate.Landed)
	}
	if !rep.Verify.Ran || !rep.Verify.OK {
		t.Fatalf("verify should have run green on the clean subset: %+v", rep.Verify)
	}
	// The ref advanced for the clean file only; the ack-path file never landed.
	if _, present, _ := g.BlobAt(ctx, "main", "clean.txt"); !present {
		t.Fatal("clean.txt should be on main")
	}
	if _, present, _ := g.BlobAt(ctx, "main", "auth/login.txt"); present {
		t.Fatal("auth/login.txt must NOT be on main (ack-path group held)")
	}
}

// TestPolicySelfModificationHeld is acceptance #4 (self-protection half): a run
// whose changes modify sigbound.policy itself is held — a change cannot loosen
// the bar that gates it — while a disjoint clean change lands, and the committed
// policy on the ref is left untouched.
func TestPolicySelfModificationHeld(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	agent := buildTestAgent(t)
	g, repo := makeGoRepo(t)
	src := "verify = echo ok\n"
	commitPolicy(t, g, repo, src)

	rep, code, _ := runRunJSON(t, repo, agent, []taskSpec{
		// Attempt to weaken the gating policy itself.
		taskWrite(t, "loosen", map[string]string{policyFileName: "lanes = off\n"}),
		taskWrite(t, "clean", map[string]string{"clean.txt": "ok\n"}),
	})
	if code != exitFlagged {
		t.Fatalf("code=%d, want exitFlagged (%d)", code, exitFlagged)
	}
	var polFlag *flaggedJSON
	for i := range rep.Integrate.Flagged {
		if rep.Integrate.Flagged[i].Branch == "agent/loosen" {
			polFlag = &rep.Integrate.Flagged[i]
		}
	}
	if polFlag == nil || !strings.Contains(polFlag.Reason, "modifies "+policyFileName) {
		t.Fatalf("flagged=%+v, want agent/loosen held for self-modification", rep.Integrate.Flagged)
	}
	if landed := strings.Join(rep.Integrate.Landed, ","); !strings.Contains(landed, "agent/clean") {
		t.Fatalf("landed=%v, want agent/clean to land", rep.Integrate.Landed)
	}
	// The gating policy on main is unchanged — the loosening attempt never landed.
	content, present, err := g.BlobAt(ctx, "main", policyFileName)
	if err != nil {
		t.Fatal(err)
	}
	if !present || content != src {
		t.Fatalf("sigbound.policy on main = %q (present=%v), want the ORIGINAL %q", content, present, src)
	}
}

// TestPolicyVerifyBatteryInjectionBypass reproduces the reviewer's proven gate
// bypass: a policy that forbids all landing (verify = false) plus an invoker
// -verify crafted to break out of the composition (`true ) ; ( true`) must NOT
// land — the policy member's failure short-circuits the ANDed chain, each member
// confined to its own nested sh. A regression against the old textual-wrap
// composition, which reported OK and landed. Both the run and serve paths.
func TestPolicyVerifyBatteryInjectionBypass(t *testing.T) {
	ctx := context.Background()
	agent := buildTestAgent(t)
	const injection = "true ) ; ( true"

	// --- sig run ---
	g, repo := makeGoRepo(t)
	commitPolicy(t, g, repo, "verify = false\n") // nothing may EVER land
	baseBefore, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	rep, code, _ := runRunJSON(t, repo, agent,
		[]taskSpec{taskWrite(t, "a", map[string]string{"a.txt": "x\n"})}, "-verify", injection)
	if rep.Verify.OK {
		t.Fatalf("verify.OK=true — the injection masked the policy member's failure (BYPASS): %+v", rep.Verify)
	}
	if code != exitVerifyFailed {
		t.Fatalf("code=%d, want exitVerifyFailed (%d): nothing may land", code, exitVerifyFailed)
	}
	baseAfter, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if baseAfter != baseBefore {
		t.Fatalf("main advanced %s -> %s: the change LANDED despite verify=false (BYPASS)", baseBefore, baseAfter)
	}

	// --- sig serve (crafted POST /runs) ---
	gs, repoServe := makeGoRepo(t)
	commitPolicy(t, gs, repoServe, "verify = false\n")
	serveBefore, err := gs.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	_, ts := newTestServer(t, "", repoServe)
	var created struct {
		RunID string `json:"runId"`
	}
	if c := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:   repoServe,
		Base:   "main",
		Tasks:  []taskSpec{taskWrite(t, "a", map[string]string{"a.txt": "x\n"})},
		Agent:  agent,
		Verify: injection,
	}, &created); c != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", c)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Report == nil || final.Report.Verify.OK {
		t.Fatalf("serve: verify.OK true or no report — injection bypass on the serve path: %+v", final.Report)
	}
	// The ref is the source of truth for "did it land": Integrate.Landed lists
	// branches folded into the CANDIDATE tree and is populated even on a verify
	// failure, so it is not a landing signal.
	serveAfter, err := gs.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if serveAfter != serveBefore {
		t.Fatalf("serve: main advanced %s -> %s despite verify=false (BYPASS)", serveBefore, serveAfter)
	}
}

// TestPolicyVerifyBatteryFailingMemberFailsGate: a legitimate multi-member
// battery ANDs correctly — a genuine failure in ANY member (here the second
// policy line) fails the gate and lands nothing, even though the first member
// passes.
func TestPolicyVerifyBatteryFailingMemberFailsGate(t *testing.T) {
	ctx := context.Background()
	agent := buildTestAgent(t)
	g, repo := makeGoRepo(t)
	commitPolicy(t, g, repo, "verify = true\nverify = false\n") // member 2 always fails
	before, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	rep, code, _ := runRunJSON(t, repo, agent, []taskSpec{taskWrite(t, "a", map[string]string{"a.txt": "x\n"})})
	if rep.Verify.OK || code != exitVerifyFailed {
		t.Fatalf("verify.OK=%v code=%d, want a failing gate when any battery member fails", rep.Verify.OK, code)
	}
	after, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("main advanced despite a failing battery member")
	}
}

// bisectPolicyVerify fails the combined tree whenever g2's file is present, so a
// three-group batch has exactly one bad group for bisect to drop. Same shape the
// -verify-bisect tests use, but supplied by the POLICY rather than a flag.
const bisectPolicyVerify = "verify = if [ -f g2.txt ]; then exit 1; fi; exit 0\n"

// TestPolicyVerifyBisectUsesPolicyBattery is N2 case (a): on a repo whose verify
// comes ONLY from sigbound.policy, `-verify-bisect` with NO -verify flag is
// accepted (the precondition sees the EFFECTIVE verify) and bisect works end to
// end — the red group is dropped and the good subset lands.
func TestPolicyVerifyBisectUsesPolicyBattery(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	agent := buildTestAgent(t)
	g, repo := makeGoRepo(t)
	commitPolicy(t, g, repo, bisectPolicyVerify)

	// No -verify anywhere on the command line: the battery is the policy's.
	rep, code, out := runRunJSON(t, repo, agent, disjointGroupTasks(t, 3), "-verify-bisect")
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK — bisect must run off the policy battery\n%s", code, out)
	}
	if rep.Verify.Bisect == nil {
		t.Fatalf("verify.bisect absent: bisect never ran off the policy battery\n%s", out)
	}
	if got := rep.Integrate.DroppedByBisect; len(got) != 1 || got[0] != "agent/g2" {
		t.Fatalf("droppedByBisect=%v, want [agent/g2]", got)
	}
	if got := rep.Integrate.Landed; len(got) != 2 {
		t.Fatalf("landed=%v, want the two green groups", got)
	}
	// The salvaged subset really landed: g0/g1 on main, g2 absent.
	paths, err := g.LsTree(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(paths, "g0.txt") || !contains(paths, "g1.txt") || contains(paths, "g2.txt") {
		t.Fatalf("landed tree=%v, want g0+g1 and NOT g2", paths)
	}
}

// TestPolicyBisectRejectedWithNoVerifyAnywhere is N2 case (b) plus its serve
// half: with neither a policy verify nor a -verify flag, -verify-bisect is still
// rejected loudly — same message and same exit code as before the precondition
// moved. Covers the -verify-impact variant too (case d's genuine half).
func TestPolicyBisectRejectedWithNoVerifyAnywhere(t *testing.T) {
	agent := buildTestAgent(t)

	// --- sig run: no policy at all, no -verify, -verify-bisect ---
	_, repo := makeGoRepo(t)
	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, []taskSpec{taskWrite(t, "a", map[string]string{"a.txt": "x\n"})}),
		"-agent", agent,
		"-verify-bisect",
	})
	if err == nil || code != exitOperationalError {
		t.Fatalf("code=%d err=%v, want exitOperationalError for -verify-bisect with no verify anywhere", code, err)
	}
	if !strings.Contains(err.Error(), "-verify-bisect requires -verify") {
		t.Fatalf("err=%q, want the unchanged -verify-bisect precondition message", err)
	}

	// -verify-impact with no verify anywhere is rejected the same way.
	buf.Reset()
	code, err = runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, []taskSpec{taskWrite(t, "a", map[string]string{"a.txt": "x\n"})}),
		"-agent", agent,
		"-verify-impact", "go test ./...",
	})
	if err == nil || code != exitOperationalError || !strings.Contains(err.Error(), "-verify-impact requires -verify") {
		t.Fatalf("code=%d err=%v, want the -verify-impact precondition rejection", code, err)
	}

	// --- sig serve: same genuine case, surfaced as the run's recorded error ---
	_, repoServe := makeGoRepo(t)
	_, ts := newTestServer(t, "", repoServe)
	var created struct {
		RunID string `json:"runId"`
	}
	if c := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:         repoServe,
		Base:         "main",
		Tasks:        []taskSpec{taskWrite(t, "a", map[string]string{"a.txt": "x\n"})},
		Agent:        agent,
		VerifyBisect: true,
	}, &created); c != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", c)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "error" || !strings.Contains(final.Error, "-verify-bisect requires -verify") {
		t.Fatalf("serve status=%q error=%q, want the bisect precondition rejection", final.Status, final.Error)
	}
}

// TestPolicyVerifyBisectUsesPolicyBatteryOverServe is N2 case (c): the serve
// equivalent of (a) — a policy-supplied battery satisfies verifyBisect's
// precondition over HTTP too, from the same single site.
func TestPolicyVerifyBisectUsesPolicyBatteryOverServe(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	agent := buildTestAgent(t)
	g, repo := makeGoRepo(t)
	commitPolicy(t, g, repo, bisectPolicyVerify)

	_, ts := newTestServer(t, "", repo)
	var created struct {
		RunID string `json:"runId"`
	}
	if c := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:         repo,
		Base:         "main",
		Tasks:        disjointGroupTasks(t, 3),
		Agent:        agent,
		VerifyBisect: true, // no verify field: the policy supplies it
	}, &created); c != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", c)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "done" || final.Report == nil {
		t.Fatalf("serve status=%q error=%q, want done (bisect off the policy battery)", final.Status, final.Error)
	}
	if got := final.Report.Integrate.DroppedByBisect; len(got) != 1 || got[0] != "agent/g2" {
		t.Fatalf("serve droppedByBisect=%v, want [agent/g2]", got)
	}
	paths, err := g.LsTree(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(paths, "g0.txt") || contains(paths, "g2.txt") {
		t.Fatalf("serve landed tree=%v, want the green subset only", paths)
	}
}

// TestPolicyVerifyImpactAcceptedThenClearedByPolicy is N2 case (d): with a
// policy battery, -verify-impact is ACCEPTED (never rejected misleadingly) and
// then cleared by the documented rule that a policy battery must always run in
// full — so verify runs unscoped and the report shows no impact scoping.
func TestPolicyVerifyImpactAcceptedThenClearedByPolicy(t *testing.T) {
	agent := buildTestAgent(t)
	g, repo := makeGoRepo(t)
	commitPolicy(t, g, repo, "verify = echo policy-verify\n")

	// -verify-impact with NO -verify flag: previously rejected at parse time.
	rep, code, out := runRunJSON(t, repo, agent,
		[]taskSpec{taskWrite(t, "a", map[string]string{"a.txt": "x\n"})},
		"-verify-impact", "echo impact-should-never-run")
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK — the flag must be accepted, then cleared\n%s", code, out)
	}
	if !rep.Verify.Ran || !rep.Verify.OK {
		t.Fatalf("verify=%+v, want the policy battery to have run green", rep.Verify)
	}
	// Impact was cleared, not used: Scope stays empty (it is only set when
	// -verify-impact is actually configured at verify time).
	if rep.Verify.Scope != "" {
		t.Fatalf("verify.scope=%q, want empty: a policy battery clears impact scoping", rep.Verify.Scope)
	}
	_ = g
}

// TestPolicyWeakerFlagRejectedBeforeAnyAgent: an EXPLICIT weaker flag against a
// stricter policy is an operational error, raised before any agent runs (no
// worktree, no spend), on both the run and serve paths.
func TestPolicyWeakerFlagRejectedBeforeAnyAgent(t *testing.T) {
	agent := buildTestAgent(t)
	g, repo := makeGoRepo(t)
	commitPolicy(t, g, repo, "lanes = strict\n")
	marker := filepath.Join(t.TempDir(), "ran")

	// sig run: -lanes off explicitly is a loud operational error.
	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, []taskSpec{taskWrite(t, "a", map[string]string{"a.txt": "x\n"})}),
		"-agent", "touch " + marker + " && " + agent,
		"-lanes", "off",
	})
	if err == nil || code != exitOperationalError {
		t.Fatalf("code=%d err=%v, want an operational error for -lanes off vs policy strict", code, err)
	}
	if !strings.Contains(err.Error(), "tighten") {
		t.Fatalf("err=%v, want a tighten-only message", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("no agent may run when policy rejects the flags")
	}

	// sig serve: the same conflict surfaces as the run's recorded error (async,
	// after 202, since resolution happens in driveRun before any agent runs).
	_, ts := newTestServer(t, "", repo)
	var created struct {
		RunID string `json:"runId"`
	}
	if c := doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Base:  "main",
		Tasks: []taskSpec{taskWrite(t, "b", map[string]string{"b.txt": "x\n"})},
		Agent: agent,
		Lanes: "off",
	}, &created); c != http.StatusAccepted {
		t.Fatalf("POST /runs status %d, want 202", c)
	}
	final := pollRun(t, ts, "", created.RunID)
	if final.Status != "error" || !strings.Contains(final.Error, "tighten") {
		t.Fatalf("serve run status=%q error=%q, want an error naming the tighten-only rule", final.Status, final.Error)
	}
}
