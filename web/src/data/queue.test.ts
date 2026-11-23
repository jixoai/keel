// The 202 queue block parser: a missing/invalid body must yield "no queue
// fact" rather than a made-up position or a zero ETA, and a known ETA must keep
// the server's number.
import { expect, test } from "nsh/test";
import { parseQueueFacts } from "./queue";

test("parseQueueFacts: 读取 202 队列块的位置与已知 ETA", () => {
  const queue = parseQueueFacts({
    queued: { position: 2, etaKnown: true, etaSeconds: 12, groupId: "g-1", active: 1, limit: 2 },
  });
  expect(queue).toEqual({ position: 2, etaKnown: true, etaSeconds: 12, groupId: "g-1" });
});

test("parseQueueFacts: ETA 未知时 etaSeconds 为 null，不伪造 0", () => {
  const queue = parseQueueFacts({ queued: { position: 1, etaKnown: false } });
  expect(queue).toEqual({ position: 1, etaKnown: false, etaSeconds: null });
});

test("parseQueueFacts: 没有队列块或位置非法＝没有队列事实", () => {
  expect(parseQueueFacts(null)).toBeUndefined();
  expect(parseQueueFacts({})).toBeUndefined();
  expect(parseQueueFacts({ queued: null })).toBeUndefined();
  expect(parseQueueFacts({ queued: { position: 0 } })).toBeUndefined();
  expect(parseQueueFacts({ queued: { position: -3 } })).toBeUndefined();
  expect(parseQueueFacts({ queued: { position: "2" } })).toBeUndefined();
});

test("parseQueueFacts: etaKnown 为真但 etaSeconds 缺失/非数仍是未知", () => {
  expect(parseQueueFacts({ queued: { position: 1, etaKnown: true } })).toEqual({
    position: 1,
    etaKnown: true,
    etaSeconds: null,
  });
  expect(parseQueueFacts({ queued: { position: 1, etaKnown: true, etaSeconds: "12" } })?.etaSeconds)
    .toBeNull();
});
