#!/usr/bin/env bash
# Binary-size gate (change keel-harness-v2, modality-input lane).
#
# Owner constraint (2026-11-23): the Go binary must not balloon. This gate
# builds the shipping commands twice — the base revision and the working tree —
# and fails when a binary grows past a small budget. It exists so a dependency
# (a PDF parser, an image format, a video probe) cannot slip in unnoticed: the
# modality lane deliberately adds none.
#
# Both shipped programs are gated: cmd/keel is the released CLI (release.yml)
# and cmd/runtime is the server image (Dockerfile). Measuring only one of them
# would let a dependency hide in the other.
#
# Usage:
#   scripts/binary-size.sh                      # base = HEAD (uncommitted delta)
#   scripts/binary-size.sh --base origin/main   # base = a ref (CI pull request)
#   scripts/binary-size.sh --budget-kb 256 --targets "linux/amd64"
#   scripts/binary-size.sh --bins "./cmd/runtime"
#
# The budget is KiB of growth per binary and target. A shrink always passes. The
# build is stripped (-s -w, -trimpath) and CGO is off, so the measurement is
# reproducible across machines; the numbers are compared, not the bytes.
#
# The base revision must be present in the local object store: the CI checkout
# uses fetch-depth 0 (shallow clones cannot measure a delta), and a local run
# needs the base commit reachable (git fetch it first if not).
set -euo pipefail

base_ref="HEAD"
budget_kb="${KEEL_BINARY_SIZE_BUDGET_KB:-128}"
targets=("linux/amd64" "linux/arm64")
bins=("./cmd/keel" "./cmd/runtime")

usage() {
  sed -n '2,26p' "$0" | sed 's/^# ?//'
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --base)
      [ "$#" -ge 2 ] || { echo "binary-size: --base needs a value" >&2; exit 2; }
      base_ref="$2"; shift 2 ;;
    --budget-kb)
      [ "$#" -ge 2 ] || { echo "binary-size: --budget-kb needs a value" >&2; exit 2; }
      budget_kb="$2"; shift 2 ;;
    --targets)
      [ "$#" -ge 2 ] || { echo "binary-size: --targets needs a value" >&2; exit 2; }
      # Space-separated list: "linux/amd64 linux/arm64".
      read -r -a targets <<<"$2"; shift 2 ;;
    --bins)
      [ "$#" -ge 2 ] || { echo "binary-size: --bins needs a value" >&2; exit 2; }
      # Space-separated list of module-relative package paths.
      read -r -a bins <<<"$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "binary-size: unknown argument $1" >&2; exit 2 ;;
  esac
done

case "$budget_kb" in
  ''|*[!0-9]*) echo "binary-size: --budget-kb must be a non-negative integer" >&2; exit 2 ;;
esac
if [ "${#targets[@]}" -eq 0 ]; then
  echo "binary-size: --targets needs at least one os/arch" >&2
  exit 2
fi
if [ "${#bins[@]}" -eq 0 ]; then
  echo "binary-size: --bins needs at least one package path" >&2
  exit 2
fi

repo_root="$(git rev-parse --show-toplevel)"
if ! base_sha="$(git -C "$repo_root" rev-parse --verify --quiet "$base_ref^{commit}")"; then
  echo "binary-size: base revision '$base_ref' is not in this clone." >&2
  echo "binary-size: the gate needs the base commit's tree (CI: actions/checkout fetch-depth: 0)." >&2
  exit 2
fi

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/keel-size.XXXXXX")"
trap 'rm -rf "$work_dir"' EXIT

# GNU stat vs BSD stat: the CI (Linux) uses the first, macOS the second.
file_size() {
  if stat -c %s "$1" >/dev/null 2>&1; then
    stat -c %s "$1"
  else
    stat -f %z "$1"
  fi
}

build_target() {
  # $1 = source dir, $2 = target, $3 = package path, $4 = output path
  local dir="$1" target="$2" pkg="$3" out="$4" goos goarch
  goos="${target%%/*}"
  goarch="${target#*/}"
  if [ "$goos" = "$target" ] || [ -z "$goarch" ]; then
    echo "binary-size: bad target $target (want os/arch)" >&2
    exit 2
  fi
  ( cd "$dir" && CGO_ENABLED=0 GOTMPDIR="$work_dir/gotmp" GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath -ldflags '-s -w' -o "$out" "$pkg" )
}

echo "== binary-size gate =="
echo "base:    $base_ref ($base_sha)"
echo "budget:  ${budget_kb} KiB per binary and target"
echo "targets: ${targets[*]}"
echo "bins:    ${bins[*]}"

# The base tree is extracted from git rather than checked out, so the current
# worktree (which may be another worktree in the same repo) is never touched.
mkdir -p "$work_dir/base" "$work_dir/current" "$work_dir/gotmp"
git -C "$repo_root" archive "$base_sha" | tar -x -C "$work_dir/base"
# The working tree copy excludes VCS metadata and client build outputs; only
# the module inputs matter for a size measurement. The built binary at the repo
# root is deliberately NOT excluded via a bare name pattern: bsdtar matches
# every path component, which would also drop cmd/keel.
tar -c -C "$repo_root" \
  --exclude .git --exclude web/node_modules --exclude web/.nsh \
  . | tar -x -C "$work_dir/current"

failed=0
measured=0
for bin in "${bins[@]}"; do
  label="$(basename "$bin")"
  for target in "${targets[@]}"; do
    arch="${target//\//-}"
    # A binary added by this change has no base build; report it rather than
    # failing the whole gate on a missing directory.
    if [ ! -d "$work_dir/base/${bin#./}" ]; then
      printf '%-11s %-16s (not present in base; new binary, nothing to compare)\n' "$label" "$target"
      continue
    fi
    build_target "$work_dir/base" "$target" "$bin" "$work_dir/base-$label-$arch"
    build_target "$work_dir/current" "$target" "$bin" "$work_dir/current-$label-$arch"
    base_size="$(file_size "$work_dir/base-$label-$arch")"
    current_size="$(file_size "$work_dir/current-$label-$arch")"
    delta=$((current_size - base_size))
    delta_kb=$((delta / 1024))
    measured=$((measured + 1))
    printf '%-11s %-16s base=%d current=%d delta=%+d bytes (%+d KiB)\n' \
      "$label" "$target" "$base_size" "$current_size" "$delta" "$delta_kb"
    if [ "$delta" -gt $((budget_kb * 1024)) ]; then
      echo "binary-size: $label ($target) grew ${delta_kb} KiB over the ${budget_kb} KiB budget" >&2
      failed=1
    fi
  done
done

if [ "$measured" -eq 0 ]; then
  echo "binary-size: no binary was measured (all requested packages are new?)" >&2
  exit 2
fi
if [ "$failed" -ne 0 ]; then
  echo "binary-size: FAIL (a dependency or generated code exceeded the budget)" >&2
  exit 1
fi
echo "binary-size: OK"
