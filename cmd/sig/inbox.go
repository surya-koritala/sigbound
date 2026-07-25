// GET /inbox (issue #109): the one list of everything across every registered
// cell that is waiting on a human — parked landings needing an ack, flagged
// conflicts, groups -verify-bisect dropped, runs the repair loop could not fix,
// and non-blocking spot-audit samples. It is a READ over data the runs already
// wrote (park.json, report.json, status.json); it computes no new verdicts and
// holds nothing itself, so a run's inbox entry can never disagree with the run.
//
// Exactly one entry type is actionable — `parked`, via POST /runs/{id}/ack and
// /reject. Every other type is there to be looked at.
package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// Inbox entry types. Everything an operator might need to act on has exactly one
// of these; a filter (?type=) matches one of them exactly.
const (
	inboxParked       = "parked"
	inboxFlagged      = "flagged"
	inboxDropped      = "dropped"
	inboxRepairFailed = "repair-failed"
	inboxAudit        = "audit"
	// inboxRedBranch: a watch cycle EXCLUDED these branches after -watch-max-red
	// consecutive cycles failed to land them (issue #111). An attention item, not
	// a landing awaiting release — there is nothing to ack. Re-pushing the branch
	// clears it; the entry stays as the record that it happened.
	inboxRedBranch = "red-branch"
)

// inboxDefaultLimit bounds GET /inbox when the caller names no ?limit. Audit
// entries in particular accumulate with every sampled landing and are expected
// to fall off the end rather than be cleared — there is no audit state machine
// to close.
const inboxDefaultLimit = 100

// inboxEntry is one row. Summary is human wording (it may change between
// releases); Type is the stable discriminant to switch on. Links are paths on
// this same daemon, so the review UI (and curl) can go straight from a row to
// the data behind it. The parked-only fields carry what an operator needs before
// deciding: WHICH paths triggered the hold and under which glob, how many
// re-verify attempts have already happened, and when the park expires.
type inboxEntry struct {
	Type    string            `json:"type"`
	CellID  string            `json:"cellId"`
	RunID   string            `json:"runId"`
	Age     string            `json:"age"`
	Summary string            `json:"summary"`
	Links   map[string]string `json:"links,omitempty"`

	Reason       string            `json:"reason,omitempty"`
	MatchedPaths map[string]string `json:"matchedPaths,omitempty"`
	Attempts     int               `json:"attempts,omitempty"`
	ExpiresAt    string            `json:"expiresAt,omitempty"`
	Branches     []string          `json:"branches,omitempty"`
}

// handleInbox serves GET /inbox?type=&limit=. Newest run first (run ids are
// timestamp-prefixed, so a descending id sort is chronological — the same
// ordering GET /runs uses).
func (s *server) handleInbox(w http.ResponseWriter, r *http.Request) {
	limit := inboxDefaultLimit
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "limit must be a non-negative integer", codeBadRequest)
			return
		}
		limit = n
	}
	want := strings.TrimSpace(r.URL.Query().Get("type"))
	switch want {
	case "", inboxParked, inboxFlagged, inboxDropped, inboxRepairFailed, inboxAudit, inboxRedBranch:
	default:
		writeErr(w, http.StatusBadRequest, "unknown type "+strconv.Quote(want)+
			"; want parked|flagged|dropped|repair-failed|audit|red-branch", codeBadRequest)
		return
	}

	var entries []inboxEntry
	now := time.Now()
	for _, rc := range s.cells {
		names, _ := os.ReadDir(rc.runsDir) // missing dir => no runs yet
		// Newest first, and stop scanning once we have enough of them: a cell's
		// run history only grows, and the tail of it is the least interesting
		// part of an inbox.
		ids := make([]string, 0, len(names))
		for _, de := range names {
			if de.IsDir() {
				ids = append(ids, de.Name())
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(ids)))
		// ponytail: every cell is scanned in full and the merged list is truncated
		// AFTER sorting. Truncating per cell (on a counter shared across cells, as
		// this once did) makes "newest first" a lie the moment there is more than
		// one cell — the first cell fills the quota with its oldest runs and every
		// later cell starves, however recent its parked landings are. Same full
		// scan GET /runs already does; add a per-cell index if it ever matters.
		for _, id := range ids {
			entries = append(entries, inboxEntriesFor(r.Context(), rc.cell.Git(), rc.cell.ID(), filepath.Join(rc.runsDir, id), id, want, now)...)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].RunID > entries[j].RunID })
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	if entries == nil {
		entries = []inboxEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// inboxEntriesFor builds every entry one run dir contributes, filtered to want
// ("" = all). It enforces the park ack-timeout on the way past (the lazy sweep —
// see enforceParkTimeout), so simply reading the inbox is enough to make an
// expired park's auto-rejection true.
func inboxEntriesFor(ctx context.Context, g *gitx.Git, cellID, dir, runID, want string, now time.Time) []inboxEntry {
	enforceParkTimeout(ctx, g, dir)
	status, _ := diskRunStatus(dir)
	rep, _ := readRunReport(dir)
	started := runStartedAt(runID, rep)
	var out []inboxEntry
	add := func(e inboxEntry) {
		if want != "" && want != e.Type {
			return
		}
		e.CellID, e.RunID = cellID, runID
		if e.Age == "" {
			e.Age = ageString(now, started)
		}
		out = append(out, e)
	}
	runLink := "/runs/" + runID

	// parked: the only actionable entry type.
	parkedBranches := map[string]bool{}
	if status == statusAwaitingAck {
		if pk, err := readPark(dir); err == nil {
			for _, b := range pk.branches() {
				parkedBranches[b] = true
			}
			e := inboxEntry{
				Type:         inboxParked,
				Age:          ageString(now, parseTimeOr(pk.CreatedAt, started)),
				Summary:      parkSummary(pk),
				Reason:       pk.Reason,
				MatchedPaths: pk.matchedPaths(),
				Attempts:     len(pk.Attempts),
				Branches:     pk.branches(),
				Links: map[string]string{
					"run":     runLink,
					"flagged": runLink + "/flagged",
					"ack":     runLink + "/ack",
					"reject":  runLink + "/reject",
				},
			}
			if deadline, ok := pk.deadline(); ok {
				e.ExpiresAt = deadline.UTC().Format(time.RFC3339)
			}
			add(e)
		} else {
			// A park that will not read back is still something a human must look
			// at — surfacing it as unactionable is strictly better than dropping
			// the run out of the inbox entirely.
			add(inboxEntry{
				Type:    inboxParked,
				Summary: "parked, but its parking record is unreadable: " + err.Error(),
				Links:   map[string]string{"run": runLink},
			})
		}
	}
	// red-branch: this cycle's backoff exclusions (issue #111). Read before the
	// report is required below — a cycle whose run ERRORED still excludes the
	// branches it took, and that is exactly when a human most needs telling.
	if red := readRedBranches(dir); len(red) > 0 {
		names := make([]string, 0, len(red))
		for _, rb := range red {
			names = append(names, rb.Branch)
		}
		add(inboxEntry{
			Type:     inboxRedBranch,
			Summary:  plural(len(red), "branch", "branches") + " excluded from watch cycles after " + plural(red[0].Cycles, "consecutive cycle", "consecutive cycles") + " that did not land it; re-push to re-qualify",
			Branches: names,
			Links:    map[string]string{"run": runLink, "events": runLink + "/events"},
		})
	}
	if rep == nil {
		return out
	}

	// flagged: real conflicts, plus any policy hold that did NOT end up parked
	// (a held group whose own tree failed verify). A branch that IS parked is
	// deliberately not listed twice.
	var conflicts []string
	for _, f := range rep.Integrate.Flagged {
		if !parkedBranches[f.Branch] {
			conflicts = append(conflicts, f.Branch)
		}
	}
	if len(conflicts) > 0 {
		add(inboxEntry{
			Type:     inboxFlagged,
			Summary:  plural(len(conflicts), "branch", "branches") + " flagged, not landed",
			Branches: conflicts,
			Links:    map[string]string{"run": runLink, "flagged": runLink + "/flagged"},
		})
	}
	if n := len(rep.Integrate.DroppedByBisect); n > 0 {
		add(inboxEntry{
			Type:     inboxDropped,
			Summary:  plural(n, "branch", "branches") + " dropped by -verify-bisect to salvage a green subset",
			Branches: rep.Integrate.DroppedByBisect,
			Links:    map[string]string{"run": runLink},
		})
	}
	if rep.Verify.Ran && !rep.Verify.OK && len(rep.Verify.Repairs) > 0 {
		add(inboxEntry{
			Type:    inboxRepairFailed,
			Summary: "verify still red after " + plural(len(rep.Verify.Repairs), "repair attempt", "repair attempts"),
			Links:   map[string]string{"run": runLink, "events": runLink + "/events"},
		})
	}
	if rep.Audit {
		e := inboxEntry{
			Type:    inboxAudit,
			Summary: "clean landing " + short(rep.Integrate.FinalSHA) + " sampled for spot audit",
			Links:   map[string]string{"run": runLink},
		}
		if rep.Integrate.FinalSHA != "" {
			e.Links["provenance"] = "/log/sha/" + rep.Integrate.FinalSHA
		}
		add(e)
	}
	return out
}

// parkSummary is a parked entry's one-line human wording: why it parked, what it
// would land, and whether a re-verify has already failed on it.
func parkSummary(pk *parkJSON) string {
	what := "touches an ack-path"
	if pk.Reason == parkReasonPolicyModified {
		what = "modifies " + policyFileName
	}
	s := "verified landing " + short(pk.VerifiedSHA) + " awaiting ack: " + what
	// Attempt 1 is the park's own verify (always green, or it would not have
	// parked); only a RE-verify, i.e. an ack that found the base moved, is news.
	if n := len(pk.Attempts); n > 1 {
		last := pk.Attempts[n-1]
		s += "; " + plural(n-1, "re-verify attempt", "re-verify attempts") + ", last " + verdictOf(last.VerifyOK)
	}
	return s
}

// runStartedAt dates a run: its report's recorded start when there is one, else
// the timestamp prefix every run id carries (see newRunID). Both can be absent
// on a hand-made dir, in which case the age simply reads as unknown.
func runStartedAt(runID string, rep *runReport) time.Time {
	if rep != nil {
		if t, err := time.Parse(time.RFC3339, rep.StartedAt); err == nil {
			return t
		}
	}
	if i := strings.IndexByte(runID, '-'); i > 0 {
		if t, err := time.Parse("20060102T150405Z", runID[:i]); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseTimeOr parses an RFC3339 stamp, falling back to def.
func parseTimeOr(s string, def time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return def
}

// ageString renders how long ago t was, to the second. An unknown or
// in-the-future timestamp reads as "unknown" rather than a negative duration.
func ageString(now, t time.Time) string {
	if t.IsZero() || t.After(now) {
		return "unknown"
	}
	return now.Sub(t).Truncate(time.Second).String()
}

// plural renders "1 branch" / "3 branches".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
