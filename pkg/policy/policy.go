// Package policy parses and evaluates a repository's sigbound.policy — the
// landing bar the repo itself owns.
//
// It is the half of that machinery with no I/O in it. Nothing here reads a
// file, runs git, spawns a process, or touches the network: a caller loads the
// bytes however it likes and hands them over. That is what makes it usable from
// outside the `sig` binary — a service enforcing the same bar on a push it just
// received has the bytes already and has no worktree at all.
//
// The engine consumes this package rather than keeping a second copy, so the
// rules a hosted gate applies are, by construction, the rules `sig` applies.
// Anything that needs git (loading the policy at a base SHA), a run's flags
// (resolving policy against them), or the integrator (grouping held branches)
// lives in the engine, where those things exist.
//
// Zero dependencies, stdlib only.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FileName is the fixed repo-root path a policy is read from. It is also the
// path whose modification triggers the self-protection hold: a change must not
// loosen the bar that gates it.
const FileName = "sigbound.policy"

// Lane and semantic modes, and the one park action. Values a policy file may
// name, defined here because this package validates them.
const (
	LaneOff          = "off"
	LaneStrict       = "strict"
	SemanticOff      = "off"
	SemanticGo       = "go"
	ParkActionReject = "reject"
)

// Policy is a parsed, validated sigbound.policy.
//
// A ZERO Policy (Present false) is what an absent file resolves to, and it is
// deliberately meaningful: it means "no policy", which is the no-migration
// default, not "an empty policy". AuditSample is -1 when unset, since 0 is a
// real value.
type Policy struct {
	Present bool
	// Hash is the sha256 (hex) of the exact file bytes. It is what a run records
	// as the policy it was gated by, so a landing can be checked against the
	// exact text that governed it rather than against whatever the file says now.
	Hash string

	// Verify is the verify battery in file order (the key is repeatable), ANDed
	// when resolved.
	Verify []string

	// Lanes/Semantic/Assert are FLOORS, not settings: only the stricter value of
	// each is a floor a caller may not go under. AssertSet distinguishes an
	// explicit assert=false from an absent key.
	Lanes     string
	Semantic  string
	AssertSet bool
	Assert    bool

	// AckPaths are globs whose modification PARKS a landing for a human.
	AckPaths []string
	// UnlandPaths park an UNLAND's inverse specifically. AckPaths binds an
	// inverse too — a path that needs an ack to change needs an ack to change
	// back — so this exists only for the asymmetric case: paths cheap to land
	// forward and expensive to take back.
	UnlandPaths []string
	// RepairDeny are globs the repair fixer may never write. Separate from
	// AckPaths because the two answer different questions: AckPaths says a human
	// must approve this change, which is right for an agent doing requested work
	// and wrong for an automatic fixer nobody asked for.
	RepairDeny []string

	// AuditSample is 0..100, or -1 when unset.
	AuditSample      int
	AckTimeout       time.Duration
	AckTimeoutAction string

	// Quota ceilings. Zero means unset.
	Parallel  int
	MaxAgents int
	Budget    time.Duration

	// Watch cadence. Unlike every other key these gate NOTHING — they say how
	// often the repo wants its arrivals integrated, not what a landing must pass.
	WatchInterval time.Duration
	WatchBatch    int
	WatchMaxRed   int
}

// Entry is one KEY=VALUE line, with the 1-based line number a diagnostic needs.
type Entry struct {
	Key   string
	Value string
	Line  int
}

// ParseFile splits flat KEY=VALUE bytes into entries: comments and blank lines
// dropped, first '=' splits, CRLF tolerated, line numbers kept.
//
// ONE flat-file dialect, not two. sig.conf and sigbound.policy are read by this
// same lexer, so a repo learns one syntax; the schema on top is what differs.
func ParseFile(data []byte) ([]Entry, error) {
	var entries []Entry
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE, got %q", i+1, trimmed)
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", i+1)
		}
		entries = append(entries, Entry{Key: key, Value: strings.TrimSpace(line[eq+1:]), Line: i + 1})
	}
	return entries, nil
}

// SplitCSV splits a comma-separated value, dropping empties and trimming space.
func SplitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Parse reads the policy KEY SCHEMA on top of ParseFile's lexer.
//
// It FAILS CLOSED, and that is the design. `verify`, `ack-paths`,
// `unland-paths` and `repair-deny` are repeatable; every other key is scalar and
// a DUPLICATE is an error — a second `lanes = off` silently overriding
// `lanes = strict` is the exact weaken-by-typo failure this refuses. An UNKNOWN
// key is an error naming the line and key, for the same reason: a typo must
// never quietly lower the bar.
//
// Hash is the sha256 of data verbatim.
func Parse(data []byte) (Policy, error) {
	entries, err := ParseFile(data)
	if err != nil {
		return Policy{}, err
	}
	sum := sha256.Sum256(data)
	pol := Policy{Present: true, Hash: hex.EncodeToString(sum[:]), AuditSample: -1}

	seen := map[string]bool{}
	scalar := func(e Entry) error {
		if seen[e.Key] {
			return fmt.Errorf("line %d: duplicate key %q", e.Line, e.Key)
		}
		seen[e.Key] = true
		return nil
	}
	globs := func(e Entry, into *[]string) error {
		g := SplitCSV(e.Value)
		if len(g) == 0 {
			return fmt.Errorf("line %d: %s requires at least one glob", e.Line, e.Key)
		}
		*into = append(*into, g...)
		return nil
	}
	posDur := func(e Entry, into *time.Duration, allowZero bool, example string) error {
		if err := scalar(e); err != nil {
			return err
		}
		d, derr := time.ParseDuration(e.Value)
		if derr != nil || d < 0 || (!allowZero && d <= 0) {
			what := "a non-negative"
			if !allowZero {
				what = "a positive"
			}
			return fmt.Errorf("line %d: %s must be %s duration (e.g. %s), got %q", e.Line, e.Key, what, example, e.Value)
		}
		*into = d
		return nil
	}
	nonNegInt := func(e Entry, into *int, min int) error {
		if err := scalar(e); err != nil {
			return err
		}
		n, cerr := strconv.Atoi(strings.TrimSpace(e.Value))
		if cerr != nil || n < min {
			word := "non-negative"
			if min > 0 {
				word = "positive"
			}
			return fmt.Errorf("line %d: %s must be a %s integer, got %q", e.Line, e.Key, word, e.Value)
		}
		*into = n
		return nil
	}

	for _, e := range entries {
		var kerr error
		switch e.Key {
		case "verify":
			if strings.TrimSpace(e.Value) == "" {
				kerr = fmt.Errorf("line %d: verify requires a command", e.Line)
				break
			}
			pol.Verify = append(pol.Verify, e.Value)
		case "ack-paths":
			kerr = globs(e, &pol.AckPaths)
		case "unland-paths":
			kerr = globs(e, &pol.UnlandPaths)
		case "repair-deny":
			kerr = globs(e, &pol.RepairDeny)
		case "lanes":
			if kerr = scalar(e); kerr == nil {
				switch e.Value {
				case LaneStrict, LaneOff:
					pol.Lanes = e.Value
				default:
					kerr = fmt.Errorf("line %d: lanes must be strict|off, got %q", e.Line, e.Value)
				}
			}
		case "semantic":
			if kerr = scalar(e); kerr == nil {
				switch e.Value {
				case SemanticGo, SemanticOff:
					pol.Semantic = e.Value
				default:
					kerr = fmt.Errorf("line %d: semantic must be go|off, got %q", e.Line, e.Value)
				}
			}
		case "assert":
			if kerr = scalar(e); kerr == nil {
				b, perr := strconv.ParseBool(e.Value)
				if perr != nil {
					kerr = fmt.Errorf("line %d: assert must be true|false, got %q", e.Line, e.Value)
					break
				}
				pol.AssertSet, pol.Assert = true, b
			}
		case "audit-sample":
			if kerr = scalar(e); kerr == nil {
				n, perr := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(e.Value), "%"))
				if perr != nil || n < 0 || n > 100 {
					kerr = fmt.Errorf("line %d: audit-sample must be an integer 0..100 (optionally with %%), got %q", e.Line, e.Value)
					break
				}
				pol.AuditSample = n
			}
		case "ack-timeout":
			kerr = posDur(e, &pol.AckTimeout, true, "72h")
		case "ack-timeout-action":
			if kerr = scalar(e); kerr == nil {
				// reject is the only action implemented. Anything else is a hard
				// error rather than a silent no-op: a policy asking for an action
				// this binary cannot perform must not quietly leave a park open.
				if e.Value != ParkActionReject {
					kerr = fmt.Errorf("line %d: ack-timeout-action must be %s, got %q", e.Line, ParkActionReject, e.Value)
					break
				}
				pol.AckTimeoutAction = e.Value
			}
		case "parallel-agents":
			kerr = nonNegInt(e, &pol.Parallel, 0)
		case "max-agents":
			kerr = nonNegInt(e, &pol.MaxAgents, 0)
		case "budget":
			kerr = posDur(e, &pol.Budget, true, "30m")
		case "watch-interval":
			kerr = posDur(e, &pol.WatchInterval, false, "30s")
		case "watch-batch":
			kerr = nonNegInt(e, &pol.WatchBatch, 1)
		case "watch-max-red":
			kerr = nonNegInt(e, &pol.WatchMaxRed, 1)
		default:
			kerr = fmt.Errorf("line %d: unknown policy key %q", e.Line, e.Key)
		}
		if kerr != nil {
			return Policy{}, kerr
		}
	}
	// A bare `ack-timeout = 72h` is complete on its own: reject is the only
	// action there is, so defaulting here keeps the second key optional without
	// making an expired park's fate implicit anywhere downstream.
	if pol.AckTimeout > 0 && pol.AckTimeoutAction == "" {
		pol.AckTimeoutAction = ParkActionReject
	}
	return pol, nil
}

// Park reasons, in the precedence HoldReason applies.
const (
	ReasonPolicyModified = "policy-modified"
	ReasonUnlandPaths    = "unland-paths"
	ReasonAckPaths       = "ack-paths"
)

// HoldReason reports why a write-set holds a landing for a human — Kind empty
// when it triggers nothing. Precedence, most to least specific:
// policy-modified > unland-paths > ack-paths.
//
// unland selects which glob sets apply. An UNLAND's inverse is tested against
// UnlandPaths first and then AckPaths — a path that needs an ack to change needs
// an ack to change back, so both bind it — while an ordinary landing sees
// AckPaths alone. Self-protection binds both by construction.
//
// Matched maps each triggering path to the glob that matched it; FileName maps
// to itself, since self-protection is not glob-driven.
func HoldReason(paths []string, pol Policy, unland bool) Hold {
	for _, p := range paths {
		if p == FileName {
			return Hold{Kind: ReasonPolicyModified, Reason: "policy: run modifies " + FileName,
				Matched: map[string]string{FileName: FileName}}
		}
	}
	if unland {
		if h := globHold(paths, pol.UnlandPaths, ReasonUnlandPaths, "policy: ack required to unland "); h.Kind != "" {
			return h
		}
	}
	return globHold(paths, pol.AckPaths, ReasonAckPaths, "policy: ack required for ")
}

// Hold is one holdback or refusal decision. Kind is empty when nothing fired.
type Hold struct {
	Kind    string
	Reason  string
	Matched map[string]string // triggering path -> the glob that matched it
	Paths   []string          // Matched's keys, sorted
}

// globHold matches paths against one glob set, wording the reason with the
// alphabetically first triggering path so the message is stable.
func globHold(paths, globs []string, kind, wording string) Hold {
	matched := map[string]string{}
	first := ""
	for _, p := range paths {
		for _, g := range globs {
			if GlobMatch(g, p) {
				matched[p] = g
				if first == "" || p < first {
					first = p
				}
				break
			}
		}
	}
	if len(matched) == 0 {
		return Hold{}
	}
	return Hold{Kind: kind, Reason: wording + first, Matched: matched, Paths: sortedKeys(matched)}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The rule that refused a repair attempt, most to least specific.
const (
	RepairRefusedPolicyFile = "policy-file"
	RepairRefusedDeny       = "repair-deny"
	RepairRefusedAckPaths   = "ack-paths"
	// RepairRefusedUnknownWriteSet is the fail-closed case: the diff that would
	// have been checked could not be read, so nothing about the fixer's edits is
	// known. Refusing an unknowable write-set is the only safe direction.
	RepairRefusedUnknownWriteSet = "unknown-write-set"
)

// RepairRefusal reports why a repair attempt's write-set must be REFUSED — Kind
// empty when the fixer may keep what it wrote.
//
// The fixer is the one agent in a run with no brief: it is handed a failure and
// rewarded for making it stop, which makes weakening whatever judged it the
// shortest path to success. So unlike an ordinary agent it is barred from three
// sets rather than merely held for review:
//
//   - FileName itself, UNCONDITIONALLY — with or without a policy file. A fixer
//     has no business writing the file that decides whether its own work lands,
//     and there is no repo state in which it does.
//   - RepairDeny globs — what this repo says the fixer may never write,
//     typically its tests and CI config. Ordinary agents stay unaffected.
//   - AckPaths globs — a path a human must approve cannot be changed by a
//     machine no human is going to look at. This leg matters most: a landing
//     holdback is decided from the AGENTS' write-sets, before repair has run at
//     all, so without this the fixer is the single path through the ack bar.
//
// Refusal is deliberately harsher than the park an equivalent agent change
// earns. A park exists to put work in front of a person; a repair is work nobody
// asked for, so there is nothing to hold — declining it leaves the tree that
// honestly failed.
func RepairRefusal(paths []string, pol Policy) Hold {
	for _, p := range paths {
		if p == FileName {
			return Hold{Kind: RepairRefusedPolicyFile, Reason: "repair may not modify " + FileName,
				Matched: map[string]string{FileName: FileName}, Paths: []string{FileName}}
		}
	}
	for _, r := range []struct {
		kind    string
		globs   []string
		wording string
	}{
		{RepairRefusedDeny, pol.RepairDeny, "repair-deny: repair may not modify "},
		{RepairRefusedAckPaths, pol.AckPaths, "ack-paths: repair may not modify a path needing a human ack, "},
	} {
		if h := globHold(paths, r.globs, r.kind, r.wording); h.Kind != "" {
			return h
		}
	}
	return Hold{}
}

// GlobMatch reports whether pattern matches name, both slash-separated
// repo-relative paths. Semantics, defined once here and property-tested:
//
//   - '?'  matches any single character except '/'.
//   - '*'  matches any run of characters except '/' (stays within one segment).
//   - '**' matches any run INCLUDING '/' (crosses segments); a '**/' prefix
//     additionally matches zero segments, so '**/x' matches both 'x' and
//     'a/b/x'. Consecutive stars collapse ('***' == '**').
//   - every other character is literal.
//
// Backtracking is memoized on (pattern index, name index), so an adversarial
// pattern (many '**') stays O(len(pattern)*len(name)) instead of exponential.
// That bound matters here: patterns come from a repo file that an agent may
// have written.
func GlobMatch(pattern, name string) bool {
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
