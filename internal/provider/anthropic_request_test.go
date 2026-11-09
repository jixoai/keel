package provider

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/modelcfg"
)

// The Anthropic Messages wire is driven by the resolved option spec: the
// factory's request patch (max_tokens, thinking) reaches the body, the level
// table is a fallback for a request that carries no resolved options, and the
// declared cap is never lifted.

// anthropicActiveModel resolves one built-in anthropic model through the real
// pipeline (embedded catalog + tenant overlay → resolver → factory), which is
// exactly what the runtime does at claim time.
func anthropicActiveModel(t *testing.T, tenantJSON, level string) modelcfg.ActiveModel {
	t.Helper()
	overlay, err := modelcfg.OverlayFromDocument([]byte(tenantJSON), modelcfg.LegacyAgent{})
	if err != nil {
		t.Fatalf("OverlayFromDocument: %v", err)
	}
	resolver, err := modelcfg.NewResolver(modelcfg.EmbeddedBuiltin(), overlay)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	resolution := resolver.Resolve()
	factory, err := modelcfg.NewFactory(&resolution)
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	active, err := factory.Resolve(modelcfg.Selection{
		ProviderID:     "anthropic",
		ModelID:        "claude-sonnet-4-5",
		ReasoningLevel: level,
	})
	if err != nil {
		t.Fatalf("Resolve(%s/%s, %s): %v", "anthropic", "claude-sonnet-4-5", level, err)
	}
	return active
}

// anthropicWireRequest builds the request the runtime builds from a resolved
// model: the model id, the frozen thinking level and the request patch
// forwarded as opaque options (internal/agent/runner.go does the same).
func anthropicWireRequest(active modelcfg.ActiveModel) CompletionRequest {
	return CompletionRequest{
		Model: active.ModelID,
		Context: LlmContext{
			SystemPrompt: "system",
			Messages: agentcore.MessageList{agentcore.UserMessage{
				RoleField: agentcore.RoleUser,
				Content:   agentcore.ContentList{agentcore.NewTextContent("hi")},
			}},
		},
		Config: StreamConfig{
			ThinkingLevel: agentcore.ThinkingLevel(active.ReasoningLevel),
			Extra:         active.RequestPatch,
		},
	}
}

// anthropicWireBody encodes and decodes the body for assertions.
func anthropicWireBody(t *testing.T, req CompletionRequest) map[string]any {
	t.Helper()
	raw, err := encodeAnthropicRequest(req, nil)
	if err != nil {
		t.Fatalf("encodeAnthropicRequest: %v", err)
	}
	return decodeBody(t, raw)
}

func bodyNumber(t *testing.T, body map[string]any, key string) int {
	t.Helper()
	value, ok := body[key].(float64)
	if !ok {
		t.Fatalf("body[%q] = %#v, want a number", key, body[key])
	}
	return int(value)
}

func bodyThinking(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("body thinking = %#v, want an object", body["thinking"])
	}
	return thinking
}

// TestAnthropicWireUsesResolvedCatalogDefaults (a): the built-in catalog's
// high reasoning patch declares a 6144-token thinking budget and the model's
// cap is 8192; the body must carry the resolved cap verbatim and a budget that
// fits under it with reply headroom — never the level table's 16384 and never
// a lifted 20480 (the pre-fix bug).
func TestAnthropicWireUsesResolvedCatalogDefaults(t *testing.T) {
	active := anthropicActiveModel(t, "", "high")
	if got := active.RequestPatch["max_tokens"]; got != 8192 {
		t.Fatalf("resolved patch max_tokens = %#v, want 8192", got)
	}
	body := anthropicWireBody(t, anthropicWireRequest(active))

	if got := bodyNumber(t, body, "max_tokens"); got != 8192 {
		t.Fatalf("max_tokens = %d, want the resolved cap 8192", got)
	}
	thinking := bodyThinking(t, body)
	if got := thinking["type"]; got != "enabled" {
		t.Fatalf("thinking.type = %#v, want enabled", got)
	}
	// 4096 = 8192 - anthropicResponseHeadroom: the resolved 6144 does not fit
	// with headroom, so the budget shrinks; the cap does not move.
	if got := bodyNumber(t, thinking, "budget_tokens"); got != 4096 {
		t.Fatalf("thinking.budget_tokens = %d, want 4096 (resolved 6144 shrunk under the 8192 cap)", got)
	}
}

// TestAnthropicWireRespectsTenantCap (b): a tenant cap of 1024 leaves no room
// for a valid thinking block, so the body carries the cap and no thinking at
// all — it is never raised to fit the level table.
func TestAnthropicWireRespectsTenantCap(t *testing.T) {
	active := anthropicActiveModel(t,
		`{"modelRules":[{"match":"*","config":{"options":{"maxOutputTokens":{"max":1024}}}}]}`,
		"high")
	if got := active.RequestPatch["max_tokens"]; got != 1024 {
		t.Fatalf("resolved patch max_tokens = %#v, want the tenant cap 1024", got)
	}
	// The factory drops a thinking block it cannot fit under the cap
	// (1024-1 < the Anthropic minimum of 1024): the patch must not ask for it.
	if _, ok := active.RequestPatch["thinking"]; ok {
		t.Fatalf("a 1024 cap cannot hold a valid thinking budget; patch = %#v", active.RequestPatch)
	}
	body := anthropicWireBody(t, anthropicWireRequest(active))

	if got := bodyNumber(t, body, "max_tokens"); got != 1024 {
		t.Fatalf("max_tokens = %d, want the tenant cap 1024", got)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatalf("a budget cannot fit under a 1024 cap; thinking must be omitted, got %#v", body["thinking"])
	}
}

// TestAnthropicWireHonoursExplicitThinkingPatch (c): an explicit resolved
// thinking budget that fits under the resolved cap travels verbatim instead of
// being replaced by the level table's high value (16384).
func TestAnthropicWireHonoursExplicitThinkingPatch(t *testing.T) {
	active := anthropicActiveModel(t,
		`{"providerModelRules":[{"providerId":"anthropic","modelId":"claude-sonnet-4-5",`+
			`"config":{"options":{"reasoningLevel":{"map":{"high":{"thinking":{"type":"enabled","budget_tokens":2048}}}}}}}]}`,
		"high")
	if got := anthropicThinkingBudget(agentcore.ThinkingLevel(active.ReasoningLevel)); got != 16384 {
		t.Fatalf("test premise: the level table maps high to 16384, got %d", got)
	}
	body := anthropicWireBody(t, anthropicWireRequest(active))

	if got := bodyNumber(t, body, "max_tokens"); got != 8192 {
		t.Fatalf("max_tokens = %d, want the resolved cap 8192", got)
	}
	thinking := bodyThinking(t, body)
	if got := bodyNumber(t, thinking, "budget_tokens"); got != 2048 {
		t.Fatalf("thinking.budget_tokens = %d, want the resolved 2048 (not the level table's 16384)", got)
	}
}

// TestAnthropicWireHonoursResolvedDisabledThinking: the built-in catalog's
// "off" level declares an explicit disabled block. The resolved declaration
// travels as declared (the level table, which would have sent nothing, has no
// say once the overlay carries thinking).
func TestAnthropicWireHonoursResolvedDisabledThinking(t *testing.T) {
	active := anthropicActiveModel(t, "", "off")
	body := anthropicWireBody(t, anthropicWireRequest(active))

	if got := bodyNumber(t, body, "max_tokens"); got != 8192 {
		t.Fatalf("max_tokens = %d, want the resolved cap 8192", got)
	}
	thinking := bodyThinking(t, body)
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking = %#v, want the resolved disabled block", thinking)
	}
	if _, ok := thinking["budget_tokens"]; ok {
		t.Fatalf("a disabled block must not grow a budget: %#v", thinking)
	}
}

// TestAnthropicWireScalesThinkingPatchToTheCap pins the shrink direction: a
// resolved budget at (or above) the cap is shrunk under it, the cap itself
// never moves.
func TestAnthropicWireScalesThinkingPatchToTheCap(t *testing.T) {
	active := anthropicActiveModel(t,
		`{"providerModelRules":[{"providerId":"anthropic","modelId":"claude-sonnet-4-5",`+
			`"config":{"options":{"reasoningLevel":{"map":{"high":{"thinking":{"type":"enabled","budget_tokens":6144}}}}}}}]}`,
		"high")
	body := anthropicWireBody(t, anthropicWireRequest(active))

	if got := bodyNumber(t, body, "max_tokens"); got != 8192 {
		t.Fatalf("max_tokens = %d, want 8192", got)
	}
	if got := bodyNumber(t, bodyThinking(t, body), "budget_tokens"); got != 4096 {
		t.Fatalf("thinking.budget_tokens = %d, want 4096 = 8192 - headroom", got)
	}
}

// TestAnthropicWireFallsBackToLevelTableWithoutOverlay (d): a request with no
// resolved options (the flat configuration path) keeps the historical body
// byte-for-byte: the level table still maps the level, and the driver's own
// default cap yields to it (that default was never declared by anyone).
func TestAnthropicWireFallsBackToLevelTableWithoutOverlay(t *testing.T) {
	tests := []struct {
		level        agentcore.ThinkingLevel
		wantMax      int
		wantBudget   int
		wantThinking bool
	}{
		{level: agentcore.ThinkingLow, wantMax: 8192, wantBudget: 2048, wantThinking: true},
		{level: agentcore.ThinkingHigh, wantMax: 20480, wantBudget: 16384, wantThinking: true},
		{level: agentcore.ThinkingOff, wantMax: 8192, wantThinking: false},
	}
	for _, test := range tests {
		t.Run(string(test.level), func(t *testing.T) {
			req := anthropicWireRequest(modelcfg.ActiveModel{ModelID: "claude-sonnet-4-5"})
			req.Config.ThinkingLevel = test.level
			body := anthropicWireBody(t, req)

			if got := bodyNumber(t, body, "max_tokens"); got != test.wantMax {
				t.Fatalf("max_tokens = %d, want %d", got, test.wantMax)
			}
			if !test.wantThinking {
				if _, ok := body["thinking"]; ok {
					t.Fatalf("thinking = %#v, want none for level %q", body["thinking"], test.level)
				}
				return
			}
			thinking := bodyThinking(t, body)
			if got := bodyNumber(t, thinking, "budget_tokens"); got != test.wantBudget {
				t.Fatalf("thinking.budget_tokens = %d, want %d", got, test.wantBudget)
			}
		})
	}
}

// TestAnthropicWireOverlayCannotRewriteEnvelope: the overlay owns its
// extensions (max_tokens, thinking) but never the protocol envelope.
func TestAnthropicWireOverlayCannotRewriteEnvelope(t *testing.T) {
	req := anthropicWireRequest(modelcfg.ActiveModel{ModelID: "claude-sonnet-4-5"})
	req.Config.Extra = map[string]any{
		"model":    "evil",
		"messages": []any{},
		"stream":   false,
		"system":   "evil",
		"tools":    []any{},
		"reasoning_context": map[string]any{
			"type": "enabled",
		},
	}
	body := anthropicWireBody(t, req)

	if body["model"] != "claude-sonnet-4-5" {
		t.Fatalf("the overlay rewrote the model: %v", body["model"])
	}
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) != 1 {
		t.Fatalf("the overlay rewrote the transcript: %v", body["messages"])
	}
	if body["stream"] != true {
		t.Fatalf("the overlay rewrote stream: %v", body["stream"])
	}
	if body["system"] != "system" {
		t.Fatalf("the overlay rewrote the system prompt: %v", body["system"])
	}
	if _, ok := body["tools"]; ok {
		t.Fatalf("the overlay injected tools: %v", body["tools"])
	}
	extension, ok := body["reasoning_context"].(map[string]any)
	if !ok || extension["type"] != "enabled" {
		t.Fatalf("the overlay's own extension did not land: %v", body["reasoning_context"])
	}
}

// TestAnthropicWireReadsJSONDecodedNumbers: an overlay can reach the driver as
// decoded JSON (float64, or json.Number under a UseNumber decoder, and arrays
// as []any) — every shape must be understood. A missed type assertion would
// silently drop the cap or the thinking budget.
func TestAnthropicWireReadsJSONDecodedNumbers(t *testing.T) {
	tests := []struct {
		name  string
		extra func(t *testing.T) map[string]any
	}{
		{
			name: "float64 (json.Unmarshal)",
			extra: func(t *testing.T) map[string]any {
				t.Helper()
				var extra map[string]any
				if err := json.Unmarshal([]byte(
					`{"max_tokens":8192,"thinking":{"type":"enabled","budget_tokens":1536}}`), &extra); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return extra
			},
		},
		{
			name: "json.Number (UseNumber)",
			extra: func(t *testing.T) map[string]any {
				t.Helper()
				decoder := json.NewDecoder(bytes.NewReader([]byte(
					`{"max_tokens":8192,"thinking":{"type":"enabled","budget_tokens":1536}}`)))
				decoder.UseNumber()
				var extra map[string]any
				if err := decoder.Decode(&extra); err != nil {
					t.Fatalf("decode: %v", err)
				}
				return extra
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := anthropicWireRequest(modelcfg.ActiveModel{ModelID: "claude-sonnet-4-5"})
			req.Config.Extra = test.extra(t)
			body := anthropicWireBody(t, req)

			if got := bodyNumber(t, body, "max_tokens"); got != 8192 {
				t.Fatalf("max_tokens = %d, want the JSON-decoded cap 8192", got)
			}
			thinking := bodyThinking(t, body)
			if got := bodyNumber(t, thinking, "budget_tokens"); got != 1536 {
				t.Fatalf("thinking.budget_tokens = %d, want the JSON-decoded 1536", got)
			}
		})
	}
}

// TestAnthropicWireRejectsNonObjectThinking: a malformed (non-object) thinking
// declaration is dropped rather than sent to the wire, and it does not fall
// back to the level table: the overlay said something, even if invalid.
func TestAnthropicWireRejectsNonObjectThinking(t *testing.T) {
	req := anthropicWireRequest(modelcfg.ActiveModel{ModelID: "claude-sonnet-4-5"})
	req.Config.ThinkingLevel = agentcore.ThinkingHigh
	req.Config.Extra = map[string]any{"thinking": "literal"}
	body := anthropicWireBody(t, req)

	if _, ok := body["thinking"]; ok {
		t.Fatalf("a non-object thinking declaration must not reach the wire: %#v", body["thinking"])
	}
	if got := bodyNumber(t, body, "max_tokens"); got != 8192 {
		t.Fatalf("max_tokens = %d, want the fallback default 8192", got)
	}
}

// TestAnthropicWireThinkingWithUnreadableBudgetTravelsVerbatim: an object
// declaration without a readable numeric budget (for example an explicit
// disabled block) has nothing to fit and travels as declared.
func TestAnthropicWireThinkingWithUnreadableBudgetTravelsVerbatim(t *testing.T) {
	req := anthropicWireRequest(modelcfg.ActiveModel{ModelID: "claude-sonnet-4-5"})
	req.Config.Extra = map[string]any{"thinking": map[string]any{"type": "disabled"}}
	body := anthropicWireBody(t, req)

	thinking := bodyThinking(t, body)
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking = %#v, want the declared disabled block", thinking)
	}
	if _, ok := thinking["budget_tokens"]; ok {
		t.Fatalf("a disabled block must not grow a budget: %v", thinking)
	}
}
