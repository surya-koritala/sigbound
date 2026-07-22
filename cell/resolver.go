package cell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Conflict is one path git's 3-way merge could not auto-resolve, carrying the
// three sides' file CONTENTS (not paths). Ours is the already-landed/base side;
// Theirs is the branch being merged in. A Resolver turns these into a single
// resolved file body.
type Conflict struct {
	Path   string
	Base   string
	Ours   string
	Theirs string
}

// Resolver resolves a single conflicted file down to one body. It is the
// bring-your-own-resolution seam the integrator consults when merge-tree reports
// a real conflict.
//
// ok=false means "I decline / cannot resolve this" — the integrator then FLAGS
// the branch for a human, exactly as if no resolver were configured, and main is
// never touched. err reports the resolver's own operational failure; the
// integrator treats a non-nil err the same as ok=false (flag), so a broken or
// slow resolver can never corrupt main. A resolution is applied only when EVERY
// conflicted path comes back ok with no error.
type Resolver interface {
	Resolve(ctx context.Context, c Conflict) (resolved string, ok bool, err error)
}

// CommandResolver is the bring-your-own-model seam: it runs an external command
// once per conflicted path. Base/Ours/Theirs are written to three temp files
// whose paths are exported as SIGBOUND_BASE / SIGBOUND_OURS / SIGBOUND_THEIRS (and the
// repo-relative path as SIGBOUND_PATH); the command writes the resolved file body
// to STDOUT. A non-zero exit, a timeout, or empty stdout all mean "unresolved"
// => ("", false, nil), so a command that abstains (or a model that isn't sure)
// can never land a partial or garbage resolution. The command can be anything —
// a merge tool, a script, or a call to a language model.
type CommandResolver struct {
	// Args is the argv to run: Args[0] is the program, Args[1:] its arguments.
	Args []string
	// Timeout bounds each per-conflict invocation. Zero means no explicit
	// timeout (the caller's context still applies).
	Timeout time.Duration
	// Env, when non-nil, is the base environment each invocation gets instead
	// of the full os.Environ() — the seam a caller (e.g. the sig CLI's
	// -env-mode scoped) uses to hand this resolver a deliberately narrowed
	// environment. The per-conflict SIGBOUND_* vars below are always appended
	// on top, so they're never affected either way. nil (the default) keeps
	// today's behavior: os.Environ() untouched.
	Env []string
}

// Resolve runs the configured command for one conflict. See CommandResolver.
func (r *CommandResolver) Resolve(ctx context.Context, c Conflict) (string, bool, error) {
	if len(r.Args) == 0 {
		return "", false, errors.New("CommandResolver: empty Args")
	}

	dir, err := os.MkdirTemp("", "sig-resolve-*")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(dir)

	write := func(name, body string) (string, error) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			return "", err
		}
		return p, nil
	}
	basePath, err := write("base", c.Base)
	if err != nil {
		return "", false, err
	}
	oursPath, err := write("ours", c.Ours)
	if err != nil {
		return "", false, err
	}
	theirsPath, err := write("theirs", c.Theirs)
	if err != nil {
		return "", false, err
	}

	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, r.Args[0], r.Args[1:]...)
	// When the timeout fires, CommandContext SIGKILLs the direct child, but a
	// grandchild that inherited our stdout pipe (e.g. `sh -c "sleep 30"` where the
	// shell forks) keeps that pipe open, so Run's internal Wait blocks until the
	// grandchild exits — defeating the timeout. WaitDelay bounds that post-kill
	// wait and force-closes the pipes, so a hung resolver returns promptly on
	// every platform. The happy path is unaffected: a resolver that exits closes
	// its pipes and Wait returns immediately.
	cmd.WaitDelay = 2 * time.Second
	env := r.Env
	if env == nil {
		env = os.Environ()
	} else {
		// Resolve runs concurrently across conflict groups (the integrator
		// folds groups in parallel), and r.Env is one slice shared by every
		// call. Clamping its capacity forces the append below to always
		// allocate a fresh backing array instead of possibly writing into
		// r.Env's own — without this, two goroutines appending at the same
		// len(env) would race on (and corrupt) the same memory.
		env = env[:len(env):len(env)]
	}
	cmd.Env = append(env,
		"SIGBOUND_BASE="+basePath,
		"SIGBOUND_OURS="+oursPath,
		"SIGBOUND_THEIRS="+theirsPath,
		"SIGBOUND_PATH="+c.Path,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	// stderr is intentionally discarded: the command signals success only via
	// exit 0 + non-empty stdout, so its diagnostics never leak into a blob.

	if err := cmd.Run(); err != nil {
		// Non-zero exit, timeout (context cancelled), or spawn failure. Fail
		// safe: decline so the integrator flags the branch, main untouched.
		return "", false, nil
	}
	if out.Len() == 0 {
		return "", false, nil // empty stdout => nothing to land
	}
	return out.String(), true, nil
}
