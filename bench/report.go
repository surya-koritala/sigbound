package bench

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// FormatResult renders one run as a human-readable block: configuration, the
// raw parallel commit throughput, then the naive-vs-OCC comparison and the OCC
// partition stats, ending with the correctness verdict.
func FormatResult(w io.Writer, r Result) {
	c := r.Config
	fmt.Fprintf(w, "== agents=%d files=%d overlap=%.2f conflict=%.2f hotfiles=%d seed=%d ==\n",
		c.Agents, c.Files, c.Overlap, c.Conflict, c.HotFiles, c.Seed)

	fmt.Fprintf(w, "  parallel commit phase: %d commits in %s = %.0f commits/sec (own-branch, no ref contention)\n",
		c.Agents, dur(r.CommitWall), r.CommitsPerSec)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  strategy\twall\tbranches/sec\tgroups\tmax-parallel\tserial-depth\tlanded\tauto-merged\tflagged")
	fmt.Fprintf(tw, "  %s\t%s\t%.0f\t%d\t%d\t%d\t%d\t%d\t%d\n",
		label(r.Naive.Strategy, "baseline"), dur(r.Naive.Duration), r.NaiveBranchesPerSec,
		r.Naive.Groups, r.Naive.MaxBatch, r.Naive.LargestGroup,
		len(r.Naive.Landed), r.Naive.AutoMerged, len(r.Naive.Flagged))
	fmt.Fprintf(tw, "  %s\t%s\t%.0f\t%d\t%d\t%d\t%d\t%d\t%d\n",
		label(r.OCC.Strategy, "candidate"), dur(r.OCC.Duration), r.OCCBranchesPerSec,
		r.OCC.Groups, r.OCC.MaxBatch, r.OCC.LargestGroup,
		len(r.OCC.Landed), r.OCC.AutoMerged, len(r.OCC.Flagged))
	_ = tw.Flush()

	fmt.Fprintf(w, "  %s speedup vs %s: %.2fx\n",
		label(r.OCC.Strategy, "candidate"), label(r.Naive.Strategy, "baseline"), r.Speedup)
	fmt.Fprintf(w, "  correctness: %s=%s %s=%s trees-equal=%s\n",
		label(r.Naive.Strategy, "baseline"), yn(r.NaiveCorrect),
		label(r.OCC.Strategy, "candidate"), yn(r.OCCCorrect), treeEq(r))
	if len(r.OCC.Flagged) > 0 {
		var names []string
		for _, f := range r.OCC.Flagged {
			names = append(names, fmt.Sprintf("%s(%s)", f.Branch, strings.Join(f.Conflicts, ",")))
		}
		fmt.Fprintf(w, "  %s flagged-for-human (seam for bring-your-own-model resolver): %s\n",
			label(r.OCC.Strategy, "candidate"), strings.Join(names, " "))
	}
	fmt.Fprintln(w)
}

// label falls back to a role name when a strategy string is empty.
func label(strategy, fallback string) string {
	if strategy == "" {
		return fallback
	}
	return strategy
}

func dur(d interface{ Seconds() float64 }) string {
	return fmt.Sprintf("%.1fms", d.Seconds()*1000)
}

func yn(b bool) string {
	if b {
		return "PASS"
	}
	return "FAIL"
}

func treeEq(r Result) string {
	if len(r.Naive.Flagged) > 0 || len(r.OCC.Flagged) > 0 {
		return "n/a(conflicts)"
	}
	return yn(r.TreesEqual)
}
