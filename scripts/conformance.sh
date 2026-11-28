#!/usr/bin/env bash
# Run the PI conformance + runtime test suites and write a machine-readable
# summary. Requires KEEL_TEST_DSN (PostgreSQL 18 with the runtime schema).
#
# KEEL_CONFORMANCE_GOLDEN names a committed baseline (docs/conformance-golden.json):
# every package's pass count must stay at or above its floor. CI sets it on the
# PI-pin workflow so an upstream pin change cannot silently drop coverage.
set -euo pipefail
cd "$(dirname "$0")/.."
: "${KEEL_TEST_DSN:?set KEEL_TEST_DSN to a PostgreSQL DSN}"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOTOOLCHAIN="${GOTOOLCHAIN:-go1.27.3}"
out="${1:-/tmp/keel-conformance.json}"
# -p 1: every package here opens the same PostgreSQL database, and several
# suites run schema migrations. Package binaries must not migrate concurrently
# (the CI test job documents the same requirement); tests inside a package
# still run normally.
test_status=0
go test -json -count=1 -p 1 \
  ./internal/loop ./internal/compaction ./internal/mcp ./internal/skills \
  ./internal/telemetry ./internal/hooks ./internal/host ./internal/durable \
  ./internal/agenttool ./internal/agent ./internal/httpapi ./internal/blob ./internal/stream \
  > "$out" || test_status=$?
golden_args=()
if [ -n "${KEEL_CONFORMANCE_GOLDEN:-}" ]; then
  golden_args=(--golden "$KEEL_CONFORMANCE_GOLDEN")
fi
summary_status=0
python3 "$(dirname "$0")/summarize-conformance.py" "${golden_args[@]}" "$out" || summary_status=$?
# Summarize on every outcome (the CI artifact must exist even on failure), but
# a failed test run is a failed gate even if the summary itself succeeded.
if [ "$test_status" -ne 0 ]; then
  exit "$test_status"
fi
exit "$summary_status"
