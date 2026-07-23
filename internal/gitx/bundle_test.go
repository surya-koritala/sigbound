package gitx

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitExec runs a raw git command in dir with the hermetic env (test setup only,
// like cell_test's listWorktrees — no need to route through g.run).
func gitExec(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = hermeticEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// TestBundleRoundTrip creates a bundle of two branches in repo A, then verifies
// and unbundles it into a fresh, independent repo B. It asserts the three
// properties cell.Export/Import are built on: the carried refs come back
// exactly, unbundle imports the OBJECTS (they resolve in B afterwards), and
// unbundle moves NO ref in B — only the caller decides where refs land.
func TestBundleRoundTrip(t *testing.T) {
	ctx := context.Background()
	a, _ := newRepo(t)
	adir := a.Dir()

	mk := func(branch, file string) string {
		gitExec(t, adir, "checkout", "-q", "-b", branch)
		writeFile(t, adir, file, branch+"\n")
		sha, err := a.CommitAll(ctx, branch+" commit")
		if err != nil {
			t.Fatal(err)
		}
		gitExec(t, adir, "checkout", "-q", "main")
		return sha
	}
	c1 := mk("b1", "b1.txt")
	c2 := mk("b2", "b2.txt")

	bundlePath := filepath.Join(t.TempDir(), "work.bundle")
	if err := a.BundleCreate(ctx, bundlePath, []string{"b1", "b2"}); err != nil {
		t.Fatalf("BundleCreate: %v", err)
	}

	b, _ := newRepo(t)
	before := gitExec(t, b.Dir(), "show-ref")

	if err := b.BundleVerify(ctx, bundlePath); err != nil {
		t.Fatalf("BundleVerify good bundle: %v", err)
	}
	refs, err := b.BundleUnbundle(ctx, bundlePath)
	if err != nil {
		t.Fatalf("BundleUnbundle: %v", err)
	}
	got := map[string]string{}
	for _, r := range refs {
		got[r.Ref] = r.OID
	}
	if got["refs/heads/b1"] != c1 || got["refs/heads/b2"] != c2 {
		t.Fatalf("carried refs = %v, want b1=%s b2=%s", got, c1, c2)
	}
	// Objects landed: the bundled commits resolve in B now.
	for _, sha := range []string{c1, c2} {
		if _, err := b.RevParse(ctx, sha); err != nil {
			t.Fatalf("object %s not imported into B: %v", sha, err)
		}
	}
	// Unbundle moved NO ref in B — the substrate cell.Import's namespace safety
	// stands on.
	if after := gitExec(t, b.Dir(), "show-ref"); after != before {
		t.Fatalf("unbundle changed B refs:\nbefore:\n%safter:\n%s", before, after)
	}
}

// TestBundleVerifyRejectsGarbage locks the loud-failure boundary: a non-bundle
// file, a missing file, and an empty ref list are all errors, not silent no-ops.
func TestBundleVerifyRejectsGarbage(t *testing.T) {
	ctx := context.Background()
	b, _ := newRepo(t)
	tmp := t.TempDir()
	junk := filepath.Join(tmp, "junk.bundle")
	writeFile(t, tmp, "junk.bundle", "this is not a bundle file\n")

	if err := b.BundleVerify(ctx, junk); err == nil {
		t.Fatal("BundleVerify on a non-bundle file: want error")
	}
	if err := b.BundleVerify(ctx, filepath.Join(tmp, "nope.bundle")); err == nil {
		t.Fatal("BundleVerify on a missing file: want error")
	}
	if err := b.BundleCreate(ctx, junk, nil); err == nil {
		t.Fatal("BundleCreate with no refs: want error")
	}
}

func TestParseUnbundleRefs(t *testing.T) {
	good := sha1a + " refs/heads/agent/t1\n" + sha1b + " refs/heads/agent/t2\n"
	refs, err := parseUnbundleRefs(good)
	if err != nil {
		t.Fatalf("parseUnbundleRefs(good): %v", err)
	}
	if len(refs) != 2 || refs[0].OID != sha1a || refs[0].Ref != "refs/heads/agent/t1" {
		t.Fatalf("parseUnbundleRefs(good) = %+v", refs)
	}
	for _, bad := range []string{
		sha1a + "\n",              // oid, no ref
		"refs/heads/x\n",          // ref, no oid
		"notanoid refs/heads/x\n", // non-hex oid
	} {
		if _, err := parseUnbundleRefs(bad); err == nil {
			t.Fatalf("parseUnbundleRefs(%q): want error", bad)
		}
	}
}
