#!/usr/bin/env python3
"""Generate the single-file E2E report for the sub-agent tree.

The report is data-driven: the run writes `run-meta.json` beside the evidence
(`shots/web/*.png`, `shots/tui/*.json`), and this script renders the TUI cell
dumps to PNG, embeds every screenshot as a data URI, and writes `report.html`.
No product state is fabricated: a check's status comes from the meta file the
acceptance run wrote, and every image is the file the run captured.

Usage:
  python3 scripts/keel-subagent-e2e-report.py --report-dir /abs/report/dir
"""
from __future__ import annotations

import argparse
import base64
import glob
import html
import json
import os
import unicodedata
from pathlib import Path

try:
    from PIL import Image, ImageDraw, ImageFont
except ImportError:  # the TUI renderer is optional; web shots still embed
    Image = None

# Cell palette. NSH's tui cell dump uses ANSI names; the run page only emits a
# small set (reset, darkgray, red, reversed/dim/bold).
FG = {
    "reset": (222, 222, 222, 255), "black": (30, 30, 30, 255),
    "red": (255, 110, 110, 255), "green": (126, 217, 87, 255),
    "yellow": (250, 200, 90, 255), "blue": (100, 170, 255, 255),
    "magenta": (220, 140, 255, 255), "cyan": (100, 220, 220, 255),
    "white": (245, 245, 245, 255), "darkgray": (120, 120, 120, 255),
    "lightred": (255, 150, 150, 255), "lightgreen": (160, 240, 120, 255),
    "lightyellow": (255, 225, 130, 255), "lightblue": (150, 200, 255, 255),
    "lightmagenta": (240, 180, 255, 255), "lightcyan": (160, 240, 240, 255),
    "gray": (160, 160, 160, 255),
}
BG = {
    "reset": (20, 20, 20, 255), "black": (10, 10, 10, 255),
    "darkgray": (58, 58, 58, 255), "red": (110, 30, 30, 255),
    "green": (30, 80, 30, 255), "yellow": (80, 70, 20, 255),
    "blue": (25, 55, 100, 255), "magenta": (70, 30, 90, 255),
    "cyan": (20, 80, 85, 255), "white": (200, 200, 200, 255),
    "gray": (90, 90, 90, 255),
}
CELL_W, CELL_H, PAD, FONT_SIZE = 9, 18, 12, 14
FONT_PATH = "/System/Library/Fonts/Hiragino Sans GB.ttc"


def color(table: dict, name: str | None, fallback: tuple) -> tuple:
    return table.get(name or "reset", fallback)


def is_wide(symbol: str) -> bool:
    return len(symbol) == 1 and unicodedata.east_asian_width(symbol) in ("W", "F")


def render_cells(path: Path, out_dir: Path) -> Path | None:
    if Image is None or not os.path.exists(FONT_PATH):
        return None
    data = json.loads(path.read_text())
    rows = data.get("rows") or []
    if not rows:
        return None
    width = len(rows[0])
    img = Image.new("RGB", (width * CELL_W + PAD * 2, len(rows) * CELL_H + PAD * 2), (20, 20, 20))
    draw = ImageDraw.Draw(img)
    font = ImageFont.truetype(FONT_PATH, FONT_SIZE)
    for y, row in enumerate(rows):
        for x, cell in enumerate(row):
            bg = color(BG, cell.get("bg"), BG["reset"])
            if "reversed" in (cell.get("modifiers") or []):
                bg = color(FG, cell.get("fg"), FG["reset"])
            draw.rectangle(
                [PAD + x * CELL_W, PAD + y * CELL_H,
                 PAD + x * CELL_W + CELL_W - 1, PAD + y * CELL_H + CELL_H - 1],
                fill=bg,
            )
    for y, row in enumerate(rows):
        x = 0
        while x < len(row):
            cell = row[x]
            fg = color(FG, cell.get("fg"), FG["reset"])
            if "reversed" in (cell.get("modifiers") or []):
                fg = color(BG, cell.get("bg"), BG["reset"])
            symbol = cell.get("symbol") or " "
            if symbol != " ":
                draw.text((PAD + x * CELL_W, PAD + y * CELL_H - 3), symbol, font=font, fill=fg)
            x += 2 if is_wide(symbol) else 1
    out_dir.mkdir(parents=True, exist_ok=True)
    out = out_dir / (path.stem + ".png")
    img.save(out)
    return out


def caption(name: str) -> str:
    """A step's caption from its evidence file name (no hidden assumptions)."""
    base = name.rsplit(".", 1)[0]
    number, _, rest = base.partition("-")
    labels = {
        "loading": "加载中（请求未返回）",
        "empty": "空态（没有可读事件）",
        "error": "错误（服务端 500 + 请求编号）",
        "permission": "无权限（会话关系缺失，结构仍可见）",
        "success-lanes": "成功（泳道树）",
        "success-timeline": "成功（390 垂直时间线）",
        "success-graph": "成功（图/拓扑视角）",
        "cancel-dialog": "取消（二次确认）",
        "cancel-stopped": "取消（停止信号与已停止状态）",
        "retry-error": "重试（节点账本读取失败）",
        "retry-done": "重试（重读后错误消失）",
        "node-actions": "子代理动作（失败节点可重试）",
        "node-retry-done": "子代理重试完成（同一会话重新排队）",
        "node-cancel-ready": "子代理取消入口（运行中节点）",
        "node-cancel-dialog": "子代理取消二次确认",
        "node-cancel-done": "子代理取消完成",
        "action-loading": "动作加载（请求未返回）",
        "action-error": "动作错误（服务端 500）",
        "action-permission": "动作无权限（缺 session.write）",
    }
    step = rest
    width = ""
    for suffix in ("-1440", "-390"):
        if rest.endswith(suffix):
            step = rest[: -len(suffix)]
            width = suffix[1:]
            break
    return f"{number} · {labels.get(step, step)} · {width}px"


def data_uri(path: Path) -> str:
    return "data:image/png;base64," + base64.b64encode(path.read_bytes()).decode()


def status_class(status: str) -> str:
    return {"PASS": "pass", "GAP": "gap", "FAIL": "fail", "PENDING": "pending"}.get(status.upper(), "pending")


def build_html(report_dir: Path, meta: dict) -> str:
    shots_web = sorted((report_dir / "shots" / "web").glob("*.png"))
    tui_png = report_dir / "shots" / "tui-png"
    tui_cells = sorted((report_dir / "shots" / "tui").glob("*.json"))
    for cell in tui_cells:
        render_cells(cell, tui_png)
    shots_tui = sorted(tui_png.glob("*.png"))
    by_name_web = {p.stem: p for p in shots_web}
    by_name_tui = {p.stem: p for p in shots_tui}
    names = sorted(set(by_name_web) | set(by_name_tui))

    def image_row(name: str) -> str:
        web = by_name_web.get(name)
        tui = by_name_tui.get(name)
        cells = []
        if web:
            cells.append(f'<figure><img src="{data_uri(web)}" alt="{html.escape(name)} web"><figcaption>web · {html.escape(caption(name))}</figcaption></figure>')
        if tui:
            cells.append(f'<figure><img src="{data_uri(tui)}" alt="{html.escape(name)} tui"><figcaption>TUI · {html.escape(caption(name))}</figcaption></figure>')
        return '<div class="shot-row">' + "".join(cells) + "</div>"

    def logs_section(logs: list[dict]) -> str:
        items = []
        for log in logs:
            path = report_dir / log["path"]
            text = path.read_text() if path.exists() else f"missing: {path}"
            items.append(
                f'<details><summary>{html.escape(log["label"])}</summary>'
                f'<pre>{html.escape(text[-12000:])}</pre></details>'
            )
        return "".join(items)

    meta = {
        "title": "keel · 子代理树 web/TUI E2E",
        "date": "",
        "branch": "",
        "commit": "",
        "main": "",
        "worktree": "",
        "public_url": None,
        "environment": [],
        "checks": [],
        "gaps": [],
        "commands": [],
        "logs": [],
        **meta,
    }
    cards = "".join(
        f'<div class="card {status_class(c["status"])}"><div class="k">{html.escape(c["title"])}</div>'
        f'<div class="v">{html.escape(c["status"])}</div><p>{html.escape(c.get("detail", ""))}</p></div>'
        for c in meta["checks"]
    )
    gaps = "".join(
        f'<li class="gap-item"><b>{html.escape(g["severity"])} · {html.escape(g["title"])}</b>'
        f'<p>{html.escape(g["detail"])}</p></li>'
        for g in meta["gaps"]
    )
    commands = "".join(
        f'<tr><td>{html.escape(c["label"])}</td><td><code>{html.escape(c["command"])}</code></td><td>{html.escape(c.get("result", ""))}</td></tr>'
        for c in meta["commands"]
    )
    env = "".join(f"<li>{html.escape(item)}</li>" for item in meta["environment"])
    public = (
        f'<a href="{html.escape(meta["public_url"])}">{html.escape(meta["public_url"])}</a>'
        if meta.get("public_url")
        else "<b>未部署公网</b>（本报告只存在于本地证据目录；没有可公开的 URL）"
    )

    return f"""<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{html.escape(meta["title"])}</title>
<style>
:root{{--bg:#0f1115;--panel:#171a21;--line:#2a2f3a;--text:#e6e8ee;--muted:#9aa3b2;--ok:#3fb950;--warn:#d29922;--bad:#f85149;--info:#58a6ff}}
*{{box-sizing:border-box}} body{{margin:0;background:var(--bg);color:var(--text);font:15px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Hiragino Sans GB",sans-serif}}
main{{max-width:1440px;margin:0 auto;padding:28px}}
h1{{font-size:26px;margin:0 0 8px}} h2{{font-size:20px;margin:34px 0 12px;border-bottom:1px solid var(--line);padding-bottom:8px}}
h3{{font-size:16px;margin:22px 0 8px}} p{{margin:6px 0;color:#cdd3df}}
.meta{{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:10px;margin:18px 0}}
.meta div{{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:10px 12px}}
.k{{color:var(--muted);font-size:12px;text-transform:uppercase;letter-spacing:.06em}} .v{{font-weight:600}}
.cards{{display:grid;grid-template-columns:repeat(auto-fit,minmax(230px,1fr));gap:12px}}
.card{{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:12px 14px}}
.card .v{{font-size:20px}} .card p{{font-size:13px;color:var(--muted)}}
.card.pass{{border-color:rgba(63,185,80,.55)}} .card.pass .v{{color:var(--ok)}}
.card.gap{{border-color:rgba(210,153,34,.65)}} .card.gap .v{{color:var(--warn)}}
.card.fail{{border-color:rgba(248,81,73,.65)}} .card.fail .v{{color:var(--bad)}}
.card.pending .v{{color:var(--info)}}
.shot-row{{display:grid;grid-template-columns:repeat(auto-fit,minmax(460px,1fr));gap:14px;margin:12px 0 22px}}
figure{{margin:0;background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:10px;overflow:hidden}}
figure img{{width:100%;height:auto;display:block;border-radius:6px;background:#000}}
figcaption{{font-size:12px;color:var(--muted);padding-top:7px}}
table{{width:100%;border-collapse:collapse;background:var(--panel);border:1px solid var(--line);border-radius:10px;overflow:hidden}}
th,td{{text-align:left;padding:9px 11px;border-bottom:1px solid var(--line);vertical-align:top;font-size:13px}}
th{{color:var(--muted);font-weight:600}} code{{background:#0b0d12;border:1px solid var(--line);border-radius:5px;padding:1px 5px;font-size:12px;color:#c9d1d9}}
pre{{background:#0b0d12;border:1px solid var(--line);border-radius:8px;padding:12px;overflow:auto;max-height:420px;font-size:12px;color:#c9d1d9}}
.gap-item{{margin:10px 0;background:var(--panel);border:1px solid rgba(210,153,34,.4);border-radius:10px;padding:10px 14px}}
.gap-item p{{margin:4px 0 0;color:var(--muted);font-size:13px}}
.note{{border-left:3px solid var(--info);background:rgba(88,166,255,.08);padding:10px 14px;border-radius:0 8px 8px 0;margin:12px 0}}
footer{{margin:40px 0 10px;color:var(--muted);font-size:12px;border-top:1px solid var(--line);padding-top:14px}}
</style></head><body><main>
<h1>{html.escape(meta["title"])}</h1>
<p>E2E 收口 · {html.escape(meta["date"])} · 分支 <code>{html.escape(meta["branch"])}</code> · commit <code>{html.escape(meta["commit"])}</code> · main <code>{html.escape(meta["main"])}</code></p>
<div class="note">{public}</div>

<h2>结论</h2>
<div class="cards">{cards}</div>
<h3>已确认的缺口（不伪造通过）</h3>
<ul>{gaps}</ul>

<h2>环境与入口</h2>
<div class="meta">
<div><div class="k">worktree</div><div class="v">{html.escape(meta["worktree"])}</div></div>
<div><div class="k">报告目录</div><div class="v">{html.escape(str(report_dir))}</div></div>
<div><div class="k">截图</div><div class="v">{len(shots_web)} web + {len(shots_tui)} TUI</div></div>
<div><div class="k">公网</div><div class="v">{"已部署" if meta.get("public_url") else "未部署"}</div></div>
</div>
<ul>{env}</ul>

<h2>执行记录</h2>
<table><thead><tr><th>检查</th><th>命令</th><th>结果</th></tr></thead><tbody>{commands}</tbody></table>

<h2>证据截图（每步）</h2>
<p>每张截图对应一次真实操作：状态稳定后由 <code>view.screenshot()</code> 抓取；web 为 PNG，TUI 为 <code>nsh-tui-cells</code> JSON，由本脚本渲染成 PNG。</p>
{''.join(image_row(name) for name in names)}

<h2>说明</h2>
<p><b>TUI 子代理树</b>：TUI 与 web 共用同一份 TSX（<code>web/src/screens/books/run.tsx</code>），
<code>nsh test --target tui</code> 可打开运行页、切换图、取消与重试；本次 TUI 截图即该实现。</p>
<p><b>深度</b>：生产记录携带 <code>depth</code>/<code>maxDepth</code>（main=0，默认 3，-1 不限），
但当前运行页只在 <code>node.subtitle</code> 里生成深度文案，而该字段只在节点没有父级时显示；
所有 spawn 子节点都有父级，因此 depth 标签实际不可见。截图中的树只体现父子缩进，不体现数字深度。</p>
<p><b>取消/重试</b>：取消是运行级停止（<code>POST /v1/sessions/{id}/cancel</code>，服务端 CancelSubtree），
重试是节点账本读取重试；两者都不是“对单个子代理重新 spawn”。子代理级
<code>POST /v1/subagents/{id}/cancel</code> 已存在但客户端没有按钮。</p>

<h2>原始日志</h2>
{logs_section(meta["logs"])}
<footer>Generated by scripts/keel-subagent-e2e-report.py · 数据源：run-meta.json + 证据截图 · 不含 DSN/密钥</footer>
</main></body></html>
"""


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--report-dir", required=True)
    args = parser.parse_args()
    report_dir = Path(args.report_dir).resolve()
    meta_path = report_dir / "run-meta.json"
    meta = json.loads(meta_path.read_text()) if meta_path.exists() else {}
    out = report_dir / "report.html"
    out.write_text(build_html(report_dir, meta))
    print(out)


if __name__ == "__main__":
    main()
