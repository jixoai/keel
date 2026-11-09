package agent

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// ActiveModel is the execution identity of one attempt: which provider and
// wire model the attempt calls, with which credential, thinking level and
// context budget. The product layer resolves it from the run's selection fact;
// the runtime only consumes it. The zero value means "nothing resolved".
type ActiveModel struct {
	ProviderID    string
	Model         string
	APIKey        string
	Stream        provider.StreamFn
	ThinkingLevel agentcore.ThinkingLevel
	// ContextWindow enables automatic compaction for the attempt when positive
	// (the model's own budget, which beats the tenant-wide default).
	ContextWindow int
	// RequestPatch is the provider-specific request body overlay the resolved
	// model carries (max output tokens, reasoning effort, ...). The runtime
	// forwards it as StreamConfig.Extra without interpreting it.
	RequestPatch map[string]any
}

// ModelResolver is the product seam between a run and its execution identity.
// selection is the raw JSON of the "selection" field of the newest input that
// carries one (nil when none does); the resolver decides what that means and
// fails closed — an unresolvable selection must return an error rather than a
// guess, because sending the wrong model is worse than failing the attempt.
//
// A runner without a resolver keeps its static fields (the fakestream path).
type ModelResolver interface {
	Resolve(ctx context.Context, scope kernel.Scope, selection json.RawMessage) (ActiveModel, error)
}

// runSelection returns the selection fact of this attempt: the newest input
// that carried one. Selection is a per-input fact, not session state — the
// attempt that answers an input must use the model that input asked for, and a
// selection made while a run was live belongs to that input's own attempt.
func runSelection(events []store.Event) json.RawMessage {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Kind != store.EventUserMessage {
			continue
		}
		var payload struct {
			Selection json.RawMessage `json:"selection"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			continue
		}
		if trimmed := bytes.TrimSpace(payload.Selection); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
			return trimmed
		}
	}
	return nil
}

// activeModel resolves the attempt's execution identity. It is called before
// any commit of the attempt (in particular before fillMissingToolResults):
// an unexecutable selection must not leave tool calls half-executed.
func (r *Runner) activeModel(ctx context.Context, lease kernel.Lease, events []store.Event) (ActiveModel, error) {
	am := ActiveModel{
		ProviderID:    r.ProviderName,
		Model:         r.Model,
		APIKey:        r.APIKey,
		Stream:        r.Stream,
		ThinkingLevel: r.ThinkingLevel,
	}
	if am.ThinkingLevel == "" {
		am.ThinkingLevel = agentcore.ThinkingLow
	}
	if r.Models == nil {
		return am, nil
	}
	resolved, err := r.Models.Resolve(ctx, lease.Scope, runSelection(events))
	if err != nil {
		return ActiveModel{}, err
	}
	if resolved.ThinkingLevel == "" {
		resolved.ThinkingLevel = am.ThinkingLevel
	}
	// A resolver that leaves the transport unset would produce a nil stream in
	// the loop (a panic at the first request); the static one is the honest
	// fallback only when the resolver answered without a transport at all.
	if resolved.Stream == nil {
		resolved.Stream = am.Stream
	}
	return resolved, nil
}
