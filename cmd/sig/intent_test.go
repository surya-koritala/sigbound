package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeIntent writes an intent file into repo's intents/ directory and returns
// its path, creating the directory on demand.
func writeIntent(t *testing.T, repo, id, body string) string {
	t.Helper()
	if err := os.MkdirAll(intentDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	path := intentPath(repo, id)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeGH puts a `gh` on PATH whose `issue list` prints script's stdout and exits
// with code. It is how the import path is driven without a network or a real
// GitHub CLI; the driver's own `gh` contract (argv, cwd, exit code, stdout JSON)
// is exactly what is being exercised.
func fakeGH(t *testing.T, script string) string {
	t.Helper()
	requirePOSIXShell(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

func TestParseIntentFullFile(t *testing.T) {
	body := `# a hand-written intent
goal = make the cache honest
goal =
goal = it must not serve a stale entry

acceptance = go test ./cache/...
files = cache/store.go, cache/store_test.go
files = cache/doc.go
priority = 7
schedule = 24h
issue = #42
`
	it, err := parseIntent([]byte(body), "cache-honesty")
	if err != nil {
		t.Fatalf("parseIntent: %v", err)
	}
	if it.ID != "cache-honesty" {
		t.Errorf("ID = %q", it.ID)
	}
	if want := "make the cache honest\n\nit must not serve a stale entry"; it.Goal != want {
		t.Errorf("Goal = %q, want %q (repeatable goal lines join with newlines, in file order)", it.Goal, want)
	}
	if it.Acceptance != "go test ./cache/..." {
		t.Errorf("Acceptance = %q", it.Acceptance)
	}
	if got, want := strings.Join(it.Files, ","), "cache/store.go,cache/store_test.go,cache/doc.go"; got != want {
		t.Errorf("Files = %q, want %q", got, want)
	}
	if it.Priority != 7 || it.Schedule != 24*time.Hour || it.Issue != 42 {
		t.Errorf("priority=%d schedule=%s issue=%d", it.Priority, it.Schedule, it.Issue)
	}
}

// TestParseIntentFailsClosed: every malformed intent is a hard error naming
// where it went wrong — a key silently ignored or a value silently defaulted is
// how an intent ends up running work nobody described.
func TestParseIntentFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, id, body string
		want           []string // substrings the error must carry
	}{
		{"no goal", "x", "priority = 1\n", []string{"goal is required"}},
		{"blank goal", "x", "goal =   \n", []string{"goal is required"}},
		{"unknown key", "x", "goal = g\nnope = 1\n", []string{"line 2", "unknown intent key", `"nope"`}},
		{"duplicate scalar", "x", "goal = g\npriority = 1\npriority = 2\n", []string{"line 3", "duplicate key", `"priority"`}},
		{"bad priority", "x", "goal = g\npriority = soon\n", []string{"line 2", "priority must be an integer"}},
		{"bad schedule", "x", "goal = g\nschedule = daily\n", []string{"line 2", "schedule must be a positive duration"}},
		{"zero schedule", "x", "goal = g\nschedule = 0s\n", []string{"line 2", "schedule must be a positive duration"}},
		{"bad issue", "x", "goal = g\nissue = zero\n", []string{"line 2", "issue must be a positive integer"}},
		{"negative issue", "x", "goal = g\nissue = -3\n", []string{"line 2", "issue must be a positive integer"}},
		{"empty acceptance", "x", "goal = g\nacceptance =\n", []string{"line 2", "acceptance requires a command"}},
		{"absolute file", "x", "goal = g\nfiles = /etc/passwd\n", []string{"line 2", "not a safe repo-relative path"}},
		{"dotdot file", "x", "goal = g\nfiles = ../escape.go\n", []string{"line 2", "not a safe repo-relative path"}},
		{"empty files", "x", "goal = g\nfiles =\n", []string{"line 2", "files requires at least one path"}},
		{"no equals", "x", "goal = g\njust a sentence\n", []string{"line 2", "expected KEY=VALUE"}},
		{"unsafe id", "../../etc/passwd", "goal = g\n", []string{"not slug-safe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseIntent([]byte(tc.body), tc.id)
			if err == nil {
				t.Fatalf("parseIntent accepted %q", tc.body)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

func TestListIntentsOrdersAndIgnoresNonIntentFiles(t *testing.T) {
	repo := t.TempDir()
	writeIntent(t, repo, "low", "goal = low\npriority = 1\n")
	writeIntent(t, repo, "high", "goal = high\npriority = 9\n")
	writeIntent(t, repo, "abc", "goal = tie a\n")
	writeIntent(t, repo, "zzz", "goal = tie z\n")
	if err := os.WriteFile(filepath.Join(intentDir(repo), "README.md"), []byte("not an intent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := listIntents(repo)
	if err != nil {
		t.Fatalf("listIntents: %v", err)
	}
	var ids []string
	for _, it := range got {
		ids = append(ids, it.ID)
	}
	if want := "high,low,abc,zzz"; strings.Join(ids, ",") != want {
		t.Errorf("ids = %v, want %s (priority desc, then id asc; README.md ignored)", ids, want)
	}

	// One malformed file fails the WHOLE listing: a list that skipped it would
	// under-report what the repo is asking for.
	writeIntent(t, repo, "broken", "goal = g\nnope = 1\n")
	if _, err := listIntents(repo); err == nil {
		t.Fatal("listIntents accepted a malformed intent file")
	}
}

func TestListIntentsMissingDirIsEmpty(t *testing.T) {
	got, err := listIntents(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("listIntents on a repo with no intents/ = %v, %v; want empty and no error", got, err)
	}
}

func TestIntentListAndShowCLI(t *testing.T) {
	repo := t.TempDir()
	writeIntent(t, repo, "cache", "goal = fix the cache\ngoal = second line\nfiles = cache/a.go\npriority = 3\nschedule = 12h\nissue = 7\n")

	var list bytes.Buffer
	if code, err := runIntent(&list, []string{"list", "-repo", repo, "-json"}); err != nil || code != exitOK {
		t.Fatalf("intent list: code=%d err=%v", code, err)
	}
	var rows []intentJSON
	if err := json.Unmarshal(list.Bytes(), &rows); err != nil {
		t.Fatalf("parse list JSON: %v\n%s", err, list.String())
	}
	if len(rows) != 1 || rows[0].ID != "cache" || rows[0].Priority != 3 || rows[0].Schedule != "12h0m0s" || rows[0].Issue != 7 {
		t.Fatalf("list row = %+v", rows)
	}

	var show bytes.Buffer
	if code, err := runIntent(&show, []string{"show", "cache", "-repo", repo, "-json"}); err != nil || code != exitOK {
		t.Fatalf("intent show: code=%d err=%v", code, err)
	}
	var one intentJSON
	if err := json.Unmarshal(show.Bytes(), &one); err != nil {
		t.Fatalf("parse show JSON: %v\n%s", err, show.String())
	}
	if one.Goal != "fix the cache\nsecond line" || len(one.Files) != 1 {
		t.Fatalf("show = %+v", one)
	}

	// A missing intent names the path it looked for, and is an error, never an
	// empty success that reads like "this intent asks for nothing".
	var missing bytes.Buffer
	code, err := runIntent(&missing, []string{"show", "nosuch", "-repo", repo})
	if err == nil || code == exitOK {
		t.Fatalf("show of a missing intent: code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), intentPath(repo, "nosuch")) {
		t.Errorf("error %q does not name the path it looked for", err)
	}
}

func TestIntentImportGitHubWritesThenSkips(t *testing.T) {
	repo := t.TempDir()
	// printf '%s', not echo: the JSON carries literal \n escapes inside a string,
	// which some shells' echo would expand into real newlines.
	fakeGH(t, `printf '%s\n' '[{"number":42,"title":"Make the cache honest","body":"It must not serve a stale entry.\n\nSee the README."},{"number":7,"title":"Second","body":""}]'`)

	var out bytes.Buffer
	if code, err := runIntent(&out, []string{"import-github", "-repo", repo, "-json"}); err != nil || code != exitOK {
		t.Fatalf("import-github: code=%d err=%v\n%s", code, err, out.String())
	}
	var res []importResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("parse import JSON: %v\n%s", err, out.String())
	}
	if len(res) != 2 || !res[0].Written || !res[1].Written || res[0].ID != "issue-42" {
		t.Fatalf("import results = %+v", res)
	}
	it, err := loadIntent(repo, "issue-42")
	if err != nil {
		t.Fatalf("loadIntent: %v", err)
	}
	if it.Issue != 42 {
		t.Errorf("issue = %d, want 42 (the number a publish command needs to close it)", it.Issue)
	}
	want := "Make the cache honest\n\nIt must not serve a stale entry.\n\nSee the README."
	if it.Goal != want {
		t.Errorf("goal = %q, want %q (title, then the issue body line by line)", it.Goal, want)
	}

	// A local edit, then a re-import: the file must survive byte-identically and
	// be reported as skipped. This is the whole idempotence contract.
	edited := "goal = my own words\npriority = 5\n"
	writeIntent(t, repo, "issue-42", edited)
	var second bytes.Buffer
	if code, err := runIntent(&second, []string{"import-github", "-repo", repo, "-json"}); err != nil || code != exitOK {
		t.Fatalf("re-import: code=%d err=%v", code, err)
	}
	if err := json.Unmarshal(second.Bytes(), &res); err != nil {
		t.Fatalf("parse re-import JSON: %v\n%s", err, second.String())
	}
	if len(res) != 2 || res[0].Written || res[1].Written {
		t.Fatalf("re-import results = %+v, want every file skipped", res)
	}
	got, err := os.ReadFile(intentPath(repo, "issue-42"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != edited {
		t.Errorf("re-import clobbered a local edit:\n%s", got)
	}
}

func TestIntentImportGitHubPassesLabelAndDirToGH(t *testing.T) {
	repo := t.TempDir()
	// The fake records its argv and cwd instead of returning issues, so the
	// contract this code depends on (label flag, JSON fields, cwd = the repo, so
	// gh resolves that repo's own remote) is asserted rather than assumed.
	fakeGH(t, `printf '%s\n' "$*" > "$PWD/gh-argv.txt"; echo '[]'`)
	var out bytes.Buffer
	if code, err := runIntent(&out, []string{"import-github", "-repo", repo, "-label", "roadmap", "-limit", "5"}); err != nil || code != exitOK {
		t.Fatalf("import-github: code=%d err=%v", code, err)
	}
	argv, err := os.ReadFile(filepath.Join(repo, "gh-argv.txt"))
	if err != nil {
		t.Fatalf("gh did not run with cwd = -repo: %v", err)
	}
	for _, want := range []string{"issue list", "--label roadmap", "--state open", "--limit 5", "--json number,title,body"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("gh argv %q lacks %q", strings.TrimSpace(string(argv)), want)
		}
	}
}

func TestIntentImportGitHubGHMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH emptying is exercised on unix; the code path is platform-independent")
	}
	repo := t.TempDir()
	t.Setenv("PATH", t.TempDir()) // an empty dir: no gh anywhere
	var out bytes.Buffer
	code, err := runIntent(&out, []string{"import-github", "-repo", repo})
	if err == nil || code == exitOK {
		t.Fatalf("missing gh: code=%d err=%v, want a clear error", code, err)
	}
	if !strings.Contains(err.Error(), "gh is not in PATH") {
		t.Errorf("error %q does not say gh is missing", err)
	}
	if _, serr := os.Stat(intentDir(repo)); serr == nil {
		t.Error("a failed import created intents/; nothing should be written when gh never ran")
	}
}

func TestIntentImportGitHubUnauthenticated(t *testing.T) {
	repo := t.TempDir()
	fakeGH(t, `echo "gh: To get started with GitHub CLI, please run: gh auth login" >&2; exit 4`)
	var out bytes.Buffer
	code, err := runIntent(&out, []string{"import-github", "-repo", repo})
	if err == nil || code == exitOK {
		t.Fatalf("unauthenticated gh: code=%d err=%v, want a clear error", code, err)
	}
	if !strings.Contains(err.Error(), "gh auth") {
		t.Errorf("error %q does not point at gh's auth state", err)
	}
}

// TestIntentImportGitHubRejectsUnparseableOutput: gh printing something other
// than the requested JSON must be a named error, never a silent zero issues —
// which reads exactly like "nothing is labeled" and would send someone looking
// at GitHub instead of at gh.
func TestIntentImportGitHubRejectsUnparseableOutput(t *testing.T) {
	repo := t.TempDir()
	fakeGH(t, `echo 'not json'`)
	var out bytes.Buffer
	code, err := runIntent(&out, []string{"import-github", "-repo", repo})
	if err == nil || code == exitOK {
		t.Fatalf("garbage from gh: code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "did not return the requested JSON") {
		t.Errorf("error %q does not name the cause", err)
	}
}

// runIntentJSON drives a full `sig run -intent` and returns the decoded report
// plus the exit code, failing on an operational error.
func runIntentJSON(t *testing.T, repo, agent, id string, extra ...string) (runReport, int) {
	t.Helper()
	args := append([]string{"-repo", repo, "-intent", id, "-agent", agent, "-json"}, extra...)
	var buf bytes.Buffer
	code, err := runRun(&buf, args)
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}
	return rep, code
}

// TestRunIntentLandsAndIsAttributable is the acceptance path end to end: an
// intent file becomes the run's task list, the run lands, and `sig log` names
// the intent the landed commit came from.
func TestRunIntentLandsAndIsAttributable(t *testing.T) {
	g, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	writeIntent(t, repo, "cache-honesty", "goal = "+mustJSON(t, map[string]any{
		"write": map[string]string{"cache.go": "package main\n\nfunc cache() int { return 1 }\n"},
	})+"\nfiles = cache.go\n")

	rep, code := runIntentJSON(t, repo, agent, "cache-honesty", "-verify", "go build ./...", "-notes", "-lanes", "strict")
	if code != exitOK {
		t.Fatalf("exit %d, want 0\n%+v", code, rep)
	}
	if rep.Intent != "cache-honesty" {
		t.Errorf("report.intent = %q, want the intent id", rep.Intent)
	}
	if len(rep.Tasks) != 1 || rep.Tasks[0].ID != "cache-honesty" || !strings.Contains(rep.Tasks[0].Prompt, "cache.go") {
		t.Fatalf("tasks = %+v, want one task built from the intent", rep.Tasks)
	}
	if len(rep.Tasks[0].Files) != 1 || rep.Tasks[0].Files[0] != "cache.go" {
		t.Errorf("task lane = %v, want the intent's files", rep.Tasks[0].Files)
	}
	if !landed(&rep) {
		t.Fatalf("run did not land: %+v", rep.Integrate)
	}

	// Attribution: the landing note rides on the landed commit, so `sig log
	// -sha` answers with the intent that produced it.
	var logOut bytes.Buffer
	if code, err := runLog(&logOut, []string{"-repo", repo, "-sha", rep.Integrate.FinalSHA, "-json"}); err != nil || code != exitOK {
		t.Fatalf("sig log -sha: code=%d err=%v\n%s", code, err, logOut.String())
	}
	var prov provenance
	if err := json.Unmarshal(logOut.Bytes(), &prov); err != nil {
		t.Fatalf("parse provenance: %v\n%s", err, logOut.String())
	}
	if prov.Intent != "cache-honesty" {
		t.Errorf("provenance.intent = %q, want cache-honesty (source %s)", prov.Intent, prov.Source)
	}
	var human bytes.Buffer
	if _, err := runLog(&human, []string{"-repo", repo, "-sha", rep.Integrate.FinalSHA}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "intent cache-honesty") {
		t.Errorf("human provenance line does not name the intent:\n%s", human.String())
	}
	// The intent file itself is INPUT and was never committed, so it cannot have
	// landed as part of the work.
	if _, present, err := g.BlobAt(context.Background(), rep.Integrate.FinalSHA, "intents/cache-honesty.intent"); err != nil || present {
		t.Errorf("intent file present in the landed tree (present=%v err=%v)", present, err)
	}
}

// TestIntentAcceptanceTightensPolicyBattery pins the tighten-only rule from both
// sides: the intent's acceptance command is ANDed onto the repo's policy
// battery, so a failing acceptance fails a run whose policy battery passes, and
// a passing acceptance cannot rescue a run whose policy battery fails.
func TestIntentAcceptanceTightensPolicyBattery(t *testing.T) {
	for _, tc := range []struct {
		name, policyVerify, acceptance string
		wantCode                       int
	}{
		{"both pass", "true", "true", exitOK},
		{"acceptance fails", "true", "false", exitVerifyFailed},
		{"policy member fails", "false", "true", exitVerifyFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, repo := makeGoRepo(t)
			agent := buildTestAgent(t)
			commitPolicy(t, g, repo, "verify = "+tc.policyVerify+"\n")
			writeIntent(t, repo, "work", "goal = "+mustJSON(t, map[string]any{
				"write": map[string]string{"work.go": "package main\n"},
			})+"\nacceptance = "+tc.acceptance+"\n")

			rep, code := runIntentJSON(t, repo, agent, "work")
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d (verify %q: %s)", code, tc.wantCode, rep.VerifyCmd, rep.Verify.Output)
			}
			// Both members must be present in the composed command whichever one
			// failed — an acceptance that REPLACED the policy battery would pass
			// this run's exit code check in the "policy member fails" case.
			if !strings.Contains(rep.VerifyCmd, tc.policyVerify) || !strings.Contains(rep.VerifyCmd, tc.acceptance) {
				t.Errorf("effective verify %q does not carry both the policy member and the acceptance", rep.VerifyCmd)
			}
		})
	}
}

// TestIntentAcceptanceComposesWithVerifyFlag: an intent's acceptance and a
// -verify flag are both gates, so both run. Neither replaces the other.
func TestIntentAcceptanceComposesWithVerifyFlag(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	writeIntent(t, repo, "work", "goal = "+mustJSON(t, map[string]any{
		"write": map[string]string{"work.go": "package main\n"},
	})+"\nacceptance = false\n")

	rep, code := runIntentJSON(t, repo, agent, "work", "-verify", "true")
	if code != exitVerifyFailed {
		t.Fatalf("exit %d, want %d: the failing acceptance must fail the run even though -verify passes (verify %q)", code, exitVerifyFailed, rep.VerifyCmd)
	}
	if !strings.Contains(rep.VerifyCmd, "false") || !strings.Contains(rep.VerifyCmd, "true") {
		t.Errorf("effective verify %q does not carry both members", rep.VerifyCmd)
	}
}

func TestRunIntentRejectsMissingAndConflictingSources(t *testing.T) {
	repo := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown intent", []string{"-repo", repo, "-intent", "nope", "-agent", "true"}, "no intent"},
		{"with -tasks", []string{"-repo", repo, "-intent", "a", "-tasks", "t.json", "-agent", "true"}, "mutually exclusive"},
		{"with -goal", []string{"-repo", repo, "-intent", "a", "-goal", "g", "-agent", "true"}, "mutually exclusive"},
		{"with -resume", []string{"-repo", repo, "-intent", "a", "-resume", "-manifest", "m.json", "-agent", "true"}, "-resume does not re-plan"},
		{"no source at all", []string{"-repo", repo, "-agent", "true"}, "one of -tasks, -goal or -intent is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			code, err := runRun(&buf, tc.args)
			if err == nil || code != exitOperationalError {
				t.Fatalf("code=%d err=%v, want an operational error", code, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestResumeCarriesIntentForward: a resumed run never re-reads the intent file
// (its acceptance is already composed into the recorded verify), but it must
// still be attributable to the intent the original run came from.
func TestResumeCarriesIntentForward(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	prior := runReport{
		BaseSHA: "0123456789abcdef0123456789abcdef01234567",
		Tasks:   []taskSpec{{ID: "cache-honesty", Prompt: "do it"}},
		Intent:  "cache-honesty",
	}
	if err := os.WriteFile(path, []byte(mustJSON(t, prior)), 0o644); err != nil {
		t.Fatal(err)
	}
	var base, strategy, agentCmd, resolverCmd, verifyCmd, repairCmd, intentID string
	if _, _, err := loadResumeManifest(path, map[string]bool{}, &base, &strategy, &agentCmd, &resolverCmd, &verifyCmd, &repairCmd, &intentID); err != nil {
		t.Fatalf("loadResumeManifest: %v", err)
	}
	if intentID != "cache-honesty" {
		t.Errorf("resumed intent = %q, want the manifest's recorded intent", intentID)
	}
}
