// Per-tenant edge quotas: concurrent streams, outbound bandwidth and stored
// bytes (spec: tenancy-quotas / 每租户配额).
//
// limits.go holds the kernel's admission seam (runs, admitted events). This
// file holds the dimensions the process edge enforces *before* work reaches a
// session: how many streams one tenant may hold open, how fast its bytes may
// leave, and how much it may store. They live here rather than in the kernel
// because they are properties of this process's network and disk, not of the
// event log — and because a rejected request must be answered with 429/413
// before any ledger row exists.
//
// # Fail-open vs fail-closed
//
// The kernel quota is fail-closed on a configuration read error (guessing a
// quota is worse than refusing). The edge quotas are the opposite once the
// tenant's numbers are known: the *numbers* are resolved through a cached
// TenantQuotaSource, and an unresolvable tenant is a 401/404 upstream, not a
// quota decision. A resolved zero means unlimited.
//
// # Storage is a live measurement, not a counter
//
// A monotonic "bytes stored" counter cannot express deletion, and a storage
// quota that never goes down is a quota that eventually locks a tenant out with
// nothing to show for it. Storage is therefore measured against the blobs
// table at admission time (bounded by the request's own size), so releasing a
// blob releases quota.
package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Edge quota errors. They are distinct types so a handler can map each onto its
// HTTP answer without guessing from a string.
var (
	// ErrStreamQuota means the tenant already holds its allowed concurrent
	// streams open (429 + Retry-After).
	ErrStreamQuota = errors.New("quota: tenant stream concurrency exceeded")
	// ErrStorageQuota means the write would push the tenant past its stored-byte
	// ceiling (413: the request's size is the problem, not the rate).
	ErrStorageQuota = errors.New("quota: tenant storage quota exceeded")
	// ErrBandwidthQuota is informational: the tenant's outbound rate is being
	// shaped rather than refused. A handler never returns it as an error; it
	// reports it so a caller can tell "throttled" from "unlimited".
	ErrBandwidthQuota = errors.New("quota: tenant bandwidth is shaped")
)

// TenantQuota is the edge-relevant part of a tenant's quota row. Zero means
// unlimited in every field.
type TenantQuota struct {
	Streams      int
	BytesPerSec  int64
	StorageBytes int64
}

// Unlimited reports whether every dimension is unbounded.
func (q TenantQuota) Unlimited() bool {
	return q.Streams <= 0 && q.BytesPerSec <= 0 && q.StorageBytes <= 0
}

// TenantQuotaSource resolves a tenant's quota row. The production
// implementation reads store.GetTenantQuota; a missing tenant is an error the
// caller must not translate into a zero (unlimited) quota.
type TenantQuotaSource interface {
	TenantQuota(ctx context.Context, tenantID uuid.UUID) (TenantQuota, error)
}

// QuotaSourceFunc adapts a function to TenantQuotaSource.
type QuotaSourceFunc func(ctx context.Context, tenantID uuid.UUID) (TenantQuota, error)

// TenantQuota implements TenantQuotaSource.
func (f QuotaSourceFunc) TenantQuota(ctx context.Context, tenantID uuid.UUID) (TenantQuota, error) {
	return f(ctx, tenantID)
}

// DefaultQuotaTTL bounds how long a tenant's quota row is cached. A quota change
// therefore applies within this window rather than on a restart; the value is
// short because an operator lowering a quota expects it to bite.
const DefaultQuotaTTL = 2 * time.Second

// Edge enforces the per-tenant stream, bandwidth and storage quotas. It is safe
// for concurrent use.
//
// The stream gauge and the bandwidth shaper are process-local on purpose: they
// bound the resources *this* process can hand out. A multi-process deployment
// enforces each process's share, which is exactly what it can exhaust.
type Edge struct {
	mu     sync.Mutex
	src    TenantQuotaSource
	ttl    time.Duration
	now    func() time.Time
	seen   map[uuid.UUID]*edgeState
	stored StoredBytesFunc
}

type edgeState struct {
	streams int
	quota   TenantQuota
	fetched time.Time
	valid   bool
	shaper  *shapeState
}

// shapeState is a simple leaky-bucket meter for outbound bytes. It is not a
// rate.Limiter: the edge shapes by delaying a write, and it must be able to
// answer "how long should this write wait" without owning a goroutine.
type shapeState struct {
	tokens float64
	last   time.Time
}

// StoredBytesFunc measures a tenant's currently stored bytes. It is a function
// rather than a counter so the quota reflects deletion (see the package
// comment). Nil disables the storage dimension.
type StoredBytesFunc func(ctx context.Context, tenantID uuid.UUID) (int64, error)

// NewEdge builds the edge enforcer. A nil source admits everything (the
// pre-RLS single-tenant deployment), and a nil stored-bytes function disables
// the storage dimension.
func NewEdge(src TenantQuotaSource, stored StoredBytesFunc) *Edge {
	return &Edge{src: src, stored: stored, ttl: DefaultQuotaTTL, now: time.Now,
		seen: map[uuid.UUID]*edgeState{}}
}

// SetTTL changes the quota cache TTL (tests).
func (e *Edge) SetTTL(ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ttl = ttl
}

// SetClock overrides the clock (tests).
func (e *Edge) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = now
}

// quotaFor resolves a tenant's quota through the short-TTL cache.
func (e *Edge) quotaFor(ctx context.Context, tenantID uuid.UUID) (TenantQuota, error) {
	if e == nil || e.src == nil {
		return TenantQuota{}, nil
	}
	e.mu.Lock()
	now := e.now()
	if st := e.seen[tenantID]; st != nil && st.valid && now.Sub(st.fetched) < e.ttl {
		quota := st.quota
		e.mu.Unlock()
		return quota, nil
	}
	e.mu.Unlock()

	quota, err := e.src.TenantQuota(ctx, tenantID)
	if err != nil {
		return TenantQuota{}, fmt.Errorf("quota: resolve tenant %s: %w", tenantID, err)
	}
	e.mu.Lock()
	st := e.edgeStateLocked(tenantID)
	st.quota = quota
	st.fetched = e.now()
	st.valid = true
	e.mu.Unlock()
	return quota, nil
}

func (e *Edge) edgeStateLocked(tenantID uuid.UUID) *edgeState {
	st := e.seen[tenantID]
	if st == nil {
		st = &edgeState{}
		e.seen[tenantID] = st
	}
	return st
}

// ErrNoEdge reports a nil Edge (a deployment without edge quotas). Callers use
// it to skip the checks explicitly instead of silently admitting everything.
var ErrNoEdge = errors.New("quota: no edge quota enforcer")

// AcquireStream reserves one concurrent stream for the tenant. The returned
// release is idempotent and must be called when the stream ends. A denied
// admission returns a RetryAfter hint (seconds) and leaves no reservation.
func (e *Edge) AcquireStream(ctx context.Context, tenantID uuid.UUID) (release func(), retryAfter time.Duration, err error) {
	if e == nil || e.src == nil {
		return func() {}, 0, nil
	}
	quota, err := e.quotaFor(ctx, tenantID)
	if err != nil {
		return nil, 0, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if quota.Streams <= 0 {
		return func() {}, 0, nil
	}
	st := e.edgeStateLocked(tenantID)
	if st.streams >= quota.Streams {
		return nil, time.Second, fmt.Errorf("%w: tenant %s holds %d/%d concurrent streams",
			ErrStreamQuota, tenantID, st.streams, quota.Streams)
	}
	st.streams++
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			if s := e.seen[tenantID]; s != nil && s.streams > 0 {
				s.streams--
			}
			e.mu.Unlock()
		})
	}, 0, nil
}

// ActiveStreams reports a tenant's current stream count (diagnostics/tests).
func (e *Edge) ActiveStreams(tenantID uuid.UUID) int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.seen[tenantID]; st != nil {
		return st.streams
	}
	return 0
}

// WaitBandwidth blocks until n bytes may leave at the tenant's configured rate.
// A zero rate is unlimited. The returned error is ErrBandwidthQuota when the
// write was delayed (informational), nil when it was not shaped at all.
func (e *Edge) WaitBandwidth(ctx context.Context, tenantID uuid.UUID, n int) error {
	if e == nil || e.src == nil || n <= 0 {
		return nil
	}
	quota, err := e.quotaFor(ctx, tenantID)
	if err != nil {
		return err
	}
	if quota.BytesPerSec <= 0 {
		return nil
	}
	e.mu.Lock()
	st := e.edgeStateLocked(tenantID)
	now := e.now()
	if st.shaper == nil {
		// Start with a bucket of one second's budget, so a fresh tenant can send a
		// burst equal to its rate without waiting.
		st.shaper = &shapeState{tokens: float64(quota.BytesPerSec), last: now}
	}
	sh := st.shaper
	sh.tokens += now.Sub(sh.last).Seconds() * float64(quota.BytesPerSec)
	if max := float64(quota.BytesPerSec); sh.tokens > max {
		sh.tokens = max
	}
	sh.last = now
	needed := float64(n)
	if sh.tokens >= needed {
		sh.tokens -= needed
		e.mu.Unlock()
		return nil
	}
	deficit := needed - sh.tokens
	sh.tokens = 0
	wait := time.Duration(deficit / float64(quota.BytesPerSec) * float64(time.Second))
	e.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	return fmt.Errorf("%w: waited %s for %d bytes at %d B/s", ErrBandwidthQuota, wait, n, quota.BytesPerSec)
}

// AdmitStorage checks that writing size more bytes keeps the tenant inside its
// storage ceiling. It measures the tenant's stored bytes (bounded by the
// caller's context) rather than trusting a cached counter, so a delete frees
// quota immediately.
func (e *Edge) AdmitStorage(ctx context.Context, tenantID uuid.UUID, size int64) (remaining int64, err error) {
	if e == nil || size <= 0 {
		return 0, nil
	}
	quota, err := e.quotaFor(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	if quota.StorageBytes <= 0 {
		return 0, nil
	}
	if e.stored == nil {
		return quota.StorageBytes, nil
	}
	used, err := e.stored(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("quota: measure stored bytes: %w", err)
	}
	remaining = quota.StorageBytes - used
	if size > remaining {
		return remaining, fmt.Errorf("%w: tenant %s used %d of %d bytes, request needs %d",
			ErrStorageQuota, tenantID, used, quota.StorageBytes, size)
	}
	return remaining, nil
}

// WriteError writes the HTTP answer for a quota rejection, including the
// Retry-After header, and reports whether it handled the error. A caller
// passes any error it got from this package: a storage rejection is a 413 (the
// body is too big for this tenant), a stream rejection a 429 with the quota
// named in the body. The tenant's numbers appear in the message; the key never
// does.
func WriteError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	status := 0
	retry := ""
	switch {
	case errors.Is(err, ErrStorageQuota):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrStreamQuota), errors.Is(err, ErrRunConcurrency), errors.Is(err, ErrEventRate):
		status = http.StatusTooManyRequests
		retry = "1"
	case errors.Is(err, ErrConfig):
		// A quota that cannot be resolved is a deployment problem, not the
		// caller's: 503 tells the client to retry later without implying its key
		// is wrong.
		status = http.StatusServiceUnavailable
		retry = "5"
	default:
		return false
	}
	if retry != "" {
		w.Header().Set("Retry-After", retry)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The message already went through logsafe.Redact at composition time when
	// it contains tenant data; this package never handles credentials at all.
	_, _ = w.Write([]byte(`{"error":` + strconv.Quote(err.Error()) + `}`))
	return true
}
