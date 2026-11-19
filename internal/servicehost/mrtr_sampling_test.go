// P5.2 sampling adapter: the executor must run on the runtime's model service
// and report objective facts (text, model, stop reason), never a price.

package servicehost

import (
	"context"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/mcphost"
)

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
