// TUI verification for the super-agent ticket board — change
// keel-collab-superagent, 4.2/4.3 (product UI).
//
// The success faces and the three writes run on the app's own fixture
// transport (`?fixture=1`), so the same `KeelHttp`-shaped methods, route,
// permission gates and refresh path are exercised. The five states, the
// read-only gate and the write-failure faces run in a harness inside the app's
// module graph, because a test's module graph is not the app's. The 390 and
// 768 frames prove the board keeps one readable column and the list/detail
// screens swap without a squeezed third pane.
import { expect, open, test } from "nsh/test";

const app = (width = 1440) => open("../../../App.tsx", { width, host: { params: { fixture: true } } });
const harness = (width: number, scenario: string) =>
  open("./superagent.harness.tsx", { width, host: { params: { scenario } } });

/** Open 02 任务 from the rail (wide) or the bottom bar (≤767) and wait for the
 * board's own content. */
async function openBoard(width: number) {
  const view = await app(width);
  await view.press(width <= 767 ? "id:books-tab-superagent" : "id:books-chapter-superagent");
  await view.waitFor("id:superagent-board", { timeout: 20_000 });
  return view;
}

const countLabel = (view: Awaited<ReturnType<typeof open>>, key: string): string =>
  view.accessibility(`id:superagent-count-${key}`)?.label ?? "";

test("TUI: superagent 工单看板成功面——四类计数、最新在前、Mandate/worker/当前报告与收件箱", async () => {
  const view = await openBoard(1440);
  // Wide: the list and its detail are one board, the detail a fixed column.
  expect(view.shows("id:superagent-detail-column")).toBe(true);
  const detailWidth = view.layout("id:superagent-detail-column")?.width ?? 0;
  expect(detailWidth).toBeGreaterThan(400);
  expect(detailWidth).toBeLessThanOrEqual(440);
  expect(countLabel(view, "waiting")).toBe("待人工 2 张");
  expect(countLabel(view, "running")).toBe("执行中 1 张");
  expect(countLabel(view, "done")).toBe("完成 1 张");
  expect(countLabel(view, "failed")).toBe("失败 1 张");

  // The server contract is updated_at DESC and the client must not re-sort:
  // the fixture's rows are -3m / -6m / -30m / -3h / -5h old in this order.
  const order = [
    "6a2f1c3e-4b5d-4e6f-8a90-1b2c3d4e5f02",
    "6a2f1c3e-4b5d-4e6f-8a90-1b2c3d4e5f01",
    "6a2f1c3e-4b5d-4e6f-8a90-1b2c3d4e5f04",
    "6a2f1c3e-4b5d-4e6f-8a90-1b2c3d4e5f05",
    "6a2f1c3e-4b5d-4e6f-8a90-1b2c3d4e5f03",
  ];
  const tops = order.map((id) => view.layout(`id:superagent-ticket-${id}`)?.y ?? -1);
  for (let index = 0; index < tops.length; index += 1) {
    expect(tops[index]).toBeGreaterThanOrEqual(0);
    if (index > 0) expect(tops[index - 1]).toBeLessThan(tops[index]);
  }

  const screen = view.texts().join("\n");
  expect(screen).toContain("工单看板");
  expect(screen).toContain("渠道 B 退款重复扣款排查");
  expect(screen).toContain("准备新客户入驻清单");
  expect(screen).toContain("补充退款审计日志");
  expect(screen).toContain("同步渠道对账数据");
  expect(screen).toContain("待人工收件箱");
  expect(screen).toContain("渠道 B 的重复扣款是否按「先退款、后追偿」处理？");
  // The expired question says so instead of pretending it can be answered.
  expect(screen).toContain("已过期");
  // The default detail is the most recently updated ticket; its mandate,
  // worker and the current Assignment's report are all on screen.
  expect(screen).toContain("Mandate");
  expect(screen).toContain("发起人");
  expect(screen).toContain("陈墨");
  expect(screen).toContain("可评论");
  expect(screen).toContain("Worker");
  expect(screen).toContain("当前 Assignment 报告");
  expect(screen).toContain("已完成 4 项中的 2 项");
  // Selecting the blocked ticket moves the detail with the list; its Mandate
  // strength and its open question are the row's own facts.
  await view.press("label:打开工单 渠道 B 退款重复扣款排查");
  await view.waitFor("id:superagent-detail-attention-b7c1d2e3-f4a5-4b6c-9d7e-8f90a1b2c301");
  const blocked = view.texts().join("\n");
  expect(blocked).toContain("1 个开放");
  expect(blocked).toContain("可写入");
  expect(view.accessibility("id:superagent-detail-status")?.label).toBe("待人工");
}, 120_000);

test("TUI: superagent 答复选项——持久写入后收件箱与四类计数一起刷新", async () => {
  const view = await openBoard(1440);
  expect(countLabel(view, "waiting")).toBe("待人工 2 张");
  expect(countLabel(view, "running")).toBe("执行中 1 张");

  await view.press("label:答复：先退款，再向渠道追偿");
  await view.waitFor("id:superagent-notice", { timeout: 20_000 });
  expect(view.texts()).toContain("答复已提交，工单已继续执行。");
  // The answered question left the inbox; its ticket moved to 执行中.
  expect(view.shows("id:superagent-attention-b7c1d2e3-f4a5-4b6c-9d7e-8f90a1b2c301")).toBe(false);
  expect(countLabel(view, "waiting")).toBe("待人工 1 张");
  expect(countLabel(view, "running")).toBe("执行中 2 张");
  expect(view.accessibility("id:superagent-ticket-status-6a2f1c3e-4b5d-4e6f-8a90-1b2c3d4e5f01")?.label).toBe("执行中");
}, 120_000);

test("TUI: superagent 自由文本答复——输入框只在 task.write 下出现，提交后清空且刷新", async () => {
  const view = await openBoard(1440);
  const input = "id:superagent-attention-b7c1d2e3-f4a5-4b6c-9d7e-8f90a1b2c301-text";
  const submit = "id:superagent-attention-b7c1d2e3-f4a5-4b6c-9d7e-8f90a1b2c301-answer";
  await view.type(input, "先按重复扣款退款，再让财务向渠道追偿。");
  expect(view.value(input)).toBe("先按重复扣款退款，再让财务向渠道追偿。");
  await view.press(submit);
  await view.waitFor("id:superagent-notice", { timeout: 20_000 });
  expect(view.texts()).toContain("答复已提交，工单已继续执行。");
  expect(view.shows("id:superagent-attention-b7c1d2e3-f4a5-4b6c-9d7e-8f90a1b2c301")).toBe(false);
}, 120_000);

test("TUI: superagent 恢复工单——blocked/failed 重开并刷新为执行中", async () => {
  const view = await openBoard(1440);
  await view.press("label:打开工单 同步渠道对账数据");
  await view.waitFor("id:superagent-ticket-resume");
  expect(view.accessibility("id:superagent-detail-status")?.label).toBe("失败");
  // The operator copy names the third resumable state explicitly.
  expect(view.texts().join("\n")).toContain("失败（failed）");

  await view.press("id:superagent-ticket-resume");
  await view.waitFor("id:superagent-notice", { timeout: 20_000 });
  expect(view.texts()).toContain("工单已恢复，等待新的执行结果。");
  expect(view.accessibility("id:superagent-detail-status")?.label).toBe("执行中");
  expect(view.shows("id:superagent-detail-revoked")).toBe(false);
}, 120_000);

test("TUI: superagent 待投递输入——失败行可见，重试与清除各自留痕", async () => {
  const view = await openBoard(1440);
  await view.press("label:打开工单 同步渠道对账数据");
  await view.waitFor("id:superagent-detail-pending");
  await view.waitFor("id:superagent-pending-fixture-manual-76-edit-1");
  const detail = view.texts().join("\n");
  expect(detail).toContain("投递失败");
  expect(detail).toContain("this deployment cannot deliver input to a worker");
  expect(view.shows("id:superagent-pending-fixture-manual-76-edit-1-retry")).toBe(true);
  await view.press("id:superagent-pending-fixture-manual-76-edit-1-retry");
  await view.waitFor("id:superagent-notice", { timeout: 20_000 });
  expect(view.texts().join("\n")).toContain("已重新投递");
  expect(view.shows("id:superagent-pending-fixture-manual-76-edit-1")).toBe(false);

  // A fresh fixture: dismiss removes the same row without a delivery attempt.
  const second = await openBoard(1440);
  await second.press("label:打开工单 同步渠道对账数据");
  await second.waitFor("id:superagent-pending-fixture-manual-76-edit-1");
  expect(second.shows("id:superagent-pending-fixture-manual-76-edit-1-dismiss")).toBe(true);
  await second.press("id:superagent-pending-fixture-manual-76-edit-1-dismiss");
  await second.waitFor("id:superagent-notice", { timeout: 20_000 });
  expect(second.texts().join("\n")).toContain("待投递输入已清除");
  expect(second.shows("id:superagent-pending-fixture-manual-76-edit-1")).toBe(false);
}, 120_000);

test("TUI: superagent 停止工单——确认对话框、Mandate 撤销、状态与操作同步关闭", async () => {
  const view = await openBoard(1440);
  expect(countLabel(view, "running")).toBe("执行中 1 张");

  await view.press("label:打开工单 准备新客户入驻清单");
  await view.waitFor("id:superagent-ticket-stop");
  await view.press("id:superagent-ticket-stop");
  await view.waitFor("id:superagent-stop-dialog");
  expect(view.texts().join("\n")).toContain("停止这张工单？");
  await view.press("id:superagent-stop-dialog-confirm");
  await view.waitFor("id:superagent-notice", { timeout: 20_000 });

  expect(view.texts()).toContain("工单已停止；Mandate 已撤销，记录保留可审计。");
  expect(view.accessibility("id:superagent-detail-status")?.label).toBe("已停止");
  expect(view.shows("id:superagent-detail-revoked")).toBe(true);
  expect(view.texts().join("\n")).toContain("Mandate 已撤销");
  expect(view.shows("id:superagent-ticket-stop")).toBe(false);
  expect(view.shows("id:superagent-ticket-resume")).toBe(false);
  // The stopped ticket moves from 执行中 to the failed/stopped bucket.
  expect(countLabel(view, "running")).toBe("执行中 0 张");
  expect(countLabel(view, "failed")).toBe("失败 2 张");
}, 120_000);

test("TUI: superagent 看板五态——加载骨架 / 空（下一步）/ 错误（请求编号）/ 权限不足（缺 task.read）", async () => {
  const loading = await harness(1440, "board-loading");
  await loading.waitFor("id:superagent-loading");
  expect(loading.shows("id:superagent-board")).toBe(false);
  expect(loading.shows("id:superagent-empty")).toBe(false);

  const empty = await harness(1440, "board-empty");
  await empty.waitFor("id:superagent-empty");
  expect(empty.texts()).toContain("还没有工单");
  expect(empty.shows("id:superagent-counts")).toBe(false);
  expect(empty.shows("id:superagent-empty-refresh")).toBe(true);

  // A failed inbox is not an empty board: the inbox error must win, and the
  // full-page empty invitation must not hide it.
  const inboxBroken = await harness(1440, "board-empty-inbox-error");
  await inboxBroken.waitFor("id:superagent-inbox-error");
  expect(inboxBroken.shows("id:superagent-empty")).toBe(false);
  expect(inboxBroken.shows("id:superagent-board-empty")).toBe(true);
  expect(inboxBroken.texts().join("\n")).toContain("没能读到收件箱");

  const broken = await harness(1440, "board-error");
  await broken.waitFor("id:superagent-error");
  const brokenText = broken.texts().join("\n");
  expect(brokenText).toContain("没能读到工单看板");
  expect(brokenText).toContain("请求编号 k-super-1");
  expect(broken.shows("id:superagent-error-retry")).toBe(true);

  // The one English 503 the server sends is localized before it reaches a page.
  const unwired = await harness(1440, "board-unwired");
  await unwired.waitFor("id:superagent-error");
  const unwiredText = unwired.texts().join("\n");
  expect(unwiredText).toContain("工单看板暂时不可用");
  expect(unwiredText).toContain("superagent 模式");
  expect(unwiredText).not.toContain("not wired");
  expect(unwiredText).toContain("请求编号 k-super-503");

  // `executing` means active only; open/cancelling get their own words instead
  // of being folded into the tile's copy.
  const transitional = await harness(1440, "board-transitional");
  await transitional.waitFor("id:superagent-board");
  expect(countLabel(transitional, "running")).toBe("执行中 0 张");
  const transitionalText = transitional.texts().join("\n");
  expect(transitionalText).toContain("全部 2 张工单 · 待派工 1 张 · 停止中 1 张");
  expect(transitionalText).toContain("正在停止");

  const denied = await harness(1440, "board-denied");
  await denied.waitFor("id:superagent-permission");
  const deniedText = denied.texts().join("\n");
  expect(deniedText).toContain("需要「查看任务」能力");
  expect(deniedText).toContain("task.read");
  expect(denied.shows("id:superagent-board")).toBe(false);
}, 120_000);

test("TUI: superagent 只读权限——读得到看板，但答复/恢复/停止不渲染假按钮，并说明缺 task.write", async () => {
  const view = await harness(1440, "board-readonly");
  await view.waitFor("id:superagent-board");

  expect(view.shows("id:superagent-attention-a-fresh-readonly")).toBe(true);
  expect(view.shows("id:superagent-attention-a-fresh-option-refund-first")).toBe(false);
  expect(view.shows("id:superagent-attention-a-fresh-text")).toBe(false);
  expect(view.shows("id:superagent-attention-a-fresh-answer")).toBe(false);
  expect(view.shows("id:superagent-write-denied")).toBe(true);
  expect(view.shows("id:superagent-ticket-resume")).toBe(false);
  expect(view.shows("id:superagent-ticket-stop")).toBe(false);
  const text = view.texts().join("\n");
  expect(text).toContain("缺少「写任务」（task.write）能力");
  expect(text).toContain("渠道 B 的重复扣款是否按「先退款、后追偿」处理？");
}, 120_000);

test("TUI: superagent 写失败反馈——答复 / 恢复 / 停止各自报出服务端原因，不吞错", async () => {
  const view = await harness(1440, "board-write-error");
  await view.waitFor("id:superagent-board");

  await view.press("label:答复：先退款，再向渠道追偿");
  await view.waitFor("text:/答复失败/", { timeout: 10_000 });
  expect(view.texts().join("\n")).toContain("task.write");

  await view.press("label:打开工单 同步渠道对账数据");
  await view.waitFor("id:superagent-ticket-resume");
  // `assignment.error` is the failure note, not a "report": it appears once.
  const assignment = view.texts().join("\n");
  expect((assignment.match(/对账接口返回 502/g) ?? []).length).toBe(1);
  expect(assignment).not.toContain("当前 Assignment 报告");
  await view.press("id:superagent-ticket-resume");
  await view.waitFor("text:/恢复失败/", { timeout: 10_000 });
  expect(view.texts().join("\n")).toContain("operator");

  await view.press("label:打开工单 准备新客户入驻清单");
  await view.waitFor("id:superagent-ticket-stop");
  await view.press("id:superagent-ticket-stop");
  await view.waitFor("id:superagent-stop-dialog");
  await view.press("id:superagent-stop-dialog-confirm");
  await view.waitFor("text:/停止失败/", { timeout: 10_000 });
}, 120_000);

test("TUI: superagent 390 —— 任务在底部导航，单列板、列表进详情、返回，无横向溢出", async () => {
  const view = await openBoard(390);
  expect(view.shows("id:books-tab-superagent")).toBe(true);
  expect(view.shows("id:superagent-detail-column")).toBe(false);
  expect(view.shows("id:superagent-count-waiting")).toBe(true);
  expect(view.shows("id:superagent-count-running")).toBe(true);
  expect(view.shows("id:superagent-count-done")).toBe(true);
  expect(view.shows("id:superagent-count-failed")).toBe(true);
  const counts = view.layout("id:superagent-counts");
  expect(counts?.width).toBeLessThanOrEqual(390);
  const list = view.layout("id:superagent-ticket-list");
  expect(list?.width).toBeLessThanOrEqual(390);

  await view.press("label:打开工单 渠道 B 退款重复扣款排查");
  await view.waitFor("id:superagent-detail");
  expect(view.shows("id:books-back")).toBe(true);
  const detail = view.texts().join("\n");
  expect(detail).toContain("Mandate");
  expect(detail).toContain("渠道 B 的重复扣款是否按「先退款、后追偿」处理？");

  await view.press("id:books-back");
  await view.waitFor("id:superagent-ticket-list");
}, 120_000);

test("TUI: superagent 768 —— 平板单屏：列表与详情分屏切换，计数不换行成压缩条", async () => {
  const view = await openBoard(768);
  expect(view.shows("id:superagent-detail-column")).toBe(false);
  expect(view.shows("id:superagent-ticket-list")).toBe(true);
  await view.press("label:打开工单 补充退款审计日志");
  await view.waitFor("id:superagent-detail");
  expect(view.shows("id:books-back")).toBe(true);
  expect(view.texts().join("\n")).toContain("补充退款审计日志");
}, 120_000);
