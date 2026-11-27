// Board SSE transport and cursor gates — the review's P1-1.
//
// A hidden event is an `id: N\n\n` frame with no data: it must advance the
// cursor and still reach the caller (so no reconnect replays it), without
// touching the queue. Duplicate and out-of-order frames must not overwrite a
// newer view. These cases would go red if the cursor gate or the seen set in
// `board.ts` (or the id-only branch in `http.ts`) were deleted.
import { expect, test } from "nsh/test";
import { boardStore } from "./board";
import type { BoardEventView } from "./board-wire";
import { KeelHttp, type FetchLike } from "./http";
import { FixtureBoard } from "./fixture-board";
import { setEffectivePermissions } from "./permissions";
import { store } from "./store";

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

const waitFor = async (predicate: () => boolean, what: string, timeout = 2500): Promise<void> => {
  const start = Date.now();
  for (;;) {
    if (predicate()) return;
    if (Date.now() - start > timeout) throw new Error(`timed out waiting for ${what}`);
    await sleep(10);
  }
};

/** A body whose chunks the case pushes; `close` ends the response normally. */
const sseBody = (chunks: string[]) => {
  let index = 0;
  let closed = false;
  let notify: (() => void) | null = null;
  return {
    body: {
      getReader: () => ({
        read: async () => {
          for (;;) {
            if (index < chunks.length) {
              const text = chunks[index++];
              return { done: false, value: new Uint8Array([...text].map((char) => char.charCodeAt(0))) };
            }
            if (closed) return { done: true, value: undefined };
            await new Promise<void>((resolve) => {
              notify = resolve;
            });
          }
        },
        releaseLock: () => {},
        cancel: async () => {},
      }),
    },
    push: (text: string) => {
      chunks.push(text);
      notify?.();
      notify = null;
    },
    close: () => {
      closed = true;
      notify?.();
      notify = null;
    },
  };
};

const event = (cursor: string, text: string): BoardEventView => ({
  cursor,
  type: "message.created",
  createdAt: "2026-11-27T10:00:00.000Z",
  payload: {},
  message: {
    id: "812",
    channel: "work",
    text,
    createdAt: "2026-11-27T10:00:00.000Z",
    labels: {},
    task: { state: "ready", owner: "a", by: "a", needs: [], epoch: 1, moves: [] },
  },
});

test("board SSE: an id-only frame advances the cursor and reconnects with Last-Event-ID", async () => {
  const headers: Array<string | undefined> = [];
  let calls = 0;
  const fetchImpl = (async (_input: unknown, init: RequestInit) => {
    calls += 1;
    const map = (init.headers ?? {}) as Record<string, string>;
    headers.push(map["Last-Event-ID"]);
    if (calls === 1) {
      // The hidden event the caller may not see; then a normal end (so the
      // client reconnects with the advanced cursor).
      const first = sseBody(["id: 41\n\n"]);
      first.close();
      return {
        ok: true,
        status: 200,
        headers: { get: () => null },
        body: first.body,
      };
    }
    // The reconnect: a body that stays open until the caller stops.
    return {
      ok: true,
      status: 200,
      headers: { get: () => null },
      body: sseBody([]).body,
    };
  }) as unknown as FetchLike;

  const http = new KeelHttp("", fetchImpl);
  const frames: Array<{ id: string; event: BoardEventView | null }> = [];
  const stop = http.boardStream({
    after: "",
    onFrame: (frame) => frames.push(frame),
    onState: () => {},
  });
  try {
    await waitFor(() => frames.length === 1, "hidden frame");
    expect(frames[0].id).toBe("41");
    expect(frames[0].event).toBeNull();
    await waitFor(() => calls >= 2, "reconnect", 3000);
    expect(headers[1]).toBe("41");
  } finally {
    stop();
  }
});

test("board store: an SSE task without moves invalidates and refetches the augmented view", async () => {
  class Augmented extends FixtureBoard {
    release: (() => void) | null = null;
    override boardThread(id: string, after?: string, limit?: number): Promise<ReturnType<FixtureBoard["boardThread"]> extends Promise<infer T> ? T : never> {
      return new Promise((resolve) => {
        this.release = () => {
          void super.boardThread(id, after, limit).then((view) => {
            view.message.task = {
              state: "claimed",
              owner: "acct-chenmo",
              by: "acct-chenmo",
              needs: [],
              epoch: 2,
              claim: { actor: "acct-chenmo", until: new Date(Date.now() + 900_000).toISOString() },
              moves: ["renew", "release", "complete", "fail", "block", "reply"],
            };
            resolve(view);
          });
        };
      });
    }
  }
  const transport = new Augmented();
  setEffectivePermissions({ "board.read": "tenant", "board.write": "tenant", "board.manage": "tenant" }, false);
  store.setBoardTransport(transport);
  boardStore.reset();
  try {
    await boardStore.open();
    const before = boardStore.cache().tasks["809"]!;
    expect(before.moves).toContain("claim");
    expect(before.moves).not.toContain("complete");
    boardStore.applyFrameForTest({
      id: "900",
      event: {
        cursor: "900",
        type: "task.claimed",
        createdAt: "2026-11-27T10:00:00.000Z",
        payload: {},
        message: {
          id: "809",
          channel: "work",
          text: "补齐退款审计日志",
          createdAt: "2026-11-27T09:00:00.000Z",
          labels: {},
          // The raw SSE projection has no viewer-dependent moves.
          task: { state: "claimed", owner: "acct-chenmo", by: "acct-chenmo", needs: [], epoch: 2, claim: { actor: "acct-chenmo", until: new Date(Date.now() + 900_000).toISOString() } },
        },
      },
    });
    const interim = boardStore.cache().tasks["809"]!;
    // Interim is pruned to the new state: no ready-only claim; no old complete.
    expect(interim.moves).not.toContain("claim");
    expect(interim.moves).not.toContain("complete");
    expect(boardStore.augmenting()["809"]).toBe(true);
    // The augmented GET replaces it with the claimed action set.
    transport.release?.();
    await waitFor(() => boardStore.augmenting()["809"] !== true, "augment refetch");
    const after = boardStore.cache().tasks["809"]!;
    expect(after.state).toBe("claimed");
    expect(after.moves).toContain("complete");
    expect(after.moves).toContain("renew");
    expect(after.moves).not.toContain("claim");
  } finally {
    boardStore.reset();
    store.setBoardTransport(null);
  }
});

test("board store: hidden advances but does not show activity; duplicate/out-of-order never overwrite", () => {
  boardStore.reset();
  try {
    boardStore.liveCursor.set("10");
    // The cursor is already past these: neither a duplicate nor a lower id may
    // write the cache (the lower id is a cursor-gate mutation).
    boardStore.applyFrameForTest({ id: "10", event: event("10", "duplicate-old") });
    boardStore.applyFrameForTest({ id: "9", event: event("9", "out-of-order") });
    expect(boardStore.cache().messages["812"]).toBeUndefined();
    expect(boardStore.lastEventAt()).toBeUndefined();

    const hidden = boardStore.liveCursor();
    boardStore.applyFrameForTest({ id: "11", event: null });
    expect(boardStore.liveCursor()).toBe("11");
    expect(boardStore.lastEventAt()).toBeUndefined();
    expect(hidden).toBe("10");

    boardStore.applyFrameForTest({ id: "12", event: event("12", "newest") });
    expect(boardStore.liveCursor()).toBe("12");
    expect(boardStore.cache().messages["812"]?.text).toBe("newest");
    expect(boardStore.lastEventAt()).toBeDefined();

    // A replay of 12 must not overwrite the view (the seen-set mutation).
    // Clear the cursor first so only the seen set can reject it: a reconnect
    // can start a fresh stream while the same frame is replayed.
    boardStore.liveCursor.set("");
    boardStore.applyFrameForTest({ id: "12", event: event("12", "replayed") });
    expect(boardStore.cache().messages["812"]?.text).toBe("newest");
  } finally {
    boardStore.reset();
  }
});
