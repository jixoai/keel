// TUI harness · the 员工 / 团队 / 运行 chapters in their non-success states —
// change keel-bot-identity-access, A4.4b.
//
// A harness is needed because a test's module graph is not the app's: the seam
// has to be installed inside the launched app. The scenario arrives in the
// launch parameters (`host.params.scenario`):
//
//   bot-loading / bot-empty / bot-error / bot-permission
//   team-loading / team-empty / team-error / team-permission
//   run-error        the team reads, the session ledger answers 500
//   run-permission   the team reads, the session relation is refused
//
// The success faces are driven by the app's own fixture transport
// (`?fixture=1` / `host.params.fixture`), not here.
import { host, mount, View } from "nsh";
import { setEffectivePermissions } from "../../data/permissions";
import { KeelApiError } from "../../data/http";
import type { IdentityApi } from "../../data/identity-api";
import type { AccountView, BotView, PermissionScope, TeamBotView } from "../../data/identity";
import type { ProfileView, RoleView } from "../../data/bots-wire";
import type { SessionSummary } from "../../data/contract";
import { identityAccess } from "../../data/identity-access";
import { botAccess } from "../../data/bots";
import { store } from "../../data/store";
import { color } from "../../ui/kit";
import { BooksChrome } from "./shell";
import { BooksChapterRoute } from "./chapters";

const scenario = (): string => String(host.params.scenario ?? "bot-empty");
const chapter = (): string => String(host.params.chapter ?? "bots");

const pageOf = <T,>(items: T[]) => ({ items, limit: 100, hasMore: false });

const BOT_ID = "bot-1";
const TEAM_ID = "team-1";
const RUN_ID = "run-1";

const ACCOUNT: AccountView = {
  accountId: "acc-1",
  username: "lina",
  displayName: "李娜",
  kind: "human",
  isSuperAdmin: false,
};

const BOT: BotView = {
  botId: BOT_ID,
  name: "客服一号",
  profileId: "profile-1",
  accountId: ACCOUNT.accountId,
  modelKind: "text",
  defaultModel: { providerId: "s2api", modelId: "deepseek-v4.1-flash" },
  harness: { tools: ["search_orders"], context: { instruction: "客服一线", permissions: ["session.read"] } },
  archived: false,
};

const TEAM: TeamBotView = {
  teamBotId: TEAM_ID,
  name: "客服团队",
  mission: "在 4 小时内解决客户问题，无法解决时升级人工。",
  roster: [{ botId: BOT_ID, role: "captain" }],
  revision: 1,
  hash: "harness-team-v1",
  archived: false,
};

const PROFILE: ProfileView = { profileId: "profile-1", accountId: ACCOUNT.accountId, roleId: "role-1" };
const ROLE: RoleView = { roleId: "role-1", name: "客服专员" };
const SUMMARY: SessionSummary = {
  sessionId: RUN_ID,
  createdAt: new Date().toISOString(),
  updatedAt: new Date().toISOString(),
  runStatus: "running",
  title: "本周退款原因分析",
};

const failure = (): KeelApiError =>
  new KeelApiError(500, "服务暂时没有响应", [], undefined, undefined, {}, "k-test-1");

const forbidden = (): KeelApiError =>
  new KeelApiError(403, "permission denied", [], undefined, "missing_relation", {
    reason: "missing_relation",
    requiredKey: "session.read",
  });

const apiFor = (name: string): IdentityApi => ({
  async listAccounts() {
    return pageOf([ACCOUNT]);
  },
  async listPermissionGroups() {
    return pageOf([]);
  },
  async listProfiles() {
    return pageOf([PROFILE]);
  },
  async listRoles() {
    return pageOf([ROLE]);
  },
  async listBots() {
    if (name === "bot-loading") return new Promise(() => {});
    if (name === "bot-error") throw failure();
    if (name === "bot-empty") return pageOf([]);
    return pageOf([BOT]);
  },
  async getBot() {
    if (name === "bot-detail-error") throw failure();
    return BOT;
  },
  async listTeamBots() {
    if (name === "team-loading") return new Promise(() => {});
    if (name === "team-error") throw failure();
    if (name === "team-empty") return pageOf([]);
    return pageOf([TEAM]);
  },
  async getTeamBot() {
    if (name === "team-detail-error") throw failure();
    return TEAM;
  },
  async listSessions() {
    return [SUMMARY];
  },
  async sessionEvents() {
    if (name === "run-error") throw failure();
    if (name === "run-permission") throw forbidden();
    return [];
  },
  async teamTasks() {
    if (name === "run-permission") throw forbidden();
    return { tasks: [], hasMore: false };
  },
  async listSubagents() {
    if (name === "run-permission") throw forbidden();
    return [];
  },
});

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
  "audit.read": "tenant",
};

identityAccess.resetForTest();
botAccess.resetForTest();
store.skipBootForTest();
store.setIdentityTransport(apiFor(scenario()));
setEffectivePermissions(
  scenario() === "bot-permission"
    ? { "session.read": "team" }
    : scenario() === "team-permission"
      ? { "bot.read": "tenant", "session.read": "team" }
      : ALL_KEYS,
  false,
);
if (chapter() === "teams") store.openChapter("teams");
else if (chapter() === "run") store.openTeamRun(TEAM_ID, RUN_ID);
else store.openChapter("bots");

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
