#!/usr/bin/env bash
# Markdown code spans in printf literals are intentional (shellcheck SC2016).
# shellcheck disable=SC2016
# keel-board-sync-v2 Phase 2 final gates (tasks 16.1 and 16.2).
#
# This script generates, from real command output:
#   openspec/changes/keel-board-sync-v2/evidence/phase-2-gates.md
#   openspec/changes/keel-board-sync-v2/evidence/phase-2-pin.txt
#
# It also writes raw logs, the full-TUI JUnit/JSON, the file-coverage TSV and
# the excluded-path list under evidence/phase-2-gates-<stamp>/.
#
# The full TUI run is exclusive: the script acquires a lock, checks for other
# nsh/cargo/go test or build processes, and refuses to run the full suite when
# one is present. A refused run is recorded as a failed gate, never as a pass.
# Run mode also requires a clean tracked worktree and this script committed. It
# prefers the active rustup toolchain and a Homebrew llvm-ar when missing from
# PATH, because nsh builds the app's Rust crates.
#
# Usage:
#   KEEL_TEST_DSN='postgres://…' scripts/board-phase2-gates.sh
#   scripts/board-phase2-gates.sh --check-only   # mapping/syntax self-check only
#
# Environment:
#   KEEL_TEST_DSN              required for a real run; used by go test -p 1 ./...
#   KEEL_BOARD_BASE            default 4cbc3e50
#   KEEL_BOARD_CONTRACT        default f00097e7
#   EXPECTED_TUI_TESTS         default 540
#   EXPECTED_UI_SCREENSHOTS    default 55
#   EXPECTED_UI_MATRIX_CELLS   default 26
#
# Exit 0 only when every gate passes, file coverage has UNCOVERED=0, the full
# TUI ran exclusively with the expected count, and HEAD did not move mid-run.
set -euo pipefail

usage() {
  cat <<'USAGE'
keel-board-sync-v2 Phase 2 final gates (16.1/16.2)

Usage:
  KEEL_TEST_DSN='postgres://…' scripts/board-phase2-gates.sh
  scripts/board-phase2-gates.sh --check-only
  scripts/board-phase2-gates.sh --help

--check-only validates the repository pins, the changed-file coverage map and
the available tooling. It does not run nsh, go test, openspec or write the final
evidence files.
USAGE
}

die() {
  printf 'board-phase2-gates: %s\n' "$*" >&2
  exit 1
}

fmt_duration() {
  local seconds=$1
  if [ "$seconds" -ge 60 ]; then
    printf '%dm %ds' "$((seconds / 60))" "$((seconds % 60))"
  else
    printf '%ds' "$seconds"
  fi
}

json_get() {
  # json_get <file> <key>; prints nothing when the key is absent.
  node -e '
    const fs = require("node:fs");
    const j = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
    const v = j[process.argv[2]];
    if (v !== undefined && v !== null) process.stdout.write(String(v));
  ' "$1" "$2"
}

check_only=0
for arg in "$@"; do
  case "$arg" in
    --check-only|--self-test) check_only=1 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $arg" ;;
  esac
done

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || die "not inside a git worktree"
cd "$repo_root"

base_ref=${KEEL_BOARD_BASE:-4cbc3e50}
contract_ref=${KEEL_BOARD_CONTRACT:-f00097e7}
expected_tui_tests=${EXPECTED_TUI_TESTS:-540}
expected_ui_screenshots=${EXPECTED_UI_SCREENSHOTS:-55}
expected_ui_matrix=${EXPECTED_UI_MATRIX_CELLS:-26}

for tool in git go node nsh openspec; do
  command -v "$tool" >/dev/null 2>&1 || die "required tool not found: $tool"
done

git rev-parse --verify --quiet "${base_ref}^{commit}" >/dev/null || die "base ref not found: $base_ref"
git rev-parse --verify --quiet "${contract_ref}^{commit}" >/dev/null || die "contract ref not found: $contract_ref"
base_full=$(git rev-parse "${base_ref}^{commit}")
contract_full=$(git rev-parse "${contract_ref}^{commit}")
head=$(git rev-parse HEAD)
head_short=$(git rev-parse --short "$head")
git merge-base --is-ancestor "$base_full" "$head" || die "base $base_ref is not an ancestor of HEAD"
git merge-base --is-ancestor "$contract_full" "$head" || die "contract $contract_ref is not an ancestor of HEAD"

board_module=github.com/jixoai/keel-board
board_line=$(go list -m -f '{{.Path}} {{.Version}}' "$board_module" 2>/dev/null) || die "go list -m failed for $board_module"
board_path=${board_line%% *}
board_version=${board_line#* }
[ "$board_path" = "$board_module" ] || die "unexpected board module path: $board_path"
[ "$board_version" = "v0.1.4" ] || die "board module is $board_version, want v0.1.4"
board_commit=$(awk 'match($0, /commit [0-9a-f]{40}/) { print substr($0, RSTART + 7, 40); exit }' NOTICE)
[ "${#board_commit}" -eq 40 ] || die "cannot read the board commit from NOTICE"
grep -Eq '^[[:space:]]*github[.]com/jixoai/keel-board[[:space:]]+v0[.]1[.]1([[:space:]]|$)' go.mod \
  || die "go.mod does not pin $board_module $board_version"

node_version=$(node --version)
go_version=$(go version)
nsh_version=$(nsh --version 2>/dev/null || true)
openspec_version=$(openspec --version 2>/dev/null || true)

if [ "$check_only" -eq 0 ] && [ -z "${KEEL_TEST_DSN:-}" ]; then
  die "KEEL_TEST_DSN is required for go test -p 1 ./... (the real-PostgreSQL suites must run, not skip)"
fi

if [ "$check_only" -eq 0 ]; then
  tracked_dirty=$(git status --porcelain --untracked-files=no)
  [ -z "$tracked_dirty" ] || die "tracked worktree changes present; commit or rebase before the final gate"
  git ls-files --error-unmatch scripts/board-phase2-gates.sh >/dev/null 2>&1 \
    || die "commit scripts/board-phase2-gates.sh before the final gate"
fi

# nsh builds the app's Rust crates. On this Mac the Homebrew cargo can miss the
# active rustup toolchain and llvm-ar is not on the default PATH; prefer the
# active rustup bin and a versioned Homebrew LLVM only when the operator's PATH
# does not already provide them.
if [ "$check_only" -eq 0 ]; then
  if command -v rustup >/dev/null 2>&1; then
    rustc_from_rustup=$(rustup which rustc 2>/dev/null || true)
    if [ -n "$rustc_from_rustup" ] && [ -x "$rustc_from_rustup" ]; then
      rust_bin=$(dirname "$rustc_from_rustup")
      case ":$PATH:" in
        *":$rust_bin:"*) ;;
        *) PATH="$rust_bin:$PATH"; export PATH ;;
      esac
    fi
  fi
  if ! command -v llvm-ar >/dev/null 2>&1; then
    for candidate in \
      /opt/homebrew/opt/llvm/bin/llvm-ar \
      /opt/homebrew/Cellar/llvm@22/*/bin/llvm-ar \
      /opt/homebrew/Cellar/llvm@21/*/bin/llvm-ar \
      /opt/homebrew/Cellar/llvm/*/bin/llvm-ar \
      /usr/local/opt/llvm/bin/llvm-ar \
      /usr/local/Cellar/llvm@*/bin/llvm-ar \
      /usr/local/Cellar/llvm/*/bin/llvm-ar; do
      if [ -x "$candidate" ]; then
        PATH="$(dirname "$candidate"):$PATH"
        export PATH
        break
      fi
    done
  fi
  command -v cargo >/dev/null 2>&1 || die "cargo not found; export the rustup toolchain PATH"
  command -v rustc >/dev/null 2>&1 || die "rustc not found; export the rustup toolchain PATH"
  command -v llvm-ar >/dev/null 2>&1 || die "llvm-ar not found; export a Homebrew LLVM bin PATH"
fi
resolved_cargo=$(command -v cargo 2>/dev/null || true)
resolved_rustc=$(command -v rustc 2>/dev/null || true)
resolved_llvm_ar=$(command -v llvm-ar 2>/dev/null || true)

tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/keel-board-phase2-gates.XXXXXX")
lock_dir=""
lock_owned=0
raw_dir=""
raw_name=""
report_tmp=""
pin_tmp=""
cleanup() {
  if [ "$lock_owned" -eq 1 ] && [ -n "$lock_dir" ]; then
    rm -rf "$lock_dir"
  fi
  if [ -n "$tmp_root" ]; then
    rm -rf "$tmp_root"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

evidence_dir="$repo_root/openspec/changes/keel-board-sync-v2/evidence"
report="$evidence_dir/phase-2-gates.md"
pin="$evidence_dir/phase-2-pin.txt"

if [ "$check_only" -eq 0 ]; then
  mkdir -p "$evidence_dir"
  lock_dir="$evidence_dir/.phase-2-gates.lock"
  if ! mkdir "$lock_dir" 2>/dev/null; then
    lock_pid=$(cat "$lock_dir/pid" 2>/dev/null || true)
    if [ -n "$lock_pid" ] && kill -0 "$lock_pid" 2>/dev/null; then
      die "another board-phase2-gates run is active (pid $lock_pid); refusing a non-exclusive run"
    fi
    rm -rf "$lock_dir"
    mkdir "$lock_dir" || die "cannot acquire the gate lock at $lock_dir"
  fi
  lock_owned=1
  printf '%s\n' "$$" >"$lock_dir/pid"

  stamp=$(date -u '+%Y%m%dT%H%M%SZ')
  raw_name="phase-2-gates-$stamp"
  raw_dir="$evidence_dir/$raw_name"
  mkdir "$raw_dir" || die "evidence directory already exists: $raw_dir"
  coverage_md="$raw_dir/file-coverage.md"
  coverage_tsv="$raw_dir/file-coverage.tsv"
  coverage_json="$raw_dir/file-coverage.json"
  excluded_txt="$raw_dir/excluded-paths.txt"
else
  coverage_md="$tmp_root/file-coverage.md"
  coverage_tsv="$tmp_root/file-coverage.tsv"
  coverage_json="$tmp_root/file-coverage.json"
  excluded_txt="$tmp_root/excluded-paths.txt"
fi

# The coverage map is intentionally inline: it needs the actual diff, not a
# hand-maintained list. Evidence artifacts and generated discovered tests are
# excluded; every other changed file must match one of the rules below.
run_coverage_map() {
  local log="$1"
  local status
  set +e
  node - "$base_full" "$head" "$coverage_md" "$coverage_tsv" "$coverage_json" "$excluded_txt" >"$log" 2>&1 <<'NODE'
const fs = require("node:fs");
const { execFileSync } = require("node:child_process");

const [base, head, mdPath, tsvPath, summaryPath, excludedPath] = process.argv.slice(2);
const repo = process.cwd();
const diff = execFileSync(
  "git",
  ["-c", "core.quotePath=false", "diff", "--name-only", `${base}..${head}`],
  { cwd: repo, encoding: "utf8" },
);
const changed = diff.split("\n").map((line) => line.trim()).filter(Boolean);
const evidencePrefix = "openspec/changes/keel-board-sync-v2/evidence/";
const generated = [
  /(^|\/)board-phase2-capture\.test\.tsx$/,
  /(^|\/)board-sse-live\.test\.tsx$/,
  /(^|\/)\.DS_Store$/,
];
const excluded = [];
const files = [];
for (const file of changed) {
  if (file.startsWith(evidencePrefix) || generated.some((re) => re.test(file))) excluded.push(file);
  else files.push(file);
}

// Every rule names the concrete test, screenshot or script check that covers
// the file. The final gate has to show UNCOVERED=0 for this diff.
const rules = [
  { re: /^\.github\/workflows\/ci\.yml$/, cov: "CI parity: `go test -p 1 ./...` + `nsh check`" },
  { re: /^README\.md$/, cov: "docs pin scan + `openspec validate --strict`" },
  { re: /^web\/README\.md$/, cov: "docs pin scan + `openspec validate --strict`" },
  { re: /^docs\/HARNESS\.md$/, cov: "docs pin scan + `go test` harnessapi 404/report/ask" },
  { re: /^docs\/RUNBOOK\.md$/, cov: "docs pin scan + `openspec validate --strict`" },
  { re: /^openspec\/changes\/keel-board-sync-v2\/(design|tasks)\.md$/, cov: "`openspec validate --strict`" },
  { re: /^openspec\/changes\/keel-board-sync-v2\/DISPOSITION\.md$/, cov: "documented Phase 2 limitations + `openspec validate --strict`" },
  { re: /^cmd\/keel\/board_serve_options_test\.go$/, cov: "`go test -p 1 ./...` (cmd/keel mount options)" },
  { re: /^cmd\/keel\/board_superagent(_test)?\.go$/, cov: "`go test` cmd/keel gate + board 404 TUI" },
  { re: /^cmd\/keel\/(board|serve)\.go$/, cov: "`go test -p 1 ./...` (cmd/keel mount/serve)" },
  { re: /^internal\/access\//, cov: "`go test` access/builtin + permission screenshots" },
  { re: /^internal\/accessgate\/matrix\.go$/, cov: "`go test` accessgate (report/ask only) + 404 TUI" },
  { re: /^internal\/boardintegration\/augment\.go$/, cov: "`go test` boardintegration moves/me + SSE augment live" },
  { re: /^internal\/boardintegration\/authorizer\.go$/, cov: "`go test` boardintegration authorization" },
  { re: /^internal\/boardintegration\/channel_owners\.go$/, cov: "`go test` boardintegration channel-owner tests" },
  { re: /^internal\/boardintegration\/flags_pg_test\.go$/, cov: "`go test` boardintegration flag matrix PG" },
  { re: /^internal\/boardintegration\/me\.go$/, cov: "`go test` TestBoardMe + identity badge" },
  { re: /^internal\/boardintegration\/moves_pg_test\.go$/, cov: "`go test` TestTaskMoves* / TestBoardMe / sync merge" },
  { re: /^internal\/boardintegration\/pg_helpers_test\.go$/, cov: "`go test` boardintegration PG helpers" },
  { re: /^internal\/boardintegration\/surface\.go$/, cov: "`go test` boardintegration HTTP/RBAC surface" },
  { re: /^internal\/boardintegration\/sync_/, cov: "`go test` boardintegration sync/admin/runtime PG" },
  { re: /^internal\/boardintegration\//, cov: "`go test -p 1 ./...` (KEEL_TEST_DSN)" },
  { re: /^internal\/harnessapi\//, cov: "`go test` harnessapi 404 + worker report/ask" },
  { re: /^internal\/store\/relations/, cov: "`go test` store relations/orphan PG" },
  { re: /^scripts\/archive\/retired-superagent\/.*README\.md$/, cov: "retired-surface check + `openspec validate`" },
  { re: /^scripts\/archive\/retired-superagent\/.*\.(mjs|sh)$/, cov: "`node --check`/`bash -n` + retired-surface check" },
  { re: /^scripts\/archive\/retired-superagent\/.*\.go$/, cov: "`go test -p 1 ./...` (archive package compile) + retired-surface check" },
  { re: /^scripts\/archive\/retired-superagent\/.*\.html$/, cov: "retired-surface check + board gone screenshot 36" },
  { re: /^scripts\/board-fake-sse-server\.mjs$/, cov: "SSE live fake-server test" },
  { re: /^scripts\/board-phase2-ui-evidence\.mjs$/, cov: "UI evidence (HTML / 55 shots / 26 matrix)" },
  { re: /^scripts\/board-phase2-gates\.sh$/, cov: "this gate (`bash -n` + executed in this run)" },
  { re: /^scripts\/.*\.sh$/, cov: "`bash -n` + phase-1 evidence" },
  { re: /^scripts\/.*\.mjs$/, cov: "`node --check`" },
  { re: /^web\/evidence\/board-capture\.template\.tsx$/, cov: "UI screenshots (55)" },
  { re: /^web\/evidence\/board-sse-live\.template\.tsx$/, cov: "SSE live fake-server test" },
  { re: /^web\/App\.tsx$/, cov: "board TUI + screenshots + full TUI" },
  { re: /^web\/theme\.json$/, cov: "UI screenshots (theme tokens) + `nsh check`" },
  { re: /^web\/src\/data\/board-sse\.test\.ts$/, cov: "SSE tests (3)" },
  { re: /^web\/src\/data\/board\.test\.ts$/, cov: "board data tests (22)" },
  { re: /^web\/src\/data\/board\.ts$/, cov: "board data tests + SSE tests + board TUI" },
  { re: /^web\/src\/data\/board-wire\.ts$/, cov: "board data tests + SSE tests + board TUI" },
  { re: /^web\/src\/data\/contract\.ts$/, cov: "`nsh check` + board data tests (old types removed)" },
  { re: /^web\/src\/data\/fixture-board\.ts$/, cov: "board data tests + board TUI + screenshots" },
  { re: /^web\/src\/data\/fixture\.ts$/, cov: "board TUI permission/identity + screenshots" },
  { re: /^web\/src\/data\/fixture-identity\.ts$/, cov: "board TUI permission/identity + screenshots" },
  { re: /^web\/src\/data\/http\.ts$/, cov: "board data tests + SSE tests (real `KeelHttp`)" },
  { re: /^web\/src\/data\/identity-stream\.ts$/, cov: "full TUI (identity/session suite)" },
  { re: /^web\/src\/data\/identity-stream\.test\.ts$/, cov: "full TUI (identity/session suite)" },
  { re: /^web\/src\/data\/permission-catalog\.ts$/, cov: "board data tests + permission screenshots" },
  { re: /^web\/src\/data\/route\.ts$/, cov: "route tests: `/superagent*` -> gone + board TUI" },
  { re: /^web\/src\/data\/route\.test\.ts$/, cov: "route tests: `/superagent*` -> gone" },
  { re: /^web\/src\/data\/store\.ts$/, cov: "route tests + board TUI" },
  { re: /^web\/src\/data\/superagent\.ts$/, cov: "deleted old client; route tests + board TUI gone + screenshot 36" },
  { re: /^web\/src\/data\/superagent\.test\.ts$/, cov: "deleted old suite; route tests + full TUI" },
  { re: /^web\/src\/screens\/board\/automation\.tsx$/, cov: "board TUI automation + screenshots 34-37" },
  { re: /^web\/src\/screens\/board\/board\.tui\.test\.tsx$/, cov: "board TUI (13)" },
  { re: /^web\/src\/screens\/board\/harness\.tsx$/, cov: "UI screenshots (55, all scenarios)" },
  { re: /^web\/src\/screens\/board\/page\.tsx$/, cov: "board TUI + five-state screenshots" },
  { re: /^web\/src\/screens\/board\/queue\.tsx$/, cov: "board TUI queue + screenshots 01-14" },
  { re: /^web\/src\/screens\/board\/state\.ts$/, cov: "board TUI actions/dialogs" },
  { re: /^web\/src\/screens\/board\/sync\.tsx$/, cov: "board TUI sync + screenshots 30-33" },
  { re: /^web\/src\/screens\/board\/task\.tsx$/, cov: "board TUI task/lease/actions + screenshots 20-28" },
  { re: /^web\/src\/screens\/books\/superagent/, cov: "deleted old board; route tests + screenshot 36 + full TUI" },
  { re: /^web\/src\/screens\/books\//, cov: "books TUI + board TUI + full TUI" },
  { re: /^web\/src\/screens\/command-palette\.tsx$/, cov: "board TUI + full TUI" },
  { re: /^web\/src\/screens\/commands-local-error\.test\.tsx$/, cov: "full TUI (540)" },
  { re: /^web\/src\/screens\/settings-models\.test\.tsx$/, cov: "full TUI (540)" },
  { re: /^internal\//, cov: "`go test -p 1 ./...`" },
  { re: /^cmd\//, cov: "`go test -p 1 ./...` (compile)" },
];

function coverageFor(file) {
  for (const rule of rules) if (rule.re.test(file)) return rule.cov;
  return null;
}

const rows = files.map((file) => {
  const cov = coverageFor(file);
  return { file, cov, covered: Boolean(cov) };
});
const uncovered = rows.filter((row) => !row.covered);
const covered = rows.length - uncovered.length;
const escape = (value) => String(value).replace(/\|/g, "\\|");
const markdown = [
  `Files: ${rows.length}; covered: ${covered}; UNCOVERED: ${uncovered.length}; excluded: ${excluded.length}.`,
  "",
  "| File | Coverage evidence | Status |",
  "|---|---|---|",
  ...rows.map((row) => `| \`${escape(row.file)}\` | ${row.cov ? escape(row.cov) : "-"} | ${row.covered ? "COVERED" : "UNCOVERED"} |`),
  "",
].join("\n");
fs.writeFileSync(mdPath, markdown);
fs.writeFileSync(
  tsvPath,
  "file\tstatus\tcoverage\n" +
    rows.map((row) => `${row.file}\t${row.covered ? "COVERED" : "UNCOVERED"}\t${row.cov ?? "-"}`).join("\n") +
    "\n",
);
fs.writeFileSync(excludedPath, excluded.length ? `${excluded.join("\n")}\n` : "");
const summary = {
  files: rows.length,
  covered,
  uncovered: uncovered.length,
  excluded: excluded.length,
  uncoveredPaths: uncovered.map((row) => row.file),
};
fs.writeFileSync(summaryPath, `${JSON.stringify(summary, null, 2)}\n`);
console.log(`files=${summary.files} covered=${summary.covered} uncovered=${summary.uncovered} excluded=${summary.excluded}`);
if (rows.length === 0) {
  console.error("coverage map found no changed files");
  process.exit(1);
}
if (uncovered.length > 0) {
  console.error(`UNCOVERED files: ${uncovered.map((row) => row.file).join(", ")}`);
  process.exit(1);
}
NODE
  status=$?
  set -e
  return "$status"
}

if [ "$check_only" -eq 1 ]; then
  coverage_log="$tmp_root/file-coverage.log"
  coverage_status=0
  run_coverage_map "$coverage_log" || coverage_status=$?
  coverage_files=$(json_get "$coverage_json" files 2>/dev/null || printf '0')
  coverage_covered=$(json_get "$coverage_json" covered 2>/dev/null || printf '0')
  coverage_uncovered=$(json_get "$coverage_json" uncovered 2>/dev/null || printf '0')
  coverage_excluded=$(json_get "$coverage_json" excluded 2>/dev/null || printf '0')
  printf 'check-only: HEAD %s (%s), base %s, contract %s\n' "$head_short" "$head" "$base_ref" "$contract_ref"
  printf 'check-only: board %s %s commit %s\n' "$board_path" "$board_version" "$board_commit"
  printf 'check-only: coverage files=%s covered=%s UNCOVERED=%s excluded=%s\n' \
    "$coverage_files" "$coverage_covered" "$coverage_uncovered" "$coverage_excluded"
  if [ "$coverage_status" -ne 0 ] || [ "$coverage_uncovered" != "0" ]; then
    [ -s "$coverage_log" ] && cat "$coverage_log"
    printf 'check-only: FAIL\n' >&2
    exit 1
  fi
  printf 'check-only: PASS\n'
  exit 0
fi

gates_tsv="$raw_dir/gates.tsv"
: >"$gates_tsv"
failures=0
full_tui_busy_reason=""
full_json="$raw_dir/full-tui.json"
full_junit="$raw_dir/full-tui-junit.xml"
full_log="$raw_dir/full-tui.log"
ui_log="$raw_dir/ui-evidence.log"
ui_rel=""
ui_shots=0
ui_matrix="0/0"
ui_matrix_covered=0
ui_matrix_total=0
full_tui_summary="NOT RUN"

record_gate() {
  local name=$1 display=$2 status=$3 duration=$4 log_rel=$5
  printf '%s\t%s\t%s\t%s\t%s\n' "$name" "$display" "$status" "$duration" "$log_rel" >>"$gates_tsv"
  if [ "$status" -ne 0 ]; then
    failures=$((failures + 1))
    printf '[FAIL] %s (exit %s, %s)\n' "$name" "$status" "$duration"
  else
    printf '[ok] %s (%s)\n' "$name" "$duration"
  fi
}

run_block() {
  local name=$1 cwd=$2 log=$3 display=$4
  shift 4
  local started ended status duration log_rel
  started=$(date +%s)
  set +e
  ( cd "$cwd" && "$@" ) >"$log" 2>&1
  status=$?
  set -e
  ended=$(date +%s)
  duration=$((ended - started))
  printf '\nEXIT:%d\n' "$status" >>"$log"
  log_rel=${log#"$raw_dir"/}
  record_gate "$name" "$display" "$status" "$(fmt_duration "$duration")" "$log_rel"
}

run_script_syntax() {
  local log="$raw_dir/script-syntax.log"
  local list="$raw_dir/changed-scripts.txt"
  local started ended duration status count path
  started=$(date +%s)
  git diff --name-only "$base_full".."$head" | grep -E '\.(sh|mjs)$' >"$list" || true
  : >"$log"
  status=0
  count=0
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    count=$((count + 1))
    if [ ! -f "$path" ]; then
      printf 'SKIP missing/deleted: %s\n' "$path" >>"$log"
      continue
    fi
    case "$path" in
      *.sh)
        if bash -n "$path" >>"$log" 2>&1; then
          printf 'ok bash -n %s\n' "$path" >>"$log"
        else
          printf 'FAIL bash -n %s\n' "$path" >>"$log"
          status=1
        fi
        ;;
      *.mjs)
        if node --check "$path" >>"$log" 2>&1; then
          printf 'ok node --check %s\n' "$path" >>"$log"
        else
          printf 'FAIL node --check %s\n' "$path" >>"$log"
          status=1
        fi
        ;;
    esac
  done <"$list"
  if [ "$count" -eq 0 ]; then
    printf 'FAIL no changed shell/mjs files found\n' >>"$log"
    status=1
  fi
  printf '\nchecked=%d status=%d\n' "$count" "$status" >>"$log"
  ended=$(date +%s)
  duration=$((ended - started))
  record_gate "script-syntax" "bash -n / node --check changed scripts" "$status" "$(fmt_duration "$duration")" "script-syntax.log"
}

run_retired_surface() {
  local log="$raw_dir/retired-surface.log"
  local started ended duration status
  started=$(date +%s)
  set +e
  (
    set -e
    test ! -e web/src/data/superagent.ts
    test ! -e web/src/data/superagent.test.ts
    test ! -e web/src/screens/books/superagent.tsx
    test ! -e web/src/screens/books/superagent.harness.tsx
    test ! -e web/src/screens/books/superagent.tui.test.tsx
    test -f scripts/archive/retired-superagent/README.md
    grep -Fq 'must not be run' scripts/archive/retired-superagent/README.md
    grep -Fq '404' scripts/archive/retired-superagent/README.md
    empty_archive=$(find scripts/archive/retired-superagent -type f -empty -print)
    if [ -n "$empty_archive" ]; then
      printf 'empty archived file(s):\n%s\n' "$empty_archive" >&2
      exit 1
    fi
    archive_files=$(find scripts/archive/retired-superagent -type f | wc -l | tr -d ' ')
    if git grep -n \
      -e 'scripts/superagent-acceptance' \
      -e 'scripts/superagent-live-github' \
      -- ':!scripts/archive/**' \
         ':!openspec/changes/archive/**' \
         ':!openspec/changes/keel-board-sync-v2/evidence/**' \
         ':!scripts/board-phase2-gates.sh'; then
      printf 'active reference to a retired script\n' >&2
      exit 1
    fi
    printf 'retired surface: old web modules absent; %s archive files present; no active script references\n' "$archive_files"
  ) >"$log" 2>&1
  status=$?
  set -e
  ended=$(date +%s)
  duration=$((ended - started))
  record_gate "retired-surface" "old board files/scripts absent; archive only" "$status" "$(fmt_duration "$duration")" "retired-surface.log"
}

run_docs_pin() {
  local log="$raw_dir/docs-pin.log"
  local started ended duration status f
  started=$(date +%s)
  set +e
  (
    set -e
    for f in README.md docs/RUNBOOK.md NOTICE; do
      grep -Fq 'keel-board v0.1.4' "$f"
      grep -Fq "$board_commit" "$f"
    done
    grep -Eq '^[[:space:]]*github[.]com/jixoai/keel-board[[:space:]]+v0[.]1[.]4([[:space:]]|$)' go.mod
    grep -Fq '/v1/superagent/tickets/{id}/report' docs/HARNESS.md
    grep -Fq '旧看板' docs/HARNESS.md
    grep -Fq 'board' web/README.md
    printf 'docs pin: module/version/commit and retired-board copy present\n'
  ) >"$log" 2>&1
  status=$?
  set -e
  ended=$(date +%s)
  duration=$((ended - started))
  record_gate "docs-pin" "module/commit/docs scan" "$status" "$(fmt_duration "$duration")" "docs-pin.log"
}

full_tui_exclusive() {
  local pattern pids pid command_line
  full_tui_busy_reason=""
  if ! command -v pgrep >/dev/null 2>&1; then
    full_tui_busy_reason="pgrep is unavailable; cannot prove the full TUI run is exclusive"
    return 1
  fi
  for pattern in 'nsh test' 'cargo test' 'cargo build' 'cargo run' 'go test' 'go vet' 'go build' 'board-phase2-ui-evidence[.]mjs'; do
    pids=$(pgrep -f "$pattern" 2>/dev/null || true)
    # shellcheck disable=SC2086
    for pid in $pids; do
      [ "$pid" = "$$" ] && continue
      command_line=$(ps -p "$pid" -o command= 2>/dev/null || true)
      full_tui_busy_reason="${full_tui_busy_reason}pid ${pid} (${pattern}): ${command_line}; "
    done
  done
  if [ -n "$full_tui_busy_reason" ]; then
    printf 'busy: %s\n' "$full_tui_busy_reason" >&2
    return 1
  fi
  printf 'exclusive preflight: no other nsh/cargo/go test or build, or UI evidence process\n'
  return 0
}

write_skipped_full_tui() {
  local reason=$1
  {
    printf 'full TUI was not run: %s\n' "$reason"
    printf 'The gate refuses to produce non-exclusive full-TUI evidence.\n'
  } >"$full_log"
  node - "$full_json" "$head" "$reason" <<'NODE'
const fs = require("node:fs");
const [path, head, reason] = process.argv.slice(2);
fs.writeFileSync(path, `${JSON.stringify({
  command: "nsh test --target tui",
  exit: null,
  tests: 0,
  passed: 0,
  failed: 0,
  skipped: 0,
  duration: "not run",
  durationSeconds: 0,
  exclusive: false,
  head,
  reason,
}, null, 2)}\n`);
NODE
  full_tui_summary="NOT RUN (not exclusive)"
}

write_full_tui_json() {
  local status=$1 seconds=$2 human=$3 exclusive=$4
  node - "$full_junit" "$full_json" "$status" "$seconds" "$human" "$head" "$exclusive" <<'NODE'
const fs = require("node:fs");
const [junitPath, outPath, exitText, secondsText, human, head, exclusiveText] = process.argv.slice(2);
let xml = "";
try {
  xml = fs.readFileSync(junitPath, "utf8");
} catch {
  xml = "";
}
const root = xml.match(/<testsuites\b[^>]*>/)?.[0] ?? xml.match(/<testsuite\b[^>]*>/)?.[0] ?? "";
const number = (name) => {
  const match = root.match(new RegExp(`\\b${name}="([0-9.]+)"`));
  return match ? Number(match[1]) : 0;
};
const tests = number("tests");
const failures = number("failures");
const errors = number("errors");
const skipped = number("skipped");
const failed = failures + errors;
const passed = Math.max(0, tests - failed - skipped);
const junitSeconds = number("time");
const summary = {
  command: "nsh test --target tui",
  exit: Number(exitText),
  tests,
  passed,
  failed,
  skipped,
  duration: human,
  durationSeconds: Number(secondsText),
  junitSeconds,
  exclusive: exclusiveText === "true",
  head,
  junit: "full-tui-junit.xml",
};
fs.writeFileSync(outPath, `${JSON.stringify(summary, null, 2)}\n`);
NODE
}

run_full_tui() {
  local started ended status duration gate_status
  if ! full_tui_exclusive >"$raw_dir/full-tui-exclusive.log" 2>&1; then
    write_skipped_full_tui "$full_tui_busy_reason"
    record_gate "full-tui" "nsh test --target tui" 1 "0s" "full-tui.log"
    return
  fi
  started=$(date +%s)
  set +e
  ( cd "$repo_root/web" && nsh test --target tui --junit "$full_junit" ) >"$full_log" 2>&1
  status=$?
  set -e
  ended=$(date +%s)
  duration=$((ended - started))
  printf '\nEXIT:%d\n' "$status" >>"$full_log"
  if ! write_full_tui_json "$status" "$duration" "$(fmt_duration "$duration")" "true"; then
    printf '{"command":"nsh test --target tui","exit":%d,"tests":0,"passed":0,"failed":0,"skipped":0,"duration":"%s","exclusive":true,"head":"%s"}\n' \
      "$status" "$(fmt_duration "$duration")" "$head" >"$full_json"
  fi

  local full_exit full_tests full_passed full_failed full_skipped full_exclusive
  full_exit=$(json_get "$full_json" exit 2>/dev/null || printf '')
  full_tests=$(json_get "$full_json" tests 2>/dev/null || printf '0')
  full_passed=$(json_get "$full_json" passed 2>/dev/null || printf '0')
  full_failed=$(json_get "$full_json" failed 2>/dev/null || printf '0')
  full_skipped=$(json_get "$full_json" skipped 2>/dev/null || printf '0')
  full_exclusive=$(json_get "$full_json" exclusive 2>/dev/null || printf 'false')

  gate_status=0
  [ "$status" -eq 0 ] || gate_status=1
  [ "$full_exit" = "0" ] || gate_status=1
  [ "$full_tests" = "$expected_tui_tests" ] || gate_status=1
  [ "$full_passed" = "$expected_tui_tests" ] || gate_status=1
  [ "$full_failed" = "0" ] || gate_status=1
  [ "$full_skipped" = "0" ] || gate_status=1
  [ "$full_exclusive" = "true" ] || gate_status=1

  lingering=$(pgrep -f 'nsh test --target tui' 2>/dev/null || true)
  if [ -n "$lingering" ]; then
    printf 'lingering full TUI process after exit: %s\n' "$lingering" >>"$full_log"
    gate_status=1
  fi

  if [ "$gate_status" -eq 0 ]; then
    full_tui_summary="${full_passed}/${full_tests} (exit ${full_exit}, $(fmt_duration "$duration"), exclusive=${full_exclusive})"
  else
    full_tui_summary="FAIL (tests=${full_tests}, passed=${full_passed}, failed=${full_failed}, skipped=${full_skipped}, exit=${full_exit})"
  fi
  record_gate "full-tui" "nsh test --target tui (exclusive)" "$gate_status" "$(fmt_duration "$duration")" "full-tui.log"
}

# 1. Diff coverage: every non-evidence file must map to a real check.
coverage_status=0
run_coverage_map "$raw_dir/file-coverage.log" || coverage_status=$?
coverage_files=$(json_get "$coverage_json" files 2>/dev/null || printf '0')
coverage_covered=$(json_get "$coverage_json" covered 2>/dev/null || printf '0')
coverage_uncovered=$(json_get "$coverage_json" uncovered 2>/dev/null || printf '0')
coverage_excluded=$(json_get "$coverage_json" excluded 2>/dev/null || printf '0')
record_gate "file-coverage" "inline node map of git diff --name-only" "$coverage_status" "0s" "file-coverage.log"

# 2. Cheap structural checks.
run_script_syntax
run_retired_surface
run_docs_pin

# 3. Type check, then the existing UI evidence suite (data/SSE/TUI/web shots).
run_block "nsh-check" "$repo_root/web" "$raw_dir/nsh-check.log" "nsh check" nsh check
run_block "ui-evidence" "$repo_root" "$ui_log" "node scripts/board-phase2-ui-evidence.mjs" node scripts/board-phase2-ui-evidence.mjs

ui_ok=1
ui_gates_line=$(grep '^gates: ' "$ui_log" 2>/dev/null | tail -n 1 || true)
ui_dir=$(sed -n 's/^evidence: //p' "$ui_log" 2>/dev/null | tail -n 1 || true)
if [ -n "$ui_dir" ] && [ -d "$ui_dir" ]; then
  ui_rel=${ui_dir#"$repo_root"/}
  if [ -d "$ui_dir/shots/web" ]; then
    ui_shots=$(find "$ui_dir/shots/web" -maxdepth 1 -type f -name '*.png' 2>/dev/null | wc -l | tr -d ' ')
  fi
  if [ -f "$ui_dir/README.txt" ]; then
    ui_matrix_line=$(grep '^Coverage matrix:' "$ui_dir/README.txt" 2>/dev/null | tail -n 1 || true)
    ui_matrix=$(printf '%s\n' "$ui_matrix_line" | sed -n 's/^Coverage matrix: \([0-9]*\)\/\([0-9]*\) covered;.*/\1\/\2/p')
    if [ -n "$ui_matrix" ]; then
      ui_matrix_covered=${ui_matrix%/*}
      ui_matrix_total=${ui_matrix#*/}
    fi
  fi
else
  ui_ok=0
fi
[ -f "$ui_dir/index.html" ] 2>/dev/null || ui_ok=0
[ -f "$ui_dir/README.txt" ] 2>/dev/null || ui_ok=0
[ "$ui_shots" = "$expected_ui_screenshots" ] || ui_ok=0
[ "$ui_matrix_covered" = "$expected_ui_matrix" ] || ui_ok=0
[ "$ui_matrix_total" = "$expected_ui_matrix" ] || ui_ok=0
[ "$ui_gates_line" = "gates: PASS" ] || ui_ok=0
if [ "$ui_ok" -ne 0 ]; then
  ui_summary="PASS (${ui_shots} screenshots, matrix ${ui_matrix}, ${ui_rel})"
else
  ui_summary="FAIL (shots=${ui_shots}, matrix=${ui_matrix}, gates='${ui_gates_line}', dir='${ui_rel}')"
  {
    printf 'UI evidence derived checks failed\n'
    printf 'command exit: %s\n' "$(awk -F '\t' '$1=="ui-evidence" {print $3}' "$gates_tsv" | tail -n 1)"
    printf 'expected screenshots: %s; actual: %s\n' "$expected_ui_screenshots" "$ui_shots"
    printf 'expected matrix cells: %s; actual: %s\n' "$expected_ui_matrix" "$ui_matrix"
    printf 'gates line: %s\n' "$ui_gates_line"
    printf 'directory: %s\n' "$ui_rel"
  } >>"$ui_log"
  record_gate "ui-evidence-check" "55 screenshots / 26 matrix / PASS" 1 "0s" "ui-evidence.log"
fi

# 4. The full TUI runs only after the UI suite has completely exited.
run_full_tui

# 5. Go unit/integration with the real PostgreSQL suites, then OpenSpec.
run_block "go-test" "$repo_root" "$raw_dir/go-tests.log" "KEEL_TEST_DSN=*** go test -p 1 ./... -count=1" \
  env KEEL_TEST_DSN="$KEEL_TEST_DSN" go test -p 1 ./... -count=1
run_block "openspec-validate" "$repo_root" "$raw_dir/openspec-validate.log" "openspec validate keel-board-sync-v2 --strict" \
  openspec validate keel-board-sync-v2 --strict

head_after=$(git rev-parse HEAD)
head_changed=0
if [ "$head_after" != "$head" ]; then
  head_changed=1
  failures=$((failures + 1))
  printf '[FAIL] HEAD moved during the gate: %s -> %s\n' "$head" "$head_after"
  printf 'HEAD moved during the gate: %s -> %s\n' "$head" "$head_after" >>"$raw_dir/head-check.log"
else
  printf 'HEAD stable at %s\n' "$head" >"$raw_dir/head-check.log"
fi

gate_status_text="PASS"
if [ "$failures" -ne 0 ] || [ "$coverage_uncovered" != "0" ] || [ "$head_changed" -ne 0 ]; then
  gate_status_text="FAIL"
fi

full_exit_pin=$(json_get "$full_json" exit 2>/dev/null || printf 'not-run')
[ -n "$full_exit_pin" ] || full_exit_pin="not-run"
full_tests_pin=$(json_get "$full_json" tests 2>/dev/null || printf '0')
full_passed_pin=$(json_get "$full_json" passed 2>/dev/null || printf '0')
full_duration_pin=$(json_get "$full_json" duration 2>/dev/null || printf 'unknown')
full_exclusive_pin=$(json_get "$full_json" exclusive 2>/dev/null || printf 'false')
go_status=$(awk -F '\t' '$1=="go-test" {print $3}' "$gates_tsv" | tail -n 1)
openspec_status=$(awk -F '\t' '$1=="openspec-validate" {print $3}' "$gates_tsv" | tail -n 1)

report_tmp="$raw_dir/phase-2-gates.md"
pin_tmp="$raw_dir/phase-2-pin.txt"
{
  printf '# keel-board-sync-v2 Phase 2 gates (16.1-16.2)\n\n'
  printf 'Generated by `scripts/board-phase2-gates.sh` at %s.\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  printf 'This file is script output, not hand-written.\n\n'
  printf '## Pin\n\n'
  printf -- '- HEAD: `%s` (`%s`)\n' "$head" "$head_short"
  printf -- '- base: `%s` (`%s`)\n' "$base_ref" "$base_full"
  printf -- '- contract: `%s` (`%s`)\n' "$contract_ref" "$contract_full"
  printf -- '- board module: `%s %s`\n' "$board_path" "$board_version"
  printf -- '- board commit: `%s`\n' "$board_commit"
  printf -- '- go: `%s`\n' "$go_version"
  printf -- '- node: `%s`; nsh: `%s`; openspec: `%s`\n' "$node_version" "$nsh_version" "$openspec_version"
  printf -- '- toolchain: cargo `%s`; rustc `%s`; llvm-ar `%s`\n' "$resolved_cargo" "$resolved_rustc" "$resolved_llvm_ar"
  printf -- '- gate: **%s**\n' "$gate_status_text"
  printf -- '- pin file: [phase-2-pin.txt](phase-2-pin.txt)\n'
  printf -- '- raw evidence: `%s/`\n\n' "$raw_name"

  printf '## Gates\n\n'
  printf '| Gate | Command | Result | Time | Log |\n'
  printf '|---|---|---|---|---|\n'
  while IFS=$'\t' read -r gate_name gate_command gate_status gate_duration gate_log; do
    [ -n "$gate_name" ] || continue
    if [ "$gate_status" -eq 0 ]; then
      gate_result=PASS
    else
      gate_result=FAIL
    fi
    printf '| %s | `%s` | %s | %s | [%s](%s/%s) |\n' \
      "$gate_name" "$gate_command" "$gate_result" "$gate_duration" "$gate_log" "$raw_name" "$gate_log"
  done <"$gates_tsv"
  printf '\nFull TUI is run only after an exclusive preflight: a gate lock, plus a check for other `nsh test`, `cargo test`/`build`/`run`, `go test`/`vet`/`build` and UI-evidence processes. A busy host is recorded as a failed gate instead of being run anyway.\n\n'

  printf '## Full TUI\n\n'
  printf '```json\n'
  if [ -f "$full_json" ]; then cat "$full_json"; else printf '{}\n'; fi
  printf '```\n\n'
  printf -- '- summary: %s\n' "$full_tui_summary"
  printf -- '- log: [full-tui.log](%s/full-tui.log)\n' "$raw_name"
  printf -- '- JUnit: [full-tui-junit.xml](%s/full-tui-junit.xml)\n' "$raw_name"
  printf -- '- exclusive preflight: [full-tui-exclusive.log](%s/full-tui-exclusive.log)\n\n' "$raw_name"

  printf '## UI evidence (16.2)\n\n'
  printf -- '- command: [ui-evidence.log](%s/ui-evidence.log)\n' "$raw_name"
  printf -- '- result: %s\n' "$ui_summary"
  if [ -n "$ui_rel" ]; then
    printf -- '- report: [%s/index.html](%s/index.html)\n' "$ui_rel" "$ui_rel"
    printf -- '- README: [%s/README.txt](%s/README.txt)\n' "$ui_rel" "$ui_rel"
  fi
  printf -- '- expected: %s screenshots, %s/%s matrix cells\n\n' "$expected_ui_screenshots" "$expected_ui_matrix" "$expected_ui_matrix"

  printf '## Go and OpenSpec\n\n'
  printf -- '- `go test -p 1 ./... -count=1` (KEEL_TEST_DSN set): exit **%s**; [go-tests.log](%s/go-tests.log)\n' "$go_status" "$raw_name"
  printf -- '- `openspec validate keel-board-sync-v2 --strict`: exit **%s**; [openspec-validate.log](%s/openspec-validate.log)\n\n' "$openspec_status" "$raw_name"

  printf '## File coverage\n\n'
  if [ -f "$coverage_md" ]; then cat "$coverage_md"; else printf 'Coverage map was not generated.\n'; fi
  printf '\n- excluded paths (evidence/generated): [excluded-paths.txt](%s/excluded-paths.txt)\n' "$raw_name"
  printf -- '- machine-readable map: [file-coverage.tsv](%s/file-coverage.tsv)\n\n' "$raw_name"

  printf '## Logs\n\n'
  printf -- '- [nsh-check.log](%s/nsh-check.log)\n' "$raw_name"
  printf -- '- [script-syntax.log](%s/script-syntax.log)\n' "$raw_name"
  printf -- '- [retired-surface.log](%s/retired-surface.log)\n' "$raw_name"
  printf -- '- [docs-pin.log](%s/docs-pin.log)\n' "$raw_name"
  printf -- '- [file-coverage.log](%s/file-coverage.log)\n' "$raw_name"
  printf -- '- [full-tui.log](%s/full-tui.log)\n' "$raw_name"
  printf -- '- [go-tests.log](%s/go-tests.log)\n' "$raw_name"
  printf -- '- [openspec-validate.log](%s/openspec-validate.log)\n' "$raw_name"
  printf -- '- [head-check.log](%s/head-check.log)\n' "$raw_name"
} >"$report_tmp"

{
  printf '# keel-board-sync-v2 Phase 2 gate pin\n'
  printf 'generated_at: %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  printf 'gate: %s\n' "$gate_status_text"
  printf 'head: %s\n' "$head"
  printf 'head_short: %s\n' "$head_short"
  printf 'base: %s\n' "$base_ref"
  printf 'base_full: %s\n' "$base_full"
  printf 'contract: %s\n' "$contract_ref"
  printf 'contract_full: %s\n' "$contract_full"
  printf 'board_module: %s %s\n' "$board_path" "$board_version"
  printf 'board_commit: %s\n' "$board_commit"
  if [ -n "$ui_rel" ]; then
    printf 'ui_evidence: %s\n' "$ui_rel"
  else
    printf 'ui_evidence: NOT FOUND\n'
  fi
  printf 'ui_screenshots: %s\n' "$ui_shots"
  printf 'ui_matrix: %s\n' "$ui_matrix"
  if [ "$gate_status_text" = "PASS" ]; then
    printf 'full_tui: %s/%s (exit %s, %s, exclusive=%s)\n' "$full_passed_pin" "$full_tests_pin" "$full_exit_pin" "$full_duration_pin" "$full_exclusive_pin"
  else
    printf 'full_tui: %s (tests=%s passed=%s exit=%s exclusive=%s)\n' "$full_tui_summary" "$full_tests_pin" "$full_passed_pin" "$full_exit_pin" "$full_exclusive_pin"
  fi
  printf 'go_test: exit %s\n' "$go_status"
  printf 'openspec_validate: exit %s\n' "$openspec_status"
  printf 'file_coverage: %s/%s, UNCOVERED=%s, excluded=%s\n' "$coverage_covered" "$coverage_files" "$coverage_uncovered" "$coverage_excluded"
  printf 'evidence_raw: %s\n' "$raw_name"
} >"$pin_tmp"

mv "$report_tmp" "$report"
mv "$pin_tmp" "$pin"

printf '\nfull TUI: %s\n' "$full_tui_summary"
printf 'UI evidence: %s\n' "$ui_summary"
printf 'file coverage: %s/%s covered, UNCOVERED=%s, excluded=%s\n' \
  "$coverage_covered" "$coverage_files" "$coverage_uncovered" "$coverage_excluded"
printf 'gate: %s\n' "$gate_status_text"
printf 'report: %s\n' "$report"
printf 'pin: %s\n' "$pin"

if [ "$gate_status_text" != "PASS" ]; then
  exit 1
fi
