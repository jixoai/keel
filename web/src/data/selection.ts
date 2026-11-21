// The selection fact one input carries (change keel-bot-identity-access,
// A4.2b3a vocabulary).
//
// The server owns the precedence (input selection > Bot/TeamBot default model >
// tenant default) and the execution binding (`botId` / `teamBotId`); the client
// only records what the session chose. This module is the one place that
// composes the wire fact, so the picker and the composer cannot disagree about
// what a team session sends.
//
// The old `agentId` / `teamId` tokens are refused by the server (HTTP 400, no
// fallback). Nothing here writes them.
import type { ModelSelection } from "./contract";

/**
 * selectionForSend composes the input's selection fact: the session's model
 * choice plus the execution binding of the team it started as.
 *
 * A TeamBot session sends BOTH keys: the model lets the picker's surface
 * decision stay visible on the wire, and `teamBotId` is what makes the run
 * resolve the captain's prompt, tools and (as a fallback) default model. With no
 * TeamBot the fact is the model choice alone; with neither there is no fact to
 * send and the server resolves the tenant default (absence is a distinct fact
 * from "an empty object").
 *
 * The parameter is a TeamBot id, never the legacy AgentTeam view: the server's
 * `teamId` token is gone, and an AgentTeam id is not a TeamBot id. A caller
 * without a real TeamBot passes null instead of inventing a binding.
 */
export function selectionForSend(
  model: ModelSelection | null,
  teamBotId: string | null,
): ModelSelection | null {
  if (!model) return null;
  const selection: ModelSelection = { ...model };
  const id = teamBotId?.trim();
  if (id) selection.teamBotId = id;
  return selection;
}
