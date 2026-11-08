// SurfaceEngine: the host-facing facade over ConversationProjection.
//
// What it adds on top of the pure projection:
//   - optimistic `submit` (idempotent by requestId);
//   - `subscribe` notifications (synchronous, re-entrancy safe);
//   - partial-frame storage (never part of ConversationView);
//   - EventSource attach/detach with the reconnect state machine:
//       list(after cursor) -> apply pages -> stream(after cursor)
//     and a gap fill on every transition back to "open".
//
// No fetch, no timers, no DOM.

import type {
  ConnectionState,
  ConversationView,
  Delivery,
  EventSource,
  InputView,
  QueueItemView,
  SurfaceEngine,
  WireEvent,
} from "./types";
import {
  ConversationProjection,
  type ApplyResult,
  type ProjectionOptions,
} from "./projection";

export type EngineOptions = ProjectionOptions;

export interface SubmitOptions {
  /** Explicit "send now" (stop barrier + reservation). Ignored while idle. */
  immediate?: boolean;
}

export interface AttachOptions {
  /** Ask for token-level partial frames (omitted/false = committed events only). */
  partials?: boolean;
  /** Snapshot page size for `list()`. Default 500. */
  limit?: number;
}

export interface PartialFrame {
  text: string;
  turnId?: string;
}

const DEFAULT_PAGE_LIMIT = 500;
const MAX_SNAPSHOT_PAGES = 100_000;

export class ChatSurfaceEngine implements SurfaceEngine {
  private readonly projection: ConversationProjection;
  private readonly listeners = new Set<() => void>();

  private latestPartial: PartialFrame | null = null;

  private attachToken = 0;
  private attachedSource: EventSource | null = null;
  private attachedOptions: AttachOptions = {};
  private cancelStream: (() => void) | null = null;
  private sawDisconnect = false;
  private resyncChain: Promise<void> = Promise.resolve();

  constructor(options: EngineOptions | string) {
    this.projection = new ConversationProjection(
      typeof options === "string" ? { sessionId: options } : options,
    );
  }

  // ---------------------------------------------------------------------
  // SurfaceEngine
  // ---------------------------------------------------------------------

  view(): ConversationView {
    return this.projection.view();
  }

  submit(text: string, delivery: Delivery, requestId: string, opts: SubmitOptions = {}): InputView {
    const input = this.projection.submit(text, delivery, requestId, opts);
    this.notify();
    return input;
  }

  /** Convenience for the explicit "send now" path. */
  submitNow(text: string, requestId: string): InputView {
    return this.submit(text, "queue", requestId, { immediate: true });
  }

  apply(event: WireEvent): ApplyResult {
    const result = this.projection.apply(event);
    if (result === "applied" && event.kind === "assistant.message") this.latestPartial = null;
    if (result === "applied" || result === "buffered") this.notify();
    if (result === "buffered") this.scheduleResync();
    return result;
  }

  applyMany(events: readonly WireEvent[]): number {
    const applied = this.projection.applyMany(events);
    if (applied > 0) this.notify();
    if (this.projection.needsResync()) this.scheduleResync();
    return applied;
  }

  applyPartial(partial: PartialFrame): void {
    this.latestPartial = { text: partial.text, turnId: partial.turnId };
    // Partials never enter ConversationView; subscribers read engine.partial().
    this.notify();
  }

  hydrate(events: readonly WireEvent[]): void {
    this.projection.hydrate(events);
    this.latestPartial = null;
    this.notify();
  }

  requestStop(): void {
    if (this.projection.requestStop()) this.notify();
  }

  subscribe(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  // ---------------------------------------------------------------------
  // Extensions beyond the frozen SurfaceEngine surface
  // ---------------------------------------------------------------------

  /** Latest non-durable token partial (or null). */
  partial(): PartialFrame | null {
    return this.latestPartial;
  }

  canAutoDrain(): boolean {
    return this.projection.canAutoDrain();
  }

  /** A "send now" reservation ready to promote; host sends it with the same requestId. */
  promotionReady(): QueueItemView | null {
    return this.projection.promotionReady();
  }

  /** Explicit user resume: lifts the post-stop auto-drain pause. */
  resumeAutoDrain(): void {
    this.projection.resumeAutoDrain();
    this.notify();
  }

  answerDecision(decisionId: string, answer: string): boolean {
    const changed = this.projection.answerDecision(decisionId, answer);
    if (changed) this.notify();
    return changed;
  }

  setConnection(status: ConnectionState["status"]): void {
    this.projection.setConnection(status);
    this.notify();
  }

  needsResync(): boolean {
    return this.projection.needsResync();
  }

  lastSeq(): number {
    return this.projection.lastSeq;
  }

  /** Snapshot-then-subscribe. Returns a detach function. */
  async attach(source: EventSource, options: AttachOptions = {}): Promise<() => void> {
    this.detachInternal(false);
    const token = (this.attachToken += 1);
    this.attachedSource = source;
    this.attachedOptions = options;
    this.sawDisconnect = false;
    this.projection.setConnection("connecting");
    this.notify();

    try {
      await this.fillFromSource(source, options, token);
    } catch (error) {
      if (token === this.attachToken) {
        this.projection.setConnection("reconnecting");
        this.notify();
      }
      throw error;
    }
    if (token !== this.attachToken) return () => {};

    this.projection.setConnection("open");
    this.notify();

    const cancel = source.stream({
      after: this.projection.lastSeq,
      partials: options.partials === true,
      onEvent: (event) => {
        this.apply(event);
      },
      onPartial: (partial) => {
        this.applyPartial(partial);
      },
      onState: (status) => {
        this.onSourceState(status, token);
      },
    });
    let stopped = false;
    const stop = () => {
      if (stopped) return;
      stopped = true;
      try {
        cancel();
      } catch {
        // A host source that already closed must not break detach.
      }
    };
    this.cancelStream = stop;
    return () => {
      if (this.cancelStream === stop) {
        this.detachInternal(true);
      } else {
        stop();
      }
    };
  }

  /** Stops the stream and marks the connection closed. */
  disconnect(): void {
    this.detachInternal(true);
  }

  // ---------------------------------------------------------------------
  // Internals
  // ---------------------------------------------------------------------

  private onSourceState(status: ConnectionState["status"], token: number): void {
    if (token !== this.attachToken) return;
    this.projection.setConnection(status);
    this.notify();
    if (status === "reconnecting" || status === "closed") {
      this.sawDisconnect = true;
      return;
    }
    if (status === "open" && (this.sawDisconnect || this.projection.needsResync())) {
      this.sawDisconnect = false;
      // Re-list before trusting the resumed stream: this is the spec's
      // "snapshot first, then subscribe" rule applied to every reconnect.
      this.scheduleResync();
    }
  }

  private scheduleResync(): void {
    const source = this.attachedSource;
    if (!source) return;
    const token = this.attachToken;
    this.resyncChain = this.resyncChain
      .then(async () => {
        if (token !== this.attachToken) return;
        await this.fillFromSource(source, this.attachedOptions, token);
      })
      .catch(() => {
        // The next "open" transition (or an explicit attach) retries.
      });
  }

  private async fillFromSource(source: EventSource, options: AttachOptions, token: number): Promise<void> {
    const limit = options.limit && options.limit > 0 ? options.limit : DEFAULT_PAGE_LIMIT;
    let appliedTotal = 0;
    for (let page = 0; page < MAX_SNAPSHOT_PAGES; page += 1) {
      if (token !== this.attachToken) return;
      const events = await source.list(this.projection.lastSeq, limit);
      if (token !== this.attachToken) return;
      if (!Array.isArray(events) || events.length === 0) break;
      const before = this.projection.lastSeq;
      const applied = this.projection.applyMany(events);
      appliedTotal += applied;
      if (applied === 0 && this.projection.lastSeq === before) break;
      if (events.length < limit) break;
    }
    if (appliedTotal > 0) this.notify();
    // If the server itself returns a non-contiguous page there is nothing more
    // to fetch right now; a later "open" transition or explicit attach retries.
    // (Retrying in a tight loop would only re-fetch the same page.)
  }

  private detachInternal(markClosed: boolean): void {
    this.attachToken += 1;
    if (this.cancelStream) {
      const cancel = this.cancelStream;
      this.cancelStream = null;
      try {
        cancel();
      } catch {
        // ignore
      }
    }
    this.attachedSource = null;
    this.sawDisconnect = false;
    if (markClosed) {
      this.projection.setConnection("closed");
      this.notify();
    }
  }

  private notify(): void {
    if (this.listeners.size === 0) return;
    for (const listener of [...this.listeners]) {
      try {
        listener();
      } catch {
        // A renderer throwing must not break the event loop for the others.
      }
    }
  }
}

export function createSurfaceEngine(options: EngineOptions | string): ChatSurfaceEngine {
  return new ChatSurfaceEngine(options);
}
