package promptcache

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

type testTool struct {
	name    string
	desc    string
	schema  string
	execNil bool
}

func (t testTool) Name() string                               { return t.name }
func (t testTool) Description() string                        { return t.desc }
func (t testTool) Schema() json.RawMessage                    { return json.RawMessage(t.schema) }
func (t testTool) ExecutionMode() agentcore.ToolExecutionMode { return agentcore.ToolExecutionParallel }
func (t testTool) Execute(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return agentcore.AgentToolResult{}, nil
}

func tools() []agentcore.AgentTool {
	return []agentcore.AgentTool{
		testTool{name: "read", desc: "read a file", schema: `{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}}}`},
		testTool{name: "bash", desc: "run a command", schema: `{"type":"object","properties":{"cmd":{"type":"string"}}}`},
		testTool{name: "agent", desc: "spawn an agent", schema: `{"type":"object"}`},
	}
}

// TestCanonicalToolsAreOrderIndependent is the property the provider cache
// depends on: the same tool set registered in any order produces the same
// prefix digest and the same canonical name order.
func TestCanonicalToolsAreOrderIndependent(t *testing.T) {
	forward := Build("you are keel", tools())
	reversedTools := append([]agentcore.AgentTool{}, tools()...)
	for i, j := 0, len(reversedTools)-1; i < j; i, j = i+1, j-1 {
		reversedTools[i], reversedTools[j] = reversedTools[j], reversedTools[i]
	}
	reversed := Build("you are keel", reversedTools)

	if forward.PrefixDigest != reversed.PrefixDigest || forward.ToolsDigest != reversed.ToolsDigest {
		t.Fatalf("registration order changed the prefix: %s vs %s", forward.PrefixDigest, reversed.PrefixDigest)
	}
	want := []string{"agent", "bash", "read"}
	if strings.Join(forward.ToolNames, ",") != strings.Join(want, ",") {
		t.Fatalf("tool names = %v, want %v", forward.ToolNames, want)
	}
}

// TestSchemaMapOrderDoesNotChangeDigest: JSON object key order is not semantic,
// so a schema built from a map in a different iteration order must not
// invalidate the cache.
func TestSchemaMapOrderDoesNotChangeDigest(t *testing.T) {
	a := []agentcore.AgentTool{testTool{name: "t", desc: "d", schema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}}}`}}
	b := []agentcore.AgentTool{testTool{name: "t", desc: "d", schema: `{"properties":{"b":{"type":"number"},"a":{"type":"string"}},"type":"object"}`}}
	if Build("s", a).PrefixDigest != Build("s", b).PrefixDigest {
		t.Fatal("schema object key order changed the prefix digest")
	}
	if got := string(CanonicalSchema(json.RawMessage(`{"b":1,"a":2}`))); got != `{"a":2,"b":1}` {
		t.Fatalf("canonical schema = %s", got)
	}
}

// TestTrackerNamesInvalidationReasons pins the documented boundary: system,
// tool set/declaration and model changes invalidate; an identical plan does
// not.
func TestTrackerNamesInvalidationReasons(t *testing.T) {
	tracker := NewTracker()
	base := Build("you are keel", tools())
	if change := tracker.Observe("model-a", base); change.Invalidated || len(change.Reasons) != 0 {
		t.Fatalf("first observation = %+v, want no invalidation", change)
	}
	if change := tracker.Observe("model-a", base); change.Invalidated {
		t.Fatalf("identical plan invalidated: %+v", change)
	}
	if change := tracker.Observe("model-a", Build("you are keel!", tools())); !change.Invalidated ||
		!strings.Contains(strings.Join(change.Reasons, ";"), "system prompt") {
		t.Fatalf("system change = %+v", change)
	}

	added := append(tools(), testTool{name: "extra", desc: "x", schema: `{"type":"object"}`})
	if change := tracker.Observe("model-a", Build("you are keel!", added)); !change.Invalidated ||
		!strings.Contains(strings.Join(change.Reasons, ";"), "added extra") {
		t.Fatalf("tool addition = %+v", change)
	}

	changedDesc := append([]agentcore.AgentTool{}, added...)
	changedDesc[0] = testTool{name: "read", desc: "read a file differently", schema: `{"type":"object"}`}
	if change := tracker.Observe("model-a", Build("you are keel!", changedDesc)); !change.Invalidated ||
		!strings.Contains(strings.Join(change.Reasons, ";"), "tool declaration") {
		t.Fatalf("tool declaration change = %+v", change)
	}

	if change := tracker.Observe("model-b", Build("you are keel!", changedDesc)); !change.Invalidated ||
		!strings.Contains(strings.Join(change.Reasons, ";"), "model changed") {
		t.Fatalf("model change = %+v", change)
	}
}

// TestTrackerKeepsSessionsSeparate: one runner serves many sessions; a plan
// difference between sessions is a cache miss for the changed session, never a
// false invalidation of another.
func TestTrackerKeepsSessionsSeparate(t *testing.T) {
	tracker := NewTracker()
	a := Build("system a", tools())
	b := Build("system b", tools())
	if change := tracker.ObserveStream("session-a", "m", a); change.Invalidated {
		t.Fatalf("first plan for session-a invalidated: %+v", change)
	}
	if change := tracker.ObserveStream("session-b", "m", b); change.Invalidated {
		t.Fatalf("first plan for session-b invalidated by another session: %+v", change)
	}
	if change := tracker.ObserveStream("session-a", "m", a); change.Invalidated {
		t.Fatalf("stable session-a plan invalidated: %+v", change)
	}
	if change := tracker.ObserveStream("session-b", "m", b); change.Invalidated {
		t.Fatalf("stable session-b plan invalidated: %+v", change)
	}
}

// TestTrackerBoundsState: a long-lived runner must not grow one entry per
// session forever. Eviction is a cache miss, never a false invalidation.
func TestTrackerBoundsState(t *testing.T) {
	tracker := NewTracker()
	tracker.max = 4
	plan := Build("s", tools())
	for i := 0; i < 50; i++ {
		tracker.ObserveStream(fmt.Sprintf("session-%d", i), "m", plan)
	}
	if len(tracker.states) > 4 {
		t.Fatalf("tracker kept %d streams, want <= 4", len(tracker.states))
	}
}

// TestBuildBoundaryIsBeforeTheTranscript documents the cache boundary: the
// plan names system+tools, never a message.
func TestBuildBoundaryIsBeforeTheTranscript(t *testing.T) {
	plan := Build("s", tools())
	if Boundary != "system+tools" || plan.Boundary != Boundary {
		t.Fatalf("boundary = %q", plan.Boundary)
	}
	if plan.SystemDigest == "" || plan.ToolsDigest == "" || plan.PrefixDigest == "" {
		t.Fatalf("plan = %+v, want all digests", plan)
	}
}

// TestInvalidSchemaIsPreserved: a malformed schema is never silently replaced.
func TestInvalidSchemaIsPreserved(t *testing.T) {
	raw := json.RawMessage(`{"type":`)
	if got := CanonicalSchema(raw); string(got) != string(raw) {
		t.Fatalf("invalid schema changed: %q -> %q", raw, got)
	}
}
