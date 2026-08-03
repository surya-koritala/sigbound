// Package gitobject provides bounded, process-reusing reads from one Git
// object database.
//
// Git remains the implementation of loose objects, packfiles, deltas and hash
// formats. Reader speaks Git's documented `cat-file --batch-command` protocol
// so callers can inspect sizes before reading content without spawning one Git
// process per object. A Reader is bound to one repository and serializes calls
// over its one request/response stream; independent repositories use
// independent Readers and never share state.
package gitobject

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// MaxBatch bounds request validation, metadata retention and the number of
	// protocol round trips a caller may place behind one acquired Reader.
	MaxBatch = 1024
	// MaxSpecBytes is intentionally generous enough for existing OSS rev:path
	// callers while preventing a hostile input from becoming an unbounded
	// protocol line or error message.
	MaxSpecBytes = 4096
	stderrMax    = 4 << 10
	closeDelay   = 2 * time.Second
)

// Type is a Git object type returned by cat-file.
type Type string

const (
	Blob   Type = "blob"
	Tree   Type = "tree"
	Commit Type = "commit"
	Tag    Type = "tag"
)

// Status describes a request that did not return content. These are ordinary,
// position-preserving results, not protocol failures: a batch may contain a
// missing or wrong-type object without making its other results ambiguous.
type Status string

const (
	Available Status = "available"
	Missing   Status = "missing"
	WrongType Status = "wrong_type"
	TooLarge  Status = "too_large"
)

// Request is constructed by Exact or Spec so a raw protocol line cannot be
// forged through a struct literal.
type Request struct {
	spec     string
	expected Type
	content  bool
	maxBytes int64
	exact    bool
}

// Exact requests metadata or content for one full, non-zero SHA-1/SHA-256
// object ID. Cloud and every other trust-boundary caller should use this form.
func Exact(oid string, expected Type, content bool, maxBytes int64) (Request, error) {
	if !validOID(oid) {
		return Request{}, fmt.Errorf("gitobject: %q is not a full non-zero SHA-1/SHA-256 object id", oid)
	}
	return request(strings.ToLower(oid), expected, content, maxBytes, true)
}

// Spec requests a Git object expression such as "<oid>:<path>" or
// "<ref>^{commit}". It exists for the OSS execution kernel's already-trusted
// repository-internal operations. Network/API boundaries should resolve to an
// immutable ID and use Exact instead. The line is strictly bounded and may not
// contain controls, so it cannot desynchronize the batch protocol.
func Spec(spec string, expected Type, content bool, maxBytes int64) (Request, error) {
	return request(spec, expected, content, maxBytes, false)
}

func request(spec string, expected Type, content bool, maxBytes int64, exact bool) (Request, error) {
	if spec == "" || len(spec) > MaxSpecBytes {
		return Request{}, fmt.Errorf("gitobject: object spec length must be 1..%d bytes", MaxSpecBytes)
	}
	for _, r := range spec {
		if r == 0 || r == '\n' || r == '\r' || r < 0x20 || r == 0x7f {
			return Request{}, errors.New("gitobject: object spec contains a protocol control character")
		}
	}
	if expected != "" && !validType(expected) {
		return Request{}, fmt.Errorf("gitobject: unsupported expected type %q", expected)
	}
	if content && maxBytes < 0 {
		return Request{}, errors.New("gitobject: content byte limit cannot be negative")
	}
	return Request{spec: spec, expected: expected, content: content, maxBytes: maxBytes, exact: exact}, nil
}

// Result is positionally paired with its Request. Content is populated only
// for Available content requests. An empty object has a non-nil zero-length
// Content slice, while metadata-only requests leave Content nil.
type Result struct {
	OID     string
	Type    Type
	Size    int64
	Status  Status
	Content []byte
	Bound   int64
}

// AggregateLimitError reports a batch whose individually admissible content
// would exceed the caller's aggregate budget. No content commands are sent in
// this case; size-before-read is therefore true for the whole batch.
type AggregateLimitError struct {
	Size  int64
	Bound int64
}

func (e *AggregateLimitError) Error() string {
	return fmt.Sprintf("gitobject: aggregate content size %d exceeds bound %d", e.Size, e.Bound)
}

var (
	ErrClosed   = errors.New("gitobject: reader closed")
	ErrPoisoned = errors.New("gitobject: reader protocol is no longer usable")
)

// Option customizes process construction. The zero-option configuration uses
// "git" from PATH and the package's closed environment.
type Option func(*options)

type options struct{ binary string }

// WithBinary selects a Git executable. It is primarily useful for tests and
// installations that deliberately pin Git outside PATH.
func WithBinary(binary string) Option {
	return func(o *options) { o.binary = binary }
}

// Reader owns one cat-file process and one strictly sequential protocol stream.
// Calls are safe from concurrent goroutines; one caller waiting for the stream
// may abandon the wait through its context without affecting the active call.
type Reader struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *capped
	cancel context.CancelFunc

	gate chan struct{}

	stateMu sync.Mutex
	closed  bool
	poison  error

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

// Open starts one `git cat-file --batch-command` process bound to repoDir.
// repoDir may be a worktree or a bare repository. The caller must Close it.
func Open(repoDir string, opts ...Option) (*Reader, error) {
	if repoDir == "" {
		return nil, errors.New("gitobject: repository directory is required")
	}
	cfg := options{binary: "git"}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.binary == "" {
		return nil, errors.New("gitobject: git binary is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, cfg.binary, "-C", repoDir, "cat-file", "--batch-command")
	cmd.Env = hermeticEnv()
	cmd.WaitDelay = closeDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gitobject: open stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		cancel()
		return nil, fmt.Errorf("gitobject: open stdout: %w", err)
	}
	stderr := &capped{limit: stderrMax}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		cancel()
		return nil, fmt.Errorf("gitobject: start git: %w", err)
	}
	r := &Reader{
		cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 32<<10), stderr: stderr, cancel: cancel,
		gate: make(chan struct{}, 1), closeDone: make(chan struct{}),
	}
	r.gate <- struct{}{}
	return r, nil
}

// Read inspects every request first and reads only individually admissible
// content after the aggregate bound is proven. aggregateMax must be positive
// when any request asks for content; metadata-only batches may pass zero.
func (r *Reader) Read(ctx context.Context, requests []Request, aggregateMax int64) ([]Result, error) {
	if ctx == nil {
		return nil, errors.New("gitobject: nil context")
	}
	if len(requests) == 0 {
		return []Result{}, nil
	}
	if len(requests) > MaxBatch {
		return nil, fmt.Errorf("gitobject: batch has %d objects, max is %d", len(requests), MaxBatch)
	}
	wantsContent := false
	for _, req := range requests {
		if req.spec == "" { // a zero-value Request bypassed the constructors
			return nil, errors.New("gitobject: request was not constructed by Exact or Spec")
		}
		wantsContent = wantsContent || req.content
	}
	if wantsContent && aggregateMax <= 0 {
		return nil, errors.New("gitobject: a positive aggregate content bound is required")
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.gate:
	}
	defer func() { r.gate <- struct{}{} }()
	if err := r.stateError(); err != nil {
		return nil, err
	}

	// Cancellation after this point interrupts a request/response round trip.
	// Its stream position is unknowable, so killing and poisoning the Reader is
	// the only safe outcome. Cancellation while waiting for gate above does not
	// affect somebody else's active operation.
	stop := context.AfterFunc(ctx, func() { r.poisonWith(ctx.Err()) })
	defer stop()

	results := make([]Result, len(requests))
	var aggregate int64
	for i, req := range requests {
		h, err := r.commandHeader("info", req.spec)
		if err != nil {
			return nil, r.protocolError(err)
		}
		res := Result{OID: h.oid, Type: h.typ, Size: h.size, Status: Available}
		switch {
		case h.missing:
			res.Status = Missing
		case req.exact && !strings.EqualFold(h.oid, req.spec):
			return nil, r.protocolError(fmt.Errorf("response object %q does not match exact request %q", h.oid, req.spec))
		case req.expected != "" && h.typ != req.expected:
			res.Status = WrongType
		case req.content && h.size > req.maxBytes:
			res.Status, res.Bound = TooLarge, req.maxBytes
		case req.content:
			if h.size > aggregateMax-aggregate {
				return nil, &AggregateLimitError{Size: aggregate + h.size, Bound: aggregateMax}
			}
			aggregate += h.size
		}
		results[i] = res
	}

	for i, req := range requests {
		if !req.content || results[i].Status != Available {
			continue
		}
		h, err := r.commandHeader("contents", req.spec)
		if err != nil {
			return nil, r.protocolError(err)
		}
		if h.missing || h.oid != results[i].OID || h.typ != results[i].Type || h.size != results[i].Size {
			return nil, r.protocolError(fmt.Errorf("content header changed after info for request %q", req.spec))
		}
		if h.size > int64(maxInt()-1) {
			return nil, r.protocolError(fmt.Errorf("object %q is too large for this process address space", req.spec))
		}
		content := make([]byte, h.size+1)
		if _, err := io.ReadFull(r.stdout, content); err != nil {
			return nil, r.protocolError(fmt.Errorf("read %d content bytes: %w", h.size, err))
		}
		if content[h.size] != '\n' {
			return nil, r.protocolError(errors.New("content record has no trailing newline"))
		}
		results[i].Content = content[:h.size]
	}
	if err := ctx.Err(); err != nil {
		return nil, r.protocolError(err)
	}
	return results, nil
}

type header struct {
	oid         string
	typ         Type
	size        int64
	missing     bool
	missingSpec string
}

func (r *Reader) commandHeader(command, spec string) (header, error) {
	if _, err := io.WriteString(r.stdin, command+" "+spec+"\n"); err != nil {
		return header{}, fmt.Errorf("write %s command: %w", command, err)
	}
	line, err := r.stdout.ReadString('\n')
	if err != nil {
		return header{}, fmt.Errorf("read %s header: %w", command, err)
	}
	h, err := parseHeader(line)
	if err == nil && h.missing && h.missingSpec != spec {
		return header{}, fmt.Errorf("missing response %q does not match request %q", h.missingSpec, spec)
	}
	return h, err
}

func parseHeader(line string) (header, error) {
	if len(line) > MaxSpecBytes+256 {
		return header{}, errors.New("cat-file header exceeds bound")
	}
	if !strings.HasSuffix(line, "\n") || strings.ContainsRune(strings.TrimSuffix(line, "\n"), 0) {
		return header{}, fmt.Errorf("malformed cat-file header %q", bounded(line))
	}
	line = strings.TrimSuffix(line, "\n")
	fields := strings.Fields(line)
	if len(fields) >= 2 && fields[len(fields)-1] == "missing" {
		spec := strings.TrimSuffix(line, " missing")
		if spec == "" || len(spec) > MaxSpecBytes {
			return header{}, fmt.Errorf("malformed missing header %q", bounded(line))
		}
		return header{missing: true, missingSpec: spec}, nil
	}
	if len(fields) != 3 || !validOID(fields[0]) {
		return header{}, fmt.Errorf("malformed cat-file header %q", bounded(line))
	}
	typ := Type(fields[1])
	if !validType(typ) {
		return header{}, fmt.Errorf("unsupported object type in header %q", bounded(line))
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return header{}, fmt.Errorf("invalid object size in header %q", bounded(line))
	}
	return header{oid: strings.ToLower(fields[0]), typ: typ, size: size}, nil
}

func validOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return false
	}
	for _, c := range b {
		if c != 0 {
			return true
		}
	}
	return false
}

func validType(t Type) bool { return t == Blob || t == Tree || t == Commit || t == Tag }

func maxInt() int { return int(^uint(0) >> 1) }

func (r *Reader) stateError() error {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.poison != nil {
		return fmt.Errorf("%w: %v", ErrPoisoned, r.poison)
	}
	return nil
}

func (r *Reader) protocolError(err error) error {
	r.poisonWith(err)
	if r.stderr != nil {
		if msg := strings.TrimSpace(r.stderr.String()); msg != "" {
			return fmt.Errorf("%w: %v: %s", ErrPoisoned, err, msg)
		}
	}
	return fmt.Errorf("%w: %v", ErrPoisoned, err)
}

func (r *Reader) poisonWith(err error) {
	if err == nil {
		err = ErrPoisoned
	}
	r.stateMu.Lock()
	if r.poison == nil && !r.closed {
		r.poison = err
		r.cancel()
	}
	r.stateMu.Unlock()
}

// Cancel makes the Reader terminal and unblocks an in-flight operation. It is
// idempotent. Consumers that implement their own fallback may call it from a
// request cancellation hook.
func (r *Reader) Cancel() { r.poisonWith(context.Canceled) }

// Close kills and reaps the helper, is safe to repeat, and cannot wait on a
// stuck protocol operation forever. A close intentionally returns nil for the
// process termination it initiated; a previously observed protocol error is
// returned by Read, where it has the relevant context.
func (r *Reader) Close() error {
	r.closeOnce.Do(func() {
		r.stateMu.Lock()
		r.closed = true
		r.cancel()
		r.stateMu.Unlock()
		_ = r.stdin.Close()
		r.closeErr = r.cmd.Wait()
		var exitErr *exec.ExitError
		if errors.As(r.closeErr, &exitErr) || errors.Is(r.closeErr, context.Canceled) {
			r.closeErr = nil
		}
		close(r.closeDone)
	})
	<-r.closeDone
	return r.closeErr
}

func hermeticEnv() []string {
	// Git's environment variables can redirect repository discovery, the object
	// database, namespaces, replacement objects, configuration and executables.
	// Remove the whole GIT_* namespace rather than trying to maintain a fragile
	// denylist, then add only the controls this read-only protocol needs. Filter
	// LC_ALL too so the appended stable protocol locale cannot be shadowed by a
	// duplicate entry (not every getenv implementation resolves duplicates the
	// same way).
	env := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "GIT_") || upper == "LC_ALL" {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
}

type capped struct {
	mu    sync.Mutex
	b     strings.Builder
	limit int
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	if room := c.limit - c.b.Len(); room > 0 {
		if len(p) > room {
			_, _ = c.b.Write(p[:room])
		} else {
			_, _ = c.b.Write(p)
		}
	}
	c.mu.Unlock()
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

func bounded(s string) string {
	if len(s) > 512 {
		return s[:512]
	}
	return s
}
