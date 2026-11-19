// The MCP App View (change feat-mcp-apps-events, P2.6): one iframe per app,
// the bridge state that survives the row's streaming re-renders, and the kit's
// dialog for the fullscreen display mode.
//
// NSH's web backend renders `<WebView>` as an `<iframe>`; for a same-origin
// document it sets the sandbox **without** `allow-same-origin` (the runtime's
// `op_web`: `same = new URL(url, location.href).origin === location.origin`,
// and only a cross-origin source keeps its own origin). So the app document is
// an opaque-origin frame: it cannot read the host's DOM, storage or cookies,
// and a same-origin `fetch` from it is a credential-less cross-origin request
// that fails. The response CSP (`default-src 'none'`, `frame-ancestors 'self'`)
// arrives with the document from `GET /v1/mcp/apps/{appId}/view`.
//
// The NSH web view also adds `allow-forms allow-popups allow-modals
// allow-downloads` to every frame (the runtime's fixed list). It cannot be
// narrowed from app code in 0.4 nightly; the bridge treats the frame as
// untrusted regardless, and the deviation is recorded in the P2 evidence.
import { cleanup, colorScheme, effect, locale as deviceLocale, platform, signal, Text, View, WebView, windowSize } from "nsh";
import type { WebViewRef } from "nsh";
import type { McpAppView } from "../../data/contract";
import { appDocumentSource, callToolResultFor, themeVariablesFor } from "../../data/mcp-apps";
import { store } from "../../data/store";
import { Icon } from "../../ui/icons";
import { Badge, Button, color, Dialog, font, radius, space, typeScale, weight } from "../../ui/kit";
import {
  createAppBridge,
  INLINE_MAX_HEIGHT,
  INLINE_MIN_HEIGHT,
  type AppBridge,
  type BridgeFailure,
  type BridgePhase,
  type DisplayMode,
} from "./bridge";
import {
  createReloadBudget,
  MAX_NAVIGATION_RELOADS,
  reloadBudgetAuditEntry,
  type ReloadBudget,
} from "./reload-budget";
import { registerWebFrame, webPageOrigin, webTransportAvailable } from "./web-transport";

const INLINE_DEFAULT_HEIGHT = 220;
const FULLSCREEN_HEIGHT_RATIO = 0.82;

export interface McpAppViewProps {
  callId: string;
  app: McpAppView;
  args: unknown;
  theme?: Record<string, Record<string, string>>;
  sandboxOrigin?: string;
}

type Getter<T> = () => T;

interface SessionSignals {
  phase: Getter<BridgePhase>;
  diagnostic: Getter<BridgeFailure | null>;
  height: Getter<number>;
  mode: Getter<DisplayMode>;
  reload: Getter<number>;
  setPhase: (value: BridgePhase) => void;
  setDiagnostic: (value: BridgeFailure | null) => void;
  setHeight: (value: number) => void;
  setMode: (value: DisplayMode) => void;
  setReload: (value: number) => void;
}

interface AppSession {
  live: McpAppViewProps;
  ref: { current?: WebViewRef };
  signals: SessionSignals;
  bridge: AppBridge;
  /** The per-View routing nonce (`&instance=`), matched against the iframe's
   * `src` when a message arrives from the page window. */
  instance: string;
  /** A navigation reload is queued (coalesces a sibling load event). */
  reloadScheduled: boolean;
  /** Automatic navigation reloads allowed for this instance. */
  budget: ReloadBudget;
  disposed: boolean;
  cleanupRegistered: boolean;
}

const sessions = new Map<string, AppSession>();

let instanceSequence = 0;

/** A routing nonce for one View instance: lowercase alphanumerics, 16–64, so
 * the document URL's `instance` parameter validates. */
function newInstance(): string {
  const cryptoAny = globalThis.crypto as { randomUUID?: () => string } | undefined;
  const raw = cryptoAny?.randomUUID?.() ?? `${Date.now().toString(36)}${Math.random().toString(36).slice(2)}`;
  const clean = raw.toLowerCase().replace(/[^a-z0-9]/g, "");
  const sequence = (++instanceSequence).toString(36);
  return `${clean}${sequence}`.padEnd(16, "0").slice(0, 40);
}

interface WebOriginState {
  status: "idle" | "loading" | "ready" | "error";
  origin?: string;
  error?: string;
}

/** The page's own origin (web target): `keel.info` answers it; the app has no
 * `location` of its own. One probe per launch, retryable. */
const webOrigin = signal<WebOriginState>({ status: "idle" });

const messageOf = (error: unknown): string => {
  if (error instanceof Error && error.message) return error.message;
  const value = error as { message?: unknown } | null;
  return typeof value?.message === "string" && value.message ? value.message : String(error);
};

function ensureWebOrigin(force = false): void {
  if (platform.os !== "web") return;
  const current = webOrigin();
  if (!force && (current.status === "loading" || current.status === "ready" || current.status === "error")) return;
  webOrigin.set({ status: "loading" });
  void webPageOrigin()
    .then((origin) => {
      webOrigin.set({ status: "ready", origin });
    })
    .catch((error: unknown) => {
      webOrigin.set({ status: "error", error: messageOf(error) });
    });
}

const titleOf = (app: McpAppView): string => app.title?.trim() || app.server || "交互界面";

function platformName(): "web" | "desktop" | "mobile" {
  if (platform.os === "web") return "web";
  return platform.idiom === "desktop" ? "desktop" : "mobile";
}

function hostContextFor(props: McpAppViewProps, mode: DisplayMode): Record<string, unknown> {
  const scheme = colorScheme();
  const variables = themeVariablesFor(props.theme, scheme);
  const context: Record<string, unknown> = {
    theme: scheme,
    displayMode: mode,
    availableDisplayModes: ["inline", "fullscreen"],
    // Inline is flexible up to the host's cap; fullscreen takes the window's
    // height (the dialog's own body). Fixed dimensions are never invented.
    containerDimensions:
      mode === "fullscreen"
        ? { maxHeight: Math.max(320, Math.round(windowSize().height * FULLSCREEN_HEIGHT_RATIO)) }
        : { maxHeight: INLINE_MAX_HEIGHT },
    platform: platformName(),
    deviceCapabilities: { touch: platform.idiom === "phone" || platform.idiom === "pad" },
  };
  if (variables) context.styles = { variables };
  try {
    const tag = deviceLocale();
    if (tag) context.locale = tag;
  } catch {
    /* a host without a locale omits the section (no empty values) */
  }
  try {
    const timeZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    if (timeZone) context.timeZone = timeZone;
  } catch {
    /* no Intl: omit */
  }
  return context;
}

/** Only what this host actually wires is advertised. `tools/call`,
 * `resources/read`, links and model context are P3/P5; permissions are granted
 * only after the user's confirmation (P3.6), so nothing is claimed here. */
function hostCapabilitiesFor(app: McpAppView): Record<string, unknown> {
  const capabilities: Record<string, unknown> = {
    // `notifications/message` is accepted (one-way host log), so `logging` is
    // a real capability, not a promise.
    logging: {},
  };
  if (app.cspDomains) capabilities.sandbox = { csp: app.cspDomains };
  return capabilities;
}

function buildBridge(session: AppSession): AppBridge {
  const post = (frame: Record<string, unknown>) => {
    const web = session.ref.current;
    if (!web) {
      session.bridge?.destroy({ kind: "transport", message: "交互界面通道不可用" });
      return;
    }
    try {
      web.postMessage(JSON.stringify(frame));
    } catch {
      session.bridge?.destroy({ kind: "transport", message: "消息无法序列化" });
    }
  };
  return createAppBridge({
    post,
    toolInput: () => session.live.args,
    toolResult: () => callToolResultFor(store.toolResultFor(session.live.callId)),
    hostCapabilities: () => hostCapabilitiesFor(session.live.app),
    hostContext: () => hostContextFor(session.live, session.signals.mode()),
    app: () => session.live.app,
    sandboxOrigin: () => session.live.sandboxOrigin,
    setDisplayMode: (mode) => {
      session.signals.setMode(mode);
      return mode;
    },
    onSize: (size) => {
      if (!session.disposed && size.height !== undefined) session.signals.setHeight(size.height);
    },
    onDisplayMode: (mode) => {
      if (!session.disposed) session.signals.setMode(mode);
    },
    onPhase: (phase) => {
      if (!session.disposed) session.signals.setPhase(phase);
    },
    onDiagnostic: (failure) => {
      if (!session.disposed) session.signals.setDiagnostic(failure);
    },
    onDestroyed: (failure) => {
      if (!session.disposed) session.signals.setDiagnostic(failure);
    },
    onTeardown: () => {
      /* the View is unmounted by the component's cleanup either way */
    },
  });
}

function createSession(callId: string, props: McpAppViewProps): AppSession {
  const phase = signal<BridgePhase>("idle");
  const diagnostic = signal<BridgeFailure | null>(null);
  const height = signal(INLINE_DEFAULT_HEIGHT);
  const mode = signal<DisplayMode>("inline");
  const reload = signal(0);
  const session: AppSession = {
    live: props,
    ref: {},
    signals: {
      phase,
      diagnostic,
      height,
      mode,
      reload,
      setPhase: (value) => phase.set(value),
      setDiagnostic: (value) => diagnostic.set(value),
      setHeight: (value) => height.set(value),
      setMode: (value) => mode.set(value),
      setReload: (value) => reload.set(value),
    },
    bridge: undefined as unknown as AppBridge,
    instance: newInstance(),
    reloadScheduled: false,
    budget: createReloadBudget(),
    disposed: false,
    cleanupRegistered: false,
  };
  session.bridge = buildBridge(session);
  sessions.set(callId, session);
  return session;
}

function sessionFor(callId: string, props: McpAppViewProps): AppSession {
  const existing = sessions.get(callId);
  if (existing) {
    existing.live = props;
    return existing;
  }
  return createSession(callId, props);
}

/** New View instance: the old bridge is gone and the next `ui/initialize`
 * starts clean. `reload` bumps the URL's generation so NSH loads the app
 * document again (a navigation may have replaced it). */
function resetBridge(session: AppSession, reason: string): void {
  const old = session.bridge;
  old.destroy({ kind: "unmounted", message: reason });
  session.signals.setPhase("idle");
  session.signals.setDiagnostic(null);
  session.signals.setHeight(INLINE_DEFAULT_HEIGHT);
  session.bridge = buildBridge(session);
}

function restartSession(session: AppSession, reload = false): void {
  session.reloadScheduled = false;
  if (reload) session.signals.setReload(session.signals.reload() + 1);
  resetBridge(session, reload ? "文档重新加载" : "实例重建");
}

/** Stop a self-navigating app: no new instance, the text fallback stays, and
 * the host records a query-free `protocol.violation` audit entry. */
function stopForReloadBudget(session: AppSession): void {
  const failure: BridgeFailure = {
    kind: "protocol",
    message: `交互界面反复重载（超过 ${MAX_NAVIGATION_RELOADS} 次），已停止`,
  };
  session.bridge.destroy(failure);
  session.signals.setPhase("destroyed");
  session.signals.setDiagnostic(failure);
  const sessionId = store.activeId();
  void store.reportMcpAppViolation(
    reloadBudgetAuditEntry(
      session.live.app.appId,
      session.live.app.server,
      sessionId ?? undefined,
    ),
  );
}

/** Coalesce a navigation's load + URL events into one reload, within the
 * instance's budget. */
function scheduleReload(session: AppSession): void {
  if (session.reloadScheduled || session.disposed) return;
  session.reloadScheduled = true;
  setTimeout(() => {
    if (session.disposed) return;
    session.reloadScheduled = false;
    if (session.budget.consume() === "exhausted") {
      stopForReloadBudget(session);
      return;
    }
    console.warn(`[mcp-app ${session.live.app.appId}] 文档导航/重载：丢弃旧实例并重新握手`);
    restartSession(session, true);
  }, 0);
}

function disposeSession(callId: string, session: AppSession): void {
  session.disposed = true;
  session.bridge.teardown();
  session.bridge.destroy({ kind: "unmounted", message: "组件已卸载" });
  sessions.delete(callId);
}

/** The app's frame and controls. The text result stays in `messages.tsx`
 * below this block: a failed View is always readable. */
export function McpAppView(props: McpAppViewProps) {
  const session = sessionFor(props.callId, props);
  if (!session.cleanupRegistered) {
    session.cleanupRegistered = true;
    cleanup(() => disposeSession(props.callId, session));
  }

  const destroyed = () => session.signals.phase() === "destroyed";
  const source = () =>
    appDocumentSource(props.app, {
      web: platform.os === "web",
      origin: webOrigin().origin,
      sandboxOrigin: props.sandboxOrigin,
      instance: session.instance,
      reload: session.signals.reload(),
    });
  const url = () => {
    const resolution = source();
    return "url" in resolution ? resolution.url : null;
  };
  const sourceError = () => {
    const resolution = source();
    return "error" in resolution ? resolution.error : null;
  };
  /** The document's content identity (not the per-instance query): a catalogue
   * reload with a new hash is a new View and restarts the bridge. */
  const documentKey = () => `${props.app.appId}:${props.app.hash}`;
  const title = () => titleOf(props.app);

  if (platform.os === "web") {
    // The component body runs once; the probe is idempotent and sets the
    // loading state synchronously, so the frame's branch never waits on an
    // effect registration to start.
    ensureWebOrigin();
    // Route this View's frames from the page window by its `instance` nonce.
    // The handler reads `session.bridge` when a frame arrives, so a rebuilt
    // instance uses the current bridge.
    const unregister = registerWebFrame(session.instance, (data, origin) => {
      session.bridge.handleMessage(data, origin);
    });
    cleanup(unregister);
    if (!webTransportAvailable() && session.signals.diagnostic() === null) {
      session.signals.setDiagnostic({
        kind: "transport",
        message: "web 页面宿主不可用，交互界面无法接收消息",
      });
    }
  }

  // The result arrives after the call row is already on screen; this is the
  // one path that hands it to the bridge (exactly once is the bridge's rule).
  effect(() => {
    session.bridge.updateToolResult(callToolResultFor(store.toolResultFor(props.callId)));
  });

  // Theme changes are a partial host-context update (SHOULD), never a reload.
  let lastScheme = colorScheme();
  effect(() => {
    const scheme = colorScheme();
    if (scheme === lastScheme) return;
    lastScheme = scheme;
    const variables = themeVariablesFor(props.theme, scheme);
    session.bridge.hostContextChanged(
      variables ? { theme: scheme, styles: { variables } } : { theme: scheme },
    );
  });

  // A display-mode change mounts the frame in its new home; the bridge belongs
  // to the new instance, so it is rebuilt after the old one answered.
  let lastMode = session.signals.mode();
  effect(() => {
    const mode = session.signals.mode();
    if (mode === lastMode) return;
    lastMode = mode;
    resetBridge(session, "显示模式变化");
  });

  // A content change (catalogue reload with a new hash) is a new document and
  // therefore a new View instance: the next initialize must start a new bridge.
  let lastDocument = documentKey();
  effect(() => {
    const key = documentKey();
    if (key === lastDocument) return;
    lastDocument = key;
    resetBridge(session, "文档版本变化");
  });

  const statusLabel = () => {
    const phase = session.signals.phase();
    if (phase === "destroyed") return "已回退";
    if (phase === "ready") return "已连接";
    return "连接中";
  };
  const statusVariant = () => {
    const phase = session.signals.phase();
    return phase === "destroyed" ? "destructive" : phase === "ready" ? "success" : "info";
  };

  const requestMode = (mode: DisplayMode) => {
    if (session.signals.mode() !== mode) session.signals.setMode(mode);
  };

  /** A load that is not the first one means the frame replaced the app
   * document (in-frame navigation). The old instance is discarded and the
   * host's own URL is loaded again: a third-party page must never inherit the
   * already-authorized channel and its tool-input/tool-result. */
  const onFrameLoad = () => {
    const snapshot = session.bridge.snapshot();
    if (snapshot.initialized || snapshot.phase === "handshaking") {
      scheduleReload(session);
      return;
    }
    session.bridge.start();
  };

  const sameDocument = (actual: string, expected: string): boolean => {
    try {
      const a = new URL(actual);
      const b = new URL(expected);
      return a.origin === b.origin && a.pathname === b.pathname;
    } catch {
      return actual === expected;
    }
  };

  /** URL-level navigation defense for **native** targets (macOS/Windows/Linux/
   * iOS/Android): those WebViews report the real navigation URL, so a mismatch
   * against the app document is a second line behind the load restart. On the
   * **web** target this handler is inert by design: NSH's `webNavigation`
   * always reports the URL the view was given (the browser hides in-frame
   * navigations), so a self-navigation still reports the original URL here.
   * The load-event restart above is the defense that actually works on web. */
  const onFrameNavigation = (event: { url?: string }) => {
    if (platform.os === "web") return;
    const expected = url();
    const actual = typeof event?.url === "string" ? event.url : "";
    if (!expected || !actual) return;
    if (sameDocument(actual, expected)) return;
    scheduleReload(session);
  };

  const webLoading = () =>
    platform.os === "web" && (webOrigin().status === "idle" || webOrigin().status === "loading");
  const webOriginError = () => platform.os === "web" && webOrigin().status === "error";

  const loadingRow = () => (
    <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, padding: space.md }}>
      <Icon name="clock" size={14} tint={color.mutedForeground} />
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>正在读取页面来源…</Text>
    </View>
  );

  const originErrorRow = () => (
    <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, padding: space.md }}>
      <Icon name="alert" size={14} tint={color.warning} />
      <Text style={{ flex: 1, fontSize: typeScale.sm, color: color.mutedForeground }}>
        无法读取页面来源：{webOrigin().error ?? "未知原因"}（文本结果仍可阅读）
      </Text>
      <Button
        variant="outline"
        size="sm"
        icon="refresh"
        label="重试"
        accessibilityLabel="重试读取页面来源"
        onPress={() => ensureWebOrigin(true)}
      />
    </View>
  );

  const sourceErrorRow = () => (
    <Text style={{ fontSize: typeScale.sm, color: color.warning, userSelect: "text" }}>
      {sourceError() ?? "交互界面地址无效"}，已回退为文本结果。
    </Text>
  );

  const frameContent = (src: string) => (
    <WebView
      ref={(node) => {
        session.ref.current = node;
      }}
      src={src}
      testID={`mcp-app-frame-${props.callId}`}
      style={{
        width: "100%",
        height: session.signals.height(),
        minHeight: INLINE_MIN_HEIGHT,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
        backgroundColor: color.card,
      }}
      onLoad={onFrameLoad}
      onNavigationStateChange={onFrameNavigation}
      // The URL-form web view has no page→host channel (`onMessage` is only
      // wired for the srcdoc form); the web target uses the page-window
      // transport above. Native web views keep their per-view channel.
      onMessage={(event) => {
        if (platform.os !== "web") session.bridge.handleMessage(event.data);
      }}
      onError={(error) =>
        session.bridge.destroy({
          kind: "transport",
          message: `交互界面加载失败${error.code ? `（${error.code}）` : ""}：${error.message}`,
        })
      }
    />
  );

  /** Reactive branches: the page origin is probed asynchronously, so the frame
   * must mount when its signal turns ready (a plain `if` here would freeze the
   * first reading). */
  const frame = () => (
    <View style={{ flexDirection: "column" }}>
      <If when={webLoading()}>{() => loadingRow()}</If>
      <If when={webOriginError()}>{() => originErrorRow()}</If>
      <If when={!webLoading() && !webOriginError()}>
        {() => (
          <If when={url()} else={sourceErrorRow()}>
            {(src) => frameContent(src())}
          </If>
        )}
      </If>
    </View>
  );

  const fallback = () => (
    <View
      testID={`mcp-app-fallback-${props.callId}`}
      accessibilityLabel="交互界面不可用"
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.md,
        padding: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
        backgroundColor: color.muted,
      }}
    >
      <Icon name="alert" size={14} tint={color.warning} />
      <Text style={{ flex: 1, fontSize: typeScale.sm, color: color.mutedForeground, userSelect: "text" }}>
        {session.signals.diagnostic()?.message ?? "交互界面不可用"}（文本结果仍可阅读）
      </Text>
      <Button
        variant="outline"
        size="sm"
        icon="refresh"
        label="重试"
        accessibilityLabel="重试交互界面"
        onPress={() => {
          session.budget = createReloadBudget();
          restartSession(session, true);
        }}
      />
    </View>
  );

  const panel = (fullscreen: boolean) => (
    <View
      style={{
        flexDirection: "column",
        gap: space.sm,
        padding: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
        backgroundColor: color.card,
        flex: fullscreen ? 1 : undefined,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <Icon name="panel" size={14} tint={color.mutedForeground} />
        <Text
          style={{
            flex: 1,
            fontSize: typeScale.sm,
            fontWeight: weight.semibold,
            color: color.foreground,
            numberOfLines: 1,
          }}
        >
          {title()}
        </Text>
        <Badge variant={statusVariant()} label={statusLabel()} />
        <Button
          variant="ghost"
          size="sm"
          icon={fullscreen ? "chevron-down" : "external"}
          label={fullscreen ? "退出全屏" : "全屏"}
          accessibilityLabel={fullscreen ? "退出全屏" : "全屏显示交互界面"}
          onPress={() => requestMode(fullscreen ? "inline" : "fullscreen")}
        />
      </View>
      <If when={destroyed()} else={frame()}>
        {() => fallback()}
      </If>
      <Text
        style={{
          fontFamily: font.mono,
          fontSize: typeScale.xs,
          color: color.mutedForeground,
          numberOfLines: 1,
        }}
      >
        {props.app.resourceUri}
      </Text>
    </View>
  );

  return (
    <View style={{ flexDirection: "column" }}>
      <If when={session.signals.mode() === "fullscreen"} else={panel(false)}>
        <Dialog
          open
          title={title()}
          description="交互界面正在全屏显示。"
          testID={`mcp-app-fullscreen-${props.callId}`}
          accessibilityLabel={`${title()} 全屏`}
          closeOnBackdropPress
          onOpenChange={(event) => {
            if (!event.open) requestMode("inline");
          }}
          style={{
            width: "92%",
            maxWidth: "92%",
            height: "88%",
            maxHeight: "88%",
          }}
        >
          <View style={{ flex: 1, minHeight: 0 }}>{panel(true)}</View>
        </Dialog>
      </If>
    </View>
  );
}
