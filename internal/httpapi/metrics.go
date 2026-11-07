// Prometheus text metrics. The JSON /v1/metrics endpoint stays for humans and
// existing clients; /metrics is the fixed-cardinality scrape surface.

package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
)

// MetricsExporter writes Prometheus text exposition (version 0.0.4).
// metrics.Registry implements it; the interface keeps httpapi independent of
// the collector implementation.
type MetricsExporter interface {
	WriteText(ctx context.Context, w io.Writer) error
}

func (s *Server) prometheusMetrics(w http.ResponseWriter, r *http.Request) {
	if s.cfg.MetricsExporter == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("metrics exporter not configured"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	if err := s.cfg.MetricsExporter.WriteText(r.Context(), w); err != nil {
		s.cfg.Logger.Warn("metrics export failed", "err", err)
	}
}
