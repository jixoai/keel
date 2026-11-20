// TUI harness · the session header's team face (change keel-agent-team-v1,
// P4 tail). The header pieces are mounted alone with the store's session
// marker seeded, so the roster dialog's five states can be checked without a
// whole workspace: the scenario comes from `host.params.scenario`.
//
// The agent registry is answered through the store's transport seam (the real
// `KeelHttp` parsing runs; only `fetch` is stubbed): `list`, `empty` (the
// team has no members), `loading` (the read never settles), `error` (500) and
// `permission` (403).
import { host, mount, View } from "nsh";
import type { AgentTeamView, AgentView } from "../data/contract";
import { KeelHttp, type FetchLike } from "../data/http";
import { store } from "../data/store";
import { SessionTeamChip, SessionTeamFacts, SessionTeamRosterDialog } from "./session-team";

const scenario = (): string => String(host.params.scenario ?? "list");

const respond = (status: number, body: unknown): Response =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

const pending = (): Promise<Response> => new Promise<Response>(() => {});

const seedAgents = (): AgentView[] => [
  {
    id: "agent_design",
    name: "设计师",
    description: "负责界面与品牌视觉",
    avatar: { kind: "identicon", seed: "agent_design" },
  },
  {
    id: "agent_research",
    name: "研究员",
    description: "负责资料检索与整理",
    avatar: { kind: "identicon", seed: "agent_research" },
  },
];

const team = (): AgentTeamView => ({
  id: "team_brand",
  name: "品牌发布小组",
  description: "负责新品发布的全链路交付",
  captain: { agentId: "agent_design" },
  members: scenario() === "empty" ? [] : [{ agentId: "agent_research" }],
  status: "valid",
  revision: 1,
  hash: "h-brand",
});

const stub: FetchLike = async (input, init) => {
  const method = (init?.method ?? "GET").toUpperCase();
  if (method === "GET" && input === "/v1/agents") {
    if (scenario() === "loading") return pending();
    if (scenario() === "error") return respond(500, { error: "成员资料读取失败：注册表不可用" });
    if (scenario() === "permission") return respond(403, { error: "当前账号没有 agent.read 权限" });
    return respond(200, { agents: seedAgents() });
  }
  return respond(404, { error: `no route for ${method} ${input}` });
};

store.activeId.set("session_demo");
store.setSessionSelection({ providerId: "s2api", modelId: "deepseek-v4.1-flash", reasoningLevel: "high" });
store.setSessionTeam(team());
// The picker normally loads the registry before a team can be chosen; seed it
// for the states that are about the roster, and leave it empty for the states
// that are about the roster's own read.
if (scenario() !== "loading" && scenario() !== "error" && scenario() !== "permission") {
  store.agents.set(seedAgents());
}
store.setAgentTeamTransport(new KeelHttp("", stub));

function Harness() {
  return (
    <View style={{ flex: 1, flexDirection: "column", gap: 8, padding: 8 }}>
      <SessionTeamFacts compact />
      <SessionTeamChip compact />
      <SessionTeamRosterDialog />
    </View>
  );
}

mount(Harness);
