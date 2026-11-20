#!/usr/bin/env python3
"""Render the keel-ga G5 acceptance report as one self-contained HTML file.

Inputs are the JSON verdicts written by scripts/acceptance-g5-perf.sh and
scripts/acceptance-g5-chaos.sh plus their raw logs. The report is fail-closed:
a missing or FAIL verdict renders a FAIL banner and exits non-zero.
"""
from __future__ import annotations

import hashlib
import html
import json
import os
import sys
from datetime import datetime
from pathlib import Path

PUBLIC_URL = "https://local-gaubee-reports.fde2e.com/keel/keel-ga-g5/report.html"


def esc(value) -> str:
    return html.escape("" if value is None else str(value))


def load_json(path: Path):
    try:
        with path.open(encoding="utf-8") as fh:
            return json.load(fh)
    except Exception:
        return None


def tail(path: Path, lines: int = 40) -> str:
    try:
        text = path.read_text(encoding="utf-8", errors="replace").splitlines()
    except Exception:
        return "(missing)"
    return "\n".join(text[-lines:]) or "(empty)"


def num(value, digits=1, suffix=""):
    if value is None:
        return "n/a"
    try:
        return f"{float(value):,.{digits}f}{suffix}"
    except (TypeError, ValueError):
        return esc(value)


def meter(label: str, value, limit, unit: str = "MB") -> str:
    try:
        v = float(value)
        l = float(limit)
    except (TypeError, ValueError):
        return ""
    pct = 0.0 if l <= 0 else min(100.0, max(0.0, v / l * 100.0))
    tone = "good" if pct < 75 else ("warn" if pct < 95 else "bad")
    return f"""
    <div class="meter">
      <div class="meter-head"><span>{esc(label)}</span><strong>{num(v, 0, unit)} / {num(l, 0, unit)}</strong></div>
      <div class="meter-track"><div class="meter-fill {tone}" style="width:{pct:.2f}%"></div></div>
      <div class="meter-foot">{pct:.1f}% of the limit</div>
    </div>"""


def card(label: str, value, note: str = "", tone: str = "") -> str:
    return f"""
    <div class="card {tone}">
      <div class="card-label">{esc(label)}</div>
      <div class="card-value">{esc(value)}</div>
      <div class="card-note">{esc(note)}</div>
    </div>"""


def badge(ok: bool, text: str) -> str:
    return f'<span class="badge {"pass" if ok else "fail"}">{esc(text)}</span>'


def kill_timeline(kills) -> str:
    times = []
    for kill in kills:
        raw = (kill or {}).get("at") or ""
        try:
            times.append(datetime.fromisoformat(raw.replace("Z", "+00:00")).timestamp())
        except Exception:
            continue
    if len(times) < 2:
        return '<p class="muted">no kill timestamps available</p>'
    start, end = times[0], times[-1]
    span = max(end - start, 1.0)
    width, height, margin = 940, 120, 40
    points = []
    for ts in times:
        x = margin + (ts - start) / span * (width - 2 * margin)
        points.append((x, height / 2))
    circles = "".join(
        f'<circle cx="{x:.1f}" cy="{y:.1f}" r="5" class="kill-dot"><title>{datetime.fromtimestamp(ts).isoformat()}</title></circle>'
        for (x, y), ts in zip(points, times)
    )
    labels = (
        f'<text x="{margin}" y="{height - 12}" class="axis">{datetime.fromtimestamp(start).strftime("%H:%M:%S")}</text>'
        f'<text x="{width - margin}" y="{height - 12}" text-anchor="end" class="axis">{datetime.fromtimestamp(end).strftime("%H:%M:%S")}</text>'
    )
    return f"""
    <svg viewBox="0 0 {width} {height}" role="img" aria-label="SIGKILL timeline">
      <line x1="{margin}" y1="{height/2}" x2="{width-margin}" y2="{height/2}" class="axis-line"/>
      {circles}
      {labels}
    </svg>
    <p class="muted">Each dot is a SIGKILL; the axis spans the measured run ({span:.0f}s).</p>"""


def rows(pairs) -> str:
    return "".join(
        f"<tr><th>{esc(k)}</th><td>{v if isinstance(v, str) and v.startswith('<') else esc(v)}</td></tr>"
        for k, v in pairs
    )


def main() -> int:
    if len(sys.argv) < 3:
        print("usage: g5-report.py <evidence-dir> <output.html> [--commit SHA]", file=sys.stderr)
        return 2
    evidence = Path(sys.argv[1]).resolve()
    output = Path(sys.argv[2]).resolve()
    commit = ""
    if "--commit" in sys.argv:
        commit = sys.argv[sys.argv.index("--commit") + 1]

    perf = load_json(evidence / "perf" / "result.json")
    chaos = load_json(evidence / "chaos" / "result.json")
    meta = load_json(evidence / "meta.json") or {}

    perf_ok = bool(perf and perf.get("verdict") == "PASS")
    chaos_ok = bool(chaos and chaos.get("verdict") == "PASS" and chaos.get("pass"))
    overall_ok = perf_ok and chaos_ok

    perf_failures = (perf or {}).get("failures") or []
    chaos_failures = (chaos or {}).get("failures") or []
    counters = (chaos or {}).get("invariantCounters") or {}
    peak_mb = (perf or {}).get("peakMB")
    peak_pct = (perf or {}).get("peakPctOfLimit")
    peak_bytes = (perf or {}).get("peakBytes")
    cgroup_max_bytes = (perf or {}).get("cgroupMaxBytes")
    expected_sessions = (perf or {}).get("expectedSessions", 1000)
    expected_runs = (perf or {}).get("expectedRuns", 64)
    sessions = (perf or {}).get("sessions")
    sse_sessions = (perf or {}).get("sseSessions")
    streams_max = (perf or {}).get("streamsActiveMax")
    runs_max = (perf or {}).get("runsActiveMax")
    runs_done = (perf or {}).get("runsFinishedDone")
    runs_failed = (perf or {}).get("runsFailedFinal")
    runs_pending = (perf or {}).get("runsPendingFinal")
    peak_limit_mb = (perf or {}).get("hardLimitMB", 2048)
    ga_limit_mb = (perf or {}).get("ceilingMB", 1800)
    faults = (chaos or {}).get("faults")
    restarts = (chaos or {}).get("restarts")
    expected_kills = (chaos or {}).get("expectedKills")
    event_count = (chaos or {}).get("eventCount")
    mean_interval = (chaos or {}).get("killIntervalMeanSec")

    missing = []
    if perf is None:
        missing.append("perf/result.json")
    if chaos is None:
        missing.append("chaos/result.json")

    gate_rows = [
        ("1000 会话在线 + 64 并发 run", badge(perf_ok, "PASS" if perf_ok else "FAIL"),
         f"sessions={sessions}/{expected_sessions}, SSE established={sse_sessions}, streamsActiveMax={streams_max}, runsActiveMax={runs_max}/{expected_runs}, runStatus.done={runs_done}, failed={runs_failed}, pending={runs_pending}"),
        ("cgroup 内存峰值 < 2GB（GA 目标 < 1.8GB）", badge(peak_mb is not None and float(peak_mb) < float(peak_limit_mb), "PASS" if peak_mb is not None and float(peak_mb) < float(peak_limit_mb) else "FAIL"),
         f"memory.peak={num(peak_mb, 1, ' MB')} ({num(peak_pct, 2, '%')} of 2048 MB); GA ceiling {ga_limit_mb} MB"),
        ("2GB cgroup 复跑（与 1000/64 合并，写清口径）", badge(cgroup_max_bytes == 2147483648 and peak_bytes is not None, "PASS" if cgroup_max_bytes == 2147483648 and peak_bytes is not None else "FAIL"),
         f"memory.max={cgroup_max_bytes} bytes; memory.peak={peak_bytes} bytes; 单次运行同时承载 1000 SSE + 64 run + 1MiB 图片"),
        ("10 分钟随机 SIGKILL（每 30s）", badge(chaos_ok, "PASS" if chaos_ok else "FAIL"),
         f"faults={faults}/{expected_kills}, restarts={restarts}, mean interval={num(mean_interval, 2, 's')}, events audited={event_count}"),
        ("0 丢事件 / 0 重复最终回复", badge(chaos_ok and all(int(counters.get(k) or 0) == 0 for k in (
            "seqGaps", "duplicateRequestIDs", "duplicateFinalReplies", "unansweredNewest",
            "orphanToolResults", "unreadAfterSettle", "stuckRuns", "runStatusErrors", "eventLoadErrors")),
            "PASS" if chaos_ok else "FAIL"),
         f"seqGaps={counters.get('seqGaps')}, duplicateFinalReplies={counters.get('duplicateFinalReplies')}, "
         f"duplicateRequestIDs={counters.get('duplicateRequestIDs')}, unansweredNewest={counters.get('unansweredNewest')}, "
         f"orphanToolResults={counters.get('orphanToolResults')}, stuckRuns={counters.get('stuckRuns')}"),
    ]

    gate_table = "".join(
        f"<tr><td>{name}</td><td>{status}</td><td>{detail}</td></tr>"
        for name, status, detail in gate_rows
    )

    perf_cards = "".join([
        card("1000 会话 SSE", f"{sse_sessions} established", f"{sessions} created, streamsMax={streams_max}"),
        card("并发 run", f"{runs_max} / {expected_runs}", f"done={runs_done}, failed={runs_failed}, pending={runs_pending}"),
        card("cgroup 峰值", num(peak_mb, 1, " MB"), f"{num(peak_pct, 2, '%')} of 2GB", "good" if peak_mb is not None and float(peak_mb) < 1800 else "warn"),
        card("SSE 硬错误", (perf or {}).get("hardSSEErrors", "n/a"), f"429 retried={((perf or {}).get('statusCounts') or {}).get('429', 'n/a')}"),
        card("图片通道", (perf or {}).get("imagesOK", "n/a"), f"errors={((perf or {}).get('imagesErr', 'n/a'))}"),
        card("吞吐", num((perf or {}).get("throughputMBs"), 2, " MB/s"), f"duration={num((perf or {}).get('durationSec'), 1, 's')}"),
    ])

    chaos_cards = "".join([
        card("SIGKILL 次数", faults if faults is not None else "n/a", f"expected {expected_kills}", "good" if chaos_ok else "bad"),
        card("重启次数", restarts if restarts is not None else "n/a", "one relaunch per fault"),
        card("kill 平均间隔", num(mean_interval, 2, "s"), f"min={num((chaos or {}).get('killIntervalMinSec'), 2, 's')} max={num((chaos or {}).get('killIntervalMaxSec'), 2, 's')}"),
        card("审计事件", event_count if event_count is not None else "n/a", f"sessions={ (chaos or {}).get('sessionsChecked') }"),
        card("丢事件", counters.get("seqGaps", "n/a"), "seq contiguous from 1"),
        card("重复最终回复", counters.get("duplicateFinalReplies", "n/a"), "newest input answered once"),
    ])

    kill_rows = ""
    kills = (chaos or {}).get("kills") or []
    prev = None
    for index, kill in enumerate(kills, 1):
        stamp = (kill or {}).get("at") or ""
        delta = ""
        try:
            ts = datetime.fromisoformat(stamp.replace("Z", "+00:00")).timestamp()
            if prev is not None:
                delta = f"{ts - prev:.2f}s"
            prev = ts
        except Exception:
            pass
        kill_rows += (
            f"<tr><td>{index}</td><td>{esc(stamp)}</td>"
            f"<td>{esc((kill or {}).get('runtime'))}</td>"
            f"<td>{esc((kill or {}).get('pid'))}</td><td>{esc(delta)}</td></tr>"
        )
    if not kill_rows:
        kill_rows = '<tr><td colspan="5">no kill records</td></tr>'

    checks = (chaos or {}).get("checks") or []
    check_items = "".join(f"<li>{esc(item)}</li>" for item in checks) or "<li>(none)</li>"

    failures_html = ""
    for item in perf_failures:
        failures_html += f'<li class="fail-text">perf: {esc(item)}</li>'
    for item in chaos_failures:
        failures_html += f'<li class="fail-text">chaos: {esc(item)}</li>'
    for item in missing:
        failures_html += f'<li class="fail-text">missing: {esc(item)}</li>'
    if not failures_html:
        failures_html = '<li class="pass-text">No assertion failures.</li>'

    evidence_rows = ""
    for path in sorted(evidence.rglob("*")):
        if not path.is_file():
            continue
        rel = path.relative_to(evidence)
        size = path.stat().st_size
        digest = ""
        try:
            digest = hashlib.sha256(path.read_bytes()).hexdigest()[:16]
        except Exception:
            pass
        evidence_rows += f"<tr><td>{esc(rel)}</td><td>{size:,}</td><td><code>{digest}</code></td></tr>"

    log_sections = ""
    chaos_runtime_log = evidence / "chaos" / "runtime-tail.log"
    if not chaos_runtime_log.exists():
        chaos_runtime_log = evidence / "chaos" / "runtime.log"
    for label, path in [
        ("perf / loadtest.log", evidence / "perf" / "loadtest.log"),
        ("perf / runtime.log", evidence / "perf" / "runtime.log"),
        ("chaos / chaos.log", evidence / "chaos" / "chaos.log"),
        ("chaos / runtime-tail.log", chaos_runtime_log),
    ]:
        log_sections += f"""
        <details>
          <summary>{esc(label)} <span class="muted">(tail 40)</span></summary>
          <pre>{esc(tail(path))}</pre>
        </details>"""

    cleanup_sections = ""
    for label, path in [
        ("perf cleanup", evidence / "perf" / "cleanup.txt"),
        ("chaos cleanup", evidence / "chaos" / "cleanup.txt"),
    ]:
        cleanup_sections += f"<h3>{esc(label)}</h3><pre>{esc(tail(path, 30))}</pre>"

    generated = datetime.now().astimezone().strftime("%Y-%m-%d %H:%M:%S %Z")
    commit_label = commit or meta.get("commit") or "(unknown)"
    dirty = meta.get("dirty")
    verdict_text = "PASS" if overall_ok else "FAIL"
    verdict_class = "pass" if overall_ok else "fail"

    document = f"""<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Keel G5 验收报告 — 1000 会话 / 64 run / 2GB / SIGKILL</title>
<style>
  :root {{
    --bg:#0b1220; --panel:#111c2e; --panel2:#152238; --text:#e6edf7; --muted:#93a4bd;
    --line:#24344d; --pass:#2ecc71; --fail:#ff5d5d; --warn:#f5b041; --good:#2ecc71; --bad:#ff5d5d;
    --accent:#5dade2;
  }}
  * {{ box-sizing:border-box; }}
  body {{ margin:0; background:var(--bg); color:var(--text); font:15px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif; }}
  main {{ max-width:1180px; margin:0 auto; padding:32px 20px 64px; }}
  h1 {{ font-size:30px; margin:0 0 6px; letter-spacing:.2px; }}
  h2 {{ font-size:21px; margin:36px 0 12px; padding-bottom:8px; border-bottom:1px solid var(--line); }}
  h3 {{ font-size:16px; margin:22px 0 8px; color:#c9d8ee; }}
  p {{ margin:8px 0; }}
  .sub {{ color:var(--muted); margin:0 0 18px; }}
  .banner {{ display:flex; align-items:center; gap:16px; padding:18px 22px; border-radius:14px; background:linear-gradient(135deg,#12233a,#0e1a2b); border:1px solid var(--line); }}
  .banner .verdict {{ font-size:28px; font-weight:800; letter-spacing:1px; }}
  .pass {{ color:var(--pass); }} .fail {{ color:var(--fail); }} .warn {{ color:var(--warn); }}
  .badge {{ display:inline-block; padding:2px 10px; border-radius:999px; font-size:12px; font-weight:700; border:1px solid; }}
  .badge.pass {{ border-color:var(--pass); background:rgba(46,204,113,.12); }}
  .badge.fail {{ border-color:var(--fail); background:rgba(255,93,93,.12); }}
  .cards {{ display:grid; grid-template-columns:repeat(auto-fit,minmax(170px,1fr)); gap:12px; margin:18px 0 6px; }}
  .card {{ background:var(--panel); border:1px solid var(--line); border-radius:12px; padding:14px 16px; }}
  .card.good {{ border-color:rgba(46,204,113,.5); }}
  .card.bad {{ border-color:rgba(255,93,93,.6); }}
  .card.warn {{ border-color:rgba(245,176,65,.6); }}
  .card-label {{ color:var(--muted); font-size:12px; text-transform:uppercase; letter-spacing:.6px; }}
  .card-value {{ font-size:26px; font-weight:750; margin:6px 0 2px; }}
  .card-note {{ color:var(--muted); font-size:12px; }}
  table {{ width:100%; border-collapse:collapse; background:var(--panel); border:1px solid var(--line); border-radius:12px; overflow:hidden; }}
  th, td {{ text-align:left; padding:10px 12px; border-bottom:1px solid var(--line); vertical-align:top; }}
  th {{ color:#c9d8ee; font-weight:650; background:var(--panel2); }}
  tr:last-child td {{ border-bottom:none; }}
  code {{ font-family:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace; font-size:12.5px; }}
  pre {{ background:#0a1322; border:1px solid var(--line); border-radius:10px; padding:12px 14px; overflow:auto; max-height:420px; font-size:12px; line-height:1.5; }}
  details {{ background:var(--panel); border:1px solid var(--line); border-radius:10px; padding:10px 14px; margin:10px 0; }}
  summary {{ cursor:pointer; font-weight:600; }}
  .muted {{ color:var(--muted); font-size:12px; }}
  .fail-text {{ color:#ffb3b3; }}
  .pass-text {{ color:#9fe8bb; }}
  .meter {{ margin:14px 0; }}
  .meter-head {{ display:flex; justify-content:space-between; font-size:13px; margin-bottom:6px; }}
  .meter-track {{ height:12px; background:#0a1322; border:1px solid var(--line); border-radius:999px; overflow:hidden; }}
  .meter-fill {{ height:100%; border-radius:999px; }}
  .meter-fill.good {{ background:linear-gradient(90deg,#27ae60,#2ecc71); }}
  .meter-fill.warn {{ background:linear-gradient(90deg,#d68910,#f5b041); }}
  .meter-fill.bad {{ background:linear-gradient(90deg,#c0392b,#ff5d5d); }}
  .meter-foot {{ color:var(--muted); font-size:11px; margin-top:4px; }}
  .kill-dot {{ fill:var(--fail); opacity:.9; }}
  .axis-line {{ stroke:#3a4d68; stroke-width:2; }}
  .axis {{ fill:var(--muted); font-size:12px; }}
  .grid2 {{ display:grid; grid-template-columns:1fr 1fr; gap:18px; }}
  @media (max-width:820px) {{ .grid2 {{ grid-template-columns:1fr; }} }}
  footer {{ margin-top:40px; color:var(--muted); font-size:12px; border-top:1px solid var(--line); padding-top:14px; }}
</style>
</head>
<body>
<main>
  <h1>Keel G5 规模与混沌验收</h1>
  <p class="sub">keel-ga §6 · 1000 会话在线 + 64 并发 run · 2GB cgroup · 10 分钟 SIGKILL 混沌</p>

  <div class="banner">
    <div class="verdict {verdict_class}">{verdict_text}</div>
    <div>
      <div><strong>Commit</strong> <code>{esc(commit_label)}</code>{' <span class="warn">(dirty worktree)</span>' if dirty else ''}</div>
      <div class="muted">Generated {esc(generated)} · evidence <code>{esc(evidence)}</code></div>
      <div class="muted">Public report: <a href="{PUBLIC_URL}" style="color:var(--accent)">{PUBLIC_URL}</a></div>
    </div>
  </div>

  <h2>门槛与实测</h2>
  <table>
    <thead><tr><th>门槛</th><th>判定</th><th>实测</th></tr></thead>
    <tbody>{gate_table}</tbody>
  </table>

  <h2>规模与 2GB cgroup（同一次运行）</h2>
  <div class="cards">{perf_cards}</div>
  {meter("cgroup memory.peak / 2048 MB hard limit", peak_mb, peak_limit_mb)}
  {meter("cgroup memory.peak / 1800 MB GA ceiling", peak_mb, ga_limit_mb)}
  <p class="muted">口径：单个 real runtime 容器（<code>--memory=2g --memory-swap=2g</code>）同时承载 1000 条 SSE、64 个并发 run（fake provider 固定 30s 延迟）与 1MiB 图片下载；loadtest 客户端在独立容器、共享 netns，不计入 runtime cgroup。峰值取 cgroup v2 <code>memory.peak</code>，不是事后 RSS 采样。</p>

  <div class="grid2">
    <div>
      <h3>Run 收敛</h3>
      <table><tbody>{rows([
        ("runsActiveMax", f"{runs_max} / {expected_runs}"),
        ("runsStartedFinal", (perf or {}).get("runsStartedFinal")),
        ("runsFinishedDone", runs_done),
        ("runsFailedFinal", runs_failed),
        ("runsPendingFinal", runs_pending),
        ("runStatusCounts", json.dumps((perf or {}).get("runStatusCounts") or {}, ensure_ascii=False)),
      ])}</tbody></table>
    </div>
    <div>
      <h3>SSE / 图片</h3>
      <table><tbody>{rows([
        ("sessions", sessions),
        ("streamsActiveMax", streams_max),
        ("durationSec", num((perf or {}).get("durationSec"), 1, "s")),
        ("texts", (perf or {}).get("texts")),
        ("imagesOK / imagesErr", f"{(perf or {}).get('imagesOK')} / {(perf or {}).get('imagesErr')}"),
        ("sseErrors / 429", f"{(perf or {}).get('sseErrors')} / {((perf or {}).get('statusCounts') or {}).get('429')}"),
        ("imageLatency p50/p95/p99", f"{num((perf or {}).get('imageLatencyP50MS'), 1, 'ms')} / {num((perf or {}).get('imageLatencyP95MS'), 1, 'ms')} / {num((perf or {}).get('imageLatencyP99MS'), 1, 'ms')}"),
      ])}</tbody></table>
    </div>
  </div>

  <h2>混沌：SIGKILL 每 30s × 10min</h2>
  <div class="cards">{chaos_cards}</div>
  {kill_timeline(kills)}
  <table>
    <thead><tr><th>#</th><th>时间 (UTC)</th><th>runtime</th><th>pid</th><th>距上次</th></tr></thead>
    <tbody>{kill_rows}</tbody>
  </table>

  <h3>不变量计数器</h3>
  <table><tbody>{rows([
    ("eventCount", event_count),
    ("sessionsChecked", (chaos or {}).get("sessionsChecked")),
    ("seqGaps（丢事件）", counters.get("seqGaps")),
    ("duplicateRequestIDs", counters.get("duplicateRequestIDs")),
    ("duplicateFinalReplies（重复最终回复）", counters.get("duplicateFinalReplies")),
    ("unansweredNewest", counters.get("unansweredNewest")),
    ("orphanToolResults", counters.get("orphanToolResults")),
    ("unreadAfterSettle", counters.get("unreadAfterSettle")),
    ("stuckRuns", counters.get("stuckRuns")),
    ("runStatusErrors", counters.get("runStatusErrors")),
    ("eventLoadErrors", counters.get("eventLoadErrors")),
  ])}</tbody></table>

  <h3>Harness 自检项</h3>
  <ul>{check_items}</ul>

  <h2>断言失败</h2>
  <ul>{failures_html}</ul>

  <h2>方法与口径</h2>
  <ul>
    <li><strong>规模与 2GB 合并</strong>：同一次运行同时覆盖 1000 SSE、64 并发 run、周期 1MiB 图片；2GB cgroup 复跑不再独立跑第二遍，避免“分开都能过、一起过不了”的假阳性。</li>
    <li><strong>64 并发如何保证</strong>：64 个会话在 t=0 预填一条 input；runtime <code>-runs 64</code>，fake provider 每次固定 30s 延迟，保证 64 个 run 同时在执行。metrics <code>keel_runs_active</code> 每 250ms 采样，最大值必须恰为 64；随后队列排空、64 个 run 行全部 done。</li>
    <li><strong>1000 会话在线</strong>：loadtest 创建 1000 个会话并保持 SSE；<code>keel_streams_active</code> 峰值必须 ≥1000。其余 936 个会话不发文本，避免把 1000 会话压测变成无界 run 洪泛。</li>
    <li><strong>SIGKILL 频率</strong>：<code>cmd/chaos -mode kill -kill-every 30s -duration 600s</code>，每次 SIGKILL 后立即重启并计数；结果 JSON 记录每个 kill 的时间戳/pid，平均值、最小值、最大值都写入报告。</li>
    <li><strong>0 丢事件 / 0 重复回复</strong>：chaos 在静默期后读取每个会话的完整事件日志，校验 seq 从 1 连续、requestId 恰好出现一次、每个 input 至多一条最终回复、最新 input 必有一条最终回复、无孤儿 tool result、read watermark 覆盖最新输入、run 状态收敛。</li>
    <li><strong>环境</strong>：macOS {esc(meta.get('host') or '')}；Go {esc(meta.get('goVersion') or '')}；Docker {esc(meta.get('dockerVersion') or '')}；PostgreSQL {esc(meta.get('pgVersion') or '')}。混沌 runtime 跑在宿主进程（不包 cgroup），规模 runtime 跑在 2GB 容器；两者都不使用真实 provider 凭据。</li>
    <li><strong>清理</strong>：专用数据库 <code>keel_test_ga_g5</code> 与 <code>keel_test_ga_g5_chaos</code> 在退出时删除，容器/端口/孤儿进程回收结果见下方 cleanup 证据。</li>
  </ul>

  <h2>证据文件</h2>
  <table>
    <thead><tr><th>文件</th><th>字节</th><th>sha256[:16]</th></tr></thead>
    <tbody>{evidence_rows}</tbody>
  </table>

  <h2>日志摘要</h2>
  {log_sections}

  <h2>清理证据</h2>
  {cleanup_sections}

  <footer>
    keel-ga G5 · generated by <code>scripts/g5-report.py</code> · 本文件自包含（无外部脚本/图片/字体）。
  </footer>
</main>
</body>
</html>
"""
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(document, encoding="utf-8")
    print(f"report: {output}")
    print(f"verdict: {verdict_text}")
    if not overall_ok:
        print("failures:")
        for item in perf_failures:
            print(f"  perf: {item}")
        for item in chaos_failures:
            print(f"  chaos: {item}")
        for item in missing:
            print(f"  missing: {item}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
