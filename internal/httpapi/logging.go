// Request logging: one structured line per request with correlation IDs, and
// nothing else. Headers, query strings, and bodies are never logged; the
// logsafe handler (cmd/runtime) additionally redacts credential-shaped values.

package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type requestIDKey struct{}

// RequestIDFrom returns the request id attached by the middleware.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// requestID reads X-Request-ID or generates one. The header is sanitized to a
// bounded length so a hostile client cannot inflate every log line.
func requestID(r *http.Request) string {
	if raw := r.Header.Get("X-Request-ID"); raw != "" {
		if len(raw) > 128 {
			raw = raw[:128]
		}
		return raw
	}
	return uuid.NewString()
}

// statusRecorder preserves the optional interfaces handlers rely on (Flusher,
// ResponseController's Unwrap) while capturing the status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer (write
// deadlines for SSE).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// serveLogged wraps the mux with request logging and the drain gate. It runs
// after the mux so r.Pattern and r.PathValue are populated; drain rejections are
// logged here too, otherwise the requests an operator most needs to see would be
// invisible during a release.
func (s *Server) serveLogged(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	rid := requestID(r)
	r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, rid))
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	rec.Header().Set("X-Request-ID", rid)
	rejectedForDrain := false

	defer func() {
		loggedPath := r.Pattern
		if rejectedForDrain {
			loggedPath = "draining"
		} else if loggedPath == "" {
			loggedPath = "unmatched"
		}
		fields := []any{
			"request_id", rid,
			"method", r.Method,
			"route", loggedPath,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(started).Milliseconds(),
		}
		if rejectedForDrain {
			fields = append(fields, "draining", true)
		}
		if tenant := r.Header.Get("X-Tenant-ID"); tenant != "" {
			if _, err := uuid.Parse(tenant); err == nil {
				fields = append(fields, "tenant_id", tenant)
			}
		}
		sessionID := ""
		if strings.Contains(r.Pattern, "/v1/sessions/{id}") {
			if id, err := uuid.Parse(r.PathValue("id")); err == nil {
				sessionID = id.String()
			}
		}
		if sessionID == "" {
			// Drain rejections happen before the mux matches, so r.Pattern and
			// PathValue are empty; parse the path directly.
			sessionID = sessionIDFromPath(r.URL.Path)
		}
		if sessionID != "" {
			fields = append(fields, "session_id", sessionID)
		}
		// Probes and scrapes stay quiet when healthy; a failure is still logged.
		if probePath(r.URL.Path) && rec.status < 400 {
			return
		}
		s.cfg.Logger.Info("http request", fields...)
	}()

	if !s.drainAdmits(r) {
		rejectedForDrain = true
		s.rejectDraining(rec)
		return
	}
	s.mux.ServeHTTP(rec, r)
}

// sessionIDFromPath extracts /v1/sessions/{uuid}/... for the pre-mux drain gate.
func sessionIDFromPath(path string) string {
	const prefix = "/v1/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	id, err := uuid.Parse(rest)
	if err != nil {
		return ""
	}
	return id.String()
}

func probePath(path string) bool {
	switch path {
	case "/v1/health", "/v1/ready", "/metrics":
		return true
	}
	return false
}
