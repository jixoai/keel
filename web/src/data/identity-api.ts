// The identity/access transport shape — change keel-bot-identity-access,
// A4.4a (accounts/groups/records) and A4.4b (Bots/TeamBots/runs).
//
// One interface, one method per server route the identity pages read or write.
// It lives apart from the screens and from the app store so both can name it
// without importing each other: the live transport is `KeelHttp` (whose methods
// already exist since A4.3), the development fixture answers it in memory, and a
// test may install its own through `identityAccess.setTransport`.
//
// The record chapter's read and the run projection's reads are optional on
// purpose. A transport without them answers nothing, and the page renders its
// 待接入 state instead of an error about a surface the deployment never had.
import type {
  DeliverablesOptions,
  DeliverablesView,
  FilePreviewView,
  SessionExportOptions,
  SessionExportView,
  SessionFilesQuery,
  SessionFilesView,
  SessionSearchOptions,
  SessionSearchView,
  SessionSummary,
  TaskPanelView,
} from "./contract";
import type {
  AccountView,
  AuditEventView,
  AuditQuery,
  BotKeyView,
  BotView,
  CreateAccountInput,
  CreateBotInput,
  CreateTeamBotInput,
  IdentityPage,
  PermissionGroupDetail,
  PermissionGroupSummary,
  ShareInput,
  ShareResourceKind,
  ShareView,
  ShareWriteResult,
  TeamBotView,
  UpdateBotInput,
  UpdateTeamBotInput,
  WhoAmI,
} from "./identity";
import type {
  ProfileView,
  RoleView,
  RunStartView,
  SessionCancelView,
  SessionEventView,
  SubagentResumeView,
  SubagentView,
  TeamTasksView,
} from "./bots-wire";
import type { ExternalProjectionView } from "./oar-projection";

export interface IssuedBotKeyView {
  key: BotKeyView;
  /** Shown once, never read back. */
  plaintext: string;
  notice: string;
}

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

  /** A Bot's Profile (Account + Role) and the Role documents. Both are reads;
   * `bot.bind` decides whether a Profile may be rebound (server-side). */
  listProfiles?(limit?: number): Promise<IdentityPage<ProfileView>>;
  getProfile?(profileId: string): Promise<ProfileView>;
  listRoles?(limit?: number): Promise<IdentityPage<RoleView>>;

  /** The Bot index and one Bot (A4.4b). */
  listBots?(limit?: number, includeArchived?: boolean): Promise<IdentityPage<BotView>>;
  getBot?(botId: string): Promise<BotView>;
  createBot?(input: CreateBotInput): Promise<BotView>;
  updateBot?(botId: string, input: UpdateBotInput): Promise<BotView>;
  archiveBot?(botId: string): Promise<void>;
  invokeBot?(botId: string, text: string, sessionId?: string, requestId?: string): Promise<RunStartView>;
  listBotKeys?(botId: string): Promise<BotKeyView[]>;
  issueBotKey?(botId: string, label?: string, accountId?: string): Promise<IssuedBotKeyView>;
  revokeBotKey?(botId: string, keyId: string): Promise<void>;

  /** The TeamBot index and one TeamBot. */
  listTeamBots?(limit?: number, includeArchived?: boolean): Promise<IdentityPage<TeamBotView>>;
  getTeamBot?(teamBotId: string): Promise<TeamBotView>;
  createTeamBot?(input: CreateTeamBotInput): Promise<TeamBotView>;
  updateTeamBot?(teamBotId: string, input: UpdateTeamBotInput): Promise<TeamBotView>;
  updateTeamBotRoster?(
    teamBotId: string,
    roster: UpdateTeamBotInput["roster"],
    expectedRevision: number,
  ): Promise<TeamBotView>;
  archiveTeamBot?(teamBotId: string): Promise<void>;
  startTeamBot?(teamBotId: string, text?: string, sessionId?: string, requestId?: string): Promise<RunStartView>;

  /** Resource sharing (A6). One trio per family; only viewer/editor can be
   * written, and the server requires the owner relation. */
  listShares?(kind: ShareResourceKind, resourceId: string): Promise<ShareView[]>;
  addShare?(kind: ShareResourceKind, resourceId: string, input: ShareInput): Promise<ShareWriteResult>;
  revokeShare?(kind: ShareResourceKind, resourceId: string, input: ShareInput): Promise<ShareWriteResult>;

  /** The run projection's reads. The session list already exists on the app's
   * own transport; it is named here so the run page can look up one session's
   * summary without importing the whole workspace store. */
  listSessions?(limit?: number, after?: string): Promise<SessionSummary[]>;
  teamTasks?(sessionId: string, limit?: number, after?: string): Promise<TeamTasksView>;
  /** The run surface's task panel (`GET /v1/sessions/{id}/task-panel`): the
   * task tree, the event stream and the live resource references. Optional:
   * a deployment without the projection answers nothing, and the panel says
   * so instead of drawing an empty tree. */
  taskPanel?(sessionId: string, memberLimit?: number, eventLimit?: number): Promise<TaskPanelView>;
  /** The external run projection (`GET /v1/sessions/{id}/external/projection`):
   * the server-folded OAR record stream (`keel.oar.render-projection/1`) the
   * conversation's external run card renders. Optional: a deployment that never
   * wired OAR answers nothing, and the conversation keeps its text row. */
  externalProjection?(sessionId: string): Promise<ExternalProjectionView>;
  /** One bounded page of a session file the panel previews
   * (`GET /v1/sessions/{id}/file-preview`). Optional, same rule. */
  filePreview?(sessionId: string, path: string, referenceHash?: string, offset?: number): Promise<FilePreviewView>;
  /** The session's file manifest (`GET /v1/sessions/{id}/files`): the panel
   * reads each row's `lastTaskId` as a card's own file attribution and its
   * `hash` as the manifest's current content version. A path-cursor page, so a
   * long manifest can be paged instead of reported as truncated. Optional:
   * without it the panel simply omits the attribution group. */
  sessionFiles?(sessionId: string, query?: SessionFilesQuery): Promise<SessionFilesView>;
  /** `GET /v1/sessions/search`: bounded transcript or session search. Optional:
   * a deployment without the read leaves the search entry out. */
  sessionSearch?(query: string, opts?: SessionSearchOptions): Promise<SessionSearchView>;
  /** `GET /v1/sessions/{id}/export`: one bounded transcript page. Optional. */
  sessionExport?(sessionId: string, opts?: SessionExportOptions): Promise<SessionExportView>;
  /** `GET /v1/sessions/{id}/deliverables`: the manifest-backed workspace
   * changes. Optional: without it the status pane omits the 交付物 section. */
  sessionDeliverables?(sessionId: string, opts?: DeliverablesOptions): Promise<DeliverablesView>;
  sessionEvents?(sessionId: string, limit?: number): Promise<SessionEventView[]>;
  listSubagents?(parentSessionId?: string, limit?: number): Promise<SubagentView[]>;
  /** The durable spawn-link tree (`GET /v1/subagents?rootSessionId=`). Optional:
   * a deployment without the projection keeps the registry-only read. */
  listSubagentTree?(rootSessionId: string, limit?: number): Promise<SubagentView[]>;
  cancelSession?(sessionId: string): Promise<SessionCancelView>;
  /** `POST /v1/subagents/{id}/cancel` — stop one child and its non-terminal
   * descendants. The parent session authorizes the write; the child alone does
   * not. Optional: a deployment without the guarded Host has no such route. */
  cancelSubagent?(sessionId: string, parentSessionId: string, reason?: string): Promise<SessionCancelView>;
  /** `POST /v1/sessions/{id}/resume` — requeue the same child session (a retry
   * of a failed/stopped run, never a new spawn). The path id is the parent. */
  resumeSubagent?(
    parentSessionId: string,
    sessionId: string,
    requestId?: string,
    reason?: string,
  ): Promise<SubagentResumeView>;

  /** The audit projection. Absent until the server exposes the route. */
  listAuditEvents?(query?: AuditQuery): Promise<IdentityPage<AuditEventView>>;
}
