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
	// TimedOut is true iff -agent-timeout expired while this attempt's agent
	// command was running (see runAgent). Not set for a run-budget cancellation
	// (see runAgentWithRetries/driveRun) or an ordinary bad exit.
	TimedOut bool `json:"timedOut,omitempty"`
	// Attempts is the total number of times this task's agent was run,
	// including retries (see -agent-retries / runAgentWithRetries). 1 when the
	// first try succeeded or -agent-retries is 0.
	Attempts int `json:"attempts,omitempty"`
	// Resumed is true iff -resume reused this task's agent/<id> branch
	// outright instead of running its agent again (see runParams.Resume /
	// resumeAgent): the branch already differed from the recorded baseSHA, so
	// it already held real committed work from a PRIOR run. Always false when
	// the agent actually ran THIS run — whether that's because -resume was
	// off, or -resume was on but the branch was missing or a stale no-op (the
	// agent committed nothing last time) and so got a fresh run.
	Resumed bool `json:"resumed,omitempty"`
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
//   - Scope and ImpactedPkgs are set only when -verify-impact is configured
//     (see computeImpact): "impact" when the scoped -verify-impact command
//     actually ran (ImpactedPkgs names which packages, transitively expanded
//     to reverse-dependents), "full" when ANY doubt fell back to the full
//     -verify command instead. Both stay zero-valued — and so vanish from the
//     JSON report via omitempty — when -verify-impact isn't set at all, so a
//     run without it reports byte-identical to before this field existed.
//   - Cached is true iff -verify-cache is set AND this verdict was served
//     from a prior PASS on the exact same (tree OID, resolved verify
//     command) instead of actually running -verify — see runVerify and
//     verifycache.go. Always false when -verify-cache isn't set, and never
//     true for a failing verdict (only passes are ever cached).
//   - Repairs details each attempt (see repairAttemptJSON).
type verifyJSON struct {
	Ran          bool                `json:"ran"`
	OK           bool                `json:"ok"`
	Attempts     int                 `json:"attempts"`
	Repaired     bool                `json:"repaired"`
	Flaky        bool                `json:"flaky,omitempty"`
	Scope        string              `json:"scope,omitempty"`
	ImpactedPkgs []string            `json:"impactedPkgs,omitempty"`
	Cached       bool                `json:"cached,omitempty"`
	Output       string              `json:"output"`
	Repairs      []repairAttemptJSON `json:"repairs,omitempty"`
}

// repairAttemptJSON is one turn of the repair loop: the fixer agent ran, the
// driver committed its edits (FilesTouched = what that commit changed vs. the
// prior head), and -verify was re-run (VerifyOK).
type repairAttemptJSON struct {
	N            int      `json:"n"`
	FilesTouched []string `json:"filesTouched"`
	VerifyOK     bool     `json:"verifyOk"`
}

// runReport is the full stdout contract of `sig run` AND the manifest -manifest
// writes to disk (see -manifest below) — one format, not two: the manifest is
// simply this same report persisted, so a `sig replay` fed a -manifest file or
// a `-json` report has identical fields to work from.
type runReport struct {
	Repo     string         `json:"repo"`
	Base     string         `json:"base"`
	BaseSHA  string         `json:"baseSHA"`
	LaneMode string         `json:"laneMode"` // lane enforcement mode: off|warn|strict
	Tasks    []taskSpec     `json:"tasks"`
	PerAgent []perAgentJSON `json:"perAgent"`
	// Strategy is the integration strategy this run was configured with (see
	// -strategy). Duplicated at the top level even though Integrate.Strategy
	// already carries the strategy actually applied, so a manifest/replay can
	// read it without depending on integrate having run at all — e.g. a
	// mid-run operational error before integrate still leaves this populated.
	Strategy  string        `json:"strategy"`
	Integrate integrateJSON `json:"integrate"`
	Verify    verifyJSON    `json:"verify"`
	LogDir    string        `json:"logDir,omitempty"` // set iff -logdir was given; full per-command logs live here
	// AgentCmd/ResolverCmd/VerifyCmd/RepairCmd/PlannerCmd are the RESOLVED
	// command strings this run actually executed (after -*-preset expansion
	// and -config merging) — provenance for `sig replay` and for a human
	// asking "what actually ran". These are redacted NOTHING: they're the
	// user's own commands, verbatim, which can embed secrets (an API key
	// baked into a -verify command, say) — see docs/USAGE.md's Provenance
	// section. ResolverCmd/VerifyCmd/RepairCmd/PlannerCmd are empty (and
	// omitted from JSON) whenever that slot wasn't configured; AgentCmd is
	// always set (-agent is required).
	AgentCmd    string `json:"agentCmd"`
	ResolverCmd string `json:"resolverCmd,omitempty"`
	VerifyCmd   string `json:"verifyCmd,omitempty"`
	RepairCmd   string `json:"repairCmd,omitempty"`
	PlannerCmd  string `json:"plannerCmd,omitempty"`
	// Version is the sigbound version (see main.Version) that produced this
	// report — a manifest replayed under a different version is still
	// readable, but a version mismatch is a hint that strategy/verify
	// semantics may have drifted between the two.
	Version string `json:"version"`
	// StartedAt is when driveRun began, RFC3339 (UTC) — part of the
	// manifest's provenance: WHEN this ran, alongside WHAT ran (the commands
	// above) and WHERE it landed (BaseSHA / Integrate.FinalSHA).
	StartedAt string `json:"startedAt"`
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
	PlannerCmd      string // recorded on the report for provenance only; driveRun itself never plans — planning already happened by the time it's called
	ResolverCmd     string
	ResolverTimeout time.Duration
	VerifyCmd       string
	VerifyImpactCmd string        // optional command run INSTEAD of -verify when computeImpact can confidently scope to the impacted Go packages; requires VerifyCmd, which stays the fallback on any doubt (see runVerify)
	VerifyRetries   int           // re-run a FAILING -verify up to this many more times on the same tree before giving up (0 = today's behavior)
	VerifyCache     bool          // skip -verify when its exact (tree OID, resolved command) pair already has a cached PASS; see verifycache.go. Off by default (opt-in)
	RepairCmd       string        // fixer-agent command run when -verify fails (empty => no repair loop)
	RepairMax       int           // max repair attempts before giving up honestly (default via flag)
	Autocommit      bool          // commit an agent's uncommitted edits when it made no commit itself
	LaneMode        string        // lane enforcement: laneOff | laneWarn | laneStrict
	KeepFailed      bool          // keep a FAILED agent's worktree on disk instead of removing it
	LogDir          string        // when set, full per-command stdout+stderr logs are written here (see openLog)
	EventsPath      string        // when set, one JSON event per line is streamed here as the run progresses ("-" = stderr); see eventEmitter
	AgentTimeout    time.Duration // per-agent command timeout (0 = none); see runAgent
	AgentRetries    int           // retry a FAILED agent (bad exit or -agent-timeout, never a lane-strict stray) up to this many more times; see runAgentWithRetries
	Budget          time.Duration // wall-clock ceiling for the agent phase + integrate + verify (0 = none); see driveRun
	Notes           bool          // when the run LANDS, attach the report as a git note under refs/notes/sigbound on the landed commit; see -notes and attachNote
	// Resume and ResumeBaseSHA implement -resume (see resumeAgent): when
	// Resume is set, driveRun refuses to run at all unless the base branch's
	// CURRENT head is still exactly ResumeBaseSHA (the baseSHA the prior run
	// recorded) — resuming onto a base that has since moved would integrate
	// against the wrong tree — and every task's agent/<id> branch that
	// already differs from ResumeBaseSHA is reused as-is instead of run
	// again.
	Resume        bool
	ResumeBaseSHA string
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
		fmt.Fprintln(fs.Output(), "usage: sig run [-config PATH] -repo PATH -base BRANCH (-tasks FILE | -goal STRING (-planner CMD | -planner-preset claude|codex|aider) [-n N] [-min-tasks N] | -resume -manifest FILE) (-agent CMD | -agent-preset claude|codex|aider) [-agent-timeout D] [-agent-retries N] [-strategy overlay] [-assert] [-resolver CMD] [-verify CMD | -verify-preset go|node|python|rust [-verify-retries N] [-verify-impact CMD] [-verify-cache] [(-repair CMD | -repair-preset claude|codex|aider) [-repair-max N]]] [-lanes off|warn|strict] [-no-autocommit] [-keep-failed] [-budget D] [-logdir DIR] [-events FILE] [-manifest FILE] [-notes] [-dry-run] [-json]")
		fs.PrintDefaults()
		fmt.Fprintln(fs.Output(), "\nexit codes:")
		fmt.Fprintln(fs.Output(), "  0  landed and verified (or -verify unset)")
		fmt.Fprintln(fs.Output(), "  1  operational error (bad flags, a git/integrate failure, etc.)")
		fmt.Fprintln(fs.Output(), "  2  usage error (bad top-level sig invocation)")
		fmt.Fprintln(fs.Output(), "  3  -verify failed; nothing landed")
		fmt.Fprintln(fs.Output(), "  4  one or more branches flagged as conflicts (the rest landed)")
		fmt.Fprintln(fs.Output(), "  5  no agent succeeded")
	}
	configFlag := fs.String("config", "", "path to a flat KEY=VALUE flags file supplying defaults for the OTHER run flags (key = flag name without its leading dash, "+
		"value = exactly what would follow it on the command line; NOT TOML despite issue #13's working title — see docs/USAGE.md's Config file section). "+
		"Precedence is command-line flag > config file > flag default. Default \"\": look for ./sig.conf in the current working directory (discovery is silent "+
		"if nothing is there); \""+configDisableSentinel+"\": disable discovery entirely; any other value: read that exact file, failing loudly if it can't be read. "+
		"\"-config\" is never itself allowed as a key inside the file")
	repo := fs.String("repo", "", "path to the target git repository")
	base := fs.String("base", "main", "base branch the agents fork from and the result lands onto")
	tasksFile := fs.String("tasks", "", `path to a JSON file: [{"id":"..","prompt":".."}] (mutually exclusive with -goal)`)
	goal := fs.String("goal", "", "natural-language goal; the -planner turns it into parallel disjoint tasks (mutually exclusive with -tasks)")
	plannerCmd := fs.String("planner", "", "planner command (run via `sh -c`), required with -goal: reads SIGBOUND_GOAL/SIGBOUND_REPOMAP/SIGBOUND_N/SIGBOUND_PROMPT and writes a JSON task array [{\"id\",\"prompt\"}] to stdout")
	plannerPreset := fs.String("planner-preset", "", "expand a known planner-harness preset (claude|codex|aider) into -planner's sh -c command; an explicit -planner always overrides its preset. "+
		"See docs/USAGE.md's Presets section for the exact expansion of each name")
	n := fs.Int("n", 4, "number of parallel tasks the -planner should produce from -goal")
	minTasks := fs.Int("min-tasks", 0, "minimum number of tasks a -goal plan must produce; fewer fails before any agent runs (fail-safe; 0 = no floor). Must be <= -n")
	plannerTimeout := fs.Duration("planner-timeout", 120*time.Second, "timeout for the -planner command (0 = none)")
	agentCmd := fs.String("agent", "", "shell command (run via `sh -c`, once per task) that edits files (and optionally commits) in the task's worktree; "+
		"receives SIGBOUND_TASK, SIGBOUND_TASK_ID, SIGBOUND_REPO, SIGBOUND_BRANCH env vars with cwd=the worktree")
	agentPreset := fs.String("agent-preset", "", "expand a known agent-harness preset (claude|codex|aider) into -agent's sh -c command; an explicit -agent always overrides its preset. "+
		"See docs/USAGE.md's Presets section for the exact expansion of each name")
	agentTimeout := fs.Duration("agent-timeout", 0, "timeout for each -agent command (0 = none); on expiry the agent fails (exit=-1) and the report marks timedOut=true")
	agentRetries := fs.Int("agent-retries", 0, "retry a FAILED agent (bad exit or -agent-timeout) up to N more times, each in a fresh worktree off the same base. "+
		"A lane-strict out-of-lane failure is never retried — that's a plan violation, not a timing fluke. 0 = no retries")
	strategy := fs.String("strategy", cell.StrategyOverlay, "integration strategy: "+strings.Join(cell.AvailableStrategies(), ", "))
	assert := fs.Bool("assert", false, "paranoid cross-check for -strategy overlay: independently recompute the combine via merge-tree and error (never land) on any tree mismatch. "+
		"Roughly doubles integration cost (it re-merges everything); for paranoia/CI, not routine use")
	resolverCmd := fs.String("resolver", "", "optional conflict resolver command (see `sig integrate -h`); reads SIGBOUND_BASE/SIGBOUND_OURS/SIGBOUND_THEIRS/SIGBOUND_PATH, writes the resolved body to stdout")
	resolverTimeout := fs.Duration("resolver-timeout", 30*time.Second, "per-conflict timeout for -resolver (0 = none)")
	verifyCmd := fs.String("verify", "", "optional command run (via `sh -c`) in a detached checkout of the integrated tree; non-zero exit => verify failed")
	verifyPreset := fs.String("verify-preset", "", "expand a known per-language build+test preset (go|node|python|rust) into -verify's sh -c command; an explicit -verify always overrides its preset. "+
		"See docs/USAGE.md's Presets section for the exact expansion of each name")
	verifyRetries := fs.Int("verify-retries", 0, "after a FAILING -verify invocation, re-run it up to N more times on the same tree; passes on any green. "+
		"A pass on a retry marks the report flaky=true (the passing run's output is reported). 0 = today's behavior: a single failing invocation fails verify. "+
		"-verify must still be deterministic — retries mitigate flaky suites, not a license to skip that")
	verifyImpactCmd := fs.String("verify-impact", "", "optional command (run via `sh -c`) run INSTEAD of -verify when Sigbound can confidently compute the impacted Go packages from the "+
		"run's landed write-set: one `go list -json ./...` spawn in the verify checkout, then a reverse-import closure over the changed packages. ANY doubt — a non-Go file changed "+
		"(including go.mod/go.sum), a change under a testdata/ directory, a `go list` failure, or an empty impact set — falls back to running the FULL -verify command instead; "+
		"impact scoping trades confidence for speed, and -verify remains the source of truth. Requires -verify (or -verify-preset): it composes WITH -verify rather than replacing "+
		"it. Receives SIGBOUND_IMPACTED_PKGS (space-separated ./relative package paths, transitively expanded to reverse-dependents) and SIGBOUND_CHANGED_FILES (space-separated "+
		"changed paths) in addition to the usual environment. See docs/USAGE.md's Scoped verification section")
	// NOTE: no backtick pairs in this description — flag.PrintDefaults treats
	// the first backtick-quoted substring in a Usage string as the flag's
	// printed type placeholder (see -agent's "sh -c" above, used
	// deliberately); an incidental pair here would replace this bool flag's
	// normal no-placeholder rendering with stray command text.
	verifyCache := fs.Bool("verify-cache", false, "cache a PASSING -verify (or -verify-impact) verdict keyed by the tree OID of the integrated commit, a hash of the exact resolved verify command "+
		"(the impacted-package list too, when -verify-impact scoped it), and the sigbound version; an exact repeat skips re-running the command entirely and the report marks verify.cached=true. "+
		"A FAILING verify is NEVER cached (fail-safe: a flaky environment must never pin a red, and a miss only ever costs a redundant re-run, never risks a wrong green) -- see docs/USAGE.md's Cache "+
		"section. Entries live under .git/sigbound/verify-cache in the TARGET repo, never the working tree; 'rm -rf .git/sigbound' resets it. Off by default: -verify's rawness is the trust anchor, "+
		"caching is opt-in")
	repairCmd := fs.String("repair", "", "optional self-healing fixer command (run via `sh -c`) invoked in a worktree at the integrated head when -verify FAILS; "+
		"receives SIGBOUND_FAILURE (captured verify output) + SIGBOUND_REPO, edits files to fix the failure (the driver auto-commits them), then -verify re-runs. Looped up to -repair-max times")
	repairPreset := fs.String("repair-preset", "", "expand a known repair-harness preset (claude|codex|aider) into -repair's sh -c command; an explicit -repair always overrides its preset. "+
		"See docs/USAGE.md's Presets section for the exact expansion of each name")
	repairMax := fs.Int("repair-max", 2, "max -repair attempts before reporting verify.ok=false honestly (only used with -repair)")
	noAutocommit := fs.Bool("no-autocommit", false, "do NOT commit an agent's uncommitted edits; by default the driver stages+commits edits an agent left uncommitted so edit-only agents still land")
	lanes := fs.String("lanes", laneWarn, "lane enforcement for declared file-sets: off (ignore) | warn (record out-of-lane writes, still land) | strict (out-of-lane => failed agent, not landed). warn is best-effort; strict is the real disjointness guarantee. "+
		"Default is warn, EXCEPT a planned run (-goal) with -lanes not set explicitly defaults to strict, since the planner already promised a disjoint split")
	keepFailed := fs.Bool("keep-failed", false, "keep a FAILED agent's worktree on disk instead of removing it, so it can be inspected; the path is printed and recorded in the report. Successful agents' worktrees are always removed")
	budget := fs.Duration("budget", 0, "wall-clock ceiling for the whole run: the agent phase, integrate, and verify combined (0 = none). On expiry, outstanding agents are cancelled and fail; "+
		"integrate/verify then run against whatever's left of that same deadline, and if they can't complete, the run reports an operational error naming the budget instead of landing anything partial")
	logDir := fs.String("logdir", "", "when set, write each agent/repair/verify/planner command's FULL stdout+stderr to <logdir>/<name>.log "+
		"(agent-<id>.log, repair-<n>.log, verify-<n>.log, planner.log), in addition to the truncated tails the report keeps in memory. "+
		"The directory is created if needed and must be writable; checked before any agent runs")
	events := fs.String("events", "", `when set, stream one JSON object per line to FILE as the run progresses ("-" = stderr), one line per lifecycle event `+
		`(run_start, agent_start/agent_done, integrate_start/integrate_done, verify_start/verify_done, repair_start/repair_done, land, run_done). `+
		`The report printed at the end remains the source of truth; events are progress, not a second report. Default "" = off`)
	dryRun := fs.Bool("dry-run", false, "load/plan the tasks, print them plus the predicted OCC partition (group count, per-group tasks and files), then exit "+
		"without creating any worktree or running any agent, verify, or repair command. With -goal the planner still runs (that's the point: see the plan before spending agent calls). "+
		"-agent (and any other run flag) is still required but is never invoked. Exit code 0 on a valid plan; a bad plan or an unmet -min-tasks floor fails exactly as it would without -dry-run")
	manifest := fs.String("manifest", "", "write the full JSON report (same shape as -json, see docs/USAGE.md's Provenance section) to FILE at the end of the run, independent of -json "+
		"(which prints to stdout instead — set both to get either or both). FILE's directory is created if needed and must be writable, checked before any agent runs, same fail-fast "+
		"policy as -logdir. A write failure AFTER the run completes is different: by then real work may already be landed on -base, so losing the manifest must never look like losing "+
		"the run — that failure is best-effort, warned loudly on stderr, and never changes the exit code. Feed the file straight to sig replay's own -manifest flag")
	resume := fs.Bool("resume", false, "resume a prior run instead of planning/loading tasks fresh: REQUIRES -manifest pointing at that prior run's own -manifest output, and -tasks/-goal must NOT be "+
		"passed (resume never re-plans; the manifest already recorded the task list). Every task whose agent/<id> branch already differs from the manifest's recorded baseSHA is reused as-is — "+
		"its agent never runs again, and the report marks it resumed=true; a task whose branch is missing or unchanged from baseSHA (its agent committed nothing last time) runs fresh, exactly "+
		"as an ordinary run. -base/-strategy/-agent/-resolver/-verify/-repair are read from the manifest too, UNLESS this command line sets that flag (or its preset) explicitly, in which case "+
		"the explicit value wins (flag beats manifest, the same precedence -config gives a command-line flag over a config file). Fails loudly, before anything runs, if -base's CURRENT head "+
		"has moved past the manifest's recorded baseSHA — resuming onto a different base would integrate against the wrong tree; re-run fresh instead. See docs/USAGE.md's Resume section")
	notes := fs.Bool("notes", false, "when the run LANDS (the base ref actually advances), attach the full JSON report as a git note on the landed commit under the NAMESPACED "+
		"ref refs/notes/sigbound (never git's default refs/notes/commits). Best-effort: a failure here is warned on stderr but never fails the run or changes its exit code, since "+
		"landing has already happened by the time this runs. See docs/USAGE.md's Provenance section for how to read the note back and how to push it (notes do not push by default). "+
		"Off by default")
	asJSON := fs.Bool("json", false, "emit the full JSON report (default: a terse human summary)")

	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitOK, nil
		}
		return exitOperationalError, err
	}
	// explicitFlags tracks every flag actually set on the command line (fs.Visit
	// only visits those), used both for -config's precedence below (a
	// command-line flag always wins over a config-file value for the same key)
	// and for the -lanes strict-default logic further down. applyConfigFile
	// also ADDS to this set every key it successfully applies from the config
	// file, so a config-chosen -lanes counts as explicit too — the user chose
	// it deliberately either way, just not via argv.
	explicitFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})
	if err := applyConfigFile(fs, *configFlag, explicitFlags); err != nil {
		return exitOperationalError, err
	}
	// Expand -agent-preset/-repair-preset/-planner-preset/-verify-preset into
	// their known-good commands BEFORE anything below consumes *agentCmd et
	// al. — so e.g. -agent-preset alone satisfies the "-agent is required"
	// check right after this, exactly like a hand-written -agent would. Runs
	// after applyConfigFile: preset flags are ordinary flags, so a preset
	// chosen via sig.conf is expanded here too, no special-casing needed. Raw
	// wins over its preset either way (see applyPresets/presetSlot).
	newAgentCmd, newRepairCmd, newPlannerCmd, newVerifyCmd, presetErr := applyPresets(os.Stderr,
		*agentCmd, *agentPreset, *repairCmd, *repairPreset, *plannerCmd, *plannerPreset, *verifyCmd, *verifyPreset)
	if presetErr != nil {
		return exitOperationalError, presetErr
	}
	*agentCmd, *repairCmd, *plannerCmd, *verifyCmd = newAgentCmd, newRepairCmd, newPlannerCmd, newVerifyCmd
	// -resume never re-plans: its task list, base, strategy, and
	// agent/resolver/verify/repair commands come from a PRIOR run's
	// -manifest file instead of -tasks/-goal, which is why it demands
	// -manifest and forbids them — checked before the manifest is even read,
	// no point loading a file that's about to be rejected as excess input.
	// Anything THIS command line sets explicitly (flag or preset —
	// explicitFlags already tracks both, the same set -config's own
	// precedence reuses) still wins over the manifest's recorded value;
	// loadResumeManifest applies that precedence per field. Run BEFORE the
	// "-agent is required" check below, so a resumed agent command supplied
	// only by the manifest still satisfies it. The moved-base refusal itself
	// lives in driveRun — it needs the CURRENT base head, which driveRun
	// resolves via RevParse, not something worth duplicating here.
	var resumeTasks []taskSpec
	var resumeBaseSHA string
	if *resume {
		if strings.TrimSpace(*tasksFile) != "" || strings.TrimSpace(*goal) != "" {
			return exitOperationalError, errors.New("-resume does not re-plan: -tasks/-goal must not be passed alongside it — the manifest already recorded the task list")
		}
		if strings.TrimSpace(*manifest) == "" {
			return exitOperationalError, errors.New("-resume requires -manifest pointing at the prior run's manifest")
		}
		var rerr error
		resumeTasks, resumeBaseSHA, rerr = loadResumeManifest(*manifest, explicitFlags, base, strategy, agentCmd, resolverCmd, verifyCmd, repairCmd)
		if rerr != nil {
			return exitOperationalError, rerr
		}
	}
	// lanesExplicit: a planned run (-goal) defaults -lanes to strict UNLESS the
	// caller set -lanes explicitly (command line OR config file — see
	// explicitFlags above), in which case that choice always wins.
	lanesExplicit := explicitFlags["lanes"]
	if *repo == "" {
		return exitOperationalError, errors.New("-repo is required")
	}
	if strings.TrimSpace(*agentCmd) == "" {
		return exitOperationalError, errors.New("-agent is required")
	}
	if strings.TrimSpace(*verifyImpactCmd) != "" && strings.TrimSpace(*verifyCmd) == "" {
		return exitOperationalError, errors.New("-verify-impact requires -verify (or -verify-preset): it composes WITH -verify, which stays required as the fallback")
	}
	if err := validateStrategy(*strategy); err != nil {
		return exitOperationalError, err
	}
	if err := validateLaneMode(*lanes); err != nil {
		return exitOperationalError, err
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

	// -manifest is provenance, not a log: an unwritable path must fail the
	// same way -logdir's does — before any agent runs — rather than silently
	// losing the run's manifest after the work is already done. See
	// prepareManifestPath and the best-effort write at the end of this
	// function (writeManifest) for the other half of that split posture.
	manifestPath := strings.TrimSpace(*manifest)
	if manifestPath != "" {
		if err := prepareManifestPath(manifestPath); err != nil {
			return exitOperationalError, err
		}
	}

	// Task source: exactly one of -tasks (explicit), -goal (planned), or
	// -resume (replayed from a manifest — already validated exclusive of
	// both, above). If both -tasks and -goal are set it is an error rather
	// than silently ignoring one.
	haveTasks := strings.TrimSpace(*tasksFile) != ""
	haveGoal := strings.TrimSpace(*goal) != ""
	switch {
	case haveTasks && haveGoal:
		return exitOperationalError, errors.New("-tasks and -goal are mutually exclusive; pass exactly one")
	case !*resume && !haveTasks && !haveGoal:
		return exitOperationalError, errors.New("one of -tasks or -goal is required")
	}

	var tasks []taskSpec
	var err error
	switch {
	case *resume:
		tasks = resumeTasks
	case haveTasks:
		tasks, err = loadTasks(*tasksFile)
		if err != nil {
			return exitOperationalError, err
		}
		if len(tasks) == 0 {
			return exitOperationalError, errors.New("no tasks in -tasks file")
		}
	default:
		// -min-tasks/-n are -goal-only concepts (a -tasks run has neither a
		// planner-requested count nor a floor to check), so this validation lives
		// here instead of the flag checks above. Caught before the planner even
		// runs, same fail-safe-before-any-agent posture as everything else in
		// this branch.
		if *minTasks > *n {
			return exitOperationalError, fmt.Errorf("-min-tasks %d exceeds -n %d", *minTasks, *n)
		}
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

	// -dry-run: the plan is already loaded/validated above (planTasks already
	// ran for -goal — that's the point, seeing the plan costs nothing further
	// once you've paid for it). Preview the predicted partition and stop here:
	// no worktree, no agent, no verify, no git mutation beyond what planning
	// itself already did.
	if *dryRun {
		if err := emitDryRun(w, buildDryRunReport(tasks, laneMode), *asJSON); err != nil {
			return exitOperationalError, err
		}
		return exitOK, nil
	}

	p := runParams{
		Repo:            *repo,
		Base:            *base,
		Strategy:        *strategy,
		Assert:          *assert,
		AgentCmd:        *agentCmd,
		PlannerCmd:      *plannerCmd,
		AgentTimeout:    *agentTimeout,
		AgentRetries:    *agentRetries,
		ResolverCmd:     *resolverCmd,
		ResolverTimeout: *resolverTimeout,
		VerifyCmd:       *verifyCmd,
		VerifyImpactCmd: *verifyImpactCmd,
		VerifyRetries:   *verifyRetries,
		VerifyCache:     *verifyCache,
		RepairCmd:       *repairCmd,
		RepairMax:       *repairMax,
		Autocommit:      !*noAutocommit,
		LaneMode:        laneMode,
		KeepFailed:      *keepFailed,
		Budget:          *budget,
		LogDir:          logDirAbs,
		EventsPath:      *events,
		Notes:           *notes,
		Resume:          *resume,
		ResumeBaseSHA:   resumeBaseSHA,
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
			if manifestPath != "" {
				writeManifest(manifestPath, rep)
			}
		}
		return exitOperationalError, err
	}
	code := runExitCode(rep)
	if err := emitReport(w, rep, *asJSON); err != nil {
		return exitOperationalError, err
	}
	if manifestPath != "" {
		writeManifest(manifestPath, rep)
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

// dryRunGroupJSON is one predicted OCC group in -dry-run's machine report:
// the task ids whose declared files put them in the same group (union-find
// over shared paths, transitively — see cell.Partition), and the union of
// every file the group touches.
type dryRunGroupJSON struct {
	Tasks []string `json:"tasks"`
	Files []string `json:"files"`
}

// dryRunReport is -dry-run's stdout contract: the loaded/planned tasks plus
// the predicted partition, computed via the SAME cell.Partition code
// integration uses — no agent runs to produce this.
type dryRunReport struct {
	Tasks       []taskSpec        `json:"tasks"`
	Groups      []dryRunGroupJSON `json:"groups"`
	Parallelism int               `json:"parallelism"` // number of groups: independent branches that could land in parallel
	LaneMode    string            `json:"laneMode"`
}

// buildDryRunReport predicts how tasks would partition without running any
// agent: each task's DECLARED Files stand in for the write-set cell.Partition
// normally computes from a real `git diff`, reusing the exact same grouping
// code the live integrator uses (see cell.Partition, integrateBranches). A
// task with no declared Files contributes an empty write-set, which never
// overlaps anything, so it lands alone in its own group — "unknown" in the
// human summary (writeDryRunSummary), an empty Files list in JSON.
func buildDryRunReport(tasks []taskSpec, laneMode string) dryRunReport {
	changes := make([]cell.BranchChange, len(tasks))
	for i, t := range tasks {
		changes[i] = cell.BranchChange{Branch: t.ID, WriteSet: cell.NewWriteSet(t.Files...)}
	}
	groups := cell.Partition(changes)
	out := make([]dryRunGroupJSON, len(groups))
	for i, g := range groups {
		ids := make([]string, len(g))
		files := cell.NewWriteSet()
		for j, bc := range g {
			ids[j] = bc.Branch
			for _, p := range bc.WriteSet.Paths() {
				files.Add(p)
			}
		}
		out[i] = dryRunGroupJSON{Tasks: ids, Files: files.Paths()}
	}
	return dryRunReport{Tasks: tasks, Groups: out, Parallelism: len(out), LaneMode: laneMode}
}

// emitDryRun writes rep to w as the full JSON report (asJSON) or the terse
// human summary — -dry-run's counterpart to emitReport.
func emitDryRun(w io.Writer, rep dryRunReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return writeDryRunSummary(w, rep)
}

// writeDryRunSummary prints -dry-run's human-readable preview: every loaded/
// planned task, the predicted OCC groups, and the resulting parallelism
// (independent groups land in parallel; tasks sharing a group must be
// serialized by the integrator).
func writeDryRunSummary(w io.Writer, rep dryRunReport) error {
	fmt.Fprintf(w, "%d task(s), lanes=%s\n", len(rep.Tasks), rep.LaneMode)
	for _, t := range rep.Tasks {
		fmt.Fprintf(w, "  task %-12s files=%v\n", t.ID, t.Files)
	}
	fmt.Fprintf(w, "predicted partition: %d group(s) (parallelism=%d)\n", len(rep.Groups), rep.Parallelism)
	for i, g := range rep.Groups {
		files := "unknown (no declared files)"
		if len(g.Files) > 0 {
			files = fmt.Sprintf("%v", g.Files)
		}
		fmt.Fprintf(w, "  group %d: tasks=%v files=%s\n", i+1, g.Tasks, files)
	}
	return nil
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

// loadResumeManifest implements -resume's manifest side: it reads path (the
// prior run's own -manifest output — the same flag doubles as -resume's
// input, so a chain of resumes just keeps overwriting/reading one file) and
// returns the task list to run — resume never re-plans, so tasks always come
// from here — plus the baseSHA that run integrated against, which driveRun's
// moved-base check compares against -base's CURRENT head.
//
// Every other manifest-recorded value (base/strategy/agent/resolver/verify/
// repair) is written into its flag pointer ONLY when that flag (or its
// -*-preset, for the three that have one) was NOT itself set explicitly on
// THIS command line — explicit is the exact same fs.Visit/applyConfigFile
// set runRun already threads through -config's precedence, so a config-file
// choice counts as explicit here too. This is what gives -resume its
// documented precedence: command-line flag > manifest.
func loadResumeManifest(path string, explicit map[string]bool, base, strategy, agentCmd, resolverCmd, verifyCmd, repairCmd *string) ([]taskSpec, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("-resume: read -manifest %s: %w", path, err)
	}
	var prior runReport
	if err := json.Unmarshal(data, &prior); err != nil {
		return nil, "", fmt.Errorf("-resume: parse -manifest %s: %w", path, err)
	}
	if len(prior.Tasks) == 0 {
		return nil, "", fmt.Errorf("-resume: -manifest %s has no recorded tasks", path)
	}
	if strings.TrimSpace(prior.BaseSHA) == "" {
		return nil, "", fmt.Errorf("-resume: -manifest %s is missing baseSHA — not a sig run manifest", path)
	}
	if !explicit["base"] && prior.Base != "" {
		*base = prior.Base
	}
	if !explicit["strategy"] && prior.Strategy != "" {
		*strategy = prior.Strategy
	}
	if !explicit["agent"] && !explicit["agent-preset"] && prior.AgentCmd != "" {
		*agentCmd = prior.AgentCmd
	}
	if !explicit["resolver"] && prior.ResolverCmd != "" {
		*resolverCmd = prior.ResolverCmd
	}
	if !explicit["verify"] && !explicit["verify-preset"] && prior.VerifyCmd != "" {
		*verifyCmd = prior.VerifyCmd
	}
	if !explicit["repair"] && !explicit["repair-preset"] && prior.RepairCmd != "" {
		*repairCmd = prior.RepairCmd
	}
	return prior.Tasks, prior.BaseSHA, nil
}

// eventEmitter streams one JSON object per line to its underlying writer as
// driveRun progresses (see -events), guarded by mu since agents run
// concurrently in goroutines (driveRun's fan-out below) and must not
// interleave partial JSON lines. A nil *eventEmitter is a valid no-op
// receiver (see emit), so every call site can emit unconditionally instead
// of guarding on -events being set.
type eventEmitter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// emit writes one NDJSON line: {"event":name,"ts":<RFC3339Nano>,...fields}.
// Best-effort, same policy as -logdir's bestEffortWriter: a write failure
// here must never fail the run, so the encode error is deliberately dropped.
func (e *eventEmitter) emit(name string, fields map[string]any) {
	if e == nil {
		return
	}
	rec := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		rec[k] = v
	}
	rec["event"] = name
	rec["ts"] = time.Now().Format(time.RFC3339Nano)
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.enc.Encode(rec) //nolint:errcheck // deliberately best-effort; see doc comment
}

// newEventEmitter opens path for -events and returns the emitter plus a
// closer to defer. "" disables events entirely (nil emitter — every emit
// call becomes a no-op). "-" streams to stderr (left open; never closed by
// the returned closer). Any other value is a file path, freshly truncated
// (an events stream is this run's own trace, not a log to accumulate across
// runs like -logdir). An open failure is returned as an error so the run
// fails early, before any agent runs — same policy as -logdir.
func newEventEmitter(path string) (*eventEmitter, func(), error) {
	noop := func() {}
	switch path {
	case "":
		return nil, noop, nil
	case "-":
		return &eventEmitter{enc: json.NewEncoder(os.Stderr)}, noop, nil
	default:
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, noop, fmt.Errorf("-events %s: %w", path, err)
		}
		return &eventEmitter{enc: json.NewEncoder(f)}, func() { f.Close() }, nil
	}
}

// driveRun executes the full orchestration: fan out agents, integrate, verify.
// It is separated from flag parsing so tests can drive it in-process.
func driveRun(ctx context.Context, p runParams, tasks []taskSpec) (rep runReport, err error) {
	runStart := time.Now()
	emit, closeEvents, err := newEventEmitter(p.EventsPath)
	if err != nil {
		return runReport{}, err
	}
	defer closeEvents()
	// run_done is always the LAST event (see the -events causal-order
	// guarantee): this defer reads the final rep/err via the named returns
	// above, so it fires no matter which of driveRun's several return points
	// was taken. Registered before closeEvents' own defer, so it runs first
	// (defers are LIFO) — the event is written before the file closes.
	defer func() {
		code := exitOperationalError
		if err == nil {
			code = runExitCode(rep)
		}
		emit.emit("run_done", map[string]any{"ok": code == exitOK, "exitCode": code, "wallMs": time.Since(runStart).Milliseconds()})
	}()
	g := gitx.New(p.Repo)

	// Pin the base to a stable commit so every agent forks the same tree and the
	// merge-base stays fixed even after we advance the branch ref.
	baseSHA, err := g.RevParse(ctx, p.Base)
	if err != nil {
		return runReport{}, fmt.Errorf("resolve base %q in %s: %w", p.Base, p.Repo, err)
	}
	// -resume's moved-base refusal: the prior run's manifest recorded
	// ResumeBaseSHA as the tree every surviving agent/<id> branch forked
	// from. If -base's CURRENT head is no longer that exact commit, someone
	// (another run, a plain commit) has moved it since — resuming onto it
	// now would integrate reused branches against a base they never actually
	// forked from. Fail loudly here, before wtRoot or any worktree exists, so
	// nothing runs at all; re-running fresh (without -resume) is the correct
	// recovery, not a silent resume onto the wrong tree.
	if p.Resume && baseSHA != p.ResumeBaseSHA {
		return runReport{}, fmt.Errorf("-resume: base %q is now at %s but the manifest recorded %s — it has moved since that run; re-run fresh instead of resuming onto a different base",
			p.Base, short(baseSHA), short(p.ResumeBaseSHA))
	}

	wtRoot, err := os.MkdirTemp("", "sig-run-*")
	if err != nil {
		return runReport{}, err
	}

	if p.LaneMode == "" {
		p.LaneMode = laneWarn // default when driven in-process without flag parsing
	}
	rep = runReport{
		Repo: p.Repo, Base: p.Base, BaseSHA: baseSHA, LaneMode: p.LaneMode, Tasks: tasks, LogDir: p.LogDir,
		Strategy:    p.Strategy,
		AgentCmd:    p.AgentCmd,
		ResolverCmd: p.ResolverCmd,
		VerifyCmd:   p.VerifyCmd,
		RepairCmd:   p.RepairCmd,
		PlannerCmd:  p.PlannerCmd,
		Version:     Version,
		StartedAt:   runStart.UTC().Format(time.RFC3339),
	}
	emit.emit("run_start", map[string]any{"repo": p.Repo, "base": p.Base, "baseSHA": baseSHA, "tasks": tasks})
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

	// -budget bounds the rest of driveRun (agent phase + integrate + verify)
	// with one wall-clock ceiling. Once it fires, every outstanding agent is
	// cancelled (see runAgent) and integrate/verify below run against whatever
	// is left of the same expired ctx — honestly, with no separate grace
	// period: if a git command can no longer complete, it fails and that
	// surfaces as an operational error naming the budget (see the integrate
	// error handling below), never a silently-landed partial tree.
	if p.Budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Budget)
		defer cancel()
	}

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
			branch := "agent/" + tasks[i].ID
			// -resume: a branch that already holds real work (its head
			// differs from baseSHA) is reused outright, no agent invocation
			// at all. resumeAgent also clears a STALE no-op branch (head ==
			// baseSHA) before falling through, so the fresh run below can
			// create it the ordinary way. See resumeAgent's doc comment.
			if p.Resume {
				if a, reused := resumeAgent(ctx, g, &admin, branch, baseSHA, p, tasks[i]); reused {
					agents[i] = a
					emit.emit("agent_done", map[string]any{"id": a.ID, "ok": a.OK, "resumed": true, "exit": a.Exit, "attempts": a.Attempts, "files": a.Files, "inLane": a.InLane, "wallMs": int64(0)})
					return
				}
			}
			emit.emit("agent_start", map[string]any{"id": tasks[i].ID, "branch": branch})
			agentStart := time.Now()
			agents[i] = runAgentWithRetries(ctx, g, &admin, wtRoot, baseSHA, p, tasks[i])
			a := agents[i]
			emit.emit("agent_done", map[string]any{"id": a.ID, "ok": a.OK, "exit": a.Exit, "attempts": a.Attempts, "files": a.Files, "inLane": a.InLane, "wallMs": time.Since(agentStart).Milliseconds()})
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
	emit.emit("integrate_start", map[string]any{"branches": branches})
	start := time.Now()
	res, err := integrateBranches(ctx, g, p.Base, baseSHA, branches, writeSets, p.Strategy, p.ResolverCmd, p.ResolverTimeout, p.Assert, false)
	if err != nil {
		return rep, budgetAwareErr(p, ctx, "integrate", err)
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
	emit.emit("integrate_done", map[string]any{"landed": ir.Landed, "flagged": ir.Flagged, "resolved": ir.Resolved, "finalSHA": ir.FinalSHA, "wallMs": ir.WallMs})

	// changedFiles is the run's LANDED write-set — every path any landed branch
	// touched — reusing the write-sets already computed for OCC partitioning
	// (writeSets above) rather than re-diffing. It's -verify-impact's input for
	// test-impact analysis (see computeImpact); built from ir.Landed (not every
	// branch that ran) since only landed changes actually reach the verified
	// tree. cell.WriteSet dedups+sorts for free.
	changedSet := cell.NewWriteSet()
	for _, b := range ir.Landed {
		for _, f := range writeSets[b] {
			changedSet.Add(f)
		}
	}
	changedFiles := changedSet.Paths()

	// ---- verify the integrated tree (self-healing via -repair), then land ----
	// Nothing is on the base ref yet. Land only if verify passes (or is unset);
	// on an honest failure leave the base ref at baseSHA, so a red run lands
	// nothing — matching the documented guarantee. verifyWithRepair may advance
	// the head via a repair; FinalSHA reports whatever we computed either way.
	landSHA := res.FinalSHA
	if strings.TrimSpace(p.VerifyCmd) != "" {
		rep.Verify, landSHA = verifyWithRepair(ctx, g, p, res.FinalSHA, changedFiles, emit)
		rep.Integrate.FinalSHA = landSHA
		if !rep.Verify.OK {
			// A -budget expiry can kill -verify mid-run (its command context is a
			// child of ctx); left alone that reads as an ordinary verify failure,
			// which is misleading — the tree may never have gotten a real run. Name
			// the real cause up front when it applies.
			if p.Budget > 0 && ctx.Err() != nil {
				rep.Verify.Output = fmt.Sprintf("run budget (%s) exhausted before verify could complete\n%s", p.Budget, rep.Verify.Output)
			}
			return rep, nil // verify failed, no repair fixed it: land nothing
		}
	}
	if err := g.UpdateRef(ctx, "refs/heads/"+p.Base, landSHA); err != nil {
		return rep, budgetAwareErr(p, ctx, "land "+short(landSHA), err)
	}
	rep.Integrate.FinalSHA = landSHA
	emit.emit("land", map[string]any{"sha": landSHA})
	// -notes: the landing already happened above, so a note failure below must
	// never look like the run itself failed — see attachNote's doc.
	if p.Notes {
		attachNote(ctx, g, landSHA, rep)
	}
	return rep, nil
}

// attachNote records rep as a git note on the landed commit, namespaced under
// refs/notes/sigbound (see gitx.NoteAdd — never git's default
// refs/notes/commits, so sigbound's provenance record never collides with a
// repo's own note usage). -notes only ever fires AFTER landing has already
// happened (see driveRun's call site): a failure here must never look like
// the run itself failed, so it's best-effort with a loud stderr warning, the
// same posture as a -manifest write failure (writeManifest).
func attachNote(ctx context.Context, g *gitx.Git, commit string, rep runReport) {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: -notes: encode report: %v\n", err)
		return
	}
	if err := g.NoteAdd(ctx, "sigbound", commit, data); err != nil {
		fmt.Fprintf(os.Stderr, "warning: -notes: attach note to %s: %v\n", short(commit), err)
	}
}

// budgetAwareErr wraps err from a driveRun phase (integrate, land), naming
// the exhausted -budget when that's honestly why op couldn't complete — ctx
// already dead by the time op ran, not some unrelated git failure. Plain
// "%s: %w" wrapping otherwise (no -budget set, or ctx is still fine and
// something else broke).
func budgetAwareErr(p runParams, ctx context.Context, op string, err error) error {
	if p.Budget > 0 && ctx.Err() != nil {
		return fmt.Errorf("%s: run budget (%s) exhausted before it could complete: %w", op, p.Budget, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// verifyWithRepair runs -verify (with -verify-retries applied, see
// runVerifyRetry) on head and, when it fails and -repair is set, drives the
// self-healing repair loop: up to p.RepairMax times it runs the fixer agent
// in a throwaway worktree (SIGBOUND_FAILURE = the last verify output,
// SIGBOUND_REPO = repo), auto-commits whatever the fixer edited, advances
// head to that commit, and re-verifies (again with retries) — stopping as
// soon as verify passes. It returns the verify verdict (green either
// first-try or after repair; honestly false if never fixed) and the
// possibly-advanced head the base ref should point at. If -verify passes on
// the first try, no repair runs. emit gets a verify_start/verify_done pair
// around every -verify invocation this call makes (attempt 0 = pre-repair, N
// = after repair round N, matching -logdir's verify-<n>.log numbering) and a
// repair_start/repair_done pair around every fixer invocation.
//
// ONE detached worktree is materialized up front and reused for every verify
// invocation this call makes — the initial attempt, its -verify-retries
// retries, and every repair round's re-verify — instead of a fresh
// WorktreeAddDetached + WorktreeRemove per attempt (up to ~5 full O(repo)
// checkouts with -repair-max=2, even when the delta each round touches is a
// handful of files). Retries on an unchanged head just re-run in place
// (runVerify resets+cleans first, see its doc); when a repair round advances
// head, this loop resets+cleans the worktree BEFORE advancing it, then
// advances it in place via CheckoutDetach — which, unlike a fresh checkout,
// only touches the paths that actually differ, so it must start from a
// pristine tree or a stale tracked-file edit / untracked leftover from the
// PRIOR head can ride straight through (see the reset+clean block right
// before each CheckoutDetach call below). It's torn down once, when this
// function returns. runRepair still gets its OWN fresh worktree per round: it
// needs a genuinely clean tree for its `git add -A` auto-commit, never one
// that might carry a verify command's leftover build artifacts (see
// runVerify's reset+clean-on-entry, which is what makes reusing the verify
// worktree safe instead of a hermeticity bug).
//
// changedFiles is the run's landed write-set — -verify-impact's input for
// computeImpact (see runVerify). Each repair round folds that round's
// FilesTouched into changedFiles and RECOMPUTES impact for the re-verify
// (same fallback rules as the initial decision, e.g. a fixer that touches a
// non-Go file falls back to full just like any other doubt) — a repair
// changes the tree, so re-deciding scope from scratch, not memoizing the
// first decision, is what keeps the fallback honest.
func verifyWithRepair(ctx context.Context, g *gitx.Git, p runParams, head string, changedFiles []string, emit *eventEmitter) (verifyJSON, string) {
	dir, err := os.MkdirTemp("", "sig-verify-*")
	if err != nil {
		return verifyJSON{Ran: true, OK: false, Output: err.Error()}, head
	}
	defer os.RemoveAll(dir)
	wtPath := filepath.Join(dir, "wt")
	if err := g.WorktreeAddDetached(ctx, wtPath, head); err != nil {
		return verifyJSON{Ran: true, OK: false, Output: "checkout " + head + ": " + err.Error()}, head
	}
	defer func() { _ = g.WorktreeRemove(ctx, wtPath) }()

	emit.emit("verify_start", map[string]any{"attempt": 0})
	vStart := time.Now()
	v := runVerifyRetry(ctx, g, wtPath, p, changedFiles, p.VerifyRetries, p.LogDir, 0)
	emit.emit("verify_done", map[string]any{"ok": v.OK, "flaky": v.Flaky, "cached": v.Cached, "attempt": 0, "wallMs": time.Since(vStart).Milliseconds()})
	// Green first try (possibly after a retry, or served from -verify-cache),
	// or no repair configured/allowed => done, loop never runs. v already
	// carries Flaky/Cached, so both survive here.
	if v.OK || strings.TrimSpace(p.RepairCmd) == "" || p.RepairMax < 1 {
		return v, head
	}

	var repairs []repairAttemptJSON
	lastOutput := v.Output
	lastScope := v.Scope
	lastImpacted := v.ImpactedPkgs
	ok := false
	flaky := false
	cached := false
	for attempt := 1; attempt <= p.RepairMax; attempt++ {
		emit.emit("repair_start", map[string]any{"attempt": attempt})
		rStart := time.Now()
		newHead, touched, rerr := runRepair(ctx, g, p, head, lastOutput, attempt)
		repairWallMs := time.Since(rStart).Milliseconds()
		rec := repairAttemptJSON{N: attempt, FilesTouched: touched, VerifyOK: false}
		if rerr != nil || newHead == head {
			// Fixer failed to spawn/commit, or produced no change: record the
			// dead attempt and stop — re-verifying an unchanged tree is pointless.
			if rerr != nil {
				lastOutput = tail(lastOutput+"\n[repair attempt "+fmt.Sprintf("%d", attempt)+" error] "+rerr.Error(), 2000)
			}
			repairs = append(repairs, rec)
			emit.emit("repair_done", map[string]any{"attempt": attempt, "verifyOk": rec.VerifyOK, "wallMs": repairWallMs})
			break
		}
		head = newHead
		// Reset + clean the reused worktree BEFORE advancing it onto the
		// repaired head. The verify invocation that just ran (above, or this
		// loop's previous round) may have left tracked-file edits or
		// untracked leftovers in place — CheckoutDetach only touches paths
		// that differ between the OLD and NEW commit, so any tracked-file
		// mutation on a path repair never touched would otherwise ride
		// straight through unreverted, and an untracked leftover could
		// collide with a path the new head is about to add and abort the
		// checkout outright. Order matters: this must run BEFORE
		// CheckoutDetach, not after (that's what runVerify's own
		// reset+clean-on-entry guards against retries on an UNCHANGED head;
		// it can't undo damage from before a checkout onto a NEW head).
		if cerr := g.At(wtPath).ResetHard(ctx); cerr != nil {
			lastOutput = tail(lastOutput+"\n[repair attempt "+fmt.Sprintf("%d", attempt)+" reset error] "+cerr.Error(), 2000)
			repairs = append(repairs, rec)
			emit.emit("repair_done", map[string]any{"attempt": attempt, "verifyOk": rec.VerifyOK, "wallMs": repairWallMs})
			break
		}
		if cerr := g.At(wtPath).Clean(ctx); cerr != nil {
			lastOutput = tail(lastOutput+"\n[repair attempt "+fmt.Sprintf("%d", attempt)+" clean error] "+cerr.Error(), 2000)
			repairs = append(repairs, rec)
			emit.emit("repair_done", map[string]any{"attempt": attempt, "verifyOk": rec.VerifyOK, "wallMs": repairWallMs})
			break
		}
		// Advance the persistent verify worktree onto the repaired head in
		// place — touches only the paths that differ — instead of tearing it
		// down and re-materializing the whole tree with a fresh
		// WorktreeAddDetached.
		if cerr := g.At(wtPath).CheckoutDetach(ctx, head); cerr != nil {
			lastOutput = tail(lastOutput+"\n[repair attempt "+fmt.Sprintf("%d", attempt)+" checkout error] "+cerr.Error(), 2000)
			repairs = append(repairs, rec)
			emit.emit("repair_done", map[string]any{"attempt": attempt, "verifyOk": rec.VerifyOK, "wallMs": repairWallMs})
			break
		}
		changedSet := cell.NewWriteSet(changedFiles...)
		for _, f := range touched {
			changedSet.Add(f)
		}
		changedFiles = changedSet.Paths()
		emit.emit("verify_start", map[string]any{"attempt": attempt})
		vStart = time.Now()
		rv := runVerifyRetry(ctx, g, wtPath, p, changedFiles, p.VerifyRetries, p.LogDir, attempt)
		emit.emit("verify_done", map[string]any{"ok": rv.OK, "flaky": rv.Flaky, "cached": rv.Cached, "attempt": attempt, "wallMs": time.Since(vStart).Milliseconds()})
		lastOutput = rv.Output
		lastScope = rv.Scope
		lastImpacted = rv.ImpactedPkgs
		rec.VerifyOK = rv.OK
		repairs = append(repairs, rec)
		emit.emit("repair_done", map[string]any{"attempt": attempt, "verifyOk": rec.VerifyOK, "wallMs": repairWallMs})
		if rv.OK {
			ok = true
			flaky = rv.Flaky
			cached = rv.Cached
			break
		}
	}

	// Reached only after an initial verify FAILURE, so ok==true means a repair
	// fixed it (Repaired=true). ok==false => honest failure with the last output.
	return verifyJSON{
		Ran:          true,
		OK:           ok,
		Attempts:     len(repairs),
		Repaired:     ok,
		Flaky:        flaky,
		Scope:        lastScope,
		ImpactedPkgs: lastImpacted,
		Cached:       cached,
		Output:       lastOutput,
		Repairs:      repairs,
	}, head
}

// runVerifyRetry runs -verify (or -verify-impact) in wtPath via runVerify and,
// on FAILURE, re-runs it up to retries more times on the SAME tree (wtPath's
// checked-out commit never changes within this call — only re-verified) —
// passing on any green invocation. A pass on an invocation after the first is
// flaky: -verify is supposed to be deterministic, so retries are a mitigation
// for flaky test suites, not a license to skip that; the flaky pass is
// surfaced via verifyJSON.Flaky rather than silently reported as a clean
// first-try green. retries=0 reproduces the pre-existing behavior exactly:
// one invocation, no retry, never flaky. When all retries are exhausted, the
// last (failing) invocation's result is returned, same as today. changedFiles
// is passed straight through to runVerify unchanged across retries (same
// tree, same scope decision every invocation). logAttempt names this logical
// verify attempt (0 = pre-repair, N = after repair round N) for -logdir's
// verify-<n>.log; every retry within this call appends to the same file (see
// openLog).
func runVerifyRetry(ctx context.Context, g *gitx.Git, wtPath string, p runParams, changedFiles []string, retries int, logDir string, logAttempt int) verifyJSON {
	v := runVerify(ctx, g, wtPath, p, changedFiles, logDir, logAttempt)
	for attempt := 0; !v.OK && attempt < retries; attempt++ {
		v = runVerify(ctx, g, wtPath, p, changedFiles, logDir, logAttempt)
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

// runAgentWithRetries wraps runAgent with -agent-retries: on a FAILED attempt
// (bad exit or an -agent-timeout expiry) it tears down that attempt's
// worktree and retries in a FRESH one off the same base, up to
// p.AgentRetries more times. Three cases stop the retry loop early, before
// attempts are exhausted:
//
//   - a lane-strict out-of-lane failure (a.InLane false) — that's a plan
//     violation, not a timing fluke a retry could fix;
//   - ctx already done (e.g. -budget exhausted) — every further attempt would
//     just fail the same way, so retrying only burns worktree churn;
//   - the worktree/branch itself couldn't be created (wtCreated false) — a
//     pre-existing agent/<id> branch collision can never succeed by retrying
//     (WorktreeAdd loud-fails every time), and any other add failure is
//     environmental; treated like a stray rather than burning retries on it.
//
// created tracks, across this call's own attempts, whether THIS RUN has
// already created agent/<id>'s worktree once before — only then is it safe to
// hand runAgent's next attempt WorktreeAddReset instead of the loud-failing
// WorktreeAdd (see runAgent's doc comment). It starts false and latches true
// the first time runAgent reports wtCreated, and never resets, so a run that
// fails attempt 1 at WorktreeAdd (e.g. a foreign agent/<id> left by a prior
// run) can never reach a reset on some later attempt — there is no later
// attempt, since that failure is terminal (see above).
//
// Only the attempt that actually ENDS the loop keeps its worktree under
// -keep-failed — whether that's because retries are exhausted, a lane-strict
// stray stopped early, or a -budget cancellation stopped early. Which of
// those applies is only known AFTER an attempt runs, so every attempt is
// allowed to keep its worktree on failure; when the loop decides to retry
// instead, that discarded attempt's worktree is torn down immediately below.
// a.Attempts records the total number of tries actually made (1 when the
// first try succeeded, failed terminally, or no retries were configured,
// matching today's report shape).
func runAgentWithRetries(ctx context.Context, g *gitx.Git, admin *sync.Mutex, wtRoot, baseSHA string, p runParams, t taskSpec) perAgentJSON {
	var a perAgentJSON
	created := false
	for attempt := 1; ; attempt++ {
		var wtCreated bool
		a, wtCreated = runAgent(ctx, g, admin, wtRoot, baseSHA, p, t, created)
		a.Attempts = attempt
		if wtCreated {
			created = true
		}
		last := attempt > p.AgentRetries
		if a.OK || last || !a.InLane || ctx.Err() != nil || !wtCreated {
			return a
		}
		// Retrying: this attempt didn't end the loop, so its kept worktree (if
		// any) isn't the final answer — remove it instead of leaking it.
		if a.WorktreeKept != "" {
			admin.Lock()
			_ = g.WorktreeRemove(context.WithoutCancel(ctx), a.WorktreeKept)
			admin.Unlock()
		}
	}
}

// runAgent creates a worktree on branch agent/<id> off base, runs the agent
// command there, and reports what it committed. The worktree is torn down
// after, UNLESS the agent ultimately FAILED (see the lane-enforcement block
// below, which can flip a.OK to false) and p.KeepFailed is set, in which case
// teardown is skipped and the path is recorded in a.WorktreeKept so it can be
// inspected. The branch (with the agent's commit, if any) survives for
// integration either way.
//
// created is true iff a PRIOR attempt in THIS RUN (from runAgentWithRetries'
// own retry loop) already succeeded at creating agent/<id>'s worktree —
// i.e. this call is re-creating a branch this run made itself on a previous,
// now-torn-down attempt. Only then is it safe to use WorktreeAddReset.
// created false (attempt 1, or any run that has never yet created this
// branch) always uses WorktreeAdd, which loud-fails if agent/<id> somehow
// already exists — e.g. a leftover branch from a prior run — rather than
// silently resetting someone else's committed work.
//
// The second return value, wtCreated, reports whether WorktreeAdd/
// WorktreeAddReset succeeded THIS call; runAgentWithRetries latches it into
// its own created for the next attempt, and also treats wtCreated==false as
// terminal (never retried) since a failed add can't be fixed by retrying.
func runAgent(ctx context.Context, g *gitx.Git, admin *sync.Mutex, wtRoot, baseSHA string, p runParams, t taskSpec, created bool) (a perAgentJSON, wtCreated bool) {
	branch := "agent/" + t.ID
	// InLane defaults true (not the zero value) so a failure that never reaches
	// the lane-enforcement block below — e.g. WorktreeAdd itself failing —
	// isn't mistaken by runAgentWithRetries for a lane-strict stray.
	a = perAgentJSON{ID: t.ID, Branch: branch, Files: []string{}, InLane: true}
	dir := filepath.Join(wtRoot, "wt-"+sanitizeID(t.ID))

	admin.Lock()
	var err error
	if created {
		err = g.WorktreeAddReset(ctx, dir, branch, baseSHA)
	} else {
		err = g.WorktreeAdd(ctx, dir, branch, baseSHA)
	}
	admin.Unlock()
	if err != nil {
		a.Exit = -1
		a.Stderr = "worktree add: " + err.Error()
		return a, false
	}
	// a is a named return: this defer runs after every downstream update to a
	// (including lane enforcement), so it sees the FINAL OK verdict.
	defer func() {
		if p.KeepFailed && !a.OK {
			a.WorktreeKept = dir
			return
		}
		admin.Lock()
		// WithoutCancel: teardown must happen even when ctx is already dead (a
		// -budget or -agent-timeout expiry) — an agent's admin cleanup is not
		// itself subject to either, only the agent command is (see actx below).
		_ = g.WorktreeRemove(context.WithoutCancel(ctx), dir)
		admin.Unlock()
	}()

	// -agent-timeout scopes ONLY the agent command below, not the worktree
	// admin around it. actx is a child of ctx, so it's also cut short if ctx
	// itself ends first (e.g. -budget) — see the runErr handling below, which
	// tells the two apart so the report blames the right one.
	actx := ctx
	if p.AgentTimeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, p.AgentTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(actx, "sh", "-c", p.AgentCmd)
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
	if runErr != nil {
		switch {
		case ctx.Err() != nil:
			// The OUTER ctx ended out from under this agent — e.g. -budget
			// exhausted — not -agent-timeout specifically, even though actx
			// (derived from ctx) also reports done at this point. Checked
			// before actx below so the report blames the real cause.
			a.Stderr = tail("run ended before the agent finished (-budget exhausted): "+a.Stderr, 800)
		case actx.Err() == context.DeadlineExceeded:
			a.TimedOut = true
			a.Exit = -1
			a.Stderr = tail(fmt.Sprintf("agent-timeout (%s) exceeded: ", p.AgentTimeout)+a.Stderr, 800)
		}
	}

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

	applyLaneEnforcement(&a, t, p)
	return a, true
}

// applyLaneEnforcement compares an agent's ACTUAL write-set (a.Files) against
// its task's DECLARED file-set (t.Files), populating a's DeclaredFiles/
// ActualFiles/InLane/Strayed and, in -lanes strict, flipping a.OK to false on
// any stray (an out-of-lane agent is a failed agent — it does not land).
// Enforced only when the task declared files (the planner path; -tasks
// entries without Files are exempt) and the agent actually landed OK. Shared
// by runAgent (an agent that ran THIS run) and resumeAgent (-resume reusing a
// branch that survived from a prior one), so both apply the exact same lane
// rules regardless of which run actually produced the branch's commits.
func applyLaneEnforcement(a *perAgentJSON, t taskSpec, p runParams) {
	a.DeclaredFiles = t.Files
	a.ActualFiles = a.Files
	a.InLane = true
	if !a.OK || p.LaneMode == laneOff || len(t.Files) == 0 {
		return
	}
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

// resumeAgent implements -resume's per-task decision (see runParams.Resume):
//
//   - branch missing (no agent/<id> ref at all): nothing to reuse. Returns
//     reused=false so the caller falls through to an ordinary fresh
//     runAgentWithRetries call.
//   - branch exists but its head equals baseSHA (a STALE no-op: that agent
//     ran in a prior run but committed nothing): deleted here — its content
//     is byte-identical to base, so nothing is lost — and reused=false,
//     again falling through to a fresh run. Deleting first is required: it
//     clears the way for the fresh run's ordinary WorktreeAdd (`-b`,
//     loud-fail-on-collision) instead of needing runAgent's WorktreeAddReset
//     gating, which is reserved for branches THIS SAME RUN created itself
//     (see runAgent's doc comment) — a branch surviving from a PRIOR run
//     never qualifies for that, no matter how safe deleting it happens to be.
//   - branch exists and its head DIFFERS from baseSHA: real committed work
//     from a prior run. Reused as-is (reused=true) — the agent never runs
//     again — with a.Resumed=true and the same lane enforcement (see
//     applyLaneEnforcement) a freshly-run agent would get.
//
// admin is locked only around the delete (an admin-level git ref mutation,
// same posture as the worktree add/remove steps it's already shared with).
func resumeAgent(ctx context.Context, g *gitx.Git, admin *sync.Mutex, branch, baseSHA string, p runParams, t taskSpec) (perAgentJSON, bool) {
	head, err := g.RevParse(ctx, branch)
	if err != nil {
		return perAgentJSON{}, false // no surviving branch: run fresh
	}
	if head == baseSHA {
		admin.Lock()
		_ = g.BranchDelete(ctx, branch)
		admin.Unlock()
		return perAgentJSON{}, false // stale no-op branch: cleared, run fresh
	}

	a := perAgentJSON{ID: t.ID, Branch: branch, SHA: head, Files: []string{}, InLane: true, Resumed: true}
	files, ferr := g.DiffNameOnly(ctx, baseSHA, head)
	if ferr != nil {
		// Same invariant runAgent enforces: a.Files must never silently stay
		// at its zero-value "touched nothing" when the real diff couldn't be
		// computed (that would read as a legitimate no-op to integrateBranches'
		// OCC partitioning) — so a failed diff fails the reuse instead. The
		// branch itself is left untouched (its content is real, unlike the
		// no-op case above): never delete on an error we don't understand.
		a.Stderr = "resume: diff " + short(baseSHA) + ".." + short(head) + " failed: " + ferr.Error()
		return a, true
	}
	if len(files) > 0 {
		a.Files = files
	}
	a.OK = true
	applyLaneEnforcement(&a, t, p)
	return a, true
}

// runVerify runs -verify inside wtPath — an ALREADY checked-out detached
// worktree prepared by the caller (verifyWithRepair reuses the same one
// across every retry and repair round in a run; see its doc) — or, when
// -verify-impact is set and computeImpact can confidently scope changedFiles
// down to a set of impacted Go packages, -verify-impact instead (with
// SIGBOUND_IMPACTED_PKGS/SIGBOUND_CHANGED_FILES set). ANY doubt (see
// computeImpact) falls back to running -verify untouched — this whole scoping
// decision is a no-op, and Scope stays "" (never "full"), when -verify-impact
// isn't configured at all, so a run without it behaves byte-identically to
// before -verify-impact existed.
//
// Every invocation starts by resetting wtPath back to HEAD (git reset --hard,
// see gitx.ResetHard) and cleaning it (git clean -fdx, see gitx.Clean) so
// EITHER a modification to a tracked file OR an untracked build artifact left
// by a PRIOR invocation reusing this same worktree (a retry, or an earlier
// repair round's re-verify) can never leak into this one — the hermeticity
// guarantee that makes worktree reuse safe. Reset alone would leave stray
// untracked files, and clean alone would leave tracked-file edits in place
// (see both functions' docs); this needs both. The main working tree is
// never touched. logAttempt names the -logdir log file
// (verify-<logAttempt>.log); see runVerifyRetry.
//
// -verify-cache (p.VerifyCache): once the command to run is resolved (full
// -verify or the -verify-impact scoped command above — the cache key must
// see the SAME command a cache miss would actually execute, so this check
// runs after that decision, never before), a cache hit short-circuits
// straight to a green verdict without spawning verifyCmd at all. A miss
// falls through to the real invocation below and, only on a genuine pass,
// writes the entry. -verify-cache off means none of this runs — no git call
// to resolve a tree OID, no filesystem read — so a run without it is
// unaffected. See verifycache.go for the key/storage design.
func runVerify(ctx context.Context, g *gitx.Git, wtPath string, p runParams, changedFiles []string, logDir string, logAttempt int) verifyJSON {
	if err := g.At(wtPath).ResetHard(ctx); err != nil {
		return verifyJSON{Ran: true, OK: false, Output: "reset worktree: " + err.Error()}
	}
	if err := g.At(wtPath).Clean(ctx); err != nil {
		return verifyJSON{Ran: true, OK: false, Output: "clean worktree: " + err.Error()}
	}

	verifyCmd := p.VerifyCmd
	var scope string
	var impacted []string
	var extraEnv []string
	if strings.TrimSpace(p.VerifyImpactCmd) != "" {
		scope = "full"
		if pkgs, ok := computeImpact(ctx, wtPath, changedFiles); ok {
			scope = "impact"
			impacted = pkgs
			verifyCmd = p.VerifyImpactCmd
			extraEnv = []string{
				"SIGBOUND_IMPACTED_PKGS=" + strings.Join(pkgs, " "),
				"SIGBOUND_CHANGED_FILES=" + strings.Join(changedFiles, " "),
			}
		}
	}

	var treeOID string
	if p.VerifyCache {
		if oid, err := g.At(wtPath).TreeOID(ctx, "HEAD"); err == nil {
			treeOID = oid
			if verifyCacheLookup(ctx, g, treeOID, verifyCmd, impacted) {
				return verifyJSON{Ran: true, OK: true, Cached: true, Scope: scope, ImpactedPkgs: impacted,
					Output: "(cached: -verify already passed on this tree; command not run)"}
			}
		}
		// treeOID resolution failing is not itself fatal to verify — it just
		// means this invocation can neither be served from nor written to the
		// cache; the real command below still runs and decides OK honestly.
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", verifyCmd)
	cmd.Dir = wtPath
	cmd.WaitDelay = 2 * time.Second // return promptly on cancel; see runAgent
	cmd.Env = append(os.Environ(), extraEnv...)
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
	err := cmd.Run()
	ok := err == nil
	if p.VerifyCache && ok && treeOID != "" {
		verifyCacheStore(ctx, g, treeOID, verifyCmd, impacted)
	}
	return verifyJSON{Ran: true, OK: ok, Scope: scope, ImpactedPkgs: impacted, Output: tail(outBuf.String(), 2000)}
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
		if a.TimedOut {
			status += " TIMEOUT"
		}
		fmt.Fprintf(w, "  agent %-12s %-16s %-16s files=%v\n", a.ID, a.Branch, status, a.Files)
		if len(a.Strayed) > 0 {
			fmt.Fprintf(w, "    out-of-lane: wrote %v outside declared %v\n", a.Strayed, a.DeclaredFiles)
		}
		if a.Attempts > 1 {
			fmt.Fprintf(w, "    attempts: %d\n", a.Attempts)
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
		if r.Verify.Cached {
			fmt.Fprint(w, "  cached")
		}
		if r.Verify.Scope != "" {
			fmt.Fprintf(w, "  scope=%s", r.Verify.Scope)
			if r.Verify.Scope == "impact" {
				fmt.Fprintf(w, " (%d pkgs)", len(r.Verify.ImpactedPkgs))
			}
		}
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

// prepareManifestPath validates that -manifest's target file can be written,
// before any agent runs — the same fail-early posture as -logdir
// (prepareLogDir): a bad -manifest path must fail the run up front, never
// silently lose the run's provenance after the work is already done. Unlike
// -logdir (a directory of many logs), -manifest names one file: this creates
// its PARENT directory (if needed) and probes writability with a throwaway
// file dropped there, without touching the manifest path itself — nothing
// should exist at path until the run actually finishes (see writeManifest).
func prepareManifestPath(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("-manifest %s: %w", path, err)
	}
	probe, err := os.CreateTemp(dir, ".sigbound-manifest-check-*")
	if err != nil {
		return fmt.Errorf("-manifest %s is not writable: %w", path, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return nil
}

// writeManifest serializes rep as JSON to path (-manifest), independent of
// -json (which prints to stdout). Unlike prepareManifestPath's early check
// (which fails the WHOLE run before any agent runs), a failure HERE is
// best-effort: by the time driveRun has returned, real work may already be
// landed on -base, and losing the provenance record must never look like
// losing the run itself — so a failure is only ever warned, loudly, on
// stderr; it never changes the process exit code.
func writeManifest(path string, rep runReport) {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: -manifest %s: encode report: %v\n", path, err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: -manifest %s: %v\n", path, err)
	}
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
