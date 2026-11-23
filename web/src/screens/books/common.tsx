// Shared pieces of the 账簿 (books) shell — change keel-bot-identity-access,
// A4.4a.
//
// The shell's grammar: a 56px chapter band, a 280 identity index column (or the
// paper alone), a paper that is bands of rows separated by hairlines, and a
// sign-off bar for high-consequence saves. Everything here is composed from the
// kit's tokens and components; no element invents a colour, a radius or a
// control.
//
// Responsive contract (design §8): ≥1440 keeps the identity index column; 1024–
// 1439 is a full-width paper; 768–1023 folds the object summary into the paper;
// ≤767 is a single column with a 3-tab daily bottom bar (身份 / 记录 enter from
// the settings page) and 44px controls.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Clipboard, Text, View, windowSize } from "nsh";
import {
  Avatar,
  Badge,
  Button,
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  Skeleton,
  ScrollArea,
  alpha,
  color,
  font,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../../ui/kit";
import { Icon, type IconName } from "../../ui/icons";
import { store } from "../../data/store";
import { RISK_LABEL, capabilityOf, type CapabilityRisk } from "../../data/permission-catalog";

// ---- measurements the shell owns ------------------------------------------

export const BOOK_BAND_HEIGHT_PX = 56;
export const BOOK_INDEX_WIDTH_PX = 280;
export const BOOK_SPINE_WIDTH_PX = 226;
export const BOOK_PAPER_MAX_WIDTH_PX = 1040;
export const BOOK_MATRIX_MAX_WIDTH_PX = 820;
export const BOOK_MOBILE_MAX_PX = 767;
export const BOOK_WIDE_MIN_PX = 1440;
export const BOOK_TOUCH_PX = 44;
/** ≥1280: the records table keeps its 330px detail column; below it the same
 * detail is a right drawer so the table keeps its columns at 1024. */
export const BOOK_RECORDS_INLINE_MIN_PX = 1280;

/** ≤767: the single column with the bottom tab bar. */
export const isMobile = (): boolean => windowSize().width <= BOOK_MOBILE_MAX_PX;
/** ≥1440: the identity chapter keeps its 280px index column. */
export const isWide = (): boolean => windowSize().width >= BOOK_WIDE_MIN_PX;
/** <1024: the compact layouts (cards, a detail sheet) instead of wide tables. */
export const isNarrow = (): boolean => windowSize().width < 1024;
/** The records detail is inline (a third column) only on a wide desktop. */
export const recordsInline = (): boolean => windowSize().width >= BOOK_RECORDS_INLINE_MIN_PX;

/** A control that meets the 44px touch floor on a phone and keeps the kit's
 * own height elsewhere. */
export const touchStyle = (): StyleProp<ViewStyle> =>
  isMobile() ? { height: BOOK_TOUCH_PX, minHeight: BOOK_TOUCH_PX } : null;

/** The focus ring at the kit's measured contrast (the kit's own helper is not
 * part of its public surface, so the same two values are repeated here: an
 * outline, not a shadow, and the foreground at 0.65). */
export const booksFocusStyle = (): StyleProp<ViewStyle> => ({
  outlineStyle: "solid",
  outlineWidth: 2,
  outlineColor: alpha(color.foreground, 0.65),
  outlineOffset: 0,
});

/** The live channel's own word. The books pages are static projections, so a
 * channel that was never opened is not a failure: it stays green 实时. A
 * channel that is connecting/reconnecting degrades to 重连中, and a deployment
 * whose transport is really offline says so. There is deliberately no gray
 * 「未连接」 badge on a page that never promised a stream. */
export function LiveBadge(props: { testID?: string }) {
  const offline = () => store.mode() === "offline";
  const status = () => store.connection().status;
  const degraded = () => !offline() && (status() === "connecting" || status() === "reconnecting");
  const label = () => (offline() ? "离线" : degraded() ? "重连中" : "实时");
  const variant = () => (offline() ? "destructive" : degraded() ? "warning" : "success");
  const icon = () => (offline() ? "wifi-off" : degraded() ? "refresh" : "wifi");
  return <Badge testID={props.testID ?? "books-live"} variant={variant()} label={label()} icon={icon()} />;
}

// ---- time / labels --------------------------------------------------------

const two = (value: number): string => (value < 10 ? `0${value}` : String(value));

/** 今天 14:12 / 昨天 18:04 / 11 月 18 日 / 2026 年 1 月 4 日. Built from local
 * getters (not a locale call) so every host prints the same words. */
export const formatWhen = (iso: string | undefined): string => {
  if (!iso) return "—";
  const at = new Date(iso);
  if (!Number.isFinite(at.getTime())) return "—";
  const now = new Date();
  const clock = `${two(at.getHours())}:${two(at.getMinutes())}`;
  const start = (date: Date) => new Date(date.getFullYear(), date.getMonth(), date.getDate()).getTime();
  const day = start(at);
  const today = start(now);
  const yesterday = today - 86_400_000;
  if (day === today) return `今天 ${clock}`;
  if (day === yesterday) return `昨天 ${clock}`;
  if (at.getFullYear() === now.getFullYear()) return `${at.getMonth() + 1} 月 ${at.getDate()} 日`;
  return `${at.getFullYear()} 年 ${at.getMonth() + 1} 月 ${at.getDate()} 日`;
};

/** 2026-11-02 — the one absolute date the facts use. */
export const formatDay = (iso: string | undefined): string => {
  if (!iso) return "—";
  const at = new Date(iso);
  if (!Number.isFinite(at.getTime())) return "—";
  return `${at.getFullYear()}-${two(at.getMonth() + 1)}-${two(at.getDate())}`;
};

/** 人 / Bot / 服务 — the label only; `can()` decides what may be done. */
export const accountKindLabel = (kind: string | undefined): string =>
  kind === "bot" ? "Bot" : kind === "service" ? "服务" : kind === "human" ? "人" : kind || "未知";

/** The avatar's letter: a Chinese name's last character reads better as a
 * mark than its pinyin initials. */
export const avatarLetter = (name: string | undefined): string => {
  const trimmed = (name ?? "").trim();
  if (!trimmed) return "?";
  const characters = [...trimmed];
  return characters[characters.length - 1] ?? "?";
};

/** A Bot/team name's first character: the mark a Bot avatar carries (客服一号 →
 * 客), not a two-character initials block that a 24px circle cannot hold. */
export const firstLetter = (name: string | undefined): string => {
  const trimmed = (name ?? "").trim();
  if (!trimmed) return "?";
  return [...trimmed][0] ?? "?";
};

/** The 账簿 avatar: one character, the same size ladder as the kit's. */
export function BookAvatar(props: { name: string; size?: "sm" | "default" | "lg" }) {
  const size = () => props.size ?? "sm";
  return (
    <Avatar
      name={props.name}
      size={size()}
      fallback={
        <Text
          style={{
            fontSize: size() === "lg" ? typeScale.base : size() === "default" ? typeScale.sm : typeScale["2xs"],
            fontWeight: weight.medium,
            color: color.mutedForeground,
          }}
        >
          {firstLetter(props.name)}
        </Text>
      }
    />
  );
}

export const copyText = async (text: string): Promise<boolean> => {
  try {
    await Clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
};

// ---- layout atoms ---------------------------------------------------------

export function Hairline(props: { style?: StyleProp<ViewStyle> }) {
  return <View style={[{ height: 1, alignSelf: "stretch", backgroundColor: color.border }, props.style ?? null]} />;
}

/** A quiet mono kicker: `04 / 身份 · 账号`. */
export function Kicker(props: { icon?: IconName; text: string; testID?: string }) {
  return (
    <View testID={props.testID} style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
      {props.icon ? <Icon name={props.icon} size={14} tint={color.mutedForeground} /> : null}
      <Text
        style={{
          fontFamily: font.mono,
          fontSize: typeScale.sm,
          letterSpacing: 0.5,
          color: color.mutedForeground,
        }}
      >
        {props.text}
      </Text>
    </View>
  );
}

/** The paper's head: kicker, the page's one H1, a purpose sentence, facts and
 * the primary actions. On a phone the H1 drops to 20 (the design's one
 * responsive type exception). */
export function SheetHead(props: {
  kicker?: { icon?: IconName; text: string };
  title: string;
  /** A leading mark (an object's avatar) beside the title. */
  leading?: JSX.Element;
  badges?: JSX.Element;
  description?: string;
  facts?: Array<{ label: string; value: string }>;
  actions?: JSX.Element;
  children?: JSX.Element;
  testID?: string;
}) {
  return (
    <View testID={props.testID} style={{ flexDirection: "column", gap: space.sm, paddingBottom: space.md }}>
      {props.kicker ? <Kicker icon={props.kicker.icon} text={props.kicker.text} /> : null}
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap", minWidth: 0 }}>
        {props.leading}
        <Text
          testID={props.testID ? `${props.testID}-title` : undefined}
          style={{
            fontSize: isMobile() ? typeScale.section : typeScale["3xl"],
            lineHeight: Math.round((isMobile() ? typeScale.section : typeScale["3xl"]) * lineHeight.tight),
            fontWeight: weight.semibold,
            color: color.foreground,
            letterSpacing: -0.4,
          }}
        >
          {props.title}
        </Text>
        {props.badges}
        {props.actions ? (
          <View style={{ marginLeft: "auto", flexDirection: "row", alignItems: "center", gap: space.sm }}>
            {props.actions}
          </View>
        ) : null}
      </View>
      {props.description !== undefined && props.description !== "" ? (
        <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
          {props.description}
        </Text>
      ) : null}
      {props.facts && props.facts.length ? (
        <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.md }}>
          <Each of={props.facts} key={(fact) => fact.label}>
            {(fact) => (
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                {fact().label} <Text style={{ color: color.foreground, fontWeight: weight.medium }}>{fact().value}</Text>
              </Text>
            )}
          </Each>
        </View>
      ) : null}
      {props.children}
    </View>
  );
}

/** A band's head: number + 20px title + a right-hand note/search. */
export function BandHead(props: { num: string; title: string; right?: JSX.Element; testID?: string }) {
  return (
    <View
      testID={props.testID}
      style={{
        flexDirection: "row",
        alignItems: "flex-end",
        gap: space.md,
        paddingTop: space.xs,
        paddingBottom: space.sm,
        borderBottomWidth: 1,
        borderBottomColor: color.border,
      }}
    >
      <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, letterSpacing: 1, color: color.mutedForeground, paddingBottom: 2 }}>
        {props.num}
      </Text>
      <Text style={{ fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground, letterSpacing: -0.2 }}>
        {props.title}
      </Text>
      {props.right ? (
        <View style={{ marginLeft: "auto", flexDirection: "row", alignItems: "center", gap: space.md, flexShrink: 0 }}>
          {props.right}
        </View>
      ) : null}
    </View>
  );
}

/** A label with a hairline: `权限组 ───────── 改权限请去「身份 → 权限组」`.
 * When `onNotePress` is set the note is a real link (not a tooltip), so the
 * page keeps the object's one source of truth one tap away. */
export function SectionLabel(props: { text: string; note?: string; onNotePress?: () => void; testID?: string }) {
  return (
    <View testID={props.testID} style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
      <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>{props.text}</Text>
      <Hairline style={{ flex: 1 }} />
      {props.note ? (
        props.onNotePress ? (
          <Button
            variant="link"
            size="sm"
            label={props.note}
            accessibilityLabel={props.note}
            onPress={props.onNotePress}
            style={isMobile() ? { minHeight: BOOK_TOUCH_PX } : undefined}
          />
        ) : (
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{props.note}</Text>
        )
      ) : null}
    </View>
  );
}

/** 账号 / 权限组 — the identity chapter's one segmented control (the index
 * column on a wide screen, the page's top row below it). */
export function IdentitySegmented(props: { section: "accounts" | "groups"; groupCount?: number }) {
  const items: Array<{ id: "accounts" | "groups"; label: string }> = [
    { id: "accounts", label: "账号" },
    { id: "groups", label: "权限组" },
  ];
  return (
    <View
      testID="identity-segment"
      accessibilityLabel="身份子页"
      style={{
        flexDirection: "row",
        gap: 2,
        padding: 2,
        borderRadius: radius.sm,
        backgroundColor: color.muted,
        borderWidth: 1,
        borderColor: color.border,
      }}
    >
      <Each of={items} key={(item) => item.id}>
        {(item) => {
          const active = () => props.section === item().id;
          return (
            <View
              testID={`identity-segment-${item().id}`}
              role="button"
              focusable
              onPress={() => store.openIdentitySection(item().id)}
              accessibilityLabel={item().label}
              accessibilityState={{ selected: active() }}
              style={{
                flex: 1,
                minHeight: isMobile() ? BOOK_TOUCH_PX : 28,
                alignItems: "center",
                justifyContent: "center",
                borderRadius: radius.xs,
                backgroundColor: active() ? color.selected : "transparent",
              }}
            >
              <Text style={{ fontSize: typeScale.sm, color: active() ? color.foreground : color.mutedForeground }}>
                {item().label}
                {item().id === "groups" && props.groupCount !== undefined ? ` ${props.groupCount}` : ""}
              </Text>
            </View>
          );
        }}
      </Each>
    </View>
  );
}

/** A pressable row: the whole row is the target (≥44px), keyboard-reachable,
 * with the kit's hover/selected/focus faces. */
export function PressRow(props: {
  onPress?: () => void;
  children?: JSX.Element;
  selected?: boolean;
  disabled?: boolean;
  accessibilityLabel: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <View
      testID={props.testID}
      role="button"
      focusable={props.onPress !== undefined && !props.disabled}
      onPress={props.onPress !== undefined && !props.disabled ? props.onPress : undefined}
      accessibilityLabel={props.accessibilityLabel}
      accessibilityState={{ disabled: props.disabled === true, selected: props.selected === true }}
      hoverStyle={props.onPress !== undefined && !props.disabled ? { backgroundColor: color.hover } : undefined}
      selectedStyle={{ backgroundColor: color.selected }}
      focusStyle={booksFocusStyle()}
      style={[
        {
          flexDirection: "row",
          alignItems: "center",
          gap: space.md,
          minHeight: BOOK_TOUCH_PX,
          paddingHorizontal: space.sm,
          borderBottomWidth: 1,
          borderBottomColor: color.border,
        },
        props.disabled ? { opacity: 0.5 } : null,
        props.style ?? null,
      ]}
    >
      {props.children}
    </View>
  );
}

/** A pressable segmented control for a small closed set (chapters, sections,
 * modes). It is the identity segment's grammar, generalised: 44px on a phone,
 * 28px elsewhere. */
export function Segmented(props: {
  testID?: string;
  accessibilityLabel?: string;
  items: Array<{ id: string; label: string; count?: string }>;
  value: string;
  disabled?: boolean;
  onSelect: (id: string) => void;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <View
      testID={props.testID}
      accessibilityLabel={props.accessibilityLabel}
      style={[
        {
          flexDirection: "row",
          gap: 2,
          padding: 2,
          borderRadius: radius.sm,
          backgroundColor: color.muted,
          borderWidth: 1,
          borderColor: color.border,
        },
        props.style ?? null,
      ]}
    >
      <Each of={props.items} key={(item) => item.id}>
        {(item) => {
          const active = () => props.value === item().id;
          return (
            <View
              testID={props.testID ? `${props.testID}-${item().id}` : undefined}
              role="button"
              focusable={props.disabled !== true}
              onPress={() => {
                if (props.disabled !== true) props.onSelect(item().id);
              }}
              accessibilityLabel={item().label}
              accessibilityState={{ selected: active(), disabled: props.disabled === true }}
              hoverStyle={active() || props.disabled ? undefined : { backgroundColor: color.hover }}
              focusStyle={booksFocusStyle()}
              style={{
                flex: 1,
                minHeight: isMobile() ? BOOK_TOUCH_PX : 28,
                alignItems: "center",
                justifyContent: "center",
                paddingHorizontal: space.sm,
                borderRadius: radius.xs,
                backgroundColor: active() ? color.selected : "transparent",
              }}
            >
              <Text
                style={{
                  fontSize: typeScale.sm,
                  color: active() ? color.foreground : color.mutedForeground,
                  numberOfLines: 1,
                }}
              >
                {item().label}
                {item().count ? ` ${item().count}` : ""}
              </Text>
            </View>
          );
        }}
      </Each>
    </View>
  );
}

/** The object page's spine at ≥1024: the object's identity, its facts, the
 * in-chapter index and the primary action. It is fixed while the paper scrolls,
 * so the reader always knows whose page this is and what remains. */
export function ObjectSpine(props: {
  testID?: string;
  kicker: { icon?: IconName; text: string };
  title: string;
  subtitle?: string;
  description?: string;
  facts?: Array<{ label: string; value: string }>;
  items: Array<{ id: string; num: string; label: string; count?: string }>;
  section: string;
  onSection: (id: string) => void;
  actions?: JSX.Element;
  footer?: JSX.Element;
  testIDPrefix?: string;
}) {
  return (
    <View
      testID={props.testID ?? "object-spine"}
      style={{
        width: isWide() ? BOOK_SPINE_WIDTH_PX : 200,
        flexShrink: 0,
        minHeight: 0,
        flexDirection: "column",
        borderRightWidth: 1,
        borderRightColor: color.border,
        backgroundColor: color.background,
      }}
    >
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
        <View style={{ paddingHorizontal: space.lg, paddingTop: space.lg, paddingBottom: space.md, gap: space.sm }}>
          <Kicker icon={props.kicker.icon} text={props.kicker.text} />
          <Text
            style={{
              fontSize: typeScale["2xl"],
              lineHeight: Math.round(typeScale["2xl"] * lineHeight.tight),
              fontWeight: weight.semibold,
              color: color.foreground,
              letterSpacing: -0.3,
              numberOfLines: 2,
              textOverflow: "ellipsis",
            }}
          >
            {props.title}
          </Text>
          {props.subtitle ? (
            <Text
              style={{
                fontFamily: font.mono,
                fontSize: typeScale.key,
                color: color.mutedForeground,
                numberOfLines: 1,
                textOverflow: "ellipsis",
              }}
            >
              {props.subtitle}
            </Text>
          ) : null}
          {props.description ? (
            <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
              {props.description}
            </Text>
          ) : null}
          {props.facts && props.facts.length ? (
            <View style={{ flexDirection: "column", gap: space.xs, paddingTop: space.xs }}>
              <Each of={props.facts} key={(fact) => fact.label}>
                {(fact) => (
                  <View style={{ flexDirection: "row", alignItems: "baseline", gap: space.sm }}>
                    <Text style={{ width: 58, flexShrink: 0, fontSize: typeScale.sm, color: color.mutedForeground }}>
                      {fact().label}
                    </Text>
                    <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.sm, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                      {fact().value}
                    </Text>
                  </View>
                )}
              </Each>
            </View>
          ) : null}
          <View style={{ flexDirection: "column", gap: 0, paddingTop: space.sm }}>
            <Each of={props.items} key={(item) => item.id}>
              {(item) => {
                const active = () => props.section === item().id;
                return (
                  <View
                    testID={props.testIDPrefix ? `${props.testIDPrefix}-${item().id}` : undefined}
                    role="button"
                    focusable
                    onPress={() => props.onSection(item().id)}
                    accessibilityLabel={`打开${item().label}`}
                    accessibilityState={{ selected: active() }}
                    hoverStyle={active() ? undefined : { backgroundColor: color.hover }}
                    focusStyle={booksFocusStyle()}
                    style={{
                      flexDirection: "row",
                      alignItems: "center",
                      gap: space.sm,
                      minHeight: 32,
                      paddingHorizontal: space.xs,
                      borderLeftWidth: 2,
                      borderLeftColor: active() ? color.foreground : "transparent",
                    }}
                  >
                    <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>{item().num}</Text>
                    <Text style={{ fontSize: typeScale.sm, color: active() ? color.foreground : color.mutedForeground }}>
                      {item().label}
                    </Text>
                    {item().count ? (
                      <Text
                        style={{
                          marginLeft: "auto",
                          fontFamily: font.mono,
                          fontSize: typeScale.key,
                          color: color.mutedForeground,
                        }}
                      >
                        {item().count}
                      </Text>
                    ) : null}
                  </View>
                );
              }}
            </Each>
          </View>
        </View>
      </ScrollArea>
      {props.actions || props.footer ? (
        <View style={{ padding: space.lg, paddingTop: space.md, gap: space.md, borderTopWidth: 1, borderTopColor: color.border }}>
          {props.actions}
          {props.footer}
        </View>
      ) : null}
    </View>
  );
}

/** The index page's paper: 1040 max, 32px gutters, one column. The index pages
 * are not inside the shell's `Paper` (they own their scroll view), so they use
 * this same geometry directly. */
export function PaperBody(props: { children?: JSX.Element; testID?: string }) {
  return (
    <View
      testID={props.testID}
      style={{
        width: "100%",
        maxWidth: BOOK_PAPER_MAX_WIDTH_PX,
        alignSelf: "center",
        paddingHorizontal: isMobile() ? space.lg : space["2xl"],
        paddingTop: space.lg,
        paddingBottom: space["2xl"],
        flexDirection: "column",
        gap: space.lg,
      }}
    >
      {props.children}
    </View>
  );
}

/** A fact line: a fixed-width key and a value that may truncate. */
export function FactLine(props: { label: string; value: string; mono?: boolean }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "baseline", gap: space.md, minWidth: 0 }}>
      <Text style={{ width: 64, flexShrink: 0, fontSize: typeScale.sm, color: color.mutedForeground }}>{props.label}</Text>
      <Text
        style={{
          flex: 1,
          minWidth: 0,
          fontSize: props.mono ? typeScale.key : typeScale.sm,
          fontFamily: props.mono ? font.mono : undefined,
          color: color.foreground,
          numberOfLines: 1,
          textOverflow: "ellipsis",
        }}
      >
        {props.value}
      </Text>
    </View>
  );
}

const riskVariant = (risk: CapabilityRisk): BadgeVariant =>
  risk === "critical" ? "destructive" : risk === "high" ? "warning" : "outline";

/** 常规 / 敏感 / 高危 — an independent yellow→red scale, never the amber
 * "unsaved" wash. */
export function RiskBadge(props: { risk: CapabilityRisk; testID?: string }) {
  return <Badge testID={props.testID} variant={riskVariant(props.risk)} label={RISK_LABEL[props.risk]} />;
}

/** The 32px scope stamp: a value + chevron in a bordered box. 未授予 is a true
 * dashed ghost; a granted scope is a filled stamp; a stamp the reader cannot
 * change carries a lock instead of the chevron. */
export function ScopeStamp(props: { label: string; unset?: boolean; disabled?: boolean; onPress?: () => void; testID?: string }) {
  const unset = () => props.unset === true;
  const ink = () => (unset() ? color.mutedForeground : props.disabled ? color.mutedForeground : color.foreground);
  return (
    <View
      testID={props.testID}
      role={props.onPress ? "button" : "text"}
      focusable={props.onPress !== undefined && !props.disabled}
      onPress={props.onPress !== undefined && !props.disabled ? props.onPress : undefined}
      accessibilityLabel={`范围：${props.label}${unset() && props.label !== "未授予" ? "，未授予" : ""}${props.disabled ? "，只读" : ""}`}
      accessibilityState={{ disabled: props.disabled === true }}
      hoverStyle={props.onPress && !props.disabled ? { borderColor: color.ring } : undefined}
      focusStyle={booksFocusStyle()}
      style={{
        height: isMobile() ? BOOK_TOUCH_PX : 32,
        minWidth: 120,
        paddingHorizontal: space.sm,
        flexDirection: "row",
        alignItems: "center",
        justifyContent: "space-between",
        gap: space.xs,
        borderRadius: radius.sm,
        borderWidth: 1,
        borderStyle: unset() ? "dashed" : "solid",
        borderColor: unset() ? color.border : props.disabled ? color.border : color.fieldEdge,
        backgroundColor: unset() ? "transparent" : color.muted,
      }}
    >
      <Text style={{ fontSize: typeScale.sm, color: ink(), numberOfLines: 1, textOverflow: "ellipsis" }}>{props.label}</Text>
      <Icon
        name={props.disabled ? "lock" : "chevron-down"}
        size={props.disabled ? 11 : 12}
        tint={unset() ? color.mutedForeground : color.mutedForeground}
      />
    </View>
  );
}

/** An inline notice: the page's next step, not a toast. */
export function StateNote(props: { tone: "info" | "warning" | "danger" | "success"; icon?: IconName; children?: JSX.Element; text?: string; testID?: string }) {
  const face = () =>
    props.tone === "danger" ? color.dangerTint : props.tone === "warning" ? color.warningTint : props.tone === "success" ? color.successTint : color.infoTint;
  const ink = () =>
    props.tone === "danger" ? color.danger : props.tone === "warning" ? color.warning : props.tone === "success" ? color.success : color.info;
  return (
    <View
      testID={props.testID}
      accessibilityLabel={props.text}
      style={{
        flexDirection: "row",
        alignItems: "flex-start",
        gap: space.md,
        padding: space.md,
        borderRadius: radius.md,
        backgroundColor: face(),
        borderWidth: 1,
        borderColor: alpha(ink(), 0.3),
      }}
    >
      <Icon name={props.icon ?? "info"} size={14} tint={ink()} />
      <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: space.xxs }}>
        {props.text !== undefined ? (
          <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.foreground }}>
            {props.text}
          </Text>
        ) : null}
        {props.children}
      </View>
    </View>
  );
}

// ---- the five states ------------------------------------------------------

/** Loading: a paper-shaped skeleton (rows matching the table's own rhythm). */
export function PageLoading(props: { rows?: number; label?: string; testID?: string }) {
  const rows = () => props.rows ?? 6;
  return (
    <View
      testID={props.testID}
      accessibilityLabel={props.label ?? "加载中"}
      style={{ flexDirection: "column", gap: space.xl, paddingVertical: space.xl }}
    >
      <View style={{ flexDirection: "column", gap: space.sm }}>
        <Skeleton width={180} height={22} accessibilityLabel="加载中" />
        <Skeleton width="55%" height={12} accessibilityLabel="加载中" />
      </View>
      <View style={{ flexDirection: "column", gap: 0 }}>
        <Each of={Array.from({ length: rows() }, (_, index) => index)} key={(index) => index}>
          {(index) => (
            <View
              testID={`skeleton-row-${index()}`}
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: space.md,
                height: 44,
                borderBottomWidth: 1,
                borderBottomColor: color.border,
              }}
            >
              <Skeleton width={28} height={28} radius={radius.pill} accessibilityLabel="加载中" />
              <Skeleton width="32%" height={10} accessibilityLabel="加载中" />
              <Skeleton width="18%" height={10} accessibilityLabel="加载中" />
              <Skeleton width="14%" height={10} accessibilityLabel="加载中" />
            </View>
          )}
        </Each>
      </View>
    </View>
  );
}

/** Error: what failed, that the reader's edits (when any) are still there, one
 * retry, and the server's request id when it answered one. */
export function PageError(props: { title: string; message: string; onRetry?: () => void; retrying?: boolean; requestId?: string; testID?: string }) {
  return (
    <Empty testID={props.testID} accessibilityLabel={props.title}>
      <EmptyMedia icon="alert" tone="danger" />
      <EmptyTitle label={props.title} />
      <EmptyDescription label={props.message} />
      {props.requestId ? (
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.mutedForeground }}>
          请求编号 {props.requestId}
        </Text>
      ) : null}
      {props.onRetry ? (
        <EmptyContent>
          <Button
            testID={props.testID ? `${props.testID}-retry` : undefined}
            variant="outline"
            label={props.retrying ? "重试中…" : "重试"}
            loading={props.retrying}
            accessibilityLabel="重试"
            onPress={props.onRetry}
            style={touchStyle()}
          />
        </EmptyContent>
      ) : null}
    </Empty>
  );
}

/** Empty: a purpose-built invitation with the one next action. */
export function PageEmpty(props: {
  icon: IconName;
  title: string;
  description: string;
  primaryLabel?: string;
  onPrimary?: () => void;
  secondaryLabel?: string;
  onSecondary?: () => void;
  testID?: string;
  primaryTestID?: string;
}) {
  return (
    <Empty testID={props.testID} accessibilityLabel={props.title}>
      <EmptyMedia icon={props.icon} />
      <EmptyTitle label={props.title} />
      <EmptyDescription label={props.description} />
      {props.primaryLabel || props.secondaryLabel ? (
        <EmptyContent>
          {props.primaryLabel && props.onPrimary ? (
            <Button
              testID={props.primaryTestID}
              label={props.primaryLabel}
              accessibilityLabel={props.primaryLabel}
              onPress={props.onPrimary}
              style={touchStyle()}
            />
          ) : null}
          {props.secondaryLabel && props.onSecondary ? (
            <Button
              variant="outline"
              label={props.secondaryLabel}
              accessibilityLabel={props.secondaryLabel}
              onPress={props.onSecondary}
              style={touchStyle()}
            />
          ) : null}
        </EmptyContent>
      ) : null}
    </Empty>
  );
}

/** 权限不足 (a page the reader reached without the capability). Four parts:
 * what happened, what is missing, who to ask, and the next step. */
export function PagePermission(props: {
  /** What the page needs, in words: 「查看账号」. */
  capability: string;
  /** The technical key, shown under 技术详情 only. */
  capabilityKey?: string;
  testID?: string;
  onCopyRequest?: () => void;
}) {
  const account = () => store.identity()?.displayName || store.identity()?.username || "你的账号";
  const request = () =>
    `我需要「${props.capability}」能力${props.capabilityKey ? `（${props.capabilityKey}）` : ""}，请把我的账号「${account()}」加入有该能力的权限组。`;
  const copy = () => {
    void copyText(request());
    props.onCopyRequest?.();
  };
  return (
    <Empty testID={props.testID ?? "page-permission"} accessibilityLabel="你没有查看这个页面的权限">
      <EmptyMedia icon="lock" tone="danger" />
      <EmptyTitle label="你没有查看这个页面的权限" />
      <EmptyDescription
        label={`需要「${props.capability}」能力。找这台部署的管理员，把你的账号「${account()}」加入有该能力的权限组。`}
      />
      <EmptyContent>
        <Button label="返回工作区" accessibilityLabel="返回工作区" onPress={() => store.openChapter("workspace")} style={touchStyle()} />
        <Button
          variant="outline"
          label="复制申请说明"
          accessibilityLabel="复制申请说明"
          onPress={copy}
          style={touchStyle()}
        />
      </EmptyContent>
      {props.capabilityKey ? (
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.mutedForeground }}>
          技术详情：需要 {props.capabilityKey}
        </Text>
      ) : null}
    </Empty>
  );
}

/** 待接入: a surface whose read route has not landed. It says so, and it never
 * shows a sample row in its place. */
export function PageUnavailable(props: { title: string; description: string; icon?: IconName; testID?: string; children?: JSX.Element }) {
  return (
    <Empty testID={props.testID} accessibilityLabel={props.title}>
      <EmptyMedia icon={props.icon ?? "clock"} />
      <EmptyTitle label={props.title} />
      <EmptyDescription label={props.description} />
      {props.children}
    </Empty>
  );
}
