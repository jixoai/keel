// The external run projection's read cache — change keel-oar-renderer.
//
// The conversation's external run card is a projection, not a fold: the server
// folds the OAR record stream once (`keel.oar.render-projection/1`) and the
// client reads it once per record burst. A turn with ten `oar.record` events is
// one request, and every row the card draws comes from that answer — the client
// never folds a second record model (docs/OAR.md §6). A later turn's run is a
// newer record, so it refolds once; nothing re-reads the same burst.
//
// A failure is kept, not retried on every render: the card's host falls back to
// the existing text row, and the conversation is never parked on a read it did
// not make. `reset()` is the test/harness seam (a session switch reuses the
// cache on purpose: the projection is the session's, not the turn's).
import { signal } from "nsh";
import { KeelApiError, errorMessageOf } from "./http";
import type { ExternalProjectionView } from "./oar-projection";
import { store } from "./store";

/** Why the card cannot draw: a typed refusal from the route (`oar_not_wired`,
 * `not_external_session`, `projection_corrupt`), a network failure, or a
 * transport that never served the surface. */
export interface OarProjectionFailure {
  status?: number;
  code?: string;
  message: string;
}

export type OarProjectionState =
  | { status: "idle" }
  | { status: "loading" }
  | { status: "ready"; projection: ExternalProjectionView }
  | { status: "error"; error: OarProjectionFailure };

const failureOf = (error: unknown): OarProjectionFailure => {
  if (error instanceof KeelApiError) {
    return { status: error.status, code: error.code, message: error.message };
  }
  return { message: errorMessageOf(error) };
};

class OarProjectionStore {
  /** One state per session id; replaced wholesale on each change (the signal
   * the host reads in a getter). */
  private readonly states = signal<Record<string, OarProjectionState>>({});
  private readonly inFlight = new Set<string>();
  /** The newest record seq each cached read was asked for, so a re-render (or
   * another record of the same burst) never issues a second request while a
   * later turn's record does refold the run. */
  private readonly readThrough = new Map<string, number>();

  /** The state the card's host draws. An unknown session is `idle` (no read
   * was asked for), never an empty projection. */
  state(sessionId: string): OarProjectionState {
    return this.states()[sessionId] ?? { status: "idle" };
  }

  /** Read this session's projection once per record it has not seen. A second
   * call — a re-render, another record of the same burst — is a no-op; a record
   * newer than the cached answer (a later turn's run) refolds it once. A
   * failure is not retried for the same record: the card's host has already
   * fallen back to its text row. */
  ensure(sessionId: string, throughSeq = 0): void {
    if (!sessionId) return;
    const covered = this.readThrough.get(sessionId) ?? -1;
    if (throughSeq <= covered) return;
    if (this.inFlight.has(sessionId)) return;
    this.readThrough.set(sessionId, throughSeq);
    void this.load(sessionId);
  }

  private async load(sessionId: string): Promise<void> {
    this.inFlight.add(sessionId);
    this.write(sessionId, { status: "loading" });
    try {
      const projection = await store.externalProjection(sessionId);
      if (!projection) {
        this.write(sessionId, {
          status: "error",
          error: {
            code: "oar_not_wired",
            message: "这个部署没有接通外部运行投影（OAR）。",
          },
        });
        return;
      }
      this.write(sessionId, { status: "ready", projection });
    } catch (error) {
      this.write(sessionId, { status: "error", error: failureOf(error) });
    } finally {
      this.inFlight.delete(sessionId);
    }
  }

  /** Drop every cached read (tests, harnesses, a tenant switch). */
  reset(): void {
    this.inFlight.clear();
    this.readThrough.clear();
    this.states.set({});
  }

  /** Drop one session's read, so the next `ensure` re-reads it. */
  forget(sessionId: string): void {
    const next = { ...this.states() };
    delete next[sessionId];
    this.states.set(next);
    this.readThrough.delete(sessionId);
  }

  private write(sessionId: string, state: OarProjectionState): void {
    this.states.set({ ...this.states(), [sessionId]: state });
  }
}

export const oarAccess = new OarProjectionStore();
