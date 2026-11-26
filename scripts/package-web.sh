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

# nsh 0.7 builds the official nsh-extra module (speech) from Rust/wasm: use the
# rustup toolchain and its llvm-ar, and fail early with the install commands when
# wasm-bindgen is missing.
if [ -x "$HOME/.cargo/bin/cargo" ]; then PATH="$HOME/.cargo/bin:$PATH"; fi
if command -v rustc >/dev/null 2>&1; then
  rustbin="$(rustc --print sysroot)/lib/rustlib/$(rustc -vV | sed -n 's/^host: //p')/bin"
  [ -d "$rustbin" ] && PATH="$rustbin:$PATH"
fi
if ! command -v wasm-bindgen >/dev/null 2>&1; then
  echo "package-web: wasm-bindgen is required (nsh-extra web runtime):" >&2
  echo "  rustup target add wasm32-unknown-unknown && rustup component add llvm-tools" >&2
  echo "  cargo install wasm-bindgen-cli --version 0.2.129 --locked" >&2
  exit 1
fi
(cd "$root/web" && nsh package --target web -o "$out")
cp "$root/web/theme.json" "$out/Keel/theme.json"
echo "web root: $out/Keel (theme.json included)"
