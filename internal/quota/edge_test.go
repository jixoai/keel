// Edge quota tests: per-tenant streams, bandwidth shaping and stored bytes
// (change keel-ga §2). The load-bearing property is the spec's "单租户打满不影响
// 他人": one tenant exhausting its quota must leave every other tenant's
// admission untouched.

package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// staticSource serves a fixed quota per tenant; an unknown tenant is an error
// (never a zero quota, which would silently mean unlimited).
type staticSource struct {
	mu     sync.Mutex
	quota  map[uuid.UUID]TenantQuota
	lookup map[uuid.UUID]int
}

func newStaticSource() *staticSource {
	return &staticSource{quota: map[uuid.UUID]TenantQuota{}, lookup: map[uuid.UUID]int{}}
}

func (s *staticSource) set(tenantID uuid.UUID, q TenantQuota) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quota[tenantID] = q
}

func (s *staticSource) TenantQuota(_ context.Context, tenantID uuid.UUID) (TenantQuota, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookup[tenantID]++
	q, ok := s.quota[tenantID]
	if !ok {
		return TenantQuota{}, fmt.Errorf("tenant %s not found", tenantID)
	}
	return q, nil
}

func (s *staticSource) lookups(tenantID uuid.UUID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookup[tenantID]
}

func TestEdgeStreamQuotaIsPerTenant(t *testing.T) {
	ctx := context.Background()
	src := newStaticSource()
	full, other := uuid.New(), uuid.New()
	src.set(full, TenantQuota{Streams: 2})
	src.set(other, TenantQuota{Streams: 2})
	edge := NewEdge(src, nil)

	releaseA, _, err := edge.AcquireStream(ctx, full)
	if err != nil {
		t.Fatalf("first stream: %v", err)
	}
	releaseB, _, err := edge.AcquireStream(ctx, full)
	if err != nil {
		t.Fatalf("second stream: %v", err)
	}
	_, retry, err := edge.AcquireStream(ctx, full)
	if !errors.Is(err, ErrStreamQuota) {
		t.Fatalf("third stream: want ErrStreamQuota, got %v", err)
	}
	if retry <= 0 {
		t.Fatalf("a stream rejection must carry a Retry-After hint, got %v", retry)
	}
	// The saturated tenant must not have consumed another tenant's capacity.
	releaseC, _, err := edge.AcquireStream(ctx, other)
	if err != nil {
		t.Fatalf("another tenant was refused while one tenant was saturated: %v", err)
	}
	releaseC()
	if edge.ActiveStreams(full) != 2 {
		t.Fatalf("active streams = %d, want 2", edge.ActiveStreams(full))
	}
	releaseA()
	releaseA() // idempotent: a double release must not free a second slot
	releaseB()
	if got := edge.ActiveStreams(full); got != 0 {
		t.Fatalf("active after release = %d", got)
	}
	// Capacity is available again.
	release, _, err := edge.AcquireStream(ctx, full)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	release()
}

func TestEdgeUnlimitedQuotaAdmitsEverything(t *testing.T) {
	ctx := context.Background()
	src := newStaticSource()
	tenant := uuid.New()
	src.set(tenant, TenantQuota{})
	edge := NewEdge(src, nil)
	for i := 0; i < 50; i++ {
		release, _, err := edge.AcquireStream(ctx, tenant)
		if err != nil {
			t.Fatalf("admission %d: %v", i, err)
		}
		defer release()
	}
}

func TestEdgeNilIsNoop(t *testing.T) {
	ctx := context.Background()
	var edge *Edge
	release, _, err := edge.AcquireStream(ctx, uuid.New())
	if err != nil {
		t.Fatalf("nil edge refused: %v", err)
	}
	release()
	if _, err := edge.AdmitStorage(ctx, uuid.New(), 1<<30); err != nil {
		t.Fatalf("nil edge storage: %v", err)
	}
	if err := edge.WaitBandwidth(ctx, uuid.New(), 1000); err != nil {
		t.Fatalf("nil edge bandwidth: %v", err)
	}
	if edge.ActiveStreams(uuid.New()) != 0 {
		t.Fatal("nil edge reported active streams")
	}
}

func TestEdgeUnknownTenantIsAnError(t *testing.T) {
	ctx := context.Background()
	edge := NewEdge(newStaticSource(), nil)
	if _, _, err := edge.AcquireStream(ctx, uuid.New()); err == nil {
		t.Fatal("an unknown tenant was admitted (a zero quota must not mean unlimited)")
	}
}

func TestEdgeQuotaCacheHonoursTTL(t *testing.T) {
	ctx := context.Background()
	src := newStaticSource()
	tenant := uuid.New()
	src.set(tenant, TenantQuota{Streams: 1})
	edge := NewEdge(src, nil)
	edge.SetTTL(time.Millisecond)
	release, _, err := edge.AcquireStream(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	release()
	first := src.lookups(tenant)
	// Within the TTL the row is cached.
	release, _, err = edge.AcquireStream(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if src.lookups(tenant) != first {
		t.Fatalf("the cache did not serve the second admission (%d lookups)", src.lookups(tenant))
	}
	time.Sleep(5 * time.Millisecond)
	src.set(tenant, TenantQuota{Streams: 5})
	release, _, err = edge.AcquireStream(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if src.lookups(tenant) <= first {
		t.Fatal("the cache never expired: a quota change would not take effect")
	}
}

func TestEdgeStorageQuota(t *testing.T) {
	ctx := context.Background()
	src := newStaticSource()
	tenant, other := uuid.New(), uuid.New()
	src.set(tenant, TenantQuota{StorageBytes: 1000})
	src.set(other, TenantQuota{StorageBytes: 1000})
	used := map[uuid.UUID]int64{tenant: 900, other: 0}
	var mu sync.Mutex
	edge := NewEdge(src, func(_ context.Context, tenantID uuid.UUID) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		return used[tenantID], nil
	})

	if _, err := edge.AdmitStorage(ctx, tenant, 50); err != nil {
		t.Fatalf("50 bytes under the ceiling: %v", err)
	}
	remaining, err := edge.AdmitStorage(ctx, tenant, 200)
	if !errors.Is(err, ErrStorageQuota) {
		t.Fatalf("200 bytes over the ceiling: %v", err)
	}
	if remaining != 100 {
		t.Fatalf("remaining = %d, want 100", remaining)
	}
	// The other tenant still has its whole budget.
	if _, err := edge.AdmitStorage(ctx, other, 900); err != nil {
		t.Fatalf("a saturated tenant blocked another: %v", err)
	}
	// Deleting bytes releases quota: the measure is live, not a counter.
	mu.Lock()
	used[tenant] = 0
	mu.Unlock()
	if _, err := edge.AdmitStorage(ctx, tenant, 900); err != nil {
		t.Fatalf("storage was not released by the measure: %v", err)
	}
}

func TestEdgeBandwidthShapesButDoesNotRefuse(t *testing.T) {
	ctx := context.Background()
	src := newStaticSource()
	tenant := uuid.New()
	// 10 KB/s: a 5 KB write fits the initial one-second burst, a 20 KB write does
	// not and must wait.
	src.set(tenant, TenantQuota{BytesPerSec: 10 << 10})
	edge := NewEdge(src, nil)
	if err := edge.WaitBandwidth(ctx, tenant, 5<<10); err != nil {
		t.Fatalf("a write inside the burst was shaped: %v", err)
	}
	start := time.Now()
	err := edge.WaitBandwidth(ctx, tenant, 20<<10)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrBandwidthQuota) {
		t.Fatalf("an over-budget write was not shaped: %v", err)
	}
	if elapsed <= 0 {
		t.Fatalf("the shaped write returned instantly (%v)", elapsed)
	}
	// Unlimited bandwidth never blocks and never reports shaping.
	unlimited := uuid.New()
	src.set(unlimited, TenantQuota{})
	before := time.Now()
	if err := edge.WaitBandwidth(ctx, unlimited, 10<<20); err != nil {
		t.Fatalf("unlimited tenant was shaped: %v", err)
	}
	if time.Since(before) > 50*time.Millisecond {
		t.Fatal("an unlimited tenant was delayed")
	}
}

func TestEdgeBandwidthRespectsContext(t *testing.T) {
	src := newStaticSource()
	tenant := uuid.New()
	src.set(tenant, TenantQuota{BytesPerSec: 1 << 10})
	edge := NewEdge(src, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A canceled context aborts the wait rather than parking the caller.
	if err := edge.WaitBandwidth(ctx, tenant, 10<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait = %v", err)
	}
}

func TestWriteErrorMapsStatusAndRetryAfter(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		status    int
		retryWait bool
	}{
		{"stream", fmt.Errorf("%w: tenant x", ErrStreamQuota), http.StatusTooManyRequests, true},
		{"run", fmt.Errorf("%w: tenant x", ErrRunConcurrency), http.StatusTooManyRequests, true},
		{"storage", fmt.Errorf("%w: tenant x", ErrStorageQuota), http.StatusRequestEntityTooLarge, false},
		{"unresolvable", fmt.Errorf("%w: tenant x", ErrConfig), http.StatusServiceUnavailable, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if !WriteError(recorder, tc.err) {
				t.Fatalf("%s was not handled", tc.name)
			}
			if recorder.Code != tc.status {
				t.Fatalf("%s status = %d, want %d", tc.name, recorder.Code, tc.status)
			}
			retry := recorder.Header().Get("Retry-After")
			if tc.retryWait && retry == "" {
				t.Fatalf("%s has no Retry-After", tc.name)
			}
			if !tc.retryWait && retry != "" {
				t.Fatalf("%s should not carry Retry-After", tc.name)
			}
			if recorder.Body.Len() == 0 {
				t.Fatalf("%s answered no body", tc.name)
			}
		})
	}
	// An unrelated error is not this package's business.
	if WriteError(httptest.NewRecorder(), errors.New("boom")) {
		t.Fatal("an unrelated error was reported as handled")
	}
}

// TestCheckStoredDoesNotDoubleCharge pins the input-time quota re-check: it
// measures what is already stored (the referenced object was counted at
// completion) and refuses only when the tenant is over the ceiling.
func TestCheckStoredDoesNotDoubleCharge(t *testing.T) {
	ctx := context.Background()
	src := newStaticSource()
	tenant, other := uuid.New(), uuid.New()
	src.set(tenant, TenantQuota{StorageBytes: 100})
	src.set(other, TenantQuota{StorageBytes: 100})
	used := map[uuid.UUID]int64{tenant: 100, other: 60}
	edge := NewEdge(src, func(_ context.Context, tenantID uuid.UUID) (int64, error) {
		return used[tenantID], nil
	})
	if stored, limit, err := edge.CheckStored(ctx, tenant); err != nil || stored != 100 || limit != 100 {
		t.Fatalf("at the ceiling: stored=%d limit=%d err=%v", stored, limit, err)
	}
	if _, _, err := edge.CheckStored(ctx, other); err != nil {
		t.Fatalf("under the ceiling: %v", err)
	}
	used[tenant] = 101
	_, _, err := edge.CheckStored(ctx, tenant)
	if !errors.Is(err, ErrStorageQuota) {
		t.Fatalf("over the ceiling: %v", err)
	}
	if !strings.Contains(err.Error(), "limit is 100") {
		t.Fatalf("the refusal must name the limit: %v", err)
	}
	// Unlimited is a no-op; a nil edge is a no-op as well.
	unlimited := uuid.New()
	src.set(unlimited, TenantQuota{})
	if _, _, err := edge.CheckStored(ctx, unlimited); err != nil {
		t.Fatalf("unlimited: %v", err)
	}
	var nilEdge *Edge
	if _, _, err := nilEdge.CheckStored(ctx, tenant); err != nil {
		t.Fatalf("nil edge: %v", err)
	}
}
