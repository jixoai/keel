package durable

import (
	"context"
	"fmt"
)

// Recover resumes the live tasks committed in the Store, as a new process
// opening the same session would. Generation attempts that were in flight count
// as failed attempts under the retry policy; when the retry budget is spent the
// run ends unanswered with `interrupted` (or `restart_limit` when the task was
// cut off too often). Tool calls follow their replay policy; a non-replay-safe
// call ends `interrupted`, and nested calls of a non-replay-safe caller are
// `abandoned`.
func (h *Harness) Recover() {
	h.mu.Lock()
	if h.closed || h.dead {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()

	for _, conv := range h.store.Conversations() {
		tasks := h.store.ListTasks(conv)
		// Tool tasks first: generation tasks wait on them.
		for _, task := range tasks {
			if !task.Live() || task.Kind != ToolTaskKind {
				continue
			}
			h.recoverToolTask(conv, task)
		}
		for _, task := range tasks {
			if !task.Live() || task.Kind != GenerationTaskKind {
				continue
			}
			h.recoverGenerationTask(conv, task)
		}
	}
	h.notify()
}

func (h *Harness) recoverToolTask(conv ConversationID, task TaskRecord) {
	restarts := task.Restarts + 1
	_, _ = h.store.UpdateTask(task.ID, func(t *TaskRecord) {
		t.Restarts = restarts
		t.State.Status = TaskPending
	})
	input, err := decodeToolInput(task)
	if err != nil {
		h.faultTool(task.ID, err)
		return
	}
	cp := decodeToolCheckpoint(task)
	call, callErr := h.readToolCall(conv, input)
	if callErr != nil {
		name := "unknown"
		var args []byte
		if input.Call != nil {
			name = input.Call.Name
			args = input.Call.Arguments
		}
		call = ToolHookCall{ID: input.CallID, Name: name, Arguments: args}
	}

	if input.Kind == "nested" {
		if abandoned, reason := h.nestedAbandoned(input); abandoned {
			message := fmt.Sprintf("Tool %s was not started: its caller ended with a restart", call.Name)
			if reason != "" {
				message = reason
			}
			h.settleFromSlot(conv, task.ID, input, call, h.toolOrNil(call.Name), "abandoned", message, true)
			return
		}
	}

	if restarts >= h.settings.RestartLimit {
		message := fmt.Sprintf("Tool %s was interrupted %d times; it is not run again", call.Name, restarts)
		h.settleFromSlot(conv, task.ID, input, call, h.toolOrNil(call.Name), "restart_limit", message,
			restarts == h.settings.RestartLimit)
		return
	}

	if cp.Phase == "execute" {
		tool := h.toolOrNil(call.Name)
		if cp.Replay != ReplaySafe || tool == nil || tool.replay() != ReplaySafe {
			message := fmt.Sprintf("Tool %s was interrupted and may have partially run", call.Name)
			h.settleFromSlot(conv, task.ID, input, call, tool, "interrupted", message, true)
			return
		}
	}

	// Resume the call from its call phase (replaying a safe tool).
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.running[task.ID] = cancel
	h.mu.Unlock()
	go h.executeToolTask(ctx, conv, task.ID, true)
}

// nestedAbandoned reports whether a nested call must be abandoned because its
// caller is not replay-safe, or is gone.
func (h *Harness) nestedAbandoned(input ToolTaskInput) (bool, string) {
	if input.Kind != "nested" {
		return false, ""
	}
	parent, ok := h.store.GetTask(input.Parent)
	if !ok || !parent.Live() {
		return true, ""
	}
	parentInput, err := decodeToolInput(parent)
	if err != nil || parentInput.Kind != "model" {
		return true, ""
	}
	parentCall, err := h.readToolCall(parent.ConversationID, parentInput)
	if err != nil {
		return true, ""
	}
	tool := h.toolOrNil(parentCall.Name)
	if tool == nil || tool.replay() != ReplaySafe {
		return true, ""
	}
	return false, ""
}

func (h *Harness) toolOrNil(name string) *Tool {
	return h.tools[name]
}

func (h *Harness) recoverGenerationTask(conv ConversationID, task TaskRecord) {
	cp := decodeGenerationCheckpoint(task)
	restarts := task.Restarts
	if cp.Phase == "request" || cp.Phase == "retry" {
		// Only an in-flight model attempt is charged as a failed attempt; a
		// generation parked on its tool round was not running.
		restarts++
	}
	_, _ = h.store.UpdateTask(task.ID, func(t *TaskRecord) {
		t.Restarts = restarts
		t.State.Status = TaskPending
	})
	if (cp.Phase == "request" || cp.Phase == "retry") && restarts >= h.settings.RestartLimit {
		message := fmt.Sprintf("The generation was interrupted %d times in its %s phase", restarts, cp.Phase)
		h.failRun(conv, task.ID, ReasonRestartLimit, message, true)
		return
	}
	if cp.Phase == "request" || cp.Phase == "retry" {
		attempt := cp.Attempt
		if attempt <= 0 {
			attempt = 1
		}
		if h.settings.Retry.Enabled && attempt <= h.settings.Retry.MaxRetries {
			next := GenerationCheckpoint{Phase: "prepare", Attempt: attempt + 1, Offered: cp.Offered}
			_, _ = h.store.UpdateTask(task.ID, func(t *TaskRecord) {
				t.State.Checkpoint = rawJSON(next)
			})
			h.launchGeneration(conv, task.ID)
			return
		}
		message := fmt.Sprintf("The host stopped while the model was answering, on attempt %d", attempt)
		h.failRun(conv, task.ID, ReasonInterrupted, message, true)
		return
	}
	h.launchGeneration(conv, task.ID)
}

func (h *Harness) launchGeneration(conv ConversationID, id TaskID) {
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.running[id] = cancel
	h.mu.Unlock()
	h.spawnGeneration(conv, id, ctx)
}
