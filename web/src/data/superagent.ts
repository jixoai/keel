// The super-agent ticket board — change keel-collab-superagent, 4.2/4.3.
//
// The board is a read projection (`GET /v1/superagent/tickets`) plus the open
// Attention inbox (`GET /v1/superagent/attention`); the operator's writes are
// answer / resume / stop. This module owns the page's data state and the pure
// vocabulary around it (status words, the four headline counters, deadlines).
// Every read and write goes through the store's one transport seam
// (`store.superagentApi()`), never a second `fetch`.
//
// Two rules the page depends on:
//   - a failed read is never replaced by sample rows; the failure carries the
//     server's request id and required key so the page can say what is wrong;
//   - an action is offered only under `task.write`. The server additionally
//     runs its independent operator authorization; a 403 from that seam is
//     shown verbatim instead of being explained away.
import { signal } from "nsh";
import type {
  SuperagentAnswerInput,
  SuperagentAttentionView,
  SuperagentBoardView,
  SuperagentTicketView,
} from "./contract";
import { KeelApiError, errorMessageOf } from "./http";
import { can } from "./permissions";
import { store } from "./store";

// ---- vocabulary -----------------------------------------------------------

/** The ticket lifecycle words (`collab.WorkItemStatus`). An unknown token
 * keeps its raw text instead of being hidden behind a safe-sounding label. */
export const SUPERAGENT_STATUS_LABEL: Record<string, string> = {
  open: "待派工",
  active: "执行中",
  blocked: "待人工",
  done: "完成",
  failed: "失败",
  cancelling: "停止中",
  cancelled: "已停止",
  unknown: "未知",
};

export const superagentStatusLabel = (status: string | undefined): string =>
  (status ? SUPERAGENT_STATUS_LABEL[status] : undefined) ?? status ?? "未知";

export type SuperagentTone = "info" | "success" | "destructive" | "warning" | "secondary" | "outline";

/** The status badge's tone. A stopped ticket is neutral: it is a recorded
 * human decision, not a failure of the worker. */
export const superagentStatusTone = (status: string | undefined): SuperagentTone => {
  switch (status) {
    case "blocked":
      return "warning";
    case "active":
      return "info";
    case "done":
      return "success";
    case "failed":
      return "destructive";
    case "cancelling":
      return "warning";
    case "cancelled":
      return "secondary";
    case "open":
      return "outline";
    default:
      return "secondary";
  }
};

/** The Mandate's recorded strength (`superagent.MandateStrength`). */
export const MANDATE_STRENGTH_LABEL: Record<string, string> = {
  observe: "只读观察",
  comment: "可评论",
  write: "可写入",
};

export const mandateStrengthLabel = (strength: string | undefined): string =>
  (strength ? MANDATE_STRENGTH_LABEL[strength] : undefined) ?? strength ?? "未记录";

/** `collab.AttentionKind`. */
export const ATTENTION_KIND_LABEL: Record<string, string> = {
  decision: "决策",
  clarify: "澄清",
  approval: "审批",
};

export const attentionKindLabel = (kind: string | undefined): string =>
  (kind ? ATTENTION_KIND_LABEL[kind] : undefined) ?? kind ?? "问题";

/** `collab.AttentionRoute`. */
export const ATTENTION_ROUTE_LABEL: Record<string, string> = {
  session: "会话",
  human: "人工",
  channel: "来源频道",
};

export const attentionRouteLabel = (route: string | undefined): string =>
  (route ? ATTENTION_ROUTE_LABEL[route] : undefined) ?? route ?? "未记录";

/** `superagent.ReportState` — the worker report's own state word. */
export const REPORT_STATE_LABEL: Record<string, string> = {
  progress: "进行中",
  waiting: "等待人工",
  ready: "完成",
  blocked: "受阻",
  failed: "失败",
  dropped: "已放弃",
};

export const reportStateLabel = (state: string | undefined): string =>
  (state ? REPORT_STATE_LABEL[state] : undefined) ?? state ?? "报告";

/** `collab.AssignmentStatus`. */
export const ASSIGNMENT_STATUS_LABEL: Record<string, string> = {
  open: "待启动",
  active: "执行中",
  done: "完成",
  failed: "失败",
  cancelled: "已取消",
};

export const assignmentStatusLabel = (status: string | undefined): string =>
  (status ? ASSIGNMENT_STATUS_LABEL[status] : undefined) ?? status ?? "未记录";

/** The built-in Source/Bot words, for the board's one-line source. An
 * unknown plugin source keeps its own token. */
export const SOURCE_LABEL: Record<string, string> = {
  github: "GitHub",
  manual: "手动",
  fixture: "演示",
};

export const THREAD_KIND_LABEL: Record<string, string> = {
  issue: "Issue",
  pull_request: "PR",
  conversation: "会话",
  thread: "讨论",
};

/** A ticket's source, in one line: `GitHub · Issue #1042`. */
export function superagentSourceText(
  thread: { source?: string; kind?: string; id?: string } | undefined,
  fallback?: string,
): string {
  if (!thread) return fallback ? (SOURCE_LABEL[fallback] ?? fallback) : "未记录来源";
  const rawSource = thread.source || fallback || "";
  const source = rawSource ? (SOURCE_LABEL[rawSource] ?? rawSource) : "来源";
  const kind = thread.kind ? ` · ${THREAD_KIND_LABEL[thread.kind] ?? thread.kind}` : "";
  const id = thread.id ? ` #${thread.id}` : "";
  return `${source}${kind}${id}`;
}

/** The requester's readable name: display, then login, then id. */
export function superagentRequesterText(ticket: SuperagentTicketView): string {
  const requester = ticket.mandate?.requester;
  if (!requester) return "未记录发起人";
  return requester.display || requester.login || requester.id;
}

// ---- the four headline counters ------------------------------------------

export interface SuperagentCounts {
  /** 待人工: blocked, or any ticket with an open Attention. */
  waiting: number;
  /** 执行中: an `active` ticket without an open Attention (a worker is doing
   * the work). `open` and `cancelling` are NOT folded in: they have their own
   * counters so the tile's copy stays true. */
  running: number;
  /** 待派工: `open` (accepted, no worker yet). */
  pending: number;
  /** 停止中: `cancelling` (a stop is propagating). */
  stopping: number;
  done: number;
  /** 失败: failed or cancelled. */
  failed: number;
  /** A status token this build does not know (kept visible, never folded into
   * one of the four and silently lost). */
  other: number;
  total: number;
}

/** The bucket a ticket falls in. Precedence is product order: a question
 * waiting for a human outranks the worker's underlying status (an `active`
 * ticket with an open Attention is 待人工), then terminal states, then work,
 * then the two transitional states (`open`, `cancelling`). */
export function superagentBucket(
  ticket: SuperagentTicketView,
): "waiting" | "running" | "pending" | "stopping" | "done" | "failed" | "other" {
  const status = ticket.status;
  if ((ticket.openAttentionCount ?? 0) > 0 || status === "blocked") return "waiting";
  if (status === "done") return "done";
  if (status === "failed" || status === "cancelled") return "failed";
  if (status === "active") return "running";
  if (status === "open") return "pending";
  if (status === "cancelling") return "stopping";
  return "other";
}

/** The board's counters, derived from its rows only. The server's status map
 * is deliberately NOT a second source: it has no open-Attention dimension, so
 * mixing it with the rows would double-count a ticket that is `active` and
 * waiting on a human. `total` is therefore the rows' length, and every row
 * lands in exactly one bucket. */
export function superagentCounts(tickets: SuperagentTicketView[]): SuperagentCounts {
  const counts: SuperagentCounts = {
    waiting: 0,
    running: 0,
    pending: 0,
    stopping: 0,
    done: 0,
    failed: 0,
    other: 0,
    total: tickets.length,
  };
  for (const ticket of tickets) {
    counts[superagentBucket(ticket)] += 1;
  }
  return counts;
}

/** The open questions of one ticket, from the inbox read (the board row only
 * carries a count, never the question text). */
export function openAttentionsOf(items: SuperagentAttentionView[], ticketId: string): SuperagentAttentionView[] {
  return items.filter((item) => item.workItemId === ticketId && (item.status ?? "open") === "open");
}

// ---- deadlines ------------------------------------------------------------

export interface SuperagentDeadline {
  /** Parsed instant, ms since epoch; absent for no/invalid deadline. */
  at?: number;
  expired: boolean;
  text: string;
}

const two = (value: number): string => (value < 10 ? `0${value}` : String(value));

/** What a question's deadline says: `今天 18:00 截止 · 剩 2 小时` / `已过期 ·
 * 11 月 26 日 09:12` / `无期限`. Built from local getters so every host prints
 * the same words (no locale call). `now` is injectable for tests. */
export function superagentDeadline(deadline: string | undefined, now = Date.now()): SuperagentDeadline {
  if (!deadline) return { expired: false, text: "无期限" };
  const at = Date.parse(deadline);
  if (!Number.isFinite(at)) return { expired: false, text: "期限无法识别" };
  const date = new Date(at);
  // Go's zero `time.Time` is `0001-01-01T00:00:00Z`, i.e. "no deadline" on the
  // wire; it must not read as a question that expired two thousand years ago.
  if (date.getFullYear() <= 1) return { expired: false, text: "无期限" };
  const clock = `${two(date.getHours())}:${two(date.getMinutes())}`;
  const start = (value: Date) => new Date(value.getFullYear(), value.getMonth(), value.getDate()).getTime();
  const today = start(new Date(now));
  const day = start(date);
  const absolute = `${date.getMonth() + 1} 月 ${date.getDate()} 日 ${clock}`;
  if (at <= now) {
    return { at, expired: true, text: `已过期 · ${absolute}` };
  }
  const remaining = at - now;
  const hours = Math.floor(remaining / 3_600_000);
  const minutes = Math.floor((remaining % 3_600_000) / 60_000);
  const left =
    hours > 0
      ? `剩 ${hours} 小时${minutes > 0 ? ` ${minutes} 分` : ""}`
      : `剩 ${Math.max(1, minutes)} 分`;
  const when = day === today ? `今天 ${clock}` : day === today + 86_400_000 ? `明天 ${clock}` : absolute;
  return { at, expired: false, text: `${when} 截止 · ${left}` };
}

// ---- the page's data state -----------------------------------------------

export interface SuperagentFailure {
  status: number | null;
  message: string;
  requestId?: string;
  requiredKey?: string;
}

export type SuperagentActionKind = "answer" | "resume" | "stop" | "retry" | "dismiss";

export interface SuperagentActionState {
  id: string;
  kind: SuperagentActionKind;
}

export interface SuperagentActionError extends SuperagentActionState {
  failure: SuperagentFailure;
}

/** The server's super-agent refusals are English one-liners. The two states a
 * page must explain are identified by the response's own facts, not by
 * matching its prose: a 503 is an unwired/unavailable board, and a 403 without
 * `requiredKey` is the independent operator seam (the read/write gates always
 * name their key). */
type SuperagentFailureContext = "read" | "action";

function superagentErrorMessage(error: KeelApiError, context: SuperagentFailureContext): string {
  if (error.status === 503) {
    return context === "action"
      ? "工单操作暂时不可用（服务端审计或投递未就绪）；请确认已启用 superagent 后重试。"
      : "工单看板暂时不可用；请确认部署已启用 superagent 模式后重试。";
  }
  if (context === "action" && error.status === 403 && !error.requiredKey) {
    return "服务端还没有接通 operator 授权（OperatorAuthorizer）；恢复、停止或答复需要管理员启用该授权。";
  }
  return errorMessageOf(error);
}

const failureOf = (error: unknown, context: SuperagentFailureContext): SuperagentFailure => {
  if (error instanceof KeelApiError) {
    const failure: SuperagentFailure = { status: error.status, message: superagentErrorMessage(error, context) };
    if (error.requestId) failure.requestId = error.requestId;
    if (error.requiredKey) failure.requiredKey = error.requiredKey;
    return failure;
  }
  return { status: null, message: errorMessageOf(error) };
};

/** The super-agent board's state and writes. A read is one board + one inbox
 * call; a write is one route call followed by a refresh, so every operation
 * leaves the page on the server's new facts instead of a local guess. */
class SuperagentAccess {
  readonly board = signal<SuperagentBoardView | null>(null);
  readonly loading = signal(false);
  readonly error = signal<SuperagentFailure | null>(null);
  readonly inbox = signal<SuperagentAttentionView[]>([]);
  readonly inboxError = signal<SuperagentFailure | null>(null);
  /** True once a read answered the inbox (even with no rows). The invalidation
   * stream uses it, with `board !== null`, to decide whether the page already
   * has projections to re-read; a chapter that was never opened stays quiet. */
  readonly inboxLoaded = signal(false);
  /** When the last successful read landed (the header's 读取于 line). */
  readonly updatedAt = signal<string | null>(null);
  /** The write in flight; every action control is disabled while it runs. */
  readonly action = signal<SuperagentActionState | null>(null);
  /** The last write's failure, attached to the control that produced it. */
  readonly actionError = signal<SuperagentActionError | null>(null);
  /** The last write's success sentence, shown above the board. */
  readonly notice = signal<string | null>(null);

  private readToken = 0;
  /** A read has been attempted for this account (including a failed one). The
   * invalidation stream may re-read then; a chapter that was never opened
   * stays quiet. */
  private attempted = false;
  /** One fact arrived while a read was in flight: re-read once it settles
   * (the in-flight answer may predate the write). */
  private pendingRefresh = false;

  /** Read once when the page mounts; a failed read waits for an explicit
   * retry/refresh instead of re-issuing on every render. */
  ensure(): void {
    if (!can("task.read")) return;
    if (this.loading() || this.board() !== null || this.error() !== null) return;
    this.attempted = true;
    void this.load();
  }

  async refresh(): Promise<void> {
    this.attempted = true;
    await this.load();
  }

  /** Re-read the board/inbox for a `superagent` invalidation fact (the identity
   * stream's one SSE kind). Only a page that already attempted a read re-reads
   * (an error page must recover without a manual retry); a chapter the reader
   * never opened must not fire background requests. A fact that lands while a
   * read is in flight is remembered and re-read once after it settles; a burst
   * is coalesced by the stream. The manual 刷新 control remains the fallback. */
  async refreshForEvent(kinds: ReadonlySet<string>): Promise<void> {
    if (!kinds.has("superagent")) return;
    if (!this.attempted) return;
    if (this.loading()) {
      this.pendingRefresh = true;
      return;
    }
    await this.load({ silent: true });
  }

  private async load(options: { silent?: boolean } = {}): Promise<void> {
    const api = store.superagentApi();
    const token = ++this.readToken;
    this.loading.set(true);
    this.error.set(null);
    // A background/SSE re-read is not an operator action: the last write's
    // success or failure sentence stays until an explicit action (which calls
    // the non-silent load) or the manual 刷新 clears it.
    if (options.silent !== true) {
      this.notice.set(null);
      this.actionError.set(null);
    }
    try {
      if (!api) {
        this.board.set(null);
        this.inbox.set([]);
        this.inboxLoaded.set(false);
        this.inboxError.set(null);
        this.error.set({ status: null, message: "这个部署没有接通超级助理看板。" });
        return;
      }
      // The two reads are independent: a failed inbox must not blank a
      // readable board (the page shows the inbox's own failure beside it).
      const [boardResult, inboxResult] = await Promise.allSettled([
        api.superagentBoard(),
        api.superagentAttention(),
      ]);
      if (token !== this.readToken) return;
      if (boardResult.status === "rejected") {
        this.board.set(null);
        this.inbox.set([]);
        this.inboxLoaded.set(false);
        this.inboxError.set(null);
        this.error.set(failureOf(boardResult.reason, "read"));
        return;
      }
      this.board.set(boardResult.value);
      if (inboxResult.status === "fulfilled") {
        this.inbox.set(inboxResult.value.items);
        this.inboxLoaded.set(true);
        this.inboxError.set(null);
      } else {
        this.inbox.set([]);
        this.inboxLoaded.set(false);
        this.inboxError.set(failureOf(inboxResult.reason, "read"));
      }
      this.updatedAt.set(new Date().toISOString());
    } finally {
      if (token === this.readToken) {
        this.loading.set(false);
        // A fact arrived during this read: its answer may predate the write
        // that emitted the fact, so read once more now that the first answer
        // has settled. This extra read is background work: it must not clear
        // the operator's write feedback.
        if (this.pendingRefresh) {
          this.pendingRefresh = false;
          void this.load({ silent: true });
        }
      }
    }
  }

  /** Answer one question. The refresh is part of the operation: an answered
   * question leaves the inbox and its ticket moves on in the same turn. A 202
   * (`deliveryPending`) keeps the answer and says it is waiting for the worker;
   * a recorded-but-failed delivery points at the ticket board's retry entry. */
  async answer(attentionId: string, input: SuperagentAnswerInput): Promise<boolean> {
    if (!this.canWrite()) return false;
    if (this.action()) return false;
    const api = store.superagentApi();
    this.action.set({ id: attentionId, kind: "answer" });
    this.actionError.set(null);
    this.notice.set(null);
    try {
      if (!api) throw new Error("这个部署没有接通超级助理看板。");
      const result = await api.answerSuperagentAttention(attentionId, input);
      await this.load();
      if (result.deliveryFailed) {
        this.notice.set("答复已记录，但投递失败；请在这张工单的「待投递输入」里重试或清除。");
      } else if (result.deliveryPending) {
        this.notice.set("答复已记录，等待投递到员工；投递成功后工单会自动继续。");
      } else {
        this.notice.set(this.error() ? "答复已提交；看板刷新失败，请点「刷新」。" : "答复已提交，工单已继续执行。");
      }
      return true;
    } catch (error) {
      this.actionError.set({ id: attentionId, kind: "answer", failure: failureOf(error, "action") });
      return false;
    } finally {
      this.clearAction(attentionId, "answer");
    }
  }

  /** Re-arm one durable pending input whose delivery failed permanently. */
  async retryPending(ticketId: string, pendingId: string): Promise<boolean> {
    if (!this.canWrite()) return false;
    if (this.action()) return false;
    const api = store.superagentApi();
    this.action.set({ id: pendingId, kind: "retry" });
    this.actionError.set(null);
    this.notice.set(null);
    try {
      if (!api) throw new Error("这个部署没有接通超级助理看板。");
      await api.retrySuperagentPending(ticketId, pendingId);
      await this.load();
      this.notice.set(this.error() ? "重试请求已提交；看板刷新失败，请点「刷新」。" : "已重新投递；仍未成功时入口会保留并显示服务端的错误。");
      return true;
    } catch (error) {
      this.actionError.set({ id: pendingId, kind: "retry", failure: failureOf(error, "action") });
      return false;
    } finally {
      this.clearAction(pendingId, "retry");
    }
  }

  /** Drop one durable pending input at the operator's explicit request. */
  async dismissPending(ticketId: string, pendingId: string): Promise<boolean> {
    if (!this.canWrite()) return false;
    if (this.action()) return false;
    const api = store.superagentApi();
    this.action.set({ id: pendingId, kind: "dismiss" });
    this.actionError.set(null);
    this.notice.set(null);
    try {
      if (!api) throw new Error("这个部署没有接通超级助理看板。");
      await api.dismissSuperagentPending(ticketId, pendingId);
      await this.load();
      this.notice.set(this.error() ? "清除请求已提交；看板刷新失败，请点「刷新」。" : "待投递输入已清除。");
      return true;
    } catch (error) {
      this.actionError.set({ id: pendingId, kind: "dismiss", failure: failureOf(error, "action") });
      return false;
    } finally {
      this.clearAction(pendingId, "dismiss");
    }
  }

  /** Reopen a blocked/finished ticket with a human-follow-up assignment. */
  async resume(ticketId: string): Promise<boolean> {
    if (!this.canWrite()) return false;
    if (this.action()) return false;
    const api = store.superagentApi();
    this.action.set({ id: ticketId, kind: "resume" });
    this.actionError.set(null);
    this.notice.set(null);
    try {
      if (!api) throw new Error("这个部署没有接通超级助理看板。");
      await api.resumeSuperagentTicket(ticketId, "operator resume");
      await this.load();
      this.notice.set(this.error() ? "恢复请求已提交；看板刷新失败，请点「刷新」。" : "工单已恢复，等待新的执行结果。");
      return true;
    } catch (error) {
      this.actionError.set({ id: ticketId, kind: "resume", failure: failureOf(error, "action") });
      return false;
    } finally {
      this.clearAction(ticketId, "resume");
    }
  }

  /** The human stop: the mandate is revoked and the server cancels every bound
   * session through the core Host. */
  async stop(ticketId: string): Promise<boolean> {
    if (!this.canWrite()) return false;
    if (this.action()) return false;
    const api = store.superagentApi();
    this.action.set({ id: ticketId, kind: "stop" });
    this.actionError.set(null);
    this.notice.set(null);
    try {
      if (!api) throw new Error("这个部署没有接通超级助理看板。");
      await api.stopSuperagentTicket(ticketId, "operator stop");
      await this.load();
      this.notice.set(this.error() ? "停止请求已提交；看板刷新失败，请点「刷新」。" : "工单已停止；Mandate 已撤销，记录保留可审计。");
      return true;
    } catch (error) {
      this.actionError.set({ id: ticketId, kind: "stop", failure: failureOf(error, "action") });
      return false;
    } finally {
      this.clearAction(ticketId, "stop");
    }
  }

  /** Drop the page's state (the account changed). */
  reset(): void {
    this.readToken += 1;
    this.attempted = false;
    this.pendingRefresh = false;
    this.board.set(null);
    this.loading.set(false);
    this.error.set(null);
    this.inbox.set([]);
    this.inboxError.set(null);
    this.inboxLoaded.set(false);
    this.updatedAt.set(null);
    this.action.set(null);
    this.actionError.set(null);
    this.notice.set(null);
  }

  /** The display gate for every write. The server also requires its operator
   * authorization; this only decides whether the UI may offer the control. */
  canWrite(): boolean {
    return can("task.write");
  }

  private clearAction(id: string, kind: SuperagentActionKind): void {
    const current = this.action();
    if (current?.id === id && current.kind === kind) this.action.set(null);
  }
}

export const superagentAccess = new SuperagentAccess();
