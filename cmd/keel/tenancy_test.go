// Tenancy wiring tests for `keel serve` and `keel admin`: the CLI provisioning
// path (plaintext shown once, nothing else prints it), the API-key middleware
// chain, and the per-tenant edge quota middleware (change keel-ga §2).

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/store"
)

// testStore opens a dedicated database for one test so the CLI assertions do
// not collide with other packages sharing KEEL_TEST_DSN.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("KEEL_TEST_DSN"))
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestAdminUsageAndQuotaCommands(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KEEL_TEST_DSN"))
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	t.Setenv("KEEL_DSN", dsn)
	var stdout, stderr strings.Builder
	if code := runAdminCmd([]string{"tenant", "create", "--name", "cli-usage"}, &stdout, &stderr); code != 0 {
		t.Fatalf("create: %d %s", code, stderr.String())
	}
	tenantID := strings.TrimSpace(strings.SplitN(stdout.String(), "\n", 2)[0][len("tenant:  "):])

	stdout.Reset()
	stderr.Reset()
	if code := runAdminCmd([]string{"quota", "set", "--tenant", tenantID, "--streams", "7", "--storage-bytes", "1024"}, &stdout, &stderr); code != 0 {
		t.Fatalf("quota set: %d %s", code, stderr.String())
	}
	stdout.Reset()
	if code := runAdminCmd([]string{"quota", "show", "--tenant", tenantID}, &stdout, &stderr); code != 0 {
		t.Fatalf("quota show: %d %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "streams:    7") {
		t.Fatalf("quota show did not report the new limit:\n%s", stdout.String())
	}

	stdout.Reset()
	if code := runAdminCmd([]string{"usage", "show", "--tenant", tenantID}, &stdout, &stderr); code != 0 {
		t.Fatalf("usage show: %d %s", code, stderr.String())
	}
	usageOut := stdout.String()
	if !strings.Contains(usageOut, "tokens") || !strings.Contains(usageOut, "bytes") {
		t.Fatalf("usage output lacks the two dimensions:\n%s", usageOut)
	}
	for _, forbidden := range []string{"price", "amount", "currency", "cost"} {
		if strings.Contains(strings.ToLower(usageOut), forbidden) {
			t.Fatalf("usage output carries %q:\n%s", forbidden, usageOut)
		}
	}
}

// TestEdgeQuotaMiddlewareRejectsSaturatedTenant pins the wiring: the middleware
// charges the resolved tenant, a saturated tenant is 429 with Retry-After, and
// the neighbouring tenant keeps its capacity.
func TestEdgeQuotaMiddlewareRejectsSaturatedTenant(t *testing.T) {
	full, other := uuid.New(), uuid.New()
	src := quota.QuotaSourceFunc(func(_ context.Context, tenantID uuid.UUID) (quota.TenantQuota, error) {
		if tenantID == full || tenantID == other {
			return quota.TenantQuota{Streams: 1}, nil
		}
		return quota.TenantQuota{}, nil
	})
	edge := quota.NewEdge(src, nil)
	var reached int
	middleware := &edgeQuotaMiddleware{Edge: edge, Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	})}
	server := httptest.NewServer(middleware)
	defer server.Close()

	// Hold one stream for `full` so the next one is refused.
	release, _, err := edge.AcquireStream(context.Background(), full)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/sessions/x/events/stream", nil)
	req.Header.Set("X-Tenant-ID", full.String())
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("saturated tenant stream: %d, want 429", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Fatal("the 429 carries no Retry-After")
	}
	// The other tenant is unaffected.
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/sessions/x/events/stream", nil)
	req.Header.Set("X-Tenant-ID", other.String())
	res, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("neighbour tenant: %d, want 200", res.StatusCode)
	}
	release()

	// A JSON path is not charged at all (the middleware is a no-op there).
	before := edge.ActiveStreams(full)
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/sessions", nil)
	req.Header.Set("X-Tenant-ID", full.String())
	res, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("non-stream path: %d", res.StatusCode)
	}
	if edge.ActiveStreams(full) != before {
		t.Fatal("a JSON path consumed a stream slot")
	}
}

// TestEdgeQuotaMiddlewareRejectsOversizedUpload pins the 413 dimension.
func TestEdgeQuotaMiddlewareRejectsOversizedUpload(t *testing.T) {
	tenant := uuid.New()
	src := quota.QuotaSourceFunc(func(context.Context, uuid.UUID) (quota.TenantQuota, error) {
		return quota.TenantQuota{Streams: 5, StorageBytes: 1000}, nil
	})
	edge := quota.NewEdge(src, func(context.Context, uuid.UUID) (int64, error) { return 900, nil })
	var reached bool
	middleware := &edgeQuotaMiddleware{Edge: edge, Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusCreated)
	})}
	server := httptest.NewServer(middleware)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/files", strings.NewReader(strings.Repeat("x", 200)))
	req.Header.Set("X-Tenant-ID", tenant.String())
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d, want 413", res.StatusCode)
	}
	if reached {
		t.Fatal("the refused upload reached the handler")
	}
	// A write that fits passes and consumes a stream slot that is then released.
	reached = false
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/files", strings.NewReader(strings.Repeat("x", 50)))
	req.Header.Set("X-Tenant-ID", tenant.String())
	res, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated || !reached {
		t.Fatalf("fitting upload: %d reached=%v", res.StatusCode, reached)
	}
	if got := edge.ActiveStreams(tenant); got != 0 {
		t.Fatalf("the upload slot leaked: %d active", got)
	}
}
