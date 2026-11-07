// Readiness probe. Liveness (/v1/health) only proves the process is up; this
// endpoint proves PostgreSQL is reachable and the schema this binary expects is
// present, so a load balancer can remove an instance before it serves errors.

package httpapi

import (
	"context"
	"net/http"
	"time"
)

// ReadyChecker verifies downstream dependencies. store.Store implements it.
type ReadyChecker interface {
	CheckReady(ctx context.Context) error
}

// readyTimeout bounds one readiness probe; it must be shorter than a typical
// orchestrator timeout (1-3s).
const readyTimeout = 2 * time.Second

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	draining := s.cfg.Drain != nil && s.cfg.Drain.Draining()
	if draining {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ready":    false,
			"draining": true,
		})
		return
	}
	if s.cfg.Ready == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ready": true, "draining": false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if err := s.cfg.Ready.CheckReady(ctx); err != nil {
		// The detail goes to the log, not the response: it can name internal
		// hosts or tables.
		s.cfg.Logger.Warn("readiness probe failed", "err", err)
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ready":    false,
			"draining": false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true, "draining": false})
}
