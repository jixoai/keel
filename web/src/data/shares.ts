// Resource sharing state — change keel-bot-identity-access, A6.
//
// One module owns the share dialog's facts for the three shareable resources
// (session, Bot, TeamBot): the current tuples, the account/group candidates,
// the draft and the write lifecycle. It talks to the same `IdentityApi` seam as
// the rest of the identity surface (`store.identityApi()`), so a live
// deployment and the development fixture take one code path.
//
// The server is authoritative:
//   - the tuple list is re-read after every add/revoke, never patched locally;
//   - the dialog offers viewer/editor only (owner/team_member are system
//     managed and the API answers 400);
//   - an owner-only action is hidden unless the caller owns the resource or is
//     a deployment super admin; the server still answers 403 if that changed.
import { signal } from "nsh";
import { botAccess } from "./bots";
import { KeelApiError, errorMessageOf } from "./http";
import { can } from "./permissions";
import { store } from "./store";
import type {
  ShareInput,
  ShareRelation,
  ShareResourceKind,
  ShareSubjectType,
  ShareView,
} from "./identity";
import type { IdentityApi } from "./identity-api";

/** One selectable subject. Accounts carry their username as the secondary
 * line; groups have none. No email or credential ever enters this shape. */
export interface ShareCandidate {
  id: string;
  label: string;
  detail?: string;
  kind?: string;
}

/** The open dialog's target. `visibilityOnly` drives the TeamBot copy: a share
 * there never grants start/manage. */
export interface ShareTarget {
  kind: ShareResourceKind;
  resourceId: string;
  name: string;
  visibilityOnly: boolean;
}

export interface ShareFailure {
  status: number | null;
  message: string;
  requiredKey?: string;
  requestId?: string;
}

const failureOf = (error: unknown): ShareFailure => {
  if (error instanceof KeelApiError) {
    const failure: ShareFailure = { status: error.status, message: errorMessageOf(error) };
    if (error.requiredKey) failure.requiredKey = error.requiredKey;
    if (error.requestId) failure.requestId = error.requestId;
    return failure;
  }
  return { status: null, message: errorMessageOf(error) };
};

/** The permission key the server requires for one family's share routes. */
export const shareKeyOf = (kind: ShareResourceKind): string =>
  kind === "session" ? "session.share" : kind === "bot" ? "bot.manage" : "team_bot.manage";

/** May the UI offer sharing on this resource? The owner check is the same fact
 * the engine uses; a super admin holds every key and passes. */
export const canShareResource = (kind: ShareResourceKind, ownerAccountId: string | undefined): boolean => {
  if (!can(shareKeyOf(kind))) return false;
  const me = store.identity();
  if (!me) return false;
  if (me.isSuperAdmin) return true;
  return !!ownerAccountId && ownerAccountId === me.accountId;
};

/** 「已共享 N」only when N > 0. */
export const sharedBadgeOf = (count: number | undefined): string | undefined =>
  typeof count === "number" && count > 0 ? `已共享 ${count}` : undefined;

export const relationLabel = (relation: ShareRelation): string =>
  relation === "editor" ? "可编辑" : "可查看";

export const shareSubjectLabel = (share: ShareView): string =>
  share.displayName || share.username || share.subjectId;

class ShareStore {
  readonly target = signal<ShareTarget | null>(null);
  readonly shares = signal<ShareView[]>([]);
  readonly loading = signal(false);
  readonly error = signal<ShareFailure | null>(null);

  /** Directory candidates. `directoryForbidden` means the caller lacks the
   * account/permission read key: existing shares stay revocable, adding is not
   * offered (with the reason), never a fabricated list. */
  readonly accounts = signal<ShareCandidate[]>([]);
  readonly groups = signal<ShareCandidate[]>([]);
  readonly directoryLoading = signal(false);
  readonly directoryError = signal<string | null>(null);
  readonly directoryForbidden = signal(false);

  readonly subjectType = signal<"account" | "group">("account");
  readonly subjectId = signal<string | null>(null);
  readonly relation = signal<ShareRelation>("viewer");

  readonly saving = signal(false);
  readonly actionError = signal<string | null>(null);
  readonly notice = signal<string | null>(null);
  /** The tuple currently being revoked (`type:id:relation`). */
  readonly revoking = signal<string | null>(null);
  /** A revoke waiting for the destructive confirmation. */
  readonly revokeCandidate = signal<ShareView | null>(null);

  private token = 0;
  private directoryToken = 0;

  private api(): IdentityApi | null {
    return store.identityApi();
  }

  open(target: ShareTarget): void {
    this.target.set(target);
    this.shares.set([]);
    this.error.set(null);
    this.notice.set(null);
    this.actionError.set(null);
    this.subjectType.set("account");
    this.subjectId.set(null);
    this.relation.set("viewer");
    this.revokeCandidate.set(null);
    void this.load();
    void this.loadDirectory();
  }

  close(): void {
    this.token += 1;
    this.directoryToken += 1;
    this.target.set(null);
    this.shares.set([]);
    this.error.set(null);
    this.loading.set(false);
    this.saving.set(false);
    this.revoking.set(null);
    this.revokeCandidate.set(null);
    this.notice.set(null);
    this.actionError.set(null);
  }

  async load(): Promise<void> {
    const target = this.target();
    const api = this.api();
    const token = ++this.token;
    if (!target) return;
    if (!api?.listShares) {
      this.error.set({ status: null, message: "这个传输不提供共享接口。" });
      return;
    }
    this.loading.set(true);
    this.error.set(null);
    try {
      const shares = await api.listShares(target.kind, target.resourceId);
      if (token !== this.token) return;
      this.shares.set(shares);
    } catch (error) {
      if (token !== this.token) return;
      this.error.set(failureOf(error));
      this.shares.set([]);
    } finally {
      if (token === this.token) this.loading.set(false);
    }
  }

  /** Read the candidate directories the caller may read. Accounts and groups
   * are independent: holding one read key still enables that half. */
  async loadDirectory(): Promise<void> {
    const api = this.api();
    const token = ++this.directoryToken;
    const canAccounts = can("account.read");
    const canGroups = can("permission.read");
    this.directoryForbidden.set(!canAccounts || !canGroups);
    this.directoryError.set(null);
    if (!api) {
      this.directoryError.set("这个传输不提供成员目录。");
      return;
    }
    if (!canAccounts && !canGroups) return;
    this.directoryLoading.set(true);
    const me = store.identity()?.accountId;
    try {
      if (canAccounts && api.listAccounts) {
        const page = await api.listAccounts(500);
        if (token !== this.directoryToken) return;
        this.accounts.set(
          page.items
            .filter((account) => account.accountId !== me && !account.disabledAt)
            .map((account) => ({
              id: account.accountId,
              label: account.displayName || account.username,
              detail: account.username,
              kind: account.kind,
            })),
        );
      }
      if (canGroups && api.listPermissionGroups) {
        const page = await api.listPermissionGroups(200);
        if (token !== this.directoryToken) return;
        this.groups.set(page.items.map((group) => ({ id: group.groupId, label: group.name })));
      }
    } catch (error) {
      if (token !== this.directoryToken) return;
      this.directoryError.set(failureOf(error).message);
    } finally {
      if (token === this.directoryToken) this.directoryLoading.set(false);
    }
  }

  candidates(): ShareCandidate[] {
    return this.subjectType() === "account" ? this.accounts() : this.groups();
  }

  setSubjectType(type: "account" | "group"): void {
    if (this.subjectType() === type) return;
    this.subjectType.set(type);
    this.subjectId.set(null);
  }

  /** Write the current draft. A duplicate tuple is an honest success
   * (`created:false`) and says so; the list is re-read either way. */
  async add(): Promise<boolean> {
    const target = this.target();
    const api = this.api();
    const subjectId = this.subjectId();
    if (!target || !subjectId || !api?.addShare) return false;
    this.saving.set(true);
    this.actionError.set(null);
    this.notice.set(null);
    const input: ShareInput = {
      subjectType: this.subjectType() as ShareSubjectType,
      subjectId,
      relation: this.relation(),
    };
    try {
      const result = await api.addShare(target.kind, target.resourceId, input);
      this.notice.set(result.created === false ? "这条共享已经存在。" : "已共享。");
      this.subjectId.set(null);
      await this.load();
      this.refreshBadge();
      return true;
    } catch (error) {
      this.actionError.set(failureOf(error).message);
      return false;
    } finally {
      this.saving.set(false);
    }
  }

  /** Confirm is a separate step: the dialog opens the kit's AlertDialog. */
  requestRevoke(share: ShareView): void {
    this.revokeCandidate.set(share);
  }

  cancelRevoke(): void {
    this.revokeCandidate.set(null);
  }

  async confirmRevoke(): Promise<void> {
    const target = this.target();
    const share = this.revokeCandidate();
    const api = this.api();
    if (!target || !share || !api?.revokeShare) return;
    const key = `${share.subjectType}:${share.subjectId}:${share.relation}`;
    this.revoking.set(key);
    this.actionError.set(null);
    this.notice.set(null);
    try {
      await api.revokeShare(target.kind, target.resourceId, {
        subjectType: share.subjectType,
        subjectId: share.subjectId,
        relation: share.relation,
      });
      this.revokeCandidate.set(null);
      this.notice.set("已撤销共享。");
      // Re-read: the dialog shows the server's list immediately, not a local
      // approximation of it.
      await this.load();
      this.refreshBadge();
    } catch (error) {
      this.revokeCandidate.set(null);
      this.actionError.set(failureOf(error).message);
    } finally {
      this.revoking.set(null);
    }
  }

  /** Keep the Bot/TeamBot list badge in step with the share dialog without a
   * second list read; the badge is display state, the next list read is the
   * server's truth. */
  private refreshBadge(): void {
    const target = this.target();
    if (!target) return;
    const count = this.shares().length;
    if (target.kind === "bot") {
      botAccess.bots.update((rows) =>
        rows.map((row) => (row.botId === target.resourceId ? { ...row, sharedCount: count } : row)),
      );
      botAccess.botDetail.update((row) =>
        row && row.botId === target.resourceId ? { ...row, sharedCount: count } : row,
      );
    } else if (target.kind === "team_bot") {
      botAccess.teams.update((rows) =>
        rows.map((row) => (row.teamBotId === target.resourceId ? { ...row, sharedCount: count } : row)),
      );
      botAccess.teamDetail.update((row) =>
        row && row.teamBotId === target.resourceId ? { ...row, sharedCount: count } : row,
      );
    }
  }

  /** Re-read the open dialog after an invalidation fact (change
   * keel-harness-v2's identity stream). A closed dialog costs nothing; an
   * account/group change also refreshes the subject directory. */
  async refreshForEvent(kinds: ReadonlySet<string>): Promise<void> {
    const target = this.target();
    if (!target) return;
    const family =
      target.kind === "session" ? "sessions" : target.kind === "bot" ? "bots" : "teams";
    const directory = kinds.has("accounts") || kinds.has("permissions");
    if (!kinds.has(family) && !directory) return;
    await this.load();
    if (directory) await this.loadDirectory();
  }
}

export const shares = new ShareStore();

/** The three entry points the screens call. */
export const openSessionShare = (resourceId: string, name: string): void =>
  shares.open({ kind: "session", resourceId, name, visibilityOnly: false });

export const openBotShare = (resourceId: string, name: string): void =>
  shares.open({ kind: "bot", resourceId, name, visibilityOnly: false });

export const openTeamBotShare = (resourceId: string, name: string): void =>
  shares.open({ kind: "team_bot", resourceId, name, visibilityOnly: true });

/** Test alias. */
export const resetSharesForTest = (): void => shares.close();
