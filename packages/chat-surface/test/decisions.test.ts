import { describe, expect, test } from "bun:test";
import { ChatSurfaceEngine } from "../src/engine";
import { decision } from "./fixtures";

function engine(): ChatSurfaceEngine {
  return new ChatSurfaceEngine({ sessionId: "s", now: () => 1, windowTurns: 0 });
}

describe("decision rows", () => {
  test("open decision carries question, options and recommendation", () => {
    const e = engine();
    e.apply(
      decision(1, {
        decisionId: "d1",
        question: "Deploy?",
        options: [
          { optionId: "yes", label: "Deploy", description: "ship it" },
          { optionId: "no", label: "Cancel" },
        ],
        recommendation: "yes",
      }),
    );
    expect(e.view().decisions).toEqual([
      {
        decisionId: "d1",
        seq: 1,
        question: "Deploy?",
        options: [
          { optionId: "yes", label: "Deploy", description: "ship it" },
          { optionId: "no", label: "Cancel" },
        ],
        recommendation: "yes",
        status: "open",
      },
    ]);
  });

  test("one answer wins; a duplicate answer does not overwrite it", () => {
    const e = engine();
    e.apply(decision(1, { decisionId: "d1" }));
    e.apply(decision(2, { decisionId: "d1", status: "answered", answer: "yes" }));
    e.apply(decision(3, { decisionId: "d1", status: "answered", answer: "no" }));
    const row = e.view().decisions[0]!;
    expect(row.status).toBe("answered");
    expect(row.answer).toBe("yes");
    expect(e.view().decisions).toHaveLength(1);
  });

  test("timeout and cancellation are terminal and disable answering", () => {
    const e = engine();
    e.apply(decision(1, { decisionId: "d1" }));
    e.apply(decision(2, { decisionId: "d1", status: "timeout" }));
    expect(e.view().decisions[0]!.status).toBe("timeout");
    expect(e.answerDecision("d1", "yes")).toBe(false);

    e.apply(decision(3, { decisionId: "d2" }));
    e.apply(decision(4, { decisionId: "d2", status: "cancelled" }));
    expect(e.view().decisions[1]!.status).toBe("cancelled");
  });

  test("optimistic answerDecision is first-write-wins", () => {
    const e = engine();
    e.apply(decision(1, { decisionId: "d1" }));
    expect(e.answerDecision("d1", "yes")).toBe(true);
    expect(e.answerDecision("d1", "no")).toBe(false);
    expect(e.view().decisions[0]).toMatchObject({ status: "answered", answer: "yes" });
  });

  test("the agentext Decision document shape is projected", () => {
    const e = engine();
    // Mirrors internal/agentext.Decision: prompt/id options/DecisionAnswer.
    e.apply({
      seq: 1,
      kind: "decision",
      payload: {
        decisionId: "d1",
        kind: "approval",
        prompt: "Allow the rm command?",
        options: [
          { id: "allow", label: "Allow" },
          { id: "deny", label: "Deny" },
        ],
        allowFreeText: false,
        status: "open",
        createdAt: "2026-11-06T00:00:00Z",
        deadline: "2026-11-06T00:05:00Z",
      },
    });
    expect(e.view().decisions[0]).toMatchObject({
      decisionId: "d1",
      question: "Allow the rm command?",
      options: [
        { optionId: "allow", label: "Allow" },
        { optionId: "deny", label: "Deny" },
      ],
      status: "open",
    });

    e.apply({
      seq: 2,
      kind: "decision",
      payload: {
        decisionId: "d1",
        status: "answered",
        answer: { optionId: "allow", text: "", at: "2026-11-06T00:00:10Z" },
      },
    });
    expect(e.view().decisions[0]).toMatchObject({ status: "answered", answer: "allow" });

    e.apply({
      seq: 3,
      kind: "decision",
      payload: { decisionId: "d2", prompt: "Timed out?", status: "timed_out" },
    });
    expect(e.view().decisions[1]!.status).toBe("timeout");
  });

  test("decision kind aliases are projected too", () => {
    const e = engine();
    e.apply({ seq: 1, kind: "decision.opened", payload: { decisionId: "d9", question: "Q?" } });
    e.apply({ seq: 2, kind: "decision.answered", payload: { decisionId: "d9", answer: "yes" } });
    expect(e.view().decisions[0]).toMatchObject({ decisionId: "d9", status: "answered", answer: "yes" });
  });
});
