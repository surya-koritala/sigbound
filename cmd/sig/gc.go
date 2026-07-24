// sig gc sweeps debris a killed or crashed sig run leaves behind: git
// worktree registrations whose directory is gone (`git worktree prune`
// equivalents), sigbound's own tempdir patterns under os.TempDir(), and
// agent/*/imported/*/* branches that outlived their run.
//
// Default is DRY-RUN: sig gc only prints what it would remove and exits 0.
// Nothing is ever deleted without -delete. A branch a run manifest under
// .git/sigbound/runs still references is kept unless -force is also given
// (loudly, per branch) -- that manifest is exactly what `sig run -resume`
// reads to decide which agent/<id> branches it can reuse (see run.go's
// resumeAgent), so sweeping one out from under a resumable run would
// silently break resume.
//
//	sig gc -repo PATH [-older-than 72h] [-delete] [-force] [-json]
//
// gc NEVER touches: the base branch or any branch outside agent/ and
// imported/<worker>/, refs/notes/sigbound, or the run history itself under
// .git/sigbound/runs (deliberately out of scope -- see docs/USAGE.md).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

// gcDefaultOlderThan is -older-than's default: debris younger than this is
// left alone even with -delete, on the theory that a run finishing in the
// last few days might still be under someone's nose.
const gcDefaultOlderThan = 72 * time.Hour

// gcTempPatterns are the os.TempDir() glob patterns every sigbound tempdir
// is created under for the lifetime of a single agent/verify/bisect/repair
// invocation (see run.go's wtRoot/verify/bisect/repair MkdirTemp calls and
// replay.go's verify tempdir) -- anything matching one of these that
// survives past a run is debris from a crash, never something else on the
// machine legitimately created.
var gcTempPatterns = []string{"sig-run-*", "sig-verify-*", "sig-bisect-*", "sig-repair-*", "sig-replay-verify-*"}

// gcBranchPrefixes are the ONLY ref prefixes gc will ever consider removing.
// Nothing outside these -- most importantly the base branch itself -- is a
// gc candidate at all.
var gcBranchPrefixes = []string{"refs/heads/agent/", "refs/heads/imported/"}

// gcReport is `sig gc`'s -json contract. WorktreesPruned/Tempdirs are
// stages (1)/(2) of the sweep (see package doc); BranchesDeleted/
// BranchesKept are stage (3). In dry-run mode this describes what WOULD
// happen; with -delete, what DID.
type gcReport struct {
	WorktreesPruned int      `json:"worktreesPruned"`
	Tempdirs        []string `json:"tempdirs"`
	BranchesDeleted []string `json:"branchesDeleted"`
	BranchesKept    []string `json:"branchesKept"`
}

// gcPlan is everything gcPlanFor computed, before anything is (maybe)
// applied. It carries the WHY behind each entry (a worktree's prune reason,
// which deletes were manifest-protected but -force overrode) that gcReport
// deliberately drops to match the -json contract. Dry-run and -delete
// render the SAME plan; -delete additionally executes it (applyGC) so what
// gets printed is exactly what happens, never an approximation.
type gcPlan struct {
	StaleWorktrees []gitx.WorktreeEntry
	Tempdirs       []string
	ToDelete       []string
	ToKeep         []string
	Forced         map[string]bool // subset of ToDelete that was manifest-protected but -force overrode
}

func (p gcPlan) report() gcReport {
	rep := gcReport{
		WorktreesPruned: len(p.StaleWorktrees),
		Tempdirs:        p.Tempdirs,
		BranchesDeleted: p.ToDelete,
		BranchesKept:    p.ToKeep,
	}
	if rep.Tempdirs == nil {
		rep.Tempdirs = []string{}
	}
	if rep.BranchesDeleted == nil {
		rep.BranchesDeleted = []string{}
	}
	if rep.BranchesKept == nil {
		rep.BranchesKept = []string{}
	}
	return rep
}

func runGC(w io.Writer, argv []string) (int, error) {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig gc -repo PATH [-older-than 72h] [-delete] [-force] [-json]")
		fmt.Fprintln(fs.Output(), "sweeps debris a killed/crashed sig run leaves: stale worktree registrations,")
		fmt.Fprintln(fs.Output(), "sigbound tempdirs under the OS temp dir, and old agent/imported branches.")
		fmt.Fprintln(fs.Output(), "dry-run by default: nothing is deleted without -delete; exits 0 either way")
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "path to the target git repository")
	olderThan := fs.Duration("older-than", gcDefaultOlderThan, "age cutoff for tempdirs (mtime) and agent/imported branches (commit date); "+
		"ignored by worktree-registration pruning, which is always safe regardless of age")
	doDelete := fs.Bool("delete", false, "actually remove debris; without this, sig gc only prints what it would remove and changes nothing")
	force := fs.Bool("force", false, "also delete agent/imported branches a run manifest under .git/sigbound/runs still references "+
		"(printed as a loud per-branch warning -- that run's -resume can no longer reuse it); never bypasses -older-than, "+
		"and has no effect on worktree pruning or tempdirs")
	asJSON := fs.Bool("json", false, "emit the result as JSON: {worktreesPruned, tempdirs, branchesDeleted, branchesKept}")
	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitOK, nil
		}
		return exitOperationalError, err
	}
	if strings.TrimSpace(*repo) == "" {
		return exitOperationalError, errors.New("-repo is required")
	}

	c, err := cell.Open(*repo)
	if err != nil {
		return exitOperationalError, err
	}
	ctx := context.Background()

	plan, err := gcPlanFor(ctx, c.Git(), *olderThan, *force)
	if err != nil {
		return exitOperationalError, err
	}

	if *doDelete {
		if err := applyGC(ctx, c, plan); err != nil {
			return exitOperationalError, err
		}
	}

	if *asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(plan.report()); err != nil {
			return exitOperationalError, err
		}
		return exitOK, nil
	}
	printGCTable(w, plan, *doDelete)
	return exitOK, nil
}

// gcPlanFor computes the full sweep plan against g without mutating
// anything: stale worktree registrations (always candidates, no age gate),
// old sigbound tempdirs under os.TempDir(), and old agent/imported branches
// split into ToDelete/ToKeep by manifest protection and force.
func gcPlanFor(ctx context.Context, g *gitx.Git, olderThan time.Duration, force bool) (gcPlan, error) {
	entries, err := g.WorktreeList(ctx)
	if err != nil {
		return gcPlan{}, fmt.Errorf("list worktrees: %w", err)
	}
	var stale []gitx.WorktreeEntry
	for _, e := range entries {
		if e.Prunable != "" {
			stale = append(stale, e)
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Path < stale[j].Path })

	cutoff := time.Now().Add(-olderThan)
	tempdirs, err := scanTempdirs(os.TempDir(), cutoff)
	if err != nil {
		return gcPlan{}, fmt.Errorf("scan tempdirs: %w", err)
	}

	branches, err := g.ForEachRefCommit(ctx, gcBranchPrefixes...)
	if err != nil {
		return gcPlan{}, fmt.Errorf("list agent/imported branches: %w", err)
	}
	protected, err := loadProtectedBranches(ctx, g)
	if err != nil {
		return gcPlan{}, fmt.Errorf("load protected branches from .git/sigbound/runs: %w", err)
	}

	plan := gcPlan{StaleWorktrees: stale, Tempdirs: tempdirs, Forced: map[string]bool{}}
	for _, b := range branches {
		if !b.CommitTime.Before(cutoff) {
			continue // not old enough: not a candidate at all, regardless of -force
		}
		if protected[b.Name] {
			if force {
				plan.Forced[b.Name] = true
				plan.ToDelete = append(plan.ToDelete, b.Name)
			} else {
				plan.ToKeep = append(plan.ToKeep, b.Name)
			}
			continue
		}
		plan.ToDelete = append(plan.ToDelete, b.Name)
	}
	sort.Strings(plan.ToDelete)
	sort.Strings(plan.ToKeep)
	return plan, nil
}

// scanTempdirs finds every sigbound tempdir under root matching
// gcTempPatterns whose mtime is at or before cutoff. A directory newer than
// cutoff is left alone even if it otherwise matches: staying fresh is the
// ONLY liveness signal available here (there is no PID or lock file to
// check -- see the package's honesty about this in docs/USAGE.md), so an
// agent/verify/bisect/repair invocation still actively writing into its own
// tempdir is never swept out from under it.
func scanTempdirs(root string, cutoff time.Time) ([]string, error) {
	var out []string
	for _, pat := range gcTempPatterns {
		matches, err := filepath.Glob(filepath.Join(root, pat))
		if err != nil {
			return nil, fmt.Errorf("glob %s: %w", pat, err)
		}
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil {
				continue // vanished between Glob and Stat: nothing left to sweep
			}
			if !fi.IsDir() {
				continue // MkdirTemp always creates a directory; a same-named file isn't ours
			}
			if fi.ModTime().After(cutoff) {
				continue // newer than the cutoff: possibly still in use, skip
			}
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out, nil
}

// loadProtectedBranches scans every .git/sigbound/runs/*/report.json (the
// durable run storage `sig serve` writes, see serve.go's writeRunReport)
// and returns the set of branch names those manifests reference: every
// agent's branch (runReport.PerAgent[].Branch, ok or not) plus every branch
// -verify-bisect dropped (runReport.Integrate.DroppedByBisect). This is
// exactly the set `sig run -resume` might still read a branch out of --
// resumeAgent reuses ANY branch that still exists and differs from the
// manifest's baseSHA, regardless of whether that agent's own run was OK --
// so sweeping one of these out from under a resumable run would silently
// break resume.
//
// A run directory with no report.json (crashed before finishing, or only
// ever wrote error.json) contributes nothing -- there's no manifest to
// protect anything for. But a report.json that EXISTS and fails to read or
// parse is a loud error that aborts gc entirely: guessing "protects
// nothing" for a manifest gc can't actually read is exactly the wrong
// direction to fail open in (it would let real debris — a branch a corrupt
// manifest still names — get swept), so this refuses to run rather than
// risk it, matching the project's "bad plan => no run" fail-safe posture.
func loadProtectedBranches(ctx context.Context, g *gitx.Git) (map[string]bool, error) {
	common, err := g.GitCommonDir(ctx)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(common, "sigbound", "runs", "*", "report.json"))
	if err != nil {
		return nil, fmt.Errorf("glob run reports: %w", err)
	}
	protected := map[string]bool{}
	for _, m := range matches {
		data, rerr := os.ReadFile(m)
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", m, rerr)
		}
		var rep runReport
		if jerr := json.Unmarshal(data, &rep); jerr != nil {
			return nil, fmt.Errorf("parse %s: %w", m, jerr)
		}
		for _, a := range rep.PerAgent {
			if a.Branch != "" {
				protected[a.Branch] = true
			}
		}
		for _, b := range rep.Integrate.DroppedByBisect {
			protected[b] = true
		}
	}
	return protected, nil
}

// applyGC executes exactly the plan gcPlanFor computed: a real
// WorktreePrune, a real os.RemoveAll per stale tempdir, a real branch
// delete for every plan.ToDelete entry (which already folds in -force's
// overrides). Nothing here recomputes candidacy -- the plan is the single
// source of truth for both "would happen" and "did happen".
func applyGC(ctx context.Context, c *cell.Cell, plan gcPlan) error {
	if len(plan.StaleWorktrees) > 0 {
		if err := c.Git().WorktreePrune(ctx); err != nil {
			return fmt.Errorf("worktree prune: %w", err)
		}
	}
	for _, dir := range plan.Tempdirs {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove tempdir %s: %w", dir, err)
		}
	}
	for _, b := range plan.ToDelete {
		if err := c.DeleteBranch(ctx, b); err != nil {
			return fmt.Errorf("delete branch %s: %w", b, err)
		}
	}
	return nil
}

// printGCTable renders plan as sig gc's human-readable (non -json) output.
// applied is true for a real -delete run (past tense: "removed"), false for
// dry-run (future tense: "would remove") -- the two describe the identical
// plan, never a different computation.
func printGCTable(w io.Writer, plan gcPlan, applied bool) {
	action := "would remove"
	if applied {
		action = "removed"
	}
	fmt.Fprintf(w, "stale worktrees: %d %s\n", len(plan.StaleWorktrees), action)
	for _, e := range plan.StaleWorktrees {
		fmt.Fprintf(w, "  %s (%s)\n", e.Path, e.Prunable)
	}
	fmt.Fprintf(w, "old tempdirs: %d %s\n", len(plan.Tempdirs), action)
	for _, d := range plan.Tempdirs {
		fmt.Fprintf(w, "  %s\n", d)
	}
	fmt.Fprintf(w, "sweepable branches: %d %s, %d kept\n", len(plan.ToDelete), action, len(plan.ToKeep))
	for _, b := range plan.ToDelete {
		note := ""
		if plan.Forced[b] {
			note = " (FORCED -- manifest-referenced; -resume for that run can no longer reuse it)"
		}
		fmt.Fprintf(w, "  %s: %s%s\n", action, b, note)
	}
	for _, b := range plan.ToKeep {
		fmt.Fprintf(w, "  kept (manifest-referenced): %s\n", b)
	}
	if !applied {
		fmt.Fprintln(w, "dry-run: nothing was removed; run with -delete to actually remove it")
	}
}

// gcInfoLine renders `sig doctor`'s informational one-line gc summary: how
// much debris a `sig gc` run would find right now, using gc's own default
// -older-than cutoff and never -force (so the branch count matches what a
// bare `sig gc -repo ... -delete` would actually remove, not what -force
// could additionally reach). Like diskInfoLine, this NEVER fails doctor: an
// unreadable repo, a corrupt manifest, or any other scan error just yields
// a quieter line instead of a FAIL -- it's advisory, not one of doctor's
// pass/fail checks.
func gcInfoLine(ctx context.Context, repoDir string) string {
	dir := repoDir
	if dir == "" {
		dir = "."
	}
	g := gitx.New(dir)
	plan, err := gcPlanFor(ctx, g, gcDefaultOlderThan, false)
	if err != nil {
		return fmt.Sprintf("gc: unable to scan %s for debris (%v)", dir, err)
	}
	return fmt.Sprintf("gc: %d stale worktree(s), %d sweepable branch(es), %d old tempdir(s) (run sig gc)",
		len(plan.StaleWorktrees), len(plan.ToDelete), len(plan.Tempdirs))
}
