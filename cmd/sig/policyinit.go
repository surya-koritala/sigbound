// `sig policy init` — write a STARTING sigbound.policy by reading the
// configuration the repository already carries (issue #148).
//
// This exists because the first question a new user has is "what do I put in
// `verify =`?", and until now nothing in the binary answered it. The command
// reads, in descending order of confidence: the GitHub Actions workflows that
// actually gate merges today, a Makefile's conventional targets, the language
// manifests, and CODEOWNERS. It writes ONE file — <repo>/sigbound.policy — and
// nothing else; it starts no run, touches no ref, and writes nothing under
// .git/sigbound/.
//
// Two properties make the output safe to hand a stranger:
//
//   - It is CONSERVATIVE. Only what can be justified from a file in the repo is
//     emitted, every emitted key names the file and line it came from, and every
//     input that could not be translated is listed as a `# unmapped:` note
//     rather than guessed at. A verify member that is subtly wrong is worse than
//     no verify member: a mangled command that exits 0 is a bar that gates
//     nothing while reporting green.
//   - It never clobbers. An existing sigbound.policy is the repo's real landing
//     bar; overwriting it is the only destructive thing this command could do,
//     so it does not, ever. It prints the lines it would have added and exits
//     non-zero instead. There is deliberately no -force.
//
// The draft is parsed with parsePolicy before it is written, and a draft that
// does not parse is never written. That check is load-bearing: loadPolicy
// treats a present-but-invalid policy as a hard error at run start, so a
// malformed file committed to a repo would fail every subsequent run.
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
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// policyInitSelfProtectNote is the header sentence that tells a reader what
// committing the file costs them. Asserted by string match in the tests so it
// cannot be dropped silently.
const policyInitSelfProtectNote = "Committing this file turns on policy self-protection: a later change to " + policyFileName + " itself parks for a human ack."

// toolProbeTimeout bounds one `go version` / `npm --version` style probe.
const toolProbeTimeout = 15 * time.Second

func runPolicy(w io.Writer, argv []string) (int, error) {
	if len(argv) == 0 {
		policyUsage(w)
		return exitOperationalError, errors.New("a subcommand is required")
	}
	switch argv[0] {
	case "init":
		return runPolicyInit(w, argv[1:])
	case "-h", "--help", "help":
		policyUsage(w)
		return exitOK, nil
	default:
		policyUsage(w)
		return exitOperationalError, fmt.Errorf("unknown subcommand %q", argv[0])
	}
}

func policyUsage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  sig policy init [-repo PATH] [-rev REV]")
	fmt.Fprintln(w, "writes a starting <repo>/"+policyFileName+" from the repo's workflows, Makefile, manifests and CODEOWNERS.")
	fmt.Fprintln(w, "an existing "+policyFileName+" is never overwritten: the suggested lines are printed and the exit code is non-zero.")
}

func runPolicyInit(w io.Writer, argv []string) (int, error) {
	fset := flag.NewFlagSet("policy init", flag.ContinueOnError)
	fset.Usage = func() {
		fmt.Fprintln(fset.Output(), "usage: sig policy init [-repo PATH] [-rev REV]")
		fmt.Fprintln(fset.Output(), "inspect the repo and write a starting "+policyFileName+", one commented line per source.")
		fset.PrintDefaults()
	}
	repo := fset.String("repo", ".", "path to the target git repository")
	rev := fset.String("rev", "HEAD", "the committed tree the sources are read from (the same posture a run's policy load uses: the bar is a versioned file, not a working-directory draft)")
	if err := fset.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitOK, nil
		}
		return exitOperationalError, err
	}
	if strings.TrimSpace(*repo) == "" {
		return exitOperationalError, errors.New("-repo is required")
	}

	ctx := context.Background()
	g := gitx.New(*repo)
	// An explicitly-named -rev that does not resolve is a user mistake, not a
	// new-repo starting point: fail BEFORE drafting or writing anything, so no
	// vacuous policy is left behind to block the corrected retry (there is no
	// -force). A failing default HEAD is the empty/bare-repo case and still yields
	// a commented template below.
	if *rev != "HEAD" {
		if _, err := g.RevParse(ctx, *rev); err != nil {
			return exitOperationalError, fmt.Errorf("cannot resolve -rev %q in %s: %w — nothing was written", *rev, *repo, err)
		}
	}
	d := buildPolicyDraft(ctx, g, *repo, *rev)
	draft := d.render()
	// Self-check BEFORE anything is written or suggested. A draft that does not
	// parse is a bug in this command, and the one output it must never produce.
	if _, err := parsePolicy([]byte(draft)); err != nil {
		fmt.Fprintln(os.Stderr, draft)
		return exitOperationalError, fmt.Errorf("internal: the drafted policy does not parse (%w) — nothing was written; please report this with the draft printed above", err)
	}

	out := filepath.Join(*repo, policyFileName)
	// O_CREATE|O_EXCL is the never-clobber guard, the concurrent-init guard, and
	// the write-outside-the-repo guard in ONE. It fails EEXIST on an existing file
	// AND refuses to follow a symlink — so a committed `sigbound.policy -> ../OUT`
	// cannot redirect the write outside the tree — and the create+write is atomic
	// against a racing second init. The old ReadFile-then-WriteFile had a TOCTOU
	// window between the check and the write and followed such a symlink.
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	switch {
	case errors.Is(err, fs.ErrExist):
		// The path is taken — an existing policy (the repo's real landing bar), or a
		// symlink O_EXCL declined to follow. Print the diff and refuse.
		//
		// The file is read ONLY when Lstat says it is not a symlink. O_EXCL closed
		// the WRITE half of the symlink hole; this closes the READ half. A repo can
		// commit `sigbound.policy` as a symlink to any path the invoking user can
		// read (~/.netrc, ~/.aws/credentials, .git-credentials), and
		// printPolicySuggestion surfaces parseConfigFile's error, which quotes the
		// offending line verbatim — one line of that file, straight into stdout, and
		// from there into whatever captures this command's output. Two of those three
		// files carry live credential material. Not reading it is the fix; the parse
		// error stays genuinely useful for a real file.
		var existing []byte
		if fi, lerr := os.Lstat(out); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			fmt.Fprintf(w, "%s is a symlink; it was not read (a symlinked policy could point at any file this user can read).\n", out)
		} else {
			existing, _ = os.ReadFile(out)
		}
		printPolicySuggestion(w, out, existing, d)
		return exitOperationalError, fmt.Errorf("%s already exists — not overwriting it; nothing was written", out)
	case err != nil:
		return exitOperationalError, fmt.Errorf("create %s: %w", out, err)
	}
	if _, err := f.WriteString(draft); err != nil {
		f.Close()
		return exitOperationalError, fmt.Errorf("write %s: %w", out, err)
	}
	if err := f.Close(); err != nil {
		return exitOperationalError, fmt.Errorf("write %s: %w", out, err)
	}
	d.printSummary(w, out)
	return exitOK, nil
}

// printPolicySuggestion is the never-clobber path: it prints the keys the draft
// carries that the existing policy does not, each under its attribution, in a
// diff shape. An existing file that does not itself parse is not a reason to
// suppress the suggestion — every drafted line is then shown as an addition,
// with the parse error named.
func printPolicySuggestion(w io.Writer, path string, existing []byte, d policyDraft) {
	have := map[string]bool{}
	entries, perr := parseConfigFile(existing)
	for _, e := range entries {
		if e.Key == "ack-paths" {
			// An existing `ack-paths = a/**, b/**` already carries both globs;
			// comparing whole values would suggest each of them again.
			for _, glob := range splitCSV(e.Value) {
				have[e.Key+"\x00"+glob] = true
			}
			continue
		}
		have[e.Key+"\x00"+e.Value] = true
	}
	fmt.Fprintf(w, "%s already exists; not overwriting it.\n", path)
	if perr != nil {
		fmt.Fprintf(w, "(it does not parse: %v — every drafted line is shown below)\n", perr)
	}
	var add []draftLine
	for _, ln := range d.lines {
		if ln.live && !have[ln.key+"\x00"+ln.value] {
			add = append(add, ln)
		}
	}
	if len(add) == 0 {
		fmt.Fprintln(w, "It already carries every line this repo's configuration suggests.")
		return
	}
	fmt.Fprintf(w, "This repo's configuration suggests %d line(s) it does not have:\n", len(add))
	for _, ln := range add {
		fmt.Fprintf(w, "  # %s\n", ln.comment)
		fmt.Fprintf(w, "+ %s = %s\n", ln.key, ln.value)
	}
	fmt.Fprintln(w, "Merge by hand what you want; nothing was written.")
}

// draftLine is one emitted policy key with the attribution comment printed
// above it. live=false emits the key COMMENTED OUT — used when the command
// could be drafted but would fail on first run here (an absent toolchain), so
// the suggestion is neither hidden nor a broken battery.
type draftLine struct {
	comment string
	key     string
	value   string
	live    bool
}

// draftNote is one input that could not be translated into a policy key.
// source is the reader that saw it (workflows, make, toolchain, codeowners).
type draftNote struct {
	source string
	detail string
	quote  []string
}

// policyDraft is the assembled draft plus what the run of each source found,
// for the summary printed to stdout.
type policyDraft struct {
	repo    string
	rev     string
	lines   []draftLine
	notes   []draftNote
	summary []string
}

func (d *policyDraft) note(source, detail string, quote ...string) {
	d.notes = append(d.notes, draftNote{source: source, detail: detail, quote: quote})
}

func (d *policyDraft) verifyCount() (live int) {
	for _, ln := range d.lines {
		if ln.live && ln.key == "verify" {
			live++
		}
	}
	return live
}

// render produces the file bytes. Every comment goes through emitComment, so a
// newline inside any interpolated source text becomes another comment line
// rather than a stray policy key — which is what makes the parsePolicy
// self-check a formality instead of a hope.
func (d *policyDraft) render() string {
	var b strings.Builder
	emitComment(&b, policyFileName+" — a STARTING landing bar written by `sig policy init` from "+d.rev+".")
	emitComment(&b, "Every key below is preceded by the file it was read from. Review it before")
	emitComment(&b, "committing: it is a starting point, not a finished policy.")
	emitComment(&b, "Nothing here is in force until it is committed at the base a run lands onto")
	emitComment(&b, `(docs/USAGE.md, "Landing policy").`)
	emitComment(&b, policyInitSelfProtectNote)

	if d.verifyCount() == 0 {
		b.WriteByte('\n')
		emitComment(&b, "No verify command could be inferred from this repository (see the notes at the")
		emitComment(&b, "end). A policy with no verify line adds no bar of its own. Put the command your")
		emitComment(&b, "CI actually runs here and uncomment it:")
		emitComment(&b, "verify = <command>")
	}
	for _, ln := range d.lines {
		b.WriteByte('\n')
		emitComment(&b, ln.comment)
		if ln.live {
			fmt.Fprintf(&b, "%s = %s\n", ln.key, ln.value)
			continue
		}
		emitComment(&b, ln.key+" = "+ln.value)
	}
	if len(d.notes) > 0 {
		b.WriteByte('\n')
		emitComment(&b, "Everything below is something a source said that a policy key cannot express.")
		emitComment(&b, "`grep '^# unmapped:' "+policyFileName+"` lists it.")
		for _, n := range d.notes {
			emitComment(&b, "unmapped: "+n.source+": "+n.detail)
			for _, q := range n.quote {
				emitComment(&b, "    "+q)
			}
		}
	}
	return b.String()
}

// printSummary is the "what I found and why" the user sees on stdout. The file
// itself carries the per-line attribution; this is the index to it.
func (d *policyDraft) printSummary(w io.Writer, path string) {
	fmt.Fprintf(w, "sig policy init: read %s at %s\n", d.repo, d.rev)
	for _, s := range d.summary {
		fmt.Fprintf(w, "  %s\n", s)
	}
	live, commented, acks := 0, 0, 0
	for _, ln := range d.lines {
		switch {
		case ln.key == "ack-paths" && ln.live:
			acks++
		case !ln.live:
			commented++
		default:
			live++
		}
	}
	fmt.Fprintf(w, "wrote %s: %d verify member(s), %d ack-paths glob(s), %d commented suggestion(s), %d unmapped note(s)\n",
		path, live, acks, commented, len(d.notes))
	if live == 0 {
		fmt.Fprintf(w, "NO verify command was inferred: %s gates nothing until you fill in a `verify =` line.\n", policyFileName)
	}
	fmt.Fprintln(w, "review it, then commit it — it takes effect at the base a run lands onto, not from your working directory.")
}

// emitComment appends s as `#` comment lines.
// Control bytes are replaced with '?' on the way out. A refused command is
// QUOTED into the notes verbatim, so a NUL in a workflow (or in a job name or
// file path interpolated into an attribution comment) would otherwise land in
// the file and make git call it binary — "Binary files differ", "0 insertions(+),
// 0 deletions(-)". That defeats the reviewable diff this file exists to be, and
// it matters most to the human acking a parked change to sigbound.policy itself
// under policy self-protection: they cannot read what they are approving. This is
// the chokepoint every comment byte routes through — detail, quote, attribution,
// and commented-out key lines alike. \r and \n keep their existing meaning (they
// split the text into further comment lines); a tab is legitimate whitespace.
func emitComment(b *strings.Builder, s string) {
	for _, ln := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		if ln == "" {
			b.WriteString("#\n")
			continue
		}
		b.WriteString("# ")
		b.WriteString(scrubControl(ln))
		b.WriteByte('\n')
	}
}

// isControlByte is the ONE definition of "a byte that must not reach the drafted
// file" -- C0 controls and DEL, with tab exempt. Every consumer asks this and
// nothing re-states it: the scan that refuses a command, the CODEOWNERS pattern
// check, and scrubControl below all route here.
//
// It is one function because it was three. A copy that checked `< 0x20` without
// DEL let `run: \x7f` through to a live verify value, and a copy that omitted the
// tab exemption disagreed with the other two on a byte that only happened to be
// unreachable. Two predicates that currently agree are a defect waiting for one
// of them to be edited.
//
// Bytes, not runes, so a control byte inside invalid UTF-8 is caught too.
func isControlByte(b byte) bool { return (b < 0x20 && b != '\t') || b == 0x7f }

// hasControlByte reports whether s holds any byte isControlByte rejects. A rune
// scan would miss one inside invalid UTF-8 (it decodes to RuneError, which is
// not a control), so this walks bytes.
func hasControlByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if isControlByte(s[i]) {
			return true
		}
	}
	return false
}

// scrubControl replaces every byte isControlByte rejects with '?'.
func scrubControl(s string) string {
	if !hasControlByte(s) {
		return s // the overwhelmingly common case: nothing to do
	}
	out := []byte(s)
	for i, c := range out {
		if isControlByte(c) {
			out[i] = '?'
		}
	}
	return string(out)
}

// codeownersCandidates is the forge's own resolution order for CODEOWNERS.
var codeownersCandidates = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

// makefileCandidates is make's own lookup order.
var makefileCandidates = []string{"GNUmakefile", "makefile", "Makefile"}

// toolchainRule maps a manifest's presence to a verify preset. The command
// comes from verifyPresets (presets.go) verbatim, so what is drafted is
// byte-identical to `-verify-preset <name>` and can never drift from it.
type toolchainRule struct {
	name     string
	manifest []string
	preset   string
	probe    []string
}

var toolchainRules = []toolchainRule{
	{"go", []string{"go.mod"}, "go", []string{"go", "version"}},
	{"node", []string{"package.json"}, "node", []string{"npm", "--version"}},
	// The python probe is `pytest --version`, NOT `python -m pytest --version`:
	// `python -m NAME` prepends the current directory to sys.path, so a
	// repo-resident `pytest.py` / `pytest/` shadows the real module and its code
	// runs during `sig policy init`. The console-script form resolves pytest from
	// PATH (its sys.path[0] is the script's own dir, not the repo) and `--version`
	// prints and exits without collection, so no repo file is imported or executed.
	{"python", []string{"pyproject.toml", "setup.py", "requirements.txt"}, "python", []string{"pytest", "--version"}},
	{"rust", []string{"Cargo.toml"}, "rust", []string{"cargo", "--version"}},
}

// buildPolicyDraft reads every source and assembles the draft. It never
// returns an error: a repo it cannot read yields a draft that is a commented
// template plus a note saying why, which is a usable starting file, where an
// error would leave the new user with nothing.
//
// Sources are read from rev's COMMITTED tree, not the working directory — the
// same posture loadPolicy uses, and the reason the per-glob file counts below
// describe a real tree. An uncommitted workflow is therefore invisible here.
func buildPolicyDraft(ctx context.Context, g *gitx.Git, repo, rev string) policyDraft {
	d := policyDraft{repo: repo, rev: rev}
	sha, err := g.RevParse(ctx, rev)
	if err != nil {
		d.note("repo", fmt.Sprintf("cannot resolve %s: %v — no source could be read", rev, oneLine(err.Error())))
		d.summary = append(d.summary, "repo: unreadable ("+oneLine(err.Error())+")")
		return d
	}
	d.rev = short(sha)
	// What a merge actually lands on. Empty when the repo cannot say, which the
	// branch-filter guard treats as "fall back to main/master" rather than as a
	// name.
	defaultBranch := g.DefaultBranch(ctx)
	// LsTreeSizes gives the path list AND each blob's size in one call, so an
	// oversized blob is skipped BEFORE BlobsBatch reads it into memory — the cap
	// binds before allocation, not after (a 500MB workflow must not become 500MB
	// of RSS just to be rejected).
	sizes, err := g.LsTreeSizes(ctx, sha)
	if err != nil {
		d.note("repo", fmt.Sprintf("cannot list the tree at %s: %v — no source could be read", short(sha), oneLine(err.Error())))
		d.summary = append(d.summary, "repo: tree unreadable ("+oneLine(err.Error())+")")
		return d
	}
	tree := make([]string, 0, len(sizes))
	inTree := make(map[string]bool, len(sizes))
	for p := range sizes {
		tree = append(tree, p)
		inTree[p] = true
	}
	sort.Strings(tree)

	// wantBlob adds a source file to the read set unless it exceeds wfMaxBytes, in
	// which case the skip becomes a visible note instead of a silent read.
	var want []string
	wantBlob := func(source, p string) {
		if sizes[p] > wfMaxBytes {
			d.note(source, fmt.Sprintf("%s is %d bytes (cap %d) — not read", p, sizes[p], wfMaxBytes))
			return
		}
		want = append(want, p)
	}
	var workflows []string
	for _, p := range tree {
		if strings.HasPrefix(p, ".github/workflows/") && (strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml")) &&
			!strings.Contains(strings.TrimPrefix(p, ".github/workflows/"), "/") {
			if sizes[p] > wfMaxBytes {
				d.note("workflows", fmt.Sprintf("%s is %d bytes (cap %d) — not scanned", p, sizes[p], wfMaxBytes))
				continue
			}
			workflows = append(workflows, p)
			want = append(want, p)
		}
	}
	sort.Strings(workflows)
	for _, p := range codeownersCandidates {
		if inTree[p] {
			wantBlob("codeowners", p)
		}
	}
	for _, p := range makefileCandidates {
		if inTree[p] {
			wantBlob("make", p)
		}
	}
	if inTree["package.json"] {
		wantBlob("toolchain", "package.json")
	}
	blobs := map[string]string{}
	if len(want) > 0 {
		specs := make([]string, 0, len(want))
		for _, p := range want {
			specs = append(specs, sha+":"+p)
		}
		got, berr := g.BlobsBatch(ctx, specs)
		if berr != nil {
			d.note("repo", fmt.Sprintf("cannot read blobs at %s: %v", short(sha), oneLine(berr.Error())))
		}
		for _, p := range want {
			if c, ok := got[sha+":"+p]; ok {
				blobs[p] = c
			}
		}
	}

	// The verify battery comes from exactly ONE source: the first that produces
	// anything, in descending order of confidence. A second source's commands
	// are not appended — they would be a duplicate battery running the same work
	// twice, and picking between two disagreeing definitions of "the bar" is a
	// judgement call this command does not have the standing to make.
	verified := d.addWorkflowVerify(workflows, blobs, defaultBranch)
	verified = d.addMakeVerify(ctx, blobs, verified)
	d.addToolchainVerify(ctx, inTree, blobs, verified)
	d.addCodeowners(inTree, blobs, tree)
	d.noteGoTestTimeout()
	return d
}

// noteGoTestTimeout surfaces a `go test` member that carries no -timeout. The
// command is emitted VERBATIM — never rewritten, that is the whole scanner
// contract — but `go test`'s 10-minute default can red-out slow-but-correct
// tests on a loaded machine (sigbound's own committed policy documents adding a
// -timeout for exactly this reason), so the omission is made visible rather than
// silently reproduced. One advisory note, however many members qualify.
func (d *policyDraft) noteGoTestTimeout() {
	for _, ln := range d.lines {
		if ln.live && ln.key == "verify" && strings.Contains(ln.value, "go test") && !strings.Contains(ln.value, "-timeout") {
			d.note("verify", "a drafted `go test` member has no `-timeout`; go test's 10m default can fail slow-but-correct tests on a loaded machine — consider adding one (e.g. `-timeout 20m`). The command is left exactly as its source wrote it.")
			return
		}
	}
}

// addWorkflowVerify runs the workflow scanner over every workflow file and
// emits one verify member per usable run step, deduplicated by command text
// (a repo whose ubuntu and windows jobs both run `go build ./...` gets one
// member, not two).
func (d *policyDraft) addWorkflowVerify(workflows []string, blobs map[string]string, defaultBranch string) bool {
	if len(workflows) == 0 {
		return false
	}
	// seen indexes d.lines, which is safe only because this is the FIRST source
	// to append; the dupe pass below runs before make/toolchain/codeowners add
	// anything.
	seen := map[string]int{} // command -> index into d.lines
	dupes := map[string]int{}
	files, steps := 0, 0
	for _, p := range workflows {
		content, ok := blobs[p]
		if !ok {
			d.note("workflows", p+" could not be read at this rev")
			continue
		}
		files++
		sc := scanWorkflow(p, []byte(content), defaultBranch)
		for _, n := range sc.Notes {
			d.note("workflows", n.Detail, n.Quote...)
		}
		for _, c := range sc.Commands {
			if _, dup := seen[c.Cmd]; dup {
				dupes[c.Cmd]++
				continue
			}
			seen[c.Cmd] = len(d.lines)
			steps++
			d.lines = append(d.lines, draftLine{
				comment: fmt.Sprintf("%s:%d job %q", p, c.Line, c.Job),
				key:     "verify",
				value:   c.Cmd,
				live:    true,
			})
		}
	}
	for cmd, n := range dupes {
		i := seen[cmd]
		d.lines[i].comment += fmt.Sprintf(" (+%d identical run step(s) in other jobs)", n)
	}
	switch {
	case steps > 0:
		d.summary = append(d.summary, fmt.Sprintf("workflows: %d file(s) -> %d verify member(s)", files, steps))
		return true
	default:
		d.summary = append(d.summary, fmt.Sprintf("workflows: %d file(s), no usable run step (see the notes in the file)", files))
		return false
	}
}

// addMakeVerify emits `make <target>` members from a Makefile's conventional
// targets, but only when the workflows produced nothing. An aggregate target
// (check/ci/verify) is preferred alone over build+lint+test, since that is what
// it exists to be.
func (d *policyDraft) addMakeVerify(ctx context.Context, blobs map[string]string, already bool) bool {
	path, content := "", ""
	for _, p := range makefileCandidates {
		if c, ok := blobs[p]; ok {
			path, content = p, c
			break
		}
	}
	if path == "" {
		return already
	}
	targets := makefileTargets(content)
	wanted := pickMakeTargets(targets)
	if len(wanted) == 0 {
		d.summary = append(d.summary, path+": no test/lint/build/check/ci/verify target")
		d.note("make", path+" has no conventional target (check, ci, verify, build, lint, test) to draft a battery from")
		return already
	}
	if already {
		d.summary = append(d.summary, fmt.Sprintf("%s: %s not used (the workflows already supplied the battery)", path, strings.Join(wanted, ", ")))
		d.note("make", fmt.Sprintf("%s has target(s) %s — not drafted, the workflows above already supply the battery", path, strings.Join(wanted, ", ")))
		return already
	}
	live, why := true, ""
	if err := probeTool(ctx, d.repo, []string{"make", "-v"}); err != nil {
		live, why = false, "; `make -v` failed here: "+oneLine(err.Error())+" — uncomment once make is available"
	}
	for _, t := range wanted {
		d.lines = append(d.lines, draftLine{
			comment: fmt.Sprintf("%s target %q%s", path, t, why),
			key:     "verify",
			value:   "make " + t,
			live:    live,
		})
	}
	d.summary = append(d.summary, fmt.Sprintf("%s: %d target(s) -> %d verify member(s)", path, len(wanted), len(wanted)))
	return live
}

// addToolchainVerify emits the ecosystem's verifyPresets command for each
// manifest present, but only when no earlier source produced a battery. A
// polyglot repo gets one member per detected manifest — the presence of a
// go.mod does not make a Cargo.toml stop mattering.
//
// The tool's version probe ANNOTATES rather than merely informs: a preset whose
// tool is absent here is emitted COMMENTED OUT, because this command's whole
// promise is a policy that works on the first run. The limit is stated in the
// comment it writes — the probe describes THIS machine, not wherever verify
// will actually run, so the line is drafted either way and one uncomment away.
func (d *policyDraft) addToolchainVerify(ctx context.Context, inTree map[string]bool, blobs map[string]string, already bool) {
	for _, r := range toolchainRules {
		found := ""
		for _, m := range r.manifest {
			if inTree[m] {
				found = m
				break
			}
		}
		if found == "" {
			continue
		}
		cmd := verifyPresets[r.preset]
		if already {
			d.summary = append(d.summary, fmt.Sprintf("%s: %s detected, not used (an earlier source already supplied the battery)", found, r.name))
			d.note("toolchain", fmt.Sprintf("%s detects the %s toolchain — not drafted, an earlier source already supplies the battery (`-verify-preset %s` is %s)", found, r.name, r.preset, cmd))
			continue
		}
		if r.name == "node" && !hasNodeTestScript(blobs["package.json"]) {
			d.summary = append(d.summary, found+": no scripts.test, `npm test` not drafted")
			d.note("toolchain", found+" has no scripts.test, so `npm test` would fail on the first run — nothing drafted")
			continue
		}
		live, why, tail := true, fmt.Sprintf("`-verify-preset %s`", r.preset), ""
		if err := probeTool(ctx, d.repo, r.probe); err != nil {
			live = false
			tail = " (commented out: probe failed)"
			why += fmt.Sprintf("; `%s` failed here: %s — this describes THIS machine, not wherever verify runs, so the line is drafted; uncomment it once the toolchain is available",
				strings.Join(r.probe, " "), oneLine(err.Error()))
		}
		d.lines = append(d.lines, draftLine{
			comment: fmt.Sprintf("%s -> %s toolchain, %s", found, r.name, why),
			key:     "verify",
			value:   cmd,
			live:    live,
		})
		d.summary = append(d.summary, fmt.Sprintf("%s: %s toolchain -> `%s`%s", found, r.name, cmd, tail))
	}
}

// addCodeowners translates CODEOWNERS patterns into ack-paths globs. Owners are
// dropped: ack-paths has no owner dimension — any ack releases the landing —
// and an owner string never reaches the right-hand side of an emitted key.
func (d *policyDraft) addCodeowners(inTree map[string]bool, blobs map[string]string, tree []string) {
	path, content := "", ""
	for _, p := range codeownersCandidates {
		if c, ok := blobs[p]; ok {
			path, content = p, c
			break
		}
	}
	if path == "" {
		return
	}
	for _, p := range codeownersCandidates {
		if p != path && inTree[p] {
			d.note("codeowners", p+" is also present; the forge reads "+path+" and so does this")
		}
	}
	emitted, refused := 0, 0
	for i, raw := range textLines([]byte(content)) {
		line := raw
		if h := strings.IndexByte(line, '#'); h >= 0 {
			line = line[:h]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pattern, owners := fields[0], fields[1:]
		if len(owners) == 0 {
			d.note("codeowners", fmt.Sprintf("%s:%d %q has no owner — not an ownership rule", path, i+1, pattern))
			continue
		}
		globs, refuse := codeownersGlobs(pattern, tree)
		if refuse != "" {
			refused++
			d.note("codeowners", fmt.Sprintf("%s:%d pattern %q: %s", path, i+1, pattern, refuse))
			continue
		}
		for _, glob := range globs {
			emitted++
			d.lines = append(d.lines, draftLine{
				comment: fmt.Sprintf("%s:%d (%s) — matches %d file(s) at %s", path, i+1, strings.Join(owners, " "), countMatches(glob, tree), d.rev),
				key:     "ack-paths",
				value:   glob,
				live:    true,
			})
		}
	}
	if emitted > 0 {
		d.note("codeowners", "owners are dropped — ack-paths has no owner dimension; ANY ack releases a parked landing, whoever the file's owner is")
	}
	d.summary = append(d.summary, fmt.Sprintf("%s: %d ack-paths glob(s), %d pattern(s) refused", path, emitted, refused))
}

// codeownersGlobs translates one CODEOWNERS pattern into globMatch's dialect
// (see policy.go). A pattern that cannot be expressed emits NOTHING and returns
// a refusal: a glob that quietly means something other than its source pattern
// is exactly the failure the notes list exists to prevent.
//
// LIMIT, stated because it is a real narrowing: CODEOWNERS inherits gitignore's
// rule that a pattern whose only slash is trailing matches a directory of that
// name at ANY depth, so `docs/` also covers `a/b/docs/`. The translation below
// anchors it at the repository root instead. The file count printed above each
// emitted line is what makes such a mistranslation visible on review.
func codeownersGlobs(pattern string, tree []string) (globs []string, refuse string) {
	switch {
	case strings.ContainsAny(pattern, ",[]"):
		return nil, "an ack-paths value is comma-split and globMatch has no character class, so `,` / `[` / `]` cannot be expressed"
	case strings.HasPrefix(pattern, "!"):
		return nil, "a negation cannot be expressed — ack-paths only adds paths that need an ack"
	case strings.Contains(pattern, `\`):
		return nil, "globMatch has no backslash escape, so the pattern cannot be translated faithfully"
	case hasControlByte(pattern):
		// A control byte here would reach a LIVE `ack-paths` value (strings.Fields
		// does not split on NUL, so `auth\x00x` is one pattern), putting a NUL in
		// the drafted file — git then calls it binary and the diff is unreviewable.
		// Comments are scrubbed by emitComment; a value is refused instead, because
		// silently rewriting a pattern would change which paths it matches.
		return nil, "the pattern contains a control character, which cannot be part of an ack-paths value"
	}
	p := strings.TrimPrefix(pattern, "/")
	switch p {
	case "", "*", "**", "*/", "**/":
		if p == "" {
			return nil, "empty pattern"
		}
		return []string{"**"}, ""
	}
	if strings.HasSuffix(p, "/") {
		return []string{strings.TrimSuffix(p, "/") + "/**"}, ""
	}
	// gitignore's anchoring rule reads the ORIGINAL pattern: a separator at the
	// beginning or in the middle anchors it at the repository root. Testing the
	// leading-slash-stripped form instead would misread the anchored `/auth` as
	// the depth-independent `auth` and emit `**/auth`, which is a different set.
	if !strings.Contains(strings.TrimSuffix(pattern, "/"), "/") {
		// No separator at all: the name matches at ANY depth. `**/p` matches a FILE
		// named p (and `**/` matches zero segments, so the repo-root file too), but
		// NOT the contents of a DIRECTORY named p — for that the subtree needs
		// `**/p/**`. Emitting only `**/p` (the old behaviour) made `.github @sec` an
		// ack-path that could never park, since nothing is a file literally named
		// `.github`. Which the pattern means is answered by the tree, exactly as the
		// anchored branch below does it; when the tree answers neither, BOTH are
		// emitted (an over-broad ack parks more for a human; a too-narrow one lets a
		// sensitive change land unattended).
		return treeDirectedGlobs("**/"+p, "**/"+p+"/**", tree), ""
	}
	// Anchored. Whether it names a file or a directory is answered by the tree
	// rather than guessed; when the tree answers neither (a path that does not
	// exist yet — exactly when an ack matters most) BOTH are emitted, because
	// an over-broad ack-paths parks more landings for a human while a too-narrow
	// one lets a sensitive change land unattended.
	return treeDirectedGlobs(p, p+"/**", tree), ""
}

// treeDirectedGlobs picks between a FILE glob and a DIRECTORY-subtree glob by
// asking the tree which the pattern actually matches: the file glob alone if it
// matches a file and no subtree, the subtree glob alone if the reverse, and BOTH
// when the tree answers neither (the path does not exist at this rev — where an
// over-broad ack is the safer error). Shared by the anchored and no-separator
// CODEOWNERS branches so both consult the tree identically.
func treeDirectedGlobs(fileGlob, dirGlob string, tree []string) []string {
	file, dir := false, false
	for _, t := range tree {
		if !file && globMatch(fileGlob, t) {
			file = true
		}
		if !dir && globMatch(dirGlob, t) {
			dir = true
		}
		if file && dir {
			break
		}
	}
	switch {
	case file && !dir:
		return []string{fileGlob}
	case dir && !file:
		return []string{dirGlob}
	default:
		return []string{fileGlob, dirGlob}
	}
}

func countMatches(glob string, tree []string) int {
	n := 0
	for _, t := range tree {
		if globMatch(glob, t) {
			n++
		}
	}
	return n
}

// makefileTargets returns the explicit target names a Makefile declares, in
// file order. Pattern rules (`%.o:`), variable assignments (`X := y`), and
// special targets (`.PHONY:`) are skipped: none of them is something `make X`
// would run as a gate.
func makefileTargets(content string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range textLines([]byte(content)) {
		// A rule's target starts in column 0; a recipe line starts with a tab and
		// a continuation with whitespace, so neither can be a target.
		if line == "" || line[0] == '\t' || line[0] == ' ' || line[0] == '#' {
			continue
		}
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		// `X := y` and `X ::= y` are assignments, not rules.
		if j := i; j+1 < len(line) && (line[j+1] == '=' || (line[j+1] == ':' && j+2 < len(line) && line[j+2] == '=')) {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name == "" || seen[name] || strings.ContainsAny(name, "%$ \t.=") {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// pickMakeTargets chooses which of a Makefile's targets form a gate. An
// aggregate target is what a repo made for exactly this purpose, so it is
// preferred ALONE; otherwise the conventional three run cheapest-first, the
// ordering docs/USAGE.md recommends for a battery.
func pickMakeTargets(targets []string) []string {
	have := map[string]bool{}
	for _, t := range targets {
		have[t] = true
	}
	for _, agg := range []string{"check", "ci", "verify"} {
		if have[agg] {
			return []string{agg}
		}
	}
	var out []string
	for _, t := range []string{"build", "lint", "test"} {
		if have[t] {
			out = append(out, t)
		}
	}
	return out
}

// hasNodeTestScript reports whether package.json declares scripts.test. Without
// it `npm test` exits non-zero on the first run, which is precisely the
// plausible-but-broken battery this command must not draft.
func hasNodeTestScript(content string) bool {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal([]byte(content), &pkg) != nil {
		return false
	}
	return strings.TrimSpace(pkg.Scripts["test"]) != ""
}

// probeTool runs a bounded version probe in dir. It reports only whether the
// tool can be invoked at all on THIS machine; it never decides what the repo's
// bar is. dir is set as the working directory so the probe describes the repo
// under -repo rather than the invoker's cwd — and so a probe run for -repo PATH
// is not silently describing somewhere else. The probes themselves are chosen to
// never execute a module resolved FROM the repo (see toolchainRules' python
// note); dir only fixes which directory an already-safe probe reports on.
func probeTool(ctx context.Context, dir string, argv []string) error {
	if len(argv) == 0 {
		return errors.New("no probe")
	}
	ctx, cancel := context.WithTimeout(ctx, toolProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second // return promptly on cancel; see runAgent
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}

// policyInfoLine is `sig doctor`'s one line about the repo's landing bar: what
// the policy at HEAD declares, or a pointer at this command when there is none.
// It is INFORMATIONAL in exactly the sense diskInfoLine and gcInfoLine are — it
// never touches doctor's allOK and can never change its exit code. A repo whose
// policy blob is missing or unparseable is reported as such and still leaves
// doctor free to exit 0 on its three real checks: whether a repository has
// chosen to own its landing bar is not a health verdict about this machine.
func policyInfoLine(ctx context.Context, repoDir string) string {
	dir := repoDir
	if dir == "" {
		dir = "."
	}
	pol, err := loadPolicy(ctx, gitx.New(dir), "HEAD")
	switch {
	case err != nil:
		return fmt.Sprintf("landing policy: %s at HEAD is unreadable (%s)", policyFileName, oneLine(err.Error()))
	case !pol.Present:
		return fmt.Sprintf("landing policy: no %s at HEAD (run `sig policy init` to draft one from this repo's CI config)", policyFileName)
	default:
		return fmt.Sprintf("landing policy: %s at HEAD — %d verify member(s), %d ack-paths glob(s)", policyFileName, len(pol.Verify), len(pol.AckPaths))
	}
}

// oneLine flattens text that will be interpolated into a single comment or
// summary line. emitComment would split an embedded newline into another
// comment line, which is safe but ugly; this keeps the note readable.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
