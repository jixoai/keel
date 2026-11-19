// TEMP DIAGNOSTIC probe: which render functions run on a resize?
import { chromium } from '/Users/kzf/Development/GitHub/jixoai/fde2e/jiudianguanjiademo/apps/client/node_modules/playwright-core/index.mjs'
const base = process.argv[2]
const password = process.env.KEEL_SERVE_PASSWORD
const browser = await chromium.launch({ channel: 'chrome', headless: true })
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
const log = (...a) => console.log(...a)
const counts = { grid: 0, asset: 0, pane: 0, gridLayout: 0 }
page.on('console', (m) => {
  const t = m.text()
  const hit = /__diag (grid|asset|pane|gridLayout)/.exec(t)
  if (hit) counts[hit[1]] += 1
})
const counters = async () => ({ ...counts })

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
await page.waitForTimeout(2000)
const before = await counters()
log('before resize:', JSON.stringify(before))
await page.setViewportSize({ width: 1040, height: 900 })
await page.waitForTimeout(1500)
const after = await counters()
log('after resize :', JSON.stringify(after))
log('deltas:', JSON.stringify({ grid: after.grid - before.grid, asset: after.asset - before.asset, pane: after.pane - before.pane, gridLayout: after.gridLayout - before.gridLayout }))
await browser.close()
