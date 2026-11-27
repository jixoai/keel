package harnessapi

// Real-PostgreSQL coverage for chat commands (change keel-plugin-commands):
// tenant boundaries, durable requestId rows, concurrency and the audit event's
// content. Skipped without KEEL_TEST_DSN.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

type commandPGHarness struct {
	st      *store.Store
	server  *httptest.Server
	tenant  uuid.UUID
	session uuid.UUID
	compact *fakeCompact
	broker  *harnessext.DecisionBroker
	plugins *fakeCommandPlugins
	tools   *fakeCommandTools
}

func newCommandPGHarness(t *testing.T) *commandPGHarness {
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
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "cmd-http-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM command_invocations WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM events WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM sessions WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE tenant_id = $1`, tenant)
	})
	compact := &fakeCompact{result: CompactResult{CheckpointID: 11, Performed: true, TokensBefore: 50, TokensAfter: 5}}
	broker := harnessext.NewDecisionBroker(harnessext.DecisionBrokerOptions{Logger: testLogger()})
	t.Cleanup(broker.Close)
	plugins := &fakeCommandPlugins{}
	tools := &fakeCommandTools{result: json.RawMessage(`{"ok":true}`)}
	server := httptest.NewServer(New(Config{
		Access: commandAccess(),
		Logger: testLogger(),
		SessionExists: func(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error) {
			_, exists, err := st.SessionOwner(ctx, tenantID, sessionID)
			return exists, err
		},
		SessionIdle:    func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil },
		CompactSession: compact.compact,
		Decisions:      broker,
		CommandStore:   st,
		CommandPlugins: plugins,
		CommandTools:   tools,
	}))
	t.Cleanup(server.Close)
	return &commandPGHarness{st: st, server: server, tenant: tenant, session: session,
		compact: compact, broker: broker, plugins: plugins, tools: tools}
}

func (h *commandPGHarness) do(t *testing.T, tenant uuid.UUID, body any) (int, map[string]any) {
	t.Helper()
	var out map[string]any
	code := doJSON(t, h.server.Client(), http.MethodPost,
		h.server.URL+"/v1/sessions/"+h.session.String()+"/commands/compact", tenant, body, &out)
	return code, out
}

func (h *commandPGHarness) postAsync(tenant uuid.UUID, body any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequest(http.MethodPost,
		h.server.URL+"/v1/sessions/"+h.session.String()+"/commands/compact", strings.NewReader(string(raw)))
	if err != nil {
		return 0, err
	}
	request.Header.Set("X-Tenant-ID", tenant.String())
	request.Header.Set("Content-Type", "application/json")
	response, err := h.server.Client().Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func TestCommandPGTenantBoundaryAndAudit(t *testing.T) {
	harness := newCommandPGHarness(t)
	otherTenant := uuid.New()
	if err := harness.st.CreateTenant(context.Background(), otherTenant, "cmd-other-"+otherTenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = harness.st.Pool().Exec(context.Background(), `DELETE FROM command_invocations WHERE tenant_id = $1`, otherTenant)
		_, _ = harness.st.Pool().Exec(context.Background(), `DELETE FROM events WHERE tenant_id = $1`, otherTenant)
		_, _ = harness.st.Pool().Exec(context.Background(), `DELETE FROM sessions WHERE tenant_id = $1`, otherTenant)
		_, _ = harness.st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE tenant_id = $1`, otherTenant)
	})

	// A foreign session is the same 404 as a missing one, before any side effect.
	code, _ := harness.do(t, otherTenant, map[string]any{"requestId": "cross"})
	if code != http.StatusNotFound {
		t.Fatalf("cross-tenant command = %d, want 404", code)
	}
	if harness.compact.callCount() != 0 {
		t.Fatal("cross-tenant command reached the seam")
	}
	if _, found, err := harness.st.GetCommandInvocation(context.Background(), otherTenant, harness.session, "cross"); err != nil || found {
		t.Fatalf("cross-tenant row = found=%v err=%v", found, err)
	}

	code, body := harness.do(t, harness.tenant, map[string]any{
		"requestId": "req-audit",
		"args":      map[string]any{},
	})
	if code != http.StatusOK || body["status"] != store.CommandStatusOK {
		t.Fatalf("own-tenant command = %d %v", code, body)
	}
	row, found, err := harness.st.GetCommandInvocation(context.Background(), harness.tenant, harness.session, "req-audit")
	if err != nil || !found || row.Status != store.CommandStatusOK || row.EventSeq == 0 {
		t.Fatalf("durable row = %+v found=%v err=%v", row, found, err)
	}
	if _, found, _ := harness.st.GetCommandInvocation(context.Background(), otherTenant, harness.session, "req-audit"); found {
		t.Fatal("another tenant read the command row")
	}

	var payload []byte
	if err := harness.st.Pool().QueryRow(context.Background(),
		`SELECT payload FROM events WHERE tenant_id=$1 AND session_id=$2 AND kind=$3 ORDER BY seq DESC LIMIT 1`,
		harness.tenant, harness.session, store.EventCommandInvoked).Scan(&payload); err != nil {
		t.Fatalf("read command.invoked: %v", err)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event["command"] != "compact" || event["argsHash"] == nil || event["argsBytes"] == nil {
		t.Fatalf("command.invoked = %v", event)
	}
	if _, hasArgs := event["args"]; hasArgs {
		t.Fatalf("command.invoked carries argument bodies: %v", event)
	}

	// Replay is served from the row: the seam is not called again.
	code, replay := harness.do(t, harness.tenant, map[string]any{"requestId": "req-audit", "args": map[string]any{}})
	if code != http.StatusOK || replay["eventSeq"] != body["eventSeq"] {
		t.Fatalf("replay = %d %v", code, replay)
	}
	if harness.compact.callCount() != 1 {
		t.Fatalf("replay re-executed the seam: %d", harness.compact.callCount())
	}
}

func TestCommandPGConcurrentRequestID(t *testing.T) {
	harness := newCommandPGHarness(t)
	harness.compact.delay = 80 * time.Millisecond
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for index := range codes {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			code, err := harness.postAsync(harness.tenant, map[string]any{"requestId": "req-concurrent"})
			if err != nil {
				codes[slot] = -1
				return
			}
			codes[slot] = code
		}(index)
	}
	wg.Wait()
	for index, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("concurrent[%d] = %d, want 200", index, code)
		}
	}
	if harness.compact.callCount() != 1 {
		t.Fatalf("concurrent same requestId executed %d times", harness.compact.callCount())
	}
}

func TestCommandPGDestructiveRowPersistsDecision(t *testing.T) {
	harness := newCommandPGHarness(t)
	harness.plugins.registry.Commands = []harnessext.PluginCommand{
		pluginCommandFixture("vendor.one", "purge", pluginpkg.DangerDestructive, true, ""),
	}
	path := "/v1/sessions/" + harness.session.String() + "/commands/vendor.one:purge"
	var first map[string]any
	code := doJSON(t, harness.server.Client(), http.MethodPost, harness.server.URL+path,
		harness.tenant, map[string]any{"requestId": "req-pg-destructive", "args": map[string]any{}}, &first)
	if code != http.StatusAccepted || first["decisionId"] == nil {
		t.Fatalf("destructive open = %d %v", code, first)
	}
	row, found, err := harness.st.GetCommandInvocation(context.Background(), harness.tenant, harness.session, "req-pg-destructive")
	if err != nil || !found || row.Status != store.CommandStatusPendingDecision || row.DecisionID != first["decisionId"] {
		t.Fatalf("pending row = %+v found=%v err=%v", row, found, err)
	}
	if _, _, err := harness.broker.Answer(context.Background(), harnessext.AnswerDecisionRequest{
		TenantID: harness.tenant, SessionID: harness.session,
		DecisionID: uuid.MustParse(row.DecisionID), OptionID: commandDecisionOptionOK,
	}); err != nil {
		t.Fatal(err)
	}
	code = doJSON(t, harness.server.Client(), http.MethodPost, harness.server.URL+path,
		harness.tenant, map[string]any{"requestId": "req-pg-destructive", "args": map[string]any{}}, &first)
	if code != http.StatusOK {
		t.Fatalf("approved destructive execution = %d %v", code, first)
	}
	row, found, err = harness.st.GetCommandInvocation(context.Background(), harness.tenant, harness.session, "req-pg-destructive")
	if err != nil || !found || row.Status != store.CommandStatusOK || row.DecisionID == "" {
		t.Fatalf("terminal row = %+v found=%v err=%v", row, found, err)
	}
	if harness.tools.callCount() != 1 {
		t.Fatalf("destructive tool calls = %d", harness.tools.callCount())
	}
}
