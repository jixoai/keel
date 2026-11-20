// In-memory placement store. It exists for unit tests and embedded single-process
// use: the production runtime uses PGStore so two workers cannot both win the
// same lease or both sell the last group slot. The semantics deliberately mirror
// pg.go, including typed conflicts and capacity refusals.
package placement

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

type memGroupKey struct {
	tenantID uuid.UUID
	groupID  uuid.UUID
}

type memSessionKey struct {
	tenantID  uuid.UUID
	sessionID uuid.UUID
}

type memRun struct {
	status     string
	groupID    uuid.UUID
	leaseUntil time.Time
}

// MemStore is a mutex-guarded Store. One instance must not be shared across
// processes; its atomicity is a Go mutex, not a database transaction.
type MemStore struct {
	mu          sync.Mutex
	now         func() time.Time
	groups      map[memGroupKey]Group
	placements  map[memSessionKey]Placement
	runs        map[memSessionKey]memRun
	nextGroupID func() uuid.UUID
}

// NewMemStore builds an empty store. The clock is injectable for lease/retry
// tests.
func NewMemStore(now func() time.Time) *MemStore {
	if now == nil {
		now = time.Now
	}
	return &MemStore{
		now:        now,
		groups:     map[memGroupKey]Group{},
		placements: map[memSessionKey]Placement{},
		runs:       map[memSessionKey]memRun{},
	}
}

func (m *MemStore) CreateGroup(_ context.Context, spec GroupSpec) (Group, error) {
	if err := validateGroupSpec(spec); err != nil {
		return Group{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, group := range m.groups {
		if group.TenantID == spec.TenantID && group.Name == spec.Name {
			return Group{}, ErrGroupExists
		}
	}
	now := m.now()
	group := Group{
		TenantID:          spec.TenantID,
		GroupID:           spec.GroupID,
		Name:              spec.Name,
		MaxConcurrentRuns: spec.MaxConcurrentRuns,
		MemoryLimitBytes:  spec.MemoryLimitBytes,
		Status:            StatusActive,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	m.groups[memGroupKey{spec.TenantID, spec.GroupID}] = group
	return group, nil
}

func (m *MemStore) EnsureGroup(ctx context.Context, spec GroupSpec) (Group, error) {
	m.mu.Lock()
	for _, group := range m.groups {
		if group.TenantID == spec.TenantID && group.Name == spec.Name {
			m.mu.Unlock()
			return group, nil
		}
	}
	m.mu.Unlock()
	group, err := m.CreateGroup(ctx, spec)
	if errors.Is(err, ErrGroupExists) {
		// Lost a race with another EnsureGroup; read the winner.
		return m.groupByName(spec.TenantID, spec.Name)
	}
	return group, err
}

func (m *MemStore) groupByName(tenantID uuid.UUID, name string) (Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, group := range m.groups {
		if group.TenantID == tenantID && group.Name == name {
			return group, nil
		}
	}
	return Group{}, ErrGroupNotFound
}

func (m *MemStore) GetGroup(_ context.Context, tenantID, groupID uuid.UUID) (Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	group, ok := m.groups[memGroupKey{tenantID, groupID}]
	if !ok {
		return Group{}, ErrGroupNotFound
	}
	return group, nil
}

func (m *MemStore) ListGroups(_ context.Context, tenantID uuid.UUID) ([]Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	groups := make([]Group, 0, len(m.groups))
	for _, group := range m.groups {
		if group.TenantID == tenantID {
			groups = append(groups, group)
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups, nil
}

func (m *MemStore) SetGroupStatus(_ context.Context, tenantID, groupID uuid.UUID, status string) (Group, error) {
	if err := ValidateGroupStatus(status); err != nil {
		return Group{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memGroupKey{tenantID, groupID}
	group, ok := m.groups[key]
	if !ok {
		return Group{}, ErrGroupNotFound
	}
	if group.Status == StatusRetired && status != StatusRetired {
		return Group{}, &Conflict{Reason: ReasonGroupUnavailable, TenantID: tenantID, GroupID: groupID,
			Detail: "retired groups cannot be reactivated"}
	}
	group.Status = status
	group.UpdatedAt = m.now()
	m.groups[key] = group
	return group, nil
}

func (m *MemStore) GetPlacement(_ context.Context, tenantID, sessionID uuid.UUID) (Placement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	placement, ok := m.placements[memSessionKey{tenantID, sessionID}]
	if !ok {
		return Placement{}, ErrPlacementNotFound
	}
	return placement, nil
}

func (m *MemStore) ClaimPlacement(_ context.Context, req claimWrite) (Placement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	key := memSessionKey{req.tenantID, req.sessionID}
	current, found := m.placements[key]
	live := found && current.LeaseUntil.After(now)

	if !found && req.expectedEpoch != 0 {
		return Placement{}, &Conflict{
			Reason: ReasonEpochMismatch, TenantID: req.tenantID, SessionID: req.sessionID,
			ExpectedEpoch: req.expectedEpoch, Detail: "session has no placement",
		}
	}
	if found && current.Epoch != req.expectedEpoch {
		return Placement{}, &Conflict{
			Reason: ReasonEpochMismatch, TenantID: req.tenantID, SessionID: req.sessionID,
			GroupID: current.GroupID, ExpectedEpoch: req.expectedEpoch,
			CurrentEpoch: current.Epoch, Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
		}
	}

	var (
		next  Placement
		group uuid.UUID
		epoch int64
	)
	if found {
		group = current.GroupID
		epoch = current.Epoch + 1
		if req.groupID != nil && *req.groupID != current.GroupID {
			if live {
				return Placement{}, &Conflict{
					Reason: req.moveReason, TenantID: req.tenantID, SessionID: req.sessionID,
					GroupID: current.GroupID, ExpectedEpoch: req.expectedEpoch,
					CurrentEpoch: current.Epoch, Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
					Detail: "live placement in another group must drain or expire before a move",
				}
			}
			group = *req.groupID
		}
		if live && current.WorkerID != req.workerID {
			return Placement{}, &Conflict{
				Reason: ReasonLeaseHeld, TenantID: req.tenantID, SessionID: req.sessionID,
				GroupID: current.GroupID, ExpectedEpoch: req.expectedEpoch,
				CurrentEpoch: current.Epoch, Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
			}
		}
	} else {
		if req.groupID == nil {
			return Placement{}, &Conflict{
				Reason: ReasonGroupUnavailable, TenantID: req.tenantID, SessionID: req.sessionID,
				Detail: "no group resolved for an unplaced session",
			}
		}
		group = *req.groupID
		epoch = 1
	}
	if err := m.checkGroupUsable(req.tenantID, group, found && group == current.GroupID); err != nil {
		return Placement{}, err
	}
	next = Placement{
		TenantID:   req.tenantID,
		SessionID:  req.sessionID,
		GroupID:    group,
		WorkerID:   req.workerID,
		Epoch:      epoch,
		LeaseUntil: now.Add(req.ttl),
		UpdatedAt:  now,
	}
	m.placements[key] = next
	return next, nil
}

func (m *MemStore) AdoptPlacement(_ context.Context, req adoptWrite) (Placement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	key := memSessionKey{req.tenantID, req.sessionID}
	current, found := m.placements[key]
	live := found && current.LeaseUntil.After(now)

	var (
		group uuid.UUID
	)
	if found {
		if req.epoch < current.Epoch {
			return Placement{}, &Conflict{
				Reason: ReasonEpochMismatch, TenantID: req.tenantID, SessionID: req.sessionID,
				GroupID: current.GroupID, ExpectedEpoch: req.epoch, CurrentEpoch: current.Epoch,
				Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
				Detail: "kernel epoch is older than the stored placement",
			}
		}
		group = current.GroupID
		if req.groupID != nil && *req.groupID != current.GroupID {
			if live {
				return Placement{}, &Conflict{
					Reason: ReasonLeaseHeld, TenantID: req.tenantID, SessionID: req.sessionID,
					GroupID: current.GroupID, ExpectedEpoch: req.epoch, CurrentEpoch: current.Epoch,
					Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
					Detail: "live placement in another group must drain or expire before a move",
				}
			}
			group = *req.groupID
		}
		if live && current.WorkerID != req.workerID {
			return Placement{}, &Conflict{
				Reason: ReasonLeaseHeld, TenantID: req.tenantID, SessionID: req.sessionID,
				GroupID: current.GroupID, ExpectedEpoch: req.epoch, CurrentEpoch: current.Epoch,
				Holder: current.WorkerID, LeaseUntil: current.LeaseUntil,
			}
		}
	} else {
		if req.epoch <= 0 {
			return Placement{}, &Conflict{
				Reason: ReasonEpochMismatch, TenantID: req.tenantID, SessionID: req.sessionID,
				ExpectedEpoch: req.epoch, Detail: "adopt requires a positive kernel epoch",
			}
		}
		if req.groupID == nil {
			return Placement{}, &Conflict{
				Reason: ReasonGroupUnavailable, TenantID: req.tenantID, SessionID: req.sessionID,
				Detail: "no group resolved for an unplaced session",
			}
		}
		group = *req.groupID
	}
	if err := m.checkGroupUsable(req.tenantID, group, found && group == current.GroupID); err != nil {
		return Placement{}, err
	}
	next := Placement{
		TenantID:   req.tenantID,
		SessionID:  req.sessionID,
		GroupID:    group,
		WorkerID:   req.workerID,
		Epoch:      req.epoch,
		LeaseUntil: now.Add(req.ttl),
		UpdatedAt:  now,
	}
	m.placements[key] = next
	return next, nil
}

func (m *MemStore) ReleasePlacement(_ context.Context, tenantID, sessionID uuid.UUID, workerID string, expectedEpoch int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memSessionKey{tenantID, sessionID}
	current, ok := m.placements[key]
	if !ok {
		return ErrPlacementNotFound
	}
	if current.Epoch != expectedEpoch || (workerID != "" && current.WorkerID != workerID) {
		return &Conflict{
			Reason: ReasonEpochMismatch, TenantID: tenantID, SessionID: sessionID, GroupID: current.GroupID,
			ExpectedEpoch: expectedEpoch, CurrentEpoch: current.Epoch, Holder: current.WorkerID,
			LeaseUntil: current.LeaseUntil,
		}
	}
	delete(m.placements, key)
	return nil
}

func (m *MemStore) AdmitRun(_ context.Context, req admitWrite) (Admission, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	key := memSessionKey{req.tenantID, req.sessionID}
	placement, ok := m.placements[key]
	if !ok {
		return Admission{}, &Conflict{
			Reason: ReasonPlacementNotFound, TenantID: req.tenantID, SessionID: req.sessionID,
		}
	}
	if req.groupID != nil && *req.groupID != placement.GroupID {
		return Admission{}, &Conflict{
			Reason: ReasonAlreadyPlaced, TenantID: req.tenantID, SessionID: req.sessionID,
			GroupID: placement.GroupID,
			Detail:  "run admission names a group other than the session's placement",
		}
	}
	if err := m.checkGroupUsable(req.tenantID, placement.GroupID, false); err != nil {
		return Admission{}, err
	}
	group := m.groups[memGroupKey{req.tenantID, placement.GroupID}]

	activeGroup, activeTenant := 0, 0
	for runKey, run := range m.runs {
		if run.status != RunStatusPending && run.status != RunStatusRunning {
			continue
		}
		if runKey.tenantID != req.tenantID {
			continue
		}
		activeTenant++
		if run.groupID == placement.GroupID {
			activeGroup++
		}
	}
	selfRun, hasRun := m.runs[key]
	selfActive := hasRun && (selfRun.status == RunStatusPending || selfRun.status == RunStatusRunning)

	if !selfActive {
		if group.MaxConcurrentRuns > 0 && activeGroup >= group.MaxConcurrentRuns {
			return Admission{}, m.capacityError(req, placement, CapacityGroup,
				group.MaxConcurrentRuns, activeGroup, now)
		}
		if req.tenantLimit > 0 && activeTenant >= req.tenantLimit {
			return Admission{}, m.capacityError(req, placement, CapacityTenant,
				req.tenantLimit, activeTenant, now)
		}
	}

	m.runs[key] = memRun{status: RunStatusPending, groupID: placement.GroupID}
	if !selfActive {
		activeGroup++
		activeTenant++
	}
	return Admission{
		TenantID:      req.tenantID,
		SessionID:     req.sessionID,
		GroupID:       placement.GroupID,
		ActiveGroup:   activeGroup,
		LimitGroup:    group.MaxConcurrentRuns,
		ActiveTenant:  activeTenant,
		LimitTenant:   req.tenantLimit,
		AlreadyActive: selfActive,
	}, nil
}

func (m *MemStore) ReleaseRun(_ context.Context, tenantID, sessionID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memSessionKey{tenantID, sessionID}
	run, ok := m.runs[key]
	if !ok || run.status != RunStatusPending {
		// A running run is released by the kernel's finish path, never here.
		return nil
	}
	run.status = RunStatusDone
	run.leaseUntil = time.Time{}
	m.runs[key] = run
	return nil
}

// SetRunStatus is a test/embedding aid: it moves an admitted run to running
// (with a lease) or to a terminal status, mirroring what the kernel's claim and
// finish paths write. It is not part of Store.
func (m *MemStore) SetRunStatus(tenantID, sessionID uuid.UUID, status string, lease time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memSessionKey{tenantID, sessionID}
	run, ok := m.runs[key]
	if !ok {
		return ErrNoRun
	}
	run.status = status
	if status == RunStatusRunning && lease > 0 {
		run.leaseUntil = m.now().Add(lease)
	}
	m.runs[key] = run
	return nil
}

func (m *MemStore) checkGroupUsable(tenantID, groupID uuid.UUID, allowDraining bool) error {
	group, ok := m.groups[memGroupKey{tenantID, groupID}]
	if !ok || group.Status == StatusRetired {
		return &Conflict{
			Reason: ReasonGroupUnavailable, TenantID: tenantID, GroupID: groupID,
		}
	}
	if group.Status == StatusDraining && !allowDraining {
		return &Conflict{
			Reason: ReasonGroupDraining, TenantID: tenantID, GroupID: groupID,
			Detail: "group is draining and takes no new placements",
		}
	}
	return nil
}

// capacityError builds the typed 429 refusal. RetryAfter is the nearest active
// run's lease expiry in the refused scope (at least DefaultRetryAfter), so the
// client's retry is explainable instead of a blind backoff.
func (m *MemStore) capacityError(req admitWrite, placement Placement, reason CapacityReason, limit, active int, now time.Time) error {
	retry := time.Duration(0)
	for key, run := range m.runs {
		if key.tenantID != req.tenantID || run.status != RunStatusRunning || !run.leaseUntil.After(now) {
			continue
		}
		if reason == CapacityGroup && run.groupID != placement.GroupID {
			continue
		}
		wait := run.leaseUntil.Sub(now)
		if retry == 0 || wait < retry {
			retry = wait
		}
	}
	if retry < DefaultRetryAfter {
		retry = DefaultRetryAfter
	}
	return &CapacityError{
		Reason:     reason,
		Resource:   capacityResource(reason),
		TenantID:   req.tenantID,
		SessionID:  req.sessionID,
		GroupID:    placement.GroupID,
		Limit:      limit,
		Active:     active,
		RetryAfter: retry,
	}
}

func capacityResource(reason CapacityReason) string {
	if reason == CapacityTenant {
		return "tenant"
	}
	return "group"
}
