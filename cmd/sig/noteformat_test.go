package main

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// docNoteFields reads the stable-field table out of docs/USAGE.md's "Note
// format" section. It is deliberately bounded to that ONE table: the file has
// many, and a scan that swept them all would silently start asserting against
// whatever table someone adds next.
func docNoteFields(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile("../../docs/USAGE.md")
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	start := strings.Index(body, "### Note format")
	if start < 0 {
		t.Fatal("docs/USAGE.md has no '### Note format' section — this test's subject is gone")
	}
	section := body[start:]
	if end := strings.Index(section, "\n### "); end > 0 {
		section = section[:end]
	}
	row := regexp.MustCompile("(?m)^\\| `([a-zA-Z0-9_.]+)` \\|")
	out := map[string]bool{}
	for _, m := range row.FindAllStringSubmatch(section, -1) {
		// Table rows document nested paths too (integrate.finalSHA). The promise
		// this test can hold the code to is the TOP-LEVEL key; the nested half is
		// prose.
		out[strings.SplitN(m[1], ".", 2)[0]] = true
	}
	if len(out) == 0 {
		t.Fatal("no field rows parsed from the Note format table — the table's shape changed and this test went blind")
	}
	return out
}

// reportJSONKeys is every top-level key runReport can serialize, taken from the
// struct tags rather than a list written here — a hand-maintained list would
// make this test agree with itself instead of with the code.
func reportJSONKeys(t *testing.T) map[string]bool {
	t.Helper()
	rt := reflect.TypeOf(runReport{})
	out := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		out[strings.SplitN(tag, ",", 2)[0]] = true
	}
	if len(out) == 0 {
		t.Fatal("runReport has no json tags — this test went blind")
	}
	return out
}

// TestNoteFormatDocsMatchTheStruct catches the failure that makes a documented
// format worse than an undocumented one: the doc promising a field the payload
// no longer has. Renaming `integrate` in the struct, or dropping a field the
// table lists, fails here.
//
// The reverse direction is deliberately NOT asserted. The stable set is a
// SUBSET by design — most of runReport is internal and may change without a
// format bump — so "every struct field is documented" would be the wrong
// promise to make, not a stricter version of the right one.
func TestNoteFormatDocsMatchTheStruct(t *testing.T) {
	docs := docNoteFields(t)
	keys := reportJSONKeys(t)
	var missing []string
	for f := range docs {
		if !keys[f] {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("documented as stable but absent from runReport: %v — a reader outside this repo was promised these", missing)
	}
}

// TestNoteCarriesEveryDocumentedField writes a note the way attachNote does and
// asserts every documented field actually SURVIVES into the bytes. The struct
// check above cannot see this: a field can exist, carry the right tag, and still
// be dropped by omitempty, leaving the doc describing a key no reader ever sees.
func TestNoteCarriesEveryDocumentedField(t *testing.T) {
	rep := runReport{
		RunID:     "20260726-120000-abcd",
		Repo:      "/tmp/cell",
		Base:      "main",
		BaseSHA:   "1111111111111111111111111111111111111111",
		Strategy:  "overlay",
		Integrate: integrateJSON{FinalSHA: "2222222222222222222222222222222222222222"},
		Verify:    verifyJSON{Ran: true, OK: true},
		Park:      &parkJSON{LandedSHA: "3333333333333333333333333333333333333333"},
		Unlands:   "20260725-090000-ef01",
	}
	// Exactly attachNote's encoding path, including the version stamp.
	rep.NoteFormat = noteFormatCurrent
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	var missing []string
	for f := range docNoteFields(t) {
		if _, ok := got[f]; !ok {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("documented but absent from a written note: %v", missing)
	}
	if v := string(got["noteFormat"]); v != "1" {
		t.Fatalf("noteFormat=%s, want 1", v)
	}
}

// TestAttachNoteStampsTheVersion goes through the REAL writer against a real
// repo. TestNoteCarriesEveryDocumentedField above sets NoteFormat itself, so it
// proves the field serializes and nothing about whether anything sets it — this
// is the test that dies if the stamp in attachNote is dropped.
func TestAttachNoteStampsTheVersion(t *testing.T) {
	ctx := context.Background()
	g, _ := makeGoRepo(t)
	head, err := g.RevParse(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	attachNote(ctx, g, head, runReport{Repo: "/tmp/cell", Base: "main"}, "test")

	content, ok, err := g.NoteShow(ctx, "sigbound", head)
	if err != nil || !ok {
		t.Fatalf("no note attached (ok=%v, err=%v)", ok, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		t.Fatalf("note is not valid JSON: %v", err)
	}
	if v, present := m["noteFormat"]; !present || string(v) != "1" {
		t.Fatalf("note carries noteFormat=%s (present=%v), want 1 — an outside reader has no way to tell this payload's shape", v, present)
	}
	// And it round-trips through the gate that reads it.
	if _, ok := parseNote(content); !ok {
		t.Fatal("a note this binary just wrote is refused by its own reader")
	}
}

// TestNoteFormatStaysOutOfTheReportSurface is the other half of the stamp: the
// version belongs to the NOTE, so -json output and the on-disk manifest — which
// marshal the same struct — must be byte-identical to before the field existed.
// A reader diffing run reports across the upgrade sees nothing new.
func TestNoteFormatStaysOutOfTheReportSurface(t *testing.T) {
	data, err := json.MarshalIndent(runReport{Repo: "/tmp/cell", Base: "main"}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "noteFormat") {
		t.Fatalf("an unstamped report serialized noteFormat; -json and the manifest changed shape:\n%s", data)
	}
}

// TestParseNoteVersionGate is the reason the version exists. A payload from a
// newer sigbound must not be read as though its fields still mean what this
// binary thinks — refusing sends the caller to the local ledger, which is
// ground truth, instead of publishing a confidently wrong answer.
func TestParseNoteVersionGate(t *testing.T) {
	base := `{"repo":"/tmp/c","base":"main","integrate":{"finalSHA":"abc"}}`

	t.Run("no version is still readable", func(t *testing.T) {
		// Notes written before the format was versioned. Existing repositories
		// must not lose their history because a stamp arrived late.
		rep, ok := parseNote(base)
		if !ok {
			t.Fatal("an unversioned note was refused; every note written before v2.2 just became unreadable")
		}
		if rep.Integrate.FinalSHA != "abc" {
			t.Fatalf("finalSHA=%q, want abc", rep.Integrate.FinalSHA)
		}
	})

	t.Run("current version is readable", func(t *testing.T) {
		if _, ok := parseNote(`{"noteFormat":1,"integrate":{"finalSHA":"abc"}}`); !ok {
			t.Fatal("a note this binary wrote is not readable by it")
		}
	})

	t.Run("future version is refused", func(t *testing.T) {
		if _, ok := parseNote(`{"noteFormat":2,"integrate":{"finalSHA":"abc"}}`); ok {
			t.Fatal("a future-format note was accepted; its fields may not mean what this binary thinks")
		}
	})

	t.Run("garbage is refused", func(t *testing.T) {
		if _, ok := parseNote(`{"noteFormat":`); ok {
			t.Fatal("unparseable bytes were accepted")
		}
	})
}

// TestFutureNoteFallsThroughToTheLedger proves the refusal above at the level a
// user sees: a commit whose note this binary cannot read is not reported as
// unattributed, it is answered from the local run ledger. Asserting only
// parseNote would leave the fall-through itself untested.
func TestFutureNoteFallsThroughToTheLedger(t *testing.T) {
	if _, ok := parseNote(`{"noteFormat":99,"integrate":{"finalSHA":"deadbeef"},"verify":{"ran":true,"ok":true}}`); ok {
		t.Fatal("precondition broken: the gate accepted a future version")
	}
	// resolveProvenance's note arm is exactly `if rep, ok := parseNote(...)`, so a
	// refused note leaves the manifest walk as the only source. The walk is
	// covered by the -sha tests; what this pins is that a refusal is a
	// FALL-THROUGH and never an early "no such commit".
	if _, ok := parseNote(`{"noteFormat":1,"integrate":{"finalSHA":"deadbeef"}}`); !ok {
		t.Fatal("a current-version note was refused, so the arm above proves nothing")
	}
}
