// TUI harness · the captain timeline's team face (change
// keel-session-surfaces-v1, S4). Two mounts share the same store facts:
//
//   · `panel` — the team activity region alone, for the five states;
//   · `transcript` — the real `Conversation` with a synthetic captain turn, so
//     the isolation check can see the card region and the transcript together.
//
// The member task read runs through the real `KeelHttp` parsing; only `fetch`
// is stubbed, exactly like the settings/roster harnesses.
import { host, mount, View } from "nsh";
import type { AgentTeamView, ConversationView, TeamTaskView } from "../data/contract";
import { KeelHttp, type FetchLike } from "../data/http";
import { store } from "../data/store";
import { Conversation } from "./conversation";
import { TeamActivityPanel } from "./team-activity";

const scenario = (): string => String(host.params.scenario ?? "success");

const respond = (status: number, body: unknown): Response =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

const pending = (): Promise<Response> => new Promise<Response>(() => {});

const task = (overrides: Partial<TeamTaskView> & Pick<TeamTaskView, "taskId" | "title" | "status">): TeamTaskView => ({
  parentTaskId: "task_captain",
  sessionId: "session_member",
  origin: "subagent",
  createdAt: new Date(Date.now() - 5 * 60_000).toISOString(),
  updatedAt: new Date(Date.now() - 2 * 60_000).toISOString(),
  ...overrides,
});

const tasks = (): TeamTaskView[] => [
  task({ taskId: "task_research", title: "检索竞品资料", status: "in_progress" }),
  task({ taskId: "task_publish", title: "整理发布清单", status: "done" }),
];

const stub: FetchLike = async (input, init) => {
  const method = (init?.method ?? "GET").toUpperCase();
  if (method === "GET" && input.startsWith("/v1/sessions/session_team_demo/team-tasks")) {
    if (scenario() === "loading") return pending();
    if (scenario() === "error") return respond(500, { error: "任务投影不可用" });
    if (scenario() === "permission") return respond(403, { error: "当前账号没有 task.read 权限" });
    if (scenario() === "empty") return respond(200, { tasks: [], nextCursor: "", hasMore: false });
    return respond(200, { tasks: tasks(), nextCursor: "", hasMore: false });
  }
  return respond(404, { error: `no route for ${method} ${input}` });
};

const team: AgentTeamView = {
  id: "team_demo",
  name: "品牌发布小组",
  captain: { agentId: "agent_design", label: "设计师" },
  members: [{ agentId: "agent_research", label: "研究员" }],
  status: "valid",
  revision: 1,
  hash: "h-team",
};

store.activeId.set("session_team_demo");
store.setSessionSelection({ providerId: "s2api", modelId: "deepseek-v4.1-flash" });
store.setTeamTaskTransport(new KeelHttp("", stub));
store.setSessionTeam(team);

const captainView = (): ConversationView => ({
  sessionId: "session_team_demo",
  turns: [
    {
      turnId: "turn_captain",
      ordinal: 1,
      origin: "user_prompt",
      status: "completed",
      visibleInputs: [
        {
          requestId: "request_captain",
          text: "请安排成员开始工作",
          delivery: "queue",
          createdAt: Date.now() - 60_000,
        },
      ],
      workSegments: [
        {
          segmentId: "segment_captain",
          toolCalls: [],
          assistantText: "CAPTAIN-REPLY-ONLY",
        },
      ],
      postTurnEvents: [],
      assistantOutput: "CAPTAIN-REPLY-ONLY",
    },
  ],
  decisions: [],
  queue: [],
  busy: false,
  stopping: false,
  connection: { status: "open", lastSeq: 7 },
});

if (scenario() === "transcript") store.conversation.set(captainView());

function PanelHarness() {
  return (
    <View style={{ flex: 1, flexDirection: "column", padding: 8 }}>
      <TeamActivityPanel />
    </View>
  );
}

mount(() => (scenario() === "transcript" ? <Conversation wide={false} /> : <PanelHarness />));
