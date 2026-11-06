#!/usr/bin/env bash
# Run the PI conformance + runtime test suites and write a machine-readable
# summary. Requires KEEL_TEST_DSN (PostgreSQL 18 with the runtime schema).
set -euo pipefail
cd "$(dirname "$0")/.."
: "${KEEL_TEST_DSN:?set KEEL_TEST_DSN to a PostgreSQL DSN}"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOTOOLCHAIN="${GOTOOLCHAIN:-go1.27.3}"
out="${1:-/tmp/keel-conformance.json}"
go test -json -count=1 \
  ./internal/loop ./internal/compaction ./internal/mcp ./internal/skills \
  ./internal/telemetry ./internal/agent ./internal/httpapi ./internal/blob ./internal/stream \
  > "$out"
exec python3 "$(dirname "$0")/summarize-conformance.py" "$out"
