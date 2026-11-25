// Identity invalidation stream — change keel-harness-v2 (identity realtime).
//
// The server exposes one tenant-local SSE tail, GET /v1/identity/events, whose
// frames are invalidation facts only: {kind, at}. This module is the single
// client of that route. It does not poll, and it never renders data: a fact
// makes the already-loaded stores re-read their ordinary, authorized endpoints,
// so every page shows the server's answer rather than a locally guessed patch.
//
// Lifecycle is keyed to auth:
//   - the books shell starts it once `auth=ok` and the account holds app.read;
//     the server refuses the route with 403 otherwise, and a 403 stops it;
//   - logout, a 401 or an account switch closes it (the store owns the session
//     and calls the closer installed here), so an old cookie is never replayed;
//   - a dropped connection reconnects with bounded exponential backoff.
//
// Frames for the same kind are coalesced in one short window: a burst of
// account writes produces one re-read, not one request per frame.
import { signal } from "nsh";
import type { FetchLike } from "./http";
import { can } from "./permissions";
import { store } from "./store";
import { identityAccess } from "./identity-access";
import { botAccess } from "./bots";
import { shares } from "./shares";

export type IdentityEventKind = "accounts" | "permissions" | "bots" | "teams" | "sessions" | "plugins";

export type IdentityStreamState = "idle" | "connecting" | "open" | "reconnecting" | "closed";

const KINDS: ReadonlySet<string> = new Set<IdentityEventKind>([
  "accounts",
  "permissions",
  "bots",
  "teams",
  "sessions",
  "plugins",
]);

const isKind = (value: unknown): value is IdentityEventKind =>
  typeof value === "string" && KINDS.has(value);

/** One invalidation fact as the server sends it. */
export interface IdentityInvalidation {
  kind: IdentityEventKind;
  at?: string;
}

/** The minimal streaming transport `KeelHttp` already exposes. A fixture or a
 * hand-written test transport has no `url`/`transport`, and the stream stays
 * off there rather than guessing an endpoint. */
interface StreamTransport {
  url(path: string): string;
  transport(): FetchLike;
}

/** The coalescing window: one re-read per kind per burst of events. */
const FLUSH_DELAY_MS = 40;

class IdentityStream {
  readonly state = signal<IdentityStreamState>("idle");
  /** The newest fact's server timestamp (tests and diagnostics). */
  readonly lastEventAt = signal<string | null>(null);

  private running = false;
  private generation = 0;
  private attempt = 0;
  private opened = false;
  private controller: AbortController | null = null;
  private retryTimer: ReturnType<typeof setTimeout> | undefined;
  private flushTimer: ReturnType<typeof setTimeout> | undefined;
  private pending = new Set<IdentityEventKind>();

  /** Start or stop to match the auth stage. The shell calls this from its
   * effect, so a login starts the stream and a logout/401 finishes it. */
  sync(auth: string): void {
    if (auth !== "ok" || !can("app.read")) {
      this.stop();
      return;
    }
    this.start();
  }

  start(): void {
    if (this.running) return;
    const transport = store.identityStreamTransport();
    if (!transport) return;
    this.running = true;
    this.attempt = 0;
    this.generation += 1;
    this.state.set("connecting");
    store.setIdentityStreamCloser(() => this.stop());
    void this.connect(this.generation);
  }

  stop(): void {
    if (!this.running && this.state() === "idle") return;
    this.running = false;
    this.generation += 1;
    this.controller?.abort();
    this.controller = null;
    if (this.retryTimer !== undefined) clearTimeout(this.retryTimer);
    if (this.flushTimer !== undefined) clearTimeout(this.flushTimer);
    this.retryTimer = undefined;
    this.flushTimer = undefined;
    this.pending.clear();
    this.attempt = 0;
    this.opened = false;
    store.setIdentityStreamCloser(null);
    this.state.set("closed");
  }

  /** Test hook: stop and forget every timer/backlog (the singleton survives). */
  reset(): void {
    this.stop();
    this.state.set("idle");
    this.lastEventAt.set(null);
  }

  private live(generation: number): boolean {
    return this.running && generation === this.generation;
  }

  private async connect(generation: number): Promise<void> {
    if (!this.live(generation)) return;
    const transport = store.identityStreamTransport();
    if (!transport) {
      this.stop();
      return;
    }
    const controller = new AbortController();
    this.controller = controller;
    try {
      const response = await transport.transport()(transport.url("/v1/identity/events"), {
        method: "GET",
        headers: { Accept: "text/event-stream" },
        credentials: "same-origin",
        signal: controller.signal,
      });
      if (!this.live(generation)) {
        controller.abort();
        return;
      }
      if (!response.ok) {
        // A dead session is not retryable: close first, then let the store show
        // the login gate. A missing key/route is a deployment fact, not a blip.
        if (response.status === 401) {
          this.stop();
          store.identityStreamUnauthorized();
          return;
        }
        if (response.status === 403 || response.status === 404 || response.status === 405 || response.status === 501) {
          this.stop();
          return;
        }
        this.schedule(generation);
        return;
      }
      if (!response.body) {
        this.schedule(generation);
        return;
      }
      // A 200 that is not an SSE body is a proxy/static-host answer, not a
      // stream: retrying would spin forever. Production always names the type;
      // an opaque test transport may omit it.
      const contentType = response.headers?.get("content-type") ?? "";
      if (contentType && !contentType.includes("text/event-stream")) {
        this.stop();
        return;
      }
      this.attempt = 0;
      this.state.set("open");
      if (this.opened) {
        // Facts are not replayed. After a dropped connection, re-read every
        // loaded projection once so a change made during the gap cannot leave a
        // page stale until the next unrelated mutation.
        this.enqueueAll();
      } else {
        this.opened = true;
      }
      await this.pump(response.body, generation);
      // A normal end (proxy close, server drain): reconnect. The next open
      // re-reads the loaded projections, so a fact lost in the gap is covered.
      if (this.live(generation)) this.schedule(generation);
    } catch {
      // A network failure or an aborted connection. abort() is our own stop;
      // live() then says so and the loop ends.
      if (this.live(generation)) this.schedule(generation);
    }
  }

  private schedule(generation: number): void {
    if (!this.live(generation)) return;
    const delay = Math.min(8000, 500 * Math.pow(1.7, this.attempt));
    this.attempt += 1;
    this.state.set("reconnecting");
    this.retryTimer = setTimeout(() => {
      this.retryTimer = undefined;
      void this.connect(generation);
    }, delay);
  }

  private async pump(body: ReadableStream<Uint8Array>, generation: number): Promise<void> {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (!this.live(generation)) return;
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        buffer = buffer.replace(/\r\n/g, "\n");
        let boundary = buffer.indexOf("\n\n");
        while (boundary >= 0) {
          this.frame(buffer.slice(0, boundary));
          buffer = buffer.slice(boundary + 2);
          boundary = buffer.indexOf("\n\n");
        }
      }
      if (buffer.trim()) this.frame(buffer);
    } finally {
      try {
        reader.releaseLock();
      } catch {
        /* already released */
      }
    }
  }

  /** Parse one SSE frame. Heartbeats are comments and cost nothing. */
  private frame(text: string): void {
    let name = "message";
    const data: string[] = [];
    for (const line of text.split("\n")) {
      if (!line || line.startsWith(":")) continue;
      const colon = line.indexOf(":");
      const field = colon >= 0 ? line.slice(0, colon) : line;
      const value = colon >= 0 ? line.slice(colon + 1).replace(/^ /, "") : "";
      if (field === "event") name = value;
      else if (field === "data") data.push(value);
    }
    if (!data.length || (name !== "identity" && name !== "message")) return;
    let payload: unknown;
    try {
      payload = JSON.parse(data.join("\n"));
    } catch {
      return;
    }
    if (!payload || typeof payload !== "object") return;
    const record = payload as { kind?: unknown; at?: unknown };
    if (!isKind(record.kind)) return;
    if (typeof record.at === "string") this.lastEventAt.set(record.at);
    this.enqueue(record.kind);
  }

  private enqueue(kind: IdentityEventKind): void {
    this.pending.add(kind);
    if (this.flushTimer !== undefined) return;
    this.flushTimer = setTimeout(() => {
      this.flushTimer = undefined;
      void this.flush();
    }, FLUSH_DELAY_MS);
  }

  private enqueueAll(): void {
    for (const kind of KINDS) {
      this.pending.add(kind as IdentityEventKind);
    }
    if (this.flushTimer !== undefined) return;
    this.flushTimer = setTimeout(() => {
      this.flushTimer = undefined;
      void this.flush();
    }, FLUSH_DELAY_MS);
  }

  private async flush(): Promise<void> {
    const kinds = new Set(this.pending);
    this.pending.clear();
    if (!kinds.size) return;
    // Permissions first: a revocation changes what the other reads may even ask
    // for, so the UI's `can()` and the stream's own authority settle before the
    // projections are re-read.
    if (kinds.has("permissions")) {
      await store.loadIdentity();
      if (store.auth() !== "ok") {
        this.stop();
        return;
      }
    }
    const safe = async (run: () => Promise<void>): Promise<void> => {
      try {
        await run();
      } catch {
        // A failed re-read is left to the next fact or the page's explicit
        // retry; the stream itself stays connected.
      }
    };
    await Promise.all([
      safe(() => identityAccess.refreshForEvent(kinds)),
      safe(() => botAccess.refreshForEvent(kinds)),
      safe(() => shares.refreshForEvent(kinds)),
    ]);
    if (kinds.has("sessions")) {
      await safe(() => store.refreshSessions());
    }
    if (kinds.has("plugins")) {
      await safe(() => store.loadPlugins());
    }
    // Losing app.read ends this stream's contract; reconnect would only 403.
    if (!can("app.read")) this.stop();
  }
}

export const identityStream = new IdentityStream();

// The store owns the session; this module owns the connection. Registering the
// sync hook here means a login on /settings or /plugins starts the stream too,
// and a whoami after a permission change stops it when app.read disappears.
store.setIdentityStreamStarter(() => identityStream.sync(store.auth()));

/** Test alias: the next case starts from idle with no backlog. */
export const resetIdentityStreamForTest = (): void => identityStream.reset();
