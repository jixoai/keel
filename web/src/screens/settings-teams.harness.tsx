// TUI harness · settings「团队」— the section alone, with the agent/team
// registry answered in memory through the store's transport seam (the real
// `KeelHttp` parsing runs; only `fetch` is stubbed).
//
// The scenario comes from the launch parameters (`host.params.scenario`):
// `list` (two teams), `empty`, `refuse` (the member/team write is refused) and
// `referenced` (a team delete is refused).
import { host, mount, View } from "nsh";
import type {
  AgentTeamView,
  AgentTeamWriteRequest,
  AgentView,
  TeamMemberView,
} from "../data/contract";
import { KeelHttp, type FetchLike } from "../data/http";
import { store } from "../data/store";
import { TeamsSection } from "./settings-agents";

const scenario = (): string => String(host.params.scenario ?? "list");

const respond = (status: number, body: unknown): Response =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

const empty = (): Response => new Response(null, { status: 204 });

const seedAgents = (): AgentView[] => [
  { id: "agent_design", name: "设计师", description: "负责界面与品牌视觉", avatar: { kind: "identicon", seed: "agent_design" } },
  { id: "agent_research", name: "研究员", description: "负责资料检索", avatar: { kind: "identicon", seed: "agent_research" } },
  { id: "agent_ops", name: "运营", description: "负责发布与沟通", avatar: { kind: "identicon", seed: "agent_ops" } },
];

const seedTeams = (): AgentTeamView[] => [
  {
    id: "team_brand",
    name: "品牌发布小组",
    description: "负责新品发布的全链路交付",
    captain: { agentId: "agent_design" },
    members: [{ agentId: "agent_research" }, { agentId: "agent_ops" }],
    status: "valid",
    revision: 3,
    hash: "h-brand",
  },
  {
    id: "team_research",
    name: "资料组",
    description: "只做检索与整理",
    captain: { agentId: "agent_research" },
    members: [],
    status: "valid",
    revision: 1,
  },
];

let agents: AgentView[] = seedAgents();
let teams: AgentTeamView[] = scenario() === "empty" ? [] : seedTeams();
let sequence = 0;

const memberOf = (agentId: string): TeamMemberView => ({ agentId });

const teamFrom = (id: string, request: AgentTeamWriteRequest): AgentTeamView => ({
  id,
  name: request.name,
  description: request.description,
  captain: request.captain,
  members: request.members ?? [],
  defaults: request.defaults,
  capabilities: request.capabilities,
  featureBindings: request.featureBindings,
  status: "valid",
  revision: 1,
});

const stub: FetchLike = async (input, init) => {
  const method = (init?.method ?? "GET").toUpperCase();
  const bodyText = typeof init?.body === "string" ? init.body : "{}";

  if (method === "GET" && input === "/v1/agents") return respond(200, { agents });
  if (method === "GET" && input === "/v1/teams") return respond(200, { teams });

  if (method === "POST" && input === "/v1/teams") {
    if (scenario() === "refuse") {
      return respond(400, { error: "队长 agent_design 当前不可用（归档或未配置模型）" });
    }
    const request = JSON.parse(bodyText) as AgentTeamWriteRequest;
    const created = teamFrom(`team_new_${++sequence}`, request);
    teams = [...teams, created];
    return respond(201, created);
  }

  if (method === "PUT" && input.startsWith("/v1/teams/")) {
    if (scenario() === "refuse") {
      return respond(400, { error: "队长 agent_design 当前不可用（归档或未配置模型）" });
    }
    const id = decodeURIComponent(input.slice("/v1/teams/".length));
    const request = JSON.parse(bodyText) as AgentTeamWriteRequest;
    const updated = teamFrom(id, request);
    teams = teams.map((team) => (team.id === id ? updated : team));
    return respond(200, updated);
  }

  if (method === "DELETE" && input.startsWith("/v1/teams/") && !input.includes("/members/")) {
    if (scenario() === "referenced") {
      return respond(409, { error: "团队「品牌发布小组」正在运行任务，暂不能删除。" });
    }
    const id = decodeURIComponent(input.slice("/v1/teams/".length));
    teams = teams.filter((team) => team.id !== id);
    return empty();
  }

  if (method === "POST" && /^\/v1\/teams\/[^/]+\/members$/.test(input)) {
    if (scenario() === "refuse") {
      return respond(400, { error: "队长不能作为普通成员添加" });
    }
    const id = decodeURIComponent(input.split("/")[3] ?? "");
    const request = JSON.parse(bodyText) as { agentId?: string };
    const agentId = String(request.agentId ?? "");
    teams = teams.map((team) =>
      team.id === id && agentId && !(team.members ?? []).some((member) => member.agentId === agentId)
        ? { ...team, members: [...(team.members ?? []), memberOf(agentId)] }
        : team,
    );
    return respond(200, teams.find((team) => team.id === id) ?? { id, name: "", captain: { agentId: "" } });
  }

  if (method === "DELETE" && /^\/v1\/teams\/[^/]+\/members\/[^/]+$/.test(input)) {
    if (scenario() === "refuse") {
      return respond(400, { error: "队长不可移除：请先改派队长" });
    }
    const [, , , rawId, , rawAgent] = input.split("/");
    const id = decodeURIComponent(rawId ?? "");
    const agentId = decodeURIComponent(rawAgent ?? "");
    teams = teams.map((team) =>
      team.id === id
        ? { ...team, members: (team.members ?? []).filter((member) => member.agentId !== agentId) }
        : team,
    );
    return respond(200, teams.find((team) => team.id === id) ?? { id, name: "", captain: { agentId: "" } });
  }

  return respond(404, { error: `no route for ${method} ${input}` });
};

/** The captain/member pickers read names from the agents store; the section
 * loads agents itself, so only the model/tool seeds a team editor never reads
 * are set here. */
store.registryLoaded.set(true);
store.providers.set([{ providerId: "s2api", name: "S2API" }]);
store.models.set([{ providerId: "s2api", modelId: "deepseek-v4.1-flash", selectable: true, executable: true }]);
store.plugins.set({ tools: [] });
store.setAgentTeamTransport(new KeelHttp("", stub));

function Harness() {
  return (
    <View style={{ flex: 1, flexDirection: "column", padding: 8 }}>
      <TeamsSection />
    </View>
  );
}

mount(Harness);
