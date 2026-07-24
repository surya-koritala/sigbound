package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParsePlanValid: a well-formed plan (disjoint files) parses to tasks.
func TestParsePlanValid(t *testing.T) {
	tasks, err := parsePlan([]byte(`[{"id":"a","prompt":"do a in a.go","files":["a.go"]},{"id":"b_1","prompt":"do b in b.go","files":["b.go"]}]`), 4)
	if err != nil {
		t.Fatalf("parsePlan: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if tasks[0].ID != "a" || tasks[1].ID != "b_1" || tasks[1].Prompt != "do b in b.go" {
		t.Fatalf("unexpected tasks: %+v", tasks)
	}
	if len(tasks[0].Files) != 1 || tasks[0].Files[0] != "a.go" {
		t.Fatalf("task a files=%v, want [a.go]", tasks[0].Files)
	}
}

// TestParsePlanRejectsOverlap: two tasks declaring the same file collide. The
// error is an *overlapError that names the overlapping path (fail-safe: a plan
// known to collide must never run).
func TestParsePlanRejectsOverlap(t *testing.T) {
	in := `[{"id":"a","prompt":"x","files":["a.go","shared.go"]},{"id":"b","prompt":"y","files":["shared.go","b.go"]}]`
	_, err := parsePlan([]byte(in), 4)
	if err == nil {
		t.Fatal("overlapping files: want error, got nil")
	}
	var oe *overlapError
	if !errors.As(err, &oe) {
		t.Fatalf("want *overlapError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "shared.go") {
		t.Fatalf("error should name the overlapping path: %v", err)
	}
	// Only shared.go overlaps; a.go/b.go must not be reported as collisions.
	if strings.Contains(err.Error(), "\"a.go\"") || strings.Contains(err.Error(), "\"b.go\"") {
		t.Fatalf("non-overlapping paths reported as colliding: %v", err)
	}
}

// TestParsePlanAcceptsDisjoint: distinct files (incl. nested paths) are accepted
// and stored normalized.
func TestParsePlanAcceptsDisjoint(t *testing.T) {
	tasks, err := parsePlan([]byte(`[{"id":"a","prompt":"x","files":["a.go"]},{"id":"b","prompt":"y","files":["dir/b.go","c.go"]}]`), 4)
	if err != nil {
		t.Fatalf("disjoint plan: %v", err)
	}
	if len(tasks) != 2 || len(tasks[1].Files) != 2 || tasks[1].Files[0] != "dir/b.go" {
		t.Fatalf("unexpected: %+v", tasks)
	}
}

// TestParsePlanRejectsBadFiles: missing/empty files and unsafe paths (absolute,
// "..") are rejected.
func TestParsePlanRejectsBadFiles(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"missing files", `[{"id":"a","prompt":"x"}]`},
		{"empty files", `[{"id":"a","prompt":"x","files":[]}]`},
		{"absolute path", `[{"id":"a","prompt":"x","files":["/etc/passwd"]}]`},
		{"dotdot path", `[{"id":"a","prompt":"x","files":["../escape.go"]}]`},
		{"dotdot segment", `[{"id":"a","prompt":"x","files":["sub/../../x.go"]}]`},
		{"empty path", `[{"id":"a","prompt":"x","files":["  "]}]`},
	}
	for _, c := range cases {
		if _, err := parsePlan([]byte(c.in), 4); err == nil {
			t.Errorf("%s: want error, got nil", c.name)
		}
	}
}

// TestParsePlanRejects: every malformed plan is an error (fail-safe — a bad plan
// must never run a broken run).
func TestParsePlanRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
	}{
		{"not json", `not json`, 4},
		{"empty", ``, 4},
		{"object not array", `{"id":"a","prompt":"x"}`, 4},
		{"empty array", `[]`, 4},
		{"empty id", `[{"id":"","prompt":"x"}]`, 4},
		{"whitespace id", `[{"id":"  ","prompt":"x"}]`, 4},
		{"empty prompt", `[{"id":"a","prompt":"  "}]`, 4},
		{"duplicate id", `[{"id":"a","prompt":"x"},{"id":"a","prompt":"y"}]`, 4},
		{"slash in id", `[{"id":"a/b","prompt":"x"}]`, 4},
		{"space in id", `[{"id":"a b","prompt":"x"}]`, 4},
		{"dotdot id", `[{"id":"..","prompt":"x"}]`, 4},
		{"too many for n", `[{"id":"a","prompt":"x"},{"id":"b","prompt":"y"},{"id":"c","prompt":"z"}]`, 2},
		{"fenced json", "```json\n[{\"id\":\"a\",\"prompt\":\"x\"}]\n```", 4},
	}
	for _, c := range cases {
		if _, err := parsePlan([]byte(c.in), c.n); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}

// TestCommandPlannerParses: a mock planner command emitting fixed JSON is parsed
// to tasks.
func TestCommandPlannerParses(t *testing.T) {
	requirePOSIXShell(t) // planner command runs via `sh -c`
	p := &CommandPlanner{
		Args:    []string{"sh", "-c", `printf '%s' '[{"id":"x1","prompt":"touch x1.go","files":["x1.go"]},{"id":"x2","prompt":"touch x2.go","files":["x2.go"]}]'`},
		Timeout: 5 * time.Second,
	}
	tasks, err := p.Plan(context.Background(), "goal", "repomap", 4)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(tasks) != 2 || tasks[0].ID != "x1" || tasks[1].ID != "x2" {
		t.Fatalf("unexpected tasks: %+v", tasks)
	}
}

// TestCommandPlannerReplansOnOverlap: the planner overlaps on its first attempt
// but returns a DISJOINT plan when asked to re-plan (keyed on SIGBOUND_REPLAN). Plan
// performs the single automatic re-plan and returns the disjoint tasks.
func TestCommandPlannerReplansOnOverlap(t *testing.T) {
	requirePOSIXShell(t)
	overlap := `[{"id":"a","prompt":"x","files":["shared.go"]},{"id":"b","prompt":"y","files":["shared.go"]}]`
	disjoint := `[{"id":"a","prompt":"x","files":["a.go"]},{"id":"b","prompt":"y","files":["b.go"]}]`
	dir := t.TempDir()
	ov := filepath.Join(dir, "ov.json")
	dj := filepath.Join(dir, "dj.json")
	if err := os.WriteFile(ov, []byte(overlap), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dj, []byte(disjoint), 0o644); err != nil {
		t.Fatal(err)
	}
	// First call (SIGBOUND_REPLAN empty) => overlap; re-plan (SIGBOUND_REPLAN set) => disjoint.
	cmd := `if [ -n "$SIGBOUND_REPLAN" ]; then cat ` + dj + `; else cat ` + ov + `; fi`
	p := &CommandPlanner{Args: []string{"sh", "-c", cmd}}
	tasks, err := p.Plan(context.Background(), "goal", "map", 4)
	if err != nil {
		t.Fatalf("Plan should have re-planned to a disjoint plan: %v", err)
	}
	if len(tasks) != 2 || tasks[0].Files[0] != "a.go" || tasks[1].Files[0] != "b.go" {
		t.Fatalf("re-plan did not yield the disjoint plan: %+v", tasks)
	}
}

// TestCommandPlannerReplanStillOverlapsFails: a planner that overlaps on BOTH the
// first attempt and the re-plan makes Plan fail, naming the overlapping path (no
// run on a plan known to collide).
func TestCommandPlannerReplanStillOverlapsFails(t *testing.T) {
	requirePOSIXShell(t)
	overlap := `[{"id":"a","prompt":"x","files":["shared.go"]},{"id":"b","prompt":"y","files":["shared.go"]}]`
	// planFileCmd => `cat FILE`, so both the first attempt and the re-plan emit
	// the same overlapping plan.
	p := &CommandPlanner{Args: []string{"sh", "-c", planFileCmd(t, overlap)}}
	_, err := p.Plan(context.Background(), "g", "m", 4)
	if err == nil {
		t.Fatal("re-plan still overlaps: want error, got nil")
	}
	if !strings.Contains(err.Error(), "shared.go") {
		t.Fatalf("error should name the overlapping path: %v", err)
	}
}

// TestCommandPlannerFailsSafe: a non-zero exit, non-JSON output, or a plan with
// more tasks than n all return an error rather than a partial plan.
func TestCommandPlannerFailsSafe(t *testing.T) {
	requirePOSIXShell(t)
	nonZero := &CommandPlanner{Args: []string{"sh", "-c", "exit 3"}}
	if _, err := nonZero.Plan(context.Background(), "g", "r", 4); err == nil {
		t.Error("non-zero exit: want error, got nil")
	}
	badJSON := &CommandPlanner{Args: []string{"sh", "-c", "echo not-json"}}
	if _, err := badJSON.Plan(context.Background(), "g", "r", 4); err == nil {
		t.Error("bad json: want error, got nil")
	}
	tooMany := &CommandPlanner{Args: []string{"sh", "-c", `printf '%s' '[{"id":"a","prompt":"x"},{"id":"b","prompt":"y"}]'`}}
	if _, err := tooMany.Plan(context.Background(), "g", "r", 1); err == nil {
		t.Error("tasks > n: want error, got nil")
	}
}

// TestCommandPlannerPassesEnv: SIGBOUND_GOAL and SIGBOUND_N reach the command.
func TestCommandPlannerPassesEnv(t *testing.T) {
	requirePOSIXShell(t)
	p := &CommandPlanner{Args: []string{"sh", "-c", `printf '[{"id":"e","prompt":"goal=%s n=%s","files":["e.go"]}]' "$SIGBOUND_GOAL" "$SIGBOUND_N"`}}
	tasks, err := p.Plan(context.Background(), "my goal", "map", 7)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := tasks[0].Prompt; got != "goal=my goal n=7" {
		t.Fatalf("env not passed through: prompt=%q", got)
	}
}

// TestCommandPlannerEnvScopedStripsCanary: -env-mode scoped (via
// CommandPlanner.EnvMode) hides a variable set in this test process from the
// planner command, while its own SIGBOUND_* vars still arrive.
func TestCommandPlannerEnvScopedStripsCanary(t *testing.T) {
	requirePOSIXShell(t)
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	p := &CommandPlanner{
		Args:    []string{"sh", "-c", `printf '[{"id":"e","prompt":"canary=[%s] goal=%s","files":["e.go"]}]' "$SIGBOUND_TEST_CANARY" "$SIGBOUND_GOAL"`},
		EnvMode: envModeScoped,
	}
	tasks, err := p.Plan(context.Background(), "my goal", "map", 4)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := tasks[0].Prompt; got != "canary=[] goal=my goal" {
		t.Fatalf("prompt=%q, want the canary stripped but SIGBOUND_GOAL intact", got)
	}
}

// TestCommandPlannerEnvInheritKeepsCanary: leaving EnvMode unset (its zero
// value) is -env-mode inherit, today's behavior — the canary reaches the
// planner command.
func TestCommandPlannerEnvInheritKeepsCanary(t *testing.T) {
	requirePOSIXShell(t)
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	p := &CommandPlanner{
		Args: []string{"sh", "-c", `printf '[{"id":"e","prompt":"canary=[%s]","files":["e.go"]}]' "$SIGBOUND_TEST_CANARY"`},
	}
	tasks, err := p.Plan(context.Background(), "g", "m", 4)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := tasks[0].Prompt; got != "canary=[leak-me]" {
		t.Fatalf("prompt=%q, want the canary present (inherit is byte-identical to today)", got)
	}
}

// TestCommandPlannerEnvScopedAllowlistPassesGlob: EnvAllow's NAME_* glob
// passes a matching parent var through in scoped mode.
func TestCommandPlannerEnvScopedAllowlistPassesGlob(t *testing.T) {
	requirePOSIXShell(t)
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	p := &CommandPlanner{
		Args:     []string{"sh", "-c", `printf '[{"id":"e","prompt":"canary=[%s]","files":["e.go"]}]' "$SIGBOUND_TEST_CANARY"`},
		EnvMode:  envModeScoped,
		EnvAllow: []string{"SIGBOUND_TEST_*"},
	}
	tasks, err := p.Plan(context.Background(), "g", "m", 4)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := tasks[0].Prompt; got != "canary=[leak-me]" {
		t.Fatalf("prompt=%q, want the glob-allowlisted canary present", got)
	}
}

// TestPlanTasksThreadsEnvModeIntoCommandPlanner: planTasks (runRun's own
// entry point into the planner) forwards its envMode/envAllow params onto
// the CommandPlanner it builds — proven end-to-end the same way
// TestCommandPlannerEnvScopedStripsCanary proves it at the CommandPlanner
// level directly.
func TestPlanTasksThreadsEnvModeIntoCommandPlanner(t *testing.T) {
	requirePOSIXShell(t)
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	dir := t.TempDir()
	writeRepoFile(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	cmd := `printf '[{"id":"e","prompt":"canary=[%s]","files":["e.go"]}]' "$SIGBOUND_TEST_CANARY"`
	tasks, err := planTasks(context.Background(), dir, "goal", cmd, 4, 0, "", envModeScoped, nil)
	if err != nil {
		t.Fatalf("planTasks: %v", err)
	}
	if got := tasks[0].Prompt; got != "canary=[]" {
		t.Fatalf("prompt=%q, want the canary stripped by the scoped mode planTasks was given", got)
	}
}

// TestDefaultPlanPromptConflictAvoidance: the default wrapper instructs disjoint
// splitting and carries the goal, repo map, and n.
func TestDefaultPlanPromptConflictAvoidance(t *testing.T) {
	prompt := DefaultPlanPrompt("build the thing", "REPOMAP-CONTENT", 3)
	for _, want := range []string{"DISJOINT", "build the thing", "REPOMAP-CONTENT", "JSON array"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("default prompt missing %q:\n%s", want, prompt)
		}
	}
}

func writeRepoFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRepoMap: the map lists files with sizes and Go exported decls, groups by
// directory, and skips .git, binaries, and unexported decls.
func TestRepoMap(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, "go.mod", "module x\n\ngo 1.21\n")
	writeRepoFile(t, dir, "main.go", "package main\n\nfunc main() {}\n\nfunc Helper() int { return 1 }\n\ntype Widget struct{}\n")
	writeRepoFile(t, dir, "sub/util.go", "package sub\n\nfunc Exported() {}\n\nfunc unexportedFn() {}\n")
	writeRepoFile(t, dir, ".git/config", "[core]\n")            // must be skipped
	writeRepoFile(t, dir, "blob.bin", "head\x00\x01binarytail") // must be skipped (NUL)

	m, err := RepoMap(dir)
	if err != nil {
		t.Fatalf("RepoMap: %v", err)
	}
	for _, want := range []string{"main.go", "go.mod", "sub/", "func Helper", "type Widget", "func Exported"} {
		if !strings.Contains(m, want) {
			t.Errorf("repo map missing %q:\n%s", want, m)
		}
	}
	for _, no := range []string{"config", "blob.bin", "unexportedFn"} {
		if strings.Contains(m, no) {
			t.Errorf("repo map should not contain %q:\n%s", no, m)
		}
	}
}
