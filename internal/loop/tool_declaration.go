// Tool-loadout declaration ported from pi's agent-loop.ts
// (declareToolChanges / getToolStateChanges, pin 55a5c69, MIT License).
//
// `context.tools` is what the runtime can execute; the transcript's system
// messages declare what the model may call. Before each request the difference
// becomes toolsAdded/toolsRemoved on a system message. When a pending system
// message exists, its tool fields are treated as intent and replaced with the
// delta between the committed transcript and the executable set, so replay
// always yields exactly context.tools. Otherwise a new system message is
// inserted before the first non-system pending message.

package loop

import (
	"github.com/jixoai/keel/internal/agentcore"
)

// declareToolChanges returns pending with the tool-loadout delta declared.
// inserted lists the new system messages it created (never messages copied from
// pending), so the caller can add them to the run's produced messages.
func declareToolChanges(agentCtx *agentcore.AgentContext, pending []agentcore.AgentMessage) (out []agentcore.AgentMessage, inserted []agentcore.AgentMessage) {
	systemIndex := -1
	for i := len(pending) - 1; i >= 0; i-- {
		if _, ok := pending[i].(agentcore.SystemMessage); ok {
			systemIndex = i
			break
		}
	}

	baseline := pending
	var pendingSys *agentcore.SystemMessage
	if systemIndex >= 0 {
		sys := pending[systemIndex].(agentcore.SystemMessage)
		pendingSys = &sys
		baseline = make([]agentcore.AgentMessage, len(pending))
		copy(baseline, pending)
		// Treat the pending message's tool fields as intent, not as committed
		// state: compare against the transcript without them.
		zeroed := sys
		zeroed.ToolsAdded = nil
		zeroed.ToolsRemoved = nil
		baseline[systemIndex] = zeroed
	}

	// getCurrentTools([...context.messages, ...baseline]).
	combined := make([]agentcore.Message, 0, len(agentCtx.Messages)+len(baseline))
	combined = append(combined, agentCtx.Messages...)
	combined = append(combined, baseline...)
	current := agentcore.CurrentTools(combined)
	desired := agentcore.ToolDeclarations(agentCtx.Tools)
	added, removed := agentcore.GetToolStateChanges(current, desired)
	unchanged := len(added) == 0 && len(removed) == 0

	if pendingSys != nil {
		// Keep the caller's message object when it already declares no tool
		// changes.
		if unchanged && len(pendingSys.ToolsAdded) == 0 && len(pendingSys.ToolsRemoved) == 0 {
			return pending, nil
		}
		out = make([]agentcore.AgentMessage, len(baseline))
		copy(out, baseline)
		merged := *pendingSys
		merged.ToolsAdded = added
		merged.ToolsRemoved = removed
		out[systemIndex] = merged
		return out, nil
	}
	if unchanged {
		return pending, nil
	}

	update := agentcore.SystemMessage{
		RoleField:    agentcore.RoleSystem,
		Content:      "",
		ToolsAdded:   added,
		ToolsRemoved: removed,
		Timestamp:    nowMillis(),
	}
	// Insert before the first non-system pending message; append when every
	// pending message is a system message (or nothing is pending).
	index := len(pending)
	for i, m := range pending {
		if _, ok := m.(agentcore.SystemMessage); !ok {
			index = i
			break
		}
	}
	out = make([]agentcore.AgentMessage, 0, len(pending)+1)
	out = append(out, pending[:index]...)
	out = append(out, update)
	out = append(out, pending[index:]...)
	return out, []agentcore.AgentMessage{update}
}
