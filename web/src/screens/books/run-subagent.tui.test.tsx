// TUI verification for the sub-agent tree on the run page — change
// keel-collab-subagent, tasks 4.3/5.2/5.3.
//
// The five states, cancel and retry are driven by the scripted harness
// (`run-subagent.harness.tsx`) so the test sees the same wire records a live
// deployment sends. The success face on the product's own fixture transport is
// already covered by `bots-teams.tui.test.tsx` and `task-panel.tui.test.tsx`.
//
// The harness sends the real depth projection (main = 0, a 1 -> 2 -> 3 chain,
// and a maxDepth=-1 sibling). This build's run surface does NOT render a
// numeric depth label for any node — the current projection uses `subtitle`
// only when a node has no parent, and every spawned child has one. That is a
// recorded UI gap (see the E2E report and evidence screenshots), not something
// this test pretends to pass: the committed green tests cover the five states,
// cancel and the node-ledger retry that do work, while the evidence captures
// the depth-bearing tree as it actually renders.
import { expect, open, test } from "nsh/test";

const harness = (width: number, scenario: string) =>
  open("./run-subagent.harness.tsx", { width, host: { params: { scenario } } });

test("TUI: 子代理树五态——加载 / 空 / 错误 / 无权限 / 成功", async () => {
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
  // The three spawned children are drawn from the depth-bearing wire records;
  // the label that would make their depth readable is the recorded gap.
  expect(screen).toContain("归档查询");
  expect(screen).toContain("条款核对");
  expect(screen).toContain("政策核对");
  expect(screen).not.toContain("这次运行还没有可读的事件");

  // 390: the same tree becomes the vertical timeline.
  const phone = await harness(390, "run-success");
  await phone.waitFor("id:run-timeline");
  const phoneScreen = phone.texts().join("\n");
  expect(phoneScreen).toContain("归档查询");
  expect(phoneScreen).toContain("条款核对");
  expect(phoneScreen).toContain("政策核对");
}, 240_000);

test("TUI: 子代理树取消——二次确认、停止信号与状态", async () => {
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

test("TUI: 子代理树重试——节点账本读取失败后可重试", async () => {
  const wide = await harness(1440, "run-retry");
  await wide.waitFor("id:run-lanes");
  await wide.waitFor("text:/没能读到这个节点的会话事件/");
  expect(wide.texts().join("\n")).toContain("节点账本暂时不可用");
  await wide.press("label:重试读取节点事件");
  await wide.waitUntil(() => !wide.texts().join("\n").includes("没能读到这个节点的会话事件"));
  expect(wide.texts().join("\n")).not.toContain("没能读到这个节点的会话事件");
}, 180_000);
