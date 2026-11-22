#!/usr/bin/env bash
# naming-scan.sh — 产品命名纪律扫描（keel-harness-naming N1/N5 +
# keel-bot-identity-access A5）
#
# 对外文件不得再用「Agent」指代产品层；产品实体统一 **Bot / TeamBot / Account /
# Role / Profile**，产品层外壳是 **Harness**。白名单每类都在输出里如实标注：
#   1. protocol       —— 协议名 AgentAPI / AgentBackend。它们是契约（不改名，改名破坏
#                        客户端与适配器），保留；文档在首次引入它们的页面（README、
#                        docs/HARNESS.md、术语表）标注技术名词例外。
#   2. code-ident      —— 仓库 Go/TS 源码中真实存在的 Agent* 标识符，以及以 `.` 引用的
#                        结构字段路径（如 `TenantConfig.Agent`）。文档引用代码符号是在
#                        引用代码事实，保留。裸词「Agent」「Agents」不在此列。
#   3. alias           —— 专门说明改名的句子（「原 Agent」「旧称」「更名」「删除」
#                        「formerly」「old」「deleted」）；这类行逐条打印，便于人工复核。
#   4. upstream        —— 上游命名：pi/pigo 的「Agent Skills 规范」、agentskills.io、
#                        「pi Agent」（描述 pigo 时的上游事实说法）。
#   5. ui-debt         —— 已知 UI fixture 遗留（当前为空；逐条打印，不静默丢弃）。
#   6. historical      —— 历史规划/残留计划文档；文件级白名单。
#
# A5 追加 legacy 扫描：docs 中不得再出现仍在使用的旧产品入口（`/v1/agents`、
# `/v1/teams`、`keel agent`、`keel team`、`TeamAgent`、`agent_teams`、
# `KEEL_SERVE_PASSWORD`）。说明「已删除/retired/旧」的句子允许并计数。
#
# 代码文件（cmd/keel/serve.go、web/src/data/fixture.ts）只扫描字符串字面量：内部字段名/注释
# 不是对外文案，只改字面量。
#
# 其余任何 capital-A `Agent` 或仍在使用旧入口的文档行命中即违规。脚本以非零码退出并打印
# file:line: token。
#
# Usage:
#   scripts/naming-scan.sh                # 扫描，零违规退出 0，有违规退出 1
#   scripts/naming-scan.sh --quiet        # 只打印统计与违规
set -euo pipefail
cd "$(dirname "$0")/.."

QUIET=0
if [[ "${1:-}" == "--quiet" ]]; then
  QUIET=1
fi

python3 - "$QUIET" <<'PY'
import pathlib, re, sys

root = pathlib.Path(".")
quiet = sys.argv[1] == "1"

# --- scope: outward-facing files -------------------------------------------
files = [
    root / "README.md",
    root / "web/README.md",
    root / "cmd/keel/serve.go",          # default system prompt literal
    root / "web/src/data/fixture.ts",    # user-visible demo strings
]
files += sorted((root / "docs").rglob("*.md"))
files += sorted((root / "site/docs").rglob("*.md"))  # site content only; config is not an outward doc
files = [f for f in files if f.is_file()]

# --- whitelist: protocol names ----------------------------------------------
PROTOCOL = {"AgentAPI", "AgentBackend"}

# --- whitelist: real code identifiers (Agent*, length > 5) -------------------
CODE_EXT = {".go", ".ts", ".tsx", ".js", ".mjs"}
SKIP_DIRS = {".git", "node_modules", ".worktrees", "dist", "build"}
code_ids = set()
for path in root.rglob("*"):
    if not path.is_file() or path.suffix not in CODE_EXT:
        continue
    if any(part in SKIP_DIRS for part in path.parts):
        continue
    try:
        text = path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        continue
    for token in re.findall(r"[A-Za-z_][A-Za-z0-9_]*", text):
        if token.startswith("Agent") and len(token) > len("Agent"):
            code_ids.add(token)

# --- whitelist: upstream naming ---------------------------------------------
UPSTREAM = re.compile(r"pi Agent|PI Agent|pigo|Agent Skills|agentskills\.io")
# --- whitelist: sentences that document the rename itself -------------------
ALIAS = re.compile(
    r"原 Agent|旧称|原名|别名|取代|更名|删除|\bold\b|\bdeleted\b|rename|formerly|earlier|alias"
)
# --- known debt (printed, not silently dropped) ----------------------------
# The retired Agent settings/fixture UI is gone; there is no known UI debt.
# Keep the category so a future exception is printed, never silently ignored.
KNOWN_UI_DEBT = {}
# Historical planning notes describe the old model; they are not product copy.
HISTORICAL_DOCS = {
    "docs/RESIDUAL-PLAN.md": "historical residual plan (pre-A5 vocabulary)",
}
# --- code files are scanned inside string literals only ---------------------
QUOTED = re.compile(r'"(?:[^"\\]|\\.)*"' + r"|'(?:[^'\\]|\\.)*'" + r"|`[^`]*`")
CODE_FILES = {".go", ".ts", ".tsx"}

TOKEN = re.compile(r"(?<![A-Za-z0-9_])Agent[A-Za-z0-9_]*")
counts = {"protocol": 0, "code-ident": 0, "alias": 0, "upstream": 0, "ui-debt": 0, "historical": 0}
alias_lines = []
debt_lines = []
violations = []

for f in files:
    try:
        lines = f.read_text(encoding="utf-8", errors="replace").splitlines()
    except OSError:
        continue
    is_code = f.suffix in CODE_FILES
    for lineno, line in enumerate(lines, 1):
        haystack = " ".join(m.group(0) for m in QUOTED.finditer(line)) if is_code else line
        if not haystack:
            continue
        # prose wraps; an alias note may continue onto the next line, so the
        # alias/upstream check reads the current line plus the two preceding ones.
        context = " ".join(lines[max(0, lineno - 3):lineno])
        for match in TOKEN.finditer(haystack):
            token = match.group(0)
            before = haystack[match.start() - 1] if match.start() > 0 else ""
            if token in PROTOCOL:
                counts["protocol"] += 1
                continue
            if token in code_ids or before == ".":
                counts["code-ident"] += 1
                continue
            key = (str(f), lineno)
            if key in KNOWN_UI_DEBT:
                counts["ui-debt"] += 1
                debt_lines.append(f"  {f}:{lineno} [{KNOWN_UI_DEBT[key]}]: {line.strip()}")
                continue
            if token == "Agent" and ALIAS.search(context):
                counts["alias"] += 1
                alias_lines.append(f"  {f}:{lineno}: {line.strip()}")
                continue
            if token == "Agent" and UPSTREAM.search(context):
                counts["upstream"] += 1
                continue
            if str(f) in HISTORICAL_DOCS:
                counts["historical"] += 1
                continue
            violations.append((str(f), lineno, token, line.strip()))

# --- legacy product surface (keel-bot-identity-access A5) -------------------
# The retired words are allowed only in sentences that document their removal
# ("retired", "旧…", "删除", "zero compatibility", …) or in historical plans.
# This scan covers docs, not scripts: shell/go fixtures are checked by the
# explicit grep in the A5 report because they intentionally mention the retired
# env var to refuse it.
LEGACY_PATTERNS = [
    re.compile(r"/v1/agents\b"),
    re.compile(r"/v1/teams\b"),
    re.compile(r"keel team\b(?!-bot)"),
    re.compile(r"keel agent\b"),
    re.compile(r"\bTeamAgent\b"),
    re.compile(r"\bagent_teams\b"),
    re.compile(r"KEEL_SERVE_PASSWORD"),
]
LEGACY_RETIRED = re.compile(
    r"retir|旧|\bold\b|删除|不再|zero[- ]compat|refus|ignor|legacy|archive|formerly|renam|migrat|deprecat",
    re.IGNORECASE,
)
legacy_docs = [root / "README.md"] + sorted((root / "docs").rglob("*.md")) + sorted((root / "site/docs").rglob("*.md"))
legacy_allowed = []
legacy_violations = []
for f in legacy_docs:
    if not f.is_file():
        continue
    try:
        lines = f.read_text(encoding="utf-8", errors="replace").splitlines()
    except OSError:
        continue
    for lineno, line in enumerate(lines, 1):
        # A wrap can put the retirement word on the previous line; read both.
        context = " ".join(lines[max(0, lineno - 2):lineno])
        for pattern in LEGACY_PATTERNS:
            match = pattern.search(line)
            if not match:
                continue
            if str(f) in HISTORICAL_DOCS or LEGACY_RETIRED.search(context):
                legacy_allowed.append(f"  {f}:{lineno}: {match.group(0)}: {line.strip()}")
            else:
                legacy_violations.append((str(f), lineno, match.group(0), line.strip()))
            break

if not quiet:
    print("naming-scan: scope = outward docs + user-visible strings")
    print("naming-scan: whitelist (see script header for the policy)")
    print(f"  protocol   (AgentAPI/AgentBackend)          hits: {counts['protocol']}")
    print(f"  code-ident (Agent* symbols / .Agent fields) hits: {counts['code-ident']}")
    print(f"  alias      (rename notes, audited below)    hits: {counts['alias']}")
    print(f"  upstream   (pi/pigo naming)                 hits: {counts['upstream']}")
    print(f"  ui-debt    (known fixture debt)             hits: {counts['ui-debt']}")
    print(f"  historical (historical plans)               hits: {counts['historical']}")
    if alias_lines:
        print("naming-scan: alias lines (explicit rename notes, for review):")
        for line in alias_lines:
            print(line)
    if debt_lines:
        print("naming-scan: known UI debt (none expected; printed if it returns):")
        for line in debt_lines:
            print(line)

if legacy_violations:
    print(f"naming-scan: FAIL — {len(legacy_violations)} legacy product-surface use(s):")
    for path, lineno, token, line in legacy_violations:
        print(f"  {path}:{lineno}: {token}: {line}")
    if legacy_allowed:
        print(f"naming-scan: ({len(legacy_allowed)} retirement note(s) allowed)")
    sys.exit(1)

if violations:
    print(f"naming-scan: FAIL — {len(violations)} residual product-name use(s):")
    for path, lineno, token, line in violations:
        print(f"  {path}:{lineno}: {token}: {line}")
    sys.exit(1)

if not quiet and legacy_allowed:
    print(f"naming-scan: legacy words only in {len(legacy_allowed)} retirement note(s) (allowed)")

print("naming-scan: PASS — no product-name 'Agent' outside the whitelist; no live legacy product surface")
PY
