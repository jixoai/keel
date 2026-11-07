package agentlooptest

import (
	"context"
	"fmt"
	"sync"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/provider"
)

// recordedRequest is one provider call observed by the suite.
type recordedRequest struct {
	Model         string
	SystemPrompt  string
	Messages      agentcore.MessageList
	Tools         []agentcore.AgentTool
	ThinkingLevel agentcore.ThinkingLevel
}

// scriptedProvider is the conformance suite's provider double. It serves the
// pre-programmed responses in order (PI's createAssistantMessageEventStream +
// streamFn), records every request, and can:
//
//   - block a call until the run context ends (cancellation),
//   - close a stream without a terminal event (PI's proxy contract), or
//   - fail the call before building a stream (the early-failure path).
//
// It never touches the network or the clock.
type scriptedProvider struct {
	mu          sync.Mutex
	responses   []agentcore.AssistantMessage
	requests    []recordedRequest
	calls       int
	blockAt     int
	entered     chan struct{}
	enteredOnce sync.Once
	noTerminal  int
	failAt      int
	failErr     error
}

func newScriptedProvider(responses ...agentcore.AssistantMessage) *scriptedProvider {
	return &scriptedProvider{responses: responses}
}

// blockAtCall makes the 1-based call index block until the run context ends,
// simulating a client that goes away mid-stream. It returns a channel closed
// when that call is entered.
func (p *scriptedProvider) blockAtCall(n int) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blockAt = n
	p.entered = make(chan struct{})
	return p.entered
}

// truncateAtCall makes the 1-based call index end mid-content: after the start
// and text deltas the stream emits the provider-contract terminal error event
// (StopReason error + a message) instead of done. That is what a real transport
// produces when the wire closes without a terminal event (provider.StreamFn's
// contract), and the loop must surface it as a terminal error message rather
// than hang.
func (p *scriptedProvider) truncateAtCall(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.noTerminal = n
}

// failAtCall makes the 1-based call index return an error instead of a stream
// (the "could not even build the stream" path).
func (p *scriptedProvider) failAtCall(n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failAt = n
	p.failErr = err
}

func (p *scriptedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *scriptedProvider) seenRequests() []recordedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedRequest{}, p.requests...)
}

// stream is the provider.StreamFn handed to the loop under test.
func (p *scriptedProvider) stream(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
	p.mu.Lock()
	p.calls++
	index := p.calls
	p.requests = append(p.requests, recordedRequest{
		Model:         model,
		SystemPrompt:  llm.SystemPrompt,
		Messages:      append(agentcore.MessageList{}, llm.Messages...),
		Tools:         append([]agentcore.AgentTool{}, llm.Tools...),
		ThinkingLevel: cfg.ThinkingLevel,
	})
	blocking := p.blockAt > 0 && index >= p.blockAt
	if blocking {
		p.enteredOnce.Do(func() { close(p.entered) })
	}
	noTerminal := p.noTerminal == index
	var failErr error
	if p.failAt == index {
		failErr = p.failErr
	}
	var response agentcore.AssistantMessage
	haveResponse := index <= len(p.responses)
	if haveResponse {
		response = p.responses[index-1]
	}
	p.mu.Unlock()

	if failErr != nil {
		return nil, failErr
	}
	if blocking {
		// The request abandons when the client (the run context) goes away: the
		// stream closes without a terminal event, like a real disconnect.
		<-ctx.Done()
		s := provider.NewAssistantMessageEventStream(1)
		s.Close()
		return s, nil
	}
	if !haveResponse && !noTerminal {
		return nil, fmt.Errorf("agentlooptest: unexpected provider call %d (script has %d responses)", index, len(p.responses))
	}
	if noTerminal && !haveResponse {
		response = agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent("partial")},
			StopReason: agentcore.StopReasonEndTurn,
		}
	}

	s := provider.NewAssistantMessageEventStream(4)
	go func() {
		defer s.Close()
		partial := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}
		_ = s.Emit(ctx, provider.StreamStartEvent{Partial: partial})
		_ = s.Emit(ctx, provider.StreamTextEvent{Partial: response})
		if noTerminal {
			// The provider contract: a runtime failure rides the stream as a
			// terminal error event, never as a bare close.
			_ = s.Emit(ctx, provider.StreamErrorEvent{Message: agentcore.AssistantMessage{
				RoleField:    agentcore.RoleAssistant,
				Content:      agentcore.ContentList{agentcore.NewTextContent("partial")},
				StopReason:   agentcore.StopReasonError,
				ErrorMessage: "connection closed before a terminal event",
			}})
			return
		}
		_ = s.Emit(ctx, provider.StreamDoneEvent{Message: response})
	}()
	return s, nil
}
