package toolsurface_test

// The `agent` tool's typed sub-agent refusals (change keel-collab-subagent):
// depth and width are their own codes, not backend_error, so the model can act
// on them (stop, split, or ask the parent to widen the deployment setting).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/toolsurface"
)

func TestAgentSpawnDepthRefusalsAreTyped(t *testing.T) {
	tenant, parent := uuid.New(), uuid.New()
	backend := &fakeAgents{err: &harnessext.SubagentDepthError{
		ParentSessionID: parent, Depth: 4, MaxDepth: 3,
	}}
	registry := builtinRegistry(t, toolsurface.Backends{Agents: backend})
	ctx := harnessext.WithRunScope(context.Background(), harnessext.RunScope{
		TenantID: tenant, SessionID: parent, RunID: "run-depth",
	})
	result, err := registry[toolsurface.Agent].Execute(ctx, "call-1",
		json.RawMessage(`{"action":"spawn","task":"too deep","maxDepth":2}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	problem := errorDocument(t, result)
	if problem["code"] != toolsurface.CodeSubagentDepthExceeded {
		t.Fatalf("depth refusal code = %v", problem["code"])
	}
	if !numberEqual(problem["maxDepth"], 3) || !numberEqual(problem["depth"], 4) {
		t.Fatalf("depth refusal details = %+v", problem)
	}
	// The narrowing request reached the backend unchanged.
	var args struct {
		MaxDepth *int `json:"maxDepth"`
	}
	if err := json.Unmarshal(backend.args, &args); err != nil || args.MaxDepth == nil || *args.MaxDepth != 2 {
		t.Fatalf("spawn args = %s err=%v", backend.args, err)
	}

	backend.err = &harnessext.SubagentChildrenError{
		ParentSessionID: parent, Active: 8, Limit: 8,
	}
	result, err = registry[toolsurface.Agent].Execute(ctx, "call-2",
		json.RawMessage(`{"action":"spawn","task":"fan-out"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	problem = errorDocument(t, result)
	if problem["code"] != toolsurface.CodeSubagentChildrenExceeded {
		t.Fatalf("width refusal code = %v", problem["code"])
	}
	if !numberEqual(problem["maxChildren"], 8) {
		t.Fatalf("width refusal details = %+v", problem)
	}

	backend.err = &harnessext.SubagentDepthPolicyError{Requested: 4, Effective: 3}
	result, err = registry[toolsurface.Agent].Execute(ctx, "call-3",
		json.RawMessage(`{"action":"spawn","task":"wider","maxDepth":4}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if problem = errorDocument(t, result); problem["code"] != toolsurface.CodeSubagentMaxDepthWidened {
		t.Fatalf("widening code = %v", problem["code"])
	}

	backend.err = collab.ErrPermissionDenied
	result, err = registry[toolsurface.Agent].Execute(ctx, "call-4",
		json.RawMessage(`{"action":"spawn","task":"denied"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if problem = errorDocument(t, result); problem["code"] != toolsurface.CodePermissionDenied {
		t.Fatalf("permission code = %v", problem["code"])
	}
}

// numberEqual compares an in-process structured value (int) or a JSON-decoded
// one (float64) against an integer.
func numberEqual(value any, want int) bool {
	switch typed := value.(type) {
	case int:
		return typed == want
	case int64:
		return typed == int64(want)
	case float64:
		return typed == float64(want)
	default:
		return false
	}
}
