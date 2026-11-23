// The management doors' gates: the rail and the settings page read one map.
//
// The daily rail shows 员工 / 团队 only when the account can open them; the
// settings card shows 身份 / 记录 under the same rule. This test pins the
// vocabulary so a future key rename cannot silently un-gate a door.
import { expect, test } from "nsh/test";
import { resetPermissions, setEffectivePermissions } from "../../data/permissions";
import type { PermissionScope } from "../../data/identity";
import { MANAGEMENT_KEYS, managementDoors } from "./management";

const keys = (values: Record<string, string>): Record<string, PermissionScope> =>
  values as Record<string, PermissionScope>;

test("管理入口：四个门都由 canAny 能力键打开，没有身份枚举", () => {
  resetPermissions();
  expect(managementDoors()).toEqual({ identity: false, records: false, bots: false, teams: false });

  setEffectivePermissions(keys({ "account.read": "tenant" }), false);
  expect(managementDoors().identity).toBe(true);
  expect(managementDoors().records).toBe(false);

  resetPermissions();
  setEffectivePermissions(keys({ "permission.read": "team" }), false);
  expect(managementDoors().identity).toBe(true);

  resetPermissions();
  setEffectivePermissions(keys({ "audit.read": "tenant" }), false);
  expect(managementDoors().records).toBe(true);
  expect(managementDoors().identity).toBe(false);

  resetPermissions();
  setEffectivePermissions(keys({ "bot.manage": "team", "team_bot.read": "team" }), false);
  const doors = managementDoors();
  expect(doors.bots).toBe(true);
  expect(doors.teams).toBe(true);

  // The keys are the product's vocabulary, never an account kind.
  expect(MANAGEMENT_KEYS.identity).toEqual(["account.read", "permission.read"]);
  expect(MANAGEMENT_KEYS.records).toEqual(["audit.read"]);
  resetPermissions();
});
