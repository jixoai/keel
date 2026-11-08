// The login gate: a live keel serve with a passphrase answers 401 until the
// browser exchanges the passphrase for the server's HttpOnly session cookie.
// This screen is the whole of the client's auth surface — the server owns the
// cookie, the tenant and the TTL.
import { signal, Text, View } from "nsh";
import { Input } from "nsh/base";
import { store } from "../data/store";
import { Icon } from "../ui/icons";
import { Press, Spinner } from "../ui/primitives";
import { alpha, colors, glass, radius, space } from "../ui/theme";

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

  return (
    <View
      style={{
        flex: 1,
        alignItems: "center",
        justifyContent: "center",
        backgroundColor: colors.background,
        padding: space.lg,
      }}
    >
      <View style={{ width: "100%", maxWidth: 360, gap: space.md }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: 10 }}>
          <View
            style={{
              width: 34,
              height: 34,
              borderRadius: radius.control,
              alignItems: "center",
              justifyContent: "center",
              ...glass.control,
            }}
          >
            <Icon name="spark" size={18} tint={colors.brand} />
          </View>
          <View style={{ gap: 2 }}>
            <Text style={{ fontSize: 18, fontWeight: "700", color: colors.foreground }}>Keel</Text>
            <Text style={{ fontSize: 11.5, color: colors.mutedForeground }}>AI 员工会话</Text>
          </View>
        </View>

        <View style={{ borderRadius: radius.card, ...glass.card, padding: space.md, gap: space.sm }}>
          <Text style={{ fontSize: 12.5, color: colors.mutedForeground }}>
            这个 keel serve 需要口令。口令在启动时设置（KEEL_SERVE_PASSWORD）。
          </Text>
          <Input
            testID="login-password"
            value={password()}
            onChange={({ value }: { value: string }) => password.set(value)}
            masked
            placeholder="服务口令"
            autoCapitalize="none"
            autoCorrect={false}
            submitBehavior="submit"
            onSubmit={() => void submit()}
            style={{
              height: 38,
              fontSize: 14,
              color: colors.foreground,
              placeholderColor: colors.mutedForeground,
              caretColor: colors.brand,
              selectionColor: alpha(colors.brand, 0.3),
              paddingHorizontal: 12,
              borderRadius: radius.control,
              borderWidth: 1,
              borderColor: colors.border,
              backgroundColor: colors.background,
            }}
          />
          <Press
            onPress={() => void submit()}
            disabled={submitting() || !password().trim()}
            accessibilityLabel="进入 Keel"
            style={{
              height: 38,
              borderRadius: radius.control,
              alignItems: "center",
              justifyContent: "center",
              flexDirection: "row",
              gap: 8,
              backgroundColor: colors.brand,
            }}
          >
            <If when={submitting()} else={<Text style={{ fontSize: 13, fontWeight: "600", color: colors.brandForeground }}>进入</Text>}>
              <Spinner size={13} tint={colors.brandForeground} />
              <Text style={{ fontSize: 13, fontWeight: "600", color: colors.brandForeground }}>验证中…</Text>
            </If>
          </Press>
          <If when={store.authError()}>
            <Text style={{ fontSize: 11.5, color: colors.danger }}>{store.authError() ?? ""}</Text>
          </If>
        </View>
      </View>
    </View>
  );
}
