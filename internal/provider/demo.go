// Demo provider driver: the executable half of the server-side demo provider.
//
// The identity, the switch and the fixed content live in internal/demo; the
// catalog registration lives in internal/modelcfg. This file is the driver the
// run path actually calls, and it is deliberately the *smallest* possible
// adapter: it renders the same chat.completion.chunk payloads a real gateway
// sends and replays them through the shared transport decoder, so the runtime
// sees the exact event stream a vendor produces (start, text deltas, done with
// usage). There is no second streaming implementation to drift, and the
// dual-failure contract of the loop is honoured unchanged.
//
// Nothing here touches the network, the process environment or a credential: the
// reply is compiled-in data, which is what makes a review deployment able to run
// every surface without a vendor account and what guarantees the demo can never
// echo a real user's data.

package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/demo"
)

// DemoProviderName is the driver's Name(). It matches the provider id the
// catalog registers, so a log line says "demo" like it would say "openai".
const DemoProviderName = demo.ProviderID

// ErrDemoModelNotAllowed is returned when a caller asks the demo driver for a
// model it does not serve. It is an early "cannot build the stream" failure, not
// a runtime event: asking the wrong model is a configuration error, and the loop
// must fail the attempt instead of streaming a convincing-looking answer.
var ErrDemoModelNotAllowed = errors.New("demo: model is not served by the demo provider")

// errDemoStreamIncomplete is returned by Complete when the demo stream ended
// without a terminal done event. It should be unreachable (the fixed payload
// always carries finish_reason), and it is reported rather than papered over so a
// truncated answer can never be mistaken for a complete one.
var errDemoStreamIncomplete = errors.New("demo: response stream ended without a terminal event")

// DemoProvider serves the demo models in-process. It implements Provider, so it
// plugs into StreamFnFromProvider exactly like a vendor driver and needs no
// special case anywhere in the loop.
type DemoProvider struct {
	models []Model
}

// NewDemoProvider builds the in-process demo provider. The model catalog is fixed
// to the demo LLM model: the other five families have their own transports
// (mediagw, realtime) and never reach a chat completion.
func NewDemoProvider() *DemoProvider {
	return &DemoProvider{models: []Model{{
		Provider:         DemoProviderName,
		ID:               demo.ModelLLM,
		DisplayName:      demo.ModelLLM,
		ContextWindow:    demoContextWindowTokens,
		MaxOutputTokens:  demoMaxOutputTokens,
		SupportsThinking: false,
		SupportsTools:    true,
		SupportsImages:   false,
	}}}
}

// Name implements Provider.
func (p *DemoProvider) Name() string { return DemoProviderName }

// Models implements Provider.
func (p *DemoProvider) Models() []Model { return append([]Model(nil), p.models...) }

// StreamCompletion implements Provider. It validates the model identity, then
// replays the fixed chunk sequence through the real OpenAI decoder.
//
// The response honours the dual failure model: the returned error is only the
// early "wrong model" case; everything after that rides the stream as events and
// the terminal event carries the assembled message.
func (p *DemoProvider) StreamCompletion(ctx context.Context, req CompletionRequest) (*AssistantMessageEventStream, error) {
	family, ok := demo.Family(req.Model)
	if !ok || family != demo.FamilyLLM {
		return nil, fmt.Errorf("%w: %q", ErrDemoModelNotAllowed, strings.TrimSpace(req.Model))
	}

	stream := NewAssistantMessageEventStream(0)
	go pumpDemo(ctx, stream, req.Model)
	return stream, nil
}

// Complete returns the demo answer as one assembled assistant message, for a
// caller that did not ask for streaming (a diagnostics call, a harness check, or
// a consumer of the OpenAI-style non-streaming shape).
//
// It is deliberately built on the *same* path as the stream: the fixed chunk
// payloads go through the shared OpenAIDecoder exactly once, and the assembled
// message is the decoder's own terminal event. There is no second hand-built
// message shape, so the streamed and non-streamed answers cannot drift, and the
// non-streaming call inherits the decoder's usage accounting instead of
// re-deriving it.
//
// The content is deterministic: the same model id always yields byte-identical
// content, usage and stop reason. Nothing is read from req besides the model
// identity — the demo never echoes a caller's text.
func (p *DemoProvider) Complete(ctx context.Context, req CompletionRequest) (agentcore.AssistantMessage, error) {
	if err := ctx.Err(); err != nil {
		return agentcore.AssistantMessage{}, err
	}
	family, ok := demo.Family(req.Model)
	if !ok || family != demo.FamilyLLM {
		return agentcore.AssistantMessage{}, fmt.Errorf("%w: %q", ErrDemoModelNotAllowed, strings.TrimSpace(req.Model))
	}

	decoder := NewOpenAIDecoder()
	for _, payload := range demo.WireChunks(req.Model) {
		if _, err := decoder.Decode(payload); err != nil {
			return agentcore.AssistantMessage{}, fmt.Errorf("demo: decode response: %w", err)
		}
	}
	final, err := decoder.Finish()
	if err != nil {
		return agentcore.AssistantMessage{}, fmt.Errorf("demo: finish response: %w", err)
	}
	for _, event := range final {
		if done, ok := event.(StreamDoneEvent); ok {
			return done.Message, nil
		}
		if failure, ok := event.(StreamErrorEvent); ok {
			if failure.Err != nil {
				return failure.Message, failure.Err
			}
			return failure.Message, errors.New("demo: " + failure.Message.ErrorMessage)
		}
	}
	return agentcore.AssistantMessage{}, errDemoStreamIncomplete
}

// pumpDemo emits the demo stream: a start event, one text event per delta, and a
// terminal done event. The payloads are decoded by the shared OpenAIDecoder
// rather than hand-assembled AssistantMessages, so the demo cannot produce an
// event shape a real decoder would not.
func pumpDemo(ctx context.Context, stream *AssistantMessageEventStream, model string) {
	defer stream.Close()

	decoder := NewOpenAIDecoder()
	// The role chunk produces the start event; emit it explicitly so a consumer
	// sees a start even if the decoder's first chunk carries no content.
	if err := stream.Emit(ctx, StreamStartEvent{Partial: agentcore.AssistantMessage{
		RoleField: agentcore.RoleAssistant,
		API:       DemoProviderName,
		Provider:  DemoProviderName,
		Model:     model,
	}}); err != nil {
		return
	}

	for _, payload := range demo.WireChunks(model) {
		events, err := decoder.Decode(payload)
		if err != nil {
			// A decoder failure on our own fixed payload is a programming defect,
			// but it is still reported on the stream rather than panicking: the
			// loop's contract is that a runtime failure is an event.
			_ = stream.Emit(context.Background(), StreamErrorEvent{
				Message: agentcore.AssistantMessage{
					RoleField:    agentcore.RoleAssistant,
					StopReason:   agentcore.StopReasonError,
					ErrorMessage: "demo: decode response: " + err.Error(),
				},
				Err: err,
			})
			return
		}
		for _, event := range events {
			if err := stream.Emit(ctx, event); err != nil {
				return
			}
		}
	}
	// Finish flushes the terminal done event (the decoder saw finish_reason on the
	// last chunk).
	final, err := decoder.Finish()
	if err != nil {
		_ = stream.Emit(context.Background(), StreamErrorEvent{
			Message: agentcore.AssistantMessage{
				RoleField:    agentcore.RoleAssistant,
				StopReason:   agentcore.StopReasonError,
				ErrorMessage: "demo: finish response: " + err.Error(),
			},
			Err: err,
		})
		return
	}
	for _, event := range final {
		if err := stream.Emit(ctx, event); err != nil {
			return
		}
	}
}

// the demo models' budgets, mirrored from the executable catalog.
const (
	demoContextWindowTokens = 128000
	demoMaxOutputTokens     = 8192
)
