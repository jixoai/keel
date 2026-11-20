#!/usr/bin/env node
// Combines the per-scenario P6A2a browser reports into one self-contained
// report: evidence/p6/degrade/report.html + summary.json.
//
// Usage: degrade-report.mjs EVIDENCE_DIR scenario [scenario ...]
//
// Each scenario directory must contain report.json (written by
// browser-degrade.mjs) and its screenshots. The exit status is non-zero when
// any scenario failed, so a CI caller cannot mistake a merge for a pass.

import fs from 'node:fs';
import path from 'node:path';

const EVIDENCE = process.argv[2];
const SCENARIOS = process.argv.slice(3);
if (!EVIDENCE || SCENARIOS.length === 0) {
  console.error('usage: degrade-report.mjs EVIDENCE_DIR scenario [scenario ...]');
  process.exit(2);
}

const escapeHtml = (value) =>
  String(value)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;');

const img = (dir, relative) => {
  const full = path.join(dir, relative);
  if (!fs.existsSync(full)) return '';
  return `<img alt="${escapeHtml(relative)}" src="data:image/png;base64,${fs.readFileSync(full).toString('base64')}">`;
};

const LABEL = {
  handshake: '握手超时 → 文本兜底',
  csp: 'CSP 拒绝未声明域名',
};
const summary = { lane: 'P6A2a', generatedAt: new Date().toISOString(), scenarios: [], pass: false };
const sections = [];

for (const name of SCENARIOS) {
  const dir = path.join(EVIDENCE, name);
  const file = path.join(dir, 'report.json');
  if (!fs.existsSync(file)) {
    summary.scenarios.push({ name, pass: false, error: `missing ${file}` });
    sections.push(`<section class="scenario bad"><h2>${escapeHtml(name)} · FAIL</h2><p>missing report.json</p></section>`);
    continue;
  }
  const report = JSON.parse(fs.readFileSync(file, 'utf8'));
  const assertions = Object.entries(report.assertions ?? {});
  const failed = assertions.filter(([, value]) => !value.ok);
  const steps = report.steps ?? [];
  summary.scenarios.push({
    name,
    run: report.run,
    pass: Boolean(report.pass),
    error: report.error ?? null,
    assertions: Object.fromEntries(assertions.map(([key, value]) => [key, Boolean(value.ok)])),
    passedAssertions: assertions.length - failed.length,
    failedAssertions: failed.map(([key]) => key),
    timing: report.timing ?? null,
    csp: report.csp ?? null,
    findings: report.findings ?? [],
    screenshots: steps.filter((entry) => entry.screenshot).map((entry) => entry.screenshot),
    startedAt: report.startedAt,
    finishedAt: report.finishedAt,
  });

  const assertionRows = assertions
    .map(
      ([key, value]) =>
        `<tr class="${value.ok ? 'ok' : 'bad'}"><td>${escapeHtml(key)}</td><td>${value.ok ? 'PASS' : 'FAIL'}</td><td><code>${escapeHtml(
          JSON.stringify(value.detail),
        )}</code></td></tr>`,
    )
    .join('\n');
  const stepBlocks = steps
    .map(
      (entry) =>
        `<section class="step ${entry.ok ? 'ok' : 'bad'}"><h3>${escapeHtml(entry.name)} ${entry.ok ? '✓' : '✗'}</h3>` +
        (entry.screenshot ? img(dir, entry.screenshot) : '') +
        `<pre>${escapeHtml(JSON.stringify(entry.detail, null, 2))}</pre></section>`,
    )
    .join('\n');
  const findings = (report.findings ?? [])
    .map(
      (finding) =>
        `<section class="step bad"><h3>${escapeHtml(finding.id)}</h3><p>${escapeHtml(finding.summary ?? '')}</p><pre>${escapeHtml(
          JSON.stringify(finding, null, 2),
        )}</pre></section>`,
    )
    .join('\n');
  const extra =
    name === 'csp'
      ? `<h3>服务端未收到请求（CSP trap）</h3><pre>${escapeHtml(
          JSON.stringify(
            {
              trapLog: report.csp?.trap?.log ?? null,
              connectionsDuringApp: report.csp?.trap?.connectionsDuringApp ?? null,
              negativeControlConnections: report.csp?.control?.connections ?? null,
              document: report.csp?.document ?? null,
            },
            null,
            2,
          ),
        )}</pre>`
      : `<h3>握手超时计时</h3><pre>${escapeHtml(JSON.stringify(report.timing ?? null, null, 2))}</pre>`;

  sections.push(
    `<section class="scenario ${report.pass ? 'ok' : 'bad'}">
      <h2>${escapeHtml(LABEL[name] ?? name)} <span class="meta">(${escapeHtml(name)})</span> · <span class="verdict ${report.pass ? 'pass' : 'fail'}">${report.pass ? 'PASS' : 'FAIL'}</span></h2>
      <div class="meta">${escapeHtml(report.startedAt)} → ${escapeHtml(report.finishedAt)} · 断言 ${assertions.length - failed.length}/${assertions.length}</div>
      ${report.error ? `<p><strong>error:</strong> <code>${escapeHtml(report.error)}</code></p>` : ''}
      <h3>断言</h3><table><thead><tr><th>断言</th><th>结果</th><th>证据</th></tr></thead><tbody>${assertionRows}</tbody></table>
      ${extra}
      ${findings ? `<h3>走查发现</h3>${findings}` : ''}
      <h3>逐步截图</h3>${stepBlocks}
    </section>`,
  );
}

summary.pass = summary.scenarios.every((scenario) => scenario.pass);
fs.writeFileSync(path.join(EVIDENCE, 'summary.json'), JSON.stringify(summary, null, 2));

const html = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>P6A2a · MCP Apps 降级场景真实浏览器 E2E</title>
<style>
  body{font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;margin:24px;max-width:1180px;color:#18181b}
  h1{font-size:22px;margin:0 0 4px} h2{font-size:18px;margin:0 0 8px} h3{font-size:15px;margin:16px 0 6px}
  .verdict{display:inline-block;padding:2px 10px;border-radius:999px;font-weight:700}
  .verdict.pass{background:#dcfce7;color:#166534}.verdict.fail{background:#fee2e2;color:#991b1b}
  table{border-collapse:collapse;width:100%;margin:8px 0 18px;font-size:12px}
  th,td{border:1px solid #e4e4e7;padding:5px 8px;text-align:left;vertical-align:top}
  tr.ok td:nth-child(2){color:#166534;font-weight:700} tr.bad td:nth-child(2){color:#991b1b;font-weight:700}
  code{font:11px/1.4 ui-monospace,SFMono-Regular,Menlo,monospace;word-break:break-word}
  pre{font:11px/1.4 ui-monospace,SFMono-Regular,Menlo,monospace;background:#f4f4f5;padding:8px;border-radius:6px;white-space:pre-wrap;word-break:break-word}
  section.scenario{border:2px solid #e4e4e7;border-radius:10px;padding:16px;margin:22px 0}
  section.scenario.ok{border-color:#86efac} section.scenario.bad{border-color:#fca5a5;background:#fff1f2}
  section.step{border:1px solid #e4e4e7;border-radius:8px;padding:12px;margin:14px 0}
  section.step.bad{border-color:#fca5a5;background:#fff1f2}
  img{max-width:100%;border:1px solid #e4e4e7;border-radius:6px;margin:6px 0}
  .meta{color:#52525b;font-size:12px;margin-bottom:12px}
</style></head><body>
<h1>P6A2a · MCP Apps 降级场景真实浏览器 E2E</h1>
<div class="meta">
  ${escapeHtml(summary.generatedAt)} ·
  <span class="verdict ${summary.pass ? 'pass' : 'fail'}">${summary.pass ? 'PASS' : 'FAIL'}</span>
  · 场景 ${summary.scenarios.filter((scenario) => scenario.pass).length}/${summary.scenarios.length}
</div>
<p>同一条 P6A1 启动链：真实 <code>keel serve</code> → 真实 stdio MCP fixture → 真实 Chrome
（playwright-core + 系统 Chrome）。证据只记录断言、方法名、状态、计时与大小；不含
cookie/token/DSN/Authorization。</p>
<p>运行：<code>scripts/mcp-apps-degrade-e2e.sh</code>（默认两个场景；端到端外限 240s/场景）。</p>
${sections.join('\n')}
</body></html>`;

fs.writeFileSync(path.join(EVIDENCE, 'report.html'), html);
console.log(JSON.stringify({ pass: summary.pass, scenarios: summary.scenarios.map((scenario) => ({ name: scenario.name, pass: scenario.pass })) }));
process.exit(summary.pass ? 0 : 1);
