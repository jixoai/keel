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

// TestPlacementHTTPGroupFullQueues202 is the spec's explainable admission: a
// full group answers 202 with the durable FIFO position and an ETA derived from
// observed attempts, commits the input (the message is durable), writes a
// queued run row, and starts the run on promotion when a slot frees.
func TestPlacementHTTPGroupFullQueues202(t *testing.T) {
	f := newPlacementFixture(t, placementFixtureOptions{})
	group := f.group(t, "limited", 1)
	running, waiting := f.session(t), f.session(t)
	f.join(t, running, group.GroupID)
	f.join(t, waiting, group.GroupID)

	if res := f.input(t, running, "slot-1", "first"); res.code != http.StatusCreated {
		t.Fatalf("first input = %d %v", res.code, res.body)
	}
	// The worker claimed the run; one observed attempt gives the queue its ETA.
	if _, err := f.st.Pool().Exec(context.Background(),
		`UPDATE runs SET status = 'running', lease_until = now() + interval '2 seconds'
		  WHERE tenant_id = $1 AND session_id = $2`, f.tenant, running); err != nil {
		t.Fatal(err)
	}
	if err := f.runtime.Service.ObserveRunDuration(context.Background(), f.tenant, running, 2*time.Second); err != nil {
		t.Fatalf("observe duration: %v", err)
	}

	res := f.input(t, waiting, "slot-2", "second")
	if res.code != http.StatusAccepted {
		t.Fatalf("full-group input = %d %v, want 202", res.code, res.body)
	}
	queued, ok := res.body["queued"].(map[string]any)
	if !ok {
		t.Fatalf("response has no queued block: %v", res.body)
	}
	if position, _ := queued["position"].(float64); position != 1 {
		t.Fatalf("queue position = %v, want 1", queued["position"])
	}
	if eta, _ := queued["etaSeconds"].(float64); eta != 2 {
		t.Fatalf("etaSeconds = %v, want the observed 2s", queued["etaSeconds"])
	}
	if res.header.Get("Retry-After") != "2" {
		t.Fatalf("Retry-After = %q, want 2", res.header.Get("Retry-After"))
	}
	status, groupID, seq := f.runRow(t, waiting)
	if status != "queued" || groupID == nil || *groupID != group.GroupID {
		t.Fatalf("queued run row = %q group %v, want queued in %s", status, groupID, group.GroupID)
	}
	if seq != 1 {
		t.Fatalf("queued input seq = %d, want the message committed (1)", seq)
	}
	if got := f.activeGroupRuns(t, group.GroupID); got != 1 {
		t.Fatalf("active group runs = %d, want 1 (the queued row holds no slot)", got)
	}

	// The running attempt settles: the settlement hook promotes the FIFO head.
	if _, err := f.st.Pool().Exec(context.Background(),
		`UPDATE runs SET status = 'done', lease_until = NULL WHERE tenant_id = $1 AND session_id = $2`,
		f.tenant, running); err != nil {
		t.Fatal(err)
	}
	f.runtime.Gate.RunSettled(context.Background(),
		kernel.Scope{TenantID: f.tenant, SessionID: running}, "done", 2*time.Second)
	status, _, _ = f.runRow(t, waiting)
	if status != "pending" {
		t.Fatalf("run after promotion = %q, want pending", status)
	}
	if got := f.activeGroupRuns(t, group.GroupID); got != 1 {
		t.Fatalf("active group runs after promotion = %d, want 1", got)
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
// exactly the group limit holds slots, the rest queue (202), and every input is
// durable (a queued run is stored work, not a dropped message).
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
	var admitted, queued, other int
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
			case http.StatusAccepted:
				queued++
			default:
				other++
				t.Errorf("race input %d = %d %v", i, res.code, res.body)
			}
		}()
	}
	wg.Wait()
	if admitted != limit || queued != attempts-limit || other != 0 {
		t.Fatalf("admitted=%d queued=%d other=%d, want %d/%d/0",
			admitted, queued, other, limit, attempts-limit)
	}
	if got := f.activeGroupRuns(t, group.GroupID); got != limit {
		t.Fatalf("active runs in group = %d, want %d", got, limit)
	}
	var committed, stored, waiting int
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE tenant_id = $1 AND kind = 'user.message'`,
		f.tenant).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM runs WHERE tenant_id = $1 AND group_id = $2 AND status = 'queued'`,
		f.tenant, group.GroupID).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM runs WHERE tenant_id = $1 AND group_id = $2`,
		f.tenant, group.GroupID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if committed != attempts || stored != attempts {
		t.Fatalf("committed messages=%d run rows=%d, want %d/%d (no input lost, no slot oversold)",
			committed, stored, attempts, attempts)
	}
	if waiting != attempts-limit {
		t.Fatalf("queued rows = %d, want %d", waiting, attempts-limit)
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
// pending row is claimable, an empty attempt settles it, and the queued next
// input is promoted. The input that arrived while the stale row held the slot
// is durable in the queue, not refused.
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
	res := f.input(t, next, "crash-next", "blocked while the stale run holds the slot")
	if res.code != http.StatusAccepted {
		t.Fatalf("input while stale pending run holds the slot = %d %v, want 202 queued", res.code, res.body)
	}
	if status, _, seq := f.runRow(t, next); status != "queued" || seq != 1 {
		t.Fatalf("next run = %q seq %d, want queued with the input committed", status, seq)
	}

	// Restart: a worker reclaims the pending row, sees no owed input, and
	// settles it. The empty attempt is not a leaked slot, and the settlement
	// hook promotes the queued input.
	ctx := context.Background()
	lease, err := f.kern.ClaimScope(ctx, kernel.Scope{TenantID: f.tenant, SessionID: stale}, "worker-restarted", 30*time.Second)
	if err != nil {
		t.Fatalf("restart claim: %v", err)
	}
	if err := f.kern.Finish(ctx, kernel.FinishRequest{Lease: lease, MaxAttempts: 3}); err != nil {
		t.Fatalf("restart settle: %v", err)
	}
	f.runtime.Gate.RunSettled(ctx, kernel.Scope{TenantID: f.tenant, SessionID: stale}, "done", 0)
	status, _, _ = f.runRow(t, stale)
	if status != "done" {
		t.Fatalf("stale run after restart = %q, want done", status)
	}
	if status, _, _ := f.runRow(t, next); status != "pending" {
		t.Fatalf("queued run after restart = %q, want pending (promoted)", status)
	}
	if got := f.activeGroupRuns(t, group.GroupID); got != 1 {
		t.Fatalf("active runs after convergence = %d, want the promoted run (1)", got)
	}
	if res := f.input(t, next, "crash-next-2", "after restart"); res.code != http.StatusCreated {
		t.Fatalf("input after restart = %d %v, want 201 (the session is already admitted)", res.code, res.body)
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
// has no user.message, so it admits through the same gate explicitly. A full
// group answers 202 with the queue position (the run starts on promotion).
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
	if res.code != http.StatusAccepted {
		t.Fatalf("trigger second = %d %v, want 202 queued", res.code, res.body)
	}
	queued, ok := res.body["queued"].(map[string]any)
	if !ok {
		t.Fatalf("trigger response has no queued block: %v", res.body)
	}
	if position, _ := queued["position"].(float64); position != 1 {
		t.Fatalf("trigger queue position = %v, want 1", queued["position"])
	}
	if status, _, _ := f.runRow(t, second); status != "queued" {
		t.Fatalf("triggered run status = %q, want queued", status)
	}
}

// TestPlacementHTTPMemoryEstimateUnavailable501 is the honest configuration
// failure: a memory-bounded group with no estimator answers 501 with the typed
// reason instead of packing with an invented number.
func TestPlacementHTTPMemoryEstimateUnavailable501(t *testing.T) {
	f := newPlacementFixture(t, placementFixtureOptions{})
	group, err := f.runtime.Service.CreateGroup(context.Background(), placement.GroupSpec{
		TenantID: f.tenant, Name: "memory-bounded", MaxConcurrentRuns: 2, MemoryLimitBytes: 64 << 20,
	})
	if err != nil {
		t.Fatalf("create memory group: %v", err)
	}
	session := f.session(t)
	f.join(t, session, group.GroupID)

	res := f.input(t, session, "memory-1", "hello")
	if res.code != http.StatusNotImplemented {
		t.Fatalf("memory-bounded input = %d %v, want 501", res.code, res.body)
	}
	if reason, _ := res.body["reason"].(string); reason != string(placement.ReasonMemoryEstimateUnavailable) {
		t.Fatalf("reason = %v, want %s", res.body["reason"], placement.ReasonMemoryEstimateUnavailable)
	}
	if f.runRows(t, session) != 0 {
		t.Fatal("a 501 admission wrote a run row")
	}
}
