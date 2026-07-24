package gitx

// Native-fuzzing targets for every parser of git command OUTPUT in this package.
// A parser of untrusted plumbing output must NEVER panic on malformed input — a
// crasher here would be a real bug. Each target refactors the parse into a pure
// helper (parseDiffRawZ, parseMergeTreeZ, parseBatchCheckLine, parseLsTreeZ,
// parseLsTreeSizesZ, parseCatFileBatch) so it can be exercised without shelling git, and asserts the parser's own
// structural invariants in addition to "did not panic". The seed corpora carry
// real valid outputs plus known-tricky inputs (empty, truncated, embedded NULs,
// missing/extra fields, non-UTF8) and any crasher found is committed under
// testdata/fuzz as a permanent regression seed.

import (
	"strings"
	"testing"
)

const (
	sha1a = "1111111111111111111111111111111111111111"
	sha1b = "2222222222222222222222222222222222222222"
)

// FuzzParseDiffRawZ fuzzes the `git diff --raw -z --no-renames` decoder.
func FuzzParseDiffRawZ(f *testing.F) {
	f.Add("")
	f.Add("\x00")
	f.Add(":100644 100644 " + sha1a + " " + sha1b + " M\x00a.go\x00")
	f.Add(":100644 000000 " + sha1a + " " + zeroOID + " D\x00gone.txt\x00")
	f.Add(":100755 100755 " + sha1a + " " + sha1b + " M\x00x\x00:120000 120000 " + sha1a + " " + sha1b + " M\x00link\x00")
	f.Add(":100644 100644 " + sha1a + " " + sha1b + " M\x00dangling-no-path") // dangling record
	f.Add(":onlytwo fields\x00path\x00")                                      // too few fields
	f.Add(":a b c d e f g\x00weird path with spaces\x00")                     // extra fields
	f.Add("\x00\x00\x00\x00")                                                 // all NULs
	f.Add(":100644 100644 " + sha1a + " " + sha1b + " M\x00\xff\xfe\x00")     // non-UTF8 path

	f.Fuzz(func(t *testing.T, out string) {
		ents, err := parseDiffRawZ(out)
		if err != nil {
			return // a rejected malformed record is fine, as long as it did not panic
		}
		for _, e := range ents {
			// Fields come from segments split on NUL / whitespace, so they can
			// never themselves contain a NUL. A violation means the parser mis-sliced.
			if strings.ContainsRune(e.Mode, 0) || strings.ContainsRune(e.OID, 0) || strings.ContainsRune(e.Path, 0) {
				t.Fatalf("parsed entry carries an embedded NUL: %+v", e)
			}
			// Deleted() must be a pure function of OID and never panic.
			_ = e.Deleted()
		}
	})
}

// FuzzParseMergeTreeZ fuzzes the `git merge-tree --write-tree -z --name-only`
// decoder in both the clean (exit 0) and conflicted (exit 1) interpretations.
func FuzzParseMergeTreeZ(f *testing.F) {
	f.Add("", false)
	f.Add(sha1a, false)                                   // clean: bare tree OID
	f.Add(sha1a+"\x00a.go\x00b.go\x00", true)             // conflict: two paths
	f.Add(sha1a+"\x00a.go\x00a.go\x00\x00trailing", true) // duplicate + empty terminator
	f.Add("\x00", true)                                   // empty tree, empty conflict list
	f.Add("\x00\x00\x00", true)                           // all-empty fields
	f.Add(sha1a+"\x00\xff\xfe\x00", true)                 // non-UTF8 conflict path
	f.Add("  "+sha1a+"  \x00x\x00", true)                 // whitespace around OID

	f.Fuzz(func(t *testing.T, stdout string, conflicted bool) {
		tree, conflicts := parseMergeTreeZ(stdout, conflicted)
		// The tree OID is field 0 up to the first NUL, then trimmed: never a NUL.
		if strings.ContainsRune(tree, 0) {
			t.Fatalf("tree OID carries an embedded NUL: %q", tree)
		}
		seen := map[string]bool{}
		for _, c := range conflicts {
			if c == "" {
				t.Fatalf("conflict list contains an empty path (should terminate the list)")
			}
			if strings.ContainsRune(c, 0) {
				t.Fatalf("conflict path carries an embedded NUL: %q", c)
			}
			if seen[c] {
				t.Fatalf("conflict list is not deduplicated: %q repeats", c)
			}
			seen[c] = true
		}
		if !conflicted && conflicts != nil {
			t.Fatalf("clean merge must report no conflicts, got %v", conflicts)
		}
	})
}

// FuzzParseDiffTreeStdinRawZ fuzzes the `git diff-tree --stdin -z --raw`
// decoder (commit-id headers kept) that DiffNameOnlyBatch uses to segment one
// combined run back into one write-set per branch.
func FuzzParseDiffTreeStdinRawZ(f *testing.F) {
	f.Add("")
	f.Add("\x00")
	f.Add(sha1a + "\x00")                                                         // header, no changes recorded (shouldn't happen in practice, but must not panic)
	f.Add(sha1a + "\x00:100644 100644 " + sha1a + " " + sha1b + " M\x00a.go\x00") // one header, one entry
	f.Add(sha1a + "\x00:100644 100644 " + sha1a + " " + sha1b + " M\x00a.go\x00" +
		sha1b + "\x00:100644 000000 " + sha1a + " " + zeroOID + " D\x00b.go\x00") // two headers back to back
	f.Add(":100644 100644 " + sha1a + " " + sha1b + " M\x00dangling-meta-before-header")  // meta with no preceding header
	f.Add(sha1a + "\x00:onlytwo fields\x00path\x00")                                      // header + malformed meta (still just a marker to us)
	f.Add(sha1a + "\x00:100644 100644 " + sha1a + " " + sha1b + " M\x00dangling-no-path") // dangling record after header
	f.Add("\x00\x00\x00\x00")                                                             // all NULs
	f.Add(sha1a + "\x00:100644 100644 " + sha1a + " " + sha1b + " M\x00\xff\xfe\x00")     // non-UTF8 path

	f.Fuzz(func(t *testing.T, out string) {
		blocks, err := parseDiffTreeStdinRawZ(out)
		if err != nil {
			return // a rejected malformed record is fine, as long as it did not panic
		}
		seen := map[string]int{}
		for _, b := range blocks {
			// A header is whatever field started the block; the parser's only
			// invariant on it is "did not start with ':'" (checked before it's
			// ever stored), so re-check that here too.
			if strings.HasPrefix(b.Header, ":") {
				t.Fatalf("block header %q looks like a meta record, not a header", b.Header)
			}
			seen[b.Header]++
			for _, p := range b.Paths {
				_ = p // paths are whatever followed a ':' meta record; any string is valid, just must not panic downstream
			}
		}
	})
}

// FuzzParseBatchCheckLine fuzzes the `git cat-file --batch-check` line decoder.
func FuzzParseBatchCheckLine(f *testing.F) {
	f.Add("")
	f.Add("\n")
	f.Add(sha1a + " blob 1234\n")
	f.Add(sha1a + " commit 42")
	f.Add("deadbeef missing\n")
	f.Add("some ref name that is missing")
	f.Add(sha1a + " blob notanumber")                  // bad size
	f.Add(sha1a + " blob 999999999999999999999999999") // size overflow
	f.Add("only two")
	f.Add("a b c d e")        // too many fields
	f.Add("   \t  \n")        // whitespace only
	f.Add(sha1a + " blob -5") // negative size

	f.Fuzz(func(t *testing.T, line string) {
		oid, typ, size, exists, err := parseBatchCheckLine(line)
		if exists {
			// A present object must carry a nil error and non-empty oid+type with
			// no interior whitespace (they came from strings.Fields).
			if err != nil {
				t.Fatalf("exists=true but err=%v", err)
			}
			if oid == "" || typ == "" {
				t.Fatalf("exists=true but oid=%q typ=%q", oid, typ)
			}
			if strings.ContainsAny(oid, " \t") || strings.ContainsAny(typ, " \t") {
				t.Fatalf("field carries whitespace: oid=%q typ=%q", oid, typ)
			}
		} else {
			// Not present => nothing meaningful returned; err may or may not be set.
			if oid != "" || typ != "" || size != 0 {
				t.Fatalf("exists=false but oid=%q typ=%q size=%d", oid, typ, size)
			}
		}
	})
}

// FuzzParseGitVersion fuzzes the `git version` output decoder that gates
// CheckMinVersion (and so every `sig run`/`sig integrate` preflight, plus
// `sig doctor`) on untrusted external command output.
func FuzzParseGitVersion(f *testing.F) {
	f.Add("git version 2.39.3 (Apple Git-146)")
	f.Add("git version 2.38.0")
	f.Add("git version 2.43.0.windows.1")
	f.Add("git version 2.30.2.msysgit.0")
	f.Add("")
	f.Add("git version")
	f.Add("git version \n")
	f.Add("not git at all")
	f.Add("2")
	f.Add("git version -1.-2")
	f.Add("git version 99999999999999999999.0")
	f.Add("\x00\xff\xfe")

	f.Fuzz(func(t *testing.T, out string) {
		major, minor, err := ParseGitVersion(out)
		if err != nil {
			return // rejecting unparseable/malformed output is fine, must not panic
		}
		if major < 0 || minor < 0 {
			t.Fatalf("ParseGitVersion(%q) = %d.%d, want non-negative", out, major, minor)
		}
	})
}

// FuzzParseUnbundleRefs fuzzes the `git bundle unbundle` stdout decoder that
// BundleUnbundle uses to learn which refs a bundle carried — untrusted output
// that cell.Import then maps into its namespace, so a crasher (panic) or an
// accepted garbage line would be a real bug at the transport trust boundary.
func FuzzParseUnbundleRefs(f *testing.F) {
	sha256a := strings.Repeat("a", 64)
	f.Add("")
	f.Add("\n")
	f.Add(sha1a + " refs/heads/agent/t1\n")
	f.Add(sha1a + " refs/heads/agent/t1\n" + sha1b + " refs/heads/agent/t2\n")
	f.Add(sha256a + " refs/heads/main\n")                          // sha256 oid
	f.Add(sha1a + " refs/tags/v1\n")                               // a tag ref
	f.Add("  " + sha1a + "   refs/heads/x  \n")                    // surrounding whitespace
	f.Add(sha1a + "\n")                                            // oid with no ref (malformed)
	f.Add("refs/heads/x\n")                                        // ref with no oid (malformed)
	f.Add("notanoid refs/heads/x\n")                               // non-hex oid
	f.Add(sha1a + " refs/heads/x extra\n")                         // ref field with a space
	f.Add("Receiving objects: 100%\n" + sha1a + " refs/heads/x\n") // progress-like noise line
	f.Add(sha1a + " \xff\xfe\n")                                   // non-UTF8 ref

	f.Fuzz(func(t *testing.T, out string) {
		refs, err := parseUnbundleRefs(out)
		if err != nil {
			return // rejecting a malformed line is fine, as long as it did not panic
		}
		for _, r := range refs {
			if !isHexOID(r.OID) {
				t.Fatalf("accepted ref with non-oid %q", r.OID)
			}
			if r.Ref == "" || strings.ContainsAny(r.Ref, " \t\n") {
				t.Fatalf("accepted ref name with whitespace or empty: %q", r.Ref)
			}
		}
	})
}

// FuzzParseLsTreeZ fuzzes the MULTI-record `git ls-tree -z <tree> --
// <paths...>` decoder that entryModesBatch uses to collapse SpliceBlobs'
// per-path mode lookups into one spawn.
func FuzzParseLsTreeZ(f *testing.F) {
	f.Add("")
	f.Add("\x00")
	f.Add("100644 blob " + sha1a + "\ta.go\x00")
	f.Add("100644 blob " + sha1a + "\ta.go\x00100755 blob " + sha1b + "\tb.sh\x00")
	f.Add("040000 tree " + sha1a + "\tsub\x00")
	f.Add("no-tab-at-all record\x00")
	f.Add("\t\x00")                                                               // tab only -> empty fields before tab
	f.Add("  \tpath\x00")                                                         // whitespace-only mode field
	f.Add("120000 blob " + sha1a + "\t\xff\xfe\x00")                              // non-UTF8 path
	f.Add("100644 blob " + sha1a + "\tdup\x00100644 blob " + sha1b + "\tdup\x00") // duplicate path

	f.Fuzz(func(t *testing.T, out string) {
		modes, err := parseLsTreeZ(out)
		if err != nil {
			return // a rejected malformed record is fine, as long as it did not panic
		}
		for path, mode := range modes {
			if mode == "" {
				t.Fatalf("path %q carries an empty mode (should have been rejected)", path)
			}
			if strings.ContainsAny(mode, " \t\n\x00") {
				t.Fatalf("mode for %q carries whitespace/NUL: %q", path, mode)
			}
		}
	})
}

// FuzzParseLsTreeSizesZ fuzzes the `git ls-tree -r -l -z <rev>` decoder that
// TreeSize sums into a disk-space preflight estimate (see cmd/sig's
// diskPreflight and `sig doctor`'s disk line).
func FuzzParseLsTreeSizesZ(f *testing.F) {
	f.Add("")
	f.Add("\x00")
	f.Add("100644 blob " + sha1a + "     582\t.github/FUNDING.yml\x00")
	f.Add("100644 blob " + sha1a + "       0\ta.go\x00100755 blob " + sha1b + "     123\tb.sh\x00")
	f.Add("160000 commit " + sha1a + "       -\tsubmod\x00") // submodule: size "-"
	f.Add("no-tab-at-all record\x00")
	f.Add("\t\x00")                                                               // tab only -> empty fields before tab
	f.Add("100644 blob " + sha1a + "  notanumber\tbad.txt\x00")                   // non-numeric size
	f.Add("100644 blob " + sha1a + "  -5\tneg.txt\x00")                           // negative size
	f.Add("100644 blob " + sha1a + "  999999999999999999999999999\thuge.txt\x00") // size overflow
	f.Add("100644 blob " + sha1a + "  582\t\xff\xfe\x00")                         // non-UTF8 path

	f.Fuzz(func(t *testing.T, out string) {
		total, err := parseLsTreeSizesZ(out)
		if err != nil {
			return // a rejected malformed record is fine, as long as it did not panic
		}
		if total < 0 {
			t.Fatalf("parseLsTreeSizesZ(%q) = %d, want non-negative", out, total)
		}
	})
}

// FuzzParseWorktreeListPorcelain fuzzes the `git worktree list --porcelain`
// decoder `sig gc` relies on to find stale worktree registrations.
func FuzzParseWorktreeListPorcelain(f *testing.F) {
	f.Add("")
	f.Add("worktree /repo\nHEAD " + sha1a + "\nbranch refs/heads/main\n\n")
	f.Add("worktree /repo\nHEAD " + sha1a + "\nbranch refs/heads/main\n\n" +
		"worktree /repo-wt\nHEAD " + sha1b + "\ndetached\nprunable gitdir file points to non-existent location\n\n")
	f.Add("prunable no worktree line came first\n")               // annotation before any "worktree" line
	f.Add("worktree\n")                                           // "worktree" with no path (no trailing space)
	f.Add("worktree \n")                                          // empty path
	f.Add("worktree /a\nprunable\n")                              // prunable with no reason text
	f.Add("worktree /a\n\nprunable after blank line, orphaned\n") // annotation after record already closed
	f.Add("worktree /a\nworktree /b\n")                           // two worktree lines, no blank line between
	f.Add("worktree /\xff\xfe\n")                                 // non-UTF8 path

	f.Fuzz(func(t *testing.T, out string) {
		entries := parseWorktreeListPorcelain(out)
		for _, e := range entries {
			// Every record must have started with a "worktree " line, so Path
			// can never itself contain a newline (it's exactly one line's tail).
			if strings.ContainsRune(e.Path, '\n') || strings.ContainsRune(e.Prunable, '\n') {
				t.Fatalf("parsed entry carries an embedded newline: %+v", e)
			}
		}
	})
}

// FuzzParseForEachRefCommit fuzzes the `git for-each-ref` decoder that `sig
// gc` uses to age-gate agent/*/imported/*/* branches.
func FuzzParseForEachRefCommit(f *testing.F) {
	f.Add("")
	f.Add("agent/t1\t1577854800\t" + sha1a)
	f.Add("agent/t1\t1577854800\t" + sha1a + "\nimported/w1/agent/t2\t1784905357\t" + sha1b)
	f.Add("too\tfew")                                  // too few fields
	f.Add("a\tb\tc\td")                                // too many fields
	f.Add("agent/t1\tnotanumber\t" + sha1a)            // bad committerdate
	f.Add("agent/t1\t-99999999999999999999\t" + sha1a) // committerdate overflow
	f.Add("agent/t1\t\t" + sha1a)                      // empty committerdate field
	f.Add("\xff\xfe\t123\t" + sha1a)                   // non-UTF8 name

	f.Fuzz(func(t *testing.T, out string) {
		refs, err := parseForEachRefCommit(out)
		if err != nil {
			return // a rejected malformed record is fine, as long as it did not panic
		}
		for _, r := range refs {
			if strings.ContainsRune(r.Name, '\t') || strings.ContainsRune(r.SHA, '\t') {
				t.Fatalf("parsed ref carries an embedded tab: %+v", r)
			}
		}
	})
}

// FuzzParseCatFileBatch fuzzes the `git cat-file --batch` decoder that
// BlobsBatch uses to collapse the resolver's per-conflict blob reads (3
// `cat-file blob` forks per path) into one spawn for a whole branch.
func FuzzParseCatFileBatch(f *testing.F) {
	f.Add("")
	f.Add(sha1a + " blob 5\nhello\n")
	f.Add(sha1a + " blob 0\n\n") // empty blob
	f.Add("HEAD:missing.txt missing\n")
	f.Add(sha1a + " blob 5\nhello\n" + "HEAD:missing.txt missing\n" + sha1b + " blob 3\nfoo\n")
	f.Add(sha1a + " blob 5\nhel\x00o\n")     // embedded NUL in content
	f.Add(sha1a + " blob notanumber\nx\n")   // bad size
	f.Add(sha1a + " blob 999999999999\nx\n") // size far exceeds remaining data
	f.Add(sha1a + " blob 5\nhello")          // missing trailing newline after content
	f.Add(sha1a + " blob 5")                 // truncated: header only, no content
	f.Add(sha1a + " blob -1\nx\n")           // negative size
	f.Add(sha1a + " tree 9\nnottext\n")      // non-blob type (caller filters, parser must still decode)
	f.Add("only two\n")                      // malformed header

	f.Fuzz(func(t *testing.T, out string) {
		entries, err := parseCatFileBatch(out)
		if err != nil {
			return // a rejected malformed record is fine, as long as it did not panic
		}
		for _, e := range entries {
			if e.Missing {
				if e.OID != "" || e.Type != "" || e.Content != "" {
					t.Fatalf("missing entry carries data: %+v", e)
				}
				continue
			}
			if e.OID == "" || e.Type == "" {
				t.Fatalf("present entry missing oid/type: %+v", e)
			}
		}
	})
}
