// `sig log -release FROM..TO` renders a commit range as a release document:
// which runs landed the commits in the range, what each landing's acceptance
// was, what parked, what -verify-bisect dropped, which agent produced what, and
// whether sigbound.policy changed inside the range. Markdown by default (paste
// it under a Keep-a-Changelog heading), -json for tooling.
//
// It is the THIRD selector of sig log and shares its posture exactly: a pure
// reader over what runs already record (report.json under .git/sigbound/runs,
// park.json, and the landing notes under refs/notes/sigbound). It adds no
// storage, writes nothing, moves no ref, and — unlike GET /inbox — deliberately
// does NOT run the lazy park-timeout sweep: rendering a document must never
// transition a run's state.
//
// Selection is two passes, and the document records both so it can be audited:
//
//   - LANDINGS are exact and reachability-based: `git rev-list FROM..TO` gives
//     the commit set, and a run claims a commit only if it actually LANDED it
//     (matchProvenance's authority test — the same one `sig log -sha` applies to
//     a note). Range commits nothing claims are counted as `unattributed`
//     rather than dropped, so the document never claims a completeness it does
//     not have.
//   - ATTENTION ITEMS (parked/dropped/flagged) land no commit, so they appear in
//     no range and are selected by the run's start time inside the committer-date
//     window of the two endpoints. That window is an APPROXIMATION — committer
//     dates come from the repo, so a rewritten history or a skewed clock shifts
//     which runs fall inside it — which is why it is printed in the document.
//     Landings are unaffected: they are reachability, not time.
//
// A release document exists to be published, so by default it carries NONE of
// the invoker's own command strings (agentCmd/verifyCmd/... can embed secrets —
// see docs/USAGE.md's Provenance section). What it does carry is the repo's own
// committed policy `verify` lines and the agent's PROGRAM NAME. -with-commands
// opts into the verbatim strings and says so in both output shapes.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// releaseDoc is the whole document: the stable -json shape AND the input to the
// Markdown renderer, so the two can never disagree. Field names match `sig log
// -json` and the run report wherever they overlap (runId, landedSHA, strategy,
// verify, startedAt, source, intent). Empty sections are omitted; the four
// counters and `window` are always present, because "zero" is an answer a
// consumer needs to be able to read.
type releaseDoc struct {
	From    string `json:"from"`
	FromSHA string `json:"fromSHA"`
	To      string `json:"to"`
	ToSHA   string `json:"toSHA"`
	// Commits is len(rev-list FROM..TO) — every commit in the range, whether or
	// not sigbound landed it.
	Commits  int              `json:"commits"`
	Window   releaseWindow    `json:"window"`
	Landings []releaseLanding `json:"landings,omitempty"`
	Parked   []releaseParked  `json:"parked,omitempty"`
	Dropped  []releaseDropped `json:"dropped,omitempty"`
	Flagged  []releaseFlagged `json:"flagged,omitempty"`
	Agents   []releaseAgent   `json:"agents,omitempty"`
	Policy   releasePolicy    `json:"policy"`
	// Unattributed is how many range commits carry no sigbound landing —
	// hand-written, imported, cherry-picked, or landed by a run whose ledger and
	// note are both gone. Reported, never hidden.
	Unattributed int `json:"unattributed"`
	// Incomplete counts run dirs inside the window whose report.json was
	// expected but missing or unparseable (a crash mid-write) — the same posture
	// readLogRow takes: every other run still renders.
	Incomplete   int  `json:"incomplete"`
	WithCommands bool `json:"withCommands"`
}

// releaseWindow is the committer-date span the attention items were selected
// from, in UTC RFC3339. Printed because it is an approximation — see the file
// comment.
type releaseWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// releaseLanding is one run that landed at least one commit in the range.
// ProvenanceSource is "manifest" (the local run ledger) or "note" (a portable
// landing note on the commit), the same discriminant `sig log -sha` uses. For a
// note-sourced landing RunID is the id the note itself records: the note already
// passed matchProvenance's authority test for a commit in the range, and
// ProvenanceSource says where the row came from.
type releaseLanding struct {
	RunID     string   `json:"runId,omitempty"`
	StartedAt string   `json:"startedAt,omitempty"`
	Source    string   `json:"source,omitempty"` // "watch" for a watch cycle; empty for `sig run` / POST /runs
	Intent    string   `json:"intent,omitempty"` // the intents/<id>.intent this run came from
	Goal      string   `json:"goal,omitempty"`   // serve runs only (request.json); see goalOf
	Tasks     []string `json:"tasks,omitempty"`
	LandedSHA string   `json:"landedSHA,omitempty"` // integrate.finalSHA, abbreviated like `sig log`'s column
	Members   int      `json:"members,omitempty"`   // branches that landed together
	Strategy  string   `json:"strategy,omitempty"`
	Verify    string   `json:"verify,omitempty"` // pass|fail|none (verifyVerdict)
	// VerifyDetail is present only when at least one of its flags is true, so a
	// plainly-green landing stays quiet. cached: served from -verify-cache;
	// flaky: a retry was needed to reach green; repaired: -repair made it pass.
	VerifyDetail *releaseVerifyDetail `json:"verifyDetail,omitempty"`
	// Acceptance is the repo's OWN `verify` lines from sigbound.policy at the
	// base SHA (policy.verify on the report) — bytes committed to the repo, so
	// already exactly as public as the repo is. NOT the effective battery: an
	// invoker's -verify is deliberately excluded. A run with no policy file
	// contributes no acceptance and its verify verdict only.
	Acceptance []string `json:"acceptance,omitempty"`
	// Agent is the PROGRAM NAME of the resolved agent command (basename of its
	// first token) — never an argument, so it cannot carry a flag-embedded key.
	// "unknown" when there is no command, or when its first token is not a plain
	// program name (see agentName).
	Agent            string           `json:"agent,omitempty"`
	PolicyHash       string           `json:"policyHash,omitempty"`
	ProvenanceSource string           `json:"provenanceSource"`
	Commands         *releaseCommands `json:"commands,omitempty"` // -with-commands only
}

type releaseVerifyDetail struct {
	Cached   bool `json:"cached,omitempty"`
	Flaky    bool `json:"flaky,omitempty"`
	Repaired bool `json:"repaired,omitempty"`
}

// releaseCommands is the -with-commands payload: the invoker's own resolved
// command strings, verbatim. Absent by construction without that flag.
type releaseCommands struct {
	Agent    string `json:"agent,omitempty"`
	Verify   string `json:"verify,omitempty"`
	Repair   string `json:"repair,omitempty"`
	Resolver string `json:"resolver,omitempty"`
	Planner  string `json:"planner,omitempty"`
}

// releaseParked is a run holding a verified landing a human has not released
// (awaiting-ack), or one a human rejected. Reason is a parkReason* constant, or
// "unreadable" when park.json will not read back — surfacing a broken park is
// strictly better than dropping the one thing a human is owed.
type releaseParked struct {
	RunID        string            `json:"runId"`
	Status       string            `json:"status"` // awaiting-ack | rejected
	Reason       string            `json:"reason,omitempty"`
	Error        string            `json:"error,omitempty"` // set with reason "unreadable"
	Branches     []string          `json:"branches,omitempty"`
	MatchedPaths map[string]string `json:"matchedPaths,omitempty"`
	Attempts     int               `json:"attempts,omitempty"`
	ExpiresAt    string            `json:"expiresAt,omitempty"`
}

// releaseDropped is a run whose -verify-bisect dropped clean branches to salvage
// a green subset. Attempts is bisect's own probe count. The salvaged subset
// lands, so the same run may also appear under Landings — once each, never twice
// in either section.
type releaseDropped struct {
	RunID    string   `json:"runId"`
	Branches []string `json:"branches"`
	Attempts int      `json:"attempts,omitempty"`
}

// releaseFlagged is a run that set branches aside as real conflicts.
type releaseFlagged struct {
	RunID    string   `json:"runId"`
	Branches []string `json:"branches"`
}

// releaseAgent is one row of the attribution table: how many runs this document
// covers that agent program ran, and how many of them landed. "Covered" is every
// run that contributes a row anywhere — a ledger run inside the window, a ledger
// landing, or a landing that survives only as a note.
type releaseAgent struct {
	Agent  string `json:"agent"`
	Runs   int    `json:"runs"`
	Landed int    `json:"landed"`
}

// releasePolicy answers "did the landing bar move inside this range". Hashes are
// in first-seen order over the covered runs — the ledger walk is oldest-first,
// and note-sourced landings follow it — each with the run that first recorded
// it. Changed is len(Hashes) > 1 — one hash (or none) means the bar held.
type releasePolicy struct {
	Changed bool                `json:"changed"`
	Hashes  []releasePolicyHash `json:"hashes,omitempty"`
}

type releasePolicyHash struct {
	Hash       string `json:"hash"`
	FirstRunID string `json:"firstRunId,omitempty"`
}

// parseReleaseRange splits `-release FROM..TO`. Exactly two dots and two present
// endpoints: `..TO`, `FROM..`, a bare rev and the three-dot `FROM...TO` are all
// REFUSED rather than guessed. An implicit endpoint would make a published
// document depend on where the caller's HEAD happened to be, and a symmetric
// difference is not a release.
func parseReleaseRange(spec string) (from, to string, err error) {
	bad := fmt.Errorf("-release %q: expected FROM..TO with both endpoints present", spec)
	if strings.Contains(spec, "...") {
		return "", "", fmt.Errorf("-release %q: FROM...TO (symmetric difference) is not a release; expected FROM..TO", spec)
	}
	parts := strings.Split(spec, "..")
	if len(parts) != 2 {
		return "", "", bad
	}
	from, to = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if from == "" || to == "" {
		return "", "", bad
	}
	for _, r := range []string{from, to} {
		// A rev is handed to git as an argv token; one that starts with '-' would
		// be read as an option, and whitespace/control bytes are never part of a
		// ref name. Cheap guard, not a claim the rev exists — RevParse decides that.
		if strings.HasPrefix(r, "-") || strings.ContainsAny(r, " \t\n\r\v\f") {
			return "", "", fmt.Errorf("-release %q: %q is not a usable revision in FROM..TO", spec, r)
		}
	}
	return from, to, nil
}

// buildRelease assembles the document. It is the ONE builder behind both `sig
// log -release` and GET /log/release, so the CLI and the HTTP surface cannot
// drift (the same property handleLog/handleLogSHA already hold).
//
// An unresolvable endpoint is an error and NO document: a half-rendered release
// is worse than none. Everything past that point degrades open and loud — a
// missing runs dir is notes-only attribution, a missing notes ref is ledger-only
// attribution, an unreadable report is counted in Incomplete, and an unreadable
// park is listed as unreadable.
func buildRelease(ctx context.Context, g *gitx.Git, runsDir, from, to string, withCommands bool) (*releaseDoc, error) {
	fromSHA, err := g.RevParse(ctx, from)
	if err != nil {
		return nil, fmt.Errorf("range start %q: %w", from, err)
	}
	toSHA, err := g.RevParse(ctx, to)
	if err != nil {
		return nil, fmt.Errorf("range end %q: %w", to, err)
	}
	commits, err := g.RevList(ctx, fromSHA, toSHA)
	if err != nil {
		return nil, err
	}
	start, err := g.CommitTime(ctx, fromSHA)
	if err != nil {
		return nil, err
	}
	end, err := g.CommitTime(ctx, toSHA)
	if err != nil {
		return nil, err
	}

	doc := &releaseDoc{
		From: from, FromSHA: fromSHA, To: to, ToSHA: toSHA,
		Commits:      len(commits),
		Window:       releaseWindow{Start: start.UTC().Format(time.RFC3339), End: end.UTC().Format(time.RFC3339)},
		WithCommands: withCommands,
	}
	inRange := make(map[string]bool, len(commits))
	unclaimed := make(map[string]bool, len(commits))
	for _, c := range commits {
		inRange[c], unclaimed[c] = true, true
	}
	inWindow := func(t time.Time) bool {
		return !t.IsZero() && !t.Before(start) && !t.After(end)
	}

	// cover folds one run this document covers into the cross-run rollups. Every
	// run that contributes a row anywhere — a ledger run in the window, a ledger
	// landing, or a landing that survives only as a note — goes through here, so
	// the attribution table and the policy list can never disagree with the
	// landings above them (a note-sourced landing naming an agent that the
	// attribution table then omits is a document contradicting itself).
	agents := map[string]*releaseAgent{}
	seenPolicy := map[string]bool{}
	claimedRuns := map[string]bool{}
	cover := func(rep *runReport, runID string) {
		name := agentName(rep.AgentCmd)
		a := agents[name]
		if a == nil {
			a = &releaseAgent{Agent: name}
			agents[name] = a
		}
		a.Runs++
		if landed(rep) {
			a.Landed++
		}
		if rep.Policy != nil && rep.Policy.Hash != "" && !seenPolicy[rep.Policy.Hash] {
			seenPolicy[rep.Policy.Hash] = true
			doc.Policy.Hashes = append(doc.Policy.Hashes, releasePolicyHash{Hash: rep.Policy.Hash, FirstRunID: runID})
		}
	}

	// Pass A (ledger) and pass B (window), in ONE walk over the run dirs —
	// oldest-first, so a policy hash's firstRunId is genuinely the first.
	ids := runDirNames(runsDir)
	for i := len(ids) - 1; i >= 0; i-- {
		id := ids[i]
		dir := filepath.Join(runsDir, id)
		rep, err := readRunReport(dir)
		if err != nil {
			// A run we cannot read is dated by its id alone. Only the ones the
			// document claims to cover (inside the window) are counted, and only
			// when a report was genuinely expected — readLogRow's exact test. A
			// dir whose NAME carries no timestamp either cannot be placed in the
			// range at all and is not counted; sigbound writes no such dir.
			if inWindow(runStartedAt(id, nil)) {
				if _, incomplete := readLogRow(runsDir, id); incomplete {
					doc.Incomplete++
				}
			}
			continue
		}
		landedHere := landedCommitsIn(rep, inRange)
		covered := len(landedHere) > 0 || inWindow(runStartedAt(id, rep))
		if !covered {
			continue
		}
		if len(landedHere) > 0 {
			for _, sha := range landedHere {
				delete(unclaimed, sha)
			}
			claimedRuns[id] = true
			doc.Landings = append(doc.Landings, landingOf(rep, id, goalOf(dir), "manifest", withCommands))
		}
		cover(rep, id)
		// Attention items are LEDGER-ONLY: they are read from park.json and the
		// local report, so a run whose dir is gone contributes its landing (from
		// its note) but no park/dropped/flagged row. There is nothing local left
		// to act on for such a run, and inventing one from a user-writable note
		// is exactly the attribution a published document must not carry.
		if p := parkedOf(dir, id); p != nil {
			doc.Parked = append(doc.Parked, *p)
		}
		if n := len(rep.Integrate.DroppedByBisect); n > 0 {
			d := releaseDropped{RunID: id, Branches: rep.Integrate.DroppedByBisect}
			if rep.Verify.Bisect != nil {
				d.Attempts = rep.Verify.Bisect.Attempts
			}
			doc.Dropped = append(doc.Dropped, d)
		}
		if len(rep.Integrate.Flagged) > 0 {
			f := releaseFlagged{RunID: id}
			for _, fl := range rep.Integrate.Flagged {
				f.Branches = append(f.Branches, fl.Branch)
			}
			doc.Flagged = append(doc.Flagged, f)
		}
	}

	// Pass A, second half: range commits the local ledger does not claim may
	// still carry a portable landing note (the run dir was gc'd, or the commit
	// was fetched from where the run actually happened). ONE `notes list` names
	// which commits have notes at all; only those are read back.
	if len(unclaimed) > 0 {
		if noted, nerr := g.NoteList(ctx, "sigbound"); nerr == nil {
			for _, sha := range noted {
				if !unclaimed[sha] {
					continue
				}
				content, ok, _ := g.NoteShow(ctx, "sigbound", sha)
				if !ok {
					continue
				}
				var rep runReport
				if json.Unmarshal([]byte(content), &rep) != nil {
					continue
				}
				// Notes are user-writable and ride in from remotes, so a note is
				// authoritative ONLY if it genuinely concerns this commit and says
				// it landed — exactly resolveProvenance's test. Anything else is
				// ignored and the commit stays unattributed.
				p := matchProvenance(&rep, sha)
				if p == nil || !p.Landed {
					continue
				}
				delete(unclaimed, sha)
				if rep.RunID != "" && claimedRuns[rep.RunID] {
					continue // already rendered from the local ledger
				}
				if rep.RunID != "" {
					claimedRuns[rep.RunID] = true
				}
				cover(&rep, rep.RunID)
				doc.Landings = append(doc.Landings, landingOf(&rep, rep.RunID, "", "note", withCommands))
			}
		}
	}
	doc.Unattributed = len(unclaimed)

	// Newest-first, like every other `sig log` view. Run ids are timestamp-
	// prefixed, so a descending id sort is chronological; the landed sha breaks
	// ties (and orders note-sourced landings whose id the note omitted).
	sort.Slice(doc.Landings, func(i, j int) bool {
		if doc.Landings[i].RunID != doc.Landings[j].RunID {
			return doc.Landings[i].RunID > doc.Landings[j].RunID
		}
		return doc.Landings[i].LandedSHA > doc.Landings[j].LandedSHA
	})
	doc.Agents = make([]releaseAgent, 0, len(agents))
	for _, a := range agents {
		doc.Agents = append(doc.Agents, *a)
	}
	sort.Slice(doc.Agents, func(i, j int) bool {
		if doc.Agents[i].Runs != doc.Agents[j].Runs {
			return doc.Agents[i].Runs > doc.Agents[j].Runs
		}
		return doc.Agents[i].Agent < doc.Agents[j].Agent
	})
	doc.Policy.Changed = len(doc.Policy.Hashes) > 1
	return doc, nil
}

// landedCommitsIn returns the commits in the range that this run actually
// LANDED: its integration commit and every member branch tip recorded as landed.
// A run that did not land contributes nothing — a member commit of a red run
// that reached the range some other way is honestly unattributed, not a landing.
// This is matchProvenance's authority test, applied in the one direction a range
// query needs (report -> commits, not commit -> report).
func landedCommitsIn(rep *runReport, inRange map[string]bool) []string {
	if !landed(rep) {
		return nil
	}
	var out []string
	if inRange[rep.Integrate.FinalSHA] {
		out = append(out, rep.Integrate.FinalSHA)
	}
	for _, a := range rep.PerAgent {
		if inRange[a.SHA] && hasString(rep.Integrate.Landed, a.Branch) {
			out = append(out, a.SHA)
		}
	}
	return out
}

// landingOf projects one report into a landing row. goal is read from the run
// dir by the caller (a note-sourced landing has no dir, so none).
func landingOf(rep *runReport, runID, goal, source string, withCommands bool) releaseLanding {
	l := releaseLanding{
		RunID:            runID,
		StartedAt:        rep.StartedAt,
		Source:           rep.Source,
		Intent:           rep.Intent,
		Goal:             goal,
		LandedSHA:        short(rep.Integrate.FinalSHA),
		Members:          len(rep.Integrate.Landed),
		Strategy:         strategyOf(rep),
		Verify:           verifyVerdict(rep.Verify),
		Agent:            agentName(rep.AgentCmd),
		ProvenanceSource: source,
	}
	if l.StartedAt == "" {
		l.StartedAt = whenFromID(runID)
	}
	for _, t := range rep.Tasks {
		l.Tasks = append(l.Tasks, t.ID)
	}
	if v := rep.Verify; v.Cached || v.Flaky || v.Repaired {
		l.VerifyDetail = &releaseVerifyDetail{Cached: v.Cached, Flaky: v.Flaky, Repaired: v.Repaired}
	}
	if rep.Policy != nil {
		l.Acceptance = rep.Policy.Verify
		l.PolicyHash = rep.Policy.Hash
	}
	if withCommands {
		l.Commands = &releaseCommands{
			Agent: rep.AgentCmd, Verify: rep.VerifyCmd, Repair: rep.RepairCmd,
			Resolver: rep.ResolverCmd, Planner: rep.PlannerCmd,
		}
	}
	return l
}

// parkedOf reads a covered run's parking record, or nil when it has none. It
// deliberately does NOT call enforceParkTimeout: that lazy sweep belongs to
// GET /inbox, and rendering a document must never transition a run's state.
func parkedOf(dir, runID string) *releaseParked {
	status, _ := diskRunStatus(dir)
	if status != statusAwaitingAck && status != statusRejected {
		return nil
	}
	pk, err := readPark(dir)
	if err != nil {
		return &releaseParked{RunID: runID, Status: status, Reason: "unreadable", Error: err.Error()}
	}
	p := &releaseParked{
		RunID: runID, Status: status, Reason: pk.Reason,
		Branches: pk.branches(), MatchedPaths: pk.matchedPaths(), Attempts: len(pk.Attempts),
	}
	if deadline, ok := pk.deadline(); ok {
		p.ExpiresAt = deadline.UTC().Format(time.RFC3339)
	}
	return p
}

// agentName reduces a resolved agent command to the PROGRAM that ran: the
// basename of its first whitespace-separated token, so `/usr/local/bin/claude
// -p` and `claude -p --model x` are both "claude". It is never an argument, so
// it cannot carry a flag-embedded key. "unknown" for an empty command and for a
// first token that is not a plain program name — notably one containing '=',
// which is a leading environment assignment (`API_KEY=… claude -p`) and exactly
// the shape that could carry a secret into a published document.
func agentName(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 || strings.Contains(fields[0], "=") {
		return "unknown"
	}
	name := fields[0]
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return "unknown"
	}
	return name
}

// ---- rendering ----

// renderReleaseMarkdown writes the paste-ready document: `###` sections under a
// one-line summary and no document title, so the block drops straight in under a
// Keep-a-Changelog `## [version]` heading.
//
// There is deliberately no Added/Fixed/Changed classification: a run records
// what landed and how it was verified, not what KIND of change it was, and
// classifying would require a field nothing writes.
func renderReleaseMarkdown(w io.Writer, doc *releaseDoc) error {
	b := &strings.Builder{}
	if doc.WithCommands {
		fmt.Fprintf(b, "> commands are included verbatim and may contain secrets\n\n")
	}
	if doc.Commits == 0 {
		fmt.Fprintf(b, "_No commits in `%s..%s`._\n", doc.From, doc.To)
		_, err := io.WriteString(w, b.String())
		return err
	}
	fmt.Fprintf(b, "`%s..%s` — %s, %s, %s unattributed.\n",
		doc.From, doc.To,
		plural(doc.Commits, "commit", "commits"),
		plural(len(doc.Landings), "landing", "landings"),
		plural(doc.Unattributed, "commit", "commits"))
	fmt.Fprintf(b, "Attention window (committer dates, approximate): %s .. %s\n", doc.Window.Start, doc.Window.End)

	if len(doc.Landings) > 0 {
		fmt.Fprintf(b, "\n### Landed\n\n")
		for _, l := range doc.Landings {
			fmt.Fprintf(b, "- **%s** — %s, %s, verify %s\n", l.LandedSHA, releaseRunLabel(l), plural(l.Members, "branch", "branches"), l.Verify)
			for _, line := range releaseLandingDetails(l) {
				fmt.Fprintf(b, "  - %s\n", line)
			}
		}
	}
	if len(doc.Parked) > 0 {
		fmt.Fprintf(b, "\n### Needed a human\n\n")
		for _, p := range doc.Parked {
			fmt.Fprintf(b, "- %s — %s (%s)", p.RunID, p.Status, p.Reason)
			if p.Error != "" {
				fmt.Fprintf(b, ": %s", p.Error)
			}
			if len(p.Branches) > 0 {
				fmt.Fprintf(b, ", branches %s", strings.Join(p.Branches, ", "))
			}
			if len(p.MatchedPaths) > 0 {
				fmt.Fprintf(b, ", matched %s", joinMatchedPaths(p.MatchedPaths))
			}
			if p.ExpiresAt != "" {
				fmt.Fprintf(b, ", expires %s", p.ExpiresAt)
			}
			fmt.Fprintln(b)
		}
	}
	if len(doc.Dropped) > 0 {
		fmt.Fprintf(b, "\n### Dropped by bisect\n\n")
		for _, d := range doc.Dropped {
			fmt.Fprintf(b, "- %s — %s dropped to salvage a green subset: %s\n",
				d.RunID, plural(len(d.Branches), "branch", "branches"), strings.Join(d.Branches, ", "))
		}
	}
	if len(doc.Flagged) > 0 {
		fmt.Fprintf(b, "\n### Flagged\n\n")
		for _, f := range doc.Flagged {
			fmt.Fprintf(b, "- %s — %s set aside as conflicts: %s\n",
				f.RunID, plural(len(f.Branches), "branch", "branches"), strings.Join(f.Branches, ", "))
		}
	}
	if len(doc.Agents) > 0 {
		fmt.Fprintf(b, "\n### Attribution\n\n| agent | runs | landed |\n|---|---|---|\n")
		for _, a := range doc.Agents {
			fmt.Fprintf(b, "| %s | %d | %d |\n", a.Agent, a.Runs, a.Landed)
		}
	}
	fmt.Fprintf(b, "\n### Policy\n\n")
	switch {
	case len(doc.Policy.Hashes) == 0:
		fmt.Fprintf(b, "No run in this range recorded a `%s`.\n", policyFileName)
	case doc.Policy.Changed:
		fmt.Fprintf(b, "`%s` CHANGED inside this range:\n\n", policyFileName)
		for _, h := range doc.Policy.Hashes {
			fmt.Fprintf(b, "- `%s` — first seen in %s\n", short(h.Hash), h.FirstRunID)
		}
	default:
		fmt.Fprintf(b, "`%s` unchanged: `%s`.\n", policyFileName, short(doc.Policy.Hashes[0].Hash))
	}

	fmt.Fprintf(b, "\n_%s in this range carry no sigbound landing", plural(doc.Unattributed, "commit", "commits"))
	fmt.Fprintf(b, " (hand-written, imported, or landed by a run whose ledger and note are both gone)")
	if doc.Incomplete > 0 {
		fmt.Fprintf(b, "; %s unreadable", plural(doc.Incomplete, "run dir", "run dirs"))
	}
	fmt.Fprintf(b, "._\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// releaseRunLabel names the run behind a landing, including where the row came
// from — a note-sourced row may be all that survives of a gc'd run.
func releaseRunLabel(l releaseLanding) string {
	run := "run " + l.RunID
	if l.RunID == "" {
		run = "run unrecorded"
	}
	if l.ProvenanceSource == "note" {
		run += " (from commit note)"
	}
	if l.Source != "" {
		run += " [" + l.Source + "]"
	}
	if l.Strategy != "" {
		run += ", " + l.Strategy
	}
	return run
}

// releaseLandingDetails is the indented detail block under one landing: only the
// fields the run actually recorded.
func releaseLandingDetails(l releaseLanding) []string {
	var out []string
	if l.Intent != "" {
		out = append(out, "intent: "+l.Intent)
	}
	if l.Goal != "" {
		out = append(out, "goal: "+l.Goal)
	}
	if len(l.Tasks) > 0 {
		out = append(out, "tasks: "+strings.Join(l.Tasks, ", "))
	}
	if len(l.Acceptance) > 0 {
		out = append(out, "acceptance: `"+strings.Join(l.Acceptance, "` · `")+"`")
	}
	if l.VerifyDetail != nil {
		var flags []string
		if l.VerifyDetail.Cached {
			flags = append(flags, "cached")
		}
		if l.VerifyDetail.Flaky {
			flags = append(flags, "flaky")
		}
		if l.VerifyDetail.Repaired {
			flags = append(flags, "repaired")
		}
		out = append(out, "verify: "+strings.Join(flags, ", "))
	}
	agent := "agent: " + l.Agent
	if l.PolicyHash != "" {
		agent += " · policy " + short(l.PolicyHash)
	}
	out = append(out, agent)
	if l.Commands != nil {
		for _, c := range []struct{ name, cmd string }{
			{"agent", l.Commands.Agent}, {"verify", l.Commands.Verify}, {"repair", l.Commands.Repair},
			{"resolver", l.Commands.Resolver}, {"planner", l.Commands.Planner},
		} {
			if c.cmd != "" {
				out = append(out, c.name+" command: `"+c.cmd+"`")
			}
		}
	}
	return out
}

// joinMatchedPaths renders a park's path -> glob map in a stable order.
func joinMatchedPaths(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" ("+m[k]+")")
	}
	return strings.Join(parts, ", ")
}

// logRelease is `sig log -release`'s selector: build once, render either shape.
// An empty range is exit 0 with a document that SAYS it is empty (plus a stderr
// note) — an empty release is a true answer, and a consumer must never be handed
// an empty file to interpret.
func logRelease(ctx context.Context, w io.Writer, g *gitx.Git, runsDir, spec string, asJSON, withCommands bool) (int, error) {
	from, to, err := parseReleaseRange(spec)
	if err != nil {
		return exitOperationalError, err
	}
	doc, err := buildRelease(ctx, g, runsDir, from, to, withCommands)
	if err != nil {
		return exitOperationalError, err
	}
	if doc.Commits == 0 {
		fmt.Fprintf(os.Stderr, "sig log: no commits in %s..%s\n", from, to)
	}
	if asJSON {
		return exitOK, writeJSONIndent(w, doc)
	}
	return exitOK, renderReleaseMarkdown(w, doc)
}
