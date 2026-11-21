// `can(key, resource?)`: the display half of the permission system. The server
// is the authority; these cases pin the fail-closed rules the UI relies on.
import { expect, test } from "nsh/test";
import { can, canAny, effectivePermissions, heldRank, permissionHint, resetPermissions, scopeOf, setEffectivePermissions } from "./permissions";

test("can: no whoami answer means no action (fail closed)", () => {
  resetPermissions();
  expect(can("session.read")).toBe(false);
  expect(effectivePermissions()).toEqual({});
});

test("can: a held key is offered; an unheld one is not", () => {
  setEffectivePermissions({ "session.read": "own", "session.write": "own" }, false);
  expect(can("session.read")).toBe(true);
  expect(can("session.write")).toBe(true);
  expect(can("session.delete")).toBe(false);
  expect(scopeOf("session.read")).toBe("own");
  expect(scopeOf("session.delete")).toBe(undefined);
});

test("can: a super admin short-circuits every key", () => {
  setEffectivePermissions({}, true);
  expect(can("secret.read.value")).toBe(true);
  expect(scopeOf("audit.read")).toBe("all");
  expect(heldRank("audit.read")).toBe(3);
});

test("can: a viewer relation offers reads only", () => {
  setEffectivePermissions({ "bot.read": "tenant", "bot.manage": "tenant" }, false);
  expect(can("bot.read", { relation: "viewer" })).toBe(true);
  expect(can("bot.manage", { relation: "viewer" })).toBe(false);
  expect(can("bot.manage", { relation: "editor" })).toBe(true);
});

test("can: an archived resource accepts no new work", () => {
  setEffectivePermissions({ "bot.read": "tenant", "bot.invoke": "tenant" }, false);
  expect(can("bot.read", { archived: true })).toBe(true);
  expect(can("bot.invoke", { archived: true })).toBe(false);
  expect(can("bot.invoke", { archived: false })).toBe(true);
});

test("canAny: a section with more than one entry point", () => {
  setEffectivePermissions({ "bot.read": "own" }, false);
  expect(canAny(["bot.create", "bot.read"])).toBe(true);
  expect(canAny(["bot.create", "bot.manage"])).toBe(false);
});

test("permissionHint names the missing key", () => {
  expect(permissionHint("account.read")).toContain("account.read");
  expect(permissionHint("")).toContain("没有");
});
