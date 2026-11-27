package harnessext

// Official telemetry plugin (`keel.telemetry`, change keel-plugin-telemetry).
//
// It is the only owner of the OTLP exporter lifecycle. The plugin is bundled,
// in-process and disabled by default: enabling it is an explicit lifecycle
// approval (POST /v1/plugins/keel.telemetry/enable with its durable decision),
// and until then the configured telemetry values are inert — the base adapter
// stays Memory/Noop and no collector request is made.
//
// Start parses the tenant telemetry section, resolves secret headers, registers
// one OTLP exporter on the hub and starts its batch worker. Stop unregisters the
// exporter first (so no captured span can enqueue afterwards), then closes it
// with a bounded flush. A failed parse/init marks the plugin FAILED and leaves
// the harness untouched.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/telemetry"
)

const (
	// TelemetryPluginID is the bundled plugin id.
	TelemetryPluginID = "keel.telemetry"
	// TelemetryPluginVersion is the bundled version.
	TelemetryPluginVersion = "1.0.0"

	telemetryExporterName = "otlp-http"
	telemetryStopTimeout  = 5 * time.Second
)

// TelemetryPluginOptions wires the plugin.
type TelemetryPluginOptions struct {
	TenantID string
	Config   telemetry.Config
	// LookupSecret resolves `secret:NAME` header references. Nil fails closed
	// when the config declares headers.
	LookupSecret func(name string) (string, bool)
	Logger       *slog.Logger
	Now          func() time.Time
}

type telemetryPlugin struct {
	options TelemetryPluginOptions

	mu       sync.Mutex
	exporter *telemetry.OTLPExporter
	hub      *telemetry.Hub
	started  bool
}

// NewTelemetryPlugin builds the official telemetry plugin. The plugin has no
// model-facing tool and no hook: it owns an exporter, nothing else.
func NewTelemetryPlugin(options TelemetryPluginOptions) *Plugin {
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	handler := &telemetryPlugin{options: options}
	return &Plugin{
		ID:       TelemetryPluginID,
		Version:  TelemetryPluginVersion,
		Provides: []string{"cap:telemetry.export"},
		Start:    handler.start,
		Stop:     handler.stop,
	}
}

func (p *telemetryPlugin) start(ctx context.Context, env *PluginEnv) error {
	hub := env.Telemetry
	if hub == nil {
		return fmt.Errorf("telemetry: the host does not expose a telemetry hub")
	}
	tenant := strings.TrimSpace(p.options.TenantID)
	if hubTenant := strings.TrimSpace(hub.TenantID()); hubTenant != "" && tenant != "" && hubTenant != tenant {
		// Fail closed: a multi-tenant process must never share an exporter or an
		// endpoint between tenants. The host wires one hub per tenant; a mismatch
		// is a wiring bug and exporting would leak signals.
		reason := "telemetry hub belongs to another tenant; refusing to export"
		hub.SetPluginState(telemetry.PluginFailed, reason)
		return fmt.Errorf("telemetry: %s", reason)
	}

	summary := telemetry.SummarizeConfig(p.options.Config)
	hub.Configure(summary)
	p.mu.Lock()
	p.hub = hub
	p.mu.Unlock()
	effective, err := telemetry.ParseConfig(p.options.Config)
	if err != nil {
		hub.SetPluginState(telemetry.PluginFailed, err.Error())
		p.auditConfig(env, telemetry.PluginFailed, summary, err)
		return fmt.Errorf("telemetry config: %w", err)
	}
	hub.SetSampleRatio(effective.SampleRatio)
	hub.SetLogRatio(effective.LogRatio)

	if effective.Inactive() {
		hub.SetPluginState(telemetry.PluginInactive, "adapter "+effective.Adapter+" has no exporter")
		p.mu.Lock()
		p.started = true
		p.mu.Unlock()
		p.auditConfig(env, telemetry.PluginInactive, summary, nil)
		return nil
	}

	headers, err := effective.ResolveHeaders(p.options.LookupSecret)
	if err != nil {
		hub.SetPluginState(telemetry.PluginFailed, err.Error())
		p.auditConfig(env, telemetry.PluginFailed, summary, err)
		return fmt.Errorf("telemetry headers: %w", err)
	}
	exporter, err := telemetry.NewOTLPExporter(telemetry.OTLPExporterOptions{
		Endpoint:    effective.Endpoint,
		ServiceName: effective.ServiceName,
		TenantID:    effective.TenantID,
		Headers:     headers,
		Signals:     effective.Signals,
		LogMinLevel: effective.LogMinLevel,
		LogRatio:    floatPointer(effective.LogRatio),
		Logger:      p.options.Logger,
		Now:         p.options.Now,
	})
	if err != nil {
		hub.SetPluginState(telemetry.PluginFailed, err.Error())
		p.auditConfig(env, telemetry.PluginFailed, summary, err)
		return fmt.Errorf("telemetry exporter: %w", err)
	}
	if err := hub.RegisterExporter(exporter); err != nil {
		hub.SetPluginState(telemetry.PluginFailed, err.Error())
		p.auditConfig(env, telemetry.PluginFailed, summary, err)
		return fmt.Errorf("telemetry exporter: %w", err)
	}
	exporter.Start()
	p.mu.Lock()
	p.exporter = exporter
	p.started = true
	p.mu.Unlock()
	hub.SetPluginState(telemetry.PluginActive, "")
	p.auditConfig(env, telemetry.PluginActive, summary, nil)
	return nil
}

func (p *telemetryPlugin) stop(ctx context.Context) error {
	p.mu.Lock()
	started := p.started
	exporter := p.exporter
	hub := p.hub
	p.exporter = nil
	p.hub = nil
	p.started = false
	p.mu.Unlock()
	if !started {
		// A Start that failed before activating leaves the FAILED diagnostic in
		// place: its rollback Stop must not rewrite it as disabled.
		return nil
	}
	if hub != nil {
		if exporter != nil {
			hub.UnregisterExporter(telemetryExporterName)
		}
		hub.SetPluginState(telemetry.PluginDisabled, "")
	}
	if exporter == nil {
		return nil
	}
	// The plugin's own registration is gone before the exporter closes: an
	// in-flight span that captured the exporter may still enqueue, and the
	// exporter contract says Close tolerates that.
	stopCtx := ctx
	if stopCtx == nil {
		stopCtx = context.Background()
	}
	cancel := func() {}
	if _, hasDeadline := stopCtx.Deadline(); !hasDeadline {
		stopCtx, cancel = context.WithTimeout(context.WithoutCancel(stopCtx), telemetryStopTimeout)
	}
	defer cancel()
	if err := exporter.Close(stopCtx); err != nil && p.options.Logger != nil {
		p.options.Logger.Warn("telemetry exporter shutdown flush incomplete", "err", err)
	}
	return nil
}

func (p *telemetryPlugin) auditConfig(env *PluginEnv, state telemetry.PluginState, summary telemetry.ConfigSummary, configErr error) {
	if env == nil || env.Audit == nil {
		return
	}
	detail, changed := telemetryConfigAuditDetail(p.options.TenantID, state, summary, configErr)
	if !changed {
		return
	}
	result := store.PluginResultOK
	if state == telemetry.PluginFailed {
		result = store.PluginResultFailed
	}
	env.Audit(context.Background(), store.PluginActionConfigure, result, detail)
}

// telemetryConfigAudit memoizes the last applied config so a configure audit row
// is written only when the effective configuration changed (or on first apply),
// never once per assembly.
var telemetryConfigAudit = struct {
	sync.Mutex
	last map[string]auditConfigSnapshot
}{last: map[string]auditConfigSnapshot{}}

type auditConfigSnapshot struct {
	state    telemetry.PluginState
	adapter  string
	signals  string
	endpoint string
	sample   float64
	logRatio float64
}

func newAuditSnapshot(state telemetry.PluginState, summary telemetry.ConfigSummary) auditConfigSnapshot {
	return auditConfigSnapshot{
		state: state, adapter: summary.Adapter, signals: signalStrings(summary.Signals),
		endpoint: summary.EndpointHost, sample: summary.SampleRatio, logRatio: summary.LogRatio,
	}
}

func (s auditConfigSnapshot) fingerprint() string {
	return fmt.Sprintf("%s|%s|%s|%s|%g|%g", s.state, s.adapter, s.signals, s.endpoint, s.sample, s.logRatio)
}

func telemetryConfigAuditDetail(tenant string, state telemetry.PluginState, summary telemetry.ConfigSummary, configErr error) (map[string]any, bool) {
	current := newAuditSnapshot(state, summary)
	key := strings.TrimSpace(tenant)
	telemetryConfigAudit.Lock()
	previous, existed := telemetryConfigAudit.last[key]
	changed := !existed || previous.fingerprint() != current.fingerprint()
	if changed {
		telemetryConfigAudit.last[key] = current
	}
	telemetryConfigAudit.Unlock()
	if !changed {
		return nil, false
	}
	detail := map[string]any{
		"state": string(state), "adapter": summary.Adapter, "signals": signalStrings(summary.Signals),
		"sampleRatio": summary.SampleRatio, "logRatio": summary.LogRatio,
		"headerNames": append([]string(nil), summary.HeaderNames...), "headerCount": len(summary.HeaderNames),
		"endpointHost": summary.EndpointHost, "valid": summary.Valid,
	}
	if summary.Reason != "" {
		detail["reason"] = summary.Reason
	}
	if configErr != nil {
		detail["error"] = configErr.Error()
	}
	if existed {
		detail["previous"] = map[string]any{
			"state": string(previous.state), "adapter": previous.adapter, "signals": previous.signals,
			"sampleRatio": previous.sample, "endpointHost": previous.endpoint,
		}
	}
	return detail, true
}

func signalStrings(signals []telemetry.Signal) string {
	parts := make([]string, 0, len(signals))
	for _, signal := range signals {
		parts = append(parts, string(signal))
	}
	return strings.Join(parts, ",")
}

func floatPointer(value float64) *float64 { return &value }
