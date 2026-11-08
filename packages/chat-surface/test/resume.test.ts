import { describe, expect, test } from "bun:test";
import { ChatSurfaceEngine } from "../src/engine";
import type { ConnectionState, EventSource, WireEvent } from "../src/types";
import { assistantMessage, runFinished, userMessage } from "./fixtures";

class FakeEventSource implements EventSource {
  events: WireEvent[] = [];
  calls: string[] = [];
  private onEvent: ((e: WireEvent) => void) | null = null;
  private onState: ((s: ConnectionState["status"]) => void) | null = null;
  private onPartial: ((p: { text: string; turnId?: string }) => void) | null = null;
  private cancelled = false;

  async list(after: number, limit = 500): Promise<WireEvent[]> {
    this.calls.push(`list:${after}`);
    return this.events
      .filter((e) => e.seq > after)
      .sort((a, b) => a.seq - b.seq)
      .slice(0, limit);
  }

  stream(opts: {
    after: number;
    partials?: boolean;
    onEvent: (e: WireEvent) => void;
    onPartial?: (p: { text: string; turnId?: string }) => void;
    onState: (s: ConnectionState["status"]) => void;
  }): () => void {
    this.calls.push(`stream:${opts.after}:${opts.partials === true}`);
    this.onEvent = opts.onEvent;
    this.onState = opts.onState;
    this.onPartial = opts.onPartial ?? null;
    return () => {
      this.cancelled = true;
      this.calls.push("cancel");
    };
  }

  emit(event: WireEvent): void {
    this.onEvent?.(event);
  }

  emitPartial(partial: { text: string; turnId?: string }): void {
    this.onPartial?.(partial);
  }

  setState(status: ConnectionState["status"]): void {
    this.onState?.(status);
  }

  isCancelled(): boolean {
    return this.cancelled;
  }
}

async function flushAsync(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await new Promise((resolve) => setTimeout(resolve, 0));
}

describe("idempotence and cursors", () => {
  test("duplicate (session, seq) events are ignored", () => {
    const e = new ChatSurfaceEngine({ sessionId: "s", now: () => 1, windowTurns: 0 });
    const user = userMessage(1, "hi", { requestId: "r1" });
    expect(e.apply(user)).toBe("applied");
    const before = JSON.stringify(e.view());
    expect(e.apply(user)).toBe("duplicate");
    expect(JSON.stringify(e.view())).toBe(before);
  });

  test("stale seqs never rewind the projection", () => {
    const e = new ChatSurfaceEngine({ sessionId: "s", now: () => 1, windowTurns: 0 });
    e.apply(userMessage(5, "five", { requestId: "r5" }));
    expect(e.apply(userMessage(3, "three", { requestId: "r3" }))).toBe("duplicate");
    expect(e.view().turns[0]!.visibleInputs[0]!.text).toBe("five");
  });

  test("a gap buffers late events until the missing seq arrives", () => {
    const e = new ChatSurfaceEngine({ sessionId: "s", now: () => 1, windowTurns: 0 });
    e.apply(userMessage(1, "one", { requestId: "r1" }));
    expect(e.apply(runFinished(3, { stopReason: "done" }))).toBe("buffered");
    expect(e.needsResync()).toBe(true);
    expect(e.view().turns[0]!.status).toBe("running");

    expect(e.apply(assistantMessage(2, { text: "answer", inputSeqs: [1] }))).toBe("applied");
    expect(e.needsResync()).toBe(false);
    expect(e.view().turns[0]!.status).toBe("completed");
    expect(e.view().turns[0]!.assistantOutput).toBe("answer");
  });

  test("hydrate rebuilds without duplicates and preserves optimistic input", () => {
    const e = new ChatSurfaceEngine({ sessionId: "s", now: () => 7, windowTurns: 0 });
    e.submit("pending", "queue", "r2");
    e.hydrate([
      userMessage(1, "first", { requestId: "r1" }),
      assistantMessage(2, { text: "answer", inputSeqs: [1] }),
      runFinished(3, { stopReason: "done" }),
    ]);
    expect(e.view().turns).toHaveLength(1);
    expect(e.view().queue.map((q) => q.requestId)).toEqual(["r2"]);

    // The optimistic item converges when its authoritative event arrives.
    e.apply(userMessage(4, "pending", { requestId: "r2" }));
    expect(e.view().queue).toHaveLength(0);
    expect(e.view().turns).toHaveLength(2);
  });

  test("hydrate keeps the seq watermark even when the snapshot is partial", () => {
    const e = new ChatSurfaceEngine({ sessionId: "s", now: () => 1, windowTurns: 0 });
    e.apply(userMessage(1, "first", { requestId: "r1" }));
    e.apply(runFinished(2, { stopReason: "done" }));
    e.hydrate([userMessage(1, "first", { requestId: "r1" })]);
    expect(e.lastSeq()).toBe(2);
    expect(e.apply(userMessage(2, "again", { requestId: "r2" }))).toBe("duplicate");
  });
});

describe("attach / reconnect", () => {
  test("snapshot is listed before the stream is subscribed", async () => {
    const source = new FakeEventSource();
    source.events = [
      userMessage(1, "hi", { requestId: "r1" }),
      assistantMessage(2, { text: "hello", inputSeqs: [1] }),
      runFinished(3, { stopReason: "done" }),
    ];
    const e = new ChatSurfaceEngine({ sessionId: "s", now: () => 1, windowTurns: 0 });
    const detach = await e.attach(source, { partials: true, limit: 2 });

    expect(source.calls[0]).toBe("list:0");
    expect(source.calls[1]).toBe("list:2");
    expect(source.calls[2]).toBe("stream:3:true");
    expect(e.view().turns[0]!.status).toBe("completed");
    expect(e.view().connection.status).toBe("open");

    detach();
    expect(source.isCancelled()).toBe(true);
    expect(e.view().connection.status).toBe("closed");
    e.disconnect();
    expect(e.view().connection.status).toBe("closed");
  });

  test("disconnect cancels the underlying stream", async () => {
    const source = new FakeEventSource();
    const e = new ChatSurfaceEngine({ sessionId: "s", now: () => 1, windowTurns: 0 });
    await e.attach(source);
    expect(source.isCancelled()).toBe(false);
    e.disconnect();
    expect(source.isCancelled()).toBe(true);
    expect(e.view().connection.status).toBe("closed");
  });

  test("reconnect lists the gap before trusting the resumed stream", async () => {
    const source = new FakeEventSource();
    source.events = [userMessage(1, "hi", { requestId: "r1" })];
    const e = new ChatSurfaceEngine({ sessionId: "s", now: () => 1, windowTurns: 0 });
    await e.attach(source);

    source.setState("reconnecting");
    expect(e.view().connection.status).toBe("reconnecting");

    source.events.push(
      assistantMessage(2, { text: "answer", inputSeqs: [1] }),
      runFinished(3, { stopReason: "done" }),
    );
    source.setState("open");
    await flushAsync();

    expect(e.view().turns[0]!.status).toBe("completed");
    expect(e.view().turns[0]!.assistantOutput).toBe("answer");
    expect(source.calls.filter((c) => c.startsWith("list:")).length).toBeGreaterThanOrEqual(2);
  });

  test("a stream gap is detected and healed by listing after the cursor", async () => {
    const source = new FakeEventSource();
    source.events = [userMessage(1, "hi", { requestId: "r1" })];
    const e = new ChatSurfaceEngine({ sessionId: "s", now: () => 1, windowTurns: 0 });
    await e.attach(source);

    // The stream loses 2..4 and delivers 5. The durable log still has them.
    source.events.push(
      assistantMessage(2, { text: "answer", inputSeqs: [1] }),
      runFinished(3, { stopReason: "done" }),
      userMessage(4, "again", { requestId: "r4" }),
      runFinished(5, { stopReason: "done" }),
    );
    source.emit(source.events[4]!);
    expect(e.needsResync()).toBe(true);

    source.setState("reconnecting");
    source.setState("open");
    await flushAsync();

    expect(e.needsResync()).toBe(false);
    expect(e.view().turns).toHaveLength(2);
    expect(e.view().turns[0]!.status).toBe("completed");
    expect(e.view().turns[1]!.status).toBe("completed");
  });

  test("partials never change the view and are cleared by a committed assistant message", () => {
    const e = new ChatSurfaceEngine({ sessionId: "s", now: () => 1, windowTurns: 0 });
    e.apply(userMessage(1, "hi", { requestId: "r1" }));
    const before = JSON.stringify(e.view());
    e.applyPartial({ text: "par", turnId: "t_1" });
    expect(JSON.stringify(e.view())).toBe(before);
    expect(e.partial()).toEqual({ text: "par", turnId: "t_1" });

    e.apply(assistantMessage(2, { text: "partial answer", inputSeqs: [1] }));
    expect(e.partial()).toBeNull();
  });
});

describe("subscriptions", () => {
  test("subscribers are notified and can unsubscribe", () => {
    const e = new ChatSurfaceEngine({ sessionId: "s", now: () => 1, windowTurns: 0 });
    let calls = 0;
    const unsubscribe = e.subscribe(() => {
      calls += 1;
    });
    e.submit("hi", "queue", "r1");
    expect(calls).toBe(1);
    e.apply(userMessage(1, "hi", { requestId: "r1" }));
    expect(calls).toBe(2);
    unsubscribe();
    e.apply(assistantMessage(2, { text: "x", inputSeqs: [1] }));
    expect(calls).toBe(2);
  });

  test("a throwing subscriber does not break the others", () => {
    const e = new ChatSurfaceEngine({ sessionId: "s", now: () => 1, windowTurns: 0 });
    let seen = 0;
    e.subscribe(() => {
      throw new Error("renderer bug");
    });
    e.subscribe(() => {
      seen += 1;
    });
    e.submit("hi", "queue", "r1");
    expect(seen).toBe(1);
  });
});
