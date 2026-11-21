// The app's route model — change keel-bot-identity-access, A4.4a.
//
// Pure functions, no transport: a page path becomes a chapter plus its own
// parameters (the identity sub-page, the open object), and vice versa. The store
// holds the signals and calls these; a test can exercise the deep-link table
// without booting the app.
export type Route = "workspace" | "settings" | "plugins" | "kit" | "bots" | "teams" | "identity" | "records";

/** The identity chapter's two sub-pages (the 280 index column's segmented). */
export type IdentitySection = "accounts" | "groups";

/** What the route carries besides its chapter: the identity sub-page and the
 * open object (an account/group id, a record id). Every chapter owns one
 * `objectId`; a chapter without objects leaves it null. */
export interface RouteState {
  route: Route;
  section: IdentitySection;
  objectId: string | null;
}

export const routeState = (route: Route, section: IdentitySection = "accounts", objectId: string | null = null): RouteState => ({
  route,
  section,
  objectId,
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
 * (`/identity/accounts/:id`, `/records/:id`, `/settings`, `/plugins`) and the
 * address bar agree with the shell. `/kit` is the kit gallery — a developer
 * route, mapped only for a dev launch (`?fixture=1` / `host.params.fixture`);
 * no product link enters it and a product URL falls back to the workspace. */
export const resolveRoute = (path: string, dev = false): RouteState => {
  if (path.startsWith("/settings")) return routeState("settings");
  if (path.startsWith("/plugins")) return routeState("plugins");
  if (dev && path.startsWith("/kit")) return routeState("kit");
  if (path === "/identity" || path.startsWith("/identity/")) {
    const parts = path.split("/").filter((part) => part !== "");
    const section: IdentitySection = parts[1] === "groups" ? "groups" : "accounts";
    return routeState("identity", section, decodePart(parts[2]));
  }
  if (path === "/records" || path.startsWith("/records/")) {
    const parts = path.split("/").filter((part) => part !== "");
    return routeState("records", "accounts", decodePart(parts[1]));
  }
  if (path.startsWith("/bots")) return routeState("bots");
  if (path.startsWith("/teams")) return routeState("teams");
  return routeState("workspace");
};

/** The path a route state names (the address bar's form). */
export const routePath = (state: RouteState): string => {
  switch (state.route) {
    case "settings":
      return "/settings";
    case "plugins":
      return "/plugins";
    case "kit":
      return "/kit";
    case "bots":
      return "/bots";
    case "teams":
      return "/teams";
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
