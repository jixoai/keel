// The plugin page (change keel-harness-shell, tasks P1–P3).
//
// P1 · the inventory reads `store.plugins` (`src/data/store.ts:197`), which
// `loadPlugins` fills from `GET /v1/plugins` (`src/data/store.ts:570-582`,
// `src/data/http.ts:362-364`). Every fact on a card is a field of
// `PluginStatusView` (`src/data/contract.ts:245-258`): id, version, state,
// provides, requires, tools, hooks, triggers, effects, diagnostics, startedAt,
// stoppedAt. The contract declares no "installable" and no "update" field —
// the 可装/更新 card is therefore an Empty that names `keel-plugin-core` (the
// entry that owns the install surface and the plugin UI route); no row is
// invented for it.
//
// P2 · opening. On the web a plugin's UI is its own page at `/plugins/<id>` and
// is carried by the platform's web view element (`<WebView src>`; NSH's web
// platform encodes it as a web view, `nsh-platform-web/src/encode.rs:1303-1315`,
// and the element is listed in `nsh`'s `docs/dev/elements.md:21,25-26`). The
// terminal has no web view: the press calls `openUrl`
// (`nsh/index.d.ts:332-333`) and, when the platform refuses it or no browser
// takes the address, the address stays on screen as a `Button variant="link"`.
// Nothing else changes on that press — the inventory, the open session and the
// route are untouched ("返回后 TUI 状态不变",
// `openspec/changes/keel-harness-shell/specs/app-shell/spec.md:61-69`).
//
// P3 · creation is an entry only: it fills the composer
// (`prefillComposer`, `src/screens/composer.tsx:55-58`) and hands back to the
// workspace. Installing a plugin depends on plugin-core's install API, which
// this contract does not carry — the card says that rather than pretending to
// install.
//
// The page writes no shell numbers of its own: the h-12 header band is
// `WORKSPACE_SHELL_HEADER_HEIGHT_PX`, the content ceiling is
// `SETTINGS_CONTENT_MAX_WIDTH_PX` (the same content column the settings page
// uses), and every size, radius and colour below is a kit token.
//
// NSH components run once: every reactive read sits inside JSX, an `<If>` or a
// function the JSX calls — nothing is copied into a local at the top.
import { cleanup, effect, host, openUrl, platform, signal, Text, View, WebView } from "nsh";
import type { PluginStatusView, TriggerSpecView, TriggerStatusView } from "../data/contract";
import { store } from "../data/store";
import { formatTime } from "../data/wire";
import { Icon } from "../ui/icons";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  Empty,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  ScrollArea,
  SETTINGS_CONTENT_MAX_WIDTH_PX,
  Skeleton,
  Table,
  TableBody,
  TableBodyRow,
  TableHeaderRow,
  Td,
  Th,
  WORKSPACE_SHELL_HEADER_HEIGHT_PX,
  alpha,
  color,
  font,
  iconSize,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../ui/kit";
import { prefillComposer } from "./composer";

// ---- where a plugin's page lives -----------------------------------------

/**
 * The origin the AgentAPI answers on — and therefore the origin the plugin
 * page is served from. Two sources, each one the platform's own:
 *
 *   · web    `keel.info.origin`, answered by the page host (`web/host.js:69-75`);
 *   · TUI    `KEEL_AGENT_URL`, exported by the launcher (`cmd/keel/tui.go:26,65`,
 *            documented in `cmd/keel/main.go:75`), read defensively because the
 *            app's capabilities may not grant `process.env`.
 *
 * With neither, the relative path is the only honest address this client can
 * name: the web view still resolves it against its page, and `openUrl` refuses
 * it — which the card then reports instead of hiding.
 */
const apiOrigin = signal("");

const originFromEnv = (): string => {
  try {
    const value: string | undefined = process.env.KEEL_AGENT_URL;
    return typeof value === "string" ? value.trim().replace(/\/+$/, "") : "";
  } catch {
    return "";
  }
};

let originAsked = false;

/** Ask the web host once; the terminal answer is the environment above. */
const readOrigin = () => {
  if (originAsked) return;
  originAsked = true;
  void (async () => {
    if (!host.available()) return;
    try {
      const info = await host.call<{ origin?: string }>("keel.info", {}, { timeout: 1200 });
      const origin = typeof info?.origin === "string" ? info.origin.replace(/\/+$/, "") : "";
      if (origin) apiOrigin.set(origin);
    } catch {
      /* a host that does not answer: the relative path stands */
    }
  })();
};

const pageOrigin = (): string => apiOrigin() || originFromEnv();

/** `/plugins/<id>` — the plugin page's path (what the browser would show). */
const pluginPath = (id: string): string => `/plugins/${encodeURIComponent(id)}`;

/** The absolute address when an origin is known, the path otherwise. */
const pluginUrl = (id: string): string => `${pageOrigin()}${pluginPath(id)}`;

// ---- state the page owns --------------------------------------------------

/** The plugin whose page the web view shows; `null` on the inventory. The
 * terminal never sets it — that press goes to the browser instead. */
const openId = signal<string | null>(null);

/** What the plugin page's address answered before the web view gets it. The
 * route belongs to keel-plugin-core (`proposal.md:35`) and is not served yet,
 * so a missing route is reported here instead of a raw 404 inside the frame. */
type PageProbe =
  | { state: "checking" }
  | { state: "ready" }
  | { state: "missing"; detail: string };

const pageProbe = signal<PageProbe>({ state: "checking" });

/** The web view's load failure, shown in place of the frame. */
const webviewError = signal<string | null>(null);

/** The last browser hand-off, per plugin: what `openUrl` answered. */
const browserOutcome = signal<{ id: string; ok: boolean; text: string } | null>(null);

const messageOf = (error: unknown): string =>
  error instanceof Error ? error.message : typeof error === "string" ? error : "未知错误";

/** Ask the plugin page once, then hand it to the web view. A 2xx means the
 * route exists; any other answer is reported with the server's own status. */
const probePluginPage = async (id: string) => {
  pageProbe.set({ state: "checking" });
  webviewError.set(null);
  try {
    const response = await fetch(pluginPath(id), { credentials: "same-origin" });
    if (openId() !== id) return;
    if (response.ok) {
      pageProbe.set({ state: "ready" });
      return;
    }
    pageProbe.set({ state: "missing", detail: `GET ${pluginPath(id)} 返回 HTTP ${response.status}` });
  } catch (error) {
    if (openId() !== id) return;
    pageProbe.set({ state: "missing", detail: `GET ${pluginPath(id)} 失败：${messageOf(error)}` });
  }
};

/** Keep the address bar in step with what the screen shows (`/plugins`,
 * `/plugins/<id>`), through the same host channel the store uses
 * (`src/data/store.ts:373-381`); a host without a URL ignores it. */
const navigate = (path: string) => {
  if (!host.available()) return;
  void (async () => {
    try {
      const current = await host.call<{ path?: string }>("keel.route", {}, { timeout: 1200 });
      if (typeof current?.path === "string" && current.path === path) return;
      await host.call("keel.navigate", { path }, { timeout: 1200 });
    } catch {
      /* terminal, desktop and test hosts keep the route in-app */
    }
  })();
};

/** The states the server's `PluginState` declares
 * (`internal/agentext/plugin.go:25-28`). A token this build does not know keeps
 * its raw text and the neutral outline — it is never mapped onto a made-up
 * state. */
const pluginStateVariant = (state: string): BadgeVariant => {
  switch (state) {
    case "active":
      return "success";
    case "pending":
      return "warning";
    case "failed":
      return "destructive";
    case "stopped":
      return "secondary";
    default:
      return "outline";
  }
};

// ---- opening --------------------------------------------------------------

/** The terminal's hand-off to the system browser. The web never calls it: its
 * plugins open inside the app (the page's web view), so no control on the web
 * depends on a capability the page cannot grant. */
const openInBrowser = async (id: string, url: string) => {
  try {
    const result = await openUrl(url);
    browserOutcome.set(
      result.opened
        ? { id, ok: true, text: "已在浏览器中打开" }
        : { id, ok: false, text: "没有应用能打开这个地址" },
    );
  } catch (error) {
    browserOutcome.set({ id, ok: false, text: `无法打开：${messageOf(error)}` });
  }
};

/** Web: check the page's address, then carry it in the web view. Terminal: hand
 * the address to the browser and leave every bit of page state alone. */
const openPlugin = (id: string) => {
  browserOutcome.set(null);
  if (platform.idiom === "terminal") {
    void openInBrowser(id, pluginUrl(id));
    return;
  }
  openId.set(id);
  void probePluginPage(id);
};

const closePlugin = () => {
  openId.set(null);
  pageProbe.set({ state: "checking" });
  webviewError.set(null);
  navigate("/plugins");
};

// ---- creation -------------------------------------------------------------

/** The request the creation entry fills into the composer. It asks for the
 * manifest and the install steps; the install itself is plugin-core's. */
const CREATE_PLUGIN_PROMPT =
  "我想创建一个 Keel 插件：先帮我梳理它要提供的能力（工具、hooks、触发器）与依赖，再给出插件清单（plugin.yaml）和安装步骤。";

const startCreation = () => {
  prefillComposer(CREATE_PLUGIN_PROMPT);
  // Back to the workspace: `closeSettings` is the store's "return to the
  // workspace" entry, and it moves the page URL with the route.
  store.closeSettings();
};

// ---- small pieces ---------------------------------------------------------

/** A table's header ink, read per render: the `color` tokens are getters, so a
 * module-level object would freeze the appearance it was imported under. */
const headText = () => ({ fontSize: typeScale.xs, color: color.mutedForeground });

/** One label over one value: the shape every "来源" fact takes. */
function Fact(props: { label: string; value: string }) {
  return (
    <View style={{ gap: space.xxs, minWidth: 96 }}>
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{props.label}</Text>
      <Text
        style={{
          fontSize: typeScale.sm,
          lineHeight: Math.round(typeScale.sm * lineHeight.normal),
          color: color.foreground,
        }}
      >
        {props.value}
      </Text>
    </View>
  );
}

/** The loading shape of the inventory: bars where the cards will be. */
function InventorySkeleton() {
  return (
    <View style={{ gap: space["2xl"] }} accessibilityLabel="正在读取插件清单">
      <View style={{ gap: space.md }}>
        <Skeleton width="42%" height={14} />
        <Skeleton lines={2} height={12} />
      </View>
      <View style={{ gap: space.md }}>
        <Skeleton width="38%" height={14} />
        <Skeleton lines={3} height={12} />
      </View>
    </View>
  );
}

/** A load failure: the server's own words, plus the retry. */
function LoadFailure(props: { text: string; onRetry: () => void }) {
  return (
    <Card
      testID="plugins-error"
      accessibilityLabel="插件清单加载失败"
      style={{
        gap: space.sm,
        padding: space.md,
        backgroundColor: color.dangerTint,
        borderColor: alpha(color.danger, 0.3),
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
        <Icon name="alert" size={iconSize.sm} tint={color.danger} />
        <Text
          style={{
            flex: 1,
            fontSize: typeScale.sm,
            lineHeight: Math.round(typeScale.sm * lineHeight.normal),
            color: color.foreground,
          }}
        >
          {props.text}
        </Text>
        <Button variant="outline" size="sm" label="重试" accessibilityLabel="重试加载插件清单" onPress={props.onRetry} />
      </View>
    </Card>
  );
}

// ---- a trigger row --------------------------------------------------------

/** The trigger's own record, without the contradiction a bare "未触发" makes
 * when the server counted fires but carries no `lastFireAt`. */
const triggerUsage = (status: TriggerStatusView | undefined): string => {
  if (!status) return "无触发记录";
  const last = formatTime(status.lastFireAt);
  const fires = status.fires;
  if (last) return fires !== undefined ? `上次 ${last} · ${fires} 次` : `上次 ${last}`;
  if (fires !== undefined && fires > 0) return `已触发 ${fires} 次`;
  return "未触发";
};

function TriggerTable(props: { pluginId: string; triggers: TriggerSpecView[] }) {
  const statusOf = (triggerId: string): TriggerStatusView | undefined =>
    store.triggers().find((row) => row.id === triggerId);
  return (
    <Table bare accessibilityLabel={`${props.pluginId} 的触发器`}>
      <TableHeaderRow>
        <Th>
          <Text style={headText()}>触发器</Text>
        </Th>
        <Th width="26%">
          <Text style={headText()}>计划</Text>
        </Th>
        <Th width="26%">
          <Text style={headText()}>状态</Text>
        </Th>
      </TableHeaderRow>
      <TableBody>
        <Each of={props.triggers} key={(trigger) => trigger.id}>
          {(trigger) => (
            <TableBodyRow>
              <Td>
                <View style={{ gap: space.xxs }}>
                  <Text style={{ fontSize: typeScale.sm, color: color.foreground, numberOfLines: 1 }}>
                    {trigger().id}
                  </Text>
                  <If when={!!trigger().prompt}>
                    <Text
                      style={{
                        fontSize: typeScale["2xs"],
                        lineHeight: Math.round(typeScale["2xs"] * lineHeight.normal),
                        color: color.mutedForeground,
                        numberOfLines: 1,
                      }}
                    >
                      {trigger().prompt ?? ""}
                    </Text>
                  </If>
                </View>
              </Td>
              <Td width="26%">
                <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
                  {[trigger().kind ?? "", trigger().schedule ?? ""].filter((part) => part.length > 0).join(" · ") ||
                    "—"}
                </Text>
              </Td>
              <Td width="26%">
                <View style={{ gap: space.xxs }}>
                  {/* A row of its own: a Badge in a column would stretch to the
                      cell's width (the pill hugs its label, never the column). */}
                  <View style={{ flexDirection: "row" }}>
                    <Badge
                      variant={trigger().enabled === false ? "secondary" : "success"}
                      label={trigger().enabled === false ? "停用" : "启用"}
                    />
                  </View>
                  <Text style={{ fontSize: typeScale["2xs"], color: color.mutedForeground, numberOfLines: 1 }}>
                    {triggerUsage(statusOf(trigger().id))}
                  </Text>
                </View>
              </Td>
            </TableBodyRow>
          )}
        </Each>
      </TableBody>
    </Table>
  );
}

// ---- one plugin -----------------------------------------------------------

function PluginCard(props: { plugin: () => PluginStatusView }) {
  const state = () => props.plugin().state ?? "unknown";
  const tools = () => props.plugin().tools ?? [];
  const hooks = () => props.plugin().hooks ?? [];
  const diagnostics = () => props.plugin().diagnostics ?? [];
  const triggers = () => props.plugin().triggers ?? [];
  const subtitle = () => {
    const parts = [
      props.plugin().version ? `v${props.plugin().version}` : "",
      props.plugin().startedAt ? `启动 ${formatTime(props.plugin().startedAt)}` : "",
      props.plugin().stoppedAt ? `停止 ${formatTime(props.plugin().stoppedAt)}` : "",
    ].filter((part) => part.length > 0);
    return parts.length > 0 ? parts.join(" · ") : undefined;
  };
  const outcome = () => {
    const row = browserOutcome();
    return row && row.id === props.plugin().id ? row : null;
  };
  return (
    <Card
      size="sm"
      testID={`plugin-${props.plugin().id}`}
      accessibilityLabel={`插件 ${props.plugin().id}`}
    >
      <CardHeader
        title={props.plugin().id}
        description={subtitle()}
        action={
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
            <Badge variant={pluginStateVariant(state())} label={state()} />
            <Button
              testID={`plugin-open-${props.plugin().id}`}
              variant="outline"
              size="sm"
              icon="external"
              label={platform.idiom === "terminal" ? "在浏览器打开" : "打开"}
              accessibilityLabel={`打开插件 ${props.plugin().id}`}
              onPress={() => openPlugin(props.plugin().id)}
            />
          </View>
        }
      />
      <CardContent>
        {/* 来源：the three contract fields that say where the plugin sits. */}
        <View style={{ flexDirection: "row", gap: space["2xl"], flexWrap: "wrap" }}>
          <Fact label="提供" value={(props.plugin().provides ?? []).join(" · ") || "—"} />
          <Fact label="依赖" value={(props.plugin().requires ?? []).join(" · ") || "无"} />
          <Fact label="效果" value={(props.plugin().effects ?? []).join(" · ") || "—"} />
        </View>

        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
          <Badge variant="secondary" label={`${tools().length} 工具`} />
          <Badge variant="secondary" label={`${hooks().length} hooks`} />
          <Badge variant="secondary" label={`${triggers().length} 触发器`} />
          <Text
            style={{
              fontFamily: font.mono,
              fontSize: typeScale["2xs"],
              color: color.mutedForeground,
              userSelect: "text",
            }}
          >
            {pluginPath(props.plugin().id)}
          </Text>
        </View>

        <If when={tools().length > 0}>
          <Text
            style={{
              fontFamily: font.mono,
              fontSize: typeScale.xs,
              lineHeight: Math.round(typeScale.xs * lineHeight.normal),
              color: color.mutedForeground,
              userSelect: "text",
            }}
          >
            {`工具：${tools().join(" · ")}`}
          </Text>
        </If>
        <If when={hooks().length > 0}>
          <Text
            style={{
              fontFamily: font.mono,
              fontSize: typeScale.xs,
              lineHeight: Math.round(typeScale.xs * lineHeight.normal),
              color: color.mutedForeground,
              userSelect: "text",
            }}
          >
            {`Hooks：${hooks().join(" · ")}`}
          </Text>
        </If>

        <If when={triggers().length > 0}>
          <TriggerTable pluginId={props.plugin().id} triggers={triggers()} />
        </If>
        <If when={triggers().length === 0}>
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>没有声明触发器。</Text>
        </If>

        <If when={diagnostics().length > 0}>
          <View style={{ gap: space.xxs }}>
            <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>诊断</Text>
            <Each of={diagnostics()} key="index">
              {(line) => (
                <Text
                  style={{
                    fontSize: typeScale.sm,
                    lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                    color: color.danger,
                    userSelect: "text",
                  }}
                >
                  {line()}
                </Text>
              )}
            </Each>
          </View>
        </If>

        {/* The terminal's browser hand-off: what openUrl answered, and — on a
            refusal — the address itself, clickable to try again. */}
        <If when={!!outcome()}>
          <View style={{ gap: space.xs }}>
            <Text
              style={{
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                color: outcome()?.ok ? color.success : color.danger,
              }}
            >
              {outcome()?.text ?? ""}
            </Text>
            <If when={outcome()?.ok === false}>
              <Button
                variant="link"
                size="sm"
                label={pluginUrl(props.plugin().id)}
                accessibilityLabel={`重新尝试打开 ${pluginUrl(props.plugin().id)}`}
                onPress={() => void openInBrowser(props.plugin().id, pluginUrl(props.plugin().id))}
              />
            </If>
          </View>
        </If>
      </CardContent>
    </Card>
  );
}

// ---- the inventory --------------------------------------------------------

function Inventory() {
  const plugins = () => store.plugins()?.plugins ?? [];
  const tools = () => store.plugins()?.tools ?? [];
  return (
    <ScrollArea
      testID="plugins-scroll"
      accessibilityLabel="插件面"
      style={{ flex: 1, minHeight: 0 }}
      contentStyle={{ flexDirection: "column" }}
    >
      <View
        style={{
          width: "100%",
          maxWidth: SETTINGS_CONTENT_MAX_WIDTH_PX,
          alignSelf: "center",
          padding: space.xl,
          gap: space["2xl"],
        }}
      >
        <Card testID="plugins-inventory">
          <CardHeader
            title="已装插件"
            description="插件在进程启动时装配；状态、来源与触发声明来自 GET /v1/plugins，触发次数来自 GET /v1/triggers。"
            action={
              <Button
                variant="ghost"
                size="sm"
                icon="refresh"
                label="刷新"
                accessibilityLabel="刷新插件清单"
                loading={store.pluginsLoading()}
                onPress={() => void store.loadPlugins()}
              />
            }
          />
          <CardContent>
            <If when={!store.plugins() && !store.pluginsError()}>
              <InventorySkeleton />
            </If>
            <If when={!!store.pluginsError()}>
              <LoadFailure
                text={store.pluginsError() ?? ""}
                onRetry={() => void store.loadPlugins()}
              />
            </If>
            <If when={!!store.plugins()}>
              <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
                <Badge variant="info" label={`generation ${store.plugins()?.generation ?? "-"}`} />
                <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
                  {`${plugins().length} 个插件 · ${tools().length} 个工具`}
                </Text>
              </View>
              <If when={plugins().length === 0}>
                <Empty testID="plugins-empty" accessibilityLabel="没有已装配的插件">
                  <EmptyMedia icon="layers" />
                  <EmptyTitle label="没有已装配的插件" />
                  <EmptyDescription label="插件随进程启动装配；装配完成后刷新本页。" />
                </Empty>
              </If>
              <View style={{ gap: space.lg }}>
                <Each of={plugins()} key={(plugin) => plugin.id}>
                  {(plugin) => <PluginCard plugin={plugin} />}
                </Each>
              </View>
              <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
                状态：active（已装配）· pending（等待依赖）· failed（装配失败）· stopped（已停止）。
              </Text>
            </If>
          </CardContent>
        </Card>

        <Card testID="plugins-available">
          <CardHeader
            title="可装与更新"
            description="GET /v1/plugins 只报告已装配插件的状态；契约里没有可装清单，也没有版本对比字段。"
          />
          <CardContent>
            <Empty accessibilityLabel="可装与更新">
              <EmptyMedia icon="layers" />
              <EmptyTitle label="没有可装或可更新的条目可以显示" />
              <EmptyDescription label="可装清单、版本检查与安装动作都依赖 keel-plugin-core（插件 UI 路由与安装 API）。它落地之前，这里保持空态，不编造条目。" />
            </Empty>
          </CardContent>
        </Card>

        <Card testID="plugins-create">
          <CardHeader
            title="创造模式"
            description="用对话描述需要的插件：能力、依赖与触发方式先说清楚，再由对话产出清单与安装步骤。"
          />
          <CardContent>
            <Text
              style={{
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                color: color.mutedForeground,
              }}
            >
              安装动作依赖 keel-plugin-core 的安装 API；本版只提供入口与状态说明，不执行安装。
            </Text>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
              <Button
                testID="plugins-create-start"
                icon="spark"
                label="打开对话并预填"
                accessibilityLabel="打开对话并预填插件需求"
                onPress={startCreation}
              />
              <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
                只预填，不发送。
              </Text>
            </View>
          </CardContent>
        </Card>
      </View>
    </ScrollArea>
  );
}

// ---- one plugin's page (web) ----------------------------------------------

/** The plugin's own page, carried by the platform's web view. The terminal
 * never reaches this component (`openPlugin` hands the address to the browser
 * there), so no web view is ever created in a terminal.
 *
 * The page is mounted only after its address answered 2xx: the route belongs to
 * keel-plugin-core and is not served yet, and a raw 404 inside the frame would
 * read as a broken app rather than as a missing dependency. */
function PluginPage(props: { id: string }) {
  const url = () => pluginUrl(props.id);
  const state = () => pageProbe().state;
  const detail = () => {
    const probe = pageProbe();
    return probe.state === "missing" ? probe.detail : "";
  };
  return (
    <View style={{ flex: 1, minHeight: 0, padding: space.xl, gap: space.md }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
        <Badge variant="outline" label="插件页面" />
        <Text
          style={{
            fontFamily: font.mono,
            fontSize: typeScale.xs,
            color: color.mutedForeground,
            userSelect: "text",
            numberOfLines: 1,
          }}
        >
          {pluginPath(props.id)}
        </Text>
        <View style={{ flex: 1 }} />
        <Button
          variant="ghost"
          size="sm"
          icon="refresh"
          label="重新检查"
          accessibilityLabel={`重新检查 ${pluginPath(props.id)}`}
          onPress={() => void probePluginPage(props.id)}
        />
      </View>

      <If when={state() === "checking"}>
        <View style={{ gap: space.md }} accessibilityLabel="正在读取插件页面">
          <Skeleton width="46%" height={14} />
          <Skeleton lines={3} height={12} />
        </View>
      </If>

      <If when={state() === "missing"}>
        <Card
          testID="plugin-page-missing"
          accessibilityLabel="插件页面尚未提供"
          style={{
            gap: space.md,
            padding: space.lg,
            backgroundColor: color.warningTint,
            borderColor: alpha(color.warning, 0.3),
          }}
        >
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
            <Icon name="alert" size={iconSize.sm} tint={color.warning} />
            <Text
              style={{
                flex: 1,
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                color: color.foreground,
                userSelect: "text",
              }}
            >
              {`插件页面尚未提供（${detail()}）。插件 UI 路由由 keel-plugin-core 提供，尚未落地。`}
            </Text>
          </View>
          <Text
            style={{
              fontFamily: font.mono,
              fontSize: typeScale.xs,
              color: color.mutedForeground,
              userSelect: "text",
            }}
          >
            {url()}
          </Text>
        </Card>
      </If>

      <If when={state() === "ready"}>
        <If
          when={!!webviewError()}
          else={
            <WebView
              testID={`plugin-webview-${props.id}`}
              src={url()}
              style={{
                flex: 1,
                minHeight: 0,
                borderRadius: radius.lg,
                borderWidth: 1,
                borderColor: color.border,
                backgroundColor: color.card,
              }}
              onError={(error) =>
                webviewError.set(error.code ? `${error.code}：${error.message}` : error.message)
              }
            />
          }
        >
          <Card
            testID="plugin-webview-error"
            accessibilityLabel="插件页面加载失败"
            style={{
              gap: space.md,
              padding: space.lg,
              backgroundColor: color.dangerTint,
              borderColor: alpha(color.danger, 0.3),
            }}
          >
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
              <Icon name="alert" size={iconSize.sm} tint={color.danger} />
              <Text
                style={{
                  flex: 1,
                  fontSize: typeScale.sm,
                  lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                  color: color.foreground,
                  userSelect: "text",
                }}
              >
                {webviewError() ?? ""}
              </Text>
            </View>
            <Text
              style={{
                fontFamily: font.mono,
                fontSize: typeScale.xs,
                color: color.mutedForeground,
                userSelect: "text",
              }}
            >
              {url()}
            </Text>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
              <Button
                variant="outline"
                size="sm"
                icon="refresh"
                label="重新检查"
                accessibilityLabel="重新检查插件页面"
                onPress={() => void probePluginPage(props.id)}
              />
              <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
                页面来源与安装动作由 keel-plugin-core 提供。
              </Text>
            </View>
          </Card>
        </If>
      </If>
    </View>
  );
}

// ---- the page -------------------------------------------------------------

/** `/plugins` — the plugin face. `PluginsScreen` is the name the app root
 * expects (`web/App.tsx:341-346`). */
export function PluginsScreen() {
  // The inventory loads on entry, out of the effect's own turn (a mount-time
  // load writes its loading signals synchronously — the same rule the settings
  // page follows, `src/screens/settings.tsx:1935-1945`).
  effect(() => {
    if (store.mode() === "checking") return;
    const timer = setTimeout(() => void store.loadPlugins(), 0);
    cleanup(() => clearTimeout(timer));
  });

  effect(() => {
    readOrigin();
  });

  // The address bar says `/plugins` while this page shows (a deep link already
  // matches and the push is skipped). The plugin's own page keeps its address
  // in the web view — `/plugins/<id>` is the plugin server's route, not this
  // client's, so the app's URL never takes it over.
  effect(() => {
    navigate("/plugins");
  });

  return (
    <View
      testID="plugins-page"
      style={{ flex: 1, minWidth: 0, backgroundColor: color.background }}
    >
      <View
        testID="plugins-header"
        style={{
          height: WORKSPACE_SHELL_HEADER_HEIGHT_PX,
          flexShrink: 0,
          flexDirection: "row",
          alignItems: "center",
          gap: space.md,
          paddingHorizontal: space.xl,
          borderBottomWidth: 1,
          borderBottomColor: color.border,
        }}
      >
        <If
          when={!!openId()}
          else={
            <Button
              variant="ghost"
              size="sm"
              icon="chevron-left"
              label="返回工作区"
              accessibilityLabel="返回工作区"
              onPress={() => store.closeSettings()}
            />
          }
        >
          <Button
            variant="ghost"
            size="sm"
            icon="chevron-left"
            label="插件列表"
            accessibilityLabel="返回插件列表"
            testID="plugins-back"
            onPress={closePlugin}
          />
        </If>
        <Text
          style={{
            flexShrink: 1,
            fontSize: typeScale.base,
            fontWeight: weight.semibold,
            color: color.foreground,
            numberOfLines: 1,
          }}
        >
          {openId() ?? "插件"}
        </Text>
        <If when={!!openId()}>
          <Text
            style={{
              flex: 1,
              fontFamily: font.mono,
              fontSize: typeScale.xs,
              color: color.mutedForeground,
              numberOfLines: 1,
              userSelect: "text",
            }}
          >
            {pluginPath(openId() ?? "")}
          </Text>
        </If>
        <If when={!openId()}>
          {/* One generation marker per screen: it lives in the inventory card
              beside the counts it describes, not in the page chrome too. */}
          <View style={{ flex: 1 }} />
        </If>
      </View>
      <If when={openId()} else={<Inventory />}>
        {(id) => <PluginPage id={id()} />}
      </If>
    </View>
  );
}
