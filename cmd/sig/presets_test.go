package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---- applyPresets: pure unit tests (the honest seam — no subprocess, no
// runRun flag parsing, just the resolved fs-values in and the resolved
// commands out) ----

// TestApplyPresetsExpandsExactTableStrings pins every preset name's expansion
// to the EXACT string in the {agent,repair,planner,verify}Presets tables —
// docs/USAGE.md's Presets section documents these same strings, so this test
// is what keeps the docs honest.
func TestApplyPresetsExpandsExactTableStrings(t *testing.T) {
	var buf bytes.Buffer
	for name, want := range agentPresets {
		agent, _, _, _, err := applyPresets(&buf, "", name, "", "", "", "", "", "")
		if err != nil {
			t.Fatalf("agent preset %q: %v", name, err)
		}
		if agent != want {
			t.Fatalf("agent preset %q = %q, want %q", name, agent, want)
		}
	}
	for name, want := range repairPresets {
		_, repair, _, _, err := applyPresets(&buf, "", "", "", name, "", "", "", "")
		if err != nil {
			t.Fatalf("repair preset %q: %v", name, err)
		}
		if repair != want {
			t.Fatalf("repair preset %q = %q, want %q", name, repair, want)
		}
	}
	for name, want := range plannerPresets {
		_, _, planner, _, err := applyPresets(&buf, "", "", "", "", "", name, "", "")
		if err != nil {
			t.Fatalf("planner preset %q: %v", name, err)
		}
		if planner != want {
			t.Fatalf("planner preset %q = %q, want %q", name, planner, want)
		}
	}
	for name, want := range verifyPresets {
		_, _, _, verify, err := applyPresets(&buf, "", "", "", "", "", "", "", name)
		if err != nil {
			t.Fatalf("verify preset %q: %v", name, err)
		}
		if verify != want {
			t.Fatalf("verify preset %q = %q, want %q", name, verify, want)
		}
	}
}

// TestApplyPresetsAnnouncesExpansionToStderr: every expansion is printed once
// to the given writer (production wires this to os.Stderr) so a user can see
// and copy exactly what will run.
func TestApplyPresetsAnnouncesExpansionToStderr(t *testing.T) {
	var buf bytes.Buffer
	if _, _, _, _, err := applyPresets(&buf, "", "claude", "", "", "", "", "", ""); err != nil {
		t.Fatalf("applyPresets: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "-agent-preset=claude") || !strings.Contains(got, agentPresets["claude"]) {
		t.Fatalf("stderr announcement = %q, want it to name -agent-preset=claude and the expanded command", got)
	}
}

// TestApplyPresetsRawOverridesPreset: an already-set raw command always wins
// over its preset — even a bogus/unknown preset name never surfaces as an
// error, since it's never even looked up once the raw flag is set (raw wins,
// per issue #17's design).
func TestApplyPresetsRawOverridesPreset(t *testing.T) {
	var buf bytes.Buffer
	rawAgent := "./my-own-agent.sh"
	rawRepair := "./my-own-repair.sh"
	rawPlanner := "./my-own-planner.sh"
	rawVerify := "make check"
	agent, repair, planner, verify, err := applyPresets(&buf,
		rawAgent, "not-a-real-preset",
		rawRepair, "not-a-real-preset",
		rawPlanner, "not-a-real-preset",
		rawVerify, "not-a-real-preset")
	if err != nil {
		t.Fatalf("applyPresets: %v (raw flags should have short-circuited every preset lookup)", err)
	}
	if agent != rawAgent || repair != rawRepair || planner != rawPlanner || verify != rawVerify {
		t.Fatalf("got agent=%q repair=%q planner=%q verify=%q, want the raw commands unchanged", agent, repair, planner, verify)
	}
	if buf.Len() != 0 {
		t.Fatalf("stderr = %q, want no announcement when raw wins (nothing was expanded)", buf.String())
	}
}

// TestApplyPresetsUnknownNameErrorsListingValidNames: an unknown preset name
// (with no raw command to override it) is a loud error naming every valid
// name for that slot, not a silent no-op or a generic parse failure.
func TestApplyPresetsUnknownNameErrorsListingValidNames(t *testing.T) {
	cases := []struct {
		name      string
		call      func() (string, string, string, string, error)
		wantFlag  string
		wantNames []string
	}{
		{
			name: "agent",
			call: func() (string, string, string, string, error) {
				return applyPresets(&bytes.Buffer{}, "", "bogus", "", "", "", "", "", "")
			},
			wantFlag:  "-agent-preset",
			wantNames: []string{"claude", "codex", "aider"},
		},
		{
			name: "repair",
			call: func() (string, string, string, string, error) {
				return applyPresets(&bytes.Buffer{}, "", "", "", "bogus", "", "", "", "")
			},
			wantFlag:  "-repair-preset",
			wantNames: []string{"claude", "codex", "aider"},
		},
		{
			name: "planner",
			call: func() (string, string, string, string, error) {
				return applyPresets(&bytes.Buffer{}, "", "", "", "", "", "bogus", "", "")
			},
			wantFlag:  "-planner-preset",
			wantNames: []string{"claude", "codex", "aider"},
		},
		{
			name: "verify",
			call: func() (string, string, string, string, error) {
				return applyPresets(&bytes.Buffer{}, "", "", "", "", "", "", "", "bogus")
			},
			wantFlag:  "-verify-preset",
			wantNames: []string{"go", "node", "python", "rust", "govulncheck", "gitleaks", "codeql"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, _, err := c.call()
			if err == nil {
				t.Fatalf("%s: want an error for an unknown preset name, got nil", c.name)
			}
			got := err.Error()
			if !strings.Contains(got, c.wantFlag) || !strings.Contains(got, `"bogus"`) {
				t.Fatalf("%s: error=%q, want it to name %s and the bad value", c.name, got, c.wantFlag)
			}
			for _, n := range c.wantNames {
				if !strings.Contains(got, n) {
					t.Fatalf("%s: error=%q, want it to list valid name %q", c.name, got, n)
				}
			}
		})
	}
}

// ---- security verify presets (govulncheck/gitleaks/codeql) ----
//
// These run the REAL expansion through `sh -c`, the way runVerify does, with
// PATH pointing only at a directory the test controls — so "the tool is
// missing" and "the tool is installed" are both constructed conditions, and no
// scanner has to exist on the machine running the suite.

// securityPresetCases pins, per preset, the tool name and the install hint its
// absent-tool failure must name. Duplicated from the table on purpose: this is
// the message a user reads off a red gate, so the test states it independently.
var securityPresetCases = []struct{ preset, tool, install string }{
	{"govulncheck", "govulncheck", "go install golang.org/x/vuln/cmd/govulncheck@latest"},
	{"gitleaks", "gitleaks", "brew install gitleaks"},
	{"codeql", "codeql", "https://github.com/github/codeql-cli-binaries/releases"},
}

// runVerifyPreset executes a -verify-preset expansion exactly as runVerify
// does — `sh -c <expansion>`, cwd = the verify checkout — with PATH replaced by
// pathDir alone, so the test decides what is installed. Returns the combined
// output and the exit error (nil == the gate would pass).
func runVerifyPreset(t *testing.T, preset, dir, pathDir string, env ...string) (string, error) {
	t.Helper()
	expansion, ok := verifyPresets[preset]
	if !ok {
		t.Fatalf("no verify preset %q", preset)
	}
	cmd := exec.Command("sh", "-c", expansion)
	cmd.Dir = dir
	// PATH last: os/exec keeps the LAST value for a duplicated key, so this
	// overrides the inherited PATH rather than racing it.
	cmd.Env = append(append(os.Environ(), env...), "PATH="+pathDir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// installFakeTool puts an executable named tool on the test's PATH dir. It
// succeeds at everything; the one behavior it models is codeql's `database
// analyze` step, which writes $FAKE_CODEQL_RESULTS into the CSV codeqlScan
// inspects — or, with $FAKE_CODEQL_NO_RESULTS_FILE set, writes nothing at all
// while still exiting 0. That is how "the scanner ran and found something" and
// "the scanner reported success without producing results" get exercised
// without a CodeQL install.
func installFakeTool(t *testing.T, pathDir, tool string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = database ] && [ \"$2\" = analyze ] && [ -z \"$FAKE_CODEQL_NO_RESULTS_FILE\" ]; then printf '%s' \"$FAKE_CODEQL_RESULTS\" > .sigbound-codeql.csv; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(pathDir, tool), []byte(script), 0o755); err != nil {
		t.Fatalf("install fake %s: %v", tool, err)
	}
}

// TestSecurityVerifyPresetsFailLoudlyWhenToolAbsent is the whole point of these
// presets: a scanner that isn't installed must FAIL the gate, naming the tool
// and how to install it. The failure mode that must never exist is the quiet
// one — skipping the scan and letting the run report green over a tree nothing
// looked at, which is worse than declaring no scanner at all because the
// receipt then claims a scan that never ran.
func TestSecurityVerifyPresetsFailLoudlyWhenToolAbsent(t *testing.T) {
	requirePOSIXShell(t)
	for _, c := range securityPresetCases {
		t.Run(c.preset, func(t *testing.T) {
			out, err := runVerifyPreset(t, c.preset, t.TempDir(), t.TempDir())
			if err == nil {
				t.Fatalf("-verify-preset=%s exited 0 with %s absent — a skipped scan reports GREEN over an unscanned tree\n%s", c.preset, c.tool, out)
			}
			if !strings.Contains(out, c.tool) {
				t.Fatalf("-verify-preset=%s absent-tool output %q does not name %q", c.preset, out, c.tool)
			}
			if !strings.Contains(out, c.install) {
				t.Fatalf("-verify-preset=%s absent-tool output %q does not say how to install it (want %q)", c.preset, out, c.install)
			}
		})
	}
}

// TestSecurityVerifyPresetsPassWhenToolIsPresentAndClean is the control for the
// test above: with the tool on PATH and nothing to report, the same expansion
// exits 0. Without this, an expansion that failed for any reason at all (a typo
// in the command, a guard that always fires) would still satisfy the
// absent-tool test, which only ever asserts failure.
func TestSecurityVerifyPresetsPassWhenToolIsPresentAndClean(t *testing.T) {
	requirePOSIXShell(t)
	for _, c := range securityPresetCases {
		t.Run(c.preset, func(t *testing.T) {
			pathDir := t.TempDir()
			installFakeTool(t, pathDir, c.tool)
			out, err := runVerifyPreset(t, c.preset, t.TempDir(), pathDir)
			if err != nil {
				t.Fatalf("-verify-preset=%s with %s installed and clean: %v\n%s", c.preset, c.tool, err, out)
			}
		})
	}
}

// TestCodeqlPresetFailsWhenAnalyzeIsNotClean pins the one scanner whose own exit
// code is NOT the verdict: `codeql database analyze` exits 0 whether it found
// results, found nothing, or wrote no file at all. The preset's CSV test is the
// whole verdict, in both directions it has to catch.
func TestCodeqlPresetFailsWhenAnalyzeIsNotClean(t *testing.T) {
	requirePOSIXShell(t)
	cases := []struct{ name, env, why string }{
		{"findings", "FAKE_CODEQL_RESULTS=rule,a finding,src/main.go,1,1", "a row in the results file is a finding; exiting 0 would land on it"},
		{"no results file", "FAKE_CODEQL_NO_RESULTS_FILE=1", "no results file means the scan did not happen, and that must never read as \"found nothing\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pathDir := t.TempDir()
			installFakeTool(t, pathDir, "codeql")
			out, err := runVerifyPreset(t, "codeql", t.TempDir(), pathDir, c.env)
			if err == nil {
				t.Fatalf("-verify-preset=codeql exited 0: %s\n%s", c.why, out)
			}
		})
	}
}

// TestSecurityPresetComposesAfterPolicyBattery: a scanner preset is an ORDINARY
// flag verify by the time policy resolution sees it, so resolvePolicy appends it
// AFTER the repo's own battery members, which keep file order. Ordering is the
// contract being pinned — the repo's build+test runs first, the invoker's
// scanner last — not merely that all three appear.
func TestSecurityPresetComposesAfterPolicyBattery(t *testing.T) {
	var buf bytes.Buffer
	_, _, _, verify, err := applyPresets(&buf, "", "", "", "", "", "", "", "govulncheck")
	if err != nil {
		t.Fatalf("applyPresets: %v", err)
	}
	pol := policy{present: true, verify: []string{"go build ./...", "go test ./..."}}
	p := runParams{VerifyCmd: verify}
	if err := resolvePolicy(pol, &p, 1); err != nil {
		t.Fatal(err)
	}
	build := strings.Index(p.VerifyCmd, "go build ./...")
	test := strings.Index(p.VerifyCmd, "go test ./...")
	scan := strings.Index(p.VerifyCmd, "govulncheck ./...")
	if build < 0 || test < 0 || scan < 0 {
		t.Fatalf("effective verify %q: want the policy battery and the scanner preset all present", p.VerifyCmd)
	}
	if !(build < test && test < scan) {
		t.Fatalf("effective verify %q: want policy members first (in file order) then the scanner appended", p.VerifyCmd)
	}
}

// TestVerifyPresetNamesAreDocumented holds docs/USAGE.md's Presets section to
// the table. A preset name is a public surface reached by typing it: one that
// exists but is written down nowhere is only findable by reading the source or
// by mistyping a name and reading the error.
func TestVerifyPresetNamesAreDocumented(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "USAGE.md"))
	if err != nil {
		t.Fatalf("read USAGE.md: %v", err)
	}
	var section []string
	in, depth := false, 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "#") {
			// The section has its own #### subsections, so it ends at the next
			// heading at or ABOVE its own level, not at the next heading at all.
			level := len(line) - len(strings.TrimLeft(line, "#"))
			if title := strings.TrimSpace(line[level:]); title == "Presets" {
				in, depth = true, level
			} else if in && level <= depth {
				in = false
			}
			continue
		}
		if in {
			section = append(section, line)
		}
	}
	if len(section) == 0 {
		t.Fatal("docs/USAGE.md has no Presets section — this test went blind, it did not find the docs honest")
	}
	body := strings.Join(section, "\n")
	for name := range verifyPresets {
		if !strings.Contains(body, "`"+name+"`") {
			t.Errorf("-verify-preset %q is not named in docs/USAGE.md's Presets section", name)
		}
	}
}

// TestApplyPresetsEmptyIsNoOp: neither a raw command nor a preset set is not
// an error — every slot but -agent is optional, and applyPresets must never
// invent a requirement runRun itself doesn't have.
func TestApplyPresetsEmptyIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	agent, repair, planner, verify, err := applyPresets(&buf, "", "", "", "", "", "", "", "")
	if err != nil {
		t.Fatalf("applyPresets: %v", err)
	}
	if agent != "" || repair != "" || planner != "" || verify != "" {
		t.Fatalf("got agent=%q repair=%q planner=%q verify=%q, want all empty", agent, repair, planner, verify)
	}
}
