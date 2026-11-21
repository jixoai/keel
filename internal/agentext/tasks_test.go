package agentext

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/tasklog"
)

// fakeTaskAPI scripts the keel.tasks plugin's backend. Only the methods a test
// exercises are set; every other method answers a zero value.
type fakeTaskAPI struct {
	create func(ctx context.Context, scope kernel.Scope, in tasklog.CreateInput, callID string) (tasklog.Card, error)
	update func(ctx context.Context, scope kernel.Scope, taskID uuid.UUID, in tasklog.UpdateInput, callID string) (tasklog.Card, error)
	show   func(ctx context.Context, scope kernel.Scope, taskID uuid.UUID) (tasklog.View, error)
	link   func(ctx context.Context, parent kernel.Scope, child uuid.UUID) (tasklog.Card, error)
}

func (f *fakeTaskAPI) EnsureCard(ctx context.Context, scope kernel.Scope) (tasklog.Card, error) {
	return tasklog.Card{}, nil
}
func (f *fakeTaskAPI) Resolve(ctx context.Context, scope kernel.Scope, taskID string) (tasklog.Card, error) {
	return tasklog.Card{}, nil
}
func (f *fakeTaskAPI) Create(ctx context.Context, scope kernel.Scope, in tasklog.CreateInput, callID string) (tasklog.Card, error) {
	if f.create == nil {
		return tasklog.Card{}, nil
	}
	return f.create(ctx, scope, in, callID)
}
func (f *fakeTaskAPI) Update(ctx context.Context, scope kernel.Scope, taskID uuid.UUID, in tasklog.UpdateInput, callID string) (tasklog.Card, error) {
	if f.update == nil {
		return tasklog.Card{}, nil
	}
	return f.update(ctx, scope, taskID, in, callID)
}
func (f *fakeTaskAPI) Attach(context.Context, kernel.Scope, uuid.UUID, string, string, string) (tasklog.Event, error) {
	return tasklog.Event{}, nil
}
func (f *fakeTaskAPI) Detach(context.Context, kernel.Scope, uuid.UUID, string, string, string) (tasklog.Event, error) {
	return tasklog.Event{}, nil
}
func (f *fakeTaskAPI) Close(context.Context, kernel.Scope, uuid.UUID, string, string) (tasklog.Card, error) {
	return tasklog.Card{}, nil
}
func (f *fakeTaskAPI) Note(context.Context, kernel.Scope, uuid.UUID, string, string) (tasklog.Event, error) {
	return tasklog.Event{}, nil
}
func (f *fakeTaskAPI) List(context.Context, kernel.Scope, tasklog.ListFilter) ([]tasklog.Card, error) {
	return nil, nil
}
func (f *fakeTaskAPI) Show(ctx context.Context, scope kernel.Scope, taskID uuid.UUID) (tasklog.View, error) {
	if f.show == nil {
		return tasklog.View{}, nil
	}
	return f.show(ctx, scope, taskID)
}
func (f *fakeTaskAPI) RebuildSession(context.Context, kernel.Scope) (tasklog.Stats, error) {
	return tasklog.Stats{}, nil
}
func (f *fakeTaskAPI) ForkCard(context.Context, kernel.Scope, uuid.UUID, int64, kernel.Scope, string, string) (tasklog.Card, error) {
	return tasklog.Card{}, nil
}
func (f *fakeTaskAPI) LinkSubagent(ctx context.Context, parent kernel.Scope, child uuid.UUID) (tasklog.Card, error) {
	if f.link == nil {
		return tasklog.Card{}, nil
	}
	return f.link(ctx, parent, child)
}

var _ tasklog.API = (*fakeTaskAPI)(nil)

func tasksToolSpec(t *testing.T, plugin *Plugin, name string) ToolSpec {
	t.Helper()
	for _, spec := range plugin.Tools {
		if spec.Name == name {
			return spec
		}
	}
	t.Fatalf("plugin has no tool %s", name)
	return ToolSpec{}
}

func runTaskTool(t *testing.T, spec ToolSpec, ctx context.Context, args string) (agentcore.AgentToolResult, map[string]any) {
	t.Helper()
	result, err := spec.Execute(ctx, "call-1", json.RawMessage(args), nil)
	if err != nil {
		t.Fatalf("%s execute: %v", spec.Name, err)
	}
	document, _ := result.StructuredContent.(map[string]any)
	return result, document
}

func TestTasksPluginCreateAndShow(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	parent := uuid.New()
	card := tasklog.Card{
		TenantID: tenant, SessionID: session, TaskID: uuid.New(),
		Title: "build dist", Status: tasklog.StatusOpen,
		CreatedSeq: 5, UpdatedSeq: 5, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	api := &fakeTaskAPI{
		create: func(_ context.Context, scope kernel.Scope, in tasklog.CreateInput, callID string) (tasklog.Card, error) {
			if scope.TenantID != tenant || scope.SessionID != session {
				t.Fatalf("create scope = %+v", scope)
			}
			if in.Title != card.Title {
				t.Fatalf("create title = %q", in.Title)
			}
			if in.ParentTaskID == nil || *in.ParentTaskID != parent {
				t.Fatalf("create parent = %+v", in.ParentTaskID)
			}
			return card, nil
		},
		show: func(_ context.Context, scope kernel.Scope, taskID uuid.UUID) (tasklog.View, error) {
			if taskID != card.TaskID {
				t.Fatalf("show id = %s", taskID)
			}
			child := card
			child.TaskID = uuid.New()
			child.ParentTaskID = &card.TaskID
			return tasklog.View{
				Card:        card,
				Children:    []tasklog.Card{child},
				Attachments: []tasklog.Attachment{{Path: "dist/app.js", Hash: "sha256:abc"}},
				Events: []tasklog.Event{{
					TaskID: card.TaskID, Kind: string(kernel.KindTaskCreated), Seq: 5,
					Payload: json.RawMessage(`{"taskId":"` + card.TaskID.String() + `","title":"build dist"}`),
				}},
			}, nil
		},
	}
	plugin := NewTasksPlugin(TasksOptions{Service: api})
	ctx := WithRunScope(context.Background(), RunScope{TenantID: tenant, SessionID: session, RunID: "run-1"})

	_, created := runTaskTool(t, tasksToolSpec(t, plugin, TasksToolCreate), ctx,
		`{"title":"build dist","parentTaskId":"`+parent.String()+`"}`)
	if created["taskId"] != card.TaskID.String() || created["title"] != "build dist" {
		t.Fatalf("create document = %+v", created)
	}

	_, shown := runTaskTool(t, tasksToolSpec(t, plugin, TasksToolShow), ctx,
		`{"taskId":"`+card.TaskID.String()+`"}`)
	if shown["card"] == nil || shown["events"] == nil || shown["children"] == nil {
		t.Fatalf("show document = %+v", shown)
	}
	attachments, ok := shown["attachments"].([]tasklog.Attachment)
	if !ok || len(attachments) != 1 || attachments[0].Path != "dist/app.js" {
		t.Fatalf("show attachments = %+v", shown["attachments"])
	}
}

func TestTasksPluginErrorsAreTyped(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	api := &fakeTaskAPI{show: func(context.Context, kernel.Scope, uuid.UUID) (tasklog.View, error) {
		return tasklog.View{}, tasklog.ErrTaskNotFound
	}, update: func(context.Context, kernel.Scope, uuid.UUID, tasklog.UpdateInput, string) (tasklog.Card, error) {
		return tasklog.Card{}, tasklog.ErrTaskCycle
	}}
	plugin := NewTasksPlugin(TasksOptions{Service: api})
	ctx := WithRunScope(context.Background(), RunScope{TenantID: tenant, SessionID: session})

	result, document := runTaskTool(t, tasksToolSpec(t, plugin, TasksToolShow), ctx, `{"taskId":"`+uuid.NewString()+`"}`)
	if result.IsError == nil || !*result.IsError {
		t.Fatalf("not-found show is not an error result: %+v", result)
	}
	problem := document["error"].(map[string]any)
	if problem["code"] != "not_found" {
		t.Fatalf("show error code = %v", problem)
	}
	_, document = runTaskTool(t, tasksToolSpec(t, plugin, TasksToolUpdate), ctx,
		`{"taskId":"`+uuid.NewString()+`","parentTaskId":"`+uuid.NewString()+`"}`)
	problem = document["error"].(map[string]any)
	if problem["code"] != "invalid_arguments" {
		t.Fatalf("cycle error code = %v", problem)
	}

	// A tool call outside a run has no session to attribute; the plugin answers
	// the typed refusal instead of guessing.
	_, document = runTaskTool(t, tasksToolSpec(t, plugin, TasksToolShow), context.Background(), `{"taskId":"`+uuid.NewString()+`"}`)
	problem = document["error"].(map[string]any)
	if problem["code"] != "run_scope_missing" {
		t.Fatalf("missing scope code = %v", problem)
	}
}

func TestTasksPluginLinksSubagentCards(t *testing.T) {
	tenant, parentSession, childSession := uuid.New(), uuid.New(), uuid.New()
	var linked bool
	api := &fakeTaskAPI{link: func(_ context.Context, parent kernel.Scope, child uuid.UUID) (tasklog.Card, error) {
		linked = true
		if parent.TenantID != tenant || parent.SessionID != parentSession || child != childSession {
			t.Fatalf("link args = %+v / %s", parent, child)
		}
		return tasklog.Card{TaskID: uuid.New()}, nil
	}}
	plugin := NewTasksPlugin(TasksOptions{Service: api})
	if len(plugin.Hooks) != 1 || plugin.Hooks[0].Event != HookSubagentStarted {
		t.Fatalf("plugin hooks = %+v", plugin.Hooks)
	}
	err := plugin.Hooks[0].Handler(context.Background(), HookPayload{
		Event:             HookSubagentStarted,
		TenantID:          tenant.String(),
		ParentSessionID:   parentSession.String(),
		SubagentSessionID: childSession.String(),
	})
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if !linked {
		t.Fatal("the subagent.started hook did not link the child card")
	}
}

func TestTasksPluginMissingServiceIsTyped(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	plugin := NewTasksPlugin(TasksOptions{})
	ctx := WithRunScope(context.Background(), RunScope{TenantID: tenant, SessionID: session})
	result, document := runTaskTool(t, tasksToolSpec(t, plugin, TasksToolList), ctx, `{}`)
	if result.IsError == nil || !*result.IsError {
		t.Fatalf("nil service is not an error: %+v", result)
	}
	problem := document["error"].(map[string]any)
	if problem["code"] != "backend_error" {
		t.Fatalf("nil service code = %v", problem)
	}
}

// TestTasksPluginDoesNotRelinkForkedCards pins the fork origin: a forked child
// already has its card with parentTaskId = origin card; the started hook must
// not reparent it to the parent's active card.
func TestTasksPluginDoesNotRelinkForkedCards(t *testing.T) {
	tenant, parentSession, childSession := uuid.New(), uuid.New(), uuid.New()
	linked := 0
	api := &fakeTaskAPI{link: func(context.Context, kernel.Scope, uuid.UUID) (tasklog.Card, error) {
		linked++
		return tasklog.Card{}, nil
	}}
	plugin := NewTasksPlugin(TasksOptions{Service: api})
	payload := HookPayload{
		Event:             HookSubagentStarted,
		TenantID:          tenant.String(),
		ParentSessionID:   parentSession.String(),
		SubagentSessionID: childSession.String(),
		Data:              map[string]string{"origin": "fork"},
	}
	if err := plugin.Hooks[0].Handler(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if linked != 0 {
		t.Fatal("a forked child was relinked to the parent's active card")
	}
	payload.Data = nil
	if err := plugin.Hooks[0].Handler(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if linked != 1 {
		t.Fatalf("a spawned child was not linked (calls=%d)", linked)
	}
}
