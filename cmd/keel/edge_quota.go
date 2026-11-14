// Per-tenant edge quota middleware for `keel serve`.
//
// The quota dimensions that bound network and disk resources are enforced here,
// at the product edge, rather than inside internal/httpapi: the httpapi package
// predates tenancy and its stream/upload handlers are also used by cmd/runtime
// (a single-tenant process with no tenant rows). Keeping the tenant check on the
// product edge means one deployment can run in both modes without the runtime
// learning about tenants.
//
// What is enforced, and where:
//
//	streams     concurrent stream-shaped requests per tenant (SSE, file reads,
//	            uploads). Exceeding it is a 429 + Retry-After.
//	bandwidth   outbound shaping is applied by the stream guard through a
//	            per-tenant limiter; the middleware only reserves the slot.
//	storage     an upload is refused (413) when its declared size would push the
//	            tenant past its ceiling. The size is the request's own
//	            Content-Length, so the refusal happens before a byte is read.
//
// Every dimension is admitted (no-op) when the tenant's quota row says zero, and
// when no quota enforcer is wired at all.
package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/quota"
)

// edgeQuotaMiddleware enforces the per-tenant stream and storage quotas on the
// requests that consume those resources. It is a no-op for everything else, so
// the cost on a plain JSON call is one path prefix comparison.
type edgeQuotaMiddleware struct {
	Edge *quota.Edge
	Next http.Handler
}

// ServeHTTP implements http.Handler.
func (m *edgeQuotaMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	edge := m.Edge
	if edge == nil || !edgeQuotaPath(r) {
		m.next().ServeHTTP(w, r)
		return
	}
	tenantID, ok := edgeTenant(r)
	if !ok {
		// No resolved tenant: the credential path will answer 401 or the tenant
		// header path will answer its own error. Quota is not the problem here.
		m.next().ServeHTTP(w, r)
		return
	}
	if _, err := edge.AdmitStorage(r.Context(), tenantID, declaredUploadSize(r)); err != nil {
		if quota.WriteError(w, err) {
			return
		}
		m.next().ServeHTTP(w, r)
		return
	}
	release, retryAfter, err := edge.AcquireStream(r.Context(), tenantID)
	if err != nil {
		if retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
		}
		if quota.WriteError(w, err) {
			return
		}
		m.next().ServeHTTP(w, r)
		return
	}
	defer release()
	// Outbound bandwidth is shaped on the responses that carry bytes (a file
	// read, an SSE stream). A request that writes nothing pays a no-op.
	if r.Method == http.MethodGet {
		w = &shapingWriter{ResponseWriter: w, edge: edge, tenantID: tenantID, ctx: r.Context()}
	}
	m.next().ServeHTTP(w, r)
}

func (m *edgeQuotaMiddleware) next() http.Handler {
	if m.Next == nil {
		return http.NotFoundHandler()
	}
	return m.Next
}

// edgeQuotaPath reports whether a request consumes a stream or storage slot.
// SSE and file transfer are the stream-shaped paths; an upload is both.
func edgeQuotaPath(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet:
		path := r.URL.Path
		return strings.HasSuffix(path, "/events/stream") ||
			strings.HasPrefix(path, "/v1/files/") ||
			path == "/v1/realtime"
	case http.MethodPost:
		return r.URL.Path == "/v1/files"
	}
	return false
}

// edgeTenant reads the resolved tenant. The API-key middleware (or the cookie
// middleware) has already set X-Tenant-ID by the time this runs.
func edgeTenant(r *http.Request) (uuid.UUID, bool) {
	raw := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	if raw == "" {
		return uuid.Nil, false
	}
	tenantID, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false
	}
	return tenantID, true
}

// declaredUploadSize is the size a request claims it will store. Only an upload
// declares one; a stream shares a blob that is already stored and charged.
func declaredUploadSize(r *http.Request) int64 {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/files" {
		return 0
	}
	if r.ContentLength > 0 {
		return r.ContentLength
	}
	return 0
}

// shapingWriter applies the tenant's outbound bandwidth to every write of a
// response body.
//
// It is a ResponseWriter wrapper rather than a change inside internal/httpapi
// for the same reason the stream quota is a middleware: the runtime package is
// single-tenant and predates tenant rows. The wrapper preserves the optional
// interfaces the SSE and file handlers rely on (Flusher, io.ReaderFrom and the
// Unwrap that http.NewResponseController needs for write deadlines) — losing
// any of them would turn bandwidth shaping into a broken stream.
type shapingWriter struct {
	http.ResponseWriter
	edge     *quota.Edge
	tenantID uuid.UUID
	ctx      context.Context
}

func (w *shapingWriter) Write(p []byte) (int, error) {
	// A shaped write is delayed, not refused: WaitBandwidth's ErrBandwidthQuota
	// only reports that the write waited. A real error (canceled context) is
	// reported so the caller aborts a stream whose client vanished.
	if err := w.edge.WaitBandwidth(w.ctx, w.tenantID, len(p)); err != nil &&
		!errors.Is(err, quota.ErrBandwidthQuota) {
		return 0, err
	}
	return w.ResponseWriter.Write(p)
}

// Flush keeps SSE working through the wrapper.
func (w *shapingWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap lets http.NewResponseController reach SetWriteDeadline.
func (w *shapingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ReadFrom keeps io.Copy (the file stream path) on a fast path while still
// shaping: the copy is routed through Write, which applies the tenant's rate.
// Without this method, io.Copy would see the underlying writer's ReadFrom and
// bypass the shaping entirely.
func (w *shapingWriter) ReadFrom(r io.Reader) (int64, error) {
	return copyThroughWriter(w, r)
}

// copyThroughWriter copies r into w one chunk at a time (shaped).
func copyThroughWriter(w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			written, writeErr := w.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}
