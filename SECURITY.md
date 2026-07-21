# Security Policy

## Supported Versions

Sigbound is pre-1.0. Security fixes land on `main` and in the latest tagged
release. Please test against `main` before reporting.

## Reporting a Vulnerability

**Please do not open a public issue for security problems.**

Report privately using GitHub's [private vulnerability reporting][gh-report]
(Security → Report a vulnerability) on this repository. Include:

- a description of the issue and its impact,
- a minimal reproduction — ideally a failing `go test` case, a fuzz input, or a
  `sigbench` scenario,
- affected version / commit, `go version`, OS, and git version.

We aim to acknowledge reports within a few days and to keep you updated as we
investigate and fix.

[gh-report]: https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability

## Threat model & what we treat as a vulnerability

Sigbound sits on top of git and is trusted to keep integration **correct**: to
keep lanes disjoint, to gate merges behind `-verify`, and to never corrupt a
repository. Because it parses untrusted git output and model output, we hold the
parsing and merge paths to a security-grade bar. In particular, the following are
treated as security issues, not ordinary bugs:

- **Parser panics or crashes** on malformed git output or model/plan output.
  Every parser has a fuzz target and seed corpus (`internal/gitx/fuzz_test.go`,
  `cmd/sig/fuzz_test.go`); a crashing input is a finding.
- **Tree or working-copy corruption** — any path where Sigbound can leave a repo,
  index, worktree, or branch in a damaged or inconsistent state.
- **Lane / disjointness violations** — a merge that lets one lane silently write
  outside its declared paths, or that clobbers another lane's changes.
- **`-verify` bypass** — landing unverified code, or any path that weakens the
  guarantee that only verified changes are merged.
- Path-escape / traversal in plan handling (e.g. writing outside the intended
  worktree).

### Fuzz-hardening stance

Parsers are expected to be total on arbitrary input: return an error, never
panic. We fuzz them in CI and welcome new fuzz targets, seed corpora, and
crashers. If you find an input that panics a parser, that is a valid security
report — please send the reproducing bytes.
