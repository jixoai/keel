package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/placement"
	placementwiring "github.com/jixoai/keel/internal/placement/wiring"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
)

// placementFixture is the production wiring over a real database: the same
// placement gate, gated committer and HTTP server the runtime boots.
type placementFixture struct {
	st      *store.Store
	kern    *kernel.Kernel
	runtime *placementwiring.Runtime
	server  *httptest.Server
	client  *http.Client
	tenant  uuid.UUID
}

type placementFixtureOptions struct {
	tenantRunLimit   int
	defaultGroupRuns int
}

func newPlacementFixture(t *testing.T, opts placementFixtureOptions) *placementFixture {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	kernelLog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		st.Close()
		t.Fatalf("kernel migrate: %v", err)
	}
	kern := kernel.New(kernelLog, kernel.Options{})
	limits := quota.New(host.QuotaConfig{RunConcurrency: opts.tenantRunLimit})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt, err := placementwiring.New(placementwiring.Config{
		Store:                    st,
		Kernel:                   kern,
		Quota:                    limits,
		WorkerID:                 "http-e2e-" + uuid.NewString()[:8],
		Strategy:                 placement.Pack,
		DefaultMaxConcurrentRuns: opts.defaultGroupRuns,
		Logger:                   log,
	})
	if err != nil {
		st.Close()
		t.Fatalf("placement wiring: %v", err)
	}
	hub := NewHub(st, log)
	hubCtx, cancelHub := context.WithCancel(ctx)
	go hub.Run(hubCtx)
	srv := New(Config{
		Store:     st,
		Hub:       hub,
		Edge:      rt.Edge,
		Placement: rt.Gate,
		Events:    kern,
		Blobs:     blob.NewReader(st, 16, 1),
		Uploader:  &blob.Uploader{Store: st, CASDir: t.TempDir()},
		Scheduler: stream.NewScheduler(100),
		Admitter:  stream.NewAdmitter(1000, 2000),
		Metrics:   &stream.Metrics{},
		Logger:    log,
	})
	hs := httptest.NewServer(srv)
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "placement-http-"+tenant.String()[:8]); err != nil {
		hs.Close()
		st.Close()
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() {
		hs.Close()
		cancelHub()
		st.Close()
	})
	return &placementFixture{st: st, kern: kern, runtime: rt, server: hs, client: hs.Client(), tenant: tenant}
}

func (f *placementFixture) session(t *testing.T) uuid.UUID {
	t.Helper()
	session := uuid.New()
	if err := f.st.CreateSession(context.Background(), f.tenant, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session
}

func (f *placementFixture) group(t *testing.T, name string, maxRuns int) placement.Group {
	t.Helper()
	group, err := f.runtime.Service.CreateGroup(context.Background(), placement.GroupSpec{
		TenantID: f.tenant, Name: name, MaxConcurrentRuns: maxRuns,
	})
	if err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
	return group
}

func (f *placementFixture) join(t *testing.T, session, group uuid.UUID) {
	t.Helper()
	if _, err := f.runtime.Service.Join(context.Background(), placement.JoinRequest{
		TenantID: f.tenant, SessionID: session, GroupID: &group,
		WorkerID: "http-e2e-owner", TTL: time.Minute,
	}); err != nil {
		t.Fatalf("join session into group: %v", err)
	}
}

type placementHTTPResult struct {
	code   int
	header http.Header
	body   map[string]any
}

func (f *placementFixture) post(t *testing.T, path string, body any) placementHTTPResult {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, f.server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", f.tenant.String())
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return placementHTTPResult{code: resp.StatusCode, header: resp.Header, body: decoded}
}

func (f *placementFixture) input(t *testing.T, session uuid.UUID, requestID, text string) placementHTTPResult {
	t.Helper()
	return f.post(t, "/v1/sessions/"+session.String()+"/input",
		map[string]string{"text": text, "requestId": requestID})
}

func (f *placementFixture) runRow(t *testing.T, session uuid.UUID) (status string, group *uuid.UUID, eventSeq int64) {
	t.Helper()
	var groupText string
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT status, COALESCE(group_id::text, '') FROM runs WHERE tenant_id = $1 AND session_id = $2`,
		f.tenant, session).Scan(&status, &groupText); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	if groupText != "" {
		parsed, err := uuid.Parse(groupText)
		if err != nil {
			t.Fatalf("parse run group %q: %v", groupText, err)
		}
		group = &parsed
	}
	_ = f.st.Pool().QueryRow(context.Background(),
		`SELECT COALESCE(max(seq), 0) FROM events WHERE tenant_id = $1 AND session_id = $2 AND kind = 'user.message'`,
		f.tenant, session).Scan(&eventSeq)
	return status, group, eventSeq
}

func (f *placementFixture) runRows(t *testing.T, session uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM runs WHERE tenant_id = $1 AND session_id = $2`,
		f.tenant, session).Scan(&n); err != nil {
		t.Fatalf("count run rows: %v", err)
	}
	return n
}

func (f *placementFixture) activeGroupRuns(t *testing.T, group uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM runs WHERE tenant_id = $1 AND group_id = $2
		  AND status IN ('pending','running')`, f.tenant, group).Scan(&n); err != nil {
		t.Fatalf("count active group runs: %v", err)
	}
	return n
}

// TestPlacementHTTPAdmitOK is the happy path: a native input is admitted into
// the pack group, the run row carries the group, and the input commits.
func TestPlacementHTTPAdmitOK(t *testing.T) {
	f := newPlacementFixture(t, placementFixtureOptions{defaultGroupRuns: 2})
	session := f.session(t)

	res := f.input(t, session, "admit-ok", "hello")
	if res.code != http.StatusCreated {
		t.Fatalf("input = %d %v, want 201", res.code, res.body)
	}
	status, groupID, seq := f.runRow(t, session)
	if status != "pending" || groupID == nil || *groupID == uuid.Nil {
		t.Fatalf("run row = status %q group %v, want pending with a group", status, groupID)
	}
	if seq != 1 {
		t.Fatalf("user.message seq = %d, want 1", seq)
	}
	placed, err := f.runtime.Service.Store().GetPlacement(context.Background(), f.tenant, session)
	if err != nil {
		t.Fatalf("read placement: %v", err)
	}
	if placed.GroupID != *groupID {
		t.Fatalf("placement group = %s, run group = %s", placed.GroupID, *groupID)
	}
	group, err := f.runtime.Service.GetGroup(context.Background(), f.tenant, placed.GroupID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if group.Name != placement.PackDefaultGroupName {
		t.Fatalf("group = %q, want %q", group.Name, placement.PackDefaultGroupName)
	}
}

// TestPlacementHTTPGroupFull429 is the spec's explainable refusal: a full group
// answers 429 with Retry-After, commits no input, writes no run row, and admits
// the same session after the running slot is freed.
func TestPlacementHTTPGroupFull429(t *testing.T) {
	f := newPlacementFixture(t, placementFixtureOptions{})
	group := f.group(t, "limited", 1)
	running, waiting := f.session(t), f.session(t)
	f.join(t, running, group.GroupID)
	f.join(t, waiting, group.GroupID)

	if res := f.input(t, running, "slot-1", "first"); res.code != http.StatusCreated {
		t.Fatalf("first input = %d %v", res.code, res.body)
	}
	// The worker claimed the run: a live lease gives the refusal an ETA.
	if _, err := f.st.Pool().Exec(context.Background(),
		`UPDATE runs SET status = 'running', lease_until = now() + interval '2 seconds'
		  WHERE tenant_id = $1 AND session_id = $2`, f.tenant, running); err != nil {
		t.Fatal(err)
	}

	res := f.input(t, waiting, "slot-2", "second")
	if res.code != http.StatusTooManyRequests {
		t.Fatalf("full-group input = %d %v, want 429", res.code, res.body)
	}
	retryAfter := res.header.Get("Retry-After")
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil || seconds < 1 {
		t.Fatalf("Retry-After = %q, want a positive number of seconds", retryAfter)
	}
	if reason, _ := res.body["reason"].(string); reason != string(placement.CapacityGroup) {
		t.Fatalf("reason = %v, want %s", res.body["reason"], placement.CapacityGroup)
	}
	if limit, _ := res.body["limit"].(float64); limit != 1 {
		t.Fatalf("limit = %v, want 1", res.body["limit"])
	}
	var refusedSeq int64
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT COALESCE(max(seq), 0) FROM events WHERE tenant_id = $1 AND session_id = $2 AND kind = 'user.message'`,
		f.tenant, waiting).Scan(&refusedSeq); err != nil {
		t.Fatal(err)
	}
	if refusedSeq != 0 || f.runRows(t, waiting) != 0 {
		t.Fatalf("refused input wrote a run row / user.message seq %d; nothing may be enqueued", refusedSeq)
	}

	// Freeing the slot admits the retry.
	if _, err := f.st.Pool().Exec(context.Background(),
		`UPDATE runs SET status = 'done', lease_until = NULL WHERE tenant_id = $1 AND session_id = $2`,
		f.tenant, running); err != nil {
		t.Fatal(err)
	}
	if res := f.input(t, waiting, "slot-3", "second"); res.code != http.StatusCreated {
		t.Fatalf("retry after the slot freed = %d %v, want 201", res.code, res.body)
	}
}

// TestPlacementHTTPTenantQuotaAcrossGroups429 proves grouping cannot oversell
// the tenant quota: two groups, one tenant limit, the second run is refused
// with the tenant reason and frees after the first becomes terminal.
func TestPlacementHTTPTenantQuotaAcrossGroups429(t *testing.T) {
	f := newPlacementFixture(t, placementFixtureOptions{tenantRunLimit: 1})
	groupA := f.group(t, "quota-a", 0)
	groupB := f.group(t, "quota-b", 0)
	sessionA, sessionB := f.session(t), f.session(t)
	f.join(t, sessionA, groupA.GroupID)
	f.join(t, sessionB, groupB.GroupID)

	if res := f.input(t, sessionA, "quota-a-1", "a"); res.code != http.StatusCreated {
		t.Fatalf("group A input = %d %v", res.code, res.body)
	}
	res := f.input(t, sessionB, "quota-b-1", "b")
	if res.code != http.StatusTooManyRequests {
		t.Fatalf("cross-group input = %d %v, want 429", res.code, res.body)
	}
	if reason, _ := res.body["reason"].(string); reason != string(placement.CapacityTenant) {
		t.Fatalf("reason = %v, want %s", res.body["reason"], placement.CapacityTenant)
	}
	if res.header.Get("Retry-After") == "" {
		t.Fatal("tenant quota refusal missing Retry-After")
	}

	if _, err := f.st.Pool().Exec(context.Background(),
		`UPDATE runs SET status = 'failed' WHERE tenant_id = $1 AND session_id = $2`,
		f.tenant, sessionA); err != nil {
		t.Fatal(err)
	}
	if res := f.input(t, sessionB, "quota-b-2", "b"); res.code != http.StatusCreated {
		t.Fatalf("group B after release = %d %v, want 201", res.code, res.body)
	}
}

// TestPlacementHTTPConcurrentAdmissionsNoOversell fires concurrent HTTP inputs:
// exactly the group limit commits input and holds a slot.
func TestPlacementHTTPConcurrentAdmissionsNoOversell(t *testing.T) {
	f := newPlacementFixture(t, placementFixtureOptions{})
	const limit = 3
	const attempts = 12
	group := f.group(t, "race", limit)
	sessions := make([]uuid.UUID, attempts)
	for i := range sessions {
		sessions[i] = f.session(t)
		f.join(t, sessions[i], group.GroupID)
	}
	var admitted, refused int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, session := range sessions {
		i, session := i, session
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := f.input(t, session, fmt.Sprintf("race-%d", i), "race")
			mu.Lock()
			defer mu.Unlock()
			switch res.code {
			case http.StatusCreated:
				admitted++
			case http.StatusTooManyRequests:
				refused++
			default:
				t.Errorf("race input %d = %d %v", i, res.code, res.body)
			}
		}()
	}
	wg.Wait()
	if admitted != limit || refused != attempts-limit {
		t.Fatalf("admitted=%d refused=%d, want %d/%d", admitted, refused, limit, attempts-limit)
	}
	if got := f.activeGroupRuns(t, group.GroupID); got != limit {
		t.Fatalf("active runs in group = %d, want %d", got, limit)
	}
	var committed int
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE tenant_id = $1 AND kind = 'user.message'`,
		f.tenant).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != limit {
		t.Fatalf("committed user messages = %d, want %d (a refused run must not store input)", committed, limit)
	}
}

// TestPlacementHTTPCancelReleasesSlot settles a claimed run through the
// kernel's cancel outcome and proves the slot is free: the next session's input
// is admitted. The cancel path is the worker's Finish(cancelled), which the
// product cancel endpoint drives.
func TestPlacementHTTPCancelReleasesSlot(t *testing.T) {
	f := newPlacementFixture(t, placementFixtureOptions{})
	group := f.group(t, "cancel", 1)
	cancelled, next := f.session(t), f.session(t)
	f.join(t, cancelled, group.GroupID)
	f.join(t, next, group.GroupID)

	if res := f.input(t, cancelled, "cancel-1", "long task"); res.code != http.StatusCreated {
		t.Fatalf("input = %d %v", res.code, res.body)
	}
	ctx := context.Background()
	lease, err := f.kern.ClaimScope(ctx, kernel.Scope{TenantID: f.tenant, SessionID: cancelled}, "worker-cancel", 30*time.Second)
	if err != nil {
		t.Fatalf("claim run: %v", err)
	}
	if err := f.kern.Finish(ctx, kernel.FinishRequest{
		Lease: lease, Err: nil, MaxAttempts: 3, Cancelled: true,
	}); err != nil {
		t.Fatalf("cancel finish: %v", err)
	}
	status, _, _ := f.runRow(t, cancelled)
	if status != "failed" {
		t.Fatalf("cancelled run status = %q, want failed (the terminal policy)", status)
	}
	if res := f.input(t, next, "cancel-2", "after cancel"); res.code != http.StatusCreated {
		t.Fatalf("input after cancel = %d %v, want 201", res.code, res.body)
	}
}

// TestPlacementHTTPCrashPendingConverges simulates a crash between admission
// and the input commit (the pending row the gate writes), then restarts: the
// pending row is claimable, an empty attempt settles it, and the slot is free.
func TestPlacementHTTPCrashPendingConverges(t *testing.T) {
	f := newPlacementFixture(t, placementFixtureOptions{})
	group := f.group(t, "crash", 1)
	stale, next := f.session(t), f.session(t)
	f.join(t, stale, group.GroupID)
	f.join(t, next, group.GroupID)

	// The gate admitted the run, then the process died before the kernel
	// commit: a pending row exists with no user.message.
	admission, err := f.runtime.Gate.Admit(context.Background(), f.tenant, stale)
	if err != nil {
		t.Fatalf("admit before crash: %v", err)
	}
	status, groupID, seq := f.runRow(t, stale)
	if status != "pending" || groupID == nil || *groupID != admission.GroupID || seq != 0 {
		t.Fatalf("pre-crash run = %q group %v seq %d", status, groupID, seq)
	}
	if res := f.input(t, next, "crash-next", "blocked while the stale run holds the slot"); res.code != http.StatusTooManyRequests {
		t.Fatalf("input while stale pending run holds the slot = %d %v, want 429", res.code, res.body)
	}

	// Restart: a worker reclaims the pending row, sees no owed input, and
	// settles it. The empty attempt is not a leaked slot.
	ctx := context.Background()
	lease, err := f.kern.ClaimScope(ctx, kernel.Scope{TenantID: f.tenant, SessionID: stale}, "worker-restarted", 30*time.Second)
	if err != nil {
		t.Fatalf("restart claim: %v", err)
	}
	if err := f.kern.Finish(ctx, kernel.FinishRequest{Lease: lease, MaxAttempts: 3}); err != nil {
		t.Fatalf("restart settle: %v", err)
	}
	status, _, _ = f.runRow(t, stale)
	if status != "done" {
		t.Fatalf("stale run after restart = %q, want done", status)
	}
	if got := f.activeGroupRuns(t, group.GroupID); got != 0 {
		t.Fatalf("active runs after convergence = %d, want 0", got)
	}
	if res := f.input(t, next, "crash-next-2", "after restart"); res.code != http.StatusCreated {
		t.Fatalf("input after restart = %d %v, want 201", res.code, res.body)
	}
}

// TestPlacementHTTPDrainingGroup409 is the line-level check for
// writePlacementError's *Conflict branch: a draining group is a placement
// decision, not a capacity refusal, and the edge must answer 409 with the
// reason and group id while still storing nothing. The service-level tests
// already pin the typed error; this one drives it through the real HTTP
// handler, the production wiring and PostgreSQL.
func TestPlacementHTTPDrainingGroup409(t *testing.T) {
	f := newPlacementFixture(t, placementFixtureOptions{})
	group := f.group(t, "draining", 2)
	session := f.session(t)
	f.join(t, session, group.GroupID)
	if _, err := f.runtime.Service.SetGroupStatus(context.Background(), f.tenant, group.GroupID, placement.StatusDraining); err != nil {
		t.Fatalf("drain group: %v", err)
	}

	res := f.input(t, session, "draining-1", "hello")
	if res.code != http.StatusConflict {
		t.Fatalf("input into a draining group = %d %v, want 409", res.code, res.body)
	}
	if reason, _ := res.body["reason"].(string); reason != string(placement.ReasonGroupDraining) {
		t.Fatalf("reason = %v, want %s", res.body["reason"], placement.ReasonGroupDraining)
	}
	if got, _ := res.body["groupId"].(string); got != group.GroupID.String() {
		t.Fatalf("groupId = %q, want %s", got, group.GroupID)
	}
	if res.header.Get("Retry-After") != "" {
		t.Fatalf("conflict carried Retry-After %q; only a capacity refusal does", res.header.Get("Retry-After"))
	}
	var seq int64
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT COALESCE(max(seq), 0) FROM events WHERE tenant_id = $1 AND session_id = $2 AND kind = 'user.message'`,
		f.tenant, session).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 0 || f.runRows(t, session) != 0 {
		t.Fatalf("refused input wrote user.message seq %d / %d run rows; a conflict stores nothing", seq, f.runRows(t, session))
	}
}

// TestPlacementHTTPTriggerRunGated covers the second enqueue door: POST /run
// has no user.message, so it admits through the same gate explicitly.
func TestPlacementHTTPTriggerRunGated(t *testing.T) {
	f := newPlacementFixture(t, placementFixtureOptions{})
	group := f.group(t, "trigger", 1)
	first, second := f.session(t), f.session(t)
	f.join(t, first, group.GroupID)
	f.join(t, second, group.GroupID)

	if res := f.post(t, "/v1/sessions/"+first.String()+"/run", map[string]any{}); res.code != http.StatusAccepted {
		t.Fatalf("trigger first = %d %v, want 202", res.code, res.body)
	}
	res := f.post(t, "/v1/sessions/"+second.String()+"/run", map[string]any{})
	if res.code != http.StatusTooManyRequests {
		t.Fatalf("trigger second = %d %v, want 429", res.code, res.body)
	}
	if reason, _ := res.body["reason"].(string); reason != string(placement.CapacityGroup) {
		t.Fatalf("trigger refusal reason = %v, want %s", res.body["reason"], placement.CapacityGroup)
	}
}
