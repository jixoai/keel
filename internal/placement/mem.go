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
	// pendingInput mirrors the runs.pending_input column: a successful commit
	// sets it, and a release must not drop a run whose input already landed.
	pendingInput bool
	// queueSeq is the FIFO position key (mirrors runs.queue_seq); 0 when the
	// row is not queued.
	queueSeq int64
	// memoryBytes is the admission memory estimate (mirrors runs.memory_bytes).
	memoryBytes int64
}

// MemStore is a mutex-guarded Store. One instance must not be shared across
// processes; its atomicity is a Go mutex, not a database transaction.
type MemStore struct {
	mu            sync.Mutex
	now           func() time.Time
	groups        map[memGroupKey]Group
	placements    map[memSessionKey]Placement
	runs          map[memSessionKey]memRun
	nextGroupID   func() uuid.UUID
	nextQueueSeq  int64
	groupSamples  map[memGroupKey][]time.Duration
	tenantSamples map[uuid.UUID][]time.Duration
}

// NewMemStore builds an empty store. The clock is injectable for lease/retry
// tests.
func NewMemStore(now func() time.Time) *MemStore {
	if now == nil {
		now = time.Now
	}
	return &MemStore{
		now:           now,
		groups:        map[memGroupKey]Group{},
		placements:    map[memSessionKey]Placement{},
		runs:          map[memSessionKey]memRun{},
		groupSamples:  map[memGroupKey][]time.Duration{},
		tenantSamples: map[uuid.UUID][]time.Duration{},
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

	selfRun, hasRun := m.runs[key]
	selfActive := hasRun && runActive(selfRun.status)
	selfQueued := hasRun && selfRun.status == RunStatusQueued
	selfAdmitted := selfActive && selfRun.groupID == placement.GroupID
	// What existed before this call: promotion below may promote this session's
	// own queued row, but Created/AlreadyActive describe this call's reservation.
	wasActive, wasQueued := selfActive, selfQueued
	if selfActive && !selfAdmitted && selfRun.status == RunStatusRunning {
		return Admission{}, &Conflict{
			Reason: ReasonAlreadyPlaced, TenantID: req.tenantID, SessionID: req.sessionID,
			GroupID: selfRun.groupID, Detail: "the session is already running in another group",
		}
	}

	if selfQueued && selfRun.groupID != placement.GroupID {
		// The placement moved while the run waited (an expired lease was
		// re-claimed elsewhere): the queued row follows its placement and joins
		// the target group's queue at the tail. A bounded target without an
		// estimate is the same typed refusal as a new admission, not a zero
		// that would stall promotion at the queue head.
		if group.MemoryLimitBytes > 0 && (!req.memoryKnown || req.memoryEstimate <= 0) {
			return Admission{}, &UnsupportedError{
				Reason: ReasonMemoryEstimateUnavailable, TenantID: req.tenantID,
				SessionID: req.sessionID, GroupID: placement.GroupID,
				Detail: "the target group defines a memory budget but no memory estimate is configured",
			}
		}
		m.nextQueueSeq++
		selfRun.groupID = placement.GroupID
		selfRun.queueSeq = m.nextQueueSeq
		// The queued row now belongs to the target group; carry the estimate
		// resolved for that group instead of the source group's 0.
		selfRun.memoryBytes = req.memoryEstimate
		m.runs[key] = selfRun
	}

	activeGroup, activeTenant := m.activeCountsLocked(req.tenantID, placement.GroupID)
	usedMemory := m.groupMemoryLocked(req.tenantID, placement.GroupID)
	// FIFO fairness: when the group has a free slot, promote queued heads before
	// this admission may take it. A crash/reconfigure window can leave queued
	// rows and free slots at the same time; filling the slot first would let the
	// newcomer jump the queue. A full group queues this admission anyway, so the
	// promotion pass is skipped there (the triggers/sweeper handle other groups).
	if group.MaxConcurrentRuns == 0 || activeGroup < group.MaxConcurrentRuns {
		m.promoteLocked(req.tenantID, req.tenantLimit)
		if final, ok := m.runs[key]; ok {
			selfRun, selfActive = final, runActive(final.status)
			selfQueued = final.status == RunStatusQueued
			selfAdmitted = selfActive && final.groupID == placement.GroupID
		}
		activeGroup, activeTenant = m.activeCountsLocked(req.tenantID, placement.GroupID)
		usedMemory = m.groupMemoryLocked(req.tenantID, placement.GroupID)
	}
	// newAdmission is the only case that consumes a fresh slot: an already
	// active row or a waiting queue entry is not a second admission.
	newAdmission := !selfActive && !selfQueued && !selfAdmitted

	if !selfAdmitted && !selfQueued {
		// The tenant quota is a hard capacity refusal: the queue never bypasses
		// it, and a queued run does not consume a tenant slot while it waits. A
		// queued row's new input is not a new admission, so it is not re-charged.
		if !selfActive && req.tenantLimit > 0 && activeTenant >= req.tenantLimit {
			return Admission{}, m.capacityError(req, placement, CapacityTenant,
				req.tenantLimit, activeTenant, now)
		}
		// Memory is a hard ceiling at admission too. No estimate is not a zero
		// estimate: refusing with a typed unsupported error is honest, while a
		// fabricated number would make the ceiling look enforced.
		if group.MemoryLimitBytes > 0 {
			if !req.memoryKnown || req.memoryEstimate <= 0 {
				return Admission{}, &UnsupportedError{
					Reason: ReasonMemoryEstimateUnavailable, TenantID: req.tenantID,
					SessionID: req.sessionID, GroupID: group.GroupID,
					Detail: "the group defines a memory budget but no memory estimate is configured",
				}
			}
			if usedMemory+req.memoryEstimate > group.MemoryLimitBytes {
				return Admission{}, m.memoryCapacityError(req, placement, group, usedMemory, now)
			}
		}
		// A new run queues when the group is full, and also when a queued head
		// remains after promotion: a head blocked by the memory budget keeps its
		// place, so a smaller newcomer must not slip past it.
		_, _, headWaiting := m.queueHeadLocked(req.tenantID, placement.GroupID)
		if (group.MaxConcurrentRuns > 0 && activeGroup >= group.MaxConcurrentRuns) ||
			(newAdmission && headWaiting) {
			// A full group (or a waiting head) queues: the queued run row is the
			// durable FIFO entry, it holds no slot, and the kernel cannot claim it.
			seq := selfRun.queueSeq
			if !selfQueued || seq == 0 {
				m.nextQueueSeq++
				seq = m.nextQueueSeq
			}
			m.runs[key] = memRun{
				status: RunStatusQueued, groupID: placement.GroupID,
				pendingInput: selfRun.pendingInput, queueSeq: seq,
				memoryBytes: req.memoryEstimate,
			}
			position := m.queuePositionLocked(req.tenantID, placement.GroupID, seq)
			return Admission{
				TenantID:      req.tenantID,
				SessionID:     req.sessionID,
				GroupID:       placement.GroupID,
				ActiveGroup:   activeGroup,
				LimitGroup:    group.MaxConcurrentRuns,
				ActiveTenant:  activeTenant,
				LimitTenant:   req.tenantLimit,
				Created:       !wasActive && !wasQueued,
				Queued:        true,
				QueuePosition: position,
				ETA:           m.etaLocked(req.tenantID, placement.GroupID, position, group.MaxConcurrentRuns),
				MemoryBytes:   req.memoryEstimate,
			}, nil
		}
	}

	if selfAdmitted {
		// Keep the live attempt's status/lease: an admission for a guide input
		// must not demote a running run back to pending.
		selfRun.groupID = placement.GroupID
		m.runs[key] = selfRun
	} else if selfQueued {
		// A queued row is only ever moved by promotion, so a repeated input cannot
		// jump ahead of an earlier queued run. The sweep below may promote it when
		// it reaches the head.
		selfRun.groupID = placement.GroupID
		m.runs[key] = selfRun
	} else {
		m.runs[key] = memRun{status: RunStatusPending, groupID: placement.GroupID,
			pendingInput: selfRun.pendingInput, memoryBytes: req.memoryEstimate}
	}
	if final, ok := m.runs[key]; ok && final.status == RunStatusQueued {
		position := m.queuePositionLocked(req.tenantID, placement.GroupID, final.queueSeq)
		activeGroup, activeTenant = m.activeCountsLocked(req.tenantID, placement.GroupID)
		return Admission{
			TenantID:      req.tenantID,
			SessionID:     req.sessionID,
			GroupID:       placement.GroupID,
			ActiveGroup:   activeGroup,
			LimitGroup:    group.MaxConcurrentRuns,
			ActiveTenant:  activeTenant,
			LimitTenant:   req.tenantLimit,
			Created:       !wasActive && !wasQueued,
			Queued:        true,
			QueuePosition: position,
			ETA:           m.etaLocked(req.tenantID, placement.GroupID, position, group.MaxConcurrentRuns),
			MemoryBytes:   req.memoryEstimate,
		}, nil
	}
	activeGroup, activeTenant = m.activeCountsLocked(req.tenantID, placement.GroupID)
	return Admission{
		TenantID:      req.tenantID,
		SessionID:     req.sessionID,
		GroupID:       placement.GroupID,
		ActiveGroup:   activeGroup,
		LimitGroup:    group.MaxConcurrentRuns,
		ActiveTenant:  activeTenant,
		LimitTenant:   req.tenantLimit,
		AlreadyActive: wasActive,
		Created:       !wasActive && !wasQueued,
		MemoryBytes:   req.memoryEstimate,
	}, nil
}

func (m *MemStore) ReleaseRun(_ context.Context, tenantID, sessionID uuid.UUID, tenantLimit int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memSessionKey{tenantID, sessionID}
	run, ok := m.runs[key]
	if ok && !run.pendingInput && (run.status == RunStatusPending || run.status == RunStatusQueued) {
		// A running run is released by the kernel's finish path, never here; a
		// pending/queued run whose input already committed is real work.
		run.status = RunStatusDone
		run.leaseUntil = time.Time{}
		run.queueSeq = 0
		m.runs[key] = run
	}
	// A slot may have freed: promote the head immediately, in the same critical
	// section that wrote the release.
	m.promoteLocked(tenantID, tenantLimit)
	return nil
}

// QueueState reads the durable queue entry for one session.
func (m *MemStore) QueueState(_ context.Context, tenantID, sessionID uuid.UUID) (QueueState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := QueueState{TenantID: tenantID, SessionID: sessionID}
	run, ok := m.runs[memSessionKey{tenantID, sessionID}]
	if !ok || run.status != RunStatusQueued {
		return state, nil
	}
	group, ok := m.groups[memGroupKey{tenantID, run.groupID}]
	if !ok {
		return state, nil
	}
	position := m.queuePositionLocked(tenantID, run.groupID, run.queueSeq)
	active, _ := m.activeCountsLocked(tenantID, run.groupID)
	return QueueState{
		TenantID: tenantID, SessionID: sessionID, GroupID: run.groupID,
		Queued: true, Position: position, Limit: group.MaxConcurrentRuns, Active: active,
		ETA: m.etaLocked(tenantID, run.groupID, position, group.MaxConcurrentRuns),
	}, nil
}

// PromoteQueued promotes the tenant's FIFO queue heads while capacity permits.
func (m *MemStore) PromoteQueued(_ context.Context, tenantID uuid.UUID, tenantLimit int) ([]Promotion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.promoteLocked(tenantID, tenantLimit), nil
}

// ObserveRunDuration records one observed attempt duration for the ETA
// average. Non-positive durations are ignored (an unknown duration is not a
// zero-length run).
func (m *MemStore) ObserveRunDuration(_ context.Context, tenantID, groupID uuid.UUID, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memGroupKey{tenantID, groupID}
	m.groupSamples[key] = trimSamples(append(m.groupSamples[key], duration))
	m.tenantSamples[tenantID] = trimSamples(append(m.tenantSamples[tenantID], duration))
	return nil
}

// GroupUsage reports each group's active-run count and memory estimate.
func (m *MemStore) GroupUsage(_ context.Context, tenantID uuid.UUID) (map[uuid.UUID]GroupUsage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	usage := map[uuid.UUID]GroupUsage{}
	for key, run := range m.runs {
		if key.tenantID != tenantID || !runActive(run.status) || run.groupID == uuid.Nil {
			continue
		}
		entry := usage[run.groupID]
		entry.Runs++
		entry.MemoryBytes += run.memoryBytes
		usage[run.groupID] = entry
	}
	return usage, nil
}

// QueuedTenants lists tenants with at least one queued run.
func (m *MemStore) QueuedTenants(_ context.Context) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[uuid.UUID]bool{}
	for key, run := range m.runs {
		if run.status == RunStatusQueued {
			seen[key.tenantID] = true
		}
	}
	tenants := make([]uuid.UUID, 0, len(seen))
	for tenantID := range seen {
		tenants = append(tenants, tenantID)
	}
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].String() < tenants[j].String() })
	return tenants, nil
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
	if status != RunStatusQueued {
		run.queueSeq = 0
	}
	if status == RunStatusRunning && lease > 0 {
		run.leaseUntil = m.now().Add(lease)
	}
	m.runs[key] = run
	return nil
}

// promoteLocked is the one promotion transition: a queued row becomes pending
// (claimable) exactly once, in FIFO order, only while the group has a free
// slot, the memory budget has headroom and the tenant quota has room. The
// status check makes it idempotent under concurrent callers.
func (m *MemStore) promoteLocked(tenantID uuid.UUID, tenantLimit int) []Promotion {
	if tenantLimit < 0 {
		// The quota could not be resolved: promoting without it could oversell, so
		// the release still happens but promotion waits for the recovery sweep.
		return nil
	}
	keys := make([]memGroupKey, 0, len(m.groups))
	for key, group := range m.groups {
		if key.tenantID == tenantID && group.Status == StatusActive {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return m.groups[keys[i]].Name < m.groups[keys[j]].Name
	})
	activeTenant := m.activeTenantLocked(tenantID)
	promoted := make([]Promotion, 0)
	for _, key := range keys {
		group := m.groups[key]
		activeGroup := m.activeGroupLocked(tenantID, key.groupID)
		usedMemory := m.groupMemoryLocked(tenantID, key.groupID)
		for {
			if group.MaxConcurrentRuns > 0 && activeGroup >= group.MaxConcurrentRuns {
				break
			}
			if tenantLimit > 0 && activeTenant >= tenantLimit {
				break
			}
			headKey, headRun, ok := m.queueHeadLocked(tenantID, key.groupID)
			if !ok {
				break
			}
			if group.MemoryLimitBytes > 0 {
				// Head-of-line: an unknown estimate or an over-budget head stops
				// this group rather than letting a later entry jump the queue.
				if headRun.memoryBytes <= 0 || usedMemory+headRun.memoryBytes > group.MemoryLimitBytes {
					break
				}
			}
			headRun.status = RunStatusPending
			headRun.queueSeq = 0
			m.runs[headKey] = headRun
			activeGroup++
			activeTenant++
			usedMemory += headRun.memoryBytes
			promoted = append(promoted, Promotion{
				TenantID: tenantID, GroupID: key.groupID, SessionID: headKey.sessionID,
			})
		}
	}
	return promoted
}

func (m *MemStore) queueHeadLocked(tenantID, groupID uuid.UUID) (memSessionKey, memRun, bool) {
	var (
		bestKey memSessionKey
		bestRun memRun
		found   bool
	)
	for key, run := range m.runs {
		if key.tenantID != tenantID || run.groupID != groupID || run.status != RunStatusQueued {
			continue
		}
		if !found || run.queueSeq < bestRun.queueSeq {
			bestKey, bestRun, found = key, run, true
		}
	}
	return bestKey, bestRun, found
}

// queuePositionLocked is the 1-based FIFO position of seq inside the group.
func (m *MemStore) queuePositionLocked(tenantID, groupID uuid.UUID, seq int64) int {
	if seq <= 0 {
		return 0
	}
	position := 0
	for key, run := range m.runs {
		if key.tenantID != tenantID || run.groupID != groupID || run.status != RunStatusQueued {
			continue
		}
		if run.queueSeq <= seq {
			position++
		}
	}
	return position
}

func (m *MemStore) activeTenantLocked(tenantID uuid.UUID) int {
	active := 0
	for key, run := range m.runs {
		if key.tenantID == tenantID && runActive(run.status) {
			active++
		}
	}
	return active
}

func (m *MemStore) activeGroupLocked(tenantID, groupID uuid.UUID) int {
	active := 0
	for key, run := range m.runs {
		if key.tenantID == tenantID && run.groupID == groupID && runActive(run.status) {
			active++
		}
	}
	return active
}

// activeCountsLocked reports (group, tenant) active counts.
func (m *MemStore) activeCountsLocked(tenantID, groupID uuid.UUID) (int, int) {
	group, tenant := 0, 0
	for key, run := range m.runs {
		if key.tenantID != tenantID || !runActive(run.status) {
			continue
		}
		tenant++
		if run.groupID == groupID {
			group++
		}
	}
	return group, tenant
}

func (m *MemStore) groupMemoryLocked(tenantID, groupID uuid.UUID) int64 {
	var used int64
	for key, run := range m.runs {
		if key.tenantID == tenantID && run.groupID == groupID && runActive(run.status) {
			used += run.memoryBytes
		}
	}
	return used
}

// etaLocked builds the explainable wait from observed durations. With no
// history the ETA is unknown (Wait stays 0 and Known is false), never a
// fabricated zero.
func (m *MemStore) etaLocked(tenantID, groupID uuid.UUID, position, limit int) QueueETA {
	avg, samples, basis := m.averageLocked(tenantID, groupID)
	if samples == 0 || avg <= 0 {
		return QueueETA{}
	}
	return QueueETA{
		Known: true, Wait: etcWait(position, limit, avg), Samples: samples, Basis: basis,
	}
}

func (m *MemStore) averageLocked(tenantID, groupID uuid.UUID) (time.Duration, int, string) {
	if samples := m.groupSamples[memGroupKey{tenantID, groupID}]; len(samples) > 0 {
		return meanDuration(samples), len(samples), "group"
	}
	if samples := m.tenantSamples[tenantID]; len(samples) > 0 {
		return meanDuration(samples), len(samples), "tenant"
	}
	return 0, 0, ""
}

func meanDuration(samples []time.Duration) time.Duration {
	var total time.Duration
	for _, sample := range samples {
		total += sample
	}
	return total / time.Duration(len(samples))
}

func trimSamples(samples []time.Duration) []time.Duration {
	if len(samples) > ETASampleRetention {
		return append([]time.Duration(nil), samples[len(samples)-ETASampleRetention:]...)
	}
	return samples
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

// memoryCapacityError is the typed refusal of a group whose memory budget has
// no headroom. The wait is the nearest running attempt's lease in the group:
// the completion that frees memory is the one that makes the retry work.
func (m *MemStore) memoryCapacityError(req admitWrite, placement Placement, group Group, used int64, now time.Time) error {
	retry := time.Duration(0)
	for key, run := range m.runs {
		if key.tenantID != req.tenantID || run.groupID != group.GroupID ||
			run.status != RunStatusRunning || !run.leaseUntil.After(now) {
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
		Reason:     CapacityGroupMemory,
		Resource:   capacityResource(CapacityGroupMemory),
		TenantID:   req.tenantID,
		SessionID:  req.sessionID,
		GroupID:    group.GroupID,
		Active:     m.activeGroupLocked(req.tenantID, group.GroupID),
		LimitBytes: group.MemoryLimitBytes,
		UsedBytes:  used,
		RetryAfter: retry,
	}
}

func capacityResource(reason CapacityReason) string {
	switch reason {
	case CapacityTenant:
		return "tenant"
	case CapacityGroupMemory:
		return "group_memory"
	default:
		return "group"
	}
}
