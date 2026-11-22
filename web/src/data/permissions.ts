// The permission gate (`can(key, resource?)`) — change
// keel-bot-identity-access, A4.3.
//
// This is the *display* half of the server's authority: it answers "may the
// UI offer this action?" from the effective `(key, scope)` set in
// `GET /v1/auth/whoami`. The server decides every request; a `can()` answer is
// never a security boundary and a hidden button is never the enforcement.
//
// The two gates are deliberately separate modules with separate vocabularies:
//   - `capabilities.can(feature)` is "does this deployment/client support the
//     surface?" (`mcp.apps`, …);
//   - this `can(key)` is "does this account hold the permission key?".
// A screen that needs both asks both.
import { signal } from "nsh";
import type { PermissionScope } from "./identity";
import { scopeRank } from "./identity";

/** The relation a listing reported for one resource, when it reported one.
 * The server filters lists by relation; the UI uses it only to hide writes a
 * `viewer` could not perform (the server still answers 403). */
export interface PermissionResource {
  relation?: "owner" | "editor" | "viewer" | "team_member";
  /** An archived resource accepts no new work (new sessions, keys, starts). */
  archived?: boolean;
}

const effective = signal<Record<string, PermissionScope>>({});
const superAdmin = signal(false);

/** Record the whoami answer (or clear it on logout). */
export function setEffectivePermissions(permissions: Record<string, PermissionScope>, isSuperAdmin: boolean): void {
  effective.set({ ...permissions });
  superAdmin.set(isSuperAdmin === true);
}

export function resetPermissions(): void {
  effective.set({});
  superAdmin.set(false);
}

/** The whole effective set, for diagnostics or a permission matrix. */
export function effectivePermissions(): Record<string, PermissionScope> {
  return effective();
}

/** The scope the caller holds for one key, if any. A super admin holds `all`. */
export function scopeOf(key: string): PermissionScope | undefined {
  const held = effective()[key];
  if (held) return held;
  return superAdmin() ? "all" : undefined;
}

/** A read-only key (`session.read`, `secret.read.metadata`, …). Writes on a
 * viewer relation and on an archived resource are hidden; the server's own
 * relation check stays authoritative. */
const isReadKey = (key: string): boolean => key.endsWith(".read") || key.includes(".read.");

/** Does this account hold `key` (possibly on `resource`)?
 *
 * - no key at all → false, whatever the resource;
 * - super admin → true (the server short-circuits it the same way);
 * - a write/manage/invoke key on a `viewer` relation or an archived resource →
 *   false (the action cannot succeed, so the UI does not offer it).
 *
 * A resource that the listing already returned is visible by construction; the
 * optional relation is used only to avoid offering a write the server would
 * refuse.
 */
export function can(key: string, resource?: PermissionResource): boolean {
  if (!key) return false;
  if (resource?.archived && !isReadKey(key)) return false;
  if (resource?.relation === "viewer" && !isReadKey(key)) return false;
  return scopeOf(key) !== undefined;
}

/** Is any of these keys held? For a section that has more than one entry
 * point (read or manage). */
export function canAny(keys: string[], resource?: PermissionResource): boolean {
  return keys.some((key) => can(key, resource));
}

/** The scope's breadth, for a matrix that sorts by `own < team < tenant < all`. */
export function heldRank(key: string): number {
  const scope = scopeOf(key);
  return scope === undefined ? -1 : scopeRank(scope);
}

/** The sentence a 403 page shows: what is missing, not a bare "forbidden". */
export function permissionHint(key: string): string {
  return key ? `需要 ${key} 权限；请联系管理员授予，或换一个有权限的账号。` : "你的账号没有执行这个操作的权限。";
}
