package agent

import (
	"context"
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/hooks"
)

// hookBeforeToolCall composes the PI tool_call gate around the existing
// authorization/guardrail callback. Authorization runs first so a hook can
// never see or run a tool the principal was not granted.
func (r *Runner) hookBeforeToolCall(next agentcore.BeforeToolCallFunc) agentcore.BeforeToolCallFunc {
	registry := r.Hooks
	if registry == nil {
		return next
	}
	return func(ctx context.Context, call agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
		if next != nil {
			if decision := next(ctx, call); decision != nil {
				return decision
			}
		}
		event := &hooks.ToolCallEvent{
			ToolCallID: call.ID,
			ToolName:   call.Name,
			Input:      append(json.RawMessage(nil), call.Arguments...),
		}
		decision := registry.EmitToolCall(ctx, event)
		if decision.Action.Normalized() == hooks.ActionDeny {
			reason := decision.Reason
			if reason == "" {
				reason = "blocked by hook"
			}
			content := agentcore.ContentList{agentcore.NewTextContent(reason)}
			block := &agentcore.BeforeToolCallDecision{
				Block:     true,
				Content:   &content,
				Terminate: decision.Terminate,
			}
			if decision.Result != nil {
				if decision.Result.Content != nil {
					block.Content = decision.Result.Content
				}
				if decision.Result.DetailsSet {
					details := decision.Result.Details
					block.Details = &details
				}
				if decision.Result.Terminate != nil {
					block.Terminate = *decision.Result.Terminate
				}
			}
			return block
		}
		if len(decision.Input) > 0 {
			return &agentcore.BeforeToolCallDecision{UpdatedInput: decision.Input}
		}
		return nil
	}
}

// hookAfterToolCall applies tool_result patches (redaction, result rewriting).
func (r *Runner) hookAfterToolCall() agentcore.AfterToolCallFunc {
	registry := r.Hooks
	if registry == nil {
		return nil
	}
	return func(ctx context.Context, call agentcore.AgentToolCall, result agentcore.AgentToolResult, isError bool) *agentcore.AfterToolCallResult {
		event := &hooks.ToolResultEvent{
			ToolCallID: call.ID,
			ToolName:   call.Name,
			Input:      append(json.RawMessage(nil), call.Arguments...),
			Content:    result.Content,
			Details:    result.Details,
			DetailsSet: result.Details != nil,
			IsError:    isError,
			Terminate:  result.Terminate,
		}
		patched, modified := registry.EmitToolResult(ctx, event)
		if !modified {
			return nil
		}
		content := patched.Content
		details := patched.Details
		terminate := patched.Terminate
		patchedError := patched.IsError
		return &agentcore.AfterToolCallResult{
			Content:   &content,
			Details:   &details,
			Terminate: terminate,
			IsError:   &patchedError,
		}
	}
}

// applyBeforeAgentStart chains the system prompt and reports guidance. Guidance
// injection needs a custom-message ledger kind Keel does not have yet, so the
// current wiring logs and ignores it instead of silently writing a user message.
func (r *Runner) applyBeforeAgentStart(ctx context.Context, systemPrompt, prompt string) string {
	if r.Hooks == nil {
		return systemPrompt
	}
	result := r.Hooks.EmitBeforeAgentStart(ctx, &hooks.BeforeAgentStartEvent{
		MessageType:  hooks.MessageTypeUser,
		Prompt:       prompt,
		SystemPrompt: systemPrompt,
	})
	if len(result.Guidance) > 0 {
		r.Logger.Warn("hook guidance ignored: no custom-message event kind", "count", len(result.Guidance))
	}
	return result.SystemPrompt
}

// emitHookNotification forwards loop lifecycle events to notification hooks.
// Actionable boundaries (turn_end, agent_before_settle) are not emitted here;
// they remain host-level decisions and are not wired in this slice.
func (r *Runner) emitHookNotification(ctx context.Context, event agentcore.AgentEvent) {
	if r.Hooks == nil {
		return
	}
	switch e := event.(type) {
	case agentcore.AgentStartEvent:
		r.Hooks.Emit(ctx, &hooks.AgentStartEvent{})
	case agentcore.AgentEndEvent:
		r.Hooks.Emit(ctx, &hooks.AgentEndEvent{Messages: e.Messages})
	case agentcore.TurnStartEvent:
		r.Hooks.Emit(ctx, &hooks.TurnStartEvent{})
	case agentcore.MessageStartEvent:
		r.Hooks.Emit(ctx, &hooks.MessageStartEvent{Message: e.Message})
	case agentcore.MessageUpdateEvent:
		r.Hooks.Emit(ctx, &hooks.MessageUpdateEvent{Message: e.Message, AssistantMessageEvent: e.AssistantMessageEvent})
	case agentcore.MessageEndEvent:
		r.Hooks.Emit(ctx, &hooks.MessageEndEvent{Message: e.Message})
	case agentcore.ToolExecutionStartEvent:
		r.Hooks.Emit(ctx, &hooks.ToolExecutionStartEvent{
			ToolCallID: e.ToolCallID, ToolName: e.ToolName, Args: hookJSON(e.Args),
		})
	case agentcore.ToolExecutionUpdateEvent:
		r.Hooks.Emit(ctx, &hooks.ToolExecutionUpdateEvent{
			ToolCallID: e.ToolCallID, ToolName: e.ToolName, PartialResult: e.PartialResult,
		})
	case agentcore.ToolExecutionEndEvent:
		r.Hooks.Emit(ctx, &hooks.ToolExecutionEndEvent{
			ToolCallID: e.ToolCallID, ToolName: e.ToolName, Result: e.Result,
			IsError: e.IsError, DurationMs: e.DurationMs,
		})
	}
}

func hookJSON(value any) json.RawMessage {
	switch v := value.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return append(json.RawMessage(nil), v...)
	case []byte:
		return append(json.RawMessage(nil), v...)
	case string:
		return json.RawMessage(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return raw
	}
}

func trailingUserText(msgs agentcore.MessageList) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if user, ok := msgs[i].(agentcore.UserMessage); ok {
			return ContentText(user.Content)
		}
	}
	return ""
}
