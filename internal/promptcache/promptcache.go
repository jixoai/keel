// Package promptcache owns the stable-prefix policy of keel.prompt-cache: the
// system prompt plus the tool declarations are the provider's prefix cache
// boundary, and that prefix must be byte-stable across turns.
//
// The boundary is deliberately drawn *before* the transcript. Providers cache a
// request prefix (system + tools + messages); a breakpoint at the last tool
// caches the system/tools prefix and nothing else. That choice is what makes
// memory hygiene safe: pruning or offloading a message changes the request
// after the boundary, so the cached prefix stays valid. A breakpoint placed
// inside the transcript would be invalidated by every pruning/compaction pass.
//
// What invalidates the prefix (and therefore the cache):
//
//   - the system prompt text changes in any byte;
//   - the tool set changes (a tool is added or removed);
//   - a tool's name, description or JSON schema changes;
//   - the model changes (a provider cache is per model);
//   - the provider changes (a cache is per provider/account).
//
// What does NOT invalidate it:
//
//   - tool registration order, because CanonicalTools sorts by name;
//   - map key order inside a JSON schema, because CanonicalSchema re-encodes
//     objects with sorted keys;
//   - message-history changes after the boundary (pruning, image offload,
//     compaction, steering).
//
// Tracker compares consecutive plans so a run can report which of the first
// five happened. It is intentionally per-process and bounded: it holds one
// previous plan per tracker, never a history.
package promptcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/jixoai/keel/internal/agentcore"
)

// Boundary names where the provider cache breakpoint sits. It is part of the
// plan so a reader cannot mistake the transcript for a cached prefix.
const Boundary = "system+tools"

// Plan is the canonical prefix of one request.
type Plan struct {
	// SystemDigest is the sha256 of the system prompt bytes.
	SystemDigest string
	// ToolsDigest is the sha256 of the canonical tool declarations (name,
	// description, canonical schema), independent of registration order.
	ToolsDigest string
	// PrefixDigest identifies the whole prefix (system + tools).
	PrefixDigest string
	// ToolNames lists the canonical (sorted) tool names, for diagnostics.
	ToolNames []string
	// Boundary is where the cache breakpoint belongs (see the package doc).
	Boundary string
}

// Change is the result of comparing one plan with the previous one.
type Change struct {
	// Invalidated is true when the cached prefix cannot be reused.
	Invalidated bool
	// Reasons names each fact that changed; empty when the prefix is stable.
	Reasons []string
	// Digest is the current prefix digest.
	Digest string
}

// Build computes the canonical plan for a system prompt and tool set. It does
// not require canonical tools: it canonicalizes internally, so a caller can
// plan without rewriting the request.
func Build(systemPrompt string, tools []agentcore.AgentTool) Plan {
	canonical := CanonicalTools(tools)
	return plan(systemPrompt, canonical)
}

func plan(systemPrompt string, canonical []agentcore.AgentTool) Plan {
	systemSum := sha256.Sum256([]byte(systemPrompt))
	var toolsHash bytes.Buffer
	names := make([]string, 0, len(canonical))
	for _, tool := range canonical {
		name := tool.Name()
		names = append(names, name)
		toolsHash.Write([]byte(name))
		toolsHash.Write([]byte{0})
		toolsHash.Write([]byte(tool.Description()))
		toolsHash.Write([]byte{0})
		toolsHash.Write(CanonicalSchema(tool.Schema()))
		toolsHash.Write([]byte{0})
	}
	toolsSum := sha256.Sum256(toolsHash.Bytes())
	prefixSum := sha256.Sum256(append(systemSum[:], toolsSum[:]...))
	return Plan{
		SystemDigest: hex.EncodeToString(systemSum[:]),
		ToolsDigest:  hex.EncodeToString(toolsSum[:]),
		PrefixDigest: hex.EncodeToString(prefixSum[:]),
		ToolNames:    names,
		Boundary:     Boundary,
	}
}

// CanonicalTools returns the tool set in a deterministic order (by name), with
// each schema re-encoded canonically. The returned tools are wrappers: name,
// description and execution behavior delegate to the original; Schema returns
// the canonical bytes. A tool whose schema is already canonical is still
// wrapped, which is cheap and keeps the contract simple.
func CanonicalTools(tools []agentcore.AgentTool) []agentcore.AgentTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]agentcore.AgentTool, len(tools))
	for i, tool := range tools {
		out[i] = canonicalTool{inner: tool, schema: CanonicalSchema(tool.Schema())}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// CanonicalSchema re-encodes a JSON object with sorted keys (encoding/json
// sorts map keys) and preserved number lexemes. Invalid JSON is returned
// unchanged: a malformed schema is the producer's bug, and silently replacing
// it with "{}" would hide it.
func CanonicalSchema(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return raw
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return raw
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return encoded
}

// canonicalTool is an AgentTool view whose Schema is canonical.
type canonicalTool struct {
	inner  agentcore.AgentTool
	schema json.RawMessage
}

func (t canonicalTool) Name() string            { return t.inner.Name() }
func (t canonicalTool) Description() string     { return t.inner.Description() }
func (t canonicalTool) Schema() json.RawMessage { return t.schema }
func (t canonicalTool) ExecutionMode() agentcore.ToolExecutionMode {
	return t.inner.ExecutionMode()
}
func (t canonicalTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return t.inner.Execute(ctx, id, args, onUpdate)
}

// DefaultTrackerStreams bounds the per-tracker state. The runner keeps one
// tracker for every session it serves; a long-lived process must not grow an
// unbounded map, and an evicted session simply reports "no previous plan" (a
// cache miss, never a false invalidation).
const DefaultTrackerStreams = 128

// trackerState is one stream's previous plan.
type trackerState struct {
	plan  Plan
	model string
}

// Tracker remembers the previous plan per stream so a run can report
// invalidation. It is safe for concurrent use. A stream is a session id (or any
// caller-chosen key): different sessions served by one runner must not be
// compared with each other.
type Tracker struct {
	mu     sync.Mutex
	states map[string]trackerState
	max    int
}

// NewTracker returns an empty tracker. Empty means "no previous plan": the
// first observation of a stream reports Invalidated=false (there was no cache
// to reuse).
func NewTracker() *Tracker {
	return &Tracker{states: make(map[string]trackerState), max: DefaultTrackerStreams}
}

// Observe compares plan with the previous observation of one single stream. It
// keeps the pre-multi-stream behavior for callers that track one session: a
// model change is reported as an invalidation.
func (t *Tracker) Observe(model string, next Plan) Change {
	return t.observe("", model, next)
}

// ObserveStream compares plan with the previous observation of stream and
// records it. The runner keys stream by session id and passes the model
// separately, so a model switch is reported as a model change rather than a
// different-stream miss.
func (t *Tracker) ObserveStream(stream, model string, next Plan) Change {
	return t.observe(stream, model, next)
}

func (t *Tracker) observe(stream, model string, next Plan) Change {
	if t == nil {
		return Change{Digest: next.PrefixDigest}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	change := Change{Digest: next.PrefixDigest}
	prev, ok := t.states[stream]
	if !ok {
		if t.max > 0 && len(t.states) >= t.max {
			// Bounded eviction: drop one arbitrary stream. Its next request is a
			// cache miss, which is safe; keeping an unbounded map is not.
			for key := range t.states {
				delete(t.states, key)
				break
			}
		}
		t.states[stream] = trackerState{plan: next, model: model}
		return change
	}
	change.Invalidated = true
	if prev.model != model {
		change.Reasons = append(change.Reasons, fmt.Sprintf("model changed (%s → %s)", prev.model, model))
	}
	if prev.plan.SystemDigest != next.SystemDigest {
		change.Reasons = append(change.Reasons, "system prompt changed")
	}
	if prev.plan.ToolsDigest != next.ToolsDigest {
		change.Reasons = append(change.Reasons, describeToolChanges(prev.plan, next))
	}
	if len(change.Reasons) == 0 {
		// A digest mismatch without a named cause means the canonical inputs
		// changed in a way this version does not classify; report the fact
		// rather than claim stability.
		if prev.plan.PrefixDigest != next.PrefixDigest {
			change.Reasons = append(change.Reasons, "prefix digest changed")
		} else {
			change.Invalidated = false
		}
	}
	t.states[stream] = trackerState{plan: next, model: model}
	return change
}

// describeToolChanges names the tool-set difference behind a tools digest
// change. It never returns an empty string for a different digest.
func describeToolChanges(prev, next Plan) string {
	prevNames := make(map[string]struct{}, len(prev.ToolNames))
	for _, name := range prev.ToolNames {
		prevNames[name] = struct{}{}
	}
	nextNames := make(map[string]struct{}, len(next.ToolNames))
	for _, name := range next.ToolNames {
		nextNames[name] = struct{}{}
	}
	var added, removed []string
	for _, name := range next.ToolNames {
		if _, ok := prevNames[name]; !ok {
			added = append(added, name)
		}
	}
	for _, name := range prev.ToolNames {
		if _, ok := nextNames[name]; !ok {
			removed = append(removed, name)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return "tool declaration changed (name/description/schema)"
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	return "tool set changed (" + strings.Join(parts, "; ") + ")"
}
