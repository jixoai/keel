// The session header's team face (change keel-agent-team-v1, P4 tail).
//
// A team session is not just "a model with another name": the header names
// the team, the captain and the model the next input runs, and it carries the
// one entry into the roster. The facts come from the session's own marker
// (`store.sessionTeam`) plus the agent registry the picker already loaded;
// nothing here writes a document or invents a member.
//
// The header stays inside one h-12 row at every width: the team name is the
// chip's label when the row has room, and moves into the title's second line
// at the single column, where the two entries become icon-only (a terminal
// keeps a short text label — an icon-only view has no characters there). The
// captain and the model always ride the title's second line, where a narrow
// column truncates the tail instead of pushing the session title out of the
// row.
import { signal, Text, View } from "nsh";
import type { AgentTeamView, AgentView, TeamMemberView } from "../data/contract";
import { store } from "../data/store";
import { selectionDisplay } from "../data/wire";
import { Icon } from "../ui/icons";
import {
  Avatar,
  Badge,
  Button,
  Dialog,
  IconButton,
  ScrollArea,
  Separator,
  Skeleton,
  Tooltip,
  color,
  iconSize,
  lineHeight,
  space,
  typeScale,
  weight,
} from "../ui/kit";
import { openModelPicker } from "./model-picker";

/** The roster dialog's open state: a header-local fact, not a store document. */
const rosterOpen = signal(false);

const openRoster = () => {
  // The names come from the agent registry; the picker normally loaded it
  // already, but a session restored without one still gets its roster read.
  if (store.agents().length === 0 && !store.agentsLoading()) void store.loadAgents();
  rosterOpen.set(true);
};

export const teamMemberCount = (team: AgentTeamView | null): number => team?.members?.length ?? 0;

const storedAgent = (agentId: string): AgentView | undefined =>
  store.agents().find((agent) => agent.id === agentId);

/** The captain's display name: the inline definition, the stored agent, the
 * seat's own label, else a readable "not loaded" word. Never a raw agent id. */
export const captainNameOf = (team: AgentTeamView): string => {
  const seat = team.captain;
  return seat?.inline?.name || storedAgent(seat?.agentId ?? "")?.name || seat?.label || "未载入的队长";
};

/** The effective model of the open team session: the session's selection (the
 * captain's default model applied on start), else the tenant default. */
export const teamModelLabel = (): string => {
  const effective = store.sessionSelection() ?? store.tenantSelection();
  return selectionDisplay(effective) || "模型未设置";
};

const memberNameOf = (member: TeamMemberView): string =>
  member.inline?.name || storedAgent(member.agentId)?.name || member.label || "未载入的成员";

const memberDescriptionOf = (member: TeamMemberView): string =>
  member.description || member.inline?.description || storedAgent(member.agentId)?.description || "";

/** The title column's second line: the captain and the model. At the single
 * column the chip loses its text, so the team name moves here too — the header
 * never relies on an icon alone to say which team is running. */
export function SessionTeamFacts(props: { compact?: boolean }) {
  const team = () => store.sessionTeam();
  const line = () => {
    const current = team();
    if (!current) return "";
    const facts = `队长 ${captainNameOf(current)} · ${teamModelLabel()}`;
    return props.compact === true ? `${current.name} · ${facts}` : facts;
  };
  return (
    <If when={!!team()}>
      <Text
        testID="workspace-header-team-facts"
        style={{
          numberOfLines: 1,
          textOverflow: "ellipsis",
          fontSize: typeScale.xs,
          lineHeight: Math.round(typeScale.xs * lineHeight.normal),
          color: color.mutedForeground,
        }}
      >
        {line()}
      </Text>
    </If>
  );
}

/** A standard session's own second line at the single column: the chip there
 * is icon-only, and "Standard shows the running model" must not depend on a
 * tooltip. At the wide column the chip's text carries the same fact, so this
 * renders nothing (no duplicate). */
export function SessionModelFacts(props: { compact?: boolean }) {
  const team = () => store.sessionTeam();
  const visible = () => props.compact === true && !team() && !!store.activeId();
  const line = () => selectionDisplay(store.sessionSelection() ?? store.tenantSelection()) || "模型未设置";
  return (
    <If when={visible()}>
      <Text
        testID="workspace-header-model-facts"
        style={{
          numberOfLines: 1,
          textOverflow: "ellipsis",
          fontSize: typeScale.xs,
          lineHeight: Math.round(typeScale.xs * lineHeight.normal),
          color: color.mutedForeground,
        }}
      >
        {line()}
      </Text>
    </If>
  );
}

/** The team's chip (opens the picker, where the mode/team is chosen) and the
 * member entry. Both are kit controls; the count is the roster's own fact.
 *
 * `compact` is the single column's short form. On a drawn platform that means
 * two icon-only buttons with the full facts in their accessibility labels and
 * the title's second line; on a terminal it means short text labels (`团队`,
 * `成员 N`) because an icon-only view has no characters there. */
export function SessionTeamChip(props: { compact?: boolean; iconOnly?: boolean }) {
  const team = () => store.sessionTeam();
  const count = () => teamMemberCount(team());
  return (
    <If when={!!team()}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <If
          when={props.iconOnly === true}
          else={
            <Button
              testID="session-model-chip"
              variant="outline"
              size="sm"
              icon="layers"
              iconTrailing="chevron-down"
              label={props.compact === true ? "团队" : `团队 · ${team()?.name ?? ""}`}
              selected
              accessibilityLabel={`本会话团队：${team()?.name ?? ""}（团队模式）`}
              tooltip={<Tooltip label={`启动模式：团队 · 队长 ${captainNameOf(team()!)} · 模型 ${teamModelLabel()}`} />}
              onPress={openModelPicker}
            />
          }
        >
          <IconButton
            testID="session-model-chip"
            variant="outline"
            icon="layers"
            selected
            label={`本会话团队：${team()?.name ?? ""}（团队模式）`}
            onPress={openModelPicker}
          />
        </If>
        <If
          when={props.iconOnly === true}
          else={
            <Button
              testID="session-team-members"
              variant="outline"
              size="sm"
              icon="users"
              label={`成员 ${count()}`}
              accessibilityLabel={`查看团队成员（${count()} 人）`}
              onPress={openRoster}
            />
          }
        >
          <IconButton
            testID="session-team-members"
            variant="outline"
            icon="users"
            label={`查看团队成员（${count()} 人）`}
            onPress={openRoster}
          />
        </If>
      </View>
    </If>
  );
}

function MemberRow(props: { member: TeamMemberView; captain: boolean }) {
  const name = () => memberNameOf(props.member);
  const description = () => memberDescriptionOf(props.member);
  return (
    <View
      testID={`team-member-${props.member.agentId || "inline"}`}
      style={{ flexDirection: "row", alignItems: "flex-start", gap: space.md }}
    >
      <Avatar src={props.member.inline?.avatar?.url ?? null} name={name()} size="default" />
      <View style={{ flex: 1, flexDirection: "column", gap: space.xxs }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>{name()}</Text>
          <If when={props.captain}>
            <Badge variant="default" label="队长" />
          </If>
        </View>
        <If when={description() !== ""} else={<Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>还没有描述</Text>}>
          <Text
            style={{
              fontSize: typeScale.xs,
              lineHeight: Math.round(typeScale.xs * lineHeight.normal),
              color: color.mutedForeground,
            }}
          >
            {description()}
          </Text>
        </If>
      </View>
    </View>
  );
}

/** The roster the header's member entry opens: the captain's mandatory seat,
 * then the members (0..N). Reads the same registry as the settings section;
 * a load failure is the dialog's own reason, never a silently empty team. */
export function SessionTeamRosterDialog() {
  const team = () => store.sessionTeam();
  const members = () => team()?.members ?? [];
  const loading = () => store.agentsLoading() && store.agents().length === 0;
  const denied = () => store.agentsErrorStatus() === 403;
  return (
    <Dialog
      testID="team-roster-dialog"
      open={rosterOpen() && !!team()}
      size="default"
      title={`团队 · ${team()?.name ?? ""}`}
      description="队长带队，成员是队长的协作者。成员的增删在设置 › 团队里完成；这里只读。"
      onOpenChange={(event) => {
        if (!event.open) rosterOpen.set(false);
      }}
      footer={<Button variant="ghost" label="关闭" onPress={() => rosterOpen.set(false)} />}
    >
      <ScrollArea testID="team-roster-list" maxHeight={360}>
        <View style={{ flexDirection: "column", gap: space.md }}>
          <Text style={{ fontSize: typeScale.xs, fontWeight: weight.medium, color: color.mutedForeground }}>队长</Text>
          <MemberRow member={team()?.captain ?? { agentId: "" }} captain />
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
            <Badge
              variant="outline"
              icon="brain"
              truncate
              style={{ maxWidth: "100%" }}
              label={`运行模型 ${teamModelLabel()}`}
            />
          </View>
          <Separator />
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
            <Text style={{ fontSize: typeScale.xs, fontWeight: weight.medium, color: color.mutedForeground }}>
              成员
            </Text>
            <Badge variant="secondary" label={`${members().length} 人`} />
          </View>
          <If when={loading()}>
            <View testID="team-roster-loading" style={{ flexDirection: "column", gap: space.sm }}>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>正在读取成员资料…</Text>
              <Skeleton lines={2} />
            </View>
          </If>
          <If when={!!store.agentsError()}>
            <View
              testID={denied() ? "team-roster-permission" : "team-roster-error"}
              style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm }}
            >
              <Icon name={denied() ? "key" : "alert"} size={iconSize.sm} tint={denied() ? color.warning : color.danger} />
              <Text
                style={{
                  flex: 1,
                  fontSize: typeScale.sm,
                  lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                  color: color.foreground,
                }}
              >
                {denied()
                  ? `没有查看成员资料的权限：${store.agentsError() ?? ""}`
                  : `成员资料读取失败：${store.agentsError() ?? ""}`}
              </Text>
              <Button
                variant="outline"
                size="sm"
                label="重试"
                accessibilityLabel="重试读取成员资料"
                onPress={() => void store.loadAgents()}
              />
            </View>
          </If>
          <If when={!loading() && !store.agentsError() && members().length === 0}>
            <Text
              style={{
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                color: color.mutedForeground,
              }}
            >
              这支团队只有队长：没有额外成员，队长会自己完成工作。
            </Text>
          </If>
          <If when={!loading() && !store.agentsError()}>
            <Each of={members()} key={(member) => member.agentId || "inline"}>
              {(member) => (
                <View style={{ flexDirection: "column", gap: space.md }}>
                  <MemberRow member={member()} captain={false} />
                </View>
              )}
            </Each>
          </If>
        </View>
      </ScrollArea>
    </Dialog>
  );
}
