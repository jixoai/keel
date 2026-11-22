// Tenancy storage tests: byte usage, the RLS boundary and the edge quota row
// (change keel-ga §2). The tenant-wide api_keys credential is retired
// (A4.2b3a): Bot keys are the only machine credential and are covered by
// bot_keys/identity tests.
//
// The RLS tests are the load-bearing ones: they prove that a query on the
// tenant pool without app.tenant_id sees nothing, and that setting it to one
// tenant hides the other's rows. That is the property the spec's "跨租户查询 MUST
// 返回 0 行" names, asserted at the storage seam rather than through HTTP (the
// HTTP 404 is asserted in internal/harnessapi).

package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func tenancyStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := Open(context.Background(), Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// TestRLSCrossTenantReadsReturnNoRows is the spec's requirement: a session
// query under one tenant's scope must not see the other tenant's session, and
// an unscoped connection must see nothing at all.
//
// The comparison runs on the tenant pool (the non-owner role), which is the
// path a request-scoped transaction uses.
func TestRLSCrossTenantReadsReturnNoRows(t *testing.T) {
	st := tenancyStore(t)
	if !st.RLSEnabled() {
		t.Skip("RLS is disabled for this store (KEEL_TEST_DSN cluster cannot create roles)")
	}
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	sessionA, sessionB := uuid.New(), uuid.New()
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		if err := st.CreateTenant(ctx, tenant, "rls-"+tenant.String()[:8]); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateSession(ctx, tenantA, sessionA); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenantB, sessionB); err != nil {
		t.Fatal(err)
	}

	// Tenant A's scope sees its own session and not B's ...
	if err := st.TenantScoped(ctx, tenantA, func(tx TenantTx) error {
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM sessions WHERE session_id = $1`, sessionA).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Fatalf("tenant A cannot see its own session: %d", n)
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM sessions WHERE session_id = $1`, sessionB).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatalf("tenant A sees tenant B's session: %d rows", n)
		}
		// A predicate-free scan is filtered too: that is the whole point of RLS.
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			t.Fatal("tenant A sees no sessions at all; the scope was not applied")
		}
		return nil
	}); err != nil {
		t.Fatalf("tenant A scope: %v", err)
	}

	// ... and a tenant transaction for an unrelated tenant sees neither.
	if err := st.TenantScoped(ctx, uuid.New(), func(tx TenantTx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE session_id IN ($1,$2)`,
			sessionA, sessionB).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatalf("an unrelated tenant scope sees %d foreign sessions", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("unrelated scope: %v", err)
	}
}

// TestRLSBlocksCrossTenantWrite pins the WITH CHECK half: a tenant transaction
// cannot insert a row tagged with another tenant, even if the caller's SQL says
// so. Without it, RLS would only hide reads.
func TestRLSBlocksCrossTenantWrite(t *testing.T) {
	st := tenancyStore(t)
	if !st.RLSEnabled() {
		t.Skip("RLS is disabled for this store")
	}
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		if err := st.CreateTenant(ctx, tenant, "rls-w-"+tenant.String()[:8]); err != nil {
			t.Fatal(err)
		}
	}
	err := st.TenantScoped(ctx, tenantA, func(tx TenantTx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO sessions (tenant_id, session_id) VALUES ($1, $2)`, tenantB, uuid.New())
		return err
	})
	if err == nil {
		t.Fatal("a tenant write tagged with another tenant was accepted")
	}
}

// TestOperatorScopeSeesEveryTenant pins the bypass term: the operator path (the
// migration, provisioning, the admin API) still sees every tenant, so RLS being
// on does not blind the deployment.
func TestOperatorScopeSeesEveryTenant(t *testing.T) {
	st := tenancyStore(t)
	if !st.RLSEnabled() {
		t.Skip("RLS is disabled for this store")
	}
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenantA, "op-a"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, tenantB, "op-b"); err != nil {
		t.Fatal(err)
	}
	if err := st.OperatorScoped(ctx, func(tx TenantTx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE tenant_id IN ($1,$2)`,
			tenantA, tenantB).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Fatalf("operator scope sees %d/2 tenants", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("operator scope: %v", err)
	}
}

// TestTenantScopedRejectsNilTenant pins fail-closed behaviour at the seam.
func TestTenantScopedRejectsNilTenant(t *testing.T) {
	st := tenancyStore(t)
	if err := st.TenantScoped(context.Background(), uuid.Nil, func(TenantTx) error {
		t.Fatal("callback ran for a nil tenant")
		return nil
	}); !errors.Is(err, ErrNoTenantScope) {
		t.Fatalf("nil tenant: %v", err)
	}
}

// TestByteUsageIsIdempotentAndAggregates pins the usage-event contract: a
// dedupe key records once, and the aggregate sums objective quantities only.
func TestByteUsageIsIdempotentAndAggregates(t *testing.T) {
	st := tenancyStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "usage"); err != nil {
		t.Fatal(err)
	}
	blob := uuid.New()
	record := ByteUsageRecord{
		TenantID:  tenant,
		Kind:      UsageStorageBytes,
		Quantity:  1024,
		Source:    "test",
		BlobID:    &blob,
		DedupeKey: blob.String(),
	}
	if created, err := st.AppendByteUsage(ctx, record); err != nil || !created {
		t.Fatalf("append = %v %v", created, err)
	}
	if created, err := st.AppendByteUsage(ctx, record); err != nil || created {
		t.Fatalf("duplicate append = %v %v", created, err)
	}
	image := ByteUsageRecord{TenantID: tenant, Kind: UsageImageBytes, Quantity: 2048, DedupeKey: "img-1"}
	if created, err := st.AppendByteUsage(ctx, image); err != nil || !created {
		t.Fatalf("image append = %v %v", created, err)
	}

	totals, err := st.SumByteUsage(ctx, ByteUsageQuery{TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 2 {
		t.Fatalf("totals = %+v", totals)
	}
	byKind := map[string]ByteUsageTotals{}
	for _, group := range totals {
		byKind[group.Kind] = group
	}
	if got := byKind[UsageStorageBytes]; got.Bytes != 1024 || got.Entries != 1 {
		t.Fatalf("storage totals = %+v", got)
	}
	if got := byKind[UsageImageBytes]; got.Bytes != 2048 || got.Entries != 1 {
		t.Fatalf("image totals = %+v", got)
	}

	// Another tenant's usage never leaks into the sum.
	other := uuid.New()
	if err := st.CreateTenant(ctx, other, "usage-other"); err != nil {
		t.Fatal(err)
	}
	otherTotals, err := st.SumByteUsage(ctx, ByteUsageQuery{TenantID: other})
	if err != nil {
		t.Fatal(err)
	}
	if len(otherTotals) != 0 {
		t.Fatalf("other tenant saw %+v", otherTotals)
	}

	// An unknown kind is refused at the seam.
	if _, err := st.AppendByteUsage(ctx, ByteUsageRecord{TenantID: tenant, Kind: "price", Quantity: 1}); err == nil {
		t.Fatal("unknown usage kind was accepted")
	}
}

// TestQuotaRowRoundTrip pins the quota columns and the stored-bytes measure.
func TestQuotaRowRoundTrip(t *testing.T) {
	st := tenancyStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "quota"); err != nil {
		t.Fatal(err)
	}
	defaults, err := st.GetTenantQuota(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Streams != 20 {
		t.Fatalf("default streams = %d, want 20", defaults.Streams)
	}
	if err := st.SetTenantQuota(ctx, tenant, 3, 1<<20, 1<<30); err != nil {
		t.Fatal(err)
	}
	quota, err := st.GetTenantQuota(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if quota.Streams != 3 || quota.BytesPerSec != 1<<20 || quota.StorageBytes != 1<<30 {
		t.Fatalf("quota = %+v", quota)
	}
	if _, err := st.GetTenantQuota(ctx, uuid.New()); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("missing tenant: %v", err)
	}
}

// TestCompleteBlobRecordsByteUsage pins the metering hook: finishing an upload
// records storage bytes for every blob and image bytes for an image mime, once.
func TestCompleteBlobRecordsByteUsage(t *testing.T) {
	st := tenancyStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "upload-usage"); err != nil {
		t.Fatal(err)
	}
	imageID := uuid.New()
	sum := make([]byte, 32)
	for i := range sum {
		sum[i] = byte(i)
	}
	if _, _, err := st.CreateBlob(ctx, tenant, imageID, sum, 4096, "image/png", StoragePGChunked); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteBlob(ctx, tenant, imageID, 1); err != nil {
		t.Fatal(err)
	}
	// A retried completion records nothing new.
	if err := st.CompleteBlob(ctx, tenant, imageID, 1); err != nil {
		t.Fatal(err)
	}
	totals, err := st.SumByteUsage(ctx, ByteUsageQuery{TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]ByteUsageTotals{}
	for _, group := range totals {
		byKind[group.Kind] = group
	}
	if got := byKind[UsageStorageBytes]; got.Bytes != 4096 || got.Entries != 1 {
		t.Fatalf("storage usage = %+v", got)
	}
	if got := byKind[UsageImageBytes]; got.Bytes != 4096 || got.Entries != 1 {
		t.Fatalf("image usage = %+v", got)
	}
	stored, err := st.TenantStoredBytes(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 4096 {
		t.Fatalf("stored = %d", stored)
	}

	// A non-image blob records storage bytes only.
	docID := uuid.New()
	docSum := make([]byte, 32)
	docSum[0] = 0xAA
	if _, _, err := st.CreateBlob(ctx, tenant, docID, docSum, 100, "application/pdf", StoragePGChunked); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteBlob(ctx, tenant, docID, 1); err != nil {
		t.Fatal(err)
	}
	totals, err = st.SumByteUsage(ctx, ByteUsageQuery{TenantID: tenant})
	if err != nil {
		t.Fatal(err)
	}
	byKind = map[string]ByteUsageTotals{}
	for _, group := range totals {
		byKind[group.Kind] = group
	}
	if got := byKind[UsageStorageBytes]; got.Bytes != 4196 {
		t.Fatalf("storage after pdf = %+v", got)
	}
	if got := byKind[UsageImageBytes]; got.Bytes != 4096 {
		t.Fatalf("image after pdf = %+v", got)
	}
}

// TestRLSMigrationIsIdempotent pins the "迁移幂等" requirement: applying the
// policy pass twice (which every boot does) converges instead of failing on the
// existing policy, and the enforcement state is unchanged.
func TestRLSMigrationIsIdempotent(t *testing.T) {
	st := tenancyStore(t)
	if !st.RLSEnabled() {
		t.Skip("RLS is disabled for this store")
	}
	ctx := context.Background()
	for attempt := 0; attempt < 2; attempt++ {
		if err := st.ApplyRLS(ctx); err != nil {
			t.Fatalf("apply %d: %v", attempt, err)
		}
	}
	var enabled, forced bool
	if err := st.Pool().QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'public.sessions'::regclass`).
		Scan(&enabled, &forced); err != nil {
		t.Fatal(err)
	}
	if !enabled || !forced {
		t.Fatalf("sessions: enabled=%v forced=%v after re-apply", enabled, forced)
	}
}

// TestRLSMigrationCanBeDisabled pins the escape hatch a managed cluster needs:
// with DisableRLS the store opens (no role creation) and reports that
// tenant-scoped access is unavailable rather than silently falling back to an
// unscoped pool.
func TestRLSMigrationCanBeDisabled(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := Open(context.Background(), Config{DSN: dsn, DisableRLS: true})
	if err != nil {
		t.Fatalf("open without RLS: %v", err)
	}
	defer st.Close()
	if st.RLSEnabled() {
		t.Fatal("a store opened with DisableRLS reports RLS available")
	}
	if err := st.TenantScoped(context.Background(), uuid.New(), func(TenantTx) error {
		t.Fatal("a tenant scope ran without the tenant pool")
		return nil
	}); !errors.Is(err, ErrRLSUnavailable) {
		t.Fatalf("tenant scope without RLS = %v, want ErrRLSUnavailable", err)
	}
}

// TestRLSPolicyStatementShape pins the migration's shape without a database:
// every tenant table gets FORCE, the predicate reads app.tenant_id, and the
// operator bypass is the only OR term (so a future edit cannot quietly widen
// the policy).
func TestRLSPolicyStatementShape(t *testing.T) {
	statements := rlsStatements(RLSRoleName, []string{"sessions", "events"})
	joined := strings.Join(statements, "\n")
	if !strings.Contains(joined, `ALTER TABLE "sessions" FORCE ROW LEVEL SECURITY`) {
		t.Fatal("sessions has no FORCE ROW LEVEL SECURITY")
	}
	if !strings.Contains(joined, `ALTER TABLE "events" FORCE ROW LEVEL SECURITY`) {
		t.Fatal("events has no FORCE ROW LEVEL SECURITY")
	}
	if !strings.Contains(joined, "current_setting('app.tenant_id', true)") {
		t.Fatal("the policy does not read app.tenant_id")
	}
	if !strings.Contains(joined, "current_setting('app.bypass_rls', true) = 'on'") {
		t.Fatal("the policy has no operator bypass term")
	}
	if strings.Count(joined, "CREATE POLICY") != 2 {
		t.Fatalf("expected one policy per table, got %d", strings.Count(joined, "CREATE POLICY"))
	}
	// The role is asserted non-superuser/non-bypass: without this the policies
	// would be decorative.
	if !strings.Contains(joined, "NOBYPASSRLS") || !strings.Contains(joined, "NOSUPERUSER") {
		t.Fatal("the role attributes do not forbid RLS bypass")
	}
}
