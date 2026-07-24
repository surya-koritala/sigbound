package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/surya-koritala/sigbound/cell"
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

// TestDriveRunAssertHealthy wires -assert (runParams.Assert -> integrateBranches
// -> cell.WithAssert) end to end on a healthy disjoint batch: it must still
// land everything normally, proving the paranoid cross-check doesn't change
// the outcome when there's nothing wrong to catch.
func TestDriveRunAssertHealthy(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	tasks := []taskSpec{
		{ID: "a", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
		})},
		{ID: "b", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"b.go": "package main\n\nfunc b() int { return 2 }\n"},
		})},
	}
	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", Assert: true, AgentCmd: agent}

	rep, err := driveRun(ctx, p, tasks)
	if err != nil {
		t.Fatalf("driveRun with -assert: %v", err)
	}
	if len(rep.Integrate.Landed) != 2 || len(rep.Integrate.Flagged) != 0 {
		t.Fatalf("landed=%d flagged=%d, want 2/0", len(rep.Integrate.Landed), len(rep.Integrate.Flagged))
	}
}

// makeSemanticGoRepo extends makeGoRepo with a.go (declaring F) and b.go (a
// caller stub that does NOT yet call F) already committed at base — the
// motivating -semantic go scenario's starting point: one agent will change
// F's signature in a.go, another will add a genuinely incompatible call to F
// in the DISJOINT file b.go.
func makeSemanticGoRepo(t *testing.T) (*gitx.Git, string) {
	t.Helper()
	g, repo := makeGoRepo(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package main\n\nfunc F() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "b.go"), []byte("package main\n\nfunc UseF() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CommitAll(ctx, "add a.go/b.go"); err != nil {
		t.Fatal(err)
	}
	return g, repo
}

// semanticScenarioTasks is the motivating scenario's two path-disjoint tasks:
// ta changes F's signature in a.go, tb adds a new call to F (the OLD,
// zero-arg form) in b.go — a real cross-file signature/caller mismatch that
// leaves a.go and b.go individually valid but the combined tree broken.
func semanticScenarioTasks(t *testing.T) []taskSpec {
	return []taskSpec{
		{ID: "ta", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"a.go": "package main\n\nfunc F(x int) int { return x }\n"},
		})},
		{ID: "tb", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"b.go": "package main\n\nfunc UseF() int { return F() }\n"},
		})},
	}
}

// TestDriveRunSemanticGoGroupsCrossFileSignatureConflict is the motivating
// scenario end to end, WITH -semantic go: branch ta changes F's signature in
// a.go; branch tb adds a brand-new call to F in the path-DISJOINT file b.go.
// Plain path partitioning would land them in parallel groups (they touch
// different files); -semantic go must instead recognize the symbol-level
// overlap, merge them into ONE partition group (report.integrate.groups==1,
// semanticEdges names the pair), and route them through the normal
// fold+merge-tree path — the merge itself still stays textually clean (the
// files never touch the same lines), so the build only breaks once -verify
// runs on the combined tree, exactly as the no-flag case does too (see
// TestDriveRunSemanticOffKeepsPathOnlyPartitioning): -verify remains the
// source of truth either way, -semantic only changes HOW the pair combines
// and what the report/events record about it.
func TestDriveRunSemanticGoGroupsCrossFileSignatureConflict(t *testing.T) {
	ctx := context.Background()
	_, repo := makeSemanticGoRepo(t)
	agent := buildTestAgent(t)
	eventsPath := filepath.Join(t.TempDir(), "events.ndjson")

	p := runParams{
		Repo:       repo,
		Base:       "main",
		Strategy:   "overlay",
		AgentCmd:   agent,
		VerifyCmd:  "go build ./...",
		Semantic:   semanticGo,
		EventsPath: eventsPath,
	}
	rep, err := driveRun(ctx, p, semanticScenarioTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	for _, a := range rep.PerAgent {
		if !a.OK {
			t.Fatalf("agent %s not ok: exit=%d stderr=%q", a.ID, a.Exit, a.Stderr)
		}
		if a.SemanticNote != "analyzed" {
			t.Fatalf("agent %s semanticNote=%q, want analyzed", a.ID, a.SemanticNote)
		}
	}

	// ---- semantically merged into ONE group, both land (no textual conflict) ----
	if rep.Integrate.Groups != 1 {
		t.Fatalf("groups=%d, want 1 (ta/tb merged by the semantic edge despite disjoint paths)", rep.Integrate.Groups)
	}
	if len(rep.Integrate.Landed) != 2 || len(rep.Integrate.Flagged) != 0 {
		t.Fatalf("landed=%d flagged=%d, want 2/0 (a.go/b.go never share a line, so no textual conflict)", len(rep.Integrate.Landed), len(rep.Integrate.Flagged))
	}
	wantEdges := [][2]string{{"agent/ta", "agent/tb"}}
	if !reflect.DeepEqual(rep.Integrate.SemanticEdges, wantEdges) {
		t.Fatalf("semanticEdges=%v, want %v", rep.Integrate.SemanticEdges, wantEdges)
	}

	// ---- -verify still catches the genuinely broken build ----
	if !rep.Verify.Ran || rep.Verify.OK {
		t.Fatalf("verify ran=%v ok=%v, want ran/red (F's signature and its new caller disagree)", rep.Verify.Ran, rep.Verify.OK)
	}

	// ---- events: semantic_done fires with the edge, after agents/before integrate ----
	events, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(events)), "\n")
	semIdx, integrateStartIdx := -1, -1
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad event line %d: %v", i, err)
		}
		switch rec["event"] {
		case "semantic_done":
			semIdx = i
			edges, _ := rec["edges"].([]any)
			if len(edges) != 1 {
				t.Fatalf("semantic_done edges=%v, want 1 pair", edges)
			}
		case "integrate_start":
			integrateStartIdx = i
		}
	}
	if semIdx < 0 {
		t.Fatal("no semantic_done event")
	}
	if integrateStartIdx < 0 || semIdx >= integrateStartIdx {
		t.Fatalf("semantic_done (line %d) must fire before integrate_start (line %d)", semIdx, integrateStartIdx)
	}
}

// TestDriveRunSemanticOffKeepsPathOnlyPartitioning is the SAME motivating
// scenario WITHOUT -semantic (the default, "off"): today's behavior. a.go and
// b.go are path-disjoint, so they land in 2 independent groups, no
// semanticEdges/semanticNote appear anywhere in the report, and -verify still
// catches the broken build — just later, after the whole batch already
// landed together, exactly as issue #58's brief describes.
func TestDriveRunSemanticOffKeepsPathOnlyPartitioning(t *testing.T) {
	ctx := context.Background()
	_, repo := makeSemanticGoRepo(t)
	agent := buildTestAgent(t)

	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		VerifyCmd: "go build ./...",
		// Semantic left at the zero value ("") -- same as an explicit "off".
	}
	rep, err := driveRun(ctx, p, semanticScenarioTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep.Integrate.Groups != 2 {
		t.Fatalf("groups=%d, want 2 (path-disjoint, no semantic analysis)", rep.Integrate.Groups)
	}
	if len(rep.Integrate.SemanticEdges) != 0 {
		t.Fatalf("semanticEdges=%v, want none (-semantic off)", rep.Integrate.SemanticEdges)
	}
	for _, a := range rep.PerAgent {
		if a.SemanticNote != "" {
			t.Fatalf("agent %s semanticNote=%q, want empty (-semantic off)", a.ID, a.SemanticNote)
		}
	}
	if len(rep.Integrate.Landed) != 2 || len(rep.Integrate.Flagged) != 0 {
		t.Fatalf("landed=%d flagged=%d, want 2/0", len(rep.Integrate.Landed), len(rep.Integrate.Flagged))
	}
	// -verify still catches it, late: the combined tree is the SAME broken
	// tree either way, path-only partitioning just didn't see it coming.
	if !rep.Verify.Ran || rep.Verify.OK {
		t.Fatalf("verify ran=%v ok=%v, want ran/red", rep.Verify.Ran, rep.Verify.OK)
	}
}

// TestIntegrateBranchesReusesPrecomputedWriteSets is the correctness anchor for
// the writeSets param. The write-set only drives OCC PARTITIONING (which
// branches are treated as overlapping); actual landed content always comes
// from the branch's real tree, so the only observable signal that a supplied
// write-set was TRUSTED (not silently re-diffed and corrected) is the
// resulting group count. Two branches touch disjoint real files (x.txt,
// y.txt — an accurate diff partitions them into 2 independent groups), but a
// deliberately WRONG writeSets claims they both touched the same path: if
// integrateBranches actually used that lie for partitioning, they land forced
// into ONE group instead of two. The complementary nil-writeSets call proves
// the batched fallback (gitx.DiffNameOnlyBatch) still computes the correct,
// disjoint real write-sets when nothing is precomputed.
func TestIntegrateBranchesReusesPrecomputedWriteSets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := gitx.New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("k\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	c, err := cell.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	mkBranch := func(branch, file string) {
		wt := filepath.Join(t.TempDir(), branch)
		if err := g.WorktreeAdd(ctx, wt, branch, base); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, file), []byte("new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := g.At(wt).CommitAll(ctx, branch); err != nil {
			t.Fatal(err)
		}
		if err := g.WorktreeRemove(ctx, wt); err != nil {
			t.Fatal(err)
		}
	}
	mkBranch("agent/x", "x.txt")
	mkBranch("agent/y", "y.txt")
	branches := []string{"agent/x", "agent/y"}

	// Lie: both branches supposedly touched the same path, though their real
	// diffs (x.txt vs y.txt) are disjoint.
	lying := map[string][]string{
		"agent/x": {"fake-shared.txt"},
		"agent/y": {"fake-shared.txt"},
	}
	res, err := integrateBranches(ctx, c, "main", base, branches, lying, "overlay", "", 0, false, false, nil, nil)
	if err != nil {
		t.Fatalf("integrateBranches (lying writeSets): %v", err)
	}
	if res.Groups != 1 {
		t.Fatalf("groups=%d with a lying shared-path writeSets, want 1 (forced together): integrateBranches did not trust the supplied write-sets", res.Groups)
	}
	if len(res.Landed) != 2 {
		t.Fatalf("landed=%v, want both branches (folding two non-conflicting real diffs together must still succeed)", res.Landed)
	}

	// No precomputed data at all: the batched fallback must compute the real,
	// disjoint write-sets and partition the same two branches into 2 groups.
	res, err = integrateBranches(ctx, c, "main", base, branches, nil, "overlay", "", 0, false, false, nil, nil)
	if err != nil {
		t.Fatalf("integrateBranches (nil writeSets): %v", err)
	}
	if res.Groups != 2 {
		t.Fatalf("groups=%d with nil writeSets, want 2 (real diffs are disjoint): the batched fallback computed the wrong write-sets", res.Groups)
	}
	if len(res.Landed) != 2 {
		t.Fatalf("landed=%v, want both branches", res.Landed)
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

// TestDriveRunKeepFailed: -keep-failed retains a FAILED agent's worktree on
// disk (and names it in the report) instead of tearing it down; without the
// flag the worktree is removed as before. A successful agent's worktree is
// always removed, flag or not — -keep-failed only affects failures.
func TestDriveRunKeepFailed(t *testing.T) {
	ctx := context.Background()
	task := taskSpec{ID: "bad", Prompt: "x"} // sig-testagent would fail on this prompt too, but "exit 1" is simpler and direct

	// ---- failing agent, -keep-failed: worktree survives, path reported ----
	_, repo := makeGoRepo(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // pin driveRun's worktree root under a dir this test controls
	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: "exit 1", KeepFailed: true}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.PerAgent) != 1 || rep.PerAgent[0].OK {
		t.Fatalf("want one failed agent, got %+v", rep.PerAgent)
	}
	kept := rep.PerAgent[0].WorktreeKept
	if kept == "" {
		t.Fatal("want worktreeKept set for a failed agent run with -keep-failed")
	}
	if fi, err := os.Stat(kept); err != nil || !fi.IsDir() {
		t.Fatalf("kept worktree %s should still exist as a dir: %v", kept, err)
	}

	// ---- same failure, no -keep-failed: worktree (and its root) cleaned up ----
	_, repo2 := makeGoRepo(t)
	tmp2 := t.TempDir()
	t.Setenv("TMPDIR", tmp2)
	p2 := runParams{Repo: repo2, Base: "main", Strategy: "overlay", AgentCmd: "exit 1"}
	rep2, err := driveRun(ctx, p2, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if got := rep2.PerAgent[0].WorktreeKept; got != "" {
		t.Fatalf("want no worktreeKept without -keep-failed, got %q", got)
	}
	entries, err := os.ReadDir(tmp2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("want the run's worktree root cleaned up under %s, found %v", tmp2, entries)
	}

	// ---- successful agent, -keep-failed set: still removed (not a failure) ----
	_, repo3 := makeGoRepo(t)
	tmp3 := t.TempDir()
	t.Setenv("TMPDIR", tmp3)
	agent := buildTestAgent(t)
	goodTask := taskSpec{ID: "good", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"ok.go": "package main\n\nfunc ok() int { return 1 }\n"},
	})}
	p3 := runParams{Repo: repo3, Base: "main", Strategy: "overlay", AgentCmd: agent, KeepFailed: true}
	rep3, err := driveRun(ctx, p3, []taskSpec{goodTask})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep3.PerAgent[0].OK {
		t.Fatalf("want the agent to succeed, got %+v", rep3.PerAgent[0])
	}
	if got := rep3.PerAgent[0].WorktreeKept; got != "" {
		t.Fatalf("want no worktreeKept for a successful agent even with -keep-failed, got %q", got)
	}
	entries3, err := os.ReadDir(tmp3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries3) != 0 {
		t.Fatalf("want the successful agent's worktree root cleaned up under %s, found %v", tmp3, entries3)
	}

	// ---- out-of-lane under -lanes strict, -keep-failed: strict turns the stray
	// into a failure, so it must be kept too, same as any other failed agent ----
	_, repo4 := makeGoRepo(t)
	tmp4 := t.TempDir()
	t.Setenv("TMPDIR", tmp4)
	strayAgent := buildTestAgent(t)
	strayTask := taskSpec{ID: "stray", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{
			"a.go": "package main\n\nfunc a() int { return 1 }\n",
			"b.go": "package main\n\nfunc b() int { return 2 }\n",
		},
	}), Files: []string{"a.go"}}
	p4 := runParams{Repo: repo4, Base: "main", Strategy: "overlay", AgentCmd: strayAgent, LaneMode: laneStrict, KeepFailed: true}
	rep4, err := driveRun(ctx, p4, []taskSpec{strayTask})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	a4 := rep4.PerAgent[0]
	if a4.OK {
		t.Fatal("strict+keep-failed: out-of-lane agent must not be OK")
	}
	kept4 := a4.WorktreeKept
	if kept4 == "" {
		t.Fatal("strict+keep-failed: want worktreeKept set for an out-of-lane agent under -lanes strict")
	}
	if fi, err := os.Stat(kept4); err != nil || !fi.IsDir() {
		t.Fatalf("kept worktree %s should still exist as a dir: %v", kept4, err)
	}
}

// TestDriveRunAgentTimeout: -agent-timeout bounds a single hung agent instead
// of letting it block the whole run. The agent sleeps far longer than the
// timeout; a driveRun call that actually enforces -agent-timeout returns well
// before the sleep would finish on its own (asserted below), and the failed
// attempt is reported exit=-1, timedOut=true, with the reason in stderr.
func TestDriveRunAgentTimeout(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	task := taskSpec{ID: "slow", Prompt: "x"}

	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: "sleep 5", AgentTimeout: 200 * time.Millisecond}
	start := time.Now()
	rep, err := driveRun(ctx, p, []taskSpec{task})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("driveRun took %s; -agent-timeout 200ms should have cut the 5s sleep short", elapsed)
	}
	if len(rep.PerAgent) != 1 {
		t.Fatalf("perAgent=%d, want 1", len(rep.PerAgent))
	}
	a := rep.PerAgent[0]
	if a.OK {
		t.Fatal("timed-out agent must not be OK")
	}
	if !a.TimedOut {
		t.Fatal("want timedOut=true")
	}
	if a.Exit != -1 {
		t.Fatalf("exit=%d, want -1", a.Exit)
	}
	if !strings.Contains(a.Stderr, "agent-timeout") {
		t.Fatalf("stderr should note the timeout: %q", a.Stderr)
	}
	if a.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1 (no -agent-retries set)", a.Attempts)
	}
}

// TestDriveRunAgentRetries: -agent-retries re-runs a FAILED agent in a fresh
// worktree off the same base, up to N more times, and Attempts reports the
// total number of tries made. Cases: succeeds on the retry; the existing
// no-retries behavior is unchanged; a lane-strict out-of-lane failure is
// never retried (it's a plan violation, not a timing fluke a retry could
// fix) — both alone and combined with -keep-failed, where the stray's
// worktree must still be kept even though it stops on attempt 1 (the attempt
// that ends the loop, not the one -agent-retries would have predicted as
// "last"); and -keep-failed keeps only the LAST failed attempt's worktree,
// tearing every earlier one down.
func TestDriveRunAgentRetries(t *testing.T) {
	ctx := context.Background()
	badTask := taskSpec{ID: "bad", Prompt: "x"}

	// ---- fails on the first invocation, succeeds on the second ----
	_, repo := makeGoRepo(t)
	marker := filepath.Join(t.TempDir(), "attempt-marker")
	flakyAgent := "test -f '" + marker + "' && { echo done > out.txt; exit 0; } || { touch '" + marker + "'; exit 1; }"
	flakyTask := taskSpec{ID: "flaky", Prompt: "x"}
	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: flakyAgent,
		AgentRetries: 1, Autocommit: true,
	}
	rep, err := driveRun(ctx, p, []taskSpec{flakyTask})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	a := rep.PerAgent[0]
	if !a.OK {
		t.Fatalf("want the retried agent to succeed, got %+v", a)
	}
	if a.Attempts != 2 {
		t.Fatalf("attempts=%d, want 2 (failed once, succeeded on retry)", a.Attempts)
	}
	if !a.Autocommitted || !contains(a.Files, "out.txt") {
		t.Fatalf("want the successful retry's edit autocommitted: %+v", a)
	}

	// ---- -agent-retries 0 (default): today's behavior — a single failing
	// attempt fails outright, never retried ----
	_, repo0 := makeGoRepo(t)
	p0 := runParams{Repo: repo0, Base: "main", Strategy: "overlay", AgentCmd: "exit 1"}
	rep0, err := driveRun(ctx, p0, []taskSpec{badTask})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	a0 := rep0.PerAgent[0]
	if a0.OK {
		t.Fatal("want the agent to fail with no retries configured")
	}
	if a0.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1 (no -agent-retries set)", a0.Attempts)
	}

	// ---- lane-strict stray: a plan violation, never retried even though
	// -agent-retries is set ----
	_, repoStray := makeGoRepo(t)
	strayAgent := buildTestAgent(t)
	strayTask := taskSpec{ID: "stray", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{
			"a.go": "package main\n\nfunc a() int { return 1 }\n",
			"b.go": "package main\n\nfunc b() int { return 2 }\n",
		},
	}), Files: []string{"a.go"}}
	pStray := runParams{Repo: repoStray, Base: "main", Strategy: "overlay", AgentCmd: strayAgent, LaneMode: laneStrict, AgentRetries: 3}
	repStray, err := driveRun(ctx, pStray, []taskSpec{strayTask})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	aStray := repStray.PerAgent[0]
	if aStray.OK {
		t.Fatal("strict: out-of-lane agent must not be OK")
	}
	if aStray.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1 (a lane-strict stray is never retried)", aStray.Attempts)
	}

	// ---- -keep-failed + -agent-retries>0 + lane-strict stray: the stray stops
	// the retry loop on attempt 1, which IS the attempt that ends the loop, so
	// -keep-failed must still keep its worktree — even though a naive
	// "attempt > AgentRetries" check would (wrongly) call attempt 1 "not the
	// last attempt" since AgentRetries is 3 ----
	_, repoStrayKF := makeGoRepo(t)
	tmpStrayKF := t.TempDir()
	t.Setenv("TMPDIR", tmpStrayKF)
	strayAgentKF := buildTestAgent(t)
	strayTaskKF := taskSpec{ID: "stray", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{
			"a.go": "package main\n\nfunc a() int { return 1 }\n",
			"b.go": "package main\n\nfunc b() int { return 2 }\n",
		},
	}), Files: []string{"a.go"}}
	pStrayKF := runParams{Repo: repoStrayKF, Base: "main", Strategy: "overlay", AgentCmd: strayAgentKF, LaneMode: laneStrict, AgentRetries: 3, KeepFailed: true}
	repStrayKF, err := driveRun(ctx, pStrayKF, []taskSpec{strayTaskKF})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	aStrayKF := repStrayKF.PerAgent[0]
	if aStrayKF.OK {
		t.Fatal("strict+keep-failed: out-of-lane agent must not be OK")
	}
	if aStrayKF.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1 (a lane-strict stray is never retried)", aStrayKF.Attempts)
	}
	if aStrayKF.WorktreeKept == "" {
		t.Fatal("strict+keep-failed+agent-retries: want worktreeKept set on the attempt that actually ends the loop")
	}
	if fi, err := os.Stat(aStrayKF.WorktreeKept); err != nil || !fi.IsDir() {
		t.Fatalf("kept worktree %s should still exist as a dir: %v", aStrayKF.WorktreeKept, err)
	}

	// ---- -keep-failed + -agent-retries: only the LAST failed attempt's
	// worktree survives; earlier ones are torn down as each attempt fails ----
	_, repoKF := makeGoRepo(t)
	tmpKF := t.TempDir()
	t.Setenv("TMPDIR", tmpKF)
	pKF := runParams{Repo: repoKF, Base: "main", Strategy: "overlay", AgentCmd: "exit 1", AgentRetries: 2, KeepFailed: true}
	repKF, err := driveRun(ctx, pKF, []taskSpec{badTask})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	aKF := repKF.PerAgent[0]
	if aKF.OK {
		t.Fatal("want the agent to still fail after exhausting retries")
	}
	if aKF.Attempts != 3 {
		t.Fatalf("attempts=%d, want 3 (1 + 2 retries)", aKF.Attempts)
	}
	if aKF.WorktreeKept == "" {
		t.Fatal("want worktreeKept set for the final failed attempt")
	}
	if fi, err := os.Stat(aKF.WorktreeKept); err != nil || !fi.IsDir() {
		t.Fatalf("kept worktree %s should still exist as a dir: %v", aKF.WorktreeKept, err)
	}
	runRootEntries, err := os.ReadDir(tmpKF)
	if err != nil {
		t.Fatal(err)
	}
	if len(runRootEntries) != 1 {
		t.Fatalf("want exactly one sig-run-* root under %s, found %v", tmpKF, runRootEntries)
	}
	wtEntries, err := os.ReadDir(filepath.Join(tmpKF, runRootEntries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if len(wtEntries) != 1 {
		t.Fatalf("want exactly one surviving worktree dir (only the last failed attempt kept), found %v", wtEntries)
	}
}

// gitOut runs a git plumbing command directly against repo (bypassing gitx,
// which has no exported branch-creation call) and returns trimmed stdout.
func gitOut(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestDriveRunAgentRetriesPreExistingBranchSurvives: a foreign agent/<id>
// branch left over from a PRIOR run (holding a real, committed user commit)
// must survive a run that reuses that same task ID with -agent-retries set.
// Attempt 1's WorktreeAdd loud-fails on the collision (see WorktreeAdd's
// doc comment); that must be terminal — never retried into a
// WorktreeAddReset that would -B-reset the foreign branch and silently
// destroy the prior commit. This is the regression case for the bug where
// the retry gate was keyed off `attempt >= 2` instead of "did THIS run
// create the branch": that condition can't tell a foreign pre-existing
// branch apart from one this run made itself on an earlier attempt.
func TestDriveRunAgentRetriesPreExistingBranchSurvives(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)

	baseSHA := gitOut(t, repo, "rev-parse", "HEAD")
	tree := gitOut(t, repo, "rev-parse", baseSHA+"^{tree}")
	// A distinct commit (new SHA, same tree) simulating a prior run's landed
	// work, reachable only through agent/collide.
	// -c ident flags: commit-tree needs an identity, and CI runners have no
	// global git config (gitx sets ident via env, but this is a raw git call).
	userSHA := gitOut(t, repo, "-c", "user.name=test", "-c", "user.email=test@local",
		"commit-tree", tree, "-p", baseSHA, "-m", "user work from a prior run")
	gitOut(t, repo, "update-ref", "refs/heads/agent/collide", userSHA)

	task := taskSpec{ID: "collide", Prompt: "x"}
	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: "exit 0", AgentRetries: 2}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	a := rep.PerAgent[0]
	if a.OK {
		t.Fatal("want the agent to fail: agent/collide collides with a foreign branch")
	}
	if a.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1 (a WorktreeAdd collision is terminal, never retried)", a.Attempts)
	}
	if !strings.Contains(a.Stderr, "worktree add") {
		t.Fatalf("want the worktree-add failure reported in stderr, got %q", a.Stderr)
	}
	if got := gitOut(t, repo, "rev-parse", "refs/heads/agent/collide"); got != userSHA {
		t.Fatalf("agent/collide moved from %s to %s: the foreign branch's commit was destroyed", userSHA, got)
	}
}

// TestDriveRunBudgetExhausted: -budget caps the whole run (agent phase +
// integrate + verify) with one wall-clock ceiling. Two tasks share one -agent
// command that routes on SIGBOUND_TASK_ID: "fast" commits immediately via the
// deterministic test agent, "slow" sleeps far longer than the budget. Once
// the budget fires, the slow agent is cancelled (fails) and ctx is already
// expired by the time driveRun reaches integrate/land, so those git calls
// can't complete either — driveRun returns an honest operational error
// naming the budget rather than landing a partial tree. (The alternative —
// completing integration inside a leftover grace period — would need a
// second, ungated context and contradicts what -budget promises; see
// driveRun's -budget comment. This is the "pick the achievable assertion"
// case called out in the -budget design.)
func TestDriveRunBudgetExhausted(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	tasks := []taskSpec{
		{ID: "fast", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"fast.go": "package main\n\nfunc fast() int { return 1 }\n"},
		})},
		{ID: "slow", Prompt: "x"},
	}
	// -agent is one command for every task; route on SIGBOUND_TASK_ID so "slow"
	// sleeps while "fast" runs the real committing test agent.
	agentCmd := `if [ "$SIGBOUND_TASK_ID" = "slow" ]; then sleep 6; else ` + agent + `; fi`

	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agentCmd, Budget: 900 * time.Millisecond}
	start := time.Now()
	rep, err := driveRun(ctx, p, tasks)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("driveRun took %s; -budget 900ms should have cut the 6s sleep short", elapsed)
	}
	if err == nil {
		t.Fatalf("want an honest operational error once the budget is exhausted, got a clean report: %+v", rep)
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("error should name the exhausted budget: %v", err)
	}

	byID := map[string]perAgentJSON{}
	for _, a := range rep.PerAgent {
		byID[a.ID] = a
	}
	if !byID["fast"].OK {
		t.Fatalf("fast agent should have finished well inside the budget: %+v", byID["fast"])
	}
	if byID["slow"].OK {
		t.Fatal("slow agent should have been cancelled by the budget")
	}

	// The budget's honest failure must never leave a partial tree landed.
	mainSHA, err2 := g.RevParse(ctx, "main")
	if err2 != nil {
		t.Fatal(err2)
	}
	if mainSHA != rep.BaseSHA {
		t.Fatalf("main=%s advanced past baseSHA=%s despite the budget failing the run", mainSHA, rep.BaseSHA)
	}
}

// TestDriveRunVerifyKilledByBudget: when -budget expires while -verify itself
// is running (agent phase + integrate already finished, unlike
// TestDriveRunBudgetExhausted above where the budget kills an agent), verify
// naturally fails because its command gets killed along with ctx — but
// without help that reads as an ordinary verify failure, hiding the real
// cause. driveRun must name the exhausted budget in the verify output.
func TestDriveRunVerifyKilledByBudget(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	task := taskSpec{ID: "noop", Prompt: "x"} // agent does nothing; nothing needs to land for verify to run

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: "exit 0",
		VerifyCmd: "sleep 5", Budget: 700 * time.Millisecond,
	}
	start := time.Now()
	rep, err := driveRun(ctx, p, []taskSpec{task})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("driveRun: %v (verify failing is a report-level outcome, not an operational error)", err)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("driveRun took %s; -budget 700ms should have cut the 5s verify sleep short", elapsed)
	}
	if !rep.Verify.Ran || rep.Verify.OK {
		t.Fatalf("want verify to have run and failed once the budget killed it, got %+v", rep.Verify)
	}
	if !strings.Contains(rep.Verify.Output, "budget") {
		t.Fatalf("verify output should name the exhausted budget instead of reading as a plain failure, got: %q", rep.Verify.Output)
	}
}

// overlapDetectorAgent returns an -agent command that proves whether two
// invocations of it ran CONCURRENTLY: each invocation atomically mkdir's a
// shared "running" marker directory (mkdir is POSIX-atomic -- no flock
// needed); an invocation that finds the marker already held (another
// invocation still has it) records an overlap before releasing it. Used to
// prove -parallel-agents actually gates driveRun's fan-out semaphore
// (issue #84), not just a value threaded through and ignored: strictly
// serial execution can never observe the marker held, concurrent execution
// can. The overlap file exists iff at least one overlap was recorded.
func overlapDetectorAgent(t *testing.T) (agentCmd, overlapFile string) {
	t.Helper()
	dir := t.TempDir()
	running := filepath.Join(dir, "running")
	overlapFile = filepath.Join(dir, "overlap")
	agentCmd = fmt.Sprintf(`if mkdir %q 2>/dev/null; then :; else echo overlap >> %q; fi; sleep 0.2; rmdir %q 2>/dev/null; exit 0`, running, overlapFile, running)
	return agentCmd, overlapFile
}

// TestDriveRunParallelAgentsCapsFanOutConcurrency proves -parallel-agents
// (runParams.ParallelAgents) actually bounds driveRun's fan-out semaphore.
// At ParallelAgents=1 the fan-out loop itself blocks on the semaphore before
// a second agent's goroutine can even start (see driveRun), so overlap is
// IMPOSSIBLE, not just unlikely; at ParallelAgents=4 all four of this test's
// tasks launch at once, so overlap is expected well within the 200ms sleep
// window overlapDetectorAgent uses.
func TestDriveRunParallelAgentsCapsFanOutConcurrency(t *testing.T) {
	overlapped := func(t *testing.T, parallelAgents int) bool {
		_, repo := makeGoRepo(t)
		agentCmd, overlapFile := overlapDetectorAgent(t)
		tasks := []taskSpec{{ID: "a", Prompt: "x"}, {ID: "b", Prompt: "x"}, {ID: "c", Prompt: "x"}, {ID: "d", Prompt: "x"}}
		p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agentCmd, ParallelAgents: parallelAgents}
		if _, err := driveRun(context.Background(), p, tasks); err != nil {
			t.Fatalf("driveRun: %v", err)
		}
		_, err := os.Stat(overlapFile)
		return err == nil
	}

	t.Run("parallelAgents=1 runs strictly serially", func(t *testing.T) {
		if overlapped(t, 1) {
			t.Fatal("overlap detected at -parallel-agents 1; agents must run strictly one at a time")
		}
	})
	t.Run("parallelAgents=4 allows concurrent agents", func(t *testing.T) {
		if !overlapped(t, 4) {
			t.Fatal("no overlap detected at -parallel-agents 4; want concurrent execution")
		}
	})
}

// TestRunRunParallelAgentsRejectsNegative: -parallel-agents must reject a
// negative value before any agent runs, same fail-safe posture as every
// other flag validation in runRun (e.g. -semantic, -lanes).
func TestRunRunParallelAgentsRejectsNegative(t *testing.T) {
	_, repo := makeGoRepo(t)
	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo, "-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", "true", "-parallel-agents", "-1",
	})
	if err == nil || !strings.Contains(err.Error(), "-parallel-agents") {
		t.Fatalf("err=%v, want a -parallel-agents complaint", err)
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("report=%q, want no report written (nothing should have run)", buf.String())
	}
}

// TestRunRunParallelAgentsThreadsIntoReportAndOmitsAtDefault: -parallel-agents
// N reaches report.parallelAgents (the CONFIGURED value, not the resolved
// GOMAXPROCS-derived cap); a run WITHOUT the flag reports byte-identical to
// before -parallel-agents existed (the field is omitted, not 0).
func TestRunRunParallelAgentsThreadsIntoReportAndOmitsAtDefault(t *testing.T) {
	run := func(extraArgs ...string) string {
		_, repo := makeGoRepo(t)
		var buf bytes.Buffer
		args := append([]string{
			"-repo", repo, "-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
			"-agent", writeFileAgent("a.txt"), "-json",
		}, extraArgs...)
		code, err := runRun(&buf, args)
		if err != nil {
			t.Fatalf("runRun: %v\n%s", err, buf.String())
		}
		if code != exitOK {
			t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
		}
		return buf.String()
	}

	withFlag := run("-parallel-agents", "2")
	var repWith runReport
	if err := json.Unmarshal([]byte(withFlag), &repWith); err != nil {
		t.Fatalf("parse report: %v\n%s", err, withFlag)
	}
	if repWith.ParallelAgents != 2 {
		t.Fatalf("parallelAgents=%d, want 2", repWith.ParallelAgents)
	}

	without := run()
	if strings.Contains(without, `"parallelAgents"`) {
		t.Fatalf("report includes parallelAgents at the default, want omitted (byte-identical to before this flag existed): %s", without)
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
		{
			name: "clean landed, publish succeeded",
			rep: runReport{
				PerAgent: []perAgentJSON{{OK: true}},
				Publish:  &publishJSON{Ran: true, OK: true},
			},
			want: exitOK,
		},
		{
			name: "publish failed, otherwise clean",
			rep: runReport{
				PerAgent: []perAgentJSON{{OK: true}},
				Verify:   verifyJSON{Ran: true, OK: true},
				Publish:  &publishJSON{Ran: true, OK: false, Exit: 1},
			},
			want: exitPublishFailed,
		},
		{
			name: "flagged wins over publish failed",
			rep: runReport{
				PerAgent:  []perAgentJSON{{OK: true}, {OK: true}},
				Integrate: integrateJSON{Flagged: []flaggedJSON{{Branch: "agent/x"}}},
				Publish:   &publishJSON{Ran: true, OK: false},
			},
			want: exitFlagged,
		},
		{
			name: "verify failed wins over publish failed",
			rep: runReport{
				PerAgent: []perAgentJSON{{OK: true}},
				Verify:   verifyJSON{Ran: true, OK: false},
				Publish:  &publishJSON{Ran: true, OK: false},
			},
			want: exitVerifyFailed,
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
// that fails on its first invocation and passes on every one after. Retries
// reuse the SAME detached worktree (see runVerify/verifyWithRepair), only
// cleaned between invocations, so the fail-once state has to live outside the
// checkout: a marker file at a fixed path, created by the command itself on
// its first (failing) run — a marker written INSIDE the checkout would be
// removed by the between-attempt clean, defeating this fixture on purpose
// (see TestDriveRunVerifyRetriesCleansWorktreeBetweenAttempts for that case).
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
	if rep1.Verify.Invocations != 2 {
		t.Fatalf("verify.invocations=%d, want 2 (the failing first try + the retry that passed)", rep1.Verify.Invocations)
	}
	if rep1.Verify.WallMs <= 0 {
		t.Fatalf("verify.wallMs=%d, want > 0 (two real command invocations)", rep1.Verify.WallMs)
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
	if rep0.Verify.Invocations != 1 {
		t.Fatalf("verify.invocations=%d, want 1 (no retry configured)", rep0.Verify.Invocations)
	}
	if code := runExitCode(rep0); code != exitVerifyFailed {
		t.Fatalf("runExitCode=%d, want exitVerifyFailed", code)
	}
}

// TestDriveRunVerifyRetriesCleansWorktreeBetweenAttempts proves the
// hermeticity guarantee that makes worktree reuse across -verify-retries safe
// (see runVerify's reset+clean-on-entry): an UNTRACKED file the verify
// command writes INSIDE the checkout on its first (failing) invocation must
// be gone by the second (retried) invocation, AND a MODIFICATION it makes to
// a file git already TRACKS (shared.txt, part of makeGoRepo's base commit)
// must be reverted to its committed content — if the worktree were reused
// WITHOUT a clean+reset between attempts, both would still be there and the
// command reports LEAKED-ARTIFACT / LEAKED-MUTATION and fails; since neither
// is, the retry sees a fully hermetic tree and passes.
func TestDriveRunVerifyRetriesCleansWorktreeBetweenAttempts(t *testing.T) {
	ctx := context.Background()
	agent := buildTestAgent(t)
	task := taskSpec{ID: "ok", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"good.go": "package main\n\nfunc good() int { return 1 }\n"},
	})}

	// counter (outside the worktree) distinguishes the first invocation from
	// the retry; leftover.txt (inside the worktree, untracked) is the
	// artifact whose survival would prove an untracked leak, and the
	// "MUTATED" line appended to the tracked shared.txt is what would prove a
	// tracked-file leak.
	counter := filepath.Join(t.TempDir(), "counter")
	verifyCmd := `n=$(cat '` + counter + `' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '` + counter + `'
if [ "$n" = "1" ]; then touch leftover.txt; echo MUTATED >> shared.txt; exit 1; fi
if [ -f leftover.txt ]; then echo LEAKED-ARTIFACT; exit 1; fi
if grep -q MUTATED shared.txt; then echo LEAKED-MUTATION; exit 1; fi
exit 0`

	_, repo := makeGoRepo(t)
	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:     verifyCmd,
		VerifyRetries: 1,
	}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if strings.Contains(rep.Verify.Output, "LEAKED-ARTIFACT") {
		t.Fatalf("worktree reuse leaked an untracked artifact across -verify-retries: %q", rep.Verify.Output)
	}
	if strings.Contains(rep.Verify.Output, "LEAKED-MUTATION") {
		t.Fatalf("worktree reuse leaked a tracked-file mutation across -verify-retries: %q", rep.Verify.Output)
	}
	if !rep.Verify.OK {
		t.Fatalf("verify.ok=false, want true (retry should see a reset+cleaned, hermetic worktree): %q", rep.Verify.Output)
	}
	if !rep.Verify.Flaky {
		t.Fatal("verify.flaky=false, want true (passed only on the 2nd invocation)")
	}
}

// TestDriveRunRepairReVerifyStartsPristine proves the OTHER half of worktree-
// reuse hermeticity, on the repair path: -verify's reset+clean-on-entry
// guards retries on an UNCHANGED head, but when a repair round advances the
// reused worktree onto a NEW head via CheckoutDetach — which only touches
// paths that actually differ between the OLD and NEW commit (see its doc) —
// a tracked-file mutation left on a path repair never touched would ride
// straight through unreverted unless the worktree is reset+cleaned BEFORE
// that checkout, not just before the next verify command runs. The verify
// command here mutates a tracked file (shared.txt) and leaves an untracked
// leftover only while the build itself is broken, then fails LOUDLY if that
// mutation is still visible once the build is fixed — so a green re-verify
// after repair proves the checkout started from a pristine tree.
func TestDriveRunRepairReVerifyStartsPristine(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	verifyCmd := `if go build ./...; then
  if grep -q MUTATED-SHARED shared.txt; then echo MUTATION-LEAKED; exit 1; fi
  exit 0
fi
echo MUTATED-SHARED >> shared.txt
touch leftover.txt
exit 1`

	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		VerifyCmd: verifyCmd,
		// Deterministic fixer, same as TestDriveRunRepairSucceeds: defines the
		// missing helper() so the SECOND (post-repair) invocation's `go build`
		// passes and reaches the mutation check above.
		RepairCmd: `printf 'package main\n\nfunc helper() int { return helperX() }\n' > repair_fix.go`,
		RepairMax: 1,
	}

	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if strings.Contains(rep.Verify.Output, "MUTATION-LEAKED") {
		t.Fatalf("re-verify after repair saw a tracked-file mutation left by the pre-repair attempt: %q", rep.Verify.Output)
	}
	if !rep.Verify.OK {
		t.Fatalf("verify.ok=false after repair, want true (re-verify should have started from a pristine tree): %q", rep.Verify.Output)
	}
	if !rep.Verify.Repaired {
		t.Fatal("repaired=false; expected the repair loop to have fixed the initial build failure")
	}
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != rep.Integrate.FinalSHA {
		t.Fatalf("main=%s not advanced to repaired finalSHA=%s", mainSHA, rep.Integrate.FinalSHA)
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
	// One repair round -> two -verify invocations total: the initial failing
	// attempt plus the post-repair re-verify that passed.
	if v.Invocations != 2 {
		t.Fatalf("verify.invocations=%d, want 2 (initial fail + post-repair re-verify)", v.Invocations)
	}
	if v.WallMs <= 0 {
		t.Fatalf("verify.wallMs=%d, want > 0", v.WallMs)
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

// disjointGroupTasks returns n tasks each writing its OWN distinct file
// (g0.txt..g{n-1}.txt), so the integrator partitions them into n disjoint
// groups — the fixture -verify-bisect operates on. The verify command in these
// tests keys purely on which of those files are present in the candidate
// checkout, so a group is "broken" exactly when its file is present.
func disjointGroupTasks(t *testing.T, n int) []taskSpec {
	t.Helper()
	tasks := make([]taskSpec, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("g%d", i)
		tasks[i] = taskSpec{ID: name, Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{name + ".txt": name + "\n"},
		})}
	}
	return tasks
}

// TestDriveRunVerifyBisectLandsGreenSubset: three disjoint groups, one of which
// (g2) breaks verify. With -verify-bisect the driver must land the two green
// groups' union (base advances to a tree with g0.txt+g1.txt but NOT g2.txt),
// report g2 as dropped-by-bisect (never flagged), and exit 0.
func TestDriveRunVerifyBisectLandsGreenSubset(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		// Broken iff g2.txt is present in the candidate checkout.
		VerifyCmd:    `if [ -f g2.txt ]; then exit 1; fi; exit 0`,
		VerifyBisect: true,
	}
	rep, err := driveRun(ctx, p, disjointGroupTasks(t, 3))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}

	if !rep.Verify.OK {
		t.Fatalf("verify.ok=false; bisect should have salvaged a green subset: %q", rep.Verify.Output)
	}
	b := rep.Verify.Bisect
	if b == nil || !b.Ran {
		t.Fatal("verify.bisect missing; expected a bisect record")
	}
	if len(b.LandedGroups) != 2 || len(b.DroppedGroups) != 1 {
		t.Fatalf("bisect landed=%v dropped=%v, want 2 landed / 1 dropped", b.LandedGroups, b.DroppedGroups)
	}
	if len(b.DroppedGroups[0]) != 1 || b.DroppedGroups[0][0] != "agent/g2" {
		t.Fatalf("dropped group=%v, want [agent/g2]", b.DroppedGroups)
	}
	// Landed/flagged accounting reflects the FINAL subset; g2 is dropped, not flagged.
	if got := rep.Integrate.Landed; len(got) != 2 || !contains(got, "agent/g0") || !contains(got, "agent/g1") {
		t.Fatalf("integrate.landed=%v, want [agent/g0 agent/g1]", got)
	}
	if got := rep.Integrate.DroppedByBisect; len(got) != 1 || got[0] != "agent/g2" {
		t.Fatalf("integrate.droppedByBisect=%v, want [agent/g2]", got)
	}
	if len(rep.Integrate.Flagged) != 0 {
		t.Fatalf("integrate.flagged=%v, want none (a dropped group is not a conflict)", rep.Integrate.Flagged)
	}
	// Base advanced to the verified union tree: g0.txt+g1.txt present, g2.txt absent.
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != rep.Integrate.FinalSHA {
		t.Fatalf("main=%s not advanced to bisected finalSHA=%s", mainSHA, rep.Integrate.FinalSHA)
	}
	paths, err := g.LsTree(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(paths, "g0.txt") || !contains(paths, "g1.txt") {
		t.Fatalf("landed tree missing a green group's file (have %v)", paths)
	}
	if contains(paths, "g2.txt") {
		t.Fatalf("landed tree contains the dropped group's g2.txt (have %v)", paths)
	}
	if code := runExitCode(rep); code != exitOK {
		t.Fatalf("runExitCode=%d, want exitOK(0) on a bisect that landed a green subset", code)
	}
}

// TestDriveRunVerifyBisectLandsNothingWhenAllBroken: every group fails verify on
// its own, so bisect can salvage NOTHING — the base must not advance and the run
// keeps exit 3.
func TestDriveRunVerifyBisectLandsNothingWhenAllBroken(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	before, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:    `exit 1`, // every candidate tree fails
		VerifyBisect: true,
	}
	rep, err := driveRun(ctx, p, disjointGroupTasks(t, 3))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep.Verify.OK {
		t.Fatal("verify.ok=true, want false: no subset can pass when every group is broken")
	}
	b := rep.Verify.Bisect
	if b == nil || !b.Ran {
		t.Fatal("verify.bisect missing; bisect should still have run and reported nothing landable")
	}
	if len(b.LandedGroups) != 0 || len(b.DroppedGroups) != 3 {
		t.Fatalf("bisect landed=%v dropped=%v, want 0 landed / 3 dropped", b.LandedGroups, b.DroppedGroups)
	}
	if len(rep.Integrate.Landed) != 0 {
		t.Fatalf("integrate.landed=%v, want empty (nothing salvaged)", rep.Integrate.Landed)
	}
	if len(rep.Integrate.DroppedByBisect) != 3 {
		t.Fatalf("integrate.droppedByBisect=%v, want all 3 branches", rep.Integrate.DroppedByBisect)
	}
	after, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("base ref advanced to %s; must stay at %s (bisect salvaged nothing)", after, before)
	}
	if code := runExitCode(rep); code != exitVerifyFailed {
		t.Fatalf("runExitCode=%d, want exitVerifyFailed(%d)", code, exitVerifyFailed)
	}
}

// TestDriveRunVerifyBisectInteractionFailureLandsNothing: two groups each pass
// ALONE but their combined tree fails (verify fails iff BOTH files are present).
// The union re-verify — the exact tree that would land — catches it, so bisect
// honestly lands NOTHING rather than a subset it never verified green together.
func TestDriveRunVerifyBisectInteractionFailureLandsNothing(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	before, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		// Green with either file alone; red only when BOTH are present together.
		VerifyCmd:    `if [ -f g0.txt ] && [ -f g1.txt ]; then exit 1; fi; exit 0`,
		VerifyBisect: true,
	}
	rep, err := driveRun(ctx, p, disjointGroupTasks(t, 2))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep.Verify.OK {
		t.Fatalf("verify.ok=true; the union of the two groups fails, so nothing should land: %q", rep.Verify.Output)
	}
	b := rep.Verify.Bisect
	if b == nil || !b.Ran {
		t.Fatal("verify.bisect missing")
	}
	if len(b.LandedGroups) != 0 {
		t.Fatalf("bisect landed=%v, want none (individually-green groups still fail together)", b.LandedGroups)
	}
	if len(b.DroppedGroups) != 2 {
		t.Fatalf("bisect dropped=%v, want both groups", b.DroppedGroups)
	}
	after, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("base ref advanced to %s; must stay at %s (interaction failure lands nothing)", after, before)
	}
	if code := runExitCode(rep); code != exitVerifyFailed {
		t.Fatalf("runExitCode=%d, want exitVerifyFailed(%d)", code, exitVerifyFailed)
	}
}

// TestDriveRunVerifyBisectOffKeepsTodaysBehavior: the same broken batch as the
// salvage test but with -verify-bisect OFF — verify fails, nothing lands, no
// bisect record, exit 3. Guards that the seam is inert unless opted into.
func TestDriveRunVerifyBisectOffKeepsTodaysBehavior(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	before, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:    `if [ -f g2.txt ]; then exit 1; fi; exit 0`,
		VerifyBisect: false,
	}
	rep, err := driveRun(ctx, p, disjointGroupTasks(t, 3))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep.Verify.OK {
		t.Fatal("verify.ok=true on a broken batch with bisect off")
	}
	if rep.Verify.Bisect != nil {
		t.Fatalf("verify.bisect=%+v, want nil with -verify-bisect off", rep.Verify.Bisect)
	}
	if len(rep.Integrate.DroppedByBisect) != 0 {
		t.Fatalf("integrate.droppedByBisect=%v, want empty with bisect off", rep.Integrate.DroppedByBisect)
	}
	after, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("base ref advanced to %s; must stay at %s (bisect off, verify failed)", after, before)
	}
	if code := runExitCode(rep); code != exitVerifyFailed {
		t.Fatalf("runExitCode=%d, want exitVerifyFailed(%d)", code, exitVerifyFailed)
	}
}

// TestDriveRunVerifyBisectAfterRepair: -repair gets FIRST shot at the whole
// broken tree (it can't fix it — g2.txt stays present), THEN bisect salvages the
// two green groups. A marker file the fixer touches proves repair ran before
// bisect landed the subset.
func TestDriveRunVerifyBisectAfterRepair(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	marker := filepath.Join(t.TempDir(), "repair-ran")
	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd: `if [ -f g2.txt ]; then exit 1; fi; exit 0`,
		// The fixer proves it ran (touches an out-of-checkout marker) and edits a
		// file, but cannot remove g2.txt, so verify stays red and the loop gives up.
		RepairCmd:    `touch '` + marker + `'; printf 'x' > repair_note.txt`,
		RepairMax:    1,
		VerifyBisect: true,
	}
	rep, err := driveRun(ctx, p, disjointGroupTasks(t, 3))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("repair marker missing: repair must run BEFORE bisect: %v", err)
	}
	if len(rep.Verify.Repairs) == 0 {
		t.Fatal("no repair attempts recorded; -repair must get first shot at the whole tree")
	}
	b := rep.Verify.Bisect
	if b == nil || !b.Ran {
		t.Fatal("verify.bisect missing; bisect must run after repair exhausts")
	}
	if !rep.Verify.OK {
		t.Fatalf("verify.ok=false; bisect should have salvaged the green subset after repair failed: %q", rep.Verify.Output)
	}
	if len(b.LandedGroups) != 2 || len(b.DroppedGroups) != 1 {
		t.Fatalf("bisect landed=%v dropped=%v, want 2 landed / 1 dropped", b.LandedGroups, b.DroppedGroups)
	}
	// The landed tree is the ORIGINAL green group heads' union — the repair's
	// throwaway edits are discarded, g2.txt dropped.
	paths, err := g.LsTree(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(paths, "g0.txt") || !contains(paths, "g1.txt") {
		t.Fatalf("landed tree missing a green group's file (have %v)", paths)
	}
	if contains(paths, "g2.txt") || contains(paths, "repair_note.txt") {
		t.Fatalf("landed tree leaked dropped/repair content (have %v)", paths)
	}
	if code := runExitCode(rep); code != exitOK {
		t.Fatalf("runExitCode=%d, want exitOK(0)", code)
	}
}

// TestDriveRunVerifyBisectBinarySplitPath: 7 disjoint groups (k=7 >
// bisectAloneMax, so bisectGoodGroups takes the k>6 binary-split path, never
// each-alone) with exactly 2 broken groups — g5 and g6, both in the second half
// of the k=7 split. Drives the split path through a real driveRun: the salvage
// must land exactly the 5 green groups' union, report g5/g6 as DroppedByBisect
// (never Flagged), and exit 0.
//
// It also pins the exact bisect_attempt count this layout takes, traced
// straight from splitGood (k=7, mid=k/2=3):
//
//	splitGood([0,1,2])   -> whole subset green (no recursion)     : 1 probe
//	splitGood([3,4,5,6]) -> whole subset red, mid=2, recurse:
//	  splitGood([3,4])   -> whole subset green (no recursion)     : 1 probe
//	  splitGood([5,6])   -> whole subset red, mid=1, recurse:
//	    splitGood([5])   -> red leaf (len==1, no recursion)       : 1 probe
//	    splitGood([6])   -> red leaf (len==1, no recursion)       : 1 probe
//	                                                        (+1 for [5,6] itself)
//	                                                    (+1 for [3,4,5,6] itself)
//
// = 6 probes for the search itself — already fewer than the 7 an each-alone
// scan of 7 groups would need — plus 1 mandatory union re-verify of the final
// good set {0,1,2,3,4} = 7 bisect_attempt events overall.
func TestDriveRunVerifyBisectBinarySplitPath(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	eventsPath := filepath.Join(t.TempDir(), "events.ndjson")

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		// Broken iff g5.txt or g6.txt is present in the candidate checkout.
		VerifyCmd:    `if [ -f g5.txt ] || [ -f g6.txt ]; then exit 1; fi; exit 0`,
		VerifyBisect: true,
		EventsPath:   eventsPath,
	}
	rep, err := driveRun(ctx, p, disjointGroupTasks(t, 7))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}

	if !rep.Verify.OK {
		t.Fatalf("verify.ok=false; bisect should have salvaged a green subset: %q", rep.Verify.Output)
	}
	b := rep.Verify.Bisect
	if b == nil || !b.Ran {
		t.Fatal("verify.bisect missing; expected a bisect record")
	}
	if len(b.LandedGroups) != 5 || len(b.DroppedGroups) != 2 {
		t.Fatalf("bisect landed=%v dropped=%v, want 5 landed / 2 dropped", b.LandedGroups, b.DroppedGroups)
	}
	wantDropped := []string{"agent/g5", "agent/g6"}
	for i, grp := range b.DroppedGroups {
		if len(grp) != 1 || grp[0] != wantDropped[i] {
			t.Fatalf("bisect.droppedGroups=%v, want %v as singleton groups in order", b.DroppedGroups, wantDropped)
		}
	}
	// Landed/flagged accounting reflects the FINAL subset; g5/g6 are dropped, not flagged.
	wantLanded := []string{"agent/g0", "agent/g1", "agent/g2", "agent/g3", "agent/g4"}
	if got := rep.Integrate.Landed; len(got) != len(wantLanded) {
		t.Fatalf("integrate.landed=%v, want %v", got, wantLanded)
	}
	for _, want := range wantLanded {
		if !contains(rep.Integrate.Landed, want) {
			t.Fatalf("integrate.landed=%v missing %s", rep.Integrate.Landed, want)
		}
	}
	if got := rep.Integrate.DroppedByBisect; len(got) != 2 || got[0] != "agent/g5" || got[1] != "agent/g6" {
		t.Fatalf("integrate.droppedByBisect=%v, want [agent/g5 agent/g6]", got)
	}
	if len(rep.Integrate.Flagged) != 0 {
		t.Fatalf("integrate.flagged=%v, want none (a dropped group is not a conflict)", rep.Integrate.Flagged)
	}
	// Base advanced to the verified union tree: g0..g4 present, g5/g6 absent.
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != rep.Integrate.FinalSHA {
		t.Fatalf("main=%s not advanced to bisected finalSHA=%s", mainSHA, rep.Integrate.FinalSHA)
	}
	paths, err := g.LsTree(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"g0.txt", "g1.txt", "g2.txt", "g3.txt", "g4.txt"} {
		if !contains(paths, f) {
			t.Fatalf("landed tree missing green group file %s (have %v)", f, paths)
		}
	}
	for _, f := range []string{"g5.txt", "g6.txt"} {
		if contains(paths, f) {
			t.Fatalf("landed tree contains dropped group file %s (have %v)", f, paths)
		}
	}
	if code := runExitCode(rep); code != exitOK {
		t.Fatalf("runExitCode=%d, want exitOK(0) on a bisect that landed a green subset", code)
	}

	// bisect_attempt count pins the split path actually ran (see the trace in
	// the doc comment above) — 6 search probes + 1 final union re-verify.
	names, _ := readEvents(t, eventsPath)
	if got, want := countOf(names, "bisect_attempt"), 7; got != want {
		t.Fatalf("bisect_attempt count=%d, want %d (6 split-search probes + 1 union re-verify)", got, want)
	}
}

// TestDriveRunVerifyBisectMergetreeStrategy: the same 3-disjoint-groups/1-broken
// shape as TestDriveRunVerifyBisectLandsGreenSubset, but with -strategy
// mergetree (IntegrateOCC) instead of the default overlay. mergetree's combine
// phase is entirely different (a parallel pairwise merge-tree reduction instead
// of an object-store overlay), but its GroupHeads/GroupBranches bookkeeping is
// meant to be strategy-agnostic — this pins that bisect salvages identically
// under it: same landed/dropped accounting, same landed tree, same exit code.
func TestDriveRunVerifyBisectMergetreeStrategy(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	p := runParams{
		Repo: repo, Base: "main", Strategy: "mergetree", AgentCmd: agent,
		// Broken iff g2.txt is present in the candidate checkout.
		VerifyCmd:    `if [ -f g2.txt ]; then exit 1; fi; exit 0`,
		VerifyBisect: true,
	}
	rep, err := driveRun(ctx, p, disjointGroupTasks(t, 3))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}

	if rep.Integrate.Strategy != "mergetree" {
		t.Fatalf("integrate.strategy=%q, want mergetree (sanity check that this test actually exercised it)", rep.Integrate.Strategy)
	}
	if !rep.Verify.OK {
		t.Fatalf("verify.ok=false; bisect should have salvaged a green subset: %q", rep.Verify.Output)
	}
	b := rep.Verify.Bisect
	if b == nil || !b.Ran {
		t.Fatal("verify.bisect missing; expected a bisect record")
	}
	if len(b.LandedGroups) != 2 || len(b.DroppedGroups) != 1 {
		t.Fatalf("bisect landed=%v dropped=%v, want 2 landed / 1 dropped", b.LandedGroups, b.DroppedGroups)
	}
	if len(b.DroppedGroups[0]) != 1 || b.DroppedGroups[0][0] != "agent/g2" {
		t.Fatalf("dropped group=%v, want [agent/g2]", b.DroppedGroups)
	}
	// Landed/flagged accounting reflects the FINAL subset; g2 is dropped, not flagged.
	if got := rep.Integrate.Landed; len(got) != 2 || !contains(got, "agent/g0") || !contains(got, "agent/g1") {
		t.Fatalf("integrate.landed=%v, want [agent/g0 agent/g1]", got)
	}
	if got := rep.Integrate.DroppedByBisect; len(got) != 1 || got[0] != "agent/g2" {
		t.Fatalf("integrate.droppedByBisect=%v, want [agent/g2]", got)
	}
	if len(rep.Integrate.Flagged) != 0 {
		t.Fatalf("integrate.flagged=%v, want none (a dropped group is not a conflict)", rep.Integrate.Flagged)
	}
	// Base advanced to the verified union tree: g0.txt+g1.txt present, g2.txt absent.
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != rep.Integrate.FinalSHA {
		t.Fatalf("main=%s not advanced to bisected finalSHA=%s", mainSHA, rep.Integrate.FinalSHA)
	}
	paths, err := g.LsTree(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(paths, "g0.txt") || !contains(paths, "g1.txt") {
		t.Fatalf("landed tree missing a green group's file (have %v)", paths)
	}
	if contains(paths, "g2.txt") {
		t.Fatalf("landed tree contains the dropped group's g2.txt (have %v)", paths)
	}
	if code := runExitCode(rep); code != exitOK {
		t.Fatalf("runExitCode=%d, want exitOK(0) on a bisect that landed a green subset", code)
	}
}

// TestSplitGood exercises the k>6 binary-split search in isolation with a fake
// eval: groups whose index is in the `bad` set fail alone, and any subset
// containing a bad group fails (the additive-failure model — a broken file stays
// broken in any superset). splitGood must return exactly the good indices.
func TestSplitGood(t *testing.T) {
	cases := []struct {
		k    int
		bad  map[int]bool
		want []int
	}{
		{k: 8, bad: map[int]bool{3: true}, want: []int{0, 1, 2, 4, 5, 6, 7}},
		{k: 8, bad: map[int]bool{}, want: []int{0, 1, 2, 3, 4, 5, 6, 7}},
		{k: 8, bad: map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true}, want: nil},
		{k: 10, bad: map[int]bool{2: true, 7: true}, want: []int{0, 1, 3, 4, 5, 6, 8, 9}},
	}
	for _, tc := range cases {
		eval := func(subset []int) bool {
			for _, i := range subset {
				if tc.bad[i] {
					return false
				}
			}
			return true
		}
		got := bisectGoodGroups(eval, tc.k)
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", tc.want) {
			t.Errorf("bisectGoodGroups(k=%d, bad=%v)=%v, want %v", tc.k, tc.bad, got, tc.want)
		}
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

// TestDriveRunLogDirCapturesFullAgentOutput: with -logdir, an agent's FULL
// stdout+stderr (well beyond the 800-byte in-memory tail) lands in
// agent-<id>.log, while a.Stderr stays bounded exactly as it does without
// -logdir. The agent fails on purpose (exit 1) — the log path must work
// regardless of whether the agent ultimately lands.
func TestDriveRunLogDirCapturesFullAgentOutput(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	logDir := t.TempDir()

	// 3000 bytes of stderr (well past the 800-byte tail) plus a stdout marker
	// (stdout is normally discarded entirely; -logdir must still capture it).
	agentCmd := "printf 'stdout-marker\\n'; head -c 3000 /dev/zero | tr '\\0' 'X' >&2; exit 1"
	task := taskSpec{ID: "logtest", Prompt: "n/a"}

	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agentCmd, LogDir: logDir}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.PerAgent) != 1 || rep.PerAgent[0].OK {
		t.Fatalf("want one failed agent (exit 1), got %+v", rep.PerAgent)
	}
	a := rep.PerAgent[0]

	// In-memory capture stays bounded, same as without -logdir.
	if len(a.Stderr) > 803 { // 800 + "..." prefix
		t.Fatalf("a.Stderr not bounded: len=%d", len(a.Stderr))
	}

	logPath := filepath.Join(logDir, "agent-logtest.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read %s: %v", logPath, err)
	}
	if !strings.Contains(string(data), "stdout-marker") {
		t.Fatalf("%s missing stdout content (stdout is discarded without -logdir, must be captured with it):\n%.200s", logPath, data)
	}
	if got := strings.Count(string(data), "X"); got != 3000 {
		t.Fatalf("%s has %d X's, want the full 3000-byte stderr (not truncated)", logPath, got)
	}

	if rep.LogDir != logDir {
		t.Fatalf("rep.LogDir=%q, want %q", rep.LogDir, logDir)
	}
	var sumBuf bytes.Buffer
	if err := emitReport(&sumBuf, rep, false); err != nil {
		t.Fatalf("emitReport: %v", err)
	}
	if !strings.Contains(sumBuf.String(), "logs: "+logDir) {
		t.Fatalf("summary missing 'logs: %s' line:\n%s", logDir, sumBuf.String())
	}
}

// TestDriveRunLogDirVerifyAndRepairFiles exercises the full verify/repair loop
// with -logdir: an initial FAILING verify (verify-0.log), a repair attempt
// (repair-1.log), and the post-repair verify that passes (verify-1.log) must
// all appear under -logdir with the expected names, alongside each agent's log.
func TestDriveRunLogDirVerifyAndRepairFiles(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	logDir := t.TempDir()

	p := runParams{
		Repo:      repo,
		Base:      "main",
		Strategy:  "overlay",
		AgentCmd:  agent,
		VerifyCmd: "go build ./...",
		RepairCmd: `printf 'package main\n\nfunc helper() int { return helperX() }\n' > repair_fix.go`,
		RepairMax: 2,
		LogDir:    logDir,
	}

	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep.Verify.OK || !rep.Verify.Repaired {
		t.Fatalf("verify ok=%v repaired=%v, want the repair loop to fix it: %q", rep.Verify.OK, rep.Verify.Repaired, rep.Verify.Output)
	}

	// sig-testagent and the repair fixer here are both silent on success (they
	// only write/commit files), so only existence is asserted for their logs;
	// verify's failing output is checked for real content separately below.
	for _, name := range []string{
		"agent-caller.log", "agent-helper.log",
		"verify-0.log", // initial verify: FAILS (undefined: helper)
		"repair-1.log", // the fixer's own log
		"verify-1.log", // post-repair verify: PASSES
	} {
		path := filepath.Join(logDir, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected log file %s: %v", path, err)
		}
	}
	v0, err := os.ReadFile(filepath.Join(logDir, "verify-0.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(v0), "helper") {
		t.Fatalf("verify-0.log missing the build failure output:\n%s", v0)
	}
}

// TestRunRunLogDirUnwritableFailsBeforeAgents: an unwritable/uncreatable
// -logdir must fail the run BEFORE any agent runs — never silently drop the
// requested logs. Here -logdir names a path that is already a regular FILE,
// so os.MkdirAll cannot turn it into a directory; a chmod-based test would be
// unreliable when tests run as root.
func TestRunRunLogDirUnwritableFailsBeforeAgents(t *testing.T) {
	_, repo := makeGoRepo(t)
	marker := filepath.Join(t.TempDir(), "agent-ran")
	agentCmd := "touch " + marker

	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(blocked, "logs") // parent "blocked" is a file, not a dir

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-base", "main",
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", agentCmd,
		"-logdir", logDir,
	})
	if err == nil {
		t.Fatal("unwritable -logdir: want error, got nil")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("no report should be written when -logdir fails before any agent runs, got:\n%s", buf.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("agent ran despite an unwritable -logdir; must fail before any agent runs")
	}
}

// tasksFileFor writes tasks as a JSON -tasks file and returns its path.
func tasksFileFor(t *testing.T, tasks []taskSpec) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(f, []byte(mustJSON(t, tasks)), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
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

// countingVerifyCmd returns a -verify command that APPENDS a line to marker
// every time it actually runs, then exits with exit — so a test can prove a
// command ran (or, just as importantly, did NOT run because -verify-cache
// served a cached verdict instead) by counting marker's lines, independent
// of whether the invocation itself passed or failed.
func countingVerifyCmd(marker string, exit int) string {
	return fmt.Sprintf("echo ran >> '%s'; exit %d", marker, exit)
}

// countLines returns the number of newline-terminated lines in path, or 0 if
// it doesn't exist yet (a command that never ran leaves no marker at all).
func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	if len(data) == 0 {
		return 0
	}
	return len(strings.Split(strings.TrimRight(string(data), "\n"), "\n"))
}

// pinBranch creates ref, pointing at sha, alongside "main" — used below to
// give a second driveRun call the exact same starting tree as the first
// without reusing "main" itself (which the first run already advanced), so a
// differently-named task can independently reproduce a byte-identical
// landed tree via a different agent branch/commit.
func pinBranch(t *testing.T, ctx context.Context, g *gitx.Git, ref, sha string) {
	t.Helper()
	if err := g.UpdateRef(ctx, "refs/heads/"+ref, sha); err != nil {
		t.Fatal(err)
	}
}

// TestDriveRunVerifyCacheHitsOnIdenticalTree proves the whole point of
// -verify-cache (issue #18): two driveRun calls that land the EXACT SAME
// tree content via DIFFERENT agent branches/commits (a resume/replay
// scenario — main2 pins the same starting commit as main, and t2 writes the
// identical bytes t1 did) share one cache entry. The second run's -verify
// command must never even be spawned — proven by countingVerifyCmd's marker
// staying at one append, not two.
func TestDriveRunVerifyCacheHitsOnIdenticalTree(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	baseSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	pinBranch(t, ctx, g, "main2", baseSHA)

	write := map[string]string{"good.go": "package main\n\nfunc good() int { return 1 }\n"}
	marker := filepath.Join(t.TempDir(), "marker")

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:   countingVerifyCmd(marker, 0),
		VerifyCache: true,
	}
	rep1, err := driveRun(ctx, p, []taskSpec{{ID: "t1", Prompt: mustJSON(t, map[string]any{"write": write})}})
	if err != nil {
		t.Fatalf("driveRun #1: %v", err)
	}
	if !rep1.Verify.OK || rep1.Verify.Cached {
		t.Fatalf("run #1: ok=%v cached=%v, want a real (uncached) pass", rep1.Verify.OK, rep1.Verify.Cached)
	}
	if n := countLines(t, marker); n != 1 {
		t.Fatalf("verify command ran %d time(s) after run #1, want 1", n)
	}
	if rep1.Verify.Invocations != 1 {
		t.Fatalf("run #1: invocations=%d, want 1 (one real -verify invocation)", rep1.Verify.Invocations)
	}
	if rep1.Verify.WallMs <= 0 {
		t.Fatalf("run #1: wallMs=%d, want > 0 (a real -verify invocation ran)", rep1.Verify.WallMs)
	}

	p2 := p
	p2.Base = "main2"
	rep2, err := driveRun(ctx, p2, []taskSpec{{ID: "t2", Prompt: mustJSON(t, map[string]any{"write": write})}})
	if err != nil {
		t.Fatalf("driveRun #2: %v", err)
	}
	if !rep2.Verify.OK || !rep2.Verify.Cached {
		t.Fatalf("run #2: ok=%v cached=%v, want a CACHED pass (identical tree+command already verified by run #1)", rep2.Verify.OK, rep2.Verify.Cached)
	}
	if n := countLines(t, marker); n != 1 {
		t.Fatalf("verify command ran %d time(s) after run #2, want still 1 (a cache hit must skip the command entirely)", n)
	}
	// A cache hit never actually spawns -verify, so it must not be counted as
	// an invocation nor contribute wall time — see verifyJSON's doc comment
	// ("the total number of times the -verify command itself was ACTUALLY
	// run"). This is what flows into sig serve's usage.json verifyAttempts/
	// verifyWallMs (issue #61 metering); a cache hit must not overcount those.
	if rep2.Verify.Invocations != 0 {
		t.Fatalf("run #2 (cached): invocations=%d, want 0 (the -verify command was never spawned)", rep2.Verify.Invocations)
	}
	if rep2.Verify.WallMs != 0 {
		t.Fatalf("run #2 (cached): wallMs=%d, want 0 (no -verify command ran to spend wall time on)", rep2.Verify.WallMs)
	}

	// Confirm the premise the whole test rests on: the two runs really did
	// land byte-identical trees, from different branches/commits.
	tree1, err := g.TreeOID(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	tree2, err := g.TreeOID(ctx, "main2")
	if err != nil {
		t.Fatal(err)
	}
	if tree1 != tree2 {
		t.Fatalf("premise broken: main tree=%s, main2 tree=%s, want identical", tree1, tree2)
	}
}

// TestDriveRunVerifyCacheNeverCachesAFailure proves the fail-safe half of
// -verify-cache: a FAILING verdict is never written to the cache, so a
// second run over the identical (tree, command) pair still re-executes the
// command in full — never a false green, and never a stale red once
// whatever caused the failure might have been fixed out-of-band.
func TestDriveRunVerifyCacheNeverCachesAFailure(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	baseSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	pinBranch(t, ctx, g, "main2", baseSHA)

	write := map[string]string{"good.go": "package main\n\nfunc good() int { return 1 }\n"}
	marker := filepath.Join(t.TempDir(), "marker")

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:   countingVerifyCmd(marker, 1), // always fails
		VerifyCache: true,
	}
	rep1, err := driveRun(ctx, p, []taskSpec{{ID: "t1", Prompt: mustJSON(t, map[string]any{"write": write})}})
	if err != nil {
		t.Fatalf("driveRun #1: %v", err)
	}
	if rep1.Verify.OK {
		t.Fatal("run #1 unexpectedly passed")
	}
	if n := countLines(t, marker); n != 1 {
		t.Fatalf("verify command ran %d time(s) after run #1, want 1", n)
	}

	p2 := p
	p2.Base = "main2"
	rep2, err := driveRun(ctx, p2, []taskSpec{{ID: "t2", Prompt: mustJSON(t, map[string]any{"write": write})}})
	if err != nil {
		t.Fatalf("driveRun #2: %v", err)
	}
	if rep2.Verify.OK || rep2.Verify.Cached {
		t.Fatalf("run #2: ok=%v cached=%v, want a real (uncached) failure", rep2.Verify.OK, rep2.Verify.Cached)
	}
	if n := countLines(t, marker); n != 2 {
		t.Fatalf("verify command ran %d time(s) after run #2, want 2 (a failure must NEVER be served from cache)", n)
	}
}

// TestDriveRunVerifyCacheMissOnDifferentCommand proves the cache key folds
// in the resolved command: the exact same tree verified with a DIFFERENT
// -verify command must miss and actually run, never reuse another
// command's cache entry for that tree.
func TestDriveRunVerifyCacheMissOnDifferentCommand(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	baseSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	pinBranch(t, ctx, g, "main2", baseSHA)

	write := map[string]string{"good.go": "package main\n\nfunc good() int { return 1 }\n"}
	markerA := filepath.Join(t.TempDir(), "markerA")
	markerB := filepath.Join(t.TempDir(), "markerB")

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:   countingVerifyCmd(markerA, 0),
		VerifyCache: true,
	}
	rep1, err := driveRun(ctx, p, []taskSpec{{ID: "t1", Prompt: mustJSON(t, map[string]any{"write": write})}})
	if err != nil {
		t.Fatalf("driveRun #1: %v", err)
	}
	if !rep1.Verify.OK {
		t.Fatalf("run #1 failed: %s", rep1.Verify.Output)
	}

	p2 := p
	p2.Base = "main2"
	p2.VerifyCmd = countingVerifyCmd(markerB, 0) // same tree, DIFFERENT command string
	rep2, err := driveRun(ctx, p2, []taskSpec{{ID: "t2", Prompt: mustJSON(t, map[string]any{"write": write})}})
	if err != nil {
		t.Fatalf("driveRun #2: %v", err)
	}
	if !rep2.Verify.OK || rep2.Verify.Cached {
		t.Fatalf("run #2: ok=%v cached=%v, want a real (uncached) pass (a different command must never hit another command's cache entry)", rep2.Verify.OK, rep2.Verify.Cached)
	}
	if n := countLines(t, markerB); n != 1 {
		t.Fatalf("command B ran %d time(s), want 1", n)
	}
}

// TestDriveRunVerifyCacheMissOnDifferentTree proves the other half of the
// key: the SAME command over a DIFFERENT tree must also miss.
func TestDriveRunVerifyCacheMissOnDifferentTree(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	marker := filepath.Join(t.TempDir(), "marker")

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:   countingVerifyCmd(marker, 0),
		VerifyCache: true,
	}
	rep1, err := driveRun(ctx, p, []taskSpec{{ID: "t1", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}})
	if err != nil {
		t.Fatalf("driveRun #1: %v", err)
	}
	if !rep1.Verify.OK {
		t.Fatalf("run #1 failed: %s", rep1.Verify.Output)
	}

	// Run #2 lands on top of run #1's already-advanced main, with DIFFERENT
	// file content: a genuinely different tree, same -verify command.
	rep2, err := driveRun(ctx, p, []taskSpec{{ID: "t2", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"b.go": "package main\n\nfunc b() int { return 2 }\n"},
	})}})
	if err != nil {
		t.Fatalf("driveRun #2: %v", err)
	}
	if !rep2.Verify.OK || rep2.Verify.Cached {
		t.Fatalf("run #2: ok=%v cached=%v, want a real (uncached) pass (a different tree must never hit run #1's entry)", rep2.Verify.OK, rep2.Verify.Cached)
	}
	if n := countLines(t, marker); n != 2 {
		t.Fatalf("verify command ran %d time(s), want 2", n)
	}
}

// TestDriveRunVerifyCacheOffNeverConsulted proves -verify-cache defaults off
// and, while off, the cache is consulted NOT AT ALL: two runs over the
// identical (tree, command) pair both execute the command for real, and no
// cache directory is ever created.
func TestDriveRunVerifyCacheOffNeverConsulted(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	baseSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	pinBranch(t, ctx, g, "main2", baseSHA)

	write := map[string]string{"good.go": "package main\n\nfunc good() int { return 1 }\n"}
	marker := filepath.Join(t.TempDir(), "marker")

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd: countingVerifyCmd(marker, 0),
		// VerifyCache deliberately left at its zero value (false, the default).
	}
	rep1, err := driveRun(ctx, p, []taskSpec{{ID: "t1", Prompt: mustJSON(t, map[string]any{"write": write})}})
	if err != nil {
		t.Fatalf("driveRun #1: %v", err)
	}
	if !rep1.Verify.OK || rep1.Verify.Cached {
		t.Fatalf("run #1: ok=%v cached=%v", rep1.Verify.OK, rep1.Verify.Cached)
	}

	p2 := p
	p2.Base = "main2"
	rep2, err := driveRun(ctx, p2, []taskSpec{{ID: "t2", Prompt: mustJSON(t, map[string]any{"write": write})}})
	if err != nil {
		t.Fatalf("driveRun #2: %v", err)
	}
	if !rep2.Verify.OK || rep2.Verify.Cached {
		t.Fatalf("run #2: ok=%v cached=%v, want a real pass with -verify-cache off, never cached=true", rep2.Verify.OK, rep2.Verify.Cached)
	}
	if n := countLines(t, marker); n != 2 {
		t.Fatalf("verify command ran %d time(s), want 2 (an identical tree+command must never be skipped with -verify-cache off)", n)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "sigbound")); !os.IsNotExist(err) {
		t.Fatalf(".git/sigbound exists with -verify-cache off (err=%v), want no cache directory ever created", err)
	}
}

// TestDriveRunVerifyCacheMissAfterRepairAdvancesTree proves a repair round's
// NEW head is naturally a different cache key (a different tree), never
// mistaken for the pre-repair tree it grew from: with -verify-cache on, the
// initial (broken) attempt and the post-repair re-verify BOTH actually
// execute -verify — the repaired tree has never been seen before, so it can
// only ever be a miss, and the report must not claim it was cached.
func TestDriveRunVerifyCacheMissAfterRepairAdvancesTree(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	marker := filepath.Join(t.TempDir(), "marker")

	p := runParams{
		Repo:        repo,
		Base:        "main",
		Strategy:    "overlay",
		AgentCmd:    agent,
		VerifyCmd:   fmt.Sprintf("echo ran >> '%s'; go build ./...", marker),
		RepairCmd:   `printf 'package main\n\nfunc helper() int { return helperX() }\n' > repair_fix.go`,
		RepairMax:   2,
		VerifyCache: true,
	}

	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep.Verify.OK {
		t.Fatalf("verify.ok=false after repair; output=%q", rep.Verify.Output)
	}
	if !rep.Verify.Repaired {
		t.Fatal("repaired=false; expected the repair loop to have fixed the initial build failure")
	}
	if rep.Verify.Cached {
		t.Fatal("cached=true on a tree never seen before (the repaired tree); a repair-advanced tree must always miss")
	}
	if n := countLines(t, marker); n != 2 {
		t.Fatalf("-verify command ran %d time(s), want 2 (pre-repair attempt + post-repair re-verify, neither served from cache)", n)
	}
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != rep.Integrate.FinalSHA {
		t.Fatalf("main=%s not advanced to repaired finalSHA=%s", mainSHA, rep.Integrate.FinalSHA)
	}
}

// openReadOnly creates an empty file at path and reopens it O_RDONLY, so every
// Write() to the returned handle fails immediately (EBADF) — a real,
// portable, permission-independent way to make an already-successfully-opened
// file fail every write, used below to reproduce the exact -logdir failure
// mode bestEffortWriter guards against.
func openReadOnly(t *testing.T, path string) *os.File {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// TestBestEffortWriterSwallowsErrors is the direct unit test of the seam: a
// real failing writer (a file opened O_RDONLY) must never make
// bestEffortWriter.Write return an error or a short count.
func TestBestEffortWriterSwallowsErrors(t *testing.T) {
	f := openReadOnly(t, filepath.Join(t.TempDir(), "readonly.log"))

	// Confirm the premise: writing to an O_RDONLY file really does fail.
	if _, err := f.Write([]byte("x")); err == nil {
		t.Fatal("premise broken: write to an O_RDONLY file succeeded")
	}

	w := bestEffortWriter{f}
	n, err := w.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("bestEffortWriter.Write returned error %v, want nil (must swallow)", err)
	}
	if n != len("hello") {
		t.Fatalf("bestEffortWriter.Write returned n=%d, want %d", n, len("hello"))
	}
}

// TestBestEffortWriterKeepsCommandRunCleanOnLogWriteFailure reproduces the
// exact os/exec behavior the CRITICAL finding is about: as soon as
// cmd.Stdout is anything other than a bare *os.File, exec services it via a
// pipe + copy-goroutine, and that goroutine's write error is promoted into
// cmd.Run()'s returned error even though the child process itself exited 0.
// The "control" subtest confirms the bug is real without the fix; the
// "fixed" subtest confirms bestEffortWriter neutralizes it while still
// capturing the real output.
func TestBestEffortWriterKeepsCommandRunCleanOnLogWriteFailure(t *testing.T) {
	t.Run("control: unwrapped failing writer fails a clean exit", func(t *testing.T) {
		f := openReadOnly(t, filepath.Join(t.TempDir(), "readonly.log"))
		var buf bytes.Buffer
		cmd := exec.Command("sh", "-c", "echo hello")
		cmd.Stdout = io.MultiWriter(&buf, f)
		if err := cmd.Run(); err == nil {
			t.Fatal("premise broken: expected the log-write failure to surface as a Run() error " +
				"(the os/exec behavior bestEffortWriter works around) — got nil")
		}
	})

	t.Run("fixed: bestEffortWriter keeps a clean exit clean", func(t *testing.T) {
		f := openReadOnly(t, filepath.Join(t.TempDir(), "readonly.log"))
		var buf bytes.Buffer
		cmd := exec.Command("sh", "-c", "echo hello")
		cmd.Stdout = io.MultiWriter(&buf, bestEffortWriter{f})
		if err := cmd.Run(); err != nil {
			t.Fatalf("Run() = %v, want nil — a failing -logdir write must never fail the command", err)
		}
		if got := strings.TrimSpace(buf.String()); got != "hello" {
			t.Fatalf("captured output = %q, want %q (real output must still land)", got, "hello")
		}
	})
}

// TestDriveRunAgentSurvivesLogWriteFailure is the agent-level counterpart:
// with -logdir wired to a log file that fails every write (forced via
// openLogFile, the seam openLog uses), a real agent that lands a commit must
// still be reported OK — not just the bestEffortWriter primitive in
// isolation, but the actual runAgent call site that wraps it.
func TestDriveRunAgentSurvivesLogWriteFailure(t *testing.T) {
	orig := openLogFile
	openLogFile = func(name string, _ int, perm os.FileMode) (*os.File, error) {
		if err := os.WriteFile(name, nil, perm); err != nil {
			return nil, err
		}
		return os.OpenFile(name, os.O_RDONLY, 0) // every Write() to this fails
	}
	t.Cleanup(func() { openLogFile = orig })

	ctx := context.Background()
	_, repo := makeGoRepo(t)
	logDir := t.TempDir()
	agentBin := buildTestAgent(t)

	task := taskSpec{ID: "survives", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"landed.go": "package main\n\nfunc landed() int { return 1 }\n"},
	})}
	// Real output on both streams so the log-write path is actually exercised
	// (not skipped because the child produced nothing to write).
	agentCmd := "printf 'stdout-line\\n'; printf 'stderr-line\\n' >&2; " + agentBin

	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agentCmd, LogDir: logDir}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.PerAgent) != 1 {
		t.Fatalf("want 1 agent result, got %d", len(rep.PerAgent))
	}
	a := rep.PerAgent[0]
	if !a.OK {
		t.Fatalf("agent OK=false (exit=%d stderr=%q); a failing -logdir write must never fail the agent", a.Exit, a.Stderr)
	}
	if a.SHA == "" || a.SHA == rep.BaseSHA {
		t.Fatalf("agent did not land its commit: sha=%q base=%q", a.SHA, rep.BaseSHA)
	}
	if len(a.Files) != 1 || a.Files[0] != "landed.go" {
		t.Fatalf("agent.Files=%v, want [landed.go]", a.Files)
	}
}

// readEvents reads an -events NDJSON file, asserting every line is valid
// JSON (the core -events contract) and returning each line decoded plus its
// "event" name, in file order (== emission order, since eventEmitter holds a
// single mutex-guarded encoder).
func readEvents(t *testing.T, path string) (names []string, recs []map[string]any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read events file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			t.Fatalf("events line %d is not valid JSON: %s", i, line)
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal events line %d: %v", i, err)
		}
		name, _ := rec["event"].(string)
		if name == "" {
			t.Fatalf("events line %d has no \"event\" field: %s", i, line)
		}
		if _, ok := rec["ts"]; !ok {
			t.Fatalf("events line %d has no \"ts\" field: %s", i, line)
		}
		names = append(names, name)
		recs = append(recs, rec)
	}
	return names, recs
}

// indexOf returns the first index of want in ss, or -1.
func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

// countOf returns how many times want appears in ss.
func countOf(ss []string, want string) int {
	n := 0
	for _, s := range ss {
		if s == want {
			n++
		}
	}
	return n
}

// TestNewEventEmitterModes exercises newEventEmitter's three path modes
// directly (below driveRun, which only ever drives it through -events): ""
// disables (nil emitter, safe no-op emit), "-" streams to stderr, a real
// path is opened fresh (truncated, not appended like -logdir) and usable,
// and an unopenable path fails loudly instead of silently dropping events.
func TestNewEventEmitterModes(t *testing.T) {
	var nilEmit *eventEmitter
	nilEmit.emit("x", map[string]any{"a": 1}) // must not panic

	e, closeFn, err := newEventEmitter("")
	if err != nil || e != nil {
		t.Fatalf(`newEventEmitter("") = (%v, _, %v), want (nil, _, nil)`, e, err)
	}
	closeFn()

	e, closeFn, err = newEventEmitter("-")
	if err != nil || e == nil {
		t.Fatalf(`newEventEmitter("-") = (%v, _, %v), want (non-nil, _, nil)`, e, err)
	}
	closeFn() // stderr must not be closed; a second use elsewhere in the test binary must stay usable

	path := filepath.Join(t.TempDir(), "events.ndjson")
	if err := os.WriteFile(path, []byte("stale content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, closeFn, err = newEventEmitter(path)
	if err != nil || e == nil {
		t.Fatalf("newEventEmitter(%q) = (%v, _, %v), want (non-nil, _, nil)", path, e, err)
	}
	e.emit("probe", map[string]any{"x": 1})
	closeFn()
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(data), "stale content") {
		t.Fatalf("events file not truncated fresh: %s", data)
	}
	if !strings.Contains(string(data), `"probe"`) {
		t.Fatalf("events file missing emitted line: %s", data)
	}

	if _, _, err := newEventEmitter(filepath.Join(path, "nested", "cant-create-under-a-file")); err == nil {
		t.Fatal("newEventEmitter with an unopenable path: want error, got nil")
	}
}

// TestDriveRunEventsNDJSON drives a real run (agents + resolver + verify)
// with -events to a temp file and checks the whole -events contract: every
// line is parseable NDJSON (via readEvents), events appear in the documented
// causal order (run_start first, run_done last, each agent's start before
// its done, integrate_done before verify_start), an agent_start/agent_done
// pair for every task, and the event counts/fields line up with the final
// report — events are progress, not a second report, so they must never
// disagree with it.
func TestDriveRunEventsNDJSON(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	eventsPath := filepath.Join(t.TempDir(), "events.ndjson")

	tasks := []taskSpec{
		{ID: "t1", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"alpha.go": "package main\n\nfunc alpha() int { return 1 }\n"},
		})},
		{ID: "t2", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"beta.go": "package main\n\nfunc beta() int { return 2 }\n"},
		})},
	}
	p := runParams{
		Repo:       repo,
		Base:       "main",
		Strategy:   "overlay",
		AgentCmd:   agent,
		VerifyCmd:  "go build ./...",
		EventsPath: eventsPath,
	}

	rep, err := driveRun(ctx, p, tasks)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep.Verify.OK {
		t.Fatalf("verify failed, want a clean landing: %q", rep.Verify.Output)
	}

	names, recs := readEvents(t, eventsPath)
	if len(names) == 0 {
		t.Fatal("no events written")
	}

	// ---- causal order ----
	if names[0] != "run_start" {
		t.Fatalf("first event=%q, want run_start", names[0])
	}
	if last := names[len(names)-1]; last != "run_done" {
		t.Fatalf("last event=%q, want run_done", last)
	}
	if id, vd := indexOf(names, "integrate_done"), indexOf(names, "verify_start"); id == -1 || vd == -1 || id >= vd {
		t.Fatalf("integrate_done (idx %d) must precede verify_start (idx %d)", id, vd)
	}

	// ---- an agent_start/agent_done pair for every task, start before done ----
	started := map[string]int{}
	done := map[string]int{}
	for i, name := range names {
		switch name {
		case "agent_start":
			id, _ := recs[i]["id"].(string)
			started[id] = i
		case "agent_done":
			id, _ := recs[i]["id"].(string)
			done[id] = i
		}
	}
	for _, task := range tasks {
		si, sok := started[task.ID]
		di, dok := done[task.ID]
		if !sok || !dok {
			t.Fatalf("task %s missing agent_start/agent_done (start=%v done=%v)", task.ID, sok, dok)
		}
		if si >= di {
			t.Fatalf("task %s: agent_start (idx %d) must precede agent_done (idx %d)", task.ID, si, di)
		}
	}

	// ---- agent_done carries files + inLane, matching the report ----
	perAgentByID := make(map[string]perAgentJSON, len(rep.PerAgent))
	for _, a := range rep.PerAgent {
		perAgentByID[a.ID] = a
	}
	for _, task := range tasks {
		rec := recs[done[task.ID]]
		a := perAgentByID[task.ID]
		gotFiles, _ := rec["files"].([]any)
		if len(gotFiles) != len(a.Files) {
			t.Fatalf("task %s: agent_done.files=%v, want %v (rep.PerAgent[%s].Files)", task.ID, gotFiles, a.Files, task.ID)
		}
		for i, f := range a.Files {
			if s, _ := gotFiles[i].(string); s != f {
				t.Fatalf("task %s: agent_done.files[%d]=%q, want %q", task.ID, i, s, f)
			}
		}
		if inLane, _ := rec["inLane"].(bool); inLane != a.InLane {
			t.Fatalf("task %s: agent_done.inLane=%v, want %v (rep.PerAgent[%s].InLane)", task.ID, inLane, a.InLane, task.ID)
		}
	}

	// ---- counts match the report ----
	if got, want := countOf(names, "agent_start"), len(rep.PerAgent); got != want {
		t.Fatalf("agent_start count=%d, want %d (len(rep.PerAgent))", got, want)
	}
	if got, want := countOf(names, "agent_done"), len(rep.PerAgent); got != want {
		t.Fatalf("agent_done count=%d, want %d (len(rep.PerAgent))", got, want)
	}
	if got := countOf(names, "run_start"); got != 1 {
		t.Fatalf("run_start count=%d, want 1", got)
	}
	if got := countOf(names, "run_done"); got != 1 {
		t.Fatalf("run_done count=%d, want 1", got)
	}
	if got := countOf(names, "land"); got != 1 {
		t.Fatalf("land count=%d, want 1 (verify passed, must land)", got)
	}

	// ---- spot-check fields on a few events against the report ----
	landRec := recs[indexOf(names, "land")]
	if sha, _ := landRec["sha"].(string); sha != rep.Integrate.FinalSHA {
		t.Fatalf("land.sha=%q, want rep.Integrate.FinalSHA=%q", sha, rep.Integrate.FinalSHA)
	}
	runStartRec := recs[0]
	if repoField, _ := runStartRec["repo"].(string); repoField != repo {
		t.Fatalf("run_start.repo=%q, want %q", repoField, repo)
	}
	if baseSHA, _ := runStartRec["baseSHA"].(string); baseSHA != rep.BaseSHA {
		t.Fatalf("run_start.baseSHA=%q, want %q", baseSHA, rep.BaseSHA)
	}
	runDoneRec := recs[len(recs)-1]
	if ok, _ := runDoneRec["ok"].(bool); !ok {
		t.Fatalf("run_done.ok=%v, want true (clean landed+verified run)", runDoneRec["ok"])
	}
	if code, _ := runDoneRec["exitCode"].(float64); int(code) != runExitCode(rep) {
		t.Fatalf("run_done.exitCode=%v, want %d (runExitCode(rep))", runDoneRec["exitCode"], runExitCode(rep))
	}
}

// TestDriveRunEventsRepairOrder exercises the repair loop's events: the
// initial verify_start/verify_done (attempt 0), then a
// repair_start/repair_done + verify_start/verify_done (attempt 1) round that
// fixes the build, in that exact order — the causal chain -events promises
// for a run that needed self-healing.
func TestDriveRunEventsRepairOrder(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	eventsPath := filepath.Join(t.TempDir(), "events.ndjson")

	p := runParams{
		Repo:       repo,
		Base:       "main",
		Strategy:   "overlay",
		AgentCmd:   agent,
		VerifyCmd:  "go build ./...",
		RepairCmd:  `printf 'package main\n\nfunc helper() int { return helperX() }\n' > repair_fix.go`,
		RepairMax:  2,
		EventsPath: eventsPath,
	}

	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep.Verify.OK || !rep.Verify.Repaired {
		t.Fatalf("verify ok=%v repaired=%v, want the repair loop to fix it", rep.Verify.OK, rep.Verify.Repaired)
	}

	names, recs := readEvents(t, eventsPath)
	// repair_done reports verifyOk for ITS round's verify, so it can only be
	// emitted once that verify (the second verify_start/verify_done pair
	// below) has actually completed — hence repair_done trails, not leads,
	// the verify pair it summarizes.
	wantOrder := []string{"verify_start", "verify_done", "repair_start", "verify_start", "verify_done", "repair_done"}
	firstVS := indexOf(names, "verify_start")
	if firstVS == -1 || firstVS+len(wantOrder) > len(names) {
		t.Fatalf("events %v missing the expected verify/repair sequence %v", names, wantOrder)
	}
	got := names[firstVS : firstVS+len(wantOrder)]
	for i, want := range wantOrder {
		if got[i] != want {
			t.Fatalf("verify/repair sequence = %v, want %v (mismatch at %d)", got, wantOrder, i)
		}
	}
	// attempt 0 (pre-repair, failing) then attempt 1 (post-repair, passing).
	if a, ok := recs[firstVS]["attempt"].(float64); !ok || a != 0 {
		t.Fatalf("first verify_start.attempt=%v, want 0", recs[firstVS]["attempt"])
	}
	if ok, _ := recs[firstVS+1]["ok"].(bool); ok {
		t.Fatal("first verify_done.ok=true, want false (build is broken before repair)")
	}
	repairStart := recs[firstVS+2]
	if a, ok := repairStart["attempt"].(float64); !ok || a != 1 {
		t.Fatalf("repair_start.attempt=%v, want 1", repairStart["attempt"])
	}
	lastVD := recs[firstVS+4]
	if ok, _ := lastVD["ok"].(bool); !ok {
		t.Fatal("post-repair verify_done.ok=false, want true (repair fixed the build)")
	}
	repairDone := recs[firstVS+5]
	if a, ok := repairDone["attempt"].(float64); !ok || a != 1 {
		t.Fatalf("repair_done.attempt=%v, want 1", repairDone["attempt"])
	}
	if ok, _ := repairDone["verifyOk"].(bool); !ok {
		t.Fatal("repair_done.verifyOk=false, want true (this round's verify passed)")
	}
}

// TestDriveRunEventsOff: with -events unset (the default, EventsPath=""),
// driveRun must behave exactly as it did before this feature existed — no
// events file, no panics, a normal report. eventEmitter's nil-receiver no-op
// is what makes every emit call site in driveRun safe to call unconditionally;
// this is the regression test for that path.
func TestDriveRunEventsOff(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	// A path under a directory that's never created: if driveRun tried to open
	// it despite EventsPath=="", that open would fail loudly.
	untouched := filepath.Join(t.TempDir(), "never-created-dir", "events.ndjson")

	task := taskSpec{ID: "solo", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"solo.go": "package main\n\nfunc solo() int { return 1 }\n"},
	})}
	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent}

	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.PerAgent) != 1 || !rep.PerAgent[0].OK {
		t.Fatalf("agent did not land: %+v", rep.PerAgent)
	}
	if _, err := os.Stat(untouched); !os.IsNotExist(err) {
		t.Fatalf("no file should exist at %s when -events is unset", untouched)
	}
}

// TestRunRunEventsFlagWired proves the -events flag actually reaches
// driveRun through runRun's normal flag-parsing path (every other -events
// test above drives driveRun directly, bypassing flag parsing entirely).
func TestRunRunEventsFlagWired(t *testing.T) {
	_, repo := makeGoRepo(t)
	agentBin := buildTestAgent(t)
	eventsPath := filepath.Join(t.TempDir(), "events.ndjson")

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-base", "main",
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
		})}}),
		"-agent", agentBin,
		"-events", eventsPath,
	})
	if err != nil {
		t.Fatalf("runRun: %v", err)
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK", code)
	}
	names, _ := readEvents(t, eventsPath)
	if names[0] != "run_start" || names[len(names)-1] != "run_done" {
		t.Fatalf("events=%v, want run_start..run_done", names)
	}
	if countOf(names, "land") != 1 {
		t.Fatalf("events=%v missing land", names)
	}
}

// TestRunRunEventsUnwritableFailsBeforeAgents: an -events path driveRun
// cannot open must fail the run before any agent runs — same fail-early
// policy as -logdir (see TestRunRunLogDirUnwritableFailsBeforeAgents, which
// this mirrors).
func TestRunRunEventsUnwritableFailsBeforeAgents(t *testing.T) {
	_, repo := makeGoRepo(t)
	marker := filepath.Join(t.TempDir(), "agent-ran")
	agentCmd := "touch " + marker

	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(blocked, "events.ndjson") // parent "blocked" is a file, not a dir

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-base", "main",
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", agentCmd,
		"-events", eventsPath,
	})
	if err == nil {
		t.Fatal("unwritable -events: want error, got nil")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("agent ran despite an unwritable -events path; must fail before any agent runs")
	}
}

// TestRunDryRunPredictsPartition: -dry-run with a -tasks file prints the
// predicted OCC groups computed from each task's DECLARED files via the real
// cell.Partition — two tasks sharing a declared file land in the same
// predicted group, a third disjoint task gets its own — and creates NO
// worktree (neither a fresh worktree root nor any entry under the repo's own
// .git/worktrees admin state) and leaves the base ref untouched. -agent is
// still required (no validation changes) but must never actually run.
func TestRunDryRunPredictsPartition(t *testing.T) {
	g, repo := makeGoRepo(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // pin: a real run would create its worktree root here

	marker := filepath.Join(t.TempDir(), "agent-ran")
	agentCmd := "touch " + marker

	tasks := []taskSpec{
		{ID: "t1", Prompt: "x", Files: []string{"a.go", "shared.go"}},
		{ID: "t2", Prompt: "x", Files: []string{"b.go", "shared.go"}}, // overlaps t1 on shared.go
		{ID: "t3", Prompt: "x", Files: []string{"c.go"}},              // disjoint
	}

	before, err := g.RevParse(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", agentCmd,
		"-dry-run",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK on a valid plan", code)
	}

	out := buf.String()
	for _, id := range []string{"t1", "t2", "t3"} {
		if !strings.Contains(out, id) {
			t.Fatalf("summary should name task %q, got:\n%s", id, out)
		}
	}
	if !strings.Contains(out, "2 group") {
		t.Fatalf("want 2 predicted groups (t1+t2 share shared.go, t3 is disjoint), got:\n%s", out)
	}

	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("agent marker exists: -dry-run must never run the agent")
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("want no worktree root created under %s, found %v", tmp, entries)
	}
	if wtAdmin, err := os.ReadDir(filepath.Join(repo, ".git", "worktrees")); err == nil {
		if len(wtAdmin) != 0 {
			t.Fatalf("want no entries under .git/worktrees, found %v", wtAdmin)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	after, err := g.RevParse(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("main advanced (%s -> %s); -dry-run must not touch the base ref", before, after)
	}
}

// TestRunDryRunJSON: -dry-run -json parses as the documented shape
// {tasks, groups:[{tasks,files}], parallelism, laneMode}. A task with no
// declared files is unknown to the partitioner and gets its own group.
func TestRunDryRunJSON(t *testing.T) {
	_, repo := makeGoRepo(t)
	tasks := []taskSpec{
		{ID: "t1", Prompt: "x", Files: []string{"a.go"}},
		{ID: "t2", Prompt: "x"}, // no declared files -> unknown, own group
	}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", "exit 1",
		"-dry-run",
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK", code)
	}

	var rep dryRunReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse -dry-run -json report: %v\n%s", err, buf.String())
	}
	if len(rep.Tasks) != 2 {
		t.Fatalf("tasks=%d, want 2", len(rep.Tasks))
	}
	if len(rep.Groups) != 2 {
		t.Fatalf("groups=%d, want 2 (disjoint declared files), got %+v", len(rep.Groups), rep.Groups)
	}
	if rep.Parallelism != len(rep.Groups) {
		t.Fatalf("parallelism=%d != len(groups)=%d", rep.Parallelism, len(rep.Groups))
	}
	if rep.LaneMode != laneWarn {
		t.Fatalf("laneMode=%q, want %q (a -tasks run keeps the warn default)", rep.LaneMode, laneWarn)
	}
	for _, gr := range rep.Groups {
		if gr.Files == nil {
			t.Fatalf("group %+v: Files should be [] not null", gr)
		}
	}
}

// TestRunRunVerifyPresetThreadsIntoVerifyCmd proves -verify-preset go isn't
// just parsed and dropped: runRun expands it (via applyPresets) into
// runParams.VerifyCmd, driveRun actually invokes it in the detached
// checkout, and it runs the real `go build ./... && go test ./...` against
// the integrated tree — makeGoRepo's fixture builds and has no test files
// (so `go test ./...` is a clean, fast "no test files" pass), giving an
// honest verify.ok=true rather than a stubbed one.
func TestRunRunVerifyPresetThreadsIntoVerifyCmd(t *testing.T) {
	agent := buildTestAgent(t)
	_, repo := makeGoRepo(t)
	tasks := []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", agent,
		"-verify-preset", "go",
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
	}

	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}
	if !rep.Verify.Ran {
		t.Fatal("verify.ran=false, want true: -verify-preset go should have set runParams.VerifyCmd")
	}
	if !rep.Verify.OK {
		t.Fatalf("verify.ok=false, want true (go build+test on a clean fixture): %s", rep.Verify.Output)
	}
}

// TestRunRunVerifyBeatsVerifyPreset: an explicit -verify always overrides
// -verify-preset (raw wins), proven end-to-end through runRun/driveRun by
// pointing -verify at a command that FAILS while -verify-preset names a
// preset ("go build ./... && go test ./...") that would otherwise pass on
// this fixture — if the preset won, this run would report verify.ok=true.
func TestRunRunVerifyBeatsVerifyPreset(t *testing.T) {
	agent := buildTestAgent(t)
	_, repo := makeGoRepo(t)
	tasks := []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", agent,
		"-verify", "exit 1",
		"-verify-preset", "go",
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitVerifyFailed {
		t.Fatalf("code=%d, want exitVerifyFailed (raw -verify should have overridden -verify-preset go)\n%s", code, buf.String())
	}

	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}
	if !rep.Verify.Ran || rep.Verify.OK {
		t.Fatalf("verify.ran=%v ok=%v, want ran=true ok=false (raw -verify 'exit 1' should have run, not the go preset)", rep.Verify.Ran, rep.Verify.OK)
	}
}

// TestRunRunUnknownAgentPresetFailsBeforeAnyAgent: an unknown -agent-preset
// name fails loudly, naming valid names, before any worktree/agent runs —
// same fail-fast policy as a bad -config file or -logdir (see
// TestConfigUnknownKeyFailsWithLineNumber).
func TestRunRunUnknownAgentPresetFailsBeforeAnyAgent(t *testing.T) {
	_, repo := makeGoRepo(t)
	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent-preset", "gpt",
	})
	if err == nil {
		t.Fatal("want an error for an unknown -agent-preset name")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	got := err.Error()
	for _, want := range []string{"-agent-preset", `"gpt"`, "claude", "codex", "aider"} {
		if !strings.Contains(got, want) {
			t.Fatalf("error=%q, want it to contain %q", got, want)
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("report=%q, want no report written (nothing should have run)", buf.String())
	}
}

// TestRunRunSemanticRejectsUnknownValue: an unrecognized -semantic value
// fails before any agent runs, same fail-fast posture as -strategy/-lanes.
func TestRunRunSemanticRejectsUnknownValue(t *testing.T) {
	_, repo := makeGoRepo(t)
	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo, "-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", "true", "-semantic", "bogus",
	})
	if err == nil || !strings.Contains(err.Error(), "-semantic") {
		t.Fatalf("err=%v, want a -semantic complaint", err)
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("report=%q, want no report written (nothing should have run)", buf.String())
	}
}

// TestRunRunSemanticFlagWiresIntoReport: -semantic go parsed off argv reaches
// driveRun (proven end-to-end by the resulting semanticEdges/groups, the same
// motivating scenario as TestDriveRunSemanticGoGroupsCrossFileSignatureConflict
// but this time going through actual flag parsing).
func TestRunRunSemanticFlagWiresIntoReport(t *testing.T) {
	_, repo := makeSemanticGoRepo(t)
	agent := buildTestAgent(t)
	tasksPath := tasksFileFor(t, semanticScenarioTasks(t))
	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo, "-base", "main", "-tasks", tasksPath,
		"-agent", agent, "-verify", "go build ./...", "-semantic", "go", "-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v", err)
	}
	if code != exitVerifyFailed {
		t.Fatalf("code=%d, want exitVerifyFailed (build is genuinely broken)", code)
	}
	var rep runReport
	if jerr := json.Unmarshal(buf.Bytes(), &rep); jerr != nil {
		t.Fatalf("parse report: %v\n%s", jerr, buf.String())
	}
	if rep.Integrate.Groups != 1 {
		t.Fatalf("groups=%d, want 1 (-semantic go merged ta/tb)", rep.Integrate.Groups)
	}
	if len(rep.Integrate.SemanticEdges) != 1 {
		t.Fatalf("semanticEdges=%v, want 1 pair", rep.Integrate.SemanticEdges)
	}
}

// ---- -verify-impact (issue #10: test-impact analysis) ----

// makeImpactGoRepo initializes a temp module with THREE packages for
// test-impact analysis (mirrors writeImpactFixture in impact_test.go, but as
// a real git repo agents can commit to): a (leaf), b (imports a), c
// (independent) — plus a non-Go file (README.md) agents can touch to
// exercise the "any doubt" fallback. `go build ./...` on the base tree
// passes.
func makeImpactGoRepo(t *testing.T) (*gitx.Git, string) {
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
	write("go.mod", "module example.com/impact\n\ngo 1.21\n")
	write("a/a.go", "package a\n\nfunc A() int { return 1 }\n")
	write("b/b.go", "package b\n\nimport \"example.com/impact/a\"\n\nfunc B() int { return a.A() + 1 }\n")
	write("c/c.go", "package c\n\nfunc C() int { return 3 }\n")
	write("README.md", "impact test fixture\n")
	if _, err := g.CommitAll(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	return g, dir
}

// newImpactVerifyParams returns a runParams whose -verify writes fullMarker
// (proving the FULL command ran) and whose -verify-impact dumps
// SIGBOUND_IMPACTED_PKGS/SIGBOUND_CHANGED_FILES to outFile (proving the
// SCOPED command ran, and exactly what it saw) — a test tells the two paths
// apart by which file shows up, not just by reading Scope back out of the
// report.
func newImpactVerifyParams(t *testing.T, repo, agent string) (p runParams, fullMarker, outFile string) {
	t.Helper()
	fullMarker = filepath.Join(t.TempDir(), "full-marker.txt")
	outFile = filepath.Join(t.TempDir(), "impact-out.txt")
	p = runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:       fmt.Sprintf("echo full > %q", fullMarker),
		VerifyImpactCmd: fmt.Sprintf(`printf '%%s\n%%s\n' "$SIGBOUND_IMPACTED_PKGS" "$SIGBOUND_CHANGED_FILES" > %q`, outFile),
	}
	return p, fullMarker, outFile
}

// TestDriveRunVerifyImpactScopesToChangedLeafPackage: changing only c/c.go
// (an independent package, nothing imports it) must run the SCOPED
// -verify-impact command with SIGBOUND_IMPACTED_PKGS=./c, not the full
// -verify command.
func TestDriveRunVerifyImpactScopesToChangedLeafPackage(t *testing.T) {
	ctx := context.Background()
	_, repo := makeImpactGoRepo(t)
	agent := buildTestAgent(t)
	p, fullMarker, outFile := newImpactVerifyParams(t, repo, agent)

	task := taskSpec{ID: "c", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"c/c.go": "package c\n\nfunc C() int { return 30 }\n"},
	})}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep.Verify.Ran || !rep.Verify.OK {
		t.Fatalf("verify ran=%v ok=%v output=%q", rep.Verify.Ran, rep.Verify.OK, rep.Verify.Output)
	}
	if rep.Verify.Scope != "impact" {
		t.Fatalf("scope=%q, want impact: %s", rep.Verify.Scope, rep.Verify.Output)
	}
	if got := strings.Join(rep.Verify.ImpactedPkgs, " "); got != "./c" {
		t.Fatalf("impactedPkgs=%q, want ./c (independent package, no reverse deps)", got)
	}
	if _, err := os.Stat(fullMarker); err == nil {
		t.Fatal("full-verify marker exists; the FULL -verify command ran, want the scoped -verify-impact command only")
	}
	out, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read verify-impact output file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "./c" {
		t.Fatalf("verify-impact saw %q, want SIGBOUND_IMPACTED_PKGS=./c on the first line", out)
	}
	if !strings.Contains(lines[1], "c/c.go") {
		t.Fatalf("SIGBOUND_CHANGED_FILES=%q, want it to include c/c.go", lines[1])
	}
}

// TestDriveRunVerifyImpactExpandsToReverseDependents: changing a/a.go must
// impact BOTH a and b (b imports a) but NOT c (independent) — the reverse
// dependency, not just the changed package itself, reaches the scoped
// command.
func TestDriveRunVerifyImpactExpandsToReverseDependents(t *testing.T) {
	ctx := context.Background()
	_, repo := makeImpactGoRepo(t)
	agent := buildTestAgent(t)
	p, fullMarker, _ := newImpactVerifyParams(t, repo, agent)

	task := taskSpec{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a/a.go": "package a\n\nfunc A() int { return 100 }\n"},
	})}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep.Verify.Scope != "impact" {
		t.Fatalf("scope=%q, want impact: %s", rep.Verify.Scope, rep.Verify.Output)
	}
	if got := strings.Join(rep.Verify.ImpactedPkgs, " "); got != "./a ./b" {
		t.Fatalf("impactedPkgs=%q, want ./a ./b (b imports a; c is independent and must be excluded)", got)
	}
	if _, err := os.Stat(fullMarker); err == nil {
		t.Fatal("full-verify marker exists, want the scoped command only")
	}
}

// TestDriveRunVerifyImpactFallsBackOnNonGoChange: a change to a non-Go file
// (README.md) is "any doubt" — the FULL -verify command must run instead of
// -verify-impact, and the report must say scope=full.
func TestDriveRunVerifyImpactFallsBackOnNonGoChange(t *testing.T) {
	ctx := context.Background()
	_, repo := makeImpactGoRepo(t)
	agent := buildTestAgent(t)
	p, fullMarker, outFile := newImpactVerifyParams(t, repo, agent)

	task := taskSpec{ID: "docs", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"README.md": "updated\n"},
	})}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep.Verify.Ran || !rep.Verify.OK {
		t.Fatalf("verify ran=%v ok=%v output=%q", rep.Verify.Ran, rep.Verify.OK, rep.Verify.Output)
	}
	if rep.Verify.Scope != "full" {
		t.Fatalf("scope=%q, want full (non-Go file changed => any doubt)", rep.Verify.Scope)
	}
	if len(rep.Verify.ImpactedPkgs) != 0 {
		t.Fatalf("impactedPkgs=%v, want none on a full-scope run", rep.Verify.ImpactedPkgs)
	}
	if _, err := os.Stat(fullMarker); err != nil {
		t.Fatalf("full-verify marker missing, want the FULL -verify command to have run: %v", err)
	}
	if _, err := os.Stat(outFile); err == nil {
		t.Fatal("verify-impact output file exists; the scoped command must not have run")
	}
}

// TestDriveRunVerifyImpactFallsBackOnGoModChange: go.mod is not a .go file
// but still lives in the module — it must still trigger the full fallback,
// same as any other non-Go change.
func TestDriveRunVerifyImpactFallsBackOnGoModChange(t *testing.T) {
	ctx := context.Background()
	_, repo := makeImpactGoRepo(t)
	agent := buildTestAgent(t)
	p, fullMarker, outFile := newImpactVerifyParams(t, repo, agent)

	task := taskSpec{ID: "mod", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"go.mod": "module example.com/impact\n\ngo 1.21\n\n// bumped\n"},
	})}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep.Verify.Scope != "full" {
		t.Fatalf("scope=%q, want full (go.mod change => any doubt)", rep.Verify.Scope)
	}
	if _, err := os.Stat(fullMarker); err != nil {
		t.Fatalf("full-verify marker missing: %v", err)
	}
	if _, err := os.Stat(outFile); err == nil {
		t.Fatal("verify-impact output file exists; want full fallback only")
	}
}

// TestDriveRunVerifyImpactAbsentIsByteIdentical proves -verify-impact is
// fully opt-in: with it unset, verify.scope/verify.impactedPkgs stay
// zero-valued (and so vanish from the JSON report via omitempty) and the
// plain -verify command runs exactly as it did before this feature existed.
func TestDriveRunVerifyImpactAbsentIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	_, repo := makeImpactGoRepo(t)
	agent := buildTestAgent(t)
	fullMarker := filepath.Join(t.TempDir(), "full-marker.txt")

	task := taskSpec{ID: "c", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"c/c.go": "package c\n\nfunc C() int { return 30 }\n"},
	})}
	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd: fmt.Sprintf("echo full > %q", fullMarker),
		// VerifyImpactCmd deliberately left unset.
	}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep.Verify.Ran || !rep.Verify.OK {
		t.Fatalf("verify ran=%v ok=%v output=%q", rep.Verify.Ran, rep.Verify.OK, rep.Verify.Output)
	}
	if rep.Verify.Scope != "" || rep.Verify.ImpactedPkgs != nil {
		t.Fatalf("scope=%q impactedPkgs=%v, want both zero-valued with -verify-impact unset", rep.Verify.Scope, rep.Verify.ImpactedPkgs)
	}
	if _, err := os.Stat(fullMarker); err != nil {
		t.Fatalf("-verify did not run: %v", err)
	}
	b, err := json.Marshal(rep.Verify)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"scope"`) || strings.Contains(string(b), `"impactedPkgs"`) {
		t.Fatalf("verify JSON=%s, want no scope/impactedPkgs keys when -verify-impact is unset (byte-identical to before this feature)", b)
	}
}

// TestDriveRunVerifyImpactRecomputesAfterRepair: the repair loop's fixer adds
// a brand-new, independent package (d/d.go) on top of the original landed
// change (c/c.go). The re-verify after repair must fold the fixer's edit
// into the changed-file set and RECOMPUTE impact from scratch — not reuse
// the pre-repair decision — so the second invocation sees ./c AND ./d.
func TestDriveRunVerifyImpactRecomputesAfterRepair(t *testing.T) {
	ctx := context.Background()
	_, repo := makeImpactGoRepo(t)
	agent := buildTestAgent(t)

	marker := filepath.Join(t.TempDir(), "verify-marker")
	out1 := filepath.Join(t.TempDir(), "impact-1.txt")
	out2 := filepath.Join(t.TempDir(), "impact-2.txt")
	// -verify-impact: fails the FIRST invocation (recording that attempt's
	// impacted set to out1), passes every later invocation (recording to
	// out2) — a marker-file flaky pattern (same trick as
	// TestDriveRunVerifyRetries) standing in for a real compiler round-trip.
	verifyImpact := fmt.Sprintf(
		`test -f %q && { printf '%%s\n' "$SIGBOUND_IMPACTED_PKGS" > %q; exit 0; } || { touch %q; printf '%%s\n' "$SIGBOUND_IMPACTED_PKGS" > %q; exit 1; }`,
		marker, out2, marker, out1,
	)
	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:       "exit 1", // canary: only reached if scoping unexpectedly falls back to full
		VerifyImpactCmd: verifyImpact,
		RepairCmd:       `mkdir -p d && printf 'package d\n\nfunc D() int { return 4 }\n' > d/d.go`,
		RepairMax:       1,
	}

	task := taskSpec{ID: "c", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"c/c.go": "package c\n\nfunc C() int { return 30 }\n"},
	})}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep.Verify.OK || !rep.Verify.Repaired {
		t.Fatalf("verify ok=%v repaired=%v output=%q", rep.Verify.OK, rep.Verify.Repaired, rep.Verify.Output)
	}
	if rep.Verify.Scope != "impact" {
		t.Fatalf("scope=%q, want impact", rep.Verify.Scope)
	}
	first, err := os.ReadFile(out1)
	if err != nil {
		t.Fatalf("read first-attempt impact: %v", err)
	}
	if strings.TrimSpace(string(first)) != "./c" {
		t.Fatalf("first attempt impacted=%q, want ./c", first)
	}
	second, err := os.ReadFile(out2)
	if err != nil {
		t.Fatalf("read post-repair impact: %v", err)
	}
	if got := strings.TrimSpace(string(second)); got != "./c ./d" {
		t.Fatalf("post-repair impacted=%q, want ./c ./d (repair's new file folded into the recomputed impact set)", got)
	}
	if got := rep.Verify.ImpactedPkgs; len(got) != 2 || got[0] != "./c" || got[1] != "./d" {
		t.Fatalf("report impactedPkgs=%v, want [./c ./d]", got)
	}
}

// TestRunRunVerifyImpactRequiresVerify: -verify-impact without -verify (or
// -verify-preset) must fail before any agent runs — it composes WITH
// -verify, which stays required as the fallback, never a substitute for it.
func TestRunRunVerifyImpactRequiresVerify(t *testing.T) {
	_, repo := makeGoRepo(t)
	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", "true",
		"-verify-impact", `go test $SIGBOUND_IMPACTED_PKGS`,
	})
	if err == nil {
		t.Fatal("want an error: -verify-impact without -verify")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	if !strings.Contains(err.Error(), "-verify-impact") || !strings.Contains(err.Error(), "-verify") {
		t.Fatalf("error=%q, want it to name -verify-impact and -verify", err.Error())
	}
	if buf.Len() != 0 {
		t.Fatalf("report=%q, want no report written (nothing should have run)", buf.String())
	}
}

// TestRunRunManifestWritesCommandsVersionTimestamp: -manifest FILE writes the
// full report to disk, independent of -json (not passed here at all — stdout
// stays the terse human summary), and the report carries the new provenance
// fields: the resolved agent/resolver/verify commands, the sigbound version,
// an RFC3339 start timestamp, and the top-level strategy.
func TestRunRunManifestWritesCommandsVersionTimestamp(t *testing.T) {
	agent := buildTestAgent(t)
	_, repo := makeGoRepo(t)
	tasks := []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}}
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	resolverCmd := `cat "$SIGBOUND_OURS"`

	before := time.Now().Add(-time.Second)
	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", agent,
		"-resolver", resolverCmd,
		"-verify", "go build ./...",
		"-manifest", manifestPath,
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
	}
	if strings.Contains(buf.String(), "\"agentCmd\"") {
		t.Fatalf("-manifest without -json still printed JSON to stdout: %s", buf.String())
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read -manifest: %v", err)
	}
	var rep runReport
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("parse manifest: %v\n%s", err, data)
	}
	if rep.AgentCmd != agent {
		t.Fatalf("manifest agentCmd=%q, want %q", rep.AgentCmd, agent)
	}
	if rep.ResolverCmd != resolverCmd {
		t.Fatalf("manifest resolverCmd=%q, want %q", rep.ResolverCmd, resolverCmd)
	}
	if rep.VerifyCmd != "go build ./..." {
		t.Fatalf("manifest verifyCmd=%q, want %q", rep.VerifyCmd, "go build ./...")
	}
	if rep.RepairCmd != "" {
		t.Fatalf("manifest repairCmd=%q, want empty (no -repair configured)", rep.RepairCmd)
	}
	if rep.PlannerCmd != "" {
		t.Fatalf("manifest plannerCmd=%q, want empty (-tasks run, no -goal/-planner)", rep.PlannerCmd)
	}
	if rep.Strategy != "overlay" {
		t.Fatalf("manifest strategy=%q, want overlay (the default)", rep.Strategy)
	}
	if rep.Version != Version {
		t.Fatalf("manifest version=%q, want %q", rep.Version, Version)
	}
	started, err := time.Parse(time.RFC3339, rep.StartedAt)
	if err != nil {
		t.Fatalf("manifest startedAt=%q is not RFC3339: %v", rep.StartedAt, err)
	}
	if started.Before(before) || started.After(time.Now().Add(time.Second)) {
		t.Fatalf("manifest startedAt=%v outside this test's wall-clock window", started)
	}
	if rep.Integrate.FinalSHA == "" {
		t.Fatal("manifest missing integrate.finalSHA")
	}
}

// TestRunRunManifestUnwritableFailsBeforeAgents: an unwritable -manifest path
// must fail the run before any agent runs, the same fail-fast policy as
// -logdir (see TestRunRunLogDirUnwritableFailsBeforeAgents) — provenance that
// can never be written is caught up front, not discovered after paying for
// the run.
func TestRunRunManifestUnwritableFailsBeforeAgents(t *testing.T) {
	_, repo := makeGoRepo(t)
	marker := filepath.Join(t.TempDir(), "agent-ran")
	agentCmd := "touch " + marker

	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(blocked, "manifest.json") // parent "blocked" is a file, not a dir

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-base", "main",
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", agentCmd,
		"-manifest", manifestPath,
	})
	if err == nil {
		t.Fatal("unwritable -manifest: want error, got nil")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("no report should be written when -manifest fails before any agent runs, got:\n%s", buf.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("agent ran despite an unwritable -manifest; must fail before any agent runs")
	}
}

// TestRunRunNotesAttachesReadableNoteToLandedCommit: -notes attaches the full
// JSON report as a git note under the NAMESPACED refs/notes/sigbound (never
// git's default refs/notes/commits) on the commit the run actually landed —
// read back with the plain `git notes --ref=sigbound show <sha>` porcelain
// docs/USAGE.md documents, proving the note is genuinely readable that way,
// not just written by NoteAdd and never verified independently.
func TestRunRunNotesAttachesReadableNoteToLandedCommit(t *testing.T) {
	agent := buildTestAgent(t)
	g, repo := makeGoRepo(t)
	tasks := []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", agent,
		"-notes",
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v", err)
	}

	mainSHA, err := g.RevParse(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != rep.Integrate.FinalSHA {
		t.Fatalf("main=%s, want it advanced to the landed finalSHA=%s", mainSHA, rep.Integrate.FinalSHA)
	}

	out, err := exec.Command("git", "-C", repo, "notes", "--ref=sigbound", "show", mainSHA).CombinedOutput()
	if err != nil {
		t.Fatalf("git notes --ref=sigbound show %s: %v\n%s", mainSHA, err, out)
	}
	var noted runReport
	if err := json.Unmarshal(out, &noted); err != nil {
		t.Fatalf("note body is not the JSON report: %v\n%s", err, out)
	}
	if noted.Integrate.FinalSHA != rep.Integrate.FinalSHA {
		t.Fatalf("noted report finalSHA=%s, want %s", noted.Integrate.FinalSHA, rep.Integrate.FinalSHA)
	}
	if noted.AgentCmd != agent {
		t.Fatalf("noted report agentCmd=%q, want %q", noted.AgentCmd, agent)
	}
}

// TestRunRunNotesOffByDefaultAttachesNothing: -notes is opt-in — a plain run
// with no -notes must leave the landed commit with no sigbound note at all.
func TestRunRunNotesOffByDefaultAttachesNothing(t *testing.T) {
	agent := buildTestAgent(t)
	_, repo := makeGoRepo(t)
	tasks := []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", agent,
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "notes", "--ref=sigbound", "show", rep.Integrate.FinalSHA).CombinedOutput(); err == nil {
		t.Fatalf("a sigbound note exists despite -notes never being passed: %s", out)
	}
}

// markerAgentCmd returns a shell -agent command that, on every invocation,
// first appends one line to <markerDir>/<task id>.log — regardless of
// success or failure — so a test can prove exactly how many times each
// task's agent actually ran. Task "c" fails the FIRST time it runs (its own
// marker log is still empty at that point) and succeeds every time after,
// so the very same command string works unmodified across an initial run
// and a -resume of it, exactly as an operator would actually reuse -agent.
// Every task that doesn't deliberately fail writes a small file the driver's
// autocommit then lands, so integration has real, disjoint content to work
// with (out-<id>.txt).
func markerAgentCmd(markerDir string) string {
	return `MARKER="` + markerDir + `/$SIGBOUND_TASK_ID.log"
BEFORE=$(wc -l < "$MARKER" 2>/dev/null || echo 0)
echo ran >> "$MARKER"
if [ "$SIGBOUND_TASK_ID" = "c" ] && [ "$BEFORE" -eq 0 ]; then
  exit 1
fi
echo "content-$SIGBOUND_TASK_ID" > "out-$SIGBOUND_TASK_ID.txt"
`
}

// markerRunCount reads how many times markerAgentCmd's task id ran, from its
// <markerDir>/<id>.log (one "ran" line per invocation); 0 if the agent for
// that id never ran at all (no log file yet).
func markerRunCount(t *testing.T, markerDir, id string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(markerDir, id+".log"))
	if err != nil {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
}

// TestRunRunResumeReRunsOnlyFailedTask is issue #19's central case: a run
// with 3 tasks where 2 agents commit and 1 fails, followed by -resume from
// that run's own -manifest. Resume must re-run ONLY the failed task's agent
// (proven via markerAgentCmd's per-task invocation counts — a and b's
// commands must not execute a second time at all), REUSE the two surviving
// branches outright (resumed=true, identical SHAs to the initial run), and
// still land all three onto -base exactly as an uninterrupted run would.
//
// The initial run is given -verify "test -f out-c.txt": since c fails, that
// file never lands, so verify fails and the run lands NOTHING — -base stays
// at its recorded baseSHA, the precondition -resume's moved-base check
// requires (a run that partially LANDS already moves -base past its own
// manifest, which is a moved base in its own right, exercised separately by
// TestRunRunResumeMovedBaseRefusesWithoutRunning). -verify is left
// unspecified on the resume call, so it's inherited from the manifest
// (flag > manifest, but nothing here overrides it) — exercising that
// inheritance too: it fails again if c's rerun doesn't actually land.
func TestRunRunResumeReRunsOnlyFailedTask(t *testing.T) {
	g, repo := makeGoRepo(t)
	markerDir := t.TempDir()
	agentCmd := markerAgentCmd(markerDir)

	tasks := []taskSpec{{ID: "a", Prompt: "x"}, {ID: "b", Prompt: "x"}, {ID: "c", Prompt: "x"}}
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")

	ctx := context.Background()
	mainBeforeInitial, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", agentCmd,
		"-verify", "test -f out-c.txt",
		"-manifest", manifestPath,
	})
	if err != nil {
		t.Fatalf("initial runRun: %v\n%s", err, buf.String())
	}
	if code != exitVerifyFailed {
		t.Fatalf("initial code=%d, want exitVerifyFailed (c's failure means out-c.txt never lands)\n%s", code, buf.String())
	}
	mainAfterInitial, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainAfterInitial != mainBeforeInitial {
		t.Fatalf("main advanced on the initial run despite verify failing; want nothing landed (baseSHA=%s stable for -resume)", mainBeforeInitial)
	}
	for _, id := range []string{"a", "b", "c"} {
		if got := markerRunCount(t, markerDir, id); got != 1 {
			t.Fatalf("marker %s ran %d times after the initial run, want 1", id, got)
		}
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var initial runReport
	if err := json.Unmarshal(data, &initial); err != nil {
		t.Fatalf("parse manifest: %v\n%s", err, data)
	}
	byID := map[string]perAgentJSON{}
	for _, a := range initial.PerAgent {
		byID[a.ID] = a
	}
	if !byID["a"].OK || !byID["b"].OK {
		t.Fatalf("want a and b to succeed initially: %+v", initial.PerAgent)
	}
	if byID["c"].OK {
		t.Fatalf("want c to fail initially: %+v", byID["c"])
	}
	aSHA, bSHA := byID["a"].SHA, byID["b"].SHA
	if aSHA == "" || bSHA == "" {
		t.Fatal("a/b should have a recorded SHA after the initial run")
	}
	if byID["a"].Resumed || byID["b"].Resumed || byID["c"].Resumed {
		t.Fatalf("nothing should be marked resumed on the INITIAL (non -resume) run: %+v", initial.PerAgent)
	}

	// ---- resume: only c's agent should run again ----
	buf.Reset()
	code, err = runRun(&buf, []string{
		"-repo", repo,
		"-resume",
		"-manifest", manifestPath,
		"-json",
	})
	if err != nil {
		t.Fatalf("resume runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("resume code=%d, want exitOK\n%s", code, buf.String())
	}

	if got := markerRunCount(t, markerDir, "a"); got != 1 {
		t.Fatalf("marker a ran %d times after resume, want 1 (must not re-run)", got)
	}
	if got := markerRunCount(t, markerDir, "b"); got != 1 {
		t.Fatalf("marker b ran %d times after resume, want 1 (must not re-run)", got)
	}
	if got := markerRunCount(t, markerDir, "c"); got != 2 {
		t.Fatalf("marker c ran %d times after resume, want 2 (must re-run exactly once)", got)
	}

	var resumed runReport
	if err := json.Unmarshal(buf.Bytes(), &resumed); err != nil {
		t.Fatalf("parse resume report: %v\n%s", err, buf.String())
	}
	byID = map[string]perAgentJSON{}
	for _, a := range resumed.PerAgent {
		byID[a.ID] = a
	}
	if !byID["a"].Resumed || !byID["a"].OK || byID["a"].SHA != aSHA {
		t.Fatalf("a should be reused unchanged: %+v", byID["a"])
	}
	if !byID["b"].Resumed || !byID["b"].OK || byID["b"].SHA != bSHA {
		t.Fatalf("b should be reused unchanged: %+v", byID["b"])
	}
	if byID["c"].Resumed {
		t.Fatalf("c should have run fresh (its prior branch was a stale no-op), not been reused: %+v", byID["c"])
	}
	if !byID["c"].OK {
		t.Fatalf("c should succeed on the resumed run: %+v", byID["c"])
	}
	if byID["c"].SHA == "" || byID["c"].SHA == initial.BaseSHA {
		t.Fatalf("c should have advanced to a fresh, real commit: %+v", byID["c"])
	}

	if len(resumed.Integrate.Landed) != 3 {
		t.Fatalf("landed=%v, want all 3 tasks", resumed.Integrate.Landed)
	}
	paths, err := g.LsTree(ctx, resumed.Integrate.FinalSHA)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"out-a.txt", "out-b.txt", "out-c.txt"} {
		if !contains(paths, want) {
			t.Fatalf("final tree %s missing %s (have %v)", resumed.Integrate.FinalSHA, want, paths)
		}
	}
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != resumed.Integrate.FinalSHA {
		t.Fatalf("main=%s not advanced to the resumed run's finalSHA=%s", mainSHA, resumed.Integrate.FinalSHA)
	}
}

// TestRunRunResumeMovedBaseRefusesWithoutRunning: once -base's CURRENT head
// is no longer exactly the manifest's recorded baseSHA, -resume must refuse
// loudly and run nothing at all — an ordinary landed run already advances
// -base past its own recorded baseSHA, so resuming from that SAME manifest
// again is already a moved base with no extra setup needed.
func TestRunRunResumeMovedBaseRefusesWithoutRunning(t *testing.T) {
	g, repo := makeGoRepo(t)
	markerDir := t.TempDir()
	marker := filepath.Join(markerDir, "ran")
	agentCmd := "touch " + marker + " && echo hi > out.txt"

	tasks := []taskSpec{{ID: "a", Prompt: "x"}}
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", agentCmd,
		"-manifest", manifestPath,
	})
	if err != nil {
		t.Fatalf("initial runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("initial code=%d, want exitOK\n%s", code, buf.String())
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatal("agent never ran on the initial run")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	mainBefore, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}

	// The initial run already landed, advancing main past the manifest's own
	// recorded baseSHA — resuming from THIS SAME manifest now must refuse.
	buf.Reset()
	code, err = runRun(&buf, []string{
		"-repo", repo,
		"-resume",
		"-manifest", manifestPath,
	})
	if err == nil {
		t.Fatalf("want an error resuming onto a moved base, got a clean report:\n%s", buf.String())
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError: %v", code, err)
	}
	if !strings.Contains(err.Error(), "moved") {
		t.Fatalf("error should name the moved base: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the agent ran despite the moved-base refusal; -resume must fail before anything runs")
	}
	mainAfter, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainAfter != mainBefore {
		t.Fatalf("main moved from %s to %s despite the refusal", mainBefore, mainAfter)
	}
}

// TestRunRunResumeWithoutManifestErrors: -resume REQUIRES -manifest (it's
// -resume's only source for the prior run's task list, base, and commands);
// omitting it must fail loudly, before any agent runs.
func TestRunRunResumeWithoutManifestErrors(t *testing.T) {
	_, repo := makeGoRepo(t)
	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-resume",
		"-agent", "true",
	})
	if err == nil {
		t.Fatal("want an error: -resume without -manifest")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	if !strings.Contains(err.Error(), "-manifest") {
		t.Fatalf("error should mention -manifest: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("no report should be written when -resume fails before any agent runs, got:\n%s", buf.String())
	}
}

// TestRunRunResumeWithTasksErrors: -resume never re-plans, so passing -tasks
// alongside it is a loud error rather than silently ignoring one or the
// other.
func TestRunRunResumeWithTasksErrors(t *testing.T) {
	_, repo := makeGoRepo(t)
	tasks := []taskSpec{{ID: "a", Prompt: "x"}}
	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-resume",
		"-tasks", tasksFileFor(t, tasks),
		"-agent", "true",
	})
	if err == nil {
		t.Fatal("want an error: -resume with -tasks")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	if !strings.Contains(err.Error(), "-tasks") {
		t.Fatalf("error should mention -tasks: %v", err)
	}
}

// parseEnvFile reads the output of `env > path` into a map, so a -publish (or
// similar) test can assert on individual SIGBOUND_* values without caring
// about the rest of the process environment or key ordering.
func parseEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env dump %s: %v", path, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// TestDriveRunPublishRunsOnceWithFullEnvOnGreenLand is issue #20's central
// case: on a clean landed run, -publish runs EXACTLY ONCE (proven by a
// marker file appended to on every invocation), with cwd = the repo itself,
// the full JSON run report piped on stdin, and every documented SIGBOUND_*
// var set to the value the report itself records —
// SIGBOUND_FINAL_SHA/BASE_BRANCH/BASE_SHA/REPO/LANDED/MANIFEST.
func TestDriveRunPublishRunsOnceWithFullEnvOnGreenLand(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	task := taskSpec{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}

	marker := filepath.Join(t.TempDir(), "publish.count")
	envOut := filepath.Join(t.TempDir(), "publish.env")
	stdinOut := filepath.Join(t.TempDir(), "publish.stdin")
	publishCmd := fmt.Sprintf(`cat > %q; echo ran >> %q; pwd > %q.cwd; env > %q`, stdinOut, marker, marker, envOut)

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		PublishCmd: publishCmd, PublishTimeout: 5 * time.Second,
		Manifest: "/tmp/some-manifest.json", // driveRun never reads/writes this file, only threads the path through as SIGBOUND_MANIFEST
	}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep.Publish == nil {
		t.Fatal("rep.Publish is nil, want it populated: -publish was set and the run landed")
	}
	if !rep.Publish.Ran || !rep.Publish.OK || rep.Publish.Exit != 0 {
		t.Fatalf("rep.Publish=%+v, want ran=true ok=true exit=0", rep.Publish)
	}

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 || lines[0] != "ran" {
		t.Fatalf("publish marker=%q, want exactly one \"ran\" line (publish must run exactly once)", data)
	}

	cwd, err := os.ReadFile(marker + ".cwd")
	if err != nil {
		t.Fatalf("read cwd marker: %v", err)
	}
	// EvalSymlinks on both sides: macOS resolves $TMPDIR through a /private
	// symlink, so `pwd` (which resolves symlinks) and repo (which doesn't)
	// can differ textually while naming the same directory.
	wantCwd, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	gotCwd, err := filepath.EvalSymlinks(strings.TrimSpace(string(cwd)))
	if err != nil {
		t.Fatal(err)
	}
	if gotCwd != wantCwd {
		t.Fatalf("publish cwd=%q, want the target repo %q", gotCwd, wantCwd)
	}

	env := parseEnvFile(t, envOut)
	want := map[string]string{
		"SIGBOUND_FINAL_SHA":   rep.Integrate.FinalSHA,
		"SIGBOUND_BASE_BRANCH": "main",
		"SIGBOUND_BASE_SHA":    rep.BaseSHA,
		"SIGBOUND_REPO":        repo,
		"SIGBOUND_LANDED":      strings.Join(rep.Integrate.Landed, " "),
		"SIGBOUND_MANIFEST":    "/tmp/some-manifest.json",
	}
	for k, wantV := range want {
		if got, ok := env[k]; !ok || got != wantV {
			t.Fatalf("%s=%q (present=%v), want %q", k, got, ok, wantV)
		}
	}
	if _, ok := env["SIGBOUND_BASE"]; ok {
		t.Fatalf("SIGBOUND_BASE is set (%q) for -publish; want only SIGBOUND_BASE_BRANCH (SIGBOUND_BASE is the -resolver slot's file-path variable)", env["SIGBOUND_BASE"])
	}
	if rep.Integrate.FinalSHA == rep.BaseSHA {
		t.Fatal("test setup: finalSHA == baseSHA, SIGBOUND_FINAL_SHA/SIGBOUND_BASE_SHA distinctness wouldn't be exercised")
	}

	// The full JSON run report must arrive on stdin, with the report's own
	// "publish" field still absent (this call is what fills it in) — and its
	// finalSHA must match the report driveRun ultimately returns.
	stdinData, err := os.ReadFile(stdinOut)
	if err != nil {
		t.Fatalf("read publish stdin dump: %v", err)
	}
	if strings.Contains(string(stdinData), `"publish"`) {
		t.Fatalf("publish stdin report already contains a \"publish\" key:\n%s", stdinData)
	}
	var stdinRep runReport
	if err := json.Unmarshal(stdinData, &stdinRep); err != nil {
		t.Fatalf("publish stdin is not valid report JSON: %v\n%s", err, stdinData)
	}
	if stdinRep.Integrate.FinalSHA != rep.Integrate.FinalSHA {
		t.Fatalf("stdin report integrate.finalSHA=%q, want %q", stdinRep.Integrate.FinalSHA, rep.Integrate.FinalSHA)
	}
}

// TestDriveRunPublishNeverRunsOnVerifyFailure: -publish is gated strictly on
// the run having LANDED. A failing -verify returns before the base ref ever
// advances (driveRun's early return), so -publish must never run at all —
// not even to report a skip; rep.Publish stays nil, same as -publish unset.
func TestDriveRunPublishNeverRunsOnVerifyFailure(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)

	before, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "publish.marker")
	p := runParams{
		Repo:       repo,
		Base:       "main",
		Strategy:   "overlay",
		AgentCmd:   agent,
		VerifyCmd:  "go build ./...", // fails: brokenBuildTasks references undefined helper()
		PublishCmd: "touch " + marker,
	}
	rep, err := driveRun(ctx, p, brokenBuildTasks(t))
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep.Verify.OK {
		t.Fatal("test setup: verify.ok=true, want a broken build")
	}
	if rep.Publish != nil {
		t.Fatalf("rep.Publish=%+v, want nil: -publish must never run on a failed verify", rep.Publish)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the -publish command ran despite verify failing")
	}
	after, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("base ref advanced to %s on verify failure; must stay at %s", after, before)
	}
	if code := runExitCode(rep); code != exitVerifyFailed {
		t.Fatalf("runExitCode=%d, want exitVerifyFailed(%d)", code, exitVerifyFailed)
	}
}

// TestDriveRunPublishFailureStillLandsAndReportsExit6: a -publish command
// that fails must NOT unland the work (the base ref has already advanced by
// the time -publish runs) and must NOT flip verify's verdict — it's recorded
// honestly in its own rep.Publish field, and the run's exit code becomes
// exitPublishFailed(6) instead of exitOK, even though everything else about
// the run succeeded.
func TestDriveRunPublishFailureStillLandsAndReportsExit6(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	task := taskSpec{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:  "go build ./...", // passes: a.go alone compiles fine
		PublishCmd: "echo boom >&2; exit 3",
	}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep.Verify.OK {
		t.Fatalf("test setup: verify failed: %s", rep.Verify.Output)
	}
	if rep.Publish == nil {
		t.Fatal("rep.Publish is nil, want it populated (the run landed)")
	}
	if rep.Publish.OK {
		t.Fatal("rep.Publish.OK=true, want false: the publish command exited 3")
	}
	if rep.Publish.Exit != 3 {
		t.Fatalf("rep.Publish.Exit=%d, want 3", rep.Publish.Exit)
	}
	if !strings.Contains(rep.Publish.Output, "boom") {
		t.Fatalf("rep.Publish.Output=%q, want it to contain the command's stderr", rep.Publish.Output)
	}

	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != rep.Integrate.FinalSHA {
		t.Fatalf("main=%s, want it still advanced to the landed finalSHA=%s despite the publish failure", mainSHA, rep.Integrate.FinalSHA)
	}

	if code := runExitCode(rep); code != exitPublishFailed {
		t.Fatalf("runExitCode=%d, want exitPublishFailed(%d)", code, exitPublishFailed)
	}
}

// TestDriveRunPublishOffReportsNoPublishField: -publish unset (the default,
// empty PublishCmd) must leave rep.Publish nil on an otherwise clean landed
// run — and since it's an omitempty pointer, that means the JSON report has
// no "publish" key at all: a run without -publish is byte-identical to
// before -publish existed.
func TestDriveRunPublishOffReportsNoPublishField(t *testing.T) {
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	task := taskSpec{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}

	p := runParams{Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if rep.Publish != nil {
		t.Fatalf("rep.Publish=%+v, want nil when -publish was never set", rep.Publish)
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"publish"`) {
		t.Fatalf("report JSON contains a \"publish\" key despite -publish being unset:\n%s", data)
	}
	if code := runExitCode(rep); code != exitOK {
		t.Fatalf("runExitCode=%d, want exitOK", code)
	}
}

// TestDriveRunPublishTimeoutKillsHungPublish: -publish-timeout bounds a
// runaway publish command exactly like -agent-timeout bounds a runaway
// agent — the run must not hang for anywhere near the hung command's actual
// duration, and the failed publish must still be reported honestly (not
// silently dropped) without unlanding the work.
func TestDriveRunPublishTimeoutKillsHungPublish(t *testing.T) {
	ctx := context.Background()
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	task := taskSpec{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		PublishCmd: "sleep 5", PublishTimeout: 200 * time.Millisecond,
	}
	start := time.Now()
	rep, err := driveRun(ctx, p, []taskSpec{task})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("driveRun took %s; -publish-timeout 200ms should have cut the 5s sleep short", elapsed)
	}
	if rep.Publish == nil {
		t.Fatal("rep.Publish is nil, want it populated")
	}
	if rep.Publish.OK {
		t.Fatal("rep.Publish.OK=true, want false: the publish command was killed by -publish-timeout")
	}

	// Landing already happened before -publish ran; a timed-out publish must
	// not unland the work.
	mainSHA, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainSHA != rep.Integrate.FinalSHA {
		t.Fatalf("main=%s, want it still advanced to the landed finalSHA=%s despite the publish timeout", mainSHA, rep.Integrate.FinalSHA)
	}
	if code := runExitCode(rep); code != exitPublishFailed {
		t.Fatalf("runExitCode=%d, want exitPublishFailed(%d)", code, exitPublishFailed)
	}
}

// TestRunRunPublishFlagWired proves -publish/-publish-timeout are actually
// threaded from the CLI flags through runParams into driveRun (the tests
// above all drive runParams directly), and that -manifest's path — not its
// contents, driveRun never waits for runRun's later writeManifest call —
// reaches -publish as SIGBOUND_MANIFEST via the real flag-parsing path.
func TestRunRunPublishFlagWired(t *testing.T) {
	agent := buildTestAgent(t)
	_, repo := makeGoRepo(t)
	tasks := []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}}

	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	envOut := filepath.Join(t.TempDir(), "publish.env")
	publishCmd := fmt.Sprintf(`env > %q`, envOut)

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, tasks),
		"-agent", agent,
		"-manifest", manifestPath,
		"-publish", publishCmd,
		"-publish-timeout", "5s",
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if rep.Publish == nil || !rep.Publish.OK {
		t.Fatalf("rep.Publish=%+v, want ran=true ok=true", rep.Publish)
	}

	env := parseEnvFile(t, envOut)
	if got := env["SIGBOUND_MANIFEST"]; got != manifestPath {
		t.Fatalf("SIGBOUND_MANIFEST=%q, want the -manifest flag's path %q", got, manifestPath)
	}
	if got := env["SIGBOUND_BASE_BRANCH"]; got != "main" {
		t.Fatalf("SIGBOUND_BASE_BRANCH=%q, want %q", got, "main")
	}
	if got := env["SIGBOUND_FINAL_SHA"]; got != rep.Integrate.FinalSHA {
		t.Fatalf("SIGBOUND_FINAL_SHA=%q, want %q", got, rep.Integrate.FinalSHA)
	}
}

// TestWriteRunSummaryShowsResumed: the human -json=false summary marks a
// -resume'd agent's status line RESUMED, same visibility as the existing
// TIMEOUT annotation, and leaves a normal agent's line untouched.
func TestWriteRunSummaryShowsResumed(t *testing.T) {
	rep := runReport{
		Repo: "/repo", Base: "main",
		PerAgent: []perAgentJSON{
			{ID: "a", Branch: "agent/a", OK: true, Resumed: true},
			{ID: "b", Branch: "agent/b", OK: true},
		},
	}
	var buf bytes.Buffer
	if err := writeRunSummary(&buf, rep); err != nil {
		t.Fatalf("writeRunSummary: %v", err)
	}
	out := buf.String()
	aLine, bLine := "", ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "agent/a"):
			aLine = line
		case strings.Contains(line, "agent/b"):
			bLine = line
		}
	}
	if !strings.Contains(aLine, "RESUMED") {
		t.Fatalf("resumed agent's line = %q, want it to contain RESUMED", aLine)
	}
	if strings.Contains(bLine, "RESUMED") {
		t.Fatalf("non-resumed agent's line = %q, want no RESUMED", bLine)
	}
}

// ---- -env-mode / -env-* (issue #56) ----------------------------------------

// envScopedFixture builds a one-agent repo whose -agent both dumps its full
// environment to agentEnvOut and writes a.go (so -no-autocommit's opposite,
// Autocommit, lands it), and whose -verify just dumps its own environment to
// verifyEnvOut and exits 0. Shared by every agent/verify -env-mode test below.
func envScopedFixture(t *testing.T) (repo, agentEnvOut, verifyEnvOut string) {
	t.Helper()
	_, repo = makeGoRepo(t)
	dir := t.TempDir()
	agentEnvOut = filepath.Join(dir, "agent.env")
	verifyEnvOut = filepath.Join(dir, "verify.env")
	return repo, agentEnvOut, verifyEnvOut
}

// TestDriveRunEnvScopedStripsCanaryFromAgentAndVerify is -env-mode scoped's
// central case: a variable present in sigbound's OWN process environment
// (planted here, standing in for one tenant's secret in a hosted setting)
// must NOT reach either the -agent or the -verify command, while the base
// environment (PATH) and each slot's own SIGBOUND_* vars still do.
func TestDriveRunEnvScopedStripsCanaryFromAgentAndVerify(t *testing.T) {
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	ctx := context.Background()
	repo, agentEnvOut, verifyEnvOut := envScopedFixture(t)

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", Autocommit: true,
		AgentCmd:  fmt.Sprintf(`env > %q; printf 'package main\n\nfunc a() int { return 1 }\n' > a.go`, agentEnvOut),
		VerifyCmd: fmt.Sprintf(`env > %q`, verifyEnvOut),
		EnvMode:   envModeScoped,
	}
	task := taskSpec{ID: "a", Prompt: "n/a"}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.PerAgent) != 1 || !rep.PerAgent[0].OK {
		t.Fatalf("agent not ok: %+v", rep.PerAgent)
	}
	if !rep.Verify.Ran || !rep.Verify.OK {
		t.Fatalf("verify ran=%v ok=%v output=%q", rep.Verify.Ran, rep.Verify.OK, rep.Verify.Output)
	}
	if rep.EnvMode != envModeScoped {
		t.Fatalf("rep.EnvMode=%q, want %q", rep.EnvMode, envModeScoped)
	}

	agentEnv := parseEnvFile(t, agentEnvOut)
	verifyEnv := parseEnvFile(t, verifyEnvOut)
	for name, env := range map[string]map[string]string{"agent": agentEnv, "verify": verifyEnv} {
		if _, leaked := env["SIGBOUND_TEST_CANARY"]; leaked {
			t.Fatalf("%s: SIGBOUND_TEST_CANARY leaked into the scoped environment: %v", name, env)
		}
		if _, ok := env["PATH"]; !ok {
			t.Fatalf("%s: PATH missing from the scoped base environment", name)
		}
	}
	if agentEnv["SIGBOUND_TASK_ID"] != "a" || agentEnv["SIGBOUND_REPO"] != repo || agentEnv["SIGBOUND_BRANCH"] != "agent/a" {
		t.Fatalf("agent's own SIGBOUND_* vars did not arrive: %+v", agentEnv)
	}
}

// TestDriveRunEnvInheritKeepsCanaryTodaysBehavior: -env-mode inherit (and
// leaving EnvMode unset, its zero value) is today's behavior, unchanged — the
// full parent environment, canary included, reaches -agent.
func TestDriveRunEnvInheritKeepsCanaryTodaysBehavior(t *testing.T) {
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	ctx := context.Background()

	for _, mode := range []string{envModeInherit, ""} {
		repo, agentEnvOut, _ := envScopedFixture(t)
		p := runParams{
			Repo: repo, Base: "main", Strategy: "overlay", Autocommit: true,
			AgentCmd: fmt.Sprintf(`env > %q; printf 'package main\n\nfunc a() int { return 1 }\n' > a.go`, agentEnvOut),
			EnvMode:  mode,
		}
		rep, err := driveRun(ctx, p, []taskSpec{{ID: "a", Prompt: "n/a"}})
		if err != nil {
			t.Fatalf("mode %q: driveRun: %v", mode, err)
		}
		if len(rep.PerAgent) != 1 || !rep.PerAgent[0].OK {
			t.Fatalf("mode %q: agent not ok: %+v", mode, rep.PerAgent)
		}
		env := parseEnvFile(t, agentEnvOut)
		if env["SIGBOUND_TEST_CANARY"] != "leak-me" {
			t.Fatalf("mode %q: SIGBOUND_TEST_CANARY=%q, want it present (inherit is byte-identical to today)", mode, env["SIGBOUND_TEST_CANARY"])
		}
	}
}

// TestDriveRunEnvScopedAllowlistScopesPerSlot: -env-agent allowlists the
// canary through to -agent ONLY — the same run's -verify (no allowlist of
// its own) still never sees it. A second allowlisted name that isn't set in
// the parent at all is silently skipped, not surfaced as an error or an
// empty entry.
func TestDriveRunEnvScopedAllowlistScopesPerSlot(t *testing.T) {
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	ctx := context.Background()
	repo, agentEnvOut, verifyEnvOut := envScopedFixture(t)

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", Autocommit: true,
		AgentCmd:  fmt.Sprintf(`env > %q; printf 'package main\n\nfunc a() int { return 1 }\n' > a.go`, agentEnvOut),
		VerifyCmd: fmt.Sprintf(`env > %q`, verifyEnvOut),
		EnvMode:   envModeScoped,
		EnvAgent:  []string{"SIGBOUND_TEST_CANARY", "SIGBOUND_TEST_DOES_NOT_EXIST"},
	}
	rep, err := driveRun(ctx, p, []taskSpec{{ID: "a", Prompt: "n/a"}})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep.Verify.OK {
		t.Fatalf("verify not ok: %q", rep.Verify.Output)
	}

	agentEnv := parseEnvFile(t, agentEnvOut)
	if agentEnv["SIGBOUND_TEST_CANARY"] != "leak-me" {
		t.Fatalf("-env-agent should have allowlisted the canary through: %+v", agentEnv)
	}
	if _, present := agentEnv["SIGBOUND_TEST_DOES_NOT_EXIST"]; present {
		t.Fatalf("an allowlisted name unset in the parent must be skipped, not passed as empty: %+v", agentEnv)
	}

	verifyEnv := parseEnvFile(t, verifyEnvOut)
	if _, leaked := verifyEnv["SIGBOUND_TEST_CANARY"]; leaked {
		t.Fatalf("-env-agent's allowlist must not leak into -verify (no allowlist of its own): %+v", verifyEnv)
	}
}

// TestDriveRunEnvScopedAllowlistGlobSuffix: a NAME_* entry in -env-agent
// passes every parent var sharing that prefix, e.g. -env-agent
// SIGBOUND_TEST_* for a family of vars a model CLI expects.
func TestDriveRunEnvScopedAllowlistGlobSuffix(t *testing.T) {
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	t.Setenv("SIGBOUND_OTHER_NOMATCH", "should-not-match")
	ctx := context.Background()
	repo, agentEnvOut, _ := envScopedFixture(t)

	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", Autocommit: true,
		AgentCmd: fmt.Sprintf(`env > %q; printf 'package main\n\nfunc a() int { return 1 }\n' > a.go`, agentEnvOut),
		EnvMode:  envModeScoped,
		EnvAgent: []string{"SIGBOUND_TEST_*"},
	}
	rep, err := driveRun(ctx, p, []taskSpec{{ID: "a", Prompt: "n/a"}})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.PerAgent) != 1 || !rep.PerAgent[0].OK {
		t.Fatalf("agent not ok: %+v", rep.PerAgent)
	}
	env := parseEnvFile(t, agentEnvOut)
	if env["SIGBOUND_TEST_CANARY"] != "leak-me" {
		t.Fatalf("glob allowlist SIGBOUND_TEST_* should have passed SIGBOUND_TEST_CANARY: %+v", env)
	}
	if _, leaked := env["SIGBOUND_OTHER_NOMATCH"]; leaked {
		t.Fatalf("glob allowlist SIGBOUND_TEST_* over-matched a non-prefixed var: %+v", env)
	}
}

// TestDriveRunEnvScopedAppliesToResolver: -env-mode scoped reaches -resolver
// too, via the same integrateBranches/CommandResolver seam -verify and
// -agent go through. Two agents edit the same line of shared.txt (a real
// conflict merge-tree can't auto-resolve), forcing the resolver to run; it
// dumps its own environment (and still resolves the conflict, via a trivial
// union, so the run lands cleanly either way).
func TestDriveRunEnvScopedAppliesToResolver(t *testing.T) {
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	resolverEnvOut := filepath.Join(t.TempDir(), "resolver.env")

	tasks := []taskSpec{
		{ID: "t1", Prompt: mustJSON(t, map[string]any{
			"edit": map[string]any{"file": "shared.txt", "line": 5, "text": "t1-was-here"},
		})},
		{ID: "t2", Prompt: mustJSON(t, map[string]any{
			"edit": map[string]any{"file": "shared.txt", "line": 5, "text": "t2-was-here"},
		})},
	}
	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		ResolverCmd:     fmt.Sprintf(`env > %q; cat "$SIGBOUND_OURS" "$SIGBOUND_THEIRS"`, resolverEnvOut),
		ResolverTimeout: 10 * time.Second,
		EnvMode:         envModeScoped,
	}
	rep, err := driveRun(ctx, p, tasks)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if len(rep.Integrate.Flagged) != 0 {
		t.Fatalf("flagged=%v, want none (the union resolver should have resolved the conflict)", rep.Integrate.Flagged)
	}

	env := parseEnvFile(t, resolverEnvOut)
	if _, leaked := env["SIGBOUND_TEST_CANARY"]; leaked {
		t.Fatalf("SIGBOUND_TEST_CANARY leaked into a scoped -resolver: %+v", env)
	}
	if _, ok := env["PATH"]; !ok {
		t.Fatalf("PATH missing from the scoped -resolver environment")
	}
	if _, ok := env["SIGBOUND_PATH"]; !ok {
		t.Fatalf("-resolver's own SIGBOUND_PATH did not arrive: %+v", env)
	}
}

// TestDriveRunEnvScopedAppliesToRepairAndPublish: -env-mode scoped reaches
// -repair and -publish too. -verify fails until -repair creates fixed.txt
// (auto-committed by the repair loop itself), after which -verify passes and
// the run lands, triggering -publish. Both fixer and publisher dump their
// own environment.
func TestDriveRunEnvScopedAppliesToRepairAndPublish(t *testing.T) {
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	ctx := context.Background()
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	dir := t.TempDir()
	repairEnvOut := filepath.Join(dir, "repair.env")
	publishEnvOut := filepath.Join(dir, "publish.env")

	task := taskSpec{ID: "a", Prompt: mustJSON(t, map[string]any{
		"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
	})}
	p := runParams{
		Repo: repo, Base: "main", Strategy: "overlay", AgentCmd: agent,
		VerifyCmd:  "test -f fixed.txt",
		RepairCmd:  fmt.Sprintf(`env > %q; printf x > fixed.txt`, repairEnvOut),
		RepairMax:  1,
		PublishCmd: fmt.Sprintf(`env > %q`, publishEnvOut),
		EnvMode:    envModeScoped,
	}
	rep, err := driveRun(ctx, p, []taskSpec{task})
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !rep.Verify.OK || !rep.Verify.Repaired {
		t.Fatalf("verify ok=%v repaired=%v, want repair to have fixed it: %q", rep.Verify.OK, rep.Verify.Repaired, rep.Verify.Output)
	}
	if rep.Publish == nil || !rep.Publish.OK {
		t.Fatalf("rep.Publish=%+v, want ran=true ok=true (the run landed)", rep.Publish)
	}

	for name, path := range map[string]string{"repair": repairEnvOut, "publish": publishEnvOut} {
		env := parseEnvFile(t, path)
		if _, leaked := env["SIGBOUND_TEST_CANARY"]; leaked {
			t.Fatalf("%s: SIGBOUND_TEST_CANARY leaked into a scoped environment: %+v", name, env)
		}
		if _, ok := env["PATH"]; !ok {
			t.Fatalf("%s: PATH missing from the scoped base environment", name)
		}
	}
}

// TestRunRunEnvModeRejectsUnknownValue: -env-mode only accepts inherit|scoped,
// checked before any agent runs — same fail-fast posture as -lanes/-strategy.
func TestRunRunEnvModeRejectsUnknownValue(t *testing.T) {
	_, repo := makeGoRepo(t)
	var buf bytes.Buffer
	_, err := runRun(&buf, []string{
		"-repo", repo, "-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", "true", "-env-mode", "bogus",
	})
	if err == nil || !strings.Contains(err.Error(), "-env-mode") {
		t.Fatalf("err=%v, want an -env-mode complaint", err)
	}
}

// TestRunRunEnvAgentRejectsBareStar: a bare "*" in any slot's -env-*
// allowlist is rejected before any agent runs, naming the offending flag —
// it would otherwise silently pass NOTHING at runtime (see
// TestSlotEnvScopedBareStarMatchesNothing), which looks like "pass
// everything" to whoever wrote it and fails open with no error either way.
func TestRunRunEnvAgentRejectsBareStar(t *testing.T) {
	_, repo := makeGoRepo(t)
	var buf bytes.Buffer
	_, err := runRun(&buf, []string{
		"-repo", repo, "-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", "true", "-env-mode", "scoped", "-env-agent", "*",
	})
	if err == nil || !strings.Contains(err.Error(), "-env-agent") {
		t.Fatalf("err=%v, want an -env-agent complaint", err)
	}
}

// TestRunRunEnvModeAndAllowlistFlagsWireIntoReport: -env-mode/-env-agent
// parsed off argv reach runParams (proven end-to-end: the agent only sees
// the canary because -env-agent named it) and -env-mode is recorded on the
// report as provenance.
func TestRunRunEnvModeAndAllowlistFlagsWireIntoReport(t *testing.T) {
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	_, repo := makeGoRepo(t)
	agentEnvOut := filepath.Join(t.TempDir(), "agent.env")
	agentCmd := fmt.Sprintf(`env > %q; printf 'package main\n\nfunc a() int { return 1 }\n' > a.go`, agentEnvOut)

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", agentCmd,
		"-env-mode", "scoped",
		"-env-agent", "SIGBOUND_TEST_CANARY",
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}
	if rep.EnvMode != envModeScoped {
		t.Fatalf("rep.EnvMode=%q, want %q", rep.EnvMode, envModeScoped)
	}
	env := parseEnvFile(t, agentEnvOut)
	if env["SIGBOUND_TEST_CANARY"] != "leak-me" {
		t.Fatalf("-env-agent SIGBOUND_TEST_CANARY did not reach the agent: %+v", env)
	}
}
