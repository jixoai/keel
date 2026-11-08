import { describe, expect, test } from "bun:test";
import { ChatSurfaceEngine } from "../src/engine";
import { assistantMessage, runFinished, userMessage } from "./fixtures";

function engine(): ChatSurfaceEngine {
  return new ChatSurfaceEngine({ sessionId: "s-1", now: () => 5_000, windowTurns: 0 });
}

/** Opens a running turn whose first assistant message consumed the input. */
function busyEngine(): ChatSurfaceEngine {
  const e = engine();
  e.apply(userMessage(1, "first", { requestId: "r1" }));
  e.apply(assistantMessage(2, { text: "working", inputSeqs: [1] }));
  return e;
}

describe("optimistic input and authoritative convergence", () => {
  test("submit shows the input immediately and the user.message converges it", () => {
    const e = engine();
    e.submit("hello", "queue", "r1");
    expect(e.view().queue).toEqual([{ requestId: "r1", text: "hello", delivery: "queue" }]);

    e.apply(userMessage(1, "hello", { requestId: "r1" }));
    expect(e.view().queue).toHaveLength(0);
    expect(e.view().turns).toHaveLength(1);
    const input = e.view().turns[0]!.visibleInputs[0]!;
    expect(input.requestId).toBe("r1");
    expect(input.seq).toBe(1);
    expect(input.consumed).toBe(false);
  });

  test("submit is idempotent by requestId", () => {
    const e = engine();
    e.submit("hello", "queue", "r1");
    e.submit("hello again", "queue", "r1");
    expect(e.view().queue).toHaveLength(1);
    expect(e.view().queue[0]!.text).toBe("hello");
  });

  test("a late retry of a consumed requestId cannot create a phantom queue item", () => {
    const e = engine();
    e.submit("hello", "queue", "r1");
    e.apply(userMessage(1, "hello", { requestId: "r1" }));
    e.apply(assistantMessage(2, { text: "answer", inputSeqs: [1] }));
    e.apply(runFinished(3, { stopReason: "done" }));

    const before = JSON.stringify(e.view());
    const returned = e.submit("hello", "queue", "r1");
    expect(returned.requestId).toBe("r1");
    expect(returned.consumed).toBe(true);
    expect(JSON.stringify(e.view())).toBe(before);
  });

  test("queue input submitted while busy waits for the ready boundary", () => {
    const e = busyEngine();
    e.submit("later", "queue", "r2");
    expect(e.view().queue.map((q) => q.requestId)).toEqual(["r2"]);
    e.apply(userMessage(3, "later", { requestId: "r2" }));
    expect(e.view().queue.map((q) => q.requestId)).toEqual(["r2"]);
    expect(e.view().turns).toHaveLength(1);
  });
});

describe("guide mode", () => {
  test("a guide submitted while busy is displayed as pending guide", () => {
    const e = busyEngine();
    e.submit("steer", "guide", "r2");
    expect(e.view().queue).toEqual([{ requestId: "r2", text: "steer", delivery: "guide" }]);
  });

  test("guide downgraded locally when idle or stopping", () => {
    const idle = engine();
    idle.submit("guide while idle", "guide", "r1");
    expect(idle.view().queue[0]!.delivery).toBe("queue");

    const stopping = busyEngine();
    stopping.requestStop();
    stopping.submit("guide while stopping", "guide", "r2");
    expect(stopping.view().queue[0]!.delivery).toBe("queue");
  });

  test("server downgrade returns the same item to the queue", () => {
    const e = busyEngine();
    e.submit("steer", "guide", "r2");
    e.apply(userMessage(3, "steer", { requestId: "r2", delivery: "queue" }));
    expect(e.view().queue).toEqual([{ requestId: "r2", text: "steer", delivery: "queue" }]);
    // The InputView records the downgrade once it is consumed.
    e.apply(assistantMessage(4, { text: "guided?", inputSeqs: [1, 3] }));
    const input = e.view().turns[0]!.visibleInputs.find((i) => i.requestId === "r2");
    expect(input?.downgraded).toBe(true);
  });

  test("an unconsumed guide falls back to queue when the run ends", () => {
    const e = busyEngine();
    e.submit("steer", "guide", "r2");
    e.apply(userMessage(3, "steer", { requestId: "r2", delivery: "guide" }));
    expect(e.view().queue[0]!.delivery).toBe("guide");
    e.apply(runFinished(4, { stopReason: "done" }));
    expect(e.view().queue[0]!.delivery).toBe("queue");
    expect(e.view().turns[0]!.visibleInputs).toHaveLength(1);
  });

  test("a consumed guide opens a work segment and leaves the queue", () => {
    const e = busyEngine();
    e.submit("steer", "guide", "r2");
    e.apply(userMessage(3, "steer", { requestId: "r2", delivery: "guide" }));
    e.apply(assistantMessage(4, { text: "steered", inputSeqs: [1, 3] }));
    const turn = e.view().turns[0]!;
    expect(turn.workSegments).toHaveLength(2);
    expect(turn.workSegments[1]!.triggerInput?.requestId).toBe("r2");
    expect(e.view().queue).toHaveLength(0);
  });
});

describe("queue order", () => {
  test("downgrade keeps the original FIFO position", () => {
    const e = busyEngine();
    e.submit("one", "queue", "r2");
    e.submit("two", "queue", "r3");
    e.submit("three", "guide", "r4");
    expect(e.view().queue.map((q) => q.requestId)).toEqual(["r2", "r3", "r4"]);

    e.apply(userMessage(3, "one", { requestId: "r2" }));
    e.apply(userMessage(4, "two", { requestId: "r3" }));
    e.apply(userMessage(5, "three", { requestId: "r4", delivery: "queue" }));
    expect(e.view().queue.map((q) => q.requestId)).toEqual(["r2", "r3", "r4"]);
  });
});

describe("stop barrier", () => {
  test("stop parks the queue until an explicit resume", () => {
    const e = busyEngine();
    e.submit("later", "queue", "r2");
    e.apply(userMessage(3, "later", { requestId: "r2" }));

    e.requestStop();
    expect(e.view().stopping).toBe(true);
    expect(e.view().busy).toBe(true);
    expect(e.canAutoDrain()).toBe(false);

    e.apply(runFinished(4, { stopReason: "cancelled" }));
    expect(e.view().stopping).toBe(false);
    expect(e.view().busy).toBe(false);
    expect(e.view().queue.map((q) => q.requestId)).toEqual(["r2"]);
    expect(e.canAutoDrain()).toBe(false); // user stopped: no auto-send

    e.resumeAutoDrain();
    expect(e.canAutoDrain()).toBe(true);
  });

  test("a new submit is the explicit resume", () => {
    const e = busyEngine();
    e.submit("later", "queue", "r2");
    e.apply(userMessage(3, "later", { requestId: "r2" }));
    e.requestStop();
    e.apply(runFinished(4, { stopReason: "cancelled" }));
    expect(e.canAutoDrain()).toBe(false);
    e.submit("fresh", "queue", "r3");
    expect(e.canAutoDrain()).toBe(true);
    expect(e.view().queue.map((q) => q.requestId)).toEqual(["r2", "r3"]);
  });

  test("stop confirmation on done also parks the queue", () => {
    const e = busyEngine();
    e.submit("later", "queue", "r2");
    e.apply(userMessage(3, "later", { requestId: "r2" }));
    e.requestStop();
    e.apply(runFinished(4, { stopReason: "done" }));
    expect(e.view().stopping).toBe(false);
    expect(e.canAutoDrain()).toBe(false);
    e.resumeAutoDrain();
    expect(e.canAutoDrain()).toBe(true);
  });

  test("stop request while idle is a no-op", () => {
    const e = engine();
    e.requestStop();
    expect(e.view().stopping).toBe(false);
  });
});

describe("immediate (send now) reservation", () => {
  test("busy send-now stops the run and reserves the queue head", () => {
    const e = busyEngine();
    e.submit("normal", "queue", "r2");
    e.submitNow("urgent", "r3");

    expect(e.view().stopping).toBe(true);
    expect(e.view().queue.map((q) => [q.requestId, q.immediate])).toEqual([
      ["r3", true],
      ["r2", undefined],
    ]);
    expect(e.canAutoDrain()).toBe(false);
    expect(e.promotionReady()).toBeNull();

    e.apply(runFinished(3, { stopReason: "cancelled" }));
    expect(e.view().stopping).toBe(false);
    expect(e.canAutoDrain()).toBe(false); // the reservation preempts normal drain
    const ready = e.promotionReady();
    expect(ready).toEqual({ requestId: "r3", text: "urgent", delivery: "queue", immediate: true });

    // The host sends with the same requestId; the user.message converges it.
    e.apply(userMessage(4, "urgent", { requestId: "r3" }));
    expect(e.view().turns[1]!.visibleInputs[0]!.requestId).toBe("r3");
    expect(e.promotionReady()).toBeNull();
    expect(e.canAutoDrain()).toBe(false); // the promoted turn is now running
    e.apply(runFinished(5, { stopReason: "done" }));
    expect(e.canAutoDrain()).toBe(true); // r2 is now the head
  });

  test("send-now while idle is a normal submit", () => {
    const e = engine();
    e.submitNow("hello", "r1");
    expect(e.view().stopping).toBe(false);
    expect(e.view().queue).toEqual([{ requestId: "r1", text: "hello", delivery: "queue" }]);
  });

  test("send-now downgrades pending guides", () => {
    const e = busyEngine();
    e.submit("steer", "guide", "r2");
    e.submitNow("urgent", "r3");
    expect(e.view().queue.map((q) => [q.requestId, q.delivery])).toEqual([
      ["r3", "queue"],
      ["r2", "queue"],
    ]);
  });
});
