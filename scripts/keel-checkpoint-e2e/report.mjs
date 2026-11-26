#!/usr/bin/env node
// Render steps.json + shots/*.png into a self-contained HTML report.
//
//   node report.mjs <run-dir>
//
// The report is local evidence, not a deployment: when E2E_PUBLIC_URL is
// empty it says so instead of pretending a public link exists.
import fs from 'node:fs';
import path from 'node:path';

const DIR = process.argv[2];
if (!DIR) {
  console.error('usage: report.mjs <run-dir>');
  process.exit(2);
}
const stepsFile = path.join(DIR, 'steps.json');
if (!fs.existsSync(stepsFile)) {
  console.error(`missing ${stepsFile}`);
  process.exit(2);
}
const report = JSON.parse(fs.readFileSync(stepsFile, 'utf8'));
const cleanupFile = path.join(DIR, 'cleanup.json');
const cleanup = fs.existsSync(cleanupFile) ? JSON.parse(fs.readFileSync(cleanupFile, 'utf8')) : null;
const tuiFile = path.join(DIR, 'tui-test.log');
const tuiLog = fs.existsSync(tuiFile) ? fs.readFileSync(tuiFile, 'utf8') : '';

const esc = (value) =>
  String(value ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
const checkCount = report.steps.reduce((sum, step) => sum + (step.checks?.length || 0), 0);
const failedChecks = report.steps.reduce((sum, step) => sum + (step.checks || []).filter((check) => !check.ok).length, 0);
const shotCount = report.steps.reduce((sum, step) => sum + (step.screenshots?.length || 0), 0);
const publicNote = report.publicUrl
  ? `<a href="${esc(report.publicUrl)}">${esc(report.publicUrl)}</a>`
  : '<strong>未部署</strong>：本报告只来自本地 <code>127.0.0.1</code> 真实栈，没有公网链接。';

const statusBadge = (status) => {
  const passed = status === 'passed';
  return `<span class="badge ${passed ? 'ok' : 'bad'}">${passed ? 'PASS' : 'FAIL'}</span>`;
};

const checksTable = (checks) => {
  if (!checks?.length) return '<p class="muted">没有断言。</p>';
  return `<table><thead><tr><th>断言</th><th>结果</th><th>实际</th><th>期望</th></tr></thead><tbody>
${checks
  .map(
    (check) => `<tr class="${check.ok ? '' : 'failed'}">
  <td>${esc(check.label)}</td>
  <td>${check.ok ? 'PASS' : 'FAIL'}</td>
  <td><code>${esc(check.actual ?? '')}</code></td>
  <td>${check.expected === undefined ? '' : `<code>${esc(JSON.stringify(check.expected))}</code>`}</td>
</tr>`,
  )
  .join('\n')}
</tbody></table>`;
};

const screenshots = (files) => {
  if (!files?.length) return '<p class="muted">没有截图。</p>';
  return `<div class="shots">${files
    .map(
      (file) => `<figure><a href="${esc(file)}"><img src="${esc(file)}" loading="lazy" alt="${esc(file)}"></a><figcaption>${esc(file)}</figcaption></figure>`,
    )
    .join('\n')}</div>`;
};

const details = (value) => {
  if (!value || Object.keys(value).length === 0) return '';
  return `<details><summary>结构化细节</summary><pre>${esc(JSON.stringify(value, null, 2))}</pre></details>`;
};

const stepSections = report.steps
  .map(
    (step, index) => `<section class="step" id="step-${esc(step.id)}">
  <h2>${String(index + 1).padStart(2, '0')} · ${esc(step.title)} ${statusBadge(step.status)}</h2>
  <p class="meta">${esc(step.id)} · ${step.durationMs ?? 0} ms · ${step.checks?.length || 0} 条断言 · ${step.screenshots?.length || 0} 张图</p>
  ${step.error ? `<pre class="error">${esc(step.error)}</pre>` : ''}
  ${checksTable(step.checks)}
  ${screenshots(step.screenshots)}
  ${details(step.details)}
</section>`,
  )
  .join('\n');

const environmentRows = Object.entries(report.environment || {})
  .map(([key, value]) => `<tr><th>${esc(key)}</th><td>${esc(value)}</td></tr>`)
  .join('\n');

const consoleBlock = report.console?.length
  ? `<ul>${report.console.map((entry) => `<li><code>${esc(entry.step)}</code> ${esc(entry.text)}</li>`).join('')}</ul>`
  : '<p class="muted">无 console error。</p>';
const pageErrorBlock = report.pageErrors?.length
  ? `<ul>${report.pageErrors.map((entry) => `<li><code>${esc(entry.step)}</code><pre>${esc(entry.error)}</pre></li>`).join('')}</ul>`
  : '<p class="muted">无 page error。</p>';
const requestBlock = report.failedRequests?.length
  ? `<ul>${report.failedRequests
      .map((entry) => `<li><code>${esc(entry.step)}</code> ${esc(entry.url)} — ${esc(entry.error || '')}</li>`)
      .join('')}</ul>`
  : '<p class="muted">无失败请求（favicon 除外）。</p>';

const html = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>keel-checkpoint E2E — ${esc(report.startedAt)}</title>
<style>
:root { color-scheme: light dark; --bg:#f7f8fa; --card:#fff; --ink:#16181d; --muted:#626b7a; --line:#e3e6ec; --ok:#0a7a3d; --bad:#b42318; --warn:#8a5a00; --accent:#0b5ed7; }
@media (prefers-color-scheme: dark) { :root { --bg:#111318; --card:#181b22; --ink:#e8ebf1; --muted:#9aa4b2; --line:#2a2f39; --ok:#4ade80; --bad:#f87171; --warn:#fbbf24; --accent:#60a5fa; } }
* { box-sizing: border-box; }
body { margin:0; padding:28px; background:var(--bg); color:var(--ink); font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans SC",sans-serif; }
main { max-width:1180px; margin:0 auto; }
h1 { font-size:24px; margin:0 0 4px; }
h2 { font-size:17px; margin:0 0 6px; display:flex; gap:10px; align-items:center; flex-wrap:wrap; }
p { margin:8px 0; }
.muted { color:var(--muted); }
.meta { color:var(--muted); font-size:12px; }
.badge { display:inline-block; padding:2px 8px; border-radius:999px; font-size:11px; font-weight:700; letter-spacing:.04em; }
.badge.ok { background:rgba(10,122,61,.12); color:var(--ok); }
.badge.bad { background:rgba(180,35,24,.12); color:var(--bad); }
.card { background:var(--card); border:1px solid var(--line); border-radius:12px; padding:18px 20px; margin:16px 0; }
.grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(180px,1fr)); gap:12px; margin:12px 0; }
.kpi { border:1px solid var(--line); border-radius:10px; padding:12px; }
.kpi b { display:block; font-size:22px; }
.banner { border-left:4px solid var(--warn); background:rgba(138,90,0,.08); padding:10px 14px; border-radius:6px; margin:12px 0; }
table { width:100%; border-collapse:collapse; margin:10px 0; font-size:13px; }
th, td { text-align:left; vertical-align:top; border-bottom:1px solid var(--line); padding:7px 8px; }
th { color:var(--muted); font-weight:600; }
tr.failed td { background:rgba(180,35,24,.06); }
code { font:12px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace; background:rgba(127,127,127,.1); padding:1px 4px; border-radius:4px; word-break:break-word; }
pre { white-space:pre-wrap; word-break:break-word; background:rgba(127,127,127,.08); padding:10px; border-radius:8px; font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace; max-height:520px; overflow:auto; }
pre.error { border-left:3px solid var(--bad); }
.shots { display:grid; grid-template-columns:repeat(auto-fit,minmax(280px,1fr)); gap:14px; margin:12px 0; }
figure { margin:0; border:1px solid var(--line); border-radius:10px; overflow:hidden; background:var(--bg); }
figure img { display:block; width:100%; height:auto; }
figcaption { padding:6px 8px; font-size:11px; color:var(--muted); }
details { margin:10px 0; }
summary { cursor:pointer; color:var(--accent); }
ul { margin:6px 0 6px 20px; padding:0; }
a { color:var(--accent); }
.step { background:var(--card); border:1px solid var(--line); border-radius:12px; padding:18px 20px; margin:18px 0; }
</style>
</head>
<body>
<main>
<h1>keel-checkpoint · 真实栈 E2E 报告</h1>
<p class="meta">生成于 ${esc(report.startedAt)} → ${esc(report.finishedAt)} · 耗时 ${Math.round((new Date(report.finishedAt || Date.now()) - new Date(report.startedAt)) / 1000)}s · commit <code>${esc(report.commit || 'unknown')}</code></p>
<div class="banner">公网：${publicNote}</div>
<div class="grid">
  <div class="kpi"><b>${report.pass ? 'PASS' : 'FAIL'}</b>总结果</div>
  <div class="kpi"><b>${checkCount}</b>断言（失败 ${failedChecks}）</div>
  <div class="kpi"><b>${report.steps.length}</b>步骤</div>
  <div class="kpi"><b>${shotCount}</b>截图</div>
</div>
${report.error ? `<div class="card"><h2>运行错误</h2><pre class="error">${esc(report.error)}</pre></div>` : ''}

<div class="card">
  <h2>环境</h2>
  <table><tbody>${environmentRows}</tbody></table>
  <p class="muted">真实 PostgreSQL 临时库；<code>keel serve --auth local-only</code> 在 13600–13799 的临时端口；无 Docker。数据库与临时密钥已由 cleanup 删除。</p>
  ${cleanup ? `<details><summary>cleanup.json</summary><pre>${esc(JSON.stringify(cleanup, null, 2))}</pre></details>` : ''}
</div>

<div class="card">
  <h2>步骤索引</h2>
  <table><thead><tr><th>#</th><th>步骤</th><th>结果</th><th>断言</th><th>图</th></tr></thead><tbody>
  ${report.steps
    .map(
      (step, index) => `<tr><td>${index + 1}</td><td><a href="#step-${esc(step.id)}">${esc(step.title)}</a></td><td>${statusBadge(step.status)}</td><td>${step.checks?.filter((c) => c.ok).length || 0}/${step.checks?.length || 0}</td><td>${step.screenshots?.length || 0}</td></tr>`,
    )
    .join('\n')}
  </tbody></table>
</div>

${stepSections}

<div class="card">
  <h2>浏览器诊断</h2>
  <h3>console error</h3>
  ${consoleBlock}
  <h3>page error</h3>
  ${pageErrorBlock}
  <h3>失败请求</h3>
  ${requestBlock}
  <p class="muted">kill -9 步骤的 checkpoint 读取失败请求是预期错误态；它由步骤内的断言与截图确认。</p>
</div>

<div class="card">
  <h2>TUI 降级验证</h2>
  <p class="muted">同一份 TSX 在 <code>nsh test --target tui</code> 的终端宿主上验证：分区可进入、生效策略是一句可读文本、收窄与恢复可操作。TUI 证据是测试日志（终端 cell 不是位图）。</p>
  ${tuiLog ? `<details open><summary>tui-test.log</summary><pre>${esc(tuiLog.slice(0, 12000))}</pre></details>` : '<p class="muted">本次未运行 TUI 测试。</p>'}
</div>

<div class="card">
  <h2>范围与限制</h2>
  <ul>
    <li>自动压缩、手动压缩、requestId 幂等回放、kill -9 重启恢复与策略收窄均在真实 PG + 本地 <code>keel serve</code> 上执行。</li>
    <li>无权限 UI 态用拦截 <code>/v1/auth/whoami</code> 的只读身份展示；服务端 403 另由无 <code>session.read</code> 的 Bot key 实测断言。</li>
    <li>501/503「未接线」态没有单独制造：生产 <code>serve</code> 始终装配策略服务；网络断开错误态已覆盖同一错误块与重试。</li>
    <li>未部署公网；报告与截图只在本机 <code>.agents/reports</code>，不入库。</li>
  </ul>
</div>
</main>
</body>
</html>
`;

fs.writeFileSync(path.join(DIR, 'report.html'), html);
console.log(`report: ${path.join(DIR, 'report.html')}`);
