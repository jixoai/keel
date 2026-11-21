// P5.2 sampling adapter: the executor must run on the runtime's model service
// and report objective facts (text, model, stop reason), never a price.

package servicehost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/mcphost"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/provider"
)

// recordingModelService resolves one fixed identity and records what the
// sampler asked it for.
type recordingModelService struct {
	resolved  agent.ActiveModel
	err       error
	selection json.RawMessage
	scope     kernel.Scope
}

func (m *recordingModelService) Resolve(_ context.Context, scope kernel.Scope, selection json.RawMessage) (agent.ActiveModel, error) {
	m.scope = scope
	m.selection = append(json.RawMessage(nil), selection...)
	return m.resolved, m.err
}

func (m *recordingModelService) ValidateSelection(json.RawMessage) error { return nil }
func (m *recordingModelService) ProviderViews() []modelcfg.ProviderView  { return nil }
func (m *recordingModelService) ModelViews() []modelcfg.ModelView        { return nil }
func (m *recordingModelService) ResolutionIssues() []modelcfg.Issue      { return nil }
func (m *recordingModelService) DefaultSelection() modelcfg.Selection    { return modelcfg.Selection{} }
func (m *recordingModelService) ProbeProvider(context.Context, string) (modelcfg.ProviderProbe, bool, error) {
	return modelcfg.ProviderProbe{}, false, nil
}

// samplingTextStream answers one completion with fixed text and captures the
// provider config the sampler handed to the last layer.
func samplingTextStream(capture func(provider.StreamConfig)) provider.StreamFn {
	return func(ctx context.Context, _ string, _ provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		if capture != nil {
			capture(cfg)
		}
		message := agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent("sampled")},
			StopReason: agentcore.StopReasonEndTurn,
		}
		stream := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer stream.Close()
			_ = stream.Emit(ctx, provider.StreamStartEvent{Partial: message})
			_ = stream.Emit(ctx, provider.StreamDoneEvent{Message: message})
		}()
		return stream, nil
	}
}

// TestRuntimeSamplerForwardsMaxTokensToTheProvider pins the review finding: the
// server's maxTokens must reach the provider request, not only the executor's
// normalization.
func TestRuntimeSamplerForwardsMaxTokensToTheProvider(t *testing.T) {
	for _, want := range []int{4096, 128} {
		var got provider.StreamConfig
		models := &recordingModelService{resolved: agent.ActiveModel{
			ProviderID: "fixture", Model: "fixture-model", APIKey: "k",
			Stream: samplingTextStream(func(cfg provider.StreamConfig) { got = cfg }),
		}}
		sampler := &runtimeSampler{models: models}
		if _, err := sampler.Sample(context.Background(), mcphost.SamplingRequest{
			Messages:  []mcphost.SamplingMessage{{Role: "user", Text: "hi"}},
			MaxTokens: want,
		}); err != nil {
			t.Fatalf("maxTokens %d: %v", want, err)
		}
		if got.Extra["max_tokens"] != want {
			t.Fatalf("maxTokens %d reached the provider as %#v, want %d", want, got.Extra, want)
		}
	}

	// No cap: the provider's own default stays in charge.
	var got provider.StreamConfig
	models := &recordingModelService{resolved: agent.ActiveModel{
		ProviderID: "fixture", Model: "fixture-model", APIKey: "k",
		Stream: samplingTextStream(func(cfg provider.StreamConfig) { got = cfg }),
	}}
	sampler := &runtimeSampler{models: models}
	if _, err := sampler.Sample(context.Background(), mcphost.SamplingRequest{
		Messages: []mcphost.SamplingMessage{{Role: "user", Text: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Extra["max_tokens"]; ok {
		t.Fatalf("an absent cap was forwarded: %#v", got.Extra)
	}
}

// TestRuntimeSamplerResolvesTheAttemptModelSelection pins the session-model
// fix: the sampler resolves the runner's stamped selection fact in the call's
// tenant/session scope; without the key the deployment default (nil) applies.
func TestRuntimeSamplerResolvesTheAttemptModelSelection(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	raw := json.RawMessage(`{"providerId":"p","modelId":"m"}`)
	ctx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: tenant.String(), SessionID: session.String(),
	})
	ctx = agentcore.WithToolModelSelection(ctx, raw)

	models := &recordingModelService{resolved: agent.ActiveModel{
		ProviderID: "p", Model: "m", APIKey: "k", Stream: samplingTextStream(nil),
	}}
	sampler := &runtimeSampler{models: models}
	if _, err := sampler.Sample(ctx, mcphost.SamplingRequest{
		Messages: []mcphost.SamplingMessage{{Role: "user", Text: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if string(models.selection) != string(raw) {
		t.Fatalf("resolver selection = %s, want %s", models.selection, raw)
	}
	if models.scope.TenantID != tenant || models.scope.SessionID != session {
		t.Fatalf("resolver scope = %+v, want the tool call's tenant/session", models.scope)
	}

	// A tool call outside a runner attempt (the app bridge) has no key: the
	// deployment default is the honest resolution.
	models.selection, models.scope = nil, kernel.Scope{}
	if _, err := sampler.Sample(context.Background(), mcphost.SamplingRequest{
		Messages: []mcphost.SamplingMessage{{Role: "user", Text: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if models.selection != nil {
		t.Fatalf("a selection was invented for an unscoped call: %s", models.selection)
	}
	if !models.scope.IsZero() {
		t.Fatalf("an unscoped call resolved a scope: %+v", models.scope)
	}
}

func TestRuntimeSamplerUsesTheRuntimeStream(t *testing.T) {
	sampler := &runtimeSampler{
		fallback:     agent.FakeStream(),
		fallbackID:   "fake-model",
		fallbackKey:  "",
		fallbackName: "fake",
	}
	result, err := sampler.Sample(context.Background(), mcphost.SamplingRequest{
		Messages:  []mcphost.SamplingMessage{{Role: "user", Text: "hello"}},
		MaxTokens: 128,
	})
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if result.Role != "assistant" || !strings.Contains(result.Text, "hello") {
		t.Fatalf("result = %+v", result)
	}
	if result.Model != "" && result.Model != "fake-model" {
		t.Fatalf("model = %q", result.Model)
	}
	if result.StopReason != "endTurn" {
		t.Fatalf("stopReason = %q", result.StopReason)
	}
}

func TestSamplingStopReasonMapping(t *testing.T) {
	if got := samplingStopReason("length"); got != "maxTokens" {
		t.Fatalf("length = %q", got)
	}
	if got := samplingStopReason("end_turn"); got != "endTurn" {
		t.Fatalf("end_turn = %q", got)
	}
	if got := samplingStopReason(""); got != "endTurn" {
		t.Fatalf("empty = %q", got)
	}
}

func TestRuntimeSamplerWithoutAModelFailsClosed(t *testing.T) {
	sampler := &runtimeSampler{}
	if _, err := sampler.Sample(context.Background(), mcphost.SamplingRequest{}); err == nil {
		t.Fatal("a sampler without a stream produced an answer")
	}
}
