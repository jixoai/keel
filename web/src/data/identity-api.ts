// The identity/access transport shape — change keel-bot-identity-access,
// A4.4a.
//
// One interface, one method per server route the identity pages read or write.
// It lives apart from the screens and from the app store so both can name it
// without importing each other: the live transport is `KeelHttp` (whose methods
// already exist since A4.3), the development fixture answers it in memory, and a
// test may install its own through `identityAccess.setTransport`.
//
// The record chapter's read is optional on purpose: the route has not landed
// yet. A transport without it answers nothing, and the page renders its 待接入
// state instead of an error about a surface the deployment never had.
import type { AuditEventView, AuditQuery, BotView, CreateAccountInput, IdentityPage, PermissionGroupDetail, PermissionGroupSummary, AccountView, WhoAmI } from "./identity";

export interface IdentityApi {
  whoami?(): Promise<WhoAmI>;

  listAccounts?(limit?: number): Promise<IdentityPage<AccountView>>;
  createAccount?(input: CreateAccountInput): Promise<AccountView>;
  getAccount?(accountId: string): Promise<{ account: AccountView; groups: PermissionGroupSummary[] }>;
  setAccountDisabled?(accountId: string, disabled: boolean, reason?: string): Promise<AccountView>;
  resetAccountPassword?(accountId: string, password: string): Promise<void>;
  addAccountGroup?(accountId: string, groupId: string): Promise<void>;
  removeAccountGroup?(accountId: string, groupId: string): Promise<void>;

  listPermissionGroups?(limit?: number): Promise<IdentityPage<PermissionGroupSummary>>;
  getPermissionGroup?(groupId: string): Promise<PermissionGroupDetail>;
  createPermissionGroup?(name: string): Promise<PermissionGroupSummary>;
  addPermissionGroupMember?(groupId: string, accountId: string): Promise<void>;
  removePermissionGroupMember?(groupId: string, accountId: string): Promise<void>;
  addPermissionGroupGrant?(groupId: string, key: string, scope: string): Promise<void>;
  removePermissionGroupGrant?(groupId: string, key: string, scope: string): Promise<void>;

  /** The Bot index: used only to answer "this account owns which Bots" on the
   * account page (the Bot pages themselves are A4.4b). */
  listBots?(limit?: number, includeArchived?: boolean): Promise<IdentityPage<BotView>>;

  /** The audit projection. Absent until the server exposes the route. */
  listAuditEvents?(query?: AuditQuery): Promise<IdentityPage<AuditEventView>>;
}
