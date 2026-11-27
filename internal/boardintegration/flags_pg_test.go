package boardintegration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	boardsync "github.com/jixoai/keel-board/sync"
)

// TestBoardFlagMatrix covers task 10.6's switch matrix at the HTTP boundary:
// board off is 404 for every board path and webhook, board on + sync off keeps
// the core board but refuses the sync surface, and both on reaches the board's
// own routes. It also proves a disabled tenant cannot see another tenant's
// board.
func TestBoardFlagMatrix(t *testing.T) {
	env := newTestEnv(t, nil)
	server := env.server()
	ctx := context.Background()

	setFlags := func(tenant uuid.UUID, doc string) {
		t.Helper()
		if _, err := env.st.SetTenantConfig(ctx, tenant, json.RawMessage(doc)); err != nil {
			t.Fatalf("set flags: %v", err)
		}
	}
	// Board off (the default).
	setFlags(env.tenant, `{}`)
	if resp := env.do(t, server, http.MethodGet, "/v1/board/channels", "alice", nil); resp.status != 404 || !containsCode(resp.body, "board_disabled") {
		t.Fatalf("board off channels = %d %s", resp.status, resp.body)
	}
	webhook := newWebhookServer(env)
	before, _ := env.surf.SyncStore().InboxDepth(ctx, env.tenant.String(), boardsync.Key("slack"))
	if resp := env.do(t, webhook, http.MethodPost, "/webhooks/slack", "", map[string]any{"type": "event_callback"}); resp.status != 404 || !containsCode(resp.body, "board_disabled") {
		t.Fatalf("board off webhook = %d %s", resp.status, resp.body)
	}
	after, _ := env.surf.SyncStore().InboxDepth(ctx, env.tenant.String(), boardsync.Key("slack"))
	if before != after {
		t.Fatalf("disabled board webhook wrote inbox: %+v -> %+v", before, after)
	}

	// Board on, sync still off.
	setFlags(env.tenant, `{"board":{"enabled":true}}`)
	if resp := env.do(t, server, http.MethodPost, "/v1/board/channels", "alice", map[string]any{"path": "work"}); resp.status != 201 {
		t.Fatalf("board on create = %d %s", resp.status, resp.body)
	}
	if resp := env.do(t, server, http.MethodGet, "/v1/board/sync/entries", "alice", nil); resp.status != 404 || !containsCode(resp.body, "sync_disabled") {
		t.Fatalf("sync off entries = %d %s", resp.status, resp.body)
	}
	if resp := env.do(t, server, http.MethodGet, "/v1/board/sources/slack/read?target=C1", "alice", nil); resp.status != 404 || !containsCode(resp.body, "sync_disabled") {
		t.Fatalf("sync off source read = %d %s", resp.status, resp.body)
	}
	if resp := env.do(t, webhook, http.MethodPost, "/webhooks/slack", "", nil); resp.status != 404 || !containsCode(resp.body, "sync_disabled") {
		t.Fatalf("sync off webhook = %d %s", resp.status, resp.body)
	}

	// Both on: the board's own management route answers (an empty entry list).
	setFlags(env.tenant, `{"board":{"enabled":true,"sync":{"enabled":true}}}`)
	resp := env.do(t, server, http.MethodGet, "/v1/board/sync/entries", "alice", nil)
	var entries struct {
		Entries []map[string]any `json:"entries"`
	}
	env.decode(t, resp, &entries)
	if len(entries.Entries) != 0 {
		t.Fatalf("empty tenant entries = %+v", entries.Entries)
	}

	// The other tenant's board is independent and still off. Its admin (Dave)
	// cannot reach the board or the deployment tenant's sync surface.
	setFlags(env.other, `{}`)
	if resp := env.do(t, server, http.MethodGet, "/v1/board/channels", "dave", nil); resp.status != 404 || !containsCode(resp.body, "board_disabled") {
		t.Fatalf("other tenant board = %d %s", resp.status, resp.body)
	}
	setFlags(env.other, `{"board":{"enabled":true,"sync":{"enabled":true}}}`)
	// Dave has board.manage in his own tenant, but sync entries are deployment
	// tenant scoped: the keel management route answers 404, never his tenant's
	// rows.
	created := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "dave", map[string]any{
		"key": "slack", "platform": "slack", "secretRef": map[string]string{"signingSecret": "NOPE"},
	})
	if created.status != 404 {
		t.Fatalf("cross-tenant entry create = %d %s", created.status, created.body)
	}
}

func newWebhookServer(env *testEnv) *httptest.Server {
	server := httptest.NewServer(env.surf.Webhook())
	env.t.Cleanup(server.Close)
	return server
}

func containsCode(body []byte, code string) bool {
	return json.Valid(body) && strings.Contains(string(body), `"code":"`+code+`"`)
}

// TestSurfaceGateStopsWrappedHandler covers the P1-1 gate composition used by
// keel serve: the superagent bridge is wrapped with Surface.Gate, so a disabled
// tenant answers 404 board_disabled before the bridge sees the request and the
// background writer (BoardEnabled) stops too.
func TestSurfaceGateStopsWrappedHandler(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setBoardFlags(t, `{"board":{"enabled":false}}`)
	ctx := context.Background()

	calls := 0
	wrapped := env.surf.Gate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTeapot)
	}))
	principal, ok := env.principal("alice")
	if !ok {
		t.Fatal("no principal")
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/board/tasks/1/claim", strings.NewReader(`{}`))
	request = request.WithContext(withTestPrincipal(request.Context(), principal))
	recorder := httptest.NewRecorder()
	wrapped.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "board_disabled") {
		t.Fatalf("disabled gate = %d %s", recorder.Code, recorder.Body.String())
	}
	if calls != 0 {
		t.Fatal("a disabled tenant reached the wrapped bridge")
	}
	if env.surf.BoardEnabled(ctx) {
		t.Fatal("BoardEnabled is true while board.enabled=false")
	}

	env.setBoardFlags(t, `{"board":{"enabled":true,"sync":{"enabled":true}}}`)
	recorder = httptest.NewRecorder()
	wrapped.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTeapot || calls != 1 {
		t.Fatalf("enabled gate = %d calls=%d", recorder.Code, calls)
	}
	if !env.surf.BoardEnabled(ctx) {
		t.Fatal("BoardEnabled is false while board.enabled=true")
	}
}
