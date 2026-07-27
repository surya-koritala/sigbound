package main

// Acceptance coverage for issue #149 (`sig unland`). Every test here drives a
// REAL `sig run` to produce the landing it then takes back, and a real
// `sig unland` through runUnland — the CLI's own front door, with real flag
// parsing — against a real repo. What is asserted is the TREE, not a message:
// an unland that restores the wrong bytes while reporting success is the exact
// failure this feature exists to make impossible.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

// unlandFixture is a repo with a monotonic run-id source and helpers to land a
// run and then take it back.
type unlandFixture struct {
	t       *testing.T
	repo    string
	g       *gitx.Git
	c       *cell.Cell
	agent   string
	runsDir string
}

// newUnlandFixture builds a Go repo (optionally with a committed
// sigbound.policy) and pins run ids to a strictly increasing sequence.
//
// The id sequence is FORCED, not hoped for: a run id's chronology lives in a
// second-resolution timestamp prefix, and these tests drive several runs well
// inside one second. Two ids from the same second sort by their random suffix,
// which would make "later run" a coin flip for the blast-radius walk — so the
// ordering the assertions depend on is constructed rather than raced for.
func newUnlandFixture(t *testing.T, policyBody string) *unlandFixture {
	t.Helper()
	requirePOSIXShell(t)
	orig := newRunID
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	n := 0
	newRunID = func() string {
		n++
		return base.Add(time.Duration(n)*time.Minute).Format(runIDTimeLayout) + fmt.Sprintf("-%06x", n)
	}
	t.Cleanup(func() { newRunID = orig })

	g, repo := makeGoRepo(t)
	if policyBody != "" {
		commitPolicy(t, g, repo, policyBody)
	}
	c, err := cell.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	runsDir, err := cellRunsDir(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return &unlandFixture{t: t, repo: repo, g: g, c: c, agent: buildTestAgent(t), runsDir: runsDir}
}

// land drives a one-task `sig run` that writes files, asserts it landed, and
// returns its report.
func (f *unlandFixture) land(id string, files map[string]string, extra ...string) runReport {
	f.t.Helper()
	rep, code, out := runRunJSON(f.t, f.repo, f.agent, []taskSpec{taskWrite(f.t, id, files)}, extra...)
	if code != exitOK {
		f.t.Fatalf("`sig run` %s: exit %d, want 0\n%s", id, code, out)
	}
	if !landed(&rep) {
		f.t.Fatalf("`sig run` %s did not land\n%s", id, out)
	}
	return rep
}

// unland drives `sig unland TARGET -repo P -json` and returns the decoded
// outcome plus the exit code. An OPERATIONAL error is returned, never fatal:
// several tests assert on exactly that.
func (f *unlandFixture) unland(target string, extra ...string) (unlandOutcome, int, error) {
	f.t.Helper()
	args := append([]string{target, "-repo", f.repo, "-json"}, extra...)
	var buf bytes.Buffer
	code, err := runUnland(&buf, args)
	if err != nil {
		return unlandOutcome{}, code, err
	}
	var out unlandOutcome
	if jerr := json.Unmarshal(buf.Bytes(), &out); jerr != nil {
		f.t.Fatalf("parse unland outcome: %v\n%s", jerr, buf.String())
	}
	return out, code, nil
}

func (f *unlandFixture) head() string {
	f.t.Helper()
	sha, err := f.g.RevParse(context.Background(), "main")
	if err != nil {
		f.t.Fatal(err)
	}
	return sha
}

func (f *unlandFixture) tree(rev string) string {
	f.t.Helper()
	t, err := f.g.TreeOID(context.Background(), rev)
	if err != nil {
		f.t.Fatalf("tree of %s: %v", rev, err)
	}
	return t
}

// blobAt reads one path from a commit's tree; present reports whether it exists.
func (f *unlandFixture) blobAt(rev, path string) (string, bool) {
	f.t.Helper()
	content, present, err := f.g.BlobAt(context.Background(), rev, path)
	if err != nil {
		f.t.Fatalf("read %s at %s: %v", path, rev, err)
	}
	return content, present
}

func (f *unlandFixture) runDirs() []string {
	f.t.Helper()
	return runDirNames(f.runsDir)
}

// syncWorktree brings the repo's own checkout back in line with the base ref a
// run just advanced. Runs happen in throwaway worktrees, so the main checkout's
// index still describes the pre-run tree; committing on top of it without this
// would stage the run's own files as deletions.
func (f *unlandFixture) syncWorktree() {
	f.t.Helper()
	if err := f.g.ResetHard(context.Background()); err != nil {
		f.t.Fatalf("sync the checkout to main: %v", err)
	}
	if _, present := f.blobAt("HEAD", "alpha.go"); !present {
		return
	}
	// Guard the guard: a stale index here would make the next commit stage the
	// landed file as a DELETION, and the test it feeds would then pass for the
	// wrong reason (an unland with nothing left to remove reads as a no-op).
	if _, err := os.Stat(filepath.Join(f.repo, "alpha.go")); err != nil {
		f.t.Fatalf("the checkout is still behind the base ref after reset: %v", err)
	}
}

// TestUnlandRestoresTheExactPreRunTree is acceptance (1), (2) and (16): the
// landed tree is the target run's pre-run tree BY OID, the inverse branch's
// contribution is that run's write-set exactly, and a second unland of the same
// run is a reported no-op rather than a second landing.
func TestUnlandRestoresTheExactPreRunTree(t *testing.T) {
	f := newUnlandFixture(t, "")
	preRun := f.head()
	target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
	if f.tree(f.head()) == f.tree(preRun) {
		t.Fatal("the run landed nothing observable: base tree unchanged")
	}

	out, code, err := f.unland(target.RunID, "-verify", "go build ./...")
	if err != nil || code != exitOK {
		t.Fatalf("unland: exit %d err %v (%s)", code, err, out.Message)
	}
	if out.Status != unlandStatusDone {
		t.Fatalf("status %q, want %q: %s", out.Status, unlandStatusDone, out.Message)
	}
	// THE assertion: the base now carries the exact tree it had before the run.
	if got, want := f.tree(f.head()), f.tree(target.BaseSHA); got != want {
		t.Fatalf("base tree after unland is %s, want the pre-run tree %s", short(got), short(want))
	}
	if f.head() != out.LandedSHA {
		t.Fatalf("base is at %s but the outcome says it landed %s", short(f.head()), short(out.LandedSHA))
	}
	// It is a NEW commit on top, never a rewind: the landing it takes back is
	// still in the history.
	anc, aerr := f.g.IsAncestor(context.Background(), target.Integrate.FinalSHA, f.head())
	if aerr != nil || !anc {
		t.Fatalf("the unland rewrote history: %s is no longer an ancestor of the base (err %v)", short(target.Integrate.FinalSHA), aerr)
	}

	// (2) the inverse branch: parented on what the target landed, contributing
	// exactly that run's write-set.
	branch := unlandBranchPrefix + out.RunID
	if out.Branch != branch {
		t.Fatalf("outcome branch %q, want %q", out.Branch, branch)
	}
	tip, terr := f.g.RevParse(context.Background(), branch)
	if terr != nil {
		t.Fatalf("the inverse branch %s does not exist: %v", branch, terr)
	}
	parent, perr := f.g.RevParse(context.Background(), branch+"^")
	if perr != nil || parent != target.Integrate.FinalSHA {
		t.Fatalf("inverse parent %s, want the target's finalSHA %s (err %v)", short(parent), short(target.Integrate.FinalSHA), perr)
	}
	inverseWS, werr := f.g.DiffNameOnly(context.Background(), target.Integrate.FinalSHA, tip)
	if werr != nil {
		t.Fatal(werr)
	}
	targetWS, werr := f.g.DiffNameOnly(context.Background(), target.BaseSHA, target.Integrate.FinalSHA)
	if werr != nil {
		t.Fatal(werr)
	}
	if strings.Join(inverseWS, ",") != strings.Join(targetWS, ",") {
		t.Fatalf("inverse write-set %v, want the target run's %v exactly", inverseWS, targetWS)
	}

	// (16) a second unland of the same run: nothing left to take back.
	before := f.head()
	out2, code2, err2 := f.unland(target.RunID, "-verify", "go build ./...")
	if err2 != nil || code2 != exitOK {
		t.Fatalf("second unland: exit %d err %v", code2, err2)
	}
	if out2.Status != unlandStatusNoOp {
		t.Fatalf("second unland status %q, want %q: %s", out2.Status, unlandStatusNoOp, out2.Message)
	}
	if f.head() != before {
		t.Fatalf("second unland moved the base from %s to %s", short(before), short(f.head()))
	}
}

// TestUnlandKeepsADisjointLaterRun is acceptance (3): a later run that touched
// nothing this one did survives the unland intact.
func TestUnlandKeepsADisjointLaterRun(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
	f.land("t2", map[string]string{"beta.go": "package main\n\nfunc beta() int { return 2 }\n"}, "-verify", "go build ./...")

	out, code, err := f.unland(target.RunID, "-verify", "go build ./...")
	if err != nil || code != exitOK || out.Status != unlandStatusDone {
		t.Fatalf("unland: exit %d status %q err %v (%s)", code, out.Status, err, out.Message)
	}
	if len(out.Entangled) != 0 {
		t.Fatalf("disjoint later run reported as entangled: %+v", out.Entangled)
	}
	if _, present := f.blobAt(f.head(), "alpha.go"); present {
		t.Fatal("alpha.go survived the unland of the run that added it")
	}
	if _, present := f.blobAt(f.head(), "beta.go"); !present {
		t.Fatal("beta.go was destroyed by an unland of a run that never touched it")
	}
}

// TestUnlandBlockedByAnEntangledLaterRun is acceptance (4): a later run on the
// SAME path makes the inverse conflict, so nothing lands at all — the base ref
// is byte-identical before and after — and the inbox says which run and which
// path.
func TestUnlandBlockedByAnEntangledLaterRun(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"shared.txt": "first\n"}, "-verify", "go build ./...")
	later := f.land("t2", map[string]string{"shared.txt": "second\n"}, "-verify", "go build ./...")

	before := f.head()
	out, code, err := f.unland(target.RunID, "-verify", "go build ./...")
	if err != nil {
		t.Fatalf("unland returned an operational error: %v", err)
	}
	if out.Status != statusUnlandBlocked {
		t.Fatalf("status %q, want %q: %s", out.Status, statusUnlandBlocked, out.Message)
	}
	if code != exitOperationalError {
		t.Fatalf("exit %d, want %d — a blocked unland landed nothing and must not read as success", code, exitOperationalError)
	}
	if f.head() != before {
		t.Fatalf("a blocked unland moved the base from %s to %s", short(before), short(f.head()))
	}
	if got, _ := f.blobAt(f.head(), "shared.txt"); got != "second\n" {
		t.Fatalf("shared.txt is %q; the blocked unland must have changed nothing", got)
	}
	// The blast radius named the entangled run BEFORE any of that was attempted.
	if len(out.Entangled) != 1 || out.Entangled[0].RunID != later.RunID {
		t.Fatalf("entangled %+v, want exactly run %s", out.Entangled, later.RunID)
	}
	if paths := out.Entangled[0].Paths; len(paths) != 1 || paths[0] != "shared.txt" {
		t.Fatalf("entangled paths %v, want [shared.txt]", paths)
	}
	if len(out.Flagged) != 1 || len(out.Flagged[0].Paths) == 0 {
		t.Fatalf("flagged %+v, want the conflicted path named", out.Flagged)
	}

	// The inbox raises it as an attention item with nothing to ack.
	entries := inboxEntriesFor(context.Background(), f.g, "c", filepath.Join(f.runsDir, out.RunID), out.RunID, inboxUnlandBlocked, time.Now())
	if len(entries) != 1 {
		t.Fatalf("GET /inbox?type=unland-blocked yielded %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Unlands != target.RunID {
		t.Fatalf("inbox entry unlands %q, want %q", e.Unlands, target.RunID)
	}
	if len(e.Entangled) != 1 || e.Entangled[0] != later.RunID {
		t.Fatalf("inbox entry entangled %v, want [%s]", e.Entangled, later.RunID)
	}
	if !hasString(e.Paths, "shared.txt") {
		t.Fatalf("inbox entry paths %v, want the conflicted path shared.txt", e.Paths)
	}
	if _, ok := e.Links["ack"]; ok {
		t.Fatal("an unland-blocked entry offers an ack link; there is nothing to ack")
	}
}

// TestUnlandDryRunTouchesNothingAndMatchesTheEndpoint is acceptance (5) and (6):
// -dry-run creates no ref, no run dir and no ledger entry, and GET
// /runs/{id}/entangled returns the identical entangled data.
func TestUnlandDryRunTouchesNothingAndMatchesTheEndpoint(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"shared.txt": "first\n"}, "-verify", "go build ./...")
	later := f.land("t2", map[string]string{"shared.txt": "second\n"}, "-verify", "go build ./...")

	beforeHead, beforeDirs := f.head(), f.runDirs()
	beforeRefs, err := f.g.ForEachRefCommit(context.Background(), "refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	out, code, uerr := f.unland(target.RunID, "-dry-run")
	if uerr != nil || code != exitOK {
		t.Fatalf("dry run: exit %d err %v", code, uerr)
	}
	if out.Status != unlandStatusDryRun {
		t.Fatalf("status %q, want %q", out.Status, unlandStatusDryRun)
	}
	if len(out.Entangled) != 1 || out.Entangled[0].RunID != later.RunID {
		t.Fatalf("dry run entangled %+v, want run %s", out.Entangled, later.RunID)
	}
	if out.RunID != "" || out.Branch != "" {
		t.Fatalf("dry run claims run %q / branch %q; it must create neither", out.RunID, out.Branch)
	}
	if f.head() != beforeHead {
		t.Fatalf("dry run moved the base from %s to %s", short(beforeHead), short(f.head()))
	}
	if got := f.runDirs(); len(got) != len(beforeDirs) {
		t.Fatalf("dry run wrote a ledger entry: %v -> %v", beforeDirs, got)
	}
	afterRefs, err := f.g.ForEachRefCommit(context.Background(), "refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRefs) != len(beforeRefs) {
		t.Fatalf("dry run created a ref: %d branches -> %d", len(beforeRefs), len(afterRefs))
	}

	// (6) the read-only endpoint answers with the same blast radius.
	_, ts := newTestServer(t, "", f.repo)
	resp, err := http.Get(ts.URL + "/runs/" + target.RunID + "/entangled")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /entangled: status %d", resp.StatusCode)
	}
	var got entangledResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Unlandable {
		t.Fatalf("endpoint says unlandable=false (%s) but -dry-run planned it", got.Reason)
	}
	wantJSON, _ := json.Marshal(out.Entangled)
	gotJSON, _ := json.Marshal(got.Entangled)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("GET /entangled entangled = %s, want -dry-run's %s", gotJSON, wantJSON)
	}
}

// TestUnlandParksUnderUnlandPaths is acceptance (7): an inverse whose write-set
// matches an unland-paths glob does NOT land; it parks with unlandsRun recorded,
// raises the ordinary parked inbox entry, and an ack then lands the exact commit
// that passed verify.
func TestUnlandParksUnderUnlandPaths(t *testing.T) {
	f := newUnlandFixture(t, "unland-paths = migrations/**\n")
	target := f.land("t1", map[string]string{"migrations/001.sql": "-- one\n"}, "-verify", "go build ./...")

	before := f.head()
	out, code, err := f.unland(target.RunID, "-verify", "go build ./...")
	if err != nil || code != exitOK {
		t.Fatalf("unland: exit %d err %v", code, err)
	}
	if out.Status != statusAwaitingAck {
		t.Fatalf("status %q, want %q: %s", out.Status, statusAwaitingAck, out.Message)
	}
	// (finding 8) the outcome, not only the report, names which path held the ack,
	// so a -json consumer on the park path sees it — it was empty here before.
	if len(out.Flagged) != 1 || !hasString(out.Flagged[0].Paths, "migrations/001.sql") {
		t.Fatalf("outcome flagged %+v, want the ack-held path named", out.Flagged)
	}
	if f.head() != before {
		t.Fatalf("a parked unland moved the base from %s to %s", short(before), short(f.head()))
	}
	dir := filepath.Join(f.runsDir, out.RunID)
	pk, perr := readPark(dir)
	if perr != nil {
		t.Fatalf("readPark: %v", perr)
	}
	if pk.Reason != parkReasonUnlandPaths {
		t.Fatalf("park reason %q, want %q", pk.Reason, parkReasonUnlandPaths)
	}
	if pk.UnlandsRun != target.RunID {
		t.Fatalf("park unlandsRun %q, want %q", pk.UnlandsRun, target.RunID)
	}
	if st, _ := diskRunStatus(dir); st != statusAwaitingAck {
		t.Fatalf("run status %q on disk, want %s", st, statusAwaitingAck)
	}
	parked := inboxEntriesFor(context.Background(), f.g, "c", dir, out.RunID, inboxParked, time.Now())
	if len(parked) != 1 {
		t.Fatalf("inbox parked entries: %d, want 1", len(parked))
	}

	// The ack lands the EXACT commit that passed verify — the same guarantee an
	// ordinary park carries, which is only true because an unland parks through
	// the same record.
	verified := pk.VerifiedSHA
	ack, aerr := ackRun(context.Background(), f.c, dir, "test", "", ackEnv{Mode: envModeInherit})
	if aerr != nil {
		t.Fatalf("ack: %v", aerr)
	}
	if ack.LandedSHA != verified || f.head() != verified {
		t.Fatalf("ack landed %s and base is %s, want the verified commit %s", short(ack.LandedSHA), short(f.head()), short(verified))
	}
	if got, want := f.tree(f.head()), f.tree(target.BaseSHA); got != want {
		t.Fatalf("acked tree %s, want the pre-run tree %s", short(got), short(want))
	}
}

// TestUnlandParkReasonPrecedence is acceptance (8): ack-paths alone also parks an
// inverse, and policy-modified wins when both apply.
func TestUnlandParkReasonPrecedence(t *testing.T) {
	t.Run("ack-paths", func(t *testing.T) {
		// ack-paths is symmetric — the forward landing would have parked too — so
		// the honest way to get a landed run whose INVERSE trips it is a policy
		// that tightened after that run landed. That is also the realistic case.
		f := newUnlandFixture(t, "")
		target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
		f.syncWorktree()
		commitPolicy(t, f.g, f.repo, "ack-paths = alpha.go\n")

		got, code, err := f.unland(target.RunID, "-verify", "go build ./...")
		if err != nil || code != exitOK {
			t.Fatalf("unland: exit %d err %v", code, err)
		}
		if got.Status != statusAwaitingAck {
			t.Fatalf("status %q, want %q: %s", got.Status, statusAwaitingAck, got.Message)
		}
		pk, perr := readPark(filepath.Join(f.runsDir, got.RunID))
		if perr != nil {
			t.Fatal(perr)
		}
		if pk.Reason != parkReasonAckPaths {
			t.Fatalf("park reason %q, want %q — ack-paths binds an inverse too", pk.Reason, parkReasonAckPaths)
		}
		if pk.UnlandsRun != target.RunID {
			t.Fatalf("park unlandsRun %q, want %q", pk.UnlandsRun, target.RunID)
		}
	})

	t.Run("policy-modified wins", func(t *testing.T) {
		// Both keys match sigbound.policy, so the reason has to be decided by
		// precedence rather than by which check happened to run first.
		f := newUnlandFixture(t, "unland-paths = sigbound.policy\nack-paths = sigbound.policy\n")
		kind, _, _ := branchHoldReason([]string{policyFileName}, mustPolicy(t, "unland-paths = sigbound.policy\nack-paths = sigbound.policy\n"), true)
		if kind != parkReasonPolicyModified {
			t.Fatalf("hold reason %q, want %q", kind, parkReasonPolicyModified)
		}
		if r := parkReasonRank(parkReasonPolicyModified); r <= parkReasonRank(parkReasonUnlandPaths) || parkReasonRank(parkReasonUnlandPaths) <= parkReasonRank(parkReasonAckPaths) {
			t.Fatal("park reason precedence is not policy-modified > unland-paths > ack-paths")
		}
		_ = f
	})
}

func mustPolicy(t *testing.T, body string) policy {
	t.Helper()
	pol, err := parsePolicy([]byte(body))
	if err != nil {
		t.Fatalf("parsePolicy(%q): %v", body, err)
	}
	return pol
}

// TestUnlandRedVerifyBlocksAndNeverParks is acceptance (9): an inverse that
// integrates clean but fails verify lands nothing, produces NO park.json, and
// reports unland-blocked with the verify output attached. A park is a VERIFIED
// landing awaiting a human; parking a red one would make every park record less
// trustworthy.
func TestUnlandRedVerifyBlocksAndNeverParks(t *testing.T) {
	f := newUnlandFixture(t, "")
	// The run ADDS the file the verify command insists on, so reverting it is
	// exactly what turns verify red.
	target := f.land("t1", map[string]string{"needed.txt": "here\n"}, "-verify", "test -f needed.txt")

	before := f.head()
	out, code, err := f.unland(target.RunID, "-verify", "test -f needed.txt")
	if err != nil {
		t.Fatalf("unland returned an operational error: %v", err)
	}
	if out.Status != statusUnlandBlocked || code != exitOperationalError {
		t.Fatalf("status %q exit %d, want %q / %d: %s", out.Status, code, statusUnlandBlocked, exitOperationalError, out.Message)
	}
	if f.head() != before {
		t.Fatalf("a red inverse landed: base moved %s -> %s", short(before), short(f.head()))
	}
	if out.Verify == nil || !out.Verify.Ran || out.Verify.OK {
		t.Fatalf("verify record %+v, want a red one attached to the outcome", out.Verify)
	}
	if _, serr := os.Stat(filepath.Join(f.runsDir, out.RunID, parkFileName)); serr == nil {
		t.Fatal("a red inverse wrote a park.json — a park must always be a VERIFIED landing")
	}
	if len(out.Flagged) != 0 {
		t.Fatalf("flagged %+v on a clean fold; the block was the verify", out.Flagged)
	}
}

// TestUnlandRefusesARunThatNeverLanded is acceptance (10): a run with no landing
// is refused, nothing is created, and the HTTP door answers 409 not_landed.
func TestUnlandRefusesARunThatNeverLanded(t *testing.T) {
	f := newUnlandFixture(t, "")
	// A run whose verify fails lands nothing but still writes a ledger entry.
	rep, code, out := runRunJSON(t, f.repo, f.agent,
		[]taskSpec{taskWrite(t, "t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"})},
		"-verify", "exit 1")
	if code != exitVerifyFailed {
		t.Fatalf("setup: exit %d, want %d\n%s", code, exitVerifyFailed, out)
	}
	if landed(&rep) {
		t.Fatal("setup: the red run landed after all")
	}
	beforeDirs, beforeHead := f.runDirs(), f.head()

	_, ucode, uerr := f.unland(rep.RunID, "-verify", "go build ./...")
	if uerr == nil {
		t.Fatal("unland of a run that never landed succeeded")
	}
	if !errors.Is(uerr, errNotLanded) {
		t.Fatalf("error %v, want it to wrap errNotLanded", uerr)
	}
	if !strings.Contains(uerr.Error(), rep.RunID) {
		t.Fatalf("error %q does not name the run", uerr)
	}
	// It must be the landed() precondition that refused, not the ancestry guard
	// downstream of it: a red run's integration commit is also absent from the
	// base's history, so both would refuse and only the REASON tells them apart.
	if !strings.Contains(uerr.Error(), "verify failed") {
		t.Fatalf("error %q does not name the verify failure — this refusal came from a later guard, so landed() is untested here", uerr)
	}
	if ucode != exitOperationalError {
		t.Fatalf("exit %d, want %d", ucode, exitOperationalError)
	}
	if got := f.runDirs(); len(got) != len(beforeDirs) {
		t.Fatalf("a refused unland created a run dir: %v -> %v", beforeDirs, got)
	}
	if f.head() != beforeHead {
		t.Fatal("a refused unland moved the base")
	}

	// The case ONLY landed() catches: a run whose integration commit IS the base,
	// so it lands nothing while every ancestry guard downstream is satisfied.
	// Without landed() this would build an inverse and report a no-op — a run
	// that never landed treated as one that did.
	noop := seedRunID(t, f.runsDir, "20260725T235900Z-fffffe", runReport{
		Base: "main", BaseSHA: beforeHead,
		Integrate: integrateJSON{FinalSHA: beforeHead},
	})
	_, ncode, nerr := f.unland(noop, "-verify", "go build ./...")
	if nerr == nil || !errors.Is(nerr, errNotLanded) {
		t.Fatalf("unland of a run whose finalSHA is the base: err %v, want errNotLanded", nerr)
	}
	if !strings.Contains(nerr.Error(), "the ref never moved") {
		t.Fatalf("error %q does not say the ref never moved", nerr)
	}
	if ncode != exitOperationalError {
		t.Fatalf("exit %d, want %d", ncode, exitOperationalError)
	}

	_, ts := newTestServer(t, "", f.repo)
	resp, err := http.Post(ts.URL+"/runs/"+rep.RunID+"/unland", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST /unland: status %d, want 409", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != codeNotLanded {
		t.Fatalf("error code %q, want %q", body["code"], codeNotLanded)
	}
}

// TestUnlandRefusesALandingNoLongerInHistory is acceptance (11): a finalSHA the
// base no longer descends from is refused, naming both commits, and nothing is
// created. This is also the already-unlanded-by-other-means case.
func TestUnlandRefusesALandingNoLongerInHistory(t *testing.T) {
	f := newUnlandFixture(t, "")
	preRun := f.head()
	target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
	// Rewind the base past the landing — the shape a history rewrite leaves.
	if err := f.g.UpdateRef(context.Background(), "refs/heads/main", preRun); err != nil {
		t.Fatal(err)
	}
	beforeDirs := f.runDirs()

	_, code, err := f.unland(target.RunID, "-verify", "go build ./...")
	if err == nil {
		t.Fatal("unland of a landing no longer in history succeeded")
	}
	if !errors.Is(err, errNotLanded) {
		t.Fatalf("error %v, want it to wrap errNotLanded", err)
	}
	for _, want := range []string{short(target.Integrate.FinalSHA), short(preRun)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}
	if code != exitOperationalError {
		t.Fatalf("exit %d, want %d", code, exitOperationalError)
	}
	if got := f.runDirs(); len(got) != len(beforeDirs) {
		t.Fatalf("a refused unland created a run dir: %v -> %v", beforeDirs, got)
	}
}

// TestUnlandLedgerEdges is acceptance (12): the forward edge on the unland run,
// the reverse edge on the run it took back, and the unland-commit role.
func TestUnlandLedgerEdges(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
	out, _, err := f.unland(target.RunID, "-verify", "go build ./...")
	if err != nil {
		t.Fatal(err)
	}

	rows, _ := scanRuns(f.runsDir, 0)
	var row *logRow
	for i := range rows {
		if rows[i].ID == out.RunID {
			row = &rows[i]
		}
	}
	if row == nil {
		t.Fatalf("`sig log` does not list the unland run %s", out.RunID)
	}
	if row.Unlands != target.RunID {
		t.Fatalf("log row unlands %q, want %q", row.Unlands, target.RunID)
	}

	ctx := context.Background()
	p, ok := resolveProvenance(ctx, f.g, f.runsDir, out.LandedSHA)
	if !ok {
		t.Fatalf("no provenance for the unland's landed commit %s", short(out.LandedSHA))
	}
	if p.Role != "unland-commit" || p.Unlands != target.RunID {
		t.Fatalf("provenance role %q unlands %q, want unland-commit / %s", p.Role, p.Unlands, target.RunID)
	}
	back, ok := resolveProvenance(ctx, f.g, f.runsDir, target.Integrate.FinalSHA)
	if !ok {
		t.Fatalf("no provenance for the target's landed commit")
	}
	if back.UnlandedBy != out.RunID {
		t.Fatalf("provenance unlandedBy %q, want the unland run %q", back.UnlandedBy, out.RunID)
	}

	// The reverse edge is also on the TARGET run's own journal.
	events, rerr := os.ReadFile(filepath.Join(f.runsDir, target.RunID, "events.ndjson"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(events), `"event":"unlanded"`) || !strings.Contains(string(events), out.RunID) {
		t.Fatalf("the target run's events.ndjson has no unlanded line naming %s", out.RunID)
	}
}

// TestUnlandPathsPolicyKey is acceptance (13): the key parses, reaches the run
// report, changes the recorded hash, and a malformed line is a hard error naming
// the line.
func TestUnlandPathsPolicyKey(t *testing.T) {
	withKey := mustPolicy(t, "verify = go build ./...\nunland-paths = migrations/**, go.sum\n")
	if got := strings.Join(withKey.unlandPaths, ","); got != "migrations/**,go.sum" {
		t.Fatalf("unlandPaths %v, want both globs in file order", withKey.unlandPaths)
	}
	if rep := policyReport(withKey); rep == nil || strings.Join(rep.UnlandPaths, ",") != "migrations/**,go.sum" {
		t.Fatalf("policy report unlandPaths %+v, want both globs", rep)
	}
	// Repeatable, like ack-paths: a second line adds rather than erroring.
	if got := mustPolicy(t, "unland-paths = a/**\nunland-paths = b/**\n").unlandPaths; len(got) != 2 {
		t.Fatalf("unlandPaths %v, want the key to be repeatable", got)
	}
	without := mustPolicy(t, "verify = go build ./...\n")
	if withKey.hash == without.hash {
		t.Fatal("adding an unland-paths line did not change the recorded policy hash")
	}
	_, err := parsePolicy([]byte("unland-paths =\n"))
	if err == nil || !strings.Contains(err.Error(), "line 1") || !strings.Contains(err.Error(), "unland-paths") {
		t.Fatalf("empty unland-paths error %v, want a hard parse error naming the line and key", err)
	}
}

// TestGCSweepsAResolvedUnlandBranch is acceptance (15): the inverse branch is a
// gc candidate once the unland has landed, and untouchable while it is parked.
func TestGCSweepsAResolvedUnlandBranch(t *testing.T) {
	origTempRoot := gcTempRoot
	gcRoot := t.TempDir()
	gcTempRoot = func() string { return gcRoot }
	t.Cleanup(func() { gcTempRoot = origTempRoot })

	t.Run("landed is swept", func(t *testing.T) {
		f := newUnlandFixture(t, "")
		target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
		out, _, err := f.unland(target.RunID, "-verify", "go build ./...")
		if err != nil || out.Status != unlandStatusDone {
			t.Fatalf("unland: %v (%s)", err, out.Status)
		}
		plan := planGC(t, f.g, -time.Hour) // a future cutoff: everything is old enough
		if !hasString(plan.ToDelete, out.Branch) {
			t.Fatalf("gc keeps the resolved inverse %s (delete %v, keep %v)", out.Branch, plan.ToDelete, plan.ToKeep)
		}
	})

	t.Run("parked is protected", func(t *testing.T) {
		f := newUnlandFixture(t, "unland-paths = migrations/**\n")
		target := f.land("t1", map[string]string{"migrations/001.sql": "-- one\n"}, "-verify", "go build ./...")
		out, _, err := f.unland(target.RunID, "-verify", "go build ./...")
		if err != nil || out.Status != statusAwaitingAck {
			t.Fatalf("unland: %v (%s)", err, out.Status)
		}
		plan := planGC(t, f.g, -time.Hour)
		if hasString(plan.ToDelete, out.Branch) {
			t.Fatalf("gc would delete the PARKED inverse %s — the only copy of a verified landing", out.Branch)
		}
		if !plan.Parked[out.Branch] {
			t.Fatalf("gc does not record %s as parked-protected (keep %v)", out.Branch, plan.ToKeep)
		}
	})
}

func planGC(t *testing.T, g *gitx.Git, olderThan time.Duration) gcPlan {
	t.Helper()
	plan, err := gcPlanFor(context.Background(), g, olderThan, false)
	if err != nil {
		t.Fatalf("gcPlanFor: %v", err)
	}
	return plan
}

// TestUnlandWithoutAPolicy is acceptance (17): a repo with no sigbound.policy
// behaves exactly as with an empty one — nothing parks, the verify comes from
// -verify alone, and the report carries the unlands/source fields on top of an
// otherwise ordinary shape.
func TestUnlandWithoutAPolicy(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
	out, code, err := f.unland(target.RunID, "-verify", "go build ./...")
	if err != nil || code != exitOK || out.Status != unlandStatusDone {
		t.Fatalf("unland: exit %d status %q err %v", code, out.Status, err)
	}
	rep, rerr := readRunReport(filepath.Join(f.runsDir, out.RunID))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.Policy != nil {
		t.Fatalf("report carries a policy block %+v for a repo with no policy", rep.Policy)
	}
	if rep.Park != nil {
		t.Fatalf("report carries a park %+v with no policy to hold anything", rep.Park)
	}
	if rep.Unlands != target.RunID || rep.Source != "unland" {
		t.Fatalf("report unlands %q source %q, want %q / unland", rep.Unlands, rep.Source, target.RunID)
	}
	if rep.VerifyCmd != "go build ./..." {
		t.Fatalf("report verifyCmd %q, want the flag verbatim with no policy battery composed in", rep.VerifyCmd)
	}
	if !landed(rep) {
		t.Fatal("the unland's own ledger entry does not read as landed")
	}
}

// TestUnlandResolverLandsOverAnEntangledPath pins the ONE documented way past an
// entanglement: -resolver, the same `sh -c` contract `sig run` takes. Without it
// this exact repo state is blocked (TestUnlandBlockedByAnEntangledLaterRun), so
// this is also the negative control for that block being a real gate rather than
// an unconditional refusal.
func TestUnlandResolverLandsOverAnEntangledPath(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"shared.txt": "first\n"}, "-verify", "go build ./...")
	f.land("t2", map[string]string{"shared.txt": "second\n"}, "-verify", "go build ./...")

	out, code, err := f.unland(target.RunID, "-verify", "go build ./...", "-resolver", "printf 'resolved\\n'")
	if err != nil || code != exitOK {
		t.Fatalf("unland with a resolver: exit %d err %v (%s)", code, err, out.Message)
	}
	if out.Status != unlandStatusDone {
		t.Fatalf("status %q, want %q: %s", out.Status, unlandStatusDone, out.Message)
	}
	if got, _ := f.blobAt(f.head(), "shared.txt"); got != "resolved\n" {
		t.Fatalf("shared.txt is %q, want the resolver's output", got)
	}
	// The entanglement was still REPORTED — advisory, not a gate.
	if len(out.Entangled) != 1 {
		t.Fatalf("entangled %+v, want the overlap reported even though it landed", out.Entangled)
	}
}

// TestUnlandRefusesAnInvalidPolicyBeforeBuildingAnything holds the last
// fail-closed guard: a typo in sigbound.policy at the CURRENT head cannot
// silently drop the bar an inverse must clear.
func TestUnlandRefusesAnInvalidPolicyBeforeBuildingAnything(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
	f.syncWorktree()
	commitPolicy(t, f.g, f.repo, "unland-paths =\n")
	beforeDirs := f.runDirs()

	_, code, err := f.unland(target.RunID, "-verify", "go build ./...")
	if err == nil {
		t.Fatal("unland ran against an invalid policy")
	}
	if !strings.Contains(err.Error(), policyFileName) {
		t.Fatalf("error %q does not name %s", err, policyFileName)
	}
	if code != exitOperationalError {
		t.Fatalf("exit %d, want %d", code, exitOperationalError)
	}
	if got := f.runDirs(); len(got) != len(beforeDirs) {
		t.Fatalf("a refused unland created a run dir: %v -> %v", beforeDirs, got)
	}
}

// TestUnlandRefusesWhenTheBaseMovesUnderIt is the compare-and-swap for the FOURTH
// ref-moving path (findings 2 and 3): the inverse verifies GREEN, but the base
// moved out from under the landing swap, so the CAS refuses and NOTHING of the
// inverse lands. The interleaving is FORCED by program order, not raced — the
// -verify command itself advances refs/heads/main to a pre-built commit, exactly
// the technique TestDriveRunRefusesToLandOverAnInterveningLanding uses — so no
// sleeps, no goroutines. Downgrading landRef's UpdateRefCAS to UpdateRef makes
// this fail: the swap then resets the intervening landing away.
func TestUnlandRefusesWhenTheBaseMovesUnderIt(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
	head := target.Integrate.FinalSHA // the head the unland computes against

	// Build a competing landing on top of head, then put main back where the
	// unland will find it; the commit stays in the object store for -verify to land.
	f.syncWorktree()
	if err := os.WriteFile(filepath.Join(f.repo, "intervening.go"), []byte("package main\n\nfunc intervening() int { return 8 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	intervening, err := f.g.CommitAll(context.Background(), "landing that arrives while the unland verifies")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.g.UpdateRef(context.Background(), "refs/heads/main", head); err != nil {
		t.Fatal(err)
	}
	if f.head() != head {
		t.Fatalf("setup: main is %s, want it restored to %s", short(f.head()), short(head))
	}

	// -verify moves main to the intervening commit, then exits 0: the inverse
	// verifies green, but the base is no longer at head when the CAS runs.
	out, code, uerr := f.unland(target.RunID, "-verify",
		fmt.Sprintf("git -C %q update-ref refs/heads/main %s", f.repo, intervening))
	if uerr != nil {
		t.Fatalf("unland returned an operational error: %v", uerr)
	}
	if out.Status != statusUnlandBlocked || code != exitOperationalError {
		t.Fatalf("status %q exit %d, want unland-blocked / %d: %s", out.Status, code, exitOperationalError, out.Message)
	}
	// The intervening landing survives, and the inverse landed NOTHING.
	if f.head() != intervening {
		t.Fatalf("main is %s, want the intervening landing %s — the CAS reset it away", short(f.head()), short(intervening))
	}
	if _, present := f.blobAt(f.head(), "alpha.go"); !present {
		t.Fatal("alpha.go is gone — the inverse landed over the moved base")
	}
	if out.Verify == nil || !out.Verify.OK {
		t.Fatalf("verify %+v, want GREEN — the refusal must not read as a broken revert", out.Verify)
	}
	// The report says "someone else landed first": landRefused names the winner.
	rep, rerr := readRunReport(filepath.Join(f.runsDir, out.RunID))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if rep.LandRefused != intervening {
		t.Fatalf("rep.LandRefused=%q, want the intervening landing %s", rep.LandRefused, intervening)
	}
	// (finding 2) the land_refused event fires here, the same shape driveRun uses.
	names, recs := readEvents(t, filepath.Join(f.runsDir, out.RunID, "events.ndjson"))
	if indexOf(names, "land") != -1 {
		t.Fatal("a `land` event fired for a landing that was refused")
	}
	i := indexOf(names, "land_refused")
	if i == -1 {
		t.Fatalf("no land_refused event in %v", names)
	}
	if mv, _ := recs[i]["movedTo"].(string); mv != intervening {
		t.Fatalf("land_refused.movedTo=%q, want %s", mv, intervening)
	}
	if bs, _ := recs[i]["baseSHA"].(string); bs != head {
		t.Fatalf("land_refused.baseSHA=%q, want the head the unland computed against %s", bs, head)
	}
	if sha, _ := recs[i]["sha"].(string); sha == "" {
		t.Fatal("land_refused.sha is empty; it must name the commit that would have landed")
	}
}

// TestUnlandNamesAnAckReleasedLaterLanding is finding 4 / acceptance (4) for
// ack-released landings: a later run whose landing an ACK released records its
// SHA in park.json, not the report's integrate block, so the blast-radius scan
// once missed it — the block would say "unland those runs first" while naming
// none. The scan now reads the park's landedSHA (the same asymmetry
// notLandedReason already applies to the target), so the later run is named.
func TestUnlandNamesAnAckReleasedLaterLanding(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"shared.txt": "first\n"}, "-verify", "go build ./...")
	// A policy that tightened AFTER the target landed, so the later run's forward
	// landing trips ack-paths and PARKS rather than landing outright (ack-paths is
	// symmetric, so this is the honest way to get a parked landing on shared.txt).
	f.syncWorktree()
	commitPolicy(t, f.g, f.repo, "ack-paths = shared.txt\n")

	later, _, lout := runRunJSON(t, f.repo, f.agent,
		[]taskSpec{taskWrite(t, "t2", map[string]string{"shared.txt": "second\n"})}, "-verify", "go build ./...")
	if later.RunID == "" {
		t.Fatalf("later run has no id\n%s", lout)
	}
	laterDir := filepath.Join(f.runsDir, later.RunID)
	if st, _ := diskRunStatus(laterDir); st != statusAwaitingAck {
		t.Fatalf("later run status %q, want %s (setup expects it to park)\n%s", st, statusAwaitingAck, lout)
	}
	// Ack it: NOW it has landed, but only park.json records the landedSHA.
	if _, aerr := ackRun(context.Background(), f.c, laterDir, "test", "", ackEnv{Mode: envModeInherit}); aerr != nil {
		t.Fatalf("ack the later run: %v", aerr)
	}
	if lrep, _ := readRunReport(laterDir); landed(lrep) {
		t.Fatal("setup invalid: the acked run's REPORT reads as landed, so the park.json path would be untested")
	}

	// Unland the target. The ack-released landing overwrote shared.txt, so it must
	// be NAMED (and the conflict blocks the unland — the gate holds either way).
	out, code, err := f.unland(target.RunID, "-verify", "go build ./...")
	if err != nil {
		t.Fatalf("unland returned an operational error: %v", err)
	}
	if out.Status != statusUnlandBlocked || code != exitOperationalError {
		t.Fatalf("status %q exit %d, want unland-blocked: %s", out.Status, code, out.Message)
	}
	if len(out.Entangled) != 1 || out.Entangled[0].RunID != later.RunID {
		t.Fatalf("entangled %+v, want the acked later run %s named", out.Entangled, later.RunID)
	}
	if !hasString(out.Entangled[0].Paths, "shared.txt") {
		t.Fatalf("entangled paths %v, want shared.txt", out.Entangled[0].Paths)
	}
}

// TestUnlandRefusesWhenBaseSHADoesNotPrecedeFinalSHA is finding 6: the M6 guard
// (baseSHA must be an ancestor of finalSHA, or tree(baseSHA) is not the pre-run
// tree) had no test. An orphan baseSHA — a root commit off the landing's history
// — makes the pair undescribable, and the guard refuses it before anything is
// built. Removing the guard makes this fail.
func TestUnlandRefusesWhenBaseSHADoesNotPrecedeFinalSHA(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
	// An ORPHAN: a root commit with no path to the landing, so it is an ancestor
	// of nothing.
	orphan, err := f.g.CommitTree(context.Background(), f.tree(target.BaseSHA), nil, "orphan")
	if err != nil {
		t.Fatal(err)
	}
	// finalSHA is the REAL landing (still in main's history, so the later ancestry
	// guard would pass); landed() is satisfied (finalSHA != baseSHA), so control
	// reaches the precedence guard rather than stopping at an earlier one.
	seeded := seedRunID(t, f.runsDir, "20260725T235900Z-ffff01", runReport{
		Base: "main", BaseSHA: orphan, Integrate: integrateJSON{FinalSHA: target.Integrate.FinalSHA},
	})
	beforeDirs := f.runDirs()
	_, code, uerr := f.unland(seeded, "-verify", "go build ./...")
	if uerr == nil {
		t.Fatal("unland accepted a run whose baseSHA does not precede its finalSHA")
	}
	if !strings.Contains(uerr.Error(), "does not precede") {
		t.Fatalf("error %q does not name the precedence failure", uerr)
	}
	if code != exitOperationalError {
		t.Fatalf("exit %d, want %d", code, exitOperationalError)
	}
	if got := f.runDirs(); len(got) != len(beforeDirs) {
		t.Fatalf("a refused unland created a run dir: %v -> %v", beforeDirs, got)
	}
}

// TestUnlandRefusesAMalformedBase is finding 11: report.json's base decides which
// ref moves and is unvalidated, so a crafted base "refs/heads/main" once ran the
// whole fold and verify only to die in `git update-ref` on
// "refs/heads/refs/heads/main". It is now refused up front, the same way
// park.json's base is validated. Removing the guard makes this fail (a run dir is
// created and the error becomes the late git-lock one).
func TestUnlandRefusesAMalformedBase(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
	// baseSHA precedes finalSHA (so the M6 guard would pass); the base itself is
	// the malformed value under test.
	seeded := seedRunID(t, f.runsDir, "20260725T235901Z-ffff02", runReport{
		Base: "refs/heads/main", BaseSHA: target.BaseSHA, Integrate: integrateJSON{FinalSHA: target.Integrate.FinalSHA},
	})
	beforeDirs := f.runDirs()
	_, code, uerr := f.unland(seeded, "-verify", "go build ./...")
	if uerr == nil {
		t.Fatal("unland accepted a base that is already a qualified ref")
	}
	if !strings.Contains(uerr.Error(), "usable branch name") {
		t.Fatalf("error %q does not name the base validation failure", uerr)
	}
	if code != exitOperationalError {
		t.Fatalf("exit %d, want %d", code, exitOperationalError)
	}
	if got := f.runDirs(); len(got) != len(beforeDirs) {
		t.Fatalf("a refused unland built something: %v -> %v", beforeDirs, got)
	}
}

// TestUnlandBlockReasonNamesEachRefusalCause is finding 1: the inbox row must
// distinguish all FOUR ways an inverse fails to land — they once collapsed to
// "failed verify" for three of them. Each arm reads differently and matches the
// state the driver actually recorded.
func TestUnlandBlockReasonNamesEachRefusalCause(t *testing.T) {
	cases := []struct {
		name string
		rep  runReport
		want string
	}{
		{"cas-refusal", runReport{LandRefused: "deadbeefcafe", Verify: verifyJSON{Ran: true, OK: true}}, "the base moved"},
		{"conflict", runReport{Integrate: integrateJSON{Flagged: []flaggedJSON{{Branch: "unland/x", Paths: []string{"a.txt"}}}}}, "conflicts with work that landed since"},
		{"red-verify", runReport{Verify: verifyJSON{Ran: true, OK: false}}, "failed verify"},
		{"park-nil", runReport{Verify: verifyJSON{Ran: true, OK: true}}, "parking record could not be written"},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		got := unlandBlockReason(&tc.rep)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: reason %q does not contain %q", tc.name, got, tc.want)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("%s and %s both read %q — the four causes must each read differently", tc.name, other, got)
		}
		seen[got] = tc.name
	}
}

// TestUnlandReportsWhenTheScanStopsAtTheLimit is finding 7: a scan cut short by
// -limit says so, instead of handing back a silently-truncated entangled list.
func TestUnlandReportsWhenTheScanStopsAtTheLimit(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
	// Two LATER runs, so a -limit of 1 cannot reach the target.
	f.land("t2", map[string]string{"beta.go": "package main\n\nfunc beta() int { return 2 }\n"}, "-verify", "go build ./...")
	f.land("t3", map[string]string{"gamma.go": "package main\n\nfunc gamma() int { return 3 }\n"}, "-verify", "go build ./...")

	limited, _, err := f.unland(target.RunID, "-dry-run", "-limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !limited.ScanLimited {
		t.Fatal("the scan stopped at -limit 1 with newer runs unread, but ScanLimited is false")
	}
	full, _, err := f.unland(target.RunID, "-dry-run", "-limit", "0")
	if err != nil {
		t.Fatal(err)
	}
	if full.ScanLimited {
		t.Fatal("a full scan (-limit 0) reported ScanLimited")
	}
}

// TestEntangledEndpointHonorsLimit is finding 9: GET /runs/{id}/entangled honors
// ?limit= exactly as the flag does, so the doc claim that it and `sig unland
// -dry-run` "can never disagree" is true for the same limit, not only the default.
func TestEntangledEndpointHonorsLimit(t *testing.T) {
	f := newUnlandFixture(t, "")
	target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"}, "-verify", "go build ./...")
	f.land("t2", map[string]string{"beta.go": "package main\n\nfunc beta() int { return 2 }\n"}, "-verify", "go build ./...")
	f.land("t3", map[string]string{"gamma.go": "package main\n\nfunc gamma() int { return 3 }\n"}, "-verify", "go build ./...")
	_, ts := newTestServer(t, "", f.repo)

	dry, _, err := f.unland(target.RunID, "-dry-run", "-limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(ts.URL + "/runs/" + target.RunID + "/entangled?limit=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got entangledResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.ScanLimited || got.ScanLimited != dry.ScanLimited {
		t.Fatalf("endpoint scanLimited=%v, dry-run=%v; both must truncate at ?limit=1", got.ScanLimited, dry.ScanLimited)
	}
}

// TestProvenanceLineRendersUnlandedByOnUnlandCommit is finding 10: the reverse
// edge (", since unlanded by run …") is where it matters most — on the unland
// commit itself, which can also be taken back — yet its arm never rendered it. A
// no-op unland leaves UnlandedBy unset upstream, so the clause stays absent there.
func TestProvenanceLineRendersUnlandedByOnUnlandCommit(t *testing.T) {
	p := &provenance{Role: "unland-commit", SHA: "abc123def456", RunID: "20260725T101500Z-ab12cd", Unlands: "20260701T090000Z-ff01aa", Verify: "pass"}
	if line := provenanceLine(p); strings.Contains(line, "since unlanded") {
		t.Fatalf("an unland with no reverse edge must not render one: %q", line)
	}
	p.UnlandedBy = "20260726T120000Z-99ffee"
	line := provenanceLine(p)
	if !strings.Contains(line, "since unlanded by run 20260726T120000Z-99ffee") {
		t.Fatalf("the unland-commit line omits unlandedBy: %q", line)
	}
}

// TestUnlandRecordsAProvenanceNoteUnderAPolicy covers the deferred hypothesis
// (unland writes no git note): a clone carries refs/notes/sigbound but not
// .git/sigbound/runs, so without a note an unland is invisible to `sig log -sha`
// there. It is attached under the same condition and shape driveRun's -notes uses
// — a sigbound.policy present at the head.
func TestUnlandRecordsAProvenanceNoteUnderAPolicy(t *testing.T) {
	f := newUnlandFixture(t, "verify = go build ./...\n") // a policy file at the head flips notes on
	target := f.land("t1", map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"})
	out, code, err := f.unland(target.RunID)
	if err != nil || code != exitOK || out.Status != unlandStatusDone {
		t.Fatalf("unland: exit %d status %q err %v (%s)", code, out.Status, err, out.Message)
	}
	content, ok, nerr := f.g.NoteShow(context.Background(), "sigbound", out.LandedSHA)
	if nerr != nil {
		t.Fatal(nerr)
	}
	if !ok {
		t.Fatal("no git note on the unland's landed commit — a clone could not see the unland via `sig log -sha`")
	}
	if !strings.Contains(content, `"unlands"`) || !strings.Contains(content, target.RunID) {
		t.Fatalf("the note does not carry the unland's report (unlands %s):\n%s", target.RunID, content)
	}
}
