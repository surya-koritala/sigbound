package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// initPolicyRepo builds a temp git repo holding files (repo-relative path ->
// content) and commits them, so `sig policy init` has a committed tree to read.
func initPolicyRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	g := gitx.New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.CommitAll(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	return dir
}

// policyInit runs the command against repo and returns stdout, the exit code,
// the error, and the file it wrote (empty when it wrote nothing).
func policyInit(t *testing.T, repo string) (stdout string, code int, err error, written string) {
	t.Helper()
	var buf bytes.Buffer
	code, err = runPolicyInit(&buf, []string{"-repo", repo})
	b, rerr := os.ReadFile(filepath.Join(repo, policyFileName))
	if rerr == nil {
		written = string(b)
	}
	return buf.String(), code, err, written
}

// mustParse asserts the drafted file parses — the invariant the command
// self-checks, restated at every call site that produces a draft.
func mustParse(t *testing.T, draft string) policy {
	t.Helper()
	pol, err := parsePolicy([]byte(draft))
	if err != nil {
		t.Fatalf("drafted policy does not parse: %v\n%s", err, draft)
	}
	return pol
}

const ciWorkflow = `name: CI
on:
  push:
  pull_request:
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
      - run: go build ./...
      - run: go test ./...
`

// TestPolicyInitGoRepoWithWorkflow is the headline acceptance: on a Go repo
// with a CI workflow the drafted battery is exactly the workflow's run steps,
// in order, each attributed to its source — AND it actually passes when run.
func TestPolicyInitGoRepoWithWorkflow(t *testing.T) {
	requirePOSIXShell(t)
	repo := initPolicyRepo(t, map[string]string{
		"go.mod":                     "module example.com/init\n\ngo 1.21\n",
		"main.go":                    "package main\n\nfunc main() {}\n",
		"main_test.go":               "package main\n\nimport \"testing\"\n\nfunc TestNothing(t *testing.T) {}\n",
		".github/workflows/ci.yml":   ciWorkflow,
		".github/workflows/other.md": "not a workflow\n",
	})
	out, code, err, draft := policyInit(t, repo)
	if err != nil || code != exitOK {
		t.Fatalf("policy init: code=%d err=%v\n%s", code, err, out)
	}
	pol := mustParse(t, draft)
	want := []string{"go build ./...", "go test ./..."}
	if len(pol.verify) != len(want) {
		t.Fatalf("verify battery = %q, want %q\n%s", pol.verify, want, draft)
	}
	for i := range want {
		if pol.verify[i] != want[i] {
			t.Fatalf("verify[%d] = %q, want %q", i, pol.verify[i], want[i])
		}
	}
	// Every emitted line is attributed to the file it came from.
	for _, line := range strings.Split(draft, "\n") {
		if !strings.HasPrefix(line, "verify = ") && !strings.HasPrefix(line, "ack-paths = ") {
			continue
		}
		if !strings.Contains(draft[:strings.Index(draft, line)], ".github/workflows/ci.yml") {
			t.Fatalf("emitted line %q has no source attribution above it\n%s", line, draft)
		}
	}
	if !strings.Contains(draft, policyInitSelfProtectNote) {
		t.Fatalf("draft header dropped the self-protection sentence\n%s", draft)
	}
	for _, action := range []string{"actions/checkout@v4", "actions/setup-go@v5"} {
		if n := strings.Count(draft, "# unmapped: workflows: .github/workflows/ci.yml"); n < 2 {
			t.Fatalf("want an unmapped entry per uses: step, got %d\n%s", n, draft)
		}
		if !strings.Contains(draft, action) {
			t.Fatalf("no unmapped entry naming %s\n%s", action, draft)
		}
	}

	// The battery must actually pass on the repo it was drafted from. This is
	// the whole promise: one command, a working policy.
	battery := joinVerifyBattery(pol.verify)
	cmd := exec.Command("sh", "-c", battery)
	cmd.Dir = repo
	if out, rerr := cmd.CombinedOutput(); rerr != nil {
		t.Fatalf("drafted battery %q failed on the repo it was drafted from: %v\n%s", battery, rerr, out)
	}
}

// TestPolicyInitLeavesRepoOtherwiseUntouched: the only thing that appears is
// sigbound.policy. No ref moves, nothing is written under .git/sigbound/.
func TestPolicyInitLeavesRepoOtherwiseUntouched(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{"go.mod": "module x\n", ".github/workflows/ci.yml": ciWorkflow})
	if _, code, err, _ := policyInit(t, repo); err != nil || code != exitOK {
		t.Fatalf("code=%d err=%v", code, err)
	}
	st, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(st)); got != "?? "+policyFileName {
		t.Fatalf("git status = %q, want only the new policy file", got)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "sigbound")); !os.IsNotExist(err) {
		t.Fatalf(".git/sigbound was created: %v", err)
	}
}

// TestPolicyInitRefusesToClobber: an existing policy is the repo's real landing
// bar. It is never overwritten, the suggestion is printed instead, and the exit
// code is non-zero.
func TestPolicyInitRefusesToClobber(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{
		"go.mod":                   "module x\n",
		".github/workflows/ci.yml": ciWorkflow,
		"CODEOWNERS":               "/auth @acme/sec\n/billing @acme/fin\n/ops @acme/ops\n",
		"auth/a.go":                "package auth\n",
		"billing/b.go":             "package billing\n",
		"ops/c.go":                 "package ops\n",
	})
	// The existing ack-paths line is comma-separated: each glob in it is already
	// carried, and must not be suggested again.
	const existing = "# hand-written\nverify = go build ./...\nlanes = strict\nack-paths = auth/**, billing/**\n"
	path := filepath.Join(repo, policyFileName)
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code, err, after := policyInit(t, repo)
	if code != exitOperationalError || err == nil {
		t.Fatalf("want a non-zero exit and an error, got code=%d err=%v", code, err)
	}
	if after != existing {
		t.Fatalf("the existing policy was modified:\n%q", after)
	}
	if !strings.Contains(out, "+ verify = go test ./...") {
		t.Fatalf("suggestion did not offer the missing member:\n%s", out)
	}
	if strings.Contains(out, "+ verify = go build ./...") {
		t.Fatalf("suggestion offered a member the policy already has:\n%s", out)
	}
	if !strings.Contains(out, "+ ack-paths = ops/**") {
		t.Fatalf("suggestion did not offer the missing glob:\n%s", out)
	}
	for _, had := range []string{"+ ack-paths = auth/**", "+ ack-paths = billing/**"} {
		if strings.Contains(out, had) {
			t.Fatalf("suggestion re-offered a glob already inside a comma-separated value:\n%s", out)
		}
	}
}

// TestPolicyInitBlockScalar covers the join rule: a block of simple commands is
// ANDed into one member; a block that is a shell PROGRAM is refused whole and
// quoted, because joining it would produce a different program.
func TestPolicyInitBlockScalar(t *testing.T) {
	wf := func(body string) string {
		return "on: [push]\njobs:\n  t:\n    steps:\n      - run: |\n" + body
	}
	for _, tc := range []struct {
		name   string
		body   string
		verify []string
		refuse string
	}{
		{"simple", "          go build ./...\n          go test ./...\n", []string{"go build ./... && go test ./..."}, ""},
		{"heredoc", "          cat <<EOF\n          hi\n          EOF\n", nil, "heredoc"},
		{"continuation", "          go test \\\n            ./...\n", nil, "line continuation"},
		{"keyword", "          if [ -f x ]; then\n          exit 1\n          fi\n", nil, "shell keyword `if`"},
		{"trailing-op", "          go build ./... &&\n          go test ./...\n", nil, "continues onto the next"},
		// A trailing `#` comment would, once &&-joined, comment out every member
		// after it (finding CRITICAL-2): refuse the whole block, do not truncate it.
		{"trailing-comment", "          go build ./... # compile\n          go test ./...\n", nil, "trailing `#` comment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initPolicyRepo(t, map[string]string{".github/workflows/ci.yml": wf(tc.body)})
			_, _, err, draft := policyInit(t, repo)
			if err != nil {
				t.Fatal(err)
			}
			pol := mustParse(t, draft)
			if len(pol.verify) != len(tc.verify) {
				t.Fatalf("verify = %q, want %q\n%s", pol.verify, tc.verify, draft)
			}
			for i := range tc.verify {
				if pol.verify[i] != tc.verify[i] {
					t.Fatalf("verify[%d] = %q, want %q", i, pol.verify[i], tc.verify[i])
				}
			}
			if tc.refuse == "" {
				return
			}
			notes := unmappedLines(draft)
			if len(notes) != 1 {
				t.Fatalf("want exactly one unmapped entry, got %d:\n%s", len(notes), draft)
			}
			if !strings.Contains(notes[0], tc.refuse) {
				t.Fatalf("unmapped entry %q does not name %q", notes[0], tc.refuse)
			}
			// The refused block is preserved verbatim for a human to fold.
			for _, ln := range strings.Split(strings.TrimSpace(tc.body), "\n") {
				if !strings.Contains(draft, strings.TrimSpace(ln)) {
					t.Fatalf("refused block line %q is not quoted in the draft\n%s", ln, draft)
				}
			}
		})
	}
}

// TestPolicyInitRefusesExpressions: a ${{ }} substitution cannot be evaluated
// here, and an empty one can leave a command that exits 0 — a bar that gates
// nothing. Nothing is emitted, the raw text is carried in the note.
func TestPolicyInitRefusesExpressions(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{
		".github/workflows/ci.yml": "on: [push]\njobs:\n  t:\n    steps:\n      - run: go test -run ${{ matrix.go }} ./...\n",
	})
	_, _, err, draft := policyInit(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	if pol := mustParse(t, draft); len(pol.verify) != 0 {
		t.Fatalf("want no verify member, got %q\n%s", pol.verify, draft)
	}
	if !strings.Contains(draft, "${{ matrix.go }}") {
		t.Fatalf("the raw step text is not carried in the note\n%s", draft)
	}
}

// TestPolicyInitNonLandingTriggers: a workflow that does not gate a merge is
// not a landing bar. A schedule-only workflow and a tag-only release workflow
// both contribute nothing — copying a release job's steps into a landing bar
// would gate every merge on publishing.
func TestPolicyInitNonLandingTriggers(t *testing.T) {
	for _, tc := range []struct{ name, on, want string }{
		{"schedule", "on:\n  schedule:\n    - cron: '0 8 * * *'\n", "schedule"},
		{"tags-only", "on:\n  push:\n    tags:\n      - 'v*'\n", "push(not-a-merge-gate)"},
		// The sibling that was missing: a push filtered to a release line never
		// fires on a merge to the default branch either, so its steps are release
		// steps. Without this the workflow's `echo` became the landing bar while
		// the manifest fallback that would have drafted a real battery went
		// unreached.
		{"branches release-only", "on:\n  push:\n    branches:\n      - 'release/**'\n", "push(not-a-merge-gate)"},
		{"branches inline release-only", "on:\n  push:\n    branches: [release/**, deploy]\n", "push(not-a-merge-gate)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initPolicyRepo(t, map[string]string{
				".github/workflows/w.yml": tc.on + "jobs:\n  t:\n    steps:\n      - run: goreleaser release --clean\n",
			})
			_, _, err, draft := policyInit(t, repo)
			if err != nil {
				t.Fatal(err)
			}
			if pol := mustParse(t, draft); len(pol.verify) != 0 {
				t.Fatalf("want no verify member, got %q\n%s", pol.verify, draft)
			}
			notes := unmappedLines(draft)
			if len(notes) != 1 || !strings.Contains(notes[0], ".github/workflows/w.yml") || !strings.Contains(notes[0], tc.want) {
				t.Fatalf("want one entry naming the file and %q, got %q\n%s", tc.want, notes, draft)
			}
		})
	}
}

// TestPolicyInitToolchainCascade: the battery comes from exactly one source.
// With no workflows, go.mod resolves through verifyPresets verbatim; with a
// workflow that produced a battery, the toolchain contributes nothing but says
// what it saw.
func TestPolicyInitToolchainCascade(t *testing.T) {
	t.Run("no workflows", func(t *testing.T) {
		repo := initPolicyRepo(t, map[string]string{"go.mod": "module x\n"})
		_, _, err, draft := policyInit(t, repo)
		if err != nil {
			t.Fatal(err)
		}
		pol := mustParse(t, draft)
		if len(pol.verify) != 1 || pol.verify[0] != verifyPresets["go"] {
			t.Fatalf("verify = %q, want exactly [%q]\n%s", pol.verify, verifyPresets["go"], draft)
		}
	})
	t.Run("workflows win", func(t *testing.T) {
		repo := initPolicyRepo(t, map[string]string{"go.mod": "module x\n", ".github/workflows/ci.yml": ciWorkflow})
		_, _, err, draft := policyInit(t, repo)
		if err != nil {
			t.Fatal(err)
		}
		pol := mustParse(t, draft)
		for _, v := range pol.verify {
			if v == verifyPresets["go"] {
				t.Fatalf("the toolchain duplicated the workflow battery\n%s", draft)
			}
		}
		if !strings.Contains(draft, "# unmapped: toolchain: go.mod detects the go toolchain") {
			t.Fatalf("the toolchain did not say what it detected\n%s", draft)
		}
	})
	t.Run("node without a test script", func(t *testing.T) {
		repo := initPolicyRepo(t, map[string]string{"package.json": `{"name":"x","scripts":{"build":"tsc"}}`})
		_, _, err, draft := policyInit(t, repo)
		if err != nil {
			t.Fatal(err)
		}
		if pol := mustParse(t, draft); len(pol.verify) != 0 {
			t.Fatalf("`npm test` was drafted for a package.json with no scripts.test: %q", pol.verify)
		}
	})
}

// TestPolicyInitMakefile: with no workflows, a Makefile's conventional targets
// become the battery; an aggregate target is preferred alone.
func TestPolicyInitMakefile(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not on PATH; the drafted lines would be commented out")
	}
	for _, tc := range []struct {
		name, mk string
		want     []string
	}{
		{"conventional", "VERSION := 1\n\nbuild:\n\tgo build ./...\n\nlint:\n\tgo vet ./...\n\ntest:\n\tgo test ./...\n", []string{"make build", "make lint", "make test"}},
		{"aggregate", "check: build test\n\nbuild:\n\ttrue\n\ntest:\n\ttrue\n", []string{"make check"}},
		{"pattern rules only", "%.o: %.c\n\tcc -c $<\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initPolicyRepo(t, map[string]string{"Makefile": tc.mk})
			_, _, err, draft := policyInit(t, repo)
			if err != nil {
				t.Fatal(err)
			}
			pol := mustParse(t, draft)
			if strings.Join(pol.verify, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("verify = %q, want %q\n%s", pol.verify, tc.want, draft)
			}
		})
	}
}

// TestPolicyInitCodeowners is the per-row translation check: each emitted glob
// is evaluated with globMatch against the fixture tree rather than compared as
// a string, and owners never reach the right-hand side of a key.
func TestPolicyInitCodeowners(t *testing.T) {
	tree := []string{"a.js", "src/b.js", "docs/x.md", "apps/web/main.go", "lib/one.go"}
	for _, tc := range []struct {
		pattern string
		want    []string
		refuse  string
	}{
		{"*.js", []string{"**/*.js"}, ""},
		{"/docs/", []string{"docs/**"}, ""},
		{"docs/", []string{"docs/**"}, ""},
		{"/apps/web", []string{"apps/web/**"}, ""},
		// A LEADING slash anchors just as a middle one does: `/lib` is the root's
		// lib, not a lib at any depth. Testing the stripped form would emit
		// `**/lib`, a different set.
		{"/lib", []string{"lib/**"}, ""},
		// A no-separator pattern naming a DIRECTORY present in the tree (`lib`, with
		// lib/one.go) must emit the subtree glob `**/lib/**`, not `**/lib` — the
		// latter matches only a FILE literally named lib and would make the ack-path
		// never park. This is finding HIGH-3.
		{"lib", []string{"**/lib/**"}, ""},
		{"lib/one.go", []string{"lib/one.go"}, ""},
		{"apps/mobile", []string{"apps/mobile", "apps/mobile/**"}, ""},
		{"**/vendor", []string{"**/vendor", "**/vendor/**"}, ""},
		{"*", []string{"**"}, ""},
		{"a,b", nil, "comma-split"},
		{"!secret", nil, "negation"},
		{"src/[ab].js", nil, "character class"},
	} {
		got, refuse := codeownersGlobs(tc.pattern, tree)
		if tc.refuse != "" {
			if refuse == "" || !strings.Contains(refuse, tc.refuse) {
				t.Fatalf("%q: refusal = %q, want one naming %q", tc.pattern, refuse, tc.refuse)
			}
			if got != nil {
				t.Fatalf("%q: refused but still emitted %q", tc.pattern, got)
			}
			continue
		}
		if refuse != "" {
			t.Fatalf("%q: unexpected refusal %q", tc.pattern, refuse)
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Fatalf("%q -> %q, want %q", tc.pattern, got, tc.want)
		}
	}
	// The glob dialect actually matches what the pattern meant.
	for _, c := range []struct {
		glob, path string
		want       bool
	}{
		{"**/*.js", "a.js", true}, {"**/*.js", "src/b.js", true}, {"**/*.js", "docs/x.md", false},
		{"docs/**", "docs/x.md", true}, {"docs/**", "a.js", false},
		{"apps/web/**", "apps/web/main.go", true}, {"apps/web/**", "lib/one.go", false},
		// The no-slash directory glob (finding HIGH-3) matches the directory's
		// contents at any depth; the bare-name glob it replaced would not.
		{"**/lib/**", "lib/one.go", true}, {"**/lib", "lib/one.go", false},
		{"**/lib/**", "a.js", false},
	} {
		if got := globMatch(c.glob, c.path); got != c.want {
			t.Fatalf("globMatch(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}

	repo := initPolicyRepo(t, map[string]string{
		"apps/web/main.go":    "package main\n",
		"a.js":                "//\n",
		".github/CODEOWNERS":  "# owners\n*.js @acme/web @dana\n/apps/web @acme/mobile\nbad,pattern @acme/x\n",
		"docs/CODEOWNERS":     "ignored @acme/y\n",
		".github/workflows/x": "",
	})
	_, _, err, draft := policyInit(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	pol := mustParse(t, draft)
	if strings.Join(pol.ackPaths, "|") != "**/*.js|apps/web/**" {
		t.Fatalf("ack-paths = %q\n%s", pol.ackPaths, draft)
	}
	for _, glob := range pol.ackPaths {
		for _, owner := range []string{"@acme", "@dana"} {
			if strings.Contains(glob, owner) {
				t.Fatalf("an owner string reached an emitted value: %q", glob)
			}
		}
	}
	for _, want := range []string{
		`.github/CODEOWNERS:2 (@acme/web @dana) — matches 1 file(s)`,
		`.github/CODEOWNERS:3 (@acme/mobile) — matches 1 file(s)`,
		`# unmapped: codeowners: .github/CODEOWNERS:4 pattern "bad,pattern"`,
		`# unmapped: codeowners: docs/CODEOWNERS is also present`,
		`# unmapped: codeowners: owners are dropped`,
	} {
		if !strings.Contains(draft, want) {
			t.Fatalf("draft is missing %q\n%s", want, draft)
		}
	}
}

// TestPolicyInitUnreadableRepo: a directory that is not a git repo still yields
// a usable starting file — a commented template plus a plain statement of why —
// never a broken policy and never nothing at all.
func TestPolicyInitUnreadableRepo(t *testing.T) {
	dir := t.TempDir()
	out, code, err, draft := policyInit(t, dir)
	if err != nil || code != exitOK {
		t.Fatalf("code=%d err=%v\n%s", code, err, out)
	}
	pol := mustParse(t, draft)
	if len(pol.verify) != 0 || len(pol.ackPaths) != 0 {
		t.Fatalf("a repo that could not be read produced keys: %+v", pol)
	}
	if !strings.Contains(draft, "# verify = <command>") {
		t.Fatalf("no commented template in the draft\n%s", draft)
	}
	if !strings.Contains(draft, "# unmapped: repo: cannot resolve HEAD") {
		t.Fatalf("the draft does not say why it read nothing\n%s", draft)
	}
	if !strings.Contains(out, "NO verify command was inferred") {
		t.Fatalf("stdout does not say plainly that nothing was inferred:\n%s", out)
	}
}

// TestPolicyInitDeterministic: same tree, same bytes. No timestamps, fixed
// ordering — so re-running it is a diff a reviewer can read.
func TestPolicyInitDeterministic(t *testing.T) {
	files := map[string]string{
		"go.mod":                   "module x\n",
		".github/workflows/ci.yml": ciWorkflow,
		".github/workflows/b.yml":  "on: [pull_request]\njobs:\n  a:\n    steps:\n      - run: echo a\n  b:\n    steps:\n      - run: echo b\n",
		"CODEOWNERS":               "*.go @acme/core\nsrc/ @acme/src\n",
	}
	repo := initPolicyRepo(t, files)
	_, _, err, first := policyInit(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, policyFileName)); err != nil {
		t.Fatal(err)
	}
	_, _, err, second := policyInit(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("two invocations differ:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestPolicyInitJobLevelRefusals: constructs that change what a step MEANS
// refuse the job, so the drafted battery never contains a command that would
// actually run somewhere else.
func TestPolicyInitJobLevelRefusals(t *testing.T) {
	for _, tc := range []struct{ name, job, want string }{
		{"services", "    services:\n      db:\n        image: postgres\n", "environment this run does not provide"},
		{"container", "    container: golang:1.22\n", "environment this run does not provide"},
		{"job env", "    env:\n        CGO_ENABLED: '0'\n", "job-level `env:`"},
		{"defaults", "    defaults:\n      run:\n        working-directory: ./sub\n", "`defaults:`"},
		// The step-scope twins, mirrored down. A conditional JOB is strictly
		// broader than a conditional step -- its condition decides whether any of
		// its steps run at all -- and a job whose failure does not fail the
		// workflow is not a gate. Both were unguarded at this scope while being
		// refused one level down.
		{"job if", "    if: github.event_name == 'schedule'\n", "conditional (`if:`)"},
		{"job continue-on-error", "    continue-on-error: true\n", "does not fail CI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initPolicyRepo(t, map[string]string{
				".github/workflows/ci.yml": "on: [push]\njobs:\n  t:\n" + tc.job + "    steps:\n      - run: go test ./...\n",
			})
			_, _, err, draft := policyInit(t, repo)
			if err != nil {
				t.Fatal(err)
			}
			if pol := mustParse(t, draft); len(pol.verify) != 0 {
				t.Fatalf("want no verify member, got %q\n%s", pol.verify, draft)
			}
			if !strings.Contains(draft, tc.want) {
				t.Fatalf("no note naming %q\n%s", tc.want, draft)
			}
		})
	}
}

// TestPolicyInitStepLevelRefusals: a step that may not run, does not gate, or
// runs elsewhere is skipped — the rest of the job still contributes. Exercised
// over BOTH `run:` forms (single-line AND `run: |` block) for every disqualifier,
// because the block form is where a refusal set by an EARLIER key in the step was
// being cleared (finding CRITICAL-1): reverting that fix drafts the block step as
// a live member here, so the "exactly one member" assertion fails on the /block
// subtests. That is this test's job as the CRITICAL-1 negative control.
func TestPolicyInitStepLevelRefusals(t *testing.T) {
	disquals := []struct{ name, keyLines, note string }{
		{"if", "        if: github.ref == 'refs/heads/main'", "conditional (`if:`)"},
		{"continue-on-error", "        continue-on-error: true", "continue-on-error: true"},
		{"working-directory", "        working-directory: ./sub", "working-directory:"},
		{"env", "        env:\n          FOO: bar", "the step sets `env:`"},
		{"shell", "        shell: pwsh", "shell: pwsh"},
	}
	runForms := []struct{ name, run string }{
		{"single-line", "        run: go build ./...\n"},
		{"block", "        run: |\n          go build ./...\n"},
	}
	for _, dq := range disquals {
		for _, rf := range runForms {
			t.Run(dq.name+"/"+rf.name, func(t *testing.T) {
				// The disqualifying key sits BEFORE `run:`, the shape real workflows
				// use and the one that triggered the bug; a clean step follows it.
				wf := "on: [push]\njobs:\n  t:\n    steps:\n" +
					"      - name: disq\n" + dq.keyLines + "\n" + rf.run +
					"      - run: go test ./...\n"
				repo := initPolicyRepo(t, map[string]string{".github/workflows/ci.yml": wf})
				_, _, err, draft := policyInit(t, repo)
				if err != nil {
					t.Fatal(err)
				}
				pol := mustParse(t, draft)
				if len(pol.verify) != 1 || pol.verify[0] != "go test ./..." {
					t.Fatalf("verify = %q, want exactly [\"go test ./...\"] — the disqualified %s step must not draft a live member\n%s", pol.verify, dq.name, draft)
				}
				if !strings.Contains(draft, dq.note) {
					t.Fatalf("no note naming %q\n%s", dq.note, draft)
				}
			})
		}
	}
}

// TestPolicyInitRunsOnRefusal: a job pinned to a non-linux runner is refused
// whole (finding MEDIUM-8) — its steps would run somewhere verify does not, and
// a windows job duplicating a linux job's command must not draft that member
// twice. A workflow of only non-linux jobs drafts zero live members, with notes.
func TestPolicyInitRunsOnRefusal(t *testing.T) {
	t.Run("windows and macos only", func(t *testing.T) {
		repo := initPolicyRepo(t, map[string]string{
			".github/workflows/ci.yml": `on: [push]
jobs:
  win:
    runs-on: windows-latest
    steps:
      - run: go test ./...
  mac:
    runs-on: macos-14
    steps:
      - run: go test ./...
`,
		})
		_, _, err, draft := policyInit(t, repo)
		if err != nil {
			t.Fatal(err)
		}
		if pol := mustParse(t, draft); len(pol.verify) != 0 {
			t.Fatalf("non-linux jobs drafted live members: %q\n%s", pol.verify, draft)
		}
		for _, want := range []string{"windows-latest", "macos-14"} {
			if !strings.Contains(draft, want) {
				t.Fatalf("no note naming the refused runner %q\n%s", want, draft)
			}
		}
	})
	t.Run("linux job still drafts, windows dup dropped", func(t *testing.T) {
		repo := initPolicyRepo(t, map[string]string{
			".github/workflows/ci.yml": `on: [push]
jobs:
  linux:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
  win:
    runs-on: windows-latest
    steps:
      - run: go test ./...
`,
		})
		_, _, err, draft := policyInit(t, repo)
		if err != nil {
			t.Fatal(err)
		}
		pol := mustParse(t, draft)
		if len(pol.verify) != 1 || pol.verify[0] != "go test ./..." {
			t.Fatalf("verify = %q, want exactly [\"go test ./...\"]\n%s", pol.verify, draft)
		}
	})
	t.Run("array with linux is allowed", func(t *testing.T) {
		repo := initPolicyRepo(t, map[string]string{
			".github/workflows/ci.yml": "on: [push]\njobs:\n  t:\n    runs-on: [self-hosted, linux, x64]\n    steps:\n      - run: go test ./...\n",
		})
		_, _, err, draft := policyInit(t, repo)
		if err != nil {
			t.Fatal(err)
		}
		if pol := mustParse(t, draft); len(pol.verify) != 1 {
			t.Fatalf("a self-hosted linux runner was refused: %q\n%s", pol.verify, draft)
		}
	})
}

// TestPolicyInitWorkflowLevelRefusals: `defaults:` and `env:` disqualify a
// workflow at BOTH scopes. GitHub applies the COLUMN-0 forms to every step in
// every job, so a top-level `defaults.run.working-directory` relocates every
// command — the canonical monorepo idiom — and drafting a step's text under it
// yields a bar that passes at the repository root while real CI fails in the
// subdirectory. Same failure as the job-level twin, one scope up.
func TestPolicyInitWorkflowLevelRefusals(t *testing.T) {
	for _, tc := range []struct{ name, head, want string }{
		{"workflow defaults working-directory",
			"on: [push]\ndefaults:\n  run:\n    working-directory: services/api\n",
			"workflow-level `defaults:`"},
		{"workflow defaults shell",
			"on: [push]\ndefaults:\n  run:\n    shell: pwsh\n",
			"workflow-level `defaults:`"},
		{"workflow env",
			"on: [push]\nenv:\n  CGO_ENABLED: '0'\n",
			"workflow-level `env:`"},
		// The job-level twins keep working — this guard is additive, not a move.
		{"job defaults",
			"on: [push]\n",
			"`defaults:`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := "jobs:\n  api:\n    runs-on: ubuntu-latest\n    steps:\n      - run: |\n          go vet ./...\n          go test ./...\n"
			if tc.name == "job defaults" {
				job = "jobs:\n  api:\n    runs-on: ubuntu-latest\n    defaults:\n      run:\n        working-directory: ./sub\n    steps:\n      - run: go test ./...\n"
			}
			repo := initPolicyRepo(t, map[string]string{".github/workflows/ci.yml": tc.head + job})
			_, _, err, draft := policyInit(t, repo)
			if err != nil {
				t.Fatal(err)
			}
			if pol := mustParse(t, draft); len(pol.verify) != 0 {
				t.Fatalf("a relocated/re-environed workflow drafted live member(s): %q\n%s", pol.verify, draft)
			}
			if !strings.Contains(draft, tc.want) {
				t.Fatalf("no note naming %q\n%s", tc.want, draft)
			}
		})
	}
}

// TestPolicyInitSymlinkedPolicyNotRead: a `sigbound.policy` symlink is never
// READ. O_EXCL closed the write half of the symlink hole; this is the read half —
// printPolicySuggestion surfaces parseConfigFile's error, which quotes the
// offending line verbatim, so reading a symlink to ~/.netrc or .git-credentials
// would print a live credential to stdout.
func TestPolicyInitSymlinkedPolicyNotRead(t *testing.T) {
	const secret = "machine github.com login alice password ghp_TESTONLY_NOT_A_REAL_TOKEN"
	repo := initPolicyRepo(t, map[string]string{"go.mod": "module x\n"})
	target := filepath.Join(t.TempDir(), "netrc")
	if err := os.WriteFile(target, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(repo, policyFileName)); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	var buf bytes.Buffer
	code, err := runPolicyInit(&buf, []string{"-repo", repo})
	if code == exitOK || err == nil {
		t.Fatalf("a symlinked policy did not refuse: code=%d err=%v", code, err)
	}
	// Neither stdout nor the returned error may carry any of the file's content.
	for what, s := range map[string]string{"stdout": buf.String(), "error": err.Error()} {
		if strings.Contains(s, "ghp_TESTONLY") || strings.Contains(s, secret) {
			t.Fatalf("the symlink target's content leaked into %s:\n%s", what, s)
		}
	}
	if !strings.Contains(buf.String(), "is a symlink") {
		t.Fatalf("stdout does not say the file was not read:\n%s", buf.String())
	}
}

// TestPolicyInitDraftHasNoControlBytes: the drafted file must stay a reviewable
// text diff. A NUL reaching it (quoted into a note from a refused command, or
// carried into an ack-paths value by a CODEOWNERS pattern) makes git report
// "Binary files differ" and "0 insertions(+), 0 deletions(-)" — unreadable to the
// human acking a parked sigbound.policy change under policy self-protection.
func TestPolicyInitDraftHasNoControlBytes(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{
		// Refused for the NUL, then QUOTED into the note verbatim.
		".github/workflows/ci.yml": "on: [push]\njobs:\n  t:\n    steps:\n      - run: echo \x00nul\n",
		// strings.Fields does not split on NUL, so this is ONE pattern that would
		// otherwise reach a live ack-paths value.
		"CODEOWNERS": "auth\x00x @acme/sec\n",
		"x.go":       "package x\n",
	})
	_, _, err, draft := policyInit(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	mustParse(t, draft)
	for i := 0; i < len(draft); i++ {
		if c := draft[i]; (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f {
			t.Fatalf("control byte %#x at offset %d of the drafted policy — git will call this file binary:\n%q", c, i, draft)
		}
	}
	// The refused input is still reported, just scrubbed rather than raw.
	if len(unmappedLines(draft)) == 0 {
		t.Fatalf("the control-byte input was dropped silently\n%s", draft)
	}
}

// TestPolicyInitBadRevExitsBeforeWriting: an explicitly-named -rev that does not
// resolve exits non-zero and writes NOTHING (finding MEDIUM-7), so the corrected
// retry is not blocked by a vacuous policy (there is no -force). Then a run
// against the same repo with a good rev succeeds.
func TestPolicyInitBadRevExitsBeforeWriting(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{"go.mod": "module x\n"})
	var buf bytes.Buffer
	code, err := runPolicyInit(&buf, []string{"-repo", repo, "-rev", "v9.9.9-typo"})
	if code == exitOK || err == nil {
		t.Fatalf("a bad -rev exited OK: code=%d err=%v", code, err)
	}
	if _, statErr := os.Stat(filepath.Join(repo, policyFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("a bad -rev wrote a policy: %v", statErr)
	}
	// The corrected invocation now works — nothing was left behind to block it.
	if _, _, err, draft := policyInit(t, repo); err != nil || draft == "" {
		t.Fatalf("the good retry failed: err=%v\n%s", err, draft)
	}
}

// TestPolicyInitNoClobberSymlink: a `sigbound.policy` that is a DANGLING symlink
// pointing outside the repo is never followed to create that file (finding
// MEDIUM-5, O_EXCL). This is the distinguishing case: the old ReadFile-then-
// WriteFile refused a symlink to an existing file (ReadFile succeeded) but would
// FOLLOW a dangling one (ReadFile fails ErrNotExist, WriteFile then creates the
// target outside). O_CREATE|O_EXCL fails EEXIST on any symlink, so nothing is
// written outside and the command exits non-zero.
func TestPolicyInitNoClobberSymlink(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{"go.mod": "module x\n"})
	outside := filepath.Join(t.TempDir(), "OUTSIDE") // deliberately does not exist
	link := filepath.Join(repo, policyFileName)
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	var buf bytes.Buffer
	code, err := runPolicyInit(&buf, []string{"-repo", repo})
	if code == exitOK || err == nil {
		t.Fatalf("O_EXCL did not refuse a symlinked policy path: code=%d err=%v", code, err)
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("the write followed the symlink and created a file outside the repo: %v", statErr)
	}
}

// TestScanWorkflowControlByteRefused: a control byte (here NUL) reaching a
// command is refused and noted, never emitted as a verify value (finding LOW —
// workflowscan.go guarded only \r\n before).
func TestScanWorkflowControlByteRefused(t *testing.T) {
	data := []byte("on: [push]\njobs:\n  t:\n    steps:\n      - run: go test ./...\x00rm -rf /\n")
	sc := scanWorkflow(".github/workflows/ci.yml", data)
	if len(sc.Commands) != 0 {
		t.Fatalf("a command with a NUL was emitted: %q", sc.Commands)
	}
	var noted bool
	for _, n := range sc.Notes {
		if strings.Contains(n.Detail, "control character") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the NUL byte was not noted: %+v", sc.Notes)
	}
}

// TestPolicyInitOversizeBlobSkipped: a source file larger than wfMaxBytes is
// skipped BEFORE it is read, and the skip is a note (finding MEDIUM-6). A
// Makefile is used because — unlike scanWorkflow — the Makefile reader has no
// internal size cap of its own, so this observes the PRE-READ filter specifically:
// remove that filter and the oversize Makefile is parsed and `make build` drafted.
func TestPolicyInitOversizeBlobSkipped(t *testing.T) {
	big := "build:\n\tgo build ./...\n" + strings.Repeat("# pad\n", (wfMaxBytes/6)+1)
	if len(big) <= wfMaxBytes {
		t.Fatalf("fixture not over cap: %d <= %d", len(big), wfMaxBytes)
	}
	repo := initPolicyRepo(t, map[string]string{"Makefile": big})
	_, _, err, draft := policyInit(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	mustParse(t, draft)
	if strings.Contains(draft, "make build") {
		t.Fatalf("the oversize Makefile was read and drafted\n%s", draft)
	}
	if !strings.Contains(draft, "not read") {
		t.Fatalf("no note about the skipped oversize file\n%s", draft)
	}
}

// TestPolicyInitProbeRunsNoRepoCode: the python probe must not execute a
// repo-resident pytest.py (finding HIGH-4). A malicious pytest.py that drops a
// sentinel proves it: after `sig policy init`, the sentinel must not exist.
func TestPolicyInitProbeRunsNoRepoCode(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{
		"requirements.txt": "pytest\n",
		// If `python -m pytest` were used with cwd on sys.path, importing pytest
		// would run this and create the sentinel.
		"pytest.py": "import os\nopen(os.path.join(os.path.dirname(__file__), 'PWNED'), 'w').close()\n",
	})
	if _, _, err, _ := policyInit(t, repo); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(repo, "PWNED")); !os.IsNotExist(statErr) {
		t.Fatalf("the probe executed repo-resident pytest.py: sentinel exists (%v)", statErr)
	}
}

// TestPolicyInitGoTestTimeoutNote: a drafted `go test` member with no -timeout
// draws one advisory note (finding LOW) — the command itself is left verbatim.
func TestPolicyInitGoTestTimeoutNote(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{"go.mod": "module x\n"})
	_, _, err, draft := policyInit(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	pol := mustParse(t, draft)
	if len(pol.verify) != 1 || pol.verify[0] != verifyPresets["go"] {
		t.Fatalf("verify = %q, want the go preset verbatim\n%s", pol.verify, draft)
	}
	if !strings.Contains(draft, "no `-timeout`") {
		t.Fatalf("no -timeout advisory for a drafted go test member\n%s", draft)
	}
}

// TestPolicyInitSelfCheckIsEnforced: the command's own output always parses.
// Exercised here over shapes that put shell metacharacters, '=' and '#' into a
// verify value, which is where an unescaped draft would break.
func TestPolicyInitSelfCheckIsEnforced(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{
		".github/workflows/ci.yml": "on: [push]\njobs:\n  t:\n    steps:\n      - run: FOO=bar go test ./... # go\n      - run: sh -c 'echo a && echo b' | cat\n",
	})
	_, _, err, draft := policyInit(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	pol := mustParse(t, draft)
	if len(pol.verify) != 2 || pol.verify[0] != "FOO=bar go test ./... # go" {
		t.Fatalf("verify = %q\n%s", pol.verify, draft)
	}
}

// TestPolicyDoctorLine: doctor gains one informational line naming this command
// when the repo has no policy, and the counts when it has one — and never
// changes doctor's exit code either way.
func TestPolicyDoctorLine(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{"go.mod": "module x\n"})
	var buf bytes.Buffer
	code, err := runDoctor(&buf, []string{"-repo", repo})
	if err != nil || code != exitOK {
		t.Fatalf("doctor: code=%d err=%v\n%s", code, err, buf.String())
	}
	if !strings.Contains(buf.String(), "no "+policyFileName+" at HEAD (run `sig policy init`") {
		t.Fatalf("doctor did not point at the command:\n%s", buf.String())
	}

	if err := os.WriteFile(filepath.Join(repo, policyFileName), []byte("verify = go build ./...\nack-paths = a/**, b/**\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.New(repo).CommitAll(context.Background(), "policy"); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	code, err = runDoctor(&buf, []string{"-repo", repo})
	if err != nil || code != exitOK {
		t.Fatalf("doctor: code=%d err=%v\n%s", code, err, buf.String())
	}
	if !strings.Contains(buf.String(), "1 verify member(s), 2 ack-paths glob(s)") {
		t.Fatalf("doctor did not report the policy's counts:\n%s", buf.String())
	}

	// An unparseable policy is information, not a doctor failure: the three real
	// checks still decide the exit code.
	if err := os.WriteFile(filepath.Join(repo, policyFileName), []byte("nonsense = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.New(repo).CommitAll(context.Background(), "bad policy"); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if code, err := runDoctor(&buf, []string{"-repo", repo}); err != nil || code != exitOK {
		t.Fatalf("an unreadable policy changed doctor's verdict: code=%d err=%v\n%s", code, err, buf.String())
	}
	if !strings.Contains(buf.String(), "is unreadable") {
		t.Fatalf("doctor did not report the unreadable policy:\n%s", buf.String())
	}
}

// TestPolicySubcommandDispatch: `sig policy` is two-token, like `sig intent`.
func TestPolicySubcommandDispatch(t *testing.T) {
	var buf bytes.Buffer
	if code, err := runPolicy(&buf, nil); code != exitOperationalError || err == nil {
		t.Fatalf("no subcommand: code=%d err=%v", code, err)
	}
	buf.Reset()
	if code, err := runPolicy(&buf, []string{"nope"}); code != exitOperationalError || err == nil {
		t.Fatalf("unknown subcommand: code=%d err=%v", code, err)
	}
	buf.Reset()
	if code, err := runPolicy(&buf, []string{"-h"}); code != exitOK || err != nil {
		t.Fatalf("-h: code=%d err=%v", code, err)
	}
	if !strings.Contains(buf.String(), "sig policy init") {
		t.Fatalf("usage does not name the subcommand:\n%s", buf.String())
	}
}

// assertRenderIsText fails if a rendered draft carries a control byte. The file
// exists to be a diff a human reads — most consequentially the human acking a
// parked change to sigbound.policy itself — and one NUL anywhere makes git call
// the whole file binary and print no diff at all. Asserted over arbitrary input
// by both fuzz targets, since a control byte can arrive from a workflow (quoted
// into a note) or from a CODEOWNERS pattern (carried into an ack-paths value).
func assertRenderIsText(t *testing.T, draft string) {
	t.Helper()
	for i := 0; i < len(draft); i++ {
		if c := draft[i]; (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f {
			t.Fatalf("control byte %#x at offset %d — git would call this file binary:\n%q", c, i, draft)
		}
	}
}

// unmappedLines returns the draft's `# unmapped:` entries.
func unmappedLines(draft string) []string {
	var out []string
	for _, ln := range strings.Split(draft, "\n") {
		if strings.HasPrefix(ln, "# unmapped: ") {
			out = append(out, ln)
		}
	}
	return out
}

// FuzzScanWorkflow asserts the two properties acceptance asks of the scanner
// over arbitrary bytes: it never panics, and any draft assembled from what it
// produced parses through parsePolicy — the invariant that keeps this command
// from ever writing a policy that fails every subsequent run.
func FuzzScanWorkflow(f *testing.F) {
	f.Add([]byte(ciWorkflow))
	f.Add([]byte("on: [push]\njobs:\n  t:\n    steps:\n      - run: |\n          a\n          b\n"))
	f.Add([]byte("on:\n  push:\n    tags: ['v*']\njobs:\n  r:\n    steps:\n      - run: x\n"))
	f.Add([]byte("on: push\njobs:\n\tt:\n\t\tsteps:\n"))
	f.Add([]byte("run: \nverify = x\n"))
	f.Add([]byte("on: [push]\njobs:\n  t:\n    steps:\n      - run: a\nb\n = c\n"))
	// Regression: a lone CR mid-value is not a line terminator to textLines but
	// is to plenty of other readers, and it used to reach a `verify =` value.
	f.Add([]byte("on: push\njobs:\n 0:\n  steps: \n   - run: 0\r0"))
	// Regression: DEL (0x7f) is a control byte the C0-only guard missed, so it
	// reached a live `verify =` value. Found by this fuzzer once the rendered
	// draft was asserted to be text (assertRenderIsText).
	f.Add([]byte("on: push\njobs:\n 0:\n  steps:\n   - run: \x7f"))
	f.Fuzz(func(t *testing.T, data []byte) {
		sc := scanWorkflow(".github/workflows/f.yml", data)
		d := policyDraft{repo: "r", rev: "deadbeef"}
		for _, c := range sc.Commands {
			if c.Cmd == "" || strings.ContainsAny(c.Cmd, "\n\r") {
				t.Fatalf("emitted command %q is empty or multi-line", c.Cmd)
			}
			if strings.Contains(c.Cmd, "${{") {
				t.Fatalf("emitted command %q carries an unevaluated expression", c.Cmd)
			}
			d.lines = append(d.lines, draftLine{comment: "f", key: "verify", value: c.Cmd, live: true})
		}
		for _, n := range sc.Notes {
			d.note("workflows", n.Detail, n.Quote...)
		}
		if _, err := parsePolicy([]byte(d.render())); err != nil {
			t.Fatalf("drafted policy does not parse: %v\n%s", err, d.render())
		}
		assertRenderIsText(t, d.render())
	})
}

// FuzzCodeownersDraft is the same pair of properties for the CODEOWNERS
// translation: no panic, and whatever it emits still parses (an ack-paths value
// is comma-split, so a stray comma in a glob would silently become two).
func FuzzCodeownersDraft(f *testing.F) {
	f.Add([]byte("*.js @a\n/docs/ @b\n"))
	f.Add([]byte("a,b @a\n!x @b\n[ab] @c\n"))
	f.Add([]byte("# comment\n\n   \n@owner-only\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		tree := []string{"a.js", "docs/x.md", "apps/web/main.go"}
		d := policyDraft{repo: "r", rev: "deadbeef"}
		d.addCodeowners(map[string]bool{".github/CODEOWNERS": true}, map[string]string{".github/CODEOWNERS": string(data)}, tree)
		pol, err := parsePolicy([]byte(d.render()))
		if err != nil {
			t.Fatalf("drafted policy does not parse: %v\n%s", err, d.render())
		}
		assertRenderIsText(t, d.render())
		for _, glob := range pol.ackPaths {
			if glob == "" || strings.ContainsAny(glob, ",\n\r\x00") {
				t.Fatalf("emitted glob %q is empty or would be re-split", glob)
			}
			globMatch(glob, "apps/web/main.go") // must not panic on any emitted glob
		}
	})
}

// TestPolicyInitScheduleOnlyJobIsNotABar is the vacuous-bar case the job-scope
// `if:` guard exists for, in the shape a real repo has it: a workflow that fires
// on push AND on a schedule, whose only jobs are a schedule-gated smoke job and
// an advisory job that is allowed to fail. The file passes the trigger check
// because `push` is there, but its real gate content is ZERO -- neither job
// blocks a merge. Drafting either one produces a bar that reports green
// unconditionally, which is the one output this command must never produce.
func TestPolicyInitScheduleOnlyJobIsNotABar(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{
		".github/workflows/ci.yml": "on:\n  push:\n  schedule:\n    - cron: '0 3 * * *'\n" +
			"jobs:\n" +
			"  nightly-smoke:\n    if: github.event_name == 'schedule'\n    steps:\n      - run: echo \"nightly smoke placeholder\"\n" +
			"  flaky-e2e:\n    continue-on-error: true\n    steps:\n      - run: ./e2e.sh || true\n",
	})
	_, _, err, draft := policyInit(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	if pol := mustParse(t, draft); len(pol.verify) != 0 {
		t.Fatalf("a schedule-only job and an advisory job drafted %q as the landing bar; neither gates a merge\n%s", pol.verify, draft)
	}
	for _, want := range []string{"nightly-smoke", "flaky-e2e"} {
		if !strings.Contains(draft, want) {
			t.Fatalf("no note naming the skipped job %q — the draft must say what it did not use\n%s", want, draft)
		}
	}
}

// TestControlBytePredicateIsOne pins the property whose absence caused two
// escapes: every consumer of "is this byte allowed in the drafted file" must
// answer identically. The scan that refuses a command, the CODEOWNERS pattern
// check and the comment scrubber previously each carried their own copy -- one
// omitted DEL (so `run: \x7f` reached a live verify value) and one omitted the
// tab exemption (harmless only because no CODEOWNERS pattern can hold a tab).
//
// This asserts agreement across all three, so a future edit to one of them fails
// here instead of shipping a byte that only one consumer lets through.
func TestControlBytePredicateIsOne(t *testing.T) {
	// The agreement loop below compares every consumer AGAINST isControlByte, so
	// it cannot notice the predicate itself changing -- both sides move together.
	// Pin the content first: what it must and must not reject, independent of
	// what it currently says. Without this, dropping the tab exemption passes.
	for _, c := range []byte{0x00, 0x01, 0x08, '\n', '\r', 0x1f, 0x7f} {
		if !isControlByte(c) {
			t.Fatalf("byte %#02x must be rejected: it corrupts the drafted line or reaches exec", c)
		}
	}
	for _, c := range []byte{'\t', ' ', 'a', '~', 0x80, 0xff} {
		if isControlByte(c) {
			t.Fatalf("byte %#02x must be allowed: a tab is legitimate whitespace in a command, and high bytes are UTF-8 continuation", c)
		}
	}
	for b := 0; b < 0x100; b++ {
		c := byte(b)
		want := isControlByte(c)
		s := "a" + string(c) + "b"
		if got := hasControlByte(s); got != want {
			t.Fatalf("byte %#02x: hasControlByte=%v, isControlByte=%v", c, got, want)
		}
		// The scrubber rewrites exactly the bytes the predicate rejects.
		if got := scrubControl(s) != s; got != want {
			t.Fatalf("byte %#02x: scrubControl rewrote=%v, isControlByte=%v", c, got, want)
		}
		// The CODEOWNERS pattern check refuses exactly those bytes. Patterns are
		// whitespace-split before they reach it, so feed the byte in a pattern
		// shape and skip the ones splitKey/Fields would never deliver.
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		_, refuse := codeownersGlobs("x"+string(c)+"y", nil)
		if refused := strings.Contains(refuse, "control character"); refused != want {
			t.Fatalf("byte %#02x: codeownersGlobs refused=%v, isControlByte=%v (refuse=%q)", c, refused, want, refuse)
		}
	}
}

// TestPolicyInitOrdinaryPushStillDrafts is the over-refusal guard for the
// branches filter. A workflow restricted to the default branch is the single
// most common CI shape there is, and refusing it would make this command useless
// on the majority of real repos. It must still draft.
func TestPolicyInitOrdinaryPushStillDrafts(t *testing.T) {
	for _, on := range []string{
		"on: [push]\n",
		"on:\n  push:\n",
		"on:\n  push:\n    branches:\n      - main\n",
		"on:\n  push:\n    branches: [main, develop]\n",
		"on:\n  push:\n    branches:\n      - master\n",
		// A wildcard that could still expand to the default branch counts as
		// mergeable: refusing it would be a guess in the expensive direction.
		"on:\n  push:\n    branches:\n      - '*'\n",
		"on:\n  push:\n    branches:\n      - 'ma*'\n",
		// An exclusion list still fires on everything else, the default branch
		// included.
		"on:\n  push:\n    branches-ignore:\n      - 'release/**'\n",
		// Release-only push, but pull_request is unfiltered — the PR trigger is
		// the merge gate, so the file still counts.
		"on:\n  push:\n    branches: [release/**]\n  pull_request:\n",
	} {
		t.Run(strings.ReplaceAll(strings.TrimSpace(on), "\n", "|"), func(t *testing.T) {
			repo := initPolicyRepo(t, map[string]string{
				".github/workflows/ci.yml": on + "jobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n",
			})
			_, _, err, draft := policyInit(t, repo)
			if err != nil {
				t.Fatal(err)
			}
			pol := mustParse(t, draft)
			if len(pol.verify) != 1 || pol.verify[0] != "go test ./..." {
				t.Fatalf("an ordinary merge-gating workflow drafted %q, want [go test ./...]\n%s", pol.verify, draft)
			}
		})
	}
}

// TestPolicyInitDeploymentJobIsNotABar: a job targeting a deployment
// environment is a deploy step, not a merge gate.
func TestPolicyInitDeploymentJobIsNotABar(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{
		".github/workflows/ci.yml": "on: [push]\njobs:\n  ship:\n    environment: production\n    steps:\n      - run: echo \"deploying\"\n",
	})
	_, _, err, draft := policyInit(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	if pol := mustParse(t, draft); len(pol.verify) != 0 {
		t.Fatalf("a deployment job drafted %q as the landing bar\n%s", pol.verify, draft)
	}
	if !strings.Contains(draft, "production") {
		t.Fatalf("no note naming the environment\n%s", draft)
	}
}
