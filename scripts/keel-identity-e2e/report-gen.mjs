#!/usr/bin/env node
// 【入仓镜像】来源：agenter2 主仓 apps/desktop/scripts/e2e-report-gen.mjs（fork-main
// 检出 /tmp/fde2e-fork-main，2026-10-20 版本——视频播放式体验版，含 R7-01 生成时防回
// 归）。本仓 drill/RUNBOOK 使用仓内路径，不再依赖某台机器的临时 checkout。

// e2e-report-gen.mjs — E2E 图文报告生成器（视频播放式体验版）
//
// Owner 2026-10-16 指令：逐步截图+HTML+解说 → 深度打磨为「模拟视频播放」体验——
//   · 底部 sticky 播放条：进度条（阅读进度映射滚动，金色填充）＋步骤关键点 markers
//     （每步一圆点按内容高度占比定位，hover 出步骤名 tooltip，点击平滑跳转）
//   · 播放/暂停：自动连续滚动（1x/2x 速度切换；滚到底自停；用户手动滚动即暂停；
//     空格 播放/暂停，←/→ 上一步/下一步）
//   · 当前步骤指示（播放条左侧：序号+名称+PASS 徽章）＋卡片左侧章节时间线
//     （连接线随阅读进度点亮金线，节点随状态变色）
//   · 图片 lightbox：布局内 max-height 缩放适配，点击开大图 overlay
//     （ESC/遮罩关闭、←/→ 与箭头按钮多图导航、文件名+步骤名、滚轮缩放、
//      拖动平移、点击/双击放大、1:1/适屏切换）
//   · 页首仪表化结论区（步骤/通过/断言/截图）＋日志默认折叠＋code 横向滚动
//   · 响应式（窄屏单列）＋打印态（隐藏播放条、自动展开日志）
//
// 用法：在截图证据目录旁放 steps.json，然后：
//   node scripts/e2e-report-gen.mjs <evidence-dir>
// 生成 <evidence-dir>/report.html（纯静态、同目录相对引用截图、零依赖、离线可开）。
//
// steps.json 形态（核心字段向后兼容；可选字段为扩展）：
// {
//   "title": "xxx 端到端验证",
//   "change": "ui-extensibility T7（F-018）",
//   "date": "2026-10-16",
//   "frozenBase": { "repo": "...", "commit": "..." },
//   "conclusion": "21/21 断言全绿 …",
//   "assertions": { "total": 21, "passed": 21 },          // 可选：仪表盘「断言」卡
//   "assertionGroups": [                                   // 可选（R7-02）：断言组稳定 ID 模型——
//     { "aid": "AG1", "group": "G0 隔离环境",              //   存在时仪表盘断言计数从本模型推导，
//       "name": "hub＋bot 前置", "result": "PASS",          //   并与 assertions 汇总/steps 挂接交叉校验，
//       "detail": "…", "step": 1 }                          //   不一致即生成失败（fail loudly）
//   ],
//   "steps": [
//     {
//       "n": 1, "name": "隔离环境首启",
//       "action": "启动 Electron dev 入口（隔离 USER_DATA_DIR/HERMES_HOME/独立 CDP 端口）",
//       "expect": "builtin 包自动播种",
//       "result": "PASS",
//       "shots": ["01-ball-bind-realdata.png"],            // 可空数组＝进程断言无截图
//       "assertions": ["AG1", "AG2"],                      // 可选：本步承载的断言组 ID（与 assertionGroups 对表）
//       "assertionNote": "进程断言无截图——…",               // 可选：显式标注（不计组/无截图等）
//       "log": "desktop.log: builtin seeded com.fde2e.work.default"
//     }
//   ]
// }
//
// R7-01 生成时防回归（2026-10-17 终审 P1）：steps[].shots 的每个 src 解析后
//   （无后缀自动补 .png，兼容历史 steps.json）必须在报告目录存在且为可解码
//   PNG（签名＋IHDR＋IEND）——否则生成失败，绝不产出带 404 引用的报告。
import { readFileSync, writeFileSync, existsSync } from 'node:fs'
import { basename, join, resolve } from 'node:path'

const dir = resolve(process.argv[2] ?? '.')
const stepsPath = join(dir, 'steps.json')
if (!existsSync(stepsPath)) {
  console.error(`缺少 ${stepsPath}（steps.json 形态见脚本头注释）`)
  process.exit(1)
}
const data = JSON.parse(readFileSync(stepsPath, 'utf8'))

const esc = (v) =>
  String(v ?? '').replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]))

const isPass = (r) => /^(PASS|GREEN|通过)/i.test(String(r))

const badge = (r) => `<span class="badge ${isPass(r) ? 'ok' : 'fail'}">${esc(r)}</span>`

/* ============ R7-01 生成时防回归：img 引用必须存在且可解码 ============ */

const IMG_EXT_RE = /\.(png|jpe?g|gif|webp|svg)$/i
const PNG_SIG = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])

function resolveShotSrc(f) {
  const clean = String(f ?? '').trim()
  if (!clean) throw new Error('steps[].shots 含空文件名')
  if (clean.includes('..') || clean.startsWith('/')) {
    throw new Error(`截图引用必须为报告目录内相对文件名（实得 "${clean}"）——报告契约是同目录相对引用`)
  }
  // 兼容历史 steps.json 无后缀：自动补 .png（R7-01 修复前的旧原料仍可生成）
  return IMG_EXT_RE.test(clean) ? clean : `${clean}.png`
}

function assertDecodableShot(src, stepLabel) {
  const file = join(dir, src)
  if (!existsSync(file)) {
    throw new Error(`R7-01 防回归：步骤 ${stepLabel} 的截图引用失效（404）——${file} 不存在（src="${src}"，报告目录 ${dir}）`)
  }
  const buf = readFileSync(file)
  if (buf.length < 33 || !buf.subarray(0, 8).equals(PNG_SIG)) {
    throw new Error(`R7-01 防回归：${file} 非 PNG（签名不符，${buf.length} 字节）`)
  }
  if (buf.toString('ascii', 12, 16) !== 'IHDR' || buf.readUInt32BE(16) === 0 || buf.readUInt32BE(20) === 0) {
    throw new Error(`R7-01 防回归：${file} PNG IHDR 异常（宽/高不可解码）`)
  }
  if (!buf.subarray(buf.length - 8, buf.length - 4).equals(Buffer.from('IEND', 'ascii'))) {
    throw new Error(`R7-01 防回归：${file} PNG 截断（缺 IEND 尾块）`)
  }
}

// 解析（无后缀补 .png）＋逐张校验——生成即失败，不产出带 404 引用的报告
const stepShots = data.steps.map((s, i) => {
  const srcs = (s.shots ?? []).map(resolveShotSrc)
  for (const src of srcs) assertDecodableShot(src, `#${Number(s.n) || i + 1} ${s.name ?? ''}`)
  return srcs
})

const stepChecks = data.steps.flatMap((s) => (Array.isArray(s.checks) ? s.checks : []));
if (data.assertions && Number(data.assertions.total) > 0 && stepChecks.length > 0 && Number(data.assertions.total) !== stepChecks.length) {
  throw new Error(`A6 防回归：assertions.total ${data.assertions.total} ≠ steps[].checks ${stepChecks.length}`)
}
for (const [i, s] of data.steps.entries()) {
  if (!s.result) throw new Error(`A6 防回归：步骤 #${i + 1} 没有 result`)
  if (!String(s.action || '').trim() || !String(s.expect || '').trim()) throw new Error(`A6 防回归：步骤 #${i + 1} 缺 action/expect`)
}

/* ============ R7-02：断言组模型（稳定 ID）——计数从模型推导并交叉校验 ============ */

const G = Array.isArray(data.assertionGroups) ? data.assertionGroups : null
let A = null
if (G) {
  const gTotal = G.length
  const gPassed = G.filter((g) => isPass(g.result)).length
  if (data.assertions && Number(data.assertions.total) > 0 &&
      (Number(data.assertions.total) !== gTotal || Number(data.assertions.passed) !== gPassed)) {
    throw new Error(`R7-02 防回归：断言计数不一致——assertionGroups 推导 ${gPassed}/${gTotal} ≠ assertions 汇总 ${data.assertions.passed}/${data.assertions.total}`)
  }
  const known = new Set(G.map((g) => String(g.aid)))
  for (const s of data.steps) {
    for (const aid of s.assertions ?? []) {
      if (!known.has(String(aid))) {
        throw new Error(`R7-02 防回归：步骤 ${Number(s.n)} 悬空引用断言组 ${aid}（assertionGroups 无此 ID）`)
      }
    }
  }
  const referenced = new Set(data.steps.flatMap((s) => (s.assertions ?? []).map(String)))
  const orphan = G.filter((g) => !referenced.has(String(g.aid))).map((g) => g.aid)
  if (orphan.length) {
    throw new Error(`R7-02 防回归：断言组未挂任何步骤（${orphan.join(',')}）——steps↔assertions 必须一一映射`)
  }
  A = { total: gTotal, passed: gPassed }
} else if (data.assertions && Number(data.assertions.total) > 0) {
  A = data.assertions
}

const agChip = (aid) => {
  const g = G ? G.find((x) => String(x.aid) === String(aid)) : null
  const ok = g ? isPass(g.result) : true
  const tip = g ? `${g.aid} · ${g.group} · ${g.name}${g.detail ? ` — ${g.detail}` : ''}` : String(aid)
  return `<span class="achip${ok ? '' : ' fail'}" title="${esc(tip)}">${esc(aid)}${ok ? ' ✓' : ' ✗'}</span>`
}

const shotsBlock = (list) => {
  const files = list ?? []
  if (!files.length) return ''
  const figures = files
    .map(
      (f) =>
        `    <figure tabindex="0" role="button" aria-label="放大查看 ${esc(basename(f))}"><div class="imgwrap"><img src="${esc(f)}" data-file="${esc(basename(f))}" alt="${esc(basename(f))}" loading="lazy" draggable="false"/></div><figcaption>${esc(basename(f))}</figcaption></figure>`,
    )
    .join('\n')
  return `  <div class="shots${files.length > 1 ? ' multi' : ''}">\n${figures}\n  </div>`
}


/* ============ A6 extension: per-step checks / findings / coverage ============ */

const checksBlock = (s) => {
  const list = Array.isArray(s.checks) ? s.checks : []
  if (!list.length) return ''
  const rowsHtml = list
    .map(
      (c) =>
        `      <tr class="${c.ok ? 'cok' : 'cfail'}"><td>${c.ok ? '✓' : '✗'}</td><td>${esc(c.name)}</td><td>${esc(c.expected ?? '—')}</td><td>${esc(c.actual ?? '—')}</td></tr>`,
    )
    .join('\n')
  return `    <details class="checks" open><summary>断言 ${list.filter((c) => c.ok).length}/${list.length}</summary><div class="checkwrap"><table><thead><tr><th></th><th>断言</th><th>预期</th><th>实际</th></tr></thead><tbody>\n${rowsHtml}\n    </tbody></table></div></details>`
}

const findingBlock = (f) => {
  const tone = String(f.severity || '').toUpperCase()
  const cls = tone === 'P0' ? 'p0' : tone === 'P1' ? 'p1' : 'p2'
  const ev = Array.isArray(f.evidence) && f.evidence.length
    ? `<ul>${f.evidence.map((x) => `<li>${esc(x)}</li>`).join('')}</ul>`
    : ''
  return `    <article class="finding ${cls}"><header><span class="sev">${esc(tone || 'P2')}</span><h3>${esc(f.title)}</h3></header><p>${esc(f.detail)}</p>${ev}</article>`
}

const findingsBlock = (() => {
  const list = Array.isArray(data.findings) ? data.findings : []
  if (!list.length) return ''
  const order = { P0: 0, P1: 1, P2: 2 }
  const sorted = list.slice().sort((a, b) => (order[String(a.severity).toUpperCase()] ?? 9) - (order[String(b.severity).toUpperCase()] ?? 9))
  const counts = { P0: 0, P1: 0, P2: 0 }
  for (const f of sorted) counts[String(f.severity).toUpperCase()] = (counts[String(f.severity).toUpperCase()] || 0) + 1
  return `<section class="findings"><p class="overline">Findings</p><h2>问题清单</h2><p class="meta">P0 ${counts.P0 || 0} · P1 ${counts.P1 || 0} · P2 ${counts.P2 || 0}</p>\n${sorted.map(findingBlock).join('\n')}</section>`
})()

const coverageBlock = (() => {
  const cov = data.coverage
  if (!cov || !Array.isArray(cov.rows)) return ''
  const rowsHtml = cov.rows
    .map(
      (r) =>
        `      <tr><td class="mono">${esc(r.file)}</td><td>${esc(r.change)}</td><td><span class="cstat ${r.status.startsWith('COVERED') ? 'covered' : r.status.startsWith('EXEMPT') ? 'exempt' : 'uncovered'}">${esc(r.status)}</span></td><td>${esc(r.surface)}</td><td>${esc(r.evidence)}</td></tr>`,
    )
    .join('\n')
  return `<section class="coverage"><p class="overline">Changed files</p><h2>改动文件覆盖表</h2><p class="meta">基线 <code>${esc(cov.baseline)}</code> · ${cov.total} 个文件 · COVERED ${cov.covered} · EXEMPT ${cov.exempt} · <b class="${cov.uncovered ? 'bad' : 'good'}">UNCOVERED ${cov.uncovered}</b></p><details><summary>展开 ${cov.total} 行</summary><div class="covwrap"><table><thead><tr><th>文件</th><th>变更</th><th>状态</th><th>面</th><th>证据</th></tr></thead><tbody>\n${rowsHtml}\n    </tbody></table></div></details></section>`
})()


const visualBlock = (() => {
  const list = Array.isArray(data.visualDelta) ? data.visualDelta : []
  if (!list.length) return ''
  // `after` is one screenshot path in newer steps.json (creation E2E) and an
  // array in older lanes; accept both without making the writer adapt.
  const afterList = (delta) => (Array.isArray(delta.after) ? delta.after : delta.after ? [delta.after] : [])
  const card = (delta) => {
    const before = delta.before
      ? `<figure><div class="imgwrap"><img src="${esc(delta.before)}" alt="before ${esc(delta.surface)}" loading="lazy"/></div><figcaption>before · ${esc(delta.note || '设计原型')}</figcaption></figure>`
      : `<div class="nobefore">旧界面不是基线（Owner 2026-11-21）；本面以真实登录/引导截图作 after。</div>`
    const after = afterList(delta)
      .map((f) => `<figure><div class="imgwrap"><img src="${esc(f)}" alt="after ${esc(delta.surface)}" loading="lazy"/></div><figcaption>after · ${esc(basename(f))}</figcaption></figure>`)
      .join('')
    return `    <article class="delta"><header><h3>${esc(delta.surface)}</h3><code>${esc((delta.files || []).join(' · '))}</code></header><div class="deltagrid"><div class="side"><span class="sidetag">before</span>${before}</div><div class="side"><span class="sidetag after">after</span>${after}</div></div></article>`
  }
  return `<section class="visuals"><p class="overline">Before / After</p><h2>视觉面前后对照</h2><p class="meta">before＝设计原型（旧 UI 明确不是基线）；after＝本次真实服务端 E2E 截图。非视觉文件在覆盖表中按三类豁免逐行登记。</p>\n${list.map(card).join('\n')}</section>`
})()

for (const delta of Array.isArray(data.visualDelta) ? data.visualDelta : []) {
  for (const ref of [delta.before, ...(Array.isArray(delta.after) ? delta.after : delta.after ? [delta.after] : [])].filter(Boolean)) {
    assertDecodableShot(ref, `visual ${delta.surface}`)
  }
}
const rows = data.steps
  .map((s, i) => {
    const ok = isPass(s.result)
    const log = s.log
      ? `    <details class="log"><summary>关键日志</summary><pre class="logcode">${esc(s.log)}</pre></details>`
      : ''
    const chips = s.assertions?.length
      ? `    <div class="achips"><span class="achips-label">断言组</span>${s.assertions.map(agChip).join('')}</div>`
      : ''
    const note = s.assertionNote ? `    <p class="anote">${esc(s.assertionNote)}</p>` : ''
    return `  <section class="step" data-no="${Number(s.n)}" data-name="${esc(s.name)}" data-result="${esc(s.result)}" data-ok="${ok ? 1 : 0}">
    <div class="node" aria-hidden="true">${Number(s.n)}</div>
    <header><h3>${esc(s.name)}</h3>${badge(s.result)}</header>
${chips}
${note}
    <dl class="fields">
      <div class="field"><dt>操作</dt><dd>${esc(s.action)}</dd></div>
      <div class="field"><dt>预期</dt><dd>${esc(s.expect)}</dd></div>
    </dl>
${checksBlock(s)}
${log}
${shotsBlock(stepShots[i])}
  </section>`
  })
  .join('\n')

const total = data.steps.length
const passed = data.steps.filter((s) => isPass(s.result)).length
const shotsTotal = stepShots.reduce((a, l) => a + l.length, 0)

const statCard = (num, den, label, okStyle) =>
  `    <div class="stat${okStyle ? ' ok' : ''}"><b>${num}${den != null ? `<i>/${den}</i>` : ''}</b><span>${label}</span></div>`


const publishBlock = (() => {
  const p = data.publish
  if (!p) return ''
  const published = p.published === true && p.url
  if (published) {
    return `<p class="publish ok"><b>公网链接</b><a href="${esc(p.url)}">${esc(p.url)}</a></p>`
  }
  return `<p class="publish pending"><b>公网链接</b><span>未发布（${esc(p.reason || '本次报告只在本地生成，未部署公网')}）</span></p>`
})()

const envBlock = (() => {
  const env = data.environment
  if (!env) return ''
  const rows = [
    ['base', env.base],
    ['database', env.database],
    ['tenant', env.tenant],
    ['postgres', env.realPostgres ? '真实' : '否'],
    ['serve', env.realServe ? '真实' : '否'],
    ['browser', env.realBrowser],
    ['model', env.model],
  ].filter(([, v]) => v !== undefined && v !== null && v !== '')
  return `    <div class="env">${rows.map(([k, v]) => `<span><b>${esc(k)}</b>${esc(v)}</span>`).join('')}</div>`
})()

const dashboard = [
  statCard(total, null, '步骤', false),
  statCard(passed, total, '步骤通过', passed === total),
  ...(A ? [statCard(A.passed, A.total, '断言通过', A.passed === A.total)] : []),
  statCard(shotsTotal, null, '截图', false),
].join('\n')

const firstStep = data.steps[0] ?? { n: 0, name: '', result: '', ok: false }

// 播放条进度条上的步骤关键点 markers（初始隐藏，JS 测量后按内容高度占比定位）
const markers = data.steps
  .map(
    (s, i) =>
      `      <button type="button" class="marker" data-i="${i}" data-tip="#${Number(s.n)} ${esc(s.name)}" aria-label="跳转到步骤 ${Number(s.n)}：${esc(s.name)}" style="left:0;display:none"></button>`,
  )
  .join('\n')


const gateFindings = Array.isArray(data.findings) ? data.findings : []
const gateP0 = gateFindings.filter((f) => String(f.severity).toUpperCase() === 'P0').length
const gateP1 = gateFindings.filter((f) => String(f.severity).toUpperCase() === 'P1').length
const gateP2 = gateFindings.filter((f) => String(f.severity).toUpperCase() === 'P2').length
const gateBanner = (gateP0 || gateP1)
  ? `<div class="gate fail"><b>A6 判定：未过门</b><span>P0 ${gateP0} · P1 ${gateP1} 未清零；P2 ${gateP2} 登记待排期。不得写作“完成”。</span></div>`
  : `<div class="gate pass"><b>A6 判定：通过</b><span>P0/P1 = 0；P2 ${gateP2} 登记。</span></div>`

const html = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<meta name="color-scheme" content="light"/>
<title>${esc(data.title)}</title>
<style>
  :root{--ink:#1A1C22;--dim:rgba(26,28,34,.72);--faint:rgba(26,28,34,.45);--hair:rgba(26,28,34,.14);
    --gold:#E9B44C;--goldDeep:#B07E24;--goldSoft:rgba(233,180,76,.12);
    --ok:#1D7A3E;--okSoft:rgba(29,122,62,.12);--fail:#B3261E;--failSoft:rgba(179,38,30,.12);
    --bg:#FFFDF7;--card:#FFFFFF;--chrome:#17181D;
    --ease:cubic-bezier(.22,.61,.36,1);--easeOut:cubic-bezier(.16,1,.3,1);--barH:64px}
  *{box-sizing:border-box}
  html.locked{overflow:hidden}
  body{margin:0;font:15px/1.65 "Space Grotesk","PingFang SC","Hiragino Sans GB",sans-serif;color:var(--ink);background:var(--bg)}
  main{max-width:960px;margin:0 auto;padding:44px 24px calc(var(--barH) + 56px)}
  .overline{letter-spacing:.22em;font-size:11px;font-weight:600;color:var(--goldDeep);text-transform:uppercase;margin:0 0 10px}
  h1{font-size:27px;line-height:1.3;margin:0 0 6px}
  h3{margin:0;font-size:16px}
  .meta{color:var(--dim);font-size:13px;margin:0 0 18px}
  .meta code{background:rgba(26,28,34,.06);padding:1px 6px;border-radius:4px;font-family:"JetBrains Mono",ui-monospace,monospace}
  .concl{border-left:3px solid var(--gold);padding:12px 16px;background:var(--goldSoft);margin:0 0 20px;border-radius:0 10px 10px 0;font-size:14.5px}
  .publish{display:flex;gap:10px;align-items:baseline;flex-wrap:wrap;margin:0 0 14px;padding:8px 12px;border-radius:9px;font-size:13px;border:1px solid var(--hair);background:rgba(26,28,34,.025);color:var(--dim)}
  .publish b{color:var(--ink);font-size:12px;letter-spacing:.06em}
  .publish a{color:var(--goldDeep);word-break:break-all}
  .publish.pending{border-color:rgba(179,38,30,.28);background:var(--failSoft)}
  .dashboard{display:grid;grid-template-columns:repeat(auto-fit,minmax(130px,1fr));gap:10px}
  .stat{border:1px solid var(--hair);border-radius:12px;padding:14px 16px 12px;background:var(--card);transition:border-color .3s var(--ease),transform .3s var(--ease)}
  .stat:hover{transform:translateY(-1px);border-color:rgba(233,180,76,.55)}
  .stat b{display:block;font-size:26px;line-height:1.15;color:var(--goldDeep);font-variant-numeric:tabular-nums}
  .stat b i{font-style:normal;font-size:13px;color:var(--faint);font-weight:600}
  .stat span{font-size:12px;color:var(--faint)}
  .stat.ok b{color:var(--ok)}
  /* 章节时间线 */
  .timeline{position:relative;padding-left:52px;margin-top:30px}
  .tl-line{position:absolute;left:17px;top:10px;bottom:10px;width:2px;background:var(--hair);border-radius:1px}
  .tl-fill{position:absolute;left:17px;top:10px;width:2px;height:0;background:var(--gold);border-radius:1px}
  .step{position:relative;border:1px solid var(--hair);border-radius:14px;padding:18px 20px;margin:0 0 24px;background:var(--card);transition:border-color .35s var(--ease),box-shadow .35s var(--ease)}
  .step.current{border-color:rgba(233,180,76,.7);box-shadow:0 6px 24px rgba(233,180,76,.14)}
  .node{position:absolute;left:-43px;top:20px;width:32px;height:32px;border-radius:50%;background:var(--card);border:2px solid var(--hair);display:flex;align-items:center;justify-content:center;font-size:12.5px;font-weight:700;color:var(--faint);font-variant-numeric:tabular-nums;transition:background .35s var(--ease),border-color .35s var(--ease),color .35s var(--ease),transform .35s var(--ease)}
  .step.past .node{border-color:var(--gold);color:var(--goldDeep)}
  .step.current .node{background:var(--gold);border-color:var(--gold);color:#17181D;transform:scale(1.08)}
  .step header{display:flex;align-items:center;gap:10px;margin-bottom:12px}
  .badge{margin-left:auto;flex:none;font-size:12px;font-weight:700;padding:2px 11px;border-radius:999px;letter-spacing:.04em}
  .badge.ok{background:var(--okSoft);color:var(--ok)}
  .badge.fail{background:var(--failSoft);color:var(--fail)}
  .badge.mini{font-size:10px;padding:1px 8px}
  .achips{display:flex;flex-wrap:wrap;align-items:center;gap:6px;margin:0 0 10px}
  .achips-label{font-size:10px;font-weight:700;letter-spacing:.12em;color:var(--goldDeep)}
  .achip{font-size:11px;font-weight:700;font-variant-numeric:tabular-nums;padding:1px 8px;border-radius:999px;background:var(--okSoft);color:var(--ok);cursor:help}
  .achip.fail{background:var(--failSoft);color:var(--fail)}
  .anote{margin:0 0 10px;font-size:12.5px;color:var(--dim);border-left:2px solid var(--gold);padding:2px 0 2px 10px}
  .fields{display:grid;gap:9px;margin:0}
  .field{display:grid;grid-template-columns:58px 1fr;gap:12px;align-items:start}
  .field dt{font-size:11px;font-weight:700;letter-spacing:.12em;color:var(--goldDeep);background:var(--goldSoft);border-radius:5px;padding:3px 0;text-align:center;margin-top:2px}
  .field dd{margin:0;font-size:14px}
  details.log{margin-top:12px;border:1px solid var(--hair);border-radius:9px;background:rgba(26,28,34,.025)}
  details.log summary{cursor:pointer;padding:7px 12px;font-size:12px;font-weight:600;color:var(--dim);list-style:none;display:flex;align-items:center;gap:8px;user-select:none}
  details.log summary::-webkit-details-marker{display:none}
  details.log summary::before{content:'▸';color:var(--goldDeep);transition:transform .25s var(--ease);display:inline-block}
  details.log[open] summary::before{transform:rotate(90deg)}
  pre.logcode{margin:0;padding:2px 12px 10px;font:12px/1.6 "JetBrains Mono",ui-monospace,monospace;color:var(--dim);overflow-x:auto;white-space:pre}
  pre.logcode::-webkit-scrollbar{height:8px}
  pre.logcode::-webkit-scrollbar-thumb{background:rgba(26,28,34,.18);border-radius:4px}
  /* 截图 */
  .shots{display:grid;gap:12px;margin-top:14px}
  .shots.multi{grid-template-columns:repeat(auto-fit,minmax(280px,1fr))}
  figure{margin:0;cursor:zoom-in;border:1px solid var(--hair);border-radius:11px;overflow:hidden;background:var(--card);transition:border-color .3s var(--ease),box-shadow .3s var(--ease)}
  figure:hover{border-color:rgba(233,180,76,.6);box-shadow:0 6px 20px rgba(26,28,34,.08)}
  figure:focus-visible{outline:2px solid var(--gold);outline-offset:2px}
  .imgwrap{position:relative;overflow:hidden;background:#FAF8F0}
  .imgwrap img{display:block;width:100%;max-height:380px;object-fit:contain;transition:transform .5s var(--ease)}
  figure:hover .imgwrap img{transform:scale(1.012)}
  .imgwrap::after{content:'⤢';position:absolute;right:8px;bottom:8px;width:24px;height:24px;border-radius:6px;background:rgba(23,24,29,.72);color:#FFFDF7;font-size:13px;display:flex;align-items:center;justify-content:center;opacity:0;transition:opacity .25s var(--ease);pointer-events:none}
  figure:hover .imgwrap::after{opacity:1}
  figcaption{padding:6px 10px;font-size:12px;color:var(--faint);text-align:center;border-top:1px solid var(--hair);background:rgba(26,28,34,.02);font-variant-numeric:tabular-nums}
  .foot{margin-top:34px;color:var(--faint);font-size:12px;text-align:center;line-height:1.8}
  .foot code{background:rgba(26,28,34,.05);padding:1px 5px;border-radius:4px}
  /* 播放条（视频式） */
  .player{position:fixed;left:0;right:0;bottom:0;z-index:60;height:var(--barH);background:var(--chrome);display:flex;align-items:center;gap:12px;padding:0 16px;box-shadow:0 -6px 24px rgba(26,28,34,.28)}
  .pp{flex:none;width:40px;height:40px;border-radius:50%;border:0;background:var(--gold);color:#17181D;font-size:13px;cursor:pointer;display:flex;align-items:center;justify-content:center;font-family:inherit;transition:transform .2s var(--ease),background .2s var(--ease)}
  .pp:hover{transform:scale(1.07);background:#F0C268}
  .spd{flex:none;background:transparent;border:1px solid rgba(255,253,247,.35);color:rgba(255,253,247,.9);border-radius:7px;padding:4px 9px;font-size:12px;font-weight:600;cursor:pointer;font-family:inherit;font-variant-numeric:tabular-nums;transition:border-color .2s var(--ease)}
  .spd:hover{border-color:var(--gold)}
  .cur{flex:none;max-width:264px;min-width:0;display:flex;align-items:center;gap:8px;white-space:nowrap;overflow:hidden;font-size:13px;color:rgba(255,253,247,.92)}
  .cur .cno{color:var(--gold);font-weight:700;flex:none}
  .track{position:relative;flex:1;height:26px;cursor:pointer;touch-action:none}
  .track::before{content:'';position:absolute;left:0;right:0;top:50%;height:4px;transform:translateY(-50%);background:rgba(255,253,247,.16);border-radius:2px}
  .fill{position:absolute;left:0;top:50%;height:4px;transform:translateY(-50%);width:0;background:var(--gold);border-radius:2px;pointer-events:none}
  .marker{position:absolute;top:50%;left:0;transform:translate(-50%,-50%);width:12px;height:12px;padding:0;border-radius:50%;background:#565A66;border:2px solid var(--chrome);cursor:pointer;display:none;transition:background .25s var(--ease),transform .2s var(--ease),box-shadow .25s var(--ease)}
  .marker:hover{transform:translate(-50%,-50%) scale(1.35)}
  .marker.past{background:var(--gold)}
  .marker.current{background:var(--gold);box-shadow:0 0 0 3px rgba(233,180,76,.35)}
  .marker::after{content:attr(data-tip);position:absolute;bottom:20px;left:50%;transform:translateX(-50%);background:var(--bg);color:var(--ink);border:1px solid var(--hair);padding:6px 11px;border-radius:8px;font-size:12px;font-weight:500;line-height:1.4;white-space:nowrap;max-width:58vw;overflow:hidden;text-overflow:ellipsis;opacity:0;pointer-events:none;transition:opacity .2s var(--ease)}
  .marker:hover::after{opacity:1}
  .count{flex:none;font-size:12px;color:rgba(255,253,247,.65);font-variant-numeric:tabular-nums;min-width:46px;text-align:right}
  /* Lightbox 大图 */
  #lightbox{position:fixed;inset:0;z-index:100;background:rgba(18,19,24,.97);display:none;flex-direction:column}
  #lightbox.open{display:flex}
  .lb-top{position:absolute;top:0;left:0;right:0;display:flex;align-items:flex-start;gap:14px;padding:14px 18px;background:linear-gradient(rgba(18,19,24,.85),rgba(18,19,24,0));color:rgba(255,253,247,.92);z-index:5}
  .lb-meta{display:flex;flex-direction:column;gap:2px;min-width:0}
  #lb-step{font-size:13px;font-weight:600;color:var(--gold)}
  #lb-file{font:12px/1.4 "JetBrains Mono",ui-monospace,monospace;color:rgba(255,253,247,.6);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .lb-x{margin-left:auto;flex:none;width:34px;height:34px;border-radius:50%;border:1px solid rgba(255,253,247,.3);background:transparent;color:rgba(255,253,247,.9);font-size:17px;cursor:pointer;font-family:inherit;transition:transform .2s var(--ease),border-color .2s var(--ease)}
  .lb-x:hover{transform:scale(1.08);border-color:var(--gold)}
  #lb-stage{flex:1;display:flex;align-items:center;justify-content:center;min-width:0;min-height:0;padding:52px 64px 64px;overflow:hidden}
  #lb-img{max-width:100%;max-height:100%;cursor:zoom-in;user-select:none;-webkit-user-drag:none;will-change:transform;transition:transform .18s var(--ease)}
  #lb-img.zoomed{cursor:grab}
  #lb-img.zoomed:active{cursor:grabbing}
  .lb-nav{position:absolute;top:50%;transform:translateY(-50%);z-index:5;width:44px;height:44px;border-radius:50%;border:1px solid rgba(255,253,247,.28);background:rgba(18,19,24,.5);color:rgba(255,253,247,.9);font-size:22px;line-height:1;cursor:pointer;font-family:inherit;transition:background .2s var(--ease),color .2s var(--ease),border-color .2s var(--ease)}
  .lb-nav:hover{background:rgba(233,180,76,.85);color:#17181D;border-color:transparent}
  .lb-prev{left:16px}
  .lb-next{right:16px}
  .lb-bottom{position:absolute;bottom:0;left:0;right:0;display:flex;align-items:center;gap:14px;padding:14px 18px;background:linear-gradient(rgba(18,19,24,0),rgba(18,19,24,.85));color:rgba(255,253,247,.75);font-size:12px;z-index:5}
  #lb-count{font-variant-numeric:tabular-nums;color:rgba(255,253,247,.9)}
  .lb-hint{flex:1;text-align:center;color:rgba(255,253,247,.45)}
  .lb-fit{background:transparent;border:1px solid rgba(255,253,247,.35);color:rgba(255,253,247,.9);border-radius:7px;padding:4px 12px;font-size:12px;font-weight:600;cursor:pointer;font-family:inherit;transition:border-color .2s var(--ease)}
  .lb-fit:hover{border-color:var(--gold)}
  .lb-zoom{min-width:44px;text-align:right;font-variant-numeric:tabular-nums}

  /* A6 extension: checks / findings / coverage */
  details.checks{margin-top:12px;border:1px solid var(--hair);border-radius:9px;background:rgba(26,28,34,.025)}
  details.checks summary{cursor:pointer;padding:7px 12px;font-size:12px;font-weight:600;color:var(--dim);list-style:none}
  details.checks summary::-webkit-details-marker{display:none}
  details.checks summary::before{content:'▸';color:var(--goldDeep);display:inline-block;margin-right:8px;transition:transform .25s var(--ease)}
  details.checks[open] summary::before{transform:rotate(90deg)}
  .checkwrap{overflow-x:auto;padding:0 10px 10px}
  .checkwrap table{width:100%;border-collapse:collapse;font-size:12.5px}
  .checkwrap th{text-align:left;font-size:10.5px;letter-spacing:.08em;color:var(--faint);padding:4px 6px;border-bottom:1px solid var(--hair)}
  .checkwrap td{padding:4px 6px;border-bottom:1px solid rgba(26,28,34,.06);vertical-align:top;word-break:break-word}
  .checkwrap tr.cok td:first-child{color:var(--ok);font-weight:700}
  .checkwrap tr.cfail td:first-child{color:var(--fail);font-weight:700}
  .checkwrap tr.cfail{background:var(--failSoft)}
  .findings,.coverage{margin-top:36px}
  .findings h2,.coverage h2{font-size:19px;margin:0 0 6px}
  .finding{border:1px solid var(--hair);border-left:4px solid var(--faint);border-radius:10px;padding:12px 16px;margin:0 0 12px;background:var(--card)}
  .finding.p0{border-left-color:var(--fail);background:var(--failSoft)}
  .finding.p1{border-left-color:#C77C12;background:rgba(199,124,18,.07)}
  .finding.p2{border-left-color:var(--gold)}
  .finding header{display:flex;align-items:center;gap:10px;margin-bottom:6px}
  .finding h3{margin:0;font-size:14.5px}
  .finding p{margin:0 0 6px;font-size:13px;color:var(--dim)}
  .finding ul{margin:4px 0 0;padding-left:18px;font-size:12px;color:var(--faint)}
  .sev{flex:none;font-size:10.5px;font-weight:800;letter-spacing:.06em;padding:2px 8px;border-radius:999px;background:var(--fail);color:#fff}
  .finding.p1 .sev{background:#C77C12}
  .finding.p2 .sev{background:var(--goldDeep)}
  .coverage details summary{cursor:pointer;font-size:12.5px;color:var(--dim);margin:8px 0}
  .covwrap{overflow:auto;max-height:560px;border:1px solid var(--hair);border-radius:10px}
  .covwrap table{width:100%;border-collapse:collapse;font-size:11.5px}
  .covwrap th{position:sticky;top:0;background:var(--card);text-align:left;padding:6px 8px;border-bottom:1px solid var(--hair);letter-spacing:.05em;color:var(--faint)}
  .covwrap td{padding:4px 8px;border-bottom:1px solid rgba(26,28,34,.06);vertical-align:top}
  .covwrap td.mono{font-family:"JetBrains Mono",ui-monospace,monospace;font-size:10.5px;word-break:break-all}
  .cstat{font-size:10px;font-weight:700;padding:1px 7px;border-radius:999px;white-space:nowrap}
  .cstat.covered{background:var(--okSoft);color:var(--ok)}
  .cstat.exempt{background:rgba(26,28,34,.06);color:var(--dim)}
  .cstat.uncovered{background:var(--failSoft);color:var(--fail)}
  .good{color:var(--ok)} .bad{color:var(--fail)}

  .visuals{margin-top:36px}
  .visuals h2{font-size:19px;margin:0 0 6px}
  .delta{border:1px solid var(--hair);border-radius:12px;padding:12px 14px;margin:0 0 14px;background:var(--card)}
  .delta header{display:flex;flex-wrap:wrap;align-items:baseline;gap:10px;margin-bottom:10px}
  .delta h3{margin:0;font-size:14.5px}
  .delta code{font-size:10.5px;color:var(--faint);background:rgba(26,28,34,.05);padding:2px 6px;border-radius:4px;word-break:break-all}
  .deltagrid{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,2fr);gap:12px}
  @media (max-width:720px){.deltagrid{grid-template-columns:1fr}}
  .side{display:flex;flex-direction:column;gap:8px;min-width:0}
  .sidetag{font-size:10px;font-weight:800;letter-spacing:.1em;color:var(--faint);text-transform:uppercase}
  .sidetag.after{color:var(--ok)}
  .delta figure{margin:0;cursor:zoom-in}
  .delta .imgwrap img{max-height:260px}
  .nobefore{border:1px dashed var(--hair);border-radius:9px;padding:18px 14px;font-size:12px;color:var(--faint);background:rgba(26,28,34,.02)}

  .env{display:flex;flex-wrap:wrap;gap:6px;margin:0 0 14px;font-size:11.5px;color:var(--dim)}
  .env span{background:rgba(26,28,34,.045);border:1px solid var(--hair);border-radius:6px;padding:3px 8px;word-break:break-all}
  .env b{font-weight:700;color:var(--goldDeep);margin-right:6px;font-size:10px;letter-spacing:.06em;text-transform:uppercase}

  .gate{display:flex;flex-wrap:wrap;align-items:baseline;gap:10px;border-radius:10px;padding:10px 14px;margin:0 0 14px;font-size:13px}
  .gate b{font-size:14px;letter-spacing:.02em}
  .gate.fail{background:var(--failSoft);border:1px solid rgba(179,38,30,.35);color:var(--fail)}
  .gate.pass{background:var(--okSoft);border:1px solid rgba(29,122,62,.35);color:var(--ok)}
  .gate span{color:var(--dim);font-size:12px}
  /* 响应式 */
  @media (max-width:720px){
    main{padding:32px 16px calc(var(--barH) + 48px)}
    h1{font-size:22px}
    .timeline{padding-left:40px}
    .tl-line,.tl-fill{left:12px}
    .node{left:-33px;width:28px;height:28px;font-size:11px}
    .cur{display:none}
    .player{gap:10px;padding:0 12px}
    .lb-hint{display:none}
    #lb-stage{padding:52px 12px 64px}
  }
  /* 打印态 */
  @media print{
    :root{--barH:0px}
    .player,#lightbox{display:none!important}
    body{background:#fff}
    main{max-width:none;padding:0}
    .step{break-inside:avoid;box-shadow:none}
    .imgwrap img{max-height:300px}
  }
</style></head><body><main>
<p class="overline">FDE2E · E2E Verification Report</p>
<h1>${esc(data.title)}</h1>
<p class="meta">${esc(data.change)} · ${esc(data.date)}${data.frozenBase ? ` · 冻结基线 <code>${esc(data.frozenBase.repo)}@${esc(String(data.frozenBase.commit).slice(0, 9))}</code>` : ''}</p>
<p class="concl">${esc(data.conclusion)}</p>
${publishBlock}
${gateBanner}
${envBlock}
<div class="dashboard">
${dashboard}
</div>
<div class="timeline" id="timeline">
  <div class="tl-line" aria-hidden="true"></div><div class="tl-fill" id="tl-fill" aria-hidden="true"></div>
${rows}
</div>
${findingsBlock}
${coverageBlock}
${visualBlock}
<footer class="foot">本报告由 <code>scripts/e2e-report-gen.mjs</code> 生成 · 纯静态零依赖单文件（离线可开）<br/>空格 播放/暂停 · ←/→ 上一步/下一步 · 点击截图看大图（滚轮缩放 / 拖动平移 / ESC 关闭）</footer>
</main>
<div class="player" id="player" role="toolbar" aria-label="报告播放控制">
  <button type="button" class="pp" id="pp" aria-label="播放" title="播放（空格）">▶</button>
  <button type="button" class="spd" id="spd" title="自动滚动速度">1x</button>
  <div class="cur" id="cur" aria-live="polite"></div>
  <div class="track" id="track">
    <div class="fill" id="fill"></div>
${markers}
  </div>
  <div class="count" id="count">--/${String(total).padStart(2, '0')}</div>
</div>
<div id="lightbox" role="dialog" aria-modal="true" aria-label="截图大图查看">
  <div class="lb-top">
    <div class="lb-meta"><span id="lb-step"></span><span id="lb-file"></span></div>
    <button type="button" class="lb-x" id="lb-x" aria-label="关闭" title="关闭（ESC）">×</button>
  </div>
  <button type="button" class="lb-nav lb-prev" id="lb-prev" aria-label="上一张（←）">‹</button>
  <div id="lb-stage"><img id="lb-img" alt="" draggable="false"/></div>
  <button type="button" class="lb-nav lb-next" id="lb-next" aria-label="下一张（→）">›</button>
  <div class="lb-bottom">
    <span id="lb-count"></span>
    <span class="lb-hint">滚轮缩放 · 拖动平移 · 点击放大 · ←/→ 切换 · ESC 关闭</span>
    <button type="button" class="lb-fit" id="lb-fit" title="适屏 / 原始尺寸（1:1）">1:1</button>
    <span class="lb-zoom" id="lb-zoom">100%</span>
  </div>
</div>
<script>
(function(){
'use strict';
var doc=document;
var steps=[].slice.call(doc.querySelectorAll('.step'));
var markers=[].slice.call(doc.querySelectorAll('.marker'));
var track=doc.getElementById('track');
var fill=doc.getElementById('fill');
var tlEl=doc.getElementById('timeline');
var tlFill=doc.getElementById('tl-fill');
var curEl=doc.getElementById('cur');
var countEl=doc.getElementById('count');
var pp=doc.getElementById('pp');
var spdBtn=doc.getElementById('spd');
var total=steps.length;
var geo=[];
var tlRect=null;
var speed=1;
var BASE=90; /* 1x 自动滚动速度：px/s */
var playing=false;
var raf=0;
var last=0;
var reduced=window.matchMedia&&window.matchMedia('(prefers-reduced-motion: reduce)').matches;
var smooth=reduced?'auto':'smooth';

function pageY(){return window.pageYOffset||doc.documentElement.scrollTop||0;}
function maxScroll(){return Math.max(1,doc.documentElement.scrollHeight-window.innerHeight);}
function topOf(el){var r=el.getBoundingClientRect();return r.top+pageY();}
function pad(n){return (n<10?'0':'')+n;}

function measure(){
  geo=steps.map(function(el,i){return{el:el,top:topOf(el),marker:markers[i]};});
  if(tlEl){var r=tlEl.getBoundingClientRect();tlRect={top:r.top+pageY(),height:r.height};}
  placeMarkers();
  paint();
}
/* markers 按内容高度占比布点；尾带（最后一屏内多章）按最小间距 nudging 防重叠——tooltip 仍指向真实步骤 */
function placeMarkers(){
  var m=maxScroll();
  var w=track.clientWidth||1;
  var GAP=14,EDGE=8;
  var xs=geo.map(function(g){return Math.min(1,Math.max(0,g.top/m))*w;});
  if(xs.length)xs[0]=Math.max(xs[0],EDGE);
  for(var i=1;i<xs.length;i++){if(xs[i]<xs[i-1]+GAP)xs[i]=xs[i-1]+GAP;}
  if(xs.length&&xs[xs.length-1]>w-EDGE){
    xs[xs.length-1]=w-EDGE;
    for(var j=xs.length-2;j>=0;j--){if(xs[j]>xs[j+1]-GAP)xs[j]=xs[j+1]-GAP;}
  }
  geo.forEach(function(g,i){
    g.marker.style.left=(xs[i]/w*100)+'%';
    g.marker.style.display='block';
  });
}
function currentIdx(){
  var y=pageY(),m=maxScroll();
  if(y>=m-4)return geo.length-1; /* 到底＝最后一章 */
  var anchor=y+120; /* 卡片顶越入视口 120px 即成为当前章（跳转定位 76px 后即命中本卡） */
  var idx=0;
  for(var i=0;i<geo.length;i++){if(geo[i].top<=anchor)idx=i;else break;}
  return idx;
}
function setCur(g){
  curEl.textContent='';
  var cno=doc.createElement('span');cno.className='cno';cno.textContent='#'+g.el.getAttribute('data-no');
  var txt=doc.createTextNode(' '+g.el.getAttribute('data-name')+' ');
  var b=doc.createElement('span');b.className='badge mini '+(g.el.getAttribute('data-ok')==='1'?'ok':'fail');b.textContent=g.el.getAttribute('data-result');
  curEl.appendChild(cno);curEl.appendChild(txt);curEl.appendChild(b);
  curEl.title=g.el.getAttribute('data-name');
}
function paint(){
  var y=pageY(),m=maxScroll(),p=Math.min(1,y/m);
  fill.style.width=(p*100)+'%';
  if(tlRect)tlFill.style.height=(p*tlRect.height)+'px'; /* 时间线金线与播放进度同相：到底即满 */
  var idx=currentIdx(),g=geo[idx]||null;
  for(var i=0;i<geo.length;i++){
    geo[i].el.classList.toggle('current',i===idx);
    geo[i].el.classList.toggle('past',i<idx);
    geo[i].marker.classList.toggle('current',i===idx);
    geo[i].marker.classList.toggle('past',i<idx);
  }
  if(g)setCur(g);
  countEl.textContent=pad(idx+1)+'/'+pad(total);
}
window.addEventListener('scroll',paint,{passive:true});
window.addEventListener('resize',measure);
window.addEventListener('load',measure);
if(window.ResizeObserver){new ResizeObserver(function(){measure();}).observe(doc.body);}
measure();

/* ---------- 自动滚动播放 ---------- */
function tick(ts){
  if(!playing)return;
  if(!last)last=ts;
  var dy=BASE*speed*(ts-last)/1000;
  last=ts;
  var y=pageY(),m=maxScroll();
  if(y>=m-1){stopPlay();return;}
  window.scrollTo(0,Math.min(m,y+dy));
  raf=window.requestAnimationFrame(tick);
}
function startPlay(){playing=true;last=0;pp.textContent='❚❚';pp.title='暂停（空格）';raf=window.requestAnimationFrame(tick);}
function stopPlay(){playing=false;pp.textContent='▶';pp.title='播放（空格）';if(raf)window.cancelAnimationFrame(raf);raf=0;}
pp.addEventListener('click',function(){if(playing)stopPlay();else startPlay();});
spdBtn.addEventListener('click',function(){speed=speed===1?2:1;spdBtn.textContent=speed+'x';});
['wheel','touchstart'].forEach(function(ev){
  window.addEventListener(ev,function(e){
    if(playing&&!(e.target&&e.target.closest&&e.target.closest('.player')))stopPlay();
  },{passive:true});
});
function jump(i){
  var g=geo[i];if(!g)return;
  window.scrollTo({top:Math.max(0,g.top-76),behavior:smooth});
}
markers.forEach(function(mk,i){
  mk.addEventListener('pointerdown',function(e){e.stopPropagation();});
  mk.addEventListener('click',function(e){e.stopPropagation();jump(i);});
});
var dragging=false;
function scrub(x){
  var r=track.getBoundingClientRect();
  var p=Math.min(1,Math.max(0,(x-r.left)/r.width));
  window.scrollTo(0,p*maxScroll());
}
track.addEventListener('pointerdown',function(e){dragging=true;try{track.setPointerCapture(e.pointerId);}catch(err){}scrub(e.clientX);});
track.addEventListener('pointermove',function(e){if(dragging)scrub(e.clientX);});
['pointerup','pointercancel'].forEach(function(ev){track.addEventListener(ev,function(){dragging=false;});});
window.addEventListener('keydown',function(e){
  if(lbOpen()){e.preventDefault();lbKey(e);return;}
  if(!(e.target===doc.body||e.target===doc.documentElement))return;
  if(e.code==='Space'){e.preventDefault();if(playing)stopPlay();else startPlay();}
  else if(e.key==='ArrowRight'){e.preventDefault();jump(Math.min(total-1,currentIdx()+1));}
  else if(e.key==='ArrowLeft'){e.preventDefault();jump(Math.max(0,currentIdx()-1));}
});

/* ---------- Lightbox ---------- */
var lb=doc.getElementById('lightbox');
var lbImg=doc.getElementById('lb-img');
var lbStage=doc.getElementById('lb-stage');
var lbFile=doc.getElementById('lb-file');
var lbStep=doc.getElementById('lb-step');
var lbCount=doc.getElementById('lb-count');
var lbZoom=doc.getElementById('lb-zoom');
var lbFit=doc.getElementById('lb-fit');
var lbPrev=doc.getElementById('lb-prev');
var lbNext=doc.getElementById('lb-next');
var imgs=[].slice.call(doc.querySelectorAll('figure img'));
var items=imgs.map(function(img){
  var fig=img.closest('figure');var st=fig.closest('.step');
  return{src:img.getAttribute('src'),file:img.getAttribute('data-file')||img.getAttribute('src'),no:st.getAttribute('data-no'),name:st.getAttribute('data-name')};
});
var li=-1,z=1,tx=0,ty=0;
function lbOpen(){return lb.classList.contains('open');}
function layoutCenter(){var r=lbStage.getBoundingClientRect();return{x:r.left+r.width/2,y:r.top+r.height/2};}
function clampPan(){
  var w=lbImg.clientWidth||1,h=lbImg.clientHeight||1;
  var lx=Math.max(0,(z-1)*w/2),ly=Math.max(0,(z-1)*h/2);
  tx=Math.min(lx,Math.max(-lx,tx));ty=Math.min(ly,Math.max(-ly,ty));
}
function applyT(){
  lbImg.style.transform='translate('+tx+'px,'+ty+'px) scale('+z+')';
  lbZoom.textContent=Math.round(z*100)+'%';
  lbFit.textContent=z===1?'1:1':'适屏';
  lbImg.classList.toggle('zoomed',z>1);
}
function naturalZ(){var w=lbImg.clientWidth||1;return Math.max(1,lbImg.naturalWidth/w);}
function setZoom(z2,cx,cy){
  z2=Math.min(8,Math.max(1,z2));
  if(cx!=null){
    var L=layoutCenter(),k=z2/z;
    var mx=cx-L.x,my=cy-L.y;
    tx=mx*(1-k)+k*tx;ty=my*(1-k)+k*ty;
  }
  z=z2;clampPan();applyT();
}
function show(i){
  if(!items.length)return;
  li=(i+items.length)%items.length;
  var it=items[li];
  z=1;tx=0;ty=0;
  lbImg.style.transform='translate(0px,0px) scale(1)';
  lbImg.onload=function(){applyT();};
  lbImg.src=it.src;
  lbFile.textContent=it.file;
  lbStep.textContent='#'+it.no+' '+it.name;
  lbCount.textContent=(li+1)+' / '+items.length;
  applyT();
  var one=items.length<2;
  lbPrev.style.display=one?'none':'';
  lbNext.style.display=one?'none':'';
}
function openLb(i){
  if(!items.length)return;
  lb.classList.add('open');
  doc.documentElement.classList.add('locked');
  show(i);
}
function closeLb(){
  lb.classList.remove('open');
  doc.documentElement.classList.remove('locked');
}
imgs.forEach(function(img,i){
  var fig=img.closest('figure');
  fig.addEventListener('click',function(){openLb(i);});
  fig.addEventListener('keydown',function(e){if(e.key==='Enter'||e.key===' '){e.preventDefault();openLb(i);}});
});
doc.getElementById('lb-x').addEventListener('click',closeLb);
lbPrev.addEventListener('click',function(){show(li-1);});
lbNext.addEventListener('click',function(){show(li+1);});
lbFit.addEventListener('click',function(){if(z===1)setZoom(naturalZ());else setZoom(1);});
lb.addEventListener('wheel',function(e){
  e.preventDefault();
  setZoom(z*(e.deltaY<0?1.15:1/1.15),e.clientX,e.clientY);
},{passive:false});
var drag=null,moved=0;
lbImg.addEventListener('pointerdown',function(e){
  drag={x:e.clientX-tx,y:e.clientY-ty};moved=0;
  try{lbImg.setPointerCapture(e.pointerId);}catch(err){}
  e.preventDefault();
});
lbImg.addEventListener('pointermove',function(e){
  if(!drag)return;
  var dx=e.clientX-drag.x,dy=e.clientY-drag.y;
  moved=Math.max(moved,Math.abs(dx-tx)+Math.abs(dy-ty));
  tx=dx;ty=dy;clampPan();applyT();
});
lbImg.addEventListener('pointerup',function(e){
  if(drag&&moved<4){
    if(z===1)setZoom(2.5,e.clientX,e.clientY);
    else setZoom(1);
  }
  drag=null;
});
['pointerup','pointercancel'].forEach(function(ev){lbImg.addEventListener(ev,function(){drag=null;});});
lb.addEventListener('click',function(e){if(e.target===lb)closeLb();});
lbStage.addEventListener('click',function(e){if(e.target===lbStage)closeLb();});
function lbKey(e){
  if(e.key==='Escape')closeLb();
  else if(e.key==='ArrowLeft')show(li-1);
  else if(e.key==='ArrowRight')show(li+1);
  else if(e.key==='+'||e.key==='=')setZoom(z*1.2);
  else if(e.key==='-')setZoom(z/1.2);
  else if(e.key==='0')setZoom(1);
  else if(e.code==='Space')e.preventDefault();
}
/* ---------- 打印态：自动展开日志 ---------- */
var openedLogs=[];
window.addEventListener('beforeprint',function(){
  openedLogs=[].slice.call(doc.querySelectorAll('details.log:not([open])'));
  openedLogs.forEach(function(d){d.setAttribute('open','');});
});
window.addEventListener('afterprint',function(){
  openedLogs.forEach(function(d){d.removeAttribute('open');});
  openedLogs=[];
});
})();
</script>
</body></html>`

const out = join(dir, 'report.html')
writeFileSync(out, html)
console.log(
  `已生成 ${out}（${total} 步 / ${passed} PASS${A ? ` / 断言 ${A.passed}/${A.total}` : ''} / 截图 ${shotsTotal} 张）`,
)
