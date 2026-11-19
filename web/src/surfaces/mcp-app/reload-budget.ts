// Per-View navigation reload budget (change feat-mcp-apps-events, P2).
//
// A malicious (or broken) app can navigate itself after every handshake, which
// makes the host discard the instance and reload its own document forever.
// Each View instance allows a bounded number of automatic reloads; the next
// one is refused, the instance is destroyed, the text fallback stays, and the
// host records a `protocol.violation` audit entry. A manual retry starts a new
// budget (the user asked for it); the budget itself is pure so it is testable
// without a browser.
import type { McpAppAuditEntry } from "../../data/contract";

export const MAX_NAVIGATION_RELOADS = 10;

export type ReloadDecision = "reload" | "exhausted";

export interface ReloadBudget {
  /** Automatic reloads already allowed. */
  readonly used: number;
  /** The configured ceiling. */
  readonly limit: number;
  /** Ask for one more automatic reload. */
  consume(): ReloadDecision;
}

export function createReloadBudget(limit = MAX_NAVIGATION_RELOADS): ReloadBudget {
  const ceiling = Number.isFinite(limit) && limit > 0 ? Math.floor(limit) : MAX_NAVIGATION_RELOADS;
  let used = 0;
  return {
    get used() {
      return used;
    },
    get limit() {
      return ceiling;
    },
    consume(): ReloadDecision {
      if (used >= ceiling) return "exhausted";
      used += 1;
      return "reload";
    },
  };
}

/** The audit entry a budget exhaustion writes. The detail is static (never the
 * URL the app navigated to): no query, no body, no credential. */
export function reloadBudgetAuditEntry(
  appId: string,
  server?: string,
  sessionId?: string,
): McpAppAuditEntry {
  return {
    appId,
    ...(server ? { server } : {}),
    ...(sessionId ? { sessionId } : {}),
    action: "protocol.violation",
    result: "refused",
    detail: `reload budget exhausted (${MAX_NAVIGATION_RELOADS})`,
  };
}
