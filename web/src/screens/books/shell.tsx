// The 账簿 shell — change keel-bot-identity-access, A4.4a.
//
// A 56px chapter band over the paper: 01 工作区 / 02 员工 / 03 团队 / 04 身份 /
// 05 记录, with the reader's own account, the live-channel state, search and the
// deployment's settings entry on the right. A chapter the reader may not read is
// not rendered at all (design §6.1: hide, never grey a destination without a
// path). On a phone the band becomes a compact title row and the same five
// destinations become a 44px bottom bar.
//
// The band's 待办 entry is a real entry with an honest body: the cross-object
// queue (denied calls, failed runs, members without invoke, pending grants) is
// A4.4b's read, so this slice says so instead of printing a made-up count.
import type { JSX } from "nsh";
import { effect, signal, Text, View } from "nsh";
import {
  AlertDialog,
  Avatar,
  Badge,
  Button,
  Dialog,
  DropdownMenu,
  DropdownMenuItem,
  DropdownMenuSeparator,
  IconButton,
  Separator,
  Sheet,
  alpha,
  color,
  font,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
} from "../../ui/kit";
import { Icon, type IconName } from "../../ui/icons";
import { store } from "../../data/store";
import { canAny } from "../../data/permissions";
import { botAccess } from "../../data/bots";
import { openPalette } from "../command-palette";
import { BOOK_BAND_HEIGHT_PX, Hairline, Kicker, PressRow, avatarLetter, booksFocusStyle, copyText, isMobile, touchStyle } from "./common";

export type ChapterId = "workspace" | "bots" | "teams" | "identity" | "records";

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
  { id: "bots", num: "02", label: "员工", icon: "bot", keys: ["bot.read", "bot.manage"] },
  { id: "teams", num: "03", label: "团队", icon: "users", keys: ["team_bot.read", "team_bot.manage"] },
  { id: "identity", num: "04", label: "身份", icon: "shield", keys: ["account.read", "permission.read"] },
  { id: "records", num: "05", label: "记录", icon: "activity", keys: ["audit.read"] },
];

const chapterVisible = (chapter: ChapterEntry): boolean => !chapter.keys || canAny(chapter.keys);

const chapterOf = (): ChapterEntry =>
  CHAPTERS.find((chapter) => chapter.id === store.route()) ?? CHAPTERS[0];

const canRead = (chapter: ChapterEntry): boolean => chapterVisible(chapter);

/** The live channel's own word. The books pages are static projections, so a
 * channel that was never opened is not a failure: it renders nothing. A
 * channel that is connecting or reconnecting says so; an open one is green
 * 实时. There is deliberately no gray 「未连接」 badge on a page that never
 * promised a stream. */
function LiveBadge() {
  const status = () => store.connection().status;
  const visible = () => status() === "open" || status() === "connecting" || status() === "reconnecting";
  const label = () => (status() === "open" ? "实时" : "重连中");
  const variant = () => (status() === "open" ? "success" : "warning");
  return (
    <If when={visible()}>
      <Badge testID="books-live" variant={variant()} label={label()} icon={status() === "open" ? "wifi" : "refresh"} />
    </If>
  );
}

// ---- the band --------------------------------------------------------------

function BandChapter(props: { chapter: ChapterEntry }) {
  const active = () => store.route() === props.chapter.id;
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
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.sm,
        paddingHorizontal: space.lg,
        height: BOOK_BAND_HEIGHT_PX,
        borderBottomWidth: 2,
        borderBottomColor: active() ? color.foreground : "transparent",
      }}
    >
      <Text
        style={{
          fontFamily: font.mono,
          fontSize: typeScale.key,
          letterSpacing: 0.4,
          color: active() ? color.foreground : color.mutedForeground,
        }}
      >
        {props.chapter.num}
      </Text>
      <Text
        style={{
          fontSize: typeScale.base,
          fontWeight: active() ? weight.medium : weight.regular,
          color: active() ? color.foreground : color.mutedForeground,
        }}
      >
        {props.chapter.label}
      </Text>
    </View>
  );
}

/** 待我处理: the real queue this chapter can prove — a team whose roster names a
 * Bot that is archived or no longer visible. A failed run or a revoked
 * `bot.invoke` needs a route the server does not expose yet, so they are named
 * in the empty copy instead of being counted. */
function TodoSheet(props: { open: boolean; onClose: () => void }) {
  const items = () => botAccess.todoItems();
  return (
    <Sheet
      open={props.open}
      side="bottom"
      onOpenChange={(event) => {
        if (!event.open) props.onClose();
      }}
      title="待我处理"
      description="这里汇总需要你决定的团队与成员问题。"
      testID="books-todo-sheet"
      footer={
        <Button
          label="去记录"
          accessibilityLabel="去记录"
          onPress={() => {
            props.onClose();
            store.openChapter("records");
          }}
          style={touchStyle()}
        />
      }
    >
      <If
        when={items().length > 0}
        else={
          <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
            现在没有需要你处理的事。团队运行失败、被别人拒绝的调用，会在「记录」里留下明细；可以按「拒绝」筛选查看。
          </Text>
        }
      >
        <View style={{ flexDirection: "column", gap: 0 }}>
          <Each of={items()} key={(item) => item.teamId}>
            {(item) => (
              <PressRow
                testID={`todo-team-${item().teamId}`}
                accessibilityLabel={`打开团队 ${item().teamName}`}
                onPress={() => {
                  props.onClose();
                  store.openTeam(item().teamId);
                }}
              >
                <Icon name="users" size={14} tint={color.mutedForeground} />
                <View style={{ flex: 1, minWidth: 0 }}>
                  <Text style={{ fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                    {item().teamName}
                  </Text>
                  <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                    {item().reason}
                  </Text>
                </View>
                <Icon name="chevron-right" size={12} tint={color.mutedForeground} />
              </PressRow>
            )}
          </Each>
        </View>
      </If>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        打开团队后，「启动前检查」会指出缺什么、找谁处理。
      </Text>
    </Sheet>
  );
}

/** 我的账号: the whoami facts and the one session action. */
function AccountDialog(props: { open: boolean; onClose: () => void; onSignOut: () => void }) {
  const me = () => store.identity();
  const name = () => me()?.displayName || me()?.username || "当前账号";
  return (
    <Dialog
      open={props.open}
      onOpenChange={(event) => {
        if (!event.open) props.onClose();
      }}
      title="我的账号"
      description="这个浏览器登录的账号，以及它在部署里的席位。"
      testID="books-account-dialog"
      footer={
        <>
          <Button variant="ghost" label="关闭" accessibilityLabel="关闭账号信息" onPress={props.onClose} style={touchStyle()} />
          <Button variant="outline" label="退出登录" accessibilityLabel="退出登录" onPress={props.onSignOut} style={touchStyle()} />
        </>
      }
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
        <Avatar name={name()} size="lg" />
        <View style={{ flexDirection: "column", gap: space.xxs, minWidth: 0 }}>
          <Text style={{ fontSize: typeScale.lg, fontWeight: weight.semibold, color: color.foreground }}>{name()}</Text>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>@{me()?.username || "—"}</Text>
        </View>
        <Badge
          variant={me()?.isSuperAdmin ? "info" : "secondary"}
          label={me()?.isSuperAdmin ? "部署管理员" : "成员"}
          style={{ marginLeft: "auto" }}
        />
      </View>
      <Separator />
      <View style={{ flexDirection: "column", gap: space.sm }}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          账号类型 <Text style={{ color: color.foreground }}>{me()?.kind === "bot" ? "Bot" : me()?.kind === "service" ? "服务" : "人"}</Text>
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          认证方式{" "}
          <Text style={{ color: color.foreground }}>
            {store.mode() === "http" && store.auth() === "ok" ? "浏览器会话" : "开发数据"}
          </Text>
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          租户 <Text style={{ color: color.foreground }}>本部署</Text>
        </Text>
      </View>
      {store.mode() !== "offline" && store.identity() === null ? (
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          还没有读到 whoami；账号菜单里的名称为空时，权限仍以服务端为准。
        </Text>
      ) : null}
    </Dialog>
  );
}

/** The account trigger and its menu: 我的账号 / 设置 / 退出. */
function AccountMenu() {
  const openAccount = signal(false);
  const confirming = signal(false);
  const name = () => store.identity()?.displayName || store.identity()?.username || "账号";
  return (
    <View style={{ flexDirection: "row", alignItems: "center" }}>
      <DropdownMenu
        testID="books-account-menu"
        accessibilityLabel="账号菜单"
        align="end"
        trigger={
          <Button
            variant="ghost"
            accessibilityLabel="账号菜单"
            testID="books-account-trigger"
            style={{ height: BOOK_BAND_HEIGHT_PX, paddingHorizontal: space.xs }}
          >
            <Avatar name={name()} size="sm" />
          </Button>
        }
      >
        <DropdownMenuItem label="我的账号" icon="users" onSelect={() => openAccount.set(true)} />
        <DropdownMenuItem label="部署设置" icon="settings" onSelect={() => store.openSettings()} />
        <DropdownMenuSeparator />
        <DropdownMenuItem label="退出登录" icon="x" danger onSelect={() => confirming.set(true)} />
      </DropdownMenu>
      <AccountDialog open={openAccount()} onClose={() => openAccount.set(false)} onSignOut={() => {
        openAccount.set(false);
        confirming.set(true);
      }} />
      <AlertDialog
        testID="books-signout-dialog"
        open={confirming()}
        title="退出登录？"
        description="服务端会话立即失效，需要重新输入用户名和密码。"
        confirmLabel="确认退出"
        cancelLabel="取消"
        destructive
        onConfirm={() => {
          confirming.set(false);
          void store.logout();
        }}
        onCancel={() => confirming.set(false)}
      />
    </View>
  );
}

// ---- the phone's bottom bar -------------------------------------------------

function BottomBar() {
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
        minHeight: BOOK_BAND_HEIGHT_PX,
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
                minHeight: BOOK_BAND_HEIGHT_PX,
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
    </View>
  );
}

/** The phone's compact top row: the chapter's number and name, search, settings
 * and the account menu. */
function PhoneTopBar(props: { onTodo: () => void }) {
  const chapter = () => chapterOf();
  const pageLabel = () =>
    store.route() === "teams" && store.routeRunId() !== null
      ? "运行"
      : store.route() === "bots" && store.routeObjectId() !== null
        ? "Bot"
        : chapter().label;
  return (
    <View
      testID="books-phone-top"
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.sm,
        height: 48,
        paddingHorizontal: space.lg,
        borderBottomWidth: 1,
        borderBottomColor: color.border,
      }}
    >
      <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>{chapter().num}</Text>
      <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>{pageLabel()}</Text>
      <View style={{ marginLeft: "auto", flexDirection: "row", alignItems: "center", gap: space.xs }}>
        <IconButton testID="books-phone-todo" icon="alert" label="待我处理" onPress={props.onTodo} style={{ width: 44, height: 44 }} />
        <IconButton testID="books-phone-search" icon="search" label="搜索" onPress={openPalette} style={{ width: 44, height: 44 }} />
        <IconButton testID="books-phone-settings" icon="settings" label="打开部署设置" onPress={() => store.openSettings()} style={{ width: 44, height: 44 }} />
        <AccountMenu />
      </View>
    </View>
  );
}

/** The band, its sheets and the paper below it. A chapter the reader cannot read
 * still renders its route's own permission page (the chapters.tsx router); the
 * band simply does not offer it. */
export function BooksChrome(props: { children?: JSX.Element }) {
  const todoOpen = signal(false);
  // The cross-object queue needs the two lists the team/bot chapters own. It is
  // read when the shell mounts (not invented): a team whose roster names an
  // archived or invisible Bot is the one pending item this slice can prove.
  effect(() => {
    if (canAny(["bot.read", "bot.manage"])) botAccess.ensureBots();
    if (canAny(["team_bot.read", "team_bot.manage"])) botAccess.ensureTeams();
  });
  // The workspace keeps its own header on a phone (the session list's brand
  // row), so the band's phone row is not drawn there: one header, not two. The
  // bottom bar still carries the chapters.
  return (
    <View testID="books-shell" style={{ flex: 1, minHeight: 0, backgroundColor: color.background }}>
      <If when={!isMobile()} else={<If when={store.route() !== "workspace"}><PhoneTopBar onTodo={() => todoOpen.set(true)} /></If>}>
        <DesktopBand onTodo={() => todoOpen.set(true)} />
      </If>
      <View style={{ flex: 1, minHeight: 0, flexDirection: "row" }}>{props.children}</View>
      <If when={isMobile()}>
        <BottomBar />
      </If>
      <TodoSheet open={todoOpen()} onClose={() => todoOpen.set(false)} />
    </View>
  );
}

function DesktopBand(props: { onTodo: () => void }) {
  const todoCount = () => botAccess.todoItems().length;
  return (
    <View
      testID="books-band"
      style={{
        flexDirection: "row",
        alignItems: "stretch",
        height: BOOK_BAND_HEIGHT_PX,
        paddingHorizontal: space.xl,
        borderBottomWidth: 1,
        borderBottomColor: color.border,
        backgroundColor: color.background,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, paddingRight: space.md }}>
        <View
          accessibilityLabel="Keel"
          style={{
            width: 22,
            height: 22,
            borderRadius: radius.sm,
            alignItems: "center",
            justifyContent: "center",
            borderWidth: 1,
            borderColor: alpha(color.foreground, 0.34),
          }}
        >
          <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, fontWeight: weight.semibold, color: color.foreground }}>K</Text>
        </View>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, letterSpacing: 1.4, color: color.foreground }}>KEEL</Text>
        <Text testID="books-deployment" style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          本部署
        </Text>
      </View>
      <Hairline style={{ width: 1, height: 20, alignSelf: "center", marginRight: space.lg }} />
      <View style={{ flexDirection: "row", alignItems: "stretch", minWidth: 0 }}>
        <Each of={CHAPTERS.filter((chapter) => chapterVisible(chapter))} key={(chapter) => chapter.id}>
          {(chapter) => <BandChapter chapter={chapter()} />}
        </Each>
      </View>
      <View style={{ marginLeft: "auto", flexDirection: "row", alignItems: "center", gap: space.md }}>
        <Button
          testID="books-todo"
          variant="ghost"
          size="sm"
          icon="alert"
          label={todoCount() > 0 ? `待办 ${todoCount()}` : "待办"}
          accessibilityLabel={todoCount() > 0 ? `待我处理，${todoCount()} 项` : "待我处理"}
          onPress={props.onTodo}
        />
        <If when={store.route() !== "workspace"}>
          <LiveBadge />
        </If>
        <IconButton testID="books-search" icon="search" label="搜索" onPress={openPalette} />
        <IconButton testID="books-settings" icon="settings" label="打开部署设置" onPress={() => store.openSettings()} />
        <AccountMenu />
      </View>
    </View>
  );
}

// ---- the paper -------------------------------------------------------------

/** The identity chapter's 280px index column (≥1440 only). */
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
