import { describe, expect, test } from "bun:test";
import { ConversationProjection } from "../src/projection";
import type { WireEvent } from "../src/types";
import {
  assistantMessage,
  compaction,
  decision,
  goalEvent,
  runFinished,
  toolResult,
  userMessage,
} from "./fixtures";

function projection(windowTurns = 0): ConversationProjection {
  return new ConversationProjection({ sessionId: "s-1", now: () => 1_000, windowTurns });
}

describe("turn projection", () => {
  test("user prompt + tool round trip + final answer", () => {
    const p = projection();
    p.apply(userMessage(1, "hi", { requestId: "r1" }));
    p.apply(
      assistantMessage(2, {
        text: "Let me check.",
        toolCalls: [{ toolCallId: "c1", name: "echo", args: { x: 1 } }],
        inputSeqs: [1],
      }),
    );
    p.apply(toolResult(3, { toolCallId: "c1", text: "ok", durationMs: 12, timestamp: 1_003 }));
    p.apply(assistantMessage(4, { text: "Done", inputSeqs: [1] }));
    p.apply(runFinished(5, { stopReason: "done", turns: 2, tokensIn: 10, tokensOut: 5, durationMs: 200 }));

    const view = p.view();
    expect(view.turns).toHaveLength(1);
    const turn = view.turns[0]!;
    expect(turn.turnId).toBe("t_1");
    expect(turn.origin).toBe("user_prompt");
    expect(turn.status).toBe("completed");
    expect(turn.visibleInputs.map((i) => i.requestId)).toEqual(["r1"]);
    expect(turn.workSegments).toHaveLength(1);
    const segment = turn.workSegments[0]!;
    expect(segment.segmentId).toBe("t_1:s0");
    expect(segment.triggerInput?.requestId).toBe("r1");
    expect(segment.toolCalls).toHaveLength(1);
    expect(segment.toolCalls[0]!.name).toBe("echo");
    expect(segment.toolCalls[0]!.result).toBe("ok");
    expect(segment.toolCalls[0]!.finishedAt).toBe(1_003);
    expect(segment.toolCalls[0]!.startedAt).toBe(991);
    expect(turn.assistantOutput).toBe("Let me check.\n\nDone");
    expect(turn.runSummary).toEqual({
      turns: 2,
      tokensIn: 10,
      tokensOut: 5,
      stopReason: "done",
      durationMs: 200,
    });
    expect(view.busy).toBe(false);
    expect(view.queue).toHaveLength(0);
  });

  test("a guide injection opens a second work segment inside the same turn", () => {
    const p = projection();
    p.apply(userMessage(1, "first", { requestId: "r1" }));
    p.apply(
      assistantMessage(2, {
        text: "step one",
        toolCalls: [{ toolCallId: "c1", name: "echo" }],
        inputSeqs: [1],
      }),
    );
    p.apply(userMessage(3, "guide me", { requestId: "r2", delivery: "guide" }));
    expect(p.view().queue.map((q) => [q.requestId, q.delivery])).toEqual([["r2", "guide"]]);

    p.apply(assistantMessage(4, { text: "step two", inputSeqs: [1, 3] }));

    const view = p.view();
    expect(view.turns).toHaveLength(1);
    const turn = view.turns[0]!;
    expect(turn.visibleInputs.map((i) => i.requestId)).toEqual(["r1", "r2"]);
    expect(turn.workSegments).toHaveLength(2);
    expect(turn.workSegments[1]!.triggerInput?.requestId).toBe("r2");
    expect(turn.workSegments[1]!.assistantText).toBe("step two");
    expect(turn.assistantOutput).toBe("step one\n\nstep two");
    expect(view.queue).toHaveLength(0);
  });

  test("queue consumption at the ready boundary opens an origin=queue_drain turn", () => {
    const p = projection();
    p.apply(userMessage(1, "first", { requestId: "r1" }));
    p.apply(assistantMessage(2, { text: "working", inputSeqs: [1] }));

    p.submit("queued", "queue", "r2");
    p.apply(userMessage(3, "queued", { requestId: "r2" }));
    expect(p.view().queue.map((q) => q.requestId)).toEqual(["r2"]);

    p.apply(runFinished(4, { stopReason: "done" }));
    expect(p.view().busy).toBe(false);
    expect(p.view().queue.map((q) => q.requestId)).toEqual(["r2"]);

    // inputSeqs is the whole model transcript: the old input repeats.
    p.apply(assistantMessage(5, { text: "answer to the queue", inputSeqs: [1, 3] }));

    const view = p.view();
    expect(view.turns).toHaveLength(2);
    const second = view.turns[1]!;
    expect(second.origin).toBe("queue_drain");
    expect(second.turnId).toBe("t_3");
    expect(second.visibleInputs.map((i) => i.requestId)).toEqual(["r2"]);
    expect(second.assistantOutput).toBe("answer to the queue");
    expect(view.queue).toHaveLength(0);
  });

  test("queue and guide mixed: guide lands in the current turn, queue in the next", () => {
    const p = projection();
    p.apply(userMessage(1, "first", { requestId: "r1" }));
    p.apply(assistantMessage(2, { text: "working", inputSeqs: [1] }));

    p.submit("queued", "queue", "r2");
    p.apply(userMessage(3, "queued", { requestId: "r2" }));
    p.submit("steer", "guide", "r3");
    p.apply(userMessage(4, "steer", { requestId: "r3", delivery: "guide" }));

    p.apply(assistantMessage(5, { text: "steered", inputSeqs: [1, 4] }));
    let view = p.view();
    expect(view.turns).toHaveLength(1);
    expect(view.turns[0]!.visibleInputs.map((i) => i.requestId)).toEqual(["r1", "r3"]);
    expect(view.turns[0]!.workSegments).toHaveLength(2);
    expect(view.queue.map((q) => q.requestId)).toEqual(["r2"]);

    p.apply(runFinished(6, { stopReason: "done" }));
    p.apply(assistantMessage(7, { text: "queue answered", inputSeqs: [1, 3] }));
    view = p.view();
    expect(view.turns).toHaveLength(2);
    expect(view.turns[1]!.origin).toBe("queue_drain");
    expect(view.turns[1]!.visibleInputs.map((i) => i.requestId)).toEqual(["r2"]);
    expect(view.queue).toHaveLength(0);
  });

  test("a guide waits for the tool-result boundary (projection follows inputSeqs)", () => {
    const p = projection();
    p.apply(userMessage(1, "first", { requestId: "r1" }));
    p.apply(assistantMessage(2, { toolCalls: [{ toolCallId: "c1", name: "echo" }], inputSeqs: [1] }));
    p.apply(userMessage(3, "steer", { requestId: "r2", delivery: "guide" }));
    p.apply(toolResult(4, { toolCallId: "c1", text: "ok" }));
    expect(p.view().queue.map((q) => q.requestId)).toEqual(["r2"]);
    expect(p.view().turns[0]!.workSegments).toHaveLength(1);

    p.apply(assistantMessage(5, { text: "steered after the tool batch", inputSeqs: [1, 3] }));
    expect(p.view().queue).toHaveLength(0);
    expect(p.view().turns[0]!.workSegments).toHaveLength(2);
    expect(p.view().turns[0]!.workSegments[1]!.triggerInput?.requestId).toBe("r2");
  });

  test("explicit origin wins", () => {
    const p = projection();
    p.apply(userMessage(1, "goal", { requestId: "r1", origin: "goal_continuation" }));
    p.apply(runFinished(2, { stopReason: "done" }));
    expect(p.view().turns[0]!.origin).toBe("goal_continuation");
  });

  test("cancelled and failed runs interrupt/fail the turn but keep content", () => {
    for (const [reason, status] of [
      ["cancelled", "interrupted"],
      ["error", "failed"],
    ] as const) {
      const p = projection();
      p.apply(userMessage(1, "hi", { requestId: "r1" }));
      p.apply(assistantMessage(2, { text: "partial answer", inputSeqs: [1] }));
      p.apply(runFinished(3, { stopReason: reason }));
      const turn = p.view().turns[0]!;
      expect(turn.status).toBe(status);
      expect(turn.assistantOutput).toBe("partial answer");
    }
  });

  test("assistant stopReason=error fails the turn immediately", () => {
    const p = projection();
    p.apply(userMessage(1, "hi", { requestId: "r1" }));
    p.apply(assistantMessage(2, { text: "", inputSeqs: [1], stopReason: "error" }));
    expect(p.view().turns[0]!.status).toBe("failed");
    expect(p.view().busy).toBe(false);
  });
});

describe("compaction / goal maintenance", () => {
  test("active compaction keeps the session busy and blocks auto-drain", () => {
    const p = projection();
    p.apply(userMessage(1, "hi", { requestId: "r1" }));
    p.apply(assistantMessage(2, { text: "working", inputSeqs: [1] }));
    p.submit("later", "queue", "r2");
    p.apply(userMessage(3, "later", { requestId: "r2" }));

    p.apply(compaction(4, { status: "started", summary: "summarizing" }));
    expect(p.isBusy()).toBe(true);
    expect(p.view().turns[0]!.postTurnEvents[0]).toMatchObject({ kind: "compaction", seq: 4 });
    expect(p.canAutoDrain()).toBe(false);

    p.apply(compaction(5, { status: "completed", summary: "short summary" }));
    expect(p.isBusy()).toBe(true); // the turn itself is still running

    p.apply(runFinished(6, { stopReason: "done" }));
    expect(p.isBusy()).toBe(false);
    expect(p.canAutoDrain()).toBe(true);
    expect(p.view().turns[0]!.postTurnEvents).toHaveLength(2);
  });

  test("goal verification started sets settling and blocks ready", () => {
    const p = projection();
    p.apply(userMessage(1, "hi", { requestId: "r1" }));
    p.apply(assistantMessage(2, { text: "answer", inputSeqs: [1] }));
    p.apply(goalEvent(3, "goal.verification.started", { status: "started", targetId: "g1" }));
    expect(p.view().turns[0]!.status).toBe("settling");
    expect(p.isBusy()).toBe(true);
    p.apply(goalEvent(4, "goal.verification.completed", { status: "completed", passed: true }));
    expect(p.isBusy()).toBe(true); // run.finished is the ready boundary
    p.apply(runFinished(5, { stopReason: "done" }));
    expect(p.isBusy()).toBe(false);
    expect(p.view().turns[0]!.status).toBe("completed");
  });

  test("manual compaction without a turn creates a manual_compact turn", () => {
    const p = projection();
    p.apply(compaction(1, { status: "started", manual: true, summary: "manual compact" }));
    let turn = p.view().turns[0]!;
    expect(turn.origin).toBe("manual_compact");
    expect(turn.status).toBe("running");
    p.apply(compaction(2, { status: "completed", manual: true, summary: "manual compact" }));
    turn = p.view().turns[0]!;
    expect(turn.status).toBe("completed");
  });
});

describe("decisions in the projection", () => {
  test("decision rows update in place", () => {
    const p = projection();
    p.apply(decision(1, { decisionId: "d1", question: "Ship it?" }));
    expect(p.view().decisions).toHaveLength(1);
    expect(p.view().decisions[0]!.status).toBe("open");

    p.apply(decision(2, { decisionId: "d1", status: "answered", answer: "yes" }));
    expect(p.view().decisions).toHaveLength(1);
    expect(p.view().decisions[0]).toMatchObject({ status: "answered", answer: "yes", seq: 1 });
  });
});

describe("determinism and bounds", () => {
  test("the same fixture projects byte-identical views", () => {
    const events: WireEvent[] = [
      userMessage(1, "hi", { requestId: "r1" }),
      assistantMessage(2, {
        text: "checking",
        thinking: "hmm",
        toolCalls: [{ toolCallId: "c1", name: "echo", args: { a: 1 } }],
        inputSeqs: [1],
      }),
      toolResult(3, { toolCallId: "c1", text: "ok", durationMs: 2 }),
      compaction(4, { status: "completed", summary: "s" }),
      assistantMessage(5, { text: "done", inputSeqs: [1] }),
      runFinished(6, { stopReason: "done", turns: 2, tokensIn: 3, tokensOut: 4, durationMs: 10 }),
      decision(7, { decisionId: "d1" }),
    ];
    const a = projection();
    const b = projection();
    a.hydrate(events);
    b.hydrate(events);
    expect(JSON.stringify(a.view())).toBe(JSON.stringify(b.view()));
  });

  test("the render window keeps only the newest turns", () => {
    const p = projection(50);
    let seq = 0;
    for (let i = 0; i < 300; i += 1) {
      p.apply(userMessage((seq += 1), `m${i}`, { requestId: `r${i}` }));
      p.apply(runFinished((seq += 1), { stopReason: "done" }));
    }
    const view = p.view();
    expect(view.turns).toHaveLength(50);
    expect(view.turns[0]!.ordinal).toBe(251);
    expect(view.turns[49]!.ordinal).toBe(300);
    expect(view.turns[49]!.visibleInputs[0]!.text).toBe("m299");
  });

  test("unknown and external events are kept as post-turn facts", () => {
    const p = projection();
    p.apply(userMessage(1, "hi", { requestId: "r1" }));
    p.apply(assistantMessage(2, { text: "x", inputSeqs: [1] }));
    p.apply({ seq: 3, kind: "external.event", payload: { type: "backend.note", text: "hello" } });
    p.apply({ seq: 4, kind: "future.kind", payload: { anything: true } });
    const turn = p.view().turns[0]!;
    expect(turn.postTurnEvents.map((e) => e.kind)).toEqual(["external.event", "future.kind"]);
  });
});
