package main

// Production manual-compaction adapter tests (change keel-checkpoint). They use
// the real PG store/kernel and a real agent.Runner behind a static binding, so
// claim, lease renewal/settle, event+checkpoint persistence and requestId
// idempotency are exercised end to end. Skipped without KEEL_TEST_DSN.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/harnessapi"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

type staticBinding struct{ runner *agent.Runner }

func (b staticBinding) Acquire(context.Context) (agent.BoundRun, error) {
	return staticBound{runner: b.runner}, nil
}

type staticBound struct{ runner *agent.Runner }

func (b staticBound) Runner() *agent.Runner         { return b.runner }
func (b staticBound) Loadout() agent.LoadoutRef     { return agent.LoadoutRef{} }
func (b staticBound) Release(context.Context) error { return nil }

// shortSummaryStream answers every summary request with a compact summary.
func shortSummaryStream() provider.StreamFn {
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		msg := agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent("## Goal\ncontinue the work")},
			StopReason: agentcore.StopReasonEndTurn,
		}
		s := provider.NewAssistantMessageEventStream(2)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			_ = s.Emit(ctx, provider.StreamTextEvent{Partial: msg})
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}
}

func commitManualInput(t *testing.T, kern *kernel.Kernel, scope kernel.Scope, requestID, text string) int64 {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"text": text, "requestId": requestID})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := kern.Commit(context.Background(), kernel.CommitRequest{
		Scope: scope, Kind: kernel.KindUserMessage, RequestID: requestID, Payload: payload,
	})
	if err != nil {
		t.Fatalf("commit input %s: %v", requestID, err)
	}
	return receipt.Seq
}

func runManualExchange(t *testing.T, kern *kernel.Kernel, runner *agent.Runner, tenant, session uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	lease, err := kern.ClaimScope(ctx, kernel.Scope{TenantID: tenant, SessionID: session}, "manual-test-"+uuid.NewString(), time.Minute)
	if err != nil {
		t.Fatalf("claim exchange: %v", err)
	}
	cursor, runErr := runner.Run(ctx, lease)
	if runErr != nil {
		t.Fatalf("run exchange: %v", runErr)
	}
	if err := kern.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: cursor, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish exchange: %v", err)
	}
}

func TestManualCompactionAdapter(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	pg, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatalf("open kernel pg: %v", err)
	}
	kern := kernel.New(pg, kernel.Options{})
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "manual-compact-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	// Two answered exchanges followed by a trailing input: the manual cut can
	// summarize the earlier turns while the pending input stays in the tail.
	runner := agent.NewRunner(kern, kern, agent.FakeStream(), "fake", "fake", "", discardLogger())
	commitManualInput(t, kern, scope, "r1", strings.Repeat("context filler ALPHA. ", 300))
	runManualExchange(t, kern, runner, tenant, session)
	commitManualInput(t, kern, scope, "r2", "second question")
	runManualExchange(t, kern, runner, tenant, session)
	commitManualInput(t, kern, scope, "r3", "please compact now")

	policy := compaction.CheckpointPolicy{Enabled: true, RetainTokens: 10, ManualCompact: true}
	runner.Stream = shortSummaryStream()
	runner.Compaction = &agent.CompactionOptions{
		ContextWindow: 100000,
		Settings:      compaction.CompactionSettings{Enabled: true, KeepRecentTokens: 10},
		Policy:        &policy,
	}
	seam := newManualCompactor(staticBinding{runner: runner}, st, kern, discardLogger())

	first, err := seam(ctx, tenant, session, "manual-1")
	if err != nil {
		t.Fatalf("manual compaction: %v", err)
	}
	if !first.Performed || first.Replayed || first.CheckpointID <= 0 {
		t.Fatalf("first result = %+v", first)
	}
	if first.TokensBefore <= first.TokensAfter {
		t.Fatalf("tokens = %d -> %d, want a reduction", first.TokensBefore, first.TokensAfter)
	}
	if first.PolicyVersion == "" || first.KeptFromSeq <= 0 {
		t.Fatalf("first result = %+v", first)
	}
	checkpoint, found, err := kern.LoadCheckpoint(ctx, scope)
	if err != nil || !found || checkpoint.UptoSeq != first.CheckpointID || checkpoint.RequestID != "manual-1" {
		t.Fatalf("stored checkpoint = (%+v, found=%v, err=%v)", checkpoint, found, err)
	}
	// The adapter settles the run, so the lease is no longer live.
	state, err := kern.LeaseState(ctx, scope)
	if err != nil {
		t.Fatalf("lease state: %v", err)
	}
	if state.Live {
		t.Fatal("manual compaction left a live lease")
	}

	// The same requestId replays the projection without a new event.
	second, err := seam(ctx, tenant, session, "manual-1")
	if err != nil {
		t.Fatalf("replayed manual compaction: %v", err)
	}
	if !second.Replayed || second.Performed || second.CheckpointID != first.CheckpointID {
		t.Fatalf("replay = %+v", second)
	}
	events, err := st.LoadEvents(ctx, tenant, session, 0, 500)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	compactions := 0
	for _, event := range events {
		if event.Kind == store.EventCompaction {
			compactions++
		}
	}
	if compactions != 1 {
		t.Fatalf("compaction events = %d, want 1", compactions)
	}

	// A live owner makes the adapter answer busy rather than racing it.
	commitManualInput(t, kern, scope, "r4", "another input")
	other, err := kern.ClaimScope(ctx, scope, "manual-test-other", time.Minute)
	if err != nil {
		t.Fatalf("claim other owner: %v", err)
	}
	if _, err := seam(ctx, tenant, session, "manual-2"); !errors.Is(err, harnessapi.ErrCompactBusy) {
		t.Fatalf("busy seam err = %v, want ErrCompactBusy", err)
	}
	if err := kern.Release(ctx, other); err != nil {
		t.Fatalf("release other owner: %v", err)
	}
}

func TestManualCompactionAdapterMissingSession(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	pg, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(pg, kernel.Options{})
	runner := agent.NewRunner(kern, kern, shortSummaryStream(), "fake", "fake", "", discardLogger())
	seam := newManualCompactor(staticBinding{runner: runner}, st, kern, discardLogger())
	if _, err := seam(ctx, uuid.New(), uuid.New(), "manual-missing"); !errors.Is(err, harnessapi.ErrSessionNotFound) {
		t.Fatalf("missing session err = %v, want ErrSessionNotFound", err)
	}
}

// testAccess implements just the two Access methods the compact handler uses;
// the embedded interface supplies the rest (panicking if an unrelated method is
// reached, which would be a test surprise rather than a silent allow).
type testAccess struct {
	harnessapi.Access
	allow bool
}

func (testAccess) Principal(context.Context, uuid.UUID) (access.Principal, error) {
	return access.Principal{IsSuperAdmin: true}, nil
}

func (a testAccess) Authorize(_ context.Context, _ access.Principal, key string, _ access.Resource) (access.Decision, error) {
	if a.allow {
		return access.Decision{Allow: true, Key: key, Scope: access.ScopeTenant}, nil
	}
	return access.Decision{Allow: false, Key: key, Reason: access.ReasonMissingRelation}, nil
}

func (testAccess) Denied(context.Context, access.Principal, access.Decision, access.Resource) {}

// seedHTTPManualSession builds the same answered exchanges + pending input as
// TestManualCompactionAdapter and returns the live seams.
func seedHTTPManualSession(t *testing.T) (*store.Store, *kernel.Kernel, uuid.UUID, uuid.UUID, harnessapi.CompactSessionFunc) {
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
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	pg, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatalf("open kernel pg: %v", err)
	}
	kern := kernel.New(pg, kernel.Options{})
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "manual-http-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	runner := agent.NewRunner(kern, kern, agent.FakeStream(), "fake", "fake", "", discardLogger())
	commitManualInput(t, kern, scope, "h1", strings.Repeat("context filler ALPHA. ", 300))
	runManualExchange(t, kern, runner, tenant, session)
	commitManualInput(t, kern, scope, "h2", "second question")
	runManualExchange(t, kern, runner, tenant, session)
	commitManualInput(t, kern, scope, "h3", "please compact now")
	policy := compaction.CheckpointPolicy{Enabled: true, RetainTokens: 10, ManualCompact: true}
	runner.Stream = shortSummaryStream()
	runner.Compaction = &agent.CompactionOptions{
		ContextWindow: 100000,
		Settings:      compaction.CompactionSettings{Enabled: true, KeepRecentTokens: 10},
		Policy:        &policy,
	}
	return st, kern, tenant, session, newManualCompactor(staticBinding{runner: runner}, st, kern, discardLogger())
}

// TestManualCompactionHTTPIntegration pins the real handler + real adapter + a
// real permission decision: 201/200 with one checkpoint, and a denied
// session.write never reaches the seam.
func TestManualCompactionHTTPIntegration(t *testing.T) {
	st, _, tenant, session, seam := seedHTTPManualSession(t)
	server := httptest.NewServer(harnessapi.New(harnessapi.Config{
		CompactSession: seam,
		Access:         testAccess{allow: true},
		Logger:         discardLogger(),
	}))
	defer server.Close()

	post := func(requestID string) (int, map[string]any) {
		t.Helper()
		body := strings.NewReader(`{"requestId":"` + requestID + `"}`)
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions/"+session.String()+"/compact", body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Tenant-ID", tenant.String())
		req.Header.Set("Content-Type", "application/json")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var decoded map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return resp.StatusCode, decoded
	}

	code, first := post("http-1")
	if code != http.StatusCreated {
		t.Fatalf("first status = %d, body=%v, want 201", code, first)
	}
	if first["performed"] != true || first["checkpointId"] == nil {
		t.Fatalf("first body = %v", first)
	}
	code, second := post("http-1")
	if code != http.StatusOK || second["replayed"] != true {
		t.Fatalf("replay = (%d, %v), want 200 replayed", code, second)
	}
	if second["checkpointId"] != first["checkpointId"] {
		t.Fatalf("replay checkpoint = %v, want %v", second["checkpointId"], first["checkpointId"])
	}
	events, err := st.LoadEvents(context.Background(), tenant, session, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	compactions := 0
	for _, event := range events {
		if event.Kind == store.EventCompaction {
			compactions++
		}
	}
	if compactions != 1 {
		t.Fatalf("compaction events = %d, want 1", compactions)
	}

	// A denied session.write is answered 403 before the seam runs. The session
	// has no new requestId row, so the event count must stay unchanged.
	denied := httptest.NewServer(harnessapi.New(harnessapi.Config{
		CompactSession: seam,
		Access:         testAccess{allow: false},
		Logger:         discardLogger(),
	}))
	defer denied.Close()
	req, err := http.NewRequest(http.MethodPost, denied.URL+"/v1/sessions/"+session.String()+"/compact",
		strings.NewReader(`{"requestId":"http-denied"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant.String())
	req.Header.Set("Content-Type", "application/json")
	resp, err := denied.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied status = %d, want 403", resp.StatusCode)
	}
	events, err = st.LoadEvents(context.Background(), tenant, session, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	compactions = 0
	for _, event := range events {
		if event.Kind == store.EventCompaction {
			compactions++
		}
	}
	if compactions != 1 {
		t.Fatalf("denied request appended a compaction event (total %d)", compactions)
	}
}
