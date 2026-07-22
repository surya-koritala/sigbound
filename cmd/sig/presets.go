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

// verifyPresets are -verify's known-good build+test command per ecosystem,
// selected by -verify-preset=NAME. Go's is backed by RepoMap's exported-decl
// scan (plan.go); RepoMap has no equivalent detection for node/python/rust —
// those three are just the idiomatic one-liner for that ecosystem.
var verifyPresets = map[string]string{
	"go":     "go build ./... && go test ./...",
	"node":   "npm test",
	"python": "python -m pytest",
	"rust":   "cargo build && cargo test",
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
