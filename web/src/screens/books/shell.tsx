// The 账簿 shell — change keel-bot-identity-access, A4.4a; top refactor A4.4f;
// shell IA change: four daily chapters (工作区 / 任务 / 员工 / 团队).
//
// The global system bar (App.tsx's `SystemBar`) is the app's top-level chrome:
// brand, plugin top-band slot, sound, notifications, theme, command palette and
// host-gated window controls. This shell owns only what is below it: the left
// rail on desktop (labels from 1280px, an icon rail from 768–1279) carrying the
// four daily chapters — 工作区 / 任务 / 员工 / 团队 — and the four bottom tabs on
// a phone plus the settings entry.
//
// 身份 and 记录 are management faces, not daily destinations: the settings
// page's 通用 section carries their entries (with the old `books-chapter-*`
// test ids and the same permission gates), so the rail is not a dumping
// ground. Account, sign-out, todo and the live-channel state stay on the
// settings page's General cards; search lives in each list that has something
// to filter.
//
// A chapter the reader may not read is not rendered at all (design §6.1: hide,
// never grey a destination without a path). The rail keeps the daily
// destinations' test ids (`books-chapter-*`); the phone's bottom tabs keep
// theirs (`books-tab-*`).
import type { JSX } from "nsh";
import { effect, safeAreaInsets, Text, View, windowSize } from "nsh";
import {
  Button,
  GLASS_BLUR,
  IconButton,
  color,
  font,
  glassState,
  radius,
  space,
  typeScale,
  weight,
} from "../../ui/kit";
import { Icon, type IconName } from "../../ui/icons";
import { store } from "../../data/store";
import { botAccess } from "../../data/bots";
import { identityStream } from "../../data/identity-stream";
import { canAny } from "../../data/permissions";
import {
  BOOK_TOUCH_PX,
  Hairline,
  Kicker,
  booksFocusStyle,
  isMobile,
  touchStyle,
} from "./common";
import { MANAGEMENT_KEYS, TASK_KEYS } from "./management";

export type ChapterId = "workspace" | "board" | "bots" | "teams" | "identity" | "records";

interface ChapterEntry {
  id: ChapterId;
  num: string;
  label: string;
  icon: IconName;
  /** The keys that make the chapter visible (workspace always is). */
  keys?: string[];
}

const CHAPTERS: ChapterEntry[] = [
  { id: "workspace", num: "01", label: "工作区", icon: "message" },
  { id: "board", num: "02", label: "任务", icon: "inbox", keys: TASK_KEYS },
  { id: "bots", num: "03", label: "员工", icon: "bot", keys: MANAGEMENT_KEYS.bots },
  { id: "teams", num: "04", label: "团队", icon: "users", keys: MANAGEMENT_KEYS.teams },
];

const chapterVisible = (chapter: ChapterEntry): boolean => !chapter.keys || canAny(chapter.keys);

/** The rail's two widths: labelled from 1280, icon-only on the tablet band.
 * The compact rail keeps 768 and 1024 from losing a chapter's width to labels
 * the bottom bar already taught. */
export const BOOK_RAIL_WIDTH_PX = 208;
export const BOOK_RAIL_COMPACT_WIDTH_PX = 60;
export const BOOK_RAIL_WIDE_MIN_PX = 1280;

const railWide = (): boolean => windowSize().width >= BOOK_RAIL_WIDE_MIN_PX;

// ---- the left rail ---------------------------------------------------------

function RailEntry(props: { chapter: ChapterEntry }) {
  const active = () => store.route() === props.chapter.id;
  const compact = () => !railWide();
  return (
    <View
      testID={`books-chapter-${props.chapter.id}`}
      role="button"
      focusable
      onPress={() => store.openChapter(props.chapter.id)}
      accessibilityLabel={`打开${props.chapter.label}`}
      accessibilityState={{ selected: active() }}
      hoverStyle={active() ? undefined : { backgroundColor: color.hover }}
      focusStyle={booksFocusStyle()}
      style={[
        {
          flexDirection: "row",
          alignItems: "center",
          justifyContent: compact() ? "center" : "flex-start",
          gap: space.sm,
          height: compact() ? BOOK_TOUCH_PX : 40,
          width: compact() ? BOOK_TOUCH_PX : undefined,
          alignSelf: compact() ? "center" : undefined,
          paddingHorizontal: compact() ? 0 : space.md,
          borderRadius: radius.md,
        },
        active() ? glassState() : {},
      ]}
    >
      <If
        when={compact()}
        else={
          <Text
            style={{
              width: 18,
              fontFamily: font.mono,
              fontSize: typeScale.key,
              letterSpacing: 0.4,
              color: active() ? color.foreground : color.mutedForeground,
            }}
          >
            {props.chapter.num}
          </Text>
        }
      >
        <Icon
          name={props.chapter.icon}
          size={18}
          tint={active() ? color.foreground : color.mutedForeground}
        />
      </If>
      <If when={!compact()}>
        <Text
          style={{
            flex: 1,
            fontSize: typeScale.base,
            fontWeight: active() ? weight.medium : weight.regular,
            color: active() ? color.foreground : color.mutedForeground,
            numberOfLines: 1,
          }}
        >
          {props.chapter.label}
        </Text>
      </If>
    </View>
  );
}

/** The desktop chapter rail: navigation, and the one settings entry. It is a
 * pane standing at the page's left edge, with a hairline against the paper. */
function ChapterRail() {
  const compact = () => !railWide();
  return (
    <View
      testID="books-rail"
      accessibilityLabel="章节导航"
      style={{
        width: compact() ? BOOK_RAIL_COMPACT_WIDTH_PX : BOOK_RAIL_WIDTH_PX,
        flexShrink: 0,
        minHeight: 0,
        flexDirection: "column",
        backgroundColor: color.sidebar,
        backdropFilter: GLASS_BLUR,
        borderRightWidth: 1,
        borderColor: color.sidebarBorder,
        boxShadow: `inset -1px 0 0 ${color.glassEdge}, inset 0 1px 0 ${color.glassInner}`,
      }}
    >
      <View style={{ flex: 1, minHeight: 0, padding: compact() ? space.sm : space.md, gap: space.xxs }}>
        <If when={!compact()}>
          <Text
            style={{
              paddingHorizontal: space.md,
              paddingBottom: space.sm,
              fontSize: typeScale.sm,
              fontWeight: weight.medium,
              letterSpacing: 0.5,
              color: color.mutedForeground,
              textTransform: "uppercase",
            }}
          >
            章节
          </Text>
        </If>
        <Each of={CHAPTERS.filter((chapter) => chapterVisible(chapter))} key={(chapter) => chapter.id}>
          {(chapter) => <RailEntry chapter={chapter()} />}
        </Each>
      </View>
      <Hairline />
      <View style={{ padding: compact() ? space.sm : space.md }}>
        <View
          testID="books-settings"
          role="button"
          focusable
          onPress={() => store.openSettings()}
          accessibilityLabel="打开设置"
          hoverStyle={{ backgroundColor: color.hover }}
          focusStyle={booksFocusStyle()}
          style={{
            flexDirection: "row",
            alignItems: "center",
            justifyContent: compact() ? "center" : "flex-start",
            gap: space.sm,
            height: compact() ? BOOK_TOUCH_PX : 40,
            width: compact() ? BOOK_TOUCH_PX : undefined,
            alignSelf: compact() ? "center" : undefined,
            paddingHorizontal: compact() ? 0 : space.md,
            borderRadius: radius.md,
          }}
        >
          <Icon name="settings" size={16} tint={color.mutedForeground} />
          <If when={!compact()}>
            <Text style={{ fontSize: typeScale.base, color: color.mutedForeground }}>设置</Text>
          </If>
        </View>
      </View>
    </View>
  );
}

// ---- the phone's bottom bar ------------------------------------------------

function BottomBar() {
  const insets = safeAreaInsets();
  return (
    <View
      testID="books-tabbar"
      accessibilityLabel="章节导航"
      style={{
        flexDirection: "row",
        alignItems: "stretch",
        backgroundColor: color.background,
        borderTopWidth: 1,
        borderTopColor: color.border,
        minHeight: BOOK_TOUCH_PX + 12,
        paddingTop: space.xxs,
        paddingBottom: space.sm + insets().bottom,
      }}
    >
      <Each of={CHAPTERS.filter((chapter) => chapterVisible(chapter))} key={(chapter) => chapter.id}>
        {(chapter) => {
          const active = () => store.route() === chapter().id;
          return (
            <View
              testID={`books-tab-${chapter().id}`}
              role="button"
              focusable
              onPress={() => store.openChapter(chapter().id)}
              accessibilityLabel={`打开${chapter().label}`}
              accessibilityState={{ selected: active() }}
              style={{
                flex: 1,
                minWidth: 0,
                minHeight: BOOK_TOUCH_PX,
                alignItems: "center",
                justifyContent: "center",
                gap: 3,
                borderTopWidth: 2,
                borderTopColor: active() ? color.foreground : "transparent",
              }}
            >
              <Icon name={chapter().icon} size={18} tint={active() ? color.foreground : color.mutedForeground} />
              <Text
                style={{
                  fontSize: typeScale.sm,
                  color: active() ? color.foreground : color.mutedForeground,
                  numberOfLines: 1,
                }}
              >
                {chapter().label}
              </Text>
            </View>
          );
        }}
      </Each>
      {/* Settings is not a chapter tab: it keeps its own 44px target after the
          three daily destinations, so the bottom bar stays the daily-chapter
          navigation. 身份 and 记录 are entered from settings. */}
      <View
        style={{
          width: BOOK_TOUCH_PX + 4,
          borderLeftWidth: 1,
          borderLeftColor: color.border,
          alignItems: "center",
          justifyContent: "center",
        }}
      >
        <IconButton
          testID="books-mobile-settings"
          icon="settings"
          label="打开设置"
          onPress={() => store.openSettings()}
          style={{ width: BOOK_TOUCH_PX, height: BOOK_TOUCH_PX }}
        />
      </View>
    </View>
  );
}

/** The shell below the global system bar: the desktop rail or the phone's
 * bottom tabs, and the chapter's paper. */
export function BooksChrome(props: { children?: JSX.Element }) {
  // The team/bot chapters read one shared store; the shell warms it once (the
  // top band no longer owns a todo count, but the lists still need the data).
  effect(() => {
    // The identity invalidation tail is keyed to the auth stage and to
    // app.read. `sync` starts it once after login (whoami lands a moment
    // later) and closes it on logout/401/account switch; the store's own
    // session teardown also calls the closer, so an unmounted shell cannot
    // leave the old cookie's stream running.
    identityStream.sync(store.auth());
    if (canAny(["bot.read", "bot.manage"])) botAccess.ensureBots();
    if (canAny(["team_bot.read", "team_bot.manage"])) botAccess.ensureTeams();
  });
  return (
    <View testID="books-shell" style={{ flex: 1, minHeight: 0, backgroundColor: color.background }}>
      <View style={{ flex: 1, minHeight: 0, flexDirection: "row" }}>
        <If when={!isMobile()}>
          <ChapterRail />
        </If>
        <View style={{ flex: 1, minWidth: 0, minHeight: 0 }}>{props.children}</View>
      </View>
      <If when={isMobile()}>
        <BottomBar />
      </If>
    </View>
  );
}

// ---- the paper -------------------------------------------------------------

/** The identity chapter's 280px index column (≥1440 only). It sits inside the
 * paper, to the right of the chapter rail. */
export function IndexColumn(props: { children?: JSX.Element; testID?: string }) {
  return (
    <View
      testID={props.testID ?? "identity-index"}
      style={{
        width: 280,
        flexShrink: 0,
        minHeight: 0,
        flexDirection: "column",
        borderRightWidth: 1,
        borderRightColor: color.border,
        backgroundColor: color.hoverFaint,
      }}
    >
      {props.children}
    </View>
  );
}

/** The paper: at most 1040 wide, 32px gutters, no card shell (bands + hairlines
 * carry the hierarchy). It scrolls as one column. */
export function Paper(props: { children?: JSX.Element; testID?: string; maxWidth?: number }) {
  return (
    <View testID={props.testID ?? "books-paper"} style={{ flex: 1, minWidth: 0, minHeight: 0, flexDirection: "column" }}>
      <View
        style={{
          flex: 1,
          minHeight: 0,
          width: "100%",
          maxWidth: props.maxWidth ?? 1040,
          alignSelf: "center",
          paddingHorizontal: isMobile() ? space.lg : space["2xl"],
          paddingTop: space.lg,
          paddingBottom: isMobile() ? space.lg : space["2xl"],
          flexDirection: "column",
        }}
      >
        {props.children}
      </View>
    </View>
  );
}

/** A back row for an object page below the two-column breakpoint. */
export function BackToIndex(props: { label: string; onPress: () => void }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", paddingBottom: space.sm }}>
      <Button
        testID="books-back"
        variant="ghost"
        size="sm"
        icon="chevron-left"
        label={props.label}
        accessibilityLabel={`返回${props.label}`}
        onPress={props.onPress}
        style={touchStyle()}
      />
    </View>
  );
}

/** The chapter's summary band on a tablet/phone: the object's name, three facts
 * and the primary action, above the paper. */
export function SummaryBand(props: { kicker: string; title: string; facts?: string[]; actions?: JSX.Element; testID?: string }) {
  return (
    <View
      testID={props.testID}
      style={{
        flexDirection: "row",
        alignItems: "flex-start",
        gap: space.md,
        paddingBottom: space.md,
        borderBottomWidth: 1,
        borderBottomColor: color.border,
      }}
    >
      <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: space.xxs }}>
        <Kicker text={props.kicker} />
        <Text
          style={{
            fontSize: isMobile() ? typeScale.section : typeScale["2xl"],
            fontWeight: weight.semibold,
            color: color.foreground,
            numberOfLines: 1,
            textOverflow: "ellipsis",
          }}
        >
          {props.title}
        </Text>
        {props.facts && props.facts.length ? (
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{props.facts.join(" · ")}</Text>
        ) : null}
      </View>
      {props.actions ? <View style={{ flexDirection: "row", gap: space.sm, flexShrink: 0 }}>{props.actions}</View> : null}
    </View>
  );
}
