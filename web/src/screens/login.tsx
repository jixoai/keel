// The auth gate (change keel-bot-identity-access, A4.3).
//
// One screen, four forms, chosen by the store's stage:
//
//   required     username + password → the server session
//   bootstrap    the deployment has no super admin: one-time setup token +
//                username + password, then the forced password change
//   must-change  the account is signed in but the server allows only the
//                password change until it has a new credential
//   unknown/off  the boot probe is running, or there is no live service
//
// The five states a page must have are here: loading (the probe), empty (the
// bootstrap form — no accounts exist yet), error (a refused credential or a
// dead service, with the reason and a retry), permission (a 403 that is not
// CSRF: the account may not use this surface) and success (the notice a logout
// or a password change leaves).
//
// The gate itself is a kit panel: a Card carrying Field/Input/Button. The only
// layout written here is the centring frame and the notice strip; control
// heights, radii, ink and padding stay the kit's, so the gate cannot drift
// from the rest of the app. A refused credential is the field's own error; a
// submit in flight is the Button's loading state.
import { signal, Text, View } from "nsh";
import { Button, Card, CardContent, CardHeader, Empty, EmptyContent, EmptyDescription, EmptyTitle, Field, Input, Spinner, color, space, typeScale } from "../ui/kit";
import { store } from "../data/store";

/** One quiet strip above the form: a success/notice or a refusal that belongs
 * to the whole gate rather than one field. */
function GateCallout(props: { tone: "success" | "warning" | "danger"; text: string; testID: string }) {
  const ink = () => (props.tone === "success" ? color.success : props.tone === "warning" ? color.warning : color.danger);
  const tint = () =>
    props.tone === "success" ? color.successTint : props.tone === "warning" ? color.warningTint : color.dangerTint;
  return (
    <View
      testID={props.testID}
      accessibilityLabel={props.text}
      style={{
        backgroundColor: tint(),
        borderRadius: 6,
        paddingVertical: space.sm,
        paddingHorizontal: space.md,
      }}
    >
      <Text style={{ fontSize: typeScale.sm, color: ink() }}>{props.text}</Text>
    </View>
  );
}

export function LoginScreen() {
  // ---- the login form -----------------------------------------------------
  const username = signal("");
  const password = signal("");
  // ---- the bootstrap form -------------------------------------------------
  const setupToken = signal("");
  const setupUsername = signal("");
  const setupPassword = signal("");
  const setupConfirm = signal("");
  // ---- the must-change form ----------------------------------------------
  const currentPassword = signal("");
  const newPassword = signal("");
  const confirmPassword = signal("");
  // ---- per-form state -----------------------------------------------------
  const submitting = signal(false);
  const formError = signal<string | null>(null);

  const stage = () => store.auth();
  const checking = () => store.mode() === "checking" || (stage() === "unknown" && store.mode() !== "offline");
  const offline = () => store.mode() === "offline";
  const busy = () => submitting();
  const error = () => formError() ?? store.authError();
  /** The permission callout already says what is missing; a field must not
   * repeat the same sentence. */
  const fieldError = () => (store.authDenied() ? formError() : error());

  const reset = () => {
    formError.set(null);
  };

  const login = async () => {
    if (submitting()) return;
    reset();
    if (!username().trim() || !password()) {
      formError.set("请输入用户名和密码。");
      return;
    }
    submitting.set(true);
    await store.login(username(), password());
    submitting.set(false);
    if (store.auth() !== "required") password.set("");
  };

  const bootstrap = async () => {
    if (submitting()) return;
    reset();
    if (!setupToken().trim() || !setupUsername().trim() || !setupPassword()) {
      formError.set("请填写设置令牌、用户名和密码。");
      return;
    }
    if (setupPassword() !== setupConfirm()) {
      formError.set("两次输入的密码不一致。");
      return;
    }
    submitting.set(true);
    await store.bootstrap(setupToken(), setupUsername(), setupPassword());
    submitting.set(false);
    if (store.auth() !== "bootstrap") {
      setupToken.set("");
      setupPassword.set("");
      setupConfirm.set("");
    }
  };

  const changePassword = async () => {
    if (submitting()) return;
    reset();
    if (!currentPassword() || !newPassword() || !confirmPassword()) {
      formError.set("请填写当前密码、新密码和确认密码。");
      return;
    }
    if (newPassword() !== confirmPassword()) {
      formError.set("两次输入的新密码不一致。");
      return;
    }
    if (newPassword() === currentPassword()) {
      formError.set("新密码不能和当前密码相同。");
      return;
    }
    submitting.set(true);
    await store.changePassword(currentPassword(), newPassword());
    submitting.set(false);
    if (store.auth() !== "must-change") {
      currentPassword.set("");
      newPassword.set("");
      confirmPassword.set("");
    }
  };

  const title = () =>
    stage() === "bootstrap" ? "初始化部署" : stage() === "must-change" ? "设置新密码" : "登录 Keel";
  const description = () =>
    stage() === "bootstrap"
      ? "这个部署还没有管理员。用一次性设置令牌创建第一个超级管理员。"
      : stage() === "must-change"
        ? "首次登录需要先修改密码，改完即可进入应用。"
        : "用账号和密码进入；Bot 请使用绑定的 API key，不从网页登录。";

  return (
    <View
      style={{
        flex: 1,
        alignItems: "center",
        justifyContent: "center",
        backgroundColor: color.background,
        padding: space["2xl"],
      }}
    >
      <Card
        testID="login-gate"
        accessibilityLabel="登录 Keel"
        // shadcn's login column (`max-w-sm`, 24rem): the form never widens
        // into a page-wide panel on a large window.
        style={{ width: "100%", maxWidth: 400 }}
      >
        <CardHeader title={title()} description={description()} />
        <CardContent>
          <View style={{ flexDirection: "column", gap: space.lg }}>
            {/* success: a logout or a changed password */}
            <If when={store.authNotice() !== null}>
              <GateCallout tone="success" text={store.authNotice() ?? ""} testID="auth-notice" />
            </If>
            {/* permission: a 403 that is not CSRF — the account may not use
                this surface. Distinct copy, not a generic failure. */}
            <If when={store.authDenied()}>
              <GateCallout
                tone="danger"
                text="权限不足：这个账号不能使用网页登录，请联系部署管理员。"
                testID="auth-denied"
              />
            </If>

            {/* loading: the boot probe has not answered yet */}
            <If when={checking()}>
              <View
                testID="auth-loading"
                accessibilityLabel="正在连接 Keel 服务"
                style={{ flexDirection: "row", alignItems: "center", gap: space.sm, paddingVertical: space.lg }}
              >
                <Spinner accessibilityLabel="正在连接 Keel 服务" />
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>正在连接 Keel 服务…</Text>
              </View>
            </If>

            {/* error/empty: no live service — an honest face with a retry,
                never a form whose submit cannot reach anyone */}
            <If when={offline()}>
              <Empty testID="auth-offline" accessibilityLabel="无法连接 Keel 服务">
                <EmptyTitle label="无法连接 Keel 服务" />
                <EmptyDescription label={store.sessionsError() ?? "确认服务已启动，然后重试。"} />
                <EmptyContent>
                  <Button
                    testID="auth-retry"
                    variant="outline"
                    label="重试"
                    accessibilityLabel="重新连接 Keel 服务"
                    onPress={() => void store.reconnect()}
                  />
                </EmptyContent>
              </Empty>
            </If>

            {/* the login form */}
            <If when={stage() === "required" && !offline() && !checking()}>
              <Field label="用户名" error={fieldError() ?? undefined}>
                <Input
                  testID="login-username"
                  value={username()}
                  onChange={(value) => username.set(value)}
                  invalid={fieldError() !== null}
                  placeholder="账号用户名"
                  accessibilityLabel="用户名"
                />
              </Field>
              <Field label="密码" hint="密码区分大小写；账号不存在和密码错误不会分别提示。">
                <Input
                  testID="login-password"
                  value={password()}
                  onChange={(value) => password.set(value)}
                  masked
                  invalid={fieldError() !== null}
                  placeholder="账号密码"
                  submitBehavior="submit"
                  onSubmit={() => void login()}
                  accessibilityLabel="密码"
                />
              </Field>
              <Button
                testID="login-submit"
                variant="default"
                label={submitting() ? "登录中…" : "进入"}
                loading={submitting()}
                disabled={submitting() || busy() || !username().trim() || !password()}
                accessibilityLabel={submitting() ? "正在登录" : "进入 Keel"}
                onPress={() => void login()}
              />
            </If>

            {/* the bootstrap form (the "empty" state: no accounts exist yet) */}
            <If when={stage() === "bootstrap" && !offline()}>
              <GateCallout
                tone="warning"
                text="设置令牌由部署方在本机运行 keel admin bootstrap 生成，只能使用一次，且有有效期。"
                testID="bootstrap-hint"
              />
              <Field label="设置令牌" error={fieldError() ?? undefined}>
                <Input
                  testID="bootstrap-token"
                  value={setupToken()}
                  onChange={(value) => setupToken.set(value)}
                  invalid={fieldError() !== null}
                  placeholder="一次性设置令牌"
                  accessibilityLabel="设置令牌"
                />
              </Field>
              <Field label="管理员用户名">
                <Input
                  testID="bootstrap-username"
                  value={setupUsername()}
                  onChange={(value) => setupUsername.set(value)}
                  placeholder="例如 admin"
                  accessibilityLabel="管理员用户名"
                />
              </Field>
              <Field label="密码" hint="至少 12 个字节（中文约 4 个字）。创建后首次登录会要求再改一次密码。">
                <Input
                  testID="bootstrap-password"
                  value={setupPassword()}
                  onChange={(value) => setupPassword.set(value)}
                  masked
                  placeholder="设置密码"
                  accessibilityLabel="管理员密码"
                />
              </Field>
              <Field label="确认密码">
                <Input
                  testID="bootstrap-confirm"
                  value={setupConfirm()}
                  onChange={(value) => setupConfirm.set(value)}
                  masked
                  placeholder="再输入一次"
                  submitBehavior="submit"
                  onSubmit={() => void bootstrap()}
                  accessibilityLabel="确认管理员密码"
                />
              </Field>
              <Button
                testID="bootstrap-submit"
                variant="default"
                label={submitting() ? "创建中…" : "创建管理员"}
                loading={submitting()}
                disabled={submitting() || !setupToken().trim() || !setupUsername().trim() || !setupPassword()}
                accessibilityLabel={submitting() ? "正在创建管理员" : "创建超级管理员"}
                onPress={() => void bootstrap()}
              />
            </If>

            {/* the must-change gate */}
            <If when={stage() === "must-change"}>
              <GateCallout
                tone="warning"
                text="这个账号还在使用初始密码。新密码生效后，其他已登录的会话会全部失效。"
                testID="must-change-hint"
              />
              <Field label="当前密码" error={fieldError() ?? undefined}>
                <Input
                  testID="change-current"
                  value={currentPassword()}
                  onChange={(value) => currentPassword.set(value)}
                  masked
                  invalid={fieldError() !== null}
                  placeholder="现在的密码"
                  accessibilityLabel="当前密码"
                />
              </Field>
              <Field label="新密码" hint="至少 12 个字节（中文约 4 个字），且不能与当前密码相同。">
                <Input
                  testID="change-new"
                  value={newPassword()}
                  onChange={(value) => newPassword.set(value)}
                  masked
                  placeholder="新密码"
                  accessibilityLabel="新密码"
                />
              </Field>
              <Field label="确认新密码">
                <Input
                  testID="change-confirm"
                  value={confirmPassword()}
                  onChange={(value) => confirmPassword.set(value)}
                  masked
                  placeholder="再输入一次新密码"
                  submitBehavior="submit"
                  onSubmit={() => void changePassword()}
                  accessibilityLabel="确认新密码"
                />
              </Field>
              <Button
                testID="change-submit"
                variant="default"
                label={submitting() ? "保存中…" : "保存并进入"}
                loading={submitting()}
                disabled={submitting() || busy() || !currentPassword() || !newPassword() || !confirmPassword()}
                accessibilityLabel={submitting() ? "正在保存新密码" : "保存新密码并进入"}
                onPress={() => void changePassword()}
              />
            </If>
          </View>
        </CardContent>
      </Card>
    </View>
  );
}
