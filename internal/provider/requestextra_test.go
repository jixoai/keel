package provider

import (
	"encoding/json"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// Request overlays (StreamConfig.Extra): the resolved model's request patch
// must reach the wire on every driver, and it must never be able to rewrite the
// envelope the driver built.

func userPromptRequest(model string, extra map[string]any) CompletionRequest {
	return CompletionRequest{
		Model: model,
		Context: LlmContext{
			SystemPrompt: "system",
			Messages: agentcore.MessageList{agentcore.UserMessage{
				RoleField: agentcore.RoleUser,
				Content:   agentcore.ContentList{agentcore.NewTextContent("hi")},
			}},
		},
		Config: StreamConfig{Extra: extra},
	}
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v\nbody: %s", err, raw)
	}
	return body
}

func TestApplyRequestExtraIsSparseAndProtectsManagedKeys(t *testing.T) {
	body := map[string]any{
		"model":    "gpt-5",
		"stream":   true,
		"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(2048)},
	}
	applyRequestExtra(body, map[string]any{
		"model":       "evil",
		"stream":      false,
		"max_tokens":  float64(512),
		"ignored_nil": nil,
		"thinking":    map[string]any{"budget_tokens": float64(1024)},
		"new_object":  map[string]any{"a": true, "skip": nil},
	}, "model", "stream")

	if body["model"] != "gpt-5" || body["stream"] != true {
		t.Fatalf("managed keys must keep the driver's values: %v", body)
	}
	if body["max_tokens"] != float64(512) {
		t.Fatalf("extension key did not land: %v", body)
	}
	if _, ok := body["ignored_nil"]; ok {
		t.Fatalf("a nil overlay value must be skipped: %v", body)
	}
	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking must stay an object: %v", body)
	}
	if thinking["budget_tokens"] != float64(1024) {
		t.Fatalf("nested overlay value did not land: %v", thinking)
	}
	if thinking["type"] != "enabled" {
		t.Fatalf("a nested overlay must not erase the existing object's siblings: %v", thinking)
	}
	object, ok := body["new_object"].(map[string]any)
	if !ok || object["a"] != true {
		t.Fatalf("a new nested object must be set wholesale: %v", body)
	}
	if _, ok := object["skip"]; ok {
		t.Fatalf("a nil value inside a nested patch must be skipped: %v", object)
	}
}

// TestApplyRequestExtraOnJSONDecodedOverlay covers the shapes an overlay
// actually arrives in: a tenant document is decoded JSON (map[string]any,
// []any, float64 numbers), not a hand-built Go map. A non-map value must
// replace wholesale, and a nested patch over a body key whose existing value is
// not an object must replace it rather than crash.
func TestApplyRequestExtraOnJSONDecodedOverlay(t *testing.T) {
	var overlay map[string]any
	raw := []byte(`{
		"max_tokens": 1024,
		"stop": ["a", "b"],
		"thinking": {"budget_tokens": 1536},
		"metadata": {"trace": {"nested": true}}
	}`)
	if err := json.Unmarshal(raw, &overlay); err != nil {
		t.Fatalf("unmarshal overlay: %v", err)
	}
	body := map[string]any{
		"model":    "m",
		"thinking": "literal",
		"stop":     "not-a-list",
	}
	applyRequestExtra(body, overlay, "model")

	if body["model"] != "m" {
		t.Fatalf("managed key rewritten: %v", body)
	}
	if body["max_tokens"] != float64(1024) {
		t.Fatalf("a JSON-decoded number must land as its decoded shape: %#v", body["max_tokens"])
	}
	if stop, ok := body["stop"].([]any); !ok || len(stop) != 2 {
		t.Fatalf("a []any value must replace wholesale: %#v", body["stop"])
	}
	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("a nested patch over a non-map body value must replace it with the object: %#v", body["thinking"])
	}
	if thinking["budget_tokens"] != float64(1536) {
		t.Fatalf("the nested patch did not land: %#v", thinking)
	}
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("a new nested object must be set wholesale: %#v", body["metadata"])
	}
	trace, ok := metadata["trace"].(map[string]any)
	if !ok || trace["nested"] != true {
		t.Fatalf("nested decoded objects must survive the merge: %#v", metadata)
	}
}

// TestChatBodyCarriesRequestExtra pins the Chat Completions overlay: the
// resolved model's request patch lands in the body, and its envelope keys are
// untouchable.
func TestChatBodyCarriesRequestExtra(t *testing.T) {
	req := userPromptRequest("deepseek-v4.1-flash", map[string]any{
		"max_tokens":     float64(512),
		"x_vendor_flag":  true,
		"model":          "evil",
		"messages":       []any{},
		"stream_options": map[string]any{"include_usage": false},
	})
	raw, err := encodeOpenAIRequest(req)
	if err != nil {
		t.Fatalf("encodeOpenAIRequest: %v", err)
	}
	body := decodeBody(t, raw)
	if body["max_tokens"] != float64(512) {
		t.Fatalf("max_tokens did not reach the wire: %v", body)
	}
	if body["x_vendor_flag"] != true {
		t.Fatalf("an unknown extension key must pass through: %v", body)
	}
	if body["model"] != "deepseek-v4.1-flash" {
		t.Fatalf("the overlay rewrote the model: %v", body["model"])
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("the overlay rewrote the transcript: %v", body["messages"])
	}
	streamOptions, ok := body["stream_options"].(map[string]any)
	if !ok || streamOptions["include_usage"] != true {
		t.Fatalf("the overlay rewrote stream_options: %v", body["stream_options"])
	}
}

// TestResponsesBodyCarriesRequestExtra pins the Responses overlay: the patch
// lands through the SDK's raw-JSON override, the driver's reasoning summary
// survives a one-level merge, and the envelope keys stay the driver's.
func TestResponsesBodyCarriesRequestExtra(t *testing.T) {
	req := userPromptRequest("gpt-5", map[string]any{
		"max_output_tokens": float64(77),
		"x_custom":          "v",
		"reasoning":         map[string]any{"effort": "high"},
		"model":             "evil",
		"input":             []any{},
		"instructions":      "evil",
	})
	req.Config.ThinkingLevel = agentcore.ThinkingLow
	params, err := buildResponsesParams(req)
	if err != nil {
		t.Fatalf("buildResponsesParams: %v", err)
	}
	raw, err := params.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	body := decodeBody(t, raw)
	if body["max_output_tokens"] != float64(77) {
		t.Fatalf("max_output_tokens did not reach the wire: %v", body)
	}
	if body["x_custom"] != "v" {
		t.Fatalf("an unknown extension key must pass through: %v", body)
	}
	if body["model"] != "gpt-5" {
		t.Fatalf("the overlay rewrote the model: %v", body["model"])
	}
	if body["instructions"] != "system" {
		t.Fatalf("the overlay rewrote instructions: %v", body["instructions"])
	}
	if items, ok := body["input"].([]any); !ok || len(items) == 0 {
		t.Fatalf("the overlay rewrote the input items: %v", body["input"])
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning must stay an object: %v", body)
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("the overlay's reasoning effort did not land: %v", reasoning)
	}
	if reasoning["summary"] != "auto" {
		t.Fatalf("a one-level merge must keep the driver's reasoning summary: %v", reasoning)
	}
}

// TestResponsesBodyWithoutExtraKeepsTypedParams: an attempt with no overlay
// must not take the raw-JSON path at all.
func TestResponsesBodyWithoutExtraKeepsTypedParams(t *testing.T) {
	params, err := buildResponsesParams(userPromptRequest("gpt-5", nil))
	if err != nil {
		t.Fatalf("buildResponsesParams: %v", err)
	}
	if override, ok := params.Overrides(); ok && override != nil {
		t.Fatalf("no overlay means the typed params travel as-is: %v", override)
	}
	raw, err := params.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	body := decodeBody(t, raw)
	if body["model"] != "gpt-5" || body["instructions"] != "system" {
		t.Fatalf("typed params body: %v", body)
	}
}
