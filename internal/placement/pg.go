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

// lockPlacementTenant serialises admissions and promotions for one tenant.
// The tenant quota is a cross-group sum and promotion must happen exactly once,
// so both paths take this one transaction-scoped advisory lock.
//
// The lock wait is bounded by lock_timeout instead of the connection's
// statement_timeout: a burst of admissions queues on this lock, and a queued
// waiter must be retried as a transient contention (55P03), not reported as a
// client error by the 5s statement ceiling.
func lockPlacementTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '1s'`); err != nil {
		return fmt.Errorf("placement: admission lock timeout: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"keel-placement-admission:"+tenantID.String()); err != nil {
		return fmt.Errorf("placement: admission lock: %w", err)
	}
	return nil
}

func (s *PGStore) AdmitRun(ctx context.Context, req admitWrite) (Admission, error) {
	var out Admission
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// One advisory lock per tenant serialises admissions and promotions
		// across every group of that tenant: the tenant quota is a cross-group
		// sum, and promotion must not race another promoter for one queue head.
		if err := lockPlacementTenant(ctx, tx, req.tenantID); err != nil {
			return err
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

		// A run row may already exist for the session. "Already active" alone is
		// not the admission fact: the row's group_id is. A pending row written by
		// the kernel's edge wake (or a pre-placement queue path) has a NULL
		// group_id; treating it as admitted would let it bypass every group slot.
		// A queued row is waiting, not active: it holds no slot.
		var (
			selfStatus string
			selfGroup  uuid.UUID
			selfHasRun bool
		)
		err = tx.QueryRow(ctx,
			`SELECT status, COALESCE(group_id, '00000000-0000-0000-0000-000000000000'::uuid)
			   FROM runs
			  WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`,
			req.tenantID, req.sessionID).
			Scan(&selfStatus, &selfGroup)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("placement: read self run: %w", err)
		}
		selfHasRun = err == nil
		selfActive := selfHasRun && runActive(selfStatus)
		selfQueued := selfHasRun && selfStatus == RunStatusQueued
		selfAdmitted := selfActive && selfGroup == group.GroupID
		if selfActive && !selfAdmitted && selfStatus == RunStatusRunning {
			// A running attempt cannot be re-homed: the placement move requires
			// the old work to drain first. Refusing is safer than moving a live
			// run's slot behind its back.
			return &Conflict{
				Reason: ReasonAlreadyPlaced, TenantID: req.tenantID, SessionID: req.sessionID,
				GroupID: selfGroup, Detail: "the session is already running in another group",
			}
		}

		activeGroup, activeTenant, usedMemory, err := activeCountsTx(ctx, tx, req.tenantID, group.GroupID)
		if err != nil {
			return err
		}

		if !selfAdmitted && !selfQueued {
			// The tenant quota is a hard refusal, never a queue bypass. A queued
			// row's new input is not a new admission: it already has its place, so
			// it is not re-charged (or refused) here.
			if !selfActive && req.tenantLimit > 0 && activeTenant >= req.tenantLimit {
				return s.capacityErrorTx(ctx, tx, req, group.GroupID, CapacityTenant,
					req.tenantLimit, activeTenant, false)
			}
			// Memory is a hard ceiling too. An unknown estimate is a typed
			// unsupported failure, not a zero: a fabricated number would make the
			// ceiling look enforced.
			if group.MemoryLimitBytes > 0 {
				if !req.memoryKnown || req.memoryEstimate <= 0 {
					return &UnsupportedError{
						Reason: ReasonMemoryEstimateUnavailable, TenantID: req.tenantID,
						SessionID: req.sessionID, GroupID: group.GroupID,
						Detail: "the group defines a memory budget but no memory estimate is configured",
					}
				}
				if usedMemory+req.memoryEstimate > group.MemoryLimitBytes {
					return s.memoryErrorTx(ctx, tx, req, group.GroupID, usedMemory, activeGroup)
				}
			}
			if group.MaxConcurrentRuns > 0 && activeGroup >= group.MaxConcurrentRuns {
				// Full group: the queued row is the durable FIFO entry. It holds no
				// slot, the kernel's claim predicate never matches it, and only a
				// promotion (guarded queued -> pending) can start it. An existing
				// queued row keeps its queue_seq (COALESCE), so a repeated input
				// does not jump the line.
				if _, err := tx.Exec(ctx,
					`INSERT INTO runs (tenant_id, session_id, status, group_id, queue_seq, memory_bytes)
					 VALUES ($1, $2, 'queued', $3, nextval('runs_queue_seq'), $4)
					 ON CONFLICT (tenant_id, session_id) DO UPDATE
					 SET status = 'queued',
					     group_id = EXCLUDED.group_id,
					     queue_seq = COALESCE(runs.queue_seq, EXCLUDED.queue_seq),
					     memory_bytes = EXCLUDED.memory_bytes,
					     updated_at = now()`,
					req.tenantID, req.sessionID, group.GroupID, req.memoryEstimate); err != nil {
					return fmt.Errorf("placement: enqueue run: %w", err)
				}
				var position int
				if err := tx.QueryRow(ctx, queuePositionQuery, req.tenantID, group.GroupID, req.sessionID).
					Scan(&position); err != nil {
					return fmt.Errorf("placement: read queue position: %w", err)
				}
				eta, err := queueETATx(ctx, tx, req.tenantID, group.GroupID, position, group.MaxConcurrentRuns)
				if err != nil {
					return err
				}
				out = Admission{
					TenantID:      req.tenantID,
					SessionID:     req.sessionID,
					GroupID:       group.GroupID,
					ActiveGroup:   activeGroup,
					LimitGroup:    group.MaxConcurrentRuns,
					ActiveTenant:  activeTenant,
					LimitTenant:   req.tenantLimit,
					Created:       !selfActive,
					Queued:        true,
					QueuePosition: position,
					ETA:           eta,
					MemoryBytes:   req.memoryEstimate,
				}
				return nil
			}
		}

		if selfQueued && selfGroup != group.GroupID {
			// The placement moved while the run waited (an expired lease was
			// re-claimed elsewhere). The queued row follows the placement and joins
			// the target group's queue at its tail: queue positions are meaningful
			// only inside one group.
			// The estimate is resolved for the *target* group: a row queued in
			// an unbounded group carries 0, and promotion in a memory-bounded
			// target would otherwise refuse it forever at the head of the queue.
			if _, err := tx.Exec(ctx,
				`UPDATE runs
				    SET group_id = $3, queue_seq = nextval('runs_queue_seq'),
				        memory_bytes = $4, updated_at = now()
				  WHERE tenant_id = $1 AND session_id = $2 AND status = 'queued'`,
				req.tenantID, req.sessionID, group.GroupID, req.memoryEstimate); err != nil {
				return fmt.Errorf("placement: re-home queued run: %w", err)
			}
			selfGroup = group.GroupID
		}
		if !selfQueued {
			if _, err := tx.Exec(ctx,
				`INSERT INTO runs (tenant_id, session_id, status, group_id, queue_seq, memory_bytes)
				 VALUES ($1, $2, 'pending', $3, NULL, $4)
				 ON CONFLICT (tenant_id, session_id) DO UPDATE
				 SET group_id = CASE WHEN runs.status = 'running' THEN runs.group_id ELSE EXCLUDED.group_id END,
				     status = CASE WHEN runs.status = 'running' THEN 'running' ELSE 'pending' END,
				     queue_seq = NULL,
				     memory_bytes = CASE WHEN runs.status = 'running' THEN runs.memory_bytes ELSE EXCLUDED.memory_bytes END,
				     updated_at = now()`,
				req.tenantID, req.sessionID, group.GroupID, req.memoryEstimate); err != nil {
				return fmt.Errorf("placement: reserve run slot: %w", err)
			}
		}
		// Capacity is available: promote FIFO heads while slots, memory and the
		// tenant quota permit. A queued row is never promoted here directly, so a
		// repeated input cannot jump ahead of an earlier queued run; the sweep may
		// promote this session's own row when it reaches the head, which is why the
		// final state is re-read.
		if _, err := s.promoteQueuedTx(ctx, tx, req.tenantID, req.tenantLimit); err != nil {
			return err
		}
		var finalStatus string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM runs
			  WHERE tenant_id = $1 AND session_id = $2`, req.tenantID, req.sessionID).
			Scan(&finalStatus); err != nil {
			return fmt.Errorf("placement: read admitted run: %w", err)
		}
		activeGroup, activeTenant, _, err = activeCountsTx(ctx, tx, req.tenantID, group.GroupID)
		if err != nil {
			return err
		}
		if finalStatus == RunStatusQueued {
			position := 0
			if err := tx.QueryRow(ctx, queuePositionQuery, req.tenantID, group.GroupID, req.sessionID).
				Scan(&position); err != nil {
				return fmt.Errorf("placement: read queue position: %w", err)
			}
			eta, err := queueETATx(ctx, tx, req.tenantID, group.GroupID, position, group.MaxConcurrentRuns)
			if err != nil {
				return err
			}
			out = Admission{
				TenantID:      req.tenantID,
				SessionID:     req.sessionID,
				GroupID:       group.GroupID,
				ActiveGroup:   activeGroup,
				LimitGroup:    group.MaxConcurrentRuns,
				ActiveTenant:  activeTenant,
				LimitTenant:   req.tenantLimit,
				Created:       !selfActive && !selfQueued,
				Queued:        true,
				QueuePosition: position,
				ETA:           eta,
				MemoryBytes:   req.memoryEstimate,
			}
			return nil
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
			Created:       !selfActive && !selfQueued,
			MemoryBytes:   req.memoryEstimate,
		}
		return nil
	})
	if err != nil {
		return Admission{}, err
	}
	return out, nil
}

// activeCountsTx reports (activeGroup, activeTenant, usedMemory) for one group.
// Active means pending+running: a queued row waits without holding a slot.
func activeCountsTx(ctx context.Context, tx pgx.Tx, tenantID, groupID uuid.UUID) (int, int, int64, error) {
	var (
		activeTenant int
		activeGroup  int
		usedMemory   int64
	)
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status IN ('pending', 'running')),
		        count(*) FILTER (WHERE status IN ('pending', 'running') AND group_id = $2),
		        COALESCE(sum(memory_bytes) FILTER (WHERE status IN ('pending', 'running') AND group_id = $2), 0)
		   FROM runs WHERE tenant_id = $1`,
		tenantID, groupID).Scan(&activeTenant, &activeGroup, &usedMemory); err != nil {
		return 0, 0, 0, fmt.Errorf("placement: count active runs: %w", err)
	}
	return activeGroup, activeTenant, usedMemory, nil
}

// queuePositionQuery is the 1-based FIFO position of one session inside its
// group (0 when the session has no queued row).
const queuePositionQuery = `SELECT count(*) FROM runs
	 WHERE tenant_id = $1 AND group_id = $2 AND status = 'queued'
	   AND queue_seq <= COALESCE(
	       (SELECT queue_seq FROM runs WHERE tenant_id = $1 AND session_id = $3), 0)`

// queueETATx builds the explainable wait from observed attempts. With no
// history it returns QueueETA{Known:false}: unknown, never a fabricated zero.
func queueETATx(ctx context.Context, tx pgx.Tx, tenantID, groupID uuid.UUID, position, limit int) (QueueETA, error) {
	avg, samples, basis, err := averageDurationTx(ctx, tx, tenantID, groupID)
	if err != nil {
		return QueueETA{}, err
	}
	if samples == 0 || avg <= 0 {
		return QueueETA{}, nil
	}
	return QueueETA{Known: true, Wait: etcWait(position, limit, avg), Samples: samples, Basis: basis}, nil
}

// averageDurationTx reads the rolling average of observed attempt durations:
// the group window first, then the tenant window as documented fallback. Both
// windows are the newest ETASampleWindow samples.
func averageDurationTx(ctx context.Context, tx pgx.Tx, tenantID, groupID uuid.UUID) (time.Duration, int, string, error) {
	var (
		avg     *float64
		samples int
	)
	if err := tx.QueryRow(ctx,
		`SELECT avg(duration_ms)::float8, count(*) FROM (
		   SELECT duration_ms FROM run_duration_samples
		    WHERE tenant_id = $1 AND group_id = $2
		    ORDER BY observed_at DESC, ctid DESC
		    LIMIT $3
		 ) recent`, tenantID, groupID, ETASampleWindow).Scan(&avg, &samples); err != nil {
		return 0, 0, "", fmt.Errorf("placement: read group duration samples: %w", err)
	}
	if samples > 0 && avg != nil {
		return time.Duration(*avg * float64(time.Millisecond)), samples, "group", nil
	}
	if err := tx.QueryRow(ctx,
		`SELECT avg(duration_ms)::float8, count(*) FROM (
		   SELECT duration_ms FROM run_duration_samples
		    WHERE tenant_id = $1
		    ORDER BY observed_at DESC, ctid DESC
		    LIMIT $2
		 ) recent`, tenantID, ETASampleWindow).Scan(&avg, &samples); err != nil {
		return 0, 0, "", fmt.Errorf("placement: read tenant duration samples: %w", err)
	}
	if samples > 0 && avg != nil {
		return time.Duration(*avg * float64(time.Millisecond)), samples, "tenant", nil
	}
	return 0, 0, "", nil
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

// memoryErrorTx is the typed refusal of a group whose memory budget has no
// headroom; the wait is the nearest running attempt in the group (its release
// frees memory).
func (s *PGStore) memoryErrorTx(ctx context.Context, tx pgx.Tx, req admitWrite, groupID uuid.UUID, used int64, active int) error {
	var (
		limit int64
		wait  time.Duration
	)
	if err := tx.QueryRow(ctx,
		`SELECT memory_limit_bytes FROM groups WHERE tenant_id = $1 AND group_id = $2`,
		req.tenantID, groupID).Scan(&limit); err != nil {
		return fmt.Errorf("placement: read group memory limit: %w", err)
	}
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(min(lease_until) - now(), interval '0')
		   FROM runs
		  WHERE tenant_id = $1 AND group_id = $2 AND status = 'running' AND lease_until IS NOT NULL`,
		req.tenantID, groupID).Scan(&wait); err != nil {
		wait = 0
	}
	if wait < DefaultRetryAfter {
		wait = DefaultRetryAfter
	}
	return &CapacityError{
		Reason:     CapacityGroupMemory,
		Resource:   capacityResource(CapacityGroupMemory),
		TenantID:   req.tenantID,
		SessionID:  req.sessionID,
		GroupID:    groupID,
		Active:     active,
		RetryAfter: wait,
		LimitBytes: limit,
		UsedBytes:  used,
	}
}

// promoteQueuedTx promotes FIFO queue heads for one tenant while capacity
// permits. Callers hold the tenant advisory lock (AdmitRun, ReleaseRun,
// PromoteQueued), so the counts it maintains cannot be changed by another
// promoter mid-sweep. The write is guarded by status='queued': a row is
// promoted exactly once even if two paths reach it.
//
// A head that does not fit the group's memory budget (or whose estimate is
// unknown) stops that group's promotion (head-of-line): letting a later entry
// jump the queue would silently break FIFO.
func (s *PGStore) promoteQueuedTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, tenantLimit int) ([]Promotion, error) {
	if tenantLimit < 0 {
		return nil, nil
	}
	// Most admissions happen with an empty queue: one indexed probe keeps the
	// common path cheap instead of listing groups and counting runs on every
	// write.
	var queued bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM runs WHERE tenant_id = $1 AND status = 'queued')`,
		tenantID).Scan(&queued); err != nil {
		return nil, fmt.Errorf("placement: probe queued runs: %w", err)
	}
	if !queued {
		return nil, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT group_id, max_concurrent_runs, memory_limit_bytes
		   FROM groups
		  WHERE tenant_id = $1 AND status = 'active'
		  ORDER BY name`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("placement: list promotable groups: %w", err)
	}
	type promotableGroup struct {
		groupID uuid.UUID
		maxRuns int
		memory  int64
	}
	var groups []promotableGroup
	for rows.Next() {
		var group promotableGroup
		if err := rows.Scan(&group.groupID, &group.maxRuns, &group.memory); err != nil {
			rows.Close()
			return nil, fmt.Errorf("placement: scan promotable group: %w", err)
		}
		groups = append(groups, group)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("placement: list promotable groups: %w", err)
	}
	if len(groups) == 0 {
		return nil, nil
	}

	// One snapshot of the active counts: promotions in this transaction are the
	// only writers that change them (the advisory lock is held).
	var activeTenant int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM runs WHERE tenant_id = $1 AND status IN ('pending', 'running')`,
		tenantID).Scan(&activeTenant); err != nil {
		return nil, fmt.Errorf("placement: count tenant runs: %w", err)
	}
	type groupUsage struct{ active, used int64 }
	usage := make(map[uuid.UUID]groupUsage, len(groups))
	usageRows, err := tx.Query(ctx,
		`SELECT group_id, count(*), COALESCE(sum(memory_bytes), 0)
		   FROM runs
		  WHERE tenant_id = $1 AND status IN ('pending', 'running') AND group_id IS NOT NULL
		  GROUP BY group_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("placement: count group runs: %w", err)
	}
	for usageRows.Next() {
		var (
			groupID uuid.UUID
			entry   groupUsage
		)
		if err := usageRows.Scan(&groupID, &entry.active, &entry.used); err != nil {
			usageRows.Close()
			return nil, fmt.Errorf("placement: scan group runs: %w", err)
		}
		usage[groupID] = entry
	}
	usageRows.Close()
	if err := usageRows.Err(); err != nil {
		return nil, fmt.Errorf("placement: count group runs: %w", err)
	}

	promoted := make([]Promotion, 0)
	for _, group := range groups {
		entry := usage[group.groupID]
		for {
			if group.maxRuns > 0 && entry.active >= int64(group.maxRuns) {
				break
			}
			if tenantLimit > 0 && activeTenant >= tenantLimit {
				break
			}
			var (
				sessionID uuid.UUID
				memory    int64
			)
			err := tx.QueryRow(ctx,
				`SELECT session_id, memory_bytes FROM runs
				  WHERE tenant_id = $1 AND group_id = $2 AND status = 'queued'
				  ORDER BY queue_seq
				  LIMIT 1
				  FOR UPDATE`, tenantID, group.groupID).Scan(&sessionID, &memory)
			if errors.Is(err, pgx.ErrNoRows) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("placement: read queue head: %w", err)
			}
			if group.memory > 0 {
				if memory <= 0 || entry.used+memory > group.memory {
					break
				}
			}
			tag, err := tx.Exec(ctx,
				`UPDATE runs SET status = 'pending', queue_seq = NULL, updated_at = now()
				  WHERE tenant_id = $1 AND session_id = $2 AND status = 'queued'`,
				tenantID, sessionID)
			if err != nil {
				return nil, fmt.Errorf("placement: promote queued run: %w", err)
			}
			if tag.RowsAffected() == 0 {
				// The row stopped being queued (a concurrent release settled it);
				// re-read the head rather than promoting blindly.
				continue
			}
			entry.active++
			entry.used += memory
			activeTenant++
			promoted = append(promoted, Promotion{
				TenantID: tenantID, GroupID: group.groupID, SessionID: sessionID,
			})
		}
	}
	return promoted, nil
}

// QueueState reads one session's durable queue entry.
func (s *PGStore) QueueState(ctx context.Context, tenantID, sessionID uuid.UUID) (QueueState, error) {
	state := QueueState{TenantID: tenantID, SessionID: sessionID}
	var (
		status   string
		groupID  uuid.UUID
		position int
	)
	err := s.pool.QueryRow(ctx,
		`SELECT status, COALESCE(group_id, '00000000-0000-0000-0000-000000000000'::uuid)
		   FROM runs WHERE tenant_id = $1 AND session_id = $2`, tenantID, sessionID).
		Scan(&status, &groupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return QueueState{}, fmt.Errorf("placement: read queue state: %w", err)
	}
	if status != RunStatusQueued || groupID == uuid.Nil {
		return state, nil
	}
	group, err := s.GetGroup(ctx, tenantID, groupID)
	if err != nil {
		if errors.Is(err, ErrGroupNotFound) {
			return state, nil
		}
		return QueueState{}, err
	}
	if err := s.pool.QueryRow(ctx, queuePositionQuery, tenantID, groupID, sessionID).Scan(&position); err != nil {
		return QueueState{}, fmt.Errorf("placement: read queue position: %w", err)
	}
	if position == 0 {
		// The row was promoted between the status read and the position read: the
		// live row is no longer waiting, so report it as admitted instead of a
		// stale position 0.
		return state, nil
	}
	active, err := s.activeGroupCount(ctx, tenantID, groupID)
	if err != nil {
		return QueueState{}, err
	}
	eta, err := s.queueETA(ctx, tenantID, groupID, position, group.MaxConcurrentRuns)
	if err != nil {
		return QueueState{}, err
	}
	return QueueState{
		TenantID: tenantID, SessionID: sessionID, GroupID: groupID,
		Queued: true, Position: position, Limit: group.MaxConcurrentRuns,
		Active: active, ETA: eta,
	}, nil
}

func (s *PGStore) activeGroupCount(ctx context.Context, tenantID, groupID uuid.UUID) (int, error) {
	var active int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM runs
		  WHERE tenant_id = $1 AND group_id = $2 AND status IN ('pending', 'running')`,
		tenantID, groupID).Scan(&active); err != nil {
		return 0, fmt.Errorf("placement: count group runs: %w", err)
	}
	return active, nil
}

func (s *PGStore) queueETA(ctx context.Context, tenantID, groupID uuid.UUID, position, limit int) (QueueETA, error) {
	avg, samples, basis, err := s.averageDuration(ctx, tenantID, groupID)
	if err != nil {
		return QueueETA{}, err
	}
	if samples == 0 || avg <= 0 {
		return QueueETA{}, nil
	}
	return QueueETA{Known: true, Wait: etcWait(position, limit, avg), Samples: samples, Basis: basis}, nil
}

func (s *PGStore) averageDuration(ctx context.Context, tenantID, groupID uuid.UUID) (time.Duration, int, string, error) {
	var (
		avg     *float64
		samples int
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT avg(duration_ms)::float8, count(*) FROM (
		   SELECT duration_ms FROM run_duration_samples
		    WHERE tenant_id = $1 AND group_id = $2
		    ORDER BY observed_at DESC, ctid DESC
		    LIMIT $3
		 ) recent`, tenantID, groupID, ETASampleWindow).Scan(&avg, &samples); err != nil {
		return 0, 0, "", fmt.Errorf("placement: read group duration samples: %w", err)
	}
	if samples > 0 && avg != nil {
		return time.Duration(*avg * float64(time.Millisecond)), samples, "group", nil
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT avg(duration_ms)::float8, count(*) FROM (
		   SELECT duration_ms FROM run_duration_samples
		    WHERE tenant_id = $1
		    ORDER BY observed_at DESC, ctid DESC
		    LIMIT $2
		 ) recent`, tenantID, ETASampleWindow).Scan(&avg, &samples); err != nil {
		return 0, 0, "", fmt.Errorf("placement: read tenant duration samples: %w", err)
	}
	if samples > 0 && avg != nil {
		return time.Duration(*avg * float64(time.Millisecond)), samples, "tenant", nil
	}
	return 0, 0, "", nil
}

func (s *PGStore) ReleaseRun(ctx context.Context, tenantID, sessionID uuid.UUID, tenantLimit int) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockPlacementTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		// Only an unstarted reservation with no unread input is ours to drop. A
		// racing successful commit sets pending_input in the same transaction it
		// appends the message, so it wins over this release; a claimed run is
		// already running and is settled by the kernel's finish path.
		if _, err := tx.Exec(ctx,
			`UPDATE runs SET status = 'done', lease_until = NULL, queue_seq = NULL, updated_at = now()
			  WHERE tenant_id = $1 AND session_id = $2
			    AND status IN ('pending', 'queued') AND NOT pending_input`,
			tenantID, sessionID); err != nil {
			return fmt.Errorf("placement: release run: %w", err)
		}
		// The slot may have freed: promote in the same transaction so a release
		// cannot leave the queue waiting for the next sweep.
		if _, err := s.promoteQueuedTx(ctx, tx, tenantID, tenantLimit); err != nil {
			return err
		}
		return nil
	})
}

// PromoteQueued promotes the tenant's FIFO queue heads while capacity permits.
func (s *PGStore) PromoteQueued(ctx context.Context, tenantID uuid.UUID, tenantLimit int) ([]Promotion, error) {
	if tenantLimit < 0 {
		return nil, nil
	}
	var promoted []Promotion
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockPlacementTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		var err error
		promoted, err = s.promoteQueuedTx(ctx, tx, tenantID, tenantLimit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return promoted, nil
}

// ObserveRunDuration appends one observed attempt duration and prunes the
// group's history to ETASampleRetention samples.
func (s *PGStore) ObserveRunDuration(ctx context.Context, tenantID, groupID uuid.UUID, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	ms := duration.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO run_duration_samples (tenant_id, group_id, duration_ms)
			 VALUES ($1, $2, $3)`, tenantID, groupID, ms); err != nil {
			return fmt.Errorf("placement: record run duration: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM run_duration_samples s
			  WHERE s.tenant_id = $1 AND s.group_id = $2
			    AND s.ctid NOT IN (
			        SELECT ctid FROM run_duration_samples
			         WHERE tenant_id = $1 AND group_id = $2
			         ORDER BY observed_at DESC, ctid DESC
			         LIMIT $3)`,
			tenantID, groupID, ETASampleRetention); err != nil {
			return fmt.Errorf("placement: prune run durations: %w", err)
		}
		return nil
	})
}

// GroupMemoryUsage reports the active-run memory estimate per group.
func (s *PGStore) GroupMemoryUsage(ctx context.Context, tenantID uuid.UUID) (map[uuid.UUID]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT group_id, COALESCE(sum(memory_bytes), 0)
		   FROM runs
		  WHERE tenant_id = $1 AND status IN ('pending', 'running') AND group_id IS NOT NULL
		  GROUP BY group_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("placement: group memory usage: %w", err)
	}
	defer rows.Close()
	usage := map[uuid.UUID]int64{}
	for rows.Next() {
		var (
			groupID uuid.UUID
			used    int64
		)
		if err := rows.Scan(&groupID, &used); err != nil {
			return nil, fmt.Errorf("placement: scan group memory usage: %w", err)
		}
		usage[groupID] = used
	}
	return usage, rows.Err()
}

// QueuedTenants lists the tenants with queued runs (crash-recovery sweep).
func (s *PGStore) QueuedTenants(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT tenant_id FROM runs WHERE status = 'queued' ORDER BY tenant_id`)
	if err != nil {
		return nil, fmt.Errorf("placement: queued tenants: %w", err)
	}
	defer rows.Close()
	var tenants []uuid.UUID
	for rows.Next() {
		var tenantID uuid.UUID
		if err := rows.Scan(&tenantID); err != nil {
			return nil, fmt.Errorf("placement: scan queued tenant: %w", err)
		}
		tenants = append(tenants, tenantID)
	}
	return tenants, rows.Err()
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
