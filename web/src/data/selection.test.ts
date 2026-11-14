// The team session's selection fact (`keel-agent-team-v1`, RUNTIME-PLAN P6).
//
// A team session must send BOTH the model mirror and the execution binding: the
// server resolves the captain (prompt, tools, default model) from teamId, and
// the model key keeps the picker's surface decision on the wire. This file runs
// as a plain unit test beside the data layer, like tool-families.test.ts.
import { expect, test } from "nsh/test";
import type { AgentTeamView, ModelSelection } from "./contract";
import { selectionForSend } from "./selection";

const team = (id: string, name = "品牌发布小组"): AgentTeamView => ({
  tenantId: "t",
  id,
  name,
  captain: { agentId: "agent_captain" },
  members: [],
  defaults: {},
  capabilities: { teamCli: { enabled: false } },
});

test("selectionForSend: 团队会话同时带 modelId 与 teamId", () => {
  const model: ModelSelection = { providerId: "s2api", modelId: "deepseek-v4.1-flash", reasoningLevel: "high" };
  expect(selectionForSend(model, team("team_demo_brand"))).toEqual({
    providerId: "s2api",
    modelId: "deepseek-v4.1-flash",
    reasoningLevel: "high",
    teamId: "team_demo_brand",
  });
});

test("selectionForSend: 标准会话只带模型", () => {
  const model: ModelSelection = { providerId: "s2api", modelId: "deepseek-v4.1-flash" };
  expect(selectionForSend(model, null)).toEqual({ providerId: "s2api", modelId: "deepseek-v4.1-flash" });
});

test("selectionForSend: 没有选择时不下发空事实（缺省 = 租户默认）", () => {
  expect(selectionForSend(null, null)).toBe(null);
  // A team marker without a session model cannot be expressed as a selection;
  // the server would have nothing to run, and the picker never produces it.
  expect(selectionForSend(null, team("team_demo_brand"))).toBe(null);
});

test("selectionForSend: 团队 id 为空白不发明绑定", () => {
  const model: ModelSelection = { modelId: "m" };
  expect(selectionForSend(model, team("   "))).toEqual({ modelId: "m" });
});
