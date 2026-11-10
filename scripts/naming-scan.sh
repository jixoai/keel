#!/usr/bin/env bash
# naming-scan.sh — Harness 命名纪律扫描（keel-harness-naming N1/N5）
#
# 对外文件不得再用「Agent」指代产品层；产品层统一 **Harness**（原 Agent）。
# 白名单只有四类，且每类都在输出里如实标注：
#   1. protocol       —— 协议名 AgentAPI / AgentBackend。它们是契约（不改名，改名破坏
#                        客户端与适配器），保留；文档在首次引入它们的页面（README、
#                        docs/HARNESS.md、术语表）标注「Harness（原 Agent）」别名。
#   2. code-ident      —— 仓库 Go/TS 源码中真实存在的 Agent* 标识符，以及以 `.` 引用的
#                        结构字段路径（如 `TenantConfig.Agent`）。文档引用代码符号是在
#                        引用代码事实，保留。裸词「Agent」「Agents」不在此列。
#   3. alias          —— 专门说明改名的句子（「原 Agent」「旧称」「更名」「取代」
#                        「别名」「formerly」「earlier」「rename」）；这类行会逐条打印，便于人工复核。
#   4. upstream       —— 上游命名：pi/pigo 的「Agent Skills 规范」、agentskills.io、
#                        「pi Agent」（描述 pigo 时的上游事实说法）。
#
# 代码文件（cmd/keel/serve.go、web/src/data/fixture.ts）只扫描字符串字面量：内部字段名/注释
# 不是对外文案，只改字面量。
#
# 其余任何 capital-A `Agent` 命中即违规。脚本以非零码退出并打印 file:line: token。
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
ALIAS = re.compile(r"原 Agent|旧称|原名|别名|取代|更名|rename|formerly|earlier|alias")
# --- code files are scanned inside string literals only ---------------------
QUOTED = re.compile(r'"(?:[^"\\]|\\.)*"' + r"|'(?:[^'\\]|\\.)*'" + r"|`[^`]*`")
CODE_FILES = {".go", ".ts", ".tsx"}

TOKEN = re.compile(r"(?<![A-Za-z0-9_])Agent[A-Za-z0-9_]*")
counts = {"protocol": 0, "code-ident": 0, "alias": 0, "upstream": 0}
alias_lines = []
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
            if token == "Agent" and ALIAS.search(context):
                counts["alias"] += 1
                alias_lines.append(f"  {f}:{lineno}: {line.strip()}")
                continue
            if token == "Agent" and UPSTREAM.search(context):
                counts["upstream"] += 1
                continue
            violations.append((str(f), lineno, token, line.strip()))

if not quiet:
    print("naming-scan: scope = outward docs + user-visible strings")
    print("naming-scan: whitelist (see script header for the policy)")
    print(f"  protocol   (AgentAPI/AgentBackend)          hits: {counts['protocol']}")
    print(f"  code-ident (Agent* symbols / .Agent fields) hits: {counts['code-ident']}")
    print(f"  alias      (rename notes, audited below)    hits: {counts['alias']}")
    print(f"  upstream   (pi/pigo naming)                 hits: {counts['upstream']}")
    if alias_lines:
        print("naming-scan: alias lines (explicit rename notes, for review):")
        for line in alias_lines:
            print(line)

if violations:
    print(f"naming-scan: FAIL — {len(violations)} residual product-name use(s):")
    for path, lineno, token, line in violations:
        print(f"  {path}:{lineno}: {token}: {line}")
    sys.exit(1)

print("naming-scan: PASS — no product-name 'Agent' outside the whitelist")
PY
