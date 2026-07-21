#!/usr/bin/env bash
# Live AI conflict resolver for the Sigbound cell.
# The cell's CommandResolver invokes this per conflicted file with:
#   $SIGBOUND_BASE  $SIGBOUND_OURS  $SIGBOUND_THEIRS  -> temp files (the 3 versions)
#   $SIGBOUND_PATH                              -> the repo-relative file path
# It must print ONLY the resolved file content to stdout.
# Any failure exits non-zero -> the cell keeps the branch FLAGGED (fail-safe).
set -euo pipefail

CLAUDE="${SIGBOUND_CLAUDE:-claude}"

read -r -d '' PROMPT <<EOF || true
You are resolving a 3-way merge conflict for the file: ${SIGBOUND_PATH}

Produce the correctly MERGED file that keeps the changes from BOTH "OURS" and
"THEIRS" relative to "BASE". Both sides are additive and compatible. The result
must be valid, compilable code with no merge markers.

Output ONLY the raw merged file content. No markdown fences, no commentary.

===== BASE =====
$(cat "$SIGBOUND_BASE")

===== OURS =====
$(cat "$SIGBOUND_OURS")

===== THEIRS =====
$(cat "$SIGBOUND_THEIRS")
EOF

# One live model call. Strip any accidental code fences to keep raw file bytes.
"$CLAUDE" -p "$PROMPT" | sed '/^```/d'
