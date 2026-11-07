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

// observeRun wraps one run in a span and records its duration and outcome.
func (r *Runner) observeRun(lease kernel.Lease, execute func() (int64, error)) (int64, error) {
	if r.Telemetry == nil || r.Telemetry.Context == nil {
		return execute()
	}
	attributes := telemetry.SpanAttributes{
		"tenant":   lease.Scope.TenantID.String(),
		"session":  lease.Scope.SessionID.String(),
		"model":    r.Model,
		"provider": r.ProviderName,
	}
	started := time.Now()
	cursor, err := telemetry.StartSpan(r.Telemetry.Context, telemetry.SpanOptions{
		Name:       "keel.agent.run",
		Attributes: attributes,
	}, func(span telemetry.Span) (int64, error) {
		return execute()
	})
	if r.Telemetry.Metrics != nil {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		metricAttributes := telemetry.SpanAttributes{
			"model":   r.Model,
			"outcome": outcome,
		}
		r.Telemetry.Metrics.Counter("keel.agent.runs", nil).Add(1, metricAttributes)
		r.Telemetry.Metrics.Histogram("keel.agent.run.duration", &telemetry.HistogramOptions{
			Unit: "ms",
		}).Record(float64(time.Since(started).Milliseconds()), metricAttributes, nil)
	}
	return cursor, err
}
