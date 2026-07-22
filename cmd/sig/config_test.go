package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- parseConfigFile: pure parser unit tests ----

func TestParseConfigFileBasics(t *testing.T) {
	data := []byte("# a standing config\n" +
		"agent=./my-agent\n" +
		"\n" +
		"  # indented comment\n" +
		"lanes = strict\n") // spaces around '=' are trimmed from both key and value
	entries, err := parseConfigFile(data)
	if err != nil {
		t.Fatalf("parseConfigFile: %v", err)
	}
	want := []configEntry{
		{Key: "agent", Value: "./my-agent", Line: 2},
		{Key: "lanes", Value: "strict", Line: 5},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries=%+v, want %+v", entries, want)
	}
	for i, e := range entries {
		if e != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, e, want[i])
		}
	}
}

// TestParseConfigFileValueWithEqualsAndSpaces: the split is on the FIRST '='
// only, so a value may contain '=' and internal spaces (a shell command being
// the obvious case) and both survive verbatim, only the outer whitespace is
// trimmed.
func TestParseConfigFileValueWithEqualsAndSpaces(t *testing.T) {
	entries, err := parseConfigFile([]byte(`verify=X=1; test "$X" = "1" && go build ./...` + "\n"))
	if err != nil {
		t.Fatalf("parseConfigFile: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%+v, want 1", entries)
	}
	want := `X=1; test "$X" = "1" && go build ./...`
	if entries[0].Key != "verify" || entries[0].Value != want {
		t.Fatalf("entry=%+v, want key=verify value=%q", entries[0], want)
	}
}

// TestParseConfigFileCRLF: Windows-style line endings must not become part of
// the value or trip up comment/blank detection.
func TestParseConfigFileCRLF(t *testing.T) {
	data := []byte("# comment\r\nagent=./a\r\n\r\nlanes=warn\r\n")
	entries, err := parseConfigFile(data)
	if err != nil {
		t.Fatalf("parseConfigFile: %v", err)
	}
	if len(entries) != 2 || entries[0].Value != "./a" || entries[1].Value != "warn" {
		t.Fatalf("entries=%+v", entries)
	}
}

// TestParseConfigFileMalformedLine: a line with no '=' at all is a hard
// error naming its 1-based line number, not a silently-skipped line.
func TestParseConfigFileMalformedLine(t *testing.T) {
	_, err := parseConfigFile([]byte("agent=./a\nnotakeyvalueline\nlanes=warn\n"))
	if err == nil {
		t.Fatal("want an error for a line with no '='")
	}
	if got := err.Error(); got != `line 2: expected KEY=VALUE, got "notakeyvalueline"` {
		t.Fatalf("error=%q", got)
	}
}

// TestParseConfigFileEmptyKey: "=value" has no key at all.
func TestParseConfigFileEmptyKey(t *testing.T) {
	_, err := parseConfigFile([]byte("=novalue\n"))
	if err == nil {
		t.Fatal("want an error for an empty key")
	}
	if got := err.Error(); got != "line 1: empty key" {
		t.Fatalf("error=%q", got)
	}
}

// TestParseConfigFileHugeKey: an implausibly long key must not panic or hang;
// it just parses as an entry (rejected later, by fs.Set, as an unknown flag).
func TestParseConfigFileHugeKey(t *testing.T) {
	huge := make([]byte, 1<<20)
	for i := range huge {
		huge[i] = 'k'
	}
	entries, err := parseConfigFile(append(huge, []byte("=v\n")...))
	if err != nil {
		t.Fatalf("parseConfigFile: %v", err)
	}
	if len(entries) != 1 || len(entries[0].Key) != len(huge) {
		t.Fatalf("entries=%d, key len=%d, want 1 entry with key len %d", len(entries), len(entries[0].Key), len(huge))
	}
}

// ---- resolveConfigPath ----

func TestResolveConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// No sig.conf present: default (unset) discovery finds nothing, no error.
	if path, err := resolveConfigPath(""); err != nil || path != "" {
		t.Fatalf("empty flag, no sig.conf: path=%q err=%v, want \"\", nil", path, err)
	}

	if err := os.WriteFile(filepath.Join(dir, configDiscoveryName), []byte("agent=./a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Default (unset) discovery now finds ./sig.conf.
	if path, err := resolveConfigPath(""); err != nil || path != configDiscoveryName {
		t.Fatalf("empty flag, sig.conf present: path=%q err=%v, want %q, nil", path, err, configDiscoveryName)
	}

	// "none" disables discovery even though sig.conf is right there.
	if path, err := resolveConfigPath("none"); err != nil || path != "" {
		t.Fatalf(`-config none: path=%q err=%v, want "", nil`, path, err)
	}

	// An explicit path is used as-is, verbatim (existence isn't checked by
	// resolveConfigPath itself — applyConfigFile's os.ReadFile surfaces that).
	if path, err := resolveConfigPath("/some/other/path.conf"); err != nil || path != "/some/other/path.conf" {
		t.Fatalf("explicit path: path=%q err=%v", path, err)
	}
}

// ---- applyConfigFile / runRun integration ----

// TestConfigPrecedenceFlagBeatsConfigBeatsDefault drives three real runs of
// the same task through runRun and inspects the JSON report's
// integrate.strategy, proving all three precedence tiers documented for
// -config: an explicit command-line flag wins over the config file, the
// config file wins over the flag's built-in default, and with neither set
// the plain default applies.
func TestConfigPrecedenceFlagBeatsConfigBeatsDefault(t *testing.T) {
	agent := buildTestAgent(t)
	confPath := filepath.Join(t.TempDir(), "sig.conf")
	if err := os.WriteFile(confPath, []byte("strategy=mergetree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oneTask := func(t *testing.T) []taskSpec {
		return []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
		})}}
	}
	runWithStrategy := func(t *testing.T, extraArgs ...string) string {
		t.Helper()
		_, repo := makeGoRepo(t)
		args := append([]string{
			"-repo", repo,
			"-tasks", tasksFileFor(t, oneTask(t)),
			"-agent", agent,
			"-json",
		}, extraArgs...)
		var buf bytes.Buffer
		code, err := runRun(&buf, args)
		if err != nil {
			t.Fatalf("runRun: %v\n%s", err, buf.String())
		}
		if code != exitOK {
			t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
		}
		var rep runReport
		if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
			t.Fatalf("parse report: %v\n%s", err, buf.String())
		}
		return rep.Integrate.Strategy
	}

	if got := runWithStrategy(t, "-config", confPath); got != "mergetree" {
		t.Fatalf("config-only: strategy=%q, want mergetree (config supplies the default)", got)
	}
	if got := runWithStrategy(t, "-config", confPath, "-strategy", "overlay"); got != "overlay" {
		t.Fatalf("flag+config: strategy=%q, want overlay (explicit flag beats config)", got)
	}
	if got := runWithStrategy(t); got != "overlay" {
		t.Fatalf("neither set: strategy=%q, want overlay (the plain flag default)", got)
	}
}

// TestConfigDiscoveryOnAndOff proves ./sig.conf discovery in the CURRENT
// WORKING DIRECTORY: present with -config left unset, it applies; present
// with -config=none, it's ignored entirely. The agent binary and every path
// passed to runRun are built BEFORE t.Chdir, since `go build` on this
// module's own package needs to run from inside the module.
func TestConfigDiscoveryOnAndOff(t *testing.T) {
	agent := buildTestAgent(t)
	_, repoOn := makeGoRepo(t)
	_, repoOff := makeGoRepo(t)
	taskFor := func(id string) []taskSpec {
		return []taskSpec{{ID: id, Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{id + ".go": "package main\n\nfunc " + id + "() int { return 1 }\n"},
		})}}
	}
	tasksOn := tasksFileFor(t, taskFor("a"))
	tasksOff := tasksFileFor(t, taskFor("b"))

	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.WriteFile(filepath.Join(cwd, configDiscoveryName), []byte("strategy=mergetree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, repo, tasksFile string, extraArgs ...string) string {
		t.Helper()
		args := append([]string{"-repo", repo, "-tasks", tasksFile, "-agent", agent, "-json"}, extraArgs...)
		var buf bytes.Buffer
		code, err := runRun(&buf, args)
		if err != nil {
			t.Fatalf("runRun: %v\n%s", err, buf.String())
		}
		if code != exitOK {
			t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
		}
		var rep runReport
		if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
			t.Fatalf("parse report: %v\n%s", err, buf.String())
		}
		return rep.Integrate.Strategy
	}

	if got := run(t, repoOn, tasksOn); got != "mergetree" {
		t.Fatalf("discovery on (-config unset, ./sig.conf present): strategy=%q, want mergetree", got)
	}
	if got := run(t, repoOff, tasksOff, "-config", "none"); got != "overlay" {
		t.Fatalf("discovery off (-config=none): strategy=%q, want overlay (sig.conf must be ignored)", got)
	}
}

// TestConfigUnknownKeyFailsWithLineNumber: a config file naming a flag that
// doesn't exist must fail loudly, naming both the bad key and the line it
// came from, and — same fail-fast policy as -logdir/-events — before any
// agent runs.
func TestConfigUnknownKeyFailsWithLineNumber(t *testing.T) {
	_, repo := makeGoRepo(t)
	marker := filepath.Join(t.TempDir(), "agent-ran")
	confPath := filepath.Join(t.TempDir(), "sig.conf")
	// line 1 comment, line 2 valid, line 3 the bad key.
	conf := "# standing flags\n" +
		"strategy=overlay\n" +
		"bogus-flag=value\n"
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-config", confPath,
		"-repo", repo,
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", "touch " + marker,
	})
	if err == nil {
		t.Fatal("want an error for an unknown config key")
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	// stdlib flag.FlagSet.Set's own error text is "no such flag -<name>";
	// applyConfigFile wraps it with "<path>:<line>: ", so this pins both the
	// bad key AND the exact line (3) it came from, not just "some digit 3
	// appears somewhere" (confPath itself is under a t.TempDir() path that
	// can contain digits).
	if got := err.Error(); !strings.Contains(got, ":3: no such flag -bogus-flag") {
		t.Fatalf("error=%q, want it to contain \":3: no such flag -bogus-flag\"", got)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("agent ran despite a bad -config file; must fail before any agent runs")
	}
}

// TestConfigRejectsConfigKeyInFile: "config" can never appear as a key
// inside the file itself — it would be self-referential.
func TestConfigRejectsConfigKeyInFile(t *testing.T) {
	_, repo := makeGoRepo(t)
	confPath := filepath.Join(t.TempDir(), "sig.conf")
	if err := os.WriteFile(confPath, []byte("config=other.conf\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	_, err := runRun(&buf, []string{
		"-config", confPath,
		"-repo", repo,
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", "true",
	})
	if err == nil {
		t.Fatal(`want an error: "config" is not allowed as a key inside a config file`)
	}
	if got := err.Error(); !strings.Contains(got, `"config" is not allowed`) {
		t.Fatalf("error=%q, want it to name the disallowed key", got)
	}
}

// TestConfigLanesCountsAsExplicitForPlannedRunStrictDefault is the key
// composition case from issue #13's design: -goal runs default -lanes to
// strict UNLESS -lanes was set explicitly — and a config-file -lanes value
// must count as explicit for that check, same as a command-line one, since
// the user chose it deliberately either way. Uses -dry-run (no live agent
// needed) so the predicted laneMode is directly visible in the report.
func TestConfigLanesCountsAsExplicitForPlannedRunStrictDefault(t *testing.T) {
	_, repo := makeGoRepo(t)
	planner := planFileCmd(t, mustJSON(t, alphaBetaPlan(t)))
	confPath := filepath.Join(t.TempDir(), "sig.conf")
	if err := os.WriteFile(confPath, []byte("lanes=warn\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-goal", "add alpha and beta helpers",
		"-planner", planner,
		"-n", "2",
		"-agent", "true",
		"-config", confPath,
		"-dry-run",
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
	}
	var rep dryRunReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}
	if rep.LaneMode != laneWarn {
		t.Fatalf("laneMode=%q, want %q: a config-file -lanes must count as explicit and override -goal's strict default", rep.LaneMode, laneWarn)
	}
}

// TestConfigValueWithEqualsAndSpacesAppliesEndToEnd proves a config value
// containing both '=' and internal spaces (a realistic shell command) makes
// it all the way through parseConfigFile -> fs.Set -> the actual -verify
// command run against the integrated tree, not just through the parser in
// isolation (see TestParseConfigFileValueWithEqualsAndSpaces for that).
func TestConfigValueWithEqualsAndSpacesAppliesEndToEnd(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	confPath := filepath.Join(t.TempDir(), "sig.conf")
	// The value itself contains '=' (twice) and spaces; parseConfigFile must
	// keep it intact so the shell sees exactly this command.
	conf := `verify=X=1; test "$X" = "1" && go build ./...` + "\n"
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-config", confPath,
		"-repo", repo,
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{
			"write": map[string]string{"a.go": "package main\n\nfunc a() int { return 1 }\n"},
		})}}),
		"-agent", agent,
		"-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, buf.String())
	}
	if !rep.Verify.Ran || !rep.Verify.OK {
		t.Fatalf("verify ran=%v ok=%v output=%q, want the config-supplied -verify command to run and pass", rep.Verify.Ran, rep.Verify.OK, rep.Verify.Output)
	}
}

// TestConfigExplicitFlagValueAppliesViaFlagValueParsing: fs.Set reuses each
// flag's own flag.Value parsing, so a non-string flag (e.g. -assert, a bool)
// set from the config file is parsed the same way it would be from argv, and
// a bad value for it fails the same way -config's own bad-key case does:
// loudly, naming the line.
func TestConfigExplicitFlagValueAppliesViaFlagValueParsing(t *testing.T) {
	_, repo := makeGoRepo(t)
	confPath := filepath.Join(t.TempDir(), "sig.conf")
	if err := os.WriteFile(confPath, []byte("assert=not-a-bool\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	_, err := runRun(&buf, []string{
		"-config", confPath,
		"-repo", repo,
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
		"-agent", "true",
	})
	if err == nil {
		t.Fatal("want an error: assert=not-a-bool is not a valid bool")
	}
	if got := err.Error(); !strings.Contains(got, ":1: ") {
		t.Fatalf("error=%q, want it to name line 1", got)
	}
}
