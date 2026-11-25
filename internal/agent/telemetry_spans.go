package agent

import (
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/telemetry"
)

// runSpanTracker mirrors the loop's lifecycle events onto PI's span tree:
// keel.agent.run is the root (opened by observeRun), turns and tool calls are
// children, and compaction is a child of the run. message_* lifecycle events
// are span events on the active turn. It is a no-op when the telemetry adapter
// does not implement telemetry.ManualStarter (callback-only adapters still get
// the run span and provider span).
type runSpanTracker struct {
	root telemetry.Span

	turn    telemetry.Span
	endTurn func()

	tools map[string]toolSpan
	// compaction is the in-flight compaction span, if any.
	compaction    telemetry.Span
	endCompaction func()
}

type toolSpan struct {
	span telemetry.Span
	end  func()
}

func newRunSpanTracker(root telemetry.Span) *runSpanTracker {
	return &runSpanTracker{root: root, tools: map[string]toolSpan{}}
}

func (t *runSpanTracker) child(parent telemetry.Span, options telemetry.SpanOptions) (telemetry.Span, func()) {
	if parent == nil {
		return nil, func() {}
	}
	starter, ok := parent.(telemetry.ManualStarter)
	if !ok {
		return nil, func() {}
	}
	return starter.StartManualSpan(options)
}

func (t *runSpanTracker) observe(event agentcore.AgentEvent) {
	if t == nil || t.root == nil {
		return
	}
	switch e := event.(type) {
	case agentcore.TurnStartEvent:
		t.finishTurn()
		t.turn, t.endTurn = t.child(t.root, telemetry.SpanOptions{Name: "keel.agent.turn"})
	case agentcore.TurnEndEvent:
		if t.turn != nil {
			t.turn.AddEvent("turn_end", telemetry.SpanAttributes{"stopReason": string(e.Message.StopReason)})
		}
		t.finishTurn()
	case agentcore.MessageStartEvent:
		t.addMessageEvent("message_start", e.Message)
	case agentcore.MessageUpdateEvent:
		t.addMessageEvent("message_update", e.Message)
	case agentcore.MessageEndEvent:
		t.addMessageEvent("message_end", e.Message)
	case agentcore.ToolExecutionStartEvent:
		parent := t.turn
		if parent == nil {
			parent = t.root
		}
		span, end := t.child(parent, telemetry.SpanOptions{
			Name: "keel.agent.tool",
			Attributes: telemetry.SpanAttributes{
				"tool": e.ToolName,
			},
		})
		t.tools[e.ToolCallID] = toolSpan{span: span, end: end}
	case agentcore.ToolExecutionUpdateEvent:
		if tool, ok := t.tools[e.ToolCallID]; ok && tool.span != nil {
			tool.span.AddEvent("tool_update", nil)
		}
	case agentcore.ToolExecutionEndEvent:
		tool, ok := t.tools[e.ToolCallID]
		if !ok {
			return
		}
		if tool.span != nil {
			if e.IsError {
				tool.span.SetStatus(telemetry.ErrorStatus(nil))
			}
			tool.span.AddEvent("tool_end", telemetry.SpanAttributes{"error": e.IsError})
		}
		tool.end()
		delete(t.tools, e.ToolCallID)
	case agentcore.CompactionStartEvent:
		t.finishCompaction()
		t.compaction, t.endCompaction = t.child(t.root, telemetry.SpanOptions{
			Name: "keel.agent.compaction",
			Attributes: telemetry.SpanAttributes{
				"reason":       e.Reason,
				"tokensBefore": e.TokensBefore,
			},
		})
	case agentcore.CompactionEvent:
		if t.compaction != nil {
			if e.ErrorMessage != "" {
				t.compaction.SetStatus(telemetry.ErrorStatus(nil))
			}
			t.compaction.AddEvent("compaction_end", telemetry.SpanAttributes{
				"tokensAfter": e.TokensAfter,
				"error":       e.ErrorMessage != "",
			})
		}
		t.finishCompaction()
	case agentcore.AgentEndEvent:
		t.close()
	}
}

func (t *runSpanTracker) addMessageEvent(name string, message agentcore.AgentMessage) {
	span := t.turn
	if span == nil {
		span = t.root
	}
	if span == nil || message == nil {
		return
	}
	span.AddEvent(name, telemetry.SpanAttributes{"role": message.Role()})
}

func (t *runSpanTracker) finishTurn() {
	if t.endTurn != nil {
		t.endTurn()
	}
	t.turn, t.endTurn = nil, nil
}

func (t *runSpanTracker) finishCompaction() {
	if t.endCompaction != nil {
		t.endCompaction()
	}
	t.compaction, t.endCompaction = nil, nil
}

// close settles every live span once. AgentEnd normally calls it; the runner's
// defer covers cancellation and early returns.
func (t *runSpanTracker) close() {
	if t == nil {
		return
	}
	t.finishTurn()
	for id, tool := range t.tools {
		tool.end()
		delete(t.tools, id)
	}
	t.finishCompaction()
}
