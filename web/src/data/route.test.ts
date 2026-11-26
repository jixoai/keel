// The route table — change keel-bot-identity-access, A4.4a.
//
// A deep link and the address bar must name the same chapter/object the shell
// draws; these cases pin the mapping for every chapter this slice adds and for
// the pre-existing settings/plugins routes.
import { expect, test } from "nsh/test";
import { resolveRoute, routePath, routeState } from "./route";

test("路由：章节与深链一一对应", () => {
  const at = (path: string, dev = false) => resolveRoute(path, dev);

  expect(at("/").route).toBe("workspace");
  expect(at("/unknown").route).toBe("workspace");
  expect(at("/settings").route).toBe("settings");
  expect(at("/plugins").route).toBe("plugins");
  expect(at("/kit").route).toBe("workspace");
  expect(at("/kit", true).route).toBe("kit");

  expect(at("/bots").route).toBe("bots");
  expect(at("/teams").route).toBe("teams");
  const board = at("/superagent");
  expect(board.route).toBe("superagent");
  expect(board.objectId).toBe(null);
  expect(at("/superagent/ticket-1").objectId).toBe("ticket-1");
  expect(at("/bots/bot-1").objectId).toBe("bot-1");
  const team = at("/teams/team-1");
  expect(team.route).toBe("teams");
  expect(team.objectId).toBe("team-1");
  expect(team.runId).toBe(null);
  const run = at("/teams/team-1/runs/run-1");
  expect(run.route).toBe("teams");
  expect(run.objectId).toBe("team-1");
  expect(run.runId).toBe("run-1");

  const identity = at("/identity");
  expect(identity.route).toBe("identity");
  expect(identity.section).toBe("accounts");
  expect(identity.objectId).toBe(null);
  expect(at("/identity/accounts").section).toBe("accounts");
  expect(at("/identity/groups").section).toBe("groups");
  expect(at("/identity/accounts/acc-1").objectId).toBe("acc-1");
  expect(at("/identity/groups/grp-1").objectId).toBe("grp-1");

  const records = at("/records/ev-7");
  expect(records.route).toBe("records");
  expect(records.objectId).toBe("ev-7");
});

test("路由：路径生成与反解一致（含编码）", () => {
  expect(routePath(routeState("workspace"))).toBe("/");
  expect(routePath(routeState("identity"))).toBe("/identity/accounts");
  expect(routePath(routeState("identity", "groups"))).toBe("/identity/groups");
  expect(routePath(routeState("identity", "groups", "a/b"))).toBe("/identity/groups/a%2Fb");
  expect(resolveRoute("/identity/groups/a%2Fb").objectId).toBe("a/b");
  expect(routePath(routeState("records", "accounts", "ev-1"))).toBe("/records/ev-1");
  expect(routePath(routeState("bots", "accounts", "bot-1"))).toBe("/bots/bot-1");
  expect(routePath(routeState("teams", "accounts", "team-1"))).toBe("/teams/team-1");
  expect(routePath(routeState("teams", "accounts", "team-1", "run-1"))).toBe("/teams/team-1/runs/run-1");
  expect(resolveRoute("/teams/team-1/runs/run-1").runId).toBe("run-1");
  expect(routePath(routeState("superagent"))).toBe("/superagent");
  expect(routePath(routeState("superagent", "accounts", "ticket-1"))).toBe("/superagent/ticket-1");
});
