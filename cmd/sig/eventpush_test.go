package main

// Tests for `sig serve -event-url` (issue #117). The properties under test are
// the ones an integration is entitled to rely on and the ones a broken receiver
// must NOT be able to break: order, attribution, a verifiable signature, and —
// above all — that no receiver behavior reaches the run.
//
// Nothing here waits out a duration to produce an interleaving: a receiver that
// must hang blocks on a channel the test closes, and a delivery the test must
// observe is awaited on a channel, not slept past.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// received is one POST a test receiver captured, kept as the RAW body bytes
// alongside the decoded envelope: the signature is defined over those exact
// bytes, so re-encoding the envelope to check it would test the wrong thing.
type received struct {
	body []byte
	sig  string
	env  eventEnvelope
}

// eventReceiver is a test HTTP receiver. respond decides each request's status
// (nil = 200) and may block; it runs on the server's own goroutine.
type eventReceiver struct {
	ts *httptest.Server

	mu   sync.Mutex
	got  []received
	seen chan struct{} // signalled (non-blocking) after every captured POST
}

func newEventReceiver(t *testing.T, respond func(n int) int) *eventReceiver {
	t.Helper()
	r := &eventReceiver{seen: make(chan struct{}, 1024)}
	r.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		n := len(r.got)
		rec := received{body: body, sig: req.Header.Get(eventPushSigHeader)}
		_ = json.Unmarshal(body, &rec.env)
		r.got = append(r.got, rec)
		r.mu.Unlock()
		select {
		case r.seen <- struct{}{}:
		default:
		}
		code := http.StatusOK
		if respond != nil {
			code = respond(n)
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(r.ts.Close)
	return r
}

func (r *eventReceiver) events() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.got...)
}

// names returns the pushed event names in delivery order.
func (r *eventReceiver) names(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, rec := range r.events() {
		var ev struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(rec.env.Event, &ev); err != nil {
			t.Fatalf("pushed event is not JSON: %s: %v", rec.body, err)
		}
		out = append(out, ev.Event)
	}
	return out
}

// waitFor blocks until the receiver has captured at least n POSTs. It consumes
// the receiver's signal channel rather than polling on a timer, so it adds no
// timing assumption of its own; the deadline is only a failure bound.
func (r *eventReceiver) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		r.mu.Lock()
		have := len(r.got)
		r.mu.Unlock()
		if have >= n {
			return
		}
		select {
		case <-r.seen:
		case <-deadline:
			t.Fatalf("receiver got %d events, want >= %d", have, n)
		}
	}
}

// newPushTestServer builds a server with event push configured, on a context
// the test cancels — the pusher's goroutine ends with it, so no test leaks one.
// queue is the delivery backlog (0 = the production eventPushQueue).
func newPushTestServer(t *testing.T, url, secret string, queue int, repos ...string) (*server, *httptest.Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := newServer(ctx, serverConfig{repos: repos, envMode: envModeInherit, eventURL: url, eventSecret: secret, eventQueue: queue})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// TestEventPushDeliversRunInOrder is the core acceptance: a real run's events
// reach the receiver, in emission order, attributed to the run and cell. The
// order assertion is against the run's OWN events.ndjson rather than a
// hand-written list, so it holds whatever the vocabulary grows into.
func TestEventPushDeliversRunInOrder(t *testing.T) {
	_, repo := makeGoRepo(t)
	rcv := newEventReceiver(t, nil)
	s, ts := newPushTestServer(t, rcv.ts.URL, "", 0, repo)

	var created struct{ RunID string }
	doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:   repo,
		Tasks:  []taskSpec{{ID: "t1"}},
		Agent:  writeFileAgent("push.txt"),
		Verify: "true",
	}, &created)
	got := pollRun(t, ts, "", created.RunID)
	if got.Status != "done" {
		t.Fatalf("run status %q, want done", got.Status)
	}

	_, dir, ok := s.findRunDir(created.RunID)
	if !ok {
		t.Fatal("run dir missing")
	}
	data, err := os.ReadFile(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("events.ndjson line %q: %v", line, err)
		}
		want = append(want, rec.Event)
	}
	if len(want) == 0 {
		t.Fatal("run wrote no events at all")
	}
	rcv.waitFor(t, len(want))

	if got := rcv.names(t); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pushed events\n got %v\nwant %v (events.ndjson order)", got, want)
	}
	for _, rec := range rcv.events() {
		if rec.env.RunID != created.RunID || rec.env.Cell != s.cells[0].cell.ID() {
			t.Fatalf("envelope attribution = {runId:%q cell:%q}, want {%q %q}", rec.env.RunID, rec.env.Cell, created.RunID, s.cells[0].cell.ID())
		}
		if rec.sig != "" {
			t.Fatalf("unsigned pusher sent a signature header %q", rec.sig)
		}
	}
	if st := s.eventPush.stats(); st.Sent != int64(len(want)) || st.Dropped != 0 || st.Failed != 0 {
		t.Fatalf("stats = %+v, want sent=%d dropped=0 failed=0", st, len(want))
	}
}

// TestEventPushSignatureVerifies checks the signature against the DOCUMENTED
// input — the raw request body bytes, nothing else — the way a receiver would,
// and that the header is absent when no secret is configured (covered above).
func TestEventPushSignatureVerifies(t *testing.T) {
	_, repo := makeGoRepo(t)
	rcv := newEventReceiver(t, nil)
	const secret = "shared-secret-value"
	s, ts := newPushTestServer(t, rcv.ts.URL, secret, 0, repo)

	var created struct{ RunID string }
	doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:  repo,
		Tasks: []taskSpec{{ID: "t1"}},
		Agent: writeFileAgent("signed.txt"),
	}, &created)
	pollRun(t, ts, "", created.RunID)
	rcv.waitFor(t, 1)

	for _, rec := range rcv.events() {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(rec.body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if rec.sig != want {
			t.Fatalf("signature %q, want %q for body %s", rec.sig, want, rec.body)
		}
		// A different key must NOT verify — otherwise the check above would pass
		// for any signature-shaped string.
		other := hmac.New(sha256.New, []byte(secret+"x"))
		other.Write(rec.body)
		if rec.sig == "sha256="+hex.EncodeToString(other.Sum(nil)) {
			t.Fatal("signature verifies under the wrong key")
		}
	}
	if st := s.eventPush.stats(); st.Sent == 0 {
		t.Fatalf("stats = %+v, want some sent", st)
	}
}

// TestEventPushHangingReceiverCannotReachTheRun is the fail-open acceptance: a
// receiver that never answers must not change the run's outcome, and must not
// hold the run open. The receiver blocks until the run has already reached a
// terminal state — the rendezvous, in place of any sleep — so a pusher that
// blocked the emitter would stall the run here rather than pass slowly.
//
// The queue is deliberately ONE deep. At the production 256 the buffer alone
// absorbs a small run's whole stream, and this test would pass even with the
// non-blocking send removed — i.e. it would prove nothing. A queue of 1 fills on
// the second event, which is where fail-open is the only thing keeping the run
// moving.
func TestEventPushHangingReceiverCannotReachTheRun(t *testing.T) {
	_, repo := makeGoRepo(t)
	release := make(chan struct{})
	var once sync.Once
	rcv := newEventReceiver(t, func(n int) int {
		<-release
		return http.StatusOK
	})
	// Registered AFTER the receiver so LIFO cleanup releases the wedged handler
	// BEFORE httptest.Close waits on it — otherwise a FAILING run (which is what
	// this test is here to catch) deadlocks in cleanup instead of reporting.
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	_, ts := newPushTestServer(t, rcv.ts.URL, "", 1, repo)

	var created struct{ RunID string }
	doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:   repo,
		Tasks:  []taskSpec{{ID: "t1"}},
		Agent:  writeFileAgent("hang.txt"),
		Verify: "true",
	}, &created)
	got := pollRun(t, ts, "", created.RunID)
	if got.Status != "done" {
		t.Fatalf("run status %q with a hanging receiver, want done", got.Status)
	}
	if got.Report == nil || got.Report.Integrate.FinalSHA == "" {
		t.Fatal("hanging receiver cost the run its landing")
	}
	once.Do(func() { close(release) })
}

// TestEventPushFailingReceiverCannotReachTheRun: a receiver that 500s every
// time burns the retry budget and gives up. The run still lands, and the loss
// is COUNTED as failed rather than reported as delivered.
func TestEventPushFailingReceiverCannotReachTheRun(t *testing.T) {
	_, repo := makeGoRepo(t)
	rcv := newEventReceiver(t, func(int) int { return http.StatusInternalServerError })
	s, ts := newPushTestServer(t, rcv.ts.URL, "", 0, repo)

	var created struct{ RunID string }
	doJSON(t, "POST", ts.URL+"/runs", "", runRequest{
		Cell:   repo,
		Tasks:  []taskSpec{{ID: "t1"}},
		Agent:  writeFileAgent("red.txt"),
		Verify: "true",
	}, &created)
	got := pollRun(t, ts, "", created.RunID)
	if got.Status != "done" {
		t.Fatalf("run status %q with a 500ing receiver, want done", got.Status)
	}
	// Every attempt is retried to the budget, so the first event alone is POSTed
	// eventPushAttempts times before it is counted failed.
	rcv.waitFor(t, eventPushAttempts)

	deadline := time.After(30 * time.Second)
	for {
		st := s.eventPush.stats()
		if st.Failed > 0 {
			if st.Sent != 0 {
				t.Fatalf("stats = %+v: a receiver that only 500s must never count a send", st)
			}
			break
		}
		select {
		case <-rcv.seen:
		case <-deadline:
			t.Fatalf("stats = %+v, want failed > 0", st)
		}
	}
}

// TestEventPushDropsAreCounted forces the overflow path: with the receiver
// wedged, the queue fills and every further line is dropped — and counted. The
// writes go through the sink exactly as an emitter's do, so this is the same
// path a real run takes when a receiver falls behind.
func TestEventPushDropsAreCounted(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	rcv := newEventReceiver(t, func(int) int {
		<-release
		return http.StatusOK
	})
	t.Cleanup(func() { once.Do(func() { close(release) }) }) // after the receiver: see the LIFO note above

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := startEventPusher(ctx, rcv.ts.URL, "", eventPushQueue)
	sink := p.sink("run-1", "cell-1")

	// One more than the queue can hold plus the one in flight: the overflow is
	// arithmetic, not timing — nothing can leave the queue while the receiver is
	// wedged, so the surplus MUST be dropped.
	const surplus = 8
	for i := 0; i < eventPushQueue+surplus+1; i++ {
		if _, err := sink.Write([]byte(`{"event":"probe","ts":"now"}` + "\n")); err != nil {
			t.Fatalf("sink.Write returned an error: %v — the sink must never fail its caller", err)
		}
	}
	if st := p.stats(); st.Dropped < surplus-1 {
		t.Fatalf("stats = %+v, want dropped >= %d", st, surplus-1)
	}
	once.Do(func() { close(release) })
}

// TestEventPushGoroutineEndsWithShutdown: the delivery goroutine's lifetime is
// exactly the daemon context's. Waiting on p.stopped (closed by the goroutine
// itself as it returns) is the leak check — a goroutine that outlived shutdown
// never closes it.
func TestEventPushGoroutineEndsWithShutdown(t *testing.T) {
	rcv := newEventReceiver(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	p := startEventPusher(ctx, rcv.ts.URL, "", eventPushQueue)

	sink := p.sink("run-1", "cell-1")
	sink.Write([]byte(`{"event":"probe","ts":"now"}` + "\n")) //nolint:errcheck // the sink never errors
	rcv.waitFor(t, 1)

	cancel()
	select {
	case <-p.stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("delivery goroutine outlived its context")
	}
}

// TestServeShutdownDrainsWithEventPush drives the real listener + serve()
// shutdown path with push configured: serve returns cleanly, the pusher's
// goroutine is gone, and the summary line reports the tally.
func TestServeShutdownDrainsWithEventPush(t *testing.T) {
	_, repo := makeGoRepo(t)
	rcv := newEventReceiver(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	s, err := newServer(ctx, serverConfig{repos: []string{repo}, envMode: envModeInherit, eventURL: rcv.ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	done := make(chan int, 1)
	go func() {
		code, _ := s.serve(out, ln, func() {})
		done <- code
	}()

	var created struct{ RunID string }
	if code := doJSON(t, "POST", "http://"+ln.Addr().String()+"/runs", "", runRequest{
		Cell:  repo,
		Tasks: []taskSpec{{ID: "t1"}},
		Agent: writeFileAgent("drain.txt"),
	}, &created); code != http.StatusAccepted {
		t.Fatalf("POST status %d", code)
	}
	rcv.waitFor(t, 1) // the run is provably underway and pushing

	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("serve returned %d, want exitOK", code)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not shut down")
	}
	select {
	case <-s.eventPush.stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("delivery goroutine outlived serve")
	}
	if !strings.Contains(out.String(), "sig serve: event push:") {
		t.Fatalf("shutdown output has no event-push tally:\n%s", out.String())
	}
}

// TestEventPushHealthReportsCounters: the drop tally is READABLE, not merely
// recorded — GET /health carries it, and never the configured URL.
func TestEventPushHealthReportsCounters(t *testing.T) {
	_, repo := makeGoRepo(t)
	rcv := newEventReceiver(t, nil)
	_, ts := newPushTestServer(t, rcv.ts.URL, "", 0, repo)

	var resp map[string]any
	if code := doJSON(t, "GET", ts.URL+"/health", "", nil, &resp); code != http.StatusOK {
		t.Fatalf("health status %d", code)
	}
	push, ok := resp["eventPush"].(map[string]any)
	if !ok {
		t.Fatalf("health has no eventPush object: %v", resp)
	}
	for _, k := range []string{"sent", "dropped", "failed"} {
		if _, ok := push[k]; !ok {
			t.Fatalf("health eventPush missing %q: %v", k, push)
		}
	}
	for k, v := range push {
		if s, ok := v.(string); ok && strings.Contains(s, rcv.ts.URL) {
			t.Fatalf("health leaks the receiver URL in %q: %v", k, v)
		}
	}

	// Not configured => the object is absent entirely, not zeroed.
	_, plain := newTestServer(t, "", repo)
	var bare map[string]any
	doJSON(t, "GET", plain.URL+"/health", "", nil, &bare)
	if _, present := bare["eventPush"]; present {
		t.Fatalf("health reports eventPush with no -event-url: %v", bare)
	}
}

// TestValidateEventURL holds the flag-parse-time refusals: an unusable or
// unsafe target must fail at startup, never at the first POST.
func TestValidateEventURL(t *testing.T) {
	ok := []string{
		"https://hooks.example.com/sigbound",
		"https://example.com:8443/x?y=1",
		"http://127.0.0.1:9000/events",
		"http://localhost:9000/events",
		"http://[::1]:9000/events",
	}
	for _, u := range ok {
		if err := validateEventURL(u); err != nil {
			t.Fatalf("validateEventURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []struct{ url, want string }{
		{"", "scheme"},
		{"example.com/hook", "scheme"},                      // no scheme at all
		{"file:///etc/passwd", "scheme"},                    // not http(s)
		{"gopher://example.com/x", "scheme"},                //
		{"https://", "no host"},                             //
		{"https://user:pw@example.com/hook", "credentials"}, // secrets don't belong in a URL
		{"http://example.com/hook", "plaintext"},            // remote plaintext
		{"http://10.0.0.5:9000/hook", "plaintext"},          //
	}
	for _, c := range bad {
		err := validateEventURL(c.url)
		if err == nil {
			t.Fatalf("validateEventURL(%q) = nil, want an error", c.url)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("validateEventURL(%q) = %v, want it to mention %q", c.url, err, c.want)
		}
	}
}

// TestServeEventFlagsRejectBadConfig: the two ways a signed integration can be
// silently unsigned or undeliverable are refused before the daemon binds.
func TestServeEventFlagsRejectBadConfig(t *testing.T) {
	_, repo := makeGoRepo(t)
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"bad url", []string{"-repos", repo, "-event-url", "ftp://example.com/x"}, "scheme"},
		{"secret without url", []string{"-repos", repo, "-event-secret-env", "SIGBOUND_TEST_EVENT_SECRET"}, "nothing to sign"},
		{"empty secret var", []string{"-repos", repo, "-event-url", "https://example.com/x", "-event-secret-env", "SIGBOUND_TEST_EVENT_SECRET_UNSET"}, "is empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, err := runServe(io.Discard, c.argv)
			if err == nil {
				t.Fatalf("runServe(%v) = %d, nil; want an error", c.argv, code)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("runServe(%v) error = %v, want it to mention %q", c.argv, err, c.want)
			}
		})
	}
}
