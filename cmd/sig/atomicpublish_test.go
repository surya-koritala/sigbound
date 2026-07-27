package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// atomicFixture is a working repo with a commit, a note on it, and a bare
// remote — the real thing, because this code path only runs when a remote is
// configured, which is exactly what makes it easy to leave untested.
type atomicFixture struct {
	t          *testing.T
	repo       string
	remote     string
	head       string
	remoteHead string // where the remote's main sat before the push under test
}

func (f *atomicFixture) git(dir string, args ...string) (string, error) {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (f *atomicFixture) mustGit(dir string, args ...string) string {
	f.t.Helper()
	out, err := f.git(dir, args...)
	if err != nil {
		f.t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

func newAtomicFixture(t *testing.T) *atomicFixture {
	t.Helper()
	requirePOSIXShell(t)
	root := t.TempDir()
	f := &atomicFixture{t: t, repo: filepath.Join(root, "work"), remote: filepath.Join(root, "remote.git")}

	f.mustGit(root, "init", "--bare", "-q", f.remote)
	f.mustGit(f.remote, "symbolic-ref", "HEAD", "refs/heads/main")
	f.mustGit(root, "init", "-q", f.repo)
	for _, kv := range [][2]string{{"gc.auto", "0"}, {"maintenance.auto", "false"}} {
		f.mustGit(f.repo, "config", kv[0], kv[1])
	}
	f.mustGit(f.repo, "checkout", "-q", "-B", "main")

	if err := os.WriteFile(filepath.Join(f.repo, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.mustGit(f.repo, "add", "-A")
	f.mustGit(f.repo, "commit", "-qm", "base")
	f.mustGit(f.repo, "remote", "add", "origin", f.remote)
	f.mustGit(f.repo, "push", "-q", "origin", "main")
	f.remoteHead = f.mustGit(f.remote, "rev-parse", "refs/heads/main")

	// The landing, plus the provenance note that is its evidence.
	if err := os.WriteFile(filepath.Join(f.repo, "f.txt"), []byte("landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.mustGit(f.repo, "add", "-A")
	f.mustGit(f.repo, "commit", "-qm", "landed")
	f.head = f.mustGit(f.repo, "rev-parse", "HEAD")
	f.mustGit(f.repo, "notes", "--ref=sigbound", "add", "-m", `{"noteFormat":1,"repo":"x"}`, f.head)
	return f
}

// TestAtomicPublishMovesBranchAndNoteTogether: the whole point. A landing on a
// remote has to arrive WITH its evidence.
//
// The note is the only record that survives on the remote when the machine that
// ran the work is gone. Anything recovering from an interrupted run fetches the
// base and looks for that run's note; if the branch moved and the note did not,
// the answer is "no" and the caller re-runs work that already landed.
func TestAtomicPublishMovesBranchAndNoteTogether(t *testing.T) {
	f := newAtomicFixture(t)

	out, err := f.git(f.repo, "push", "--atomic", "--porcelain", "origin", "main", "refs/notes/sigbound")
	if err != nil {
		t.Fatalf("atomic push failed: %v\n%s", err, out)
	}

	if got := f.mustGit(f.remote, "rev-parse", "refs/heads/main"); got != f.head {
		t.Fatalf("remote main=%s, want %s", short(got), short(f.head))
	}
	if _, gerr := f.git(f.remote, "rev-parse", "refs/notes/sigbound"); gerr != nil {
		t.Fatal("the branch reached the remote without its provenance note; a recovering caller cannot tell this run landed")
	}
}

// TestAtomicPublishMovesNeitherWhenTheNoteIsRefused is the property, FORCED
// rather than reasoned about. A pre-receive hook on the remote refuses
// refs/notes/* — which is exactly what a token that can write branches but not
// notes produces in the wild.
//
// Without --atomic the branch would land and the note would not, leaving the
// precise state this exists to prevent. The assertion is that the REMOTE BRANCH
// DID NOT MOVE.
func TestAtomicPublishMovesNeitherWhenTheNoteIsRefused(t *testing.T) {
	f := newAtomicFixture(t)

	// The `update` hook, NOT `pre-receive`. That distinction is the entire test:
	// pre-receive runs once for the whole push and its exit code rejects
	// everything, so a test built on it passes with or without --atomic and
	// proves nothing. `update` runs PER REF and refuses only the ref it is given
	// — which is what a real per-namespace permission looks like, and the only
	// setup where --atomic is what stops the branch from landing alone.
	hook := filepath.Join(f.remote, "hooks", "update")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$1\" in refs/notes/*) echo \"notes are not writable by this token\" >&2; exit 1;; esac\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := f.git(f.repo, "push", "--atomic", "--porcelain", "origin", "main", "refs/notes/sigbound")
	if err == nil {
		t.Fatalf("the push succeeded although the remote refused the notes ref:\n%s", out)
	}

	// THE assertion. Either both refs move or neither does.
	got := f.mustGit(f.remote, "rev-parse", "refs/heads/main")
	if got != f.remoteHead {
		t.Fatalf("remote main moved to %s despite the transaction failing (was %s): the branch landed without its evidence, which is the exact state --atomic exists to prevent",
			short(got), short(f.remoteHead))
	}
	if _, gerr := f.git(f.remote, "rev-parse", "refs/notes/sigbound"); gerr == nil {
		t.Fatal("the refused notes ref exists on the remote")
	}

	// And the failure names which ref was refused, from git's own porcelain
	// output rather than from prose anyone had to parse.
	if !strings.Contains(out, "refs/notes/sigbound") {
		t.Fatalf("the failure does not name the refused ref, so a caller cannot tell a notes-permission problem from a moved base:\n%s", out)
	}
}

// TestPublishPresetSkipsAMissingNotesRef: -notes can be off, and naming a ref
// that does not exist would fail every publish for a repo that never wrote one.
// The preset's guard is `git show-ref --verify --quiet`, so this pins that a
// repo with no note still pushes its branch.
func TestPublishPresetSkipsAMissingNotesRef(t *testing.T) {
	f := newAtomicFixture(t)
	f.mustGit(f.repo, "update-ref", "-d", "refs/notes/sigbound")

	// The preset's own shell, verbatim in shape: collect the refs, include the
	// note only if it exists, one --atomic transaction.
	cmd := exec.Command("sh", "-c",
		`sb_refs="main"; git show-ref --verify --quiet refs/notes/sigbound && sb_refs="$sb_refs refs/notes/sigbound"; `+
			`git push --atomic --porcelain origin $sb_refs`)
	cmd.Dir = f.repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	raw, err := cmd.CombinedOutput()
	out := string(raw)
	if err != nil {
		t.Fatalf("publish failed for a repo with no note: %v\n%s", err, out)
	}
	if got := f.mustGit(f.remote, "rev-parse", "refs/heads/main"); got != f.head {
		t.Fatalf("remote main=%s, want %s", short(got), short(f.head))
	}
}
