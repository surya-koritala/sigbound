// Metering (issue #61): a per-run usage record derived from data driveRun's
// report already tracks (agent counts, integrate/verify wall time, repair
// rounds) plus the one number the report itself doesn't carry — the run's total
// wall clock, which the caller brackets around the whole run (acceptance ->
// terminal write, covering planning for a -goal run too). EVERY run records one,
// whichever entry point started it: sig serve's execRun and sig run's runRun
// both end in recordRunUsage (issue #159), because a ledger you have to
// special-case by drive path is not a ledger. There is NO price, currency, or
// external billing call anywhere here: this is the DATA layer a hosted product
// would meter on, not a biller.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// UsageJSON is one run's metering record, always computed for every run that
// produced at least a partial report (mirrors report.json's own gating — see
// execRun and runRun) and persisted alongside it as usage.json, so it survives a
// restart exactly like report.json/error.json do.
type UsageJSON struct {
	AgentsTotal     int   `json:"agentsTotal"`
	AgentsOK        int   `json:"agentsOk"`
	AgentsFailed    int   `json:"agentsFailed"`
	IntegrateWallMs int64 `json:"integrateWallMs"`
	// VerifyAttempts is the number of times the -verify command itself was
	// actually invoked, including -verify-retries and every repair round's
	// re-verify (report.Verify.invocations). RepairAttempts is the number of
	// repair rounds actually run (report.Verify.attempts).
	VerifyAttempts int   `json:"verifyAttempts"`
	RepairAttempts int   `json:"repairAttempts"`
	VerifyWallMs   int64 `json:"verifyWallMs"`
	// TotalWallMs is the run's full wall clock as its driver saw it: from the
	// moment the run was accepted (POST /runs for serve; the end of flag
	// validation for `sig run`) to the run's terminal write, which for a -goal
	// run includes planning time driveRun itself never sees. The two brackets
	// are the same point to within the CLI's own -tasks read, which serve's
	// caller did before it ever posted; validation that FAILS is metered on
	// neither path, returning before any run dir exists. NOT derivable from the
	// report alone (it has no end timestamp).
	TotalWallMs int64 `json:"totalWallMs"`
	// Landed is true iff the run's base ref actually advanced. This is NOT
	// the same as report.integrate.finalSHA != report.baseSHA: finalSHA is
	// populated with the INTEGRATED tree even when -verify fails and nothing
	// is ever written to the ref (see driveRun's landSHA handling) — see
	// computeUsage.
	Landed bool `json:"landed"`
	// ReportBytes is the size of report.json on disk, one crude proxy for
	// how much this run cost to store/transfer.
	ReportBytes int64 `json:"reportBytes"`
	// AgentWallMs is the summed wall time of this run's agent invocations
	// (report.perAgent[].wallMs). Agents run CONCURRENTLY, so this is machine
	// time spent on agents, not elapsed time — it can exceed TotalWallMs. An
	// adopted or -resume-reused branch ran no agent and contributes 0. Written
	// from v2.1 on: a run recorded by an older binary has no per-agent wallMs
	// and so reads back 0 here.
	AgentWallMs int64 `json:"agentWallMs,omitempty"`
	// InputTokens/OutputTokens/CostUSD are INGESTED, not measured: they are the
	// sum of whatever the run's agents wrote to SIGBOUND_USAGE_FILE (see
	// ingestAgentUsage). Sigbound never sees a token or a price itself — this is
	// an optional seam, and every field here is 0 (and omitted) for the normal
	// case where no agent wrote anything. CostAgents is how many agents actually
	// reported, so a reader can tell a genuine zero from partial coverage.
	InputTokens  int64   `json:"inputTokens,omitempty"`
	OutputTokens int64   `json:"outputTokens,omitempty"`
	CostUSD      float64 `json:"costUsd,omitempty"`
	CostAgents   int     `json:"costAgents,omitempty"`
}

// computeUsage derives a run's usage record from its finished report, the
// wall-clock total the caller measured around the whole run (see
// recordRunUsage, which is what every drive path calls), and the run's own
// directory — which is where report.json's size and the optional
// per-agent cost files both come from. landed can only be told apart from
// "verify failed, nothing written to the ref" by combining
// BaseSHA/Integrate.FinalSHA with Verify.Ran/Verify.OK, per Landed's doc comment
// above.
func computeUsage(dir string, rep *runReport, totalWallMs int64) UsageJSON {
	u := UsageJSON{
		AgentsTotal:     len(rep.PerAgent),
		IntegrateWallMs: rep.Integrate.WallMs,
		VerifyAttempts:  rep.Verify.Invocations,
		RepairAttempts:  rep.Verify.Attempts,
		VerifyWallMs:    rep.Verify.WallMs,
		TotalWallMs:     totalWallMs,
		Landed:          rep.Integrate.FinalSHA != rep.BaseSHA && (!rep.Verify.Ran || rep.Verify.OK),
		ReportBytes:     reportFileSize(dir),
	}
	for _, a := range rep.PerAgent {
		if a.OK {
			u.AgentsOK++
		} else {
			u.AgentsFailed++
		}
		u.AgentWallMs = addSat(u.AgentWallMs, a.WallMs)
	}
	ingestAgentUsage(dir, &u)
	return u
}

// recordRunUsage computes and persists dir's metering record for a run that has
// just written its report. BOTH entry points go through here — `sig serve`'s
// execRun and `sig run`'s runRun — because the driveErr rule below is the one
// place the two could silently disagree, and a ledger whose landed flag depends
// on which door the run came in is worse than no ledger.
//
// driveErr is the error driveRun returned (nil on a completed run). A non-nil
// one FORCES Landed false: a driveRun error only ever originates before or
// exactly at landing (see driveRun's err returns), so the ref never advanced —
// while computeUsage's report-field heuristic can read the other way for exactly
// the case that matters. A refused compare-and-swap (gitx.ErrRefMoved) leaves
// FinalSHA set to the integrated tree and Verify green, which the heuristic
// scores as a landing that provably did not happen. The heuristic is trustworthy
// only for a completed, non-erroring driveRun return.
//
// Call it AFTER the report is written: reportBytes is report.json's size.
func recordRunUsage(dir string, rep *runReport, startedAt time.Time, driveErr error) {
	u := computeUsage(dir, rep, time.Since(startedAt).Milliseconds())
	if driveErr != nil {
		u.Landed = false
	}
	writeRunUsage(dir, u)
}

// agentUsagePrefix names the per-agent cost files an agent MAY write via
// SIGBOUND_USAGE_FILE. They live directly in the run directory (one per agent,
// so concurrent agents never share a file) rather than in a subdirectory, so no
// directory has to exist before an agent runs and the filename is always a plain
// name under the run dir whatever the task id contains. See runAgent.
const agentUsagePrefix = "agent-usage-"

// agentUsageJSON is the OPTIONAL token/cost record an agent may write to the
// path in SIGBOUND_USAGE_FILE. Every field is optional and unknown fields are
// IGNORED, not rejected: an agent will usually be dumping its own vendor's usage
// blob, and a seam that only accepted an exact schema would go unused.
type agentUsageJSON struct {
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
}

// agentUsageMaxBytes bounds how much of one agent-written usage file is read
// into the daemon. The record is a handful of numbers; the ceiling is set at a
// vendor blob's worth of slack rather than the minimum that would fit, because
// the seam's whole point is accepting a blob nobody here has seen. What it buys
// is that the SIZE of a daemon allocation stops being the agent's choice: a 1 GiB
// agent-usage-*.json costs 64 KiB and a line on stderr.
const agentUsageMaxBytes = 64 << 10

// ingestAgentUsage sums every agent-usage-*.json in dir into u. It is silent by
// construction: no file (the normal case), an unreadable file, one that is not
// JSON, or one carrying a negative number is skipped without a word, because
// this is a best-effort seam that must never affect a run or its report. A
// skipped file is simply absent from CostAgents, which is what makes partial
// coverage visible instead of pretending to a total.
//
// The ONE loud case is a file over agentUsageMaxBytes: silence there would hide
// the reason a run's cost went missing behind a file the writer believed was
// fine, and unlike malformed JSON it is not a shape the seam ever promised to
// tolerate.
//
// The listing is a ReadDir + prefix/suffix match, NOT filepath.Glob: dir is a
// repo-derived path, and a repo whose path contains a '[' makes a glob pattern
// that matches nothing at all — silently dropping every cost file in the cell.
func ingestAgentUsage(dir string, u *UsageJSON) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, agentUsagePrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		p := filepath.Join(dir, name)
		// Bounded by the READ, not by a stat: the entry type above is a snapshot
		// the agent can invalidate before this open (swap the file for a symlink
		// to something enormous), and a stat would then be sizing a different file
		// than the one read. A LimitReader cannot be raced. One byte over the cap
		// is enough to tell there is more.
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, agentUsageMaxBytes+1))
		f.Close()
		if err != nil {
			continue
		}
		if len(data) > agentUsageMaxBytes {
			fmt.Fprintf(os.Stderr, "sig: ignoring agent usage file over %d bytes: %s\n", agentUsageMaxBytes, p)
			continue
		}
		var a agentUsageJSON
		if json.Unmarshal(data, &a) != nil {
			continue
		}
		if a.InputTokens < 0 || a.OutputTokens < 0 || a.CostUSD < 0 || math.IsNaN(a.CostUSD) || math.IsInf(a.CostUSD, 0) {
			continue
		}
		// Per-file values are finite and non-negative by the check above; their
		// SUM is not bounded by anything — two finite 1e308 costs make +Inf, which
		// no JSON encoder can write, so usage.json would fail to encode and the
		// board would 200 with an empty body. Saturate instead: a huge finite
		// number is wrong in the same direction the input was, and still renders.
		u.InputTokens = addSat(u.InputTokens, a.InputTokens)
		u.OutputTokens = addSat(u.OutputTokens, a.OutputTokens)
		u.CostUSD = addFinite(u.CostUSD, a.CostUSD)
		u.CostAgents++
	}
}

// addSat adds two counters, saturating at the int64 limits instead of wrapping.
// Wrapping a token count turns a huge number into a negative one, which is worse
// than useless in a metric: it is a plausible-looking lie.
func addSat(a, b int64) int64 {
	switch {
	case b > 0 && a > math.MaxInt64-b:
		return math.MaxInt64
	case b < 0 && a < math.MinInt64-b:
		return math.MinInt64
	}
	return a + b
}

// addFinite adds two costs, collapsing an overflow to the largest finite float64
// rather than returning ±Inf/NaN — neither of which encoding/json can write, and
// a value that cannot be encoded takes the whole response down with it.
func addFinite(a, b float64) float64 {
	s := a + b
	if math.IsInf(s, 0) || math.IsNaN(s) {
		return math.MaxFloat64
	}
	return s
}

// reportFileSize returns report.json's on-disk size for dir, 0 if it isn't
// there (or unreadable) — best-effort, matching writeRunReport's own posture.
func reportFileSize(dir string) int64 {
	fi, err := os.Stat(filepath.Join(dir, "report.json"))
	if err != nil {
		return 0
	}
	return fi.Size()
}

// writeRunUsage publishes the metering record atomically, for the same reason
// writeRunReport does: /board reads this file on every request, and a torn read
// silently costs the run its wall-clock and cost numbers for that response.
//
// Best-effort, like every other durable write around it: a failure here is a
// line on stderr and nothing more. By the time this is called the run is over
// and its report already written, whatever the outcome was — landed or not —
// so losing the meter must never look like losing the run itself. The prefix is
// the bare command name because both entry points reach this — a `sig run`
// warning that announced itself as `sig serve` would send its reader hunting a
// daemon that isn't running.
func writeRunUsage(dir string, u UsageJSON) {
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sig: encode usage for %s: %v\n", dir, err)
		return
	}
	if err := atomicWriteFile(dir, "usage.json", data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "sig: write usage %s: %v\n", dir, err)
	}
}

func readRunUsage(dir string) (*UsageJSON, error) {
	data, err := os.ReadFile(filepath.Join(dir, "usage.json"))
	if err != nil {
		return nil, err
	}
	var u UsageJSON
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// usageTotals is GET /usage's aggregate shape: UsageJSON's fields summed
// across every run that has a usage record, plus Runs/Landed counts.
type usageTotals struct {
	Runs            int   `json:"runs"`
	Landed          int   `json:"landed"`
	AgentsTotal     int   `json:"agentsTotal"`
	AgentsOK        int   `json:"agentsOk"`
	AgentsFailed    int   `json:"agentsFailed"`
	IntegrateWallMs int64 `json:"integrateWallMs"`
	VerifyAttempts  int   `json:"verifyAttempts"`
	RepairAttempts  int   `json:"repairAttempts"`
	VerifyWallMs    int64 `json:"verifyWallMs"`
	TotalWallMs     int64 `json:"totalWallMs"`
	ReportBytes     int64 `json:"reportBytes"`
}

// addUsage folds one run's usage record into these totals.
func (t *usageTotals) addUsage(u UsageJSON) {
	t.Runs++
	if u.Landed {
		t.Landed++
	}
	t.AgentsTotal += u.AgentsTotal
	t.AgentsOK += u.AgentsOK
	t.AgentsFailed += u.AgentsFailed
	t.IntegrateWallMs += u.IntegrateWallMs
	t.VerifyAttempts += u.VerifyAttempts
	t.RepairAttempts += u.RepairAttempts
	t.VerifyWallMs += u.VerifyWallMs
	t.TotalWallMs += u.TotalWallMs
	t.ReportBytes += u.ReportBytes
}

// addTotals folds another cell's already-summed totals into these (the
// grand-total rollup across cells).
func (t *usageTotals) addTotals(o usageTotals) {
	t.Runs += o.Runs
	t.Landed += o.Landed
	t.AgentsTotal += o.AgentsTotal
	t.AgentsOK += o.AgentsOK
	t.AgentsFailed += o.AgentsFailed
	t.IntegrateWallMs += o.IntegrateWallMs
	t.VerifyAttempts += o.VerifyAttempts
	t.RepairAttempts += o.RepairAttempts
	t.VerifyWallMs += o.VerifyWallMs
	t.TotalWallMs += o.TotalWallMs
	t.ReportBytes += o.ReportBytes
}
