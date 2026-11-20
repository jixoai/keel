// Member task cards of a captain session (change keel-session-surfaces-v1,
// S4: 成员结果进入 captain 时间线).
//
// A sub-agent's card lives in the child session's ledger; its parentTaskId
// points at the captain's card. This endpoint answers the projection the team
// timeline renders — title, status and update time — without copying the card
// and without exposing the child session's transcript. It creates no truth:
// the ledger remains the source, and the only writable path is the task tool's
// task.created/task.updated events.
package agentapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/jixoai/keel/internal/tasklog"
)

// TeamTaskView is one member card on the wire. The fields are the task-card
// projection's own; there is deliberately no `result` or `summary` field; the
// UI must say "member task status", not invent a result sentence.
type TeamTaskView struct {
	TaskID       string    `json:"taskId"`
	ParentTaskID string    `json:"parentTaskId,omitempty"`
	SessionID    string    `json:"sessionId"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	Origin       string    `json:"origin,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	ClosedSeq    int64     `json:"closedSeq,omitempty"`
}

// listTeamTasks implements GET /v1/sessions/{id}/team-tasks.
func (s *Server) listTeamTasks(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.TeamTasks == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("team task projection is not wired"))
		return
	}
	// The session boundary is checked before the projection: a foreign or
	// missing session answers 404, exactly like the event log's read, instead
	// of an empty 200 page that pretends the session exists.
	if s.cfg.SessionExists != nil {
		exists, err := s.cfg.SessionExists(r.Context(), tenantID, sessionID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !exists {
			writeErr(w, http.StatusNotFound, ErrSessionNotFound)
			return
		}
	}
	limit, err := queryInt(r, "limit", tasklog.DefaultTeamTaskLimit, 1, tasklog.MaxTeamTaskLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	page, err := s.cfg.TeamTasks.ListTeamTasks(r.Context(), tenantID, sessionID, limit, r.URL.Query().Get("after"))
	if err != nil {
		if errors.Is(err, tasklog.ErrInvalidCursor) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	tasks := make([]TeamTaskView, 0, len(page.Cards))
	for _, card := range page.Cards {
		view := TeamTaskView{
			TaskID:    card.TaskID.String(),
			SessionID: card.SessionID.String(),
			Title:     card.Title,
			Status:    card.Status,
			Origin:    card.Origin,
			CreatedAt: card.CreatedAt.UTC(),
			UpdatedAt: card.UpdatedAt.UTC(),
			ClosedSeq: card.ClosedSeq,
		}
		if card.ParentTaskID != nil {
			view.ParentTaskID = card.ParentTaskID.String()
		}
		tasks = append(tasks, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":      tasks,
		"nextCursor": page.NextCursor,
		"hasMore":    page.HasMore,
	})
}
