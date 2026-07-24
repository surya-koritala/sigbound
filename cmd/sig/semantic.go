// Semantic conflict detection (-semantic go): an OPT-IN, Go-only, best-effort
// pass that catches a class of conflict path-overlap partitioning can never
// see — branch A renames or changes the signature of func F in a.go, branch B
// adds a brand-new call to F in b.go. The write-sets are path-disjoint, so
// today's partition lands them in parallel and the merge is textually clean;
// the build only breaks once -verify runs on the combined tree. This pass
// runs BETWEEN the agent phase and integrate: it parses (go/parser/go/ast,
// stdlib only — no go/types, no type-checking; see the package doc in
// docs/USAGE.md's Semantic conflicts section for why) the base and branch
// versions of every changed .go file and extracts each branch's DECLARED and
// REFERENCED symbol names, qualified by directory (Go's own
// package-per-directory convention). Two branches are flagged as
// semantically overlapping — and unioned into one partition group, see
// cell.PartitionSemantic — when one declares-changed a symbol the other
// declares-changed too, or newly references.
//
// Fails open, always: a parse failure, a non-Go file in a branch's write-set,
// or a git read error means that ONE branch contributes no semantic edges —
// never an operational error, and never a reason to keep that branch from
// integrating on its path-based merits alone.
package main

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// -semantic's two accepted values.
const (
	semanticOff = "off"
	semanticGo  = "go"
)

// validateSemanticMode rejects any -semantic value that is not a known mode.
// "" (the in-process zero value for runParams.Semantic, same convention as
// LaneMode/EnvMode) is accepted as a synonym for off.
func validateSemanticMode(s string) error {
	switch s {
	case "", semanticOff, semanticGo:
		return nil
	default:
		return fmt.Errorf("unknown -semantic mode %q (want go|off)", s)
	}
}

// fileSymbols is what one version (base or branch) of one Go file
// contributes: every top-level DECLARED symbol (qualified name -> a printed
// signature, so add/remove/signature-change can be told apart from
// no-change) and every symbol it REFERENCES that resolves, by name, to
// somewhere in this module (qualified name -> present).
type fileSymbols struct {
	declared map[string]string
	refs     map[string]bool
}

// parseGoSource parses one Go source file. It NEVER panics: go/parser is
// being pointed at agent-written code read straight out of a git blob, i.e.
// untrusted input, so a syntax error is returned as an ordinary error exactly
// as go/parser itself would, and the recover below is defense in depth
// against anything else (a parser internal panic on some pathological input)
// — see FuzzExtractFileSymbols.
func parseGoSource(src []byte) (f *ast.File, fset *token.FileSet, err error) {
	fset = token.NewFileSet()
	defer func() {
		if r := recover(); r != nil {
			f = nil
			err = fmt.Errorf("panic parsing go source: %v", r)
		}
	}()
	f, err = parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	return f, fset, err
}

// renderNode prints an AST node back to source text — used as a stable,
// comparable "signature" string for a declaration (so a base/branch diff can
// tell "same params, different name" from "same name, different params"
// without a real type checker). A nil node or a render failure yields "",
// which simply compares equal to itself and unequal to any real signature —
// never a crash.
func renderNode(fset *token.FileSet, n ast.Node) (out string) {
	if n == nil {
		return ""
	}
	defer func() {
		if r := recover(); r != nil {
			out = ""
		}
	}()
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, n); err != nil {
		return ""
	}
	return buf.String()
}

// extractFileSymbols parses src (one Go file's content at one git revision)
// and extracts its DECLARED and REFERENCED symbols, both qualified by dir
// (the file's repo-relative directory — Go's package-per-directory
// convention stands in for a real package identity, since this pass never
// loads the module). modulePath is this repo's module path (from go.mod,
// "" when unknown); it is only needed to resolve a CROSS-package selector
// reference (pkg.Sym) to a directory inside this module — a same-package
// bare call is unaffected by it.
//
// DECLARED covers top-level funcs (receiver-qualified for methods, e.g.
// "(*T).M"), types, consts, and vars. REFERENCED is deliberately narrow —
// only two shapes, both cheap and low-noise without a type checker:
//   - a bare call F(...): attributed to dir (the referencing file's own
//     package, i.e. "the same module" as opposed to some external import).
//   - a selector expression pkg.Sym (called or not, e.g. also a type
//     reference like "var x pkg.Type"): attributed to pkg's own directory,
//     but ONLY when pkg is a locally-imported name whose import path lives
//     inside modulePath — an external/stdlib import never resolves, so it
//     never creates a reference (there is nothing in this repo it could
//     collide with).
//
// It NEVER panics on adversarial input (this parses agent-written code
// straight from a git blob) — see FuzzExtractFileSymbols. An empty/blank src
// (the other side of an add or delete) yields the zero value with no error.
func extractFileSymbols(dir, modulePath string, src []byte) (fs fileSymbols, err error) {
	fs = fileSymbols{declared: map[string]string{}, refs: map[string]bool{}}
	if len(bytes.TrimSpace(src)) == 0 {
		return fs, nil
	}
	// Defense in depth on top of parseGoSource/renderNode's own recovers:
	// nothing below this point should be able to panic on a successfully
	// parsed AST, but this is untrusted input and the one hard requirement
	// is "never crash the run".
	defer func() {
		if r := recover(); r != nil {
			fs = fileSymbols{}
			err = fmt.Errorf("panic analyzing go source: %v", r)
		}
	}()

	file, fset, perr := parseGoSource(src)
	if perr != nil {
		return fileSymbols{}, perr
	}

	// Local import name -> import path, for resolving qualified selectors.
	// Blank ("_") and dot (".") imports contribute no usable selector name
	// and are skipped (a dot-import's unqualified symbols are indistinguishable
	// from a same-package bare call anyway, given no type information).
	imports := map[string]string{}
	for _, spec := range file.Imports {
		importPath, uerr := strconv.Unquote(spec.Path.Value)
		if uerr != nil {
			continue
		}
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "_" || name == "." || name == "" {
			continue
		}
		imports[name] = importPath
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			qname := dir + "." + d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				qname = dir + ".(" + recvTypeName(d.Recv.List[0].Type) + ")." + d.Name.Name
			}
			fs.declared[qname] = renderNode(fset, d.Type)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					fs.declared[dir+"."+s.Name.Name] = renderNode(fset, s.Type)
				case *ast.ValueSpec:
					typeSig := renderNode(fset, s.Type)
					for i, nm := range s.Names {
						if nm.Name == "_" {
							continue
						}
						sig := typeSig
						if i < len(s.Values) {
							sig += "=" + renderNode(fset, s.Values[i])
						}
						fs.declared[dir+"."+nm.Name] = sig
					}
				}
			}
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch expr := n.(type) {
		case *ast.CallExpr:
			if id, ok := expr.Fun.(*ast.Ident); ok {
				fs.refs[dir+"."+id.Name] = true
			}
		case *ast.SelectorExpr:
			if pkgIdent, ok := expr.X.(*ast.Ident); ok {
				if impPath, ok := imports[pkgIdent.Name]; ok {
					if d, ok := dirFromImportPath(impPath, modulePath); ok {
						fs.refs[d+"."+expr.Sel.Name] = true
					}
				}
			}
		}
		return true
	})

	return fs, nil
}

// dirFromImportPath maps an import path to a repo-relative directory when it
// lives inside modulePath (this repo's own module) — the root package itself
// maps to "." (matching extractFileSymbols'/goExportedDecls' directory
// convention for repo-root files). modulePath == "" (go.mod unreadable) or
// an external/stdlib import both simply fail to resolve.
func dirFromImportPath(importPath, modulePath string) (string, bool) {
	if modulePath == "" {
		return "", false
	}
	if importPath == modulePath {
		return ".", true
	}
	if prefix := modulePath + "/"; strings.HasPrefix(importPath, prefix) {
		return strings.TrimPrefix(importPath, prefix), true
	}
	return "", false
}

// diffDeclared returns the qualified names whose declaration DIFFERS between
// base and branch: added (only in branch), removed (only in base), or a
// different rendered signature on both sides.
func diffDeclared(base, branch map[string]string) map[string]bool {
	changed := map[string]bool{}
	for name, sig := range branch {
		if baseSig, ok := base[name]; !ok || baseSig != sig {
			changed[name] = true
		}
	}
	for name := range base {
		if _, ok := branch[name]; !ok {
			changed[name] = true
		}
	}
	return changed
}

// newRefs returns the qualified names branch references that base did not —
// only a NEW reference counts, so a branch that keeps calling something it
// was already calling is not flagged for that call.
func newRefs(base, branch map[string]bool) map[string]bool {
	added := map[string]bool{}
	for name := range branch {
		if !base[name] {
			added[name] = true
		}
	}
	return added
}

// blobBatcher is the seam semantic analysis reads blobs through: either a raw
// gitx.Git (a cat-file spawn per call) or a cell (its reused cat-file --batch
// daemon). Both satisfy gitx.BlobsBatch's signature, so the -semantic phase's
// per-branch reads route through the cell's daemon in a live run yet still take a
// plain *gitx.Git in the unit tests.
type blobBatcher interface {
	BlobsBatch(ctx context.Context, specs []string) (map[string]string, error)
}

// readModulePath returns the `module ...` path declared in go.mod at rev, or
// "" if go.mod is absent/unreadable/has no module line. Callers degrade
// gracefully on "": cross-package selector references simply never resolve
// (extractFileSymbols/dirFromImportPath); same-package bare-call references
// are unaffected. The single go.mod read goes through blobs (the daemon in a
// live run) like every other -semantic blob read.
func readModulePath(ctx context.Context, blobs blobBatcher, rev string) string {
	m, err := blobs.BlobsBatch(ctx, []string{rev + ":go.mod"})
	if err != nil {
		return ""
	}
	content, ok := m[rev+":go.mod"]
	if !ok {
		return ""
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if p, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(p)
		}
	}
	return ""
}

// branchSemanticWriteSet computes one branch's Go SYMBOL write-set relative
// to baseSHA: the qualified names it DECLARED-CHANGED and the qualified
// names it NEWLY REFERENCES (see extractFileSymbols/diffDeclared/newRefs).
// files is the branch's ACTUAL write-set (already computed for lane
// enforcement — no extra `git diff`).
//
// declared/refs are nil (both) iff the branch could NOT be confidently
// analyzed — a non-Go file in its write-set, a parse failure, or a git read
// error — in which case note explains why and the caller MUST fail open:
// this branch contributes no semantic edges, exactly as if -semantic were
// off for it. A successful analysis (possibly of zero .go files) returns
// non-nil, possibly-empty maps and note "analyzed".
func branchSemanticWriteSet(ctx context.Context, blobs blobBatcher, modulePath, baseSHA, branch string, files []string) (declared, refs map[string]bool, note string) {
	var goFiles []string
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			return nil, nil, fmt.Sprintf("skipped: non-Go file in changes (%s)", f)
		}
		goFiles = append(goFiles, f)
	}
	if len(goFiles) == 0 {
		return map[string]bool{}, map[string]bool{}, "analyzed"
	}

	specs := make([]string, 0, len(goFiles)*2)
	for _, path := range goFiles {
		specs = append(specs, baseSHA+":"+path, branch+":"+path)
	}
	contents, err := blobs.BlobsBatch(ctx, specs)
	if err != nil {
		return nil, nil, fmt.Sprintf("skipped: read blobs: %v", err)
	}

	declared = map[string]bool{}
	refs = map[string]bool{}
	for _, path := range goFiles {
		dir := filepath.ToSlash(filepath.Dir(path))
		baseSyms, berr := extractFileSymbols(dir, modulePath, []byte(contents[baseSHA+":"+path]))
		if berr != nil {
			return nil, nil, fmt.Sprintf("skipped: parse error in %s", path)
		}
		branchSyms, brerr := extractFileSymbols(dir, modulePath, []byte(contents[branch+":"+path]))
		if brerr != nil {
			return nil, nil, fmt.Sprintf("skipped: parse error in %s", path)
		}
		for name := range diffDeclared(baseSyms.declared, branchSyms.declared) {
			declared[name] = true
		}
		for name := range newRefs(baseSyms.refs, branchSyms.refs) {
			refs[name] = true
		}
	}
	return declared, refs, "analyzed"
}

// branchSemantics is one successfully-analyzed branch's Go symbol write-set.
type branchSemantics struct {
	branch   string
	declared map[string]bool
	refs     map[string]bool
}

// semanticOverlap implements the conflict rule: A and B overlap iff A
// declared-changed a symbol B also declared-changed, or B newly references,
// or symmetrically B declared-changed a symbol A newly references.
func semanticOverlap(a, b branchSemantics) bool {
	for name := range a.declared {
		if b.declared[name] || b.refs[name] {
			return true
		}
	}
	for name := range b.declared {
		if a.refs[name] {
			return true
		}
	}
	return false
}

// computeSemanticEdges runs -semantic go over every branch the agent phase
// actually committed (a.OK), and returns:
//   - edges: pairs of branch names (sorted, for a deterministic report/event)
//     a symbol-level overlap was found for — feed straight into
//     cell.WithSemanticEdges (see driveRun's call site).
//   - notes: branch name -> perAgentJSON.SemanticNote ("analyzed" or a
//     "skipped: ..." reason).
//
// NEVER fails the run: every analysis error is a per-branch "skipped" note,
// not an operational error (see branchSemanticWriteSet). baseSHA is the
// pinned base every branch forked from.
func computeSemanticEdges(ctx context.Context, blobs blobBatcher, baseSHA string, agents []perAgentJSON) (edges [][2]string, notes map[string]string) {
	modulePath := readModulePath(ctx, blobs, baseSHA)
	notes = make(map[string]string, len(agents))

	var syms []branchSemantics
	for _, a := range agents {
		if !a.OK {
			continue
		}
		declared, refs, note := branchSemanticWriteSet(ctx, blobs, modulePath, baseSHA, a.Branch, a.Files)
		notes[a.Branch] = note
		if declared == nil && refs == nil {
			continue // fail-open: this branch contributes no edges
		}
		syms = append(syms, branchSemantics{branch: a.Branch, declared: declared, refs: refs})
	}

	for i := 0; i < len(syms); i++ {
		for j := i + 1; j < len(syms); j++ {
			if semanticOverlap(syms[i], syms[j]) {
				edges = append(edges, [2]string{syms[i].branch, syms[j].branch})
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i][0] != edges[j][0] {
			return edges[i][0] < edges[j][0]
		}
		return edges[i][1] < edges[j][1]
	})
	return edges, notes
}
