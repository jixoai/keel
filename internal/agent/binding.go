package agent

// The execution binding seam (change keel-bot-identity-access, A4.2b3a).
//
// A session belongs to exactly one Bot, and an input may additionally name the
// Bot (standard mode) or TeamBot (team mode) whose environment answers it. The
// runner needs four things from that binding and nothing more:
//
//   - the system prompt it contributes (and a hash to freeze in the run
//     summary);
//   - the tool ids the Bot's Harness grants (an empty set grants nothing: the
//     Harness is a whitelist, not a default);
//   - a default model binding used only when the input named none (the Bot is
//     a default, never a lock);
//   - the team mission and roster in team mode, so the captain can delegate.
//
// The types here are plain data on purpose: the product layer resolves the
// documents and the runner consumes the result, so the kernel loop never learns
// what a Bot or a TeamBot is (the same split ModelResolver already follows).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/jixoai/keel/internal/kernel"
)

// SurfaceModelBinding is a Bot's default model: provider + wire model, plus
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
// "no binding".
type RunSurface struct {
	// Prompt is the system prompt the binding contributes ("" = none).
	Prompt string
	// PromptHash digests Prompt ("" when empty) and is frozen in the run
	// summary so a reader can tell which prompt answered the attempt.
	PromptHash string
	// ToolIDs are the Harness-whitelisted tool ids mapped to true. A non-nil
	// empty map means "the binding grants no tool"; nil still means "no
	// binding", which keeps the deployment registry for a manager/operator run
	// that has no Bot. A Bot's Harness is always a whitelist: an empty list
	// grants nothing.
	ToolIDs map[string]bool
	// TeamMode reports that the binding came from a team selection. A team run
	// always keeps the delegation primitive available (see Runner.toolsForRun).
	TeamMode bool
	// Mission is the TeamBot's objective in team mode ("" otherwise). It is
	// rendered into the prompt as the team context.
	Mission string
	// Members is the captain's roster in team mode (nil in standard mode). It is
	// rendered into the prompt so delegation can name a member instead of
	// guessing a botId.
	Members []RosterMember
	// AgentID, TeamID, TeamRevision and TeamHash are the identity facts frozen
	// into the run summary ("" / 0 when absent). The field names keep the
	// historical wire shape; the values are a Bot id and a TeamBot id
	// (A4.2b3a).
	AgentID      string
	TeamID       string
	TeamRevision uint64
	TeamHash     string
	// DefaultModel is the Bot's default model, used only when the input fact
	// named no model (the selection > Bot default > tenant default order).
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
// binding keys (botId/teamBotId) and the picker hints travel unchanged.
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
		// The input chose a model: the Bot default never overrides a choice.
		return selection
	}
	if providerID, _ := current["providerId"].(string); strings.TrimSpace(providerID) != "" {
		// A half binding is the resolver's refusal to report (provider without a
		// model is ambiguous); the Bot default must not paper over it.
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

// digestPrompt is the stable identity of a system prompt in the run summary:
// "sha256:<hex>" over the trimmed bytes, "" for an empty prompt. The digest is
// taken over the prompt that goes on the wire (the host's rendered form), so a
// change in a host-side section — MCP servers, plugin standard, skills index —
// changes the summary even when the agent's own text did not.
func digestPrompt(prompt string) string {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// RosterMember is one teammate as the captain sees it: the bot id the agent
// tool's spawn action takes, plus the label and role text that make the choice
// informed.
type RosterMember struct {
	BotID       string
	Label       string
	Description string
}

// renderTeamMission is the prompt section stating the team's objective. An
// empty mission renders nothing.
func renderTeamMission(mission string) string {
	mission = strings.TrimSpace(mission)
	if mission == "" {
		return ""
	}
	return "<team_mission>\n" + mission + "\n</team_mission>"
}

// renderTeamMembers is the prompt section listing the team's members. An empty
// roster renders nothing (a captain-only team has no one to delegate to).
func renderTeamMembers(members []RosterMember) string {
	if len(members) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<team_members>\n")
	for _, m := range members {
		label := strings.TrimSpace(m.Label)
		if label == "" {
			label = m.BotID
		}
		b.WriteString("- ")
		b.WriteString(m.BotID)
		b.WriteString(" — ")
		b.WriteString(label)
		if role := strings.TrimSpace(m.Description); role != "" {
			b.WriteString(": ")
			b.WriteString(role)
		}
		b.WriteString("\n")
	}
	b.WriteString("</team_members>")
	return b.String()
}
