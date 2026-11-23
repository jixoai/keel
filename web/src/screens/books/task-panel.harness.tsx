// TUI harness · the task panel's five states and the resource preview —
// change keel-harness-v2, W4.
//
// A harness is needed because a test's module graph is not the app's: the seam
// has to be installed inside the launched app. The scenario arrives in the
// launch parameters (`host.params.scenario`):
//
//   panel-loading     the panel read never settles
//   panel-empty       a session with no task card at all
//   panel-error       the panel read answers 500 (request id carried)
//   panel-permission  the panel read is refused (403 + required key)
//   panel-success     a real tree: root + own child + sub-agent + fork member
//   panel-changed     the same tree, and a reference whose file has changed
//   panel-files-error the manifest read fails beside a readable panel
//   panel-files-empty the manifest answers, but no row is attributed to a card
//   panel-files-paged the manifest answers one page and a cursor to the next
//   panel-no-manifest the transport has no manifest read at all
//
// The success faces of the *run surface entry* are driven by the app's own
// fixture transport (`?fixture=1` / `host.params.fixture`), not here.
import { host, mount, View } from "nsh";
import { setEffectivePermissions } from "../../data/permissions";
import { KeelApiError } from "../../data/http";
import type { IdentityApi } from "../../data/identity-api";
import type { AccountView, PermissionScope } from "../../data/identity";
import type { FilePreviewView, TaskPanelView } from "../../data/contract";
import { store } from "../../data/store";
import { taskPanelAccess } from "../../data/tasks";
import { color } from "../../ui/kit";
import { BooksChrome } from "./shell";
import { TaskPanelBody } from "./task-panel";

const scenario = (): string => String(host.params.scenario ?? "panel-success");
const width = (): number => Number(host.params.width ?? 0);

const HASH_A = "sha256:" + "a".repeat(64);
const HASH_B = "sha256:" + "b".repeat(64);
const SESSION = "8a10c1de-0000-4000-8000-000000000001";
const RUNNING = "8a10c1de-0000-4000-8000-000000000002";
const ORDER = "8a10c1de-0000-4000-8000-000000000003";
const FORK = "8a10c1de-0000-4000-8000-000000000004";
const EMPTY_SESSION = "8a10c1de-0000-4000-8000-000000000005";

const ACCOUNT: AccountView = {
  accountId: "acc-1",
  username: "lina",
  displayName: "李娜",
  kind: "human",
  isSuperAdmin: false,
};

const failure = (): KeelApiError =>
  new KeelApiError(500, "服务暂时没有响应", [], undefined, undefined, {}, "k-panel-1");

const forbidden = (): KeelApiError =>
  new KeelApiError(403, "permission denied", [], undefined, "missing_relation", {
    reason: "missing_relation",
    requiredKey: "session.read",
  });

/** The panel's tree: a session root, one own child, one sub-agent member and a
 * fork member under it. Every card the read carries is drawn; the attachments
 * reference two files, one of which the preview answers as changed. */
const PANEL: TaskPanelView = {
  sessionId: SESSION,
  cards: [
    {
      taskId: "task-captain",
      sessionId: SESSION,
      title: "本周退款原因分析",
      status: "in_progress",
      origin: "session",
      createdSeq: 2,
      updatedSeq: 9,
      createdAt: "2026-11-23T09:00:00Z",
      updatedAt: "2026-11-23T09:12:00Z",
    },
    {
      taskId: "task-summary",
      parentTaskId: "task-captain",
      sessionId: SESSION,
      title: "汇总渠道异常",
      status: "done",
      origin: "tool",
      createdSeq: 6,
      updatedSeq: 18,
      createdAt: "2026-11-23T09:04:00Z",
      updatedAt: "2026-11-23T09:10:00Z",
    },
  ],
  members: [
    {
      taskId: "task-order",
      parentTaskId: "task-captain",
      sessionId: ORDER,
      title: "订单查询",
      status: "done",
      origin: "subagent",
      member: true,
      createdAt: "2026-11-23T09:01:00Z",
      updatedAt: "2026-11-23T09:03:00Z",
    },
    {
      taskId: "task-fork",
      parentTaskId: "task-captain",
      sessionId: FORK,
      title: "退款条款对照",
      status: "open",
      origin: "fork",
      originSeq: 12,
      member: true,
      createdAt: "2026-11-23T09:02:00Z",
      updatedAt: "2026-11-23T09:05:00Z",
    },
  ],
  ancestors: [],
  attachments: [
    { taskId: "task-captain", path: "reports/refund-analysis.md", hash: HASH_A },
    { taskId: "task-captain", path: "reports/refund-report.json", hash: HASH_B },
  ],
  events: [
    { taskId: "task-captain", kind: "task.created", seq: 2, at: "2026-11-23T09:00:00Z", title: "本周退款原因分析", status: "open", origin: "session" },
    { taskId: "task-captain", kind: "task.attached", seq: 4, at: "2026-11-23T09:01:30Z", path: "reports/refund-analysis.md", hash: HASH_A },
    { taskId: "task-captain", kind: "task.note", seq: 5, at: "2026-11-23T09:02:00Z", text: "渠道 B 的退款率翻倍。" },
    { taskId: "task-order", kind: "task.created", seq: 2, at: "2026-11-23T09:03:00Z", title: "订单查询", status: "open" },
    { taskId: "task-fork", kind: "task.created", seq: 1, at: "2026-11-23T09:04:00Z", title: "退款条款对照", origin: "fork", originSeq: 12 },
    { taskId: "task-fork", kind: "task.updated", seq: 3, at: "2026-11-23T09:05:00Z", status: "open" },
    { taskId: "task-summary", kind: "task.created", seq: 6, at: "2026-11-23T09:06:00Z", title: "汇总渠道异常" },
    { taskId: "task-summary", kind: "task.closed", seq: 18, at: "2026-11-23T09:10:00Z", reason: "汇总完成" },
  ],
  limits: { cardLimit: 1000, memberLimit: 200, eventLimit: 500, attachmentLimit: 1000 },
};

const API = (name: string): IdentityApi => {
  const api: IdentityApi = {
    async listAccounts() {
      return { items: [ACCOUNT], limit: 100, hasMore: false };
    },
    async taskPanel(sessionId: string) {
      if (name === "panel-loading") return new Promise(() => {});
      if (name === "panel-error") throw failure();
      if (name === "panel-permission") throw forbidden();
      if (name === "panel-empty") {
        return {
          sessionId,
          cards: [],
          members: [],
          ancestors: [],
          attachments: [],
          events: [],
          limits: { memberLimit: 200, eventLimit: 500 },
        };
      }
      return { ...PANEL, sessionId };
    },
    async filePreview(_sessionId: string, path: string, referenceHash?: string): Promise<FilePreviewView> {
      const hash = name === "panel-changed" && path === "reports/refund-analysis.md" ? HASH_B : referenceHash ?? HASH_A;
      const changed = referenceHash !== undefined && hash !== referenceHash;
      const content =
        path === "reports/refund-by-channel.csv"
          ? "渠道,退款单,占比\nB,2,66.7%\nA,1,33.3%\n"
          : "# 退款原因分析\n\n本周退款 3 单，2 单来自同一渠道（渠道 B）。\n";
      return {
        path,
        hash,
        size: 2048,
        seq: 19,
        offset: 0,
        limit: 200,
        totalLines: 4,
        returnedLines: 4,
        nextOffset: 4,
        truncated: false,
        content,
        referenceHash,
        matchesReference: referenceHash === undefined ? undefined : !changed,
      };
    },
  };
  if (name === "panel-no-manifest") return api;
  api.sessionFiles = async (_sessionId: string, query?: { limit?: number; cursor?: string }) => {
    if (name === "panel-files-error") {
      throw new KeelApiError(500, "文件清单暂时不可用", [], undefined, undefined, {}, "k-files-1");
    }
    if (name === "panel-files-paged") {
      // A two-page manifest: the first answer carries one attributed row and a
      // cursor, the second the row after it. The panel must append, not replace,
      // and must not call the first page complete.
      if (query?.cursor === "reports/page-1.csv") {
        return {
          files: [{ path: "reports/page-2.csv", hash: HASH_B, size: 24, updatedSeq: 23, lastTaskId: "task-summary" }],
          count: 1,
          limit: query?.limit ?? 200,
          truncated: false,
          hasMore: false,
        };
      }
      return {
        files: [{ path: "reports/page-1.csv", hash: HASH_A, size: 12, updatedSeq: 22, lastTaskId: "task-summary" }],
        count: 1,
        limit: query?.limit ?? 200,
        truncated: true,
        hasMore: true,
        nextCursor: "reports/page-1.csv",
      };
    }
    const files: Array<{ path: string; hash?: string; size: number; updatedSeq: number; lastTaskId?: string }> = [
      { path: "src/composer.tsx", size: 1200, updatedSeq: 8 },
      // Listed above as a live attachment: the file group must not repeat it.
      { path: "reports/refund-analysis.md", hash: HASH_A, size: 2048, updatedSeq: 19, lastTaskId: "task-captain" },
      { path: "reports/refund-report.json", hash: HASH_B, size: 180, updatedSeq: 21, lastTaskId: "task-captain" },
    ];
    if (name !== "panel-files-empty") {
      // A manifest-only write by the summary card; no attach event names it.
      files.push({ path: "reports/refund-by-channel.csv", hash: HASH_A, size: 48, updatedSeq: 22, lastTaskId: "task-summary" });
      // A manifest row the fixture cannot hash (the defensive branch): the
      // preview must say 清单当前版本, never 未核对 or 与引用一致.
      files.push({ path: "reports/unhashed.csv", size: 6, updatedSeq: 23, lastTaskId: "task-summary" });
    }
    return { files, count: files.length, limit: query?.limit ?? 200, truncated: false, hasMore: false };
  };
  return api;
};

const ALL_KEYS: Record<string, PermissionScope> = {
  "session.read": "tenant",
  "session.write": "tenant",
};

store.skipBootForTest();
store.setIdentityTransport(API(scenario()));
setEffectivePermissions(scenario() === "panel-permission" ? {} : ALL_KEYS, false);
taskPanelAccess.reset();
taskPanelAccess.ensure(scenario() === "panel-empty" ? EMPTY_SESSION : SESSION);

function Harness() {
  return (
    <View style={{ flex: 1, minHeight: 0, backgroundColor: color.background }}>
      <BooksChrome>
        <View style={{ flex: 1, minHeight: 0, paddingHorizontal: 24, paddingTop: 16 }}>
          <TaskPanelBody sessionId={scenario() === "panel-empty" ? EMPTY_SESSION : SESSION} />
        </View>
      </BooksChrome>
    </View>
  );
}

void width;
void RUNNING;
mount(Harness);
