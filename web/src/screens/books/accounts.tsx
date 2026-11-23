// 04 身份 · 账号 — change keel-bot-identity-access, A4.4a.
//
// The index answers "who is here, what kind, in which groups, usable?" and the
// account page answers "what can this person do, and where does it come
// from?" — read-only, because authority is granted in a permission group (one
// concept, one entry point, design P2). Every action is offered only when the
// caller holds its key; the server refuses the write either way.
//
// What the API does not return is said, not guessed: the account listing has no
// "last activity" field, so the index derives it from the audit ledger when the
// reader may read records; without that read the column prints 「—」 once with a
// page note, never 「未记录」 on every row. An account whose group memberships
// could not be read shows "—", not "no groups".
import { effect, signal, Text, View } from "nsh";
import {
  AlertDialog,
  Avatar,
  Badge,
  Button,
  Dialog,
  DropdownMenu,
  DropdownMenuItem,
  Field,
  IconButton,
  Input,
  ScrollArea,
  Select,
  Table,
  TableBodyRow,
  TableHeaderRow,
  Td,
  Th,
  Tooltip,
  color,
  font,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
} from "../../ui/kit";
import { store } from "../../data/store";
import { can } from "../../data/permissions";
import {
  canGrantMembership,
  canManageAccounts,
  canReadGroups,
  canResetCredentials,
  identityAccess,
} from "../../data/identity-access";
import { CAPABILITIES, capabilityOf, groupCapabilities, scopeLabel } from "../../data/permission-catalog";
import {
  groupDisplayName,
  isSuperAdminGroup,
  type AccountView,
  type BotView,
  type PermissionGroupSummary,
  type PermissionScope,
} from "../../data/identity";
import {
  FactLine,
  Hairline,
  PageEmpty,
  PageError,
  PageLoading,
  PagePermission,
  PressRow,
  SectionLabel,
  SheetHead,
  StateNote,
  accountKindLabel,
  fieldHeightProp,
  formatDay,
  formatWhen,
  isMobile,
  touchStyle,
} from "./common";
import { BackToIndex } from "./shell";

type AccountSection = "overview" | "capabilities" | "groups" | "bots" | "security";

const ACCOUNT_SECTIONS: Array<{ id: AccountSection; num: string; label: string }> = [
  { id: "overview", num: "01", label: "概览" },
  { id: "capabilities", num: "02", label: "能力" },
  { id: "groups", num: "03", label: "权限组" },
  { id: "bots", num: "04", label: "Bot" },
  { id: "security", num: "05", label: "安全" },
];

const accountSection = signal<AccountSection>("overview");

/** A one-time temporary password a reader can hand over. 14 characters from an
 * unambiguous alphabet (no O/0/I/l), prefixed so the server's 12-byte floor is
 * met even with a short suffix. */
export const temporaryPassword = (): string => {
  const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789";
  const bytes = new Uint8Array(14);
  const cryptoAny = globalThis.crypto as { getRandomValues?: (array: Uint8Array) => Uint8Array } | undefined;
  if (cryptoAny?.getRandomValues) cryptoAny.getRandomValues(bytes);
  else for (let index = 0; index < bytes.length; index += 1) bytes[index] = Math.floor(Math.random() * 256);
  let body = "";
  for (const byte of bytes) body += alphabet[byte % alphabet.length];
  return `Keel-${body}`;
};

// ---- the index -------------------------------------------------------------

const visibleAccounts = (): AccountView[] => {
  const query = identityAccess.accountsQuery().trim().toLowerCase();
  const kind = identityAccess.accountsKind();
  const status = identityAccess.accountsStatus();
  return identityAccess.accounts().filter((row) => {
    if (kind !== "all" && row.kind !== kind) return false;
    if (status === "active" && row.disabledAt) return false;
    if (status === "disabled" && !row.disabledAt) return false;
    if (!query) return true;
    const groups = (row as { groups?: PermissionGroupSummary[] }).groups ?? [];
    return [row.displayName, row.username, ...groups.flatMap((group) => [group.name, groupDisplayName(group.name)])]
      .filter((part): part is string => typeof part === "string")
      .some((part) => part.toLowerCase().includes(query));
  });
};

const groupTextOf = (row: AccountView): string => {
  const groups = (row as { groups?: PermissionGroupSummary[] }).groups;
  if (groups === undefined) return "—";
  if (groups.length === 0) return "—";
  return groups.map((group) => (group.builtin ? `${groupDisplayName(group.name)}（内置组）` : groupDisplayName(group.name))).join(" · ");
};

/** The account the paper opens on when the route names none: the reader's own
 * account when the index has it, otherwise the first row. Only a genuinely
 * empty index has no default and shows the welcome. */
export const preferredAccountId = (): string | null => {
  const rows = identityAccess.accounts();
  const me = store.identity()?.accountId;
  return (me ? rows.find((row) => row.accountId === me)?.accountId : undefined) ?? rows[0]?.accountId ?? null;
};

/** The role words the account header carries: a super admin is one deployment
 * role, and each membership is a role badge of its own. */
const roleBadgesOf = (row: AccountView | undefined, detail: { groups: PermissionGroupSummary[] } | null, superAdmin: boolean) => {
  const groups = (row as { groups?: PermissionGroupSummary[] } | undefined)?.groups ?? detail?.groups ?? [];
  if (superAdmin) return groups.filter((group) => !isSuperAdminGroup(group.name));
  return groups;
};

const statusBadge = (row: AccountView) =>
  row.disabledAt ? (
    <Badge variant="destructive" label="已禁用" style={{ alignSelf: "flex-start" }} />
  ) : (
    <Badge variant="success" label="启用" icon="check" style={{ alignSelf: "flex-start" }} />
  );

/** One row in the 280 index column (or the phone's card). */
function AccountIndexRow(props: { row: AccountView; selected: boolean }) {
  return (
    <PressRow
      testID={`account-row-${props.row.accountId}`}
      accessibilityLabel={`打开账号 ${props.row.displayName}`}
      selected={props.selected}
      onPress={() => store.openIdentityObject("accounts", props.row.accountId)}
      style={{ borderBottomWidth: 0, minHeight: 52, borderRadius: radius.sm }}
    >
      <Avatar name={props.row.displayName} size="sm" />
      <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: 1 }}>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {props.row.displayName || props.row.username}
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {props.row.username} · {accountKindLabel(props.row.kind)}
          {props.row.disabledAt ? " · 已禁用" : ""}
        </Text>
      </View>
      <If when={props.row.isSuperAdmin}>
        <Badge variant="info" label="超级管理员" />
      </If>
    </PressRow>
  );
}

/** The identity chapter's left column: the segmented sub-page, one search, the
 * rows, and the one create action. ≥1440 only. */
export function AccountsIndex() {
  const all = () => identityAccess.accounts();
  const busy = () => identityAccess.accountsLoading() || identityAccess.accountsError() !== null;
  return (
    <View style={{ flex: 1, minHeight: 0, flexDirection: "column" }}>
      <View style={{ padding: space.lg, gap: space.md, borderBottomWidth: 1, borderBottomColor: color.border }}>
        <Input
          testID="accounts-search"
          value={identityAccess.accountsQuery()}
          onChange={(value) => identityAccess.accountsQuery.set(value)}
          placeholder="搜索名字、用户名或权限组"
          accessibilityLabel="搜索账号"
          size="field"
        />
      </View>
      <ScrollArea testID="accounts-index-scroll" accessibilityLabel="账号列表" style={{ flex: 1, minHeight: 0 }}>
        <View style={{ flexDirection: "column", padding: space.sm, gap: space.xxs }}>
          <If when={identityAccess.accountsLoading() && identityAccess.accounts().length === 0}>
            <PageLoading rows={6} testID="accounts-index-loading" />
          </If>
          <If when={!identityAccess.accountsLoading() && identityAccess.accountsError() !== null}>
            <If
              when={identityAccess.accountsError()?.status === 403}
              else={
                <PageError
                  title="没能读到账号列表"
                  message={identityAccess.accountsError()?.message ?? ""}
                  onRetry={() => void identityAccess.loadAccounts()}
                  retrying={identityAccess.accountsLoading()}
                  requestId={identityAccess.accountsError()?.requestId}
                />
              }
            >
              <PagePermission
                capability={capabilityOf(identityAccess.accountsError()?.requiredKey ?? "account.read").displayName}
                capabilityKey={identityAccess.accountsError()?.requiredKey ?? "account.read"}
              />
            </If>
          </If>
          <If when={!identityAccess.accountsLoading() && identityAccess.accountsError() === null && visibleAccounts().length === 0}>
            <If when={all().length === 0}>
              <View testID="accounts-index-empty" style={{ padding: space.lg }}>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  还没有账号。在主区新建第一个账号。
                </Text>
              </View>
            </If>
            <If when={all().length > 0}>
              <PageEmpty
                icon="users"
                title="没有匹配的账号"
                description="换个名字或用户名；也可以清空类型/状态筛选。"
              />
            </If>
          </If>
          <Each of={visibleAccounts()} key={(row) => row.accountId}>
            {(row) => (
              <AccountIndexRow
                row={row()}
                selected={(store.routeObjectId() ?? preferredAccountId()) === row().accountId}
              />
            )}
          </Each>
        </View>
      </ScrollArea>
      <View style={{ padding: space.lg, gap: space.sm, borderTopWidth: 1, borderTopColor: color.border }}>
        <If when={identityAccess.accountsHasMore()}>
          <Button
            testID="accounts-load-more-index"
            variant="ghost"
            size="sm"
            label="加载更多"
            accessibilityLabel="加载更多账号"
            onPress={() => void identityAccess.loadMoreAccounts()}
            style={{ alignSelf: "stretch" }}
          />
        </If>
        <If when={!canManageAccounts()}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>只读：缺「管理账号」能力。</Text>
        </If>
        <If when={canManageAccounts() && busy()}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>读取完成后再新建账号。</Text>
        </If>
        <If when={canManageAccounts() && !busy() && all().length > 0}>
          <Button
            testID="accounts-create"
            variant="outline"
            size="field"
            icon="plus"
            label="新建账号"
            accessibilityLabel="新建账号"
            onPress={() => createAccountDialog.set(true)}
            style={[{ alignSelf: "stretch" }, touchStyle()]}
          />
        </If>
      </View>
    </View>
  );
}

/** The account index as a page (1024–1439) — the full five-column table. */
export function AccountsPage() {
  const all = () => identityAccess.accounts();
  const people = () => all().filter((row) => row.kind === "human").length;
  const bots = () => all().filter((row) => row.kind === "bot").length;
  const services = () => all().filter((row) => row.kind === "service").length;
  const noData = () => (identityAccess.accountsLoading() || identityAccess.accountsError() !== null) && all().length === 0;
  const busy = () => identityAccess.accountsLoading() || identityAccess.accountsError() !== null;
  return (
    <ScrollArea testID="accounts-page-scroll" accessibilityLabel="账号" style={{ flex: 1, minHeight: 0 }}>
      <View style={{ flexDirection: "column", gap: space.lg }}>
        <SheetHead
          testID="accounts-head"
          kicker={{ icon: "shield", text: "04 / 身份 · 账号" }}
          title="账号"
          description="这台部署里有谁，他们能做什么，现在是否可用。"
          facts={[
            { label: "共", value: noData() ? "—" : `${all().length} 个账号` },
            {
              label: "构成",
              value: noData() ? "—" : `${people()} 人 · ${bots()} Bot${services() ? ` · ${services()} 服务` : ""}`,
            },
          ]}
          actions={
            canManageAccounts() && all().length > 0 && !busy() ? (
              <Button
                testID="accounts-create-page"
                size="field"
                icon="plus"
                label="新建账号"
                accessibilityLabel="新建账号"
                disabled={busy()}
                onPress={() => createAccountDialog.set(true)}
                style={touchStyle()}
              />
            ) : undefined
          }
        >
          <View style={{ flexDirection: "row", gap: space.sm, flexWrap: "wrap", alignItems: "center" }}>
            <Input
              testID="accounts-search-page"
              value={identityAccess.accountsQuery()}
              onChange={(value) => identityAccess.accountsQuery.set(value)}
              placeholder="搜索名字、用户名或权限组"
              accessibilityLabel="搜索账号"
              size="field"
              height={fieldHeightProp()}
              style={{ flex: 1, minWidth: 180 }}
            />
            <If
              when={isMobile()}
              else={
                <Select
                  testID="accounts-status-filter"
                  size="field"
                  items={[
                    { value: "all", title: "全部状态" },
                    { value: "active", title: "启用" },
                    { value: "disabled", title: "已禁用" },
                  ]}
                  value={identityAccess.accountsStatus()}
                  onChange={(value) =>
                    identityAccess.accountsStatus.set((value as "all" | "active" | "disabled") ?? "all")
                  }
                  accessibilityLabel="按状态筛选"
                  style={{ width: 120 }}
                />
              }
            >
              <View style={{ flexDirection: "row", gap: space.xs, width: "100%" }}>
                <Each
                  of={[
                    { value: "all", title: "全部" },
                    { value: "active", title: "启用" },
                    { value: "disabled", title: "已禁用" },
                  ]}
                  key={(item) => item.value}
                >
                  {(item) => {
                    const active = () => identityAccess.accountsStatus() === item().value;
                    return (
                      <View
                        testID={`accounts-status-${item().value}`}
                        role="button"
                        focusable
                        onPress={() => identityAccess.accountsStatus.set(item().value as "all" | "active" | "disabled")}
                        accessibilityLabel={item().title}
                        accessibilityState={{ selected: active() }}
                        style={{
                          flex: 1,
                          height: 44,
                          alignItems: "center",
                          justifyContent: "center",
                          borderRadius: radius.sm,
                          borderWidth: 1,
                          borderColor: active() ? color.border : color.input,
                          backgroundColor: active() ? color.selected : color.field,
                        }}
                      >
                        <Text style={{ fontSize: typeScale.sm, color: active() ? color.foreground : color.mutedForeground }}>
                          {item().title}
                        </Text>
                      </View>
                    );
                  }}
                </Each>
              </View>
            </If>
          </View>
        </SheetHead>

        <If when={identityAccess.accountsLoading() && all().length === 0}>
          <PageLoading rows={7} testID="accounts-loading" />
        </If>
        <If when={!identityAccess.accountsLoading() && identityAccess.accountsError() !== null}>
          <If when={identityAccess.accountsError()?.status === 403}>
            <PagePermission
              capability={capabilityOf(identityAccess.accountsError()?.requiredKey ?? "account.read").displayName}
              capabilityKey={identityAccess.accountsError()?.requiredKey ?? "account.read"}
            />
          </If>
          <If when={identityAccess.accountsError()?.status !== 403}>
            <PageError
              title="没能读到账号列表"
              message={identityAccess.accountsError()?.message ?? ""}
              onRetry={() => void identityAccess.loadAccounts()}
              retrying={identityAccess.accountsLoading()}
              requestId={identityAccess.accountsError()?.requestId}
              testID="accounts-error"
            />
          </If>
        </If>
        <If when={!identityAccess.accountsLoading() && identityAccess.accountsError() === null && all().length === 0}>
          <PageEmpty
            testID="accounts-empty"
            icon="users"
            title="还没有人加入这台部署"
            description="先建一个账号，再决定他能看到什么。也可以先建权限组，再往里加人。"
            primaryLabel={canManageAccounts() ? "新建账号" : undefined}
            onPrimary={canManageAccounts() ? () => createAccountDialog.set(true) : undefined}
            secondaryLabel={canReadGroups() ? "先建权限组" : undefined}
            onSecondary={canReadGroups() ? () => store.openIdentitySection("groups") : undefined}
          />
        </If>
        <If when={!identityAccess.accountsLoading() && identityAccess.accountsError() === null && all().length > 0}>
          <If when={isMobile()} else={<AccountsTable />}>
            <AccountsCards />
          </If>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, paddingTop: space.sm }}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              已显示 {visibleAccounts().length} / {all().length} 个账号
            </Text>
            <If when={identityAccess.accountsHasMore()}>
              <Button
                testID="accounts-load-more"
                variant="outline"
                size="sm"
                label="加载更多"
                accessibilityLabel="加载更多账号"
                onPress={() => void identityAccess.loadMoreAccounts()}
                style={[touchStyle(), { marginLeft: "auto" }]}
              />
            </If>
          </View>
          <If
            when={
              identityAccess.activityState() === "unavailable" ||
              identityAccess.activityState() === "forbidden" ||
              !can("audit.read")
            }
          >
            <Text
              testID="accounts-activity-note"
              style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}
            >
              {identityAccess.activityState() === "unavailable"
                ? "活动追踪待接入：账号接口没有最近活动字段，审计读取也未接通；这一列显示「—」，不显示推测值。"
                : "你没有「查看记录」能力，所以「最近活动」显示「—」；这是一条说明，不逐行重复。"}
            </Text>
          </If>
        </If>
      </View>
    </ScrollArea>
  );
}

function AccountsTable() {
  return (
    <Table testID="accounts-table" bare accessibilityLabel="账号列表">
      <TableHeaderRow testID="accounts-table-head">
        <Th>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>账号</Text>
        </Th>
        <Th width={80}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>类型</Text>
        </Th>
        <Th>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>权限组</Text>
        </Th>
        <Th width={110}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>状态</Text>
        </Th>
        <Th width={120}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>最近活动</Text>
        </Th>
      </TableHeaderRow>
      <Each of={visibleAccounts()} key={(row) => row.accountId}>
        {(row) => (
          <TableBodyRow testID={`account-row-${row().accountId}`}>
            <Td>
              <PressRow
                accessibilityLabel={`打开账号 ${row().displayName}`}
                onPress={() => store.openIdentityObject("accounts", row().accountId)}
                style={{ borderBottomWidth: 0, minHeight: 44, marginHorizontal: -space.lg, paddingHorizontal: space.lg }}
              >
                <Avatar name={row().displayName} size="sm" />
                <View style={{ flexDirection: "column", minWidth: 0 }}>
                  <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                    {row().displayName || row().username}
                  </Text>
                  <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                    {row().username}
                  </Text>
                </View>
              </PressRow>
            </Td>
            <Td>
              <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>{accountKindLabel(row().kind)}</Text>
            </Td>
            <Td>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                {groupTextOf(row())}
              </Text>
            </Td>
            <Td>{statusBadge(row())}</Td>
            <Td>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                {formatWhen(identityAccess.lastActivityOf(row())?.occurredAt)}
              </Text>
            </Td>
          </TableBodyRow>
        )}
      </Each>
    </Table>
  );
}

function AccountsCards() {
  return (
    <View style={{ flexDirection: "column", gap: space.sm }}>
      <Each of={visibleAccounts()} key={(row) => row.accountId}>
        {(row) => (
          <PressRow
            testID={`account-card-${row().accountId}`}
            accessibilityLabel={`打开账号 ${row().displayName}`}
            onPress={() => store.openIdentityObject("accounts", row().accountId)}
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
              <Avatar name={row().displayName} size="sm" />
              <View style={{ flex: 1, minWidth: 0 }}>
                <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                  {row().displayName || row().username}
                </Text>
                <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>
                  {row().username} · {accountKindLabel(row().kind)}
                </Text>
              </View>
              {statusBadge(row())}
            </View>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
              {groupTextOf(row())}
              {identityAccess.lastActivityOf(row())
                ? ` · 最近 ${formatWhen(identityAccess.lastActivityOf(row())?.occurredAt)}`
                : ""}
            </Text>
          </PressRow>
        )}
      </Each>
    </View>
  );
}

// ---- the account page -------------------------------------------------------

const effectiveOf = (accountId: string): Array<{ key: string; scope: PermissionScope; sources: string[] }> => {
  const detail = identityAccess.accountDetail();
  if (!detail || detail.account.accountId !== accountId) return [];
  // A deployment administrator is short-circuited to every key by the server;
  // showing the union of its groups would understate what it can do.
  if (detail.account.isSuperAdmin) {
    return CAPABILITIES.map((capability) => ({
      key: capability.key,
      scope: "all" as PermissionScope,
      sources: ["超级管理员"],
    }));
  }
  const map = new Map<string, { scope: PermissionScope; sources: string[] }>();
  for (const group of detail.groups) {
    const groupDetail = identityAccess.groupDetailOf(group.groupId);
    if (!groupDetail) continue;
    for (const grant of groupDetail.grants) {
      const previous = map.get(grant.key);
      if (!previous) {
        map.set(grant.key, { scope: grant.scope, sources: [groupDisplayName(group.name)] });
        continue;
      }
      if (scopeIndex(grant.scope) > scopeIndex(previous.scope)) previous.scope = grant.scope;
      const label = groupDisplayName(group.name);
      if (!previous.sources.includes(label)) previous.sources.push(label);
    }
  }
  return [...map.entries()].map(([key, value]) => ({ key, ...value }));
};

const scopeIndex = (scope: PermissionScope): number => ["own", "team", "tenant", "all"].indexOf(scope);

const ACTIVE_ACCOUNT = signal<string | null>(null);

/** The account page. It loads its detail and the account's Bots; the index's
 * row is patched from the same answer, so the two cannot disagree. */
export function AccountPage(props: { accountId: string; showBack: boolean }) {
  const id = () => props.accountId;
  effect(() => {
    const accountId = id();
    if (ACTIVE_ACCOUNT() === accountId) return;
    ACTIVE_ACCOUNT.set(accountId);
    accountSection.set("overview");
    void identityAccess.loadAccount(accountId);
    void identityAccess.loadAccountBots(accountId);
  });
  const detail = () => identityAccess.accountDetail();
  const row = () => identityAccess.accounts().find((entry) => entry.accountId === id());
  const name = () => detail()?.account.displayName || row()?.displayName || row()?.username || "账号";
  const disabled = () => !!detail()?.account.disabledAt;
  const superAdmin = () => detail()?.account.isSuperAdmin === true;
  const botAccount = () => (detail()?.account.kind ?? row()?.kind) === "bot";
  return (
    <ScrollArea testID="account-page-scroll" accessibilityLabel={`账号 ${name()}`} style={{ flex: 1, minHeight: 0 }}>
      <View style={{ flexDirection: "column", gap: space.lg }}>
        <If when={props.showBack}>
          <BackToIndex label="账号" onPress={() => store.closeIdentityObject()} />
        </If>
        <If when={identityAccess.accountDetailLoading() && detail() === null}>
          <PageLoading rows={5} testID="account-loading" />
        </If>
        <If when={identityAccess.accountDetailError() !== null}>
          <If when={identityAccess.accountDetailError()?.status === 403}>
            <PagePermission
              capability={capabilityOf(identityAccess.accountDetailError()?.requiredKey ?? "account.read").displayName}
              capabilityKey={identityAccess.accountDetailError()?.requiredKey ?? "account.read"}
            />
          </If>
          <If when={identityAccess.accountDetailError()?.status !== 403}>
            <PageError
              title="没能读到这个账号"
              message={identityAccess.accountDetailError()?.message ?? ""}
              onRetry={() => void identityAccess.loadAccount(id())}
              retrying={identityAccess.accountDetailLoading()}
              requestId={identityAccess.accountDetailError()?.requestId}
              testID="account-error"
            />
          </If>
        </If>
        <If when={detail() !== null && identityAccess.accountDetailError() === null}>
          <AccountHead accountId={id()} name={name()} disabled={disabled()} superAdmin={superAdmin()} botAccount={botAccount()} />
          <AccountSectionIndex />
          <Hairline />
          <View>
            <If when={accountSection() === "overview"}>
              <AccountOverview accountId={id()} />
            </If>
            <If when={accountSection() === "capabilities"}>
              <AccountCapabilities accountId={id()} />
            </If>
            <If when={accountSection() === "groups"}>
              <AccountGroups accountId={id()} />
            </If>
            <If when={accountSection() === "bots"}>
              <AccountBots accountId={id()} />
            </If>
            <If when={accountSection() === "security"}>
              <AccountSecurity accountId={id()} name={name()} disabled={disabled()} superAdmin={superAdmin()} botAccount={botAccount()} />
            </If>
          </View>
        </If>
      </View>
    </ScrollArea>
  );
}

function AccountHead(props: { accountId: string; name: string; disabled: boolean; superAdmin: boolean; botAccount: boolean }) {
  const detail = () => identityAccess.accountDetail();
  const row = () => identityAccess.accounts().find((entry) => entry.accountId === props.accountId);
  const groups = () => detail()?.groups ?? [];
  const roles = () => roleBadgesOf(row(), detail(), props.superAdmin);
  return (
    <SheetHead
      testID="account-head"
      kicker={{ icon: "shield", text: "04 / 身份 · 账号" }}
      leading={<Avatar name={props.name} size="lg" />}
      title={props.name}
      badges={
        <>
          <Badge variant="outline" label={accountKindLabel(detail()?.account.kind)} />
          <If when={props.superAdmin}>
            <Badge variant="info" label="超级管理员" />
          </If>
          <Each of={roles().slice(0, 2)} key={(group) => group.groupId}>
            {(group) => <Badge variant="secondary" label={groupDisplayName(group().name)} />}
          </Each>
          <If when={roles().length > 2}>
            <View tooltip={<Tooltip label={roles().map((group) => groupDisplayName(group.name)).join("、")} />}>
              <Badge variant="outline" label={`+${roles().length - 2}`} accessibilityLabel={`还有 ${roles().length - 2} 个角色`} />
            </View>
          </If>
          <If when={props.disabled} else={<Badge variant="success" label="启用" icon="check" />}>
            <Badge variant="destructive" label="已禁用" />
          </If>
        </>
      }
      description={`@${detail()?.account.username ?? "—"} · ${groups().length} 个权限组`}
      facts={[
        { label: "创建于", value: formatDay(detail()?.account.createdAt) },
        { label: "最近变更", value: formatWhen(detail()?.account.updatedAt) },
      ]}
      actions={
        <>
          <If when={props.superAdmin}>
            <Text testID="account-superadmin-identity-note" style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              身份由部署决定
            </Text>
          </If>
          <If when={!props.superAdmin && canGrantMembership()}>
            <Button
              testID="account-add-group-head"
              size="field"
              icon="user-plus"
              label="加入权限组"
              accessibilityLabel="加入权限组"
              onPress={() => addGroupDialog.set(true)}
              style={touchStyle()}
            />
          </If>
          <If when={!props.superAdmin && can("account.manage")}>
            <DropdownMenu
              testID="account-more"
              accessibilityLabel="更多账号操作"
              align="end"
              trigger={
                <IconButton
                  testID="account-more-trigger"
                  icon="dots"
                  label="更多账号操作"
                  style={isMobile() ? { width: 44, height: 44 } : undefined}
                />
              }
            >
              <DropdownMenuItem
                testID="account-disable-menu"
                label={props.disabled ? "启用账号" : "禁用账号"}
                icon={props.disabled ? "check" : "lock"}
                danger={!props.disabled}
                style={isMobile() ? { height: 44 } : undefined}
                onSelect={() => {
                  if (props.disabled) void identityAccess.setAccountDisabled(props.accountId, false);
                  else disableAccountRequest.set({ accountId: props.accountId, name: props.name });
                }}
              />
            </DropdownMenu>
          </If>
        </>
      }
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
        <Button
          testID="account-records-link-inline"
          variant="link"
          size="sm"
          label="记录 05"
          accessibilityLabel="查看记录"
          onPress={() => store.openChapter("records")}
          style={isMobile() ? { minHeight: 44 } : undefined}
        />
      </View>
    </SheetHead>
  );
}

function AccountSectionIndex() {
  return (
    <View
      testID="account-sections"
      style={{ flexDirection: "row", gap: space.xs, flexWrap: "wrap", alignItems: "center" }}
    >
      <Each of={ACCOUNT_SECTIONS} key={(section) => section.id}>
        {(section) => {
          const active = () => accountSection() === section().id;
          return (
            <Button
              testID={`account-section-${section().id}`}
              variant="ghost"
              size="sm"
              label={`${section().num} ${section().label}`}
              accessibilityLabel={`账号章节 ${section().label}`}
              selected={active()}
              onPress={() => accountSection.set(section().id)}
              style={[
                { paddingHorizontal: space.md },
                isMobile() ? { height: 44 } : null,
                active() ? { backgroundColor: color.selected } : null,
              ]}
            />
          );
        }}
      </Each>
    </View>
  );
}

function AccountOverview(props: { accountId: string }) {
  const detail = () => identityAccess.accountDetail();
  const groups = () => detail()?.groups ?? [];
  const bots = () => identityAccess.accountBots();
  const effective = () => effectiveOf(props.accountId);
  const superAdmin = () => detail()?.account.isSuperAdmin === true;
  const lastActivity = () => {
    const row = identityAccess.accounts().find((entry) => entry.accountId === props.accountId);
    return row ? identityAccess.lastActivityOf(row) : undefined;
  };
  return (
    <View style={{ flexDirection: "column", gap: space["2xl"] }}>
      <View testID="account-summary" style={{ flexDirection: "column", gap: space.xs }}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>账本摘要</Text>
        <View style={{ flexDirection: "row", alignItems: "baseline", gap: space.sm, flexWrap: "wrap" }}>
          <Text style={{ fontSize: typeScale.lg, fontWeight: weight.medium, color: color.foreground }}>
            {groups().length} 个权限组
          </Text>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>·</Text>
          <Text style={{ fontSize: typeScale.lg, fontWeight: weight.medium, color: color.foreground }}>
            {identityAccess.accountBotsLoaded() ? `${bots().length} 个 Bot` : "Bot 未读到"}
          </Text>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>·</Text>
          <Text style={{ fontSize: typeScale.lg, fontWeight: weight.medium, color: color.foreground }}>
            {superAdmin() ? `全部能力（${effective().length} 项）` : `${effective().length} 项有效能力`}
          </Text>
        </View>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {lastActivity() ? `最近活动 ${formatWhen(lastActivity()?.occurredAt)} · ${lastActivity()?.summary ?? "有记录"}` : "最近活动 —"}
        </Text>
      </View>
      <View style={{ flexDirection: "column", gap: space.md }}>
        <SectionLabel
          text="权限组"
          note="改权限请去「身份 → 权限组」"
          onNotePress={canReadGroups() ? () => store.openIdentitySection("groups") : undefined}
        />
        <If when={groups().length === 0}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            这个账号还没有加入任何权限组。加入一个组后，能力才会出现。
          </Text>
        </If>
        <View style={{ flexDirection: "row", gap: space.sm, flexWrap: "wrap" }}>
          <Each of={groups()} key={(group) => group.groupId}>
            {(group) => (
              <Badge
                variant={group().builtin ? "outline" : "secondary"}
                label={group().builtin ? `${groupDisplayName(group().name)} · 内置` : groupDisplayName(group().name)}
              />
            )}
          </Each>
        </View>
      </View>
      <View style={{ flexDirection: "column", gap: space.md }}>
        <SectionLabel
          text="拥有的 Bot"
          note="Bot 在「员工」章管理"
          onNotePress={() => store.openChapter("bots")}
        />
        <If when={identityAccess.accountBotsError() !== null}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            {identityAccess.accountBotsError()?.message}
          </Text>
        </If>
        <If when={identityAccess.accountBotsError() === null && bots().length === 0}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>这个账号还没有 Bot。</Text>
        </If>
        <Each of={bots().slice(0, 3)} key={(bot) => bot.botId}>
          {(bot) => <BotRow bot={bot()} />}
        </Each>
        <If when={bots().length > 3}>
          <Button
            variant="link"
            size="sm"
            label={`查看全部 ${bots().length} 个 Bot`}
            accessibilityLabel="打开员工章"
            onPress={() => store.openChapter("bots")}
            style={isMobile() ? { minHeight: 44 } : undefined}
          />
        </If>
      </View>
      <StateNote
        tone={detail()?.account.isSuperAdmin ? "info" : detail()?.account.disabledAt ? "danger" : "info"}
        icon={detail()?.account.isSuperAdmin ? "shield" : detail()?.account.disabledAt ? "lock" : "info"}
        text={
          detail()?.account.isSuperAdmin
            ? "超级管理员由部署级身份短路为全部能力；不可禁用或删除，找回密码只能在这台机器的终端进行。"
            : detail()?.account.disabledAt
              ? "已禁用：登录会话已吊销，Bot 密钥不可用。启用后对方才能再次登录。"
              : "账号可用。权限来自所属权限组的并集；直接授权只走 API/CLI，不在本页编辑。"
        }
      />
    </View>
  );
}

function BotRow(props: { bot: BotView }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, minHeight: 40, borderBottomWidth: 1, borderBottomColor: color.border }}>
      <Avatar name={props.bot.name} size={24} />
      <View style={{ flex: 1, minWidth: 0 }}>
        <Text style={{ fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>{props.bot.name}</Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {props.bot.modelKind || "模型类型未知"} · {props.bot.defaultModel.modelId || "默认模型未设置"}
        </Text>
      </View>
      <If when={props.bot.archived}>
        <Badge variant="outline" label="已归档" />
      </If>
    </View>
  );
}

function AccountCapabilities(props: { accountId: string }) {
  const effective = () => effectiveOf(props.accountId);
  const domains = () => groupCapabilities(effective().map((entry) => entry.key));
  const detail = () => identityAccess.accountDetail();
  const unread = () => (detail()?.groups ?? []).filter((group) => !identityAccess.groupDetailOf(group.groupId)).length;
  return (
    <View testID="account-capabilities" style={{ flexDirection: "column", gap: space.lg }}>
      <StateNote
        tone="info"
        icon="shield"
        text={
          detail()?.account.isSuperAdmin
            ? `超级管理员由服务端短路为全部能力（按目录 ${effective().length} 项列出），范围均为「全部」。`
            : "有效能力 = 这个账号所属权限组的并集（取最宽范围）。直接授权不在本页展示；本页只读。"
        }
      />
      <If when={unread() > 0}>
        <StateNote
          tone="warning"
          icon="alert"
          text={`有 ${unread()} 个权限组没有读到，下面可能不完整。`}
        />
      </If>
      <If when={effective().length === 0} else={<CapabilityDomains domains={domains} sources />}>
        <PageEmpty
          icon="shield"
          title="这个账号还没有能力"
          description="加入一个权限组，能力会按组出现在这里。"
          primaryLabel={detail()?.account.isSuperAdmin !== true && canGrantMembership() ? "加入权限组" : undefined}
          onPrimary={detail()?.account.isSuperAdmin !== true && canGrantMembership() ? () => addGroupDialog.set(true) : undefined}
        />
      </If>
    </View>
  );
}

function SourceCell(props: { sources: string[] }) {
  if (props.sources.length === 0) {
    return <Text style={{ width: 140, fontSize: typeScale.sm, color: color.mutedForeground }}>—</Text>;
  }
  const head = props.sources[0];
  const extra = props.sources.length - 1;
  return (
    <View
      style={{ width: 140, flexDirection: "row", alignItems: "center", gap: space.xxs, minWidth: 0 }}
      tooltip={<Tooltip label={props.sources.join("、")} />}
      accessibilityLabel={`来源 ${props.sources.join("、")}`}
    >
      <Text style={{ flexShrink: 1, minWidth: 0, fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
        {head}
      </Text>
      {extra > 0 ? (
        <Text testID={`source-more-${head}`} style={{ flexShrink: 0, fontSize: typeScale.sm, color: color.mutedForeground }}>
          +{extra}
        </Text>
      ) : null}
    </View>
  );
}

function CapabilityDomains(props: {
  domains: () => Array<{ id: string; label: string; icon: string; items: Array<{ key: string; displayName: string; risk: string }> }>;
  sources: boolean;
}) {
  const sourceOf = (key: string): string[] => {
    const found = effectiveOf(ACTIVE_ACCOUNT() ?? "").find((entry) => entry.key === key);
    return found ? found.sources : [];
  };
  const scopeOfKey = (key: string): PermissionScope | undefined =>
    effectiveOf(ACTIVE_ACCOUNT() ?? "").find((entry) => entry.key === key)?.scope;
  return (
    <View style={{ flexDirection: "column", gap: space.lg }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, paddingBottom: space.xs, borderBottomWidth: 1, borderBottomColor: color.border }}>
        <Text style={{ flex: 1, fontSize: typeScale.sm, color: color.mutedForeground }}>能力</Text>
        <Text style={{ width: 110, fontSize: typeScale.sm, color: color.mutedForeground }}>范围</Text>
        <Text style={{ width: 140, fontSize: typeScale.sm, color: color.mutedForeground }}>来源</Text>
      </View>
      <Each of={props.domains()} key={(domain) => domain.id}>
        {(domain) => (
          <View style={{ flexDirection: "column" }}>
            <View style={{ flexDirection: "row", alignItems: "baseline", gap: space.md, paddingBottom: space.xs }}>
              <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>{domain().label}</Text>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{domain().items.length} 项</Text>
            </View>
            <Each of={domain().items} key={(item) => item.key}>
              {(item) => (
                <View
                  style={{
                    flexDirection: "row",
                    alignItems: "center",
                    gap: space.md,
                    minHeight: 40,
                    borderTopWidth: 1,
                    borderTopColor: color.border,
                  }}
                >
                  <View style={{ flex: 1, minWidth: 0 }}>
                    <Text style={{ fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                      {item().displayName}
                    </Text>
                    <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                      {item().key}
                    </Text>
                  </View>
                  <Text style={{ width: 110, fontSize: typeScale.sm, color: color.foreground }}>{scopeLabel(scopeOfKey(item().key))}</Text>
                  {props.sources ? <SourceCell sources={sourceOf(item().key)} /> : <View style={{ width: 140 }} />}
                </View>
              )}
            </Each>
          </View>
        )}
      </Each>
    </View>
  );
}

function AccountGroups(props: { accountId: string }) {
  const detail = () => identityAccess.accountDetail();
  const groups = () => detail()?.groups ?? [];
  const superAdmin = () => detail()?.account.isSuperAdmin === true;
  return (
    <View testID="account-groups" style={{ flexDirection: "column", gap: space.lg }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
        <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground }}>
          所属权限组 {groups().length} 个
        </Text>
        <If
          when={!superAdmin() && canGrantMembership()}
          else={
            <Text style={{ marginLeft: "auto", fontSize: typeScale.sm, color: color.mutedForeground }}>
              {superAdmin() ? "身份由部署决定；超级管理员固定拥有全部能力。" : "只读：缺「授予账号权限」能力。"}
            </Text>
          }
        >
          <Button
            testID="account-add-group"
            size="sm"
            icon="user-plus"
            label="加入权限组"
            accessibilityLabel="加入权限组"
            onPress={() => addGroupDialog.set(true)}
            style={[touchStyle(), { marginLeft: "auto" }]}
          />
        </If>
      </View>
      <StateNote tone="info" icon="info" text="权限只做加法：账号的最终能力是所有组与直接授权的并集。移除一个组不会影响它从其他组得到的能力。" />
      <If when={groups().length === 0}>
        <PageEmpty
          icon="shield"
          title="还没有加入任何权限组"
          description="加入一个组，组里的能力会出现在这个账号的能力页。"
          primaryLabel={!superAdmin() && canGrantMembership() ? "加入权限组" : undefined}
          onPrimary={!superAdmin() && canGrantMembership() ? () => addGroupDialog.set(true) : undefined}
        />
      </If>
      <Each of={groups()} key={(group) => group.groupId}>
        {(group) => {
          const known = () => identityAccess.groupDetailOf(group().groupId);
          return (
            <View
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: space.md,
                minHeight: 44,
                borderTopWidth: 1,
                borderTopColor: color.border,
              }}
            >
              <View style={{ flex: 1, minWidth: 0 }}>
                <Text style={{ fontSize: typeScale.base, color: color.foreground }}>
                  {groupDisplayName(group().name)}
                  {group().builtin ? <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}> · 内置组</Text> : null}
                </Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  {known() ? `${known()!.members.length} 位成员 · ${known()!.grants.length} 项能力` : "成员与能力未读到"}
                </Text>
              </View>
              <Button
                variant="ghost"
                size="sm"
                label="查看"
                accessibilityLabel={`打开权限组 ${groupDisplayName(group().name)}`}
                onPress={() => store.openIdentityObject("groups", group().groupId)}
              />
              <If when={canGrantMembership() && !group().builtin}>
                <Button
                  variant="ghost"
                  size="sm"
                  label="移除"
                  accessibilityLabel={`移除 ${groupDisplayName(group().name)}`}
                  onPress={() => removeGroupRequest.set({ accountId: props.accountId, groupId: group().groupId, name: groupDisplayName(group().name) })}
                  style={touchStyle()}
                />
              </If>
            </View>
          );
        }}
      </Each>
      <If when={!canGrantMembership() && !superAdmin()}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          你可以查看，但不能修改成员：需要「授予账号权限」能力。
        </Text>
      </If>
    </View>
  );
}

function AccountBots(props: { accountId: string }) {
  const bots = () => identityAccess.accountBots();
  return (
    <View testID="account-bots" style={{ flexDirection: "column", gap: space.lg }}>
      <StateNote tone="info" icon="bot" text="这里只列这个账号拥有的 Bot；身份、工作方式与密钥在「员工」章管理。" />
      <If when={identityAccess.accountBotsError() !== null}>
        <PageError
          title="没能读到 Bot 列表"
          message={identityAccess.accountBotsError()?.message ?? ""}
          onRetry={() => void identityAccess.loadAccountBots(props.accountId)}
          requestId={identityAccess.accountBotsError()?.requestId}
        />
      </If>
      <If when={identityAccess.accountBotsError() === null && bots().length === 0}>
        <PageEmpty
          icon="bot"
          title="这个账号还没有 Bot"
          description="Bot 在「员工」章创建；创建后需要它的归属账号与工作范围都允许，才能行动。"
          secondaryLabel="去员工章"
          onSecondary={() => store.openChapter("bots")}
        />
      </If>
      <Each of={bots()} key={(bot) => bot.botId}>
        {(bot) => <BotRow bot={bot()} />}
      </Each>
    </View>
  );
}

function AccountSecurity(props: { accountId: string; name: string; disabled: boolean; superAdmin: boolean; botAccount: boolean }) {
  const error = () => identityAccess.accountActionError();
  const busy = () => identityAccess.accountActionBusy() === props.accountId;
  return (
    <View testID="account-security" style={{ flexDirection: "column", gap: space.lg }}>
      <View style={{ flexDirection: "column", gap: space.md }}>
        <SectionLabel text="状态" />
        <FactLine label="当前" value={props.disabled ? "已禁用" : "启用"} />
        <FactLine label="影响" value="禁用会吊销这个账号的全部会话；Bot 密钥随之不可用。" />
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
          <If when={props.superAdmin}>
            <Button
              testID="account-disable"
              variant="outline"
              label="禁用账号"
              accessibilityLabel="禁用账号"
              disabled
              style={touchStyle()}
            />
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>超级管理员不可禁用或删除。</Text>
          </If>
          <If
            when={!props.superAdmin && can("account.manage")}
            else={
              <If when={!props.superAdmin}>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>缺「管理账号」能力，不能改变状态。</Text>
              </If>
            }
          >
            <Button
              testID="account-disable"
              variant={props.disabled ? "outline" : "destructive"}
              label={props.disabled ? "启用账号" : "禁用账号"}
              accessibilityLabel={props.disabled ? "启用账号" : "禁用账号"}
              loading={busy()}
              onPress={() => {
                if (props.disabled) void identityAccess.setAccountDisabled(props.accountId, false);
                else disableAccountRequest.set({ accountId: props.accountId, name: props.name });
              }}
              style={touchStyle()}
            />
          </If>
        </View>
      </View>
      <Hairline />
      <View style={{ flexDirection: "column", gap: space.md }}>
        <SectionLabel text="密码" />
        <If when={props.superAdmin}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            超级管理员的密码只能在这台机器的终端重置（keel admin reset）；网页不提供入口。
          </Text>
        </If>
        <If when={!props.superAdmin && props.botAccount}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            Bot 账号用绑定 Bot 的密钥登录，不设置密码；密钥在 Bot 页签发与吊销。
          </Text>
        </If>
        <If
          when={!props.superAdmin && !props.botAccount && canResetCredentials()}
          else={
            <If when={!props.superAdmin && !props.botAccount}>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>缺「重置账号密码」能力，不能重置密码。</Text>
            </If>
          }
        >
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
            <Button
              testID="account-reset"
              variant="outline"
              label="重置密码"
              accessibilityLabel="重置密码"
              onPress={() => resetPasswordRequest.set({ accountId: props.accountId, name: props.name })}
              style={touchStyle()}
            />
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              生成一次临时密码；对方下次登录必须修改，其他会话全部失效。
            </Text>
          </View>
        </If>
      </View>
      <If when={error() !== null}>
        <StateNote tone="danger" icon="alert" text={error() ?? ""} />
      </If>
    </View>
  );
}

// ---- dialogs (module-level so a route change keeps them closed) -------------

const createAccountDialog = signal(false);
const addGroupDialog = signal(false);
const removeGroupRequest = signal<{ accountId: string; groupId: string; name: string } | null>(null);
const disableAccountRequest = signal<{ accountId: string; name: string } | null>(null);
const resetPasswordRequest = signal<{ accountId: string; name: string } | null>(null);

/** 新建账号: username, display name, kind, and — for a human — a generated
 * temporary password shown once. A Bot account takes no password (it signs in
 * with a Bot-bound key). */
export function CreateAccountDialog() {
  const username = signal("");
  const displayName = signal("");
  const kind = signal<"human" | "bot" | "service">("human");
  const password = signal("");
  const busy = signal(false);
  const error = signal<string | null>(null);
  const created = signal<{ name: string; password: string; bot: boolean; accountId: string } | null>(null);
  const open = () => createAccountDialog();
  const reset = () => {
    username.set("");
    displayName.set("");
    kind.set("human");
    password.set(temporaryPassword());
    busy.set(false);
    error.set(null);
    created.set(null);
  };
  const close = () => createAccountDialog.set(false);
  // Opening the dialog starts a clean form: a previous success's one-time
  // password must never reappear behind a new attempt.
  let wasOpen = false;
  effect(() => {
    const openNow = createAccountDialog();
    if (openNow && !wasOpen) reset();
    wasOpen = openNow;
  });
  const submit = async () => {
    if (busy()) return;
    error.set(null);
    if (!username().trim()) {
      error.set("请填写用户名。");
      return;
    }
    if (kind() === "human" && password().length < 12) {
      error.set("人账号的密码至少要 8 个字符。");
      return;
    }
    busy.set(true);
    const account = await identityAccess.createAccount({
      username: username().trim(),
      displayName: displayName().trim() || undefined,
      kind: kind(),
      ...(kind() === "human" ? { password: password() } : {}),
    });
    busy.set(false);
    if (!account) {
      error.set(identityAccess.accountActionError() ?? "创建失败。");
      return;
    }
    created.set({ name: account.displayName || account.username, password: password(), bot: account.kind === "bot", accountId: account.accountId });
  };
  return (
    <Dialog
      testID="account-create-dialog"
      open={open()}
      onOpenChange={(event) => {
        if (!event.open) close();
      }}
      title={created() ? "账号已创建" : "新建账号"}
      description={created() ? "把凭据通过安全渠道交给对方。" : "账号是权限主体；能力来自加入的权限组。"}
      footer={
        created() ? (
          <>
            <Button variant="outline" label="关闭" accessibilityLabel="关闭新建账号" onPress={close} style={touchStyle()} />
            <Button
              label="查看账号"
              accessibilityLabel="查看新建的账号"
              onPress={() => {
                const accountId = created()?.accountId ?? "";
                close();
                if (accountId) store.openIdentityObject("accounts", accountId);
              }}
              style={touchStyle()}
            />
          </>
        ) : (
          <>
            <Button variant="ghost" label="取消" accessibilityLabel="取消新建账号" onPress={close} style={touchStyle()} />
            <Button
              testID="account-create-submit"
              label={busy() ? "创建中…" : "创建账号"}
              accessibilityLabel="创建账号"
              loading={busy()}
              disabled={busy() || !username().trim()}
              onPress={() => void submit()}
              style={touchStyle()}
            />
          </>
        )
      }
    >
      <If when={created() !== null} else={<CreateAccountForm username={username} displayName={displayName} kind={kind} password={password} error={error} />}>
        <View style={{ flexDirection: "column", gap: space.md }}>
          <Text style={{ fontSize: typeScale.base, color: color.foreground }}>{created()?.name} 已创建。</Text>
          <If when={!!created()?.bot}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              Bot 账号用密钥登录；Bot 本身在「员工」章创建。
            </Text>
          </If>
          <If when={!created()?.bot}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>临时密码（只显示这一次）</Text>
            <View
              style={{
                padding: space.md,
                borderRadius: radius.md,
                borderWidth: 1,
                borderColor: color.border,
                backgroundColor: color.muted,
              }}
            >
              <Text style={{ fontFamily: font.mono, fontSize: typeScale.caption, color: color.foreground, userSelect: "text" }}>
                {created()?.password ?? ""}
              </Text>
            </View>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              需要更换时，由有「重置账号密码」能力的人在这个账号的安全页重置。
            </Text>
          </If>
        </View>
      </If>
    </Dialog>
  );
}

function CreateAccountForm(props: {
  username: { (): string; set: (value: string) => void };
  displayName: { (): string; set: (value: string) => void };
  kind: { (): "human" | "bot" | "service"; set: (value: "human" | "bot" | "service") => void };
  password: { (): string; set: (value: string) => void };
  error: { (): string | null };
}) {
  return (
    <View style={{ flexDirection: "column", gap: space.md }}>
      <Field label="用户名" required hint="登录名；创建后不改。">
        <Input
          testID="account-create-username"
          value={props.username()}
          onChange={(value) => props.username.set(value)}
          placeholder="例如 zhangsan"
          accessibilityLabel="用户名"
          size="field"
          height={fieldHeightProp()}
        />
      </Field>
      <Field label="显示名" hint="列表与记录里显示的名字。">
        <Input
          testID="account-create-name"
          value={props.displayName()}
          onChange={(value) => props.displayName.set(value)}
          placeholder="例如 张三"
          accessibilityLabel="显示名"
          size="field"
          height={fieldHeightProp()}
        />
      </Field>
      <Field label="类型" hint="人账号用密码登录；Bot/服务账号用密钥登录。">
        <Select
          testID="account-create-kind"
          size="field"
          items={[
            { value: "human", title: "人" },
            { value: "bot", title: "Bot" },
            { value: "service", title: "服务" },
          ]}
          value={props.kind()}
          onChange={(value) => props.kind.set((value as "human" | "bot" | "service") ?? "human")}
          accessibilityLabel="账号类型"
          height={fieldHeightProp()}
        />
      </Field>
      <If when={props.kind() === "human"}>
        <Field label="临时密码" hint="至少 8 个字符；创建后只显示一次。">
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
            <Input
              testID="account-create-password"
              value={props.password()}
              onChange={(value) => props.password.set(value)}
              accessibilityLabel="临时密码"
              size="field"
              height={fieldHeightProp()}
              style={{ flex: 1 }}
            />
            <Button
              variant="outline"
              size="field"
              label="换一个"
              accessibilityLabel="换一个临时密码"
              onPress={() => props.password.set(temporaryPassword())}
              style={touchStyle()}
            />
          </View>
        </Field>
      </If>
      <If when={props.error() !== null}>
        <StateNote tone="danger" icon="alert" text={props.error() ?? ""} />
      </If>
    </View>
  );
}

/** 加入权限组: every group the account is not in, with its member count. */
export function AddGroupDialog(props: { accountId: string }) {
  const query = signal("");
  const busy = signal<string | null>(null);
  const error = signal<string | null>(null);
  const joined = () => new Set((identityAccess.accountDetail()?.groups ?? []).map((group) => group.groupId));
  const rows = () =>
    identityAccess.groups().filter((group) => {
      if (joined().has(group.groupId)) return false;
      const text = query().trim().toLowerCase();
      return !text || group.name.toLowerCase().includes(text) || groupDisplayName(group.name).toLowerCase().includes(text);
    });
  const close = () => addGroupDialog.set(false);
  const add = async (groupId: string) => {
    busy.set(groupId);
    error.set(null);
    const ok = await identityAccess.addAccountGroup(props.accountId, groupId);
    busy.set(null);
    if (ok) close();
    else error.set(identityAccess.accountActionError() ?? "加入失败。");
  };
  return (
    <Dialog
      testID="account-add-group-dialog"
      open={addGroupDialog()}
      onOpenChange={(event) => {
        if (!event.open) close();
      }}
      title="加入权限组"
      description="加入后，这个组带来的能力会出现在账号的能力页。"
      footer={<Button variant="ghost" label="关闭" accessibilityLabel="关闭加入权限组" onPress={close} style={touchStyle()} />}
    >
      <Input
        testID="account-add-group-search"
        value={query()}
        onChange={(value) => query.set(value)}
        placeholder="搜索权限组"
        accessibilityLabel="搜索权限组"
        size="field"
        height={fieldHeightProp()}
      />
      <If when={rows().length === 0}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          没有可加入的组。这个账号已经在所有组里，或还没有更多组。
        </Text>
      </If>
      <ScrollArea maxHeight={320} style={{ flexDirection: "column" }}>
        <Each of={rows()} key={(group) => group.groupId}>
          {(group) => (
            <PressRow
              testID={`account-add-group-${group().groupId}`}
              accessibilityLabel={`加入 ${groupDisplayName(group().name)}`}
              onPress={() => void add(group().groupId)}
              disabled={busy() !== null}
            >
              <View style={{ flex: 1, minWidth: 0 }}>
                <Text style={{ fontSize: typeScale.base, color: color.foreground }}>
                  {groupDisplayName(group().name)}
                  {group().builtin ? <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}> · 内置组</Text> : null}
                </Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  {group().memberCount !== undefined ? `${group().memberCount} 位成员 · ${group().grantCount ?? 0} 项能力` : "成员与能力未读到"}
                </Text>
              </View>
              <If when={busy() === group().groupId} else={<Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>加入</Text>}>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>加入中…</Text>
              </If>
            </PressRow>
          )}
        </Each>
      </ScrollArea>
      <If when={error() !== null}>
        <StateNote tone="danger" icon="alert" text={error() ?? ""} />
      </If>
    </Dialog>
  );
}

/** The three confirmations the account page raises: disable, remove a group
 * membership, reset a password (with its one-time result). */
export function AccountConfirmations() {
  const disable = () => disableAccountRequest();
  const remove = () => removeGroupRequest();
  return (
    <>
      <AlertDialog
        testID="account-disable-dialog"
        open={disable() !== null}
        title="禁用账号？"
        description={`禁用 ${disable()?.name ?? ""} 会吊销该账号的全部会话；Bot 密钥随之不可用。可以随时重新启用。`}
        confirmLabel="禁用"
        cancelLabel="取消"
        destructive
        onConfirm={() => {
          const request = disable();
          disableAccountRequest.set(null);
          if (request) void identityAccess.setAccountDisabled(request.accountId, true, "在身份页禁用");
        }}
        onCancel={() => disableAccountRequest.set(null)}
      />
      <AlertDialog
        testID="account-remove-group-dialog"
        open={remove() !== null}
        title="移出这个权限组？"
        description={`移除后 ${remove()?.name ?? ""} 仍保留其他组与直接授权带来的能力。`}
        confirmLabel="移除"
        cancelLabel="取消"
        destructive
        onConfirm={() => {
          const request = remove();
          removeGroupRequest.set(null);
          if (request) void identityAccess.removeAccountGroup(request.accountId, request.groupId);
        }}
        onCancel={() => removeGroupRequest.set(null)}
      />
      <ResetPasswordDialog />
      <AddGroupDialog accountId={identityAccess.accountDetailId() ?? ""} />
    </>
  );
}

function ResetPasswordDialog() {
  const request = () => resetPasswordRequest();
  const password = signal("");
  const busy = signal(false);
  const done = signal(false);
  const error = signal<string | null>(null);
  let seededFor: string | null = null;
  // Seed the one-time value when a new target opens the dialog (never during
  // render: this is a write, and it belongs in an effect).
  effect(() => {
    const target = request();
    if (!target || target.accountId === seededFor) return;
    seededFor = target.accountId;
    password.set(temporaryPassword());
    done.set(false);
    error.set(null);
  });
  const close = () => {
    resetPasswordRequest.set(null);
    seededFor = null;
    done.set(false);
    error.set(null);
  };
  const submit = async () => {
    const target = request();
    if (!target || busy()) return;
    busy.set(true);
    error.set(null);
    const ok = await identityAccess.resetAccountPassword(target.accountId, password());
    busy.set(false);
    if (ok) done.set(true);
    else error.set(identityAccess.accountActionError() ?? "重置失败。");
  };
  return (
    <Dialog
      testID="account-reset-dialog"
      open={request() !== null}
      onOpenChange={(event) => {
        if (!event.open) close();
      }}
      title={done() ? "密码已重置" : `重置 ${request()?.name ?? ""} 的密码`}
      description={done() ? "其他会话已全部失效；对方下次登录必须修改密码。" : "生成一次临时密码，只显示这一次。"}
      footer={
        done() ? (
          <Button label="我已保存" accessibilityLabel="我已保存密码" onPress={close} style={touchStyle()} />
        ) : (
          <>
            <Button variant="ghost" label="取消" accessibilityLabel="取消重置密码" onPress={close} style={touchStyle()} />
            <Button
              testID="account-reset-submit"
              label={busy() ? "重置中…" : "确认重置"}
              accessibilityLabel="确认重置密码"
              loading={busy()}
              disabled={busy()}
              onPress={() => void submit()}
              style={touchStyle()}
            />
          </>
        )
      }
    >
      <If when={done()} else={
        <View style={{ flexDirection: "column", gap: space.md }}>
          <Field label="临时密码" hint="至少 8 个字符；可以换一个。">
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
              <Input
                testID="account-reset-password"
                value={password()}
                onChange={(value) => password.set(value)}
                accessibilityLabel="临时密码"
                size="field"
                height={fieldHeightProp()}
                style={{ flex: 1 }}
              />
              <Button
                variant="outline"
                size="field"
                label="换一个"
                accessibilityLabel="换一个临时密码"
                onPress={() => password.set(temporaryPassword())}
                style={touchStyle()}
              />
            </View>
          </Field>
          <If when={error() !== null}>
            <StateNote tone="danger" icon="alert" text={error() ?? ""} />
          </If>
        </View>
      }>
        <View style={{ flexDirection: "column", gap: space.md }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>临时密码（只显示这一次）</Text>
          <View
            style={{
              padding: space.md,
              borderRadius: radius.md,
              borderWidth: 1,
              borderColor: color.border,
              backgroundColor: color.muted,
            }}
          >
            <Text style={{ fontFamily: font.mono, fontSize: typeScale.caption, color: color.foreground, userSelect: "text" }}>
              {password()}
            </Text>
          </View>
        </View>
      </If>
    </Dialog>
  );
}

/** The account page's empty paper (wide layout, nothing chosen yet). */
export function AccountsWelcome() {
  const empty = () => identityAccess.accounts().length === 0 && !identityAccess.accountsLoading();
  return (
    <PageEmpty
      testID="accounts-welcome"
      icon="shield"
      title={empty() ? "还没有人加入这台部署" : "选择一个账号"}
      description={
        empty()
          ? "先建一个账号，再决定他能看到什么；也可以先建权限组，再往里加人。"
          : "左侧是这台部署的账号；点一行在右侧查看它的能力来源与安全操作。"
      }
      primaryLabel={canManageAccounts() ? "新建账号" : undefined}
      onPrimary={canManageAccounts() ? () => createAccountDialog.set(true) : undefined}
      secondaryLabel={empty() && canReadGroups() ? "先建权限组" : undefined}
      onSecondary={empty() && canReadGroups() ? () => store.openIdentitySection("groups") : undefined}
    />
  );
}
