// Ported from pi/packages/telemetry/test/propagation.test.ts:14-139
// @ 55a5c6914570e2ae31ca36297654483bbbf46883.
// The cross-process worker case becomes a JSON round-trip: Carrier is an
// explicit string map, so crossing a process boundary is encoding/json only.

package telemetry

import (
	"reflect"
	"testing"
)

func TestLinksRecordInitialAndLateLinks(t *testing.T) {
	memory := NewMemory()
	var target Span
	requireError(t, memory.StartSpan(SpanOptions{Name: "target"}, func(span Span) error {
		target = span
		return nil
	}))
	if target == nil {
		t.Fatal("expected target span")
	}

	initial := SpanAttributes{"reason": []string{"initial"}}
	requireError(t, memory.StartSpan(SpanOptions{
		Name:  "linked",
		Links: []SpanLink{{Context: target, Attributes: initial}},
	}, func(span Span) error {
		initial["reason"].([]string)[0] = "mutated"
		span.AddLink(SpanLink{Context: target, Attributes: SpanAttributes{"reason": "late"}})
		span.AddLink(SpanLink{Context: NoopContext})
		return nil
	}))

	spans := memory.Spans()
	linked := spanByName(t, spans, "linked")
	targetRecord := spanByName(t, spans, "target")
	requireRoot(t, linked)
	if len(linked.Links) != 2 {
		t.Fatalf("links = %v, want 2 (noop link dropped)", linked.Links)
	}
	if !reflect.DeepEqual(linked.Links[0].Context, targetRecord.Context) {
		t.Fatalf("first link context = %v, want %v", linked.Links[0].Context, targetRecord.Context)
	}
	requireAttributes(t, linked.Links[0].Attributes, SpanAttributes{"reason": []string{"initial"}})
	requireAttributes(t, linked.Links[1].Attributes, SpanAttributes{"reason": "late"})
}

func TestLinksAfterSourceSettlementIgnored(t *testing.T) {
	memory := NewMemory()
	var source Span
	requireError(t, memory.StartSpan(SpanOptions{Name: "link-source"}, func(span Span) error {
		source = span
		return nil
	}))
	source.AddLink(SpanLink{Context: source})
	if links := mustSpan(t, memory, "link-source").Links; len(links) != 0 {
		t.Fatalf("links = %v, want none", links)
	}
}

func TestPropagationDetachesCarrierAndLinkSnapshots(t *testing.T) {
	memory := NewMemory()
	var target Span
	requireError(t, memory.StartSpan(SpanOptions{Name: "target"}, func(span Span) error {
		target = span
		return nil
	}))

	original, ok := memory.Inject(target)
	if !ok {
		t.Fatal("expected carrier")
	}
	injected, ok := memory.Inject(target)
	if !ok {
		t.Fatal("expected carrier")
	}
	injected[carrierSpanKey] = "999"
	reinjected, _ := memory.Inject(target)
	if !reflect.DeepEqual(reinjected, original) {
		t.Fatalf("carrier = %v, want detached %v", reinjected, original)
	}

	requireError(t, memory.StartSpan(SpanOptions{
		Name:  "linked",
		Links: []SpanLink{{Context: target, Attributes: SpanAttributes{"tags": []string{"original"}}}},
	}, func(Span) error { return nil }))

	first := memory.Spans()[0]
	second := memory.Spans()[1]
	first.Context[carrierSpanKey] = "123"
	second.Links[0].Context[carrierSpanKey] = "456"
	second.Links[0].Attributes["tags"] = []string{"mutated"}

	fresh := memory.Spans()[1]
	if !reflect.DeepEqual(fresh.Links[0].Context, original) {
		t.Fatalf("link context = %v, want %v", fresh.Links[0].Context, original)
	}
	requireAttributes(t, fresh.Links[0].Attributes, SpanAttributes{"tags": []string{"original"}})
}

func TestLinksNewRootToExtractedRemoteTarget(t *testing.T) {
	sender := NewMemory()
	receiver := NewMemory()
	var carrier Carrier
	requireError(t, sender.StartSpan(SpanOptions{Name: "trigger"}, func(span Span) error {
		injected, ok := sender.Inject(span)
		if !ok {
			t.Fatal("expected sender carrier")
		}
		carrier = injected
		return nil
	}))

	target := receiver.Extract(jsonRoundTrip(t, carrier))
	requireError(t, receiver.StartSpan(SpanOptions{
		Name:  "background",
		Links: []SpanLink{{Context: target, Attributes: SpanAttributes{"reason": "restart"}}},
	}, func(span Span) error {
		span.AddLink(SpanLink{Context: target})
		return nil
	}))

	record := mustSpan(t, receiver, "background")
	requireRoot(t, record)
	if len(record.Links) != 2 {
		t.Fatalf("links = %v, want 2", record.Links)
	}
	expected := []RecordedLink{
		{Context: carrier, Attributes: SpanAttributes{"reason": "restart"}},
		{Context: carrier, Attributes: SpanAttributes{}},
	}
	if !reflect.DeepEqual(record.Links, expected) {
		t.Fatalf("links = %v, want %v", record.Links, expected)
	}
}

func TestPropagationTreatsRecognizedIdentifiersAsOpaque(t *testing.T) {
	memory := NewMemory()
	carrier := Carrier{
		carrierScopeKey: "external-recorder",
		carrierSpanKey:  "external-span",
		carrierTraceKey: "external-trace",
	}
	requireError(t, memory.Extract(carrier).StartSpan(SpanOptions{Name: "receiver"}, func(Span) error {
		return nil
	}))
	record := mustSpan(t, memory, "receiver")
	if !reflect.DeepEqual(record.ParentContext, carrier) {
		t.Fatalf("parent context = %v, want %v", record.ParentContext, carrier)
	}
}
