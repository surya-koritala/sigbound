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
	"errors"
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
	case "ack":
		code, err := runAck(os.Stdout, os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "sig ack:", err)
		}
		if code != 0 {
			os.Exit(code)
		}
	case "reject":
		code, err := runReject(os.Stdout, os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "sig reject:", err)
		}
		if code != 0 {
			os.Exit(code)
		}
	case "gc":
		code, err := runGC(os.Stdout, os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "sig gc:", err)
		}
		if code != 0 {
			os.Exit(code)
		}
	case "log":
		code, err := runLog(os.Stdout, os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "sig log:", err)
		}
		if code != 0 {
			os.Exit(code)
		}
	case "intent":
		code, err := runIntent(os.Stdout, os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "sig intent:", err)
		}
		if code != 0 {
			os.Exit(code)
		}
	case "replay":
		code, err := runReplay(os.Stdout, os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "sig replay:", err)
		}
		if code != 0 {
			os.Exit(code)
		}
	case "serve":
		code, err := runServe(os.Stdout, os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "sig serve:", err)
		}
		if code != 0 {
			os.Exit(code)
		}
	case "export":
		if err := runExport(os.Stdout, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "sig export:", err)
			os.Exit(1)
		}
	case "import":
		if err := runImport(os.Stdout, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "sig import:", err)
			os.Exit(1)
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
	fmt.Fprintln(w, "  sig integrate -repo PATH -base BRANCH -branches b1,b2,..  (see 'sig integrate -h' for all flags)")
	fmt.Fprintln(w, "  sig run       -repo PATH -base BRANCH (-tasks FILE | -goal STRING) -agent CMD  (see 'sig run -h' for all flags)")
	fmt.Fprintln(w, "  sig replay    -manifest FILE  (deterministically re-integrate a prior run's recorded inputs; see 'sig replay -h')")
	fmt.Fprintln(w, "  sig export    -repo PATH -bundle FILE -branches b1,b2,..  (bundle branches for a coordinator to import)")
	fmt.Fprintln(w, "  sig import    -repo PATH -bundle FILE [-from WORKER_ID]  (import a bundle under imported/<worker>/; then 'sig integrate' it)")
	fmt.Fprintln(w, "  sig serve     -repos PATH[,PATH..] [-addr HOST:PORT]  (HTTP run API over driveRun; see 'sig serve -h')")
	fmt.Fprintln(w, "  sig ack       RUN_ID -repo PATH  (release a parked landing: lands the exact verified tree; see 'sig ack -h')")
	fmt.Fprintln(w, "  sig reject    RUN_ID -repo PATH [-reason TEXT]  (reject a parked landing; branches kept, nothing lands)")
	fmt.Fprintln(w, "  sig doctor    [-repo PATH]")
	fmt.Fprintln(w, "  sig gc        -repo PATH [-older-than 72h] [-delete] [-force] [-json]  (sweep debris a crashed run left; dry-run by default; see 'sig gc -h')")
	fmt.Fprintln(w, "  sig log       -repo PATH [-limit 50] [-sha COMMIT | -task ID] [-json]  (read-only run history + commit provenance; see 'sig log -h')")
	fmt.Fprintln(w, "  sig intent    list|show ID|import-github -repo PATH  (the repo's intents/*.intent statements of work; run one with 'sig run -intent ID')")
	fmt.Fprintln(w, "  sig version")
	fmt.Fprintln(w, "strategies:", strings.Join(cell.AvailableStrategies(), ", "))
}

// integrateBranches computes each branch's write-set versus baseSHA and hands the
// batch to the cell's integrator — the ONE integration code path in this binary.
// `sig integrate`, `sig run`, and `sig replay` all call it through a cell.Cell,
// so the driver never reimplements partition / parallel folding / conflict
// handling / landing; it only supplies the branch names and the same
// resolver/strategy/assert knobs. When land is true the base branch ref is
// advanced to the integrated commit.
//
// writeSets carries any ALREADY-COMPUTED write-sets (branch -> paths), e.g.
// `sig run`'s runAgent already ran `git diff` per agent for lane enforcement —
// reusing that here avoids re-diffing the same branch. A nil map, or a branch
// missing from it (or mapped to nil), is not treated as "no changes"; its
// write-set is computed here instead, for every such branch in ONE batched
// diff-tree call rather than a `git diff --name-only` fork per branch (see
// gitx.DiffNameOnlyBatch). `sig integrate` has no precomputed data, so it
// always passes nil and every branch goes through the batched path.
//
// resolverEnv is forwarded to cell.CommandResolver.Env as-is: nil keeps that
// type's own default (the full os.Environ(), today's behavior), non-nil is a
// caller-scoped base environment (see `sig run`'s -env-mode). `sig integrate`
// and `sig replay` always pass nil — -env-mode is a `sig run`-only flag.
//
// semanticEdges are extra cross-branch grouping edges from -semantic go (see
// computeSemanticEdges), fed straight into cell.WithSemanticEdges so
// PartitionSemantic unions them on top of path overlap. `sig integrate` and
// `sig replay` always pass nil — -semantic is a `sig run`-only flag.
//
// Contract: every branch must contain baseSHA (or BE it). A branch that
// doesn't refuses the whole batch before anything integrates — see the
// ancestry guard below for WHY that branch would silently delete landed work.
func integrateBranches(ctx context.Context, c *cell.Cell, baseRef, baseSHA string, branches []string, writeSets map[string][]string, strategy, resolverCmd string, resolverTimeout time.Duration, assert, land bool, resolverEnv []string, semanticEdges [][2]string) (cell.IntegrationResult, error) {
	// Ancestry guard (issue #130): the overlay strategy takes a branch's
	// contribution to be the TWO-tree diff baseSHA→tip, so a branch that does
	// not contain baseSHA carries, as its "contribution", the REMOVAL of
	// everything the base gained since that branch forked — and the OCC
	// partitioner cannot catch it, because such a branch's base...tip write-set
	// is empty. Every branch must therefore contain the base before anything
	// reaches the integrator; the invariant is adoptableAgainst's (the same one
	// `sig serve -watch` adoption enforces — see adoptBranch), fails CLOSED on
	// any error, and one refused branch refuses the whole batch: nothing lands.
	//
	// The guard binds EVERY strategy, not just the one that motivates it.
	// porcelain would in fact merge a diverged branch correctly, since a real
	// `git merge` uses the merge base rather than a two-tree diff — but the
	// strategies are documented as equivalent in what they land (see
	// cell/integrate.go), and a stale branch is the one input that made them
	// disagree. Refusing it everywhere restores that equivalence; the cost is
	// that porcelain loses an ability the others never had.
	g := c.Git()
	for _, b := range branches {
		head, err := g.RevParse(ctx, b)
		if err != nil {
			return cell.IntegrationResult{}, fmt.Errorf("resolve branch %q: %w", b, err)
		}
		if head == baseSHA {
			continue // IS the base: contains it trivially, empty contribution
		}
		switch verdict, err := adoptableAgainst(ctx, g, baseSHA, head); {
		case err != nil:
			return cell.IntegrationResult{}, fmt.Errorf("branch %q: cannot determine whether it contains base %s; refusing to integrate: %w", b, short(baseSHA), err)
		case verdict != adoptOK:
			return cell.IntegrationResult{}, fmt.Errorf("branch %q does not contain base %s: integrating it would delete everything the base gained since the branch forked. "+
				"Rebase the branch onto %s (or re-fork it from the current base) and retry. "+
				"Branches from `sig import` (imported/<worker>/*) routinely predate the coordinator's base and need the same rebase. Nothing was landed", b, short(baseSHA), short(baseSHA))
		}
	}

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
		computed, err = c.Git().DiffNameOnlyBatch(ctx, baseSHA, need)
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

	var opts []func(*cell.Integrator)
	if land {
		opts = append(opts, func(in *cell.Integrator) { in.WithLandRef("refs/heads/" + baseRef) })
	}
	if assert {
		opts = append(opts, func(in *cell.Integrator) { in.WithAssert() })
	}
	if len(semanticEdges) > 0 {
		opts = append(opts, func(in *cell.Integrator) { in.WithSemanticEdges(semanticEdges) })
	}
	if cmd := strings.TrimSpace(resolverCmd); cmd != "" {
		// Same shell-wrapped CommandResolver the integrate command uses, so the
		// SIGBOUND_BASE/SIGBOUND_OURS/SIGBOUND_THEIRS/SIGBOUND_PATH contract is identical.
		r := &cell.CommandResolver{
			Args:    []string{"sh", "-c", cmd},
			Timeout: resolverTimeout,
			Env:     resolverEnv,
		}
		opts = append(opts, func(in *cell.Integrator) { in.WithResolver(r) })
	}
	return c.Integrate(ctx, baseSHA, changes, strategy, opts...)
}

// flaggedJSON is one branch the engine set aside for a human, with the paths
// that conflicted. Reason is set only for a landing-policy hold (an ack-path
// change or a self-modification of sigbound.policy — see policyHoldback):
// empty, and so omitted from JSON, for an ordinary merge conflict, keeping a
// conflict-only run's report byte-identical to before the policy feature.
type flaggedJSON struct {
	Branch string   `json:"branch"`
	Paths  []string `json:"paths"`
	Reason string   `json:"reason,omitempty"`
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
	// Open the cell for this repo: it runs the same cheap preflight (git present
	// + version >= 2.38, the merge-tree/overlay plumbing the engine hard-depends
	// on from 2.38 onward — `sig doctor` has the full live probe), confirms the
	// repo, and owns the git handle the integration runs through.
	c, err := cell.Open(*repo)
	if err != nil {
		return err
	}

	// Resolve the base branch to a stable commit SHA so the merge-base is fixed
	// even as we advance the branch ref at the end.
	baseSHA, err := c.Git().RevParse(ctx, *base)
	if err != nil {
		return fmt.Errorf("resolve base %q in %s: %w", *base, *repo, err)
	}

	// Hand the batch to the shared integrate path (partition, parallel folding,
	// optional resolver, and landing are entirely the cell's job).
	start := time.Now()
	res, err := integrateBranches(ctx, c, *base, baseSHA, branches, nil, *strategy, *resolverCmd, *resolverTimeout, *assert, !*noLand, nil, nil)
	if err != nil {
		// A refused landing swap (issue #138) is not a broken integration: the
		// batch folded fine, somebody else simply advanced the base while it ran —
		// a window as long as the fold plus any -resolver takes. Nothing landed,
		// and the recovery is to integrate again against the head that won, so say
		// that rather than let it read as a plumbing failure. The exit is non-zero
		// either way, and no JSON is printed: there is no landing to report.
		if errors.Is(err, gitx.ErrRefMoved) {
			moved, _ := c.Git().RevParse(ctx, *base)
			return fmt.Errorf("the base %q moved to %s while this integration was computing against %s; nothing landed — re-run against the new head: %w",
				*base, shortMoved(moved), short(baseSHA), err)
		}
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
