// TUI/web evidence harness · the sub-agent tree on the run page — change
// keel-collab-subagent, task 4.3/5.3.
//
// The five states and the depth/cancel/retry facts a reviewer needs to see are
// driven by one scripted identity transport, selected by the launch parameter
// `host.params.scenario`:
//
//   run-loading     the team reads, the session ledger never settles
//   run-empty       no summary, no session ledger, no tasks, no sub-agents
//   run-error       the session ledger answers 500 (request id carried)
//   run-permission  the session ledger relation is refused (403 + key)
//   run-success     captain + four spawned records: a 1 -> 2 -> 3 depth chain
//                   (maxDepth 3) and a sibling with maxDepth=-1 (unlimited)
//   run-cancel      run-success, plus a cancelSession that reports cancelled
//   run-retry       run-success, with the depth-2 child's ledger failing once
//
// The success faces of the *product entry* are still the app's own fixture
// transport (`?fixture=1` / `host.params.fixture`); this harness exists because
// a test's module graph is not the app's. The wire facts (record depth and
// maxDepth, main = 0) are exactly what the server sends; the run surface does
// not render a numeric depth label, which is a recorded UI gap, not something
// this harness hides.
import { host, mount, View } from "nsh";
import { setEffectivePermissions } from "../../data/permissions";
import { KeelApiError } from "../../data/http";
import type { IdentityApi } from "../../data/identity-api";
import type { AccountView, PermissionScope, TeamBotView } from "../../data/identity";
import type { SessionSummary } from "../../data/contract";
import type { SubagentView } from "../../data/bots-wire";
import { identityAccess } from "../../data/identity-access";
import { botAccess } from "../../data/bots";
import { store } from "../../data/store";
import { color } from "../../ui/kit";
import { BooksChrome } from "./shell";
import { BooksChapterRoute } from "./chapters";

const scenario = (): string => String(host.params.scenario ?? "run-success");

const pageOf = <T,>(items: T[]) => ({ items, limit: 100, hasMore: false });

const TEAM_ID = "team-subagent";
const RUN_ID = "8a10c1de-0000-4000-8000-000000000010";
const CHILD_DEPTH_1 = "8a10c1de-0000-4000-8000-000000000011";
const CHILD_DEPTH_2 = "8a10c1de-0000-4000-8000-000000000012";
const CHILD_DEPTH_3 = "8a10c1de-0000-4000-8000-000000000013";
const CHILD_UNLIMITED = "8a10c1de-0000-4000-8000-000000000014";

const ACCOUNT: AccountView = {
  accountId: "acc-1",
  username: "lina",
  displayName: "李娜",
  kind: "human",
  isSuperAdmin: false,
};

const TEAM: TeamBotView = {
  teamBotId: TEAM_ID,
  name: "客服团队",
  mission: "在 4 小时内解决客户问题，无法解决时升级人工。",
  roster: [{ botId: "bot-captain", role: "captain" }],
  revision: 1,
  hash: "harness-team-subagent-v1",
  archived: false,
};

const SUMMARY: SessionSummary = {
  sessionId: RUN_ID,
  createdAt: new Date(Date.now() - 120_000).toISOString(),
  updatedAt: new Date().toISOString(),
  runStatus: "running",
  title: "本周退款原因分析",
};

const failure = (): KeelApiError =>
  new KeelApiError(500, "服务暂时没有响应", [], undefined, undefined, {}, "k-run-1");

const forbidden = (): KeelApiError =>
  new KeelApiError(403, "permission denied", [], undefined, "missing_relation", {
    reason: "missing_relation",
    requiredKey: "session.read",
  });

/** The three wire records the run projection must draw: the server's own depth
 * projection, not a number this harness invented for the tree. */
const SUBAGENTS: SubagentView[] = [
  {
    id: `sub-${CHILD_DEPTH_1}`,
    parentSessionId: RUN_ID,
    sessionId: CHILD_DEPTH_1,
    task: "查询退款订单",
    agent: "归档查询",
    status: "completed",
    origin: "spawn",
    depth: 1,
    maxDepth: 3,
    startedAt: new Date(Date.now() - 90_000).toISOString(),
    endedAt: new Date(Date.now() - 80_000).toISOString(),
    usage: { turns: 1, inputTokens: 1000, outputTokens: 120, durationMs: 10_000 },
  },
  {
    id: `sub-${CHILD_DEPTH_2}`,
    parentSessionId: CHILD_DEPTH_1,
    sessionId: CHILD_DEPTH_2,
    task: "核对退款条款",
    agent: "条款核对",
    status: "failed",
    origin: "spawn",
    depth: 2,
    maxDepth: 3,
    startedAt: new Date(Date.now() - 70_000).toISOString(),
    endedAt: new Date(Date.now() - 60_000).toISOString(),
    error: "调用被拒：缺「调用 Bot」能力（bot.invoke）。",
  },
  {
    id: `sub-${CHILD_DEPTH_3}`,
    parentSessionId: CHILD_DEPTH_2,
    sessionId: CHILD_DEPTH_3,
    task: "复核政策原文",
    agent: "政策核对",
    status: "completed",
    origin: "spawn",
    depth: 3,
    maxDepth: 3,
    startedAt: new Date(Date.now() - 50_000).toISOString(),
    endedAt: new Date(Date.now() - 40_000).toISOString(),
    usage: { turns: 2, inputTokens: 2000, outputTokens: 240, durationMs: 10_000 },
  },
  {
    id: `sub-${CHILD_UNLIMITED}`,
    parentSessionId: RUN_ID,
    sessionId: CHILD_UNLIMITED,
    task: "不限深度分支",
    agent: "深度不限",
    status: "completed",
    origin: "spawn",
    depth: 1,
    maxDepth: -1,
    startedAt: new Date(Date.now() - 30_000).toISOString(),
    endedAt: new Date(Date.now() - 20_000).toISOString(),
    usage: { turns: 1, inputTokens: 900, outputTokens: 90, durationMs: 10_000 },
  },
];

let cancelled = false;
let childLedgerAttempts = 0;

const baseApi = (): IdentityApi => ({
  async listAccounts() {
    return pageOf([ACCOUNT]);
  },
  async listBots() {
    return pageOf([]);
  },
  async listTeamBots() {
    return pageOf([TEAM]);
  },
  async getTeamBot() {
    return TEAM;
  },
  async listSessions() {
    if (scenario() === "run-empty") return [];
    return [{ ...SUMMARY, runStatus: cancelled ? "cancelled" : "running" }];
  },
  async cancelSession() {
    cancelled = true;
    return { status: "cancelled", runStatus: "cancelled", cancelled: true, detail: "已停止子树" };
  },
});

const apiFor = (name: string): IdentityApi => {
  const api = baseApi();
  if (name === "run-empty") return api;
  api.teamTasks = async () => ({ tasks: [], hasMore: false });
  api.sessionEvents = async (sessionId: string) => {
    if (name === "run-loading") return new Promise(() => {});
    if (name === "run-error") throw failure();
    if (name === "run-permission") throw forbidden();
    if (name === "run-retry" && sessionId === CHILD_DEPTH_2) {
      childLedgerAttempts += 1;
      if (childLedgerAttempts === 1) {
        throw new KeelApiError(500, "节点账本暂时不可用", [], undefined, undefined, {}, "k-node-1");
      }
    }
    return [];
  };
  api.listSubagents = async () => {
    if (name === "run-permission") throw forbidden();
    return SUBAGENTS;
  };
  return api;
};

const ALL_KEYS: Record<string, PermissionScope> = {
  "bot.read": "tenant",
  "bot.create": "tenant",
  "bot.manage": "tenant",
  "bot.bind": "tenant",
  "bot.invoke": "tenant",
  "team_bot.read": "tenant",
  "team_bot.manage": "tenant",
  "account.read": "tenant",
  "permission.read": "tenant",
  "session.read": "team",
  "session.write": "team",
  "audit.read": "tenant",
};

identityAccess.resetForTest();
botAccess.resetForTest();
store.skipBootForTest();
store.setIdentityTransport(apiFor(scenario()));
setEffectivePermissions(ALL_KEYS, false);
store.openTeamRun(TEAM_ID, RUN_ID);

function Harness() {
  return (
    <View style={{ flex: 1, minHeight: 0, backgroundColor: color.background }}>
      <BooksChrome>
        <BooksChapterRoute />
      </BooksChrome>
    </View>
  );
}

mount(Harness);
