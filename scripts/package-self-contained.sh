#!/usr/bin/env bash
# Build one self-contained keel binary for this machine: the Go binary + the
# platform's relocatable PostgreSQL 18 runtime + the packaged web client.
#
# Usage: scripts/package-self-contained.sh [--pg-version 18.7.0] [--out dist]
# The generated assets/ directory is gitignored; a checkout build has only the
# placeholder and falls back to external PostgreSQL/web.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
pg_version="18.7.0"
out="$root/dist"
while [ $# -gt 0 ]; do
  case "$1" in
    --pg-version) pg_version="$2"; shift 2 ;;
    --out) out="$2"; shift 2 ;;
    -h|--help) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

os="$(uname -s)"
arch="$(uname -m)"
case "$os/$arch" in
  Darwin/arm64) artifact="darwin-arm64v8" ;;
  Darwin/x86_64) artifact="darwin-amd64" ;;
  Linux/aarch64|Linux/arm64) artifact="linux-arm64v8" ;;
  Linux/x86_64) artifact="linux-amd64" ;;
  *) echo "unsupported platform: $os/$arch" >&2; exit 1 ;;
esac

base="https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-${artifact}/${pg_version}"
jar="${base}/embedded-postgres-binaries-${artifact}-${pg_version}.jar"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "→ fetching PostgreSQL ${pg_version} (${artifact})"
curl -fsSL --retry 3 -o "$tmp/pg.jar" "$jar"
echo "→ extracting the relocatable PG tree"
unzip -p "$tmp/pg.jar" '*.txz' > "$root/cmd/keel/assets/pg.txz"

echo "→ packaging the web client"
./scripts/package-web.sh "$root/web/dist/web/keel" >/dev/null
rm -rf "$root/cmd/keel/assets/web"
cp -R "$root/web/dist/web/keel/Keel" "$root/cmd/keel/assets/web"
cp "$root/web/icon.png" "$root/cmd/keel/assets/icon.png"
cp "$root/README.md" "$root/cmd/keel/assets/README.md"

mkdir -p "$out"
echo "→ building the single binary"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$out/keel" ./cmd/keel
size="$(du -h "$out/keel" | cut -f1)"
echo "✓ $out/keel ($size)  [PG ${pg_version}, ${artifact}]"
