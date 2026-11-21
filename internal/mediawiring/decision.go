package mediawiring

import (
	"encoding/json"
	"strings"

	"github.com/jixoai/keel/internal/mediaadapter"
	"github.com/jixoai/keel/internal/modelcfg"
)

// validateDecision walks every executable decision model in the resolution and
// runs the adapter's local checks: the provider must name a System One adapter,
// the adapter/model pair must publish a capability descriptor, and a saved
// binding must pass Preflight. It performs no provider call and never invents
// an answer. A provider that only overrides one model's family (the demo or an
// aggregator) is checked for that model, not for its provider type.
func validateDecision(resolved resolved) []modelcfg.Issue {
	var issues []modelcfg.Issue
	for _, provider := range resolved.resolution.Providers {
		if provider.API.Type == modelcfg.APITypeDemo {
			continue
		}
		for _, model := range provider.Models {
			if model.ProviderType != FamilyDecision || !model.Enabled {
				continue
			}
			issues = append(issues, validateDecisionModel(provider, model)...)
		}
	}
	return issues
}

func validateDecisionModel(provider modelcfg.ProviderView, model modelcfg.ModelView) []modelcfg.Issue {
	config := provider.Decision
	if config == nil || strings.TrimSpace(config.Adapter) == "" {
		return []modelcfg.Issue{issue(provider.ProviderID, "decision.adapter",
			"decision provider must declare an adapter (typesafe|cloudflare|openrouter|openjev)")}
	}
	client, err := mediaadapter.NewDecisionClient(mediaadapter.DecisionConfig{
		Adapter:         config.Adapter,
		BaseURL:         provider.API.BaseURL,
		Model:           model.ModelID,
		ProtocolVersion: config.ProtocolVersion,
		// Preflight is local. A missing credential is a runtime/settings fact the
		// provider views already report; it must not turn a shape check into a
		// save-blocking error, and no request is made here anyway.
		AllowAnonymous: true,
	})
	if err != nil {
		return []modelcfg.Issue{issue(provider.ProviderID, "decision", err.Error())}
	}
	binding := config.Binding
	if binding == nil {
		return nil
	}
	var issues []modelcfg.Issue
	add := func(field, message string) {
		issues = append(issues, issue(provider.ProviderID, "decision.binding."+field, message))
	}

	state := json.RawMessage(binding.State)
	if len(strings.TrimSpace(string(state))) == 0 || string(state) == "null" {
		add("state", "state is required for a saved decision binding")
	}
	var questions map[string]mediaadapter.DecisionQuestion
	if len(strings.TrimSpace(string(binding.Questions))) == 0 || string(binding.Questions) == "null" {
		add("questions", "at least one question is required")
	} else if err := json.Unmarshal(binding.Questions, &questions); err != nil {
		add("questions", "questions must be a JSON object keyed by question id")
	} else if len(questions) == 0 {
		add("questions", "at least one question is required")
	}

	if len(questions) > 0 {
		policy := mediaadapter.DecisionPolicy{
			Mode:       binding.Mode,
			FailPolicy: binding.FailPolicy,
			Threshold:  binding.Threshold,
		}
		for _, field := range client.CheckPolicy(policy) {
			add("policy."+field.Field, field.Message)
		}
		for _, field := range client.Preflight(state, questions) {
			add(field.Field, field.Message)
		}
	}
	return issues
}
