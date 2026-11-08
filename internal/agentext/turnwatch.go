package agentext

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jixoai/keel/internal/telemetry"
)

// TurnTelemetry decorates the run telemetry context the worker installs on the
// runner. The kernel's runner already opens one `keel.agent.run` span per
// attempt with the tenant/session attributes (agent.RunTelemetry), so that span
// is the product layer's only dependency-free observation point for the turn
// lifecycle: `turn.started` when it opens, `turn.finished` when it settles.
//
// The decorator adds no I/O and no lock: hook emission is the bus's bounded
// asynchronous queue, and every other span/metric passes through unchanged.
type TurnTelemetry struct {
	Base  telemetry.Context
	Hooks *HookBus
	Now   func() time.Time
}

// RunSpanName is the kernel runner's span name (agent.RunTelemetry).
const RunSpanName = "keel.agent.run"

// Metrics delegates to the wrapped context.
func (t *TurnTelemetry) Metrics() telemetry.Metrics {
	if t.Base == nil {
		return nil
	}
	return t.Base.Metrics()
}

// Propagation delegates to the wrapped context.
func (t *TurnTelemetry) Propagation() telemetry.Propagation {
	if t.Base == nil {
		return nil
	}
	return t.Base.Propagation()
}

// StartSpan wraps the run span with the turn lifecycle hooks.
func (t *TurnTelemetry) StartSpan(options telemetry.SpanOptions, callback func(telemetry.Span) error) error {
	if t.Base == nil {
		return callback(&nopSpan{})
	}
	if options.Name != RunSpanName || t.Hooks == nil {
		return t.Base.StartSpan(options, callback)
	}
	tenantID := attributeString(options.Attributes, "tenant")
	sessionID := attributeString(options.Attributes, "session")
	started := t.now()
	runID := ""
	if sessionID != "" {
		runID = fmt.Sprintf("%s-%d", sessionID, started.UnixMilli())
	}
	if tenantID != "" {
		payload := HookPayload{
			Event:     HookTurnStarted,
			TenantID:  tenantID,
			SessionID: sessionID,
			RunID:     runID,
			Status:    "started",
			At:        started,
		}
		payload.SetData("model", attributeString(options.Attributes, "model"))
		payload.SetData("provider", attributeString(options.Attributes, "provider"))
		t.Hooks.Emit(payload)
	}
	var runErr error
	err := t.Base.StartSpan(options, func(span telemetry.Span) error {
		runErr = callback(span)
		return runErr
	})
	if tenantID != "" {
		status := "ok"
		switch {
		case errors.Is(runErr, context.Canceled), errors.Is(err, context.Canceled):
			status = "cancelled"
		case runErr != nil || err != nil:
			status = "error"
		}
		payload := HookPayload{
			Event:     HookTurnFinished,
			TenantID:  tenantID,
			SessionID: sessionID,
			RunID:     runID,
			Status:    status,
			At:        t.now(),
		}
		payload.SetData("durationMs", fmt.Sprintf("%d", time.Since(started).Milliseconds()))
		// No error text: a provider failure message may echo request details, and
		// hook payloads are built from identifiers only by contract. The status
		// and the hook counters make the failure visible; the log carries detail.
		t.Hooks.Emit(payload)
	}
	return err
}

func (t *TurnTelemetry) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now().UTC()
}

func attributeString(attributes telemetry.SpanAttributes, key string) string {
	if attributes == nil {
		return ""
	}
	value, ok := attributes[key]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}

// nopSpan is a settled span used when the product layer runs without a
// telemetry adapter (the runner still calls the callback exactly once).
type nopSpan struct{}

func (*nopSpan) Metrics() telemetry.Metrics                { return nil }
func (*nopSpan) Propagation() telemetry.Propagation        { return nil }
func (*nopSpan) AddLink(telemetry.SpanLink)                {}
func (*nopSpan) AddEvent(string, telemetry.SpanAttributes) {}
func (*nopSpan) SetAttributes(telemetry.SpanAttributes)    {}
func (*nopSpan) SetStatus(telemetry.SpanStatus)            {}
func (s *nopSpan) StartSpan(_ telemetry.SpanOptions, callback func(telemetry.Span) error) error {
	return callback(s)
}
