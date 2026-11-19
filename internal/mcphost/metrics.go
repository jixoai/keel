package mcphost

// MCP metrics (change feat-mcp-apps-events, P4.7). The set is fixed and every
// label value comes from a closed vocabulary; session, tenant, app instance
// and request identities never appear, so the series count does not grow with
// sessions or with the number of app instances.
//
// Bounded series budget (fixed at construction, independent of traffic):
//
//	mcp_servers{state}                   2 series (connected|disconnected)
//	mcp_surface_generation               1 series
//	mcp_app_fetch_total{result}          4 series (ok|error|reused|other)
//	mcp_app_bridge_frames_total{...}    15 series (5 methods × 3 directions)
//	mcp_listen_reconnects_total          1 series
// mcp_mrtr_rounds                      1 histogram (6 buckets + sum/count)
//
// That is 24 logical samples, exported as 31 Prometheus wire series: the MRTR
// histogram contributes 8 series on the wire (5 finite buckets + +Inf, _sum and
// _count) in place of its one logical sample. The upper bound is asserted in
// metrics_test.go.
//
// The `server` label is deliberately absent: one series per server would grow
// with configuration, and the task's contract is `mcp_servers{state}`.

import (
	"github.com/jixoai/keel/internal/metrics"
)

// Metrics is the MCP metric set. A nil *Metrics is a valid no-op, so a
// deployment without a registry pays nothing.
type Metrics struct {
	servers           *metrics.GaugeVec
	surfaceGeneration *metrics.Gauge
	appFetch          *metrics.CounterVec
	bridgeFrames      *metrics.CounterVec
	listenReconnects  *metrics.Counter
	mrtrRounds        *metrics.Histogram
}

// NewMetrics registers the MCP families on reg. A nil registry returns a no-op
// set (the deployment does not export Prometheus text).
func NewMetrics(reg *metrics.Registry) *Metrics {
	if reg == nil {
		return nil
	}
	return &Metrics{
		servers: reg.NewGaugeVec("mcp_servers",
			"MCP servers in each connection state (connected|disconnected).", "state"),
		surfaceGeneration: reg.NewGauge("mcp_surface_generation",
			"Monotonic generation of the published MCP surface."),
		appFetch: reg.NewCounterVec("mcp_app_fetch_total",
			"App document resolutions by result (ok|error|reused|other).", "result"),
		bridgeFrames: reg.NewCounterVec("mcp_app_bridge_frames_total",
			"App bridge frames by method (tools/call, resources/read, ui/message, ui/open-link, other) and direction (in|out).",
			"method", "direction"),
		listenReconnects: reg.NewCounter("mcp_listen_reconnects_total",
			"subscriptions/listen reconnect attempts."),
		mrtrRounds: reg.NewHistogram("mcp_mrtr_rounds",
			"MRTR input-required rounds per tool call.", []float64{1, 2, 3, 4, 5}),
	}
}

// ServerStates records the per-state server totals.
func (m *Metrics) ServerStates(connected, disconnected int) {
	if m == nil {
		return
	}
	m.servers.With("connected").Set(float64(connected))
	m.servers.With("disconnected").Set(float64(disconnected))
}

// SurfaceGeneration records the published surface generation.
func (m *Metrics) SurfaceGeneration(generation int64) {
	if m == nil {
		return
	}
	m.surfaceGeneration.Set(float64(generation))
}

// AppFetch records one app document resolution. result is one of
// ok|error|reused; anything else is folded to "other".
func (m *Metrics) AppFetch(result string) {
	if m == nil {
		return
	}
	m.appFetch.With(appFetchResult(result)).Inc()
}

func appFetchResult(result string) string {
	switch result {
	case "ok", "error", "reused":
		return result
	default:
		return "other"
	}
}

// BridgeFrame records one bridge frame. method is one of the four bridge
// actions (anything else folds to "other"); direction is in|out.
func (m *Metrics) BridgeFrame(method, direction string) {
	if m == nil {
		return
	}
	m.bridgeFrames.With(bridgeMethod(method), bridgeDirection(direction)).Inc()
}

func bridgeMethod(method string) string {
	switch method {
	case "tools/call", "resources/read", "ui/message", "ui/open-link":
		return method
	default:
		return "other"
	}
}

func bridgeDirection(direction string) string {
	switch direction {
	case "in", "out":
		return direction
	default:
		return "other"
	}
}

// ListenReconnect counts one reconnect attempt of a listen stream (the first
// attempt is not a reconnect; a retry after a drop is).
func (m *Metrics) ListenReconnect() {
	if m == nil {
		return
	}
	m.listenReconnects.Inc()
}

// MRTRRound records the round count of one MRTR exchange (P5 executes it; the
// instrument exists here so the series is fixed from boot).
func (m *Metrics) MRTRRound(rounds int) {
	if m == nil {
		return
	}
	m.mrtrRounds.Observe(float64(rounds))
}
