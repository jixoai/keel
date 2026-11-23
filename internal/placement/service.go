// Service is the policy layer: it resolves a strategy to a group, applies the
// lease TTL, resolves the tenant run quota, and returns the typed conflict /
// capacity errors the edge maps to 409 / 429. The store performs the atomic
// writes that make those promises hold under concurrent workers.
package placement

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	memoryEstimate MemoryEstimator
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
		memoryEstimate: opts.MemoryEstimator,
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
			id, err := s.resolveStrategyGroup(ctx, req.TenantID, req.SessionID, strategy)
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

// AdmitRun reserves one run slot for a placed session, or queues it when the
// group's slots are full. Group slots, the memory budget and the tenant quota
// are counted from the runs table inside one DB transaction, so concurrent
// workers (even in different groups) cannot oversell any limit.
//
//   - a full group is a queued admission (durable FIFO entry + position/ETA);
//   - the tenant quota stays a hard CapacityError (429 + Retry-After): the
//     queue is never an oversell channel;
//   - a memory-bounded group without an estimate is a typed UnsupportedError,
//     never an invented number.
//
// A process that crashes between admission and the input commit leaves a
// claimable pending row (or a queued row) behind; the next worker claims it,
// finds no owed input and settles it as done, and the queue sweeper promotes
// the next entry, so no separate ledger exists to reconcile. When the commit
// itself fails in-process, GatedCommitter releases the reservation immediately;
// ReleaseRun is the guarded form of that (pending/queued, no unread input).
func (s *Service) AdmitRun(ctx context.Context, req AdmitRequest) (Admission, error) {
	if err := validatePlacementIDs(req.TenantID, req.SessionID); err != nil {
		return Admission{}, err
	}
	tenantLimit, err := s.resolveTenantLimit(ctx, req.TenantID)
	if err != nil {
		// Fail closed: a quota that cannot be resolved must not admit.
		return Admission{}, err
	}
	// The estimate is only meaningful for a memory-bounded group; resolve it
	// after reading the group so an unbounded deployment pays nothing. An unknown
	// estimate is passed through as unknown: the store turns it into the typed
	// UnsupportedError only when it actually needs a new reservation (a queued
	// run's repeated input keeps its existing estimate and is never refused for
	// the estimator's absence).
	placement, err := s.store.GetPlacement(ctx, req.TenantID, req.SessionID)
	if err != nil {
		if errors.Is(err, ErrPlacementNotFound) {
			return Admission{}, &Conflict{
				Reason: ReasonPlacementNotFound, TenantID: req.TenantID, SessionID: req.SessionID,
			}
		}
		return Admission{}, err
	}
	group, err := s.store.GetGroup(ctx, req.TenantID, placement.GroupID)
	if err != nil {
		if errors.Is(err, ErrGroupNotFound) {
			return Admission{}, &Conflict{
				Reason: ReasonGroupUnavailable, TenantID: req.TenantID,
				SessionID: req.SessionID, GroupID: placement.GroupID,
			}
		}
		return Admission{}, err
	}
	var (
		estimate int64
		known    bool
	)
	if group.MemoryLimitBytes > 0 {
		estimate, known, err = s.estimateMemory(ctx, req.TenantID, req.SessionID)
		if err != nil {
			return Admission{}, err
		}
	}
	return s.store.AdmitRun(ctx, admitWrite{
		tenantID:       req.TenantID,
		sessionID:      req.SessionID,
		groupID:        req.GroupID,
		tenantLimit:    tenantLimit,
		memoryEstimate: estimate,
		memoryKnown:    known,
	})
}

// ReleaseRun frees a slot whose run was admitted but abandoned before the
// kernel started it (pending or queued, no unread input), then promotes the
// tenant's FIFO queue heads while capacity permits. A running run is released
// by the kernel's finish path, never here.
func (s *Service) ReleaseRun(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	if err := validatePlacementIDs(tenantID, sessionID); err != nil {
		return err
	}
	tenantLimit, err := s.resolveTenantLimit(ctx, tenantID)
	if err != nil {
		// The release itself must not be lost because the quota document is
		// unreadable; skip promotion (negative limit) and let the recovery sweep
		// retry. Returning the quota error would strand the reservation.
		tenantLimit = -1
	}
	return s.store.ReleaseRun(ctx, tenantID, sessionID, tenantLimit)
}

// QueueState reports the durable queue entry for one session (Queued=false when
// its run is not waiting). The HTTP edge reads it after a commit so a queued
// admission answers 202 with its position and ETA.
func (s *Service) QueueState(ctx context.Context, tenantID, sessionID uuid.UUID) (QueueState, error) {
	if err := validatePlacementIDs(tenantID, sessionID); err != nil {
		return QueueState{}, err
	}
	return s.store.QueueState(ctx, tenantID, sessionID)
}

// PromoteQueued promotes the tenant's FIFO queue heads while slots, the memory
// budget and the tenant quota all permit. It is the settlement/release path;
// errors are surfaced so the caller can log them (the recovery sweep converges).
func (s *Service) PromoteQueued(ctx context.Context, tenantID uuid.UUID) ([]Promotion, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("placement: tenant id is required")
	}
	tenantLimit, err := s.resolveTenantLimit(ctx, tenantID)
	if err != nil {
		// Fail closed: promoting without a readable quota could oversell it.
		return nil, err
	}
	return s.store.PromoteQueued(ctx, tenantID, tenantLimit)
}

// PromoteAllQueued is the crash-recovery sweep: every tenant with a queued run
// gets one FIFO promotion pass. Per-tenant failures do not stop the sweep; they
// are returned so the caller can log them.
func (s *Service) PromoteAllQueued(ctx context.Context) ([]Promotion, error) {
	tenants, err := s.store.QueuedTenants(ctx)
	if err != nil {
		return nil, err
	}
	var (
		promoted []Promotion
		failures []error
	)
	for _, tenantID := range tenants {
		tenantPromoted, err := s.PromoteQueued(ctx, tenantID)
		if err != nil {
			failures = append(failures, fmt.Errorf("tenant %s: %w", tenantID, err))
			continue
		}
		promoted = append(promoted, tenantPromoted...)
	}
	return promoted, errors.Join(failures...)
}

// ObserveRunDuration records one observed attempt duration for the queue ETA
// average. A session without a placement has no group to attribute it to, so it
// is ignored (the run was not admitted through this gate).
func (s *Service) ObserveRunDuration(ctx context.Context, tenantID, sessionID uuid.UUID, duration time.Duration) error {
	if err := validatePlacementIDs(tenantID, sessionID); err != nil {
		return err
	}
	if duration <= 0 {
		return nil
	}
	placement, err := s.store.GetPlacement(ctx, tenantID, sessionID)
	if err != nil {
		if errors.Is(err, ErrPlacementNotFound) {
			return nil
		}
		return err
	}
	return s.store.ObserveRunDuration(ctx, tenantID, placement.GroupID, duration)
}

// resolveTenantLimit resolves the tenant run quota (0 = unlimited). An error is
// fail-closed; the message names the resolution step so a misconfigured quota
// document is diagnosable.
func (s *Service) resolveTenantLimit(ctx context.Context, tenantID uuid.UUID) (int, error) {
	if s.tenantRunLimit == nil {
		return 0, nil
	}
	limit, err := s.tenantRunLimit(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("placement: resolve tenant run limit: %w", err)
	}
	return limit, nil
}

// estimateMemory resolves the session's memory estimate. known=false means the
// estimator is absent or answered a non-positive value: callers turn that into
// a typed unsupported error rather than a fabricated number.
func (s *Service) estimateMemory(ctx context.Context, tenantID, sessionID uuid.UUID) (int64, bool, error) {
	if s.memoryEstimate == nil {
		return 0, false, nil
	}
	estimate, err := s.memoryEstimate(ctx, tenantID, sessionID)
	if err != nil {
		return 0, false, fmt.Errorf("placement: resolve memory estimate: %w", err)
	}
	if estimate <= 0 {
		return 0, false, nil
	}
	return estimate, true, nil
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
		id, err := s.resolveStrategyGroup(ctx, tenantID, sessionID, strategy)
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
//   - pack chooses among the tenant's active pack groups by memory headroom
//     (best fit: the smallest leftover after placing the session), creating
//     "pack-default" when none exists. A memory-bounded pack group needs an
//     observed estimate; without one the call fails with *UnsupportedError
//     instead of inventing a number.
func (s *Service) resolveStrategyGroup(ctx context.Context, tenantID, sessionID uuid.UUID, strategy Strategy) (uuid.UUID, error) {
	switch strategy {
	case Pack:
		return s.packGroup(ctx, tenantID, sessionID)
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

// packGroup packs one unplaced session into a pack group by memory headroom.
// Only the tenant's active pack groups are candidates (pack-default plus any
// "pack-*" group an operator created); the strategy never creates a second
// group on its own, because one group is hosted by one worker process in this
// change, so a group nobody hosts would strand the session.
func (s *Service) packGroup(ctx context.Context, tenantID, sessionID uuid.UUID) (uuid.UUID, error) {
	groups, err := s.store.ListGroups(ctx, tenantID)
	if err != nil {
		return uuid.Nil, err
	}
	candidates := make([]Group, 0, len(groups))
	for _, group := range groups {
		if group.Status == StatusActive && isPackGroupName(group.Name) {
			candidates = append(candidates, group)
		}
	}
	if len(candidates) == 0 {
		// The default pack group's limits are deployment configuration. A
		// memory-bounded default with no estimate is refused before the group is
		// created, so the failure does not leave an unusable group behind.
		if s.defaultMemory > 0 {
			if _, known, err := s.estimateMemory(ctx, tenantID, sessionID); err != nil {
				return uuid.Nil, err
			} else if !known {
				return uuid.Nil, &UnsupportedError{
					Reason: ReasonMemoryEstimateUnavailable, TenantID: tenantID,
					SessionID: sessionID,
					Detail:    "the default pack group defines a memory budget but no memory estimate is configured",
				}
			}
		}
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
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	bounded := 0
	for _, group := range candidates {
		if group.MemoryLimitBytes > 0 {
			bounded++
		}
	}
	if bounded == 0 {
		// Every pack group is unbounded: no estimate is involved.
		return candidates[0].GroupID, nil
	}
	estimate, known, err := s.estimateMemory(ctx, tenantID, sessionID)
	if err != nil {
		return uuid.Nil, err
	}
	if !known {
		// Without an estimate only an unbounded group can be chosen honestly.
		for _, group := range candidates {
			if group.MemoryLimitBytes == 0 {
				return group.GroupID, nil
			}
		}
		return uuid.Nil, &UnsupportedError{
			Reason: ReasonMemoryEstimateUnavailable, TenantID: tenantID, SessionID: sessionID,
			Detail: "the pack groups define memory budgets but no memory estimate is configured",
		}
	}
	usage, err := s.store.GroupUsage(ctx, tenantID)
	if err != nil {
		return uuid.Nil, err
	}
	var (
		best      *Group
		bestLeft  int64
		unbounded bool
		// Diagnostics of the refused pack pool: every candidate group's active
		// runs and estimates, summed. Active is a run count, never a group count,
		// and UsedBytes is filled whenever the store knows the estimates.
		activeRuns int
		usedBytes  int64
		limitBytes int64
	)
	for i := range candidates {
		group := &candidates[i]
		entry := usage[group.GroupID]
		activeRuns += entry.Runs
		usedBytes += entry.MemoryBytes
		limitBytes += group.MemoryLimitBytes
		if group.MemoryLimitBytes == 0 {
			// An unbounded group is the fallback: a bounded group that fits is the
			// denser (and therefore preferred) packing choice.
			if best == nil {
				best, unbounded = group, true
			}
			continue
		}
		used := entry.MemoryBytes
		if used+estimate > group.MemoryLimitBytes {
			continue
		}
		left := group.MemoryLimitBytes - used - estimate
		if unbounded || best == nil || left < bestLeft || (left == bestLeft && group.Name < best.Name) {
			best, bestLeft, unbounded = group, left, false
		}
	}
	if best == nil {
		// No pack group has headroom. Memory is a hard ceiling like a full group
		// slot; unlike slots there is no queue, because a queued run behind an
		// over-budget group could never be promoted without eviction. The
		// diagnostics aggregate the whole pack pool the caller may spill into:
		// runs (a count, not len(groups)), used bytes and the pool's capacity.
		return uuid.Nil, &CapacityError{
			Reason:     CapacityGroupMemory,
			Resource:   capacityResource(CapacityGroupMemory),
			TenantID:   tenantID,
			SessionID:  sessionID,
			LimitBytes: limitBytes,
			UsedBytes:  usedBytes,
			Active:     activeRuns,
			RetryAfter: DefaultRetryAfter,
		}
	}
	return best.GroupID, nil
}

// isPackGroupName reports whether a group belongs to the pack pool. The
// strategies are the only creators of these names (see resolveStrategyGroup).
func isPackGroupName(name string) bool {
	return name == PackDefaultGroupName || strings.HasPrefix(name, "pack-")
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
