// Package gitx is a thin, injection-safe wrapper over the system git binary.
//
// Design rules (see project spec):
//   - We build ON TOP of git; we never reimplement it and never use go-git.
//   - Every git invocation goes through exec with an explicit argv slice — no
//     shell, so repo paths / branch names can never be interpreted as options
//     or shell metacharacters.
//   - The environment is pinned (fixed identity, no system/global config, gpg
//     signing off) so the same operations are deterministic across machines and
//     inside a benchmark.
package gitx

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Git targets a single working directory (the main repo or one worktree). All
// commands run with `-C dir`, so one Git value maps to one directory; use
// At(dir) to get a sibling value pointing at a worktree that shares the store.
type Git struct {
	bin string
	dir string
}

// New returns a Git bound to dir, using the "git" binary from PATH.
func New(dir string) *Git { return &Git{bin: "git", dir: dir} }

// WithBinary overrides the git executable (mainly for tests / custom installs).
func (g *Git) WithBinary(bin string) *Git { return &Git{bin: bin, dir: g.dir} }

// At returns a Git pointing at a different directory (e.g. a worktree) but the
// same binary. The two share the underlying object store on disk.
func (g *Git) At(dir string) *Git { return &Git{bin: g.bin, dir: dir} }

// Dir reports the directory this Git operates in.
func (g *Git) Dir() string { return g.dir }

// runRaw executes git with a pinned, hermetic environment. It returns stdout,
// stderr, and the process exit code. A non-zero exit is NOT an error here (some
// git commands use exit codes as signal, e.g. merge-tree returns 1 on conflict);
// err is non-nil only when the process could not run at all. It delegates to
// runWith (see plumbing.go), which also supports an alternate index and stdin.
func (g *Git) runRaw(ctx context.Context, args ...string) (stdout, stderr string, code int, err error) {
	return g.runWith(ctx, nil, nil, args...)
}

// run executes git and returns trimmed stdout, treating any non-zero exit as an
// error carrying stderr.
func (g *Git) run(ctx context.Context, args ...string) (string, error) {
	stdout, stderr, code, err := g.runRaw(ctx, args...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return strings.TrimRight(stdout, "\n"),
			fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), code, strings.TrimSpace(stderr))
	}
	return strings.TrimRight(stdout, "\n"), nil
}

// hermeticEnv pins identity and disables system/global config + gpg signing so
// results don't depend on the host's ~/.gitconfig.
func hermeticEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=sigbound",
		"GIT_AUTHOR_EMAIL=sigbound@local",
		"GIT_COMMITTER_NAME=sigbound",
		"GIT_COMMITTER_EMAIL=sigbound@local",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	)
}

// Init creates a new repository at g.dir with `main` as the initial branch.
func (g *Git) Init(ctx context.Context) error {
	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return err
	}
	_, err := g.run(ctx, "init", "-q", "-b", "main")
	if err != nil {
		return err
	}
	// gc.auto=0 keeps background repacks from perturbing benchmark timings.
	_, err = g.run(ctx, "config", "gc.auto", "0")
	return err
}

// AddAll stages every change in the working tree (`git add -A`). This scans the
// whole worktree, so it is O(repo size); prefer Add when the changed paths are
// known.
func (g *Git) AddAll(ctx context.Context) error {
	_, err := g.run(ctx, "add", "-A")
	return err
}

// HasUncommittedChanges reports whether the working tree has any staged,
// unstaged, or untracked changes relative to HEAD (`git status --porcelain`
// non-empty). The driver uses it to decide whether an agent left edits it never
// committed, so those edits can be auto-committed instead of lost.
func (g *Git) HasUncommittedChanges(ctx context.Context) (bool, error) {
	out, err := g.run(ctx, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// Add stages specific paths (`git add -- <paths>`). Cost is O(len(paths)), so a
// worker that knows what it touched avoids a full-worktree rescan.
func (g *Git) Add(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	_, err := g.run(ctx, append([]string{"add", "--"}, paths...)...)
	return err
}

// Commit records staged changes and returns the new commit SHA. It allows empty
// commits so callers don't have to special-case no-op edits.
func (g *Git) Commit(ctx context.Context, msg string) (string, error) {
	if _, err := g.run(ctx, "commit", "-q", "--allow-empty", "--no-gpg-sign", "-m", msg); err != nil {
		return "", err
	}
	return g.HeadSHA(ctx)
}

// CommitAll stages everything and commits in one step.
func (g *Git) CommitAll(ctx context.Context, msg string) (string, error) {
	if err := g.AddAll(ctx); err != nil {
		return "", err
	}
	return g.Commit(ctx, msg)
}

// AddAllSparse stages every change like AddAll but adds `--sparse`, so paths
// OUTSIDE the worktree's sparse-checkout definition are staged too. Plain `git
// add -A` SKIPS them (a safety default), which in a sparse worktree (see
// WorktreePopulateSparse) would silently drop an agent's out-of-lane write
// BEFORE it reached the commit — hiding it from the base...head diff lane
// enforcement reads, so a stray that must be caught never would be. --sparse
// keeps that write in the commit so lane enforcement catches it exactly as in a
// full checkout, and leaves the skip-worktree entries untouched (never staged
// as spurious deletions just because they're absent from disk). On a non-sparse
// worktree --sparse is a no-op — plain AddAll's exact behavior.
func (g *Git) AddAllSparse(ctx context.Context) error {
	_, err := g.run(ctx, "add", "-A", "--sparse")
	return err
}

// CommitAllSparse is CommitAll's sparse-worktree form: it stages with
// AddAllSparse (so out-of-sparse paths are included, not dropped) then commits.
// The driver's auto-commit path uses it when an agent ran in a sparse worktree,
// so an edit-only agent's out-of-lane stray still lands in the branch and is
// caught by lane enforcement, identically to a full checkout.
func (g *Git) CommitAllSparse(ctx context.Context, msg string) (string, error) {
	if err := g.AddAllSparse(ctx); err != nil {
		return "", err
	}
	return g.Commit(ctx, msg)
}

// HeadSHA returns the SHA of HEAD.
func (g *Git) HeadSHA(ctx context.Context) (string, error) {
	return g.run(ctx, "rev-parse", "HEAD")
}

// FastImport feeds a fast-import stream on stdin, creating blobs, commits and
// refs in ONE process. The benchmark uses it to synthesize many agent branches
// without a `git commit` fork each. --done requires the stream to terminate with
// a `done` command, so a truncated stream fails loudly instead of importing a
// partial fixture. It is NOT a realistic per-agent commit path (a real agent is an
// independent slow process — see worker.Run); it only measures git's import speed.
func (g *Git) FastImport(ctx context.Context, stream []byte) error {
	_, stderr, code, err := g.runWith(ctx, nil, stream, "fast-import", "--quiet", "--done")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("fast-import: exit %d: %s", code, strings.TrimSpace(stderr))
	}
	return nil
}

// RevParse resolves any ref/commit-ish to a SHA.
func (g *Git) RevParse(ctx context.Context, ref string) (string, error) {
	return g.run(ctx, "rev-parse", "--verify", ref+"^{commit}")
}

// TreeOID returns the OID of a commit's top-level tree. Because git trees are
// content-addressed, two commits have equal TreeOID iff their trees are byte-for
// -byte identical — an O(1) exact tree-equality test with no per-file reads.
func (g *Git) TreeOID(ctx context.Context, rev string) (string, error) {
	return g.run(ctx, "rev-parse", "--verify", rev+"^{tree}")
}

// GitCommonDir returns the absolute path of the repo's SHARED git directory
// (`git rev-parse --path-format=absolute --git-common-dir`) — the main
// repo's .git, even when g points at a linked worktree (a worktree's own
// .git is a per-worktree file pointing at a subdirectory of this one; every
// worktree off the same repo resolves to the SAME common dir). Used to
// locate storage that must live with the repo itself and survive across
// worktrees/clones being reused, e.g. -verify-cache's on-disk entries (see
// cmd/sig's verifyCacheDir).
func (g *Git) GitCommonDir(ctx context.Context) (string, error) {
	return g.run(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

// WorktreeAdd creates a new worktree at path on a NEW branch off base. Runs in
// the main repo; the new worktree shares this repo's object store. Uses `-b`
// (create, error if branch already exists) so this can never silently reset a
// pre-existing branch — including a leftover agent/<id> branch from a prior
// run that still holds a user's committed work; that must fail loudly, not
// vanish. A caller that needs to re-create a branch THIS SAME RUN already
// made (e.g. an -agent-retries attempt) wants WorktreeAddReset instead.
func (g *Git) WorktreeAdd(ctx context.Context, path, branch, base string) error {
	_, err := g.run(ctx, "worktree", "add", "-q", "-b", branch, "--", path, base)
	return err
}

// WorktreeAddReset creates a new worktree at path on branch, resetting branch
// to base if it already exists (`git worktree add -B`). ONLY safe when branch
// is one THIS SAME RUN already created earlier — e.g. cmd/sig's
// -agent-retries re-creating attempt N+1's worktree after tearing down
// attempt N's failed one on the same agent/<id> branch. Never call this on a
// branch name that might pre-exist from outside the current run: -B silently
// discards whatever commits it already pointed at, which is exactly what
// WorktreeAdd's loud-fail `-b` protects against.
func (g *Git) WorktreeAddReset(ctx context.Context, path, branch, base string) error {
	_, err := g.run(ctx, "worktree", "add", "-q", "-B", branch, "--", path, base)
	return err
}

// WorktreeAddNoCheckout is WorktreeAdd's two-phase form: it creates the worktree
// and its `-b` branch (the SAME loud-fail-on-collision rule — never silently
// resets a pre-existing branch) but STOPS before checking the tree out. The
// working directory is left empty and the index unpopulated, with HEAD already
// pointing at branch@base; the caller then runs WorktreePopulate (on a Git bound
// to path) to materialize it. Splitting the add this way lets a caller hold its
// serialization lock across only the shared-state half — the branch ref,
// packed-refs, .git/worktrees/<name> metadata — and populate in parallel; see
// cell.AddWorktree.
func (g *Git) WorktreeAddNoCheckout(ctx context.Context, path, branch, base string) error {
	_, err := g.run(ctx, "worktree", "add", "-q", "--no-checkout", "-b", branch, "--", path, base)
	return err
}

// WorktreeAddResetNoCheckout is WorktreeAddReset's no-checkout form (`-B`): same
// reset-if-branch-exists behavior and the same ONLY-safe-for-a-branch-this-run-
// already-made caveat as WorktreeAddReset, but stops before checkout like
// WorktreeAddNoCheckout. Pair it with WorktreePopulate.
func (g *Git) WorktreeAddResetNoCheckout(ctx context.Context, path, branch, base string) error {
	_, err := g.run(ctx, "worktree", "add", "-q", "--no-checkout", "-B", branch, "--", path, base)
	return err
}

// WorktreePopulate materializes a worktree created with WorktreeAddNoCheckout/
// WorktreeAddResetNoCheckout: `git reset --hard` fills THIS worktree's own index
// and working tree from its already-set HEAD (branch@base), reading only the
// shared, content-addressed object store. It moves no ref (reset targets HEAD,
// where the branch already is) and touches no other worktree's state, so
// concurrent populates in distinct worktrees never contend — the O(tree size)
// checkout runs in parallel. g must be bound to the worktree dir (use At(dir)).
func (g *Git) WorktreePopulate(ctx context.Context) error {
	_, err := g.run(ctx, "reset", "--hard", "-q")
	return err
}

// WorktreePopulateSparse materializes a worktree created with
// WorktreeAddNoCheckout as a SPARSE checkout: it pins the worktree's
// sparse-checkout patterns to exactly paths, then checks out HEAD so ONLY the
// files matching those patterns land on disk. The index is still fully
// populated from HEAD (every base path present, the skip-worktree bit set on
// the unmaterialized ones), so a later add/commit still produces a COMPLETE,
// correct tree — only the working directory is partial. This is
// WorktreePopulate's lane-scoped counterpart in the #85 add/populate split: it
// runs OUTSIDE the cell lock in place of the full `reset --hard`, trading an
// O(tree size) checkout for one bounded by len(paths) — the disk + I/O win #86
// exists for. g must be bound to the worktree dir (use At(dir)).
//
// --no-cone matches paths as literal gitignore-style entries (exact files), not
// cone-mode directory prefixes: a lane of individual files must not drag in each
// file's whole directory. An empty paths is a caller bug — a no-lane worktree
// must take the full WorktreePopulate path instead — so it errors rather than
// silently checking out an empty tree.
func (g *Git) WorktreePopulateSparse(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("WorktreePopulateSparse: no paths (a no-lane worktree must use WorktreePopulate)")
	}
	if _, err := g.run(ctx, append([]string{"sparse-checkout", "set", "--no-cone"}, paths...)...); err != nil {
		return err
	}
	// The worktree was added --no-checkout, so nothing is on disk and its index
	// is empty; checking out HEAD now fills the index from HEAD and writes only
	// the sparse set (skip-worktree on the rest) into the working tree.
	if _, err := g.run(ctx, "checkout", "-q", "HEAD"); err != nil {
		return err
	}
	return nil
}

// WorktreeAddSparse creates a worktree WITHOUT checking out the tree, then
// populates the index from base via read-tree. The working directory starts
// empty but the index holds every base path, so a commit that stages only the
// changed files still produces a complete, correct tree. This trades an O(M)
// checkout for an O(1) index write — the difference between a benchmark that
// spends its time in git plumbing vs. in filesystem writes. Agents materialize
// individual files they need with CheckoutPaths.
func (g *Git) WorktreeAddSparse(ctx context.Context, path, branch, base string) error {
	if _, err := g.run(ctx, "worktree", "add", "-q", "--no-checkout", "-b", branch, "--", path, base); err != nil {
		return err
	}
	// read-tree must run inside the new worktree to fill its index.
	if _, err := g.At(path).run(ctx, "read-tree", "HEAD"); err != nil {
		return err
	}
	return nil
}

// CheckoutPaths materializes specific paths from the index into the working
// directory (`git checkout -- <paths>`). Used with sparse worktrees to bring a
// file onto disk before editing it.
func (g *Git) CheckoutPaths(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	_, err := g.run(ctx, append([]string{"checkout", "--"}, paths...)...)
	return err
}

// WorktreeAddDetached checks out commit into a new worktree at path in detached
// HEAD state (no branch created). Used to materialize an integrated tree for
// verification without touching any branch ref or the main working tree.
func (g *Git) WorktreeAddDetached(ctx context.Context, path, commit string) error {
	_, err := g.run(ctx, "worktree", "add", "-q", "--detach", "--", path, commit)
	return err
}

// WorktreeRemove tears down a worktree's directory + admin files. The branch it
// pointed at is left intact in the shared store.
func (g *Git) WorktreeRemove(ctx context.Context, path string) error {
	_, err := g.run(ctx, "worktree", "remove", "--force", "--", path)
	return err
}

// WorktreeEntry is one record from `git worktree list --porcelain`. Prunable
// is non-empty exactly when git considers this worktree's registration stale
// (its gitdir file points to a directory that no longer exists) — the same
// determination `git worktree prune` itself acts on — so a caller deciding
// what WorktreePrune would remove never has to reimplement that logic.
type WorktreeEntry struct {
	Path     string
	Prunable string // reason, e.g. "gitdir file points to non-existent location"; empty when not prunable
}

// WorktreeList lists every worktree git has registered for this repo
// (including the main one) via `git worktree list --porcelain`.
func (g *Git) WorktreeList(ctx context.Context) ([]WorktreeEntry, error) {
	out, err := g.run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeListPorcelain(out), nil
}

// parseWorktreeListPorcelain decodes `git worktree list --porcelain` output:
// records are separated by a blank line, each starting with a "worktree
// <path>" line; an optional "prunable <reason>" line, when present, carries
// the Prunable annotation for the CURRENT record. It is a pure function of
// untrusted git output and must never panic on malformed input.
func parseWorktreeListPorcelain(out string) []WorktreeEntry {
	var entries []WorktreeEntry
	var cur *WorktreeEntry
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			entries = append(entries, WorktreeEntry{Path: strings.TrimPrefix(line, "worktree ")})
			cur = &entries[len(entries)-1]
		case strings.HasPrefix(line, "prunable ") && cur != nil:
			cur.Prunable = strings.TrimPrefix(line, "prunable ")
		case line == "":
			cur = nil
		}
	}
	return entries
}

// WorktreePrune removes administrative files for every worktree whose
// directory no longer exists (`git worktree prune`) — the real-deletion half
// of what WorktreeList's Prunable annotation reports. Used by `sig gc` to
// clear registrations a killed run left behind (the worktree dir is gone,
// but git still lists it until this runs).
func (g *Git) WorktreePrune(ctx context.Context) error {
	_, err := g.run(ctx, "worktree", "prune")
	return err
}

// RefCommit is one ref's short name alongside its target commit's SHA and
// committer time, as read by ForEachRefCommit.
type RefCommit struct {
	Name       string
	SHA        string
	CommitTime time.Time
}

// ForEachRefCommit lists every ref under the given prefixes (each a full
// "refs/heads/..." path) with its target commit's SHA and committer time, in
// ONE `git for-each-ref` call. Prefixes match as trailing path components —
// git's own for-each-ref semantics — so "refs/heads/agent/" matches
// refs/heads/agent/t1 but not refs/heads/agent2/x. Used by `sig gc` to find
// agent/* and imported/*/* branches old enough to sweep.
func (g *Git) ForEachRefCommit(ctx context.Context, prefixes ...string) ([]RefCommit, error) {
	args := append([]string{"for-each-ref", "--format=%(refname:short)%09%(committerdate:unix)%09%(objectname)"}, prefixes...)
	out, err := g.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseForEachRefCommit(out)
}

// parseForEachRefCommit decodes ForEachRefCommit's tab-separated output. It
// is a pure function of untrusted git output and must never panic on
// malformed input.
func parseForEachRefCommit(out string) ([]RefCommit, error) {
	var refs []RefCommit
	for _, line := range splitLines(out) {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("for-each-ref: malformed record %q", line)
		}
		sec, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("for-each-ref: bad committerdate in %q: %w", line, err)
		}
		refs = append(refs, RefCommit{Name: fields[0], SHA: fields[2], CommitTime: time.Unix(sec, 0)})
	}
	return refs, nil
}

// BranchDelete force-deletes a local branch ref (`git branch -D`). Any commit
// it pointed at is left in the object store (unreachable, subject to normal
// gc) — this only ever removes the REF, never a commit. Used by `sig run
// -resume` to clear a stale no-op agent/<id> branch (head == the base it
// forked from, i.e. that agent committed nothing last time) before letting a
// fresh run re-create it with the ordinary loud-fail WorktreeAdd.
func (g *Git) BranchDelete(ctx context.Context, branch string) error {
	_, err := g.run(ctx, "branch", "-D", branch)
	return err
}

// CheckoutDetach switches an ALREADY-detached worktree to commit — `git
// checkout --detach <commit>` — touching only the paths that differ from
// whatever the worktree currently has checked out. Used to advance a verify
// worktree onto a repair-advanced head instead of tearing it down and running
// a fresh WorktreeAddDetached, which re-materializes the whole tree.
func (g *Git) CheckoutDetach(ctx context.Context, commit string) error {
	_, err := g.run(ctx, "checkout", "-q", "--detach", commit)
	return err
}

// Clean removes every untracked and ignored file/directory from the working
// tree (`git clean -fdx`). Required before reusing a worktree for another
// verify/repair invocation: a command under test can leave build artifacts
// (binaries, coverage files, generated code) that must not leak into the next
// invocation's view of the tree — without this, reuse would trade a real
// performance win for a hermeticity bug. Toolchain/module caches (GOCACHE,
// GOMODCACHE, and the like) live outside the worktree, so this never touches
// them.
//
// Clean alone is NOT enough to make a worktree pristine: it only ever deletes
// untracked/ignored paths, so a command under test that instead MODIFIES a
// file git already tracks leaves that edit in place — pair Clean with
// ResetHard wherever reuse needs a fully pristine tree.
func (g *Git) Clean(ctx context.Context) error {
	_, err := g.run(ctx, "clean", "-q", "-fdx")
	return err
}

// ResetHard restores every tracked file in the working tree to HEAD's
// committed content (`git reset --hard`), discarding any staged or unstaged
// modification a prior command made to a file git already tracks. This is
// Clean's complement, not a replacement for it: Clean deletes untracked
// leftovers, ResetHard reverts tracked-file edits, and reusing a worktree for
// another verify/repair invocation needs BOTH to guarantee the next
// invocation sees the exact tree HEAD names — a command under test that edits
// a tracked file (a config, a fixture, a generated-but-committed file) would
// otherwise leak that edit into the next invocation, and — because `git
// checkout --detach` (see CheckoutDetach) only touches paths that differ
// between the old and new commit — straight through a later checkout too if
// the mutated path happens to be identical in both commits' trees.
func (g *Git) ResetHard(ctx context.Context) error {
	_, err := g.run(ctx, "reset", "-q", "--hard")
	return err
}

// CheckoutB creates or resets branch to start and checks it out (`git checkout
// -B`). Used to rewind an integration worktree back to base between groups; only
// the paths that differ from the current state are touched, so it's cheap.
func (g *Git) CheckoutB(ctx context.Context, branch, start string) error {
	_, err := g.run(ctx, "checkout", "-q", "-B", branch, start)
	return err
}

// DiffNameOnly returns the set of paths that differ on branch relative to base
// (`git diff --name-only base...branch`). This is the raw write-set.
func (g *Git) DiffNameOnly(ctx context.Context, base, branch string) ([]string, error) {
	out, err := g.run(ctx, "diff", "--name-only", base+"..."+branch)
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// MergeResult reports the outcome of a merge attempt.
type MergeResult struct {
	// OK is true when git produced a clean merged tree (including 3-way
	// auto-resolution of non-overlapping hunks — tier (a), no human needed).
	OK bool
	// Conflicts lists the paths git could not auto-resolve. Non-empty only when
	// OK is false. On conflict the merge is aborted so the worktree is clean.
	Conflicts []string
}

// Merge merges one or more refs into the current branch with `git merge`.
//
//   - One ref  -> ordinary 3-way merge (may fast-forward).
//   - Many refs -> octopus merge (single commit, N parents). Safe to use only
//     when the refs are known conflict-free (e.g. OCC-disjoint groups).
//
// A clean result yields OK=true. A content conflict yields OK=false with the
// conflicting paths, and the working tree is restored via `merge --abort`. Any
// other failure returns an error.
func (g *Git) Merge(ctx context.Context, msg string, refs ...string) (MergeResult, error) {
	args := append([]string{"merge", "--no-edit", "--no-gpg-sign", "-m", msg}, refs...)
	_, mergeErr := g.run(ctx, args...)
	if mergeErr == nil {
		return MergeResult{OK: true}, nil
	}
	// Non-zero exit: distinguish a real conflict from an operational error.
	conflicts, cerr := g.unmergedPaths(ctx)
	if cerr != nil {
		return MergeResult{}, cerr
	}
	if len(conflicts) == 0 {
		// Not a conflict (e.g. octopus refused, bad ref, index.lock
		// contention). Surface git's own stderr (carried on mergeErr via
		// g.run's %s-wrapped message) instead of swallowing it — a merge
		// failure a human can't see is unfixable when it flakes.
		return MergeResult{}, fmt.Errorf("merge %v failed without recorded conflicts: %w", refs, mergeErr)
	}
	if _, err := g.run(ctx, "merge", "--abort"); err != nil {
		return MergeResult{}, fmt.Errorf("merge --abort after conflict: %w", err)
	}
	return MergeResult{OK: false, Conflicts: conflicts}, nil
}

// unmergedPaths lists paths in a conflicted state in the index.
func (g *Git) unmergedPaths(ctx context.Context) ([]string, error) {
	out, err := g.run(ctx, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// ShowFile returns the contents of path at the given commit-ish (`git show
// rev:path`). Used by correctness checks to read the integrated tree without a
// checkout.
func (g *Git) ShowFile(ctx context.Context, rev, path string) (string, error) {
	return g.run(ctx, "show", rev+":"+path)
}

// LsTree lists the paths present in a tree (recursively).
func (g *Git) LsTree(ctx context.Context, rev string) ([]string, error) {
	out, err := g.run(ctx, "ls-tree", "-r", "--name-only", rev)
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// TreeSize returns the total size in bytes of every blob in rev's tree,
// summed recursively via `git ls-tree -r -l -z` (each blob's size field is
// the object's raw content length, the same number `git cat-file -s` would
// report for it). This slightly UNDERCOUNTS what a full checkout of rev
// would actually occupy on disk — filesystem block rounding, a worktree's
// own per-checkout admin files, and packed-object growth as `worktree add`
// unpacks into the shared store are all ignored — good enough for a
// preflight estimate, not a byte-exact accounting; callers pad it with a
// safety margin rather than treating it as an upper bound. Used by cmd/sig's
// disk-space preflight (`sig run`'s -no-disk-check gate and `sig doctor`'s
// disk line) to bound N worktrees' worth of checkout against free space
// before spending any agent call.
// Submodule entries report size "-" (their content lives in another
// repository's object store, not this one) and contribute 0.
func (g *Git) TreeSize(ctx context.Context, rev string) (int64, error) {
	out, err := g.run(ctx, "ls-tree", "-r", "-l", "-z", rev)
	if err != nil {
		return 0, err
	}
	return parseLsTreeSizesZ(out)
}

// parseLsTreeSizesZ sums the blob sizes from `git ls-tree -r -l -z` output.
// Each record is "<mode> SP <type> SP <oid> SP <size>\t<path>\0" — the size
// field is space-padded for column alignment in git's own formatting, hence
// strings.Fields rather than a fixed-width slice. A submodule entry's size is
// the literal string "-" (git doesn't track a submodule's content size in
// THIS repository's object store) and contributes 0, not an error. It is a
// pure function of untrusted git output and must never panic on malformed
// input. Fuzzed by FuzzParseLsTreeSizesZ.
func parseLsTreeSizesZ(out string) (int64, error) {
	var total int64
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" { // trailing empty after final NUL
			continue
		}
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			return 0, fmt.Errorf("ls-tree -l: malformed record %q", rec)
		}
		fields := strings.Fields(rec[:tab])
		if len(fields) < 4 {
			return 0, fmt.Errorf("ls-tree -l: malformed record %q", rec)
		}
		if fields[3] == "-" { // submodule: no size in this object store
			continue
		}
		n, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("ls-tree -l: bad size in record %q", rec)
		}
		total += n
	}
	return total, nil
}

// MergeTreeResult is the outcome of an in-memory (object-store) 3-way merge.
type MergeTreeResult struct {
	Tree      string   // OID of the merged tree (valid even when conflicted)
	OK        bool     // true when git produced a clean merge
	Conflicts []string // paths needing human resolution (only when !OK)
}

// MergeTree performs a 3-way merge of ours and theirs against the given merge
// base entirely in the object store — no worktree, no checkout, no index. It
// writes the merged tree and returns its OID. This is the MVCC-style landing
// primitive: many of these run in parallel with zero working-tree contention.
//
// A clean merge (git auto-resolves non-overlapping hunks, tier a) yields
// OK=true. A content conflict (tier b) yields OK=false with the conflicting
// paths. Callers wrap the tree in a commit via CommitTree to accumulate.
func (g *Git) MergeTree(ctx context.Context, mergeBase, ours, theirs string) (MergeTreeResult, error) {
	stdout, stderr, code, err := g.runRaw(ctx,
		"merge-tree", "--write-tree", "-z", "--name-only",
		"--merge-base="+mergeBase, ours, theirs)
	if err != nil {
		return MergeTreeResult{}, err
	}
	switch code {
	case 0:
		tree, _ := parseMergeTreeZ(stdout, false)
		return MergeTreeResult{Tree: tree, OK: true}, nil
	case 1:
		tree, conflicts := parseMergeTreeZ(stdout, true)
		return MergeTreeResult{Tree: tree, OK: false, Conflicts: conflicts}, nil
	default:
		return MergeTreeResult{}, fmt.Errorf("merge-tree exit %d: %s", code, strings.TrimSpace(stderr))
	}
}

// parseMergeTreeZ decodes `git merge-tree --write-tree -z --name-only` stdout.
// The output is NUL-separated: field 0 is the merged tree OID; when conflicted is
// true (git exit 1) the fields after it, up to the first empty field, are the
// conflicted path names (deduplicated). It is a pure function of untrusted git
// output and must never panic on malformed input — strings.Split always yields at
// least one element, so fields[0] is always safe. Fuzzed by FuzzParseMergeTreeZ.
func parseMergeTreeZ(stdout string, conflicted bool) (tree string, conflicts []string) {
	fields := strings.Split(stdout, "\x00")
	tree = strings.TrimSpace(fields[0])
	if !conflicted {
		return tree, nil
	}
	seen := map[string]bool{}
	for _, f := range fields[1:] {
		if f == "" {
			break
		}
		if !seen[f] {
			seen[f] = true
			conflicts = append(conflicts, f)
		}
	}
	return tree, conflicts
}

// CommitTree wraps a tree OID in a commit with the given parents, returning the
// new commit OID. Used to accumulate a chain of merged trees without ever
// touching a working directory.
func (g *Git) CommitTree(ctx context.Context, tree string, parents []string, msg string) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	args = append(args, "-m", msg)
	return g.run(ctx, args...)
}

func splitLines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
