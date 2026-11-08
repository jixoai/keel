package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemStore is the in-memory fake LogStore. It exists for tests (and for the kernel's
// own unit tests); the runtime uses PGStore. It reproduces the observable
// semantics of the PostgreSQL adapter on a single process:
//
//   - the session is the linearization point: one lock serializes a session
//     transaction from BeginSession to Commit/Rollback and every lease verb,
//     just like the sessions row lock does in PG;
//   - the lease authority (owner, epoch, expiry) lives on the session, exactly
//     like sessions.owner / owner_epoch / lease_until; the run row only carries
//     queue state (status, attempts, cursor);
//   - sequences are allocated inside the transaction, so a rollback leaves no
//     gap and a rejected commit consumes nothing.
type MemStore struct {
	mu          sync.Mutex
	now         func() time.Time
	sessions    map[Scope]*memSession
	order       []Scope
	failApp     error
	failCommit  error
	retryFailed int // armed transient append failures
	begins      int // BeginSession calls (retry-budget evidence)
}

var _ LogStore = (*MemStore)(nil)

type memSession struct {
	events  []Event
	dedupe  map[string]int64 // namespaced dedupe key -> seq
	nextSeq int64
	lease   memLease // the authority
	run     *memRun  // queue state; nil = no run row
	// external, when set, marks the session as owned by an external backend.
	// Such a session never gets a run row, so it is never claimable.
	external *memExternal
}

// memLease mirrors sessions.owner / owner_epoch / lease_until.
type memLease struct {
	owner string
	epoch int64
	until time.Time
	held  bool // lease_until IS NOT NULL
}

type memRun struct {
	status    string
	attempts  int
	cursor    int64
	lastError string
	// loadoutID/loadoutRevision are the claim pin (kernel.PinRunLoadout);
	// loadoutGeneration/loadoutOwnerEpoch are the settle metadata written with
	// FinishLease, so an audit can name the generation and fencing epoch the
	// run finished under.
	loadoutID         string
	loadoutRevision   int64
	loadoutGeneration int64
	loadoutOwnerEpoch int64
	// pendingInput is the durable unread-input flag (PG parity).
	pendingInput bool
	// readWatermark is the last proven read point (PG parity).
	readWatermark int64
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{
		now:      time.Now,
		sessions: map[Scope]*memSession{},
	}
}

// SetClock replaces the store's clock (test hook for lease expiry).
func (m *MemStore) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// CreateSession registers a session. Sessions are never created by the kernel:
// the edge creates them (store.CreateSession), exactly as today.
func (m *MemStore) CreateSession(tenantID, sessionID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := Scope{TenantID: tenantID, SessionID: sessionID}
	if _, ok := m.sessions[scope]; ok {
		return
	}
	m.sessions[scope] = &memSession{dedupe: map[string]int64{}, nextSeq: 1}
	m.order = append(m.order, scope)
}

// EnqueueRun marks a session as having work (store.EnqueueRun semantics: a
// running run is left alone, anything else becomes pending). An external
// session is never enqueued: it must not enter the native run/lease flow
// (external-gateway spec: 外部会话 MUST NOT 进入原生 run/lease 作业流).
func (m *MemStore) EnqueueRun(tenantID, sessionID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := Scope{TenantID: tenantID, SessionID: sessionID}
	s, ok := m.sessions[scope]
	if !ok {
		s = &memSession{dedupe: map[string]int64{}, nextSeq: 1}
		m.sessions[scope] = s
		m.order = append(m.order, scope)
	}
	// A bare enqueue queues work; only an edge commit marks the input unread
	// (the flag is about client input, not about "there is something to do").
	m.wakeRunLocked(s, false)
}

// FailNextAppend makes the next Append fail (rollback test hook). Nothing is
// written and no sequence is consumed.
func (m *MemStore) FailNextAppend(err error) {
	if err == nil {
		err = errors.New("memstore: injected append failure")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failApp = err
}

// FailNextCommit makes the next Commit fail (rollback test hook).
func (m *MemStore) FailNextCommit(err error) {
	if err == nil {
		err = errors.New("memstore: injected commit failure")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failCommit = err
}

// FailNextRetryable arms n transient (retryable) append failures, the way a
// deadlocked PostgreSQL statement would surface. Nothing is written and no
// sequence is consumed, so a retry succeeds.
func (m *MemStore) FailNextRetryable(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryFailed = n
}

// BeginSessionCount reports how many session transactions were opened
// (retry-budget evidence for tests).
func (m *MemStore) BeginSessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.begins
}

// BeginSession opens a session transaction and holds the store lock until it
// commits or rolls back.
func (m *MemStore) BeginSession(ctx context.Context, tenantID, sessionID uuid.UUID) (SessionTxn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	scope := Scope{TenantID: tenantID, SessionID: sessionID}
	s, ok := m.sessions[scope]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("memstore: session %s: %w", scope, ErrNotFound)
	}
	m.begins++
	return &memTxn{m: m, s: s, scope: scope, seq: s.nextSeq}, nil
}

// LoadEvents reads a page of the log.
func (m *MemStore) LoadEvents(ctx context.Context, tenantID, sessionID uuid.UUID, afterSeq int64, limit int) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[Scope{TenantID: tenantID, SessionID: sessionID}]
	if !ok {
		return nil, fmt.Errorf("memstore: session: %w", ErrNotFound)
	}
	out := make([]Event, 0, limit)
	for _, ev := range s.events {
		if ev.Seq <= afterSeq {
			continue
		}
		out = append(out, cloneEvent(ev))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// FindDedupe looks up one event by its namespaced dedupe key.
func (m *MemStore) FindDedupe(ctx context.Context, tenantID, sessionID uuid.UUID, key string) (Event, bool, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[Scope{TenantID: tenantID, SessionID: sessionID}]
	if !ok {
		return Event{}, false, nil
	}
	return s.dedupeEvent(key)
}

// SessionExists reports whether the (tenant, session) pair exists.
func (m *MemStore) SessionExists(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessions[Scope{TenantID: tenantID, SessionID: sessionID}]
	return ok, nil
}

// ClaimLease claims the first runnable run.
func (m *MemStore) ClaimLease(ctx context.Context, owner string, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range m.order {
		s := m.sessions[scope]
		if s == nil || !m.runnableLocked(scope, s) {
			continue
		}
		return m.claimLocked(scope, s, owner, ttl), nil
	}
	return Lease{}, fmt.Errorf("memstore: %w", ErrNoRunnable)
}

// ClaimScope claims one specific session.
func (m *MemStore) ClaimScope(ctx context.Context, scope Scope, owner string, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[scope]
	if !ok {
		return Lease{}, fmt.Errorf("memstore: session %s: %w", scope, ErrNotFound)
	}
	if m.leaseStateLocked(scope, s).Live {
		return Lease{}, fmt.Errorf("memstore: %s: %w", scope, ErrLeaseHeld)
	}
	if s.run == nil {
		return Lease{}, fmt.Errorf("memstore: no run row for %s: %w", scope, ErrNoRunnable)
	}
	if !m.runnableLocked(scope, s) {
		return Lease{}, fmt.Errorf("memstore: %s: %w", scope, ErrNoRunnable)
	}
	return m.claimLocked(scope, s, owner, ttl), nil
}

func (m *MemStore) runnableLocked(scope Scope, s *memSession) bool {
	if s.run == nil {
		return false
	}
	// Fail closed: an external session is never claimable, even if a dirty run
	// row was injected into the queue (PG parity: the claim SQL filters
	// state_owner too).
	if s.isExternal() {
		return false
	}
	switch s.run.status {
	case RunStatusPending:
		return true
	case RunStatusRunning:
		return !m.leaseStateLocked(scope, s).Live
	default:
		return false
	}
}

func (m *MemStore) claimLocked(scope Scope, s *memSession, owner string, ttl time.Duration) Lease {
	reclaimed := s.run != nil && s.run.status == RunStatusRunning
	// The claim delivers the unread-input fact without clearing it: an attempt
	// abandoned before it reads the log must leave the input visible to the
	// next claim. Only a finish whose watermark covers every input clears it
	// (PG parity: runs.pending_input / read_watermark).
	pendingInput := s.run.pendingInput
	readWatermark := s.run.readWatermark
	s.lease.epoch++
	s.lease.owner = owner
	s.lease.until = m.now().Add(ttl)
	s.lease.held = true
	s.run.status = RunStatusRunning
	s.run.attempts++
	// A new claim invalidates the previous owner's loadout pin and settle
	// metadata; the new owner writes its own with PinRunLoadout and FinishLease.
	s.run.loadoutID = ""
	s.run.loadoutRevision = 0
	s.run.loadoutGeneration = 0
	s.run.loadoutOwnerEpoch = 0
	return Lease{
		Scope:         scope,
		Token:         OwnerToken{Owner: owner, Epoch: s.lease.epoch},
		Until:         s.lease.until,
		Reclaimed:     reclaimed,
		PendingInput:  pendingInput,
		ReadWatermark: readWatermark,
	}
}

// RenewLease conditionally extends the lease on owner+epoch.
func (m *MemStore) RenewLease(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[lease.Scope]
	if !ok {
		return Lease{}, fmt.Errorf("memstore: session %s: %w", lease.Scope, ErrNotFound)
	}
	if err := checkToken(m.leaseStateLocked(lease.Scope, s), lease.Token); err != nil {
		return Lease{}, fmt.Errorf("memstore: renew: %w", err)
	}
	s.lease.until = m.now().Add(ttl)
	return Lease{Scope: lease.Scope, Token: lease.Token, Until: s.lease.until}, nil
}

// FinishLease settles a claimed run.
func (m *MemStore) FinishLease(ctx context.Context, req FinishRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[req.Lease.Scope]
	if !ok {
		return fmt.Errorf("memstore: session %s: %w", req.Lease.Scope, ErrNotFound)
	}
	if err := checkToken(m.leaseStateLocked(req.Lease.Scope, s), req.Lease.Token); err != nil {
		return fmt.Errorf("memstore: finish: %w", err)
	}
	if s.run == nil {
		return fmt.Errorf("memstore: finish: %w", ErrNotFound)
	}
	moreInput := false
	if req.Cancelled {
		// A cancelled attempt is terminal: its unread input is not a re-queue
		// trigger (PG parity).
		moreInput = false
	} else if req.Err != nil {
		// A failed attempt leaves unread input pending (PG parity).
		moreInput = true
	} else {
		// PG parity: an input above the read watermark is unread unless a later
		// assistant message consumed it (payload.inputSeqs). A guide input
		// injected and answered inside the run must not re-queue the session.
		probeAfter := req.probeAfter()
		answered := make(map[int64]bool)
		for _, ev := range s.events {
			if ev.Kind != KindAssistantMessage || ev.Seq <= probeAfter {
				continue
			}
			var payload struct {
				InputSeqs []int64 `json:"inputSeqs"`
			}
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				continue
			}
			for _, seq := range payload.InputSeqs {
				answered[seq] = true
			}
		}
		for _, ev := range s.events {
			if ev.Seq > probeAfter && ev.Kind == KindUserMessage && !answered[ev.Seq] {
				moreInput = true
				break
			}
		}
	}
	s.run.status = finishStatus(req.Err, s.run.attempts, req.MaxAttempts, moreInput, req.Cancelled)
	s.run.pendingInput = moreInput
	// The read point is a fact about what the attempt loaded: write it whether
	// or not the attempt succeeded (PG parity).
	readWatermark := s.run.readWatermark
	switch {
	case req.ReadThrough > 0:
		readWatermark = req.ReadThrough
	case req.Err == nil:
		readWatermark = req.probeAfter()
	}
	if readWatermark > s.run.readWatermark {
		s.run.readWatermark = readWatermark
	}
	if req.Cursor > s.run.cursor {
		s.run.cursor = req.Cursor
	}
	if req.Err != nil {
		s.run.lastError = req.Err.Error()
	} else {
		s.run.lastError = ""
	}
	if req.Loadout.ID != "" {
		s.run.loadoutID = req.Loadout.ID
	}
	if req.Loadout.Revision != 0 {
		s.run.loadoutRevision = req.Loadout.Revision
	}
	if req.Loadout.Generation != 0 {
		s.run.loadoutGeneration = req.Loadout.Generation
	}
	if req.Loadout.OwnerEpoch != 0 {
		s.run.loadoutOwnerEpoch = req.Loadout.OwnerEpoch
	} else {
		s.run.loadoutOwnerEpoch = req.Lease.Token.Epoch
	}
	s.lease.held = false
	s.lease.until = time.Time{}
	return nil
}

// ReleaseLease returns a claim without settling the run.
func (m *MemStore) ReleaseLease(ctx context.Context, scope Scope, token OwnerToken) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[scope]
	if !ok {
		return fmt.Errorf("memstore: session %s: %w", scope, ErrNotFound)
	}
	if err := checkToken(m.leaseStateLocked(scope, s), token); err != nil {
		return fmt.Errorf("memstore: release: %w", err)
	}
	if s.run == nil {
		return fmt.Errorf("memstore: release: %w", ErrNotFound)
	}
	// A released run keeps whatever unread input it had: the session is being
	// handed back, not consumed.
	s.run.status = RunStatusPending
	s.lease.held = false
	s.lease.until = time.Time{}
	return nil
}

// FindLease reads the fencing state.
func (m *MemStore) FindLease(ctx context.Context, scope Scope) (LeaseState, error) {
	if err := ctx.Err(); err != nil {
		return LeaseState{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[scope]
	if !ok {
		return LeaseState{}, fmt.Errorf("memstore: session %s: %w", scope, ErrNotFound)
	}
	return m.leaseStateLocked(scope, s), nil
}

// leaseStateLocked is the lease authority of one session. Like the PostgreSQL
// session row, it keeps the last owner/epoch even after the lease is cleared:
// a handoff audit reads who the previous owner was, while Found/Live stay the
// fencing decision.
func (m *MemStore) leaseStateLocked(scope Scope, s *memSession) LeaseState {
	if s == nil {
		return LeaseState{}
	}
	state := LeaseState{Lease: Lease{
		Scope: scope,
		Token: OwnerToken{Owner: s.lease.owner, Epoch: s.lease.epoch},
		Until: s.lease.until,
	}}
	if !s.lease.held {
		return state
	}
	state.Found = true
	state.Live = s.lease.until.After(m.now())
	return state
}

// memTxn is one session transaction; it holds the store lock.
type memTxn struct {
	m      *MemStore
	s      *memSession
	scope  Scope
	seq    int64
	staged []memStaged
	done   bool
}

type memStaged struct {
	kind    EventKind
	payload json.RawMessage
	dedupe  string
}

func (t *memTxn) Lease(ctx context.Context) (LeaseState, error) {
	if err := ctx.Err(); err != nil {
		return LeaseState{}, err
	}
	return t.m.leaseStateLocked(t.scope, t.s), nil
}

func (t *memTxn) ExternalState(ctx context.Context) (ExternalState, error) {
	if err := ctx.Err(); err != nil {
		return ExternalState{}, err
	}
	if t.s.external == nil {
		return ExternalState{StateOwner: StateOwnerKeel}, nil
	}
	return ExternalState{StateOwner: StateOwnerExternal, Status: t.s.external.marks.Status}, nil
}

func (t *memTxn) Dedupe(ctx context.Context, key string) (Event, bool, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, false, err
	}
	if key == "" {
		return Event{}, false, nil
	}
	return t.s.dedupeEvent(key)
}

// FindToolCall scans the in-memory log backwards for the newest assistant
// message carrying this tool call. The scan is bounded by the log itself; the
// fake exists for tests, so it does not need the PostgreSQL index.
func (t *memTxn) FindToolCall(ctx context.Context, callID string) (ToolCallRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return ToolCallRef{}, false, err
	}
	for i := len(t.s.events) - 1; i >= 0; i-- {
		ev := t.s.events[i]
		if ev.Kind != KindAssistantMessage {
			continue
		}
		calls, err := toolCallBlocks(ev.Payload)
		if err != nil {
			// A malformed payload cannot be repaired at read time; keep
			// scanning instead of failing the commit.
			continue
		}
		for _, c := range calls {
			if c.ID == callID {
				return ToolCallRef{ToolCallID: c.ID, ToolName: c.Name, Seq: ev.Seq}, true, nil
			}
		}
	}
	return ToolCallRef{}, false, nil
}

func (t *memTxn) Append(ctx context.Context, kind EventKind, payload json.RawMessage, dedupeKey string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if t.m.retryFailed > 0 {
		t.m.retryFailed--
		return 0, markRetryable(errors.New("memstore: injected transient failure"))
	}
	if t.m.failApp != nil {
		err := t.m.failApp
		t.m.failApp = nil
		return 0, err
	}
	seq := t.seq + int64(len(t.staged))
	t.staged = append(t.staged, memStaged{kind: kind, payload: append(json.RawMessage(nil), payload...), dedupe: dedupeKey})
	return seq, nil
}

func (t *memTxn) Commit(ctx context.Context) error {
	if t.done {
		return errors.New("memstore: transaction already settled")
	}
	t.done = true
	defer t.m.mu.Unlock()
	if t.m.failCommit != nil {
		err := t.m.failCommit
		t.m.failCommit = nil
		return err
	}
	for _, st := range t.staged {
		seq := t.s.nextSeq
		t.s.nextSeq++
		t.s.events = append(t.s.events, Event{Seq: seq, Kind: st.kind, Payload: st.payload})
		if st.dedupe != "" {
			t.s.dedupe[st.dedupe] = seq
		}
		if st.kind == KindUserMessage {
			// PG parity: edge input wakes the run in the same transaction as
			// the append (see pgTxn.Append); an external session is never
			// enqueued, and the wake is what marks the input unread.
			t.m.wakeRunLocked(t.s, true)
		}
	}
	return nil
}

// wakeRunLocked mirrors PG's in-transaction run wake: a run already executing
// keeps running, anything else becomes pending, and an external session gets no
// run row at all.
func (m *MemStore) wakeRunLocked(s *memSession, unreadInput bool) {
	if s.isExternal() {
		return
	}
	if s.run == nil {
		s.run = &memRun{status: RunStatusPending}
	}
	if s.run.status != RunStatusRunning {
		s.run.status = RunStatusPending
	}
	if unreadInput {
		// The input is durable: the new attempt must learn that it was not read
		// by whoever ran before (PG parity: runs.pending_input is set by the
		// edge-commit transaction only; a bare enqueue just queues).
		s.run.pendingInput = true
	}
}

func (t *memTxn) Rollback(ctx context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	t.m.mu.Unlock()
	return nil
}

// dedupeEvent resolves a namespaced dedupe key against the log.
func (s *memSession) dedupeEvent(key string) (Event, bool, error) {
	if key == "" {
		return Event{}, false, nil
	}
	seq, ok := s.dedupe[key]
	if !ok {
		return Event{}, false, nil
	}
	idx := seq - 1
	if idx < 0 || int(idx) >= len(s.events) {
		return Event{}, false, fmt.Errorf("memstore: dedupe index %d out of range", seq)
	}
	return cloneEvent(s.events[idx]), true, nil
}

func cloneEvent(ev Event) Event {
	return Event{Seq: ev.Seq, Kind: ev.Kind, Payload: append(json.RawMessage(nil), ev.Payload...)}
}
