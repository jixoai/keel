package durable

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/jixoai/keel/internal/agentcore"
)

// Document kinds.
const (
	docInbox  = "pi.inbox"
	docLive   = "pi.live"
	docAgent  = "pi.agent"
	docNested = "pi.tool.nested-call"
)

// Request is one model request. Messages use the durable context shape so a
// system message (an instructions delta) can appear where PI puts it.
type Request struct {
	ConversationID ConversationID
	Messages       []ContextMessage
	Tools          []*Tool
	Model          ModelRef
	GenerationID   string
	Attempt        int
}

// ContextMessage is one model-context message. Role is "user", "assistant",
// "toolResult" or "system".
type ContextMessage struct {
	Role       string                `json:"role"`
	Content    agentcore.ContentList `json:"content,omitempty"`
	Timestamp  int64                 `json:"timestamp,omitempty"`
	ToolCallID string                `json:"toolCallId,omitempty"`
	ToolName   string                `json:"toolName,omitempty"`
	IsError    bool                  `json:"isError,omitempty"`
}

// Text flattens the message's text content.
func (m ContextMessage) Text() string {
	out := ""
	for _, c := range m.Content {
		if t, ok := c.(agentcore.TextContent); ok {
			out += t.Text
		}
	}
	return out
}

// Provider is the model seam. A generation attempt is idempotent per
// GenerationID: a provider may use it to avoid a duplicate request.
type Provider interface {
	Complete(ctx context.Context, req Request) (agentcore.AssistantMessage, error)
}

// Options configures a Harness.
type Options struct {
	Store    Store
	Settings Settings
	Tools    []*Tool
	Hooks    []*Hook
	Provider Provider
	// Model is the default model ref.
	Model ModelRef
	// AvailableModels, when non-nil, restricts which refs resolve. A model not
	// in the list fails the run with no_model.
	AvailableModels []ModelRef
	Now             func() int64
	OnReport        func(error)
}

// Harness is a durable agent harness over one Store. It holds no conversation
// state of its own beyond running goroutines and cancellation: entries, inbox,
// live run control, tasks and submissions all live in the Store.
type Harness struct {
	store     Store
	settings  Settings
	tools     map[string]*Tool
	toolOrder []*Tool
	hooks     []*Hook
	provider  Provider
	model     ModelRef
	available map[string]bool // empty means every model is available
	now       func() int64
	onReport  func(error)

	mu       sync.Mutex
	closed   bool
	dead     bool
	running  map[TaskID]context.CancelFunc
	notifyCh *notifier
}

// NewHarness builds a harness. Without a store it creates an in-memory one and
// a root conversation.
func NewHarness(opts Options) *Harness {
	store := opts.Store
	if store == nil {
		store = NewMemStore()
	}
	h := &Harness{
		store:     store,
		settings:  ResolveSettings(opts.Settings),
		tools:     map[string]*Tool{},
		hooks:     opts.Hooks,
		provider:  opts.Provider,
		model:     opts.Model,
		now:       opts.Now,
		onReport:  opts.OnReport,
		running:   map[TaskID]context.CancelFunc{},
		notifyCh:  newNotifier(),
		available: map[string]bool{},
	}
	for _, tool := range opts.Tools {
		h.tools[tool.Name] = tool
		h.toolOrder = append(h.toolOrder, tool)
	}
	for _, ref := range opts.AvailableModels {
		h.available[ref.Provider+"/"+ref.ModelID] = true
	}
	if h.now == nil {
		h.now = nowMillis
	}
	store.CreateConversation(RootConversationID)
	return h
}

// Store exposes the persistence seam (tests inspect tasks and submissions).
func (h *Harness) Store() Store { return h.store }

// Settings returns the resolved settings.
func (h *Harness) Settings() Settings { return h.settings }

// Close stops scheduling and rejects pending waits. It does not settle live
// tasks: a crash is a crash.
func (h *Harness) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for id, cancel := range h.running {
		cancel()
		delete(h.running, id)
	}
	h.notifyLocked()
}

// Kill simulates the host dying: scheduling stops, live tasks stay as they were
// committed, and later storage calls from the dead harness do nothing. A new
// Harness over the same Store recovers them.
func (h *Harness) Kill() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dead = true
	h.closed = true
	for id, cancel := range h.running {
		cancel()
		delete(h.running, id)
	}
	h.notifyLocked()
}

// Closed reports whether the harness was closed or killed.
func (h *Harness) Closed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// Root returns the reserved root conversation.
func (h *Harness) Root() *Conversation { return &Conversation{h: h, id: RootConversationID} }

// Conversation returns a handle for an existing conversation id.
func (h *Harness) Conversation(id ConversationID) *Conversation {
	return &Conversation{h: h, id: id}
}

// CreateConversation registers and returns a new ownerless conversation.
func (h *Harness) CreateConversation() *Conversation {
	h.mu.Lock()
	defer h.mu.Unlock()
	var n int
	for _, id := range h.store.Conversations() {
		if len(id) > 7 && string(id)[:7] == "conv-" {
			n++
		}
	}
	id := ConversationID(fmt.Sprintf("conv-%d", n+1))
	h.store.CreateConversation(id)
	return &Conversation{h: h, id: id}
}

func (h *Harness) notifyLocked() { h.notifyCh.notify() }

func (h *Harness) notify() { h.notifyCh.notify() }

func (h *Harness) checkAliveLocked() error {
	if h.dead || h.closed {
		return ErrHarnessClosed{}
	}
	return nil
}

// report sends a non-fatal failure to the host.
func (h *Harness) report(err error) {
	if err != nil && h.onReport != nil {
		h.onReport(err)
	}
}

func (h *Harness) mustBeAlive() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.checkAliveLocked()
}

// wake waits for the next state change.
func (h *Harness) wake(ctx context.Context) error {
	return h.notifyCh.wait(ctx)
}

// IsBusy reports whether a run is live.
func (h *Harness) IsBusy(conv *Conversation) bool {
	live := h.loadLive(conv.id)
	return live.Run != nil
}

// WaitForIdle waits until the conversation has no live run and no live tasks.
func (h *Harness) WaitForIdle(ctx context.Context, conv *Conversation) error {
	for {
		ch := h.notifyCh.current()
		if h.Closed() {
			return ErrHarnessClosed{}
		}
		if !h.hasLiveWork(conv.id) {
			return nil
		}
		if err := waitChan(ctx, ch); err != nil {
			return err
		}
	}
}

func (h *Harness) hasLiveWork(conv ConversationID) bool {
	if live := h.loadLive(conv); live.Run != nil {
		return true
	}
	for _, task := range h.store.ListTasks(conv) {
		if task.Live() {
			return true
		}
	}
	return false
}

// ---- documents ----

func (h *Harness) loadLive(conv ConversationID) LiveState {
	raw, ok := h.store.GetDoc(conv, docLive, "")
	if !ok {
		return LiveState{}
	}
	var live LiveState
	if err := json.Unmarshal(raw, &live); err != nil {
		return LiveState{}
	}
	return live
}

func (h *Harness) saveLive(conv ConversationID, live LiveState) {
	h.store.SetDoc(conv, docLive, "", marshalDoc(live))
}

func (h *Harness) loadInbox(conv ConversationID) []InboxItem {
	raw, ok := h.store.GetDoc(conv, docInbox, "")
	if !ok {
		return nil
	}
	var items []InboxItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	return items
}

func (h *Harness) saveInbox(conv ConversationID, items []InboxItem) {
	if items == nil {
		items = []InboxItem{}
	}
	h.store.SetDoc(conv, docInbox, "", marshalDoc(items))
}

func (h *Harness) loadAgent(conv ConversationID) AgentState {
	raw, ok := h.store.GetDoc(conv, docAgent, "")
	if !ok {
		return AgentState{}
	}
	var state AgentState
	if err := json.Unmarshal(raw, &state); err != nil {
		return AgentState{}
	}
	return state
}

func (h *Harness) saveAgent(conv ConversationID, state AgentState) {
	h.store.SetDoc(conv, docAgent, "", marshalDoc(state))
}

func (h *Harness) loadNested(conv ConversationID, parent TaskID, key string) (NestedCallDoc, bool) {
	raw, ok := h.store.GetDoc(conv, docNested, string(parent)+"/"+key)
	if !ok {
		return NestedCallDoc{}, false
	}
	var doc NestedCallDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return NestedCallDoc{}, false
	}
	return doc, true
}

func (h *Harness) saveNested(conv ConversationID, parent TaskID, key string, doc NestedCallDoc) {
	h.store.SetDoc(conv, docNested, string(parent)+"/"+key, marshalDoc(doc))
}

func (h *Harness) deleteNested(conv ConversationID, parent TaskID, key string) {
	h.store.DeleteDoc(conv, docNested, string(parent)+"/"+key)
}

// ---- context ----

// Context returns the model context of a conversation: the active range from
// the newest head marker, with edits applied.
func (h *Harness) Context(conv *Conversation) []ContextMessage {
	entries := h.store.Entries(conv.id)
	start := 0
	for i, e := range entries {
		if e.Head {
			start = i
		}
	}
	omitted := map[EntryID]bool{}
	for _, e := range entries[start:] {
		for _, edit := range e.Edits {
			if edit.Action == "omit" || edit.Action == "replace" {
				omitted[edit.Target] = true
			}
		}
	}
	var out []ContextMessage
	for _, e := range entries[start:] {
		if omitted[e.ID] {
			continue
		}
		if e.Kind == "pi.system" {
			var data struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(e.Data, &data)
			out = append(out, ContextMessage{
				Role:    "system",
				Content: agentcore.ContentList{agentcore.NewTextContent(data.Text)},
			})
			continue
		}
		for _, m := range e.Model {
			// A failed or aborted answer is not model context (PI excludes
			// non-answer stop reasons); a continue sees the user message before it.
			if assistant, ok := m.(agentcore.AssistantMessage); ok &&
				(assistant.StopReason == agentcore.StopReasonError || assistant.StopReason == agentcore.StopReasonAborted) {
				continue
			}
			out = append(out, contextMessageOf(m))
		}
	}
	return fillMissingToolResults(out)
}

func contextMessageOf(m agentcore.Message) ContextMessage {
	switch msg := m.(type) {
	case agentcore.UserMessage:
		return ContextMessage{Role: agentcore.RoleUser, Content: msg.Content, Timestamp: msg.Timestamp}
	case agentcore.AssistantMessage:
		return ContextMessage{Role: agentcore.RoleAssistant, Content: msg.Content, Timestamp: msg.Timestamp}
	case agentcore.ToolResultMessage:
		return ContextMessage{
			Role: agentcore.RoleToolResult, Content: msg.Content, Timestamp: msg.Timestamp,
			ToolCallID: msg.ToolCallID, ToolName: msg.ToolName, IsError: msg.IsError,
		}
	case agentcore.CompactionMessage:
		return ContextMessage{Role: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent(msg.Summary)}}
	default:
		return ContextMessage{Role: m.Role()}
	}
}

// fillMissingToolResults gives every unanswered tool call a synthetic error
// result, so a context cut off after tool calls still ends in tool results.
func fillMissingToolResults(messages []ContextMessage) []ContextMessage {
	out := make([]ContextMessage, 0, len(messages))
	pending := map[string]string{} // call id -> name
	flush := func() {
		for id, name := range pending {
			out = append(out, ContextMessage{
				Role:       agentcore.RoleToolResult,
				ToolCallID: id,
				ToolName:   name,
				IsError:    true,
				Content: agentcore.ContentList{agentcore.NewTextContent(
					fmt.Sprintf("[error] Tool call %s was not executed", id))},
			})
		}
		pending = map[string]string{}
	}
	for _, m := range messages {
		switch m.Role {
		case agentcore.RoleAssistant:
			flush()
			for _, c := range m.Content {
				if tc, ok := c.(agentcore.ToolCallContent); ok {
					pending[tc.ID] = tc.Name
				}
			}
			out = append(out, m)
		case agentcore.RoleToolResult:
			delete(pending, m.ToolCallID)
			out = append(out, m)
		default:
			flush()
			out = append(out, m)
		}
	}
	flush()
	return out
}

func lastNonSystem(messages []ContextMessage) *ContextMessage {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "system" {
			return &messages[i]
		}
	}
	return nil
}

// Entries returns a conversation's committed entries.
func (h *Harness) Entries(conv *Conversation) []EntryRecord { return h.store.Entries(conv.id) }

// ---- conversation ----

// Conversation is a stateless handle for one transcript scope.
type Conversation struct {
	h  *Harness
	id ConversationID
}

// ID returns the conversation id.
func (c *Conversation) ID() ConversationID { return c.id }

// Context returns the model context.
func (c *Conversation) Context() []ContextMessage { return c.h.Context(c) }

// Entries returns committed entries.
func (c *Conversation) Entries() []EntryRecord { return c.h.Entries(c) }

// Configure stores an agent change. Fields with their zero value clear nothing
// they do not name: callers pass the whole desired state (PI's configure is a
// per-field change; the tests only set model/tools/instructions).
func (c *Conversation) Configure(ctx context.Context, state AgentState) error {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	if err := c.h.checkAliveLocked(); err != nil {
		return err
	}
	c.h.saveAgent(c.id, state)
	c.h.notifyLocked()
	return nil
}

// SetInstructions changes the instructions and records a system entry before
// the next request, as PI's pi.system delta does.
func (c *Conversation) SetInstructions(ctx context.Context, instructions string) error {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	if err := c.h.checkAliveLocked(); err != nil {
		return err
	}
	state := c.h.loadAgent(c.id)
	state.Instructions = instructions
	c.h.saveAgent(c.id, state)
	c.h.store.SetDoc(c.id, docAgent, "instructions-pending", []byte("1"))
	c.h.notifyLocked()
	return nil
}

// Submission is an awaitable handle for one durable submission.
type Submission struct {
	h  *Harness
	id SubmissionID
}

// ID returns the submission id.
func (s *Submission) ID() SubmissionID { return s.id }

// Status reads the current record.
func (s *Submission) Status() (SubmissionRecord, error) {
	rec, ok := s.h.store.GetSubmission(s.id)
	if !ok {
		return SubmissionRecord{}, ErrSubmissionNotFound{ID: s.id}
	}
	return rec, nil
}

// Wait blocks until the submission is done or unanswered.
func (s *Submission) Wait(ctx context.Context) (SubmissionRecord, error) {
	for {
		ch := s.h.notifyCh.current()
		rec, ok := s.h.store.GetSubmission(s.id)
		if !ok {
			return SubmissionRecord{}, ErrSubmissionNotFound{ID: s.id}
		}
		if rec.Settled() {
			return rec, nil
		}
		if s.h.Closed() {
			return rec, ErrHarnessClosed{}
		}
		if err := waitChan(ctx, ch); err != nil {
			return rec, err
		}
	}
}

// Abort withdraws a queued submission; placed and settled submissions are
// reported instead.
func (s *Submission) Abort(ctx context.Context) (AbortResult, error) {
	return s.h.abortSubmission(ctx, s.id, "")
}

// Abort aborts the conversation: withdraws queued inputs, marks live
// generation and tool tasks aborted, and waits for the run to end.
func (c *Conversation) Abort(ctx context.Context) error {
	return c.h.abortConversation(ctx, c.id)
}

// Submission reacquires a submission by id (after a reopen, for example).
func (h *Harness) Submission(id SubmissionID) (*Submission, bool) {
	if _, ok := h.store.GetSubmission(id); !ok {
		return nil, false
	}
	return &Submission{h: h, id: id}, true
}

// Inbox returns the conversation's queued items.
func (h *Harness) Inbox(conv *Conversation) []InboxItem { return h.loadInbox(conv.id) }

// Live returns the conversation's live document.
func (h *Harness) Live(conv *Conversation) LiveState { return h.loadLive(conv.id) }

// Task reads one task record.
func (h *Harness) Task(id TaskID) (TaskRecord, bool) { return h.store.GetTask(id) }

// Tasks returns every task of a conversation.
func (h *Harness) Tasks(conv *Conversation) []TaskRecord { return h.store.ListTasks(conv.id) }

// Reset admits a pi.reset write: a new context, with optional handoff text.
func (c *Conversation) Reset(ctx context.Context, handoff string) error {
	draft := SubmissionDraft{Type: SubmissionWrite, Write: resetDraft(handoff, c.h.now())}
	_, err := c.h.admit(ctx, c.id, draft)
	return err
}

// Submit admits an input/write/continue submission.
func (c *Conversation) Submit(ctx context.Context, draft SubmissionDraft) (*Submission, error) {
	rec, err := c.h.admit(ctx, c.id, draft)
	if err != nil {
		return nil, err
	}
	return &Submission{h: c.h, id: rec.ID}, nil
}

// Committed runs whose result a caller wants to reuse (generation idempotency).
func (h *Harness) generationResult(task TaskRecord) (EntryID, bool) {
	if task.State.Outcome == nil || task.State.Outcome.Status != OutcomeCompleted {
		return 0, false
	}
	var result struct {
		EntryID EntryID `json:"entryId"`
	}
	if err := json.Unmarshal(task.State.Outcome.Result, &result); err != nil {
		return 0, false
	}
	return result.EntryID, true
}
