// The development fixture's identity data — change keel-bot-identity-access,
// A4.4a.
//
// A scripted `IdentityApi` for `?fixture=1` (and the TUI/desktop test hosts):
// the design board's sample deployment — twelve accounts, five permission
// groups, a week of audit rows — with real in-memory transitions, so the books
// shell, the matrix's diff and the ledger render from the same code paths a
// live deployment drives. It is a development transport, never a product
// fallback: nothing selects it unless the launch asked for it (`fixture=true`),
// and a real `keel serve` is always `KeelHttp`.
import { KeelApiError } from "./http";
import { CAPABILITIES } from "./permission-catalog";
import type { IdentityApi } from "./identity-api";
import type {
  FilePreviewView,
  TaskPanelAttachmentView,
  TaskPanelCardView,
  TaskPanelEventView,
  TaskPanelView,
  TeamTaskView,
} from "./contract";
import type {
  ProfileView,
  RoleView,
  RunStartView,
  SessionCancelView,
  SessionEventView,
  SubagentView,
} from "./bots-wire";
import type {
  AccountView,
  AuditEventView,
  AuditKind,
  AuditQuery,
  BotKeyView,
  BotView,
  CreateAccountInput,
  CreateBotInput,
  CreateTeamBotInput,
  IdentityPage,
  ModelRef,
  PermissionGroupDetail,
  PermissionGroupSummary,
  PermissionScope,
  RosterEntry,
  ShareInput,
  ShareResourceKind,
  ShareView,
  ShareWriteResult,
  TeamBotView,
  UpdateBotInput,
  UpdateTeamBotInput,
  WhoAmI,
} from "./identity";

const TENANT_ID = "8f14e45f-ceea-467f-a1b2-6c1d2f4e7a90";

/** A stable id per fixture entity (a UUID-shaped token, the routes' own form). */
const id = (seed: string): string => {
  let hash = 0;
  for (let index = 0; index < seed.length; index += 1) hash = (hash * 31 + seed.charCodeAt(index)) >>> 0;
  const hex = hash.toString(16).padStart(8, "0");
  return `${hex}-1111-4111-8111-${hex}${hex.slice(0, 4)}`;
};

const nowIso = (): string => new Date().toISOString();

const daysAgoIso = (days: number): string => new Date(Date.now() - days * 86_400_000).toISOString();

/** Today at a wall-clock time (the demo's events read like the board's). */
const todayAt = (hours: number, minutes: number, seconds: number): string => {
  const now = new Date();
  return new Date(now.getFullYear(), now.getMonth(), now.getDate(), hours, minutes, seconds).toISOString();
};

const pageOf = <T>(items: T[], limit: number): IdentityPage<T> => ({
  items: items.slice(0, limit),
  limit,
  hasMore: items.length > limit,
});

const account = (
  key: string,
  username: string,
  displayName: string,
  kind: string,
  opts: { superAdmin?: boolean; disabled?: boolean; createdDaysAgo?: number } = {},
): AccountView => ({
  accountId: id(key),
  username,
  displayName,
  kind,
  isSuperAdmin: opts.superAdmin === true,
  disabledAt: opts.disabled ? daysAgoIso(4) : undefined,
  createdAt: daysAgoIso(opts.createdDaysAgo ?? 20),
  updatedAt: daysAgoIso(1),
});

interface FixtureGroup {
  summary: PermissionGroupSummary;
  members: Array<{ accountId: string; addedBy?: string; createdAt?: string }>;
  grants: Array<{ key: string; scope: PermissionScope }>;
}

const grant = (key: string, scope: PermissionScope) => ({ key, scope });

const bot = (
  key: string,
  name: string,
  profileKey: string,
  accountKey: string,
  opts: { instruction: string; permissions: string[]; tools?: string[]; archived?: boolean; days?: number; sharedCount?: number },
): BotView => ({
  botId: id(`bot-${key}`),
  name,
  profileId: id(`profile-${profileKey}`),
  accountId: id(accountKey),
  modelKind: "text",
  defaultModel: { providerId: "s2api", modelId: "deepseek-v4.1-flash" },
  harness: {
    tools: opts.tools ?? [],
    context: { instruction: opts.instruction, permissions: opts.permissions },
  },
  archived: opts.archived === true,
  createdAt: daysAgoIso(opts.days ?? 14),
  updatedAt: daysAgoIso(1),
  sharedCount: opts.sharedCount ?? 0,
});

/** The fixture run's stable session ids. The same ids name the member task
 * cards, the sub-agent records and the member ledgers, so the run page's
 * projection joins on real keys. */
const RUN_ID = id("run-captain");
const ORDER_SESSION = id("run-order");
const REFUND_SESSION = id("run-refund");
const KNOWLEDGE_SESSION = id("run-knowledge");
const AFTER_SESSION = id("run-aftersales");

const todayMs = (hours: number, minutes: number, seconds: number): number => Date.parse(todayAt(hours, minutes, seconds));

/** A timestamp relative to the launch, so the sample run is always coherent
 * (a fixed 14:32 can be in the future when the test runs in the morning). */
const agoMs = (minutes: number, seconds = 0): number => Date.now() - minutes * 60_000 - seconds * 1000;
const agoIso = (minutes: number, seconds = 0): string => new Date(agoMs(minutes, seconds)).toISOString();

const runEvent = (seq: number, kind: string, payload: Record<string, unknown>): SessionEventView => ({
  seq,
  kind,
  payload,
});

const CAPTAIN_EVENTS: SessionEventView[] = [
  runEvent(1, "user.message", {
    text: "本周退款原因分析：按渠道拆开，找出异常升高的两类。",
    timestamp: agoMs(4, 12),
  }),
  runEvent(2, "assistant.message", {
    content: [
      { type: "text", text: "我先查一遍本周的退款订单。" },
      { type: "toolCall", id: "call-orders", name: "search_orders", arguments: { range: "7d", status: "refunded" } },
    ],
    usage: { inputTokens: 1200, outputTokens: 400 },
    timestamp: agoMs(4, 0),
  }),
  runEvent(3, "tool.result", {
    toolCallId: "call-orders",
    toolName: "search_orders",
    content: [{ type: "text", text: "找到 3 个订单；其中 2 个来自同一渠道。" }],
    isError: false,
    durationMs: 800,
    timestamp: agoMs(3, 59),
  }),
  runEvent(4, "assistant.message", {
    content: [{ type: "toolCall", id: "call-aftersales", name: "invoke_bot", arguments: { botId: id("bot-aftersales") } }],
    timestamp: agoMs(0, 54),
  }),
  runEvent(5, "tool.result", {
    toolCallId: "call-aftersales",
    toolName: "invoke_bot",
    content: [{ type: "text", text: "403 denied: missing bot.invoke" }],
    isError: true,
    durationMs: 200,
    timestamp: agoMs(0, 54),
  }),
  runEvent(6, "run.finished", {
    tokensIn: 9000,
    tokensOut: 3400,
    durationMs: 252_000,
    stopReason: "completed",
    provider: "s2api",
    model: "deepseek-v4.1-flash",
    timestamp: agoMs(0, 0),
  }),
];

const memberEvents = (
  name: string,
  tokensIn: number,
  tokensOut: number,
  durationMs: number,
  tool: string,
  startedAt: number,
): SessionEventView[] => [
  runEvent(1, "user.message", { text: `${name} 的成员任务`, timestamp: startedAt }),
  runEvent(2, "assistant.message", {
    content: [{ type: "toolCall", id: `${name}-call`, name: tool, arguments: {} }],
    usage: { inputTokens: Math.round(tokensIn / 2), outputTokens: Math.round(tokensOut / 2) },
    timestamp: startedAt + 2000,
  }),
  runEvent(3, "tool.result", {
    toolCallId: `${name}-call`,
    toolName: tool,
    content: [{ type: "text", text: "完成" }],
    isError: false,
    durationMs: 600,
    timestamp: startedAt + 2600,
  }),
  runEvent(4, "run.finished", {
    tokensIn,
    tokensOut,
    durationMs,
    stopReason: "completed",
    provider: "s2api",
    model: "deepseek-v4.1-flash",
    timestamp: startedAt + durationMs,
  }),
];

const MEMBER_EVENTS: Record<string, SessionEventView[]> = {
  [ORDER_SESSION]: memberEvents("订单查询", 8400, 1500, 66_000, "search_orders", agoMs(4, 0)),
  [REFUND_SESSION]: memberEvents("退款专员", 2600, 600, 38_000, "refund_policy", agoMs(3, 0)),
  [KNOWLEDGE_SESSION]: memberEvents("知识库", 7200, 1900, 138_000, "search_docs", agoMs(3, 50)),
};

/** The fixture run's task panel data. The captain's own card is the session's
 * initial card (origin "session"); the member cards are the sub-agent cards the
 * registry already names; one fork card carries origin=fork and the originSeq of
 * the position it branched from. The seq values are the fixture's ledger order,
 * so the panel's stream and the tree's creation order are both stable. */
const CAPTAIN_CARD_ID = "task-captain";
const CAPTAIN_TASKS: TaskPanelCardView[] = [
  {
    taskId: CAPTAIN_CARD_ID,
    sessionId: RUN_ID,
    title: "本周退款原因分析",
    status: "in_progress",
    origin: "session",
    createdSeq: 2,
    updatedSeq: 9,
    createdAt: agoIso(4, 12),
    updatedAt: agoIso(0, 54),
  },
  {
    taskId: "task-captain-summary",
    parentTaskId: CAPTAIN_CARD_ID,
    sessionId: RUN_ID,
    title: "汇总渠道异常",
    status: "done",
    origin: "tool",
    createdSeq: 6,
    updatedSeq: 18,
    closedSeq: 18,
    createdAt: agoIso(3, 40),
    updatedAt: agoIso(1, 20),
  },
];

const FORK_CARD_ID = "task-refund-fork";
const FORK_ORIGIN_SEQ = 12;

const REFUND_REPORT_HASH = "sha256:" + "3a".repeat(32);
const REFUND_JSON_HASH = "sha256:" + "7c".repeat(32);
const REFUND_CHANNEL_HASH = "sha256:" + "5e".repeat(32);

/** The panel's live resource references point at files the fixture manifest
 * holds, so the preview route resolves them (and a reference whose bytes no
 * longer match says so instead of pretending). */
const FIXTURE_FILES: Record<string, { hash: string; seq: number; content: string }> = {
  "reports/refund-analysis.md": {
    hash: REFUND_REPORT_HASH,
    seq: 19,
    content:
      "# 退款原因分析\n\n本周退款 3 单，2 单来自同一渠道（渠道 B）。\n\n1. 渠道 B 的退款率 4.8%，是上周的两倍。\n2. 售后升级被拒：缺 bot.invoke，需人工确认。\n",
  },
  "reports/refund-report.json": {
    hash: REFUND_JSON_HASH,
    seq: 21,
    content: JSON.stringify(
      { range: "7d", refunded: 3, channels: [{ id: "B", refunds: 2 }, { id: "A", refunds: 1 }] },
      null,
      2,
    ),
  },
  // A manifest-only write: the summary card produced it through the file
  // tools, so the task log carries no attach event for it. The panel's file
  // attribution group is the only place it is named.
  "reports/refund-by-channel.csv": {
    hash: REFUND_CHANNEL_HASH,
    seq: 22,
    content: "渠道,退款单,占比\nB,2,66.7%\nA,1,33.3%\n",
  },
};

/** The manifest hash of a fixture path, or undefined when the fixture holds no
 * bytes for it. The development transport stamps this same value on the file
 * row, so a card's reference and the manifest agree exactly like a live
 * deployment's. */
export const fixtureFileHash = (path: string): string | undefined => FIXTURE_FILES[path]?.hash;

/** One member card in the panel's wire shape. The title/status/origin come from
 * the task row; the seq values are the card's own ledger positions. */
const taskPanelCard = (card: TeamTaskView, extra: { originSeq?: number; origin?: string; member?: boolean } = {}): TaskPanelCardView => ({
  taskId: card.taskId,
  parentTaskId: card.parentTaskId,
  sessionId: card.sessionId,
  title: card.title,
  status: card.status,
  origin: extra.origin ?? card.origin,
  originSeq: extra.originSeq,
  createdSeq: undefined,
  updatedSeq: undefined,
  createdAt: card.createdAt,
  updatedAt: card.updatedAt,
  member: extra.member === true ? true : undefined,
});

/** The event stream of one fixture card. The kinds and fields are the six the
 * task log defines; the fork card's stream starts with the task.created that
 * carries origin=fork and the originSeq.
 *
 * `seq` is assigned from the caller's cursor (`next`), so the fixture's stream
 * is one monotonically ordered ledger exactly like the materialized
 * `task_events` rows the live route reads: seq values never repeat and never
 * go backwards. */
const taskPanelEventsFor = (
  card: TaskPanelCardView,
  parentCardId: string,
  next: () => number,
): TaskPanelEventView[] => {
  const base = card.createdAt ? Date.parse(card.createdAt) : Date.now();
  const iso = (offsetMs: number) => new Date(base + offsetMs).toISOString();
  const created: TaskPanelEventView = {
    taskId: card.taskId,
    kind: "task.created",
    seq: next(),
    at: iso(0),
    title: card.title,
    status: "open",
    origin: card.origin,
    parentTaskId: card.parentTaskId ?? (card.taskId === CAPTAIN_CARD_ID ? undefined : parentCardId),
  };
  if (card.origin === "fork") created.originSeq = card.originSeq ?? FORK_ORIGIN_SEQ;
  const updated: TaskPanelEventView = {
    taskId: card.taskId,
    kind: "task.updated",
    seq: next(),
    at: iso(60_000),
    status: card.status,
  };
  const events: TaskPanelEventView[] = [created, updated];
  if (card.taskId === CAPTAIN_CARD_ID) {
    events.push(
      {
        taskId: card.taskId,
        kind: "task.attached",
        seq: next(),
        at: iso(90_000),
        path: "reports/refund-analysis.md",
        hash: REFUND_REPORT_HASH,
      },
      {
        taskId: card.taskId,
        kind: "task.note",
        seq: next(),
        at: iso(120_000),
        text: "渠道 B 的退款率翻倍，先看它的退款条款。",
      },
    );
  }
  if (card.taskId === "task-refund") {
    events.push({
      taskId: card.taskId,
      kind: "task.note",
      seq: next(),
      at: iso(90_000),
      text: "退款材料已备齐，等待人工确认。",
    });
  }
  return events;
};

/** The current user: 李娜, a team administrator — enough to see every chapter,
 * and a `bot.invoke` ceiling of 本团队 so the matrix's 全部/整个租户 options are
 * visibly disabled with their reason. */
const CURRENT_ACCOUNT: AccountView = account("lina", "lina", "李娜", "human", { createdDaysAgo: 19 });

/** The fixture's own ids, exported so the session fixture can attribute its
 * sample sessions to the same accounts the share dialogs list. */
export const fixtureAccountId = (key: string): string => id(key);
export const fixtureCurrentAccountId: string = CURRENT_ACCOUNT.accountId;
/** The fixture run's stable session ids, exported so the development transport
 * can key its run-scoped reads (the file manifest's attribution) to them. */
export const fixtureRunSessionId: string = RUN_ID;
export const fixtureForkSessionId: string = REFUND_SESSION;

const CURRENT_PERMISSIONS: Record<string, PermissionScope> = {
  "session.read": "team",
  "session.write": "team",
  "session.delete": "team",
  "session.share": "team",
  "bot.read": "tenant",
  "bot.create": "tenant",
  "bot.manage": "tenant",
  "bot.bind": "tenant",
  "bot.invoke": "team",
  "team_bot.read": "tenant",
  "team_bot.manage": "tenant",
  "account.read": "tenant",
  "account.manage": "tenant",
  "account.credential.reset": "tenant",
  "account.permission.grant": "tenant",
  "permission.read": "tenant",
  "permission.manage": "tenant",
  "audit.read": "tenant",
  "secret.read.metadata": "tenant",
  "model.read": "tenant",
  "skill.read": "tenant",
  "plugin.read": "tenant",
  "mcp.read": "tenant",
  "notification.read": "tenant",
  "trigger.read": "tenant",
};

export class FixtureIdentity implements IdentityApi {
  private readonly roles: RoleView[] = [
    { roleId: id("role-butler"), name: "管家", instruction: "听懂一句话需求，组建团队、配权限、跑起来，并让人看懂你做了什么。" },
    { roleId: id("role-support"), name: "客服专员", instruction: "客服一线：查订单、拟回复、升级人工。" },
    { roleId: id("role-order"), name: "订单专家", instruction: "查询订单与物流，给出可核对的事实。" },
    { roleId: id("role-refund"), name: "退款专员", instruction: "核对退款条件，准备退款材料。" },
    { roleId: id("role-knowledge"), name: "知识库", instruction: "在内部文档里找答案，引用来源。" },
    { roleId: id("role-aftersales"), name: "售后升级", instruction: "把无法自行解决的问题整理后升级给人工。" },
    { roleId: id("role-finance"), name: "财务助手", instruction: "读取账目与发票信息。" },
  ];

  private readonly profiles: ProfileView[] = [
    { profileId: id("profile-butler"), accountId: id("butler"), roleId: id("role-butler") },
    { profileId: id("profile-support"), accountId: id("lina"), roleId: id("role-support") },
    { profileId: id("profile-order"), accountId: id("lina"), roleId: id("role-order") },
    { profileId: id("profile-refund"), accountId: id("lina"), roleId: id("role-refund") },
    { profileId: id("profile-knowledge"), accountId: id("wangqiang"), roleId: id("role-knowledge") },
    { profileId: id("profile-aftersales"), accountId: id("wangqiang"), roleId: id("role-aftersales") },
    { profileId: id("profile-finance"), accountId: id("finance"), roleId: id("role-finance") },
  ];

  private readonly accounts: AccountView[] = [
    CURRENT_ACCOUNT,
    account("wangqiang", "wangqiang", "王强", "human", { superAdmin: true, createdDaysAgo: 21 }),
    account("chenmo", "chenmo", "陈墨", "human", { createdDaysAgo: 6 }),
    account("zhaoym", "zhaoym", "赵一鸣", "human", { createdDaysAgo: 15 }),
    account("wutong", "wutong", "吴桐", "human", { disabled: true, createdDaysAgo: 12 }),
    account("zhouyu", "zhouyu", "周雨", "human", { createdDaysAgo: 9 }),
    account("xuning", "xuning", "许宁", "human", { createdDaysAgo: 3 }),
    account("zhenghao", "zhenghao", "郑好", "human", { createdDaysAgo: 8 }),
    account("linke", "linke", "林可", "human", { createdDaysAgo: 5 }),
    account("butler", "butler", "管家", "bot", { createdDaysAgo: 21 }),
    account("finance", "finance-bot", "财务助手", "bot", { createdDaysAgo: 30 }),
  ];

  private readonly groups: FixtureGroup[] = [
    {
      summary: { groupId: id("group-super"), name: "超级管理员", builtin: true, updatedAt: daysAgoIso(21) },
      members: [{ accountId: id("wangqiang") }],
      // The built-in super-admin group is the whole catalogue at 全部 (the
      // server seeds it that way): a short list here would understate it.
      grants: CAPABILITIES.map((capability) => grant(capability.key, "all")),
    },
    {
      summary: { groupId: id("group-tenant-admin"), name: "租户管理员", builtin: true, updatedAt: daysAgoIso(6) },
      members: [{ accountId: id("lina") }, { accountId: id("xuning") }, { accountId: id("zhaoym") }],
      grants: [
        grant("account.read", "tenant"),
        grant("account.manage", "tenant"),
        grant("permission.read", "tenant"),
        grant("permission.manage", "tenant"),
        grant("bot.read", "tenant"),
        grant("bot.create", "tenant"),
        grant("team_bot.read", "tenant"),
        grant("team_bot.manage", "tenant"),
        grant("audit.read", "tenant"),
      ],
    },
    {
      summary: { groupId: id("group-support"), name: "客服组", builtin: false, updatedAt: todayAt(14, 12, 4) },
      members: [
        { accountId: id("lina") },
        { accountId: id("chenmo") },
        { accountId: id("wutong") },
        { accountId: id("zhouyu") },
        { accountId: id("zhenghao") },
        { accountId: id("linke") },
        { accountId: id("butler") },
        { accountId: id("finance") },
      ],
      grants: [
        grant("session.read", "team"),
        grant("session.write", "team"),
        grant("bot.read", "tenant"),
        grant("bot.invoke", "team"),
        grant("account.read", "tenant"),
        grant("audit.read", "tenant"),
      ],
    },
    {
      summary: { groupId: id("group-bot-ops"), name: "Bot 运营", builtin: false, updatedAt: daysAgoIso(2) },
      members: [
        { accountId: id("lina") },
        { accountId: id("butler") },
      ],
      grants: [
        grant("bot.read", "tenant"),
        grant("bot.create", "tenant"),
        grant("bot.manage", "tenant"),
        grant("bot.bind", "tenant"),
        grant("bot.invoke", "tenant"),
        grant("session.read", "tenant"),
        grant("session.write", "tenant"),
        grant("team_bot.read", "tenant"),
        grant("model.read", "tenant"),
      ],
    },
    {
      summary: { groupId: id("group-auditor"), name: "审计员", builtin: true, updatedAt: daysAgoIso(10) },
      members: [{ accountId: id("zhaoym") }, { accountId: id("wangqiang") }],
      grants: [
        grant("audit.read", "tenant"),
        grant("account.read", "tenant"),
        grant("permission.read", "tenant"),
        grant("session.read", "tenant"),
        grant("bot.read", "tenant"),
        grant("team_bot.read", "tenant"),
      ],
    },
  ];

  private readonly bots: BotView[] = [
    bot("butler", "管家", "butler", "butler", {
      instruction: "听懂一句话需求，组建团队、配权限、跑起来，并让人看懂你做了什么。",
      permissions: [
        "session.read",
        "session.write",
        "bot.read",
        "bot.create",
        "bot.manage",
        "bot.bind",
        "bot.invoke",
        "team_bot.read",
        "team_bot.manage",
        "account.read",
        "permission.read",
        "permission.manage",
        "model.read",
        "audit.read",
      ],
      days: 21,
    }),
    bot("support", "客服一号", "support", "lina", {
      instruction: "客服一线：查订单、拟回复、升级人工。",
      permissions: [
        "session.read",
        "session.write",
        "session.share",
        "bot.read",
        "bot.invoke",
        "team_bot.read",
        "account.read",
        "secret.read.metadata",
      ],
      tools: ["search_orders", "get_order", "send_reply"],
      days: 14,
      sharedCount: 1,
    }),
    bot("order", "订单查询", "order", "lina", {
      instruction: "查询订单与物流，给出可核对的事实。",
      permissions: ["session.read", "session.write", "bot.read", "team_bot.read"],
      tools: ["search_orders", "get_order"],
      days: 11,
    }),
    bot("refund", "退款专员", "refund", "lina", {
      instruction: "核对退款条件，准备退款材料；金额超过阈值升级人工。",
      permissions: ["session.read", "session.write", "bot.read"],
      tools: ["refund_policy", "prepare_refund"],
      days: 10,
    }),
    bot("knowledge", "知识库", "knowledge", "wangqiang", {
      instruction: "在内部文档里找答案，引用来源。",
      permissions: ["session.read", "bot.read"],
      tools: ["search_docs"],
      days: 9,
    }),
    bot("aftersales", "售后升级", "aftersales", "wangqiang", {
      instruction: "把无法自行解决的问题整理后升级给人工。",
      permissions: ["session.read", "bot.read"],
      tools: ["create_ticket"],
      days: 8,
    }),
    bot("finance", "财务助手", "finance", "finance", {
      instruction: "读取账目与发票信息。",
      permissions: ["session.read", "secret.read.metadata", "secret.read.value"],
      archived: true,
      days: 30,
    }),
  ];

  private readonly botKeys: BotKeyView[] = [
    {
      keyId: id("key-support-prod"),
      prefix: "kb_sup",
      accountId: id("lina"),
      botId: id("bot-support"),
      label: "生产 Webhook",
      createdAt: todayAt(9, 18, 41),
      lastUsedAt: todayAt(14, 32, 18),
    },
    {
      keyId: id("key-support-old"),
      prefix: "kb_old",
      accountId: id("lina"),
      botId: id("bot-support"),
      label: "旧版触发器",
      createdAt: daysAgoIso(12),
      revokedAt: daysAgoIso(3),
      lastUsedAt: daysAgoIso(4),
    },
  ];
  private issuedPlaintext: string | null = null;

  /** In-memory relation tuples for the share dialogs. The keys are
   * `<kind>:<resourceId>`, the values the viewer/editor tuples. Two seeds keep
   * the success faces real: 客服一号 is shared with 陈墨, 客服团队 with 客服组. */
  private readonly shareTuples = new Map<string, ShareView[]>([
    [
      `bot:${id("bot-support")}`,
      [{ subjectType: "account", subjectId: id("chenmo"), relation: "viewer", displayName: "陈墨", username: "chenmo" }],
    ],
    [
      `team_bot:${id("team-support")}`,
      [{ subjectType: "group", subjectId: id("group-support"), relation: "viewer", displayName: "客服组" }],
    ],
  ]);

  private readonly teams: TeamBotView[] = [
    {
      teamBotId: id("team-support"),
      name: "客服团队",
      mission:
        "在 4 小时内解决客户问题。先查订单与物流；无法解决时升级给人工，并附上订单号、已尝试的步骤和客户原话。",
      roster: [
        { botId: id("bot-support"), role: "captain" },
        { botId: id("bot-order"), role: "member" },
        { botId: id("bot-refund"), role: "member" },
        { botId: id("bot-knowledge"), role: "member" },
        { botId: id("bot-aftersales"), role: "member" },
      ],
      revision: 3,
      hash: "fixture-team-support-v3",
      archived: false,
      createdAt: daysAgoIso(12),
      updatedAt: daysAgoIso(1),
      ownerAccountId: id("lina"),
      sharedCount: 1,
    },
    {
      teamBotId: id("team-content"),
      name: "内容运营",
      mission: "每周产出一篇客户故事，事实必须来自订单与客服记录。",
      roster: [{ botId: id("bot-knowledge"), role: "captain" }],
      revision: 1,
      hash: "fixture-team-content-v1",
      archived: false,
      createdAt: daysAgoIso(6),
      updatedAt: daysAgoIso(2),
      ownerAccountId: id("lina"),
      sharedCount: 0,
    },
    {
      teamBotId: id("team-finance"),
      name: "财务对账",
      mission: "每月核对发票与账目；发现差异时列出订单号与金额，交给人工确认。",
      roster: [{ botId: id("bot-finance"), role: "captain" }],
      revision: 1,
      hash: "fixture-team-finance-v1",
      archived: false,
      createdAt: daysAgoIso(20),
      updatedAt: daysAgoIso(8),
      ownerAccountId: id("finance"),
      sharedCount: 0,
    },
  ];

  private readonly tasks: TeamTaskView[] = [
    {
      taskId: "task-order",
      sessionId: ORDER_SESSION,
      title: "订单查询",
      status: "done",
      origin: "subagent",
      createdAt: agoIso(4, 0),
      updatedAt: agoIso(3, 0),
    },
    {
      taskId: "task-refund",
      parentTaskId: "task-order",
      sessionId: REFUND_SESSION,
      title: "退款专员",
      status: "done",
      origin: "subagent",
      createdAt: agoIso(3, 0),
      updatedAt: agoIso(2, 30),
    },
    {
      // A fork whose origin card is the captain's card: the projection links
      // it to the captain session as a member (its row lives in the refund
      // session's ledger), and the fork's own session names the captain card
      // as an ancestor.
      taskId: FORK_CARD_ID,
      parentTaskId: CAPTAIN_CARD_ID,
      sessionId: REFUND_SESSION,
      title: "退款条款对照",
      status: "open",
      origin: "fork",
      createdAt: agoIso(2, 40),
      updatedAt: agoIso(1, 10),
    },
    {
      taskId: "task-knowledge",
      sessionId: KNOWLEDGE_SESSION,
      title: "知识库",
      status: "done",
      origin: "subagent",
      createdAt: agoIso(3, 50),
      updatedAt: agoIso(1, 50),
    },
    {
      taskId: "task-aftersales",
      sessionId: AFTER_SESSION,
      title: "售后升级",
      status: "blocked",
      origin: "subagent",
      createdAt: agoIso(0, 54),
      updatedAt: agoIso(0, 54),
    },
  ];

  private readonly subagents: SubagentView[] = [
    {
      id: `sub-${ORDER_SESSION}`,
      parentSessionId: RUN_ID,
      sessionId: ORDER_SESSION,
      task: "查询本周退款订单",
      agent: "订单查询",
      status: "completed",
      origin: "spawn",
      startedAt: agoIso(4, 0),
      endedAt: agoIso(3, 0),
      summary: "找到 3 个退款订单，2 个来自同一渠道。",
      usage: { turns: 2, inputTokens: 8400, outputTokens: 1500, durationMs: 66_000 },
    },
    {
      id: `sub-${REFUND_SESSION}`,
      parentSessionId: ORDER_SESSION,
      sessionId: REFUND_SESSION,
      task: "核对退款条件",
      agent: "退款专员",
      status: "completed",
      origin: "spawn",
      startedAt: agoIso(3, 0),
      endedAt: agoIso(2, 30),
      summary: "退款材料已备齐，等待人工确认。",
      usage: { turns: 1, inputTokens: 2600, outputTokens: 600, durationMs: 38_000 },
    },
    {
      id: `sub-${KNOWLEDGE_SESSION}`,
      parentSessionId: RUN_ID,
      sessionId: KNOWLEDGE_SESSION,
      task: "查退款政策",
      agent: "知识库",
      status: "completed",
      origin: "spawn",
      startedAt: agoIso(3, 50),
      endedAt: agoIso(1, 50),
      summary: "引用退款政策第 3 节。",
      usage: { turns: 2, inputTokens: 7200, outputTokens: 1900, durationMs: 138_000 },
    },
    {
      id: `sub-${AFTER_SESSION}`,
      parentSessionId: RUN_ID,
      sessionId: AFTER_SESSION,
      task: "升级给人工",
      agent: "售后升级",
      status: "failed",
      origin: "spawn",
      startedAt: agoIso(0, 54),
      endedAt: agoIso(0, 54),
      error: "调用被拒：缺「调用 Bot」能力（bot.invoke）。",
    },
  ];

  private cancelledRuns = new Set<string>();
  private runLive = false;
  private runSequence = 0;

  private readonly events: AuditEventView[] = [
    event("ev-1", todayAt(14, 35, 18), "invoke", "调用 Bot 被拒：缺「调用 Bot」（bot.invoke）", "客服一号", "Bot", "售后升级", "denied", "k-7f31", false, {
      actorId: id("bot-support"),
      objectId: id("bot-aftersales"),
      capabilityKey: "bot.invoke",
      objectOwnerName: "王强",
      objectOwnerId: id("wangqiang"),
      grantableBy: ["客服组", "超级管理员"],
    }),
    event("ev-2", todayAt(14, 33, 30), "invoke", "创建 Bot", "客服一号", "Bot", "退款专员", "ok", undefined, false, {
      actorId: id("bot-support"),
      objectId: id("bot-refund"),
    }),
    event("ev-3", todayAt(14, 32, 18), "invoke", "调用 Bot", "客服一号", "Bot", "订单查询", "ok", undefined, false, {
      actorId: id("bot-support"),
      objectId: id("bot-order"),
    }),
    event("ev-4", todayAt(14, 12, 4), "grant", "保存权限组", "李娜", "权限组", "客服组 · 3 项", "ok", undefined, false, {
      actorId: id("lina"),
      objectId: id("group-support"),
    }),
    event("ev-5", todayAt(14, 10, 22), "login", "登录成功", "李娜", "账号", "李娜", "ok", undefined, false, {
      actorId: id("lina"),
      objectId: id("lina"),
    }),
    event("ev-6", todayAt(9, 20, 14), "deny", "调用 Bot 被拒：缺「调用 Bot」（bot.invoke）", "客服一号", "Bot", "财务助手", "denied", undefined, false, {
      actorId: id("bot-support"),
      objectId: id("bot-finance"),
      capabilityKey: "bot.invoke",
      objectOwnerName: "财务助手",
      grantableBy: ["Bot 运营", "超级管理员"],
    }),
    event("ev-7", todayAt(9, 18, 41), "grant", "签发 Bot 密钥", "李娜", "Bot", "客服一号 · 生产 Webhook", "ok", undefined, false, {
      actorId: id("lina"),
      objectId: id("bot-support"),
    }),
    event("ev-8", daysAgoIso(1), "deny", "读取密钥被拒：缺「读取密钥值」（secret.read.value）", "客服一号", "密钥", "生产密钥", "denied", undefined, false, {
      actorId: id("bot-support"),
      objectId: id("key-support-prod"),
      capabilityKey: "secret.read.value",
      objectOwnerName: "客服一号",
      grantableBy: ["客服组"],
    }),
    event("ev-9", daysAgoIso(1), "relation", "共享会话", "李娜", "会话", "订单 88213 → 陈墨", "ok", undefined, false, {
      actorId: id("lina"),
    }),
    event("ev-10", daysAgoIso(2), "account", "禁用账号", "王强", "账号", "吴桐", "ok", undefined, true, {
      actorId: id("wangqiang"),
      objectId: id("wutong"),
    }),
    event("ev-11", daysAgoIso(2), "super-admin", "跨租户读取", "王强", "记录", "acme 审计", "ok", undefined, true, {
      actorId: id("wangqiang"),
    }),
    event("ev-12", daysAgoIso(3), "team", "启动团队", "李娜", "团队", "客服团队", "ok", undefined, false, {
      actorId: id("lina"),
      objectId: id("team-support"),
      teamId: id("team-support"),
      runId: RUN_ID,
    }),
  ];

  async whoami(): Promise<WhoAmI> {
    return {
      tenantId: TENANT_ID,
      accountId: CURRENT_ACCOUNT.accountId,
      username: CURRENT_ACCOUNT.username,
      displayName: CURRENT_ACCOUNT.displayName,
      kind: CURRENT_ACCOUNT.kind,
      disabled: false,
      isSuperAdmin: false,
      permissions: { ...CURRENT_PERMISSIONS },
    };
  }

  // ---- accounts ------------------------------------------------------------

  async listAccounts(limit = 100): Promise<IdentityPage<AccountView>> {
    return pageOf(this.accounts, limit);
  }

  async createAccount(input: CreateAccountInput): Promise<AccountView> {
    const username = input.username.trim();
    if (this.accounts.some((row) => row.username === username)) {
      throw new KeelApiError(409, "这个用户名已被占用，请换一个。", [], undefined, "username_taken");
    }
    const created = account(`account-${username}`, username, input.displayName?.trim() || username, input.kind ?? "human", { createdDaysAgo: 0 });
    created.createdAt = nowIso();
    created.updatedAt = nowIso();
    this.accounts.unshift(created);
    return created;
  }

  async getAccount(accountId: string): Promise<{ account: AccountView; groups: PermissionGroupSummary[] }> {
    const found = this.accounts.find((row) => row.accountId === accountId);
    if (!found) throw new KeelApiError(404, "没有找到这个账号。", [], undefined, "account_not_found");
    return {
      account: { ...found },
      groups: this.groups.filter((group) => group.members.some((member) => member.accountId === accountId)).map((group) => group.summary),
    };
  }

  async setAccountDisabled(accountId: string, disabled: boolean, _reason?: string): Promise<AccountView> {
    const found = this.accounts.find((row) => row.accountId === accountId);
    if (!found) throw new KeelApiError(404, "没有找到这个账号。");
    if (found.isSuperAdmin) {
      throw new KeelApiError(403, "超级管理员不可禁用或删除。", [], undefined, "super_admin_protected", {
        reason: "missing_grant",
        requiredKey: "account.manage",
      });
    }
    found.disabledAt = disabled ? nowIso() : undefined;
    found.updatedAt = nowIso();
    return { ...found };
  }

  async resetAccountPassword(accountId: string, _password: string): Promise<void> {
    const found = this.accounts.find((row) => row.accountId === accountId);
    if (!found) throw new KeelApiError(404, "没有找到这个账号。");
    if (found.isSuperAdmin) {
      throw new KeelApiError(403, "超级管理员只能在这台机器的终端重置密码。", [], undefined, "super_admin_protected", {
        reason: "missing_grant",
        requiredKey: "account.credential.reset",
      });
    }
    if (found.kind === "bot") {
      throw new KeelApiError(400, "Bot 账号使用密钥登录，不设置密码。");
    }
  }

  async addAccountGroup(accountId: string, groupId: string): Promise<void> {
    const group = this.groups.find((row) => row.summary.groupId === groupId);
    if (!group) throw new KeelApiError(404, "没有找到这个权限组。");
    if (group.summary.builtin) throw new KeelApiError(403, "这是内置组，不能直接修改。");
    if (!group.members.some((member) => member.accountId === accountId)) {
      group.members.push({ accountId, addedBy: CURRENT_ACCOUNT.accountId, createdAt: nowIso() });
      group.summary.updatedAt = nowIso();
    }
  }

  async removeAccountGroup(accountId: string, groupId: string): Promise<void> {
    const group = this.groups.find((row) => row.summary.groupId === groupId);
    if (!group) throw new KeelApiError(404, "没有找到这个权限组。");
    if (group.summary.builtin) throw new KeelApiError(403, "这是内置组，不能直接修改。");
    group.members = group.members.filter((member) => member.accountId !== accountId);
    group.summary.updatedAt = nowIso();
  }

  // ---- groups --------------------------------------------------------------

  async listPermissionGroups(limit = 100): Promise<IdentityPage<PermissionGroupSummary>> {
    return pageOf(
      this.groups.map((group) => ({ ...group.summary })),
      limit,
    );
  }

  async getPermissionGroup(groupId: string): Promise<PermissionGroupDetail> {
    const group = this.groups.find((row) => row.summary.groupId === groupId);
    if (!group) throw new KeelApiError(404, "没有找到这个权限组。");
    return {
      group: { ...group.summary },
      grants: group.grants.map((entry) => ({ ...entry })),
      members: group.members.map((member) => ({ ...member })),
    };
  }

  async createPermissionGroup(name: string): Promise<PermissionGroupSummary> {
    const summary: PermissionGroupSummary = {
      groupId: id(`group-${name}-${this.groups.length}`),
      name: name.trim(),
      builtin: false,
      createdAt: nowIso(),
      updatedAt: nowIso(),
    };
    this.groups.push({ summary, members: [], grants: [] });
    return { ...summary };
  }

  async addPermissionGroupMember(groupId: string, accountId: string): Promise<void> {
    const group = this.groups.find((row) => row.summary.groupId === groupId);
    if (!group) throw new KeelApiError(404, "没有找到这个权限组。");
    if (group.summary.builtin) throw new KeelApiError(403, "这是内置组，不能直接修改。");
    if (!group.members.some((member) => member.accountId === accountId)) {
      group.members.push({ accountId, addedBy: CURRENT_ACCOUNT.accountId, createdAt: nowIso() });
      group.summary.updatedAt = nowIso();
    }
  }

  async removePermissionGroupMember(groupId: string, accountId: string): Promise<void> {
    const group = this.groups.find((row) => row.summary.groupId === groupId);
    if (!group) throw new KeelApiError(404, "没有找到这个权限组。");
    if (group.summary.builtin) throw new KeelApiError(403, "这是内置组，不能直接修改。");
    group.members = group.members.filter((member) => member.accountId !== accountId);
    group.summary.updatedAt = nowIso();
  }

  async addPermissionGroupGrant(groupId: string, key: string, scope: string): Promise<void> {
    const group = this.groups.find((row) => row.summary.groupId === groupId);
    if (!group) throw new KeelApiError(404, "没有找到这个权限组。");
    if (group.summary.builtin) throw new KeelApiError(403, "这是内置组，不能直接修改。");
    if (!group.grants.some((entry) => entry.key === key && entry.scope === scope)) {
      group.grants.push({ key, scope: scope as PermissionScope });
      group.summary.updatedAt = nowIso();
    }
  }

  async removePermissionGroupGrant(groupId: string, key: string, scope: string): Promise<void> {
    const group = this.groups.find((row) => row.summary.groupId === groupId);
    if (!group) throw new KeelApiError(404, "没有找到这个权限组。");
    if (group.summary.builtin) throw new KeelApiError(403, "这是内置组，不能直接修改。");
    group.grants = group.grants.filter((entry) => !(entry.key === key && entry.scope === scope));
    group.summary.updatedAt = nowIso();
  }

  // ---- resource sharing ----------------------------------------------------

  async listShares(kind: ShareResourceKind, resourceId: string): Promise<ShareView[]> {
    return (this.shareTuples.get(`${kind}:${resourceId}`) ?? []).map((share) => ({ ...share }));
  }

  async addShare(kind: ShareResourceKind, resourceId: string, input: ShareInput): Promise<ShareWriteResult> {
    if (input.relation !== "viewer" && input.relation !== "editor") {
      throw new KeelApiError(400, "owner 与 team_member 由系统管理，不能通过共享授予。", [], undefined, "invalid_relation");
    }
    if (input.subjectType === "account" && input.subjectId === CURRENT_ACCOUNT.accountId) {
      throw new KeelApiError(400, "不能把资源共享给自己。", [], undefined, "self_share");
    }
    this.requireShareResource(kind, resourceId);
    const share: ShareView = {
      subjectType: input.subjectType,
      subjectId: input.subjectId,
      relation: input.relation,
      ...this.shareSubjectFacts(input),
    };
    const key = `${kind}:${resourceId}`;
    const rows = this.shareTuples.get(key) ?? [];
    if (rows.some((row) => row.subjectType === share.subjectType && row.subjectId === share.subjectId && row.relation === share.relation)) {
      return { created: false, share: { ...share } };
    }
    rows.push(share);
    this.shareTuples.set(key, rows);
    this.bumpSharedCount(kind, resourceId, 1);
    return { created: true, share: { ...share } };
  }

  async revokeShare(kind: ShareResourceKind, resourceId: string, input: ShareInput): Promise<ShareWriteResult> {
    const key = `${kind}:${resourceId}`;
    const rows = this.shareTuples.get(key) ?? [];
    const index = rows.findIndex(
      (row) => row.subjectType === input.subjectType && row.subjectId === input.subjectId && row.relation === input.relation,
    );
    const removed = index >= 0 ? rows[index] : undefined;
    if (!removed) throw new KeelApiError(404, "这条共享不存在。", [], undefined, "not_found");
    rows.splice(index, 1);
    this.shareTuples.set(key, rows);
    this.bumpSharedCount(kind, resourceId, -1);
    return { revoked: true, share: { ...removed } };
  }

  private requireShareResource(kind: ShareResourceKind, resourceId: string): void {
    if (!resourceId) throw new KeelApiError(400, "缺少资源 id。");
    if (kind === "bot" && !this.bots.some((entry) => entry.botId === resourceId)) {
      throw new KeelApiError(404, "没有找到这个 Bot。", [], undefined, "not_found");
    }
    if (kind === "team_bot" && !this.teams.some((entry) => entry.teamBotId === resourceId)) {
      throw new KeelApiError(404, "没有找到这个团队。", [], undefined, "not_found");
    }
  }

  private shareSubjectFacts(input: ShareInput): Pick<ShareView, "displayName" | "username"> {
    if (input.subjectType === "account") {
      const account = this.accounts.find((row) => row.accountId === input.subjectId);
      if (!account) throw new KeelApiError(404, "共享对象不存在。", [], undefined, "subject_not_found");
      return { displayName: account.displayName, username: account.username };
    }
    if (input.subjectType === "group") {
      const group = this.groups.find((row) => row.summary.groupId === input.subjectId);
      if (!group) throw new KeelApiError(404, "共享对象不存在。", [], undefined, "subject_not_found");
      return { displayName: group.summary.name };
    }
    const team = this.teams.find((row) => row.teamBotId === input.subjectId);
    if (!team) throw new KeelApiError(404, "共享对象不存在。", [], undefined, "subject_not_found");
    return { displayName: team.name };
  }

  private bumpSharedCount(kind: ShareResourceKind, resourceId: string, delta: number): void {
    const apply = (current: number | undefined) => Math.max(0, (current ?? 0) + delta);
    if (kind === "bot") {
      const bot = this.bots.find((entry) => entry.botId === resourceId);
      if (bot) bot.sharedCount = apply(bot.sharedCount);
    } else if (kind === "team_bot") {
      const team = this.teams.find((entry) => entry.teamBotId === resourceId);
      if (team) team.sharedCount = apply(team.sharedCount);
    }
  }

  // ---- profiles & roles ----------------------------------------------------

  async listProfiles(limit = 100): Promise<IdentityPage<ProfileView>> {
    return pageOf(this.profiles.map((profile) => ({ ...profile })), limit);
  }

  async getProfile(profileId: string): Promise<ProfileView> {
    const found = this.profiles.find((profile) => profile.profileId === profileId);
    if (!found) throw new KeelApiError(404, "没有找到这个身份档案。");
    return { ...found };
  }

  async listRoles(limit = 100): Promise<IdentityPage<RoleView>> {
    return pageOf(this.roles.map((role) => ({ ...role })), limit);
  }

  // ---- bots ----------------------------------------------------------------

  async listBots(limit = 100, includeArchived = false): Promise<IdentityPage<BotView>> {
    const rows = this.bots.filter((entry) => includeArchived || !entry.archived);
    return pageOf(rows.map((entry) => ({ ...entry })), limit);
  }

  async getBot(botId: string): Promise<BotView> {
    const found = this.bots.find((entry) => entry.botId === botId);
    if (!found) throw new KeelApiError(404, "没有找到这个 Bot，或者你没有访问权限。");
    return { ...found };
  }

  async createBot(input: CreateBotInput): Promise<BotView> {
    const profile = this.profiles.find((entry) => entry.profileId === input.profileId);
    if (!profile) throw new KeelApiError(400, "这个身份档案不存在。");
    if (this.bots.some((entry) => entry.name === input.name && !entry.archived)) {
      throw new KeelApiError(409, "已经有一个同名 Bot 了，换一个名字。");
    }
    const created: BotView = {
      botId: input.botId || id(`bot-created-${this.bots.length}`),
      name: input.name.trim(),
      profileId: profile.profileId,
      accountId: profile.accountId,
      modelKind: input.modelKind,
      defaultModel: input.defaultModel ?? { providerId: "s2api", modelId: "deepseek-v4.1-flash" },
      harness: input.harness ?? { tools: [], context: { instruction: "", permissions: [] } },
      archived: false,
      createdAt: nowIso(),
      updatedAt: nowIso(),
    };
    this.bots.push(created);
    return { ...created };
  }

  async updateBot(botId: string, input: UpdateBotInput): Promise<BotView> {
    const found = this.bots.find((entry) => entry.botId === botId);
    if (!found) throw new KeelApiError(404, "没有找到这个 Bot，或者你没有访问权限。");
    if (input.expectedArchived !== undefined && input.expectedArchived !== found.archived) {
      throw new KeelApiError(409, "这个 Bot 刚被改过（归档状态已变化）。刷新后再试。");
    }
    if (input.profileId !== undefined) {
      const profile = this.profiles.find((entry) => entry.profileId === input.profileId);
      if (!profile) throw new KeelApiError(400, "这个身份档案不存在。");
      found.profileId = profile.profileId;
      found.accountId = profile.accountId;
    }
    if (input.name !== undefined) found.name = input.name;
    if (input.modelKind !== undefined) found.modelKind = input.modelKind;
    if (input.defaultModel !== undefined) found.defaultModel = { ...input.defaultModel };
    if (input.harness !== undefined) {
      const known = new Set(CAPABILITIES.map((capability) => capability.key));
      const unknown = input.harness.context.permissions.find((key) => !known.has(key));
      if (unknown) {
        throw new KeelApiError(400, `harness permission: unknown key ${unknown}`, [], undefined, "invalid_permission_key");
      }
      found.harness = {
        tools: [...input.harness.tools],
        context: {
          instruction: input.harness.context.instruction,
          permissions: [...input.harness.context.permissions],
        },
      };
    }
    found.updatedAt = nowIso();
    return { ...found };
  }

  async archiveBot(botId: string): Promise<void> {
    const found = this.bots.find((entry) => entry.botId === botId);
    if (!found) throw new KeelApiError(404, "没有找到这个 Bot。");
    found.archived = true;
    found.updatedAt = nowIso();
  }

  async invokeBot(botId: string, _text: string): Promise<RunStartView> {
    const found = this.bots.find((entry) => entry.botId === botId);
    if (!found) throw new KeelApiError(404, "没有找到这个 Bot。");
    if (found.archived) throw new KeelApiError(409, "这个 Bot 已归档，不能再开始新的会话。");
    this.runSequence += 1;
    return { botId, sessionId: id(`bot-invoke-${this.runSequence}`), seq: 1 };
  }

  async listBotKeys(botId: string): Promise<BotKeyView[]> {
    return this.botKeys.filter((key) => key.botId === botId).map((key) => ({ ...key }));
  }

  async issueBotKey(botId: string, label = ""): Promise<{ key: BotKeyView; plaintext: string; notice: string }> {
    const found = this.bots.find((entry) => entry.botId === botId);
    if (!found) throw new KeelApiError(404, "没有找到这个 Bot。");
    if (found.archived) throw new KeelApiError(409, "已归档的 Bot 不能签发密钥。");
    const key: BotKeyView = {
      keyId: id(`key-new-${this.botKeys.length}`),
      prefix: "kb_new",
      accountId: found.accountId ?? "",
      botId,
      label: label.trim() || "未命名密钥",
      createdAt: nowIso(),
    };
    this.botKeys.push(key);
    this.issuedPlaintext = `kb_new_${key.keyId.replace(/-/g, "")}`;
    return {
      key: { ...key },
      plaintext: this.issuedPlaintext,
      notice: "这个密钥只显示一次，关闭后不能回读。",
    };
  }

  async revokeBotKey(botId: string, keyId: string): Promise<void> {
    const found = this.botKeys.find((key) => key.botId === botId && key.keyId === keyId);
    if (!found) throw new KeelApiError(404, "没有找到这把密钥。");
    found.revokedAt = nowIso();
  }

  // ---- team bots -----------------------------------------------------------

  async listTeamBots(limit = 100, includeArchived = false): Promise<IdentityPage<TeamBotView>> {
    const rows = this.teams.filter((team) => includeArchived || !team.archived);
    return pageOf(
      rows.map((team) => ({ ...team, roster: team.roster.map((seat) => ({ ...seat })) })),
      limit,
    );
  }

  async getTeamBot(teamBotId: string): Promise<TeamBotView> {
    const found = this.teams.find((team) => team.teamBotId === teamBotId);
    if (!found) throw new KeelApiError(404, "没有找到这个团队，或者你没有访问权限。");
    return { ...found, roster: found.roster.map((seat) => ({ ...seat })) };
  }

  async createTeamBot(input: CreateTeamBotInput): Promise<TeamBotView> {
    const created: TeamBotView = {
      teamBotId: input.teamBotId || id(`team-new-${this.teams.length}`),
      name: input.name,
      mission: input.mission,
      roster: (input.roster ?? []).map((seat) => ({ ...seat })),
      revision: 1,
      hash: `fixture-team-${this.teams.length + 1}`,
      archived: false,
      createdAt: nowIso(),
      updatedAt: nowIso(),
    };
    this.teams.push(created);
    return { ...created, roster: created.roster.map((seat) => ({ ...seat })) };
  }

  async updateTeamBot(teamBotId: string, input: UpdateTeamBotInput): Promise<TeamBotView> {
    const found = this.teams.find((team) => team.teamBotId === teamBotId);
    if (!found) throw new KeelApiError(404, "没有找到这个团队。");
    if (input.expectedRevision !== found.revision) {
      throw new KeelApiError(409, "这个团队刚被改过（版本已更新）。刷新后再试。", [], found.revision);
    }
    if (input.name !== undefined) found.name = input.name;
    if (input.mission !== undefined) found.mission = input.mission;
    if (input.roster !== undefined) found.roster = input.roster.map((seat) => ({ ...seat }));
    found.revision += 1;
    found.hash = `${found.hash}-r${found.revision}`;
    found.updatedAt = nowIso();
    return { ...found, roster: found.roster.map((seat) => ({ ...seat })) };
  }

  async updateTeamBotRoster(teamBotId: string, roster: RosterEntry[] | undefined, expectedRevision: number): Promise<TeamBotView> {
    return this.updateTeamBot(teamBotId, { roster, expectedRevision });
  }

  async archiveTeamBot(teamBotId: string): Promise<void> {
    const found = this.teams.find((team) => team.teamBotId === teamBotId);
    if (!found) throw new KeelApiError(404, "没有找到这个团队。");
    found.archived = true;
    found.updatedAt = nowIso();
  }

  async startTeamBot(teamBotId: string, _text?: string): Promise<RunStartView> {
    const found = this.teams.find((team) => team.teamBotId === teamBotId);
    if (!found) throw new KeelApiError(404, "没有找到这个团队。");
    if (found.archived) throw new KeelApiError(409, "这个团队已归档，不能启动。");
    const captain = found.roster.find((seat) => seat.role === "captain");
    if (!captain) throw new KeelApiError(409, "名单里没有队长。");
    const captainBot = this.bots.find((entry) => entry.botId === captain.botId);
    if (!captainBot) throw new KeelApiError(409, "队长不在可见 Bot 里。");
    if (captainBot.archived) throw new KeelApiError(409, "队长已归档。");
    this.cancelledRuns.delete(RUN_ID);
    this.runLive = true;
    return { teamBotId, captainBotId: captain.botId, sessionId: RUN_ID, seq: 1 };
  }

  // ---- the run projection's reads -----------------------------------------

  async sessionEvents(sessionId: string): Promise<SessionEventView[]> {
    if (sessionId === RUN_ID) {
      // A started run is live until it is stopped: the fixture withholds the
      // terminal run.finished so the page's running state is real (the stop
      // action appends its own cancelled terminal event).
      const suppressTerminal = this.runLive || this.cancelledRuns.has(sessionId);
      const events = CAPTAIN_EVENTS.filter((event) => !(suppressTerminal && event.kind === "run.finished")).map((event) => ({ ...event }));
      if (this.cancelledRuns.has(sessionId)) {
        events.push(
          runEvent(7, "run.finished", {
            tokensIn: 9000,
            tokensOut: 3400,
            durationMs: 260_000,
            stopReason: "cancelled",
            provider: "s2api",
            model: "deepseek-v4.1-flash",
            timestamp: agoMs(0, 0),
          }),
        );
      }
      return events;
    }
    const member = MEMBER_EVENTS[sessionId];
    if (member) return member.map((event) => ({ ...event }));
    if (sessionId === AFTER_SESSION) return [];
    return [];
  }

  async teamTasks(sessionId: string): Promise<{ tasks: TeamTaskView[]; hasMore: boolean }> {
    if (sessionId !== RUN_ID) return { tasks: [], hasMore: false };
    // The endpoint answers the *member* cards (the captain's own card is the
    // session's initial card, not a member): the run page's topology depends
    // on that split, and the panel reads the own cards separately.
    return { tasks: this.tasks.map((task) => ({ ...task })), hasMore: false };
  }

  /** The run surface's task panel, computed from the same in-memory task state
   * the team page and the member cards read. The captain session's own cards
   * are CAPTAIN_TASKS; the member cards are this.tasks (the sub-agent and fork
   * cards whose parentTaskId points at a captain card); a card whose parent
   * lives outside the asked session is reported as an ancestor. No panel field
   * exists that this data does not hold. */
  async taskPanel(sessionId: string, memberLimit = 200, eventLimit = 500): Promise<TaskPanelView> {
    const ownCard = (task: TeamTaskView): TaskPanelCardView =>
      taskPanelCard(task, {
        origin: task.taskId === FORK_CARD_ID ? "fork" : task.origin,
        originSeq: task.taskId === FORK_CARD_ID ? FORK_ORIGIN_SEQ : undefined,
      });
    // The session's own cards are the rows its ledger holds: the captain run's
    // are CAPTAIN_TASKS, every other sample session's are the task rows whose
    // sessionId is that session (the row never travels into another session).
    const cards =
      sessionId === RUN_ID
        ? CAPTAIN_TASKS.map((card) => ({ ...card }))
        : this.tasks.filter((task) => task.sessionId === sessionId).map(ownCard);
    const ownIds = new Set(cards.map((card) => card.taskId));

    // A member is a card whose parent is one of *this* session's cards; its own
    // row lives in its own session. This is the projection's join: a card whose
    // parent points elsewhere is not a member here, and its row is not copied.
    const members = this.tasks
      .filter((task) => task.sessionId !== sessionId && task.parentTaskId !== undefined && ownIds.has(task.parentTaskId))
      .map((task) =>
        taskPanelCard(task, {
          origin: task.taskId === FORK_CARD_ID ? "fork" : task.origin,
          originSeq: task.taskId === FORK_CARD_ID ? FORK_ORIGIN_SEQ : undefined,
          member: true,
        }),
      )
      // The projection's member order: newest activity first, then task id.
      .sort((left, right) => {
        const leftAt = Date.parse(left.updatedAt ?? "");
        const rightAt = Date.parse(right.updatedAt ?? "");
        if (leftAt !== rightAt) return rightAt - leftAt;
        return left.taskId < right.taskId ? 1 : left.taskId > right.taskId ? -1 : 0;
      });

    // An ancestor is a parent card the read did not carry: its row lives in
    // another session, so it is named (with its own title) rather than copied
    // into the tree as an own card. The chain is not walked past the first
    // parent, exactly like the projection.
    const cardById = new Map<string, TaskPanelCardView>();
    for (const card of CAPTAIN_TASKS) cardById.set(card.taskId, { ...card });
    for (const task of this.tasks) {
      if (!cardById.has(task.taskId)) cardById.set(task.taskId, ownCard(task));
    }
    const ancestors: TaskPanelCardView[] = [];
    for (const card of [...cards, ...members]) {
      const parentId = card.parentTaskId;
      if (!parentId || ownIds.has(parentId) || ancestors.some((ancestor) => ancestor.taskId === parentId)) continue;
      const parent = cardById.get(parentId);
      if (parent) ancestors.push({ ...parent, member: true });
    }

    // The stream is the asked session's own ledger: a member card's events live
    // in its own session and are not copied into this one.
    const events: TaskPanelEventView[] = [];
    const attachments: TaskPanelAttachmentView[] = [];
    let seq = 20;
    const next = () => (seq += 1);
    for (const card of cards) {
      const parentCardId = card.parentTaskId ?? CAPTAIN_CARD_ID;
      events.push(...taskPanelEventsFor(card, parentCardId, next));
      if (card.taskId === CAPTAIN_CARD_ID) {
        attachments.push(
          { taskId: card.taskId, path: "reports/refund-analysis.md", hash: REFUND_REPORT_HASH },
          { taskId: card.taskId, path: "reports/refund-report.json", hash: REFUND_JSON_HASH },
        );
      }
    }
    // The stream is ledger-ordered: the fixture's seq values encode the order,
    // so sorting makes it explicit rather than relying on array order. The page
    // is the newest N, like the route's own event page.
    events.sort((left, right) => left.seq - right.seq);
    const boundedMemberLimit = Math.max(1, memberLimit);
    const boundedEventLimit = Math.max(1, eventLimit);
    const memberPage = members.slice(0, boundedMemberLimit);
    const eventPage = events.slice(-boundedEventLimit);
    const truncatedMembers = members.length - memberPage.length;
    return {
      sessionId,
      cards,
      members: memberPage,
      ancestors,
      attachments,
      events: eventPage,
      limits: {
        cardLimit: 1000,
        cardsTruncated: false,
        memberLimit: boundedMemberLimit,
        truncated: truncatedMembers > 0,
        truncatedMemberCount: truncatedMembers > 0 ? truncatedMembers : undefined,
        eventLimit: boundedEventLimit,
        eventsTruncated: events.length > eventPage.length,
        attachmentLimit: 1000,
        attachmentsTruncated: false,
        ancestorsTruncated: false,
      },
    };
  }

  async filePreview(sessionId: string, path: string, referenceHash?: string, offset = 0): Promise<FilePreviewView> {
    if (sessionId !== RUN_ID && sessionId !== REFUND_SESSION) {
      throw new KeelApiError(404, "没有找到这个会话。");
    }
    const file = FIXTURE_FILES[path];
    if (!file) throw new KeelApiError(404, `会话里没有 ${path} 这个文件。`);
    const matchesReference = referenceHash === undefined ? undefined : referenceHash === file.hash;
    return {
      path,
      hash: file.hash,
      size: file.content.length,
      seq: file.seq,
      offset,
      limit: 200,
      totalLines: file.content.split("\n").length,
      returnedLines: file.content.split("\n").length,
      nextOffset: file.content.split("\n").length,
      truncated: false,
      content: file.content,
      referenceHash,
      matchesReference,
    };
  }

  async listSubagents(parentSessionId?: string): Promise<SubagentView[]> {
    const rows = parentSessionId
      ? this.subagents.filter((record) => record.parentSessionId === parentSessionId)
      : this.subagents;
    return rows.map((record) => ({ ...record, usage: record.usage ? { ...record.usage } : undefined }));
  }

  async cancelSession(sessionId: string): Promise<SessionCancelView> {
    if (sessionId !== RUN_ID) throw new KeelApiError(404, "没有找到这个会话。");
    this.cancelledRuns.add(sessionId);
    this.runLive = false;
    return { status: "cancelled", runStatus: "cancelled", cancelled: true };
  }

  // ---- records -------------------------------------------------------------

  async listAuditEvents(query: AuditQuery = {}): Promise<IdentityPage<AuditEventView>> {
    const limit = Math.max(1, Math.min(500, query.limit ?? 100));
    const since = query.since ? Date.parse(query.since) : Number.NEGATIVE_INFINITY;
    const kinds = query.kind ? new Set(query.kind.split(",")) : null;
    const rows = this.events
      .filter((row) => {
        const at = row.occurredAt ? Date.parse(row.occurredAt) : Number.NEGATIVE_INFINITY;
        if (Number.isFinite(since) && at < since) return false;
        if (kinds && !kinds.has(row.kind)) return false;
        if (query.result && row.result !== query.result) return false;
        return true;
      })
      .sort((left, right) => Date.parse(right.occurredAt ?? "") - Date.parse(left.occurredAt ?? ""));
    return pageOf(rows, limit);
  }
}

function event(
  eventId: string,
  occurredAt: string,
  kind: AuditKind,
  summary: string,
  actorName: string,
  objectType: string,
  objectName: string,
  result: AuditEventView["result"],
  requestId?: string,
  superAdmin = false,
  extra: Partial<AuditEventView> = {},
): AuditEventView {
  return {
    eventId,
    occurredAt,
    kind: superAdmin && kind !== "super-admin" && kind !== "deny" ? "super-admin" : kind,
    action: kind,
    summary,
    actorName,
    actorKind: actorName === "客服一号" ? "bot" : "human",
    objectType,
    objectName,
    result,
    requestId,
    source: "203.0.113.x",
    ...extra,
  };
}
