#!/usr/bin/env bash
# action/run.sh — assemble and execute `sig run` from this action's inputs,
# then publish its outputs (exit-code, final-sha, report, landed-count) to
# GITHUB_OUTPUT. Never swallows sig run's exit code: any non-zero result
# fails this step (see docs/USAGE.md#exit-codes for what each code means) —
# but the outputs are always written first, so a caller can still inspect
# what happened even on a failing run (e.g. a verify failure).
#
# Required env: REPORT_PATH, GITHUB_OUTPUT (set by the Actions runtime).
# Optional env: INPUT_REPO_PATH (default "."), INPUT_BASE, INPUT_TASKS,
# INPUT_GOAL, INPUT_PLANNER, INPUT_N, INPUT_AGENT, INPUT_RESOLVER,
# INPUT_VERIFY, INPUT_REPAIR, INPUT_STRATEGY, INPUT_EXTRA_ARGS.
set -euo pipefail

# build_sig_run_args populates the ARGS array from INPUT_* env vars, passing
# only the flags whose input is actually set. Split out from main() so it can
# be sourced and tested without running sig at all (see action/run_test.sh).
build_sig_run_args() {
  local repo_path="${INPUT_REPO_PATH:-.}"
  ARGS=(run -repo "$repo_path" -json)
  [ -n "${INPUT_BASE:-}" ] && ARGS+=(-base "$INPUT_BASE")
  [ -n "${INPUT_TASKS:-}" ] && ARGS+=(-tasks "$INPUT_TASKS")
  [ -n "${INPUT_GOAL:-}" ] && ARGS+=(-goal "$INPUT_GOAL")
  [ -n "${INPUT_PLANNER:-}" ] && ARGS+=(-planner "$INPUT_PLANNER")
  [ -n "${INPUT_N:-}" ] && ARGS+=(-n "$INPUT_N")
  [ -n "${INPUT_AGENT:-}" ] && ARGS+=(-agent "$INPUT_AGENT")
  [ -n "${INPUT_RESOLVER:-}" ] && ARGS+=(-resolver "$INPUT_RESOLVER")
  [ -n "${INPUT_VERIFY:-}" ] && ARGS+=(-verify "$INPUT_VERIFY")
  [ -n "${INPUT_REPAIR:-}" ] && ARGS+=(-repair "$INPUT_REPAIR")
  [ -n "${INPUT_STRATEGY:-}" ] && ARGS+=(-strategy "$INPUT_STRATEGY")
  # Escape hatch: appended verbatim, last, so it can override anything above.
  # Tokenized (not eval'd) so a quoted value like -verify "go test ./..."
  # survives as one argument, the same way a shell would parse it — but
  # nothing in the string is ever executed. xargs's own quote-aware word
  # splitting groups quoted words; each token is emitted NUL-delimited by
  # printf (run directly by xargs, no shell in between) and read back into
  # an array, so shell metacharacters like $(...), `...`, and ; land as
  # inert literal argv entries instead of being interpreted.
  # The guard strips whitespace before testing: a whitespace-only value must
  # skip tokenizing entirely, because GNU xargs (Linux runners) runs printf
  # once even on empty input, emitting one phantom empty token — BSD xargs
  # (macOS) does not, which is how this survived local testing.
  local extra_stripped="${INPUT_EXTRA_ARGS:-}"
  extra_stripped="${extra_stripped//[[:space:]]/}"
  if [ -n "$extra_stripped" ]; then
    local extra=()
    while IFS= read -r -d '' tok; do
      extra+=("$tok")
    done < <(xargs -n1 printf '%s\0' <<<"$INPUT_EXTRA_ARGS")
    # Guard the append: under `set -u`, "${extra[@]}" on a still-empty array
    # (e.g. INPUT_EXTRA_ARGS was whitespace-only) is an unbound-variable
    # error on bash <4.4 — notably macOS's stock /bin/bash (3.2). (A bare
    # `[ ... ] && ARGS+=(...)` would also trip `set -e` when the test is
    # false, since that's the exit status of the whole statement.)
    if [ "${#extra[@]}" -gt 0 ]; then
      ARGS+=("${extra[@]}")
    fi
  fi
}

# exit_code_message maps a sig run exit code to a one-line explanation (see
# docs/USAGE.md#exit-codes).
exit_code_message() {
  case "$1" in
    0) echo "landed and verified (or -verify was not set); -publish succeeded or was not set" ;;
    1) echo "operational error (bad flags, a git/integrate failure, etc.)" ;;
    2) echo "usage error (bad top-level sig invocation)" ;;
    3) echo "-verify failed; nothing landed" ;;
    4) echo "one or more branches flagged as conflicts; the rest landed" ;;
    5) echo "no agent succeeded" ;;
    6) echo "landed and verified, but -publish failed" ;;
    *) echo "unrecognized exit code" ;;
  esac
}

main() {
  local report_path="${REPORT_PATH:?REPORT_PATH is required}"
  mkdir -p "$(dirname "$report_path")"

  local ARGS=()
  build_sig_run_args
  echo "sigbound-action: sig ${ARGS[*]}" >&2

  set +e
  sig "${ARGS[@]}" | tee "$report_path"
  local exit_code="${PIPESTATUS[0]}"
  set -e

  local final_sha="" landed_count=""
  if jq empty "$report_path" >/dev/null 2>&1; then
    final_sha="$(jq -r '.integrate.finalSHA // empty' "$report_path")"
    landed_count="$(jq -r '(.integrate.landed // []) | length' "$report_path")"
  fi

  {
    echo "exit-code=$exit_code"
    echo "final-sha=$final_sha"
    echo "report=$report_path"
    echo "landed-count=$landed_count"
  } >>"$GITHUB_OUTPUT"

  if [ "$exit_code" -ne 0 ]; then
    echo "::error::sig run exited $exit_code: $(exit_code_message "$exit_code") (see docs/USAGE.md#exit-codes)" >&2
    exit "$exit_code"
  fi
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
