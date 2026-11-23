package harnessapi

// Session query (change keel-harness-v2, W12 `keel.session-query`): a bounded
// read over the transcript and a bounded export of one session.
//
// What these routes are:
//
//   GET /v1/sessions/search?q=&scope=sessions|messages&sessionId=&kind=
//                          &limit=&after=
//       a page of session or message hits. The page is keyset-ordered and
//       capped; the handler filters every candidate through the same
//       access.Visible relation the session list uses, so a tenant-wide search
//       can never return a session the caller cannot read.
//
//   GET /v1/sessions/{id}/export?afterSeq=&limit=&format=json|ndjson
//       one bounded page of the transcript's text messages. Tool payloads,
//       selection/attachments and the rest of the ledger are not exported; a
//       caller that wants the whole session must page. There is no "dump the
//       session" form.
//
// Neither route invents state: search reads events, export reads events, and
// the title on a hit comes from internal/sessiontitle. The JSON page is the
// typed API; NDJSON is the same page in the export-friendly encoding.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/fileplugin"
)

const (
	defaultSearchLimit = 20
	maxSearchLimit     = 100
	// searchTextLimit bounds the query string, in runes. It is a guard against
	// an unbounded scan, not a product limit.
	searchTextLimit = 200
	// searchTextPreview bounds the message text a search reads per row. The
	// predicate still sees the whole text; only the snippet source is capped.
	searchTextPreview = 4096

	defaultExportLimit = 200
	maxExportLimit     = 500
	// exportMessageBytes bounds one exported message; a longer text is cut and
	// flagged, never silently shortened.
	exportMessageBytes = 32 * 1024
	// exportPageBytes bounds one exported page's text. The page stops at the
	// last message that fits, so an export is always a bounded read.
	exportPageBytes = 512 * 1024

	defaultDeliverablesLimit = 100
	maxDeliverablesLimit     = 500
)

// ErrBadQueryCursor is a malformed search cursor (400).
var ErrBadQueryCursor = errors.New("harnessapi: malformed query cursor")

// --- search ---------------------------------------------------------------

// MessageSearchFilter is one message-search page request. Text is required;
// SessionID narrows to one session; Kind is "user", "assistant" or "".
type MessageSearchFilter struct {
	Text       string
	SessionID  uuid.UUID
	HasSession bool
	Kind       string
	Limit      int
	After      string
}

// MessageHit is one message match on the wire. SessionTitle is derived from the
// session's first user message (internal/sessiontitle), so a hit and its list
// row never disagree.
type MessageHit struct {
	SessionID    string    `json:"sessionId"`
	SessionTitle string    `json:"sessionTitle"`
	Seq          int64     `json:"seq"`
	Kind         string    `json:"kind"`
	Role         string    `json:"role"`
	At           time.Time `json:"at"`
	Snippet      string    `json:"snippet"`
}

// MessageHitPage is one keyset page of message hits.
type MessageHitPage struct {
	Hits       []MessageHit
	NextCursor string
	HasMore    bool
}

// SessionSearchFilter is one session-search page request.
type SessionSearchFilter struct {
	Text  string
	Limit int
	After string
}

// SessionHit is one session match on the wire: the derived title plus the text
// that matched. Owner/visibility facts stay in the list route; a hit names only
// what it matched.
type SessionHit struct {
	SessionID     string     `json:"sessionId"`
	Title         string     `json:"title"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	LastMessageAt *time.Time `json:"lastMessageAt"`
	Snippet       string     `json:"snippet"`
}

// SessionSearchPage is one keyset page of session hits.
type SessionSearchPage struct {
	Hits       []SessionHit
	NextCursor string
	HasMore    bool
}

// SessionSearcher is the transcript/session search seam. Nil answers 501 on
// GET /v1/sessions/search and reports the capability false (a missing access
// surface is the separate 503 path).
type SessionSearcher interface {
	SearchMessages(ctx context.Context, tenantID uuid.UUID, filter MessageSearchFilter) (MessageHitPage, error)
	SearchSessions(ctx context.Context, tenantID uuid.UUID, filter SessionSearchFilter) (SessionSearchPage, error)
}

// --- export ---------------------------------------------------------------

// ExportFilter is one export page request.
type ExportFilter struct {
	AfterSeq int64
	Limit    int
}

// ExportMessage is one transcript message on the wire. Only the payload's text
// is exported; everything else stays in the ledger.
type ExportMessage struct {
	Seq           int64     `json:"seq"`
	Kind          string    `json:"kind"`
	Role          string    `json:"role"`
	At            time.Time `json:"at"`
	Text          string    `json:"text"`
	TextTruncated bool      `json:"textTruncated,omitempty"`
}

// ExportPage is one bounded page of a session's transcript.
type ExportPage struct {
	SessionID string
	Title     string
	Messages  []ExportMessage
	// NextAfterSeq is the seq of the last exported message; the next page starts
	// after it. It is 0 when the page is empty.
	NextAfterSeq int64
	HasMore      bool
}

// SessionExporter is the export seam. Nil answers 501 on
// GET /v1/sessions/{id}/export and reports the capability false (a missing
// access surface is the separate 503 path).
type SessionExporter interface {
	ExportMessages(ctx context.Context, tenantID, sessionID uuid.UUID, filter ExportFilter) (ExportPage, error)
}

// --- routes ---------------------------------------------------------------

func (s *Server) sessionQueryRoutes() {
	s.mux.HandleFunc("GET /v1/sessions/search", s.searchSessions)
	s.mux.HandleFunc("GET /v1/sessions/{id}/export", s.exportSession)
}

func (s *Server) searchSessions(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.SessionSearch == nil {
		notWired(w, "session search is not wired in this deployment")
		return
	}
	// Visibility is a relation, not a tenant scope: a tenant-wide search is
	// filtered through the same seam the list uses. A deployment without it
	// fails closed instead of returning every session in the tenant.
	if s.cfg.Access == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("session search requires the identity access surface"))
		return
	}
	principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, errors.New("authentication required"))
		return
	}
	text := strings.TrimSpace(r.URL.Query().Get("q"))
	if text == "" {
		writeErr(w, http.StatusBadRequest, errors.New("q is required"))
		return
	}
	if utf8.RuneCountInString(text) > searchTextLimit {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("q must be at most %d characters", searchTextLimit))
		return
	}
	limit, err := queryInt(r, "limit", defaultSearchLimit, 1, maxSearchLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		scope = "messages"
	}
	switch scope {
	case "messages":
		s.searchMessages(w, r, tenantID, principal, text, limit)
	case "sessions":
		s.searchSessionRows(w, r, tenantID, principal, text, limit)
	default:
		writeErr(w, http.StatusBadRequest, errors.New("scope must be sessions or messages"))
	}
}

func (s *Server) searchMessages(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, principal access.Principal, text string, limit int) {
	filter := MessageSearchFilter{Text: text, Limit: limit, After: r.URL.Query().Get("after")}
	if raw := strings.TrimSpace(r.URL.Query().Get("sessionId")); raw != "" {
		sessionID, err := uuid.Parse(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("sessionId is not a UUID"))
			return
		}
		filter.SessionID = sessionID
		filter.HasSession = true
	}
	switch kind := strings.TrimSpace(r.URL.Query().Get("kind")); kind {
	case "":
	case "user":
		filter.Kind = "user.message"
	case "assistant":
		filter.Kind = "assistant.message"
	default:
		writeErr(w, http.StatusBadRequest, errors.New("kind must be user or assistant"))
		return
	}
	page, err := s.cfg.SessionSearch.SearchMessages(r.Context(), tenantID, filter)
	if err != nil {
		if errors.Is(err, ErrBadQueryCursor) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(page.Hits) > 0 {
		allowed, ok := s.allowedSessions(w, r, principal, page.Hits)
		if !ok {
			return
		}
		filtered := page.Hits[:0]
		for _, hit := range page.Hits {
			if allowed[hit.SessionID] {
				filtered = append(filtered, hit)
			}
		}
		page.Hits = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":      "messages",
		"query":      text,
		"messages":   page.Hits,
		"count":      len(page.Hits),
		"limit":      limit,
		"nextCursor": page.NextCursor,
		"hasMore":    page.HasMore,
		"limits":     map[string]int{"maxLimit": maxSearchLimit, "textLimit": searchTextLimit, "snippetSource": searchTextPreview},
	})
}

func (s *Server) searchSessionRows(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, principal access.Principal, text string, limit int) {
	page, err := s.cfg.SessionSearch.SearchSessions(r.Context(), tenantID, SessionSearchFilter{
		Text:  text,
		Limit: limit,
		After: r.URL.Query().Get("after"),
	})
	if err != nil {
		if errors.Is(err, ErrBadQueryCursor) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(page.Hits) > 0 {
		allowed, ok := s.allowedSessionHits(w, r, principal, page.Hits)
		if !ok {
			return
		}
		filtered := page.Hits[:0]
		for _, hit := range page.Hits {
			if allowed[hit.SessionID] {
				filtered = append(filtered, hit)
			}
		}
		page.Hits = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":      "sessions",
		"query":      text,
		"sessions":   page.Hits,
		"count":      len(page.Hits),
		"limit":      limit,
		"nextCursor": page.NextCursor,
		"hasMore":    page.HasMore,
		"limits":     map[string]int{"maxLimit": maxSearchLimit, "textLimit": searchTextLimit},
	})
}

// allowedSessions applies the session-read visibility relation to the page's
// candidate ids. A denied decision is a 403; a partial answer filters the page
// (the page's own cursor stays valid, so the client can continue).
func (s *Server) allowedSessions(w http.ResponseWriter, r *http.Request, principal access.Principal, hits []MessageHit) (map[string]bool, bool) {
	ids := make([]string, 0, len(hits))
	seen := make(map[string]bool, len(hits))
	for _, hit := range hits {
		if !seen[hit.SessionID] {
			seen[hit.SessionID] = true
			ids = append(ids, hit.SessionID)
		}
	}
	visible, err := s.cfg.Access.Visible(r.Context(), principal, access.KeySessionRead, "session", ids)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, false
	}
	if !visible.Allowed() {
		writeDenied(w, visible.Decision)
		return nil, false
	}
	allowed := make(map[string]bool, len(visible.IDs))
	for _, id := range visible.IDs {
		allowed[id] = true
	}
	return allowed, true
}

func (s *Server) allowedSessionHits(w http.ResponseWriter, r *http.Request, principal access.Principal, hits []SessionHit) (map[string]bool, bool) {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.SessionID)
	}
	visible, err := s.cfg.Access.Visible(r.Context(), principal, access.KeySessionRead, "session", ids)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, false
	}
	if !visible.Allowed() {
		writeDenied(w, visible.Decision)
		return nil, false
	}
	allowed := make(map[string]bool, len(visible.IDs))
	for _, id := range visible.IDs {
		allowed[id] = true
	}
	return allowed, true
}

// --- export ---------------------------------------------------------------

// SessionExportView is the typed JSON page
// (GET /v1/sessions/{id}/export).
type SessionExportView struct {
	SessionID    string          `json:"sessionId"`
	Title        string          `json:"title"`
	Format       string          `json:"format"`
	Messages     []ExportMessage `json:"messages"`
	Count        int             `json:"count"`
	Limit        int             `json:"limit"`
	AfterSeq     int64           `json:"afterSeq"`
	NextAfterSeq int64           `json:"nextAfterSeq"`
	HasMore      bool            `json:"hasMore"`
	Truncated    bool            `json:"truncated"`
	Limits       map[string]int  `json:"limits"`
}

func (s *Server) exportSession(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.SessionExport == nil {
		notWired(w, "session export is not wired in this deployment")
		return
	}
	if !s.requireSession(w, r, tenantID, sessionID) {
		return
	}
	afterSeq, err := queryInt64(r, "afterSeq", 0, 0, 1<<52)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	limit, err := queryInt(r, "limit", defaultExportLimit, 1, maxExportLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	format := strings.TrimSpace(r.URL.Query().Get("format"))
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "ndjson" {
		writeErr(w, http.StatusBadRequest, errors.New("format must be json or ndjson"))
		return
	}
	page, err := s.cfg.SessionExport.ExportMessages(r.Context(), tenantID, sessionID, ExportFilter{
		AfterSeq: afterSeq,
		Limit:    limit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	view := boundedExportPage(sessionID, page, limit, afterSeq)
	if format == "ndjson" {
		writeExportNDJSON(w, view)
		return
	}
	view.Format = "json"
	writeJSON(w, http.StatusOK, view)
}

// boundedExportPage enforces the page's byte bound on top of the store's row
// bound: whole messages are kept while they fit, and a message longer than
// exportMessageBytes is cut with TextTruncated. The page reports hasMore when
// either the store or the byte bound stopped it.
func boundedExportPage(sessionID uuid.UUID, page ExportPage, limit int, afterSeq int64) SessionExportView {
	view := SessionExportView{
		SessionID: sessionID.String(),
		Title:     page.Title,
		Messages:  make([]ExportMessage, 0, len(page.Messages)),
		Limit:     limit,
		AfterSeq:  afterSeq,
		HasMore:   page.HasMore,
		Limits:    map[string]int{"maxMessages": maxExportLimit, "maxBytes": exportPageBytes, "maxMessageBytes": exportMessageBytes},
	}
	bytes := 0
	for _, message := range page.Messages {
		text := message.Text
		truncated := message.TextTruncated
		if len(text) > exportMessageBytes {
			text = truncateUTF8(text, exportMessageBytes)
			truncated = true
		}
		if bytes+len(text) > exportPageBytes && len(view.Messages) > 0 {
			view.HasMore = true
			view.Truncated = true
			break
		}
		bytes += len(text)
		if truncated {
			message.TextTruncated = true
		}
		message.Text = text
		view.Messages = append(view.Messages, message)
		view.NextAfterSeq = message.Seq
	}
	view.Count = len(view.Messages)
	return view
}

func writeExportNDJSON(w http.ResponseWriter, view SessionExportView) {
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="session-%s.jsonl"`, view.SessionID))
	w.Header().Set("X-Keel-Next-After-Seq", strconv.FormatInt(view.NextAfterSeq, 10))
	w.Header().Set("X-Keel-Has-More", strconv.FormatBool(view.HasMore))
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	for _, message := range view.Messages {
		line := map[string]any{
			"seq":  message.Seq,
			"kind": message.Kind,
			"role": message.Role,
			"at":   message.At.UTC(),
			"text": message.Text,
		}
		if message.TextTruncated {
			line["textTruncated"] = true
		}
		if err := encoder.Encode(line); err != nil {
			return
		}
	}
}

func truncateUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	cut := text[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// queryInt64 is queryInt for a non-negative 64-bit value.
func queryInt64(r *http.Request, name string, def, min, max int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if value < min || value > max {
		return 0, fmt.Errorf("%s must be between %d and %d", name, min, max)
	}
	return value, nil
}

// --- deliverables ---------------------------------------------------------

// DeliverableView is one manifest row on the wire: the path, the content hash
// and size the manifest records, and the link the client previews it through.
// It is a projection of file_manifest, never a second file truth.
type DeliverableView struct {
	Path       string `json:"path"`
	Hash       string `json:"hash"`
	Size       int64  `json:"size"`
	Mode       uint32 `json:"mode"`
	UpdatedSeq int64  `json:"updatedSeq"`
	LastTaskID string `json:"lastTaskId,omitempty"`
	PreviewURL string `json:"previewUrl"`
}

func (s *Server) deliverableRoutes() {
	s.mux.HandleFunc("GET /v1/sessions/{id}/deliverables", s.listSessionDeliverables)
}

func (s *Server) listSessionDeliverables(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.SessionFiles == nil {
		notWired(w, "the session file manifest is not wired in this deployment")
		return
	}
	if !s.requireSession(w, r, tenantID, sessionID) {
		return
	}
	limit, err := queryInt(r, "limit", defaultDeliverablesLimit, 1, maxDeliverablesLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	after, err := decodeDeliverableCursor(r.URL.Query().Get("after"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	entries, err := s.cfg.SessionFiles.List(r.Context(), fileplugin.Scope{TenantID: tenantID, SessionID: sessionID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	start := 0
	if after != "" {
		start = sort.Search(len(entries), func(i int) bool { return entries[i].Path > after })
	}
	end := start + limit
	hasMore := end < len(entries)
	if end > len(entries) {
		end = len(entries)
	}
	page := entries[start:end]
	rows := make([]DeliverableView, 0, len(page))
	for _, entry := range page {
		rows = append(rows, DeliverableView{
			Path:       entry.Path,
			Hash:       string(entry.Hash),
			Size:       entry.Size,
			Mode:       entry.Mode,
			UpdatedSeq: entry.UpdatedSeq,
			LastTaskID: entry.LastTaskID,
			PreviewURL: deliverablePreviewURL(sessionID, entry.Path, string(entry.Hash)),
		})
	}
	next := ""
	if hasMore && len(page) > 0 {
		next = encodeDeliverableCursor(page[len(page)-1].Path)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId":    sessionID.String(),
		"deliverables": rows,
		"count":        len(rows),
		"limit":        limit,
		"nextCursor":   next,
		"hasMore":      hasMore,
		"limits":       map[string]int{"maxLimit": maxDeliverablesLimit},
	})
}

func deliverablePreviewURL(sessionID uuid.UUID, path, hash string) string {
	query := url.Values{}
	query.Set("path", path)
	if hash != "" {
		query.Set("hash", hash)
	}
	return "/v1/sessions/" + sessionID.String() + "/file-preview?" + query.Encode()
}

func encodeDeliverableCursor(path string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(path))
}

func decodeDeliverableCursor(cursor string) (string, error) {
	if strings.TrimSpace(cursor) == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil || string(raw) == "" {
		return "", fmt.Errorf("%w: not a path cursor", ErrBadCursor)
	}
	return string(raw), nil
}
