// The global system bar — the shell IA change.
//
// One bar for every route: the brand / window-drag area on the left, the
// plugin top-band slot in the middle, and the system controls on the right
// (sound on/off + sound settings, the notification panel, the light/dark
// switch, the command palette, and window controls only where the host lets an
// app draw them). It replaces the books shell's per-page brand strip and the
// settings page's empty title band; each screen keeps its own toolbar below it.
//
// Host rules this file follows:
//   · window controls come from `nsh/desktop` only. `isAvailable` is false on
//     the web and in a terminal, and a title bar that reserves native control
//     width (`titleBar().left/right` non-zero: macOS traffic lights, the
//     Windows/Linux caption buttons) already has the system's own controls — in
//     either case the app draws none, never a fake button. Where it does draw
//     them, each view carries `desktop={{ windowControl }}` and the platform
//     carries out its press, so no JS handler can run it twice.
//   · a terminal draws no icons, so each action falls back to a short text
//     button with the same accessibility label (the kit's own `HeaderAction`
//     pattern in App.tsx).
//   · the plugin slot keeps its contract: a contribution renders in
//     `books-top`, and with none the same area is only the drag region, never
//     an empty panel.
import type { JSX } from "nsh";
import { colorScheme, effect, platform, signal, Text, View } from "nsh";
import {
  isAvailable as desktopIsAvailable,
  currentWindow,
  titleBar,
  type WindowRef,
} from "nsh/desktop";
import { store, type Notice } from "../data/store";
import { sessionTitle } from "../ui/session-title";
import { applyAppearance } from "../ui/appearance";
import {
  previewSoundCue,
  setSoundEnabled,
  setSoundVolume,
  soundAvailable,
  soundPreferences,
  type SoundVolume,
} from "../data/sound";
import {
  Button,
  Dialog,
  GLASS_BLUR,
  IconButton,
  Switch,
  color,
  radius,
  space,
  typeScale,
  weight,
} from "../ui/kit";
import { Icon, type IconName } from "../ui/icons";
import { BOOK_TOUCH_PX, LiveBadge, PressRow, formatWhen, isMobile } from "./books/common";
import { BrandMark, PluginTopPanels, topBandHasContent } from "./books/plugin-top-panels";
import { openPalette } from "./command-palette";

/** The bar's height: the kit's h-12 title band. */
export const SYSTEM_BAR_HEIGHT_PX = 48;

/** The notification panel and the sound dialog are module-level, like the
 * app's other overlays, so the bar and a future shortcut share one flag. */
export const notificationPanelOpen = signal(false);
export const soundSettingsOpen = signal(false);

// ---- the bar --------------------------------------------------------------

export function SystemBar() {
  return (
    <View
      testID="keel-system-bar"
      accessibilityLabel="系统栏"
      style={{
        height: SYSTEM_BAR_HEIGHT_PX,
        flexShrink: 0,
        flexDirection: "row",
        alignItems: "center",
        gap: isMobile() ? space.sm : space.md,
        paddingHorizontal: isMobile() ? space.sm : space.lg,
        backgroundColor: color.panel,
        backdropFilter: GLASS_BLUR,
        borderBottomWidth: 1,
        borderBottomColor: color.border,
        boxShadow: `inset 0 1px 0 ${color.glassInner}`,
      }}
    >
      {/* The brand, the plugin slot and the window's drag region are one
          left-hand area; the slot renders nothing when no plugin contributes,
          and the spacer is then the whole drag area. */}
      <View
        testID="books-top"
        accessibilityLabel="品牌、插件面板与窗口拖动区"
        style={{
          flex: 1,
          // A terminal lays a view with no text out at no width; one point of
          // floor keeps the slot an addressable (and plugin-renderable) region.
          minWidth: platform.idiom === "terminal" ? 1 : 0,
          flexDirection: "row",
          alignItems: "center",
          gap: space.sm,
        }}
        desktop={{ windowDrag: true }}
      >
        <View testID="system-bar-brand" style={{ flexShrink: 0 }}>
          <BrandMark />
        </View>
        <PluginTopPanels />
        <If when={!topBandHasContent()}>
          <View style={{ flex: 1, minWidth: 0 }} />
        </If>
      </View>
      <SystemBarActions />
    </View>
  );
}

/** The right end: the five system controls, then the window buttons only when
 * the host gives the app their area. */
function SystemBarActions() {
  const soundOn = () => soundPreferences().enabled;
  const dark = () => colorScheme() === "dark";
  const toggleTheme = () => applyAppearance(dark() ? "light" : "dark");
  const pending = () => store.sessions().filter((row) => row.pendingDecision === true).length;
  return (
    <View
      testID="system-bar-actions"
      style={{ flexShrink: 0, flexDirection: "row", alignItems: "center", gap: isMobile() ? space.xxs : space.xs }}
    >
      <BarAction
        testID="system-bar-sound"
        icon={soundOn() ? "volume" : "volume-off"}
        label={soundOn() ? "关闭提示音" : "打开提示音"}
        terminalLabel={soundOn() ? "声" : "静"}
        selected={soundOn()}
        onPress={() => setSoundEnabled(!soundOn())}
      />
      <BarAction
        testID="system-bar-sound-settings"
        icon="settings"
        label="声音设置"
        terminalLabel="音设"
        onPress={() => soundSettingsOpen.set(true)}
      />
      <BarAction
        testID="system-bar-notifications"
        icon="bell"
        label={pending() > 0 ? `通知，${pending()} 个待审批` : "通知"}
        terminalLabel={pending() > 0 ? `通知${pending()}` : "通知"}
        count={pending()}
        onPress={() => notificationPanelOpen.set(true)}
      />
      <BarAction
        testID="system-bar-theme"
        icon={dark() ? "sun" : "moon"}
        label={dark() ? "切换到浅色主题" : "切换到深色主题"}
        terminalLabel={dark() ? "浅色" : "深色"}
        onPress={toggleTheme}
      />
      <BarAction
        testID="system-bar-command"
        icon="command"
        label="打开命令面板"
        terminalLabel="命令"
        onPress={openPalette}
      />
      <WindowControls />
    </View>
  );
}

/** One system action: an icon button on a drawn host, the same action as a
 * short text button in a terminal (an icon-only view has no characters
 * there). `count` draws the notification badge on the drawn form; the terminal
 * label already carries it. */
function BarAction(props: {
  testID: string;
  icon: IconName;
  label: string;
  terminalLabel: string;
  onPress: () => void;
  selected?: boolean;
  count?: number;
}) {
  const control = () => (isMobile() ? { width: BOOK_TOUCH_PX, height: BOOK_TOUCH_PX } : null);
  return (
    <If
      when={platform.idiom === "terminal"}
      else={
        <View style={{ position: "relative", alignItems: "center", justifyContent: "center" }}>
          <IconButton
            testID={props.testID}
            icon={props.icon}
            label={props.label}
            selected={props.selected}
            onPress={props.onPress}
            style={control()}
          />
          <If when={(props.count ?? 0) > 0}>
            <View
              style={{
                position: "absolute",
                top: 3,
                right: 3,
                minWidth: 14,
                height: 14,
                paddingHorizontal: 3,
                borderRadius: radius.pill,
                backgroundColor: color.danger,
                alignItems: "center",
                justifyContent: "center",
                pointerEvents: "none",
              }}
            >
              <Text style={{ fontSize: typeScale["2xs"], fontWeight: weight.semibold, color: color.destructiveForeground }}>
                {(props.count ?? 0) > 9 ? "9+" : String(props.count ?? 0)}
              </Text>
            </View>
          </If>
        </View>
      }
    >
      <Button
        testID={props.testID}
        variant="ghost"
        size="sm"
        label={props.terminalLabel}
        accessibilityLabel={props.label}
        selected={props.selected}
        onPress={props.onPress}
      />
    </If>
  );
}

// ---- window controls ------------------------------------------------------

/** Whether this host lets the app draw the window buttons: `nsh/desktop` is
 * available AND the hidden title bar reserves no native control width (macOS
 * traffic lights, Windows/Linux caption buttons). A browser and a terminal
 * answer false, so the bar never grows fake buttons. */
export function windowControlsAvailable(): boolean {
  if (!desktopIsAvailable) return false;
  try {
    const area = titleBar();
    return area.height > 0 && area.left === 0 && area.right === 0;
  } catch {
    return false;
  }
}

function currentWindowRef(): WindowRef | null {
  if (!desktopIsAvailable) return null;
  try {
    return currentWindow();
  } catch {
    return null;
  }
}

function WindowControls() {
  // Captured while rendering: `currentWindow()` throws outside a component.
  // The platform carries out each press (`windowControl`); the ref only feeds
  // the maximize/restore label and icon.
  const win = windowControlsAvailable() ? currentWindowRef() : null;
  const maximized = () => win?.state()?.mode === "maximized";
  return (
    <If when={win !== null}>
      <View
        testID="system-bar-window-controls"
        style={{ flexDirection: "row", alignItems: "center", marginLeft: space.xxs }}
      >
        <WindowButton testID="system-bar-window-minimize" label="最小化窗口" control="minimize">
          <Icon name="minus" size={14} tint={color.mutedForeground} />
        </WindowButton>
        <WindowButton
          testID="system-bar-window-maximize"
          label={maximized() ? "还原窗口" : "最大化窗口"}
          control="maximize"
        >
          <Icon name={maximized() ? "copy" : "square"} size={12} tint={color.mutedForeground} />
        </WindowButton>
        <WindowButton testID="system-bar-window-close" label="关闭窗口" control="close" danger>
          <Icon name="x" size={14} tint={color.mutedForeground} />
        </WindowButton>
      </View>
    </If>
  );
}

function WindowButton(props: {
  testID: string;
  label: string;
  control: "minimize" | "maximize" | "close";
  danger?: boolean;
  children?: JSX.Element;
}) {
  return (
    <View
      testID={props.testID}
      role="button"
      focusable
      accessibilityLabel={props.label}
      desktop={{ windowControl: props.control }}
      hoverStyle={{ backgroundColor: props.danger ? color.dangerTint : color.hover }}
      style={{ width: 40, height: 28, alignItems: "center", justifyContent: "center", borderRadius: radius.sm }}
    >
      {props.children}
    </View>
  );
}

// ---- the notification panel ----------------------------------------------

interface RecentNotice {
  id: string;
  tone: Notice["tone"];
  text: string;
  at: number;
}

const recentNotices = signal<RecentNotice[]>([]);
let lastNotice: Notice | null = null;
let noticeSeq = 0;

/** Keep the last few in-app notices for the panel. Mounted once by
 * `SystemBarOverlays`, so a toast's six-second lifetime does not erase the
 * fact from the panel. */
function recordNoticeHistory() {
  effect(() => {
    const notice = store.notice();
    if (notice === lastNotice) return;
    lastNotice = notice;
    if (!notice) return;
    noticeSeq += 1;
    recentNotices.update((list) => [{ id: `notice-${noticeSeq}`, tone: notice.tone, text: notice.text, at: Date.now() }, ...list].slice(0, 8));
  });
}

/** The mounted overlays: the notification panel and the sound dialog. They
 * live at the app root (as siblings of the routed screen), never inside the
 * bar's fixed-height row. */
export function SystemBarOverlays(props: { onOpenSession?: (sessionId: string) => void }) {
  recordNoticeHistory();
  return (
    <View testID="system-bar-overlays" style={{ flex: 0 }}>
      <NotificationPanel onOpenSession={props.onOpenSession} />
      <SoundSettingsDialog />
    </View>
  );
}

function pendingApprovals() {
  return store.sessions().filter((row) => row.pendingDecision === true);
}

function NotificationPanel(props: { onOpenSession?: (sessionId: string) => void }) {
  const openSession = (sessionId: string) => {
    notificationPanelOpen.set(false);
    props.onOpenSession?.(sessionId);
  };
  return (
    <Dialog
      testID="notifications-panel"
      open={notificationPanelOpen()}
      onOpenChange={(event) => notificationPanelOpen.set(event.open)}
      title="通知"
      description="需要你决定的审批，和这台客户端最近的提示。"
      size="default"
      maxHeight={520}
    >
      <View style={{ flexDirection: "column", gap: space.md }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Text style={{ flex: 1, fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>
            待审批 · {pendingApprovals().length}
          </Text>
          <LiveBadge testID="notifications-live" />
        </View>
        <If
          when={pendingApprovals().length > 0}
          else={
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              没有等待你的审批。运行中的会话会在问题到达时出现在这里，并响一声提示音（可在声音设置里关闭）。
            </Text>
          }
        >
          <View testID="notifications-approvals" style={{ flexDirection: "column" }}>
            <Each of={pendingApprovals()} key={(row) => row.sessionId}>
              {(row) => (
                <PressRow
                  testID={`notifications-approval-${row().sessionId}`}
                  accessibilityLabel={`打开待审批会话 ${sessionTitle(row())}`}
                  onPress={() => openSession(row().sessionId)}
                >
                  <Icon name="bell" size={14} tint={color.warning} />
                  <View style={{ flex: 1, minWidth: 0 }}>
                    <Text style={{ fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                      {sessionTitle(row())}
                    </Text>
                    <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                      {row().lastMessage ?? "等待你的确认"}
                    </Text>
                  </View>
                  <Icon name="chevron-right" size={12} tint={color.mutedForeground} />
                </PressRow>
              )}
            </Each>
          </View>
        </If>
        <View style={{ height: 1, backgroundColor: color.border }} />
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>最近提示</Text>
        <If
          when={recentNotices().length > 0}
          else={
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              还没有新的提示。保存失败、连接状态和审批到达都会留在这里。
            </Text>
          }
        >
          <View testID="notifications-recent" style={{ flexDirection: "column", gap: space.sm }}>
            <Each of={recentNotices()} key={(item) => item.id}>
              {(item) => (
                <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm }}>
                  <Icon name={item().tone === "danger" ? "alert" : item().tone === "warn" ? "alert" : "info"} size={14} tint={item().tone === "danger" ? color.danger : item().tone === "warn" ? color.warning : color.mutedForeground} />
                  <View style={{ flex: 1, minWidth: 0 }}>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>{item().text}</Text>
                    <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{formatWhen(new Date(item().at).toISOString())}</Text>
                  </View>
                </View>
              )}
            </Each>
          </View>
        </If>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Icon name="wifi" size={14} tint={color.mutedForeground} />
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            {store.mode() === "offline" ? "离线：恢复连接后提示会继续到达。" : "实时通道与审批通知已打开。"}
          </Text>
        </View>
      </View>
    </Dialog>
  );
}

// ---- the sound dialog -----------------------------------------------------

const VOLUMES: Array<{ id: SoundVolume; label: string }> = [
  { id: "low", label: "低" },
  { id: "medium", label: "中" },
  { id: "high", label: "高" },
];

function SoundSettingsDialog() {
  return (
    <Dialog
      testID="sound-settings"
      open={soundSettingsOpen()}
      onOpenChange={(event) => soundSettingsOpen.set(event.open)}
      title="声音"
      description="审批到达时是否响提示音；普通运行输出不会响。"
      size="sm"
    >
      <View style={{ flexDirection: "column", gap: space.lg }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
          <Switch
            testID="sound-settings-enabled"
            checked={soundPreferences().enabled}
            onChange={(checked) => setSoundEnabled(checked)}
            label={`提示音 ${soundPreferences().enabled ? "开" : "关"}`}
            accessibilityLabel="切换审批提示音"
          />
        </View>
        <View style={{ flexDirection: "column", gap: space.sm }}>
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>音量</Text>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
            <Each of={VOLUMES} key={(entry) => entry.id}>
              {(entry) => (
                <Button
                  testID={`sound-settings-volume-${entry().id}`}
                  variant={soundPreferences().volume === entry().id ? "secondary" : "ghost"}
                  size="sm"
                  label={entry().label}
                  accessibilityLabel={`提示音音量 ${entry().label}`}
                  selected={soundPreferences().volume === entry().id}
                  onPress={() => setSoundVolume(entry().id)}
                />
              )}
            </Each>
            <Button
              testID="sound-settings-preview"
              variant="outline"
              size="sm"
              icon="volume"
              label="试听"
              accessibilityLabel="试听提示音"
              onPress={() => void previewSoundCue()}
            />
          </View>
        </View>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {soundAvailable() === false
            ? "这台宿主不播放应用内声音；系统通知仍由浏览器或操作系统决定。"
            : "提示音只响一次，音量为本机偏好；系统通知仍由浏览器或操作系统决定。"}
        </Text>
      </View>
    </Dialog>
  );
}
