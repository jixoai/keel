// TUI harness · the super-agent board's five states, the read-only gate and
// the write-failure faces — change keel-collab-superagent, 4.2/4.3.
//
// A harness is needed because a test's module graph is not the app's: the
// transport seam must be installed inside the launched app. The scenario
// arrives in the launch parameters (`host.params.scenario`):
//
//   board-loading   the two reads never settle
//   board-empty     a tenant with no ticket and no open question
//   board-error     the board read answers 500 (request id carried)
//   board-denied    the account holds no `task.read`
//   board-success   one ticket per headline state + a fresh and an expired ask
//   board-readonly  the same board under `task.read` only
//   board-write-error the same board; every write is refused
//
// The success writes (answer/resume/stop) are driven by the app's own fixture
// transport (`?fixture=1` / `host.params.fixture`), not here.
import { host, mount, View } from "nsh";
import { setEffectivePermissions } from "../../data/permissions";
import { KeelApiError } from "../../data/http";
import type { PermissionScope } from "../../data/identity";
import type {
  SuperagentAnswerInput,
  SuperagentAttentionInboxView,
  SuperagentAttentionView,
  SuperagentBoardView,
  SuperagentTicketView,
} from "../../data/contract";
import type { SuperagentApi } from "../../data/contract";
import { store } from "../../data/store";
import { superagentAccess } from "../../data/superagent";
import { color, space } from "../../ui/kit";
import { BooksChrome } from "./shell";
import { SuperagentPage } from "./superagent";

const scenario = (): string => String(host.params.scenario ?? "board-success");

const failure = (): KeelApiError =>
  new KeelApiError(500, "服务暂时没有响应", [], undefined, undefined, {}, "k-super-1");

const forbiddenKey = (key: string): KeelApiError =>
  new KeelApiError(403, "permission denied", [], undefined, "missing_grant", {
    reason: "missing_grant",
    requiredKey: key,
  });

const operatorForbidden = (): KeelApiError =>
  new KeelApiError(403, "superagent: operator authorization is not wired", [], undefined, "forbidden", {
    reason: "missing_relation",
  });

const THREAD = {
  source: "github",
  kind: "issue",
  id: "1042",
  url: "https://github.com/acme/refunds/issues/1042",
};

const MANDATE = {
  requester: { id: "acc-chenmo", login: "chenmo", display: "陈墨", isBot: false },
  source: "github",
  thread: THREAD,
  strength: "write",
  policyVersion: "policy-7",
  acceptedAt: "2026-11-26T08:00:00Z",
  acceptedBy: "orchestrator",
};

const TICKETS: SuperagentTicketView[] = [
  {
    id: "t-wait",
    tenantId: "tenant-1",
    modeId: "superagent",
    title: "渠道 B 退款重复扣款排查",
    status: "blocked",
    request: "用户反馈渠道 B 在 24 小时内出现两次退款扣款；请核对账单流水并给出处理建议。",
    workerBot: "bot-refund",
    thread: THREAD,
    mandate: MANDATE,
    createdAt: "2026-11-26T08:00:00Z",
    updatedAt: "2026-11-26T09:30:00Z",
    assignmentCount: 1,
    activeAssignment: {
      id: "as-1",
      botId: "bot-refund",
      sessionId: "8a10c1de-0000-4000-8000-000000000001",
      status: "active",
      title: "核对退款流水",
      result: { state: "progress", summary: "已核对 3 笔流水，其中 1 笔确实重复。" },
      createdAt: "2026-11-26T08:05:00Z",
      updatedAt: "2026-11-26T09:30:00Z",
    },
    attentionCount: 1,
    openAttentionCount: 1,
  },
  {
    id: "t-run",
    modeId: "superagent",
    title: "准备新客户入驻清单",
    status: "active",
    request: "为新签约的连锁客户准备入驻清单和第一周检查项。",
    workerBot: "bot-butler",
    thread: { source: "manual", kind: "conversation", id: "manual-77" },
    mandate: { ...MANDATE, strength: "comment", thread: { source: "manual", kind: "conversation", id: "manual-77" } },
    createdAt: "2026-11-26T07:30:00Z",
    updatedAt: "2026-11-26T09:20:00Z",
    assignmentCount: 1,
    activeAssignment: {
      id: "as-2",
      botId: "bot-butler",
      sessionId: "8a10c1de-0000-4000-8000-000000000002",
      status: "active",
      title: "整理入驻清单",
      result: { state: "progress", summary: "已完成 4 项中的 2 项。" },
      createdAt: "2026-11-26T07:35:00Z",
      updatedAt: "2026-11-26T09:20:00Z",
    },
    attentionCount: 0,
    openAttentionCount: 0,
  },
  {
    id: "t-done",
    modeId: "superagent",
    title: "补充退款审计日志",
    status: "done",
    request: "在退款流程的关键节点补齐审计日志。",
    workerBot: "bot-knowledge",
    thread: { source: "github", kind: "issue", id: "1039" },
    mandate: { ...MANDATE, strength: "write", thread: { source: "github", kind: "issue", id: "1039" } },
    createdAt: "2026-11-25T08:00:00Z",
    updatedAt: "2026-11-26T04:00:00Z",
    assignmentCount: 2,
    attentionCount: 1,
    openAttentionCount: 0,
  },
  {
    id: "t-fail",
    modeId: "superagent",
    title: "同步渠道对账数据",
    status: "failed",
    request: "把渠道 B 的日终对账文件同步到财务库。",
    workerBot: "bot-order",
    thread: { source: "manual", kind: "conversation", id: "manual-76" },
    mandate: { ...MANDATE, thread: { source: "manual", kind: "conversation", id: "manual-76" } },
    createdAt: "2026-11-26T04:00:00Z",
    updatedAt: "2026-11-26T08:00:00Z",
    assignmentCount: 1,
    activeAssignment: { id: "as-4", botId: "bot-order", status: "failed", error: "对账接口返回 502" },
    attentionCount: 0,
    openAttentionCount: 0,
    pending: [
      {
        id: "fixture:manual-76:edit-1",
        kind: "followup",
        text: "Issue body: 日终对账文件改走手工上传。",
        trusted: true,
        allowDispatch: true,
        attempts: 12,
        lastError: "superagent: this deployment cannot deliver input to a worker",
        failed: true,
        createdAt: "2026-11-26T08:02:00Z",
      },
    ],
    pendingCount: 1,
    failedPendingCount: 1,
  },
  {
    id: "t-expired",
    modeId: "superagent",
    title: "确认发票抬头与合同主体",
    status: "blocked",
    request: "客户要求把发票抬头改为另一家公司，需要确认是否与合同主体一致。",
    workerBot: "bot-support",
    thread: { source: "github", kind: "issue", id: "1038" },
    mandate: { ...MANDATE, strength: "comment", thread: { source: "github", kind: "issue", id: "1038" } },
    createdAt: "2026-11-26T01:00:00Z",
    updatedAt: "2026-11-26T05:00:00Z",
    assignmentCount: 1,
    activeAssignment: { id: "as-5", botId: "bot-support", status: "active", title: "核对合同主体" },
    attentionCount: 1,
    openAttentionCount: 1,
  },
];

const ATTENTIONS: SuperagentAttentionView[] = [
  {
    id: "a-fresh",
    workItemId: "t-wait",
    assignmentId: "as-1",
    kind: "decision",
    prompt: "渠道 B 的重复扣款是否按「先退款、后追偿」处理？",
    options: [
      { id: "refund-first", label: "先退款，再向渠道追偿", description: "客户优先；追偿由财务发起。" },
      { id: "verify-first", label: "先核对渠道流水", description: "确认是重复扣款后再退。" },
    ],
    allowFreeText: true,
    route: "channel",
    channel: "1042",
    status: "open",
    createdAt: "2026-11-26T09:10:00Z",
    deadline: "2099-11-26T18:00:00Z",
    ticketTitle: "渠道 B 退款重复扣款排查",
    thread: THREAD,
  },
  {
    id: "a-expired",
    workItemId: "t-expired",
    assignmentId: "as-5",
    kind: "clarify",
    prompt: "客户希望发票抬头写「上海某某科技有限公司」，请确认是否与合同主体一致。",
    allowFreeText: true,
    route: "channel",
    channel: "1038",
    status: "open",
    createdAt: "2026-11-26T05:00:00Z",
    deadline: "2026-11-26T06:00:00Z",
    ticketTitle: "确认发票抬头与合同主体",
    thread: { source: "github", kind: "issue", id: "1038" },
  },
];

const board = (): SuperagentBoardView => ({
  tenantId: "tenant-1",
  modeId: "superagent",
  tickets: TICKETS,
});

const inbox = (): SuperagentAttentionInboxView => ({ tenantId: "tenant-1", items: ATTENTIONS });

const API = (name: string): SuperagentApi => ({
  async superagentBoard() {
    if (name === "board-loading") return new Promise<SuperagentBoardView>(() => {});
    if (name === "board-error") throw failure();
    if (name === "board-unwired") {
      throw new KeelApiError(
        503,
        "the super-agent board is not wired in this deployment",
        [],
        undefined,
        undefined,
        {},
        "k-super-503",
      );
    }
    if (name === "board-empty") return { tenantId: "tenant-1", modeId: "superagent", tickets: [] };
    if (name === "board-empty-inbox-error") return { tenantId: "tenant-1", modeId: "superagent", tickets: [] };
    if (name === "board-transitional") {
      return {
        tenantId: "tenant-1",
        modeId: "superagent",
        tickets: [
          { id: "t-open", modeId: "superagent", title: "等待派工", status: "open" },
          { id: "t-cancelling", modeId: "superagent", title: "正在停止", status: "cancelling" },
        ],
      };
    }
    return board();
  },
  async superagentAttention() {
    if (name === "board-loading") return new Promise<SuperagentAttentionInboxView>(() => {});
    if (name === "board-error") throw failure();
    if (name === "board-empty-inbox-error") throw failure();
    if (name === "board-empty") return { tenantId: "tenant-1", items: [] };
    if (name === "board-transitional") return { tenantId: "tenant-1", items: [] };
    return inbox();
  },
  async answerSuperagentAttention(_attentionId: string, _answer: SuperagentAnswerInput) {
    if (name === "board-write-error") throw forbiddenKey("task.write");
    throw new KeelApiError(409, "这条问题已经答复。", [], undefined, "attention_closed");
  },
  async resumeSuperagentTicket(ticketId: string) {
    if (name === "board-write-error") throw operatorForbidden();
    const ticket = TICKETS.find((row) => row.id === ticketId);
    if (!ticket) throw new KeelApiError(404, "ticket not found");
    return { ...ticket, status: "active" };
  },
  async stopSuperagentTicket(_ticketId: string) {
    if (name === "board-write-error") throw new KeelApiError(500, "停止传播失败", [], undefined, undefined, {}, "k-stop-1");
    return;
  },
  async retrySuperagentPending(ticketId: string, pendingId: string) {
    if (name === "board-write-error") throw operatorForbidden();
    const ticket = TICKETS.find((row) => row.id === ticketId);
    if (!ticket) throw new KeelApiError(404, "ticket not found");
    const pending = (ticket.pending ?? []).map((entry) =>
      entry.id === pendingId ? { ...entry, failed: false, attempts: (entry.attempts ?? 0) + 1 } : entry,
    );
    return { ...ticket, pending, failedPendingCount: pending.filter((entry) => entry.failed === true).length };
  },
  async dismissSuperagentPending(ticketId: string, pendingId: string) {
    if (name === "board-write-error") throw operatorForbidden();
    const ticket = TICKETS.find((row) => row.id === ticketId);
    if (!ticket) throw new KeelApiError(404, "ticket not found");
    const pending = (ticket.pending ?? []).filter((entry) => entry.id !== pendingId);
    return { ...ticket, pending, pendingCount: pending.length, failedPendingCount: 0 };
  },
});

const PERMISSIONS: Record<string, PermissionScope> = {
  "task.read": "team",
  "task.write": "team",
};

const granted = (): Record<string, PermissionScope> => {
  const name = scenario();
  if (name === "board-denied") return {};
  if (name === "board-readonly") return { "task.read": "team" };
  return PERMISSIONS;
};

store.skipBootForTest();
store.setSuperagentTransport(API(scenario()));
setEffectivePermissions(granted(), false);
superagentAccess.reset();
store.openChapter("superagent");

function Harness() {
  return (
    <View style={{ flex: 1, minHeight: 0, backgroundColor: color.background }}>
      <BooksChrome>
        <View style={{ flex: 1, minHeight: 0, padding: space.lg }}>
          <SuperagentPage />
        </View>
      </BooksChrome>
    </View>
  );
}

mount(Harness);
