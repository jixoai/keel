package modelcfg

import (
	"errors"
	"strings"
	"testing"
)

func TestFactoryProtocolPatches(t *testing.T) {
	cat := EmbeddedBuiltin()
	resolution := resolveFrom(t, cat, Overlay{})
	factory := mustFactory(t, resolution)

	tests := []struct {
		name        string
		selection   Selection
		wantLevel   string
		wantAPI     string
		wantMax     int
		wantEffort  string
		wantThink   string
		wantBudget  int
		maxTokenKey string
	}{
		{
			name:        "openai chat high",
			selection:   Selection{ProviderID: "s2api", ModelID: "deepseek-v4.1-flash", ReasoningLevel: "high"},
			wantLevel:   "high",
			wantAPI:     APITypeOpenAIChatCompletions,
			wantMax:     8192,
			wantEffort:  "high",
			maxTokenKey: "max_tokens",
		},
		{
			name:        "openai chat off",
			selection:   Selection{ProviderID: "s2api", ModelID: "deepseek-v4.1-flash", ReasoningLevel: "off"},
			wantLevel:   "off",
			wantAPI:     APITypeOpenAIChatCompletions,
			wantMax:     8192,
			maxTokenKey: "max_tokens",
		},
		{
			name:        "openai responses high",
			selection:   Selection{ProviderID: "openai", ModelID: "gpt-5", ReasoningLevel: "high"},
			wantLevel:   "high",
			wantAPI:     APITypeOpenAIResponses,
			wantMax:     8192,
			wantEffort:  "high",
			maxTokenKey: "max_output_tokens",
		},
		{
			name:        "anthropic high",
			selection:   Selection{ProviderID: "anthropic", ModelID: "claude-sonnet-4-5", ReasoningLevel: "high"},
			wantLevel:   "high",
			wantAPI:     APITypeAnthropicMessages,
			wantMax:     8192,
			wantThink:   "enabled",
			wantBudget:  6144,
			maxTokenKey: "max_tokens",
		},
		{
			name:        "anthropic off",
			selection:   Selection{ProviderID: "anthropic", ModelID: "claude-sonnet-4-5", ReasoningLevel: "off"},
			wantLevel:   "off",
			wantAPI:     APITypeAnthropicMessages,
			wantMax:     8192,
			wantThink:   "disabled",
			maxTokenKey: "max_tokens",
		},
		{
			name:        "empty level defaults to first value",
			selection:   Selection{ProviderID: "s2api", ModelID: "deepseek-v4.1-flash"},
			wantLevel:   "off",
			wantAPI:     APITypeOpenAIChatCompletions,
			wantMax:     8192,
			maxTokenKey: "max_tokens",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			active, err := factory.Resolve(test.selection)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if active.ProviderID != test.selection.ProviderID || active.ModelID != test.selection.ModelID {
				t.Fatalf("identity = %s/%s", active.ProviderID, active.ModelID)
			}
			if active.API.Type != test.wantAPI {
				t.Fatalf("api.type = %q, want %q", active.API.Type, test.wantAPI)
			}
			if active.ReasoningLevel != test.wantLevel {
				t.Fatalf("reasoningLevel = %q, want %q", active.ReasoningLevel, test.wantLevel)
			}
			if active.MaxOutputTokens != test.wantMax {
				t.Fatalf("maxOutputTokens = %d, want %d", active.MaxOutputTokens, test.wantMax)
			}
			if got := patchInt(t, active.RequestPatch, test.maxTokenKey); got != test.wantMax {
				t.Fatalf("patch[%s] = %d, want %d", test.maxTokenKey, got, test.wantMax)
			}
			if test.maxTokenKey == "max_tokens" {
				if _, ok := active.RequestPatch["max_output_tokens"]; ok {
					t.Fatalf("chat/anthropic patch must not contain max_output_tokens: %#v", active.RequestPatch)
				}
			} else if _, ok := active.RequestPatch["max_tokens"]; ok {
				t.Fatalf("responses patch must not contain max_tokens: %#v", active.RequestPatch)
			}
			if test.wantEffort != "" {
				switch test.wantAPI {
				case APITypeOpenAIChatCompletions:
					if got := patchString(t, active.RequestPatch, "reasoning_effort"); got != test.wantEffort {
						t.Fatalf("reasoning_effort = %q, want %q", got, test.wantEffort)
					}
				case APITypeOpenAIResponses:
					reasoning := patchObject(t, active.RequestPatch, "reasoning")
					if got := patchString(t, reasoning, "effort"); got != test.wantEffort {
						t.Fatalf("reasoning.effort = %q, want %q", got, test.wantEffort)
					}
				}
			} else if _, ok := active.RequestPatch["reasoning_effort"]; ok {
				t.Fatalf("off/default level must not send reasoning_effort: %#v", active.RequestPatch)
			}
			if test.wantThink != "" {
				thinking := patchObject(t, active.RequestPatch, "thinking")
				if got := patchString(t, thinking, "type"); got != test.wantThink {
					t.Fatalf("thinking.type = %q, want %q", got, test.wantThink)
				}
				if test.wantBudget > 0 {
					if got := patchInt(t, thinking, "budget_tokens"); got != test.wantBudget {
						t.Fatalf("thinking.budget_tokens = %d, want %d", got, test.wantBudget)
					}
				}
			}
		})
	}
}

func TestFactoryUsesModelMaxOutputTokens(t *testing.T) {
	cat := newTestCatalog(t, func(doc map[string]any) {
		rule := doc["modelRules"].([]any)[0].(map[string]any)
		options := rule["config"].(map[string]any)["options"].(map[string]any)
		options["maxOutputTokens"].(map[string]any)["max"] = 4096
	})
	resolution := resolveFrom(t, cat, Overlay{})
	factory := mustFactory(t, resolution)
	active, err := factory.Resolve(Selection{ProviderID: "chat", ModelID: "deepseek-v4.1-flash", ReasoningLevel: "high"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if active.MaxOutputTokens != 4096 {
		t.Fatalf("maxOutputTokens = %d, want 4096", active.MaxOutputTokens)
	}
	if got := patchInt(t, active.RequestPatch, "max_tokens"); got != 4096 {
		t.Fatalf("max_tokens = %d, want 4096", got)
	}
}

func TestFactoryClampsAnthropicThinkingBudget(t *testing.T) {
	cat := newTestCatalog(t, func(doc map[string]any) {
		rule := doc["modelRules"].([]any)[0].(map[string]any)
		options := rule["config"].(map[string]any)["options"].(map[string]any)
		options["maxOutputTokens"].(map[string]any)["max"] = 2048
	})
	resolution := resolveFrom(t, cat, Overlay{})
	factory := mustFactory(t, resolution)
	active, err := factory.Resolve(Selection{ProviderID: "anth", ModelID: "claude-test", ReasoningLevel: "high"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	thinking := patchObject(t, active.RequestPatch, "thinking")
	// Anthropic requires budget_tokens strictly below max_tokens, so the patch
	// clamps under the cap (2048-1); the provider driver additionally reserves
	// reply headroom under the cap and drops the block when nothing fits.
	if got := patchInt(t, thinking, "budget_tokens"); got != 2047 {
		t.Fatalf("thinking budget must be clamped below maxOutputTokens, got %d", got)
	}
	if got := patchInt(t, active.RequestPatch, "max_tokens"); got != 2048 {
		t.Fatalf("max_tokens = %d", got)
	}
}

func TestFactoryErrors(t *testing.T) {
	cat := EmbeddedBuiltin()
	resolution := resolveFrom(t, cat, Overlay{})
	factory := mustFactory(t, resolution)

	tests := []struct {
		name      string
		selection Selection
		field     string
	}{
		{
			name:      "unknown provider",
			selection: Selection{ProviderID: "nope", ModelID: "deepseek-v4.1-flash"},
			field:     "providerId",
		},
		{
			name:      "unknown model for provider",
			selection: Selection{ProviderID: "s2api", ModelID: "nope"},
			field:     "modelId",
		},
		{
			name:      "unknown model without provider",
			selection: Selection{ModelID: "nope"},
			field:     "modelId",
		},
		{
			name:      "unknown reasoning level",
			selection: Selection{ProviderID: "s2api", ModelID: "deepseek-v4.1-flash", ReasoningLevel: "extreme"},
			field:     "reasoningLevel",
		},
		{
			name:      "empty model id",
			selection: Selection{ProviderID: "s2api"},
			field:     "modelId",
		},
		{
			name:      "ambiguous model without provider",
			selection: Selection{ModelID: "deepseek-v4.1-flash"},
			field:     "providerId",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := factory.Resolve(test.selection)
			if err == nil {
				t.Fatalf("Resolve must fail")
			}
			if !errors.Is(err, ErrNotExecutable) {
				t.Fatalf("error must wrap ErrNotExecutable, got %v", err)
			}
			var notExecutable *NotExecutableError
			if !errors.As(err, &notExecutable) {
				t.Fatalf("error must be *NotExecutableError, got %T", err)
			}
			if len(notExecutable.Issues) == 0 {
				t.Fatalf("error must carry issues")
			}
			found := false
			for _, issue := range notExecutable.Issues {
				if issue.Field == test.field {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("no issue with field %q in %v", test.field, issueMessages(notExecutable.Issues))
			}
		})
	}
}

func TestFactoryUniqueModelWithoutProvider(t *testing.T) {
	cat := EmbeddedBuiltin()
	factory := mustFactory(t, resolveFrom(t, cat, Overlay{}))
	active, err := factory.Resolve(Selection{ModelID: "gpt-5"})
	if err != nil {
		t.Fatalf("unique model id must resolve: %v", err)
	}
	if active.ProviderID != "openai" {
		t.Fatalf("provider = %q, want openai", active.ProviderID)
	}
}

func TestFactoryRequestPatchIsFresh(t *testing.T) {
	cat := EmbeddedBuiltin()
	factory := mustFactory(t, resolveFrom(t, cat, Overlay{}))
	selection := Selection{ProviderID: "anthropic", ModelID: "claude-sonnet-4-5", ReasoningLevel: "high"}

	first, err := factory.Resolve(selection)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	first.RequestPatch["max_tokens"] = 1
	thinking := patchObject(t, first.RequestPatch, "thinking")
	thinking["budget_tokens"] = 1
	first.Options.ReasoningLevel.Values[0] = "mutated"

	second, err := factory.Resolve(selection)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := patchInt(t, second.RequestPatch, "max_tokens"); got != 8192 {
		t.Fatalf("second patch max_tokens = %d, want 8192", got)
	}
	if got := patchInt(t, patchObject(t, second.RequestPatch, "thinking"), "budget_tokens"); got != 6144 {
		t.Fatalf("second patch thinking budget = %d, want 6144", got)
	}
	if second.Options.ReasoningLevel.Values[0] != "off" {
		t.Fatalf("factory options were mutated: %v", second.Options.ReasoningLevel.Values)
	}
}

func TestNewFactoryRejectsNilResolution(t *testing.T) {
	if _, err := NewFactory(nil); err == nil {
		t.Fatalf("NewFactory(nil) must fail")
	}
}

func TestFactoryUnknownLevelErrorMentionsValues(t *testing.T) {
	cat := EmbeddedBuiltin()
	factory := mustFactory(t, resolveFrom(t, cat, Overlay{}))
	_, err := factory.Resolve(Selection{ProviderID: "s2api", ModelID: "deepseek-v4.1-flash", ReasoningLevel: "extreme"})
	if err == nil {
		t.Fatalf("unknown level must fail")
	}
	var notExecutable *NotExecutableError
	if !errors.As(err, &notExecutable) {
		t.Fatalf("error type = %T", err)
	}
	found := false
	for _, issue := range notExecutable.Issues {
		if issue.Field == "reasoningLevel" && strings.Contains(issue.Message, "off|low|medium|high") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown level issue should list allowed values: %v", issueMessages(notExecutable.Issues))
	}
}

func TestFactoryRejectsPatchKeyOutsideValues(t *testing.T) {
	cat := newTestCatalog(t, func(doc map[string]any) {
		rule := doc["modelRules"].([]any)[0].(map[string]any)
		options := rule["config"].(map[string]any)["options"].(map[string]any)
		reasoning := options["reasoningLevel"].(map[string]any)
		reasoning["values"] = anyStrings("off")
		reasoning["map"] = map[string]any{
			"off":  map[string]any{},
			"high": map[string]any{"reasoning_effort": "high"},
		}
	})
	factory := mustFactory(t, resolveFrom(t, cat, Overlay{}))
	_, err := factory.Resolve(Selection{ProviderID: "chat", ModelID: "deepseek-v4.1-flash", ReasoningLevel: "high"})
	if err == nil {
		t.Fatalf("a patch key outside values must be rejected")
	}
	if !errors.Is(err, ErrNotExecutable) {
		t.Fatalf("error must wrap ErrNotExecutable, got %v", err)
	}
	var notExecutable *NotExecutableError
	if !errors.As(err, &notExecutable) {
		t.Fatalf("error type = %T", err)
	}
	requireIssue(t, notExecutable.Issues, "reasoningLevel", "")
}
