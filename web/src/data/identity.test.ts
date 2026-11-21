// The identity parsers: the server's wire shapes stay exactly what it sent.
// Unknown scopes, empty ids and unknown kinds are dropped at the boundary, not
// guessed into a safe-looking value.
import { expect, test } from "nsh/test";
import {
  identityPage,
  parseAuthSession,
  parseBot,
  parseBotKey,
  parsePermissionGroupDetail,
  parseTeamBot,
  parseWhoAmI,
} from "./identity";

test("whoami: account, bot binding and the effective (key, scope) set", () => {
  const me = parseWhoAmI({
    tenantId: "t-1",
    account: { id: "a-1", username: "admin", displayName: "管理员", kind: "human", isSuperAdmin: true },
    permissions: { "session.read": "own", "bot.invoke": "team", "secret.read": "nope" },
    bot: { id: "support-1" },
    keyId: "k-1",
    keyPrefix: "keel_bot_abc",
  });
  expect(me.accountId).toBe("a-1");
  expect(me.username).toBe("admin");
  expect(me.isSuperAdmin).toBe(true);
  expect(me.botId).toBe("support-1");
  expect(me.keyPrefix).toBe("keel_bot_abc");
  // An unknown scope is dropped rather than widened to a default.
  expect(Object.keys(me.permissions).sort()).toEqual(["bot.invoke", "session.read"]);
  expect(me.permissions["bot.invoke"]).toBe("team");
  expect(me.permissions["secret.read"]).toBe(undefined);
});

test("session: login answer carries csrf + mustChangePassword; bootstrapRequired is anonymous", () => {
  const signedIn = parseAuthSession({
    authenticated: true,
    mode: "required",
    csrfToken: "csrf-1",
    account: { id: "a-1", username: "admin", displayName: "管理员", isSuperAdmin: false },
    mustChangePassword: true,
    tenantId: "t-1",
    sessionId: "s-1",
    expiresAt: "2026-11-23T00:00:00Z",
  });
  expect(signedIn.authenticated).toBe(true);
  expect(signedIn.csrfToken).toBe("csrf-1");
  expect(signedIn.mustChangePassword).toBe(true);
  expect(signedIn.accountId).toBe("a-1");
  expect(signedIn.sessionId).toBe("s-1");

  const anonymous = parseAuthSession({
    authenticated: false,
    mode: "required",
    bootstrapRequired: true,
    csrfToken: "csrf-anon",
  });
  expect(anonymous.authenticated).toBe(false);
  expect(anonymous.bootstrapRequired).toBe(true);
  expect(anonymous.mustChangePassword).toBe(false);
});

test("bot: profile, model kind, default model and harness survive; missing arrays stay empty", () => {
  const bot = parseBot({
    botId: "support-1",
    name: "客服一号",
    profileId: "p-1",
    accountId: "a-1",
    modelKind: "text",
    defaultModel: { providerId: "magpie", modelId: "qwen3.8-flash" },
    harness: { tools: ["bash", "", "tasks"], context: { instruction: "回答要简短", permissions: ["session.read"] } },
    archived: false,
  });
  expect(bot.botId).toBe("support-1");
  expect(bot.defaultModel.providerId).toBe("magpie");
  expect(bot.harness.tools).toEqual(["bash", "tasks"]);
  expect(bot.harness.context.permissions).toEqual(["session.read"]);
  const bare = parseBot({ botId: "x", name: "X", profileId: "p", modelKind: "text" });
  expect(bare.harness.tools).toEqual([]);
  expect(bare.defaultModel).toEqual({ providerId: "", modelId: "" });
});

test("team bot: mission + roster; a seat without a bot id is dropped", () => {
  const team = parseTeamBot({
    teamBotId: "support-team",
    name: "客服团队",
    mission: "24 小时内解决客户问题",
    roster: [
      { botId: "support-1", role: "captain" },
      { botId: "", role: "member" },
      { botId: "image-1", role: "member" },
    ],
    revision: 4,
    hash: "h-4",
  });
  expect(team.mission).toBe("24 小时内解决客户问题");
  expect(team.roster.map((seat) => seat.botId)).toEqual(["support-1", "image-1"]);
  expect(team.revision).toBe(4);
  expect(team.hash).toBe("h-4");
});

test("bot key: metadata only (no plaintext field exists on the wire shape)", () => {
  const key = parseBotKey({
    keyId: "k-1",
    prefix: "keel_bot_abc",
    accountId: "a-1",
    botId: "support-1",
    label: "CI",
    createdAt: "2026-11-22T00:00:00Z",
    revokedAt: null,
    lastUsedAt: "2026-11-22T01:00:00Z",
  });
  expect(key.keyId).toBe("k-1");
  expect(key.prefix).toBe("keel_bot_abc");
  expect(key.revokedAt).toBe(undefined);
  expect(key.lastUsedAt).toBe("2026-11-22T01:00:00Z");
});

test("group detail: grants keep only known scopes; members keep their account", () => {
  const detail = parsePermissionGroupDetail({
    group: { groupId: "g-1", name: "bot-operator", builtin: true },
    grants: [
      { key: "session.read", scope: "own" },
      { key: "bot.invoke", scope: "wat" },
    ],
    members: [{ accountId: "a-1", addedBy: "a-2" }, { accountId: "" }],
  });
  expect(detail.group.builtin).toBe(true);
  expect(detail.grants).toEqual([{ key: "session.read", scope: "own" }]);
  expect(detail.members.map((member) => member.accountId)).toEqual(["a-1"]);
});

test("pagination: a full page is a lower bound on more, not a total", () => {
  expect(identityPage(["a"], 500)).toEqual({ items: ["a"], limit: 500, hasMore: false });
  expect(identityPage(["a", "b"], 2).hasMore).toBe(true);
  expect(identityPage([], 2).hasMore).toBe(false);
});
