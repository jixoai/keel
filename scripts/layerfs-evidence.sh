#!/usr/bin/env bash
# W2.3 layerfs + file-plugin evidence gates.
#
# Runs the deterministic gates and writes one transcript per gate into the
# evidence directory. It never prints the test DSN. The full `go test ./...`
# run is the caller's gate (it needs the shared test database and takes
# minutes); this script runs the focused race/scale/GBash gates.
#
# Usage: scripts/layerfs-evidence.sh [EVIDENCE_DIR]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EVIDENCE="${1:-$ROOT/openspec/changes/keel-harness-v2/evidence/w2.3-layerfs}"
mkdir -p "$EVIDENCE"

run() { # NAME COMMAND...
  local name="$1"; shift
  printf '[layerfs-evidence] %s\n' "$name"
  {
    printf '$ %s\n\n' "$*"
    if "$@"; then
      printf '\n[exit 0]\n'
    else
      printf '\n[exit non-zero]\n'
      return 1
    fi
  } > "$EVIDENCE/$name.txt" 2>&1
}

cd "$ROOT"

run go-build go build ./...
run go-vet go vet ./internal/layerfs/... ./internal/fileplugin ./internal/toolsurface ./internal/harnessext ./cmd/keel
run test-race go test -race ./internal/layerfs/... ./internal/fileplugin ./internal/toolsurface ./internal/harnessext -count=1
run test-scale go test ./internal/fileplugin -run 'TestCaptureOneHundredThousandFilesIsOneEvent' -v -count=1
run test-gbash go test ./internal/layerfs/gbashfs -run 'TestGBashUsesLayerfsAtANonRootMount' -v -count=1
run test-write-through go test ./internal/fileplugin -run 'TestWriteReadPagingAndPreconditions|TestEditExactUniqueAndReplaceAll|TestMaterializeFromManifestAndSnapshot|TestWriteThroughReachesBash' -v -count=1
run test-fs-write-order go test ./internal/fileplugin -run 'TestFSWriteFollowsToolResultWithoutKeyCollision|TestCaptureCommitsExactlyOneBatchEvent|TestCaptureFailureIsOneSystemEvent' -v -count=1

if [[ -f "$ROOT/.env.local" ]]; then
  DSN="$(grep '^KEEL_TEST_DSN_W2_LAYERFS=' "$ROOT/.env.local" | cut -d= -f2- || true)"
  if [[ -n "$DSN" ]]; then
    KEEL_TEST_DSN="$DSN" run test-pg-manifest go test ./internal/fileplugin -run TestPostgresManifestProjection -v -count=1
  fi
fi

printf '[layerfs-evidence] transcripts in %s\n' "$EVIDENCE"
