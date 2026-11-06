// Ported from pi/packages/telemetry/test/telemetry.test.ts:138-186 and
// test/propagation.test.ts:116-127 @ 55a5c6914570e2ae31ca36297654483bbbf46883.

package telemetry

import (
	"errors"
	"testing"
)

func TestNoopContextAdmitsSynchronouslyAndReusesSpan(t *testing.T) {
	var first Span
	result, err := StartSpan(NoopContext, SpanOptions{Name: "first"}, func(span Span) (int, error) {
		first = span
		child, childErr := StartSpan(span, SpanOptions{Name: "child"}, func(child Span) (Span, error) {
			return child, nil
		})
		if childErr != nil {
			return 0, childErr
		}
		if child != span {
			return 0, errors.New("noop child was not the shared span")
		}
		return 42, nil
	})
	requireError(t, err)
	requireEqual(t, "result", result, 42)
	if first != NoopContext {
		t.Fatalf("callback span = %#v, want shared noop context", first)
	}
	if NoopContext.Metrics() != NoopMetrics {
		t.Fatal("noop metrics identity changed")
	}
	if NoopContext.Propagation() != NoopPropagation {
		t.Fatal("noop propagation identity changed")
	}
	if first.Metrics() != NoopMetrics || first.Propagation() != NoopPropagation {
		t.Fatal("noop span lost adapter capabilities")
	}
}

func TestNoopContextPreservesErrorsAndPanics(t *testing.T) {
	syncError := errors.New("sync")
	_, err := StartSpan(NoopContext, SpanOptions{Name: "sync"}, func(Span) (int, error) {
		return 0, syncError
	})
	if !errors.Is(err, syncError) {
		t.Fatalf("error = %v, want %v", err, syncError)
	}

	panicValue := &struct{ Kind string }{Kind: "panic"}
	func() {
		defer func() {
			if recovered := recover(); recovered != panicValue {
				t.Fatalf("panic = %#v, want %#v", recovered, panicValue)
			}
		}()
		_ = NoopContext.StartSpan(SpanOptions{Name: "panic"}, func(Span) error {
			panic(panicValue)
		})
	}()
}

func TestNoopContextIgnoresRecordingPayloads(t *testing.T) {
	result, err := StartSpan(NoopContext, SpanOptions{
		Name:       "operation",
		Attributes: SpanAttributes{"kind": "test"},
	}, func(span Span) (int, error) {
		span.AddEvent("event", SpanAttributes{"count": 1})
		span.SetAttributes(SpanAttributes{"complete": true})
		span.SetStatus(ErrorStatus(&SpanError{Name: "Expected", Message: "test"}))
		span.AddLink(SpanLink{Context: span})
		return 42, nil
	})
	requireError(t, err)
	requireEqual(t, "result", result, 42)
}

func TestNoopPropagationInspectsNothing(t *testing.T) {
	if carrier, ok := NoopPropagation.Inject(NoopContext); ok || carrier != nil {
		t.Fatalf("inject = %v, %v; want nil, false", carrier, ok)
	}
	if extracted := NoopPropagation.Extract(Carrier{"unknown": "format"}); extracted != NoopContext {
		t.Fatalf("extract = %#v, want noop context", extracted)
	}
}
