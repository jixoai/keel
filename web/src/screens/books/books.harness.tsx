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
import { effect, host, mount, signal, View } from "nsh";
import { setEffectivePermissions } from "../../data/permissions";
import { applyAppearance } from "../../ui/appearance";
import { KeelApiError } from "../../data/http";
import type { IdentityApi } from "../../data/identity-api";
import type { AccountView, AuditEventView, PermissionGroupSummary, PermissionScope } from "../../data/identity";
import { identityAccess } from "../../data/identity-access";
import { store } from "../../data/store";
import { BooksChrome } from "./shell";
import { color } from "../../ui/kit";
import { IdentityChapter } from "./identity";
import { RecordsChapter } from "./records";

interface HarnessParams {
  scenario?: string;
  chapter?: string;
  section?: string;
  object?: string;
}

/** The TUI test host passes its parameters directly; the web dev host has no
 * `location` in the app sandbox, so a browser launch reads the same names from
 * `keel.route`'s query string. */
const directParams = (host.params ?? {}) as HarnessParams;
const params = signal<HarnessParams | null>(directParams.scenario !== undefined ? directParams : null);

/** A web evidence entry sets the same parameters the TUI host passes. The app
 * sandbox has no `location`, so the wrapper reads its own scenario and calls
 * this before the first read. */
export function setHarnessParams(next: HarnessParams): void {
  params.set(next);
}

const scenarioOf = (p: HarnessParams | null): string => String(p?.scenario ?? "empty");
const chapterOf = (p: HarnessParams | null): string => String(p?.chapter ?? "identity");
const identitySectionOf = (p: HarnessParams | null): "accounts" | "groups" =>
  p?.section === "groups" ? "groups" : "accounts";
const identityObjectOf = (p: HarnessParams | null): string | null =>
  typeof p?.object === "string" && p.object !== "" ? p.object : null;
const recordsChapterOf = (p: HarnessParams | null): boolean => chapterOf(p) === "records";

const scenario = (): string => scenarioOf(params());
const identitySection = (): "accounts" | "groups" => identitySectionOf(params());
const identityObject = (): string | null => identityObjectOf(params());
const recordsChapter = (): boolean => recordsChapterOf(params());

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
    if (name === "accounts-forbidden") {
      throw new KeelApiError(403, "你没有查看账号的权限。", [], undefined, undefined, {
        reason: "missing_grant",
        requiredKey: "account.read",
      });
    }
    if (name === "empty") return pageOf([]);
    return pageOf([account("lina", "李娜"), account("chenmo", "陈墨")]);
  },
  async listPermissionGroups() {
    if (name === "error") throw new KeelApiError(500, "服务暂时没有响应", [], undefined, undefined, {}, "req-42");
    if (name === "groups-forbidden") {
      throw new KeelApiError(403, "你没有查看权限组的权限。", [], undefined, undefined, {
        reason: "missing_grant",
        requiredKey: "permission.read",
      });
    }
    return pageOf(name === "empty" || name === "groups-empty" ? [] : [group]);
  },
  async getPermissionGroup(groupId: string) {
    if (name === "group-loading") return new Promise(() => {});
    if (name === "groups-forbidden" || name === "group-forbidden") {
      throw new KeelApiError(403, "你没有查看权限组的权限。", [], undefined, undefined, {
        reason: "missing_grant",
        requiredKey: "permission.read",
      });
    }
    if (name === "empty" || groupId !== group.groupId) throw new KeelApiError(404, "没有找到这个权限组。");
    if (name === "group-error") throw new KeelApiError(500, "服务暂时没有响应", [], undefined, undefined, {}, "req-group");
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
          if (name === "records-loading") return new Promise(() => {});
          if (name === "records-error") throw new KeelApiError(500, "服务暂时没有响应", [], undefined, undefined, {}, "req-records");
          if (name === "records-forbidden") {
            throw new KeelApiError(403, "你没有查看记录的权限。", [], undefined, undefined, {
              reason: "missing_grant",
              requiredKey: "audit.read",
            });
          }
          return pageOf(name === "empty" ? [] : [event]);
        },
      }),
});

const ALL_KEYS: Record<string, PermissionScope> = {
  "account.read": "tenant",
  "account.manage": "tenant",
  "bot.read": "tenant",
  "team_bot.read": "tenant",
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

let configured = false;

/** Install the harness's transport, route and keys once the parameters are
 * known (immediately in the TUI, after `keel.route` in a browser). */
function configure(p: HarnessParams): void {
  if (configured) return;
  configured = true;
  identityAccess.resetForTest();
  // The harness owns its transport and keys: cancel the store's boot probe so a
  // late answer cannot reset the scenario (the probe checks its token after
  // every await).
  store.skipBootForTest();
  store.setIdentityTransport(apiFor(scenarioOf(p)));
  applyAppearance("dark");
  store.identity.set({
    tenantId: "tenant-harness",
    accountId: "acc-lina",
    username: "lina",
    displayName: "李娜",
    kind: "human",
    disabled: false,
    isSuperAdmin: false,
    permissions: {},
  });
  if (recordsChapterOf(p)) {
    store.openChapter("records");
  } else {
    store.openIdentitySection(identitySectionOf(p));
    const object = identityObjectOf(p);
    if (object !== null) store.openIdentityObject(identitySectionOf(p), object);
  }
  setEffectivePermissions(
    scenarioOf(p) === "permission"
      ? { "session.read": "team" as PermissionScope }
      : scenarioOf(p) === "group-readonly"
        ? readonlyKeys()
        : ALL_KEYS,
    false,
  );
}

if (directParams.scenario !== undefined) configure(directParams);

function Harness() {
  effect(() => {
    const p = params();
    if (p !== null) configure(p);
  });
  return (
    <If when={params() !== null} else={<View style={{ flex: 1, backgroundColor: color.background }} />}>
      <View style={{ flex: 1, minHeight: 0, backgroundColor: color.background }}>
        <BooksChrome>
          <If when={recordsChapter()} else={<IdentityChapter />}>
            <RecordsChapter />
          </If>
        </BooksChrome>
      </View>
    </If>
  );
}

mount(Harness);
