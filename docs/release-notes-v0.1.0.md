# Sigbound v0.1.0

Run multiple AI coding agents on one repository in parallel, and merge their
work automatically — landing only changes that build and pass your tests.

This is the first public release. It runs on top of plain git, works with any
host, and uses whatever model and harness you already have.

## Highlights

- **Parallel merge** — non-conflicting changes from many agents are combined in
  one pass, not one merge at a time.
- **AI conflict resolution, fail-safe** — a model resolves overlaps; anything it
  is unsure about is flagged for review rather than guessed.
- **Verified merges** — nothing lands unless the combined result passes your
  `-verify` build and test command.
- **Self-repair** — a merge that breaks the build is routed back to an agent to
  fix, then re-checked.
- **File lanes** — each task declares the files it may touch; an agent that
  strays is rejected.
- **Bring your own model** — planner, agent, resolver, and repair are each a
  command you supply.
- **Plan from a goal** — give a goal and a planner command and Sigbound fans it
  out into parallel tasks.

## Benchmark

Merging agents' branches into one repository on a single laptop, correctness
verified on every run: 512 agents integrate in ~1.8 s versus ~26 s for a
sequential `git merge` — about 15× faster, and the gap widens as agents are
added. Reproduce with `go run ./cmd/sigbench -sweep`.

## Install

```bash
git clone https://github.com/surya-koritala/sigbound && cd sigbound && go build -o sig ./cmd/sig
```

Requires Go 1.25+ and the `git` binary. See the [README](../README.md) for a
full `sig run` example.

## Early release — not yet

This is an early release focused on the merge engine and CLI, verified on real
repositories. It does **not** yet include multi-machine execution, a UI, or a
hosted service. Sigbound builds on top of git and does not aim to become a git
host. Feedback and issues are welcome.
