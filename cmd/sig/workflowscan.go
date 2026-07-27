// Line-oriented extraction of the `run:` steps from a GitHub Actions workflow,
// for `sig policy init` (issue #148).
//
// THIS IS NOT A YAML PARSER, and no YAML parser is going to appear here:
// sigbound has zero module dependencies (go.sum is empty; see the note at the
// head of cmd/sig/config.go about why sig.conf is not TOML) and is keeping it.
// What follows is a HEURISTIC over the common workflow shapes — key lines,
// `- ` sequence items, and `|` block scalars, tracked by leading-space column.
// Anchors, aliases, flow mappings, multi-document streams, tags and merge keys
// are not understood at all.
//
// The consequence is stated once and holds everywhere below: a shape this
// scanner does not recognize produces FEWER suggestions, never a wrong one.
// Every refusal becomes a note the draft prints verbatim, so what was skipped
// is visible to the human reviewing the file rather than silently missing. A
// verify member that is subtly wrong is the worst output this command could
// produce — a mangled command that exits 0 is a landing bar that gates nothing
// while looking green — so every ambiguity below resolves to "emit nothing and
// say so".
package main

import (
	"fmt"
	"sort"
	"strings"
)

// wfCommand is one `run:` step the scanner is willing to translate into a
// verify member. Cmd is guaranteed non-empty and free of newlines (a newline
// would end the `verify = ...` line early — see parseConfigFile).
type wfCommand struct {
	Line int    // 1-based line of the `run:` key it came from
	Job  string // job name, or "" when the file's jobs nesting was not recognized
	Cmd  string
}

// wfNote is one construct the scanner saw and did NOT translate. Detail is a
// single line; Quote holds source lines reproduced verbatim underneath it (the
// refused command, so a human can fold it into one line by hand). Neither may
// contain a newline — the draft writer enforces that (see commentLines).
type wfNote struct {
	Detail string
	Quote  []string
}

// wfScan is the result of scanning one workflow file.
type wfScan struct {
	Commands []wfCommand
	Notes    []wfNote
}

func (sc *wfScan) note(format string, a ...any) {
	sc.Notes = append(sc.Notes, wfNote{Detail: fmt.Sprintf(format, a...)})
}

func (sc *wfScan) noteQuoted(quote []string, format string, a ...any) {
	sc.Notes = append(sc.Notes, wfNote{Detail: fmt.Sprintf(format, a...), Quote: quote})
}

// wfMaxBytes is the size past which a workflow file is not scanned at all. It
// is readForMap's (plan.go) cap, reused rather than given a flag of its own: a
// workflow that large is not a shape this scanner was written for.
const wfMaxBytes = 512 * 1024

// shellKeywords are the words that, at the start of a line inside a `run: |`
// block, mean the block is a shell PROGRAM rather than a list of commands.
// Joining such a block with " && " would produce something that is not the same
// program, so the block is refused whole. This scanner does not parse shell and
// is not going to start.
var shellKeywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true,
	"for": true, "while": true, "until": true, "do": true, "done": true,
	"case": true, "esac": true, "select": true, "function": true,
	"{": true, "}": true, "(": true, ")": true,
}

// scanWorkflow extracts the usable `run:` commands from one workflow file's
// bytes. path is used only in note text. It never errors and never panics on
// arbitrary bytes (FuzzScanWorkflow): unrecognized input yields an empty scan
// plus notes.
func scanWorkflow(path string, data []byte, defaultBranch string) wfScan {
	var sc wfScan
	if len(data) > wfMaxBytes {
		sc.note("%s is %d bytes (cap %d) — not scanned", path, len(data), wfMaxBytes)
		return sc
	}
	lines := textLines(data)
	// A tab in the indentation makes every column comparison below meaningless
	// (and is invalid YAML anyway), so the file contributes nothing rather than
	// a battery assembled from nesting that was guessed wrong.
	for i, ln := range lines {
		if _, tab := indentOf(ln); tab {
			sc.note("%s:%d has a tab in its indentation — this scanner tracks nesting by leading spaces and cannot trust the file", path, i+1)
			return sc
		}
	}

	trig, trigLine := workflowTriggers(lines, defaultBranch)
	switch {
	case trigLine == 0:
		sc.note("%s has no top-level `on:` key — cannot tell whether it gates a merge", path)
		return sc
	case !trig["push"] && !trig["pull_request"]:
		names := make([]string, 0, len(trig))
		for n := range trig {
			names = append(names, n)
		}
		sort.Strings(names)
		sc.note("%s:%d on=[%s] — neither push nor pull_request, so it is not a landing bar; for a run cadence see watch-interval/watch-batch/watch-max-red instead", path, trigLine, strings.Join(names, " "))
		return sc
	}

	// WORKFLOW-level `defaults:` and `env:` are the same hazard as their job-level
	// twins (scanJob refuses those), one scope up: GitHub applies them to every
	// step in every job, so `defaults.run.working-directory` silently relocates
	// every command — the canonical Actions idiom for a monorepo whose code lives
	// in a subdirectory — and a top-level `env:` supplies variables a verify
	// command would not have. Emitting a step's text under either would emit a
	// command that runs somewhere else, or without its environment: a bar that
	// passes at the repository root while the real CI fails in the subdirectory.
	// The whole file is refused, because the relocation applies to all of it.
	for _, k := range []string{"defaults", "env"} {
		if at, _ := topLevelBlock(lines, k); at >= 0 {
			sc.note("%s:%d workflow-level `%s:` applies to every step in every job — it can relocate the working directory or supply variables a verify command would not have, so the file contributes nothing", path, at+1, k)
			return sc
		}
	}

	jobsAt, jobsEnd := topLevelBlock(lines, "jobs")
	if jobsAt < 0 {
		sc.note("%s has no top-level `jobs:` key — nothing to read run steps from", path)
		return sc
	}
	jobIndent := firstIndent(lines, jobsAt+1, jobsEnd)
	if jobIndent < 0 {
		sc.note("%s:%d `jobs:` is empty", path, jobsAt+1)
		return sc
	}
	// Split the jobs block at every key line sitting exactly at the first job's
	// column. Anything deeper belongs to the job above it.
	starts := []int{}
	for i := jobsAt + 1; i < jobsEnd; i++ {
		if blankOrComment(lines[i]) {
			continue
		}
		if n, _ := indentOf(lines[i]); n != jobIndent {
			continue
		}
		if _, _, _, item, ok := splitKey(lines[i]); ok && !item {
			starts = append(starts, i)
		}
	}
	for k, s := range starts {
		e := jobsEnd
		if k+1 < len(starts) {
			e = starts[k+1]
		}
		name, _, _, _, _ := splitKey(lines[s])
		sc.scanJob(path, name, lines, s+1, e)
	}
	return sc
}

// scanJob reads one job's body (lines[s:e], the lines below its name). It
// refuses the whole job for constructs that change what its steps MEAN —
// a container/services environment this run does not provide, job-level env
// the verify command would not have, a defaults block that redirects the
// working directory — because emitting such a step's command would emit a
// command that runs somewhere else.
func (sc *wfScan) scanJob(path, name string, lines []string, s, e int) {
	keyIndent := firstIndent(lines, s, e)
	if keyIndent < 0 {
		return
	}
	stepsAt := -1
	linuxConfirmed := false
	for i := s; i < e; i++ {
		if blankOrComment(lines[i]) {
			continue
		}
		if n, _ := indentOf(lines[i]); n != keyIndent {
			continue
		}
		key, val, _, item, ok := splitKey(lines[i])
		if !ok || item {
			continue
		}
		// AFFIRMATIVE CONFIRMATION, not enumerated refusal (issue #158). Every key
		// a job may carry has to be one this scanner has actually reasoned about;
		// anything else disqualifies the job.
		//
		// The old shape was the inverse — draft from every job, then refuse the
		// constructs known to break it — and it needed four rounds of additions at
		// four different scopes, each found the same way: a construct nobody had
		// enumerated produced a live `verify` member that exits 0 on broken code.
		// That sequence has no end, because it is a list of everything GitHub
		// Actions will ever add.
		//
		// The asymmetry is what makes inverting cheap. A refused workflow falls
		// through to the Makefile and manifest sources, which draft a real battery
		// — so OVER-refusing costs a less specific draft, while UNDER-refusing
		// costs a landing bar that reports green on failing code.
		switch key {
		case "runs-on":
			// The battery is drafted for the machine verify runs on, so linux must
			// be CONFIRMED rather than merely not-contradicted. A missing runs-on
			// used to be accepted on the grounds that many workflows rely on a
			// default; under inversion that is exactly the unrecognized input this
			// change exists to stop trusting.
			if refuse := runsOnRefusal(val); refuse != "" {
				sc.note("%s:%d job %q %s", path, i+1, name, refuse)
				return
			}
			linuxConfirmed = true
		case "strategy":
			// A matrix runs the SAME step text N times over different values.
			// Emitting that text once is faithful; a step that interpolates a
			// matrix value is refused separately, by the ${{ }} rule.
			sc.note("%s:%d job %q has `strategy:` — a matrix leg is not represented; its steps are emitted once", path, i+1, name)
		case "steps":
			stepsAt = i
		case "name", "needs", "timeout-minutes", "permissions", "outputs", "concurrency":
			// Reasoned about and harmless to a drafted battery: none of them changes
			// WHETHER the steps run, WHERE they run, or what environment they see.
			// `needs` only orders jobs — a job that runs after a dependency
			// succeeded still gates the merge. `permissions` scopes the workflow
			// token, which a build-and-test command does not use.
		default:
			// Includes every key the old deny-list enumerated (container, services,
			// environment, env, defaults, if, continue-on-error) and, far more
			// importantly, every key nobody has thought about yet.
			sc.note("%s:%d job %q has `%s:`, which this scanner has not confirmed is safe for a landing bar — the job contributes nothing", path, i+1, name, key)
			return
		}
	}
	if stepsAt < 0 {
		return
	}
	if !linuxConfirmed {
		sc.note("%s job %q does not say `runs-on:` — the runner OS is unconfirmed, and a battery drafted for the wrong machine is a bar that does not run where verify does, so the job contributes nothing", path, name)
		return
	}
	stepsEnd := e
	for i := stepsAt + 1; i < e; i++ {
		if blankOrComment(lines[i]) {
			continue
		}
		if n, _ := indentOf(lines[i]); n <= keyIndent {
			stepsEnd = i
			break
		}
	}
	itemIndent := -1
	starts := []int{}
	for i := stepsAt + 1; i < stepsEnd; i++ {
		if blankOrComment(lines[i]) {
			continue
		}
		n, _ := indentOf(lines[i])
		if !isSeqItem(lines[i]) {
			continue
		}
		if itemIndent < 0 {
			itemIndent = n
		}
		if n == itemIndent {
			starts = append(starts, i)
		}
	}
	for k, ss := range starts {
		se := stepsEnd
		if k+1 < len(starts) {
			se = starts[k+1]
		}
		sc.scanStep(path, name, lines, ss, se)
	}
}

// scanStep reads one `- ` step item (lines[s:e]) and emits at most one command.
func (sc *wfScan) scanStep(path, job string, lines []string, s, e int) {
	_, _, keyCol, _, _ := splitKey(lines[s])
	var (
		run     string
		runLine int
		block   bool
		uses    string
		refuse  string
		quote   []string
	)
	for i := s; i < e; i++ {
		key, val, col, _, ok := splitKey(lines[i])
		if !ok || col != keyCol {
			continue // nested content (with:/env: children, block-scalar body)
		}
		switch key {
		case "run":
			runLine = i + 1
			switch {
			case val == "|" || val == "|-" || val == "|+":
				block = true
				body := blockBody(lines, i+1, e, col)
				quote = body
				// Tuple-assigning `run, refuse = joinRunBlock(body)` would CLEAR a
				// refusal an earlier key in this step already set (working-directory,
				// if, env, continue-on-error, shell all commonly precede `run:`),
				// drafting a live member with zero notes. The block branch may set a
				// refusal but must never erase one, and may fill `run` only when the
				// step is otherwise clean.
				if r, rf := joinRunBlock(body); rf != "" {
					refuse = rf
				} else if refuse == "" {
					run = r
				}
			case strings.HasPrefix(val, ">"):
				refuse = "a folded (`>`) block scalar joins its lines with spaces, which is not the command it looks like"
				quote = blockBody(lines, i+1, e, col)
			default:
				run = val
			}
		case "uses":
			uses = stripInlineComment(val)
		case "if":
			refuse = "the step is conditional (`if:`), so it may not run at all"
		case "continue-on-error":
			if b := stripInlineComment(val); b == "true" {
				refuse = "`continue-on-error: true` — the step does not gate the job"
			}
		case "working-directory":
			refuse = "`working-directory:` — the command runs somewhere other than the repository root"
		case "env":
			refuse = "the step sets `env:` — a verify command would not have those variables"
		case "shell":
			if sh := stripInlineComment(val); sh != "bash" && sh != "sh" {
				refuse = fmt.Sprintf("`shell: %s` — verify runs the command through `sh -c`", sh)
			}
		}
	}
	if uses != "" && run == "" && refuse == "" {
		sc.note("%s:%d job %q step `uses: %s` — action setup; the run supplies the tree and -env-mode supplies the environment", path, s+1, job, uses)
		return
	}
	if run == "" && refuse == "" {
		return
	}
	// A control byte surviving inside the command would corrupt the `verify = ...`
	// line: a CR or LF (a lone CR mid-line is not a line terminator to textLines
	// but is to plenty of other readers) ends the line early and turns the
	// remainder into a policy key of its own, and a NUL reaches the value and then
	// exec. Refuse rather than strip: silently rewriting somebody's command is
	// exactly the wrong-suggestion failure this scanner avoids. What counts as a
	// control byte is isControlByte's call, not this function's -- see the note
	// there on why there is exactly one of them.
	if refuse == "" && hasControlByte(run) {
		refuse = "the command contains a control character (a NUL, DEL, carriage return, or newline), which cannot be a single `verify` line"
	}
	if refuse == "" && strings.Contains(run, "${{") {
		refuse = "the command interpolates a `${{ }}` expression, which cannot be evaluated here — an empty substitution can leave a command that exits 0 and gates nothing"
		if !block {
			quote = []string{run}
		}
	}
	if refuse != "" {
		if len(quote) == 0 && run != "" {
			quote = []string{run}
		}
		sc.noteQuoted(quote, "%s:%d job %q run step skipped: %s", path, max(runLine, s+1), job, refuse)
		return
	}
	if strings.TrimSpace(run) == "" {
		return
	}
	sc.Commands = append(sc.Commands, wfCommand{Line: runLine, Job: job, Cmd: strings.TrimSpace(run)})
}

// joinRunBlock turns a `run: |` block into ONE command, or refuses it.
//
// The default step shell is `bash -e`, so a block of simple commands has the
// same all-must-pass meaning as those commands ANDed — that equivalence is the
// entire licence for joining, and it evaporates the moment the block is a shell
// program rather than a list. A trailing operator, a line continuation, a
// heredoc, a trailing `#` comment, or a leading shell keyword each mean the next
// line is not an independent command (or would be commented out), so the block
// is refused whole and quoted in the draft for a human to fold by hand.
//
// Whole-line `#` comments are DROPPED rather than joined: `a && # c && b`
// comments out everything after it, which would silently delete members of the
// battery. A TRAILING `#` cannot be dropped the same way (the text before it is
// a real command), so a line with a whitespace-preceded `#` refuses the block.
func joinRunBlock(body []string) (cmd, refuse string) {
	var cmds []string
	for _, raw := range body {
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		// A trailing `#` comment (`go build ./... # compile`) would, once joined
		// with ` && `, comment out every member after it — the same silent-deletion
		// failure whole-line comments are dropped to avoid, but mid-line. Detecting
		// it exactly needs shell tokenization (a `#` inside a quoted string is not a
		// comment), which this scanner does not do, so it takes the same posture it
		// takes for `<<` and trailing `\`: refuse the whole block and quote it. A `#`
		// preceded by whitespace is the shell-comment shape; one inside quotes is
		// over-refused, which is in-spec (fewer suggestions, never a wrong one).
		if strings.Contains(t, " #") || strings.Contains(t, "\t#") {
			return "", "a line has a trailing `#` comment, which would swallow the rest of the &&-joined block"
		}
		if strings.Contains(t, "<<") {
			return "", "the block contains a heredoc (`<<`), which cannot be joined into one line"
		}
		if strings.HasSuffix(t, `\`) {
			return "", "the block uses a line continuation (trailing `\\`)"
		}
		for _, op := range []string{"&&", "||", "|", ";", "&"} {
			if strings.HasSuffix(t, op) {
				return "", fmt.Sprintf("a line ends with `%s`, so it continues onto the next one", op)
			}
		}
		word := t
		if i := strings.IndexAny(word, " \t"); i >= 0 {
			word = word[:i]
		}
		if shellKeywords[word] {
			return "", fmt.Sprintf("a line begins with the shell keyword `%s`, so the block is a program rather than a list of commands", word)
		}
		cmds = append(cmds, t)
	}
	switch len(cmds) {
	case 0:
		return "", "the block holds no commands"
	case 1:
		return cmds[0], ""
	default:
		return strings.Join(cmds, " && "), ""
	}
}

// blockBody returns the body lines of a block scalar whose key sits at column
// keyCol, dedented by the body's own first indentation. Blank lines are kept
// (they are inside the block) but trailing ones are trimmed.
func blockBody(lines []string, s, e, keyCol int) []string {
	bodyIndent := -1
	var out []string
	for i := s; i < e; i++ {
		if strings.TrimSpace(lines[i]) == "" {
			out = append(out, "")
			continue
		}
		n, _ := indentOf(lines[i])
		if n <= keyCol {
			break
		}
		if bodyIndent < 0 {
			bodyIndent = n
		}
		if n < bodyIndent {
			break
		}
		out = append(out, lines[i][bodyIndent:])
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

// workflowTriggers reads the top-level `on:` key — inline (`on: push`,
// `on: [push, pull_request]`) or as a block of names — and returns the trigger
// names plus the 1-based line `on:` was found on (0 when absent). GitHub
// workflows sometimes quote the key ("on":) because bare `on` is a YAML 1.1
// boolean, so quotes are stripped.
func workflowTriggers(lines []string, defaultBranch string) (map[string]bool, int) {
	trig := map[string]bool{}
	for i, ln := range lines {
		if blankOrComment(ln) {
			continue
		}
		if n, _ := indentOf(ln); n != 0 {
			continue
		}
		key, val, _, item, ok := splitKey(ln)
		if !ok || item || key != "on" {
			continue
		}
		if v := stripInlineComment(val); v != "" {
			for _, name := range strings.FieldsFunc(strings.Trim(v, "[]"), func(r rune) bool { return r == ',' || r == ' ' }) {
				if n := strings.Trim(name, `"'`); n != "" {
					trig[n] = true
				}
			}
			return trig, i + 1
		}
		blockIndent := -1
		for j := i + 1; j < len(lines); j++ {
			if blankOrComment(lines[j]) {
				continue
			}
			n, _ := indentOf(lines[j])
			if n == 0 {
				break
			}
			if blockIndent < 0 {
				blockIndent = n
			}
			if n != blockIndent {
				continue
			}
			t := stripInlineComment(strings.TrimSpace(lines[j]))
			t = strings.TrimSpace(strings.TrimPrefix(t, "-"))
			if k := strings.IndexByte(t, ':'); k > 0 {
				t = t[:k]
			}
			t = strings.Trim(t, `"'`)
			switch {
			case t == "":
			case (t == "push" || t == "pull_request") && triggerIsNotAMergeGate(lines, j, blockIndent, defaultBranch):
				// Recorded under a DIFFERENT name so the merge-gate test below does
				// not see it: a release workflow's steps are release steps, and
				// copying them into a landing bar would gate every merge on
				// publishing. The name keeps the trigger it came from, so the note
				// says pull_request when that is what was refused.
				trig[t+"(not-a-merge-gate)"] = true
			default:
				trig[t] = true
			}
		}
		return trig, i + 1
	}
	return trig, 0
}

// triggerIsNotAMergeGate reports whether the `push:` or `pull_request:` trigger
// whose key is at lines[at] (indented `indent`) fires on something other than a
// merge to the default branch. Two shapes, one meaning:
//
//   - tags only. A tag push is a release, not a merge.
//   - branches that cannot include the default branch (`release/**`, `deploy`).
//     A push filtered to a release line never fires on a merge either. For
//     `pull_request:` the same key filters the PR's BASE branch, which is the
//     same question asked from the other side: a PR into `release/**` does not
//     gate a merge to the default branch. The tags leg is simply inert there --
//     `pull_request:` has no tags key -- so one helper answers for both.
//
// These were one guard and one gap. `on: push: tags: ['v*']` was refused while
// `on: push: branches: [release/**]` -- identical semantics -- was not, so a
// release job's `echo` became the landing bar for a repo whose tests fail, with
// the toolchain fallback that would have drafted a real battery sitting unreached.
//
// ponytail: the default-branch test is name matching (main/master, or any
// wildcard that could expand to them). Reading the repo's actual default branch
// would be exact; do that if this ever misfires. Over-refusing is the cheap
// direction here -- a refused workflow falls back to the manifests and drafts a
// better bar, never a worse one.
func triggerIsNotAMergeGate(lines []string, at, indent int, defaultBranch string) bool {
	tags, branches := false, false
	mergeable := false
	lastKey := "" // the key any following `- ` items belong to
	for j := at + 1; j < len(lines); j++ {
		if blankOrComment(lines[j]) {
			continue
		}
		n, _ := indentOf(lines[j])
		if n <= indent {
			break
		}
		key, val, _, item, ok := splitKey(lines[j])
		if item {
			// A `- main` entry belongs to the key it sits under, and ONLY that
			// key. Testing `branches` seen-anywhere instead let a `- '**/*.md'`
			// under paths-ignore: mark a release-only push as mergeable -- and
			// since that pattern's fixed prefix is empty, every branch matched it.
			// A routine `paths-ignore:` line then reopened the vacuous draft this
			// guard exists to prevent, depending on key order.
			if lastKey == "branches" && branchCouldBeDefault(listItemValue(lines[j]), defaultBranch) {
				mergeable = true
			}
			continue
		}
		if !ok {
			continue
		}
		lastKey = key
		switch key {
		case "tags", "tags-ignore":
			tags = true
		case "branches":
			branches = true
			// An inline list: `branches: [main, release/**]`.
			for _, b := range inlineListValues(val) {
				if branchCouldBeDefault(b, defaultBranch) {
					mergeable = true
				}
			}
		case "branches-ignore":
			// An exclusion list still fires on everything else, including the
			// default branch, so it does not narrow this away from a merge gate.
			branches, mergeable = true, true
		}
	}
	if tags && !branches {
		return true
	}
	return branches && !mergeable
}

// branchCouldBeDefault reports whether a `branches:` entry could match the
// repository's default branch. A wildcard that is not anchored to some other
// prefix could expand to anything, so it counts.
func branchCouldBeDefault(b, defaultBranch string) bool {
	b = strings.Trim(strings.TrimSpace(b), `"'`)
	if b == "" {
		return false
	}
	// The names to test against. When the repo told us its default branch, that
	// is the only one that matters -- a gitflow repo on `develop` gating merges
	// with `branches: [develop]` was previously refused by a main/master name
	// test, silently discarding its real battery. main/master are the fallback
	// for a repo that could not say (bare, freshly init'd, no origin/HEAD).
	names := []string{"main", "master"}
	if defaultBranch != "" {
		names = []string{defaultBranch}
	}
	for _, n := range names {
		if b == n {
			return true
		}
	}
	// A wildcard matches the default branch when everything before it is a
	// prefix of that name: `ma*` could be `main`, `release/**` could not.
	if i := strings.IndexAny(b, "*?["); i >= 0 {
		prefix := b[:i]
		for _, n := range names {
			if strings.HasPrefix(n, prefix) {
				return true
			}
		}
	}
	return false
}

// listItemValue is the value of a `- item` line.
func listItemValue(ln string) string {
	s := stripInlineComment(strings.TrimSpace(ln))
	return strings.TrimSpace(strings.TrimPrefix(s, "-"))
}

// inlineListValues splits `[a, b]` (or a bare scalar) into its entries.
func inlineListValues(val string) []string {
	v := strings.TrimSpace(stripInlineComment(val))
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// topLevelBlock locates a column-0 key and the exclusive end of its block (the
// next non-blank column-0 line, or EOF). Returns -1 when the key is absent.
func topLevelBlock(lines []string, want string) (at, end int) {
	at = -1
	for i, ln := range lines {
		if blankOrComment(ln) {
			continue
		}
		if n, _ := indentOf(ln); n != 0 {
			continue
		}
		if at >= 0 {
			return at, i
		}
		if key, _, _, item, ok := splitKey(ln); ok && !item && key == want {
			at = i
		}
	}
	if at < 0 {
		return -1, 0
	}
	return at, len(lines)
}

// firstIndent returns the indentation of the first non-blank line in
// lines[s:e], or -1 when there is none.
func firstIndent(lines []string, s, e int) int {
	for i := s; i < e && i < len(lines); i++ {
		if blankOrComment(lines[i]) {
			continue
		}
		n, _ := indentOf(lines[i])
		return n
	}
	return -1
}

// textLines splits data into lines with any trailing CR removed, so a file
// checked out with CRLF endings scans identically to one with LF.
func textLines(data []byte) []string {
	ls := strings.Split(string(data), "\n")
	for i := range ls {
		ls[i] = strings.TrimSuffix(ls[i], "\r")
	}
	return ls
}

// indentOf counts a line's leading spaces and reports whether a tab appears in
// that indentation (which makes every column comparison here meaningless).
func indentOf(s string) (n int, tab bool) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ':
			n++
		case '\t':
			return n, true
		default:
			return n, false
		}
	}
	return n, false
}

func blankOrComment(s string) bool {
	t := strings.TrimSpace(s)
	return t == "" || strings.HasPrefix(t, "#")
}

func isSeqItem(s string) bool {
	n, _ := indentOf(s)
	rest := s[n:]
	return rest == "-" || strings.HasPrefix(rest, "- ")
}

// splitKey reads a `key: value` mapping line, tolerating a leading `- `
// sequence marker. keyCol is the column the KEY starts at, so `- run: x` nests
// like a key at that column and the block scalar under it can be found by
// comparing indentation against it. ok is false for anything that is not a
// mapping key line (a plain scalar, a bare `-`, a URL with no space after the
// colon). Only the FIRST colon splits, so `run: docker run a:b` keeps its value
// intact.
func splitKey(line string) (key, value string, keyCol int, item, ok bool) {
	n, tab := indentOf(line)
	if tab {
		return "", "", n, false, false
	}
	rest := line[n:]
	keyCol = n
	if rest == "-" || strings.HasPrefix(rest, "- ") {
		item = true
		adv := 1
		for adv < len(rest) && rest[adv] == ' ' {
			adv++
		}
		keyCol = n + adv
		rest = rest[adv:]
	}
	i := strings.IndexByte(rest, ':')
	if i <= 0 {
		return "", "", keyCol, item, false
	}
	if i+1 < len(rest) && rest[i+1] != ' ' {
		return "", "", keyCol, item, false
	}
	key = strings.Trim(strings.TrimSpace(rest[:i]), `"'`)
	value = strings.TrimSpace(rest[i+1:])
	return key, value, keyCol, item, key != ""
}

// runsOnRefusal reports why a job's `runs-on:` value disqualifies it, or "" when
// it names a linux/ubuntu runner. It reads the inline scalar (`ubuntu-latest`)
// and inline array (`[self-hosted, linux, x64]`) forms; a `${{ }}` expression, a
// block form with no inline value, or a value naming only non-linux/self-hosted
// labels is refused, because the OS the steps would run on is then either unknown
// or not the one verify runs on. This is a heuristic, so it errs toward refusal.
func runsOnRefusal(val string) string {
	v := stripInlineComment(val)
	if strings.Contains(v, "${{") {
		return "has a `runs-on:` `${{ }}` expression whose runner OS cannot be determined here"
	}
	for _, tok := range strings.FieldsFunc(strings.Trim(v, "[]"), func(r rune) bool { return r == ',' || r == ' ' }) {
		t := strings.ToLower(strings.Trim(tok, `"'`))
		if strings.Contains(t, "ubuntu") || strings.Contains(t, "linux") {
			return "" // a linux runner: draft its steps
		}
	}
	if strings.TrimSpace(v) == "" {
		return "has a `runs-on:` this scanner cannot read inline, so the runner OS is unknown"
	}
	return fmt.Sprintf("has `runs-on: %s`, not a linux/ubuntu runner — a verify member is drafted for the machine verify runs on", v)
}

// stripInlineComment drops a trailing ` # ...` comment from a STRUCTURAL value
// (a trigger name, an action reference, a shell name). It is deliberately not
// applied to a `run:` value: a `#` there may be part of the command, and a
// command is copied verbatim or not at all.
func stripInlineComment(s string) string {
	if strings.HasPrefix(s, "#") {
		return ""
	}
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
