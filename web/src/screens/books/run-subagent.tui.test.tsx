// TUI verification for the sub-agent tree on the run page — change
// keel-collab-subagent, tasks 4.1/4.3/5.2/5.3.
//
// The five states, depth labels, per-child cancel/retry and the ledger retry
// are driven by the scripted harness (`run-subagent.harness.tsx`) so the test
// sees the same wire records a live deployment sends. The success face on the
// product's own fixture transport is covered by `bots-teams.tui.test.tsx` and
// `task-panel.tui.test.tsx`; the fixture also carries depth/maxDepth now.
//
// Depth wording is part of the assertion: main = `depth 0 · 主`, a child is
// `depth N/M`, the two special caps are `depth N/-1 · 不限` and
// `depth N/0 · 禁止派生`. A fork/task is explicitly outside the delegation
// depth (`depth — · 分叉不计` / `depth — · 任务卡`).
import { expect, open, test } from "nsh/test";

const harness = (width: number, scenario: string) =>
  open("./run-subagent.harness.tsx", { width, host: { params: { scenario } } });

test("TUI: 子代理树五态——加载 / 空 / 错误 / 无权限 / 成功（含 depth 文案）", async () => {
  const loading = await harness(1440, "run-loading");
  await loading.waitFor("id:run-loading");
  expect(loading.texts().join("\n")).not.toContain("本周退款原因分析");

  const empty = await harness(1440, "run-empty");
  await empty.waitFor("id:run-empty");
  expect(empty.texts()).toContain("这次运行还没有可读的事件");
  expect(empty.shows("id:run-lanes")).toBe(false);

  const broken = await harness(1440, "run-error");
  await broken.waitFor("id:run-error");
  expect(broken.texts()).toContain("没能读到这次运行");
  expect(broken.texts()).toContain("请求编号 k-run-1");

  // A refused session relation is the run page's partial face: the team shell
  // stays visible, the missing ledger is named, and nothing is invented.
  const denied = await harness(1440, "run-permission");
  await denied.waitFor("id:run-lanes");
  expect(denied.texts().join("\n")).toContain("只显示已经读到的部分");
  expect(denied.texts()).toContain("客服团队");

  const success = await harness(1440, "run-success");
  await success.waitFor("id:run-lanes");
  const screen = success.texts().join("\n");
  expect(screen).toContain("归档查询");
  expect(screen).toContain("条款核对");
  expect(screen).toContain("政策核对");
  expect(screen).not.toContain("这次运行还没有可读的事件");
  // Main, the 1 -> 2 -> 3 chain, unlimited (-1), forbidden (0).
  expect(screen).toContain("depth 0 · 主");
  expect(screen).toContain("depth 1/3");
  expect(screen).toContain("depth 2/3");
  expect(screen).toContain("depth 3/3");
  expect(screen).toContain("depth 1/-1 · 不限");
  expect(screen).toContain("depth 1/0 · 禁止派生");
  // The inspector repeats the selected node's depth (the failed child).
  expect(screen).toContain("深度");
  expect(screen).toContain("depth 2/3");

  // 390: the same tree becomes the vertical timeline with the same labels.
  const phone = await harness(390, "run-success");
  await phone.waitFor("id:run-timeline");
  const phoneScreen = phone.texts().join("\n");
  expect(phoneScreen).toContain("归档查询");
  expect(phoneScreen).toContain("条款核对");
  expect(phoneScreen).toContain("政策核对");
  expect(phoneScreen).toContain("depth 0 · 主");
  expect(phoneScreen).toContain("depth 1/3");
  expect(phoneScreen).toContain("depth 3/3");
  expect(phoneScreen).toContain("depth 1/-1 · 不限");
  expect(phoneScreen).toContain("depth 1/0 · 禁止派生");
}, 300_000);

test("TUI: 子代理节点取消/重试——取消子树、重试同一会话（1440）", async () => {
  const wide = await harness(1440, "run-success");
  await wide.waitFor("id:run-lanes");
  // The failed child is selected by default: retry is the resume action.
  await wide.waitFor("id:run-node-actions");
  expect(wide.shows("id:run-node-retry")).toBe(true);
  await wide.press("id:run-node-retry");
  await wide.waitFor("id:run-node-action-notice");
  expect(wide.texts().join("\n")).toContain("已重新排队");

  // Select the running unlimited child and cancel its subtree.
  await wide.press("label:查看节点 深度不限，运行中");
  await wide.waitFor("id:run-node-cancel");
  expect(wide.texts().join("\n")).toContain("depth 1/-1 · 不限");
  await wide.press("id:run-node-cancel");
  await wide.waitFor("id:run-node-cancel-dialog");
  expect(wide.texts().join("\n")).toContain("取消这个子代理子树？");
  await wide.press("id:run-node-cancel-dialog-confirm");
  await wide.waitFor("id:run-node-action-notice");
  expect(wide.texts().join("\n")).toContain("已取消");
  // The same node stays selected after the reload; it is now stopped and can
  // be retried through the resume path.
  await wide.waitFor("id:run-node-retry");
  await wide.press("id:run-node-retry");
  await wide.waitFor("id:run-node-action-notice");
  expect(wide.texts().join("\n")).toContain("已重新排队");
}, 240_000);

test("TUI: 子代理节点取消/重试——390 时间线检查器 Sheet", async () => {
  const phone = await harness(390, "run-success");
  await phone.waitFor("id:run-timeline");
  await phone.press("label:查看节点 条款核对，失败");
  await phone.waitFor("id:run-node-actions");
  expect(phone.shows("id:run-node-retry")).toBe(true);
  await phone.press("id:run-node-retry");
  await phone.waitFor("id:run-node-action-notice");
  expect(phone.texts().join("\n")).toContain("已重新排队");

  // A fresh app for the cancel flow: the retry sheet is still open and would
  // intercept a press on the timeline underneath it.
  const cancelled = await harness(390, "run-success");
  await cancelled.waitFor("id:run-timeline");
  await cancelled.press("label:查看节点 深度不限，运行中");
  await cancelled.waitFor("id:run-node-actions");
  await cancelled.press("id:run-node-cancel");
  await cancelled.waitFor("id:run-node-cancel-dialog");
  await cancelled.press("id:run-node-cancel-dialog-confirm");
  await cancelled.waitFor("id:run-node-action-notice");
  expect(cancelled.texts().join("\n")).toContain("已取消");
}, 240_000);

test("TUI: 子代理动作五态——空 / 加载 / 错误 / 无权限 / 成功", async () => {
  // Empty: the run has no sub-agent node, so it has no node actions.
  const empty = await harness(1440, "run-empty");
  await empty.waitFor("id:run-empty");
  expect(empty.shows("id:run-node-actions")).toBe(false);

  // Loading: the resume call never settles; the button says 重试中… and no
  // notice/error is invented.
  const loading = await harness(1440, "run-action-loading");
  await loading.waitFor("id:run-lanes");
  await loading.waitFor("id:run-node-actions");
  await loading.press("id:run-node-retry");
  await loading.waitFor("text:/重试中…/");
  expect(loading.shows("id:run-node-action-notice")).toBe(false);

  // Error: the resume call answers 500 with a request id-bearing error.
  const broken = await harness(1440, "run-action-error");
  await broken.waitFor("id:run-lanes");
  await broken.press("id:run-node-retry");
  await broken.waitFor("id:run-node-action-error");
  expect(broken.texts().join("\n")).toContain("重试子代理失败");
  expect(broken.texts().join("\n")).toContain("子代理动作暂时不可用");

  // No permission: both the child cancel and the resume name the missing
  // session.write key instead of pretending the action ran.
  const denied = await harness(1440, "run-action-permission");
  await denied.waitFor("id:run-lanes");
  await denied.press("label:查看节点 深度不限，运行中");
  await denied.waitFor("id:run-node-cancel");
  await denied.press("id:run-node-cancel");
  await denied.waitFor("id:run-node-cancel-dialog");
  await denied.press("id:run-node-cancel-dialog-confirm");
  await denied.waitFor("id:run-node-action-error");
  expect(denied.texts().join("\n")).toContain("取消子代理被拒");
  expect(denied.texts().join("\n")).toContain("缺「继续会话」能力");
  await denied.press("label:查看节点 条款核对，失败");
  await denied.waitFor("id:run-node-retry");
  await denied.press("id:run-node-retry");
  await denied.waitFor("id:run-node-action-error");
  expect(denied.texts().join("\n")).toContain("重试子代理被拒");
}, 300_000);

test("TUI: 子代理树运行级取消——二次确认、停止信号与状态", async () => {
  const wide = await harness(1440, "run-cancel");
  await wide.waitFor("id:run-lanes");
  await wide.press("id:run-stop");
  await wide.waitFor("id:run-stop-dialog");
  expect(wide.texts().join("\n")).toContain("停止这次运行？");
  await wide.press("id:run-stop-dialog-confirm");
  await wide.waitFor("id:run-stop-notice");
  const stopped = wide.texts().join("\n");
  expect(stopped).toContain("已发出停止信号");
  expect(stopped).toContain("已停止");
  expect(wide.shows("id:run-stop")).toBe(false);

  const phone = await harness(390, "run-cancel");
  await phone.waitFor("id:run-timeline");
  await phone.press("id:run-stop-mobile");
  await phone.waitFor("id:run-stop-dialog");
  await phone.press("id:run-stop-dialog-confirm");
  await phone.waitFor("id:run-stop-notice");
  const phoneStopped = phone.texts().join("\n");
  expect(phoneStopped).toContain("已发出停止信号");
  expect(phoneStopped).toContain("已停止");
}, 240_000);

test("TUI: 节点账本读取失败后可重试", async () => {
  const wide = await harness(1440, "run-retry");
  await wide.waitFor("id:run-lanes");
  await wide.waitFor("text:/没能读到这个节点的会话事件/");
  expect(wide.texts().join("\n")).toContain("节点账本暂时不可用");
  await wide.press("label:重试读取节点事件");
  await wide.waitUntil(() => !wide.texts().join("\n").includes("没能读到这个节点的会话事件"));
  expect(wide.texts().join("\n")).not.toContain("没能读到这个节点的会话事件");
}, 180_000);
