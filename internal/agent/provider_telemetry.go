package agent

// Request-level provider telemetry, following PI's pi.ai.request contract.
// The wrapper observes the stream a StreamFn returns; it never inspects or
// records message bodies or response ids, and it adds no network I/O. The
// runner installs it in the composition root, so the loop and providers stay
// unaware of telemetry (provider.StreamConfig.Telemetry is only a seam).

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/telemetry"
)

// PI request-level telemetry names.
const (
	aiRequestSpanName          = "pi.ai.request"
	aiRequestDurationMetric    = "pi.ai.request.duration"
	aiFirstChunkDurationMetric = "pi.ai.first_chunk.duration"
	aiTokensMetric             = "pi.ai.tokens"
)

// Attribute names on the pi.ai.request span and its metrics.
const (
	attrProvider    = "provider"
	attrModel       = "model"
	attrAPI         = "api"
	attrFirstChunk  = "first_chunk"
	attrStop        = "stop"
	attrUsageInput  = "usage.input"
	attrUsageOutput = "usage.output"
	attrCacheRead   = "usage.cache_read"
	attrCacheWrite  = "usage.cache_write"
	attrTokenType   = "type"
)

// NewObservedStream wraps base so every model request records a PI-style
// pi.ai.request span plus duration/first-chunk/token metrics. A nil base or nil
// telemetry returns base unchanged, so telemetry stays optional and costs
// nothing when disabled.
//
// The span starts before base is called, so a connection/build failure is also
// observed; it settles on the terminal event, on context cancellation
// (aborted, status ok), or on an abnormal close (error status). An error stop
// marks the span error; aborted stays ok because the user or the shutdown path
// caused it.
func NewObservedStream(base provider.StreamFn, tel telemetry.Context) provider.StreamFn {
	if base == nil || tel == nil {
		return base
	}
	metrics := tel.Metrics()
	if metrics == nil {
		// The telemetry contract allows a context without the metrics
		// capability; spans still record, measurements become no-ops.
		metrics = telemetry.NoopMetrics
	}
	observer := &streamObserver{
		base: base,
		tel:  tel,
		requestDuration: metrics.Histogram(aiRequestDurationMetric, &telemetry.HistogramOptions{
			InstrumentOptions: telemetry.InstrumentOptions{Unit: "ms", Description: "Provider request wall clock"},
		}),
		firstChunkDuration: metrics.Histogram(aiFirstChunkDurationMetric, &telemetry.HistogramOptions{
			InstrumentOptions: telemetry.InstrumentOptions{Unit: "ms", Description: "Time to first streamed chunk"},
		}),
		tokens: metrics.Counter(aiTokensMetric, &telemetry.InstrumentOptions{
			Description: "Provider-reported token counts by type",
		}),
	}
	return observer.stream
}

type streamObserver struct {
	base               provider.StreamFn
	tel                telemetry.Context
	requestDuration    telemetry.Histogram
	firstChunkDuration telemetry.Histogram
	tokens             telemetry.Counter
}

func (o *streamObserver) stream(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
	startedAt := time.Now()
	out := provider.NewAssistantMessageEventStream(16)
	built := make(chan error, 1)
	var builtOnce sync.Once
	signalBuilt := func(err error) {
		builtOnce.Do(func() { built <- err })
	}

	var callbackRan bool
	go func() {
		startErr := o.tel.StartSpan(telemetry.SpanOptions{
			Name:       aiRequestSpanName,
			Attributes: telemetry.SpanAttributes{attrModel: model},
		}, func(span telemetry.Span) error {
			callbackRan = true
			return o.observe(ctx, model, startedAt, llm, cfg, out, signalBuilt, span)
		})
		if callbackRan {
			return
		}
		// The adapter rejected the request before admitting the callback. Close
		// the stream so a consumer never blocks on a channel that has no
		// producer, and fail the StreamFn call.
		if startErr == nil {
			startErr = errors.New("telemetry: span callback was not admitted")
		}
		out.SetError(startErr)
		out.Close()
		signalBuilt(startErr)
	}()

	if err := <-built; err != nil {
		return nil, err
	}
	return out, nil
}

// observe builds the provider stream inside the span and, on success, hands the
// stream to the consumer before draining it.
func (o *streamObserver) observe(ctx context.Context, model string, startedAt time.Time, llm provider.LlmContext,
	cfg provider.StreamConfig, out *provider.AssistantMessageEventStream, signalBuilt func(error), span telemetry.Span) error {
	base, err := o.base(ctx, model, llm, cfg)
	if err != nil {
		status := aiErrorStatus(err, "")
		stop := agentcore.StopReasonError
		if errors.Is(err, context.Canceled) {
			status = telemetry.OK()
			stop = agentcore.StopReasonAborted
		}
		span.SetAttributes(telemetry.SpanAttributes{attrStop: stop})
		span.SetStatus(status)
		o.recordMetrics(span, "", model, "", stop, nil, 0, false, startedAt)
		out.SetError(err)
		out.Close()
		signalBuilt(err)
		return nil
	}
	if base == nil {
		err := errors.New("observed stream: base returned a nil stream")
		span.SetAttributes(telemetry.SpanAttributes{attrStop: agentcore.StopReasonError})
		span.SetStatus(aiErrorStatus(err, ""))
		o.recordMetrics(span, "", model, "", agentcore.StopReasonError, nil, 0, false, startedAt)
		out.SetError(err)
		out.Close()
		signalBuilt(err)
		return nil
	}
	signalBuilt(nil)
	return o.consume(ctx, model, startedAt, base, out, span)
}

// consume forwards every base event to the observed stream and settles the span
// around the whole exchange.
func (o *streamObserver) consume(ctx context.Context, model string, startedAt time.Time, base, out *provider.AssistantMessageEventStream, span telemetry.Span) error {
	var (
		providerName, apiValue, stop string
		usage                        *agentcore.Usage
		firstChunk                   time.Duration
		haveFirstChunk               bool
		terminal, failed             bool
		failure                      error
		failureMessage               string
	)

	observeMessage := func(message agentcore.AssistantMessage) {
		updated := telemetry.SpanAttributes{}
		if providerName == "" && message.Provider != "" {
			providerName = message.Provider
			updated[attrProvider] = providerName
		}
		if apiValue == "" && message.API != "" {
			apiValue = message.API
			updated[attrAPI] = apiValue
		}
		if len(updated) > 0 {
			span.SetAttributes(updated)
		}
		if message.Usage != nil {
			usage = message.Usage
		}
		if message.StopReason != "" {
			stop = message.StopReason
		}
	}
	markFirstChunk := func() {
		if haveFirstChunk {
			return
		}
		haveFirstChunk = true
		firstChunk = time.Since(startedAt)
	}
	finish := func(status telemetry.SpanStatus) {
		if failed && !terminal {
			// A stream that closed without a terminal event is abnormal; its
			// stop attribute must not echo a partial's stale stop reason.
			stop = agentcore.StopReasonError
		}
		if stop == "" {
			if terminal && !failed {
				stop = agentcore.StopReasonEndTurn
			} else {
				stop = agentcore.StopReasonError
			}
		}
		span.SetAttributes(telemetry.SpanAttributes{attrStop: stop})
		if haveFirstChunk {
			span.SetAttributes(telemetry.SpanAttributes{attrFirstChunk: durationMillis(firstChunk)})
		}
		if usage != nil {
			span.SetAttributes(telemetry.SpanAttributes{
				attrUsageInput:  usage.InputTokens,
				attrUsageOutput: usage.OutputTokens,
				attrCacheRead:   usage.CacheReadTokens,
				attrCacheWrite:  usage.CacheWriteTokens,
			})
		}
		span.SetStatus(status)
		o.recordMetrics(span, providerName, model, apiValue, stop, usage, firstChunk, haveFirstChunk, startedAt)
	}

	for {
		select {
		case event, ok := <-base.Events():
			if !ok {
				if !terminal {
					failed = true
					failureMessage = "provider stream closed without a terminal event"
					finish(telemetry.ErrorStatus(&telemetry.SpanError{Name: "error", Message: failureMessage}))
				}
				out.Close()
				return nil
			}
			switch e := event.(type) {
			case provider.StreamStartEvent:
				observeMessage(e.Partial)
			case provider.StreamTextEvent:
				observeMessage(e.Partial)
				markFirstChunk()
			case provider.StreamThinkingEvent:
				observeMessage(e.Partial)
				markFirstChunk()
			case provider.StreamToolCallEvent:
				observeMessage(e.Partial)
				markFirstChunk()
			case provider.StreamDoneEvent:
				terminal = true
				observeMessage(e.Message)
				failed = e.Message.StopReason == agentcore.StopReasonError
				failureMessage = e.Message.ErrorMessage
			case provider.StreamErrorEvent:
				terminal = true
				observeMessage(e.Message)
				failed = e.Message.StopReason != agentcore.StopReasonAborted
				failure = e.Err
				failureMessage = e.Message.ErrorMessage
			}
			emitErr := out.Emit(ctx, event)
			if emitErr != nil {
				// The consumer cancelled or stopped reading. Settle as an
				// aborted request: the locals already hold the usage and stop
				// reason observed so far.
				if stop == "" {
					stop = agentcore.StopReasonAborted
				}
				if terminal && failed {
					finish(aiErrorStatus(failure, failureMessage))
				} else {
					finish(telemetry.OK())
				}
				out.Close()
				return nil
			}
			if terminal {
				if failed {
					finish(aiErrorStatus(failure, failureMessage))
				} else {
					finish(telemetry.OK())
				}
				out.Close()
				return nil
			}
		case <-ctx.Done():
			if stop == "" {
				stop = agentcore.StopReasonAborted
			}
			finish(telemetry.OK())
			out.Close()
			return nil
		}
	}
}

// recordMetrics emits the three PI request instruments. Token counts are only
// recorded when non-zero: a missing counter means "upstream did not report it",
// and a zero measurement would blur that with a real zero.
func (o *streamObserver) recordMetrics(span telemetry.Span, providerName, model, apiValue, stop string, usage *agentcore.Usage,
	firstChunk time.Duration, haveFirstChunk bool, startedAt time.Time) {
	baseAttributes := telemetry.SpanAttributes{attrModel: model}
	if providerName != "" {
		baseAttributes[attrProvider] = providerName
	}
	if apiValue != "" {
		baseAttributes[attrAPI] = apiValue
	}
	durationAttributes := make(telemetry.SpanAttributes, len(baseAttributes)+1)
	for name, value := range baseAttributes {
		durationAttributes[name] = value
	}
	if stop != "" {
		durationAttributes[attrStop] = stop
	}
	o.requestDuration.Record(durationMillis(time.Since(startedAt)), durationAttributes, span)
	if haveFirstChunk {
		o.firstChunkDuration.Record(durationMillis(firstChunk), baseAttributes, span)
	}
	if usage == nil {
		return
	}
	for _, token := range []struct {
		kind  string
		value int
	}{
		{"input", usage.InputTokens},
		{"output", usage.OutputTokens},
		{"cache_read", usage.CacheReadTokens},
		{"cache_write", usage.CacheWriteTokens},
	} {
		if token.value == 0 {
			continue
		}
		attributes := telemetry.SpanAttributes{attrModel: model, attrTokenType: token.kind}
		for name, value := range baseAttributes {
			attributes[name] = value
		}
		o.tokens.Add(float64(token.value), attributes)
	}
}

func aiErrorStatus(err error, message string) telemetry.SpanStatus {
	if err != nil {
		return telemetry.ErrorStatusFromError(err)
	}
	if message == "" {
		message = "ai request failed"
	}
	return telemetry.ErrorStatus(&telemetry.SpanError{Name: "error", Message: message})
}

func durationMillis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
