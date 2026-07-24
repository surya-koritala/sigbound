package cell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// requirePOSIXShell skips a resolver test on Windows: every CommandResolver
// here is driven through `sh -c` with POSIX semantics (git merge-file, `env`,
// `sleep`, `exit N`), which Windows CI does not provide. The resolver
// primitive itself is platform-agnostic — it execs whatever argv it is given —
// so this skips only these shell-scripted tests, not the feature. See #94.
func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("resolver test drives a command via `sh -c`; needs a POSIX shell, unix-only (issue #94)")
	}
}

// conflictAgent creates one agent that writes a private file AND rewrites line 5
// of the shared hot file f000.txt to marker. Two such agents therefore change the
// SAME line differently => a real 3-way conflict merge-tree cannot auto-resolve.
// Built synchronously so the returned changes are in a deterministic order
// (index 0 lands first, later ones must be resolved or flagged).
func conflictAgent(t *testing.T, pool *WorktreePool, base string, i int, marker string) BranchChange {
	t.Helper()
	ctx := context.Background()
	wt, err := pool.Acquire(ctx, base)
	if err != nil {
		t.Fatalf("acquire agent %d: %v", i, err)
	}
	priv := fmt.Sprintf("agent_%03d.txt", i)
	if err := os.WriteFile(filepath.Join(wt.Dir, priv), []byte(fmt.Sprintf("agent %d\n", i)), 0o644); err != nil {
		t.Fatalf("agent %d private: %v", i, err)
	}
	editDistinctLine("f000.txt", marker, 5)(wt.Dir)
	if _, err := wt.Git().CommitAll(ctx, fmt.Sprintf("agent %d", i)); err != nil {
		t.Fatalf("agent %d commit: %v", i, err)
	}
	ws, err := WriteSetFor(ctx, pool.Git(), base, wt.Branch)
	if err != nil {
		t.Fatalf("agent %d writeset: %v", i, err)
	}
	if err := pool.Release(ctx, wt); err != nil {
		t.Fatalf("agent %d release: %v", i, err)
	}
	return BranchChange{Branch: wt.Branch, WriteSet: ws}
}

const (
	markerA = "AAA-agent0-was-here"
	markerB = "BBB-agent1-was-here"
)

// unionResolver runs `git merge-file --union` over the three temp files the
// CommandResolver exposes, producing a clean union of both additive edits with
// no conflict markers. This is a real external-command resolver — the
// bring-your-own-model seam, here scripted with a merge tool.
func unionResolver() *CommandResolver {
	return &CommandResolver{
		Args: []string{"sh", "-c",
			`git merge-file -p --union "$SIGBOUND_OURS" "$SIGBOUND_BASE" "$SIGBOUND_THEIRS"`},
		Timeout: 10 * time.Second,
	}
}

// TestResolverUnionLands is case (a): a scripted resolver that correctly unions
// two conflicting additive edits => the conflicting branch LANDS and the final
// tree carries BOTH changes.
func TestResolverUnionLands(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	g, pool, base := scenario(t, 4)

	changes := []BranchChange{
		conflictAgent(t, pool, base, 0, markerA),
		conflictAgent(t, pool, base, 1, markerB),
	}

	in := NewIntegrator(pool.Git()).WithResolver(unionResolver())
	res, err := in.IntegrateOCC(ctx, base, changes)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}

	// Both branches share f000.txt => a single group of two; both must land.
	if res.Groups != 1 {
		t.Fatalf("groups=%d, want 1", res.Groups)
	}
	if len(res.Landed) != 2 || len(res.Flagged) != 0 {
		t.Fatalf("landed=%d flagged=%d, want 2/0", len(res.Landed), len(res.Flagged))
	}
	assertAllLanded(t, g, res.FinalSHA, 2)

	// The resolved hot file must contain BOTH agents' markers and no leftover
	// conflict markers.
	got, err := g.ShowFile(ctx, res.FinalSHA, "f000.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, markerA) || !strings.Contains(got, markerB) {
		t.Fatalf("resolved f000.txt missing a marker:\n%s", got)
	}
	if strings.Contains(got, "<<<<<<<") || strings.Contains(got, ">>>>>>>") {
		t.Fatalf("resolved f000.txt still has conflict markers:\n%s", got)
	}
}

// TestResolverBadResultFlags is case (b): a resolver that emits garbage / exits
// non-zero / emits nothing NEVER lands — the branch stays flagged and the final
// tree is byte-for-byte the same as with no resolver at all (main uncorrupted).
func TestResolverBadResultFlags(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	g, pool, base := scenario(t, 4)

	changes := []BranchChange{
		conflictAgent(t, pool, base, 0, markerA),
		conflictAgent(t, pool, base, 1, markerB),
	}

	// Baseline: no resolver. Agent 0 lands, agent 1 is flagged.
	baseline, err := NewIntegrator(pool.Git()).IntegrateOCC(ctx, base, changes)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if len(baseline.Landed) != 1 || len(baseline.Flagged) != 1 {
		t.Fatalf("baseline landed=%d flagged=%d, want 1/1", len(baseline.Landed), len(baseline.Flagged))
	}
	wantTree := treeHash(t, g, baseline.FinalSHA)

	bad := map[string]*CommandResolver{
		"garbage-nonzero": {Args: []string{"sh", "-c", "echo '<<<<< GARBAGE not a real merge >>>>>'; exit 1"}},
		"empty-stdout":    {Args: []string{"sh", "-c", "exit 0"}},
	}
	for name, r := range bad {
		t.Run(name, func(t *testing.T) {
			res, err := NewIntegrator(pool.Git()).WithResolver(r).IntegrateOCC(ctx, base, changes)
			if err != nil {
				t.Fatalf("integrate: %v", err)
			}
			if len(res.Landed) != 1 || len(res.Flagged) != 1 {
				t.Fatalf("landed=%d flagged=%d, want 1/1 (branch must stay flagged)", len(res.Landed), len(res.Flagged))
			}
			if len(res.Flagged[0].Conflicts) != 1 || res.Flagged[0].Conflicts[0] != "f000.txt" {
				t.Fatalf("flagged conflicts=%v, want [f000.txt]", res.Flagged[0].Conflicts)
			}
			// Final tree identical to the no-resolver run: nothing garbage landed.
			if got := treeHash(t, g, res.FinalSHA); got != wantTree {
				t.Fatalf("final tree changed by a declining resolver (main corrupted)")
			}
			// And the hot file holds only agent 0's clean edit — no markers, no
			// garbage from the failed resolver.
			hot, err := g.ShowFile(ctx, res.FinalSHA, "f000.txt")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(hot, markerA) || strings.Contains(hot, markerB) {
				t.Fatalf("hot file not the clean agent-0 version:\n%s", hot)
			}
			if strings.Contains(hot, "GARBAGE") || strings.Contains(hot, "<<<<<<<") {
				t.Fatalf("garbage/markers leaked into landed tree:\n%s", hot)
			}
		})
	}
}

// TestResolverTimeoutFlags is case (c): a resolver that hangs past its timeout is
// killed and treated as a decline, so the branch is flagged and main is intact.
func TestResolverTimeoutFlags(t *testing.T) {
	requirePOSIXShell(t)
	ctx := context.Background()
	g, pool, base := scenario(t, 4)

	changes := []BranchChange{
		conflictAgent(t, pool, base, 0, markerA),
		conflictAgent(t, pool, base, 1, markerB),
	}

	slow := &CommandResolver{
		Args:    []string{"sh", "-c", "sleep 30"},
		Timeout: 150 * time.Millisecond,
	}
	start := time.Now()
	res, err := NewIntegrator(pool.Git()).WithResolver(slow).IntegrateOCC(ctx, base, changes)
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("resolver was not bounded by its timeout: took %v", elapsed)
	}
	if len(res.Landed) != 1 || len(res.Flagged) != 1 {
		t.Fatalf("landed=%d flagged=%d, want 1/1 (timeout must flag)", len(res.Landed), len(res.Flagged))
	}
	if len(res.Flagged[0].Conflicts) != 1 || res.Flagged[0].Conflicts[0] != "f000.txt" {
		t.Fatalf("flagged conflicts=%v, want [f000.txt]", res.Flagged[0].Conflicts)
	}
	// Only agent 0's clean edit landed.
	hot, err := g.ShowFile(ctx, res.FinalSHA, "f000.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hot, markerA) || strings.Contains(hot, markerB) {
		t.Fatalf("hot file not the clean agent-0 version after timeout:\n%s", hot)
	}
}

// ---- Env (issue #56: scoped environments) ----------------------------------

// TestCommandResolverEnvFieldScopesEnvironment: a non-nil Env replaces
// os.Environ() as the resolver command's base environment — a variable set
// in THIS test process (standing in for another tenant's secret in a hosted
// caller) does not reach the command, while the per-conflict SIGBOUND_* vars
// and whatever Env itself named still do. The resolver command is `env`
// itself, so its own stdout (captured as the "resolved" body) IS the
// environment it saw — no temp file needed to inspect it.
func TestCommandResolverEnvFieldScopesEnvironment(t *testing.T) {
	requirePOSIXShell(t)
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	r := &CommandResolver{
		Args: []string{"sh", "-c", "env"},
		Env:  []string{"PATH=" + os.Getenv("PATH")},
	}
	resolved, ok, err := r.Resolve(context.Background(), Conflict{Path: "f.txt", Base: "b", Ours: "o", Theirs: "t"})
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if strings.Contains(resolved, "SIGBOUND_TEST_CANARY") {
		t.Fatalf("scoped Env leaked SIGBOUND_TEST_CANARY:\n%s", resolved)
	}
	if !strings.Contains(resolved, "PATH=") {
		t.Fatalf("scoped Env dropped PATH, which it explicitly named:\n%s", resolved)
	}
	if !strings.Contains(resolved, "SIGBOUND_PATH=f.txt") {
		t.Fatalf("per-conflict SIGBOUND_PATH missing from a scoped Env:\n%s", resolved)
	}
}

// TestCommandResolverEnvNilKeepsLibraryDefault: Env left nil (the zero
// value) is today's behavior, unchanged — the full os.Environ(), same as
// before this field existed.
func TestCommandResolverEnvNilKeepsLibraryDefault(t *testing.T) {
	requirePOSIXShell(t)
	t.Setenv("SIGBOUND_TEST_CANARY", "leak-me")
	r := &CommandResolver{Args: []string{"sh", "-c", "env"}}
	resolved, ok, err := r.Resolve(context.Background(), Conflict{Path: "f.txt"})
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(resolved, "SIGBOUND_TEST_CANARY=leak-me") {
		t.Fatalf("nil Env should default to the full os.Environ() (today's behavior):\n%s", resolved)
	}
}

// TestCommandResolverEnvConcurrentCallsDoNotCorruptEachOther: the integrator
// folds independent conflict groups in parallel (see combineDisjoint /
// Integrator's group goroutines), so Resolve can be called concurrently many
// times on the SAME *CommandResolver — and therefore the same r.Env slice.
// Each concurrent call must see its OWN conflict's SIGBOUND_PATH, never
// another call's. env is built with spare capacity (make(..., 1, 8)) so a
// naive `append(r.Env, ...)` would have room to write in place into r.Env's
// shared backing array; Resolve's capacity clamp must force every call to
// allocate its own array instead. Run under `go test -race` this also
// catches the data race directly, not just a wrong-value symptom.
func TestCommandResolverEnvConcurrentCallsDoNotCorruptEachOther(t *testing.T) {
	requirePOSIXShell(t)
	base := make([]string, 1, 8) // len 1, cap 8: deliberate spare capacity
	base[0] = "PATH=" + os.Getenv("PATH")
	r := &CommandResolver{Args: []string{"sh", "-c", `printf 'SEEN:%s' "$SIGBOUND_PATH"`}}
	r.Env = base

	const n = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("path-%03d.txt", i)
			resolved, ok, err := r.Resolve(context.Background(), Conflict{Path: path})
			want := "SEEN:" + path
			if err != nil || !ok || resolved != want {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("call %d: ok=%v err=%v resolved=%q, want %q", i, ok, err, resolved, want))
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("%d/%d concurrent Resolve calls saw the wrong SIGBOUND_PATH (shared Env corrupted):\n%s", len(failures), n, strings.Join(failures, "\n"))
	}
}
