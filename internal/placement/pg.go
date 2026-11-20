// PostgreSQL placement store. Every promise the spec makes about placement is
// enforced here, in one transaction per operation:
//
//   - a lease claim is a compare-and-set on the stored epoch plus a check of
//     lease_until, so two groups racing for one session produce exactly one
//     winner and a typed Conflict (409) for the loser;
//   - a run admission counts group slots and the tenant's runs in the same
//     transaction that reserves the slot, under a per-tenant advisory lock, so
//     two workers (even in different groups) cannot oversell either limit.
//
// The store uses the operator pool and every statement carries tenant_id; RLS
// remains the second line of defence (the tables are in rlsTenantTables).
package placement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the production Store.
type PGStore struct {
	pool      *pgxpool.Pool
	txRetries int
}

// NewPGStore wraps a pgx pool. Callers open and migrate the database through
// internal/store first (schema.sql owns the placement DDL).
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool, txRetries: 4}
}

var _ Store = (*PGStore)(nil)

func (s *PGStore) CreateGroup(ctx context.Context, spec GroupSpec) (Group, error) {
	if err := validateGroupSpec(spec); err != nil {
		return Group{}, err
	}
	var group Group
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		created, err := scanGroup(tx.QueryRow(ctx,
			`INSERT INTO groups (tenant_id, group_id, name, max_concurrent_runs, memory_limit_bytes)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING tenant_id, group_id, name, max_concurrent_runs, memory_limit_bytes,
			           status, created_at, updated_at`,
			spec.TenantID, spec.GroupID, spec.Name, spec.MaxConcurrentRuns, spec.MemoryLimitBytes))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return fmt.Errorf("%w: %s", ErrGroupExists, spec.Name)
			}
			return fmt.Errorf("placement: create group: %w", err)
		}
		group = created
		return nil
	})
	if err != nil {
		return Group{}, err
	}
	return group, nil
}

func (s *PGStore) EnsureGroup(ctx context.Context, spec GroupSpec) (Group, error) {
	if err := validateGroupSpec(spec); err != nil {
		return Group{}, err
	}
	var group Group
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO groups (tenant_id, group_id, name, max_concurrent_runs, memory_limit_bytes)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (tenant_id, name) DO NOTHING
			 RETURNING tenant_id, group_id, name, max_concurrent_runs, memory_limit_bytes,
			           status, created_at, updated_at`,
			spec.TenantID, spec.GroupID, spec.Name, spec.MaxConcurrentRuns, spec.MemoryLimitBytes)
		var err error
		group, err = scanGroup(row)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Another process created it (or it existed before). A new statement
		// gets a fresh snapshot under READ COMMITTED, so it sees the winner.
		group, err = scanGroup(tx.QueryRow(ctx, groupSelect+` WHERE tenant_id = $1 AND name = $2`,
			spec.TenantID, spec.Name))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("placement: ensure group: %w", ErrGroupNotFound)
		}
		return err
	})
	if err != nil {
		return Group{}, err
	}
	return group, nil
}

const groupSelect = `SELECT tenant_id, group_id, name, max_concurrent_runs, memory_limit_bytes,
                             status, created_at, updated_at
                        FROM groups`

func scanGroup(row pgx.Row) (Group, error) {
	var group Group
	if err := row.Scan(&group.TenantID, &group.GroupID, &group.Name, &group.MaxConcurrentRuns,
		&group.MemoryLimitBytes, &group.Status, &group.CreatedAt, &group.UpdatedAt); err != nil {
		return Group{}, err
	}
	return group, nil
}

func (s *PGStore) GetGroup(ctx context.Context, tenantID, groupID uuid.UUID) (Group, error) {
	group, err := scanGroup(s.pool.QueryRow(ctx, groupSelect+` WHERE tenant_id = $1 AND group_id = $2`,
		tenantID, groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Group{}, fmt.Errorf("placement: get group: %w", ErrGroupNotFound)
	}
	if err != nil {
		return Group{}, fmt.Errorf("placement: get group: %w", err)
	}
	return group, nil
}

func (s *PGStore) ListGroups(ctx context.Context, tenantID uuid.UUID) ([]Group, error) {
	rows, err := s.pool.Query(ctx, groupSelect+` WHERE tenant_id = $1 ORDER BY name`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("placement: list groups: %w", err)
	}
	defer rows.Close()
	var groups []Group
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("placement: list groups: %w", err)
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *PGStore) SetGroupStatus(ctx context.Context, tenantID, groupID uuid.UUID, status string) (Group, error) {
	if err := ValidateGroupStatus(status); err != nil {
		return Group{}, err
	}
	var group Group
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		updated, err := scanGroup(tx.QueryRow(ctx,
			`UPDATE groups SET status = $3, updated_at = now()
			  WHERE tenant_id = $1 AND group_id = $2 AND status <> 'retired'
			  RETURNING tenant_id, group_id, name, max_concurrent_runs, memory_limit_bytes,
			            status, created_at, updated_at`,
			tenantID, groupID, status))
		if errors.Is(err, pgx.ErrNoRows) {
			if _, getErr := scanGroup(tx.QueryRow(ctx, groupSelect+`
				 WHERE tenant_id = $1 AND group_id = $2`, tenantID, groupID)); getErr != nil {
				if errors.Is(getErr, pgx.ErrNoRows) {
					return fmt.Errorf("placement: get group: %w", ErrGroupNotFound)
				}
				return getErr
			}
			return &Conflict{
				Reason: ReasonGroupUnavailable, TenantID: tenantID, GroupID: groupID,
				Detail: fmt.Sprintf("retired group cannot become %q", status),
			}
		}
		if err != nil {
			return fmt.Errorf("placement: set group status: %w", err)
		}
		group = updated
		return nil
	})
	if err != nil {
		return Group{}, err
	}
	return group, nil
}

const placementSelect = `SELECT tenant_id, session_id, group_id, worker_id, epoch, lease_until, updated_at
                           FROM session_placement`

func scanPlacement(row pgx.Row) (Placement, error) {
	var placement Placement
	if err := row.Scan(&placement.TenantID, &placement.SessionID, &placement.GroupID,
		&placement.WorkerID, &placement.Epoch, &placement.LeaseUntil, &placement.UpdatedAt); err != nil {
		return Placement{}, err
	}
	return placement, nil
}

func (s *PGStore) GetPlacement(ctx context.Context, tenantID, sessionID uuid.UUID) (Placement, error) {
	placement, err := scanPlacement(s.pool.QueryRow(ctx, placementSelect+`
		 WHERE tenant_id = $1 AND session_id = $2`, tenantID, sessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Placement{}, fmt.Errorf("placement: get placement: %w", ErrPlacementNotFound)
	}
	if err != nil {
		return Placement{}, fmt.Errorf("placement: get placement: %w", err)
	}
	return placement, nil
}

func (s *PGStore) ClaimPlacement(ctx context.Context, req claimWrite) (Placement, error) {
	ttl := req.ttl
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	var out Placement
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var (
			current Placement
			live    bool
		)
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, session_id, group_id, worker_id, epoch, lease_until, updated_at,
			        lease_until > now()
			   FROM session_placement
			  WHERE tenant_id = $1 AND session_id = $2
			  FOR UPDATE`, req.tenantID, req.sessionID).
			Scan(&current.TenantID, &current.SessionID, &current.GroupID, &current.WorkerID,
				&current.Epoch, &current.LeaseUntil, &current.UpdatedAt, &live)
		found := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("placement: lock placement: %w", err)
		}
		if !found && req.expectedEpoch != 0 {
			return &Conflict{
				Reason: ReasonEpochMismatch, TenantID: req.tenantID, SessionID: req.sessionID,
				ExpectedEpoch: req.expectedEpoch, Detail: "session has no placement",
			}
		}
		if found && current.Epoch != req.expectedEpoch {
			return &Conflict{
				Reason: ReasonEpochMismatch, TenantID: req.tenantID, SessionID: req.sessionID,
				GroupID: current.GroupID, ExpectedEpoch: req.expectedEpoch,
				CurrentEpoch: current.Epoch, Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
			}
		}

		group := current.GroupID
		epoch := int64(1)
		if found {
			epoch = current.Epoch + 1
			if req.groupID != nil && *req.groupID != current.GroupID {
				if live {
					return &Conflict{
						Reason: req.moveReason, TenantID: req.tenantID, SessionID: req.sessionID,
						GroupID: current.GroupID, ExpectedEpoch: req.expectedEpoch,
						CurrentEpoch: current.Epoch, Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
						Detail: "live placement in another group must drain or expire before a move",
					}
				}
				group = *req.groupID
			}
			if live && current.WorkerID != req.workerID {
				return &Conflict{
					Reason: ReasonLeaseHeld, TenantID: req.tenantID, SessionID: req.sessionID,
					GroupID: current.GroupID, ExpectedEpoch: req.expectedEpoch,
					CurrentEpoch: current.Epoch, Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
				}
			}
		} else {
			if req.groupID == nil {
				return &Conflict{
					Reason: ReasonGroupUnavailable, TenantID: req.tenantID, SessionID: req.sessionID,
					Detail: "no group resolved for an unplaced session",
				}
			}
			group = *req.groupID
		}
		if err := checkGroupTx(ctx, tx, req.tenantID, group, found && group == current.GroupID); err != nil {
			return err
		}

		if !found {
			out, err = scanPlacement(tx.QueryRow(ctx,
				`INSERT INTO session_placement
				        (tenant_id, session_id, group_id, worker_id, epoch, lease_until, updated_at)
				 VALUES ($1, $2, $3, $4, $5, now() + $6::interval, now())
				 RETURNING tenant_id, session_id, group_id, worker_id, epoch, lease_until, updated_at`,
				req.tenantID, req.sessionID, group, req.workerID, epoch, ttl.String()))
			if err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					// A concurrent claim inserted between our snapshot and INSERT.
					return &Conflict{
						Reason: ReasonEpochMismatch, TenantID: req.tenantID, SessionID: req.sessionID,
						ExpectedEpoch: req.expectedEpoch, CurrentEpoch: 1,
						Detail: "a concurrent claim created the placement",
					}
				}
				return fmt.Errorf("placement: insert placement: %w", err)
			}
			return nil
		}

		// The write carries both fences: the epoch guard and the lease state.
		// The Go check above produces the typed reason; this predicate is the
		// defence in depth that keeps a policy drift from stealing a live lease.
		tag, err := tx.Exec(ctx,
			`UPDATE session_placement
			    SET group_id = $3, worker_id = $4, epoch = $5,
			        lease_until = now() + $6::interval, updated_at = now()
			  WHERE tenant_id = $1 AND session_id = $2 AND epoch = $7
			    AND (lease_until <= now() OR worker_id = $4)`,
			req.tenantID, req.sessionID, group, req.workerID, epoch, ttl.String(), req.expectedEpoch)
		if err != nil {
			return fmt.Errorf("placement: claim placement: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Defensive: the row was locked, so this only happens if a writer
			// escaped the lock protocol. Fail as a stale claim, never retry.
			return &Conflict{
				Reason: ReasonEpochMismatch, TenantID: req.tenantID, SessionID: req.sessionID,
				GroupID: current.GroupID, ExpectedEpoch: req.expectedEpoch,
				CurrentEpoch: current.Epoch, Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
			}
		}
		out, err = scanPlacement(tx.QueryRow(ctx, placementSelect+`
			 WHERE tenant_id = $1 AND session_id = $2`, req.tenantID, req.sessionID))
		if err != nil {
			return fmt.Errorf("placement: read claimed placement: %w", err)
		}
		return nil
	})
	if err != nil {
		return Placement{}, err
	}
	return out, nil
}

func (s *PGStore) AdoptPlacement(ctx context.Context, req adoptWrite) (Placement, error) {
	ttl := req.ttl
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	var out Placement
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var (
			current Placement
			live    bool
		)
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, session_id, group_id, worker_id, epoch, lease_until, updated_at,
			        lease_until > now()
			   FROM session_placement
			  WHERE tenant_id = $1 AND session_id = $2
			  FOR UPDATE`, req.tenantID, req.sessionID).
			Scan(&current.TenantID, &current.SessionID, &current.GroupID, &current.WorkerID,
				&current.Epoch, &current.LeaseUntil, &current.UpdatedAt, &live)
		found := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("placement: lock placement: %w", err)
		}

		group := current.GroupID
		if found {
			if req.epoch < current.Epoch {
				return &Conflict{
					Reason: ReasonEpochMismatch, TenantID: req.tenantID, SessionID: req.sessionID,
					GroupID: current.GroupID, ExpectedEpoch: req.epoch, CurrentEpoch: current.Epoch,
					Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
					Detail: "kernel epoch is older than the stored placement",
				}
			}
			if req.groupID != nil && *req.groupID != current.GroupID {
				if live {
					return &Conflict{
						Reason: ReasonLeaseHeld, TenantID: req.tenantID, SessionID: req.sessionID,
						GroupID: current.GroupID, ExpectedEpoch: req.epoch, CurrentEpoch: current.Epoch,
						Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
						Detail: "live placement in another group must drain or expire before a move",
					}
				}
				group = *req.groupID
			}
			if live && current.WorkerID != req.workerID {
				return &Conflict{
					Reason: ReasonLeaseHeld, TenantID: req.tenantID, SessionID: req.sessionID,
					GroupID: current.GroupID, ExpectedEpoch: req.epoch, CurrentEpoch: current.Epoch,
					Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
				}
			}
		} else {
			if req.epoch <= 0 {
				return &Conflict{
					Reason: ReasonEpochMismatch, TenantID: req.tenantID, SessionID: req.sessionID,
					ExpectedEpoch: req.epoch, Detail: "adopt requires a positive kernel epoch",
				}
			}
			if req.groupID == nil {
				return &Conflict{
					Reason: ReasonGroupUnavailable, TenantID: req.tenantID, SessionID: req.sessionID,
					Detail: "no group resolved for an unplaced session",
				}
			}
			group = *req.groupID
		}
		if err := checkGroupTx(ctx, tx, req.tenantID, group, found && group == current.GroupID); err != nil {
			return err
		}

		if !found {
			out, err = scanPlacement(tx.QueryRow(ctx,
				`INSERT INTO session_placement
				        (tenant_id, session_id, group_id, worker_id, epoch, lease_until, updated_at)
				 VALUES ($1, $2, $3, $4, $5, now() + $6::interval, now())
				 RETURNING tenant_id, session_id, group_id, worker_id, epoch, lease_until, updated_at`,
				req.tenantID, req.sessionID, group, req.workerID, req.epoch, ttl.String()))
			if err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					return &Conflict{
						Reason: ReasonEpochMismatch, TenantID: req.tenantID, SessionID: req.sessionID,
						ExpectedEpoch: req.epoch, Detail: "a concurrent claim created the placement",
					}
				}
				return fmt.Errorf("placement: insert adopted placement: %w", err)
			}
			return nil
		}

		tag, err := tx.Exec(ctx,
			`UPDATE session_placement
			    SET group_id = $3, worker_id = $4, epoch = $5,
			        lease_until = now() + $6::interval, updated_at = now()
			  WHERE tenant_id = $1 AND session_id = $2 AND epoch = $7
			    AND (lease_until <= now() OR worker_id = $4)`,
			req.tenantID, req.sessionID, group, req.workerID, req.epoch, ttl.String(), current.Epoch)
		if err != nil {
			return fmt.Errorf("placement: adopt placement: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return &Conflict{
				Reason: ReasonEpochMismatch, TenantID: req.tenantID, SessionID: req.sessionID,
				GroupID: current.GroupID, ExpectedEpoch: req.epoch,
				CurrentEpoch: current.Epoch, Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
			}
		}
		out, err = scanPlacement(tx.QueryRow(ctx, placementSelect+`
			 WHERE tenant_id = $1 AND session_id = $2`, req.tenantID, req.sessionID))
		if err != nil {
			return fmt.Errorf("placement: read adopted placement: %w", err)
		}
		return nil
	})
	if err != nil {
		return Placement{}, err
	}
	return out, nil
}

func (s *PGStore) ReleasePlacement(ctx context.Context, tenantID, sessionID uuid.UUID, workerID string, expectedEpoch int64) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM session_placement
			  WHERE tenant_id = $1 AND session_id = $2 AND epoch = $3
			    AND ($4 = '' OR worker_id = $4)`,
			tenantID, sessionID, expectedEpoch, workerID)
		if err != nil {
			return fmt.Errorf("placement: release placement: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		current, err := scanPlacement(tx.QueryRow(ctx, placementSelect+`
			 WHERE tenant_id = $1 AND session_id = $2`, tenantID, sessionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("placement: get placement: %w", ErrPlacementNotFound)
		}
		if err != nil {
			return fmt.Errorf("placement: read left placement: %w", err)
		}
		return &Conflict{
			Reason: ReasonEpochMismatch, TenantID: tenantID, SessionID: sessionID, GroupID: current.GroupID,
			ExpectedEpoch: expectedEpoch, CurrentEpoch: current.Epoch, Holder: current.WorkerID,
			LeaseUntil: current.LeaseUntil,
		}
	})
}

func (s *PGStore) AdmitRun(ctx context.Context, req admitWrite) (Admission, error) {
	var out Admission
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// One advisory lock per tenant serialises admissions across every group
		// of that tenant: the tenant quota is a cross-group sum, so checking it
		// without serialisation would let two groups each pass their own check.
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"keel-placement-admission:"+req.tenantID.String()); err != nil {
			return fmt.Errorf("placement: admission lock: %w", err)
		}
		current, err := scanPlacement(tx.QueryRow(ctx, placementSelect+`
			 WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`, req.tenantID, req.sessionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return &Conflict{Reason: ReasonPlacementNotFound, TenantID: req.tenantID, SessionID: req.sessionID}
		}
		if err != nil {
			return fmt.Errorf("placement: lock placement for admission: %w", err)
		}
		if req.groupID != nil && *req.groupID != current.GroupID {
			return &Conflict{
				Reason: ReasonAlreadyPlaced, TenantID: req.tenantID, SessionID: req.sessionID,
				GroupID: current.GroupID,
				Detail:  "run admission names a group other than the session's placement",
			}
		}
		group, err := scanGroup(tx.QueryRow(ctx, groupSelect+`
			 WHERE tenant_id = $1 AND group_id = $2`, req.tenantID, current.GroupID))
		if errors.Is(err, pgx.ErrNoRows) {
			return &Conflict{Reason: ReasonGroupUnavailable, TenantID: req.tenantID,
				SessionID: req.sessionID, GroupID: current.GroupID}
		}
		if err != nil {
			return fmt.Errorf("placement: read admission group: %w", err)
		}
		if group.Status == StatusRetired {
			return &Conflict{Reason: ReasonGroupUnavailable, TenantID: req.tenantID,
				SessionID: req.sessionID, GroupID: group.GroupID}
		}
		if group.Status == StatusDraining {
			return &Conflict{Reason: ReasonGroupDraining, TenantID: req.tenantID,
				SessionID: req.sessionID, GroupID: group.GroupID,
				Detail: "group is draining and takes no new runs"}
		}

		var selfStatus string
		err = tx.QueryRow(ctx,
			`SELECT status FROM runs
			  WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`,
			req.tenantID, req.sessionID).Scan(&selfStatus)
		selfActive := false
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("placement: read self run: %w", err)
		}
		if err == nil {
			selfActive = selfStatus == RunStatusPending || selfStatus == RunStatusRunning
		}

		var activeGroup, activeTenant int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM runs
			  WHERE tenant_id = $1 AND group_id = $2 AND status IN ('pending', 'running')`,
			req.tenantID, group.GroupID).Scan(&activeGroup); err != nil {
			return fmt.Errorf("placement: count group runs: %w", err)
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM runs
			  WHERE tenant_id = $1 AND status IN ('pending', 'running')`,
			req.tenantID).Scan(&activeTenant); err != nil {
			return fmt.Errorf("placement: count tenant runs: %w", err)
		}

		if !selfActive {
			if group.MaxConcurrentRuns > 0 && activeGroup >= group.MaxConcurrentRuns {
				return s.capacityErrorTx(ctx, tx, req, group.GroupID, CapacityGroup,
					group.MaxConcurrentRuns, activeGroup, true)
			}
			if req.tenantLimit > 0 && activeTenant >= req.tenantLimit {
				return s.capacityErrorTx(ctx, tx, req, group.GroupID, CapacityTenant,
					req.tenantLimit, activeTenant, false)
			}
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO runs (tenant_id, session_id, status, group_id)
			 VALUES ($1, $2, 'pending', $3)
			 ON CONFLICT (tenant_id, session_id) DO UPDATE
			 SET group_id = CASE WHEN runs.status = 'running' THEN runs.group_id ELSE EXCLUDED.group_id END,
			     status = CASE WHEN runs.status = 'running' THEN 'running' ELSE 'pending' END,
			     updated_at = now()`,
			req.tenantID, req.sessionID, group.GroupID); err != nil {
			return fmt.Errorf("placement: reserve run slot: %w", err)
		}
		if !selfActive {
			activeGroup++
			activeTenant++
		}
		out = Admission{
			TenantID:      req.tenantID,
			SessionID:     req.sessionID,
			GroupID:       group.GroupID,
			ActiveGroup:   activeGroup,
			LimitGroup:    group.MaxConcurrentRuns,
			ActiveTenant:  activeTenant,
			LimitTenant:   req.tenantLimit,
			AlreadyActive: selfActive,
		}
		return nil
	})
	if err != nil {
		return Admission{}, err
	}
	return out, nil
}

// capacityErrorTx builds the typed 429. RetryAfter comes from the nearest
// running lease in the refused scope (at least DefaultRetryAfter), so a client
// retry is explainable rather than a blind backoff.
func (s *PGStore) capacityErrorTx(ctx context.Context, tx pgx.Tx, req admitWrite, groupID uuid.UUID, reason CapacityReason, limit, active int, byGroup bool) error {
	query := `SELECT COALESCE(min(lease_until) - now(), interval '0')
	            FROM runs
	           WHERE tenant_id = $1 AND status = 'running' AND lease_until IS NOT NULL`
	args := []any{req.tenantID}
	if byGroup {
		query += ` AND group_id = $2`
		args = append(args, groupID)
	}
	var wait time.Duration
	if err := tx.QueryRow(ctx, query, args...).Scan(&wait); err != nil {
		wait = 0
	}
	if wait < DefaultRetryAfter {
		wait = DefaultRetryAfter
	}
	return &CapacityError{
		Reason:     reason,
		Resource:   capacityResource(reason),
		TenantID:   req.tenantID,
		SessionID:  req.sessionID,
		GroupID:    groupID,
		Limit:      limit,
		Active:     active,
		RetryAfter: wait,
	}
}

func (s *PGStore) ReleaseRun(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE runs SET status = 'done', lease_until = NULL, updated_at = now()
			  WHERE tenant_id = $1 AND session_id = $2 AND status = 'pending'`,
			tenantID, sessionID); err != nil {
			return fmt.Errorf("placement: release run: %w", err)
		}
		return nil
	})
}

// checkGroupTx refuses a group that cannot take the write. Retired and missing
// are the same refusal to a caller (there is no target); draining is distinct
// because the group will come back after its sessions finish.
func checkGroupTx(ctx context.Context, tx pgx.Tx, tenantID, groupID uuid.UUID, allowDraining bool) error {
	var status string
	err := tx.QueryRow(ctx,
		`SELECT status FROM groups WHERE tenant_id = $1 AND group_id = $2`,
		tenantID, groupID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || status == StatusRetired {
		return &Conflict{Reason: ReasonGroupUnavailable, TenantID: tenantID, GroupID: groupID}
	}
	if err != nil {
		return fmt.Errorf("placement: read group status: %w", err)
	}
	if status == StatusDraining && !allowDraining {
		return &Conflict{Reason: ReasonGroupDraining, TenantID: tenantID, GroupID: groupID,
			Detail: "group is draining and takes no new placements"}
	}
	return nil
}

// inTx runs fn in a transaction, retrying only the transient concurrency
// failures (deadlock, serialization, lock timeout). Typed conflicts are
// decisions, not transient errors: they return immediately.
func (s *PGStore) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	attempts := s.txRetries
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("placement: begin: %w", err)
		}
		err = fn(tx)
		if err == nil {
			if err := tx.Commit(ctx); err == nil {
				return nil
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
			_ = tx.Rollback(ctx)
		}
		if !isTransientTx(lastErr) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(10+attempt*25) * time.Millisecond):
		}
	}
	return lastErr
}

func isTransientTx(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40P01", "40001", "55P03":
			return true
		}
	}
	return false
}
