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
//
// A payload that cannot be decoded is SKIPPED with a diagnostic (when diagnose
// is supplied) instead of failing the whole run: the kernel validates payload
// shapes at commit time, so a malformed payload here can only be legacy or
// foreign data, and refusing to run the session over one unreadable event would
// freeze the conversation. The skip is recorded, never silent.
func MessagesFromEvents(events []store.Event, diagnose func(seq int64, kind string, err error)) agentcore.MessageList {
	msgs, _ := DecodeMessages(events, diagnose)
	return msgs
}

// DecodeMessages is MessagesFromEvents with the committed sequence each message
// came from. The runner needs the mapping to present input a previous attempt
// never read last (see presentUnreadInputLast).
func DecodeMessages(events []store.Event, diagnose func(seq int64, kind string, err error)) (agentcore.MessageList, []int64) {
	skip := func(ev store.Event, err error) {
		if diagnose != nil {
			diagnose(ev.Seq, ev.Kind, err)
		}
	}
	msgs := make(agentcore.MessageList, 0, len(events))
	seqs := make([]int64, 0, len(events))
	appendMsg := func(seq int64, msg agentcore.Message) {
		msgs = append(msgs, msg)
		seqs = append(seqs, seq)
	}
	for _, ev := range events {
		switch ev.Kind {
		case store.EventUserMessage:
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				skip(ev, fmt.Errorf("decode user event %d: %w", ev.Seq, err))
				continue
			}
			appendMsg(ev.Seq, agentcore.UserMessage{
				RoleField: agentcore.RoleUser,
				Content:   agentcore.ContentList{agentcore.NewTextContent(p.Text)},
			})
		case store.EventAssistantMessage:
			var m agentcore.AssistantMessage
			if err := json.Unmarshal(ev.Payload, &m); err != nil {
				skip(ev, fmt.Errorf("decode assistant event %d: %w", ev.Seq, err))
				continue
			}
			if m.RoleField == "" {
				m.RoleField = agentcore.RoleAssistant
			}
			appendMsg(ev.Seq, m)
		case store.EventToolResult:
			var m agentcore.ToolResultMessage
			if err := json.Unmarshal(ev.Payload, &m); err != nil {
				skip(ev, fmt.Errorf("decode tool result event %d: %w", ev.Seq, err))
				continue
			}
			if m.RoleField == "" {
				m.RoleField = agentcore.RoleToolResult
			}
			appendMsg(ev.Seq, m)
		case store.EventCompaction:
			var p struct {
				Summary      string `json:"summary"`
				TokensBefore int    `json:"tokensBefore"`
				Timestamp    int64  `json:"timestamp"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				skip(ev, fmt.Errorf("decode compaction event %d: %w", ev.Seq, err))
				continue
			}
			appendMsg(ev.Seq, agentcore.CompactionMessage{
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
	return msgs, seqs
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
