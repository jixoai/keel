package toolsurface_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/tasklog"
	"github.com/jixoai/keel/internal/toolsurface"
)

// fakeTasks is a task-log backend whose attribution rules are scripted, so the
// mandatory-taskId gate can be exercised without a database.
type fakeTasks struct {
	defaultTask  string
	validIDs     map[string]bool
	attribute    []string
	attributeErr error
	invoked      []string
	lastArgs     json.RawMessage
	invokeErr    error
}

func (f *fakeTasks) Attribute(_ context.Context, scope harnessext.RunScope, taskID string) (string, error) {
	f.attribute = append(f.attribute, scope.SessionID.String()+":"+taskID)
	if f.attributeErr != nil {
		return "", f.attributeErr
	}
	if taskID == "" {
		return f.defaultTask, nil
	}
	if f.validIDs != nil && !f.validIDs[taskID] {
		return "", tasklog.ErrTaskNotFound
	}
	return taskID, nil
}

func (f *fakeTasks) Invoke(_ context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	if f.invokeErr != nil {
		return agentcore.AgentToolResult{}, f.invokeErr
	}
	f.invoked = append(f.invoked, action)
	f.lastArgs = args
	document := map[string]any{"action": action}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(`{"action":"` + action + `"}`)},
		StructuredContent: document,
	}, nil
}

// recordingCreation records the arguments the gate handed it.
type recordingCreation struct {
	calls    int
	lastArgs json.RawMessage
}

func (r *recordingCreation) Invoke(_ context.Context, _ string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	r.calls++
	r.lastArgs = append(json.RawMessage(nil), args...)
	return agentcore.AgentToolResult{StructuredContent: map[string]any{"ok": true}}, nil
}

func taskScope() (context.Context, harnessext.RunScope) {
	scope := harnessext.RunScope{TenantID: uuid.New(), SessionID: uuid.New(), RunID: "run-1"}
	return harnessext.WithRunScope(context.Background(), scope), scope
}

func TestTaskGateStampsTheAmbientCard(t *testing.T) {
	backend := &fakeTasks{defaultTask: "task-initial"}
	creation := &recordingCreation{}
	registry := builtinRegistry(t, toolsurface.Backends{Tasks: backend, Creation: creation})
	ctx, _ := taskScope()

	result, err := registry[toolsurface.Creation].Execute(ctx, "call-1", json.RawMessage(`{"action":"catalog"}`), nil)
	if err != nil || result.IsError != nil && *result.IsError {
		t.Fatalf("creation execute = %+v, %v", result, err)
	}
	if creation.calls != 1 {
		t.Fatalf("creation calls = %d, want 1", creation.calls)
	}
	var stamped map[string]any
	if err := json.Unmarshal(creation.lastArgs, &stamped); err != nil {
		t.Fatalf("stamped args: %v (%s)", err, creation.lastArgs)
	}
	if stamped["taskId"] != "task-initial" {
		t.Fatalf("stamped taskId = %v (%s)", stamped["taskId"], creation.lastArgs)
	}
	if len(backend.attribute) != 1 || !strings.HasSuffix(backend.attribute[0], ":") {
		t.Fatalf("attribute calls = %v, want one with an empty taskId", backend.attribute)
	}
}

func TestTaskGateRejectsMissingOrForeignCards(t *testing.T) {
	backend := &fakeTasks{defaultTask: "task-initial", validIDs: map[string]bool{"task-known": true}}
	creation := &recordingCreation{}
	registry := builtinRegistry(t, toolsurface.Backends{Tasks: backend, Creation: creation})
	ctx, _ := taskScope()

	// A card that does not exist in the session is refused before the tool runs,
	// and the refusal is a typed result the model can read (the runner commits
	// it as the call's tool.result, so the log records the rejected fact).
	result, err := registry[toolsurface.Creation].Execute(ctx, "call-2", json.RawMessage(`{"action":"catalog","taskId":"task-ghost"}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	problem := errorDocument(t, result)
	if problem["code"] != toolsurface.CodeTaskNotFound {
		t.Fatalf("foreign card code = %v", problem)
	}
	if creation.calls != 0 {
		t.Fatalf("the tool ran despite a foreign taskId (calls = %d)", creation.calls)
	}

	// No card can be resolved at all: typed task_id_required, still no run.
	backend.attributeErr = tasklog.ErrTaskRequired
	result, err = registry[toolsurface.Creation].Execute(ctx, "call-3", json.RawMessage(`{"action":"catalog"}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	problem = errorDocument(t, result)
	if problem["code"] != toolsurface.CodeTaskRequired {
		t.Fatalf("unattributable call code = %v", problem)
	}
	if creation.calls != 0 {
		t.Fatalf("the tool ran without a card (calls = %d)", creation.calls)
	}
}

func TestTaskGatePassesAValidTaskIDThrough(t *testing.T) {
	backend := &fakeTasks{defaultTask: "task-initial", validIDs: map[string]bool{"task-known": true}}
	creation := &recordingCreation{}
	registry := builtinRegistry(t, toolsurface.Backends{Tasks: backend, Creation: creation})
	ctx, _ := taskScope()

	if _, err := registry[toolsurface.Creation].Execute(ctx, "call-4", json.RawMessage(`{"action":"catalog","taskId":"task-known"}`), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var args map[string]any
	if err := json.Unmarshal(creation.lastArgs, &args); err != nil {
		t.Fatal(err)
	}
	if args["taskId"] != "task-known" {
		t.Fatalf("valid taskId was rewritten: %s", creation.lastArgs)
	}
	if len(backend.attribute) != 1 || !strings.HasSuffix(backend.attribute[0], ":task-known") {
		t.Fatalf("attribute calls = %v", backend.attribute)
	}
}

func TestTaskGateLeavesNonRunCallsAlone(t *testing.T) {
	backend := &fakeTasks{defaultTask: "task-initial"}
	creation := &recordingCreation{}
	registry := builtinRegistry(t, toolsurface.Backends{Tasks: backend, Creation: creation})

	// The plugin UI bridge executes over the same registry with no ambient run.
	if _, err := registry[toolsurface.Creation].Execute(context.Background(), "bridge-1", json.RawMessage(`{"action":"catalog"}`), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(backend.attribute) != 0 {
		t.Fatalf("a non-run call was attributed: %v", backend.attribute)
	}
	if creation.calls != 1 {
		t.Fatalf("bridge call did not reach the tool (calls = %d)", creation.calls)
	}
	var args map[string]any
	if err := json.Unmarshal(creation.lastArgs, &args); err != nil {
		t.Fatal(err)
	}
	if _, ok := args["taskId"]; ok {
		t.Fatalf("a non-run call was stamped: %s", creation.lastArgs)
	}
}

func TestTasksToolActionsRouteThroughTheBackendAndStayUntargeted(t *testing.T) {
	backend := &fakeTasks{defaultTask: "task-initial"}
	registry := builtinRegistry(t, toolsurface.Backends{Tasks: backend})
	ctx, _ := taskScope()

	// The tasks tool's own taskId names the action's target; the gate must not
	// confuse it with the caller's card or refuse a foreign-session target.
	result, err := registry[toolsurface.Tasks].Execute(ctx, "call-5", json.RawMessage(`{"action":"show","taskId":"child-in-other-session"}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.IsError != nil && *result.IsError {
		t.Fatalf("tasks.show = %+v", result.StructuredContent)
	}
	if len(backend.invoked) != 1 || backend.invoked[0] != "show" {
		t.Fatalf("invoked actions = %v", backend.invoked)
	}
	var args map[string]any
	if err := json.Unmarshal(backend.lastArgs, &args); err != nil {
		t.Fatal(err)
	}
	if args["taskId"] != "child-in-other-session" {
		t.Fatalf("tasks tool args changed: %s", backend.lastArgs)
	}
	// The ambient card was still resolved (attribution), just not injected.
	if len(backend.attribute) != 1 || !strings.HasSuffix(backend.attribute[0], ":") {
		t.Fatalf("attribute calls = %v", backend.attribute)
	}
}

func TestTaskGateIsInvisibleToTheManifestHash(t *testing.T) {
	plain, err := toolsurface.ManifestHash(toolsurface.BuiltinTools(nil, toolsurface.Backends{}))
	if err != nil {
		t.Fatal(err)
	}
	gated, err := toolsurface.ManifestHash(toolsurface.BuiltinTools(nil, toolsurface.Backends{
		Tasks: &fakeTasks{defaultTask: "task-initial"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if plain != gated {
		t.Fatalf("wiring the task log changed the model-visible manifest: %s vs %s", plain, gated)
	}
}

func TestTaskGateDisabledPluginIsTyped(t *testing.T) {
	// A disabled plugin's Invoke answers ErrPluginState (what
	// Manager.InvokeToolResult returns): the tasks tool maps it to
	// plugin_disabled, which is the plugin-boundary contract.
	backend := &fakeTasks{defaultTask: "task-initial", invokeErr: harnessext.ErrPluginState}
	registry := builtinRegistry(t, toolsurface.Backends{Tasks: backend})
	ctx, _ := taskScope()
	result, err := registry[toolsurface.Tasks].Execute(ctx, "call-6", json.RawMessage(`{"action":"list"}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	problem := errorDocument(t, result)
	if problem["code"] != toolsurface.CodePluginDisabled {
		t.Fatalf("disabled tasks plugin code = %v", problem)
	}
}
