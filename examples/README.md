# Try Sigbound on your own repo in 5 minutes

Sigbound runs several AI agents on one git repo in parallel and merges their
work automatically — only landing what builds and passes tests.

## 1. Build

```bash
go build -o sig ./cmd/sig
```

## 2. Point it at any git repo with a build/test command

`examples/tasks.json` describes three independent features that touch disjoint
files (so they integrate in parallel). Each task's `files` list is enforced —
an agent that strays outside its lane is rejected.

```bash
./sig run \
  -repo /path/to/your/repo \
  -tasks examples/tasks.json \
  -agent 'sh -c "cd \"$PWD\"; claude -p --permission-mode acceptEdits \"$SIGBOUND_TASK\""' \
  -strategy overlay \
  -resolver 'sh -c "git merge-file -p --union \"$SIGBOUND_OURS\" \"$SIGBOUND_BASE\" \"$SIGBOUND_THEIRS\""' \
  -verify 'go build ./... && go test ./...' \
  -repair 'sh -c "cd \"$PWD\"; claude -p --permission-mode acceptEdits \"Fix this build failure, then stop: $SIGBOUND_FAILURE\""' \
  -repair-max 2 \
  -json
```

Every model slot — `-agent`, `-resolver`, `-repair`, and `-planner` (below) —
is a shell command **you** supply. Bring your own model and harness; the
example uses the `claude` CLI, but any command that edits files in the working
directory works.

## 3. Or start from a one-sentence goal

Give it a goal instead of a task list and a planner model splits it into
disjoint tasks for you:

```bash
./sig run \
  -repo /path/to/your/repo \
  -goal "Add CSV export, search, and a summary command" \
  -planner 'sh -c "claude -p \"$SIGBOUND_PROMPT\""' \
  -n 3 \
  -agent '...' -resolver '...' -verify 'go build ./... && go test ./...'
```

## What comes back

A JSON report: which agents committed, how the branches were grouped and
integrated, which conflicts were resolved or flagged, whether the merged tree
built and tested, and the final commit SHA. Nothing that fails `-verify` is
ever landed.

## Environment variables passed to your commands

| Var | Given to | Meaning |
|-----|----------|---------|
| `SIGBOUND_TASK`, `SIGBOUND_TASK_ID` | `-agent` | the task prompt / id; cwd is the task's worktree |
| `SIGBOUND_GOAL`, `SIGBOUND_REPOMAP`, `SIGBOUND_PROMPT`, `SIGBOUND_N` | `-planner` | the goal, a repo map, a ready prompt, the task count |
| `SIGBOUND_BASE`, `SIGBOUND_OURS`, `SIGBOUND_THEIRS`, `SIGBOUND_PATH` | `-resolver` | the three versions of a conflicted file |
| `SIGBOUND_FAILURE` | `-repair` | the build/test output to fix |
