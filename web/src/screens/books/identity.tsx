// The 04 身份 chapter — change keel-bot-identity-access, A4.4a.
//
// One exception to the shell's drill-in (design §2.2): at ≥1440 identity keeps a
// 280px index column, so switching between accounts/groups never costs a trip
// back to an index page. Below that the same index is the page (full-width
// table/cards) and an object opens as a page with a back row. The 账号/权限组
// segment is always one tap away.
//
// The chapter gates on the capability, not on the route: a deep link to an
// object the account may not read renders the four-part permission page.
import { effect, View } from "nsh";
import { color, space } from "../../ui/kit";
import { store } from "../../data/store";
import { canAny } from "../../data/permissions";
import { identityAccess } from "../../data/identity-access";
import {
  AccountConfirmations,
  AccountPage,
  AccountsIndex,
  AccountsPage,
  AccountsWelcome,
  CreateAccountDialog,
  preferredAccountId,
} from "./accounts";
import { GroupDialogs, GroupPage, GroupsIndex, GroupsPage, GroupsWelcome, preferredGroupId } from "./groups";
import { IdentitySegmented, Kicker, PageLoading, PagePermission, isWide } from "./common";
import { IndexColumn, Paper } from "./shell";

/** The chapter's own capability gate (the shell hides the tab, a deep link
 * still lands here). */
export function IdentityChapter() {
  const section = () => store.routeSection();
  const objectId = () => store.routeObjectId();
  const canAccounts = () => canAny(["account.read", "account.manage"]);
  const canGroups = () => canAny(["permission.read", "permission.manage"]);
  const allowed = () => (section() === "groups" ? canGroups() : canAccounts());
  effect(() => {
    if (!allowed()) {
      // A reader with only one of the two sub-pages lands on the one they may
      // read; an explicit deep link to an object keeps its own permission page.
      if (objectId() === null) {
        if (section() === "accounts" && canGroups()) store.openIdentitySection("groups");
        else if (section() === "groups" && canAccounts()) store.openIdentitySection("accounts");
      }
      return;
    }
    identityAccess.ensureIndexes();
  });
  return (
    <View testID="identity-chapter" style={{ flex: 1, minHeight: 0, flexDirection: "row" }}>
      <If
        when={allowed()}
        else={
          <View style={{ flex: 1, minHeight: 0, justifyContent: "center" }}>
            <PagePermission
              capability={section() === "groups" ? "查看权限组" : "查看账号"}
              capabilityKey={section() === "groups" ? "permission.read" : "account.read"}
            />
          </View>
        }
      >
        <If when={isWide()} else={<NarrowIdentity />}>
          <IndexColumn>
            <View style={{ paddingHorizontal: space.lg, paddingTop: space.lg, paddingBottom: space.md, gap: space.md }}>
              <Kicker icon="shield" text="身份" />
              <IdentitySegmented section={section()} groupCount={identityAccess.groups().length || undefined} />
            </View>
            <If when={section() === "accounts"} else={<GroupsIndex />}>
              <AccountsIndex />
            </If>
          </IndexColumn>
          <Paper>
            <If when={section() === "accounts"}>
              <If
                when={(objectId() ?? preferredAccountId()) !== null}
                else={
                  <If
                    when={identityAccess.accountsLoading() && identityAccess.accounts().length === 0}
                    else={<View style={{ flex: 1, justifyContent: "center" }}><AccountsWelcome /></View>}
                  >
                    <PageLoading rows={5} testID="identity-paper-loading" />
                  </If>
                }
              >
                <AccountPage accountId={objectId() ?? preferredAccountId() ?? ""} showBack={false} />
              </If>
            </If>
            <If when={section() === "groups"}>
              <If
                when={(objectId() ?? preferredGroupId()) !== null}
                else={
                  <If
                    when={identityAccess.groupsLoading() && identityAccess.groups().length === 0}
                    else={<View style={{ flex: 1, justifyContent: "center" }}><GroupsWelcome /></View>}
                  >
                    <PageLoading rows={5} testID="identity-groups-loading" />
                  </If>
                }
              >
                <GroupPage groupId={objectId() ?? preferredGroupId() ?? ""} showBack={false} />
              </If>
            </If>
          </Paper>
        </If>
        {/* The chapter's dialogs live here, outside the two layouts: the index's
            create actions must work before any object is open. */}
        <CreateAccountDialog />
        <AccountConfirmations />
        <GroupDialogs />
      </If>
    </View>
  );
}

/** 1024–1439 and ≤767: the index is the page; an object opens over it. */
function NarrowIdentity() {
  const section = () => store.routeSection();
  const objectId = () => store.routeObjectId();
  return (
    <Paper>
      <View style={{ width: 220, paddingBottom: space.md }}>
        <IdentitySegmented section={section()} groupCount={identityAccess.groups().length || undefined} />
      </View>
      <If when={section() === "accounts"} else={<NarrowGroups />}>
        <If when={objectId() !== null} else={<AccountsPage />}>
          <AccountPage accountId={objectId() ?? ""} showBack />
        </If>
      </If>
    </Paper>
  );
}

function NarrowGroups() {
  const objectId = () => store.routeObjectId();
  return (
    <If when={objectId() !== null} else={<GroupsPage />}>
      <GroupPage groupId={objectId() ?? ""} showBack />
    </If>
  );
}
