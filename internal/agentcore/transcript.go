// Transcript helpers ported from pi's packages/ai/src/utils/transcript.ts
// (pin 55a5c69, MIT License), narrowed to Keel's message model: system prompt
// text and tool declarations live on SystemMessage entries, so a transcript can
// declare a tool loadout and later announce additions/removals.

package agentcore

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ToToolDeclaration strips a tool down to the interface the model sees. The
// executable payload (Execute) and display-only fields are not part of the
// declaration, mirroring pi's toToolDeclaration.
func ToToolDeclaration(tool AgentTool) ToolDeclaration {
	params := json.RawMessage(tool.Schema())
	if len(bytes.TrimSpace(params)) == 0 {
		params = json.RawMessage("{}")
	}
	return ToolDeclaration{
		Name:        tool.Name(),
		Description: tool.Description(),
		Parameters:  canonicalJSON(params),
	}
}

// ToolDeclarations maps tools to their model-facing declarations in order.
func ToolDeclarations(tools []AgentTool) []ToolDeclaration {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ToolDeclaration, 0, len(tools))
	for _, tool := range tools {
		out = append(out, ToToolDeclaration(tool))
	}
	return out
}

// CurrentTools resolves the tool loadout declared by a transcript: every
// system message's removals apply first, then its additions, in message order
// (pi's getCurrentTools).
func CurrentTools(messages []Message) []ToolDeclaration {
	byName := make(map[string]ToolDeclaration)
	var order []string
	for _, m := range messages {
		sys, ok := m.(SystemMessage)
		if !ok {
			continue
		}
		for _, ref := range sys.ToolsRemoved {
			delete(byName, ref.Name)
		}
		for _, decl := range sys.ToolsAdded {
			if _, exists := byName[decl.Name]; !exists {
				order = append(order, decl.Name)
			}
			byName[decl.Name] = decl
		}
	}
	out := make([]ToolDeclaration, 0, len(byName))
	for _, name := range order {
		if decl, ok := byName[name]; ok {
			out = append(out, decl)
		}
	}
	return out
}

// GetToolStateChanges compares two complete tool states. A changed definition
// is reported as a removal plus an addition (pi's getToolStateChanges).
func GetToolStateChanges(previous, current []ToolDeclaration) (added []ToolDeclaration, removed []ToolReference) {
	prev := make(map[string]ToolDeclaration, len(previous))
	for _, t := range previous {
		prev[t.Name] = t
	}
	cur := make(map[string]ToolDeclaration, len(current))
	for _, t := range current {
		cur[t.Name] = t
	}
	for _, t := range current {
		old, ok := prev[t.Name]
		if !ok || !DeclarationsEqual(old, t) {
			added = append(added, ToDeclaration(t))
		}
	}
	for _, t := range previous {
		now, ok := cur[t.Name]
		if !ok || !DeclarationsEqual(t, now) {
			removed = append(removed, ToolReference{Name: t.Name})
		}
	}
	return added, removed
}

// DeclarationsEqual reports whether two declarations present the same
// interface to the model. Parameters are compared after canonical JSON
// normalisation so whitespace/key-order differences do not read as changes.
func DeclarationsEqual(left, right ToolDeclaration) bool {
	return left.Name == right.Name &&
		left.Description == right.Description &&
		bytes.Equal(canonicalJSON(left.Parameters), canonicalJSON(right.Parameters))
}

// ToDeclaration copies a declaration with canonical parameters.
func ToDeclaration(t ToolDeclaration) ToolDeclaration {
	return ToolDeclaration{Name: t.Name, Description: t.Description, Parameters: canonicalJSON(t.Parameters)}
}

// CurrentSystemMessage replays every system message into one leading message
// holding the current prompt text and tool loadout (pi's
// getCurrentSystemMessage, minus app-specific sections). It returns nil when
// the transcript has no system message.
func CurrentSystemMessage(messages MessageList) *SystemMessage {
	var texts []string
	var timestamp int64
	seen := false
	for _, m := range messages {
		sys, ok := m.(SystemMessage)
		if !ok {
			continue
		}
		if !seen {
			timestamp = sys.Timestamp
			seen = true
		}
		if strings.TrimSpace(sys.Content) != "" {
			texts = append(texts, sys.Content)
		}
	}
	tools := CurrentTools(messages)
	if !seen && len(tools) == 0 {
		return nil
	}
	return &SystemMessage{
		RoleField:  RoleSystem,
		Content:    strings.Join(texts, "\n\n"),
		ToolsAdded: tools,
		Timestamp:  timestamp,
	}
}

// canonicalJSON re-encodes raw JSON with sorted object keys and no insignificant
// whitespace. Invalid or empty input is returned unchanged.
func canonicalJSON(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}
