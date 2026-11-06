// Behaviour ported from pi/packages/agent/src/proxy.ts @ 55a5c691
// (MIT License); implemented independently in Go against Keel's coarser
// provider event contract.

package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/provider"
)

// ProxyEvent is one server-sent event from a PI-compatible proxy. The proxy
// strips the partial message from deltas to save bandwidth; the client
// reconstructs it here.
type ProxyEvent struct {
	Type             string                     `json:"type"`
	ContentIndex     int                        `json:"contentIndex"`
	Delta            string                     `json:"delta"`
	ContentSignature string                     `json:"contentSignature"`
	ID               string                     `json:"id"`
	ToolName         string                     `json:"toolName"`
	ToolCall         *agentcore.ToolCallContent `json:"toolCall"`
	Reason           string                     `json:"reason"`
	ErrorMessage     string                     `json:"errorMessage"`
	Usage            *proxyUsage                `json:"usage"`
}

type proxyUsage struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	CacheRead   int `json:"cacheRead"`
	CacheWrite  int `json:"cacheWrite"`
	TotalTokens int `json:"totalTokens"`
}

// ProxyOptions configures the proxy stream function.
type ProxyOptions struct {
	ProxyURL  string
	AuthToken string
	Client    *http.Client
}

// NewProxyStream returns a StreamFn that routes model calls through a
// PI-compatible proxy (POST {proxyUrl}/api/stream). The proxy owns provider
// credentials; the client sends the model, context, and request options.
func NewProxyStream(opts ProxyOptions) provider.StreamFn {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 0}
	}
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		stream := provider.NewAssistantMessageEventStream(16)
		go runProxyStream(ctx, client, opts, model, llm, cfg, stream)
		return stream, nil
	}
}

type proxyTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type proxyContext struct {
	SystemPrompt string                `json:"systemPrompt"`
	Messages     agentcore.MessageList `json:"messages"`
	Tools        []proxyTool           `json:"tools,omitempty"`
}

type proxyRequest struct {
	Model   string         `json:"model"`
	Context proxyContext   `json:"context"`
	Options map[string]any `json:"options"`
}

func buildProxyContext(llm provider.LlmContext) proxyContext {
	tools := make([]proxyTool, 0, len(llm.Tools))
	for _, tool := range llm.Tools {
		tools = append(tools, proxyTool{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters:  tool.Schema(),
		})
	}
	return proxyContext{SystemPrompt: llm.SystemPrompt, Messages: llm.Messages, Tools: tools}
}

func runProxyStream(ctx context.Context, client *http.Client, opts ProxyOptions, model string,
	llm provider.LlmContext, cfg provider.StreamConfig, stream *provider.AssistantMessageEventStream) {
	defer stream.Close()

	partial := agentcore.AssistantMessage{
		RoleField: agentcore.RoleAssistant,
		Model:     model,
	}
	emit := func(ev provider.AssistantMessageEvent) bool {
		return stream.Emit(ctx, ev) == nil
	}

	body, err := json.Marshal(proxyRequest{
		Model:   model,
		Context: buildProxyContext(llm),
		Options: map[string]any{
			"temperature": cfg.Extra["temperature"],
			"maxTokens":   cfg.Extra["maxTokens"],
		},
	})
	if err != nil {
		failProxy(ctx, &partial, stream, "error", err.Error())
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(opts.ProxyURL, "/")+"/api/stream", bytes.NewReader(body))
	if err != nil {
		failProxy(ctx, &partial, stream, "error", err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+opts.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		reason := "error"
		if ctx.Err() != nil {
			reason = "aborted"
		}
		failProxy(ctx, &partial, stream, reason, err.Error())
		return
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		msg := fmt.Sprintf("Proxy error: %d %s", res.StatusCode, http.StatusText(res.StatusCode))
		var errorBody struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(res.Body).Decode(&errorBody) == nil && errorBody.Error != "" {
			msg = "Proxy error: " + errorBody.Error
		}
		failProxy(ctx, &partial, stream, "error", msg)
		return
	}

	toolArgs := map[int]*strings.Builder{}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	sawTerminal := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if raw == "" {
			continue
		}
		var ev ProxyEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			continue
		}
		providerEvent, terminal := processProxyEvent(&ev, &partial, toolArgs)
		if terminal {
			sawTerminal = true
		}
		if providerEvent != nil && !emit(providerEvent) {
			return
		}
		if sawTerminal {
			break
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		failProxy(ctx, &partial, stream, "error", err.Error())
		return
	}
	if !sawTerminal {
		reason := "error"
		if ctx.Err() != nil {
			reason = "aborted"
		}
		failProxy(ctx, &partial, stream, reason,
			"Connection closed by proxy server before the response completed")
	}
}

// processProxyEvent updates the partial message and returns the provider-level
// event to emit plus whether the proxy event was terminal.
func processProxyEvent(ev *ProxyEvent, partial *agentcore.AssistantMessage, toolArgs map[int]*strings.Builder) (provider.AssistantMessageEvent, bool) {
	switch ev.Type {
	case "start":
		return provider.StreamStartEvent{Partial: *partial}, false
	case "text_start":
		setContent(partial, ev.ContentIndex, agentcore.NewTextContent(""))
		return provider.StreamTextEvent{Partial: *partial}, false
	case "text_delta":
		if text, ok := contentAt(partial, ev.ContentIndex).(agentcore.TextContent); ok {
			text.Text += ev.Delta
			setContent(partial, ev.ContentIndex, text)
		}
		return provider.StreamTextEvent{Partial: *partial}, false
	case "text_end":
		if text, ok := contentAt(partial, ev.ContentIndex).(agentcore.TextContent); ok {
			text.TextSignature = ev.ContentSignature
			setContent(partial, ev.ContentIndex, text)
		}
		return provider.StreamTextEvent{Partial: *partial}, false
	case "thinking_start":
		setContent(partial, ev.ContentIndex, agentcore.NewThinkingContent(""))
		return provider.StreamThinkingEvent{Partial: *partial}, false
	case "thinking_delta":
		if thinking, ok := contentAt(partial, ev.ContentIndex).(agentcore.ThinkingContent); ok {
			thinking.Thinking += ev.Delta
			setContent(partial, ev.ContentIndex, thinking)
		}
		return provider.StreamThinkingEvent{Partial: *partial}, false
	case "thinking_end":
		if thinking, ok := contentAt(partial, ev.ContentIndex).(agentcore.ThinkingContent); ok {
			thinking.ThinkingSignature = ev.ContentSignature
			setContent(partial, ev.ContentIndex, thinking)
		}
		return provider.StreamThinkingEvent{Partial: *partial}, false
	case "toolcall_start":
		builder := &strings.Builder{}
		toolArgs[ev.ContentIndex] = builder
		setContent(partial, ev.ContentIndex, agentcore.NewToolCallContent(ev.ID, ev.ToolName, nil))
		return provider.StreamToolCallEvent{Partial: *partial}, false
	case "toolcall_delta":
		if builder, ok := toolArgs[ev.ContentIndex]; ok {
			builder.WriteString(ev.Delta)
		}
		call, _ := contentAt(partial, ev.ContentIndex).(agentcore.ToolCallContent)
		call.Arguments = json.RawMessage(builderString(toolArgs[ev.ContentIndex]))
		setContent(partial, ev.ContentIndex, call)
		return provider.StreamToolCallEvent{Partial: *partial}, false
	case "toolcall_end":
		call := agentcore.ToolCallContent{}
		if ev.ToolCall != nil {
			call = *ev.ToolCall
		} else {
			call, _ = contentAt(partial, ev.ContentIndex).(agentcore.ToolCallContent)
		}
		setContent(partial, ev.ContentIndex, call)
		return provider.StreamToolCallEvent{Partial: *partial}, false
	case "done":
		partial.StopReason = mapProxyStop(ev.Reason)
		partial.Usage = usageOf(ev.Usage)
		return provider.StreamDoneEvent{Message: *partial}, true
	case "error":
		partial.StopReason = agentcore.StopReasonError
		if ev.Reason == "aborted" {
			partial.StopReason = agentcore.StopReasonAborted
		}
		partial.ErrorMessage = ev.ErrorMessage
		partial.Usage = usageOf(ev.Usage)
		return provider.StreamErrorEvent{Message: *partial, Err: fmt.Errorf("%s", ev.ErrorMessage)}, true
	default:
		return nil, false
	}
}

func builderString(builder *strings.Builder) string {
	if builder == nil {
		return ""
	}
	return builder.String()
}

func setContent(msg *agentcore.AssistantMessage, index int, content agentcore.Content) {
	for len(msg.Content) <= index {
		msg.Content = append(msg.Content, agentcore.NewTextContent(""))
	}
	msg.Content[index] = content
}

func contentAt(msg *agentcore.AssistantMessage, index int) agentcore.Content {
	if index < 0 || index >= len(msg.Content) {
		return nil
	}
	return msg.Content[index]
}

func mapProxyStop(reason string) string {
	switch reason {
	case "length":
		return agentcore.StopReasonLength
	case "toolUse":
		return agentcore.StopReasonToolUse
	case "aborted":
		return agentcore.StopReasonAborted
	case "error":
		return agentcore.StopReasonError
	default:
		return agentcore.StopReasonEndTurn
	}
}

func usageOf(usage *proxyUsage) *agentcore.Usage {
	if usage == nil {
		return nil
	}
	return &agentcore.Usage{InputTokens: usage.Input, OutputTokens: usage.Output}
}

func failProxy(ctx context.Context, partial *agentcore.AssistantMessage, stream *provider.AssistantMessageEventStream, reason, message string) {
	if reason == "aborted" {
		partial.StopReason = agentcore.StopReasonAborted
	} else {
		partial.StopReason = agentcore.StopReasonError
	}
	partial.ErrorMessage = message
	_ = stream.Emit(ctx, provider.StreamErrorEvent{Message: *partial, Err: fmt.Errorf("%s", message)})
}
