// -env-mode (issue #56): scope what environment a slot's external command
// sees instead of always handing it the whole parent process's environment.
// On a laptop, run by the user in their own shell, inheriting everything is
// harmless -- it's their shell either way. In a hosted setting, driving many
// tenants' commands from one process, the full parent env is a cross-tenant
// leak by construction: every BYO shell command (agent/resolver/verify/
// repair/planner/publish) sees every secret sigbound itself has, not just
// the ones that command needs. -env-mode scoped switches every slot to a
// minimal base environment plus that slot's own -env-* allowlist. -env-mode
// inherit (the default) is today's behavior, byte-identical.
package main

import (
	"fmt"
	"os"
	"strings"
)

const (
	envModeInherit = "inherit"
	envModeScoped  = "scoped"
)

// validateEnvMode rejects any -env-mode value that is not a known mode.
func validateEnvMode(m string) error {
	switch m {
	case envModeInherit, envModeScoped:
		return nil
	default:
		return fmt.Errorf("unknown -env-mode %q (want inherit|scoped)", m)
	}
}

// validateEnvAllow rejects a bare "*" entry (or, equivalently, any entry
// whose glob prefix is empty once the trailing "*" is cut) in a slot's
// -env-* allowlist. permitted() below now treats an empty prefix as
// matching nothing, so a bare "*" is inert rather than a leak -- but an
// allowlist that silently does nothing is its own bug: whoever wrote
// "-env-agent '*'" meant "pass everything" and got "pass nothing" instead,
// with no error either way. Reject it loudly here, before any command runs,
// naming the flag it came from so the fix is obvious.
func validateEnvAllow(flag string, allow []string) error {
	for _, a := range allow {
		if rest, ok := strings.CutSuffix(a, "*"); ok && rest == "" {
			return fmt.Errorf("%s: bare '*' would pass the entire environment; list variable names or NAME_* families", flag)
		}
	}
	return nil
}

// baseEnvNames are the fixed variable names a scoped slot's command gets
// regardless of that slot's -env-* allowlist: the minimum needed to find an
// interpreter, resolve paths, and behave sanely (home dir, locale,
// terminal). Only names actually SET in the parent process's environment are
// passed through -- this list is a ceiling, not a guarantee any of them exist.
var baseEnvNames = []string{"PATH", "HOME", "USER", "SHELL", "TMPDIR", "LANG", "TERM"}

// baseEnvPrefixes are glob-style name prefixes carried alongside
// baseEnvNames: LC_* (the rest of the locale family: LC_ALL, LC_CTYPE, ...)
// and GIT_* (git itself reads a family of GIT_*-prefixed overrides; blocking
// all of them would break ordinary git usage inside an agent/verify/repair
// command, which is not what least-privilege scoping is for).
var baseEnvPrefixes = []string{"LC_", "GIT_"}

// slotEnv builds the environment for one command slot (agent/resolver/
// verify/repair/planner/publish) -- the ONE implementation every cmd.Env
// assignment in this package routes through, so scoping can never drift
// slot to slot.
//
// mode == envModeInherit (or any other value -- callers that never set
// EnvMode, e.g. tests driving driveRun directly, get this too): today's
// behavior, byte-identical -- the full parent environment plus sigboundVars
// appended (sigboundVars win on any name collision; see os/exec's
// "last value for a duplicate key wins" rule).
//
// mode == envModeScoped: the parent environment is NOT inherited. The
// command instead gets baseEnvNames/baseEnvPrefixes (whichever are actually
// set in the parent), plus allow -- extra variable NAMES this slot's -env-*
// flag was configured to pass through, each either an exact name or a
// NAME_* glob -- plus sigboundVars. A name in allow that isn't set in the
// parent is silently skipped: an allowlist describes what's PERMITTED, not
// what's REQUIRED.
func slotEnv(mode string, allow []string, sigboundVars []string) []string {
	if mode != envModeScoped {
		return append(os.Environ(), sigboundVars...)
	}
	permitted := func(name string) bool {
		for _, n := range baseEnvNames {
			if name == n {
				return true
			}
		}
		for _, prefix := range baseEnvPrefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		for _, a := range allow {
			if rest, ok := strings.CutSuffix(a, "*"); ok {
				// A bare "*" cuts to an empty prefix -- HasPrefix(name, "")
				// is true for every name, which would silently pass the
				// entire parent environment. An empty prefix matches
				// nothing instead; validateEnvAllow rejects a bare "*"
				// outright at flag validation, so this is a second,
				// independent guard on the same rule.
				if rest != "" && strings.HasPrefix(name, rest) {
					return true
				}
			} else if name == a {
				return true
			}
		}
		return false
	}
	var out []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if ok && permitted(name) {
			out = append(out, kv)
		}
	}
	return append(out, sigboundVars...)
}
