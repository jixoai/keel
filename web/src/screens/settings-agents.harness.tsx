// TUI harness · settings「Agents」— the section alone, with the registry
// answered in memory.
//
// The product's transport is same-origin `globalThis.fetch`; an in-memory
// terminal has no origin for a relative URL to resolve against, so the harness
// installs a seam on the store (`setAgentTeamTransport`) holding a real
// `KeelHttp` whose fetch is the stub below. What is under test is still the
// real transport parsing and the section's rendering, not a second fake
// client. The scenario comes from the launch parameters
// (`host.params.scenario`), keeping one harness for every state the gate
// covers.
import { host, mount, View } from "nsh";
import type { AgentView, AgentWriteRequest } from "../data/contract";
import { KeelHttp, type FetchLike } from "../data/http";
import { store } from "../data/store";
import { AgentsSection } from "./settings-agents";

const scenario = (): string => String(host.params.scenario ?? "list");

const respond = (status: number, body: unknown): Response =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

const empty = (): Response => new Response(null, { status: 204 });

const seedAgents = (): AgentView[] => [
  {
    id: "agent_design",
    name: "设计师",
    description: "负责界面与品牌视觉",
    avatar: { kind: "identicon", seed: "agent_design" },
    defaultModel: { providerId: "s2api", modelId: "deepseek-v4.1-flash" },
    tools: [
      { toolId: "workspace.read", enabled: true },
      { toolId: "image.generate", enabled: true },
    ],
    systemPrompt: { kind: "inline", text: "你是资深视觉设计师。" },
  },
  {
    id: "agent_research",
    name: "研究员",
    description: "负责资料检索与整理",
    avatar: { kind: "identicon", seed: "agent_research" },
    defaultModel: { providerId: "s2api", modelId: "deepseek-v4.1-flash" },
    tools: [{ toolId: "web.fetch", enabled: true }],
  },
];

let agents: AgentView[] = scenario() === "empty" ? [] : seedAgents();
let sequence = 0;

const stub: FetchLike = async (input, init) => {
  const method = (init?.method ?? "GET").toUpperCase();
  const bodyText = typeof init?.body === "string" ? init.body : "{}";
  if (method === "GET" && input === "/v1/agents") return respond(200, { agents });

  if (method === "POST" && input === "/v1/agents") {
    if (scenario() === "refuse") {
      return respond(400, { error: "工具 image.generate 未安装，无法授予" });
    }
    const request = JSON.parse(bodyText) as AgentWriteRequest;
    const created: AgentView = {
      id: `agent_new_${++sequence}`,
      name: request.name,
      description: request.description,
      avatar: request.avatar,
      defaultModel: request.defaultModel,
      tools: request.tools,
      systemPrompt: request.systemPrompt,
    };
    agents = [...agents, created];
    return respond(201, created);
  }

  if (method === "PUT" && input.startsWith("/v1/agents/")) {
    const id = decodeURIComponent(input.slice("/v1/agents/".length));
    const request = JSON.parse(bodyText) as AgentWriteRequest;
    agents = agents.map((agent) =>
      agent.id === id
        ? { ...agent, name: request.name, description: request.description, tools: request.tools }
        : agent,
    );
    return respond(200, agents.find((agent) => agent.id === id) ?? { id, name: request.name });
  }

  if (method === "DELETE" && input.startsWith("/v1/agents/")) {
    if (scenario() === "referenced") {
      return respond(409, { error: "Agent 设计师 正被团队「品牌发布小组」引用，先移出团队再删除。" });
    }
    const id = decodeURIComponent(input.slice("/v1/agents/".length));
    agents = agents.filter((agent) => agent.id !== id);
    return empty();
  }

  return respond(404, { error: `no route for ${method} ${input}` });
};

setAgentSeeds();
store.setAgentTeamTransport(new KeelHttp("", stub));

function setAgentSeeds() {
  store.providers.set([{ providerId: "s2api", name: "S2API" }]);
  store.models.set([
    { providerId: "s2api", modelId: "deepseek-v4.1-flash", selectable: true, executable: true },
    { providerId: "s2api", modelId: "qwen3.8-flash", selectable: true, executable: true },
  ]);
  store.registryLoaded.set(true);
  store.plugins.set({ tools: ["workspace.read", "image.generate", "web.fetch"] });
}

function Harness() {
  return (
    <View style={{ flex: 1, flexDirection: "column", padding: 8 }}>
      <AgentsSection />
    </View>
  );
}

mount(Harness);
