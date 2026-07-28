package attest_test

import (
	"errors"
	"testing"

	"github.com/surya-koritala/sigbound/v2/pkg/attest"
)

// This file is package attest_test on purpose: it may use ONLY the exported
// surface, exactly as a hosted gate would. If a caller outside this repository
// cannot answer a question with what is imported here, neither can this test.

// realNote is a note as `sig` actually writes one — the full run report, of
// which the stable subset is a small part. Parsing must ignore everything it
// does not promise rather than choke on it, or every engine release breaks every
// reader.
const realNote = `{
  "noteFormat": 1,
  "runId": "20260726T101500Z-a1b2c3",
  "repo": "/srv/cells/api",
  "base": "main",
  "baseSHA": "1111111111111111111111111111111111111111",
  "laneMode": "strict",
  "tasks": [{"id": "a", "prompt": "…"}],
  "perAgent": [{"id": "a", "branch": "agent/a", "ok": true}],
  "strategy": "overlay",
  "integrate": {
    "strategy": "overlay",
    "landed": ["agent/a", "agent/b"],
    "finalSHA": "2222222222222222222222222222222222222222",
    "wallMs": 812
  },
  "verify": {"ran": true, "ok": true, "invocations": 1},
  "agentCmd": "claude -p --permission-mode acceptEdits \"$SIGBOUND_TASK\"",
  "policy": {"hash": "abc123"}
}`

// TestParseARealNote is the whole use case: a service that received a push has
// the note bytes and needs to know whether that commit was verified.
func TestParseARealNote(t *testing.T) {
	n, err := attest.Parse(realNote)
	if err != nil {
		t.Fatalf("a note sigbound actually writes did not parse: %v", err)
	}
	if n.Format != attest.CurrentFormat {
		t.Fatalf("format=%d, want %d", n.Format, attest.CurrentFormat)
	}
	if n.RunID != "20260726T101500Z-a1b2c3" || n.Base != "main" || n.Strategy != "overlay" {
		t.Fatalf("stable fields did not survive: %+v", n)
	}
	if len(n.Integrate.Landed) != 2 {
		t.Fatalf("landed=%v, want both branches", n.Integrate.Landed)
	}
	if !n.Verify.OK || !n.Verify.Ran {
		t.Fatalf("verify=%+v, want a green verdict", n.Verify)
	}
	if !n.Landed() {
		t.Fatal("a green run whose ref moved does not report as landed")
	}
	if got := n.LandedSHA(); got != "2222222222222222222222222222222222222222" {
		t.Fatalf("landedSHA=%q", got)
	}
}

// TestVerifyRedIsNotALanding is the trap this type exists to stop a caller
// falling into. finalSHA is populated with the integrated tree EVEN WHEN verify
// went red and nothing was written to the base ref — so a gate that read
// finalSHA and concluded "landed" would wave through a tree that failed.
func TestVerifyRedIsNotALanding(t *testing.T) {
	n, err := attest.Parse(`{"noteFormat":1,"baseSHA":"aaa",
		"integrate":{"finalSHA":"bbb"},"verify":{"ran":true,"ok":false}}`)
	if err != nil {
		t.Fatal(err)
	}
	if n.Integrate.FinalSHA == "" {
		t.Fatal("precondition: finalSHA should be populated even on a red verify")
	}
	if n.Landed() {
		t.Fatal("a run whose verify went RED reports as landed; a gate reading this would accept a tree that failed")
	}
	if got := n.LandedSHA(); got != "" {
		t.Fatalf("landedSHA=%q for a run that landed nothing", got)
	}
}

// TestAckReleasedLanding: a run that parked lands later, by a human's ack, and
// that commit is recorded in park — the run's own report predates the ack and
// cannot carry it. A reader that only looked at integrate would report that
// nothing landed.
func TestAckReleasedLanding(t *testing.T) {
	n, err := attest.Parse(`{"noteFormat":1,"baseSHA":"aaa",
		"integrate":{"finalSHA":"aaa"},"verify":{"ran":true,"ok":true},
		"park":{"landedSHA":"ccc","reason":"ack-paths","approvedBy":"alice"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !n.Landed() {
		t.Fatal("an ack-released landing reports as not landed")
	}
	if got := n.LandedSHA(); got != "ccc" {
		t.Fatalf("landedSHA=%q, want the ack's commit", got)
	}
	if n.Park.ApprovedBy != "alice" {
		t.Fatalf("approvedBy=%q", n.Park.ApprovedBy)
	}
}

// TestConcerns is the authority test, and the reason parsing alone is not
// enough. A note is user-writable and arrives with the commit from whatever
// remote sent it, so what makes it usable is that it genuinely claims the commit
// being asked about.
func TestConcerns(t *testing.T) {
	n, err := attest.Parse(realNote)
	if err != nil {
		t.Fatal(err)
	}
	const landed = "2222222222222222222222222222222222222222"

	if !n.Concerns(landed) {
		t.Fatal("the note does not claim the commit it landed")
	}
	if !n.Concerns(landed[:12]) {
		t.Fatal("an abbreviated sha does not match")
	}
	// A note lifted onto an unrelated commit. It parses perfectly and must be
	// worth nothing.
	if n.Concerns("9999999999999999999999999999999999999999") {
		t.Fatal("the note claims a commit it has nothing to do with; a forged note lifted onto any commit would now vouch for it")
	}
	if n.Concerns("") {
		t.Fatal("an empty sha matched")
	}
	// The base is not a landing — the note says what this run PRODUCED.
	if n.Concerns("1111111111111111111111111111111111111111") {
		t.Fatal("the note claims its own base commit")
	}
}

// TestFormatGate: a payload from a newer sigbound must be refused, not guessed
// at. Its fields may no longer mean what this reader thinks, and a confidently
// wrong answer about whether a commit was verified is worse than no answer.
func TestFormatGate(t *testing.T) {
	t.Run("no version is readable", func(t *testing.T) {
		// Notes written before the format was versioned.
		n, err := attest.Parse(`{"baseSHA":"aaa","integrate":{"finalSHA":"bbb"},"verify":{"ran":true,"ok":true}}`)
		if err != nil {
			t.Fatalf("an unversioned note was refused: %v", err)
		}
		if n.Format != 0 {
			t.Fatalf("format=%d, want 0 for an unversioned note", n.Format)
		}
		if !n.Landed() {
			t.Fatal("an unversioned note lost its verdict")
		}
	})

	t.Run("a future version is refused with a distinguishable error", func(t *testing.T) {
		_, err := attest.Parse(`{"noteFormat":99,"integrate":{"finalSHA":"bbb"}}`)
		if err == nil {
			t.Fatal("a future-format note was accepted")
		}
		// errors.Is, not string matching: a caller has to be able to tell "too
		// new, fall through to your own records" from "these bytes are garbage".
		if !errors.Is(err, attest.ErrFutureFormat) {
			t.Fatalf("err=%v, want it to wrap ErrFutureFormat so a caller can branch on it", err)
		}
	})

	t.Run("garbage is refused and is NOT a future-format error", func(t *testing.T) {
		_, err := attest.Parse(`{"noteFormat":`)
		if err == nil {
			t.Fatal("unparseable bytes were accepted")
		}
		if errors.Is(err, attest.ErrFutureFormat) {
			t.Fatal("malformed bytes were reported as a future format; a caller would wait for an upgrade that fixes nothing")
		}
	})
}
