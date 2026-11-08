package agentext

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Decision lifecycle: approval/clarify questions an agent asks a human. The
// kernel does not know about decisions (they are not event kinds); this package
// owns the primitive end to end:
//
//   - a decision has exactly one terminal state: answered, timed_out or
//     cancelled;
//   - an answer is idempotent (same answer twice is one fact, not two) and
//     conflicting answers are refused;
//   - every transition appends one journal entry, so the answer is event-ized
//     in the product layer without inventing a kernel event kind;
//   - the row is durable (projected onto the existing tasks bookkeeping table),
//     so an answer committed before a crash survives it; the *waiter* is
//     process-local by construction (the run that asked lives in this process).
type DecisionStatus string

const (
	DecisionOpen      DecisionStatus = "open"
	DecisionAnswered  DecisionStatus = "answered"
	DecisionTimedOut  DecisionStatus = "timed_out"
	DecisionCancelled DecisionStatus = "cancelled"
)

// Terminal reports whether the status is final.
func (s DecisionStatus) Terminal() bool { return s != DecisionOpen }

// DecisionOption is one selectable answer.
type DecisionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// DecisionAnswer is the recorded answer: an option for a menu, free text for a
// clarify question, or both when the decision allows free text.
type DecisionAnswer struct {
	OptionID string    `json:"optionId,omitempty"`
	Text     string    `json:"text,omitempty"`
	At       time.Time `json:"at"`
}

// Decision is the wire/state document of one approval question.
type Decision struct {
	DecisionID string `json:"decisionId"`
	TenantID   string `json:"tenantId,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	// RequestID is the opener's idempotency key: opening twice with the same
	// key returns the same decision.
	RequestID     string           `json:"requestId,omitempty"`
	Kind          string           `json:"kind"`
	Prompt        string           `json:"prompt"`
	Options       []DecisionOption `json:"options,omitempty"`
	AllowFreeText bool             `json:"allowFreeText"`
	Status        DecisionStatus   `json:"status"`
	CreatedAt     time.Time        `json:"createdAt"`
	Deadline      time.Time        `json:"deadline"`
	AnsweredAt    *time.Time       `json:"answeredAt,omitempty"`
	Answer        *DecisionAnswer  `json:"answer,omitempty"`
	// Seq is the per-session decision-journal sequence of the latest entry.
	Seq    int64           `json:"seq"`
	Events []DecisionEvent `json:"events,omitempty"`
}

// DecisionEvent is one journal entry of a decision.
type DecisionEvent struct {
	Seq        int64           `json:"seq"`
	DecisionID string          `json:"decisionId"`
	Type       string          `json:"type"`
	At         time.Time       `json:"at"`
	Answer     *DecisionAnswer `json:"answer,omitempty"`
	Detail     string          `json:"detail,omitempty"`
}

// Errors of the decision primitive. The HTTP layer maps them onto 404/409/400.
var (
	ErrDecisionNotFound = errors.New("agentext: decision not found")
	ErrDecisionClosed   = errors.New("agentext: decision is already terminal")
	ErrDecisionConflict = errors.New("agentext: decision was already answered differently")
	ErrInvalidDecision  = errors.New("agentext: invalid decision request")
)

// OpenDecisionRequest asks for a new decision.
type OpenDecisionRequest struct {
	TenantID      uuid.UUID
	SessionID     uuid.UUID
	RequestID     string
	Kind          string
	Prompt        string
	Options       []DecisionOption
	AllowFreeText bool
	// Timeout bounds the wait (default 5 minutes, capped at 24 hours).
	Timeout time.Duration
}

// AnswerDecisionRequest submits an answer.
type AnswerDecisionRequest struct {
	TenantID   uuid.UUID
	SessionID  uuid.UUID
	DecisionID uuid.UUID
	OptionID   string
	Text       string
}

// DecisionRow is the durable projection of one decision. Events is the answer
// journal; it is stored in the same row so a read reconstructs the whole
// lifecycle without a second table.
type DecisionRow struct {
	TenantID      uuid.UUID
	SessionID     uuid.UUID
	DecisionID    uuid.UUID
	RequestID     string
	Kind          string
	Prompt        string
	Options       []DecisionOption
	AllowFreeText bool
	Status        DecisionStatus
	CreatedAt     time.Time
	Deadline      time.Time
	AnsweredAt    *time.Time
	Answer        *DecisionAnswer
	Events        []DecisionEvent
}

// DecisionStore persists decision rows and their journals. Two
// implementations exist: MemoryDecisionStore (tests, and a degraded
// no-database mode) and TasksDecisionStore (PostgreSQL, over the existing
// tasks bookkeeping table).
type DecisionStore interface {
	// Create inserts a row. An existing (tenant, session, decision) is left
	// untouched; the caller reads it back to answer with the stored value.
	Create(ctx context.Context, row DecisionRow) error
	Get(ctx context.Context, tenantID, sessionID, decisionID uuid.UUID) (DecisionRow, error)
	Save(ctx context.Context, row DecisionRow) error
	List(ctx context.Context, tenantID, sessionID uuid.UUID) ([]DecisionRow, error)
}

// DecisionBrokerOptions configures the broker.
type DecisionBrokerOptions struct {
	Store DecisionStore
	Hooks *HookBus
	// DefaultTimeout applies when OpenDecisionRequest.Timeout is zero.
	DefaultTimeout time.Duration
	Logger         *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

const (
	defaultDecisionTimeout = 5 * time.Minute
	maxDecisionTimeout     = 24 * time.Hour
	decisionNamespaceSeed  = "keel.decision.v1"
	// maxDecisionsPerSession bounds the in-process answer journal lookups and
	// the waiter bookkeeping; the durable rows are unbounded but a session that
	// opens thousands of decisions is a bug.
	maxDecisionsPerSession = 512
)

// DecisionBroker implements the decision primitive: open, answer (idempotent),
// wait (bounded), timeout and cancellation.
type DecisionBroker struct {
	store   DecisionStore
	hooks   *HookBus
	log     *slog.Logger
	now     func() time.Time
	timeout time.Duration

	locks keyedMutex

	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
	timers  map[string]*time.Timer
	closed  bool
}

// NewDecisionBroker builds a broker. A nil store keeps the primitive available
// in memory (degraded: restart loses rows); production wires TasksDecisionStore.
func NewDecisionBroker(opts DecisionBrokerOptions) *DecisionBroker {
	if opts.Store == nil {
		opts.Store = NewMemoryDecisionStore()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = defaultDecisionTimeout
	}
	if opts.DefaultTimeout > maxDecisionTimeout {
		opts.DefaultTimeout = maxDecisionTimeout
	}
	b := &DecisionBroker{
		store:   opts.Store,
		hooks:   opts.Hooks,
		log:     opts.Logger,
		now:     opts.Now,
		timeout: opts.DefaultTimeout,
		locks:   keyedMutex{locks: make(map[string]*lockEntry)},
		waiters: make(map[string]map[chan struct{}]struct{}),
		timers:  make(map[string]*time.Timer),
	}
	return b
}

// DecisionIDFor derives the idempotent decision id of an opener request. The
// same (tenant, session, requestId) always names the same decision, so a retried
// open cannot duplicate the question and a restarted process can find it again.
func DecisionIDFor(tenantID, sessionID uuid.UUID, requestID string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(decisionNamespaceSeed+"|"+
		tenantID.String()+"|"+sessionID.String()+"|"+requestID))
}

func decisionKey(tenantID, sessionID, decisionID uuid.UUID) string {
	return tenantID.String() + ":" + sessionID.String() + ":" + decisionID.String()
}

// Open creates (or returns) a decision and starts its timeout. created reports
// whether this call inserted the row.
func (b *DecisionBroker) Open(ctx context.Context, req OpenDecisionRequest) (Decision, bool, error) {
	if req.TenantID == uuid.Nil || req.SessionID == uuid.Nil {
		return Decision{}, false, fmt.Errorf("%w: tenant and session are required", ErrInvalidDecision)
	}
	if req.Kind == "" {
		req.Kind = "approval"
	}
	if req.Prompt == "" && len(req.Options) == 0 {
		return Decision{}, false, fmt.Errorf("%w: prompt or options are required", ErrInvalidDecision)
	}
	seen := make(map[string]struct{}, len(req.Options))
	for _, option := range req.Options {
		if option.ID == "" {
			return Decision{}, false, fmt.Errorf("%w: every option needs an id", ErrInvalidDecision)
		}
		if _, dup := seen[option.ID]; dup {
			return Decision{}, false, fmt.Errorf("%w: duplicate option id %q", ErrInvalidDecision, option.ID)
		}
		seen[option.ID] = struct{}{}
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = b.timeout
	}
	if timeout > maxDecisionTimeout {
		timeout = maxDecisionTimeout
	}
	requestID := req.RequestID
	if requestID == "" {
		requestID = uuid.NewString()
	}
	decisionID := DecisionIDFor(req.TenantID, req.SessionID, requestID)
	key := decisionKey(req.TenantID, req.SessionID, decisionID)
	unlock := b.locks.Lock(key)
	defer unlock()

	if row, err := b.store.Get(ctx, req.TenantID, req.SessionID, decisionID); err == nil {
		// Already opened: the retry gets the stored decision (with whatever
		// terminal state it reached meanwhile).
		row = b.normalize(ctx, row)
		return rowToDecision(row), false, nil
	} else if !errors.Is(err, ErrDecisionNotFound) {
		return Decision{}, false, err
	}

	now := b.now()
	row := DecisionRow{
		TenantID:      req.TenantID,
		SessionID:     req.SessionID,
		DecisionID:    decisionID,
		RequestID:     requestID,
		Kind:          req.Kind,
		Prompt:        req.Prompt,
		Options:       append([]DecisionOption(nil), req.Options...),
		AllowFreeText: req.AllowFreeText,
		Status:        DecisionOpen,
		CreatedAt:     now,
		Deadline:      now.Add(timeout),
		Events: []DecisionEvent{{
			Seq:        1,
			DecisionID: decisionID.String(),
			Type:       "opened",
			At:         now,
		}},
	}
	if err := b.store.Create(ctx, row); err != nil {
		return Decision{}, false, err
	}
	b.scheduleTimeout(key, req.TenantID, req.SessionID, decisionID, timeout)
	decision := rowToDecision(row)
	b.emit(HookDecisionOpened, decision, "open")
	return decision, true, nil
}

// Answer records an answer idempotently. duplicate reports that this exact
// answer was already recorded (no new journal entry, no new hook).
func (b *DecisionBroker) Answer(ctx context.Context, req AnswerDecisionRequest) (Decision, bool, error) {
	if req.TenantID == uuid.Nil || req.SessionID == uuid.Nil || req.DecisionID == uuid.Nil {
		return Decision{}, false, fmt.Errorf("%w: tenant, session and decision are required", ErrInvalidDecision)
	}
	key := decisionKey(req.TenantID, req.SessionID, req.DecisionID)
	unlock := b.locks.Lock(key)
	defer unlock()

	row, err := b.store.Get(ctx, req.TenantID, req.SessionID, req.DecisionID)
	if err != nil {
		return Decision{}, false, err
	}
	row = b.normalize(ctx, row)
	switch row.Status {
	case DecisionAnswered:
		if row.Answer != nil && sameAnswer(*row.Answer, req) {
			return rowToDecision(row), true, nil
		}
		return rowToDecision(row), false, ErrDecisionConflict
	case DecisionTimedOut, DecisionCancelled:
		return rowToDecision(row), false, ErrDecisionClosed
	}

	answer := DecisionAnswer{OptionID: req.OptionID, Text: req.Text, At: b.now()}
	if answer.OptionID == "" && answer.Text == "" {
		return rowToDecision(row), false, fmt.Errorf("%w: optionId or text is required", ErrInvalidDecision)
	}
	if answer.OptionID != "" {
		found := false
		for _, option := range row.Options {
			if option.ID == answer.OptionID {
				found = true
				break
			}
		}
		if !found {
			return rowToDecision(row), false, fmt.Errorf("%w: unknown optionId %q", ErrInvalidDecision, answer.OptionID)
		}
	}
	if answer.Text != "" && !row.AllowFreeText && answer.OptionID == "" {
		return rowToDecision(row), false, fmt.Errorf("%w: free text is not allowed for this decision", ErrInvalidDecision)
	}

	row.Status = DecisionAnswered
	row.Answer = &answer
	answeredAt := answer.At
	row.AnsweredAt = &answeredAt
	row.Events = append(row.Events, DecisionEvent{
		Seq:        nextDecisionSeq(row.Events),
		DecisionID: row.DecisionID.String(),
		Type:       "answered",
		At:         answer.At,
		Answer:     &answer,
	})
	if err := b.store.Save(ctx, row); err != nil {
		return Decision{}, false, err
	}
	b.cancelTimeout(key)
	b.notifyWaiters(key)
	decision := rowToDecision(row)
	b.emit(HookDecisionAnswered, decision, "answered")
	return decision, false, nil
}

// Wait blocks until the decision reaches a terminal state or ctx ends.
func (b *DecisionBroker) Wait(ctx context.Context, tenantID, sessionID, decisionID uuid.UUID) (Decision, error) {
	key := decisionKey(tenantID, sessionID, decisionID)
	if row, err := b.store.Get(ctx, tenantID, sessionID, decisionID); err != nil {
		return Decision{}, err
	} else {
		row = b.expireRow(ctx, row)
		if row.Status.Terminal() {
			return rowToDecision(row), nil
		}
	}
	ch := b.addWaiter(key)
	defer b.removeWaiter(key, ch)
	select {
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	case <-ch:
		row, err := b.store.Get(ctx, tenantID, sessionID, decisionID)
		if err != nil {
			return Decision{}, err
		}
		return rowToDecision(b.expireRow(ctx, row)), nil
	}
}

// Get reads one decision; the second result reports existence.
func (b *DecisionBroker) Get(ctx context.Context, tenantID, sessionID, decisionID uuid.UUID) (Decision, bool) {
	row, err := b.store.Get(ctx, tenantID, sessionID, decisionID)
	if err != nil {
		return Decision{}, false
	}
	return rowToDecision(b.expireRow(ctx, row)), true
}

// List returns the session's decisions, newest first.
func (b *DecisionBroker) List(ctx context.Context, tenantID, sessionID uuid.UUID) ([]Decision, error) {
	rows, err := b.store.List(ctx, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	now := b.now()
	for i := range rows {
		if rows[i].Status == DecisionOpen && now.After(rows[i].Deadline) {
			rows[i] = b.expireRow(ctx, rows[i])
		}
	}
	out := make([]Decision, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowToDecision(row))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].DecisionID > out[j].DecisionID
	})
	if len(out) > maxDecisionsPerSession {
		out = out[:maxDecisionsPerSession]
	}
	return out, nil
}

// CancelSession cancels every open decision of a session (called by the session
// cancel endpoint). It returns how many decisions changed state.
func (b *DecisionBroker) CancelSession(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) int {
	rows, err := b.store.List(ctx, tenantID, sessionID)
	if err != nil {
		b.log.Warn("cancel session decisions", "session_id", sessionID, "err", err)
		return 0
	}
	changed := 0
	for _, candidate := range rows {
		if candidate.Status != DecisionOpen {
			continue
		}
		key := decisionKey(tenantID, sessionID, candidate.DecisionID)
		unlock := b.locks.Lock(key)
		row, err := b.store.Get(ctx, tenantID, sessionID, candidate.DecisionID)
		if err != nil || row.Status != DecisionOpen {
			unlock()
			continue
		}
		row.Status = DecisionCancelled
		row.Events = append(row.Events, DecisionEvent{
			Seq:        nextDecisionSeq(row.Events),
			DecisionID: row.DecisionID.String(),
			Type:       "cancelled",
			At:         b.now(),
			Detail:     reason,
		})
		if err := b.store.Save(ctx, row); err != nil {
			unlock()
			b.log.Warn("cancel decision", "session_id", sessionID, "decision_id", candidate.DecisionID, "err", err)
			continue
		}
		b.cancelTimeout(key)
		b.notifyWaiters(key)
		unlock()
		changed++
	}
	return changed
}

// Close stops the timers. It is idempotent; rows are left as they are.
func (b *DecisionBroker) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	timers := make([]*time.Timer, 0, len(b.timers))
	for _, timer := range b.timers {
		timers = append(timers, timer)
	}
	b.timers = make(map[string]*time.Timer)
	b.mu.Unlock()
	for _, timer := range timers {
		timer.Stop()
	}
}

// normalize applies the durable timeout rule to a row read from the store: an
// open decision past its deadline becomes timed_out. Doing it lazily on read
// makes the timeout survive a restart even when the in-process timer died.
func (b *DecisionBroker) normalize(ctx context.Context, row DecisionRow) DecisionRow {
	if row.Status != DecisionOpen || !b.now().After(row.Deadline) {
		return row
	}
	return b.expireIfDue(ctx, row, false)
}

func (b *DecisionBroker) expireRow(ctx context.Context, row DecisionRow) DecisionRow {
	return b.expireIfDue(ctx, row, true)
}

// expireIfDue applies the durable timeout rule. takeLock is false when the
// caller already holds the decision's keyed lock (the keyed mutex is not
// reentrant).
func (b *DecisionBroker) expireIfDue(ctx context.Context, row DecisionRow, takeLock bool) DecisionRow {
	key := decisionKey(row.TenantID, row.SessionID, row.DecisionID)
	if takeLock {
		unlock := b.locks.Lock(key)
		defer unlock()
	}
	current, err := b.store.Get(ctx, row.TenantID, row.SessionID, row.DecisionID)
	if err != nil {
		return row
	}
	if current.Status != DecisionOpen || !b.now().After(current.Deadline) {
		return current
	}
	current.Status = DecisionTimedOut
	current.Events = append(current.Events, DecisionEvent{
		Seq:        nextDecisionSeq(current.Events),
		DecisionID: current.DecisionID.String(),
		Type:       "timed_out",
		At:         b.now(),
		Detail:     "deadline passed",
	})
	if err := b.store.Save(ctx, current); err != nil {
		b.log.Warn("expire decision", "decision_id", current.DecisionID, "err", err)
		return current
	}
	b.cancelTimeout(key)
	b.notifyWaiters(key)
	return current
}

func (b *DecisionBroker) scheduleTimeout(key string, tenantID, sessionID, decisionID uuid.UUID, after time.Duration) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	if timer, ok := b.timers[key]; ok {
		timer.Stop()
	}
	b.timers[key] = time.AfterFunc(after, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		row, err := b.store.Get(ctx, tenantID, sessionID, decisionID)
		if err == nil {
			b.expireRow(ctx, row)
		}
		b.mu.Lock()
		delete(b.timers, key)
		b.mu.Unlock()
	})
	b.mu.Unlock()
}

func (b *DecisionBroker) cancelTimeout(key string) {
	b.mu.Lock()
	timer := b.timers[key]
	delete(b.timers, key)
	b.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
}

func (b *DecisionBroker) addWaiter(key string) chan struct{} {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if b.waiters[key] == nil {
		b.waiters[key] = make(map[chan struct{}]struct{})
	}
	b.waiters[key][ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *DecisionBroker) removeWaiter(key string, ch chan struct{}) {
	b.mu.Lock()
	delete(b.waiters[key], ch)
	if len(b.waiters[key]) == 0 {
		delete(b.waiters, key)
	}
	b.mu.Unlock()
}

func (b *DecisionBroker) notifyWaiters(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.waiters[key] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (b *DecisionBroker) emit(event HookEvent, decision Decision, status string) {
	if b.hooks == nil {
		return
	}
	payload := HookPayload{
		Event:      event,
		TenantID:   decision.TenantID,
		SessionID:  decision.SessionID,
		DecisionID: decision.DecisionID,
		RequestID:  decision.RequestID,
		Status:     status,
		At:         b.now(),
	}
	payload.SetData("kind", decision.Kind)
	b.hooks.Emit(payload)
}

func nextDecisionSeq(events []DecisionEvent) int64 {
	if len(events) == 0 {
		return 1
	}
	return events[len(events)-1].Seq + 1
}

func sameAnswer(answer DecisionAnswer, req AnswerDecisionRequest) bool {
	return answer.OptionID == req.OptionID && answer.Text == req.Text
}

func rowToDecision(row DecisionRow) Decision {
	decision := Decision{
		DecisionID:    row.DecisionID.String(),
		TenantID:      row.TenantID.String(),
		SessionID:     row.SessionID.String(),
		RequestID:     row.RequestID,
		Kind:          row.Kind,
		Prompt:        row.Prompt,
		Options:       append([]DecisionOption(nil), row.Options...),
		AllowFreeText: row.AllowFreeText,
		Status:        row.Status,
		CreatedAt:     row.CreatedAt,
		Deadline:      row.Deadline,
		AnsweredAt:    row.AnsweredAt,
		Answer:        row.Answer,
		Events:        append([]DecisionEvent(nil), row.Events...),
	}
	if len(row.Events) > 0 {
		decision.Seq = row.Events[len(row.Events)-1].Seq
	}
	return decision
}

// keyedMutex serializes work per logical key without one global lock. Entries
// are dropped when the last holder unlocks.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

// Lock locks key and returns the unlock function.
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	entry := k.locks[key]
	if entry == nil {
		entry = &lockEntry{}
		k.locks[key] = entry
	}
	entry.refs++
	k.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
