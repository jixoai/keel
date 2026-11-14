// The login gate: a live keel serve with a passphrase answers 401 until the
// browser exchanges the passphrase for the server's HttpOnly session cookie.
// This screen is the whole of the client's auth surface — the server owns the
// cookie, the tenant and the TTL.
//
// The gate itself is a kit panel: Card (Header / Content) carrying one Field
// around the passphrase Input and one Button. The only layout written here is
// the centering frame — the control heights, radii, ink and padding are the
// kit's, so the gate cannot drift from the rest of the app. A refused
// passphrase is the field's own error (Field takes the server's message); a
// submit in flight is the Button's loading state.
import { signal, View } from "nsh";
import { Button, Card, CardContent, CardHeader, Field, Input, color, space } from "../ui/kit";
import { store } from "../data/store";

export function LoginScreen() {
  const password = signal("");
  const submitting = signal(false);

  const submit = async () => {
    if (submitting() || !password().trim()) return;
    submitting.set(true);
    const ok = await store.login(password());
    submitting.set(false);
    if (ok) password.set("");
  };

  // The server's answer, as the field's error: a wrong passphrase belongs to
  // this field, not to a loose line under the form.
  //
  // The store's 401 word is 「口令不正确」 — what failed, with no way forward
  // (`better-writing`: an error names the next step). The mapping lives here,
  // in the presentation, so the store keeps carrying the server's own meaning
  // and every other failure (a network error, a 5xx) still shows verbatim.
  const error = () => {
    const message = store.authError();
    if (message === null) return undefined;
    if (message === "口令不正确") {
      return "口令不正确。请向管理员或启动服务的终端确认 KEEL_SERVE_PASSWORD，再重新输入。";
    }
    return message;
  };

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
        style={{ width: "100%", maxWidth: 384 }}
      >
        <CardHeader title="Keel" description="AI 员工会话" />
        <CardContent>
          <Field
            label="服务口令"
            hint="这个服务已开启口令保护。口令由启动服务的人设置（服务端环境变量 KEEL_SERVE_PASSWORD）。"
            error={error()}
          >
            <Input
              testID="login-password"
              value={password()}
              onChange={(value) => password.set(value)}
              masked
              invalid={error() !== undefined}
              placeholder="输入服务口令"
              submitBehavior="submit"
              onSubmit={() => void submit()}
              accessibilityLabel="服务口令"
            />
          </Field>
          <Button
            testID="login-submit"
            variant="default"
            label={submitting() ? "验证中…" : "进入"}
            loading={submitting()}
            disabled={submitting() || !password().trim()}
            accessibilityLabel={submitting() ? "正在验证口令" : "进入 Keel"}
            onPress={() => void submit()}
          />
        </CardContent>
      </Card>
    </View>
  );
}
