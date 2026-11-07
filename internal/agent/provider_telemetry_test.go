package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/telemetry"
)

// emittingStream returns a StreamFn that emits events in order and closes. The
// events must already carry their partial messages.
func emittingStream(events ...provider.AssistantMessageEvent) provider.StreamFn {
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		stream := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer stream.Close()
			for _, event := range events {
				if err := stream.Emit(ctx, event); err != nil {
					return
				}
			}
		}()
		return stream, nil
	}
}

func observedPartial() agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField: agentcore.RoleAssistant,
		Provider:  "openai",
		API:       "openai",
		Model:     "gpt-5",
	}
}

func drainObserved(t *testing.T, ctx context.Context, stream *provider.AssistantMessageEventStream) []provider.AssistantMessageEvent {
	t.Helper()
	var events []provider.AssistantMessageEvent
	for event := range stream.Events() {
		events = append(events, event)
	}
	if _, err := stream.Result(ctx); err != nil {
		t.Fatalf("stream result: %v", err)
	}
	return events
}

// waitForSettledSpan polls the memory adapter until the request span settles.
// The span callback runs in the observer goroutine, so channel close can be
// observed a few microseconds before settlement.
func waitForSettledSpan(t *testing.T, memory *telemetry.Memory) telemetry.RecordedSpan {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		spans := memory.Spans()
		if len(spans) == 1 && spans[0].Settled {
			return spans[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for a settled span; spans = %+v", memory.Spans())
	return telemetry.RecordedSpan{}
}

func attribute(t *testing.T, span telemetry.RecordedSpan, name string) any {
	t.Helper()
	value, ok := span.Attributes[name]
	if !ok {
		t.Fatalf("span %q missing attribute %q: %v", span.Name, name, span.Attributes)
	}
	return value
}

func TestObservedStreamRecordsRequestSpanAndMetrics(t *testing.T) {
	partial := observedPartial()
	final := partial
	final.StopReason = agentcore.StopReasonEndTurn
	final.Usage = &agentcore.Usage{
		InputTokens: 100, OutputTokens: 40,
		CacheReadTokens: 30, CacheWriteTokens: 20,
	}
	base := emittingStream(
		provider.StreamStartEvent{Partial: partial},
		provider.StreamTextEvent{Partial: partial},
		provider.StreamDoneEvent{Message: final},
	)
	memory := telemetry.NewMemory()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := NewObservedStream(base, memory)(ctx, "gpt-5", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	events := drainObserved(t, ctx, stream)
	if len(events) != 3 {
		t.Fatalf("forwarded events = %d, want 3", len(events))
	}

	span := waitForSettledSpan(t, memory)
	if span.Name != aiRequestSpanName {
		t.Fatalf("span name = %q", span.Name)
	}
	if span.Status.Status != telemetry.SpanStatusOK {
		t.Fatalf("status = %+v", span.Status)
	}
	if got := attribute(t, span, attrProvider); got != "openai" {
		t.Fatalf("provider = %v", got)
	}
	if got := attribute(t, span, attrAPI); got != "openai" {
		t.Fatalf("api = %v", got)
	}
	if got := attribute(t, span, attrModel); got != "gpt-5" {
		t.Fatalf("model = %v", got)
	}
	if got := attribute(t, span, attrStop); got != agentcore.StopReasonEndTurn {
		t.Fatalf("stop = %v", got)
	}
	if got, ok := attribute(t, span, attrFirstChunk).(float64); !ok || got < 0 {
		t.Fatalf("first_chunk = %v", span.Attributes[attrFirstChunk])
	}
	wantUsage := map[string]float64{
		attrUsageInput:  100,
		attrUsageOutput: 40,
		attrCacheRead:   30,
		attrCacheWrite:  20,
	}
	for name, want := range wantUsage {
		if got := attribute(t, span, name); got != want {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}

	measurements := memory.Measurements()
	assertMeasurement(t, measurements, aiRequestDurationMetric, nil)
	assertMeasurement(t, measurements, aiFirstChunkDurationMetric, nil)
	wantTokens := map[string]float64{
		"input": 100, "output": 40, "cache_read": 30, "cache_write": 20,
	}
	for kind, want := range wantTokens {
		assertMeasurement(t, measurements, aiTokensMetric, telemetry.SpanAttributes{attrTokenType: kind}, want)
	}
}

func assertMeasurement(t *testing.T, measurements []telemetry.RecordedMeasurement, name string, attributes telemetry.SpanAttributes, want ...float64) {
	t.Helper()
	for _, measurement := range measurements {
		if measurement.Name != name {
			continue
		}
		matches := true
		for key, value := range attributes {
			if measurement.Attributes[key] != value {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		if len(want) > 0 && measurement.Value != want[0] {
			t.Fatalf("%s value = %v, want %v", name, measurement.Value, want[0])
		}
		return
	}
	t.Fatalf("measurement %q %v not found in %+v", name, attributes, measurements)
}

func TestObservedStreamErrorStopMarksSpanError(t *testing.T) {
	partial := observedPartial()
	final := partial
	final.StopReason = agentcore.StopReasonError
	final.ErrorMessage = "upstream exploded"
	final.Usage = &agentcore.Usage{InputTokens: 5, OutputTokens: 1}
	base := emittingStream(
		provider.StreamStartEvent{Partial: partial},
		provider.StreamErrorEvent{Message: final, Err: errors.New("upstream exploded")},
	)
	memory := telemetry.NewMemory()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := NewObservedStream(base, memory)(ctx, "gpt-5", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	drainObserved(t, ctx, stream)

	span := waitForSettledSpan(t, memory)
	if span.Status.Status != telemetry.SpanStatusError {
		t.Fatalf("status = %+v, want error", span.Status)
	}
	if span.Status.Error == nil || span.Status.Error.Message != "upstream exploded" {
		t.Fatalf("error = %+v", span.Status.Error)
	}
	if got := attribute(t, span, attrStop); got != agentcore.StopReasonError {
		t.Fatalf("stop = %v", got)
	}
	// Usage observed before the failure is still recorded.
	if got := attribute(t, span, attrUsageInput); got != float64(5) {
		t.Fatalf("usage.input = %v", got)
	}
}

func TestObservedStreamAbortedStopKeepsSpanOK(t *testing.T) {
	partial := observedPartial()
	final := partial
	final.StopReason = agentcore.StopReasonAborted
	final.ErrorMessage = "context canceled"
	base := emittingStream(
		provider.StreamStartEvent{Partial: partial},
		provider.StreamErrorEvent{Message: final, Err: context.Canceled},
	)
	memory := telemetry.NewMemory()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := NewObservedStream(base, memory)(ctx, "gpt-5", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	drainObserved(t, ctx, stream)

	span := waitForSettledSpan(t, memory)
	if span.Status.Status != telemetry.SpanStatusOK {
		t.Fatalf("status = %+v, want ok for aborted", span.Status)
	}
	if got := attribute(t, span, attrStop); got != agentcore.StopReasonAborted {
		t.Fatalf("stop = %v", got)
	}
}

func TestObservedStreamCloseWithoutTerminalMarksError(t *testing.T) {
	partial := observedPartial()
	memory := telemetry.NewMemory()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := NewObservedStream(emittingStream(provider.StreamStartEvent{Partial: partial}), memory)(
		ctx, "gpt-5", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Events() {
	}
	if _, err := stream.Result(ctx); err == nil {
		t.Fatal("expected ErrStreamIncomplete")
	}

	span := waitForSettledSpan(t, memory)
	if span.Status.Status != telemetry.SpanStatusError {
		t.Fatalf("status = %+v, want error", span.Status)
	}
	if got := attribute(t, span, attrStop); got != agentcore.StopReasonError {
		t.Fatalf("stop = %v", got)
	}
}

func TestObservedStreamNilTelemetryIsIdentity(t *testing.T) {
	base := emittingStream(provider.StreamDoneEvent{Message: observedPartial()})
	if wrapped := NewObservedStream(base, nil); wrapped == nil {
		t.Fatal("nil telemetry dropped the stream function")
	}
}

func TestObservedStreamBuildFailureRecordsErrorSpan(t *testing.T) {
	base := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		return nil, errors.New("connect refused")
	}
	memory := telemetry.NewMemory()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := NewObservedStream(base, memory)(ctx, "gpt-5", provider.LlmContext{}, provider.StreamConfig{})
	if err == nil || stream != nil {
		t.Fatalf("build failure returned stream=%v err=%v", stream, err)
	}
	span := waitForSettledSpan(t, memory)
	if span.Name != aiRequestSpanName || span.Status.Status != telemetry.SpanStatusError {
		t.Fatalf("span = %+v", span)
	}
	if got := attribute(t, span, attrStop); got != agentcore.StopReasonError {
		t.Fatalf("stop = %v", got)
	}
	if span.Status.Error == nil || span.Status.Error.Message != "connect refused" {
		t.Fatalf("error = %+v", span.Status.Error)
	}
	assertMeasurement(t, memory.Measurements(), aiRequestDurationMetric, nil)
}

func TestObservedStreamCanceledBuildIsAborted(t *testing.T) {
	base := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		return nil, context.Canceled
	}
	memory := telemetry.NewMemory()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := NewObservedStream(base, memory)(ctx, "gpt-5", provider.LlmContext{}, provider.StreamConfig{}); err == nil {
		t.Fatal("canceled build returned no error")
	}
	span := waitForSettledSpan(t, memory)
	if span.Status.Status != telemetry.SpanStatusOK {
		t.Fatalf("status = %+v, want ok for canceled", span.Status)
	}
	if got := attribute(t, span, attrStop); got != agentcore.StopReasonAborted {
		t.Fatalf("stop = %v", got)
	}
}

// rejectingContext is a telemetry adapter that refuses to admit a span.
type rejectingContext struct{}

func (rejectingContext) Metrics() telemetry.Metrics         { return telemetry.NoopMetrics }
func (rejectingContext) Propagation() telemetry.Propagation { return telemetry.NoopPropagation }
func (rejectingContext) StartSpan(telemetry.SpanOptions, func(telemetry.Span) error) error {
	return errors.New("telemetry unavailable")
}

func TestObservedStreamStartSpanRejectionClosesStream(t *testing.T) {
	base := emittingStream(provider.StreamDoneEvent{Message: observedPartial()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	var (
		stream *provider.AssistantMessageEventStream
		err    error
	)
	go func() {
		stream, err = NewObservedStream(base, rejectingContext{})(ctx, "gpt-5", provider.LlmContext{}, provider.StreamConfig{})
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("stream call blocked when StartSpan rejected the callback")
	}
	if err == nil || stream != nil {
		t.Fatalf("rejected span returned stream=%v err=%v", stream, err)
	}
}
