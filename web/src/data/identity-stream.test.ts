// The identity invalidation stream's client behavior — change keel-harness-v2.
//
// These cases pin the acceptance facts the server tests cannot: a fact re-reads
// the loaded projections without a manual refresh, a burst coalesces into one
// re-read, heartbeats do not cause requests, and a 401 closes the stream for
// good (no reconnect with the old cookie) while returning to the login gate.
import { expect, test } from "nsh/test";
import { botAccess } from "./bots";
import { KeelHttp, type FetchLike } from "./http";
import { identityAccess } from "./identity-access";
import { identityStream, resetIdentityStreamForTest } from "./identity-stream";
import { can, setEffectivePermissions } from "./permissions";
import { resetSharesForTest } from "./shares";
import { store } from "./store";

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

const waitFor = async (predicate: () => boolean, what: string, timeoutMs = 2000): Promise<number> => {
  const start = Date.now();
  for (;;) {
    if (predicate()) return Date.now() - start;
    if (Date.now() - start > timeoutMs) throw new Error(`timed out waiting for ${what}`);
    await sleep(5);
  }
};

const json = (status: number, body: unknown) => ({
  ok: status >= 200 && status < 300,
  status,
  headers: { get: () => null },
  json: async () => body,
  text: async () => JSON.stringify(body),
});

const frame = (kind: string): string =>
  `event: identity\ndata: ${JSON.stringify({ kind, at: new Date().toISOString() })}\n\n`;

/** A live SSE body whose chunks the case pushes by hand. */
const liveStream = () => {
  const chunks: string[] = [];
  let notify: (() => void) | null = null;
  let closed = false;
  return {
    body: {
      getReader: () => ({
        read: async () => {
          for (;;) {
            if (chunks.length) {
              const text = chunks.shift() as string;
              return { done: false, value: new Uint8Array([...text].map((c) => c.charCodeAt(0))) };
            }
            if (closed) return { done: true, value: undefined };
            await new Promise<void>((resolve) => {
              notify = resolve;
            });
          }
        },
        releaseLock: () => {},
        cancel: async () => {},
      }),
    },
    push: (text: string) => {
      chunks.push(text);
      notify?.();
      notify = null;
    },
    close: () => {
      closed = true;
      notify?.();
      notify = null;
    },
  };
};

const setup = (
  fetchImpl: FetchLike,
  permissions: Record<string, "own" | "team" | "tenant" | "all"> = { "app.read": "tenant" },
): void => {
  resetIdentityStreamForTest();
  identityAccess.resetForTest();
  botAccess.resetForTest();
  resetSharesForTest();
  store.skipBootForTest();
  store.useHTTPForTest(new KeelHttp("", fetchImpl));
  store.resetAuthForTest();
  setEffectivePermissions(permissions, false);
  store.auth.set("ok");
};

test("identity stream: an accounts fact re-reads accounts within 2s, once per burst", async () => {
  const stream = liveStream();
  let streamRequests = 0;
  let accountCalls = 0;
  const fetchImpl = (async (input: unknown) => {
    const path = String(input).split("?")[0];
    if (path === "/v1/identity/events") {
      streamRequests += 1;
      return { ok: true, status: 200, headers: { get: () => null }, body: stream.body };
    }
    if (path === "/v1/accounts") {
      accountCalls += 1;
      return json(200, {
        accounts: [
          {
            accountId: "acc-1",
            username: "lina",
            displayName: "李娜",
            kind: "human",
            isSuperAdmin: false,
          },
        ],
      });
    }
    return json(404, { error: "not found" });
  }) as unknown as FetchLike;

  setup(fetchImpl, { "app.read": "tenant", "account.read": "tenant" });
  await identityAccess.loadAccounts();
  expect(accountCalls).toBe(1);

  identityStream.sync("ok");
  expect(streamRequests).toBeGreaterThanOrEqual(1);
  expect(identityStream.state()).toBe("connecting");

  const elapsed = await waitFor(() => accountCalls === 1 && identityStream.state() === "open", "stream open");
  expect(elapsed).toBeLessThan(2000);

  stream.push(frame("accounts"));
  const latency = await waitFor(() => accountCalls === 2, "accounts re-read");
  expect(latency).toBeLessThan(2000);

  // A burst of the same fact inside one coalescing window is one request.
  stream.push(frame("accounts"));
  stream.push(frame("accounts"));
  stream.push(frame("accounts"));
  await waitFor(() => accountCalls === 3, "coalesced re-read");
  await sleep(120);
  expect(accountCalls).toBe(3);

  identityStream.stop();
  expect(identityStream.state()).toBe("closed");
});

test("identity stream: a permissions fact re-reads whoami and revokes the local capability", async () => {
  const stream = liveStream();
  let whoamiCalls = 0;
  let groupCalls = 0;
  let accountCalls = 0;
  let whoamiPermissions: Record<string, string> = {
    "app.read": "tenant",
    "account.read": "tenant",
    "permission.read": "tenant",
    "session.read": "own",
  };
  const fetchImpl = (async (input: unknown) => {
    const path = String(input).split("?")[0];
    if (path === "/v1/identity/events") {
      return { ok: true, status: 200, headers: { get: () => null }, body: stream.body };
    }
    if (path === "/v1/auth/whoami") {
      whoamiCalls += 1;
      return json(200, {
        tenantId: "t-1",
        account: { id: "acc-1", username: "lina", displayName: "李娜", kind: "human" },
        permissions: whoamiPermissions,
      });
    }
    if (path === "/v1/accounts") {
      accountCalls += 1;
      return json(200, { accounts: [] });
    }
    if (path === "/v1/permission-groups") {
      groupCalls += 1;
      return json(200, { groups: [] });
    }
    return json(404, { error: "not found" });
  }) as unknown as FetchLike;

  setup(fetchImpl, {
    "app.read": "tenant",
    "account.read": "tenant",
    "permission.read": "tenant",
  });
  await store.loadIdentity();
  expect(whoamiCalls).toBe(1);
  identityAccess.ensureIndexes();
  await waitFor(() => !identityAccess.accountsLoading() && !identityAccess.groupsLoading(), "indexes");
  expect(accountCalls).toBe(1);
  expect(groupCalls).toBe(1);

  identityStream.sync("ok");
  await waitFor(() => identityStream.state() === "open", "stream open");

  // The server's next whoami answer no longer carries session.read. The fact
  // alone must make the UI's can() follow the server within 2s.
  whoamiPermissions = {
    "app.read": "tenant",
    "account.read": "tenant",
    "permission.read": "tenant",
  };
  stream.push(frame("permissions"));
  const latency = await waitFor(() => whoamiCalls >= 2, "whoami re-read");
  expect(latency).toBeLessThan(2000);
  await waitFor(() => identityAccess.groupsLoading() === false && groupCalls >= 2, "groups re-read");
  expect(groupCalls).toBeGreaterThanOrEqual(2);
  expect(accountCalls).toBeGreaterThanOrEqual(2);
  // loadIdentity recorded the new effective set: the revoked capability is no
  // longer offered, while app.read keeps the stream alive.
  expect(can("session.read")).toBe(false);
  expect(can("app.read")).toBe(true);

  identityStream.stop();
});

test("identity stream: a sessions fact refreshes the session list; heartbeats do nothing", async () => {
  const stream = liveStream();
  let sessionCalls = 0;
  const fetchImpl = (async (input: unknown) => {
    const path = String(input).split("?")[0];
    if (path === "/v1/identity/events") {
      return { ok: true, status: 200, headers: { get: () => null }, body: stream.body };
    }
    if (path === "/v1/sessions") {
      sessionCalls += 1;
      return json(200, { sessions: [] });
    }
    return json(404, { error: "not found" });
  }) as unknown as FetchLike;

  setup(fetchImpl);
  await store.refreshSessions();
  expect(sessionCalls).toBe(1);

  identityStream.sync("ok");
  await waitFor(() => identityStream.state() === "open", "stream open");

  // Keepalives are comments: they must not wake a single request.
  stream.push(": heartbeat 2026-11-22T00:00:00Z\n\n");
  await sleep(120);
  expect(sessionCalls).toBe(1);

  stream.push(frame("sessions"));
  expect(await waitFor(() => sessionCalls === 2, "session list re-read")).toBeLessThan(2000);

  stream.push(frame("sessions"));
  stream.push(frame("sessions"));
  await waitFor(() => sessionCalls === 3, "coalesced session re-read");
  await sleep(120);
  expect(sessionCalls).toBe(3);

  identityStream.stop();
});

test("identity stream: a reconnect re-reads loaded projections (facts are not replayed)", async () => {
  const streams: Array<ReturnType<typeof liveStream>> = [];
  let streamRequests = 0;
  let sessionCalls = 0;
  const fetchImpl = (async (input: unknown) => {
    const path = String(input).split("?")[0];
    if (path === "/v1/identity/events") {
      streamRequests += 1;
      const stream = liveStream();
      streams.push(stream);
      return { ok: true, status: 200, headers: { get: () => null }, body: stream.body };
    }
    if (path === "/v1/auth/whoami") {
      return json(200, {
        tenantId: "t-1",
        account: { id: "acc-1", username: "lina", displayName: "李娜", kind: "human" },
        permissions: { "app.read": "tenant" },
      });
    }
    if (path === "/v1/sessions") {
      sessionCalls += 1;
      return json(200, { sessions: [] });
    }
    return json(404, { error: "not found" });
  }) as unknown as FetchLike;

  setup(fetchImpl);
  await store.refreshSessions();
  expect(sessionCalls).toBe(1);

  identityStream.sync("ok");
  await waitFor(() => identityStream.state() === "open", "stream open");
  // The server/proxy drops the connection; the stream reconnects with backoff.
  streams[0].close();
  await waitFor(() => streamRequests >= 2 && identityStream.state() === "open", "reconnect", 4000);
  // On the second open every loaded projection is re-read once: the gap's facts
  // are not replayed by the server.
  await waitFor(() => sessionCalls >= 2, "resync after reconnect", 4000);

  identityStream.stop();
});

test("identity stream: a 401 closes it, never reconnects, and returns to the login gate", async () => {
  let streamRequests = 0;
  const fetchImpl = (async (input: unknown) => {
    const path = String(input).split("?")[0];
    if (path === "/v1/identity/events") {
      streamRequests += 1;
      return { ok: false, status: 401, headers: { get: () => null }, body: null };
    }
    return json(404, { error: "not found" });
  }) as unknown as FetchLike;

  setup(fetchImpl);
  expect(store.auth()).toBe("ok");
  identityStream.sync("ok");
  await waitFor(() => identityStream.state() === "closed", "stream closed");
  expect(streamRequests).toBe(1);
  expect(store.auth()).toBe("required");
  expect(store.identity()).toBe(null);

  // The reconnect backoff would have fired by now; a dead cookie must not.
  await sleep(900);
  expect(streamRequests).toBe(1);
  expect(identityStream.state()).toBe("closed");
});

test("identity stream: logout closes the transport before the next account", async () => {
  const stream = liveStream();
  let streamRequests = 0;
  const fetchImpl = (async (input: unknown) => {
    const path = String(input).split("?")[0];
    if (path === "/v1/identity/events") {
      streamRequests += 1;
      return { ok: true, status: 200, headers: { get: () => null }, body: stream.body };
    }
    return json(200, { authenticated: false });
  }) as unknown as FetchLike;

  setup(fetchImpl);
  identityStream.sync("ok");
  await waitFor(() => identityStream.state() === "open", "stream open");

  // The store's session teardown closes the stream through the installed
  // closer (the shell is gone; the stream must not outlive the cookie).
  await store.logout();
  expect(identityStream.state()).toBe("closed");
  expect(store.auth()).toBe("required");
  expect(streamRequests).toBe(1);
});
