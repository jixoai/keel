package agentext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// SessionGate is the narrow session seam the product layer needs: create an
// idempotent session row and classify it (external sessions never enter the
// native turn path). *store.Store satisfies both methods.
type SessionGate interface {
	CreateSession(ctx context.Context, tenantID, sessionID uuid.UUID) error
	SessionOwner(ctx context.Context, tenantID, sessionID uuid.UUID) (string, bool, error)
}

// RunState is the run-row projection the product layer reads (status only; the
// event log remains the truth).
type RunState struct {
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	CursorSeq int64  `json:"cursorSeq"`
	LastError string `json:"lastError,omitempty"`
}

// RunStatusReader reads the run-row mirror.
type RunStatusReader interface {
	RunState(ctx context.Context, tenantID, sessionID uuid.UUID) (RunState, error)
}

// ErrNoRun reports that the session has no run row (nothing was ever queued).
var ErrNoRun = errors.New("agentext: session has no run row")

// ErrCancelUnavailable reports that the cancel seam cannot reach the attempt
// (a run executing in another process, or no RunControl wiring at all).
var ErrCancelUnavailable = errors.New("agentext: cancel seam is not wired for this run")

// StartTurnRequest is one product-initiated turn: a user.message committed
// through the kernel's edge seam (which also wakes the run in the same
// transaction) plus the optional session creation that precedes it.
type StartTurnRequest struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	Text      string
	// RequestID is the idempotency key of the turn (kernel KeyRequest). A
	// repeated start with the same key reuses the committed message.
	RequestID string
	// SessionID may be zero: a fresh session id is generated and returned.
	// Extras are merged into the user.message payload (e.g. sub-agent marks).
	Extras map[string]any
}

// StartTurnResult reports where the turn landed.
type StartTurnResult struct {
	TenantID  uuid.UUID `json:"-"`
	SessionID uuid.UUID `json:"sessionId"`
	Seq       int64     `json:"seq"`
	RequestID string    `json:"requestId"`
	Deduped   bool      `json:"deduped"`
}

// TurnStarter starts a turn through the kernel edge seam. This is the one code
// path triggers, sub-agents and the manual fire endpoint share: the durable log
// is the only truth and the kernel's requestId dedupe is the durable
// idempotency key.
type TurnStarter struct {
	Gate  SessionGate
	Edge  kernel.Committer
	Hooks *HookBus
	Log   *slog.Logger
	Now   func() time.Time
}

// NewTurnStarter validates the inputs. Gate and Edge are required.
func NewTurnStarter(gate SessionGate, edge kernel.Committer, hooks *HookBus, log *slog.Logger) *TurnStarter {
	if log == nil {
		log = slog.Default()
	}
	return &TurnStarter{Gate: gate, Edge: edge, Hooks: hooks, Log: log,
		Now: func() time.Time { return time.Now().UTC() }}
}

// StartTurn commits the user.message (edge class, idempotent by requestId).
func (s *TurnStarter) StartTurn(ctx context.Context, req StartTurnRequest) (StartTurnResult, error) {
	if s.Gate == nil || s.Edge == nil {
		return StartTurnResult{}, fmt.Errorf("%w: turn starter needs the session gate and the kernel commit seam", ErrCancelUnavailable)
	}
	if req.TenantID == uuid.Nil {
		return StartTurnResult{}, errors.New("agentext: tenant is required")
	}
	if req.Text == "" {
		return StartTurnResult{}, errors.New("agentext: turn text is required")
	}
	requestID := req.RequestID
	if requestID == "" {
		requestID = uuid.NewString()
	}
	sessionID := req.SessionID
	if sessionID == uuid.Nil {
		sessionID = uuid.New()
	}
	if err := s.Gate.CreateSession(ctx, req.TenantID, sessionID); err != nil {
		return StartTurnResult{}, fmt.Errorf("agentext: create session: %w", err)
	}
	payload := map[string]any{"text": req.Text, "requestId": requestID}
	for key, value := range req.Extras {
		payload[key] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return StartTurnResult{}, fmt.Errorf("agentext: encode turn: %w", err)
	}
	receipt, err := s.Edge.Commit(ctx, kernel.CommitRequest{
		Scope:      kernel.Scope{TenantID: req.TenantID, SessionID: sessionID},
		Kind:       kernel.KindUserMessage,
		RequestID:  requestID,
		Payload:    raw,
		NativeEdge: true,
	})
	if err != nil {
		return StartTurnResult{}, err
	}
	if s.Hooks != nil {
		payload := HookPayload{
			Event:     HookTurnStarted,
			TenantID:  req.TenantID.String(),
			SessionID: sessionID.String(),
			RequestID: requestID,
			Status:    "started",
			At:        s.Now(),
		}
		payload.SetData("seq", fmt.Sprintf("%d", receipt.Seq))
		s.Hooks.Emit(payload)
	}
	return StartTurnResult{
		TenantID:  req.TenantID,
		SessionID: sessionID,
		Seq:       receipt.Seq,
		RequestID: requestID,
		Deduped:   receipt.Deduped,
	}, nil
}

// --- run control ---------------------------------------------------------

// CancelResult is the idempotent answer of a cancel request.
type CancelResult struct {
	// Status is one of: cancelled (a local attempt was stopped), cancelling
	// (cancel signalled, settlement still in flight), settled (no active
	// attempt: the durable run already reached a terminal state), idle (nothing
	// is running), not_wired (the attempt is not reachable from this process).
	Status    string `json:"status"`
	RunStatus string `json:"runStatus,omitempty"`
	Cancelled bool   `json:"cancelled"`
	Detail    string `json:"detail,omitempty"`
}

type localRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

type overlayEntry struct {
	status string
	at     time.Time
}

const (
	maxOverlayEntries = 2048
	cancelWait        = 5 * time.Second
)

// RunControl is the in-process cancellation seam.
//
// The kernel has no "cancel" lease verb: a run is stopped by the context that
// executes it. Whoever executes a run calls BeginRun, which returns a context
// Cancel will cancel and a finish func to call when the attempt settles. The
// HTTP cancel endpoint therefore answers truthfully:
//
//   - local attempt: cancel it and wait (bounded) for settlement;
//   - durable terminal run: settled (idempotent no-op);
//   - queued/pending run: idle (there is no active attempt to stop);
//   - running elsewhere: 501 (ErrCancelUnavailable) — never a fake success.
//
// The orchestrator's worker integration is one line in its claim loop
// (ctx, finish := runControl.BeginRun(ctx, lease.Scope, runID)); the seam stays
// opt-in so the product layer never guesses.
type RunControl struct {
	Runs RunStatusReader
	Log  *slog.Logger
	Now  func() time.Time

	mu      sync.Mutex
	local   map[kernel.Scope]*localRun
	overlay map[kernel.Scope]overlayEntry
	order   []kernel.Scope
}

// NewRunControl builds the seam. Runs may be nil (the cancel endpoint then
// answers 501 for everything except local runs).
func NewRunControl(runs RunStatusReader, log *slog.Logger) *RunControl {
	if log == nil {
		log = slog.Default()
	}
	return &RunControl{
		Runs:    runs,
		Log:     log,
		Now:     func() time.Time { return time.Now().UTC() },
		local:   make(map[kernel.Scope]*localRun),
		overlay: make(map[kernel.Scope]overlayEntry),
	}
}

// BeginRun registers a locally executing run. The returned context is cancelled
// by Cancel; finish must be called exactly once when the attempt settles (it is
// idempotent and reports the final outcome).
func (c *RunControl) BeginRun(parent context.Context, scope kernel.Scope, runID string) (context.Context, func(outcome string)) {
	// The run scope travels with the run context, so a model tool executing
	// inside this attempt can attribute its work (spawn_subagent) without any
	// extra worker plumbing.
	ctx, cancel := context.WithCancel(WithRunScope(parent, RunScope{
		TenantID: scope.TenantID, SessionID: scope.SessionID, RunID: runID,
	}))
	entry := &localRun{cancel: cancel, done: make(chan struct{})}
	c.mu.Lock()
	if previous := c.local[scope]; previous != nil {
		// A newer attempt replaced an old registration (should not happen with
		// the lease fence, but never leak the old one).
		previous.once.Do(func() { close(previous.done) })
	}
	c.local[scope] = entry
	delete(c.overlay, scope)
	c.mu.Unlock()

	finish := func(outcome string) {
		entry.once.Do(func() {
			c.mu.Lock()
			if c.local[scope] == entry {
				delete(c.local, scope)
			}
			c.setOverlayLocked(scope, outcome)
			c.mu.Unlock()
			close(entry.done)
		})
	}
	return ctx, finish
}

// Cancel stops the local attempt if this process owns it, otherwise reports the
// durable run state.
func (c *RunControl) Cancel(ctx context.Context, scope kernel.Scope) (CancelResult, error) {
	c.mu.Lock()
	entry := c.local[scope]
	c.mu.Unlock()
	if entry != nil {
		entry.cancel()
		timer := time.NewTimer(cancelWait)
		defer timer.Stop()
		select {
		case <-entry.done:
			c.mu.Lock()
			c.setOverlayLocked(scope, "cancelled")
			c.mu.Unlock()
			return CancelResult{Status: "cancelled", RunStatus: "cancelled", Cancelled: true,
				Detail: "the in-process attempt was cancelled; committed events are kept"}, nil
		case <-ctx.Done():
			return CancelResult{Status: "cancelling", Cancelled: true,
				Detail: "cancel signalled; settlement is still in flight"}, nil
		case <-timer.C:
			return CancelResult{Status: "cancelling", Cancelled: true,
				Detail: "cancel signalled; the attempt has not settled yet"}, nil
		}
	}
	// A cancel this process already performed is an idempotent success even
	// after the attempt settled (the kernel policy would report "failed").
	if status, ok := c.Status(scope); ok && status == "cancelled" {
		return CancelResult{Status: "cancelled", RunStatus: "cancelled", Cancelled: true,
			Detail: "the run was already cancelled; committed events are kept"}, nil
	}
	if c.Runs == nil {
		return CancelResult{Status: "not_wired"}, ErrCancelUnavailable
	}
	state, err := c.Runs.RunState(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		if errors.Is(err, ErrNoRun) {
			return CancelResult{Status: "idle", Detail: "no run has been started for this session"}, nil
		}
		return CancelResult{}, err
	}
	switch state.Status {
	case "done", "failed", "cancelled":
		return CancelResult{Status: "settled", RunStatus: state.Status, Cancelled: false,
			Detail: "the run already reached a terminal state"}, nil
	case "pending":
		return CancelResult{Status: "idle", RunStatus: "pending",
			Detail: "the run is queued but has no active attempt; the input stays unanswered until a cancel seam is wired"}, nil
	case "running":
		return CancelResult{Status: "not_wired", RunStatus: "running"}, ErrCancelUnavailable
	default:
		return CancelResult{Status: "idle", RunStatus: state.Status}, nil
	}
}

// Status reports the product-layer override of the durable run status (a run
// this process cancelled settles as failed in the kernel's policy table; the
// product snapshot remembers the cancellation).
func (c *RunControl) Status(scope kernel.Scope) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	overlay, ok := c.overlay[scope]
	return overlay.status, ok
}

func (c *RunControl) setOverlayLocked(scope kernel.Scope, status string) {
	if _, exists := c.overlay[scope]; !exists {
		c.order = append(c.order, scope)
	}
	c.overlay[scope] = overlayEntry{status: status, at: c.Now()}
	if len(c.order) > maxOverlayEntries {
		drop := len(c.order) - maxOverlayEntries
		for _, old := range c.order[:drop] {
			delete(c.overlay, old)
		}
		c.order = append([]kernel.Scope(nil), c.order[drop:]...)
	}
}

// --- partials ------------------------------------------------------------

// Partial is one token-level frame. It is a transport fact only: it is never
// stored, never replayed and never carries an SSE id, so it cannot move a
// client's Last-Event-ID cursor.
type Partial struct {
	Type      string    `json:"type"`
	MessageID string    `json:"messageId,omitempty"`
	Delta     string    `json:"delta,omitempty"`
	Seq       int64     `json:"seq,omitempty"`
	At        time.Time `json:"at"`
}

// PartialHub is the in-process broadcast bus of partial frames. Subscribers get
// a bounded channel; a slow subscriber drops frames instead of blocking the
// producer (partials are advisory by contract).
type PartialHub struct {
	mu   sync.Mutex
	subs map[string]map[*partialSub]struct{}

	published atomic.Int64
	dropped   atomic.Int64
	now       func() time.Time
}

type partialSub struct {
	ch chan Partial
}

const partialBuffer = 16

// NewPartialHub builds an empty hub.
func NewPartialHub() *PartialHub {
	return &PartialHub{subs: make(map[string]map[*partialSub]struct{}),
		now: func() time.Time { return time.Now().UTC() }}
}

func partialKey(tenantID, sessionID uuid.UUID) string {
	return tenantID.String() + ":" + sessionID.String()
}

// Publish broadcasts one partial to the session's subscribers.
func (h *PartialHub) Publish(tenantID, sessionID uuid.UUID, partial Partial) bool {
	if partial.At.IsZero() {
		partial.At = h.now()
	}
	h.published.Add(1)
	h.mu.Lock()
	defer h.mu.Unlock()
	subs := h.subs[partialKey(tenantID, sessionID)]
	delivered := false
	for sub := range subs {
		select {
		case sub.ch <- partial:
			delivered = true
		default:
			h.dropped.Add(1)
		}
	}
	return delivered
}

// Subscribe returns the session's partial channel and a cancel function.
func (h *PartialHub) Subscribe(tenantID, sessionID uuid.UUID) (<-chan Partial, func()) {
	sub := &partialSub{ch: make(chan Partial, partialBuffer)}
	key := partialKey(tenantID, sessionID)
	h.mu.Lock()
	if h.subs[key] == nil {
		h.subs[key] = make(map[*partialSub]struct{})
	}
	h.subs[key][sub] = struct{}{}
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		if set := h.subs[key]; set != nil {
			delete(set, sub)
			if len(set) == 0 {
				delete(h.subs, key)
			}
		}
		h.mu.Unlock()
	}
	return sub.ch, cancel
}

// Stats reports published/dropped frame counters and the subscriber count.
func (h *PartialHub) Stats() (published, dropped int64, subscribers int) {
	h.mu.Lock()
	subs := 0
	for _, set := range h.subs {
		subs += len(set)
	}
	h.mu.Unlock()
	return h.published.Load(), h.dropped.Load(), subs
}
