package kernel

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema/*.sql
var kernelSchemaFS embed.FS

// PGPool is the narrow pool surface PGStore needs. *pgxpool.Pool — which is
// what store.Store.Pool() returns — satisfies it, so the kernel does not
// import internal/store and cannot use any of its query methods: PGStore owns
// its own transactional SQL.
type PGPool interface {
	queryer
	Begin(ctx context.Context) (pgx.Tx, error)
}

// queryer is the surface shared by the pool and an open transaction.
type queryer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var _ PGPool = (*pgxpool.Pool)(nil)

// PGStore is the PostgreSQL LogStore used by the runtime. It runs on the same
// pool as internal/store (same DSN, same statement timeouts) but owns its own
// transactions and treats the session row as the single linearization point:
//
//   - the sessions row carries owner / owner_epoch / lease_until, so the lease
//     authority sits on the same row whose lock already serializes sequence
//     allocation;
//   - Commit locks the session row (BeginSession) and re-reads the epoch there;
//   - Claim / Renew / Finish / Release take the session row lock first and only
//     then touch the runs row, so every transaction follows the fixed lock
//     order sessions -> runs (no claim/finish deadlock);
//   - the runs columns of the same names are a mirror for the legacy queue
//     scan and for run-status readers, written only under the session lock;
//   - every claim validates the run state in the claim write itself
//     (claimablePredicate), so a run settled between the candidate snapshot and
//     the write can never be resurrected;
//   - every write transaction is replayed on transient failures (deadlock /
//     serialization / lock unavailable) with the store's retry budget.
type PGStore struct {
	pool    PGPool
	retries atomic.Int64 // transient failures replayed (evidence for tests)
}

var _ LogStore = (*PGStore)(nil)

// NewPG wraps a pool and applies the kernel's additive migrations. Pass
// store.Store.Pool() — the pool must already carry the base schema
// (store.Open migrates it).
func NewPG(ctx context.Context, pool PGPool) (*PGStore, error) {
	s := &PGStore{pool: pool}
	if err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Migrate applies every file in schema/ in lexical order. It takes the same
// advisory lock store.Migrate takes, so the two migrations never run
// concurrently, and retries transient deadlock/serialization failures like the
// store does.
func (s *PGStore) Migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(kernelSchemaFS, "schema")
	if err != nil {
		return fmt.Errorf("kernel: read migrations: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < retryBudget; attempt++ {
		if err := s.migrateOnce(ctx, entries); err != nil {
			lastErr = err
			if !isRetryablePG(err) {
				return err
			}
			if waitErr := waitRetry(ctx, attempt); waitErr != nil {
				return waitErr
			}
			continue
		}
		return nil
	}
	return lastErr
}

func (s *PGStore) migrateOnce(ctx context.Context, entries []fs.DirEntry) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("kernel: migrate begin: %w", err)
	}
	defer tx.Rollback(ctx)
	// Same discipline as store.Migrate: SET LOCAL so the timeout-free session
	// never leaks into pooled connections; lock_timeout keeps a lock wait
	// bounded (55P03 is retried by the caller).
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 0; SET LOCAL lock_timeout = '30s'"); err != nil {
		return fmt.Errorf("kernel: migrate settings: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(714322)"); err != nil {
		return fmt.Errorf("kernel: migrate lock: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		sql, err := kernelSchemaFS.ReadFile("schema/" + entry.Name())
		if err != nil {
			return fmt.Errorf("kernel: read %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("kernel: migrate %s: %w", entry.Name(), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("kernel: migrate commit: %w", err)
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

// retryTx replays a whole transaction on transient failures. Each attempt
// starts from a fresh transaction; the operations are conditional (owner+epoch
// guarded) so a replay can never double-apply.
func (s *PGStore) retryTx(ctx context.Context, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < retryBudget; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryableTx(err) && !isRetryablePG(err) {
			return err
		}
		s.retries.Add(1)
		if waitErr := waitRetry(ctx, attempt); waitErr != nil {
			return waitErr
		}
	}
	return lastErr
}

// pgFail classifies a driver error, marking transient transaction failures so
// the kernel's Commit retry loop can replay the whole attempt.
func pgFail(err error) error {
	if err == nil {
		return nil
	}
	if isRetryablePG(err) {
		return markRetryable(err)
	}
	return err
}

// BeginSession opens the session transaction: it locks the session row — the
// linearization point shared with Claim / Renew / Finish — and fails with
// ErrNotFound for a pair that does not exist.
func (s *PGStore) BeginSession(ctx context.Context, tenantID, sessionID uuid.UUID) (SessionTxn, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("kernel: begin session: %w", pgFail(err))
	}
	var (
		next       int64
		stateOwner string
		extStatus  string
	)
	err = tx.QueryRow(ctx,
		`SELECT next_seq, COALESCE(state_owner, 'keel'), COALESCE(external_status, '')
		 FROM sessions
		 WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`,
		tenantID, sessionID).Scan(&next, &stateOwner, &extStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		tx.Rollback(ctx)
		return nil, fmt.Errorf("kernel: session %s/%s: %w", tenantID, sessionID, ErrNotFound)
	}
	if err != nil {
		tx.Rollback(ctx)
		return nil, fmt.Errorf("kernel: lock session: %w", pgFail(err))
	}
	return &pgTxn{
		tx: tx, tenantID: tenantID, sessionID: sessionID, nextSeq: next,
		stateOwner: stateOwner, externalStatus: extStatus,
	}, nil
}

// pgTxn is one session transaction; it holds the session row lock.
type pgTxn struct {
	tx        pgx.Tx
	tenantID  uuid.UUID
	sessionID uuid.UUID
	nextSeq   int64
	// stateOwner and externalStatus are read by the same locking SELECT that
	// BeginSession already runs, so the external-lane check costs no extra
	// query.
	stateOwner     string
	externalStatus string
}

func (t *pgTxn) Lease(ctx context.Context) (LeaseState, error) {
	scope := Scope{TenantID: t.tenantID, SessionID: t.sessionID}
	state, _, err := sessionLease(ctx, t.tx, scope)
	return state, err
}

func (t *pgTxn) ExternalState(context.Context) (ExternalState, error) {
	return ExternalState{StateOwner: t.stateOwner, Status: t.externalStatus}, nil
}

func (t *pgTxn) Dedupe(ctx context.Context, key string) (Event, bool, error) {
	if key == "" {
		return Event{}, false, nil
	}
	var ev Event
	err := t.tx.QueryRow(ctx,
		`SELECT seq, kind, payload FROM events
		 WHERE tenant_id = $1 AND session_id = $2 AND dedupe_key = $3`,
		t.tenantID, t.sessionID, key).Scan(&ev.Seq, &ev.Kind, &ev.Payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("kernel: dedupe lookup: %w", pgFail(err))
	}
	return ev, true, nil
}

// FindToolCall answers the pairing check with one indexed lookup: the partial
// GIN index on assistant payloads (schema/0002) lets the jsonpath probe find
// the newest message carrying the call without scanning the log. Should the
// index be missing the query still answers correctly (filtered reverse scan),
// which is what the README records as the degraded mode.
func (t *pgTxn) FindToolCall(ctx context.Context, callID string) (ToolCallRef, bool, error) {
	path, err := toolCallPath(callID)
	if err != nil {
		return ToolCallRef{}, false, err
	}
	var (
		seq     int64
		payload []byte
	)
	err = t.tx.QueryRow(ctx,
		`SELECT seq, payload FROM events
		 WHERE tenant_id = $1 AND session_id = $2 AND kind = $3 AND payload @? $4::jsonpath
		 ORDER BY seq DESC LIMIT 1`,
		t.tenantID, t.sessionID, string(KindAssistantMessage), path).Scan(&seq, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return ToolCallRef{}, false, nil
	}
	if err != nil {
		return ToolCallRef{}, false, fmt.Errorf("kernel: tool call lookup: %w", pgFail(err))
	}
	calls, err := toolCallBlocks(payload)
	if err != nil {
		// A malformed payload cannot back a pairing decision; treat the call as
		// absent instead of failing the commit on legacy-written data.
		return ToolCallRef{}, false, nil
	}
	for _, c := range calls {
		if c.ID == callID {
			return ToolCallRef{ToolCallID: c.ID, ToolName: c.Name, Seq: seq}, true, nil
		}
	}
	return ToolCallRef{}, false, nil
}

// toolCallPath builds the jsonpath matching a toolCall block with this id. The
// id is JSON-escaped, so any id is safe, and the path travels as a bind
// parameter (never string-concatenated into SQL).
func toolCallPath(callID string) (string, error) {
	quoted, err := json.Marshal(callID)
	if err != nil {
		return "", fmt.Errorf("kernel: cannot encode tool call id: %w", err)
	}
	return `$.content[*] ? (@.type == "toolCall" && @.id == ` + string(quoted) + `)`, nil
}

func (t *pgTxn) Append(ctx context.Context, kind EventKind, payload json.RawMessage, dedupeKey string) (int64, error) {
	seq := t.nextSeq
	if _, err := t.tx.Exec(ctx,
		`UPDATE sessions SET next_seq = next_seq + 1, updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2`,
		t.tenantID, t.sessionID); err != nil {
		return 0, fmt.Errorf("kernel: allocate seq: %w", pgFail(err))
	}
	if _, err := t.tx.Exec(ctx,
		`INSERT INTO events (tenant_id, session_id, seq, kind, payload, dedupe_key)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`,
		t.tenantID, t.sessionID, seq, string(kind), []byte(payload), dedupeKey); err != nil {
		return 0, fmt.Errorf("kernel: insert event: %w", pgFail(err))
	}
	if kind == KindUserMessage {
		// Edge input wakes the run in the SAME transaction as the append: there
		// is no window where the client's message is committed but no run is
		// queued for it, so a crash between the commit and the HTTP layer's
		// bookkeeping cannot strand the input. A running run keeps running —
		// its finish probe (read-through watermark) decides whether this input
		// needs another turn — and an external session is never enqueued (it
		// has no native run/lease flow).
		if _, err := t.tx.Exec(ctx,
			`INSERT INTO runs (tenant_id, session_id, status, pending_input)
			 SELECT $1, $2, 'pending', true
			 WHERE NOT EXISTS (
			     SELECT 1 FROM sessions
			     WHERE tenant_id = $1 AND session_id = $2 AND state_owner = 'external')
			 ON CONFLICT (tenant_id, session_id) DO UPDATE
			 SET status = CASE WHEN runs.status = 'running' THEN 'running' ELSE 'pending' END,
			     pending_input = true,
			     updated_at = now()
			 WHERE NOT EXISTS (
			     SELECT 1 FROM sessions
			     WHERE tenant_id = $1 AND session_id = $2 AND state_owner = 'external')`,
			t.tenantID, t.sessionID); err != nil {
			return 0, fmt.Errorf("kernel: wake run: %w", pgFail(err))
		}
	}
	t.nextSeq++
	return seq, nil
}

func (t *pgTxn) Commit(ctx context.Context) error {
	if err := t.tx.Commit(ctx); err != nil {
		return fmt.Errorf("kernel: commit event: %w", pgFail(err))
	}
	return nil
}

func (t *pgTxn) Rollback(ctx context.Context) error {
	err := t.tx.Rollback(ctx)
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return err
}

// sessionLeaseSQL reads the lease authority from the session row.
const sessionLeaseSQL = `SELECT COALESCE(owner, ''), owner_epoch,
	       COALESCE(owner IS NOT NULL AND lease_until IS NOT NULL, false),
	       COALESCE(lease_until > now(), false),
	       COALESCE(lease_until, 'epoch'::timestamptz)
	FROM sessions WHERE tenant_id = $1 AND session_id = $2`

// sessionLease reads the lease authority of one session. found=false means the
// session row itself does not exist.
func sessionLease(ctx context.Context, q queryer, scope Scope) (LeaseState, bool, error) {
	var (
		owner string
		epoch int64
		until time.Time
	)
	state := LeaseState{}
	err := q.QueryRow(ctx, sessionLeaseSQL, scope.TenantID, scope.SessionID).
		Scan(&owner, &epoch, &state.Found, &state.Live, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeaseState{}, false, nil
	}
	if err != nil {
		return LeaseState{}, false, fmt.Errorf("kernel: session lease: %w", pgFail(err))
	}
	state.Lease = Lease{Scope: scope, Token: OwnerToken{Owner: owner, Epoch: epoch}, Until: until}
	return state, true, nil
}

// LoadEvents reads a page of the log; an empty page is checked against the
// sessions row so a foreign or missing session answers ErrNotFound.
func (s *PGStore) LoadEvents(ctx context.Context, tenantID, sessionID uuid.UUID, afterSeq int64, limit int) ([]Event, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT seq, kind, payload FROM events
		 WHERE tenant_id = $1 AND session_id = $2 AND seq > $3
		 ORDER BY seq LIMIT $4`, tenantID, sessionID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("kernel: load events: %w", err)
	}
	defer rows.Close()
	out := make([]Event, 0, limit)
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.Seq, &ev.Kind, &ev.Payload); err != nil {
			return nil, fmt.Errorf("kernel: scan event: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		exists, err := s.SessionExists(ctx, tenantID, sessionID)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("kernel: session %s/%s: %w", tenantID, sessionID, ErrNotFound)
		}
	}
	return out, nil
}

// FindDedupe looks up one committed event by its namespaced dedupe key.
func (s *PGStore) FindDedupe(ctx context.Context, tenantID, sessionID uuid.UUID, key string) (Event, bool, error) {
	if key == "" {
		return Event{}, false, nil
	}
	var ev Event
	err := s.pool.QueryRow(ctx,
		`SELECT seq, kind, payload FROM events
		 WHERE tenant_id = $1 AND session_id = $2 AND dedupe_key = $3`,
		tenantID, sessionID, key).Scan(&ev.Seq, &ev.Kind, &ev.Payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("kernel: dedupe lookup: %w", err)
	}
	return ev, true, nil
}

// SessionExists reports whether the (tenant, session) pair exists.
func (s *PGStore) SessionExists(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM sessions WHERE tenant_id = $1 AND session_id = $2)`,
		tenantID, sessionID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("kernel: session probe: %w", err)
	}
	return exists, nil
}

// claimablePredicate builds the predicate that makes a run claimable: pending,
// or running with an expired (or never set) lease. alias qualifies every column
// for the joined candidate scan ("" for the single-table guard write). The
// predicate is used both to pick a candidate and to guard the claim write
// itself, so a run settled between the candidate snapshot and the write cannot
// be resurrected. The lease columns here are the runs mirror; the authority is
// the session row, which the claim holds.
func claimablePredicate(alias string) string {
	return alias + `status = 'pending' OR (` + alias + `status = 'running' AND (` +
		alias + `lease_until IS NULL OR ` + alias + `lease_until < now()))`
}

// claimCandidate picks one runnable run and locks its session row, skipping
// sessions another claimer already holds. Claimability is read from the
// authority (sessions), so the candidate stays correct under concurrency; the
// runs row is only a snapshot here and is re-validated by the guarded claim.
var claimCandidate = `SELECT r.tenant_id, r.session_id
	FROM runs r
	JOIN sessions s ON s.tenant_id = r.tenant_id AND s.session_id = r.session_id
	WHERE (` + claimablePredicate("r.") + `)
	  AND (s.lease_until IS NULL OR s.lease_until < now())
	  AND s.state_owner <> 'external'
	ORDER BY r.updated_at
	LIMIT 1
	FOR UPDATE OF s SKIP LOCKED`

// errClaimCandidateLost means the candidate stopped being claimable between the
// scan and the guarded write (another writer settled or re-leased it). The
// caller rolls back and re-scans.
var errClaimCandidateLost = errors.New("kernel: claim candidate lost")

// claimScanAttempts bounds the re-scan loop of a global claim.
const claimScanAttempts = 4

// ClaimLease claims any runnable run: it locks the candidate's session row,
// increments the epoch there, then claims the run row under the claimable
// predicate. A candidate that stopped being claimable is re-scanned.
func (s *PGStore) ClaimLease(ctx context.Context, owner string, ttl time.Duration) (Lease, error) {
	var lease Lease
	err := s.retryTx(ctx, func() error {
		var err error
		lease, err = s.claimLease(ctx, owner, ttl)
		return err
	})
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func (s *PGStore) claimLease(ctx context.Context, owner string, ttl time.Duration) (Lease, error) {
	var lastErr error
	for attempt := 0; attempt < claimScanAttempts; attempt++ {
		lease, err := s.claimCandidateOnce(ctx, owner, ttl)
		switch {
		case err == nil:
			return lease, nil
		case errors.Is(err, errClaimCandidateLost):
			lastErr = err
			continue
		default:
			return Lease{}, err
		}
	}
	return Lease{}, fmt.Errorf("kernel: claim: %w (candidate kept changing: %v)", ErrNoRunnable, lastErr)
}

func (s *PGStore) claimCandidateOnce(ctx context.Context, owner string, ttl time.Duration) (Lease, error) {
	var lease Lease
	err := func() error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("kernel: claim begin: %w", pgFail(err))
		}
		defer tx.Rollback(ctx)
		var scope Scope
		err = tx.QueryRow(ctx, claimCandidate).Scan(&scope.TenantID, &scope.SessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("kernel: claim: %w", ErrNoRunnable)
		}
		if err != nil {
			return fmt.Errorf("kernel: claim candidate: %w", pgFail(err))
		}
		lease, err = claimLocked(ctx, tx, scope, owner, ttl)
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("kernel: claim commit: %w", pgFail(err))
		}
		return nil
	}()
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// ClaimScope claims one specific session. It locks the session row first (lock
// order sessions -> runs), checks the authority, then claims the run row under
// the same claimable predicate as the global path.
func (s *PGStore) ClaimScope(ctx context.Context, scope Scope, owner string, ttl time.Duration) (Lease, error) {
	var lease Lease
	err := s.retryTx(ctx, func() error {
		var err error
		lease, err = s.claimScope(ctx, scope, owner, ttl)
		return err
	})
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func (s *PGStore) claimScope(ctx context.Context, scope Scope, owner string, ttl time.Duration) (Lease, error) {
	var lease Lease
	err := func() error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("kernel: claim scope begin: %w", pgFail(err))
		}
		defer tx.Rollback(ctx)

		var (
			next       int64
			stateOwner string
		)
		err = tx.QueryRow(ctx,
			`SELECT next_seq, COALESCE(state_owner, 'keel') FROM sessions
			 WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`,
			scope.TenantID, scope.SessionID).Scan(&next, &stateOwner)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("kernel: claim scope: %w", ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("kernel: lock session: %w", pgFail(err))
		}
		// Fail closed even if a dirty run row exists: an external session is
		// never claimable, whatever the queue table says.
		if stateOwner == StateOwnerExternal {
			return fmt.Errorf("kernel: claim scope: %w", ErrNoRunnable)
		}
		state, _, err := sessionLease(ctx, tx, scope)
		if err != nil {
			return err
		}
		if state.Live {
			return fmt.Errorf("kernel: claim scope: %w", ErrLeaseHeld)
		}
		// The mirror on the run row is written under this same session lock, so
		// in a healthy database it agrees with the authority; checking it as well
		// fails closed instead of overwriting a run some writer still holds.
		var mirrorLive bool
		err = tx.QueryRow(ctx,
			`SELECT COALESCE(status = 'running' AND lease_until > now(), false)
			 FROM runs WHERE tenant_id = $1 AND session_id = $2`,
			scope.TenantID, scope.SessionID).Scan(&mirrorLive)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("kernel: claim scope: %w", ErrNoRunnable)
		}
		if err != nil {
			return fmt.Errorf("kernel: claim scope run: %w", pgFail(err))
		}
		if mirrorLive {
			return fmt.Errorf("kernel: claim scope: %w", ErrLeaseHeld)
		}
		lease, err = claimLocked(ctx, tx, scope, owner, ttl)
		if errors.Is(err, errClaimCandidateLost) {
			// Not claimable after all (settled meanwhile).
			return fmt.Errorf("kernel: claim scope: %w", ErrNoRunnable)
		}
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("kernel: claim scope commit: %w", pgFail(err))
		}
		return nil
	}()
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// claimLocked performs the claim inside an open transaction that already holds
// the session row lock: bump the session epoch, then claim the run row under
// the claimable predicate. The predicate is what makes the write safe against
// the candidate snapshot: a run that a concurrent writer (a handoff finish, a
// legacy settle) moved to done/failed — or re-leased — matches nothing, so the
// whole transaction is rolled back and the caller re-scans instead of
// resurrecting a settled run.
func claimLocked(ctx context.Context, tx pgx.Tx, scope Scope, owner string, ttl time.Duration) (Lease, error) {
	var (
		epoch         int64
		until         time.Time
		pendingInput  bool
		readWatermark int64
	)
	// Read the unread-input fact under the session lock, before the guarded run
	// write. The claim only DELIVERS it: clearing it here would lose the input
	// if this attempt is abandoned (drain, crash) before it reads the log — the
	// next claim must still see "there is input you have not proven read".
	// Only a Finish whose read-through watermark covers every input clears it.
	if err := tx.QueryRow(ctx,
		`SELECT pending_input, read_watermark FROM runs
		 WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`,
		scope.TenantID, scope.SessionID).Scan(&pendingInput, &readWatermark); err != nil {
		return Lease{}, fmt.Errorf("kernel: claim pending input: %w", pgFail(err))
	}
	if err := tx.QueryRow(ctx,
		`UPDATE sessions SET owner = $3, owner_epoch = owner_epoch + 1,
		                     lease_until = now() + $4::interval, updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2
		 RETURNING owner_epoch, lease_until`,
		scope.TenantID, scope.SessionID, owner, ttl.String()).Scan(&epoch, &until); err != nil {
		return Lease{}, fmt.Errorf("kernel: claim epoch: %w", pgFail(err))
	}
	// Reclaimed reports whether this claim took over a run that was still
	// running (its lease had expired). The mirror is only written under this
	// session lock, so reading it here is consistent with the guarded write.
	var previous string
	err := tx.QueryRow(ctx,
		`SELECT status FROM runs WHERE tenant_id = $1 AND session_id = $2`,
		scope.TenantID, scope.SessionID).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, errClaimCandidateLost
	}
	if err != nil {
		return Lease{}, fmt.Errorf("kernel: claim previous status: %w", pgFail(err))
	}
	tag, err := tx.Exec(ctx,
		`UPDATE runs SET status = 'running',
		                 attempts = attempts + 1,
		                 owner = $3, owner_epoch = $4, lease_until = $5,
		                 -- A new claim invalidates the previous owner's loadout
		                 -- pin; the new owner writes its own (PinRunLoadout).
		                 loadout_id = NULL, loadout_revision = 0,
		                 loadout_generation = 0, loadout_owner_epoch = 0,
		                 updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2 AND (`+claimablePredicate("")+`)`,
		scope.TenantID, scope.SessionID, owner, epoch, until)
	if err != nil {
		return Lease{}, fmt.Errorf("kernel: claim run: %w", pgFail(err))
	}
	if tag.RowsAffected() == 0 {
		return Lease{}, errClaimCandidateLost
	}
	return Lease{Scope: scope, Token: OwnerToken{Owner: owner, Epoch: epoch}, Until: until,
		Reclaimed: previous == RunStatusRunning, PendingInput: pendingInput,
		ReadWatermark: readWatermark}, nil
}

// RenewLease conditionally extends the lease on owner+epoch and mirrors it.
func (s *PGStore) RenewLease(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	var renewed Lease
	err := s.retryTx(ctx, func() error {
		var err error
		renewed, err = s.renew(ctx, lease, ttl)
		return err
	})
	if err != nil {
		return Lease{}, err
	}
	return renewed, nil
}

func (s *PGStore) renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	var renewed Lease
	err := func() error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("kernel: renew begin: %w", pgFail(err))
		}
		defer tx.Rollback(ctx)
		var until time.Time
		err = tx.QueryRow(ctx,
			`UPDATE sessions SET lease_until = now() + $3::interval, updated_at = now()
			 WHERE tenant_id = $1 AND session_id = $2
			   AND owner = $4 AND owner_epoch = $5 AND lease_until > now()
			 RETURNING lease_until`,
			lease.Scope.TenantID, lease.Scope.SessionID, ttl.String(),
			lease.Token.Owner, lease.Token.Epoch).Scan(&until)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("kernel: renew: %w", s.classifyLeaseFailure(ctx, tx, lease.Scope, lease.Token))
		}
		if err != nil {
			return fmt.Errorf("kernel: renew: %w", pgFail(err))
		}
		if _, err := tx.Exec(ctx,
			`UPDATE runs SET lease_until = $3, updated_at = now()
			 WHERE tenant_id = $1 AND session_id = $2`,
			lease.Scope.TenantID, lease.Scope.SessionID, until); err != nil {
			return fmt.Errorf("kernel: renew run mirror: %w", pgFail(err))
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("kernel: renew commit: %w", pgFail(err))
		}
		renewed = Lease{Scope: lease.Scope, Token: lease.Token, Until: until}
		return nil
	}()
	if err != nil {
		return Lease{}, err
	}
	return renewed, nil
}

// FinishLease settles the run in one transaction: lock the session row, check
// the token against the authority, apply the store's outcome policy, clear the
// lease on both rows.
func (s *PGStore) FinishLease(ctx context.Context, req FinishRequest) error {
	return s.retryTx(ctx, func() error { return s.finish(ctx, req) })
}

func (s *PGStore) finish(ctx context.Context, req FinishRequest) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("kernel: finish begin: %w", pgFail(err))
	}
	defer tx.Rollback(ctx)
	state, ok, err := lockSessionLease(ctx, tx, req.Lease.Scope)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("kernel: finish: %w", ErrNotFound)
	}
	if err := checkToken(state, req.Lease.Token); err != nil {
		return fmt.Errorf("kernel: finish: %w", err)
	}
	var (
		attempts     int
		flagNow      bool
		oldWatermark int64
	)
	err = tx.QueryRow(ctx,
		`SELECT attempts, pending_input, read_watermark FROM runs
		 WHERE tenant_id = $1 AND session_id = $2`,
		req.Lease.Scope.TenantID, req.Lease.Scope.SessionID).Scan(&attempts, &flagNow, &oldWatermark)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("kernel: finish: %w", ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("kernel: finish run read: %w", pgFail(err))
	}
	moreInput := false
	if !req.Cancelled {
		// A cancelled attempt is terminal: the input it did not answer is not a
		// re-queue trigger (see FinishRequest.Cancelled).
		moreInput = unreadInput(ctx, tx, req, flagNow)
	}
	status := finishStatus(req.Err, attempts, req.MaxAttempts, moreInput, req.Cancelled)
	lastError := ""
	if req.Err != nil {
		lastError = req.Err.Error()
	}
	// The attempt's read point is a fact about what it LOADED, so a finish
	// writes it whether or not the attempt succeeded (adversarial review N1): a
	// failed attempt that committed an answer must still hand its watermark to
	// the successor, or the unread input is invisible (the cursor has moved
	// past it). Only a caller that supplies no watermark at all falls back to
	// its cursor, and only on success.
	readWatermark := oldWatermark
	switch {
	case req.ReadThrough > 0:
		readWatermark = req.ReadThrough
	case req.Err == nil:
		readWatermark = req.probeAfter()
	}
	if readWatermark < oldWatermark {
		// The record is monotonic: a delayed or coarse write never rewinds it.
		readWatermark = oldWatermark
	}
	if err := settleLocked(ctx, tx, req.Lease.Scope, status, req.Cursor, lastError, moreInput, readWatermark); err != nil {
		return err
	}
	if err := recordRunLoadoutLocked(ctx, tx, req.Lease.Scope, req.Loadout); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("kernel: finish commit: %w", pgFail(err))
	}
	return nil
}

// recordRunLoadoutLocked writes the loadout identity the run finished under.
// It runs in the same transaction (and under the same session lock) as the
// settle, so the metadata cannot describe a different attempt than the one
// that settled the run. A zero snapshot id/revision is left untouched; the
// epoch is always refreshed.
func recordRunLoadoutLocked(ctx context.Context, tx pgx.Tx, scope Scope, ref RunLoadout) error {
	if _, err := tx.Exec(ctx,
		`UPDATE runs SET loadout_id = COALESCE(NULLIF($3, ''), loadout_id),
		                 loadout_revision = COALESCE(NULLIF($4, 0), loadout_revision),
		                 loadout_generation = COALESCE(NULLIF($5, 0), loadout_generation),
		                 loadout_owner_epoch = $6
		 WHERE tenant_id = $1 AND session_id = $2`,
		scope.TenantID, scope.SessionID, ref.ID, ref.Revision, ref.Generation, ref.OwnerEpoch); err != nil {
		return fmt.Errorf("kernel: record run loadout: %w", pgFail(err))
	}
	return nil
}

// unreadInput decides whether the next attempt must read input this one did
// not:
//
//   - the read-through watermark (FinishRequest.ReadThrough) is the precise read
//     boundary: input with a higher seq was never part of the transcript;
//   - a caller that supplies none falls back to its cursor, the legacy contract
//     — the cursor is the caller's assertion of how far it read, and the worker
//     always supplies the watermark (Runner.RunWithWatermark);
//   - a failed attempt leaves the flag set: the input may be unanswered even
//     though the log holds nothing newer. The next attempt runs under the same
//     generationId dedupe, so an extra turn cannot duplicate a final reply.
func unreadInput(ctx context.Context, q queryer, req FinishRequest, flagNow bool) bool {
	if req.Err != nil {
		return true
	}
	// The consumption fact is authoritative when the log carries it: a guide
	// input consumed inside the run sits above the read watermark but IS
	// answered (an assistant message lists it in inputSeqs), so a watermark-only
	// probe would re-queue a session that owes nothing. A user message no later
	// assistant references is unread — that is exactly the runner's owedTurn
	// rule, evaluated against the committed log. Legacy logs (no inputSeqs
	// anywhere) fall back to the watermark probe by construction: no later
	// assistant references anything, so every input above the watermark counts.
	var more bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM events u
		    WHERE u.tenant_id = $1 AND u.session_id = $2
		      AND u.seq > $3 AND u.kind = $4
		      AND NOT EXISTS (
		          SELECT 1 FROM events a
		          WHERE a.tenant_id = u.tenant_id AND a.session_id = u.session_id
		            AND a.kind = $5 AND a.seq > u.seq
		            AND a.payload->'inputSeqs' @> to_jsonb(u.seq)
		      ))`,
		req.Lease.Scope.TenantID, req.Lease.Scope.SessionID,
		req.probeAfter(), string(KindUserMessage), string(KindAssistantMessage)).Scan(&more); err != nil {
		// The probe failed: keep the flag rather than clear it — an unread
		// input is stranded forever, a redundant turn is not.
		return flagNow
	}
	return more
}

// ReleaseLease gives a claim back without settling the run.
func (s *PGStore) ReleaseLease(ctx context.Context, scope Scope, token OwnerToken) error {
	return s.retryTx(ctx, func() error { return s.release(ctx, scope, token) })
}

func (s *PGStore) release(ctx context.Context, scope Scope, token OwnerToken) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("kernel: release begin: %w", pgFail(err))
	}
	defer tx.Rollback(ctx)
	state, ok, err := lockSessionLease(ctx, tx, scope)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("kernel: release: %w", ErrNotFound)
	}
	if err := checkToken(state, token); err != nil {
		return fmt.Errorf("kernel: release: %w", err)
	}
	// A released run keeps whatever unread input it had: the session is being
	// handed back, not consumed.
	var (
		pendingInput  bool
		readWatermark int64
	)
	if err := tx.QueryRow(ctx,
		`SELECT pending_input, read_watermark FROM runs WHERE tenant_id = $1 AND session_id = $2`,
		scope.TenantID, scope.SessionID).Scan(&pendingInput, &readWatermark); err != nil {
		return fmt.Errorf("kernel: release pending input: %w", pgFail(err))
	}
	if err := settleLocked(ctx, tx, scope, RunStatusPending, 0, "", pendingInput, readWatermark); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("kernel: release commit: %w", pgFail(err))
	}
	return nil
}

// lockSessionLease locks the session row and reads the authority.
func lockSessionLease(ctx context.Context, tx pgx.Tx, scope Scope) (LeaseState, bool, error) {
	var next int64
	err := tx.QueryRow(ctx,
		`SELECT next_seq FROM sessions WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`,
		scope.TenantID, scope.SessionID).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeaseState{}, false, nil
	}
	if err != nil {
		return LeaseState{}, false, fmt.Errorf("kernel: lock session: %w", pgFail(err))
	}
	return sessionLease(ctx, tx, scope)
}

// settleLocked clears the lease on both rows and settles the run state.
// pendingInput is the settled unread-input flag: a run that goes back to
// pending with unread input hands that fact to the next claim.
func settleLocked(ctx context.Context, tx pgx.Tx, scope Scope, status string, cursor int64, lastError string, pendingInput bool, readWatermark int64) error {
	if _, err := tx.Exec(ctx,
		`UPDATE runs SET status = $3, cursor_seq = GREATEST(cursor_seq, $4),
		                 last_error = NULLIF($5, ''), lease_until = NULL,
		                 owner = NULL, owner_epoch = 0, pending_input = $6,
		                 read_watermark = $7, updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2`,
		scope.TenantID, scope.SessionID, status, cursor, lastError, pendingInput, readWatermark); err != nil {
		return fmt.Errorf("kernel: settle run: %w", pgFail(err))
	}
	if _, err := tx.Exec(ctx,
		`UPDATE sessions SET lease_until = NULL, updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2`,
		scope.TenantID, scope.SessionID); err != nil {
		return fmt.Errorf("kernel: clear lease: %w", pgFail(err))
	}
	return nil
}

// FindLease reads the session's fencing state.
func (s *PGStore) FindLease(ctx context.Context, scope Scope) (LeaseState, error) {
	state, ok, err := sessionLease(ctx, s.pool, scope)
	if err != nil {
		return LeaseState{}, err
	}
	if !ok {
		return LeaseState{}, nil
	}
	return state, nil
}

// classifyLeaseFailure explains why a guarded update matched no row.
func (s *PGStore) classifyLeaseFailure(ctx context.Context, q queryer, scope Scope, token OwnerToken) error {
	state, ok, err := sessionLease(ctx, q, scope)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return checkToken(state, token)
}
