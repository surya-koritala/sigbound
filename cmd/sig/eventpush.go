// Event push (issue #117): `sig serve -event-url URL` POSTs every NDJSON event
// line to one HTTP receiver as it is emitted — the integration seam for chat
// notifications, dashboards and downstream automation, so a consumer doesn't
// have to poll GET /runs/{id}/events.
//
// FAIL-OPEN IS THE WHOLE CONTRACT. Delivery is a bounded queue drained by ONE
// goroutine; the emitting side hands a line over without ever blocking on it. A
// receiver that 500s, hangs, or is simply gone cannot change a run's outcome,
// its duration, or what lands — the queue fills and the overflow is DROPPED.
// Every drop is COUNTED and reported (GET /health's eventPush object, plus a
// summary line at shutdown): an integration that quietly loses events is the
// failure this must not have.
//
// ORDER: one goroutine, one in-flight POST at a time, FIFO queue — a receiver
// sees events in emission order. That is also exactly why a slow receiver costs
// drops rather than reordering: the queue is never overtaken.
//
// SIGNATURE: with -event-secret-env NAME set, every POST carries
// `X-Sigbound-Signature: sha256=<hex>` — the HMAC-SHA256 of the RAW REQUEST BODY
// BYTES, keyed by that env var's value. Nothing else is in the signing input: no
// timestamp, no header, no canonicalization, so a receiver verifies by HMACing
// the bytes it read, before parsing them. It authenticates the body, NOT its
// freshness — an identical body replays with a valid signature, so a receiver
// that cares dedupes on the event's own runId + ts.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

const (
	// eventPushQueue is the in-memory backlog one pusher will hold before it
	// starts dropping. It bounds memory AND how far behind a receiver may fall
	// before the loss becomes visible; a bigger buffer would only delay the
	// drop, not prevent it.
	eventPushQueue = 256
	// eventPushAttempts/eventPushBackoff/eventPushTimeout are the RETRY BUDGET —
	// the only thing a hostile receiver can spend. Worst case per event:
	// 3 × 5s of request plus 200ms + 400ms of backoff, after which the event is
	// counted failed and the queue moves on. Deliberately constants, not flags:
	// a knob here tunes how long a broken receiver stalls the QUEUE, which is
	// the wrong thing to invite an operator to enlarge.
	eventPushAttempts = 3
	eventPushBackoff  = 200 * time.Millisecond
	eventPushTimeout  = 5 * time.Second
	// eventPushSigHeader carries "sha256=<hex>"; see this file's doc comment for
	// the exact signing input.
	eventPushSigHeader = "X-Sigbound-Signature"
)

// eventPost is one queued delivery: the exact NDJSON line an emitter wrote,
// plus the attribution a receiver cannot recover from the line itself (events
// carry no run id, and two cells run concurrently).
type eventPost struct {
	runID string
	cell  string
	line  []byte
}

// eventEnvelope is the POSTed body. The event is embedded VERBATIM as a nested
// object rather than merged into the envelope: `event` fields are a public
// vocabulary (docs/USAGE.md's Events table) that already includes a `cell` key
// on the watch stream, and merging would silently overwrite it.
type eventEnvelope struct {
	RunID string          `json:"runId,omitempty"`
	Cell  string          `json:"cell,omitempty"`
	Event json.RawMessage `json:"event"`
}

// eventPusher owns the queue, the single delivery goroutine and the counters
// for one daemon. Build it with startEventPusher; a nil *eventPusher is a valid
// no-op receiver on every method here, so "no -event-url" needs no branch at
// any call site.
type eventPusher struct {
	url    string
	secret string // "" => unsigned deliveries
	client *http.Client
	q      chan eventPost
	// stopped closes when the delivery goroutine returns — the shutdown
	// assertion (and the test's leak check) rather than a timing guess.
	stopped chan struct{}

	sent    atomic.Int64
	dropped atomic.Int64
	failed  atomic.Int64
}

// startEventPusher builds a pusher and starts its one delivery goroutine, which
// runs until ctx is cancelled. ctx is the daemon's baseCtx, so shutdown both
// aborts an in-flight POST and ends the goroutine — it never outlives serve.
//
// queue is eventPushQueue everywhere but a test: the fail-open behavior only
// engages once the buffer is FULL, so proving it needs a buffer a test can
// actually fill.
func startEventPusher(ctx context.Context, rawURL, secret string, queue int) *eventPusher {
	p := &eventPusher{
		url:     rawURL,
		secret:  secret,
		client:  &http.Client{Timeout: eventPushTimeout},
		q:       make(chan eventPost, queue),
		stopped: make(chan struct{}),
	}
	go p.loop(ctx)
	return p
}

// loop is the single consumer: one POST at a time, in queue order.
func (p *eventPusher) loop(ctx context.Context) {
	defer close(p.stopped)
	for {
		select {
		case <-ctx.Done():
			// Shutdown. Whatever is still queued never ships; count it so the
			// tally stays honest. Exact except at this boundary itself — a run
			// still draining can emit after this read, and those lines land in
			// the queue nobody is reading and go uncounted.
			p.dropped.Add(int64(len(p.q)))
			return
		case it := <-p.q:
			p.deliver(ctx, it)
		}
	}
}

// deliver POSTs one event, retrying within the bounded budget. Every exit path
// counts exactly one of sent/failed, so sent+failed+dropped accounts for every
// line that ever entered the queue.
func (p *eventPusher) deliver(ctx context.Context, it eventPost) {
	body, err := json.Marshal(eventEnvelope{RunID: it.runID, Cell: it.cell, Event: json.RawMessage(it.line)})
	if err != nil {
		// The line came straight from json.Encoder, so this is unreachable in
		// practice; count it rather than pretend it shipped.
		p.failed.Add(1)
		return
	}
	backoff := eventPushBackoff
	for attempt := 1; ; attempt++ {
		ok, retry := p.post(ctx, body)
		if ok {
			p.sent.Add(1)
			return
		}
		if !retry || attempt >= eventPushAttempts {
			break
		}
		select {
		case <-ctx.Done():
			p.failed.Add(1)
			return
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	p.failed.Add(1)
}

// post makes ONE attempt. retry distinguishes "the receiver might yet accept
// this" (transport error, 429, 5xx) from a verdict resending the identical body
// cannot change (any other 4xx) — retrying a rejected body just spends the
// budget and delays the queue.
func (p *eventPusher) post(ctx context.Context, body []byte) (ok, retry bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return false, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sigbound/"+Version)
	if p.secret != "" {
		mac := hmac.New(sha256.New, []byte(p.secret))
		mac.Write(body) //nolint:errcheck // hash.Hash.Write never returns an error
		req.Header.Set(eventPushSigHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false, true
	}
	// Drain a bounded prefix so the connection can be reused; the body is not
	// part of the contract — only the status is.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close() //nolint:errcheck // nothing to recover on a close error
	switch {
	case resp.StatusCode < 300:
		return true, false
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return false, true
	default:
		return false, false
	}
}

// sink returns the io.Writer an eventEmitter fans its lines onto for one run.
// runID/cell are empty for the daemon-level watch stream, whose lines name
// their own cell. A nil pusher returns a nil Writer (a genuinely nil interface,
// not a typed nil) — that is the "-event-url unset" case, and every caller
// leaves a nil writer out of the emitter entirely.
func (p *eventPusher) sink(runID, cell string) io.Writer {
	if p == nil {
		return nil
	}
	return &eventSink{p: p, runID: runID, cell: cell}
}

type eventSink struct {
	p     *eventPusher
	runID string
	cell  string
}

// Write receives exactly ONE complete NDJSON line: json.Encoder.Encode issues a
// single Write per value, and eventEmitter holds its mutex across that call. It
// never blocks and never reports an error — it runs on the RUN's goroutine
// under that mutex, so blocking here would block the run, which is the failure
// this file exists to prevent. A full queue is a counted drop.
func (s *eventSink) Write(b []byte) (int, error) {
	// Copy: the encoder reuses its buffer as soon as this returns.
	line := append([]byte(nil), bytes.TrimRight(b, "\n")...)
	if len(line) == 0 {
		return len(b), nil
	}
	// Once the drain goroutine has returned nobody will ever read the queue
	// again, so a send that SUCCEEDS here is still a loss -- and a silent one,
	// because the shutdown tally read len(q) once at cancel and stopped. A
	// draining run emitting after that is the designed path, not an exotic
	// race (s.wg.Wait() exists for exactly it), so check stopped first and
	// count those lines as dropped rather than letting them vanish into a
	// buffer with no reader.
	select {
	case <-s.p.stopped:
		s.p.dropped.Add(1)
		return len(b), nil
	default:
	}
	select {
	case s.p.q <- eventPost{runID: s.runID, cell: s.cell, line: line}:
	default:
		s.p.dropped.Add(1)
	}
	return len(b), nil
}

// eventPushStats is GET /health's eventPush object, and the shutdown summary.
// The configured URL is deliberately NOT reported: a webhook URL is frequently
// itself a secret (the token lives in its path), and /health is unauthenticated
// in loopback dev mode.
type eventPushStats struct {
	Sent    int64 `json:"sent"`
	Dropped int64 `json:"dropped"`
	Failed  int64 `json:"failed"`
}

// stats snapshots the counters, or nil when no pusher is configured. The three
// are read independently, so a snapshot taken mid-flight can be off by the
// events in transit — it is a tally, not a transactional view.
func (p *eventPusher) stats() *eventPushStats {
	if p == nil {
		return nil
	}
	return &eventPushStats{Sent: p.sent.Load(), Dropped: p.dropped.Load(), Failed: p.failed.Load()}
}

// validateEventURL refuses a target at FLAG-PARSE time rather than at the first
// POST: a daemon that accepted -event-url and then silently delivered nothing
// for its whole life is the worst outcome available here.
//
// Plaintext http is confined to a loopback receiver on purpose. Event bodies
// carry repo paths, branch names, SHAs and (on park_failed) verify output, and
// the HMAC signs the body without encrypting it — an https receiver, or a proxy
// terminating TLS on localhost, is the supported way to reach a remote one.
func validateEventURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("-event-url %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("-event-url %q: scheme must be http or https, not %q", raw, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("-event-url %q: no host", raw)
	}
	if u.User != nil {
		return fmt.Errorf("-event-url %q: credentials in the URL are not accepted; authenticate the receiver with -event-secret-env instead", raw)
	}
	if u.Scheme == "http" && !hostIsLoopback(u.Hostname()) {
		return fmt.Errorf("-event-url %q: plaintext http is only accepted for a loopback receiver — event bodies carry repo paths, branch names and command output. Use https, or terminate TLS at a loopback proxy", raw)
	}
	return nil
}
