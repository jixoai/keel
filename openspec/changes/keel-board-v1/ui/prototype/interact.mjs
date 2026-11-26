// Interaction checks for the keel-board task-face prototype.
//
// Usage:
//   PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node interact.mjs
//
// These are prototype-level checks (fixture state changes), not production E2E:
// they prove the designed action set, confirmations, read-only faces and mobile
// sheet are reachable and do not throw.
import assert from "node:assert/strict";
import { writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const modulePath = process.env.PLAYWRIGHT_MODULE || "playwright";
const { chromium } = await import(modulePath);
const base = pathToFileURL(join(here, "index.html")).href;
const browser = await chromium.launch({ headless: true, channel: "chrome" });
const errors = [];
const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
page.on("pageerror", (error) => errors.push(`pageerror: ${error.message}`));
page.on("console", (message) => { if (message.type() === "error") errors.push(`console: ${message.text()}`); });
const logLines = [];
const pass = (name) => { console.log(`PASS ${name}`); logLines.push(`PASS ${name}`); };

await page.goto(`${base}?chrome=0&scene=queue`);
await page.waitForTimeout(250);
assert.equal(await page.locator(".row").count(), 8);
await page.locator('[data-action="load-more"]').click();
await page.waitForTimeout(200);
assert.equal(await page.locator(".row").count(), 14);
pass("queue renders the oldest-first first page and paginates");

// Claim a ready task.
await page.locator('[data-action="open-task"][data-id="809"]').click();
await page.waitForTimeout(200);
assert.equal(await page.locator(".task-card-head .badge").first().innerText(), "可认领");
await page.locator('[data-action="claim"]').click();
await page.waitForTimeout(250);
assert.equal(await page.locator(".task-card-head .badge").first().innerText(), "已认领");
assert.match(await page.locator("[data-countdown]").first().innerText(), /^\d{2}:\d{2}$/);
pass("claim moves ready -> claimed with a lease countdown");

// Complete a claimed task through its confirmation.
await page.goto(`${base}?chrome=0&scene=task&id=812`);
await page.waitForTimeout(250);
await page.locator('[data-action="dialog"][data-dialog="complete"]').click();
await page.waitForTimeout(150);
assert.equal(await page.locator(".dialog h2").innerText(), "完成 #812");
await page.locator("#d-result").fill("幂等键已覆盖提交链路，回归测试通过。");
await page.locator('[data-action="confirm"][data-kind="complete"]').click();
await page.waitForTimeout(300);
assert.equal(await page.locator(".task-card-head .badge").first().innerText(), "已完成");
assert.match(await page.locator(".toast").last().innerText(), /已完成 #812/);
assert.equal(await page.evaluate(() => eval("taskById(\"846\")").state), "ready");
pass("complete writes the result, readies the dependent and reports it");

// Answer a mirrored blocked task.
await page.goto(`${base}?chrome=0&scene=task&id=814`);
await page.waitForTimeout(250);
await page.locator('[data-action="dialog"][data-dialog="answer"]').click();
await page.waitForTimeout(150);
await page.locator("#d-answer").fill("按状态分文案：认领、阻塞、完成各一条。");
await page.locator('[data-action="confirm"][data-kind="answer"]').click();
await page.waitForTimeout(300);
assert.equal(await page.locator(".task-card-head .badge").first().innerText(), "可认领");
assert.equal(await page.locator(".reply").count(), 3);
pass("answer unblocks a mirrored task and appends one reply");

// Reply through the composer.
await page.goto(`${base}?chrome=0&scene=task&id=812`);
await page.waitForTimeout(250);
const beforeReply = await page.locator(".reply").count();
await page.locator("#composer-input").fill("已确认灰度 5%，30 分钟无重复。");
await page.locator('[data-action="send-reply"]').click();
await page.waitForTimeout(800);
assert.equal(await page.locator(".reply").count(), beforeReply + 1);
pass("composer appends exactly one reply");

// State tabs and a filter.
await page.goto(`${base}?chrome=0&scene=queue`);
await page.waitForTimeout(250);
await page.locator('[data-action="tab"][data-tab="completed"]').click();
await page.waitForTimeout(200);
assert.equal(await page.locator(".row").count(), 4);
await page.locator('[data-action="tab"][data-tab="failed"]').click();
await page.waitForTimeout(200);
assert.equal(await page.locator(".row").count(), 2);
await page.locator('[data-action="tab"][data-tab="open"]').click();
await page.waitForTimeout(200);
await page.locator(".filter-wrap.desktop-only [data-action=\"menu\"]").first().click();
await page.waitForTimeout(100);
await page.locator('[data-action="filter"][data-key="channel"][data-value="work/release"]').click();
await page.waitForTimeout(250);
assert.equal(await page.locator(".filter-tag").count(), 1);
assert.ok((await page.locator(".row").count()) > 0);
pass("state tabs and the channel filter narrow the fixture");

// Claim-lost and read-only faces hide the settlement actions.
await page.goto(`${base}?chrome=0&scene=task&id=812&claim=lost`);
await page.waitForTimeout(250);
assert.equal(await page.locator('[data-action="dialog"][data-dialog="complete"]').count(), 0);
assert.match((await page.locator(".task-card").innerText()), /认领凭据不在本标签页/);
await page.goto(`${base}?chrome=0&scene=task&id=809&me=reader`);
await page.waitForTimeout(250);
assert.equal(await page.locator('[data-action="claim"]').count(), 0);
assert.equal(await page.locator('[data-action="send-reply"]').count(), 0);
pass("claim-lost and read-only keep settlement actions hidden");

// Sync automation toggles.
await page.goto(`${base}?chrome=0&scene=automation`);
await page.waitForTimeout(250);
const watcher = page.locator('[data-action="toggle-watcher"][data-id="triage-inbox"]');
assert.equal(await watcher.getAttribute("aria-checked"), "true");
await watcher.click();
await page.waitForTimeout(200);
assert.equal(await watcher.getAttribute("aria-checked"), "false");
const schedule = page.locator('[data-action="toggle-schedule"][data-id="晨间队列摘要"]');
assert.equal(await schedule.getAttribute("aria-checked"), "true");
await schedule.click();
await page.waitForTimeout(200);
assert.equal(await schedule.getAttribute("aria-checked"), "false");
pass("watcher and schedule switches toggle");

// Mobile filter sheet and touch heights.
await page.setViewportSize({ width: 390, height: 844 });
await page.goto(`${base}?chrome=0&scene=queue`);
await page.waitForTimeout(250);
await page.locator('[data-action="dialog"][data-dialog="filters"]').click();
await page.waitForTimeout(200);
assert.equal(await page.locator(".dialog h2").innerText(), "筛选任务");
assert.equal(await page.locator("#f-channel").evaluate((el) => getComputedStyle(el.parentElement).height), "44px");
assert.equal(await page.locator('.dialog .actions .btn[data-action="dialog-close"]').evaluate((el) => getComputedStyle(el).height), "44px");
pass("mobile filter sheet uses 44px fields and actions");

// P1 / P0 faces: loading, stale, empty badges, assigned, read-only claim,
// archive, migration, 404, claim-lost recovery, six-field NewTask, access.
await page.goto(`${base}?chrome=0&scene=queue&state=loading`);
await page.waitForTimeout(200);
assert.equal(await page.locator(".tabs-tray").count(), 1);
assert.equal(await page.locator(".filter-bar").count(), 1);
assert.ok((await page.locator(".skeleton-row").count()) >= 3);
pass("queue loading keeps tabs/filter and shows skeletons");

await page.goto(`${base}?chrome=0&scene=queue&state=stale`);
await page.waitForTimeout(200);
const staleMeta = await page.locator(".queue-meta").innerText();
assert.match(staleMeta, /最后更新 14:02:11/);
assert.doesNotMatch(staleMeta, /实时更新/);
pass("stale queue does not claim live-updating");

await page.goto(`${base}?chrome=0&scene=sync&state=empty`);
await page.waitForTimeout(200);
assert.equal(await page.locator('[data-view="sync"] .count').innerText(), "0");
await page.goto(`${base}?chrome=0&scene=automation&state=empty`);
await page.waitForTimeout(200);
assert.equal(await page.locator('[data-view="automation"] .count').innerText(), "0");
pass("sync/automation empty states report zero issues");

await page.goto(`${base}?chrome=0&scene=task&id=824`);
await page.waitForTimeout(200);
assert.match(await page.locator(".task-card").innerText(), /仅限 审计员·阿岚/);
pass("assigned ready names the only allowed claimant");

await page.goto(`${base}?chrome=0&scene=task&id=812&me=reader`);
await page.waitForTimeout(200);
const readOnlyCard = await page.locator(".task-card").innerText();
assert.doesNotMatch(readOnlyCard, /由本标签页持有/);
assert.match(readOnlyCard, /持有者 陈墨/);
pass("read-only never claims the tab holds a claim");

await page.goto(`${base}?chrome=0&scene=queue&archive=1`);
await page.waitForTimeout(200);
assert.equal(await page.locator(".row").count(), 1);
assert.equal(await page.locator(".archive-banner").count(), 1);
await page.goto(`${base}?chrome=0&scene=task&id=852`);
await page.waitForTimeout(200);
assert.match(await page.locator(".task-card").innerText(), /归档任务只读/);
assert.equal(await page.locator('[data-action="send-reply"]').count(), 0);
assert.equal(await page.locator('[data-action="dialog"][data-dialog="complete"]').count(), 0);
pass("archived tasks are read-only and actionless");

await page.goto(`${base}?chrome=0&scene=queue&state=migrate`);
await page.waitForTimeout(200);
assert.match(await page.locator("#main").innerText(), /任务面已升级/);
assert.equal(await page.locator(".tabs-tray").count(), 0);
await page.goto(`${base}?chrome=0&scene=queue&state=permission&me=none`);
await page.waitForTimeout(200);
assert.match(await page.locator("#main").innerText(), /任务面不存在/);
assert.equal(await page.locator(".tabs-tray").count(), 0);
pass("migration and no-read 404 are explicit, not silent");

await page.goto(`${base}?chrome=0&scene=task&id=812&claim=lost`);
await page.waitForTimeout(200);
assert.equal(await page.locator('[data-action="takeover"]').count(), 1);
assert.equal(await page.locator('[data-action="dialog"][data-dialog="force-release"]').count(), 1);
await page.locator('[data-action="takeover"]').click();
await page.waitForTimeout(250);
assert.equal(await page.locator('[data-action="dialog"][data-dialog="complete"]').count(), 1);
assert.match(await page.locator(".task-card").innerText(), /由本标签页持有/);
pass("claim-lost offers takeover and force release and recovers");

await page.goto(`${base}?chrome=0&scene=queue&dialog=new`);
await page.waitForTimeout(200);
assert.equal(await page.locator(".dialog .form-row").count(), 6);
assert.equal(await page.locator('[data-action="toggle-new-claim"]').count(), 1);
assert.equal(await page.locator(".dialog.full").count(), 1);
pass("new task sheet has six fields and is full-screen on mobile");

await page.goto(`${base}?chrome=0&scene=task&id=812&dialog=access`);
await page.waitForTimeout(200);
assert.equal(await page.locator(".dialog input").count(), 0);
assert.match(await page.locator(".dialog").innerText(), /不能在这里授权/);
pass("channel access sheet is read-only without an API");

// Mobile 44px tab/chip/switch targets.
await page.setViewportSize({ width: 390, height: 844 });
await page.goto(`${base}?chrome=0&scene=queue`);
await page.waitForTimeout(200);
assert.equal(await page.locator(".tab").first().evaluate((el) => Math.round(el.getBoundingClientRect().height)), 44);
assert.equal(await page.locator(".chip").first().evaluate((el) => Math.round(el.getBoundingClientRect().height)), 44);
await page.goto(`${base}?chrome=0&scene=sync`);
await page.waitForTimeout(200);
assert.equal(await page.locator(".switch").first().evaluate((el) => Math.round(el.getBoundingClientRect().height)), 44);
pass("mobile tabs, chips and switches meet the 44px floor");

assert.deepEqual(errors, []);
pass("no page or console errors");
const log = [
  "# keel-board task-face prototype interaction log",
  `generatedAt: ${new Date().toISOString()}`,
  `node: ${process.version}`,
  `playwright: ${modulePath}`,
  `base: ${base}`,
  "",
  ...logLines,
  "",
  `errors: ${errors.length}`
].join("\n");
await writeFile(join(here, "interact.log"), log + "\n");
await browser.close();
console.log(`\n${errors.length} errors`);
