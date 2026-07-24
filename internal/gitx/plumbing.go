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
//   - DiffNameOnlyBatch : every branch's write-set vs. base in ONE `git
//                    diff-tree --stdin` process instead of one `git diff` fork
//                    per branch.
//   - UpdateRefs   : one `git update-ref --stdin` applying every ref move
//                    atomically in a single process (the final landing).
//   - BlobsBatch   : every conflict's base/ours/theirs blob CONTENT in ONE `git
//                    cat-file --batch` process instead of a `cat-file blob` fork
//                    per side per path (the resolver seam's blob reads).
//   - entryModesBatch : every resolved path's file mode in ONE `git ls-tree`
//                    call instead of a spawn per path (SpliceBlobs).

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
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

// diffTreeBlockZ is one input line's records from a `git diff-tree --stdin`
// run that KEEPS the commit-id header (i.e. --no-commit-id is NOT passed) —
// the header is the exact rev given as that line's first token, letting
// DiffNameOnlyBatch segment one combined run back into one write-set per
// branch.
type diffTreeBlockZ struct {
	Header string
	Paths  []string
}

// parseDiffTreeStdinRawZ decodes NUL-delimited output from `git diff-tree
// --stdin -r -z --raw --no-renames --no-abbrev` WITH commit-id headers. Git's
// --raw meta records always start with ':' (":<srcmode> <dstmode> ..."); a
// field that does NOT start with ':' can therefore only be a commit-id header
// — a bare path never appears except immediately after a ':' meta record, so
// there is no ambiguity between the two. Each header starts a new block; the
// paths that follow (the second field of each meta/path pair) belong to it,
// until the next header or end of input. A line with no diff contributes no
// header and no block at all, so blocks are a SUBSEQUENCE of the input lines,
// in the same relative order. It is a pure function of untrusted git output
// and must never panic. Fuzzed by FuzzParseDiffTreeStdinRawZ.
func parseDiffTreeStdinRawZ(out string) ([]diffTreeBlockZ, error) {
	parts := strings.Split(out, "\x00")
	var blocks []diffTreeBlockZ
	i := 0
	for i < len(parts) {
		f := parts[i]
		if f == "" { // trailing empty after final NUL
			i++
			continue
		}
		if !strings.HasPrefix(f, ":") {
			blocks = append(blocks, diffTreeBlockZ{Header: f})
			i++
			continue
		}
		if len(blocks) == 0 {
			return nil, fmt.Errorf("diff-tree --stdin: meta record %q before any header", f)
		}
		if i+1 >= len(parts) {
			return nil, fmt.Errorf("diff-tree --stdin: dangling record %q", f)
		}
		cur := &blocks[len(blocks)-1]
		cur.Paths = append(cur.Paths, parts[i+1])
		i += 2
	}
	return blocks, nil
}

// DiffNameOnlyBatch returns each branch's write-set versus baseSHA — the same
// paths a per-branch DiffNameOnly(ctx, baseSHA, branch) would return — computed
// in ONE `git diff-tree --stdin` process instead of an N-way fork of `git diff
// --name-only`. The batched replacement for a per-branch DiffNameOnly loop.
//
// The --stdin two-tree form only accepts raw commit OIDs (branch names/refs are
// not resolved there), so every branch is first resolved to its commit SHA in
// ONE `cat-file --batch-check` process (BatchReader) rather than a `git
// rev-parse` fork per branch. An unresolvable branch is a loud error, matching
// DiffNameOnly's per-call failure on a bad ref.
//
// A branch identical to base (no changes) maps to a nil slice, same as
// DiffNameOnly's empty result.
func (g *Git) DiffNameOnlyBatch(ctx context.Context, baseSHA string, branches []string) (map[string][]string, error) {
	result := make(map[string][]string, len(branches))
	if len(branches) == 0 {
		return result, nil
	}
	if strings.ContainsAny(baseSHA, " \t\n") {
		return nil, fmt.Errorf("diff-tree --stdin: illegal whitespace in base %q", baseSHA)
	}

	br, err := g.NewBatchReader(ctx)
	if err != nil {
		return nil, err
	}
	defer br.Close()

	shas := make([]string, len(branches))
	for i, b := range branches {
		sha, rerr := br.ResolveCommit(b)
		if rerr != nil {
			return nil, fmt.Errorf("resolve %q: %w", b, rerr)
		}
		shas[i] = sha
		result[b] = nil // default: no changes vs base unless a block below says otherwise
	}

	var in strings.Builder
	for _, sha := range shas {
		in.WriteString(sha)
		in.WriteByte(' ')
		in.WriteString(baseSHA)
		in.WriteByte('\n')
	}
	// Same flags as diffTreeUnion, minus --no-commit-id: the header is exactly
	// what lets this batched call be segmented back into one result per branch,
	// which the union (which doesn't care which tree a path came from) doesn't need.
	out, se, code, err := g.runWith(ctx, nil, []byte(in.String()),
		"diff-tree", "--stdin", "-r", "-z", "--raw", "--no-renames", "--no-abbrev")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("diff-tree --stdin: exit %d: %s", code, strings.TrimSpace(se))
	}
	blocks, err := parseDiffTreeStdinRawZ(out)
	if err != nil {
		return nil, err
	}

	// Blocks are a subsequence of the input lines in the same relative order
	// (a no-diff line contributes none) — walk both in lockstep rather than
	// keying by header SHA, so two branches that happen to share a commit SHA
	// (e.g. two agents landing byte-identical results) are still both filled in
	// correctly instead of colliding on one map key.
	bi := 0
	for i, b := range branches {
		if bi < len(blocks) && blocks[bi].Header == shas[i] {
			result[b] = blocks[bi].Paths
			bi++
		}
	}
	if bi != len(blocks) {
		return nil, fmt.Errorf("diff-tree --stdin: %d unconsumed block(s); output did not match the %d requested branches", len(blocks)-bi, len(branches))
	}
	return result, nil
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

// NoteAdd attaches content as a git note to commit under a NAMESPACED ref
// (`git notes --ref=<ref> add -f -F - <commit>`), overwriting any note
// already there for that commit. Notes ride with the objects on any host —
// they're an ordinary ref plus commits, just like a branch — which is why
// this is how sigbound attaches structured provenance to a landed commit
// instead of stuffing it into the commit message. ref is a bare name (e.g.
// "sigbound"), never "refs/notes/..." itself; the caller decides the
// namespace, which the caller MUST keep separate from git's own default
// (refs/notes/commits) so a repo's own note usage is never disturbed by
// sigbound's.
func (g *Git) NoteAdd(ctx context.Context, ref, commit string, content []byte) error {
	_, se, code, err := g.runWith(ctx, nil, content, "notes", "--ref="+ref, "add", "-f", "-F", "-", commit)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("notes --ref=%s add %s: exit %d: %s", ref, commit, code, strings.TrimSpace(se))
	}
	return nil
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

// BlobsBatch resolves multiple object specs — each anything `git cat-file`
// accepts, most usefully a "<rev>:<path>" spec (exactly what BlobAt builds as
// rev+":"+path) — to their content, in ONE `git cat-file --batch` process
// instead of one `git cat-file blob <spec>` fork per spec. This is what turns
// the resolver's per-conflicted-path blob reads (3 forks per path: base, ours,
// theirs) into a single spawn for a whole branch's conflicts.
//
// The returned map is keyed by the exact spec string passed in. A spec that
// doesn't resolve (missing object) or resolves to something other than a blob
// (e.g. a tree) is simply absent from the map — result[spec], ok := ...; !ok
// means "absent", the same not-an-error contract as BlobAt's present bool.
func (g *Git) BlobsBatch(ctx context.Context, specs []string) (map[string]string, error) {
	result := make(map[string]string, len(specs))
	if len(specs) == 0 {
		return result, nil
	}
	var in strings.Builder
	for _, s := range specs {
		// cat-file --batch reads one spec per line; a literal newline in a spec
		// would desync the request stream from the response stream. Reject it
		// loudly rather than silently misattribute one path's content to another.
		if strings.Contains(s, "\n") {
			return nil, fmt.Errorf("cat-file --batch: illegal newline in spec %q", s)
		}
		in.WriteString(s)
		in.WriteByte('\n')
	}
	out, se, code, err := g.runWith(ctx, nil, []byte(in.String()), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("cat-file --batch: exit %d: %s", code, strings.TrimSpace(se))
	}
	entries, err := parseCatFileBatch(out)
	if err != nil {
		return nil, err
	}
	// The output is always exactly one record per input line, in order (never
	// a subsequence, unlike --batch-check's missing-only echo) — so a count
	// mismatch means the response desynced from the request; treat that as a
	// loud failure rather than silently returning wrong content for a spec.
	if len(entries) != len(specs) {
		return nil, fmt.Errorf("cat-file --batch: got %d record(s) for %d spec(s)", len(entries), len(specs))
	}
	for i, e := range entries {
		if e.Missing || e.Type != "blob" {
			continue
		}
		result[specs[i]] = e.Content
	}
	return result, nil
}

// catFileBatchEntry is one object's record from `git cat-file --batch`: either
// present (OID/Type/Content filled) or Missing (git could not resolve the
// input spec to an object).
type catFileBatchEntry struct {
	OID     string
	Type    string
	Content string
	Missing bool
}

// parseCatFileBatch decodes `git cat-file --batch` output: one record per
// input line, in the SAME order as the input. Unlike --batch-check, a present
// record's header carries only the resolved OID (not the input spec), so
// matching records back to requests is purely positional — safe here because
// the output is always exactly 1:1 with the input, never a subsequence. A
// present record is "<oid> <type> <size>\n" followed by exactly <size> raw
// content bytes and one trailing '\n'; a missing record is "<input spec>
// missing\n". It is a pure function of untrusted git output and must never
// panic on malformed input (truncated header, bad size, content shorter than
// declared, missing trailing newline). Fuzzed by FuzzParseCatFileBatch.
func parseCatFileBatch(out string) ([]catFileBatchEntry, error) {
	var entries []catFileBatchEntry
	pos := 0
	for pos < len(out) {
		nl := strings.IndexByte(out[pos:], '\n')
		if nl < 0 {
			return nil, fmt.Errorf("cat-file --batch: truncated header %q", out[pos:])
		}
		line := out[pos : pos+nl]
		pos += nl + 1
		if strings.HasSuffix(line, " missing") {
			entries = append(entries, catFileBatchEntry{Missing: true})
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("cat-file --batch: malformed header %q", line)
		}
		size, serr := strconv.ParseInt(fields[2], 10, 64)
		if serr != nil || size < 0 || size > int64(len(out)-pos) {
			return nil, fmt.Errorf("cat-file --batch: bad size in header %q", line)
		}
		n := int(size) // safe: bounded above by len(out)-pos, which fits in int
		content := out[pos : pos+n]
		pos += n
		if pos >= len(out) || out[pos] != '\n' {
			return nil, fmt.Errorf("cat-file --batch: missing trailing newline after content for %q", line)
		}
		pos++
		entries = append(entries, catFileBatchEntry{OID: fields[0], Type: fields[1], Content: content})
	}
	return entries, nil
}

// entryModesBatch returns the file mode of every path in paths within tree, in
// ONE `git ls-tree -z` call instead of one spawn per path — the batched
// replacement for a per-path entryMode loop (SpliceBlobs used to fork one
// `ls-tree` per resolved blob). A path absent from tree is simply missing from
// the returned map, same "" convention as a single entryMode lookup.
func (g *Git) entryModesBatch(ctx context.Context, tree string, paths []string) (map[string]string, error) {
	result := make(map[string]string, len(paths))
	if len(paths) == 0 {
		return result, nil
	}
	args := append([]string{"ls-tree", "-z", tree, "--"}, paths...)
	out, se, code, err := g.runWith(ctx, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("ls-tree %s -- %s: exit %d: %s", tree, strings.Join(paths, " "), code, strings.TrimSpace(se))
	}
	return parseLsTreeZ(out)
}

// parseLsTreeZ decodes MULTI-record `git ls-tree -z <tree> -- <paths...>`
// output into a path -> mode map. Unlike a single-path lookup, git does not
// echo back paths it can't find (a missing path just contributes no record),
// and returns records in TREE order rather than the order paths were
// requested in — so records are matched to callers by the path each record
// itself carries, not by position. It is a pure function of untrusted git
// output and must never panic on malformed input. Fuzzed by FuzzParseLsTreeZ.
func parseLsTreeZ(out string) (map[string]string, error) {
	result := map[string]string{}
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" { // trailing empty after final NUL
			continue
		}
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("ls-tree: malformed record %q", rec)
		}
		fields := strings.Fields(rec[:tab])
		if len(fields) < 1 {
			return nil, fmt.Errorf("ls-tree: malformed record %q", rec)
		}
		result[rec[tab+1:]] = fields[0]
	}
	return result, nil
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

	paths := make([]string, len(blobs))
	for i, rb := range blobs {
		// update-index --index-info is line-oriented (mode SP oid TAB path) with
		// no NUL variant, so a tab/newline in a path would corrupt the stream.
		// Reject it loudly rather than write a wrong tree.
		if strings.ContainsAny(rb.Path, "\t\n") {
			return "", fmt.Errorf("splice: path %q contains tab/newline (unsupported by update-index --index-info)", rb.Path)
		}
		paths[i] = rb.Path
	}
	// One `ls-tree` call for every resolved path's mode instead of a spawn per
	// path (see entryModesBatch).
	modes, err := g.entryModesBatch(ctx, baseTree, paths)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for _, rb := range blobs {
		mode := modes[rb.Path]
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

// BatchBlobReader wraps one long-running `git cat-file --batch` for reading blob
// CONTENT — the persistent counterpart to the per-call BlobsBatch spawn. A cell
// keeps ONE alive for its whole lifetime (see cell.BlobsBatch) so a busy cell —
// semantic analysis over many branches, the review three-sides endpoint hit
// repeatedly, a resolver's conflict reads — resolves blobs without a `git`
// process per operation. It is NOT internally synchronized: the caller (the
// cell) serializes every Read, which it must, because the --batch wire protocol
// is strictly sequential (one request line in, one response record out, in
// order — pipelining two reads would interleave their records).
type BatchBlobReader struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// NewBatchBlobReader starts a `cat-file --batch` bound to this repo. It is
// started on a background context, not any request's ctx, because it outlives
// the call that first needs it (a cell reuses it across operations); its cmd
// carries a WaitDelay so a wedged git cannot hang Close forever. The caller must
// Close it to reap the process.
func (g *Git) NewBatchBlobReader() (*BatchBlobReader, error) {
	cmd := exec.CommandContext(context.Background(), g.bin, "-C", g.dir, "cat-file", "--batch")
	cmd.Env = hermeticEnv()
	cmd.WaitDelay = 2 * time.Second // never let a wedged git hang cell teardown
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
	return &BatchBlobReader{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

// Read resolves each spec to its blob content through the SAME persistent
// process, keyed by the exact spec string — the same map contract as BlobsBatch,
// including that a spec which is missing or resolves to a non-blob (e.g. a tree)
// is simply absent from the map (BlobAt's present=false => empty convention).
//
// A returned error means the wire stream desynced or a pipe failed: the reader's
// position is now unknown, so the caller MUST discard it (Close and, if it still
// wants the answer, fall back to a fresh spawn). It never partially trusts a
// desynced stream — one bad record fails the whole call.
func (br *BatchBlobReader) Read(specs []string) (map[string]string, error) {
	result := make(map[string]string, len(specs))
	for _, s := range specs {
		// One spec per line; a literal newline would desync request from response.
		if strings.Contains(s, "\n") {
			return nil, fmt.Errorf("cat-file --batch: illegal newline in spec %q", s)
		}
		if _, err := io.WriteString(br.stdin, s+"\n"); err != nil {
			return nil, err
		}
		line, err := br.stdout.ReadString('\n')
		if err != nil {
			return nil, err
		}
		// The header grammar is identical to --batch-check's, so reuse its parser:
		// "<oid> <type> <size>" for a present object, "<spec> missing" otherwise.
		_, typ, size, exists, perr := parseBatchCheckLine(line)
		if perr != nil {
			return nil, fmt.Errorf("cat-file --batch: %w", perr)
		}
		if !exists {
			continue // missing spec: absent from the map, not an error
		}
		if size < 0 { // guard make() below; a real object never reports this
			return nil, fmt.Errorf("cat-file --batch: negative size in %q", strings.TrimSpace(line))
		}
		buf := make([]byte, size+1) // content + git's trailing newline
		if _, err := io.ReadFull(br.stdout, buf); err != nil {
			return nil, err
		}
		if buf[size] != '\n' {
			return nil, fmt.Errorf("cat-file --batch: missing trailing newline after %q", strings.TrimSpace(line))
		}
		if typ == "blob" {
			result[s] = string(buf[:size])
		}
	}
	return result, nil
}

// Close shuts the pipe and reaps the process. Safe to call more than once (a
// second Wait just returns an already-exited error, which the caller ignores).
func (br *BatchBlobReader) Close() error {
	_ = br.stdin.Close()
	return br.cmd.Wait()
}
