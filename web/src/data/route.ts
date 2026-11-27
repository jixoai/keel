// The app's route model — change keel-bot-identity-access, A4.4a/A4.4b.
//
// Pure functions, no transport: a page path becomes a chapter plus its own
// parameters (the identity sub-page, the open object, an open team run), and
// vice versa. The store holds the signals and calls these; a test can exercise
// the deep-link table without booting the app.
export type Route =
  | "workspace"
  | "settings"
  | "plugins"
  | "kit"
  | "board"
  | "gone"
  | "bots"
  | "teams"
  | "identity"
  | "records";

/** The identity chapter's two sub-pages (the 280 index column's segmented). */
export type IdentitySection = "accounts" | "groups";

/** The board's three views. `/board` and `/board/:id` are the queue/thread;
 * `/board/sync` and `/board/automation` are the management faces. */
export type BoardView = "queue" | "sync" | "automation";

/** What the route carries besides its chapter: the identity sub-page, the open
 * object (an account/group/bot/team/record/task id), for a team the open run,
 * and the board's view. Every chapter owns one `objectId`; a chapter without
 * objects leaves it null. */
export interface RouteState {
  route: Route;
  section: IdentitySection;
  objectId: string | null;
  /** `/teams/:teamId/runs/:runId` — the captain session id. */
  runId: string | null;
  /** `/board`, `/board/sync`, `/board/automation`. */
  boardView: BoardView;
}

export const routeState = (
  route: Route,
  section: IdentitySection = "accounts",
  objectId: string | null = null,
  runId: string | null = null,
  boardView: BoardView = "queue",
): RouteState => ({
  route,
  section,
  objectId,
  runId,
  boardView,
});

const decodePart = (part: string | undefined): string | null => {
  if (!part) return null;
  try {
    return decodeURIComponent(part);
  } catch {
    return part;
  }
};

/** The route a page path names: the web host's `keel.route` answer and its
 * back/forward events both arrive as paths, so a deep link
 * (`/board/:taskId`, `/identity/accounts/:id`, `/records/:id`, `/bots/:id`,
 * `/teams/:id/runs/:runId`, `/settings`, `/plugins`) and the address bar agree
 * with the shell. `/kit` is the kit gallery — a developer route, mapped only
 * for a dev launch (`?fixture=1` / `host.params.fixture`); no product link
 * enters it and a product URL falls back to the workspace. The retired
 * `/superagent*` addresses are `/gone` (404), never a redirect. */
export const resolveRoute = (path: string, dev = false): RouteState => {
  const bare = path.split("?")[0].split("#")[0];
  if (bare.startsWith("/settings")) return routeState("settings");
  if (bare.startsWith("/plugins")) return routeState("plugins");
  if (dev && bare.startsWith("/kit")) return routeState("kit");
  if (bare === "/superagent" || bare.startsWith("/superagent/")) {
    return routeState("gone");
  }
  if (bare === "/board" || bare.startsWith("/board/")) {
    const parts = bare.split("/").filter((part) => part !== "");
    if (parts[1] === "sync") return routeState("board", "accounts", null, null, "sync");
    if (parts[1] === "automation") return routeState("board", "accounts", null, null, "automation");
    return routeState("board", "accounts", decodePart(parts[1]));
  }
  if (bare === "/identity" || bare.startsWith("/identity/")) {
    const parts = bare.split("/").filter((part) => part !== "");
    const section: IdentitySection = parts[1] === "groups" ? "groups" : "accounts";
    return routeState("identity", section, decodePart(parts[2]));
  }
  if (bare === "/records" || bare.startsWith("/records/")) {
    const parts = bare.split("/").filter((part) => part !== "");
    return routeState("records", "accounts", decodePart(parts[1]));
  }
  if (bare === "/bots" || bare.startsWith("/bots/")) {
    const parts = bare.split("/").filter((part) => part !== "");
    return routeState("bots", "accounts", decodePart(parts[1]));
  }
  if (bare === "/teams" || bare.startsWith("/teams/")) {
    const parts = bare.split("/").filter((part) => part !== "");
    const teamId = decodePart(parts[1]);
    const runId = parts[2] === "runs" ? decodePart(parts[3]) : null;
    return routeState("teams", "accounts", teamId, runId);
  }
  return routeState("workspace");
};

/** The path a route state names (the address bar's form). A board view other
 * than the queue is a path segment; its filters travel in the search (the
 * store appends them). */
export const routePath = (state: RouteState): string => {
  switch (state.route) {
    case "settings":
      return "/settings";
    case "plugins":
      return "/plugins";
    case "kit":
      return "/kit";
    case "gone":
      return state.objectId ? `/superagent/${encodeURIComponent(state.objectId)}` : "/superagent";
    case "board": {
      if (state.boardView === "sync") return "/board/sync";
      if (state.boardView === "automation") return "/board/automation";
      return state.objectId ? `/board/${encodeURIComponent(state.objectId)}` : "/board";
    }
    case "bots":
      return state.objectId ? `/bots/${encodeURIComponent(state.objectId)}` : "/bots";
    case "teams": {
      if (!state.objectId) return "/teams";
      const base = `/teams/${encodeURIComponent(state.objectId)}`;
      return state.runId ? `${base}/runs/${encodeURIComponent(state.runId)}` : base;
    }
    case "identity": {
      const section = state.section === "groups" ? "groups" : "accounts";
      return `/identity/${section}${state.objectId ? `/${encodeURIComponent(state.objectId)}` : ""}`;
    }
    case "records":
      return `/records${state.objectId ? `/${encodeURIComponent(state.objectId)}` : ""}`;
    case "workspace":
    default:
      return "/";
  }
};
