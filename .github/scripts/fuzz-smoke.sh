#!/usr/bin/env bash
# .github/scripts/fuzz-smoke.sh — run a Go fuzz smoke target with one retry
# for the known worker-shutdown deadline flake: `go test -fuzz` can report
# "context deadline exceeded" at the -fuzztime cutoff on slow CI runners with
# zero crashers found. A real crasher writes a corpus file under
# <pkg>/testdata/fuzz/<target>/; if one appears after a failure, fail
# immediately without retrying — persistent breakage must never be masked as
# a flake. Absent a crasher artifact, retry once; a second failure fails the
# job regardless.
#
# Usage: fuzz-smoke.sh <Target> <pkg> <fuzztime>
#   e.g. fuzz-smoke.sh FuzzParsePlan ./cmd/sig/ 15s
set -euo pipefail

# run_fuzz_attempt runs one fuzz attempt. Split out so tests can stub it
# (see fuzz-smoke_test.sh) without invoking the real `go test`.
run_fuzz_attempt() {
  local target="$1" pkg="$2" fuzztime="$3"
  go test -run=NONE -fuzz="$target" -fuzztime="$fuzztime" "$pkg"
}

# corpus_dir prints the testdata/fuzz directory a crasher for this target
# would land in.
corpus_dir() {
  local pkg="$1" target="$2"
  printf '%s/testdata/fuzz/%s' "${pkg%/}" "$target"
}

# new_crasher_files prints any new or modified file under the target's
# corpus dir. `git status --porcelain` covers both untracked and modified
# paths; --untracked-files=all expands a brand-new directory into its
# individual files instead of collapsing it to one entry.
new_crasher_files() {
  local dir="$1"
  git status --porcelain --untracked-files=all -- "$dir" 2>/dev/null | awk '{print $2}'
}

main() {
  local target="${1:?target required}" pkg="${2:?pkg required}" fuzztime="${3:?fuzztime required}"
  local dir
  dir="$(corpus_dir "$pkg" "$target")"

  local attempt
  for attempt in 1 2; do
    if run_fuzz_attempt "$target" "$pkg" "$fuzztime"; then
      return 0
    fi

    local crashers
    crashers="$(new_crasher_files "$dir")"
    if [ -n "$crashers" ]; then
      echo "fuzz-smoke: real crasher found for $target, failing immediately (no retry):" >&2
      local f
      while IFS= read -r f; do
        [ -n "$f" ] || continue
        echo "--- $f ---" >&2
        cat "$f" >&2
      done <<<"$crashers"
      return 1
    fi

    if [ "$attempt" -eq 1 ]; then
      echo "fuzz-smoke: $target failed with no crasher artifact; retrying once for the known shutdown-deadline flake" >&2
    else
      echo "fuzz-smoke: $target failed again with no crasher artifact; not a flake, failing the job" >&2
    fi
  done

  return 1
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
