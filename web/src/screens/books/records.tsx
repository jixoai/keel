// 记录 05 — change keel-bot-identity-access, A4.4a.
//
// The audit ledger's projection: one row per event, a filter bar (range, kind,
// result, free text over the actor/object), a detail panel and an export of
// exactly what is on screen. It is read-only: the ledger is the server's, this
// page never writes a second one.
//
// The read route (`GET /v1/audit/events`) has not landed; a deployment that
// answers 404/405/501 gets the 待接入 face, never a sample ledger. The
// development fixture serves the same read, so the whole surface (including
// its five states) is exercised on the fixture transport.
import { effect, signal, Text, View } from "nsh";
import {
  Badge,
  Button,
  EmptyContent,
  Input,
  ScrollArea,
  Select,
  Sheet,
  Table,
  TableBodyRow,
  TableHeaderRow,
  Td,
  Th,
  color,
  font,
  radius,
  space,
  typeScale,
  weight,
} from "../../ui/kit";
import { Icon } from "../../ui/icons";
import { store } from "../../data/store";
import { botAccess } from "../../data/bots";
import { canReadRecords, identityAccess } from "../../data/identity-access";
import { capabilityOf } from "../../data/permission-catalog";
import { Paper } from "./shell";
import { groupDisplayName, type AuditEventView, type AuditKind } from "../../data/identity";
import {
  FactLine,
  Hairline,
  PageEmpty,
  PageError,
  PageLoading,
  PagePermission,
  PageUnavailable,
  PressRow,
  SheetHead,
  StateNote,
  copyText,
  formatWhen,
  isMobile,
  isNarrow,
  recordsInline,
  touchStyle,
} from "./common";

const KIND_LABEL: Record<AuditKind, string> = {
  login: "登录",
  grant: "授权",
  deny: "拒绝",
  "super-admin": "超级管理员",
  relation: "关系",
  invoke: "调用",
  account: "账号",
  team: "团队",
  other: "其他",
};

const RESULT_LABEL: Record<AuditEventView["result"], string> = {
  ok: "成功",
  denied: "拒绝",
  error: "失败",
};

const resultBadge = (event: AuditEventView) =>
  event.result === "ok" ? (
    <Badge variant="success" label="成功" icon="check" style={{ alignSelf: "flex-start" }} />
  ) : event.result === "denied" ? (
    <Badge variant="destructive" label="拒绝" icon="lock" style={{ alignSelf: "flex-start" }} />
  ) : (
    <Badge variant="destructive" label="失败" icon="alert" style={{ alignSelf: "flex-start" }} />
  );

/** A super-admin action carries its own badge: the ledger's reader must see
 * that a deployment-level role acted, not just "an admin". */
const superBadge = (event: AuditEventView) =>
  event.kind === "super-admin" ? <Badge variant="info" label="超级管理员" accessibilityLabel="超级管理员操作" /> : null;

/** The missing key: the record's structured field first, then a `domain.key`
 * token in the sentence the server sent. */
const missingKeyOf = (event: AuditEventView): string | undefined => {
  if (event.capabilityKey) return event.capabilityKey;
  const match = (event.summary ?? "").match(/\b[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)+\b/);
  return match?.[0];
};

/** The human line: the server's sentence when it sent one; otherwise the kind's
 * own word (never an invented sentence about what happened). */
const eventText = (event: AuditEventView): string => event.summary ?? (event.action ? `${KIND_LABEL[event.kind]} · ${event.action}` : KIND_LABEL[event.kind]);

const eventActor = (event: AuditEventView): string =>
  event.actorName ? (event.actorKind === "bot" ? `${event.actorName}（Bot）` : event.actorName) : "—";

const eventObject = (event: AuditEventView): string =>
  event.objectName ? (event.objectType ? `${event.objectName}` : event.objectName) : "—";

const kindFilterItems = () => [
  { value: "all", title: "全部事件" },
  { value: "login", title: "登录" },
  { value: "grant", title: "授权" },
  { value: "deny", title: "拒绝" },
  { value: "super-admin", title: "超级管理员" },
  { value: "relation", title: "关系" },
  { value: "invoke", title: "调用" },
  { value: "account", title: "账号" },
  { value: "team", title: "团队" },
];

const kindLabel = (kind: string): string => (KIND_LABEL[kind as AuditKind] ?? kind);

const deniedCount = (): number => identityAccess.records().filter((event) => event.result === "denied").length;
const superAdminCount = (): number => identityAccess.records().filter((event) => event.kind === "super-admin").length;

const csvOf = (events: AuditEventView[]): string => {
  const escape = (value: string): string => `"${value.replace(/"/g, '""')}"`;
  const rows = [
    ["时间", "类型", "事件", "操作者", "对象", "结果", "请求编号"],
    ...events.map((event) => [
      event.occurredAt ?? "",
      kindLabel(event.kind),
      eventText(event),
      eventActor(event),
      eventObject(event),
      RESULT_LABEL[event.result],
      event.requestId ?? "",
    ]),
  ];
  return rows.map((row) => row.map(escape).join(",")).join("\n");
};

const exportNotice = signal<string | null>(null);
const mobileFilter = signal<"kind" | "result" | null>(null);

const kindTitleOf = (kind: string): string =>
  kindFilterItems().find((item) => item.value === kind)?.title ?? "全部事件";

const resultTitleOf = (result: string): string =>
  result === "ok" ? "成功" : result === "denied" ? "拒绝" : result === "error" ? "失败" : "全部结果";

function FilterStamp(props: { label: string; testID: string; onPress: () => void }) {
  return (
    <View
      testID={props.testID}
      role="button"
      focusable
      onPress={props.onPress}
      accessibilityLabel={`筛选：${props.label}`}
      style={{
        flex: 1,
        minWidth: 120,
        height: 44,
        paddingHorizontal: space.md,
        flexDirection: "row",
        alignItems: "center",
        justifyContent: "space-between",
        gap: space.xs,
        borderRadius: radius.sm,
        borderWidth: 1,
        borderColor: color.input,
        backgroundColor: color.field,
      }}
    >
      <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>{props.label}</Text>
      <Icon name="chevron-down" size={12} tint={color.mutedForeground} />
    </View>
  );
}

/** The phone's two filter choices as one bottom sheet (44px rows). */
function RecordsFilterSheet() {
  const open = () => mobileFilter();
  const kindItems = () => kindFilterItems();
  const resultItems = [
    { value: "all", title: "全部结果" },
    { value: "ok", title: "成功" },
    { value: "denied", title: "拒绝" },
    { value: "error", title: "失败" },
  ];
  const close = () => mobileFilter.set(null);
  return (
    <Sheet
      testID="records-filter-sheet"
      open={open() !== null}
      side="bottom"
      style={{ height: 420 }}
      title={open() === "kind" ? "按事件类型筛选" : "按结果筛选"}
      onOpenChange={(event) => {
        if (!event.open) close();
      }}
      footer={<Button variant="outline" label="完成" accessibilityLabel="完成筛选" onPress={close} style={touchStyle()} />}
    >
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
        <Each of={open() === "kind" ? kindItems() : resultItems} key={(item) => item.value}>
          {(item) => {
            const selected = () =>
              open() === "kind" ? identityAccess.recordsKind() === item().value : identityAccess.recordsResult() === item().value;
            return (
              <PressRow
                testID={`records-filter-${item().value}`}
                accessibilityLabel={item().title}
                selected={selected()}
                onPress={() => {
                  if (open() === "kind") identityAccess.setRecordsKind(item().value);
                  else identityAccess.setRecordsResult(item().value as "all" | "ok" | "denied" | "error");
                  close();
                }}
              >
                <Text style={{ fontSize: typeScale.base, color: color.foreground }}>{item().title}</Text>
                <If when={selected()}>
                  <Icon name="check" size={14} tint={color.primary} />
                </If>
              </PressRow>
            );
          }}
        </Each>
      </ScrollArea>
    </Sheet>
  );
}

/** The records chapter. */
export function RecordsChapter() {
  const records = () => identityAccess.visibleRecords();
  const selected = () => records().find((event) => event.eventId === store.routeObjectId()) ?? null;
  const allowed = () => canReadRecords();
  effect(() => {
    if (allowed()) identityAccess.ensureRecords();
  });
  return (
    <View style={{ flex: 1, minHeight: 0, flexDirection: "column" }}>
      <If when={allowed()} else={<Paper><View style={{ flex: 1, minHeight: 0, justifyContent: "center" }}><PagePermission capability="查看记录" capabilityKey="audit.read" /></View></Paper>}>
        <Paper>
        <ScrollArea testID="records-page-scroll" accessibilityLabel="记录" style={{ flex: 1, minHeight: 0 }}>
          <View style={{ flexDirection: "column", gap: space.lg }}>
            <SheetHead
              testID="records-head"
              kicker={{ icon: "activity", text: "05 / 记录" }}
              title="记录"
              badges={
                <If when={deniedCount() > 0}>
                  <Badge variant="destructive" label={`${deniedCount()} 条拒绝`} />
                </If>
              }
              description="登录、授权、拒绝、超级管理员、关系与调用在同一张账上；记录只有一个真源。"
              facts={[
                { label: "当前", value: `${records().length} 条事件` },
                { label: "", value: `${deniedCount()} 条拒绝 · ${superAdminCount()} 条超级管理员` },
              ]}
              actions={
                <Button
                  testID="records-export"
                  variant="outline"
                  size={isMobile() ? "default" : "sm"}
                  icon="download"
                  label="导出 CSV"
                  accessibilityLabel="导出 CSV"
                  disabled={records().length === 0}
                  onPress={() => {
                    void copyText(csvOf(records())).then((ok) => {
                      exportNotice.set(ok ? `已复制 ${records().length} 行 CSV 到剪贴板。` : "复制失败：这个环境没有可写的剪贴板。");
                    });
                  }}
                  style={touchStyle()}
                />
              }
            />
            <If when={exportNotice() !== null}>
              <StateNote
                tone={exportNotice()?.startsWith("已复制") ? "success" : "danger"}
                icon={exportNotice()?.startsWith("已复制") ? "check" : "alert"}
                text={exportNotice() ?? ""}
              />
            </If>

            <RecordsFilters />

            <If when={identityAccess.recordsUnavailable()}>
              <PageUnavailable
                testID="records-unavailable"
                icon="clock"
                title="记录接口尚未接入"
                description="这个部署还没有提供审计读取接口；页面按设计就位，接入后会按当前筛选显示真实记录。这里不显示示例数据。"
              >
                <EmptyContent>
                  <Button label="去身份" accessibilityLabel="去身份" onPress={() => store.openChapter("identity")} style={touchStyle()} />
                  <Button
                    variant="outline"
                    label="返回工作区"
                    accessibilityLabel="返回工作区"
                    onPress={() => store.openChapter("workspace")}
                    style={touchStyle()}
                  />
                </EmptyContent>
              </PageUnavailable>
            </If>
            <If when={!identityAccess.recordsUnavailable() && identityAccess.recordsLoading() && identityAccess.records().length === 0}>
              <PageLoading rows={7} testID="records-loading" />
            </If>
            <If when={!identityAccess.recordsUnavailable() && !identityAccess.recordsLoading() && identityAccess.recordsError() !== null}>
              <If when={identityAccess.recordsError()?.status === 403}>
                <PagePermission
                  capability="查看记录"
                  capabilityKey={identityAccess.recordsError()?.requiredKey ?? "audit.read"}
                />
              </If>
              <If when={identityAccess.recordsError()?.status !== 403}>
                <PageError
                  title="没能读到记录"
                  message={identityAccess.recordsError()?.message ?? ""}
                  onRetry={() => void identityAccess.loadRecords()}
                  retrying={identityAccess.recordsLoading()}
                  requestId={identityAccess.recordsError()?.requestId}
                  testID="records-error"
                />
              </If>
            </If>
            <If when={!identityAccess.recordsUnavailable() && !identityAccess.recordsLoading() && identityAccess.recordsError() === null && records().length === 0}>
              <PageEmpty
                testID="records-empty"
                icon="activity"
                title="还没有需要记录的操作"
                description="这台部署还没有产生需要记录的操作。有人登录、授权或调用之后，会出现在这里。"
                secondaryLabel="返回工作区"
                onSecondary={() => store.openChapter("workspace")}
              />
            </If>
            <If when={!identityAccess.recordsUnavailable() && !identityAccess.recordsLoading() && identityAccess.recordsError() === null && records().length > 0}>
              <If
                when={isNarrow()}
                else={
                  <If when={recordsInline()} else={<RecordsGrid selectedId={() => store.routeObjectId()} inline={false} />}>
                    <RecordsGrid selectedId={() => store.routeObjectId()} inline />
                  </If>
                }
              >
                <RecordsCards />
              </If>
            </If>
          </View>
        </ScrollArea>
        </Paper>
        <If when={!recordsInline()}>
          <RecordSheet event={() => selected()} />
        </If>
        <If when={isMobile()}>
          <RecordsFilterSheet />
        </If>
      </If>
    </View>
  );
}

function RecordsFilters() {
  const rangeItems = [
    { value: "today", title: "今天" },
    { value: "7d", title: "7 天" },
    { value: "30d", title: "30 天" },
  ];
  return (
    <View style={{ flexDirection: "column", gap: space.sm }}>
      <View style={{ flexDirection: "row", gap: space.sm, alignItems: "center", flexWrap: "wrap" }}>
        <View
          testID="records-range"
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
          <Each of={rangeItems} key={(item) => item.value}>
            {(item) => {
              const active = () => identityAccess.recordsRange() === item().value;
              return (
                <View
                  testID={`records-range-${item().value}`}
                  role="button"
                  focusable
                  onPress={() => identityAccess.setRecordsRange(item().value as "today" | "7d" | "30d")}
                  accessibilityLabel={`时间范围 ${item().title}`}
                  accessibilityState={{ selected: active() }}
                  style={{
                    minHeight: isMobile() ? 44 : 28,
                    paddingHorizontal: space.md,
                    alignItems: "center",
                    justifyContent: "center",
                    borderRadius: radius.xs,
                    backgroundColor: active() ? color.selected : "transparent",
                  }}
                >
                  <Text style={{ fontSize: typeScale.sm, color: active() ? color.foreground : color.mutedForeground }}>{item().title}</Text>
                </View>
              );
            }}
          </Each>
        </View>
        <If
          when={isMobile()}
          else={
            <>
              <Select
                testID="records-kind"
                size="sm"
                items={kindFilterItems()}
                value={identityAccess.recordsKind()}
                onChange={(value) => identityAccess.setRecordsKind(value ?? "all")}
                accessibilityLabel="按事件类型筛选"
                style={{ width: 130 }}
              />
              <Select
                testID="records-result"
                size="sm"
                items={[
                  { value: "all", title: "全部结果" },
                  { value: "ok", title: "成功" },
                  { value: "denied", title: "拒绝" },
                  { value: "error", title: "失败" },
                ]}
                value={identityAccess.recordsResult()}
                onChange={(value) => identityAccess.setRecordsResult((value as "all" | "ok" | "denied" | "error") ?? "all")}
                accessibilityLabel="按结果筛选"
                style={{ width: 120 }}
              />
            </>
          }
        >
          <FilterStamp
            label={kindTitleOf(identityAccess.recordsKind())}
            testID="records-kind-stamp"
            onPress={() => mobileFilter.set("kind")}
          />
          <FilterStamp
            label={resultTitleOf(identityAccess.recordsResult())}
            testID="records-result-stamp"
            onPress={() => mobileFilter.set("result")}
          />
        </If>
        <Input
          testID="records-search"
          value={identityAccess.recordsQuery()}
          onChange={(value) => identityAccess.recordsQuery.set(value)}
          placeholder="搜索操作者或对象"
          accessibilityLabel="搜索操作者或对象"
          size="sm"
          style={{ flex: 1, minWidth: 160, height: isMobile() ? 44 : undefined }}
        />
      </View>
      <Hairline />
    </View>
  );
}

function RecordsGrid(props: { selectedId: () => string | null; inline: boolean }) {
  const events = () => identityAccess.visibleRecords();
  // With a ledger on screen, the detail pane opens on the first (newest) row
  // instead of leaving 310px of empty paper; only a truly empty ledger has no
  // selection.
  const selected = () => events().find((event) => event.eventId === props.selectedId()) ?? events()[0] ?? null;
  return (
    <View style={{ flexDirection: "row", gap: space.lg, minHeight: 0 }}>
      <View style={{ flex: 1, minWidth: 0 }}>
        <Table testID="records-table" bare accessibilityLabel="记录列表">
          <TableHeaderRow>
            <Th width={110}>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>时间</Text>
            </Th>
            <Th>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>事件</Text>
            </Th>
            <Th width={120}>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>操作者</Text>
            </Th>
            <Th width={150}>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>对象</Text>
            </Th>
            <Th width={130}>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>结果</Text>
            </Th>
          </TableHeaderRow>
          <Each of={events()} key={(event) => event.eventId}>
            {(event) => (
              <TableBodyRow testID={`record-row-${event().eventId}`}>
                <Td>
                  <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.mutedForeground }}>
                    {formatWhen(event().occurredAt)}
                  </Text>
                </Td>
                <Td>
                  <PressRow
                    accessibilityLabel={`打开记录 ${eventText(event())}`}
                    onPress={() => store.openRecord(event().eventId)}
                    selected={props.selectedId() === event().eventId || (props.selectedId() === null && events()[0]?.eventId === event().eventId)}
                    style={{ borderBottomWidth: 0, minHeight: 44, marginHorizontal: -space.lg, paddingHorizontal: space.lg }}
                  >
                    <Text
                      style={{
                        flexShrink: 1,
                        minWidth: 0,
                        fontSize: typeScale.sm,
                        color: event().result === "denied" ? color.danger : color.foreground,
                        numberOfLines: 1,
                        textOverflow: "ellipsis",
                      }}
                    >
                      {eventText(event())}
                    </Text>
                  </PressRow>
                </Td>
                <Td>
                  <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                    {eventActor(event())}
                  </Text>
                </Td>
                <Td>
                  <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                    {eventObject(event())}
                  </Text>
                </Td>
                <Td>
                  <View style={{ flexDirection: "column", alignItems: "flex-start", gap: space.xxs }}>
                    {resultBadge(event())}
                    {superBadge(event())}
                  </View>
                </Td>
              </TableBodyRow>
            )}
          </Each>
        </Table>
        <View style={{ flexDirection: "row", alignItems: "center", paddingTop: space.sm, gap: space.md }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            显示 {events().length} 条 · 按时间倒序
          </Text>
          <If when={identityAccess.recordsHasMore()}>
            <Text style={{ marginLeft: "auto", fontSize: typeScale.sm, color: color.mutedForeground }}>还有更多；缩小时间范围或类型可以看全。</Text>
          </If>
        </View>
      </View>
      <If when={props.inline}>
        <View style={{ width: 330, flexShrink: 0, borderLeftWidth: 1, borderLeftColor: color.border, paddingLeft: space.lg }}>
          <If
            when={selected() !== null}
            else={
              <View style={{ flexDirection: "column", gap: space.sm, paddingVertical: space.lg }}>
                <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>没有匹配的记录</Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>清空搜索，或换一个时间范围与结果筛选。</Text>
              </View>
            }
          >
            <RecordDetail event={() => selected()!} />
          </If>
        </View>
      </If>
    </View>
  );
}

function RecordDetail(props: { event: () => AuditEventView; showActions?: boolean }) {
  const event = () => props.event();
  const missingKey = () => missingKeyOf(event());
  const capability = () => (missingKey() ? capabilityOf(missingKey()!).displayName : undefined);
  interface RecordLink {
    label: string;
    testID: string;
    icon: "external" | "play" | "user-plus";
    run: () => void;
  }
  const objectLinks = (): RecordLink[] => {
    const objectId = event().objectId;
    const links: RecordLink[] = [];
    if (objectId) {
      switch (event().objectType) {
        case "权限组":
          links.push({ label: "打开权限组", testID: "record-open-object", icon: "external", run: () => store.openIdentityObject("groups", objectId) });
          break;
        case "Bot":
          links.push({ label: "打开 Bot", testID: "record-open-object", icon: "external", run: () => store.openBot(objectId) });
          break;
        case "账号":
          links.push({ label: "打开账号", testID: "record-open-object", icon: "external", run: () => store.openIdentityObject("accounts", objectId) });
          break;
        case "团队":
          links.push({ label: "打开团队", testID: "record-open-object", icon: "external", run: () => store.openTeam(objectId) });
          break;
        case "会话":
          links.push({
            label: "打开会话",
            testID: "record-open-object",
            icon: "external",
            run: () => {
              store.openChapter("workspace");
              void store.openSession(objectId);
            },
          });
          break;
        default:
          break;
      }
    }
    if (event().teamId && event().runId) {
      const teamId = event().teamId!;
      const runId = event().runId!;
      links.push({ label: "打开运行", testID: "record-open-run", icon: "play", run: () => store.openTeamRun(teamId, runId) });
    }
    if (event().objectOwnerId && event().objectType !== "账号") {
      const ownerId = event().objectOwnerId!;
      links.push({
        label: "打开归属账号",
        testID: "record-open-owner",
        icon: "user-plus",
        run: () => store.openIdentityObject("accounts", ownerId),
      });
    }
    return links;
  };
  // The record may name the object without naming its owner. Resolve a Bot or
  // account already in the reader's index so the detail still answers 「找谁」.
  const ownerName = () => {
    if (event().objectOwnerName) return event().objectOwnerName;
    if (!event().objectId) return undefined;
    if (event().objectType === "Bot") {
      const bot = botAccess.bots().find((entry) => entry.botId === event().objectId);
      const account = identityAccess.accounts().find((entry) => entry.accountId === bot?.accountId);
      return account?.displayName || account?.username;
    }
    if (event().objectType === "账号") {
      const account = identityAccess.accounts().find((entry) => entry.accountId === event().objectId);
      return account?.displayName || account?.username;
    }
    if (event().objectType === "权限组") return "超级管理员";
    return undefined;
  };
  const grantable = () => (event().grantableBy ?? []).map(groupDisplayName);
  return (
    <View testID="record-detail" style={{ flexDirection: "column", gap: space.md }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <Text style={{ fontSize: typeScale.caption, fontWeight: weight.semibold, color: color.foreground, flexShrink: 1 }}>
          {eventText(event())}
        </Text>
        {resultBadge(event())}
        {superBadge(event())}
      </View>
      <Hairline />
      <View style={{ flexDirection: "column", gap: space.sm }}>
        <FactLine label="时间" value={formatWhen(event().occurredAt)} />
        <FactLine label="类型" value={kindLabel(event().kind)} />
        <FactLine label="操作者" value={eventActor(event())} />
        <FactLine label="对象" value={eventObject(event())} />
        <FactLine label="结果" value={RESULT_LABEL[event().result]} />
        <If when={!!event().requestId}>
          <FactLine label="请求编号" value={event().requestId ?? ""} mono />
        </If>
        <If when={!!event().source}>
          <FactLine label="来源" value={event().source ?? ""} mono />
        </If>
      </View>
      <If when={event().result === "denied"}>
        <StateNote testID="record-denied-reason" tone="danger" icon="lock" text="这次操作被拒。先看缺哪项能力，再找能授予它的人。">
          <View style={{ flexDirection: "column", gap: space.xs, paddingTop: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>
              缺少能力：{missingKey() ? (
                <>
                  {capability()}（<Text style={{ fontFamily: font.mono, fontSize: typeScale.key }}>{missingKey()}</Text>）
                </>
              ) : (
                "记录没有提供缺失的能力 key"
              )}
            </Text>
            <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>
              对象归属：{ownerName() || "记录没有提供归属账号"}
            </Text>
            <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>
              可授予组/人：{grantable().length ? grantable().join("、") : ownerName() ? `${ownerName()} 或超级管理员` : "找对象归属者的管理员"}
            </Text>
          </View>
        </StateNote>
      </If>
      <If when={props.showActions !== false}>
      <View style={{ flexDirection: "row", gap: space.sm, flexWrap: "wrap" }}>
        <Each of={objectLinks()} key={(link) => link.testID}>
          {(link) => (
            <Button
              testID={link().testID}
              variant="outline"
              size="sm"
              icon={link().icon}
              label={link().label}
              accessibilityLabel={link().label}
              onPress={() => link().run()}
              style={touchStyle()}
            />
          )}
        </Each>
        <If when={!!event().requestId}>
          <Button
            variant="outline"
            size="sm"
            icon="copy"
            label="复制编号"
            accessibilityLabel="复制请求编号"
            onPress={() => void copyText(event().requestId ?? "")}
            style={touchStyle()}
          />
        </If>
        <Button
          variant="link"
          size="sm"
          label="按操作者筛选"
          accessibilityLabel="按操作者筛选记录"
          onPress={() => identityAccess.recordsQuery.set(event().actorName ?? "")}
          style={isMobile() ? { minHeight: 44 } : undefined}
        />
      </View>
      </If>
    </View>
  );
}

function RecordsCards() {
  const events = () => identityAccess.visibleRecords();
  return (
    <View style={{ flexDirection: "column", gap: space.sm }}>
      <Each of={events()} key={(event) => event.eventId}>
        {(event) => (
          <PressRow
            testID={`record-card-${event().eventId}`}
            accessibilityLabel={`打开记录 ${eventText(event())}`}
            selected={store.routeObjectId() === event().eventId}
            onPress={() => store.openRecord(event().eventId)}
            style={{
              flexDirection: "column",
              alignItems: "stretch",
              gap: space.xs,
              padding: space.md,
              borderRadius: radius.md,
              borderWidth: 1,
              borderColor: color.border,
              minHeight: 64,
            }}
          >
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
              <Text
                style={{
                  flex: 1,
                  minWidth: 0,
                  fontSize: typeScale.sm,
                  color: event().result === "denied" ? color.danger : color.foreground,
                  numberOfLines: 1,
                  textOverflow: "ellipsis",
                }}
              >
                {eventText(event())}
              </Text>
              {superBadge(event())}
              {resultBadge(event())}
            </View>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
              {formatWhen(event().occurredAt)} · {eventActor(event())} · {eventObject(event())}
            </Text>
          </PressRow>
        )}
      </Each>
    </View>
  );
}

/** The phone's detail: a bottom sheet over the list (the row stays visible
 * behind it); closing clears the choice without changing the filters. */
function RecordSheet(props: { event: () => AuditEventView | null }) {
  return (
    <Sheet
      testID="record-sheet"
      open={props.event() !== null}
      side={isMobile() || isNarrow() ? "bottom" : "right"}
      style={isMobile() || isNarrow() ? { height: 460, maxHeight: "80%" } : undefined}
      title="记录详情"
      onOpenChange={(event) => {
        if (!event.open) store.openRecord(null);
      }}
      footer={
        <Button variant="outline" label="关闭" accessibilityLabel="关闭记录详情" onPress={() => store.openRecord(null)} style={touchStyle()} />
      }
    >
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
        <If when={props.event() !== null}>
          <RecordDetail event={() => props.event() as AuditEventView} />
        </If>
      </ScrollArea>
    </Sheet>
  );
}
