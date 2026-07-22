// sig run is the orchestration driver: point it at a repo and a set of
// parallel tasks and it (a) runs one agent per task in its own isolated worktree
// off the base commit, (b) hands the successfully-committed branches to the cell
// integrator — the exact same code path `sig integrate` uses, via
// integrateBranches — advancing the base ref to the integrated commit, and (c)
// optionally verifies the integrated tree in a throwaway detached checkout.
//
//	sig run -repo PATH -base main -tasks tasks.json -agent './my-agent' \
//	          -strategy overlay [-assert] [-resolver 'CMD'] [-verify 'go build ./...'] [-json]
//
// The agent command is run once per task via `sh -c`, with cwd set to that
// task's worktree and these env vars exported:
//
//	SIGBOUND_TASK      the task prompt
//	SIGBOUND_TASK_ID   the task id
//	SIGBOUND_REPO      the target repo path
//	SIGBOUND_BRANCH    the branch the agent must commit to (agent/<id>)
//
// The agent is expected to edit files AND commit in that worktree; the driver
// reads the resulting branch head, never the main working tree.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

// taskSpec is one unit of parallel work, loaded from the -tasks JSON file or
// emitted by the planner. Files is the task's DECLARED file-set: the exact paths
// it is allowed to create/modify. The planner must fill it (and the plan is
// validated pairwise-disjoint); tasks from -tasks may omit it, in which case
// run-time lane enforcement is skipped for that task.
type taskSpec struct {
	ID     string   `json:"id"`
	Prompt string   `json:"prompt"`
	Files  []string `json:"files,omitempty"`
}

// perAgentJSON is the outcome of running one agent in its worktree.
type perAgentJSON struct {
	ID            string   `json:"id"`
	Branch        string   `json:"branch"`
	SHA           string   `json:"sha"`
	Files         []string `json:"files"`
	OK            bool     `json:"ok"`
	Exit          int      `json:"exit"`
	Autocommitted bool     `json:"autocommitted,omitempty"` // driver committed edits the agent left uncommitted
	// Lane accounting (see runAgent). DeclaredFiles is the task's declared
	// file-set; ActualFiles is the agent's real write-set (git diff base...head);
	// InLane is true when ActualFiles ⊆ DeclaredFiles; Strayed lists the paths the
	// agent wrote outside its lane.
	DeclaredFiles []string `json:"declaredFiles,omitempty"`
	ActualFiles   []string `json:"actualFiles,omitempty"`
	InLane        bool     `json:"inLane"`
	Strayed       []string `json:"strayed,omitempty"`
	Stderr        string   `json:"stderr,omitempty"`
	// WorktreeKept is the absolute path to the agent's worktree, set only when
	// -keep-failed retained it (i.e. the agent failed and teardown was skipped).
	// Empty for every successful agent, and for a failed one without -keep-failed.
	WorktreeKept string `json:"worktreeKept,omitempty"`
}

// integrateJSON is the cell integrator's result, projected for the report.
type integrateJSON struct {
	Strategy string        `json:"strategy"`
	Groups   int           `json:"groups"`
	Landed   []string      `json:"landed"`
	Flagged  []flaggedJSON `json:"flagged"`
	Resolved int           `json:"resolved"` // overlapping branches that still landed (auto-merged or resolver-resolved)
	FinalSHA string        `json:"finalSHA"`
	WallMs   int64         `json:"wallMs"`
}

// verifyJSON records whether/-how the -verify command fared on the integrated
// tree, plus the self-healing repair loop's outcome (see verifyWithRepair).
//
//   - Attempts is the number of repair attempts actually made (0 when verify
//     passed on the first try or no -repair was configured).
//   - Repaired is true iff verify FAILED initially and a repair made it pass.
//   - OK is the FINAL verdict: green either on the first try or after repair.
//     When repair never fixes it, OK stays false and Output is the last failure
//     — the driver reports honestly and never claims a false green.
//   - Flaky is true iff a -verify-retries retry was needed to reach a green
//     invocation (i.e. the first invocation on that tree failed but a later one
//     on the SAME tree passed) — see runVerifyRetry. -verify is supposed to be
//     deterministic; a flaky pass is surfaced, not silently swallowed.
//   - Repairs details each attempt (see repairAttemptJSON).
type verifyJSON struct {
	Ran      bool                `json:"ran"`
	OK       bool                `json:"ok"`
	Attempts int                 `json:"attempts"`
	Repaired bool                `json:"repaired"`
	Flaky    bool                `json:"flaky,omitempty"`
	Output   string              `json:"output"`
	Repairs  []repairAttemptJSON `json:"repairs,omitempty"`
}

// repairAttemptJSON is one turn of the repair loop: the fixer agent ran, the
// driver committed its edits (FilesTouched = what that commit changed vs. the
// prior head), and -verify was re-run (VerifyOK).
type repairAttemptJSON struct {
	N            int      `json:"n"`
	FilesTouched []string `json:"filesTouched"`
	VerifyOK     bool     `json:"verifyOk"`
}

// runReport is the full stdout contract of `sig run`.
type runReport struct {
	Repo      string         `json:"repo"`
	Base      string         `json:"base"`
	BaseSHA   string         `json:"baseSHA"`
	LaneMode  string         `json:"laneMode"` // lane enforcement mode: off|warn|strict
	Tasks     []taskSpec     `json:"tasks"`
	PerAgent  []perAgentJSON `json:"perAgent"`
	Integrate integrateJSON  `json:"integrate"`
	Verify    verifyJSON     `json:"verify"`
	LogDir    string         `json:"logDir,omitempty"` // set iff -logdir was given; full per-command logs live here
}

// sig run exit codes. An operational error (bad flags, a git/integrate
// failure, etc. — returned as err from runRun) always wins and exits 1.
// Among report-level outcomes (err == nil, the run completed), runExitCode
// applies this precedence, most to least severe: -verify failed beats no
// agent succeeded beats some branches flagged as conflicts; a clean
// landed+verified run (or -verify unset entirely) is 0. Usage errors from the
// top-level CLI invocation (unknown subcommand, missing args) exit 2 — see
// main.go — and never reach runRun. Documented in `sig run -h`.
const (
	exitOK               = 0
	exitOperationalError = 1
	// 2 is reserved for top-level CLI usage errors; see main.go.
	exitVerifyFailed     = 3
	exitFlagged          = 4
	exitNoAgentSucceeded = 5
)

// runExitCode maps a completed run's report to its exit code. It is the only
// place the report -> exit code mapping lives; see the exit-code constants
// above for the precedence it implements.
func runExitCode(rep runReport) int {
	if rep.Verify.Ran && !rep.Verify.OK {
		return exitVerifyFailed
	}
	anyAgentOK := false
	for _, a := range rep.PerAgent {
		if a.OK {
			anyAgentOK = true
			break
		}
	}
	if !anyAgentOK {
		return exitNoAgentSucceeded
	}
	if len(rep.Integrate.Flagged) > 0 {
		return exitFlagged
	}
	return exitOK
}

// runParams is the resolved configuration for one driver run.
type runParams struct {
	Repo            string
	Base            string
	Strategy        string
	Assert          bool // paranoid overlay-vs-merge-tree cross-check (see cell.WithAssert)
	AgentCmd        string
	ResolverCmd     string
	ResolverTimeout time.Duration
	VerifyCmd       string
	VerifyRetries   int    // re-run a FAILING -verify up to this many more times on the same tree before giving up (0 = today's behavior)
	RepairCmd       string // fixer-agent command run when -verify fails (empty => no repair loop)
	RepairMax       int    // max repair attempts before giving up honestly (default via flag)
	Autocommit      bool   // commit an agent's uncommitted edits when it made no commit itself
	LaneMode        string // lane enforcement: laneOff | laneWarn | laneStrict
	KeepFailed      bool   // keep a FAILED agent's worktree on disk instead of removing it
	LogDir          string // when set, full per-command stdout+stderr logs are written here (see openLog)
}

// repairFailureMax bounds the captured verify output handed to the fixer agent
// via SIGBOUND_FAILURE, so a runaway build log can't blow up the child's env.
const repairFailureMax = 8000

// Lane-enforcement modes for -lanes. A task's DECLARED file-set is a promise;
// its ACTUAL write-set (git diff base...head) is what the agent really touched.
// An agent is "out of lane" when its actual write-set is not a subset of the
// declared set. Combined with a plan validated pairwise-disjoint at plan time,
// keeping every agent in-lane makes the landed branches actually-disjoint by
// construction.
//
//   - laneOff: ignore lanes entirely.
//   - laneWarn (default): record strayed files in the report but still land the
//     agent. This is BEST-EFFORT — an out-of-lane agent can still collide with
//     another; warn only surfaces it after the fact.
//   - laneStrict: treat out-of-lane as a failed agent — do NOT land it, and
//     record why. This is the REAL guarantee: only in-lane agents land, so the
//     declared-disjoint invariant is preserved on the base branch.
const (
	laneOff    = "off"
	laneWarn   = "warn"
	laneStrict = "strict"
)

// validateLaneMode rejects any -lanes value that is not a known mode.
func validateLaneMode(m string) error {
	switch m {
	case laneOff, laneWarn, laneStrict:
		return nil
	default:
		return fmt.Errorf("unknown -lanes mode %q (want off|warn|strict)", m)
	}
}

// runRun parses flags, drives the run, and returns the process exit code
// alongside an error. err is non-nil only for operational failures (bad
// flags, a git/integrate failure, etc.); the caller (main) prints it and the
// returned code is exitOperationalError. When err is nil, code is either
// exitOK (including `-h`, which just prints usage) or one of the
// report-level codes from runExitCode.
func runRun(w io.Writer, argv []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig run -repo PATH -base BRANCH (-tasks FILE | -goal STRING -planner CMD [-n N] [-min-tasks N]) -agent CMD [-strategy overlay] [-assert] [-resolver CMD] [-verify CMD [-verify-retries N] [-repair CMD [-repair-max N]]] [-lanes off|warn|strict] [-no-autocommit] [-keep-failed] [-logdir DIR] [-json]")
		fs.PrintDefaults()
		fmt.Fprintln(fs.Output(), "\nexit codes:")
		fmt.Fprintln(fs.Output(), "  0  landed and verified (or -verify unset)")
		fmt.Fprintln(fs.Output(), "  1  operational error (bad flags, a git/integrate failure, etc.)")
		fmt.Fprintln(fs.Output(), "  2  usage error (bad top-level sig invocation)")
		fmt.Fprintln(fs.Output(), "  3  -verify failed; nothing landed")
		fmt.Fprintln(fs.Output(), "  4  one or more branches flagged as conflicts (the rest landed)")
		fmt.Fprintln(fs.Output(), "  5  no agent succeeded")
	}
	repo := fs.String("repo", "", "path to the target git repository")
	base := fs.String("base", "main", "base branch the agents fork from and the result lands onto")
	tasksFile := fs.String("tasks", "", `path to a JSON file: [{"id":"..","prompt":".."}] (mutually exclusive with -goal)`)
	goal := fs.String("goal", "", "natural-language goal; the -planner turns it into parallel disjoint tasks (mutually exclusive with -tasks)")
	plannerCmd := fs.String("planner", "", "planner command (run via `sh -c`), required with -goal: reads SIGBOUND_GOAL/SIGBOUND_REPOMAP/SIGBOUND_N/SIGBOUND_PROMPT and writes a JSON task array [{\"id\",\"prompt\"}] to stdout")
	n := fs.Int("n", 4, "number of parallel tasks the -planner should produce from -goal")
	minTasks := fs.Int("min-tasks", 0, "minimum number of tasks a -goal plan must produce; fewer fails before any agent runs (fail-safe; 0 = no floor). Must be <= -n")
	plannerTimeout := fs.Duration("planner-timeout", 120*time.Second, "timeout for the -planner command (0 = none)")
	agentCmd := fs.String("agent", "", "shell command (run via `sh -c`, once per task) that edits files (and optionally commits) in the task's worktree; "+
		"receives SIGBOUND_TASK, SIGBOUND_TASK_ID, SIGBOUND_REPO, SIGBOUND_BRANCH env vars with cwd=the worktree")
	strategy := fs.String("strategy", cell.StrategyOverlay, "integration strategy: "+strings.Join(cell.AvailableStrategies(), ", "))
	assert := fs.Bool("assert", false, "paranoid cross-check for -strategy overlay: independently recompute the combine via merge-tree and error (never land) on any tree mismatch. "+
		"Roughly doubles integration cost (it re-merges everything); for paranoia/CI, not routine use")
	resolverCmd := fs.String("resolver", "", "optional conflict resolver command (see `sig integrate -h`); reads SIGBOUND_BASE/SIGBOUND_OURS/SIGBOUND_THEIRS/SIGBOUND_PATH, writes the resolved body to stdout")
	resolverTimeout := fs.Duration("resolver-timeout", 30*time.Second, "per-conflict timeout for -resolver (0 = none)")
	verifyCmd := fs.String("verify", "", "optional command run (via `sh -c`) in a detached checkout of the integrated tree; non-zero exit => verify failed")
	verifyRetries := fs.Int("verify-retries", 0, "after a FAILING -verify invocation, re-run it up to N more times on the same tree; passes on any green. "+
		"A pass on a retry marks the report flaky=true (the passing run's output is reported). 0 = today's behavior: a single failing invocation fails verify. "+
		"-verify must still be deterministic — retries mitigate flaky suites, not a license to skip that")
	repairCmd := fs.String("repair", "", "optional self-healing fixer command (run via `sh -c`) invoked in a worktree at the integrated head when -verify FAILS; "+
		"receives SIGBOUND_FAILURE (captured verify output) + SIGBOUND_REPO, edits files to fix the failure (the driver auto-commits them), then -verify re-runs. Looped up to -repair-max times")
	repairMax := fs.Int("repair-max", 2, "max -repair attempts before reporting verify.ok=false honestly (only used with -repair)")
	noAutocommit := fs.Bool("no-autocommit", false, "do NOT commit an agent's uncommitted edits; by default the driver stages+commits edits an agent left uncommitted so edit-only agents still land")
	lanes := fs.String("lanes", laneWarn, "lane enforcement for declared file-sets: off (ignore) | warn (record out-of-lane writes, still land) | strict (out-of-lane => failed agent, not landed). warn is best-effort; strict is the real disjointness guarantee. "+
		"Default is warn, EXCEPT a planned run (-goal) with -lanes not set explicitly defaults to strict, since the planner already promised a disjoint split")
	keepFailed := fs.Bool("keep-failed", false, "keep a FAILED agent's worktree on disk instead of removing it, so it can be inspected; the path is printed and recorded in the report. Successful agents' worktrees are always removed")
	logDir := fs.String("logdir", "", "when set, write each agent/repair/verify/planner command's FULL stdout+stderr to <logdir>/<name>.log "+
		"(agent-<id>.log, repair-<n>.log, verify-<n>.log, planner.log), in addition to the truncated tails the report keeps in memory. "+
		"The directory is created if needed and must be writable; checked before any agent runs")
	asJSON := fs.Bool("json", false, "emit the full JSON report (default: a terse human summary)")

	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitOK, nil
		}
		return exitOperationalError, err
	}
	// lanesExplicit tracks whether the caller passed -lanes (vs. relying on its
	// zero-value default), via fs.Visit (only visits flags actually set on the
	// command line). Needed below: a planned run defaults to strict UNLESS the
	// caller set -lanes explicitly, in which case that choice always wins.
	lanesExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "lanes" {
			lanesExplicit = true
		}
	})
	if *repo == "" {
		return exitOperationalError, errors.New("-repo is required")
	}
	if strings.TrimSpace(*agentCmd) == "" {
		return exitOperationalError, errors.New("-agent is required")
	}
	if err := validateStrategy(*strategy); err != nil {
		return exitOperationalError, err
	}
	if err := validateLaneMode(*lanes); err != nil {
		return exitOperationalError, err
	}
	if *minTasks > *n {
		return exitOperationalError, fmt.Errorf("-min-tasks %d exceeds -n %d", *minTasks, *n)
	}
	// Cheap preflight: git present + version >= 2.38, before touching the repo
	// or spawning any agent. The engine hard-depends on merge-tree/overlay
	// plumbing that only exists from 2.38 onward; catching that here turns a
	// cryptic mid-run "merge-tree exit N" into one actionable line. This does
	// NOT exercise the plumbing itself (too slow to run on every invocation) —
	// see `sig doctor` for the live probe.
	if err := gitx.CheckMinVersion(context.Background(), "git"); err != nil {
		return exitOperationalError, err
	}

	// -logdir is validated (created + confirmed writable) before any agent or
	// planner command runs, same as every other fail-safe check above: a bad
	// -logdir must fail the run up front, never silently drop logs partway
	// through. See openLog for how it's used once streaming starts.
	logDirAbs := strings.TrimSpace(*logDir)
	if logDirAbs != "" {
		if err := prepareLogDir(logDirAbs); err != nil {
			return exitOperationalError, err
		}
	}

	// Task source: exactly one of -tasks (explicit) or -goal (planned). If both
	// are set it is an error rather than silently ignoring one.
	haveTasks := strings.TrimSpace(*tasksFile) != ""
	haveGoal := strings.TrimSpace(*goal) != ""
	switch {
	case haveTasks && haveGoal:
		return exitOperationalError, errors.New("-tasks and -goal are mutually exclusive; pass exactly one")
	case !haveTasks && !haveGoal:
		return exitOperationalError, errors.New("one of -tasks or -goal is required")
	}

	var tasks []taskSpec
	var err error
	if haveTasks {
		tasks, err = loadTasks(*tasksFile)
		if err != nil {
			return exitOperationalError, err
		}
		if len(tasks) == 0 {
			return exitOperationalError, errors.New("no tasks in -tasks file")
		}
	} else {
		// Plan from the goal. A bad plan returns an error here — before any agent
		// runs — so a broken plan never launches a broken run (fail-safe).
		tasks, err = planTasks(context.Background(), *repo, *goal, *plannerCmd, *n, *plannerTimeout, logDirAbs)
		if err != nil {
			return exitOperationalError, err
		}
		// -min-tasks is a fail-safe floor: DefaultPlanPrompt explicitly permits the
		// planner to return FEWER than -n tasks when the goal doesn't split cleanly,
		// so a degenerate single-task plan otherwise passes with no signal. Checked
		// BEFORE any agent runs, same as every other plan validation in planTasks.
		if *minTasks > 0 && len(tasks) < *minTasks {
			return exitOperationalError, fmt.Errorf("planner produced %d tasks, -min-tasks %d", len(tasks), *minTasks)
		}
		// Fewer tasks than requested is allowed (and not floored out above), but
		// under-parallelizing should never be silent — surface it on stderr.
		if len(tasks) < *n {
			fmt.Fprintf(os.Stderr, "warning: planner produced %d tasks, requested -n %d\n", len(tasks), *n)
		}
	}

	// Lane enforcement default: -lanes itself defaults to warn (declared above),
	// but a PLANNED run (-goal) gets strict by default instead — the planner
	// already promised a pairwise-disjoint split, and strict is the only mode
	// that actually preserves that invariant on land. An explicit -lanes always
	// wins over this. -tasks runs are unaffected (warn, as before).
	laneMode := *lanes
	if haveGoal && !lanesExplicit {
		laneMode = laneStrict
	}

	p := runParams{
		Repo:            *repo,
		Base:            *base,
		Strategy:        *strategy,
		Assert:          *assert,
		AgentCmd:        *agentCmd,
		ResolverCmd:     *resolverCmd,
		ResolverTimeout: *resolverTimeout,
		VerifyCmd:       *verifyCmd,
		VerifyRetries:   *verifyRetries,
		RepairCmd:       *repairCmd,
		RepairMax:       *repairMax,
		Autocommit:      !*noAutocommit,
		LaneMode:        laneMode,
		KeepFailed:      *keepFailed,
		LogDir:          logDirAbs,
	}
	rep, err := driveRun(context.Background(), p, tasks)
	if err != nil {
		// A mid-run failure (e.g. integrate) can still leave real work behind: every
		// agent that finished has a committed agent/<id> branch, and rep already
		// names them. Emit what driveRun assembled before surfacing the error, so
		// the operator can recover it instead of losing it to a discarded report
		// (this is also the precondition for -resume to find prior branches). A
		// report with no agents (e.g. the base ref itself never resolved) has
		// nothing worth printing, so stays silent, same as before. Any write error
		// here is swallowed on purpose: err is the operational failure that matters
		// and must reach stderr unchanged.
		if len(rep.PerAgent) > 0 {
			_ = emitReport(w, rep, *asJSON)
		}
		return exitOperationalError, err
	}
	code := runExitCode(rep)
	if err := emitReport(w, rep, *asJSON); err != nil {
		return exitOperationalError, err
	}
	return code, nil
}

// emitReport writes rep to w as the full JSON report (asJSON) or the terse
// human summary — the one place `sig run`'s stdout contract is rendered, used
// both for a completed run and for the partial report on a mid-run error.
func emitReport(w io.Writer, rep runReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return writeRunSummary(w, rep)
}

// loadTasks reads and validates the -tasks JSON array.
func loadTasks(path string) ([]taskSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	var tasks []taskSpec
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("parse tasks JSON: %w", err)
	}
	seen := map[string]bool{}
	for i, t := range tasks {
		if strings.TrimSpace(t.ID) == "" {
			return nil, fmt.Errorf("task %d has an empty id", i)
		}
		if seen[t.ID] {
			return nil, fmt.Errorf("duplicate task id %q", t.ID)
		}
		seen[t.ID] = true
	}
	return tasks, nil
}

// driveRun executes the full orchestration: fan out agents, integrate, verify.
// It is separated from flag parsing so tests can drive it in-process.
func driveRun(ctx context.Context, p runParams, tasks []taskSpec) (runReport, error) {
	g := gitx.New(p.Repo)

	// Pin the base to a stable commit so every agent forks the same tree and the
	// merge-base stays fixed even after we advance the branch ref.
	baseSHA, err := g.RevParse(ctx, p.Base)
	if err != nil {
		return runReport{}, fmt.Errorf("resolve base %q in %s: %w", p.Base, p.Repo, err)
	}

	wtRoot, err := os.MkdirTemp("", "sig-run-*")
	if err != nil {
		return runReport{}, err
	}

	if p.LaneMode == "" {
		p.LaneMode = laneWarn // default when driven in-process without flag parsing
	}
	rep := runReport{Repo: p.Repo, Base: p.Base, BaseSHA: baseSHA, LaneMode: p.LaneMode, Tasks: tasks, LogDir: p.LogDir}
	// Normally wtRoot (and every per-agent worktree under it) is torn down when
	// the run ends. -keep-failed retains individual failed agents' worktrees
	// (see runAgent); when any survive, leave wtRoot itself in place too, or
	// this blanket cleanup would delete the very thing -keep-failed kept.
	defer func() {
		for _, a := range rep.PerAgent {
			if a.WorktreeKept != "" {
				return
			}
		}
		os.RemoveAll(wtRoot)
	}()

	// ---- fan out: one agent per task, each in its own isolated worktree ----
	agents := make([]perAgentJSON, len(tasks))
	var admin sync.Mutex // serialize git worktree add/remove admin steps
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, runtime.GOMAXPROCS(0)))
	for i := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			agents[i] = runAgent(ctx, g, &admin, wtRoot, baseSHA, p, tasks[i])
		}(i)
	}
	wg.Wait()
	rep.PerAgent = agents

	// Only branches the agent actually committed to (exit 0, head advanced) are
	// candidates for integration; failures/no-ops are left out, never landed.
	// writeSets reuses each agent's ACTUAL write-set (a.Files, already computed
	// above for lane enforcement) so integrateBranches doesn't re-diff every
	// branch it was just handed.
	var branches []string
	writeSets := make(map[string][]string, len(agents))
	for _, a := range agents {
		if a.OK {
			branches = append(branches, a.Branch)
			writeSets[a.Branch] = a.Files
		}
	}

	// ---- integrate via the shared cell path, WITHOUT landing yet ----
	// The integrated commit is computed detached; the base ref is advanced only
	// after -verify passes (below), so a failing verify never lands a broken tree.
	start := time.Now()
	res, err := integrateBranches(ctx, g, p.Base, baseSHA, branches, writeSets, p.Strategy, p.ResolverCmd, p.ResolverTimeout, p.Assert, false)
	if err != nil {
		return rep, fmt.Errorf("integrate: %w", err)
	}
	wall := time.Since(start)

	ir := integrateJSON{
		Strategy: res.Strategy,
		Groups:   res.Groups,
		Landed:   res.Landed,
		Resolved: res.AutoMerged,
		FinalSHA: res.FinalSHA,
		WallMs:   wall.Milliseconds(),
		Flagged:  []flaggedJSON{},
	}
	if ir.Landed == nil {
		ir.Landed = []string{}
	}
	for _, f := range res.Flagged {
		ir.Flagged = append(ir.Flagged, flaggedJSON{Branch: f.Branch, Paths: f.Conflicts})
	}
	rep.Integrate = ir

	// ---- verify the integrated tree (self-healing via -repair), then land ----
	// Nothing is on the base ref yet. Land only if verify passes (or is unset);
	// on an honest failure leave the base ref at baseSHA, so a red run lands
	// nothing — matching the documented guarantee. verifyWithRepair may advance
	// the head via a repair; FinalSHA reports whatever we computed either way.
	landSHA := res.FinalSHA
	if strings.TrimSpace(p.VerifyCmd) != "" {
		rep.Verify, landSHA = verifyWithRepair(ctx, g, p, res.FinalSHA)
		rep.Integrate.FinalSHA = landSHA
		if !rep.Verify.OK {
			return rep, nil // verify failed, no repair fixed it: land nothing
		}
	}
	if err := g.UpdateRef(ctx, "refs/heads/"+p.Base, landSHA); err != nil {
		return rep, fmt.Errorf("land %s: %w", short(landSHA), err)
	}
	rep.Integrate.FinalSHA = landSHA
	return rep, nil
}

// verifyWithRepair runs -verify (with -verify-retries applied, see
// runVerifyRetry) on head and, when it fails and -repair is set, drives the
// self-healing repair loop: up to p.RepairMax times it materializes the
// current head in a worktree, runs the fixer agent there (SIGBOUND_FAILURE =
// the last verify output, SIGBOUND_REPO = repo), auto-commits whatever the fixer
// edited, advances head to that commit, and re-verifies (again with retries) —
// stopping as soon as verify passes. It returns the verify verdict (green
// either first-try or after repair; honestly false if never fixed) and the
// possibly-advanced head the base ref should point at. If -verify passes on
// the first try, no repair runs.
func verifyWithRepair(ctx context.Context, g *gitx.Git, p runParams, head string) (verifyJSON, string) {
	v := runVerifyRetry(ctx, g, head, p.VerifyCmd, p.VerifyRetries, p.LogDir, 0)
	// Green first try (possibly after a retry), or no repair configured/allowed
	// => done, loop never runs. v already carries Flaky, so it survives here.
	if v.OK || strings.TrimSpace(p.RepairCmd) == "" || p.RepairMax < 1 {
		return v, head
	}

	var repairs []repairAttemptJSON
	lastOutput := v.Output
	ok := false
	flaky := false
	for attempt := 1; attempt <= p.RepairMax; attempt++ {
		newHead, touched, rerr := runRepair(ctx, g, p, head, lastOutput, attempt)
		rec := repairAttemptJSON{N: attempt, FilesTouched: touched, VerifyOK: false}
		if rerr != nil || newHead == head {
			// Fixer failed to spawn/commit, or produced no change: record the
			// dead attempt and stop — re-verifying an unchanged tree is pointless.
			if rerr != nil {
				lastOutput = tail(lastOutput+"\n[repair attempt "+fmt.Sprintf("%d", attempt)+" error] "+rerr.Error(), 2000)
			}
			repairs = append(repairs, rec)
			break
		}
		head = newHead
		rv := runVerifyRetry(ctx, g, head, p.VerifyCmd, p.VerifyRetries, p.LogDir, attempt)
		lastOutput = rv.Output
		rec.VerifyOK = rv.OK
		repairs = append(repairs, rec)
		if rv.OK {
			ok = true
			flaky = rv.Flaky
			break
		}
	}

	// Reached only after an initial verify FAILURE, so ok==true means a repair
	// fixed it (Repaired=true). ok==false => honest failure with the last output.
	return verifyJSON{
		Ran:      true,
		OK:       ok,
		Attempts: len(repairs),
		Repaired: ok,
		Flaky:    flaky,
		Output:   lastOutput,
		Repairs:  repairs,
	}, head
}

// runVerifyRetry runs verifyCmd on head via runVerify and, on FAILURE, re-runs
// it up to retries more times on the SAME tree (head is never re-integrated or
// re-checked-out to a different commit — only re-verified) — passing on any
// green invocation. A pass on an invocation after the first is flaky: -verify
// is supposed to be deterministic, so retries are a mitigation for flaky test
// suites, not a license to skip that; the flaky pass is surfaced via
// verifyJSON.Flaky rather than silently reported as a clean first-try green.
// retries=0 reproduces the pre-existing behavior exactly: one invocation, no
// retry, never flaky. When all retries are exhausted, the last (failing)
// invocation's result is returned, same as today. logAttempt names this
// logical verify attempt (0 = pre-repair, N = after repair round N) for
// -logdir's verify-<n>.log; every retry within this call appends to the same
// file (see openLog).
func runVerifyRetry(ctx context.Context, g *gitx.Git, head, verifyCmd string, retries int, logDir string, logAttempt int) verifyJSON {
	v := runVerify(ctx, g, head, verifyCmd, logDir, logAttempt)
	for attempt := 0; !v.OK && attempt < retries; attempt++ {
		v = runVerify(ctx, g, head, verifyCmd, logDir, logAttempt)
		if v.OK {
			v.Flaky = true
		}
	}
	return v
}

// runRepair materializes head in a throwaway detached worktree, runs the -repair
// fixer agent there (cwd = worktree; SIGBOUND_FAILURE = the verify output that
// triggered it, truncated; SIGBOUND_REPO = repo), then auto-commits whatever the
// fixer edited — exactly like an edit-only agent (reusing the CommitAll path).
// It returns the new commit SHA (== head when the fixer changed nothing) and the
// files that commit touched vs. head. The main working tree is never touched.
// attempt is this repair round's 1-based number (see verifyWithRepair's loop),
// used only to name the -logdir log file (repair-<attempt>.log).
func runRepair(ctx context.Context, g *gitx.Git, p runParams, head, failure string, attempt int) (string, []string, error) {
	dir, err := os.MkdirTemp("", "sig-repair-*")
	if err != nil {
		return head, nil, err
	}
	defer os.RemoveAll(dir)

	wtPath := filepath.Join(dir, "wt")
	if err := g.WorktreeAddDetached(ctx, wtPath, head); err != nil {
		return head, nil, fmt.Errorf("repair worktree at %s: %w", short(head), err)
	}
	defer func() { _ = g.WorktreeRemove(ctx, wtPath) }()

	cmd := exec.CommandContext(ctx, "sh", "-c", p.RepairCmd)
	cmd.Dir = wtPath
	cmd.WaitDelay = 2 * time.Second // return promptly on cancel; see runAgent
	cmd.Env = append(os.Environ(),
		"SIGBOUND_FAILURE="+tail(failure, repairFailureMax),
		"SIGBOUND_REPO="+p.Repo,
	)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	// stdout is discarded; the fixer signals its work through file edits (which
	// the driver commits) exactly like an edit-only agent. With -logdir, both
	// streams also stream to a full log file; see runAgent for the same pattern.
	// The log is wrapped in bestEffortWriter (both here and for stdout, even
	// though stdout would otherwise be a bare *os.File) so a log-write failure
	// can never fail the repair command; wrapping stdout costs exec's
	// pipe+goroutine instead of a plain fd dup, which is fine since WaitDelay is
	// set above.
	if logf := openLog(p.LogDir, fmt.Sprintf("repair-%d.log", attempt)); logf != nil {
		defer logf.Close()
		blog := bestEffortWriter{logf}
		cmd.Stdout = blog
		cmd.Stderr = io.MultiWriter(&errBuf, blog)
	}
	if runErr := cmd.Run(); runErr != nil {
		return head, nil, fmt.Errorf("repair command: %w: %s", runErr, tail(errBuf.String(), 400))
	}

	wt := g.At(wtPath)
	newHead, herr := wt.HeadSHA(ctx)
	if herr != nil {
		return head, nil, herr
	}
	// Auto-commit the fixer's edits: a bring-your-own fixer that only edits (no
	// git) leaves work uncommitted, so the head never advances. If it committed
	// itself (head advanced) it is untouched; otherwise stage+commit its edits.
	if newHead == head {
		if dirty, derr := wt.HasUncommittedChanges(ctx); derr == nil && dirty {
			sha, cerr := wt.CommitAll(ctx, "repair: fix verify failure")
			if cerr != nil {
				return head, nil, fmt.Errorf("repair commit: %w", cerr)
			}
			newHead = sha
		}
	}
	if newHead == head {
		return head, nil, nil // fixer made no change
	}
	files, ferr := g.DiffNameOnly(ctx, head, newHead)
	if ferr != nil {
		files = nil
	}
	return newHead, files, nil
}

// runAgent creates a worktree on branch agent/<id> off base, runs the agent
// command there, and reports what it committed. The worktree is torn down
// after, UNLESS the agent ultimately FAILED (see the lane-enforcement block
// below, which can flip a.OK to false) and p.KeepFailed is set, in which case
// teardown is skipped and the path is recorded in a.WorktreeKept so it can be
// inspected. The branch (with the agent's commit, if any) survives for
// integration either way.
func runAgent(ctx context.Context, g *gitx.Git, admin *sync.Mutex, wtRoot, baseSHA string, p runParams, t taskSpec) (a perAgentJSON) {
	branch := "agent/" + t.ID
	a = perAgentJSON{ID: t.ID, Branch: branch, Files: []string{}}
	dir := filepath.Join(wtRoot, "wt-"+sanitizeID(t.ID))

	admin.Lock()
	err := g.WorktreeAdd(ctx, dir, branch, baseSHA)
	admin.Unlock()
	if err != nil {
		a.Exit = -1
		a.Stderr = "worktree add: " + err.Error()
		return a
	}
	// a is a named return: this defer runs after every downstream update to a
	// (including lane enforcement), so it sees the FINAL OK verdict.
	defer func() {
		if p.KeepFailed && !a.OK {
			a.WorktreeKept = dir
			return
		}
		admin.Lock()
		_ = g.WorktreeRemove(ctx, dir)
		admin.Unlock()
	}()

	cmd := exec.CommandContext(ctx, "sh", "-c", p.AgentCmd)
	cmd.Dir = dir
	// If the run is cancelled, WaitDelay force-closes inherited pipes so a hung
	// agent (or one that leaked a background process holding our stderr) can't
	// block the whole run. See cell.CommandResolver.Resolve for the mechanism.
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(),
		"SIGBOUND_TASK="+t.Prompt,
		"SIGBOUND_TASK_ID="+t.ID,
		"SIGBOUND_REPO="+p.Repo,
		"SIGBOUND_BRANCH="+branch,
	)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	// stdout is discarded; the agent signals its work through commits + exit code.
	// With -logdir, both streams ALSO stream to a full log file (io.MultiWriter)
	// while errBuf keeps capturing the same bounded in-memory tail as before.
	// The log is wrapped in bestEffortWriter (both here and for stdout, even
	// though stdout would otherwise be a bare *os.File) so a log-write failure
	// can never fail the agent; wrapping stdout costs exec's pipe+goroutine
	// instead of a plain fd dup, which is fine since WaitDelay is set above.
	if logf := openLog(p.LogDir, "agent-"+sanitizeID(t.ID)+".log"); logf != nil {
		defer logf.Close()
		blog := bestEffortWriter{logf}
		cmd.Stdout = blog
		cmd.Stderr = io.MultiWriter(&errBuf, blog)
	}
	runErr := cmd.Run()
	a.Exit = 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			a.Exit = ee.ExitCode()
		} else {
			a.Exit = -1 // could not spawn
		}
	}
	a.Stderr = tail(errBuf.String(), 800)

	// Read what the agent committed straight from the branch head — never the
	// main working tree.
	wt := g.At(dir)
	head, herr := wt.HeadSHA(ctx)

	// Auto-commit: a bring-your-own agent that can only edit (not run git) leaves
	// its work uncommitted, so the branch head never advances and nothing lands.
	// When the agent exited cleanly, made no commit of its own, but DID leave
	// changes in the worktree, the driver stages+commits them as the agent — so
	// edit-only agents land too. Agents that commit themselves (head advanced)
	// are untouched. Disabled by -no-autocommit.
	if p.Autocommit && runErr == nil && herr == nil && head == baseSHA {
		if dirty, derr := wt.HasUncommittedChanges(ctx); derr == nil && dirty {
			if sha, cerr := wt.CommitAll(ctx, "agent: "+t.ID); cerr == nil {
				head, herr = sha, nil
				a.Autocommitted = true
			}
		}
	}

	if herr == nil {
		a.SHA = head
	}
	a.OK = runErr == nil && herr == nil && head != "" && head != baseSHA
	if a.OK {
		// INVARIANT: an agent whose write-set cannot be computed must never enter
		// a write-set-driven partition. a.Files feeds integrateBranches (via
		// writeSets below) as the branch's write-set for overlap detection; if the
		// diff errors, a.Files would stay at its zero value ([]string{}), which
		// integrateBranches reads as a POSITIVE assertion of "touched nothing" —
		// silently routing an overlapping branch through the overlay path with
		// stale/wrong content. So a failed diff fails the agent instead. An empty
		// result with no error (len(files)==0, ferr==nil) is a legitimate no-op
		// agent and must NOT fail here.
		files, ferr := g.DiffNameOnly(ctx, baseSHA, head)
		if ferr != nil {
			a.OK = false
			a.Stderr = tail("diff "+short(baseSHA)+".."+short(head)+" failed: "+ferr.Error()+"\n"+a.Stderr, 800)
		} else if len(files) > 0 {
			a.Files = files
		}
	}

	// ---- lane enforcement ----
	// Compare the agent's ACTUAL write-set against the task's DECLARED file-set.
	// Only enforce when the task declared files (planner path) and the agent
	// actually landed a write-set; -tasks entries without Files are exempt.
	a.DeclaredFiles = t.Files
	a.ActualFiles = a.Files
	a.InLane = true
	if a.OK && p.LaneMode != laneOff && len(t.Files) > 0 {
		declared := make(map[string]bool, len(t.Files))
		for _, f := range t.Files {
			declared[f] = true
		}
		var strayed []string
		for _, f := range a.Files {
			if !declared[f] {
				strayed = append(strayed, f)
			}
		}
		if len(strayed) > 0 {
			a.Strayed = strayed
			a.InLane = false
			if p.LaneMode == laneStrict {
				// strict: an out-of-lane agent is a failed agent — it does not
				// land. Recorded here (never silently dropped).
				a.OK = false
				a.Stderr = tail("out-of-lane (strict): wrote outside declared files "+
					fmt.Sprintf("%v", strayed)+"; not landed\n"+a.Stderr, 800)
			}
		}
	}
	return a
}

// runVerify materializes finalSHA into a detached worktree and runs verifyCmd
// there. The main working tree is never touched. logAttempt names the
// -logdir log file (verify-<logAttempt>.log); see runVerifyRetry.
func runVerify(ctx context.Context, g *gitx.Git, finalSHA, verifyCmd string, logDir string, logAttempt int) verifyJSON {
	dir, err := os.MkdirTemp("", "sig-verify-*")
	if err != nil {
		return verifyJSON{Ran: true, OK: false, Output: err.Error()}
	}
	defer os.RemoveAll(dir)

	checkout := filepath.Join(dir, "wt")
	if err := g.WorktreeAddDetached(ctx, checkout, finalSHA); err != nil {
		return verifyJSON{Ran: true, OK: false, Output: "checkout " + finalSHA + ": " + err.Error()}
	}
	defer func() { _ = g.WorktreeRemove(ctx, checkout) }()

	cmd := exec.CommandContext(ctx, "sh", "-c", verifyCmd)
	cmd.Dir = checkout
	cmd.WaitDelay = 2 * time.Second // return promptly on cancel; see runAgent
	cmd.Env = os.Environ()
	// Combined output, same as CombinedOutput(): both streams into one buffer
	// (outBuf) for the bounded report tail. With -logdir, both ALSO stream to
	// a full log file; see runAgent for the same pattern.
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	if logf := openLog(logDir, fmt.Sprintf("verify-%d.log", logAttempt)); logf != nil {
		defer logf.Close()
		// bestEffortWriter: a log-write failure must never turn a passing verify
		// into OK=false. See bestEffortWriter's doc for the os/exec mechanism.
		cmd.Stdout = io.MultiWriter(&outBuf, bestEffortWriter{logf})
		cmd.Stderr = cmd.Stdout
	}
	err = cmd.Run()
	return verifyJSON{Ran: true, OK: err == nil, Output: tail(outBuf.String(), 2000)}
}

func writeRunSummary(w io.Writer, r runReport) error {
	fmt.Fprintf(w, "repo %s  base %s (%s)  lanes=%s\n", r.Repo, r.Base, short(r.BaseSHA), r.LaneMode)
	if r.LogDir != "" {
		fmt.Fprintf(w, "logs: %s\n", r.LogDir)
	}
	for _, a := range r.PerAgent {
		status := "ok"
		if !a.OK {
			status = fmt.Sprintf("FAILED(exit %d)", a.Exit)
		}
		fmt.Fprintf(w, "  agent %-12s %-16s %-16s files=%v\n", a.ID, a.Branch, status, a.Files)
		if len(a.Strayed) > 0 {
			fmt.Fprintf(w, "    out-of-lane: wrote %v outside declared %v\n", a.Strayed, a.DeclaredFiles)
		}
		if a.WorktreeKept != "" {
			fmt.Fprintf(w, "    kept worktree: %s\n", a.WorktreeKept)
		}
	}
	fmt.Fprintf(w, "integrate: strategy=%s groups=%d landed=%d flagged=%d resolved=%d final=%s (%dms)\n",
		r.Integrate.Strategy, r.Integrate.Groups, len(r.Integrate.Landed),
		len(r.Integrate.Flagged), r.Integrate.Resolved, short(r.Integrate.FinalSHA), r.Integrate.WallMs)
	for _, f := range r.Integrate.Flagged {
		fmt.Fprintf(w, "  flagged %s on %v\n", f.Branch, f.Paths)
	}
	if r.Verify.Ran {
		v := "PASS"
		if !r.Verify.OK {
			v = "FAIL"
		}
		fmt.Fprintf(w, "verify: %s", v)
		if r.Verify.Flaky {
			fmt.Fprint(w, "  flaky=true")
		}
		if r.Verify.Attempts > 0 {
			fmt.Fprintf(w, "  repaired=%v attempts=%d", r.Verify.Repaired, r.Verify.Attempts)
		}
		fmt.Fprintln(w)
		for _, a := range r.Verify.Repairs {
			outcome := "still-failing"
			if a.VerifyOK {
				outcome = "PASS"
			}
			fmt.Fprintf(w, "  repair #%d: touched=%v verify=%s\n", a.N, a.FilesTouched, outcome)
		}
	}
	return nil
}

// sanitizeID maps a task id to a safe directory-name component.
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "task"
	}
	return b.String()
}

// prepareLogDir creates dir (and its parents) for -logdir and confirms it is
// actually writable, so a bad -logdir fails before any agent or planner
// command runs instead of silently dropping every full-output log. MkdirAll
// alone would not catch an existing-but-unwritable directory, so this also
// probes with a throwaway file.
func prepareLogDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("-logdir %s: %w", dir, err)
	}
	probe, err := os.CreateTemp(dir, ".sigbound-logdir-check-*")
	if err != nil {
		return fmt.Errorf("-logdir %s is not writable: %w", dir, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return nil
}

// openLog opens <logDir>/name for a command's full stdout+stderr capture (see
// -logdir), appended so multiple invocations that share one logical attempt
// (-verify-retries re-running on the same tree, or a planner re-plan) land in
// the same file instead of clobbering each other. Returns nil when logDir is
// empty or the file can't be opened; -logdir's directory is validated once,
// up front (prepareLogDir), before any agent runs, so an open failure here is
// unexpected — when it happens that command simply runs without a log.
//
// Opening is only half the guarantee: every caller wraps the returned file in
// bestEffortWriter before attaching it to cmd.Stdout/Stderr, so a WRITE
// failure AFTER a successful open (disk full, permissions revoked mid-run,
// etc.) degrades the same way — the command that triggered it still runs and
// reports normally, never failed because of a log write. See bestEffortWriter
// for why that wrapping is required at every call site, not just here.
func openLog(logDir, name string) *os.File {
	if logDir == "" {
		return nil
	}
	f, err := openLogFile(filepath.Join(logDir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return f
}

// openLogFile is the file-open call openLog uses, factored into a var so a
// test can substitute a file that fails every Write (e.g. one opened
// O_RDONLY) to exercise the bestEffortWriter path end-to-end through a real
// agent run, without needing to fabricate an actual disk-full condition.
// Production code never reassigns this.
var openLogFile = os.OpenFile

// bestEffortWriter wraps a log-file writer so a failing Write can never fail
// the command being logged. os/exec promotes a copy-goroutine's write error
// into cmd.Run()'s returned error even when the child process itself exited
// 0: as soon as any of Stdout/Stderr is not a bare *os.File, exec adds a
// pipe+goroutine to service it, and that goroutine's write error overrides an
// otherwise-clean exit (see cmd.Wait in package os/exec). A -logdir write
// failure (disk full, log file closed out from under us, etc.) must never
// turn a successful agent/verify/repair/planner run into a reported failure,
// so every write here is swallowed and reported as a (full-length, nil-error)
// success — the command runs to completion either way; only the log is
// incomplete.
type bestEffortWriter struct {
	w io.Writer
}

func (b bestEffortWriter) Write(p []byte) (int, error) {
	b.w.Write(p) //nolint:errcheck // deliberately best-effort; see type doc
	return len(p), nil
}

// tail returns at most the last max characters of s, prefixed with an ellipsis
// when truncated.
func tail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "..." + s[len(s)-max:]
}

// short abbreviates a SHA for human output.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
