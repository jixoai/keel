package harnessapi

// PUT /v1/settings/models with a decision provider: the media wiring is the
// validator, so a binding Preflight failure comes back as the 400 body's
// field-path issues and nothing is stored or applied.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/mediawiring"
	"github.com/jixoai/keel/internal/modelcfg"
)

// mediaSettingsRuntime is the settings runtime seam over the real media wiring
// validation (no provider call, no assembly).
type mediaSettingsRuntime struct {
	deployment json.RawMessage
}

func (m *mediaSettingsRuntime) DeploymentModels() json.RawMessage { return m.deployment }

func (m *mediaSettingsRuntime) RejectedModelsOverride() (int64, string, bool) { return 0, "", false }

func (m *mediaSettingsRuntime) ValidateModelsDocument(document json.RawMessage) (ModelCandidate, []modelcfg.Issue, error) {
	if issues := mediawiring.ValidateModels(mediawiring.Options{Models: document}); len(issues) > 0 {
		return ModelCandidate{}, issues, nil
	}
	return ModelCandidate{Selection: modelcfg.Selection{ProviderID: "judge", ModelID: "judge-1"}}, nil, nil
}

func (m *mediaSettingsRuntime) ApplyModels(context.Context, json.RawMessage) error { return nil }

func decisionSettingsDocument(adapter string, binding any) map[string]any {
	return map[string]any{
		"providers": []any{map[string]any{
			"id":       "judge",
			"type":     "decision",
			"api":      map[string]any{"type": "openai-chat-completions", "baseUrl": "https://decision.example/v1"},
			"access":   map[string]any{"type": "api-key", "env": "DECISION_KEY"},
			"modelIds": []string{"judge-1"},
			"decision": map[string]any{"adapter": adapter, "binding": binding},
		}},
		"selection": map[string]any{"providerId": "judge", "modelId": "judge-1"},
	}
}

func TestSettingsModelsRefusesADecisionBindingThatFailsPreflight(t *testing.T) {
	settings := newFakeModelSettingsStore()
	runtime := &mediaSettingsRuntime{deployment: json.RawMessage(`{}`)}
	server := settingsServer(t, func(cfg *Config) {
		cfg.ModelSettingsStore = settings
		cfg.ModelSettingsRuntime = runtime
	})
	client := server.Client()
	tenant := uuid.New()

	// Cloudflare does not support box questions: the save is refused with the
	// field path the settings form highlights.
	document := decisionSettingsDocument("cloudflare", map[string]any{
		"state":     map[string]any{"image_url": "data:image/png;base64,AAAA"},
		"questions": map[string]any{"find": map[string]any{"type": "box", "instructions": "locate the label"}},
	})
	var body struct {
		Error  string           `json:"error"`
		Issues []modelcfg.Issue `json:"issues"`
	}
	code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/models", tenant, document, &body)
	if code != http.StatusBadRequest {
		t.Fatalf("box on cloudflare = %d, want 400 (%+v)", code, body)
	}
	found := false
	for _, issue := range body.Issues {
		if issue.Field == "providers[judge].decision.binding.questions.find.type" {
			found = true
		}
	}
	if !found {
		t.Fatalf("400 body does not carry the question-type field: %+v", body.Issues)
	}
	if revision := settingsRow(t, settings, tenant); revision != 0 {
		t.Fatalf("a refused document must not be stored: revision %d", revision)
	}

	// The same binding on TypeSafe is accepted: box is part of its surface.
	document = decisionSettingsDocument("typesafe", map[string]any{
		"state":     map[string]any{"image_url": "data:image/png;base64,AAAA"},
		"questions": map[string]any{"find": map[string]any{"type": "box", "instructions": "locate the label"}},
	})
	body.Issues = nil
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/models", tenant, document, &body); code != http.StatusOK {
		t.Fatalf("typesafe box = %d, want 200 (%s)", code, strings.Join(issueFields(body.Issues), ", "))
	}
}

func issueFields(issues []modelcfg.Issue) []string {
	fields := make([]string, 0, len(issues))
	for _, issue := range issues {
		fields = append(fields, issue.Field+": "+issue.Message)
	}
	return fields
}
