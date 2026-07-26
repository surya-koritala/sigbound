// GET /board (issue #114): the delivery board and its metrics, both DERIVED —
// the board from the run journal (.git/sigbound/runs) and the repo's intents
// directory, the metrics from the same journal plus the per-run metering records
// serve already writes (usage.json). It stores nothing and decides nothing.
//
// A card cannot be moved, because a card is a fact. An intent's column is
// COMPUTED from its runs on every request (see boardColumn), so the board can
// never hold an opinion the journal disagrees with: delete a run dir and the
// card moves; ack a park and the card moves. There is no board state to
// reconcile, and this file adds no mutating endpoint — ack/reject on a parked
// run remain the only mutations the review UI can reach.
//
// Rows come from scanRuns, the SAME reader `sig log` and GET /log use, so the
// board, the log and the CLI cannot drift. Metrics are counters, not rates: a
// consumer divides them itself, and the reconciliation test can re-derive every
// one of them from the raw files.
//
// LIMIT worth stating: both halves are bounded by ?limit (default 50 runs per
// cell, 0 = all). The metrics therefore describe that window — the last N runs
// per cell — not all history, and they only count runs that left a readable
// report.
package main

import (
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Board columns. Every one is derived from the journal on read (see
// boardColumn); none is settable.
const (
	boardOpen        = "open"         // no run has been recorded for this intent yet
	boardRunning     = "running"      // a run is queued or in flight
	boardAwaitingAck = "awaiting-ack" // a park is waiting on a human — the actionable column
	boardLanded      = "landed"       // the newest run advanced the base ref
	boardAttention   = "attention"    // newest run finished without landing, errored, or was interrupted
)

// boardDefaultLimit bounds the per-cell run scan when the caller names no
// ?limit, matching GET /log's default so the two surfaces read the same window.
const boardDefaultLimit = 50

// boardIntent is one card: an intent (or the no-intent bucket) plus the runs
// attributed to it, newest first. Goal/Priority come from intents/<id>.intent
// and are absent for a card the journal produced but the intents directory no
// longer has a file for — Missing marks exactly that case, which is the journal
// winning over the directory rather than a run being dropped from the board.
type boardIntent struct {
	ID       string   `json:"id"` // "" for the no-intent bucket
	Column   string   `json:"column"`
	Goal     string   `json:"goal,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Schedule string   `json:"schedule,omitempty"`
	Issue    int      `json:"issue,omitempty"`
	Missing  bool     `json:"missing,omitempty"`
	Runs     []logRow `json:"runs"`
}

// boardCell is one registered cell's whole board. IntentsError is set when the
// intents directory would not parse: the runs still render (they come from the
// journal, which does not depend on that directory), and the reason the intent
// cards are missing is reported rather than swallowed.
type boardCell struct {
	Cell         string        `json:"cell"`
	Repo         string        `json:"repo"`
	Intents      []boardIntent `json:"intents"`
	IntentsError string        `json:"intentsError,omitempty"`
}

// boardPreset is landing success for one -agent-preset. Preset is the preset
// NAME when the run's recorded agent command is byte-identical to that preset's
// expansion, and "custom" otherwise — sigbound records the RESOLVED command and
// never the preset name (see docs/USAGE.md's Provenance section), so this is a
// reverse match on the expansion, not a recorded fact. A preset command edited
// by hand therefore reads as "custom".
type boardPreset struct {
	Preset string `json:"preset"`
	Runs   int    `json:"runs"`
	Landed int    `json:"landed"`
}

// boardMetrics is the metrics half: COUNTERS, so every rate a reader wants is
// an explicit ratio of two numbers they can see, and so a test can re-derive
// each one from the raw report/usage files independently.
type boardMetrics struct {
	Runs   int `json:"runs"`   // runs with a readable report inside the window
	Landed int `json:"landed"` // of those, runs whose base ref actually advanced
	// Verify pass rate = VerifyPassed / VerifyRan. Runs with no -verify
	// configured are in neither number.
	VerifyRan    int `json:"verifyRan"`
	VerifyPassed int `json:"verifyPassed"`
	// Salvage (bisect-save) rate = BisectSalvaged / BisectRan. Bisect only runs
	// when the full combined tree failed verify, so BisectRan is already
	// "runs that would otherwise have landed nothing".
	BisectRan      int `json:"bisectRan"`
	BisectSalvaged int `json:"bisectSalvaged"`
	// Flag rate = FlaggedRuns / Runs: runs that set at least one branch aside as
	// a real conflict. Counted per RUN, not per branch.
	FlaggedRuns int `json:"flaggedRuns"`
	// Mean time-to-land = LandedWallMs / LandedWallRuns. Only runs that landed
	// AND carry a metering record contribute; a landed run from a `sig run`
	// invocation has no usage.json and is in neither number.
	LandedWallMs   int64 `json:"landedWallMs"`
	LandedWallRuns int   `json:"landedWallRuns"`
	// Agent wall-time per landed change = AgentWallMs / AgentWallLanded, over
	// runs whose metering record carries an agent wall time at all (written from
	// v2.1 on — an older run contributes to neither number, so the mean is not
	// silently diluted by zeros).
	AgentWallMs     int64 `json:"agentWallMs"`
	AgentWallLanded int   `json:"agentWallLanded"`
	// Cost per landed change = CostUSD / CostLanded, over runs whose agents
	// actually wrote a SIGBOUND_USAGE_FILE. CostRuns is how many runs reported
	// anything at all; all three stay 0 when nobody uses the seam, which is the
	// normal case.
	CostUSD      float64 `json:"costUsd"`
	CostRuns     int     `json:"costRuns"`
	CostLanded   int     `json:"costLanded"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	// Presets is landing success per resolved agent command, sorted by preset
	// name. See boardPreset.
	Presets []boardPreset `json:"presets"`
}

type boardResponse struct {
	Cells   []boardCell  `json:"cells"`
	Metrics boardMetrics `json:"metrics"`
}

// handleBoard serves GET /board?limit=N. Read-only, gated by the same bearer
// token as every other data route.
func (s *server) handleBoard(w http.ResponseWriter, r *http.Request) {
	limit := boardDefaultLimit
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "limit must be a non-negative integer", codeBadRequest)
			return
		}
		limit = n
	}
	resp := boardResponse{Cells: make([]boardCell, 0, len(s.cells))}
	presets := map[string]*boardPreset{}
	for _, rc := range s.cells {
		rows, _ := scanRuns(rc.runsDir, limit)
		bc := boardCell{Cell: rc.cell.ID(), Repo: rc.cell.Repo()}
		// The intents directory is read from the WORKING TREE, exactly as
		// `sig intent list` does (an intent is input, not a gate — see
		// intent.go). A directory that will not parse costs the intent cards,
		// never the runs.
		intents, err := listIntents(rc.cell.Repo())
		if err != nil {
			bc.IntentsError = err.Error()
		}
		bc.Intents = buildBoard(intents, rows)
		resp.Cells = append(resp.Cells, bc)
		// ponytail: metrics re-read each run's report.json that scanRuns just
		// read, plus its usage.json — two small reads per run on a local
		// single-user daemon, bounded by the same limit. Fold the two passes
		// together if a big history ever makes it show.
		for _, row := range rows {
			foldMetrics(&resp.Metrics, presets, filepath.Join(rc.runsDir, row.ID))
		}
	}
	resp.Metrics.Presets = make([]boardPreset, 0, len(presets))
	for _, p := range presets {
		resp.Metrics.Presets = append(resp.Metrics.Presets, *p)
	}
	sort.Slice(resp.Metrics.Presets, func(i, j int) bool {
		return resp.Metrics.Presets[i].Preset < resp.Metrics.Presets[j].Preset
	})
	writeJSON(w, http.StatusOK, resp)
}

// buildBoard groups the journal's rows under the intents that produced them.
// Every intent on disk gets a card even with no runs (that IS the open column),
// every run reaches exactly one card, and a run naming an intent whose file is
// gone gets a Missing card rather than disappearing — the journal wins over the
// directory in both directions. rows must be newest-first (scanRuns' order),
// which is what makes each card's Runs newest-first too.
func buildBoard(intents []intent, rows []logRow) []boardIntent {
	byID := map[string][]logRow{}
	for _, row := range rows {
		byID[row.Intent] = append(byID[row.Intent], row)
	}
	out := make([]boardIntent, 0, len(intents)+len(byID))
	for _, it := range intents {
		card := boardIntent{ID: it.ID, Goal: it.Goal, Priority: it.Priority, Issue: it.Issue, Runs: []logRow{}}
		if rows := byID[it.ID]; rows != nil {
			card.Runs = rows
		}
		if it.Schedule > 0 {
			card.Schedule = it.Schedule.String()
		}
		card.Column = boardColumn(card.Runs)
		delete(byID, it.ID)
		out = append(out, card)
	}
	// Whatever is left named an intent with no file on disk (deleted, or never
	// committed to this checkout), plus the "" bucket of runs that came from no
	// intent at all. Both are real work and both stay on the board.
	rest := make([]string, 0, len(byID))
	for id := range byID {
		rest = append(rest, id)
	}
	sort.Strings(rest)
	for _, id := range rest {
		out = append(out, boardIntent{ID: id, Column: boardColumn(byID[id]), Missing: id != "", Runs: byID[id]})
	}
	return out
}

// boardColumn derives one card's column from its runs, newest first. Precedence
// is deliberate rather than "whatever the newest run says": an awaiting-ack park
// is the one state a human owes an answer to, so it outranks a newer run that
// has since landed — a newer landing must not hide a park that is still holding
// verified work. A run in flight outranks a finished one for the same reason in
// reverse: it is the live state.
func boardColumn(rows []logRow) string {
	if len(rows) == 0 {
		return boardOpen
	}
	for _, r := range rows {
		if r.Status == statusAwaitingAck {
			return boardAwaitingAck
		}
	}
	for _, r := range rows {
		if r.Status == "queued" || r.Status == "running" {
			return boardRunning
		}
	}
	// A landed card needs BOTH a terminal "done" and a landed sha. The sha alone
	// is not enough: a run's own report records the commit it computed even when
	// the ref was never advanced, so a REJECTED park — whose report is a green,
	// fully verified landing a human refused — carries a landedSHA while having
	// landed nothing. Rejected, errored and interrupted runs are all attention.
	if rows[0].Status == "done" && rows[0].LandedSHA != "" {
		return boardLanded
	}
	return boardAttention
}

// foldMetrics folds one run directory into the running counters. A run whose
// report cannot be read contributes NOTHING — not a zero — because a run with no
// report has no verdict to average, and counting it as a failure would be an
// invention. The same rule applies per metric: a run without a metering record
// is absent from the wall-clock and cost numbers, not a zero in them.
func foldMetrics(m *boardMetrics, presets map[string]*boardPreset, dir string) {
	rep, err := readRunReport(dir)
	if err != nil {
		return
	}
	m.Runs++
	didLand := landed(rep)
	if didLand {
		m.Landed++
	}
	if rep.Verify.Ran {
		m.VerifyRan++
		if rep.Verify.OK {
			m.VerifyPassed++
		}
	}
	if b := rep.Verify.Bisect; b != nil && b.Ran {
		m.BisectRan++
		if len(b.LandedGroups) > 0 {
			m.BisectSalvaged++
		}
	}
	if len(rep.Integrate.Flagged) > 0 {
		m.FlaggedRuns++
	}
	name := presetOfAgentCmd(rep.AgentCmd)
	p := presets[name]
	if p == nil {
		p = &boardPreset{Preset: name}
		presets[name] = p
	}
	p.Runs++
	if didLand {
		p.Landed++
	}

	u, err := readRunUsage(dir)
	if err != nil {
		return
	}
	// usage.Landed is authoritative over the report heuristic: execRun forces it
	// false for a run that errored mid-flight, where the ref provably never moved
	// (see execRun).
	if u.Landed {
		m.LandedWallMs += u.TotalWallMs
		m.LandedWallRuns++
	}
	if u.AgentWallMs > 0 {
		m.AgentWallMs += u.AgentWallMs
		if u.Landed {
			m.AgentWallLanded++
		}
	}
	if u.CostAgents > 0 {
		m.CostUSD += u.CostUSD
		m.InputTokens += u.InputTokens
		m.OutputTokens += u.OutputTokens
		m.CostRuns++
		if u.Landed {
			m.CostLanded++
		}
	}
}

// presetOfAgentCmd reverse-matches a run's RESOLVED agent command against the
// -agent-preset table. Reports are recorded with the expanded command and never
// the preset name, so an exact match is the only honest claim available; every
// other command — a hand-written one, or a preset someone edited — is "custom".
func presetOfAgentCmd(cmd string) string {
	for name, expanded := range agentPresets {
		if cmd == expanded {
			return name
		}
	}
	return "custom"
}
