// Command sig log is a READ-ONLY query layer over what runs already record —
// the report.json manifests under .git/sigbound/runs and the landing notes
// under refs/notes/sigbound. It adds no storage and changes nothing a run
// writes; it only reads back the run history. Three views:
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
	Goal  string `json:"goal,omitempty"`
	Tasks int    `json:"tasks"`
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
	// LandedSHA is integrate.finalSHA, shown ONLY when the run actually
	// advanced the base ref (see landed): finalSHA is populated with the
	// integrated tree even on a verify failure that lands nothing, so a bare
	// finalSHA is not proof of a landing.
	LandedSHA string `json:"landedSHA,omitempty"`
	// PolicyHash is surfaced only if a run recorded one (issue #108). No run
	// written so far has — runReport has no such field yet — so this reads back
	// empty and is omitted; the reader tolerates its absence and never depends
	// on #108.
	PolicyHash string `json:"policyHash,omitempty"`
	Error      string `json:"error,omitempty"`      // error/interrupted runs: the recorded reason
	Incomplete bool   `json:"incomplete,omitempty"` // report expected but missing/unparseable (crash mid-write)
}

// provenance answers `sig log -sha`: which run landed a commit, from which
// task, by which agent. Source records how it was resolved — "note" (the
// landing note on the commit, portable across clones) or "manifest" (the local
// run ledger). Role classifies the commit:
//
//	landed-commit             the run's integrated commit that advanced the base
//	member-landed             an agent branch tip that landed as part of a run
//	member-dropped-by-bisect  an agent branch that integrated clean but whose
//	                          group failed -verify, so bisect dropped it (never
//	                          landed) — still fully attributed, not "unknown"
//	member-flagged            an agent branch set aside as a real conflict
//	member                    an agent branch of a run that did not land
type provenance struct {
	SHA       string `json:"sha"`
	Landed    bool   `json:"landed"`
	Role      string `json:"role"`
	RunID     string `json:"runId,omitempty"` // empty when only a portable note answered
	TaskID    string `json:"taskId,omitempty"`
	Agent     string `json:"agent,omitempty"` // resolved agent command
	Branch    string `json:"branch,omitempty"`
	Strategy  string `json:"strategy,omitempty"`
	Verify    string `json:"verify,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	FinalSHA  string `json:"finalSHA,omitempty"` // the run's landed integration commit
	Members   int    `json:"members,omitempty"`  // branches that landed together (landed-commit only)
	Source    string `json:"source"`
}

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
		fmt.Fprintln(fs.Output(), "usage: sig log -repo PATH [-limit 50] [-sha COMMIT | -task ID] [-json]")
		fmt.Fprintln(fs.Output(), "read-only history over .git/sigbound/runs + refs/notes/sigbound; adds no storage, changes nothing runs write.")
		fmt.Fprintln(fs.Output(), "  (no -sha/-task): runs newest-first        -sha COMMIT: which run/task/agent landed it (exit 1 if none)")
		fmt.Fprintln(fs.Output(), "  -task ID: that task across runs/resumes")
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "path to the target git repository")
	limit := fs.Int("limit", 50, "max runs in the newest-first list (0 = all); ignored with -sha/-task")
	sha := fs.String("sha", "", "provenance for one commit: which run landed it, from which task, by which agent")
	task := fs.String("task", "", "follow one task id across every run and resume, oldest-first")
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
	if *sha != "" && *task != "" {
		return exitOperationalError, errors.New("-sha and -task are mutually exclusive")
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
	default:
		return logList(w, runsDir, *limit, *asJSON)
	}
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

// readLogRow projects one run dir into a logRow. It reads report.json exactly
// once (so a tolerant second decode can pick up a future policyHash without a
// second file read), and NEVER crashes on a partial dir: a report that is
// missing or won't parse yields an Incomplete row carrying whatever status.json
// / error.json / the run-id timestamp still provide. incomplete is true only
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
			// Tolerant second decode: a policyHash written by a future run
			// (issue #108) surfaces here; runReport has no such field today, so
			// this is empty for every run written so far.
			var extra struct {
				PolicyHash string `json:"policyHash"`
			}
			_ = json.Unmarshal(data, &extra)
			row.PolicyHash = extra.PolicyHash
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

// fillRowFromReport copies the rendered fields out of a parsed report.
func fillRowFromReport(row *logRow, rep *runReport) {
	row.StartedAt = rep.StartedAt
	row.Tasks = len(rep.Tasks)
	row.Agents = len(rep.PerAgent)
	row.AgentCmd = rep.AgentCmd
	row.Strategy = strategyOf(rep)
	row.Landed = len(rep.Integrate.Landed)
	row.Flagged = len(rep.Integrate.Flagged)
	row.Dropped = len(rep.Integrate.DroppedByBisect)
	row.Verify = verifyVerdict(rep.Verify)
	if landed(rep) {
		row.LandedSHA = short(rep.Integrate.FinalSHA)
	}
}

// resolveProvenance answers -sha for one commit against one cell. Notes first:
// a landing note is the whole report keyed by the landed commit and rides with
// that commit to any clone, so this answers even when the local ledger has no
// dir for the run (a commit fetched from where the run actually happened) —
// this is the cheap path (one git call, no manifest scan). Falling through, it
// walks manifests newest-first and stops at the first match. ok is false only
// when NO note and NO manifest claims the commit — a commit sigbound never
// landed.
func resolveProvenance(ctx context.Context, g *gitx.Git, runsDir, sha string) (*provenance, bool) {
	if content, ok, _ := g.NoteShow(ctx, "sigbound", sha); ok {
		var rep runReport
		if json.Unmarshal([]byte(content), &rep) == nil {
			p := provenanceFromFinal(&rep, sha)
			p.Source = "note" // RunID intentionally left empty: the note is portable, the run dir may not be local
			return p, true
		}
		// A note that won't parse as a report is not sigbound's — fall through.
	}
	for _, id := range runDirNames(runsDir) {
		rep, err := readRunReport(filepath.Join(runsDir, id))
		if err != nil {
			continue // unreadable/partial dir: never crash a provenance query, just skip it
		}
		if p := matchProvenance(rep, sha); p != nil {
			p.RunID = id
			p.Source = "manifest"
			return p, true
		}
	}
	return nil, false
}

// matchProvenance tests one report for the commit and returns its provenance,
// or nil. It matches the run's landed integration commit (integrate.finalSHA)
// and every agent branch tip (perAgent[].sha) — which together cover overlay
// landings (finalSHA is a fresh combine commit; members are the branch tips),
// octopus/merge landings (finalSHA a merge, members its parents) and
// bisect-salvaged runs (some members landed, others dropped). A full 40/64-hex
// sha matches exactly; a shorter arg matches by prefix.
func matchProvenance(rep *runReport, sha string) *provenance {
	match := shaMatcher(sha)
	if match(rep.Integrate.FinalSHA) && landed(rep) {
		return provenanceFromFinal(rep, rep.Integrate.FinalSHA)
	}
	for _, a := range rep.PerAgent {
		if !match(a.SHA) {
			continue
		}
		p := &provenance{
			SHA:       a.SHA,
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
// commit — the aggregate of every landed branch, so it names no single task.
func provenanceFromFinal(rep *runReport, sha string) *provenance {
	return &provenance{
		SHA:       sha,
		Landed:    landed(rep),
		Role:      "landed-commit",
		Agent:     rep.AgentCmd,
		Strategy:  strategyOf(rep),
		Verify:    verifyVerdict(rep.Verify),
		StartedAt: rep.StartedAt,
		FinalSHA:  rep.Integrate.FinalSHA,
		Members:   len(rep.Integrate.Landed),
	}
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

// provenanceLine is the one-line human rendering of a provenance result.
func provenanceLine(p *provenance) string {
	run := p.RunID
	if run == "" {
		run = "(from commit note, started " + p.StartedAt + ")"
	} else {
		run = "run " + run
	}
	switch p.Role {
	case "landed-commit":
		return fmt.Sprintf("commit %s: landed integration commit of %s (%s, %d branch(es)), verify %s",
			short(p.SHA), run, p.Strategy, p.Members, p.Verify)
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
