package gitx

// Native-fuzzing targets for every parser of git command OUTPUT in this package.
// A parser of untrusted plumbing output must NEVER panic on malformed input — a
// crasher here would be a real bug. Each target refactors the parse into a pure
// helper (parseDiffRawZ, parseMergeTreeZ, parseBatchCheckLine, parseLsTreeModeZ)
// so it can be exercised without shelling git, and asserts the parser's own
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

// FuzzParseLsTreeModeZ fuzzes the single-record `git ls-tree -z` mode decoder.
func FuzzParseLsTreeModeZ(f *testing.F) {
	f.Add("")
	f.Add("\x00")
	f.Add("100644 blob " + sha1a + "\ta.go\x00")
	f.Add("100755 blob " + sha1a + "\tx\x00")
	f.Add("040000 tree " + sha1a + "\tsub")
	f.Add("no-tab-at-all record")
	f.Add("\t")                                      // tab only -> empty fields before tab
	f.Add("  \tpath")                                // whitespace-only mode field
	f.Add("120000 blob " + sha1a + "\t\xff\xfe\x00") // non-UTF8 path

	f.Fuzz(func(t *testing.T, out string) {
		mode, err := parseLsTreeModeZ(out)
		if err != nil {
			return
		}
		// On success mode is either "" (path absent) or the first whitespace-split
		// field before the tab: it can never contain whitespace or a NUL.
		if strings.ContainsAny(mode, " \t\n\x00") {
			t.Fatalf("mode carries whitespace/NUL: %q", mode)
		}
	})
}
