// Package quota implements the contract kernel's admission seam (kernel.Limits)
// from the tenant configuration document.
//
// The kernel owns no quota policy: it calls AdmitRun when a run is claimed and
// AdmitEvent before an event is committed. This package supplies the policy
// from host.TenantConfig.Quota — the same document the admin API writes — so
// there is exactly one quota vocabulary. Two dimensions are enforced:
//
//   - run concurrency: how many runs of one tenant may execute at the same
//     time in this process. A claim reserves a slot; finish/release returns it.
//   - event rate: a per-tenant token bucket (events per minute) shared by every
//     session of the tenant.
//
// # Configuration source
//
// The hosted default document (the process boot configuration) supplies the
// fallback. When a DocumentSource is attached, each tenant's stored document is
// read (short TTL cache) and merged over the default: an administrator's
// PUT /v1/tenants/{id}/config takes effect for the tenant's next admissions.
// A read or parse failure is fail-closed: the admission is refused rather than
// guessing a quota; the kernel audits the refusal.
//
// The counters are process-local on purpose. They are a capacity guard for the
// process that executes the work (the worker), not an accounting ledger;
// durable usage is recorded by the usage ledger. A multi-runtime deployment
// enforces its per-process share, which is exactly the resource it can
// actually exhaust.
package quota

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
)

// Admission errors. The kernel wraps them in ErrQuota; callers can compare
// with errors.Is to tell a run-capacity rejection from an event-rate one.
var (
	// ErrRunConcurrency means the tenant already runs its allowed number of
	// concurrent runs.
	ErrRunConcurrency = errors.New("quota: tenant run concurrency exceeded")
	// ErrEventRate means the tenant's admitted-event budget is spent for now.
	ErrEventRate = errors.New("quota: tenant event rate exceeded")
	// ErrConfig means the tenant's quota could not be resolved (fail-closed).
	ErrConfig = errors.New("quota: tenant quota unavailable")
)

// DefaultConfigTTL bounds how long a tenant's stored quota is cached. A config
// change therefore applies to admissions within this window ("next run"), not
// on a restart.
const DefaultConfigTTL = 2 * time.Second

// DocumentSource reads a tenant's stored configuration document. It is the
// narrow seam over the admin-written tenant row (store.GetTenantConfig); the
// returned bytes are the canonical JSON document. A missing tenant returns an
// empty document ("{}"), not an error.
type DocumentSource interface {
	TenantDocument(ctx context.Context, tenantID uuid.UUID) (document []byte, revision int64, err error)
}

// Limits is a kernel.Limits implementation backed by an in-memory per-tenant
// admission state. It is safe for concurrent use.
type Limits struct {
	mu   sync.Mutex
	now  func() time.Time
	base host.QuotaConfig
	src  DocumentSource
	ttl  time.Duration
	seen map[uuid.UUID]*tenantState
}

type tenantState struct {
	// active is the number of claimed runs of this tenant executing now.
	active int
	// tokens is the event bucket; replenished at eventsPerMinute/60 per second
	// up to capacity eventsPerMinute.
	tokens float64
	last   time.Time
	// quota is the resolved configuration and when it was fetched.
	quota    host.QuotaConfig
	fetched  time.Time
	hasQuota bool
}

var _ kernel.Limits = (*Limits)(nil)

// New builds the limits from the hosting process's default tenant document. A
// zero quota admits everything.
func New(cfg host.QuotaConfig) *Limits {
	return &Limits{now: time.Now, base: cfg, ttl: DefaultConfigTTL, seen: map[uuid.UUID]*tenantState{}}
}

// NewWithClock is New with an injected clock (tests).
func NewWithClock(cfg host.QuotaConfig, now func() time.Time) *Limits {
	l := New(cfg)
	if now != nil {
		l.now = now
	}
	return l
}

// WithSource attaches the per-tenant document source. The stored document's
// quota is merged over the default (tenant values win when non-zero).
func (l *Limits) WithSource(src DocumentSource) *Limits {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.src = src
	l.seen = map[uuid.UUID]*tenantState{}
	return l
}

// SetTTL changes the document cache TTL (tests).
func (l *Limits) SetTTL(ttl time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ttl > 0 {
		l.ttl = ttl
	}
}

// Update replaces the hosting default quota. In-flight reservations are kept: a
// configuration change lowers the limit for new claims without killing the runs
// already admitted (a run pins its admission, like it pins its loadout).
func (l *Limits) Update(cfg host.QuotaConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.base = cfg
	for _, st := range l.seen {
		st.hasQuota = false
		if capacity := float64(cfg.EventsPerMinute); capacity > 0 && st.tokens > capacity {
			st.tokens = capacity
		}
	}
}

// AdmitRun reserves one concurrent-run slot for the tenant.
func (l *Limits) AdmitRun(ctx context.Context, scope kernel.Scope) error {
	cfg, err := l.configFor(ctx, scope.TenantID)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if cfg.RunConcurrency <= 0 {
		return nil
	}
	st := l.stateLocked(scope.TenantID)
	if st.active >= cfg.RunConcurrency {
		return fmt.Errorf("%w: tenant %s runs %d/%d", ErrRunConcurrency, scope.TenantID, st.active, cfg.RunConcurrency)
	}
	st.active++
	return nil
}

// ReleaseRun returns a slot reserved by AdmitRun. It is idempotent-safe: the
// counter clamps at zero, so a stale finish can never underflow it.
func (l *Limits) ReleaseRun(_ context.Context, scope kernel.Scope) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.seen[scope.TenantID]
	if st == nil || st.active == 0 {
		return
	}
	st.active--
}

// AdmitEvent takes one token from the tenant's event bucket.
func (l *Limits) AdmitEvent(ctx context.Context, scope kernel.Scope, kind kernel.EventKind) error {
	cfg, err := l.configFor(ctx, scope.TenantID)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := cfg.EventsPerMinute
	if limit <= 0 {
		return nil
	}
	st := l.stateLocked(scope.TenantID)
	now := l.now()
	capacity := float64(limit)
	ratePerSecond := capacity / 60.0
	if !st.last.IsZero() {
		st.tokens += now.Sub(st.last).Seconds() * ratePerSecond
		if st.tokens > capacity {
			st.tokens = capacity
		}
	} else {
		st.tokens = capacity
	}
	st.last = now
	if st.tokens < 1 {
		return fmt.Errorf("%w: tenant %s budget %.2f events/min, kind %s", ErrEventRate, scope.TenantID, capacity, kind)
	}
	st.tokens--
	return nil
}

// Active returns the tenant's current concurrent-run count (diagnostics/tests).
func (l *Limits) Active(tenantID uuid.UUID) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := l.seen[tenantID]; st != nil {
		return st.active
	}
	return 0
}

// Quota returns the quota currently resolved for a tenant (diagnostics/tests).
func (l *Limits) Quota(ctx context.Context, tenantID uuid.UUID) (host.QuotaConfig, error) {
	return l.configFor(ctx, tenantID)
}

// configFor resolves the tenant's quota: the cached stored document merged over
// the hosting default, refreshed after the TTL. The fetch happens outside the
// lock; a concurrent refresh may win, which is harmless (same document).
func (l *Limits) configFor(ctx context.Context, tenantID uuid.UUID) (host.QuotaConfig, error) {
	l.mu.Lock()
	base, src, ttl, now := l.base, l.src, l.ttl, l.now()
	st := l.seen[tenantID]
	if src == nil {
		l.mu.Unlock()
		return base, nil
	}
	if st != nil && st.hasQuota && now.Sub(st.fetched) < ttl {
		cfg := st.quota
		l.mu.Unlock()
		return cfg, nil
	}
	l.mu.Unlock()

	document, _, err := src.TenantDocument(ctx, tenantID)
	if err != nil {
		return host.QuotaConfig{}, fmt.Errorf("%w: read tenant %s: %v", ErrConfig, tenantID, err)
	}
	doc, err := host.ParseTenantConfig(document)
	if err != nil {
		return host.QuotaConfig{}, fmt.Errorf("%w: tenant %s: %v", ErrConfig, tenantID, err)
	}
	if err := doc.Validate(); err != nil {
		return host.QuotaConfig{}, fmt.Errorf("%w: tenant %s: %v", ErrConfig, tenantID, err)
	}
	cfg := host.Merge(host.TenantConfig{Quota: base}, doc).Quota

	l.mu.Lock()
	fresh := l.stateLocked(tenantID)
	fresh.quota = cfg
	fresh.fetched = l.now()
	fresh.hasQuota = true
	l.mu.Unlock()
	return cfg, nil
}

func (l *Limits) stateLocked(tenantID uuid.UUID) *tenantState {
	st := l.seen[tenantID]
	if st == nil {
		st = &tenantState{}
		l.seen[tenantID] = st
	}
	return st
}
