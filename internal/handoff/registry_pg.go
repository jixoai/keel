// PGRegistry: the durable handoff audit (runtime_instances + handoffs).
//
// It runs on the runtime's own pool (the same one internal/store and the
// kernel use) but owns its transactions and its embedded schema, so the
// handoff tables exist as soon as a runtime boots.
package handoff

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed schema/*.sql
var handoffSchemaFS embed.FS

// PGPool is the narrow pool surface the registry needs; *pgxpool.Pool
// satisfies it.
type PGPool interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// PGRegistry is the PostgreSQL Registry.
type PGRegistry struct {
	pool PGPool
}

var _ Registry = (*PGRegistry)(nil)

// NewPGRegistry builds the registry and applies its additive schema. The
// migration takes the same advisory lock store/kernel migrations take, so
// concurrent booters serialize.
func NewPGRegistry(ctx context.Context, pool PGPool) (*PGRegistry, error) {
	r := &PGRegistry{pool: pool}
	if err := r.Migrate(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Migrate applies every file in schema/ in lexical order, retrying transient
// deadlock/serialization failures.
func (r *PGRegistry) Migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(handoffSchemaFS, "schema")
	if err != nil {
		return fmt.Errorf("handoff: read migrations: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if err := r.migrateOnce(ctx, entries); err != nil {
			lastErr = err
			if !isRetryablePG(err) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(50+attempt*75) * time.Millisecond):
			}
			continue
		}
		return nil
	}
	return lastErr
}

// migrateSessionSettings is the same SET LOCAL discipline store and kernel use:
// the timeout-free session belongs to the migration transaction only, and a
// lock wait is bounded by lock_timeout instead of the 5s statement timeout.
const migrateSessionSettings = "SET LOCAL statement_timeout = 0; SET LOCAL lock_timeout = '30s'"

func (r *PGRegistry) migrateOnce(ctx context.Context, entries []fs.DirEntry) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("handoff: migrate begin: %w", err)
	}
	defer tx.Rollback(ctx)
	// Same discipline as store/kernel migrations: DDL cost is bounded by the
	// data, not by the request-path statement timeout. SET LOCAL keeps the
	// timeout-free session inside this transaction; lock_timeout bounds a lock
	// wait and 55P03 is in isRetryablePG, so a busy database backs off instead
	// of failing boot.
	if _, err := tx.Exec(ctx, migrateSessionSettings); err != nil {
		return fmt.Errorf("handoff: migrate settings: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(714322)"); err != nil {
		return fmt.Errorf("handoff: migrate lock: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		sql, err := handoffSchemaFS.ReadFile("schema/" + entry.Name())
		if err != nil {
			return fmt.Errorf("handoff: read %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("handoff: migrate %s: %w", entry.Name(), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("handoff: migrate commit: %w", err)
	}
	return nil
}

func isRetryablePG(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40P01", "40001", "55P03":
			return true
		}
	}
	return false
}

// SaveRuntime upserts one runtime row.
func (r *PGRegistry) SaveRuntime(ctx context.Context, rec RuntimeRecord) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO runtime_instances (runtime_id, loadout_id, loadout_revision, status,
		                                drain_mode, drain_deadline, in_flight_runs,
		                                started_at, ready_at, drain_started_at, stopped_at, reason)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7,
		         $8, $9, $10, $11, NULLIF($12, ''))
		 ON CONFLICT (runtime_id) DO UPDATE SET
		     loadout_id = EXCLUDED.loadout_id,
		     loadout_revision = EXCLUDED.loadout_revision,
		     status = EXCLUDED.status,
		     drain_mode = EXCLUDED.drain_mode,
		     drain_deadline = EXCLUDED.drain_deadline,
		     in_flight_runs = EXCLUDED.in_flight_runs,
		     ready_at = COALESCE(runtime_instances.ready_at, EXCLUDED.ready_at),
		     drain_started_at = COALESCE(EXCLUDED.drain_started_at, runtime_instances.drain_started_at),
		     stopped_at = COALESCE(EXCLUDED.stopped_at, runtime_instances.stopped_at),
		     reason = EXCLUDED.reason`,
		rec.RuntimeID, rec.LoadoutID, rec.Revision, rec.Status, string(rec.Mode),
		nullTime(rec.Deadline), rec.InFlight, nullTime(rec.StartedAt), nullTime(rec.ReadyAt),
		nullTime(rec.DrainAt), nullTime(rec.StoppedAt), rec.Reason)
	if err != nil {
		return fmt.Errorf("handoff: save runtime: %w", err)
	}
	return nil
}

// Runtime reads one runtime row.
func (r *PGRegistry) Runtime(ctx context.Context, runtimeID string) (RuntimeRecord, error) {
	var (
		rec      RuntimeRecord
		mode     *string
		deadline *time.Time
		readyAt  *time.Time
		drainAt  *time.Time
		stopAt   *time.Time
		reason   *string
	)
	err := r.pool.QueryRow(ctx,
		`SELECT runtime_id, loadout_id, loadout_revision, status, drain_mode, drain_deadline,
		        in_flight_runs, started_at, ready_at, drain_started_at, stopped_at, reason
		 FROM runtime_instances WHERE runtime_id = $1`, runtimeID).
		Scan(&rec.RuntimeID, &rec.LoadoutID, &rec.Revision, &rec.Status, &mode, &deadline,
			&rec.InFlight, &rec.StartedAt, &readyAt, &drainAt, &stopAt, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeRecord{}, ErrUnknownRuntime
	}
	if err != nil {
		return RuntimeRecord{}, fmt.Errorf("handoff: read runtime: %w", err)
	}
	fillRuntime(&rec, mode, deadline, readyAt, drainAt, stopAt, reason)
	return rec, nil
}

// Runtimes lists runtime rows, newest first.
func (r *PGRegistry) Runtimes(ctx context.Context, limit int) ([]RuntimeRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx,
		`SELECT runtime_id, loadout_id, loadout_revision, status, drain_mode, drain_deadline,
		        in_flight_runs, started_at, ready_at, drain_started_at, stopped_at, reason
		 FROM runtime_instances ORDER BY started_at DESC, runtime_id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("handoff: list runtimes: %w", err)
	}
	defer rows.Close()
	var out []RuntimeRecord
	for rows.Next() {
		var (
			rec      RuntimeRecord
			mode     *string
			deadline *time.Time
			readyAt  *time.Time
			drainAt  *time.Time
			stopAt   *time.Time
			reason   *string
		)
		if err := rows.Scan(&rec.RuntimeID, &rec.LoadoutID, &rec.Revision, &rec.Status, &mode, &deadline,
			&rec.InFlight, &rec.StartedAt, &readyAt, &drainAt, &stopAt, &reason); err != nil {
			return nil, fmt.Errorf("handoff: scan runtime: %w", err)
		}
		fillRuntime(&rec, mode, deadline, readyAt, drainAt, stopAt, reason)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func fillRuntime(rec *RuntimeRecord, mode *string, deadline, readyAt, drainAt, stopAt *time.Time, reason *string) {
	if mode != nil {
		rec.Mode = Mode(*mode)
	}
	if deadline != nil {
		rec.Deadline = *deadline
	}
	if readyAt != nil {
		rec.ReadyAt = *readyAt
	}
	if drainAt != nil {
		rec.DrainAt = *drainAt
	}
	if stopAt != nil {
		rec.StoppedAt = *stopAt
	}
	if reason != nil {
		rec.Reason = *reason
	}
}

// SaveHandoff inserts one handoff row.
func (r *PGRegistry) SaveHandoff(ctx context.Context, rec HandoffRecord) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO handoffs (handoff_id, tenant_id, session_id, mode, from_runtime, to_runtime,
		                       from_epoch, to_epoch, loadout_id, loadout_revision, status,
		                       in_flight_runs, started_at, finished_at, detail)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		 ON CONFLICT (handoff_id) DO UPDATE SET
		     status = EXCLUDED.status,
		     in_flight_runs = EXCLUDED.in_flight_runs,
		     finished_at = EXCLUDED.finished_at,
		     detail = EXCLUDED.detail`,
		rec.ID, nullUUID(rec.TenantID), nullUUID(rec.SessionID), string(rec.Mode),
		rec.FromRuntime, rec.ToRuntime, rec.FromEpoch, rec.ToEpoch, rec.LoadoutID, rec.Revision,
		rec.Status, rec.InFlight, nullTime(rec.StartedAt), nullTime(rec.FinishedAt), rec.Detail)
	if err != nil {
		return fmt.Errorf("handoff: save handoff: %w", err)
	}
	return nil
}

// FinishHandoff closes a started handoff row.
func (r *PGRegistry) FinishHandoff(ctx context.Context, id, status string, inFlight int, detail string, at time.Time) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE handoffs SET status = $2, in_flight_runs = $3, detail = $4, finished_at = $5
		 WHERE handoff_id = $1`, id, status, inFlight, detail, at)
	if err != nil {
		return fmt.Errorf("handoff: finish handoff: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUnknownHandoff
	}
	return nil
}

// Handoff reads one row.
func (r *PGRegistry) Handoff(ctx context.Context, id string) (HandoffRecord, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT handoff_id, tenant_id, session_id, mode, from_runtime, to_runtime,
		        from_epoch, to_epoch, loadout_id, loadout_revision, status,
		        in_flight_runs, started_at, finished_at, detail
		 FROM handoffs WHERE handoff_id = $1`, id)
	rec, err := scanHandoff(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return HandoffRecord{}, ErrUnknownHandoff
	}
	if err != nil {
		return HandoffRecord{}, fmt.Errorf("handoff: read handoff: %w", err)
	}
	return rec, nil
}

// ListHandoffs lists rows matching the query, newest first.
func (r *PGRegistry) ListHandoffs(ctx context.Context, q HandoffQuery) ([]HandoffRecord, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx,
		`SELECT handoff_id, tenant_id, session_id, mode, from_runtime, to_runtime,
		        from_epoch, to_epoch, loadout_id, loadout_revision, status,
		        in_flight_runs, started_at, finished_at, detail
		 FROM handoffs
		 WHERE ($1::uuid IS NULL OR tenant_id = $1)
		   AND ($2::uuid IS NULL OR session_id = $2)
		   AND ($3 = '' OR from_runtime = $3)
		   AND ($4 = '' OR to_runtime = $4)
		 ORDER BY started_at DESC, handoff_id LIMIT $5`,
		nullUUID(q.TenantID), nullUUID(q.SessionID), q.FromRuntime, q.ToRuntime, limit)
	if err != nil {
		return nil, fmt.Errorf("handoff: list handoffs: %w", err)
	}
	defer rows.Close()
	var out []HandoffRecord
	for rows.Next() {
		rec, err := scanHandoff(rows)
		if err != nil {
			return nil, fmt.Errorf("handoff: scan handoff: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func scanHandoff(row pgx.Row) (HandoffRecord, error) {
	var (
		rec      HandoffRecord
		mode     string
		tenant   *uuid.UUID
		session  *uuid.UUID
		finished *time.Time
	)
	if err := row.Scan(&rec.ID, &tenant, &session, &mode, &rec.FromRuntime, &rec.ToRuntime,
		&rec.FromEpoch, &rec.ToEpoch, &rec.LoadoutID, &rec.Revision, &rec.Status,
		&rec.InFlight, &rec.StartedAt, &finished, &rec.Detail); err != nil {
		return HandoffRecord{}, err
	}
	rec.Mode = Mode(mode)
	if tenant != nil {
		rec.TenantID = *tenant
	}
	if session != nil {
		rec.SessionID = *session
	}
	if finished != nil {
		rec.FinishedAt = *finished
	}
	return rec, nil
}

// nullTime renders a zero time as NULL, so the columns keep their semantics
// ("never happened") instead of storing the zero timestamp.
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nullUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
