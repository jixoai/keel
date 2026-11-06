// Package agent turns the durable event log into model turns: it rebuilds the
// conversation from committed events, runs the stateless loop, and appends the
// resulting messages back as events.
package agent

import (
	"encoding/json"
	"fmt"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/store"
)

// MessagesFromEvents rebuilds the model context from the event log. Only
// committed events are consulted; there is no process-local state.
func MessagesFromEvents(events []store.Event) (agentcore.MessageList, error) {
	msgs := make(agentcore.MessageList, 0, len(events))
	for _, ev := range events {
		switch ev.Kind {
		case store.EventUserMessage:
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return nil, fmt.Errorf("agent: decode user event %d: %w", ev.Seq, err)
			}
			msgs = append(msgs, agentcore.UserMessage{
				RoleField: agentcore.RoleUser,
				Content:   agentcore.ContentList{agentcore.NewTextContent(p.Text)},
			})
		case store.EventAssistantMessage:
			var m agentcore.AssistantMessage
			if err := json.Unmarshal(ev.Payload, &m); err != nil {
				return nil, fmt.Errorf("agent: decode assistant event %d: %w", ev.Seq, err)
			}
			if m.RoleField == "" {
				m.RoleField = agentcore.RoleAssistant
			}
			msgs = append(msgs, m)
		case store.EventToolResult:
			var m agentcore.ToolResultMessage
			if err := json.Unmarshal(ev.Payload, &m); err != nil {
				return nil, fmt.Errorf("agent: decode tool result event %d: %w", ev.Seq, err)
			}
			if m.RoleField == "" {
				m.RoleField = agentcore.RoleToolResult
			}
			msgs = append(msgs, m)
		case store.EventCompaction:
			var p struct {
				Summary      string `json:"summary"`
				TokensBefore int    `json:"tokensBefore"`
				Timestamp    int64  `json:"timestamp"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return nil, fmt.Errorf("agent: decode compaction event %d: %w", ev.Seq, err)
			}
			msgs = append(msgs, agentcore.CompactionMessage{
				RoleField:    agentcore.RoleCompaction,
				Summary:      p.Summary,
				TokensBefore: p.TokensBefore,
				Timestamp:    p.Timestamp,
			})
		default:
			// run.started / run.failed and any future bookkeeping events do not
			// participate in the model context.
		}
	}
	return msgs, nil
}

// ContentText concatenates the text blocks of a content list.
func ContentText(content agentcore.ContentList) string {
	out := ""
	for _, c := range content {
		if t, ok := c.(agentcore.TextContent); ok {
			out += t.Text
		}
	}
	return out
}
