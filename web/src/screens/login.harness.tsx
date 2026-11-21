// TUI harness · the auth gate alone, with the real `KeelHttp` and only `fetch`
// stubbed. The scenario comes from the launch parameters
// (`host.params.scenario`):
//
//   login        an ordinary deployment; `admin` / `correct-password-123`
//   rate-limit   the login endpoint answers 429 with Retry-After: 30
//   bootstrap    no super admin: the setup form, then the forced change
//   must-change  an authenticated session whose password must be replaced
//   denied       the login endpoint answers 403 (not CSRF): the permission face
//   csrf         the anonymous session carries no token, so the login POST is
//                refused with csrf_failed (a client that lost the cookie)
//   pending      the session document never answers (the loading state)
//
// Every route the store touches after a successful sign-in is answered:
// capabilities, whoami and the session list. A route the gate does not need is
// left out on purpose, so a test catches a request it did not expect.
import { host, mount, Text, View } from "nsh";
import { Button } from "../ui/kit";
import { KeelHttp, type FetchLike } from "../data/http";
import { store } from "../data/store";
import { LoginScreen } from "./login";

const scenario = (): string => String(host.params.scenario ?? "login");

const json = (status: number, body: unknown, headers: Record<string, string> = {}): Response =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json", ...headers },
  });

const nothing = (): Promise<Response> => new Promise<Response>(() => {});

const password = "correct-password-123";
const setupPassword = "new-password-123";
const changedPassword = "changed-password-456";
/** Set by the harness's expiry button: the next session-list read is a 401. */
let expired = false;

const sessionBody = () => {
  switch (scenario()) {
    case "must-change":
      return {
        authenticated: true,
        mode: "required",
        csrfToken: "session-csrf",
        mustChangePassword: true,
        tenantId: "t-1",
        account: { id: "a-1", username: "admin", displayName: "管理员", isSuperAdmin: true },
        sessionId: "s-1",
      };
    case "bootstrap":
      return { authenticated: false, mode: "required", bootstrapRequired: true, csrfToken: "anon-csrf" };
    case "csrf":
      // No token: the login POST will carry no header, exactly like a browser
      // whose CSRF cookie was lost.
      return { authenticated: false, mode: "required", bootstrapRequired: false };
    default:
      return { authenticated: false, mode: "required", bootstrapRequired: false, csrfToken: "anon-csrf" };
  }
};

const stub: FetchLike = async (input, init) => {
  const url = String(input);
  const path = url.split("?")[0] ?? url;
  const method = (init?.method ?? "GET").toUpperCase();
  const body = typeof init?.body === "string" ? (JSON.parse(init.body) as Record<string, unknown>) : {};
  const headers = (init?.headers as Record<string, string> | undefined) ?? {};
  const route = `${method} ${path}`;

  switch (route) {
    case "GET /v1/auth/session":
      if (scenario() === "pending") return nothing();
      return json(200, sessionBody());
    case "POST /v1/auth/login":
      if (scenario() === "rate-limit") {
        return json(429, { error: "too many login attempts", code: "rate_limited" }, { "Retry-After": "30" });
      }
      if (scenario() === "denied") {
        return json(403, { error: "forbidden", code: "bot_browser_forbidden" });
      }
      if (scenario() === "csrf") {
        return headers["X-Keel-CSRF"] === "anon-csrf"
          ? json(200, { authenticated: true, csrfToken: "session-csrf", mustChangePassword: false })
          : json(403, { error: "a valid X-Keel-CSRF header is required for writes", code: "csrf_failed" });
      }
      if (body.username !== "admin" || body.password !== (scenario() === "bootstrap" ? setupPassword : password)) {
        return json(401, { error: "invalid username or password", code: "invalid_credentials" });
      }
      return json(200, {
        authenticated: true,
        mode: "required",
        csrfToken: "session-csrf",
        mustChangePassword: scenario() === "bootstrap",
        tenantId: "t-1",
        account: { id: "a-1", username: "admin", displayName: "管理员", isSuperAdmin: false },
      });
    case "POST /v1/auth/bootstrap":
      if (body.token !== "setup-token-123") {
        return json(401, { error: "the setup token is invalid", code: "invalid_setup_token" });
      }
      return json(200, { bootstrapped: true, mustChangePassword: true });
    case "POST /v1/auth/password":
      if (body.currentPassword !== setupPassword) {
        return json(401, { error: "the current password is not valid", code: "invalid_credentials" });
      }
      return json(200, {
        authenticated: true,
        mode: "required",
        csrfToken: "session-csrf-2",
        mustChangePassword: false,
        tenantId: "t-1",
        account: { id: "a-1", username: "admin", displayName: "管理员", isSuperAdmin: false },
      });
    case "POST /v1/auth/logout":
      return json(200, { authenticated: false });
    case "GET /v1/auth/whoami":
      return json(200, {
        tenantId: "t-1",
        account: { id: "a-1", username: "admin", displayName: "管理员", kind: "human", isSuperAdmin: false },
        permissions: { "session.read": "own" },
      });
    case "GET /v1/capabilities":
      return json(200, { features: {} });
    case "GET /v1/sessions":
      return expired
        ? json(401, { error: "authentication required", code: "invalid_session" })
        : json(200, { sessions: [] });
    default:
      return json(404, { error: "not found" });
  }
};

const http = new KeelHttp("", stub);
store.useHTTPForTest(http);
void store.reconnect();

function Harness() {
  return (
    <View style={{ flex: 1, flexDirection: "column", padding: 8, gap: 8 }}>
      {/* A signed-in session replaces the gate with the product; the harness
          stands in for the product with the one action this slice owns. */}
      <If when={store.auth() === "ok"}>
        <View testID="harness-signed-in" style={{ flexDirection: "column", gap: 8 }}>
          <Text testID="harness-signed-in-label">已进入应用</Text>
          <Button
            testID="harness-logout"
            variant="outline"
            label="退出登录"
            accessibilityLabel="退出登录"
            onPress={() => void store.logout()}
          />
          <Button
            testID="harness-expire"
            variant="outline"
            label="模拟会话过期"
            accessibilityLabel="模拟会话过期"
            onPress={() => {
              expired = true;
              void store.refreshSessions();
            }}
          />
        </View>
      </If>
      <LoginScreen />
    </View>
  );
}

mount(Harness);
