package gitx

// This file is the plumbing layer: object-database primitives that let the
// integrator land branches WITHOUT a working tree or per-op process churn.
//
//   - runWith      : one exec entry point that can pin an alternate index and
//                    pipe stdin (the substrate for the batched commands below).
//   - DiffRaw      : destination-side (mode, oid) for every changed path.
//   - OverlayTrees : union of disjoint trees over a base via a scratch index —
//                    the "no merge at all" fast path (proven equal to merge-tree
//                    for disjoint inputs, see plumbing_test.go).
//   - BatchReader  : one long-running `git cat-file --batch-check` reused for all
//                    object/ref resolution across a run (no per-lookup spawn).
//   - UpdateRefs   : one `git update-ref --stdin` applying every ref move
//                    atomically in a single process (the final landing).

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"

// runWith is the single exec entry point. extraEnv is appended to the hermetic
// environment (e.g. GIT_INDEX_FILE=... to target a scratch index), and stdin, if
// non-nil, is piped to the process. A non-zero exit is returned in code (not as
// err) so callers can treat exit codes as signal; err is non-nil only when the
// process could not run.
func (g *Git) runWith(ctx context.Context, extraEnv []string, stdin []byte, args ...string) (stdout, stderr string, code int, err error) {
	full := append([]string{"-C", g.dir}, args...)
	cmd := exec.CommandContext(ctx, g.bin, full...)
	cmd.Env = hermeticEnv()
	if len(extraEnv) > 0 {
		cmd.Env = append(cmd.Env, extraEnv...)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	runErr := cmd.Run()
	if runErr == nil {
		return so.String(), se.String(), 0, nil
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return so.String(), se.String(), ee.ExitCode(), nil
	}
	return so.String(), se.String(), -1, fmt.Errorf("git %s: %w", strings.Join(args, " "), runErr)
}

// DiffRawEntry is one changed path from `git diff --raw`, carrying the
// DESTINATION-side tree entry (what `to` has). A deletion has Mode "000000" and
// an all-zero OID — fed verbatim to `update-index --index-info` it removes the
// path, so overlay callers need no special case.
type DiffRawEntry struct {
	Mode string // e.g. 100644, 100755, 120000, 160000, or 000000 for a deletion
	OID  string // 40-hex blob/commit OID (all zeros for a deletion)
	Path string
}

// Deleted reports whether this entry removes a path.
func (e DiffRawEntry) Deleted() bool { return e.OID == zeroOID }

// DiffRaw returns the destination-side entries that differ between two tree-ish
// revisions (a commit, a bare tree OID, or a ref). Rename detection is OFF so a
// rename appears as delete + add — exactly the shape a tree overlay applies. OIDs
// are full (--no-abbrev) so entries drop straight into update-index.
func (g *Git) DiffRaw(ctx context.Context, from, to string) ([]DiffRawEntry, error) {
	out, se, code, err := g.runWith(ctx, nil, nil,
		"diff", "--raw", "--no-renames", "--no-abbrev", "-z", from, to)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("diff --raw %s %s: exit %d: %s", from, to, code, strings.TrimSpace(se))
	}
	return parseDiffRawZ(out)
}

// parseDiffRawZ decodes NUL-delimited `git diff --raw -z --no-renames` output.
// Records are: ":<srcmode> <dstmode> <srcsha> <dstsha> <status>\0<path>\0".
func parseDiffRawZ(out string) ([]DiffRawEntry, error) {
	parts := strings.Split(out, "\x00")
	var ents []DiffRawEntry
	i := 0
	for i < len(parts) {
		meta := parts[i]
		if meta == "" { // trailing empty after final NUL
			i++
			continue
		}
		if i+1 >= len(parts) {
			return nil, fmt.Errorf("diff --raw: dangling record %q", meta)
		}
		path := parts[i+1]
		i += 2
		f := strings.Fields(strings.TrimPrefix(meta, ":"))
		if len(f) < 4 {
			return nil, fmt.Errorf("diff --raw: malformed record %q", meta)
		}
		ents = append(ents, DiffRawEntry{Mode: f[1], OID: f[3], Path: path})
	}
	return ents, nil
}

// OverlayTrees builds the union of several trees over a common base by overlaying
// each tree's changed entries onto base in a private scratch index — the disjoint
// fast path that runs NO 3-way merge. It is provably equal to merging the trees
// when their change-sets touch disjoint paths (TestOverlayTreesEqualsMergeTree).
//
// Mechanism, all in the object store (no worktree): a temp GIT_INDEX_FILE seeded
// from base via read-tree; every tree's `diff --raw` entries (gathered in
// parallel) applied through ONE `update-index --index-info`; one `write-tree`
// emits the union tree OID.
//
// The per-tree diffs are independent and run concurrently; only the index writes
// are serialized (they must be — a single index file).
func (g *Git) OverlayTrees(ctx context.Context, base string, trees []string) (string, error) {
	// Reserve a unique path, then remove it so read-tree writes a fresh index
	// (a 0-byte file is not a valid index).
	f, err := os.CreateTemp("", "sig-idx-*")
	if err != nil {
		return "", err
	}
	idxPath := f.Name()
	_ = f.Close()
	_ = os.Remove(idxPath)
	defer os.Remove(idxPath)
	idxEnv := []string{"GIT_INDEX_FILE=" + idxPath}

	if _, se, code, err := g.runWith(ctx, idxEnv, nil, "read-tree", base); err != nil {
		return "", err
	} else if code != 0 {
		return "", fmt.Errorf("overlay read-tree %s: %s", base, strings.TrimSpace(se))
	}

	// Gather every tree's changed entries against base in ONE `git diff-tree
	// --stdin` process (not one `git diff` per tree). The two-tree stdin form
	// silently ignores an unreadable left-hand tree, so validate the trees first
	// to keep a bad head a loud error instead of a silently-dropped change-set.
	if err := g.ensureTreeish(ctx, trees); err != nil {
		return "", err
	}
	all, err := g.diffTreeUnion(ctx, base, trees)
	if err != nil {
		return "", err
	}

	if len(all) > 0 {
		var b strings.Builder
		for _, e := range all {
			// update-index --index-info is line-oriented (mode SP oid TAB path);
			// there is no NUL variant, so a tab/newline in a path would corrupt
			// the stream. Reject it loudly rather than land a wrong tree.
			if strings.ContainsAny(e.Path, "\t\n") {
				return "", fmt.Errorf("overlay: path %q contains tab/newline (unsupported by update-index --index-info)", e.Path)
			}
			fmt.Fprintf(&b, "%s %s\t%s\n", e.Mode, e.OID, e.Path)
		}
		if _, se, code, err := g.runWith(ctx, idxEnv, []byte(b.String()), "update-index", "--index-info"); err != nil {
			return "", err
		} else if code != 0 {
			return "", fmt.Errorf("overlay update-index: %s", strings.TrimSpace(se))
		}
	}

	out, se, code, err := g.runWith(ctx, idxEnv, nil, "write-tree")
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("overlay write-tree: %s", strings.TrimSpace(se))
	}
	return strings.TrimSpace(out), nil
}

// diffTreeUnion returns the union of every tree's DESTINATION-side changed
// entries against base, computed in ONE `git diff-tree --stdin` process rather
// than a `git diff` fork per tree — the batched replacement for the old
// per-branch diff fan-out. Each input line is "<tree> <base>": the two-tree
// --stdin form diffs its SECOND argument to its FIRST (the reverse of the
// command line), so writing the tree first yields the base->tree diff, whose
// destination side is that tree's entry — exactly what the overlay index applies.
// Callers pass path-disjoint trees (the OCC invariant), so concatenating every
// pair's records never puts two entries on one path; order is irrelevant.
func (g *Git) diffTreeUnion(ctx context.Context, base string, trees []string) ([]DiffRawEntry, error) {
	if len(trees) == 0 {
		return nil, nil
	}
	if strings.ContainsAny(base, " \t\n") {
		return nil, fmt.Errorf("diff-tree --stdin: illegal whitespace in base %q", base)
	}
	var in strings.Builder
	for _, t := range trees {
		if strings.ContainsAny(t, " \t\n") {
			return nil, fmt.Errorf("diff-tree --stdin: illegal whitespace in tree %q", t)
		}
		in.WriteString(t)
		in.WriteByte(' ')
		in.WriteString(base)
		in.WriteByte('\n')
	}
	// -r recurses into subtrees (raw diff-tree is otherwise tree-shallow);
	// --no-commit-id/-z/--no-abbrev/--no-renames match DiffRaw's record shape so
	// parseDiffRawZ decodes the concatenated stream unchanged.
	out, se, code, err := g.runWith(ctx, nil, []byte(in.String()),
		"diff-tree", "--stdin", "-r", "-z", "--raw", "--no-renames", "--no-abbrev", "--no-commit-id")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("diff-tree --stdin: exit %d: %s", code, strings.TrimSpace(se))
	}
	return parseDiffRawZ(out)
}

// ensureTreeish verifies every rev resolves to a readable tree, in ONE
// `git cat-file --batch-check` process. diffTreeUnion's two-tree stdin form
// SILENTLY skips an unreadable left-hand tree (exit 0, no records), which would
// drop that head from the overlay union with no error; validating up front turns
// that into a loud failure — the robustness the old per-branch DiffRaw gave for free.
func (g *Git) ensureTreeish(ctx context.Context, revs []string) error {
	if len(revs) == 0 {
		return nil
	}
	var in strings.Builder
	for _, r := range revs {
		if strings.ContainsAny(r, " \t\n") {
			return fmt.Errorf("cat-file --batch-check: illegal whitespace in %q", r)
		}
		in.WriteString(r)
		in.WriteString("^{tree}\n")
	}
	out, se, code, err := g.runWith(ctx, nil, []byte(in.String()), "cat-file", "--batch-check")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("cat-file --batch-check: exit %d: %s", code, strings.TrimSpace(se))
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		// batch-check prints "<oid> tree <size>" for a good tree, "<input> missing"
		// (or "... ambiguous") otherwise.
		if strings.HasSuffix(line, " missing") || strings.HasSuffix(line, " ambiguous") {
			return fmt.Errorf("overlay: unreadable tree-ish: %s", line)
		}
	}
	return nil
}

// UpdateRefs moves every ref to its new OID in ONE `git update-ref --stdin`
// process. update-ref applies the whole batch as a single transaction, so this
// is the atomic final-landing primitive (one ref for a single land, many for a
// batch). No-op on an empty map.
func (g *Git) UpdateRefs(ctx context.Context, updates map[string]string) error {
	if len(updates) == 0 {
		return nil
	}
	var b strings.Builder
	for ref, oid := range updates {
		if strings.ContainsAny(ref, " \t\n") || strings.ContainsAny(oid, " \t\n") {
			return fmt.Errorf("update-ref: illegal whitespace in ref %q / oid %q", ref, oid)
		}
		fmt.Fprintf(&b, "update %s %s\n", ref, oid)
	}
	_, se, code, err := g.runWith(ctx, nil, []byte(b.String()), "update-ref", "--stdin")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("update-ref --stdin: exit %d: %s", code, strings.TrimSpace(se))
	}
	return nil
}

// UpdateRef is the single-ref convenience over UpdateRefs.
func (g *Git) UpdateRef(ctx context.Context, ref, oid string) error {
	return g.UpdateRefs(ctx, map[string]string{ref: oid})
}

// HashObject writes body to the object store as a blob and returns its OID
// (`git hash-object -w --stdin`). Used to materialize a conflict resolver's
// output before splicing it into a tree — no worktree involved.
func (g *Git) HashObject(ctx context.Context, body []byte) (string, error) {
	out, se, code, err := g.runWith(ctx, nil, body, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("hash-object: exit %d: %s", code, strings.TrimSpace(se))
	}
	return strings.TrimSpace(out), nil
}

// BlobAt returns the contents of path at tree-ish rev (`git cat-file blob
// rev:path`). present is false when the path is absent at rev (e.g. one side of
// an add/add or delete/modify conflict) — NOT an error; the caller treats it as
// empty. err is reserved for a real git failure (unrunnable process).
func (g *Git) BlobAt(ctx context.Context, rev, path string) (content string, present bool, err error) {
	out, _, code, err := g.runWith(ctx, nil, nil, "cat-file", "blob", rev+":"+path)
	if err != nil {
		return "", false, err
	}
	if code != 0 {
		return "", false, nil // path absent at this rev
	}
	return out, true, nil
}

// entryMode returns the file mode of path within tree ("" when absent), so a
// spliced blob keeps the conflicted file's original mode (regular/executable/
// symlink) instead of being forced to 100644.
func (g *Git) entryMode(ctx context.Context, tree, path string) (string, error) {
	out, se, code, err := g.runWith(ctx, nil, nil, "ls-tree", "-z", tree, "--", path)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("ls-tree %s -- %s: exit %d: %s", tree, path, code, strings.TrimSpace(se))
	}
	return parseLsTreeModeZ(out)
}

// parseLsTreeModeZ extracts the file mode from a single-record `git ls-tree -z
// <tree> -- <path>` output. The record is "<mode> <type> <oid>\t<path>" with a
// trailing NUL; an empty result means the path is absent in the tree. It is a
// pure function of untrusted git output and must never panic on malformed input.
// Fuzzed by FuzzParseLsTreeModeZ.
func parseLsTreeModeZ(out string) (string, error) {
	rec := strings.TrimRight(out, "\x00")
	if rec == "" {
		return "", nil // path not present in tree
	}
	// A single `ls-tree -z` record for one path carries NUL only as its terminator
	// (trimmed above). An interior NUL means malformed or multi-record output whose
	// "mode" would be garbage — reject it rather than return a NUL-laden mode that
	// would later corrupt an `update-index --index-info` stream. (Found by fuzzing:
	// input "\x00\t" previously yielded mode "\x00".)
	if strings.IndexByte(rec, 0) >= 0 {
		return "", fmt.Errorf("ls-tree: embedded NUL in record %q", rec)
	}
	tab := strings.IndexByte(rec, '\t')
	if tab < 0 {
		return "", fmt.Errorf("ls-tree: malformed record %q", rec)
	}
	fields := strings.Fields(rec[:tab])
	if len(fields) < 1 {
		return "", fmt.Errorf("ls-tree: malformed record %q", rec)
	}
	return fields[0], nil
}

// ResolvedBlob is a conflict resolver's output for one path: the new file body.
type ResolvedBlob struct {
	Path    string
	Content string
}

// SpliceBlobs returns a new tree equal to baseTree but with each ResolvedBlob's
// Content substituted at its Path, preserving each path's existing file mode. It
// hash-objects every body, then rewrites the tree through a private scratch index
// (read-tree baseTree; update-index --index-info; write-tree) — never touching a
// worktree. This is how a conflicted merge-tree result becomes a clean, resolved
// tree once a Resolver has supplied a body for every conflicted path.
func (g *Git) SpliceBlobs(ctx context.Context, baseTree string, blobs []ResolvedBlob) (string, error) {
	if len(blobs) == 0 {
		return baseTree, nil
	}
	// Reserve a unique path, then remove it so read-tree writes a fresh index
	// (a 0-byte file is not a valid index) — same pattern as OverlayTrees.
	f, err := os.CreateTemp("", "sig-splice-*")
	if err != nil {
		return "", err
	}
	idxPath := f.Name()
	_ = f.Close()
	_ = os.Remove(idxPath)
	defer os.Remove(idxPath)
	idxEnv := []string{"GIT_INDEX_FILE=" + idxPath}

	if _, se, code, err := g.runWith(ctx, idxEnv, nil, "read-tree", baseTree); err != nil {
		return "", err
	} else if code != 0 {
		return "", fmt.Errorf("splice read-tree %s: %s", baseTree, strings.TrimSpace(se))
	}

	var b strings.Builder
	for _, rb := range blobs {
		// update-index --index-info is line-oriented (mode SP oid TAB path) with
		// no NUL variant, so a tab/newline in a path would corrupt the stream.
		// Reject it loudly rather than write a wrong tree.
		if strings.ContainsAny(rb.Path, "\t\n") {
			return "", fmt.Errorf("splice: path %q contains tab/newline (unsupported by update-index --index-info)", rb.Path)
		}
		mode, err := g.entryMode(ctx, baseTree, rb.Path)
		if err != nil {
			return "", err
		}
		if mode == "" {
			mode = "100644" // path absent in baseTree (e.g. delete/modify): add as a regular file
		}
		oid, err := g.HashObject(ctx, []byte(rb.Content))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s %s\t%s\n", mode, oid, rb.Path)
	}
	if _, se, code, err := g.runWith(ctx, idxEnv, []byte(b.String()), "update-index", "--index-info"); err != nil {
		return "", err
	} else if code != 0 {
		return "", fmt.Errorf("splice update-index: %s", strings.TrimSpace(se))
	}

	out, se, code, err := g.runWith(ctx, idxEnv, nil, "write-tree")
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("splice write-tree: %s", strings.TrimSpace(se))
	}
	return strings.TrimSpace(out), nil
}

// BatchReader wraps one long-running `git cat-file --batch-check`. Object and ref
// resolution across a whole run flows through this single process instead of a
// `git rev-parse`/`cat-file` fork per lookup. Safe for concurrent callers (the
// request/response round-trip is serialized by mu).
type BatchReader struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	closed bool
}

// NewBatchReader starts a cat-file --batch-check bound to this repo. The caller
// must Close it to reap the process.
func (g *Git) NewBatchReader(ctx context.Context) (*BatchReader, error) {
	cmd := exec.CommandContext(ctx, g.bin, "-C", g.dir, "cat-file", "--batch-check")
	cmd.Env = hermeticEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &BatchReader{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

// Resolve looks up a revision spec ("<sha>", a ref, "<rev>^{commit}",
// "<rev>:<path>", ...). exists is false when git reports the object missing;
// err is reserved for pipe/protocol failures.
func (br *BatchReader) Resolve(spec string) (oid, typ string, size int64, exists bool, err error) {
	br.mu.Lock()
	defer br.mu.Unlock()
	if br.closed {
		return "", "", 0, false, errors.New("batch reader closed")
	}
	if _, err = io.WriteString(br.stdin, spec+"\n"); err != nil {
		return "", "", 0, false, err
	}
	line, err := br.stdout.ReadString('\n')
	if err != nil {
		return "", "", 0, false, err
	}
	return parseBatchCheckLine(line)
}

// parseBatchCheckLine decodes ONE response line from `git cat-file --batch-check`.
// A present object is "<oid> <type> <size>"; a missing one is "<spec> missing".
// It is a pure function of untrusted git output and must never panic on malformed
// input (short lines, embedded NULs, junk sizes) — every field index is guarded by
// a length check first. Fuzzed by FuzzParseBatchCheckLine.
func parseBatchCheckLine(line string) (oid, typ string, size int64, exists bool, err error) {
	line = strings.TrimRight(line, "\n")
	fields := strings.Fields(line)
	if len(fields) >= 2 && fields[len(fields)-1] == "missing" {
		return "", "", 0, false, nil
	}
	if len(fields) != 3 {
		return "", "", 0, false, fmt.Errorf("cat-file batch: unexpected response %q", line)
	}
	var sz int64
	if _, err := fmt.Sscanf(fields[2], "%d", &sz); err != nil {
		return "", "", 0, false, fmt.Errorf("cat-file batch: bad size in %q", line)
	}
	return fields[0], fields[1], sz, true, nil
}

// ResolveCommit returns the commit OID a ref/rev points at, or an error if it
// does not resolve to a real object.
func (br *BatchReader) ResolveCommit(rev string) (string, error) {
	oid, _, _, ok, err := br.Resolve(rev + "^{commit}")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("cannot resolve %q to a commit", rev)
	}
	return oid, nil
}

// Close shuts the pipe and reaps the process.
func (br *BatchReader) Close() error {
	br.mu.Lock()
	defer br.mu.Unlock()
	if br.closed {
		return nil
	}
	br.closed = true
	_ = br.stdin.Close()
	return br.cmd.Wait()
}
