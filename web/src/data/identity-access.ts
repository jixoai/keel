// The identity/access state — change keel-bot-identity-access, A4.4a.
//
// One module owns what the 04 身份 and 05 记录 chapters read and write: the
// account and permission-group indexes, one object's detail, the group matrix's
// unsaved draft and the audit projection. It talks to `store.identityApi()`
// (the app's `KeelHttp`, or a test/development transport through the same
// seam) and never to `fetch`.
//
// The server is authoritative in two ways this module must not blur:
//   - every list/detail is the server's answer; nothing is invented for a
//     missing field (an unavailable membership stays unknown, not "no groups");
//   - a draft is only a local comparison against the last saved answer. Saving
//     sends exactly the diff and then re-reads the group, so the ledger the
//     reader sees is the server's state again (never a second truth).
//
// A 403 keeps its required key (`KeelApiError.requiredKey`), so the page can say
// which capability is missing; `can()` only decides what to offer before the
// request — the server refuses the write either way.
import { signal } from "nsh";
import { KeelApiError, errorMessageOf } from "./http";
import { can, scopeOf } from "./permissions";
import { store } from "./store";
import {
  groupDisplayName,
  isSuperAdminGroup,
  scopeRank,
  type AccountView,
  type AuditEventView,
  type AuditQuery,
  type BotView,
  type CreateAccountInput,
  type IdentityPage,
  type PermissionGroupDetail,
  type PermissionGroupSummary,
  type PermissionScope,
} from "./identity";
import type { IdentityApi } from "./identity-api";
import { isHighRisk } from "./permission-catalog";

/** A failed read or write, kept as facts (not a pre-built sentence) so a page
 * can decide between its error, permission and unavailable faces. */
export interface AccessFailure {
  /** HTTP status, or null for a lost network/invalid answer. */
  status: number | null;
  message: string;
  /** The capability a 403 named, when the server named one. */
  requiredKey?: string;
  /** The server's request id, when the edge answered one. */
  requestId?: string;
  /** The transport does not serve this surface at all (no method, or the
   * server's default-deny 404 on a route that has not landed). */
  unavailable?: boolean;
}

/** One account row with the memberships the group read could resolve. `groups`
 * stays undefined when the group details were not readable: the table then
 * shows "—" instead of claiming the account belongs to nothing. */
export interface AccountRow extends AccountView {
  groups?: PermissionGroupSummary[];
}

/** One group row with the counts its detail supplied (absent while the detail
 * is unreadable or still loading). */
export interface GroupRow extends PermissionGroupSummary {
  memberCount?: number;
  grantCount?: number;
}

export type GroupChangeKind = "add" | "raise" | "narrow" | "revoke";

export interface GroupChange {
  key: string;
  from?: PermissionScope;
  to?: PermissionScope;
  kind: GroupChangeKind;
  /** A critical-risk key widened (add/raise): the ledger needs an explicit
   * confirmation, not a silent save. */
  highRisk: boolean;
}

const failureOf = (error: unknown): AccessFailure => {
  if (error instanceof KeelApiError) {
    const failure: AccessFailure = { status: error.status, message: errorMessageOf(error) };
    if (error.requiredKey) failure.requiredKey = error.requiredKey;
    if (error.requestId) failure.requestId = error.requestId;
    return failure;
  }
  return { status: null, message: errorMessageOf(error) };
};

/** The highest stored scope per key: the group's effective grant map (the table
 * stores one row per (key, scope) pair, and the engine takes the widest). */
const savedGrantsOf = (detail: PermissionGroupDetail | null): Record<string, PermissionScope> => {
  const saved: Record<string, PermissionScope> = {};
  if (!detail) return saved;
  for (const grant of detail.grants) {
    const previous = saved[grant.key];
    if (previous === undefined || scopeRank(grant.scope) > scopeRank(previous)) saved[grant.key] = grant.scope;
  }
  return saved;
};

const recordsKindOf = (kind: string): string | undefined => (kind === "all" ? undefined : kind);

const startOfToday = (): Date => {
  const now = new Date();
  return new Date(now.getFullYear(), now.getMonth(), now.getDate());
};

const sinceOf = (range: "today" | "7d" | "30d"): string => {
  const days = range === "today" ? 0 : range === "7d" ? 7 : 30;
  if (days === 0) return startOfToday().toISOString();
  return new Date(Date.now() - days * 24 * 60 * 60 * 1000).toISOString();
};

class IdentityAccessStore {
  // ---- accounts ------------------------------------------------------------
  readonly accounts = signal<AccountRow[]>([]);
  readonly accountsLoading = signal(false);
  readonly accountsError = signal<AccessFailure | null>(null);
  /** A full page came back: another page may exist (the API has no total). */
  readonly accountsHasMore = signal(false);
  readonly accountsLimit = signal(100);
  /** The search/filter state lives here so leaving the chapter and coming back
   * keeps the reader's view (module-level, like the app's other list state). */
  readonly accountsQuery = signal("");
  readonly accountsKind = signal<"all" | "human" | "bot" | "service">("all");
  readonly accountsStatus = signal<"all" | "active" | "disabled">("all");
  /** The chapter's one automatic read happened (success or failure). A failure
   * must not make the chapter's entry effect retry forever: the page's own 重试
   * is the way back. */
  private accountsAttempted = false;
  private accountsToken = 0;

  // ---- groups --------------------------------------------------------------
  readonly groups = signal<GroupRow[]>([]);
  readonly groupsLoading = signal(false);
  readonly groupsError = signal<AccessFailure | null>(null);
  readonly groupsQuery = signal("");
  private groupsAttempted = false;
  private groupsToken = 0;
  /** Group details read for the index (counts + memberships); the detail page
   * reuses the cache until it reloads. */
  private readonly groupCache = signal<Record<string, PermissionGroupDetail>>({});

  // ---- one account ---------------------------------------------------------
  readonly accountDetail = signal<{ account: AccountView; groups: PermissionGroupSummary[] } | null>(null);
  readonly accountDetailId = signal<string | null>(null);
  readonly accountDetailLoading = signal(false);
  readonly accountDetailError = signal<AccessFailure | null>(null);
  readonly accountBots = signal<BotView[]>([]);
  readonly accountBotsLoaded = signal(false);
  readonly accountBotsError = signal<AccessFailure | null>(null);
  private accountToken = 0;
  private accountBotsToken = 0;

  // ---- one group -----------------------------------------------------------
  readonly groupDetail = signal<PermissionGroupDetail | null>(null);
  readonly groupDetailId = signal<string | null>(null);
  readonly groupDetailLoading = signal(false);
  readonly groupDetailError = signal<AccessFailure | null>(null);
  /** The unsaved matrix: key -> chosen scope (`undefined` = 未授予). Null until
   * a detail is read. */
  readonly groupDraft = signal<Record<string, PermissionScope | undefined> | null>(null);
  /** The last saved answer's effective map (the diff's `from`). */
  readonly groupSaved = signal<Record<string, PermissionScope>>({});
  readonly groupSaving = signal(false);
  readonly groupSaveError = signal<string | null>(null);
  /** A member add/remove or copy failure, kept next to the sections. */
  readonly groupActionError = signal<string | null>(null);
  readonly groupSearch = signal("");
  private groupToken = 0;

  // ---- records -------------------------------------------------------------
  readonly records = signal<AuditEventView[]>([]);
  readonly recordsLoading = signal(false);
  readonly recordsError = signal<AccessFailure | null>(null);
  /** The deployment has no audit read route: the page says 待接入, it does not
   * pretend the ledger is empty. */
  readonly recordsUnavailable = signal(false);
  readonly recordsHasMore = signal(false);
  readonly recordsQuery = signal("");
  readonly recordsRange = signal<"today" | "7d" | "30d">("7d");
  readonly recordsKind = signal<string>("all");
  readonly recordsResult = signal<"all" | "ok" | "denied" | "error">("all");
  private recordsToken = 0;
  private recordsAttempted = false;

  // ---- the account index's 「最近活动」 -------------------------------------
  // The account listing has no activity field. When the reader may read the
  // audit ledger, the index derives the last event per account from it; when it
  // may not (or the deployment has no audit read), the column prints 「—」 and
  // the page says so once — it never repeats 「未记录」 on every row.
  readonly activityEvents = signal<AuditEventView[]>([]);
  readonly activityState = signal<"idle" | "loading" | "ready" | "unavailable" | "forbidden">("idle");
  private activityToken = 0;

  // ---- plumbing ------------------------------------------------------------

  private api(): IdentityApi | null {
    return store.identityApi();
  }

  // ---- accounts ------------------------------------------------------------

  /** Read the index (optionally with a larger limit: 「加载更多」 raises it). */
  async loadAccounts(): Promise<void> {
    const api = this.api();
    const token = ++this.accountsToken;
    if (!api) {
      this.accountsError.set({ status: null, message: "这个传输不提供身份接口，页面无法读取账号。", unavailable: true });
      return;
    }
    this.accountsLoading.set(true);
    this.accountsError.set(null);
    try {
      const page: IdentityPage<AccountView> = await api.listAccounts!(this.accountsLimit());
      if (token !== this.accountsToken) return;
      this.accounts.set(page.items.map((account) => ({ ...account })));
      this.accountsHasMore.set(page.hasMore);
      this.applyMemberships();
    } catch (error) {
      if (token !== this.accountsToken) return;
      this.accountsError.set(failureOf(error));
    } finally {
      if (token === this.accountsToken) this.accountsLoading.set(false);
    }
  }

  /** A list entry was created/removed elsewhere: re-read at the same limit. */
  private reloadAccountsSoon(): void {
    void this.loadAccounts();
  }

  /** 「加载更多」: one step (50) up to the API's 500 bound. */
  async loadMoreAccounts(): Promise<void> {
    const next = Math.min(500, this.accountsLimit() + 50);
    if (next === this.accountsLimit() || this.accountsLoading()) return;
    this.accountsLimit.set(next);
    await this.loadAccounts();
  }

  /** Resolve each account's memberships from the group details already read.
   * A group whose detail failed stays unknown, so the column shows "—". */
  private applyMemberships(): void {
    const details = this.groupCache();
    const known = Object.values(details);
    if (!known.length) return;
    const byAccount = new Map<string, PermissionGroupSummary[]>();
    for (const detail of known) {
      for (const member of detail.members) {
        const bucket = byAccount.get(member.accountId) ?? [];
        bucket.push(detail.group);
        byAccount.set(member.accountId, bucket);
      }
    }
    this.accounts.update((rows) =>
      rows.map((row) => {
        const groups = byAccount.get(row.accountId) ?? [];
        // A known-empty list is a fact; an account in no known group is
        // "none" only when every group's detail was read.
        return { ...row, groups };
      }),
    );
  }

  async createAccount(input: CreateAccountInput): Promise<AccountView | null> {
    const api = this.api();
    if (!api?.createAccount) return null;
    try {
      const account = await api.createAccount(input);
        this.reloadAccountsSoon();
      return account;
    } catch (error) {
      this.accountActionError.set(failureOf(error).message);
      return null;
    }
  }

  /** One account's detail. The list's group memberships are the fallback while
   * the detail read is in flight. */
  async loadAccount(accountId: string): Promise<void> {
    const api = this.api();
    const token = ++this.accountToken;
    this.accountDetailId.set(accountId);
    if (!api) {
      this.accountDetailError.set({ status: null, message: "这个传输不提供身份接口，页面无法读取账号。", unavailable: true });
      return;
    }
    this.accountDetailLoading.set(true);
    this.accountDetailError.set(null);
    // Never show the previous account's facts under the new id while this read
    // is in flight.
    if (this.accountDetail()?.account.accountId !== accountId) this.accountDetail.set(null);
    try {
      const detail = await api.getAccount!(accountId);
      if (token !== this.accountToken) return;
      this.accountDetail.set(detail);
      // Refresh the row's kind/status/name in the index from the same answer.
      this.accounts.update((rows) =>
        rows.map((row) => (row.accountId === detail.account.accountId ? { ...row, ...detail.account } : row)),
      );
    } catch (error) {
      if (token !== this.accountToken) return;
      this.accountDetailError.set(failureOf(error));
      this.accountDetail.set(null);
    } finally {
      if (token === this.accountToken) this.accountDetailLoading.set(false);
    }
  }

  /** The Bots owned by one account (read-only index; the Bot pages are A4.4b). */
  async loadAccountBots(accountId: string): Promise<void> {
    const api = this.api();
    const token = ++this.accountBotsToken;
    if (token === this.accountBotsToken) this.accountBotsError.set(null);
    if (!api?.listBots) {
      this.accountBots.set([]);
      this.accountBotsLoaded.set(true);
      this.accountBotsError.set({ status: null, message: "这个传输不提供 Bot 索引。", unavailable: true });
      return;
    }
    try {
      const page = await api.listBots(500, true);
      if (token !== this.accountBotsToken) return;
      this.accountBots.set(page.items.filter((bot) => bot.accountId === accountId));
      this.accountBotsLoaded.set(true);
    } catch (error) {
      if (token !== this.accountBotsToken) return;
      this.accountBotsError.set(failureOf(error));
      this.accountBots.set([]);
      this.accountBotsLoaded.set(true);
    }
  }

  readonly accountActionError = signal<string | null>(null);
  readonly accountActionBusy = signal<string | null>(null);

  async setAccountDisabled(accountId: string, disabled: boolean, reason?: string): Promise<boolean> {
    const api = this.api();
    if (!api?.setAccountDisabled) return false;
    this.accountActionBusy.set(accountId);
    this.accountActionError.set(null);
    try {
      const account = await api.setAccountDisabled(accountId, disabled, reason);
      this.accountDetail.update((detail) => (detail ? { ...detail, account } : detail));
      this.accounts.update((rows) => rows.map((row) => (row.accountId === accountId ? { ...row, ...account } : row)));
      return true;
    } catch (error) {
      this.accountActionError.set(failureOf(error).message);
      return false;
    } finally {
      this.accountActionBusy.set(null);
    }
  }

  /** Reset a password to a one-time temporary value (the server derives must-
   * change + session revocation from this write). */
  async resetAccountPassword(accountId: string, password: string): Promise<boolean> {
    const api = this.api();
    if (!api?.resetAccountPassword) return false;
    this.accountActionBusy.set(accountId);
    this.accountActionError.set(null);
    try {
      await api.resetAccountPassword(accountId, password);
      return true;
    } catch (error) {
      this.accountActionError.set(failureOf(error).message);
      return false;
    } finally {
      this.accountActionBusy.set(null);
    }
  }

  async addAccountGroup(accountId: string, groupId: string): Promise<boolean> {
    const api = this.api();
    if (!api?.addAccountGroup) return false;
    this.accountActionBusy.set(accountId);
    this.accountActionError.set(null);
    try {
      await api.addAccountGroup(accountId, groupId);
      await this.loadAccount(accountId);
      await this.loadGroups();
      return true;
    } catch (error) {
      this.accountActionError.set(failureOf(error).message);
      return false;
    } finally {
      this.accountActionBusy.set(null);
    }
  }

  async removeAccountGroup(accountId: string, groupId: string): Promise<boolean> {
    const api = this.api();
    if (!api?.removeAccountGroup) return false;
    this.accountActionBusy.set(accountId);
    this.accountActionError.set(null);
    try {
      await api.removeAccountGroup(accountId, groupId);
      await this.loadAccount(accountId);
      await this.loadGroups();
      return true;
    } catch (error) {
      this.accountActionError.set(failureOf(error).message);
      return false;
    } finally {
      this.accountActionBusy.set(null);
    }
  }

  // ---- groups --------------------------------------------------------------

  /** The group index. Each summary's detail supplies the member/grant counts and
   * the account memberships; a detail that fails leaves that row's counts
   * unknown rather than zero. */
  async loadGroups(): Promise<void> {
    const api = this.api();
    const token = ++this.groupsToken;
    if (!api?.listPermissionGroups || !api.getPermissionGroup) {
      this.groupsError.set({ status: null, message: "这个传输不提供权限组接口。", unavailable: true });
      return;
    }
    this.groupsLoading.set(true);
    this.groupsError.set(null);
    try {
      const page = await api.listPermissionGroups(100);
      if (token !== this.groupsToken) return;
      const rows = page.items.map((group) => ({ ...group }));
      this.groups.set(rows);
      // Read the details in small batches: the index needs each group's counts
      // and memberships, but a deployment with many groups must not fire one
      // request per group at once.
      const details: Array<PermissionGroupDetail | null> = [];
      for (let index = 0; index < rows.length; index += 6) {
        const batch = await Promise.all(
          rows.slice(index, index + 6).map(async (row) => {
            try {
              return (await api.getPermissionGroup!(row.groupId)) as PermissionGroupDetail;
            } catch {
              return null;
            }
          }),
        );
        details.push(...batch);
      }
      if (token !== this.groupsToken) return;
      const cache: Record<string, PermissionGroupDetail> = {};
      for (const detail of details) if (detail) cache[detail.group.groupId] = detail;
      this.groupCache.set(cache);
      this.groups.set(
        rows.map((row) => {
          const detail = cache[row.groupId];
          return detail
            ? { ...row, memberCount: detail.members.length, grantCount: detail.grants.length }
            : { ...row };
        }),
      );
      this.applyMemberships();
    } catch (error) {
      if (token !== this.groupsToken) return;
      this.groupsError.set(failureOf(error));
    } finally {
      if (token === this.groupsToken) this.groupsLoading.set(false);
    }
  }

  /** One group's detail; resets the draft to the saved answer (unless the
   * caller keeps the reader's edits). */
  async loadGroup(groupId: string, opts: { keepDraft?: boolean } = {}): Promise<void> {
    const api = this.api();
    const token = ++this.groupToken;
    this.groupDetailId.set(groupId);
    if (!api?.getPermissionGroup) {
      this.groupDetailError.set({ status: null, message: "这个传输不提供权限组接口。", unavailable: true });
      return;
    }
    this.groupDetailLoading.set(true);
    this.groupDetailError.set(null);
    if (this.groupDetail()?.group.groupId !== groupId) {
      this.groupDetail.set(null);
      if (!opts.keepDraft) this.groupDraft.set(null);
    }
    try {
      const detail = await api.getPermissionGroup(groupId);
      if (token !== this.groupToken) return;
      this.groupDetail.set(detail);
      const saved = savedGrantsOf(detail);
      this.groupSaved.set(saved);
      if (!opts.keepDraft) this.groupDraft.set({ ...saved });
      this.groupSaveError.set(null);
      this.groupCache.update((cache) => ({ ...cache, [detail.group.groupId]: detail }));
      this.groups.update((rows) =>
        rows.map((row) =>
          row.groupId === detail.group.groupId
            ? { ...row, ...detail.group, memberCount: detail.members.length, grantCount: detail.grants.length }
            : row,
        ),
      );
    } catch (error) {
      if (token !== this.groupToken) return;
      this.groupDetailError.set(failureOf(error));
      this.groupDetail.set(null);
    } finally {
      if (token === this.groupToken) this.groupDetailLoading.set(false);
    }
  }

  /** The matrix's draft write: a scope, or undefined for 未授予. */
  setDraftScope(key: string, scope: PermissionScope | undefined): void {
    this.groupDraft.update((draft) => {
      const next = { ...(draft ?? {}) };
      if (scope === undefined) next[key] = undefined;
      else next[key] = scope;
      return next;
    });
  }

  discardDraft(): void {
    this.groupDraft.set({ ...this.groupSaved() });
    this.groupSaveError.set(null);
  }

  /** A group detail already read (the index reads every group's detail for its
   * counts). `undefined` means "not read", never "no grants". */
  groupDetailOf(groupId: string): PermissionGroupDetail | undefined {
    return this.groupCache()[groupId] ?? (this.groupDetail()?.group.groupId === groupId ? this.groupDetail() ?? undefined : undefined);
  }

  /** The unsaved changes, in catalogue order (the server's semantics: one
   * effective scope per key). */
  groupChanges(): GroupChange[] {    const draft = this.groupDraft();
    if (!draft) return [];
    const saved = this.groupSaved();
    const keys = new Set([...Object.keys(saved), ...Object.keys(draft)]);
    const changes: GroupChange[] = [];
    for (const key of keys) {
      const from = saved[key];
      const to = draft[key];
      if (from === to) continue;
      const kind: GroupChangeKind =
        from === undefined ? "add" : to === undefined ? "revoke" : scopeRank(to) > scopeRank(from) ? "raise" : "narrow";
      changes.push({ key, from, to, kind, highRisk: isHighRisk(key) && (kind === "add" || kind === "raise") });
    }
    return changes;
  }

  /** Write exactly the diff, then re-read the group. `applied` is how many
   * changes the server accepted; a partial failure leaves the rest in the draft
   * so a retry sends only what is left. */
  async saveGroupDraft(): Promise<{ ok: boolean; applied: number; total: number }> {
    const api = this.api();
    const detail = this.groupDetail();
    const changes = this.groupChanges();
    if (!api || !detail || !changes.length) return { ok: true, applied: 0, total: 0 };
    if (!api.addPermissionGroupGrant || !api.removePermissionGroupGrant) {
      this.groupSaveError.set("这个传输不支持保存权限组。");
      return { ok: false, applied: 0, total: changes.length };
    }
    const groupId = detail.group.groupId;
    const intended = { ...(this.groupDraft() ?? {}) };
    let applied = 0;
    this.groupSaving.set(true);
    this.groupSaveError.set(null);
    try {
      for (const change of changes) {
        if (change.to === undefined) {
          const scopes = detail.grants.filter((grant) => grant.key === change.key).map((grant) => grant.scope);
          for (const scope of scopes) await api.removePermissionGroupGrant!(groupId, change.key, scope);
        } else {
          // Add before removing: the widened grant is live before the old one
          // goes, so a save can never drop access mid-flight.
          await api.addPermissionGroupGrant!(groupId, change.key, change.to);
          const others = detail.grants
            .filter((grant) => grant.key === change.key && grant.scope !== change.to)
            .map((grant) => grant.scope);
          for (const scope of others) await api.removePermissionGroupGrant!(groupId, change.key, scope);
        }
        applied += 1;
      }
      await this.loadGroup(groupId, { keepDraft: false });
      return { ok: true, applied, total: changes.length };
    } catch (error) {
      const failure = failureOf(error);
      this.groupSaveError.set(
        applied > 0
          ? `已应用 ${applied} / ${changes.length} 项；下一项失败：${failure.message}`
          : failure.message,
      );
      // The server's state is the truth now: re-read it, then keep the reader's
      // intent in the draft so the remaining diff is what a retry sends.
      await this.loadGroup(groupId, { keepDraft: false });
      this.groupDraft.set(intended);
      return { ok: false, applied, total: changes.length };
    } finally {
      this.groupSaving.set(false);
    }
  }

  async createGroup(name: string): Promise<PermissionGroupSummary | null> {
    const api = this.api();
    if (!api?.createPermissionGroup) return null;
    this.groupActionError.set(null);
    try {
      const group = await api.createPermissionGroup(name);
      await this.loadGroups();
      return group;
    } catch (error) {
      this.groupActionError.set(failureOf(error).message);
      return null;
    }
  }

  readonly groupCopying = signal(false);

  /** 另存为副本: a new custom group carrying the source's effective grants (one
   * per key). Members are a separate decision, so the copy starts with none —
   * the result says so. */
  async copyGroupAs(source: PermissionGroupDetail, name: string): Promise<PermissionGroupSummary | null> {
    const api = this.api();
    if (!api?.createPermissionGroup || !api.addPermissionGroupGrant) return null;
    this.groupCopying.set(true);
    this.groupActionError.set(null);
    try {
      const created = await api.createPermissionGroup(name);
      const saved = savedGrantsOf(source);
      for (const [key, scope] of Object.entries(saved)) {
        await api.addPermissionGroupGrant(created.groupId, key, scope);
      }
      await this.loadGroups();
      return created;
    } catch (error) {
      this.groupActionError.set(`副本没有完整建好：${failureOf(error).message}`);
      await this.loadGroups();
      return null;
    } finally {
      this.groupCopying.set(false);
    }
  }

  async addGroupMember(groupId: string, accountId: string): Promise<boolean> {
    const api = this.api();
    if (!api?.addPermissionGroupMember) return false;
    this.groupActionError.set(null);
    try {
      await api.addPermissionGroupMember(groupId, accountId);
      await this.loadGroup(groupId, { keepDraft: true });
      await this.loadGroups();
      return true;
    } catch (error) {
      this.groupActionError.set(failureOf(error).message);
      return false;
    } finally {
      }
  }

  async removeGroupMember(groupId: string, accountId: string): Promise<boolean> {
    const api = this.api();
    if (!api?.removePermissionGroupMember) return false;
    this.groupActionError.set(null);
    try {
      await api.removePermissionGroupMember(groupId, accountId);
      await this.loadGroup(groupId, { keepDraft: true });
      await this.loadGroups();
      return true;
    } catch (error) {
      this.groupActionError.set(failureOf(error).message);
      return false;
    } finally {
      }
  }

  /** The rows the matrix shows: the catalogue's domains, plus any stored key the
   * catalogue does not know (so a grant never disappears from the ledger). */
  // (The page groups `allCapabilityDomains()` itself; an unknown stored key is
  // collected there from `groupDetail().grants`.)

  // ---- records -------------------------------------------------------------

  /** The audit projection. A transport/route without the surface answers the
   * 待接入 face — never a fabricated ledger. */
  async loadRecords(): Promise<void> {
    const api = this.api();
    const token = ++this.recordsToken;
    this.recordsUnavailable.set(false);
    if (!api) {
      this.recordsError.set({ status: null, message: "这个传输不提供身份接口。" , unavailable: true });
      return;
    }
    if (!api.listAuditEvents) {
      this.records.set([]);
      this.recordsError.set(null);
      this.recordsUnavailable.set(true);
      this.recordsLoading.set(false);
      return;
    }
    this.recordsLoading.set(true);
    this.recordsError.set(null);
    const result = this.recordsResult();
    const kind = recordsKindOf(this.recordsKind());
    const query: AuditQuery = {
      limit: 100,
      since: sinceOf(this.recordsRange()),
      ...(kind ? { kind } : {}),
      ...(result === "all" ? {} : { result }),
    };
    try {
      const page = await api.listAuditEvents(query);
      if (token !== this.recordsToken) return;
      this.records.set(page.items);
      this.recordsHasMore.set(page.hasMore);
    } catch (error) {
      if (token !== this.recordsToken) return;
      const failure = failureOf(error);
      // The route has not landed: 404 is the server's default-deny, not "the
      // record you asked for is gone". 405/501 are a deployment without the
      // read surface. All three become the one honest 待接入 face.
      if (failure.status === 404 || failure.status === 405 || failure.status === 501) {
        this.records.set([]);
        this.recordsUnavailable.set(true);
        this.recordsError.set(null);
        } else {
        this.recordsError.set(failure);
      }
    } finally {
      if (token === this.recordsToken) this.recordsLoading.set(false);
    }
  }

  setRecordsRange(range: "today" | "7d" | "30d"): void {
    if (this.recordsRange() === range) return;
    this.recordsRange.set(range);
    void this.loadRecords();
  }

  setRecordsKind(kind: string): void {
    if (this.recordsKind() === kind) return;
    this.recordsKind.set(kind);
    void this.loadRecords();
  }

  setRecordsResult(result: "all" | "ok" | "denied" | "error"): void {
    if (this.recordsResult() === result) return;
    this.recordsResult.set(result);
    void this.loadRecords();
  }

  /** The rows after the page's own text filter (a projection over what the
   * server returned; it never turns a missing row into a match). */
  visibleRecords(): AuditEventView[] {
    const query = this.recordsQuery().trim().toLowerCase();
    if (!query) return this.records();
    return this.records().filter((event) =>
      [event.summary, event.action, event.actorName, event.objectName, event.actorKind, event.objectType]
        .filter((part): part is string => typeof part === "string")
        .some((part) => part.toLowerCase().includes(query)),
    );
  }

  // ---- the account index's recent activity --------------------------------

  /** Read one bounded page of the ledger for the account index's fifth column.
   * This is a projection, not a second ledger: the rows stay in
   * `activityEvents`, the 05 chapter keeps its own filtered view. */
  async loadActivity(): Promise<void> {
    if (this.activityState() === "loading" || this.activityState() === "ready") return;
    const api = this.api();
    if (!api?.listAuditEvents) {
      this.activityState.set("unavailable");
      return;
    }
    const token = ++this.activityToken;
    this.activityState.set("loading");
    try {
      const page = await api.listAuditEvents({
        limit: 200,
        since: new Date(Date.now() - 30 * 24 * 60 * 60 * 1000).toISOString(),
      });
      if (token !== this.activityToken) return;
      this.activityEvents.set(
        [...page.items].sort((left, right) => Date.parse(right.occurredAt ?? "") - Date.parse(left.occurredAt ?? "")),
      );
      this.activityState.set("ready");
    } catch (error) {
      if (token !== this.activityToken) return;
      // A route that has not landed is the same honest 「—」 as a deployment
      // without audit; a 403 keeps its own word (the reader may not read the
      // ledger). Neither turns the identity page into an error page.
      this.activityState.set(failureOf(error).status === 403 ? "forbidden" : "unavailable");
    }
  }

  /** The last audit event this account acted in, matched by id first and by the
   * display name the record carries (the wire's older shape). */
  lastActivityOf(account: AccountView): AuditEventView | undefined {
    const name = account.displayName || account.username;
    return this.activityEvents().find(
      (event) =>
        event.actorId === account.accountId ||
        (!!name && (event.actorName === name || event.actorName === account.username)),
    );
  }

  /** The reader's own role in this deployment, in the same words the group page
   * uses: a super admin first, then a built-in group, then any group. */
  currentRoleLabel(): string {
    const me = store.identity();
    if (me?.isSuperAdmin) return "超级管理员";
    const row = this.accounts().find((account) => account.accountId === me?.accountId);
    const groups = row?.groups ?? [];
    const preferred =
      groups.find((group) => group.builtin && !isSuperAdminGroup(group.name)) ?? groups[0];
    return preferred ? groupDisplayName(preferred.name) : "成员";
  }

  // ---- boot ----------------------------------------------------------------

  /** The identity chapter's first read: the indexes are loaded once per launch
   * (or after a write that invalidated them). */
  ensureIndexes(): void {
    if (!this.accountsAttempted) {
      this.accountsAttempted = true;
      void this.loadAccounts();
    }
    if (!this.groupsAttempted) {
      this.groupsAttempted = true;
      void this.loadGroups();
    }
    // The index's 「最近活动」 is the ledger's projection; only a reader with
    // `audit.read` can derive it, and a deployment without the route leaves the
    // column at 「—」 with one page-level note.
    if (can("audit.read")) void this.loadActivity();
  }

  /** The records chapter's first read. */
  ensureRecords(): void {
    if (this.recordsAttempted) return;
    this.recordsAttempted = true;
    void this.loadRecords();
  }

  /** Drop everything read for the previous session (logout, a new login, a
   * failed probe): the next account must never see the last one's accounts,
   * groups or records from memory. Also used by tests for a clean start. */
  clearSession(): void {
    this.accountsToken += 1;
    this.groupsToken += 1;
    this.accountToken += 1;
    this.accountBotsToken += 1;
    this.groupToken += 1;
    this.recordsToken += 1;
    this.accounts.set([]);
    this.accountsError.set(null);
    this.accountsLoading.set(false);
    this.accountsLimit.set(100);
    this.accountsAttempted = false;
    this.groups.set([]);
    this.groupsError.set(null);
    this.groupsLoading.set(false);
    this.groupsAttempted = false;
    this.groupCache.set({});
    this.accountDetail.set(null);
    this.accountDetailError.set(null);
    this.accountDetailLoading.set(false);
    this.accountActionError.set(null);
    this.accountBots.set([]);
    this.accountBotsError.set(null);
    this.accountBotsLoaded.set(false);
    this.groupDetail.set(null);
    this.groupDetailError.set(null);
    this.groupDetailLoading.set(false);
    this.groupDraft.set(null);
    this.groupSaved.set({});
    this.groupSaving.set(false);
    this.groupSaveError.set(null);
    this.groupActionError.set(null);
    this.records.set([]);
    this.recordsError.set(null);
    this.recordsLoading.set(false);
    this.recordsUnavailable.set(false);
    this.recordsHasMore.set(false);
    this.recordsAttempted = false;
    this.activityToken += 1;
    this.activityEvents.set([]);
    this.activityState.set("idle");
    // The view state is part of the session's memory too: the next account must
    // not open on the previous one's search/filter.
    this.accountsQuery.set("");
    this.accountsKind.set("all");
    this.accountsStatus.set("all");
    this.accountsLimit.set(100);
    this.groupsQuery.set("");
    this.groupSearch.set("");
    this.recordsQuery.set("");
    this.recordsRange.set("7d");
    this.recordsKind.set("all");
    this.recordsResult.set("all");
  }

  /** Test alias (the same reset). */
  resetForTest(): void {
    this.clearSession();
  }
}

export const identityAccess = new IdentityAccessStore();

/** The scope the caller holds for a key, as the matrix's ceiling (`can()` and
 * `heldRank` are the display contract; the server still refuses anything
 * above it). */
export const ownScopeOf = (key: string): PermissionScope | undefined => scopeOf(key);

export const canReadAccounts = (): boolean => can("account.read");
export const canManageAccounts = (): boolean => can("account.manage");
export const canResetCredentials = (): boolean => can("account.credential.reset");
export const canGrantMembership = (): boolean => can("account.permission.grant");
export const canReadGroups = (): boolean => can("permission.read");
export const canManageGroups = (): boolean => can("permission.manage");
export const canReadRecords = (): boolean => can("audit.read");

