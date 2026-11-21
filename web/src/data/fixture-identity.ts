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
  AccountView,
  AuditEventView,
  AuditKind,
  AuditQuery,
  BotView,
  CreateAccountInput,
  IdentityPage,
  PermissionGroupDetail,
  PermissionGroupSummary,
  PermissionScope,
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

/** The current user: 李娜, a team administrator — enough to see every chapter,
 * and a `bot.invoke` ceiling of 本团队 so the matrix's 全部/整个租户 options are
 * visibly disabled with their reason. */
const CURRENT_ACCOUNT: AccountView = account("lina", "lina", "李娜", "human", { createdDaysAgo: 19 });

const CURRENT_PERMISSIONS: Record<string, PermissionScope> = {
  "session.read": "team",
  "session.write": "team",
  "session.delete": "team",
  "session.share": "team",
  "bot.read": "tenant",
  "bot.create": "tenant",
  "bot.manage": "tenant",
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
    account("support1", "support-1", "客服一号", "bot", { createdDaysAgo: 14 }),
    account("orderbot", "order-bot", "订单查询", "bot", { createdDaysAgo: 11 }),
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
        { accountId: id("support1") },
        { accountId: id("orderbot") },
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
        { accountId: id("support1") },
        { accountId: id("orderbot") },
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
    {
      botId: id("bot-butler"),
      name: "管家",
      profileId: id("profile-butler"),
      accountId: id("butler"),
      modelKind: "text",
      defaultModel: { providerId: "s2api", modelId: "deepseek-v4.1-flash" },
      harness: { tools: [], context: { instruction: "", permissions: [] } },
      archived: false,
    },
    {
      botId: id("bot-support"),
      name: "客服一号",
      profileId: id("profile-support"),
      accountId: id("support1"),
      modelKind: "text",
      defaultModel: { providerId: "s2api", modelId: "deepseek-v4.1-flash" },
      harness: { tools: [], context: { instruction: "", permissions: [] } },
      archived: false,
    },
    {
      botId: id("bot-order"),
      name: "订单查询",
      profileId: id("profile-order"),
      accountId: id("orderbot"),
      modelKind: "text",
      defaultModel: { providerId: "s2api", modelId: "deepseek-v4.1-flash" },
      harness: { tools: [], context: { instruction: "", permissions: [] } },
      archived: false,
    },
  ];

  private readonly events: AuditEventView[] = [
    event("ev-1", todayAt(14, 35, 18), "invoke", "调用 Bot 被拒", "客服一号", "Bot", "售后升级", "denied", "k-7f31"),
    event("ev-2", todayAt(14, 33, 30), "invoke", "创建 Bot", "客服一号", "Bot", "退款专员", "ok"),
    event("ev-3", todayAt(14, 32, 18), "invoke", "调用 Bot", "客服一号", "Bot", "订单查询", "ok"),
    event("ev-4", todayAt(14, 12, 4), "grant", "保存权限组", "李娜", "权限组", "客服组 · 3 项", "ok"),
    event("ev-5", todayAt(14, 10, 22), "login", "登录成功", "李娜", "账号", "李娜", "ok"),
    event("ev-6", todayAt(9, 20, 14), "deny", "调用 Bot 被拒", "客服一号", "Bot", "财务助手", "denied"),
    event("ev-7", todayAt(9, 18, 41), "grant", "签发 Bot 密钥", "李娜", "Bot", "客服一号 · 生产 Webhook", "ok"),
    event("ev-8", daysAgoIso(1), "deny", "读取密钥被拒", "客服一号", "密钥", "生产密钥", "denied"),
    event("ev-9", daysAgoIso(1), "relation", "共享会话", "李娜", "会话", "订单 88213 → 陈墨", "ok"),
    event("ev-10", daysAgoIso(2), "account", "禁用账号", "王强", "账号", "吴桐", "ok", undefined, true),
    event("ev-11", daysAgoIso(2), "super-admin", "跨租户读取", "王强", "记录", "acme 审计", "ok", undefined, true),
    event("ev-12", daysAgoIso(3), "team", "启动团队", "李娜", "团队", "客服团队", "ok"),
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

  // ---- bots ----------------------------------------------------------------

  async listBots(limit = 100, _includeArchived = false): Promise<IdentityPage<BotView>> {
    return pageOf(this.bots.map((bot) => ({ ...bot })), limit);
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
  };
}
