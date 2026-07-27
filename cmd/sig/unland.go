// `sig unland RUN_ID` (issue #149): take back exactly one landed run's
// contribution to the base branch, THROUGH THE LANDING GATE. Every other part of
// this product can make change happen; this is the part that can take it back,
// and it is held to the identical bar — an unland that breaks the build is
// exactly as unacceptable as a landing that does.
//
// It is NOT `git revert` of the landing commit and NOT a history rewrite. The
// inverse is one commit carrying the run's PRE-run tree, parented on the commit
// it landed:
//
//	inverse = commit-tree(tree(target.baseSHA), parents=[target.finalSHA])
//
// so `diff target.finalSHA..inverse` is that run's contribution reversed and
// nothing else — no patch application, no -m parent selection, and no dependence
// on the landing having been a merge commit (overlay landings are not). That
// branch is then an ORDINARY input to machinery that already exists: it is folded
// onto the base's CURRENT head through cell.IntegrateOnto with finalSHA as the
// 3-way merge base, verified under the policy at that head, and either landed
// through landRef's compare-and-swap or parked for a human — exactly as a run is.
//
// It FAILS CLOSED. A path a later run also touched conflicts and stops the whole
// thing; a red inverse lands nothing and is NOT parked (a park is a verified
// landing awaiting a human, and weakening that would make every park record less
// trustworthy). There is deliberately no -force: an entangled inverse is resolved
// with -resolver, by unlanding the entangled runs too, or by a human — never by
// landing a half-revert.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

// unlandBranchPrefix namespaces the inverse branch. Keyed on the NEW run's id,
// not the target's, so repeated unlands of one run never collide (the same
// reasoning as parkRefKey). It is a gcBranchPrefixes member, so a resolved
// inverse is sweepable; an unresolved park protects it unconditionally by name
// (see loadParkedBranches).
const unlandBranchPrefix = "unland/"

// unlandDefaultLimit bounds how many NEWER run manifests the blast-radius scan
// reads. The scan costs one report read plus one `git diff --name-only` per
// newer run, so a repo with a long history pays for a bound rather than a full
// walk. -limit 0 walks them all.
const unlandDefaultLimit = 200

// statusUnlandBlocked is the run status of an unland whose inverse could not be
// offered as a landing at all — it conflicted with a later run, or it verified
// red. Terminal and DURABLE: recoverStaleRuns only rewrites queued/running, so
// it survives restarts, and there is nothing to ack (the inverse branch is kept
// for a human to resolve with -resolver, or to unland the entangled runs first).
const statusUnlandBlocked = "unland-blocked"

// unland outcome statuses, alongside statusAwaitingAck and statusUnlandBlocked.
const (
	unlandStatusDone = "done"
	// unlandStatusNoOp: the fold produced the tree the base already has, i.e.
	// this run's contribution is already gone (most often: it was unlanded
	// before). Nothing lands and that is not an error.
	unlandStatusNoOp = "no-op"
	// unlandStatusDryRun is -dry-run's verdict: preconditions passed and the
	// blast radius below is what an unland would face. Nothing was built.
	unlandStatusDryRun = "dry-run"
)

// errNotLanded is the sentinel both front doors map to a refusal: the target run
// never actually advanced the base ref (no finalSHA, finalSHA == baseSHA, or a
// red verify — see landed), or its landing is no longer in the base's history.
// A sentinel rather than a string match, so the HTTP status and the error text
// stay independent (issue #93).
var errNotLanded = errors.New("that run did not land, so there is nothing to unland")

// entangledRun is one LATER landed run whose write-set overlaps the run being
// unlanded, with the paths they share. The overlap test is cell.WriteSet.Overlaps
// — the same comparison cell.Partition uses to decide which branches may land in
// parallel, applied backwards in time.
//
// It is ADVISORY, not a gate: an overlap on the same path is what produces a
// conflict, and the CONFLICT is the gate. A run that overlaps but whose changes
// still merge cleanly lands. Reporting the overlap up front is what makes the
// outcome predictable before the verify cost is paid.
type entangledRun struct {
	RunID     string   `json:"runId"`
	StartedAt string   `json:"startedAt,omitempty"`
	FinalSHA  string   `json:"finalSHA,omitempty"`
	Paths     []string `json:"paths"`
}

// unlandOutcome is what an unland did — the CLI's -json output and the POST
// response body, one shape for both front doors.
type unlandOutcome struct {
	RunID     string         `json:"runId,omitempty"` // the UNLAND run's id; empty on -dry-run, which creates none
	Unlands   string         `json:"unlands"`
	Status    string         `json:"status"` // done | awaiting-ack | unland-blocked | no-op | dry-run
	Branch    string         `json:"branch,omitempty"`
	LandedSHA string         `json:"landedSHA,omitempty"`
	WriteSet  []string       `json:"writeSet,omitempty"`
	Entangled []entangledRun `json:"entangled,omitempty"`
	// ScanLimited is true when the blast-radius scan stopped at -limit before
	// reaching the target, so Entangled may be missing later runs. A -json
	// consumer sees the list is partial rather than trusting it as complete.
	ScanLimited bool          `json:"scanLimited,omitempty"`
	Flagged     []flaggedJSON `json:"flagged,omitempty"`
	Verify      *verifyJSON   `json:"verify,omitempty"`
	Message     string        `json:"message"`
}

// unlandParams is one unland's resolved configuration. Verify/Resolver come from
// the invoker; the policy's own battery is composed on top by resolvePolicy, so
// a repo with a `verify` line needs no flag.
type unlandParams struct {
	TargetID      string
	VerifyCmd     string
	VerifyRetries int
	ResolverCmd   string
	Reason        string
	Limit         int
	DryRun        bool
	Actor         string // "cli" or "http", recorded on the target run's unlanded event
	Env           ackEnv
}

// unlandPlan is everything the preconditions and the blast-radius scan
// established about one target run, before anything is built. Every field is
// read-only evidence: no ref has moved and no run dir exists yet.
type unlandPlan struct {
	Base     string // the target run's base BRANCH
	BaseSHA  string // the target's fork point — the tree the inverse restores
	FinalSHA string // the commit the target landed
	Head     string // that branch's head right now; the inverse folds onto this
	// WriteSet is the target run's own contribution, `git diff --name-only
	// baseSHA...finalSHA`. It is also the inverse's write-set exactly, which is
	// what makes the policy check below honest without building anything.
	WriteSet  []string
	Entangled []entangledRun
	// Truncated is set when the blast-radius scan stopped at -limit with newer
	// runs still unread, so the entangled list may be incomplete — reported, not
	// silently swallowed, since -limit exists for the long-history repo that is
	// exactly where the scan gets cut short.
	Truncated bool
}

// planUnland runs every precondition and the blast-radius scan. It touches
// nothing: on any refusal NO branch, run dir, or ref has been created, which is
// the whole reason the guards live here rather than inline in the driver.
//
// The guards, all fail-closed:
//
//   - report.json missing or unparseable => refuse naming the run dir. An
//     unreadable ledger entry is not evidence of what landed.
//   - landed(rep) false => errNotLanded.
//   - baseSHA not an ancestor of finalSHA => refuse. The recorded pair does not
//     describe a landing this can invert.
//   - finalSHA not an ancestor of the base's current head => refuse naming both.
//     History was rewritten, or that landing is already gone.
func planUnland(ctx context.Context, g *gitx.Git, runsDir, targetID string, limit int) (*unlandPlan, error) {
	dir := filepath.Join(runsDir, targetID)
	rep, err := readRunReport(dir)
	if err != nil {
		return nil, fmt.Errorf("read the run ledger entry %s: %w", filepath.Join(dir, "report.json"), err)
	}
	if !landed(rep) {
		return nil, fmt.Errorf("%w: run %s (%s)", errNotLanded, targetID, notLandedReason(dir, rep))
	}
	// The report's base decides which ref an unland moves, and landRef prepends
	// "refs/heads/" to it, so it must be a plain branch name — validated the same
	// way park.json's base is (usableBranchName), plus a guard on an already-
	// qualified ref, BEFORE the whole verify is paid. A crafted base like
	// "refs/heads/main" would otherwise run the entire fold and verify only to die
	// in `git update-ref` on "refs/heads/refs/heads/main".
	if !usableBranchName(rep.Base) || strings.HasPrefix(rep.Base, "refs/") {
		return nil, fmt.Errorf("run %s records base %q, which is not a usable branch name", targetID, rep.Base)
	}
	head, err := g.RevParse(ctx, rep.Base)
	if err != nil {
		return nil, fmt.Errorf("resolve base %q: %w", rep.Base, err)
	}
	// The recorded pair must describe a landing at all: baseSHA is where the
	// run's branches forked, finalSHA what the ref advanced to, so the former
	// must be an ancestor of the latter or tree(baseSHA) is not the pre-run tree.
	switch anc, aerr := g.IsAncestor(ctx, rep.BaseSHA, rep.Integrate.FinalSHA); {
	case aerr != nil:
		return nil, fmt.Errorf("ancestry of %s from %s: %w", short(rep.Integrate.FinalSHA), short(rep.BaseSHA), aerr)
	case !anc:
		return nil, fmt.Errorf("run %s records baseSHA %s and finalSHA %s, but the former does not precede the latter — that pair does not describe a landing this can invert",
			targetID, short(rep.BaseSHA), short(rep.Integrate.FinalSHA))
	}
	switch anc, aerr := g.IsAncestor(ctx, rep.Integrate.FinalSHA, head); {
	case aerr != nil:
		return nil, fmt.Errorf("ancestry of %s from %s: %w", short(head), short(rep.Integrate.FinalSHA), aerr)
	case !anc:
		return nil, fmt.Errorf("%w: run %s landed %s, which is no longer in %s's history (now at %s) — the history was rewritten, or that landing is already gone",
			errNotLanded, targetID, short(rep.Integrate.FinalSHA), rep.Base, short(head))
	}
	ws, err := g.DiffNameOnly(ctx, rep.BaseSHA, rep.Integrate.FinalSHA)
	if err != nil {
		return nil, fmt.Errorf("write-set of run %s: %w", targetID, err)
	}
	ent, truncated, err := blastRadius(ctx, g, runsDir, targetID, ws, limit)
	if err != nil {
		return nil, err
	}
	return &unlandPlan{
		Base: rep.Base, BaseSHA: rep.BaseSHA, FinalSHA: rep.Integrate.FinalSHA,
		Head: head, WriteSet: ws, Entangled: ent, Truncated: truncated,
	}, nil
}

// notLandedReason says WHICH of landed()'s three conditions the run failed, so a
// refusal names the actual state rather than restating the rule.
//
// KNOWN LIMIT, stated here because the bare reason would otherwise mislead: a
// landing an ACK released is recorded in park.json, not in the report's
// integrate block, so landed() reads false for it and this refuses. `sig unland`
// inverts what the run REPORT says landed; a park an ack released has to be
// undone by hand (or by a fresh run) until the ledger records it.
func notLandedReason(dir string, rep *runReport) string {
	// ackedLandedSHA, not a raw park read: it is the one definition of "did this
	// ack actually land" (see park.go), so a park whose landRef failed is not
	// described here as a landing an ack released.
	if ackedLandedSHA(dir) != "" {
		return "its landing was released by an ack, which the run's own ledger entry does not record — unland inverts what the report says landed, so it cannot invert this one"
	}
	switch {
	case rep.Integrate.FinalSHA == "":
		return "it recorded no integration commit"
	case rep.Integrate.FinalSHA == rep.BaseSHA:
		return "its integration commit is the base itself, so the ref never moved"
	default:
		return "its verify failed, so nothing was landed"
	}
}

// blastRadius names the LATER landed runs whose write-sets overlap the target's.
// Run ids are timestamp-prefixed and runDirNames sorts descending, so the walk
// starts at the newest run and stops the moment it reaches the target — every
// run older than it is by construction not "later". limit bounds how many newer
// run dirs are read (0 = all); the bound is on dirs WALKED, not on matches, so
// the cost is what it says on the flag.
//
// A newer run whose report or diff cannot be read is SKIPPED rather than fatal:
// this scan is advisory (the conflict is the gate), so an unreadable neighbour
// costs a line of reporting, never a wrong landing.
func blastRadius(ctx context.Context, g *gitx.Git, runsDir, targetID string, writeSet []string, limit int) (ent []entangledRun, truncated bool, err error) {
	ws := cell.NewWriteSet(writeSet...)
	var out []entangledRun
	walked := 0
	for _, id := range runDirNames(runsDir) {
		if id <= targetID {
			break // reached the target: everything from here back is older
		}
		if limit > 0 && walked >= limit {
			truncated = true // stopped early: newer runs beyond the bound went unread
			break
		}
		walked++
		dir := filepath.Join(runsDir, id)
		rep, rerr := readRunReport(dir)
		if rerr != nil {
			continue
		}
		base, final := rep.BaseSHA, rep.Integrate.FinalSHA
		if !landed(rep) {
			// An ACK released this landing: its SHA lives in park.json, not the
			// report's integrate block — the same asymmetry notLandedReason reads
			// for the target run. Without this, an acked landing that overwrote a
			// shared path is invisible to the scan, and the block message would tell
			// the operator to "unland those runs first" while naming none.
			//
			// ackedLandedSHA is the ONE definition of "did this ack actually land"
			// (shared with landed/readLogRow/foldMetrics): it gates on the run
			// status as well as resolvedAt, so a park whose landRef failed for any
			// reason other than ErrRefMoved is never counted. The park is then read
			// for its baseSHA — the fork point this landing's contribution is
			// measured from, which the SHA alone does not carry.
			sha := ackedLandedSHA(dir)
			if sha == "" {
				continue
			}
			pk, perr := readPark(dir)
			if perr != nil {
				continue
			}
			base, final = pk.BaseSHA, sha
		}
		paths, derr := g.DiffNameOnly(ctx, base, final)
		if derr != nil {
			continue
		}
		if !ws.Overlaps(cell.NewWriteSet(paths...)) {
			continue
		}
		shared := make([]string, 0, 1)
		for _, p := range paths {
			if ws.Contains(p) {
				shared = append(shared, p)
			}
		}
		out = append(out, entangledRun{RunID: id, StartedAt: rep.StartedAt, FinalSHA: final, Paths: shared})
	}
	return out, truncated, nil
}

// unlandRun is THE unland code path — `sig unland` and POST /runs/{id}/unland
// are two front doors onto this one function, exactly as ackRun is for an ack.
// It writes a run dir on BOTH doors, unlike `sig run` (which writes one only
// under `sig serve`): an unland can PARK, a park is a durable record, and `sig
// ack RUN_ID -repo PATH` already resolves run dirs from the CLI.
//
// Ordering is the contract. Nothing durable is created until every precondition
// has passed and the policy at the current head has parsed, so a refusal leaves
// the repository byte-identical; and landRef is the LAST step, so a crash
// anywhere before it landed nothing.
func unlandRun(ctx context.Context, c *cell.Cell, p unlandParams) (unlandOutcome, error) {
	g := c.Git()
	out := unlandOutcome{Unlands: p.TargetID}
	runsDir, err := cellRunsDir(ctx, c)
	if err != nil {
		return out, err
	}
	plan, err := planUnland(ctx, g, runsDir, p.TargetID, p.Limit)
	if err != nil {
		return out, err
	}
	out.WriteSet, out.Entangled, out.ScanLimited = plan.WriteSet, plan.Entangled, plan.Truncated
	// The policy at the head this would land on, loaded BEFORE anything is built:
	// a typo in sigbound.policy must not silently drop the bar an inverse has to
	// clear, exactly as for a run.
	pol, err := loadPolicy(ctx, g, plan.Head)
	if err != nil {
		return out, err
	}
	if p.DryRun {
		out.Status = unlandStatusDryRun
		out.Message = fmt.Sprintf("would unland %s from %s (%s, %s entangled); nothing was built",
			p.TargetID, plan.Base, plural(len(plan.WriteSet), "path", "paths"), plural(len(plan.Entangled), "later run", "later runs"))
		return out, nil
	}

	rp := runParams{
		Repo: c.Repo(), Base: plan.Base,
		VerifyCmd: p.VerifyCmd, VerifyRetries: p.VerifyRetries,
		ResolverCmd: p.ResolverCmd, ResolverTimeout: parkResolverTimeout,
		EnvMode: p.Env.Mode, EnvVerify: p.Env.Verify, EnvResolver: p.Env.Resolver,
	}
	// The RAW, pre-policy verify: a park records THIS, so a later ack re-composes
	// the battery against the policy at whatever base it finds rather than running
	// the policy's members twice. Same contract as driveRun's rawVerifyCmd.
	rawVerify := rp.VerifyCmd
	if err := resolvePolicy(pol, &rp, 1); err != nil {
		return out, err
	}

	dir, runID, err := startRunDir(ctx, g, "")
	if err != nil {
		return out, err
	}
	rp.RunID = runID
	out.RunID = runID
	emit, closeEvents, err := newEventEmitter(filepath.Join(dir, "events.ndjson"), nil)
	if err != nil {
		return out, err
	}
	defer closeEvents()
	emit.emit("unland_start", map[string]any{
		"target": p.TargetID, "reason": p.Reason, "writeSet": plan.WriteSet, "entangled": plan.Entangled,
	})

	rep := runReport{
		RunID: runID, Repo: c.Repo(), Base: plan.Base, BaseSHA: plan.Head,
		LaneMode: laneOff, EnvMode: rp.EnvMode, Strategy: "onto",
		Tasks: []taskSpec{}, PerAgent: []perAgentJSON{},
		ResolverCmd: rp.ResolverCmd, VerifyCmd: rp.VerifyCmd,
		Version: Version, StartedAt: time.Now().UTC().Format(time.RFC3339),
		Source: "unland", Unlands: p.TargetID, Entangled: plan.Entangled,
		Policy:    policyReport(pol),
		Integrate: integrateJSON{Strategy: "onto", Groups: 1, Landed: []string{}, Flagged: []flaggedJSON{}},
	}
	// finish journals the run dir on every exit path. The ORDERING is
	// finishRunDir's contract, for the same reason: an unresolved park.json
	// outranks the phase marker (see diskRunStatus), so the record must be on disk
	// before the marker flips. A park.json that cannot be written is a HARD
	// failure — it is the only record of a landing that has not landed — unlike
	// the best-effort report and status writes around it.
	finish := func(o unlandOutcome, status string) (unlandOutcome, error) {
		rep.Verify = derefVerify(o.Verify)
		writeRunReport(dir, rep)
		if rep.Park != nil {
			if err := writePark(dir, rep.Park); err != nil {
				writeRunStatus(dir, "error", "write parking record: "+err.Error())
				return o, fmt.Errorf("write parking record: %w", err)
			}
		}
		writeRunStatus(dir, status, o.Message)
		emit.emit("unland_done", map[string]any{"status": o.Status, "landedSHA": o.LandedSHA})
		return o, nil
	}

	branch := unlandBranchPrefix + runID
	out.Branch = branch
	if err := buildInverse(ctx, g, branch, plan, p); err != nil {
		return finishErr(finish, out, err)
	}

	// The fold: the inverse's OWN changes against its fork point (finalSHA),
	// applied to the base as it stands now. This is what turns "a later run
	// touched this path" into a flagged conflict rather than the silent clobber
	// issue #130 describes — and it is the same integrateVerifyPark an ack's
	// re-verify goes through, so a tree an unland lands and a tree an ack lands
	// are gated by identical code.
	finalSHA, v, flagged, ferr := integrateVerifyPark(ctx, c, rp, plan.FinalSHA, plan.Head, []string{branch})
	out.Verify, out.Flagged = &v, flagged
	if ferr != nil || len(flagged) > 0 || (v.Ran && !v.OK) {
		rep.Integrate.Flagged = flagged
		out.Status = statusUnlandBlocked
		out.Message = blockedMessage(p.TargetID, plan, flagged, v, ferr)
		return finish(out, statusUnlandBlocked)
	}

	// Already gone: the fold reproduced the tree the base already has, so there
	// is nothing to land. Not an error — a second unland of one run reports this.
	sameTree, err := treesEqual(ctx, g, finalSHA, plan.Head)
	if err != nil {
		return finishErr(finish, out, err)
	}
	if sameTree {
		out.Status = unlandStatusNoOp
		out.Message = fmt.Sprintf("run %s's contribution is already absent from %s; nothing to land", p.TargetID, plan.Base)
		return finish(out, unlandStatusDone)
	}

	// Policy hold. The inverse's write-set is the target run's write-set exactly,
	// so this is decided from the plan rather than re-diffed. ack-paths binds an
	// inverse too — a path that needs an ack to change needs an ack to change back
	// — and self-protection covers sigbound.policy by construction.
	if kind, reason, matched := branchHoldReason(plan.WriteSet, pol, true); kind != "" {
		groups := []parkGroupJSON{{Branches: []string{branch}, MatchedPaths: matched, Reason: kind}}
		pk := parkRecord(ctx, c, rp, pol, plan.FinalSHA, plan.Head, rawVerify, groups, finalSHA, v, emit)
		if pk == nil {
			// parkRecord fails closed when it cannot pin the verified commit; with
			// nothing ackable there is nothing to offer, so report it as blocked
			// rather than land a tree policy says a human must see first.
			out.Status = statusUnlandBlocked
			out.Message = "could not record a parking record for the inverse; nothing landed"
			return finish(out, statusUnlandBlocked)
		}
		pk.UnlandsRun, pk.Entangled = p.TargetID, plan.Entangled
		rep.Park = pk
		rep.Integrate.Flagged = []flaggedJSON{{Branch: branch, Paths: sortedKeys(matched), Reason: reason}}
		// Surface the hold on the outcome too: out.Flagged was set once from the
		// (clean) integrate above and never updated on this branch, so a -json
		// consumer could not see which path forced the ack. Issue #149's
		// unlandOutcome example shows it populated.
		out.Flagged = rep.Integrate.Flagged
		out.Status = statusAwaitingAck
		out.Message = fmt.Sprintf("verified the inverse of %s as %s but did not land it: %s — ack or reject it",
			p.TargetID, short(finalSHA), reason)
		return finish(out, statusAwaitingAck)
	}

	// THE LANDING. Same landRef, same compare-and-swap, as every other path: the
	// swap applies only while the base still holds the head this was computed
	// against, so a landing that arrived in between is refused rather than reset
	// away. This is the last durable step — a crash before it landed nothing.
	if err := landRef(ctx, g, plan.Base, plan.Head, finalSHA); err != nil {
		if errors.Is(err, gitx.ErrRefMoved) {
			moved, _ := g.RevParse(ctx, plan.Base)
			rep.LandRefused = moved
			// Same event, same shape, as driveRun's identical refusal (run.go): a
			// consumer watching for a refused landing must see it on this path too,
			// or the fourth ref-mover is the one that stays silent.
			emit.emit("land_refused", map[string]any{"sha": finalSHA, "baseSHA": plan.Head, "movedTo": moved})
			out.Status = statusUnlandBlocked
			out.Message = fmt.Sprintf("nothing landed: %s moved to %s while this unland was computing against %s — run it again against the new head",
				plan.Base, shortMoved(moved), short(plan.Head))
			return finish(out, statusUnlandBlocked)
		}
		return finishErr(finish, out, fmt.Errorf("land %s: %w", short(finalSHA), err))
	}
	rep.Integrate.Landed, rep.Integrate.FinalSHA = []string{branch}, finalSHA
	emit.emit("land", map[string]any{"sha": finalSHA})
	// Record the unland on the landed commit as a git note — the same shape,
	// condition, and best-effort posture as driveRun's -notes default-flip (issue
	// #110): a sigbound.policy at the head means the repo wants its landings
	// recorded. A clone carries refs/notes/sigbound but not .git/sigbound/runs, so
	// without this an unland is invisible to `sig log -sha` there. Namespaced and
	// non-pushing by default, exactly like a run's; there is simply no -notes flag
	// to opt out on this door.
	if _, present, perr := g.BlobAt(ctx, plan.Head, policyFileName); perr == nil && present {
		rep.Verify = derefVerify(out.Verify)
		attachNote(ctx, g, finalSHA, rep, "unland")
	}
	out.Status, out.LandedSHA = unlandStatusDone, finalSHA
	out.Message = fmt.Sprintf("unlanded run %s: %s now at %s", p.TargetID, plan.Base, short(finalSHA))
	// The reverse edge on the TARGET run's own journal, through the same
	// appendRunEvent ack/reject/repark use for events after a driver returned.
	appendRunEvent(filepath.Join(runsDir, p.TargetID), "unlanded", map[string]any{
		"by": runID, "sha": finalSHA, "actor": p.Actor,
	})
	return finish(out, unlandStatusDone)
}

// buildInverse creates the commit carrying the target run's PRE-run tree,
// parented on the commit it landed, and points branch at it. That parent is what
// makes the branch's own contribution — measured against its fork point, which
// is what IntegrateOnto merges from — exactly the reversal of the run and
// nothing else.
func buildInverse(ctx context.Context, g *gitx.Git, branch string, plan *unlandPlan, p unlandParams) error {
	tree, err := g.TreeOID(ctx, plan.BaseSHA)
	if err != nil {
		return fmt.Errorf("tree of %s: %w", short(plan.BaseSHA), err)
	}
	msg := "sigbound: unland " + p.TargetID
	if r := strings.TrimSpace(p.Reason); r != "" {
		msg += "\n\n" + r
	}
	inverse, err := g.CommitTree(ctx, tree, []string{plan.FinalSHA}, msg)
	if err != nil {
		return fmt.Errorf("commit the inverse tree: %w", err)
	}
	if err := g.UpdateRef(ctx, "refs/heads/"+branch, inverse); err != nil {
		return fmt.Errorf("create %s: %w", branch, err)
	}
	return nil
}

// blockedMessage explains an inverse that could not be offered as a landing.
// A conflict names the entangled runs, because "resolve it, or unland them too"
// is the operator's actual next move and the run ids are what they need for it.
func blockedMessage(target string, plan *unlandPlan, flagged []flaggedJSON, v verifyJSON, err error) string {
	switch {
	case err != nil:
		return "nothing landed: the inverse could not be integrated: " + err.Error()
	case len(flagged) > 0:
		msg := fmt.Sprintf("nothing landed: the inverse of %s conflicts on %s", target, strings.Join(flagged[0].Paths, ", "))
		if ids := entangledIDs(plan.Entangled); len(ids) > 0 {
			msg += " — entangled with " + strings.Join(ids, ", ")
		}
		return msg + "; resolve with -resolver, or unland those runs first"
	default:
		return "nothing landed: the reverted tree failed verify:\n" + tail(v.Output, parkVerifyOutputMax)
	}
}

func entangledIDs(ent []entangledRun) []string {
	out := make([]string, 0, len(ent))
	for _, e := range ent {
		out = append(out, e.RunID)
	}
	return out
}

// treesEqual reports whether two commits carry the identical tree OID.
func treesEqual(ctx context.Context, g *gitx.Git, a, b string) (bool, error) {
	ta, err := g.TreeOID(ctx, a)
	if err != nil {
		return false, fmt.Errorf("tree of %s: %w", short(a), err)
	}
	tb, err := g.TreeOID(ctx, b)
	if err != nil {
		return false, fmt.Errorf("tree of %s: %w", short(b), err)
	}
	return ta == tb, nil
}

// finishErr records an operational failure in the run dir before returning it,
// so a crash-shaped unland leaves the same journal a served run's failure does
// rather than a dir stuck at "running".
func finishErr(finish func(unlandOutcome, string) (unlandOutcome, error), out unlandOutcome, err error) (unlandOutcome, error) {
	out.Status, out.Message = "error", err.Error()
	_, _ = finish(out, "error")
	return out, err
}

// derefVerify renders an optional verify record as the report's value field.
func derefVerify(v *verifyJSON) verifyJSON {
	if v == nil {
		return verifyJSON{}
	}
	return *v
}

// ---- CLI: sig unland ----

func runUnland(w io.Writer, argv []string) (int, error) {
	fs := flag.NewFlagSet("unland", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig unland RUN_ID -repo PATH [-verify CMD] [-resolver CMD] [-reason TEXT] [-limit 200] [-dry-run] [-json]")
		fmt.Fprintln(fs.Output(), "take back one landed run's contribution as a NEW commit on the base, through the")
		fmt.Fprintln(fs.Output(), "normal gate: the reverted tree is verified and only then landed (or parked for an")
		fmt.Fprintln(fs.Output(), "ack). A conflict with a later run, or a red verify, lands nothing at all.")
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "path to the target git repository (the cell whose run history holds RUN_ID)")
	verify := fs.String("verify", "", "command run in a detached checkout of the REVERTED tree; appended to the policy's verify battery, which is never weakened. Non-zero exit lands nothing")
	verifyRetries := fs.Int("verify-retries", 0, "after a FAILING -verify invocation, re-run it up to N more times on the same tree")
	resolver := fs.String("resolver", "", "conflict-resolver command (`sh -c`, same SIGBOUND_BASE/OURS/THEIRS/PATH contract as `sig run`). The only mechanism for landing an inverse over a path a later run also touched")
	reason := fs.String("reason", "", "why, recorded verbatim in the inverse commit message and the unland_start event")
	limit := fs.Int("limit", unlandDefaultLimit, "how many newer runs the blast-radius scan reads to report entanglement (0 = all)")
	dryRun := fs.Bool("dry-run", false, "print the blast radius and the preconditions verdict; build no branch, write no run dir, touch no ref")
	asJSON := fs.Bool("json", false, "emit the outcome as JSON")
	// RUN_ID is positional and documented FIRST, which stdlib flag cannot parse on
	// its own; pull a leading positional off before parsing so both that form and
	// `sig unland -repo P RUN_ID` work. Same handling as `sig ack`/`sig reject`.
	var runID string
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		runID, argv = argv[0], argv[1:]
	}
	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitOK, nil
		}
		return exitOperationalError, err
	}
	if runID == "" && fs.NArg() == 1 {
		runID = fs.Arg(0)
	} else if runID == "" || fs.NArg() > 0 {
		return exitOperationalError, errors.New("exactly one RUN_ID is required")
	}
	if !validRunID(runID) {
		return exitOperationalError, fmt.Errorf("invalid run id %q", runID)
	}
	if strings.TrimSpace(*repo) == "" {
		return exitOperationalError, errors.New("-repo is required")
	}
	if *limit < 0 {
		return exitOperationalError, errors.New("-limit must be >= 0 (0 = walk every newer run)")
	}
	c, err := cell.Open(*repo)
	if err != nil {
		return exitOperationalError, err
	}
	ctx := context.Background()
	if _, serr := os.Stat(filepath.Join(mustRunsDir(ctx, c), runID)); serr != nil {
		return exitOperationalError, fmt.Errorf("no run %s in %s", runID, *repo)
	}
	out, err := unlandRun(ctx, c, unlandParams{
		TargetID: runID, VerifyCmd: *verify, VerifyRetries: *verifyRetries,
		ResolverCmd: *resolver, Reason: *reason, Limit: *limit, DryRun: *dryRun,
		// The invoker's own environment, matching `sig run`'s inherit default; on
		// `sig serve` it is the operator's server flags instead.
		Actor: "cli", Env: ackEnv{Mode: envModeInherit},
	})
	if err != nil {
		return exitOperationalError, err
	}
	if *asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return exitOperationalError, err
		}
	} else {
		writeUnlandSummary(w, out)
	}
	// An unland that landed nothing because it was blocked is a failed operation
	// from the caller's point of view: a script that unlands and moves on must not
	// read a conflicted or red inverse as success. A no-op is not a failure.
	if out.Status == statusUnlandBlocked {
		return exitOperationalError, nil
	}
	return exitOK, nil
}

// writeUnlandSummary is the human rendering: the verdict, then the entangled
// runs, which are the reason a blocked unland is blocked and the warning on one
// that is not.
func writeUnlandSummary(w io.Writer, out unlandOutcome) {
	fmt.Fprintf(w, "unland %s: %s\n", out.Unlands, out.Message)
	for _, e := range out.Entangled {
		fmt.Fprintf(w, "  entangled: run %s (%s) also touched %s\n", e.RunID, short(e.FinalSHA), strings.Join(e.Paths, ", "))
	}
	// The scan stopped at -limit: the entangled list above is a floor, and the
	// long-history repo the flag exists for is exactly where it gets cut short.
	if out.ScanLimited {
		fmt.Fprintln(w, "  note: the -limit was reached; later runs beyond it were not scanned, so this list may be incomplete")
	}
}

// mustRunsDir is cellRunsDir for a caller that has already opened the cell and
// treats an unreadable git dir as "no such run" — the stat that follows fails
// either way, with a message naming the run the user actually asked about.
func mustRunsDir(ctx context.Context, c *cell.Cell) string {
	dir, err := cellRunsDir(ctx, c)
	if err != nil {
		return ""
	}
	return dir
}
