#!/usr/bin/env bash
# Smoke test for action/run.sh's pure functions: build_sig_run_args and
# exit_code_message. No network, no sig binary, no GITHUB_OUTPUT required.
set -euo pipefail
cd "$(dirname "$0")"
source ./run.sh

assert_eq() {
  if [ "$1" != "$2" ]; then
    echo "mismatch:" >&2
    echo "  got:  $1" >&2
    echo "  want: $2" >&2
    exit 1
  fi
}

# serialize joins args with a control character unlikely to appear in any of
# them, so the comparison can't be fooled by spaces inside individual args.
serialize() { printf '%s\x1f' "$@"; }

unset INPUT_BASE INPUT_TASKS INPUT_GOAL INPUT_PLANNER INPUT_N INPUT_RESOLVER \
  INPUT_VERIFY INPUT_REPAIR INPUT_STRATEGY INPUT_EXTRA_ARGS 2>/dev/null || true

# Minimal: repo-path default + required agent only.
INPUT_REPO_PATH="."
INPUT_AGENT='claude -p "$SIGBOUND_TASK"'
build_sig_run_args
assert_eq "$(serialize "${ARGS[@]}")" \
  "$(serialize run -repo . -json -agent 'claude -p "$SIGBOUND_TASK"')"

# Every named flag plus extra-args, in the documented order; extra-args
# appended last and its quoted token preserved as one argument.
INPUT_BASE="main"
INPUT_TASKS="tasks.json"
INPUT_VERIFY="go build ./... && go test ./..."
INPUT_STRATEGY="overlay"
INPUT_EXTRA_ARGS='-agent-timeout 300s -lanes strict'
build_sig_run_args
assert_eq "$(serialize "${ARGS[@]}")" \
  "$(serialize run -repo . -json -base main -tasks tasks.json \
    -agent 'claude -p "$SIGBOUND_TASK"' -verify 'go build ./... && go test ./...' \
    -strategy overlay -agent-timeout 300s -lanes strict)"

# goal/planner/n/resolver/repair round out the rest of the mapping.
unset INPUT_TASKS INPUT_VERIFY INPUT_STRATEGY INPUT_EXTRA_ARGS
INPUT_GOAL="Add CSV export"
INPUT_PLANNER='claude -p "$SIGBOUND_PROMPT"'
INPUT_N="3"
INPUT_RESOLVER='git merge-file -p --union "$SIGBOUND_OURS" "$SIGBOUND_BASE" "$SIGBOUND_THEIRS"'
INPUT_REPAIR='claude -p "Fix this: $SIGBOUND_FAILURE"'
build_sig_run_args
assert_eq "$(serialize "${ARGS[@]}")" \
  "$(serialize run -repo . -json -base main -goal "Add CSV export" \
    -planner 'claude -p "$SIGBOUND_PROMPT"' -n 3 -agent 'claude -p "$SIGBOUND_TASK"' \
    -resolver 'git merge-file -p --union "$SIGBOUND_OURS" "$SIGBOUND_BASE" "$SIGBOUND_THEIRS"' \
    -repair 'claude -p "Fix this: $SIGBOUND_FAILURE"')"

# --- INPUT_EXTRA_ARGS tokenizing: quote-aware, never executes -------------
#
# build_sig_run_args tokenizes INPUT_EXTRA_ARGS with xargs (quote-aware word
# splitting, no shell in between) rather than eval. These cases pin that
# down: quoted words still group into one argument, and shell metacharacters
# ($(...), backticks, ;) land as inert literal argv text instead of running.

unset INPUT_TASKS INPUT_GOAL INPUT_PLANNER INPUT_N INPUT_RESOLVER INPUT_VERIFY \
  INPUT_REPAIR INPUT_STRATEGY

# Quoted args with spaces group into a single argument, same as a shell
# would parse them.
INPUT_BASE="main"
INPUT_EXTRA_ARGS='-message "hello world" -count 2'
build_sig_run_args
assert_eq "$(serialize "${ARGS[@]}")" \
  "$(serialize run -repo . -json -base main -agent 'claude -p "$SIGBOUND_TASK"' \
    -message "hello world" -count 2)"

# Injection payloads must come through as inert literal arguments: no
# execution, and the payload text itself survives unexpanded. Each check
# clears its own marker file, runs build_sig_run_args, then asserts the
# marker was never created and the literal payload shows up in ARGS.
injection_dir="$(mktemp -d)"
trap 'rm -rf "$injection_dir"' EXIT

assert_inert() {
  local extra_args="$1" marker="$2" want_token="$3"
  rm -f "$marker"
  INPUT_EXTRA_ARGS="$extra_args"
  build_sig_run_args
  if [ -f "$marker" ]; then
    echo "injection executed: INPUT_EXTRA_ARGS=$extra_args created $marker" >&2
    exit 1
  fi
  local found=0 a
  for a in "${ARGS[@]}"; do
    [ "$a" = "$want_token" ] && found=1
  done
  if [ "$found" -ne 1 ]; then
    echo "payload not preserved literally: $want_token" >&2
    echo "ARGS: $(serialize "${ARGS[@]}")" >&2
    exit 1
  fi
}

# $(...) command substitution: stays literal text, never runs.
marker="$injection_dir/cmdsub"
assert_inert "-message \"\$(touch $marker)\"" "$marker" "\$(touch $marker)"

# Backticks: same deal, never runs.
marker="$injection_dir/backtick"
assert_inert "-message \"\`touch $marker\`\"" "$marker" "\`touch $marker\`"

# Bare semicolon: not a command separator here, just a literal argument.
marker="$injection_dir/semi"
assert_inert "-flag ; touch $marker" "$marker" ";"

# Empty and whitespace-only input: no extra args appended, no error.
unset INPUT_BASE
INPUT_EXTRA_ARGS=""
build_sig_run_args
assert_eq "$(serialize "${ARGS[@]}")" \
  "$(serialize run -repo . -json -agent 'claude -p "$SIGBOUND_TASK"')"

INPUT_EXTRA_ARGS="   "
build_sig_run_args
assert_eq "$(serialize "${ARGS[@]}")" \
  "$(serialize run -repo . -json -agent 'claude -p "$SIGBOUND_TASK"')"

echo "run.sh: extra-args tokenizing ok (no injection, quoting preserved)"

# exit_code_message covers every documented code plus an unknown fallback.
for code in 0 1 2 3 4 5 6 99; do
  msg="$(exit_code_message "$code")"
  [ -n "$msg" ]
done

echo "run.sh: ok"
