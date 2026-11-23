// The run surface's task panel — change keel-harness-v2, W4.
//
// 任务图 / 事件流 / 资源预览 are one projection of `GET /v1/sessions/{id}/task-panel`
// (task cards + member cards + ancestors + attachment references + the task
// event stream) plus `GET /v1/sessions/{id}/file-preview` for the reference a
// reader opens. Nothing here holds task state: this module reads, folds and
// formats, and every function is pure so the tree can be reasoned about (and
// tested) without a transport.
//
// What the fold refuses to do:
//   - invent a node: a card exists only when the read carried it;
//   - invent a parent label: a parent that is outside the read is named by its
//     id (the surface says 另一会话);
//   - invent an order: the panel's own order is (createdSeq, taskId); a member
//     card's is the read's own (updatedAt desc, taskId desc);
//   - claim a resource matches: a preview reports the manifest's current hash
//     and the reference is compared, never assumed.
import { signal } from "nsh";
import type {
  FilePreviewView,
  SessionFileView,
  SessionFilesView,
  TaskPanelAttachmentView,
  TaskPanelCardView,
  TaskPanelEventView,
  TaskPanelView,
} from "./contract";
import { KeelApiError, errorMessageOf } from "./http";
import { store } from "./store";

// ---- labels ---------------------------------------------------------------

/** The card statuses the task log defines (internal/tasklog/types.go). A
 * token this build does not know keeps its raw text. */
export type TaskCardStatus = "open" | "in_progress" | "blocked" | "done" | "closed";

export const CARD_STATUS_LABEL: Record<string, string> = {
  open: "待处理",
  in_progress: "进行中",
  blocked: "受阻",
  done: "完成",
  closed: "已关闭",
};

export const cardStatusLabel = (status: string | undefined): string =>
  (status ? CARD_STATUS_LABEL[status] : undefined) ?? status ?? "未知";

/** A card's origin in the reader's words. */
export const cardOriginLabel = (origin: string | undefined): string => {
  switch (origin) {
    case "session":
      return "会话初始卡";
    case "tool":
      return "工具创建";
    case "subagent":
      return "子代理";
    case "fork":
      return "分叉";
    default:
      return origin ?? "";
  }
};

/** The six task event kinds (spec: task-log). A foreign kind keeps its own
 * text and is marked 未知 in the surface rather than translated. */
export const EVENT_KIND_LABEL: Record<string, string> = {
  "task.created": "创建",
  "task.updated": "更新",
  "task.attached": "挂载资源",
  "task.detached": "解绑资源",
  "task.closed": "关闭",
  "task.note": "注记",
};

export const eventKindLabel = (kind: string): string => EVENT_KIND_LABEL[kind] ?? kind;

/** The status badge's tone: an unknown token stays neutral. */
export type CardTone = "info" | "success" | "destructive" | "secondary" | "outline";

export const cardTone = (status: string | undefined): CardTone => {
  switch (status) {
    case "in_progress":
      return "info";
    case "done":
      return "success";
    case "blocked":
      return "destructive";
    case "closed":
      return "secondary";
    case "open":
    default:
      return "outline";
  }
};

// ---- the tree -------------------------------------------------------------

/** One row of the panel's tree: a card plus how it relates to the session's
 * root. A card of another session is a member; a parent outside the session is
 * an ancestor and is drawn above the root with 另一会话. */
export interface TaskTreeRow {
  card: TaskPanelCardView;
  depth: number;
  /** The label of the edge that reaches this row (分叉 / 子代理 / 工具创建 / 初始). */
  edge: string;
  /** The card that reaches it: a card id in this read, or an id the read did
   * not carry (an external parent). */
  parentId?: string;
  /** True when the parent is not in the read: the row's parent column says
   * 另一会话 instead of a title it does not have. */
  externalParent: boolean;
  /** True for a card whose own row lives in another session. */
  member: boolean;
}

/** The fork fields a row exposes: the origin sequence and the branch title. */
export interface ForkNote {
  taskId: string;
  originSeq: number;
  title: string;
}

/** The ordered tree: the session's own cards by creation sequence, each
 * followed by the member cards whose parent is that card (newest activity
 * first, the read's order), the member cards of an ancestor last. */
export function taskTree(panel: TaskPanelView): TaskTreeRow[] {
  const cards = panel.cards ?? [];
  const members = panel.members ?? [];
  const ancestors = panel.ancestors ?? [];
  const byId = new Map<string, TaskPanelCardView>();
  for (const card of [...cards, ...members, ...ancestors]) {
    if (card.taskId && !byId.has(card.taskId)) byId.set(card.taskId, card);
  }
  const ownIds = new Set(cards.map((card) => card.taskId));

  // The root of the session's own tree: the earliest card whose parent is not
  // an own card, else the earliest own card. A session starts with one card;
  // a fork's session carries the fork card.
  const ordered = [...cards].sort((left, right) => {
    const leftSeq = left.createdSeq ?? Number.POSITIVE_INFINITY;
    const rightSeq = right.createdSeq ?? Number.POSITIVE_INFINITY;
    if (leftSeq !== rightSeq) return leftSeq - rightSeq;
    return left.taskId < right.taskId ? -1 : left.taskId > right.taskId ? 1 : 0;
  });
  const roots = ordered.filter((card) => !card.parentTaskId || !ownIds.has(card.parentTaskId));
  const root = roots[0] ?? ordered[0];

  const rows: TaskTreeRow[] = [];
  // Ancestors first, so a reader sees where the session branched from before
  // its own root. They are not part of the tree's own count.
  for (const card of ancestors) {
    rows.push({
      card,
      depth: 0,
      edge: card.origin === "fork" ? "分叉原点" : "父卡",
      parentId: card.parentTaskId,
      externalParent: card.parentTaskId !== undefined,
      member: true,
    });
  }
  const childrenOf = (parentId: string): TaskPanelCardView[] => members.filter((card) => card.parentTaskId === parentId);
  const ownChildren = (parentId: string): TaskPanelCardView[] => ordered.filter((card) => card.parentTaskId === parentId);

  const walk = (card: TaskPanelCardView, depth: number, edge: string, externalParent: boolean, member: boolean) => {
    if (rows.some((row) => row.card.taskId === card.taskId)) return;
    rows.push({ card, depth, edge, parentId: card.parentTaskId, externalParent, member: isMember(card, ownIds) });
    for (const child of ownChildren(card.taskId)) {
      walk(child, depth + 1, cardOriginLabel(child.origin) || "子卡", false, false);
    }
    for (const child of childrenOf(card.taskId)) {
      const childEdge = child.origin === "fork" ? "分叉" : "子代理";
      walk(child, depth + 1, childEdge, false, true);
    }
  };
  if (root) {
    const rootExternal = root.parentTaskId !== undefined && !ownIds.has(root.parentTaskId) && !byId.has(root.parentTaskId);
    walk(root, 0, cardOriginLabel(root.origin) || "初始", rootExternal, false);
  }
  // Every card the read carried appears exactly once: a card whose parent link
  // names a card outside the tree is walked as a second root (the read's own
  // shape, not a dropped node).
  for (const card of ordered) {
    walk(card, 0, cardOriginLabel(card.origin) || "卡", card.parentTaskId !== undefined && !byId.has(card.parentTaskId), false);
  }
  for (const card of members) {
    walk(card, 0, card.origin === "fork" ? "分叉" : "子代理", card.parentTaskId !== undefined && !byId.has(card.parentTaskId), true);
  }
  return rows;
}

const isMember = (card: TaskPanelCardView, ownIds: Set<string>): boolean =>
  card.member === true || !ownIds.has(card.taskId);

/** The branches the panel reports: one per fork card, naming the card and the
 * ledger position it branched from. A session with no fork card has none. */
export function forkBranches(panel: TaskPanelView): ForkNote[] {
  const all = [...(panel.cards ?? []), ...(panel.members ?? []), ...(panel.ancestors ?? [])];
  return all
    .filter((card) => card.origin === "fork" && card.originSeq !== undefined)
    .map((card) => ({ taskId: card.taskId, originSeq: card.originSeq!, title: card.title }))
    .sort((left, right) => left.originSeq - right.originSeq);
}

/** The counts the panel's head states: own cards, member cards, live resource
 * references. */
export function panelCounts(panel: TaskPanelView): { cards: number; members: number; attachments: number; events: number } {
  return {
    cards: (panel.cards ?? []).length,
    members: (panel.members ?? []).length,
    attachments: (panel.attachments ?? []).length,
    events: (panel.events ?? []).length,
  };
}

// ---- the event stream -----------------------------------------------------

/** One event line as the surface renders it: a time, the card's title (or its
 * id when the read did not carry the card), the kind, and whichever of the
 * event's own fields the kind carries. No sentence is composed: the line's
 * parts are the event's own strings. */
export interface TaskEventLine {
  event: TaskPanelEventView;
  cardTitle: string;
  cardKnown: boolean;
  detail: string;
}

export function eventLines(panel: TaskPanelView): TaskEventLine[] {
  const titles = new Map<string, string>();
  for (const card of [...(panel.cards ?? []), ...(panel.members ?? []), ...(panel.ancestors ?? [])]) {
    titles.set(card.taskId, card.title);
  }
  const lines = (panel.events ?? []).map((event): TaskEventLine => {
    const title = titles.get(event.taskId);
    return {
      event,
      cardTitle: title ?? event.taskId,
      cardKnown: title !== undefined,
      detail: eventDetail(event),
    };
  });
  return lines.sort((left, right) => left.event.seq - right.event.seq);
}

/** The part of an event that its own kind carries: a title, a status, a
 * (path, hash) pair, a note's text, a close reason. A field the kind did not
 * carry stays empty. */
export function eventDetail(event: TaskPanelEventView): string {
  switch (event.kind) {
    case "task.created":
    case "task.updated": {
      const parts = [event.title, event.status ? cardStatusLabel(event.status) : undefined].filter(
        (part): part is string => typeof part === "string" && part !== "",
      );
      return parts.join(" · ");
    }
    case "task.attached":
    case "task.detached":
      return event.path ? `${event.path} · ${shortHash(event.hash)}` : "";
    case "task.note":
      return event.text ?? "";
    case "task.closed":
      return event.reason ? `原因：${event.reason}` : "";
    default:
      return event.unparsed ? "这个版本不认识的事件" : "";
  }
}

/** `sha256:3a3a…` → `sha256:3a3a…` shortened for a row. The full hash stays in
 * the event's own field and in the resource row. */
export const shortHash = (hash: string | undefined): string => {
  if (!hash) return "";
  const bare = hash.startsWith("sha256:") ? hash.slice("sha256:".length) : hash;
  return bare.length <= 12 ? hash : `sha256:${bare.slice(0, 10)}…`;
};

// ---- resources ------------------------------------------------------------

/** One file the session's manifest attributes to a task card: the manifest's
 * own `lastTaskId`, which the file plugin stamps on every write. It is the
 * "tool/fs reference" half of the panel that the task events do not carry: an
 * attach event records (path, hash), while a write records the card that wrote
 * it. Both are read; neither is composed. */
export interface FileReference {
  path: string;
  size: number;
  updatedSeq: number;
  taskId: string;
  cardTitle: string;
}

/** The files a card owns, limited to the own cards the panel drew. A file
 * whose `lastTaskId` names a card outside this session's tree (a member card's
 * row lives in another session) is not listed: the panel never claims another
 * session's card. A reference the panel already lists as a live attachment is
 * left to that group, which carries its (path, hash); this group adds the
 * writes the task events do not carry, so one reference is not printed twice. */
export function fileReferences(panel: TaskPanelView, files: SessionFileView[]): FileReference[] {
  const order = new Map<string, number>();
  const titles = new Map<string, string>();
  (panel.cards ?? []).forEach((card, index) => {
    if (titles.has(card.taskId)) return;
    order.set(card.taskId, index);
    titles.set(card.taskId, card.title);
  });
  // The manifest path is session-wide, so a live attachment of the same path
  // is the same file even when its card differs from the write's lastTaskId.
  const attached = new Set((panel.attachments ?? []).map((attachment) => attachment.path));
  const out: FileReference[] = [];
  for (const file of files) {
    const taskId = file.lastTaskId;
    if (!taskId || !titles.has(taskId)) continue;
    if (attached.has(file.path)) continue;
    out.push({
      path: file.path,
      size: file.size,
      updatedSeq: file.updatedSeq,
      taskId,
      cardTitle: titles.get(taskId) ?? taskId,
    });
  }
  // The panel's own tree order decides the groups (own cards by creation
  // sequence), then the path: two reads of the same session render alike.
  return out.sort((left, right) => {
    const leftOrder = order.get(left.taskId) ?? Number.MAX_SAFE_INTEGER;
    const rightOrder = order.get(right.taskId) ?? Number.MAX_SAFE_INTEGER;
    if (leftOrder !== rightOrder) return leftOrder - rightOrder;
    return left.path < right.path ? -1 : left.path > right.path ? 1 : 0;
  });
}

/** The preview a reader asked for. The resolved file is the manifest's current
 * one; `matchesReference` says whether it is still the bytes the card
 * recorded. */
export interface ResourcePreview {
  attachment: TaskPanelAttachmentView;
  preview: FilePreviewView;
  /** True when the file the manifest holds is not the referenced content. */
  changed: boolean;
}

/** The comparison the surface states: an answered `matchesReference` wins; a
 * deployment that did not answer one leaves the fact unknown (`null`), which
 * the surface prints as 未核对 rather than 一致. */
export function resourceChanged(attachment: TaskPanelAttachmentView, preview: FilePreviewView): boolean | null {
  if (preview.matchesReference === undefined) return null;
  if (preview.matchesReference) return false;
  return true;
}

/** One line naming the preview's size and pages. */
export function previewSummary(preview: FilePreviewView): string {
  const size = preview.size >= 1024 * 1024 ? `${(preview.size / 1024 / 1024).toFixed(1)} MB` : `${Math.max(1, Math.round(preview.size / 1024))} KB`;
  if (preview.totalLines <= 0) return size;
  return `${size} · ${preview.returnedLines}/${preview.totalLines} 行`;
}

/** The truncation facts the panel reports, as sentences it can stand behind:
 * each names the read's own bound, never a guessed one. */
export function panelGaps(panel: TaskPanelView): string[] {
  const gaps: string[] = [];
  const limits = panel.limits;
  if (limits?.truncated) {
    const count = limits.truncatedMemberCount ?? 0;
    // The count is exact when the read saw every linked row, and a floor of 1
    // when the read stopped at its own bound. A count of 1 cannot say whether
    // one row was left or the bound itself hid the rest, so the sentence names
    // the bound without a number; a larger floor is still a true minimum.
    gaps.push(
      count > 1
        ? `成员卡超过 ${limits.memberLimit} 张的上限，这里只画了前 ${limits.memberLimit} 张（至少还有 ${count} 张没画）。`
        : `成员卡达到 ${limits.memberLimit} 张上限，更早的成员没有画出来。`,
    );
  }
  if (limits?.eventsTruncated) {
    gaps.push(`事件达到 ${limits.eventLimit} 条上限，更早的事件没有读入。`);
  }
  if (limits?.ancestorsTruncated) {
    gaps.push("这条链的上层卡超过读取上限，更早的父卡没有解析。");
  }
  for (const line of eventLines(panel)) {
    if (line.event.unparsed) {
      gaps.push("事件流里有这个版本不认识的 kind：它们只显示 kind 与序号。");
      break;
    }
  }
  return gaps;
}

/** The panel's five states, decided from the read alone. */
export type TaskPanelPhase = "loading" | "empty" | "error" | "permission" | "success";

export function panelPhase(input: {
  loading: boolean;
  error: { status: number | null } | null;
  panel: TaskPanelView | null;
}): TaskPanelPhase {
  if (input.loading && !input.panel) return "loading";
  if (input.error !== null) return input.error.status === 403 ? "permission" : "error";
  if (!input.panel) return "empty";
  const counts = panelCounts(input.panel);
  return counts.cards === 0 && counts.members === 0 && counts.events === 0 ? "empty" : "success";
}


// ---- the entry ------------------------------------------------------------

/** Whether the app's transport can answer the panel's read at all (the
 * production `KeelHttp` and the fixture both can). A transport that cannot
 * leaves the entry out; a wired deployment that answers a failure still shows
 * the entry and the failure. */
export function isTaskPanelReadable(): boolean {
  return typeof store.identityApi()?.taskPanel === "function";
}

/** Whether the run surface offers the panel at all. The deployment's
 * capability answer decides; the local entry then reads the projection. The
 * run page calls this with the capability map the app already read. */
export function shouldShowTaskPanel(features: Record<string, unknown> | null | undefined, reading: boolean): boolean {
  if (features && features["task.panel"] === false) return false;
  if (features && features["task.panel"] === true) return true;
  // No capability answer: the transport's own read decides (a deployment that
  // never wired the projection answers the error face instead of nothing).
  return reading;
}

/** The panel's five states, decided from the read alone. */

/** The failure face the panel renders: a status (403 = permission) and the
 * server's message. Never a fabricated one. */
export interface TaskPanelFailure {
  status: number | null;
  message: string;
  requestId?: string;
  requiredKey?: string;
}

const failureOf = (error: unknown): TaskPanelFailure => {
  if (error instanceof KeelApiError) {
    const failure: TaskPanelFailure = { status: error.status, message: errorMessageOf(error) };
    if (error.requestId) failure.requestId = error.requestId;
    if (error.requiredKey) failure.requiredKey = error.requiredKey;
    return failure;
  }
  return { status: null, message: errorMessageOf(error) };
};

const failureKey = (sessionId: string, taskId?: string): string =>
  taskId ? `${sessionId}:${taskId}` : sessionId;

class TaskPanelStore {
  readonly panel = signal<TaskPanelView | null>(null);
  readonly loading = signal(false);
  readonly error = signal<TaskPanelFailure | null>(null);
  /** The session the current answer belongs to: a answer for a session the
   * reader already left never lands (the panel is session-scoped). */
  readonly sessionId = signal<string | null>(null);
  /** The session manifest's rows, read with the panel: each row's own
   * `lastTaskId` attributes it to a card (the tool/fs reference an attach
   * event does not carry). A failed read is recorded, never substituted with
   * an empty list that reads as "no file". */
  readonly files = signal<SessionFileView[]>([]);
  /** True once a transport answered the manifest read (even with no rows), so
   * the resource view can tell "no attributed file" apart from "this
   * deployment never wired the read". */
  readonly filesAvailable = signal(false);
  /** The manifest read stopped at its own row cap: attributed files may be
   * missing, and the surface says so instead of implying the list is whole. */
  readonly filesTruncated = signal(false);
  readonly filesError = signal<TaskPanelFailure | null>(null);
  /** The open resource: the attachment the reader asked to preview. */
  readonly preview = signal<{ attachment: TaskPanelAttachmentView; view: FilePreviewView } | null>(null);
  readonly previewLoading = signal<string | null>(null);
  readonly previewError = signal<TaskPanelFailure | null>(null);
  /** The card the reader selected (the events list filters to it when set). */
  readonly selectedTaskId = signal<string | null>(null);
  /** The production of the panel's sections: 图 / 事件 / 资源. */
  readonly section = signal<"graph" | "events" | "resources">("graph");

  private readToken = 0;
  private previewToken = 0;
  private readonly attempted = new Set<string>();

  private api() {
    return store.identityApi();
  }

  /** Read the panel once per session. A repeated call after a success is a
   * no-op; `refresh` forces a re-read (the panel's 刷新 control). */
  ensure(sessionId: string): void {
    if (!sessionId) return;
    // An effect that calls this re-runs whenever the signals it reads change,
    // so every branch here must be stable once a read is in flight or has
    // landed: loading is in flight, a success is done, and an error stays an
    // error until 重试/刷新 clears it (otherwise a failed read would re-run the
    // failed request on every render).
    if (this.loading() && this.sessionId() === sessionId) return;
    if (this.sessionId() === sessionId && this.panel() !== null && this.error() === null) return;
    // An error belongs to the session that produced it: switching to another
    // session must start its own read instead of inheriting this failure.
    if (this.sessionId() === sessionId && this.attempted.has(sessionId) && this.error() !== null) return;
    this.attempted.add(sessionId);
    void this.load(sessionId);
  }

  async refresh(sessionId: string): Promise<void> {
    this.attempted.delete(sessionId);
    await this.load(sessionId);
  }

  async load(sessionId: string): Promise<void> {
    if (!sessionId) return;
    const api = this.api();
    const token = ++this.readToken;
    // A read for a new session must not render the previous session's tree
    // while it lands: clear the whole panel state first, then let the new
    // answer fill it. A same-session refresh keeps the current view.
    if (this.sessionId() !== sessionId) {
      this.panel.set(null);
      this.files.set([]);
      this.filesAvailable.set(false);
      this.filesTruncated.set(false);
      this.preview.set(null);
      this.previewLoading.set(null);
      this.previewError.set(null);
      this.selectedTaskId.set(null);
    }
    this.loading.set(true);
    this.error.set(null);
    this.filesError.set(null);
    this.filesAvailable.set(false);
    this.filesTruncated.set(false);
    this.sessionId.set(sessionId);
    if (!api?.taskPanel) {
      // A deployment without the projection is not an empty tree: it says so.
      this.panel.set(null);
      this.files.set([]);
      this.error.set({ status: null, message: "这个部署没有接通任务面板读数。" });
      this.loading.set(false);
      return;
    }
    try {
      // The panel projection and the session manifest are one view of the same
      // session: both are read, and a manifest failure is reported next to the
      // resources rather than failing the whole panel.
      const [view, manifest] = await Promise.all([
        api.taskPanel(sessionId),
        this.filesApi(sessionId, token),
      ]);
      if (token !== this.readToken) return;
      this.panel.set(view);
      this.files.set(manifest?.files ?? []);
      this.filesAvailable.set(manifest !== null);
      this.filesTruncated.set(manifest?.truncated === true);
      this.selectedTaskId.set(null);
      this.preview.set(null);
    } catch (error) {
      if (token !== this.readToken) return;
      this.panel.set(null);
      this.files.set([]);
      this.filesAvailable.set(false);
      this.filesTruncated.set(false);
      this.error.set(failureOf(error));
    } finally {
      if (token === this.readToken) this.loading.set(false);
    }
  }

  /** The session manifest (`GET /v1/sessions/{id}/files`), or null when this
   * transport has none. The panel calls it with the projection so one refresh
   * reads both; a failure is reported beside the resources, and a late answer
   * for a session the reader already left never lands. */
  private async filesApi(sessionId: string, token: number): Promise<SessionFilesView | null> {
    try {
      const manifest = await store.sessionFileManifest(sessionId);
      if (token !== this.readToken) return null;
      return manifest;
    } catch (error) {
      if (token === this.readToken) this.filesError.set(failureOf(error));
      return null;
    }
  }

  /** The reader's 重试: one explicit re-read. */
  retry(sessionId: string): void {
    if (!sessionId) return;
    this.attempted.delete(sessionId);
    void this.load(sessionId);
  }

  select(taskId: string | null): void {
    this.selectedTaskId.set(taskId);
  }

  setSection(section: "graph" | "events" | "resources"): void {
    this.section.set(section);
    if (section !== "resources") {
      this.preview.set(null);
      this.previewError.set(null);
    }
  }

  /** Open one attached reference: the path is the one the event recorded, and
   * the answer is the session manifest's current bytes for it. */
  async openResource(sessionId: string, attachment: TaskPanelAttachmentView): Promise<void> {
    if (!sessionId) return;
    const api = this.api();
    const token = ++this.previewToken;
    this.previewLoading.set(failureKey(sessionId, attachment.path));
    this.previewError.set(null);
    if (!api?.filePreview) {
      this.previewLoading.set(null);
      this.previewError.set({ status: null, message: "这个部署没有接通文件预览。" });
      return;
    }
    try {
      // A manifest-attributed file has no recorded reference hash: asking for
      // a comparison against "" would make the transport (or a harness) answer
      // one, so the empty string is sent as "no reference".
      const reference = attachment.hash === "" ? undefined : attachment.hash;
      const view = await api.filePreview(sessionId, attachment.path, reference);
      if (token !== this.previewToken) return;
      this.preview.set({ attachment, view });
    } catch (error) {
      if (token !== this.previewToken) return;
      this.preview.set(null);
      this.previewError.set(failureOf(error));
    } finally {
      if (token === this.previewToken) this.previewLoading.set(null);
    }
  }

  closePreview(): void {
    this.preview.set(null);
    this.previewError.set(null);
  }

  /** Drop the reader's transient panel state (the session changed). */
  reset(): void {
    this.readToken += 1;
    this.previewToken += 1;
    this.panel.set(null);
    this.files.set([]);
    this.filesAvailable.set(false);
    this.filesTruncated.set(false);
    this.filesError.set(null);
    this.loading.set(false);
    this.error.set(null);
    this.sessionId.set(null);
    this.preview.set(null);
    this.previewLoading.set(null);
    this.previewError.set(null);
    this.selectedTaskId.set(null);
    this.section.set("graph");
    this.attempted.clear();
  }
}

export const taskPanelAccess = new TaskPanelStore();
