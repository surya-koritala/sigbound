// sig serve is a thin, single-process HTTP wrapper over the SAME orchestration
// driveRun runs — it forks no engine. Point it at one or more git repos and it
// opens each as a cell at startup; POST /runs drives a run through the exact
// driveRun machinery `sig run` uses, so the verify-gate promise ("nothing lands
// unless -verify passes") holds by construction — serve adds no new landing
// path. Every run's full report and event stream are written under the TARGET
// repo's .git/sigbound/runs/<runId>/ (the same .git/sigbound storage precedent
// as -verify-cache), so history survives a restart: the GET endpoints read from
// disk.
//
// CRASH SAFETY: alongside report.json/events.ndjson, every run dir carries a
// status.json phase marker (queued/running/done/error), written atomically at
// each transition, plus a request.json journal of the POSTed request body
// written at accept time. On startup, any run a PRIOR process left
// queued/running (its PID is gone) is rewritten to status "interrupted" — so a
// daemon killed mid-run is reported honestly instead of looking like it's still
// running forever. See docs/USAGE.md's "Crash recovery" section.
//
//	sig serve -addr 127.0.0.1:7777 -repos /path/a,/path/b [-token-env NAME] [-allow-remote]
//
// POSTURE: this is a SINGLE-PROCESS, SINGLE-USER daemon, NOT a multi-tenant
// service. It binds loopback by default and REFUSES a non-loopback -addr unless
// -allow-remote is also set. It ships no TLS and no user model: if you expose it
// beyond localhost, putting TLS and network auth in front of it (a reverse
// proxy) is YOUR job. Auth here is one shared bearer token, nothing more.
//
// AUTH: if the env var named by -token-env (default SIGBOUND_SERVE_TOKEN) is
// set, every request must carry `Authorization: Bearer <token>` (constant-time
// compared) — EXCEPT the /ui and /ui/ shell itself (see handleUI): a browser
// navigation there can never carry that header, so the static, data-free page
// is served unauthenticated and carries the token on its own fetches after.
// Every /runs/... data endpoint (including /ui's own /runs/.../flagged data)
// stays gated. If the token env var is unset AND the bind is loopback, auth is
// off entirely (dev mode). A non-loopback bind REQUIRES the token — serve
// refuses to start without it.
//
// Endpoints (JSON in, JSON out):
//
//	GET  /health           -> {ok, version, cells:[{id, repo}]}
//	POST /runs             -> Content-Type: application/json required; 202 {runId,...}; runs ASYNC via driveRun
//	GET  /runs             -> {runs:[{id, cell, status, startedAt, finalSHA?}]}
//	GET  /runs/{id}        -> {status: queued|running|done|error|interrupted, report?, usage?}
//	GET  /runs/{id}/events -> the run's events.ndjson (application/x-ndjson)
//	GET  /runs/{id}/usage  -> that run's metering record (see usage.go)
//	GET  /usage            -> usage totals across all run history, + a per-cell rollup
//	GET  /runs/{id}/flagged                    -> {runId, cell, flagged:[{branch, paths}]}
//	GET  /runs/{id}/flagged/{branch}/{path...} -> {path, base, ours, theirs, baseSHA} (three sides)
//	GET  /ui (and /ui/)    -> the read-only conflict-review HTML page (see handleUI)
//
// The /flagged endpoints + /ui are the conflict-review surface (issue #62): a
// READ-ONLY view of the branches a run flagged (real conflicts a resolver
// declined, or none was set) so a human can inspect the three sides before
// resolving on the CLI. They add NO landing path — nothing merges or lands from
// the browser; the safe pattern stays sig run/integrate.
//
// One run per cell at a time: a second POST for a cell whose run is still in
// flight gets 409 Conflict (a run moves the base ref; serializing is the only
// honest semantics). DIFFERENT cells run fully in parallel — the sharding payoff.
//
// QUOTAS: managed-layer ceilings, all opt-in via server flags (0 = unlimited,
// today's behavior byte-identical): -max-agents-per-run (400 on POST /runs
// before any run starts), -max-run-time (a request's -budget, capped via
// min()), -max-parallel-agents (a request's -parallel-agents, capped the
// same min() way), -max-concurrent-runs (429 across ALL cells, on top of the
// per-cell 409). METERING is always on — a usage.json is written alongside
// every run's report.json, derived from data driveRun already tracks.
// Neither is a biller: no price, currency, or external metering call. See
// docs/USAGE.md's "Quotas and metering" section.
//
// EVENT PUSH: -event-url mirrors every run's NDJSON event lines to one HTTP
// receiver as they are emitted (optionally HMAC-signed via -event-secret-env),
// so an integration doesn't have to poll GET /runs/{id}/events. Delivery is
// FAIL-OPEN and can never block or fail a run — a receiver that hangs costs
// dropped events, counted on GET /health. See eventpush.go and docs/USAGE.md's
// "Event push" section.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/surya-koritala/sigbound/cell"
	"github.com/surya-koritala/sigbound/internal/gitx"
)

// uiHTML is the single self-contained conflict-review page served at GET /ui.
// Vanilla HTML+CSS+JS, no framework, no external asset — CSP-safe and works
// offline (a daemon may be air-gapped). See handleUI and docs/USAGE.md.
//
//go:embed ui.html
var uiHTML []byte

// serveMaxBody bounds a POST /runs body so a client can't stream an unbounded
// request into the daemon. A run request is a handful of small fields; 1 MiB is
// generous.
const serveMaxBody = 1 << 20

// registeredCell is one opened cell plus its cached runs directory
// (<git-common-dir>/sigbound/runs), resolved once at startup so per-request
// handlers never re-shell `git rev-parse`.
type registeredCell struct {
	cell    *cell.Cell
	runsDir string
}

// server holds the run registry and per-cell run state for one `sig serve`
// process. Its zero value is not usable — build it with newServer.
type server struct {
	token   string          // "" => auth disabled (only permitted on a loopback bind)
	baseCtx context.Context // cancelled on shutdown; every in-flight run honors it

	// Environment policy is the OPERATOR's, set once via the server's -env-*
	// flags and applied to EVERY run — a request never gets to widen it (a
	// caller must not be able to say "inherit" and read the daemon's whole
	// env). The one exception the design allows is a request narrowing to a
	// stricter/explicit -env-mode; see buildParams.
	envMode     string
	envAgent    []string
	envResolver []string
	envVerify   []string
	envRepair   []string
	envPlanner  []string
	envPublish  []string

	cells []*registeredCell
	byKey map[string]*registeredCell // cell id AND absolute repo path -> cell

	// Managed-layer quotas (see docs/USAGE.md "Quotas and metering"), all
	// opt-in via server flags: 0 = unlimited, today's (#60) behavior
	// byte-identical. maxAgentsPerRun and maxConcurrentRuns are enforced in
	// handleCreateRun BEFORE a run starts (no run dir, no cell slot held on
	// rejection); maxRunTime is folded into a request's -budget via
	// min() in buildParams — a request can only make its own budget
	// stricter, never laxer than the server ceiling. maxParallelAgents caps
	// a request's -parallel-agents the same min() way — see issue #84.
	maxAgentsPerRun   int
	maxRunTime        time.Duration
	maxConcurrentRuns int
	maxParallelAgents int

	// Watch mode (issue #111), all zero/nil unless -watch is set. watchEvents is
	// the daemon-level event stream (the cell-scoped watch_* lines that belong to
	// no single run); watchOn gates POST /queue; queued holds each cell's
	// explicitly enqueued branches. See watch.go.
	watchOn     bool
	watchEvents *eventEmitter

	// Event push (issue #117), nil unless -event-url is set: every run's event
	// lines (and the watch stream's) are mirrored to one HTTP receiver as they
	// are emitted. Fail-open by construction — see eventpush.go.
	eventPush *eventPusher

	mu         sync.Mutex
	busy       map[string]bool       // cell id -> a run is in flight (per-cell 409)
	activeRuns int                   // runs in flight across ALL cells (maxConcurrentRuns' 429 counter)
	runs       map[string]*runRecord // runId -> live record for THIS process
	queued     map[string][]string   // cell id -> branches enqueued via POST /queue
	wg         sync.WaitGroup        // in-flight run goroutines AND watch loops, waited on shutdown
}

// runRecord is the in-memory truth for a run started by THIS process. Disk
// (status.json / report.json / events.ndjson under the run dir) is the durable
// record the GET endpoints fall back to for runs from a prior process (restart
// survival) — see diskRunStatus and recoverStaleRuns.
type runRecord struct {
	id        string
	cellID    string
	repo      string
	dir       string
	status    string // queued | running | done | error
	startedAt time.Time
	finalSHA  string
	errMsg    string
}

// serverConfig is newServer's input, split out from flag parsing so tests build
// a server directly (httptest) without a listener or signal loop.
type serverConfig struct {
	repos       []string
	token       string
	envMode     string
	envAgent    []string
	envResolver []string
	envVerify   []string
	envRepair   []string
	envPlanner  []string
	envPublish  []string

	// Managed-layer quotas; see server.maxAgentsPerRun's doc comment. 0 (the
	// zero value) is unlimited on every one of these.
	maxAgentsPerRun   int
	maxRunTime        time.Duration
	maxConcurrentRuns int
	maxParallelAgents int

	// Event push (issue #117): eventURL "" disables it entirely; eventSecret ""
	// delivers unsigned. The URL is validated at flag-parse time (see
	// validateEventURL) — newServer only starts the pusher. eventQueue is the
	// delivery backlog, 0 => eventPushQueue; only a test sets it (see
	// startEventPusher).
	eventURL    string
	eventSecret string
	eventQueue  int
}

// newServer opens every repo as a cell (fail-fast on any bad repo) and resolves
// each cell's runs directory once. baseCtx is handed to every run so a shutdown
// cancel propagates into driveRun (which already honors ctx via its -budget
// machinery).
func newServer(baseCtx context.Context, cfg serverConfig) (*server, error) {
	if len(cfg.repos) == 0 {
		return nil, errors.New("-repos is required (comma-separated repo paths)")
	}
	s := &server{
		token:             cfg.token,
		baseCtx:           baseCtx,
		envMode:           cfg.envMode,
		envAgent:          cfg.envAgent,
		envResolver:       cfg.envResolver,
		envVerify:         cfg.envVerify,
		envRepair:         cfg.envRepair,
		envPlanner:        cfg.envPlanner,
		envPublish:        cfg.envPublish,
		maxAgentsPerRun:   cfg.maxAgentsPerRun,
		maxRunTime:        cfg.maxRunTime,
		maxConcurrentRuns: cfg.maxConcurrentRuns,
		maxParallelAgents: cfg.maxParallelAgents,
		byKey:             map[string]*registeredCell{},
		busy:              map[string]bool{},
		runs:              map[string]*runRecord{},
		queued:            map[string][]string{},
	}
	// One pusher per daemon, started here so its goroutine's lifetime is exactly
	// baseCtx's — it cannot outlive the shutdown that cancels every run.
	if cfg.eventURL != "" {
		queue := cfg.eventQueue
		if queue <= 0 {
			queue = eventPushQueue
		}
		s.eventPush = startEventPusher(baseCtx, cfg.eventURL, cfg.eventSecret, queue)
	}
	ctx := context.Background()
	for _, repo := range cfg.repos {
		c, err := cell.Open(repo)
		if err != nil {
			return nil, fmt.Errorf("open cell %s: %w", repo, err)
		}
		if _, dup := s.byKey[c.ID()]; dup {
			return nil, fmt.Errorf("duplicate repo %s: cell id %s is already registered", repo, c.ID())
		}
		common, err := c.Git().GitCommonDir(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve git dir for %s: %w", repo, err)
		}
		rc := &registeredCell{cell: c, runsDir: filepath.Join(common, "sigbound", "runs")}
		s.cells = append(s.cells, rc)
		s.byKey[c.ID()] = rc
		s.byKey[c.Repo()] = rc
		// Startup recovery (issue #90): a run this cell's dir still shows
		// queued/running belongs to a process that is, by construction, not
		// this one (a fresh process never reuses its predecessor's PID) — no
		// goroutine of ours will ever finish it. Rewrite it to "interrupted"
		// now, before the server accepts any request, so GET reports it
		// honestly instead of "running" forever. See recoverStaleRuns.
		recoverStaleRuns(rc.runsDir, os.Getpid())
		// Parking (issue #109) deliberately survives that sweep — awaiting-ack is
		// durable, and recoverStaleRuns only ever rewrites queued/running. What a
		// restart DOES owe a parked run is the lazy ack-timeout: a daemon that was
		// down past a park's deadline must report it expired from its first
		// request onward, not from whenever someone next opens the inbox.
		expireParks(ctx, c.Git(), rc.runsDir)
	}
	return s, nil
}

// handler builds the request router. The enhanced (Go 1.22+) ServeMux does the
// method + path matching, so there is no hand-rolled routing. When a token is
// configured every request is bearer-checked first; with no token (dev mode on
// loopback) the mux is served directly.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /runs", s.handleCreateRun)
	mux.HandleFunc("GET /runs", s.handleListRuns)
	mux.HandleFunc("GET /runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /runs/{id}/events", s.handleRunEvents)
	mux.HandleFunc("GET /runs/{id}/usage", s.handleRunUsage)
	// Conflict-review surface (issue #62): the flagged-branch listing, the
	// three-sides detail for one flagged path, and the read-only HTML page that
	// renders them. The /runs/.../flagged* endpoints are gated exactly like every
	// other data route below. /ui and /ui/ are special-cased OUT of that gate
	// (see the dispatch below): a browser navigation can never carry an
	// Authorization header, so gating the page itself would 401 before the page
	// — and its own token field — ever loads. That's safe because handleUI's
	// shell is static and data-free: it fetches every run/flagged/etc. through
	// authenticated requests carrying a token typed into its own sessionStorage
	// field, so serving the shell unauthenticated leaks nothing.
	mux.HandleFunc("GET /runs/{id}/flagged", s.handleFlagged)
	mux.HandleFunc("GET /runs/{id}/flagged/{rest...}", s.handleFlaggedDetail)
	// Run parking (issue #109). These two POSTs are the ONLY mutating endpoints
	// besides POST /runs, and the only ones the review UI can reach — an ack does
	// not integrate anything new by itself, it releases a landing this daemon
	// already computed and verified. GET /inbox is the read side.
	mux.HandleFunc("POST /runs/{id}/ack", s.handleAck)
	mux.HandleFunc("POST /runs/{id}/reject", s.handleReject)
	mux.HandleFunc("GET /inbox", s.handleInbox)
	// Watch mode's one enqueue endpoint (issue #111). It starts nothing by
	// itself: it names existing branches for the next cycle, which drives the
	// same run POST /runs would. Only meaningful with -watch (400 otherwise).
	mux.HandleFunc("POST /queue", s.handleQueue)
	mux.HandleFunc("GET /ui", s.handleUI)
	mux.HandleFunc("GET /ui/", s.handleUI)
	mux.HandleFunc("GET /usage", s.handleUsageAggregate)
	// Read-only run-history surface (issue #110), the HTTP mirror of `sig log`.
	// Both routes call the SAME reader helpers the CLI uses (scanRuns /
	// resolveProvenance in log.go), so the two can never drift; they sit behind
	// the same bearer gate and read-only posture as every data route above.
	mux.HandleFunc("GET /log", s.handleLog)
	mux.HandleFunc("GET /log/sha/{sha}", s.handleLogSHA)
	if s.token == "" {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ui" || r.URL.Path == "/ui/" {
			mux.ServeHTTP(w, r)
			return
		}
		if !s.authOK(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized: send Authorization: Bearer <token>", codeUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// authOK constant-time-compares the request's bearer token against the
// configured one. Only reached when a token is set (see handler). A missing or
// malformed header, or any mismatch (including a length mismatch, which
// ConstantTimeCompare reports as 0 without leaking length via timing), fails.
func (s *server) authOK(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// ---- endpoints ----

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	type cellInfo struct {
		ID   string `json:"id"`
		Repo string `json:"repo"`
	}
	cells := make([]cellInfo, len(s.cells))
	for i, rc := range s.cells {
		cells[i] = cellInfo{ID: rc.cell.ID(), Repo: rc.cell.Repo()}
	}
	resp := map[string]any{
		"ok":      true,
		"version": Version,
		"cells":   cells,
	}
	// Event-push counters ride here (absent when -event-url isn't set): drops
	// have to be READABLE, not just recorded, or the fail-open posture becomes
	// silent loss. See eventPushStats.
	if st := s.eventPush.stats(); st != nil {
		resp["eventPush"] = st
	}
	writeJSON(w, http.StatusOK, resp)
}

// runRequest is the POST /runs body. Fields mirror `sig run`'s flags by name
// (camelCased); durations are Go duration strings ("30s", "2m"). Unknown fields
// are rejected (see handleCreateRun) so a typo'd knob fails loudly instead of
// silently doing nothing. Environment policy is deliberately NOT here — it is
// the operator's, set on the server (see server.envMode) — except EnvMode, which
// a request may set to override the server default per the documented posture.
type runRequest struct {
	Cell string `json:"cell"` // cell id OR repo path (required)
	Base string `json:"base"` // default "main"

	Tasks []taskSpec `json:"tasks"` // explicit tasks (mutually exclusive with goal)
	Goal  string     `json:"goal"`  // natural-language goal for the planner

	Planner        string `json:"planner"`        // required with goal (no preset expansion in serve)
	N              int    `json:"n"`              // planner target task count (default 4)
	MinTasks       int    `json:"minTasks"`       // plan floor (0 = none)
	PlannerTimeout string `json:"plannerTimeout"` // default 120s

	Agent        string `json:"agent"` // required
	AgentTimeout string `json:"agentTimeout"`
	AgentRetries int    `json:"agentRetries"`

	Strategy string `json:"strategy"` // default overlay
	Assert   bool   `json:"assert"`
	Semantic string `json:"semantic"` // off | go (default off)

	Resolver        string `json:"resolver"`
	ResolverTimeout string `json:"resolverTimeout"` // default 30s

	Verify        string `json:"verify"`
	VerifyImpact  string `json:"verifyImpact"` // requires verify
	VerifyRetries int    `json:"verifyRetries"`
	VerifyCache   bool   `json:"verifyCache"`
	VerifyBisect  bool   `json:"verifyBisect"` // requires verify

	Repair    string `json:"repair"`
	RepairMax int    `json:"repairMax"` // default 2

	NoAutocommit   bool   `json:"noAutocommit"`
	Lanes          string `json:"lanes"` // off | warn | strict
	KeepFailed     bool   `json:"keepFailed"`
	ParallelAgents int    `json:"parallelAgents"` // fan-out concurrency cap; <=0 = server default (see server.maxParallelAgents)
	Budget         string `json:"budget"`
	Notes          bool   `json:"notes"`

	Publish        string `json:"publish"`
	PublishTimeout string `json:"publishTimeout"` // default 120s

	EnvMode string `json:"envMode"` // overrides the server default when set
}

// planSpec carries the goal-planning inputs buildParams parses so the async run
// goroutine can plan (planTasks) without re-parsing/re-validating.
type planSpec struct {
	goal           string
	plannerCmd     string
	n              int
	minTasks       int
	plannerTimeout time.Duration
}

func (s *server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json", codeUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, serveMaxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req runRequest
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), codeBadRequest)
		return
	}

	rc := s.resolveCell(req.Cell)
	if rc == nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown cell %q; known cells: %s", req.Cell, strings.Join(s.cellKeys(), ", ")), codeCellNotFound)
		return
	}

	haveTasks := len(req.Tasks) > 0
	haveGoal := strings.TrimSpace(req.Goal) != ""
	switch {
	case haveTasks && haveGoal:
		writeErr(w, http.StatusBadRequest, "tasks and goal are mutually exclusive; pass exactly one", codeBadRequest)
		return
	case !haveTasks && !haveGoal:
		writeErr(w, http.StatusBadRequest, "one of tasks or goal is required", codeBadRequest)
		return
	}
	if strings.TrimSpace(req.Agent) == "" {
		writeErr(w, http.StatusBadRequest, "agent is required", codeBadRequest)
		return
	}
	if haveTasks {
		if err := validateReqTasks(req.Tasks); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error(), codeBadRequest)
			return
		}
	}

	p, plan, err := s.buildParams(req, rc.cell.Repo(), haveGoal)
	if err != nil {
		code := codeBadRequest
		if errors.Is(err, errEnvWidenRefused) {
			code = codeEnvWidenRefused
		}
		writeErr(w, http.StatusBadRequest, err.Error(), code)
		return
	}

	// Quota: -max-agents-per-run, checked before anything is acquired or
	// created (see server.maxAgentsPerRun's doc comment) — a rejection here
	// starts no run at all. For an explicit -tasks request the count is
	// exact; for a -goal (planner) request the true count isn't known until
	// planning runs asynchronously, so the only synchronously-available
	// number is N, the planner's already-validated/defaulted target count
	// (plan.n) — a BYO planner that ignores N is outside what serve can
	// check before starting.
	if s.maxAgentsPerRun > 0 {
		agentCount := len(req.Tasks)
		if haveGoal {
			agentCount = plan.n
		}
		if agentCount > s.maxAgentsPerRun {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("agent count %d exceeds this server's max-agents-per-run %d", agentCount, s.maxAgentsPerRun), codeQuotaAgents)
			return
		}
	}

	rec, err := s.acquireRun(rc, req, &p)
	switch {
	case errors.Is(err, errCellBusy):
		writeErr(w, http.StatusConflict, err.Error(), codeCellBusy)
		return
	case errors.Is(err, errQuotaConcurrency):
		writeErr(w, http.StatusTooManyRequests, err.Error(), codeQuotaConcurrency)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error(), codeInternalError)
		return
	}
	go s.execRun(rec, p, req.Tasks, plan, haveGoal)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"runId":  rec.id,
		"cell":   rc.cell.ID(),
		"status": "queued",
	})
}

// errCellBusy / errQuotaConcurrency are acquireRun's two REFUSALS, as sentinels
// rather than response codes: the HTTP path turns them into 409/429, and a watch
// cycle turns the first into a skipped tick (see watchCycle). Same condition,
// two callers, one place that decides it.
//
// Each one's text is the TRAILING CLAUSE of the message it is wrapped into, so
// the composed error reproduces byte-for-byte the wording POST /runs has always
// returned — a sentinel is an implementation detail and must not show up in a
// response body.
var (
	errCellBusy         = errors.New("one run per cell at a time")
	errQuotaConcurrency = errors.New("try again once a run finishes")
)

// acquireRun takes the cell's run slot and creates the run's durable directory
// and journal — everything that must be true BEFORE a run starts, and the one
// place it happens. POST /runs calls it and then starts execRun in a goroutine
// (so it can return 202 immediately); a watch cycle calls it and then runs
// execRun synchronously (so its loop stays serial). Both therefore hold the same
// per-cell lock, count against the same concurrency quota, and leave the same
// crash trail — a cycle run is not a second kind of run.
//
// The caller MUST call execRun on the returned record on some path: acquireRun
// has already taken the slot, the counter, and s.wg on its behalf, and execRun's
// defers are what release all three.
func (s *server) acquireRun(rc *registeredCell, req runRequest, p *runParams) (*runRecord, error) {
	cellID := rc.cell.ID()
	s.mu.Lock()
	if s.busy[cellID] {
		s.mu.Unlock()
		return nil, fmt.Errorf("a run is already in progress for cell %s (%s); %w", cellID, rc.cell.Repo(), errCellBusy)
	}
	// Quota: -max-concurrent-runs, a global ceiling across ALL cells on top of
	// the per-cell busy check — taken under the same lock, before the run dir is
	// created, so a refusal here starts no run and leaves no dir.
	if s.maxConcurrentRuns > 0 && s.activeRuns >= s.maxConcurrentRuns {
		s.mu.Unlock()
		return nil, fmt.Errorf("this server's max-concurrent-runs %d is already in flight; %w", s.maxConcurrentRuns, errQuotaConcurrency)
	}
	runID := newRunID()
	dir := filepath.Join(rc.runsDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	// Journal (issue #90): status.json records "queued" the instant the run
	// is accepted, and request.json the request behind it — both BEFORE the
	// run is even started, so a crash in the gap between accept and its first
	// instruction still leaves an accurate, diagnosable trail on disk (see
	// docs/USAGE.md's "Crash recovery" section). A watch cycle journals the
	// batch it assembled in the same shape; unlike a POSTed body that is a
	// record of what ran, not a request anyone can re-POST.
	writeRunStatus(dir, "queued", "")
	writeRunRequest(dir, req)
	s.busy[cellID] = true
	s.activeRuns++
	rec := &runRecord{id: runID, cellID: cellID, repo: rc.cell.Repo(), dir: dir, status: "queued", startedAt: time.Now()}
	s.runs[runID] = rec
	s.wg.Add(1)
	s.mu.Unlock()

	p.EventsPath = filepath.Join(dir, "events.ndjson")
	// Event push (issue #117) mirrors those same lines to -event-url, tagged with
	// the run and cell a receiver can't infer from the line itself. nil when no
	// -event-url is configured, which leaves driveRun's emitter untouched.
	p.EventSink = s.eventPush.sink(runID, cellID)
	// The run id is the key the spot-audit sample is drawn on (see
	// auditSelected): a recorded, replayable id, never a fresh random draw.
	p.RunID = runID
	return rec, nil
}

// execRun is the async body of a run: plan (if a goal), driveRun, then persist
// the report (or an error marker) to disk and update the in-memory record. Runs
// under s.baseCtx, so a shutdown cancel aborts it — the same ctx driveRun's
// -budget machinery already honors.
func (s *server) execRun(rec *runRecord, p runParams, tasks []taskSpec, plan planSpec, haveGoal bool) {
	defer s.wg.Done()
	// Releases the cell slot AND the global concurrency counter on every
	// return path — including a panic unwinding through here — so
	// -max-concurrent-runs never wedges on an error.
	defer func() {
		s.mu.Lock()
		s.busy[rec.cellID] = false
		s.activeRuns--
		s.mu.Unlock()
	}()

	// Transition: queued -> running (see handleCreateRun's "queued" write).
	s.mu.Lock()
	rec.status = "running"
	s.mu.Unlock()
	writeRunStatus(rec.dir, "running", "")

	if haveGoal {
		planned, err := planTasks(s.baseCtx, p.Repo, plan.goal, plan.plannerCmd, plan.n, plan.plannerTimeout, "", p.EnvMode, s.envPlanner)
		if err != nil {
			s.failRun(rec, "plan: "+err.Error())
			return
		}
		if plan.minTasks > 0 && len(planned) < plan.minTasks {
			s.failRun(rec, fmt.Sprintf("planner produced %d tasks, minTasks %d", len(planned), plan.minTasks))
			return
		}
		tasks = planned
	}

	rep, err := driveRun(s.baseCtx, p, tasks)
	if err != nil {
		// A mid-run failure can still leave real work on agent/<id> branches;
		// persist whatever driveRun assembled (same recovery posture as
		// runRun's partial-report emit) before recording the error.
		if len(rep.PerAgent) > 0 {
			writeRunReport(rec.dir, rep)
			// A driveRun error only ever originates before or exactly at
			// landing (see driveRun's err returns), so the ref never
			// advanced — Landed is unconditionally false here, unlike
			// computeUsage's report-field heuristic (accurate only for a
			// completed, non-erroring driveRun return, i.e. the path below).
			u := computeUsage(&rep, time.Since(rec.startedAt).Milliseconds(), reportFileSize(rec.dir))
			u.Landed = false
			writeRunUsage(rec.dir, u)
		}
		s.failRun(rec, err.Error())
		return
	}
	writeRunReport(rec.dir, rep)
	writeRunUsage(rec.dir, computeUsage(&rep, time.Since(rec.startedAt).Milliseconds(), reportFileSize(rec.dir)))
	// Parking (issue #109): a run that verified a landing but deliberately did
	// not advance the ref is NOT done — it is awaiting a human. finishRunDir owns
	// the record-before-marker ordering that makes that durable, shared with `sig
	// run` so a park is journaled identically whichever entry point produced it.
	status, err := finishRunDir(rec.dir, rep)
	if err != nil {
		s.failRun(rec, "write parking record: "+err.Error())
		return
	}
	s.mu.Lock()
	rec.status = status
	rec.finalSHA = rep.Integrate.FinalSHA
	s.mu.Unlock()
}

func (s *server) failRun(rec *runRecord, msg string) {
	writeRunError(rec.dir, msg, rec.startedAt, rec.cellID)
	writeRunStatus(rec.dir, "error", "")
	s.mu.Lock()
	rec.status = "error"
	rec.errMsg = msg
	s.mu.Unlock()
}

// runStatusResponse is GET /runs/{id}'s body. Report is the full runReport,
// present only once the run is done (read from report.json). Usage is the
// run's metering record (see usage.go), present whenever usage.json was
// written — i.e. whenever Report is, plus a run that errored with a partial
// report (see execRun) — nil (omitted) on a still-running run or one that
// wrote no report at all.
type runStatusResponse struct {
	ID        string     `json:"id"`
	Cell      string     `json:"cell"`
	Status    string     `json:"status"`
	StartedAt string     `json:"startedAt,omitempty"`
	Error     string     `json:"error,omitempty"`
	Report    *runReport `json:"report,omitempty"`
	Usage     *UsageJSON `json:"usage,omitempty"`
	// Park is the run's parking record (issue #109), present whenever park.json
	// exists and reads back — on an awaiting-ack run it is what an ack would
	// land; on a done/rejected one it is the record of what happened to it.
	Park *parkJSON `json:"park,omitempty"`
}

func (s *server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validRunID(id) {
		writeErr(w, http.StatusNotFound, "unknown run", codeRunNotFound)
		return
	}

	// Live record (this process) is authoritative for a running/error run and
	// tells us the dir; fall back to a disk scan for runs from a prior process.
	s.mu.Lock()
	rec := s.runs[id]
	var dir, cellID, status, errMsg string
	var startedAt time.Time
	if rec != nil {
		dir, cellID, status, errMsg, startedAt = rec.dir, rec.cellID, rec.status, rec.errMsg, rec.startedAt
	}
	s.mu.Unlock()

	if rec == nil {
		frc, fdir, ok := s.findRunDir(id)
		if !ok {
			writeErr(w, http.StatusNotFound, "unknown run", codeRunNotFound)
			return
		}
		dir, cellID = fdir, frc.cell.ID()
		// Not tracked in this process's memory: a prior process's run.
		// status.json (see runStatusFile) is the source of truth for it,
		// including "interrupted" for one that process left mid-flight.
		status, errMsg = diskRunStatus(fdir)
	}
	// Lazy ack-timeout (issue #109): reading a run is one of the moments an
	// expired park becomes rejected, so a caller never sees awaiting-ack for a
	// park already past its deadline. A run this process owns gets its in-memory
	// status refreshed from disk to match.
	if rc := s.byKey[cellID]; rc != nil && enforceParkTimeout(r.Context(), rc.cell.Git(), dir) {
		status, errMsg = diskRunStatus(dir)
		if rec != nil {
			s.mu.Lock()
			rec.status = status
			s.mu.Unlock()
		}
	}

	resp := runStatusResponse{ID: id, Cell: cellID, Status: status}
	if !startedAt.IsZero() {
		resp.StartedAt = startedAt.UTC().Format(time.RFC3339)
	}
	if pk, err := readPark(dir); err == nil {
		resp.Park = pk
	}
	// The report is the run's own record and exists from the moment driveRun
	// returns, which is also when a park is created — so a parked (or since
	// rejected) run serves it exactly like a done one.
	if status == "done" || status == statusAwaitingAck || status == statusRejected {
		if rep, err := readRunReport(dir); err == nil {
			resp.Report = rep
			if resp.StartedAt == "" {
				resp.StartedAt = rep.StartedAt
			}
		}
	}
	if status == "error" {
		if errMsg == "" {
			errMsg = readRunErrorMsg(dir)
		}
		resp.Error = errMsg
	}
	if status == "interrupted" {
		resp.Error = errMsg
	}
	if u, err := readRunUsage(dir); err == nil {
		resp.Usage = u
	}
	writeJSON(w, http.StatusOK, resp)
}

// runListEntry is one row of GET /runs.
type runListEntry struct {
	ID        string `json:"id"`
	Cell      string `json:"cell"`
	Status    string `json:"status"`
	StartedAt string `json:"startedAt,omitempty"`
	FinalSHA  string `json:"finalSHA,omitempty"`
}

func (s *server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	// Snapshot live statuses under the lock, then do all disk IO unlocked.
	s.mu.Lock()
	live := make(map[string]*runRecord, len(s.runs))
	for id, rec := range s.runs {
		live[id] = rec
	}
	s.mu.Unlock()

	// ponytail: list reads each done run's report.json for finalSHA; fine for a
	// single-process daemon over a handful of repos. Add a per-cell index file
	// if run history ever grows large enough for this scan to matter.
	var entries []runListEntry
	for _, rc := range s.cells {
		names, _ := os.ReadDir(rc.runsDir) // missing dir => no runs yet
		for _, de := range names {
			if !de.IsDir() {
				continue
			}
			id := de.Name()
			dir := filepath.Join(rc.runsDir, id)
			e := runListEntry{ID: id, Cell: rc.cell.ID()}
			if rec := live[id]; rec != nil {
				e.Status = rec.status
				e.StartedAt = rec.startedAt.UTC().Format(time.RFC3339)
				e.FinalSHA = rec.finalSHA
			} else {
				e.Status, _ = diskRunStatus(dir)
			}
			if e.Status == "done" && e.FinalSHA == "" {
				if rep, err := readRunReport(dir); err == nil {
					e.FinalSHA = rep.Integrate.FinalSHA
					e.StartedAt = rep.StartedAt
				}
			}
			entries = append(entries, e)
		}
	}
	// runIds are timestamp-prefixed, so a descending id sort is newest-first.
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID > entries[j].ID })
	if entries == nil {
		entries = []runListEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": entries})
}

func (s *server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validRunID(id) {
		writeErr(w, http.StatusNotFound, "unknown run", codeRunNotFound)
		return
	}
	s.mu.Lock()
	rec := s.runs[id]
	var dir string
	if rec != nil {
		dir = rec.dir
	}
	s.mu.Unlock()
	if rec == nil {
		_, fdir, ok := s.findRunDir(id)
		if !ok {
			writeErr(w, http.StatusNotFound, "unknown run", codeRunNotFound)
			return
		}
		dir = fdir
	}
	data, err := os.ReadFile(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		if os.IsNotExist(err) {
			// The run exists but hasn't emitted anything yet — an empty stream,
			// not an error.
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read events: "+err.Error(), codeInternalError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	w.Write(data) //nolint:errcheck // response body; nothing to recover on a write error
}

// handleRunUsage serves one run's metering record (see usage.go). 404s the
// same way an unknown run does when the id doesn't resolve to any run dir,
// and separately 404s (with a distinguishing message) when the dir exists
// but usage.json doesn't yet — a still-running run, or one that errored
// before any agent ran (see execRun's writeRunUsage gating).
func (s *server) handleRunUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validRunID(id) {
		writeErr(w, http.StatusNotFound, "unknown run", codeRunNotFound)
		return
	}
	s.mu.Lock()
	rec := s.runs[id]
	var dir string
	if rec != nil {
		dir = rec.dir
	}
	s.mu.Unlock()
	if rec == nil {
		_, fdir, ok := s.findRunDir(id)
		if !ok {
			writeErr(w, http.StatusNotFound, "unknown run", codeRunNotFound)
			return
		}
		dir = fdir
	}
	u, err := readRunUsage(dir)
	if err != nil {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("usage not available for run %s (not yet completed, or no report was written)", id), codeNotFound)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// handleUsageAggregate serves GET /usage: usage totals across every run in
// history that has a usage record, plus a per-cell rollup — the "metering"
// story (see docs/USAGE.md "Quotas and metering"). Same disk scan shape as
// handleListRuns, unlocked (usage.json is read-only, immutable once written).
func (s *server) handleUsageAggregate(w http.ResponseWriter, r *http.Request) {
	type cellUsage struct {
		Cell  string      `json:"cell"`
		Repo  string      `json:"repo"`
		Usage usageTotals `json:"usage"`
	}
	var total usageTotals
	cells := make([]cellUsage, 0, len(s.cells))
	for _, rc := range s.cells {
		var cu usageTotals
		names, _ := os.ReadDir(rc.runsDir) // missing dir => no runs yet
		for _, de := range names {
			if !de.IsDir() {
				continue
			}
			if u, err := readRunUsage(filepath.Join(rc.runsDir, de.Name())); err == nil {
				cu.addUsage(*u)
			}
		}
		cells = append(cells, cellUsage{Cell: rc.cell.ID(), Repo: rc.cell.Repo(), Usage: cu})
		total.addTotals(cu)
	}
	writeJSON(w, http.StatusOK, map[string]any{"totals": total, "cells": cells})
}

// ---- run-history surface (issue #110): the HTTP mirror of `sig log` ----

// logCellRuns is one cell's newest-first run list in GET /log's response. The
// Runs slice is exactly what `sig log -repo <that cell> -json` renders — same
// scanRuns call, so the two surfaces cannot drift.
type logCellRuns struct {
	Cell string   `json:"cell"`
	Repo string   `json:"repo"`
	Runs []logRow `json:"runs"`
}

// handleLog serves GET /log?limit=N: the newest-first run history for every
// registered cell, grouped by cell — the read-only mirror of `sig log`. Same
// lazy per-cell scan as the CLI (scanRuns reads at most N manifests per cell),
// same rows.
func (s *server) handleLog(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "limit must be a non-negative integer", codeBadRequest)
			return
		}
		limit = n
	}
	cells := make([]logCellRuns, 0, len(s.cells))
	for _, rc := range s.cells {
		rows, _ := scanRuns(rc.runsDir, limit)
		cells = append(cells, logCellRuns{Cell: rc.cell.ID(), Repo: rc.cell.Repo(), Runs: rows})
	}
	writeJSON(w, http.StatusOK, map[string]any{"cells": cells})
}

// handleLogSHA serves GET /log/sha/<sha>: provenance for one commit — which run
// landed it, from which task, by which agent — the mirror of `sig log -sha`.
// It asks each registered cell in turn (resolveProvenance is notes-first, then
// a manifest walk) and answers with the first that claims the commit; a commit
// no cell landed is a 404, the HTTP analogue of the CLI's exit 1.
func (s *server) handleLogSHA(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !validCommitArg(sha) {
		writeErr(w, http.StatusBadRequest, "invalid commit sha: expected 4..64 hex characters", codeBadRequest)
		return
	}
	for _, rc := range s.cells {
		if p, ok := resolveProvenance(r.Context(), rc.cell.Git(), rc.runsDir, sha); ok {
			writeJSON(w, http.StatusOK, map[string]any{"cell": rc.cell.ID(), "provenance": p})
			return
		}
	}
	writeErr(w, http.StatusNotFound, "not landed by sigbound", codeNotFound)
}

// ---- conflict-review surface (issue #62) ----

// locateRun resolves a run id to its cell and durable directory: the live
// record (this process) is authoritative and cheap; otherwise a disk scan finds
// a prior process's run. Same live-then-disk shape handleGetRun/handleRunUsage
// use, factored so the flagged endpoints share it.
func (s *server) locateRun(id string) (*registeredCell, string, bool) {
	s.mu.Lock()
	rec := s.runs[id]
	var cellID, dir string
	if rec != nil {
		cellID, dir = rec.cellID, rec.dir
	}
	s.mu.Unlock()
	if rec != nil {
		return s.byKey[cellID], dir, true
	}
	return s.findRunDir(id)
}

// flaggedListResponse is GET /runs/{id}/flagged: the run's flagged branches and
// their conflicted paths, straight from the persisted report — the allowlist the
// detail endpoint validates against.
type flaggedListResponse struct {
	RunID   string        `json:"runId"`
	Cell    string        `json:"cell"`
	Flagged []flaggedJSON `json:"flagged"`
}

// handleFlagged lists a run's flagged branches + conflicted paths. 404s an
// unknown run and a run with no persisted report yet (still running, or errored
// before integrate) — the flagged set only exists once a run completed integrate.
func (s *server) handleFlagged(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validRunID(id) {
		writeErr(w, http.StatusNotFound, "unknown run", codeRunNotFound)
		return
	}
	rc, dir, ok := s.locateRun(id)
	if !ok || rc == nil {
		writeErr(w, http.StatusNotFound, "unknown run", codeRunNotFound)
		return
	}
	rep, err := readRunReport(dir)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no report for this run yet (still running, or it never reached integrate)", codeNotFound)
		return
	}
	flagged := rep.Integrate.Flagged
	if flagged == nil {
		flagged = []flaggedJSON{}
	}
	writeJSON(w, http.StatusOK, flaggedListResponse{RunID: id, Cell: rc.cell.ID(), Flagged: flagged})
}

// flaggedDetailResponse is GET /runs/{id}/flagged/{branch}/{path...}: the three
// sides of one conflicted path. base is the run's base commit, ours the landed
// integrated tree, theirs the flagged branch's recorded tip — all read as blobs
// from the object store. A side is JSON null when the path is absent there (an
// add/delete conflict) or its recorded commit is no longer resolvable.
type flaggedDetailResponse struct {
	Path    string  `json:"path"`
	Base    *string `json:"base"`
	Ours    *string `json:"ours"`
	Theirs  *string `json:"theirs"`
	BaseSHA string  `json:"baseSHA,omitempty"`
}

// handleFlaggedDetail returns the three sides for ONE conflicted path. The path
// is not read from the filesystem: {branch}/{path...} must EXACTLY equal a
// branch+"/"+path pair recorded in this run's flagged set (an allowlist from the
// report), so a request for any path NOT flagged — a traversal, an absolute
// path, or a real repo file that wasn't flagged — 404s and reads nothing. The
// three blob versions are then resolved from the recorded commit OIDs (base =
// report.baseSHA, ours = report.integrate.finalSHA, theirs = the flagged
// branch's recorded per-agent SHA) in ONE `git cat-file --batch` (BlobsBatch);
// a spec that doesn't resolve is simply absent from the map => that side null.
func (s *server) handleFlaggedDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validRunID(id) {
		writeErr(w, http.StatusNotFound, "unknown run", codeRunNotFound)
		return
	}
	rc, dir, ok := s.locateRun(id)
	if !ok || rc == nil {
		writeErr(w, http.StatusNotFound, "unknown run", codeRunNotFound)
		return
	}
	rep, err := readRunReport(dir)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no report for this run yet (still running, or it never reached integrate)", codeNotFound)
		return
	}

	// Allowlist: rest must EXACTLY match a recorded flagged branch + "/" + path.
	// Exact equality is the whole safety property — no filesystem lookup, no path
	// cleaning, no prefix guessing decides what gets read; only a pair the run
	// itself flagged can ever match.
	rest := r.PathValue("rest")
	var branch, path string
	found := false
	for _, f := range rep.Integrate.Flagged {
		for _, p := range f.Paths {
			if rest == f.Branch+"/"+p {
				branch, path = f.Branch, p
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "no such flagged path for this run", codeNotFound)
		return
	}

	// theirs' commit: the flagged branch's tip the run recorded (per-agent SHA).
	branchSHA := ""
	for _, a := range rep.PerAgent {
		if a.Branch == branch {
			branchSHA = a.SHA
			break
		}
	}

	// One cat-file --batch resolves every side's blob. An empty recorded SHA is
	// never turned into a spec (":path" would resolve against the index, not what
	// we mean) — that side stays null.
	specs := make([]string, 0, 3)
	sideSpec := map[string]string{}
	addSide := func(name, sha string) {
		if sha == "" {
			return
		}
		spec := sha + ":" + path
		sideSpec[name] = spec
		specs = append(specs, spec)
	}
	addSide("base", rep.BaseSHA)
	addSide("ours", rep.Integrate.FinalSHA)
	addSide("theirs", branchSHA)

	contents, err := rc.cell.BlobsBatch(r.Context(), specs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read blobs: "+err.Error(), codeInternalError)
		return
	}
	side := func(name string) *string {
		spec, ok := sideSpec[name]
		if !ok {
			return nil
		}
		v, present := contents[spec]
		if !present {
			return nil
		}
		return &v
	}
	writeJSON(w, http.StatusOK, flaggedDetailResponse{
		Path:    path,
		Base:    side("base"),
		Ours:    side("ours"),
		Theirs:  side("theirs"),
		BaseSHA: rep.BaseSHA,
	})
}

// ---- run parking surface (issue #109) ----

// ackRequest is the optional POST /runs/{id}/ack|reject body. Only `reason` is
// accepted, and only by reject — an ack carries no data because it decides
// nothing about WHAT lands: that was fixed when the run parked. Unknown fields
// are rejected like every other body on this daemon.
type ackRequest struct {
	Reason string `json:"reason"`
}

func (s *server) handleAck(w http.ResponseWriter, r *http.Request) { s.handleAckReject(w, r, true) }

func (s *server) handleReject(w http.ResponseWriter, r *http.Request) {
	s.handleAckReject(w, r, false)
}

// handleAckReject is the HTTP front door onto ackRun/rejectRun — it validates
// and locks, then delegates every decision about what lands to that shared
// function, which `sig ack`/`sig reject` call identically.
//
// It holds the cell's run slot for the duration, the same slot POST /runs
// takes: an ack whose base moved runs a real integrate + verify against the cell
// and then moves the base ref, so it must serialize with runs and with other
// acks exactly as a run does. That is also what makes a second ack arriving
// while a re-verify is in flight a 409 rather than two concurrent landings. The
// global -max-concurrent-runs counter is deliberately NOT taken: refusing to
// release an already-verified landing because the daemon is busy would be a
// quota applied to the wrong thing.
func (s *server) handleAckReject(w http.ResponseWriter, r *http.Request, ack bool) {
	id := r.PathValue("id")
	if !validRunID(id) {
		writeErr(w, http.StatusNotFound, "unknown run", codeRunNotFound)
		return
	}
	rc, dir, ok := s.locateRun(id)
	if !ok || rc == nil {
		writeErr(w, http.StatusNotFound, "unknown run", codeRunNotFound)
		return
	}
	var body ackRequest
	r.Body = http.MaxBytesReader(w, r.Body, serveMaxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), codeBadRequest)
		return
	}
	if ack && strings.TrimSpace(body.Reason) != "" {
		writeErr(w, http.StatusBadRequest, "reason applies to reject, not ack", codeBadRequest)
		return
	}

	cellID := rc.cell.ID()
	s.mu.Lock()
	if s.busy[cellID] {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, fmt.Sprintf("cell %s (%s) is busy with a run or another ack; one at a time", cellID, rc.cell.Repo()), codeCellBusy)
		return
	}
	s.busy[cellID] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy[cellID] = false
		s.mu.Unlock()
	}()

	var out ackOutcome
	var err error
	if ack {
		// s.baseCtx, NOT the request's: an ack is a durable state transition, and
		// its re-verify can run as long as the repo's verify command does. On the
		// request context, a client timeout or a closed browser tab would abort a
		// real integrate+verify partway and record it as a RED attempt — a park
		// claiming "verify failed" when nobody actually asked verify. Only a daemon
		// shutdown cancels it, exactly as for a run.
		//
		// Environment policy for a re-verify is the OPERATOR's, exactly as it is
		// for a run — a request never chooses what env the daemon's commands see.
		out, err = ackRun(s.baseCtx, rc.cell, dir, "http", ackEnv{Mode: s.envMode, Verify: s.envVerify, Resolver: s.envResolver})
	} else {
		out, err = rejectRun(r.Context(), rc.cell.Git(), dir, "http", body.Reason)
	}
	if err != nil {
		switch {
		case errors.Is(err, errNotAwaitingAck):
			// Covers an already-resolved run too: errParkResolved wraps this.
			writeErr(w, http.StatusConflict, err.Error(), codeNotParked)
		case errors.Is(err, errParkBusy):
			writeErr(w, http.StatusConflict, err.Error(), codeCellBusy)
		default:
			writeErr(w, http.StatusInternalServerError, err.Error(), codeInternalError)
		}
		return
	}
	// Keep this process's in-memory view honest for a run it started itself;
	// the disk journal was already updated by ackRun/rejectRun.
	s.mu.Lock()
	if rec := s.runs[id]; rec != nil {
		rec.status = out.Status
		if out.LandedSHA != "" {
			rec.finalSHA = out.LandedSHA
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// handleUI serves the single self-contained conflict-review page (uiHTML). A
// strict CSP declares the page needs NOTHING external — same-origin fetches
// only, inline script/style (the page IS one inline file), no remote src of any
// kind — so it renders on an air-gapped daemon and can never be made to pull a
// third-party asset. The shell is served UNAUTHENTICATED even when a token is
// set (it is data-free — a browser navigation cannot carry a bearer token, so
// gating it would make the page unreachable); every /runs data endpoint the
// page fetches stays gated, and the page carries the pasted token on those
// fetches.
func (s *server) handleUI(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write(uiHTML) //nolint:errcheck // response body; nothing to recover on a write error
}

// ---- request -> runParams ----

// errEnvWidenRefused is buildParams' sentinel for the one env-mode rejection
// (see below): a request tried to widen the server's scoped env-mode to
// inherit. handleCreateRun checks it with errors.Is to pick the
// codeEnvWidenRefused response code instead of the generic codeBadRequest --
// deliberately a sentinel, not a string match on the error text, so the two
// stay independent (issue #93).
var errEnvWidenRefused = errors.New("envMode: a request cannot widen the server's scoped env-mode to inherit")

// buildParams validates a runRequest the way runRun validates its flags (reusing
// the same validate* helpers) and maps it onto a runParams for driveRun, plus a
// planSpec for the goal path. Environment policy always comes from the server;
// EnvMode alone may be narrowed by the request. Any validation failure is a 400
// (returned as an error the caller turns into one).
func (s *server) buildParams(req runRequest, repo string, haveGoal bool) (runParams, planSpec, error) {
	var zeroPlan planSpec

	base := strings.TrimSpace(req.Base)
	if base == "" {
		base = "main"
	}
	strategy := strings.TrimSpace(req.Strategy)
	if strategy == "" {
		strategy = cell.StrategyOverlay
	}
	if err := validateStrategy(strategy); err != nil {
		return runParams{}, zeroPlan, err
	}
	semantic := strings.TrimSpace(req.Semantic)
	if semantic == "" {
		semantic = semanticOff
	}
	if err := validateSemanticMode(semantic); err != nil {
		return runParams{}, zeroPlan, err
	}
	// Lane default mirrors runRun: warn, except a planned (goal) run defaults to
	// strict, since the planner already promised a disjoint split.
	lanes := strings.TrimSpace(req.Lanes)
	if lanes == "" {
		if haveGoal {
			lanes = laneStrict
		} else {
			lanes = laneWarn
		}
	}
	if err := validateLaneMode(lanes); err != nil {
		return runParams{}, zeroPlan, err
	}
	// Env policy: server default, request may narrow to an explicit mode but
	// never widen it. A scoped server must stay scoped no matter what a
	// request asks for -- inherit would hand every BYO command (agent/
	// resolver/verify/repair/planner/publish) the daemon's full environment,
	// secrets included (issue #60). Widening the other direction (server
	// inherit, request scoped) is always allowed: that's narrowing.
	envMode := s.envMode
	if m := strings.TrimSpace(req.EnvMode); m != "" {
		if s.envMode == envModeScoped && m == envModeInherit {
			return runParams{}, zeroPlan, errEnvWidenRefused
		}
		envMode = m
	}
	if err := validateEnvMode(envMode); err != nil {
		return runParams{}, zeroPlan, err
	}
	// verifyBisect/verifyImpact's "requires verify" preconditions are NOT checked
	// here: the cell's sigbound.policy may supply the verify battery, and that is
	// only readable from the pinned base SHA inside driveRun. Checking the REQUEST
	// field here would reject verifyBisect on a policy-bearing cell whose verify
	// comes solely from the policy. driveRun validates the EFFECTIVE verify
	// command for both this path and `sig run` from one site (see
	// validateVerifyPreconditions); a genuine no-verify-anywhere request is still
	// rejected loudly, as the run's recorded error rather than a 400.

	agentTimeout, err := parseDur(req.AgentTimeout, 0)
	if err != nil {
		return runParams{}, zeroPlan, fmt.Errorf("agentTimeout: %w", err)
	}
	resolverTimeout, err := parseDur(req.ResolverTimeout, 30*time.Second)
	if err != nil {
		return runParams{}, zeroPlan, fmt.Errorf("resolverTimeout: %w", err)
	}
	budget, err := parseDur(req.Budget, 0)
	if err != nil {
		return runParams{}, zeroPlan, fmt.Errorf("budget: %w", err)
	}
	// Quota: -max-run-time caps -budget at this server's ceiling. A
	// request can only make its own budget STRICTER, never laxer than the
	// server's: an unset/0 request budget (unlimited) becomes the ceiling,
	// and a request budget already under the ceiling is left alone.
	if s.maxRunTime > 0 {
		if budget <= 0 || s.maxRunTime < budget {
			budget = s.maxRunTime
		}
	}
	// Quota: -max-parallel-agents caps -parallel-agents at this server's
	// ceiling, exactly like -max-run-time caps -budget above — a request can
	// only narrow its own fan-out concurrency, never exceed the server's. An
	// unset/non-positive request value (today's GOMAXPROCS default) becomes
	// the ceiling; a request value already under the ceiling is left alone.
	parallelAgents := req.ParallelAgents
	if s.maxParallelAgents > 0 {
		if parallelAgents <= 0 || s.maxParallelAgents < parallelAgents {
			parallelAgents = s.maxParallelAgents
		}
	}
	publishTimeout, err := parseDur(req.PublishTimeout, 120*time.Second)
	if err != nil {
		return runParams{}, zeroPlan, fmt.Errorf("publishTimeout: %w", err)
	}

	repairMax := req.RepairMax
	if repairMax <= 0 {
		repairMax = 2
	}

	p := runParams{
		Repo:            repo,
		Base:            base,
		Strategy:        strategy,
		Assert:          req.Assert,
		Semantic:        semantic,
		AgentCmd:        req.Agent,
		PlannerCmd:      strings.TrimSpace(req.Planner),
		AgentTimeout:    agentTimeout,
		AgentRetries:    req.AgentRetries,
		ResolverCmd:     req.Resolver,
		ResolverTimeout: resolverTimeout,
		VerifyCmd:       req.Verify,
		VerifyImpactCmd: req.VerifyImpact,
		VerifyRetries:   req.VerifyRetries,
		VerifyCache:     req.VerifyCache,
		VerifyBisect:    req.VerifyBisect,
		RepairCmd:       req.Repair,
		RepairMax:       repairMax,
		Autocommit:      !req.NoAutocommit,
		LaneMode:        lanes,
		KeepFailed:      req.KeepFailed,
		ParallelAgents:  parallelAgents,
		Budget:          budget,
		Notes:           req.Notes,
		PublishCmd:      req.Publish,
		PublishTimeout:  publishTimeout,
		EnvMode:         envMode,
		EnvAgent:        s.envAgent,
		EnvResolver:     s.envResolver,
		EnvVerify:       s.envVerify,
		EnvRepair:       s.envRepair,
		EnvPublish:      s.envPublish,
		// A request field a caller actually set counts as an explicit choice, so
		// a deliberate weaker lanes/semantic/assert is an error against a
		// stricter policy (see resolvePolicy), not a silent tighten. A JSON bool
		// can't distinguish an explicit assert=false from an omitted one, so only
		// assert=true is treated as explicit; policy's assert=true floor still
		// tightens an omitted/false request silently, the safe direction.
		PolicyExplicit: policyExplicit{
			Lanes:    strings.TrimSpace(req.Lanes) != "",
			Semantic: strings.TrimSpace(req.Semantic) != "",
			Assert:   req.Assert,
		},
	}

	if !haveGoal {
		return p, zeroPlan, nil
	}
	// Goal path: planner required (serve does no preset expansion), plan
	// inputs parsed/validated here so the async goroutine only runs the plan.
	if p.PlannerCmd == "" {
		return runParams{}, zeroPlan, errors.New("planner is required with goal")
	}
	n := req.N
	if n <= 0 {
		n = 4
	}
	if req.MinTasks > n {
		return runParams{}, zeroPlan, fmt.Errorf("minTasks %d exceeds n %d", req.MinTasks, n)
	}
	plannerTimeout, err := parseDur(req.PlannerTimeout, 120*time.Second)
	if err != nil {
		return runParams{}, zeroPlan, fmt.Errorf("plannerTimeout: %w", err)
	}
	return p, planSpec{goal: strings.TrimSpace(req.Goal), plannerCmd: p.PlannerCmd, n: n, minTasks: req.MinTasks, plannerTimeout: plannerTimeout}, nil
}

// ---- lookups & helpers ----

func (s *server) resolveCell(key string) *registeredCell {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	if rc := s.byKey[key]; rc != nil {
		return rc
	}
	if abs, err := filepath.Abs(key); err == nil {
		return s.byKey[abs]
	}
	return nil
}

// cellKeys returns the cell ids, for a "known cells: ..." error message.
func (s *server) cellKeys() []string {
	ids := make([]string, len(s.cells))
	for i, rc := range s.cells {
		ids[i] = rc.cell.ID()
	}
	return ids
}

// findRunDir locates a run's directory across every cell's runs dir. Used for
// runs not in this process's memory (a prior process's history). Linear over the
// registered cells, which is a handful.
func (s *server) findRunDir(id string) (*registeredCell, string, bool) {
	for _, rc := range s.cells {
		dir := filepath.Join(rc.runsDir, id)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return rc, dir, true
		}
	}
	return nil, "", false
}

// validRunID guards a run id from the URL before it is joined onto a runs dir:
// it must be a single safe path component (slugSafe already rejects ""/"."/".."
// and anything outside [A-Za-z0-9._-]) and bounded in length, so a hostile id
// can never traverse out of the runs dir. A reject is reported as "unknown run".
func validRunID(id string) bool {
	return len(id) <= 128 && slugSafe(id)
}

// newRunID is a timestamp prefix (second precision, so a lexical sort is
// chronological) plus 6 random bytes, so two runs in the same second — only ever
// on DIFFERENT cells, since one cell serializes — never collide.
func newRunID() string {
	var b [6]byte
	_, _ = rand.Read(b[:]) // crypto/rand; a read error only weakens uniqueness, never correctness
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// validateReqTasks mirrors loadTasks' checks for an inline task array: every id
// non-empty and unique.
func validateReqTasks(tasks []taskSpec) error {
	seen := map[string]bool{}
	for i, t := range tasks {
		if strings.TrimSpace(t.ID) == "" {
			return fmt.Errorf("task %d has an empty id", i)
		}
		if seen[t.ID] {
			return fmt.Errorf("duplicate task id %q", t.ID)
		}
		seen[t.ID] = true
	}
	return nil
}

// parseDur parses a Go duration string, treating "" as def.
func parseDur(s string, def time.Duration) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}

// ---- durable run storage (.git/sigbound/runs/<id>/) ----

// startRunDir creates and journals one `sig run` invocation's durable run
// directory — the same layout acquireRun creates for a served run, because
// EVERYTHING that can release a park is keyed on that directory (issue #137):
// `sig ack`/`sig reject` resolve a run id to it, the ack-timeout sweep and `sig
// gc`'s unconditional branch protection scan it for park.json, and a park's
// keep-alive ref is named after the run id so gc can tell an open park's ref
// from a stranded one. A CLI run with no directory parks into a record only its
// own -manifest can see, and nothing supported can then land the verified tree.
//
// The startup crash-recovery sweep runs here too. It is `sig serve`'s at
// newServer, but a repo that never runs a daemon has no other moment to heal a
// dir a Ctrl-C left saying "running". Recovery only rewrites a run whose pid
// the liveness probe reports gone (see recoverStaleRuns) -- which on unix
// leaves a live daemon's runs alone, and on Windows leaves nothing alone,
// because pidAlive has no implementation there and calls every pid dead (see
// issue #94, and the recovery section in docs/USAGE.md for what that costs).
func startRunDir(ctx context.Context, g *gitx.Git) (dir, runID string, err error) {
	common, err := g.GitCommonDir(ctx)
	if err != nil {
		return "", "", err
	}
	runsDir := filepath.Join(common, "sigbound", "runs")
	recoverStaleRuns(runsDir, os.Getpid())
	runID = newRunID()
	dir = filepath.Join(runsDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("create run dir: %w", err)
	}
	writeRunStatus(dir, "running", "")
	return dir, runID, nil
}

// finishRunDir records a completed run's outcome in its run dir and reports the
// status it wrote. Both entry points end here — `sig serve`'s execRun and `sig
// run`'s runRun — because the ORDERING is the contract, not a convenience: an
// unresolved park.json OUTRANKS the phase marker (see diskRunStatus), so the
// record must be on disk before the marker flips. A crash in the reverse gap
// leaves a run marked done with a park nothing knows about; a crash in this one
// leaves it marked running, which recovery and every reader then heal to
// awaiting-ack. A park.json that cannot be written is consequently a HARD
// failure for the run — it is the only record of a landing that has not landed
// — unlike the best-effort report and status writes around it.
func finishRunDir(dir string, rep runReport) (string, error) {
	status := "done"
	if rep.Park != nil {
		if err := writePark(dir, rep.Park); err != nil {
			return "", err
		}
		status = statusAwaitingAck
	}
	writeRunStatus(dir, status, "")
	return status, nil
}

// runErrorFile is error.json's shape: the marker written when a run fails
// operationally, so a restart can report status=error with the reason instead of
// mistaking the dir for an interrupted run.
type runErrorFile struct {
	Error     string `json:"error"`
	Cell      string `json:"cell"`
	StartedAt string `json:"startedAt"`
}

func writeRunReport(dir string, rep runReport) {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sig serve: encode report for %s: %v\n", dir, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "sig serve: write report %s: %v\n", dir, err)
	}
}

func writeRunError(dir, msg string, startedAt time.Time, cellID string) {
	data, err := json.MarshalIndent(runErrorFile{Error: msg, Cell: cellID, StartedAt: startedAt.UTC().Format(time.RFC3339)}, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "error.json"), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "sig serve: write error marker %s: %v\n", dir, err)
	}
}

func readRunReport(dir string) (*runReport, error) {
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		return nil, err
	}
	var rep runReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

func readRunErrorMsg(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "error.json"))
	if err != nil {
		return ""
	}
	var e runErrorFile
	if err := json.Unmarshal(data, &e); err != nil {
		return ""
	}
	return e.Error
}

// diskStatus reports a run's status from its directory alone: report.json =>
// done, error.json => error, neither => "" (caller decides — interrupted).
func diskStatus(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "report.json")); err == nil {
		return "done"
	}
	if _, err := os.Stat(filepath.Join(dir, "error.json")); err == nil {
		return "error"
	}
	return ""
}

// ---- crash-safe run journal: status.json / request.json (issue #90) ----

// runStatusFile is status.json's shape: the tiny phase marker written
// atomically (write-then-rename, same pattern as verifyCacheStore) at every
// run transition — POST accept ("queued"), the goroutine's first instruction
// ("running"), and the run's end ("done"/"error"). It is what lets a restart
// tell "still running" apart from "was running when the process died":
// recoverStaleRuns rewrites a dead owner's queued/running entry to
// "interrupted" at startup, and diskRunStatus reads status.json as the source
// of truth for any run this process didn't itself start.
type runStatusFile struct {
	Status    string `json:"status"` // queued | running | done | error | interrupted
	UpdatedAt string `json:"updatedAt"`
	PID       int    `json:"pid"`
	Note      string `json:"note,omitempty"` // set on "interrupted": why (see recoverStaleRuns)
}

// writeRunStatus atomically records dir's current phase. Best-effort, like
// every other durable-storage writer in this file: a write failure here (a
// full disk, a read-only .git) must never turn an otherwise-successful run
// into a reported failure.
func writeRunStatus(dir, status, note string) {
	data, err := json.MarshalIndent(runStatusFile{
		Status:    status,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		PID:       os.Getpid(),
		Note:      note,
	}, "", "  ")
	if err != nil {
		return
	}
	// Write-then-rename through the shared helper: a concurrent GET must never
	// observe a torn status.json, and the run dir has genuinely concurrent
	// writers — an ack, a reject and the lazy timeout sweep can all be resolving
	// the same run from different processes — so the temp file this goes through
	// must be unique per writer. See atomicWriteFile.
	_ = atomicWriteFile(dir, "status.json", data, 0o644)
}

func readRunStatus(dir string) (*runStatusFile, error) {
	data, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		return nil, err
	}
	var sf runStatusFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, err
	}
	return &sf, nil
}

// diskRunStatus reports status (+ an explanatory note, for error/interrupted)
// for a run dir NOT tracked in this process's memory, i.e. a prior process's
// run. status.json is the source of truth when present. A dir written before
// this feature existed (no status.json) falls back to diskStatus's
// report.json/error.json check, and — with neither — is reported
// "interrupted": there's no more information on disk than "something ran
// here and never reached a terminal state".
func diskRunStatus(dir string) (status, note string) {
	status, note = markerRunStatus(dir)
	// An UNRESOLVED parking record outranks the phase marker (issue #109). The
	// two are written in sequence — park.json first, deliberately — so a crash in
	// that gap leaves a genuinely parked run marked "running", which startup
	// recovery would then rewrite to "interrupted": a verified landing that no
	// ack or reject could ever reach again, whose branches `sig gc` would pin
	// forever with nothing able to release them. park.json is the authority on
	// whether a park exists; status.json only records the phase.
	switch status {
	case statusAwaitingAck, statusRejected, "done":
		return status, note
	}
	if pk, err := readPark(dir); err == nil && pk.ResolvedAt == "" {
		return statusAwaitingAck, "an unresolved parking record outranks the recorded phase " + strconv.Quote(status)
	}
	return status, note
}

// markerRunStatus reports what status.json alone says, falling back to the
// report.json/error.json probe for a dir written before that file existed. See
// diskRunStatus, which layers the parking record's authority on top.
func markerRunStatus(dir string) (status, note string) {
	if sf, err := readRunStatus(dir); err == nil {
		return sf.Status, sf.Note
	}
	if st := diskStatus(dir); st != "" {
		return st, ""
	}
	return "interrupted", "no status recorded for this run"
}

// writeRunRequest journals the POSTed request body (request.json) at accept
// time, alongside status.json: the run's inputs, known before the goroutine
// even starts, so an interrupted run is diagnosable and its body can be
// re-POSTed by hand as the manual recovery path (see docs/USAGE.md's "Crash
// recovery" section). req is exactly what the caller sent — serve's
// environment policy lives on the SERVER (server.envMode etc.), never in a
// request, so there are no env values in here to sanitize out.
func writeRunRequest(dir string, req runRequest) {
	data, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "request.json"), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "sig serve: write request journal %s: %v\n", dir, err)
	}
}

// recoverStaleRuns is newServer's startup recovery pass over one cell's runs
// dir: a run whose status.json still says queued/running belongs to a dead
// owner if its recorded PID no longer resolves to a live process. A dead
// owner's entry gets rewritten to "interrupted" here, before the server
// accepts its first request -- that's what keeps GET from reporting
// "running" forever after a kill -9. A LIVE recorded PID is left alone
// unconditionally, whether it's our own PID (a run this same process is
// still doing -- restart recovery doesn't run mid-process, but the test
// suite plants this case directly) or a sibling daemon's (multiple `sig
// serve` processes can share a runs dir; recovery must never stomp another
// daemon's in-flight run just because it isn't ours). ourPID is accepted as
// a parameter (not read via os.Getpid() inside) purely so a test can name a
// specific "this process" pid without actually running as it; the recovery
// decision itself no longer depends on it.
//
// Accepted residual: pidAlive only checks that SOME process holds sf.PID
// right now, not that it's the same process that wrote the status. On a
// system that recycles PIDs fast enough, a genuinely dead owner's pid could
// already belong to an unrelated process by the time recovery runs, and that
// run would be false-positived as still alive and left "running"/"queued"
// forever (until manually inspected -- see docs/USAGE.md's "Crash recovery"
// section). This is a narrow window and consistent with the rest of this
// file's best-effort posture, not a correctness guarantee.
func recoverStaleRuns(runsDir string, ourPID int) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return // missing dir (no runs yet, or first startup) => nothing to recover
	}
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		dir := filepath.Join(runsDir, de.Name())
		sf, err := readRunStatus(dir)
		if err != nil || (sf.Status != "queued" && sf.Status != "running") {
			continue // no status.json, or already terminal -- nothing to do
		}
		if pidAlive(sf.PID) {
			continue // owning process still alive -- ours or a sibling daemon's, never touch it
		}
		// A dead owner that had already written an unresolved park.json got as far
		// as producing a VERIFIED landing; it just never flipped the marker. That
		// run is parked, not interrupted -- calling it interrupted would make the
		// landing permanently unreachable (see diskRunStatus). Heal the marker
		// instead.
		if pk, perr := readPark(dir); perr == nil && pk.ResolvedAt == "" {
			writeRunStatus(dir, statusAwaitingAck, fmt.Sprintf("recovered at startup: owning process (pid %d) is gone, but this run had already parked a verified landing", sf.PID))
			continue
		}
		writeRunStatus(dir, "interrupted", fmt.Sprintf("recovered at startup: owning process (pid %d) is gone", sf.PID))
	}
}

// pidAlive reports whether pid names a currently running process. On Unix,
// os.FindProcess always succeeds without checking anything; sending signal 0
// is the standard, side-effect-free way to actually probe existence.
//
// Windows note: Process.Signal rejects signal 0 ("not supported by windows"),
// so this returns false for every pid there, live or dead. recoverStaleRuns
// consequently over-reaches on Windows — with no liveness test it would mark
// even a live sibling run "interrupted" — and since issue #137 that is NOT
// confined to the serve daemon: startRunDir runs the same sweep, so the `sig
// run` CLI, the primary Windows path, has it too. Neither is validated on that
// platform. What the over-reach does and does not cost is stated in
// docs/USAGE.md's "Crash recovery" section; the short version is a misreported
// phase, never lost work, because a live run rewrites its own marker and an
// unresolved park.json outranks the marker regardless. Issue #94 settled the
// Windows posture (ship the binary, state the limits) and is closed; a correct
// probe (OpenProcess + GetExitCodeProcess) was left out of it and remains
// follow-up work.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// ---- HTTP write helpers ----

// Error codes: a stable, machine-readable discriminant alongside every error
// response's human text (see writeErr). ADDITIVE atop the pre-existing
// {error} shape -- SemVer-safe (issue #93). Documented as part of the API
// surface in docs/USAGE.md; treat this vocabulary as frozen-ish -- a
// programmatic caller (the GitHub Action, an SDK, retry logic) switches on
// these strings, so a rename is a breaking change even though the field
// itself is new.
const (
	codeUnauthorized         = "unauthorized"           // 401: missing/wrong bearer token
	codeNotFound             = "not_found"              // 404: generic, not run- or cell-specific
	codeRunNotFound          = "run_not_found"          // 404: run id doesn't resolve
	codeCellNotFound         = "cell_not_found"         // 400: POST /runs named an unregistered cell
	codeCellBusy             = "cell_busy"              // 409: that cell already has a run (or an ack's re-verify) in flight
	codeNotParked            = "not_parked"             // 409: ack/reject on a run that isn't awaiting-ack
	codeQuotaConcurrency     = "quota_concurrency"      // 429: -max-concurrent-runs exceeded
	codeQuotaAgents          = "quota_agents"           // 400: -max-agents-per-run exceeded
	codeEnvWidenRefused      = "env_widen_refused"      // 400: request tried to widen scoped envMode
	codeWatchDisabled        = "watch_disabled"         // 400: POST /queue on a server started without -watch
	codeBadRequest           = "bad_request"            // 400: everything else validation-shaped
	codeUnsupportedMediaType = "unsupported_media_type" // 415: Content-Type isn't application/json
	codeInternalError        = "internal_error"         // 500: an operational failure, not caller error
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v) //nolint:errcheck // response body; nothing to recover on a write error
}

func writeErr(w http.ResponseWriter, status int, msg, code string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// ---- CLI entry + lifecycle ----

// addrIsLoopback reports whether -addr binds only the loopback interface. An
// empty host (":7777") or 0.0.0.0/:: binds every interface and is NOT loopback;
// "localhost" and any IP that IsLoopback() is.
func addrIsLoopback(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("invalid -addr %q: %w", addr, err)
	}
	return hostIsLoopback(host), nil
}

// hostIsLoopback reports whether a bare host (no port) names only this machine.
// An empty host means "every interface" and is NOT loopback; a name other than
// localhost is not resolved — an unresolved name is treated as remote, the safe
// direction for both callers (the bind posture above and -event-url's plaintext
// rule in validateEventURL).
func hostIsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// serveStartupCheck enforces the network posture before binding: a non-loopback
// -addr needs -allow-remote (an explicit opt-in past the localhost default) AND
// a token (a public bind must never be unauthenticated).
func serveStartupCheck(addr string, allowRemote, tokenSet bool, tokenEnv string) error {
	loopback, err := addrIsLoopback(addr)
	if err != nil {
		return err
	}
	if loopback {
		return nil
	}
	if !allowRemote {
		return fmt.Errorf("refusing to bind non-loopback address %q without -allow-remote: sig serve is a single-user daemon with no TLS and no user model — put a TLS-terminating reverse proxy in front of it and pass -allow-remote to acknowledge that", addr)
	}
	if !tokenSet {
		return fmt.Errorf("refusing to bind non-loopback address %q without an auth token: set the env var named by -token-env (%s) to a shared secret", addr, tokenEnv)
	}
	return nil
}

func runServe(w io.Writer, argv []string) (int, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sig serve -repos PATH[,PATH...] [-addr HOST:PORT] [-token-env NAME] [-allow-remote] [-env-mode inherit|scoped] [-env-agent NAMES] [-event-url URL] ...")
		fs.PrintDefaults()
		fmt.Fprintln(fs.Output(), "\nA single-process, single-user daemon over driveRun. Binds loopback by default;")
		fmt.Fprintln(fs.Output(), "refuses a non-loopback -addr unless -allow-remote is set, and a non-loopback bind")
		fmt.Fprintln(fs.Output(), "always requires the -token-env token. See docs/USAGE.md's `sig serve` section.")
	}
	addr := fs.String("addr", "127.0.0.1:7777", "host:port to bind; loopback by default. A non-loopback value requires -allow-remote AND the -token-env token")
	reposCSV := fs.String("repos", "", "comma-separated paths to the git repositories to serve, one cell each (required)")
	tokenEnv := fs.String("token-env", "SIGBOUND_SERVE_TOKEN", "name of the env var holding the shared bearer token; when that var is set, every request must send Authorization: Bearer <token>. "+
		"Unset + loopback bind = auth off (dev). A non-loopback bind requires it")
	allowRemote := fs.Bool("allow-remote", false, "permit a non-loopback -addr. sig serve ships no TLS and no user model; you accept responsibility for putting a TLS-terminating, access-controlled proxy in front of it")
	// Environment policy is operator-set here (not per request): a caller must
	// not be able to widen what env the daemon's commands see. Default is
	// scoped — a daemon must not leak its environment by default (issue #56).
	envMode := fs.String("env-mode", envModeScoped, "environment given to every run's -agent/-resolver/-verify/-repair/-planner/-publish command: scoped (default for serve — a minimal base env plus SIGBOUND_* plus each -env-* allowlist) "+
		"or inherit (the full daemon environment, today's `sig run` default). A request may narrow this per run but never widen the allowlists. See docs/USAGE.md's Scoped environments section")
	envAgent := fs.String("env-agent", "", "-env-mode scoped: comma-separated extra parent-env variable NAMES (or NAME_* globs) passed through to every run's -agent, e.g. ANTHROPIC_API_KEY")
	envResolver := fs.String("env-resolver", "", "-env-mode scoped: same as -env-agent, for -resolver")
	envVerify := fs.String("env-verify", "", "-env-mode scoped: same as -env-agent, for -verify")
	envRepair := fs.String("env-repair", "", "-env-mode scoped: same as -env-agent, for -repair")
	envPlanner := fs.String("env-planner", "", "-env-mode scoped: same as -env-agent, for -planner")
	envPublish := fs.String("env-publish", "", "-env-mode scoped: same as -env-agent, for -publish")
	// Managed-layer quotas (see docs/USAGE.md "Quotas and metering"), all
	// opt-in: 0 = unlimited, byte-identical to today's (#60) behavior.
	maxAgentsPerRun := fs.Int("max-agents-per-run", 0, "quota: reject (400) a POST /runs whose agent count exceeds N, before any run starts. For a -goal request the target -n is checked (the true count isn't known until planning runs). 0 = unlimited (default)")
	maxRunTime := fs.Duration("max-run-time", 0, "quota: cap every run's -budget at this duration (e.g. 10m); a request's own shorter -budget always wins (min of the two). 0 = unlimited (default)")
	maxConcurrentRuns := fs.Int("max-concurrent-runs", 0, "quota: reject (429) a POST /runs once N runs are in flight across ALL cells, on top of the existing per-cell 409. 0 = unlimited (default)")
	maxParallelAgents := fs.Int("max-parallel-agents", 0, "quota: cap every run's -parallel-agents (fan-out concurrency) at this value; a request's own smaller value always wins (min of the two), and an over-ask is silently capped, not rejected. 0 = unlimited (default)")
	// Watch mode (issue #111): continuous cycles over each cell's arrivals. Off
	// by default — serve stays request/response unless asked otherwise. The
	// cell's sigbound.policy may set the cadence keys itself, in which case
	// passing the matching flag EXPLICITLY is an error (see resolveWatchConfig).
	watch := fs.Bool("watch", false, "watch each cell's agent/* and imported/*/* branches (plus POST /queue enqueues) and drive a continuous integration cycle over what arrives. "+
		"Each cycle is an ordinary policy-gated, verify-gated run through the same path POST /runs uses; a cycle only differs in its manifest's source=watch")
	watchBase := fs.String("watch-base", "main", "-watch: the branch each cycle integrates onto, and the base its cell's sigbound.policy is read from")
	watchInterval := fs.Duration("watch-interval", 30*time.Second, "-watch: how often a cell may start a cycle. A cycle that finds nothing pending starts no run at all")
	watchBatch := fs.Int("watch-batch", 0, "-watch: start a cycle EARLY once this many branches are pending, without waiting out -watch-interval. 0 = interval only (default)")
	watchVerify := fs.String("watch-verify", "", "-watch: the verify command every cycle must pass before it lands, composed with the cell policy's own verify battery. "+
		"Required unless the cell's sigbound.policy declares a verify line; pass `true` to accept landing every cycle unverified")
	watchMaxRed := fs.Int("watch-max-red", 3, "-watch: exclude a branch after this many consecutive cycles that failed to land it, raising a red-branch inbox entry. Re-pushing the branch clears the count")
	// Event push (issue #117): mirror the NDJSON event stream to an HTTP
	// receiver. Delivery is fail-open — see eventpush.go and docs/USAGE.md's
	// "Event push" section.
	eventURL := fs.String("event-url", "", "POST every run event to this URL as it is emitted, as {runId, cell, event} JSON. Delivery is fail-open: it never blocks or fails a run, and events a slow or broken receiver can't keep up with are DROPPED and counted (GET /health). "+
		"http:// is accepted only for a loopback receiver")
	eventSecretEnv := fs.String("event-secret-env", "", "-event-url: name of the env var holding the shared secret every POST is signed with — "+eventPushSigHeader+": sha256=<hex>, the HMAC-SHA256 of the raw request body. Unset = unsigned deliveries")

	if err := fs.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return exitOK, nil
		}
		return exitOperationalError, err
	}
	// Which flags were set EXPLICITLY, so a cadence the repo's own policy
	// declares can refuse to be overridden by one silently (see
	// resolveWatchConfig). Same fs.Visit set `sig run` builds for -config.
	explicitFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicitFlags[f.Name] = true })
	repos := splitCSV(*reposCSV)
	if len(repos) == 0 {
		return exitOperationalError, errors.New("-repos is required (comma-separated repo paths)")
	}
	if err := validateEnvMode(*envMode); err != nil {
		return exitOperationalError, err
	}
	for _, slot := range []struct{ flag, val string }{
		{"-env-agent", *envAgent}, {"-env-resolver", *envResolver}, {"-env-verify", *envVerify},
		{"-env-repair", *envRepair}, {"-env-planner", *envPlanner}, {"-env-publish", *envPublish},
	} {
		if err := validateEnvAllow(slot.flag, splitCSV(slot.val)); err != nil {
			return exitOperationalError, err
		}
	}
	token := os.Getenv(*tokenEnv)
	if err := serveStartupCheck(*addr, *allowRemote, token != "", *tokenEnv); err != nil {
		return exitOperationalError, err
	}
	// Event push: refuse an unusable configuration HERE, not at the first POST.
	// An empty named secret var is an error rather than a fallback to unsigned:
	// an operator who asked for signing and silently got none is exactly the
	// misconfiguration a receiver cannot detect.
	eventSecret := ""
	if *eventURL != "" {
		if err := validateEventURL(*eventURL); err != nil {
			return exitOperationalError, err
		}
	}
	if *eventSecretEnv != "" {
		if *eventURL == "" {
			return exitOperationalError, errors.New("-event-secret-env has nothing to sign without -event-url")
		}
		if eventSecret = os.Getenv(*eventSecretEnv); eventSecret == "" {
			return exitOperationalError, fmt.Errorf("-event-secret-env %s: that env var is empty; set it, or drop the flag to deliver unsigned", *eventSecretEnv)
		}
	}
	// Same cheap git preflight sig run/integrate do before touching a repo.
	if err := gitx.CheckMinVersion(context.Background(), "git"); err != nil {
		return exitOperationalError, err
	}

	// baseCtx cancels on SIGINT/SIGTERM; every in-flight run honors it, so a
	// shutdown aborts running commands rather than waiting them out.
	baseCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := newServer(baseCtx, serverConfig{
		repos:             repos,
		token:             token,
		envMode:           *envMode,
		envAgent:          splitCSV(*envAgent),
		envResolver:       splitCSV(*envResolver),
		envVerify:         splitCSV(*envVerify),
		envRepair:         splitCSV(*envRepair),
		envPlanner:        splitCSV(*envPlanner),
		envPublish:        splitCSV(*envPublish),
		maxAgentsPerRun:   *maxAgentsPerRun,
		maxRunTime:        *maxRunTime,
		maxConcurrentRuns: *maxConcurrentRuns,
		maxParallelAgents: *maxParallelAgents,
		eventURL:          *eventURL,
		eventSecret:       eventSecret,
	})
	if err != nil {
		return exitOperationalError, err
	}

	// Watch loops start BEFORE the listener binds: a cadence the cell's policy
	// rejects (or a repo with no watch base) must be a startup error, not a
	// daemon that is already serving when it discovers it cannot watch.
	if *watch {
		s.watchOn = true
		// The daemon-level stream goes to stdout AND, when configured, to the
		// push receiver. Its lines belong to no single run, so they are pushed
		// with no runId; each already names its own cell.
		s.watchEvents = &eventEmitter{enc: json.NewEncoder(teeEvents(w, s.eventPush.sink("", "")))}
		if err := s.startWatch(baseCtx, watchConfig{
			base:     *watchBase,
			interval: *watchInterval,
			batch:    *watchBatch,
			maxRed:   *watchMaxRed,
			verify:   *watchVerify,
		}, explicitFlags); err != nil {
			return exitOperationalError, err
		}
	}

	// Bind before announcing, so a port already in use is an immediate,
	// clear operational error rather than a surprise mid-serve.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return exitOperationalError, err
	}
	authDesc := "off (loopback dev mode)"
	if token != "" {
		authDesc = "required (bearer token)"
	}
	fmt.Fprintf(w, "sig serve %s listening on %s — %d cell(s), auth %s\n", Version, ln.Addr(), len(s.cells), authDesc)
	for _, rc := range s.cells {
		fmt.Fprintf(w, "  cell %s  %s\n", rc.cell.ID(), rc.cell.Repo())
	}
	if s.eventPush != nil {
		signed := "unsigned"
		if eventSecret != "" {
			signed = "signed with $" + *eventSecretEnv
		}
		fmt.Fprintf(w, "  event push -> %s (%s)\n", *eventURL, signed)
	}
	return s.serve(w, ln, stop)
}

// serve runs the HTTP server on ln until s.baseCtx is cancelled (a signal in
// production) or Serve fails, then gracefully drains: stop accepting, let
// in-flight runs observe the already-cancelled baseCtx and write their final
// reports, and exit. stop restores default signal handling so a second Ctrl-C
// hard-kills instead of blocking on a stuck drain (a test passes a no-op). Split
// out from runServe so a test can drive the real listener + shutdown path with a
// context it controls.
func (s *server) serve(w io.Writer, ln net.Listener, stop func()) (int, error) {
	srv := &http.Server{Handler: s.handler()}
	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		if err != nil {
			return exitOperationalError, err
		}
		return exitOK, nil
	case <-s.baseCtx.Done():
		stop()
		fmt.Fprintln(w, "sig serve: shutting down, draining in-flight runs...")
		shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
		s.wg.Wait()
		// The push tally, once no run can add to it. Printed even when nothing
		// was dropped: an operator who has to go looking for a loss count is one
		// who won't find it. The pusher's own goroutine ended with baseCtx.
		if st := s.eventPush.stats(); st != nil {
			fmt.Fprintf(w, "sig serve: event push: %d sent, %d dropped, %d failed\n", st.Sent, st.Dropped, st.Failed)
		}
		fmt.Fprintln(w, "sig serve: stopped")
		return exitOK, nil
	}
}
