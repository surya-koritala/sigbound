package gitx

import (
	"bufio"
	"fmt"
	"strings"
	"testing"
)

// nopWC is a stdin that swallows request writes (we drive the response stream by
// hand, so the request side is irrelevant).
type nopWC struct{}

func (nopWC) Write(p []byte) (int, error) { return len(p), nil }
func (nopWC) Close() error                { return nil }

// newFakeReader builds a BatchBlobReader whose stdout is a canned byte stream —
// exactly what a daemon that was killed / desynced mid-response would leave in
// the pipe. cmd is nil; Read never touches it (only Close does, which we skip).
func newFakeReader(stream string) *BatchBlobReader {
	return &BatchBlobReader{
		stdin:  nopWC{},
		stdout: bufio.NewReader(strings.NewReader(stream)),
	}
}

// TestReadTruncationNeverReturnsPartialContent: a daemon killed PARTWAY THROUGH a
// multi-blob response must NEVER surface a truncated blob as valid content. Every
// truncation shape must make Read return an error and an unusable (nil) map.
func TestReadTruncationNeverReturnsPartialContent(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		specs  []string
	}{
		{
			// Header promises 10 bytes, stream dies after 4. Classic mid-content kill.
			name:   "content shorter than declared size",
			stream: "aaaaaaaa blob 10\nABCD",
			specs:  []string{"A"},
		},
		{
			// Exactly <size> content bytes but the process died before the trailing
			// LF git always writes. io.ReadFull wants size+1 => short read => error.
			name:   "content present but trailing newline truncated",
			stream: "aaaaaaaa blob 5\nhello",
			specs:  []string{"A"},
		},
		{
			// Header line itself truncated (no LF): ReadString hits EOF.
			name:   "header truncated mid-line",
			stream: "aaaaaaaa blob 5",
			specs:  []string{"A"},
		},
		{
			// Empty stream: daemon died before writing anything for this request.
			name:   "empty response",
			stream: "",
			specs:  []string{"A"},
		},
		{
			// Negative size in the header must be rejected before make([]byte, size+1).
			name:   "negative declared size",
			stream: "aaaaaaaa blob -1\n",
			specs:  []string{"A"},
		},
		{
			// A bogus huge positive size must be rejected before make() allocates it
			// (the size cap), not attempted and OOM/panicked.
			name:   "absurd declared size",
			stream: "aaaaaaaa blob 999999999999\nx\n",
			specs:  []string{"A"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			br := newFakeReader(tc.stream)
			m, err := br.Read(tc.specs)
			if err == nil {
				t.Fatalf("truncated stream returned nil error; map=%q — a partial response was trusted", m)
			}
			if m != nil {
				t.Fatalf("truncated stream returned a non-nil map %q alongside error %v — caller might trust it", m, err)
			}
		})
	}
}

// TestReadFirstRecordDiscardedWhenSecondTruncates: the FIRST blob in a multi-blob
// response is complete and valid; the SECOND is truncated
// (daemon killed partway). Read must NOT return the good first record — one bad
// record fails the whole call, so the caller's fail-open spawn re-reads all of
// it. A leaked-through first record would be a silent trust of a desynced stream.
func TestReadFirstRecordDiscardedWhenSecondTruncates(t *testing.T) {
	// Record A: complete "hi\n" body. Record B: header promises 100 bytes, only 2 arrive.
	stream := "aaaaaaaa blob 2\nhi\n" + "bbbbbbbb blob 100\nxy"
	br := newFakeReader(stream)
	m, err := br.Read([]string{"A", "B"})
	if err == nil {
		t.Fatalf("second-record truncation returned nil error; map=%q", m)
	}
	if m != nil {
		t.Fatalf("got non-nil map %q — the valid first record leaked despite a truncated second", m)
	}
}

// TestReadContentResemblingHeadersStaysFramed proves the size-framing defeats a
// desync attack: a blob whose CONTENT contains lines that look exactly like
// batch headers ("<oid> blob <n>") and embedded newlines must be returned
// verbatim, and the NEXT spec's real header must still parse — content is never
// re-interpreted as protocol.
func TestReadContentResemblingHeadersStaysFramed(t *testing.T) {
	evil := "0000000000000000000000000000000000000000 blob 999\nFAKE HEADER\nmore\n"
	stream := fmt.Sprintf("aaaaaaaa blob %d\n%s\n", len(evil), evil) + // record A: evil content
		"bbbbbbbb blob 5\nhello\n" // record B: a normal blob right after
	br := newFakeReader(stream)
	m, err := br.Read([]string{"A", "B"})
	if err != nil {
		t.Fatalf("framed content unexpectedly errored: %v", err)
	}
	if m["A"] != evil {
		t.Fatalf("header-resembling content corrupted:\n got  %q\n want %q", m["A"], evil)
	}
	if m["B"] != "hello" {
		t.Fatalf("record after header-resembling content desynced: B=%q want %q", m["B"], "hello")
	}
}
