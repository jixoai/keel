// The share surface's display contract: owner gating and labels. The server is
// still the authority; these tests pin the one fact the UI needs before it
// offers the action — key held and the caller owns the resource (or is a
// deployment super admin).
import { expect, test } from "nsh/test";
import { resetPermissions, setEffectivePermissions } from "./permissions";
import { canShareResource, relationLabel, sharedBadgeOf } from "./shares";
import { store } from "./store";

const me = (accountId: string, isSuperAdmin = false) =>
  store.identity.set({
    tenantId: "t-1",
    accountId,
    disabled: false,
    isSuperAdmin,
    permissions: {},
  });

test("share gating: key + ownership; a super admin passes; an editor key alone does not", () => {
  me("a-1");
  setEffectivePermissions({ "session.share": "team", "bot.manage": "tenant" }, false);
  expect(canShareResource("session", "a-1")).toBe(true);
  expect(canShareResource("session", "a-2")).toBe(false);
  expect(canShareResource("bot", "a-1")).toBe(true);
  expect(canShareResource("team_bot", "a-1")).toBe(false);

  // A key with no owner relation (or no key at all) never offers the action.
  setEffectivePermissions({ "session.write": "team" }, false);
  expect(canShareResource("session", "a-1")).toBe(false);

  me("a-2", true);
  setEffectivePermissions({}, true);
  expect(canShareResource("session", "a-1")).toBe(true);
  resetPermissions();
  store.identity.set(null);
});

test("share labels: viewer/editor words and the 已共享 badge only above zero", () => {
  expect(relationLabel("viewer")).toBe("可查看");
  expect(relationLabel("editor")).toBe("可编辑");
  expect(sharedBadgeOf(0)).toBe(undefined);
  expect(sharedBadgeOf(undefined)).toBe(undefined);
  expect(sharedBadgeOf(2)).toBe("已共享 2");
});
