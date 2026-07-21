package bench

// ab.go is the RIGOROUS benchmark: for each strategy, run W unmeasured warmups
// then K measured integrations against the SAME committed fixture, and report the
// median / min / max / p90 wall-clock — not a single noisy number. Correctness is
// checked on every measured run, and every strategy's final tree OID is compared
// against a shared reference so "trees-equal" is verified across ALL strategies,
// not just naive-vs-occ.

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/surya-koritala/sigbound/cell"
)

// StratStat is the measured distribution for one strategy over K runs.
type StratStat struct {
	Strategy              string
	Runs                  []time.Duration // K measured durations
	Median, Min, Max, P90 time.Duration

	Correct        bool // tree held every landed change, on every measured run
	TreeComparable bool // no measured run flagged a conflict (OID compare is meaningful)
	TreeMatchesRef bool // final tree OID == the shared reference, on every run

	Landed, Flagged, AutoMerged    int // from the last measured run (stable across runs)
	Groups, MaxBatch, LargestGroup int

	Err error
}

// ABResult is one config measured across several strategies.
type ABResult struct {
	Config        Config
	Warmup, Runs  int
	CommitWall    time.Duration
	CommitsPerSec float64
	Order         []string // strategy order as requested
	Stats         map[string]*StratStat
	RefTreeOID    string // reference final tree OID (first conflict-free run seen)
}

// RunAB builds the fixture once, then measures each strategy with warmups + runs
// measured passes. Reference tree comes from the first conflict-free run of the
// first strategy; every other conflict-free run must match it, which is the
// cross-strategy correctness invariant the project must never break.
func RunAB(ctx context.Context, cfg Config, strategies []string, warmup, runs int) (ABResult, error) {
	cfg = cfg.withDefaults()
	if runs <= 0 {
		runs = 5
	}
	if warmup < 0 {
		warmup = 0
	}
	out := ABResult{Config: cfg, Warmup: warmup, Runs: runs, Order: strategies, Stats: map[string]*StratStat{}}

	h, cleanup, err := setup(ctx, cfg)
	if err != nil {
		return out, err
	}
	defer cleanup()
	out.CommitWall = h.commitWall
	out.CommitsPerSec = h.commitsPerSec

	integ := cell.NewIntegrator(h.g)

	for _, s := range strategies {
		st := &StratStat{Strategy: s, Correct: true, TreeComparable: true, TreeMatchesRef: true}

		// Warmups prime the OS page cache / git internals; not measured.
		for w := 0; w < warmup && st.Err == nil; w++ {
			if _, err := integ.Integrate(ctx, h.base, h.changes, s); err != nil {
				st.Err = fmt.Errorf("warmup: %w", err)
			}
		}

		for r := 0; r < runs && st.Err == nil; r++ {
			ir, err := integ.Integrate(ctx, h.base, h.changes, s)
			if err != nil {
				st.Err = err
				break
			}
			st.Runs = append(st.Runs, ir.Duration)
			st.Landed, st.Flagged, st.AutoMerged = len(ir.Landed), len(ir.Flagged), ir.AutoMerged
			st.Groups, st.MaxBatch, st.LargestGroup = ir.Groups, ir.MaxBatch, ir.LargestGroup

			ok, err := treeHasAllLanded(ctx, h.g, ir, h.branchToPriv)
			if err != nil {
				st.Err = err
				break
			}
			st.Correct = st.Correct && ok

			if len(ir.Flagged) == 0 {
				toid, err := h.g.TreeOID(ctx, ir.FinalSHA)
				if err != nil {
					st.Err = err
					break
				}
				if out.RefTreeOID == "" {
					out.RefTreeOID = toid
				}
				if toid != out.RefTreeOID {
					st.TreeMatchesRef = false
				}
			} else {
				st.TreeComparable = false
			}
		}
		st.Median, st.Min, st.Max, st.P90 = summarize(st.Runs)
		out.Stats[s] = st
	}
	return out, nil
}

// summarize returns median, min, max and nearest-rank p90 of the durations.
func summarize(ds []time.Duration) (median, mn, mx, p90 time.Duration) {
	if len(ds) == 0 {
		return 0, 0, 0, 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	mn, mx = s[0], s[len(s)-1]
	if len(s)%2 == 1 {
		median = s[len(s)/2]
	} else {
		median = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	rank := int(math.Ceil(0.9*float64(len(s)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	p90 = s[rank]
	return median, mn, mx, p90
}

func msOf(d time.Duration) float64 { return d.Seconds() * 1000 }

// FormatSweep prints, for a set of ABResults that share an overlap (varying only
// agent count), a median wall-clock table with speedups + correctness, then a
// compact min–max spread table so variance is visible.
func FormatSweep(w io.Writer, title string, results []ABResult, strategies []string) {
	fmt.Fprintf(w, "\n=== %s ===\n", title)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	hdr := "agents"
	for _, s := range strategies {
		hdr += "\t" + s + "(ms)"
	}
	hdr += "\tmt/naive\tovl/naive\tmt/porc\tovl/porc\ttrees-eq\tcorrect\tcommits/s"
	fmt.Fprintln(tw, hdr)
	for _, r := range results {
		row := fmt.Sprintf("%d", r.Config.Agents)
		for _, s := range strategies {
			row += "\t" + medCell(r.Stats[s])
		}
		row += "\t" + speedup(r, "mergetree", "naive")
		row += "\t" + speedup(r, "overlay", "naive")
		row += "\t" + speedup(r, "mergetree", "porcelain")
		row += "\t" + speedup(r, "overlay", "porcelain")
		row += "\t" + treesEqAll(r, strategies)
		row += "\t" + correctAll(r, strategies)
		row += fmt.Sprintf("\t%.0f", r.CommitsPerSec)
		fmt.Fprintln(tw, row)
	}
	_ = tw.Flush()

	fmt.Fprintln(w, "  spread (min–max ms):")
	tw2 := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	h2 := "  agents"
	for _, s := range strategies {
		h2 += "\t" + s
	}
	fmt.Fprintln(tw2, h2)
	for _, r := range results {
		row := fmt.Sprintf("  %d", r.Config.Agents)
		for _, s := range strategies {
			st := r.Stats[s]
			if st == nil || len(st.Runs) == 0 {
				row += "\t-"
				continue
			}
			row += fmt.Sprintf("\t%.1f–%.1f", msOf(st.Min), msOf(st.Max))
		}
		fmt.Fprintln(tw2, row)
	}
	_ = tw2.Flush()
}

func medCell(st *StratStat) string {
	if st == nil {
		return "-"
	}
	if st.Err != nil {
		return "ERR"
	}
	if len(st.Runs) == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", msOf(st.Median))
}

// speedup is base_median / cand_median (>1 means cand is faster).
func speedup(r ABResult, cand, base string) string {
	c, b := r.Stats[cand], r.Stats[base]
	if c == nil || b == nil || c.Err != nil || b.Err != nil ||
		len(c.Runs) == 0 || len(b.Runs) == 0 || c.Median <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.2fx", b.Median.Seconds()/c.Median.Seconds())
}

func treesEqAll(r ABResult, strategies []string) string {
	comparable := false
	for _, s := range strategies {
		st := r.Stats[s]
		if st == nil || st.Err != nil {
			continue
		}
		if !st.TreeComparable {
			continue
		}
		comparable = true
		if !st.TreeMatchesRef {
			return "NO"
		}
	}
	if !comparable {
		return "n/a"
	}
	return "yes"
}

func correctAll(r ABResult, strategies []string) string {
	for _, s := range strategies {
		st := r.Stats[s]
		if st == nil {
			continue
		}
		if st.Err != nil {
			return "ERR"
		}
		if !st.Correct {
			return "FAIL"
		}
	}
	return "PASS"
}
