package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

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
	// Properties is the resolved capability fact sheet. Nil means "unknown"
	// (the static/fake path), which is deliberately different from "declares no
	// capability": the runtime only refuses what the configuration explicitly
	// denies.
	Properties *ModelProperties
	// ImageCaps is the wire driver's image transport declaration (file_id / url /
	// base64 plus the byte ceilings). Outbound image resolution reads it to pick
	// the form; the zero value means "base64 only", the universally supported
	// shape.
	ImageCaps provider.ImageCaps
	// Endpoint is the API root the attempt calls (…/v1, …/v1beta). The outbound
	// image pipeline uploads provider files against it, so a handle lands in the
	// same account the request uses.
	Endpoint string
}

// ModelProperties is the capability fact sheet of an attempt's model, as plain
// data: the runtime must not depend on configuration types, and the caller (the
// product layer) owns what the fields mean. The attachment/media gate is not
// wired to this yet (see the change's tasks); the tool gate is.
type ModelProperties struct {
	SupportsToolCall         bool
	SupportsJSONSchemaOutput bool
	SupportsNativeWebSearch  bool
	ImageInput               bool
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

// runSelection returns the selection fact of this attempt: the newest fact that
// carried one. Selection is a per-input fact, not session state — the attempt
// that answers an input must use the model that input asked for, and a
// selection made while a run was live belongs to that input's own attempt.
//
// A resume.attempt may carry a selection too: an explicit resume is how a
// parent changes a stopped child's execution binding (for example another
// provider) without appending a second user message. The newest of the two
// facts wins, so a later user input still overrides the resume override.
func runSelection(events []store.Event) json.RawMessage {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Kind != store.EventUserMessage && ev.Kind != store.EventResumeAttempt {
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
	return r.activeModelFor(ctx, lease, events, runSelection(events))
}

// activeModelFor resolves the attempt's execution identity from an explicit
// selection fact. The caller supplies the fact because the execution binding may
// contribute a default model the input itself did not name
// (selection > agent default > tenant default); everything else is the historic
// path.
func (r *Runner) activeModelFor(ctx context.Context, lease kernel.Lease, _ []store.Event, selection json.RawMessage) (ActiveModel, error) {
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
	resolved, err := r.Models.Resolve(ctx, lease.Scope, selection)
	if err != nil {
		return ActiveModel{}, err
	}
	if resolved.ThinkingLevel == "" {
		resolved.ThinkingLevel = am.ThinkingLevel
	}
	// A resolver that answers without a transport would make the loop call the
	// boot/fake stream while the ledger records the resolved identity — exactly
	// the "the record says model X, the wire was something else" failure this
	// seam exists to prevent. It is refused, not papered over.
	if resolved.Stream == nil {
		return ActiveModel{}, fmt.Errorf("agent: model resolver returned no transport for %s/%s",
			resolved.ProviderID, resolved.Model)
	}
	if resolved.Model == "" {
		return ActiveModel{}, fmt.Errorf("agent: model resolver returned no model id (provider %q)", resolved.ProviderID)
	}
	return resolved, nil
}
