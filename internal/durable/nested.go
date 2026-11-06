package durable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

var positiveIntegerKey = regexp.MustCompile(`^[1-9][0-9]*$`)

func checkNestedKey(key string) error {
	if key == "" || strings.Contains(key, "/") || key == "__proto__" || positiveIntegerKey.MatchString(key) {
		return fmt.Errorf(
			`Nested call key %q must be non-empty, without "/", not "__proto__", and not a positive integer`, key)
	}
	return nil
}

// executeNestedCall starts (or reattaches to) a nested call and waits for its
// result. Cancelling ctx stops only the wait.
func (h *Harness) executeNestedCall(ctx context.Context, parent TaskID, parentCallID, name string, args json.RawMessage, key string, options NestedOptions) (NestedResult, error) {
	parentTask, ok := h.store.GetTask(parent)
	if !ok {
		return NestedResult{}, fmt.Errorf("durable: calling task %s does not exist", parent)
	}
	conv := parentTask.ConversationID
	call := ToolCall{
		Type: "toolCall", ID: parentCallID + "/" + key, Name: name,
		Arguments: cloneRaw(args),
	}

	h.mu.Lock()
	if h.dead || h.closed {
		h.mu.Unlock()
		return NestedResult{}, ErrHarnessClosed{}
	}
	doc, exists := h.loadNested(conv, parent, key)
	var id TaskID
	if exists {
		id = doc.TaskID
		existing, ok := h.store.GetTask(id)
		if !ok {
			h.mu.Unlock()
			return NestedResult{}, fmt.Errorf("Nested call %s was already made but its task is missing", call.ID)
		}
		input, _ := decodeToolInput(existing)
		same := input.Kind == "nested" && input.Call != nil && input.Call.Name == name &&
			jsonEqual(input.Call.Arguments, call.Arguments)
		if !same {
			h.mu.Unlock()
			return NestedResult{}, fmt.Errorf("Nested call %s was already made with another tool or other arguments", call.ID)
		}
		if doc.Result != nil {
			result := cloneNestedResult(*doc.Result)
			h.mu.Unlock()
			return result, nil
		}
		h.mu.Unlock()
	} else {
		input := ToolTaskInput{
			Kind: "nested", Parent: parent, ParentCallID: parentCallID, Key: key, Call: &call,
		}
		if options.Progress != nil && !*options.Progress {
			input.Progress = options.Progress
		}
		task := TaskRecord{
			ConversationID: conv, Kind: ToolTaskKind, Version: 3,
			Input: rawJSON(input), Owner: parent,
			State:     TaskState{Status: TaskPending, Checkpoint: rawJSON(ToolTaskCheckpoint{Phase: "call"})},
			StartedAt: h.now(),
		}
		created, _ := h.store.CreateTask(task)
		id = created.ID
		doc = NestedCallDoc{TaskID: id}
		h.saveNested(conv, parent, key, doc)
		live := h.loadLive(conv)
		live.NestedTools = append(live.NestedTools, NestedToolSlot{
			CallID: call.ID, ParentCallID: parentCallID, ParentTaskID: parent,
			Name: name, TaskID: id, Arguments: cloneRaw(args), Status: "pending",
		})
		h.saveLive(conv, live)
		runCtx, cancel := context.WithCancel(context.Background())
		h.running[id] = cancel
		go h.executeToolTask(runCtx, conv, id, false)
		h.notifyLocked()
		h.mu.Unlock()
	}

	task, err := h.waitTask(ctx, id)
	if err != nil {
		return NestedResult{}, err
	}
	doc, ok = h.loadNested(conv, parent, key)
	if ok && doc.Result != nil {
		return cloneNestedResult(*doc.Result), nil
	}
	return fallbackNestedResult(id, name, task.State.Outcome), nil
}

func fallbackNestedResult(taskID TaskID, name string, outcome *Outcome) NestedResult {
	status := OutcomeFaulted
	message := fmt.Sprintf("Tool %s ended without a result", name)
	if outcome != nil {
		switch outcome.Status {
		case OutcomeFaulted:
			status = OutcomeFaulted
			if outcome.Error != nil {
				message = fmt.Sprintf("Tool %s failed: %s", name, outcome.Error.Message)
			}
		case OutcomeOrphaned:
			status = OutcomeOrphaned
			message = fmt.Sprintf("Tool %s could not resume: %s", name, outcome.Reason)
		case OutcomeAborted:
			status = OutcomeAborted
			message = fmt.Sprintf("Tool %s was aborted", name)
		case OutcomeFailed:
			status = OutcomeFailed
			if outcome.Error != nil {
				message = outcome.Error.Message
			}
		}
	}
	return NestedResult{
		TaskID: taskID, IsError: true,
		Diagnostics: []Diagnostic{{Severity: "error", Code: status, Message: message}},
	}
}

// abortNestedOf aborts the nested calls a tool task left running, then waits
// for them to settle (children end before their caller).
func (h *Harness) abortNestedOf(parent TaskID) {
	h.mu.Lock()
	var ids []TaskID
	for _, task := range h.store.ListTasks("") {
		if task.Owner != parent || !task.Live() {
			continue
		}
		input, err := decodeToolInput(task)
		if err == nil && input.Kind == "nested" {
			ids = append(ids, task.ID)
			if cancel, ok := h.running[task.ID]; ok {
				cancel()
			}
		}
	}
	h.mu.Unlock()
	for _, id := range ids {
		h.waitTerminal(id)
	}
}

// abortOwnedTools cancels and waits for a generation's model-issued tool tasks.
func (h *Harness) abortOwnedTools(genTaskID TaskID) {
	h.mu.Lock()
	var ids []TaskID
	for _, task := range h.store.ListTasks("") {
		if task.Owner != genTaskID || !task.Live() {
			continue
		}
		ids = append(ids, task.ID)
		if cancel, ok := h.running[task.ID]; ok {
			cancel()
		}
	}
	h.mu.Unlock()
	for _, id := range ids {
		h.waitTerminal(id)
	}
	h.appendPendingToolResults(genTaskID)
}

func (h *Harness) waitTerminal(id TaskID) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rec, ok := h.store.GetTask(id)
		if !ok || rec.Terminal() {
			return
		}
		_ = h.notifyCh.wait(context.Background())
	}
}

// appendPendingToolResults writes aborted results for calls a sequential round
// never started, so a continue sees a complete turn.
func (h *Harness) appendPendingToolResults(genTaskID TaskID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	task, ok := h.store.GetTask(genTaskID)
	if !ok {
		return
	}
	conv := task.ConversationID
	cp := decodeGenerationCheckpoint(task)
	live := h.loadLive(conv)
	for i := range live.Tools {
		slot := &live.Tools[i]
		if slot.TaskID != "" || slot.Status == "done" {
			continue
		}
		call, err := h.readToolCall(conv, ToolTaskInput{Kind: "model", Assistant: cp.Assistant, CallID: slot.CallID})
		if err != nil {
			continue
		}
		result := harnessError("aborted", fmt.Sprintf("Tool %s was aborted", call.Name))
		if slot.Unoffered {
			result = harnessError("tool_unavailable", fmt.Sprintf("Tool %s is not available", call.Name))
		}
		entry := h.appendToolResultLocked(conv, call, result, -1)
		slot.Status = "done"
		slot.Entry = entry.ID
	}
	h.saveLive(conv, live)
}

// ---- tool round ----

// createToolTaskLocked creates one model-issued tool task and launches it. The
// caller owns the live slot.
func (h *Harness) createToolTaskLocked(conv ConversationID, genTaskID TaskID, assistant EntryID, call ToolCall, unoffered bool) TaskID {
	input := ToolTaskInput{Kind: "model", Assistant: assistant, CallID: call.ID, Unoffered: unoffered}
	task := TaskRecord{
		ConversationID: conv, Kind: ToolTaskKind, Version: 3,
		Input: rawJSON(input), Owner: genTaskID,
		State:     TaskState{Status: TaskPending, Checkpoint: rawJSON(ToolTaskCheckpoint{Phase: "call"})},
		StartedAt: h.now(),
	}
	created, _ := h.store.CreateTask(task)
	runCtx, cancel := context.WithCancel(context.Background())
	h.running[created.ID] = cancel
	go h.executeToolTask(runCtx, conv, created.ID, false)
	return created.ID
}

// startToolRound appends the tool-calling answer and creates its tool tasks.
func (h *Harness) startToolRound(conv ConversationID, genTaskID TaskID, msg agentcore.AssistantMessage, calls []ToolCall, offered []string) {
	entry := h.appendAssistant(conv, msg)
	sequential := h.settings.ToolExecution == ToolExecutionSequential
	if !sequential {
		for _, call := range calls {
			if tool := h.resolveOfferedTool(conv, call.Name); tool != nil && tool.ExecutionMode == ToolExecutionSequential {
				sequential = true
				break
			}
		}
	}
	offeredSet := map[string]bool{}
	for _, name := range offered {
		offeredSet[name] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	live := h.loadLive(conv)
	// Reset the round's slots.
	live.Tools = nil
	live.NestedTools = nil
	cp := GenerationCheckpoint{Phase: "tools", Assistant: entry.ID, Offered: offered}
	for i, call := range calls {
		unoffered := !offeredSet[call.Name]
		slot := ToolSlot{CallID: call.ID, Name: call.Name, Status: "pending", Unoffered: unoffered}
		if sequential && i > 0 {
			cp.Pending = append(cp.Pending, call.ID)
			live.Tools = append(live.Tools, slot)
			continue
		}
		id := h.createToolTaskLocked(conv, genTaskID, entry.ID, call, unoffered)
		slot.TaskID = id
		cp.Tools = append(cp.Tools, id)
		live.Tools = append(live.Tools, slot)
	}
	if live.Run != nil && live.Run.TaskID == genTaskID {
		live.Run.Turns++
		if live.Run.First == 0 {
			live.Run.First = entry.ID
		}
	}
	h.saveLive(conv, live)
	_, _ = h.store.UpdateTask(genTaskID, func(t *TaskRecord) {
		t.State.Status = TaskWaiting
		t.State.On = append([]TaskID(nil), cp.Tools...)
		t.State.Checkpoint = rawJSON(cp)
	})
	h.notifyLocked()
}

var errRoundAborted = errors.New("durable: tool round aborted")

// runToolPhase drives the generation's tools phase. It returns finished=false
// when the checkpoint changed and the caller should re-enter.
func (h *Harness) runToolPhase(ctx context.Context, conv ConversationID, genTaskID TaskID, cp GenerationCheckpoint) (finished, terminate bool, handoff string, err error) {
	for {
		if ctx.Err() != nil {
			return false, false, "", errRoundAborted
		}
		task, ok := h.store.GetTask(genTaskID)
		if !ok || task.Terminal() {
			return false, false, "", errRoundAborted
		}
		cp = decodeGenerationCheckpoint(task)
		// A sequential round starts its next call only after the current one
		// settled.
		existingLive := false
		for _, id := range cp.Tools {
			rec, ok := h.store.GetTask(id)
			if !ok || !rec.Terminal() {
				existingLive = true
				break
			}
		}
		if existingLive {
			if err := h.waitRound(ctx, cp.Tools); err != nil {
				if ctx.Err() != nil {
					return false, false, "", errRoundAborted
				}
				return false, false, "", err
			}
			continue
		}
		if len(cp.Pending) > 0 {
			callID := cp.Pending[0]
			assistantEntry := cp.Assistant
			h.mu.Lock()
			live := h.loadLive(conv)
			slotIndex := -1
			for i := range live.Tools {
				if live.Tools[i].CallID == callID && live.Tools[i].TaskID == "" {
					slotIndex = i
					break
				}
			}
			call, err := h.readToolCall(conv, ToolTaskInput{Kind: "model", Assistant: assistantEntry, CallID: callID})
			if err != nil {
				h.mu.Unlock()
				return false, false, "", err
			}
			var id TaskID
			if slotIndex >= 0 {
				slot := live.Tools[slotIndex]
				id = h.createToolTaskLocked(conv, genTaskID, assistantEntry,
					ToolCall{Type: "toolCall", ID: call.ID, Name: call.Name, Arguments: call.Arguments}, slot.Unoffered)
				live.Tools[slotIndex].TaskID = id
				h.saveLive(conv, live)
			}
			cp.Tools = append(cp.Tools, id)
			cp.Pending = cp.Pending[1:]
			_, _ = h.store.UpdateTask(genTaskID, func(t *TaskRecord) {
				t.State.Status = TaskWaiting
				t.State.On = append([]TaskID(nil), cp.Tools...)
				t.State.Checkpoint = rawJSON(cp)
			})
			h.notifyLocked()
			h.mu.Unlock()
			if _, err := h.waitTask(ctx, id); err != nil {
				if ctx.Err() != nil {
					return false, false, "", errRoundAborted
				}
				return false, false, "", err
			}
			continue
		}
		// Wait for every created tool task.
		allDone := true
		for _, id := range cp.Tools {
			rec, ok := h.store.GetTask(id)
			if !ok || !rec.Terminal() {
				allDone = false
				break
			}
		}
		if !allDone {
			if err := h.waitRound(ctx, cp.Tools); err != nil {
				if ctx.Err() != nil {
					return false, false, "", errRoundAborted
				}
				return false, false, "", err
			}
			continue
		}
		// Collect controls in call order from the live slots.
		live := h.loadLive(conv)
		controls := map[TaskID]*ToolControl{}
		for _, id := range cp.Tools {
			rec, ok := h.store.GetTask(id)
			if !ok || rec.State.Outcome == nil || rec.State.Outcome.Status != OutcomeCompleted {
				continue
			}
			var result ToolTaskResult
			if err := json.Unmarshal(rec.State.Outcome.Result, &result); err == nil {
				controls[id] = result.Control
			}
		}
		terminate = len(live.Tools) > 0
		for _, slot := range live.Tools {
			if slot.TaskID == "" {
				terminate = false
				break
			}
			control := controls[slot.TaskID]
			if control == nil || !control.Terminate {
				terminate = false
				break
			}
		}
		for _, slot := range live.Tools {
			control := controls[slot.TaskID]
			if control != nil && control.Handoff != "" {
				handoff = control.Handoff
			}
		}
		return true, terminate, handoff, nil
	}
}

// waitRound waits for a set of tool tasks; it returns early when the run context
// is cancelled.
func (h *Harness) waitRound(ctx context.Context, ids []TaskID) error {
	for {
		ch := h.notifyCh.current()
		all := true
		for _, id := range ids {
			rec, ok := h.store.GetTask(id)
			if !ok || !rec.Terminal() {
				all = false
				break
			}
		}
		if all {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := waitChan(ctx, ch); err != nil {
			return err
		}
	}
}
