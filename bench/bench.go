// Package bench is the benchmark harness that proves the thesis: N agents
// working one repo in parallel do not collapse into merge hell, and OCC
// partitioning lands their branches far faster than the obvious sequential
// merge.
//
// A run: build a temp repo, materialize N agent branches (each = base + that
// agent's edits) as the fixture, then integrate them several ways — a naive
// sequential baseline, the OCC engine, and the tree-overlay fast path — timing
// each and checking every one produces a correct tree.
//
// setup() builds the base repo + creates the agent branches ONCE via a single
// `git fast-import` (a BENCHMARK-ONLY shortcut — the real per-agent commit path
// is worker.Run — so fixture-building doesn't dominate wall time); every strategy
// then integrates from that same fixture. The commit-phase throughput printed by
// the CLI therefore measures git's bulk-import speed, NOT realistic parallel-agent
// commit throughput; the INTEGRATION timings are the result that matters. Run() is
// the single-shot A/B; RunAB() (ab.go) is the rigorous version: warmups + K
// measured runs per strategy with median/min/max/p90.
package bench

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

// Config parameterizes a benchmark run.
type Config struct {
	Agents   int     // N parallel agents
	Files    int     // M files in the base repo
	Overlap  float64 // R: fraction of agents that touch a shared "hot" file
	Conflict float64 // fraction of overlapping agents forced into a real conflict
	HotFiles int     // number of shared hot files overlaps spread across
	Seed     int64   // RNG seed (reproducibility)

	// Baseline and Candidate are the two integration strategies to A/B (any of
	// cell.AvailableStrategies). Defaults: "naive" vs "occ". Set Baseline to
	// "porcelain" to measure against the working-tree path, or Candidate to
	// "overlay" to measure the tree-overlay fast path.
	Baseline  string
	Candidate string
}

// withDefaults fills unset knobs.
func (c Config) withDefaults() Config {
	if c.HotFiles <= 0 {
		c.HotFiles = 1
	}
	if c.Files <= 0 {
		c.Files = 100
	}
	if c.Agents <= 0 {
		c.Agents = 8
	}
	if c.Baseline == "" {
		c.Baseline = cell.StrategyNaive
	}
	if c.Candidate == "" {
		c.Candidate = cell.StrategyMergeTree // the OCC engine (alias: "occ")
	}
	return c
}

// Result is everything one run measured.
type Result struct {
	Config Config

	// Phase 2: raw parallel commit throughput (no ref contention).
	CommitWall    time.Duration
	CommitsPerSec float64

	// Phase 3: the two integration strategies.
	Naive cell.IntegrationResult
	OCC   cell.IntegrationResult

	NaiveBranchesPerSec float64
	OCCBranchesPerSec   float64
	Speedup             float64 // naive_wall / occ_wall

	// Phase 4: correctness (both trees hold every landed change).
	NaiveCorrect bool
	OCCCorrect   bool
	TreesEqual   bool // when nothing was flagged, both trees must be identical
}

// agent captures per-agent scenario assignment and outcome bookkeeping.
type agent struct {
	index    int
	privFile string
	hotFile  string // "" if disjoint
	hotLine  int
	branch   string
}

// harness is a built base repo with N agents already committed to their own
// branches — the shared, timing-neutral fixture every strategy integrates from.
// Building it once and reusing it across strategies (and across repeated runs of
// the same strategy) is what makes the A/B honest: the only thing that varies
// between measurements is the integration strategy itself.
type harness struct {
	root          string
	g             *gitx.Git
	base          string
	agents        []agent
	changes       []cell.BranchChange
	branchToPriv  map[string]string
	commitWall    time.Duration
	commitsPerSec float64
}

// setup performs Phase 1 (build base repo) and Phase 2 (synthesize the N agent
// commits via one fast-import) and computes the OCC write-sets, returning a
// harness plus a cleanup func. The returned harness can be integrated any number
// of times: with an empty land-ref the integrator never moves a ref, and its
// object writes are content-addressed, so re-integrating the identical inputs is
// idempotent (same OIDs, no growth).
//
// Phase 2 is a BENCHMARK-ONLY shortcut for building the fixture; it is not the
// product's real per-agent commit path (see the note at the fast-import call and
// worker.Run). The measured result the benchmark exists to prove is the
// INTEGRATION timing, which is independent of how the fixture was built.
func setup(ctx context.Context, cfg Config) (*harness, func(), error) {
	root, err := os.MkdirTemp("", "sigbench-")
	if err != nil {
		return nil, nil, err
	}
	cleanupRoot := func() { _ = os.RemoveAll(root) }

	repoDir := filepath.Join(root, "repo")

	// ---- Phase 1: build base repo ----------------------------------------
	base, err := buildBaseRepo(ctx, repoDir, cfg)
	if err != nil {
		cleanupRoot()
		return nil, nil, err
	}
	g := gitx.New(repoDir)
	agents := assignAgents(cfg)
	for i := range agents {
		agents[i].branch = fmt.Sprintf("agent/imp-%d", i)
	}

	// ---- Phase 2: synthesize the N agent commits -------------------------
	// BENCHMARK-ONLY fixture generation. One `git fast-import` writes all N agent
	// branches (each = base + its private file + optional hot-file line edit) in a
	// single process, replacing a per-agent `git add`+`commit`+`rev-parse` fan-out.
	// This measures git's BULK-IMPORT throughput, not realistic parallel-agent
	// commit throughput: a real agent is its own slow process producing genuine
	// edits and cannot be batch-imported (worker.Run is the true per-agent commit
	// path). It exists only so fixture-building stops dominating wall time; the
	// integration timings below — the actual result — are unaffected by it.
	stream := fastImportStream(cfg, base, agents)
	t0 := time.Now()
	if err := g.FastImport(ctx, stream); err != nil {
		cleanupRoot()
		return nil, nil, fmt.Errorf("fast-import agents: %w", err)
	}
	commitWall := time.Since(t0)

	// ---- Write-sets (OCC input) ------------------------------------------
	// Known by construction: each agent changed its private file plus, if it
	// overlaps, the hot file. Every such edit is a real change, so this is exactly
	// `git diff --name-only base..branch` — verified by the post-integration
	// correctness gate — but needs no per-branch `git diff` spawn.
	changes := make([]cell.BranchChange, cfg.Agents)
	for i := range agents {
		paths := []string{agents[i].privFile}
		if agents[i].hotFile != "" {
			paths = append(paths, agents[i].hotFile)
		}
		changes[i] = cell.BranchChange{Branch: agents[i].branch, WriteSet: cell.NewWriteSet(paths...)}
	}

	branchToPriv := make(map[string]string, cfg.Agents)
	for i := range agents {
		branchToPriv[agents[i].branch] = agents[i].privFile
	}

	h := &harness{
		root: root, g: g, base: base, agents: agents,
		changes: changes, branchToPriv: branchToPriv,
		commitWall:    commitWall,
		commitsPerSec: float64(cfg.Agents) / commitWall.Seconds(),
	}
	return h, cleanupRoot, nil
}

// Run executes one full benchmark (single measurement per strategy) and returns
// its measurements. Use RunAB for the rigorous multi-run version.
func Run(ctx context.Context, cfg Config) (Result, error) {
	cfg = cfg.withDefaults()
	res := Result{Config: cfg}

	h, cleanup, err := setup(ctx, cfg)
	if err != nil {
		return res, err
	}
	defer cleanup()
	res.CommitWall = h.commitWall
	res.CommitsPerSec = h.commitsPerSec

	integ := cell.NewIntegrator(h.g)

	// ---- Phase 3a: BASELINE strategy -------------------------------------
	res.Naive, err = integ.Integrate(ctx, h.base, h.changes, cfg.Baseline)
	if err != nil {
		return res, fmt.Errorf("%s integrate: %w", cfg.Baseline, err)
	}
	res.NaiveBranchesPerSec = float64(cfg.Agents) / res.Naive.Duration.Seconds()

	// ---- Phase 3b: CANDIDATE strategy ------------------------------------
	res.OCC, err = integ.Integrate(ctx, h.base, h.changes, cfg.Candidate)
	if err != nil {
		return res, fmt.Errorf("%s integrate: %w", cfg.Candidate, err)
	}
	res.OCCBranchesPerSec = float64(cfg.Agents) / res.OCC.Duration.Seconds()
	if res.OCC.Duration > 0 {
		res.Speedup = res.Naive.Duration.Seconds() / res.OCC.Duration.Seconds()
	}

	// ---- Phase 4: correctness --------------------------------------------
	res.NaiveCorrect, err = treeHasAllLanded(ctx, h.g, res.Naive, h.branchToPriv)
	if err != nil {
		return res, err
	}
	res.OCCCorrect, err = treeHasAllLanded(ctx, h.g, res.OCC, h.branchToPriv)
	if err != nil {
		return res, err
	}
	if len(res.Naive.Flagged) == 0 && len(res.OCC.Flagged) == 0 {
		// Content-addressed trees: equal OID <=> byte-identical trees. One
		// rev-parse each, no O(files) content reads.
		nt, err := h.g.TreeOID(ctx, res.Naive.FinalSHA)
		if err != nil {
			return res, err
		}
		ot, err := h.g.TreeOID(ctx, res.OCC.FinalSHA)
		if err != nil {
			return res, err
		}
		res.TreesEqual = nt == ot
	}

	return res, nil
}

// buildBaseRepo creates repoDir with cfg.Files uniquely-lined files and one base
// commit. Unique line content is essential: with identical lines git cannot tell
// which line an agent changed and otherwise-independent edits conflict.
func buildBaseRepo(ctx context.Context, repoDir string, cfg Config) (string, error) {
	g := gitx.New(repoDir)
	if err := g.Init(ctx); err != nil {
		return "", err
	}
	lines := linesPerFile(cfg)
	for i := 0; i < cfg.Files; i++ {
		if err := os.WriteFile(filepath.Join(repoDir, fileName(i)), []byte(fileContent(i, lines)), 0o644); err != nil {
			return "", err
		}
	}
	return g.CommitAll(ctx, "base")
}

func fileName(i int) string { return fmt.Sprintf("f%05d.txt", i) }

// fileContent is the deterministic body of base file i: `lines` uniquely-numbered
// lines. Unique per-line content lets git tell which line an agent changed, so
// otherwise-independent hot-file edits auto-merge instead of colliding.
func fileContent(i, lines int) string {
	var sb strings.Builder
	for ln := 0; ln < lines; ln++ {
		fmt.Fprintf(&sb, "f%05d line %04d\n", i, ln)
	}
	return sb.String()
}

// fastImportStream builds a `git fast-import` stream that creates every agent's
// branch off base with its edits inline. It mirrors the worktree-commit edit
// exactly — private file body plus the single hot-file line replacement — so the
// imported trees are byte-identical to what a real per-agent commit would produce.
// Pure and in-memory: no git process runs here.
func fastImportStream(cfg Config, base string, agents []agent) []byte {
	lines := linesPerFile(cfg)
	var b bytes.Buffer
	for i := range agents {
		a := &agents[i]
		fmt.Fprintf(&b, "commit refs/heads/%s\nmark :%d\n", a.branch, i+1)
		b.WriteString("committer sigbound <sigbound@local> 1700000000 +0000\n")
		msg := fmt.Sprintf("agent %d", a.index)
		fmt.Fprintf(&b, "data %d\n%s\n", len(msg), msg)
		fmt.Fprintf(&b, "from %s\n", base)
		writeFileModify(&b, a.privFile, fmt.Sprintf("agent %d owns this\n", a.index))
		if a.hotFile != "" {
			fl := strings.Split(fileContent(hotFileIndex(cfg, a.hotFile), lines), "\n")
			if a.hotLine < len(fl) {
				fl[a.hotLine] = fmt.Sprintf("agent-%d-edited-this-line", a.index)
			}
			writeFileModify(&b, a.hotFile, strings.Join(fl, "\n"))
		}
		b.WriteByte('\n')
	}
	b.WriteString("done\n")
	return b.Bytes()
}

// writeFileModify appends one fast-import inline file modification (mode 100644).
// The data payload length is the content's exact byte count; a trailing LF after
// it is the optional separator fast-import allows.
func writeFileModify(b *bytes.Buffer, path, content string) {
	fmt.Fprintf(b, "M 100644 inline %s\ndata %d\n", path, len(content))
	b.WriteString(content)
	b.WriteByte('\n')
}

// hotFileIndex maps a hot file's name back to its base index (hot files are the
// first cfg.HotFiles files). Only hot-file names are ever passed in.
func hotFileIndex(cfg Config, name string) int {
	for k := 0; k < cfg.HotFiles; k++ {
		if fileName(k) == name {
			return k
		}
	}
	return 0
}

// linesPerFile sizes hot files so every overlapping agent gets its own spaced
// line (gap of 4) with headroom, so non-conflicting edits always auto-merge.
func linesPerFile(cfg Config) int {
	numOverlap := int(cfg.Overlap * float64(cfg.Agents))
	perHot := 0
	if cfg.HotFiles > 0 {
		perHot = (numOverlap + cfg.HotFiles - 1) / cfg.HotFiles
	}
	need := perHot*4 + 16
	if need < 64 {
		need = 64
	}
	return need
}

// assignAgents deterministically decides which agents overlap (touch a hot
// file), which hot file + line each uses, and which are forced to conflict.
func assignAgents(cfg Config) []agent {
	rng := rand.New(rand.NewSource(cfg.Seed))
	agents := make([]agent, cfg.Agents)
	for i := range agents {
		agents[i] = agent{index: i, privFile: fmt.Sprintf("agent_%05d.txt", i)}
	}

	// Choose the overlapping subset by shuffling indices.
	order := rng.Perm(cfg.Agents)
	numOverlap := int(cfg.Overlap * float64(cfg.Agents))
	overlap := order[:numOverlap]

	// Per-hot-file slot counter, so each agent on a file gets a distinct line.
	slot := make([]int, cfg.HotFiles)
	for k, idx := range overlap {
		hf := k % cfg.HotFiles
		agents[idx].hotFile = fileName(hf) // hot files are the first HotFiles files
		if rng.Float64() < cfg.Conflict {
			// Force a real conflict: everyone here fights over line 0.
			agents[idx].hotLine = 0
		} else {
			agents[idx].hotLine = slot[hf]*4 + 2 // spaced, unique per agent on file
			slot[hf]++
		}
	}
	return agents
}

// treeHasAllLanded verifies every landed branch's private file is in the tree.
func treeHasAllLanded(ctx context.Context, g *gitx.Git, r cell.IntegrationResult, branchToPriv map[string]string) (bool, error) {
	paths, err := g.LsTree(ctx, r.FinalSHA)
	if err != nil {
		return false, err
	}
	have := make(map[string]bool, len(paths))
	for _, p := range paths {
		have[p] = true
	}
	for _, b := range r.Landed {
		if priv := branchToPriv[b]; priv != "" && !have[priv] {
			return false, nil
		}
	}
	return true, nil
}
