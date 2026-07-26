// Metering for sig serve's managed layer (issue #61): a per-run usage record
// derived from data driveRun's report already tracks (agent counts,
// integrate/verify wall time, repair rounds) plus the one number the report
// itself doesn't carry — the run's total wall clock, which sig serve's
// execRun brackets (POST /runs acceptance -> the run's terminal write,
// covering planning for a -goal run too). There is NO price, currency, or
// external billing call anywhere here: this is the DATA layer a hosted
// product would meter on, not a biller.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// UsageJSON is one run's metering record, always computed for every run that
// produced at least a partial report (mirrors report.json's own gating — see
// execRun) and persisted alongside it as usage.json, so it survives a
// restart exactly like report.json/error.json do.
type UsageJSON struct {
	AgentsTotal     int   `json:"agentsTotal"`
	AgentsOK        int   `json:"agentsOk"`
	AgentsFailed    int   `json:"agentsFailed"`
	IntegrateWallMs int64 `json:"integrateWallMs"`
	// VerifyAttempts is the number of times the -verify command itself was
	// actually invoked, including -verify-retries and every repair round's
	// re-verify (report.verify.invocations). RepairAttempts is the number of
	// repair rounds actually run (report.verify.attempts).
	VerifyAttempts int   `json:"verifyAttempts"`
	RepairAttempts int   `json:"repairAttempts"`
	VerifyWallMs   int64 `json:"verifyWallMs"`
	// TotalWallMs is the run's full wall clock as sig serve saw it: from
	// POST /runs acceptance to the run's terminal write, which for a -goal
	// run includes planning time driveRun itself never sees. NOT derivable
	// from the report alone (it has no end timestamp) — see execRun.
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
// wall-clock total the caller measured around the whole run (see execRun), and
// the run's own directory — which is where report.json's size and the optional
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
		u.AgentWallMs += a.WallMs
	}
	ingestAgentUsage(dir, &u)
	return u
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

// ingestAgentUsage sums every agent-usage-*.json in dir into u. It is silent by
// construction: no file (the normal case), an unreadable file, one that is not
// JSON, or one carrying a negative number is skipped without a word, because
// this is a best-effort seam that must never affect a run or its report. A
// skipped file is simply absent from CostAgents, which is what makes partial
// coverage visible instead of pretending to a total.
func ingestAgentUsage(dir string, u *UsageJSON) {
	paths, err := filepath.Glob(filepath.Join(dir, agentUsagePrefix+"*.json"))
	if err != nil {
		return // only ErrBadPattern, which this fixed pattern cannot produce
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var a agentUsageJSON
		if json.Unmarshal(data, &a) != nil {
			continue
		}
		if a.InputTokens < 0 || a.OutputTokens < 0 || a.CostUSD < 0 || math.IsNaN(a.CostUSD) || math.IsInf(a.CostUSD, 0) {
			continue
		}
		u.InputTokens += a.InputTokens
		u.OutputTokens += a.OutputTokens
		u.CostUSD += a.CostUSD
		u.CostAgents++
	}
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

func writeRunUsage(dir string, u UsageJSON) {
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sig serve: encode usage for %s: %v\n", dir, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "usage.json"), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "sig serve: write usage %s: %v\n", dir, err)
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
