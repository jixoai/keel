package quota

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1700000000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestRunConcurrencyLimit(t *testing.T) {
	l := New(host.QuotaConfig{RunConcurrency: 2})
	a := kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}
	b := kernel.Scope{TenantID: a.TenantID, SessionID: uuid.New()}
	if err := l.AdmitRun(context.Background(), a); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := l.AdmitRun(context.Background(), b); err != nil {
		t.Fatalf("second run: %v", err)
	}
	err := l.AdmitRun(context.Background(), kernel.Scope{TenantID: a.TenantID, SessionID: uuid.New()})
	if !errors.Is(err, ErrRunConcurrency) {
		t.Fatalf("third run: want ErrRunConcurrency, got %v", err)
	}
	// A rejected admission must not consume a slot.
	if got := l.Active(a.TenantID); got != 2 {
		t.Fatalf("active after rejection: %d, want 2", got)
	}
	l.ReleaseRun(context.Background(), a)
	if err := l.AdmitRun(context.Background(), kernel.Scope{TenantID: a.TenantID, SessionID: uuid.New()}); err != nil {
		t.Fatalf("after release: %v", err)
	}
	// Releasing a slot that was never taken clamps at zero.
	l.ReleaseRun(context.Background(), kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()})
}

func TestMultiTenantIsolation(t *testing.T) {
	l := New(host.QuotaConfig{RunConcurrency: 1, EventsPerMinute: 1})
	tenantA, tenantB := uuid.New(), uuid.New()
	scopeA := kernel.Scope{TenantID: tenantA, SessionID: uuid.New()}
	scopeB := kernel.Scope{TenantID: tenantB, SessionID: uuid.New()}
	if err := l.AdmitRun(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	if err := l.AdmitRun(context.Background(), kernel.Scope{TenantID: tenantA, SessionID: uuid.New()}); !errors.Is(err, ErrRunConcurrency) {
		t.Fatalf("tenant A should be capped: %v", err)
	}
	if err := l.AdmitRun(context.Background(), scopeB); err != nil {
		t.Fatalf("tenant B must not be affected: %v", err)
	}
	if err := l.AdmitEvent(context.Background(), scopeA, kernel.KindUserMessage); err != nil {
		t.Fatal(err)
	}
	if err := l.AdmitEvent(context.Background(), scopeA, kernel.KindUserMessage); !errors.Is(err, ErrEventRate) {
		t.Fatalf("tenant A event budget should be spent: %v", err)
	}
	if err := l.AdmitEvent(context.Background(), scopeB, kernel.KindUserMessage); err != nil {
		t.Fatalf("tenant B event budget must be independent: %v", err)
	}
}

func TestEventRateRefills(t *testing.T) {
	clock := newFakeClock()
	l := NewWithClock(host.QuotaConfig{EventsPerMinute: 2}, clock.Now)
	scope := kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}
	for i := 0; i < 2; i++ {
		if err := l.AdmitEvent(context.Background(), scope, kernel.KindUserMessage); err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
	}
	if err := l.AdmitEvent(context.Background(), scope, kernel.KindUserMessage); !errors.Is(err, ErrEventRate) {
		t.Fatalf("budget should be spent: %v", err)
	}
	// One event per 30s at 2/min.
	clock.Advance(29 * time.Second)
	if err := l.AdmitEvent(context.Background(), scope, kernel.KindUserMessage); !errors.Is(err, ErrEventRate) {
		t.Fatalf("must not refill before 30s: %v", err)
	}
	clock.Advance(2 * time.Second)
	if err := l.AdmitEvent(context.Background(), scope, kernel.KindUserMessage); err != nil {
		t.Fatalf("after refill: %v", err)
	}
}

func TestZeroQuotaAdmitsEverything(t *testing.T) {
	l := New(host.QuotaConfig{})
	scope := kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}
	for i := 0; i < 1000; i++ {
		if err := l.AdmitRun(context.Background(), scope); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if err := l.AdmitEvent(context.Background(), scope, kernel.KindUserMessage); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
}

func TestUpdateAppliesToNewAdmissions(t *testing.T) {
	l := New(host.QuotaConfig{RunConcurrency: 2})
	scope := kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}
	if err := l.AdmitRun(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if err := l.AdmitRun(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	l.Update(host.QuotaConfig{RunConcurrency: 1})
	// In-flight reservations survive; new claims see the lower limit.
	if err := l.AdmitRun(context.Background(), scope); !errors.Is(err, ErrRunConcurrency) {
		t.Fatalf("lowered limit must gate new claims: %v", err)
	}
	l.ReleaseRun(context.Background(), scope)
	l.ReleaseRun(context.Background(), scope)
	if err := l.AdmitRun(context.Background(), scope); err != nil {
		t.Fatalf("drained below the new limit: %v", err)
	}
}

func TestConcurrentAdmission(t *testing.T) {
	l := New(host.QuotaConfig{RunConcurrency: 4, EventsPerMinute: 1000000})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scope := kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}
			for j := 0; j < 50; j++ {
				if err := l.AdmitRun(context.Background(), scope); err != nil {
					t.Errorf("admit: %v", err)
					return
				}
				l.ReleaseRun(context.Background(), scope)
				if err := l.AdmitEvent(context.Background(), scope, kernel.KindUserMessage); err != nil {
					t.Errorf("event: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestKernelRunQuotaWired proves the seam is on the production claim path: a
// claim over the tenant limit is rejected (and the claim undone), and a finish
// returns the slot so the next claim succeeds.
func TestKernelRunQuotaWired(t *testing.T) {
	ctx := context.Background()
	mem := kernel.NewMemStore()
	lim := New(host.QuotaConfig{RunConcurrency: 1})
	k := kernel.New(mem, kernel.Options{Limits: lim})
	tenant := uuid.New()
	first, second := uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{first, second} {
		mem.CreateSession(tenant, session)
		mem.EnqueueRun(tenant, session)
	}
	lease, err := k.Claim(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := k.Claim(ctx, "worker", time.Minute); !errors.Is(err, kernel.ErrQuota) {
		t.Fatalf("second claim: want ErrQuota, got %v", err)
	}
	if got := lim.Active(tenant); got != 1 {
		t.Fatalf("active after quota rejection: %d, want 1", got)
	}
	// The rejected scope must be claimable again once the slot frees up.
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got := lim.Active(tenant); got != 0 {
		t.Fatalf("slot was not released on finish: %d", got)
	}
	if _, err := k.Claim(ctx, "worker", time.Minute); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
}

// TestKernelEventQuotaWired proves the commit path consults AdmitEvent.
func TestKernelEventQuotaWired(t *testing.T) {
	ctx := context.Background()
	mem := kernel.NewMemStore()
	clock := newFakeClock()
	lim := NewWithClock(host.QuotaConfig{EventsPerMinute: 1}, clock.Now)
	k := kernel.New(mem, kernel.Options{Limits: lim})
	tenant := uuid.New()
	scope := kernel.Scope{TenantID: tenant, SessionID: uuid.New()}
	mem.CreateSession(tenant, scope.SessionID)
	mem.EnqueueRun(tenant, scope.SessionID)

	commit := func(requestID string) error {
		body, _ := json.Marshal(map[string]string{"text": "hello"})
		_, err := k.Commit(ctx, kernel.CommitRequest{
			Kind:      kernel.KindUserMessage,
			Scope:     scope,
			Payload:   body,
			RequestID: requestID,
		})
		return err
	}
	if err := commit("r1"); err != nil {
		t.Fatalf("first event: %v", err)
	}
	if err := commit("r2"); !errors.Is(err, kernel.ErrQuota) {
		t.Fatalf("second event: want ErrQuota, got %v", err)
	}
	clock.Advance(time.Minute)
	if err := commit("r3"); err != nil {
		t.Fatalf("event after refill: %v", err)
	}
}

// fakeSource serves stored tenant documents and can be made to fail.
type fakeSource struct {
	mu      sync.Mutex
	docs    map[uuid.UUID]string
	reads   int
	failErr error
}

func (s *fakeSource) TenantDocument(_ context.Context, tenantID uuid.UUID) ([]byte, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.failErr != nil {
		return nil, 0, s.failErr
	}
	doc, ok := s.docs[tenantID]
	if !ok {
		doc = "{}"
	}
	return []byte(doc), 1, nil
}

func (s *fakeSource) set(tenantID uuid.UUID, doc string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.docs == nil {
		s.docs = map[uuid.UUID]string{}
	}
	s.docs[tenantID] = doc
}

func TestStoredTenantDocumentOverridesDefault(t *testing.T) {
	clock := newFakeClock()
	src := &fakeSource{}
	tenantA, tenantB := uuid.New(), uuid.New()
	src.set(tenantA, `{"quota":{"runConcurrency":1,"eventsPerMinute":1}}`)
	src.set(tenantB, `{"quota":{"runConcurrency":3}}`)
	l := NewWithClock(host.QuotaConfig{RunConcurrency: 5}, clock.Now).WithSource(src)
	ctx := context.Background()

	scopeA := kernel.Scope{TenantID: tenantA, SessionID: uuid.New()}
	if err := l.AdmitRun(ctx, scopeA); err != nil {
		t.Fatal(err)
	}
	if err := l.AdmitRun(ctx, kernel.Scope{TenantID: tenantA, SessionID: uuid.New()}); !errors.Is(err, ErrRunConcurrency) {
		t.Fatalf("stored quota must lower the limit: %v", err)
	}
	// Tenant B's document is independent (and does not inherit A's).
	for i := 0; i < 3; i++ {
		if err := l.AdmitRun(ctx, kernel.Scope{TenantID: tenantB, SessionID: uuid.New()}); err != nil {
			t.Fatalf("tenant B run %d: %v", i, err)
		}
	}
	if err := l.AdmitRun(ctx, kernel.Scope{TenantID: tenantB, SessionID: uuid.New()}); !errors.Is(err, ErrRunConcurrency) {
		t.Fatalf("tenant B should be capped at 3: %v", err)
	}
	// The event bucket comes from the document too.
	if err := l.AdmitEvent(ctx, kernel.Scope{TenantID: tenantA, SessionID: uuid.New()}, kernel.KindUserMessage); err != nil {
		t.Fatal(err)
	}
	if err := l.AdmitEvent(ctx, kernel.Scope{TenantID: tenantA, SessionID: uuid.New()}, kernel.KindUserMessage); !errors.Is(err, ErrEventRate) {
		t.Fatalf("event budget should come from the stored document: %v", err)
	}
}

func TestStoredDocumentRefreshAfterTTL(t *testing.T) {
	clock := newFakeClock()
	src := &fakeSource{}
	tenant := uuid.New()
	src.set(tenant, `{"quota":{"runConcurrency":1}}`)
	l := NewWithClock(host.QuotaConfig{}, clock.Now).WithSource(src)
	l.SetTTL(time.Second)
	ctx := context.Background()
	scope := kernel.Scope{TenantID: tenant, SessionID: uuid.New()}
	if err := l.AdmitRun(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if err := l.AdmitRun(ctx, scope); !errors.Is(err, ErrRunConcurrency) {
		t.Fatalf("limit 1: %v", err)
	}
	src.set(tenant, `{"quota":{"runConcurrency":2}}`)
	// Cached: the change is invisible until the TTL passes.
	if err := l.AdmitRun(ctx, scope); !errors.Is(err, ErrRunConcurrency) {
		t.Fatalf("cached quota should still say 1: %v", err)
	}
	clock.Advance(2 * time.Second)
	if err := l.AdmitRun(ctx, scope); err != nil {
		t.Fatalf("refreshed quota should admit: %v", err)
	}
}

func TestConfigFailureIsFailClosed(t *testing.T) {
	src := &fakeSource{failErr: errors.New("db down")}
	l := New(host.QuotaConfig{}).WithSource(src)
	scope := kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}
	if err := l.AdmitRun(context.Background(), scope); !errors.Is(err, ErrConfig) {
		t.Fatalf("run admission must fail closed: %v", err)
	}
	if err := l.AdmitEvent(context.Background(), scope, kernel.KindUserMessage); !errors.Is(err, ErrConfig) {
		t.Fatalf("event admission must fail closed: %v", err)
	}
	l.ReleaseRun(context.Background(), scope) // no reservation was made
	if got := l.Active(scope.TenantID); got != 0 {
		t.Fatalf("release must not underflow: %d", got)
	}
}

func TestMalformedStoredDocumentIsFailClosed(t *testing.T) {
	src := &fakeSource{}
	tenant := uuid.New()
	src.set(tenant, `{"quota":{"runConcurrency":-1}}`)
	l := New(host.QuotaConfig{}).WithSource(src)
	scope := kernel.Scope{TenantID: tenant, SessionID: uuid.New()}
	if err := l.AdmitRun(context.Background(), scope); !errors.Is(err, ErrConfig) {
		t.Fatalf("invalid stored quota must fail closed: %v", err)
	}
}
