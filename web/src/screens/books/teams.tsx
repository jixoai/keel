// The 03 团队 chapter — change keel-bot-identity-access, A4.4b.
//
// A TeamBot is a mission plus a roster of Bot references. It owns no session and
// grants no permission: every seat runs with its own account's scope. The page
// makes that sentence permanent, shows each seat's invocability, and puts the
// one start entry behind the three preflight checks.
//
// The run list is honest about its boundary: the server has no team-run list
// route yet, so only the starts this browser saw are listed. A start receipt is
// a real session id; history beyond it is named as a gap, never filled with a
// sample row.
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
import { can } from "../../data/permissions";
import { botAccess, type PreflightRow, type StartedRun } from "../../data/bots";
import { canShareResource, openTeamBotShare, sharedBadgeOf } from "../../data/shares";
import { capabilityOf } from "../../data/permission-catalog";
import type { BotView, RosterEntry, TeamBotView } from "../../data/identity";
import { identityAccess } from "../../data/identity-access";
import {
  BandHead,
  BookAvatar,
  FactLine,
  Kicker,
  ObjectSpine,
  PageEmpty,
  PageError,
  PageLoading,
  PagePermission,
  PageUnavailable,
  PaperBody,
  PressRow,
  Segmented,
  SheetHead,
  StateNote,
  copyText,
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

const captainSeat = (roster: RosterEntry[]): RosterEntry | undefined => roster.find((seat) => seat.role === "captain");

const accountLabelOf = (bot: BotView | undefined): string =>
  bot ? botAccess.accountNameOf(bot.accountId) || "账号名未读取" : "不在可见 Bot 里";

const invocationOf = (bot: BotView | undefined): { label: string; variant: "success" | "destructive" | "secondary" } => {
  if (!bot) return { label: "不可调用", variant: "destructive" };
  if (bot.archived) return { label: "已归档", variant: "secondary" };
  if (!can("bot.invoke")) return { label: "不可调用", variant: "destructive" };
  return { label: "可调用", variant: "success" };
};

// ---- the page state -------------------------------------------------------

export type TeamSection = "mission" | "roster" | "runs" | "records";

const TEAM_SECTIONS: Array<{ id: TeamSection; num: string; label: string }> = [
  { id: "mission", num: "01", label: "使命" },
  { id: "roster", num: "02", label: "成员" },
  { id: "runs", num: "03", label: "运行" },
  { id: "records", num: "04", label: "记录" },
];

const teamSection = signal<TeamSection>("roster");
const teamLedgerOpen = signal(false);
const teamConfirmHigh = signal<Record<string, boolean>>({});
const teamArchiveOpen = signal(false);
const teamStartOpen = signal(false);
const teamAddMemberOpen = signal(false);
const teamMemberQuery = signal("");
const teamCaptainOpen = signal(false);
const teamCaptainChoice = signal("");
const teamCreateOpen = signal(false);
const teamCreateName = signal("");
const teamCreateMission = signal("");
const teamCreateCaptain = signal("");
const lastTeamId = signal<string | null>(null);

const sectionLabel = (section: TeamSection): string => TEAM_SECTIONS.find((item) => item.id === section)?.label ?? "成员";
const sectionNum = (section: TeamSection): string => TEAM_SECTIONS.find((item) => item.id === section)?.num ?? "02";

/** The run page's spine sections point back at the team page; this setter lets
 * it land on the section the reader asked for instead of a default tab. */
export const showTeamSection = (section: TeamSection): void => {
  teamSection.set(section);
};

/** Drop the reader's transient team UI when the session changes. */
export function resetTeamsUi(): void {
  teamSection.set("roster");
  teamLedgerOpen.set(false);
  teamConfirmHigh.set({});
  teamArchiveOpen.set(false);
  teamStartOpen.set(false);
  teamAddMemberOpen.set(false);
  teamMemberQuery.set("");
  teamCaptainOpen.set(false);
  teamCaptainChoice.set("");
  teamCreateOpen.set(false);
  teamCreateName.set("");
  teamCreateMission.set("");
  teamCreateCaptain.set("");
  lastTeamId.set(null);
}

// ---- the chapter ----------------------------------------------------------

export function TeamsChapter() {
  const objectId = () => store.routeObjectId();
  return (
    <View testID="teams-chapter" style={{ flex: 1, minHeight: 0, flexDirection: "row" }}>
      <If when={objectId() !== null} else={<TeamsIndex />}>
        <TeamPage teamId={objectId() ?? ""} />
      </If>
      <TeamDialogs />
    </View>
  );
}

// ---- the index ------------------------------------------------------------

export function TeamsIndex() {
  const visible = () => botAccess.visibleTeams();
  const loading = () => botAccess.teamsLoading() && botAccess.teams().length === 0;
  const failed = () => botAccess.teamsError() !== null;
  const error = () => botAccess.teamsError();
  const active = () => botAccess.teams().filter((team) => !team.archived).length;
  const todo = () => botAccess.todoItems().length;
  return (
    <ScrollArea testID="teams-index" accessibilityLabel="团队索引" style={{ flex: 1, minHeight: 0 }}>
      <PaperBody testID="teams-paper">
        <SheetHead
          testID="teams-head"
          kicker={{ icon: "users", text: "03 / 团队 · TeamBot" }}
          title="团队"
          badges={
            <>
              <Badge variant="secondary" label={`${botAccess.teams().length} 个团队`} />
              <Badge variant="secondary" label={`${active()} 个待启动`} />
              <If when={todo() > 0}>
                <Badge variant="warning" icon="alert" label={`${todo()} 项待处理`} />
              </If>
            </>
          }
          description="给团队一句使命，再选几个 Bot 加入。团队本身不拥有权限，也不拥有会话。"
          actions={
            <If when={can("team_bot.manage")}>
              <Button
                testID="teams-create"
                size="sm"
                icon="plus"
                label="新建团队"
                accessibilityLabel="新建团队"
                onPress={() => {
                  teamCreateName.set("");
                  teamCreateMission.set("");
                  teamCreateCaptain.set("");
                  teamCreateOpen.set(true);
                }}
                style={touchStyle()}
              />
            </If>
          }
        />
        <View style={{ flexDirection: "row", gap: space.sm, flexWrap: "wrap", alignItems: "center" }}>
          <Input
            testID="teams-search"
            value={botAccess.teamsQuery()}
            onChange={(value) => botAccess.teamsQuery.set(value)}
            placeholder="搜索团队或使命"
            accessibilityLabel="搜索团队"
            size="sm"
            style={{ width: isMobile() ? "100%" : 280, height: isMobile() ? 44 : undefined }}
          />
          <Select
            testID="teams-status-filter"
            size={isMobile() ? "lg" : "sm"}
            items={[
              { value: "all", title: "全部状态" },
              { value: "active", title: "未归档" },
              { value: "archived", title: "已归档" },
            ]}
            value={botAccess.teamsStatus()}
            onChange={(value) => botAccess.teamsStatus.set((value ?? "all") as "all" | "active" | "archived")}
            accessibilityLabel="按状态筛选"
            height={isMobile() ? 44 : undefined}
            style={{ width: isMobile() ? "100%" : 150 }}
          />
        </View>

        <If when={loading()}>
          <PageLoading rows={6} testID="teams-index-loading" />
        </If>
        <If when={failed() && !loading()}>
          <If when={error()?.status === 403} else={<PageError
            title="没能读到团队列表"
            message={error()?.unavailable ? "这个部署还没有接入团队索引。" : error()?.message ?? ""}
            requestId={error()?.requestId}
            onRetry={() => void botAccess.loadTeams()}
            retrying={botAccess.teamsLoading()}
            testID="teams-index-error"
          />}>
            <PagePermission
              capability={capabilityOf(error()?.requiredKey ?? "team_bot.read").displayName}
              capabilityKey={error()?.requiredKey ?? "team_bot.read"}
            />
          </If>
        </If>
        <If when={!loading() && !failed() && botAccess.teams().length === 0}>
          <PageEmpty
            testID="teams-index-empty"
            icon="users"
            title="还没有团队"
            description="给团队一句使命，再选几个 Bot 加入。团队本身不拥有权限。"
            primaryLabel={can("team_bot.manage") ? "新建团队" : undefined}
            onPrimary={() => {
              teamCreateName.set("");
              teamCreateMission.set("");
              teamCreateCaptain.set("");
              teamCreateOpen.set(true);
            }}
            secondaryLabel="去员工"
            onSecondary={() => store.openChapter("bots")}
          />
        </If>
        <If when={!loading() && !failed() && botAccess.teams().length > 0 && visible().length === 0}>
          <PageEmpty
            icon="search"
            title="没有匹配的团队"
            description="换个名字或清空筛选。"
            primaryLabel="清空筛选"
            onPrimary={() => {
              botAccess.teamsQuery.set("");
              botAccess.teamsStatus.set("all");
            }}
          />
        </If>
        <If when={!loading() && !failed() && visible().length > 0}>
          <If when={isNarrow()} else={<TeamsTable rows={visible()} />}>
            <TeamsCards rows={visible()} />
          </If>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, paddingTop: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              显示 {visible().length} / {botAccess.teams().length} 个团队
            </Text>
            <If when={botAccess.teamsHasMore()}>
              <Button
                testID="teams-load-more"
                variant="ghost"
                size="sm"
                label="加载更多"
                accessibilityLabel="加载更多团队"
                loading={botAccess.teamsLoading()}
                onPress={() => void botAccess.loadMoreTeams()}
                style={touchStyle()}
              />
            </If>
          </View>
          <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
            运行历史还没有从服务端返回；列表里的「最近运行」只显示本次打开期间启动的运行，不显示推测值。
          </Text>
        </If>
      </PaperBody>
    </ScrollArea>
  );
}

function teamStatus(team: TeamBotView): { label: string; variant: "success" | "warning" | "secondary" | "outline" } {
  if (team.archived) return { label: "已归档", variant: "secondary" };
  if (botAccess.botsLoading() && botAccess.bots().length === 0) return { label: "载入中", variant: "outline" };
  if (botAccess.botsError() !== null) return { label: "成员状态未知", variant: "outline" };
  const seat = captainSeat(team.roster);
  if (!seat || !team.mission.trim()) return { label: "草稿", variant: "outline" };
  const problems = team.roster.filter((entry) => {
    const bot = botAccess.bots().find((candidate) => candidate.botId === entry.botId);
    return !bot || bot.archived || !can("bot.invoke");
  });
  if (problems.length > 0) return { label: "有成员需处理", variant: "warning" };
  return { label: "待启动", variant: "success" };
}

function teamStatusBadge(team: TeamBotView) {
  const status = teamStatus(team);
  return <Badge variant={status.variant} label={status.label} style={{ alignSelf: "flex-start" }} />;
}

function latestRun(teamId: string): StartedRun | undefined {
  return botAccess.recentRunOf(teamId);
}

function TeamsTable(props: { rows: TeamBotView[] }) {
  return (
    <Table testID="teams-table" bare accessibilityLabel="团队列表">
      <TableHeaderRow>
        <Th>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>团队</Text>
        </Th>
        <Th width={140}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>队长</Text>
        </Th>
        <Th width={220}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>使命</Text>
        </Th>
        <Th width={86}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>成员</Text>
        </Th>
        <Th width={110}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>状态</Text>
        </Th>
        <Th width={110}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>最近运行</Text>
        </Th>
      </TableHeaderRow>
      <Each of={props.rows} key={(team) => team.teamBotId}>
        {(team) => (
          <TableBodyRow testID={`team-row-${team().teamBotId}`}>
            <Td>
              <PressRow
                accessibilityLabel={`打开团队 ${team().name}`}
                onPress={() => store.openTeam(team().teamBotId)}
                style={{ borderBottomWidth: 0, minHeight: 44, marginHorizontal: -space.lg, paddingHorizontal: space.lg }}
              >
                <BookAvatar name={team().name} />
                <View style={{ flexDirection: "column", minWidth: 0 }}>
                  <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                    {team().name}
                  </Text>
                  <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                    {team().teamBotId}
                  </Text>
                </View>
              </PressRow>
            </Td>
            <Td>
              <Text style={{ fontSize: typeScale.sm, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                {botAccess.botNameOf(captainSeat(team().roster)?.botId ?? "") || "未设队长"}
              </Text>
            </Td>
            <Td>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                {team().mission}
              </Text>
            </Td>
            <Td>
              <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>{team().roster.length}</Text>
            </Td>
            <Td>{teamStatusBadge(team())}</Td>
            <Td>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                {latestRun(team().teamBotId) ? formatWhen(latestRun(team().teamBotId)?.startedAt) : "—"}
              </Text>
            </Td>
          </TableBodyRow>
        )}
      </Each>
    </Table>
  );
}

function TeamsCards(props: { rows: TeamBotView[] }) {
  return (
    <View style={{ flexDirection: "column", gap: space.sm }}>
      <Each of={props.rows} key={(team) => team.teamBotId}>
        {(team) => (
          <PressRow
            testID={`team-card-${team().teamBotId}`}
            accessibilityLabel={`打开团队 ${team().name}`}
            onPress={() => store.openTeam(team().teamBotId)}
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
              <BookAvatar name={team().name} />
              <View style={{ flex: 1, minWidth: 0 }}>
                <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                  {team().name}
                </Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                  队长 {botAccess.botNameOf(captainSeat(team().roster)?.botId ?? "") || "未设"} · {team().roster.length} 名成员
                </Text>
              </View>
              {teamStatusBadge(team())}
            </View>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 2 }}>
              {team().mission}
            </Text>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              最近运行 {latestRun(team().teamBotId) ? formatWhen(latestRun(team().teamBotId)?.startedAt) : "未记录"}
            </Text>
          </PressRow>
        )}
      </Each>
    </View>
  );
}

// ---- the page -------------------------------------------------------------

export function TeamPage(props: { teamId: string }) {
  const id = () => props.teamId;
  effect(() => {
    const teamId = id();
    if (lastTeamId() !== teamId) {
      lastTeamId.set(teamId);
      teamSection.set("roster");
      teamConfirmHigh.set({});
    }
    if (botAccess.teamDetailId() !== teamId || botAccess.teamDetail() === null) void botAccess.loadTeam(teamId);
  });
  const team = () => botAccess.teamDetail();
  const loading = () => botAccess.teamDetailLoading() && team() === null;
  const failed = () => botAccess.teamDetailError() !== null;
  const error = () => botAccess.teamDetailError();
  const name = () => team()?.name ?? botAccess.teams().find((row) => row.teamBotId === id())?.name ?? "团队";
  const dirty = () => botAccess.teamChanges().length > 0;
  const canManage = () => can("team_bot.manage", { archived: team()?.archived === true });
  const captain = () => captainSeat(team()?.roster ?? []);
  const captainBot = () => botAccess.bots().find((bot) => bot.botId === captain()?.botId);
  return (
    <View testID="team-page" style={{ flex: 1, minHeight: 0, flexDirection: "row" }}>
      <If when={!isNarrow()}>
        <ObjectSpine
          testID="team-spine"
          testIDPrefix="team-section"
          kicker={{ icon: "users", text: "团队 / TeamBot" }}
          title={name()}
          subtitle={team() ? `${captainBot()?.name ?? "未设队长"} · ${team()!.roster.length} 名成员` : ""}
          description={team()?.mission || undefined}
          facts={
            team()
              ? [
                  { label: "队长", value: captainBot()?.name ?? "未设队长" },
                  { label: "成员", value: `${team()!.roster.length} 名` },
                  { label: "最近运行", value: latestRun(team()!.teamBotId) ? formatWhen(latestRun(team()!.teamBotId)?.startedAt) : "未记录" },
                  { label: "共享", value: sharedBadgeOf(team()!.sharedCount) ?? "未共享" },
                  { label: "状态", value: team()!.archived ? "已归档" : teamStatus(team()!).label },
                ]
              : []
          }
          items={TEAM_SECTIONS.map((item) => ({
            ...item,
            count: item.id === "roster" ? String(team()?.roster.length ?? 0) : undefined,
          }))}
          section={teamSection()}
          onSection={(next) => {
            teamSection.set(next as TeamSection);
            if (next === "runs") botAccess.loadTeams();
          }}
          actions={
            <>
              <Button
                variant="outline"
                size="sm"
                icon={dirty() ? "check" : "play"}
                label={dirty() ? "去登记" : "去启动前检查"}
                accessibilityLabel={dirty() ? "去登记" : "去启动前检查"}
                onPress={() => {
                  if (dirty()) {
                    teamConfirmHigh.set({});
                    teamLedgerOpen.set(true);
                  } else {
                    teamSection.set("roster");
                  }
                }}
                style={{ width: "100%" }}
              />
              <DropdownMenu
                testID="team-more"
                accessibilityLabel="更多团队操作"
                align="start"
                trigger={<Button variant="ghost" size="sm" icon="dots" label="更多" accessibilityLabel="更多团队操作" style={{ width: "100%" }} />}
              >
                <DropdownMenuItem
                  label="共享团队"
                  icon="users"
                  disabled={!canShareResource("team_bot", team()?.ownerAccountId)}
                  onSelect={() => openTeamBotShare(id(), name())}
                />
                <DropdownMenuItem
                  label="归档团队"
                  icon="trash"
                  danger
                  disabled={!canManage() || team()?.archived === true}
                  onSelect={() => teamArchiveOpen.set(true)}
                />
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  label="复制团队"
                  icon="copy"
                  disabled
                  onSelect={() => {
                    /* copying is a next-version action; disabled with the label */
                  }}
                />
              </DropdownMenu>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, lineHeight: Math.round(typeScale.sm * lineHeight.normal) }}>
                {dirty() ? "有未保存的变更；登记后影响下一次启动。" : "登记后写入记录 05。"}
              </Text>
            </>
          }
        />
      </If>
      <View style={{ flex: 1, minWidth: 0, minHeight: 0, flexDirection: "column" }}>
        <ScrollArea testID="team-page-scroll" accessibilityLabel={`团队 ${name()}`} style={{ flex: 1, minHeight: 0 }}>
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
              <BackToIndex label="团队" onPress={() => store.closeTeam()} />
              <TeamSummary />
            </If>
            <If when={loading()}>
              <PageLoading rows={6} testID="team-loading" />
            </If>
            <If when={failed() && !loading()}>
              <If when={error()?.status === 403} else={<PageError
                title="没能读到这个团队"
                message={error()?.message ?? ""}
                requestId={error()?.requestId}
                onRetry={() => void botAccess.loadTeam(id())}
                retrying={botAccess.teamDetailLoading()}
                testID="team-error"
              />}>
                <PagePermission
                  capability={capabilityOf(error()?.requiredKey ?? "team_bot.read").displayName}
                  capabilityKey={error()?.requiredKey ?? "team_bot.read"}
                />
              </If>
            </If>
            <If when={team() !== null && !failed()}>
              <If when={botAccess.teamNotice() !== null}>
                <StateNote tone="success" icon="check" text={botAccess.teamNotice() ?? ""} testID="team-notice" />
              </If>
              <If when={botAccess.teamActionError() !== null}>
                <StateNote tone="danger" icon="alert" text={botAccess.teamActionError() ?? ""} testID="team-action-error" />
              </If>
              <If when={botAccess.teamSaveError() !== null}>
                <StateNote tone="danger" icon="alert" text={botAccess.teamSaveError() ?? ""} testID="team-save-error" />
              </If>
              <If when={team()?.archived === true}>
                <StateNote tone="warning" icon="lock" text="这个团队已归档：不能启动或修改；历史运行仍然保留。" />
              </If>
              <BandHead
                testID="team-band-head"
                num={`03 / ${sectionNum(teamSection())}`}
                title={sectionLabel(teamSection())}
                right={
                  <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                    {teamSection() === "roster" ? `${team()?.roster.length ?? 0} 名成员` : ""}
                  </Text>
                }
              />
              <If when={teamSection() === "mission"}><TeamMissionSection /></If>
              <If when={teamSection() === "roster"}><TeamRosterSection /></If>
              <If when={teamSection() === "runs"}><TeamRunsSection /></If>
              <If when={teamSection() === "records"}><TeamRecordsSection /></If>
            </If>
          </View>
        </ScrollArea>
        <If when={!isMobile() && team() !== null && !failed() && botAccess.teamChanges().length > 0}>
          <View style={{ width: "100%", maxWidth: 1040, alignSelf: "center", paddingHorizontal: space["2xl"] }}>
            <TeamSignoff />
          </View>
        </If>
        <If when={isMobile() && team() !== null && !failed()}>
          <TeamMobileBar canManage={canManage()} />
        </If>
      </View>
    </View>
  );
}

function TeamSummary() {
  const team = () => botAccess.teamDetail();
  const captain = () => botAccess.bots().find((bot) => bot.botId === captainSeat(team()?.roster ?? [])?.botId);
  const dirty = () => botAccess.teamChanges().length;
  const status = () => (team() ? teamStatus(team()!) : { label: "—", variant: "outline" as const });
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
        <Kicker icon="users" text="团队 · TeamBot" />
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
            {team()?.name ?? "团队"}
          </Text>
          <Badge variant={status().variant} label={status().label} style={{ alignSelf: "flex-start" }} />
        </View>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          队长 {captain()?.name ?? "未设"} · {team()?.roster.length ?? 0} 名成员 · 最近运行{" "}
          {latestRun(team()?.teamBotId ?? "") ? formatWhen(latestRun(team()?.teamBotId ?? "")?.startedAt) : "未记录"}
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 2 }}>{team()?.mission}</Text>
      </View>
      <Segmented
        testID="team-segment"
        accessibilityLabel="团队章节"
        items={TEAM_SECTIONS.map((item) => ({
          id: item.id,
          label: item.label,
          count: item.id === "roster" ? String(team()?.roster.length ?? 0) : undefined,
        }))}
        value={teamSection()}
        onSelect={(next) => teamSection.set(next as TeamSection)}
      />
    </View>
  );
}

function TeamMobileBar(props: { canManage: boolean }) {
  const team = () => botAccess.teamDetail();
  const dirty = () => botAccess.teamChanges().length > 0;
  return (
    <View
      testID="team-mobile-bar"
      style={{ flexDirection: "row", gap: space.sm, padding: space.md, borderTopWidth: 1, borderTopColor: color.border, backgroundColor: color.background }}
    >
      <Button
        variant="outline"
        size="lg"
        icon={dirty() ? "users" : "play"}
        label={dirty() ? "启动前检查" : "去启动前检查"}
        accessibilityLabel="去启动前检查"
        onPress={() => teamSection.set("roster")}
        style={{ flex: 1, height: 44 }}
      />
      <If when={dirty()}>
        <Button
          size="lg"
          icon="check"
          label="检查并登记"
          accessibilityLabel="检查并登记"
          disabled={!props.canManage || team()?.archived === true}
          onPress={() => {
            teamConfirmHigh.set({});
            teamLedgerOpen.set(true);
          }}
          style={{ flex: 1, height: 44 }}
        />
      </If>
    </View>
  );
}

// ---- mission --------------------------------------------------------------

function TeamMissionSection() {
  const team = () => botAccess.teamDetail();
  const draft = () => botAccess.teamDraft();
  const canManage = () => can("team_bot.manage", { archived: team()?.archived === true });
  return (
    <View testID="team-mission" style={{ flexDirection: "column", gap: space.md, maxWidth: 760 }}>
      <StateNote
        tone="info"
        icon="info"
        text="团队本身不拥有权限，也不拥有会话。每个成员 Bot 按自己的账号与工作范围执行。"
      />
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>使命</Text>
      <Textarea
        testID="team-mission-input"
        value={draft()?.mission ?? ""}
        onChange={(value) => botAccess.setTeamMission(value)}
        rows={8}
        maxLength={4000}
        disabled={!canManage()}
        placeholder="在 24 小时内解决客户问题；无法解决时升级给人工，并附上订单号与已尝试的步骤。"
        accessibilityLabel="团队使命"
      />
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        {draft()?.mission.length ?? 0} / 4000 · 每次成员会话都会带上这句使命。写清楚目标、边界、升级条件。
      </Text>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        登记后影响下一次启动，不影响正在运行的会话。
      </Text>
      <If when={!canManage()}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>只读：需要「管理团队」能力才能修改使命。</Text>
      </If>
    </View>
  );
}

// ---- roster/preflight -----------------------------------------------------

function TeamRosterSection() {
  const team = () => botAccess.teamDetail();
  const draft = () => botAccess.teamDraft();
  const roster = () => draft()?.roster ?? [];
  const captain = () => captainSeat(roster());
  const members = () => roster().filter((seat) => seat.botId !== captain()?.botId);
  const canManage = () => can("team_bot.manage", { archived: team()?.archived === true });
  const manageDraft = () => canManage() && team()?.archived !== true;
  return (
    <View testID="team-roster" style={{ flexDirection: "column", gap: space.lg }}>
      <StateNote
        tone="info"
        icon="info"
        text="团队本身不拥有权限，也不拥有会话。每个成员 Bot 按自己的账号与工作范围执行；能不能调用某个 Bot，取决于你是否持有它的「调用 Bot」能力。"
      />
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>
          成员 {roster().length}
        </Text>
        <View style={{ marginLeft: "auto", flexDirection: "row", gap: space.sm, flexWrap: "wrap" }}>
          <Button
            testID="team-add-member"
            variant="outline"
            size={isMobile() ? "lg" : "sm"}
            icon="plus"
            label="添加成员"
            accessibilityLabel="添加成员"
            disabled={!manageDraft()}
            onPress={() => {
              teamMemberQuery.set("");
              teamAddMemberOpen.set(true);
            }}
            style={touchStyle()}
          />
          <Button
            testID="team-change-captain"
            variant="ghost"
            size={isMobile() ? "lg" : "sm"}
            label="更换队长"
            accessibilityLabel="更换队长"
            disabled={!manageDraft() || members().length === 0}
            onPress={() => {
              teamCaptainChoice.set("");
              teamCaptainOpen.set(true);
            }}
            style={touchStyle()}
          />
        </View>
      </View>
      <View style={{ flexDirection: isNarrow() ? "column" : "row", gap: space["2xl"], alignItems: "flex-start" }}>
        <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: space.md }}>
          <If
            when={roster().length > 0}
            else={
              <PageEmpty
                icon="users"
                title="还没有成员"
                description="先加一个队长；队长负责拆分任务和调用成员。"
                primaryLabel={manageDraft() ? "添加成员" : undefined}
                onPrimary={() => {
                  teamMemberQuery.set("");
                  teamAddMemberOpen.set(true);
                }}
              />
            }
          >
            <View style={{ flexDirection: "column" }}>
              <If when={captain() !== undefined}>
                <RosterRow seat={captain()!} captain canManage={manageDraft()} onRemove={() => undefined} />
              </If>
              <Each of={members()} key={(seat) => seat.botId}>
                {(seat) => (
                  <RosterRow seat={seat()} captain={false} canManage={manageDraft()} onRemove={() => botAccess.removeRosterMember(seat().botId)} />
                )}
              </Each>
            </View>
            <View
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: space.sm,
                minHeight: 44,
                paddingHorizontal: space.sm,
                borderWidth: 1,
                borderStyle: "dashed",
                borderColor: color.border,
                borderRadius: radius.md,
              }}
            >
              <Icon name="plus" size={14} tint={color.mutedForeground} />
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>从你能调用的 Bot 里选</Text>
              <Button
                variant="ghost"
                size="sm"
                label="添加成员"
                accessibilityLabel="添加成员"
                disabled={!manageDraft()}
                onPress={() => {
                  teamMemberQuery.set("");
                  teamAddMemberOpen.set(true);
                }}
                style={{ marginLeft: "auto", ...touchStyle() }}
              />
            </View>
          </If>
        </View>
        <View
          style={{
            width: isNarrow() ? "100%" : 320,
            flexShrink: 0,
            flexDirection: "column",
            gap: space.md,
            padding: isNarrow() ? 0 : space.md,
            borderWidth: isNarrow() ? 0 : 1,
            borderColor: color.border,
            borderRadius: radius.md,
            backgroundColor: isNarrow() ? undefined : color.hoverFaint,
          }}
        >
          <Preflight />
          <TeamStartPanel />
        </View>
      </View>
    </View>
  );
}

function RosterRow(props: { seat: RosterEntry; captain: boolean; canManage: boolean; onRemove: () => void }) {
  const bot = () => botAccess.bots().find((entry) => entry.botId === props.seat.botId);
  const availability = () => invocationOf(bot());
  const account = () => accountLabelOf(bot());
  const crossAccount = () => {
    const me = store.identity()?.accountId;
    return !!bot()?.accountId && !!me && bot()!.accountId !== me;
  };
  return (
    <View
      testID={`roster-row-${props.seat.botId}`}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.md,
        minHeight: 56,
        paddingVertical: space.sm,
        paddingHorizontal: space.sm,
        borderTopWidth: 1,
        borderTopColor: color.border,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flex: 1, minWidth: 0 }}>
        <BookAvatar name={bot()?.name ?? props.seat.botId} />
        <View style={{ flexDirection: "column", minWidth: 0 }}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs, flexWrap: "wrap" }}>
            <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
              {bot()?.name ?? "未载入的 Bot"}
            </Text>
            <If when={props.captain}>
              <Badge variant="info" label="队长" />
            </If>
          </View>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
            {account()} · {modelKindLabel(bot()?.modelKind)}
            {crossAccount() ? " · 其他账号" : ""}
          </Text>
        </View>
      </View>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexShrink: 0 }}>
        <Badge variant={availability().variant} label={availability().label} />
        <If when={availability().label !== "可调用" && !props.captain}>
          <Button
            variant="outline"
            size="sm"
            icon="copy"
            label="复制申请"
            accessibilityLabel="复制调用权限申请"
            onPress={() => {
              void copyText(
                `我需要「调用 Bot」能力（bot.invoke）来使用团队的成员「${bot()?.name ?? props.seat.botId}」。请把「${account()}」的授权开放给我。`,
              );
            }}
            style={touchStyle()}
          />
        </If>
        <If when={props.canManage && !props.captain}>
          <Button
            testID={`roster-remove-${props.seat.botId}`}
            variant="ghost"
            size="sm"
            icon="x"
            label="移除"
            accessibilityLabel={`移除成员 ${bot()?.name ?? props.seat.botId}`}
            onPress={props.onRemove}
            style={touchStyle()}
          />
        </If>
        <If when={props.captain}>
          <Badge variant="outline" label="名单固定第一行" />
        </If>
      </View>
    </View>
  );
}

function Preflight() {
  const rows = () => botAccess.preflight();
  const pass = () => botAccess.preflightPass();
  return (
    <View testID="team-preflight" style={{ flexDirection: "column", gap: space.sm, maxWidth: 760 }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
        <Text style={{ fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground }}>启动前检查</Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {rows().filter((row) => row.ok).length} / {rows().length}
        </Text>
      </View>
      <View style={{ flexDirection: "column" }}>
        <Each of={rows()} key={(row) => row.id}>
          {(row) => <PreflightRowView row={row()} />}
        </Each>
      </View>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, lineHeight: Math.round(typeScale.sm * lineHeight.normal) }}>
        {pass() ? "检查全部通过。启动后队长会收到使命，并可以创建、分叉、调用成员会话。" : "先把未通过的检查处理掉，启动按钮才会可用。"}
      </Text>
    </View>
  );
}

function PreflightRowView(props: { row: PreflightRow }) {
  const row = () => props.row;
  return (
    <View
      testID={`preflight-${row().id}`}
      style={{
        flexDirection: "row",
        alignItems: "flex-start",
        gap: space.md,
        paddingVertical: space.sm,
        borderTopWidth: 1,
        borderTopColor: color.border,
      }}
    >
      <Icon name={row().ok ? "check" : "alert"} size={14} tint={row().ok ? color.success : color.warning} />
      <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: 2 }}>
        <Text style={{ fontSize: typeScale.base, color: color.foreground }}>{row().title}</Text>
        <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
          {row().detail}
        </Text>
      </View>
      <If when={!row().ok}>
        <Button
          variant="ghost"
          size="sm"
          label="去处理"
          accessibilityLabel={`去处理：${row().title}`}
          onPress={() => {
            if (row().id === "mission") teamSection.set("mission");
            else teamSection.set("roster");
          }}
          style={touchStyle()}
        />
      </If>
    </View>
  );
}

function TeamStartPanel() {
  const team = () => botAccess.teamDetail();
  const pass = () => botAccess.preflightPass();
  const canManage = () => can("team_bot.manage", { archived: team()?.archived === true });
  const reason = () => {
    if (team()?.archived) return "这个团队已归档，不能启动。";
    if (!canManage()) return "启动需要「管理团队」能力。";
    if (botAccess.teamChanges().length > 0) return "有未保存的名单或使命；先保存并登记，再启动。";
    if (!pass()) return botAccess.blockingReason();
    return "";
  };
  return (
    <View style={{ flexDirection: "column", gap: space.sm, maxWidth: 760 }}>
      <Button
        testID="team-start"
        size={isMobile() ? "lg" : "default"}
        icon="play"
        label={botAccess.teamStarting() ? "启动中…" : "启动团队"}
        accessibilityLabel="启动团队"
        loading={botAccess.teamStarting()}
        disabled={reason() !== "" || botAccess.teamStarting()}
        onPress={() => {
          teamConfirmHigh.set({});
          teamStartOpen.set(true);
        }}
        style={touchStyle()}
      />
      <If when={reason() !== ""}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, lineHeight: Math.round(typeScale.sm * lineHeight.normal) }}>
          {reason()}
        </Text>
      </If>
      <View style={{ flexDirection: "column", gap: space.xs }}>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>启动后会发生什么</Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>· 队长收到使命，可以创建、分叉、调用成员会话。</Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>· 会产生模型用量；运行页可以随时停止。</Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>· 成员各自以自己的账号与工作范围执行。</Text>
      </View>
    </View>
  );
}

function TeamSignoff() {
  const changes = () => botAccess.teamChanges();
  const dirty = () => changes().length > 0;
  const high = () => changes().filter((change) => change.highRisk);
  const me = () => store.identity();
  return (
    <View
      testID="team-signoff"
      style={{ flexDirection: "row", alignItems: "center", gap: space.md, paddingTop: space.md, borderTopWidth: 1, borderTopColor: color.border, flexWrap: "wrap" }}
    >
      <View style={{ flexDirection: "column", minWidth: 0 }}>
        <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>
          本次登记人：{me()?.displayName || me()?.username || "你的账号"}
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>登记后写入记录 05 · 影响下一次启动</Text>
      </View>
      <If when={dirty()}>
        <Badge variant="warning" label={`${changes().length} 项未保存`} />
      </If>
      <If when={high().length > 0}>
        <Badge variant="destructive" label={`${high().length} 项高危 · 需确认`} />
      </If>
      <View style={{ marginLeft: "auto", flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <Button
          testID="team-discard"
          variant="ghost"
          size="sm"
          label="放弃"
          accessibilityLabel="放弃修改"
          disabled={!dirty()}
          onPress={() => botAccess.discardTeamDraft()}
          style={touchStyle()}
        />
        <Button
          testID="team-save"
          size="sm"
          icon="check"
          label="检查并登记"
          accessibilityLabel="检查并登记"
          disabled={!dirty() || !can("team_bot.manage", { archived: false })}
          onPress={() => {
            teamConfirmHigh.set({});
            teamLedgerOpen.set(true);
          }}
          style={touchStyle()}
        />
      </View>
    </View>
  );
}

// ---- runs -----------------------------------------------------------------

function TeamRunsSection() {
  const team = () => botAccess.teamDetail();
  const runs = () => botAccess.startedRuns()[team()?.teamBotId ?? ""] ?? [];
  return (
    <View testID="team-runs" style={{ flexDirection: "column", gap: space.md, maxWidth: 820 }}>
      <If
        when={runs().length > 0}
        else={
          <PageEmpty
            icon="play"
            title="这个团队还没有跑过"
            description="启动一次，运行记录会展开成一条时间账。"
            primaryLabel="去启动前检查"
            onPrimary={() => teamSection.set("roster")}
            testID="team-runs-empty"
          />
        }
      >
        <View style={{ flexDirection: "column" }}>
          <Each of={runs()} key={(run) => run.start.sessionId}>
            {(run) => (
              <PressRow
                testID={`team-run-${run().start.sessionId}`}
                accessibilityLabel={`打开运行 ${run().start.sessionId}`}
                onPress={() => store.openTeamRun(team()?.teamBotId ?? "", run().start.sessionId)}
              >
                <Icon name="activity" size={14} tint={color.mutedForeground} />
                <View style={{ flex: 1, minWidth: 0 }}>
                  <Text style={{ fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                    运行 · {run().start.sessionId.slice(0, 8)}
                  </Text>
                  <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                    启动于 {formatWhen(run().startedAt)} · 队长会话
                  </Text>
                </View>
                <Icon name="chevron-right" size={12} tint={color.mutedForeground} />
              </PressRow>
            )}
          </Each>
        </View>
      </If>
      <StateNote
        tone="info"
        icon="info"
        text="这次浏览器打开期间启动的运行会列在这里。服务端还没有团队运行历史接口，所以旧的运行不会出现在列表里；运行本身仍然可以通过它的会话打开。"
      />
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, lineHeight: Math.round(typeScale.sm * lineHeight.normal) }}>
        技术详情：需要 GET /v1/team-bots/{team()?.teamBotId || "{id}"}/runs（运行 id、队长会话、状态、开始/结束、用量、失败摘要）。
      </Text>
    </View>
  );
}

function TeamRecordsSection() {
  const team = () => botAccess.teamDetail();
  return (
    <View testID="team-records" style={{ flexDirection: "column", gap: space.md, maxWidth: 720 }}>
      <PageUnavailable
        icon="activity"
        title="这个团队的操作记录在记录 05 里"
        description="启动、停止、名单变更都会写入同一本账。这里不复制一份，避免出现第二个真源。"
      >
        <Button
          testID="team-records-open"
          label="去记录里搜索这个团队"
          accessibilityLabel="去记录里搜索这个团队"
          onPress={() => {
            identityAccess.recordsQuery.set(team()?.name ?? "");
            store.openChapter("records");
          }}
          style={touchStyle()}
        />
      </PageUnavailable>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        记录支持按自由文本搜索操作者、对象与事件；搜索结果只覆盖已读取的时间范围。
      </Text>
    </View>
  );
}

// ---- dialogs and sheets ---------------------------------------------------

export function TeamDialogs() {
  return (
    <>
      <TeamLedgerSheet />
      <TeamArchiveDialog />
      <TeamStartDialog />
      <TeamAddMemberDialog />
      <TeamCaptainDialog />
      <TeamCreateDialog />
    </>
  );
}

function TeamLedgerSheet() {
  const changes = () => botAccess.teamChanges();
  const high = () => changes().filter((change) => change.highRisk);
  const allTicked = () => high().every((change) => teamConfirmHigh()[`${change.kind}-${change.memberId ?? ""}`] === true);
  const me = () => store.identity();
  const close = () => teamLedgerOpen.set(false);
  const submit = async () => {
    const result = await botAccess.saveTeamDraft();
    if (result.ok) {
      teamLedgerOpen.set(false);
      teamConfirmHigh.set({});
    }
  };
  return (
    <Sheet
      testID="team-ledger-sheet"
      open={teamLedgerOpen()}
      side={isMobile() ? "bottom" : "right"}
      style={isMobile() ? { height: 560, maxHeight: "88%" } : undefined}
      title={`确认 ${changes().length} 项变更`}
      description="登记前核对：这次变更会影响这个团队的下一次启动。"
      onOpenChange={(event) => {
        if (!event.open) close();
      }}
      footer={
        <>
          <Button variant="outline" label="取消" accessibilityLabel="取消登记" onPress={close} style={touchStyle()} />
          <Button
            testID="team-ledger-confirm"
            label={botAccess.teamSaving() ? "登记中…" : "确认并登记"}
            accessibilityLabel="确认并登记"
            loading={botAccess.teamSaving()}
            disabled={!allTicked() || botAccess.teamSaving()}
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
            本次登记人：{me()?.displayName || me()?.username || "你的账号"}
          </Text>
          <Each of={changes()} key={(change) => `${change.kind}-${change.memberId ?? ""}`}>
            {(change) => (
              <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.md, minHeight: 40, paddingVertical: space.xs, borderTopWidth: 1, borderTopColor: color.border }}>
                <View style={{ flex: 1, minWidth: 0 }}>
                  <Text style={{ fontSize: typeScale.base, color: color.foreground }}>
                    {change().label}
                    {change().memberId ? <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}> {change().memberId}</Text> : null}
                  </Text>
                  <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 2 }}>
                    {change().from} → <Text style={{ color: color.foreground }}>{change().to}</Text>
                  </Text>
                </View>
                <If when={change().highRisk}>
                  <Badge variant="destructive" label="高危" />
                </If>
              </View>
            )}
          </Each>
          <StateNote tone="info" icon="info" text="影响下一次启动 · 正在运行的会话不受影响 · 写入记录 05。" />
          <Each of={high()} key={(change) => `${change.kind}-${change.memberId ?? ""}`}>
            {(change) => (
              <Checkbox
                testID={`team-high-risk-${change().kind}`}
                checked={teamConfirmHigh()[`${change().kind}-${change().memberId ?? ""}`] === true}
                onChange={(checked) =>
                  teamConfirmHigh.update((map) => ({ ...map, [`${change().kind}-${change().memberId ?? ""}`]: checked }))
                }
                label={`我确认更换队长为「${change().to}」（高危）`}
                style={isMobile() ? { minHeight: 44 } : undefined}
              />
            )}
          </Each>
          <If when={high().length > 0 && !allTicked()}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>勾选后才能登记。</Text>
          </If>
          <If when={botAccess.teamSaveError() !== null}>
            <StateNote tone="danger" icon="alert" text={botAccess.teamSaveError() ?? ""} />
          </If>
        </View>
      </ScrollArea>
    </Sheet>
  );
}

function TeamArchiveDialog() {
  return (
    <AlertDialog
      testID="team-archive-dialog"
      open={teamArchiveOpen()}
      title="归档这个团队？"
      description="归档后不能再启动或修改名单；历史运行仍然保留。归档不能在这个页面撤销。"
      confirmLabel="归档"
      cancelLabel="取消"
      destructive
      onConfirm={() => {
        void botAccess.archiveTeam(botAccess.teamDetail()?.teamBotId ?? "").then((ok) => {
          if (ok) teamArchiveOpen.set(false);
        });
      }}
      onCancel={() => teamArchiveOpen.set(false)}
    />
  );
}

function TeamStartDialog() {
  const team = () => botAccess.teamDetail();
  const captain = () => botAccess.bots().find((bot) => bot.botId === captainSeat(team()?.roster ?? [])?.botId);
  return (
    <AlertDialog
      testID="team-start-dialog"
      open={teamStartOpen()}
      title="启动这个团队？"
      description={`队长 ${captain()?.name ?? "未设"} 会收到这句使命，并可以创建、分叉、调用成员会话。会产生模型用量；运行页可以随时停止。`}
      confirmLabel="启动"
      cancelLabel="取消"
      onConfirm={() => {
        teamStartOpen.set(false);
        void botAccess.startTeam(team()?.teamBotId ?? "").then((result) => {
          if (result?.sessionId) store.openTeamRun(team()?.teamBotId ?? "", result.sessionId);
        });
      }}
      onCancel={() => teamStartOpen.set(false)}
    />
  );
}

function TeamAddMemberDialog() {
  const draft = () => botAccess.teamDraft();
  const rosterIds = () => new Set((draft()?.roster ?? []).map((seat) => seat.botId));
  const query = () => teamMemberQuery().trim().toLowerCase();
  const candidates = () =>
    botAccess
      .bots()
      .filter((bot) => !rosterIds().has(bot.botId))
      .filter((bot) => !query() || [bot.name, accountLabelOf(bot), bot.modelKind].some((part) => part.toLowerCase().includes(query())));
  return (
    <Sheet
      testID="team-add-member-sheet"
      open={teamAddMemberOpen()}
      side={isMobile() ? "bottom" : "right"}
      style={isMobile() ? { height: 560, maxHeight: "88%" } : undefined}
      title="添加成员"
      description="从你能调用的 Bot 里选。团队不会因此获得它们的权限。"
      onOpenChange={(event) => {
        if (!event.open) teamAddMemberOpen.set(false);
      }}
      footer={<Button variant="outline" label="完成" accessibilityLabel="完成添加成员" onPress={() => teamAddMemberOpen.set(false)} style={touchStyle()} />}
    >
      <View style={{ flexDirection: "column", gap: space.md, flex: 1, minHeight: 0 }}>
        <Input
          testID="team-member-search"
          value={teamMemberQuery()}
          onChange={teamMemberQuery.set}
          placeholder="搜索 Bot 或账号"
          accessibilityLabel="搜索可加入的 Bot"
          size="sm"
          style={{ height: isMobile() ? 44 : undefined }}
        />
        <If when={!can("bot.invoke")}>
          <StateNote
            tone="warning"
            icon="lock"
            text="你没有「调用 Bot」能力，不能把成员加入团队。让 Bot 的归属者授予你「调用 Bot」后再试。"
          />
        </If>
        <ScrollArea style={{ flex: 1, minHeight: 0 }}>
          <If
            when={candidates().length > 0}
            else={<Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>没有可加入的 Bot：换个名字，或先到「员工」创建。</Text>}
          >
            <View style={{ flexDirection: "column" }}>
              <Each of={candidates()} key={(bot) => bot.botId}>
                {(bot) => {
                  const availability = () => invocationOf(bot());
                  const disabled = () => !can("bot.invoke") || bot().archived;
                  return (
                    <View
                      testID={`team-candidate-${bot().botId}`}
                      style={{ flexDirection: "row", alignItems: "center", gap: space.md, minHeight: 56, paddingVertical: space.sm, borderTopWidth: 1, borderTopColor: color.border }}
                    >
                      <BookAvatar name={bot().name} />
                      <View style={{ flex: 1, minWidth: 0 }}>
                        <Text style={{ fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>{bot().name}</Text>
                        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                          {accountLabelOf(bot())} · {modelKindLabel(bot().modelKind)}
                        </Text>
                      </View>
                      <Badge variant={availability().variant} label={availability().label} />
                      <Button
                        testID={`team-candidate-add-${bot().botId}`}
                        size={isMobile() ? "lg" : "sm"}
                        label="加入"
                        accessibilityLabel={`加入 ${bot().name}`}
                        disabled={disabled()}
                        onPress={() => {
                          botAccess.addRosterMember(bot().botId);
                          teamAddMemberOpen.set(false);
                        }}
                        style={touchStyle()}
                      />
                    </View>
                  );
                }}
              </Each>
            </View>
          </If>
        </ScrollArea>
        <If when={!can("bot.invoke")}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            服务端会在加入与启动时各核对一次「调用 Bot」；这里只是提前说明。
          </Text>
        </If>
      </View>
    </Sheet>
  );
}

function TeamCaptainDialog() {
  const draft = () => botAccess.teamDraft();
  const captain = () => captainSeat(draft()?.roster ?? []);
  const others = () => (draft()?.roster ?? []).filter((seat) => seat.botId !== captain()?.botId);
  return (
    <Dialog
      testID="team-captain-dialog"
      open={teamCaptainOpen()}
      title="更换队长"
      description="队长负责拆分任务、调用成员；它仍以自己的权限执行。"
      onOpenChange={(event) => {
        if (!event.open) teamCaptainOpen.set(false);
      }}
      footer={
        <>
          <Button variant="outline" label="取消" accessibilityLabel="取消更换队长" onPress={() => teamCaptainOpen.set(false)} style={touchStyle()} />
          <Button
            testID="team-captain-confirm"
            label="更换"
            accessibilityLabel="确认更换队长"
            disabled={teamCaptainChoice() === ""}
            onPress={() => {
              botAccess.setCaptain(teamCaptainChoice());
              teamCaptainOpen.set(false);
            }}
            style={touchStyle()}
          />
        </>
      }
    >
      <View style={{ flexDirection: "column", gap: space.md }}>
        <Select
          testID="team-captain-select"
          items={others().map((seat) => ({
            value: seat.botId,
            title: botAccess.botNameOf(seat.botId),
          }))}
          value={teamCaptainChoice()}
          onChange={(value) => teamCaptainChoice.set(value ?? "")}
          accessibilityLabel="新队长"
          height={isMobile() ? 44 : undefined}
        />
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          更换队长会改变下一次启动时谁负责拆分任务；原来的队长留在名单里，变成普通成员。
        </Text>
      </View>
    </Dialog>
  );
}

function TeamCreateDialog() {
  const captainOptions = () =>
    botAccess.bots().filter((bot) => !bot.archived).map((bot) => ({
      value: bot.botId,
      title: `${bot.name} · ${accountLabelOf(bot)}`,
    }));
  return (
    <Dialog
      testID="team-create-dialog"
      open={teamCreateOpen()}
      title="新建团队"
      description="一句使命 + 一名队长；其他成员可以在创建后加入。"
      size="lg"
      onOpenChange={(event) => {
        if (!event.open) teamCreateOpen.set(false);
      }}
      footer={
        <>
          <Button variant="outline" label="取消" accessibilityLabel="取消新建团队" onPress={() => teamCreateOpen.set(false)} style={touchStyle()} />
          <Button
            testID="team-create-confirm"
            label="创建团队"
            accessibilityLabel="创建团队"
            disabled={teamCreateName().trim() === "" || teamCreateMission().trim() === "" || teamCreateCaptain() === "" || !can("bot.invoke")}
            onPress={() => {
              void botAccess.createTeam({
                name: teamCreateName().trim(),
                mission: teamCreateMission().trim(),
                roster: [{ botId: teamCreateCaptain(), role: "captain" }],
              }).then((created) => {
                if (!created) return;
                teamCreateOpen.set(false);
                store.openTeam(created.teamBotId);
              });
            }}
            style={touchStyle()}
          />
        </>
      }
    >
      <ScrollArea maxHeight={isMobile() ? 420 : 520} style={{ flexDirection: "column" }}>
        <View style={{ flexDirection: "column", gap: space.md }}>
          <View style={{ flexDirection: "column", gap: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>团队名</Text>
            <Input testID="team-create-name" value={teamCreateName()} onChange={teamCreateName.set} placeholder="客服团队" accessibilityLabel="团队名" />
          </View>
          <View style={{ flexDirection: "column", gap: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>使命</Text>
            <Textarea
              testID="team-create-mission"
              value={teamCreateMission()}
              onChange={teamCreateMission.set}
              rows={5}
              placeholder="在 24 小时内解决客户问题；无法解决时升级给人工。"
              accessibilityLabel="团队使命"
            />
          </View>
          <View style={{ flexDirection: "column", gap: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>队长</Text>
            <Select
              testID="team-create-captain"
              items={captainOptions()}
              value={teamCreateCaptain()}
              searchable
              onChange={(value) => teamCreateCaptain.set(value ?? "")}
              accessibilityLabel="团队队长"
              height={isMobile() ? 44 : undefined}
            />
            <If when={!can("bot.invoke")}>
              <StateNote tone="warning" icon="lock" text="创建团队需要你能调用队长：先获得「调用 Bot」能力。" />
            </If>
          </View>
          <If when={botAccess.teamActionError() !== null}>
            <StateNote tone="danger" icon="alert" text={botAccess.teamActionError() ?? ""} />
          </If>
        </View>
      </ScrollArea>
    </Dialog>
  );
}
