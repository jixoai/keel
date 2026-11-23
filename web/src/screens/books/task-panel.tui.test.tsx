// TUI verification for the task panel — change keel-harness-v2, W4.
//
// The success face runs on the app's own fixture transport through the run
// page (a real in-memory task tree, a fork member and two attached
// references). The five states run in a harness inside the app's module graph,
// because a test's module graph is not the app's. The 390 frame proves the
// panel stays one column with no squeezed row.
import { expect, open, test } from "nsh/test";

const app = (width = 1440) => open("../../../App.tsx", { width, host: { params: { fixture: true } } });
const harness = (width: number, scenario: string) =>
  open("./task-panel.harness.tsx", { width, host: { params: { scenario } } });

/** Open the fixture's 客服团队 run (the design board's sample) and wait for the
 * run page's own views. */
async function openFixtureRun(width: number) {
  const view = await app(width);
  // ≥1024: the chapter rail; ≤767: the same destination is a bottom tab.
  await view.press(width <= 767 ? "id:books-tab-teams" : "id:books-chapter-teams");
  await view.waitFor("id:teams-head");
  await view.press("label:打开团队 客服团队");
  await view.waitFor("id:team-roster");
  await view.press("id:team-start");
  await view.waitFor("id:team-start-dialog");
  await view.press("id:team-start-dialog-confirm");
  // ≤767 the run page draws the vertical timeline; wider it draws the swimlane.
  await view.waitFor(width <= 767 ? "id:run-timeline" : "id:run-lanes", { timeout: 20000 });
  return view;
}

test("TUI: 运行页的任务面板入口——真实读数，图里有根卡、成员与分叉原点", async () => {
  const view = await openFixtureRun(1440);
  await view.waitFor("id:run-task-panel-entry");
  const entry = view.texts().join("\n");
  expect(entry).toContain("任务面板");
  // The read landed before the entry rendered: the counts are the fixture's.
  expect(entry).toMatch(/卡 · \d+ 事件/);

  await view.press("id:run-task-panel-entry");
  await view.waitFor("id:task-panel");
  expect(view.shows("id:task-panel-graph")).toBe(true);
  const screen = view.texts().join("\n");
  expect(screen).toContain("本周退款原因分析");
  expect(screen).toContain("汇总渠道异常");
  // The member card's row lives in another session: the panel says so.
  expect(screen).toContain("另一会话");
  // The fork member is a branch with its own origin sequence.
  expect(view.shows("id:task-panel-fork-task-refund-fork")).toBe(true);
  expect(screen).toContain("seq 12");
  expect(screen).toContain("分叉");
  // Fidelity: only cards whose parent is an own card are members here. The
  // knowledge/aftersales cards' parents are elsewhere or absent, so they are
  // not drawn as this session's members.
  expect(view.shows("id:task-panel-card-task-knowledge")).toBe(false);
  expect(view.shows("id:task-panel-card-task-aftersales")).toBe(false);
}, 120_000);

test("TUI: 任务面板三视图——图 / 事件流 / 资源预览（事件带 kind 与卡名）", async () => {
  const view = await openFixtureRun(1440);
  await view.press("id:run-task-panel-entry");
  await view.waitFor("id:task-panel-graph");
  expect(view.texts().join("\n")).toContain("任务卡、事件流与资源引用都是这次会话账本的投影");

  await view.press("id:task-panel-section-events");
  await view.waitFor("id:task-panel-events");
  const events = view.texts().join("\n");
  expect(events).toContain("创建");
  expect(events).toContain("挂载资源");
  expect(events).toContain("注记");
  expect(events).toContain("渠道 B 的退款率翻倍");
  // A note's text is the event's own string, not a composed sentence.
  expect(view.shows("id:task-panel-events-all")).toBe(false);

  // Selecting a card filters the stream to it; 看全部 clears the filter.
  await view.press("id:task-panel-section-graph");
  await view.waitFor("id:task-panel-graph");
  await view.press("id:task-panel-card-task-captain");
  await view.waitFor("id:task-panel-events");
  expect(view.texts().join("\n")).toContain("只看 本周退款原因分析");
  expect(view.shows("id:task-panel-events-all")).toBe(true);
  await view.press("id:task-panel-events-all");
  await view.waitFor("text:这次会话的全部任务事件");
  expect(view.shows("id:task-panel-events-all")).toBe(false);

  await view.press("id:task-panel-section-resources");
  await view.waitFor("id:task-panel-resources");
  const resources = view.texts().join("\n");
  expect(resources).toContain("reports/refund-analysis.md");
  expect(resources).toContain("reports/refund-report.json");
  expect(resources).toContain("资源只记录 (path, hash)");
  // The manifest attributes the CSV to the summary card (lastTaskId); it has no
  // attach event, so this group is the only place it is named.
  expect(resources).toContain("文件清单 · 按卡归属（lastTaskId）");
  expect(view.shows("id:task-panel-file-reports/refund-by-channel.csv")).toBe(true);
  // A file already shown as a live attachment is not printed twice.
  expect(view.shows("id:task-panel-file-reports/refund-report.json")).toBe(false);
}, 120_000);

test("TUI: 资源预览——按会话清单打开，与引用一致", async () => {
  const view = await openFixtureRun(1440);
  await view.press("id:run-task-panel-entry");
  await view.waitFor("id:task-panel");
  await view.press("id:task-panel-section-resources");
  await view.waitFor("id:task-panel-resources");
  await view.press("id:task-panel-resource-reports/refund-analysis.md");
  await view.waitFor("id:task-panel-preview");
  const preview = view.texts().join("\n");
  expect(preview).toContain("与引用一致");
  expect(preview).toContain("退款原因分析");
  // The preview's size line is the file's own: this fixture file is under a
  // kilobyte, so it prints bytes — never a rounded-up 1 KB.
  expect(preview).toMatch(/\d+ B · \d+\/\d+ 行/);
  expect(preview).not.toContain("1 KB");
  expect(view.shows("id:task-panel-preview-content")).toBe(true);
}, 120_000);

test("TUI: 任务面板五态——加载 / 空 / 错（请求编号）/ 权限不足", async () => {
  const loading = await harness(1440, "panel-loading");
  await loading.waitFor("id:task-panel-loading");
  expect(loading.texts().join("\n")).not.toContain("本周退款原因分析");

  const empty = await harness(1440, "panel-empty");
  await empty.waitFor("id:task-panel-empty");
  expect(empty.texts()).toContain("这次会话还没有任务卡");
  expect(empty.shows("id:task-panel-section")).toBe(false);

  const broken = await harness(1440, "panel-error");
  await broken.waitFor("id:task-panel-error");
  expect(broken.texts()).toContain("没能读到这次会话的任务记忆");
  expect(broken.texts()).toContain("请求编号 k-panel-1");

  const denied = await harness(1440, "panel-permission");
  await denied.waitFor("id:task-panel-permission");
  expect(denied.texts().join("\n")).toContain("需要「查看会话」能力");
}, 120_000);

test("TUI: 资源预览——文件已变化时如实说明，不冒充旧引用", async () => {
  const view = await harness(1440, "panel-changed");
  await view.waitFor("id:task-panel");
  await view.press("id:task-panel-section-resources");
  await view.waitFor("id:task-panel-resources");
  await view.press("id:task-panel-resource-reports/refund-analysis.md");
  await view.waitFor("id:task-panel-preview");
  const screen = view.texts().join("\n");
  expect(screen).toContain("文件已变化");
  expect(screen).toContain("下面是当前内容，不是卡记录的那一份");
  expect(view.shows("id:task-panel-preview-content")).toBe(true);
  expect(screen).not.toContain("与引用一致");
}, 120_000);

test("TUI: 文件归属——按卡归属的清单文件可打开，且不与挂载资源重复", async () => {
  const view = await harness(1440, "panel-success");
  await view.waitFor("id:task-panel");
  await view.press("id:task-panel-section-resources");
  await view.waitFor("id:task-panel-files");
  expect(view.texts().join("\n")).toContain("汇总渠道异常");
  expect(view.shows("id:task-panel-file-reports/refund-by-channel.csv")).toBe(true);
  // The two reports are live attachments: the file group leaves them to the
  // attachment rows above instead of naming the same (path, hash) again.
  expect(view.shows("id:task-panel-file-reports/refund-analysis.md")).toBe(false);
  // The manifest row carries its own content hash, so the preview compares it
  // with the bytes it read and says they match.
  await view.press("id:task-panel-file-reports/refund-by-channel.csv");
  await view.waitFor("id:task-panel-preview");
  const preview = view.texts().join("\n");
  expect(preview).toContain("与引用一致");
  expect(preview).toContain("渠道,退款单,占比");
  expect(preview).not.toContain("清单当前版本");
  expect(preview).not.toContain("未核对");

  // A manifest row without a hash (the defensive branch) has nothing to
  // compare: the badge names the answer instead of claiming a check.
  await view.press("id:task-panel-file-reports/unhashed.csv");
  await view.waitFor("id:task-panel-preview");
  const unhashed = view.texts().join("\n");
  expect(unhashed).toContain("清单当前版本");
  expect(unhashed).not.toContain("未核对");
}, 120_000);

test("TUI: 文件清单翻页——第一页可加载下一页，行不会互相覆盖", async () => {
  const view = await harness(1440, "panel-files-paged");
  await view.waitFor("id:task-panel");
  await view.press("id:task-panel-section-resources");
  await view.waitFor("id:task-panel-files");
  expect(view.shows("id:task-panel-file-reports/page-1.csv")).toBe(true);
  expect(view.shows("id:task-panel-file-reports/page-2.csv")).toBe(false);
  // The first answer said a later page exists: the group offers the read
  // instead of calling the list complete.
  expect(view.shows("id:task-panel-files-more")).toBe(true);
  await view.press("id:task-panel-files-more");
  await view.waitFor("id:task-panel-file-reports/page-2.csv");
  expect(view.shows("id:task-panel-file-reports/page-1.csv")).toBe(true);
  // The last page has no cursor: the control is gone, not left claiming more.
  expect(view.shows("id:task-panel-files-more")).toBe(false);
  expect(view.shows("id:task-panel-files-truncated")).toBe(false);
}, 120_000);

test("TUI: 文件清单三态——失败如实报、空如实说、未接就不出现", async () => {
  const failed = await harness(1440, "panel-files-error");
  await failed.waitFor("id:task-panel");
  await failed.press("id:task-panel-section-resources");
  await failed.waitFor("id:task-panel-files-error");
  expect(failed.texts().join("\n")).toContain("没能读到会话的文件清单");
  expect(failed.shows("id:task-panel-files")).toBe(false);

  const empty = await harness(1440, "panel-files-empty");
  await empty.waitFor("id:task-panel");
  await empty.press("id:task-panel-section-resources");
  await empty.waitFor("id:task-panel-files-empty");
  expect(empty.texts()).toContain("文件清单里没有额外归属到本会话卡的路径。");

  const unwired = await harness(1440, "panel-no-manifest");
  await unwired.waitFor("id:task-panel");
  await unwired.press("id:task-panel-section-resources");
  await unwired.waitFor("id:task-panel-resources");
  expect(unwired.shows("id:task-panel-files")).toBe(false);
  expect(unwired.shows("id:task-panel-files-error")).toBe(false);
}, 120_000);

test("TUI: 390 任务面板——单列，入口与页面都在，无横向溢出", async () => {
  const view = await openFixtureRun(390);
  await view.waitFor("id:run-task-panel-entry");
  expect(view.shows("id:run-task-panel-band")).toBe(false);
  await view.press("id:run-task-panel-entry");
  await view.waitFor("id:task-panel");
  const screen = view.texts().join("\n");
  expect(screen).toContain("任务面板");
  expect(screen).toContain("本周退款原因分析");
  expect(view.shows("id:task-panel-graph")).toBe(true);
  // The phone form is the bottom sheet, not a squeezed third column: the
  // entry flips to 收起 once the sheet is open.
  expect(screen).toContain("收起");
  expect(view.shows("id:run-task-panel-band")).toBe(false);
  // The resource face carries the manifest-attributed file at phone width too:
  // the row is one line in the sheet, not a squeezed third column.
  await view.press("id:task-panel-section-resources");
  await view.waitFor("id:task-panel-files");
  expect(view.shows("id:task-panel-file-reports/refund-by-channel.csv")).toBe(true);
}, 120_000);
