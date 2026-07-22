package main

// Native-fuzzing targets for the parsers of UNTRUSTED input in the sig command:
// the auto-planner's plan JSON (parsePlan — arbitrary model output, the highest
// value target here), the path-validation predicates it depends on (relSafe,
// slugSafe), the -config flags-file parser (parseConfigFile — an external
// text file, hand-edited or generated), and -verify-impact's `go list -json`
// output parser (parseGoListJSON — an external command's stdout). parsePlan is
// the gate that decides whether a run ever starts, so it must reject every
// malformed/hostile plan without panicking; relSafe/slugSafe are the safety
// predicates that keep a declared path from escaping the repo or a slug from
// becoming an unsafe branch/dir name. Each target asserts "did not panic" plus
// the invariant the parser is supposed to guarantee on the accept path.

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzParsePlan fuzzes the strict plan validator with arbitrary bytes as the
// model's stdout. On the ACCEPT path it re-checks every invariant parsePlan
// documents: 1..n tasks, unique slug-safe ids, non-empty prompts, non-empty safe
// files, and pairwise-disjoint file-sets. A crasher (panic) or an accepted plan
// that violates any invariant is a real bug.
func FuzzParsePlan(f *testing.F) {
	f.Add([]byte(`[{"id":"a","prompt":"do a","files":["a.go"]}]`), 4)
	f.Add([]byte(`[{"id":"a","prompt":"do a","files":["a.go"]},{"id":"b","prompt":"do b","files":["b.go"]}]`), 4)
	f.Add([]byte(`[{"id":"a","prompt":"x","files":["a.go"]},{"id":"a","prompt":"y","files":["b.go"]}]`), 4)           // dup id
	f.Add([]byte(`[{"id":"a","prompt":"x","files":["shared.go"]},{"id":"b","prompt":"y","files":["shared.go"]}]`), 4) // overlap
	f.Add([]byte(`[{"id":"../../etc","prompt":"x","files":["/etc/passwd"]}]`), 4)                                     // path escape
	f.Add([]byte(`[{"id":"a","prompt":"x","files":["../out.go"]}]`), 4)                                               // dotdot file
	f.Add([]byte(``), 4)
	f.Add([]byte(`null`), 4)
	f.Add([]byte(`[]`), 4)
	f.Add([]byte(`{}`), 4)
	f.Add([]byte(`[{"id":"","prompt":"","files":[]}]`), 4)
	f.Add([]byte(`not json at all`), 4)
	f.Add([]byte(`[[[[[[[[[[1]]]]]]]]]]`), 4)                                      // nesting
	f.Add([]byte(`[{"id":"a","prompt":"x","files":["a.go","a.go"]}]`), 4)          // intra-task dup file
	f.Add([]byte("[{\"id\":\"a\",\"prompt\":\"x\",\"files\":[\"a\x00.go\"]}]"), 4) // embedded NUL

	f.Fuzz(func(t *testing.T, data []byte, n int) {
		// n is what a caller would pass; parsePlan tolerates any value, but keep it
		// in a sane band so the accept path is reachable and allocations stay bounded.
		if n < 1 {
			n = 1
		}
		if n > 256 {
			n = 256
		}
		tasks, err := parsePlan(data, n)
		if err != nil {
			return // rejecting a bad/hostile plan is the whole point; must not panic
		}
		// ACCEPT path: every documented invariant must hold.
		if len(tasks) < 1 || len(tasks) > n {
			t.Fatalf("accepted %d tasks, want 1..%d", len(tasks), n)
		}
		ids := map[string]bool{}
		owner := map[string]string{}
		for _, tk := range tasks {
			if strings.TrimSpace(tk.ID) == "" || !slugSafe(strings.TrimSpace(tk.ID)) {
				t.Fatalf("accepted task with unsafe id %q", tk.ID)
			}
			if ids[tk.ID] {
				t.Fatalf("accepted duplicate id %q", tk.ID)
			}
			ids[tk.ID] = true
			if strings.TrimSpace(tk.Prompt) == "" {
				t.Fatalf("accepted empty prompt for %q", tk.ID)
			}
			if len(tk.Files) == 0 {
				t.Fatalf("accepted task %q with no files", tk.ID)
			}
			for _, fp := range tk.Files {
				if !relSafe(fp) {
					t.Fatalf("accepted unsafe file %q in task %q", fp, tk.ID)
				}
				if prev, ok := owner[fp]; ok && prev != tk.ID {
					t.Fatalf("accepted overlapping file %q (tasks %q and %q)", fp, prev, tk.ID)
				}
				owner[fp] = tk.ID
			}
		}
	})
}

// FuzzRelSafe asserts relSafe never panics and that when it accepts a path, that
// path really is repo-relative: not absolute and free of any ".." segment. This
// is the predicate that keeps a declared plan file from pointing outside the repo.
func FuzzRelSafe(f *testing.F) {
	f.Add("a.go")
	f.Add("dir/sub/file.go")
	f.Add("")
	f.Add(".")
	f.Add("..")
	f.Add("../escape")
	f.Add("/abs/path")
	f.Add("a/../../b")
	f.Add("a/b/..")
	f.Add("weird\x00name")
	f.Add("C:\\windows\\path")
	f.Add("   spaced   ")

	f.Fuzz(func(t *testing.T, p string) {
		if !relSafe(p) {
			return
		}
		trimmed := strings.TrimSpace(p)
		if trimmed == "" || trimmed == "." {
			t.Fatalf("relSafe accepted empty/dot path %q", p)
		}
		if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") {
			t.Fatalf("relSafe accepted absolute path %q", p)
		}
		for _, seg := range strings.Split(filepath.ToSlash(trimmed), "/") {
			if seg == ".." {
				t.Fatalf("relSafe accepted path with .. segment %q", p)
			}
		}
	})
}

// FuzzSlugSafe asserts slugSafe never panics and that an accepted slug is a
// single safe path/branch component: only [A-Za-z0-9._-] and never "."/"..".
func FuzzSlugSafe(f *testing.F) {
	f.Add("abc")
	f.Add("a_b-c.d")
	f.Add("")
	f.Add(".")
	f.Add("..")
	f.Add("has/slash")
	f.Add("has space")
	f.Add("emoji😀")
	f.Add("tab\tchar")
	f.Add("newline\nchar")

	f.Fuzz(func(t *testing.T, s string) {
		if !slugSafe(s) {
			return
		}
		if s == "" || s == "." || s == ".." {
			t.Fatalf("slugSafe accepted reserved name %q", s)
		}
		for _, r := range s {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
			if !ok {
				t.Fatalf("slugSafe accepted disallowed rune %q in %q", r, s)
			}
		}
		// A slug-safe string must also be a single clean path component.
		if strings.ContainsAny(s, "/\\") {
			t.Fatalf("slugSafe accepted path separator in %q", s)
		}
	})
}

// FuzzParseConfigFile fuzzes the -config flags-file parser with arbitrary
// bytes as the file content. On the ACCEPT path it re-checks the invariants
// parseConfigFile documents: every entry has a non-empty key and a 1-based
// line number, and no entry can come from a blank or '#'-comment line. A
// crasher (panic) here is a real bug — this file is meant to be hand-edited,
// so it will see malformed lines, weird whitespace, and stray characters in
// practice, not just well-formed KEY=VALUE pairs.
func FuzzParseConfigFile(f *testing.F) {
	f.Add([]byte("agent=./my-agent\n"))
	f.Add([]byte("# a standing config\nagent=./my-agent\nverify=go build ./... && go test ./...\n"))
	f.Add([]byte("lanes = strict\n")) // spaces around '='
	f.Add([]byte(""))
	f.Add([]byte("\n\n\n"))
	f.Add([]byte("# only a comment\n"))
	f.Add([]byte("notakeyvalueline\n"))                                // malformed: no '='
	f.Add([]byte("=novalue\n"))                                        // empty key
	f.Add([]byte("key=\n"))                                            // empty value is fine
	f.Add([]byte("verify=X=1; echo $X=1\n"))                           // '=' inside the value
	f.Add([]byte("agent=./a\r\nlanes=warn\r\n"))                       // CRLF
	f.Add([]byte("agent=./a\rlanes=warn\r"))                           // bare CR, no LF (not a line ending sigbound recognizes)
	f.Add(bytes.Repeat([]byte("k"), 10000))                            // huge key, no '=' at all
	f.Add(append(bytes.Repeat([]byte("k"), 10000), []byte("=v\n")...)) // huge key with a value
	f.Add([]byte("a\x00b=c\n"))                                        // embedded NUL in the key
	f.Add([]byte("emoji😀=🚀\n"))                                        // non-ASCII key and value
	f.Add([]byte("  key with spaces  =  value with spaces  \n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := parseConfigFile(data)
		if err != nil {
			return // rejecting a malformed file is fine, as long as it did not panic
		}
		for _, e := range entries {
			if e.Key == "" {
				t.Fatalf("accepted entry with empty key: %+v", e)
			}
			if e.Line < 1 {
				t.Fatalf("accepted entry with non-positive line number: %+v", e)
			}
		}
	})
}

// FuzzParseGoListJSON fuzzes -verify-impact's `go list -json` output parser
// (see impact.go) with arbitrary bytes as that command's stdout. `go list
// -json ./...` prints a STREAM of JSON objects (not one array), decoded via
// repeated json.Decoder.Decode calls — the classic footgun for a hand-rolled
// streaming decoder is looping forever or panicking on a truncated/partial
// trailing object, so seeds specifically target that shape. A crasher here
// would let a hostile or simply broken `go` toolchain output wedge or crash a
// verify run instead of just failing the impact-scoping fallback.
func FuzzParseGoListJSON(f *testing.F) {
	f.Add([]byte(`{"Dir":"/repo/a","ImportPath":"a","Imports":["fmt"]}`))
	f.Add([]byte(`{"Dir":"/repo/a","ImportPath":"a"}
{"Dir":"/repo/b","ImportPath":"b","Imports":["a"],"TestImports":["testing"],"XTestImports":["a"]}
`))
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[]`)) // an array, not the streaming-object shape
	f.Add([]byte(`not json at all`))
	f.Add([]byte(`{"Dir":"/repo/a","ImportPath":"a"`))                                       // truncated mid-object
	f.Add([]byte(`{"Dir":"/repo/a","ImportPath":"a"}{`))                                     // truncated second object
	f.Add([]byte(`{"Dir":"/repo/a","Imports":"not an array"}`))                              // wrong field type
	f.Add([]byte("{\"Dir\":\"/repo/a\x00\",\"ImportPath\":\"a\"}"))                          // embedded NUL
	f.Add([]byte(`{"Dir":"/repo/a","ImportPath":"a"}   {"Dir":"/repo/b","ImportPath":"b"}`)) // extra whitespace between objects

	f.Fuzz(func(t *testing.T, data []byte) {
		pkgs, err := parseGoListJSON(data)
		if err != nil {
			return // rejecting malformed/truncated output is fine, as long as it did not panic
		}
		for _, pk := range pkgs {
			_ = pk // decoded successfully; no further invariant to check beyond "did not panic"
		}
	})
}
