// Browser pass over the temp demo instance: login → new session → pick demo-llm
// in the ordinary picker → send one message → the server-side demo provider
// answers. Screenshots + DOM text go to /tmp/keel-verify/shots.
import { chromium } from '/Users/kzf/Development/GitHub/jixoai/fde2e/jiudianguanjiademo/apps/client/node_modules/playwright-core/index.mjs'
import { mkdirSync } from 'node:fs'

const base = process.argv[2] ?? 'http://127.0.0.1:13791'
const password = process.env.KEEL_SERVE_PASSWORD
const SHOTS = '/tmp/keel-verify/shots'
mkdirSync(SHOTS, { recursive: true })

const browser = await chromium.launch({ channel: 'chrome', headless: true })
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
const log = (...a) => console.log(...a)

await page.goto(base + '/', { waitUntil: 'domcontentloaded' })
await page.locator('[data-testid="login-password"]').waitFor({ timeout: 15000 })
const loginInput = page.locator('[data-testid="login-password"] input, input[data-testid="login-password"]').first()
await loginInput.fill(password)
await page.locator('[data-testid="login-submit"]').click()
await page.locator('[data-testid="session-list"]').waitFor({ timeout: 15000 })
log('login ok; workspace text head:', (await page.locator('body').innerText()).replace(/\s+/g, ' ').slice(0, 160))
await page.screenshot({ path: `${SHOTS}/01-workspace.png` })

// new session → the model chip opens the ordinary picker
const newBtn = page.getByRole('button', { name: /新建会话/ }).first()
if (await newBtn.count()) { await newBtn.click(); await page.waitForTimeout(1200) }
const chip = page.locator('[data-testid="session-model-chip"]').first()
await chip.click()
await page.locator('[data-testid="session-model-picker"]').waitFor({ timeout: 10000 })
await page.waitForTimeout(500)
const labels = await page.evaluate(() => [...document.querySelectorAll('[data-testid="session-model-picker"] [aria-label]')].map((n) => n.getAttribute('aria-label') + '|' + (n.innerText ?? '').replace(/\s+/g, ' ')))
log('picker buttons:'); for (const l of labels) log('  ', l)
await page.screenshot({ path: `${SHOTS}/02-picker.png` })

const pickerText = (await page.locator('[data-testid="session-model-picker"]').innerText()).replace(/\s+/g, ' ')
log('picker has Demo group:', /Demo/i.test(pickerText), '| has demo-llm:', /demo-llm/i.test(pickerText))

// choose demo-llm (the row button's accessible name is 选择模型 <display>)
const llm = labels.find((l) => /选择模型 Demo LLM/.test(l)) ?? labels.find((l) => /demo\s*llm/i.test(l))
if (!llm) { log('FAIL: no demo-llm row'); await browser.close(); process.exit(1) }
const aria = llm.split('|')[0]
log('clicking row:', JSON.stringify(aria))
const row = page.locator(`[aria-label="${aria}"]`).first()
await row.scrollIntoViewIfNeeded().catch(() => {})
await page.waitForTimeout(300)
const box = await row.boundingBox()
log('row box:', JSON.stringify(box))
await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
await page.mouse.down()
await page.mouse.up()
await page.waitForTimeout(1500)
log('after pick, chip =', await chip.innerText())
await page.screenshot({ path: `${SHOTS}/03-picked.png` })
const stillOpen = await page.locator('[data-testid="session-model-picker"]').count()
log('picker still open after pick:', stillOpen)
if (stillOpen) {
  const closeBtn = page.locator('[data-testid="session-model-picker"] [aria-label="关闭"]').first()
  if (await closeBtn.count()) { await closeBtn.click({ force: true }).catch(() => {}) }
  else { await page.keyboard.press('Escape') }
  await page.waitForTimeout(800)
}
log('picker count after close:', await page.locator('[data-testid="session-model-picker"]').count())

// send one message through the composer
const input = page.locator('[data-testid="composer-input"] input, input[data-testid="composer-input"], [data-testid="composer-input"] textarea').first()
await input.click()
await input.fill('你好，请用一句话自我介绍。')
await page.locator('[data-testid="composer-send"]').first().click()
// wait for the assistant reply (demo text is fixed)
await page.waitForFunction(
  () => document.body.innerText.includes('这是 keel 服务端直接生成的回复'),
  undefined,
  { timeout: 45000 },
).catch(() => {})
const body = (await page.locator('body').innerText()).replace(/\s+/g, ' ')
log('reply present:', body.includes('这是 keel 服务端直接生成的回复'))
log('user message present:', body.includes('请用一句话自我介绍'))
log('body excerpt:', body.slice(0, 400))
await page.screenshot({ path: `${SHOTS}/04-llm-reply.png`, fullPage: false })
log('no 「演示」 wording on this demo deployment UI:', !body.includes('演示'))
await browser.close()
