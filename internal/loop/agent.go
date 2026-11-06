// Stateful Agent wrapper ported from pi's packages/agent/src/agent.ts
// (pin 55a5c69, MIT License), narrowed to the lifecycle behaviours Keel needs:
// transcript-owned state, queueing, abort, reset, and awaited subscribers.
//
// Agent owns the current transcript, drives the low-level loop through
// StartRun/StartContinue, reduces the emitted events into state, and awaits
// every subscriber synchronously. `agent_end` is the last loop event, but the
// agent becomes idle only after its subscribers settle.

package loop

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
)

// QueueMode controls how many queued user messages are injected at a queue
// drain point.
type QueueMode string

const (
	// QueueAll drains every queued message at that point.
	QueueAll QueueMode = "all"
	// QueueOneAtATime drains only the oldest queued message, leaving the rest
	// for later drain points.
	QueueOneAtATime QueueMode = "one-at-a-time"
)

// Agent errors, worded exactly like pi's Agent so a caller (or a test ported
// from pi) sees the same contract.
var (
	ErrAgentBusyPrompt          = errors.New("Agent is already processing a prompt. Use steer() or followUp() to queue messages, or wait for completion.")
	ErrAgentBusyContinue        = errors.New("Agent is already processing. Wait for completion before continuing.")
	ErrAgentBusyReset           = errors.New("Agent is already processing. Wait for completion before resetting.")
	ErrAgentBusy                = errors.New("Agent is already processing.")
	ErrNoMessagesToContinueFrom = errors.New("No messages to continue from")
)

// AgentInitialState seeds an Agent. SystemPrompt and Tools become the leading
// system message unless Messages already starts with one.
type AgentInitialState struct {
	SystemPrompt  string
	Model         string
	ThinkingLevel agentcore.ThinkingLevel
	Tools         []agentcore.AgentTool
	Messages      agentcore.MessageList
}

// AgentOptions configures an Agent.
type AgentOptions struct {
	InitialState AgentInitialState
	// LoopConfig carries the model, provider, stream, API key, transform and
	// conversion hooks. Model/ThinkingLevel are overridden per run from state.
	LoopConfig LoopConfig
	// Batch carries the tool registry and prepare/before/after hooks.
	Batch agenttool.BatchConfig

	// FinishTurn, PrepareRequest, PrepareNextTurn, OnBeforeSettle and
	// EmitSettleEvents are forwarded to every loop run.
	FinishTurn       func(ctx context.Context, turn AgentTurnContext) *AgentTurnDecision
	PrepareRequest   func(ctx context.Context, req PrepareRequestContext) *RequestUpdate
	PrepareNextTurn  func(ctx context.Context, agentCtx *agentcore.AgentContext) *TurnUpdate
	OnBeforeSettle   func(ctx context.Context, agentCtx *agentcore.AgentContext) bool
	EmitSettleEvents bool
	EventBuffer      int
	SessionID        string

	// SteeringMode / FollowUpMode default to one-at-a-time.
	SteeringMode QueueMode
	FollowUpMode QueueMode
}

// AgentState is a snapshot of the agent's public state.
type AgentState struct {
	SystemPrompt     string
	Model            string
	ThinkingLevel    agentcore.ThinkingLevel
	Tools            []agentcore.AgentTool
	Messages         agentcore.MessageList
	IsStreaming      bool
	StreamingMessage agentcore.AgentMessage
	PendingToolCalls map[string]struct{}
	ErrorMessage     string
}

// Agent is a stateful wrapper around the low-level loop.
type Agent struct {
	mu    sync.Mutex
	state AgentState

	opts AgentOptions

	steering *messageQueue
	followUp *messageQueue

	listeners map[int64]func(agentcore.AgentEvent, context.Context)
	nextSubID int64

	active *agentActiveRun
}

type agentActiveRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	idle   chan struct{}
}

// messageQueue is pi's PendingMessageQueue.
type messageQueue struct {
	mu    sync.Mutex
	mode  QueueMode
	items []agentcore.AgentMessage
}

func newMessageQueue(mode QueueMode) *messageQueue {
	if mode != QueueAll {
		mode = QueueOneAtATime
	}
	return &messageQueue{mode: mode}
}

func (q *messageQueue) enqueue(m agentcore.AgentMessage) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, m)
}

func (q *messageQueue) peek() []agentcore.AgentMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.mode == QueueAll {
		return append([]agentcore.AgentMessage{}, q.items...)
	}
	if len(q.items) == 0 {
		return nil
	}
	return []agentcore.AgentMessage{q.items[0]}
}

func (q *messageQueue) drain() []agentcore.AgentMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.items)
	if q.mode != QueueAll && n > 1 {
		n = 1
	}
	out := append([]agentcore.AgentMessage{}, q.items[:n]...)
	q.items = append([]agentcore.AgentMessage{}, q.items[n:]...)
	return out
}

func (q *messageQueue) clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = nil
}

func (q *messageQueue) hasItems() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) > 0
}

func (q *messageQueue) setMode(mode QueueMode) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if mode == QueueAll {
		q.mode = QueueAll
	} else {
		q.mode = QueueOneAtATime
	}
}

// NewAgent builds an agent from options, seeding the leading system message the
// way pi's createMutableAgentState does.
func NewAgent(opts AgentOptions) *Agent {
	state := AgentState{
		SystemPrompt:     opts.InitialState.SystemPrompt,
		Model:            opts.InitialState.Model,
		ThinkingLevel:    opts.InitialState.ThinkingLevel,
		Tools:            append([]agentcore.AgentTool{}, opts.InitialState.Tools...),
		Messages:         append(agentcore.MessageList{}, opts.InitialState.Messages...),
		PendingToolCalls: map[string]struct{}{},
	}
	if state.Model == "" {
		state.Model = opts.LoopConfig.Model
	}
	if state.ThinkingLevel == "" {
		state.ThinkingLevel = opts.LoopConfig.ThinkingLevel
	}
	if state.ThinkingLevel == "" {
		state.ThinkingLevel = agentcore.ThinkingOff
	}
	initial := agentcore.SystemMessage{
		RoleField:  agentcore.RoleSystem,
		Content:    state.SystemPrompt,
		ToolsAdded: agentcore.ToolDeclarations(state.Tools),
		Timestamp:  0,
	}
	if len(state.Messages) == 0 || state.Messages[0].Role() != agentcore.RoleSystem {
		if initial.Content != "" || len(initial.ToolsAdded) > 0 {
			state.Messages = append(agentcore.MessageList{initial}, state.Messages...)
		}
	}
	if opts.Batch.Registry == nil && len(state.Tools) > 0 {
		reg := agenttool.NewToolRegistry()
		for _, tool := range state.Tools {
			if err := reg.Register(tool); err == nil {
				opts.Batch.Registry = reg
			}
		}
	}
	return &Agent{
		state:     state,
		opts:      opts,
		steering:  newMessageQueue(opts.SteeringMode),
		followUp:  newMessageQueue(opts.FollowUpMode),
		listeners: make(map[int64]func(agentcore.AgentEvent, context.Context)),
	}
}

// State returns a copy of the current state.
func (a *Agent) State() AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.state
	out.Tools = append([]agentcore.AgentTool{}, a.state.Tools...)
	out.Messages = append(agentcore.MessageList{}, a.state.Messages...)
	out.PendingToolCalls = make(map[string]struct{}, len(a.state.PendingToolCalls))
	for id := range a.state.PendingToolCalls {
		out.PendingToolCalls[id] = struct{}{}
	}
	return out
}

// SetModel replaces the active model for future turns.
func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	a.state.Model = model
	a.mu.Unlock()
}

// SetThinkingLevel replaces the requested reasoning level for future turns.
func (a *Agent) SetThinkingLevel(level agentcore.ThinkingLevel) {
	a.mu.Lock()
	a.state.ThinkingLevel = level
	a.mu.Unlock()
}

// SetTools replaces the executable tool set (the top-level array is copied).
func (a *Agent) SetTools(tools []agentcore.AgentTool) {
	a.mu.Lock()
	a.state.Tools = append([]agentcore.AgentTool{}, tools...)
	a.mu.Unlock()
}

// SetMessages replaces the transcript (the top-level array is copied).
func (a *Agent) SetMessages(messages []agentcore.AgentMessage) {
	a.mu.Lock()
	a.state.Messages = append(agentcore.MessageList{}, messages...)
	a.mu.Unlock()
}

// Subscribe registers a listener and returns an unsubscribe function. Listener
// calls are awaited in subscription order and are part of run settlement.
func (a *Agent) Subscribe(listener func(agentcore.AgentEvent, context.Context)) func() {
	a.mu.Lock()
	id := a.nextSubID
	a.nextSubID++
	a.listeners[id] = listener
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		delete(a.listeners, id)
		a.mu.Unlock()
	}
}

// Steer queues a message to be injected after the current assistant turn.
func (a *Agent) Steer(message agentcore.AgentMessage) { a.steering.enqueue(message) }

// FollowUp queues a message to run only after the agent would otherwise stop.
func (a *Agent) FollowUp(message agentcore.AgentMessage) { a.followUp.enqueue(message) }

// ClearSteeringQueue removes all queued steering messages.
func (a *Agent) ClearSteeringQueue() { a.steering.clear() }

// ClearFollowUpQueue removes all queued follow-up messages.
func (a *Agent) ClearFollowUpQueue() { a.followUp.clear() }

// ClearAllQueues removes all queued steering and follow-up messages.
func (a *Agent) ClearAllQueues() {
	a.ClearSteeringQueue()
	a.ClearFollowUpQueue()
}

// HasQueuedMessages reports whether either queue still has pending messages.
func (a *Agent) HasQueuedMessages() bool {
	return a.steering.hasItems() || a.followUp.hasItems()
}

// PeekQueuedMessages previews the messages selected for the next turn without
// consuming them.
func (a *Agent) PeekQueuedMessages() []agentcore.AgentMessage {
	if steering := a.steering.peek(); len(steering) > 0 {
		return steering
	}
	return a.followUp.peek()
}

// SteerMode sets how steering messages are drained.
func (a *Agent) SteerMode(mode QueueMode) { a.steering.setMode(mode) }

// FollowUpMode sets how follow-up messages are drained.
func (a *Agent) FollowUpMode(mode QueueMode) { a.followUp.setMode(mode) }

// Abort cancels the active run, if any.
func (a *Agent) Abort() {
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if active != nil {
		active.cancel()
	}
}

// WaitForIdle returns a channel closed when the active run and all awaited
// listeners have finished. It is already closed when the agent is idle.
func (a *Agent) WaitForIdle() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != nil {
		return a.active.idle
	}
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Prompt starts a new prompt from text.
func (a *Agent) Prompt(text string) error {
	return a.PromptMessages([]agentcore.AgentMessage{agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(text)},
		Timestamp: nowMillis(),
	}})
}

// PromptMessages starts a new prompt run with the given messages.
func (a *Agent) PromptMessages(messages []agentcore.AgentMessage) error {
	a.mu.Lock()
	busy := a.active != nil
	a.mu.Unlock()
	if busy {
		return ErrAgentBusyPrompt
	}
	return a.runMessages(messages, false, true)
}

// Continue resumes from the current transcript. The last message must be a
// user or tool-result message, unless a queued message can be used as a prompt.
func (a *Agent) Continue() error {
	a.mu.Lock()
	busy := a.active != nil
	messages := append(agentcore.MessageList{}, a.state.Messages...)
	a.mu.Unlock()
	if busy {
		return ErrAgentBusyContinue
	}
	if len(messages) == 0 {
		return ErrNoMessagesToContinueFrom
	}
	allSystem := true
	for _, m := range messages {
		if m.Role() != agentcore.RoleSystem {
			allSystem = false
			break
		}
	}
	if allSystem {
		return ErrNoMessagesToContinueFrom
	}
	if messages[len(messages)-1].Role() == agentcore.RoleAssistant {
		if steering := a.steering.drain(); len(steering) > 0 {
			return a.runMessages(steering, true, true)
		}
		if follow := a.followUp.drain(); len(follow) > 0 {
			return a.runMessages(follow, false, true)
		}
		return ErrContinueFromAssistant
	}
	return a.runMessages(nil, false, false)
}

// Reset clears conversation state and queues while retaining the replayed
// prompt/tool baseline.
func (a *Agent) Reset() error {
	a.mu.Lock()
	if a.active != nil {
		a.mu.Unlock()
		return ErrAgentBusyReset
	}
	baseline := agentcore.CurrentSystemMessage(a.state.Messages)
	a.state.Messages = nil
	if baseline != nil {
		a.state.Messages = agentcore.MessageList{*baseline}
	}
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolCalls = map[string]struct{}{}
	a.state.ErrorMessage = ""
	a.mu.Unlock()
	a.ClearAllQueues()
	return nil
}

// runMessages drives one loop run. prompts are appended to the run snapshot and
// marked as the new prompt (unless isContinuation); they are not part of the
// pre-existing transcript.
func (a *Agent) runMessages(prompts []agentcore.AgentMessage, skipInitialSteeringPoll, isPrompt bool) error {
	a.mu.Lock()
	if a.active != nil {
		a.mu.Unlock()
		return ErrAgentBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	active := &agentActiveRun{ctx: ctx, cancel: cancel, idle: make(chan struct{})}
	a.active = active
	a.state.IsStreaming = true
	a.state.StreamingMessage = nil
	a.state.ErrorMessage = ""
	snapshot := &agentcore.AgentContext{
		SystemPrompt: a.state.SystemPrompt,
		Messages:     append(agentcore.MessageList{}, a.state.Messages...),
		Tools:        append([]agentcore.AgentTool{}, a.state.Tools...),
	}
	model := a.state.Model
	thinking := a.state.ThinkingLevel
	a.mu.Unlock()

	if len(prompts) > 0 {
		snapshot.Messages = append(snapshot.Messages, prompts...)
	}
	cfg := a.runConfig(model, thinking)
	cfg.SkipInitialSteeringPoll = skipInitialSteeringPoll
	if isPrompt {
		cfg.PromptCount = len(prompts)
	}

	var stream *LoopEventStream
	var err error
	if isPrompt {
		stream = StartRun(ctx, snapshot, cfg)
	} else {
		stream, err = StartContinue(ctx, snapshot, cfg)
		if err != nil {
			a.finishRun()
			return err
		}
	}
	for ev := range stream.Events() {
		a.processEvent(ev, ctx)
	}
	_, runErr := stream.Result(ctx)
	a.finishRun()
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return nil
}

// runConfig builds the per-run loop configuration from the current state.
func (a *Agent) runConfig(model string, thinking agentcore.ThinkingLevel) RunConfig {
	return RunConfig{
		LoopConfig: LoopConfig{
			Model:            model,
			APIKey:           a.opts.LoopConfig.APIKey,
			ThinkingLevel:    thinking,
			Stream:           a.opts.LoopConfig.Stream,
			TransformContext: a.opts.LoopConfig.TransformContext,
			ConvertToLlm:     a.opts.LoopConfig.ConvertToLlm,
			GetAPIKey:        a.opts.LoopConfig.GetAPIKey,
			Provider:         a.opts.LoopConfig.Provider,
			ContextWindow:    a.opts.LoopConfig.ContextWindow,
			Compaction:       a.opts.LoopConfig.Compaction,
			SummaryStream:    a.opts.LoopConfig.SummaryStream,
			SummaryModel:     a.opts.LoopConfig.SummaryModel,
			Extra:            a.opts.LoopConfig.Extra,
		},
		Batch:            a.opts.Batch,
		FinishTurn:       a.opts.FinishTurn,
		PrepareRequest:   a.opts.PrepareRequest,
		PrepareNextTurn:  a.opts.PrepareNextTurn,
		OnBeforeSettle:   a.opts.OnBeforeSettle,
		EmitSettleEvents: a.opts.EmitSettleEvents,
		EventBuffer:      a.opts.EventBuffer,
		SessionID:        a.opts.SessionID,
		GetSteeringMessages: func(context.Context) []agentcore.AgentMessage {
			return a.steering.drain()
		},
		GetFollowUpMessages: func(context.Context, *agentcore.AgentContext) []agentcore.AgentMessage {
			return a.followUp.drain()
		},
	}
}

// finishRun clears runtime-owned state and resolves waiters. It runs after all
// events (including agent_end) and their listeners settled.
func (a *Agent) finishRun() {
	a.mu.Lock()
	a.state.IsStreaming = false
	a.state.StreamingMessage = nil
	a.state.PendingToolCalls = map[string]struct{}{}
	active := a.active
	a.active = nil
	a.mu.Unlock()
	if active != nil {
		close(active.idle)
	}
}

// processEvent reduces one loop event into state, then awaits subscribers.
func (a *Agent) processEvent(ev agentcore.AgentEvent, ctx context.Context) {
	a.mu.Lock()
	switch e := ev.(type) {
	case agentcore.MessageStartEvent:
		a.state.StreamingMessage = e.Message
	case agentcore.MessageUpdateEvent:
		a.state.StreamingMessage = e.Message
	case agentcore.MessageEndEvent:
		a.state.StreamingMessage = nil
		a.state.Messages = append(a.state.Messages, e.Message)
	case agentcore.ToolExecutionStartEvent:
		a.state.PendingToolCalls[e.ToolCallID] = struct{}{}
	case agentcore.ToolExecutionEndEvent:
		delete(a.state.PendingToolCalls, e.ToolCallID)
	case agentcore.TurnEndEvent:
		if e.Message.ErrorMessage != "" {
			a.state.ErrorMessage = e.Message.ErrorMessage
		}
	case agentcore.AgentEndEvent:
		a.state.StreamingMessage = nil
	}
	listeners := make([]func(agentcore.AgentEvent, context.Context), 0, len(a.listeners))
	for _, fn := range a.listeners {
		listeners = append(listeners, fn)
	}
	a.mu.Unlock()
	for _, fn := range listeners {
		fn(ev, ctx)
	}
}

// String renders a compact state summary for logs.
func (a *Agent) String() string {
	s := a.State()
	return fmt.Sprintf("Agent{model=%s streaming=%v messages=%d}", s.Model, s.IsStreaming, len(s.Messages))
}
