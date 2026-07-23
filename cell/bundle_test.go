package cell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// refSet returns every ref -> sha in the repo (raw show-ref; an empty map when
// the repo has no refs, which show-ref signals with exit 1).
func refSet(t *testing.T, g *gitx.Git) map[string]string {
	t.Helper()
	out, _ := exec.Command("git", "-C", g.Dir(), "show-ref").CombinedOutput()
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			m[fields[1]] = fields[0]
		}
	}
	return m
}

// seedCoordinator builds a fresh coordinator repo whose main points at base,
// carrying ONLY the base objects — dogfooding the bundle transport to move the
// base itself. The returned repo has none of the worker's agent objects until
// something Imports them, so the round-trip really exercises object transport.
func seedCoordinator(t *testing.T, worker *Cell, base string) *gitx.Git {
	t.Helper()
	ctx := context.Background()
	baseBundle := filepath.Join(t.TempDir(), "base.bundle")
	if err := worker.Export(ctx, baseBundle, []string{"main"}); err != nil {
		t.Fatalf("export base: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "coordinator")
	gB := gitx.New(dir)
	if err := gB.Init(ctx); err != nil {
		t.Fatalf("init coordinator: %v", err)
	}
	if _, err := gB.BundleUnbundle(ctx, baseBundle); err != nil {
		t.Fatalf("seed unbundle: %v", err)
	}
	if err := gB.UpdateRef(ctx, "refs/heads/main", base); err != nil {
		t.Fatalf("seed main ref: %v", err)
	}
	return gB
}

// TestExportImportRoundTrip is the distributed round-trip that matters: a worker
// cell (A) exports its agent branches as a bundle; a coordinator cell (B) with
// the same base but NONE of A's agent objects imports the bundle and integrates
// the imported branches with the ordinary engine. The final tree in B must equal
// the tree A would produce integrating locally — the transport is lossless.
func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	const n = 8
	gA, poolA, base := scenario(t, 12)
	changes := spawnAgents(t, poolA, base, n, nil) // fully disjoint

	cA, err := Open(gA.Dir())
	if err != nil {
		t.Fatal(err)
	}
	branchNames := make([]string, n)
	for i, ch := range changes {
		branchNames[i] = ch.Branch
	}

	// Ground truth: what integrating locally in A produces (no land).
	localRes, err := cA.Integrate(ctx, base, changes, StrategyOverlay)
	if err != nil {
		t.Fatalf("local integrate in A: %v", err)
	}
	localTree, err := gA.TreeOID(ctx, localRes.FinalSHA)
	if err != nil {
		t.Fatal(err)
	}

	// Coordinator B seeded with only the base, then given the agent bundle.
	gB := seedCoordinator(t, cA, base)
	cB, err := Open(gB.Dir())
	if err != nil {
		t.Fatal(err)
	}
	agentBundle := filepath.Join(t.TempDir(), "agents.bundle")
	if err := cA.Export(ctx, agentBundle, branchNames); err != nil {
		t.Fatalf("export agents: %v", err)
	}
	imported, err := cB.Import(ctx, agentBundle, "w1")
	if err != nil {
		t.Fatalf("import into B: %v", err)
	}
	if len(imported) != n {
		t.Fatalf("imported %d branches, want %d", len(imported), n)
	}
	for _, ib := range imported {
		if !strings.HasPrefix(ib.Ref, "refs/heads/imported/w1/") {
			t.Fatalf("imported ref %q escaped the w1 namespace", ib.Ref)
		}
	}

	// Integrate the imported branches in B with the SAME engine (composition:
	// no new integration path — the imported refs are just branch names).
	bChanges := make([]BranchChange, len(imported))
	for i, ib := range imported {
		ws, err := WriteSetFor(ctx, cB.Git(), base, ib.Ref)
		if err != nil {
			t.Fatalf("write-set for %s: %v", ib.Ref, err)
		}
		bChanges[i] = BranchChange{Branch: ib.Ref, WriteSet: ws}
	}
	bRes, err := cB.Integrate(ctx, base, bChanges, StrategyOverlay)
	if err != nil {
		t.Fatalf("integrate imported in B: %v", err)
	}
	if len(bRes.Landed) != n || len(bRes.Flagged) != 0 {
		t.Fatalf("B landed=%d flagged=%d, want %d/0", len(bRes.Landed), len(bRes.Flagged), n)
	}
	bTree, err := cB.Git().TreeOID(ctx, bRes.FinalSHA)
	if err != nil {
		t.Fatal(err)
	}
	if localTree != bTree {
		t.Fatalf("transport lossy: A local tree %s != B imported tree %s", localTree, bTree)
	}
	assertAllLanded(t, cB.Git(), bRes.FinalSHA, n)
}

// TestImportNamespaceSafety is the safety invariant: a bundle carrying a ref
// literally named "main" (at a different commit) must NOT move the coordinator's
// main, and must touch no ref outside the import namespace. It lands only under
// refs/heads/imported/<worker>/.
func TestImportNamespaceSafety(t *testing.T) {
	ctx := context.Background()
	gA, _, base := scenario(t, 3)
	cA, err := Open(gA.Dir())
	if err != nil {
		t.Fatal(err)
	}

	// Seed B with main == base BEFORE A's main moves.
	gB := seedCoordinator(t, cA, base)
	cB, err := Open(gB.Dir())
	if err != nil {
		t.Fatal(err)
	}

	// Advance A's main to a different commit, then bundle it AS "main".
	if err := os.WriteFile(filepath.Join(gA.Dir(), "evil.txt"), []byte("evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	evilMain, err := gA.CommitAll(ctx, "advance A main")
	if err != nil {
		t.Fatal(err)
	}
	if evilMain == base {
		t.Fatal("advancing A main did not change its commit")
	}
	evilBundle := filepath.Join(t.TempDir(), "evil.bundle")
	if err := cA.Export(ctx, evilBundle, []string{"main"}); err != nil {
		t.Fatal(err)
	}

	before := refSet(t, cB.Git())
	imported, err := cB.Import(ctx, evilBundle, "evil")
	if err != nil {
		t.Fatalf("import evil bundle: %v", err)
	}

	// B's main is untouched.
	mainNow, err := cB.Git().RevParse(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mainNow != base {
		t.Fatalf("import moved B's main to %s (base was %s)", mainNow, base)
	}
	// The carried "main" landed only under the namespace, at evilMain.
	if len(imported) != 1 || imported[0].Ref != "refs/heads/imported/evil/main" || imported[0].SHA != evilMain {
		t.Fatalf("imported = %+v, want one ref refs/heads/imported/evil/main at %s", imported, evilMain)
	}
	// The ONLY ref that changed is the namespaced one; everything else identical.
	after := refSet(t, cB.Git())
	for ref, sha := range before {
		if after[ref] != sha {
			t.Fatalf("import changed pre-existing ref %s: %s -> %s", ref, sha, after[ref])
		}
	}
	for ref := range after {
		if _, existed := before[ref]; !existed && !strings.HasPrefix(ref, "refs/heads/imported/evil/") {
			t.Fatalf("import created ref outside the namespace: %s", ref)
		}
	}
}

// TestImportCorruptBundleFails proves a bad bundle imports nothing: a non-bundle
// file fails at verify, and a header-intact but truncated bundle fails at
// unbundle. Either way Import errors and no ref is created.
func TestImportCorruptBundleFails(t *testing.T) {
	ctx := context.Background()
	g, pool, base := scenario(t, 3)
	c, err := Open(g.Dir())
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()

	// (a) A non-bundle file: rejected at verify.
	junk := filepath.Join(tmp, "junk.bundle")
	if err := os.WriteFile(junk, []byte("this is not a bundle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := refSet(t, c.Git())
	if _, err := c.Import(ctx, junk, "w"); err == nil {
		t.Fatal("import of a non-bundle file: want error")
	}
	if got := refSet(t, c.Git()); len(got) != len(before) {
		t.Fatal("failed import of junk still changed refs")
	}

	// (b) A truncated but header-intact bundle: rejected at unbundle (index-pack).
	changes := spawnAgents(t, pool, base, 2, nil)
	good := filepath.Join(tmp, "good.bundle")
	if err := c.Export(ctx, good, []string{changes[0].Branch, changes[1].Branch}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	before = refSet(t, c.Git())
	if _, err := c.Import(ctx, good, "w2"); err == nil {
		t.Fatal("import of a truncated bundle: want error")
	}
	if got := refSet(t, c.Git()); len(got) != len(before) {
		t.Fatal("failed import of a truncated bundle still changed refs")
	}
}

// TestReimportIdempotent picks the idempotent semantics: importing the SAME
// bundle under the SAME worker id twice is a no-op re-run — same result, no ref
// churn. (Also re-checks safety: importing a bundle carrying agent/* into a repo
// that already HAS those agent/* branches must not move them.)
func TestReimportIdempotent(t *testing.T) {
	ctx := context.Background()
	g, pool, base := scenario(t, 3)
	c, err := Open(g.Dir())
	if err != nil {
		t.Fatal(err)
	}
	changes := spawnAgents(t, pool, base, 2, nil)
	bundle := filepath.Join(t.TempDir(), "w.bundle")
	if err := c.Export(ctx, bundle, []string{changes[0].Branch, changes[1].Branch}); err != nil {
		t.Fatal(err)
	}

	imp1, err := c.Import(ctx, bundle, "w1")
	if err != nil {
		t.Fatal(err)
	}
	refs1 := refSet(t, c.Git())
	imp2, err := c.Import(ctx, bundle, "w1")
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	refs2 := refSet(t, c.Git())

	if !sameImports(imp1, imp2) {
		t.Fatalf("re-import returned a different result:\n%+v\n%+v", imp1, imp2)
	}
	if len(refs1) != len(refs2) {
		t.Fatalf("re-import changed the ref count: %d -> %d", len(refs1), len(refs2))
	}
	for ref, sha := range refs1 {
		if refs2[ref] != sha {
			t.Fatalf("re-import moved ref %s: %s -> %s", ref, sha, refs2[ref])
		}
	}
	// The worker's real agent/* branches were never touched by importing a bundle
	// that carried them.
	for _, ch := range changes {
		if _, ok := refs2["refs/heads/"+ch.Branch]; !ok {
			t.Fatalf("agent branch %s vanished after import", ch.Branch)
		}
	}
}

func sameImports(a, b []ImportedBranch) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(xs []ImportedBranch) []string {
		out := make([]string, len(xs))
		for i, x := range xs {
			out[i] = x.Original + "|" + x.Ref + "|" + x.SHA
		}
		sort.Strings(out)
		return out
	}
	ka, kb := key(a), key(b)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

// TestExportMissingBranch: a missing branch is a clean error naming it, and no
// bundle file is written.
func TestExportMissingBranch(t *testing.T) {
	ctx := context.Background()
	g, _, _ := scenario(t, 1)
	c, err := Open(g.Dir())
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "x.bundle")
	err = c.Export(ctx, bundle, []string{"main", "no-such-branch"})
	if err == nil {
		t.Fatal("export of a missing branch: want error")
	}
	if !strings.Contains(err.Error(), "no-such-branch") {
		t.Fatalf("error should name the missing branch, got: %v", err)
	}
	if _, statErr := os.Stat(bundle); !os.IsNotExist(statErr) {
		t.Fatalf("bundle should not be written on validation failure (stat err=%v)", statErr)
	}
}

// TestWorkerIDValidation covers the default-from-filename and the reject-unsafe
// paths without needing a real bundle.
func TestWorkerIDValidation(t *testing.T) {
	for in, want := range map[string]string{
		"/x/worker-a.bundle": "worker-a",
		"agents.bundle":      "agents",
		"noext":              "noext",
		"a.b.bundle":         "a.b",
	} {
		if got := bundleStem(in); got != want {
			t.Fatalf("bundleStem(%q) = %q, want %q", in, got, want)
		}
	}
	for _, ok := range []string{"w1", "worker-a", "a.b_c", "Node07"} {
		if !workerIDSafe(ok) {
			t.Fatalf("workerIDSafe(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "has space", "up/../down", "emoji😀"} {
		if workerIDSafe(bad) {
			t.Fatalf("workerIDSafe(%q) = true, want false", bad)
		}
	}
}
