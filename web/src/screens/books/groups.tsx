// 04 身份 · 权限组 — change keel-bot-identity-access, A4.4a.
//
// The matrix is the one authorization surface (design P2): every key is a row
// with its risk, its `domain.action` token and one scope stamp. A scope above
// the caller's own is disabled with its reason in words (not a tooltip), a
// built-in group is read-only with 另存为副本 as the way forward, and the save
// is a ledger: the diff, the signatory, the number of members affected, one
// explicit confirmation for every widened high-risk key, then 保存并签记 — and
// the server writes the record. Nothing here is a second truth: the draft is a
// comparison against the last server answer, and a save re-reads the group.
import { effect, signal, Text, View } from "nsh";
import {
  AlertDialog,
  Avatar,
  Badge,
  Button,
  Checkbox,
  Dialog,
  Field,
  Input,
  ScrollArea,
  Select,
  Sheet,
  Table,
  TableBodyRow,
  TableHeaderRow,
  Td,
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
import { Icon, type IconName } from "../../ui/icons";
import { store } from "../../data/store";
import {
  canGrantMembership,
  canManageGroups,
  identityAccess,
  ownScopeOf,
  type GroupChange,
} from "../../data/identity-access";
import {
  SCOPE_LABEL,
  allCapabilityDomains,
  capabilityOf,
  isKnownKey,
  scopeDisabledReason,
  scopeLabel,
  scopesOf,
} from "../../data/permission-catalog";
import type { PermissionGroupDetail, PermissionGroupSummary, PermissionScope } from "../../data/identity";
import {
  BandHead,
  Hairline,
  PageEmpty,
  PageError,
  PageLoading,
  PagePermission,
  PressRow,
  RiskBadge,
  SheetHead,
  StateNote,
  ScopeStamp,
  formatWhen,
  isMobile,
  touchStyle,
} from "./common";
import { BackToIndex } from "./shell";

// ---- the index -------------------------------------------------------------

const visibleGroups = (): PermissionGroupSummary[] => {
  const query = identityAccess.groupsQuery().trim().toLowerCase();
  const rows = identityAccess.groups();
  if (!query) return rows;
  return rows.filter((group) => group.name.toLowerCase().includes(query));
};

const groupCountsOf = (group: PermissionGroupSummary): string => {
  const row = identityAccess.groups().find((entry) => entry.groupId === group.groupId);
  if (!row || row.memberCount === undefined) return "成员与能力未读到";
  const unsaved = group.groupId === identityAccess.groupDetailId() ? identityAccess.groupChanges().length : 0;
  return `${row.grantCount ?? 0} 项 · ${row.memberCount} 人${unsaved ? ` · ${unsaved} 项未保存` : ""}`;
};

function GroupIndexRow(props: { group: PermissionGroupSummary; selected: boolean }) {
  return (
    <PressRow
      testID={`group-row-${props.group.groupId}`}
      accessibilityLabel={`打开权限组 ${props.group.name}`}
      selected={props.selected}
      onPress={() => store.openIdentityObject("groups", props.group.groupId)}
      style={{ borderBottomWidth: 0, minHeight: 52, borderRadius: radius.sm }}
    >
      <Avatar name={props.group.name} size="sm" />
      <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: 1 }}>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {props.group.name}
        </Text>
        <Text style={{ fontSize: typeScale.key, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {props.group.builtin ? "内置 · " : ""}
          {groupCountsOf(props.group)}
        </Text>
      </View>
      <If when={props.group.groupId === identityAccess.groupDetailId() && identityAccess.groupChanges().length > 0}>
        <Badge variant="warning" label="改" />
      </If>
    </PressRow>
  );
}

/** The identity chapter's permission-group index (≥1440). */
export function GroupsIndex() {
  return (
    <View style={{ flex: 1, minHeight: 0, flexDirection: "column" }}>
      <View style={{ padding: space.lg, gap: space.md, borderBottomWidth: 1, borderBottomColor: color.border }}>
        <Input
          testID="groups-search"
          value={identityAccess.groupsQuery()}
          onChange={(value) => identityAccess.groupsQuery.set(value)}
          placeholder="搜索权限组"
          accessibilityLabel="搜索权限组"
        />
      </View>
      <ScrollArea testID="groups-index-scroll" accessibilityLabel="权限组列表" style={{ flex: 1, minHeight: 0 }}>
        <View style={{ flexDirection: "column", padding: space.sm, gap: space.xxs }}>
          <If when={identityAccess.groupsLoading() && identityAccess.groups().length === 0}>
            <PageLoading rows={5} testID="groups-index-loading" />
          </If>
          <If when={!identityAccess.groupsLoading() && identityAccess.groupsError() !== null}>
            <If
              when={identityAccess.groupsError()?.status === 403}
              else={
                <PageError
                  title="没能读到权限组"
                  message={identityAccess.groupsError()?.message ?? ""}
                  onRetry={() => void identityAccess.loadGroups()}
                  retrying={identityAccess.groupsLoading()}
                />
              }
            >
              <PagePermission
                capability={capabilityOf(identityAccess.groupsError()?.requiredKey ?? "permission.read").displayName}
                capabilityKey={identityAccess.groupsError()?.requiredKey ?? "permission.read"}
              />
            </If>
          </If>
          <If when={!identityAccess.groupsLoading() && identityAccess.groupsError() === null && visibleGroups().length === 0}>
            <PageEmpty
              icon="shield"
              title={identityAccess.groups().length === 0 ? "还没有自定义组" : "没有匹配的权限组"}
              description={
                identityAccess.groups().length === 0
                  ? "内置组已经覆盖常见岗位。团队有自己的分工时，再建一个组。"
                  : "换个名字再试。"
              }
              primaryLabel={canManageGroups() ? "新建权限组" : undefined}
              onPrimary={canManageGroups() ? () => createGroupDialog.set(true) : undefined}
            />
          </If>
          <Each of={visibleGroups()} key={(group) => group.groupId}>
            {(group) => <GroupIndexRow group={group()} selected={store.routeObjectId() === group().groupId} />}
          </Each>
        </View>
      </ScrollArea>
      <View style={{ padding: space.lg, borderTopWidth: 1, borderTopColor: color.border }}>
        <If when={canManageGroups()} else={<Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>只读：缺「管理权限组」能力。</Text>}>
          <Button
            testID="groups-create"
            variant="outline"
            size="sm"
            icon="plus"
            label="新建权限组"
            accessibilityLabel="新建权限组"
            onPress={() => createGroupDialog.set(true)}
            style={{ alignSelf: "stretch" }}
          />
        </If>
      </View>
    </View>
  );
}

/** The permission-group index as a page (1024–1439). */
export function GroupsPage() {
  const all = () => identityAccess.groups();
  return (
    <ScrollArea testID="groups-page-scroll" accessibilityLabel="权限组" style={{ flex: 1, minHeight: 0 }}>
      <View style={{ flexDirection: "column", gap: space.lg }}>
        <SheetHead
          testID="groups-head"
          kicker={{ icon: "shield", text: "04 / 身份 · 权限组" }}
          title="权限组"
          description="谁能做什么，按组维护；内置组只读，可以另存为副本。"
          facts={[
            { label: "共", value: `${all().length} 个组` },
            { label: "", value: `${all().filter((group) => group.builtin).length} 个内置 · ${all().filter((group) => !group.builtin).length} 个自定义` },
          ]}
          actions={
            canManageGroups() ? (
              <Button
                testID="groups-create-page"
                size={isMobile() ? "default" : "sm"}
                icon="plus"
                label="新建权限组"
                accessibilityLabel="新建权限组"
                onPress={() => createGroupDialog.set(true)}
                style={touchStyle()}
              />
            ) : undefined
          }
        >
          <Input
            testID="groups-search-page"
            value={identityAccess.groupsQuery()}
            onChange={(value) => identityAccess.groupsQuery.set(value)}
            placeholder="搜索权限组"
            accessibilityLabel="搜索权限组"
            style={{ height: isMobile() ? 44 : undefined }}
          />
        </SheetHead>
        <If when={identityAccess.groupsLoading() && all().length === 0}>
          <PageLoading rows={6} testID="groups-loading" />
        </If>
        <If when={!identityAccess.groupsLoading() && identityAccess.groupsError() !== null}>
          <If when={identityAccess.groupsError()?.status === 403}>
            <PagePermission
              capability={capabilityOf(identityAccess.groupsError()?.requiredKey ?? "permission.read").displayName}
              capabilityKey={identityAccess.groupsError()?.requiredKey ?? "permission.read"}
            />
          </If>
          <If when={identityAccess.groupsError()?.status !== 403}>
            <PageError
              title="没能读到权限组"
              message={identityAccess.groupsError()?.message ?? ""}
              onRetry={() => void identityAccess.loadGroups()}
              retrying={identityAccess.groupsLoading()}
              testID="groups-error"
            />
          </If>
        </If>
        <If when={!identityAccess.groupsLoading() && identityAccess.groupsError() === null && all().length === 0}>
          <PageEmpty
            testID="groups-empty"
            icon="shield"
            title="还没有自定义组"
            description="内置组已经覆盖常见岗位。团队有自己的分工时，再建一个组。"
            primaryLabel={canManageGroups() ? "新建权限组" : undefined}
            onPrimary={canManageGroups() ? () => createGroupDialog.set(true) : undefined}
          />
        </If>
        <If when={!identityAccess.groupsLoading() && identityAccess.groupsError() === null && all().length > 0}>
          <If when={isMobile()} else={<GroupsTable />}>
            <GroupsCards />
          </If>
        </If>
      </View>
    </ScrollArea>
  );
}

function GroupsTable() {
  return (
    <Table testID="groups-table" bare accessibilityLabel="权限组列表">
      <TableHeaderRow>
        <Th>权限组</Th>
        <Th width={90}>类型</Th>
        <Th width={90}>成员</Th>
        <Th width={90}>能力</Th>
        <Th width={130}>最近修改</Th>
      </TableHeaderRow>
      <Each of={visibleGroups()} key={(group) => group.groupId}>
        {(group) => {
          const row = () => identityAccess.groups().find((entry) => entry.groupId === group().groupId);
          return (
            <TableBodyRow testID={`group-row-${group().groupId}`}>
              <Td>
                <PressRow
                  accessibilityLabel={`打开权限组 ${group().name}`}
                  onPress={() => store.openIdentityObject("groups", group().groupId)}
                  style={{ borderBottomWidth: 0, minHeight: 44, marginHorizontal: -space.lg, paddingHorizontal: space.lg }}
                >
                  <Avatar name={group().name} size="sm" />
                  <Text style={{ fontSize: typeScale.base, color: color.foreground }}>{group().name}</Text>
                </PressRow>
              </Td>
              <Td>
                <Badge variant={group().builtin ? "outline" : "secondary"} label={group().builtin ? "内置组" : "自定义"} />
              </Td>
              <Td>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  {row()?.memberCount !== undefined ? `${row()?.memberCount} 人` : "—"}
                </Text>
              </Td>
              <Td>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  {row()?.grantCount !== undefined ? `${row()?.grantCount} 项` : "—"}
                </Text>
              </Td>
              <Td>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{formatWhen(group().updatedAt)}</Text>
              </Td>
            </TableBodyRow>
          );
        }}
      </Each>
    </Table>
  );
}

function GroupsCards() {
  return (
    <View style={{ flexDirection: "column", gap: space.sm }}>
      <Each of={visibleGroups()} key={(group) => group.groupId}>
        {(group) => (
          <PressRow
            testID={`group-card-${group().groupId}`}
            accessibilityLabel={`打开权限组 ${group().name}`}
            onPress={() => store.openIdentityObject("groups", group().groupId)}
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
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
              <Avatar name={group().name} size="sm" />
              <Text style={{ flex: 1, fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground }}>{group().name}</Text>
              <Badge variant={group().builtin ? "outline" : "secondary"} label={group().builtin ? "内置组" : "自定义"} />
            </View>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{groupCountsOf(group())}</Text>
          </PressRow>
        )}
      </Each>
    </View>
  );
}

// ---- the group page ---------------------------------------------------------

const groupCopyDialog = signal(false);
const createGroupDialog = signal(false);
const addMemberDialog = signal(false);
const ledgerOpen = signal(false);
const scopeSheetKey = signal<string | null>(null);
const removeMemberRequest = signal<{ groupId: string; accountId: string; name: string } | null>(null);
const confirmHighRisk = signal<Record<string, boolean>>({});
const savedNotice = signal<string | null>(null);
const collapsedDomains = signal<Record<string, boolean>>({});

export function GroupPage(props: { groupId: string; showBack: boolean }) {
  const id = () => props.groupId;
  effect(() => {
    const groupId = id();
    if (identityAccess.groupDetailId() === groupId && identityAccess.groupDetail() !== null) return;
    void identityAccess.loadGroup(groupId);
    confirmHighRisk.set({});
    savedNotice.set(null);
  });
  const detail = () => identityAccess.groupDetail();
  const builtin = () => detail()?.group.builtin === true;
  const editable = () => canManageGroups() && !builtin();
  const name = () => detail()?.group.name ?? identityAccess.groups().find((row) => row.groupId === id())?.name ?? "权限组";
  return (
    <View style={{ flex: 1, minHeight: 0, flexDirection: "column" }}>
    <ScrollArea testID="group-page-scroll" accessibilityLabel={`权限组 ${name()}`} style={{ flex: 1, minHeight: 0 }}>
      <View style={{ flexDirection: "column", gap: space.lg }}>
        <If when={props.showBack}>
          <BackToIndex label="权限组" onPress={() => store.closeIdentityObject()} />
        </If>
        <If when={identityAccess.groupDetailLoading() && detail() === null}>
          <PageLoading rows={6} testID="group-loading" />
        </If>
        <If when={identityAccess.groupDetailError() !== null}>
          <If when={identityAccess.groupDetailError()?.status === 403}>
            <PagePermission
              capability={capabilityOf(identityAccess.groupDetailError()?.requiredKey ?? "permission.read").displayName}
              capabilityKey={identityAccess.groupDetailError()?.requiredKey ?? "permission.read"}
            />
          </If>
          <If when={identityAccess.groupDetailError()?.status !== 403}>
            <PageError
              title="没能读到这个权限组"
              message={identityAccess.groupDetailError()?.message ?? ""}
              onRetry={() => void identityAccess.loadGroup(id())}
              retrying={identityAccess.groupDetailLoading()}
              testID="group-error"
            />
          </If>
        </If>
        <If when={detail() !== null && identityAccess.groupDetailError() === null}>
          <GroupHead groupId={id()} editable={editable()} builtin={builtin()} />
          <If when={savedNotice() !== null}>
            <StateNote tone="success" icon="check" text={savedNotice() ?? ""} testID="group-saved-notice" />
          </If>
          <If when={identityAccess.groupSaveError() !== null}>
            <StateNote tone="danger" icon="alert" text={identityAccess.groupSaveError() ?? ""} testID="group-save-error" />
          </If>
          <If when={builtin()}>
            <StateNote
              tone="info"
              icon="lock"
              text="这是内置组，不能直接修改。可以「另存为副本」再调整；副本从这些能力开始，成员另加。"
            />
          </If>
          <If when={!builtin() && !canManageGroups()}>
            <StateNote tone="warning" icon="lock" text="你可以查看，但不能修改：缺「管理权限组」能力。" />
          </If>
          <GroupMatrix editable={editable()} builtin={builtin()} />
          <GroupChanges editable={editable()} />
          <GroupMembers groupId={id()} builtin={builtin()} />
        </If>
      </View>
    </ScrollArea>
    <If when={detail() !== null && identityAccess.groupDetailError() === null}>
      <View style={{ paddingTop: space.md }}>
        <GroupSignoff />
      </View>
    </If>
    </View>
  );
}

function GroupHead(props: { groupId: string; editable: boolean; builtin: boolean }) {
  const detail = () => identityAccess.groupDetail();
  const grants = () => detail()?.grants.length ?? 0;
  return (
    <SheetHead
      testID="group-head"
      kicker={{ icon: "shield", text: "04 / 身份 · 权限组" }}
      title={detail()?.group.name ?? "权限组"}
      badges={
        <>
          <Badge variant="outline" label={props.builtin ? "内置组" : "自定义"} />
          <Badge variant="secondary" label={`${detail()?.members.length ?? 0} 位成员`} />
          <Badge variant="secondary" label={`已授 ${grants()} 项`} />
        </>
      }
      description="权限只做加法：账号的最终能力是所有组与直接授权的并集。保存会写入 05 记录。"
      facts={[{ label: "最近修改", value: formatWhen(detail()?.group.updatedAt) }]}
      actions={
        <Button
          testID="group-copy"
          variant="outline"
          size="sm"
          label="另存为副本"
          accessibilityLabel="另存为副本"
          disabled={!canManageGroups()}
          onPress={() => groupCopyDialog.set(true)}
          style={touchStyle()}
        />
      }
    />
  );
}

interface MatrixDomain {
  id: string;
  label: string;
  icon: string;
  items: Array<{ key: string; displayName: string; risk: string }>;
}

/** The catalogue's domains, plus any stored key the catalogue does not know.
 * A grant is never hidden because this build has not heard of its key. */
const matrixDomains = (detail: PermissionGroupDetail | null): MatrixDomain[] => {
  const domains = allCapabilityDomains().map((domain) => ({
    id: domain.id,
    label: domain.label,
    icon: domain.icon,
    items: domain.items.map((item) => ({ key: item.key, displayName: item.displayName, risk: item.risk })),
  }));
  if (!detail) return domains;
  const seen = new Set(domains.flatMap((domain) => domain.items.map((item) => item.key)));
  for (const grant of detail.grants) {
    if (isKnownKey(grant.key) || seen.has(grant.key)) continue;
    seen.add(grant.key);
    const capability = capabilityOf(grant.key);
    const existing = domains.find((domain) => domain.id === capability.domain);
    const item = { key: grant.key, displayName: capability.displayName, risk: capability.risk };
    if (existing) existing.items.push(item);
    else domains.push({ id: capability.domain, label: capability.domain, icon: "layers", items: [item] });
  }
  return domains;
};

function GroupMatrix(props: { editable: boolean; builtin: boolean }) {
  const detail = () => identityAccess.groupDetail();
  const draft = () => identityAccess.groupDraft() ?? {};
  const query = () => identityAccess.groupSearch().trim().toLowerCase();
  const domains = () =>
    matrixDomains(detail()).filter((domain) => {
      if (!query()) return true;
      return (
        domain.label.toLowerCase().includes(query()) ||
        domain.items.some((item) => item.displayName.toLowerCase().includes(query()) || item.key.toLowerCase().includes(query()))
      );
    });
  const grantedOf = (domain: MatrixDomain): number =>
    domain.items.filter((item) => draft()[item.key] !== undefined).length;
  const isOpen = (domain: MatrixDomain): boolean => {
    if (query()) return true;
    const decided = collapsedDomains()[domain.id];
    if (decided !== undefined) return !decided;
    // A domain with grants starts open (the reader sees what is granted first);
    // an empty one folds away until they open it.
    return grantedOf(domain) > 0;
  };
  const toggle = (domain: MatrixDomain) => {
    collapsedDomains.update((map) => ({ ...map, [domain.id]: isOpen(domain) }));
  };
  return (
    <View testID="group-matrix" style={{ flexDirection: "column", gap: space.md }}>
      <BandHead
        num="04 / 02"
        title="能力"
        right={
          <Input
            testID="group-capability-search"
            value={identityAccess.groupSearch()}
            onChange={(value) => identityAccess.groupSearch.set(value)}
            placeholder="搜索能力或 key"
            accessibilityLabel="搜索能力"
            size="sm"
            style={{ width: isMobile() ? 150 : 220, height: isMobile() ? 44 : undefined }}
          />
        }
      />
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          已授 {Object.values(draft()).filter((scope) => scope !== undefined).length} 项
          {identityAccess.groupChanges().length ? ` · ${identityAccess.groupChanges().length} 项未保存` : ""}
        </Text>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>
          虚线 = 未授予
        </Text>
        <If when={!props.editable}>
          <Badge variant="outline" label="只读" />
        </If>
      </View>
      <If when={Object.values(draft()).filter((scope) => scope !== undefined).length === 0}>
        <StateNote
          tone="info"
          icon="info"
          text="这个组还没有任何能力。从下面的域开始勾选第一项；保存前会先给你看这次的变更。"
        />
      </If>
      <If when={!isMobile()}>
        <View
          style={{
            flexDirection: "row",
            gap: space.md,
            height: 28,
            alignItems: "center",
            borderBottomWidth: 1,
            borderBottomColor: color.border,
            maxWidth: 820,
          }}
        >
          <Text style={{ flex: 1, fontFamily: font.mono, fontSize: typeScale.key, letterSpacing: 0.6, color: color.mutedForeground }}>能力</Text>
          <Text style={{ width: 74, fontFamily: font.mono, fontSize: typeScale.key, letterSpacing: 0.6, color: color.mutedForeground }}>风险</Text>
          <Text style={{ width: 168, fontFamily: font.mono, fontSize: typeScale.key, letterSpacing: 0.6, color: color.mutedForeground }}>KEY</Text>
          <Text style={{ width: 150, fontFamily: font.mono, fontSize: typeScale.key, letterSpacing: 0.6, color: color.mutedForeground }}>范围</Text>
        </View>
      </If>
      <Each of={domains()} key={(domain) => domain.id}>
        {(domain) => (
          <View testID={`group-domain-${domain().id}`} style={{ flexDirection: "column", maxWidth: 820 }}>
            <PressRow
              accessibilityLabel={`${isOpen(domain()) ? "折叠" : "展开"} ${domain().label} 能力`}
              onPress={() => toggle(domain())}
              style={{ borderBottomWidth: 0, minHeight: 38, paddingHorizontal: 0 }}
            >
              <Icon name={domain().icon as IconName} size={14} tint={color.mutedForeground} />
              <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>{domain().label}</Text>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                已授 {grantedOf(domain())} / {domain().items.length}
              </Text>
              <View style={{ marginLeft: "auto" }}>
                <Icon name={isOpen(domain()) ? "chevron-down" : "chevron-right"} size={12} tint={color.mutedForeground} />
              </View>
            </PressRow>
            <If when={isOpen(domain())}>
              <Each of={domain().items} key={(item) => item.key}>
                {(item) => (
                  <CapabilityRow capability={item()} editable={props.editable} />
                )}
              </Each>
            </If>
            <Hairline />
          </View>
        )}
      </Each>
      <If when={domains().length === 0}>
        <PageEmpty
          icon="search"
          title="没有匹配的能力"
          description="换个能力名或 key；清空搜索会回到全部域。"
        />
      </If>
    </View>
  );
}

function CapabilityRow(props: {
  capability: { key: string; displayName: string; risk: string };
  editable: boolean;
}) {
  const draft = () => identityAccess.groupDraft() ?? {};
  const saved = () => identityAccess.groupSaved();
  const value = () => draft()[props.capability.key];
  const changed = () => saved()[props.capability.key] !== value();
  const held = () => ownScopeOf(props.capability.key);
  const allowed = () => scopesOf(props.capability.key);
  const ceiling = () => {
    const scopes = allowed();
    const own = held();
    if (own === undefined) return undefined;
    const ownIndex = ["own", "team", "tenant", "all"].indexOf(own);
    return scopes.filter((scope) => ["own", "team", "tenant", "all"].indexOf(scope) <= ownIndex).pop();
  };
  const limited = () => {
    const allowedScopes = allowed();
    const top = allowedScopes[allowedScopes.length - 1];
    if (held() === undefined) return true;
    return top !== undefined && top !== ceiling();
  };
  const items = () => [
    { value: "", title: "未授予" },
    ...allowed().map((scope) => {
      const reason = scopeDisabledReason(props.capability.key, scope, held());
      return {
        value: scope,
        title: reason ? `${SCOPE_LABEL[scope]} — ${reason}` : SCOPE_LABEL[scope],
        disabled: reason !== undefined && reason !== "",
      };
    }),
  ];
  // A key this build does not know is preserved as-is, never edited into one of
  // the catalogue's rows (the server would only refuse the write).
  const canEdit = () => props.editable && isKnownKey(props.capability.key);
  return (
    <View
      testID={`capability-row-${props.capability.key}`}
      style={{
        flexDirection: "column",
        gap: 2,
        borderTopWidth: 1,
        borderTopColor: color.border,
        paddingVertical: space.xs,
        backgroundColor: changed() ? alpha(color.warning, 0.06) : undefined,
      }}
    >
      <If when={!isMobile()} else={<CapabilityRowPhone capability={props.capability} canEdit={canEdit()} />}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, minHeight: 44 }}>
          <View style={{ flex: 1, minWidth: 0 }}>
            <Text style={{ fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
              {props.capability.displayName}
              {changed() ? <Text style={{ color: color.warning }}> · 未保存</Text> : null}
            </Text>
            <If when={!isKnownKey(props.capability.key)}>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>这个 key 不在当前能力目录里；按原样保留。</Text>
            </If>
          </View>
          <View style={{ width: 74 }}>
            <RiskBadge risk={capabilityOf(props.capability.key).risk} />
          </View>
          <Text
            style={{ width: 168, fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}
          >
            {props.capability.key}
          </Text>
          <View style={{ width: 150 }}>
            <If when={canEdit()} else={<ScopeStamp label={scopeLabel(value())} unset={value() === undefined} disabled />}>
              <Select
                testID={`capability-scope-${props.capability.key}`}
                size="lg"
                items={items()}
                value={value() ?? ""}
                onChange={(next) =>
                  identityAccess.setDraftScope(props.capability.key, next === null || next === "" ? undefined : (next as PermissionScope))
                }
                accessibilityLabel={`${props.capability.displayName} 的范围`}
              />
            </If>
          </View>
        </View>
      </If>
      <If when={limited()}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs, paddingLeft: 2 }}>
          <Icon name="lock" size={11} tint={color.mutedForeground} />
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            {held() === undefined ? "你没有这项能力，不能授予它。" : `你最多只能授予「${scopeLabel(ceiling())}」。`}
          </Text>
        </View>
      </If>
    </View>
  );
}

/** The phone row: name + key + risk, then the 44px scope control (a stamp that
 * opens the bottom sheet; a wide table would scroll sideways here). */
function CapabilityRowPhone(props: {
  capability: { key: string; displayName: string; risk: string };
  canEdit: boolean;
}) {
  const value = () => (identityAccess.groupDraft() ?? {})[props.capability.key];
  return (
    <View style={{ flexDirection: "column", gap: space.sm, paddingVertical: space.xs }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <View style={{ flex: 1, minWidth: 0 }}>
          <Text style={{ fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
            {props.capability.displayName}
          </Text>
          <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
            {props.capability.key}
          </Text>
        </View>
        <RiskBadge risk={capabilityOf(props.capability.key).risk} />
      </View>
      <If when={props.canEdit} else={<ScopeStamp label={scopeLabel(value())} unset={value() === undefined} disabled />}>
        <ScopeStamp
          testID={`capability-scope-${props.capability.key}`}
          label={`范围 · ${scopeLabel(value())}`}
          unset={value() === undefined}
          onPress={() => scopeSheetKey.set(props.capability.key)}
        />
      </If>
    </View>
  );
}

/** The scope choice as a bottom sheet on a phone (44px rows, the reason in
 * words under every disabled option). */
function ScopeSheet() {
  const key = () => scopeSheetKey();
  const value = () => (key() ? (identityAccess.groupDraft() ?? {})[key()!] : undefined);
  const close = () => scopeSheetKey.set(null);
  return (
    <Sheet
      testID="capability-scope-sheet"
      open={key() !== null}
      side="bottom"
      title={key() ? capabilityOf(key()!).displayName : "范围"}
      description="选择这个组在这项能力上的范围。"
      style={isMobile() ? { height: 340, maxHeight: "80%" } : undefined}
      onOpenChange={(event) => {
        if (!event.open) close();
      }}
      footer={<Button variant="outline" label="完成" accessibilityLabel="完成选择范围" onPress={close} style={touchStyle()} />}
    >
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
      <View style={{ flexDirection: "column", gap: 0 }}>
        <ScopeChoice label="未授予" selected={value() === undefined} onPress={() => { identityAccess.setDraftScope(key()!, undefined); close(); }} />
        <Each of={key() ? scopesOf(key()!) : []} key={(scope) => scope}>
          {(scope) => {
            const reason = () => (key() ? scopeDisabledReason(key()!, scope(), ownScopeOf(key()!)) : undefined);
            return (
              <ScopeChoice
                label={SCOPE_LABEL[scope()]}
                selected={value() === scope()}
                reason={reason()}
                onPress={() => {
                  if (reason()) return;
                  identityAccess.setDraftScope(key()!, scope());
                  close();
                }}
              />
            );
          }}
        </Each>
      </View>
      </ScrollArea>
    </Sheet>
  );
}

function ScopeChoice(props: { label: string; selected: boolean; reason?: string; onPress: () => void }) {
  const disabled = () => props.reason !== undefined && props.reason !== "";
  return (
    <PressRow
      accessibilityLabel={`${props.label}${props.reason ? `，${props.reason}` : ""}`}
      onPress={disabled() ? undefined : props.onPress}
      disabled={disabled()}
      selected={props.selected}
      style={{ borderBottomWidth: 1 }}
    >
      <Text style={{ fontSize: typeScale.base, color: color.foreground }}>{props.label}</Text>
      <If when={props.selected}>
        <Icon name="check" size={14} tint={color.primary} />
      </If>
      <If when={props.reason !== undefined && props.reason !== ""}>
        <Text style={{ marginLeft: "auto", fontSize: typeScale.sm, color: color.mutedForeground }}>{props.reason}</Text>
      </If>
    </PressRow>
  );
}

const changeLabel = (change: GroupChange): string =>
  change.kind === "add" ? "新增" : change.kind === "raise" ? "范围提升" : change.kind === "revoke" ? "撤销" : "收窄";

function GroupChanges(props: { editable: boolean }) {
  const changes = () => identityAccess.groupChanges();
  const members = () => identityAccess.groupDetail()?.members.length ?? 0;
  return (
    <If when={changes().length > 0}>
      <View testID="group-changes" style={{ flexDirection: "column", gap: space.sm }}>
        <BandHead num="—" title="本次变更" right={<Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>保存前可核对</Text>} />
        <Each of={changes()} key={(change) => change.key}>
          {(change) => (
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, minHeight: 32, borderTopWidth: 1, borderTopColor: color.border }}>
              <View style={{ flex: 1, minWidth: 0 }}>
                <Text style={{ fontSize: typeScale.base, color: color.foreground }}>
                  {capabilityOf(change().key).displayName}
                </Text>
                <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>{change().key}</Text>
              </View>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                {scopeLabel(change().from)} → <Text style={{ color: color.foreground }}>{scopeLabel(change().to)}</Text>
              </Text>
              <Badge
                variant={change().kind === "add" || change().kind === "raise" ? "info" : "secondary"}
                label={changeLabel(change())}
              />
              <If when={change().highRisk}>
                <RiskBadge risk="critical" />
              </If>
            </View>
          )}
        </Each>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Icon name="alert" size={12} tint={color.mutedForeground} />
          <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
            这次变更影响 {members()} 位成员的下一次操作；正在运行的会话不受影响，撤销在下一次工具调用时生效。
          </Text>
        </View>
        <If when={!props.editable}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>只读：这里不提供保存。</Text>
        </If>
      </View>
    </If>
  );
}

/** The sign-off bar: who, how many, and the one save. It sits at the end of the
 * paper (the paper scrolls as one column; the bar is its last band). */
function GroupSignoff() {
  const changes = () => identityAccess.groupChanges();
  const dirty = () => changes().length > 0;
  const editable = () => canManageGroups() && identityAccess.groupDetail()?.group.builtin !== true;
  const me = () => store.identity();
  return (
    <View
      testID="group-signoff"
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
          本次登记人：{me()?.displayName || me()?.username || "当前账号"}
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>保存后写入 05 记录 · 立即生效</Text>
      </View>
      <If when={dirty()}>
        <Badge variant="warning" label={`${changes().length} 项未保存`} />
      </If>
      <If when={dirty() && changes().some((change) => change.highRisk)}>
        <Badge variant="destructive" label={`${changes().filter((change) => change.highRisk).length} 项高危 · 需确认`} />
      </If>
      <View style={{ marginLeft: "auto", flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <Button
          testID="group-discard"
          variant="ghost"
          size="sm"
          label="放弃"
          accessibilityLabel="放弃修改"
          disabled={!dirty()}
          onPress={() => identityAccess.discardDraft()}
          style={touchStyle()}
        />
        <Button
          testID="group-save"
          size={isMobile() ? "default" : "sm"}
          icon="check"
          label="检查并签记"
          accessibilityLabel="检查并签记"
          disabled={!dirty() || !editable() || identityAccess.groupSaving()}
          loading={identityAccess.groupSaving()}
          onPress={() => {
            confirmHighRisk.set({});
            ledgerOpen.set(true);
          }}
          style={touchStyle()}
        />
      </View>
    </View>
  );
}

/** 保存前账本: the diff, the signatory, the affected members and one explicit
 * tick per widened high-risk key. The confirm stays disabled until every tick is
 * there. */
function LedgerSheet() {
  const changes = () => identityAccess.groupChanges();
  const high = () => changes().filter((change) => change.highRisk);
  const allTicked = () => high().every((change) => confirmHighRisk()[change.key] === true);
  const members = () => identityAccess.groupDetail()?.members.length ?? 0;
  const me = () => store.identity();
  const close = () => ledgerOpen.set(false);
  const submit = async () => {
    const result = await identityAccess.saveGroupDraft();
    if (result.ok) {
      ledgerOpen.set(false);
      savedNotice.set(`已保存并登记 ${result.applied} 项变更 · 已写入 05 记录。`);
      confirmHighRisk.set({});
    }
  };
  return (
    <Sheet
      testID="group-ledger-sheet"
      open={ledgerOpen()}
      side={isMobile() ? "bottom" : "right"}
      style={isMobile() ? { height: 520, maxHeight: "88%" } : undefined}
      title={`确认 ${changes().length} 项变更`}
      description="保存前核对：这次变更会立即影响这些成员的下一次操作。"
      onOpenChange={(event) => {
        if (!event.open) close();
      }}
      footer={
        <>
          <Button variant="outline" label="取消" accessibilityLabel="取消保存" onPress={close} style={touchStyle()} />
          <Button
            testID="group-ledger-confirm"
            label={identityAccess.groupSaving() ? "保存中…" : "确认并登记"}
            accessibilityLabel="确认并登记"
            loading={identityAccess.groupSaving()}
            disabled={!allTicked() || identityAccess.groupSaving()}
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
      </View>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        本次登记人：{me()?.displayName || me()?.username || "当前账号"}
      </Text>
      <ScrollArea maxHeight={isMobile() ? 260 : 360} style={{ flexDirection: "column" }}>
        <Each of={changes()} key={(change) => change.key}>
          {(change) => (
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, minHeight: 36, borderTopWidth: 1, borderTopColor: color.border }}>
              <View style={{ flex: 1, minWidth: 0 }}>
                <Text style={{ fontSize: typeScale.base, color: color.foreground }}>{capabilityOf(change().key).displayName}</Text>
                <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>{change().key}</Text>
              </View>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                {scopeLabel(change().from)} → <Text style={{ color: color.foreground }}>{scopeLabel(change().to)}</Text>
              </Text>
              <Badge variant={change().kind === "add" || change().kind === "raise" ? "info" : "secondary"} label={changeLabel(change())} />
              <If when={change().highRisk}>
                <Badge variant="destructive" label="高危" />
              </If>
            </View>
          )}
        </Each>
      </ScrollArea>
      <StateNote tone="info" icon="info" text={`影响 ${members()} 位成员的下一次操作 · 写入 05 记录。`} />
      <Each of={high()} key={(change) => change.key}>
        {(change) => {
          const target = () =>
            `${capabilityOf(change().key).displayName}」的「${scopeLabel(change().to)}」范围（高危）`;
          return (
            <Checkbox
              testID={`group-high-risk-${change().key}`}
              checked={confirmHighRisk()[change().key] === true}
              onChange={(checked) => confirmHighRisk.update((map) => ({ ...map, [change().key]: checked }))}
              label={`我确认「${target()}`}
              style={isMobile() ? { minHeight: 44 } : undefined}
            />
          );
        }}
      </Each>
      <If when={high().length > 0 && !allTicked()}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>勾选后才能登记。</Text>
      </If>
      <If when={identityAccess.groupSaveError() !== null}>
        <StateNote tone="danger" icon="alert" text={identityAccess.groupSaveError() ?? ""} />
      </If>
      </View>
      </ScrollArea>
    </Sheet>
  );
}

/** 成员: the roster with the account names the index already read. */
function GroupMembers(props: { groupId: string; builtin: boolean }) {
  const detail = () => identityAccess.groupDetail();
  const members = () => detail()?.members ?? [];
  const rowOf = (accountId: string) => identityAccess.accounts().find((account) => account.accountId === accountId);
  const editable = () => canGrantMembership() && !props.builtin;
  return (
    <View testID="group-members" style={{ flexDirection: "column", gap: space.md }}>
      <BandHead
        num="04 / 03"
        title="成员"
        right={
          <If
            when={editable()}
            else={
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                {props.builtin ? "内置组只读" : "缺「授予账号权限」能力"}
              </Text>
            }
          >
            <Button
              testID="group-add-member"
              variant="outline"
              size="sm"
              icon="user-plus"
              label="添加成员"
              accessibilityLabel="添加成员"
              onPress={() => addMemberDialog.set(true)}
              style={touchStyle()}
            />
          </If>
        }
      />
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        {members().length} 位成员 · 成员的能力是它所有组的并集
      </Text>
      <If when={members().length === 0}>
        <PageEmpty
          icon="users"
          title="这个组还没有成员"
          description="加人之后，组里的能力才会作用到对方的下一次操作。"
          primaryLabel={editable() ? "添加成员" : undefined}
          onPrimary={editable() ? () => addMemberDialog.set(true) : undefined}
        />
      </If>
      <Each of={members()} key={(member) => member.accountId}>
        {(member) => {
          const account = rowOf(member().accountId);
          const name = account?.displayName || account?.username || member().accountId;
          return (
            <View
              testID={`group-member-${member().accountId}`}
              style={{ flexDirection: "row", alignItems: "center", gap: space.md, minHeight: 44, borderTopWidth: 1, borderTopColor: color.border }}
            >
              <Avatar name={name} size="sm" />
              <View style={{ flex: 1, minWidth: 0 }}>
                <Text style={{ fontSize: typeScale.base, color: color.foreground }}>{name}</Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  {account ? `${account.username} · ${account.kind === "bot" ? "Bot" : account.kind === "service" ? "服务" : "人"}` : "账号未在索引里读到"}
                  {member().createdAt ? ` · 加入于 ${formatWhen(member().createdAt)}` : ""}
                </Text>
              </View>
              <If when={editable()}>
                <Button
                  variant="ghost"
                  size="sm"
                  label="移除"
                  accessibilityLabel={`移除成员 ${name}`}
                  onPress={() => removeMemberRequest.set({ groupId: props.groupId, accountId: member().accountId, name })}
                  style={touchStyle()}
                />
              </If>
            </View>
          );
        }}
      </Each>
      <If when={identityAccess.groupActionError() !== null}>
        <StateNote tone="danger" icon="alert" text={identityAccess.groupActionError() ?? ""} />
      </If>
    </View>
  );
}

/** 添加成员: accounts not already in the group, with their own groups as a
 * duplicate guard. */
function AddMemberDialog(props: { groupId: string }) {
  const query = signal("");
  const busy = signal<string | null>(null);
  const error = signal<string | null>(null);
  const members = () => new Set((identityAccess.groupDetail()?.members ?? []).map((member) => member.accountId));
  const rows = () =>
    identityAccess.accounts().filter((account) => {
      if (members().has(account.accountId)) return false;
      const text = query().trim().toLowerCase();
      return !text || [account.displayName, account.username].some((part) => part.toLowerCase().includes(text));
    });
  const close = () => addMemberDialog.set(false);
  const add = async (accountId: string) => {
    busy.set(accountId);
    error.set(null);
    const ok = await identityAccess.addGroupMember(props.groupId, accountId);
    busy.set(null);
    if (ok) close();
    else error.set(identityAccess.groupActionError() ?? "添加失败。");
  };
  return (
    <Dialog
      testID="group-add-member-dialog"
      open={addMemberDialog()}
      onOpenChange={(event) => {
        if (!event.open) close();
      }}
      title="添加成员"
      description="候选行显示对方已在的组，避免重复授权。"
      footer={<Button variant="ghost" label="关闭" accessibilityLabel="关闭添加成员" onPress={close} style={touchStyle()} />}
    >
      <Input
        testID="group-add-member-search"
        value={query()}
        onChange={(value) => query.set(value)}
        placeholder="搜索名字或用户名"
        accessibilityLabel="搜索成员"
                style={{ height: isMobile() ? 44 : undefined }}
        />
      <If when={rows().length === 0}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>没有可添加的人。换个名字，或先建账号。</Text>
      </If>
      <ScrollArea maxHeight={320} style={{ flexDirection: "column" }}>
        <Each of={rows()} key={(account) => account.accountId}>
          {(account) => (
            <PressRow
              testID={`group-add-member-${account().accountId}`}
              accessibilityLabel={`添加成员 ${account().displayName}`}
              onPress={() => void add(account().accountId)}
              disabled={busy() !== null}
            >
              <Avatar name={account().displayName} size="sm" />
              <View style={{ flex: 1, minWidth: 0 }}>
                <Text style={{ fontSize: typeScale.base, color: color.foreground }}>
                  {account().displayName || account().username}
                </Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  {account().username} · {account().kind === "bot" ? "Bot" : account().kind === "service" ? "服务" : "人"}
                </Text>
              </View>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                {busy() === account().accountId ? "添加中…" : "添加"}
              </Text>
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

/** 新建权限组 / 另存为副本 — the two create paths. */
function CreateGroupDialog() {
  const name = signal("");
  const busy = signal(false);
  const error = signal<string | null>(null);
  const close = () => createGroupDialog.set(false);
  const submit = async () => {
    if (!name().trim() || busy()) return;
    busy.set(true);
    error.set(null);
    const group = await identityAccess.createGroup(name().trim());
    busy.set(false);
    if (group) {
      close();
      name.set("");
      store.openIdentityObject("groups", group.groupId);
    } else {
      error.set(identityAccess.groupActionError() ?? "创建失败。");
    }
  };
  return (
    <Dialog
      testID="group-create-dialog"
      open={createGroupDialog()}
      onOpenChange={(event) => {
        if (!event.open) close();
      }}
      title="新建权限组"
      description="先建一个空组，再选能力、加成员。"
      footer={
        <>
          <Button variant="ghost" label="取消" accessibilityLabel="取消新建权限组" onPress={close} style={touchStyle()} />
          <Button
            testID="group-create-submit"
            label={busy() ? "创建中…" : "创建"}
            accessibilityLabel="创建权限组"
            loading={busy()}
            disabled={busy() || !name().trim()}
            onPress={() => void submit()}
            style={touchStyle()}
          />
        </>
      }
    >
      <Field label="名称" required>
        <Input
          testID="group-create-name"
          value={name()}
          onChange={(value) => name.set(value)}
          placeholder="例如 客服组"
          accessibilityLabel="权限组名称"
          style={{ height: isMobile() ? 44 : undefined }}
        />
      </Field>
      <If when={error() !== null}>
        <StateNote tone="danger" icon="alert" text={error() ?? ""} />
      </If>
    </Dialog>
  );
}

function CopyGroupDialog(props: { groupId: string }) {
  const name = signal("");
  const busy = signal(false);
  const error = signal<string | null>(null);
  const source = () => identityAccess.groupDetail();
  let seededFor: string | null = null;
  effect(() => {
    const detail = source();
    if (!detail || detail.group.groupId !== props.groupId || seededFor === detail.group.groupId) return;
    seededFor = detail.group.groupId;
    name.set(`${detail.group.name} 副本`);
  });
  const close = () => groupCopyDialog.set(false);
  const submit = async () => {
    const detail = source();
    if (!detail || !name().trim() || busy()) return;
    busy.set(true);
    error.set(null);
    const created = await identityAccess.copyGroupAs(detail, name().trim());
    busy.set(false);
    if (created) {
      close();
      store.openIdentityObject("groups", created.groupId);
    } else {
      error.set(identityAccess.groupActionError() ?? "复制失败。");
    }
  };
  return (
    <Dialog
      testID="group-copy-dialog"
      open={groupCopyDialog()}
      onOpenChange={(event) => {
        if (!event.open) close();
      }}
      title="另存为副本"
      description="副本从这些能力开始，成员另加；原组不受影响。"
      footer={
        <>
          <Button variant="ghost" label="取消" accessibilityLabel="取消另存为副本" onPress={close} style={touchStyle()} />
          <Button
            testID="group-copy-submit"
            label={busy() ? "复制中…" : "创建副本"}
            accessibilityLabel="创建副本"
            loading={busy()}
            disabled={busy() || !name().trim()}
            onPress={() => void submit()}
            style={touchStyle()}
          />
        </>
      }
    >
      <Field label="副本名称" required>
        <Input
          testID="group-copy-name"
          value={name()}
          onChange={(value) => name.set(value)}
          accessibilityLabel="副本名称"
          style={{ height: isMobile() ? 44 : undefined }}
        />
      </Field>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        会复制每个 key 的最宽范围；成员不会复制，避免把同一批人意外放进新组。
      </Text>
      <If when={error() !== null}>
        <StateNote tone="danger" icon="alert" text={error() ?? ""} />
      </If>
    </Dialog>
  );
}

/** Every dialog the chapter can raise, mounted once by the identity chapter (the
 * index's 新建权限组 works whether or not a group page is open). */
export function GroupDialogs() {
  const groupId = () => identityAccess.groupDetailId() ?? "";
  const remove = () => removeMemberRequest();
  return (
    <>
      <AlertDialog
        testID="group-remove-member-dialog"
        open={remove() !== null}
        title="移出这个成员？"
        description={`移除后 ${remove()?.name ?? ""} 仍保留其他组与直接授权带来的能力。`}
        confirmLabel="移出"
        cancelLabel="取消"
        destructive
        onConfirm={() => {
          const request = remove();
          removeMemberRequest.set(null);
          if (request) void identityAccess.removeGroupMember(request.groupId, request.accountId);
        }}
        onCancel={() => removeMemberRequest.set(null)}
      />
      <AddMemberDialog groupId={groupId()} />
      <LedgerSheet />
      <ScopeSheet />
      <CreateGroupDialog />
      <CopyGroupDialog groupId={groupId()} />
    </>
  );
}

/** The group page's empty paper (wide layout). */
export function GroupsWelcome() {
  return (
    <PageEmpty
      testID="groups-welcome"
      icon="shield"
      title="选择一个权限组"
      description="左侧是这台部署的权限组；点一行查看并编辑它的能力与成员。"
      primaryLabel={canManageGroups() ? "新建权限组" : undefined}
      onPrimary={canManageGroups() ? () => createGroupDialog.set(true) : undefined}
    />
  );
}

/** Mounted by the chapter: the group page's paper. Its dialogs live at the
 * chapter level (`GroupDialogs`), so the index's create action works too. */
export function GroupSurface(props: { groupId: string; showBack: boolean }) {
  return (
    <View style={{ flex: 1, minHeight: 0 }}>
      <GroupPage groupId={props.groupId} showBack={props.showBack} />
    </View>
  );
}

