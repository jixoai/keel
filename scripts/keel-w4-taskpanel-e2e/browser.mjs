#!/usr/bin/env node
// Browser evidence for the run surface's task panel (change keel-harness-v2,
// W4). Drives the packaged web build on a real Chrome against the development
// transport (`?fixture=1`): the fixture carries a real in-memory run ledger, a
// task tree with a sub-agent and a fork member, and two attached references, so
// every screenshot shows the panel reading data — not a mock.
//
//   package → serve → 客服团队 → 启动 → 运行页 → 任务面板（图 / 事件流 / 资源）
//
// It writes one PNG per step plus report.json (assertions, overflow
// measurements and console errors). No credential is read or written: the
// fixture launch needs none.
//
// Env:
//   E2E_BASE      http://127.0.0.1:<port> (the served build; `?fixture=1`)
//   E2E_EVIDENCE  output directory (created)
//   PLAYWRIGHT_CORE / E2E_CHROME  optional overrides
import fs from 'node:fs';
import path from 'node:path';

const BASE = (process.env.E2E_BASE || 'http://127.0.0.1:13752').replace(/\/$/, '');
const EVIDENCE = process.env.E2E_EVIDENCE || '/tmp/w4-task-panel';
const CHROME = process.env.E2E_CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const PLAYWRIGHT =
  process.env.PLAYWRIGHT_CORE || '/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs';

fs.mkdirSync(EVIDENCE, { recursive: true });

const playwright = await import(PLAYWRIGHT);
const { chromium } = playwright;
const browser = await chromium.launch({ executablePath: CHROME, headless: true });

const report = {
  lane: 'W4-task-panel',
  base: BASE,
  startedAt: new Date().toISOString(),
  steps: [],
  console: [],
  pageErrors: [],
  failedRequests: [],
  pass: false,
};
let seq = 0;

const note = (label, kind, ok, value) => report.steps.push({ label, kind, ok, value });
/** The host renders `testID` as `data-testid` (web/host.js), the same identity
 * the unit suites press. */
const id = (testId) => `[data-testid="${testId}"]`;
/** The fixture's own stable ids (the same hash the development transport
 * computes): the run page's deep link names the captain session. */
const FIXTURE_TEAM = 'bfb7f85f-1111-4111-8111-bfb7f85fbfb7';
const FIXTURE_RUN = '0c545de2-1111-4111-8111-0c545de20c54';
const label = (text) => `[aria-label="${text}"]`;

async function shot(page, label, file) {
  await page.screenshot({ path: path.join(EVIDENCE, file), fullPage: true });
  note(label, 'screenshot', true, { file });
}

async function overflow(page, label) {
  const value = await page.evaluate(() => ({
    innerWidth: window.innerWidth,
    docScrollWidth: document.documentElement.scrollWidth,
    docClientWidth: document.documentElement.clientWidth,
    bodyScrollWidth: document.body.scrollWidth,
  }));
  const ok = value.docScrollWidth <= value.innerWidth && value.bodyScrollWidth <= value.innerWidth;
  note(label, 'overflow', ok, value);
  return ok;
}

async function assertText(page, label, text, expected = true) {
  const body = (await page.evaluate(() => document.body.innerText || '')).replace(/\s+/g, ' ');
  const found = body.includes(text);
  note(label, 'assert', found === expected, { text, found });
  return found === expected;
}

const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
const page = await context.newPage();
page.on('console', (message) => {
  if (message.type() === 'error') report.console.push(message.text());
});
page.on('pageerror', (error) => report.pageErrors.push(String(error && error.stack ? error.stack : error)));
page.on('requestfailed', (request) => {
  // The static server has no favicon; that request is not an app failure.
  if (!request.url().endsWith('/favicon.ico')) {
    report.failedRequests.push({ url: request.url(), error: request.failure()?.errorText });
  }
});

try {
await page.goto(`${BASE}/?fixture=1`, { waitUntil: 'domcontentloaded' });
await page.waitForTimeout(1200);
// The chapter rail is the app's own navigation; the fixture launches the books
// shell. A guard here keeps a layout change from producing a green run that
// screenshotted the wrong page.
note('workspace shell', 'assert', await page.locator('text=工作区').first().isVisible(), { text: '工作区' });

// ---- 1. the team page and a started run ----------------------------------

// 团队 is a chapter of the books shell (the left rail at this width).
await page.locator(id('books-chapter-teams')).first().click();
await page.waitForTimeout(700);
await page.locator(label('打开团队 客服团队')).first().click();
await page.waitForTimeout(900);
note('team page', 'assert', await page.locator(id('team-roster')).first().isVisible(), { text: 'team-roster' });

await page.locator(id('team-start')).first();
report.steps.push({ label: 'team-start count', kind: 'value', ok: true, value: { count: await page.locator(id('team-start')).count(), disabled: await page.locator(id('team-start')).first().isDisabled().catch(() => null) } });
await page.locator(id('team-start')).first().click();
await page.waitForTimeout(700);
report.steps.push({ label: 'dialog visible', kind: 'value', ok: true, value: { card: await page.locator(id('team-start-dialog-card')).count(), confirm: await page.locator(id('team-start-dialog-confirm')).count() } });
await page.locator(id('team-start-dialog-confirm')).first().click({ timeout: 8000 });
await page.waitForTimeout(2000);
report.steps.push({ label: 'start receipt notice', kind: 'assert', ok: await page.locator('text=团队已启动').first().isVisible(), value: {} });
// The run page's own deep link: `/teams/:teamId/runs/:runId`, the captain
// session's id (the fixture's stable id). Loading it directly proves the run
// surface resolves a run without a start receipt and reads the same ledger.
// The team page keeps its own run index (03 运行): the start receipt's row
// lives there, and pressing it opens `/teams/:id/runs/:runId` in-app.
await page.locator(id('team-section-runs')).first().click();
await page.waitForTimeout(1000);
report.steps.push({ label: 'run rows', kind: 'value', ok: true, value: { rows: await page.locator('[data-testid^="team-run-"]').count(), url: page.url() } });
if (await page.locator('[data-testid^="team-run-"]').count()) {
  await page.locator('[data-testid^="team-run-"]').first().click();
}
await page.waitForTimeout(1500);
await page.locator(id('run-page')).first().waitFor({ timeout: 20000 });
note('run page', 'assert', await page.locator('text=任务面板').first().isVisible(), { text: '任务面板' });
await shot(page, 'run page with panel entry', 'run-entry-1440.png');
await overflow(page, 'run-entry-1440');

// ---- 2. the panel's three sections ---------------------------------------

await page.locator(id('run-task-panel-entry')).first().click();
await page.waitForTimeout(900);
note('panel tree', 'assert', await page.locator(id('task-panel')).first().isVisible(), { text: 'task-panel' });
note('panel card', 'assert', await page.locator(id('task-panel-card-task-captain')).first().isVisible(), { text: 'task-captain' });
note('panel fork', 'assert', await page.locator(id('task-panel-fork-task-refund-fork')).first().isVisible(), { text: 'task-refund-fork' });
await shot(page, 'task panel graph 1440', 'task-panel-graph-1440.png');
await overflow(page, 'task-panel-graph-1440');

await page.locator(id('task-panel-section-events')).first().click();
await page.waitForTimeout(600);
note('panel events', 'assert', await page.locator(id('task-panel-events')).first().isVisible(), { text: 'task-panel-events' });
await shot(page, 'task panel events 1440', 'task-panel-events-1440.png');
await overflow(page, 'task-panel-events-1440');

await page.locator(id('task-panel-section-resources')).first().click();
await page.waitForTimeout(600);
note('panel resources', 'assert', await page.locator(id('task-panel-resources')).first().isVisible(), { text: 'task-panel-resources' });
await page.locator(id('task-panel-resource-reports/refund-analysis.md')).first().click();
await page.waitForTimeout(900);
note('resource preview', 'assert', await page.locator(id('task-panel-preview')).first().isVisible(), { text: 'task-panel-preview' });
note('resource match', 'assert', await page.locator('text=与引用一致').first().isVisible(), { text: '与引用一致' });
await shot(page, 'task panel preview 1440', 'task-panel-preview-1440.png');
await overflow(page, 'task-panel-preview-1440');

// The manifest's own attribution (lastTaskId): the summary card's CSV has no
// attach event, so the file group is the only place it is named. A file the
// attachment group already lists must not be printed twice.
note('file group', 'assert', await page.locator(id('task-panel-files')).first().isVisible(), { text: 'task-panel-files' });
note('attributed file', 'assert', await page.locator(id('task-panel-file-reports/refund-by-channel.csv')).first().isVisible(), { text: 'reports/refund-by-channel.csv' });
const repeated = await page.locator(id('task-panel-file-reports/refund-analysis.md')).count();
note('attached file not repeated', 'assert', repeated === 0, { count: repeated });
await page.locator(id('task-panel-file-reports/refund-by-channel.csv')).first().click();
await page.waitForTimeout(900);
// The manifest row carries no reference hash: the preview answers the current
// bytes and badges them as such instead of claiming a comparison.
note('manifest preview', 'assert', await page.locator('text=清单当前版本').first().isVisible(), { text: '清单当前版本' });
note('manifest preview content', 'assert', await page.locator('text=渠道,退款单,占比').first().isVisible(), { text: '渠道,退款单,占比' });
await shot(page, 'task panel attributed file 1440', 'task-panel-resources-after-1440.png');
await overflow(page, 'task-panel-resources-after-1440');

// ---- 3. the phone frame ---------------------------------------------------

await page.setViewportSize({ width: 390, height: 844 });
await page.waitForTimeout(900);
await page.screenshot({ path: path.join(EVIDENCE, 'task-panel-390.png'), fullPage: true });
note('380 phone screenshot', 'screenshot', true, { file: 'task-panel-390.png' });
await overflow(page, 'task-panel-390');
// The entry stays one line at 390: the panel is a bottom sheet, and the page
// has no third column to squeeze.
note('phone panel entry', 'assert', await page.locator(id('run-task-panel-entry')).first().isVisible(), { text: 'run-task-panel-entry' });

} catch (error) {
  note(`failed: ${error && error.message ? error.message.slice(0, 400) : String(error)}`, 'assert', false, {
    url: page.url(),
  });
}
report.pass = report.steps.every((step) => step.ok !== false) && report.pageErrors.length === 0;
report.finishedAt = new Date().toISOString();
fs.writeFileSync(path.join(EVIDENCE, 'report.json'), JSON.stringify(report, null, 2));
await browser.close();
if (!report.pass) {
  console.error(`W4 task panel evidence failed: ${JSON.stringify({ console: report.console, pageErrors: report.pageErrors }, null, 2)}`);
  process.exit(1);
}
console.log(`W4 task panel evidence: ${EVIDENCE}`);
void seq;
