// The app root: a sidebar + conversation on wide screens, two screens on
// narrow ones (and in the TUI's narrow grid). One code path for web and the
// TUI target; the only platform check is the window width.
import { effect, mount, signal, Text, View, windowSize } from "nsh";
import { store } from "./src/data/store";
import { Icon } from "./src/ui/icons";
import { Press, Spinner } from "./src/ui/primitives";
import { alpha, colors, glass, radius, space } from "./src/ui/theme";
import { SessionList } from "./src/screens/session-list";
import { Conversation } from "./src/screens/conversation";

const isWide = () => windowSize().width >= 900;

function NoticeToast() {
  return (
    <If when={store.notice()}>
      <View style={{ position: "absolute", top: 12, left: 0, right: 0, alignItems: "center", zIndex: 10 }}>
        <Press
          onPress={() => store.dismissNotice()}
          accessibilityLabel="关闭提示"
          style={{
            maxWidth: 640,
            flexDirection: "row",
            alignItems: "center",
            gap: space.sm,
            paddingHorizontal: 14,
            minHeight: 36,
            borderRadius: radius.card,
            ...glass.popover,
            backgroundColor:
              store.notice()?.tone === "danger"
                ? colors.dangerTint
                : store.notice()?.tone === "warn"
                  ? colors.warningTint
                  : store.notice()?.tone === "success"
                    ? colors.successTint
                    : colors.infoTint,
            borderColor:
              store.notice()?.tone === "danger"
                ? alpha(colors.danger, 0.32)
                : store.notice()?.tone === "warn"
                  ? alpha(colors.warning, 0.32)
                  : store.notice()?.tone === "success"
                    ? alpha(colors.success, 0.32)
                    : alpha(colors.info, 0.32),
          }}
        >
          <Icon
            name={store.notice()?.tone === "danger" || store.notice()?.tone === "warn" ? "alert" : "check"}
            size={14}
            tint={
              store.notice()?.tone === "danger"
                ? colors.danger
                : store.notice()?.tone === "warn"
                  ? colors.warning
                  : store.notice()?.tone === "success"
                    ? colors.success
                    : colors.info
            }
          />
          <Text style={{ flex: 1, fontSize: 12, color: colors.foreground }}>{store.notice()?.text ?? ""}</Text>
          <Icon name="x" size={12} tint={colors.mutedForeground} />
        </Press>
      </View>
    </If>
  );
}

function ProbeBadge() {
  return (
    <If when={store.mode() === "checking"}>
      <View
        style={{
          position: "absolute",
          top: 10,
          right: 12,
          flexDirection: "row",
          alignItems: "center",
          gap: 6,
          paddingHorizontal: 10,
          height: 28,
          borderRadius: radius.pill,
          ...glass.popover,
        }}
      >
        <Spinner size={11} tint={colors.brand} />
        <Text style={{ fontSize: 11, color: colors.mutedForeground }}>正在探测 keel serve…</Text>
      </View>
    </If>
  );
}

function App() {
  const screen = signal<"list" | "chat">("list");
  const autoOpened = signal(false);

  effect(() => {
    if (!isWide() || store.mode() === "checking" || autoOpened() || store.activeId()) return;
    const rows = store.sessions();
    if (rows.length === 0) return;
    autoOpened.set(true);
    void store.openSession(rows[0].sessionId);
  });

  const select = (id: string) => {
    void store.openSession(id);
    if (!isWide()) screen.set("chat");
  };
  const back = () => {
    store.closeSession();
    screen.set("list");
  };

  return (
    <View
      style={{
        flex: 1,
        flexDirection: isWide() ? "row" : "column",
        backgroundColor: colors.background,
      }}
    >
      <If
        when={isWide()}
        else={
          <If
            when={screen() === "chat" && !!store.activeId()}
            else={<SessionList onSelect={select} activeId={store.activeId()} wide={false} />}
          >
            <Conversation wide={false} onBack={back} onOpenList={back} />
          </If>
        }
      >
        <SessionList onSelect={select} activeId={store.activeId()} wide />
        <View style={{ flex: 1, minWidth: 0 }}>
          <Conversation wide />
        </View>
      </If>
      <NoticeToast />
      <ProbeBadge />
    </View>
  );
}

mount(App);
