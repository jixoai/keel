// The books chapters — change keel-bot-identity-access, A4.4a/A4.4b.
//
// 02 员工 and 03 团队 are their own chapters (A4.4b); 04 身份 and 记录 05 are
// A4.4a's. The router only picks the chapter; each chapter gates on its own
// capability, so a deep link to a page the account may not read renders the
// four-part permission page instead of a blank shell.
import { Text, View } from "nsh";
import { Button, EmptyContent, color, lineHeight, space, typeScale } from "../../ui/kit";
import { store } from "../../data/store";
import { canAny } from "../../data/permissions";
import { PagePermission, PageUnavailable, touchStyle } from "./common";
import { Paper } from "./shell";
import { IdentityChapter } from "./identity";
import { RecordsChapter } from "./records";
import { BotsChapter } from "./bots";
import { TeamsChapter } from "./teams";
import { TeamRunPage } from "./run";

/** 03 团队 — the chapter's own capability gate when the chapter component is
 * not mounted (kept for the route router below). */
function TeamsDenied() {
  return <PagePermission capability="查看团队" capabilityKey="team_bot.read" />;
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
    <If when={route() === "bots"} else={<NotBots />}>
      <BotsChapter />
    </If>
  );
}

function NotBots() {
  const route = () => store.route();
  const runId = () => store.routeRunId();
  return (
    <If when={route() === "teams"} else={<Fallback />}>
      <If when={canAny(["team_bot.read", "team_bot.manage"])} else={<TeamsDenied />}>
        <If when={runId() !== null} else={<TeamsChapter />}>
          <TeamRunPage teamId={store.routeObjectId() ?? ""} runId={runId() ?? ""} />
        </If>
      </If>
    </If>
  );
}

function Fallback() {
  return (
    <Paper maxWidth={720}>
      <View style={{ flex: 1, minHeight: 0, alignItems: "center", justifyContent: "center" }}>
        <PageUnavailable
          icon="layers"
          title="这个页面不在当前章节里"
          description="回到工作区，或从顶部章节索引进入员工、团队、身份与记录。"
        >
          <EmptyContent>
            <Button label="返回工作区" accessibilityLabel="返回工作区" onPress={() => store.openChapter("workspace")} style={touchStyle()} />
          </EmptyContent>
          <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground, textAlign: "center" }}>
            页面地址没有对应到已知章节；你的数据没有被修改。
          </Text>
        </PageUnavailable>
      </View>
    </Paper>
  );
}
