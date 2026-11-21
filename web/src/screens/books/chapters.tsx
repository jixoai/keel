// The books chapters this slice does not build yet — change
// keel-bot-identity-access, A4.4a.
//
// A4.4b owns the Bot index/detail/try-run and the TeamBot/TeamRun pages. This
// slice keeps their chapter entries real (they appear only when the caller holds
// the chapter's key) and their content honest: the page says the pages are not
// in this slice and offers the next useful step. No sample Bot, team or run is
// rendered in their place.
import { Text, View } from "nsh";
import { Button, EmptyContent, color, lineHeight, space, typeScale } from "../../ui/kit";
import { store } from "../../data/store";
import { canAny } from "../../data/permissions";
import { PagePermission, PageUnavailable, touchStyle } from "./common";
import { Paper } from "./shell";
import { IdentityChapter } from "./identity";
import { RecordsChapter } from "./records";

/** 02 员工 — the Bot chapter's pending page. */
function BotsPending() {
  if (!canAny(["bot.read", "bot.manage"])) {
    return <PagePermission capability="查看 Bot" capabilityKey="bot.read" />;
  }
  return (
    <PageUnavailable
      testID="bots-pending"
      icon="bot"
      title="员工章在这一片的下一页"
      description="Bot 的索引、详情（身份 / 工作方式 / 能力 / 密钥 / 会话）与试跑由 A4.4b 落地。这一片只保留章节入口，不显示示例 Bot。"
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
      <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground, textAlign: "center" }}>
        Bot 属于一个账号，按账号范围 ∩ Bot 授权执行；这两层都在 A4.4b 的 Bot 页里解释。
      </Text>
    </PageUnavailable>
  );
}

/** 03 团队 — the TeamBot/TeamRun chapter's pending page. */
function TeamsPending() {
  if (!canAny(["team_bot.read", "team_bot.manage"])) {
    return <PagePermission capability="查看团队" capabilityKey="team_bot.read" />;
  }
  return (
    <PageUnavailable
      testID="teams-pending"
      icon="users"
      title="团队与运行在这一片的下一页"
      description="TeamBot 的使命、名单、启动检查与 TeamRun 的泳道 DAG 由 A4.4b 落地。这一片只保留章节入口，不显示示例团队或运行。"
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
      <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground, textAlign: "center" }}>
        团队本身不拥有权限，也不拥有会话：每个成员 Bot 按自己的账号与工作范围执行。
      </Text>
    </PageUnavailable>
  );
}

/** The route's chapter body (the workspace and the settings/plugin/kit pages are
 * App.tsx's own cases). */
export function BooksChapterRoute() {
  const route = () => store.route();
  return (
    <View testID="books-chapter" style={{ flex: 1, minHeight: 0, flexDirection: "row" }}>
      <If when={route() === "identity"} else={<NotIdentity />}>
        <IdentityChapter />
      </If>
    </View>
  );
}

function NotIdentity() {
  const route = () => store.route();
  return (
    <If when={route() === "records"} else={<NotRecords />}>
      <RecordsChapter />
    </If>
  );
}

function NotRecords() {
  const route = () => store.route();
  return (
    <Paper maxWidth={720}>
      <View style={{ flex: 1, minHeight: 0, alignItems: "center", justifyContent: "center" }}>
        <If when={route() === "teams"} else={<BotsPending />}>
          <TeamsPending />
        </If>
      </View>
    </Paper>
  );
}
