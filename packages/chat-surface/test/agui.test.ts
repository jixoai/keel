import { describe, expect, test } from "bun:test";
import { toAgUi } from "../src/agui";
import type { AgUiEvent } from "../src/agui";
import type { ConversationView, WireEvent } from "../src/types";
import {
  assistantMessage,
  decision,
  goalEvent,
  runFinished,
  toolResult,
  userMessage,
} from "./fixtures";

function types(events: WireEvent[]): string[] {
  return toAgUi(events).map((e) => e.type);
}

describe("AG-UI mapping", () => {
  test("text + tool call + result follow the documented sequence", () => {
    const events: WireEvent[] = [
      userMessage(1, "check the file", { requestId: "r1" }),
      assistantMessage(2, {
        text: "Let me look.",
        inputSeqs: [1],
        toolCalls: [{ toolCallId: "c1", name: "read_file", args: { path: "a.txt" } }],
      }),
      toolResult(3, { toolCallId: "c1", text: "contents" }),
      runFinished(4, { stopReason: "done" }),
    ];
    expect(types(events)).toEqual([
      "RUN_STARTED",
      "STEP_STARTED",
      "CUSTOM", // keel.user.message
      "TEXT_MESSAGE_START",
      "TEXT_MESSAGE_CONTENT",
      "TEXT_MESSAGE_END",
      "TOOL_CALL_START",
      "TOOL_CALL_ARGS",
      "TOOL_CALL_END",
      "TOOL_CALL_RESULT",
      "STEP_FINISHED",
      "RUN_FINISHED",
    ]);

    const mapped = toAgUi(events);
    const args = mapped.find((e) => e.type === "TOOL_CALL_ARGS") as Extract<
      (typeof mapped)[number],
      { type: "TOOL_CALL_ARGS" }
    >;
    expect(args.delta).toBe('{"path":"a.txt"}');
    const result = mapped.find((e) => e.type === "TOOL_CALL_RESULT") as Extract<
      (typeof mapped)[number],
      { type: "TOOL_CALL_RESULT" }
    >;
    expect(result).toMatchObject({ toolCallId: "c1", content: "contents", role: "tool" });
    const text = mapped.find((e) => e.type === "TEXT_MESSAGE_CONTENT") as Extract<
      (typeof mapped)[number],
      { type: "TEXT_MESSAGE_CONTENT" }
    >;
    expect(text.delta).toBe("Let me look.");
  });

  test("thinking maps to AG-UI reasoning events", () => {
    const events: WireEvent[] = [
      userMessage(1, "hi", { requestId: "r1" }),
      assistantMessage(2, { text: "answer", thinking: "reasoning here", inputSeqs: [1] }),
      runFinished(3, { stopReason: "done" }),
    ];
    const mapped = toAgUi(events);
    expect(mapped.map((e) => e.type)).toContain("REASONING_START");
    expect(mapped.map((e) => e.type)).toContain("REASONING_MESSAGE_CONTENT");
    const reasoning = mapped.find((e) => e.type === "REASONING_MESSAGE_CONTENT") as Extract<
      (typeof mapped)[number],
      { type: "REASONING_MESSAGE_CONTENT" }
    >;
    expect(reasoning.delta).toBe("reasoning here");
  });

  test("an open decision becomes an HITL interrupt on RUN_FINISHED", () => {
    const events: WireEvent[] = [
      userMessage(1, "deploy", { requestId: "r1" }),
      decision(2, { decisionId: "d1", question: "Deploy now?" }),
      runFinished(3, { stopReason: "done" }),
    ];
    const mapped = toAgUi(events);
    const custom = mapped.find((e) => e.type === "CUSTOM" && e.name === "keel.decision");
    expect(custom).toBeDefined();
    const finish = mapped.find((e) => e.type === "RUN_FINISHED") as Extract<
      (typeof mapped)[number],
      { type: "RUN_FINISHED" }
    >;
    expect(finish.outcome).toEqual({
      type: "interrupt",
      interrupts: [{ id: "d1", reason: "decision", message: "Deploy now?" }],
    });
  });

  test("an answered decision no longer interrupts the run", () => {
    const events: WireEvent[] = [
      userMessage(1, "deploy", { requestId: "r1" }),
      decision(2, { decisionId: "d1", question: "Deploy now?" }),
      decision(3, { decisionId: "d1", status: "answered", answer: "yes" }),
      runFinished(4, { stopReason: "done" }),
    ];
    const finish = toAgUi(events).find((e) => e.type === "RUN_FINISHED") as Extract<
      AgUiEvent,
      { type: "RUN_FINISHED" }
    >;
    expect(finish.outcome).toEqual({ type: "success" });
  });

  test("a failed run maps to RUN_ERROR", () => {
    const events: WireEvent[] = [
      userMessage(1, "hi", { requestId: "r1" }),
      runFinished(2, { stopReason: "error", errorMessage: "provider 401" }),
    ];
    const mapped = toAgUi(events);
    const error = mapped.find((e) => e.type === "RUN_ERROR") as Extract<
      (typeof mapped)[number],
      { type: "RUN_ERROR" }
    >;
    expect(error).toMatchObject({ message: "provider 401", code: "error" });
    expect(mapped.some((e) => e.type === "RUN_FINISHED")).toBe(false);
  });

  test("turns become steps; a second turn closes and reopens the step", () => {
    const events: WireEvent[] = [
      userMessage(1, "one", { requestId: "r1" }),
      assistantMessage(2, { text: "a1", inputSeqs: [1] }),
      runFinished(3, { stopReason: "done" }),
      userMessage(4, "two", { requestId: "r4" }),
      assistantMessage(5, { text: "a2", inputSeqs: [4] }),
      runFinished(6, { stopReason: "done" }),
    ];
    const mapped = toAgUi(events);
    const steps = mapped.filter((e) => e.type === "STEP_STARTED" || e.type === "STEP_FINISHED");
    expect(steps).toEqual([
      { type: "STEP_STARTED", stepName: "turn:t_1" },
      { type: "STEP_FINISHED", stepName: "turn:t_1" },
      { type: "STEP_STARTED", stepName: "turn:t_4" },
      { type: "STEP_FINISHED", stepName: "turn:t_4" },
    ]);
    expect(mapped.filter((e) => e.type === "RUN_STARTED")).toHaveLength(2);
    expect(mapped.filter((e) => e.type === "RUN_FINISHED")).toHaveLength(2);
  });

  test("state snapshot from the view and state events from payloads", () => {
    const view = { sessionId: "s", turns: [], decisions: [], queue: [], busy: false, stopping: false, connection: { status: "open", lastSeq: 1 } } as ConversationView;
    const events: WireEvent[] = [
      userMessage(1, "hi", { requestId: "r1" }),
      { seq: 2, kind: "state.snapshot", payload: { state: { mode: "x" } } },
      { seq: 3, kind: "state.patch", payload: { patch: [{ op: "replace", path: "/mode", value: "y" }] } },
    ];
    const mapped = toAgUi(events, { view, threadId: "thread-1" });
    expect(mapped[0]).toEqual({ type: "STATE_SNAPSHOT", snapshot: view });
    expect(mapped[1]).toMatchObject({ type: "RUN_STARTED", threadId: "thread-1" });
    expect(mapped.some((e) => e.type === "STATE_SNAPSHOT" && (e as { snapshot: unknown }).snapshot !== view)).toBe(true);
    const delta = mapped.find((e) => e.type === "STATE_DELTA") as Extract<
      (typeof mapped)[number],
      { type: "STATE_DELTA" }
    >;
    expect(delta.delta).toEqual([{ op: "replace", path: "/mode", value: "y" }]);
  });

  test("goal and unknown events become custom events, duplicates are ignored", () => {
    const events: WireEvent[] = [
      userMessage(1, "hi", { requestId: "r1" }),
      goalEvent(2, "goal.verification.started", { status: "started" }),
      { seq: 3, kind: "something.new", payload: { x: 1 } },
      { seq: 3, kind: "something.new", payload: { x: 1 } },
    ];
    const mapped = toAgUi(events);
    expect(mapped.map((e) => e.type)).toContain("CUSTOM");
    const names = mapped.filter((e) => e.type === "CUSTOM").map((e) => (e as { name: string }).name);
    expect(names).toContain("keel.goal");
    expect(names.filter((n) => n === "keel.event")).toHaveLength(1);
  });

  test("the mapping is deterministic", () => {
    const events: WireEvent[] = [
      userMessage(1, "hi", { requestId: "r1" }),
      assistantMessage(2, { text: "x", thinking: "y", inputSeqs: [1], toolCalls: [{ toolCallId: "c", name: "t" }] }),
      toolResult(3, { toolCallId: "c", text: "ok" }),
      runFinished(4, { stopReason: "done" }),
    ];
    expect(JSON.stringify(toAgUi(events))).toBe(JSON.stringify(toAgUi(events)));
  });
});
