// Starting a Session from the workspace.
//
// Every Session belongs to exactly one Bot (identity-access spec): the worker
// refuses an unowned session, so a "new session" button that posts `{}` is a
// dead end. This seam resolves the tenant's single active Bot and creates the
// session under it; with zero or several Bots it routes to the 员工 chapter,
// where the Bot is chosen explicitly (a picker belongs to that page, not to a
// workspace shortcut).
import { botAccess } from "./bots";
import type { BotView } from "./identity";
import { store } from "./store";

/** A workspace shortcut can name a Bot only when the tenant has exactly one
 * active employee; otherwise choosing is the 员工 page's job. */
export function resolveWorkspaceBot(bots: BotView[]): BotView | null {
  const active = bots.filter((bot) => !bot.archived);
  return active.length === 1 ? active[0]! : null;
}

export async function createSessionFromWorkspace(): Promise<string | null> {
  // The demo transport keeps its scripted sessions and has no real Bot to
  // own one; its own create path stays untouched.
  if (store.mode() !== "http") return store.createSession();

  botAccess.ensureBots();
  if (botAccess.bots().length === 0 && !botAccess.botsLoading()) {
    await botAccess.loadBots();
  }
  const bot = resolveWorkspaceBot(botAccess.bots());
  if (bot) return store.createSession(bot.botId);

  const active = botAccess.bots().filter((entry) => !entry.archived);
  store.notice.set(
    active.length
      ? { tone: "info", text: "这个租户有多位员工：到「员工」页选一位开始会话。" }
      : { tone: "danger", text: "还没有员工（Bot）：先在「员工」页创建一位，再开始会话。" },
  );
  store.openChapter("bots");
  return null;
}
