#!/usr/bin/env node
// Render the keel.telemetry live E2E evidence into one self-contained HTML
// report. The report is generated from steps.json + residue.txt; it is never
// hand-written and never reads a secret file.
//
//   node report.mjs <evidence-dir>
import fs from 'node:fs';
import path from 'node:path';

const DIR = process.argv[2];
if (!DIR) {
  console.error('usage: report.mjs <evidence-dir>');
  process.exit(2);
}
const stepsFile = path.join(DIR, 'steps.json');
if (!fs.existsSync(stepsFile)) {
  console.error(`missing ${stepsFile}`);
  process.exit(2);
}
const report = JSON.parse(fs.readFileSync(stepsFile, 'utf8'));
const residueFile = path.join(DIR, 'residue.txt');
const residue = fs.existsSync(residueFile) ? fs.readFileSync(residueFile, 'utf8') : '';
const collectorFile = path.join(DIR, 'collector-summary.json');
const collector = fs.existsSync(collectorFile)
  ? JSON.parse(fs.readFileSync(collectorFile, 'utf8'))
  : report.collector || {};

const esc = (value) =>
  String(value ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');

const totalChecks = report.steps.reduce((sum, step) => sum + (step.checks?.length || 0), 0);
const failedChecks = report.steps.reduce((sum, step) => sum + (step.checks || []).filter((check) => !check.ok).length, 0);
const passedSteps = report.steps.filter((step) => step.status === 'passed').length;
const overallPass = report.pass === true && failedChecks === 0;

const badge = (status) => {
  const pass = status === 'passed';
  return `<span class="badge ${pass ? 'ok' : 'bad'}">${pass ? 'PASS' : 'FAIL'}</span>`;
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
  <td>${check.expected === null || check.expected === undefined ? '' : `<code>${esc(JSON.stringify(check.expected))}</code>`}</td>
</tr>`,
  )
  .join('\n')}
</tbody></table>`;
};

const details = (value) => {
  if (!value || Object.keys(value).length === 0) return '';
  const text = JSON.stringify(value, null, 2);
  return `<details><summary>结构化细节 (${text.length} bytes)</summary><pre>${esc(text)}</pre></details>`;
};

const stepSections = report.steps
  .map(
    (step, index) => `<section class="step" id="step-${esc(step.id)}">
  <h2>${String(index).padStart(2, '0')} · ${esc(step.title)} ${badge(step.status)}</h2>
  <p class="meta">${esc(step.id)} · ${step.durationMs ?? 0} ms · ${step.checks?.length || 0} 条断言 · ${(step.checks || []).filter((check) => !check.ok).length} 条失败</p>
  ${checksTable(step.checks)}
  ${details(step.details)}
</section>`,
  )
  .join('\n');

const environmentRows = Object.entries(report.environment || {})
  .map(([key, value]) => `<tr><th>${esc(key)}</th><td>${esc(value)}</td></tr>`)
  .join('\n');

const pathRows = Object.entries(collector.paths || {})
  .map(([requestPath, count]) => `<tr><td><code>${esc(requestPath)}</code></td><td>${esc(count)}</td></tr>`)
  .join('\n') || '<tr><td colspan="2" class="muted">没有请求</td></tr>';

const list = (values) =>
  values && values.length
    ? `<ul>${values.map((value) => `<li><code>${esc(value)}</code></li>`).join('')}</ul>`
    : '<p class="muted">无</p>';

const auditSummary = (() => {
  const step = report.steps.find((entry) => entry.id === 'enable-approval');
  const actions = step?.details?.auditActions;
  if (!actions) return '<p class="muted">无审计细节。</p>';
  return `<ul>${actions.map((entry) => `<li><code>${esc(entry.action)}</code> ${esc(entry.result)} ${esc(entry.actor || '')}</li>`).join('')}</ul>`;
})();

const html = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>keel.telemetry live E2E — ${esc(report.startedAt)}</title>
<style>
:root { color-scheme: light dark; --bg:#f7f8fa; --card:#fff; --ink:#16181d; --muted:#626b7a; --line:#e3e6ec; --ok:#0a7a3d; --bad:#b42318; --accent:#0b5ed7; }
@media (prefers-color-scheme: dark) { :root { --bg:#111318; --card:#181b22; --ink:#e8ebf1; --muted:#9aa4b2; --line:#2a2f39; --ok:#4ade80; --bad:#f87171; --accent:#60a5fa; } }
* { box-sizing: border-box; }
body { margin:0; padding:28px; background:var(--bg); color:var(--ink); font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans SC",sans-serif; }
main { max-width:1180px; margin:0 auto; }
h1 { font-size:24px; margin:0 0 4px; }
h2 { font-size:17px; margin:0 0 6px; display:flex; gap:10px; align-items:center; flex-wrap:wrap; }
p { margin:8px 0; }
.muted { color:var(--muted); }
.meta { color:var(--muted); font-size:12px; }
.badge { display:inline-block; padding:1px 8px; border-radius:999px; font-size:12px; font-weight:600; color:#fff; }
.badge.ok { background:var(--ok); }
.badge.bad { background:var(--bad); }
.hero { background:var(--card); border:1px solid var(--line); border-radius:12px; padding:18px 20px; margin-bottom:18px; }
.stats { display:flex; gap:18px; flex-wrap:wrap; margin-top:10px; }
.stat { min-width:150px; }
.stat b { display:block; font-size:22px; }
table { width:100%; border-collapse:collapse; background:var(--card); border:1px solid var(--line); border-radius:8px; overflow:hidden; margin:10px 0; }
th, td { text-align:left; padding:7px 10px; border-bottom:1px solid var(--line); vertical-align:top; }
th { font-size:12px; color:var(--muted); font-weight:600; }
tr.failed td { background:color-mix(in srgb, var(--bad) 8%, transparent); }
code { font-family:ui-monospace,SFMono-Regular,Menlo,monospace; font-size:12px; overflow-wrap:anywhere; }
pre { background:var(--card); border:1px solid var(--line); border-radius:8px; padding:12px; overflow:auto; font-size:12px; }
section.step { background:var(--card); border:1px solid var(--line); border-radius:12px; padding:16px 18px; margin:16px 0; }
details { margin-top:10px; }
summary { cursor:pointer; color:var(--accent); font-size:13px; }
ul { margin:6px 0; padding-left:22px; }
.cols { display:grid; grid-template-columns:repeat(auto-fit,minmax(320px,1fr)); gap:16px; }
.panel { background:var(--card); border:1px solid var(--line); border-radius:12px; padding:14px 16px; margin:16px 0; }
a { color:var(--accent); }
</style>
</head>
<body>
<main>
<section class="hero">
  <h1>keel.telemetry 真实服务 E2E ${overallPass ? 'PASS' : 'FAIL'}</h1>
  <p class="muted">任务 7.4 · 真实 PostgreSQL + 真实 <code>keel serve</code> + 独立 OTLP collector · HTTP/JSON 证据（无浏览器）</p>
  <div class="stats">
    <div class="stat"><span class="muted">步骤</span><b>${passedSteps}/${report.steps.length}</b></div>
    <div class="stat"><span class="muted">断言</span><b>${totalChecks - failedChecks}/${totalChecks}</b></div>
    <div class="stat"><span class="muted">collector 请求</span><b>${collector.requests ?? 0}</b></div>
    <div class="stat"><span class="muted">commit</span><b><code>${esc(String(report.commit || '').slice(0, 12))}</code></b></div>
  </div>
  ${report.error ? `<pre>${esc(report.error)}</pre>` : ''}
</section>

<section class="panel">
  <h2>环境</h2>
  <table><tbody>${environmentRows}</tbody></table>
  <p class="muted">开始 ${esc(report.startedAt)} · 结束 ${esc(report.finishedAt)}</p>
</section>

<section class="panel">
  <h2>collector 收到的事实</h2>
  <div class="cols">
    <div>
      <h3>按路径</h3>
      <table><thead><tr><th>路径</th><th>请求数</th></tr></thead><tbody>${pathRows}</tbody></table>
      <h3>Authorization</h3>
      <p class="muted">只记录 SHA-256 指纹，secrets 不出现在证据中。</p>
      ${list((collector.authorizationFingerprints || []).map((value) => `sha256:${String(value).slice(0, 16)}…`))}
    </div>
    <div>
      <h3>span 名</h3>${list(collector.spanNames)}
      <h3>metric 名</h3>${list(collector.metricNames)}
      <h3>log event</h3>${list(collector.logEvents)}
    </div>
  </div>
  <h3>插件审计（enable）</h3>
  ${auditSummary}
</section>

${stepSections}

<section class="panel">
  <h2>资源回收 residue.txt</h2>
  <pre>${esc(residue || '（缺失）')}</pre>
</section>
</main>
</body>
</html>
`;

fs.writeFileSync(path.join(DIR, 'report.html'), html);

// A small generated index next to the report, so an evidence directory can be
// read without opening the HTML.
const summary = [
  `keel.telemetry live E2E: ${overallPass ? 'PASS' : 'FAIL'}`,
  `startedAt=${report.startedAt}`,
  `finishedAt=${report.finishedAt}`,
  `commit=${report.commit || ''}`,
  `steps=${passedSteps}/${report.steps.length}`,
  `checks=${totalChecks - failedChecks}/${totalChecks}`,
  `collectorRequests=${collector.requests ?? 0}`,
  `collectorPaths=${JSON.stringify(collector.paths || {})}`,
  `spanNames=${JSON.stringify(collector.spanNames || [])}`,
  `metricNames=${JSON.stringify(collector.metricNames || [])}`,
  `logEvents=${JSON.stringify(collector.logEvents || [])}`,
  'artifacts=report.html, steps.json, collector-requests.jsonl, collector-summary.json, serve.log, run.log, residue.txt',
  `residue=${residue.includes('teardown=clean') ? 'clean' : 'check residue.txt'}`,
].join('\n') + '\n';
fs.writeFileSync(path.join(DIR, 'summary.txt'), summary);

console.log(`report: ${path.join(DIR, 'report.html')} (${html.length} bytes, ${overallPass ? 'PASS' : 'FAIL'})`);
console.log(`summary: ${path.join(DIR, 'summary.txt')}`);
// Generation succeeded even when the evidence records failures: the driver's
// exit code is the gate, not the renderer's.
process.exit(0);
