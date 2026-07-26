package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// ---- applyPresets: pure unit tests (the honest seam — no subprocess, no
// runRun flag parsing, just the resolved fs-values in and the resolved
// commands out) ----

// TestApplyPresetsExpandsExactTableStrings pins every preset name's expansion
// to the EXACT string in the {agent,repair,planner,verify}Presets tables —
// docs/USAGE.md's Presets section documents these same strings, so this test
// is what keeps the docs honest.
func TestApplyPresetsExpandsExactTableStrings(t *testing.T) {
	var buf bytes.Buffer
	for name, want := range agentPresets {
		agent, _, _, _, err := applyPresets(&buf, "", name, "", "", "", "", "", "")
		if err != nil {
			t.Fatalf("agent preset %q: %v", name, err)
		}
		if agent != want {
			t.Fatalf("agent preset %q = %q, want %q", name, agent, want)
		}
	}
	for name, want := range repairPresets {
		_, repair, _, _, err := applyPresets(&buf, "", "", "", name, "", "", "", "")
		if err != nil {
			t.Fatalf("repair preset %q: %v", name, err)
		}
		if repair != want {
			t.Fatalf("repair preset %q = %q, want %q", name, repair, want)
		}
	}
	for name, want := range plannerPresets {
		_, _, planner, _, err := applyPresets(&buf, "", "", "", "", "", name, "", "")
		if err != nil {
			t.Fatalf("planner preset %q: %v", name, err)
		}
		if planner != want {
			t.Fatalf("planner preset %q = %q, want %q", name, planner, want)
		}
	}
	for name, want := range verifyPresets {
		_, _, _, verify, err := applyPresets(&buf, "", "", "", "", "", "", "", name)
		if err != nil {
			t.Fatalf("verify preset %q: %v", name, err)
		}
		if verify != want {
			t.Fatalf("verify preset %q = %q, want %q", name, verify, want)
		}
	}
}

// TestApplyPresetsAnnouncesExpansionToStderr: every expansion is printed once
// to the given writer (production wires this to os.Stderr) so a user can see
// and copy exactly what will run.
func TestApplyPresetsAnnouncesExpansionToStderr(t *testing.T) {
	var buf bytes.Buffer
	if _, _, _, _, err := applyPresets(&buf, "", "claude", "", "", "", "", "", ""); err != nil {
		t.Fatalf("applyPresets: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "-agent-preset=claude") || !strings.Contains(got, agentPresets["claude"]) {
		t.Fatalf("stderr announcement = %q, want it to name -agent-preset=claude and the expanded command", got)
	}
}

// TestApplyPresetsRawOverridesPreset: an already-set raw command always wins
// over its preset — even a bogus/unknown preset name never surfaces as an
// error, since it's never even looked up once the raw flag is set (raw wins,
// per issue #17's design).
func TestApplyPresetsRawOverridesPreset(t *testing.T) {
	var buf bytes.Buffer
	rawAgent := "./my-own-agent.sh"
	rawRepair := "./my-own-repair.sh"
	rawPlanner := "./my-own-planner.sh"
	rawVerify := "make check"
	agent, repair, planner, verify, err := applyPresets(&buf,
		rawAgent, "not-a-real-preset",
		rawRepair, "not-a-real-preset",
		rawPlanner, "not-a-real-preset",
		rawVerify, "not-a-real-preset")
	if err != nil {
		t.Fatalf("applyPresets: %v (raw flags should have short-circuited every preset lookup)", err)
	}
	if agent != rawAgent || repair != rawRepair || planner != rawPlanner || verify != rawVerify {
		t.Fatalf("got agent=%q repair=%q planner=%q verify=%q, want the raw commands unchanged", agent, repair, planner, verify)
	}
	if buf.Len() != 0 {
		t.Fatalf("stderr = %q, want no announcement when raw wins (nothing was expanded)", buf.String())
	}
}

// TestApplyPresetsUnknownNameErrorsListingValidNames: an unknown preset name
// (with no raw command to override it) is a loud error naming every valid
// name for that slot, not a silent no-op or a generic parse failure.
func TestApplyPresetsUnknownNameErrorsListingValidNames(t *testing.T) {
	cases := []struct {
		name      string
		call      func() (string, string, string, string, error)
		wantFlag  string
		wantNames []string
	}{
		{
			name: "agent",
			call: func() (string, string, string, string, error) {
				return applyPresets(&bytes.Buffer{}, "", "bogus", "", "", "", "", "", "")
			},
			wantFlag:  "-agent-preset",
			wantNames: []string{"claude", "codex", "aider"},
		},
		{
			name: "repair",
			call: func() (string, string, string, string, error) {
				return applyPresets(&bytes.Buffer{}, "", "", "", "bogus", "", "", "", "")
			},
			wantFlag:  "-repair-preset",
			wantNames: []string{"claude", "codex", "aider"},
		},
		{
			name: "planner",
			call: func() (string, string, string, string, error) {
				return applyPresets(&bytes.Buffer{}, "", "", "", "", "", "bogus", "", "")
			},
			wantFlag:  "-planner-preset",
			wantNames: []string{"claude", "codex", "aider"},
		},
		{
			name: "verify",
			call: func() (string, string, string, string, error) {
				return applyPresets(&bytes.Buffer{}, "", "", "", "", "", "", "", "bogus")
			},
			wantFlag:  "-verify-preset",
			wantNames: []string{"go", "node", "python", "rust", "govulncheck", "gitleaks", "codeql"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, _, err := c.call()
			if err == nil {
				t.Fatalf("%s: want an error for an unknown preset name, got nil", c.name)
			}
			got := err.Error()
			if !strings.Contains(got, c.wantFlag) || !strings.Contains(got, `"bogus"`) {
				t.Fatalf("%s: error=%q, want it to name %s and the bad value", c.name, got, c.wantFlag)
			}
			for _, n := range c.wantNames {
				if !strings.Contains(got, n) {
					t.Fatalf("%s: error=%q, want it to list valid name %q", c.name, got, n)
				}
			}
		})
	}
}

// ---- security verify presets (govulncheck/gitleaks/codeql) ----
//
// These run the REAL expansion through `sh -c`, the way runVerify does, with
// PATH pointing only at a directory the test controls — so "the tool is
// missing" and "the tool is installed" are both constructed conditions, and no
// scanner has to exist on the machine running the suite.

// securityPresetCases pins, per preset, the tool name and the install hint its
// absent-tool failure must name. Duplicated from the table on purpose: this is
// the message a user reads off a red gate, so the test states it independently.
var securityPresetCases = []struct{ preset, tool, install string }{
	{"govulncheck", "govulncheck", "go install golang.org/x/vuln/cmd/govulncheck@latest"},
	{"gitleaks", "gitleaks", "brew install gitleaks"},
	{"codeql", "codeql", "https://github.com/github/codeql-cli-binaries/releases"},
}

// runVerifyPreset executes a -verify-preset expansion exactly as runVerify
// does — `sh -c <expansion>`, cwd = the verify checkout — with PATH replaced by
// pathDir alone, so the test decides what is installed. Returns the combined
// output and the exit error (nil == the gate would pass).
func runVerifyPreset(t *testing.T, preset, dir, pathDir string, env ...string) (string, error) {
	t.Helper()
	expansion, ok := verifyPresets[preset]
	if !ok {
		t.Fatalf("no verify preset %q", preset)
	}
	cmd := exec.Command("sh", "-c", expansion)
	cmd.Dir = dir
	// PATH last: os/exec keeps the LAST value for a duplicated key, so this
	// overrides the inherited PATH rather than racing it.
	cmd.Env = append(append(os.Environ(), env...), "PATH="+pathDir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// installFakeTool puts an executable named tool on the test's PATH dir. It
// succeeds at everything; the one behavior it models is codeql's `database
// analyze` step, which writes $FAKE_CODEQL_RESULTS into the CSV codeqlScan
// inspects — or, with $FAKE_CODEQL_NO_RESULTS_FILE set, writes nothing at all
// while still exiting 0. That is how "the scanner ran and found something" and
// "the scanner reported success without producing results" get exercised
// without a CodeQL install.
func installFakeTool(t *testing.T, pathDir, tool string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = database ] && [ \"$2\" = analyze ] && [ -z \"$FAKE_CODEQL_NO_RESULTS_FILE\" ]; then printf '%s' \"$FAKE_CODEQL_RESULTS\" > .sigbound-codeql.csv; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(pathDir, tool), []byte(script), 0o755); err != nil {
		t.Fatalf("install fake %s: %v", tool, err)
	}
}

// TestSecurityVerifyPresetsFailLoudlyWhenToolAbsent is the whole point of these
// presets: a scanner that isn't installed must FAIL the gate, naming the tool
// and how to install it. The failure mode that must never exist is the quiet
// one — skipping the scan and letting the run report green over a tree nothing
// looked at, which is worse than declaring no scanner at all because the
// receipt then claims a scan that never ran.
func TestSecurityVerifyPresetsFailLoudlyWhenToolAbsent(t *testing.T) {
	requirePOSIXShell(t)
	for _, c := range securityPresetCases {
		t.Run(c.preset, func(t *testing.T) {
			out, err := runVerifyPreset(t, c.preset, t.TempDir(), t.TempDir())
			if err == nil {
				t.Fatalf("-verify-preset=%s exited 0 with %s absent — a skipped scan reports GREEN over an unscanned tree\n%s", c.preset, c.tool, out)
			}
			if !strings.Contains(out, c.tool) {
				t.Fatalf("-verify-preset=%s absent-tool output %q does not name %q", c.preset, out, c.tool)
			}
			if !strings.Contains(out, c.install) {
				t.Fatalf("-verify-preset=%s absent-tool output %q does not say how to install it (want %q)", c.preset, out, c.install)
			}
		})
	}
}

// TestSecurityVerifyPresetsPassWhenToolIsPresentAndClean is the control for the
// test above: with the tool on PATH and nothing to report, the same expansion
// exits 0. Without this, an expansion that failed for any reason at all (a typo
// in the command, a guard that always fires) would still satisfy the
// absent-tool test, which only ever asserts failure.
func TestSecurityVerifyPresetsPassWhenToolIsPresentAndClean(t *testing.T) {
	requirePOSIXShell(t)
	for _, c := range securityPresetCases {
		t.Run(c.preset, func(t *testing.T) {
			pathDir := t.TempDir()
			installFakeTool(t, pathDir, c.tool)
			out, err := runVerifyPreset(t, c.preset, t.TempDir(), pathDir)
			if err != nil {
				t.Fatalf("-verify-preset=%s with %s installed and clean: %v\n%s", c.preset, c.tool, err, out)
			}
		})
	}
}

// TestCodeqlPresetFailsWhenAnalyzeIsNotClean pins the one scanner whose own exit
// code is NOT the verdict: `codeql database analyze` exits 0 whether it found
// results, found nothing, or wrote no file at all. The preset's CSV test is the
// whole verdict, in both directions it has to catch.
func TestCodeqlPresetFailsWhenAnalyzeIsNotClean(t *testing.T) {
	requirePOSIXShell(t)
	cases := []struct{ name, env, why string }{
		{"findings", "FAKE_CODEQL_RESULTS=rule,a finding,src/main.go,1,1", "a row in the results file is a finding; exiting 0 would land on it"},
		{"no results file", "FAKE_CODEQL_NO_RESULTS_FILE=1", "no results file means the scan did not happen, and that must never read as \"found nothing\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pathDir := t.TempDir()
			installFakeTool(t, pathDir, "codeql")
			out, err := runVerifyPreset(t, "codeql", t.TempDir(), pathDir, c.env)
			if err == nil {
				t.Fatalf("-verify-preset=codeql exited 0: %s\n%s", c.why, out)
			}
		})
	}
}

// TestSecurityPresetComposesAfterPolicyBattery: a scanner preset is an ORDINARY
// flag verify by the time policy resolution sees it, so resolvePolicy appends it
// AFTER the repo's own battery members, which keep file order. Ordering is the
// contract being pinned — the repo's build+test runs first, the invoker's
// scanner last — not merely that all three appear.
func TestSecurityPresetComposesAfterPolicyBattery(t *testing.T) {
	var buf bytes.Buffer
	_, _, _, verify, err := applyPresets(&buf, "", "", "", "", "", "", "", "govulncheck")
	if err != nil {
		t.Fatalf("applyPresets: %v", err)
	}
	pol := policy{present: true, verify: []string{"go build ./...", "go test ./..."}}
	p := runParams{VerifyCmd: verify}
	if err := resolvePolicy(pol, &p, 1); err != nil {
		t.Fatal(err)
	}
	build := strings.Index(p.VerifyCmd, "go build ./...")
	test := strings.Index(p.VerifyCmd, "go test ./...")
	scan := strings.Index(p.VerifyCmd, "govulncheck ./...")
	if build < 0 || test < 0 || scan < 0 {
		t.Fatalf("effective verify %q: want the policy battery and the scanner preset all present", p.VerifyCmd)
	}
	if !(build < test && test < scan) {
		t.Fatalf("effective verify %q: want policy members first (in file order) then the scanner appended", p.VerifyCmd)
	}
}

// ---- publish receipt presets (github-receipt / gitlab-receipt) ----
//
// These run the REAL expansion through `sh -c`, the way runPublish does, with
// PATH pointing only at a directory the test controls — so "gh is missing",
// "gh is there but unauthenticated" and "gh works" are all constructed
// conditions, and neither host CLI has to exist on the machine running the
// suite.

// receiptPresetCases pins, per preset, the host CLI, the install hint its
// absent-tool failure must name, and the flags that carry the request's target
// branch, head branch and body. Duplicated from the table on purpose: these are
// the message a user reads off a failed publish step and the request the host
// actually receives, so the test states both independently.
var receiptPresetCases = []struct{ preset, tool, install, subcommand, targetFlag, headFlag, bodyFlag string }{
	{"github-receipt", "gh", "https://cli.github.com", "pr create", "--base", "--head", "--body"},
	{"gitlab-receipt", "glab", "https://gitlab.com/gitlab-org/cli", "mr create", "--target-branch", "--source-branch", "--description"},
}

// receiptPathDir is a PATH containing exactly the real `git` and `awk` the
// expansion needs plus whatever the test installs itself. Replacing PATH rather
// than prepending is what makes "gh is not installed" constructible on a
// machine that has gh — prepending would find the real one.
func receiptPathDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"git", "awk"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not on PATH: %v", tool, err)
		}
		if err := os.Symlink(real, filepath.Join(dir, tool)); err != nil {
			t.Fatalf("link %s into the test PATH: %v", tool, err)
		}
	}
	return dir
}

// receiptRepo builds the shape these presets are for: a repo with a local bare
// remote (`origin`) whose default branch is `main`, plus an `integration`
// branch that carries a commit and has NEVER been pushed. Everything the
// expansion asks the remote — `git ls-remote --symref origin HEAD`, then the
// push — is answered by real git against a real repository, no network. Returns
// the work repo and the bare remote's path.
func receiptRepo(t *testing.T) (repo, remote string) {
	t.Helper()
	root := t.TempDir()
	repo, remote = filepath.Join(root, "repo"), filepath.Join(root, "remote.git")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(root, "init", "--bare", "-q", remote)
	// Set the bare repo's HEAD explicitly rather than relying on `init -b`:
	// this symref IS what `ls-remote --symref` reports, so the fixture states
	// the thing under test instead of inheriting it from a git default.
	git(remote, "symbolic-ref", "HEAD", "refs/heads/main")
	git(root, "init", "-q", repo)
	git(repo, "checkout", "-q", "-B", "main")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", "-A")
	git(repo, "commit", "-qm", "base")
	git(repo, "remote", "add", "origin", remote)
	git(repo, "push", "-q", "origin", "main")
	git(repo, "checkout", "-q", "-b", "integration")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", "-A")
	git(repo, "commit", "-qm", "landed")
	git(repo, "checkout", "-q", "main")
	return repo, remote
}

// remoteRefs lists the bare remote's branches, so a test can assert both that a
// push happened and that one did NOT.
func remoteRefs(t *testing.T, remote string) string {
	t.Helper()
	cmd := exec.Command("git", "--git-dir="+remote, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read remote refs: %v\n%s", err, out)
	}
	return string(out)
}

// runPublishPreset executes a -publish-preset expansion exactly as runPublish
// does — `sh -c <expansion>`, cwd = the repo — with PATH replaced by pathDir
// alone, so the test decides what is installed. Returns the combined output and
// the exit error (nil == the request was opened).
func runPublishPreset(t *testing.T, preset, dir, pathDir string, env ...string) (string, error) {
	t.Helper()
	expansion, ok := publishPresets[preset]
	if !ok {
		t.Fatalf("no publish preset %q", preset)
	}
	cmd := exec.Command("sh", "-c", expansion)
	cmd.Dir = dir
	// PATH last: os/exec keeps the LAST value for a duplicated key, so this
	// overrides the inherited PATH rather than racing it.
	cmd.Env = append(append(os.Environ(), env...), "PATH="+pathDir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// installFakeHostCLI puts an executable named tool on the test's PATH dir,
// standing in for gh/glab. Per invocation it records `<subcommand>` into
// $FAKE_CALLS, every argument one per line into $FAKE_ARGS, the value that
// followed --body/--description verbatim into $FAKE_BODY (so a test can assert
// the receipt arrived as exactly one argument, byte for byte), and the remote's
// branch list AT CALL TIME into $FAKE_REFS — which is what proves the push
// happened BEFORE the request was opened rather than merely alongside it.
// `auth status` exits $FAKE_AUTH_EXIT (default 0), which is how
// "unauthenticated" is constructed.
func installFakeHostCLI(t *testing.T, pathDir, tool string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"printf '%s %s\\n' \"$1\" \"$2\" >> \"$FAKE_CALLS\"\n" +
		"if [ \"$1\" = auth ]; then exit ${FAKE_AUTH_EXIT:-0}; fi\n" +
		"printf '%s\\n' \"$@\" > \"$FAKE_ARGS\"\n" +
		"prev=\n" +
		"for a in \"$@\"; do\n" +
		"  case $prev in --body|--description) printf '%s' \"$a\" > \"$FAKE_BODY\" ;; esac\n" +
		"  prev=$a\n" +
		"done\n" +
		"git --git-dir=\"$FAKE_REMOTE\" for-each-ref --format='%(refname)' > \"$FAKE_REFS\"\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(pathDir, tool), []byte(script), 0o755); err != nil {
		t.Fatalf("install fake %s: %v", tool, err)
	}
}

// receiptEnv is the SIGBOUND_* env plus the fake CLI's recording paths for one
// run of an expansion, with every artifact under dir.
func receiptEnv(dir, baseBranch, receipt string, remote string) []string {
	return []string{
		"FAKE_CALLS=" + filepath.Join(dir, "calls"),
		"FAKE_ARGS=" + filepath.Join(dir, "args"),
		"FAKE_BODY=" + filepath.Join(dir, "body"),
		"FAKE_REFS=" + filepath.Join(dir, "refs"),
		"FAKE_REMOTE=" + remote,
		"SIGBOUND_BASE_BRANCH=" + baseBranch,
		"SIGBOUND_LANDED=agent/a",
		"SIGBOUND_RECEIPT=" + receipt,
	}
}

// TestPublishReceiptPresetsFailLoudlyWhenCLIAbsent: an absent gh/glab must FAIL
// the publish step naming the tool, how to install it, and — because the person
// reading this is staring at a red step wondering what happened to their work —
// that the landing is untouched. The failure mode that must never exist is the
// quiet one: a skipped receipt leaves report.publish claiming ok on a run
// nobody was ever told about.
//
// The output assertions are the whole test. `sh -c` already exits non-zero on a
// missing command, so asserting only the exit code would pass with the entire
// PATH check deleted.
func TestPublishReceiptPresetsFailLoudlyWhenCLIAbsent(t *testing.T) {
	requirePOSIXShell(t)
	for _, c := range receiptPresetCases {
		t.Run(c.preset, func(t *testing.T) {
			out, err := runPublishPreset(t, c.preset, t.TempDir(), receiptPathDir(t))
			if err == nil {
				t.Fatalf("-publish-preset=%s exited 0 with %s absent — a skipped receipt reports a publish that never happened\n%s", c.preset, c.tool, out)
			}
			for _, want := range []string{c.preset, c.tool, c.install, "the landing already happened and still stands"} {
				if !strings.Contains(out, want) {
					t.Fatalf("-publish-preset=%s absent-CLI output %q does not contain %q", c.preset, out, want)
				}
			}
		})
	}
}

// TestPublishReceiptPresetsFailLoudlyWhenUnauthenticated is the second half of
// the same guarantee, and the one `sh -c` gives nothing for free: gh/glab IS on
// PATH, it just has no usable credentials. The receipt must fail saying so and
// naming the fix — before pushing anything and without opening a request.
func TestPublishReceiptPresetsFailLoudlyWhenUnauthenticated(t *testing.T) {
	requirePOSIXShell(t)
	for _, c := range receiptPresetCases {
		t.Run(c.preset, func(t *testing.T) {
			repo, remote := receiptRepo(t)
			pathDir := receiptPathDir(t)
			installFakeHostCLI(t, pathDir, c.tool)
			dir := t.TempDir()
			before := remoteRefs(t, remote)
			out, err := runPublishPreset(t, c.preset, repo, pathDir,
				append(receiptEnv(dir, "integration", "x", remote), "FAKE_AUTH_EXIT=1")...)
			if err == nil {
				t.Fatalf("-publish-preset=%s exited 0 with %s unauthenticated\n%s", c.preset, c.tool, out)
			}
			for _, want := range []string{c.preset, "not authenticated", c.tool + " auth login"} {
				if !strings.Contains(out, want) {
					t.Fatalf("-publish-preset=%s unauthenticated output %q does not contain %q", c.preset, out, want)
				}
			}
			logged, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if strings.Contains(string(logged), c.subcommand) {
				t.Fatalf("-publish-preset=%s ran %q despite failing the auth check: calls=%q", c.preset, c.subcommand, logged)
			}
			if after := remoteRefs(t, remote); after != before {
				t.Fatalf("-publish-preset=%s pushed to the remote despite failing the auth check:\nbefore %q\nafter  %q", c.preset, before, after)
			}
		})
	}
}

// TestPublishReceiptPresetsPushThenOpenRequest is issue #116's actual shape and
// the control for every failure test here: with the CLI installed and
// authenticated and -base on an integration branch, the expansion PUSHES the
// landed branch and then opens a request from it onto the remote's DEFAULT
// branch, carrying the receipt as the body. Without this, an expansion broken
// in any way at all (a typo, a guard that always fires) would still satisfy
// tests that only ever assert failure.
//
// The ordering assertion is the interesting one: the fake CLI records the
// remote's branch list at the moment it is invoked, so "the branch was already
// pushed by then" is checked directly rather than inferred from the `&&`.
func TestPublishReceiptPresetsPushThenOpenRequest(t *testing.T) {
	requirePOSIXShell(t)
	for _, c := range receiptPresetCases {
		t.Run(c.preset, func(t *testing.T) {
			repo, remote := receiptRepo(t)
			pathDir := receiptPathDir(t)
			installFakeHostCLI(t, pathDir, c.tool)
			dir := t.TempDir()
			receipt := "**sigbound receipt** — landed `abc123` onto `integration`."
			out, err := runPublishPreset(t, c.preset, repo, pathDir,
				receiptEnv(dir, "integration", receipt, remote)...)
			if err != nil {
				t.Fatalf("-publish-preset=%s with %s installed and authenticated: %v\n%s", c.preset, c.tool, err, out)
			}
			if refs := remoteRefs(t, remote); !strings.Contains(refs, "refs/heads/integration") {
				t.Fatalf("-publish-preset=%s never pushed the landed branch; remote has:\n%s", c.preset, refs)
			}
			logged, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatalf("read call log: %v", err)
			}
			if !strings.Contains(string(logged), c.subcommand) {
				t.Fatalf("-publish-preset=%s never ran %q: calls=%q", c.preset, c.subcommand, logged)
			}
			atCall, err := os.ReadFile(filepath.Join(dir, "refs"))
			if err != nil {
				t.Fatalf("read remote refs recorded at %s time: %v", c.subcommand, err)
			}
			if !strings.Contains(string(atCall), "refs/heads/integration") {
				t.Fatalf("-publish-preset=%s opened the request BEFORE the push landed on the remote; refs at call time:\n%s", c.preset, atCall)
			}
			args, err := os.ReadFile(filepath.Join(dir, "args"))
			if err != nil {
				t.Fatalf("read %s args: %v", c.tool, err)
			}
			// The request targets the remote's DEFAULT branch and is headed by
			// the landed branch. Both are passed explicitly, so what the guard
			// checked and what the host receives are the same value.
			for _, want := range []string{c.targetFlag + "\nmain\n", c.headFlag + "\nintegration\n", c.bodyFlag + "\n"} {
				if !strings.Contains(string(args), want) {
					t.Fatalf("-publish-preset=%s %s args do not contain %q:\n%s", c.preset, c.tool, want, args)
				}
			}
			got, err := os.ReadFile(filepath.Join(dir, "body"))
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			if string(got) != receipt {
				t.Fatalf("-publish-preset=%s request body %q, want %q", c.preset, got, receipt)
			}
		})
	}
}

// TestPublishReceiptPresetsRefuseToOpenOntoTheDefaultBranch pins the guard that
// exists because of what `-base main` means on a repo whose default IS main:
// there is nothing to open a request against, since the landed branch and the
// target would be the same branch. Both hosts reject that, so the only question
// is whether sigbound finds out BEFORE or AFTER pushing — and the answer has to
// be before, or a run that cannot produce a receipt still moves the remote's
// default branch as a side effect of trying.
//
// The message is half the guard: it has to name what to do instead, because the
// fix is a different -base, not a retry.
func TestPublishReceiptPresetsRefuseToOpenOntoTheDefaultBranch(t *testing.T) {
	requirePOSIXShell(t)
	for _, c := range receiptPresetCases {
		t.Run(c.preset, func(t *testing.T) {
			repo, remote := receiptRepo(t)
			pathDir := receiptPathDir(t)
			installFakeHostCLI(t, pathDir, c.tool)
			dir := t.TempDir()
			// A local commit on main that the remote has NOT seen: if the guard
			// fires only after the push, the remote's main moves and this test
			// says so.
			if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("unpushed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "unpushed"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = repo
				cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			before := remoteRefs(t, remote)
			out, err := runPublishPreset(t, c.preset, repo, pathDir,
				receiptEnv(dir, "main", "x", remote)...)
			if err == nil {
				t.Fatalf("-publish-preset=%s exited 0 with -base main == the remote's default branch\n%s", c.preset, out)
			}
			for _, want := range []string{c.preset, "IS the default branch", "onto itself", "integration branch", "the landing already happened and still stands"} {
				if !strings.Contains(out, want) {
					t.Fatalf("-publish-preset=%s default-branch output %q does not contain %q", c.preset, out, want)
				}
			}
			if after := remoteRefs(t, remote); after != before {
				t.Fatalf("-publish-preset=%s pushed before discovering it could not open a request:\nbefore %q\nafter  %q", c.preset, before, after)
			}
			logged, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if strings.Contains(string(logged), c.subcommand) {
				t.Fatalf("-publish-preset=%s ran %q anyway: calls=%q", c.preset, c.subcommand, logged)
			}
		})
	}
}

// TestPublishReceiptPresetsFailLoudlyWhenRemoteIsUnreadable: the default branch
// is resolved off the remote, so a remote that cannot be read at all — wrong
// name, no credentials, no network — must fail here rather than silently leave
// the target empty and open a request against nothing.
func TestPublishReceiptPresetsFailLoudlyWhenRemoteIsUnreadable(t *testing.T) {
	requirePOSIXShell(t)
	for _, c := range receiptPresetCases {
		t.Run(c.preset, func(t *testing.T) {
			repo, remote := receiptRepo(t)
			pathDir := receiptPathDir(t)
			installFakeHostCLI(t, pathDir, c.tool)
			dir := t.TempDir()
			out, err := runPublishPreset(t, c.preset, repo, pathDir,
				append(receiptEnv(dir, "integration", "x", remote), "SIGBOUND_REMOTE=not-a-remote")...)
			if err == nil {
				t.Fatalf("-publish-preset=%s exited 0 against a remote that does not exist\n%s", c.preset, out)
			}
			for _, want := range []string{c.preset, "not-a-remote", "SIGBOUND_REMOTE"} {
				if !strings.Contains(out, want) {
					t.Fatalf("-publish-preset=%s unreadable-remote output %q does not contain %q", c.preset, out, want)
				}
			}
			logged, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if strings.Contains(string(logged), c.subcommand) {
				t.Fatalf("-publish-preset=%s ran %q with no target branch: calls=%q", c.preset, c.subcommand, logged)
			}
		})
	}
}

// TestPublishReceiptPresetsQuoteTheReceiptBody is the injection test. The
// receipt embeds task prompts, which are arbitrary user prose, and the presets
// hand that prose to a host CLI through a shell. A prompt that ends up EXECUTED
// instead of posted is a critical defect: agents get their prompts from
// planners, issue importers and API callers, so "the goal is trusted" is not a
// property this tool has.
//
// The guarantee is that the body travels as an environment variable referenced
// inside double quotes, so sh expands it without re-scanning the result: every
// metacharacter below is data. Asserting the body arrives BYTE-IDENTICAL as a
// single argument — not merely that nothing exploded — is what makes this test
// non-vacuous: drop the quotes around $SIGBOUND_RECEIPT and word splitting
// hands the CLI a mangled fragment, which this catches.
func TestPublishReceiptPresetsQuoteTheReceiptBody(t *testing.T) {
	requirePOSIXShell(t)
	for _, c := range receiptPresetCases {
		t.Run(c.preset, func(t *testing.T) {
			repo, remote := receiptRepo(t)
			pathDir := receiptPathDir(t)
			installFakeHostCLI(t, pathDir, c.tool)
			dir := t.TempDir()
			pwned := filepath.Join(dir, "pwned")
			// Every way a shell could be talked into running this text:
			// closing the quote, a command list, command substitution both
			// ways, a background fork, a glob, and embedded newlines.
			receipt := "- tasks:\n" +
				"  - `a`: '; touch " + pwned + "; echo '\n" +
				"  - `b`: $(touch " + pwned + ") `touch " + pwned + "` & touch " + pwned + "\n" +
				"  - `c`: rm -rf * ${IFS}\n"
			out, err := runPublishPreset(t, c.preset, repo, pathDir,
				receiptEnv(dir, "integration", receipt, remote)...)
			if err != nil {
				t.Fatalf("-publish-preset=%s: %v\n%s", c.preset, err, out)
			}
			if _, statErr := os.Stat(pwned); statErr == nil {
				t.Fatalf("-publish-preset=%s EXECUTED the receipt body: %s exists", c.preset, pwned)
			}
			got, err := os.ReadFile(filepath.Join(dir, "body"))
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			if string(got) != receipt {
				t.Fatalf("-publish-preset=%s request body %q, want it byte-identical to %q (one argument, no splitting)", c.preset, got, receipt)
			}
		})
	}
}

// TestPublishPresetSlotResolvesLikeEveryOtherSlot: -publish-preset goes through
// presetSlot rather than applyPresets (see applyPresets' doc), so this pins the
// three behaviors that sharing presetSlot is supposed to buy — a name expands
// to the exact table string, a raw -publish wins outright, and an unknown name
// is a loud error listing every valid one.
func TestPublishPresetSlotResolvesLikeEveryOtherSlot(t *testing.T) {
	for name, want := range publishPresets {
		var buf bytes.Buffer
		got, err := presetSlot(&buf, publishPresets, "", name, "-publish", "-publish-preset")
		if err != nil {
			t.Fatalf("publish preset %q: %v", name, err)
		}
		if got != want {
			t.Fatalf("publish preset %q = %q, want %q", name, got, want)
		}
		if !strings.Contains(buf.String(), "-publish-preset="+name) || !strings.Contains(buf.String(), want) {
			t.Fatalf("publish preset %q announcement = %q, want it to name the flag and the expansion", name, buf.String())
		}
	}

	var buf bytes.Buffer
	raw := "git push origin HEAD"
	got, err := presetSlot(&buf, publishPresets, raw, "not-a-real-preset", "-publish", "-publish-preset")
	if err != nil {
		t.Fatalf("raw -publish should short-circuit the preset lookup: %v", err)
	}
	if got != raw || buf.Len() != 0 {
		t.Fatalf("got %q (stderr %q), want the raw command unchanged and no announcement", got, buf.String())
	}

	_, err = presetSlot(&bytes.Buffer{}, publishPresets, "", "bogus", "-publish", "-publish-preset")
	if err == nil {
		t.Fatal("want an error for an unknown -publish-preset name, got nil")
	}
	for _, want := range []string{"-publish-preset", `"bogus"`, "github-receipt", "gitlab-receipt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error=%q, want it to contain %q", err, want)
		}
	}
}

// TestReceiptBodyReportsWhatTheReportRecorded: the receipt is provenance for a
// landing that already happened, so every line of it has to come off the report
// — run id, intent, landed SHA and the base it advanced, verify's verdict, the
// agent tally and what each task was asked to do.
func TestReceiptBodyReportsWhatTheReportRecorded(t *testing.T) {
	rep := runReport{
		Base: "main", BaseSHA: "1111111111111111111111111111111111111111",
		Intent:  "add-metrics",
		Version: "v9.9.9", StartedAt: "2026-07-26T10:00:00Z",
		Tasks:    []taskSpec{{ID: "a", Prompt: "add a counter\nsecond line must not appear"}, {ID: "b", Prompt: "add a gauge"}},
		PerAgent: []perAgentJSON{{ID: "a", OK: true}, {ID: "b", OK: false}},
		Integrate: integrateJSON{
			FinalSHA: "2222222222222222222222222222222222222222",
			Landed:   []string{"agent/a"},
		},
		Verify: verifyJSON{Ran: true, OK: true},
	}
	got := receiptBody("run-42", rep)
	for _, want := range []string{
		"**sigbound receipt**",
		"- run: `run-42`",
		"- intent: `add-metrics`",
		"- landed sha: `2222222222222222222222222222222222222222` (base was `1111111111111111111111111111111111111111`)",
		"- verify: passed",
		"- agents: 1 of 2 succeeded; landed: `agent/a`",
		"  - `a`: add a counter\n",
		"  - `b`: add a gauge\n",
		"sigbound v9.9.9, run started 2026-07-26T10:00:00Z",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("receipt body is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "second line must not appear") {
		t.Errorf("receipt body carried a task prompt past its first line:\n%s", got)
	}

	// A run with no id and no intent simply omits those lines rather than
	// printing empty ones — `sig run` has no run id at all.
	bare := receiptBody("  ", runReport{Base: "main", Verify: verifyJSON{}})
	if strings.Contains(bare, "- run:") || strings.Contains(bare, "- intent:") {
		t.Errorf("receipt body invented a run/intent line for a run that had neither:\n%s", bare)
	}
	if !strings.Contains(bare, "- agents: 0 of 0 succeeded; landed: none") {
		t.Errorf("receipt body should say landed: none rather than render an empty list:\n%s", bare)
	}
}

// TestReceiptBodyIsBounded: a task prompt is arbitrary prose, and the receipt
// becomes one environment variable (Linux caps a single one at 128 KiB) and one
// comment body. Both the per-prompt length and the task COUNT are bounded, and
// what was left out is stated rather than silently dropped. The prompt is cut on
// a rune boundary — a receipt truncated into invalid UTF-8 is not a receipt.
func TestReceiptBodyIsBounded(t *testing.T) {
	rep := runReport{Base: "main"}
	for i := 0; i < receiptMaxTasks+7; i++ {
		rep.Tasks = append(rep.Tasks, taskSpec{ID: fmt.Sprintf("t%d", i), Prompt: strings.Repeat("é", receiptPromptMax+50)})
	}
	got := receiptBody("", rep)
	if !strings.Contains(got, "… and 7 more") {
		t.Errorf("receipt body dropped tasks without saying how many:\n%s", got)
	}
	if strings.Contains(got, "`t20`") {
		t.Errorf("receipt body listed more than receiptMaxTasks tasks:\n%s", got)
	}
	if !strings.Contains(got, strings.Repeat("é", receiptPromptMax)+"...") {
		t.Errorf("receipt body did not truncate an over-long prompt on a rune boundary:\n%s", got)
	}
	if !utf8.ValidString(got) {
		t.Error("receipt body is not valid UTF-8")
	}
}

// TestReceiptVerdictNeverClaimsAnUnrunVerify: "not configured" is its own
// answer. A receipt reading "verify: passed" on a run that had no -verify would
// claim a check that never ran — the same lie the security presets' PATH check
// exists to prevent, one layer further out.
func TestReceiptVerdictNeverClaimsAnUnrunVerify(t *testing.T) {
	cases := []struct {
		v    verifyJSON
		want string
	}{
		{verifyJSON{}, "not configured for this run"},
		{verifyJSON{Ran: true, OK: true}, "passed"},
		{verifyJSON{Ran: true, OK: true, Repaired: true}, "passed (after repair)"},
		{verifyJSON{Ran: true}, "FAILED"},
	}
	for _, c := range cases {
		if got := receiptVerdict(c.v); got != c.want {
			t.Errorf("receiptVerdict(%+v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// TestPresetNamesAreDocumented holds docs/USAGE.md's Presets section to
// the tables. A preset name is a public surface reached by typing it: one that
// exists but is written down nowhere is only findable by reading the source or
// by mistyping a name and reading the error.
func TestPresetNamesAreDocumented(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "USAGE.md"))
	if err != nil {
		t.Fatalf("read USAGE.md: %v", err)
	}
	var section []string
	in, depth, fenced := false, 0, false
	for _, line := range strings.Split(string(data), "\n") {
		// Fenced code blocks first: the section contains shell and policy
		// samples whose comments start with "#", and reading one of those as a
		// level-1 heading silently ENDS the section mid-way — which is how this
		// test can pass while looking at half the docs it claims to check.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			if in {
				section = append(section, line)
			}
			continue
		}
		if !fenced && strings.HasPrefix(line, "#") {
			// The section has its own #### subsections, so it ends at the next
			// heading at or ABOVE its own level, not at the next heading at all.
			level := len(line) - len(strings.TrimLeft(line, "#"))
			if title := strings.TrimSpace(line[level:]); title == "Presets" {
				in, depth = true, level
			} else if in && level <= depth {
				in = false
			}
			continue
		}
		if in {
			section = append(section, line)
		}
	}
	if len(section) == 0 {
		t.Fatal("docs/USAGE.md has no Presets section — this test went blind, it did not find the docs honest")
	}
	body := strings.Join(section, "\n")
	for flag, table := range map[string]map[string]string{"-verify-preset": verifyPresets, "-publish-preset": publishPresets} {
		for name := range table {
			if !strings.Contains(body, "`"+name+"`") {
				t.Errorf("%s %q is not named in docs/USAGE.md's Presets section", flag, name)
			}
		}
	}
	// The receipt presets' expansions are long enough that a docs table row
	// drifts from the code silently. Pin them verbatim, the way the section
	// itself claims to document them ("Exact expansions").
	for name, want := range publishPresets {
		if !strings.Contains(body, want) {
			t.Errorf("-publish-preset %q's EXACT expansion is not in docs/USAGE.md's Presets section; it must read:\n%s", name, want)
		}
	}
}

// TestApplyPresetsEmptyIsNoOp: neither a raw command nor a preset set is not
// an error — every slot but -agent is optional, and applyPresets must never
// invent a requirement runRun itself doesn't have.
func TestApplyPresetsEmptyIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	agent, repair, planner, verify, err := applyPresets(&buf, "", "", "", "", "", "", "", "")
	if err != nil {
		t.Fatalf("applyPresets: %v", err)
	}
	if agent != "" || repair != "" || planner != "" || verify != "" {
		t.Fatalf("got agent=%q repair=%q planner=%q verify=%q, want all empty", agent, repair, planner, verify)
	}
}
