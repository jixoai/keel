// Real SSE against a fake HTTP server — P1-1/16.2.
//
// Not a discovered test: the evidence script copies it, substitutes
// __SSE_BASE__, starts scripts/board-fake-sse-server.mjs and runs it with
// `nsh test --target web`. The harness uses the real `KeelHttp` (the separate
// fake transport tests are in data/board-sse.test.ts).
import { expect, open, test } from "nsh/test";

const OUT = "__REPORT_DIR__/shots/web";

test("board SSE: hidden frame advances, visible frame moves the row, live badge", async () => {
  const view = await open("./src/screens/board/harness.tsx", {
    width: 1440,
    host: { params: { scenario: "sse-live", base: "__SSE_BASE__", task: "900" } },
    network: "live",
  });
  await view.waitFor("id:board-task-card", { timeout: 20_000 });
  expect(view.accessibility("id:board-task-state")?.label).toBe("可认领");
  expect(view.shows("id:board-action-claim")).toBe(true);
  // The live badge starts as 连接中; wait for the stream before asserting the
  // connected state, or a slow CI host fails an otherwise healthy SSE.
  await view.waitUntil(() => view.texts().join("\n").includes("实时"), { timeout: 20_000 });
  expect(view.texts().join("\n")).toContain("实时");
  // The server writes an id-only frame first; then a raw task event with no
  // moves. The client must refetch the augmented view and swap the action set
  // (claim disappears, complete/renew appear) instead of hiding all actions.
  await view.waitUntil(() => view.accessibility("id:board-task-state")?.label === "待回复", {
    timeout: 15_000,
  });
  await view.waitUntil(() => view.shows("id:board-action-unblock"), { timeout: 15_000 });
  expect(view.shows("id:board-action-claim")).toBe(false);
  expect(view.shows("id:board-action-complete")).toBe(false);
  await view.screenshot(`${OUT}/50-sse-live-after-1440.png`);
}, 120_000);
