package durable

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// Task state machine, retry/restart limits and generation attempts.

func TestTaskStateMachine_PendingRunningDoneFailedAndRestartLimitDefaults(t *testing.T) {
	settings := ResolveSettings(Settings{})
	if settings.Retry.MaxRetries != 3 || settings.Retry.BaseDelayMs != 2000 || settings.Retry.MaxAgentDelayMs != 60000 {
		t.Fatalf("retry defaults = %+v", settings.Retry)
	}
	if settings.Progress.PartialIntervalMs != 100 || settings.Progress.OutputIntervalMs != 100 || settings.Progress.PersistWithinMs != 1000 {
		t.Fatalf("progress defaults = %+v", settings.Progress)
	}
	if settings.RestartLimit != 5 {
		t.Fatalf("restart limit = %d", settings.RestartLimit)
	}
	if settings.ToolExecution != ToolExecutionParallel || settings.SteeringMode != QueueOneAtATime ||
		settings.FollowUpMode != QueueOneAtATime || settings.AfterFailedRun != AfterFailedKeep {
		t.Fatalf("settings defaults = %+v", settings)
	}
	store := NewMemStore()
	task, created := store.CreateTask(TaskRecord{
		ConversationID: RootConversationID, Kind: GenerationTaskKind, Version: 1, Input: emptyJSON(),
		State: TaskState{Status: TaskPending, Checkpoint: rawJSON(GenerationCheckpoint{Phase: "prepare"})},
	})
	if !created || task.ID == "" || task.StatusWord() != "pending" || !task.Live() {
		t.Fatalf("pending task = %+v", task)
	}
	task, _ = store.UpdateTask(task.ID, func(t *TaskRecord) {
		t.State.Status = TaskRunning
		t.Restarts = 2
	})
	if task.StatusWord() != "running" {
		t.Fatalf("running = %+v", task)
	}
	task, _ = store.UpdateTask(task.ID, func(t *TaskRecord) {
		t.State.Status = TaskTerminal
		t.State.Outcome = &Outcome{Status: OutcomeCompleted, Result: rawJSON(map[string]any{"entryId": 1})}
	})
	if task.StatusWord() != "done" || task.Terminal() {
		if !task.Terminal() || task.StatusWord() != "done" {
			t.Fatalf("done = %+v", task)
		}
	}
	task, _ = store.UpdateTask(task.ID, func(t *TaskRecord) {
		t.State.Outcome = &Outcome{Status: OutcomeFailed, Error: &TaskError{Message: "x"}}
	})
	if task.StatusWord() != "failed" {
		t.Fatalf("failed = %+v", task)
	}
}

func TestGeneration_RetriesAFailedAttemptUnderTheRetryPolicy(t *testing.T) {
	ctx := ctxT(t)
	base := &fauxProvider{}
	base.setResponses(errorMessage("503 Service Unavailable"), textMessage("recovered"))
	var generationIDs []string
	provider := &recordingProvider{inner: base, onRequest: func(req Request) {
		generationIDs = append(generationIDs, req.GenerationID)
	}}
	settings := Settings{Retry: &RetryPolicy{Enabled: true, MaxRetries: 1, BaseDelayMs: 1, MaxAgentDelayMs: 5}}
	h, root := openChat(t, nil, Options{Provider: provider, Settings: settings})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	if base.callCount() != 2 {
		t.Fatalf("calls = %d", base.callCount())
	}
	if len(generationIDs) != 2 || !strings.HasSuffix(generationIDs[0], ":1") || !strings.HasSuffix(generationIDs[1], ":2") {
		t.Fatalf("generation ids = %v", generationIDs)
	}
	if h.Live(root).RunFailure != nil {
		t.Fatalf("failure = %+v", h.Live(root).RunFailure)
	}
}

type recordingProvider struct {
	inner     *fauxProvider
	onRequest func(Request)
}

func (p *recordingProvider) Complete(ctx context.Context, req Request) (agentcore.AssistantMessage, error) {
	p.onRequest(req)
	return p.inner.Complete(ctx, req)
}

func TestGeneration_EndsModelErrorWhenRetriesAreExhausted(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	faux.setResponses(errorMessage("503 Service Unavailable"), errorMessage("503 Service Unavailable"))
	h, root := openChat(t, nil, Options{Provider: faux,
		Settings: Settings{Retry: &RetryPolicy{Enabled: true, MaxRetries: 1, BaseDelayMs: 1, MaxAgentDelayMs: 5}}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionUnanswered || settled.Reason != ReasonModelError {
		t.Fatalf("settled = %+v", settled)
	}
	if faux.callCount() != 2 {
		t.Fatalf("calls = %d", faux.callCount())
	}
	if failure := h.Live(root).RunFailure; failure == nil || failure.Reason != ReasonModelError {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestGeneration_DoesNotRetryANonRetryableModelError(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	faux.setResponses(errorMessage("Invalid request"))
	_, root := openChat(t, nil, Options{Provider: faux})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if faux.callCount() != 1 {
		t.Fatalf("calls = %d", faux.callCount())
	}
}

func TestGeneration_ReusesACommittedAnswerWithoutARequest(t *testing.T) {
	ctx := ctxT(t)
	store := NewMemStore()
	faux1 := &fauxProvider{}
	faux1.setResponses(textMessage("committed"))
	h1, root1 := openChat(t, store, Options{Provider: faux1})
	sub, err := root1.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	runs := 0
	var taskID TaskID
	for _, task := range h1.Tasks(root1) {
		if task.Kind == GenerationTaskKind {
			runs++
			taskID = task.ID
		}
	}
	if runs != 1 {
		t.Fatalf("generations = %d", runs)
	}
	h1.Kill()

	faux2 := &fauxProvider{}
	faux2.setResponses()
	h2, _ := openChat(t, store, Options{Provider: faux2})
	h2.Recover()
	if faux2.callCount() != 0 {
		t.Fatalf("recovery made a request")
	}
	task, ok := h2.Task(taskID)
	if !ok || task.State.Outcome == nil || task.State.Outcome.Status != OutcomeCompleted {
		t.Fatalf("task = %+v", task)
	}
	var result struct {
		EntryID EntryID `json:"entryId"`
	}
	_ = json.Unmarshal(task.State.Outcome.Result, &result)
	if result.EntryID != settled.AnswerID {
		t.Fatalf("result entry = %d, answer = %d", result.EntryID, settled.AnswerID)
	}
}

func restartingWorkTool(started *deferred) *Tool {
	return &Tool{
		Name: "work", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			api.Output("work " + strings.Repeat("o", 40) + "\n")
			api.Details(map[string]int{"run": 1})
			started.resolve()
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		},
	}
}

func TestTaskRestart_AnInterruptedToolCallSettlesInterruptedAndTheRunContinues(t *testing.T) {
	ctx := ctxT(t)
	store := NewMemStore()
	started := newDeferred()
	var codes []string
	hook := &Hook{Task: ToolTaskKind, AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
		codes = append(codes, codesOf(result)...)
		return nil, nil
	}}
	faux1 := &fauxProvider{}
	faux1.setResponses(calls(callSpec{name: "work", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h1, root1 := openChat(t, store, Options{Provider: faux1, Tools: []*Tool{restartingWorkTool(started)}, Hooks: []*Hook{hook}})
	sub, err := root1.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-started.ch
	h1.Kill()

	faux2 := &fauxProvider{}
	faux2.setResponses(textMessage("done"))
	h2, root2 := openChat(t, store, Options{Provider: faux2, Tools: []*Tool{restartingWorkTool(newDeferred())},
		Hooks: []*Hook{hook}})
	h2.Recover()
	reopened, _ := h2.Submission(sub.ID())
	settled, err := reopened.Wait(ctx)
	if err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	if len(codes) != 1 || codes[0] != "interrupted" {
		t.Fatalf("hook codes = %v", codes)
	}
	messages := toolResults(h2, root2)
	if !messages["c1"].IsError {
		t.Fatalf("result = %+v", messages["c1"])
	}
	if got := toolCodes(h2, root2, "c1"); len(got) != 1 || got[0] != "interrupted" {
		t.Fatalf("codes = %v", got)
	}
	if !strings.HasPrefix(OutputText(messages["c1"].Content), "work "+strings.Repeat("o", 40)) {
		t.Fatalf("partial output = %q", strings.Repeat("x", 0)+OutputText(messages["c1"].Content))
	}
}

func TestTaskRestart_AToolCutOffAtTheRestartLimitEndsWithRestartLimit(t *testing.T) {
	ctx := ctxT(t)
	store := NewMemStore()
	started := newDeferred()
	var codes []string
	hook := &Hook{Task: ToolTaskKind, AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
		codes = append(codes, codesOf(result)...)
		return nil, nil
	}}
	faux1 := &fauxProvider{}
	faux1.setResponses(calls(callSpec{name: "work", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h1, root1 := openChat(t, store, Options{Provider: faux1, Tools: []*Tool{restartingWorkTool(started)},
		Hooks: []*Hook{hook}, Settings: Settings{RestartLimit: 1}})
	sub, err := root1.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-started.ch
	var toolTask TaskID
	for _, task := range h1.Tasks(root1) {
		if task.Kind == ToolTaskKind {
			toolTask = task.ID
		}
	}
	h1.Kill()

	faux2 := &fauxProvider{}
	faux2.setResponses(textMessage("done"))
	h2, root2 := openChat(t, store, Options{Provider: faux2, Tools: []*Tool{restartingWorkTool(newDeferred())},
		Hooks: []*Hook{hook}, Settings: Settings{RestartLimit: 1}})
	h2.Recover()
	reopened, _ := h2.Submission(sub.ID())
	if settled, err := reopened.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	if len(codes) != 1 || codes[0] != "restart_limit" {
		t.Fatalf("hook codes = %v", codes)
	}
	if got := toolCodes(h2, root2, "c1"); len(got) != 1 || got[0] != "restart_limit" {
		t.Fatalf("codes = %v", got)
	}
	task, ok := h2.Task(toolTask)
	if !ok || task.State.Outcome == nil || task.State.Outcome.Status != OutcomeFailed ||
		task.State.Outcome.Error == nil || !strings.Contains(string(task.State.Outcome.Error.Detail), "restart_limit") {
		t.Fatalf("task = %+v", task.State)
	}
}

func TestGeneration_RecoveryChargesTheInterruptedAttemptUnderRetry(t *testing.T) {
	ctx := ctxT(t)
	store := NewMemStore()
	faux1 := &fauxProvider{}
	step, reached, _ := gated(textMessage("never"))
	faux1.setResponses(step)
	h1, root1 := openChat(t, store, Options{Provider: faux1})
	sub, err := root1.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	h1.Kill()

	faux2 := &fauxProvider{}
	faux2.setResponses(textMessage("after the retry"))
	h2, root2 := openChat(t, store, Options{Provider: faux2,
		Settings: Settings{Retry: &RetryPolicy{Enabled: true, MaxRetries: 3, BaseDelayMs: 1, MaxAgentDelayMs: 2}}})
	h2.Recover()
	reopened, _ := h2.Submission(sub.ID())
	settled, err := reopened.Wait(ctx)
	if err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	if faux2.callCount() != 1 {
		t.Fatalf("calls = %d", faux2.callCount())
	}
	_ = root2
}
