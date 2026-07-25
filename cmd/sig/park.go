// Run parking (issue #109): the ack/reject half of the repo-owned landing
// policy. When a run's landed-candidate group touches an `ack-paths` glob — or
// modifies sigbound.policy itself — that group does NOT auto-land. It is
// integrated and VERIFIED like any other landing and then PARKED: the exact
// verified commit is recorded in the run dir's park.json, the base ref is left
// alone, the branches are kept, and the run sits in `awaiting-ack` until a human
// acks or rejects it.
//
// THE POINT OF THE WHOLE FILE: an ack is an INPUT to the existing landing gate,
// never a second landing path. On an ack whose base has not moved, what lands is
// byte-for-byte the tree that passed verify — the recorded commit, checked to
// still exist, to still carry the recorded tree OID, and to still descend from
// the recorded base, and then handed to the SAME landRef the driver itself uses.
// If the base HAS moved, the stale tree is never landed: the parked branches are
// re-integrated onto the new base and re-verified under the policy AT THAT NEW
// BASE, and only that fresh green tree lands (a red one re-parks with the
// failure attached). `sig ack`/`sig reject` and POST /runs/{id}/ack|reject both
// call ackRun/rejectRun here — one choke point, two front doors.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

// parkFileName is the run dir's parking record, alongside status.json /
// report.json / events.ndjson. Its presence is also what makes `sig gc` protect
// the run's branches unconditionally (see loadParkedBranches).
const parkFileName = "park.json"

// Park reasons — the machine-readable discriminant on a park record and on each
// parked group. policy-modified outranks ack-paths when a group triggers both.
const (
	parkReasonAckPaths       = "ack-paths"
	parkReasonPolicyModified = "policy-modified"
)

// parkActionReject is the only sigbound.policy `ack-timeout-action` v2.0
// implements: an expired park is auto-rejected (branches kept, nothing lands).
const parkActionReject = "reject"

// Run statuses this feature adds to the crash journal's vocabulary (see
// runStatusFile). awaiting-ack is DURABLE — recoverStaleRuns only ever rewrites
// queued/running, so a parked run survives daemon restarts indefinitely, which
// is the entire point: the human it is waiting for may not be back for days.
// rejected is terminal.
const (
	statusAwaitingAck = "awaiting-ack"
	statusRejected    = "rejected"
)

// parkVerifyOutputMax bounds the verify output a re-verify attempt records in
// park.json, so a runaway build log can't grow the record without limit.
const parkVerifyOutputMax = 4000

// parkResolverTimeout is the per-conflict timeout an ack's re-integration gives
// the run's recorded resolver command. The report records the resolver COMMAND
// but not its -resolver-timeout, so this is the same 30s default `sig run` /
// `sig integrate` / `sig replay` all fall back to.
const parkResolverTimeout = 30 * time.Second

// parkGroupJSON is one held integration group: the entangled branches that must
// land or not land together, and the paths that triggered the hold mapped to the
// ack-paths glob each one matched (policyFileName maps to itself — self-
// protection is not glob-driven). See policyHoldback.
type parkGroupJSON struct {
	Branches     []string          `json:"branches"`
	MatchedPaths map[string]string `json:"matchedPaths,omitempty"`
	Reason       string            `json:"reason,omitempty"`
}

// parkAttemptJSON is one verify cycle this park has been through. Attempt 1 is
// always the park's OWN verify — the one that made it parkable, recorded so the
// record is self-contained provenance rather than a verdict you have to go find
// in the run report. Every later attempt is a re-verify an ack ran because the
// base had moved (see ackReverify). VerifyOK is that attempt's verdict: true
// means FinalSHA landed, false means the park stayed open with this failure
// attached for the human to look at.
type parkAttemptJSON struct {
	N        int           `json:"n"`
	At       string        `json:"at"`
	BaseSHA  string        `json:"baseSHA"`
	FinalSHA string        `json:"finalSHA,omitempty"`
	VerifyOK bool          `json:"verifyOk"`
	Output   string        `json:"output,omitempty"`
	Flagged  []flaggedJSON `json:"flagged,omitempty"`
	Error    string        `json:"error,omitempty"`
}

// parkJSON is park.json: everything an ack needs, and nothing it can take on
// trust. VerifiedSHA is the commit an ack lands and VerifiedTree its tree OID —
// recorded separately on purpose, because comparing them at ack time is what
// catches a park record that has been edited to point somewhere else (a mutated
// verifiedSHA can only pass if it carries the identical tree, in which case
// landing it is landing the same bytes). BaseSHA is the base at verify time: an
// ack compares it against the base's CURRENT head to decide between releasing
// the recorded landing and re-verifying from scratch.
//
// The Base/Strategy/Verify/... block is the re-verify input set: what an ack
// re-runs when the base has moved. Verify is deliberately the RAW, pre-policy
// verify command — the ack re-loads sigbound.policy at the NEW base and composes
// the battery again through resolvePolicy, so a policy that tightened while the
// run sat parked applies to the landing it releases.
type parkJSON struct {
	VerifiedSHA  string `json:"verifiedSHA"`
	VerifiedTree string `json:"verifiedTree"`
	BaseSHA      string `json:"baseSHA"`
	// ForkSHA is the commit the parked BRANCHES were created off — the run's own
	// base. It is not BaseSHA: by the time a group parks, the run's clean groups
	// may already have landed and moved the base past the fork point. Every
	// re-integration of these branches uses ForkSHA as its 3-way merge base, so
	// each branch contributes its own changes and never "everything that landed
	// since it forked" (see cell.Integrator.IntegrateOnto). The branches never
	// move, so this never changes.
	ForkSHA   string          `json:"forkSHA"`
	Groups    []parkGroupJSON `json:"groups"`
	Reason    string          `json:"reason"`
	CreatedAt string          `json:"createdAt"`
	// AckTimeout/AckTimeoutAction come from the policy at park time. An absent
	// timeout parks forever, which is the default: an unacked landing is not a
	// problem that time solves.
	AckTimeout       string            `json:"ackTimeout,omitempty"`
	AckTimeoutAction string            `json:"ackTimeoutAction,omitempty"`
	Attempts         []parkAttemptJSON `json:"attempts,omitempty"`

	// Re-verify inputs (see the type comment).
	Base          string `json:"base"`
	Verify        string `json:"verify,omitempty"`
	VerifyRetries int    `json:"verifyRetries,omitempty"`
	Resolver      string `json:"resolver,omitempty"`

	// Outcome, written once the park is resolved.
	LandedSHA    string `json:"landedSHA,omitempty"`
	ResolvedAt   string `json:"resolvedAt,omitempty"`
	RejectReason string `json:"rejectReason,omitempty"`
}

// branches flattens every parked group's branches, in group order.
func (pk *parkJSON) branches() []string {
	var out []string
	for _, g := range pk.Groups {
		out = append(out, g.Branches...)
	}
	return out
}

// matchedPaths merges every group's triggering path -> glob mapping, for the
// inbox entry and the review UI.
func (pk *parkJSON) matchedPaths() map[string]string {
	out := map[string]string{}
	for _, g := range pk.Groups {
		for p, glob := range g.MatchedPaths {
			out[p] = glob
		}
	}
	return out
}

// deadline reports when this park expires, and whether it expires at all. A park
// with no ack-timeout — or one whose action this binary does not implement —
// never expires.
func (pk *parkJSON) deadline() (time.Time, bool) {
	if pk.AckTimeout == "" || pk.AckTimeoutAction != parkActionReject {
		return time.Time{}, false
	}
	d, err := time.ParseDuration(pk.AckTimeout)
	if err != nil || d <= 0 {
		return time.Time{}, false
	}
	created, err := time.Parse(time.RFC3339, pk.CreatedAt)
	if err != nil {
		return time.Time{}, false
	}
	return created.Add(d), true
}

// validate is the fail-closed gate on a park record read back from disk. Every
// field an ack would act on is checked for SHAPE here — real hex object names, a
// safe base branch, at least one real branch to re-integrate, a known reason —
// so a truncated, hand-edited, or garbage park.json is refused outright instead
// of reaching git with whatever it happened to contain. It deliberately does NOT
// check anything against the repo: that is validateParkedLanding's job, run at
// ack time against live objects.
func (pk *parkJSON) validate() error {
	for _, f := range []struct{ name, val string }{
		{"verifiedSHA", pk.VerifiedSHA},
		{"verifiedTree", pk.VerifiedTree},
		{"baseSHA", pk.BaseSHA},
		{"forkSHA", pk.ForkSHA},
	} {
		if !validCommitArg(f.val) {
			return fmt.Errorf("%s: %s %q is not a hex object name", parkFileName, f.name, f.val)
		}
	}
	if pk.Base == "" || !relSafe(pk.Base) || strings.ContainsAny(pk.Base, " \t\n:?*[\\") {
		return fmt.Errorf("%s: base %q is not a usable branch name", parkFileName, pk.Base)
	}
	if pk.Reason != parkReasonAckPaths && pk.Reason != parkReasonPolicyModified {
		return fmt.Errorf("%s: unknown reason %q", parkFileName, pk.Reason)
	}
	if len(pk.Groups) == 0 {
		return fmt.Errorf("%s: no parked groups", parkFileName)
	}
	n := 0
	for _, g := range pk.Groups {
		for _, b := range g.Branches {
			if b == "" || !relSafe(b) || strings.ContainsAny(b, " \t\n:?*[\\") {
				return fmt.Errorf("%s: branch %q is not a usable branch name", parkFileName, b)
			}
			n++
		}
	}
	if n == 0 {
		return fmt.Errorf("%s: parked groups name no branches", parkFileName)
	}
	if _, err := time.Parse(time.RFC3339, pk.CreatedAt); err != nil {
		return fmt.Errorf("%s: createdAt %q is not RFC3339", parkFileName, pk.CreatedAt)
	}
	return nil
}

// writePark records pk atomically (write-then-rename, the same pattern
// writeRunStatus and verifyCacheStore use): a concurrent GET /inbox must never
// observe a torn park.json. Unlike the other durable writers in this codebase
// this one RETURNS its error — park.json is not a log, it is the only record of
// a verified landing that has not landed yet, so losing it must be loud.
func writePark(dir string, pk *parkJSON) error {
	data, err := json.MarshalIndent(pk, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".park.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, parkFileName)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// readPark reads and VALIDATES dir's park.json. Every failure — missing,
// unreadable, unparseable, or structurally wrong — is an error, and every caller
// treats an error as "this park cannot be acted on": ack refuses, the timeout
// sweep leaves it alone, and gc protects its branches by refusing to run. Fail
// closed, in the one direction that cannot lose work.
func readPark(dir string) (*parkJSON, error) {
	data, err := os.ReadFile(filepath.Join(dir, parkFileName))
	if err != nil {
		return nil, err
	}
	pk, err := parsePark(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, parkFileName), err)
	}
	return pk, nil
}

// parsePark decodes and validates park.json's bytes — split from readPark so the
// parser of this UNTRUSTED-by-construction file (it lives on disk, outlives the
// process that wrote it, and decides what a ref advances to) can be fuzzed on
// its own. Unknown fields are tolerated: a record written by a NEWER sigbound is
// forward-compatible data, not corruption. Everything an ack acts on is not.
func parsePark(data []byte) (*parkJSON, error) {
	var pk parkJSON
	if err := json.Unmarshal(data, &pk); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := pk.validate(); err != nil {
		return nil, err
	}
	return &pk, nil
}

// ---- the park pass: verify what was held, then park it ----

// parkHeldGroups is driveRun's park pass. The run's clean groups have already
// landed, so landSHA is the base as it NOW stands while forkSHA is where the
// held branches were actually created — the held groups are folded onto the
// former with the latter as their merge base, and the result is verified on its
// own. A park is always an ALREADY-VERIFIED landing that is also a strict
// descendant of the current base, which together are exactly what let a later
// ack just advance the ref.
//
// It returns nil — no park — when that tree cannot honestly be offered as a
// landing: an integrate failure, a real merge conflict among the held branches,
// or a red verify. In every one of those cases the branches simply stay flagged
// with their policy reason, which is where they already are; nothing is lost and
// nothing unverified is ever recorded as ackable.
func parkHeldGroups(ctx context.Context, c *cell.Cell, p runParams, pol policy, forkSHA, landSHA, rawVerify string, groups []parkGroupJSON, emit *eventEmitter) *parkJSON {
	branches := make([]string, 0, len(groups))
	for _, g := range groups {
		branches = append(branches, g.Branches...)
	}
	if len(branches) == 0 {
		return nil
	}
	finalSHA, v, flagged, err := integrateVerifyPark(ctx, c, p, forkSHA, landSHA, branches)
	if err != nil || len(flagged) > 0 || (v.Ran && !v.OK) {
		emit.emit("park_failed", map[string]any{
			"branches": branches,
			"flagged":  flagged,
			"error":    errText(err),
			"output":   tail(v.Output, parkVerifyOutputMax),
		})
		return nil
	}
	tree, err := c.Git().TreeOID(ctx, finalSHA)
	if err != nil {
		emit.emit("park_failed", map[string]any{"branches": branches, "error": errText(err)})
		return nil
	}
	reason := parkReasonAckPaths
	for _, g := range groups {
		if g.Reason == parkReasonPolicyModified {
			reason = parkReasonPolicyModified
			break
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	pk := &parkJSON{
		VerifiedSHA:  finalSHA,
		VerifiedTree: tree,
		BaseSHA:      landSHA,
		ForkSHA:      forkSHA,
		Groups:       groups,
		Reason:       reason,
		CreatedAt:    now,
		Base:         p.Base,
		// The RAW verify: an ack re-composes the battery against the policy at
		// whatever base it finds, so recording the already-composed one here would
		// run the policy's members twice on a re-verify.
		Verify:        rawVerify,
		VerifyRetries: p.VerifyRetries,
		Resolver:      p.ResolverCmd,
		Attempts: []parkAttemptJSON{{
			N:        1,
			At:       now,
			BaseSHA:  landSHA,
			FinalSHA: finalSHA,
			VerifyOK: true,
			Output:   tail(v.Output, parkVerifyOutputMax),
		}},
	}
	if pol.ackTimeout > 0 {
		pk.AckTimeout = pol.ackTimeout.String()
		pk.AckTimeoutAction = pol.ackTimeoutAction
	}
	emit.emit("parked", map[string]any{
		"reason":       reason,
		"verifiedSHA":  finalSHA,
		"baseSHA":      landSHA,
		"forkSHA":      forkSHA,
		"branches":     branches,
		"matchedPaths": pk.matchedPaths(),
	})
	return pk
}

// integrateVerifyPark folds branches (created off forkSHA) onto onto — the base
// as it stands right now — WITHOUT landing, and verifies the resulting tree. It
// is the shared body of both the park pass above and an ack's re-verify after
// the base moved (ackReverify), so a tree that gets parked and a tree that gets
// re-verified are gated by identical code.
//
// The fold goes through cell.IntegrateOnto, the one seam that merges a branch
// against its OWN fork point while producing a descendant of the current base —
// integrating with onto as the merge base instead would read everything that
// landed since the fork as this branch's changes and revert it.
//
// -verify-impact is deliberately dropped here: it runs a scoped command INSTEAD
// of verify, and a landing a human is being asked to authorize gets the full
// command, not a narrower one. -verify-bisect is likewise not applied — a parked
// group is entangled by write-set overlap and lands whole or not at all, so
// there is no subset to salvage. A caller with no verify command at all gets
// verifyJSON{Ran:false}, which every caller reads as "nothing said no".
func integrateVerifyPark(ctx context.Context, c *cell.Cell, p runParams, forkSHA, onto string, branches []string) (finalSHA string, v verifyJSON, flagged []flaggedJSON, err error) {
	g := c.Git()
	// Write-sets are computed against the FORK point, the only base against
	// which a branch's changes are its own.
	ws, err := g.DiffNameOnlyBatch(ctx, forkSHA, branches)
	if err != nil {
		return "", verifyJSON{}, nil, fmt.Errorf("write-sets: %w", err)
	}
	changes := make([]cell.BranchChange, 0, len(branches))
	for _, b := range branches {
		changes = append(changes, cell.BranchChange{Branch: b, WriteSet: cell.NewWriteSet(ws[b]...)})
	}
	var opts []func(*cell.Integrator)
	if cmd := strings.TrimSpace(p.ResolverCmd); cmd != "" {
		var resolverEnv []string
		if p.EnvMode == envModeScoped {
			resolverEnv = slotEnv(envModeScoped, p.EnvResolver, nil)
		}
		r := &cell.CommandResolver{Args: []string{"sh", "-c", cmd}, Timeout: p.ResolverTimeout, Env: resolverEnv}
		opts = append(opts, func(in *cell.Integrator) { in.WithResolver(r) })
	}
	res, err := c.IntegrateOnto(ctx, forkSHA, onto, changes, opts...)
	if err != nil {
		return "", verifyJSON{}, nil, fmt.Errorf("integrate: %w", err)
	}
	for _, f := range res.Flagged {
		flagged = append(flagged, flaggedJSON{Branch: f.Branch, Paths: f.Conflicts})
	}
	if strings.TrimSpace(p.VerifyCmd) == "" {
		return res.FinalSHA, verifyJSON{}, flagged, nil
	}
	dir, derr := os.MkdirTemp("", "sig-park-*")
	if derr != nil {
		return "", verifyJSON{}, flagged, fmt.Errorf("verify worktree: %w", derr)
	}
	defer os.RemoveAll(dir)
	wtPath := filepath.Join(dir, "wt")
	if werr := g.WorktreeAddDetached(ctx, wtPath, res.FinalSHA); werr != nil {
		return "", verifyJSON{}, flagged, fmt.Errorf("verify checkout %s: %w", short(res.FinalSHA), werr)
	}
	defer func() { _ = g.WorktreeRemove(ctx, wtPath) }()
	pv := p
	pv.VerifyImpactCmd = ""
	return res.FinalSHA, runVerifyRetry(ctx, g, wtPath, pv, nil, pv.VerifyRetries, "", 0), flagged, nil
}

// ---- spot-audit sampling ----

// auditSelected reports whether a run id falls in the policy's audit-sample
// percentage: sha256(runId) mod 100 < pct. Deterministic and replayable by
// construction — the same id selects the same way in every process, on every
// machine, forever — which is the whole reason there is no RNG here. A run with
// no id (i.e. `sig run`, which has none) is never selected.
func auditSelected(runID string, pct int) bool {
	if pct <= 0 || runID == "" {
		return false
	}
	if pct >= 100 {
		return true
	}
	sum := sha256.Sum256([]byte(runID))
	return binary.BigEndian.Uint64(sum[:8])%100 < uint64(pct)
}

// ---- ack / reject: the one choke point ----

// errNotAwaitingAck is the sentinel both front doors map to a 409 / a non-zero
// exit: ack and reject are only meaningful on a run that is actually parked.
// A sentinel rather than a string match, so the HTTP status and the error text
// stay independent (issue #93).
var errNotAwaitingAck = errors.New("run is not awaiting ack")

// ackEnv is the environment policy an ack's re-verify runs the recorded
// verify/resolver commands under. It is NOT recorded in the park (a run's
// environment can carry secrets its command text never mentions — see
// runReport.EnvMode), so it comes from whoever is acking: the operator's server
// flags on POST /runs/{id}/ack, the invoker's own environment on `sig ack`.
type ackEnv struct {
	Mode     string
	Verify   []string
	Resolver []string
}

// ackOutcome is what an ack or reject did, returned to both front doors so the
// HTTP body and the CLI's output describe the same thing.
type ackOutcome struct {
	RunID      string `json:"runId"`
	Status     string `json:"status"`
	LandedSHA  string `json:"landedSHA,omitempty"`
	Reverified bool   `json:"reverified,omitempty"`
	Attempts   int    `json:"attempts,omitempty"`
	Message    string `json:"message"`
}

// ackRun releases a parked landing. It is THE ack code path — `sig ack` and
// POST /runs/{id}/ack are two front doors onto this one function, which is what
// makes "an ack lands exactly the verified tree" a property of one place rather
// than a convention two call sites have to keep agreeing on.
//
// Base UNCHANGED since verify: the recorded commit is re-checked against the
// live object store (it must still exist, still carry the recorded tree OID, and
// still descend from the recorded base — see validateParkedLanding) and then
// handed to landRef, the same call driveRun lands through. Nothing is
// recomputed, so what lands is byte-for-byte the tree that passed verify.
//
// Base MOVED: the stale tree is NOT landed. The parked branches are
// re-integrated onto the base's current head and re-verified under the policy
// loaded AT THAT HEAD — a policy that tightened while the run sat parked gates
// the landing it releases. Green lands the NEW commit and records it as the
// park's verified landing; red leaves the run parked with the failed attempt
// attached, which is what the inbox then shows.
func ackRun(ctx context.Context, c *cell.Cell, dir, actor string, env ackEnv) (ackOutcome, error) {
	runID := filepath.Base(dir)
	// An expired park is already rejected by the time an ack arrives — enforce
	// the timeout here too, not just on the read paths, so the answer never
	// depends on whether anyone happened to look at the inbox first.
	enforceParkTimeout(dir)
	if st, _ := diskRunStatus(dir); st != statusAwaitingAck {
		return ackOutcome{}, fmt.Errorf("%w (status %s)", errNotAwaitingAck, st)
	}
	pk, err := readPark(dir)
	if err != nil {
		return ackOutcome{}, fmt.Errorf("read parking record: %w", err)
	}
	g := c.Git()
	current, err := g.RevParse(ctx, pk.Base)
	if err != nil {
		return ackOutcome{}, fmt.Errorf("resolve base %q: %w", pk.Base, err)
	}
	if err := validateParkedLanding(ctx, g, pk); err != nil {
		return ackOutcome{}, err
	}
	if current == pk.BaseSHA {
		if err := landRef(ctx, g, pk.Base, pk.VerifiedSHA); err != nil {
			return ackOutcome{}, fmt.Errorf("land %s: %w", short(pk.VerifiedSHA), err)
		}
		pk.LandedSHA = pk.VerifiedSHA
		pk.ResolvedAt = time.Now().UTC().Format(time.RFC3339)
		if err := writePark(dir, pk); err != nil {
			// The ref has moved; the record of why must not be silently lost.
			fmt.Fprintf(os.Stderr, "sig: ack %s landed %s but could not update %s: %v\n", runID, short(pk.LandedSHA), parkFileName, err)
		}
		writeRunStatus(dir, "done", "")
		appendRunEvent(dir, "ack", map[string]any{"actor": actor, "sha": pk.LandedSHA, "reverified": false})
		return ackOutcome{
			RunID: runID, Status: "done", LandedSHA: pk.LandedSHA,
			Message: fmt.Sprintf("landed the verified commit %s on %s", short(pk.LandedSHA), pk.Base),
		}, nil
	}
	return ackReverify(ctx, c, dir, actor, env, pk, current)
}

// ackReverify is ackRun's base-moved half: the recorded tree was verified
// against a base that is no longer there, so it is discarded as a landing
// candidate and the parked branches are integrated + verified afresh against
// what IS there. The attempt is recorded either way, so a park that has been
// acked into three successive red re-verifies says so.
func ackReverify(ctx context.Context, c *cell.Cell, dir, actor string, env ackEnv, pk *parkJSON, current string) (ackOutcome, error) {
	runID := filepath.Base(dir)
	branches := pk.branches()
	p := runParams{
		Repo:            c.Repo(),
		Base:            pk.Base,
		VerifyCmd:       pk.Verify,
		VerifyRetries:   pk.VerifyRetries,
		ResolverCmd:     pk.Resolver,
		ResolverTimeout: parkResolverTimeout,
		EnvMode:         env.Mode,
		EnvVerify:       env.Verify,
		EnvResolver:     env.Resolver,
	}
	// The FULL policy gate, at the base this landing is about to go onto — not
	// the one that gated the original run. resolvePolicy is the same choke point
	// driveRun reaches, so an ack cannot land under a laxer bar than a fresh run
	// would face. Nothing here is "explicit", so a tightened policy silently
	// raises the bar rather than erroring.
	pol, perr := loadPolicy(ctx, c.Git(), current)
	if perr == nil {
		perr = resolvePolicy(pol, &p, len(branches))
	}
	att := parkAttemptJSON{
		N:       len(pk.Attempts) + 1,
		At:      time.Now().UTC().Format(time.RFC3339),
		BaseSHA: current,
	}
	var finalSHA string
	var v verifyJSON
	var flagged []flaggedJSON
	if perr != nil {
		att.Error = perr.Error()
	} else {
		var err error
		finalSHA, v, flagged, err = integrateVerifyPark(ctx, c, p, pk.ForkSHA, current, branches)
		att.FinalSHA, att.Flagged, att.Output = finalSHA, flagged, tail(v.Output, parkVerifyOutputMax)
		att.Error = errText(err)
		att.VerifyOK = err == nil && len(flagged) == 0 && (!v.Ran || v.OK)
	}
	pk.Attempts = append(pk.Attempts, att)
	appendRunEvent(dir, "repark", map[string]any{
		"attempt": att.N, "verdict": verdictOf(att.VerifyOK), "baseSHA": current, "finalSHA": att.FinalSHA,
	})
	if !att.VerifyOK {
		if err := writePark(dir, pk); err != nil {
			return ackOutcome{}, fmt.Errorf("record re-verify attempt: %w", err)
		}
		// Re-assert awaiting-ack: the park stays open, now with a failure the
		// inbox can show, and the run is emphatically not done.
		writeRunStatus(dir, statusAwaitingAck, fmt.Sprintf("re-verify attempt %d failed after the base moved to %s", att.N, short(current)))
		return ackOutcome{
			RunID: runID, Status: statusAwaitingAck, Reverified: true, Attempts: att.N,
			Message: fmt.Sprintf("base moved to %s; re-verify attempt %d failed — still parked", short(current), att.N),
		}, nil
	}
	tree, terr := c.Git().TreeOID(ctx, finalSHA)
	if terr != nil {
		return ackOutcome{}, fmt.Errorf("tree of re-verified %s: %w", short(finalSHA), terr)
	}
	if err := landRef(ctx, c.Git(), pk.Base, finalSHA); err != nil {
		return ackOutcome{}, fmt.Errorf("land %s: %w", short(finalSHA), err)
	}
	pk.VerifiedSHA, pk.VerifiedTree, pk.BaseSHA = finalSHA, tree, current
	pk.LandedSHA = finalSHA
	pk.ResolvedAt = time.Now().UTC().Format(time.RFC3339)
	if err := writePark(dir, pk); err != nil {
		fmt.Fprintf(os.Stderr, "sig: ack %s landed %s but could not update %s: %v\n", runID, short(finalSHA), parkFileName, err)
	}
	writeRunStatus(dir, "done", "")
	appendRunEvent(dir, "ack", map[string]any{"actor": actor, "sha": finalSHA, "reverified": true, "attempt": att.N})
	return ackOutcome{
		RunID: runID, Status: "done", LandedSHA: finalSHA, Reverified: true, Attempts: att.N,
		Message: fmt.Sprintf("base had moved to %s; re-verified green and landed %s", short(current), short(finalSHA)),
	}, nil
}

// validateParkedLanding re-checks a recorded landing against the LIVE object
// store, immediately before it is allowed to move a ref. Three independent
// checks, each of which alone would let a mutated record through:
//
//   - the commit still resolves (it was not garbage collected, and the record
//     names a real object rather than plausible-looking hex);
//   - its tree OID still equals the recorded one, so a verifiedSHA edited to
//     point at some OTHER commit is refused — the only way past this is a commit
//     with a byte-identical tree, which is the same landing;
//   - the recorded base is still an ancestor of it, so the landing is genuinely
//     a descendant of what it was verified against rather than an unrelated
//     history.
//
// Any failure refuses the ack outright. Landing something other than the exact
// verified tree is the one outcome this whole feature exists to prevent.
func validateParkedLanding(ctx context.Context, g *gitx.Git, pk *parkJSON) error {
	if _, err := g.RevParse(ctx, pk.VerifiedSHA); err != nil {
		return fmt.Errorf("refusing to ack: recorded verifiedSHA %s no longer resolves to a commit: %w", short(pk.VerifiedSHA), err)
	}
	tree, err := g.TreeOID(ctx, pk.VerifiedSHA)
	if err != nil {
		return fmt.Errorf("refusing to ack: tree of recorded verifiedSHA %s: %w", short(pk.VerifiedSHA), err)
	}
	if tree != pk.VerifiedTree {
		return fmt.Errorf("refusing to ack: recorded verifiedSHA %s has tree %s but the parking record says %s — that is not the tree verify passed",
			short(pk.VerifiedSHA), short(tree), short(pk.VerifiedTree))
	}
	anc, err := g.IsAncestor(ctx, pk.BaseSHA, pk.VerifiedSHA)
	if err != nil {
		return fmt.Errorf("refusing to ack: ancestry of %s from %s: %w", short(pk.VerifiedSHA), short(pk.BaseSHA), err)
	}
	if !anc {
		return fmt.Errorf("refusing to ack: recorded verifiedSHA %s does not descend from the recorded baseSHA %s", short(pk.VerifiedSHA), short(pk.BaseSHA))
	}
	return nil
}

// rejectRun marks a parked run rejected: terminal, nothing lands, and the
// branches are KEPT exactly as they are — a rejection is a decision not to land,
// never a decision to destroy the work. reason is optional and recorded verbatim.
func rejectRun(dir, actor, reason string) (ackOutcome, error) {
	runID := filepath.Base(dir)
	enforceParkTimeout(dir)
	if st, _ := diskRunStatus(dir); st != statusAwaitingAck {
		return ackOutcome{}, fmt.Errorf("%w (status %s)", errNotAwaitingAck, st)
	}
	pk, err := readPark(dir)
	if err != nil {
		return ackOutcome{}, fmt.Errorf("read parking record: %w", err)
	}
	pk.RejectReason = strings.TrimSpace(reason)
	pk.ResolvedAt = time.Now().UTC().Format(time.RFC3339)
	if err := writePark(dir, pk); err != nil {
		return ackOutcome{}, fmt.Errorf("record rejection: %w", err)
	}
	writeRunStatus(dir, statusRejected, pk.RejectReason)
	appendRunEvent(dir, "reject", map[string]any{"actor": actor, "reason": pk.RejectReason})
	msg := "rejected; branches kept, nothing landed"
	if pk.RejectReason != "" {
		msg += ": " + pk.RejectReason
	}
	return ackOutcome{RunID: runID, Status: statusRejected, Message: msg}, nil
}

// enforceParkTimeout is the LAZY ack-timeout sweep: an expired park is
// auto-rejected the next time anyone looks at it — serve startup, GET /inbox,
// GET /runs/{id}, or an ack/reject itself. There is deliberately no timer
// goroutine: nothing depends on the transition happening at the instant it comes
// due, only on it being true by the time anyone can observe otherwise.
//
// A park with no timeout, or one whose record no longer reads back cleanly, is
// left alone — an unreadable park is not evidence that it expired.
func enforceParkTimeout(dir string) bool {
	if st, _ := diskRunStatus(dir); st != statusAwaitingAck {
		return false
	}
	pk, err := readPark(dir)
	if err != nil {
		return false
	}
	deadline, ok := pk.deadline()
	if !ok || time.Now().Before(deadline) {
		return false
	}
	pk.RejectReason = fmt.Sprintf("ack-timeout %s expired without an ack", pk.AckTimeout)
	pk.ResolvedAt = time.Now().UTC().Format(time.RFC3339)
	if err := writePark(dir, pk); err != nil {
		return false
	}
	writeRunStatus(dir, statusRejected, pk.RejectReason)
	appendRunEvent(dir, "reject", map[string]any{"actor": "timeout", "reason": pk.RejectReason})
	return true
}

// expireParks runs enforceParkTimeout over every run dir under runsDir — the
// startup half of the lazy sweep, run alongside recoverStaleRuns so a daemon
// that was down past a park's deadline reports it correctly from its first
// request onward.
func expireParks(runsDir string) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return
	}
	for _, de := range entries {
		if de.IsDir() {
			enforceParkTimeout(filepath.Join(runsDir, de.Name()))
		}
	}
}

// appendRunEvent appends one NDJSON line to a run's events.ndjson, for the
// events that happen AFTER driveRun has returned (ack/reject/repark). It reuses
// eventEmitter so those lines carry the identical {event, ts, ...} shape as
// every in-run event; best-effort, like every other event write.
func appendRunEvent(dir, name string, fields map[string]any) {
	f, err := os.OpenFile(filepath.Join(dir, "events.ndjson"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	(&eventEmitter{enc: json.NewEncoder(f)}).emit(name, fields)
}

// errText renders err for a JSON field, "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// verdictOf renders a re-verify attempt's boolean verdict as the green/red
// wording the repark event and the inbox use.
func verdictOf(ok bool) string {
	if ok {
		return "green"
	}
	return "red"
}

// ---- CLI: sig ack / sig reject ----

// runAck and runReject are the CLI front doors. They resolve the run id to its
// durable run directory under the target repo and call the SAME ackRun/rejectRun
// the HTTP handlers do — the CLI has no parallel implementation to drift from.
func runAck(w io.Writer, argv []string) (int, error) { return runAckReject(w, argv, true) }

func runReject(w io.Writer, argv []string) (int, error) { return runAckReject(w, argv, false) }

func runAckReject(w io.Writer, argv []string, ack bool) (int, error) {
	name := "reject"
	if ack {
		name = "ack"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: sig %s RUN_ID -repo PATH [-reason TEXT] [-json]\n", name)
		if ack {
			fmt.Fprintln(fs.Output(), "release a parked landing: lands the exact commit that passed verify when the base")
			fmt.Fprintln(fs.Output(), "has not moved, else re-integrates + re-verifies the parked branches onto the")
			fmt.Fprintln(fs.Output(), "current base and lands only a green result (a red one stays parked)")
		} else {
			fmt.Fprintln(fs.Output(), "reject a parked landing: terminal, nothing lands, the branches are kept")
		}
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "path to the target git repository (the cell whose run history holds RUN_ID)")
	reason := fs.String("reason", "", "optional reason, recorded in the run's parking record (reject only)")
	asJSON := fs.Bool("json", false, "emit the outcome as JSON")
	// RUN_ID is positional and documented FIRST (`sig ack RUN_ID -repo P`), which
	// stdlib flag cannot parse on its own — it stops at the first non-flag
	// argument and hands the rest back unparsed. Pull a leading positional off
	// before parsing so both that form and `sig ack -repo P RUN_ID` work.
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
	if ack && strings.TrimSpace(*reason) != "" {
		return exitOperationalError, errors.New("-reason applies to sig reject, not sig ack")
	}
	c, err := cell.Open(*repo)
	if err != nil {
		return exitOperationalError, err
	}
	ctx := context.Background()
	common, err := c.Git().GitCommonDir(ctx)
	if err != nil {
		return exitOperationalError, err
	}
	dir := filepath.Join(common, "sigbound", "runs", runID)
	if fi, serr := os.Stat(dir); serr != nil || !fi.IsDir() {
		return exitOperationalError, fmt.Errorf("no run %s in %s", runID, *repo)
	}
	var out ackOutcome
	if ack {
		// Environment policy for the re-verify is the invoker's own, matching
		// `sig run`'s inherit default; on `sig serve` it is the operator's server
		// flags instead. Never the park's — a run's environment is not recorded.
		out, err = ackRun(ctx, c, dir, "cli", ackEnv{Mode: envModeInherit})
	} else {
		out, err = rejectRun(dir, "cli", *reason)
	}
	if err != nil {
		return exitOperationalError, err
	}
	if *asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return exitOperationalError, err
		}
		return exitOK, nil
	}
	fmt.Fprintf(w, "%s %s: %s\n", name, out.RunID, out.Message)
	return exitOK, nil
}

// ---- gc protection ----

// loadParkedBranches returns every branch named by a park.json under the repo's
// run history. Unlike loadProtectedBranches' manifest protection, this set is
// UNCONDITIONAL: a parked branch is the only copy of a verified landing that has
// not landed yet, so no age cutoff and no -force may sweep it. A park.json that
// exists but cannot be read is a hard error that aborts gc entirely — the same
// fail-closed posture a corrupt manifest already gets, and the only safe answer
// when the question is "which branches must I not delete".
func loadParkedBranches(ctx context.Context, g *gitx.Git) (map[string]bool, error) {
	common, err := g.GitCommonDir(ctx)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(common, "sigbound", "runs", "*", parkFileName))
	if err != nil {
		return nil, fmt.Errorf("glob parking records: %w", err)
	}
	sort.Strings(matches)
	parked := map[string]bool{}
	for _, m := range matches {
		pk, rerr := readPark(filepath.Dir(m))
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", m, rerr)
		}
		if pk.ResolvedAt != "" {
			continue // acked or rejected: no longer parked, ordinary sweep rules apply
		}
		for _, b := range pk.branches() {
			parked[b] = true
		}
	}
	return parked, nil
}
