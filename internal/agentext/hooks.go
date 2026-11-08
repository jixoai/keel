package agentext

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// HookEvent is one name of the first-phase hook closed set. The set only grows
// (additive changes are compatible; renaming or removing is a breaking change
// and needs a new event name).
type HookEvent string

const (
	HookSubagentStarted  HookEvent = "subagent.started"
	HookSubagentFinished HookEvent = "subagent.finished"
	HookTriggerBefore    HookEvent = "trigger.before"
	HookTriggerAfter     HookEvent = "trigger.after"
	HookTurnStarted      HookEvent = "turn.started"
	HookTurnFinished     HookEvent = "turn.finished"
	HookDecisionOpened   HookEvent = "decision.opened"
	HookDecisionAnswered HookEvent = "decision.answered"
)

// HookEvents returns the closed set in declaration order.
func HookEvents() []HookEvent {
	return []HookEvent{
		HookSubagentStarted,
		HookSubagentFinished,
		HookTriggerBefore,
		HookTriggerAfter,
		HookTurnStarted,
		HookTurnFinished,
		HookDecisionOpened,
		HookDecisionAnswered,
	}
}

// ValidHookEvent reports whether name belongs to the closed set.
func ValidHookEvent(name string) bool {
	for _, event := range HookEvents() {
		if string(event) == name {
			return true
		}
	}
	return false
}

// HookPayload is the document a hook handler receives. It carries identifiers
// only: tenant/session/run plus the related trigger/decision/subagent ids. It is
// built from enum/status/identifier fields, so it can never carry a transcript
// body, a secret or a credential. Data is reserved for short scalar metadata
// (bounded by DataValueLimit at emit time).
type HookPayload struct {
	Event             HookEvent         `json:"event"`
	TenantID          string            `json:"tenantId"`
	SessionID         string            `json:"sessionId,omitempty"`
	RunID             string            `json:"runId,omitempty"`
	ParentSessionID   string            `json:"parentSessionId,omitempty"`
	SubagentSessionID string            `json:"subagentSessionId,omitempty"`
	TriggerID         string            `json:"triggerId,omitempty"`
	DecisionID        string            `json:"decisionId,omitempty"`
	RequestID         string            `json:"requestId,omitempty"`
	Status            string            `json:"status,omitempty"`
	At                time.Time         `json:"at"`
	Data              map[string]string `json:"data,omitempty"`
}

// DataValueLimit bounds every Data value so a hook payload stays small and
// cannot smuggle a body through the metadata channel.
const DataValueLimit = 256

// SetData writes one bounded scalar into the payload metadata.
func (p *HookPayload) SetData(key, value string) {
	if key == "" || value == "" {
		return
	}
	if len(value) > DataValueLimit {
		value = value[:DataValueLimit]
	}
	if p.Data == nil {
		p.Data = make(map[string]string, 2)
	}
	p.Data[key] = value
}

// HookHandler handles one event. It runs on the hook worker pool; the payload is
// immutable. A returned error is retried once and then recorded.
type HookHandler func(ctx context.Context, payload HookPayload) error

// HookOptions configures one subscription.
type HookOptions struct {
	Event   HookEvent
	Handler HookHandler
	// Timeout bounds one handler invocation (default 5s).
	Timeout time.Duration
}

// HookStat is the observable state of one subscription. A failed hook is
// visible here (and in the log) without failing the ledger that triggered it.
type HookStat struct {
	Owner     string     `json:"owner"`
	Event     HookEvent  `json:"event"`
	Handler   string     `json:"handler"`
	Invoked   int64      `json:"invoked"`
	Failed    int64      `json:"failed"`
	Retries   int64      `json:"retries"`
	LastError string     `json:"lastError,omitempty"`
	LastAt    *time.Time `json:"lastAt,omitempty"`
	Timeout   string     `json:"timeout"`
}

// HookBusOptions configures the bus.
type HookBusOptions struct {
	// Queue is the dispatch queue depth (default 1024). Emit is non-blocking:
	// a full queue drops and counts, it never applies back-pressure to the
	// ledger.
	Queue int
	// Workers is the dispatch concurrency (default 2).
	Workers int
	// RetryDelay is the pause before the single retry of a failed handler
	// (default 25ms).
	RetryDelay time.Duration
	Logger     *slog.Logger
}

// maxHookRetries is the bounded retry budget of one handler invocation. Hooks
// are advisory: retrying forever would turn a broken plugin into an unbounded
// background load, so one retry is the contract.
const maxHookRetries = 1

type hookSubscription struct {
	owner   string
	name    string
	event   HookEvent
	handler HookHandler
	timeout time.Duration

	invoked atomic.Int64
	failed  atomic.Int64
	retries atomic.Int64

	mu        sync.Mutex
	lastError string
	lastAt    time.Time
}

func (s *hookSubscription) recordError(err error, at time.Time) {
	s.failed.Add(1)
	s.mu.Lock()
	s.lastError = err.Error()
	s.lastAt = at
	s.mu.Unlock()
}

func (s *hookSubscription) recordSuccess(at time.Time) {
	s.mu.Lock()
	s.lastAt = at
	s.mu.Unlock()
}

func (s *hookSubscription) stat() HookStat {
	s.mu.Lock()
	lastError, lastAt := s.lastError, s.lastAt
	s.mu.Unlock()
	var lastAtPtr *time.Time
	if !lastAt.IsZero() {
		at := lastAt
		lastAtPtr = &at
	}
	return HookStat{
		Owner:     s.owner,
		Event:     s.event,
		Handler:   s.name,
		Invoked:   s.invoked.Load(),
		Failed:    s.failed.Load(),
		Retries:   s.retries.Load(),
		LastError: lastError,
		LastAt:    lastAtPtr,
		Timeout:   s.timeout.String(),
	}
}

// HookBus fans out hook payloads to declarative subscriptions. Dispatch is
// asynchronous and bounded: Emit never blocks the caller (the ledger commit
// path), a full queue is visible as dropped events, and handler failures are
// per-subscription counters with the last error.
type HookBus struct {
	log        *slog.Logger
	queue      chan job
	retryDelay time.Duration

	mu      sync.RWMutex
	subs    map[HookEvent][]*hookSubscription
	nextID  uint64
	closed  bool
	workers sync.WaitGroup

	dropped   atomic.Int64
	emitted   atomic.Int64
	delivered atomic.Int64

	ctx    context.Context
	cancel context.CancelFunc
}

type job struct {
	payload  HookPayload
	enqueued time.Time
}

// NewHookBus starts the dispatch workers. Call Close to stop them.
func NewHookBus(opts HookBusOptions) *HookBus {
	if opts.Queue <= 0 {
		opts.Queue = 1024
	}
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.RetryDelay <= 0 {
		opts.RetryDelay = 25 * time.Millisecond
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &HookBus{
		log:        opts.Logger,
		queue:      make(chan job, opts.Queue),
		retryDelay: opts.RetryDelay,
		subs:       make(map[HookEvent][]*hookSubscription),
		ctx:        ctx,
		cancel:     cancel,
	}
	for i := 0; i < opts.Workers; i++ {
		b.workers.Add(1)
		go b.work()
	}
	return b
}

// Subscribe registers a handler. The returned function removes the
// subscription; it is safe to call more than once. An invalid event name or a
// nil handler is an error (the closed set is part of the contract).
func (b *HookBus) Subscribe(owner string, opts HookOptions) (func(), error) {
	if opts.Handler == nil {
		return nil, fmt.Errorf("agentext: hook handler is required")
	}
	if !ValidHookEvent(string(opts.Event)) {
		return nil, fmt.Errorf("agentext: %q is not in the first-phase hook set", opts.Event)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, fmt.Errorf("agentext: hook bus is closed")
	}
	b.nextID++
	sub := &hookSubscription{
		owner:   owner,
		name:    fmt.Sprintf("%s#%d", owner, b.nextID),
		event:   opts.Event,
		handler: opts.Handler,
		timeout: opts.Timeout,
	}
	b.subs[opts.Event] = append(b.subs[opts.Event], sub)
	b.mu.Unlock()

	var once sync.Once
	remove := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			list := b.subs[opts.Event]
			for i, candidate := range list {
				if candidate == sub {
					b.subs[opts.Event] = append(list[:i:i], list[i+1:]...)
					break
				}
			}
			if len(b.subs[opts.Event]) == 0 {
				delete(b.subs, opts.Event)
			}
		})
	}
	return remove, nil
}

// Emit queues one payload for asynchronous delivery. It never blocks: a full
// queue drops the payload and increments the dropped counter (visible through
// Stats). It reports whether the payload was queued.
func (b *HookBus) Emit(payload HookPayload) bool {
	if payload.Event == "" {
		return false
	}
	if payload.At.IsZero() {
		payload.At = time.Now().UTC()
	}
	b.emitted.Add(1)
	select {
	case b.queue <- job{payload: payload, enqueued: time.Now()}:
		return true
	default:
		n := b.dropped.Add(1)
		// Log the first few drops loudly, then sample: a broken hook consumer
		// must not turn into a log flood.
		if n <= 5 || n%1000 == 0 {
			b.log.Warn("hook queue full, dropping payload", "event", payload.Event,
				"session_id", payload.SessionID, "trigger_id", payload.TriggerID, "dropped", n)
		}
		return false
	}
}

func (b *HookBus) work() {
	defer b.workers.Done()
	for {
		select {
		case <-b.ctx.Done():
			return
		case j := <-b.queue:
			b.dispatch(j.payload)
		}
	}
}

func (b *HookBus) dispatch(payload HookPayload) {
	b.mu.RLock()
	list := b.subs[payload.Event]
	subs := append([]*hookSubscription(nil), list...)
	b.mu.RUnlock()
	for _, sub := range subs {
		b.invoke(sub, payload)
	}
}

func (b *HookBus) invoke(sub *hookSubscription, payload HookPayload) {
	sub.invoked.Add(1)
	var lastErr error
	for attempt := 0; attempt <= maxHookRetries; attempt++ {
		if attempt > 0 {
			sub.retries.Add(1)
			select {
			case <-b.ctx.Done():
				return
			case <-time.After(b.retryDelay):
			}
		}
		ctx, cancel := context.WithTimeout(b.ctx, sub.timeout)
		err := sub.handler(ctx, payload)
		cancel()
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
	}
	now := time.Now().UTC()
	b.delivered.Add(1)
	if lastErr != nil {
		sub.recordError(lastErr, now)
		b.log.Warn("hook failed", "event", payload.Event, "owner", sub.owner,
			"session_id", payload.SessionID, "trigger_id", payload.TriggerID, "err", lastErr)
		return
	}
	sub.recordSuccess(now)
}

// Stats returns every subscription's counters sorted by owner+event.
func (b *HookBus) Stats() []HookStat {
	b.mu.RLock()
	out := make([]HookStat, 0, len(b.subs))
	for _, list := range b.subs {
		for _, sub := range list {
			out = append(out, sub.stat())
		}
	}
	b.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}
		if out[i].Event != out[j].Event {
			return out[i].Event < out[j].Event
		}
		return out[i].Handler < out[j].Handler
	})
	return out
}

// StatsSummary is the aggregate hook health exposed by GET /v1/capabilities and
// GET /v1/plugins.
type StatsSummary struct {
	Subscriptions int   `json:"subscriptions"`
	Emitted       int64 `json:"emitted"`
	Delivered     int64 `json:"delivered"`
	Dropped       int64 `json:"dropped"`
	Failed        int64 `json:"failed"`
	Queued        int   `json:"queued"`
	Capacity      int   `json:"capacity"`
}

// Summary aggregates the bus counters.
func (b *HookBus) Summary() StatsSummary {
	b.mu.RLock()
	subs := 0
	for _, list := range b.subs {
		subs += len(list)
	}
	b.mu.RUnlock()
	var failed int64
	for _, stat := range b.Stats() {
		failed += stat.Failed
	}
	return StatsSummary{
		Subscriptions: subs,
		Emitted:       b.emitted.Load(),
		Delivered:     b.delivered.Load(),
		Dropped:       b.dropped.Load(),
		Failed:        failed,
		Queued:        len(b.queue),
		Capacity:      cap(b.queue),
	}
}

// Close stops the workers. It does not wait for queued jobs: a hook is
// advisory, and shutdown must not be held hostage by a slow plugin. It is
// idempotent.
func (b *HookBus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	b.workers.Wait()
}
