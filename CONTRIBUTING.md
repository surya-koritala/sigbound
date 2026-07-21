# Contributing to Sigbound

Thanks for your interest. Sigbound is an early, focused engine — the surface
area is small on purpose. All changes, including from maintainers, go through a
pull request.

## Workflow

Every change lands via a pull request. Direct pushes to `main` are rejected —
branch protection requires a PR that passes CI and review before it can merge.

1. Fork (or branch, if you have push access) and create a topic branch.
2. Make your change with tests.
3. Run the [local checks](#local-checks) until they are green.
4. Update `CHANGELOG.md` under `[Unreleased]`.
5. Open a PR and fill in the [checklist](#pull-request-checklist).

### Branch naming

Prefix the branch with its type:

- `feat/` — a new feature (e.g. `feat/lane-glob-patterns`)
- `fix/` — a bug fix (e.g. `fix/lstree-parse-panic`)
- `chore/` — tooling, CI, deps, or housekeeping (e.g. `chore/bump-go`)
- `docs/` — documentation only (e.g. `docs/usage-resolver-env`)

### Commits

- One logical change per commit; keep the history readable.
- Imperative subject under ~72 chars (`Fix ls-tree parse on empty tree`), a blank line, then a body explaining *why* when it isn't obvious.
- Reference the issue where relevant (`Fixes #12`).

### Pull request checklist

A PR is ready for review when:

- [ ] `go build ./...` is green.
- [ ] `gofmt -l .` reports nothing (code is formatted).
- [ ] `go vet ./...` is clean.
- [ ] `go test -race ./...` passes, including the fuzz seed corpora.
- [ ] Correctness is preserved — no change weakens the `-verify` gate or the "flag conflicts, never guess" invariant.
- [ ] New behavior comes with tests; parsers of git or model output come with a fuzz target and seed corpus.
- [ ] `CHANGELOG.md` is updated under `[Unreleased]`.

### How CI gates the PR

CI runs the same checks on every push to a PR: build, `gofmt`, `go vet`, and
`go test -race ./...`. A PR cannot merge until CI is green and the review is
approved — branch protection enforces both. Keep changes small so the gate stays
fast and the review stays focused.

## Local checks

Run these before opening or updating a PR:

```bash
gofmt -l .                  # must print nothing
go vet ./...
go build ./...
go test -race ./...         # full suite, including fuzz seed corpora
```

Benchmark (the A/B integration benchmark; changes to the integration path should
keep it green and correct):

```bash
go run ./cmd/sigbench -sweep -runs 5 -warmup 1
```

Fuzzing (run a target for a while when you touch a parser):

```bash
go test -run=xxx -fuzz=FuzzLsTree -fuzztime=60s ./internal/gitx/
go test -run=xxx -fuzz=FuzzParsePlan -fuzztime=60s ./cmd/sig/
```

## Layout

The engine lives in `cell/`; the git plumbing (shell-outs only, no go-git) in
`internal/gitx/`; the CLI, planner, lane enforcement, and repair loop in
`cmd/sig/`; the benchmark harness in `bench/` and `cmd/sigbench/`.

## Ground rules

- **Build on top of git; never rebuild git.** Worktrees, branches, and
  `merge-tree` are the substrate. No git server, no hosting.
- **Parsers must never panic on malformed input.** Anything that parses git
  output or model output gets a fuzz target and a seed corpus. See
  `internal/gitx/fuzz_test.go` and `cmd/sig/fuzz_test.go`.
- **Never land unverified code.** The whole point is that `-verify` gates the
  merge. Changes that weaken that guarantee won't be merged.
- Keep the existing tests green. New behavior comes with tests.

## Reporting

Open an issue with a reproduction. For anything touching the merge/integration
path, a failing test or a `sigbench` scenario is the most useful thing you can
attach.

See [`docs/USAGE.md`](docs/USAGE.md) for the full command reference and
[`docs/RELEASE.md`](docs/RELEASE.md) for the release process.
