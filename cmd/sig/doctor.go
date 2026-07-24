// sig doctor probes the git binary and the exact plumbing the engine
// hard-depends on — `git merge-tree --write-tree -z --name-only
// --merge-base=` (gitx.MergeTree) and the overlay index plumbing
// (gitx.OverlayTrees) — so a version or capability gap surfaces as one clear
// "requires git >= 2.38" line instead of a cryptic mid-run "merge-tree exit
// N". `sig run` and `sig integrate` also run the cheap part of this
// (gitx.CheckMinVersion) implicitly before doing anything; `sig doctor` is
// the full picture, including a live probe that actually exercises the
// invocations rather than trusting the version string.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// emptySHA1Tree is git's well-known empty-tree object ID. It is valid input
// to `read-tree`/`ls-tree`/etc in any SHA-1 repository without needing to
// exist in that repository's object store, so it's the seed the live probe
// builds its synthetic commits from without ever touching a worktree, a
// branch, or a ref.
const emptySHA1Tree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// doctorCheck is one probe: a human-readable name and a function that
// returns nil on success or an actionable error on failure.
type doctorCheck struct {
	name string
	run  func(ctx context.Context) error
}

func runDoctor(w io.Writer, argv []string) (int, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig doctor [-repo PATH]")
		fmt.Fprintln(fs.Output(), "probes git and the merge-tree/overlay plumbing sigbound depends on;")
		fmt.Fprintln(fs.Output(), "exits 0 if every check passes, 1 if any fails")
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "run the live probe inside this existing repo instead of a throwaway temp repo")
	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitOK, nil
		}
		return exitOperationalError, err
	}

	checks := []doctorCheck{
		{"git on PATH", checkGitPresent},
		{"git version >= 2.38", func(ctx context.Context) error { return gitx.CheckMinVersion(ctx, "git") }},
		{"live probe: merge-tree + overlay plumbing", func(ctx context.Context) error { return liveProbe(ctx, *repo) }},
	}

	ctx := context.Background()
	allOK := true
	for _, c := range checks {
		if err := c.run(ctx); err != nil {
			allOK = false
			fmt.Fprintf(w, "%s: FAIL: %v\n", c.name, err)
			continue
		}
		fmt.Fprintf(w, "%s: ok\n", c.name)
	}
	// Informational only (see diskInfoLine's doc comment): a disk-space
	// estimate that can't be formed never fails doctor, so this runs
	// unconditionally and never touches allOK.
	fmt.Fprintln(w, diskInfoLine(ctx, *repo))
	// Same posture as the disk line above: a debris count that can't be
	// formed never fails doctor (see gcInfoLine's doc comment).
	fmt.Fprintln(w, gcInfoLine(ctx, *repo))
	if !allOK {
		return exitOperationalError, nil
	}
	return exitOK, nil
}

// checkGitPresent verifies the git binary can actually be run and its
// output looks like a version string, independent of what that version is
// (that's the next check).
func checkGitPresent(ctx context.Context) error {
	if _, _, _, err := gitx.GitVersion(ctx, "git"); err != nil {
		return err
	}
	return nil
}

// liveProbe exercises the exact plumbing MergeTree and OverlayTrees wrap,
// against real synthetic history, rather than trusting the version string.
// It builds two tiny commits (ours, theirs) that each touch a different file
// off a common base — entirely via plumbing (hash-object/write-tree/
// commit-tree), never a worktree, branch, or ref — so running it against an
// existing -repo never mutates anything the caller can see; the only trace
// left behind is a few unreferenced ("dangling") objects, same as any other
// git plumbing tool, and harmless.
//
// With no -repo, it does the same thing inside a throwaway temp repo instead.
func liveProbe(ctx context.Context, repo string) error {
	dir := repo
	if dir == "" {
		tmp, err := os.MkdirTemp("", "sig-doctor-*")
		if err != nil {
			return fmt.Errorf("create throwaway temp repo: %w", err)
		}
		defer os.RemoveAll(tmp)
		if err := gitx.New(tmp).Init(ctx); err != nil {
			return fmt.Errorf("init throwaway temp repo: %w", err)
		}
		dir = tmp
	}
	g := gitx.New(dir)

	baseTree, err := g.SpliceBlobs(ctx, emptySHA1Tree, []gitx.ResolvedBlob{
		{Path: "SIGBOUND-DOCTOR-PROBE.txt", Content: "base\n"},
	})
	if err != nil {
		return fmt.Errorf("build synthetic base tree: %w", err)
	}
	baseCommit, err := g.CommitTree(ctx, baseTree, nil, "sig doctor: synthetic base")
	if err != nil {
		return fmt.Errorf("commit synthetic base: %w", err)
	}

	oursTree, err := g.SpliceBlobs(ctx, baseTree, []gitx.ResolvedBlob{
		{Path: "SIGBOUND-DOCTOR-PROBE.txt", Content: "base\nours\n"}, // modifies the base file
	})
	if err != nil {
		return fmt.Errorf("build synthetic ours tree: %w", err)
	}
	oursCommit, err := g.CommitTree(ctx, oursTree, []string{baseCommit}, "sig doctor: synthetic ours")
	if err != nil {
		return fmt.Errorf("commit synthetic ours: %w", err)
	}

	theirsTree, err := g.SpliceBlobs(ctx, baseTree, []gitx.ResolvedBlob{
		{Path: "SIGBOUND-DOCTOR-PROBE-2.txt", Content: "theirs\n"}, // disjoint new file
	})
	if err != nil {
		return fmt.Errorf("build synthetic theirs tree: %w", err)
	}
	theirsCommit, err := g.CommitTree(ctx, theirsTree, []string{baseCommit}, "sig doctor: synthetic theirs")
	if err != nil {
		return fmt.Errorf("commit synthetic theirs: %w", err)
	}

	// The exact invocation gitx.MergeTree wraps: `git merge-tree --write-tree
	// -z --name-only --merge-base=<base> <ours> <theirs>`.
	mt, err := g.MergeTree(ctx, baseCommit, oursCommit, theirsCommit)
	if err != nil {
		return fmt.Errorf("git merge-tree --write-tree -z --name-only --merge-base=... failed "+
			"(requires git >= 2.38): %w", err)
	}
	if !mt.OK {
		return fmt.Errorf("git merge-tree unexpectedly conflicted on disjoint synthetic changes: %v", mt.Conflicts)
	}

	// The overlay index plumbing: read-tree / diff-tree --stdin / update-index
	// --index-info / write-tree.
	overlay, err := g.OverlayTrees(ctx, baseCommit, []string{oursCommit, theirsCommit})
	if err != nil {
		return fmt.Errorf("overlay index plumbing (read-tree/update-index/write-tree) failed: %w", err)
	}
	if overlay != mt.Tree {
		return fmt.Errorf("overlay tree %s != merge-tree %s on disjoint synthetic changes", overlay, mt.Tree)
	}
	return nil
}
