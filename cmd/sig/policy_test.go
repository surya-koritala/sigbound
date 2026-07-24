package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---- parsePolicy ----

// TestParsePolicyValid parses a full policy and checks every key maps to the
// right field: verify is a battery in file order, ack-paths is repeatable AND
// comma-splittable, and the scalars/quotas/durations all land. The hash is the
// sha256 of the exact bytes (non-empty, present=true).
func TestParsePolicyValid(t *testing.T) {
	src := "# landing policy\n" +
		"verify = go build ./...\n" +
		"verify = go test ./...\n" +
		"lanes = strict\n" +
		"semantic = go\n" +
		"assert = true\n" +
		"ack-paths = auth/**, billing/**\n" +
		"ack-paths = infra/*.tf\n" +
		"audit-sample = 25%\n" +
		"ack-timeout = 72h\n" +
		"parallel-agents = 8\n" +
		"max-agents = 16\n" +
		"budget = 30m\n"
	pol, err := parsePolicy([]byte(src))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}
	if !pol.present || pol.hash == "" {
		t.Fatalf("present=%v hash=%q, want present with a hash", pol.present, pol.hash)
	}
	if want := []string{"go build ./...", "go test ./..."}; !reflect.DeepEqual(pol.verify, want) {
		t.Fatalf("verify=%v, want %v", pol.verify, want)
	}
	if want := []string{"auth/**", "billing/**", "infra/*.tf"}; !reflect.DeepEqual(pol.ackPaths, want) {
		t.Fatalf("ackPaths=%v, want %v", pol.ackPaths, want)
	}
	if pol.lanes != laneStrict || pol.semantic != semanticGo || !pol.assert || !pol.assertSet {
		t.Fatalf("lanes=%q semantic=%q assert=%v/%v", pol.lanes, pol.semantic, pol.assert, pol.assertSet)
	}
	if pol.auditSample != 25 || pol.ackTimeout != 72*time.Hour {
		t.Fatalf("auditSample=%d ackTimeout=%v", pol.auditSample, pol.ackTimeout)
	}
	if pol.parallel != 8 || pol.maxAgents != 16 || pol.budget != 30*time.Minute {
		t.Fatalf("parallel=%d maxAgents=%d budget=%v", pol.parallel, pol.maxAgents, pol.budget)
	}
}

// TestParsePolicyEmptyIsPresent: an empty (or comment-only) policy file still
// EXISTS — present=true with a hash — so the self-protection hold is active even
// when the file declares nothing. (Absence, which resolves to a zero policy, is
// loadPolicy's job, tested end-to-end.)
func TestParsePolicyEmptyIsPresent(t *testing.T) {
	pol, err := parsePolicy([]byte("# nothing but a comment\n"))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}
	if !pol.present || pol.hash == "" || pol.auditSample != -1 {
		t.Fatalf("present=%v hash=%q auditSample=%d, want present, hashed, unset(-1)", pol.present, pol.hash, pol.auditSample)
	}
}

// TestParsePolicyUnknownKey: a typo'd key fails closed, naming the line and the
// key — a silently-ignored key could be a policy the repo THINKS it declared.
func TestParsePolicyUnknownKey(t *testing.T) {
	_, err := parsePolicy([]byte("verify = go test ./...\nlanez = strict\n"))
	if err == nil || !strings.Contains(err.Error(), `line 2: unknown policy key "lanez"`) {
		t.Fatalf("err=%v, want it to name line 2 + the bad key", err)
	}
}

// TestParsePolicyMalformedValues: every scalar validates its value, and the
// error names the line. Table covers each value class's failure mode.
func TestParsePolicyMalformedValues(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"lanes", "lanes = warn\n", "lanes must be strict|off"},
		{"semantic", "semantic = rust\n", "semantic must be go|off"},
		{"assert", "assert = maybe\n", "assert must be true|false"},
		{"audit-hi", "audit-sample = 250%\n", "audit-sample must be an integer 0..100"},
		{"audit-neg", "audit-sample = -5\n", "audit-sample must be an integer 0..100"},
		{"ack-timeout", "ack-timeout = soon\n", "ack-timeout must be a non-negative duration"},
		{"parallel", "parallel-agents = lots\n", "parallel-agents must be a non-negative integer"},
		{"max-agents", "max-agents = -1\n", "max-agents must be a non-negative integer"},
		{"budget", "budget = ages\n", "budget must be a non-negative duration"},
		{"empty-verify", "verify =\n", "verify requires a command"},
	} {
		_, err := parsePolicy([]byte(tc.src))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

// TestParsePolicyDuplicateScalar: a repeated SCALAR key is an error (a second
// lanes= silently overriding the first is the exact weaken-by-typo failure this
// file fails closed against); verify and ack-paths are exempt (repeatable).
func TestParsePolicyDuplicateScalar(t *testing.T) {
	_, err := parsePolicy([]byte("lanes = strict\nlanes = off\n"))
	if err == nil || !strings.Contains(err.Error(), `line 2: duplicate key "lanes"`) {
		t.Fatalf("err=%v, want a duplicate-key error on line 2", err)
	}
	if _, err := parsePolicy([]byte("verify = a\nverify = b\nack-paths = x\nack-paths = y\n")); err != nil {
		t.Fatalf("repeatable keys must not be duplicate errors: %v", err)
	}
}

// TestParsePolicyHashChangesWithBytes: the hash is over the exact bytes, so any
// content change (even a comment) changes it — that is what makes policyHash a
// faithful fingerprint of the gating file.
func TestParsePolicyHashChangesWithBytes(t *testing.T) {
	a, _ := parsePolicy([]byte("lanes = strict\n"))
	b, _ := parsePolicy([]byte("lanes = strict\n# note\n"))
	if a.hash == b.hash || a.hash == "" {
		t.Fatalf("hashes should differ: %q vs %q", a.hash, b.hash)
	}
}

// ---- globMatch ----

// TestGlobMatch pins the documented semantics: ? and * stay within a segment,
// ** crosses segments, and a **/ prefix matches zero leading segments.
func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"auth/**", "auth/login.go", true},
		{"auth/**", "auth/oauth/token.go", true},
		{"auth/**", "authz/login.go", false}, // literal "auth/" prefix, not "authz/"
		{"auth/**", "auth", false},           // ** needs the trailing slash content
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false}, // single * never crosses '/'
		{"**/*.go", "cmd/sig/main.go", true},
		{"**/secrets.yaml", "secrets.yaml", true}, // **/ matches zero segments
		{"**/secrets.yaml", "deploy/prod/secrets.yaml", true},
		{"infra/*.tf", "infra/main.tf", true},
		{"infra/*.tf", "infra/mod/main.tf", false},
		{"a?c", "abc", true},
		{"a?c", "a/c", false}, // ? never matches '/'
		{"**", "any/deep/path", true},
		{"exact", "exact", true},
		{"exact", "exacto", false},
	} {
		if got := globMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("globMatch(%q, %q)=%v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// TestGlobMatchDoubleStarProperty: '**' alone matches EVERY path, and 'p/**'
// matches exactly the paths under 'p/' — the core ack-paths property.
func TestGlobMatchDoubleStarProperty(t *testing.T) {
	for _, p := range []string{"", "x", "a/b", "a/b/c/d", "auth/x", "authx/y"} {
		if !globMatch("**", p) {
			t.Errorf("** should match %q", p)
		}
		under := strings.HasPrefix(p, "auth/")
		if globMatch("auth/**", p) != under {
			t.Errorf("auth/** on %q = %v, want %v", p, !under, under)
		}
	}
}

// ---- resolvePolicy ----

// TestResolvePolicyVerifyAppend: the flag verify is APPENDED to the policy
// battery (both run), ANDed. With no flag verify, the policy battery alone
// becomes the effective command.
func TestResolvePolicyVerifyAppend(t *testing.T) {
	pol := policy{present: true, verify: []string{"go build ./...", "go test ./..."}}

	p := runParams{VerifyCmd: "golangci-lint run"}
	if err := resolvePolicy(pol, &p, 1); err != nil {
		t.Fatal(err)
	}
	// All three run, flag verify last, each in its own subshell.
	for _, want := range []string{"go build ./...", "go test ./...", "golangci-lint run"} {
		if !strings.Contains(p.VerifyCmd, want) {
			t.Fatalf("effective verify %q missing %q", p.VerifyCmd, want)
		}
	}
	if strings.Count(p.VerifyCmd, "&&") != 2 {
		t.Fatalf("effective verify %q: want two && joins", p.VerifyCmd)
	}

	p2 := runParams{} // no flag verify: policy battery alone
	if err := resolvePolicy(pol, &p2, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p2.VerifyCmd, "go build") || !strings.Contains(p2.VerifyCmd, "go test") {
		t.Fatalf("policy-only verify %q, want both members", p2.VerifyCmd)
	}

	// A policy battery forces FULL verify: an invoker's -verify-impact scoping is
	// dropped so it can't run instead of (and thereby bypass) the policy battery.
	p3 := runParams{VerifyCmd: "go test ./...", VerifyImpactCmd: "go test $SIGBOUND_IMPACTED_PKGS"}
	if err := resolvePolicy(pol, &p3, 1); err != nil {
		t.Fatal(err)
	}
	if p3.VerifyImpactCmd != "" {
		t.Fatalf("VerifyImpactCmd=%q, want it cleared when a policy battery is present", p3.VerifyImpactCmd)
	}
}

// TestResolvePolicyTightenOnly is the invoker-cannot-weaken core, per key class:
// an unset flag is raised to the policy floor silently; an EXPLICIT weaker flag
// is a loud error naming both sources.
func TestResolvePolicyTightenOnly(t *testing.T) {
	strictPol := policy{present: true, lanes: laneStrict, semantic: semanticGo, assertSet: true, assert: true}

	// Unset flags (default warn / off / false) tighten silently to the floor.
	p := runParams{LaneMode: laneWarn, Semantic: semanticOff}
	if err := resolvePolicy(strictPol, &p, 1); err != nil {
		t.Fatalf("silent tighten must not error: %v", err)
	}
	if p.LaneMode != laneStrict || p.Semantic != semanticGo || !p.Assert {
		t.Fatalf("after tighten: lanes=%q semantic=%q assert=%v, want strict/go/true", p.LaneMode, p.Semantic, p.Assert)
	}

	// Explicit weaker choices each error, naming both sides.
	for _, tc := range []struct {
		name string
		p    runParams
		want string
	}{
		{"lanes", runParams{LaneMode: laneOff, PolicyExplicit: policyExplicit{Lanes: true}}, "lanes=strict"},
		{"semantic", runParams{LaneMode: laneStrict, Semantic: semanticOff, PolicyExplicit: policyExplicit{Semantic: true}}, "semantic=go"},
		{"assert", runParams{LaneMode: laneStrict, Semantic: semanticGo, PolicyExplicit: policyExplicit{Assert: true}}, "assert=true"},
	} {
		pp := tc.p
		err := resolvePolicy(strictPol, &pp, 1)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "tighten") {
			t.Errorf("%s: err=%v, want a tighten-only error naming %q", tc.name, err, tc.want)
		}
	}
}

// TestResolvePolicyQuotaMinClamp: quotas are min(policy, flag) with the serve
// clamp convention (a non-positive flag becomes the ceiling) — never an error.
func TestResolvePolicyQuotaMinClamp(t *testing.T) {
	pol := policy{present: true, parallel: 4, budget: 10 * time.Minute}

	// Flag under the ceiling wins; flag over the ceiling is clamped down.
	p := runParams{ParallelAgents: 2, Budget: 5 * time.Minute}
	if err := resolvePolicy(pol, &p, 1); err != nil {
		t.Fatal(err)
	}
	if p.ParallelAgents != 2 || p.Budget != 5*time.Minute {
		t.Fatalf("under ceiling: parallel=%d budget=%v, want 2 / 5m", p.ParallelAgents, p.Budget)
	}
	p = runParams{ParallelAgents: 64, Budget: time.Hour}
	if err := resolvePolicy(pol, &p, 1); err != nil {
		t.Fatal(err)
	}
	if p.ParallelAgents != 4 || p.Budget != 10*time.Minute {
		t.Fatalf("over ceiling: parallel=%d budget=%v, want clamp to 4 / 10m", p.ParallelAgents, p.Budget)
	}
	// Unset flag (0) becomes the ceiling.
	p = runParams{}
	if err := resolvePolicy(pol, &p, 1); err != nil {
		t.Fatal(err)
	}
	if p.ParallelAgents != 4 || p.Budget != 10*time.Minute {
		t.Fatalf("unset flag: parallel=%d budget=%v, want ceiling 4 / 10m", p.ParallelAgents, p.Budget)
	}
}

// TestResolvePolicyMaxAgentsReject: a run whose task count exceeds max-agents is
// rejected (a run can't silently drop tasks), same reject posture as serve's
// max-agents-per-run; at/under the ceiling is fine.
func TestResolvePolicyMaxAgentsReject(t *testing.T) {
	pol := policy{present: true, maxAgents: 3}
	p := runParams{}
	if err := resolvePolicy(pol, &p, 4); err == nil || !strings.Contains(err.Error(), "max-agents=3") {
		t.Fatalf("4 tasks vs max-agents=3: err=%v, want a reject naming the ceiling", err)
	}
	if err := resolvePolicy(pol, &p, 3); err != nil {
		t.Fatalf("3 tasks vs max-agents=3 must pass: %v", err)
	}
}

// TestResolvePolicyAbsentNoop: an absent policy leaves p untouched — the
// zero-migration guarantee at the resolver layer.
func TestResolvePolicyAbsentNoop(t *testing.T) {
	p := runParams{LaneMode: laneWarn, Semantic: semanticOff, VerifyCmd: "go test ./...", ParallelAgents: 9, Budget: time.Hour}
	before := p
	if err := resolvePolicy(policy{}, &p, 100); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, before) {
		t.Fatalf("absent policy mutated params: %+v -> %+v", before, p)
	}
}

// ---- policyHoldback ----

// TestPolicyHoldbackAckPath: a branch touching an ack-path is held with the
// ack reason; a disjoint clean branch is cleared to integrate.
func TestPolicyHoldbackAckPath(t *testing.T) {
	pol := policy{present: true, ackPaths: []string{"auth/**"}}
	ws := map[string][]string{
		"agent/a": {"auth/login.go"},
		"agent/b": {"docs/readme.md"},
	}
	clear, held := policyHoldback(pol, []string{"agent/a", "agent/b"}, ws, nil)
	if !reflect.DeepEqual(clear, []string{"agent/b"}) {
		t.Fatalf("clear=%v, want [agent/b]", clear)
	}
	if len(held) != 1 || held[0].Branch != "agent/a" || !strings.Contains(held[0].Reason, "ack required for auth/login.go") {
		t.Fatalf("held=%+v, want agent/a with an ack reason", held)
	}
}

// TestPolicyHoldbackSelfModification: a branch modifying sigbound.policy is held
// unconditionally (a change cannot loosen the bar that gates it), even with no
// ack-paths declared — the mere existence of a policy activates self-protection.
func TestPolicyHoldbackSelfModification(t *testing.T) {
	pol := policy{present: true} // no ack-paths
	ws := map[string][]string{"agent/a": {policyFileName, "x.go"}}
	clear, held := policyHoldback(pol, []string{"agent/a"}, ws, nil)
	if len(clear) != 0 || len(held) != 1 || !strings.Contains(held[0].Reason, "modifies "+policyFileName) {
		t.Fatalf("clear=%v held=%+v, want agent/a held for self-modification", clear, held)
	}
}

// TestPolicyHoldbackGroupEntanglement: a clean branch overlapping an ack-path
// branch (same group by write-set overlap) is held TOO — you can't land part of
// an entangled group — and its reason carries the group's trigger.
func TestPolicyHoldbackGroupEntanglement(t *testing.T) {
	pol := policy{present: true, ackPaths: []string{"auth/**"}}
	ws := map[string][]string{
		"agent/a": {"auth/login.go", "shared.go"}, // triggers ack
		"agent/b": {"shared.go"},                  // overlaps a on shared.go => same group
		"agent/c": {"lonely.go"},                  // disjoint => cleared
	}
	clear, held := policyHoldback(pol, []string{"agent/a", "agent/b", "agent/c"}, ws, nil)
	if !reflect.DeepEqual(clear, []string{"agent/c"}) {
		t.Fatalf("clear=%v, want [agent/c]", clear)
	}
	heldBranches := map[string]string{}
	for _, h := range held {
		heldBranches[h.Branch] = h.Reason
	}
	if len(heldBranches) != 2 || heldBranches["agent/a"] == "" || heldBranches["agent/b"] == "" {
		t.Fatalf("held=%+v, want agent/a and agent/b both held with reasons", held)
	}
}

// TestPolicyHoldbackAbsentClearsAll: with no policy present, nothing is held —
// every ok branch is cleared, so a no-policy run integrates byte-identically.
func TestPolicyHoldbackAbsentClearsAll(t *testing.T) {
	ws := map[string][]string{"agent/a": {policyFileName}, "agent/b": {"auth/x"}}
	clear, held := policyHoldback(policy{}, []string{"agent/a", "agent/b"}, ws, nil)
	if len(held) != 0 || len(clear) != 2 {
		t.Fatalf("absent policy: clear=%v held=%v, want all cleared, none held", clear, held)
	}
}
