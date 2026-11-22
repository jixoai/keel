package supervisor

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// SQLStore adapts *store.Store to the supervisor's persistence seam. It is the
// only place that knows both vocabularies; the supervisor stays storage-agnostic
// and the store gains no dependency on recovery policy.
type SQLStore struct{ st *store.Store }

// NewSQLStore wraps a store. A nil store yields nil (the supervisor's New
// rejects it with a readable error).
func NewSQLStore(st *store.Store) *SQLStore {
	if st == nil {
		return nil
	}
	return &SQLStore{st: st}
}

// ListSupervisorRuns implements Store.
func (s *SQLStore) ListSupervisorRuns(ctx context.Context, limit int, after *RunCursor) ([]RunRecord, error) {
	var cursor *store.SupervisorCursor
	if after != nil {
		cursor = &store.SupervisorCursor{
			UpdatedAt: after.UpdatedAt, TenantID: after.TenantID, SessionID: after.SessionID,
		}
	}
	rows, err := s.st.ListSupervisorRuns(ctx, limit, cursor)
	if err != nil {
		return nil, err
	}
	out := make([]RunRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, RunRecord{
			TenantID: row.TenantID, SessionID: row.SessionID, Status: row.Status,
			Attempts: row.Attempts, LeaseUntil: row.LeaseUntil, CursorSeq: row.CursorSeq,
			LastError: row.LastError, UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

// RequeueRun implements Store.
func (s *SQLStore) RequeueRun(ctx context.Context, tenantID, sessionID uuid.UUID) (RequeueOutcome, error) {
	outcome, err := s.st.RequeueRun(ctx, tenantID, sessionID)
	return RequeueOutcome{Status: outcome.Status, Attempts: outcome.Attempts}, err
}

// ParkRun implements Store.
func (s *SQLStore) ParkRun(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) (bool, error) {
	return s.st.ParkRun(ctx, tenantID, sessionID, reason)
}

// ResumeAttemptCount implements Store.
func (s *SQLStore) ResumeAttemptCount(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error) {
	return s.st.ResumeAttemptCount(ctx, tenantID, sessionID)
}

// AutoResumeCount implements Store.
func (s *SQLStore) AutoResumeCount(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error) {
	return s.st.AutoResumeCount(ctx, tenantID, sessionID)
}

// LastAutoResume implements Store.
func (s *SQLStore) LastAutoResume(ctx context.Context, tenantID, sessionID uuid.UUID) (time.Time, bool, error) {
	return s.st.LastAutoResume(ctx, tenantID, sessionID)
}

// EscalationCount implements Store.
func (s *SQLStore) EscalationCount(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error) {
	return s.st.EscalationCount(ctx, tenantID, sessionID)
}

// EscalationsSince implements Store.
func (s *SQLStore) EscalationsSince(ctx context.Context, tenantID, sessionID uuid.UUID, since time.Time) (int, error) {
	return s.st.EscalationsSince(ctx, tenantID, sessionID, since)
}

// LatestSelection implements Store.
func (s *SQLStore) LatestSelection(ctx context.Context, tenantID, sessionID uuid.UUID) (json.RawMessage, error) {
	return s.st.LatestSelection(ctx, tenantID, sessionID)
}

var _ Store = (*SQLStore)(nil)
