package agentext

import (
	"context"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/provider"
)

// StreamTee is the partial producer: it wraps the provider stream of a run and
// republishes its text/thinking deltas on the in-process hub while forwarding
// every event to the loop unchanged.
//
// Invariants:
//   - transport-only: nothing is persisted, and the loop sees the exact event
//     sequence (and the exact terminal result) it would have seen without the
//     tee;
//   - scope-bound: the run context carries the RunScope (RunControl.BeginRun
//     installs it), so a delta is published to the right session or not at all;
//   - non-blocking producers: a slow subscriber drops frames (PartialHub), so
//     model latency never depends on a UI consumer;
//   - cancellation is forwarding-honest: a cancelled request ends both streams.
func StreamTee(base provider.StreamFn, hub *PartialHub) provider.StreamFn {
	if base == nil || hub == nil {
		return base
	}
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		scope, hasScope := RunScopeFromContext(ctx)
		if !hasScope {
			// A run that did not register with RunControl has no session to
			// attribute frames to; the request is served untouched.
			return base(ctx, model, llm, cfg)
		}
		upstream, err := base(ctx, model, llm, cfg)
		if err != nil {
			return nil, err
		}
		out := provider.NewAssistantMessageEventStream(16)
		go func() {
			defer out.Close()
			last := map[string]string{}
			for event := range upstream.Events() {
				switch typed := event.(type) {
				case provider.StreamTextEvent:
					publishDelta(hub, scope, "assistant.delta", last, messageText(typed.Partial))
				case provider.StreamThinkingEvent:
					publishDelta(hub, scope, "thinking.delta", last, messageThinking(typed.Partial))
				}
				if err := out.Emit(ctx, event); err != nil {
					return
				}
			}
		}()
		return out, nil
	}
}

// publishDelta computes the suffix a partial message added since the previous
// partial of the same kind. Providers emit cumulative partials, not deltas, so
// the difference is what a token-level consumer wants.
func publishDelta(hub *PartialHub, scope RunScope, kind string, last map[string]string, full string) {
	previous := last[kind]
	last[kind] = full
	var delta string
	switch {
	case full == previous:
		return
	case strings.HasPrefix(full, previous):
		delta = full[len(previous):]
	default:
		// A provider that rewrites the partial (not cumulative) still gets a
		// frame; the consumer sees the whole current text.
		delta = full
	}
	if delta == "" {
		return
	}
	hub.Publish(scope.TenantID, scope.SessionID, Partial{Type: kind, Delta: delta})
}

func messageText(message agentcore.AssistantMessage) string {
	var builder strings.Builder
	for _, content := range message.Content {
		if block, ok := content.(agentcore.TextContent); ok {
			builder.WriteString(block.Text)
		}
	}
	return builder.String()
}

func messageThinking(message agentcore.AssistantMessage) string {
	var builder strings.Builder
	for _, content := range message.Content {
		if block, ok := content.(agentcore.ThinkingContent); ok {
			builder.WriteString(block.Thinking)
		}
	}
	return builder.String()
}
