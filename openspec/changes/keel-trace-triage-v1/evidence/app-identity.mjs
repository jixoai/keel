// Is a resize's DOM replacement app-wide (framework) or image-surface-only?
// Tags shell elements on the default (chat) screen, resizes, compares.
import { chromium } from '/Users/kzf/Development/GitHub/jixoai/fde2e/jiudianguanjiademo/apps/client/node_modules/playwright-core/index.mjs'
const base = process.argv[2]
const password = process.env.KEEL_SERVE_PASSWORD
const browser = await chromium.launch({ channel: 'chrome', headless: true })
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
await page.goto(base + '/', { waitUntil: 'domcontentloaded' })
await page.locator('[data-testid="login-password"] input').waitFor({ timeout: 15000 })
await page.locator('[data-testid="login-password"] input').fill(password)
await page.locator('[data-testid="login-submit"]').click()
await page.locator('[data-testid="session-list"]').waitFor({ timeout: 15000 })
await page.waitForTimeout(2500)
// open the newest session so the conversation is mounted
const rows = page.locator('[data-testid="session-list"] [role="button"]')
const box = await rows.first().boundingBox()
await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2)
await page.waitForTimeout(1500)
const tag = async (phase) => page.evaluate((phase) => {
  const out = []
  const tagOf = (n) => { if (!n.__p) n.__p = `${phase}-${out.length}`; return n.__p }
  document.querySelectorAll('[data-testid]').forEach((n) => {
    const id = n.getAttribute('data-testid')
    if (/workspace-(shell|sidebar|main|header)|session-list|composer-input|conversation-panel|session-row/.test(id)) out.push({ id, tag: tagOf(n) })
  })
  return out
}, phase)
const before = await tag('b')
await page.setViewportSize({ width: 1040, height: 900 })
await page.waitForTimeout(1500)
const after = await tag('a')
const same = JSON.stringify(before.map((r) => r.tag)) === JSON.stringify(after.map((r) => r.tag))
console.log('elements:', before.length)
console.log('before:', JSON.stringify(before))
console.log('after :', JSON.stringify(after))
console.log('shell identity kept across resize:', same)
await browser.close()
