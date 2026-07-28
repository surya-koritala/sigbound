// sigbound.policy — the repo-owned landing policy (issue #108). A flat
// KEY=VALUE file checked into the target repo declaring what a landing
// REQUIRES: the repo, not the invoker, owns its landing bar. It is loaded from
// the BASE SHA'S TREE at run start (versioned like any other file, so the
// policy that gates a landing is the one committed at the base being landed
// on), parsed with the SAME lexer as sig.conf (parseConfigFile — one flat-file
// dialect, not two), and resolved against the run's flags/request by ONE shared
// function (resolvePolicy) reached by both `sig run` and `sig serve` through
// their shared driveRun choke point. Flags may only TIGHTEN policy, never
// loosen it. See docs/USAGE.md's "Landing policy" section.
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/v2/cell"
	"github.com/surya-koritala/sigbound/v2/internal/gitx"
	sbpolicy "github.com/surya-koritala/sigbound/v2/pkg/policy"
)

// The landing-policy TYPE, PARSER and EVALUATORS live in pkg/policy, which has
// no I/O in it and is therefore usable from outside this binary — a hosted gate
// enforcing the same bar on a push it just received has the bytes already and
// no worktree at all. Aliased rather than wrapped so this package keeps reading
// exactly as it did, and so there is provably ONE definition of the rules: the
// engine consumes the library it publishes, on every run.
//
// What stays here is what needs things pkg/policy deliberately does not have —
// git (loading the policy at a base SHA), a run's flags (resolving policy
// against them), and the integrator (grouping held branches).
type (
	policy = sbpolicy.Policy
	// Hold is one holdback or refusal decision; Kind is empty when nothing fired.
	policyHold = sbpolicy.Hold
)

const (
	policyFileName = sbpolicy.FileName

	parkReasonPolicyModifiedPkg = sbpolicy.ReasonPolicyModified

	repairRefusedPolicyFile      = sbpolicy.RepairRefusedPolicyFile
	repairRefusedDeny            = sbpolicy.RepairRefusedDeny
	repairRefusedAckPaths        = sbpolicy.RepairRefusedAckPaths
	repairRefusedUnknownWriteSet = sbpolicy.RepairRefusedUnknownWriteSet
)

// parsePolicy, branchHoldReason, repairRefusalReason and globMatch are thin
// re-exports so every call site in this package is unchanged and the shared
// library is exercised by the whole existing suite rather than by tests written
// only for it.
func parsePolicy(data []byte) (policy, error) { return sbpolicy.Parse(data) }

func branchHoldReason(paths []string, pol policy, unland bool) (kind, reason string, matched map[string]string) {
	h := sbpolicy.HoldReason(paths, pol, unland)
	return h.Kind, h.Reason, h.Matched
}

func repairRefusalReason(paths []string, pol policy) (kind, reason string, refused []string) {
	h := sbpolicy.RepairRefusal(paths, pol)
	return h.Kind, h.Reason, h.Paths
}

func globMatch(pattern, name string) bool { return sbpolicy.GlobMatch(pattern, name) }

// policyExplicit names the policy-governed dimensions the invoker chose
// DELIBERATELY (a command-line flag / sig.conf value for `sig run`; a non-empty
// request field for `sig serve`). resolvePolicy needs it to tell a deliberate
// weaker choice — a loud error — from an unset default silently tightened to
// the policy floor. Only the tighten-or-error keys need it; the verify battery
// (always appended) and the quota clamps (min(), documented) never conflict.
type policyExplicit struct {
	Lanes    bool
	Semantic bool
	Assert   bool
}

// loadPolicy reads policyFileName from rev's tree (git show rev:sigbound.policy,
// via BlobAt) and parses it. An ABSENT file is not an error — it resolves to a
// zero policy (present=false), the no-migration default. A present-but-invalid
// file (unknown key, malformed value) is a hard error naming file+line+key, so
// a typo can never silently weaken the bar (fail closed).
func loadPolicy(ctx context.Context, g *gitx.Git, rev string) (policy, error) {
	content, present, err := g.BlobAt(ctx, rev, policyFileName)
	if err != nil {
		return policy{}, fmt.Errorf("read %s at %s: %w", policyFileName, short(rev), err)
	}
	if !present {
		return policy{}, nil // no policy file at base: today's behavior, unchanged
	}
	pol, err := parsePolicy([]byte(content))
	if err != nil {
		return policy{}, fmt.Errorf("%s: %w", policyFileName, err)
	}
	return pol, nil
}

func resolvePolicy(pol policy, p *runParams, taskCount int) error {
	if !pol.Present {
		return nil
	}
	// verify: policy battery, then the flag/request verify appended.
	battery := append([]string(nil), pol.Verify...)
	if fv := strings.TrimSpace(p.VerifyCmd); fv != "" {
		battery = append(battery, p.VerifyCmd)
	}
	if len(battery) > 0 {
		p.VerifyCmd = joinVerifyBattery(battery)
	}
	// A policy-imposed verify battery must run in FULL: -verify-impact runs a
	// scoped command INSTEAD of the verify command (see runVerify), which would
	// let an invoker's impact optimization bypass the policy's battery — a
	// weakening of the gate. When the policy contributes any verify member, drop
	// impact scoping so the whole battery always runs.
	if len(pol.Verify) > 0 {
		p.VerifyImpactCmd = ""
	}
	// lanes: strict floor.
	if pol.Lanes == laneStrict && laneRank(p.LaneMode) < laneRank(laneStrict) {
		if p.PolicyExplicit.Lanes {
			return fmt.Errorf("policy %s: lanes=strict; flag -lanes=%s — flags may only tighten policy", policyFileName, p.LaneMode)
		}
		p.LaneMode = laneStrict
	}
	// semantic: go floor.
	if pol.Semantic == semanticGo && p.Semantic != semanticGo {
		if p.PolicyExplicit.Semantic {
			return fmt.Errorf("policy %s: semantic=go; flag -semantic=%s — flags may only tighten policy", policyFileName, effectiveSemantic(p.Semantic))
		}
		p.Semantic = semanticGo
	}
	// assert: true floor.
	if pol.AssertSet && pol.Assert && !p.Assert {
		if p.PolicyExplicit.Assert {
			return fmt.Errorf("policy %s: assert=true; flag -assert=false — flags may only tighten policy", policyFileName)
		}
		p.Assert = true
	}
	// quotas: min-clamp, no error (min is the established, documented semantics).
	p.ParallelAgents = clampCeiling(p.ParallelAgents, pol.Parallel)
	p.Budget = clampCeiling(p.Budget, pol.Budget)
	if pol.MaxAgents > 0 && taskCount > pol.MaxAgents {
		return fmt.Errorf("policy %s: max-agents=%d, but this run has %d tasks", policyFileName, pol.MaxAgents, taskCount)
	}
	return nil
}

// validateVerifyPreconditions rejects -verify-bisect / -verify-impact when no
// verify command exists to bisect over or fall back to. It runs in driveRun
// immediately AFTER resolvePolicy, against the EFFECTIVE p.VerifyCmd, so a
// verify battery supplied solely by the repo's sigbound.policy satisfies the
// precondition — a policy-bearing repo can use bisect without passing a
// redundant -verify. Checking the flag/request at parse time (where this used to
// live) could not see the policy, since the policy is only readable from the
// pinned base SHA inside driveRun. Both `sig run` and serve reach this one site.
//
// -verify-impact needs no special case for the policy-battery run: resolvePolicy
// has already CLEARED VerifyImpactCmd whenever the policy contributed a battery
// (impact runs INSTEAD of verify, so it must never bypass a policy battery), and
// a cleared impact command trivially satisfies this check — the flag is accepted
// and then dropped by that documented behavior, never rejected misleadingly. The
// check still fires for the genuine case: an impact command with no verify
// anywhere.
func validateVerifyPreconditions(p runParams) error {
	if strings.TrimSpace(p.VerifyCmd) != "" {
		return nil
	}
	if strings.TrimSpace(p.VerifyImpactCmd) != "" {
		return errors.New("-verify-impact requires -verify (or -verify-preset, or a verify line in sigbound.policy): it composes WITH verify, which stays required as the fallback")
	}
	if p.VerifyBisect {
		return errors.New("-verify-bisect requires -verify (or -verify-preset, or a verify line in sigbound.policy): it bisects over verify's verdict on the combined tree")
	}
	return nil
}

// policyReport builds the report/manifest's policy block, or nil when no policy
// file exists at the base (so a run against a repo with no policy reports
// byte-identical to before this feature). Verify is the policy's OWN declared
// battery; the effective composed command (policy plus any appended flag verify)
// is the report's top-level verifyCmd. AckTimeout renders as its duration
// string only when set. See policyJSON.
func policyReport(pol policy) *policyJSON {
	if !pol.Present {
		return nil
	}
	out := &policyJSON{Hash: pol.Hash, Verify: pol.Verify, AckPaths: pol.AckPaths, UnlandPaths: pol.UnlandPaths}
	if pol.AuditSample >= 0 {
		n := pol.AuditSample
		out.AuditSample = &n
	}
	if pol.AckTimeout > 0 {
		out.AckTimeout = pol.AckTimeout.String()
		out.AckTimeoutAction = pol.AckTimeoutAction
	}
	return out
}

// joinVerifyBattery composes the battery into one command string that the run's
// existing `sh -c <cmd>` verify path executes. A member is UNTRUSTED shell
// (an invoker's -verify, or a repo's own policy line), so it must NOT be
// textually embedded into a compound command: a member like `true ) ; ( true`
// would break out of any surrounding wrapping and append a top-level statement
// whose exit 0 masks a prior member's failure — landing red, the one thing the
// verify gate must never allow. Instead each member runs in its OWN nested
// `sh -c '<member>'`, single-quote-escaped so every metacharacter stays confined
// to that nested shell, and the nested shells are ANDed: any member's non-zero
// exit short-circuits the chain and fails the gate. A single member is passed
// through verbatim — no composition, so byte-identical to a plain -verify.
//
// ponytail: per-member failure REPORTING (naming which member failed) needs the
// members run as separate exec invocations in Go, threaded through the whole
// verify/cache/bisect/impact path — a large diff for a report-only nicety. Kept
// as a single string here so those paths are unchanged; add per-member reporting
// when the review-UI need is real.
func joinVerifyBattery(members []string) string {
	if len(members) == 1 {
		return members[0]
	}
	parts := make([]string, len(members))
	for i, m := range members {
		parts[i] = "sh -c " + shellQuote(m)
	}
	return strings.Join(parts, " && ")
}

// shellQuote wraps s in single quotes for POSIX sh, escaping any embedded single
// quote as the standard '\” sequence (close-quote, escaped quote, reopen). The
// result passes s to a nested `sh -c` verbatim with every other character
// literal, so a battery member's metacharacters cannot escape into the composed
// command (see joinVerifyBattery).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// laneRank orders the lane modes by strictness so a policy floor can be
// compared against a flag: off < warn < strict. An empty/unknown value ranks as
// the least strict (0), matching driveRun's own "" => warn default posture
// erring safe (policy can only raise it).
func laneRank(m string) int {
	switch m {
	case laneStrict:
		return 2
	case laneWarn:
		return 1
	default:
		return 0
	}
}

// effectiveSemantic renders the in-process "" default as its documented name
// (off) for a resolvePolicy error message, so the report reads semantic=off
// rather than an empty string.
func effectiveSemantic(s string) string {
	if s == "" {
		return semanticOff
	}
	return s
}

// clampCeiling returns min(val, ceiling) with the serve quota convention: a
// non-positive ceiling means "unlimited" (no clamp), and a non-positive val
// (today's default) is treated as unlimited and so becomes the ceiling. Reused
// for both parallel-agents (int) and budget (Duration).
func clampCeiling[T int | time.Duration](val, ceiling T) T {
	if ceiling > 0 && (val <= 0 || ceiling < val) {
		return ceiling
	}
	return val
}

// policyHoldback splits ok agent branches into those cleared to integrate and
// those HELD by policy — never auto-landed. Held branches are recorded twice, for
// two different consumers: as flaggedJSON entries (branches kept, ref not
// advanced, reason recorded — the #108 report/UI surface, unchanged) and as
// parkGroupJSON groups, which is what driveRun's park pass turns into a
// park.json awaiting a human ack (#109). A caller that only wants the interim
// flagging can ignore groups; the two always describe the same branch set.
//
// A branch is a trigger when it modifies policyFileName itself (self-protection:
// a change cannot loosen the bar that gates it) or touches a path matching an
// ack-paths glob. Because a group is entangled by write-set overlap — you cannot
// land part of it — the WHOLE group is held if ANY member triggers. Grouping
// uses the SAME partition the integrator will (write-set overlap + semantic
// edges), so the held set matches exactly. An absent policy holds nothing.
//
// Held groups compose with bisect salvage untouched: only the CLEARED branches
// reach integrate/verify/bisect, so disjoint clean groups still land.
func policyHoldback(pol policy, okBranches []string, writeSets map[string][]string, semanticEdges [][2]string) (clear []string, held []flaggedJSON, groups []parkGroupJSON) {
	if !pol.Present || len(okBranches) == 0 {
		return okBranches, nil, nil
	}
	changes := make([]cell.BranchChange, 0, len(okBranches))
	for _, b := range okBranches {
		changes = append(changes, cell.BranchChange{Branch: b, WriteSet: cell.NewWriteSet(writeSets[b]...)})
	}
	for _, g := range cell.PartitionSemantic(changes, semanticEdges) {
		groupReason, groupKind := "", ""
		entries := make([]flaggedJSON, 0, len(g))
		pg := parkGroupJSON{MatchedPaths: map[string]string{}}
		for _, bc := range g {
			kind, reason, matched := branchHoldReason(writeSets[bc.Branch], pol, false)
			if reason != "" && groupReason == "" {
				groupReason = reason
			}
			// policy-modified outranks ack-paths for the GROUP's reason, the same
			// precedence branchHoldReason applies within one branch.
			if kind == parkReasonPolicyModified || (kind != "" && groupKind == "") {
				groupKind = kind
			}
			paths := make([]string, 0, len(matched))
			for p, glob := range matched {
				paths = append(paths, p)
				pg.MatchedPaths[p] = glob
			}
			sort.Strings(paths)
			entries = append(entries, flaggedJSON{Branch: bc.Branch, Paths: paths, Reason: reason})
			pg.Branches = append(pg.Branches, bc.Branch)
		}
		if groupReason == "" {
			for _, bc := range g {
				clear = append(clear, bc.Branch)
			}
			continue
		}
		// Held group: a member held only by entanglement (no trigger of its own)
		// carries the group's trigger reason so the hold is always explained.
		for i := range entries {
			if entries[i].Reason == "" {
				entries[i].Reason = groupReason
			}
		}
		pg.Reason = groupKind
		held = append(held, entries...)
		groups = append(groups, pg)
	}
	return clear, held, groups
}

// branchHoldReason reports why one branch's own write-set holds it (kind empty
// when it triggers nothing). Precedence, most to least specific:
// policy-modified > unland-paths > ack-paths. kind is the machine-readable park
// reason (parkReason*), reason its human wording, and matched maps each
// triggering path to the GLOB that matched it (policyFileName maps to itself,
// since self-protection is not glob-driven).
//
// unland selects which glob sets apply. An UNLAND's inverse is tested against
// unland-paths first and then ack-paths — a path that needs an ack to change
// needs an ack to change back, so both bind it — while an ordinary landing sees
// ack-paths alone. Self-protection binds both by construction.
