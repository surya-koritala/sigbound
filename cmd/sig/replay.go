// sig replay re-runs ONLY the integration+verify portion of a prior `sig run`
// from its manifest (see -manifest on `sig run`, or a plain -json report —
// same shape either way): given the recorded base SHA, the recorded
// strategy/resolver/verify commands, and every agent the original run
// recorded as having succeeded, it re-integrates the EXACT SAME commit SHAs
// (never a branch's current tip, which may have moved or been deleted since)
// and compares the recomputed tree to the one the original run recorded.
//
//	sig replay -manifest FILE
//
// Integration is already deterministic (Partition is order-stable,
// combineDisjoint is a fixed reduction — see cell), so this never lands and
// never moves any ref: it is a pure, read-only recomputation, feeding
// `sig integrate -no-land` the same recorded inputs.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/cell"
)

// sig replay's own exit codes — distinct from `sig run`'s (see the constants
// near runExitCode), since replay reports a different kind of outcome
// entirely: not "did the run succeed" but "does the repo still reproduce it".
const (
	exitReplayReproduced = 0 // the recomputed tree is byte-identical to the recorded one
	exitReplayDiverged   = 1 // both recomputed cleanly, but the trees differ (both OIDs are printed)
	exitReplayRepoState  = 2 // replay itself could not run: a bad manifest, a recorded SHA no longer resolvable, an integrate/checkout failure
)

// replayResolverTimeout is the per-conflict timeout replay gives the
// recorded resolver command. The manifest records the resolver COMMAND
// (ResolverCmd) but not its -resolver-timeout, so replay uses the same
// 30s default `sig run`/`sig integrate` themselves fall back to.
const replayResolverTimeout = 30 * time.Second

func runReplay(w io.Writer, argv []string) (int, error) {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig replay -manifest FILE")
		fs.PrintDefaults()
		fmt.Fprintln(fs.Output(), "\nexit codes:")
		fmt.Fprintln(fs.Output(), "  0  REPRODUCED: the recomputed tree matches the recorded one")
		fmt.Fprintln(fs.Output(), "  1  DIVERGED: both recomputed cleanly, but the trees differ")
		fmt.Fprintln(fs.Output(), "  2  replay could not run (bad manifest, a recorded SHA no longer exists, an integrate/checkout failure)")
	}
	manifestPath := fs.String("manifest", "", "path to a JSON report written by sig run's -manifest flag (or -json — same shape)")
	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitReplayReproduced, nil
		}
		return exitReplayRepoState, err
	}
	if strings.TrimSpace(*manifestPath) == "" {
		return exitReplayRepoState, fmt.Errorf("-manifest is required")
	}

	data, err := os.ReadFile(*manifestPath)
	if err != nil {
		return exitReplayRepoState, fmt.Errorf("read -manifest %s: %w", *manifestPath, err)
	}
	var rep runReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return exitReplayRepoState, fmt.Errorf("parse -manifest %s: %w", *manifestPath, err)
	}
	if strings.TrimSpace(rep.Repo) == "" || strings.TrimSpace(rep.BaseSHA) == "" {
		return exitReplayRepoState, fmt.Errorf("-manifest %s: missing repo/baseSHA — not a sig run report", *manifestPath)
	}
	// Integrate.Strategy is the strategy the original run actually applied;
	// fall back to the top-level Strategy (always recorded — see driveRun) for
	// a manifest whose integrate phase never got that far (e.g. it errored
	// before landing).
	strategy := rep.Integrate.Strategy
	if strategy == "" {
		strategy = rep.Strategy
	}
	if strategy == "" {
		return exitReplayRepoState, fmt.Errorf("-manifest %s: missing an integration strategy", *manifestPath)
	}
	if strings.TrimSpace(rep.Integrate.FinalSHA) == "" {
		return exitReplayRepoState, fmt.Errorf("-manifest %s: no recorded integrate.finalSHA to compare against", *manifestPath)
	}

	ctx := context.Background()
	// Open the cell for the recorded repo: it runs the same git preflight
	// (>= 2.38) and confirms the repo, then owns the git handle replay's
	// read-only re-integration and verify checkout run through.
	c, err := cell.Open(rep.Repo)
	if err != nil {
		return exitReplayRepoState, err
	}
	g := c.Git()

	// The recorded base SHA and every succeeded agent's recorded SHA must
	// still resolve to real objects in THIS repo. A branch can be deleted (and
	// its now-unreachable commit eventually garbage collected) after the run
	// that produced this manifest; replay needs the EXACT objects that run
	// integrated, never whatever a same-named branch happens to point at
	// today — so it checks SHAs directly, not branch names.
	if _, err := g.RevParse(ctx, rep.BaseSHA); err != nil {
		return exitReplayRepoState, fmt.Errorf("recorded baseSHA %s no longer resolves in %s (garbage collected?): %w", short(rep.BaseSHA), rep.Repo, err)
	}
	// A -verify-bisect run that salvaged a subset (see verifyBisect) records
	// integrate.finalSHA as the LANDED SUBSET's tree, not the full agent set —
	// the dropped groups' BRANCH NAMES (driveRun hands integrateBranches
	// a.Branch, e.g. "agent/g2", never a.SHA — see driveRun) are named in
	// DroppedByBisect. Re-integrating the full set would recompute a
	// different (larger) tree and falsely DIVERGE, so exclude them here the
	// same way the original run did before it ever reached verify.
	dropped := make(map[string]bool, len(rep.Integrate.DroppedByBisect))
	for _, branch := range rep.Integrate.DroppedByBisect {
		dropped[branch] = true
	}

	var branches []string
	writeSets := make(map[string][]string, len(rep.PerAgent))
	for _, a := range rep.PerAgent {
		// Mirrors driveRun's own candidate selection exactly: every agent the
		// original run recorded OK=true was handed to integrateBranches,
		// regardless of whether it ultimately landed or was flagged — a
		// flagged branch's SHA only replays correctly if it's included too.
		// EXCEPT a branch -verify-bisect dropped (see dropped above): the
		// recorded finalSHA never included it, so including it here would
		// reproduce a tree that was never actually landed.
		if !a.OK || dropped[a.Branch] {
			continue
		}
		if strings.TrimSpace(a.SHA) == "" {
			return exitReplayRepoState, fmt.Errorf("-manifest %s: agent %s recorded ok=true with no sha", *manifestPath, a.ID)
		}
		if _, err := g.RevParse(ctx, a.SHA); err != nil {
			return exitReplayRepoState, fmt.Errorf("agent %s's recorded sha %s no longer resolves in %s (garbage collected?): %w", a.ID, short(a.SHA), rep.Repo, err)
		}
		branches = append(branches, a.SHA)
		writeSets[a.SHA] = a.Files
	}

	// Re-integrate the exact recorded inputs. land=false: replay is read-only
	// and never moves any ref, matching `sig integrate -no-land`.
	res, err := integrateBranches(ctx, c, rep.Base, rep.BaseSHA, branches, writeSets, strategy, rep.ResolverCmd, replayResolverTimeout, false, false, nil)
	if err != nil {
		return exitReplayRepoState, fmt.Errorf("re-integrate: %w", err)
	}

	// Re-run the recorded -verify command against the recomputed tree, purely
	// as an extra confirmation printed alongside the result — REPRODUCED vs.
	// DIVERGED below is decided by the tree OID comparison alone. Integration
	// is deterministic by construction; -verify is only guaranteed
	// deterministic BY CONVENTION (see docs/USAGE.md's Determinism section),
	// so a verify command that behaves differently on replay is informative,
	// not itself a repo-state error.
	if strings.TrimSpace(rep.VerifyCmd) != "" {
		dir, derr := os.MkdirTemp("", "sig-replay-verify-*")
		if derr != nil {
			return exitReplayRepoState, fmt.Errorf("verify worktree: %w", derr)
		}
		defer os.RemoveAll(dir)
		wtPath := filepath.Join(dir, "wt")
		if werr := g.WorktreeAddDetached(ctx, wtPath, res.FinalSHA); werr != nil {
			return exitReplayRepoState, fmt.Errorf("verify checkout %s: %w", short(res.FinalSHA), werr)
		}
		defer func() { _ = g.WorktreeRemove(ctx, wtPath) }()
		v := runVerify(ctx, g, wtPath, runParams{VerifyCmd: rep.VerifyCmd}, nil, "", 0)
		status := "PASS"
		if !v.OK {
			status = "FAIL"
		}
		fmt.Fprintf(w, "verify (replayed): %s\n", status)
	}

	gotTree, err := g.TreeOID(ctx, res.FinalSHA)
	if err != nil {
		return exitReplayRepoState, fmt.Errorf("tree of recomputed %s: %w", short(res.FinalSHA), err)
	}
	wantTree, err := g.TreeOID(ctx, rep.Integrate.FinalSHA)
	if err != nil {
		return exitReplayRepoState, fmt.Errorf("tree of recorded finalSHA %s (garbage collected?): %w", short(rep.Integrate.FinalSHA), err)
	}
	if gotTree == wantTree {
		fmt.Fprintf(w, "REPRODUCED tree=%s\n", gotTree)
		return exitReplayReproduced, nil
	}
	fmt.Fprintf(w, "DIVERGED recorded=%s recomputed=%s\n", wantTree, gotTree)
	return exitReplayDiverged, nil
}
