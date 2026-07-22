// Auto-planner: turn one natural-language goal into N parallel, mostly-disjoint
// tasks. Conflict avoidance happens at PLAN time — the planner is told to split
// the goal into tasks that touch DISJOINT files/areas of THIS repo (using a
// compact repo map), so the agents the plan drives integrate cleanly instead of
// piling up merge conflicts after the fact.
//
// The model is a bring-your-own seam, mirroring cell.CommandResolver: a
// CommandPlanner runs a configured command (which can be a `claude` call) with
// the goal, the repo map, N, and a ready-to-send prompt in the environment, and
// reads the plan back from stdout as a JSON array [{"id","prompt","files"}]. The plan is
// validated STRICTLY: a bad plan returns an error and NO run ever starts.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Task is one unit of parallel work the planner emits. It is the same shape the
// -tasks file uses (taskSpec), so a planned []Task feeds driveRun unchanged.
type Task = taskSpec

// Planner turns a goal + repo map into up to n independent tasks.
type Planner interface {
	Plan(ctx context.Context, goal, repoMap string, n int) ([]Task, error)
}

// CommandPlanner is the bring-your-own-model seam. It runs an external command
// (Args[0] the program, Args[1:] its arguments — e.g. `sh -c '<claude call>'`)
// with these environment variables exported:
//
//	SIGBOUND_GOAL     the raw natural-language goal
//	SIGBOUND_REPOMAP  the compact repo map (see RepoMap)
//	SIGBOUND_N        the requested number of tasks
//	SIGBOUND_PROMPT   a ready-to-send prompt (goal + repo map + the disjoint-tasks
//	               instruction + the JSON schema) — the command can forward this
//	               straight to a model, or ignore it and build its own from the
//	               structured pieces above.
//
// The command must write the plan to STDOUT as a JSON array
// [{"id","prompt","files":[...]}]. The output is validated strictly (valid JSON
// array; 1..n tasks; each id non-empty, unique, slug-safe; each prompt non-empty;
// each files list non-empty, safe, and pairwise-disjoint across tasks). Any
// violation — a bad exit, a timeout, non-JSON, or a malformed plan — returns an
// error so a broken plan can never launch a broken run (fail-safe). An
// overlapping-but-otherwise-valid plan triggers one automatic re-plan first.
type CommandPlanner struct {
	// Args is the argv to run: Args[0] is the program, Args[1:] its arguments.
	Args []string
	// Timeout bounds the invocation. Zero means no explicit timeout (the
	// caller's context still applies).
	Timeout time.Duration
	// LogDir, when set, gets the planner command's full stdout+stderr for
	// every invocation (the first attempt and, on an overlap, the automatic
	// re-plan) appended to <LogDir>/planner.log. See openLog (run.go).
	LogDir string
}

// Plan runs the configured command and parses+validates its plan. See
// CommandPlanner. If the first plan is structurally valid but its declared
// file-sets OVERLAP, Plan makes exactly ONE automatic re-plan attempt, feeding
// the planner the overlap (which files, which tasks) via SIGBOUND_REPLAN and an
// appended note in SIGBOUND_PROMPT, and asks for a disjoint split. If the re-plan
// still overlaps — or the planner is unavailable — Plan returns an error naming
// the overlapping paths (fail-safe: never start a run on a plan known to
// collide). Non-overlap errors (bad JSON, missing files, etc.) fail immediately
// with no re-plan.
func (p *CommandPlanner) Plan(ctx context.Context, goal, repoMap string, n int) ([]Task, error) {
	if len(p.Args) == 0 {
		return nil, errors.New("CommandPlanner: empty Args")
	}
	if n < 1 {
		return nil, fmt.Errorf("CommandPlanner: n must be >= 1, got %d", n)
	}

	tasks, err := p.attempt(ctx, goal, repoMap, n, "")
	var oe *overlapError
	if errors.As(err, &oe) {
		// One re-plan attempt with the overlap fed back to the planner.
		tasks2, err2 := p.attempt(ctx, goal, repoMap, n, oe.feedback())
		if err2 == nil {
			return tasks2, nil
		}
		return nil, fmt.Errorf("plan overlaps and the automatic re-plan did not fix it: %w (re-plan attempt: %v)", oe, err2)
	}
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

// attempt runs the planner command once (with an optional overlap-feedback
// string) and parses+validates the output. A structurally-valid-but-overlapping
// plan is returned as a wrapped *overlapError so Plan can detect it and re-plan.
func (p *CommandPlanner) attempt(ctx context.Context, goal, repoMap string, n int, feedback string) ([]Task, error) {
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}

	prompt := DefaultPlanPrompt(goal, repoMap, n)
	if feedback != "" {
		prompt += "\n\nRE-PLAN REQUIRED — your previous plan was rejected:\n" + feedback + "\n"
	}

	cmd := exec.CommandContext(ctx, p.Args[0], p.Args[1:]...)
	// Bound the wait after a timeout/cancel so a hung planner whose grandchild
	// inherited our stdout pipe can't block past its timeout. See the note in
	// cell.CommandResolver.Resolve.
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(),
		"SIGBOUND_GOAL="+goal,
		"SIGBOUND_REPOMAP="+repoMap,
		"SIGBOUND_N="+strconv.Itoa(n),
		"SIGBOUND_PROMPT="+prompt,
		"SIGBOUND_REPLAN="+feedback, // empty on the first attempt; set on a re-plan
	)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	// With -logdir, both streams ALSO stream to a full log file (io.MultiWriter)
	// while out/errBuf keep parsing the plan / reporting failures as before.
	if logf := openLog(p.LogDir, "planner.log"); logf != nil {
		defer logf.Close()
		cmd.Stdout = io.MultiWriter(&out, logf)
		cmd.Stderr = io.MultiWriter(&errBuf, logf)
	}
	if err := cmd.Run(); err != nil {
		// Non-zero exit, timeout, or spawn failure. Fail safe: no plan, no run.
		return nil, fmt.Errorf("planner command failed: %w: %s", err, tail(errBuf.String(), 400))
	}

	tasks, err := parsePlan(out.Bytes(), n)
	if err != nil {
		// Preserve *overlapError through the wrap so Plan's errors.As can see it.
		return nil, fmt.Errorf("planner produced an invalid plan: %w", err)
	}
	return tasks, nil
}

// parsePlan strictly parses and validates the planner's stdout. A plan is valid
// iff it is a JSON array of 1..n objects, each with a non-empty, unique,
// slug-safe id, a non-empty prompt, and a non-empty "files" list of safe
// repo-relative paths (no absolute paths, no ".." segments). On top of the
// per-task checks the declared file-sets must be PAIRWISE-DISJOINT: no path may
// appear in two tasks. A disjoint-plan violation is returned as *overlapError
// (which names the colliding paths and tasks) so callers can attempt a re-plan;
// every other violation is a plain error. Anything invalid is fail-safe: a bad
// plan never starts a run.
func parsePlan(data []byte, n int) ([]Task, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("empty plan output")
	}
	var tasks []Task
	if err := json.Unmarshal(trimmed, &tasks); err != nil {
		return nil, fmt.Errorf("not a valid JSON array of {id,prompt,files}: %w", err)
	}
	if len(tasks) == 0 {
		return nil, errors.New("plan has no tasks")
	}
	if len(tasks) > n {
		return nil, fmt.Errorf("plan has %d tasks, more than n=%d", len(tasks), n)
	}
	seen := map[string]bool{}
	owner := map[string][]string{} // declared path -> task ids that claim it
	for i := range tasks {
		t := &tasks[i]
		id := strings.TrimSpace(t.ID)
		if id == "" {
			return nil, fmt.Errorf("task %d has an empty id", i)
		}
		if !slugSafe(id) {
			return nil, fmt.Errorf("task %d id %q is not slug-safe (allowed: A-Za-z0-9._-, not . or ..)", i, t.ID)
		}
		if seen[id] {
			return nil, fmt.Errorf("duplicate task id %q", id)
		}
		seen[id] = true
		if strings.TrimSpace(t.Prompt) == "" {
			return nil, fmt.Errorf("task %q has an empty prompt", id)
		}
		if len(t.Files) == 0 {
			return nil, fmt.Errorf("task %q declares no files (each task must list the exact files it will create or modify)", id)
		}
		inTask := map[string]bool{}
		for j, f := range t.Files {
			nf := strings.TrimSpace(filepath.ToSlash(f))
			if !relSafe(nf) {
				return nil, fmt.Errorf("task %q file %q is not a safe repo-relative path (no absolute paths, no \"..\")", id, f)
			}
			t.Files[j] = nf // store the normalized path for lane enforcement
			if !inTask[nf] {
				inTask[nf] = true
				owner[nf] = append(owner[nf], id)
			}
		}
	}
	// Pairwise-disjoint check: any path claimed by more than one task collides.
	var collisions []fileCollision
	for _, path := range sortedKeys(owner) {
		if ids := owner[path]; len(ids) > 1 {
			collisions = append(collisions, fileCollision{path: path, tasks: ids})
		}
	}
	if len(collisions) > 0 {
		return nil, &overlapError{collisions: collisions}
	}
	return tasks, nil
}

// overlapError reports that two or more tasks declared the same file — a
// violation of the pairwise-disjoint invariant. It names every colliding path
// and the tasks that claim it, and can render itself as re-plan feedback.
type overlapError struct {
	collisions []fileCollision
}

type fileCollision struct {
	path  string
	tasks []string
}

func (e *overlapError) Error() string {
	parts := make([]string, 0, len(e.collisions))
	for _, c := range e.collisions {
		parts = append(parts, fmt.Sprintf("%q claimed by tasks %s", c.path, strings.Join(c.tasks, ", ")))
	}
	return "plan is not pairwise-disjoint: " + strings.Join(parts, "; ")
}

// feedback renders the overlap as an instruction the planner can act on during a
// re-plan attempt.
func (e *overlapError) feedback() string {
	var b strings.Builder
	b.WriteString("Your previous plan declared the SAME file in more than one task, which is not allowed. Overlaps:\n")
	for _, c := range e.collisions {
		fmt.Fprintf(&b, "  - %s is claimed by tasks: %s\n", c.path, strings.Join(c.tasks, ", "))
	}
	b.WriteString("Re-split the GOAL so every task's \"files\" list is DISJOINT — no file may appear in more than one task. Prefer giving each task its own new files.")
	return b.String()
}

// sortedKeys returns the keys of m in ascending order (deterministic overlap
// reporting for tests and re-plan feedback).
func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// relSafe reports whether p is a safe repo-relative file path: non-empty, not
// absolute, and with no ".." segment (so a declared file cannot escape the repo
// or be confused with a diff path). Paths are compared forward-slashed, matching
// git's `diff --name-only` output.
func relSafe(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" || p == "." {
		return false
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// slugSafe reports whether s is safe to use as a git branch component and a
// worktree directory name: non-empty, only [A-Za-z0-9._-], and not the special
// path names "." / ".." (which would be valid characters but unsafe paths).
func slugSafe(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// DefaultPlanPrompt is the conflict-avoidance prompt wrapper CommandPlanner hands
// the model: split the goal into N INDEPENDENT tasks touching DISJOINT files so
// the resulting agents integrate cleanly, each task naming the files it owns, and
// return the plan as a bare JSON array.
func DefaultPlanPrompt(goal, repoMap string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You are the planner for a set of parallel coding agents. Split the GOAL below into
at most %d INDEPENDENT tasks that separate agents can do in parallel, each in its
own git worktree, after which their branches are merged back together automatically.

CRITICAL — conflict avoidance: the tasks must touch DISJOINT files / areas of the
repository so they merge with NO conflicts. Use the REPO MAP to choose the split.
Each task MUST declare, in a "files" array, the EXACT repo-relative paths it will
create or modify, and those file sets MUST NOT overlap across tasks (no path may
appear in two tasks). Prefer giving each task its own new files. Paths must be
repo-relative (no leading "/", no ".."). If the goal cannot be cleanly split into
%d disjoint tasks, return FEWER tasks — never overlapping ones. Each prompt must
be complete and self-contained (an agent sees only its own prompt, not the goal
or this message).

Return ONLY a JSON array (no prose, no markdown fences) of 1..%d objects:
  [{"id":"<short-slug>","prompt":"<self-contained instruction>","files":["path/one.go","path/two.go"]}]
Each "id" is a unique short slug matching [A-Za-z0-9._-]; each "files" list is
non-empty and disjoint from every other task's.

GOAL:
%s

REPO MAP:
%s
`, n, n, n, strings.TrimSpace(goal), strings.TrimSpace(repoMap))
	return b.String()
}

// planTasks is the run-command entry point: scan the repo map, build a
// CommandPlanner from plannerCmd, and return its validated plan. Any failure
// (missing planner, scan error, bad plan) returns an error and NO run is
// started. logDir (may be empty) is forwarded to CommandPlanner.LogDir.
func planTasks(ctx context.Context, repo, goal, plannerCmd string, n int, timeout time.Duration, logDir string) ([]taskSpec, error) {
	if strings.TrimSpace(plannerCmd) == "" {
		return nil, errors.New("-goal requires -planner (the command that produces the plan)")
	}
	if n < 1 {
		return nil, fmt.Errorf("-n must be >= 1, got %d", n)
	}
	repoMap, err := RepoMap(repo)
	if err != nil {
		return nil, fmt.Errorf("scan repo map: %w", err)
	}
	planner := &CommandPlanner{Args: []string{"sh", "-c", plannerCmd}, Timeout: timeout, LogDir: logDir}
	return planner.Plan(ctx, goal, repoMap, n)
}

// ---- repo map ---------------------------------------------------------------

const (
	maxRepoMapBytes = 16000 // hard cap on the emitted map; truncated past this
	maxDeclsPerFile = 12    // exported decls listed per Go file before "+N more"
	sniffBytes      = 512   // bytes read to decide whether a file is binary
)

// skipDir is the set of directory names never walked in the repo map.
var skipDir = map[string]bool{
	".git": true, "vendor": true, "node_modules": true, ".idea": true, ".vscode": true,
}

type fileEntry struct {
	name  string
	size  int64
	decls []string // exported Go decls (nil for non-Go / unparsed files)
}

// RepoMap returns a compact, deterministic description of repoPath: the file tree
// grouped by directory, each file with its size, and for Go files the top-level
// EXPORTED declarations (via go/parser). .git, vendor, node_modules and binary
// files are skipped. Output is capped at maxRepoMapBytes so it stays cheap to
// hand a model. It is the planner's view of the repo for choosing a disjoint split.
func RepoMap(repoPath string) (string, error) {
	info, err := os.Stat(repoPath)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repo map: %s is not a directory", repoPath)
	}

	byDir := map[string][]fileEntry{}
	nFiles := 0

	walkErr := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, don't abort the whole scan
		}
		if d.IsDir() {
			if path != repoPath && skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks, sockets, etc.
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		content, binary := readForMap(path, fi.Size())
		if binary {
			return nil // skip binaries
		}
		rel, err := filepath.Rel(repoPath, path)
		if err != nil {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." {
			dir = "."
		}
		e := fileEntry{name: filepath.Base(rel), size: fi.Size()}
		if strings.HasSuffix(e.name, ".go") && content != nil {
			e.decls = goExportedDecls(content)
		}
		byDir[dir] = append(byDir[dir], e)
		nFiles++
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}

	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	var b strings.Builder
	fmt.Fprintf(&b, "# repo map: %s (%d files, %d dirs)\n", filepath.Base(repoPath), nFiles, len(dirs))
	truncated := false
	for _, dir := range dirs {
		label := dir + "/"
		if dir == "." {
			label = "./"
		}
		fmt.Fprintf(&b, "%s\n", label)
		files := byDir[dir]
		sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
		for _, f := range files {
			line := "  " + f.name + "  (" + humanSize(f.size) + ")"
			if len(f.decls) > 0 {
				shown := f.decls
				extra := 0
				if len(shown) > maxDeclsPerFile {
					extra = len(shown) - maxDeclsPerFile
					shown = shown[:maxDeclsPerFile]
				}
				line += "  " + strings.Join(shown, ", ")
				if extra > 0 {
					line += fmt.Sprintf(", +%d more", extra)
				}
			}
			if b.Len()+len(line)+1 > maxRepoMapBytes {
				truncated = true
				break
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		if truncated {
			break
		}
	}
	if truncated {
		b.WriteString("... (repo map truncated)\n")
	}
	return b.String(), nil
}

// readForMap reads a file for the repo map. It returns the content only for
// small-enough text files (needed to parse Go decls) and reports whether the
// file looks binary (a NUL byte in the first sniffBytes). Large files are still
// listed by size but their content is not returned.
func readForMap(path string, size int64) (content []byte, binary bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	head := make([]byte, sniffBytes)
	nnn, _ := f.Read(head)
	head = head[:nnn]
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, true // binary
	}
	// Only pull the full body for reasonably-sized files (Go-decl parsing).
	if size > 512*1024 {
		return nil, false
	}
	rest, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return rest, false
}

// goExportedDecls returns the top-level exported declarations of one Go source
// file: functions, exported methods (as "(Recv).Method"), types, and top-level
// vars/consts. A file that fails to parse yields whatever decls were recovered
// (or none) — it never errors, so the map is best-effort.
func goExportedDecls(src []byte) []string {
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if f == nil {
		return nil
	}
	var out []string
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv := recvTypeName(d.Recv.List[0].Type)
				bare := strings.TrimPrefix(recv, "*")
				if ast.IsExported(bare) && ast.IsExported(d.Name.Name) {
					out = append(out, "("+recv+")."+d.Name.Name)
				}
			} else if ast.IsExported(d.Name.Name) {
				out = append(out, "func "+d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if ast.IsExported(s.Name.Name) {
						out = append(out, "type "+s.Name.Name)
					}
				case *ast.ValueSpec:
					for _, nm := range s.Names {
						if ast.IsExported(nm.Name) {
							out = append(out, nm.Name)
						}
					}
				}
			}
		}
	}
	return out
}

// recvTypeName renders a method receiver type name (handling *T and generic
// receivers T[P]) for the repo map.
func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	}
	return ""
}

// humanSize renders a byte count compactly (B / KB / MB) for the repo map.
func humanSize(n int64) string {
	switch {
	case n < 1024:
		return strconv.FormatInt(n, 10) + "B"
	case n < 1024*1024:
		return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + "KB"
	default:
		return strconv.FormatFloat(float64(n)/(1024*1024), 'f', 1, 64) + "MB"
	}
}
