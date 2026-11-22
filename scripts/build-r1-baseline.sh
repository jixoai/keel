#!/usr/bin/env bash
# Build the frozen R1 baseline runtime for the differential.
#
# The R1 gate compares the pre-change runtime against the candidate. "Pre-change"
# must be a fixed commit, not `main`: once a change merges, main moves and the
# differential would compare against itself. The reference is recorded once in
# cmd/r1diff/baseline.json (W0.5 re-pin: e7361f8, the last commit before the
# keel-service-graph work, with the r1-basic-v2 scenario).
#
# Usage: scripts/build-r1-baseline.sh [git-ref] [output]
#        KEEL_R1_PIN=<path> scripts/build-r1-baseline.sh
set -euo pipefail
PIN="${KEEL_R1_PIN:-cmd/r1diff/baseline.json}"
REF="${1:-}"
if [ -z "$REF" ]; then
  REF="$(node -e '
    const fs = require("node:fs");
    const pin = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
    if (!pin.ref) { console.error("pin has no ref"); process.exit(1); }
    process.stdout.write(pin.ref);
  ' "$PIN")"
fi
OUT="${2:-/tmp/keel-r1-base}"
SRC="$(mktemp -d /tmp/keel-r1-baseline.XXXXXX)"
trap 'rm -rf "$SRC"' EXIT
git archive "$REF" | tar -x -C "$SRC"
( cd "$SRC" && go build -o "$OUT" ./cmd/runtime )
echo "built R1 baseline $REF (pin: $PIN) -> $OUT"
