// intents/ — the repo's standing statements of work (issue #112). One file per
// intent under intents/, in the SAME flat KEY=VALUE dialect as sig.conf and
// sigbound.policy: they all share parseConfigFile's lexer (comments, blank
// lines, first-'=' split, CRLF tolerance, line numbers), so there is one
// flat-file dialect in this binary, not three. An intent's id is its filename
// minus the ".intent" extension, and `sig run -intent <id>` turns it into the
// run's task list the way -tasks does.
//
// An intent is INPUT, not a gate, and is therefore read from the WORKING TREE —
// unlike sigbound.policy, which gates a landing and so is read from the base
// SHA's committed tree (see policy.go). That split is what lets an intent just
// written by `sig intent import-github` be run before it is committed, and it is
// safe because the one gate-touching key, `acceptance`, composes exactly the way
// a -verify flag does: APPENDED to the policy battery, never replacing it (see
// runRun's -intent branch and resolvePolicy). An intent can therefore only make
// a run's landing bar stricter, never weaker, however it arrived on disk.
//
// Two things live on top of that base (issue #113). TEMPLATES: files under
// intents/templates/ in the same dialect, copied to intents/<id>.intent by `sig
// intent new` — a copy, with no substitution and no expansion. RECURRING: an
// intent's `schedule` is made live by `sig serve -watch`, whose existing cycle
// asks once per tick which intents are due and fires one as an ordinary run.
// This file owns the durable half of that (intent-fired.json, due-ness); the
// cycle that drives it is watchIntentCycle in watch.go.
//
// TRUST BOUNDARY, and it moved when `schedule` became live. An intent is read
// from the working tree, and a scheduled one is FIRED by the daemon, so whoever
// can write a `sig serve -watch -watch-agent` daemon's working tree can schedule
// agent execution on that machine: an UNTRACKED intents/<id>.intent with a
// `schedule` line is fired by the next due tick, with no commit, no review and
// nobody choosing to run it. The converse is what makes this a boundary rather
// than a hole: LANDING an intents/*.intent changes nothing, because a landing
// moves the base ref and never touches the working tree — a committed intent
// goes live only when someone checks it out into the daemon's tree. Treat write
// access to a watching daemon's working tree as execute access on that machine
// (docs/USAGE.md, Recurring intents).
//
// See docs/USAGE.md's Intents section.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

const (
	// intentDirName is the repo-relative directory intents are read from and
	// written to; intentFileExt is the extension that marks a file in it as an
	// intent. Anything else in the directory (a README, a scratch note) is
	// ignored, so the directory can hold more than intents.
	intentDirName = "intents"
	intentFileExt = ".intent"
	// intentTemplateDirName is the SUBDIRECTORY of intents/ holding the
	// skeletons `sig intent new -template` instantiates. A subdirectory is what
	// keeps a template out of the repo's actual asks: listIntents never recurses
	// and skips directories, so a file in here is invisible to `sig intent list`,
	// to `sig run -intent`, and to the recurring runtime.
	intentTemplateDirName = "templates"
)

// intent is one parsed, validated intent file. Every field but ID and Goal is
// optional; ID comes from the filename, never from the file's contents, so an
// intent cannot claim to be a different intent than the one it was loaded as.
type intent struct {
	ID   string // filename minus intentFileExt; slug-safe (it becomes a task id and a branch component)
	Path string // the file it was parsed from, for error messages
	Goal string // the prose an agent is given as its task prompt; repeatable `goal` lines joined with "\n"
	// Acceptance is a verify command for runs started from this intent. It is
	// APPENDED to whatever verify the run already has (a -verify flag and the
	// repo's sigbound.policy battery), so it can only ever add a way to fail —
	// the tighten-only rule flags obey against policy. LIMIT: like -verify, it
	// is subject to -verify-impact, which by its own documented design runs a
	// scoped command INSTEAD of the verify command; a run that pairs the two
	// scopes this command away exactly as it would a -verify flag (a policy
	// battery is the only thing that suppresses -verify-impact — see
	// resolvePolicy).
	Acceptance string
	// Files is the intent's lane: the exact repo-relative paths a run from it
	// may create or modify, enforced by -lanes like any -tasks task's own files
	// (see applyLaneEnforcement). Empty means no declared lane, so no lane
	// enforcement — the same meaning it has in a -tasks file.
	Files []string
	// Priority orders `sig intent list` (higher first, then id); nothing else
	// reads it. It is a hint for whoever picks the next intent to run, not a
	// queue the driver services.
	Priority int
	// Schedule is how often this intent wants to run. Zero when unset. It is
	// acted on by ONE thing: a `sig serve -watch` cycle, which fires a due intent
	// as an ordinary run (see watchIntentCycle). Nothing else — a `sig run
	// -intent` is always an explicit ask and ignores the schedule entirely.
	Schedule time.Duration
	// Issue is the GitHub issue number this intent was imported from (0 when it
	// was not), recorded by `sig intent import-github` so a later publish
	// command can close the issue when the intent lands. Nothing in this binary
	// closes issues.
	Issue int
}

// intentDir is the repo's intents directory. It need not exist: an absent
// directory is an empty intent set, not an error (see listIntents).
func intentDir(repo string) string { return filepath.Join(repo, intentDirName) }

// intentPath is the file id is read from and written to.
func intentPath(repo, id string) string { return filepath.Join(intentDir(repo), id+intentFileExt) }

// loadIntent reads and parses one intent by id. A missing file names the exact
// path it looked for (an intent id is easy to typo, and the caller cannot fix it
// without knowing where it was expected); a malformed one fails loudly naming
// file, line and key — never a partial intent, and never a silently skipped key.
func loadIntent(repo, id string) (intent, error) {
	// Validate the id before touching the filesystem. parseIntent checks it too,
	// but only after the bytes are read, so an unsafe id would open and slurp a
	// file before being refused. intentPath already confines the result under
	// the repo's intents/ dir, so this is depth rather than a hole -- but there
	// is no reason to read a file this function is going to reject.
	if !slugSafe(id) {
		return intent{}, fmt.Errorf("intent id %q is not a safe name: it becomes a task id and a branch component", id)
	}
	path := intentPath(repo, id)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return intent{}, fmt.Errorf("no intent %q: %s does not exist (see `sig intent list`)", id, path)
		}
		return intent{}, fmt.Errorf("read intent %q: %w", id, err)
	}
	it, err := parseIntent(data, id)
	if err != nil {
		return intent{}, fmt.Errorf("%s: %w", path, err)
	}
	it.Path = path
	return it, nil
}

// listIntents parses EVERY *.intent file in the repo's intents directory,
// ordered by Priority descending then id ascending. A missing directory yields
// no intents and no error (a repo need not use intents at all), but a file that
// is present and malformed fails the WHOLE listing: a list that quietly dropped
// the one file it could not read would misreport what the repo is asking for.
func listIntents(repo string) ([]intent, error) {
	entries, err := os.ReadDir(intentDir(repo))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", intentDir(repo), err)
	}
	var out []intent
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), intentFileExt) {
			continue
		}
		it, err := loadIntent(repo, strings.TrimSuffix(de.Name(), intentFileExt))
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// parseIntent parses the flat KEY=VALUE bytes of one intent file, whose id (from
// the filename) is passed in rather than read from the contents. It reuses
// parseConfigFile's lexer and adds the intent KEY SCHEMA on top, with the same
// posture parsePolicy has: `goal` and `files` are repeatable, every other key is
// scalar and a DUPLICATE is an error (a second `acceptance` silently overriding
// the first is exactly the kind of edit that would change a landing bar without
// anyone reading it), and an UNKNOWN key is an error naming line and key. Every
// rejection names the 1-based line.
//
// `goal` lines are joined with "\n" in file order, so an imported issue body
// keeps its line structure. Values are edge-trimmed by the shared lexer, so
// LEADING INDENTATION inside multi-line prose is not preserved.
func parseIntent(data []byte, id string) (intent, error) {
	if !slugSafe(id) {
		return intent{}, fmt.Errorf("intent id %q is not slug-safe (allowed: A-Za-z0-9._-, not . or ..): it becomes a task id and a branch component", id)
	}
	entries, err := parseConfigFile(data)
	if err != nil {
		return intent{}, err
	}
	it := intent{ID: id}
	var goalLines []string
	seen := map[string]bool{} // scalar keys already set (duplicate => error)
	scalar := func(e configEntry) error {
		if seen[e.Key] {
			return fmt.Errorf("line %d: duplicate key %q", e.Line, e.Key)
		}
		seen[e.Key] = true
		return nil
	}
	for _, e := range entries {
		switch e.Key {
		case "goal":
			goalLines = append(goalLines, e.Value)
		case "files":
			paths := splitCSV(e.Value)
			if len(paths) == 0 {
				return intent{}, fmt.Errorf("line %d: files requires at least one path", e.Line)
			}
			for _, p := range paths {
				np := filepath.ToSlash(p)
				if !relSafe(np) {
					return intent{}, fmt.Errorf("line %d: files entry %q is not a safe repo-relative path (no absolute paths, no \"..\")", e.Line, p)
				}
				it.Files = append(it.Files, np)
			}
		case "acceptance":
			if err := scalar(e); err != nil {
				return intent{}, err
			}
			if strings.TrimSpace(e.Value) == "" {
				return intent{}, fmt.Errorf("line %d: acceptance requires a command", e.Line)
			}
			it.Acceptance = e.Value
		case "priority":
			if err := scalar(e); err != nil {
				return intent{}, err
			}
			n, err := strconv.Atoi(strings.TrimSpace(e.Value))
			if err != nil {
				return intent{}, fmt.Errorf("line %d: priority must be an integer, got %q", e.Line, e.Value)
			}
			it.Priority = n
		case "schedule":
			if err := scalar(e); err != nil {
				return intent{}, err
			}
			// Validated to the same shape as the policy's watch-interval — a
			// positive Go duration — so #113's runtime inherits a value this
			// binary has already rejected the bad forms of.
			d, err := time.ParseDuration(strings.TrimSpace(e.Value))
			if err != nil || d <= 0 {
				return intent{}, fmt.Errorf("line %d: schedule must be a positive duration (e.g. 24h), got %q", e.Line, e.Value)
			}
			it.Schedule = d
		case "issue":
			if err := scalar(e); err != nil {
				return intent{}, err
			}
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(e.Value), "#")))
			if err != nil || n <= 0 {
				return intent{}, fmt.Errorf("line %d: issue must be a positive integer, got %q", e.Line, e.Value)
			}
			it.Issue = n
		default:
			return intent{}, fmt.Errorf("line %d: unknown intent key %q", e.Line, e.Key)
		}
	}
	it.Goal = strings.TrimSpace(strings.Join(goalLines, "\n"))
	if it.Goal == "" {
		return intent{}, errors.New("goal is required (the prose an agent is given as its task prompt)")
	}
	return it, nil
}

// ---- the recurring runtime's durable state (issue #113) ----

// intentFiredFileName records when each scheduled intent last FIRED, per repo,
// beside the run journal under <git-common-dir>/sigbound/ — the same place
// watch-seen.json lives, and for the same reason: it is a daemon's state about
// the repo, never content of it, so a scheduled run must not dirty the working
// tree it is about to hand an agent.
const intentFiredFileName = "intent-fired.json"

// intentFired is intent-fired.json's shape: intent id -> its last fire.
//
// It is a LEDGER, and that is the one place it differs from watch-seen.json,
// which is a cache. Losing this file costs one EXTRA fire per scheduled intent
// (an unknown last-fire reads as never fired, and never fired is due), where
// losing the seen-set costs only a re-examination. That direction is chosen
// deliberately: a fire is an ordinary policy- and verify-gated run, so an extra
// one wastes a cycle, while failing closed on an unreadable record would be a
// schedule that silently stopped.
type intentFired struct {
	Intents map[string]intentFiredEntry `json:"intents"`
}

// intentFiredEntry is one intent's last fire. Both fields are written by the
// SAME atomic write, when the run slot is TAKEN and before any agent runs — see
// watchIntentCycle for why that end of the run is the one that matters.
type intentFiredEntry struct {
	FiredAt time.Time `json:"firedAt"`
	// Branches are the fire branches (agent/<intent>-<stamp>) this intent has
	// produced that still exist. It is the DURABLE half of retirement: a watch
	// cycle over arrivals is gated by the repo policy plus -watch-verify and NOT
	// by any intent's `acceptance`, so a fire's own branch offered back as an
	// ordinary arrival could land exactly the work the acceptance rejected.
	// watchCollect excludes every name here, and because the name is written
	// BEFORE the run starts, that holds across a lost seen-set and across a crash
	// mid-run — neither of which the seen-set, a cache, could survive.
	//
	// Bounded, not a log: each fire drops the names that no longer resolve, and a
	// fire that landed cleanly deletes its own branch (see retireIntentBranch),
	// so this holds exactly the fire branches that still EXIST — a red fire's,
	// and any a human or an ack kept — never one entry per fire forever.
	Branches []string `json:"branches,omitempty"`
}

// readIntentFired reads the record. Every failure — missing, unreadable,
// truncated, malformed, wrong shape, and the empty path `sig intent show` passes
// for a repo git cannot answer for — yields an EMPTY record rather than an
// error: see intentFired's doc comment for what an empty record costs.
func readIntentFired(path string) intentFired {
	empty := intentFired{Intents: map[string]intentFiredEntry{}}
	if path == "" {
		return empty
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	var f intentFired
	if err := json.Unmarshal(data, &f); err != nil || f.Intents == nil {
		return empty
	}
	for id, e := range f.Intents {
		// The two fields fail open in OPPOSITE directions, so they are dropped
		// separately. An unknown fire time means "never fired" -- the same thing
		// its absence means, and firing one extra time is the safe answer. An
		// unknown branch list is NOT safe to forget: those names are what
		// watchCollect refuses to adopt, and dropping them makes a red fire's
		// branch adoptable by the cycle bar, which does not apply the intent's
		// acceptance. So an entry with no time keeps its branches and simply
		// reads as never-fired.
		if id == "" {
			delete(f.Intents, id)
			continue
		}
		if e.FiredAt.IsZero() && len(e.Branches) == 0 {
			delete(f.Intents, id)
		}
	}
	return f
}

// fireBranches is every branch the record names, across all intents: the
// branches scheduled fires produced that no fire has yet seen disappear. This is
// what watchCollect refuses to adopt — see intentFiredEntry.Branches.
func (f intentFired) fireBranches() map[string]bool {
	out := map[string]bool{}
	for _, e := range f.Intents {
		for _, b := range e.Branches {
			out[b] = true
		}
	}
	return out
}

// writeIntentFired publishes the record atomically through atomicWriteFile
// (unique temp + rename, hardened against Windows' contended-destination
// failure), so a daemon killed mid-write leaves either the whole old record or
// the whole new one.
//
// Unlike writeWatchSeen this is NOT best-effort: it returns its error. A fire
// that is not recorded is a fire that happens again on the next tick, so the
// caller has to be able to say so out loud (see watchIntentCycle).
func writeIntentFired(path string, f intentFired) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return atomicWriteFile(dir, filepath.Base(path), data, 0o644)
}

// intentFiredPathFor resolves a repo's fired record for the CLI, which has no
// server to have resolved it already. A repo git cannot answer for (not a
// repository, git missing from PATH) yields "", which readIntentFired reports as
// an empty record — `sig intent list`/`show` only REPORT the schedule, and must
// not fail over state that only a watch daemon writes.
func intentFiredPathFor(ctx context.Context, repo string) string {
	common, err := gitx.New(repo).GitCommonDir(ctx)
	if err != nil {
		return ""
	}
	return filepath.Join(common, "sigbound", intentFiredFileName)
}

// intentFiredSkew is how far ahead of now a recorded fire may be and still be
// believed. Clocks step (NTP, a VM resuming, a machine that booted with a bad
// RTC), and a stamp a few minutes ahead is that; a stamp a year ahead is not a
// fire, and believing it would wedge the schedule for a year.
const intentFiredSkew = time.Hour

// effectiveFiredAt is the last-fire time due-ness is actually judged against. A
// stamp in the FUTURE beyond intentFiredSkew is not a time this daemon can have
// fired at, so it reads as NEVER fired — which is the same fail-open direction
// the whole record degrades in (see intentFired): the cost is one extra fire,
// never a schedule that silently stopped. It is surfaced rather than swallowed:
// `sig intent show` flags it and the watch loop emits watch_error
// (at: intent-fired-future) on the tick that finds it, and the fire that follows
// overwrites the stamp with a real one.
func effectiveFiredAt(last, now time.Time) time.Time {
	if last.After(now.Add(intentFiredSkew)) {
		return time.Time{}
	}
	return last
}

// intentDue reports whether it wants to fire at now, given the last time it did
// (zero = never). An intent with no schedule is never due.
//
// A missed window fires ONCE on return, not once per window missed: due-ness is
// a comparison against the LAST fire, and firing re-bases it on the moment it
// fired, so a daemon that was down for a week returns and runs a 24h intent one
// time, not seven. A never-fired scheduled intent is due immediately — the first
// tick that sees it runs it, which is also what makes adding a schedule to an
// intent take effect without anyone having to seed a timestamp.
func intentDue(it intent, last, now time.Time) bool {
	if it.Schedule <= 0 {
		return false
	}
	last = effectiveFiredAt(last, now)
	return last.IsZero() || !now.Before(last.Add(it.Schedule))
}

// dueIntent picks the intent to fire now: the LEAST RECENTLY FIRED of the due
// ones (a never-fired intent is the oldest there is), with ties keeping
// listIntents' order — priority desc, then id asc. Rotation rather than fixed
// priority order is what stops a short-scheduled or high-priority intent from
// owning every tick a cell has: under priority order alone, an intent due on
// every tick meant every lower one was permanently starved.
//
// ONE per call, deliberately: a run carries exactly one intent id for
// attribution, one acceptance command in its verify battery, and one lane, so
// batching two intents into a cycle would blur all three. The rest wait for a
// later tick.
func dueIntent(intents []intent, fired intentFired, now time.Time) (intent, bool) {
	var pick intent
	var pickAt time.Time
	found := false
	for _, it := range intents {
		last := effectiveFiredAt(fired.Intents[it.ID].FiredAt, now)
		if !intentDue(it, last, now) {
			continue
		}
		if !found || last.Before(pickAt) {
			pick, pickAt, found = it, last, true
		}
	}
	return pick, found
}

// intentJSON is the stable machine shape of `sig intent list`/`show`, and the
// only place an intent is rendered for a consumer. Schedule renders as its
// duration string, and every optional field is omitempty, so a minimal intent
// renders minimally.
type intentJSON struct {
	ID         string   `json:"id"`
	Path       string   `json:"path"`
	Goal       string   `json:"goal"`
	Acceptance string   `json:"acceptance,omitempty"`
	Files      []string `json:"files,omitempty"`
	Priority   int      `json:"priority,omitempty"`
	Schedule   string   `json:"schedule,omitempty"`
	// LastFired, NextDue and Due describe the recurring runtime and appear only
	// for an intent that HAS a schedule. LastFired/NextDue stay absent until a
	// cycle has fired it once; Due is the verdict the next `sig serve -watch`
	// tick would reach, so a never-fired scheduled intent reports due with no
	// next-due timestamp — it is due now.
	LastFired string `json:"lastFired,omitempty"`
	NextDue   string `json:"nextDue,omitempty"`
	Due       bool   `json:"due,omitempty"`
	// FiredInFuture says the recorded last fire is far enough AHEAD of now that
	// no daemon can have made it (see effectiveFiredAt), so it is being ignored
	// and the intent reads as never fired. Reported because the alternative is an
	// intent that says "next due" a year out and nobody knowing why.
	FiredInFuture bool `json:"firedInFuture,omitempty"`
	Issue         int  `json:"issue,omitempty"`
}

// intentReport renders one intent. last is when a cycle last fired it (zero =
// never) and now is the instant due-ness is judged at, both passed in so a
// listing judges every row against one clock.
func intentReport(it intent, last, now time.Time) intentJSON {
	out := intentJSON{
		ID: it.ID, Path: it.Path, Goal: it.Goal, Acceptance: it.Acceptance,
		Files: it.Files, Priority: it.Priority, Issue: it.Issue,
	}
	if it.Schedule > 0 {
		out.Schedule = it.Schedule.String()
		out.Due = intentDue(it, last, now)
		out.FiredInFuture = !last.IsZero() && effectiveFiredAt(last, now).IsZero()
		if !last.IsZero() {
			out.LastFired = last.UTC().Format(time.RFC3339)
			out.NextDue = last.Add(it.Schedule).UTC().Format(time.RFC3339)
		}
	}
	return out
}

func runIntent(w io.Writer, argv []string) (int, error) {
	if len(argv) == 0 {
		intentUsage(w)
		return exitOperationalError, errors.New("a subcommand is required")
	}
	switch argv[0] {
	case "list":
		return runIntentList(w, argv[1:])
	case "show":
		return runIntentShow(w, argv[1:])
	case "new":
		return runIntentNew(w, argv[1:])
	case "import-github":
		return runIntentImportGitHub(w, argv[1:])
	case "-h", "--help", "help":
		intentUsage(w)
		return exitOK, nil
	default:
		intentUsage(w)
		return exitOperationalError, fmt.Errorf("unknown subcommand %q", argv[0])
	}
}

func intentUsage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  sig intent list          -repo PATH [-json]")
	fmt.Fprintln(w, "  sig intent show ID       -repo PATH [-json]")
	fmt.Fprintln(w, "  sig intent new ID        -repo PATH -template NAME")
	fmt.Fprintln(w, "  sig intent import-github -repo PATH [-label sigbound] [-limit 100] [-json]")
	fmt.Fprintln(w, "intents live in <repo>/intents/*.intent; run one with `sig run -intent ID`.")
}

func runIntentList(w io.Writer, argv []string) (int, error) {
	fs := flag.NewFlagSet("intent list", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig intent list -repo PATH [-json]")
		fmt.Fprintln(fs.Output(), "list every intents/*.intent in the repo's WORKING TREE, highest priority first.")
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "path to the target git repository")
	asJSON := fs.Bool("json", false, "emit JSON (stable field names; see docs/USAGE.md's Intents section)")
	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitOK, nil
		}
		return exitOperationalError, err
	}
	if strings.TrimSpace(*repo) == "" {
		return exitOperationalError, errors.New("-repo is required")
	}
	intents, err := listIntents(*repo)
	if err != nil {
		return exitOperationalError, err
	}
	// One read of the fired record and one clock for the whole listing, so two
	// rows can never disagree about what "due" meant.
	fired := readIntentFired(intentFiredPathFor(context.Background(), *repo))
	now := time.Now()
	if *asJSON {
		rows := make([]intentJSON, 0, len(intents))
		for _, it := range intents {
			rows = append(rows, intentReport(it, fired.Intents[it.ID].FiredAt, now))
		}
		return exitOK, writeJSONIndent(w, rows)
	}
	if len(intents) == 0 {
		fmt.Fprintf(w, "no intents in %s\n", intentDir(*repo))
		return exitOK, nil
	}
	scheduled := false
	fmt.Fprintf(w, "%-24s %4s %-10s  %s\n", "INTENT", "PRIO", "SCHEDULE", "GOAL")
	for _, it := range intents {
		sched := "-"
		if it.Schedule > 0 {
			scheduled = true
			sched = it.Schedule.String()
			if intentDue(it, fired.Intents[it.ID].FiredAt, now) {
				sched += "!" // due at the next `sig serve -watch` tick
			}
		}
		fmt.Fprintf(w, "%-24s %4d %-10s  %s\n", it.ID, it.Priority, sched, firstLine(it.Goal))
	}
	legend := ""
	if scheduled {
		legend = " (schedule! = due at the next `sig serve -watch` tick)"
	}
	fmt.Fprintf(w, "\n%d intent(s) in %s%s\n", len(intents), intentDir(*repo), legend)
	return exitOK, nil
}

func runIntentShow(w io.Writer, argv []string) (int, error) {
	fs := flag.NewFlagSet("intent show", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig intent show ID -repo PATH [-json]")
		fmt.Fprintln(fs.Output(), "print one intent as parsed — what `sig run -intent ID` would actually run.")
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "path to the target git repository")
	asJSON := fs.Bool("json", false, "emit JSON (stable field names; see docs/USAGE.md's Intents section)")
	// ID is positional and documented FIRST (`sig intent show ID -repo P`),
	// which stdlib flag cannot parse on its own — it stops at the first non-flag
	// argument. Pull a leading positional off before parsing, the same way
	// `sig ack RUN_ID` does, so both orders work.
	var id string
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		id, argv = argv[0], argv[1:]
	}
	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitOK, nil
		}
		return exitOperationalError, err
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	} else if id == "" || fs.NArg() > 0 {
		return exitOperationalError, errors.New("exactly one intent ID is required")
	}
	if strings.TrimSpace(*repo) == "" {
		return exitOperationalError, errors.New("-repo is required")
	}
	it, err := loadIntent(*repo, id)
	if err != nil {
		return exitOperationalError, err
	}
	last := readIntentFired(intentFiredPathFor(context.Background(), *repo)).Intents[it.ID].FiredAt
	now := time.Now()
	if *asJSON {
		return exitOK, writeJSONIndent(w, intentReport(it, last, now))
	}
	fmt.Fprintf(w, "intent %s  (%s)\n", it.ID, it.Path)
	if it.Priority != 0 {
		fmt.Fprintf(w, "priority:   %d\n", it.Priority)
	}
	if it.Issue > 0 {
		fmt.Fprintf(w, "issue:      #%d\n", it.Issue)
	}
	if len(it.Files) > 0 {
		fmt.Fprintf(w, "files:      %s\n", strings.Join(it.Files, ", "))
	}
	if it.Acceptance != "" {
		fmt.Fprintf(w, "acceptance: %s\n", it.Acceptance)
	}
	if it.Schedule > 0 {
		fmt.Fprintf(w, "schedule:   %s (fired by `sig serve -watch`)\n", it.Schedule)
		if last.IsZero() {
			fmt.Fprintf(w, "next due:   now (never fired)\n")
		} else {
			fmt.Fprintf(w, "last fired: %s\n", last.UTC().Format(time.RFC3339))
			next := last.Add(it.Schedule)
			if effectiveFiredAt(last, now).IsZero() {
				fmt.Fprintf(w, "next due:   now (last fired is in the FUTURE: no daemon can have fired then, so it is IGNORED and this intent is due)\n")
			} else if intentDue(it, last, now) {
				fmt.Fprintf(w, "next due:   now (was due %s)\n", next.UTC().Format(time.RFC3339))
			} else {
				fmt.Fprintf(w, "next due:   %s\n", next.UTC().Format(time.RFC3339))
			}
		}
	}
	fmt.Fprintf(w, "goal:\n%s\n", indentLines(it.Goal, "  "))
	return exitOK, nil
}

// runIntentNew instantiates intents/<ID>.intent from a template.
func runIntentNew(w io.Writer, argv []string) (int, error) {
	fs := flag.NewFlagSet("intent new", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig intent new ID -repo PATH -template NAME")
		fmt.Fprintln(fs.Output(), "copy intents/templates/NAME.intent to intents/ID.intent. It is a COPY: no substitution,")
		fmt.Fprintln(fs.Output(), "no expansion — a template is an intent file like any other. An existing intent is never overwritten.")
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "path to the target git repository")
	template := fs.String("template", "", "name of the skeleton under intents/templates/ to instantiate, without the .intent extension")
	// ID is positional and documented FIRST, which stdlib flag cannot parse on
	// its own; same leading-positional pull `sig intent show` does, so both
	// orders work.
	var id string
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		id, argv = argv[0], argv[1:]
	}
	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitOK, nil
		}
		return exitOperationalError, err
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	} else if id == "" || fs.NArg() > 0 {
		return exitOperationalError, errors.New("exactly one intent ID is required")
	}
	if strings.TrimSpace(*repo) == "" {
		return exitOperationalError, errors.New("-repo is required")
	}
	if strings.TrimSpace(*template) == "" {
		return exitOperationalError, fmt.Errorf("-template is required: name a file under %s%s", intentTemplateDir(*repo), templateHint(*repo))
	}
	path, err := newIntentFromTemplate(*repo, strings.TrimSpace(*template), id)
	if err != nil {
		return exitOperationalError, err
	}
	fmt.Fprintf(w, "wrote %s from template %s\n", path, *template)
	fmt.Fprintf(w, "edit it, then run it with `sig run -repo %s -intent %s`\n", *repo, id)
	return exitOK, nil
}

// intentTemplateDir / intentTemplatePath locate the skeletons `sig intent new`
// copies from.
func intentTemplateDir(repo string) string {
	return filepath.Join(intentDir(repo), intentTemplateDirName)
}

func intentTemplatePath(repo, name string) string {
	return filepath.Join(intentTemplateDir(repo), name+intentFileExt)
}

// newIntentFromTemplate writes intents/<id>.intent from
// intents/templates/<name>.intent and returns the path it wrote.
//
// It is a COPY, deliberately and permanently: a template is written in the
// intent dialect and instantiating it substitutes nothing, includes nothing and
// expands nothing. A template mechanism that grew its own language would be a
// second dialect to learn, a second parser to trust, and a second place a
// landing bar could be described.
//
// Two guards, both the ones `sig intent import-github` already applies: the
// bytes must parse as a valid intent UNDER THE NEW ID before anything is written
// (a file this binary cannot read back is never left behind), and the create is
// O_EXCL, so an instantiation never overwrites an intent — including a
// hand-edited one that happens to share the id.
func newIntentFromTemplate(repo, name, id string) (string, error) {
	// refComponentSafe, not just slugSafe: creation is the friendly place to
	// refuse an id git will not accept as a branch. An id like "a..b" parses,
	// lists and shows perfectly well and then fails at worktree add — for a
	// SCHEDULED intent, once per window, unattended (see watchIntentCycle).
	if !refComponentSafe(id) {
		return "", fmt.Errorf("intent id %q cannot be a git branch component (allowed: A-Za-z0-9._-, no \"..\", no leading \".\" or \"-\", no trailing \".\" or \".lock\"): it becomes the branch agent/%s", id, id)
	}
	if !slugSafe(name) {
		return "", fmt.Errorf("template name %q is not a safe name (allowed: A-Za-z0-9._-, not . or ..)", name)
	}
	tpl := intentTemplatePath(repo, name)
	data, err := os.ReadFile(tpl)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("no template %q: %s does not exist%s", name, tpl, templateHint(repo))
		}
		return "", fmt.Errorf("read template %q: %w", name, err)
	}
	if _, err := parseIntent(data, id); err != nil {
		return "", fmt.Errorf("%s would not render a valid intent: %w", tpl, err)
	}
	if err := os.MkdirAll(intentDir(repo), 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", intentDir(repo), err)
	}
	path := intentPath(repo, id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	switch {
	case errors.Is(err, fs.ErrExist):
		return "", fmt.Errorf("intent %q already exists at %s: an instantiation never overwrites — delete it, or choose another id", id, path)
	case err != nil:
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", fmt.Errorf("write %s: %w", path, werr)
	}
	return path, nil
}

// templateHint names the templates that DO exist, for the error raised when the
// named one does not. Templates have no listing command of their own, so this is
// the only place the set is discoverable; an unreadable directory contributes
// nothing rather than a second error about a hint.
func templateHint(repo string) string {
	entries, err := os.ReadDir(intentTemplateDir(repo))
	if err != nil {
		return ""
	}
	var names []string
	for _, de := range entries {
		if !de.IsDir() && strings.HasSuffix(de.Name(), intentFileExt) {
			names = append(names, strings.TrimSuffix(de.Name(), intentFileExt))
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return " (available: " + strings.Join(names, ", ") + ")"
}

// ghIssue is the subset of `gh issue list --json number,title,body` this reads.
// Anything else gh emits is ignored rather than rejected: gh owns that shape and
// may grow fields.
type ghIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// ghTimeout bounds the one `gh` invocation. gh talks to the network, so it needs
// a ceiling; it is not configurable because a stuck listing is a retry, not a
// knob.
const ghTimeout = 60 * time.Second

// importedIntentID is the intent id for a GitHub issue. It is derived from the
// issue NUMBER, never the title: the number is the issue's stable identity, and
// an id that moved when someone retitled an issue would make re-import create a
// second file for the same work — the exact duplication idempotence must prevent.
func importedIntentID(number int) string { return "issue-" + strconv.Itoa(number) }

// importResult is one issue's outcome, in file order.
type importResult struct {
	Issue   int    `json:"issue"`
	ID      string `json:"id"`
	Path    string `json:"path"`
	Written bool   `json:"written"` // false => an intent file already existed and was left untouched
}

func runIntentImportGitHub(w io.Writer, argv []string) (int, error) {
	fs := flag.NewFlagSet("intent import-github", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig intent import-github -repo PATH [-label sigbound] [-limit 100] [-json]")
		fmt.Fprintln(fs.Output(), "convert labeled OPEN GitHub issues into intents/issue-<N>.intent files via the `gh` CLI.")
		fmt.Fprintln(fs.Output(), "re-import is idempotent: an intent file that already exists is SKIPPED, never overwritten.")
		fs.PrintDefaults()
	}
	repo := fs.String("repo", "", "path to the target git repository (also the directory `gh` runs in, so it resolves the same GitHub remote)")
	label := fs.String("label", "sigbound", "GitHub issue label to import")
	limit := fs.Int("limit", 100, "max issues to fetch from `gh issue list`")
	asJSON := fs.Bool("json", false, "emit JSON (stable field names; see docs/USAGE.md's Intents section)")
	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitOK, nil
		}
		return exitOperationalError, err
	}
	if strings.TrimSpace(*repo) == "" {
		return exitOperationalError, errors.New("-repo is required")
	}
	if strings.TrimSpace(*label) == "" {
		return exitOperationalError, errors.New("-label must not be empty")
	}
	if *limit < 1 {
		return exitOperationalError, fmt.Errorf("-limit must be >= 1, got %d", *limit)
	}
	issues, err := ghListIssues(context.Background(), *repo, *label, *limit)
	if err != nil {
		return exitOperationalError, err
	}
	results, err := writeImportedIntents(*repo, issues)
	if err != nil {
		// One file per issue, so a failure part-way leaves the earlier ones on
		// disk. Name them: an operator who is not told cannot tell a partial
		// import from one that did nothing, and the next run silently skips
		// whatever landed.
		for _, r := range results {
			if r.Written {
				fmt.Fprintf(w, "wrote %s before the import failed\n", r.Path)
			}
		}
		return exitOperationalError, err
	}
	if *asJSON {
		return exitOK, writeJSONIndent(w, results)
	}
	written := 0
	for _, r := range results {
		verb := "skipped (already exists)"
		if r.Written {
			verb = "wrote"
			written++
		}
		fmt.Fprintf(w, "issue #%-6d %-14s %s\n", r.Issue, verb, r.Path)
	}
	fmt.Fprintf(w, "\n%d issue(s) labeled %q: %d written, %d skipped\n", len(results), *label, written, len(results)-written)
	return exitOK, nil
}

// ghListIssues runs `gh issue list` in repo and decodes the result. gh missing
// from PATH, gh failing (unauthenticated, no GitHub remote, rate-limited), and
// gh emitting something that is not the requested JSON are each a clear,
// actionable error naming what to do — never a crash and never a silent zero
// issues, which would look exactly like "nothing is labeled".
func ghListIssues(ctx context.Context, repo, label string, limit int) ([]ghIssue, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("`sig intent import-github` shells out to the GitHub CLI, and gh is not in PATH: %w (install it from https://cli.github.com, or write intents/*.intent by hand)", err)
	}
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "issue", "list",
		"--label", label, "--state", "open", "--limit", strconv.Itoa(limit), "--json", "number,title,body")
	cmd.Dir = repo
	cmd.WaitDelay = 2 * time.Second // return promptly on cancel; see runAgent
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh issue list failed: %w: %s (is gh authenticated for this repo? run `gh auth status`)", err, tail(strings.TrimSpace(stderr.String()), 400))
	}
	var issues []ghIssue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("gh issue list did not return the requested JSON: %w", err)
	}
	return issues, nil
}

// writeImportedIntents renders each issue into intents/issue-<N>.intent, in the
// order gh returned them.
//
// IDEMPOTENCE RULE: a file that already exists is left EXACTLY as it is — the
// create is O_EXCL, so an existing file is never read, never merged, never
// overwritten, and two concurrent imports cannot both claim one path. Local
// edits therefore always survive a re-import, and a re-import of an unchanged
// issue set changes nothing on disk. The cost, stated because it is the whole
// trade: an issue EDITED after import does not re-sync. Delete the intent file
// and import again to take the new text (documented in docs/USAGE.md).
//
// An issue whose body would render an unparseable intent is rejected before
// anything is written: a file this binary cannot read back is never left behind.
func writeImportedIntents(repo string, issues []ghIssue) ([]importResult, error) {
	if len(issues) == 0 {
		return nil, nil
	}
	dir := intentDir(repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	results := make([]importResult, 0, len(issues))
	for _, iss := range issues {
		if iss.Number <= 0 {
			return results, fmt.Errorf("gh returned an issue with number %d", iss.Number)
		}
		id := importedIntentID(iss.Number)
		body := renderImportedIntent(iss)
		if _, err := parseIntent(body, id); err != nil {
			return results, fmt.Errorf("issue #%d would not render a valid intent: %w", iss.Number, err)
		}
		path := intentPath(repo, id)
		res := importResult{Issue: iss.Number, ID: id, Path: path}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		switch {
		case errors.Is(err, fs.ErrExist):
			results = append(results, res) // Written stays false: left untouched
			continue
		case err != nil:
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
		_, werr := f.Write(body)
		cerr := f.Close()
		if werr != nil {
			return nil, fmt.Errorf("write %s: %w", path, werr)
		}
		if cerr != nil {
			return nil, fmt.Errorf("write %s: %w", path, cerr)
		}
		res.Written = true
		results = append(results, res)
	}
	return results, nil
}

// renderImportedIntent renders one issue as intent-file bytes: the title as the
// first `goal` line, then the body one `goal` line per line, so the whole issue
// text reaches the agent as its prompt. Each body line is carried on its own key
// because the shared lexer is line-oriented — there is no multi-line value in
// this dialect — which also means a body line's LEADING INDENTATION is lost when
// the file is read back (values are edge-trimmed). Prose survives; the exact
// layout of an indented code block does not.
func renderImportedIntent(iss ghIssue) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# intents/%s.intent — imported from GitHub issue #%d by `sig intent import-github`.\n", importedIntentID(iss.Number), iss.Number)
	b.WriteString("# Edit freely: re-importing SKIPS a file that already exists (docs/USAGE.md, Intents).\n")
	fmt.Fprintf(&b, "issue = %d\n", iss.Number)
	for _, line := range append([]string{iss.Title, ""}, strings.Split(iss.Body, "\n")...) {
		fmt.Fprintf(&b, "goal = %s\n", strings.TrimSuffix(line, "\r"))
	}
	return []byte(b.String())
}

// firstLine is the one-line summary of a multi-line goal for `sig intent list`.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// indentLines prefixes every line of s, so `sig intent show`'s goal block is
// visibly one field rather than free text at the left margin.
func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
