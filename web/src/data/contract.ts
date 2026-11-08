// Keel UI ↔ headless contract.
//
// The single source of truth is `packages/chat-surface/src/types.ts` (frozen;
// another agent owns its implementation). This module only re-exports the
// types so every screen imports them from one place, and adds the few
// UI-side shapes the contract does not cover (session list rows, attachment
// chips, the extended command set). None of this changes the frozen contract.
export type {
  ConversationView,
  ConnectionState,
  DecisionView,
  Delivery,
  EventSource,
  InputView,
  QueueItemView,
  RunSummary,
  SurfaceCommands,
  SurfaceEngine,
  ToolCallView,
  TurnOrigin,
  TurnStatus,
  TurnView,
  WireEvent,
  WorkSegment,
} from "../../../packages/chat-surface/src/types";

import type { Delivery, SurfaceCommands, WireEvent } from "../../../packages/chat-surface/src/types";

/** A row of `GET /v1/sessions`. */
export interface SessionSummary {
  sessionId: string;
  createdAt?: string;
  updatedAt?: string;
  lastSeq?: number;
  runStatus?: string;
  lastMessageAt?: string;
  /** Optional enrichments (the API may add them; the client falls back to
   * loading the last events of a session). */
  title?: string;
  lastMessage?: string;
}

/** An attachment shown in the conversation (uploaded by the composer). */
export interface AttachmentRef {
  fileId?: string;
  name: string;
  mime?: string;
  size?: number;
  /** A blob: or https: URL, when the platform can show it. */
  url?: string;
}

/** What the composer sends: the contract's `send` plus attachments. */
export interface UiCommands extends SurfaceCommands {
  sendWithAttachments?(
    text: string,
    opts: { requestId: string; delivery: Delivery; attachments?: AttachmentRef[] },
  ): Promise<void>;
}

/** The engine the UI runs against. `setConnection` is a client-side seam: the
 * headless core owns the view's connection state, but the transport learns
 * about it first. The local projection uses it; a future `@keel/chat-surface`
 * implementation can ignore it if it wires the transport itself. */
export interface UiEngine {
  view(): import("../../../packages/chat-surface/src/types").ConversationView;
  submit(text: string, delivery: Delivery, requestId: string, attachments?: AttachmentRef[]): void;
  apply(event: WireEvent, attachments?: AttachmentRef[]): void;
  applyPartial(partial: { text: string; turnId?: string }): void;
  hydrate(events: WireEvent[], attachmentIndex?: Map<string, AttachmentRef[]>): void;
  requestStop(): void;
  subscribe(fn: () => void): () => void;
  setConnection(status: import("../../../packages/chat-surface/src/types").ConnectionState["status"], lastSeq: number): void;
  setSessionId(sessionId: string): void;
  attachmentsFor(requestId: string): AttachmentRef[] | undefined;
  /** A decision's deadline (ms since epoch), when the event carried one. */
  decisionTimeout(decisionId: string): number | undefined;
  removeOptimistic(requestId: string): void;
  /** Undo a stop request the server refused (keeps the composer usable). */
  resetStopping(): void;
  /** Partial text for the turn currently streaming (UI-only). */
  partialText(): string | undefined;
  /** The turn the partial belongs to. */
  partialTurnId(): string | undefined;
}
