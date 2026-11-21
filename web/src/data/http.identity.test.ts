// The identity transport: CSRF on every write, error detail (403 reason, 429
// Retry-After), bounded paging and the exact paths/bodies the server expects.
// The real KeelHttp parsing runs; only fetch is stubbed.
import { expect, test } from "nsh/test";
import { KeelApiError, KeelHttp, errorMessageOf, type FetchLike } from "./http";

interface Call {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: string;
}

const json = (status: number, body: unknown, headers: Record<string, string> = {}): Response =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json", ...headers },
  });

/** A fetch stub: one handler per `METHOD path` (before the query string). */
const stub = (handlers: Record<string, (call: Call) => Response>) => {
  const calls: Call[] = [];
  const fetchImpl: FetchLike = async (input, init) => {
    const url = String(input);
    const method = (init?.method ?? "GET").toUpperCase();
    const body = typeof init?.body === "string" ? init.body : "";
    const call: Call = {
      url,
      method,
      headers: (init?.headers as Record<string, string> | undefined) ?? {},
      body,
    };
    calls.push(call);
    const path = url.split("?")[0] ?? url;
    const handler = handlers[`${method} ${path}`];
    if (!handler) return json(404, { error: "not found" });
    return handler(call);
  };
  return { fetchImpl, calls };
};

test("auth: the anonymous session token authorizes the login POST; the session's token authorizes writes", async () => {
  const { fetchImpl, calls } = stub({
    "GET /v1/auth/session": () =>
      json(200, { authenticated: false, mode: "required", bootstrapRequired: false, csrfToken: "anon-csrf" }),
    "POST /v1/auth/login": (call) => {
      expect(call.headers["X-Keel-CSRF"]).toBe("anon-csrf");
      expect(JSON.parse(call.body)).toEqual({ username: "admin", password: "s3cret-passphrase" });
      return json(200, {
        authenticated: true,
        mode: "required",
        csrfToken: "session-csrf",
        account: { id: "a-1", username: "admin", isSuperAdmin: true },
        mustChangePassword: false,
      });
    },
    "POST /v1/accounts": (call) => {
      expect(call.headers["X-Keel-CSRF"]).toBe("session-csrf");
      expect(JSON.parse(call.body)).toEqual({ username: "bob", password: "another-passphrase" });
      return json(201, {
        account: { accountId: "a-2", username: "bob", displayName: "bob", kind: "human", isSuperAdmin: false },
      });
    },
  });
  const http = new KeelHttp("", fetchImpl);
  const session = await http.authSession();
  expect(session.csrfToken).toBe("anon-csrf");
  const signedIn = await http.login("admin", "s3cret-passphrase");
  expect(signedIn.authenticated).toBe(true);
  expect(http.csrf()).toBe("session-csrf");
  const account = await http.createAccount({ username: "bob", password: "another-passphrase" });
  expect(account.accountId).toBe("a-2");
  // No write ran without a token.
  expect(calls.filter((call) => call.method !== "GET").every((call) => call.headers["X-Keel-CSRF"])).toBe(true);
});

test("auth: login without a prior session read fetches the anonymous CSRF token first", async () => {
  const { fetchImpl, calls } = stub({
    "GET /v1/auth/session": () => json(200, { authenticated: false, csrfToken: "anon" }),
    "POST /v1/auth/login": (call) => {
      expect(call.headers["X-Keel-CSRF"]).toBe("anon");
      return json(200, { authenticated: true, csrfToken: "next", mustChangePassword: true });
    },
  });
  const http = new KeelHttp("", fetchImpl);
  const session = await http.login("admin", "pass");
  expect(session.mustChangePassword).toBe(true);
  expect(calls.map((call) => call.method + " " + call.url.split("?")[0])).toEqual([
    "GET /v1/auth/session",
    "POST /v1/auth/login",
  ]);
});

test("auth: a 401 is one shape for an unknown account and a wrong password", async () => {
  const { fetchImpl } = stub({
    "GET /v1/auth/session": () => json(200, { authenticated: false, csrfToken: "anon" }),
    "POST /v1/auth/login": () => json(401, { error: "invalid username or password", code: "invalid_credentials" }),
  });
  const http = new KeelHttp("", fetchImpl);
  let refusal: KeelApiError | null = null;
  try {
    await http.login("nobody", "wrong");
  } catch (error) {
    refusal = error as KeelApiError;
  }
  expect(refusal?.status).toBe(401);
  expect(refusal?.code).toBe("invalid_credentials");
  expect(errorMessageOf(refusal)).toBe("登录已失效，请重新登录。");
});

test("403 keeps the engine's reason and required key", async () => {
  const { fetchImpl } = stub({
    "GET /v1/bots": () =>
      json(403, { error: "forbidden", code: "forbidden", reason: "missing_grant", requiredKey: "bot.read" }),
  });
  const http = new KeelHttp("", fetchImpl);
  let refusal: KeelApiError | null = null;
  try {
    await http.listBots();
  } catch (error) {
    refusal = error as KeelApiError;
  }
  expect(refusal?.status).toBe(403);
  expect(refusal?.reason).toBe("missing_grant");
  expect(refusal?.requiredKey).toBe("bot.read");
  expect(errorMessageOf(refusal)).toContain("bot.read");
});

test("429 carries Retry-After as whole seconds", async () => {
  const { fetchImpl } = stub({
    "GET /v1/auth/session": () => json(200, { authenticated: false, csrfToken: "anon" }),
    "POST /v1/auth/login": () => json(429, { error: "too many login attempts" }, { "Retry-After": "42" }),
  });
  const http = new KeelHttp("", fetchImpl);
  let refusal: KeelApiError | null = null;
  try {
    await http.login("admin", "wrong");
  } catch (error) {
    refusal = error as KeelApiError;
  }
  expect(refusal?.status).toBe(429);
  expect(refusal?.retryAfterSeconds).toBe(42);
  expect(errorMessageOf(refusal)).toContain("42");
});

test("listings: the limit is clamped to the API's 1..500 and includeArchived is explicit", async () => {
  const seen: string[] = [];
  const { fetchImpl } = stub({
    "GET /v1/bots": (call) => {
      seen.push(call.url);
      return json(200, { bots: [{ botId: "b1", name: "B", profileId: "p", modelKind: "text" }] });
    },
  });
  const http = new KeelHttp("", fetchImpl);
  const page = await http.listBots(999, true);
  expect(page.limit).toBe(500);
  expect(page.hasMore).toBe(false);
  expect(page.items.map((bot) => bot.botId)).toEqual(["b1"]);
  expect(seen[0]).toContain("limit=500");
  expect(seen[0]).toContain("includeArchived=1");
});

test("listings: a full page reports hasMore as a lower bound", async () => {
  const { fetchImpl } = stub({
    "GET /v1/accounts": () =>
      json(200, {
        accounts: [
          { accountId: "a1", username: "a1", displayName: "A1", kind: "human", isSuperAdmin: false },
          { accountId: "a2", username: "a2", displayName: "A2", kind: "human", isSuperAdmin: false },
        ],
      }),
  });
  const http = new KeelHttp("", fetchImpl);
  const page = await http.listAccounts(2);
  expect(page.items.length).toBe(2);
  expect(page.hasMore).toBe(true);
});

test("writes: team roster CAS, grant scope query and one-time bot key plaintext", async () => {
  const { fetchImpl, calls } = stub({
    "POST /v1/team-bots/support-team/roster": (call) => {
      expect(JSON.parse(call.body)).toEqual({
        roster: [{ botId: "support-1", role: "captain" }],
        expectedRevision: 3,
      });
      return json(200, {
        teamBot: { teamBotId: "support-team", name: "客服", mission: "m", revision: 4, roster: [] },
      });
    },
    "DELETE /v1/permission-groups/g-1/grants/bot.invoke": (call) => {
      expect(call.url).toContain("scope=tenant");
      return json(200, { removed: true });
    },
    "POST /v1/bots/support-1/keys": (call) => {
      expect(JSON.parse(call.body)).toEqual({ label: "CI" });
      return json(201, {
        key: { keyId: "k-1", prefix: "keel_bot_ab", accountId: "a-1", botId: "support-1", label: "CI" },
        plaintext: "keel_bot_abc123",
        notice: "shown once",
      });
    },
  });
  const http = new KeelHttp("", fetchImpl);
  // A signed-in client's token; only the paths/bodies matter here.
  (http as unknown as { csrfToken: string }).csrfToken = "csrf";
  const team = await http.updateTeamBotRoster("support-team", [{ botId: "support-1", role: "captain" }], 3);
  expect(team.revision).toBe(4);
  await http.removePermissionGroupGrant("g-1", "bot.invoke", "tenant");
  const issued = await http.issueBotKey("support-1", "CI");
  expect(issued.plaintext).toBe("keel_bot_abc123");
  expect(calls.length).toBe(3);
});

test("whoami: parsed identity is the can() source", async () => {
  const { fetchImpl } = stub({
    "GET /v1/auth/whoami": () =>
      json(200, {
        tenantId: "t-1",
        account: { id: "a-1", username: "admin", isSuperAdmin: true },
        permissions: { "session.read": "own" },
      }),
  });
  const http = new KeelHttp("", fetchImpl);
  const me = await http.whoami();
  expect(me.accountId).toBe("a-1");
  expect(me.permissions["session.read"]).toBe("own");
});
