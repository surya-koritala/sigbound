package main

// Native-fuzzing targets for the parsers of UNTRUSTED input in the sig command:
// the auto-planner's plan JSON (parsePlan — arbitrary model output, the highest
// value target here) and the path-validation predicates it depends on (relSafe,
// slugSafe). parsePlan is the gate that decides whether a run ever starts, so it
// must reject every malformed/hostile plan without panicking; relSafe/slugSafe are
// the safety predicates that keep a declared path from escaping the repo or a slug
// from becoming an unsafe branch/dir name. Each target asserts "did not panic"
// plus the invariant the parser is supposed to guarantee on the accept path.

import (
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
