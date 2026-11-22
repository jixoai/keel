package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// supervisorTestDSN is a fresh database owned by this test binary. The
// supervisor scans runs across tenants by design, so sharing the developer's
// database would make the tests observe each other's leftover runs.
var supervisorTestDSN string

func TestMain(m *testing.M) {
	base := strings.TrimSpace(os.Getenv("KEEL_TEST_DSN"))
	if base == "" {
		os.Exit(m.Run())
	}
	created, cleanup, err := createSupervisorTestDatabase(base)
	if err != nil {
		// The database tests skip when KEEL_TEST_DSN is unset; a set-but-unusable
		// DSN must fail loudly instead of silently skipping the integration proof.
		os.Stderr.WriteString("supervisor: create test database: " + err.Error() + "\n")
		os.Exit(1)
	}
	supervisorTestDSN = created
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func createSupervisorTestDatabase(base string) (string, func(), error) {
	ctx := context.Background()
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" {
		return "", nil, errors.New("KEEL_TEST_DSN must be a URL DSN for this package")
	}
	config, err := pgx.ParseConfig(base)
	if err != nil {
		return "", nil, err
	}
	admin := config.Copy()
	admin.Database = "postgres"
	conn, err := pgx.ConnectConfig(ctx, admin)
	if err != nil {
		return "", nil, err
	}
	name := "keel_supervisor_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		conn.Close(ctx)
		return "", nil, err
	}
	created := *parsed
	created.Path = "/" + name
	cleanup := func() {
		defer conn.Close(context.Background())
		_, _ = conn.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}
	return created.String(), cleanup, nil
}

func pgStoreForSupervisor(t *testing.T) *store.Store {
	t.Helper()
	if supervisorTestDSN == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := store.Open(context.Background(), store.Config{DSN: supervisorTestDSN})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	// The supervisor scans every tenant, so each test starts from an empty
	// queue/ledger. Tests in this package run sequentially.
	if _, err := st.Pool().Exec(context.Background(),
		`TRUNCATE TABLE events, runs, sessions, tenants CASCADE`); err != nil {
		t.Fatalf("reset supervisor test database: %v", err)
	}
	return st
}

func pgKernelForSupervisor(t *testing.T, st *store.Store) *kernel.Kernel {
	t.Helper()
	pg, err := kernel.NewPG(context.Background(), st.Pool())
	if err != nil {
		t.Fatalf("open kernel: %v", err)
	}
	return kernel.New(pg, kernel.Options{})
}

func pgSession(t *testing.T, st *store.Store) (uuid.UUID, uuid.UUID) {
	t.Helper()
	tenant, session := uuid.New(), uuid.New()
	ctx := context.Background()
	if err := st.CreateTenant(ctx, tenant, "supervisor-test"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	return tenant, session
}

func pgFailedRun(t *testing.T, st *store.Store, kern *kernel.Kernel, tenant, session uuid.UUID) {
	t.Helper()
	lease, err := kern.ClaimScope(context.Background(),
		kernel.Scope{TenantID: tenant, SessionID: session}, "failed-run-owner", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := kern.Finish(context.Background(), kernel.FinishRequest{
		Lease: lease, Err: errors.New("provider stream failed"), MaxAttempts: 1,
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	run, err := st.GetRun(context.Background(), tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunFailed {
		t.Fatalf("run status = %q, want failed", run.Status)
	}
}

func pgEvents(t *testing.T, st *store.Store, tenant, session uuid.UUID) []kernel.Event {
	t.Helper()
	events, err := st.LoadEvents(context.Background(), tenant, session, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]kernel.Event, 0, len(events))
	for _, event := range events {
		out = append(out, kernel.Event{Seq: event.Seq, Kind: kernel.EventKind(event.Kind), Payload: event.Payload})
	}
	return out
}

func TestPGSupervisorResumesFailedRunAndRecordsTheFact(t *testing.T) {
	st := pgStoreForSupervisor(t)
	kern := pgKernelForSupervisor(t, st)
	tenant, session := pgSession(t, st)
	pgFailedRun(t, st, kern, tenant, session)

	sup, err := New(Config{
		Store: NewSQLStore(st), Ledger: kern, Logger: testLogger(),
		Policy: func() (Policy, ConfigVersion) {
			return DefaultPolicy(), ConfigVersion{Hash: "sha256:pg", Revision: 3}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Recovered != 1 {
		t.Fatalf("report = %+v", report)
	}
	run, err := st.GetRun(context.Background(), tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunPending {
		t.Fatalf("run status = %q, want pending", run.Status)
	}
	count, err := st.AutoResumeCount(context.Background(), tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("auto resume count = %d, want 1", count)
	}
	var found bool
	for _, event := range pgEvents(t, st, tenant, session) {
		if event.Kind != kernel.KindResumeAttempt {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["actor"] != "supervisor" || payload["result"] != "requeued" ||
			payload["counted"] != true || payload["configRevision"] != float64(3) {
			t.Fatalf("resume payload = %+v", payload)
		}
		found = true
	}
	if !found {
		t.Fatal("no resume.attempt fact was committed")
	}

	// A second scan must not add a second attempt: the row is pending and the
	// worker owns it now.
	if _, err := sup.Scan(context.Background()); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	count, err = st.AutoResumeCount(context.Background(), tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("auto resume count after second scan = %d, want 1", count)
	}
}

func TestPGManualPolicyParksAndExplicitResumeContinues(t *testing.T) {
	st := pgStoreForSupervisor(t)
	kern := pgKernelForSupervisor(t, st)
	tenant, session := pgSession(t, st)

	policy := DefaultPolicy()
	policy.Resume.Mode = ModeManual
	sup, err := New(Config{
		Store: NewSQLStore(st), Ledger: kern, Logger: testLogger(),
		Policy: func() (Policy, ConfigVersion) { return policy, ConfigVersion{} },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Parked != 1 {
		t.Fatalf("report = %+v", report)
	}
	run, err := st.GetRun(context.Background(), tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunFailed || !strings.Contains(run.LastError, "resume.mode=manual") {
		t.Fatalf("parked run = %+v", run)
	}
	// The parked row is not claimable: no worker may silently resume it.
	if _, err := kern.ClaimScope(context.Background(),
		kernel.Scope{TenantID: tenant, SessionID: session}, "worker", time.Minute); !errors.Is(err, kernel.ErrNoRunnable) {
		t.Fatalf("claim of a parked run = %v, want ErrNoRunnable", err)
	}
	// The explicit resume endpoint reuses RequeueRun; the same call continues it.
	outcome, err := st.RequeueRun(context.Background(), tenant, session)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if outcome.Status != "requeued" {
		t.Fatalf("requeue outcome = %+v", outcome)
	}
	if _, err := kern.ClaimScope(context.Background(),
		kernel.Scope{TenantID: tenant, SessionID: session}, "worker", time.Minute); err != nil {
		t.Fatalf("claim after explicit resume: %v", err)
	}
}

func TestPGEscalationIsDurable(t *testing.T) {
	st := pgStoreForSupervisor(t)
	kern := pgKernelForSupervisor(t, st)
	tenant, session := pgSession(t, st)
	pgFailedRun(t, st, kern, tenant, session)

	policy := DefaultPolicy()
	policy.Resume.MaxAttempts = 0 // exhaust immediately
	notifier := &fakeNotifier{}
	sup, err := New(Config{
		Store: NewSQLStore(st), Ledger: kern, Logger: testLogger(), Notifier: notifier,
		Policy: func() (Policy, ConfigVersion) { return policy, ConfigVersion{Revision: 4} },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Escalated != 1 || len(notifier.requests) != 1 {
		t.Fatalf("report = %+v, notifications = %d", report, len(notifier.requests))
	}
	count, err := st.EscalationsSince(context.Background(), tenant, session, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("escalation count = %d, want 1", count)
	}
	var payload map[string]any
	for _, event := range pgEvents(t, st, tenant, session) {
		if event.Kind != kernel.KindSupervisionEscalated {
			continue
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
	}
	if payload["level"] != "owner" || payload["result"] != "owner_notified" ||
		payload["attempts"] != float64(0) || payload["configRevision"] != float64(4) {
		t.Fatalf("escalation payload = %+v", payload)
	}
}
