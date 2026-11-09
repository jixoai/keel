package servicehost

import (
	"context"
	"testing"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/servicegraph"
)

// TestLLMServiceSendsTheModelIDNotTheProviderName pins the wire model id.
//
// The runner sends Model() to the provider, so it must be the id the provider
// actually serves. ResolveProvider's second return value is the provider's
// registry name ("openai"), and using it here made every real-model request
// carry "model":"openai" — which the gateway rejects with model_not_found
// (caught by the real-model E2E, scripts/real-model-e2e.sh).
func TestLLMServiceSendsTheModelIDNotTheProviderName(t *testing.T) {
	cfg := Config{
		Tenant: host.TenantConfig{Agent: host.AgentConfig{
			Model:    "deepseek-v4.1-flash",
			Protocol: "openai",
			BaseURL:  "https://example.invalid/v1",
		}},
		LLM: LLMOptions{APIKey: "test-key", LookupEnv: func(string) (string, bool) { return "", false }},
	}
	instance, err := newLLMFactory(cfg.Tenant, cfg)(context.Background(), servicegraph.Resolved{})
	if err != nil {
		t.Fatalf("newLLMFactory: %v", err)
	}
	svc, ok := instance.(*llmService)
	if !ok {
		t.Fatalf("instance = %T", instance)
	}
	if got := svc.Model(); got != "deepseek-v4.1-flash" {
		t.Fatalf("Model() = %q, want the wire model id (a provider name here is model_not_found at the gateway)", got)
	}
	if got := svc.ProviderName(); got == "" || got == "fake" {
		t.Fatalf("ProviderName() = %q, want the resolved provider", got)
	}
}
