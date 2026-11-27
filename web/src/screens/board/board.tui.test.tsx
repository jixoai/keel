// TUI verification for the board — keel-board-v1 tasks 15.1–15.7/16.1.
//
// The success flows run on the app's own fixture transport (`?fixture=1`), so
// the real route, data layer, permissions and write paths are exercised. The
// five states and permission faces run in `harness.tsx` inside the app's
// module graph (a test's graph is not the app's). Every assertion is on
// visible text/accessibility state or a real transition, never on sample data.
import { expect, open, test } from "nsh/test";

const app = (width = 1440) => open("../../../App.tsx", { width, host: { params: { fixture: true } } });
const harness = (width: number, scenario: string) =>
  open("./harness.tsx", { width, host: { params: { scenario } } });

async function openBoard(width = 1440) {
  const view = await app(width);
  await view.press(width <= 767 ? "id:books-tab-board" : "id:books-chapter-board");
  await view.waitFor("id:board-page", { timeout: 20_000 });
  return view;
}

async function openTask(view: Awaited<ReturnType<typeof open>>, id: string) {
  await view.press(`id:board-row-${id}`);
  await view.waitFor("id:board-task-page", { timeout: 20_000 });
}

test("TUI: 任务队列——状态 tabs、最早优先、行内状态/依赖/租约，筛选与分页保留", async () => {
  const view = await openBoard(1440);
  await view.waitFor("id:board-queue");
  const texts = view.texts().join("\n");
  expect(texts).toContain("任务队列");
  expect(texts).toContain("最早优先");
  // The fixture's roots are 798/809/812/814/816/818/820/822/824; the open tab
  // keeps the four working states and hides terminal rows.
  expect(view.shows("id:board-row-798")).toBe(true);
  expect(view.shows("id:board-row-812")).toBe(true);
  expect(view.shows("id:board-row-818")).toBe(false);
  expect(view.accessibility("id:board-row-state-812")?.label).toBe("已认领");
  expect(view.accessibility("id:board-row-state-798")?.label).toBe("等待依赖");
  expect(texts).toContain("前置 0/2");
  expect(texts).toContain("已认领");
  // The claimed row's countdown is local text, not a server "live" claim.
  expect(texts).toMatch(/剩余 \d/);
  // Filters are real controls; the archived toggle shows the archived row and
  // no open rows.
  await view.press("id:board-filter-archive");
  await view.waitFor("id:board-row-826", { timeout: 20_000 });
  expect(view.shows("id:board-row-812")).toBe(false);
  expect(view.accessibility("id:board-row-state-826")?.label).toBe("已完成");
  await view.press("id:board-filter-clear");
  await view.waitFor("id:board-row-812", { timeout: 20_000 });
  // Completed is one state tab away; the row shows its terminal badge.
  await view.press("id:board-state-tab-completed");
  await view.waitFor("id:board-row-818", { timeout: 20_000 });
  expect(view.shows("id:board-row-812")).toBe(false);
}, 120_000);

test("TUI: 任务页——claim 丢失单一真相，正常任务完成与回复", async () => {
  const view = await openBoard(1440);
  await openTask(view, "812");
  await view.waitFor("id:board-task-card");
  const texts = view.texts().join("\n");
  expect(texts).toContain("修复结算页偶发重复扣款");
  // The fixture's #812 is held by this account, but this tab has no token:
  // one truth, no countdown/progress, and no token-bearing or endpoint-less
  // action. takeover/force release have no board v0.1.1 endpoint.
  expect(view.shows("id:board-claim-lost")).toBe(true);
  expect(view.shows("id:board-task-lease")).toBe(false);
  expect(view.shows("id:board-task-lease-progress")).toBe(false);
  expect(texts).not.toContain("剩余");
  expect(view.shows("id:board-action-complete")).toBe(false);
  expect(view.shows("id:board-action-renew")).toBe(false);
  expect(view.shows("id:board-action-fail")).toBe(false);
  expect(view.shows("id:board-action-block")).toBe(false);
  expect(view.shows("id:board-action-takeover")).toBe(false);
  expect(view.shows("id:board-action-release.force")).toBe(false);
  expect(texts).not.toContain("收回认领");
  expect(texts).toContain("等待租约到期后可重新认领");
  // A ready task keeps the normal journey: claim, complete, reply.
  await view.press("id:board-task-back");
  await view.waitFor("id:board-queue");
  await openTask(view, "809");
  await view.waitFor("id:board-task-card");
  await view.keyboard.press("c");
  await view.waitFor("id:board-notice", { timeout: 20_000 });
  expect(view.texts()).toContain("已认领 #809");
  await view.keyboard.press("e");
  await view.waitFor("id:board-dialog-complete");
  await view.type("id:board-dialog-complete-input", "已修复并压测通过。");
  await view.press("id:board-dialog-complete-submit");
  await view.waitFor("id:board-notice", { timeout: 20_000 });
  expect(view.texts()).toContain("已完成 #809");
  expect(view.accessibility("id:board-task-state")?.label).toBe("已完成");
  // A finished task leaves the complete/renew actions; reply remains.
  expect(view.shows("id:board-action-complete")).toBe(false);
  // The reply composer is a real write; its draft clears on success.
  await view.type("id:board-reply-input", "完成记录：压测 200 次无重复扣款。");
  await view.press("id:board-reply-send");
  await view.waitFor("text:/压测 200 次无重复扣款/", { timeout: 20_000 });
  expect(view.value("id:board-reply-input")).toBe("");
}, 120_000);

test("TUI: 可认领任务——认领后出现续租/完成，释放回到 ready", async () => {
  const view = await openBoard(1440);
  await openTask(view, "809");
  await view.waitFor("id:board-task-card");
  expect(view.shows("id:board-action-claim")).toBe(true);
  expect(view.shows("id:board-action-complete")).toBe(false);
  // The card's action row can sit below the terminal viewport; the keyboard
  // path is the accessible one and is what a TUI operator uses.
  await view.keyboard.press("c");
  await view.waitFor("id:board-notice", { timeout: 20_000 });
  expect(view.texts()).toContain("已认领 #809");
  expect(view.accessibility("id:board-task-state")?.label).toBe("已认领");
  expect(view.shows("id:board-action-complete")).toBe(true);
  expect(view.shows("id:board-action-renew")).toBe(true);
  await view.keyboard.press("l");
  await view.waitFor("id:board-dialog-release");
  await view.press("label:释放");
  await view.waitFor("id:board-notice", { timeout: 20_000 });
  expect(view.texts()).toContain("已释放 #809");
  expect(view.accessibility("id:board-task-state")?.label).toBe("可认领");
}, 120_000);

test("TUI: 回复空态、筛选无结果、完成筛选与返回队列", async () => {
  const view = await openBoard(1440);
  await openTask(view, "816");
  await view.waitFor("id:board-thread-empty");
  expect(view.texts()).toContain("还没有回复");
  expect(view.shows("id:board-action-claim")).toBe(true);
  await view.press("id:board-task-back");
  await view.waitFor("id:board-queue");
  await view.press("id:board-state-tab-all");
  await view.waitFor("id:board-row-822", { timeout: 20_000 });
  expect(view.accessibility("id:board-row-state-822")?.label).toBe("已取消");
  expect(view.texts()).toContain("已取消");
}, 120_000);

test("TUI: 同步——入口、lag/凭据/死信、重试与收件箱", async () => {
  const view = await openBoard(1440);
  await view.press("id:board-view-sync");
  await view.waitFor("id:board-sync", { timeout: 20_000 });
  const texts = view.texts().join("\n");
  expect(texts).toContain("同步");
  expect(view.shows("id:board-sync-entry-slack")).toBe(true);
  expect(view.shows("id:board-sync-entry-github")).toBe(true);
  expect(view.shows("id:board-sync-credential-github")).toBe(true);
  expect(texts).toContain("凭据失效");
  expect(texts).toContain("凭据缺失");
  // Missing backend fields are one honest note per card, not repeated metrics.
  expect(texts).not.toContain("滞后未知");
  expect(texts).not.toContain("目标 · 未知");
  expect(texts).toContain("外部目标与 24h 统计暂未上报");
  expect(view.shows("id:board-sync-backend-gap-slack")).toBe(true);
  expect(view.shows("id:board-sync-targets-unknown-slack")).toBe(false);
  // Raw provider errors are not product copy.
  expect(texts).toContain("平台限流，稍后重试");
  expect(texts).toContain("平台签名已过期");
  expect(texts).not.toContain("provider answered 429");
  expect(texts).toContain("死信 · 2");
  expect(view.shows("id:board-sync-retry-all")).toBe(true);
  await view.press("id:board-sync-retry-all");
  await view.waitFor("id:board-notice", { timeout: 20_000 });
  await view.waitFor("text:/死信 · 0/", { timeout: 20_000 });
  await view.press("id:board-sync-inbox-open-slack");
  await view.waitFor("text:/收件箱 · Slack/", { timeout: 20_000 });
  expect(view.texts().join("\n")).toContain("不显示正文或凭据");
}, 120_000);

test("TUI: 自动化——watcher 开关、schedule 权限被回收只能取消", async () => {
  const view = await openBoard(1440);
  await view.press("id:board-view-automation");
  await view.waitFor("id:board-automation", { timeout: 20_000 });
  const texts = view.texts().join("\n");
  expect(texts).toContain("监视器");
  expect(texts).toContain("triage-inbox");
  expect(texts).toContain("定时任务");
  expect(view.shows("id:board-schedule-revoked-legacy-reminder")).toBe(true);
  expect(texts).toContain("已自动停发");
  // Unknown run stats are hidden, not printed as a value; the canceled row
  // has no cancel action, and the dead row's clear is the only destructive one.
  expect(texts).not.toContain("最近 未知");
  expect(view.shows("id:board-schedule-cancel-legacy-reminder")).toBe(false);
  expect(view.shows("id:board-schedule-cancel-stuck-reminder")).toBe(true);
  await view.press("id:board-watcher-switch-triage-inbox");
  await view.waitFor("id:board-notice", { timeout: 20_000 });
  expect(view.texts()).toContain("已停用 triage-inbox");
  // Statuses are the server's set: pending / disabled / canceled(revoked).
  expect(texts).toContain("运行中");
  expect(texts).toContain("已暂停");
  expect(texts).toContain("已取消");
  // A disabled plan resumes through the existing enable endpoint.
  await view.press("id:board-schedule-switch-weekly-reconcile");
  await view.waitFor("id:board-notice", { timeout: 20_000 });
  expect(view.texts()).toContain("定时任务已继续");
  await view.press("id:board-schedule-switch-weekly-reconcile");
  await view.waitFor("id:board-notice", { timeout: 20_000 });
  expect(view.texts()).toContain("定时任务已暂停");
  // A live plan is removed by DELETE; the dead/revoked one is cleared.
  await view.press("id:board-schedule-cancel-morning-queue");
  await view.waitFor("id:board-schedule-cancel-dialog");
  await view.press("label:取消任务");
  await view.waitFor("id:board-notice", { timeout: 20_000 });
  expect(view.texts()).toContain("定时任务已取消");
  expect(view.shows("id:board-schedule-morning-queue")).toBe(false);
}, 120_000);

test("TUI: 五态——loading/empty/error/disabled/404/迁移/只读权限", async () => {
  const loading = await harness(1440, "loading");
  await loading.waitFor("id:board-queue-skeleton", { timeout: 20_000 });
  expect(loading.shows("id:board-queue")).toBe(true);

  const empty = await harness(1440, "empty");
  await empty.waitFor("id:board-queue-empty", { timeout: 20_000 });
  expect(empty.texts()).toContain("队列是空的");

  const noChannels = await harness(1440, "no-channels");
  await noChannels.waitFor("id:board-queue-empty-channels", { timeout: 20_000 });
  expect(noChannels.texts()).toContain("你还没有可访问的频道");

  const error = await harness(1440, "error");
  await error.waitFor("id:board-queue-error", { timeout: 20_000 });
  expect(error.texts().join("\n")).toContain("req-board-1");
  // The five-state rule: the queue shell (tabs + filters) stays, and the
  // error is inside it rather than replacing the page.
  expect(error.shows("id:board-state-tablist")).toBe(true);
  expect(error.shows("id:board-filter-bar")).toBe(true);

  const disabled = await harness(1440, "disabled");
  await disabled.waitFor("id:board-disabled", { timeout: 20_000 });
  expect(disabled.texts()).toContain("任务看板未启用");

  const denied = await harness(1440, "denied");
  await denied.waitFor("id:board-not-found", { timeout: 20_000 });
  expect(denied.texts()).toContain("任务面不存在，或你没有访问权限");
  expect(denied.texts()).not.toContain("board.read");
  expect(denied.shows("id:books-chapter-board")).toBe(false);

  const legacy = await harness(1440, "legacy");
  await legacy.waitFor("id:board-migrate", { timeout: 20_000 });
  expect(legacy.texts()).toContain("任务面已升级，还差一步");

  const reader = await harness(1440, "reader");
  await reader.waitFor("id:board-queue", { timeout: 20_000 });
  expect(reader.shows("id:board-view-sync")).toBe(false);
  expect(reader.shows("id:board-new-task")).toBe(false);
  await openTask(reader, "812");
  expect(reader.texts().join("\n")).toContain("认领凭据不在本标签页");
  expect(reader.shows("id:board-action-complete")).toBe(false);

  const noMoves = await harness(1440, "no-moves");
  await noMoves.waitFor("id:board-row-812", { timeout: 20_000 });
  expect(noMoves.shows("id:board-moves-missing")).toBe(true);
  await openTask(noMoves, "812");
  await noMoves.waitFor("id:board-task-card");
  expect(noMoves.shows("id:board-moves-missing")).toBe(true);
  expect(noMoves.shows("id:board-action-complete")).toBe(false);

  const noMe = await harness(1440, "me-missing");
  await noMe.waitFor("id:board-row-812", { timeout: 20_000 });
  expect(noMe.shows("id:board-identity-unknown")).toBe(true);
  await openTask(noMe, "812");
  await noMe.waitFor("id:board-task-card");
  expect(noMe.shows("id:board-identity-unknown")).toBe(true);
  expect(noMe.texts().join("\n")).toContain("续租或结算前需要重新认领");

  const syncReader = await harness(1440, "sync-reader");
  await syncReader.waitFor("id:board-sync-permission", { timeout: 20_000 });
  expect(syncReader.texts().join("\n")).toContain("board.manage");

  const syncError = await harness(1440, "sync-error");
  await syncError.waitFor("id:board-sync-error", { timeout: 20_000 });
  expect(syncError.texts().join("\n")).toContain("req-sync-1");

  const autoError = await harness(1440, "automation-error");
  await autoError.waitFor("id:board-automation-error", { timeout: 20_000 });
  expect(autoError.texts().join("\n")).toContain("req-auto-1");
}, 180_000);

test("TUI: 旧 /superagent 下线页，不是重定向", async () => {
  const view = await harness(1440, "gone");
  await view.waitFor("id:board-gone", { timeout: 20_000 });
  expect(view.texts().join("\n")).toContain("这个地址已经下线");
  expect(view.texts().join("\n")).toContain("不做重定向");
  expect(view.shows("id:board-gone-queue")).toBe(true);
  expect(view.shows("id:board-queue")).toBe(false);
}, 120_000);

test("TUI: 假时钟——认领租约到点，卡片按 ready 呈现并说明 lease ran out", async () => {
  const view = await open("../../../App.tsx", {
    width: 1440,
    host: { params: { fixture: true } },
    clock: {},
  });
  await view.press("id:books-chapter-board");
  await view.waitFor("id:board-row-812", { timeout: 20_000 });
  await openTask(view, "812");
  await view.waitFor("id:board-task-card");
  expect(view.accessibility("id:board-task-state")?.label).toBe("已认领");
  // The fixture's lease is 14 minutes; move the app's own clock past it.
  await view.clock?.fastForward(16 * 60 * 1000);
  await view.waitUntil(() => view.accessibility("id:board-task-state")?.label === "可认领");
  expect(view.shows("id:board-lease-ran-out")).toBe(true);
  expect(view.texts().join("\n")).toContain("租约已到期");
}, 120_000);

test("TUI: 390 任务页——线程根与已有回复首屏可读，Composer 固定在 tab 之上", async () => {
  const view = await openBoard(390);
  await openTask(view, "809");
  await view.waitFor("id:board-task-card");
  await view.waitFor("id:board-thread-root", { timeout: 20_000 });
  await view.waitFor("id:board-reply-8091", { timeout: 20_000 });
  const root = view.layout("id:board-thread-root");
  const reply = view.layout("id:board-reply-8091");
  expect(root).toBeDefined();
  expect(root?.y ?? -1).toBeGreaterThanOrEqual(0);
  expect((root?.y ?? 0) + (root?.height ?? 0)).toBeLessThanOrEqual(844);
  expect(reply).toBeDefined();
  expect(reply?.y ?? -1).toBeGreaterThanOrEqual(0);
  expect((reply?.y ?? 0) + (reply?.height ?? 0)).toBeLessThanOrEqual(844);
  const composer = view.layout("id:board-reply-input");
  expect(composer).toBeDefined();
  expect(composer?.y ?? 0).toBeGreaterThan(0);
  expect((composer?.y ?? 0) + (composer?.height ?? 0)).toBeLessThanOrEqual(844);
  await view.type("id:board-reply-input", "390 首屏回复");
  await view.press("id:board-reply-send");
  await view.waitFor("text:/390 首屏回复/", { timeout: 20_000 });
}, 120_000);

test("TUI: 390 筛选参数——活动条件以 chips 展示，可逐个移除", async () => {
  const filtered = await open("./harness.tsx", {
    width: 390,
    host: { params: { scenario: "success", search: "channel=work/release" } },
  });
  await filtered.waitFor("id:board-filter-chip-channel", { timeout: 20_000 });
  expect(filtered.texts().join("\n")).toContain("频道 work/release");
  expect(filtered.shows("id:board-row-812")).toBe(false);
  expect(filtered.shows("id:board-row-798")).toBe(true);
  await filtered.press("id:board-filter-chip-channel");
  await filtered.waitUntil(() => !filtered.shows("id:board-filter-chip-channel"));
  expect(filtered.shows("id:board-row-812")).toBe(true);
}, 120_000);

test("TUI: 390 手机——单行横向 tabs、筛选收口、44px 控件、无横向溢出", async () => {
  const view = await openBoard(390);
  await view.waitFor("id:board-queue");
  // ≤767: no inline filter bar; one 44px 筛选 button opens the bottom sheet.
  expect(view.shows("id:board-filter-bar")).toBe(false);
  expect(view.shows("id:board-filter-open")).toBe(true);
  const tabs = view.layout("id:board-state-tabs");
  expect(tabs?.height ?? 0).toBeLessThanOrEqual(56);
  const row = view.layout("id:board-row-812");
  expect(row?.width ?? 0).toBeLessThanOrEqual(390);
  expect(row?.x ?? 0).toBeGreaterThanOrEqual(0);
  // Rows meet the touch floor on a phone; the row is the primary target.
  expect(row?.height ?? 0).toBeGreaterThanOrEqual(44);
  await view.press("id:board-filter-open");
  await view.waitFor("text:/筛选任务/", { timeout: 20_000 });
  expect(view.shows("id:board-filter-bar")).toBe(true);
  expect(view.shows("id:board-filter-reset")).toBe(true);
  expect(view.shows("id:board-filter-selected-count")).toBe(true);
  const forField = view.layout("id:board-filter-for");
  const archive = view.layout("id:board-filter-archive");
  expect(forField).toBeDefined();
  expect(archive).toBeDefined();
  expect(Math.abs((forField?.width ?? 0) - (archive?.width ?? 0))).toBeLessThanOrEqual(1);
  // The archive toggle is reachable and switches the list in place.
  await view.press("id:board-filter-archive");
  await view.waitFor("id:board-row-826", { timeout: 20_000 });
}, 120_000);
