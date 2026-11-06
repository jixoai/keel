package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/jixoai/keel/internal/agentcore"
)

// Emit dispatches a notification event. Return values are ignored (PI:
// notification handlers are observation-only); errors are reported and never
// change the run. Observer registrations are still invoked in order.
func (r *Registry) Emit(ctx context.Context, event Event) {
	for _, reg := range r.snapshot(event.EventType()) {
		if reg.Async {
			go r.emitAsync(ctx, reg, cloneEvent(event))
			continue
		}
		inv := r.invoke(ctx, reg, event)
		if inv.err != nil {
			r.handleFailure(reg, event, inv.err)
		}
		r.record(reg, event, inv, ActionAllow, inv.decision.Reason)
	}
}

func (r *Registry) emitAsync(ctx context.Context, reg Registration, event Event) {
	inv := r.invoke(ctx, reg, event)
	if inv.err != nil {
		r.handleFailure(reg, event, inv.err)
	}
	r.record(reg, event, inv, ActionAllow, inv.decision.Reason)
}

// cloneEvent gives async notification handlers their own payload so a
// concurrent synchronous handler cannot race with them. Unknown event types are
// passed through (async is only allowed on the notification set).
func cloneEvent(event Event) Event {
	switch e := event.(type) {
	case *AgentStartEvent:
		copy := *e
		return &copy
	case *AgentEndEvent:
		copy := *e
		copy.Messages = cloneMessages(e.Messages)
		return &copy
	case *AgentSettledEvent:
		copy := *e
		return &copy
	case *TurnStartEvent:
		copy := *e
		return &copy
	case *MessageStartEvent:
		copy := *e
		copy.Message = cloneMessage(e.Message)
		return &copy
	case *MessageUpdateEvent:
		copy := *e
		copy.Message = cloneMessage(e.Message)
		return &copy
	case *ToolExecutionStartEvent:
		copy := *e
		copy.Args = append(json.RawMessage(nil), e.Args...)
		return &copy
	case *ToolExecutionUpdateEvent:
		copy := *e
		copy.Args = append(json.RawMessage(nil), e.Args...)
		copy.PartialResult = cloneToolResult(e.PartialResult)
		return &copy
	case *ToolExecutionEndEvent:
		copy := *e
		copy.Result = cloneToolResult(e.Result)
		if e.DurationMs != nil {
			duration := *e.DurationMs
			copy.DurationMs = &duration
		}
		return &copy
	case *SessionCompactEvent:
		copy := *e
		copy.CompactionEntry = append(json.RawMessage(nil), e.CompactionEntry...)
		return &copy
	case *SessionCompactFailedEvent:
		copy := *e
		return &copy
	default:
		return event
	}
}

func cloneToolResult(result agentcore.AgentToolResult) agentcore.AgentToolResult {
	result.Content = cloneContentList(result.Content)
	if result.Terminate != nil {
		terminate := *result.Terminate
		result.Terminate = &terminate
	}
	return result
}

// EmitBeforeAgentStart runs before_agent_start handlers in registration order.
// SystemPrompt chains across handlers: each handler sees the value produced by
// earlier handlers, and a handler may replace it for the run (by mutating the
// event or returning Decision.SystemPrompt). Guidance messages are collected in
// handler order and the host injects them into the run.
func (r *Registry) EmitBeforeAgentStart(ctx context.Context, event *BeforeAgentStartEvent) BeforeAgentStartResult {
	result := BeforeAgentStartResult{SystemPrompt: event.SystemPrompt}
	for _, reg := range r.snapshot(EventBeforeAgentStart) {
		current := *event
		current.SystemPrompt = event.SystemPrompt
		inv := r.invoke(ctx, reg, &current)
		if inv.err != nil {
			r.handleFailure(reg, &current, inv.err)
			r.record(reg, &current, inv, ActionAllow, "")
			continue
		}
		if reg.Observer {
			r.record(reg, &current, inv, ActionAllow, inv.decision.Reason)
			continue
		}

		action := inv.decision.Action.Normalized()
		// A handler may mutate event.SystemPrompt in place; that is equivalent
		// to returning the new value.
		changed := false
		if current.SystemPrompt != event.SystemPrompt {
			event.SystemPrompt = current.SystemPrompt
			result.SystemPrompt = current.SystemPrompt
			changed = true
		}
		if inv.decision.SystemPrompt != nil {
			event.SystemPrompt = *inv.decision.SystemPrompt
			result.SystemPrompt = event.SystemPrompt
			changed = true
		}
		if len(inv.decision.Guidance) > 0 {
			result.Guidance = append(result.Guidance, inv.decision.Guidance...)
			changed = true
		}
		if changed && action == ActionAllow {
			action = ActionModify
		}
		result.SystemPrompt = event.SystemPrompt
		r.record(reg, &current, inv, action, inv.decision.Reason)
	}
	return result
}

// EmitContext runs context handlers before each LLM call. Messages is a deep
// copy of the host conversation (without system prompt); replacements chain
// across handlers. Handlers may return Decision.Messages or mutate
// event.Messages in place.
func (r *Registry) EmitContext(ctx context.Context, messages []agentcore.Message) []agentcore.Message {
	current := cloneMessages(messages)
	for _, reg := range r.snapshot(EventContext) {
		before := cloneMessages(current)
		event := &ContextEvent{Messages: current}
		inv := r.invoke(ctx, reg, event)
		if inv.err != nil {
			r.handleFailure(reg, event, inv.err)
			r.record(reg, event, inv, ActionAllow, "")
			continue
		}
		if reg.Observer {
			r.record(reg, event, inv, ActionAllow, inv.decision.Reason)
			continue
		}

		action := inv.decision.Action.Normalized()
		if action != ActionDeny {
			if inv.decision.Messages != nil {
				current = inv.decision.Messages
				action = ActionModify
			} else if event.Messages != nil && !reflect.DeepEqual(event.Messages, before) {
				current = event.Messages
				action = ActionModify
			}
		}
		r.record(reg, event, inv, action, inv.decision.Reason)
	}
	return current
}

// EmitToolCall runs tool_call handlers. The first deny wins and stops dispatch;
// a modify decision replaces Input for later handlers and for execution.
// Handler errors fail closed by default (PI: tool_call errors block the tool).
func (r *Registry) EmitToolCall(ctx context.Context, event *ToolCallEvent) Decision {
	result := Decision{Action: ActionAllow}
	for _, reg := range r.snapshot(EventToolCall) {
		before := append([]byte(nil), event.Input...)
		inv := r.invoke(ctx, reg, event)
		if inv.err != nil {
			policy := r.handleFailure(reg, event, inv.err)
			if reg.Observer || policy != ErrorPolicyFailClosed {
				r.record(reg, event, inv, ActionAllow, "")
				continue
			}
			deny := Deny(fmt.Sprintf("hook %q failed: %v", reg.Name, inv.err))
			r.record(reg, event, inv, ActionDeny, deny.Reason)
			return deny
		}
		if reg.Observer {
			r.record(reg, event, inv, ActionAllow, inv.decision.Reason)
			continue
		}

		action := inv.decision.Action.Normalized()
		if action == ActionDeny {
			if inv.decision.Reason == "" {
				inv.decision.Reason = fmt.Sprintf("denied by hook %q", reg.Name)
			}
			r.record(reg, event, inv, ActionDeny, inv.decision.Reason)
			return inv.decision
		}
		if len(inv.decision.Input) > 0 {
			event.Input = inv.decision.Input
			action = ActionModify
		} else if !bytes.Equal(before, event.Input) {
			inv.decision.Input = event.Input
			action = ActionModify
		}
		if action == ActionModify {
			inv.decision.Action = ActionModify
			result = inv.decision
		}
		r.record(reg, event, inv, action, inv.decision.Reason)
	}
	return result
}

// EmitToolResult runs tool_result handlers and chains partial patches. It
// returns the effective result event and whether any handler modified it.
func (r *Registry) EmitToolResult(ctx context.Context, event *ToolResultEvent) (ToolResultEvent, bool) {
	current := *event
	modified := false
	for _, reg := range r.snapshot(EventToolResult) {
		handlerEvent := current
		inv := r.invoke(ctx, reg, &handlerEvent)
		if inv.err != nil {
			r.handleFailure(reg, &handlerEvent, inv.err)
			r.record(reg, &handlerEvent, inv, ActionAllow, "")
			continue
		}
		if reg.Observer {
			r.record(reg, &handlerEvent, inv, ActionAllow, inv.decision.Reason)
			continue
		}
		action := inv.decision.Action.Normalized()
		if action == ActionDeny {
			// tool_result has no deny semantic in PI; a deny decision is
			// recorded but cannot un-execute the tool.
			r.record(reg, &handlerEvent, inv, ActionDeny, inv.decision.Reason)
			continue
		}
		if inv.decision.Result != nil {
			patch := inv.decision.Result
			if patch.Content != nil {
				current.Content = *patch.Content
				modified = true
			}
			if patch.DetailsSet {
				current.Details = patch.Details
				modified = true
			}
			if patch.IsError != nil {
				current.IsError = *patch.IsError
				modified = true
			}
			if patch.Terminate != nil {
				current.Terminate = patch.Terminate
				modified = true
			}
			action = ActionModify
		}
		r.record(reg, &handlerEvent, inv, action, inv.decision.Reason)
	}
	return current, modified
}

// EmitMessageEnd runs message_end handlers. A replacement must keep the same
// role; a mismatched replacement is reported and skipped. It returns the final
// message and whether it was replaced.
func (r *Registry) EmitMessageEnd(ctx context.Context, event MessageEndEvent) (agentcore.Message, bool) {
	current := event.Message
	modified := false
	for _, reg := range r.snapshot(EventMessageEnd) {
		handlerEvent := MessageEndEvent{Message: current}
		inv := r.invoke(ctx, reg, &handlerEvent)
		if inv.err != nil {
			r.handleFailure(reg, &handlerEvent, inv.err)
			r.record(reg, &handlerEvent, inv, ActionAllow, "")
			continue
		}
		if reg.Observer {
			r.record(reg, &handlerEvent, inv, ActionAllow, inv.decision.Reason)
			continue
		}
		action := inv.decision.Action.Normalized()
		if action != ActionDeny && inv.decision.Message != nil {
			if current == nil || inv.decision.Message.Role() != current.Role() {
				err := fmt.Errorf("message_end handlers must return a message with the same role")
				r.reportError(HookError{Hook: reg.Name, Event: EventMessageEnd, Err: err})
				r.record(reg, &handlerEvent, inv, ActionAllow, err.Error())
				continue
			}
			current = inv.decision.Message
			modified = true
			action = ActionModify
		}
		r.record(reg, &handlerEvent, inv, action, inv.decision.Reason)
	}
	return current, modified
}

// EmitSessionBeforeCompact runs session_before_compact handlers. The first
// cancel stops compaction and dispatch; the last custom compaction wins.
func (r *Registry) EmitSessionBeforeCompact(ctx context.Context, event *SessionBeforeCompactEvent) SessionBeforeCompactResult {
	var result SessionBeforeCompactResult
	for _, reg := range r.snapshot(EventSessionBeforeCompact) {
		inv := r.invoke(ctx, reg, event)
		if inv.err != nil {
			policy := r.handleFailure(reg, event, inv.err)
			if !reg.Observer && policy == ErrorPolicyFailClosed {
				result = SessionBeforeCompactResult{Cancel: true}
				r.record(reg, event, inv, ActionDeny, inv.err.Error())
				return result
			}
			r.record(reg, event, inv, ActionAllow, "")
			continue
		}
		if reg.Observer {
			r.record(reg, event, inv, ActionAllow, inv.decision.Reason)
			continue
		}
		action := inv.decision.Action.Normalized()
		if action == ActionDeny {
			result = SessionBeforeCompactResult{Cancel: true}
			r.record(reg, event, inv, ActionDeny, inv.decision.Reason)
			return result
		}
		if inv.decision.Compaction != nil {
			result.Compaction = inv.decision.Compaction
			action = ActionModify
		}
		r.record(reg, event, inv, action, inv.decision.Reason)
	}
	return result
}

// cloneMessages deep-copies the message list so a handler cannot mutate the
// host conversation by accident. Content blocks and details are copied one
// level deep. Custom Message implementations are kept as-is (they are expected
// to be immutable values).
func cloneMessages(messages []agentcore.Message) []agentcore.Message {
	out := make([]agentcore.Message, len(messages))
	for i, message := range messages {
		out[i] = cloneMessage(message)
	}
	return out
}

func cloneMessage(message agentcore.Message) agentcore.Message {
	switch m := message.(type) {
	case agentcore.UserMessage:
		m.Content = cloneContentList(m.Content)
		return m
	case agentcore.AssistantMessage:
		m.Content = cloneContentList(m.Content)
		if m.Usage != nil {
			usage := *m.Usage
			m.Usage = &usage
		}
		return m
	case agentcore.ToolResultMessage:
		m.Content = cloneContentList(m.Content)
		if m.DurationMs != nil {
			duration := *m.DurationMs
			m.DurationMs = &duration
		}
		return m
	case agentcore.CompactionMessage:
		if m.Details != nil {
			m.Details = append(json.RawMessage(nil), m.Details...)
		}
		return m
	default:
		return message
	}
}

func cloneContentList(list agentcore.ContentList) agentcore.ContentList {
	if list == nil {
		return nil
	}
	out := make(agentcore.ContentList, len(list))
	for i, content := range list {
		switch c := content.(type) {
		case agentcore.ToolCallContent:
			if c.Arguments != nil {
				c.Arguments = append(json.RawMessage(nil), c.Arguments...)
			}
			out[i] = c
		case agentcore.ImageContent:
			out[i] = c
		default:
			out[i] = content
		}
	}
	return out
}
