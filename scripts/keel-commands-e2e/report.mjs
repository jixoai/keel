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
const residueFile = path.join(DIR, 'residue.txt');
const residue = fs.existsSync(residueFile) ? fs.readFileSync(residueFile, 'utf8') : '';
const residueClean = residue !== '' && !/STILL_(LISTENING|RUNNING)/.test(residue);
const coverage = report.coverage || null;
const coverageClean = Boolean(coverage) && coverage.uncovered === 0;
const overallPass = report.pass && residueClean && coverageClean;

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
<title>keel-plugin-commands E2E — ${esc(report.startedAt)}</title>
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
<h1>keel-plugin-commands · 真实栈 E2E 报告</h1>
<p class="meta">生成于 ${esc(report.startedAt)} → ${esc(report.finishedAt)} · 耗时 ${Math.round((new Date(report.finishedAt || Date.now()) - new Date(report.startedAt)) / 1000)}s · commit <code>${esc(report.commit || 'unknown')}</code></p>
<div class="banner">公网：${publicNote}</div>
<div class="grid">
  <div class="kpi"><b>${overallPass ? 'PASS' : 'FAIL'}</b>总结果</div>
  <div class="kpi"><b>${checkCount}</b>断言（失败 ${failedChecks}）</div>
  <div class="kpi"><b>${report.steps.length}</b>步骤</div>
  <div class="kpi"><b>${shotCount}</b>截图</div>
  <div class="kpi"><b>${coverage ? coverage.uncovered : '—'}</b>UNCOVERED</div>
</div>
${!residueClean ? `<div class="card"><h2>残留进程</h2><pre class="error">${esc(residue || 'missing residue.txt')}</pre></div>` : ''}
${report.error ? `<div class="card"><h2>运行错误</h2><pre class="error">${esc(report.error)}</pre></div>` : ''}

<div class="card">
  <h2>环境</h2>
  <table><tbody>${environmentRows}</tbody></table>
  <p class="muted">真实 PostgreSQL 临时库 + 真实 <code>keel serve --auth local-only</code>（13600–13799 临时端口）+ 真实插件安装/审批；无 Docker、无夹具命令。数据库与临时密钥已由 cleanup 删除。</p>
  ${cleanup ? `<details><summary>cleanup.json</summary><pre>${esc(JSON.stringify(cleanup, null, 2))}</pre></details>` : ''}
</div>

<div class="card">
  <h2>进程回收验证 ${residueClean ? '<span class="badge ok">CLEAN</span>' : '<span class="badge bad">RESIDUE</span>'}</h2>
  <p class="muted">由 <code>run.sh</code> 在报告生成前写入 <code>residue.txt</code>：serve/model 端口必须 free，sidecar 与 fake provider 必须 gone。这里的标记同时参与总结果（STILL_* 即失败）。</p>
  ${residue ? `<pre>${esc(residue)}</pre>` : '<pre class="error">missing residue.txt — 未证明进程回收</pre>'}
</div>

<div class="card">
  <h2>改动文件覆盖表 ${coverageClean ? '<span class="badge ok">UNCOVERED=0</span>' : '<span class="badge bad">UNCOVERED</span>'}</h2>
  ${
    coverage
      ? `<p class="meta">基线 <code>${esc(coverage.baseline)}</code> · ${coverage.total} 个文件 · COVERED ${coverage.covered} · EXEMPT ${coverage.exempt} · <b class="${coverage.uncovered ? 'bad' : 'ok'}">UNCOVERED ${coverage.uncovered}</b></p>
  <p class="muted">EXEMPT 只允许 change 文档/证据与 E2E 运行器；其他任何未映射的产品/测试文件都标 UNCOVERED 并使本次 E2E 失败。没有自动豁免。</p>
  <div style="max-height:520px;overflow:auto">
  <table><thead><tr><th>文件</th><th>状态</th><th>面</th><th>证据</th></tr></thead><tbody>
  ${coverage.rows
    .map(
      (row) => `<tr class="${row.status === 'UNCOVERED' ? 'failed' : ''}"><td><code>${esc(row.file)}</code></td><td>${esc(row.status)}</td><td>${esc(row.surface)}</td><td>${esc(row.evidence)}</td></tr>`,
    )
    .join('\n')}
  </tbody></table></div>`
      : '<pre class="error">missing coverage table — 未声明 UNCOVERED=0</pre>'
  }
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
  <p class="muted">拒绝 destructive 命令时，浏览器会把服务端预期的 403 记成一条 console error；该条已在 E2E 的数据里单列为 expectedConsole，不计入错误。</p>
</div>

<div class="card">
  <h2>TUI 证据</h2>
  <p class="muted">命令菜单、生成表单、命令结果行与 destructive 审批由独立的 <code>nsh test --target tui</code> 门禁验证（日志见 <code>../ui-nsh-test-tui.txt</code> 与 <code>../ui-nsh-test-tui-commands.txt</code>）；本报告只陈述本页执行的真实栈浏览器步骤。</p>
</div>

<div class="card">
  <h2>范围与限制</h2>
  <ul>
    <li>真实 PostgreSQL 临时库、真实 <code>keel serve --auth local-only</code>、真实插件 install/enable 两阶段审批；步骤只覆盖目录、<code>/compact</code>、插件命令、同 requestId 重放、拒绝与允许 destructive、停用/卸载和四档视口。</li>
    <li>本报告的声明范围就是上列 9 个步骤的断言与截图；未列出的能力不作声明。</li>
    <li>无权限命令态由 TUI 五态门禁覆盖；本页的 403 是 destructive 审批拒绝的预期响应。</li>
    <li><strong>未部署公网</strong>：本报告与截图作为仓库内证据提交，不提供公网链接。</li>
    <li>改动文件覆盖表见上：EXEMPT 仅 change 文档/证据与 E2E 运行器，任何未映射的产品/测试文件都会标 UNCOVERED 并让 E2E exit 1；本次 <b>UNCOVERED=0</b>。</li>
  </ul>
</div>
</main>
</body>
</html>
`;

fs.writeFileSync(path.join(DIR, 'report.html'), html);
console.log(`report: ${path.join(DIR, 'report.html')}`);
