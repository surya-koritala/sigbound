package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// buildTestAgent compiles the deterministic sig-testagent helper and returns
// its binary path, so the driver runs a real subprocess agent (no live LLM).
func buildTestAgent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "testagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/surya-koritala/sigbound/cmd/sig-testagent")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sig-testagent: %v\n%s", err, out)
	}
	return bin
}

// makeGoRepo initializes a temp module with a couple of Go files plus a data
// file agents will edit. `go build ./...` on the base tree passes.
func makeGoRepo(t *testing.T) (*gitx.Git, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	g := gitx.New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/e2e\n\ngo 1.21\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	// shared.txt: many distinct lines so an edit targets an unambiguous line.
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString("shared base line ")
		sb.WriteByte(byte('0' + i%10))
		sb.WriteByte('\n')
	}
	write("shared.txt", sb.String())
	if _, err := g.CommitAll(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	return g, dir
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDriveRunEndToEnd exercises the whole driver on a real repo with the
// deterministic agent: two disjoint tasks and one that overlaps a third on the
// same line of a shared file, resolved by a trivial union resolver, then
// verified with `go build ./...`.
func TestDriveRunEndToEnd(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	// t1: disjoint (own new file). t2: new file + edits shared.txt line 5.
	// t3: edits the SAME shared.txt line 5 -> real conflict with t2.
	tasks := []taskSpec{
		{ID: "t1", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"},
		})},
		{ID: "t2", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"beta.go": "package main\n\nfunc beta() int { return 2 }\n"},
			"edit":  map[string]any{"file": "shared.txt", "line": 5, "text": "t2-was-here"},
		})},
		{ID: "t3", Prompt: mustJSON(t, map[string]any{
			"edit": map[string]any{"file": "shared.txt", "line": 5, "text": "t3-was-here"},
		})},
	}

	p := runParams{
		Repo:     repo,
		Base:     "main",
		Strategy: "overlay",
		AgentCmd: agent, // sh -c execs the binary; it reads SIGBOUND_TASK
		// Trivial union resolver: emit ours then theirs (keeps both agents' work).
		ResolverCmd:     `cat "$SIGBOUND_OURS" "$SIGBOUND_THEIRS"`,
		ResolverTimeout: 10 * time.Second,
		VerifyCmd:       "go build ./...",
	}

	rep, err := driveRun(ctx, p, tasks)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}

	// ---- every agent committed on its own branch ----
	if len(rep.PerAgent) != 3 {
		t.Fatalf("perAgent=%d, want 3", len(rep.PerAgent))
	}
	byID := map[string]perAgentJSON{}
	for _, a := range rep.PerAgent {
		byID[a.ID] = a
		if !a.OK {
			t.Fatalf("agent %s not ok: exit=%d stderr=%q", a.ID, a.Exit, a.Stderr)
		}
		if a.Branch != "agent/"+a.ID {
			t.Fatalf("agent %s branch=%q, want agent/%s", a.ID, a.Branch, a.ID)
		}
		if a.SHA == "" || a.SHA == rep.BaseSHA {
			t.Fatalf("agent %s did not advance its branch (sha=%q)", a.ID, a.SHA)
		}
	}
	// write-sets: t1 disjoint, t2 touches beta.go + shared.txt, t3 touches shared.txt.
	if got := byID["t1"].Files; len(got) != 1 || got[0] != "alpha.go" {
		t.Fatalf("t1 files=%v, want [alpha.go]", got)
	}
	if !contains(byID["t2"].Files, "shared.txt") || !contains(byID["t2"].Files, "beta.go") {
		t.Fatalf("t2 files=%v, want beta.go+shared.txt", byID["t2"].Files)
	}

	// ---- integration: t1 disjoint (own group) parallel to the {t2,t3} group ----
	ig := rep.Integrate
	if ig.Groups != 2 {
		t.Fatalf("integrate groups=%d, want 2 (disjoint t1 || {t2,t3})", ig.Groups)
	}
	if len(ig.Landed) != 3 {
		t.Fatalf("landed=%d (%v), want 3", len(ig.Landed), ig.Landed)
	}
	if len(ig.Flagged) != 0 {
		t.Fatalf("flagged=%v, want none (resolver resolves the overlap)", ig.Flagged)
	}
	if ig.Resolved < 1 {
		t.Fatalf("resolved=%d, want >=1 (t2/t3 overlap resolved)", ig.Resolved)
	}
	if ig.FinalSHA == "" {
		t.Fatal("empty finalSHA")
	}

	// ---- final tree contains all disjoint work AND both overlap markers ----
	paths, err := g.LsTree(ctx, ig.FinalSHA)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alpha.go", "beta.go", "shared.txt", "main.go", "go.mod"} {
		if !contains(paths, want) {
			t.Fatalf("final tree %s missing %s (have %v)", ig.FinalSHA, want, paths)
		}
	}
	shared, err := g.ShowFile(ctx, ig.FinalSHA, "shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shared, "t2-was-here") || !strings.Contains(shared, "t3-was-here") {
		t.Fatalf("resolved shared.txt missing a marker:\n%s", shared)
	}

	// ---- base branch advanced to the integrated commit ----
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != ig.FinalSHA {
		t.Fatalf("main=%s not advanced to finalSHA=%s", mainSHA, ig.FinalSHA)
	}

	// ---- verify (`go build ./...`) passed on the integrated tree ----
	if !rep.Verify.Ran || !rep.Verify.OK {
		t.Fatalf("verify ran=%v ok=%v output=%q", rep.Verify.Ran, rep.Verify.OK, rep.Verify.Output)
	}
}

// TestDriveRunFlagsWithoutResolver runs the same overlap with NO resolver: the
// overlapping branch must be flagged (not silently dropped, not landed).
func TestDriveRunFlagsWithoutResolver(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	tasks := []taskSpec{
		{ID: "a", Prompt: mustJSON(t, map[string]any{
			"edit": map[string]any{"file": "shared.txt", "line": 5, "text": "a-here"},
		})},
		{ID: "b", Prompt: mustJSON(t, map[string]any{
			"edit": map[string]any{"file": "shared.txt", "line": 5, "text": "b-here"},
		})},
	}
	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent}

	rep, err := driveRun(ctx, p, tasks)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.Integrate.Landed) != 1 || len(rep.Integrate.Flagged) != 1 {
		t.Fatalf("landed=%d flagged=%d, want 1/1", len(rep.Integrate.Landed), len(rep.Integrate.Flagged))
	}
	if got := rep.Integrate.Flagged[0].Paths; len(got) != 1 || got[0] != "shared.txt" {
		t.Fatalf("flagged paths=%v, want [shared.txt]", got)
	}
}

// TestDriveRunLaneEnforcement: an agent that declares Files=[a.go] but also
// writes b.go is "out of lane". In warn mode it still lands but the report
// records strayed=[b.go]; in strict mode it is treated as a failed agent — not
// landed — with the reason recorded.
func TestDriveRunLaneEnforcement(t *testing.T) {
	ctx := context.Background()
	agent := buildTestAgent(t)

	// Declares only a.go, but the agent writes a.go AND b.go.
	prompt := mustJSON(t, map[string]any{
		"write": map[string]string{
			"a.go": "package main\n\nfunc a() int { return 1 }\n",
			"b.go": "package main\n\nfunc b() int { return 2 }\n",
		},
	})
	task := taskSpec{ID: "lane1", Prompt: prompt, Files: []string{"a.go"}}

	// ---- warn (default): lands with strayed=[b.go], inLane=false ----
	_, repoWarn := makeGoRepo(t)
	warn := runParams{Repo: repoWarn, Base: "main", Strategy: "overlay", AgentCmd: agent, LaneMode: laneWarn}
	repW, err := driveRun(ctx, warn, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun warn: %v", err)
	}
	aw := repW.PerAgent[0]
	if !aw.OK {
		t.Fatalf("warn: out-of-lane agent should still land, got not OK: %q", aw.Stderr)
	}
	if aw.InLane {
		t.Fatal("warn: agent should be marked out-of-lane (inLane=false)")
	}
	if !contains(aw.Strayed, "b.go") || contains(aw.Strayed, "a.go") {
		t.Fatalf("warn: strayed=%v, want exactly [b.go]", aw.Strayed)
	}
	if !contains(aw.DeclaredFiles, "a.go") {
		t.Fatalf("warn: declaredFiles=%v, want a.go", aw.DeclaredFiles)
	}
	if len(repW.Integrate.Landed) != 1 {
		t.Fatalf("warn: landed=%d, want 1 (still lands)", len(repW.Integrate.Landed))
	}

	// ---- strict: NOT landed, agent not OK, reason recorded ----
	_, repoStrict := makeGoRepo(t)
	strict := runParams{Repo: repoStrict, Base: "main", Strategy: "overlay", AgentCmd: agent, LaneMode: laneStrict}
	repS, err := driveRun(ctx, strict, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun strict: %v", err)
	}
	as := repS.PerAgent[0]
	if as.OK {
		t.Fatal("strict: out-of-lane agent must not be OK")
	}
	if as.InLane {
		t.Fatal("strict: agent should be out-of-lane")
	}
	if !contains(as.Strayed, "b.go") {
		t.Fatalf("strict: strayed=%v, want b.go", as.Strayed)
	}
	if !strings.Contains(as.Stderr, "out-of-lane") {
		t.Fatalf("strict: out-of-lane reason not recorded: %q", as.Stderr)
	}
	if len(repS.Integrate.Landed) != 0 {
		t.Fatalf("strict: landed=%d, want 0 (out-of-lane agent not landed)", len(repS.Integrate.Landed))
	}

	// ---- off: no lane accounting even though it strayed; still lands ----
	_, repoOff := makeGoRepo(t)
	off := runParams{Repo: repoOff, Base: "main", Strategy: "overlay", AgentCmd: agent, LaneMode: laneOff}
	repO, err := driveRun(ctx, off, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun off: %v", err)
	}
	ao := repO.PerAgent[0]
	if !ao.OK || !ao.InLane || len(ao.Strayed) != 0 {
		t.Fatalf("off: want OK, inLane=true, no strayed; got ok=%v inLane=%v strayed=%v", ao.OK, ao.InLane, ao.Strayed)
	}
	if len(repO.Integrate.Landed) != 1 {
		t.Fatalf("off: landed=%d, want 1", len(repO.Integrate.Landed))
	}
}

// TestRunExitCode exercises the outcome->exit-code mapping directly (the
// seam runRun uses), including the override precedence when a report matches
// more than one failure class: verify-failed > no-agent-succeeded > flagged.
func TestRunExitCode(t *testing.T) {
	cases := []struct {
		name string
		rep  runReport
		want int
	}{
		{
			name: "clean landed, verify unset",
			rep:  runReport{PerAgent: []perAgentJSON{{OK: true}}},
			want: exitOK,
		},
		{
			name: "clean landed, verify passed",
			rep: runReport{
				PerAgent: []perAgentJSON{{OK: true}},
				Verify:   verifyJSON{Ran: true, OK: true},
			},
			want: exitOK,
		},
		{
			name: "verify failed wins over flagged",
			rep: runReport{
				PerAgent:  []perAgentJSON{{OK: true}},
				Integrate: integrateJSON{Flagged: []flaggedJSON{{Branch: "agent/x"}}},
				Verify:    verifyJSON{Ran: true, OK: false},
			},
			want: exitVerifyFailed,
		},
		{
			name: "verify failed wins over no-agent-succeeded",
			rep: runReport{
				PerAgent: []perAgentJSON{{OK: false}},
				Verify:   verifyJSON{Ran: true, OK: false},
			},
			want: exitVerifyFailed,
		},
		{
			name: "no agent succeeded, verify never ran",
			rep:  runReport{PerAgent: []perAgentJSON{{OK: false}, {OK: false}}},
			want: exitNoAgentSucceeded,
		},
		{
			name: "no agent succeeded, verify trivially passed",
			rep: runReport{
				PerAgent: []perAgentJSON{{OK: false}},
				Verify:   verifyJSON{Ran: true, OK: true},
			},
			want: exitNoAgentSucceeded,
		},
		{
			name: "no-agent-succeeded wins over flagged",
			rep: runReport{
				PerAgent:  []perAgentJSON{{OK: false}},
				Integrate: integrateJSON{Flagged: []flaggedJSON{{Branch: "agent/x"}}},
			},
			want: exitNoAgentSucceeded,
		},
		{
			name: "flagged only",
			rep: runReport{
				PerAgent:  []perAgentJSON{{OK: true}, {OK: true}},
				Integrate: integrateJSON{Flagged: []flaggedJSON{{Branch: "agent/x"}}},
			},
			want: exitFlagged,
		},
	}
	for _, c := range cases {
		if got := runExitCode(c.rep); got != c.want {
			t.Errorf("%s: runExitCode=%d, want %d", c.name, got, c.want)
		}
	}
}

// TestDriveRunIntegrateFailureKeepsPerAgent: a REAL integrate failure (an
// unknown strategy name — driveRun itself doesn't validate it, unlike runRun's
// flag parsing) after two agents have genuinely committed. driveRun must
// return the error AND a report whose PerAgent already names the branches
// those agents landed — the exact data runRun needs to still emit on a
// mid-run failure. Regression test for issue #5: this data used to be
// silently discarded by runRun's caller.
func TestDriveRunIntegrateFailureKeepsPerAgent(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	tasks := []taskSpec{
		{ID: "t1", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"},
		})},
		{ID: "t2", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"beta.go": "package main\n\nfunc beta() int { return 2 }\n"},
		})},
	}
	p := runParams{Repo: repo, Base: "main", Strategy: "not-a-real-strategy", AgentCmd: agent}

	rep, err := driveRun(ctx, p, tasks)
	if err == nil {
		t.Fatal("expected an integrate error for an unknown strategy")
	}
	if !strings.Contains(err.Error(), "integrate:") {
		t.Fatalf("err=%q, want it to name the integrate stage", err)
	}

	if len(rep.PerAgent) != 2 {
		t.Fatalf("perAgent=%d, want 2 (both agents ran before the integrate error)", len(rep.PerAgent))
	}
	for _, a := range rep.PerAgent {
		if !a.OK {
			t.Fatalf("agent %s not ok: exit=%d stderr=%q", a.ID, a.Exit, a.Stderr)
		}
		if a.SHA == "" || a.SHA == rep.BaseSHA {
			t.Fatalf("agent %s did not advance its branch (sha=%q)", a.ID, a.SHA)
		}
		if a.Branch != "agent/"+a.ID {
			t.Fatalf("agent %s branch=%q, want agent/%s", a.ID, a.Branch, a.ID)
		}
	}
	// Nothing integrated: the report's integrate section stays zero-valued.
	if len(rep.Integrate.Landed) != 0 {
		t.Fatalf("landed=%v, want none (integrate itself errored)", rep.Integrate.Landed)
	}
}

// TestEmitReport is a direct test of the seam runRun calls to render the
// report (on both the success path and the new mid-run-error path): given a
// report with PerAgent already filled in, it must write valid JSON in -json
// mode and the terse summary otherwise — the exact two code paths runRun used
// to have inlined before driveRun's error path needed the same rendering.
func TestEmitReport(t *testing.T) {
	rep := runReport{
		Repo: "/tmp/repo", Base: "main", BaseSHA: "abc123",
		PerAgent: []perAgentJSON{
			{ID: "t1", Branch: "agent/t1", SHA: "deadbeef", OK: true, Files: []string{"alpha.go"}},
		},
	}

	var jsonBuf bytes.Buffer
	if err := emitReport(&jsonBuf, rep, true); err != nil {
		t.Fatalf("emitReport(json): %v", err)
	}
	var got runReport
	if err := json.Unmarshal(jsonBuf.Bytes(), &got); err != nil {
		t.Fatalf("parse emitted JSON: %v\n%s", err, jsonBuf.String())
	}
	if len(got.PerAgent) != 1 || got.PerAgent[0].Branch != "agent/t1" || got.PerAgent[0].SHA != "deadbeef" {
		t.Fatalf("round-tripped report perAgent=%+v, want the agent/t1 branch preserved", got.PerAgent)
	}

	var sumBuf bytes.Buffer
	if err := emitReport(&sumBuf, rep, false); err != nil {
		t.Fatalf("emitReport(summary): %v", err)
	}
	summary := sumBuf.String()
	if !strings.Contains(summary, "agent/t1") || !strings.Contains(summary, "t1") {
		t.Fatalf("summary missing the surviving agent branch:\n%s", summary)
	}
}

// TestRunRunNoReportWhenNoAgentsRan: when driveRun fails before any agent ran
// (here: -base names a branch that doesn't exist, so RevParse fails first),
// runRun must print nothing — an empty PerAgent means there is nothing worth
// recovering, same as before this change.
func TestRunRunNoReportWhenNoAgentsRan(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	tasksFile := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(tasksFile, []byte(`[{"id":"a","prompt":"x"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-base", "does-not-exist",
		"-tasks", tasksFile,
		"-agent", agent,
	})
	if err == nil {
		t.Fatal("expected an error resolving a nonexistent base")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("no report should be written when no agents ran, got:\n%s", buf.String())
	}
}

// TestDriveRunVerifyRetries covers -verify-retries against a verify command
// that fails on its first invocation and passes on every one after. Each
// invocation runs in a FRESH detached worktree (see runVerify), so the
// fail-once state has to live outside the checkout: a marker file at a fixed
// path, created by the command itself on its first (failing) run.
func TestDriveRunVerifyRetries(t *testing.T) {
	ctx := context.Background()
	agent := buildTestAgent(t)
	task := taskSpec{ID: "ok", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"good.go": "package main\n\nfunc good() int { return 1 }\n"},
	})}
	flakyVerify := func(marker string) string {
		return "test -f '" + marker + "' || { touch '" + marker + "'; exit 1; }"
	}

	// ---- -verify-retries 1: fails once, retry passes -> ok=true, flaky=true ----
	_, repo1 := makeGoRepo(t)
	p1 := runParams{
		Repo: repo1, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:     flakyVerify(filepath.Join(t.TempDir(), "marker")),
		VerifyRetries: 1,
	}
	rep1, err := driveRun(ctx, p1, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep1.Verify.OK {
		t.Fatalf("verify.ok=false, want true after a retry: %q", rep1.Verify.Output)
	}
	if !rep1.Verify.Flaky {
		t.Fatal("verify.flaky=false, want true (passed only on the 2nd invocation)")
	}
	if mainSHA, err := gitx.New(repo1).RevParse(ctx, "main"); err != nil || mainSHA != rep1.Integrate.FinalSHA {
		t.Fatalf("flaky-but-green run must still land: main=%s finalSHA=%s err=%v", mainSHA, rep1.Integrate.FinalSHA, err)
	}

	// ---- -verify-retries 0 (fresh marker, same command): today's behavior — a
	// single failing invocation fails verify outright, never flaky ----
	_, repo0 := makeGoRepo(t)
	p0 := runParams{
		Repo: repo0, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:     flakyVerify(filepath.Join(t.TempDir(), "marker")),
		VerifyRetries: 0,
	}
	rep0, err := driveRun(ctx, p0, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep0.Verify.OK {
		t.Fatal("verify.ok=true with -verify-retries 0, want false (no retry to save a failing first invocation)")
	}
	if rep0.Verify.Flaky {
		t.Fatal("verify.flaky=true with -verify-retries 0, want false")
	}
	if code := runExitCode(rep0); code != exitVerifyFailed {
		t.Fatalf("runExitCode=%d, want exitVerifyFailed", code)
	}
}

// TestDriveRunRepairUsesVerifyRetries: the repair-loop interplay is untouched
// at the default -verify-retries=0 — same fixture, same assertions as
// TestDriveRunRepairSucceeds, just with VerifyRetries set explicitly to prove
// the new seam doesn't change repair behavior when retries are off.
func TestDriveRunRepairUsesVerifyRetries(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	p := runParams{
		Repo:          repo,
		Base:          "main",
		Strategy:      "overlay",
		AgentCmd:      agent,
		VerifyCmd:     "go build ./...",
		VerifyRetries: 0,
		RepairCmd:     `printf 'package main\n\nfunc helper() int { return helperX() }\n' > repair_fix.go`,
		RepairMax:     2,
	}

	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	v := rep.Verify
	if !v.Ran || !v.OK {
		t.Fatalf("verify ran=%v ok=%v output=%q", v.Ran, v.OK, v.Output)
	}
	if !v.Repaired {
		t.Fatal("repaired=false; expected the repair loop to have fixed an initial failure")
	}
	if v.Flaky {
		t.Fatal("flaky=true with -verify-retries 0; repair loop behavior must be unaffected")
	}
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != rep.Integrate.FinalSHA {
		t.Fatalf("main=%s not advanced to repaired finalSHA=%s", mainSHA, rep.Integrate.FinalSHA)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// brokenBuildTasks are two DISJOINT tasks whose combined tree does NOT compile:
// `caller.go` calls helper(), but the other task only defines helperX() — so the
// integrated tree fails `go build ./...` with "undefined: helper". Both write
// their own file, so integration itself is clean (the break is semantic, not a
// merge conflict) — exactly the case the self-healing repair loop targets.
func brokenBuildTasks(t *testing.T) []taskSpec {
	t.Helper()
	return []taskSpec{
		{ID: "caller", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"caller.go": "package main\n\nfunc caller() int { return helper() }\n"},
		})},
		{ID: "helper", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"helper.go": "package main\n\nfunc helperX() int { return 42 }\n"},
		})},
	}
}

// TestDriveRunRepairSucceeds: integration yields a tree that fails `go build`,
// -repair supplies the missing helper() (a deterministic, LLM-free fixer), and
// re-verify PASSES. The report must show repaired=true, attempts>=1, the fix
// commit's files, and the fix must LAND on the base branch.
func TestDriveRunRepairSucceeds(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		VerifyCmd: "go build ./...",
		// Deterministic fixer: define the missing helper() in a new file. The
		// driver auto-commits it (the fixer only edits, never runs git).
		RepairCmd: `printf 'package main\n\nfunc helper() int { return helperX() }\n' > repair_fix.go`,
		RepairMax: 2,
	}

	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}

	// Both agents committed disjoint files, integration landed both cleanly.
	if len(rep.Integrate.Landed) != 2 {
		t.Fatalf("landed=%v, want both agent branches", rep.Integrate.Landed)
	}

	v := rep.Verify
	if !v.Ran {
		t.Fatal("verify did not run")
	}
	if !v.OK {
		t.Fatalf("verify.ok=false after repair; output=%q", v.Output)
	}
	if !v.Repaired {
		t.Fatal("repaired=false; expected the repair loop to have fixed an initial failure")
	}
	if v.Attempts < 1 {
		t.Fatalf("attempts=%d, want >=1", v.Attempts)
	}
	if len(v.Repairs) == 0 {
		t.Fatal("no per-attempt repair records")
	}
	last := v.Repairs[len(v.Repairs)-1]
	if !last.VerifyOK {
		t.Fatalf("last repair attempt verifyOk=false: %+v", last)
	}
	if !contains(last.FilesTouched, "repair_fix.go") {
		t.Fatalf("repair filesTouched=%v, want repair_fix.go", last.FilesTouched)
	}

	// The fix must LAND: base branch advanced to the repaired head, whose tree
	// contains the fix file and actually compiles.
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != rep.Integrate.FinalSHA {
		t.Fatalf("main=%s not advanced to repaired finalSHA=%s", mainSHA, rep.Integrate.FinalSHA)
	}
	paths, err := g.LsTree(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"caller.go", "helper.go", "repair_fix.go"} {
		if !contains(paths, want) {
			t.Fatalf("landed tree missing %s (have %v)", want, paths)
		}
	}
}

// TestDriveRunVerifyFailLandsNothing: when -verify fails and no -repair is set,
// the base branch must NOT advance — a red run lands nothing, per the documented
// guarantee. Regression test for landing before verify (base ref advanced while
// verify.ok=false).
func TestDriveRunVerifyFailLandsNothing(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	before, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}

	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		VerifyCmd: "go build ./...", // fails: brokenBuildTasks references undefined helper()
	}
	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep.Verify.OK {
		t.Fatal("verify.ok=true on a broken build")
	}
	after, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("base ref advanced to %s on verify failure; must stay at %s (land nothing)", after, before)
	}
	// This is the exact bug issue #4 fixes: verify.ok=false must not exit 0.
	if code := runExitCode(rep); code != exitVerifyFailed {
		t.Fatalf("runExitCode=%d, want exitVerifyFailed(%d) on a verify failure", code, exitVerifyFailed)
	}
}

// TestDriveRunRepairFailsHonestly: the same broken build, but the fixer does NOT
// fix it and -repair-max=1. The loop must run exactly once and then report
// verify.ok=false HONESTLY (no false green), repaired=false, with the per-attempt
// record showing the fixer touched a file yet verify still failed.
func TestDriveRunRepairFailsHonestly(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		VerifyCmd: "go build ./...",
		// A fixer that edits a file but does NOT define helper() — the build stays
		// broken, so the loop cannot make verify pass.
		RepairCmd: `printf 'package main\n\nfunc noop() int { return 7 }\n' > repair_noop.go`,
		RepairMax: 1,
	}

	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}

	v := rep.Verify
	if !v.Ran {
		t.Fatal("verify did not run")
	}
	if v.OK {
		t.Fatal("verify.ok=true — must NOT claim green when the repair never fixed the build")
	}
	if v.Repaired {
		t.Fatal("repaired=true — nothing was actually repaired")
	}
	if v.Attempts != 1 {
		t.Fatalf("attempts=%d, want exactly 1 (repair-max=1)", v.Attempts)
	}
	if len(v.Repairs) != 1 {
		t.Fatalf("repairs=%d records, want 1", len(v.Repairs))
	}
	if v.Repairs[0].VerifyOK {
		t.Fatal("repair attempt verifyOk=true, but the build is still broken")
	}
	if !contains(v.Repairs[0].FilesTouched, "repair_noop.go") {
		t.Fatalf("attempt filesTouched=%v, want repair_noop.go", v.Repairs[0].FilesTouched)
	}
	if strings.TrimSpace(v.Output) == "" {
		t.Fatal("empty verify output; the honest failure output must be reported")
	}
}

// TestDriveRunRepairSkippedWhenVerifyPasses: when -verify passes on the FIRST
// try, the repair loop must never run even though -repair is configured.
func TestDriveRunRepairSkippedWhenVerifyPasses(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	// A single benign task whose integrated tree compiles fine.
	tasks := []taskSpec{
		{ID: "ok", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"good.go": "package main\n\nfunc good() int { return 1 }\n"},
		})},
	}
	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		VerifyCmd: "go build ./...",
		RepairCmd: `printf 'package main\n\nfunc never() {}\n' > should_not_exist.go`,
		RepairMax: 3,
	}

	rep, err := driveRun(ctx, p, tasks)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	v := rep.Verify
	if !v.Ran || !v.OK {
		t.Fatalf("verify ran=%v ok=%v, want first-try pass", v.Ran, v.OK)
	}
	if v.Attempts != 0 || v.Repaired || len(v.Repairs) != 0 {
		t.Fatalf("repair loop ran on a first-try pass: attempts=%d repaired=%v repairs=%d", v.Attempts, v.Repaired, len(v.Repairs))
	}
	// The fixer must never have executed, so its file is absent from the tree.
	paths, err := g.LsTree(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if contains(paths, "should_not_exist.go") {
		t.Fatal("fixer ran despite verify passing on the first try")
	}
}
