// The session list: create, delete (two-step), search, empty/loading/error
// states, last-message summary and run status. On a wide screen it is the
// sidebar; on a narrow one (and in the TUI's narrow grid) it is the first
// screen, with the conversation a second one.
import { signal, Text, View } from "nsh";
import { Input } from "nsh/base";
import type { SessionSummary } from "../data/contract";
import { store } from "../data/store";
import { formatTime } from "../data/wire";
import { Icon } from "../ui/icons";
import { Badge, IconButton, PillButton, Press, Spinner, StatusDot } from "../ui/primitives";
import { alpha, colors, glass, radius, space } from "../ui/theme";

const query = signal("");
const deleting = signal<string | null>(null);
const refreshing = signal(false);

const runTone = (status: string | undefined): "running" | "done" | "failed" | "idle" =>
  status === "running" ? "running" : status === "failed" || status === "error" ? "failed" : status === "cancelled" ? "idle" : "done";

const runLabel = (status: string | undefined): string | null => {
  if (status === "running") return "运行中";
  if (status === "failed" || status === "error") return "失败";
  if (status === "cancelled") return "已取消";
  return null;
};

function SessionRow(props: { row: SessionSummary; active: boolean; onSelect: () => void }) {
  const row = () => props.row;
  const title = () => row().title || row().sessionId.slice(0, 10);
  const summary = () => row().lastMessage || "（空会话）";
  const confirming = () => deleting() === row().sessionId;
  return (
    <View
      style={{
        borderRadius: radius.control,
        backgroundColor: props.active ? colors.glassState : alpha(colors.foreground, 0),
        borderWidth: props.active ? 1 : 0,
        borderColor: colors.glassStateRim,
      }}
    >
      <If
        when={confirming()}
        else={
          <View style={{ flexDirection: "row", alignItems: "center", gap: 2, paddingRight: 4 }}>
            <Press
              onPress={props.onSelect}
              accessibilityLabel={`打开会话 ${title()}`}
              accessibilityState={{ selected: props.active }}
              hoverStyle={{ backgroundColor: colors.hover }}
              style={{ flex: 1, paddingLeft: 10, paddingVertical: 9, gap: 3, borderRadius: radius.control }}
            >
              <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
                <StatusDot tone={runTone(row().runStatus)} />
                <Text
                  style={{
                    flex: 1,
                    numberOfLines: 1,
                    fontSize: 13,
                    fontWeight: props.active ? "700" : "600",
                    color: colors.foreground,
                  }}
                >
                  {title()}
                </Text>
                <Text style={{ fontSize: 10, color: colors.mutedForeground, fontVariant: ["tabular-nums"] }}>
                  {formatTime(row().lastMessageAt)}
                </Text>
              </View>
              <View style={{ flexDirection: "row", alignItems: "center", gap: 6, paddingLeft: 13 }}>
                <Text style={{ flex: 1, numberOfLines: 1, fontSize: 11.5, color: colors.mutedForeground }}>
                  {summary()}
                </Text>
                <If when={!!runLabel(row().runStatus)}>
                  <Text
                    style={{
                      fontSize: 9.5,
                      fontWeight: "600",
                      color:
                        runTone(row().runStatus) === "running"
                          ? colors.brand
                          : runTone(row().runStatus) === "failed"
                            ? colors.danger
                            : colors.mutedForeground,
                    }}
                  >
                    {runLabel(row().runStatus)}
                  </Text>
                </If>
              </View>
            </Press>
            <IconButton
              icon="trash"
              label="删除会话"
              size={28}
              iconSize={14}
              onPress={() => deleting.set(row().sessionId)}
            />
          </View>
        }
      >
        <View style={{ padding: 10, gap: space.sm }}>
          <Text style={{ fontSize: 12, color: colors.foreground }}>
            删除「{title()}」？此操作不可撤销。
          </Text>
          <View style={{ flexDirection: "row", gap: space.sm }}>
            <PillButton
              label="删除"
              tone="danger"
              compact
              onPress={() => {
                const id = row().sessionId;
                deleting.set(null);
                void store.deleteSession(id);
              }}
            />
            <PillButton label="取消" tone="ghost" compact onPress={() => deleting.set(null)} />
          </View>
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
          <View
            style={{
              width: 28,
              height: 28,
              borderRadius: 9,
              alignItems: "center",
              justifyContent: "center",
              backgroundColor: colors.brandMuted,
              borderWidth: 1,
              borderColor: alpha(colors.brand, 0.3),
            }}
          >
            <Icon name="spark" size={15} tint={colors.brand} />
          </View>
          <View style={{ flex: 1 }}>
            <Text style={{ fontSize: 16, fontWeight: "700", color: colors.foreground }}>Keel</Text>
            <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>AI 员工会话</Text>
          </View>
          <IconButton
            icon={refreshing() ? "refresh" : "refresh"}
            label="刷新会话"
            size={30}
            iconSize={15}
            onPress={() => void refresh()}
          />
          <IconButton
            icon="plus"
            label="新建会话"
            size={30}
            iconSize={16}
            tint={colors.brand}
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
          {query() ? <IconButton icon="x" label="清除搜索" size={22} iconSize={11} onPress={() => query.set("")} /> : null}
        </View>
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
            {!query() ? <PillButton label="新建会话" tone="primary" compact onPress={() => void store.createSession()} /> : null}
          </View>
        ) : (
          <View style={{ gap: 2, paddingHorizontal: 6, paddingBottom: 8 }}>
            {filtered().map((row) => (
              <SessionRow
                row={row}
                active={props.activeId === row.sessionId}
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
            onPress={() => void store.setPartials(!store.showPartials())}
            accessibilityLabel="切换 token 级 partial"
            accessibilityState={{ selected: store.showPartials() }}
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
          <Text style={{ fontSize: 9.5, color: colors.mutedForeground }}>NSH nightly</Text>
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
        <StatusDot tone={tone() === "success" ? "done" : tone() === "warning" ? "running" : "idle"} size={6} />
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
