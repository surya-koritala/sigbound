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
		{"tags-only", "on:\n  push:\n    tags:\n      - 'v*'\n", "push(tags-only)"},
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
		{"lib", []string{"**/lib"}, ""},
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
// runs elsewhere is skipped — the rest of the job still contributes.
func TestPolicyInitStepLevelRefusals(t *testing.T) {
	repo := initPolicyRepo(t, map[string]string{
		".github/workflows/ci.yml": `on: [push]
jobs:
  t:
    steps:
      - name: conditional
        if: github.ref == 'refs/heads/main'
        run: go test -tags main ./...
      - name: soft
        continue-on-error: true
        run: go vet ./...
      - name: elsewhere
        working-directory: ./sub
        run: go build ./...
      - name: other shell
        shell: pwsh
        run: Get-ChildItem
      - name: real
        run: go test ./...
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
	for _, want := range []string{"conditional (`if:`)", "continue-on-error: true", "working-directory:", "shell: pwsh"} {
		if !strings.Contains(draft, want) {
			t.Fatalf("no note naming %q\n%s", want, draft)
		}
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
		for _, glob := range pol.ackPaths {
			if glob == "" || strings.ContainsAny(glob, ",\n\r") {
				t.Fatalf("emitted glob %q is empty or would be re-split", glob)
			}
			globMatch(glob, "apps/web/main.go") // must not panic on any emitted glob
		}
	})
}
