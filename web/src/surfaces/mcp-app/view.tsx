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
  setPhase: (value: BridgePhase) => void;
  setDiagnostic: (value: BridgeFailure | null) => void;
  setHeight: (value: number) => void;
  setMode: (value: DisplayMode) => void;
}

interface AppSession {
  live: McpAppViewProps;
  ref: { current?: WebViewRef };
  signals: SessionSignals;
  bridge: AppBridge;
  disposed: boolean;
  cleanupRegistered: boolean;
}

const sessions = new Map<string, AppSession>();

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
  const session: AppSession = {
    live: props,
    ref: {},
    signals: {
      phase,
      diagnostic,
      height,
      mode,
      setPhase: (value) => phase.set(value),
      setDiagnostic: (value) => diagnostic.set(value),
      setHeight: (value) => height.set(value),
      setMode: (value) => mode.set(value),
    },
    bridge: undefined as unknown as AppBridge,
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

/** New View instance (retry or display-mode change): the old bridge is gone
 * and the iframe mounts again, so the next `ui/initialize` starts clean. */
function restartSession(session: AppSession): void {
  const old = session.bridge;
  old.destroy({ kind: "unmounted", message: "实例重建" });
  session.signals.setPhase("idle");
  session.signals.setDiagnostic(null);
  session.signals.setHeight(INLINE_DEFAULT_HEIGHT);
  session.bridge = buildBridge(session);
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
      sandboxOrigin: props.sandboxOrigin,
      web: platform.os === "web",
    });
  const url = () => {
    const resolution = source();
    return "url" in resolution ? resolution.url : null;
  };
  const sourceError = () => {
    const resolution = source();
    return "error" in resolution ? resolution.error : null;
  };
  const title = () => titleOf(props.app);

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
    restartSession(session);
  });

  // A content change (catalogue reload with a new hash) is a new document and
  // therefore a new View instance: the next initialize must start a new bridge.
  let lastSrc = url();
  effect(() => {
    const src = url();
    if (src === lastSrc) return;
    lastSrc = src;
    restartSession(session);
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

  const frame = () => {
    const src = url();
    if (!src) {
      return (
        <Text style={{ fontSize: typeScale.sm, color: color.warning, userSelect: "text" }}>
          {sourceError() ?? "交互界面地址无效"}，已回退为文本结果。
        </Text>
      );
    }
    return (
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
        onLoad={() => session.bridge.start()}
        onMessage={(event) => session.bridge.handleMessage(event.data)}
        onError={(error) =>
          session.bridge.destroy({
            kind: "transport",
            message: `交互界面加载失败${error.code ? `（${error.code}）` : ""}：${error.message}`,
          })
        }
      />
    );
  };

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
        onPress={() => restartSession(session)}
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
        {fallback()}
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
