#!/usr/bin/env bash
# Smoke test for fuzz-smoke.sh's retry/crasher-detection logic. No network,
# no real `go test -fuzz` run: run_fuzz_attempt is stubbed per case, and
# new_crasher_files is exercised against a throwaway git repo so git status
# behaves for real without touching this repo's working tree.
set -euo pipefail
cd "$(dirname "$0")"
source ./fuzz-smoke.sh

assert_eq() {
  if [ "$1" != "$2" ]; then
    echo "mismatch:" >&2
    echo "  got:  $1" >&2
    echo "  want: $2" >&2
    exit 1
  fi
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cd "$work"
git init -q
git config user.email test@example.com
git config user.name test

# --- success on the first attempt: no retry, no crasher scan needed -------
run_fuzz_attempt() { return 0; }
out="$(main FuzzThing ./pkg/ 15s 2>&1)"
assert_eq "$out" ""

# --- flake path: fails once with no crasher artifact, retries, succeeds ---
# main runs inside $(...), a subshell, so a plain variable bump in
# run_fuzz_attempt wouldn't be visible out here; count via a file instead.
counter="$work/attempts"
echo 0 >"$counter"
run_fuzz_attempt() {
  local n
  n="$(($(cat "$counter") + 1))"
  echo "$n" >"$counter"
  [ "$n" -ge 2 ]
}
out="$(main FuzzThing ./pkg/ 15s 2>&1)"
assert_eq "$(cat "$counter")" "2"
echo "$out" | grep -q "retrying once for the known shutdown-deadline flake"

# --- persistent failure, no crasher: fails after exactly one retry --------
run_fuzz_attempt() { return 1; }
if out="$(main FuzzThing ./pkg/ 15s 2>&1)"; then
  echo "expected main to fail on persistent breakage" >&2
  exit 1
fi
assert_eq "$(echo "$out" | grep -c "no crasher artifact")" "2"
echo "$out" | grep -q "not a flake, failing the job"

# --- real crasher: fails immediately on the FIRST failure, no retry -------
mkdir -p pkg/testdata/fuzz/FuzzThing
printf 'go test fuzz v1\nstring("boom")\n' >pkg/testdata/fuzz/FuzzThing/deadbeef

echo 0 >"$counter"
run_fuzz_attempt() {
  local n
  n="$(($(cat "$counter") + 1))"
  echo "$n" >"$counter"
  return 1
}
if out="$(main FuzzThing ./pkg/ 15s 2>&1)"; then
  echo "expected main to fail on a real crasher" >&2
  exit 1
fi
assert_eq "$(cat "$counter")" "1"
echo "$out" | grep -q "real crasher found for FuzzThing, failing immediately"
echo "$out" | grep -q "pkg/testdata/fuzz/FuzzThing/deadbeef"
echo "$out" | grep -q "boom"

rm -rf pkg/testdata/fuzz

echo "fuzz-smoke.sh: ok"
