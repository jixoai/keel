// Service is the policy layer: it resolves a strategy to a group, applies the
// lease TTL, resolves the tenant run quota, and returns the typed conflict /
// capacity errors the edge maps to 409 / 429. The store performs the atomic
// writes that make those promises hold under concurrent workers.
package placement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidStrategy reports an unknown placement strategy.
var ErrInvalidStrategy = errors.New("placement: unknown strategy")

// Service wires a Store into the placement strategies.
type Service struct {
	store          Store
	strategy       Strategy
	pinGroup       *uuid.UUID
	defaultMaxRuns int
	defaultMemory  int64
	sessionMemory  int64
	tenantRunLimit func(ctx context.Context, tenantID uuid.UUID) (int, error)
	newID          func() uuid.UUID
	leaseTTL       time.Duration
	now            func() time.Time
}

// New validates the options and builds the service. A nil store is a
// configuration error, not a lazy default: placement must never fall back to a
// process-local map in production.
func New(opts Options) (*Service, error) {
	if opts.Store == nil {
		return nil, errors.New("placement: store is required")
	}
	strategy := opts.Strategy
	if strategy == "" {
		strategy = Pack
	}
	switch strategy {
	case Pack, User, Pin:
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidStrategy, strategy)
	}
	sessionMemory := opts.SessionMemoryBytes
	if sessionMemory <= 0 {
		sessionMemory = DefaultSessionMemoryBytes
	}
	newID := opts.NewID
	if newID == nil {
		newID = uuid.New
	}
	return &Service{
		store:          opts.Store,
		strategy:       strategy,
		pinGroup:       opts.PinGroupID,
		defaultMaxRuns: opts.DefaultMaxConcurrentRuns,
		defaultMemory:  opts.DefaultMemoryLimitBytes,
		sessionMemory:  sessionMemory,
		tenantRunLimit: opts.TenantRunLimit,
		newID:          newID,
		leaseTTL:       DefaultLeaseTTL,
		now:            time.Now,
	}, nil
}

// Store exposes the wired store for tests and diagnostics.
func (s *Service) Store() Store { return s.store }

// CreateGroup creates an explicit group (an operator or a project bootstrap).
// Zero limits mean unlimited, exactly as the schema documents.
func (s *Service) CreateGroup(ctx context.Context, spec GroupSpec) (Group, error) {
	if spec.GroupID == uuid.Nil {
		spec.GroupID = s.newID()
	}
	return s.store.CreateGroup(ctx, spec)
}

// GetGroup reads one group.
func (s *Service) GetGroup(ctx context.Context, tenantID, groupID uuid.UUID) (Group, error) {
	return s.store.GetGroup(ctx, tenantID, groupID)
}

// ListGroups lists a tenant's groups (diagnostics / placement views).
func (s *Service) ListGroups(ctx context.Context, tenantID uuid.UUID) ([]Group, error) {
	return s.store.ListGroups(ctx, tenantID)
}

// SetGroupStatus starts a drain or retires a group. Retired is terminal.
func (s *Service) SetGroupStatus(ctx context.Context, tenantID, groupID uuid.UUID, status string) (Group, error) {
	if err := ValidateGroupStatus(status); err != nil {
		return Group{}, err
	}
	return s.store.SetGroupStatus(ctx, tenantID, groupID, status)
}

// Claim is the epoch-fenced placement claim used by a worker before it starts
// a run. A successful claim advances the epoch and renews the lease; a stale
// epoch, a live lease held by another worker, or a live lease in another group
// is a typed Conflict (HTTP 409), and the loser must not start a run.
func (s *Service) Claim(ctx context.Context, req ClaimRequest) (Placement, error) {
	if err := validatePlacementIDs(req.TenantID, req.SessionID); err != nil {
		return Placement{}, err
	}
	if req.WorkerID == "" {
		return Placement{}, errors.New("placement: claim requires a worker id")
	}
	if req.ExpectedEpoch < 0 {
		return Placement{}, errors.New("placement: expected epoch must be >= 0")
	}
	strategy := s.strategyFor(req.Strategy)
	groupID, err := s.targetGroup(ctx, req.TenantID, req.SessionID, req.GroupID, strategy)
	if err != nil {
		return Placement{}, err
	}
	return s.store.ClaimPlacement(ctx, claimWrite{
		tenantID:      req.TenantID,
		sessionID:     req.SessionID,
		groupID:       groupID,
		expectedEpoch: req.ExpectedEpoch,
		workerID:      req.WorkerID,
		ttl:           s.ttl(req.TTL),
		moveReason:    ReasonLeaseHeld,
	})
}

// Adopt mirrors the kernel's fencing epoch into the placement row. The kernel
// is the authority for execution; the placement follows (never lowers) its
// epoch. An expired placement may be adopted, a live one only by its holder.
func (s *Service) Adopt(ctx context.Context, req AdoptRequest) (Placement, error) {
	if err := validatePlacementIDs(req.TenantID, req.SessionID); err != nil {
		return Placement{}, err
	}
	if req.WorkerID == "" {
		return Placement{}, errors.New("placement: adopt requires a worker id")
	}
	if req.Epoch <= 0 {
		return Placement{}, errors.New("placement: adopt requires a positive epoch")
	}
	strategy := s.strategyFor(req.Strategy)
	groupID, err := s.targetGroup(ctx, req.TenantID, req.SessionID, req.GroupID, strategy)
	if err != nil {
		return Placement{}, err
	}
	return s.store.AdoptPlacement(ctx, adoptWrite{
		tenantID:  req.TenantID,
		sessionID: req.SessionID,
		groupID:   groupID,
		workerID:  req.WorkerID,
		epoch:     req.Epoch,
		ttl:       s.ttl(req.TTL),
	})
}

// Join places a session and takes its lease in one step. Joining the current
// group again is idempotent for the same worker; joining a different group
// while the current lease is live is ReasonAlreadyPlaced, because a move must
// go through drain/expire + claim.
func (s *Service) Join(ctx context.Context, req JoinRequest) (Placement, error) {
	if err := validatePlacementIDs(req.TenantID, req.SessionID); err != nil {
		return Placement{}, err
	}
	if req.WorkerID == "" {
		return Placement{}, errors.New("placement: join requires a worker id")
	}
	strategy := s.strategyFor(req.Strategy)
	groupID := req.GroupID
	expectedEpoch := int64(0)
	current, err := s.store.GetPlacement(ctx, req.TenantID, req.SessionID)
	switch {
	case err == nil:
		expectedEpoch = current.Epoch
		if groupID == nil {
			id := current.GroupID
			groupID = &id
		}
	case errors.Is(err, ErrPlacementNotFound):
		if groupID == nil {
			id, err := s.resolveStrategyGroup(ctx, req.TenantID, strategy)
			if err != nil {
				return Placement{}, err
			}
			groupID = &id
		}
	default:
		return Placement{}, err
	}
	return s.store.ClaimPlacement(ctx, claimWrite{
		tenantID:      req.TenantID,
		sessionID:     req.SessionID,
		groupID:       groupID,
		expectedEpoch: expectedEpoch,
		workerID:      req.WorkerID,
		ttl:           s.ttl(req.TTL),
		moveReason:    ReasonAlreadyPlaced,
	})
}

// Leave removes the placement fact. A stale epoch cannot delete a newer one.
func (s *Service) Leave(ctx context.Context, req LeaveRequest) (Placement, error) {
	if err := validatePlacementIDs(req.TenantID, req.SessionID); err != nil {
		return Placement{}, err
	}
	if req.ExpectedEpoch <= 0 {
		return Placement{}, errors.New("placement: leave requires a positive expected epoch")
	}
	current, err := s.store.GetPlacement(ctx, req.TenantID, req.SessionID)
	if err != nil {
		return Placement{}, err
	}
	if err := s.store.ReleasePlacement(ctx, req.TenantID, req.SessionID, req.WorkerID, req.ExpectedEpoch); err != nil {
		return Placement{}, err
	}
	return current, nil
}

// Seek returns a migration plan; it moves nothing. The caller drives the plan:
// drain the old group, then let the new group rebuild the session from the
// event ledger (no process state is copied).
func (s *Service) Seek(ctx context.Context, req SeekRequest) (MigrationPlan, error) {
	if err := validatePlacementIDs(req.TenantID, req.SessionID); err != nil {
		return MigrationPlan{}, err
	}
	if req.ToGroupID == uuid.Nil {
		return MigrationPlan{}, errors.New("placement: seek requires a target group")
	}
	current, err := s.store.GetPlacement(ctx, req.TenantID, req.SessionID)
	if err != nil {
		if errors.Is(err, ErrPlacementNotFound) {
			return MigrationPlan{}, &Conflict{
				Reason: ReasonPlacementNotFound, TenantID: req.TenantID, SessionID: req.SessionID,
			}
		}
		return MigrationPlan{}, err
	}
	if req.ExpectedEpoch != 0 && req.ExpectedEpoch != current.Epoch {
		return MigrationPlan{}, &Conflict{
			Reason: ReasonEpochMismatch, TenantID: req.TenantID, SessionID: req.SessionID,
			GroupID: current.GroupID, ExpectedEpoch: req.ExpectedEpoch,
			CurrentEpoch: current.Epoch, Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
		}
	}
	fromGroup, err := s.store.GetGroup(ctx, req.TenantID, current.GroupID)
	if err != nil {
		return MigrationPlan{}, err
	}
	toGroup, err := s.store.GetGroup(ctx, req.TenantID, req.ToGroupID)
	if err != nil {
		if errors.Is(err, ErrGroupNotFound) {
			return MigrationPlan{}, &Conflict{
				Reason: ReasonGroupUnavailable, TenantID: req.TenantID, SessionID: req.SessionID,
				GroupID: req.ToGroupID,
			}
		}
		return MigrationPlan{}, err
	}
	if toGroup.Status != StatusActive {
		return MigrationPlan{}, &Conflict{
			Reason: ReasonGroupDraining, TenantID: req.TenantID, SessionID: req.SessionID,
			GroupID: req.ToGroupID, Detail: "target group is not active",
		}
	}
	return MigrationPlan{
		From:             current,
		FromGroup:        fromGroup,
		To:               toGroup,
		ExpectedEpoch:    current.Epoch,
		NextEpoch:        current.Epoch + 1,
		DrainRequired:    current.LeaseUntil.After(s.now()),
		RebuildFromSeq:   0,
		CopyProcessState: false,
	}, nil
}

// AdmitRun reserves one run slot for a placed session. Group slots and the
// tenant quota are counted from the runs table inside one DB transaction, so
// concurrent workers (even in different groups) cannot oversell either limit.
// A full group or spent tenant quota is a typed CapacityError (HTTP 429 with
// Retry-After), never a silently dropped run.
//
// The pending run row this writes is the durable queue entry. A process that
// crashes between admission and the input commit leaves a claimable row; the
// next worker claims it, finds no owed input and settles it as done, so no
// separate sweeper is needed (there is no second ledger to reconcile). When the
// commit itself fails in-process, GatedCommitter releases the reservation
// immediately; ReleaseRun is the guarded form of that (pending, no unread
// input).
func (s *Service) AdmitRun(ctx context.Context, req AdmitRequest) (Admission, error) {
	if err := validatePlacementIDs(req.TenantID, req.SessionID); err != nil {
		return Admission{}, err
	}
	tenantLimit := 0
	if s.tenantRunLimit != nil {
		limit, err := s.tenantRunLimit(ctx, req.TenantID)
		if err != nil {
			// Fail closed: a quota that cannot be resolved must not admit.
			return Admission{}, fmt.Errorf("placement: resolve tenant run limit: %w", err)
		}
		tenantLimit = limit
	}
	return s.store.AdmitRun(ctx, admitWrite{
		tenantID:    req.TenantID,
		sessionID:   req.SessionID,
		groupID:     req.GroupID,
		tenantLimit: tenantLimit,
	})
}

// ReleaseRun frees a slot whose run was admitted but abandoned before the
// kernel started it. A running run is released by the kernel's finish path,
// never here.
func (s *Service) ReleaseRun(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	if err := validatePlacementIDs(tenantID, sessionID); err != nil {
		return err
	}
	return s.store.ReleaseRun(ctx, tenantID, sessionID)
}

func (s *Service) strategyFor(strategy Strategy) Strategy {
	if strategy == "" {
		return s.strategy
	}
	return strategy
}

func (s *Service) ttl(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return s.leaseTTL
	}
	return ttl
}

// targetGroup keeps the current group when the request names none and the
// session is placed; otherwise it resolves the strategy. The current placement
// is a hint, not authority: the store's compare-and-set is what decides.
func (s *Service) targetGroup(ctx context.Context, tenantID, sessionID uuid.UUID, groupID *uuid.UUID, strategy Strategy) (*uuid.UUID, error) {
	if groupID != nil {
		return groupID, nil
	}
	current, err := s.store.GetPlacement(ctx, tenantID, sessionID)
	switch {
	case err == nil:
		id := current.GroupID
		return &id, nil
	case errors.Is(err, ErrPlacementNotFound):
		id, err := s.resolveStrategyGroup(ctx, tenantID, strategy)
		if err != nil {
			return nil, err
		}
		return &id, nil
	default:
		return nil, err
	}
}

// resolveStrategyGroup maps a strategy to an automatic group.
//
//   - pin needs the caller's group; the strategy never guesses one.
//   - user ensures one "user" group per tenant (enterprise isolation SKU).
//   - pack ensures the single "pack-default" group. Multi-group packing by
//     memory headroom is a later optimisation; the spec allows the default
//     pack to be one group, and the memory limit is recorded for that step.
func (s *Service) resolveStrategyGroup(ctx context.Context, tenantID uuid.UUID, strategy Strategy) (uuid.UUID, error) {
	switch strategy {
	case Pack:
		group, err := s.store.EnsureGroup(ctx, GroupSpec{
			TenantID:          tenantID,
			GroupID:           s.newID(),
			Name:              PackDefaultGroupName,
			MaxConcurrentRuns: s.defaultMaxRuns,
			MemoryLimitBytes:  s.defaultMemory,
		})
		if err != nil {
			return uuid.Nil, err
		}
		return group.GroupID, nil
	case User:
		group, err := s.store.EnsureGroup(ctx, GroupSpec{
			TenantID:          tenantID,
			GroupID:           s.newID(),
			Name:              UserGroupName,
			MaxConcurrentRuns: s.defaultMaxRuns,
			MemoryLimitBytes:  s.defaultMemory,
		})
		if err != nil {
			return uuid.Nil, err
		}
		return group.GroupID, nil
	case Pin:
		if s.pinGroup != nil && *s.pinGroup != uuid.Nil {
			return *s.pinGroup, nil
		}
		return uuid.Nil, &Conflict{
			Reason: ReasonPinRequiresGroup, TenantID: tenantID,
			Detail: "pin strategy requires an explicit or configured group id",
		}
	default:
		return uuid.Nil, fmt.Errorf("%w: %q", ErrInvalidStrategy, strategy)
	}
}

func validatePlacementIDs(tenantID, sessionID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return errors.New("placement: tenant id is required")
	}
	if sessionID == uuid.Nil {
		return errors.New("placement: session id is required")
	}
	return nil
}
