// 10k-event performance gate (openspec: "单次投影 MUST 在 50ms 量级完成").
//
// The measured operation is the single projection the renderer waits for:
// hydrate(all events) + view(). The incremental path (apply 10k events one by
// one) is measured too because that is what a live stream does.
//
// Node/Bun on Apple Silicon: both are well under the 50ms budget; the reported
// numbers live in docs/CHAT-SURFACE.md. The assertion is deliberately looser
// than the measured value to keep CI machines from flaking.

import { describe, expect, test } from "bun:test";
import { ConversationProjection } from "../src/projection";
import type { WireEvent } from "../src/types";
import { assistantMessage, runFinished, toolResult, userMessage } from "./fixtures";

function buildEvents(turns: number): WireEvent[] {
  const events: WireEvent[] = [];
  let seq = 0;
  for (let i = 0; i < turns; i += 1) {
    const userSeq = (seq += 1);
    events.push(userMessage(userSeq, `question ${i}`, { requestId: `r${i}` }));
    events.push(
      assistantMessage((seq += 1), {
        text: `working on ${i}`,
        toolCalls: [{ toolCallId: `c${i}`, name: "echo", args: { i } }],
        inputSeqs: [userSeq],
      }),
    );
    events.push(
      toolResult((seq += 1), {
        toolCallId: `c${i}`,
        text: `ok ${i}`,
        durationMs: 3,
        timestamp: seq * 1_000,
      }),
    );
    events.push(assistantMessage((seq += 1), { text: `answer ${i}`, inputSeqs: [userSeq] }));
    events.push(
      runFinished((seq += 1), {
        stopReason: "done",
        turns: 2,
        tokensIn: 10,
        tokensOut: 5,
        durationMs: 20,
      }),
    );
  }
  return events;
}

function median(values: number[]): number {
  const sorted = [...values].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 0 ? (sorted[mid - 1]! + sorted[mid]!) / 2 : sorted[mid]!;
}

const EVENTS = buildEvents(2_000); // 10,000 events

function measureHydrate(): number {
  const projection = new ConversationProjection({ sessionId: "perf", windowTurns: 200 });
  const start = performance.now();
  projection.hydrate(EVENTS);
  const view = projection.view();
  const elapsed = performance.now() - start;
  if (view.turns.length !== 200) throw new Error(`window kept ${view.turns.length} turns`);
  return elapsed;
}

function measureIncremental(): number {
  const projection = new ConversationProjection({ sessionId: "perf", windowTurns: 200 });
  const start = performance.now();
  for (const event of EVENTS) projection.apply(event);
  const view = projection.view();
  const elapsed = performance.now() - start;
  if (view.turns.length !== 200) throw new Error(`window kept ${view.turns.length} turns`);
  return elapsed;
}

describe("10k event performance", () => {
  test("hydrate + view stays in the 50ms order of magnitude", () => {
    measureHydrate(); // warm-up
    const samples = Array.from({ length: 5 }, measureHydrate);
    const best = Math.min(...samples);
    const med = median(samples);
    console.log(`[perf] hydrate 10k events: best=${best.toFixed(2)}ms median=${med.toFixed(2)}ms`);
    // Gate on the best sample: a single projection is what the renderer waits
    // for; median absorbs scheduler/GC noise on shared CI hosts.
    expect(best).toBeLessThan(50);
  });

  test("incremental apply of 10k events stays bounded", () => {
    measureIncremental(); // warm-up
    const samples = Array.from({ length: 3 }, measureIncremental);
    const best = Math.min(...samples);
    const med = median(samples);
    console.log(`[perf] apply 10k events: best=${best.toFixed(2)}ms median=${med.toFixed(2)}ms`);
    expect(best).toBeLessThan(100);
  });
});
