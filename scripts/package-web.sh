#!/usr/bin/env bash
# Package the web client and complete the publishable web root.
#
# `nsh package --target web` emits the static site (…/Keel/) but not the host
# theme token document. `keel serve` hands that document (web/theme.json,
# published next to the web build) to plugin UIs through the bridge — see
# loadHostTheme in cmd/keel/plugins_bridge.go — so a site that skips this copy
# runs without plugin theme tokens.
#
# Usage: scripts/package-web.sh [out-dir]   (default: web/dist/web/keel)
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
out="${1:-$root/web/dist/web/keel}"
case "$out" in
  /*) ;;
  *) out="$root/$out" ;;
esac

(cd "$root/web" && nsh package --target web -o "$out")
cp "$root/web/theme.json" "$out/Keel/theme.json"
echo "web root: $out/Keel (theme.json included)"
