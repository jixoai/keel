// Runner-level telemetry, following pi's callback-scoped span model
// (packages/telemetry). The AI request span/metrics live with the provider; this
// span covers one durable run and makes the runtime observable without
// coupling the kernel to an exporter.

package agent

import (
	"time"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/telemetry"
)

// RunTelemetry is the optional observability seam of a Runner.
type RunTelemetry struct {
	Context telemetry.Context
	Metrics telemetry.Metrics
}

// observeRun wraps one run in a span and records its duration and outcome. The
// callback receives the span so the attempt can publish the identity it
// actually ran with (a multi-model tenant resolves per attempt, so the boot
// identity is not the run's identity).
func (r *Runner) observeRun(lease kernel.Lease, execute func(telemetry.Span) (int64, error)) (int64, error) {
	if r.Telemetry == nil || r.Telemetry.Context == nil {
		return execute(nil)
	}
	attributes := telemetry.SpanAttributes{
		"tenant":   lease.Scope.TenantID.String(),
		"session":  lease.Scope.SessionID.String(),
		"model":    r.Model,
		"provider": r.ProviderName,
	}
	cursor, err := telemetry.StartSpan(r.Telemetry.Context, telemetry.SpanOptions{
		Name:       "keel.agent.run",
		Attributes: attributes,
	}, func(span telemetry.Span) (int64, error) {
		return execute(span)
	})
	return cursor, err
}

// recordRunMetrics records the run counter and duration histogram with the
// identity the attempt actually resolved to (the boot defaults are only the
// label of a runtime that never got as far as resolving one).
func (r *Runner) recordRunMetrics(res RunResult, started time.Time, err error) {
	if r.Telemetry == nil || r.Telemetry.Metrics == nil {
		return
	}
	// The outcome follows the attempt's terminal status, not just the returned
	// error: a provider turn failure rides the loop as a terminal assistant
	// message and settles the run failed without a Go error.
	// The status is the settled attempt outcome (a cancellation is stamped by the
	// worker, which owns the terminal state, so it is not visible here).
	outcome := "ok"
	if err != nil || res.Summary.Status == kernel.RunSummaryFailed {
		outcome = "error"
	}
	model := res.Summary.Model
	if model == "" {
		model = r.Model
	}
	provider := res.Summary.Provider
	if provider == "" {
		provider = r.ProviderName
	}
	metricAttributes := telemetry.SpanAttributes{
		"model":    model,
		"provider": provider,
		"outcome":  outcome,
	}
	r.Telemetry.Metrics.Counter("keel.agent.runs", nil).Add(1, metricAttributes)
	r.Telemetry.Metrics.Histogram("keel.agent.run.duration", &telemetry.HistogramOptions{
		Unit: "ms",
	}).Record(float64(time.Since(started).Milliseconds()), metricAttributes, nil)
}
