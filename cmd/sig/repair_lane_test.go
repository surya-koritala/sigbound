package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepairRefusalReason pins the three rules and their precedence. Each case
// asserts the rule that fired AND the paths it named — a refusal that reports
// the wrong trigger is as useless as no refusal, since the paths are what the
// report and the repair_refused event show a human.
func TestRepairRefusalReason(t *testing.T) {
	withDeny := policy{present: true, repairDeny: []string{"**/*_test.go"}}
	withAck := policy{present: true, ackPaths: []string{"cmd/sig/run.go"}}
	both := policy{
		present:    true,
		repairDeny: []string{"**/*_test.go"},
		ackPaths:   []string{"cmd/sig/run.go"},
	}

	cases := []struct {
		name      string
		paths     []string
		pol       policy
		wantKind  string
		wantPaths []string
	}{{
		// The floor: no policy file exists at base, so pol is the zero value and
		// policyHoldback would hold nothing at all. The fixer is still barred
		// from writing the bar.
		name:      "policy file barred with no policy at all",
		paths:     []string{"main.go", policyFileName},
		pol:       policy{},
		wantKind:  repairRefusedPolicyFile,
		wantPaths: []string{policyFileName},
	}, {
		name:      "repair-deny glob",
		paths:     []string{"cell/occ_test.go"},
		pol:       withDeny,
		wantKind:  repairRefusedDeny,
		wantPaths: []string{"cell/occ_test.go"},
	}, {
		// The leg that closes the real hole: policyHoldback runs on the AGENTS'
		// write-sets, before repair exists, so ack-paths would otherwise bind
		// every code path except the fixer.
		name:      "ack-paths bind the fixer too",
		paths:     []string{"cmd/sig/run.go"},
		pol:       withAck,
		wantKind:  repairRefusedAckPaths,
		wantPaths: []string{"cmd/sig/run.go"},
	}, {
		name:      "policy file outranks the glob rules",
		paths:     []string{"cell/occ_test.go", policyFileName, "cmd/sig/run.go"},
		pol:       both,
		wantKind:  repairRefusedPolicyFile,
		wantPaths: []string{policyFileName},
	}, {
		name:      "repair-deny outranks ack-paths",
		paths:     []string{"cmd/sig/run.go", "cell/occ_test.go"},
		pol:       both,
		wantKind:  repairRefusedDeny,
		wantPaths: []string{"cell/occ_test.go"},
	}, {
		name:     "an ordinary file is allowed",
		paths:    []string{"cell/occ.go", "README.md"},
		pol:      both,
		wantKind: "",
	}, {
		name:     "no rules configured allows ordinary files",
		paths:    []string{"cell/occ_test.go", "cmd/sig/run.go"},
		pol:      policy{present: true},
		wantKind: "",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, reason, refused := repairRefusalReason(tc.paths, tc.pol)
			if kind != tc.wantKind {
				t.Fatalf("kind=%q, want %q (reason=%q)", kind, tc.wantKind, reason)
			}
			if tc.wantKind == "" {
				if reason != "" || len(refused) != 0 {
					t.Fatalf("allowed path reported reason=%q refused=%v", reason, refused)
				}
				return
			}
			if reason == "" {
				t.Fatal("refused with an empty reason: nothing would explain the refusal")
			}
			if strings.Join(refused, ",") != strings.Join(tc.wantPaths, ",") {
				t.Fatalf("refused paths=%v, want %v", refused, tc.wantPaths)
			}
		})
	}
}

// TestDriveRunRepairCannotWeakenTheTestThatJudgedIt is the end-to-end proof of
// the product's central claim on the one path where it was false.
//
// The repo ships a test asserting guarded() == 1. An agent changes guarded() to
// return 2, so `go test` goes red. The fixer is then handed that failure, and
// takes the shortest path from red to green available to it: deleting the
// assertion. Before this guard existed the driver committed that edit, the
// re-verify passed on a suite with nothing left in it, and the change LANDED
// carrying a green verify.
//
// The assertion that matters is the last one — the landed tree. Checking only
// the report would pass against a driver that recorded a refusal and landed the
// weakened tree anyway.
func TestDriveRunRepairCannotWeakenTheTestThatJudgedIt(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	const assertion = "guarded() changed under us"
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("guard.go", "package main\n\nfunc guarded() int { return 1 }\n")
	write("guard_test.go", "package main\n\nimport \"testing\"\n\n"+
		"func TestGuarded(t *testing.T) {\n\tif guarded() != 1 {\n\t\tt.Fatal(\""+assertion+"\")\n\t}\n}\n")
	commitPolicy(t, g, repo, "verify = go test ./...\nrepair-deny = **/*_test.go\n")

	p := runParams{
		Repo:     repo,
		Base:     "main",
		Strategy: "overlay",
		AgentCmd: agent,
		// The fixer's cheapest route to green: gut the file that judged it.
		RepairCmd: `printf 'package main\n' > guard_test.go`,
		RepairMax: 2,
	}
	tasks := []taskSpec{taskWrite(t, "break", map[string]string{
		"guard.go": "package main\n\nfunc guarded() int { return 2 }\n",
	})}

	rep, err := driveRun(ctx, p, tasks)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}

	v := rep.Verify
	if v.OK {
		t.Fatal("verify.ok=true: the run reported success on a tree whose test was gutted")
	}
	if len(v.Repairs) == 0 {
		t.Fatal("no repair attempt recorded; the fixer never ran, so this test proves nothing")
	}
	last := v.Repairs[len(v.Repairs)-1]
	if last.Refused != repairRefusedDeny {
		t.Fatalf("repair refused=%q, want %q (attempt=%+v)", last.Refused, repairRefusedDeny, last)
	}
	if !contains(last.RefusedPaths, "guard_test.go") {
		t.Fatalf("refusedPaths=%v, want it to name guard_test.go", last.RefusedPaths)
	}

	// The tree is the assertion that catches this failure mode. The test file on
	// the base branch must still contain the assertion the fixer wanted gone.
	landed, present, err := g.BlobAt(ctx, "main", "guard_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("guard_test.go is gone from main: the fixer deleted the test and it landed")
	}
	if !strings.Contains(landed, assertion) {
		t.Fatalf("the assertion is gone from the landed guard_test.go; tree is:\n%s", landed)
	}
}

// TestDriveRunRepairMayStillFixOrdinaryCode is the other half of the pair: the
// guard must refuse the fixer's reach for the bar WITHOUT breaking repair
// itself. Same policy, same fixer shape, but the edit lands in ordinary source
// — so it is allowed, re-verify passes, and the run lands green.
//
// Without this, tightening repairRefusalReason until it refuses everything
// would pass the test above and silently destroy the feature.
func TestDriveRunRepairMayStillFixOrdinaryCode(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	commitPolicy(t, g, repo, "verify = go build ./...\nrepair-deny = **/*_test.go\nack-paths = shared.txt\n")

	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		RepairCmd: `printf 'package main\n\nfunc helper() int { return helperX() }\n' > repair_fix.go`,
		RepairMax: 2,
	}

	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}

	v := rep.Verify
	if !v.OK {
		t.Fatalf("verify.ok=false; an allowed repair was blocked. output=%q", v.Output)
	}
	if !v.Repaired {
		t.Fatal("repaired=false; the fixer's ordinary-source edit did not take effect")
	}
	last := v.Repairs[len(v.Repairs)-1]
	if last.Refused != "" {
		t.Fatalf("an ordinary source edit was refused as %q (paths=%v)", last.Refused, last.RefusedPaths)
	}
	if _, present, err := g.BlobAt(ctx, "main", "repair_fix.go"); err != nil || !present {
		t.Fatalf("the allowed fix did not land on main (present=%v, err=%v)", present, err)
	}
}
