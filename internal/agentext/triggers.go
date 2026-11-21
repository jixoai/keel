package agentext

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/tasklog"
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
//     returned to the caller; nothing is silently dropped;
//   - webhook authenticity: a webhook fire is accepted only when its
//     HMAC-SHA256 signature over (tenant, trigger, timestamp, key, body) is
//     valid and inside the replay window. A trigger without a configured secret
//     fails closed: the route refuses every signed delivery and audits the
//     refusal. The secret is never serialized or logged;
//   - bounded delivery: a trigger has a per-trigger in-flight cap (excess fires
//     get ErrTriggerBusy instead of queueing forever), and an interval/cron
//     trigger replays at most CatchUpLimit missed windows after a restart,
//     skipping older ones with an audit record so a long outage cannot storm;
//   - ownership: a fired turn is immediately attributable, so the session's
//     initial task card is created as part of the fire (task-log: 会话起步即有卡).
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
	// MaxConcurrent caps in-flight fires of this trigger (default 1). A fire
	// above the cap is refused with ErrTriggerBusy (HTTP 429) instead of
	// queueing without a bound; idempotent retries of an in-flight key still
	// wait for that key's first fire and then dedupe.
	MaxConcurrent int `json:"maxConcurrent,omitempty"`
	// WebhookSecret is the HMAC-SHA256 key for signed webhook deliveries. It is
	// deliberately not serialized: neither GET /v1/triggers nor the plugin
	// snapshot may expose it. A webhook fire without it is refused (fail
	// closed) and audited.
	WebhookSecret string `json:"-"`
	// WebhookSecretEnv names the environment variable the host resolves
	// WebhookSecret from at registration time. The name is public; the value is
	// not. An unset/empty variable leaves the trigger unconfigured (fail closed).
	WebhookSecretEnv string `json:"webhookSecretEnv,omitempty"`
	// WebhookToleranceSec is the accepted clock skew for a delivery timestamp
	// (default 300, maximum 86400).
	WebhookToleranceSec int `json:"webhookToleranceSec,omitempty"`
	// CatchUpLimit bounds how many missed interval/cron windows are replayed
	// after a restart (default 3; 0 uses the engine default). Older missed
	// windows are coalesced into one "skipped" audit record.
	CatchUpLimit int `json:"catchUpLimit,omitempty"`
}

// FireRequest is one manual, scheduled or webhook invocation.
type FireRequest struct {
	// IdempotencyKey is required for webhooks and optional for a manual fire
	// (a generated key is reported back; retries of a manual fire must send
	// their own key).
	IdempotencyKey string
	// Reason is recorded in the audit: "manual", "schedule", "catchup" or
	// "webhook".
	Reason string
	// WebhookTimestamp is the raw X-Keel-Timestamp header (unix seconds).
	WebhookTimestamp string
	// WebhookSignature is the raw X-Keel-Signature header ("sha256=<hex>").
	WebhookSignature string
	// WebhookBody is the exact request body that was signed. It is only used
	// when Reason is "webhook".
	WebhookBody []byte
	// ScheduledFor is the planned window of a scheduled fire (zero for manual
	// and webhook fires). It is the durable catch-up cursor, not the wall clock
	// of the attempt.
	ScheduledFor time.Time
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
	// TaskCardID is the session's initial card, created as part of the fire so
	// the first tool call is attributable without a lazy creation detour.
	TaskCardID string `json:"taskCardId,omitempty"`
	// TaskCardError is set when the turn was committed but the card could not
	// be created; the turn is not rolled back (the lazy first-call path still
	// creates it), and the failure is visible instead of silent.
	TaskCardError string `json:"taskCardError,omitempty"`
}

// AuditEntry is one recorded fire, dedupe, refusal or catch-up skip.
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
	Deduped        bool      `json:"deduped,omitempty"`
	TaskCardID     string    `json:"taskCardId,omitempty"`
	TaskCardError  string    `json:"taskCardError,omitempty"`
}

// TriggerStatus is the observable state of one trigger.
type TriggerStatus struct {
	TriggerSpec
	NextFireAt *time.Time `json:"nextFireAt,omitempty"`
	LastFireAt *time.Time `json:"lastFireAt,omitempty"`
	// LastScheduledAt is the newest scheduled window the engine processed
	// (fired, deduped, failed or skipped). It survives a restart via
	// TriggerStateStore and is the catch-up cursor.
	LastScheduledAt   *time.Time   `json:"lastScheduledAt,omitempty"`
	LastStatus        string       `json:"lastStatus,omitempty"`
	Fires             int64        `json:"fires"`
	Failures          int64        `json:"failures"`
	Dedupes           int64        `json:"dedupes,omitempty"`
	Throttled         int64        `json:"throttled,omitempty"`
	Rejected          int64        `json:"rejected,omitempty"`
	Skipped           int64        `json:"skipped,omitempty"`
	InFlight          int          `json:"inFlight"`
	WebhookConfigured bool         `json:"webhookConfigured,omitempty"`
	Audit             []AuditEntry `json:"audit,omitempty"`
}

// Trigger errors. The HTTP layer maps them onto 404/400/409/401/403/429/502.
var (
	ErrTriggerNotFound    = errors.New("agentext: trigger not found")
	ErrTriggerDisabled    = errors.New("agentext: trigger is disabled")
	ErrTriggerKeyRequired = errors.New("agentext: an idempotency key is required")
	ErrInvalidTrigger     = errors.New("agentext: invalid trigger")
	ErrTriggerStartFailed = errors.New("agentext: trigger start failed")
	// ErrTriggerBusy is the explicit per-trigger concurrency refusal (HTTP
	// 429). The engine does not queue above the cap: a bounded refusal is
	// observable, a growing queue is not.
	ErrTriggerBusy = errors.New("agentext: trigger is at its concurrency limit")
	// ErrTriggerWebhookUnauthorized covers a missing/invalid/stale signature.
	ErrTriggerWebhookUnauthorized = errors.New("agentext: webhook signature is invalid")
	// ErrTriggerWebhookUnconfigured is the fail-closed answer of a webhook
	// trigger that declares no HMAC secret.
	ErrTriggerWebhookUnconfigured = errors.New("agentext: webhook secret is not configured")
)

// TriggerStateStore is the durable catch-up cursor (PostgreSQL in production).
// It is optional: without it a scheduled trigger keeps the v1 behavior (no
// replay after a restart), and every fire still works.
type TriggerStateStore interface {
	LoadTriggerState(ctx context.Context, tenantID uuid.UUID, triggerID string) (store.TriggerState, bool, error)
	SaveTriggerState(ctx context.Context, state store.TriggerState) error
}

// TaskCardEnsurer creates a session's initial task card (satisfied by
// *tasklog.Service). It is optional so the extension surface stays usable
// without a task log, and it is the same seam the lazy first-tool-call path
// uses.
type TaskCardEnsurer interface {
	EnsureCard(ctx context.Context, scope kernel.Scope) (tasklog.Card, error)
}

// TriggerEngineOptions configures the engine.
type TriggerEngineOptions struct {
	// Starter starts a turn (TurnStarter in production).
	Starter turnStarter
	Hooks   *HookBus
	Logger  *slog.Logger
	Now     func() time.Time
	// State persists the last processed scheduled window for catch-up.
	State TriggerStateStore
	// Tasks creates the session's initial card as part of a fire.
	Tasks           TaskCardEnsurer
	DefaultAttempts int
	DefaultBackoff  time.Duration
	// DefaultConcurrency is the per-trigger in-flight cap when a spec does not
	// set MaxConcurrent (default 1).
	DefaultConcurrency int
	// CatchUpLimit bounds replayed windows per restart (default 3).
	CatchUpLimit int
	// CatchUpWindow bounds how far back a restart may replay (default 24h).
	CatchUpWindow time.Duration
	// WebhookTolerance is the accepted delivery clock skew (default 5m).
	WebhookTolerance time.Duration
}

// turnStarter is the narrow start seam of the trigger engine (satisfied by
// *TurnStarter). It is an interface so tests can count starts without a kernel.
type turnStarter interface {
	StartTurn(ctx context.Context, req StartTurnRequest) (StartTurnResult, error)
}

const (
	defaultTriggerAttempts    = 3
	defaultTriggerBackoff     = 250 * time.Millisecond
	defaultTriggerConcurrency = 1
	defaultCatchUpLimit       = 3
	defaultCatchUpWindow      = 24 * time.Hour
	defaultWebhookTolerance   = 5 * time.Minute
	maxWebhookTolerance       = 24 * time.Hour
	minWebhookSecretBytes     = 16
	maxIdempotencyKeyBytes    = 256
	maxTriggerConcurrency     = 1024
	maxTriggerCatchUpLimit    = 100
	maxFireHistory            = 4096
	maxAuditPerTrigger        = 20
)

// TriggerEngine owns interval/cron/webhook triggers and their audit trail.
type TriggerEngine struct {
	starter            turnStarter
	hooks              *HookBus
	log                *slog.Logger
	now                func() time.Time
	state              TriggerStateStore
	tasks              TaskCardEnsurer
	defaultAttempts    int
	defaultBackoff     time.Duration
	defaultConcurrency int
	catchUpLimit       int
	catchUpWindow      time.Duration
	webhookTolerance   time.Duration

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
	// slots bounds in-flight fires of this trigger. len(slots) is the live
	// count; a failed non-blocking send is the throttled refusal.
	slots chan struct{}
	// webhookTolerance is the resolved replay window (spec override or engine
	// default).
	webhookTolerance time.Duration
	// catchUpLimit is the resolved per-trigger replay cap.
	catchUpLimit int

	stop     chan struct{}
	stopOnce sync.Once

	mu              sync.Mutex
	nextFire        time.Time
	lastFireAt      time.Time
	lastScheduledAt time.Time
	lastStatus      string
	fires           int64
	failures        int64
	dedupes         int64
	throttled       int64
	rejected        int64
	skipped         int64
	audit           []AuditEntry
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
	if opts.DefaultConcurrency <= 0 {
		opts.DefaultConcurrency = defaultTriggerConcurrency
	}
	if opts.CatchUpLimit <= 0 {
		opts.CatchUpLimit = defaultCatchUpLimit
	}
	if opts.CatchUpWindow <= 0 {
		opts.CatchUpWindow = defaultCatchUpWindow
	}
	if opts.WebhookTolerance <= 0 {
		opts.WebhookTolerance = defaultWebhookTolerance
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &TriggerEngine{
		starter:            opts.Starter,
		hooks:              opts.Hooks,
		log:                opts.Logger,
		now:                opts.Now,
		state:              opts.State,
		tasks:              opts.Tasks,
		defaultAttempts:    opts.DefaultAttempts,
		defaultBackoff:     opts.DefaultBackoff,
		defaultConcurrency: opts.DefaultConcurrency,
		catchUpLimit:       opts.CatchUpLimit,
		catchUpWindow:      opts.CatchUpWindow,
		webhookTolerance:   opts.WebhookTolerance,
		locks:              keyedMutex{locks: make(map[string]*lockEntry)},
		triggers:           make(map[string]*triggerRuntime),
		fired:              make(map[string]FireResult),
		rootCtx:            ctx,
		cancel:             cancel,
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
	if spec.MaxConcurrent < 0 || spec.MaxConcurrent > maxTriggerConcurrency {
		return nil, fmt.Errorf("%w: maxConcurrent must be between 0 and %d",
			ErrInvalidTrigger, maxTriggerConcurrency)
	}
	if spec.MaxConcurrent == 0 {
		spec.MaxConcurrent = e.defaultConcurrency
	}
	if spec.CatchUpLimit < 0 || spec.CatchUpLimit > maxTriggerCatchUpLimit {
		return nil, fmt.Errorf("%w: catchUpLimit must be between 0 and %d",
			ErrInvalidTrigger, maxTriggerCatchUpLimit)
	}
	if spec.CatchUpLimit == 0 {
		spec.CatchUpLimit = e.catchUpLimit
	}
	if spec.WebhookToleranceSec < 0 ||
		time.Duration(spec.WebhookToleranceSec)*time.Second > maxWebhookTolerance {
		return nil, fmt.Errorf("%w: webhookToleranceSec must be between 0 and %d",
			ErrInvalidTrigger, int(maxWebhookTolerance/time.Second))
	}
	// Resolve the secret from the named variable once, at registration: the
	// environment is the only place a value is allowed to live, and the spec
	// keeps only the variable name.
	if spec.WebhookSecret == "" && spec.WebhookSecretEnv != "" {
		spec.WebhookSecret = os.Getenv(spec.WebhookSecretEnv)
	}
	if spec.WebhookSecret != "" && len(spec.WebhookSecret) < minWebhookSecretBytes {
		return nil, fmt.Errorf("%w: webhook secret must be at least %d bytes",
			ErrInvalidTrigger, minWebhookSecretBytes)
	}
	rt.spec = spec
	rt.slots = make(chan struct{}, spec.MaxConcurrent)
	rt.webhookTolerance = e.webhookTolerance
	if spec.WebhookToleranceSec > 0 {
		rt.webhookTolerance = time.Duration(spec.WebhookToleranceSec) * time.Second
	}
	rt.catchUpLimit = spec.CatchUpLimit

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
	if len(req.IdempotencyKey) > maxIdempotencyKeyBytes {
		return FireResult{}, fmt.Errorf("%w: idempotency key is too long", ErrInvalidTrigger)
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

	// A webhook delivery proves authenticity before anything else: the
	// signature covers the tenant, trigger, timestamp, idempotency key and the
	// exact body, so a captured request cannot be replayed under another key,
	// another tenant or after the window. A trigger without a secret fails
	// closed; every refusal is audited (without the secret or the signature).
	if req.Reason == "webhook" {
		if err := e.verifyWebhook(rt, ownerTenant, req); err != nil {
			e.record(rt, AuditEntry{
				At:             e.now(),
				TriggerID:      id,
				Reason:         req.Reason,
				IdempotencyKey: req.IdempotencyKey,
				Status:         "rejected",
				Error:          err.Error(),
			})
			return FireResult{}, err
		}
	}

	// Serialize concurrent fires of the same key: the audit entry, hooks and
	// retry budget must be counted once; the kernel's requestId dedupe then
	// makes the commit itself idempotent across restarts.
	lockKey := id + "|" + ownerTenant.String() + "|" + req.IdempotencyKey
	unlock := e.locks.Lock(lockKey)
	defer unlock()

	if previous, ok := e.lookupFired(lockKey); ok && previous.Status == "fired" {
		previous.Deduped = true
		e.record(rt, AuditEntry{
			At:             e.now(),
			TriggerID:      id,
			Reason:         req.Reason,
			IdempotencyKey: req.IdempotencyKey,
			SessionID:      previous.SessionID,
			Seq:            previous.Seq,
			Status:         "deduped",
			Deduped:        true,
		})
		return previous, nil
	}

	// Bounded delivery: refuse above the trigger's in-flight cap instead of
	// queueing without a bound. The same-key wait above is not a concurrency
	// violation — it is an idempotent retry that ends in the dedupe branch.
	if !rt.tryAcquire() {
		e.record(rt, AuditEntry{
			At:             e.now(),
			TriggerID:      id,
			Reason:         req.Reason,
			IdempotencyKey: req.IdempotencyKey,
			Status:         "throttled",
			Error:          ErrTriggerBusy.Error(),
		})
		return FireResult{}, ErrTriggerBusy
	}
	defer rt.release()

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
	// The turn is committed; make its session immediately attributable. The
	// task-log spec says a session has a card from its first input, and the
	// trigger is the first input for a fresh trigger session. A failure here
	// does not roll the turn back (the lazy first-tool-call path still creates
	// the card); it is recorded on the result and the audit instead of hidden.
	if lastErr == nil && e.tasks != nil {
		card, cardErr := e.tasks.EnsureCard(ctx, kernel.Scope{TenantID: ownerTenant, SessionID: sessionID})
		if cardErr != nil {
			result.TaskCardError = cardErr.Error()
		} else {
			result.TaskCardID = card.TaskID.String()
		}
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
		Deduped:        result.Deduped,
		TaskCardID:     result.TaskCardID,
		TaskCardError:  result.TaskCardError,
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
			TriggerSpec:       rt.spec,
			LastStatus:        rt.lastStatus,
			Fires:             rt.fires,
			Failures:          rt.failures,
			Dedupes:           rt.dedupes,
			Throttled:         rt.throttled,
			Rejected:          rt.rejected,
			Skipped:           rt.skipped,
			InFlight:          len(rt.slots),
			WebhookConfigured: rt.spec.WebhookSecret != "",
			Audit:             append([]AuditEntry(nil), rt.audit...),
		}
		if !rt.nextFire.IsZero() {
			next := rt.nextFire
			status.NextFireAt = &next
		}
		if !rt.lastFireAt.IsZero() {
			last := rt.lastFireAt
			status.LastFireAt = &last
		}
		if !rt.lastScheduledAt.IsZero() {
			last := rt.lastScheduledAt
			status.LastScheduledAt = &last
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
	e.resume(rt)
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
		result, err := e.Fire(ctx, rt.spec.ID, uuid.Nil, FireRequest{
			IdempotencyKey: key,
			Reason:         "schedule",
			ScheduledFor:   next,
		})
		cancel()
		// A throttled window is not advanced: a manual/webhook fire occupying
		// the slot must not silently consume the scheduled window. It stays the
		// cursor, so a restart can replay it (subject to the catch-up cap).
		if result.Status != "" && result.Status != "throttled" {
			e.saveCursor(rt, next, result.Status)
		}
		if err != nil {
			// A scheduled failure is already recorded on the trigger; log it and
			// keep the schedule alive (a broken provider must not stop the clock).
			e.log.Warn("scheduled trigger failed", "trigger", rt.spec.ID, "err", err)
		}
	}
}

// resume loads the durable cursor and replays the missed windows it proves.
// Without a state store this is a no-op (the v1 behavior: the next window is
// the first one after now).
func (e *TriggerEngine) resume(rt *triggerRuntime) {
	if e.state == nil || !rt.scheduled() || rt.spec.TenantID == "" {
		return
	}
	tenantID, err := uuid.Parse(rt.spec.TenantID)
	if err != nil {
		e.log.Warn("trigger catch-up skipped: tenant is not a UUID", "trigger", rt.spec.ID)
		return
	}
	ctx, cancel := context.WithTimeout(e.rootCtx, 10*time.Second)
	state, ok, err := e.state.LoadTriggerState(ctx, tenantID, rt.spec.ID)
	cancel()
	if err != nil {
		// Catch-up is best effort: the schedule still runs from now, and the
		// next fire persists a fresh cursor. The warning is the only loss mark.
		e.log.Warn("trigger catch-up state unavailable", "trigger", rt.spec.ID, "err", err)
		return
	}
	if !ok {
		return
	}
	rt.mu.Lock()
	rt.lastScheduledAt = state.LastScheduledAt
	if !state.LastFiredAt.IsZero() {
		rt.lastFireAt = state.LastFiredAt
	}
	if state.LastStatus != "" {
		rt.lastStatus = state.LastStatus
	}
	rt.mu.Unlock()

	now := e.now()
	if state.LastScheduledAt.IsZero() || !state.LastScheduledAt.Before(now) {
		return
	}
	fire, dropped, newest := e.catchUpPlan(rt, state.LastScheduledAt, now)
	if len(fire) == 0 {
		if dropped > 0 && !newest.IsZero() {
			// No window was replayed, but the cursor still advances over the
			// coalesced ones: otherwise every restart would rescan the same
			// outage and re-audit it.
			e.recordSkipped(rt, dropped, newest)
			e.saveCursor(rt, newest, "skipped")
		}
		return
	}
	// Audit the coalesced windows before the replays so the newest audit entry
	// and LastStatus remain the newest actual fire.
	if dropped > 0 {
		e.recordSkipped(rt, dropped, newest)
	}
	for _, scheduledFor := range fire {
		if e.rootCtx.Err() != nil || rt.stopped() {
			return
		}
		key := fmt.Sprintf("sched:%s:%d", rt.spec.ID, scheduledFor.Unix())
		fireCtx, fireCancel := context.WithTimeout(e.rootCtx, 30*time.Second)
		result, err := e.Fire(fireCtx, rt.spec.ID, uuid.Nil, FireRequest{
			IdempotencyKey: key,
			Reason:         "catchup",
			ScheduledFor:   scheduledFor,
		})
		fireCancel()
		if result.Status == "throttled" {
			// Do not advance past a window that could not run; the next restart
			// retries it (bounded by the cap) instead of losing it silently.
			return
		}
		if result.Status != "" {
			e.saveCursor(rt, scheduledFor, result.Status)
		}
		if err != nil {
			e.log.Warn("catch-up fire failed", "trigger", rt.spec.ID, "scheduledFor", scheduledFor, "err", err)
		}
	}
}

// catchUpPlan returns the windows to replay (at most the trigger's catch-up
// limit: when the limit forces a choice, only the newest windows are kept), how
// many older windows are coalesced away, and the newest missed window (the
// cursor to persist even when nothing is replayed). The lookback window bounds
// both the scan and the age of a replay so a long outage cannot produce a
// storm.
func (e *TriggerEngine) catchUpPlan(rt *triggerRuntime, last, now time.Time) ([]time.Time, int, time.Time) {
	limit := rt.catchUpLimit
	if limit <= 0 {
		return nil, 0, time.Time{}
	}
	from := last
	if e.catchUpWindow > 0 && now.Sub(last) > e.catchUpWindow {
		from = now.Add(-e.catchUpWindow)
	}
	switch rt.spec.Kind {
	case TriggerInterval:
		if rt.interval <= 0 {
			return nil, 0, time.Time{}
		}
		// O(1): windows are last + k*interval. Count what is older than the
		// lookback bound, then keep the newest `limit` windows.
		dropped := 0
		if from.After(last) {
			skippedBefore := int64(from.Sub(last) / rt.interval)
			if skippedBefore > 0 {
				last = last.Add(time.Duration(skippedBefore) * rt.interval)
				dropped += int(skippedBefore)
			}
		}
		total := int64(now.Sub(last) / rt.interval)
		if total <= 0 {
			return nil, dropped, time.Time{}
		}
		start := total - int64(limit) + 1
		if start < 1 {
			start = 1
		}
		dropped += int(start - 1)
		fire := make([]time.Time, 0, total-start+1)
		for k := start; k <= total; k++ {
			fire = append(fire, last.Add(time.Duration(k)*rt.interval))
		}
		return fire, dropped, fire[len(fire)-1]
	case TriggerCron:
		// Cron has minute granularity, so a bounded cursor walk is cheap. A ring
		// buffer keeps the newest `limit` matches while counting the older ones.
		var ring []time.Time
		dropped := 0
		cursor := last
		for {
			next := rt.cron.Next(cursor)
			if next.IsZero() || next.After(now) {
				break
			}
			cursor = next
			if next.Before(from) {
				dropped++
				continue
			}
			if len(ring) < limit {
				ring = append(ring, next)
				continue
			}
			ring = append(ring[1:], next)
			dropped++
		}
		return ring, dropped, cursor
	}
	return nil, 0, time.Time{}
}

// saveCursor persists the last processed window. A persistence failure is a
// warning, not a fire failure: the turn already committed and the kernel
// requestId keeps the turn idempotent; only the catch-up optimization is lost.
func (e *TriggerEngine) saveCursor(rt *triggerRuntime, scheduledFor time.Time, status string) {
	rt.mu.Lock()
	rt.lastScheduledAt = scheduledFor
	if status != "" {
		rt.lastStatus = status
	}
	rt.mu.Unlock()
	if e.state == nil || rt.spec.TenantID == "" {
		return
	}
	tenantID, err := uuid.Parse(rt.spec.TenantID)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(e.rootCtx, 5*time.Second)
	err = e.state.SaveTriggerState(ctx, store.TriggerState{
		TenantID:        tenantID,
		TriggerID:       rt.spec.ID,
		LastScheduledAt: scheduledFor,
		LastFiredAt:     e.now(),
		LastStatus:      status,
	})
	cancel()
	if err != nil {
		e.log.Warn("trigger cursor persist failed", "trigger", rt.spec.ID, "err", err)
	}
}

// recordSkipped makes coalesced windows visible: the newest replayed window is
// the cursor, and the audit says how many older windows were dropped by the cap
// or the lookback bound.
func (e *TriggerEngine) recordSkipped(rt *triggerRuntime, dropped int, newest time.Time) {
	if dropped <= 0 {
		return
	}
	e.record(rt, AuditEntry{
		At:        e.now(),
		TriggerID: rt.spec.ID,
		Reason:    "catchup",
		Status:    "skipped",
		Error: fmt.Sprintf("coalesced %d missed window(s) before %s",
			dropped, newest.UTC().Format(time.RFC3339)),
	})
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
	switch entry.Status {
	case "fired":
		rt.fires++
		rt.lastFireAt = entry.At
	case "failed":
		rt.fires++
		rt.failures++
		rt.lastFireAt = entry.At
	case "deduped":
		rt.dedupes++
	case "throttled":
		rt.throttled++
	case "rejected":
		rt.rejected++
	case "skipped":
		rt.skipped++
	}
	rt.lastStatus = entry.Status
	rt.audit = append([]AuditEntry{entry}, rt.audit...)
	if len(rt.audit) > maxAuditPerTrigger {
		rt.audit = rt.audit[:maxAuditPerTrigger]
	}
	rt.mu.Unlock()
}

// tryAcquire takes one in-flight slot without blocking.
func (rt *triggerRuntime) tryAcquire() bool {
	select {
	case rt.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (rt *triggerRuntime) release() { <-rt.slots }

// stopped reports whether this trigger was unregistered (used to stop a
// catch-up loop early; the scheduler also selects on rt.stop).
func (rt *triggerRuntime) stopped() bool {
	select {
	case <-rt.stop:
		return true
	default:
		return false
	}
}

// verifyWebhook checks the signature and the replay window. The canonical
// string is documented in docs/HARNESS.md:
//
//	v1\n<tenantId>\n<triggerId>\n<timestamp>\n<idempotencyKey>\n<body>
//
// Both the timestamp and the key are covered so a captured delivery cannot be
// re-aimed at another window or dedupe key. The comparison is constant time,
// and errors carry a category only (never the secret or the received
// signature).
func (e *TriggerEngine) verifyWebhook(rt *triggerRuntime, tenantID uuid.UUID, req FireRequest) error {
	secret := rt.spec.WebhookSecret
	if secret == "" {
		return ErrTriggerWebhookUnconfigured
	}
	if req.WebhookTimestamp == "" {
		return fmt.Errorf("%w: missing_timestamp", ErrTriggerWebhookUnauthorized)
	}
	if req.WebhookSignature == "" {
		return fmt.Errorf("%w: missing_signature", ErrTriggerWebhookUnauthorized)
	}
	timestamp, err := strconv.ParseInt(strings.TrimSpace(req.WebhookTimestamp), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: malformed_timestamp", ErrTriggerWebhookUnauthorized)
	}
	now := e.now().Unix()
	// Bound the value before subtracting: ParseInt accepts MinInt64 and the
	// subtraction would overflow into a bogus "fresh" window.
	horizon := int64(maxWebhookTolerance / time.Second)
	if timestamp < now-horizon || timestamp > now+horizon {
		return fmt.Errorf("%w: stale_timestamp", ErrTriggerWebhookUnauthorized)
	}
	skew := now - timestamp
	if skew < 0 {
		skew = -skew
	}
	if skew > int64(rt.webhookTolerance/time.Second) {
		return fmt.Errorf("%w: stale_timestamp", ErrTriggerWebhookUnauthorized)
	}
	if !strings.HasPrefix(req.WebhookSignature, "sha256=") {
		return fmt.Errorf("%w: unsupported_signature", ErrTriggerWebhookUnauthorized)
	}
	provided := strings.ToLower(strings.TrimPrefix(req.WebhookSignature, "sha256="))
	canonical := "v1\n" + tenantID.String() + "\n" + rt.spec.ID + "\n" +
		req.WebhookTimestamp + "\n" + req.IdempotencyKey + "\n"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	mac.Write(req.WebhookBody)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(provided)) {
		return fmt.Errorf("%w: bad_signature", ErrTriggerWebhookUnauthorized)
	}
	return nil
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
