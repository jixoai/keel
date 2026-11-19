// Does a pure signal change (window resize) recreate the gallery's <img>
// elements? Tags every img, counts IMG load events during the resize, and
// re-reads the tags afterwards. Same navigation path as trace-replay.mjs.
import { chromium } from '/Users/kzf/Development/GitHub/jixoai/fde2e/jiudianguanjiademo/apps/client/node_modules/playwright-core/index.mjs'
const base = process.argv[2] ?? 'http://127.0.0.1:13792'
const password = process.env.KEEL_SERVE_PASSWORD
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
const newBtn = page.getByRole('button', { name: /新建会话/ }).first()
if (await newBtn.count()) { await newBtn.click(); await page.waitForTimeout(900) }
await page.locator('[data-testid="session-model-chip"]').first().click()
await page.locator('[data-testid="session-model-picker"]').waitFor({ timeout: 10000 })
await page.waitForTimeout(400)
const row = page.locator('[aria-label="选择模型 Demo Image"]').first()
await row.scrollIntoViewIfNeeded().catch(() => {})
const box = await row.boundingBox()
await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2); await page.mouse.down(); await page.mouse.up()
await page.waitForTimeout(1000)
const apply = page.getByRole('button', { name: '新建会话并应用' }).first()
const ab = await apply.boundingBox()
await page.mouse.click(ab.x + ab.width / 2, ab.y + ab.height / 2)
await page.locator('[data-testid="image-surface"]').waitFor({ timeout: 15000 })
const field = page.locator('[data-testid="image-prompt"] textarea, [data-testid="image-prompt"] input').first()
await field.click(); await field.fill('一只在窗边看雨的猫')
await page.locator('[data-testid="image-generate"]').first().click()
await page.waitForFunction(() => document.querySelectorAll('[data-testid^="image-asset-"]').length >= 2, undefined, { timeout: 30000 })
await page.waitForTimeout(1500)

// tag + count IMG loads from here on
await page.evaluate(() => {
  window.__imgTags = []
  window.__imgLoads = 0
  document.querySelectorAll('img').forEach((n, i) => { n.__probeTag = `t${i}`; window.__imgTags.push(`t${i}`) })
  document.addEventListener('load', (e) => { if (e.target?.tagName === 'IMG') window.__imgLoads += 1 }, true)
})
const before = await page.evaluate(() => ({ tags: [...document.querySelectorAll('img')].map((n) => n.__probeTag ?? null), srcs: [...document.querySelectorAll('img')].map((n) => n.getAttribute('src')) }))
log('before resize:', JSON.stringify(before.tags))

await page.setViewportSize({ width: 1040, height: 900 })
await page.waitForTimeout(1200)
await page.setViewportSize({ width: 1280, height: 900 })
await page.waitForTimeout(1200)

const after = await page.evaluate(() => ({ tags: [...document.querySelectorAll('img')].map((n) => n.__probeTag ?? null), loads: window.__imgLoads, sameCount: document.querySelectorAll('img').length }))
log('after  resize:', JSON.stringify(after.tags))
log('same element identities (no rebuild):', JSON.stringify(before.tags) === JSON.stringify(after.tags))
log('IMG load events during resize:', after.loads)
await page.screenshot({ path: '/tmp/keel-verify/shots-trace/after-resize.png' })
await browser.close()
