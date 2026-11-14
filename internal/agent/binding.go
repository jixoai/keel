package agent

// The execution binding seam (change keel-agent-team-v1, RUNTIME-PLAN §b/c).
//
// A selection fact may name the Agent (standard mode) or AgentTeam (team mode)
// that answers an input, besides the model identity. The runner needs three
// things from that binding and nothing more:
//
//   - the system prompt it contributes (and a hash to freeze in the run
//     summary);
//   - the tool ids the agent grants (nil = "no grants declared", which keeps the
//     whole registry, the documented default);
//   - a default model binding used only when the input named none (the agent is
//     a default, never a lock).
//
// The types here are plain data on purpose: the product layer resolves the
// documents and the runner consumes the result, so the kernel loop never learns
// what an Agent or a Team is (the same split ModelResolver already follows).

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jixoai/keel/internal/kernel"
)

// SurfaceModelBinding is an agent's default model: provider + wire model, plus
// the provider-specific request overlay. It carries no credential.
type SurfaceModelBinding struct {
	ProviderID string
	ModelID    string
	Tuning     map[string]any
}

// Complete reports whether the binding names both halves.
func (b *SurfaceModelBinding) Complete() bool {
	return b != nil && strings.TrimSpace(b.ProviderID) != "" && strings.TrimSpace(b.ModelID) != ""
}

// RunSurface is one attempt's resolved execution binding. The zero value means
// "no binding": the run keeps the deployment prompt and the whole registry.
type RunSurface struct {
	// Prompt is the system prompt the binding contributes ("" = none).
	Prompt string
	// PromptHash digests Prompt ("" when empty) and is frozen in the run
	// summary so a reader can tell which prompt answered the attempt.
	PromptHash string
	// ToolIDs are the granted tool ids mapped to their per-agent switch. Nil
	// means the binding declares no grants at all: keep every tool.
	ToolIDs map[string]bool
	// TeamMode reports that the binding came from a team selection. A team run
	// always keeps the delegation primitive available (see Runner.toolsForRun).
	TeamMode bool
	// AgentID, TeamID, TeamRevision and TeamHash are the identity facts frozen
	// into the run summary ("" / 0 when absent).
	AgentID      string
	TeamID       string
	TeamRevision uint64
	TeamHash     string
	// DefaultModel is the agent's default model, used only when the input fact
	// named no model (the selection > agent default > tenant default order).
	DefaultModel *SurfaceModelBinding
}

// Bound reports whether the surface names an execution identity.
func (s RunSurface) Bound() bool { return s.AgentID != "" || s.TeamID != "" }

// BindingResolver is the product seam between a run and its execution binding.
// selection is the raw "selection" field of the newest input that carried one
// (nil when none does), exactly what ModelResolver receives. A resolver that
// cannot resolve a binding the input named fails closed: a wrong prompt or a
// too-wide tool surface is worse than a failed attempt.
//
// A runner without a resolver keeps the deployment prompt and the whole
// registry (the pre-binding behaviour).
type BindingResolver interface {
	ResolveBinding(ctx context.Context, scope kernel.Scope, selection json.RawMessage) (RunSurface, error)
}

// resolveSurface returns this attempt's binding (the zero surface when no
// resolver is wired).
func (r *Runner) resolveSurface(ctx context.Context, lease kernel.Lease, selection json.RawMessage) (RunSurface, error) {
	if r.Bindings == nil {
		return RunSurface{}, nil
	}
	return r.Bindings.ResolveBinding(ctx, lease.Scope, selection)
}

// MergeSelectionDefaultModel merges a binding's default model into a selection
// fact that named no model of its own. Field presence is what the resolver
// reads, so the merge happens on the decoded value and is re-encoded; the
// binding keys (agentId/teamId) and the picker hints travel unchanged.
//
// It returns the original fact when nothing needs merging (nil stays nil: "no
// choice made" is a distinct fact from "an empty object").
func MergeSelectionDefaultModel(selection json.RawMessage, binding *SurfaceModelBinding) json.RawMessage {
	if !binding.Complete() {
		return selection
	}
	var current map[string]any
	if len(strings.TrimSpace(string(selection))) > 0 {
		if err := json.Unmarshal(selection, &current); err != nil || current == nil {
			current = nil
		}
	}
	if current == nil {
		current = map[string]any{}
	}
	if modelID, _ := current["modelId"].(string); strings.TrimSpace(modelID) != "" {
		// The input chose a model: the agent default never overrides a choice.
		return selection
	}
	if providerID, _ := current["providerId"].(string); strings.TrimSpace(providerID) != "" {
		// A half binding is the resolver's refusal to report (provider without a
		// model is ambiguous); the agent default must not paper over it.
		return selection
	}
	current["providerId"] = binding.ProviderID
	current["modelId"] = binding.ModelID
	merged, err := json.Marshal(current)
	if err != nil {
		return selection
	}
	return merged
}
