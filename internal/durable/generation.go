package durable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// GenerationCheckpoint is the durable checkpoint of pi.generation.
type GenerationCheckpoint struct {
	Phase     string   `json:"phase"` // prepare | request | tools
	Attempt   int      `json:"attempt,omitempty"`
	Assistant EntryID  `json:"assistant,omitempty"`
	Tools     []TaskID `json:"tools,omitempty"`
	Pending   []string `json:"pending,omitempty"`
	Offered   []string `json:"offered,omitempty"`
}

func decodeGenerationCheckpoint(task TaskRecord) GenerationCheckpoint {
	var cp GenerationCheckpoint
	if len(task.State.Checkpoint) > 0 {
		_ = json.Unmarshal(task.State.Checkpoint, &cp)
	}
	if cp.Attempt == 0 {
		cp.Attempt = 1
	}
	if cp.Phase == "" {
		cp.Phase = "prepare"
	}
	return cp
}

// startRunLocked creates a conversation-owned generation and launches it.
func (h *Harness) startRunLocked(conv ConversationID, input json.RawMessage, inputs []SubmissionID, first EntryID, keepFailure bool) TaskID {
	task := TaskRecord{
		ConversationID: conv,
		Kind:           GenerationTaskKind,
		Version:        2,
		Input:          cloneRaw(input),
		State: TaskState{
			Status:     TaskPending,
			Checkpoint: rawJSON(GenerationCheckpoint{Phase: "prepare", Attempt: 1}),
		},
		StartedAt: h.now(),
	}
	created, _ := h.store.CreateTask(task)
	live := h.loadLive(conv)
	live.Run = &LiveRun{TaskID: created.ID, Inputs: append([]SubmissionID(nil), inputs...), Turns: 0, First: first}
	live.Generation = &GenerationLive{Attempt: 1}
	if !keepFailure {
		live.RunFailure = nil
	}
	live.Tools = nil
	live.NestedTools = nil
	h.saveLive(conv, live)

	runCtx, cancel := context.WithCancel(context.Background())
	h.running[created.ID] = cancel
	h.spawnGeneration(conv, created.ID, runCtx)
	return created.ID
}

// spawnGeneration starts a run's goroutine on the next scheduler turn. The
// admission commit is already durable; a caller that submits again immediately
// observes the busy conversation before its generation can run, as PI's
// scheduler does.
func (h *Harness) spawnGeneration(conv ConversationID, id TaskID, ctx context.Context) {
	go func() {
		time.Sleep(time.Millisecond)
		if h.Closed() {
			return
		}
		h.runGeneration(ctx, conv, id)
	}()
}

func (h *Harness) clearRunning(id TaskID) {
	h.mu.Lock()
	delete(h.running, id)
	h.mu.Unlock()
}

func (h *Harness) alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.dead && !h.closed
}

// runGeneration executes one generation task to a terminal outcome.
func (h *Harness) runGeneration(ctx context.Context, conv ConversationID, taskID TaskID) {
	defer h.clearRunning(taskID)

	h.mu.Lock()
	if !h.aliveLocked() {
		h.mu.Unlock()
		return
	}
	if task, ok := h.store.GetTask(taskID); ok && task.State.Status == TaskPending {
		_, _ = h.store.UpdateTask(taskID, func(t *TaskRecord) {
			t.State.Status = TaskRunning
			if t.StartedAt == 0 {
				t.StartedAt = h.now()
			}
		})
	}
	h.notifyLocked()
	h.mu.Unlock()

	for {
		if ctx.Err() != nil {
			if h.Closed() {
				return
			}
			h.abortRun(conv, taskID)
			return
		}
		h.mu.Lock()
		if h.dead {
			h.mu.Unlock()
			return
		}
		if h.closed {
			// Close is not a crash: leave the task running for a later process.
			h.mu.Unlock()
			return
		}
		task, ok := h.store.GetTask(taskID)
		h.mu.Unlock()
		if !ok || task.Terminal() {
			return
		}
		cp := decodeGenerationCheckpoint(task)
		if cp.Phase == "tools" {
			finished, terminate, handoff, err := h.runToolPhase(ctx, conv, taskID, cp)
			if err != nil {
				if errors.Is(err, errRoundAborted) || ctx.Err() != nil {
					h.abortRun(conv, taskID)
				}
				return
			}
			if !finished {
				continue
			}
			if terminate {
				h.finishAnswered(conv, taskID, cp.Assistant)
				return
			}
			// A queued reset cuts the run at the post-tools boundary, before an
			// answer; queued follow-ups start the next run in its context.
			if hasQueuedReset(h.loadInbox(conv)) {
				h.endRunAtReset(conv, taskID, cp.Assistant)
				return
			}
			if h.settings.MaxTurns > 0 && h.runTurns(conv, taskID) >= h.settings.MaxTurns {
				h.failRun(conv, taskID, ReasonTurnLimit, formatTurns(h.runTurns(conv, taskID)), true)
				return
			}
			h.applyPostToolsBoundary(conv, taskID)
			h.handOverGeneration(conv, taskID, cp.Assistant, handoff)
			return
		}

		attempt := cp.Attempt
		if attempt <= 0 {
			attempt = 1
		}
		// The context must end in a user message or tool results; system messages
		// say nothing the model answers.
		h.appendPendingSystem(conv)
		messages := h.Context(&Conversation{h: h, id: conv})
		last := lastNonSystem(messages)
		if last == nil || (last.Role != agentcore.RoleUser && last.Role != agentcore.RoleToolResult) {
			h.failRun(conv, taskID, ReasonNothingToAnswer, "The conversation has nothing to answer", true)
			return
		}
		ref := h.modelFor(conv)
		if !h.modelAvailable(ref) {
			message := "No model is configured"
			if ref.Provider != "" || ref.ModelID != "" {
				message = fmt.Sprintf("Model %s/%s is not available", ref.Provider, ref.ModelID)
			}
			h.failRun(conv, taskID, ReasonNoModel, message, true)
			return
		}
		if h.provider == nil {
			h.failRun(conv, taskID, ReasonNoModel, "No model is configured", true)
			return
		}
		offered := h.offeredTools(conv)
		names := make([]string, len(offered))
		for i, t := range offered {
			names[i] = t.Name
		}
		h.mu.Lock()
		_, _ = h.store.UpdateTask(taskID, func(t *TaskRecord) {
			t.State.Status = TaskRunning
			t.State.Checkpoint = rawJSON(GenerationCheckpoint{
				Phase: "request", Attempt: attempt, Offered: names,
			})
		})
		live := h.loadLive(conv)
		live.Generation = &GenerationLive{Attempt: attempt}
		h.saveLive(conv, live)
		h.notifyLocked()
		h.mu.Unlock()

		req := Request{
			ConversationID: conv,
			Messages:       messages,
			Tools:          offered,
			Model:          ref,
			GenerationID:   fmt.Sprintf("%s:%d", taskID, attempt),
			Attempt:        attempt,
		}
		msg, err := h.provider.Complete(ctx, req)
		if ctx.Err() != nil {
			if h.Closed() {
				return
			}
			h.abortRun(conv, taskID)
			return
		}
		if h.isDead() {
			return
		}
		if err != nil {
			msg = agentcore.AssistantMessage{
				RoleField:    agentcore.RoleAssistant,
				StopReason:   agentcore.StopReasonError,
				ErrorMessage: err.Error(),
				Timestamp:    h.now(),
			}
		}

		switch {
		case msg.StopReason == agentcore.StopReasonError || msg.StopReason == "error":
			retry := h.settings.Retry.Enabled && attempt <= h.settings.Retry.MaxRetries && isRetryableAssistantError(msg)
			if retry {
				until := h.now() + retryDelayMs(*h.settings.Retry, attempt)
				h.mu.Lock()
				live := h.loadLive(conv)
				live.Generation = &GenerationLive{Attempt: attempt, Retry: &RetryLive{At: until, Error: msg.ErrorMessage}}
				h.saveLive(conv, live)
				_, _ = h.store.UpdateTask(taskID, func(t *TaskRecord) {
					t.State.Status = TaskSleeping
					t.State.Until = until
					t.State.Checkpoint = rawJSON(GenerationCheckpoint{Phase: "retry", Attempt: attempt + 1})
				})
				h.notifyLocked()
				h.mu.Unlock()
				if !h.sleepUntil(ctx, until) {
					h.abortRun(conv, taskID)
					return
				}
				continue
			}
			detail := msg.ErrorMessage
			if detail == "" {
				detail = fmt.Sprintf("Model response ended with stop reason %s", msg.StopReason)
			}
			h.failRun(conv, taskID, ReasonModelError, detail, true)
			return

		case msg.StopReason == agentcore.StopReasonToolUse && len(msg.ToolCalls()) > 0:
			calls := toolCallsOf(msg)
			h.startToolRound(conv, taskID, msg, calls, names)
			continue

		case msg.StopReason == agentcore.StopReasonEndTurn || msg.StopReason == agentcore.StopReasonLength ||
			msg.StopReason == agentcore.StopReasonToolUse:
			h.appendAssistant(conv, msg)
			entry := h.lastEntry(conv)
			h.finishAnswered(conv, taskID, entry)
			return

		default:
			detail := msg.ErrorMessage
			if detail == "" {
				detail = fmt.Sprintf("Model response ended with stop reason %s", msg.StopReason)
			}
			h.failRun(conv, taskID, ReasonModelError, detail, true)
			return
		}
	}
}

func (h *Harness) isDead() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dead
}

func (h *Harness) aliveLocked() bool { return !h.dead && !h.closed }

func formatTurns(n int) string {
	if n == 1 {
		return "1 turn"
	}
	return fmt.Sprintf("%d turns", n)
}

func (h *Harness) runTurns(conv ConversationID, taskID TaskID) int {
	live := h.loadLive(conv)
	if live.Run != nil && live.Run.TaskID == taskID {
		return live.Run.Turns
	}
	return 0
}

func (h *Harness) sleepUntil(ctx context.Context, until int64) bool {
	for {
		now := h.now()
		if now >= until {
			return true
		}
		d := time.Duration(until-now) * time.Millisecond
		if d > time.Second {
			d = time.Second
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
		if h.isDead() {
			return false
		}
	}
}

func (h *Harness) appendPendingSystem(conv ConversationID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.store.GetDoc(conv, docAgent, "instructions-pending"); !ok {
		return
	}
	state := h.loadAgent(conv)
	h.store.DeleteDoc(conv, docAgent, "instructions-pending")
	if state.Instructions == "" {
		return
	}
	h.store.AppendEntry(conv, EntryRecord{
		Kind: "pi.system",
		Data: rawJSON(map[string]string{"text": state.Instructions}),
	})
	h.notifyLocked()
}

func (h *Harness) appendAssistant(conv ConversationID, msg agentcore.AssistantMessage) EntryRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.store.AppendEntry(conv, EntryRecord{Kind: "pi.assistant", Model: []agentcore.Message{msg}})
	h.notifyLocked()
	return entry
}

func (h *Harness) lastEntry(conv ConversationID) EntryID {
	entries := h.store.Entries(conv)
	if len(entries) == 0 {
		return 0
	}
	return entries[len(entries)-1].ID
}

func (h *Harness) modelFor(conv ConversationID) ModelRef {
	state := h.loadAgent(conv)
	if state.Model != nil {
		return *state.Model
	}
	return h.model
}

func (h *Harness) modelAvailable(ref ModelRef) bool {
	if len(h.available) == 0 {
		return ref.Provider != "" || ref.ModelID != ""
	}
	return h.available[ref.Provider+"/"+ref.ModelID]
}

func (h *Harness) offeredTools(conv ConversationID) []*Tool {
	state := h.loadAgent(conv)
	if state.Tools == nil {
		return append([]*Tool(nil), h.toolOrder...)
	}
	var out []*Tool
	for _, name := range *state.Tools {
		if tool, ok := h.tools[name]; ok {
			out = append(out, tool)
		}
	}
	return out
}

// finishAnswered settles the run's inputs done and places the final boundary.
func (h *Harness) finishAnswered(conv ConversationID, taskID TaskID, answer EntryID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dead || h.closed {
		return
	}
	if _, ok := h.store.GetTask(taskID); !ok {
		return
	}
	live := h.loadLive(conv)
	boundary := h.applyBoundaryLocked(conv, h.loadInbox(conv), "final")
	var inputs []SubmissionID
	if live.Run != nil && live.Run.TaskID == taskID {
		inputs = append([]SubmissionID(nil), live.Run.Inputs...)
	}
	live.Run = nil
	live.Generation = nil
	live.Tools = nil
	live.NestedTools = nil
	h.saveLive(conv, live)
	// Publish the end of the run before settling its inputs, as one PI commit.
	for _, id := range inputs {
		h.settleSubmissionLocked(id, SubmissionDone, "", "", 0, answer)
	}
	_, _ = h.store.UpdateTask(taskID, func(t *TaskRecord) {
		t.State.Status = TaskTerminal
		t.EndedAt = h.now()
		t.State.Checkpoint = nil
		t.State.Outcome = &Outcome{Status: OutcomeCompleted, Result: rawJSON(map[string]any{"entryId": answer})}
	})
	if len(boundary.Users) > 0 {
		h.startRunLocked(conv, emptyJSON(), boundary.Users, boundary.First, false)
	}
	h.notifyLocked()
}

// failRun ends a run without an answer and applies afterFailedRun.
func (h *Harness) failRun(conv ConversationID, taskID TaskID, reason, detail string, applyPolicy bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dead || h.closed {
		return
	}
	live := h.loadLive(conv)
	if live.Run != nil && live.Run.TaskID == taskID {
		inputs := append([]SubmissionID(nil), live.Run.Inputs...)
		settleDetail := detail
		if reason == ReasonNoModel || reason == ReasonAborted || reason == ReasonReset {
			// The settlement carries the reason; only the live failure carries
			// the extra detail for these.
			settleDetail = ""
		}
		// Publish the failure before settling its inputs, so a waiter that wakes
		// on the settlement also sees pi.live.runFailure (one PI commit).
		live.RunFailure = &RunFailure{
			TaskID: taskID,
			Inputs: inputs,
			Reason: reason,
			Detail: detail,
			First:  live.Run.First,
		}
		live.Run = nil
		live.Generation = nil
		live.Tools = nil
		live.NestedTools = nil
		h.saveLive(conv, live)
		for _, id := range inputs {
			h.settleSubmissionLocked(id, SubmissionUnanswered, reason, settleDetail, 0, 0)
		}
	}
	_, _ = h.store.UpdateTask(taskID, func(t *TaskRecord) {
		t.State.Status = TaskTerminal
		t.EndedAt = h.now()
		t.State.Checkpoint = nil
		t.State.Outcome = &Outcome{
			Status: OutcomeFailed,
			Error: &TaskError{
				Message: detail,
				Detail:  rawJSON(map[string]string{"reason": reason}),
			},
		}
	})
	if applyPolicy {
		switch h.settings.AfterFailedRun {
		case AfterFailedWithdraw:
			h.withdrawQueuedInputsLocked(conv, ReasonRunFailed)
		case AfterFailedContinue:
			res := h.applyBoundaryLocked(conv, h.loadInbox(conv), "final")
			if len(res.Users) > 0 {
				h.startRunLocked(conv, emptyJSON(), res.Users, res.First, true)
			}
		}
	}
	h.notifyLocked()
}

// endRunAtReset ends a run the queued reset cut before an answer and starts the
// next run with the inputs the reset boundary placed, keeping the failure.
func (h *Harness) endRunAtReset(conv ConversationID, taskID TaskID, assistant EntryID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dead || h.closed {
		return
	}
	live := h.loadLive(conv)
	boundary := h.applyBoundaryLocked(conv, h.loadInbox(conv), "final")
	var inputs []SubmissionID
	var first EntryID
	if live.Run != nil && live.Run.TaskID == taskID {
		inputs = append([]SubmissionID(nil), live.Run.Inputs...)
		first = live.Run.First
		live.RunFailure = &RunFailure{
			TaskID: taskID, Inputs: inputs, Reason: ReasonReset, Detail: "", First: first,
		}
	}
	live.Run = nil
	live.Generation = nil
	live.Tools = nil
	live.NestedTools = nil
	h.saveLive(conv, live)
	for _, id := range inputs {
		h.settleSubmissionLocked(id, SubmissionUnanswered, ReasonReset, "", 0, 0)
	}
	_, _ = h.store.UpdateTask(taskID, func(t *TaskRecord) {
		t.State.Status = TaskTerminal
		t.EndedAt = h.now()
		t.State.Checkpoint = nil
		t.State.Outcome = &Outcome{Status: OutcomeCompleted, Result: rawJSON(map[string]any{"entryId": assistant})}
	})
	if len(boundary.Users) > 0 {
		h.startRunLocked(conv, emptyJSON(), boundary.Users, boundary.First, true)
	}
	h.notifyLocked()
}

// abortRun ends a run as aborted after its context was cancelled.
func (h *Harness) abortRun(conv ConversationID, taskID TaskID) {
	h.abortOwnedTools(taskID)
	h.failRun(conv, taskID, ReasonAborted, "", false)
}

// applyPostToolsBoundary places the steers a round's boundary selects; they
// join the run that continues in the next generation.
func (h *Harness) applyPostToolsBoundary(conv ConversationID, taskID TaskID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	live := h.loadLive(conv)
	res := h.applyBoundaryLocked(conv, h.loadInbox(conv), "postTools")
	if len(res.Users) > 0 && live.Run != nil && live.Run.TaskID == taskID {
		live.Run.Inputs = append(live.Run.Inputs, res.Users...)
		h.saveLive(conv, live)
	}
	h.notifyLocked()
}

// handOverGeneration creates the next round's generation and marks this one
// complete.
func (h *Harness) handOverGeneration(conv ConversationID, taskID TaskID, assistant EntryID, handoff string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	live := h.loadLive(conv)
	if handoff != "" {
		entry := h.appendEntryLocked(conv, EntryDraft{
			Kind: "pi.reset", Head: "self",
			Model: contentMessages(rawJSON(handoff), h.now()),
		})
		assistant = entry.ID
	}
	if live.Run != nil && live.Run.TaskID == taskID {
		live.Run.TaskID = ""
	}
	_, _ = h.store.UpdateTask(taskID, func(t *TaskRecord) {
		t.State.Status = TaskTerminal
		t.EndedAt = h.now()
		t.State.Checkpoint = nil
		t.State.Outcome = &Outcome{Status: OutcomeCompleted, Result: rawJSON(map[string]any{"entryId": assistant})}
	})
	var inputs []SubmissionID
	first := EntryID(0)
	turns := 0
	if live.Run != nil && live.Run.TaskID == "" {
		inputs = live.Run.Inputs
		first = live.Run.First
		turns = live.Run.Turns
	}
	live.Tools = nil
	live.NestedTools = nil
	h.saveLive(conv, live)
	next := TaskRecord{
		ConversationID: conv, Kind: GenerationTaskKind, Version: 2,
		Input:     emptyJSON(),
		State:     TaskState{Status: TaskPending, Checkpoint: rawJSON(GenerationCheckpoint{Phase: "prepare", Attempt: 1})},
		StartedAt: h.now(),
	}
	created, _ := h.store.CreateTask(next)
	live = h.loadLive(conv)
	live.Run = &LiveRun{TaskID: created.ID, Inputs: inputs, Turns: turns, First: first}
	live.Generation = &GenerationLive{Attempt: 1}
	h.saveLive(conv, live)
	runCtx, cancel := context.WithCancel(context.Background())
	h.running[created.ID] = cancel
	h.spawnGeneration(conv, created.ID, runCtx)
	h.notifyLocked()
}

func toolCallsOf(msg agentcore.AssistantMessage) []ToolCall {
	var out []ToolCall
	for _, c := range msg.ToolCalls() {
		out = append(out, ToolCall{Type: "toolCall", ID: c.ID, Name: c.Name, Arguments: cloneRaw(c.Arguments)})
	}
	return out
}

// isRetryableAssistantError mirrors PI's pi-ai retry classifier.
func isRetryableAssistantError(msg agentcore.AssistantMessage) bool {
	if msg.StopReason != agentcore.StopReasonError || msg.ErrorMessage == "" {
		return false
	}
	if nonRetryableErrorPattern.MatchString(msg.ErrorMessage) {
		return false
	}
	return retryableErrorPattern.MatchString(msg.ErrorMessage)
}

var nonRetryableErrorPattern = regexp.MustCompile(`(?i)(GoUsageLimitError|FreeUsageLimitError|Monthly usage limit reached|available balance|insufficient_quota|out of budget|quota exceeded|billing|subscription_sharing_usage_limit_exceeded|"code"\s*:\s*"?1308)`)

var retryableErrorPattern = regexp.MustCompile(`(?i)(overloaded|server_busy|servers are currently busy|currently experiencing high demand|model is at capacity|rate.?limit|too many requests|\b(?:429|500|502|503|504|520|524)\b|service.?(?:is.?)?(?:temporarily.?)?unavailable|server.?error|internal.?error|provider.?returned.?(?:an.?)?error|exceeded request buffer limit while retrying upstream|unable to forward upstream|upstream.?request.?failed|upstream_stream_read_error|stream was interrupted|http/?2 stream failed|network.?error|connection.?error|connection.?refused|connection.?lost|other side closed|fetch failed|request to \S+ failed, reason:|getaddrinfo|ENOTFOUND)`) //nolint:lll
