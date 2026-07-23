package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

func TestValidateSemanticMode(t *testing.T) {
	for _, ok := range []string{"", semanticOff, semanticGo} {
		if err := validateSemanticMode(ok); err != nil {
			t.Fatalf("validateSemanticMode(%q) = %v, want nil", ok, err)
		}
	}
	if err := validateSemanticMode("yes"); err == nil {
		t.Fatal("validateSemanticMode(\"yes\") = nil, want an error")
	}
}

// ---- extractFileSymbols ------------------------------------------------

func declNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestExtractFileSymbolsDeclaredFuncsTypesConstsVars(t *testing.T) {
	src := []byte(`package p

func F(x int) int { return x }

func (t *T) M(y int) {}
func (u U) N() {}

type T struct{ X int }

const C = 1

var V, W = 1, 2
`)
	fs, err := extractFileSymbols("pkg", "", src)
	if err != nil {
		t.Fatalf("extractFileSymbols: %v", err)
	}
	want := []string{"pkg.(*T).M", "pkg.(U).N", "pkg.C", "pkg.F", "pkg.T", "pkg.V", "pkg.W"}
	if got := declNames(fs.declared); !reflect.DeepEqual(got, want) {
		t.Fatalf("declared=%v, want %v", got, want)
	}
}

func TestExtractFileSymbolsReferencesBareCallSamePackage(t *testing.T) {
	src := []byte(`package p

func UseF() int { return F(1) }
`)
	fs, err := extractFileSymbols("pkg", "", src)
	if err != nil {
		t.Fatalf("extractFileSymbols: %v", err)
	}
	if !fs.refs["pkg.F"] {
		t.Fatalf("refs=%v, want pkg.F present (bare call is a same-package reference)", fs.refs)
	}
}

func TestExtractFileSymbolsReferencesQualifiedSelectorInModule(t *testing.T) {
	src := []byte(`package p

import "example.com/mod/other"

func Use() int { return other.F(1) }
`)
	fs, err := extractFileSymbols("pkg", "example.com/mod", src)
	if err != nil {
		t.Fatalf("extractFileSymbols: %v", err)
	}
	if !fs.refs["other.F"] {
		t.Fatalf("refs=%v, want other.F present (in-module qualified selector)", fs.refs)
	}
}

func TestExtractFileSymbolsIgnoresExternalImportSelector(t *testing.T) {
	src := []byte(`package p

import "fmt"

func Use() { fmt.Println("hi") }
`)
	fs, err := extractFileSymbols("pkg", "example.com/mod", src)
	if err != nil {
		t.Fatalf("extractFileSymbols: %v", err)
	}
	for name := range fs.refs {
		if name == "fmt.Println" {
			t.Fatalf("refs=%v should not resolve an external (stdlib) import", fs.refs)
		}
	}
}

func TestExtractFileSymbolsEmptySource(t *testing.T) {
	fs, err := extractFileSymbols("pkg", "", nil)
	if err != nil {
		t.Fatalf("extractFileSymbols(nil): %v", err)
	}
	if len(fs.declared) != 0 || len(fs.refs) != 0 {
		t.Fatalf("empty source should yield no symbols, got %+v", fs)
	}
}

func TestExtractFileSymbolsParseErrorReturnsError(t *testing.T) {
	if _, err := extractFileSymbols("pkg", "", []byte("this is not { go code")); err == nil {
		t.Fatal("expected a parse error for malformed go source")
	}
}

// ---- signature-change detection ----------------------------------------

func TestDiffDeclaredCatchesSignatureChange(t *testing.T) {
	baseSrc := []byte("package p\n\nfunc F() int { return 1 }\n")
	branchSrc := []byte("package p\n\nfunc F(x int) int { return x }\n")
	base, err := extractFileSymbols("pkg", "", baseSrc)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := extractFileSymbols("pkg", "", branchSrc)
	if err != nil {
		t.Fatal(err)
	}
	changed := diffDeclared(base.declared, branch.declared)
	if !changed["pkg.F"] {
		t.Fatalf("changed=%v, want pkg.F flagged (signature changed)", changed)
	}
}

func TestDiffDeclaredNoChangeWhenIdentical(t *testing.T) {
	src := []byte("package p\n\nfunc F(x int) int { return x }\n")
	base, err := extractFileSymbols("pkg", "", src)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := extractFileSymbols("pkg", "", src)
	if err != nil {
		t.Fatal(err)
	}
	if changed := diffDeclared(base.declared, branch.declared); len(changed) != 0 {
		t.Fatalf("changed=%v, want none (identical source)", changed)
	}
}

func TestDiffDeclaredMethodReceiverDistinguishesTypes(t *testing.T) {
	// (*A).Foo and (*B).Foo share a method NAME but must be DIFFERENT
	// qualified symbols — changing one must not look like changing the other.
	baseSrc := []byte(`package p

type A struct{}
type B struct{}

func (a *A) Foo() int { return 1 }
func (b *B) Foo() int { return 1 }
`)
	branchSrc := []byte(`package p

type A struct{}
type B struct{}

func (a *A) Foo(x int) int { return x }
func (b *B) Foo() int { return 1 }
`)
	base, err := extractFileSymbols("pkg", "", baseSrc)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := extractFileSymbols("pkg", "", branchSrc)
	if err != nil {
		t.Fatal(err)
	}
	changed := diffDeclared(base.declared, branch.declared)
	if !changed["pkg.(*A).Foo"] {
		t.Fatalf("changed=%v, want pkg.(*A).Foo flagged", changed)
	}
	if changed["pkg.(*B).Foo"] {
		t.Fatalf("changed=%v, (*B).Foo must NOT be flagged (different receiver, unchanged)", changed)
	}
}

func TestNewRefsOnlyReportsAdditions(t *testing.T) {
	base := map[string]bool{"pkg.F": true}
	branch := map[string]bool{"pkg.F": true, "pkg.G": true}
	added := newRefs(base, branch)
	if len(added) != 1 || !added["pkg.G"] {
		t.Fatalf("added=%v, want only pkg.G (pkg.F was already referenced)", added)
	}
}

// ---- semanticOverlap / computeSemanticEdges -----------------------------

func TestSemanticOverlapDeclaredVsReferenced(t *testing.T) {
	a := branchSemantics{branch: "a", declared: map[string]bool{"pkg.F": true}, refs: map[string]bool{}}
	b := branchSemantics{branch: "b", declared: map[string]bool{}, refs: map[string]bool{"pkg.F": true}}
	if !semanticOverlap(a, b) {
		t.Fatal("expected overlap: a declared-changed F, b newly references F")
	}
	if !semanticOverlap(b, a) {
		t.Fatal("expected overlap symmetrically")
	}
}

func TestSemanticOverlapBothDeclaredSameSymbol(t *testing.T) {
	a := branchSemantics{branch: "a", declared: map[string]bool{"pkg.F": true}, refs: map[string]bool{}}
	b := branchSemantics{branch: "b", declared: map[string]bool{"pkg.F": true}, refs: map[string]bool{}}
	if !semanticOverlap(a, b) {
		t.Fatal("expected overlap: both declared-changed the same symbol")
	}
}

func TestSemanticOverlapNoRelation(t *testing.T) {
	a := branchSemantics{branch: "a", declared: map[string]bool{"pkg.F": true}, refs: map[string]bool{}}
	b := branchSemantics{branch: "b", declared: map[string]bool{"pkg.G": true}, refs: map[string]bool{"pkg.H": true}}
	if semanticOverlap(a, b) {
		t.Fatal("expected no overlap: disjoint symbol names")
	}
}

// gitRepoWithGoFile builds a minimal git repo (module + one go file at base)
// for branchSemanticWriteSet/computeSemanticEdges tests below.
func gitRepoWithGoFile(t *testing.T, modulePath string, files map[string]string) (*gitx.Git, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	g := gitx.New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if modulePath != "" {
		files["go.mod"] = "module " + modulePath + "\n\ngo 1.21\n"
	}
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	return g, base
}

func mkBranchFrom(t *testing.T, g *gitx.Git, branch, base string, writes map[string]string) {
	t.Helper()
	ctx := context.Background()
	wt := filepath.Join(t.TempDir(), branch)
	if err := g.WorktreeAdd(ctx, wt, branch, base); err != nil {
		t.Fatal(err)
	}
	for rel, content := range writes {
		full := filepath.Join(wt, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.At(wt).CommitAll(ctx, branch); err != nil {
		t.Fatal(err)
	}
	if err := g.WorktreeRemove(ctx, wt); err != nil {
		t.Fatal(err)
	}
}

func TestBranchSemanticWriteSetSkipsNonGoFile(t *testing.T) {
	g, base := gitRepoWithGoFile(t, "example.com/mod", map[string]string{"a.go": "package p\n"})
	mkBranchFrom(t, g, "agent/b", base, map[string]string{"notes.txt": "hi\n"})
	declared, refs, note := branchSemanticWriteSet(context.Background(), g, "example.com/mod", base, "agent/b", []string{"notes.txt"})
	if declared != nil || refs != nil {
		t.Fatalf("declared/refs should be nil on a non-Go file, got %v/%v", declared, refs)
	}
	if note == "" || note == "analyzed" {
		t.Fatalf("note=%q, want a skipped: reason", note)
	}
}

func TestBranchSemanticWriteSetSkipsParseFailure(t *testing.T) {
	g, base := gitRepoWithGoFile(t, "example.com/mod", map[string]string{"a.go": "package p\n\nfunc F() {}\n"})
	mkBranchFrom(t, g, "agent/b", base, map[string]string{"a.go": "not valid go{{{"})
	declared, refs, note := branchSemanticWriteSet(context.Background(), g, "example.com/mod", base, "agent/b", []string{"a.go"})
	if declared != nil || refs != nil {
		t.Fatalf("declared/refs should be nil on a parse failure, got %v/%v", declared, refs)
	}
	if note == "" || note == "analyzed" {
		t.Fatalf("note=%q, want a skipped: reason", note)
	}
}

func TestBranchSemanticWriteSetAnalyzedNoGoFiles(t *testing.T) {
	g, base := gitRepoWithGoFile(t, "example.com/mod", map[string]string{"a.go": "package p\n"})
	declared, refs, note := branchSemanticWriteSet(context.Background(), g, "example.com/mod", base, "agent/b", nil)
	if declared == nil || refs == nil {
		t.Fatalf("declared/refs should be non-nil (empty) with no files, got %v/%v", declared, refs)
	}
	if note != "analyzed" {
		t.Fatalf("note=%q, want analyzed", note)
	}
}

// TestComputeSemanticEdgesMotivatingScenario is the unit-level version of the
// motivating example: branch A changes F's signature in a.go; branch B adds a
// brand-new call to F in a DISJOINT file b.go. Both declared-changed/newly-
// referenced sets must overlap on "F", producing an edge.
func TestComputeSemanticEdgesMotivatingScenario(t *testing.T) {
	g, base := gitRepoWithGoFile(t, "example.com/mod", map[string]string{
		"a.go": "package p\n\nfunc F() int { return 1 }\n",
		"b.go": "package p\n\nfunc UseF() int { return 1 }\n",
	})
	mkBranchFrom(t, g, "agent/a", base, map[string]string{"a.go": "package p\n\nfunc F(x int) int { return x }\n"})
	mkBranchFrom(t, g, "agent/b", base, map[string]string{"b.go": "package p\n\nfunc UseF() int { return F(1) }\n"})

	agents := []perAgentJSON{
		{Branch: "agent/a", OK: true, Files: []string{"a.go"}},
		{Branch: "agent/b", OK: true, Files: []string{"b.go"}},
	}
	edges, notes := computeSemanticEdges(context.Background(), g, base, agents)
	want := [][2]string{{"agent/a", "agent/b"}}
	if !reflect.DeepEqual(edges, want) {
		t.Fatalf("edges=%v, want %v", edges, want)
	}
	if notes["agent/a"] != "analyzed" || notes["agent/b"] != "analyzed" {
		t.Fatalf("notes=%v, want both analyzed", notes)
	}
}

// TestComputeSemanticEdgesBothBranchesDeclareChangeSameSymbol is the "same
// symbol, both changed" case: branch A removes F from a.go (a refactor that
// moves it out), branch B independently adds a NEW file c.go that ALSO
// declares F — a real redeclaration conflict if both land, on two
// path-disjoint files.
func TestComputeSemanticEdgesBothBranchesDeclareChangeSameSymbol(t *testing.T) {
	g, base := gitRepoWithGoFile(t, "example.com/mod", map[string]string{
		"a.go": "package p\n\nfunc F() int { return 1 }\n",
	})
	mkBranchFrom(t, g, "agent/a", base, map[string]string{"a.go": "package p\n"})                              // removes F
	mkBranchFrom(t, g, "agent/b", base, map[string]string{"c.go": "package p\n\nfunc F() int { return 2 }\n"}) // re-adds F elsewhere

	agents := []perAgentJSON{
		{Branch: "agent/a", OK: true, Files: []string{"a.go"}},
		{Branch: "agent/b", OK: true, Files: []string{"c.go"}},
	}
	edges, _ := computeSemanticEdges(context.Background(), g, base, agents)
	want := [][2]string{{"agent/a", "agent/b"}}
	if !reflect.DeepEqual(edges, want) {
		t.Fatalf("edges=%v, want %v (both branches declared-change F)", edges, want)
	}
}

func TestComputeSemanticEdgesNoRelationStaysEmpty(t *testing.T) {
	g, base := gitRepoWithGoFile(t, "example.com/mod", map[string]string{
		"a.go": "package p\n\nfunc F() int { return 1 }\n",
		"b.go": "package p\n\nfunc G() int { return 2 }\n",
	})
	mkBranchFrom(t, g, "agent/a", base, map[string]string{"a.go": "package p\n\nfunc F() int { return 99 }\n"})
	mkBranchFrom(t, g, "agent/b", base, map[string]string{"b.go": "package p\n\nfunc G() int { return 100 }\n"})

	agents := []perAgentJSON{
		{Branch: "agent/a", OK: true, Files: []string{"a.go"}},
		{Branch: "agent/b", OK: true, Files: []string{"b.go"}},
	}
	edges, _ := computeSemanticEdges(context.Background(), g, base, agents)
	if len(edges) != 0 {
		t.Fatalf("edges=%v, want none (F's BODY changed, not its signature; G is unrelated)", edges)
	}
}

func TestComputeSemanticEdgesSkipsNonOKAgents(t *testing.T) {
	g, base := gitRepoWithGoFile(t, "example.com/mod", map[string]string{"a.go": "package p\n\nfunc F() int { return 1 }\n"})
	agents := []perAgentJSON{
		{Branch: "agent/a", OK: false, Files: []string{"a.go"}},
	}
	edges, notes := computeSemanticEdges(context.Background(), g, base, agents)
	if len(edges) != 0 {
		t.Fatalf("edges=%v, want none", edges)
	}
	if _, ok := notes["agent/a"]; ok {
		t.Fatalf("notes=%v, a failed agent should not be analyzed at all", notes)
	}
}
