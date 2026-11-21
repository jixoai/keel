package mediawiring

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
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

// failingDecisionExecutor is the fail-closed stand-in for a configured decision
// provider whose credential could not be resolved. It never touches the wire.
type failingDecisionExecutor struct {
	reason string
}

func (e *failingDecisionExecutor) Decide(context.Context, mediaadapter.DecisionRequest) (*mediaadapter.DecisionOutcome, error) {
	return nil, missingCredential(FamilyDecision, "decide", e.reason)
}

// buildDecisionTool assembles the minimal runtime consumer for the decision
// family: the tenant's provider/model/credential and the saved binding's
// policy/questions become decision_ask. The model may supply state/questions
// per call, but never mode or failPolicy. A missing adapter or unusable wire
// configuration omits the tool and reports an issue; a missing credential keeps
// the tool and fails with the typed missing_credentials error without a call.
func buildDecisionTool(resolved resolved, opts Options) (agentcore.AgentTool, FamilyState, []modelcfg.Issue) {
	state := FamilyState{Family: FamilyDecision}
	candidate, ok := chooseModel(resolved, FamilyDecision)
	if !ok {
		return nil, state, nil
	}
	state.Configured = true
	state.ProviderID, state.ModelID = candidate.provider.ProviderID, candidate.model.ModelID
	config := candidate.provider.Decision
	if config == nil || strings.TrimSpace(config.Adapter) == "" {
		state.Reason = "decision provider must declare an adapter"
		return nil, state, []modelcfg.Issue{issue(candidate.provider.ProviderID, "decision.adapter",
			"decision provider must declare an adapter (typesafe|cloudflare|openrouter|openjev)")}
	}

	key, reason := credential(opts, candidate.provider)
	var executor mediaadapter.DecisionExecutor
	if reason != "" {
		state.Reason = reason
		executor = &failingDecisionExecutor{reason: reason}
	} else {
		client, err := mediaadapter.NewDecisionClient(mediaadapter.DecisionConfig{
			Adapter:         config.Adapter,
			BaseURL:         candidate.provider.API.BaseURL,
			APIKey:          key,
			Model:           candidate.model.ModelID,
			ProtocolVersion: config.ProtocolVersion,
			Client:          httpClient(opts),
			// A credential-free provider (local OpenJev) is the only case where
			// an empty key is a complete declaration.
			AllowAnonymous: candidate.provider.Access.CredentialFree(),
		})
		if err != nil {
			state.Reason = err.Error()
			return nil, state, []modelcfg.Issue{issue(candidate.provider.ProviderID, "decision", err.Error())}
		}
		executor = client
	}

	policy := mediaadapter.DefaultDecisionPolicy()
	var bindingState json.RawMessage
	var bindingQuestions map[string]mediaadapter.DecisionQuestion
	if binding := config.Binding; binding != nil {
		policy = mediaadapter.DecisionPolicy{
			Mode:       binding.Mode,
			FailPolicy: binding.FailPolicy,
			Threshold:  binding.Threshold,
		}
		if rawJSONPresent(binding.State) {
			if !json.Valid(binding.State) {
				state.Reason = "decision binding state is not valid JSON"
				return nil, state, []modelcfg.Issue{issue(candidate.provider.ProviderID, "decision.binding.state",
					"state must be valid JSON")}
			}
			bindingState = binding.State
		}
		if rawJSONPresent(binding.Questions) {
			if err := json.Unmarshal(binding.Questions, &bindingQuestions); err != nil || len(bindingQuestions) == 0 {
				state.Reason = "decision binding questions are not a non-empty object"
				return nil, state, []modelcfg.Issue{issue(candidate.provider.ProviderID, "decision.binding.questions",
					"questions must be a non-empty JSON object keyed by question id")}
			}
		}
	}

	state.Wired = true
	if reason == "" {
		state.Reason = ""
	}
	return mediaadapter.NewDecisionTool(executor, policy, bindingState, bindingQuestions), state, nil
}

// rawJSONPresent reports whether a saved binding field carries a value other
// than absent/null.
func rawJSONPresent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}
