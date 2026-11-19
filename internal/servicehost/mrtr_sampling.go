// MCP sampling against the session's model service (change
// feat-mcp-apps-events, P5.2).
//
// MRTR `sampling/createMessage` must run on the model the session is already
// configured for, not on a second provider path. This adapter reads the llm
// service of the current generation — the same ModelService the runner uses —
// resolves the tenant's default selection (the session inherits it when its run
// names no model) and performs one non-streaming completion through that
// model's StreamFn.
//
// Bounds and accounting are the executor's job (maxTokens cap, per-call budget,
// audit). This file only maps the wire messages onto the provider request and
// reports objective usage facts (model id, stop reason); it never computes or
// writes a price.

package servicehost

import (
	"context"
	"fmt"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/mcphost"
	"github.com/jixoai/keel/internal/provider"
)

// MCPSampler returns the sampling executor over this generation's model
// service, or nil when the runtime has no provider configuration. A nil result
// keeps sampling refused (fail closed) instead of inventing a model.
func (r *Runtime) MCPSampler() mcphost.Sampler {
	if r == nil {
		return nil
	}
	llm := r.llm()
	if llm == nil {
		return nil
	}
	return &runtimeSampler{
		models:       llm.Models(),
		fallback:     llm.StreamFn(),
		fallbackID:   llm.Model(),
		fallbackKey:  llm.APIKey(),
		fallbackName: llm.ProviderName(),
	}
}

// runtimeSampler resolves the attempt model per request. The model service is
// immutable per generation, so a sampling request cannot observe a half-applied
// settings change.
type runtimeSampler struct {
	models       ModelService
	fallback     provider.StreamFn
	fallbackID   string
	fallbackKey  string
	fallbackName string
}

func (s *runtimeSampler) Sample(ctx context.Context, request mcphost.SamplingRequest) (mcphost.SamplingResult, error) {
	stream := s.fallback
	model := s.fallbackID
	apiKey := s.fallbackKey
	level := agentcore.ThinkingMinimal
	if s.models != nil {
		active, err := s.models.Resolve(ctx, kernel.Scope{}, nil)
		if err != nil {
			return mcphost.SamplingResult{}, fmt.Errorf("sampling: resolve the session model: %w", err)
		}
		stream = active.Stream
		model = active.Model
		apiKey = active.APIKey
		if active.ThinkingLevel != "" {
			level = active.ThinkingLevel
		}
	}
	if stream == nil || strings.TrimSpace(model) == "" {
		return mcphost.SamplingResult{}, fmt.Errorf("sampling: no executable model is configured")
	}
	messages := make(agentcore.MessageList, 0, len(request.Messages))
	for _, message := range request.Messages {
		content := agentcore.ContentList{agentcore.NewTextContent(message.Text)}
		switch strings.ToLower(message.Role) {
		case "assistant":
			messages = append(messages, agentcore.AssistantMessage{
				RoleField: agentcore.RoleAssistant,
				Content:   content,
			})
		default:
			messages = append(messages, agentcore.UserMessage{
				RoleField: agentcore.RoleUser,
				Content:   content,
			})
		}
	}
	streamed, err := stream(ctx, model, provider.LlmContext{
		SystemPrompt: request.SystemPrompt,
		Messages:     messages,
	}, provider.StreamConfig{
		APIKey:        apiKey,
		ThinkingLevel: level,
	})
	if err != nil {
		return mcphost.SamplingResult{}, fmt.Errorf("sampling: %w", err)
	}
	answer, err := streamed.Result(ctx)
	if err != nil {
		return mcphost.SamplingResult{}, fmt.Errorf("sampling: %w", err)
	}
	if answer.StopReason == agentcore.StopReasonError || answer.StopReason == agentcore.StopReasonAborted {
		message := strings.TrimSpace(answer.ErrorMessage)
		if message == "" {
			message = "the model returned " + answer.StopReason
		}
		return mcphost.SamplingResult{}, fmt.Errorf("sampling: %s", message)
	}
	text := agentcore.ContentToText(answer.Content)
	if strings.TrimSpace(text) == "" {
		return mcphost.SamplingResult{}, fmt.Errorf("sampling: the model returned no text")
	}
	result := mcphost.SamplingResult{
		Role:       "assistant",
		Text:       text,
		Model:      answer.Model,
		StopReason: samplingStopReason(answer.StopReason),
	}
	if result.Model == "" {
		result.Model = model
	}
	return result, nil
}

// samplingStopReason maps the runtime's stop reasons onto the MCP vocabulary.
func samplingStopReason(reason string) string {
	switch reason {
	case agentcore.StopReasonLength:
		return "maxTokens"
	case agentcore.StopReasonEndTurn, "":
		return "endTurn"
	default:
		return "endTurn"
	}
}

// Compile-time assertion: the runtime sampler is the executor's sampler.
var _ mcphost.Sampler = (*runtimeSampler)(nil)
