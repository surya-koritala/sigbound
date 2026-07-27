// Package attest reads the provenance note sigbound attaches to a landed
// commit under refs/notes/sigbound — the record that answers "which run landed
// this commit, and did it pass".
//
// The note is deliberately the half of the record that TRAVELS. It rides on the
// commit, survives gc, and is readable from a clone that never had the run
// directory. That is the whole reason it exists, and it is why this package
// does: anything outside the `sig` binary that wants to answer that question has
// to parse it, and parsing a struct that can change shape in any release, with
// no way to tell which shape you are looking at, is not something anyone should
// have to reimplement.
//
// The engine writes the note and this package reads it, so the format has one
// definition rather than two that drift.
//
// # What a reader may rely on
//
// Note carries only the fields documented as STABLE in docs/USAGE.md's "Note
// format" section. The payload has more in it; the rest is internal and may
// change without a version bump, which is exactly why it is not here — a type
// that exposed everything would turn every internal field into a promise.
//
// # What this is NOT
//
// It is not authentication, and no amount of parsing makes it one. A note is
// user-writable and arrives with the commit from whatever remote sent it.
// Parsing tells you what the bytes SAY. Whether to believe them is a separate
// question, and the answer that matters is Concerns: a note is authoritative
// only for the commit it actually claims. A note lifted onto an unrelated commit
// parses perfectly and concerns nothing.
//
// Zero dependencies, stdlib only.
package attest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// CurrentFormat is the note version this package writes and reads.
//
// It versions the NOTE PAYLOAD and nothing else — not the release, not the
// engine. The compatibility rule, stated so an outside reader can rely on it:
// ADDING a field does not bump it, and within a version an existing field never
// changes meaning. Removing a documented field, or changing what one means,
// does.
const CurrentFormat = 1

// ErrFutureFormat is returned for a note written by a newer sigbound than this
// package understands.
//
// It is an ERROR rather than a best-effort parse on purpose. The fields may no
// longer mean what this code thinks, and a confidently wrong answer about
// whether a commit was verified is worse than no answer: a caller that gets this
// should fall through to whatever ground truth it has, exactly as `sig log`
// falls through to the local run ledger.
var ErrFutureFormat = errors.New("attest: note format is newer than this reader understands")

// Note is the stable subset of a provenance note.
type Note struct {
	// Format is the payload version. ZERO means the note predates versioning —
	// it is still readable, and its shape is the one this package reads.
	Format int `json:"noteFormat,omitempty"`

	RunID   string `json:"runId,omitempty"`
	Repo    string `json:"repo"`
	Base    string `json:"base"`
	BaseSHA string `json:"baseSHA"`
	// Strategy is the integration strategy actually applied.
	Strategy string `json:"strategy"`

	Integrate Integrate `json:"integrate"`
	Verify    Verify    `json:"verify"`
	// Park is present only for a run that parked. It is how an ACK-released
	// landing is recorded: the run's own report predates the ack and cannot
	// carry it.
	Park *Park `json:"park,omitempty"`
	// Unlands is the run id this run took back, for an unland.
	Unlands string `json:"unlands,omitempty"`
}

// Integrate is the integration result.
type Integrate struct {
	// FinalSHA is the commit the run produced. It is populated even when verify
	// went red and NOTHING was written to the base ref, so it is not on its own
	// proof of a landing — see Note.Landed.
	FinalSHA string `json:"finalSHA"`
	// Landed names the branches that landed together.
	Landed []string `json:"landed"`
}

// Verify is the verdict.
type Verify struct {
	Ran      bool `json:"ran"`
	OK       bool `json:"ok"`
	Flaky    bool `json:"flaky,omitempty"`
	Cached   bool `json:"cached,omitempty"`
	Repaired bool `json:"repaired,omitempty"`
}

// Park is a parked landing's record.
type Park struct {
	// LandedSHA is the commit an ack released, empty until one did.
	LandedSHA string `json:"landedSHA,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// ApprovedBy is who released it, as the caller supplied it. A CLAIM, never
	// proof: sigbound has no user model and never verified this string, and a
	// note is written by whoever pushed the commit.
	ApprovedBy string `json:"approvedBy,omitempty"`
}

// Parse decodes one note payload, refusing a version this package cannot read.
//
// A note with NO version parses as today's shape: notes written before the
// format carried a version must not become unreadable because a stamp arrived
// late.
func Parse(content string) (*Note, error) {
	var n Note
	if err := json.Unmarshal([]byte(content), &n); err != nil {
		return nil, fmt.Errorf("attest: parse note: %w", err)
	}
	if n.Format > CurrentFormat {
		return nil, fmt.Errorf("%w (note says %d, this reader knows %d)", ErrFutureFormat, n.Format, CurrentFormat)
	}
	return &n, nil
}

// Landed reports whether the run actually moved the base ref.
//
// FinalSHA alone is NOT that: it holds the integrated tree even when verify
// failed and nothing was written. A real landing needs a moved ref AND a
// green-or-unset verify. An ack-released landing is recorded in Park instead,
// because the report predates the ack.
func (n *Note) Landed() bool {
	if n.Park != nil && n.Park.LandedSHA != "" {
		return true
	}
	return n.Integrate.FinalSHA != "" &&
		n.Integrate.FinalSHA != n.BaseSHA &&
		(!n.Verify.Ran || n.Verify.OK)
}

// LandedSHA is the commit this note's run put on the base ref, or "" if none
// did. An ack's commit wins when the run's own record shows no landing.
func (n *Note) LandedSHA() string {
	if n.Integrate.FinalSHA != "" && n.Integrate.FinalSHA != n.BaseSHA && (!n.Verify.Ran || n.Verify.OK) {
		return n.Integrate.FinalSHA
	}
	if n.Park != nil {
		return n.Park.LandedSHA
	}
	return ""
}

// Concerns reports whether this note is authoritative FOR sha — the only
// question that matters when the note came from somewhere you do not control.
//
// A note is user-writable and rides in with the commit, so its payload buys it
// nothing on its own. What makes it usable is that it genuinely claims the
// commit being asked about: the run's landed integration commit, or the commit
// an ack released. A forged note, or one lifted onto an unrelated commit,
// carries somebody else's SHAs, does not match, and must be discarded.
//
// sha may be a full object name or a prefix of one.
func (n *Note) Concerns(sha string) bool {
	if sha == "" {
		return false
	}
	match := func(candidate string) bool {
		if candidate == "" {
			return false
		}
		if len(sha) == len(candidate) {
			return strings.EqualFold(sha, candidate)
		}
		return len(sha) < len(candidate) && strings.EqualFold(sha, candidate[:len(sha)])
	}
	if match(n.Integrate.FinalSHA) && n.Landed() {
		return true
	}
	return n.Park != nil && match(n.Park.LandedSHA)
}
