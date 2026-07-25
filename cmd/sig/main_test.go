package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// treeFiles returns rev's tree as a set, for asserting the base tree itself —
// the issue-#130 failure mode is a WRONG tree behind an advanced ref, so these
// tests assert file presence, never just the exit path.
func treeFiles(t *testing.T, g *gitx.Git, rev string) map[string]bool {
	t.Helper()
	files, err := g.LsTree(context.Background(), rev)
	if err != nil {
		t.Fatal(err)
	}
	have := make(map[string]bool, len(files))
	for _, f := range files {
		have[f] = true
	}
	return have
}

// TestIntegrateRefusesStaleBranchesAfterBaseMoved is issue #130's exact
// scenario: land two branches (moving the base), then feed those same
// now-stale branches back to `sig integrate`. Their overlay contribution
// versus the MOVED base is the deletion of everything it gained, so the
// ancestry guard must refuse the batch, land nothing, and leave the base ref
// and tree untouched.
func TestIntegrateRefusesStaleBranchesAfterBaseMoved(t *testing.T) {
	ctx := context.Background()
	g, base := gitRepoWithGoFile(t, "", map[string]string{"base.txt": "base\n"})
	mkBranchFrom(t, g, "agent/a", base, map[string]string{"a.txt": "a\n"})
	mkBranchFrom(t, g, "agent/b", base, map[string]string{"b.txt": "b\n"})

	// First integrate lands both and advances main past the branches' tips.
	captureStdout(t, func() {
		if err := runIntegrate([]string{"-repo", g.Dir(), "-base", "main", "-branches", "agent/a,agent/b"}); err != nil {
			t.Fatalf("first integrate: %v", err)
		}
	})
	moved, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if moved == base {
		t.Fatal("first integrate did not move the base; scenario not established")
	}

	// Re-integrating the SAME branches must refuse — with an error naming an
	// offending branch and the base — and change nothing.
	err = runIntegrate([]string{"-repo", g.Dir(), "-base", "main", "-branches", "agent/a,agent/b"})
	if err == nil {
		t.Fatal("re-integrating stale branches succeeded; want refusal")
	}
	if !strings.Contains(err.Error(), "agent/a") || !strings.Contains(err.Error(), short(moved)) {
		t.Fatalf("refusal must name the offending branch and the base, got: %v", err)
	}
	after, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if after != moved {
		t.Fatalf("base ref moved on a refused integrate: %s -> %s", moved, after)
	}
	have := treeFiles(t, g, "main")
	if !have["a.txt"] || !have["b.txt"] || !have["base.txt"] {
		t.Fatalf("base tree lost landed files on a refused integrate: %v", have)
	}
}

// TestIntegrateRefusesImportedBranchPredatingBase is the bundle workflow's
// version of the same hazard: `sig import` creates imported/<worker>/* branches
// forked from whatever base the WORKER had, which routinely predates the
// coordinator's. Such a branch must be refused, deleting nothing.
func TestIntegrateRefusesImportedBranchPredatingBase(t *testing.T) {
	ctx := context.Background()
	g, fork := gitRepoWithGoFile(t, "", map[string]string{"base.txt": "base\n"})
	mkBranchFrom(t, g, "imported/w1/agent/t1", fork, map[string]string{"t1.txt": "t1\n"})

	// The coordinator's main moves on past the worker's fork point.
	if err := os.WriteFile(filepath.Join(g.Dir(), "landed.txt"), []byte("landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	moved, err := g.CommitAll(ctx, "landed on coordinator")
	if err != nil {
		t.Fatal(err)
	}

	if err := runIntegrate([]string{"-repo", g.Dir(), "-base", "main", "-branches", "imported/w1/agent/t1"}); err == nil {
		t.Fatal("integrating an imported branch that predates the base succeeded; want refusal")
	}
	after, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if after != moved {
		t.Fatalf("base ref moved on a refused integrate: %s -> %s", moved, after)
	}
	if have := treeFiles(t, g, "main"); !have["landed.txt"] {
		t.Fatalf("refused integrate deleted landed.txt from the base tree: %v", have)
	}
}

// TestIntegrateLandsBranchContainingBase pins the no-regression side of the
// guard: a branch forked from the CURRENT base descends from it and must
// integrate and land exactly as before.
func TestIntegrateLandsBranchContainingBase(t *testing.T) {
	g, base := gitRepoWithGoFile(t, "", map[string]string{"base.txt": "base\n"})
	mkBranchFrom(t, g, "agent/ok", base, map[string]string{"ok.txt": "ok\n"})

	out := captureStdout(t, func() {
		if err := runIntegrate([]string{"-repo", g.Dir(), "-base", "main", "-branches", "agent/ok"}); err != nil {
			t.Fatalf("runIntegrate: %v", err)
		}
	})
	var res resultJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse integrate json: %v\n%s", err, out)
	}
	if len(res.Landed) != 1 || res.Landed[0] != "agent/ok" {
		t.Fatalf("landed = %v, want [agent/ok]", res.Landed)
	}
	if have := treeFiles(t, g, "main"); !have["ok.txt"] || !have["base.txt"] {
		t.Fatalf("base tree missing files after a legitimate integrate: %v", have)
	}
}

// TestIntegrateRefusesWhenTheBaseMovedUnderTheResolver forces the interleaving
// `sig integrate` is exposed to and CANNOT narrow away: -resolver runs between
// the base read at the top of the command and the ref write at the bottom, and
// it runs for as long as the operator's resolver takes. A competing landing
// arriving in that window used to be reset away by this command — a plain
// read-then-write — while it printed a successful result.
//
// The resolver here IS the competing writer, so the race is forced by
// construction rather than by timing: it lands other/winner on main and then
// resolves the conflict, so integration proceeds all the way to a landing whose
// expected old value is gone. The swap must be refused, the winner must survive
// intact, and the command must exit non-zero.
// resolverSeamRepo is gitRepoWithGoFile with a removal that tolerates a
// Windows failure. The tests below hand `sig integrate` a -resolver that shells
// out to git, and a shell's grandchild is not ours to reap portably: on unix an
// unreaped one cannot stop the directory going away, and on Windows an open
// handle blocks the unlink and fails t.TempDir()'s cleanup AFTER the test has
// already made its assertions. Reaping arbitrary grandchildren of a
// user-supplied resolver would be a product change, not a test fix. What is
// deliberately NOT tolerated here is a failure in the test body: these tests
// cover a guard against silently resetting away someone else's landing, so they
// run on every platform rather than skipping Windows, and only the rmdir is
// best-effort.
func resolverSeamRepo(t *testing.T, files map[string]string) (*gitx.Git, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sig-resolver-seam-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ctx := context.Background()
	g := gitx.New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
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

func TestIntegrateRefusesWhenTheBaseMovedUnderTheResolver(t *testing.T) {
	ctx := context.Background()
	g, base := resolverSeamRepo(t, map[string]string{"shared.txt": "l1\nl2\nl3\nl4\nl5\n"})
	// Same line, two different bodies: a real conflict merge-tree cannot
	// auto-resolve, so the resolver is guaranteed to run.
	mkBranchFrom(t, g, "agent/a", base, map[string]string{"shared.txt": "l1\nl2\na\nl4\nl5\n"})
	mkBranchFrom(t, g, "agent/b", base, map[string]string{"shared.txt": "l1\nl2\nb\nl4\nl5\n"})
	mkBranchFrom(t, g, "other/winner", base, map[string]string{"winner.txt": "winner\n"})
	winner, err := g.RevParse(ctx, "other/winner")
	if err != nil {
		t.Fatal(err)
	}

	// Lands the competing commit, then resolves (union) so the integration
	// really does reach its landing rather than flagging out early.
	resolver := fmt.Sprintf(`git -C %q update-ref refs/heads/main %s && cat "$SIGBOUND_OURS" "$SIGBOUND_THEIRS"`, g.Dir(), winner)
	err = runIntegrate([]string{
		"-repo", g.Dir(), "-base", "main", "-branches", "agent/a,agent/b",
		"-resolver", resolver, "-resolver-timeout", "30s",
	})
	if err == nil {
		t.Fatal("integrate landed onto a base that moved under the resolver; want a refusal")
	}
	if !errors.Is(err, gitx.ErrRefMoved) {
		t.Fatalf("refusal is not ErrRefMoved (so a caller cannot tell a lost race from a broken repo): %v", err)
	}
	if !strings.Contains(err.Error(), short(winner)) || !strings.Contains(err.Error(), short(base)) {
		t.Fatalf("refusal names neither the head that won (%s) nor the one integrated against (%s): %v", short(winner), short(base), err)
	}

	// The whole point: the competing landing is still there, untouched.
	after, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if after != winner {
		t.Fatalf("main = %s, want the competing landing %s — the refused integrate reset it away", after, winner)
	}
	if have := treeFiles(t, g, "main"); !have["winner.txt"] {
		t.Fatalf("the competing landing's file is gone from the base tree: %v", have)
	}
}

// TestIntegrateLandsThroughAResolverWhenTheBaseHeldStill is the negative control
// for the guard above: the SAME conflicting batch and the SAME resolver, minus
// the competing landing, must still land normally. Without it, a guard that
// refused every resolver-using integration would pass the test above.
func TestIntegrateLandsThroughAResolverWhenTheBaseHeldStill(t *testing.T) {
	ctx := context.Background()
	g, base := resolverSeamRepo(t, map[string]string{"shared.txt": "l1\nl2\nl3\nl4\nl5\n"})
	mkBranchFrom(t, g, "agent/a", base, map[string]string{"shared.txt": "l1\nl2\na\nl4\nl5\n"})
	mkBranchFrom(t, g, "agent/b", base, map[string]string{"shared.txt": "l1\nl2\nb\nl4\nl5\n"})

	out := captureStdout(t, func() {
		if err := runIntegrate([]string{
			"-repo", g.Dir(), "-base", "main", "-branches", "agent/a,agent/b",
			"-resolver", `cat "$SIGBOUND_OURS" "$SIGBOUND_THEIRS"`, "-resolver-timeout", "30s",
		}); err != nil {
			t.Fatalf("runIntegrate: %v", err)
		}
	})
	var res resultJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse integrate json: %v\n%s", err, out)
	}
	if len(res.Landed) != 2 {
		t.Fatalf("landed = %v, want both branches resolved and landed", res.Landed)
	}
	after, err := g.RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if after != res.FinalSHA || after == base {
		t.Fatalf("main = %s, want the integrated commit %s (base was %s)", after, res.FinalSHA, base)
	}
}
