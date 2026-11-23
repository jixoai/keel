package harnessapi

// PostgreSQL implementations of the session query seams. They follow
// PGSessionLister: one keyset page per call, tenant predicate first, and the
// same tenant-scoped transaction when the store has RLS. Search and export read
// the event ledger; nothing here writes.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jixoai/keel/internal/sessiontitle"
	"github.com/jixoai/keel/internal/store"
)

// PGSessionQuery answers the bounded transcript search and export reads.
type PGSessionQuery struct {
	store *store.Store
}

// NewPGSessionQuery builds the production query implementation over a store.
func NewPGSessionQuery(st *store.Store) *PGSessionQuery {
	if st == nil {
		return nil
	}
	return &PGSessionQuery{store: st}
}

// queryer is what both the pool and a tenant transaction satisfy.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// withTenant runs fn in a tenant-scoped transaction when RLS is enabled, else
// against the operator pool. Every statement still carries tenant_id.
func (q *PGSessionQuery) withTenant(ctx context.Context, tenantID uuid.UUID, fn func(queryer) error) error {
	if q == nil || q.store == nil {
		return errors.New("harnessapi: session query is not configured")
	}
	if q.store.RLSEnabled() {
		return q.store.TenantScoped(ctx, tenantID, func(tx store.TenantTx) error { return fn(tx) })
	}
	var pool *pgxpool.Pool = q.store.Pool()
	if pool == nil {
		return errors.New("harnessapi: session query has no database pool")
	}
	return fn(pool)
}

// messageSearchSelect is the page projection: the message, its session's first
// user message (the title source), and the session's archived state in the
// predicate. Only payload->>'text' is read from the event.
const messageSearchSelect = `SELECT e.session_id, e.seq, e.kind, e.created_at,
       left(e.payload ->> 'text', 4096) AS text,
       COALESCE(first_input.text, '') AS first_input_text
  FROM events e
  JOIN sessions s
    ON s.tenant_id = e.tenant_id AND s.session_id = e.session_id
  LEFT JOIN LATERAL (
       SELECT e2.payload ->> 'text' AS text
         FROM events e2
        WHERE e2.tenant_id = e.tenant_id AND e2.session_id = e.session_id
          AND e2.kind = 'user.message'
        ORDER BY e2.seq
        LIMIT 1
  ) first_input ON true`

// SearchMessages answers one keyset page of transcript matches.
func (q *PGSessionQuery) SearchMessages(ctx context.Context, tenantID uuid.UUID, filter MessageSearchFilter) (MessageHitPage, error) {
	if filter.Limit <= 0 || filter.Limit > maxSearchLimit {
		filter.Limit = defaultSearchLimit
	}
	query := messageSearchSelect + `
 WHERE e.tenant_id = $1
   AND s.archived_at IS NULL
   AND e.kind IN ('user.message', 'assistant.message')
   AND e.payload ? 'text'
   AND strpos(lower(e.payload ->> 'text'), lower($2)) > 0`
	args := []any{tenantID, filter.Text}
	if filter.HasSession {
		args = append(args, filter.SessionID)
		query += fmt.Sprintf(" AND e.session_id = $%d", len(args))
	}
	if filter.Kind != "" {
		args = append(args, filter.Kind)
		query += fmt.Sprintf(" AND e.kind = $%d", len(args))
	}
	if filter.After != "" {
		cursor, err := decodeMessageCursor(filter.After)
		if err != nil {
			return MessageHitPage{}, err
		}
		args = append(args, cursor.at, cursor.sessionID, cursor.seq)
		query += fmt.Sprintf(" AND (e.created_at, e.session_id, e.seq) < ($%d, $%d, $%d)", len(args)-2, len(args)-1, len(args))
	}
	args = append(args, filter.Limit+1)
	query += fmt.Sprintf(` ORDER BY e.created_at DESC, e.session_id DESC, e.seq DESC LIMIT $%d`, len(args))

	hits := make([]MessageHit, 0, filter.Limit+1)
	var last messageCursor
	err := q.withTenant(ctx, tenantID, func(db queryer) error {
		rows, err := db.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("harnessapi: search messages: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var sessionID uuid.UUID
			var seq int64
			var kind, text, firstText string
			var at time.Time
			if err := rows.Scan(&sessionID, &seq, &kind, &at, &text, &firstText); err != nil {
				return fmt.Errorf("harnessapi: scan message hit: %w", err)
			}
			// The cursor names the last row that will be RETURNED. The query
			// fetches limit+1 to detect hasMore, so the extra row must not move
			// the cursor: otherwise the next page would skip it.
			if len(hits) < filter.Limit {
				last = messageCursor{at: at.UTC(), sessionID: sessionID, seq: seq}
			}
			hits = append(hits, MessageHit{
				SessionID:    sessionID.String(),
				SessionTitle: sessiontitle.Derive(firstText),
				Seq:          seq,
				Kind:         kind,
				Role:         strings.TrimSuffix(kind, ".message"),
				At:           at.UTC(),
				Snippet:      messageSnippet(text, filter.Text),
			})
		}
		return rows.Err()
	})
	if err != nil {
		return MessageHitPage{}, err
	}
	page := MessageHitPage{HasMore: len(hits) > filter.Limit}
	if page.HasMore {
		hits = hits[:filter.Limit]
	}
	page.Hits = hits
	if page.HasMore && len(hits) > 0 {
		page.NextCursor = encodeMessageCursor(last.at, last.sessionID, last.seq)
	}
	return page, nil
}

// sessionSearchSelect mirrors the list's title lateral and adds the last
// transcript message (the second searchable field). archived_at filters the
// same soft-deleted rows the list hides.
const sessionSearchSelect = `SELECT s.session_id, s.created_at, s.updated_at,
       (SELECT max(e.created_at) FROM events e
         WHERE e.tenant_id = s.tenant_id AND e.session_id = s.session_id) AS last_message_at,
       COALESCE(first_input.text, '') AS first_input_text,
       COALESCE(last_input.text, '') AS last_input_text
  FROM sessions s
  LEFT JOIN LATERAL (
       SELECT e.payload ->> 'text' AS text
         FROM events e
        WHERE e.tenant_id = s.tenant_id AND e.session_id = s.session_id
          AND e.kind = 'user.message'
        ORDER BY e.seq
        LIMIT 1
  ) first_input ON true
  LEFT JOIN LATERAL (
       SELECT e.payload ->> 'text' AS text
         FROM events e
        WHERE e.tenant_id = s.tenant_id AND e.session_id = s.session_id
          AND e.kind IN ('user.message', 'assistant.message')
          AND e.payload ? 'text'
        ORDER BY e.seq DESC
        LIMIT 1
  ) last_input ON true`

// SearchSessions answers one keyset page of sessions whose derived title or
// last transcript message contains the query.
func (q *PGSessionQuery) SearchSessions(ctx context.Context, tenantID uuid.UUID, filter SessionSearchFilter) (SessionSearchPage, error) {
	if filter.Limit <= 0 || filter.Limit > maxSearchLimit {
		filter.Limit = defaultSearchLimit
	}
	query := sessionSearchSelect + `
 WHERE s.tenant_id = $1
   AND s.archived_at IS NULL
   AND (strpos(lower(COALESCE(first_input.text, '')), lower($2)) > 0
        OR strpos(lower(COALESCE(last_input.text, '')), lower($2)) > 0)`
	args := []any{tenantID, filter.Text}
	if filter.After != "" {
		cursor, err := decodeSessionCursor(filter.After)
		if err != nil {
			return SessionSearchPage{}, err
		}
		args = append(args, cursor.updatedAt, cursor.sessionID)
		query += fmt.Sprintf(" AND (s.updated_at, s.session_id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, filter.Limit+1)
	query += fmt.Sprintf(` ORDER BY s.updated_at DESC, s.session_id DESC LIMIT $%d`, len(args))

	hits := make([]SessionHit, 0, filter.Limit+1)
	var last sessionCursor
	err := q.withTenant(ctx, tenantID, func(db queryer) error {
		rows, err := db.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("harnessapi: search sessions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var sessionID uuid.UUID
			var createdAt, updatedAt time.Time
			var lastMessageAt sql.NullTime
			var firstText, lastText string
			if err := rows.Scan(&sessionID, &createdAt, &updatedAt, &lastMessageAt, &firstText, &lastText); err != nil {
				return fmt.Errorf("harnessapi: scan session hit: %w", err)
			}
			matched := firstText
			if !containsFold(firstText, filter.Text) {
				matched = lastText
			}
			hit := SessionHit{
				SessionID: sessionID.String(),
				Title:     sessiontitle.Derive(firstText),
				CreatedAt: createdAt.UTC(),
				UpdatedAt: updatedAt.UTC(),
				Snippet:   messageSnippet(matched, filter.Text),
			}
			if lastMessageAt.Valid {
				at := lastMessageAt.Time.UTC()
				hit.LastMessageAt = &at
			}
			// Same rule as the message page: the extra row proves hasMore but
			// never moves the cursor.
			if len(hits) < filter.Limit {
				last = sessionCursor{updatedAt: updatedAt.UTC(), sessionID: sessionID}
			}
			hits = append(hits, hit)
		}
		return rows.Err()
	})
	if err != nil {
		return SessionSearchPage{}, err
	}
	page := SessionSearchPage{HasMore: len(hits) > filter.Limit}
	if page.HasMore {
		hits = hits[:filter.Limit]
	}
	page.Hits = hits
	if page.HasMore && len(hits) > 0 {
		page.NextCursor = encodeSessionCursor(last.updatedAt, last.sessionID)
	}
	return page, nil
}

// ExportMessages answers one bounded page of a session's text transcript.
func (q *PGSessionQuery) ExportMessages(ctx context.Context, tenantID, sessionID uuid.UUID, filter ExportFilter) (ExportPage, error) {
	if filter.Limit <= 0 || filter.Limit > maxExportLimit {
		filter.Limit = defaultExportLimit
	}
	if filter.AfterSeq < 0 {
		filter.AfterSeq = 0
	}
	page := ExportPage{SessionID: sessionID.String()}
	err := q.withTenant(ctx, tenantID, func(db queryer) error {
		var firstText sql.NullString
		err := db.QueryRow(ctx, `
			SELECT payload ->> 'text'
			  FROM events
			 WHERE tenant_id = $1 AND session_id = $2 AND kind = 'user.message'
			 ORDER BY seq
			 LIMIT 1`, tenantID, sessionID).Scan(&firstText)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("harnessapi: export title: %w", err)
		}
		if firstText.Valid {
			page.Title = sessiontitle.Derive(firstText.String)
		}
		rows, err := db.Query(ctx, `
			SELECT seq, kind, created_at,
			       left(text_of, $4) AS text,
			       length(text_of) AS total_chars
			  FROM (
			        SELECT seq, kind, created_at,
			               CASE kind
			                 WHEN 'assistant.message' THEN COALESCE(
			                       NULLIF((
			                         SELECT string_agg(elem ->> 'text', E'\n')
			                           FROM jsonb_array_elements(
			                                  CASE WHEN jsonb_typeof(payload -> 'content') = 'array'
			                                       THEN payload -> 'content'
			                                       ELSE '[]'::jsonb END) elem
			                          WHERE elem ->> 'type' = 'text'
			                       ), ''),
			                       payload ->> 'text')
			                 ELSE payload ->> 'text'
			               END AS text_of
			          FROM events
			         WHERE tenant_id = $1 AND session_id = $2 AND seq > $3
			           AND kind IN ('user.message', 'assistant.message')
			       ) transcript
			 WHERE text_of IS NOT NULL AND text_of <> ''
			 ORDER BY seq
			 LIMIT $5`,
			tenantID, sessionID, filter.AfterSeq, exportMessageChars, filter.Limit+1)
		if err != nil {
			return fmt.Errorf("harnessapi: export messages: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var message ExportMessage
			var total int
			if err := rows.Scan(&message.Seq, &message.Kind, &message.At, &message.Text, &total); err != nil {
				return fmt.Errorf("harnessapi: scan exported message: %w", err)
			}
			message.At = message.At.UTC()
			message.Role = strings.TrimSuffix(message.Kind, ".message")
			message.TextTruncated = total > utf8.RuneCountInString(message.Text)
			page.Messages = append(page.Messages, message)
		}
		return rows.Err()
	})
	if err != nil {
		return ExportPage{}, err
	}
	page.HasMore = len(page.Messages) > filter.Limit
	if page.HasMore {
		page.Messages = page.Messages[:filter.Limit]
	}
	if len(page.Messages) > 0 {
		page.NextAfterSeq = page.Messages[len(page.Messages)-1].Seq
	}
	return page, nil
}

// exportMessageChars is how many characters one message carries into the page;
// the HTTP layer applies the byte bound on top.
const exportMessageChars = 32768

func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// messageSnippet is a one-line window around the first match, at most 160
// runes. It never cuts UTF-8 and marks a clipped head/tail with an ellipsis.
func messageSnippet(text, needle string) string {
	const maxRunes = 160
	const contextRunes = 40
	flat := strings.Join(strings.Fields(text), " ")
	if flat == "" {
		return ""
	}
	runes := []rune(flat)
	index := strings.Index(strings.ToLower(flat), strings.ToLower(needle))
	start := 0
	if index >= 0 && needle != "" {
		start = utf8.RuneCountInString(flat[:index]) - contextRunes
		if start < 0 {
			start = 0
		}
	}
	end := start + maxRunes
	if end > len(runes) {
		end = len(runes)
	}
	out := string(runes[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}

type messageCursor struct {
	at        time.Time
	sessionID uuid.UUID
	seq       int64
}

func encodeMessageCursor(at time.Time, sessionID uuid.UUID, seq int64) string {
	raw := strconv.FormatInt(at.UnixMicro(), 10) + ":" + sessionID.String() + ":" + strconv.FormatInt(seq, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeMessageCursor(cursor string) (messageCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil {
		return messageCursor{}, fmt.Errorf("%w: not base64url", ErrBadQueryCursor)
	}
	parts := strings.SplitN(string(raw), ":", 3)
	if len(parts) != 3 {
		return messageCursor{}, fmt.Errorf("%w: expected <unixMicro>:<uuid>:<seq>", ErrBadQueryCursor)
	}
	micros, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return messageCursor{}, fmt.Errorf("%w: bad timestamp", ErrBadQueryCursor)
	}
	sessionID, err := uuid.Parse(parts[1])
	if err != nil {
		return messageCursor{}, fmt.Errorf("%w: bad session id", ErrBadQueryCursor)
	}
	seq, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return messageCursor{}, fmt.Errorf("%w: bad sequence", ErrBadQueryCursor)
	}
	return messageCursor{at: time.UnixMicro(micros).UTC(), sessionID: sessionID, seq: seq}, nil
}
