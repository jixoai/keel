// The run-state vocabulary's queue projection (W2 review P2): a durable
// placement queue entry must never read as 就绪/ready, and the FIFO
// position/ETA the server reported must be shown when it exists.
import { expect, test } from "nsh/test";
import { conversationState, queueDetail, runStateLabel, runStateTone, sessionState } from "./run-state";

test("sessionState: queued 映射为排队中，不再落到 ready", () => {
  expect(sessionState("queued")).toBe("queued");
  expect(sessionState("QUEUED")).toBe("queued");
  expect(runStateLabel.queued).toBe("排队中");
  expect(runStateTone.queued).toBe("neutral");
  // The other states keep their words.
  expect(sessionState("running")).toBe("running");
  expect(sessionState(undefined)).toBe("ready");
  expect(sessionState("done")).toBe("ready");
});

test("queueDetail: 有队列事实时展示位置与 ETA", () => {
  expect(queueDetail({ position: 2, etaKnown: true, etaSeconds: 12 })).toBe("排队中 · 第 2 位 · 约 12 秒");
  // A floor of one second: a positive ETA never renders as 约 0 秒.
  expect(queueDetail({ position: 1, etaKnown: true, etaSeconds: 0.2 })).toBe("排队中 · 第 1 位 · 约 1 秒");
});

test("queueDetail: 未知 ETA 只说位置，不伪造 0", () => {
  expect(queueDetail({ position: 3, etaKnown: false, etaSeconds: null })).toBe("排队中 · 第 3 位");
  expect(queueDetail(undefined)).toBe("排队中");
  // A queued run row without a position (e.g. refreshed list without the queue
  // block) still says 排队中 rather than inventing one.
  expect(queueDetail({ position: 0, etaKnown: false, etaSeconds: null })).toBe("排队中");
});

test("conversationState: 队列事实不改变对话投影（由调用方优先用 queue）", () => {
  const view = {
    connection: { status: "open" },
    decisions: [],
    stopping: false,
    busy: false,
    turns: [],
  } as unknown as Parameters<typeof conversationState>[0];
  expect(conversationState(view)).toBe("ready");
  expect(conversationState(null)).toBe("ready");
});
