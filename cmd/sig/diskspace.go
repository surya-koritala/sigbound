// Disk-space preflight (issue #92): driveRun creates one full worktree
// checkout per task; on a large repo a big -n/-tasks run can exhaust the
// filesystem mid-flight — AFTER real API spend on the agents that already
// ran — failing later agents with a confusing `worktree add`/write ENOSPC.
// diskPreflight estimates the fan-out's footprint (task count x checked-out
// tree size, see gitx.TreeSize) against free space on the worktree root's
// filesystem and refuses the run up front when it's clearly not going to
// fit. `sig doctor` surfaces the same numbers as an informational line.
//
// Sparse lanes (issue #86, not yet built) would check out only each agent's
// declared files instead of the full tree, shrinking the true footprint
// well below this estimate — today's full-checkout WorktreeAdd is exactly
// what this bounds, so the estimate will get more conservative than
// necessary once that lands, never less safe.
package main

import (
	"context"
	"fmt"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// diskFreeBytes is the free-space getter diskPreflight/diskInfoLine call —
// statfsFree (see diskspace_unix.go / diskspace_other.go) by default,
// factored into a var so a test can fabricate a low free-space reading
// without needing a real near-full filesystem, the same seam pattern
// openLogFile gives openLog.
var diskFreeBytes = statfsFree

// referenceAgentRun is the agent count `sig doctor`'s disk line illustrates
// against — the exact number from issue #92's motivating scenario ("a
// 512-agent run creates 512 worktrees of the full tree"). It has no bearing
// on any REAL run's estimate (diskPreflight always uses that run's actual
// task count); it's just a fixed, memorable yardstick for a command that
// has no task count of its own to work with.
const referenceAgentRun = 512

// diskShortfall is the pure arithmetic behind the preflight verdict: nTasks
// full checkouts at treeBytes each, padded by a 10% safety margin before
// comparing to free. The margin exists because the raw ls-tree blob-size sum
// undercounts real on-disk usage a bit (filesystem block rounding, a
// worktree's own per-checkout admin files, packed-object growth as
// `worktree add` unpacks into the shared store) — padding it keeps an
// estimate that's already approximate from flip-flopping right at the edge.
// refuse is true only once the padded estimate exceeds free.
func diskShortfall(treeBytes int64, nTasks int, free uint64) (estimate, margin int64, refuse bool) {
	estimate = treeBytes * int64(nTasks)
	margin = estimate + estimate/10
	return estimate, margin, margin > 0 && uint64(margin) > free
}

// diskPreflight refuses a run BEFORE any agent starts (and so before any
// real API spend) when driveRun's worktree fan-out would plausibly exhaust
// the filesystem under wtRoot. It fails OPEN — returns nil, letting the run
// proceed — on anything that keeps it from reaching a confident verdict: a
// tree size git can't compute, or a free-space reading the platform/
// filesystem can't give (see diskFreeBytes). An estimate that can't be
// formed must never block a run that might otherwise have been fine; only an
// ACTUAL, confident shortfall refuses. -no-disk-check skips this call
// entirely for a caller who has already reasoned about their own headroom.
func diskPreflight(ctx context.Context, g *gitx.Git, baseSHA, wtRoot string, nTasks int) error {
	if nTasks <= 0 {
		return nil
	}
	treeBytes, err := g.TreeSize(ctx, baseSHA)
	if err != nil {
		return nil // best-effort; see doc comment
	}
	free, ok := diskFreeBytes(wtRoot)
	if !ok {
		return nil // unsupported platform, or unreadable filesystem; see doc comment
	}
	estimate, margin, refuse := diskShortfall(treeBytes, nTasks, free)
	if !refuse {
		return nil
	}
	return fmt.Errorf("disk preflight: %d worktree(s) x %s tree = ~%s needed (~%s with a 10%% safety margin), "+
		"but only %s free on %s; pass -no-disk-check to skip this estimate and run anyway",
		nTasks, humanSize(treeBytes), humanSize(estimate), humanSize(margin), humanSize(int64(free)), wtRoot)
}

// diskInfoLine renders `sig doctor`'s informational disk-space line. Unlike
// doctor's pass/fail checks, this NEVER fails doctor — see diskPreflight's
// doc comment for why an estimate that can't be formed must stay silent
// rather than alarming. repoDir is -repo as passed to `sig doctor`; "" (no
// -repo given) falls back to the current directory, the natural guess for
// what someone running `sig doctor` from inside their repo means.
func diskInfoLine(ctx context.Context, repoDir string) string {
	dir := repoDir
	if dir == "" {
		dir = "."
	}
	treeBytes, sizeErr := gitx.New(dir).TreeSize(ctx, "HEAD")
	free, freeOK := diskFreeBytes(dir)

	switch {
	case sizeErr != nil:
		return fmt.Sprintf("disk: unable to determine repo tree size in %s (%v)", dir, sizeErr)
	case !freeOK:
		return fmt.Sprintf("disk: repo tree ~%s; free space unknown on %s (unsupported platform)", humanSize(treeBytes), dir)
	default:
		need := treeBytes * referenceAgentRun
		return fmt.Sprintf("disk: repo tree ~%s, free %s on %s (a %d-agent run needs ~%s)",
			humanSize(treeBytes), humanSize(int64(free)), dir, referenceAgentRun, humanSize(need))
	}
}
