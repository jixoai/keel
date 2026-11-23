package harnessapi

// The task panel's reads (change keel-harness-v2, W4): the task tree with its
// member cards and external ancestors, the session's task event stream, and the
// live resource references. They are projections over task_cards/task_events;
// no route here writes task state and none reads the raw ledger payload itself
// (the task-log decoder owns that).
//
//   GET /v1/sessions/{id}/task-panel       the tree + events + attachments
//
// The existing reads stay as they are: `GET /v1/sessions/{id}/tasks` is the
// composer's ``$`` group (its shape is frozen) and
// `GET /v1/sessions/{id}/team-tasks` is the captain's member page. The panel
// needs the parents, the attachment set and the event stream, which neither of
// those carries; folding them into this one read keeps the panel a single
// projection read instead of four round trips.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/tasklog"
)

// TaskPanelTree is one card of the panel's tree on the wire.
type TaskPanelTree struct {
	TaskID       string    `json:"taskId"`
	ParentTaskID string    `json:"parentTaskId,omitempty"`
	SessionID    string    `json:"sessionId"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	Origin       string    `json:"origin,omitempty"`
	OriginSeq    int64     `json:"originSeq,omitempty"`
	CreatedSeq   int64     `json:"createdSeq,omitempty"`
	UpdatedSeq   int64     `json:"updatedSeq,omitempty"`
	ClosedSeq    int64     `json:"closedSeq,omitempty"`
	CreatedAt    time.Time `json:"createdAt,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt,omitempty"`
	// Member is true for a card whose row lives in another session.
	Member bool `json:"member,omitempty"`
}

// TaskPanelEvent is one task event row on the wire.
type TaskPanelEvent struct {
	TaskID       string    `json:"taskId"`
	Kind         string    `json:"kind"`
	Seq          int64     `json:"seq"`
	At           time.Time `json:"at,omitempty"`
	Title        string    `json:"title,omitempty"`
	Status       string    `json:"status,omitempty"`
	ParentTaskID string    `json:"parentTaskId,omitempty"`
	Origin       string    `json:"origin,omitempty"`
	OriginSeq    int64     `json:"originSeq,omitempty"`
	Path         string    `json:"path,omitempty"`
	Hash         string    `json:"hash,omitempty"`
	Text         string    `json:"text,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Unparsed     bool      `json:"unparsed,omitempty"`
}

// TaskPanelAttachment is one live (path, hash) reference on the wire. The bytes
// stay in the file layer; the panel resolves a reference through the existing
// files/blob reads, never through an invented path.
type TaskPanelAttachment struct {
	TaskID string `json:"taskId"`
	Path   string `json:"path"`
	Hash   string `json:"hash"`
}

type TaskPanelView struct {
	SessionID   string                `json:"sessionId"`
	Cards       []TaskPanelTree       `json:"cards"`
	Members     []TaskPanelTree       `json:"members"`
	Ancestors   []TaskPanelTree       `json:"ancestors"`
	Attachments []TaskPanelAttachment `json:"attachments"`
	Events      []TaskPanelEvent      `json:"events"`
	Limits      TaskPanelLimits       `json:"limits"`
}

type TaskPanelLimits struct {
	CardLimit      int  `json:"cardLimit"`
	CardsTruncated bool `json:"cardsTruncated"`
	MemberLimit    int  `json:"memberLimit"`
	Truncated      bool `json:"truncated"`
	// TruncatedMemberCount is exact when the read saw every linked row and a
	// floor (at least 1) when the read itself stopped at its bound.
	TruncatedMemberCount int  `json:"truncatedMemberCount,omitempty"`
	EventLimit           int  `json:"eventLimit"`
	EventsTruncated      bool `json:"eventsTruncated"`
	// AttachmentLimit bounds the attach/detach history the live resource set
	// was folded from; AttachmentsTruncated says an older reference may be
	// missing from that set.
	AttachmentLimit      int  `json:"attachmentLimit"`
	AttachmentsTruncated bool `json:"attachmentsTruncated"`
	AncestorsTruncated   bool `json:"ancestorsTruncated"`
}

// TaskPanelLister is the task-log projection read behind the panel route. Nil
// answers 503 and the capability is reported false.
type TaskPanelLister interface {
	Panel(ctx context.Context, scope kernel.Scope, memberLimit, eventLimit int) (tasklog.Panel, error)
}

// SessionFileReader is the file plugin's bounded read, scoped to a session
// exactly like the model's read tool. The panel resolves an attached (path,
// hash) through this seam: the path must exist in the session's own manifest,
// so a preview cannot name a file the session does not hold, and the hash it
// reports is the manifest's current one (the panel compares it with the
// reference instead of pretending the reference still matches). Nil omits the
// preview route and the capability flag.
type SessionFileReader interface {
	ReadEntry(ctx context.Context, scope fileplugin.Scope, path string, offset, limit int) (fileplugin.ReadResult, error)
}

// FilePreviewView is one bounded page of a session file.
type FilePreviewView struct {
	Path          string `json:"path"`
	Hash          string `json:"hash"`
	Size          int64  `json:"size"`
	Seq           int64  `json:"seq,omitempty"`
	Offset        int    `json:"offset"`
	Limit         int    `json:"limit"`
	TotalLines    int    `json:"totalLines"`
	ReturnedLines int    `json:"returnedLines"`
	NextOffset    int    `json:"nextOffset"`
	Truncated     bool   `json:"truncated"`
	Content       string `json:"content"`
	// ReferenceHash echoes the (path, hash) the caller asked to preview.
	ReferenceHash string `json:"referenceHash,omitempty"`
	// MatchesReference is false when the session's file changed since the
	// reference was recorded: the bytes shown are the current ones, and the
	// panel must say so rather than claim the old reference was read.
	MatchesReference *bool `json:"matchesReference,omitempty"`
}

func (s *Server) taskPanelRoutes() {
	s.mux.HandleFunc("GET /v1/sessions/{id}/task-panel", s.getTaskPanel)
	s.mux.HandleFunc("GET /v1/sessions/{id}/file-preview", s.getFilePreview)
}

// getFilePreview resolves one path through the session's own manifest and
// answers a bounded page. The path is never invented: it must be in the
// manifest, and the hash the answer carries is the manifest's, not the caller's.
func (s *Server) getFilePreview(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.FilePreview == nil {
		notWired(w, "the session file preview is not wired in this deployment")
		return
	}
	if !s.requireSession(w, r, tenantID, sessionID) {
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeErr(w, http.StatusBadRequest, errors.New("path is required"))
		return
	}
	offset, err := queryInt(r, "offset", 0, 0, 1<<20)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	limit, err := queryInt(r, "limit", fileplugin.DefaultReadLines, 1, fileplugin.MaxReadLines)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.cfg.FilePreview.ReadEntry(r.Context(),
		fileplugin.Scope{TenantID: tenantID, SessionID: sessionID}, path, offset, limit)
	if err != nil {
		if code := fileplugin.CodeOf(err); code == fileplugin.CodeNotFound {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	view := FilePreviewView{
		Path:          result.Path,
		Hash:          result.Hash,
		Size:          result.Size,
		Seq:           result.Seq,
		Offset:        result.Offset,
		Limit:         result.Limit,
		TotalLines:    result.TotalLines,
		ReturnedLines: result.ReturnedLines,
		NextOffset:    result.NextOffset,
		Truncated:     result.Truncated,
		Content:       result.Content,
	}
	if reference := strings.TrimSpace(r.URL.Query().Get("hash")); reference != "" {
		view.ReferenceHash = reference
		matches := result.Hash == reference
		view.MatchesReference = &matches
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) getTaskPanel(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.TaskPanel == nil {
		notWired(w, "the task panel projection is not wired in this deployment")
		return
	}
	if !s.requireSession(w, r, tenantID, sessionID) {
		return
	}
	memberLimit, err := queryInt(r, "memberLimit", tasklog.DefaultPanelMemberLimit, 1, tasklog.MaxPanelMemberLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	eventLimit, err := queryInt(r, "eventLimit", tasklog.DefaultPanelEventLimit, 1, tasklog.MaxPanelEventLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	panel, err := s.cfg.TaskPanel.Panel(r.Context(),
		kernel.Scope{TenantID: tenantID, SessionID: sessionID}, memberLimit, eventLimit)
	if err != nil {
		if errors.Is(err, tasklog.ErrTaskRequired) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, panelView(panel))
}

func panelView(panel tasklog.Panel) TaskPanelView {
	own := map[uuid.UUID]bool{}
	for _, card := range panel.Cards {
		own[card.TaskID] = true
	}
	toTree := func(card tasklog.Card, member bool) TaskPanelTree {
		view := TaskPanelTree{
			TaskID:     card.TaskID.String(),
			SessionID:  card.SessionID.String(),
			Title:      card.Title,
			Status:     card.Status,
			Origin:     card.Origin,
			OriginSeq:  card.OriginSeq,
			CreatedSeq: card.CreatedSeq,
			UpdatedSeq: card.UpdatedSeq,
			ClosedSeq:  card.ClosedSeq,
			Member:     member,
		}
		if card.ParentTaskID != nil {
			view.ParentTaskID = card.ParentTaskID.String()
		}
		if !card.CreatedAt.IsZero() {
			view.CreatedAt = card.CreatedAt.UTC()
		}
		if !card.UpdatedAt.IsZero() {
			view.UpdatedAt = card.UpdatedAt.UTC()
		}
		return view
	}
	view := TaskPanelView{
		SessionID:   panel.SessionID.String(),
		Cards:       make([]TaskPanelTree, 0, len(panel.Cards)),
		Members:     make([]TaskPanelTree, 0, len(panel.Members)),
		Ancestors:   make([]TaskPanelTree, 0, len(panel.Ancestors)),
		Attachments: make([]TaskPanelAttachment, 0, len(panel.Attachments)),
		Events:      make([]TaskPanelEvent, 0, len(panel.Events)),
		Limits: TaskPanelLimits{
			CardLimit:            panel.CardLimit,
			CardsTruncated:       panel.CardsTruncated,
			MemberLimit:          panel.MemberLimit,
			Truncated:            panel.Truncated,
			TruncatedMemberCount: panel.TruncatedMemberCount,
			EventLimit:           panel.EventLimit,
			EventsTruncated:      panel.EventsTruncated,
			AttachmentLimit:      panel.AttachmentLimit,
			AttachmentsTruncated: panel.AttachmentsTruncated,
			AncestorsTruncated:   panel.AncestorsTruncated,
		},
	}
	for _, card := range panel.Cards {
		view.Cards = append(view.Cards, toTree(card, false))
	}
	for _, card := range panel.Members {
		// A member row may live outside the asked session; own is what the
		// card's own session says. A linked card whose session is the asked one
		// is somehow not reported as a member (the fold drops duplicates).
		view.Members = append(view.Members, toTree(card, !own[card.TaskID]))
	}
	for _, card := range panel.Ancestors {
		view.Ancestors = append(view.Ancestors, toTree(card, true))
	}
	for _, attachment := range panel.Attachments {
		view.Attachments = append(view.Attachments, TaskPanelAttachment{
			TaskID: attachment.TaskID.String(),
			Path:   attachment.Path,
			Hash:   attachment.Hash,
		})
	}
	for _, event := range panel.Events {
		wire := TaskPanelEvent{
			TaskID:       event.TaskID.String(),
			Kind:         event.Kind,
			Seq:          event.Seq,
			Title:        event.Title,
			Status:       event.Status,
			ParentTaskID: event.ParentTaskID,
			Origin:       event.Origin,
			OriginSeq:    event.OriginSeq,
			Path:         event.Path,
			Hash:         event.Hash,
			Text:         event.Text,
			Reason:       event.Reason,
			Unparsed:     event.Unparsed,
		}
		if !event.At.IsZero() {
			wire.At = event.At.UTC()
		}
		view.Events = append(view.Events, wire)
	}
	return view
}
