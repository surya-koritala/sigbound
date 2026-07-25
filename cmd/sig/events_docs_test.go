package main

// The event vocabulary has no allowlist in code (issue #140): docs/USAGE.md's
// Events tables are the only written description of a public surface — `-events`,
// `GET /runs/{id}/events`, and the daemon's own watch stream. These tests ARE
// that allowlist: they hold the tables and the emitters to one set, in both
// directions, because either drift is a lie about a surface others integrate
// against.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// usageEventRow matches a table row whose first cell is exactly one backticked
// event name, optionally daggered as post-run. Header and separator rows do not
// match, and neither does any row of the flag or action-input tables (their
// first cells carry a leading `-` or a hyphenated name).
var usageEventRow = regexp.MustCompile("^\\|\\s*`([a-z_]+)`\\s*(†?)\\s*\\|")

// docEventNames returns every event documented under a heading titled "Events"
// — both the run table and `sig watch`'s cycle table — plus the subset marked
// †. Bounding on the heading, not on the row shape alone, is what keeps the
// other backtick-first-cell tables out of the vocabulary.
func docEventNames(t *testing.T) (all, postRun map[string]bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "USAGE.md"))
	if err != nil {
		t.Fatalf("read USAGE.md: %v", err)
	}
	all, postRun = map[string]bool{}, map[string]bool{}
	inEvents := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "#") {
			inEvents = strings.TrimSpace(strings.TrimLeft(line, "#")) == "Events"
			continue
		}
		if !inEvents {
			continue
		}
		m := usageEventRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// Sets hide a duplicated row, so reject it here: two rows for one event
		// are two descriptions that can disagree.
		if all[m[1]] {
			t.Fatalf("docs/USAGE.md documents %q twice", m[1])
		}
		all[m[1]] = true
		if m[2] != "" {
			postRun[m[1]] = true
		}
	}
	return all, postRun
}

// codeEventNames returns every event name this package can emit, split by
// whether it is written while the run is live (eventEmitter.emit) or appended
// to a finished run's events.ndjson afterwards (appendRunEvent). Every .go file
// is parsed with build tags ignored, so a platform-gated event still owes a row.
//
// A name must be a string literal AT the call site: one reaching an emitter
// through a variable is a fatal error here, not a silent omission, since a
// vocabulary this test cannot read is one it cannot hold to the docs.
//
// CEILING: it finds the two emitters by name. A future THIRD writer of
// events.ndjson would be invisible to it. Renaming or bypassing either of these
// two is not — the names then go missing from the code set and surface below as
// documented-but-unemitted, which is why the comparison runs in both directions.
func codeEventNames(t *testing.T) (inRun, postRun map[string]bool) {
	t.Helper()
	inRun, postRun = map[string]bool{}, map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			// appendRunEvent's own emit forwards its caller's name; the names
			// live at ITS call sites, which this loop reaches like any other.
			if !ok || fn.Body == nil || fn.Name.Name == "appendRunEvent" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var into map[string]bool
				var arg int
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr: // <emitter>.emit(name, fields)
					if fun.Sel.Name == "emit" {
						into, arg = inRun, 0
					}
				case *ast.Ident: // appendRunEvent(dir, name, fields)
					if fun.Name == "appendRunEvent" {
						into, arg = postRun, 1
					}
				}
				if into == nil || arg >= len(call.Args) {
					return true
				}
				lit, ok := call.Args[arg].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s: event name is not a string literal — keep it at the call site, or docs/USAGE.md's Events table can no longer be held to the code",
						fset.Position(call.Args[arg].Pos()))
					return false
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: unquote event name: %v", fset.Position(lit.Pos()), err)
					return false
				}
				into[s] = true
				return true
			})
		}
	}
	// An extractor that finds nothing agrees with an empty docs table, so say
	// so loudly instead of passing vacuously: no events found means this test
	// went blind, never that the code stopped emitting.
	if len(inRun)+len(postRun) == 0 {
		t.Fatalf("no event names found in the package source — the emitters this test reads (eventEmitter.emit, appendRunEvent) were renamed or replaced")
	}
	return inRun, postRun
}

// missingFrom returns the sorted members of a that b lacks.
func missingFrom(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// TestEventVocabularyMatchesUsageDocs fails in BOTH directions: an event added
// without a row is a public surface nothing describes (integrations then get
// written against guesswork), and a row for an event nothing emits is a promise
// a consumer can wait on forever.
func TestEventVocabularyMatchesUsageDocs(t *testing.T) {
	docs, _ := docEventNames(t)
	inRun, postRun := codeEventNames(t)
	code := map[string]bool{}
	for n := range inRun {
		code[n] = true
	}
	for n := range postRun {
		code[n] = true
	}
	if missing := missingFrom(code, docs); len(missing) > 0 {
		t.Errorf("emitted but undocumented: %v — add a row to docs/USAGE.md's Events table", missing)
	}
	if stale := missingFrom(docs, code); len(stale) > 0 {
		t.Errorf("documented but never emitted: %v — drop the row, or restore the emitter it describes", stale)
	}
}

// TestPostRunEventsAreMarkedInUsageDocs holds the table's † marking to
// appendRunEvent's call sites. The prose under the table names the marking and
// never a count or a position, so nothing there needs editing as the set grows
// — unlike the count it replaced, which had gone wrong in both magnitude (five
// named, four appended after the run) and position (five rows back reaches
// `publish_start`, which is emitted mid-run).
func TestPostRunEventsAreMarkedInUsageDocs(t *testing.T) {
	_, marked := docEventNames(t)
	_, postRun := codeEventNames(t)
	if missing := missingFrom(postRun, marked); len(missing) > 0 {
		t.Errorf("appended after the run finished but not marked † in docs/USAGE.md: %v", missing)
	}
	if extra := missingFrom(marked, postRun); len(extra) > 0 {
		t.Errorf("marked † in docs/USAGE.md but emitted during the run: %v", extra)
	}
}
