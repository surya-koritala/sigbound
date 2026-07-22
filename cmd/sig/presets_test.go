package main

import (
	"bytes"
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
			wantNames: []string{"go", "node", "python", "rust"},
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
