// Command sig is a thin CLI over the cell integration engine. It does NOT
// reimplement integration — it only computes each branch's write-set and hands
// the batch to cell.Integrator, which partitions, parallelizes, lands the
// non-conflicting branches onto the base branch, and flags real conflicts. The
// result is printed as JSON.
//
//	sig integrate -repo PATH -base main -branches agent/t1,agent/t2,agent/t3 -strategy overlay
//
// -strategy selects the engine (overlay by default); see cell.AvailableStrategies.
// By default the base branch ref is advanced to the integrated commit; pass
// -no-land to leave the final commit detached for inspection.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "integrate":
		if err := runIntegrate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "sig integrate:", err)
			os.Exit(1)
		}
	case "run":
		code, err := runRun(os.Stdout, os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "sig run:", err)
		}
		if code != 0 {
			os.Exit(code)
		}
	case "doctor":
		code, err := runDoctor(os.Stdout, os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "sig doctor:", err)
		}
		if code != 0 {
			os.Exit(code)
		}
	case "version", "-v", "--version":
		runVersion(os.Stdout)
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "sig: unknown subcommand %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  sig integrate -repo PATH -base BRANCH -branches b1,b2,.. [-strategy overlay] [-assert] [-no-land]")
	fmt.Fprintln(w, "  sig run       -repo PATH -base BRANCH (-tasks FILE | -goal STRING -planner CMD [-n N]) -agent CMD [-strategy overlay] [-assert] [-resolver CMD] [-verify CMD [-repair CMD [-repair-max N]]] [-lanes off|warn|strict] [-no-autocommit] [-json]")
	fmt.Fprintln(w, "  sig doctor    [-repo PATH]")
	fmt.Fprintln(w, "  sig version")
	fmt.Fprintln(w, "strategies:", strings.Join(cell.AvailableStrategies(), ", "))
}

// integrateBranches computes each branch's write-set versus baseSHA and hands the
// batch to cell.Integrator — the ONE integration code path in this binary. Both
// `sig integrate` and `sig run` call it, so the driver never reimplements
// partition / parallel folding / conflict handling / landing; it only supplies
// the branch names and the same resolver/strategy/assert knobs. When land is
// true the base branch ref is advanced to the integrated commit.
//
// writeSets carries any ALREADY-COMPUTED write-sets (branch -> paths), e.g.
// `sig run`'s runAgent already ran `git diff` per agent for lane enforcement —
// reusing that here avoids re-diffing the same branch. A nil map, or a branch
// missing from it (or mapped to nil), is not treated as "no changes"; its
// write-set is computed here instead, for every such branch in ONE batched
// diff-tree call rather than a `git diff --name-only` fork per branch (see
// gitx.DiffNameOnlyBatch). `sig integrate` has no precomputed data, so it
// always passes nil and every branch goes through the batched path.
func integrateBranches(ctx context.Context, g *gitx.Git, baseRef, baseSHA string, branches []string, writeSets map[string][]string, strategy, resolverCmd string, resolverTimeout time.Duration, assert, land bool) (cell.IntegrationResult, error) {
	var need []string
	for _, b := range branches {
		// Contract: omit the key (or map it to nil) to request recompute; an
		// empty non-nil slice is a positive assertion of no changes.
		if ws := writeSets[b]; ws == nil {
			need = append(need, b)
		}
	}
	var computed map[string][]string
	if len(need) > 0 {
		var err error
		computed, err = g.DiffNameOnlyBatch(ctx, baseSHA, need)
		if err != nil {
			return cell.IntegrationResult{}, fmt.Errorf("batch write-sets: %w", err)
		}
	}

	changes := make([]cell.BranchChange, 0, len(branches))
	for _, b := range branches {
		paths := writeSets[b]
		if paths == nil {
			paths = computed[b]
		}
		changes = append(changes, cell.BranchChange{Branch: b, WriteSet: cell.NewWriteSet(paths...)})
	}

	in := cell.NewIntegrator(g)
	if land {
		in = in.WithLandRef("refs/heads/" + baseRef)
	}
	if assert {
		in = in.WithAssert()
	}
	if cmd := strings.TrimSpace(resolverCmd); cmd != "" {
		// Same shell-wrapped CommandResolver the integrate command uses, so the
		// SIGBOUND_BASE/SIGBOUND_OURS/SIGBOUND_THEIRS/SIGBOUND_PATH contract is identical.
		in = in.WithResolver(&cell.CommandResolver{
			Args:    []string{"sh", "-c", cmd},
			Timeout: resolverTimeout,
		})
	}
	return in.Integrate(ctx, baseSHA, changes, strategy)
}

// flaggedJSON is one branch the engine set aside for a human, with the paths
// that conflicted.
type flaggedJSON struct {
	Branch string   `json:"branch"`
	Paths  []string `json:"paths"`
}

// resultJSON is the integrate command's stdout contract.
type resultJSON struct {
	Repo        string        `json:"repo"`
	Base        string        `json:"base"`
	BaseSHA     string        `json:"baseSHA"`
	Strategy    string        `json:"strategy"`
	Groups      int           `json:"groups"`
	MaxParallel int           `json:"max-parallel"`
	Landed      []string      `json:"landed"`
	Flagged     []flaggedJSON `json:"flagged"`
	FinalSHA    string        `json:"finalSHA"`
	WallMs      int64         `json:"wall-ms"`
}

func runIntegrate(argv []string) error {
	fs := flag.NewFlagSet("integrate", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig integrate -repo PATH -base BRANCH -branches b1,b2,.. [-strategy overlay] [-assert] [-no-land]")
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "path to the target git repository")
	base := fs.String("base", "main", "base branch to land the branches onto")
	branchesCSV := fs.String("branches", "", "comma-separated agent branch names to integrate")
	strategy := fs.String("strategy", cell.StrategyOverlay, "integration strategy: "+strings.Join(cell.AvailableStrategies(), ", "))
	assert := fs.Bool("assert", false, "paranoid cross-check for -strategy overlay: independently recompute the combine via merge-tree and error (never land) on any tree mismatch. "+
		"Roughly doubles integration cost (it re-merges everything); for paranoia/CI, not routine use")
	noLand := fs.Bool("no-land", false, "integrate without moving the base ref (leave finalSHA detached)")
	resolverCmd := fs.String("resolver", "", "shell command (run via `sh -c`) invoked per conflicted path to resolve conflicts; "+
		"reads the SIGBOUND_BASE/SIGBOUND_OURS/SIGBOUND_THEIRS file paths + SIGBOUND_PATH env vars, writes the resolved body to stdout. "+
		"Empty stdout, non-zero exit, or timeout => branch stays flagged (fail-safe)")
	resolverTimeout := fs.Duration("resolver-timeout", 30*time.Second, "per-conflict timeout for -resolver (0 = none)")

	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return nil // -h already printed usage
		}
		return err
	}
	if *repo == "" {
		return fmt.Errorf("-repo is required")
	}
	branches := splitCSV(*branchesCSV)
	if len(branches) == 0 {
		return fmt.Errorf("-branches is required (comma-separated branch names)")
	}
	if err := validateStrategy(*strategy); err != nil {
		return err
	}

	ctx := context.Background()
	// Cheap preflight: git present + version >= 2.38, before touching the
	// repo. See runRun's identical check for why (the engine hard-depends on
	// merge-tree/overlay plumbing from git 2.38 onward); `sig doctor` has the
	// full live probe.
	if err := gitx.CheckMinVersion(ctx, "git"); err != nil {
		return err
	}
	g := gitx.New(*repo)

	// Resolve the base branch to a stable commit SHA so the merge-base is fixed
	// even as we advance the branch ref at the end.
	baseSHA, err := g.RevParse(ctx, *base)
	if err != nil {
		return fmt.Errorf("resolve base %q in %s: %w", *base, *repo, err)
	}

	// Hand the batch to the shared integrate path (partition, parallel folding,
	// optional resolver, and landing are entirely the cell's job).
	start := time.Now()
	res, err := integrateBranches(ctx, g, *base, baseSHA, branches, nil, *strategy, *resolverCmd, *resolverTimeout, *assert, !*noLand)
	if err != nil {
		return err
	}
	wall := time.Since(start)

	out := resultJSON{
		Repo:        *repo,
		Base:        *base,
		BaseSHA:     baseSHA,
		Strategy:    res.Strategy,
		Groups:      res.Groups,
		MaxParallel: res.MaxBatch,
		Landed:      res.Landed,
		Flagged:     make([]flaggedJSON, 0, len(res.Flagged)),
		FinalSHA:    res.FinalSHA,
		WallMs:      wall.Milliseconds(),
	}
	if out.Landed == nil {
		out.Landed = []string{}
	}
	for _, f := range res.Flagged {
		out.Flagged = append(out.Flagged, flaggedJSON{Branch: f.Branch, Paths: f.Conflicts})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validateStrategy(s string) error {
	if s == "occ" { // accepted alias for mergetree
		return nil
	}
	for _, v := range cell.AvailableStrategies() {
		if s == v {
			return nil
		}
	}
	return fmt.Errorf("unknown strategy %q (have %s)", s, strings.Join(cell.AvailableStrategies(), ", "))
}
