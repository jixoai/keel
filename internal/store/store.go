// Package store is the PostgreSQL persistence layer. It is deliberately thin:
// every query carries tenant_id, and the only long-lived object is a pgxpool.
//
// The runtime process holds no conversation state. Events are the sole truth;
// checkpoints are projections; blobs are stored as 64KiB chunk rows so a read
// never materialises a whole image.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaFS embed.FS

// Config tunes the pool for a 2GB host. The defaults follow docs/BACKPRESSURE.md
// §5: enough connections to keep 1000 image streams fed in 2-chunk batches,
// few enough that PG's per-backend memory stays bounded.
type Config struct {
	DSN            string
	MaxConns       int32         // default 24
	MinConns       int32         // default 8
	AcquireTimeout time.Duration // default 300ms
	// StatementTimeout is applied to every connection. A statement that exceeds
	// it is cancelled server-side instead of pinning a backend.
	StatementTimeout time.Duration // default 5s
}

func (c *Config) applyDefaults() {
	if c.MaxConns == 0 {
		c.MaxConns = 24
	}
	if c.MinConns == 0 {
		c.MinConns = 8
	}
	if c.AcquireTimeout == 0 {
		c.AcquireTimeout = 300 * time.Millisecond
	}
	if c.StatementTimeout == 0 {
		c.StatementTimeout = 5 * time.Second
	}
}

// Store wraps a pgx pool.
type Store struct {
	pool           *pgxpool.Pool
	acquireTimeout time.Duration
}

// Open connects, tunes the session, and applies the schema (idempotent).
func Open(ctx context.Context, cfg Config) (*Store, error) {
	cfg.applyDefaults()
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	pc.MaxConnLifetime = 30 * time.Minute
	pc.MaxConnIdleTime = 5 * time.Minute
	pc.HealthCheckPeriod = 30 * time.Second
	pc.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// SET does not accept bind parameters; the value is an int64 we control.
		_, err := conn.Exec(ctx, fmt.Sprintf(
			"SET statement_timeout = %d; SET idle_in_transaction_session_timeout = '3s'",
			cfg.StatementTimeout.Milliseconds()))
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("store: pool: %w", err)
	}
	s := &Store{pool: pool, acquireTimeout: cfg.AcquireTimeout}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool for the blob reader (which needs its own
// keyset queries) without making every query public.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Migrate applies schema.sql. It is safe to run on every start. Concurrent
// migrators (tests, replicas) can deadlock on DDL locks even with the advisory
// lock, so transient deadlock/serialization failures are retried.
func (s *Store) Migrate(ctx context.Context) error {
	sql, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("store: read schema: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if err := s.migrateOnce(ctx, string(sql)); err != nil {
			lastErr = err
			if !isRetryableTx(err) {
				return err
			}
			time.Sleep(time.Duration(50+attempt*75) * time.Millisecond)
			continue
		}
		return nil
	}
	return lastErr
}

func isRetryableTx(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40P01", "40001", "55P03":
			return true
		}
	}
	return false
}

func (s *Store) migrateOnce(ctx context.Context, sql string) error {
	// Serialize concurrent migrations (test packages and replicas start at the
	// same time); the lock is released automatically at commit.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: migrate begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(714322)"); err != nil {
		return fmt.Errorf("store: migrate lock: %w", err)
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: migrate commit: %w", err)
	}
	return nil
}

// Acquire returns a connection with a short timeout: when the pool is saturated
// the caller gets a fast failure it can turn into 503 + Retry-After instead of
// letting the queue grow.
func (s *Store) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, s.acquireTimeout)
	defer cancel()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: acquire: %w", err)
	}
	return conn, nil
}

// CreateTenant inserts a tenant (idempotent by id).
func (s *Store) CreateTenant(ctx context.Context, tenantID uuid.UUID, name string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tenants (tenant_id, name) VALUES ($1, $2)
		 ON CONFLICT (tenant_id) DO NOTHING`, tenantID, name)
	if err != nil {
		return fmt.Errorf("store: create tenant: %w", err)
	}
	return nil
}

// CreateSession inserts a session (idempotent).
func (s *Store) CreateSession(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (tenant_id, session_id) VALUES ($1, $2)
		 ON CONFLICT (tenant_id, session_id) DO NOTHING`, tenantID, sessionID)
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// Event is one committed fact.
type Event struct {
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// AppendEvent commits one event and returns its per-session sequence. The
// sequence is allocated under the session row lock, so two writers on the same
// session cannot interleave but different sessions never contend.
func (s *Store) AppendEvent(ctx context.Context, tenantID, sessionID uuid.UUID, kind string, payload any) (int64, error) {
	seq, _, err := s.AppendEventDedup(ctx, tenantID, sessionID, kind, payload, "")
	return seq, err
}

// AppendEventDedup is AppendEvent with an optional idempotency key. A non-empty
// key that already exists returns the existing sequence with inserted=false and
// writes nothing. The check and the allocation happen under the same session
// row lock, so concurrent retries cannot create a gap or a duplicate.
func (s *Store) AppendEventDedup(ctx context.Context, tenantID, sessionID uuid.UUID, kind string, payload any, dedupeKey string) (int64, bool, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, false, fmt.Errorf("store: marshal event: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		seq, inserted, err := s.appendEventDedupOnce(ctx, tenantID, sessionID, kind, raw, dedupeKey)
		if err == nil {
			return seq, inserted, nil
		}
		lastErr = err
		if !isRetryableTx(err) {
			return 0, false, err
		}
		select {
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case <-time.After(time.Duration(10+attempt*20) * time.Millisecond):
		}
	}
	return 0, false, lastErr
}

// appendEventDedupOnce is one transaction of AppendEventDedup. Concurrent
// session locks and the events insert can deadlock under parallel appends;
// the caller retries that class of failure.
func (s *Store) appendEventDedupOnce(ctx context.Context, tenantID, sessionID uuid.UUID, kind string, raw []byte, dedupeKey string) (int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the session first: it serializes both dedupe checks and sequence
	// allocation for this session.
	var next int64
	err = tx.QueryRow(ctx,
		`SELECT next_seq FROM sessions
		 WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`,
		tenantID, sessionID).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, ErrNoSession
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: lock session: %w", err)
	}
	if dedupeKey != "" {
		var existing int64
		err := tx.QueryRow(ctx,
			`SELECT seq FROM events
			 WHERE tenant_id = $1 AND session_id = $2 AND dedupe_key = $3`,
			tenantID, sessionID, dedupeKey).Scan(&existing)
		if err == nil {
			if err := tx.Commit(ctx); err != nil {
				return 0, false, fmt.Errorf("store: commit dedupe hit: %w", err)
			}
			return existing, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, false, fmt.Errorf("store: dedupe lookup: %w", err)
		}
	}
	seq := next
	if _, err := tx.Exec(ctx,
		`UPDATE sessions SET next_seq = next_seq + 1, updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2`, tenantID, sessionID); err != nil {
		return 0, false, fmt.Errorf("store: allocate seq: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO events (tenant_id, session_id, seq, kind, payload, dedupe_key)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`,
		tenantID, sessionID, seq, kind, raw, dedupeKey); err != nil {
		return 0, false, fmt.Errorf("store: insert event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("store: commit event: %w", err)
	}
	return seq, true, nil
}

// ErrNoSession is returned when the session does not exist for the tenant.
var ErrNoSession = errors.New("store: session not found")

// LoadEvents returns up to limit events with seq > afterSeq, in order.
func (s *Store) LoadEvents(ctx context.Context, tenantID, sessionID uuid.UUID, afterSeq int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx,
		`SELECT seq, kind, payload FROM events
		 WHERE tenant_id = $1 AND session_id = $2 AND seq > $3
		 ORDER BY seq LIMIT $4`, tenantID, sessionID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: load events: %w", err)
	}
	defer rows.Close()
	out := make([]Event, 0, limit)
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.Seq, &ev.Kind, &ev.Payload); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// SaveCheckpoint stores a compaction projection. Events are never deleted.
func (s *Store) SaveCheckpoint(ctx context.Context, tenantID, sessionID uuid.UUID, uptoSeq int64, summary string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO checkpoints (tenant_id, session_id, upto_seq, summary)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, session_id, upto_seq) DO UPDATE SET summary = EXCLUDED.summary`,
		tenantID, sessionID, uptoSeq, summary)
	if err != nil {
		return fmt.Errorf("store: save checkpoint: %w", err)
	}
	return nil
}

// Submission is the idempotent result of a client request.
type Submission struct {
	ID     uuid.UUID
	Seq    *int64
	Status string
}

// Submit records a request idempotently. A replay with the same requestId returns
// the existing row (and never creates a second user message).
func (s *Store) Submit(ctx context.Context, tenantID uuid.UUID, requestID string, sessionID uuid.UUID, submissionID uuid.UUID) (Submission, bool, error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO submissions (tenant_id, request_id, session_id, submission_id, status)
		 VALUES ($1, $2, $3, $4, 'pending')
		 ON CONFLICT (tenant_id, request_id) DO NOTHING`,
		tenantID, requestID, sessionID, submissionID)
	if err != nil {
		return Submission{}, false, fmt.Errorf("store: submit: %w", err)
	}
	created := tag.RowsAffected() == 1
	var out Submission
	var seq *int64
	err = s.pool.QueryRow(ctx,
		`SELECT submission_id, seq, status FROM submissions
		 WHERE tenant_id = $1 AND request_id = $2`, tenantID, requestID).Scan(&out.ID, &seq, &out.Status)
	if err != nil {
		return Submission{}, created, fmt.Errorf("store: read submission: %w", err)
	}
	out.Seq = seq
	return out, created, nil
}

// SetSubmissionStatus updates the submission state and the sequence at which it
// became visible.
func (s *Store) SetSubmissionStatus(ctx context.Context, tenantID uuid.UUID, requestID, status string, seq int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE submissions SET status = $3, seq = $4
		 WHERE tenant_id = $1 AND request_id = $2`, tenantID, requestID, status, seq)
	if err != nil {
		return fmt.Errorf("store: set submission status: %w", err)
	}
	return nil
}
