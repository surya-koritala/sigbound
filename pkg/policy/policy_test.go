package policy_test

import (
	"strings"
	"testing"
	"time"

	"github.com/surya-koritala/sigbound/pkg/policy"
)

// package policy_test on purpose: only the exported surface, exactly as a
// hosted push guard would use it. If a caller outside this repository cannot
// evaluate a push with what is imported here, neither can this test.
//
// The engine's own suite already covers these rules through `sig`. What this
// file proves is different and is the reason the package exists: the rules are
// reachable WITHOUT the engine — no git, no worktree, no run directory, no
// subprocess. A service that has just received a push has the bytes and nothing
// else.

const realPolicy = `# the repo's landing bar
verify = go build ./...
verify = go test ./...
lanes = strict
semantic = go
ack-paths = cmd/sig/policy.go, cell/**
repair-deny = **/*_test.go, .github/workflows/**
audit-sample = 25%
ack-timeout = 72h
max-agents = 64
`

func mustParse(t *testing.T, src string) policy.Policy {
	t.Helper()
	pol, err := policy.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return pol
}

func TestParseARealPolicy(t *testing.T) {
	pol := mustParse(t, realPolicy)
	if !pol.Present {
		t.Fatal("a parsed policy reports Present false")
	}
	if len(pol.Verify) != 2 || pol.Verify[0] != "go build ./..." {
		t.Fatalf("verify=%q, want both members in file order", pol.Verify)
	}
	if pol.Lanes != policy.LaneStrict || pol.Semantic != policy.SemanticGo {
		t.Fatalf("lanes=%q semantic=%q", pol.Lanes, pol.Semantic)
	}
	if pol.AuditSample != 25 || pol.MaxAgents != 64 {
		t.Fatalf("auditSample=%d maxAgents=%d", pol.AuditSample, pol.MaxAgents)
	}
	if pol.Hash == "" {
		t.Fatal("no hash: a landing cannot be checked against the exact text that governed it")
	}
	// `ack-timeout = 72h` alone is complete — the action defaults rather than
	// leaving an expired park's fate implicit.
	if pol.AckTimeoutAction != policy.ParkActionReject {
		t.Fatalf("ackTimeoutAction=%q, want %q by default", pol.AckTimeoutAction, policy.ParkActionReject)
	}
}

// TestAbsentPolicyIsNotAnEmptyPolicy. The zero value means "no policy", which is
// the no-migration default — not "a policy that requires nothing". A gate that
// confused the two would treat every repo without a file as having opted in to
// an empty bar.
func TestAbsentPolicyIsNotAnEmptyPolicy(t *testing.T) {
	var none policy.Policy
	if none.Present {
		t.Fatal("the zero policy reports Present")
	}
	if h := policy.HoldReason([]string{"anything.go"}, none, false); h.Kind != "" {
		t.Fatalf("an absent policy held a path: %+v", h)
	}
	// But the self-protection floor still binds a repair, with or without a file.
	if h := policy.RepairRefusal([]string{policy.FileName}, none); h.Kind != policy.RepairRefusedPolicyFile {
		t.Fatalf("a fixer was allowed to write %s in a repo with no policy: %+v", policy.FileName, h)
	}
}

// TestFailsClosed: every way a policy file can be wrong is an ERROR, never a
// silently weaker bar. A typo that parsed as "no opinion" is the exact failure
// this refuses — the repo would think it declared something it did not.
func TestFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"unknown key", "lanez = strict\n", `unknown policy key "lanez"`},
		{"duplicate scalar", "lanes = strict\nlanes = off\n", `duplicate key "lanes"`},
		{"bad lanes value", "lanes = warn\n", "lanes must be strict|off"},
		{"bad semantic value", "semantic = rust\n", "semantic must be go|off"},
		{"bad assert value", "assert = maybe\n", "assert must be true|false"},
		{"audit out of range", "audit-sample = 200\n", "0..100"},
		{"empty verify", "verify =\n", "verify requires a command"},
		{"empty glob list", "ack-paths =\n", "requires at least one glob"},
		{"negative duration", "budget = -5m\n", "budget must be"},
		{"unknown timeout action", "ack-timeout-action = park\n", "must be reject"},
		{"not KEY=VALUE", "lanes strict\n", "expected KEY=VALUE"},
		{"empty key", "= strict\n", "empty key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := policy.Parse([]byte(tc.src))
			if err == nil {
				t.Fatal("accepted; a malformed policy must never resolve to a weaker bar")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestHoldReasonPrecedence: policy-modified outranks unland-paths outranks
// ack-paths. A change cannot loosen the bar that gates it, so self-protection
// wins whatever else also matched.
func TestHoldReasonPrecedence(t *testing.T) {
	pol := mustParse(t, "ack-paths = cmd/**\nunland-paths = docs/**\n")

	if h := policy.HoldReason([]string{"cmd/x.go", policy.FileName}, pol, false); h.Kind != policy.ReasonPolicyModified {
		t.Fatalf("kind=%q, want %q", h.Kind, policy.ReasonPolicyModified)
	}
	if h := policy.HoldReason([]string{"cmd/x.go", "docs/y.md"}, pol, true); h.Kind != policy.ReasonUnlandPaths {
		t.Fatalf("kind=%q, want %q on an unland", h.Kind, policy.ReasonUnlandPaths)
	}
	// docs/** binds an unland only. A forward landing sees ack-paths alone.
	if h := policy.HoldReason([]string{"docs/y.md"}, pol, false); h.Kind != "" {
		t.Fatalf("a forward landing was held by unland-paths: %+v", h)
	}
	h := policy.HoldReason([]string{"cmd/x.go"}, pol, false)
	if h.Kind != policy.ReasonAckPaths {
		t.Fatalf("kind=%q", h.Kind)
	}
	if h.Matched["cmd/x.go"] != "cmd/**" {
		t.Fatalf("matched=%v, want the triggering path mapped to its glob", h.Matched)
	}
}

// TestRepairRefusalIsStricterThanAHold. The fixer is the one agent with no
// brief: handed a failure and rewarded for making it stop, which makes weakening
// whatever judged it the shortest path to green.
func TestRepairRefusalIsStricterThanAHold(t *testing.T) {
	pol := mustParse(t, realPolicy)

	for _, tc := range []struct{ name, path, want string }{
		{"the policy file itself", policy.FileName, policy.RepairRefusedPolicyFile},
		// internal/** is in no ack-paths glob, so repair-deny is provably what
		// fired here rather than the ack-paths leg standing in for it.
		{"a repair-deny glob", "internal/gitx/gitx_test.go", policy.RepairRefusedDeny},
		{"an ack-path", "cmd/sig/policy.go", policy.RepairRefusedAckPaths},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if h := policy.RepairRefusal([]string{tc.path}, pol); h.Kind != tc.want {
				t.Fatalf("kind=%q, want %q", h.Kind, tc.want)
			}
		})
	}
	// Ordinary source in no glob at all: the fixer may fix it, which is the
	// feature. A rule tightened until repair never works would pass every
	// assertion above and destroy the thing they are guarding.
	if h := policy.RepairRefusal([]string{"internal/gitx/gitx.go", "README.md"}, pol); h.Kind != "" {
		t.Fatalf("an ordinary source edit was refused: %+v", h)
	}
	// The distinction that justifies repair-deny existing as its own key: an
	// ordinary AGENT may still write a test the fixer may not. internal/** is in
	// no ack-paths glob, so nothing else can be producing this answer.
	if h := policy.HoldReason([]string{"internal/gitx/gitx_test.go"}, pol, false); h.Kind != "" {
		t.Fatalf("repair-deny held an ordinary AGENT landing (%+v); it must bind the fixer only", h)
	}
}

// TestGlobMatch pins the semantics an outside caller writes patterns against.
func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},            // * stays within one segment
		{"**/*.go", "cmd/sig/main.go", true},      // ** crosses them
		{"**/secrets.yaml", "secrets.yaml", true}, // **/ also matches zero segments
		{"**/secrets.yaml", "deploy/prod/secrets.yaml", true},
		{"cmd/**", "cmd/sig/run.go", true},
		{"cmd/**", "cell/occ.go", false},
		{"?.go", "a.go", true},
		{"?.go", "ab.go", false},
		{"a/?/c", "a/b/c", true},
		{"a/?/c", "a//c", false},
		{"***", "a/b/c", true}, // consecutive stars collapse
		{"exact.txt", "exact.txt", true},
		{"exact.txt", "exact.txtx", false},
	} {
		if got := policy.GlobMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("GlobMatch(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// TestAdversarialGlobTerminates: patterns come from a repo file an AGENT may
// have written, so a pathological one must not hang the gate evaluating it.
// Memoization is what bounds this; without it the call is exponential.
func TestAdversarialGlobTerminates(t *testing.T) {
	pattern := strings.Repeat("**/", 40) + "x"
	name := strings.Repeat("a/", 40) + "y"
	done := make(chan bool, 1)
	go func() { done <- policy.GlobMatch(pattern, name) }()
	select {
	case got := <-done:
		if got {
			t.Fatal("the adversarial pattern matched a name it should not")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GlobMatch did not terminate promptly on an adversarial pattern; a policy file could hang every push")
	}
}
