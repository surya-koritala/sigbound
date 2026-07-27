package main

import (
	"strings"
	"testing"
)

// approvedParkRecord is a park.json that passes parkJSON.validate, resolved as
// an ack would leave it. Deliberately its own helper rather than a shared one:
// these tests are about what an ACK records, and a fixture they own cannot be
// loosened out from under them by a change made for some other test's sake.
func approvedParkRecord(sha string) *parkJSON {
	const zero = "1111111111111111111111111111111111111111"
	return &parkJSON{
		VerifiedSHA:  sha,
		VerifiedTree: "3333333333333333333333333333333333333333",
		BaseSHA:      zero,
		ForkSHA:      zero,
		Base:         "main",
		Groups:       []parkGroupJSON{{Branches: []string{"agent/a"}, Reason: parkReasonAckPaths}},
		Reason:       parkReasonAckPaths,
		CreatedAt:    "2026-07-26T00:00:00Z",
	}
}

// TestSanitizeApprover pins the on-the-way-in cleaning. The value reaches a JSON
// document AND a git note, so what matters is that neither can be structurally
// broken by it — and that the cleaning happens once, here, rather than at each
// render site where one of them will eventually be forgotten.
func TestSanitizeApprover(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"an ordinary name is untouched", "alice", "alice"},
		{"surrounding whitespace goes", "  alice  ", "alice"},
		{"an email survives", "alice@example.com", "alice@example.com"},
		{"unicode survives", "Ada Lovelace — release manager", "Ada Lovelace — release manager"},
		{"empty stays empty", "", ""},
		{"whitespace only becomes empty", "   \t  ", ""},
		// A newline is how a payload forges structure around itself in a note.
		{"newlines are dropped", "alice\n### Landed\nnot really", "alice### Landednot really"},
		{"carriage returns are dropped", "alice\r\nbob", "alicebob"},
		{"tabs are dropped", "alice\tbob", "alicebob"},
		{"NUL is dropped", "alice\x00bob", "alicebob"},
		{"DEL is dropped", "alice\x7fbob", "alicebob"},
		{"escape sequences lose the escape", "alice\x1b[31m", "alice[31m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeApprover(c.in); got != c.want {
				t.Fatalf("sanitizeApprover(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	t.Run("a long value is bounded", func(t *testing.T) {
		got := sanitizeApprover(strings.Repeat("a", approverMax*10))
		if len(got) > approverMax {
			t.Fatalf("len=%d, want <= %d: an unbounded value bloats both the record and the note", len(got), approverMax)
		}
		if len(got) == 0 {
			t.Fatal("a long value was emptied rather than truncated")
		}
	})

	t.Run("control bytes cannot survive truncation either", func(t *testing.T) {
		// Dropping happens BEFORE the bound, so a value that is all newlines
		// cannot fill the budget and push real characters out.
		if got := sanitizeApprover(strings.Repeat("\n", approverMax*2) + "alice"); got != "alice" {
			t.Fatalf("got %q, want alice", got)
		}
	})
}

// TestApproverTravelsOnTheCommit is the acceptance the issue calls the property
// that matters: a clone with ONLY refs/notes/sigbound — no run directory —
// answers who approved a landing.
//
// That is the whole point of recording it here rather than in whatever system
// drove the ack. An approval that exists only in someone else's database is an
// approval you cannot prove later, which is exactly the question parking is for:
// a sensitive path, six months on, "who signed off on this?"
func TestApproverTravelsOnTheCommit(t *testing.T) {
	const sha = "2222222222222222222222222222222222222222"

	// A report as it exists AFTER an ack: the park record resolved, carrying the
	// approver. This is what attachAckNote serialises onto the released commit.
	pk := approvedParkRecord(sha)
	pk.ResolvedAt, pk.LandedSHA = "2026-07-26T00:05:00Z", sha
	pk.ApprovedBy = "alice"
	rep := &runReport{RunID: "20260726T000000Z-run", Park: pk}

	p := matchProvenance(rep, sha)
	if p == nil {
		t.Fatal("the released commit is not attributed at all")
	}
	if p.Role != roleAckLanded {
		t.Fatalf("role=%q, want %q", p.Role, roleAckLanded)
	}
	if p.ApprovedBy != "alice" {
		t.Fatalf("approvedBy=%q, want alice — a clone with only the note cannot answer who signed off", p.ApprovedBy)
	}

	// And it renders. A field nothing shows is a field nobody can use.
	line := provenanceLine(p)
	if !strings.Contains(line, "alice") {
		t.Fatalf("the approver is not in the rendered line: %q", line)
	}

	// It must read as RECORDED, never as verified: sigbound has no user model and
	// never checked this string.
	if !strings.Contains(line, "recorded as approved by") {
		t.Fatalf("the line presents the approver without saying it is merely recorded: %q", line)
	}
}

// TestApproverFromANoteIsMarkedAsAClaim. A note is user-writable and arrives
// with the commit from whatever remote sent it, so an approver read back from
// one is a claim. The existing machinery already marks note-sourced answers; the
// approver must ride inside that marking rather than beside it.
func TestApproverFromANoteIsMarkedAsAClaim(t *testing.T) {
	p := &provenance{
		SHA: "2222222222222222222222222222222222222222", Landed: true,
		Role: roleAckLanded, ApprovedBy: "alice", Source: "note",
		StartedAt: "2026-07-26T00:00:00Z", Strategy: "overlay", Verify: "pass",
	}
	line := provenanceLine(p)
	if !strings.Contains(line, "from commit note") {
		t.Fatalf("a note-sourced approval does not say where it came from: %q", line)
	}
	if strings.Contains(line, "run 20260726") {
		t.Fatalf("a note-sourced answer named a local run: %q", line)
	}
}

// TestApproverUnsetChangesNothing: omitting -by must leave today's output
// byte-identical, which is what a person acking their own local run produces.
func TestApproverUnsetChangesNothing(t *testing.T) {
	const sha = "2222222222222222222222222222222222222222"
	pk := approvedParkRecord(sha)
	pk.ResolvedAt, pk.LandedSHA = "2026-07-26T00:05:00Z", sha
	// ApprovedBy deliberately unset.
	p := matchProvenance(&runReport{RunID: "r", Park: pk}, sha)
	if p == nil {
		t.Fatal("no provenance")
	}
	if p.ApprovedBy != "" {
		t.Fatalf("approvedBy=%q on an ack that recorded nobody", p.ApprovedBy)
	}
	if line := provenanceLine(p); strings.Contains(line, "approved by") {
		t.Fatalf("an ack with no recorded approver still rendered an approver clause: %q", line)
	}
}
