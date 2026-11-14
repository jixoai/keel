// The selection fact one input carries (`keel-agent-team-v1`).
//
// The server owns the precedence (input selection > agent default model >
// tenant default) and the execution binding (agentId/teamId); the client only
// records what the session chose. This module is the one place that composes
// the wire fact, so the picker and the composer cannot disagree about what a
// team session sends.

import type { AgentTeamView, ModelSelection } from "./contract";

/**
 * selectionForSend composes the input's selection fact: the session's model
 * choice plus the execution binding of the team it started as.
 *
 * A team session sends BOTH keys: the model lets the picker's surface decision
 * stay visible on the wire, and teamId is what makes the run resolve the
 * captain's prompt, tools and (as a fallback) default model. With no team the
 * fact is the model choice alone; with neither there is no fact to send and the
 * server resolves the tenant default (absence is a distinct fact from "an empty
 * object").
 */
export function selectionForSend(
  model: ModelSelection | null,
  team: AgentTeamView | null,
): ModelSelection | null {
  if (!model) return null;
  const selection: ModelSelection = { ...model };
  const teamId = team?.id?.trim();
  if (teamId) selection.teamId = teamId;
  return selection;
}
