// The team session's selection fact (change keel-bot-identity-access, A4.2b3a).
//
// A team session must send BOTH the model mirror and the execution binding: the
// server resolves the captain (prompt, tools, default model) from teamBotId, and
// the model key keeps the picker's surface decision on the wire. This file runs
// as a plain unit test beside the data layer, like tool-families.test.ts.
import { expect, test } from "nsh/test";
import type { ModelSelection } from "./contract";
import { selectionForSend } from "./selection";

test("selectionForSend: 团队会话同时带 modelId 与 teamBotId", () => {
  const model: ModelSelection = { providerId: "s2api", modelId: "deepseek-v4.1-flash", reasoningLevel: "high" };
  expect(selectionForSend(model, "team_demo_brand")).toEqual({
    providerId: "s2api",
    modelId: "deepseek-v4.1-flash",
    reasoningLevel: "high",
    teamBotId: "team_demo_brand",
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
  expect(selectionForSend(null, "team_demo_brand")).toBe(null);
});

test("selectionForSend: 团队 id 为空白不发明绑定", () => {
  const model: ModelSelection = { modelId: "m" };
  expect(selectionForSend(model, "   ")).toEqual({ modelId: "m" });
});

test("selectionForSend: 不再下发旧 agentId / teamId 词表", () => {
  const model: ModelSelection = { providerId: "s2api", modelId: "deepseek-v4.1-flash" };
  const selection = selectionForSend(model, "team_demo_brand");
  expect(selection?.teamBotId).toBe("team_demo_brand");
  expect("agentId" in (selection ?? {})).toBe(false);
  expect("teamId" in (selection ?? {})).toBe(false);
});
