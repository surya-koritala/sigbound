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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

// policyFileName is the fixed repo-root path the policy is read from at the base
// SHA. Also the path whose modification triggers the self-protection hold (a
// change cannot loosen the bar that gates it — see policyHoldback).
const policyFileName = "sigbound.policy"

// policy is a parsed, validated sigbound.policy. A zero policy (present=false)
// is what an absent file resolves to: exactly today's flag behavior, zero
// migration. auditSample is -1 when unset (0 is a leg, meaningful value).
type policy struct {
	present     bool
	hash        string   // sha256 (hex) of the exact file bytes; recorded as policyHash
	verify      []string // the verify battery, in file order (repeatable key), ANDed at resolve
	lanes       string   // "" | laneOff | laneStrict — only laneStrict is a floor
	semantic    string   // "" | semanticOff | semanticGo — only semanticGo is a floor
	assertSet   bool     // whether an assert= line was present
	assert      bool     // policy's assert value (a floor only when true)
	ackPaths    []string // globs; a landed change touching one PARKS the landing for a human ack (#109)
	auditSample int      // 0..100, or -1 when unset: the spot-audit sampling rate (see auditSelected)
	ackTimeout  time.Duration
	// ackTimeoutAction is what an EXPIRED park auto-transitions to (see
	// enforceParkTimeout). parkActionReject is the only value v2.0 accepts; the
	// key is parsed now so a future action name is a forward-compatible addition
	// rather than a new key. Defaults to parkActionReject whenever ackTimeout is
	// set, so `ack-timeout = 72h` alone is complete.
	ackTimeoutAction string
	parallel         int           // parallel-agents ceiling (0 = unset)
	maxAgents        int           // task-count ceiling (0 = unset)
	budget           time.Duration // budget ceiling (0 = unset)
	// watchInterval/watchBatch/watchMaxRed are `sig serve -watch`'s cadence
	// (issue #111), 0 when unset. Unlike every other key here these gate NOTHING
	// — they set how often this repo wants its arrivals integrated, not what a
	// landing must pass — so they are read by the watch loop at startup
	// (resolveWatchConfig) rather than by resolvePolicy, and never appear on the
	// report. They live here because the repo, not the invoker, owns the answer,
	// and because parsePolicy must know them or a repo that sets one would fail
	// every run with "unknown policy key".
	watchInterval time.Duration
	watchBatch    int
	watchMaxRed   int
}

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

// parsePolicy parses the flat KEY=VALUE bytes into a validated policy. It reuses
// parseConfigFile's lexer (comments, blank lines, first-'=' split, CRLF
// tolerance, line numbers) and adds the policy KEY SCHEMA on top: verify and
// ack-paths are repeatable; every other key is scalar and a DUPLICATE is an
// error (a second lanes=off silently overriding lanes=strict is the exact
// weaken-by-typo failure mode this file fails closed against). An UNKNOWN key is
// an error naming the line and key. hash is the sha256 of data verbatim.
func parsePolicy(data []byte) (policy, error) {
	entries, err := parseConfigFile(data)
	if err != nil {
		return policy{}, err
	}
	sum := sha256.Sum256(data)
	pol := policy{present: true, hash: hex.EncodeToString(sum[:]), auditSample: -1}
	seen := map[string]bool{} // scalar keys already set (duplicate => error)
	scalar := func(e configEntry) error {
		if seen[e.Key] {
			return fmt.Errorf("line %d: duplicate key %q", e.Line, e.Key)
		}
		seen[e.Key] = true
		return nil
	}
	for _, e := range entries {
		switch e.Key {
		case "verify":
			if strings.TrimSpace(e.Value) == "" {
				return policy{}, fmt.Errorf("line %d: verify requires a command", e.Line)
			}
			pol.verify = append(pol.verify, e.Value)
		case "ack-paths":
			globs := splitCSV(e.Value)
			if len(globs) == 0 {
				return policy{}, fmt.Errorf("line %d: ack-paths requires at least one glob", e.Line)
			}
			pol.ackPaths = append(pol.ackPaths, globs...)
		case "lanes":
			if err := scalar(e); err != nil {
				return policy{}, err
			}
			switch e.Value {
			case laneStrict, laneOff:
				pol.lanes = e.Value
			default:
				return policy{}, fmt.Errorf("line %d: lanes must be strict|off, got %q", e.Line, e.Value)
			}
		case "semantic":
			if err := scalar(e); err != nil {
				return policy{}, err
			}
			switch e.Value {
			case semanticGo, semanticOff:
				pol.semantic = e.Value
			default:
				return policy{}, fmt.Errorf("line %d: semantic must be go|off, got %q", e.Line, e.Value)
			}
		case "assert":
			if err := scalar(e); err != nil {
				return policy{}, err
			}
			b, err := strconv.ParseBool(e.Value)
			if err != nil {
				return policy{}, fmt.Errorf("line %d: assert must be true|false, got %q", e.Line, e.Value)
			}
			pol.assertSet, pol.assert = true, b
		case "audit-sample":
			if err := scalar(e); err != nil {
				return policy{}, err
			}
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(e.Value), "%"))
			if err != nil || n < 0 || n > 100 {
				return policy{}, fmt.Errorf("line %d: audit-sample must be an integer 0..100 (optionally with %%), got %q", e.Line, e.Value)
			}
			pol.auditSample = n
		case "ack-timeout":
			if err := scalar(e); err != nil {
				return policy{}, err
			}
			d, err := time.ParseDuration(e.Value)
			if err != nil || d < 0 {
				return policy{}, fmt.Errorf("line %d: ack-timeout must be a non-negative duration (e.g. 72h), got %q", e.Line, e.Value)
			}
			pol.ackTimeout = d
		case "ack-timeout-action":
			if err := scalar(e); err != nil {
				return policy{}, err
			}
			// reject is the only action v2.0 implements. Anything else is a hard
			// error rather than a silent no-op: a policy asking for an action this
			// binary cannot perform must not quietly leave an expired park open.
			if e.Value != parkActionReject {
				return policy{}, fmt.Errorf("line %d: ack-timeout-action must be %s, got %q", e.Line, parkActionReject, e.Value)
			}
			pol.ackTimeoutAction = e.Value
		case "parallel-agents":
			if err := scalar(e); err != nil {
				return policy{}, err
			}
			n, err := strconv.Atoi(strings.TrimSpace(e.Value))
			if err != nil || n < 0 {
				return policy{}, fmt.Errorf("line %d: parallel-agents must be a non-negative integer, got %q", e.Line, e.Value)
			}
			pol.parallel = n
		case "max-agents":
			if err := scalar(e); err != nil {
				return policy{}, err
			}
			n, err := strconv.Atoi(strings.TrimSpace(e.Value))
			if err != nil || n < 0 {
				return policy{}, fmt.Errorf("line %d: max-agents must be a non-negative integer, got %q", e.Line, e.Value)
			}
			pol.maxAgents = n
		case "budget":
			if err := scalar(e); err != nil {
				return policy{}, err
			}
			d, err := time.ParseDuration(e.Value)
			if err != nil || d < 0 {
				return policy{}, fmt.Errorf("line %d: budget must be a non-negative duration (e.g. 30m), got %q", e.Line, e.Value)
			}
			pol.budget = d
		case "watch-interval":
			if err := scalar(e); err != nil {
				return policy{}, err
			}
			d, err := time.ParseDuration(e.Value)
			if err != nil || d <= 0 {
				return policy{}, fmt.Errorf("line %d: watch-interval must be a positive duration (e.g. 30s), got %q", e.Line, e.Value)
			}
			pol.watchInterval = d
		case "watch-batch":
			if err := scalar(e); err != nil {
				return policy{}, err
			}
			n, err := strconv.Atoi(strings.TrimSpace(e.Value))
			if err != nil || n < 1 {
				return policy{}, fmt.Errorf("line %d: watch-batch must be a positive integer, got %q", e.Line, e.Value)
			}
			pol.watchBatch = n
		case "watch-max-red":
			if err := scalar(e); err != nil {
				return policy{}, err
			}
			n, err := strconv.Atoi(strings.TrimSpace(e.Value))
			if err != nil || n < 1 {
				return policy{}, fmt.Errorf("line %d: watch-max-red must be a positive integer, got %q", e.Line, e.Value)
			}
			pol.watchMaxRed = n
		default:
			return policy{}, fmt.Errorf("line %d: unknown policy key %q", e.Line, e.Key)
		}
	}
	// A bare `ack-timeout = 72h` is complete on its own: reject is the only
	// action v2.0 has, so defaulting here keeps the second key optional without
	// making an expired park's fate implicit anywhere downstream.
	if pol.ackTimeout > 0 && pol.ackTimeoutAction == "" {
		pol.ackTimeoutAction = parkActionReject
	}
	return pol, nil
}

// resolvePolicy applies pol to p IN PLACE, the single shared choke point both
// `sig run` and `sig serve` reach through driveRun. Precedence is
// stricter-only: flags may tighten policy, never loosen it.
//
//   - verify: the flag/request verify command is APPENDED to the policy battery
//     (both run). Members are ANDed in file order, each run in its own nested
//     `sh -c` (see joinVerifyBattery) so an untrusted member's metacharacters
//     cannot escape to mask another member's failure, and any member's failure
//     fails the whole gate — the -verify invariant, never weakened.
//   - lanes/semantic/assert: policy strict/go/true is a FLOOR. An unset flag is
//     silently raised to it; an EXPLICIT weaker flag is a loud error naming both
//     sources and values.
//   - quotas (parallel-agents, budget): effective = min(policy, flag) via the
//     same clamp posture serve's quota code uses. max-agents rejects a run whose
//     task count exceeds it (a run can't silently drop tasks — same reject
//     semantics as serve's max-agents-per-run).
//
// An absent policy resolves to a no-op, leaving p byte-identical to today.
func resolvePolicy(pol policy, p *runParams, taskCount int) error {
	if !pol.present {
		return nil
	}
	// verify: policy battery, then the flag/request verify appended.
	battery := append([]string(nil), pol.verify...)
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
	if len(pol.verify) > 0 {
		p.VerifyImpactCmd = ""
	}
	// lanes: strict floor.
	if pol.lanes == laneStrict && laneRank(p.LaneMode) < laneRank(laneStrict) {
		if p.PolicyExplicit.Lanes {
			return fmt.Errorf("policy %s: lanes=strict; flag -lanes=%s — flags may only tighten policy", policyFileName, p.LaneMode)
		}
		p.LaneMode = laneStrict
	}
	// semantic: go floor.
	if pol.semantic == semanticGo && p.Semantic != semanticGo {
		if p.PolicyExplicit.Semantic {
			return fmt.Errorf("policy %s: semantic=go; flag -semantic=%s — flags may only tighten policy", policyFileName, effectiveSemantic(p.Semantic))
		}
		p.Semantic = semanticGo
	}
	// assert: true floor.
	if pol.assertSet && pol.assert && !p.Assert {
		if p.PolicyExplicit.Assert {
			return fmt.Errorf("policy %s: assert=true; flag -assert=false — flags may only tighten policy", policyFileName)
		}
		p.Assert = true
	}
	// quotas: min-clamp, no error (min is the established, documented semantics).
	p.ParallelAgents = clampCeiling(p.ParallelAgents, pol.parallel)
	p.Budget = clampCeiling(p.Budget, pol.budget)
	if pol.maxAgents > 0 && taskCount > pol.maxAgents {
		return fmt.Errorf("policy %s: max-agents=%d, but this run has %d tasks", policyFileName, pol.maxAgents, taskCount)
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
	if !pol.present {
		return nil
	}
	out := &policyJSON{Hash: pol.hash, Verify: pol.verify, AckPaths: pol.ackPaths}
	if pol.auditSample >= 0 {
		n := pol.auditSample
		out.AuditSample = &n
	}
	if pol.ackTimeout > 0 {
		out.AckTimeout = pol.ackTimeout.String()
		out.AckTimeoutAction = pol.ackTimeoutAction
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
	if !pol.present || len(okBranches) == 0 {
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
			kind, reason, matched := branchHoldReason(writeSets[bc.Branch], pol)
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
// when it triggers nothing): self-modification of the policy file takes
// precedence over an ack-path match. kind is the machine-readable park reason
// (parkReason*), reason its human wording, and matched maps each triggering path
// to the ack-paths GLOB that matched it (policyFileName maps to itself, since
// self-protection is not glob-driven).
func branchHoldReason(paths []string, pol policy) (kind, reason string, matched map[string]string) {
	for _, p := range paths {
		if p == policyFileName {
			return parkReasonPolicyModified, "policy: run modifies " + policyFileName, map[string]string{policyFileName: policyFileName}
		}
	}
	acked := map[string]string{}
	first := ""
	for _, p := range paths {
		for _, glob := range pol.ackPaths {
			if globMatch(glob, p) {
				acked[p] = glob
				if first == "" || p < first {
					first = p
				}
				break
			}
		}
	}
	if len(acked) > 0 {
		return parkReasonAckPaths, "policy: ack required for " + first, acked
	}
	return "", "", nil
}

// globMatch reports whether pattern matches name, both slash-separated
// repo-relative paths. Semantics (defined once here, fuzzed and property-tested):
//
//   - '?'  matches any single character except '/'.
//   - '*'  matches any run of characters except '/' (stays within one segment).
//   - '**' matches any run of characters INCLUDING '/' (crosses segments); a
//     '**/' prefix additionally matches zero segments, so '**/x' matches both
//     'x' and 'a/b/x'. Consecutive stars collapse ('***' == '**').
//   - every other character is literal.
//
// Backtracking is memoized on (pattern index, name index), so an adversarial
// pattern (many '**') stays O(len(pattern)*len(name)) instead of exponential.
func globMatch(pattern, name string) bool {
	memo := make(map[[2]int]bool)
	var rec func(pi, si int) bool
	rec = func(pi, si int) (res bool) {
		key := [2]int{pi, si}
		if v, ok := memo[key]; ok {
			return v
		}
		defer func() { memo[key] = res }()
		for pi < len(pattern) {
			c := pattern[pi]
			switch c {
			case '*':
				if pi+1 < len(pattern) && pattern[pi+1] == '*' {
					// '**': collapse the run of stars, match any suffix.
					for pi < len(pattern) && pattern[pi] == '*' {
						pi++
					}
					if pi == len(pattern) {
						return true // trailing '**' matches the rest, '/' and all
					}
					// '**/' also matches zero leading segments.
					if pattern[pi] == '/' && rec(pi+1, si) {
						return true
					}
					for i := si; i <= len(name); i++ {
						if rec(pi, i) {
							return true
						}
					}
					return false
				}
				// single '*': match a run of non-'/' characters.
				pi++
				for i := si; ; i++ {
					if rec(pi, i) {
						return true
					}
					if i >= len(name) || name[i] == '/' {
						return false
					}
				}
			case '?':
				if si >= len(name) || name[si] == '/' {
					return false
				}
				pi++
				si++
			default:
				if si >= len(name) || name[si] != c {
					return false
				}
				pi++
				si++
			}
		}
		return si == len(name)
	}
	return rec(0, 0)
}
