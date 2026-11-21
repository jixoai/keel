// TUI acceptance: the auth gate's five states and the two credential flows.
//
//   nsh test --target tui
//
// The harness answers the server in memory through the real `KeelHttp` (only
// fetch is stubbed), so these cases exercise the request shapes, the CSRF
// token, the error mapping and the stage machine — not a second UI.
import { expect, open, test } from "nsh/test";

const openGate = (scenario: string) =>
  open("./login.harness.tsx", { host: { params: { scenario } } });

const signIn = async (app: Awaited<ReturnType<typeof openGate>>, password = "correct-password-123") => {
  await app.waitFor("id:login-username");
  await app.type("id:login-username", "admin");
  await app.type("id:login-password", password);
  await app.press("id:login-submit");
};

test("登录页：账号密码登录成功即进入应用，旧口令文案不再出现", async () => {
  const app = await openGate("login");
  await app.waitFor("id:login-username");
  expect(app.texts().join("\n")).not.toContain("KEEL_SERVE_PASSWORD");
  expect(app.texts().join("\n")).not.toContain("服务口令");
  await signIn(app);
  await app.waitFor("text:已进入应用");
  expect(app.shows("id:login-username")).toBe(false);
});

test("登录页：错误凭据只说一次人话，不区分账号不存在与密码错误", async () => {
  const app = await openGate("login");
  await signIn(app, "wrong-password");
  await app.waitFor("text:/用户名或密码不正确/");
  expect(app.shows("id:login-username")).toBe(true);
  const page = app.texts().join("\n");
  expect(page).toContain("用户名或密码不正确");
  expect(page).not.toContain("用户不存在");
  expect(page).not.toContain("密码错误，");
});

test("登录页：429 显示等待秒数", async () => {
  const app = await openGate("rate-limit");
  await signIn(app);
  await app.waitFor("text:/尝试次数过多，请在 30 秒后重试/");
  expect(app.shows("id:login-username")).toBe(true);
});

test("引导：setup token + 用户名 + 密码 → 强制改密 → 应用", async () => {
  const app = await openGate("bootstrap");
  await app.waitFor("id:bootstrap-token");
  await app.type("id:bootstrap-token", "setup-token-123");
  await app.type("id:bootstrap-username", "admin");
  await app.type("id:bootstrap-password", "new-password-123");
  await app.type("id:bootstrap-confirm", "new-password-123");
  await app.press("id:bootstrap-submit");
  await app.waitFor("id:change-current");
  expect(app.texts()).toContain("设置新密码");
  await app.type("id:change-current", "new-password-123");
  await app.type("id:change-new", "changed-password-456");
  await app.type("id:change-confirm", "changed-password-456");
  await app.press("id:change-submit");
  await app.waitFor("text:已进入应用");
});

test("强制改密门：当前密码错了仍停在门内，改对才进入", async () => {
  const app = await openGate("must-change");
  await app.waitFor("id:change-current");
  await app.type("id:change-current", "not-the-password");
  await app.type("id:change-new", "changed-password-456");
  await app.type("id:change-confirm", "changed-password-456");
  await app.press("id:change-submit");
  await app.waitFor("text:/当前密码不正确/");
  expect(app.shows("id:change-current")).toBe(true);

  await app.clear("id:change-current");
  await app.type("id:change-current", "new-password-123");
  await app.press("id:change-submit");
  await app.waitFor("text:已进入应用");
});

test("CSRF：匿名会话没有令牌时登录写请求被拒，页面给出可行动原因", async () => {
  const app = await openGate("csrf");
  await signIn(app);
  await app.waitFor("text:/安全令牌已失效/");
  expect(app.shows("id:login-username")).toBe(true);
});

test("权限不足：403 显示权限面，而不是通用失败", async () => {
  const app = await openGate("denied");
  await signIn(app);
  await app.waitFor("id:auth-denied");
  expect(app.texts().join("\n")).toContain("权限不足");
});

test("登出：清掉本地状态并回到登录页，留下成功提示", async () => {
  const app = await openGate("login");
  await signIn(app);
  await app.waitFor("text:已进入应用");
  await app.press("id:harness-logout");
  await app.waitFor("text:/已安全退出登录/");
  expect(app.shows("id:login-username")).toBe(true);
});

test("会话过期：401 回到登录页并说明原因，不留下白屏", async () => {
  const app = await openGate("login");
  await signIn(app);
  await app.waitFor("text:已进入应用");
  await app.press("id:harness-expire");
  await app.waitFor("text:/会话已过期，请重新登录/");
  expect(app.shows("id:login-username")).toBe(true);
  expect(app.shows("id:harness-signed-in")).toBe(false);
});

test("加载态：会话文档未返回时是加载面，不是空表单", async () => {
  const app = await openGate("pending");
  await app.waitFor("id:auth-loading");
  expect(app.shows("id:login-username")).toBe(false);
});
