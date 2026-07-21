---
name: Bug report
about: Report incorrect behavior, a crash, or a correctness issue
title: "[bug] "
labels: bug
assignees: ''
---

## What happened

A clear description of the bug.

## Expected behavior

What you expected to happen instead.

## Reproduction

Steps, command line, or a minimal plan/scenario that triggers it.

```
# sig command(s) and relevant output
```

## Environment

- Sigbound version / commit:
- `go version`:
- OS / arch:
- git version:

## Merge-path issues

If this touches the merge / integration path (lane enforcement, `merge-tree`,
diff/tree parsing, the repair loop, or `-verify` gating), **please attach a
failing `go test` case or a `sigbench` scenario** that reproduces it. Parser
panics and tree corruption are treated as security issues — see `SECURITY.md`.
