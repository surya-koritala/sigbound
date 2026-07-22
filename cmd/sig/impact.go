// Test-impact analysis for -verify-impact (issue #10): map the run's landed
// write-set to the Go packages that could actually be affected by it, so a
// BYO -verify-impact command can run only their tests instead of the whole
// suite. See runVerify for how the decision is used and docs/USAGE.md's
// Scoped verification section for the full contract.
//
// The whole thing is FAIL-SAFE by construction: computeImpact returns ok=false
// on ANY doubt (a non-Go file changed, a change under testdata/, a `go list`
// failure, an empty impact set, a changed file `go list` never saw) and the
// caller's contract is to fall back to the full -verify command whenever that
// happens. Impact scoping trades confidence for speed; it is never allowed to
// trade away correctness.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/cell"
)

// goListPkg is the subset of `go list -json` fields test-impact analysis
// needs. Imports/TestImports/XTestImports are all treated as edges into the
// reverse-dependency graph: a package's OWN test files (TestImports) or its
// external test package (XTestImports) can import a package the main sources
// never do, and a change to that package should still impact this one.
type goListPkg struct {
	Dir          string   `json:"Dir"`
	ImportPath   string   `json:"ImportPath"`
	Imports      []string `json:"Imports"`
	TestImports  []string `json:"TestImports"`
	XTestImports []string `json:"XTestImports"`
}

// parseGoListJSON decodes the streaming JSON object sequence `go list -json`
// prints — one object per package, NOT a single JSON array — into goListPkg
// records. Untrusted external-command output, so like every other parser of
// external input in this package (see fuzz_test.go) it must never panic: a
// malformed/truncated/hostile object stops decoding and returns an error.
func parseGoListJSON(data []byte) ([]goListPkg, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var pkgs []goListPkg
	for {
		var pk goListPkg
		if err := dec.Decode(&pk); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		pkgs = append(pkgs, pk)
	}
	return pkgs, nil
}

// impactedPackages closes changed (a set of import paths) over the reverse
// import graph built from pkgs: a change to package A impacts A itself and
// every package that imports A, directly or transitively, through ANY of
// Imports/TestImports/XTestImports.
func impactedPackages(pkgs []goListPkg, changed map[string]bool) map[string]bool {
	reverse := make(map[string][]string, len(pkgs))
	for _, pk := range pkgs {
		for _, edges := range [][]string{pk.Imports, pk.TestImports, pk.XTestImports} {
			for _, imp := range edges {
				reverse[imp] = append(reverse[imp], pk.ImportPath)
			}
		}
	}
	impacted := make(map[string]bool, len(changed))
	queue := make([]string, 0, len(changed))
	for imp := range changed {
		impacted[imp] = true
		queue = append(queue, imp)
	}
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, dep := range reverse[cur] {
			if !impacted[dep] {
				impacted[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	return impacted
}

// changedFilesInDoubt reports whether ANY path in changedFiles makes impact
// scoping untrustworthy: a non-.go file (go.mod, go.sum, docs, data, ...) —
// it can change build/test behavior in ways a package-import graph can't
// see — or a path under a directory literally named "testdata", which `go
// list` never treats as package source, so it can never be mapped to an
// impacted package by directory match even when the file itself ends in
// .go. One doubtful path in a batch taints the whole batch.
func changedFilesInDoubt(changedFiles []string) bool {
	for _, f := range changedFiles {
		if filepath.Ext(f) != ".go" {
			return true
		}
		for _, seg := range strings.Split(filepath.ToSlash(f), "/") {
			if seg == "testdata" {
				return true
			}
		}
	}
	return false
}

// pkgArg formats a package directory, relative to the module root, the way
// -verify-impact's SIGBOUND_IMPACTED_PKGS is documented to: "./a/b", or "."
// for the module root itself — the same shape `go test`/`go build` accept
// directly as package arguments.
func pkgArg(rel string) string {
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "."
	}
	return "./" + rel
}

// computeImpact tries to map changedFiles (repo-relative paths, e.g. the
// run's landed write-set) to the Go packages a scoped -verify-impact run
// needs: the packages containing those files, plus every reverse dependent
// (see impactedPackages). checkoutDir is the detached worktree `go list`
// runs in — the SAME tree -verify/-verify-impact is about to see.
//
// ok is false on ANY doubt (see changedFilesInDoubt), a `go list` spawn or
// parse failure, a changed .go file whose directory `go list` never
// reported (it isn't a real package in this tree — a module-boundary or
// build-tag case impact analysis can't reason about safely), or an empty
// resulting impact set. The caller (runVerify) falls back to the full
// -verify command whenever ok is false.
func computeImpact(ctx context.Context, checkoutDir string, changedFiles []string) ([]string, bool) {
	if len(changedFiles) == 0 || changedFilesInDoubt(changedFiles) {
		return nil, false
	}

	cmd := exec.CommandContext(ctx, "go", "list", "-json", "./...")
	cmd.Dir = checkoutDir
	cmd.WaitDelay = 2 * time.Second // return promptly on cancel; see runAgent
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	pkgs, err := parseGoListJSON(out)
	if err != nil {
		return nil, false
	}

	byImport := make(map[string]goListPkg, len(pkgs))
	byDir := make(map[string]string, len(pkgs)) // module-relative dir -> import path
	for _, pk := range pkgs {
		if pk.ImportPath == "" || pk.Dir == "" {
			continue
		}
		byImport[pk.ImportPath] = pk
		if rel, err := filepath.Rel(checkoutDir, pk.Dir); err == nil {
			byDir[filepath.ToSlash(rel)] = pk.ImportPath
		}
	}

	changed := make(map[string]bool, len(changedFiles))
	for _, f := range changedFiles {
		dir := filepath.ToSlash(filepath.Dir(filepath.Clean(f)))
		imp, found := byDir[dir]
		if !found {
			return nil, false
		}
		changed[imp] = true
	}

	// cell.WriteSet gives dedup+deterministic sort for free (see cell/writeset.go)
	// rather than hand-rolling it here.
	set := cell.NewWriteSet()
	for imp := range impactedPackages(pkgs, changed) {
		pk, ok := byImport[imp]
		if !ok {
			continue
		}
		rel, err := filepath.Rel(checkoutDir, pk.Dir)
		if err != nil {
			return nil, false
		}
		set.Add(pkgArg(rel))
	}
	out2 := set.Paths()
	if len(out2) == 0 {
		return nil, false
	}
	return out2, true
}
