// Command sig log is a READ-ONLY query layer over what runs already record —
// the report.json manifests under .git/sigbound/runs and the landing notes
// under refs/notes/sigbound. It adds no storage and changes nothing a run
// writes; it only reads back the run history. Four views:
//
//	sig log -repo P                runs newest-first: id, when, task/agent
//	                               counts, landed/flagged/dropped, verify
//	                               verdict, landed SHA (short)
//	sig log -repo P -sha COMMIT    provenance for one commit: which run landed
//	                               it, from which task, by which agent. Notes
//	                               first (a landing note rides with the commit
//	                               to any clone), then a manifest walk — answers
//	                               correctly for overlay/octopus landings and
//	                               for bisect-salvaged subsets, INCLUDING the
//	                               branches bisect dropped (reported as "dropped
//	                               by bisect", never "unknown"). A commit
//	                               sigbound never landed => exit 1.
//	sig log -repo P -task ID       one task across every run and resume, oldest
//	                               -first.
//	sig log -repo P -release A..B  that commit range as a release document:
//	                               which runs landed it, what each landing's
//	                               acceptance was, what parked, what bisect
//	                               dropped, and what nothing claims. See
//	                               release.go.
//
// -limit bounds the newest-first list (default 50) and the scan is LAZY: run
// dirs are ordered by their timestamp-prefixed id (a descending sort is
// chronological, see newRunID), and only the rendered dirs are read — a large
// history costs one ReadDir plus at most -limit manifest reads. -json emits a
// stable machine shape (documented in docs/USAGE.md, same as the run report).
//
// The reader helpers here (scanRuns, resolveProvenance) are the SAME code sig
// serve's GET /log and GET /log/sha/<sha> routes call, so the HTTP surface and
// the CLI can never drift.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

// runIDTimeLayout is newRunID's timestamp-prefix layout (the first 16 bytes of
// a run id), used to recover a run's wall-clock time from its id when a
// (crashed) run left no readable report to carry StartedAt.
const runIDTimeLayout = "20060102T150405Z"

// logRow is one run in `sig log`'s newest-first list and the stable element of
// its -json array. Every field is projected from the run dir: report.json for a
// completed run, plus status.json/error.json/request.json for status, error
// text and (serve runs only) the original goal. A run whose report is missing
// or unparseable — crashed mid-write — still renders, with Incomplete set and
// whatever the other files carry.
type logRow struct {
	ID        string `json:"id"`
	StartedAt string `json:"startedAt,omitempty"`
	Status    string `json:"status,omitempty"` // queued|running|done|error|interrupted (see diskRunStatus)
	// Goal is the natural-language goal a -goal run was launched with. It is
	// ONLY persisted for serve runs (request.json); a CLI -goal run records
	// just the planned Tasks, so this is usually empty and Tasks is the handle.
	Goal string `json:"goal,omitempty"`
	// Intent is the intents/<id>.intent this run was started from (`sig run
	// -intent`), empty for every other task source — the handle that attributes
	// a landing back to the work the repo asked for. See intent.go.
	Intent string `json:"intent,omitempty"`
	Tasks  int    `json:"tasks"`
	// Agents is len(perAgent). AgentCmd is the RESOLVED agent command this run
	// ran (after any -agent-preset expansion): sigbound records the expanded
	// command, never the preset name, so this is the honest "which agent" — see
	// docs/USAGE.md's Provenance section.
	Agents   int    `json:"agents"`
	AgentCmd string `json:"agentCmd,omitempty"`
	Strategy string `json:"strategy,omitempty"`
	Landed   int    `json:"landed"`  // len(integrate.landed) — the final landed subset on a bisect run
	Flagged  int    `json:"flagged"` // len(integrate.flagged) — real conflicts set aside for a human
	Dropped  int    `json:"dropped"` // len(integrate.droppedByBisect) — clean branches a failing group cost
	Verify   string `json:"verify,omitempty"`
	// LandedSHA is the commit this run put on the base ref, shown ONLY when the
	// ref actually advanced (see landed): finalSHA is populated with the
	// integrated tree even on a verify failure that lands nothing, so a bare
	// finalSHA is not proof of a landing. For a run that parked and was later
	// ACKED it is the ack's commit from park.json — the report predates the ack
	// and cannot carry it (see ackedLandedSHA).
	LandedSHA string `json:"landedSHA,omitempty"`
	// PolicyHash is the sha256 of the sigbound.policy this run resolved, read
	// from where policyReport actually writes it (report.policy.hash — NOT a
	// top-level policyHash key, which nothing has ever written). Empty and
	// omitted for a run against a repo with no policy file.
	PolicyHash string `json:"policyHash,omitempty"`
	// Unlands is the run id this run took back (`sig unland`, issue #149), empty
	// for every ordinary run. Only the FORWARD edge appears in this list: the
	// reverse one (who unlanded a given run) would need every newer manifest read,
	// and the list deliberately reads at most -limit of them. `sig log -sha`
	// reports the reverse edge as unlandedBy, since its walk visits them all.
	Unlands    string `json:"unlands,omitempty"`
	Error      string `json:"error,omitempty"`      // error/interrupted runs: the recorded reason
	Incomplete bool   `json:"incomplete,omitempty"` // report expected but missing/unparseable (crash mid-write)
}

// provenance answers `sig log -sha`: which run landed a commit, from which
// task, by which agent. Source records how it was resolved — "note" (the
// landing note on the commit, portable across clones) or "manifest" (the local
// run ledger). Role classifies the commit:
//
//	landed-commit             the run's integrated commit that advanced the base
//	unland-commit             a landed-commit whose run was an unland (see
//	                          unland.go): it advanced the base by taking another
//	                          run's contribution back, and Unlands names that run
//	ack-landed-commit         a parked landing a HUMAN released with `sig ack`,
//	                          which is the only role a person had to approve
//	member-landed             an agent branch tip that landed as part of a run
//	member-dropped-by-bisect  an agent branch that integrated clean but whose
//	                          group failed -verify, so bisect dropped it (never
//	                          landed) — still fully attributed, not "unknown"
//	member-flagged            an agent branch set aside as a real conflict
//	member                    an agent branch of a run that did not land
type provenance struct {
	SHA    string `json:"sha"`
	Landed bool   `json:"landed"`
	Role   string `json:"role"`
	// RunID is the LOCAL LEDGER's run dir and nothing else: empty whenever only a
	// portable note answered. That emptiness is load-bearing — it is what
	// provenanceLine and release.go's releaseRunLabel render as "(from commit
	// note)" — so no matchProvenance arm may fill it from a note's payload, and
	// resolveProvenance clears it on the note path so none can.
	RunID string `json:"runId,omitempty"`
	// Intent is the intents/<id>.intent the landing run was started from
	// (`sig run -intent`), empty for every other task source. It answers "which
	// intent did this commit come from" — including from a portable landing
	// note, since the note is the whole report.
	Intent    string `json:"intent,omitempty"`
	TaskID    string `json:"taskId,omitempty"`
	Agent     string `json:"agent,omitempty"` // resolved agent command
	Branch    string `json:"branch,omitempty"`
	Strategy  string `json:"strategy,omitempty"`
	Verify    string `json:"verify,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	FinalSHA  string `json:"finalSHA,omitempty"` // the run's landed integration commit
	Members   int    `json:"members,omitempty"`  // branches that landed together (landed-commit / ack-landed-commit only)
	// Unlands is the run id an unland-commit took back (issue #149).
	Unlands string `json:"unlands,omitempty"`
	// UnlandedBy names the unland run that took THIS commit's run back — the
	// reverse edge. Populated only on the manifest-walk path, which visits every
	// run dir and so answers exactly; a portable landing note carries the run's
	// own report and cannot know what a later run did to it, so it is empty there.
	UnlandedBy string `json:"unlandedBy,omitempty"`
	// ApprovedBy is who released this parked landing (`sig ack -by`, issue #175),
	// set only on an ack-landed-commit. It is a CLAIM, never proof: the engine has
	// no user model, the string is whatever the caller supplied, and on a
	// note-sourced answer it arrived with the commit from whatever remote sent it.
	// Source is the field that says which, and every renderer must keep saying so.
	ApprovedBy string `json:"approvedBy,omitempty"`
	Source     string `json:"source"`
}

// roleAckLanded is the provenance role for a landing a human released with `sig
// ack`. Named, unlike its sibling roles, because release.go tests for it too —
// two files agreeing on a literal by hand is the drift this costs one line to
// rule out.
const roleAckLanded = "ack-landed-commit"

// taskRow is one appearance of a task across the run history (`sig log -task`),
// oldest-first — a task re-run under -resume shows once per run it appeared in.
type taskRow struct {
	RunID     string `json:"runId"`
	StartedAt string `json:"startedAt,omitempty"`
	Branch    string `json:"branch,omitempty"`
	SHA       string `json:"sha,omitempty"`
	OK        bool   `json:"ok"`
	Resumed   bool   `json:"resumed,omitempty"`
	Landed    bool   `json:"landed"`
	Verify    string `json:"verify,omitempty"`
}

func runLog(w io.Writer, argv []string) (int, error) {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig log -repo PATH [-limit 50] [-sha COMMIT | -task ID | -release FROM..TO] [-json] [-with-commands]")
		fmt.Fprintln(fs.Output(), "read-only history over .git/sigbound/runs + refs/notes/sigbound; adds no storage, changes nothing runs write.")
		fmt.Fprintln(fs.Output(), "  (no selector): runs newest-first          -sha COMMIT: which run/task/agent landed it (exit 1 if none)")
		fmt.Fprintln(fs.Output(), "  -task ID: that task across runs/resumes   -release FROM..TO: release notes for that commit range")
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "path to the target git repository")
	limit := fs.Int("limit", 50, "max runs in the newest-first list (0 = all); ignored with -sha/-task/-release")
	sha := fs.String("sha", "", "provenance for one commit: which run landed it, from which task, by which agent")
	task := fs.String("task", "", "follow one task id across every run and resume, oldest-first")
	release := fs.String("release", "", "FROM..TO: assemble release notes for that commit range from the ledger and landing notes")
	withCommands := fs.Bool("with-commands", false, "-release: include the resolved commands verbatim; they are the invoker's own and CAN contain secrets")
	asJSON := fs.Bool("json", false, "emit JSON (stable field names, omit-empty; see docs/USAGE.md)")
	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitOK, nil
		}
		return exitOperationalError, err
	}
	if strings.TrimSpace(*repo) == "" {
		return exitOperationalError, errors.New("-repo is required")
	}
	if n := boolCount(*sha != "", *task != "", *release != ""); n > 1 {
		return exitOperationalError, errors.New("-sha, -task and -release are mutually exclusive")
	}

	c, err := cell.Open(*repo)
	if err != nil {
		return exitOperationalError, err
	}
	ctx := context.Background()
	runsDir, err := cellRunsDir(ctx, c)
	if err != nil {
		return exitOperationalError, err
	}

	switch {
	case *sha != "":
		return logSHA(ctx, w, c.Git(), runsDir, *sha, *asJSON)
	case *task != "":
		return logTask(w, runsDir, *task, *asJSON)
	case *release != "":
		return logRelease(ctx, w, c.Git(), runsDir, *release, *asJSON, *withCommands)
	default:
		return logList(w, runsDir, *limit, *asJSON)
	}
}

// boolCount counts the true conditions, for a mutual-exclusion check.
func boolCount(conds ...bool) int {
	n := 0
	for _, c := range conds {
		if c {
			n++
		}
	}
	return n
}

// cellRunsDir is the run-history root for a cell: <git-common-dir>/sigbound/runs
// — the exact path sig serve writes to and sig gc scans (see gc.go's
// loadProtectedBranches). Missing until a run has ever been recorded; scanRuns
// et al. tolerate its absence (an empty history, not an error).
func cellRunsDir(ctx context.Context, c *cell.Cell) (string, error) {
	common, err := c.Git().GitCommonDir(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(common, "sigbound", "runs"), nil
}

// runDirNames returns runsDir's run-id subdirectories sorted DESCENDING, i.e.
// newest-first (run ids are timestamp-prefixed, so a lexical sort is
// chronological — see newRunID). A missing runsDir yields no names, not an
// error. This is the one directory read the whole reader is built on: callers
// take a prefix for laziness (the list) or walk it all (provenance/task).
func runDirNames(runsDir string) []string {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return nil // no runs yet (or unreadable): an empty history
	}
	names := make([]string, 0, len(entries))
	for _, de := range entries {
		if de.IsDir() {
			names = append(names, de.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names
}

// scanRuns renders the newest-first run list, reading AT MOST limit manifests
// (limit <= 0 means all). Because runDirNames already orders dirs newest-first,
// only the dirs actually rendered are ever opened — a 10k-run history serves
// -limit 5 with one ReadDir and five report reads, never a full scan. incomplete
// counts rendered rows whose report was expected but missing/unparseable.
func scanRuns(runsDir string, limit int) (rows []logRow, incomplete int) {
	names := runDirNames(runsDir)
	if limit > 0 && len(names) > limit {
		names = names[:limit]
	}
	rows = make([]logRow, 0, len(names))
	for _, id := range names {
		row, inc := readLogRow(runsDir, id)
		row.Incomplete = inc
		if inc {
			incomplete++
		}
		rows = append(rows, row)
	}
	return rows, incomplete
}

// readLogRow projects one run dir into a logRow. It NEVER crashes on a partial
// dir: a report that is missing or won't parse yields an Incomplete row
// carrying whatever status.json / error.json / the run-id timestamp still
// provide. incomplete is true only
// when a report was expected (a "done" run, or a torn dir with no terminal
// marker at all) but could not be read — a clean error/interrupted run is a
// known outcome, not corruption.
func readLogRow(runsDir, id string) (row logRow, incomplete bool) {
	dir := filepath.Join(runsDir, id)
	row = logRow{ID: id}
	status, note := diskRunStatus(dir)
	row.Status = status

	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err == nil {
		var rep runReport
		if jerr := json.Unmarshal(data, &rep); jerr == nil {
			fillRowFromReport(&row, &rep)
			// The report alone cannot answer "what landed" — it records a commit
			// it never landed when verify went red, and an ACK's commit lands
			// after it was written. landedSHA owns that rule; GET /runs applies
			// the identical one.
			row.LandedSHA = short(landedSHA(&rep, dir))
			row.Goal = goalOf(dir)
			if row.StartedAt == "" {
				row.StartedAt = whenFromID(id)
			}
			return row, false
		}
		// report.json present but unparseable: a crash mid-write.
		incomplete = true
	} else {
		// No report. A clean error/interrupted run is a known state; a dir with
		// no terminal marker at all (diskRunStatus == "interrupted" with a
		// synthesized note, or "done" with a vanished report) is corruption.
		incomplete = status == "done" || status == "interrupted"
	}

	if msg := readRunErrorMsg(dir); msg != "" {
		row.Error = msg
	} else if note != "" {
		row.Error = note
	}
	row.Goal = goalOf(dir)
	if row.StartedAt == "" {
		row.StartedAt = whenFromID(id)
	}
	return row, incomplete
}

// fillRowFromReport copies the rendered fields out of a parsed report — every
// one except LandedSHA, which the report cannot answer on its own (see
// landedSHA) and readLogRow fills from the run dir.
func fillRowFromReport(row *logRow, rep *runReport) {
	row.StartedAt = rep.StartedAt
	row.Intent = rep.Intent
	row.Tasks = len(rep.Tasks)
	row.Agents = len(rep.PerAgent)
	row.AgentCmd = rep.AgentCmd
	row.Strategy = strategyOf(rep)
	row.Landed = len(rep.Integrate.Landed)
	row.Flagged = len(rep.Integrate.Flagged)
	row.Dropped = len(rep.Integrate.DroppedByBisect)
	row.Verify = verifyVerdict(rep.Verify)
	if rep.Policy != nil {
		row.PolicyHash = rep.Policy.Hash
	}
	row.Unlands = rep.Unlands
}

// noteFormatCurrent is the version stamped into every note this binary writes
// (runReport.NoteFormat, set in attachNote). It versions the NOTE PAYLOAD and
// nothing else — it is not tied to the release version and moves on its own
// schedule.
//
// The compatibility rule, also stated in docs/USAGE.md so an outside reader can
// rely on it: ADDING a field does not bump this, and existing fields do not
// change meaning within a version. Anything that would make a reader written
// against version N wrong — removing a documented field, or changing what one
// means — bumps it.
const noteFormatCurrent = 1

// parseNote decodes one note payload, and is the single gate BOTH note readers
// go through (resolveProvenance here, and the release-notes reader in
// release.go) so a version bump is one edit rather than two that can drift.
//
// Three cases, and the middle one is the reason this exists:
//
//   - Unparseable: not a note this binary can use. Not authoritative.
//   - noteFormat ABSENT (zero): a note written before the format carried a
//     version. Its shape is exactly the one this binary already reads, so it
//     stays readable. Existing repositories must not lose their history because
//     a version stamp arrived late.
//   - noteFormat GREATER than this binary knows: written by a newer sigbound,
//     in a shape whose fields may no longer mean what this binary thinks. It is
//     refused rather than guessed at — the caller falls through to the local
//     manifest ledger, which is ground truth. Refusing is deliberately silent
//     and non-fatal: an unreadable note must never crash a provenance query, and
//     must never attribute a landing it cannot actually parse.
//
// This is a statement about SHAPE, never about authenticity. A note is
// user-writable and rides in from untrusted remotes; a version field is a hint
// about how to read the bytes and buys the payload no trust whatsoever. The
// caller still has to prove the note concerns the queried commit
// (matchProvenance), exactly as before.
func parseNote(content string) (*runReport, bool) {
	var rep runReport
	if json.Unmarshal([]byte(content), &rep) != nil {
		return nil, false
	}
	if rep.NoteFormat > noteFormatCurrent {
		return nil, false
	}
	return &rep, true
}

// resolveProvenance answers -sha for one commit against one cell. Notes first:
// a landing note is the whole report keyed by the landed commit and rides with
// that commit to any clone, so this answers even when the local ledger has no
// dir for the run (a commit fetched from where the run actually happened) —
// this is the cheap path (one git call, no manifest scan). Falling through, it
// walks manifests newest-first and stops at the first match. ok is false only
// when NO note and NO manifest claims the commit — a commit sigbound never
// landed.
//
// A note is user-writable and rides across clones from untrusted remotes, so
// its payload is NOT trusted blindly: a note is authoritative only if it
// genuinely concerns the queried commit — the commit is the run's landed
// integration commit or one of its recorded member tips (matchProvenance is
// exactly that test). A forged or unrelated note (one whose finalSHA and members
// are some other commit) is non-authoritative and falls through to the local
// manifest ledger, which is ground truth.
func resolveProvenance(ctx context.Context, g *gitx.Git, runsDir, sha string) (*provenance, bool) {
	if content, ok, _ := g.NoteShow(ctx, "sigbound", sha); ok {
		if rep, ok := parseNote(content); ok {
			if p := matchProvenance(rep, sha); p != nil {
				// A note-sourced answer NAMES NO RUN. RunID is the local ledger's dir
				// name (filled in by the walk below), and its emptiness is the signal
				// every renderer keys "(from commit note)" on. Cleared HERE, once, at
				// the single point where a note's payload becomes an answer, rather
				// than trusted to each arm: an arm that promoted the payload's own
				// runId would let a forged note both borrow a real local run's
				// identity and pass itself off as the ledger's own record.
				p.RunID, p.Source = "", "note"
				return p, true
			}
			// The note parses but is not about THIS commit — treat it as
			// non-authoritative and fall through to the manifest walk.
		}
	}
	// The reverse unland edge is collected AS the walk goes (issue #149) rather
	// than by a second pass: run ids are timestamp-prefixed and this walk is
	// newest-first, so an unland run is always visited before the run it reverses.
	// Only a LANDED unland counts — one that was blocked or parked took nothing
	// back — and the edge costs nothing, since the walk already reads every
	// manifest it passes.
	unlandedBy := map[string]string{}
	for _, id := range runDirNames(runsDir) {
		dir := filepath.Join(runsDir, id)
		rep, err := readRunReport(dir)
		if err != nil {
			continue // unreadable/partial dir: never crash a provenance query, just skip it
		}
		if rep.Unlands != "" && landed(rep) && unlandedBy[rep.Unlands] == "" {
			unlandedBy[rep.Unlands] = id
		}
		// An ACK lands after this report was written and records the commit in
		// park.json, which is never folded back into the report (see
		// ackedLandedSHA), so the ledger can only answer for an acked landing if we
		// go and look — the same OR readLogRow applies, DERIVED on read rather than
		// back-dated into the run's own historical record. Only for a run that
		// actually parked, so an ordinary run costs no extra file reads.
		if rep.Park != nil && rep.Park.LandedSHA == "" {
			rep.Park.LandedSHA = ackedLandedSHA(dir)
		}
		if p := matchProvenance(rep, sha); p != nil {
			p.RunID = id
			p.Source = "manifest"
			p.UnlandedBy = unlandedBy[id]
			return p, true
		}
	}
	return nil, false
}

// matchProvenance tests one report for the commit and returns its provenance,
// or nil. It matches the run's landed integration commit (integrate.finalSHA),
// the commit an ack released (park.landedSHA, see the arm below) and every agent
// branch tip (perAgent[].sha) — which together cover overlay landings (finalSHA
// is a fresh combine commit; members are the branch tips), octopus/merge
// landings (finalSHA a merge, members its parents) and bisect-salvaged runs
// (some members landed, others dropped). A full 40/64-hex sha matches exactly; a
// shorter arg matches by prefix.
func matchProvenance(rep *runReport, sha string) *provenance {
	match := shaMatcher(sha)
	if match(rep.Integrate.FinalSHA) && landed(rep) {
		return provenanceFromFinal(rep, rep.Integrate.FinalSHA)
	}
	// An ACKED landing (issue #160) matches on the park record's landedSHA and
	// nothing else. The report itself can never claim this commit: it was written
	// before the ack existed, so integrate.finalSHA is whatever the RUN landed on
	// its own — the base commit for a run that parked its only group, a different
	// commit for a mixed run whose clean group landed — and the acked commit
	// reached the base ref later, by a human's `sig ack` (see ackedLandedSHA). A
	// resolved landedSHA is therefore a landing this report knows about, but only
	// for THAT commit.
	//
	// The spoofing bar, precisely. MATCHING is exactly the finalSHA arm's: a note
	// lifted onto an unrelated commit carries somebody else's landedSHA, does not
	// match, and falls through to the ledger. What a match BUYS is also exactly
	// the other arms': the answer names no run, because resolveProvenance clears
	// RunID on the note path — so a note cannot dress its claim in a real local
	// run's identity, and it renders with the same "(from commit note)" marker as
	// every other note-sourced answer. Nothing here is trusted that the finalSHA
	// arm does not already trust.
	if rep.Park != nil && match(rep.Park.LandedSHA) {
		return &provenance{
			SHA:       rep.Park.LandedSHA,
			Landed:    true,
			Role:      roleAckLanded,
			Intent:    rep.Intent,
			Agent:     rep.AgentCmd,
			Strategy:  strategyOf(rep),
			Verify:    verifyVerdict(rep.Verify),
			StartedAt: rep.StartedAt,
			FinalSHA:  rep.Park.LandedSHA,
			Members:   len(rep.Park.branches()),
			// Who signed off, if the ack recorded anyone. This is the field that
			// makes "who approved this, six months later" answerable from a clone
			// that has only the note -- the property parking exists for.
			ApprovedBy: rep.Park.ApprovedBy,
		}
	}
	for _, a := range rep.PerAgent {
		if !match(a.SHA) {
			continue
		}
		p := &provenance{
			SHA:       a.SHA,
			Intent:    rep.Intent,
			TaskID:    a.ID,
			Agent:     rep.AgentCmd,
			Branch:    a.Branch,
			Strategy:  strategyOf(rep),
			Verify:    verifyVerdict(rep.Verify),
			StartedAt: rep.StartedAt,
			FinalSHA:  rep.Integrate.FinalSHA,
		}
		switch {
		case hasString(rep.Integrate.DroppedByBisect, a.Branch):
			p.Role, p.Landed = "member-dropped-by-bisect", false
		case flaggedHas(rep.Integrate.Flagged, a.Branch):
			p.Role, p.Landed = "member-flagged", false
		case hasString(rep.Integrate.Landed, a.Branch) && landed(rep):
			p.Role, p.Landed = "member-landed", true
		default:
			p.Role, p.Landed = "member", false
		}
		return p
	}
	return nil
}

// provenanceFromFinal builds the provenance for a run's landed integration
// commit — the aggregate of every landed branch, so it names no single task. An
// unland's landing gets its own role: it advanced the base by REMOVING another
// run's contribution, which "landed-commit" alone would not say.
func provenanceFromFinal(rep *runReport, sha string) *provenance {
	p := &provenance{
		SHA:       sha,
		Landed:    landed(rep),
		Role:      "landed-commit",
		Intent:    rep.Intent,
		Agent:     rep.AgentCmd,
		Strategy:  strategyOf(rep),
		Verify:    verifyVerdict(rep.Verify),
		StartedAt: rep.StartedAt,
		FinalSHA:  rep.Integrate.FinalSHA,
		Members:   len(rep.Integrate.Landed),
	}
	if rep.Unlands != "" {
		p.Role, p.Unlands = "unland-commit", rep.Unlands
	}
	return p
}

// scanTask follows one task id across the whole history, oldest-first (the
// order it was worked and re-worked in). A task appears once per run whose
// perAgent set names it — including every -resume that re-ran or reused it.
func scanTask(runsDir, taskID string) []taskRow {
	names := runDirNames(runsDir)
	rows := make([]taskRow, 0)
	for i := len(names) - 1; i >= 0; i-- { // reverse of newest-first => oldest-first
		id := names[i]
		rep, err := readRunReport(filepath.Join(runsDir, id))
		if err != nil {
			continue
		}
		for _, a := range rep.PerAgent {
			if a.ID != taskID {
				continue
			}
			rows = append(rows, taskRow{
				RunID:     id,
				StartedAt: rep.StartedAt,
				Branch:    a.Branch,
				SHA:       a.SHA,
				OK:        a.OK,
				Resumed:   a.Resumed,
				Landed:    hasString(rep.Integrate.Landed, a.Branch) && landed(rep),
				Verify:    verifyVerdict(rep.Verify),
			})
		}
	}
	return rows
}

// ---- shared small helpers ----

// landed mirrors computeUsage's Landed rule (usage.go): finalSHA is populated
// with the integrated tree even when -verify fails and NOTHING is written to
// the base ref, so an actual landing needs both a moved ref (finalSHA !=
// baseSHA) and a green-or-unset verify.
func landed(rep *runReport) bool {
	return rep.Integrate.FinalSHA != "" &&
		rep.Integrate.FinalSHA != rep.BaseSHA &&
		(!rep.Verify.Ran || rep.Verify.OK)
}

// landedSHA is the commit this run put on the base ref, or "" if none did — the
// ONE derivation every surface that shows a landed commit reads (`sig log`'s
// rows, GET /log, GET /runs). The run's own landing wins when its report proves
// one; otherwise it is the commit an ACK landed afterwards, which the report
// predates and cannot carry (see ackedLandedSHA).
//
// It lives here because deriving it twice is exactly how the surfaces drifted:
// GET /runs used to read integrate.finalSHA raw, so it showed a SHA for a run
// whose -verify went red and never moved the ref, and showed nothing for an
// acked park that really did land (issue #161). For the same reason no caller
// may put a condition in FRONT of it, least of all a status gate: a run that
// lands its clean groups and only then parks a held one (issue #109) sits at
// awaiting-ack with the base ref already moved, so "ask this only for a done
// run" is a second, stronger copy of the rule that drops a real landing.
//
// The limit: it answers from the run's OWN report, so a dir whose report.json is
// missing or torn reports no landing. That is the same conservative answer
// readLogRow's Incomplete row gives — "nothing recorded here", not proof that
// nothing landed.
func landedSHA(rep *runReport, dir string) string {
	if landed(rep) {
		return rep.Integrate.FinalSHA
	}
	return ackedLandedSHA(dir)
}

// verifyVerdict collapses the verify record to a one-word verdict for a row:
// "pass" (green, first try or after repair), "fail" (never went green), or
// "none" (-verify wasn't configured).
func verifyVerdict(v verifyJSON) string {
	switch {
	case !v.Ran:
		return "none"
	case v.OK:
		return "pass"
	default:
		return "fail"
	}
}

// strategyOf prefers the strategy integrate actually applied, falling back to
// the run's configured strategy for a report whose integrate never ran.
func strategyOf(rep *runReport) string {
	if rep.Integrate.Strategy != "" {
		return rep.Integrate.Strategy
	}
	return rep.Strategy
}

// whenFromID recovers a run's wall-clock start from its timestamp-prefixed id,
// the fallback when a crashed run left no report to carry StartedAt. Returns ""
// for an id that doesn't carry the expected prefix.
func whenFromID(id string) string {
	if len(id) < len(runIDTimeLayout) {
		return ""
	}
	t, err := time.Parse(runIDTimeLayout, id[:len(runIDTimeLayout)])
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// goalOf reads the original goal from a serve run's request journal
// (request.json). Best-effort: CLI runs write no request.json, and a -tasks run
// has no goal — both simply yield "".
func goalOf(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		return ""
	}
	var v struct {
		Goal string `json:"goal"`
	}
	_ = json.Unmarshal(data, &v)
	return strings.TrimSpace(v.Goal)
}

// shaMatcher returns an equality test for a target commit arg: exact for a full
// 40 (sha1) or 64 (sha256) hex sha, prefix otherwise, so a short sha works.
func shaMatcher(sha string) func(string) bool {
	full := len(sha) == 40 || len(sha) == 64
	return func(candidate string) bool {
		if candidate == "" {
			return false
		}
		if full {
			return candidate == sha
		}
		return strings.HasPrefix(candidate, sha)
	}
}

// validCommitArg bounds a -sha / GET /log/sha argument to a plausible hex
// object name (4..64 hex chars) before it is handed to git or matched — a cheap
// guard, not a claim the commit exists.
func validCommitArg(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func hasString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func flaggedHas(flagged []flaggedJSON, branch string) bool {
	for _, f := range flagged {
		if f.Branch == branch {
			return true
		}
	}
	return false
}

// ---- rendering ----

func logList(w io.Writer, runsDir string, limit int, asJSON bool) (int, error) {
	rows, incomplete := scanRuns(runsDir, limit)
	if asJSON {
		return exitOK, writeJSONIndent(w, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "no runs recorded")
		return exitOK, nil
	}
	fmt.Fprintf(w, "%-28s  %-20s  %-11s  %5s %6s  %3s %3s %3s  %-6s  %s\n",
		"RUN", "STARTED", "STATUS", "TASKS", "AGENTS", "LND", "FLG", "DRP", "VERIFY", "LANDED")
	for _, r := range rows {
		st := r.Status
		if r.Incomplete {
			st += "*"
		}
		fmt.Fprintf(w, "%-28s  %-20s  %-11s  %5d %6d  %3d %3d %3d  %-6s  %s\n",
			r.ID, r.StartedAt, st, r.Tasks, r.Agents, r.Landed, r.Flagged, r.Dropped, r.Verify, r.LandedSHA)
	}
	fmt.Fprintf(w, "\n%d run(s)", len(rows))
	if incomplete > 0 {
		fmt.Fprintf(w, ", %d incomplete (marked *)", incomplete)
	}
	fmt.Fprintln(w)
	return exitOK, nil
}

func logSHA(ctx context.Context, w io.Writer, g *gitx.Git, runsDir, sha string, asJSON bool) (int, error) {
	if !validCommitArg(sha) {
		return exitOperationalError, fmt.Errorf("invalid commit %q: expected 4..64 hex characters", sha)
	}
	p, ok := resolveProvenance(ctx, g, runsDir, sha)
	if !ok {
		if asJSON {
			// exit 1 is the signal; still emit a parseable negative for tooling.
			if err := writeJSONIndent(w, map[string]any{"sha": sha, "landed": false, "role": "not-landed"}); err != nil {
				return exitOperationalError, err
			}
		} else {
			fmt.Fprintf(w, "commit %s: not landed by sigbound\n", sha)
		}
		return exitOperationalError, nil
	}
	if asJSON {
		return exitOK, writeJSONIndent(w, p)
	}
	fmt.Fprintln(w, provenanceLine(p))
	return exitOK, nil
}

// provenanceLine is the one-line human rendering of a provenance result. The
// intent clause is appended only when the run recorded one, so a commit landed
// by a -tasks/-goal run reads exactly as it did before intents existed.
func provenanceLine(p *provenance) string {
	// Where the answer came from, said out loud. A note is user-writable and
	// rides in on a fetch from wherever the commit came from, so a note-sourced
	// line must never read like the ledger's own record. Keyed on Source, the
	// field that actually records that (resolveProvenance sets it) — the string
	// is unchanged, since a note-sourced answer carries no run id either, but the
	// marker no longer hangs on one arm remembering to leave RunID empty.
	run := "run " + p.RunID
	if p.Source == "note" {
		run = "(from commit note, started " + p.StartedAt + ")"
	}
	if p.Intent != "" {
		run += " (intent " + p.Intent + ")"
	}
	unlanded := ""
	if p.UnlandedBy != "" {
		unlanded = ", since unlanded by run " + p.UnlandedBy
	}
	switch p.Role {
	case "unland-commit":
		// An unland commit can itself be unlanded (you take back a take-back), and
		// that reverse edge is most informative exactly here. The no-op-unland case
		// leaves UnlandedBy unset upstream, so this stays silent for it.
		return fmt.Sprintf("commit %s: %s took run %s back out of the base, verify %s%s",
			short(p.SHA), run, p.Unlands, p.Verify, unlanded)
	case "landed-commit":
		return fmt.Sprintf("commit %s: landed integration commit of %s (%s, %d branch(es)), verify %s%s",
			short(p.SHA), run, p.Strategy, p.Members, p.Verify, unlanded)
	case roleAckLanded:
		// An acked landing is a landing, so it carries the same reverse edge: a
		// later unland can take it back exactly like any other.
		// The approver is rendered as a RECORDED claim, never as a verified
		// identity: sigbound has no user model and never checked it. On a
		// note-sourced answer `run` already reads "(from commit note...)", which is
		// what marks the whole line -- including this -- as the note's word.
		by := ""
		if p.ApprovedBy != "" {
			by = ", recorded as approved by " + p.ApprovedBy
		}
		return fmt.Sprintf("commit %s: released by a human ACK of %s (%s, %d parked branch(es)), verify %s%s%s",
			short(p.SHA), run, p.Strategy, p.Members, p.Verify, by, unlanded)
	case "member-landed":
		return fmt.Sprintf("commit %s: landed by %s, task %s, agent %s (%s), verify %s",
			short(p.SHA), run, p.TaskID, quote(p.Agent), p.Strategy, p.Verify)
	case "member-dropped-by-bisect":
		return fmt.Sprintf("commit %s: dropped by bisect in %s, task %s, agent %s — its group failed -verify; never landed",
			short(p.SHA), run, p.TaskID, quote(p.Agent))
	case "member-flagged":
		return fmt.Sprintf("commit %s: flagged as a conflict in %s, task %s, agent %s — set aside for a human; never landed",
			short(p.SHA), run, p.TaskID, quote(p.Agent))
	default:
		return fmt.Sprintf("commit %s: ran in %s as task %s, agent %s, but that run did not land",
			short(p.SHA), run, p.TaskID, quote(p.Agent))
	}
}

func logTask(w io.Writer, runsDir, taskID string, asJSON bool) (int, error) {
	rows := scanTask(runsDir, taskID)
	if asJSON {
		return exitOK, writeJSONIndent(w, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintf(w, "task %s: not found in any recorded run\n", taskID)
		return exitOK, nil
	}
	fmt.Fprintf(w, "task %s across %d run(s):\n", taskID, len(rows))
	for _, r := range rows {
		flags := ""
		if r.Resumed {
			flags += " resumed"
		}
		if r.Landed {
			flags += " landed"
		} else {
			flags += " not-landed"
		}
		fmt.Fprintf(w, "  %-28s  %-20s  %-16s  sha %-9s  ok=%v  verify=%s%s\n",
			r.RunID, r.StartedAt, r.Branch, short(r.SHA), r.OK, r.Verify, flags)
	}
	return exitOK, nil
}

func quote(s string) string {
	if s == "" {
		return `""`
	}
	return `"` + s + `"`
}

// writeJSONIndent emits v as 2-space-indented JSON with a trailing newline —
// the same shape sig run's -json and the serve routes produce.
func writeJSONIndent(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
