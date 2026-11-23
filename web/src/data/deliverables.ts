// The workspace-changes / deliverables projection — change keel-harness-v2,
// W12 `keel.deliverables`.
//
// A deliverable is a `file_manifest` row: the path, the content hash and the
// size the file plugin already records. The status pane reads one bounded page
// through `GET /v1/sessions/{id}/deliverables` and previews a row through the
// manifest's own bounded read (`/file-preview`), so this module never invents a
// path, a hash or a second file truth.
import { signal } from "nsh";
import { can } from "./capabilities";
import type { DeliverableView, DeliverablesView, FilePreviewView } from "./contract";
import { KeelApiError, errorMessageOf } from "./http";
import { store } from "./store";

export type DeliverablesPhase = "loading" | "empty" | "error" | "permission" | "success";

export interface DeliverablesFailure {
  status: number | null;
  message: string;
}

const failureOf = (error: unknown): DeliverablesFailure => {
  if (error instanceof KeelApiError) {
    return {
      status: error.status,
      message: error.status === 403 ? "没有查看这个会话交付物的权限。" : errorMessageOf(error),
    };
  }
  return { status: null, message: errorMessageOf(error) };
};

/** A row's preview: the manifest's current bytes and whether they still match
 * the hash the row carried. */
export interface DeliverablePreview {
  row: DeliverableView;
  view: FilePreviewView;
  matches: boolean | null;
}

class DeliverablesAccess {
  readonly sessionId = signal<string | null>(null);
  readonly rows = signal<DeliverableView[]>([]);
  readonly loading = signal(false);
  readonly error = signal<DeliverablesFailure | null>(null);
  readonly hasMore = signal(false);
  readonly nextCursor = signal<string | undefined>(undefined);
  readonly preview = signal<DeliverablePreview | null>(null);
  readonly previewLoading = signal<string | null>(null);
  readonly previewError = signal<DeliverablesFailure | null>(null);

  private readToken = 0;
  private previewToken = 0;

  available(): boolean {
    return can("session.deliverables");
  }

  private readonly attempted = new Set<string>();

  /** Read the active session's first page once. An attempt is remembered even
   * when it failed: a render loop must not retry a failed read (the pane's
   * 重试 calls `refresh`). */
  ensure(sessionId: string): void {
    if (!sessionId || !this.available()) return;
    if (this.loading() && this.sessionId() === sessionId) return;
    // The same session is read once: a success is enough, and a failure waits
    // for 刷新 instead of retrying on every render. A DIFFERENT session always
    // reads, even if it was read earlier in this launch (its rows were cleared
    // when the reader left it).
    if (this.sessionId() === sessionId) {
      if (this.rows().length > 0 && this.error() === null) return;
      if (this.attempted.has(sessionId)) return;
    }
    this.attempted.add(sessionId);
    void this.load(sessionId);
  }

  async refresh(sessionId: string): Promise<void> {
    this.attempted.delete(sessionId);
    await this.load(sessionId);
  }

  async load(sessionId: string): Promise<void> {
    const api = store.identityApi();
    if (!sessionId || !api?.sessionDeliverables) return;
    const token = ++this.readToken;
    const changed = this.sessionId() !== sessionId;
    this.sessionId.set(sessionId);
    if (changed) {
      this.rows.set([]);
      this.hasMore.set(false);
      this.nextCursor.set(undefined);
      this.closePreview();
    }
    this.loading.set(true);
    this.error.set(null);
    try {
      const page = await api.sessionDeliverables(sessionId, { limit: 100 });
      if (token !== this.readToken) return;
      this.rows.set(page.deliverables ?? []);
      this.hasMore.set(page.hasMore === true);
      this.nextCursor.set(page.nextCursor ?? undefined);
    } catch (error) {
      if (token !== this.readToken) return;
      this.rows.set([]);
      this.hasMore.set(false);
      this.nextCursor.set(undefined);
      this.error.set(failureOf(error));
    } finally {
      if (token === this.readToken) this.loading.set(false);
    }
  }

  /** The next manifest page, appended (a long workspace keeps the read
   * bounded). */
  async more(): Promise<void> {
    const api = store.identityApi();
    const sessionId = this.sessionId();
    const after = this.nextCursor();
    if (!sessionId || !after || !api?.sessionDeliverables || this.loading()) return;
    const token = this.readToken;
    this.loading.set(true);
    try {
      const page: DeliverablesView = await api.sessionDeliverables(sessionId, { limit: 100, after });
      if (token !== this.readToken) return;
      this.rows.set([...this.rows(), ...(page.deliverables ?? [])]);
      this.hasMore.set(page.hasMore === true);
      this.nextCursor.set(page.nextCursor ?? undefined);
    } catch (error) {
      if (token !== this.readToken) return;
      this.error.set(failureOf(error));
    } finally {
      if (token === this.readToken) this.loading.set(false);
    }
  }

  /** Preview one row through the session's own manifest. The hash sent is the
   * row's own, so the answer can say whether the bytes still match. */
  async openPreview(row: DeliverableView): Promise<void> {
    const api = store.identityApi();
    const sessionId = this.sessionId();
    if (!sessionId || !api) return;
    if (!api.filePreview) {
      this.previewError.set({ status: null, message: "这个部署没有提供文件预览。" });
      return;
    }
    const token = ++this.previewToken;
    this.previewLoading.set(row.path);
    this.previewError.set(null);
    try {
      const view = await api.filePreview(sessionId, row.path, row.hash);
      if (token !== this.previewToken) return;
      const matches = view.matchesReference === undefined ? null : view.matchesReference;
      this.preview.set({ row, view, matches });
    } catch (error) {
      if (token !== this.previewToken) return;
      this.previewError.set(failureOf(error));
    } finally {
      if (token === this.previewToken) this.previewLoading.set(null);
    }
  }

  closePreview(): void {
    this.previewToken += 1;
    this.preview.set(null);
    this.previewError.set(null);
    this.previewLoading.set(null);
  }

  phase(): DeliverablesPhase {
    if (this.loading() && this.rows().length === 0 && this.error() === null) return "loading";
    const error = this.error();
    if (error) return error.status === 403 ? "permission" : "error";
    if (this.rows().length === 0) return "empty";
    return "success";
  }
}

/** `sha256:3a3a…` shortened for a row; the full hash is the row's own field. */
export const shortDeliverableHash = (hash: string | undefined): string => {
  if (!hash) return "";
  const bare = hash.startsWith("sha256:") ? hash.slice("sha256:".length) : hash;
  return bare.length <= 12 ? hash : `sha256:${bare.slice(0, 10)}…`;
};

/** The size in the reader's words; the bytes are the manifest's own number. */
export const deliverableSize = (size: number): string => {
  if (size >= 1024 * 1024) return `${(size / 1024 / 1024).toFixed(1)} MB`;
  if (size >= 1024) return `${(size / 1024).toFixed(1)} KB`;
  return `${size} B`;
};

export const deliverablesAccess = new DeliverablesAccess();
