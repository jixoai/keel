package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/demo"
)

// collectDemoStream drains a provider stream and returns the events plus the
// terminal message.
func collectDemoStream(t *testing.T, stream *AssistantMessageEventStream) ([]AssistantMessageEvent, agentcore.AssistantMessage) {
	t.Helper()
	var events []AssistantMessageEvent
	for event := range stream.Events() {
		events = append(events, event)
	}
	message, err := stream.Result(context.Background())
	if err != nil {
		t.Fatalf("stream result: %v", err)
	}
	return events, message
}

// TestDemoProviderStreamsTheFixedReply pins the in-process driver's contract: the
// stream carries a start event, text deltas whose concatenation is exactly the
// fixed reply, and a terminal done event with usage and an end-turn stop reason.
func TestDemoProviderStreamsTheFixedReply(t *testing.T) {
	driver := NewDemoProvider()
	if driver.Name() != demo.ProviderID {
		t.Fatalf("Name() = %q, want %q", driver.Name(), demo.ProviderID)
	}

	stream, err := driver.StreamCompletion(context.Background(), CompletionRequest{Model: demo.ModelLLM})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	events, message := collectDemoStream(t, stream)

	var startSeen, doneSeen bool
	var text string
	textEvents := 0
	for _, event := range events {
		switch event.EventKind() {
		case StreamEventStart:
			startSeen = true
		case StreamEventText:
			// A text event carries the *partial* message: its content is the
			// accumulated reply so far, so the last one is the full text.
			textEvents++
			text = agentcore.ContentToText(event.(StreamTextEvent).Partial.Content)
		case StreamEventDone:
			doneSeen = true
		case StreamEventError:
			t.Fatalf("the demo stream must not carry an error event: %+v", event)
		}
	}
	if !startSeen {
		t.Fatal("the stream must open with a start event")
	}
	if !doneSeen {
		t.Fatal("the stream must close with a done event")
	}
	if textEvents < 2 {
		t.Fatalf("text events = %d, want several deltas (the reply must stream)", textEvents)
	}
	if text != demo.ReplyText {
		t.Fatalf("streamed text = %q, want the fixed reply", text)
	}
	if got := agentcore.ContentToText(message.Content); got != demo.ReplyText {
		t.Fatalf("terminal message text = %q, want the fixed reply", got)
	}
	if message.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("stopReason = %q, want end_turn", message.StopReason)
	}
	if message.Usage == nil || message.Usage.OutputTokens <= 0 {
		t.Fatalf("terminal message usage = %+v, want a reported output token count", message.Usage)
	}
}

// TestDemoProviderIsDeterministic pins reproducibility: two runs produce the same
// events and the same terminal facts, so a review screenshot is stable.
func TestDemoProviderIsDeterministic(t *testing.T) {
	driver := NewDemoProvider()
	first, firstMessage := collectDemoStream(t, mustDemoStream(t, driver))
	second, secondMessage := collectDemoStream(t, mustDemoStream(t, driver))

	if len(first) != len(second) {
		t.Fatalf("event counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].EventKind() != second[i].EventKind() {
			t.Fatalf("event %d kind = %q, want %q", i, first[i].EventKind(), second[i].EventKind())
		}
	}
	if agentcore.ContentToText(firstMessage.Content) != agentcore.ContentToText(secondMessage.Content) {
		t.Fatal("the two runs produced different text")
	}
	if firstMessage.Usage.OutputTokens != secondMessage.Usage.OutputTokens {
		t.Fatalf("usage differs: %d vs %d", firstMessage.Usage.OutputTokens, secondMessage.Usage.OutputTokens)
	}
}

// TestDemoProviderRefusesForeignModels pins the model gate: the demo driver serves
// exactly its own LLM model, so a mis-wired selection fails as an early error
// instead of streaming a convincing-looking answer for the wrong model.
func TestDemoProviderRefusesForeignModels(t *testing.T) {
	driver := NewDemoProvider()
	for _, modelID := range []string{"gpt-5", "", "demo-image", "demo-realtime", "demo-tts"} {
		if _, err := driver.StreamCompletion(context.Background(), CompletionRequest{Model: modelID}); !errors.Is(err, ErrDemoModelNotAllowed) {
			t.Fatalf("StreamCompletion(%q) err = %v, want ErrDemoModelNotAllowed", modelID, err)
		}
	}
}

// TestDemoProviderHonoursCancellation pins the loop's cancellation contract: a
// cancelled context ends the stream without leaking the producer.
func TestDemoProviderHonoursCancellation(t *testing.T) {
	driver := NewDemoProvider()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream, err := driver.StreamCompletion(ctx, CompletionRequest{Model: demo.ModelLLM})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	// Draining must terminate: the producer stops on the cancelled context and
	// closes the stream.
	for range stream.Events() {
	}
}

// mustDemoStream opens one demo stream or fails the test.
func mustDemoStream(t *testing.T, driver *DemoProvider) *AssistantMessageEventStream {
	t.Helper()
	stream, err := driver.StreamCompletion(context.Background(), CompletionRequest{Model: demo.ModelLLM})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	return stream
}

// ---------------------------------------------------------------------------
// Non-streaming path (Complete)
// ---------------------------------------------------------------------------

// demoRequestWithUserText builds a request whose context carries a distinctive
// user sentence, so "the demo never echoes the caller" can be asserted.
func demoRequestWithUserText(model string) CompletionRequest {
	return CompletionRequest{
		Model: model,
		Context: LlmContext{
			SystemPrompt: "you are a helpful assistant",
			Messages: agentcore.MessageList{
				agentcore.UserMessage{Content: agentcore.ContentList{
					agentcore.NewTextContent("secret-user-payload-9f3c"),
				}},
			},
		},
	}
}

// TestDemoProviderCompleteReturnsTheFixedReply pins the non-streaming answer: one
// assistant message with the fixed text, a positive usage count, end_turn, and no
// trace of the request's own text.
func TestDemoProviderCompleteReturnsTheFixedReply(t *testing.T) {
	driver := NewDemoProvider()

	message, err := driver.Complete(context.Background(), demoRequestWithUserText(demo.ModelLLM))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if message.Role() != agentcore.RoleAssistant {
		t.Fatalf("role = %q, want %q", message.Role(), agentcore.RoleAssistant)
	}
	if got := agentcore.ContentToText(message.Content); got != demo.ReplyText {
		t.Fatalf("content = %q, want the fixed demo text %q", got, demo.ReplyText)
	}
	if message.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("stopReason = %q, want %q", message.StopReason, agentcore.StopReasonEndTurn)
	}
	if message.Usage == nil {
		t.Fatal("the demo answer must carry usage")
	}
	if want := len([]rune(demo.ReplyText)); message.Usage.OutputTokens != want {
		t.Fatalf("usage.output = %d, want %d", message.Usage.OutputTokens, want)
	}
	if message.ResponseID != demo.ResponseID {
		t.Fatalf("responseId = %q, want %q", message.ResponseID, demo.ResponseID)
	}
	if message.ResponseModel != demo.ModelLLM {
		t.Fatalf("responseModel = %q, want %q", message.ResponseModel, demo.ModelLLM)
	}

	// The demo MUST NOT read the request: neither the user sentence nor the system
	// prompt may appear anywhere in the answer.
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	for _, forbidden := range []string{"secret-user-payload-9f3c", "helpful assistant"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("the demo answer echoed request text %q: %s", forbidden, encoded)
		}
	}
}

// TestDemoProviderCompleteIsByteIdenticalAcrossCalls pins reproducibility at the
// byte level: two calls serialize to the same JSON. The demo stamps no wall-clock
// field, so nothing in the message varies between calls.
func TestDemoProviderCompleteIsByteIdenticalAcrossCalls(t *testing.T) {
	driver := NewDemoProvider()

	first, err := json.Marshal(mustCompleteDemo(t, driver))
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	second, err := json.Marshal(mustCompleteDemo(t, driver))
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("repeated Complete is not byte-identical:\nfirst  %s\nsecond %s", first, second)
	}
}

// TestDemoProviderCompleteMatchesStreamedAnswer pins the single-decode rule: the
// non-streaming message and the streamed terminal message carry the same content,
// stop reason and usage, because both are produced by the same OpenAIDecoder.
func TestDemoProviderCompleteMatchesStreamedAnswer(t *testing.T) {
	driver := NewDemoProvider()

	complete, err := driver.Complete(context.Background(), CompletionRequest{Model: demo.ModelLLM})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_, streamed := collectDemoStream(t, mustDemoStream(t, driver))

	if got, want := agentcore.ContentToText(complete.Content), agentcore.ContentToText(streamed.Content); got != want {
		t.Fatalf("non-streamed content %q != streamed %q", got, want)
	}
	if complete.StopReason != streamed.StopReason {
		t.Fatalf("stopReason differs: non-streamed %q, streamed %q", complete.StopReason, streamed.StopReason)
	}
	if (complete.Usage == nil) != (streamed.Usage == nil) {
		t.Fatalf("usage presence differs: non-streamed %+v, streamed %+v", complete.Usage, streamed.Usage)
	}
	if complete.Usage != nil && *complete.Usage != *streamed.Usage {
		t.Fatalf("usage differs: non-streamed %+v, streamed %+v", *complete.Usage, *streamed.Usage)
	}
}

// TestDemoProviderCompleteRefusesForeignModels pins the same model gate on the
// non-streaming path, so a mis-wired selection fails there too instead of
// returning a convincing-looking answer.
func TestDemoProviderCompleteRefusesForeignModels(t *testing.T) {
	driver := NewDemoProvider()
	for _, modelID := range []string{"gpt-5", "", demo.ModelImage, demo.ModelDecision, demo.ModelSTT} {
		if _, err := driver.Complete(context.Background(), CompletionRequest{Model: modelID}); !errors.Is(err, ErrDemoModelNotAllowed) {
			t.Fatalf("Complete(%q) err = %v, want ErrDemoModelNotAllowed", modelID, err)
		}
	}
}

// TestDemoProviderReachesNoEnvironmentOrNetwork pins the demo's isolation: the
// driver answers with no credential in the request and no provider endpoint, which
// is only possible because nothing in it performs IO.
func TestDemoProviderReachesNoEnvironmentOrNetwork(t *testing.T) {
	driver := NewDemoProvider()

	// An empty API key and an empty endpoint base are the realistic review state
	// (no vendor account configured). A driver that reached out would fail here.
	request := demoRequestWithUserText(demo.ModelLLM)
	if request.Config.APIKey != "" {
		t.Fatal("the isolation check assumes an empty API key")
	}
	message, err := driver.Complete(context.Background(), request)
	if err != nil {
		t.Fatalf("Complete without a credential: %v", err)
	}
	if got := agentcore.ContentToText(message.Content); got != demo.ReplyText {
		t.Fatalf("content = %q, want the fixed reply", got)
	}
}

// mustCompleteDemo runs the non-streaming call and fails the test on error.
func mustCompleteDemo(t *testing.T, driver *DemoProvider) agentcore.AssistantMessage {
	t.Helper()
	message, err := driver.Complete(context.Background(), CompletionRequest{Model: demo.ModelLLM})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return message
}
