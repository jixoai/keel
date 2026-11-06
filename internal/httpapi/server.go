// Package httpapi is the runtime's HTTP surface: sessions, events (JSON + SSE),
// file upload/download, and health/metrics. It is intentionally thin — all
// durable facts live in PostgreSQL, and the only in-memory structures are the
// SSE wakeup hub and the bounded caches in the stream package.
package httpapi

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
)

// Config wires the server's dependencies.
type Config struct {
	Store    *store.Store
	Hub      *Hub
	Blobs    *blob.Reader
	Uploader *blob.Uploader
	Scheduler *stream.Scheduler
	Admitter  *stream.Admitter
	Limiter   *stream.Limiter
	Metrics   *stream.Metrics
	Logger    *slog.Logger
	CASDir    string
	// MaxBody caps JSON request bodies (uploads use MaxUpload).
	MaxBody   int64
	MaxUpload int64
}

// Server is the HTTP handler.
type Server struct {
	cfg Config
	mux *http.ServeMux
}

// New builds the server and its routes.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = &stream.Metrics{}
	}
	if cfg.MaxBody == 0 {
		cfg.MaxBody = 1 << 20
	}
	if cfg.MaxUpload == 0 {
		cfg.MaxUpload = 64 << 20
	}
	if cfg.CASDir != "" && cfg.Blobs != nil {
		cfg.Blobs.CASDir = cfg.CASDir
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

var sseExitLogs int64

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v1/health", s.health)
	s.mux.HandleFunc("GET /v1/metrics", s.metrics)
	s.mux.HandleFunc("POST /v1/tenants", s.createTenant)
	s.mux.HandleFunc("POST /v1/sessions", s.createSession)
	s.mux.HandleFunc("POST /v1/sessions/{id}/input", s.input)
	s.mux.HandleFunc("POST /v1/sessions/{id}/events", s.appendEvent)
	s.mux.HandleFunc("GET /v1/sessions/{id}/events", s.listEvents)
	s.mux.HandleFunc("GET /v1/sessions/{id}/events/stream", s.streamEvents)
	s.mux.HandleFunc("POST /v1/files", s.upload)
	s.mux.HandleFunc("GET /v1/files/{id}/meta", s.fileMeta)
	s.mux.HandleFunc("GET /v1/files/{id}", s.file)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"pid":     pid,
		"bootId":  bootID,
		"goroutine": runtime.NumGoroutine(),
		"heapMB":  float64(m.HeapAlloc) / 1e6,
		"rssMB":   float64(m.Sys) / 1e6,
	})
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	inflight, control, classes, rejected := s.cfg.Scheduler.Snapshot()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	writeJSON(w, 200, map[string]any{
		"stream":   s.cfg.Metrics.Snapshot(),
		"inflight": inflight,
		"control":  control,
		"classes": map[string]int{"small": classes[0], "medium": classes[1], "large": classes[2]},
		"rejected": rejected,
		"goroutine": runtime.NumGoroutine(),
		"heapMB":   float64(m.HeapAlloc) / 1e6,
		"sysMB":    float64(m.Sys) / 1e6,
	})
}

// tenantFrom reads the X-Tenant-ID header.
func tenantFrom(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	return id, err == nil
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	id := uuid.New()
	if raw := r.Header.Get("X-Tenant-ID"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeErr(w, 400, fmt.Errorf("bad tenant id: %w", err))
			return
		}
		id = parsed
	}
	if err := s.cfg.Store.CreateTenant(r.Context(), id, body.Name); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 201, map[string]any{"tenantId": id, "name": body.Name})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
	}
	_ = decodeBody(w, r, s.cfg.MaxBody, &body)
	id := uuid.New()
	if body.SessionID != "" {
		parsed, err := uuid.Parse(body.SessionID)
		if err != nil {
			writeErr(w, 400, fmt.Errorf("bad sessionId: %w", err))
			return
		}
		id = parsed
	}
	if err := s.cfg.Store.CreateSession(r.Context(), tenantID, id); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 201, map[string]any{"sessionId": id})
}

func (s *Server) input(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad session id: %w", err))
		return
	}
	var body struct {
		Text      string `json:"text"`
		RequestID string `json:"requestId"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeErr(w, 400, errors.New("text is required"))
		return
	}
	requestID := body.RequestID
	if requestID == "" {
		requestID = r.Header.Get("X-Request-ID")
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	sub, created, err := s.cfg.Store.Submit(r.Context(), tenantID, requestID, sessionID, uuid.New())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if created {
		seq, err := s.cfg.Store.AppendEvent(r.Context(), tenantID, sessionID, "user.message",
			map[string]any{"text": body.Text, "requestId": requestID})
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		if err := s.cfg.Store.SetSubmissionStatus(r.Context(), tenantID, requestID, "committed", seq); err != nil {
			writeErr(w, 500, err)
			return
		}
		sub.Seq = &seq
	}
	status := 200
	if created {
		status = 201
	}
	writeJSON(w, status, map[string]any{
		"submissionId": sub.ID,
		"requestId":    requestID,
		"status":       sub.Status,
		"seq":          sub.Seq,
		"created":      created,
	})
}

func (s *Server) appendEvent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad session id: %w", err))
		return
	}
	var body struct {
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if body.Kind == "" {
		writeErr(w, 400, errors.New("kind is required"))
		return
	}
	if len(body.Payload) == 0 {
		body.Payload = json.RawMessage(`{}`)
	}
	seq, err := s.cfg.Store.AppendEvent(r.Context(), tenantID, sessionID, body.Kind, body.Payload)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 201, map[string]any{"seq": seq})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad session id: %w", err))
		return
	}
	after := parseAfter("", r.URL.Query().Get("after"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.cfg.Store.LoadEvents(r.Context(), tenantID, sessionID, after, limit)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"events": events})
}

// streamEvents is the SSE endpoint. It subscribes to the hub for wakeups and
// runs a keyset query after its cursor; Last-Event-ID resumes exactly where the
// client left off.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad session id: %w", err))
		return
	}
	if !s.cfg.Admitter.Allow() {
		delay := s.cfg.Admitter.Delay() / 1000
		if delay < 1 {
			delay = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(delay, 10))
		writeErr(w, http.StatusTooManyRequests, errors.New("too many new streams"))
		return
	}
	release, ok := s.cfg.Scheduler.AcquireControl()
	if !ok {
		s.cfg.Metrics.AdmissionReject.Add(1)
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, errors.New("control stream capacity exhausted"))
		return
	}
	defer release()
	s.cfg.Metrics.StreamsActive.Add(1)
	defer s.cfg.Metrics.StreamsActive.Add(-1)
	s.cfg.Metrics.StreamsTotal.Add(1)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, errors.New("streaming unsupported"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()

	rc := http.NewResponseController(w)
	after := parseAfter(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after"))
	wake, cancel := s.cfg.Hub.Subscribe(tenantID, sessionID)
	defer cancel()

	exit := func(reason string, err error) {
		ctxErr := ""
		if r.Context().Err() != nil {
			ctxErr = r.Context().Err().Error()
		}
		if atomic.AddInt64(&sseExitLogs, 1) <= 50 {
			s.cfg.Logger.Warn("sse exit", "reason", reason, "err", err, "ctx", ctxErr, "after", after)
		}
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	// NOTIFY is the primary wakeup; the fallback is a safety net for a dropped
	// notification. Five seconds keeps 1000 sessions at 200 QPS instead of 1000.
	fallback := time.NewTimer(5 * time.Second)
	defer fallback.Stop()

	for {
		events, err := s.cfg.Store.LoadEvents(r.Context(), tenantID, sessionID, after, 200)
		if err != nil {
			if r.Context().Err() != nil {
				exit("load-client-gone", err)
				return
			}
			// A transient pool/statement timeout must not tear down the stream:
			// the client keeps its cursor and the next attempt resumes there.
			s.cfg.Logger.Warn("sse load retrying", "err", err)
			select {
			case <-r.Context().Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
			continue
		}
		for _, ev := range events {
			frame := fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, ev.Payload)
			if err := setWriteDeadline(rc, 10*time.Second); err != nil {
				exit("deadline", err)
				return
			}
			if _, err := w.Write([]byte(frame)); err != nil {
				exit("write", err)
				return
			}
			after = ev.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
			if !fallback.Stop() {
				select {
				case <-fallback.C:
				default:
				}
			}
			fallback.Reset(5 * time.Second)
		}
		select {
		case <-r.Context().Done():
			exit("done", r.Context().Err())
			return
		case <-wake:
		case <-heartbeat.C:
			if err := setWriteDeadline(rc, 10*time.Second); err != nil {
				exit("heartbeat-deadline", err)
				return
			}
			if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
				exit("heartbeat-write", err)
				return
			}
			flusher.Flush()
		case <-fallback.C:
			// Safety net: a dropped NOTIFY costs at most one fallback interval.
		}
	}
}

// setWriteDeadline bounds one SSE write. ErrNotSupported means a middleware
// wrapper hides the underlying writer; that is a deployment bug, but SSE must
// keep working, so it is tolerated here (the file path fails loudly instead).
func setWriteDeadline(rc *http.ResponseController, d time.Duration) error {
	err := rc.SetWriteDeadline(time.Now().Add(d))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// upload accepts a raw body (Content-Type = mime) or multipart/form-data with a
// single "file" part, spools it, and returns the file id/url.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	release, ok := s.cfg.Scheduler.Acquire(stream.ClassOf(r.ContentLength))
	if !ok {
		s.cfg.Metrics.AdmissionReject.Add(1)
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, errors.New("upload capacity exhausted"))
		return
	}
	defer release()

	mime := r.Header.Get("Content-Type")
	if strings.HasPrefix(mime, "multipart/form-data") {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeErr(w, 400, err)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		defer file.Close()
		if mime == "" || strings.HasPrefix(mime, "multipart/") {
			mime = header.Header.Get("Content-Type")
		}
		meta, err := s.cfg.Uploader.Put(r.Context(), tenantID, mime, file)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		s.writeFileMeta(w, meta)
		return
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	meta, err := s.cfg.Uploader.Put(r.Context(), tenantID, mime, http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.writeFileMeta(w, meta)
}

func (s *Server) writeFileMeta(w http.ResponseWriter, meta store.BlobMeta) {
	writeJSON(w, 201, map[string]any{
		"fileId":  meta.BlobID,
		"url":     "/v1/files/" + meta.BlobID.String(),
		"size":    meta.Size,
		"mime":    meta.Mime,
		"sha256":  hex.EncodeToString(meta.SHA256),
		"storage": meta.Storage,
		"chunks":  meta.Chunks,
	})
}

func (s *Server) fileMeta(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad file id: %w", err))
		return
	}
	meta, err := s.cfg.Store.BlobMetaOf(r.Context(), tenantID, id)
	if err != nil {
		status := 500
		if errors.Is(err, store.ErrNoBlob) {
			status = 404
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"fileId": meta.BlobID, "size": meta.Size, "mime": meta.Mime,
		"sha256": hex.EncodeToString(meta.SHA256), "storage": meta.Storage,
		"chunks": meta.Chunks, "complete": meta.Complete,
		"wireLength": blob.WireLen(meta.Size),
	})
}

// file streams a blob as raw bytes or base64. Raw supports standard Range;
// base64 uses the client's wire offset (multiple of 4) so a resumed stream
// continues the same base64 sequence.
func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad file id: %w", err))
		return
	}
	meta, err := s.cfg.Store.BlobMetaOf(r.Context(), tenantID, id)
	if err != nil {
		status := 500
		if errors.Is(err, store.ErrNoBlob) {
			status = 404
		}
		writeErr(w, status, err)
		return
	}
	asBase64 := r.URL.Query().Get("format") == "base64"

	if !s.cfg.Admitter.Allow() {
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, errors.New("too many new streams"))
		return
	}
	class := stream.ClassOf(meta.Size)
	if asBase64 {
		class = stream.ClassOf(blob.WireLen(meta.Size))
	}
	release, ok := s.cfg.Scheduler.Acquire(class)
	if !ok {
		s.cfg.Metrics.AdmissionReject.Add(1)
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, errors.New("stream capacity exhausted"))
		return
	}
	defer release()
	s.cfg.Metrics.StreamsActive.Add(1)
	defer s.cfg.Metrics.StreamsActive.Add(-1)
	s.cfg.Metrics.StreamsTotal.Add(1)

	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "private, max-age=3600")
	if meta.Mime != "" {
		h.Set("Content-Type", meta.Mime)
	}

	if asBase64 {
		total := blob.WireLen(meta.Size)
		off := parseAfter(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("offset"))
		if off > total || off%4 != 0 {
			writeErr(w, 416, fmt.Errorf("invalid wire offset %d", off))
			return
		}
		h.Set("Content-Length", strconv.FormatInt(total-off, 10))
		status := 200
		if off > 0 {
			status = http.StatusPartialContent
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, total-1, total))
			s.cfg.Metrics.Resumes.Add(1)
		}
		w.WriteHeader(status)
		guard := stream.NewGuard(w, stream.GuardConfig{Limiter: s.cfg.Limiter})
		n, err := s.cfg.Blobs.StreamBase64(r.Context(), tenantID, id, meta, guard, off)
		guard.Close()
		s.cfg.Metrics.BytesSent.Add(n)
		s.cfg.Metrics.ChunksFetched.Add((meta.Size + store.ChunkSize - 1) / store.ChunkSize)
		if err != nil {
			// Headers are gone; abort the connection without a second response.
			s.cfg.Logger.Error("file base64 stream failed", "file", id, "err", err)
			panic(http.ErrAbortHandler)
		}
		return
	}

	start, end := int64(0), meta.Size-1
	status := 200
	if rng := r.Header.Get("Range"); rng != "" {
		parsedStart, parsedEnd, ok := parseRange(rng, meta.Size)
		if !ok {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", meta.Size))
			writeErr(w, 416, errors.New("invalid range"))
			return
		}
		start, end = parsedStart, parsedEnd
		status = http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, meta.Size))
		s.cfg.Metrics.Resumes.Add(1)
	}
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(status)
	guard := stream.NewGuard(w, stream.GuardConfig{Limiter: s.cfg.Limiter})
	n, err := s.cfg.Blobs.StreamRaw(r.Context(), tenantID, id, meta, guard, start, end-start+1)
	guard.Close()
	s.cfg.Metrics.BytesSent.Add(n)
	if err != nil {
		s.cfg.Logger.Error("file raw stream failed", "file", id, "err", err)
		panic(http.ErrAbortHandler)
	}
}

// parseRange parses "bytes=start-end" (single range only).
func parseRange(v string, size int64) (int64, int64, bool) {
	if !strings.HasPrefix(v, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(v, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] == "" {
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, http.ErrBodyReadAfterClose) {
			return errors.New("body closed")
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

