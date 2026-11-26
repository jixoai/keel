// E2E evidence capture template — the sub-agent tree run page.
//
// This file is NOT a discovered test (its name ends .template.tsx). The
// evidence run copies it to `web/subagent-e2e-capture.test.tsx`, replaces
// `__REPORT_DIR__`, and runs it once per target:
//
//   nsh test --target web subagent-e2e-capture.test.tsx
//   nsh test --target tui subagent-e2e-capture.test.tsx
//
// The same steps run on both targets. `view.screenshot()` returns PNG bytes on
// the web host and `nsh-tui-cells` JSON on the TUI host, so the helper keeps
// each file in the directory/extension its bytes say it is. Every shot follows
// a real state transition; nothing is drawn from a fixture after the fact.
import { open, test } from "nsh/test";

const OUT = "__REPORT_DIR__/shots";

const harness = (width: number, scenario: string) =>
  open("./src/screens/books/run-subagent.harness.tsx", { width, host: { params: { scenario } } });

const shot = async (view: { screenshot: (path?: string | null) => Promise<Uint8Array> }, name: string) => {
  const probe = await view.screenshot();
  const png = probe[0] === 0x89 && probe[1] === 0x50;
  await view.screenshot(`${OUT}/${png ? "web" : "tui"}/${name}.${png ? "png" : "json"}`);
};

test("subagent tree evidence", async () => {
  const loading = await harness(1440, "run-loading");
  await loading.waitFor("id:run-loading");
  await shot(loading, "01-loading-1440");

  const empty = await harness(1440, "run-empty");
  await empty.waitFor("id:run-empty");
  await shot(empty, "02-empty-1440");

  const broken = await harness(1440, "run-error");
  await broken.waitFor("id:run-error");
  await shot(broken, "03-error-1440");

  const denied = await harness(1440, "run-permission");
  await denied.waitFor("id:run-lanes");
  await shot(denied, "04-permission-1440");

  const success = await harness(1440, "run-success");
  await success.waitFor("id:run-lanes");
  await shot(success, "05-success-lanes-1440");
  await success.press("id:run-mode-graph");
  await success.waitFor("id:run-graph");
  await shot(success, "06-success-graph-1440");

  const cancel = await harness(1440, "run-cancel");
  await cancel.waitFor("id:run-lanes");
  await cancel.press("id:run-stop");
  await cancel.waitFor("id:run-stop-dialog");
  await shot(cancel, "07-cancel-dialog-1440");
  await cancel.press("id:run-stop-dialog-confirm");
  await cancel.waitFor("id:run-stop-notice");
  await cancel.waitFor("text:已停止");
  await shot(cancel, "08-cancel-stopped-1440");

  const retry = await harness(1440, "run-retry");
  await retry.waitFor("id:run-lanes");
  await retry.waitFor("text:/没能读到这个节点的会话事件/");
  await shot(retry, "09-retry-error-1440");
  await retry.press("label:重试读取节点事件");
  await retry.waitUntil(() => !retry.texts().join("\n").includes("没能读到这个节点的会话事件"));
  await shot(retry, "10-retry-done-1440");

  const phoneLoading = await harness(390, "run-loading");
  await phoneLoading.waitFor("id:run-loading");
  await shot(phoneLoading, "11-loading-390");

  const phoneEmpty = await harness(390, "run-empty");
  await phoneEmpty.waitFor("id:run-empty");
  await shot(phoneEmpty, "12-empty-390");

  const phoneError = await harness(390, "run-error");
  await phoneError.waitFor("id:run-error");
  await shot(phoneError, "13-error-390");

  const phoneDenied = await harness(390, "run-permission");
  await phoneDenied.waitFor("id:run-timeline");
  await shot(phoneDenied, "14-permission-390");

  const phoneSuccess = await harness(390, "run-success");
  await phoneSuccess.waitFor("id:run-timeline");
  await shot(phoneSuccess, "15-success-timeline-390");
  await phoneSuccess.press("id:run-mode-graph");
  await phoneSuccess.waitFor("id:run-graph");
  await shot(phoneSuccess, "16-success-graph-390");

  const phoneCancel = await harness(390, "run-cancel");
  await phoneCancel.waitFor("id:run-timeline");
  await phoneCancel.press("id:run-stop-mobile");
  await phoneCancel.waitFor("id:run-stop-dialog");
  await shot(phoneCancel, "17-cancel-dialog-390");
  await phoneCancel.press("id:run-stop-dialog-confirm");
  await phoneCancel.waitFor("id:run-stop-notice");
  await phoneCancel.waitFor("text:已停止");
  await shot(phoneCancel, "18-cancel-stopped-390");

  const phoneRetry = await harness(390, "run-retry");
  await phoneRetry.waitFor("id:run-timeline");
  await phoneRetry.press("id:run-inspector-open-mobile");
  await phoneRetry.waitFor("text:/没能读到这个节点的会话事件/");
  await shot(phoneRetry, "19-retry-error-390");
  await phoneRetry.press("label:重试读取节点事件");
  await phoneRetry.waitUntil(() => !phoneRetry.texts().join("\n").includes("没能读到这个节点的会话事件"));
  await shot(phoneRetry, "20-retry-done-390");
}, 1_200_000);
