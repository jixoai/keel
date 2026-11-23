// The queued run-state's source-of-truth rules, at the store boundary the UI
// actually reads (W2 follow-up review P1):
//   - a client queue fact is valid only while the server still says queued;
//   - attach() replays history through the same callback as live events, so a
//     historical message/finish must not wipe a queued row's fact;
//   - retry answers 202 with the same queue block as input/run.
import { expect, test } from "nsh/test";
import type { QueueFacts, SessionSummary } from "./contract";
import { KeelHttp, type FetchLike } from "./http";
import { store } from "./store";

const json = (status: number, body: unknown): Response =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

const QUEUE: QueueFacts = { position: 2, etaKnown: true, etaSeconds: 12, groupId: "g-1" };

let retryStatus = 202;
let retryBody: unknown = {};

const fetchImpl: FetchLike = async (input, init) => {
  const path = String(input).split("?")[0];
  const method = (init?.method ?? "GET").toUpperCase();
  if (method === "POST" && path === "/v1/sessions/s-queue/retry") return json(retryStatus, retryBody);
  if (method === "GET" && path === "/v1/sessions") return json(200, { sessions: [] });
  return json(404, { error: "not found" });
};

const prepare = (rows: SessionSummary[] = [{ sessionId: "s-queue", runStatus: "queued", queue: QUEUE }]) => {
  store.clearRefreshTimerForTest();
  store.useHTTPForTest(new KeelHttp("", fetchImpl));
  store.sessions.set(rows);
  store.activeId.set("s-queue");
};

const row = (): SessionSummary | undefined =>
  store.sessions().find((candidate) => candidate.sessionId === "s-queue");

test("mergeSessions: a non-queued server row drops the client queue fact", () => {
  prepare();
  store.mergeSessionsForTest([{ sessionId: "s-queue", runStatus: "running" }]);
  expect(row()?.runStatus).toBe("running");
  expect(row()?.queue).toBeUndefined();
  expect(store.activeQueue()).toBeUndefined();

  // Also when the server row itself carries a stale queue block.
  prepare();
  store.mergeSessionsForTest([
    { sessionId: "s-queue", runStatus: "done", queue: { position: 9, etaKnown: false, etaSeconds: null } },
  ]);
  expect(row()?.runStatus).toBe("done");
  expect(row()?.queue).toBeUndefined();
});

test("mergeSessions: a still-queued row keeps the admission fact", () => {
  prepare();
  // The listing never carries position/ETA; while the server still says
  // queued, the 202's fact stays valid (position may be stale until promoted).
  store.mergeSessionsForTest([{ sessionId: "s-queue", runStatus: "queued" }]);
  expect(row()?.queue).toEqual(QUEUE);
  expect(store.activeQueue()).toEqual(QUEUE);
});

test("historical replay through attach() does not clear a queued row", () => {
  prepare();
  store.applyLiveEventForTest({ seq: 3, kind: "run.finished", payload: { stopReason: "done" } });
  store.applyLiveEventForTest({ seq: 4, kind: "user.message", payload: { text: "old prompt" } });
  store.applyLiveEventForTest({ seq: 5, kind: "assistant.message", payload: { text: "old answer" } });
  store.clearRefreshTimerForTest();
  expect(row()?.runStatus).toBe("queued");
  expect(row()?.queue).toEqual(QUEUE);
});

test("retry: a queued 202 renders 排队中 with its position/ETA", async () => {
  prepare([{ sessionId: "s-queue", runStatus: "done" }]);
  retryStatus = 202;
  retryBody = {
    created: true,
    seq: 9,
    requestId: "retry-1",
    queued: { position: 2, etaKnown: true, etaSeconds: 12, groupId: "g-1" },
  };
  const ok = await store.retryTurn("turn-1");
  store.clearRefreshTimerForTest();
  expect(ok).toBe(true);
  expect(row()?.runStatus).toBe("queued");
  expect(row()?.queue).toEqual(QUEUE);
  expect(store.activeQueue()).toEqual(QUEUE);
});

test("retry: an admitted retry is running and clears any stale queue fact", async () => {
  prepare();
  retryStatus = 201;
  retryBody = { created: true, seq: 10, requestId: "retry-2" };
  const ok = await store.retryTurn("turn-2");
  store.clearRefreshTimerForTest();
  expect(ok).toBe(true);
  expect(row()?.runStatus).toBe("running");
  expect(row()?.queue).toBeUndefined();
  expect(store.activeQueue()).toBeUndefined();
});
