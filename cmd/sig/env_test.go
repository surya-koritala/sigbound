package main

import (
	"os"
	"strings"
	"testing"
)

// hasVar reports whether env (a []string of "NAME=VALUE" entries, the
// os/exec.Cmd.Env shape) contains an entry for name.
func hasVar(env []string, name string) bool {
	for _, kv := range env {
		if n, _, ok := strings.Cut(kv, "="); ok && n == name {
			return true
		}
	}
	return false
}

// TestSlotEnvInheritIsByteIdenticalToToday: -env-mode inherit (and any other
// value besides envModeScoped, e.g. the zero value "" a direct runParams{}
// carries) returns exactly append(os.Environ(), sigboundVars...) — today's
// behavior, unchanged.
func TestSlotEnvInheritIsByteIdenticalToToday(t *testing.T) {
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	sigboundVars := []string{"SIGBOUND_TASK=hello"}
	for _, mode := range []string{envModeInherit, "", "garbage"} {
		got := slotEnv(mode, nil, sigboundVars)
		want := append(os.Environ(), sigboundVars...)
		if len(got) != len(want) {
			t.Fatalf("mode %q: len=%d, want %d", mode, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("mode %q: entry %d = %q, want %q", mode, i, got[i], want[i])
			}
		}
	}
}

// TestSlotEnvScopedStripsParentVars: -env-mode scoped with no allowlist keeps
// only the fixed base names/prefixes (PATH survives) and drops everything
// else from the parent, incl. a var like SIGBOUND_TEST_CANARY that isn't in
// the base set and wasn't allowlisted.
func TestSlotEnvScopedStripsParentVars(t *testing.T) {
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	got := slotEnv(envModeScoped, nil, []string{"SIGBOUND_TASK=hello"})
	if hasVar(got, "SIGBOUND_TEST_CANARY") {
		t.Fatalf("scoped env leaked SIGBOUND_TEST_CANARY: %v", got)
	}
	if !hasVar(got, "PATH") {
		t.Fatalf("scoped env dropped PATH (base env should always survive when set): %v", got)
	}
	if !hasVar(got, "SIGBOUND_TASK") {
		t.Fatalf("scoped env dropped the slot's own SIGBOUND_TASK var: %v", got)
	}
}

// TestSlotEnvScopedAllowlistPassesExactName: an allowlisted exact name reaches
// the scoped env; an unlisted name in the same parent process still doesn't.
func TestSlotEnvScopedAllowlistPassesExactName(t *testing.T) {
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	t.Setenv("SIGBOUND_TEST_OTHER", "not-allowed")
	got := slotEnv(envModeScoped, []string{"SIGBOUND_TEST_CANARY"}, nil)
	if !hasVar(got, "SIGBOUND_TEST_CANARY") {
		t.Fatalf("allowlisted name missing: %v", got)
	}
	if hasVar(got, "SIGBOUND_TEST_OTHER") {
		t.Fatalf("non-allowlisted sibling var leaked: %v", got)
	}
}

// TestSlotEnvScopedAllowlistGlobSuffix: a NAME_* allowlist entry passes every
// parent var whose name has that prefix.
func TestSlotEnvScopedAllowlistGlobSuffix(t *testing.T) {
	t.Setenv("SIGBOUND_TEST_A", "a")
	t.Setenv("SIGBOUND_TEST_B", "b")
	t.Setenv("SIGBOUND_OTHER", "nope")
	got := slotEnv(envModeScoped, []string{"SIGBOUND_TEST_*"}, nil)
	if !hasVar(got, "SIGBOUND_TEST_A") || !hasVar(got, "SIGBOUND_TEST_B") {
		t.Fatalf("glob allowlist missed a matching var: %v", got)
	}
	if hasVar(got, "SIGBOUND_OTHER") {
		t.Fatalf("glob allowlist over-matched a non-prefixed var: %v", got)
	}
}

// TestSlotEnvScopedAllowlistUnsetNameSkipped: an allowlisted name that isn't
// actually set in the parent is silently skipped — no empty-valued entry, no
// error.
func TestSlotEnvScopedAllowlistUnsetNameSkipped(t *testing.T) {
	os.Unsetenv("SIGBOUND_TEST_DOES_NOT_EXIST")
	got := slotEnv(envModeScoped, []string{"SIGBOUND_TEST_DOES_NOT_EXIST"}, nil)
	if hasVar(got, "SIGBOUND_TEST_DOES_NOT_EXIST") {
		t.Fatalf("unset allowlisted name should be skipped, not passed as empty: %v", got)
	}
}

// TestSlotEnvScopedSigboundVarsAlwaysWin: sigboundVars are appended last, so
// they win over a same-named parent/allowlisted var (matches os/exec's "last
// value for a duplicate key wins" — see slotEnv's doc).
func TestSlotEnvScopedSigboundVarsAlwaysWin(t *testing.T) {
	t.Setenv("PATH", "/parent/path")
	got := slotEnv(envModeScoped, nil, []string{"PATH=/overridden/path"})
	count := 0
	last := ""
	for _, kv := range got {
		if strings.HasPrefix(kv, "PATH=") {
			count++
			last = kv
		}
	}
	if count == 0 {
		t.Fatal("PATH missing entirely")
	}
	if last != "PATH=/overridden/path" {
		t.Fatalf("last PATH entry = %q, want the sigboundVars override to win", last)
	}
}

// TestSlotEnvScopedBareStarMatchesNothing: a bare "*" allowlist entry must
// NOT degenerate into "pass everything" (CutSuffix("*", "*") yields an empty
// prefix, and HasPrefix(name, "") is true for every name) -- permitted()
// treats an empty prefix as matching nothing, so a bare "*" passes through
// only the fixed base env, same as no allowlist at all. This is the runtime
// half of the fix; validateEnvAllow (TestValidateEnvAllowRejectsBareStar)
// is the other half, rejecting a bare "*" outright before any command runs.
func TestSlotEnvScopedBareStarMatchesNothing(t *testing.T) {
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	got := slotEnv(envModeScoped, []string{"*"}, nil)
	if hasVar(got, "SIGBOUND_TEST_CANARY") {
		t.Fatalf("bare '*' allowlist entry leaked the full parent environment: %v", got)
	}
	if !hasVar(got, "PATH") {
		t.Fatalf("scoped env dropped PATH (base env should always survive when set): %v", got)
	}
}

func TestValidateEnvAllowRejectsBareStar(t *testing.T) {
	if err := validateEnvAllow("-env-agent", []string{"*"}); err == nil {
		t.Fatal("bare '*': want error, got nil")
	} else if !strings.Contains(err.Error(), "-env-agent") {
		t.Errorf("error %q does not name the offending flag", err)
	}
	if err := validateEnvAllow("-env-agent", []string{"ANTHROPIC_API_KEY", "ANTHROPIC_*"}); err != nil {
		t.Errorf("legitimate allowlist rejected: %v", err)
	}
	if err := validateEnvAllow("-env-agent", nil); err != nil {
		t.Errorf("empty allowlist rejected: %v", err)
	}
}

func TestValidateEnvMode(t *testing.T) {
	if err := validateEnvMode(envModeInherit); err != nil {
		t.Errorf("inherit: %v", err)
	}
	if err := validateEnvMode(envModeScoped); err != nil {
		t.Errorf("scoped: %v", err)
	}
	if err := validateEnvMode("bogus"); err == nil {
		t.Error("bogus mode: want error, got nil")
	}
}
