// Named presets for -agent/-repair/-planner/-verify (issue #17): a known-good
// sh -c command for a harness/ecosystem, keyed by a short name, so wiring up
// the SIGBOUND_* env by hand (see examples/README.md and docs/USAGE.md) is
// optional rather than the only way in. A preset encodes only the harness's
// CLI shape (how to invoke it non-interactively), never the model — bring
// your own model is unaffected. There is deliberately no -resolver-preset:
// the built-in union-resolver example in the README is repo-specific, not a
// generic wiring, so it stays out of scope here.
package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// agentPresets are -agent's known-good sh -c wiring, selected by
// -agent-preset=NAME. Mirrors the `claude` wiring docs/USAGE.md and
// examples/README.md already hand-write; codex/aider get the same shape
// with their own non-interactive/auto-apply flags.
var agentPresets = map[string]string{
	"claude": `claude -p --permission-mode acceptEdits "$SIGBOUND_TASK"`,
	"codex":  `codex exec --full-auto "$SIGBOUND_TASK"`,
	"aider":  `aider --yes --message "$SIGBOUND_TASK"`,
}

// repairPresets are -repair's known-good wiring, selected by
// -repair-preset=NAME. Same shape as agentPresets, wrapping SIGBOUND_FAILURE
// in the same "fix this build failure" framing the README's -repair example
// uses.
var repairPresets = map[string]string{
	"claude": `claude -p --permission-mode acceptEdits "Fix this build failure: $SIGBOUND_FAILURE"`,
	"codex":  `codex exec --full-auto "Fix this build failure: $SIGBOUND_FAILURE"`,
	"aider":  `aider --yes --message "Fix this build failure: $SIGBOUND_FAILURE"`,
}

// plannerPresets are -planner's known-good wiring, selected by
// -planner-preset=NAME. The planner only needs to print the plan JSON to
// stdout (see DefaultPlanPrompt) — it never edits files — so these use each
// harness's plain non-interactive print mode rather than its auto-apply-edits
// flags (contrast agentPresets' --full-auto / --permission-mode acceptEdits).
var plannerPresets = map[string]string{
	"claude": `claude -p "$SIGBOUND_PROMPT"`,
	"codex":  `codex exec "$SIGBOUND_PROMPT"`,
	"aider":  `aider --yes --message "$SIGBOUND_PROMPT"`,
}

// verifyPresets are -verify's known-good command per ecosystem or scanner,
// selected by -verify-preset=NAME. Go's build+test is backed by RepoMap's
// exported-decl scan (plan.go); RepoMap has no equivalent detection for
// node/python/rust — those three are just the idiomatic one-liner for that
// ecosystem.
//
// The security names (govulncheck/gitleaks/codeql) run a scanner INSTEAD of a
// build+test, so they are meant to COMPOSE with one rather than replace it: put
// the build+test members in the repo's sigbound.policy and pass the scanner as
// -verify-preset, and resolvePolicy runs the policy's members first and appends
// the scanner last. Nothing here changes that ordering — a preset is an ordinary
// flag verify by the time policy resolution sees it.
var verifyPresets = map[string]string{
	"go":     "go build ./... && go test ./...",
	"node":   "npm test",
	"python": "python -m pytest",
	"rust":   "cargo build && cargo test",

	"govulncheck": securityVerify("govulncheck", "go install golang.org/x/vuln/cmd/govulncheck@latest", "govulncheck ./..."),
	"gitleaks":    securityVerify("gitleaks", "brew install gitleaks, or a release binary from https://github.com/gitleaks/gitleaks", gitleaksScan),
	"codeql":      securityVerify("codeql", "download the CodeQL CLI from https://github.com/github/codeql-cli-binaries/releases", codeqlScan),
}

// gitleaksScan scans the WORKING TREE of the verify checkout (--no-git), not the
// repo's history: the gate decides what this landing may put on the base branch,
// and a history scan would instead fail every run for as long as any ancestor
// commit contains a secret — a gate nobody can turn green is one that gets
// switched off. --redact keeps the matched value out of stdout, which sigbound
// captures into the run report — and from there into the manifest, any git note
// -notes attaches, and what `sig log` replays — plus -logdir's full log; a gate
// that copies the secret it found into all of those is its own incident.
// gitleaks exits non-zero on any finding, and that is what fails the gate.
const gitleaksScan = "gitleaks detect --no-git --redact"

// codeqlScan builds a database from the verify checkout and analyzes it with the
// CLI's default query suite for the language. Two parts are deliberate:
//
//   - the language, which CodeQL cannot infer for us. It is SIGBOUND_CODEQL_LANG,
//     defaulting to go. LIMIT: under -env-mode scoped that variable reaches the
//     verify command only if -env-verify allowlists it — unset, this scans go.
//   - the results check. `codeql database analyze` exits 0 whether or not it
//     found anything, so WITHOUT the CSV test this member would report green over
//     real findings. An analyze that wrote no file at all also fails ([ -f ]),
//     rather than passing as "nothing found" — the file's absence means the scan
//     did not happen, which is exactly the outcome that must never read as green.
//
// The database is built inside the checkout: runVerify runs `git clean -fdx`
// before every verify attempt, so it never survives into another one, and the
// checkout is a throwaway worktree that is never what lands.
const codeqlScan = `codeql database create .sigbound-codeql-db --language="${SIGBOUND_CODEQL_LANG:-go}" --overwrite && ` +
	`codeql database analyze .sigbound-codeql-db --format=csv --output=.sigbound-codeql.csv && ` +
	`[ -f .sigbound-codeql.csv ] && [ ! -s .sigbound-codeql.csv ]`

// securityVerify composes one scanner preset: a PATH check for the tool, then
// the scan. The check is there for the FAILURE MODE, which in a landing gate
// matters more than the scan itself — an ABSENT tool must FAIL the gate, never
// skip it. A member that skipped would let the run report green over a tree
// nothing scanned, and a receipt claiming a scan that never ran is worse than
// having no scanner at all. So this must never be written as
// `command -v TOOL || exit 0`, nor as a scan suffixed with `|| true`.
//
// HONEST LIMIT on what the check itself buys: `sh -c` already fails a missing
// command with exit 127, so the prefix does not change the VERDICT — absence
// failed the gate before it existed. What it adds is the MESSAGE: the tool's
// name and how to install it, in place of the shell's bare "not found", which
// tells a user staring at a red gate nothing about which of an N-member battery
// went missing or what to do next.
func securityVerify(tool, install, scan string) string {
	return fmt.Sprintf(`command -v %s >/dev/null 2>&1 || { echo "sigbound -verify-preset=%s: %s is not on PATH (install: %s) -- failing the gate rather than skipping the scan and reporting green" >&2; exit 1; }; %s`,
		tool, tool, tool, install, scan)
}

// presetSlot resolves one -X / -X-preset flag pair. A raw cmd, when already
// set, always wins — the preset name isn't even looked up, so a stray or
// misspelled -X-preset never breaks a run that already supplies its own -X
// (documented in each -X-preset flag's usage text). Otherwise an empty
// preset is a no-op (both left "", e.g. optional -repair-preset), and a set
// preset resolves via table or fails loudly listing every valid name. A
// newly-expanded command is announced once to stderrW — cmdFlag/presetFlag
// name the flags in that message — so the user can see and copy exactly what
// will run.
func presetSlot(stderrW io.Writer, table map[string]string, cmd, preset, cmdFlag, presetFlag string) (string, error) {
	if strings.TrimSpace(cmd) != "" {
		return cmd, nil
	}
	if strings.TrimSpace(preset) == "" {
		return cmd, nil
	}
	expanded, ok := table[preset]
	if !ok {
		names := make([]string, 0, len(table))
		for n := range table {
			names = append(names, n)
		}
		sort.Strings(names)
		return "", fmt.Errorf("%s: unknown preset %q (want one of: %s)", presetFlag, preset, strings.Join(names, ", "))
	}
	fmt.Fprintf(stderrW, "%s=%s expands %s to: %s\n", presetFlag, preset, cmdFlag, expanded)
	return expanded, nil
}

// applyPresets expands every -*-preset flag into its known-good command,
// raw-wins per presetSlot, BEFORE the rest of runRun consumes the resulting
// agent/repair/planner/verify commands — so e.g. -agent-preset alone (no
// -agent) satisfies runRun's "-agent is required" check exactly like a
// hand-written -agent would. stderrW is where each expansion is announced
// (os.Stderr in production; a buffer in tests). Returns the resolved
// agent/repair/planner/verify commands, or the first unknown-preset error
// encountered (agent, then repair, then planner, then verify).
func applyPresets(stderrW io.Writer, agentCmd, agentPreset, repairCmd, repairPreset, plannerCmd, plannerPreset, verifyCmd, verifyPreset string) (newAgentCmd, newRepairCmd, newPlannerCmd, newVerifyCmd string, err error) {
	if newAgentCmd, err = presetSlot(stderrW, agentPresets, agentCmd, agentPreset, "-agent", "-agent-preset"); err != nil {
		return "", "", "", "", err
	}
	if newRepairCmd, err = presetSlot(stderrW, repairPresets, repairCmd, repairPreset, "-repair", "-repair-preset"); err != nil {
		return "", "", "", "", err
	}
	if newPlannerCmd, err = presetSlot(stderrW, plannerPresets, plannerCmd, plannerPreset, "-planner", "-planner-preset"); err != nil {
		return "", "", "", "", err
	}
	if newVerifyCmd, err = presetSlot(stderrW, verifyPresets, verifyCmd, verifyPreset, "-verify", "-verify-preset"); err != nil {
		return "", "", "", "", err
	}
	return newAgentCmd, newRepairCmd, newPlannerCmd, newVerifyCmd, nil
}
