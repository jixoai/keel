package harnessapi

// GET /v1/telemetry/status (change keel-plugin-telemetry §5.3).
//
// The read is tenant-scoped: the hub owns exactly one tenant, and a caller
// naming another tenant gets the same 404 as an unknown deployment. The body
// carries only bounded facts: no header value, no endpoint query/userinfo, no
// record payload and no other tenant's data.

import (
	"errors"
	"net/http"

	"github.com/jixoai/keel/internal/telemetry"
)

var (
	errTelemetryNotWired      = errors.New("telemetry status is not wired in this deployment")
	errTelemetryNotFound      = errors.New("telemetry is not available for this tenant")
	errTelemetryTenantUnbound = errors.New("the telemetry hub is not bound to a tenant")
)

type telemetryStatusResponse struct {
	PluginID            string                     `json:"pluginId"`
	State               telemetry.PluginState      `json:"state"`
	Enabled             bool                       `json:"enabled"`
	Reason              string                     `json:"reason,omitempty"`
	Signals             []telemetry.Signal         `json:"signals"`
	EndpointHost        string                     `json:"endpointHost,omitempty"`
	HeaderNames         []string                   `json:"headerNames,omitempty"`
	HeaderCount         int                        `json:"headerCount"`
	SampleRatio         float64                    `json:"sampleRatio"`
	LogRatio            float64                    `json:"logRatio"`
	QueueDepth          int                        `json:"queueDepth"`
	Dropped             uint64                     `json:"dropped"`
	Exported            uint64                     `json:"exported"`
	MetricSeriesDropped uint64                     `json:"metricSeriesDropped"`
	AttributesDropped   uint64                     `json:"attributesDropped"`
	EventsDropped       uint64                     `json:"eventsDropped"`
	LinksDropped        uint64                     `json:"linksDropped"`
	LastErrorClass      string                     `json:"lastErrorClass,omitempty"`
	Configured          bool                       `json:"configured"`
	Adapter             string                     `json:"adapter,omitempty"`
	ServiceName         string                     `json:"serviceName,omitempty"`
	Exporters           []telemetry.ExporterStatus `json:"exporters,omitempty"`
}

func (s *Server) telemetryStatus(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Telemetry == nil {
		writeErr(w, http.StatusServiceUnavailable, errTelemetryNotWired)
		return
	}
	hubTenant := s.cfg.Telemetry.TenantID()
	if hubTenant == "" {
		// An unbound hub cannot prove isolation: answer unavailable instead of
		// rendering a status another tenant could have written.
		writeErr(w, http.StatusServiceUnavailable, errTelemetryTenantUnbound)
		return
	}
	if hubTenant != tenantID.String() {
		// Cross-tenant read: never confirm that another tenant configured
		// telemetry, and never render its endpoint or queue.
		writeErr(w, http.StatusNotFound, errTelemetryNotFound)
		return
	}
	status := s.cfg.Telemetry.Status()
	body := telemetryStatusResponse{
		PluginID:            status.PluginID,
		State:               status.State,
		Enabled:             status.Enabled,
		Reason:              status.Reason,
		Signals:             status.Signals,
		EndpointHost:        status.EndpointHost,
		HeaderNames:         status.HeaderNames,
		HeaderCount:         status.HeaderCount,
		SampleRatio:         status.SampleRatio,
		LogRatio:            status.LogRatio,
		QueueDepth:          status.QueueDepth,
		Dropped:             status.Dropped,
		Exported:            status.Exported,
		MetricSeriesDropped: status.MetricSeriesDropped,
		AttributesDropped:   status.AttributesDropped,
		EventsDropped:       status.EventsDropped,
		LinksDropped:        status.LinksDropped,
		LastErrorClass:      status.LastErrorClass,
		Configured:          status.Config.Configured,
		Adapter:             status.Config.Adapter,
		ServiceName:         status.Config.ServiceName,
		Exporters:           status.Exporters,
	}
	if body.Signals == nil {
		body.Signals = []telemetry.Signal{}
	}
	if body.HeaderNames == nil {
		body.HeaderNames = []string{}
	}
	writeJSON(w, http.StatusOK, body)
}
