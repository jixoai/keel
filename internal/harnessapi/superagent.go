package harnessapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

var errSuperagentUnwired = errors.New("the super-agent board is not wired in this deployment")

// SuperagentReader is the read-only projection of the super-agent queue. The
// orchestrator is assembled in the host process; this package only serves its
// ticket board and attention inbox, never creates or mutates work.
type SuperagentReader interface {
	Board(ctx context.Context, tenantID uuid.UUID, filter superagent.BoardFilter) (superagent.BoardView, error)
	Inbox(ctx context.Context, tenantID uuid.UUID) (superagent.AttentionInbox, error)
}

// superagentRoutes mounts the read-only board. A write path (manual ticket
// creation, stop, answer) is deliberately not exposed here in the first slice:
// those actions belong to the Source/Channel and the operator console, and a
// second HTTP write path would need its own authorization matrix.
func (s *Server) superagentRoutes() {
	s.mux.HandleFunc("GET /v1/superagent/tickets", s.listSuperagentTickets)
	s.mux.HandleFunc("GET /v1/superagent/attention", s.listSuperagentAttention)
}

func (s *Server) superagentReady(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	if s.cfg.Superagent == nil || s.cfg.Access == nil {
		writeErr(w, http.StatusServiceUnavailable, errSuperagentUnwired)
		return uuid.Nil, false
	}
	// The board is tenant-scoped. task.read is relation-scoped on individual
	// task cards, so the tenant-level projection checks that the caller holds the
	// key rather than pretending a single row relation covers the whole queue.
	_, tenantID, ok := s.requireHeldKey(w, r, access.KeyTaskRead)
	if !ok {
		return uuid.Nil, false
	}
	return tenantID, true
}

func (s *Server) listSuperagentTickets(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.superagentReady(w, r)
	if !ok {
		return
	}
	filter := superagent.BoardFilter{}
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		filter.Status = collab.WorkItemStatus(raw)
	}
	if _, present := r.URL.Query()["limit"]; present {
		limit, err := queryInt(r, "limit", 100, 1, 500)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		filter.Limit = limit
	}
	board, err := s.cfg.Superagent.Board(r.Context(), tenantID, filter)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, board)
}

func (s *Server) listSuperagentAttention(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.superagentReady(w, r)
	if !ok {
		return
	}
	inbox, err := s.cfg.Superagent.Inbox(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, inbox)
}
