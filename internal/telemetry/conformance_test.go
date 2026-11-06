// Ported from pi/packages/telemetry/test/conformance.test.ts:1-49,
// test/telemetry.test.ts:138-186, and src/testing/conformance.ts:44-386
// @ 55a5c6914570e2ae31ca36297654483bbbf46883.
// Core callback-adapter cases: capabilities, callback lifecycle, status,
// recording, settlement, and parentage.

package telemetry

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestConformanceCapabilities(t *testing.T) {
	memory := NewMemory()
	assertCapabilities := func(context Context) {
		t.Helper()
		if context.Metrics() != memory {
			t.Fatalf("context metrics = %#v, want recorder", context.Metrics())
		}
		if context.Propagation() != memory {
			t.Fatalf("context propagation = %#v, want recorder", context.Propagation())
		}
	}

	assertCapabilities(memory)

	var settled Span
	err := memory.StartSpan(SpanOptions{Name: "capabilities"}, func(span Span) error {
		settled = span
		assertCapabilities(span)
		if err := span.StartSpan(SpanOptions{Name: "capabilities.child"}, func(child Span) error {
			assertCapabilities(child)
			return nil
		}); err != nil {
			return err
		}
		carrier, ok := memory.Inject(span)
		if !ok {
			return errors.New("expected injected carrier")
		}
		parent := memory.Extract(carrier)
		assertCapabilities(parent)
		return parent.StartSpan(SpanOptions{Name: "capabilities.extracted"}, func(extracted Span) error {
			assertCapabilities(extracted)
			return nil
		})
	})
	requireError(t, err)
	if settled == nil {
		t.Fatal("expected callback span")
	}

	// A settled span still advertises adapter capabilities to inert children.
	if err := settled.StartSpan(SpanOptions{Name: "capabilities.inert"}, func(inert Span) error {
		assertCapabilities(inert)
		return nil
	}); err != nil {
		t.Fatalf("inert child: %v", err)
	}
}

func TestConformancePropagationRoundTrip(t *testing.T) {
	memory := NewMemory()
	var sender Span
	var senderCarrier Carrier
	err := memory.StartSpan(SpanOptions{Name: "sender"}, func(span Span) error {
		sender = span
		carrier, ok := memory.Inject(span)
		if !ok {
			return errors.New("expected injected carrier")
		}
		senderCarrier = carrier
		wire := jsonRoundTrip(t, carrier)
		parent := memory.Extract(wire)
		for key := range wire {
			wire[key] = "mutated"
		}
		// Mutating the wire after Extract must not affect the resumed parent.
		return parent.StartSpan(SpanOptions{Name: "receiver"}, func(Span) error { return nil })
	})
	requireError(t, err)

	injected, ok := memory.Inject(sender)
	if !ok || !reflect.DeepEqual(injected, senderCarrier) {
		t.Fatalf("reinject = %v (ok=%v), want %v", injected, ok, senderCarrier)
	}
	spans := memory.Spans()
	receiver := spanByName(t, spans, "receiver")
	senderRecord := spanByName(t, spans, "sender")
	if !reflect.DeepEqual(receiver.ParentContext, senderRecord.Context) {
		t.Fatalf("receiver parent context = %v, want %v", receiver.ParentContext, senderRecord.Context)
	}
	requireEqual(t, "receiver trace ID", receiver.Context[carrierTraceKey], senderRecord.Context[carrierTraceKey])
	requireParentID(t, receiver, senderRecord.ID)
}

func TestConformanceAbsentAndUnknownCarriersAreRoots(t *testing.T) {
	memory := NewMemory()
	if _, ok := memory.Inject(NoopContext); ok {
		t.Fatal("expected no carrier for noop context")
	}
	carriers := []Carrier{nil, {}, {"unknown": "format"}}
	for index, carrier := range carriers {
		context := memory.Extract(carrier)
		if context != memory {
			t.Fatalf("extract(%v) = %#v, want recorder root", carrier, context)
		}
		name := fmt.Sprintf("fallback-%d", index)
		result, err := StartSpan(context, SpanOptions{Name: name}, func(Span) (int, error) { return 42, nil })
		if err != nil || result != 42 {
			t.Fatalf("fallback span = %d, %v", result, err)
		}
	}
	for _, span := range memory.Spans() {
		requireRoot(t, span)
	}
}

func TestConformancePropagationAcrossFreshAdapters(t *testing.T) {
	sender := NewMemory()
	receiver := NewMemory()
	worker := NewMemory()

	var senderCarrier Carrier
	requireError(t, sender.StartSpan(SpanOptions{Name: "sender"}, func(span Span) error {
		carrier, ok := sender.Inject(span)
		if !ok {
			return errors.New("expected sender carrier")
		}
		senderCarrier = carrier
		return nil
	}))

	var receiverCarrier Carrier
	requireError(t, receiver.Extract(jsonRoundTrip(t, senderCarrier)).StartSpan(
		SpanOptions{Name: "receiver"},
		func(span Span) error {
			carrier, ok := receiver.Inject(span)
			if !ok {
				return errors.New("expected receiver carrier")
			}
			receiverCarrier = carrier
			return nil
		},
	))
	requireError(t, worker.Extract(jsonRoundTrip(t, receiverCarrier)).StartSpan(
		SpanOptions{Name: "worker"},
		func(Span) error { return nil },
	))

	if got := mustSpan(t, receiver, "receiver").ParentContext; !reflect.DeepEqual(got, senderCarrier) {
		t.Fatalf("receiver parent = %v, want %v", got, senderCarrier)
	}
	if got := mustSpan(t, worker, "worker").ParentContext; !reflect.DeepEqual(got, receiverCarrier) {
		t.Fatalf("worker parent = %v, want %v", got, receiverCarrier)
	}
}

func TestConformanceCallbackLifecycle(t *testing.T) {
	memory := NewMemory()
	type result struct{ Value int }

	var calls int
	value, err := StartSpan(memory, SpanOptions{Name: "success"}, func(Span) (result, error) {
		calls++
		return result{Value: 42}, nil
	})
	requireError(t, err)
	requireEqual(t, "calls", calls, 1)
	requireEqual(t, "result", value.Value, 42)

	span := mustSpan(t, memory, "success")
	requireSpanStatus(t, span, SpanStatusOK)
	if !span.Settled {
		t.Fatal("expected settled span")
	}
}

func TestConformancePreservesRejectionValues(t *testing.T) {
	memory := NewMemory()
	syncError := errors.New("sync")
	_, err := StartSpan(memory, SpanOptions{Name: "sync-error"}, func(Span) (int, error) {
		return 0, syncError
	})
	if !errors.Is(err, syncError) {
		t.Fatalf("sync error = %v, want %v", err, syncError)
	}

	asyncError := errors.New("async")
	_, err = StartSpan(memory, SpanOptions{Name: "async-error"}, func(Span) (int, error) {
		return 0, asyncError
	})
	if !errors.Is(err, asyncError) {
		t.Fatalf("async error = %v, want %v", err, asyncError)
	}

	panicValue := &struct{ Reason string }{Reason: "panic"}
	func() {
		defer func() {
			if recovered := recover(); recovered != panicValue {
				t.Fatalf("panic = %#v, want %#v", recovered, panicValue)
			}
		}()
		_ = memory.StartSpan(SpanOptions{Name: "panic-error"}, func(Span) error {
			panic(panicValue)
		})
	}()

	for _, name := range []string{"sync-error", "async-error", "panic-error"} {
		requireSpanStatus(t, mustSpan(t, memory, name), SpanStatusError)
	}
}

func TestConformanceExplicitStatusWins(t *testing.T) {
	memory := NewMemory()
	requireError(t, memory.StartSpan(SpanOptions{Name: "last-status"}, func(span Span) error {
		span.SetStatus(ErrorStatus(&SpanError{Name: "Expected", Message: "first"}))
		span.SetStatus(OK())
		return nil
	}))

	thrown := errors.New("after explicit status")
	_, err := StartSpan(memory, SpanOptions{Name: "explicit-before-error"}, func(span Span) (int, error) {
		span.SetStatus(OK())
		return 0, thrown
	})
	if !errors.Is(err, thrown) {
		t.Fatalf("explicit-before-error returned %v", err)
	}

	rejected := errors.New("after async explicit status")
	_, err = StartSpan(memory, SpanOptions{Name: "explicit-before-rejection"}, func(span Span) (int, error) {
		span.SetStatus(ErrorStatus(&SpanError{Name: "Expected", Message: "async failure"}))
		return 0, rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatalf("explicit-before-rejection returned %v", err)
	}

	requireError(t, memory.StartSpan(SpanOptions{Name: "expected-failure"}, func(span Span) error {
		span.SetStatus(ErrorStatus(&SpanError{Name: "Expected", Message: "returned failure"}))
		return nil
	}))

	spans := memory.Spans()
	if status := spanByName(t, spans, "last-status").Status; status.Status != SpanStatusOK {
		t.Fatalf("last-status = %+v, want ok", status)
	}
	if status := spanByName(t, spans, "explicit-before-error").Status; status.Status != SpanStatusOK {
		t.Fatalf("explicit-before-error = %+v, want ok", status)
	}
	expected := ErrorStatus(&SpanError{Name: "Expected", Message: "async failure"})
	if status := spanByName(t, spans, "explicit-before-rejection").Status; !reflect.DeepEqual(status, expected) {
		t.Fatalf("explicit-before-rejection = %+v, want %+v", status, expected)
	}
	expected = ErrorStatus(&SpanError{Name: "Expected", Message: "returned failure"})
	if status := spanByName(t, spans, "expected-failure").Status; !reflect.DeepEqual(status, expected) {
		t.Fatalf("expected-failure = %+v, want %+v", status, expected)
	}
}

func TestConformanceMergesAttributesAndEvents(t *testing.T) {
	memory := NewMemory()
	requireError(t, memory.StartSpan(SpanOptions{
		Name: "recording",
		Attributes: SpanAttributes{
			"start":     "value",
			"overwrite": "start",
			"ignored":   nil,
		},
	}, func(span Span) error {
		span.SetAttributes(SpanAttributes{"count": 1, "overwrite": "middle"})
		span.SetAttributes(SpanAttributes{"count": nil, "overwrite": "end"})
		span.AddEvent("first", SpanAttributes{"index": 1, "ignored": nil})
		span.AddEvent("second", SpanAttributes{"index": 2})
		return nil
	}))

	span := mustSpan(t, memory, "recording")
	requireAttributes(t, span.Attributes, SpanAttributes{
		"start":     "value",
		"overwrite": "end",
		"count":     float64(1),
	})
	if len(span.Events) != 2 {
		t.Fatalf("events = %v, want 2", span.Events)
	}
	requireEqual(t, "first event", span.Events[0].Name, "first")
	requireAttributes(t, span.Events[0].Attributes, SpanAttributes{"index": float64(1)})
	requireEqual(t, "second event", span.Events[1].Name, "second")
	requireAttributes(t, span.Events[1].Attributes, SpanAttributes{"index": float64(2)})
}

func TestConformanceCallsAfterSettlementAreInert(t *testing.T) {
	memory := NewMemory()
	var captured Span
	requireError(t, memory.StartSpan(SpanOptions{
		Name:       "settled",
		Attributes: SpanAttributes{"value": "initial"},
	}, func(span Span) error {
		captured = span
		return nil
	}))
	if captured == nil {
		t.Fatal("expected callback span")
	}

	captured.SetAttributes(SpanAttributes{"value": "late"})
	captured.AddEvent("late", SpanAttributes{"value": true})
	captured.SetStatus(ErrorStatus(nil))
	captured.AddLink(SpanLink{Context: captured})

	var childAdmitted bool
	value, err := StartSpan(captured, SpanOptions{Name: "late-child"}, func(Span) (int, error) {
		childAdmitted = true
		return 7, nil
	})
	requireError(t, err)
	if !childAdmitted {
		t.Fatal("inert child callback was not admitted")
	}
	requireEqual(t, "late child result", value, 7)

	spans := memory.Spans()
	if len(spans) != 1 {
		t.Fatalf("spans = %v, want only the settled span", spanNames(spans))
	}
	requireAttributes(t, spans[0].Attributes, SpanAttributes{"value": "initial"})
	if len(spans[0].Events) != 0 {
		t.Fatalf("events = %v, want none", spans[0].Events)
	}
	requireSpanStatus(t, spans[0], SpanStatusOK)
}

func TestConformanceParentageIncludesConcurrentChildren(t *testing.T) {
	memory := NewMemory()
	release := make(chan struct{})
	var firstErr error
	err := memory.StartSpan(SpanOptions{Name: "parent"}, func(parent Span) error {
		firstDone := make(chan error, 1)
		go func() {
			firstDone <- parent.StartSpan(SpanOptions{Name: "first-child"}, func(Span) error {
				<-release
				return nil
			})
		}()
		secondErr := parent.StartSpan(SpanOptions{Name: "second-child"}, func(Span) error {
			return nil
		})
		close(release)
		firstErr = <-firstDone
		return secondErr
	})
	requireError(t, err)
	requireError(t, firstErr)

	spans := memory.Spans()
	if len(spans) != 3 {
		t.Fatalf("spans = %v, want parent and two children", spanNames(spans))
	}
	parent := spanByName(t, spans, "parent")
	first := spanByName(t, spans, "first-child")
	second := spanByName(t, spans, "second-child")
	requireRoot(t, parent)
	requireParentID(t, first, parent.ID)
	requireParentID(t, second, parent.ID)
	if first.EndSequence == nil || second.EndSequence == nil || parent.EndSequence == nil {
		t.Fatalf("end sequences: first=%v second=%v parent=%v", first.EndSequence, second.EndSequence, parent.EndSequence)
	}
	if !(*second.EndSequence < *first.EndSequence && *first.EndSequence < *parent.EndSequence) {
		t.Fatalf("end sequence order: second=%d first=%d parent=%d",
			*second.EndSequence, *first.EndSequence, *parent.EndSequence)
	}
}

func TestConformanceSnapshotsAreDetached(t *testing.T) {
	memory := NewMemory()
	requireError(t, memory.StartSpan(SpanOptions{
		Name:       "snapshot",
		Attributes: SpanAttributes{"tags": []string{"initial"}},
	}, func(span Span) error {
		span.AddEvent("event", SpanAttributes{"value": 1})
		open := memory.Spans()[0]
		if open.Settled {
			t.Fatal("open span reported settled")
		}
		if open.EndSequence != nil {
			t.Fatalf("open span end sequence = %d, want nil", *open.EndSequence)
		}
		return nil
	}))

	first := memory.Spans()[0]
	if !first.Settled || first.EndSequence == nil || *first.EndSequence != 1 {
		t.Fatalf("settled snapshot = %+v", first)
	}
	first.Attributes["tags"] = []string{"mutated"}
	first.Events[0].Attributes["value"] = 2

	second := memory.Spans()[0]
	requireAttributes(t, second.Attributes, SpanAttributes{"tags": []string{"initial"}})
	requireAttributes(t, second.Events[0].Attributes, SpanAttributes{"value": float64(1)})
}
