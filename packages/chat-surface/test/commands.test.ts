// command.invoked projection (change keel-plugin-commands, task 4.2).
//
// A command is a named user action, not a message: the projection keeps one row
// per requestId, replaces a destructive command's pending phase with its
// terminal outcome, and never fabricates a user/assistant event. `/compact`
// keeps its existing compaction presentation; the command row is the invocation
// record beside it.
import { describe, expect, test } from "bun:test";
import { ConversationProjection } from "../src/projection";
import type { WireEvent } from "../src/types";
import { compaction, userMessage } from "./fixtures";

function projection(): ConversationProjection {
  return new ConversationProjection({ sessionId: "s-commands", now: () => 1_000, windowTurns: 0 });
}

function commandEvent(seq: number, payload: Record<string, unknown>): WireEvent {
  return { seq, kind: "command.invoked", payload };
}

describe("command.invoked projection", () => {
  test("one successful invocation is one row, not a message", () => {
    const p = projection();
    p.apply(
      commandEvent(1, {
        requestId: "cmd-1",
        command: "compact",
        source: "builtin",
        status: "ok",
        argsHash: "sha256:abc",
        argsBytes: 2,
        durationMs: 87,
        data: { status: "ok", summary: "compacted" },
      }),
    );
    const view = p.view();
    expect(view.turns).toHaveLength(0);
    expect(view.commands).toHaveLength(1);
    expect(view.commands[0]).toMatchObject({
      requestId: "cmd-1",
      command: "compact",
      source: "builtin",
      status: "ok",
      seq: 1,
      durationMs: 87,
      argsHash: "sha256:abc",
      argsBytes: 2,
    });
    expect(view.commands[0]!.data).toEqual({ status: "ok", summary: "compacted" });
    // The projection never writes user/assistant facts for a command.
    expect(view.turns).toHaveLength(0);
  });

  test("pending_decision is replaced in place by the terminal outcome", () => {
    const p = projection();
    p.apply(
      commandEvent(1, {
        requestId: "cmd-destructive",
        command: "vendor.search:purge",
        source: "plugin",
        status: "pending_decision",
        decisionId: "dec-1",
        argsHash: "sha256:def",
        argsBytes: 20,
      }),
    );
    expect(p.view().commands).toHaveLength(1);
    expect(p.view().commands[0]!.status).toBe("pending_decision");
    expect(p.view().commands[0]!.decisionId).toBe("dec-1");

    p.apply(
      commandEvent(2, {
        requestId: "cmd-destructive",
        command: "vendor.search:purge",
        source: "plugin",
        status: "refused",
        decisionId: "dec-1",
        argsHash: "sha256:def",
        argsBytes: 20,
        durationMs: 4_200,
        errorCode: "decision_refused",
        error: "the destructive command was refused",
      }),
    );
    const view = p.view();
    expect(view.commands).toHaveLength(1);
    expect(view.commands[0]).toMatchObject({
      status: "refused",
      seq: 2,
      durationMs: 4_200,
      errorCode: "decision_refused",
      error: "the destructive command was refused",
    });
  });

  test("failure, timeout and cancellation stay visible with their stable code", () => {
    const p = projection();
    const failures = [
      ["cmd-error", "error", "tool_error"],
      ["cmd-timeout", "timed_out", "decision_timeout"],
      ["cmd-cancel", "cancelled", "decision_cancelled"],
    ] as const;
    failures.forEach(([requestId, status, code], index) => {
      p.apply(
        commandEvent(index + 1, {
          requestId,
          command: "vendor.search:reindex",
          source: "plugin",
          status,
          errorCode: code,
          error: `failure ${code}`,
        }),
      );
    });
    const view = p.view();
    expect(view.commands.map((row) => row.status)).toEqual(["error", "timed_out", "cancelled"]);
    expect(view.commands.map((row) => row.errorCode)).toEqual([
      "tool_error",
      "decision_timeout",
      "decision_cancelled",
    ]);
    expect(view.commands.every((row) => !!row.error)).toBe(true);
  });

  test("a truncated result is marked, and an argument document is never carried", () => {
    const p = projection();
    p.apply(
      commandEvent(1, {
        requestId: "cmd-big",
        command: "vendor.search:reindex",
        source: "plugin",
        status: "ok",
        argsHash: "sha256:xyz",
        argsBytes: 9,
        truncated: true,
        data: { truncated: true, bytes: 90_000 },
      }),
    );
    const row = p.view().commands[0]!;
    expect(row.truncated).toBe(true);
    expect(row.data).toEqual({ truncated: true, bytes: 90_000 });
    expect(JSON.stringify(row)).not.toContain('"args"');
  });

  test("compaction keeps its own presentation beside the command row", () => {
    const p = projection();
    p.apply(commandEvent(1, { requestId: "cmd-c", command: "compact", source: "builtin", status: "ok" }));
    p.apply(compaction(2, { summary: "上下文已压缩", manual: true }));
    p.apply(compaction(3, { summary: "done", status: "completed", manual: true }));
    const view = p.view();
    expect(view.commands).toHaveLength(1);
    // The manual compaction still creates its maintenance turn with a
    // compaction post-turn event (the existing contract).
    expect(view.turns.length).toBeGreaterThanOrEqual(1);
    expect(view.turns.every((turn) => turn.origin === "manual_compact")).toBe(true);
    expect(view.turns.flatMap((turn) => turn.postTurnEvents.map((event) => event.kind))).toContain(
      "compaction",
    );
  });

  test("historical commands restore on hydrate in ledger order", () => {
    const p = projection();
    p.hydrate([
      commandEvent(1, { requestId: "a", command: "compact", source: "builtin", status: "ok" }),
      userMessage(2, "hi", { requestId: "r1" }),
      commandEvent(3, { requestId: "b", command: "skill:deploy-flow", source: "skill", status: "ok" }),
    ]);
    const view = p.view();
    expect(view.commands.map((row) => row.requestId)).toEqual(["a", "b"]);
    expect(view.turns).toHaveLength(1);
  });
});
