// Equivalent reproduction of the trace-triage finding path (A01/A03/A05/A06):
// build a real gallery on the image surface (server-side demo provider), then
// record a Chrome trace of the IDLE window so the same metrics can be derived:
// RunTask>=200ms, main-thread busy ratio, frame presented/dropped, layout/paint
// counts, invalidation-tracked node modifications, GC events.
import { chromium } from '/Users/kzf/Development/GitHub/jixoai/fde2e/jiudianguanjiademo/apps/client/node_modules/playwright-core/index.mjs'
import { writeFileSync, mkdirSync } from 'node:fs'

const base = process.argv[2] ?? 'http://127.0.0.1:13792'
const out = process.argv[3] ?? '/tmp/keel-verify/evidence/trace-after.json'
const idleMs = Number(process.argv[4] ?? 6000)
const password = process.env.KEEL_SERVE_PASSWORD
mkdirSync('/tmp/keel-verify/shots-trace', { recursive: true })

const CATEGORIES = [
  "blink,benchmark,rail,disabled-by-default-blink.debug.layout",
  "benchmark,rail",
  "benchmark,latencyInfo,rail,input.scrolling",
  "blink,devtools.timeline",
  "blink.user_timing",
  "blink,rail",
  "cc,benchmark,disabled-by-default-devtools.timeline.frame",
  "cc,disabled-by-default-devtools.timeline",
  "cppgc",
  "devtools.timeline",
  "devtools.timeline,disabled-by-default-v8.gc",
  "devtools.timeline,v8",
  "disabled-by-default-devtools.screenshot",
  "disabled-by-default-devtools.timeline",
  "disabled-by-default-devtools.timeline.frame",
  "disabled-by-default-devtools.v8-source-rundown",
  "disabled-by-default-devtools.v8-source-rundown-sources",
  "disabled-by-default-v8.cpu_profiler",
  "disabled-by-default-v8.inspector",
  "loading",
  "v8",
  "v8.execute",
  "benchmark",
].join(",")

const browser = await chromium.launch({ channel: 'chrome', headless: true })
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
const log = (...a) => console.log(...a)

await page.goto(base + '/', { waitUntil: 'domcontentloaded' })
await page.locator('[data-testid="login-password"] input').waitFor({ timeout: 15000 })
await page.locator('[data-testid="login-password"] input').fill(password)
await page.locator('[data-testid="login-submit"]').click()
await page.locator('[data-testid="session-list"]').waitFor({ timeout: 15000 })
await page.waitForTimeout(1200)

// new session → open the picker → choose Demo Image (a cross-surface model:
// the client creates a fresh session and mounts the image surface)
const newBtn = page.getByRole('button', { name: /新建会话/ }).first()
if (await newBtn.count()) { await newBtn.click(); await page.waitForTimeout(1000) }
await page.locator('[data-testid="session-model-chip"]').first().click()
await page.locator('[data-testid="session-model-picker"]').waitFor({ timeout: 10000 })
await page.waitForTimeout(400)
const row = page.locator('[aria-label="选择模型 Demo Image"]').first()
await row.scrollIntoViewIfNeeded().catch(() => {})
await page.waitForTimeout(250)
const box = await row.boundingBox()
log('Demo Image row box:', JSON.stringify(box))
await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
await page.mouse.down(); await page.mouse.up()
await page.waitForTimeout(1200)
// cross-surface switch: the confirm card offers 「新建会话并应用」
const apply = page.getByRole('button', { name: '新建会话并应用' }).first()
if (await apply.count()) {
  const abox = await apply.boundingBox()
  log('apply box:', JSON.stringify(abox))
  if (abox) { await page.mouse.click(abox.x + abox.width / 2, abox.y + abox.height / 2) }
} else {
  const close = page.locator('[data-testid="session-model-picker"] [aria-label="关闭"]').first()
  if (await close.count()) await close.click({ force: true }).catch(() => {})
}
await page.locator('[data-testid="image-surface"]').waitFor({ timeout: 15000 })
log('image surface mounted; picker count:', await page.locator('[data-testid="session-model-picker"]').count())
const promptField = page.locator('[data-testid="image-prompt"] textarea, [data-testid="image-prompt"] input, [data-testid="image-prompt"] [contenteditable="true"]').first()
if (await promptField.count()) {
  await promptField.click()
  await promptField.fill('一只在窗边看雨的猫')
} else {
  const wrap = page.locator('[data-testid="image-prompt"]').first()
  const wbox = await wrap.boundingBox()
  await page.mouse.click(wbox.x + wbox.width / 2, wbox.y + wbox.height / 2)
  await page.keyboard.type('一只在窗边看雨的猫')
}
log('prompt typed, generate disabled?', await page.locator('[data-testid="image-generate"]').first().isDisabled().catch(() => 'n/a'))
await page.waitForTimeout(400)
// count = 2 (default is 2 already), generate
await page.locator('[data-testid="image-generate"]').first().click()
await page.waitForFunction(
  () => document.querySelectorAll('[data-testid^="image-asset-"]').length >= 2,
  undefined,
  { timeout: 30000 },
)
log('gallery assets:', await page.locator('[data-testid^="image-asset-"]').count())
const imgSrcs = await page.evaluate(() => [...document.querySelectorAll('img')].map((n) => n.getAttribute('src')))
log('img srcs:', JSON.stringify(imgSrcs))
const pageText = (await page.locator('body').innerText()).replace(/\s+/g, ' ')
log('surface text checks: 演示=', pageText.includes('演示'), '| demo data url=', imgSrcs.some((s) => /surfaces\/(image|realtime)|aurora|prism|tidal|bloom/.test(s ?? '')), '| panel badge 已完成=', pageText.includes('已完成'))
await page.screenshot({ path: '/tmp/keel-verify/shots-trace/gallery.png' })
await page.waitForTimeout(2500) // let the post-generation layout settle

// ---- the idle trace window ------------------------------------------------
const cdp = await browser.newBrowserCDPSession()
const chunks = []
cdp.on('Tracing.dataCollected', (e) => chunks.push(...e.value))
const done = new Promise((resolve) => cdp.on('Tracing.tracingComplete', resolve))
await cdp.send('Tracing.start', { transferMode: 'ReportEvents', categories: CATEGORIES })
await page.waitForTimeout(idleMs)
// positive control: a hover repaint must show up as Paint/layout events, so a
// zero count in the idle window above is a real zero, not a disabled category
await cdp.send('Tracing.recordClockSyncMarker', { syncId: 'control-start' }).catch(() => {})
const asset = page.locator('[data-testid^="image-asset-"]').first()
const ab = await asset.boundingBox()
if (ab) {
  await page.mouse.move(ab.x + ab.width / 2, ab.y + ab.height / 2, { steps: 8 })
  await page.mouse.move(ab.x + ab.width / 2 + 20, ab.y + ab.height / 2, { steps: 8 })
}
await page.waitForTimeout(1500)
await cdp.send('Tracing.end')
await done
const trace = { traceEvents: chunks }
writeFileSync(out, JSON.stringify(trace))
log('trace events:', chunks.length, '->', out)
await page.screenshot({ path: '/tmp/keel-verify/shots-trace/idle-end.png' })
await browser.close()
