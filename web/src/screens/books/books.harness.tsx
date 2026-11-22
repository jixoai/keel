// TUI harness · the identity/records chapters in their non-success states —
// change keel-bot-identity-access, A4.4a.
//
// A harness is needed because a test's module graph is not the app's: the seam
// has to be installed inside the launched app. The scenario arrives in the
// launch parameters (`host.params.scenario`):
//
//   loading       the account read never settles
//   empty         no accounts and no groups
//   error         the account read answers 500
//   permission    the caller holds no identity key (the chapter gate)
//   records-error the records chapter whose audit read answers 500
//   unavailable   the records chapter with no audit read surface
//   records       the records chapter with a small, real ledger
//
// The success faces are driven by the app's own fixture transport
// (`?fixture=1` / `host.params.fixture`), not here.
import { host, mount, View } from "nsh";
import { setEffectivePermissions } from "../../data/permissions";
import { KeelApiError } from "../../data/http";
import type { IdentityApi } from "../../data/identity-api";
import type { AccountView, AuditEventView, PermissionGroupSummary, PermissionScope } from "../../data/identity";
import { identityAccess } from "../../data/identity-access";
import { store } from "../../data/store";
import { BooksChrome } from "./shell";
import { color } from "../../ui/kit";
import { IdentityChapter } from "./identity";
import { RecordsChapter } from "./records";

const scenario = (): string => String(host.params.scenario ?? "empty");
const chapter = (): string => String(host.params.chapter ?? "identity");
const identitySection = (): "accounts" | "groups" => (host.params.section === "groups" ? "groups" : "accounts");
const identityObject = (): string | null =>
  typeof host.params.object === "string" && host.params.object !== "" ? host.params.object : null;
const recordsChapter = (): boolean => chapter() === "records";

const pageOf = <T,>(items: T[]) => ({ items, limit: 100, hasMore: false });

const account = (username: string, displayName: string): AccountView => ({
  accountId: `acc-${username}`,
  username,
  displayName,
  kind: "human",
  isSuperAdmin: false,
});

const group: PermissionGroupSummary = { groupId: "grp-support", name: "客服组", builtin: false };

const event: AuditEventView = {
  eventId: "ev-1",
  occurredAt: new Date().toISOString(),
  kind: "deny",
  action: "deny",
  summary: "调用 Bot 被拒",
  actorName: "客服一号",
  actorKind: "bot",
  objectType: "Bot",
  objectName: "售后升级",
  result: "denied",
  requestId: "k-7f31",
};

const apiFor = (name: string): IdentityApi => ({
  async listAccounts() {
    if (name === "loading") return new Promise(() => {});
    if (name === "error") throw new KeelApiError(500, "服务暂时没有响应", [], undefined, undefined, {}, "req-42");
    if (name === "empty") return pageOf([]);
    return pageOf([account("lina", "李娜"), account("chenmo", "陈墨")]);
  },
  async listPermissionGroups() {
    if (name === "error") throw new KeelApiError(500, "服务暂时没有响应", [], undefined, undefined, {}, "req-42");
    return pageOf(name === "empty" || name === "groups-empty" ? [] : [group]);
  },
  async getPermissionGroup(groupId: string) {
    if (name === "group-loading") return new Promise(() => {});
    if (name === "empty" || groupId !== group.groupId) throw new KeelApiError(404, "没有找到这个权限组。");
    if (name === "group-error") throw new KeelApiError(500, "服务暂时没有响应");
    return {
      group,
      grants: name === "group-empty" ? [] : [{ key: "session.read", scope: "team" }],
      members: [{ accountId: account("lina", "李娜").accountId }],
    };
  },
  async listBots() {
    return pageOf([]);
  },
  ...(name === "unavailable"
    ? {}
    : {
        async listAuditEvents() {
          if (name === "records-error") throw new KeelApiError(500, "服务暂时没有响应");
          return pageOf(name === "empty" ? [] : [event]);
        },
      }),
});

const ALL_KEYS: Record<string, PermissionScope> = {
  "account.read": "tenant",
  "account.manage": "tenant",
  "account.credential.reset": "tenant",
  "account.permission.grant": "tenant",
  "permission.read": "tenant",
  "permission.manage": "tenant",
  "audit.read": "tenant",
  "session.read": "team",
};

const readonlyKeys = (): Record<string, PermissionScope> => {
  const keys = { ...ALL_KEYS };
  delete keys["permission.manage"];
  return keys;
};

identityAccess.resetForTest();
// The harness owns its transport and keys: cancel the store's boot probe so a
// late answer cannot reset the scenario (the probe checks its token after every
// await).
store.skipBootForTest();
store.setIdentityTransport(apiFor(scenario()));
if (recordsChapter()) {
  store.openChapter("records");
} else {
  store.openIdentitySection(identitySection());
  if (identityObject() !== null) store.openIdentityObject(identitySection(), identityObject()!);
}
setEffectivePermissions(
  scenario() === "permission"
    ? { "session.read": "team" as PermissionScope }
    : scenario() === "group-readonly"
      ? readonlyKeys()
      : ALL_KEYS,
  false,
);

function Harness() {
  return (
    <View style={{ flex: 1, minHeight: 0, backgroundColor: color.background }}>
      <BooksChrome>
        <If when={recordsChapter()} else={<IdentityChapter />}>
          <RecordsChapter />
        </If>
      </BooksChrome>
    </View>
  );
}

mount(Harness);
