package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeImpactFixture writes a 3-package Go module directly to dir (no git
// needed — computeImpact only cares about files on disk, since it runs `go
// list` in whatever checkout it's handed): a (leaf), b (imports a), c
// (independent). Mirrors the "pkg a; pkg b imports a; pkg c independent"
// fixture the driveRun-level -verify-impact tests in run_test.go build on
// top of via a real repo/agent.
func writeImpactFixture(t *testing.T, dir string) {
	t.Helper()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/impact\n\ngo 1.21\n")
	write("a/a.go", "package a\n\nfunc A() int { return 1 }\n")
	write("b/b.go", "package b\n\nimport \"example.com/impact/a\"\n\nfunc B() int { return a.A() + 1 }\n")
	write("c/c.go", "package c\n\nfunc C() int { return 3 }\n")
}

// TestComputeImpactChangedLeafPackage: changing the independent package c
// impacts exactly c — nothing imports it, so there are no reverse deps.
func TestComputeImpactChangedLeafPackage(t *testing.T) {
	dir := t.TempDir()
	writeImpactFixture(t, dir)

	pkgs, ok := computeImpact(context.Background(), dir, []string{"c/c.go"})
	if !ok {
		t.Fatal("computeImpact ok=false, want true for a plain in-module package change")
	}
	if got := strings.Join(pkgs, " "); got != "./c" {
		t.Fatalf("impacted=%q, want ./c", got)
	}
}

// TestComputeImpactExpandsToReverseDependents: changing a impacts a AND b
// (b imports a), transitively — but NOT c, which is independent.
func TestComputeImpactExpandsToReverseDependents(t *testing.T) {
	dir := t.TempDir()
	writeImpactFixture(t, dir)

	pkgs, ok := computeImpact(context.Background(), dir, []string{"a/a.go"})
	if !ok {
		t.Fatal("computeImpact ok=false, want true")
	}
	if got := strings.Join(pkgs, " "); got != "./a ./b" {
		t.Fatalf("impacted=%q, want ./a ./b (b imports a; c must be excluded)", got)
	}
}

// TestComputeImpactFallsBackOnDoubt covers every "any doubt" trigger listed
// in computeImpact's doc: a non-Go file, a change under testdata/, an empty
// changed-file list, and a batch where just ONE path is doubtful.
func TestComputeImpactFallsBackOnDoubt(t *testing.T) {
	dir := t.TempDir()
	writeImpactFixture(t, dir)
	ctx := context.Background()

	cases := map[string][]string{
		"go.mod":                             {"go.mod"},
		"non-Go file":                        {"README.md"},
		"testdata/ path":                     {"a/testdata/fixture.go"},
		"empty changed set":                  nil,
		"one doubtful path taints the batch": {"a/a.go", "README.md"},
	}
	for name, changed := range cases {
		if _, ok := computeImpact(ctx, dir, changed); ok {
			t.Fatalf("%s: computeImpact ok=true for %v, want fallback (any doubt)", name, changed)
		}
	}
}

// TestComputeImpactUnknownDirectoryIsDoubt: a changed .go file whose
// directory `go list` never reported isn't a real package in this tree —
// doubt, not a crash.
func TestComputeImpactUnknownDirectoryIsDoubt(t *testing.T) {
	dir := t.TempDir()
	writeImpactFixture(t, dir)

	if _, ok := computeImpact(context.Background(), dir, []string{"nope/missing.go"}); ok {
		t.Fatal("computeImpact ok=true for an unknown package directory, want fallback")
	}
}

// TestParseGoListJSON decodes `go list -json`'s actual shape: a STREAM of
// JSON objects, not a single array.
func TestParseGoListJSON(t *testing.T) {
	data := []byte(`{"Dir":"/repo/a","ImportPath":"example.com/impact/a","Imports":["fmt"]}
{"Dir":"/repo/b","ImportPath":"example.com/impact/b","Imports":["example.com/impact/a"],"TestImports":["testing"]}
`)
	pkgs, err := parseGoListJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("got %d packages, want 2", len(pkgs))
	}
	if pkgs[1].ImportPath != "example.com/impact/b" {
		t.Fatalf("pkgs[1].ImportPath=%q, want example.com/impact/b", pkgs[1].ImportPath)
	}
	if len(pkgs[1].TestImports) != 1 || pkgs[1].TestImports[0] != "testing" {
		t.Fatalf("pkgs[1].TestImports=%v, want [testing]", pkgs[1].TestImports)
	}
}

// TestParseGoListJSONMalformedErrors: malformed input must error, not panic
// (see FuzzParseGoListJSON for the exhaustive version of this check).
func TestParseGoListJSONMalformedErrors(t *testing.T) {
	if _, err := parseGoListJSON([]byte("not json at all")); err == nil {
		t.Fatal("want an error decoding malformed go-list output")
	}
}

// TestImpactedPackagesTransitiveClosure exercises the reverse-graph closure
// directly, independent of go list/computeImpact: a chain a<-b<-c plus an
// unrelated package d.
func TestImpactedPackagesTransitiveClosure(t *testing.T) {
	pkgs := []goListPkg{
		{ImportPath: "a"},
		{ImportPath: "b", Imports: []string{"a"}},
		{ImportPath: "c", Imports: []string{"b"}},
		{ImportPath: "d"},
	}
	impacted := impactedPackages(pkgs, map[string]bool{"a": true})
	for _, want := range []string{"a", "b", "c"} {
		if !impacted[want] {
			t.Fatalf("impacted=%v missing %q (transitive reverse dep of a)", impacted, want)
		}
	}
	if impacted["d"] {
		t.Fatalf("impacted=%v should not include unrelated package d", impacted)
	}
}

// TestImpactedPackagesTestOnlyImportCounts: a package that imports the
// changed one ONLY from its test files (TestImports/XTestImports) is still
// impacted — its tests can break even though its main sources never
// reference the changed package.
func TestImpactedPackagesTestOnlyImportCounts(t *testing.T) {
	pkgs := []goListPkg{
		{ImportPath: "a"},
		{ImportPath: "b", TestImports: []string{"a"}},
		{ImportPath: "c", XTestImports: []string{"a"}},
	}
	impacted := impactedPackages(pkgs, map[string]bool{"a": true})
	if !impacted["b"] || !impacted["c"] {
		t.Fatalf("impacted=%v, want b and c included (test-only imports of a)", impacted)
	}
}
