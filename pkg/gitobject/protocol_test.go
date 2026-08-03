package gitobject

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func fakeReader(stream string) *Reader {
	r := &Reader{
		stdin: nopWriteCloser{}, stdout: bufio.NewReader(strings.NewReader(stream)),
		stderr: &capped{limit: stderrMax}, cancel: func() {}, gate: make(chan struct{}, 1),
		closeDone: make(chan struct{}),
	}
	r.gate <- struct{}{}
	return r
}

func exactContent(t *testing.T, oid string, max int64) Request {
	t.Helper()
	r, err := Exact(oid, Blob, true, max)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestProtocolTruncationNeverReturnsPartialContent(t *testing.T) {
	oid := strings.Repeat("a", 40)
	cases := []struct {
		name, stream   string
		max, aggregate int64
	}{
		{name: "content shorter than declared size", stream: fmt.Sprintf("%s blob 10\n%s blob 10\nABCD", oid, oid)},
		{name: "content present but trailing newline truncated", stream: fmt.Sprintf("%s blob 5\n%s blob 5\nhello", oid, oid)},
		{name: "content header truncated", stream: fmt.Sprintf("%s blob 5\n%s blob 5", oid, oid)},
		{name: "empty response", stream: ""},
		{name: "negative info size", stream: fmt.Sprintf("%s blob -1\n", oid)},
		{
			"address-space overflow",
			fmt.Sprintf("%s blob %d\n%s blob %d\n", oid, int64(^uint64(0)>>1), oid, int64(^uint64(0)>>1)),
			int64(^uint64(0) >> 1), int64(^uint64(0) >> 1),
		},
		{name: "content metadata changed", stream: fmt.Sprintf("%s blob 5\n%s blob 6\nhello\n", oid, oid)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.max == 0 {
				tc.max, tc.aggregate = 100, 100
			}
			r := fakeReader(tc.stream)
			results, err := r.Read(t.Context(), []Request{exactContent(t, oid, tc.max)}, tc.aggregate)
			if err == nil || results != nil {
				t.Fatalf("malformed stream returned results=%+v err=%v", results, err)
			}
			if _, err := r.Read(t.Context(), []Request{exactContent(t, oid, 100)}, 100); err == nil {
				t.Fatal("poisoned reader accepted a later call")
			}
		})
	}
}

func TestProtocolDropsEarlierContentWhenLaterRecordTruncates(t *testing.T) {
	a := strings.Repeat("a", 40)
	b := strings.Repeat("b", 40)
	stream := fmt.Sprintf("%s blob 2\n%s blob 100\n%s blob 2\nhi\n%s blob 100\nxy", a, b, a, b)
	r := fakeReader(stream)
	requests := []Request{exactContent(t, a, 100), exactContent(t, b, 100)}
	results, err := r.Read(t.Context(), requests, 200)
	if err == nil || results != nil {
		t.Fatalf("later truncation leaked earlier content: results=%+v err=%v", results, err)
	}
}

func TestProtocolContentThatLooksLikeHeadersStaysFramed(t *testing.T) {
	a := strings.Repeat("a", 40)
	b := strings.Repeat("b", 40)
	evil := strings.Repeat("0", 40) + " blob 999\nFAKE HEADER\nmore\n"
	stream := fmt.Sprintf("%s blob %d\n%s blob 5\n%s blob %d\n%s\n%s blob 5\nhello\n", a, len(evil), b, a, len(evil), evil, b)
	r := fakeReader(stream)
	requests := []Request{exactContent(t, a, int64(len(evil))), exactContent(t, b, 5)}
	results, err := r.Read(t.Context(), requests, int64(len(evil)+5))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(results[0].Content, []byte(evil)) || string(results[1].Content) != "hello" {
		t.Fatalf("framing changed content: %q / %q", results[0].Content, results[1].Content)
	}
}

func TestProtocolRejectsMismatchedMissingEcho(t *testing.T) {
	requested := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	r := fakeReader(other + " missing\n")
	results, err := r.Read(t.Context(), []Request{exactContent(t, requested, 100)}, 100)
	if err == nil || results != nil || !errors.Is(err, ErrPoisoned) {
		t.Fatalf("mismatched missing echo returned results=%+v err=%v", results, err)
	}
}
