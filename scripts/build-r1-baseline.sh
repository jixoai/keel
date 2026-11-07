#!/usr/bin/env bash
# Build the frozen W0 baseline runtime for the R1 differential.
#
# The R1 gate compares the pre-change runtime against the candidate. "Pre-change"
# must be a fixed commit, not `main`: once this change merges, main moves and the
# differential would compare against itself. The frozen base is e7361f8 (the last
# commit before the keel-service-graph work started).
#
# Usage: scripts/build-r1-baseline.sh [git-ref] [output]
set -euo pipefail
REF="${1:-e7361f8}"
OUT="${2:-/tmp/keel-r1-base}"
SRC="$(mktemp -d /tmp/keel-r1-baseline.XXXXXX)"
trap 'rm -rf "$SRC"' EXIT
git archive "$REF" | tar -x -C "$SRC"
( cd "$SRC" && go build -o "$OUT" ./cmd/runtime )
echo "built R1 baseline $REF -> $OUT"
