package harnessapi

// Composer completion sources (change keel-harness-v2, W7 skills): the `$`
// wake-up opens one grouped list (skills / files / tasks). The skill group
// reads GET /v1/settings/skills; these two routes are the session-local data
// the other groups need, so the shell never invents a path or a task title.
//
// Both reads are projections, not new truth: the file list is the session's
// file_manifest (the same rows the read/write tools resolve) and the task list
// is the task-card projection (the ledger remains the source). Neither route
// writes, neither reaches the database directly — the hosting process injects
// the same services the tools use.

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/tasklog"
)

// SessionFileLister is the file plugin's manifest read. Nil omits the file
// group from the capability answer.
type SessionFileLister interface {
	List(ctx context.Context, scope fileplugin.Scope) ([]fileplugin.Entry, error)
}

// SessionTaskLister is the task-log projection's card read. Nil omits the task
// group from the capability answer.
type SessionTaskLister interface {
	List(ctx context.Context, scope kernel.Scope, filter tasklog.ListFilter) ([]tasklog.Card, error)
}

// SessionFileView is one file row on the wire. Hash is the manifest's own
// content hash (prefixed, "sha256:<hex>") — the same value a read/preview
// compares a reference against — omitted only when the row carries none.
type SessionFileView struct {
	Path       string `json:"path"`
	Hash       string `json:"hash,omitempty"`
	Size       int64  `json:"size"`
	UpdatedSeq int64  `json:"updatedSeq"`
	LastTaskID string `json:"lastTaskId,omitempty"`
}

// MaxSessionFileCursorLen bounds the cursor a caller may send. The cursor is the
// last path of the previous page, so a pathological length is a client error,
// never a read of an unbounded key.
const MaxSessionFileCursorLen = 1024

// SessionTaskView is one task-card row on the wire.
type SessionTaskView struct {
	TaskID       string    `json:"taskId"`
	ParentTaskID string    `json:"parentTaskId,omitempty"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func (s *Server) completionRoutes() {
	s.mux.HandleFunc("GET /v1/sessions/{id}/files", s.listSessionFiles)
	s.mux.HandleFunc("GET /v1/sessions/{id}/tasks", s.listSessionTasks)
}

func (s *Server) listSessionFiles(w http.ResponseWriter, r *http.Request) {
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
	limit, err := queryInt(r, "limit", 200, 1, 1000)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if len(cursor) > MaxSessionFileCursorLen {
		writeErr(w, http.StatusBadRequest, errors.New("cursor is longer than the route allows"))
		return
	}
	entries, err := s.cfg.SessionFiles.List(r.Context(), fileplugin.Scope{TenantID: tenantID, SessionID: sessionID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// The manifest is a path-sorted list, so the page is an exclusive path
	// cursor: the next call starts at the first path greater than `cursor`. A
	// cursor the manifest no longer holds is still a valid boundary (a row was
	// deleted between pages), not an error.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	start := sort.Search(len(entries), func(index int) bool { return entries[index].Path > cursor })
	entries = entries[start:]
	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}
	files := make([]SessionFileView, 0, len(entries))
	for _, entry := range entries {
		view := SessionFileView{
			Path:       entry.Path,
			Size:       entry.Size,
			UpdatedSeq: entry.UpdatedSeq,
			LastTaskID: entry.LastTaskID,
		}
		if entry.Hash.Valid() {
			view.Hash = entry.Hash.Prefixed()
		}
		files = append(files, view)
	}
	nextCursor := ""
	if hasMore && len(files) > 0 {
		nextCursor = files[len(files)-1].Path
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files": files,
		"count": len(files),
		"limit": limit,
		// truncated keeps its original meaning (a later page exists); the cursor
		// fields are what a client pages with.
		"truncated":  hasMore,
		"hasMore":    hasMore,
		"nextCursor": nextCursor,
	})
}

func (s *Server) listSessionTasks(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.SessionTasks == nil {
		notWired(w, "the task-card projection is not wired in this deployment")
		return
	}
	if !s.requireSession(w, r, tenantID, sessionID) {
		return
	}
	limit, err := queryInt(r, "limit", 100, 1, 500)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	status := r.URL.Query().Get("status")
	cards, err := s.cfg.SessionTasks.List(r.Context(),
		kernel.Scope{TenantID: tenantID, SessionID: sessionID},
		tasklog.ListFilter{Status: status})
	if err != nil {
		if errors.Is(err, tasklog.ErrInvalidStatus) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].UpdatedAt.After(cards[j].UpdatedAt) })
	truncated := len(cards) > limit
	if truncated {
		cards = cards[:limit]
	}
	tasks := make([]SessionTaskView, 0, len(cards))
	for _, card := range cards {
		view := SessionTaskView{
			TaskID:    card.TaskID.String(),
			Title:     card.Title,
			Status:    card.Status,
			UpdatedAt: card.UpdatedAt.UTC(),
		}
		if card.ParentTaskID != nil {
			view.ParentTaskID = card.ParentTaskID.String()
		}
		tasks = append(tasks, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":     tasks,
		"count":     len(tasks),
		"limit":     limit,
		"truncated": truncated,
	})
}

// requireSession applies the same tenant boundary the other session routes use:
// a foreign or missing session is a 404, never an empty page that pretends the
// session exists. A deployment without the check (a test) skips it.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request, tenantID, sessionID uuid.UUID) bool {
	if s.cfg.SessionExists == nil {
		return true
	}
	exists, err := s.cfg.SessionExists(r.Context(), tenantID, sessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	if !exists {
		writeErr(w, http.StatusNotFound, ErrSessionNotFound)
		return false
	}
	return true
}
