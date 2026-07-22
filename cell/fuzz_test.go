package cell

// FuzzStrategiesAgree is the differential engine fuzzer: it turns arbitrary
// fuzz bytes into a deterministic multi-agent scenario (N agents, each editing
// a path from a small fixed palette with a modify/add/delete edit) and asserts
// the load-bearing invariant integrate.go's doc comment promises but only
// strategies_test.go's FIXED scenarios ever checked: porcelain, naive,
// mergetree and overlay must all land/flag the exact same branches and produce
// a byte-identical final tree. Porcelain (git's own working-tree merge) is the
// reference; any strategy that disagrees with it is a real engine bug, not a
// decoder problem — the decoder is written to be conservative (every byte
// wraps via modulo) precisely so fuzzing explores the SCENARIO space, not
// decode failures.
//
// Ad-hoc fuzzing already found one real bug this way (the ls-tree parser's NUL
// acceptance, see internal/gitx/fuzz_test.go); this target points the same
// technique at the integrator itself.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

const (
	fzExistingFiles = 4  // base palette: f0.txt..f3.txt, for modify/delete
	fzNewFiles      = 2  // add-only palette: new0.txt/new1.txt, forces add/add collisions
	fzLinesPerFile  = 40 // matches scenario()'s convention (cell/integrate_test.go)
	fzMinAgents     = 2
	fzMaxAgents     = 8
)

// fzKind is the edit an agent performs. Mode changes are deliberately out of
// scope (see the design note in the issue-22 brief): modify/add/delete cover
// every shape the seed corpus needs (disjoint, same-file, delete-vs-modify,
// add/add) without widening the decoder.
type fzKind int

const (
	fzModify fzKind = iota
	fzAdd
	fzDelete
	fzNoop // empty-edit agent: branch == base, empty write-set
	fzKindCount
)

// fzEdit is one agent's decoded action.
type fzEdit struct {
	kind    fzKind
	pathIdx int // raw decoded byte; reduced mod a palette size on use
	variant int // raw decoded byte; reduced mod a small range on use
}

// fzScenario is a fully-decoded, deterministic multi-agent batch.
type fzScenario struct {
	clustered bool // true narrows both palettes so overlap is far more likely
	edits     []fzEdit
}

// existingPalette/newPalette: the "overlap pattern" knob. Clustered narrows
// the palette to force collisions (same-file / add-add); spread lets the raw
// byte spread across the full palette, favoring disjoint edits.
func (s fzScenario) existingPalette() int {
	if s.clustered {
		return 2
	}
	return fzExistingFiles
}

func (s fzScenario) newPalette() int {
	if s.clustered {
		return 1
	}
	return fzNewFiles
}

func (e fzEdit) existingIdx(s fzScenario) int { return e.pathIdx % s.existingPalette() }
func (e fzEdit) newIdx(s fzScenario) int      { return e.pathIdx % s.newPalette() }

// fzPath is the path an edit touches ("" for noop).
func fzPath(s fzScenario, e fzEdit) string {
	switch e.kind {
	case fzAdd:
		return fmt.Sprintf("new%d.txt", e.newIdx(s))
	case fzModify, fzDelete:
		return fmt.Sprintf("f%d.txt", e.existingIdx(s))
	default:
		return ""
	}
}

// decodeFzScenario turns arbitrary fuzz bytes into a scenario. Every field is
// read positionally and reduced with modulo (or the zero-fill default when
// data runs out), so ANY input — including empty — decodes to a valid
// scenario: fuzzing this decoder is not the goal, fuzzing the SCENARIO SPACE
// it reaches is.
func decodeFzScenario(data []byte) fzScenario {
	pos := 0
	next := func() byte {
		if pos >= len(data) {
			return 0
		}
		b := data[pos]
		pos++
		return b
	}
	clustered := next()%2 == 1
	n := fzMinAgents + int(next())%(fzMaxAgents-fzMinAgents+1)
	edits := make([]fzEdit, n)
	for i := range edits {
		edits[i] = fzEdit{
			kind:    fzKind(int(next()) % int(fzKindCount)),
			pathIdx: int(next()),
			variant: int(next()),
		}
	}
	return fzScenario{clustered: clustered, edits: edits}
}

// fzEncode is decodeFzScenario's inverse, used only to build readable seed
// corpus entries below (f.Add wants bytes, not structs).
func fzEncode(clustered bool, edits []fzEdit) []byte {
	b := make([]byte, 0, 2+3*len(edits))
	var c byte
	if clustered {
		c = 1
	}
	b = append(b, c, byte(len(edits)-fzMinAgents))
	for _, e := range edits {
		b = append(b, byte(e.kind), byte(e.pathIdx), byte(e.variant))
	}
	return b
}

// fzFileContent is the deterministic body of base file idx: fzLinesPerFile
// uniquely-numbered lines, so a distinct-line edit always auto-merges cleanly.
func fzFileContent(idx int) string {
	var sb strings.Builder
	for ln := 0; ln < fzLinesPerFile; ln++ {
		fmt.Fprintf(&sb, "f%d line %02d\n", idx, ln)
	}
	return sb.String()
}

// fzLine maps a variant to a spaced line number (1, 6, 11, ...). The gap of 5
// is the same spacing cell/integrate_test.go's hotEdit uses to get git a clean
// 3-way auto-merge instead of an adjacent-hunk false conflict.
func fzLine(variant int) int { return (variant%8)*5 + 1 }

// fzModifyContent rewrites base file idx so line fzLine(variant) carries a
// marker unique to (agentIdx, variant). Two agents that land on the SAME line
// (equal variant, mod 8) always disagree in content (agentIdx differs), so
// same-line edits are a guaranteed real conflict; different lines auto-merge.
func fzModifyContent(idx, agentIdx, variant int) string {
	lines := strings.Split(fzFileContent(idx), "\n")
	ln := fzLine(variant)
	if ln < len(lines) {
		lines[ln] = fmt.Sprintf("agent-%d-line-%d-v%d", agentIdx, ln, variant)
	}
	return strings.Join(lines, "\n")
}

// fzBuildStream builds a `git fast-import` stream creating every agent's
// branch off base in ONE process (the "fastest fixture path" bench/bench.go
// uses for the same reason: fixture-building must not dominate the fuzz
// iteration's time budget). A noop edit is a bare ref reset — no new commit,
// no divergence from base — matching the empty-edit-agent seed shape exactly.
func fzBuildStream(base string, s fzScenario) []byte {
	var b bytes.Buffer
	mark := 1
	for i, e := range s.edits {
		branch := fmt.Sprintf("agent/%d", i)
		if e.kind == fzNoop {
			fmt.Fprintf(&b, "reset refs/heads/%s\nfrom %s\n\n", branch, base)
			continue
		}
		path := fzPath(s, e)
		fmt.Fprintf(&b, "commit refs/heads/%s\nmark :%d\n", branch, mark)
		mark++
		b.WriteString("committer sigbound <sigbound@local> 1700000000 +0000\n")
		msg := fmt.Sprintf("agent %d", i)
		fmt.Fprintf(&b, "data %d\n%s\n", len(msg), msg)
		fmt.Fprintf(&b, "from %s\n", base)
		switch e.kind {
		case fzModify:
			fzWriteFileModify(&b, path, fzModifyContent(e.existingIdx(s), i, e.variant))
		case fzAdd:
			fzWriteFileModify(&b, path, fmt.Sprintf("agent-%d-added-v%d\n", i, e.variant))
		case fzDelete:
			fmt.Fprintf(&b, "D %s\n", path)
		}
		b.WriteByte('\n')
	}
	b.WriteString("done\n")
	return b.Bytes()
}

func fzWriteFileModify(b *bytes.Buffer, path, content string) {
	fmt.Fprintf(b, "M 100644 inline %s\ndata %d\n", path, len(content))
	b.WriteString(content)
	b.WriteByte('\n')
}

// fzChanges builds the BranchChange batch KNOWN BY CONSTRUCTION (bench.go's
// same shortcut: each agent touches at most one path, so its write-set is
// exactly that path, computed with no `git diff` spawn).
func fzChanges(s fzScenario) []BranchChange {
	out := make([]BranchChange, len(s.edits))
	for i, e := range s.edits {
		branch := fmt.Sprintf("agent/%d", i)
		if e.kind == fzNoop {
			out[i] = BranchChange{Branch: branch, WriteSet: NewWriteSet()}
			continue
		}
		out[i] = BranchChange{Branch: branch, WriteSet: NewWriteSet(fzPath(s, e))}
	}
	return out
}

func fzFlaggedNames(frs []BranchResult) []string {
	out := make([]string, len(frs))
	for i, fr := range frs {
		out[i] = fr.Branch
	}
	slices.Sort(out)
	return out
}

func fzSorted(xs []string) []string {
	out := slices.Clone(xs)
	slices.Sort(out)
	return out
}

// FuzzStrategiesAgree is the property test described above.
func FuzzStrategiesAgree(f *testing.F) {
	// ---- seed corpus: the known-interesting shapes ----

	// All-disjoint: 4 agents each modify a DIFFERENT existing file.
	f.Add(fzEncode(false, []fzEdit{
		{kind: fzModify, pathIdx: 0, variant: 0},
		{kind: fzModify, pathIdx: 1, variant: 0},
		{kind: fzModify, pathIdx: 2, variant: 0},
		{kind: fzModify, pathIdx: 3, variant: 0},
	}))

	// All-same-file: 4 agents modify f0.txt on distinct spaced lines -> one
	// group, every branch auto-merges clean.
	f.Add(fzEncode(false, []fzEdit{
		{kind: fzModify, pathIdx: 0, variant: 0},
		{kind: fzModify, pathIdx: 0, variant: 1},
		{kind: fzModify, pathIdx: 0, variant: 2},
		{kind: fzModify, pathIdx: 0, variant: 3},
	}))

	// Mixed overlap: 3 agents share f0.txt on distinct lines (one auto-merge
	// group), 3 more each modify a distinct disjoint file.
	f.Add(fzEncode(false, []fzEdit{
		{kind: fzModify, pathIdx: 0, variant: 0},
		{kind: fzModify, pathIdx: 0, variant: 1},
		{kind: fzModify, pathIdx: 0, variant: 2},
		{kind: fzModify, pathIdx: 1, variant: 0},
		{kind: fzModify, pathIdx: 2, variant: 0},
		{kind: fzModify, pathIdx: 3, variant: 0},
	}))

	// Delete-vs-modify conflict: one agent deletes f0.txt, another modifies a
	// line of it -> real conflict, every strategy must flag it identically.
	f.Add(fzEncode(false, []fzEdit{
		{kind: fzDelete, pathIdx: 0, variant: 0},
		{kind: fzModify, pathIdx: 0, variant: 2},
	}))

	// Add-add same path, different content: two agents both add new0.txt with
	// different bodies -> conflict, not a silent last-write-wins.
	f.Add(fzEncode(false, []fzEdit{
		{kind: fzAdd, pathIdx: 0, variant: 1},
		{kind: fzAdd, pathIdx: 0, variant: 5},
	}))

	// Empty-edit agent: one agent contributes no change at all, alongside two
	// agents that do land something.
	f.Add(fzEncode(false, []fzEdit{
		{kind: fzNoop, pathIdx: 0, variant: 0},
		{kind: fzModify, pathIdx: 1, variant: 2},
		{kind: fzAdd, pathIdx: 1, variant: 3},
	}))

	// Clustered: narrow palettes so every kind collides on every path.
	f.Add(fzEncode(true, []fzEdit{
		{kind: fzModify, pathIdx: 0, variant: 0},
		{kind: fzAdd, pathIdx: 0, variant: 1},
		{kind: fzDelete, pathIdx: 1, variant: 0},
		{kind: fzModify, pathIdx: 1, variant: 4},
		{kind: fzAdd, pathIdx: 0, variant: 2},
	}))

	f.Fuzz(func(t *testing.T, data []byte) {
		s := decodeFzScenario(data)
		ctx := context.Background()

		dir := t.TempDir()
		g := gitx.New(dir)
		if err := g.Init(ctx); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < fzExistingFiles; i++ {
			path := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
			if err := os.WriteFile(path, []byte(fzFileContent(i)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		base, err := g.CommitAll(ctx, "base")
		if err != nil {
			t.Fatal(err)
		}

		if err := g.FastImport(ctx, fzBuildStream(base, s)); err != nil {
			t.Fatalf("data=%x scenario=%+v: fast-import: %v", data, s, err)
		}
		changes := fzChanges(s)

		in := NewIntegrator(g)
		results := make(map[string]IntegrationResult, len(AvailableStrategies()))
		for _, strat := range AvailableStrategies() {
			res, err := in.Integrate(ctx, base, changes, strat)
			if err != nil {
				t.Fatalf("data=%x scenario=%+v: %s: %v", data, s, strat, err)
			}
			results[strat] = res
		}

		// Porcelain (git's own working-tree merge) is the reference: every
		// other strategy must land/flag the exact same branches and produce
		// a byte-identical final tree.
		ref := results[StrategyPorcelain]
		refLanded := fzSorted(ref.Landed)
		refFlagged := fzFlaggedNames(ref.Flagged)
		refTree := treeOID(t, in, ref.FinalSHA)

		for _, strat := range AvailableStrategies() {
			if strat == StrategyPorcelain {
				continue
			}
			res := results[strat]
			if gotLanded := fzSorted(res.Landed); !slices.Equal(gotLanded, refLanded) {
				t.Fatalf("data=%x scenario=%+v: %s landed=%v != porcelain landed=%v",
					data, s, strat, gotLanded, refLanded)
			}
			if gotFlagged := fzFlaggedNames(res.Flagged); !slices.Equal(gotFlagged, refFlagged) {
				t.Fatalf("data=%x scenario=%+v: %s flagged=%v != porcelain flagged=%v",
					data, s, strat, gotFlagged, refFlagged)
			}
			if gotTree := treeOID(t, in, res.FinalSHA); gotTree != refTree {
				t.Fatalf("data=%x scenario=%+v: %s tree=%s != porcelain tree=%s (partition/merge invariant violated)",
					data, s, strat, gotTree, refTree)
			}
		}
	})
}
