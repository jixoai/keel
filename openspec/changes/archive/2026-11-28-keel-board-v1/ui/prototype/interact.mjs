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
assert.equal(await page.locator(".row").count(), 3);
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
assert.match(await page.locator("#main").innerText(), /任务面不可用/);
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

// P1-1: keyboard and confirmAction must respect TaskView.moves.
await page.setViewportSize({ width: 1440, height: 1000 });
const stateOf = (id) => page.evaluate((taskId) => eval(`taskById("${taskId}").state`), id);
const movesOf = (id) => page.evaluate((taskId) => eval(`taskById("${taskId}").moves`), id);
await page.goto(`${base}?chrome=0&scene=task&id=806`);
await page.waitForTimeout(200);
assert.deepEqual(await movesOf("806"), ["reply"]);
await page.keyboard.press("x");
await page.waitForTimeout(150);
assert.equal(await page.locator(".dialog").count(), 0);
assert.equal(await stateOf("806"), "completed");
await page.goto(`${base}?chrome=0&scene=task&id=809`);
await page.waitForTimeout(200);
await page.keyboard.press("e");
await page.waitForTimeout(150);
assert.equal(await page.locator(".dialog").count(), 0);
assert.equal(await stateOf("809"), "ready");
await page.goto(`${base}?chrome=0&scene=task&id=830`);
await page.waitForTimeout(200);
assert.equal((await movesOf("830")).includes("block"), false);
await page.keyboard.press("b");
await page.waitForTimeout(150);
assert.equal(await page.locator(".dialog").count(), 0);
await page.goto(`${base}?chrome=0&scene=task&id=812`);
await page.waitForTimeout(200);
await page.keyboard.press("e");
await page.waitForTimeout(150);
assert.match(await page.locator(".dialog h2").innerText(), /完成 #812/);
await page.keyboard.press("Escape");
await page.goto(`${base}?chrome=0&scene=task&id=806&dialog=cancel`);
await page.waitForTimeout(200);
assert.equal(await page.locator(".dialog").count(), 0);
assert.equal(await stateOf("806"), "completed");
await page.goto(`${base}?chrome=0&scene=task&id=812&dialog=complete`);
await page.waitForTimeout(200);
await page.evaluate(() => { eval('taskById("812")').moves = []; });
await page.locator('[data-action="confirm"][data-kind="complete"]').click();
await page.waitForTimeout(200);
assert.equal(await stateOf("812"), "claimed");
assert.equal(await page.locator(".dialog").count(), 0);
pass("keyboard, dialog= and confirmAction are fenced by moves");

// P1-2: desktop NewTask is a 480px right sheet with a visible footer; mobile is full screen.
await page.goto(`${base}?chrome=0&scene=queue&dialog=new`);
await page.waitForTimeout(250);
const sheet = await page.locator(".dialog.sheet").evaluate((el) => {
  const r = el.getBoundingClientRect();
  return { left: r.left, right: r.right, width: r.width, height: r.height };
});
const desktopFooter = await page.locator('[data-action="confirm"][data-kind="new"]').evaluate((el) => el.getBoundingClientRect().bottom);
assert.ok(sheet.width >= 470 && sheet.width <= 490);
assert.ok(sheet.left > 700);
assert.ok(desktopFooter <= 1000);
await page.setViewportSize({ width: 390, height: 844 });
await page.goto(`${base}?chrome=0&scene=queue&dialog=new`);
await page.waitForTimeout(250);
const full = await page.locator(".dialog.full").evaluate((el) => { const r = el.getBoundingClientRect(); return { top: r.top, height: r.height }; });
const mobileFooter = await page.locator('[data-action="confirm"][data-kind="new"]').evaluate((el) => el.getBoundingClientRect().bottom);
assert.ok(Math.abs(full.height - 844) < 2);
assert.ok(mobileFooter <= 844);
pass("NewTask is a right sheet on desktop and full screen on mobile with visible footer");

// P1-3: mobile last content stays above the tab bar / Composer.
await page.goto(`${base}?chrome=0&scene=task&id=812`);
await page.waitForTimeout(250);
const primaryRect = await page.locator(".action-primary .btn").first().evaluate((el) => el.getBoundingClientRect().toJSON());
assert.ok(primaryRect.top > 0 && primaryRect.bottom <= 788);
const sendHeight = await page.locator('.composer-bar [data-action="send-reply"]').evaluate((el) => Math.round(el.getBoundingClientRect().height));
assert.equal(sendHeight, 44);
await page.evaluate(() => { const paper = document.getElementById("paper"); paper.scrollTop = paper.scrollHeight; });
await page.waitForTimeout(150);
const lastReply = await page.locator(".reply").last().evaluate((el) => el.getBoundingClientRect().bottom);
const composerTop = await page.locator(".composer").evaluate((el) => el.getBoundingClientRect().top);
assert.ok(lastReply <= composerTop - 4, `reply ${lastReply} composer ${composerTop}`);
await page.goto(`${base}?chrome=0&scene=automation`);
await page.waitForTimeout(250);
await page.evaluate(() => { const paper = document.getElementById("paper"); paper.scrollTop = paper.scrollHeight; });
await page.waitForTimeout(150);
const lastAuto = await page.locator(".auto-row").last().locator(".btn").last().evaluate((el) => el.getBoundingClientRect().bottom);
assert.ok(lastAuto <= 788, `automation ${lastAuto}`);
await page.goto(`${base}?chrome=0&scene=sync`);
await page.waitForTimeout(250);
await page.evaluate(() => { const paper = document.getElementById("paper"); paper.scrollTop = paper.scrollHeight; });
await page.waitForTimeout(150);
const lastDead = await page.locator(".dead-row .btn").last().evaluate((el) => el.getBoundingClientRect().bottom);
assert.ok(lastDead <= 788, `dead ${lastDead}`);
pass("mobile tab/Composer never cover the last action or reply");

// P1-4: page-specific error copy and retained data.
await page.setViewportSize({ width: 1440, height: 1000 });
await page.goto(`${base}?chrome=0&scene=queue&state=error`);
await page.waitForTimeout(200);
assert.ok((await page.locator(".row").count()) > 0);
assert.match(await page.locator("#main").innerText(), /你的筛选还在/);
await page.goto(`${base}?chrome=0&scene=task&id=812&state=error`);
await page.waitForTimeout(200);
assert.equal(await page.locator(".task-card").count(), 1);
assert.doesNotMatch(await page.locator("#main").innerText(), /你的筛选还在/);
await page.goto(`${base}?chrome=0&scene=sync&state=error`);
await page.waitForTimeout(200);
assert.ok((await page.locator(".sync-card").count()) > 0);
assert.doesNotMatch(await page.locator("#main").innerText(), /你的筛选还在/);
await page.goto(`${base}?chrome=0&scene=automation&state=error`);
await page.waitForTimeout(200);
assert.ok((await page.locator(".auto-row").count()) > 0);
assert.doesNotMatch(await page.locator("#main").innerText(), /你的筛选还在/);
await page.goto(`${base}?chrome=0&scene=sync&state=error&me=reader`);
await page.waitForTimeout(200);
assert.match(await page.locator("#main").innerText(), /管理任务面/);
assert.doesNotMatch(await page.locator("#main").innerText(), /没能读到同步状态/);
pass("errors retain data, keep page-specific copy and gate non-managers");

// P1-5: no board.read hides the task entry; legacy keeps it.
await page.goto(`${base}?chrome=0&scene=queue&state=permission&me=reader`);
await page.waitForTimeout(200);
assert.deepEqual(await page.locator('[data-scene="queue"]').evaluateAll((els) => els.map((el) => el.hidden)), [true, true]);
await page.goto(`${base}?chrome=0&scene=queue&state=migrate`);
await page.waitForTimeout(200);
assert.deepEqual(await page.locator('[data-scene="queue"]').evaluateAll((els) => els.map((el) => el.hidden)), [false, false]);
pass("task entry is hidden without board.read/task.read and kept for legacy");

// P2: archive only under archive=1; batch retry and delivery sheet.
await page.goto(`${base}?chrome=0&scene=queue&state=success&tab=all`);
await page.waitForTimeout(200);
assert.equal(await page.locator('[data-action="open-task"][data-id="852"]').count(), 0);
await page.goto(`${base}?chrome=0&scene=queue&archive=1`);
await page.waitForTimeout(200);
assert.equal(await page.locator('[data-action="open-task"][data-id="852"]').count(), 1);
await page.goto(`${base}?chrome=0&scene=sync`);
await page.waitForTimeout(200);
assert.equal(await page.locator('[data-action="retry-all"]').count(), 1);
await page.locator('[data-action="retry-all"]').click();
await page.waitForTimeout(200);
assert.equal(await page.locator(".dead-row").count(), 0);
await page.locator('[data-action="dialog"][data-dialog="deliveries"]').first().click();
await page.waitForTimeout(200);
assert.match(await page.locator(".dialog").innerText(), /投递记录/);
await page.locator('[data-action="dialog-close"]').last().click();
pass("archive is gated, dead letters batch-retry and delivery sheet open");

// P2: watcher has no fake 5s undo affordance.
await page.goto(`${base}?chrome=0&scene=automation`);
await page.waitForTimeout(200);
await page.locator('[data-action="toggle-watcher"][data-id="triage-inbox"]').click();
await page.waitForTimeout(200);
assert.doesNotMatch(await page.locator(".toast").last().innerText(), /5 秒/);
pass("watcher toggle has no fake undo promise");

// P2: shortcut help lists only implemented keys and tab navigation exists.
await page.goto(`${base}?chrome=0&scene=task&id=812&dialog=shortcuts`);
await page.waitForTimeout(200);
const help = await page.locator(".dialog").innerText();
assert.match(help, /1–5/);
assert.doesNotMatch(help, /Space/);
await page.goto(`${base}?chrome=0&scene=queue`);
await page.waitForTimeout(200);
await page.keyboard.press("]");
await page.waitForTimeout(150);
assert.ok((await page.locator('[data-action="tab"][data-tab="completed"]').getAttribute("aria-selected")) === "true");
await page.keyboard.press("2");
await page.waitForTimeout(150);
assert.ok((await page.locator('[data-action="sub"][data-sub="waiting"]').getAttribute("class")).includes("active"));
pass("shortcut help aligns with implemented keys and tab/subfilter keys work");

// P1-1: mobile Composer is fixed above the tab bar, and max scroll/focus work.
await page.setViewportSize({ width: 390, height: 844 });
await page.goto(`${base}?chrome=0&scene=task&id=812`);
await page.waitForTimeout(300);
const composerBox = await page.locator(".composer").evaluate((el) => {
  const r = el.getBoundingClientRect();
  return { position: getComputedStyle(el).position, top: r.top, bottom: r.bottom, height: r.height };
});
assert.equal(composerBox.position, "fixed");
assert.ok(composerBox.top >= 0 && composerBox.bottom <= 788, JSON.stringify(composerBox));
await page.goto(`${base}?chrome=0&scene=task&id=812&scroll=bottom`);
await page.waitForTimeout(300);
const lastReplyBottom2 = await page.locator(".reply").last().evaluate((el) => el.getBoundingClientRect().bottom);
const composerTop2 = await page.locator(".composer").evaluate((el) => el.getBoundingClientRect().top);
assert.ok(lastReplyBottom2 <= composerTop2 - 4, `reply ${lastReplyBottom2} composer ${composerTop2}`);
assert.ok(composerTop2 - lastReplyBottom2 <= 32, `gap ${composerTop2 - lastReplyBottom2}`);
await page.goto(`${base}?chrome=0&scene=task&id=812&focus=reply`);
await page.waitForTimeout(400);
assert.equal(await page.evaluate(() => document.activeElement?.id), "composer-input");
assert.ok(await page.locator(".composer").evaluate((el) => { const r = el.getBoundingClientRect(); return r.top >= 0 && r.bottom <= 788; }));
pass("mobile Composer is fixed, max-scroll gap is tight and reply focuses it");

// P1-2: mobile more opens a bottom sheet above the tab bar with 44px rows.
await page.goto(`${base}?chrome=0&scene=task&id=812`);
await page.waitForTimeout(250);
await page.locator('[data-action="menu"][data-mobile-sheet="1"]').click();
await page.waitForTimeout(250);
assert.equal(await page.locator(".dialog.sheet").count(), 1);
const moreItems = await page.locator(".dialog .menu-item").evaluateAll((els) => els.map((el) => { const r = el.getBoundingClientRect(); return { top: r.top, bottom: r.bottom, height: r.height }; }));
assert.ok(moreItems.length > 0);
assert.ok(moreItems.every((r) => r.height >= 44 && r.bottom <= 844), JSON.stringify(moreItems));
pass("mobile more is a bottom sheet above the tab bar with 44px rows");

// P1-3: capability-driven 404/403; state=permission does not mislabel surfaces.
for (const [scene, needle] of [["queue", "任务面不可用"], ["task", "任务不存在"], ["sync", "同步不可用"], ["automation", "自动化不可用"]]) {
  await page.setViewportSize({ width: 1440, height: 1000 });
  const id = scene === "task" ? "&id=812" : "";
  await page.goto(`${base}?chrome=0&scene=${scene}${id}&state=success&me=none`);
  await page.waitForTimeout(200);
  assert.match(await page.locator("#main").innerText(), new RegExp(needle));
}
await page.goto(`${base}?chrome=0&scene=sync&state=permission`);
await page.waitForTimeout(200);
assert.match(await page.locator("#main").innerText(), /管理任务面/);
assert.doesNotMatch(await page.locator("#main").innerText(), /实时|只读/);
await page.goto(`${base}?chrome=0&scene=automation&state=permission`);
await page.waitForTimeout(200);
assert.match(await page.locator("#main").innerText(), /管理任务面/);
assert.doesNotMatch(await page.locator("#main").innerText(), /实时|只读/);
await page.goto(`${base}?chrome=0&scene=queue&state=permission`);
await page.waitForTimeout(200);
assert.match(await page.locator("#main").innerText(), /任务面不可用/);
pass("me=none deep links are 404, reader is 403 and permission frames are labeled correctly");

// P2: stale/error writes disabled; dialog= guarded; NewTask board.* rejected; needs search.
await page.goto(`${base}?chrome=0&scene=task&id=812&state=stale`);
await page.waitForTimeout(200);
assert.equal(await page.locator('[data-action="dialog"][data-dialog="complete"]').count(), 0);
await page.keyboard.press("e");
await page.waitForTimeout(150);
assert.equal(await page.locator(".dialog").count(), 0);
await page.goto(`${base}?chrome=0&scene=task&id=812&state=error`);
await page.waitForTimeout(200);
assert.equal(await page.locator('[data-action="dialog"][data-dialog="complete"]').count(), 0);
assert.equal(await page.locator('[data-action="send-reply"]').count(), 0);
await page.goto(`${base}?chrome=0&scene=task&id=806&dialog=cancel`);
await page.waitForTimeout(200);
assert.equal(await page.locator(".dialog").count(), 0);
await page.goto(`${base}?chrome=0&scene=queue&dialog=new`);
await page.waitForTimeout(200);
await page.locator("#d-text").fill("测试 board 标签拒绝");
await page.locator("#d-label").fill("board.task=ready");
await page.locator('[data-action="confirm"][data-kind="new"]').click();
await page.waitForTimeout(200);
assert.equal(await page.locator(".dialog").count(), 1);
assert.match(await page.locator(".toast").last().innerText(), /board\.\*/);
await page.goto(`${base}?chrome=0&scene=task&id=830&dialog=needs`);
await page.waitForTimeout(200);
await page.locator("#d-needs-search").fill("809");
await page.waitForTimeout(150);
assert.equal(await page.locator(".check-row:visible").count(), 1);
pass("stale/error writes are disabled, board.* is rejected and needs search works");

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
