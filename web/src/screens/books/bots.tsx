// The 02 员工 chapter — change keel-bot-identity-access, A4.4b.
//
// A Bot index and one Bot page. The index answers 「我的 AI 员工有哪些、各自能
// 干什么、现在能不能用」; the page is the Bot's spec sheet: identity (Profile =
// account + persona), working mode (model kind, default model, instruction),
// capabilities (account range ∩ harness → effective), keys and the session
// index.
//
// Two product facts drive the copy:
//   - a Bot's Harness permission list is a strict allow-list. Empty does not
//     mean "inherit the account"; it means the Bot can do nothing. The page says
//     so instead of following the older design's inheritance wording.
//   - the model's answer is not this page's: 试跑 starts a real session and the
//     page shows the receipt, then opens the workspace for the stream.
import { effect, signal, Text, View } from "nsh";
import {
  AlertDialog,
  Badge,
  Button,
  Checkbox,
  Dialog,
  DropdownMenu,
  DropdownMenuItem,
  DropdownMenuSeparator,
  Field,
  Input,
  ScrollArea,
  Select,
  Sheet,
  Table,
  TableBodyRow,
  TableHeaderRow,
  Td,
  Textarea,
  Th,
  alpha,
  color,
  font,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
} from "../../ui/kit";
import { Icon } from "../../ui/icons";
import { store } from "../../data/store";
import { can, canAny } from "../../data/permissions";
import { identityAccess } from "../../data/identity-access";
import { botAccess, type BotChange, type BotOption } from "../../data/bots";
import { canShareResource, openBotShare, sharedBadgeOf } from "../../data/shares";
import { capabilityOf, scopeLabel, type CapabilityRisk } from "../../data/permission-catalog";
import type { BotKeyView, BotView, ModelRef, PermissionScope } from "../../data/identity";
import { displayNameOf, selectionDisplay } from "../../data/wire";
import {
  BOOK_FIELD_HEIGHT_PX,
  BandHead,
  BookAvatar,
  FactLine,
  Hairline,
  Kicker,
  ObjectSpine,
  PageEmpty,
  PageError,
  PageLoading,
  PagePermission,
  PageUnavailable,
  PaperBody,
  PressRow,
  RiskBadge,
  Segmented,
  SheetHead,
  StateNote,
  copyText,
  fieldHeightProp,
  formatWhen,
  isMobile,
  isNarrow,
  touchStyle,
} from "./common";
import { BackToIndex } from "./shell";

// ---- labels ---------------------------------------------------------------

const MODEL_KIND_LABEL: Record<string, string> = {
  text: "文本",
  image: "图像",
  decision: "决策",
  realtime: "实时",
};

const modelKindLabel = (kind: string | undefined): string => (kind ? MODEL_KIND_LABEL[kind] ?? kind : "未设置");

const MODEL_KIND_ITEMS = [
  { value: "text", title: "文本" },
  { value: "image", title: "图像" },
  { value: "decision", title: "决策" },
  { value: "realtime", title: "实时" },
];

const modelKeyOf = (model: ModelRef | undefined): string =>
  model && model.providerId && model.modelId ? `${model.providerId}/${model.modelId}` : "";

const modelOfKey = (key: string): ModelRef | undefined => {
  const slash = key.indexOf("/");
  if (slash <= 0) return undefined;
  return { providerId: key.slice(0, slash), modelId: key.slice(slash + 1) };
};

const modelLabel = (model: ModelRef | undefined): string => {
  if (!model || (!model.providerId && !model.modelId)) return "未设置";
  return `${model.providerId ? `${model.providerId} / ` : ""}${displayNameOf(model.modelId) || model.modelId}`;
};

const statusBadge = (bot: BotView) =>
  bot.archived ? (
    <Badge variant="secondary" label="已归档" icon="lock" style={{ alignSelf: "flex-start" }} />
  ) : (
    <Badge variant="success" label="可用" style={{ alignSelf: "flex-start" }} />
  );

const accountLabel = (accountId: string | undefined): string =>
  botAccess.accountNameOf(accountId) || (accountId ? "账号名未读取" : "未知账号");

// ---- the page state -------------------------------------------------------

type BotSection = "identity" | "work" | "capabilities" | "keys" | "sessions";

const BOT_SECTIONS: Array<{ id: BotSection; num: string; label: string }> = [
  { id: "identity", num: "01", label: "身份" },
  { id: "work", num: "02", label: "工作方式" },
  { id: "capabilities", num: "03", label: "能力" },
  { id: "keys", num: "04", label: "密钥" },
  { id: "sessions", num: "05", label: "会话" },
];

const botSection = signal<BotSection>("capabilities");
const botShowAll = signal(false);
const botCapQuery = signal("");
const botTryOpen = signal(false);
const botTryText = signal("帮我查一下订单 88213 的物流状态，并拟一条给客户的回复。");
const botLedgerOpen = signal(false);
const botConfirmHigh = signal<Record<string, boolean>>({});
const botArchiveOpen = signal(false);
const botIssueOpen = signal(false);
const botIssueLabel = signal("");
const botRevoke = signal<{ keyId: string; label: string } | null>(null);
const botRebindOpen = signal(false);
const botRebindAccount = signal("");
const botRebindProfile = signal("");
const botCreateOpen = signal(false);
const botCreateName = signal("");
const botCreateProfile = signal("");
const botCreateKind = signal("text");
const botCreateModel = signal("");
const botKeyCopied = signal(false);
const lastBotId = signal<string | null>(null);

const sectionLabel = (section: BotSection): string => BOT_SECTIONS.find((item) => item.id === section)?.label ?? "能力";

const sectionNum = (section: BotSection): string => BOT_SECTIONS.find((item) => item.id === section)?.num ?? "03";

/** Drop the reader's transient UI (sections, sheets, drafts' confirmations) when
 * the session changes. Data lives in `botAccess`, which clears itself. */
export function resetBotsUi(): void {
  botSection.set("capabilities");
  botShowAll.set(false);
  botCapQuery.set("");
  botTryOpen.set(false);
  botTryText.set("帮我查一下订单 88213 的物流状态，并拟一条给客户的回复。");
  botLedgerOpen.set(false);
  botConfirmHigh.set({});
  botArchiveOpen.set(false);
  botIssueOpen.set(false);
  botIssueLabel.set("");
  botRevoke.set(null);
  botRebindOpen.set(false);
  botRebindAccount.set("");
  botRebindProfile.set("");
  botCreateOpen.set(false);
  botCreateName.set("");
  botCreateProfile.set("");
  botCreateKind.set("text");
  botCreateModel.set("");
  botKeyCopied.set(false);
  lastBotId.set(null);
}

// ---- the chapter ----------------------------------------------------------

export function BotsChapter() {
  const objectId = () => store.routeObjectId();
  const allowed = () => canAny(["bot.read", "bot.manage"]);
  effect(() => {
    if (allowed()) botAccess.ensureBots();
  });
  return (
    <View testID="bots-chapter" style={{ flex: 1, minHeight: 0, flexDirection: "row" }}>
      <If
        when={allowed()}
        else={
          <View style={{ flex: 1, minHeight: 0, justifyContent: "center" }}>
            <PagePermission capability="查看 Bot" capabilityKey="bot.read" />
          </View>
        }
      >
        <If when={objectId() !== null} else={<BotsIndex />}>
          <BotPage botId={objectId() ?? ""} />
        </If>
      </If>
      <BotDialogs />
    </View>
  );
}

// ---- the index ------------------------------------------------------------

export function BotsIndex() {
  const visible = () => botAccess.visibleBots();
  const loading = () => botAccess.botsLoading() && botAccess.bots().length === 0;
  const failed = () => botAccess.botsError() !== null;
  const error = () => botAccess.botsError();
  const active = () => botAccess.bots().filter((bot) => !bot.archived).length;
  const archived = () => botAccess.bots().filter((bot) => bot.archived).length;
  const filtered = () => !!botAccess.botsQuery().trim() || botAccess.botsModelKind() !== "all" || botAccess.botsStatus() !== "all";
  return (
    <ScrollArea testID="bots-index" accessibilityLabel="Bot 索引" style={{ flex: 1, minHeight: 0 }}>
      <PaperBody testID="bots-paper">
        <SheetHead
          testID="bots-head"
          kicker={{ icon: "bot", text: "02 / 员工 · Bot" }}
          title="员工"
          badges={
            <>
              <Badge variant="secondary" label={`${botAccess.bots().length} 个 Bot`} />
              <Badge variant="secondary" label={`${active()} 个可用`} />
              <If when={archived() > 0}>
                <Badge variant="outline" label={`${archived()} 个已归档`} />
              </If>
            </>
          }
          description="我的 AI 员工有哪些、各自能干什么、现在能不能用。"
          actions={
            <If when={can("bot.create")}>
              <Button
                testID="bots-create"
                size="field"
                icon="plus"
                label="新建 Bot"
                accessibilityLabel="新建 Bot"
                onPress={() => botCreateOpen.set(true)}
                style={touchStyle()}
              />
            </If>
          }
        />
        <View style={{ flexDirection: "row", gap: space.sm, flexWrap: "wrap", alignItems: "center" }}>
          <Input
            testID="bots-search"
            value={botAccess.botsQuery()}
            onChange={(value) => botAccess.botsQuery.set(value)}
            placeholder="搜索名字、账号或模型"
            accessibilityLabel="搜索 Bot"
            size="field"
            height={fieldHeightProp()}
            style={{ width: isMobile() ? "100%" : 260 }}
          />
          <View style={{ width: isMobile() ? "48%" : 140 }}>
            <Select
              testID="bots-kind-filter"
              size="field"
              items={[{ value: "all", title: "全部模型类型" }, ...MODEL_KIND_ITEMS]}
              value={botAccess.botsModelKind()}
              onChange={(value) => botAccess.botsModelKind.set(value ?? "all")}
              accessibilityLabel="按模型类型筛选"
              height={fieldHeightProp()}
            />
          </View>
          <View style={{ width: isMobile() ? "48%" : 130 }}>
            <Select
              testID="bots-status-filter"
              size="field"
              items={[
                { value: "all", title: "全部状态" },
                { value: "active", title: "可用" },
                { value: "archived", title: "已归档" },
              ]}
              value={botAccess.botsStatus()}
              onChange={(value) => botAccess.botsStatus.set((value ?? "all") as "all" | "active" | "archived")}
              accessibilityLabel="按状态筛选"
              height={fieldHeightProp()}
            />
          </View>
        </View>

        <If when={loading()}>
          <PageLoading rows={6} testID="bots-index-loading" />
        </If>

        <If when={failed() && !loading()}>
          <If when={error()?.status === 403} else={<PageError
            title="没能读到 Bot 列表"
            message={error()?.unavailable ? "这个部署还没有接入 Bot 索引。" : error()?.message ?? ""}
            requestId={error()?.requestId}
            onRetry={() => void botAccess.loadBots()}
            retrying={botAccess.botsLoading()}
            testID="bots-index-error"
          />}>
            <PagePermission
              capability={capabilityOf(error()?.requiredKey ?? "bot.read").displayName}
              capabilityKey={error()?.requiredKey ?? "bot.read"}
            />
          </If>
        </If>

        <If when={!loading() && !failed() && botAccess.bots().length === 0}>
          <PageEmpty
            testID="bots-index-empty"
            icon="bot"
            title="还没有 AI 员工"
            description="先建一个 Bot：给它一个身份、一个默认模型和一组工具。"
            primaryLabel={can("bot.create") ? "创建第一个 Bot" : undefined}
            onPrimary={() => botCreateOpen.set(true)}
            secondaryLabel="去身份"
            onSecondary={() => store.openChapter("identity")}
          />
        </If>

        <If when={!loading() && !failed() && botAccess.bots().length > 0 && visible().length === 0}>
          <PageEmpty
            icon="search"
            title="没有匹配的 Bot"
            description="换个名字或清空筛选；筛选不会改变服务端的名单。"
            primaryLabel="清空筛选"
            onPrimary={() => {
              botAccess.botsQuery.set("");
              botAccess.botsModelKind.set("all");
              botAccess.botsStatus.set("all");
            }}
          />
        </If>

        <If when={!loading() && !failed() && visible().length > 0}>
          <If when={isNarrow()} else={<BotsTable rows={visible()} />}>
            <BotsCards rows={visible()} />
          </If>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, paddingTop: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              显示 {visible().length} / {botAccess.bots().length} 个 Bot
              {filtered() ? "（已筛选）" : ""}
            </Text>
            <If when={botAccess.botsHasMore()}>
              <Button
                testID="bots-load-more"
                variant="ghost"
                size="sm"
                label="加载更多"
                accessibilityLabel="加载更多 Bot"
                loading={botAccess.botsLoading()}
                onPress={() => void botAccess.loadMoreBots()}
                style={touchStyle()}
              />
            </If>
          </View>
          <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
            「最近活动」还没有从服务端返回；这一列留空，不显示推测值。打开一个 Bot 可以看它的工作方式、能力与密钥。
          </Text>
        </If>
      </PaperBody>
    </ScrollArea>
  );
}

function BotsTable(props: { rows: BotView[] }) {
  return (
    <Table testID="bots-table" bare accessibilityLabel="Bot 列表">
      <TableHeaderRow testID="bots-table-head">
        <Th>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>Bot</Text>
        </Th>
        <Th width={150}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>所属账号</Text>
        </Th>
        <Th width={86}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>模型类型</Text>
        </Th>
        <Th width={170}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>默认模型</Text>
        </Th>
        <Th width={90}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>状态</Text>
        </Th>
        <Th width={110}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>最近活动</Text>
        </Th>
      </TableHeaderRow>
      <Each of={props.rows} key={(bot) => bot.botId}>
        {(bot) => (
          <TableBodyRow testID={`bot-row-${bot().botId}`}>
            <Td>
              <PressRow
                accessibilityLabel={`打开 Bot ${bot().name}`}
                onPress={() => store.openBot(bot().botId)}
                style={{ borderBottomWidth: 0, minHeight: 44, marginHorizontal: -space.lg, paddingHorizontal: space.lg }}
              >
                <BookAvatar name={bot().name} />
                <View style={{ flexDirection: "column", minWidth: 0 }}>
                  <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                    {bot().name}
                  </Text>
                  <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                    {bot().botId}
                  </Text>
                </View>
              </PressRow>
            </Td>
            <Td>
              <Text style={{ fontSize: typeScale.sm, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                {accountLabel(bot().accountId)}
              </Text>
            </Td>
            <Td>
              <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>{modelKindLabel(bot().modelKind)}</Text>
            </Td>
            <Td>
              <View style={{ flexDirection: "column", minWidth: 0 }}>
                <Text style={{ fontSize: typeScale.sm, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                  {displayNameOf(bot().defaultModel.modelId) || bot().defaultModel.modelId || "未设置"}
                </Text>
                <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                  {bot().defaultModel.providerId || "—"}
                </Text>
              </View>
            </Td>
            <Td>
              <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}>
                {statusBadge(bot())}
                <If when={sharedBadgeOf(bot().sharedCount) !== undefined}>
                  <Badge variant="outline" label={sharedBadgeOf(bot().sharedCount)} />
                </If>
              </View>
            </Td>
            <Td>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>—</Text>
            </Td>
          </TableBodyRow>
        )}
      </Each>
    </Table>
  );
}

function BotsCards(props: { rows: BotView[] }) {
  return (
    <View style={{ flexDirection: "column", gap: space.sm }}>
      <Each of={props.rows} key={(bot) => bot.botId}>
        {(bot) => (
          <PressRow
            testID={`bot-card-${bot().botId}`}
            accessibilityLabel={`打开 Bot ${bot().name}`}
            onPress={() => store.openBot(bot().botId)}
            style={{
              flexDirection: "column",
              alignItems: "stretch",
              gap: space.xs,
              padding: space.md,
              borderRadius: radius.md,
              borderWidth: 1,
              borderColor: color.border,
              borderBottomWidth: 1,
              minHeight: 64,
            }}
          >
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
              <BookAvatar name={bot().name} />
              <View style={{ flex: 1, minWidth: 0 }}>
                <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                  {bot().name}
                </Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                  {accountLabel(bot().accountId)} · {modelKindLabel(bot().modelKind)}
                </Text>
              </View>
              <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}>
                {statusBadge(bot())}
                <If when={sharedBadgeOf(bot().sharedCount) !== undefined}>
                  <Badge variant="outline" label={sharedBadgeOf(bot().sharedCount)} />
                </If>
              </View>
            </View>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
              {modelLabel(bot().defaultModel)} · 最近活动未记录
            </Text>
          </PressRow>
        )}
      </Each>
    </View>
  );
}

// ---- the page -------------------------------------------------------------

export function BotPage(props: { botId: string }) {
  const id = () => props.botId;
  effect(() => {
    const botId = id();
    if (lastBotId() !== botId) {
      lastBotId.set(botId);
      botSection.set("capabilities");
      botShowAll.set(false);
      botCapQuery.set("");
      botConfirmHigh.set({});
    }
    if (botAccess.botDetailId() !== botId || botAccess.botDetail() === null) void botAccess.loadBot(botId);
  });
  const bot = () => botAccess.botDetail();
  const loading = () => botAccess.botDetailLoading() && bot() === null;
  const failed = () => botAccess.botDetailError() !== null;
  const error = () => botAccess.botDetailError();
  const name = () => bot()?.name ?? botAccess.bots().find((entry) => entry.botId === id())?.name ?? "Bot";
  const dirty = () => botAccess.botChanges().length > 0;
  const canManage = () => can("bot.manage", { archived: bot()?.archived === true });
  const canBind = () => can("bot.bind", { archived: bot()?.archived === true });
  const tryReason = () =>
    bot()?.archived
      ? "这个 Bot 已归档，不能开始新的会话。"
      : !can("bot.invoke", { archived: false })
        ? "你需要这个 Bot 的调用权，才能试跑。"
        : "";
  return (
    <View testID="bot-page" style={{ flex: 1, minHeight: 0, flexDirection: "row" }}>
      <If when={!isNarrow()}>
        <ObjectSpine
          testID="bot-spine"
          testIDPrefix="bot-section"
          kicker={{ icon: "bot", text: "员工 / Bot" }}
          title={name()}
          subtitle={bot() ? `${modelKindLabel(bot()!.modelKind)} · ${modelLabel(bot()!.defaultModel)}` : ""}
          description={bot()?.harness.context.instruction || undefined}
          facts={
            bot()
              ? [
                  { label: "归属者", value: accountLabel(bot()!.accountId) },
                  { label: "人设", value: botAccess.roleOf(botAccess.profileOf(bot()))?.name ?? "未读取" },
                  { label: "最近活动", value: "未记录" },
                  { label: "共享", value: sharedBadgeOf(bot()!.sharedCount) ?? "未共享" },
                  { label: "状态", value: bot()!.archived ? "已归档" : "可用" },
                ]
              : []
          }
          items={BOT_SECTIONS.map((item) => ({
            ...item,
            count:
              item.id === "capabilities"
                ? `${botAccess.botCapabilityRows(false, "").length}`
                : item.id === "keys"
                  ? botAccess.botKeys().filter((key) => !key.revokedAt).length
                    ? String(botAccess.botKeys().filter((key) => !key.revokedAt).length)
                    : undefined
                  : undefined,
          }))}
          section={botSection()}
          onSection={(next) => {
            botSection.set(next as BotSection);
            if (next === "keys" && canManage() && !botAccess.botKeysLoaded()) void botAccess.loadBotKeys(id());
          }}
          actions={
            <>
              <Button
                testID="bot-try"
                variant={dirty() ? "outline" : "default"}
                size="sm"
                icon="play"
                label="试跑"
                accessibilityLabel="试跑这个 Bot"
                disabled={tryReason() !== ""}
                onPress={() => {
                  botAccess.resetTryRun();
                  botTryOpen.set(true);
                }}
                style={{ width: "100%" }}
              />
              <DropdownMenu
                testID="bot-more"
                accessibilityLabel="更多 Bot 操作"
                align="start"
                trigger={<Button variant="ghost" size="sm" icon="dots" label="更多" accessibilityLabel="更多 Bot 操作" style={{ width: "100%" }} />}
              >
                <DropdownMenuItem
                  label="共享 Bot"
                  icon="users"
                  disabled={!canShareResource("bot", bot()?.accountId)}
                  onSelect={() => openBotShare(id(), name())}
                />
                <DropdownMenuItem
                  label="改绑身份档案"
                  icon="users"
                  disabled={!canBind() || !canManage()}
                  onSelect={() => {
                    botRebindAccount.set(bot()?.accountId ?? "");
                    botRebindProfile.set("");
                    botRebindOpen.set(true);
                  }}
                />
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  label="归档 Bot"
                  icon="trash"
                  danger
                  disabled={!canManage() || bot()?.archived === true}
                  onSelect={() => botArchiveOpen.set(true)}
                />
              </DropdownMenu>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, lineHeight: Math.round(typeScale.sm * lineHeight.normal) }}>
                {tryReason() ||
                  (dirty()
                    ? "有未保存的变更；保存后立即影响下一次会话。试跑仍使用已保存的工作范围。"
                    : `最近保存 ${formatWhen(bot()?.updatedAt)}`)}
              </Text>
            </>
          }
        />
      </If>
      <View style={{ flex: 1, minWidth: 0, minHeight: 0, flexDirection: "column" }}>
        <ScrollArea testID="bot-page-scroll" accessibilityLabel={`Bot ${name()}`} style={{ flex: 1, minHeight: 0 }}>
          <View
            style={{
              width: "100%",
              maxWidth: 1040,
              alignSelf: "center",
              paddingHorizontal: isMobile() ? space.lg : space["2xl"],
              paddingTop: space.lg,
              paddingBottom: space.lg,
              flexDirection: "column",
              gap: space.lg,
            }}
          >
            <If when={isNarrow()}>
              <BackToIndex label="员工" onPress={() => store.closeBot()} />
              <BotSummary botId={id()} />
            </If>
            <If when={loading()}>
              <PageLoading rows={6} testID="bot-loading" />
            </If>
            <If when={failed() && !loading()}>
              <If when={error()?.status === 403} else={<PageError
                title="没能读到这个 Bot"
                message={error()?.message ?? ""}
                requestId={error()?.requestId}
                onRetry={() => void botAccess.loadBot(id())}
                retrying={botAccess.botDetailLoading()}
                testID="bot-error"
              />}>
                <PagePermission
                  capability={capabilityOf(error()?.requiredKey ?? "bot.read").displayName}
                  capabilityKey={error()?.requiredKey ?? "bot.read"}
                />
              </If>
            </If>
            <If when={bot() !== null && !failed()}>
              <If when={botAccess.botNotice() !== null}>
                <StateNote tone="success" icon="check" text={botAccess.botNotice() ?? ""} testID="bot-notice" />
              </If>
              <If when={botAccess.botActionError() !== null}>
                <StateNote tone="danger" icon="alert" text={botAccess.botActionError() ?? ""} testID="bot-action-error" />
              </If>
              <If when={botAccess.botSaveError() !== null}>
                <StateNote tone="danger" icon="alert" text={botAccess.botSaveError() ?? ""} testID="bot-save-error" />
              </If>
              <If when={bot()?.archived === true}>
                <StateNote tone="warning" icon="lock" text="这个 Bot 已归档：不能开始新会话或签发密钥；历史会话仍然可读。" />
              </If>
              <BandHead
                testID="bot-band-head"
                num={`02 / ${sectionNum(botSection())}`}
                title={sectionLabel(botSection())}
                right={
                  isNarrow() ? undefined : (
                    <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                      {botSection() === "capabilities" ? "账号范围 · Bot 授权 → 有效能力" : accountLabel(bot()?.accountId)}
                    </Text>
                  )
                }
              />
              <If when={botSection() === "identity"}><BotIdentitySection /></If>
              <If when={botSection() === "work"}><BotWorkSection /></If>
              <If when={botSection() === "capabilities"}><BotCapabilitiesSection /></If>
              <If when={botSection() === "keys"}><BotKeysSection botId={id()} /></If>
              <If when={botSection() === "sessions"}><BotSessionsSection /></If>
            </If>
          </View>
        </ScrollArea>
        <If when={!isMobile() && bot() !== null && !failed() && botAccess.botChanges().length > 0}>
          <View style={{ width: "100%", maxWidth: 1040, alignSelf: "center", paddingHorizontal: space["2xl"] }}>
            <BotSignoff />
          </View>
        </If>
        <If when={isMobile() && bot() !== null && !failed()}>
          <BotMobileBar canManage={canManage()} tryReason={tryReason()} />
        </If>
      </View>
    </View>
  );
}

/** The phone/tablet summary: the object, three facts, the section segment. */
function BotSummary(props: { botId: string }) {
  const bot = () => botAccess.botDetail();
  const role = () => botAccess.roleOf(botAccess.profileOf(bot()))?.name;
  const dirty = () => botAccess.botChanges().length;
  const available = () => botAccess.botCapabilityRows(false, "").filter((row) => row.effective === "usable").length;
  const high = () => botAccess.botChanges().filter((change) => change.highRisk).length;
  return (
    <View style={{ flexDirection: "column", gap: space.sm }}>
      <View
        style={{
          flexDirection: "column",
          gap: space.xs,
          padding: space.md,
          borderRadius: radius.md,
          borderWidth: 1,
          borderColor: color.border,
          backgroundColor: color.hoverFaint,
        }}
      >
        <Kicker icon="bot" text="员工 · Bot" />
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Text
            style={{
              flex: 1,
              minWidth: 0,
              fontSize: typeScale.section,
              fontWeight: weight.semibold,
              color: color.foreground,
              numberOfLines: 1,
              textOverflow: "ellipsis",
            }}
          >
            {bot()?.name ?? "Bot"}
          </Text>
          <If when={bot()?.archived === true} else={<Badge variant="success" label="可用" />}>
            <Badge variant="secondary" label="已归档" icon="lock" />
          </If>
        </View>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {modelKindLabel(bot()?.modelKind)} · {modelLabel(bot()?.defaultModel)} · 归属者 {accountLabel(bot()?.accountId)}
          {role() ? ` · 人设 ${role()}` : ""}
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {available()} 项有效能力
          {dirty() > 0 ? ` · ${dirty()} 项未保存` : ""}
          {high() > 0 ? ` · 含 ${high()} 项高危` : ""}
        </Text>
      </View>
      <Segmented
        testID="bot-segment"
        accessibilityLabel="Bot 章节"
        items={BOT_SECTIONS.map((item) => ({
          id: item.id,
          label: item.label,
          count: item.id === "capabilities" ? String(botAccess.botCapabilityRows(false, "").length) : undefined,
        }))}
        value={botSection()}
        onSelect={(next) => {
          botSection.set(next as BotSection);
          if (next === "keys" && can("bot.manage") && !botAccess.botKeysLoaded()) void botAccess.loadBotKeys(props.botId);
        }}
      />
    </View>
  );
}

function BotMobileBar(props: { canManage: boolean; tryReason: string }) {
  const dirty = () => botAccess.botChanges().length > 0;
  const high = () => botAccess.botChanges().filter((change) => change.highRisk).length;
  return (
    <View
      testID="bot-mobile-bar"
      style={{
        flexDirection: "row",
        gap: space.sm,
        padding: space.md,
        borderTopWidth: 1,
        borderTopColor: color.border,
        backgroundColor: color.background,
      }}
    >
      <If
        when={dirty()}
        else={
          <Button
            size="lg"
            icon="play"
            label="试跑"
            accessibilityLabel="试跑这个 Bot"
            disabled={props.tryReason !== ""}
            onPress={() => {
              botAccess.resetTryRun();
              botTryOpen.set(true);
            }}
            style={{ flex: 1, height: 44 }}
          />
        }
      >
        <Button
          variant="outline"
          size="lg"
          icon="play"
          label="试跑"
          accessibilityLabel="试跑这个 Bot"
          disabled={props.tryReason !== ""}
          onPress={() => {
            botAccess.resetTryRun();
            botTryOpen.set(true);
          }}
          style={{ flex: 1, height: 44 }}
        />
        <Button
          size="lg"
          icon="check"
          label="检查并登记"
          accessibilityLabel="检查并登记"
          disabled={!props.canManage}
          onPress={() => {
            botConfirmHigh.set({});
            botLedgerOpen.set(true);
          }}
          style={{ flex: 1, height: 44 }}
        />
      </If>
    </View>
  );
}

// ---- identity -------------------------------------------------------------

function BotIdentitySection() {
  const bot = () => botAccess.botDetail();
  const profile = () => botAccess.profileOf(bot());
  const role = () => botAccess.roleOf(profile());
  const account = () => identityAccess.accounts().find((entry) => entry.accountId === bot()?.accountId);
  const canManage = () => can("bot.manage", { archived: bot()?.archived === true });
  const canBind = () => can("bot.bind", { archived: bot()?.archived === true });
  return (
    <View testID="bot-identity" style={{ flexDirection: "column", gap: space.lg }}>
      <View style={{ flexDirection: "column", gap: space.sm, maxWidth: 720 }}>
        <FactLine label="归属账号" value={account()?.displayName || account()?.username || bot()?.accountId || "未读取"} />
        <FactLine label="账号类型" value={account()?.kind === "bot" ? "Bot 账号" : account()?.kind === "service" ? "服务账号" : "人账号"} />
        <FactLine label="人设" value={role()?.name ?? (botAccess.profilesError() ? "未读取（缺「查看身份档案」）" : "未设置")} />
        <FactLine label="身份档案" value={bot()?.profileId ?? "—"} mono />
        <FactLine label="创建时间" value={formatWhen(bot()?.createdAt)} />
        <FactLine label="最近修改" value={formatWhen(bot()?.updatedAt)} />
      </View>
      <If when={botAccess.profilesError() !== null}>
        <StateNote
          tone="info"
          icon="info"
          text={`身份档案没有读到：${botAccess.profilesError()?.message ?? ""}。归属账号仍来自 Bot 自己；人设名称留空，不猜测。`}
        />
      </If>
      <If when={role()?.instruction}>
        <View style={{ flexDirection: "column", gap: space.xs, maxWidth: 720 }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>人设说明</Text>
          <Text style={{ fontSize: typeScale.base, lineHeight: Math.round(typeScale.base * lineHeight.normal), color: color.foreground }}>
            {role()?.instruction}
          </Text>
        </View>
      </If>
      <StateNote
        tone="info"
        icon="info"
        text="人设决定语气与做法，不授予任何能力。Bot 的能力只来自所属账号的范围与它自己的工作范围。"
      />
      <View style={{ flexDirection: "row", gap: space.sm, flexWrap: "wrap" }}>
        <Button
          testID="bot-rebind"
          variant="outline"
          label="改绑身份档案"
          accessibilityLabel="改绑身份档案"
          disabled={!canBind() || !canManage()}
          onPress={() => {
            botRebindAccount.set(bot()?.accountId ?? "");
            botRebindProfile.set("");
            botRebindOpen.set(true);
          }}
          style={touchStyle()}
        />
        <Button
          testID="bot-archive"
          variant="outline"
          icon="trash"
          label="归档 Bot"
          accessibilityLabel="归档 Bot"
          disabled={!canManage() || bot()?.archived === true}
          onPress={() => botArchiveOpen.set(true)}
          style={touchStyle()}
        />
      </View>
      <If when={!canBind()}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          改绑身份档案需要「绑定 Bot 密钥」能力；归档需要「管理 Bot」能力。
        </Text>
      </If>
    </View>
  );
}

// ---- working mode ---------------------------------------------------------

function BotWorkSection() {
  const bot = () => botAccess.botDetail();
  const draft = () => botAccess.botDraft();
  const canManage = () => can("bot.manage", { archived: bot()?.archived === true });
  effect(() => {
    if (!store.registryLoaded() && !store.modelsError()) void store.loadRegistry();
  });
  const modelItems = () => {
    const current = modelKeyOf(draft()?.defaultModel);
    const rows = store.models()
      .filter((model) => model.selectable !== false && model.executable !== false)
      .map((model) => ({
        value: `${model.providerId}/${model.modelId}`,
        title: `${displayNameOf(model.modelId) || model.modelId} · ${model.providerId}`,
        group: model.providerId,
        disabled: model.enabled === false,
      }));
    if (current && !rows.some((row) => row.value === current)) {
      rows.unshift({ value: current, title: `${modelLabel(draft()?.defaultModel)}（当前）`, group: "当前", disabled: false });
    }
    return rows;
  };
  return (
    <View testID="bot-work" style={{ flexDirection: "column", gap: space.lg, maxWidth: 760 }}>
      <View style={{ flexDirection: "column", gap: space.xs }}>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>模型类型</Text>
        <Select
          testID="bot-model-kind"
          size="field"
          items={MODEL_KIND_ITEMS}
          value={draft()?.modelKind ?? bot()?.modelKind ?? "text"}
          disabled={!canManage()}
          onChange={(value) => {
            if (value) botAccess.setBotField("modelKind", value);
          }}
          accessibilityLabel="模型类型"
          height={fieldHeightProp()}
        />
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          模型类型决定它能处理文字、图像、实时语音或做判断；改变后请确认默认模型仍然可用。
        </Text>
      </View>
      <View style={{ flexDirection: "column", gap: space.xs }}>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>默认模型</Text>
        <Select
          testID="bot-default-model"
          size="field"
          items={modelItems()}
          value={modelKeyOf(draft()?.defaultModel)}
          disabled={!canManage() || modelItems().length === 0}
          searchable
          searchPlaceholder="搜索模型"
          onChange={(value) => {
            const model = value ? modelOfKey(value) : undefined;
            if (model) botAccess.setBotDefaultModel(model);
          }}
          accessibilityLabel="默认模型"
          height={fieldHeightProp()}
        />
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          运行时可以由调用参数覆盖；每次会话会记录实际使用的模型。{" "}
          <If when={modelItems().length === 0}>模型目录还没有读到，先去「设置 → 模型接入」检查。</If>
        </Text>
      </View>
      <View style={{ flexDirection: "column", gap: space.xs }}>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>工作说明</Text>
        <Textarea
          testID="bot-instruction"
          value={draft()?.instruction ?? ""}
          onChange={(value) => botAccess.setBotField("instruction", value)}
          rows={6}
          maxLength={4000}
          disabled={!canManage()}
          placeholder="写它怎么做事、语气、边界；不授予任何能力。"
          accessibilityLabel="工作说明"
        />
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {draft()?.instruction.length ?? 0} / 4000 · 工作说明只影响语气与做法，不产生能力。
        </Text>
      </View>
      <View style={{ flexDirection: "column", gap: space.xs }}>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>工具</Text>
        <If
          when={(bot()?.harness.tools.length ?? 0) > 0}
          else={<Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>这个 Bot 没有固定工具白名单；它能用的工具由部署的注册表决定。</Text>}
        >
          <View style={{ flexDirection: "row", gap: space.xs, flexWrap: "wrap" }}>
            <Each of={bot()?.harness.tools ?? []} key={(tool) => tool}>
              {(tool) => <Badge variant="outline" label={tool()} />}
            </Each>
          </View>
        </If>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          工具目录还没有从服务端返回；这里只显示 Bot 自己保存的工具白名单，不猜测可用性。
        </Text>
      </View>
      <If when={!canManage()}>
        <StateNote tone="warning" icon="lock" text="你可以查看，但不能修改：需要这个 Bot 的「管理 Bot」能力。" />
      </If>
    </View>
  );
}

// ---- capabilities ---------------------------------------------------------

function BotCapabilitiesSection() {
  const bot = () => botAccess.botDetail();
  const rows = () => botAccess.botCapabilityRows(botShowAll(), botCapQuery());
  const allowed = () => rows().filter((row) => row.effective === "usable").length;
  const narrowed = () => rows().filter((row) => row.effective === "narrowed").length;
  const missing = () => rows().filter((row) => row.effective === "missing").length;
  const unknown = () => botAccess.unknownDraftKeys();
  const canManage = () => can("bot.manage", { archived: bot()?.archived === true });
  return (
    <View testID="bot-capabilities" style={{ flexDirection: "column", gap: space.md }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          已授 {allowed()} 项 · 被 Bot 授权收窄 {narrowed()} 项 · 账号未授予 {missing()} 项
        </Text>
        <View style={{ marginLeft: "auto", width: isMobile() ? "100%" : 240 }}>
          <Input
            testID="bot-capability-search"
            value={botCapQuery()}
            onChange={(value) => botCapQuery.set(value)}
            placeholder="为什么它不能…？输入能力名"
            accessibilityLabel="搜索能力"
            size="field"
            height={fieldHeightProp()}
            style={{ width: "100%" }}
          />
        </View>
      </View>
      <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
        Bot 只能做账号范围允许、且 Bot 授权允许的事。Bot 授权是允许名单：空名单表示它什么也不能做，不是继承账号。
      </Text>
      <If when={unknown().length > 0}>
        <StateNote
          tone="danger"
          icon="alert"
          text={`这个 Bot 的工作范围里有不在当前能力目录的 key：${unknown().join("、")}。服务端会拒绝登记；把对应行改为「收窄」移除它。`}
        />
      </If>
      <If when={!botAccess.accountScopeKnown(bot()?.accountId)}>
        <StateNote
          tone="info"
          icon="info"
          text="账号范围没有读到：缺「查看权限组」能力，或者部署还没有权限组。账号范围未知时，有效能力不显示为可用。"
        />
      </If>
      <If when={rows().length === 0}>
        <PageEmpty
          icon="search"
          title="没有匹配的能力"
          description="换个能力名或 key；清空搜索会回到当前清单。"
          primaryLabel="清空搜索"
          onPrimary={() => botCapQuery.set("")}
        />
      </If>
      <If when={rows().length > 0}>
        <If when={!isMobile()} else={<BotCapabilityRowsPhone rows={rows()} canManage={canManage()} />}>
          <View style={{ flexDirection: "column", maxWidth: 820 }}>
            <View
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: space.md,
                height: BOOK_FIELD_HEIGHT_PX,
                borderBottomWidth: 1,
                borderBottomColor: color.border,
              }}
            >
              <Text style={{ flex: 1, fontFamily: font.mono, fontSize: typeScale.key, letterSpacing: 0.6, color: color.mutedForeground }}>能力</Text>
              <Text style={{ width: 64, fontFamily: font.mono, fontSize: typeScale.key, letterSpacing: 0.6, color: color.mutedForeground }}>风险</Text>
              <Text style={{ width: 132, fontFamily: font.mono, fontSize: typeScale.key, letterSpacing: 0.6, color: color.mutedForeground }}>账号范围</Text>
              <Text style={{ width: 138, fontFamily: font.mono, fontSize: typeScale.key, letterSpacing: 0.6, color: color.mutedForeground }}>Bot 授权</Text>
              <Text style={{ width: 104, fontFamily: font.mono, fontSize: typeScale.key, letterSpacing: 0.6, color: color.mutedForeground }}>有效能力</Text>
            </View>
            <Each of={rows()} key={(row) => row.key}>
              {(row) => <BotCapabilityRow row={row()} canManage={canManage()} />}
            </Each>
          </View>
        </If>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap", maxWidth: 820 }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            {rows().length} 项 · 虚线 = 未授予
          </Text>
          <Button
            testID="bot-capability-all"
            variant="ghost"
            size="sm"
            label={botShowAll() ? "收起完整清单" : "展开完整清单"}
            accessibilityLabel={botShowAll() ? "收起完整清单" : "展开完整清单"}
            onPress={() => botShowAll.set(!botShowAll())}
            style={touchStyle()}
          />
        </View>
        <BotChanges />
        <If when={!canManage()}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            只读：需要这个 Bot 的「管理 Bot」能力才能修改它的授权。
          </Text>
        </If>
        <If when={canManage() && botAccess.unknownDraftKeys().length > 0}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            先处理未知 key，登记才会可用。
          </Text>
        </If>
      </If>
    </View>
  );
}

function effectiveBadge(row: BotOption) {
  if (row.effective === "usable") return <Badge variant="success" icon="check" label="可用" />;
  if (row.effective === "narrowed") return <Badge variant="outline" icon="lock" label="收窄" />;
  return <Badge variant="outline" icon="x" label="不可用" />;
}

function accountScopeText(row: BotOption): string {
  if (row.accountScope !== undefined) return scopeLabel(row.accountScope);
  return row.effective === "missing" ? "未授予" : "—";
}

function BotCapabilityRow(props: { row: BotOption; canManage: boolean }) {
  const row = () => props.row;
  const changed = () =>
    botAccess
      .botChanges()
      .some((change) => change.field === "permission" && change.key === row().key);
  return (
    <View
      testID={`bot-capability-${row().key}`}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.md,
        minHeight: 44,
        borderTopWidth: 1,
        borderTopColor: color.border,
        backgroundColor: changed() ? alpha(color.warning, 0.06) : undefined,
      }}
    >
      <View style={{ flex: 1, minWidth: 0 }}>
        <Text style={{ fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {row().label}
          {changed() ? <Text style={{ color: color.warning }}> · 未保存</Text> : null}
        </Text>
        <Text
          style={{
            fontFamily: font.mono,
            fontSize: typeScale.key,
            color: row().unknown ? color.danger : color.mutedForeground,
            numberOfLines: 1,
            textOverflow: "ellipsis",
          }}
        >
          {row().unknown ? `${row().key} · 不在能力目录` : row().key}
        </Text>
      </View>
      <View style={{ width: 64 }}>
        <RiskBadge risk={row().risk} />
      </View>
      <Text style={{ width: 132, fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
        {accountScopeText(row())}
      </Text>
      <View style={{ width: 138 }}>
        <If when={props.canManage} else={<Badge variant="outline" label={row().allowed ? "允许" : "收窄"} />}>
          <Select
            testID={`bot-harness-${row().key}`}
            size="sm"
            items={[
              { value: "allow", title: "允许" },
              { value: "narrow", title: "收窄" },
            ]}
            value={row().allowed ? "allow" : "narrow"}
            onChange={(value) => botAccess.setHarnessAllowed(row().key, value === "allow")}
            accessibilityLabel={`${row().label} 的 Bot 授权`}
          />
        </If>
      </View>
      <View style={{ width: 104 }}>{effectiveBadge(row())}</View>
    </View>
  );
}

function BotCapabilityRowsPhone(props: { rows: BotOption[]; canManage: boolean }) {
  return (
    <View style={{ flexDirection: "column", gap: 0 }}>
      <Each of={props.rows} key={(row) => row.key}>
        {(row) => (
          <View
            testID={`bot-capability-${row().key}`}
            style={{ flexDirection: "column", gap: space.sm, paddingVertical: space.sm, borderTopWidth: 1, borderTopColor: color.border }}
          >
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
              <View style={{ flex: 1, minWidth: 0 }}>
                <Text style={{ fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                  {row().label}
                </Text>
                <Text
                  style={{
                    fontFamily: font.mono,
                    fontSize: typeScale.key,
                    color: row().unknown ? color.danger : color.mutedForeground,
                    numberOfLines: 1,
                    textOverflow: "ellipsis",
                  }}
                >
                  {row().key}
                </Text>
              </View>
              <RiskBadge risk={row().risk} />
            </View>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                账号范围 <Text style={{ color: color.foreground }}>{accountScopeText(row())}</Text>
              </Text>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>有效能力</Text>
              {effectiveBadge(row())}
            </View>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
              <Text style={{ width: 64, fontSize: typeScale.sm, color: color.mutedForeground }}>Bot 授权</Text>
              <View style={{ flex: 1 }}>
                <If when={props.canManage} else={<Badge variant="outline" label={row().allowed ? "允许" : "收窄"} />}>
                  <Select
                    testID={`bot-harness-${row().key}`}
                    size="field"
                    items={[
                      { value: "allow", title: "允许" },
                      { value: "narrow", title: "收窄" },
                    ]}
                    value={row().allowed ? "allow" : "narrow"}
                    onChange={(value) => botAccess.setHarnessAllowed(row().key, value === "allow")}
                    accessibilityLabel={`${row().label} 的 Bot 授权`}
                    height={fieldHeightProp()}
                  />
                </If>
              </View>
            </View>
          </View>
        )}
      </Each>
    </View>
  );
}

function BotChanges() {
  const changes = () => botAccess.botChanges();
  return (
    <If when={changes().length > 0}>
      <View testID="bot-changes" style={{ flexDirection: "column", gap: space.sm, maxWidth: 820 }}>
        <BandHead num="—" title="本次变更" right={<Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>登记前可核对</Text>} />
        <Each of={changes()} key={(change) => `${change.field}-${change.key ?? ""}`}>
          {(change) => (
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, minHeight: 36, borderTopWidth: 1, borderTopColor: color.border }}>
              <View style={{ flex: 1, minWidth: 0 }}>
                <Text style={{ fontSize: typeScale.base, color: color.foreground }}>
                  {change().label}
                  {change().key ? <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}> {change().key}</Text> : null}
                </Text>
              </View>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                {change().from} → <Text style={{ color: color.foreground }}>{change().to}</Text>
              </Text>
              <If when={change().highRisk}>
                <Badge variant="destructive" label="高危" />
              </If>
            </View>
          )}
        </Each>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          影响下一次会话；正在运行的会话不会被改写。登记后会写入记录 05。
        </Text>
      </View>
    </If>
  );
}

function BotSignoff() {
  const changes = () => botAccess.botChanges();
  const dirty = () => changes().length > 0;
  const high = () => changes().filter((change) => change.highRisk);
  const me = () => store.identity();
  return (
    <View
      testID="bot-signoff"
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.md,
        paddingTop: space.md,
        borderTopWidth: 1,
        borderTopColor: color.border,
        flexWrap: "wrap",
      }}
    >
      <View style={{ flexDirection: "column", minWidth: 0 }}>
        <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>
          本次登记人：{me()?.displayName || me()?.username || "你的账号"}
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>登记后写入记录 05 · 影响下一次会话</Text>
      </View>
      <If when={dirty()}>
        <Badge variant="warning" label={`${changes().length} 项未保存`} />
      </If>
      <If when={high().length > 0}>
        <Badge variant="destructive" label={`${high().length} 项高危 · 需确认`} />
      </If>
      <View style={{ marginLeft: "auto", flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <Button
          testID="bot-discard"
          variant="ghost"
          size="sm"
          label="放弃"
          accessibilityLabel="放弃修改"
          disabled={!dirty()}
          onPress={() => botAccess.discardBotDraft()}
          style={touchStyle()}
        />
        <Button
          testID="bot-save"
          size="field"
          icon="check"
          label="检查并登记"
          accessibilityLabel="检查并登记"
          disabled={!dirty() || !can("bot.manage", { archived: false })}
          onPress={() => {
            botConfirmHigh.set({});
            botLedgerOpen.set(true);
          }}
          style={touchStyle()}
        />
      </View>
    </View>
  );
}

// ---- keys -----------------------------------------------------------------

function BotKeysSection(props: { botId: string }) {
  const bot = () => botAccess.botDetail();
  const manage = () => can("bot.manage", { archived: bot()?.archived === true });
  effect(() => {
    if (manage() && !botAccess.botKeysLoaded() && !botAccess.botKeysLoading()) void botAccess.loadBotKeys(props.botId);
  });
  const keys = () => botAccess.botKeys();
  const active = () => keys().filter((key) => !key.revokedAt);
  const revoked = () => keys().filter((key) => key.revokedAt);
  return (
    <View testID="bot-keys" style={{ flexDirection: "column", gap: space.md, maxWidth: 820 }}>
      <If
        when={manage()}
        else={
          <StateNote
            tone="warning"
            icon="lock"
            text="密钥需要这个 Bot 的「管理 Bot」能力才能读取与签发；「绑定 Bot 密钥」用于改绑身份档案。"
          />
        }
      >
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            {active().length} 把有效密钥 · {revoked().length} 把已吊销
          </Text>
          <Button
            testID="bot-key-issue"
            size={isMobile() ? "lg" : "sm"}
            icon="key"
            label="签发密钥"
            accessibilityLabel="签发密钥"
            disabled={bot()?.archived === true}
            onPress={() => {
              botIssueLabel.set("");
              botIssueOpen.set(true);
            }}
            style={{ marginLeft: "auto", ...touchStyle() }}
          />
        </View>
        <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
          密钥用于触发器或 Webhook 以这个 Bot 的身份行动。明文只显示一次，关闭后不能回读；吊销立即生效。
        </Text>
        <If when={botAccess.botKeysError() !== null}>
          <If when={botAccess.botKeysError()?.status === 403} else={<PageError
            title="没能读到密钥"
            message={botAccess.botKeysError()?.message ?? ""}
            requestId={botAccess.botKeysError()?.requestId}
            onRetry={() => void botAccess.loadBotKeys(props.botId)}
            retrying={botAccess.botKeysLoading()}
            testID="bot-keys-error"
          />}>
            <PagePermission
              capability={capabilityOf(botAccess.botKeysError()?.requiredKey ?? "bot.manage").displayName}
              capabilityKey={botAccess.botKeysError()?.requiredKey ?? "bot.manage"}
            />
          </If>
        </If>
        <If when={botAccess.botKeysLoading() && !botAccess.botKeysLoaded()}>
          <PageLoading rows={3} testID="bot-keys-loading" />
        </If>
        <If when={botAccess.botKeysLoaded() && botAccess.botKeysError() === null && keys().length === 0}>
          <PageEmpty
            icon="key"
            title="还没有密钥"
            description="触发器或 Webhook 需要一把密钥，才能以这个 Bot 的身份行动。"
            primaryLabel="签发密钥"
            onPrimary={() => {
              botIssueLabel.set("");
              botIssueOpen.set(true);
            }}
          />
        </If>
        <If when={keys().length > 0}>
          <View style={{ flexDirection: "column" }}>
            <Each of={keys()} key={(key) => key.keyId}>
              {(key) => <BotKeyRow keyView={key()} botId={props.botId} />}
            </Each>
          </View>
        </If>
      </If>
    </View>
  );
}

function BotKeyRow(props: { keyView: BotKeyView; botId: string }) {
  const revoked = () => !!props.keyView.revokedAt;
  return (
    <View
      testID={`bot-key-${props.keyView.keyId}`}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.md,
        minHeight: 52,
        borderTopWidth: 1,
        borderTopColor: color.border,
        opacity: revoked() ? 0.6 : 1,
      }}
    >
      <Icon name="key" size={14} tint={color.mutedForeground} />
      <View style={{ flex: 1, minWidth: 0 }}>
        <Text style={{ fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {props.keyView.label || "未命名密钥"}
        </Text>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {props.keyView.prefix}… · 最近使用 {formatWhen(props.keyView.lastUsedAt)}
        </Text>
      </View>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        创建 {formatWhen(props.keyView.createdAt)}
      </Text>
      <If when={revoked()} else={
        <Button
          testID={`bot-key-revoke-${props.keyView.keyId}`}
          variant="ghost"
          size="sm"
          label="吊销"
          accessibilityLabel={`吊销密钥 ${props.keyView.label}`}
          onPress={() => botRevoke.set({ keyId: props.keyView.keyId, label: props.keyView.label })}
          style={touchStyle()}
        />
      }>
        <Badge variant="secondary" label="已吊销" />
      </If>
    </View>
  );
}

// ---- sessions -------------------------------------------------------------

function BotSessionsSection() {
  return (
    <View testID="bot-sessions" style={{ flexDirection: "column", gap: space.md, maxWidth: 720 }}>
      <PageUnavailable
        icon="message"
        title="Bot 的最近会话将在下一版本上线"
        description="现在可以在工作区按 Bot 名称找到会话；等会话索引接口接入后，这里会列出这个 Bot 最近的会话、状态与用量。"
        testID="bot-sessions-unavailable"
      >
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, textAlign: "center" }}>
          技术详情：需要 GET /v1/sessions?botId={"{botId}"} 才能按 Bot 过滤会话。
        </Text>
      </PageUnavailable>
      <Button
        variant="outline"
        label="去工作区"
        accessibilityLabel="去工作区"
        onPress={() => store.openChapter("workspace")}
        style={touchStyle()}
      />
    </View>
  );
}

// ---- dialogs and sheets ---------------------------------------------------

export function BotDialogs() {
  return (
    <>
      <BotTrySheet />
      <BotLedgerSheet />
      <BotIssueDialog />
      <IssuedKeyDialog />
      <BotRevokeDialog />
      <BotArchiveDialog />
      <BotRebindDialog />
      <BotCreateDialog />
    </>
  );
}

function BotTrySheet() {
  const bot = () => botAccess.botDetail();
  const phase = () => botAccess.tryRunPhase();
  const close = () => {
    botTryOpen.set(false);
    botAccess.resetTryRun();
  };
  return (
    <Sheet
      testID="bot-try-sheet"
      open={botTryOpen()}
      side="right"
      title={bot() ? `试跑 ${bot()!.name}` : "试跑"}
      description="用一句话看看它会怎么做。试跑会创建一个真实会话。"
      onOpenChange={(event) => {
        if (!event.open) close();
      }}
      footer={
        <>
          <Button variant="outline" label="关闭" accessibilityLabel="关闭试跑" onPress={close} style={touchStyle()} />
          <Button
            testID="bot-try-run"
            label={phase() === "running" ? "正在开始…" : "开始试跑"}
            accessibilityLabel="开始试跑"
            loading={phase() === "running"}
            disabled={botTryText().trim() === "" || phase() === "running"}
            onPress={() => void botAccess.tryRun(bot()?.botId ?? "", botTryText())}
            style={touchStyle()}
          />
        </>
      }
    >
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
        <View style={{ flexDirection: "column", gap: space.md }}>
          <Textarea
            testID="bot-try-input"
            value={botTryText()}
            onChange={botTryText.set}
            rows={4}
            placeholder="说一句话，看看它会怎么做。"
            accessibilityLabel="试跑任务"
          />
          <View style={{ flexDirection: "row", gap: space.xs, flexWrap: "wrap" }}>
            <Button
              variant="ghost"
              size="sm"
              label="查订单物流"
              accessibilityLabel="填入查订单物流"
              onPress={() => botTryText.set("帮我查一下订单 88213 的物流状态，并拟一条给客户的回复。")}
            />
            <Button
              variant="ghost"
              size="sm"
              label="总结退款原因"
              accessibilityLabel="填入总结退款原因"
              onPress={() => botTryText.set("把本周退款原因按渠道拆开，找出异常升高的两类。")}
            />
          </View>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, lineHeight: Math.round(typeScale.sm * lineHeight.normal) }}>
            试跑通过 POST /v1/bots/{bot()?.botId || "{id}"}/invoke 开始一轮。页面只显示调用回执；回答在工作区里流式显示。
          </Text>
          <If when={phase() === "done" && botAccess.tryRunResult() !== null}>
            <StateNote tone="success" icon="check" text="这一轮已经创建了会话。" testID="bot-try-success">
              <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>
                会话 {botAccess.tryRunResult()?.sessionId}
                {botAccess.tryRunResult()?.deduped ? " · 已去重" : ""}
              </Text>
              <View style={{ flexDirection: "row", gap: space.sm, paddingTop: space.xs, flexWrap: "wrap" }}>
                <Button
                  testID="bot-try-open"
                  size="sm"
                  icon="external"
                  label="打开会话"
                  accessibilityLabel="打开试跑会话"
                  onPress={() => {
                    const sessionId = botAccess.tryRunResult()?.sessionId;
                    if (!sessionId) return;
                    close();
                    store.openChapter("workspace");
                    void store.openSession(sessionId);
                  }}
                />
                <Button
                  variant="outline"
                  size="sm"
                  label="再试一次"
                  accessibilityLabel="再试一次"
                  onPress={() => botAccess.resetTryRun()}
                />
              </View>
            </StateNote>
          </If>
          <If when={phase() === "error" && botAccess.tryRunError() !== null}>
            <StateNote tone="danger" icon="alert" text={botAccess.tryRunError() ?? ""} testID="bot-try-error" />
          </If>
        </View>
      </ScrollArea>
    </Sheet>
  );
}

function BotLedgerSheet() {
  const changes = () => botAccess.botChanges();
  const high = () => changes().filter((change) => change.highRisk);
  const allTicked = () => high().every((change) => botConfirmHigh()[change.key ?? ""] === true);
  const unknown = () => botAccess.unknownDraftKeys();
  const me = () => store.identity();
  const close = () => botLedgerOpen.set(false);
  const submit = async () => {
    const result = await botAccess.saveBotDraft();
    if (result.ok) {
      botLedgerOpen.set(false);
      botConfirmHigh.set({});
    }
  };
  return (
    <Sheet
      testID="bot-ledger-sheet"
      open={botLedgerOpen()}
      side={isMobile() ? "bottom" : "right"}
      style={isMobile() ? { height: 560, maxHeight: "88%" } : undefined}
      title={`确认 ${changes().length} 项变更`}
      description="登记前核对：这次变更会影响这个 Bot 的下一次会话。"
      onOpenChange={(event) => {
        if (!event.open) close();
      }}
      footer={
        <>
          <Button variant="outline" label="取消" accessibilityLabel="取消登记" onPress={close} style={touchStyle()} />
          <Button
            testID="bot-ledger-confirm"
            label={botAccess.botSaving() ? "登记中…" : "确认并登记"}
            accessibilityLabel="确认并登记"
            loading={botAccess.botSaving()}
            disabled={!allTicked() || unknown().length > 0 || botAccess.botSaving()}
            onPress={() => void submit()}
            style={touchStyle()}
          />
        </>
      }
    >
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
        <View style={{ flexDirection: "column", gap: space.md }}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
            <If when={high().length > 0} else={<Badge variant="outline" label="无高危" />}>
              <Badge variant="destructive" label={`${high().length} 项高危 · 需确认`} />
            </If>
            <If when={unknown().length > 0}>
              <Badge variant="destructive" label={`${unknown().length} 个未知 key`} />
            </If>
          </View>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            本次登记人：{me()?.displayName || me()?.username || "你的账号"}
          </Text>
          <Each of={changes()} key={(change) => `${change.field}-${change.key ?? ""}`}>
            {(change) => (
              <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, minHeight: 38, borderTopWidth: 1, borderTopColor: color.border }}>
                <View style={{ flex: 1, minWidth: 0 }}>
                  <Text style={{ fontSize: typeScale.base, color: color.foreground }}>{change().label}</Text>
                  <If when={change().key}>
                    <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>{change().key}</Text>
                  </If>
                </View>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  {change().from} → <Text style={{ color: color.foreground }}>{change().to}</Text>
                </Text>
                <If when={change().highRisk}>
                  <Badge variant="destructive" label="高危" />
                </If>
              </View>
            )}
          </Each>
          <StateNote tone="info" icon="info" text="影响下一次会话 · 写入记录 05。" />
          <If when={unknown().length > 0}>
            <StateNote
              tone="danger"
              icon="alert"
              text={`这些 key 不在当前能力目录里，服务端会拒绝登记：${unknown().join("、")}。先把对应行改为「收窄」移除。`}
            />
          </If>
          <Each of={high()} key={(change) => change.key ?? ""}>
            {(change) => (
              <Checkbox
                testID={`bot-high-risk-${change().key}`}
                checked={botConfirmHigh()[change().key ?? ""] === true}
                onChange={(checked) => botConfirmHigh.update((map) => ({ ...map, [change().key ?? ""]: checked }))}
                label={`我确认「${change().label}」的「${change().to}」（高危）`}
                style={isMobile() ? { minHeight: 44 } : undefined}
              />
            )}
          </Each>
          <If when={high().length > 0 && !allTicked()}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>勾选后才能登记。</Text>
          </If>
          <If when={botAccess.botSaveError() !== null}>
            <StateNote tone="danger" icon="alert" text={botAccess.botSaveError() ?? ""} />
          </If>
        </View>
      </ScrollArea>
    </Sheet>
  );
}

function BotIssueDialog() {
  return (
    <Dialog
      testID="bot-issue-dialog"
      open={botIssueOpen()}
      title="签发一把新密钥"
      description="明文只显示一次。它只能以这个 Bot 的身份行动。"
      onOpenChange={(event) => {
        if (!event.open) botIssueOpen.set(false);
      }}
      footer={
        <>
          <Button variant="outline" label="取消" accessibilityLabel="取消签发" onPress={() => botIssueOpen.set(false)} style={touchStyle()} />
          <Button
            testID="bot-issue-confirm"
            label={botAccess.keyBusy() === "issue" ? "生成中…" : "签发"}
            accessibilityLabel="签发密钥"
            loading={botAccess.keyBusy() === "issue"}
            onPress={() => {
              void botAccess.issueBotKey(botAccess.botDetail()?.botId ?? "", botIssueLabel()).then((ok) => {
                if (ok) botIssueOpen.set(false);
              });
            }}
            style={touchStyle()}
          />
        </>
      }
    >
      <View style={{ flexDirection: "column", gap: space.sm }}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>标签（例如：生产 Webhook）</Text>
        <Input
          testID="bot-issue-label"
          value={botIssueLabel()}
          onChange={botIssueLabel.set}
          placeholder="生产 Webhook"
          accessibilityLabel="密钥标签"
          size="field"
          height={fieldHeightProp()}
        />
        <If when={botAccess.botActionError() !== null}>
          <StateNote tone="danger" icon="alert" text={botAccess.botActionError() ?? ""} />
        </If>
      </View>
    </Dialog>
  );
}

function IssuedKeyDialog() {
  const issued = () => botAccess.issuedKey();
  const copied = () => botKeyCopied();
  return (
    <Dialog
      testID="bot-issued-dialog"
      open={issued() !== null}
      title="密钥已签发"
      description="复制它并保存好。关闭后不能回读。"
      onOpenChange={(event) => {
        if (!event.open) {
          botAccess.closeIssuedKey();
          botKeyCopied.set(false);
        }
      }}
      footer={
        <Button
          testID="bot-issued-done"
          label="我已保存"
          accessibilityLabel="我已保存密钥"
          onPress={() => {
            botAccess.closeIssuedKey();
            botKeyCopied.set(false);
          }}
          style={touchStyle()}
        />
      }
    >
      <View style={{ flexDirection: "column", gap: space.md }}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {issued()?.key.label || "未命名密钥"} · 前缀 {issued()?.key.prefix}
        </Text>
        <View
          style={{
            padding: space.md,
            borderRadius: radius.sm,
            borderWidth: 1,
            borderColor: color.fieldEdge,
            backgroundColor: color.field,
          }}
        >
          <Text testID="bot-issued-plaintext" style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.foreground }}>
            {issued()?.plaintext ?? ""}
          </Text>
        </View>
        <View style={{ flexDirection: "row", gap: space.sm, alignItems: "center" }}>
          <Button
            testID="bot-issued-copy"
            variant="outline"
            size="sm"
            icon="copy"
            label="复制密钥"
            accessibilityLabel="复制密钥"
            onPress={() => {
              void copyText(issued()?.plaintext ?? "").then((ok) => botKeyCopied.set(ok));
            }}
          />
          <If when={copied()}>
            <Text style={{ fontSize: typeScale.sm, color: color.success }}>已复制</Text>
          </If>
        </View>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{issued()?.notice}</Text>
      </View>
    </Dialog>
  );
}

function BotRevokeDialog() {
  const target = () => botRevoke();
  return (
    <AlertDialog
      testID="bot-revoke-dialog"
      open={target() !== null}
      title="吊销这把密钥？"
      description={`使用「${target()?.label || "未命名密钥"}」的触发器或 Webhook 会在下一次调用时立即失败。`}
      confirmLabel="吊销"
      cancelLabel="取消"
      destructive
      onConfirm={() => {
        const current = target();
        if (!current) return;
        void botAccess.revokeBotKey(botAccess.botDetail()?.botId ?? "", current.keyId).then((ok) => {
          if (ok) botRevoke.set(null);
        });
      }}
      onCancel={() => botRevoke.set(null)}
    />
  );
}

function BotArchiveDialog() {
  return (
    <AlertDialog
      testID="bot-archive-dialog"
      open={botArchiveOpen()}
      title="归档这个 Bot？"
      description="归档后它不能再新建会话或签发密钥；历史会话仍然可读。归档不能在这个页面撤销。"
      confirmLabel="归档"
      cancelLabel="取消"
      destructive
      onConfirm={() => {
        void botAccess.archiveBot(botAccess.botDetail()?.botId ?? "").then((ok) => {
          if (ok) botArchiveOpen.set(false);
        });
      }}
      onCancel={() => botArchiveOpen.set(false)}
    />
  );
}

function BotRebindDialog() {
  const accounts = () => identityAccess.accounts();
  const profiles = () => botAccess.profiles();
  const account = () => botRebindAccount();
  const accountProfiles = () => profiles().filter((profile) => profile.accountId === account());
  const labelOf = (profileId: string): string => {
    const profile = profiles().find((entry) => entry.profileId === profileId);
    const role = profile ? botAccess.roleOf(profile) : undefined;
    const owner = profile ? botAccess.accountNameOf(profile.accountId) : "";
    return [owner || "账号", role?.name || "人设未读取"].join(" · ");
  };
  effect(() => {
    if (!botRebindOpen()) return;
    if (accounts().length === 0 && can("account.read")) void identityAccess.loadAccounts();
    botAccess.ensureProfiles();
  });
  return (
    <Dialog
      testID="bot-rebind-dialog"
      open={botRebindOpen()}
      title="改绑身份档案"
      description="Bot 的归属账号由身份档案决定；改绑后它按新账号的范围执行。"
      onOpenChange={(event) => {
        if (!event.open) botRebindOpen.set(false);
      }}
      footer={
        <>
          <Button variant="outline" label="取消" accessibilityLabel="取消改绑" onPress={() => botRebindOpen.set(false)} style={touchStyle()} />
          <Button
            testID="bot-rebind-confirm"
            label="改绑"
            accessibilityLabel="确认改绑"
            disabled={botRebindProfile() === "" || !can("bot.bind")}
            onPress={() => {
              void botAccess.rebindProfile(botAccess.botDetail()?.botId ?? "", botRebindProfile()).then((ok) => {
                if (ok) botRebindOpen.set(false);
              });
            }}
            style={touchStyle()}
          />
        </>
      }
    >
      <View style={{ flexDirection: "column", gap: space.md }}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>归属账号</Text>
        <Select
          testID="bot-rebind-account"
          size="field"
          items={accounts().map((entry) => ({
            value: entry.accountId,
            title: `${entry.displayName || entry.username} · ${entry.kind === "bot" ? "Bot" : "人"}`,
          }))}
          value={account()}
          searchable
          onChange={(value) => {
            botRebindAccount.set(value ?? "");
            botRebindProfile.set("");
          }}
          accessibilityLabel="归属账号"
          height={fieldHeightProp()}
        />
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>身份档案（账号 + 人设）</Text>
        <Select
          testID="bot-rebind-profile"
          size="field"
          items={accountProfiles().map((profile) => ({ value: profile.profileId, title: labelOf(profile.profileId) }))}
          value={botRebindProfile()}
          disabled={accountProfiles().length === 0}
          onChange={(value) => botRebindProfile.set(value ?? "")}
          accessibilityLabel="身份档案"
          height={fieldHeightProp()}
        />
        <If when={accountProfiles().length === 0}>
          <StateNote
            tone="info"
            icon="info"
            text="这个账号还没有身份档案。请先让管理员为它建立身份档案（账号 + 人设），再回来改绑。"
          />
        </If>
        <If when={!can("bot.bind")}>
          <StateNote tone="warning" icon="lock" text="改绑需要「绑定 Bot 密钥」能力。" />
        </If>
        <If when={botAccess.botActionError() !== null}>
          <StateNote tone="danger" icon="alert" text={botAccess.botActionError() ?? ""} />
        </If>
      </View>
    </Dialog>
  );
}

function BotCreateDialog() {
  const accounts = () => identityAccess.accounts();
  const profiles = () => botAccess.profiles();
  effect(() => {
    if (!botCreateOpen()) return;
    if (accounts().length === 0 && can("account.read")) void identityAccess.loadAccounts();
    botAccess.ensureProfiles();
    if (!store.registryLoaded() && !store.modelsError()) void store.loadRegistry();
  });
  const modelItems = () =>
    store
      .models()
      .filter((model) => model.selectable !== false && model.executable !== false)
      .map((model) => ({
        value: `${model.providerId}/${model.modelId}`,
        title: `${displayNameOf(model.modelId) || model.modelId} · ${model.providerId}`,
        group: model.providerId,
      }));
  const profileLabel = (profileId: string): string => {
    const profile = profiles().find((entry) => entry.profileId === profileId);
    const role = profile ? botAccess.roleOf(profile) : undefined;
    const owner = profile ? botAccess.accountNameOf(profile.accountId) : "";
    return [owner || "账号", role?.name || "人设未读取"].join(" · ");
  };
  return (
    <Dialog
      testID="bot-create-dialog"
      open={botCreateOpen()}
      title="新建 Bot"
      description="给它一个身份、一个默认模型和一组工具。创建后可以在能力页调整授权。"
      size="lg"
      onOpenChange={(event) => {
        if (!event.open) botCreateOpen.set(false);
      }}
      footer={
        <>
          <Button variant="outline" label="取消" accessibilityLabel="取消新建" onPress={() => botCreateOpen.set(false)} style={touchStyle()} />
          <Button
            testID="bot-create-confirm"
            label="创建 Bot"
            accessibilityLabel="创建 Bot"
            disabled={
              botCreateName().trim() === "" ||
              botCreateProfile() === "" ||
              modelOfKey(botCreateModel()) === undefined
            }
            onPress={() => {
              const model = modelOfKey(botCreateModel());
              void botAccess
                .createBot({
                  name: botCreateName().trim(),
                  profileId: botCreateProfile(),
                  modelKind: botCreateKind(),
                  ...(model ? { defaultModel: model } : {}),
                })
                .then((created) => {
                  if (!created) return;
                  botCreateOpen.set(false);
                  store.openBot(created.botId);
                });
            }}
            style={touchStyle()}
          />
        </>
      }
    >
      <ScrollArea maxHeight={isMobile() ? 420 : 520} style={{ flexDirection: "column" }}>
        <View style={{ flexDirection: "column", gap: space.md }}>
          <Field label="名字">
            <Input
              testID="bot-create-name"
              value={botCreateName()}
              onChange={botCreateName.set}
              placeholder="客服一号"
              accessibilityLabel="Bot 名字"
              size="field"
              height={fieldHeightProp()}
            />
          </Field>
          <Field
            label="身份档案（账号 + 人设）"
            hint={profiles().length === 0 ? "还没有可用的身份档案。先让管理员为账号建立身份档案，再回来创建 Bot。" : undefined}
          >
            <Select
              testID="bot-create-profile"
              size="field"
              items={profiles().map((profile) => ({ value: profile.profileId, title: profileLabel(profile.profileId) }))}
              value={botCreateProfile()}
              searchable
              onChange={(value) => botCreateProfile.set(value ?? "")}
              accessibilityLabel="身份档案"
              height={fieldHeightProp()}
            />
          </Field>
          <Field label="模型类型">
            <Select
              testID="bot-create-kind"
              size="field"
              items={MODEL_KIND_ITEMS}
              value={botCreateKind()}
              onChange={(value) => botCreateKind.set(value ?? "text")}
              accessibilityLabel="模型类型"
              height={fieldHeightProp()}
            />
          </Field>
          <Field
            label="默认模型"
            hint={modelItems().length === 0 ? "模型目录还没有读到；先去「设置 → 模型接入」检查。" : undefined}
          >
            <Select
              testID="bot-create-model"
              size="field"
              items={modelItems()}
              value={botCreateModel()}
              searchable
              disabled={modelItems().length === 0}
              onChange={(value) => botCreateModel.set(value ?? "")}
              accessibilityLabel="默认模型"
              height={fieldHeightProp()}
            />
          </Field>
          <StateNote
            tone="info"
            icon="info"
            text="新建的 Bot 工作范围是空的：空名单表示它暂时什么也不能做。创建后到「能力」页逐项允许它需要的能力。"
          />
          <If when={botAccess.botActionError() !== null}>
            <StateNote tone="danger" icon="alert" text={botAccess.botActionError() ?? ""} />
          </If>
        </View>
      </ScrollArea>
    </Dialog>
  );
}
