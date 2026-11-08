package agentext

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// Triggers turn a schedule or an external webhook into a turn. They are product
// policy: the kernel never sees a trigger. Three properties are contractual:
//
//   - idempotency: every fire has a key. The key selects the target session
//     (deterministically, when the trigger does not pin one) and the
//     user.message requestId, so a retried fire — even after a restart — reuses
//     the committed message instead of starting a second turn;
//   - audit: every fire is recorded (when, why, target session, sequence,
//     attempt count, error) and the committed message payload carries the same
//     provenance durably;
//   - visible failure: an exhausted retry budget is recorded on the trigger and
//     returned to the caller; nothing is silently dropped.
type TriggerKind string

const (
	TriggerInterval TriggerKind = "interval"
	TriggerCron     TriggerKind = "cron"
	TriggerWebhook  TriggerKind = "webhook"
)

// TriggerSpec is the declarative form (the wire form of GET /v1/triggers).
type TriggerSpec struct {
	ID string `json:"id"`
	// Kind is interval, cron or webhook.
	Kind TriggerKind `json:"kind"`
	// Schedule is a Go duration ("5m") for interval and a 5-field cron
	// expression ("*/5 * * * *") for cron.
	Schedule string `json:"schedule,omitempty"`
	// Prompt is the user message a fire commits.
	Prompt string `json:"prompt"`
	// TenantID owns the turns a scheduled fire starts (manual/webhook fires
	// inherit the caller's tenant).
	TenantID string `json:"tenantId,omitempty"`
	// SessionID pins the target session. Empty means "one deterministic
	// session per idempotency key" (a fresh session per trigger window).
	SessionID string `json:"sessionId,omitempty"`
	Enabled   bool   `json:"enabled"`
	// Owner names the plugin that declared the trigger ("" = builtin).
	Owner string `json:"owner,omitempty"`
	// MaxAttempts bounds one fire (default 3).
	MaxAttempts int `json:"maxAttempts,omitempty"`
	// RetryBackoff is the pause before retry N+1 (default 250ms, linear).
	RetryBackoffMs int64 `json:"retryBackoffMs,omitempty"`
}

// FireRequest is one manual or webhook invocation.
type FireRequest struct {
	// IdempotencyKey is required for webhooks and optional for a manual fire
	// (a generated key is reported back; retries of a manual fire must send
	// their own key).
	IdempotencyKey string
	// Reason is recorded in the audit ("manual" or "webhook").
	Reason string
}

// FireResult is the idempotent answer of a fire.
type FireResult struct {
	TriggerID      string `json:"triggerId"`
	Status         string `json:"status"`
	IdempotencyKey string `json:"idempotencyKey"`
	SessionID      string `json:"sessionId,omitempty"`
	Seq            int64  `json:"seq,omitempty"`
	Deduped        bool   `json:"deduped"`
	Attempts       int    `json:"attempts"`
	Error          string `json:"error,omitempty"`
}

// AuditEntry is one recorded fire.
type AuditEntry struct {
	At             time.Time `json:"at"`
	TriggerID      string    `json:"triggerId"`
	Reason         string    `json:"reason"`
	IdempotencyKey string    `json:"idempotencyKey"`
	SessionID      string    `json:"sessionId,omitempty"`
	Seq            int64     `json:"seq,omitempty"`
	Status         string    `json:"status"`
	Attempts       int       `json:"attempts"`
	Error          string    `json:"error,omitempty"`
}

// TriggerStatus is the observable state of one trigger.
type TriggerStatus struct {
	TriggerSpec
	NextFireAt *time.Time   `json:"nextFireAt,omitempty"`
	LastFireAt *time.Time   `json:"lastFireAt,omitempty"`
	LastStatus string       `json:"lastStatus,omitempty"`
	Fires      int64        `json:"fires"`
	Failures   int64        `json:"failures"`
	Audit      []AuditEntry `json:"audit,omitempty"`
}

// Trigger errors. The HTTP layer maps them onto 404/400/409/502.
var (
	ErrTriggerNotFound    = errors.New("agentext: trigger not found")
	ErrTriggerDisabled    = errors.New("agentext: trigger is disabled")
	ErrTriggerKeyRequired = errors.New("agentext: an idempotency key is required")
	ErrInvalidTrigger     = errors.New("agentext: invalid trigger")
	ErrTriggerStartFailed = errors.New("agentext: trigger start failed")
)

// TriggerEngineOptions configures the engine.
type TriggerEngineOptions struct {
	// Starter starts a turn (TurnStarter in production).
	Starter         turnStarter
	Hooks           *HookBus
	Logger          *slog.Logger
	Now             func() time.Time
	DefaultAttempts int
	DefaultBackoff  time.Duration
}

// turnStarter is the narrow start seam of the trigger engine (satisfied by
// *TurnStarter). It is an interface so tests can count starts without a kernel.
type turnStarter interface {
	StartTurn(ctx context.Context, req StartTurnRequest) (StartTurnResult, error)
}

const (
	defaultTriggerAttempts = 3
	defaultTriggerBackoff  = 250 * time.Millisecond
	maxFireHistory         = 4096
	maxAuditPerTrigger     = 20
)

// TriggerEngine owns interval/cron/webhook triggers and their audit trail.
type TriggerEngine struct {
	starter         turnStarter
	hooks           *HookBus
	log             *slog.Logger
	now             func() time.Time
	defaultAttempts int
	defaultBackoff  time.Duration

	locks keyedMutex

	mu       sync.Mutex
	triggers map[string]*triggerRuntime
	fired    map[string]FireResult
	order    []string
	closed   bool

	rootCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

type triggerRuntime struct {
	spec     TriggerSpec
	interval time.Duration
	cron     *cronSchedule

	stop     chan struct{}
	stopOnce sync.Once

	mu         sync.Mutex
	nextFire   time.Time
	lastFireAt time.Time
	lastStatus string
	fires      int64
	failures   int64
	audit      []AuditEntry
}

// NewTriggerEngine builds the engine. Call Close to stop the schedulers.
func NewTriggerEngine(opts TriggerEngineOptions) *TriggerEngine {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.DefaultAttempts <= 0 {
		opts.DefaultAttempts = defaultTriggerAttempts
	}
	if opts.DefaultBackoff <= 0 {
		opts.DefaultBackoff = defaultTriggerBackoff
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &TriggerEngine{
		starter:         opts.Starter,
		hooks:           opts.Hooks,
		log:             opts.Logger,
		now:             opts.Now,
		defaultAttempts: opts.DefaultAttempts,
		defaultBackoff:  opts.DefaultBackoff,
		locks:           keyedMutex{locks: make(map[string]*lockEntry)},
		triggers:        make(map[string]*triggerRuntime),
		fired:           make(map[string]FireResult),
		rootCtx:         ctx,
		cancel:          cancel,
	}
}

// Register adds one trigger and starts its scheduler when it is enabled and
// scheduled. The returned function removes it.
func (e *TriggerEngine) Register(spec TriggerSpec) (func(), error) {
	if spec.ID == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidTrigger)
	}
	switch spec.Kind {
	case TriggerInterval, TriggerCron, TriggerWebhook:
	default:
		return nil, fmt.Errorf("%w: unknown kind %q", ErrInvalidTrigger, spec.Kind)
	}
	if strings.TrimSpace(spec.Prompt) == "" {
		return nil, fmt.Errorf("%w: prompt is required", ErrInvalidTrigger)
	}
	rt := &triggerRuntime{spec: spec, stop: make(chan struct{})}
	switch spec.Kind {
	case TriggerInterval:
		interval, err := time.ParseDuration(spec.Schedule)
		if err != nil || interval <= 0 {
			return nil, fmt.Errorf("%w: interval schedule %q is not a positive duration", ErrInvalidTrigger, spec.Schedule)
		}
		if interval < time.Second {
			return nil, fmt.Errorf("%w: interval must be at least 1s", ErrInvalidTrigger)
		}
		rt.interval = interval
	case TriggerCron:
		schedule, err := parseCron(spec.Schedule)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidTrigger, err)
		}
		rt.cron = schedule
	case TriggerWebhook:
		// Schedule may be empty: a webhook fires only when called.
	}
	if spec.Kind == TriggerInterval || spec.Kind == TriggerCron {
		if _, err := uuid.Parse(spec.TenantID); err != nil && spec.TenantID != "" {
			return nil, fmt.Errorf("%w: tenantId %q is not a UUID", ErrInvalidTrigger, spec.TenantID)
		}
		if spec.TenantID == "" {
			return nil, fmt.Errorf("%w: scheduled triggers need a tenantId", ErrInvalidTrigger)
		}
	}
	if spec.SessionID != "" {
		if _, err := uuid.Parse(spec.SessionID); err != nil {
			return nil, fmt.Errorf("%w: sessionId %q is not a UUID", ErrInvalidTrigger, spec.SessionID)
		}
	}
	if spec.MaxAttempts <= 0 {
		spec.MaxAttempts = e.defaultAttempts
	}
	if spec.RetryBackoffMs <= 0 {
		spec.RetryBackoffMs = e.defaultBackoff.Milliseconds()
	}
	rt.spec = spec

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, fmt.Errorf("agentext: trigger engine is closed")
	}
	if existing, ok := e.triggers[spec.ID]; ok {
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: trigger %q already registered by %q", ErrInvalidTrigger, spec.ID, existing.spec.Owner)
	}
	e.triggers[spec.ID] = rt
	e.mu.Unlock()

	if spec.Enabled && rt.scheduled() {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.schedule(rt)
		}()
	}
	var once sync.Once
	remove := func() {
		once.Do(func() {
			rt.stopOnce.Do(func() { close(rt.stop) })
			e.mu.Lock()
			if e.triggers[spec.ID] == rt {
				delete(e.triggers, spec.ID)
			}
			e.mu.Unlock()
		})
	}
	return remove, nil
}

func (rt *triggerRuntime) scheduled() bool {
	return rt.spec.Kind == TriggerInterval || rt.spec.Kind == TriggerCron
}

// Fire runs one trigger. Scheduled fires use the trigger's own tenant; manual
// and webhook fires use tenantID.
func (e *TriggerEngine) Fire(ctx context.Context, id string, tenantID uuid.UUID, req FireRequest) (FireResult, error) {
	e.mu.Lock()
	rt := e.triggers[id]
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return FireResult{}, fmt.Errorf("agentext: trigger engine is closed")
	}
	if rt == nil {
		return FireResult{}, ErrTriggerNotFound
	}
	if !rt.spec.Enabled {
		return FireResult{}, ErrTriggerDisabled
	}
	if req.Reason == "" {
		req.Reason = "manual"
	}
	if req.IdempotencyKey == "" {
		if req.Reason == "webhook" {
			return FireResult{}, ErrTriggerKeyRequired
		}
		req.IdempotencyKey = uuid.NewString()
	}
	ownerTenant := tenantID
	if rt.spec.TenantID != "" {
		parsed, err := uuid.Parse(rt.spec.TenantID)
		if err != nil {
			return FireResult{}, fmt.Errorf("%w: trigger tenant %q is not a UUID", ErrInvalidTrigger, rt.spec.TenantID)
		}
		// A trigger belongs to its tenant: a caller from another tenant must not
		// be able to fire it (the 404 does not leak existence).
		if tenantID != uuid.Nil && parsed != tenantID {
			return FireResult{}, ErrTriggerNotFound
		}
		ownerTenant = parsed
	}
	if ownerTenant == uuid.Nil {
		return FireResult{}, fmt.Errorf("%w: tenant is required to fire %q", ErrInvalidTrigger, id)
	}

	// Serialize concurrent fires of the same key: the audit entry, hooks and
	// retry budget must be counted once; the kernel's requestId dedupe then
	// makes the commit itself idempotent across restarts.
	lockKey := id + "|" + ownerTenant.String() + "|" + req.IdempotencyKey
	unlock := e.locks.Lock(lockKey)
	defer unlock()

	if previous, ok := e.lookupFired(lockKey); ok && previous.Status == "fired" {
		previous.Deduped = true
		return previous, nil
	}

	sessionID := uuid.Nil
	if rt.spec.SessionID != "" {
		sessionID = uuid.MustParse(rt.spec.SessionID)
	} else {
		sessionID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("keel.trigger.v1|"+
			ownerTenant.String()+"|"+id+"|"+req.IdempotencyKey))
	}
	requestID := "trigger:" + id + ":" + req.IdempotencyKey

	if e.hooks != nil {
		payload := HookPayload{
			Event:     HookTriggerBefore,
			TenantID:  ownerTenant.String(),
			SessionID: sessionID.String(),
			TriggerID: id,
			RequestID: requestID,
			Status:    "before",
			At:        e.now(),
		}
		payload.SetData("reason", req.Reason)
		e.hooks.Emit(payload)
	}

	result := FireResult{
		TriggerID:      id,
		IdempotencyKey: req.IdempotencyKey,
		SessionID:      sessionID.String(),
	}
	var lastErr error
	for attempt := 1; attempt <= rt.spec.MaxAttempts; attempt++ {
		result.Attempts = attempt
		start, err := e.starter.StartTurn(ctx, StartTurnRequest{
			TenantID:  ownerTenant,
			SessionID: sessionID,
			Text:      rt.spec.Prompt,
			RequestID: requestID,
			Extras: map[string]any{
				"trigger": map[string]any{
					"id":             id,
					"idempotencyKey": req.IdempotencyKey,
					"reason":         req.Reason,
				},
			},
		})
		if err == nil {
			result.Status = "fired"
			result.Seq = start.Seq
			result.Deduped = result.Deduped || start.Deduped
			lastErr = nil
			break
		}
		lastErr = err
		if !retryableTriggerError(err) || attempt == rt.spec.MaxAttempts {
			break
		}
		backoff := time.Duration(rt.spec.RetryBackoffMs) * time.Millisecond * time.Duration(attempt)
		select {
		case <-ctx.Done():
			lastErr = ctx.Err()
			attempt = rt.spec.MaxAttempts
		case <-time.After(backoff):
		}
	}
	if lastErr != nil {
		result.Status = "failed"
		result.Error = lastErr.Error()
	}
	entry := AuditEntry{
		At:             e.now(),
		TriggerID:      id,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
		SessionID:      result.SessionID,
		Seq:            result.Seq,
		Status:         result.Status,
		Attempts:       result.Attempts,
		Error:          result.Error,
	}
	e.record(rt, entry)
	e.rememberFired(lockKey, result)

	if e.hooks != nil {
		payload := HookPayload{
			Event:     HookTriggerAfter,
			TenantID:  ownerTenant.String(),
			SessionID: result.SessionID,
			TriggerID: id,
			RequestID: requestID,
			Status:    result.Status,
			At:        entry.At,
		}
		payload.SetData("reason", req.Reason)
		payload.SetData("attempts", fmt.Sprintf("%d", result.Attempts))
		// The error detail stays in the trigger's audit (GET /v1/triggers); the
		// hook payload carries the status only (no free-form provider text).
		e.hooks.Emit(payload)
	}
	if lastErr != nil {
		return result, fmt.Errorf("%w: %v", ErrTriggerStartFailed, lastErr)
	}
	return result, nil
}

// List returns every trigger's state, sorted by id.
func (e *TriggerEngine) List() []TriggerStatus {
	e.mu.Lock()
	runtimes := make([]*triggerRuntime, 0, len(e.triggers))
	for _, rt := range e.triggers {
		runtimes = append(runtimes, rt)
	}
	e.mu.Unlock()
	out := make([]TriggerStatus, 0, len(runtimes))
	for _, rt := range runtimes {
		rt.mu.Lock()
		status := TriggerStatus{
			TriggerSpec: rt.spec,
			LastStatus:  rt.lastStatus,
			Fires:       rt.fires,
			Failures:    rt.failures,
			Audit:       append([]AuditEntry(nil), rt.audit...),
		}
		if !rt.nextFire.IsZero() {
			next := rt.nextFire
			status.NextFireAt = &next
		}
		if !rt.lastFireAt.IsZero() {
			last := rt.lastFireAt
			status.LastFireAt = &last
		}
		rt.mu.Unlock()
		out = append(out, status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns one trigger's state.
func (e *TriggerEngine) Get(id string) (TriggerStatus, bool) {
	for _, status := range e.List() {
		if status.ID == id {
			return status, true
		}
	}
	return TriggerStatus{}, false
}

// UnregisterAll removes every trigger (plugin unload / engine shutdown helper).
func (e *TriggerEngine) UnregisterAll() {
	e.mu.Lock()
	ids := make([]string, 0, len(e.triggers))
	for id := range e.triggers {
		ids = append(ids, id)
	}
	runtimes := e.triggers
	e.triggers = make(map[string]*triggerRuntime)
	e.mu.Unlock()
	for _, id := range ids {
		if rt := runtimes[id]; rt != nil {
			rt.stopOnce.Do(func() { close(rt.stop) })
		}
	}
}

// Close stops the schedulers and is idempotent.
func (e *TriggerEngine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	runtimes := e.triggers
	e.triggers = make(map[string]*triggerRuntime)
	e.mu.Unlock()
	for _, rt := range runtimes {
		rt.stopOnce.Do(func() { close(rt.stop) })
	}
	e.cancel()
	e.wg.Wait()
}

func (e *TriggerEngine) schedule(rt *triggerRuntime) {
	for {
		next := e.nextFireTime(rt, e.now())
		if next.IsZero() {
			return
		}
		rt.mu.Lock()
		rt.nextFire = next
		rt.mu.Unlock()
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-e.rootCtx.Done():
			timer.Stop()
			return
		case <-rt.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		key := fmt.Sprintf("sched:%s:%d", rt.spec.ID, next.Unix())
		ctx, cancel := context.WithTimeout(e.rootCtx, 30*time.Second)
		if _, err := e.Fire(ctx, rt.spec.ID, uuid.Nil, FireRequest{IdempotencyKey: key, Reason: "schedule"}); err != nil {
			// A scheduled failure is already recorded on the trigger; log it and
			// keep the schedule alive (a broken provider must not stop the clock).
			e.log.Warn("scheduled trigger failed", "trigger", rt.spec.ID, "err", err)
		}
		cancel()
	}
}

// nextFireTime computes the next scheduled instant strictly after now.
func (e *TriggerEngine) nextFireTime(rt *triggerRuntime, now time.Time) time.Time {
	switch rt.spec.Kind {
	case TriggerInterval:
		// Align to the interval grid so a restart cannot shift the window and
		// the idempotency key is stable across processes.
		start := now.Truncate(rt.interval)
		next := start.Add(rt.interval)
		if !next.After(now) {
			next = next.Add(rt.interval)
		}
		return next
	case TriggerCron:
		return rt.cron.Next(now)
	}
	return time.Time{}
}

func (e *TriggerEngine) record(rt *triggerRuntime, entry AuditEntry) {
	rt.mu.Lock()
	rt.fires++
	if entry.Status != "fired" {
		rt.failures++
	}
	rt.lastFireAt = entry.At
	rt.lastStatus = entry.Status
	rt.audit = append([]AuditEntry{entry}, rt.audit...)
	if len(rt.audit) > maxAuditPerTrigger {
		rt.audit = rt.audit[:maxAuditPerTrigger]
	}
	rt.mu.Unlock()
}

func (e *TriggerEngine) lookupFired(key string) (FireResult, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	result, ok := e.fired[key]
	return result, ok
}

func (e *TriggerEngine) rememberFired(key string, result FireResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.fired[key]; !exists {
		e.order = append(e.order, key)
	}
	e.fired[key] = result
	if len(e.order) > maxFireHistory {
		drop := len(e.order) - maxFireHistory
		for _, old := range e.order[:drop] {
			delete(e.fired, old)
		}
		e.order = append([]string(nil), e.order[drop:]...)
	}
}

// retryableTriggerError keeps retries away from definitive refusals: retrying a
// quota rejection or a foreign session only burns the retry budget.
func retryableTriggerError(err error) bool {
	switch {
	case errors.Is(err, kernel.ErrQuota),
		errors.Is(err, kernel.ErrNotFound),
		errors.Is(err, kernel.ErrExternalSession),
		errors.Is(err, kernel.ErrKeyConflict),
		errors.Is(err, kernel.ErrInvalidRequest),
		errors.Is(err, kernel.ErrKeyKindMismatch),
		errors.Is(err, kernel.ErrMissingToken),
		errors.Is(err, context.Canceled):
		return false
	}
	return true
}

// --- cron ----------------------------------------------------------------

// cronSchedule is a parsed 5-field crontab expression (minute hour dom month
// dow). It intentionally implements the common subset (lists, ranges, steps,
// `*`, and the standard dom-or-dow rule) without adding a dependency.
type cronSchedule struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

func (c *cronSchedule) matchDay(day time.Time) bool {
	if c.month&(1<<uint(day.Month())) == 0 {
		return false
	}
	domMatch := c.dom&(1<<uint(day.Day())) != 0
	dowMatch := c.dow&(1<<uint(int(day.Weekday()))) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowMatch
	case c.dowStar:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}

// Next returns the first matching minute strictly after `after`, or the zero
// time when the expression cannot match within five years.
func (c *cronSchedule) Next(after time.Time) time.Time {
	start := after.Truncate(time.Minute).Add(time.Minute)
	for day := 0; day <= 366*5; day++ {
		current := start.AddDate(0, 0, day)
		if !c.matchDay(current) {
			continue
		}
		for hour := 0; hour < 24; hour++ {
			if c.hour&(1<<uint(hour)) == 0 {
				continue
			}
			for minute := 0; minute < 60; minute++ {
				if c.minute&(1<<uint(minute)) == 0 {
					continue
				}
				candidate := time.Date(current.Year(), current.Month(), current.Day(), hour, minute, 0, 0, after.Location())
				if candidate.After(after) {
					return candidate
				}
			}
		}
	}
	return time.Time{}
}

func parseCron(expression string) (*cronSchedule, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression %q must have 5 fields (minute hour dom month dow)", expression)
	}
	minute, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("minute field: %w", err)
	}
	hour, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("hour field: %w", err)
	}
	dom, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("day-of-month field: %w", err)
	}
	month, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("month field: %w", err)
	}
	dow, err := parseCronField(fields[4], 0, 7)
	if err != nil {
		return nil, fmt.Errorf("day-of-week field: %w", err)
	}
	// Sunday is both 0 and 7 in crontab; normalize 7 to 0.
	if dow&(1<<7) != 0 {
		dow = (dow &^ (1 << 7)) | 1
	}
	return &cronSchedule{
		minute:  minute,
		hour:    hour,
		dom:     dom,
		month:   month,
		dow:     dow,
		domStar: fields[2] == "*",
		dowStar: fields[4] == "*",
	}, nil
}

func parseCronField(field string, min, max int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return 0, fmt.Errorf("empty list element in %q", field)
		}
		step := 1
		rangePart := part
		if idx := strings.IndexByte(part, '/'); idx >= 0 {
			rangePart = part[:idx]
			parsed, err := strconv.Atoi(part[idx+1:])
			if err != nil || parsed <= 0 {
				return 0, fmt.Errorf("invalid step in %q", part)
			}
			step = parsed
		}
		low, high := min, max
		switch {
		case rangePart == "*":
			// keep full range
		case strings.ContainsRune(rangePart, '-'):
			pieces := strings.SplitN(rangePart, "-", 2)
			parsedLow, err := strconv.Atoi(pieces[0])
			if err != nil {
				return 0, fmt.Errorf("invalid range start %q", pieces[0])
			}
			parsedHigh, err := strconv.Atoi(pieces[1])
			if err != nil {
				return 0, fmt.Errorf("invalid range end %q", pieces[1])
			}
			low, high = parsedLow, parsedHigh
		default:
			parsed, err := strconv.Atoi(rangePart)
			if err != nil {
				return 0, fmt.Errorf("invalid value %q", rangePart)
			}
			low, high = parsed, parsed
		}
		if low < min || high > max || low > high {
			return 0, fmt.Errorf("value %q out of range %d-%d", part, min, max)
		}
		for value := low; value <= high; value += step {
			bits |= 1 << uint(value)
		}
	}
	if bits == 0 {
		return 0, fmt.Errorf("empty field %q", field)
	}
	return bits, nil
}
