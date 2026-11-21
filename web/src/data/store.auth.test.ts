// The store's auth stage machine against the real KeelHttp: login loads whoami
// into `can()`, must-change holds the gate, logout clears every local trace.
import { expect, test } from "nsh/test";
import { KeelHttp, type FetchLike } from "./http";
import { can, effectivePermissions } from "./permissions";
import { store } from "./store";

const json = (status: number, body: unknown): Response =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

let mustChange = false;
let whoamiPermissions: Record<string, string> = { "session.read": "own" };
let expired = false;

const fetchImpl: FetchLike = async (input, init) => {
  const path = String(input).split("?")[0];
  const method = (init?.method ?? "GET").toUpperCase();
  switch (`${method} ${path}`) {
    case "GET /v1/auth/session":
      return json(200, { authenticated: false, mode: "required", csrfToken: "anon" });
    case "POST /v1/auth/login":
      return json(200, {
        authenticated: true,
        mode: "required",
        csrfToken: "session-csrf",
        mustChangePassword: mustChange,
        tenantId: "t-1",
        account: { id: "a-1", username: "admin", displayName: "管理员", isSuperAdmin: false },
      });
    case "GET /v1/auth/whoami":
      return json(200, {
        tenantId: "t-1",
        account: { id: "a-1", username: "admin", displayName: "管理员", kind: "human", isSuperAdmin: false },
        permissions: whoamiPermissions,
      });
    case "GET /v1/capabilities":
      return json(200, { features: {} });
    case "GET /v1/sessions":
      return expired ? json(401, { error: "authentication required", code: "invalid_session" }) : json(200, { sessions: [] });
    case "POST /v1/auth/logout":
      return json(200, { authenticated: false });
    default:
      return json(404, { error: "not found" });
  }
};

const prepare = () => {
  mustChange = false;
  whoamiPermissions = { "session.read": "own" };
  expired = false;
  store.useHTTPForTest(new KeelHttp("", fetchImpl));
  store.resetAuthForTest();
  store.auth.set("required");
};

test("login: whoami feeds can(); an unheld key stays hidden", async () => {
  prepare();
  expect(can("session.read")).toBe(false); // fail closed before the read
  const ok = await store.login("admin", "correct-password-123");
  expect(ok).toBe(true);
  expect(store.auth()).toBe("ok");
  expect(store.identity()?.username).toBe("admin");
  expect(can("session.read")).toBe(true);
  expect(can("bot.manage")).toBe(false);
  expect(effectivePermissions()).toEqual({ "session.read": "own" });
});

test("must-change: login holds the gate and loads no permissions", async () => {
  prepare();
  mustChange = true;
  const ok = await store.login("admin", "correct-password-123");
  expect(ok).toBe(true);
  expect(store.auth()).toBe("must-change");
  expect(can("session.read")).toBe(false);
});

test("logout: the server session and every local fact are cleared", async () => {
  prepare();
  await store.login("admin", "correct-password-123");
  expect(can("session.read")).toBe(true);
  await store.logout();
  expect(store.auth()).toBe("required");
  expect(store.identity()).toBe(null);
  expect(store.sessions()).toEqual([]);
  expect(can("session.read")).toBe(false);
  expect(store.authNotice()).toContain("已安全退出登录");
});

test("session expiry: a 401 returns to the gate with a reason, not a blank list", async () => {
  prepare();
  await store.login("admin", "correct-password-123");
  expired = true;
  await store.refreshSessions();
  expect(store.auth()).toBe("required");
  expect(store.identity()).toBe(null);
  expect(store.sessions()).toEqual([]);
  expect(store.authNotice()).toContain("会话已过期");
});
