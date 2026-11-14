// TUI gate · the event stream survives a transient 404.
//
// A 404 is retried (bounded) instead of closing the stream. This matters because
// "closed" is terminal in the store: the session would stop receiving live
// updates and the badge would read「未连接」until a manual refresh.
import { expect, open, test } from "nsh/test";

test("事件流：连续 404 后有界重试并成功 open，全程不 closed", async () => {
  const app = await open("./stream-retry.fixture.tsx", { clock: {} });
  for (let i = 0; i < 6; i += 1) await app.clock!.runFor(1000);
  const lines = app.texts();
  const log = lines.filter((t) => t.includes("reconnecting")).pop() ?? "";
  // The client retried the 404s and reached the open state…
  expect(log.includes("open")).toBe(true);
  // …and never treated a 404 as terminal (the store has no re-subscribe path).
  expect(log.includes("closed")).toBe(false);
});
