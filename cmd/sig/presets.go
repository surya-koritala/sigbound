// Named presets for -agent/-repair/-planner/-verify (issue #17) and -publish
// (issue #116): a known-good sh -c command for a harness/ecosystem/host CLI,
// keyed by a short name, so wiring up the SIGBOUND_* env by hand (see
// examples/README.md and docs/USAGE.md) is
// optional rather than the only way in. A preset encodes only the harness's
// CLI shape (how to invoke it non-interactively), never the model — bring
// your own model is unaffected. There is deliberately no -resolver-preset:
// the built-in union-resolver example in the README is repo-specific, not a
// generic wiring, so it stays out of scope here.
package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// agentPresets are -agent's known-good sh -c wiring, selected by
// -agent-preset=NAME. Mirrors the `claude` wiring docs/USAGE.md and
// examples/README.md already hand-write; codex/aider get the same shape
// with their own non-interactive/auto-apply flags.
var agentPresets = map[string]string{
	"claude": `claude -p --permission-mode acceptEdits "$SIGBOUND_TASK"`,
	"codex":  `codex exec --full-auto "$SIGBOUND_TASK"`,
	"aider":  `aider --yes --message "$SIGBOUND_TASK"`,
}

// repairPresets are -repair's known-good wiring, selected by
// -repair-preset=NAME. Same shape as agentPresets, wrapping SIGBOUND_FAILURE
// in the same "fix this build failure" framing the README's -repair example
// uses.
var repairPresets = map[string]string{
	"claude": `claude -p --permission-mode acceptEdits "Fix this build failure: $SIGBOUND_FAILURE"`,
	"codex":  `codex exec --full-auto "Fix this build failure: $SIGBOUND_FAILURE"`,
	"aider":  `aider --yes --message "Fix this build failure: $SIGBOUND_FAILURE"`,
}

// plannerPresets are -planner's known-good wiring, selected by
// -planner-preset=NAME. The planner only needs to print the plan JSON to
// stdout (see DefaultPlanPrompt) — it never edits files — so these use each
// harness's plain non-interactive print mode rather than its auto-apply-edits
// flags (contrast agentPresets' --full-auto / --permission-mode acceptEdits).
var plannerPresets = map[string]string{
	"claude": `claude -p "$SIGBOUND_PROMPT"`,
	"codex":  `codex exec "$SIGBOUND_PROMPT"`,
	"aider":  `aider --yes --message "$SIGBOUND_PROMPT"`,
}

// verifyPresets are -verify's known-good command per ecosystem or scanner,
// selected by -verify-preset=NAME. Go's build+test is backed by RepoMap's
// exported-decl scan (plan.go); RepoMap has no equivalent detection for
// node/python/rust — those three are just the idiomatic one-liner for that
// ecosystem.
//
// The security names (govulncheck/gitleaks/codeql) run a scanner INSTEAD of a
// build+test, so they are meant to COMPOSE with one rather than replace it: put
// the build+test members in the repo's sigbound.policy and pass the scanner as
// -verify-preset, and resolvePolicy runs the policy's members first and appends
// the scanner last. Nothing here changes that ordering — a preset is an ordinary
// flag verify by the time policy resolution sees it.
var verifyPresets = map[string]string{
	"go":     "go build ./... && go test ./...",
	"node":   "npm test",
	"python": "python -m pytest",
	"rust":   "cargo build && cargo test",

	"govulncheck": securityVerify("govulncheck", "go install golang.org/x/vuln/cmd/govulncheck@latest", "govulncheck ./..."),
	"gitleaks":    securityVerify("gitleaks", "brew install gitleaks, or a release binary from https://github.com/gitleaks/gitleaks", gitleaksScan),
	"codeql":      securityVerify("codeql", "download the CodeQL CLI from https://github.com/github/codeql-cli-binaries/releases", codeqlScan),
}

// gitleaksScan scans the WORKING TREE of the verify checkout (--no-git), not the
// repo's history: the gate decides what this landing may put on the base branch,
// and a history scan would instead fail every run for as long as any ancestor
// commit contains a secret — a gate nobody can turn green is one that gets
// switched off. --redact keeps the matched value out of stdout, which sigbound
// captures into the run report — and from there into the manifest, any git note
// -notes attaches, and what `sig log` replays — plus -logdir's full log; a gate
// that copies the secret it found into all of those is its own incident.
// gitleaks exits non-zero on any finding, and that is what fails the gate.
const gitleaksScan = "gitleaks detect --no-git --redact"

// codeqlScan builds a database from the verify checkout and analyzes it with the
// CLI's default query suite for the language. Two parts are deliberate:
//
//   - the language, which CodeQL cannot infer for us. It is SIGBOUND_CODEQL_LANG,
//     defaulting to go. LIMIT: under -env-mode scoped that variable reaches the
//     verify command only if -env-verify allowlists it — unset, this scans go.
//   - the results check. `codeql database analyze` exits 0 whether or not it
//     found anything, so WITHOUT the CSV test this member would report green over
//     real findings. An analyze that wrote no file at all also fails ([ -f ]),
//     rather than passing as "nothing found" — the file's absence means the scan
//     did not happen, which is exactly the outcome that must never read as green.
//
// The database is built inside the checkout: runVerify runs `git clean -fdx`
// before every verify attempt, so it never survives into another one, and the
// checkout is a throwaway worktree that is never what lands.
const codeqlScan = `codeql database create .sigbound-codeql-db --language="${SIGBOUND_CODEQL_LANG:-go}" --overwrite && ` +
	`codeql database analyze .sigbound-codeql-db --format=csv --output=.sigbound-codeql.csv && ` +
	`[ -f .sigbound-codeql.csv ] && [ ! -s .sigbound-codeql.csv ]`

// securityVerify composes one scanner preset: a PATH check for the tool, then
// the scan. The check is there for the FAILURE MODE, which in a landing gate
// matters more than the scan itself — an ABSENT tool must FAIL the gate, never
// skip it. A member that skipped would let the run report green over a tree
// nothing scanned, and a receipt claiming a scan that never ran is worse than
// having no scanner at all. So this must never be written as
// `command -v TOOL || exit 0`, nor as a scan suffixed with `|| true`.
//
// HONEST LIMIT on what the check itself buys: `sh -c` already fails a missing
// command with exit 127, so the prefix does not change the VERDICT — absence
// failed the gate before it existed. What it adds is the MESSAGE: the tool's
// name and how to install it, in place of the shell's bare "not found", which
// tells a user staring at a red gate nothing about which of an N-member battery
// went missing or what to do next.
func securityVerify(tool, install, scan string) string {
	return fmt.Sprintf(`command -v %s >/dev/null 2>&1 || { echo "sigbound -verify-preset=%s: %s is not on PATH (install: %s) -- failing the gate rather than skipping the scan and reporting green" >&2; exit 1; }; %s`,
		tool, tool, tool, install, scan)
}

// publishPresets are -publish's known-good wiring, selected by
// -publish-preset=NAME: push the landed base branch to the remote and open a
// pull/merge request whose BODY is this run's receipt (see receiptBody), or, on
// every run after the first, comment that receipt onto the request already
// open. The landing stays local until a human merges that request — the receipt
// is the provenance record attached to it, so what sigbound did is reviewable
// where the team already reviews.
//
// These are the only presets in this file that run AFTER the landing gate.
// Nothing here can un-land a landing: by the time -publish runs, the base ref
// has already advanced. A receipt that fails — no gh/glab, an unauthenticated
// one, a rejected push, a host outage — leaves the landed tree and verify's
// verdict exactly as they are. Its message is captured in
// report.publish.output (runPublish records the command's own stdout+stderr;
// it never streams them) and the run exits 6 rather than 0 — unless a
// higher-precedence condition already claimed the code, e.g. 4 for a run that
// landed with a branch flagged (see runExitCode; publish is checked last).
//
// The body travels as an ENVIRONMENT VARIABLE and is referenced inside double
// quotes, which is what keeps it inert. It is arbitrary user prose — task
// prompts are free text written for an LLM — and sh does not re-scan the
// result of a parameter expansion for metacharacters, so a prompt containing
// `'; rm -rf ~; echo '` is one --body argument and never a command. These
// strings are constants: no run data is interpolated into the command itself.
//
// HONEST LIMIT: the receipt is a request DESCRIPTION, and both hosts act on
// closing keywords there. A task prompt containing "Closes #12" will close
// issue 12 when the receipt request merges. That is the issue-close linkage
// issue #116 asks for when the run came from an imported issue — and it fires
// just the same for a prompt that only happens to mention an issue number.
var publishPresets = map[string]string{
	"github-receipt": receiptPublish("github-receipt", "gh", "https://cli.github.com", "pull request", "GH_TOKEN",
		`gh pr create --base "$sb_target" --head "$SIGBOUND_BASE_BRANCH" --title "sigbound: $SIGBOUND_LANDED" --body "$SIGBOUND_RECEIPT"`,
		`gh pr comment "$SIGBOUND_BASE_BRANCH" --body "$SIGBOUND_RECEIPT"`),
	"gitlab-receipt": receiptPublish("gitlab-receipt", "glab", "https://gitlab.com/gitlab-org/cli", "merge request", "GLAB_TOKEN",
		`glab mr create --source-branch "$SIGBOUND_BASE_BRANCH" --target-branch "$sb_target" --title "sigbound: $SIGBOUND_LANDED" --description "$SIGBOUND_RECEIPT" --yes`,
		`glab mr note "$SIGBOUND_BASE_BRANCH" --message "$SIGBOUND_RECEIPT"`),
}

// receiptStands closes every receipt failure message. The person reading one is
// looking at a red publish step wondering what happened to their work, and the
// answer — nothing — is the first thing they need.
const receiptStands = ` -- the landing already happened and still stands; only this receipt is missing`

// receiptPublish composes one receipt preset: four preflight checks, then push,
// then open — or, once a request is already open, comment on it. `create` and
// `comment` are the host's two commands; both read $sb_target (the remote's
// default branch, resolved below), and pr/token name what that host calls the
// thing and its CI token, for the messages.
//
// CREATE, ELSE COMMENT is what makes the STEADY STATE work, and it is the whole
// reason this is not just `create`. These presets require -base to be a
// long-lived integration branch, so run 1 opens the request and run 2 onward
// would hit "a pull request for branch X already exists" forever: exit 6 every
// time, and — worse than the red — run N's receipt posted NOWHERE while the
// request body still described run 1. Falling back to a comment makes the
// steady state additive: the request accumulates one receipt per run, in order,
// where a reviewer reading that request will find them.
//
// The fallback is unconditional (`create || comment`) rather than matched
// against the "already exists" text, because neither CLI gives that case its
// own exit code — gh exits 1 for it exactly as for a permissions failure — and
// matching English error prose from another tool is the kind of thing that
// breaks silently on their next release. The cost of being unconditional is
// small and self-correcting: if create failed for some OTHER reason, the
// comment almost always fails too (no request to comment on), and BOTH errors
// end up in report.publish.output. Create-first also gets the lifecycle right
// in a way comment-first does not — once the previous receipt request is
// MERGED, create opens a fresh one, whereas `gh pr comment <branch>` would
// happily keep appending to the merged one.
//
// Every check FAILS LOUDLY rather than skipping, the same discipline
// securityVerify has for an absent scanner and for the same reason: a skipped
// receipt leaves report.publish claiming ok on a run whose landing nobody was
// ever told about. So never soften one into `|| exit 0`, and never suffix the
// chain with `|| true`.
//
//   - CLI on PATH, naming the install.
//   - CLI authenticated. The message names -env-publish TOKEN as well as `auth
//     login`, because the standard CI shape hits this for the second reason:
//     -env-mode scoped drops GH_TOKEN/GITHUB_TOKEN/GLAB_TOKEN/SSH_AUTH_SOCK
//     (they are not in baseEnvNames), so a GitHub Actions run reads a message
//     telling it to run an interactive login it cannot run. HONEST LIMIT:
//     `auth status` checks credentials, not permissions — a token that cannot
//     open a request on this repo passes here and fails at the create,
//     reported the same way.
//   - The remote's default branch is readable. `git ls-remote --symref` asks the
//     remote itself, which is host-agnostic (one expression serves both presets)
//     and needs no JSON parsing out of either CLI. It is resolved ONCE and used
//     for both the guard below and the request's base, so the branch checked is
//     exactly the branch targeted — passing --base explicitly also sidesteps
//     gh's `gh-merge-base` git-config fallback, which could otherwise retarget
//     the request at something this never inspected.
//   - The landed branch is NOT that default branch. Neither host can open a
//     request from a branch onto itself, and this catches it BEFORE the push
//     rather than leaving a pushed branch and a failed create behind. `-base
//     main` on a repo whose default is main is the common shape of this: there
//     is no request to open, the push IS the publish.
//
// The remote is ${SIGBOUND_REMOTE:-origin} — the same ${VAR:-default} shape
// codeqlScan uses for SIGBOUND_CODEQL_LANG, and with the same limit: under
// -env-mode scoped it reaches this command only if -env-publish allowlists it,
// so unset, this uses origin (which is what every -publish example in
// docs/USAGE.md hardcodes; no Go code in this repo assumes a remote name at
// all). The push is an ordinary fast-forward push and is never forced: a remote
// branch that has moved underneath this run rejects it, loudly, as it should.
func receiptPublish(preset, tool, install, pr, token, create, comment string) string {
	return fmt.Sprintf(`command -v %[2]s >/dev/null 2>&1 || { echo "sigbound -publish-preset=%[1]s: %[2]s is not on PATH (install: %[3]s)`+receiptStands+`" >&2; exit 1; }; `+
		`%[2]s auth status >/dev/null 2>&1 || { echo "sigbound -publish-preset=%[1]s: %[2]s is not authenticated (run: %[2]s auth login -- or, under -env-mode scoped, pass -env-publish %[5]s: the scoped base env does not carry %[5]s or SSH_AUTH_SOCK)`+receiptStands+`" >&2; exit 1; }; `+
		`sb_remote=${SIGBOUND_REMOTE:-origin}; `+
		`sb_target=$(git ls-remote --symref "$sb_remote" HEAD | awk '$1=="ref:" && $3=="HEAD"{sub("refs/heads/","",$2); print $2; exit}'); `+
		`[ -n "$sb_target" ] || { echo "sigbound -publish-preset=%[1]s: cannot read the default branch of remote $sb_remote (git ls-remote failed; set SIGBOUND_REMOTE if the remote is not named origin)`+receiptStands+`" >&2; exit 1; }; `+
		`[ "$sb_target" != "$SIGBOUND_BASE_BRANCH" ] || { echo "sigbound -publish-preset=%[1]s: -base $SIGBOUND_BASE_BRANCH IS the default branch of remote $sb_remote, and a %[4]s cannot be opened from a branch onto itself -- re-run with -base on an integration branch, or drop the preset and publish with -publish 'git push $sb_remote $SIGBOUND_BASE_BRANCH'`+receiptStands+`" >&2; exit 1; }; `+
		// The branch and its provenance note go in ONE --atomic transaction, so
		// the remote ends up with the landing AND its evidence, or with neither.
		// Pushing them separately leaves a window where the branch has moved and
		// the note has not — and "did this run already land?" is answered by
		// fetching the base and looking for that run's note, so a caller
		// recovering from an interrupted run reads "no" and re-runs work that
		// already landed, paying for it twice. A plain network failure between two
		// pushes is enough to reach it.
		//
		// The notes ref is only included when it exists locally: -notes can be off,
		// and naming a missing ref would fail every publish for a repo that never
		// wrote one. --porcelain makes the failure machine-readable — git prints
		// one status line per ref, so "which ref was refused" is read off git's own
		// output rather than guessed at from prose. That matters for the trap this
		// creates: a token allowed to move the branch but not refs/notes/* turns a
		// working landing into a failing publish, and the error has to say which
		// ref it was.
		//
		// --atomic needs support on BOTH ends. An ancient server that lacks it
		// fails loudly here rather than silently degrading into two pushes and
		// reintroducing the window the flag exists to close.
		`sb_refs="$SIGBOUND_BASE_BRANCH"; `+
		`git show-ref --verify --quiet refs/notes/sigbound && sb_refs="$sb_refs refs/notes/sigbound"; `+
		`git push --atomic --porcelain "$sb_remote" $sb_refs || { echo "sigbound -publish-preset=%[1]s: atomic push of [$sb_refs] to $sb_remote failed -- NOTHING was pushed (--atomic moves every ref or none). `+
		`A rejected refs/notes/sigbound usually means the token can write branches but not refs/notes/*; a rejected branch usually means it moved underneath this run, so re-run against the new head`+receiptStands+`" >&2; exit 1; }; `+
		`{ %[6]s || { echo "sigbound -publish-preset=%[1]s: could not open a new %[4]s (usually because one is already open for $SIGBOUND_BASE_BRANCH); posting this run's receipt on the existing one instead" >&2; %[7]s; }; }`,
		preset, tool, install, pr, token, create, comment)
}

// receiptPromptMax/receiptMaxItems bound the receipt. EVERY variable-length
// thing in it is arbitrary text of unbounded length — task prompts and ids are
// prose an LLM wrote, branch names and file paths come from a plan it also
// wrote — and this string is handed to a child process as ONE environment
// variable (Linux caps a single one at 128 KiB) and then posted as a request
// body (GitHub caps one at 65,536 characters). So there is no unbounded path
// through receiptBody: every list is cut to receiptMaxItems entries and every
// entry to receiptPromptMax runes, with whatever was left out COUNTED rather
// than silently dropped. Worst case is roughly 25k characters.
const (
	receiptPromptMax = 200
	receiptMaxItems  = 20
)

// receiptBody renders the receipt every -publish command receives as
// SIGBOUND_RECEIPT (see runPublish): what landed, from which run, under which
// verdict, and — the part a human is actually being asked about — WHAT DID NOT.
// It reports ONLY what the report already recorded, provenance for a landing
// that has already happened, never a second opinion about it. Markdown, because
// both hosts render a request body as Markdown and it degrades to readable
// plain text anywhere else.
//
// The not-landed lines are the point of the artifact, not decoration. A run can
// land its clean groups and PARK one (see park.go): the base advanced, so
// -publish fires, and the receipt is then the document in front of the person
// whose ack the parked group is waiting on. A receipt that reported "2 of 2
// agents succeeded" and stopped would be describing a run that is still holding
// work back — the reader would have to already know to go look. Same for a
// conflict-flagged branch and for a bisect-dropped one.
//
// There is no run-level "goal" to print: -goal is planned into tasks before
// driveRun ever sees it, and -intent becomes a single task whose prompt IS the
// intent's goal (see runRun). The task prompts are therefore the honest answer
// to "what was this asked to do", bounded per the constants above.
func receiptBody(runID string, rep runReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**sigbound receipt** — landed `%s` onto `%s`.\n\n", short(rep.Integrate.FinalSHA), rep.Base)
	if id := strings.TrimSpace(runID); id != "" {
		fmt.Fprintf(&b, "- run: `%s`\n", id)
	}
	if id := strings.TrimSpace(rep.Intent); id != "" {
		fmt.Fprintf(&b, "- intent: `%s`\n", id)
	}
	fmt.Fprintf(&b, "- landed sha: `%s` (base was `%s`)\n", rep.Integrate.FinalSHA, rep.BaseSHA)
	fmt.Fprintf(&b, "- verify: %s\n", receiptVerdict(rep.Verify))
	okAgents := 0
	for _, a := range rep.PerAgent {
		if a.OK {
			okAgents++
		}
	}
	fmt.Fprintf(&b, "- agents: %d of %d succeeded; landed: %s\n", okAgents, len(rep.PerAgent), receiptItems(rep.Integrate.Landed))

	// PARKED first among the outcomes: it is the only one that is waiting on
	// this reader to do something.
	if pk := rep.Park; pk != nil {
		fmt.Fprintf(&b, "- **PARKED, AWAITING ACK** (%s): `%s` verified GREEN but was deliberately NOT landed — %s. Held branches: %s. Triggering paths: %s\n",
			receiptAckWindow(pk), short(pk.VerifiedSHA), receiptPrompt(pk.Reason),
			receiptItems(pk.branches()), receiptItems(sortedKeys(pk.matchedPaths())))
	}
	if f := rep.Integrate.Flagged; len(f) > 0 {
		branches := make([]string, 0, len(f))
		var paths []string
		for _, x := range f {
			branches = append(branches, x.Branch)
			paths = append(paths, x.Paths...)
		}
		fmt.Fprintf(&b, "- **NOT LANDED** — %d branch(es) flagged as conflicts and withheld: %s (paths: %s)\n",
			len(f), receiptItems(branches), receiptItems(paths))
	}
	if d := rep.Integrate.DroppedByBisect; len(d) > 0 {
		fmt.Fprintf(&b, "- **NOT LANDED** — %d branch(es) dropped by -verify-bisect (their group broke the combined tree): %s\n",
			len(d), receiptItems(d))
	}
	// A refused landing means nothing landed, so -publish never fires and this
	// line cannot appear on a real receipt today. It is here because
	// receiptBody is a pure function of the report and a receipt that could
	// silently omit "somebody else landed first" would be the exact failure the
	// lines above exist to prevent, the moment any caller renders a report this
	// function did not gate.
	if s := strings.TrimSpace(rep.LandRefused); s != "" {
		fmt.Fprintf(&b, "- **LANDING REFUSED** — the base moved to `%s` while this run was computing against `%s`; nothing landed\n", short(s), short(rep.BaseSHA))
	}

	b.WriteString("- tasks:\n")
	for i, t := range rep.Tasks {
		if i == receiptMaxItems {
			fmt.Fprintf(&b, "  - … and %d more\n", len(rep.Tasks)-receiptMaxItems)
			break
		}
		fmt.Fprintf(&b, "  - `%s`: %s\n", receiptPrompt(t.ID), receiptPrompt(t.Prompt))
	}
	fmt.Fprintf(&b, "\nsigbound %s, run started %s.\n", rep.Version, rep.StartedAt)
	return b.String()
}

// receiptAckWindow says whether the park expires on its own. "no timeout" is
// the default and the honest word for it: an unacked landing is not a problem
// time solves, so the receipt must not imply somebody else will handle it.
func receiptAckWindow(pk *parkJSON) string {
	if pk.AckTimeout == "" || pk.AckTimeoutAction == "" {
		return "no ack timeout — it waits for a human"
	}
	return "ack within " + pk.AckTimeout + ", else " + pk.AckTimeoutAction
}

// receiptItems renders one bounded, backticked list — landed branches, held
// branches, flagged branches, triggering paths. Bounded twice, because both
// dimensions are attacker-shaped in the sense that matters here: a plan can name
// 500 branches, and nothing validates how long any one id or path is.
func receiptItems(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	more := 0
	if len(names) > receiptMaxItems {
		more, names = len(names)-receiptMaxItems, names[:receiptMaxItems]
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + receiptPrompt(n) + "`"
	}
	out := strings.Join(quoted, ", ")
	if more > 0 {
		out += fmt.Sprintf(", … and %d more", more)
	}
	return out
}

// receiptVerdict states -verify's verdict for the receipt. "not configured" is
// its own answer, distinct from a pass: a receipt reading "verify: passed" on a
// run with no -verify would claim a check that never ran.
func receiptVerdict(v verifyJSON) string {
	switch {
	case !v.Ran:
		return "not configured for this run"
	case v.OK && v.Repaired:
		return "passed (after repair)"
	case v.OK:
		return "passed"
	default:
		return "FAILED"
	}
}

// receiptPrompt reduces one task prompt to a bounded single line: first line
// only (reusing firstLine, `sig intent list`'s own summarizer), cut on a rune
// boundary so a multi-byte prompt can't be truncated into invalid UTF-8.
func receiptPrompt(s string) string {
	s = strings.TrimSpace(firstLine(strings.TrimSpace(s)))
	if r := []rune(s); len(r) > receiptPromptMax {
		return string(r[:receiptPromptMax]) + "..."
	}
	return s
}

// presetSlot resolves one -X / -X-preset flag pair. A raw cmd, when already
// set, always wins — the preset name isn't even looked up, so a stray or
// misspelled -X-preset never breaks a run that already supplies its own -X
// (documented in each -X-preset flag's usage text). Otherwise an empty
// preset is a no-op (both left "", e.g. optional -repair-preset), and a set
// preset resolves via table or fails loudly listing every valid name. A
// newly-expanded command is announced once to stderrW — cmdFlag/presetFlag
// name the flags in that message — so the user can see and copy exactly what
// will run.
func presetSlot(stderrW io.Writer, table map[string]string, cmd, preset, cmdFlag, presetFlag string) (string, error) {
	if strings.TrimSpace(cmd) != "" {
		return cmd, nil
	}
	if strings.TrimSpace(preset) == "" {
		return cmd, nil
	}
	expanded, ok := table[preset]
	if !ok {
		names := make([]string, 0, len(table))
		for n := range table {
			names = append(names, n)
		}
		sort.Strings(names)
		return "", fmt.Errorf("%s: unknown preset %q (want one of: %s)", presetFlag, preset, strings.Join(names, ", "))
	}
	fmt.Fprintf(stderrW, "%s=%s expands %s to: %s\n", presetFlag, preset, cmdFlag, expanded)
	return expanded, nil
}

// applyPresets expands every -*-preset flag into its known-good command,
// raw-wins per presetSlot, BEFORE the rest of runRun consumes the resulting
// agent/repair/planner/verify commands — so e.g. -agent-preset alone (no
// -agent) satisfies runRun's "-agent is required" check exactly like a
// hand-written -agent would. stderrW is where each expansion is announced
// (os.Stderr in production; a buffer in tests). Returns the resolved
// agent/repair/planner/verify commands, or the first unknown-preset error
// encountered (agent, then repair, then planner, then verify).
//
// -publish-preset is deliberately NOT resolved here: nothing between this call
// and runParams reads -publish, so it goes through presetSlot at its own call
// site in runRun rather than widening this signature by another pair. Both
// paths share presetSlot, so raw-wins and the unknown-name error are identical.
func applyPresets(stderrW io.Writer, agentCmd, agentPreset, repairCmd, repairPreset, plannerCmd, plannerPreset, verifyCmd, verifyPreset string) (newAgentCmd, newRepairCmd, newPlannerCmd, newVerifyCmd string, err error) {
	if newAgentCmd, err = presetSlot(stderrW, agentPresets, agentCmd, agentPreset, "-agent", "-agent-preset"); err != nil {
		return "", "", "", "", err
	}
	if newRepairCmd, err = presetSlot(stderrW, repairPresets, repairCmd, repairPreset, "-repair", "-repair-preset"); err != nil {
		return "", "", "", "", err
	}
	if newPlannerCmd, err = presetSlot(stderrW, plannerPresets, plannerCmd, plannerPreset, "-planner", "-planner-preset"); err != nil {
		return "", "", "", "", err
	}
	if newVerifyCmd, err = presetSlot(stderrW, verifyPresets, verifyCmd, verifyPreset, "-verify", "-verify-preset"); err != nil {
		return "", "", "", "", err
	}
	return newAgentCmd, newRepairCmd, newPlannerCmd, newVerifyCmd, nil
}
