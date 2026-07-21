// Command sigbench runs the Sigbound parallel-integration benchmark: N agents
// commit to one repo in parallel, then their branches are integrated by each
// selected strategy, with median/min/max timings over K runs and a correctness
// check per run.
//
// Single config (all strategies, 5 runs, 1 warmup):
//
//	sigbench -agents 64 -files 2000 -overlap 0.1 -runs 5 -warmup 1
//
// Just two strategies:
//
//	sigbench -agents 64 -strategy porcelain,overlay
//
// Full A/B sweep (agents 8..256 × overlap {0.1,0.5}, files=2000), the headline
// table:
//
//	sigbench -sweep -runs 5 -warmup 1
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/surya-koritala/sigbound/bench"
	"github.com/surya-koritala/sigbound/cell"
)

func main() {
	var (
		agents   = flag.Int("agents", 8, "number of parallel agents (N)")
		files    = flag.Int("files", 2000, "number of files in the base repo (M)")
		overlap  = flag.Float64("overlap", 0.1, "fraction of agents that touch a shared hot file (R)")
		conflict = flag.Float64("conflict", 0.0, "fraction of overlapping agents forced into a real conflict")
		hotfiles = flag.Int("hotfiles", 1, "number of shared hot files overlaps are spread across")
		seed     = flag.Int64("seed", 1, "RNG seed (reproducibility)")
		store    = flag.String("store", "", "optional SQLite path to persist run metadata (off hot path)")
		strategy = flag.String("strategy", "all", "comma-separated strategies, or 'all' (porcelain,naive,mergetree,overlay)")
		runs     = flag.Int("runs", 5, "measured runs per strategy (median/min/max reported)")
		warmup   = flag.Int("warmup", 1, "warmup runs per strategy (unmeasured)")
		sweep    = flag.Bool("sweep", false, "run the full agents×overlap sweep (files=2000, agents 8..256, overlap 0.1 & 0.5)")
	)
	flag.Parse()

	strategies, err := parseStrategies(*strategy)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sigbench:", err)
		os.Exit(1)
	}

	ctx := context.Background()

	if *sweep {
		if err := runSweep(ctx, strategies, *runs, *warmup, *seed); err != nil {
			fmt.Fprintln(os.Stderr, "sigbench: sweep error:", err)
			os.Exit(1)
		}
		return
	}

	cfg := bench.Config{
		Agents:    *agents,
		Files:     *files,
		Overlap:   *overlap,
		Conflict:  *conflict,
		HotFiles:  *hotfiles,
		Seed:      *seed,
		StorePath: *store,
	}
	ab, err := bench.RunAB(ctx, cfg, strategies, *warmup, *runs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sigbench: error:", err)
		os.Exit(1)
	}
	title := fmt.Sprintf("agents=%d files=%d overlap=%.2f conflict=%.2f hotfiles=%d seed=%d runs=%d warmup=%d",
		cfg.Agents, cfg.Files, cfg.Overlap, cfg.Conflict, cfg.HotFiles, cfg.Seed, *runs, *warmup)
	fmt.Printf("commit phase: %d commits in %.1fms = %.0f commits/sec (fast-import fixture; benchmark-only, not a real per-agent commit rate)\n",
		cfg.Agents, ab.CommitWall.Seconds()*1000, ab.CommitsPerSec)
	bench.FormatSweep(os.Stdout, title, []bench.ABResult{ab}, strategies)

	if !gateOK(ab, strategies) {
		fmt.Fprintln(os.Stderr, "sigbench: CORRECTNESS FAILURE")
		os.Exit(2)
	}
}

// runSweep prints the headline A/B table: agents {8,32,64,128,256} × overlap
// {0.1, 0.5} at files=2000, conflict=0 (all changes must land), for each strategy.
func runSweep(ctx context.Context, strategies []string, runs, warmup int, seed int64) error {
	agentCounts := []int{8, 32, 64, 128, 256}
	overlaps := []float64{0.1, 0.5}
	for _, ov := range overlaps {
		var results []bench.ABResult
		for _, n := range agentCounts {
			cfg := bench.Config{Agents: n, Files: 2000, Overlap: ov, Conflict: 0, HotFiles: 1, Seed: seed}
			ab, err := bench.RunAB(ctx, cfg, strategies, warmup, runs)
			if err != nil {
				return fmt.Errorf("agents=%d overlap=%.2f: %w", n, ov, err)
			}
			results = append(results, ab)
			fmt.Fprintf(os.Stderr, "  [sweep] done agents=%d overlap=%.2f commits/s=%.0f\n", n, ov, ab.CommitsPerSec)
			if !gateOK(ab, strategies) {
				fmt.Fprintf(os.Stderr, "  [sweep] CORRECTNESS FAILURE at agents=%d overlap=%.2f\n", n, ov)
			}
		}
		bench.FormatSweep(os.Stdout, fmt.Sprintf("overlap=%.2f files=2000 conflict=0 seed=%d runs=%d warmup=%d",
			ov, seed, runs, warmup), results, strategies)
	}
	return nil
}

// gateOK is true when every measured strategy stayed correct and, where
// comparable, produced the shared reference tree.
func gateOK(ab bench.ABResult, strategies []string) bool {
	for _, s := range strategies {
		st := ab.Stats[s]
		if st == nil || st.Err != nil || !st.Correct {
			return false
		}
		if st.TreeComparable && !st.TreeMatchesRef {
			return false
		}
	}
	return true
}

// parseStrategies expands "all" or validates a comma-separated list.
func parseStrategies(s string) ([]string, error) {
	if s == "" || s == "all" {
		return cell.AvailableStrategies(), nil
	}
	valid := map[string]bool{}
	for _, v := range cell.AvailableStrategies() {
		valid[v] = true
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == "occ" {
			p = cell.StrategyMergeTree
		}
		if !valid[p] {
			return nil, fmt.Errorf("unknown strategy %q (have %v or 'all')", p, cell.AvailableStrategies())
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no strategies selected")
	}
	return out, nil
}
