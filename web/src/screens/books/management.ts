// The management doors — 身份 / 记录 / 员工 / 团队, and the daily 任务 chapter.
//
// The daily rail carries 工作区 / 任务 / 员工 / 团队; 身份 and 记录 are the
// settings page's management entries. Both surfaces read the same `canAny`
// gates here, so a deep link, the rail and the settings card cannot disagree
// about who may see a destination. This is the display half only; the server
// remains the authority.
import { canAny } from "../../data/permissions";

export type ManagementDoor = "identity" | "records" | "bots" | "teams";

/** The keys that open the daily 任务 (board) chapter. `board.read` is the
 * product key; `task.read` keeps the section visible through the migration
 * window (the page then explains the one missing grant). Both are display
 * gates only; the server decides every read and write. */
export const TASK_KEYS = ["board.read", "task.read"];

/** The keys that open each door, in the product's own vocabulary. */
export const MANAGEMENT_KEYS: Record<ManagementDoor, string[]> = {
  identity: ["account.read", "permission.read"],
  records: ["audit.read"],
  bots: ["bot.read", "bot.manage"],
  teams: ["team_bot.read", "team_bot.manage"],
};

/** Which doors this account may open, read at render time. */
export function managementDoors(): Record<ManagementDoor, boolean> {
  return {
    identity: canAny(MANAGEMENT_KEYS.identity),
    records: canAny(MANAGEMENT_KEYS.records),
    bots: canAny(MANAGEMENT_KEYS.bots),
    teams: canAny(MANAGEMENT_KEYS.teams),
  };
}
