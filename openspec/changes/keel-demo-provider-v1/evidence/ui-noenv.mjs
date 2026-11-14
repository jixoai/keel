// No-demo audit against the live deployment (13760): login → open an existing
// session (read-only) → open the model picker → screenshot + text assertions.
import { chromium } from '/Users/kzf/Development/GitHub/jixoai/fde2e/jiudianguanjiademo/apps/client/node_modules/playwright-core/index.mjs'
import { mkdirSync } from 'node:fs'

const base = process.argv[2] ?? 'http://127.0.0.1:13760'
const password = process.env.KEEL_SERVE_PASSWORD
const SHOTS = process.argv[3] ?? '/tmp/keel-verify/shots-noenv'
mkdirSync(SHOTS, { recursive: true })

const browser = await chromium.launch({ channel: 'chrome', headless: true })
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
const log = (...a) => console.log(...a)

await page.goto(base + '/', { waitUntil: 'domcontentloaded' })
await page.locator('[data-testid="login-password"] input').waitFor({ timeout: 15000 })
await page.locator('[data-testid="login-password"] input').fill(password)
await page.locator('[data-testid="login-submit"]').click()
await page.locator('[data-testid="session-list"]').waitFor({ timeout: 15000 })
await page.waitForTimeout(2000)
const listText = (await page.locator('body').innerText()).replace(/\s+/g, ' ')
log('workspace has 演示:', listText.includes('演示'), '| has demo (case-insensitive):', /demo/i.test(listText))
await page.screenshot({ path: `${SHOTS}/01-workspace.png` })

// open the most recent session row (read-only: no new session is created)
const rows = page.locator('[data-testid="session-list"] [role="button"]')
log('session rows:', await rows.count())
const target = rows.first()
const box = await target.boundingBox()
if (!box) { log('FAIL: no session row'); await browser.close(); process.exit(1) }
await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2)
await page.waitForTimeout(1500)
const chip = page.locator('[data-testid="session-model-chip"]').first()
log('chip present:', await page.locator('[data-testid="session-model-chip"]').count())
const chipBox = await chip.boundingBox()
await page.mouse.click(chipBox.x + chipBox.width / 2, chipBox.y + chipBox.height / 2)
await page.locator('[data-testid="session-model-picker"]').waitFor({ timeout: 10000 })
await page.waitForTimeout(800)
const pickerText = (await page.locator('[data-testid="session-model-picker"]').innerText()).replace(/\s+/g, ' ')
log('picker has 演示:', pickerText.includes('演示'), '| has Demo group:', /(^|\s)Demo(\s|$)/.test(pickerText), '| demo- in text:', pickerText.includes('demo-'))
const providerGroups = await page.evaluate(() =>
  [...document.querySelectorAll('[data-testid="session-model-picker"] [aria-label]')]
    .map((n) => n.getAttribute('aria-label'))
    .filter((l) => l && !/选择模型|思考档|关闭|恢复默认|本会话|会话选择|标准模式|团队模式|只读/.test(l)),
)
log('provider group labels in picker:', JSON.stringify(providerGroups))
await page.screenshot({ path: `${SHOTS}/02-picker.png` })
const pageText = (await page.locator('body').innerText()).replace(/\s+/g, ' ')
log('whole page has 演示:', pageText.includes('演示'))
log('picker excerpt:', pickerText.slice(0, 300))
await browser.close()
