#!/usr/bin/env bash
# Smoke test for action/install.sh's pure functions (resolve_tag's pinned-
# version branch and arch_for do no network I/O, so this runs offline).
set -euo pipefail
cd "$(dirname "$0")"
source ./install.sh

assert_eq() {
  if [ "$1" != "$2" ]; then
    echo "mismatch: got '$1', want '$2'" >&2
    exit 1
  fi
}

# arch_for
assert_eq "$(arch_for X64)" amd64
assert_eq "$(arch_for ARM64)" arm64
if arch_for MIPS >/dev/null 2>&1; then
  echo "expected arch_for MIPS to fail" >&2
  exit 1
fi

# resolve_tag: pinned version, no network involved
resolve_tag "0.2.0" "surya-koritala/sigbound"
assert_eq "$tag" "v0.2.0"
assert_eq "$ver" "0.2.0"

resolve_tag "v0.2.0" "surya-koritala/sigbound"
assert_eq "$tag" "v0.2.0"
assert_eq "$ver" "0.2.0"

echo "install.sh: ok"
