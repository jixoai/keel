// The session list: create, delete (a centered confirmation, never an inline
// row push), search, empty/loading/error states, last-message summary and a
// quiet projection of the run state. On a wide screen it is the sidebar; on a
// narrow one (and in the TUI's narrow grid) it is the first screen, with the
// conversation a second one.
import { platform, signal, Text, type TextStyle, View, windowSize } from "nsh";
import { Input } from "nsh/base";
import type { SessionSummary } from "../data/contract";
import { store } from "../data/store";
import { formatTime } from "../data/wire";
import { Icon } from "../ui/icons";
import { IconButton, PillButton, Press, Spinner, StatusDot } from "../ui/primitives";
import { conversationState, runStateLabel, sessionState } from "../ui/run-state";
import { cellWidth, fitCells } from "../ui/text";
import { alpha, colors, glass, radius, space } from "../ui/theme";
import { requestDelete } from "./delete-dialog";

const query = signal("");
const refreshing = signal(false);

const isTerminal = () => platform.idiom === "terminal";
const isPhone = () => !isTerminal() && windowSize().width < 900;

/** The one-word state of a row: the active row projects the open conversation
 * (which knows about stopping), every other row projects runStatus. */
const rowStateOf = (row: SessionSummary, active: boolean) => {
  if (active && store.conversation()) return conversationState(store.conversation());
  return sessionState(row.runStatus);
};

const rowDotTone = (state: ReturnType<typeof rowStateOf>) =>
  state === "running"
    ? "running"
    : state === "failed"
      ? "failed"
      : state === "stopping" || state === "reconnecting"
        ? "warning"
        : state === "stopped"
          ? "idle"
          : "done";

function Highlighted(props: { text: string; style: TextStyle }) {
  const parts = () => {
    const needle = query().trim().toLowerCase();
    if (!needle) return [{ text: props.text, match: false }];
    const haystack = props.text.toLowerCase();
    const out: Array<{ text: string; match: boolean }> = [];
    let index = 0;
    while (index < props.text.length) {
      const at = haystack.indexOf(needle, index);
      if (at < 0) {
        out.push({ text: props.text.slice(index), match: false });
        break;
      }
      if (at > index) out.push({ text: props.text.slice(index, at), match: false });
      out.push({ text: props.text.slice(at, at + needle.length), match: true });
      index = at + needle.length;
    }
    return out;
  };
  return (
    <Text style={props.style}>
      {parts().map((part) =>
        part.match ? (
          <Text style={{ backgroundColor: colors.mark, color: colors.markForeground }}>{part.text}</Text>
        ) : (
          part.text
        ),
      )}
    </Text>
  );
}

function SessionRow(props: { row: SessionSummary; active: boolean; onSelect: () => void; wide: boolean }) {
  const hovered = signal(false);
  const focused = signal(false);
  const row = () => props.row;
  const title = () => row().title || row().sessionId.slice(0, 10);
  const summary = () => row().lastMessage || "（空会话）";
  const searching = () => query().trim().length > 0;
  // Mobile reveals delete with a long press; desktop with the row's overflow
  // button (hover or keyboard focus). Neither is a persistent trash icon.
  const canLongPress = () => isPhone() && !searching();
  const showMenu = () => props.wide && !isTerminal() && !searching();
  const state = () => rowStateOf(row(), props.active);
  // The active row's state is the header pill's job; rows only surface a
  // failure, so no run word is repeated down the sidebar.
  const stateLabel = () => (!props.active && state() === "failed" ? runStateLabel.failed : null);
  const time = () => formatTime(row().lastMessageAt);
  const cells = () =>
    isTerminal() ? Math.max(24, Math.floor(windowSize().width / 8)) : Math.max(24, Math.floor(windowSize().width));
  const titleBudget = () => Math.max(6, cells() - 12 - cellWidth(time()));
  const previewBudget = () =>
    Math.max(6, cells() - 8 - (stateLabel() ? cellWidth(stateLabel()!) + 1 : 0));
  const titleText = () => (isTerminal() ? fitCells(title(), titleBudget()) : title());
  const summaryText = () => (isTerminal() ? fitCells(summary(), previewBudget()) : summary());
  const openDelete = () => requestDelete(row().sessionId, title());

  return (
    <View
      onMouseEnter={() => hovered.set(true)}
      onMouseLeave={() => hovered.set(false)}
      style={{
        flexDirection: "row",
        alignItems: "center",
        borderRadius: radius.control,
        backgroundColor: props.active ? colors.brandTint : alpha(colors.foreground, 0),
        borderLeftWidth: props.active ? 2 : 0,
        borderLeftColor: colors.brand,
        transitionProperty: "background-color",
        transitionDuration: 160,
      }}
    >
      <Press
        onPress={props.onSelect}
        onLongPress={canLongPress() ? openDelete : undefined}
        onFocus={() => focused.set(true)}
        onBlur={() => focused.set(false)}
        accessibilityLabel={`打开会话 ${title()}`}
        accessibilityState={{ selected: props.active }}
        hoverStyle={{ backgroundColor: props.active ? undefined : colors.hover }}
        style={{
          flex: 1,
          minWidth: 0,
          paddingLeft: props.active ? 8 : 10,
          paddingRight: 6,
          paddingVertical: 8,
          gap: 3,
          borderRadius: radius.control,
        }}
      >
        <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
          <StatusDot tone={rowDotTone(state())} />
          <Highlighted
            text={titleText()}
            style={{
              flex: 1,
              numberOfLines: 1,
              textOverflow: "ellipsis",
              fontSize: 13,
              fontWeight: props.active ? "700" : "600",
              color: colors.foreground,
            }}
          />
          <If when={!!stateLabel()}>
            <Text
              style={{
                fontSize: 9.5,
                fontWeight: "600",
                color: state() === "failed" ? colors.danger : colors.mutedForeground,
              }}
            >
              {stateLabel() ?? ""}
            </Text>
          </If>
          <Text
            style={{
              fontSize: 10,
              color: colors.mutedForeground,
              fontVariant: ["tabular-nums"],
              numberOfLines: 1,
            }}
          >
            {time()}
          </Text>
        </View>
        <View style={{ flexDirection: "row", alignItems: "center", gap: 6, paddingLeft: isTerminal() ? 0 : 13 }}>
          <Highlighted
            text={summaryText()}
            style={{
              flex: 1,
              numberOfLines: 1,
              textOverflow: "ellipsis",
              fontSize: 11.5,
              color: colors.mutedForeground,
            }}
          />
        </View>
      </Press>
      <If when={showMenu()}>
        <View
          style={{
            width: 28,
            marginRight: 3,
            opacity: hovered() || focused() ? 1 : 0,
            transitionProperty: "opacity",
            transitionDuration: 160,
            pointerEvents: hovered() || focused() ? "auto" : "none",
          }}
        >
          <IconButton
            icon="dots"
            label="删除会话"
            size={26}
            iconSize={14}
            hitSlop={6}
            onPress={openDelete}
            onFocus={() => focused.set(true)}
            onBlur={() => focused.set(false)}
          />
        </View>
      </If>
    </View>
  );
}

function Skeleton() {
  return (
    <View style={{ gap: space.sm, padding: space.md }}>
      {[0, 1, 2, 3].map((i) => (
        <View style={{ gap: 6, paddingVertical: 6 }}>
          <View style={{ width: `${70 - i * 8}%`, height: 12, borderRadius: 6, backgroundColor: colors.muted }} />
          <View style={{ width: `${88 - i * 5}%`, height: 10, borderRadius: 5, backgroundColor: colors.muted }} />
        </View>
      ))}
    </View>
  );
}

export function SessionList(props: { onSelect: (id: string) => void; activeId: string | null; wide: boolean }) {
  const filtered = () => {
    const q = query().trim().toLowerCase();
    const rows = store.sessions();
    if (!q) return rows;
    return rows.filter((row) =>
      `${row.title ?? ""} ${row.lastMessage ?? ""} ${row.sessionId}`.toLowerCase().includes(q),
    );
  };
  const refresh = async () => {
    refreshing.set(true);
    await store.refreshSessions();
    refreshing.set(false);
  };
  const chipSlop = () => (isPhone() ? 11 : undefined);
  return (
    <View
      style={{
        flex: 1,
        width: props.wide ? 300 : "100%",
        backgroundColor: colors.sidebar,
        borderRightWidth: props.wide ? 1 : 0,
        borderRightColor: colors.sidebarBorder,
      }}
    >
      <View style={{ padding: space.md, gap: space.sm }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, height: 36 }}>
          <If when={!isTerminal()}>
            <View
              style={{
                width: 28,
                height: 28,
                borderRadius: 9,
                alignItems: "center",
                justifyContent: "center",
                backgroundColor: colors.muted,
                borderWidth: 1,
                borderColor: colors.border,
              }}
            >
              <Icon name="spark" size={15} tint={colors.brand} />
            </View>
          </If>
          <View style={{ flex: 1 }}>
            <Text style={{ fontSize: 16, fontWeight: "700", color: colors.foreground }}>Keel</Text>
            <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>AI 员工会话</Text>
          </View>
          <IconButton
            icon="settings"
            label="设置"
            size={30}
            iconSize={15}
            hitSlop={isPhone() ? 7 : undefined}
            onPress={() => store.openSettings()}
          />
          <IconButton
            icon="refresh"
            label="刷新会话"
            size={30}
            iconSize={15}
            hitSlop={isPhone() ? 7 : undefined}
            onPress={() => void refresh()}
          />
          <IconButton
            icon="plus"
            label="新建会话"
            size={30}
            iconSize={16}
            tint={colors.brand}
            hitSlop={isPhone() ? 7 : undefined}
            onPress={() => void store.createSession()}
          />
        </View>
        <View
          style={{
            flexDirection: "row",
            alignItems: "center",
            gap: 6,
            height: 34,
            paddingHorizontal: 9,
            borderRadius: radius.control,
            ...glass.field,
          }}
        >
          <Icon name="search" size={14} tint={colors.mutedForeground} />
          <Input
            value={query()}
            onChange={({ value }) => query.set(value)}
            placeholder="搜索会话"
            style={{
              flex: 1,
              height: 30,
              fontSize: 12.5,
              color: colors.foreground,
              placeholderColor: colors.mutedForeground,
              caretColor: colors.brand,
            }}
          />
          {query() ? (
            <IconButton
              icon="x"
              label="清除搜索"
              size={22}
              iconSize={11}
              hitSlop={11}
              onPress={() => query.set("")}
            />
          ) : null}
        </View>
        <If when={query().trim().length > 0}>
          <Text style={{ paddingLeft: 2, fontSize: 10.5, color: colors.mutedForeground }}>
            {filtered().length > 0 ? `${filtered().length} 个结果` : "没有结果"}
          </Text>
        </If>
      </View>

      <View style={{ flex: 1, minHeight: 0 }}>
        {store.sessionsLoading() ? (
          <Skeleton />
        ) : store.sessionsError() ? (
          <View style={{ padding: space.lg, gap: space.sm }}>
            <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
              <Icon name="alert" size={14} tint={colors.danger} />
              <Text style={{ fontSize: 12.5, fontWeight: "600", color: colors.danger }}>会话列表加载失败</Text>
            </View>
            <Text style={{ fontSize: 11.5, lineHeight: 17, color: colors.mutedForeground }}>
              {store.sessionsError()}
            </Text>
            <PillButton label="重试" compact onPress={() => void refresh()} />
          </View>
        ) : filtered().length === 0 ? (
          <View style={{ padding: space.xl, alignItems: "center", gap: space.sm }}>
            <Icon name="message" size={22} tint={colors.mutedForeground} />
            <Text style={{ fontSize: 12.5, color: colors.mutedForeground }}>
              {query() ? "没有匹配的会话" : "还没有会话"}
            </Text>
            {query() ? (
              <PillButton label="清除搜索" compact onPress={() => query.set("")} />
            ) : (
              <PillButton label="新建会话" tone="primary" compact onPress={() => void store.createSession()} />
            )}
          </View>
        ) : (
          <View style={{ gap: 2, paddingHorizontal: 6, paddingBottom: 8 }}>
            {filtered().map((row) => (
              <SessionRow
                row={row}
                active={props.activeId === row.sessionId}
                wide={props.wide}
                onSelect={() => props.onSelect(row.sessionId)}
              />
            ))}
          </View>
        )}
      </View>

      <View
        style={{
          padding: space.md,
          gap: space.sm,
          borderTopWidth: 1,
          borderTopColor: colors.sidebarBorder,
        }}
      >
        <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
          <Icon
            name={store.connection().status === "open" ? "wifi" : "wifi-off"}
            size={12}
            tint={
              store.connection().status === "open"
                ? colors.success
                : store.connection().status === "reconnecting"
                  ? colors.warning
                  : colors.mutedForeground
            }
          />
          <Text style={{ flex: 1, fontSize: 10.5, color: colors.mutedForeground }}>
            {store.mode() === "checking"
              ? "正在探测 keel serve…"
              : store.mode() === "fixture"
                ? "演示数据 · 未连接服务"
                : store.connection().status === "open"
                  ? `实时服务 · 已连接${store.connection().lastSeq ? ` @${store.connection().lastSeq}` : ""}`
                  : store.connection().status === "reconnecting"
                    ? "实时服务 · 重连中…"
                    : "实时服务 · 未连接"}
          </Text>
          {store.mode() !== "checking" ? (
            <Press
              onPress={() => void store.switchMode(store.mode() === "fixture" ? "http" : "fixture")}
              accessibilityLabel="切换数据源"
              hitSlop={chipSlop()}
              hoverStyle={{ opacity: 0.88 }}
              style={{
                paddingHorizontal: 8,
                height: 22,
                borderRadius: radius.pill,
                backgroundColor: store.mode() === "fixture" ? colors.warningTint : colors.successTint,
                alignItems: "center",
                justifyContent: "center",
              }}
            >
              <Text
                style={{
                  fontSize: 10,
                  fontWeight: "600",
                  color: store.mode() === "fixture" ? colors.warning : colors.success,
                }}
              >
                {store.mode() === "fixture" ? "切到实时" : "切到演示"}
              </Text>
            </Press>
          ) : null}
        </View>
        <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
          <Press
            onPress={() => store.openSettings()}
            accessibilityLabel="打开设置"
            hitSlop={chipSlop()}
            hoverStyle={{ opacity: 0.88 }}
            style={{
              flexDirection: "row",
              alignItems: "center",
              gap: 5,
              paddingHorizontal: 8,
              height: 22,
              borderRadius: radius.pill,
              backgroundColor: colors.muted,
            }}
          >
            <Icon name="settings" size={10} tint={colors.mutedForeground} />
            <Text style={{ fontSize: 10, fontWeight: "600", color: colors.mutedForeground }}>设置</Text>
          </Press>
          <Press
            onPress={() => void store.setPartials(!store.showPartials())}
            accessibilityLabel="切换 token 级 partial"
            accessibilityState={{ selected: store.showPartials() }}
            hitSlop={chipSlop()}
            hoverStyle={{ opacity: 0.88 }}
            style={{
              flexDirection: "row",
              alignItems: "center",
              gap: 5,
              paddingHorizontal: 8,
              height: 22,
              borderRadius: radius.pill,
              backgroundColor: store.showPartials() ? colors.brandTint : colors.muted,
            }}
          >
            <Icon name="spark" size={10} tint={store.showPartials() ? colors.brand : colors.mutedForeground} />
            <Text
              style={{
                fontSize: 10,
                fontWeight: "600",
                color: store.showPartials() ? colors.brand : colors.mutedForeground,
              }}
            >
              partial {store.showPartials() ? "开" : "关"}
            </Text>
          </Press>
          {store.mode() === "fixture" ? (
            <Press
              onPress={() => store.simulateDrop()}
              accessibilityLabel="模拟断线重连"
              hitSlop={chipSlop()}
              hoverStyle={{ opacity: 0.88 }}
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: 5,
                paddingHorizontal: 8,
                height: 22,
                borderRadius: radius.pill,
                backgroundColor: colors.muted,
              }}
            >
              <Icon name="wifi-off" size={10} tint={colors.mutedForeground} />
              <Text style={{ fontSize: 10, fontWeight: "600", color: colors.mutedForeground }}>断线演示</Text>
            </Press>
          ) : null}
          <View style={{ flex: 1 }} />
          <If when={!isTerminal()}>
            <Text style={{ fontSize: 9.5, color: colors.mutedForeground }}>NSH nightly</Text>
          </If>
        </View>
      </View>
    </View>
  );
}

/** A compact connection strip for the conversation header. */
export function ConnectionBadge() {
  const state = () => store.connection();
  const mode = () => store.mode();
  const tone = () =>
    state().status === "open" ? "success" : state().status === "reconnecting" ? "warning" : "neutral";
  const label = () =>
    mode() === "checking"
      ? "探测中"
      : mode() === "fixture"
        ? "演示"
        : state().status === "open"
          ? "已连接"
          : state().status === "reconnecting"
            ? "重连中"
            : state().status === "connecting"
              ? "连接中"
              : "未连接";
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: 5 }}>
      {state().status === "reconnecting" || state().status === "connecting" ? (
        <Spinner size={10} tint={tone() === "warning" ? colors.warning : colors.mutedForeground} />
      ) : (
        <StatusDot tone={tone() === "success" ? "done" : tone() === "warning" ? "warning" : "idle"} size={6} />
      )}
      <Text
        style={{
          fontSize: 10.5,
          color:
            tone() === "success" ? colors.success : tone() === "warning" ? colors.warning : colors.mutedForeground,
        }}
      >
        {label()}
      </Text>
      {mode() === "http" && state().lastSeq ? (
        <Text style={{ fontSize: 9.5, color: colors.mutedForeground }}>@{state().lastSeq}</Text>
      ) : null}
    </View>
  );
}
