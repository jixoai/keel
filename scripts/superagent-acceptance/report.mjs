#!/usr/bin/env node
// Generates the scripted HTML report from report.json + captured evidence.
//
// Usage:
//   node scripts/superagent-acceptance/report.mjs <report-dir>
//
// The HTML is always generated; never hand-edit report.html.

import fs from "node:fs";
import path from "node:path";

const dir = process.argv[2];
if (!dir) {
  console.error("usage: node report.mjs <report-dir>");
  process.exit(2);
}
const dataPath = path.join(dir, "report.json");
const data = JSON.parse(fs.readFileSync(dataPath, "utf8"));

const esc = (value) => String(value == null ? "" : value).replace(/[&<>"']/g, (c) => ({
  "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
}[c]));
const rich = (value) => esc(value).replace(/`([^`]+)`/g, "<code>$1</code>").replace(/\n/g, "<br>");
const badge = (result) => `<span class="badge ${result === "PASS" ? "ok" : result === "FAIL" ? "fail" : "info"}">${esc(result)}</span>`;

function figures(shots) {
  if (!shots || !shots.length) return "";
  return `<div class="shots ${shots.length > 1 ? "multi" : ""}">` + shots.map((shot) => {
    const src = typeof shot === "string" ? shot : shot.src;
    const caption = typeof shot === "string" ? "" : shot.caption;
    return `<figure><a href="${esc(src)}" target="_blank" rel="noreferrer"><img loading="lazy" src="${esc(src)}" alt="${esc(caption || src)}"></a>${caption ? `<figcaption>${esc(caption)}</figcaption>` : ""}</figure>`;
  }).join("") + "</div>";
}

function stepCard(step) {
  return `<article class="step">
    <div class="step-no">${esc(step.n)}</div>
    <div class="step-body">
      <header><h3>${esc(step.name)}</h3>${badge(step.result || "INFO")}</header>
      <dl>
        <div><dt>操作</dt><dd>${rich(step.action)}</dd></div>
        <div><dt>预期</dt><dd>${rich(step.expect)}</dd></div>
        ${step.assertionNote ? `<div><dt>断言</dt><dd>${rich(step.assertionNote)}</dd></div>` : ""}
      </dl>
      ${step.log ? `<details class="log" open><summary>证据 / 日志</summary><pre>${esc(step.log)}</pre></details>` : ""}
      ${figures(step.shots)}
      ${step.links ? `<p class="links">${step.links.map((l) => `<a href="${esc(l.href)}">${esc(l.label)}</a>`).join(" · ")}</p>` : ""}
    </div>
  </article>`;
}

const stats = (data.stats || []).map((s) => `<div class="stat"><b>${esc(s.value)}</b><span>${esc(s.label)}</span></div>`).join("");
const env = (data.env || []).map(([k, v]) => `<div><dt>${esc(k)}</dt><dd>${rich(v)}</dd></div>`).join("");
const assertions = (data.assertions || []).map((a) => `<tr>
  <td><code>${esc(a.id)}</code></td><td>${esc(a.group)}</td><td>${esc(a.name)}</td>
  <td>${badge(a.result)}</td><td>${rich(a.detail)}</td><td class="num">${esc(a.step == null ? "—" : a.step)}</td>
</tr>`).join("");
const gaps = (data.gaps || []).map((g) => `<li><span class="sev ${g.severity === "P0" ? "p0" : g.severity === "P1" ? "p1" : "p2"}">${esc(g.severity)}</span><div><b>${esc(g.title)}</b><p>${rich(g.detail)}</p></div></li>`).join("");
const evidence = (data.evidence || []).map((e) => `<li><code>${esc(e.path)}</code><span>${esc(e.note)}</span></li>`).join("");
const commands = (data.commands || []).map((c) => `<pre class="cmd">${esc(c)}</pre>`).join("");

const html = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light">
<title>${esc(data.title)}</title>
<style>
  :root{--ink:#161a22;--dim:#586171;--faint:#8b94a3;--hair:#dde2e9;--gold:#e3a13a;--gold-deep:#9b6511;
    --gold-soft:#fdf2dd;--ok:#177a45;--ok-soft:#e5f4eb;--fail:#b3261e;--fail-soft:#fbe9e7;--info:#1d5fa8;--info-soft:#e6f0fb;
    --bg:#f7f4ec;--card:#fff;--mono:"SFMono-Regular",ui-monospace,"JetBrains Mono",Menlo,monospace;
    --sans:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Hiragino Sans GB",sans-serif}
  *{box-sizing:border-box}
  body{margin:0;background:var(--bg);color:var(--ink);font:14.5px/1.65 var(--sans)}
  main{max-width:1040px;margin:0 auto;padding:46px 24px 64px}
  .overline{letter-spacing:.2em;font-size:11px;font-weight:700;color:var(--gold-deep);margin:0 0 9px}
  h1{font-size:28px;line-height:1.28;margin:0 0 6px}
  .sub{color:var(--dim);font-size:14px;margin:0 0 18px;max-width:800px}
  .meta{display:flex;flex-wrap:wrap;gap:7px;margin-bottom:22px}
  .chip{font-size:11.5px;border:1px solid var(--hair);background:#fff;border-radius:999px;padding:2px 10px;color:var(--dim)}
  .chip b{color:var(--ink)}
  .concl{border-left:3px solid var(--gold);background:var(--gold-soft);padding:14px 17px;border-radius:0 11px 11px 0;margin:0 0 22px;font-size:14px}
  .concl b{color:var(--gold-deep)}
  .stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(120px,1fr));gap:9px;margin-bottom:26px}
  .stat{border:1px solid var(--hair);background:var(--card);border-radius:12px;padding:12px 14px 9px}
  .stat b{display:block;font-size:23px;line-height:1.15;color:var(--gold-deep);font-variant-numeric:tabular-nums}
  .stat span{font-size:11.5px;color:var(--faint)}
  h2{font-size:18px;margin:34px 0 14px;padding-bottom:8px;border-bottom:1px solid var(--hair)}
  .env{display:grid;grid-template-columns:130px 1fr;gap:7px 14px;margin:0 0 8px}
  .env dt{font-size:11.5px;font-weight:700;color:var(--gold-deep);letter-spacing:.05em}
  .env dd{margin:0;font-size:13px;color:var(--dim)}
  .timeline{position:relative;padding-left:46px}
  .timeline::before{content:"";position:absolute;left:16px;top:8px;bottom:8px;width:2px;background:var(--hair)}
  .step{position:relative;margin-bottom:20px;background:var(--card);border:1px solid var(--hair);border-radius:14px;padding:16px 18px}
  .step-no{position:absolute;left:-46px;top:16px;width:32px;height:32px;border-radius:50%;background:var(--card);border:2px solid var(--gold);color:var(--gold-deep);display:flex;align-items:center;justify-content:center;font-weight:700;font-size:12px;font-variant-numeric:tabular-nums}
  .step header{display:flex;align-items:center;gap:10px;margin-bottom:9px}
  .step h3{font-size:15px;margin:0}
  .badge{margin-left:auto;font-size:11px;font-weight:700;padding:2px 9px;border-radius:999px}
  .badge.ok{background:var(--ok-soft);color:var(--ok)} .badge.fail{background:var(--fail-soft);color:var(--fail)} .badge.info{background:var(--info-soft);color:var(--info)}
  dl{margin:0}
  dl>div{display:grid;grid-template-columns:46px 1fr;gap:10px;margin:0 0 6px}
  dt{font-size:11px;font-weight:700;color:var(--gold-deep);padding-top:2px}
  dd{margin:0;color:var(--dim);font-size:13px}
  code{font:12px var(--mono);background:rgba(22,26,34,.055);padding:1px 5px;border-radius:4px}
  details.log{margin-top:10px;border:1px solid var(--hair);border-radius:8px;background:rgba(22,26,34,.025)}
  details.log summary{cursor:pointer;padding:6px 11px;font-size:11.5px;font-weight:600;color:var(--dim)}
  details.log pre{margin:0;padding:2px 11px 10px;font:11.5px/1.6 var(--mono);color:var(--dim);overflow-x:auto;white-space:pre-wrap;overflow-wrap:anywhere}
  .shots{display:grid;gap:12px;margin-top:12px}
  .shots.multi{grid-template-columns:repeat(auto-fit,minmax(300px,1fr))}
  figure{margin:0;border:1px solid var(--hair);border-radius:10px;overflow:hidden;background:#faf8f1}
  figure img{display:block;width:100%;max-height:440px;object-fit:contain;background:#fff}
  figcaption{padding:5px 9px;font-size:11.5px;color:var(--faint);border-top:1px solid var(--hair);text-align:center}
  .links{font-size:12px;margin:10px 0 0}
  .links a{color:var(--info);text-decoration:none}
  table{width:100%;border-collapse:collapse;font-size:12.5px;background:var(--card);border:1px solid var(--hair);border-radius:10px;overflow:hidden}
  th{text-align:left;font-size:11px;letter-spacing:.06em;color:var(--faint);background:#fafbfc;padding:8px 10px;border-bottom:1px solid var(--hair)}
  td{padding:8px 10px;border-bottom:1px solid #eef1f5;vertical-align:top;color:var(--dim)}
  tr:last-child td{border-bottom:0}
  td.num{text-align:right;font-variant-numeric:tabular-nums}
  ul.gaps{list-style:none;margin:0;padding:0;display:flex;flex-direction:column;gap:10px}
  ul.gaps li{display:grid;grid-template-columns:38px 1fr;gap:10px;background:var(--card);border:1px solid var(--hair);border-radius:11px;padding:12px 14px}
  ul.gaps b{display:block;font-size:13.5px;margin-bottom:2px}
  ul.gaps p{margin:0;color:var(--dim);font-size:12.5px}
  .sev{display:inline-flex;align-items:center;justify-content:center;height:24px;border-radius:6px;font-size:11px;font-weight:700}
  .sev.p0{background:var(--fail-soft);color:var(--fail)} .sev.p1{background:var(--gold-soft);color:var(--gold-deep)} .sev.p2{background:var(--info-soft);color:var(--info)}
  ul.evidence{list-style:none;margin:0;padding:0;font-size:12.5px;color:var(--dim)}
  ul.evidence li{display:flex;gap:10px;padding:4px 0;border-bottom:1px dashed var(--hair)}
  ul.evidence li:last-child{border-bottom:0}
  ul.evidence span{color:var(--faint)}
  pre.cmd{background:#111722;color:#d7e2ef;border-radius:9px;padding:11px 13px;font:11.5px/1.55 var(--mono);overflow-x:auto;white-space:pre-wrap;overflow-wrap:anywhere;margin:0 0 8px}
  footer{margin-top:38px;padding-top:14px;border-top:1px solid var(--hair);color:var(--faint);font-size:11.5px;display:flex;justify-content:space-between;gap:12px;flex-wrap:wrap}
  @media (max-width:720px){
    main{padding:28px 14px 48px} h1{font-size:22px}
    .timeline{padding-left:34px} .step-no{left:-38px;width:28px;height:28px}
    .env{grid-template-columns:100px 1fr} dl>div{grid-template-columns:1fr}
    table{display:block;overflow-x:auto}
  }
</style></head>
<body><main>
  <p class="overline">${esc(data.overline || "FDE2E · KEEL")}</p>
  <h1>${esc(data.title)}</h1>
  <p class="sub">${rich(data.subtitle)}</p>
  <div class="meta">${(data.meta || []).map((m) => `<span class="chip">${rich(m)}</span>`).join("")}</div>
  <div class="concl">${rich(data.conclusion)}</div>
  <div class="stats">${stats}</div>

  <h2>环境与方法</h2>
  <dl class="env">${env}</dl>

  <h2>逐步时间线</h2>
  <div class="timeline">${(data.steps || []).map(stepCard).join("")}</div>

  <h2>断言总表</h2>
  <table><thead><tr><th>ID</th><th>组</th><th>断言</th><th>结果</th><th>证据</th><th class="num">步骤</th></tr></thead><tbody>${assertions}</tbody></table>

  <h2>UI 缺口（必须在产品化前补齐）</h2>
  <ul class="gaps">${gaps}</ul>

  <h2>证据文件</h2>
  <ul class="evidence">${evidence}</ul>

  <h2>复现命令</h2>
  ${commands}

  <footer><span>${esc(data.footer || "")}</span><span>scripted by scripts/superagent-acceptance/report.mjs</span></footer>
</main></body></html>`;

fs.writeFileSync(path.join(dir, "report.html"), html);
console.log("wrote " + path.join(dir, "report.html"));
