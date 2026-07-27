package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// ---- issue #174: -run-id ----

// runsDirOf is the durable run root a run writes into.
func runsDirOf(t *testing.T, repo string) string {
	t.Helper()
	common, err := gitx.New(repo).GitCommonDir(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(common, "sigbound", "runs")
}

// TestRunIDIsUsedVerbatim is the whole point of the flag: a caller that queued
// work under an id it chose must find the run under THAT id, not one the engine
// picked. Asserting the directory (and not merely the report's runId field) is
// what makes it a usable handle — `sig ack`, `sig reject` and the timeout sweep
// all resolve a run by finding that directory.
func TestRunIDIsUsedVerbatim(t *testing.T) {
	_, repo := makeGoRepo(t)
	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo, "-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{"write": map[string]string{"a.txt": "x"}})}}),
		"-agent", buildTestAgent(t), "-run-id", "queued-run-1", "-json",
	})
	if err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	if code != exitOK {
		t.Fatalf("code=%d, want exitOK\n%s", code, buf.String())
	}
	if _, statErr := os.Stat(filepath.Join(runsDirOf(t, repo), "queued-run-1")); statErr != nil {
		t.Fatalf("no run directory at the id the caller asked for: %v", statErr)
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, buf.String())
	}
	if rep.RunID != "queued-run-1" {
		t.Fatalf("report runId=%q, want queued-run-1 — the caller cannot correlate its own id", rep.RunID)
	}
}

// TestRunIDReachesTheFirstEvent: a caller streaming -events must see ITS id in
// the first line, or it cannot correlate the stream with the run it started —
// which is the reconciliation the flag exists for.
func TestRunIDReachesTheFirstEvent(t *testing.T) {
	_, repo := makeGoRepo(t)
	events := filepath.Join(t.TempDir(), "events.jsonl")
	var buf bytes.Buffer
	if _, err := runRun(&buf, []string{
		"-repo", repo, "-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{"write": map[string]string{"a.txt": "x"}})}}),
		"-agent", buildTestAgent(t), "-run-id", "evt-run-1", "-events", events,
	}); err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)[0]
	var ev map[string]any
	if err := json.Unmarshal([]byte(first), &ev); err != nil {
		t.Fatalf("first event is not JSON: %v (%q)", err, first)
	}
	if ev["event"] != "run_start" {
		t.Fatalf("first event is %v, want run_start", ev["event"])
	}
	if ev["runId"] != "evt-run-1" {
		t.Fatalf("run_start carries runId=%v, want evt-run-1 — a streaming caller cannot match its own id", ev["runId"])
	}
}

// TestRunIDUnsafeIsRefusedBeforeAnythingExists. The id becomes BOTH a directory
// name and a git branch component, so both properties are checked — and checked
// before any directory is created, which is the acceptance the issue names
// specifically. -logdir is passed precisely because it is a directory runRun
// would otherwise create on the way to discovering the id was bad.
func TestRunIDUnsafeIsRefusedBeforeAnythingExists(t *testing.T) {
	for _, id := range []string{
		"../escape",  // path traversal
		"a/b",        // separator
		"a..b",       // legal chars, illegal ref component
		".hidden",    // leading dot: illegal ref component
		"-dashfirst", // reads as a flag in argv
		"x.lock",     // git refuses
		".",
		"..",
	} {
		t.Run(id, func(t *testing.T) {
			_, repo := makeGoRepo(t)
			logDir := filepath.Join(t.TempDir(), "logs")
			var buf bytes.Buffer
			code, err := runRun(&buf, []string{
				"-repo", repo, "-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: "x"}}),
				"-agent", "true", "-run-id", id, "-logdir", logDir,
			})
			if err == nil {
				t.Fatalf("id %q was accepted", id)
			}
			if !strings.Contains(err.Error(), "-run-id") {
				t.Fatalf("error does not name the flag the caller must fix: %v", err)
			}
			if code != exitOperationalError {
				t.Fatalf("code=%d, want exitOperationalError", code)
			}
			if _, statErr := os.Stat(logDir); statErr == nil {
				t.Fatal("-logdir was created before the id was rejected; the refusal is not happening before anything is created")
			}
			if entries, _ := os.ReadDir(runsDirOf(t, repo)); len(entries) != 0 {
				t.Fatalf("a run directory was created for a refused id: %v", entries)
			}
		})
	}
}

// TestRunIDCollisionRefusesAndLeavesTheOtherRunAlone. Merging into an existing
// run's directory would corrupt the record `sig ack` and the timeout sweep
// resolve through, so the second use is refused — and the FIRST run's report
// must still be exactly what it was, which is the half a bare "it errored"
// assertion would miss.
func TestRunIDCollisionRefusesAndLeavesTheOtherRunAlone(t *testing.T) {
	_, repo := makeGoRepo(t)
	agent := buildTestAgent(t)
	tasks := tasksFileFor(t, []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{"write": map[string]string{"a.txt": "first"}})}})

	var buf bytes.Buffer
	if _, err := runRun(&buf, []string{"-repo", repo, "-tasks", tasks, "-agent", agent, "-run-id", "dup"}); err != nil {
		t.Fatalf("first run: %v\n%s", err, buf.String())
	}
	reportPath := filepath.Join(runsDirOf(t, repo), "dup", "report.json")
	before, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}

	buf.Reset()
	code, err := runRun(&buf, []string{"-repo", repo, "-tasks", tasks, "-agent", agent, "-run-id", "dup"})
	if err == nil {
		t.Fatal("reusing an existing run id was accepted")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error does not say the id is taken: %v", err)
	}
	if code != exitOperationalError {
		t.Fatalf("code=%d, want exitOperationalError", code)
	}
	after, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("the first run's report is gone: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the first run's report changed; the refused run wrote into someone else's directory")
	}
}

// TestRunIDUnsetIsUnchanged: the generated id is still a timestamp-prefixed one,
// so a repo that never passes the flag sorts and behaves exactly as before.
func TestRunIDUnsetIsUnchanged(t *testing.T) {
	_, repo := makeGoRepo(t)
	var buf bytes.Buffer
	if _, err := runRun(&buf, []string{
		"-repo", repo, "-tasks", tasksFileFor(t, []taskSpec{{ID: "a", Prompt: mustJSON(t, map[string]any{"write": map[string]string{"a.txt": "x"}})}}),
		"-agent", buildTestAgent(t), "-json",
	}); err != nil {
		t.Fatalf("runRun: %v\n%s", err, buf.String())
	}
	var rep runReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if _, terr := time.Parse(runIDTimeLayout, strings.SplitN(rep.RunID, "-", 2)[0]); terr != nil {
		t.Fatalf("generated runId %q lost its timestamp prefix, so `sig log`'s chronological sort breaks: %v", rep.RunID, terr)
	}
}

// ---- issue #173: signals ----

// TestInterruptedBySignalIgnoresConfiguredTimeouts is the subtle half, and the
// one most likely to be got wrong. -budget and -agent-timeout both derive their
// own WithTimeout children from the run's context; if the classification asked a
// PHASE's context instead of the signal one, every budget exhaustion would be
// reported as "someone stopped us" and every retry policy keyed on that would
// misfire.
func TestInterruptedBySignalIgnoresConfiguredTimeouts(t *testing.T) {
	sigCtx, stop := context.WithCancel(context.Background())
	defer stop()

	// A budget/agent-timeout child expiring says nothing about the signal.
	budgetCtx, cancelBudget := context.WithCancel(sigCtx)
	cancelBudget()
	if budgetCtx.Err() == nil {
		t.Fatal("precondition: the derived child should be done")
	}
	if interruptedBySignal(sigCtx) {
		t.Fatal("a run whose -budget expired was classified as interrupted by a signal; every caller retrying on interruption would now retry a run that did exactly what it was told")
	}

	// The signal itself is the only thing that flips it.
	stop()
	if !interruptedBySignal(sigCtx) {
		t.Fatal("a cancelled signal context is not reported as an interruption")
	}
}

// TestRunInterruptedSalvagesTheRecord is the acceptance the issue calls the case
// that matters most: a run stopped part-way must leave a record of what it had
// already done, marked as interrupted, with a distinct exit code — instead of
// vanishing and leaving the caller unable to tell an interrupted run from a
// crashed one.
//
// The interleaving is FORCED, not slept for. The agent command writes a sentinel
// and then blocks; the test waits for that sentinel — an explicit signal from
// the child that it is running — and only then cancels. There is no window to
// miss.
func TestRunInterruptedSalvagesTheRecord(t *testing.T) {
	requirePOSIXShell(t)
	_, repo := makeGoRepo(t)

	sentinel := filepath.Join(t.TempDir(), "agent-started")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prev := notifyRunSignals
	notifyRunSignals = func(context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} }
	t.Cleanup(func() { notifyRunSignals = prev })

	go func() {
		for {
			if _, err := os.Stat(sentinel); err == nil {
				cancel() // the agent is provably mid-flight
				return
			}
			if ctx.Err() != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var buf bytes.Buffer
	code, err := runRun(&buf, []string{
		"-repo", repo,
		"-tasks", tasksFileFor(t, []taskSpec{{ID: "slow", Prompt: "x"}}),
		"-agent", "sh -c 'touch " + sentinel + "; sleep 120'",
		"-run-id", "interrupted-run",
	})

	// err may be nil: cancelling kills the agents, and a run whose every agent
	// died completes "normally" with nothing landed. The exit code is what has
	// to tell the truth.
	if code != exitInterrupted {
		t.Fatalf("code=%d, want exitInterrupted(%d) — a caller cannot tell being stopped from failing: %v", code, exitInterrupted, err)
	}

	// The record has to exist, and has to say interrupted rather than error.
	statusPath := filepath.Join(runsDirOf(t, repo), "interrupted-run", "status.json")
	data, rerr := os.ReadFile(statusPath)
	if rerr != nil {
		t.Fatalf("the interrupted run left no status record at all: %v", rerr)
	}
	var st struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("status.json is not JSON: %v (%s)", err, data)
	}
	if st.Status != "interrupted" {
		t.Fatalf("status=%q, want interrupted — an interrupted run is indistinguishable from a failed one", st.Status)
	}
}
