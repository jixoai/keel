// The permission matrix's diff and the scope ceiling — change
// keel-bot-identity-access, A4.4a.
//
// These are the two places where a UI mistake would become a real grant: the
// disabled options (`grant ⊆ own`) and the exact diff a save sends. Both are
// pure enough to pin here, through the same store the pages use.
import { expect, test } from "nsh/test";
import type { PermissionGroupDetail } from "./identity";
import { identityAccess } from "./identity-access";
import type { IdentityApi } from "./identity-api";
import { setEffectivePermissions } from "./permissions";
import { scopeDisabledReason } from "./permission-catalog";
import { store } from "./store";

const round = {
  start: () => {
    store.skipBootForTest();
    identityAccess.resetForTest();
    setEffectivePermissions({ "session.read": "team", "bot.invoke": "team", "account.read": "tenant" }, false);
  },
};

test("范围上限：超过自己持有的范围禁用并给原因", () => {
  round.start();
  // session.read: the caller holds 本团队.
  expect(scopeDisabledReason("session.read", "own", "team")).toBeUndefined();
  expect(scopeDisabledReason("session.read", "team", "team")).toBeUndefined();
  expect(scopeDisabledReason("session.read", "tenant", "team")).toBe("你最多只能授予「本团队」。");
  expect(scopeDisabledReason("session.read", "all", "team")).toBe("你最多只能授予「本团队」。");
  // A tenant-only key cannot use own/team at all.
  expect(scopeDisabledReason("account.read", "team", "tenant")).toBe("这项能力只支持租户级范围。");
  expect(scopeDisabledReason("account.read", "tenant", "tenant")).toBeUndefined();
  // No key, no grant.
  expect(scopeDisabledReason("bot.manage", "team", undefined)).toBe("你没有这项能力，不能授予它。");
  // A deployment administrator holds everything.
  setEffectivePermissions({}, true);
  expect(scopeDisabledReason("session.read", "all", "all")).toBeUndefined();
});

test("退出/换号：不把上一个会话的账号与筛选留在内存里", async () => {
  round.start();
  store.setIdentityTransport({
    async listAccounts() {
      return {
        items: [
          {
            accountId: "acc-1",
            username: "lina",
            displayName: "李娜",
            kind: "human",
            isSuperAdmin: false,
          },
        ],
        limit: 100,
        hasMore: false,
      };
    },
    async listPermissionGroups() {
      return { items: [], limit: 100, hasMore: false };
    },
  });
  await identityAccess.loadAccounts();
  identityAccess.accountsQuery.set("李娜");
  expect(identityAccess.accounts().length).toBe(1);
  identityAccess.clearSession();
  expect(identityAccess.accounts().length).toBe(0);
  expect(identityAccess.accountsQuery()).toBe("");
});

test("变更集：新增 / 提升 / 收窄 / 撤销，高危只在放宽时要求确认", async () => {
  round.start();
  const detail: PermissionGroupDetail = {
    group: { groupId: "grp-1", name: "客服组", builtin: false },
    grants: [
      { key: "session.read", scope: "team" },
      { key: "session.write", scope: "own" },
    ],
    members: [{ accountId: "acc-1" }, { accountId: "acc-2" }],
  };
  const calls: string[] = [];
  const api: IdentityApi = {
    async listAccounts() {
      return { items: [], limit: 100, hasMore: false };
    },
    async listPermissionGroups() {
      return { items: [], limit: 100, hasMore: false };
    },
    async getPermissionGroup() {
      return detail;
    },
    async addPermissionGroupGrant(_groupId, key, scope) {
      calls.push(`add ${key} ${scope}`);
      detail.grants.push({ key, scope: scope as "own" | "team" | "tenant" | "all" });
    },
    async removePermissionGroupGrant(_groupId, key, scope) {
      calls.push(`remove ${key} ${scope}`);
      detail.grants = detail.grants.filter((entry) => !(entry.key === key && entry.scope === scope));
    },
  };
  store.setIdentityTransport(api);
  await identityAccess.loadGroup("grp-1");
  expect(identityAccess.groupChanges().length).toBe(0);

  identityAccess.setDraftScope("session.read", "tenant"); // raise
  identityAccess.setDraftScope("session.write", undefined); // revoke
  identityAccess.setDraftScope("bot.invoke", "team"); // add, critical → high risk
  const changes = identityAccess.groupChanges();
  const kinds = changes.map((change) => `${change.key}:${change.kind}:${change.highRisk}`).sort();
  expect(kinds).toEqual(["bot.invoke:add:true", "session.read:raise:false", "session.write:revoke:false"]);

  // The save adds the widened grant before removing the old one, and the
  // critical add is in the same ledger.
  const result = await identityAccess.saveGroupDraft();
  expect(result.ok).toBe(true);
  expect(calls).toContain("add session.read tenant");
  expect(calls).toContain("remove session.read team");
  expect(calls.indexOf("add session.read tenant")).toBeLessThan(calls.indexOf("remove session.read team"));
  expect(calls).toContain("remove session.write own");
  expect(calls).toContain("add bot.invoke team");
});
