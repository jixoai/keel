package agentapi

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jixoai/keel/internal/store"
)

// PGQuerier is the read-only query seam the session lister needs. pgxpool.Pool
// satisfies it; a test can supply a fake.
type PGQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PGSessionLister answers GET /v1/sessions from PostgreSQL with keyset
// pagination: the cursor is (updated_at, session_id), so pages are stable even
// while sessions are being written (no OFFSET drift, no duplicates).
type PGSessionLister struct {
	db PGQuerier
}

// NewPGSessionLister builds the production session lister over a store.
func NewPGSessionLister(st *store.Store) *PGSessionLister {
	if st == nil {
		return nil
	}
	return &PGSessionLister{db: st.Pool()}
}

// sessionSelect is the whole list projection in one round trip: the session
// row, its run status, the last event time, and the title source — the text of
// the session's first user.message, read with a lateral so the page never
// degrades into a per-row query. Only the payload's "text" field is read: the
// title must never pick up selection/attachments JSON.
const sessionSelect = `SELECT s.session_id, s.created_at, s.updated_at,
       GREATEST(s.next_seq - 1, 0) AS last_seq,
       COALESCE(r.status, '') AS run_status,
       (SELECT max(e.created_at) FROM events e
         WHERE e.tenant_id = s.tenant_id AND e.session_id = s.session_id) AS last_message_at,
       COALESCE(first_input.text, '') AS first_input_text
  FROM sessions s
  LEFT JOIN runs r ON r.tenant_id = s.tenant_id AND r.session_id = s.session_id
  LEFT JOIN LATERAL (
       SELECT e.payload ->> 'text' AS text
         FROM events e
        WHERE e.tenant_id = s.tenant_id AND e.session_id = s.session_id
          AND e.kind = 'user.message'
        ORDER BY e.seq
        LIMIT 1
  ) first_input ON true
 WHERE s.tenant_id = $1`

// ListSessionSummaries reads one page, newest activity first.
func (l *PGSessionLister) ListSessionSummaries(ctx context.Context, tenantID uuid.UUID, limit int, after string) (SessionPage, error) {
	if l == nil || l.db == nil {
		return SessionPage{}, fmt.Errorf("agentapi: session lister is not configured")
	}
	if limit <= 0 || limit > maxSessionsLimit {
		limit = defaultSessionsLimit
	}
	query := sessionSelect + `
 ORDER BY s.updated_at DESC, s.session_id DESC
 LIMIT $2`
	args := []any{tenantID, limit + 1}
	if after != "" {
		cursor, err := decodeSessionCursor(after)
		if err != nil {
			return SessionPage{}, err
		}
		query = sessionSelect + `
   AND (s.updated_at, s.session_id) < ($2::timestamptz, $3::uuid)
 ORDER BY s.updated_at DESC, s.session_id DESC
 LIMIT $4`
		args = []any{tenantID, cursor.updatedAt, cursor.sessionID, limit + 1}
	}
	rows, err := l.db.Query(ctx, query, args...)
	if err != nil {
		return SessionPage{}, fmt.Errorf("agentapi: list sessions: %w", err)
	}
	defer rows.Close()
	type row struct {
		summary SessionSummary
		updated time.Time
		id      uuid.UUID
	}
	collected := make([]row, 0, limit+1)
	for rows.Next() {
		var (
			sessionID     uuid.UUID
			createdAt     time.Time
			updatedAt     time.Time
			lastSeq       int64
			runStatus     string
			lastMessageAt sql.NullTime
			firstInput    sql.NullString
		)
		if err := rows.Scan(&sessionID, &createdAt, &updatedAt, &lastSeq, &runStatus, &lastMessageAt, &firstInput); err != nil {
			return SessionPage{}, fmt.Errorf("agentapi: scan session: %w", err)
		}
		summary := SessionSummary{
			SessionID: sessionID.String(),
			CreatedAt: createdAt.UTC(),
			UpdatedAt: updatedAt.UTC(),
			LastSeq:   lastSeq,
			RunStatus: runStatus,
			Title:     deriveSessionTitle(firstInput.String),
		}
		if lastMessageAt.Valid {
			at := lastMessageAt.Time.UTC()
			summary.LastMessageAt = &at
		}
		collected = append(collected, row{summary: summary, updated: updatedAt, id: sessionID})
	}
	if err := rows.Err(); err != nil {
		return SessionPage{}, fmt.Errorf("agentapi: list sessions: %w", err)
	}
	page := SessionPage{HasMore: len(collected) > limit}
	if page.HasMore {
		collected = collected[:limit]
	}
	page.Sessions = make([]SessionSummary, 0, len(collected))
	for _, entry := range collected {
		page.Sessions = append(page.Sessions, entry.summary)
	}
	if page.HasMore && len(collected) > 0 {
		last := collected[len(collected)-1]
		page.NextCursor = encodeSessionCursor(last.updated, last.id)
	}
	return page, nil
}

type sessionCursor struct {
	updatedAt time.Time
	sessionID uuid.UUID
}

func encodeSessionCursor(updatedAt time.Time, sessionID uuid.UUID) string {
	raw := strconv.FormatInt(updatedAt.UnixMicro(), 10) + ":" + sessionID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeSessionCursor(cursor string) (sessionCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil {
		return sessionCursor{}, fmt.Errorf("%w: not base64url", ErrBadCursor)
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return sessionCursor{}, fmt.Errorf("%w: expected <unixMicro>:<uuid>", ErrBadCursor)
	}
	micros, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return sessionCursor{}, fmt.Errorf("%w: bad timestamp", ErrBadCursor)
	}
	sessionID, err := uuid.Parse(parts[1])
	if err != nil {
		return sessionCursor{}, fmt.Errorf("%w: bad session id", ErrBadCursor)
	}
	return sessionCursor{updatedAt: time.UnixMicro(micros).UTC(), sessionID: sessionID}, nil
}
