// Conformance tests ported from pi's packages/agent/test/agent.test.ts
// (pin 55a5c69, MIT License). Every test keeps pi's it(...) title (prefixed with
// TestAgent) so the two suites can be diffed line by line. Go adaptations:
//   - AbortSignal -> context.Context whose Done closes on Abort();
//   - provider throws -> StreamFn returns an error;
//   - Promise -> error return / channel.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/provider"
)

func agentUser(text string) agentcore.UserMessage {
	return agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: textContent(text), Timestamp: 1}
}

func agentAssistant(text string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: textContent(text), StopReason: agentcore.StopReasonEndTurn}
}

func agentToolUse(calls ...agentcore.ToolCallContent) agentcore.AssistantMessage {
	content := make(agentcore.ContentList, 0, len(calls))
	for _, c := range calls {
		content = append(content, c)
	}
	return agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: content, StopReason: agentcore.StopReasonToolUse}
}

func agentTool(name string) agentcore.AgentTool {
	return funcTool{name: name, schema: `{"type":"object"}`, run: func(_ context.Context, _ string, _ json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		return agentcore.AgentToolResult{Content: textContent(name)}, nil
	}}
}

// agentScriptedStream hands out responses in order and records requests.
func agentScriptedStream(t *testing.T, responses ...agentcore.AssistantMessage) (provider.StreamFn, func() []provider.LlmContext) {
	t.Helper()
	var (
		mu   sync.Mutex
		next int
		reqs []provider.LlmContext
	)
	fn := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		mu.Lock()
		i := next
		next++
		reqs = append(reqs, provider.LlmContext{
			SystemPrompt: llm.SystemPrompt,
			Messages:     append(agentcore.MessageList{}, llm.Messages...),
			Tools:        llm.Tools,
		})
		mu.Unlock()
		if i >= len(responses) {
			return nil, errors.New("unexpected provider call")
		}
		msg := responses[i]
		s := provider.NewAssistantMessageEventStream(8)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			_ = s.Emit(ctx, provider.StreamTextEvent{Partial: msg})
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}
	return fn, func() []provider.LlmContext {
		mu.Lock()
		defer mu.Unlock()
		out := make([]provider.LlmContext, len(reqs))
		copy(out, reqs)
		return out
	}
}

func newTestAgent(t *testing.T, opts AgentOptions) *Agent {
	t.Helper()
	if opts.EventBuffer == 0 {
		opts.EventBuffer = 8
	}
	if opts.LoopConfig.Provider == "" {
		opts.LoopConfig.Provider = "mock"
	}
	if opts.LoopConfig.Model == "" {
		opts.LoopConfig.Model = "mock"
	}
	return NewAgent(opts)
}

func TestAgentShouldCreateInstanceWithCustomInitialState(t *testing.T) {
	agent := newTestAgent(t, AgentOptions{InitialState: AgentInitialState{
		SystemPrompt:  "You are a helpful assistant.",
		ThinkingLevel: agentcore.ThinkingLow,
	}, LoopConfig: LoopConfig{Model: "mock"}})

	state := agent.State()
	if len(state.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(state.Messages))
	}
	sys, ok := state.Messages[0].(agentcore.SystemMessage)
	if !ok || sys.Content != "You are a helpful assistant." {
		t.Fatalf("initial message = %#v", state.Messages[0])
	}
	if state.ThinkingLevel != agentcore.ThinkingLow {
		t.Fatalf("thinking level = %q", state.ThinkingLevel)
	}
	if state.IsStreaming {
		t.Fatal("isStreaming = true, want false")
	}
}

func TestAgentConvertsInitialPromptAndToolsIntoTranscriptState(t *testing.T) {
	tool := agentTool("echo")
	agent := newTestAgent(t, AgentOptions{InitialState: AgentInitialState{
		SystemPrompt: "You are helpful.",
		Tools:        []agentcore.AgentTool{tool},
	}})
	state := agent.State()
	sys, ok := state.Messages[0].(agentcore.SystemMessage)
	if !ok {
		t.Fatalf("initial message is %T", state.Messages[0])
	}
	if sys.Content != "You are helpful." {
		t.Fatalf("content = %q", sys.Content)
	}
	if len(sys.ToolsAdded) != 1 || sys.ToolsAdded[0].Name != "echo" {
		t.Fatalf("toolsAdded = %#v", sys.ToolsAdded)
	}
	if strings.Contains(string(sys.ToolsAdded[0].Parameters), "execute") {
		t.Fatal("tool declaration leaked executable payload")
	}
}

func TestAgentDeclaresToolLoadoutChangesToModelBeforeNextRequest(t *testing.T) {
	first := agentTool("first")
	second := agentTool("second")
	streamFn, requests := agentScriptedStream(t,
		agentAssistant("done"), agentAssistant("done"), agentAssistant("done"))
	agent := newTestAgent(t, AgentOptions{
		InitialState: AgentInitialState{SystemPrompt: "You are helpful.", Tools: []agentcore.AgentTool{first}},
		LoopConfig:   LoopConfig{Stream: streamFn},
	})

	if err := agent.Prompt("one"); err != nil {
		t.Fatal(err)
	}
	agent.SetTools([]agentcore.AgentTool{second})
	if err := agent.Prompt("two"); err != nil {
		t.Fatal(err)
	}
	if err := agent.Prompt("three"); err != nil {
		t.Fatal(err)
	}

	deltas := make([][]string, 0, len(requests()))
	for _, req := range requests() {
		var rec []string
		for _, m := range req.Messages {
			sys, ok := m.(agentcore.SystemMessage)
			if !ok {
				continue
			}
			added := make([]string, 0, len(sys.ToolsAdded))
			for _, d := range sys.ToolsAdded {
				added = append(added, d.Name)
			}
			removed := make([]string, 0, len(sys.ToolsRemoved))
			for _, r := range sys.ToolsRemoved {
				removed = append(removed, r.Name)
			}
			rec = append(rec, "+"+strings.Join(added, ","), "-"+strings.Join(removed, ","))
		}
		deltas = append(deltas, rec)
	}
	want := [][]string{
		{"+first", "-"},
		{"+first", "-", "+second", "-first"},
		{"+first", "-", "+second", "-first"},
	}
	if !reflect.DeepEqual(deltas, want) {
		t.Fatalf("tool deltas = %v, want %v", deltas, want)
	}

	var update *agentcore.SystemMessage
	for _, m := range agent.State().Messages {
		if sys, ok := m.(agentcore.SystemMessage); ok && len(sys.ToolsRemoved) > 0 {
			update = &sys
		}
	}
	if update == nil {
		t.Fatal("no system message announced the tool removal")
	}
	if update.Content != "" || len(update.ToolsAdded) != 1 || update.ToolsAdded[0].Name != "second" ||
		len(update.ToolsRemoved) != 1 || update.ToolsRemoved[0].Name != "first" {
		t.Fatalf("update = %#v", *update)
	}
}

func TestAgentMergesToolChangesIntoPendingSystemMessage(t *testing.T) {
	tool := agentTool("echo")
	streamFn, requests := agentScriptedStream(t, agentAssistant("done"))
	agent := newTestAgent(t, AgentOptions{
		InitialState: AgentInitialState{SystemPrompt: "You are helpful."},
		LoopConfig:   LoopConfig{Stream: streamFn},
	})
	agent.SetTools([]agentcore.AgentTool{tool})

	pending := agentcore.SystemMessage{RoleField: agentcore.RoleSystem, Content: "", Timestamp: 1}
	if err := agent.PromptMessages([]agentcore.AgentMessage{pending, agentUser("hi")}); err != nil {
		t.Fatal(err)
	}

	state := agent.State()
	if len(state.Messages) != 4 {
		t.Fatalf("messages = %d, want 4: %v", len(state.Messages), messageRoles(state.Messages))
	}
	sys, ok := state.Messages[1].(agentcore.SystemMessage)
	if !ok {
		t.Fatalf("messages[1] = %T", state.Messages[1])
	}
	if len(sys.ToolsAdded) != 1 || sys.ToolsAdded[0].Name != "echo" {
		t.Fatalf("merged toolsAdded = %#v", sys.ToolsAdded)
	}
	systemCount := 0
	for _, m := range requests()[0].Messages {
		if m.Role() == agentcore.RoleSystem {
			systemCount++
		}
	}
	if systemCount != 2 {
		t.Fatalf("request system messages = %d, want 2 (initial + merged)", systemCount)
	}
}

func TestAgentRestoresTranscriptBaselineWhenReset(t *testing.T) {
	tool := agentTool("echo")
	agent := newTestAgent(t, AgentOptions{InitialState: AgentInitialState{
		SystemPrompt: "You are helpful.",
		Tools:        []agentcore.AgentTool{tool},
		Messages:     agentcore.MessageList{agentUser("old")},
	}})
	if got := messageRoles(agent.State().Messages); !reflect.DeepEqual(got, []string{"system", "user"}) {
		t.Fatalf("initial roles = %v", got)
	}
	if err := agent.Reset(); err != nil {
		t.Fatal(err)
	}
	state := agent.State()
	if len(state.Messages) != 1 {
		t.Fatalf("messages after reset = %d, want 1", len(state.Messages))
	}
	sys, ok := state.Messages[0].(agentcore.SystemMessage)
	if !ok || sys.Content != "You are helpful." || len(sys.ToolsAdded) != 1 || sys.ToolsAdded[0].Name != "echo" {
		t.Fatalf("baseline = %#v", state.Messages[0])
	}
}

func TestAgentShouldSubscribeToEvents(t *testing.T) {
	agent := newTestAgent(t, AgentOptions{})
	var count atomic.Int32
	unsubscribe := agent.Subscribe(func(agentcore.AgentEvent, context.Context) { count.Add(1) })
	if count.Load() != 0 {
		t.Fatal("subscribe emitted an event")
	}
	agent.SetThinkingLevel(agentcore.ThinkingLow)
	if count.Load() != 0 {
		t.Fatal("state mutator emitted an event")
	}
	unsubscribe()
	agent.SetThinkingLevel(agentcore.ThinkingHigh)
	if count.Load() != 0 {
		t.Fatal("unsubscribed listener was called")
	}
}

func TestAgentEmitsFullLifecycleEventsForThrownRunFailures(t *testing.T) {
	agent := newTestAgent(t, AgentOptions{LoopConfig: LoopConfig{
		Stream: func(context.Context, string, provider.LlmContext, provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
			return nil, errors.New("provider exploded")
		},
	}})
	var events []string
	agent.Subscribe(func(ev agentcore.AgentEvent, _ context.Context) {
		events = append(events, ev.EventType())
	})

	if err := agent.Prompt("hello"); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"agent_start",
		"message_start", "message_end", // user prompt
		"turn_start",
		"message_start", "message_end", // synthesized failure
		"turn_end",
		"telemetry", // Keel extension emitted just before agent_end
		"agent_end",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	state := agent.State()
	last, ok := state.Messages[len(state.Messages)-1].(agentcore.AssistantMessage)
	if !ok {
		t.Fatalf("last message = %T", state.Messages[len(state.Messages)-1])
	}
	if last.StopReason != agentcore.StopReasonError || last.ErrorMessage != "provider exploded" {
		t.Fatalf("failure message = %q / %q", last.StopReason, last.ErrorMessage)
	}
	if state.ErrorMessage != "provider exploded" {
		t.Fatalf("state errorMessage = %q", state.ErrorMessage)
	}
}

func TestAgentShouldAwaitAsyncSubscribersBeforePromptResolves(t *testing.T) {
	streamFn, _ := agentScriptedStream(t, agentAssistant("ok"))
	agent := newTestAgent(t, AgentOptions{LoopConfig: LoopConfig{Stream: streamFn}})
	barrier := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	agent.Subscribe(func(ev agentcore.AgentEvent, _ context.Context) {
		if ev.EventType() == agentcore.EventAgentEnd {
			close(barrier)
			<-release
			close(finished)
		}
	})

	done := make(chan error, 1)
	go func() { done <- agent.Prompt("hello") }()

	select {
	case <-barrier:
	case <-time.After(2 * time.Second):
		t.Fatal("agent_end listener never ran")
	}
	select {
	case <-done:
		t.Fatal("prompt resolved before the async listener settled")
	default:
	}
	if !agent.State().IsStreaming {
		t.Fatal("isStreaming = false while the agent_end listener is still running")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("listener did not finish")
	}
	if agent.State().IsStreaming {
		t.Fatal("isStreaming = true after settlement")
	}
}

func TestAgentWaitForIdleShouldWaitForAsyncSubscribers(t *testing.T) {
	streamFn, _ := agentScriptedStream(t, agentAssistant("ok"))
	agent := newTestAgent(t, AgentOptions{LoopConfig: LoopConfig{Stream: streamFn}})
	barrier := make(chan struct{})
	agent.Subscribe(func(ev agentcore.AgentEvent, _ context.Context) {
		if ev.EventType() == agentcore.EventMessageEnd {
			<-barrier
		}
	})

	done := make(chan error, 1)
	go func() { done <- agent.Prompt("hello") }()
	time.Sleep(10 * time.Millisecond)
	idle := agent.WaitForIdle()
	select {
	case <-idle:
		t.Fatal("idle resolved before the message_end listener settled")
	default:
	}
	close(barrier)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-idle:
	case <-time.After(2 * time.Second):
		t.Fatal("idle never resolved")
	}
}

func TestAgentShouldPassTheActiveAbortSignalToSubscribers(t *testing.T) {
	streamFn := func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					_ = s.Emit(ctx, provider.StreamErrorEvent{Message: agentcore.AssistantMessage{
						RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonAborted, ErrorMessage: "Aborted",
					}})
					return
				case <-ticker.C:
				}
			}
		}()
		return s, nil
	}
	agent := newTestAgent(t, AgentOptions{LoopConfig: LoopConfig{Stream: streamFn}})
	signalCh := make(chan context.Context, 1)
	agent.Subscribe(func(ev agentcore.AgentEvent, ctx context.Context) {
		if ev.EventType() == agentcore.EventAgentStart {
			select {
			case signalCh <- ctx:
			default:
			}
		}
	})
	done := make(chan error, 1)
	go func() { done <- agent.Prompt("hello") }()
	var captured context.Context
	select {
	case captured = <-signalCh:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive a run context")
	}
	if captured.Err() != nil {
		t.Fatal("run context aborted before Abort()")
	}
	agent.Abort()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if captured.Err() == nil {
		t.Fatal("run context was not aborted")
	}
}

func TestAgentShouldIgnoreToolUpdatesAfterTheToolExecutionSettles(t *testing.T) {
	var delayed agentcore.ToolUpdateFunc
	var updates atomic.Int32
	tool := funcTool{name: "delayed_tool", schema: `{"type":"object"}`, run: func(_ context.Context, _ string, _ json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		delayed = onUpdate
		onUpdate(agentcore.AgentToolResult{Content: textContent("running")})
		terminate := true
		return agentcore.AgentToolResult{Content: textContent("ok"), Terminate: &terminate}, nil
	}}
	streamFn, _ := agentScriptedStream(t, agentToolUse(call("call-1", "delayed_tool", `{}`)))
	agent := newTestAgent(t, AgentOptions{
		InitialState: AgentInitialState{Tools: []agentcore.AgentTool{tool}},
		LoopConfig:   LoopConfig{Stream: streamFn},
	})
	agent.Subscribe(func(ev agentcore.AgentEvent, _ context.Context) {
		if ev.EventType() == agentcore.EventToolExecutionUpdate {
			updates.Add(1)
		}
	})
	if err := agent.Prompt("run tool"); err != nil {
		t.Fatal(err)
	}
	if updates.Load() != 1 {
		t.Fatalf("tool updates = %d, want 1", updates.Load())
	}
	delayed(agentcore.AgentToolResult{Content: textContent("late")})
	time.Sleep(5 * time.Millisecond)
	if updates.Load() != 1 {
		t.Fatalf("late update emitted an event: %d", updates.Load())
	}
}

func TestAgentShouldSupportSteeringMessageQueue(t *testing.T) {
	agent := newTestAgent(t, AgentOptions{})
	message := agentUser("Steering message")
	agent.Steer(message)
	for _, m := range agent.State().Messages {
		if m.Role() == agentcore.RoleUser {
			t.Fatal("queued message leaked into state.messages")
		}
	}
	if !reflect.DeepEqual(agent.PeekQueuedMessages(), []agentcore.AgentMessage{message}) {
		t.Fatal("peek did not return the queued message")
	}
}

func TestAgentShouldSupportFollowUpMessageQueue(t *testing.T) {
	agent := newTestAgent(t, AgentOptions{})
	agent.FollowUp(agentUser("Follow-up message"))
	for _, m := range agent.State().Messages {
		if m.Role() == agentcore.RoleUser {
			t.Fatal("queued message leaked into state.messages")
		}
	}
	if !agent.HasQueuedMessages() {
		t.Fatal("hasQueuedMessages = false")
	}
}

func TestAgentShouldRejectResetWhileProcessingWithoutCorruptingTheTranscript(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	streamFn := func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			close(started)
			<-release
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: agentAssistant("Done")})
		}()
		return s, nil
	}
	agent := newTestAgent(t, AgentOptions{LoopConfig: LoopConfig{Stream: streamFn}})
	promptSeen := make(chan struct{})
	agent.Subscribe(func(ev agentcore.AgentEvent, _ context.Context) {
		if e, ok := ev.(agentcore.MessageEndEvent); ok && e.Message.Role() == agentcore.RoleUser {
			close(promptSeen)
		}
	})
	done := make(chan error, 1)
	go func() { done <- agent.Prompt("Hello") }()
	<-started
	<-promptSeen

	state := agent.State()
	if !state.IsStreaming || !reflect.DeepEqual(messageRoles(state.Messages), []string{"user"}) {
		t.Fatalf("state before reset = streaming:%v roles:%v", state.IsStreaming, messageRoles(state.Messages))
	}
	if err := agent.Reset(); !errors.Is(err, ErrAgentBusyReset) {
		t.Fatalf("reset error = %v, want ErrAgentBusyReset", err)
	}
	if roles := messageRoles(agent.State().Messages); !reflect.DeepEqual(roles, []string{"user"}) {
		t.Fatalf("reset corrupted the transcript: %v", roles)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if roles := messageRoles(agent.State().Messages); !reflect.DeepEqual(roles, []string{"user", "assistant"}) {
		t.Fatalf("final roles = %v", roles)
	}
}

func TestAgentShouldThrowWhenPromptCalledWhileStreaming(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	streamFn := func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			close(started)
			<-release
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: agentAssistant("ok")})
		}()
		return s, nil
	}
	agent := newTestAgent(t, AgentOptions{LoopConfig: LoopConfig{Stream: streamFn}})
	done := make(chan error, 1)
	go func() { done <- agent.Prompt("First message") }()
	<-started
	if err := agent.Prompt("Second message"); !errors.Is(err, ErrAgentBusyPrompt) {
		t.Fatalf("second prompt error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAgentShouldThrowWhenContinueCalledWhileStreaming(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	streamFn := func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			close(started)
			<-release
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: agentAssistant("ok")})
		}()
		return s, nil
	}
	agent := newTestAgent(t, AgentOptions{LoopConfig: LoopConfig{Stream: streamFn}})
	done := make(chan error, 1)
	go func() { done <- agent.Prompt("First message") }()
	<-started
	if err := agent.Continue(); !errors.Is(err, ErrAgentBusyContinue) {
		t.Fatalf("continue error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAgentContinueProcessesQueuedFollowUpMessagesAfterAnAssistantTurn(t *testing.T) {
	streamFn, requests := agentScriptedStream(t, agentAssistant("Processed"))
	agent := newTestAgent(t, AgentOptions{LoopConfig: LoopConfig{Stream: streamFn}})
	agent.SetMessages([]agentcore.AgentMessage{agentUser("Initial"), agentAssistant("Initial response")})
	agent.FollowUp(agentUser("Queued follow-up"))

	if err := agent.Continue(); err != nil {
		t.Fatal(err)
	}
	if got := len(requests()); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
	found := false
	for _, req := range requests() {
		for _, m := range req.Messages {
			if u, ok := m.(agentcore.UserMessage); ok && agentcore.ContentToText(u.Content) == "Queued follow-up" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("queued follow-up was not sent to the provider")
	}
	state := agent.State()
	if state.Messages[len(state.Messages)-1].Role() != agentcore.RoleAssistant {
		t.Fatalf("last role = %q", state.Messages[len(state.Messages)-1].Role())
	}
}

func TestAgentContinueKeepsModeSteeringSemanticsForAssistantTailFallback(t *testing.T) {
	for _, tc := range []struct {
		name             string
		mode             QueueMode
		expectedRequests int
	}{
		{"one-at-a-time", QueueOneAtATime, 2},
		{"all", QueueAll, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streamFn, requests := agentScriptedStream(t, agentAssistant("Processed"), agentAssistant("Processed"))
			agent := newTestAgent(t, AgentOptions{
				LoopConfig:   LoopConfig{Stream: streamFn},
				SteeringMode: tc.mode,
			})
			agent.SetMessages([]agentcore.AgentMessage{agentUser("Initial"), agentAssistant("Initial response")})
			agent.Steer(agentUser("Steering 1"))
			agent.Steer(agentUser("Steering 2"))

			if err := agent.Continue(); err != nil {
				t.Fatal(err)
			}
			reqs := requests()
			if len(reqs) != tc.expectedRequests {
				t.Fatalf("provider calls = %d, want %d", len(reqs), tc.expectedRequests)
			}
			first := requestUserTexts(reqs[0])
			second := ""
			if len(reqs) > 1 {
				second = strings.Join(requestUserTexts(reqs[1]), ",")
			}
			if !containsString(first, "Steering 1") {
				t.Fatalf("first request users = %v, want Steering 1", first)
			}
			if tc.mode == QueueOneAtATime {
				if containsString(first, "Steering 2") {
					t.Fatal("one-at-a-time delivered both messages in the first turn")
				}
				if !strings.Contains(second, "Steering 2") {
					t.Fatalf("second request users = %q, want Steering 2", second)
				}
			} else if !containsString(first, "Steering 2") {
				t.Fatalf("all mode first request users = %v, want both", first)
			}
		})
	}
}

func TestAgentRejectsQueuedContinuationWithoutDrainingQueues(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []agentcore.AgentMessage
	}{
		{"empty", nil},
		{"system-only", []agentcore.AgentMessage{agentcore.SystemMessage{RoleField: agentcore.RoleSystem, Content: "system only", Timestamp: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newTestAgent(t, AgentOptions{InitialState: AgentInitialState{Messages: agentcore.MessageList(tc.messages)}})
			steering := agentUser("steering")
			followUp := agentUser("follow-up")
			agent.Steer(steering)
			agent.FollowUp(followUp)

			if err := agent.Continue(); !errors.Is(err, ErrNoMessagesToContinueFrom) {
				t.Fatalf("continue error = %v", err)
			}
			if got := agent.PeekQueuedMessages(); !reflect.DeepEqual(got, []agentcore.AgentMessage{steering}) {
				t.Fatalf("peek = %v, want the steering message", got)
			}
			agent.ClearSteeringQueue()
			if got := agent.PeekQueuedMessages(); !reflect.DeepEqual(got, []agentcore.AgentMessage{followUp}) {
				t.Fatalf("peek after clearing steering = %v, want the follow-up", got)
			}
		})
	}
}

func TestAgentPreviewsTheNextSelectedQueuedMessagesWithoutConsumingThem(t *testing.T) {
	agent := newTestAgent(t, AgentOptions{SteeringMode: QueueOneAtATime, FollowUpMode: QueueAll})
	first := agentUser("first steering")
	second := agentUser("second steering")
	followUp := agentUser("follow-up")
	agent.Steer(first)
	agent.Steer(second)
	agent.FollowUp(followUp)

	if got := agent.PeekQueuedMessages(); !reflect.DeepEqual(got, []agentcore.AgentMessage{first}) {
		t.Fatalf("first peek = %v", got)
	}
	if got := agent.PeekQueuedMessages(); !reflect.DeepEqual(got, []agentcore.AgentMessage{first}) {
		t.Fatalf("second peek consumed messages: %v", got)
	}
	agent.ClearSteeringQueue()
	if got := agent.PeekQueuedMessages(); !reflect.DeepEqual(got, []agentcore.AgentMessage{followUp}) {
		t.Fatalf("peek after clearing steering = %v", got)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
