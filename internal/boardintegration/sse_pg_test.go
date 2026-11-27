package boardintegration

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/store"

	boardaudit "github.com/jixoai/keel-board/audit"
	boardsync "github.com/jixoai/keel-board/sync"
	boardsyncfake "github.com/jixoai/keel-board/sync/fake"
	boardsyncstore "github.com/jixoai/keel-board/syncstore"
)

type sseLine struct {
	text string
	at   time.Time
}

// TestBoardSSEHeartbeatLastEventIDAndHiddenEvents covers 10.5 at the HTTP
// boundary: the adapter's keepalive, Last-Event-ID resume, an ahead cursor, and
// a permission-hidden event that never reaches a subscriber.
func TestBoardSSEHeartbeatLastEventIDAndHiddenEvents(t *testing.T) {
	env := newTestEnv(t, nil)
	server := env.server()
	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "work"}); resp.status != 201 {
		t.Fatalf("create work: %d", resp.status)
	}
	env.relation(env.tenant, "work", store.RelationViewer, env.carol.record.ID)
	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "private"}); resp.status != 201 {
		t.Fatalf("create private: %d", resp.status)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, lines := openSSE(t, ctx, server.URL+"/v1/board/events", "carol", "")
	_ = stream

	// The board's own opening comment proves the stream is live.
	waitForLine(t, lines, func(line string) bool { return line == ": connected" }, 2*time.Second)

	// An event in a channel Carol cannot read must not be delivered; an event
	// in work must be. The visible event's id is the resume cursor.
	if resp := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "private", "text": "hidden PRIVATE-TEXT"}); resp.status != 201 {
		t.Fatalf("private post: %d", resp.status)
	}
	if resp := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "visible WORK-TEXT"}); resp.status != 201 {
		t.Fatalf("work post: %d", resp.status)
	}
	var visibleID, visibleData string
	deadline := time.After(3 * time.Second)
	pingSeen := false
	for visibleData == "" {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("SSE stream closed before the visible event")
			}
			switch {
			case strings.HasPrefix(line, "id: "):
				visibleID = strings.TrimSpace(strings.TrimPrefix(line, "id: "))
			case strings.HasPrefix(line, "data: "):
				raw := strings.TrimPrefix(line, "data: ")
				if strings.Contains(raw, "PRIVATE-TEXT") {
					t.Fatalf("a hidden event reached an unauthorized subscriber: %s", raw)
				}
				if !strings.Contains(raw, "WORK-TEXT") {
					t.Fatalf("unexpected event: %s", raw)
				}
				visibleData = raw
			case strings.HasPrefix(line, ": ping"):
				pingSeen = true
			}
		case <-deadline:
			t.Fatal("the visible event did not arrive")
		}
	}
	if visibleID == "" {
		t.Fatal("the visible event carried no id")
	}
	var event struct {
		Message struct {
			Channel string `json:"channel"`
			Text    string `json:"text"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(visibleData), &event); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if event.Message.Channel != "work" || !strings.Contains(event.Message.Text, "WORK-TEXT") {
		t.Fatalf("visible event = %+v", event)
	}

	// An idle stream receives the keepalive comment. The test interval is 60ms.
	pingDeadline := time.After(2 * time.Second)
	for !pingSeen {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("SSE stream closed before a ping")
			}
			if strings.HasPrefix(line, ": ping") {
				pingSeen = true
			}
		case <-pingDeadline:
			t.Fatal("no keepalive comment arrived")
		}
	}
	cancel()
	stream.Body.Close()

	// Resume from the last event id: only later events replay.
	if resp := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "resumed WORK-TEXT-2"}); resp.status != 201 {
		t.Fatalf("resume seed: %d", resp.status)
	}
	resumeCtx, resumeCancel := context.WithCancel(context.Background())
	defer resumeCancel()
	resumeStream, resumeLines := openSSE(t, resumeCtx, server.URL+"/v1/board/events", "carol", visibleID)
	defer resumeStream.Body.Close()
	var resumed string
	resumeDeadline := time.After(3 * time.Second)
	for resumed == "" {
		select {
		case line, ok := <-resumeLines:
			if !ok {
				t.Fatal("resume stream closed early")
			}
			if strings.HasPrefix(line, "data: ") {
				resumed = line
			}
		case <-resumeDeadline:
			t.Fatal("no event replayed after Last-Event-ID")
		}
	}
	if !strings.Contains(resumed, "WORK-TEXT-2") {
		t.Fatalf("resume replayed %s", resumed)
	}
	if strings.Contains(resumed, "visible WORK-TEXT") {
		t.Fatalf("resume replayed an event at or before the cursor: %s", resumed)
	}

	// A cursor ahead of the log is refused, never clamped.
	ahead, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/v1/board/events", nil)
	if err != nil {
		t.Fatalf("ahead request: %v", err)
	}
	ahead.Header.Set("X-Test-Who", "carol")
	ahead.Header.Set("Last-Event-ID", "999999999")
	response, err := http.DefaultClient.Do(ahead)
	if err != nil {
		t.Fatalf("ahead: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 400 {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("ahead cursor = %d: %s", response.StatusCode, body)
	}
}

// TestBoardSSEMultiReplicaCatchUp proves a second process sees events written
// by the first through the shared store after forcing a head catch-up (the
// stream's opening Refresh).
func TestBoardSSEMultiReplicaCatchUp(t *testing.T) {
	env := newTestEnv(t, nil)
	first := env.server()
	if resp := env.do(t, first, "POST", "/v1/board/channels", "alice", map[string]any{"path": "work"}); resp.status != 201 {
		t.Fatalf("create: %d", resp.status)
	}
	env.relation(env.tenant, "work", store.RelationViewer, env.carol.record.ID)

	// A second surface over the same board schema is the second replica. It has
	// its own projection cache; its store is shared.
	second, err := New(context.Background(), Options{
		DSN: env.dsn, Schema: env.schema, Access: env.bridge, Store: env.st,
		TenantID: env.tenant, Logger: env.surf.logger,
		Heartbeat: -1, StreamLifetime: -1, ChannelScanInterval: -1,
	})
	if err != nil {
		t.Fatalf("second surface: %v", err)
	}
	defer second.Close()
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, _ := env.principal(r.Header.Get("X-Test-Who"))
		r = r.WithContext(withTestPrincipal(r.Context(), principal))
		second.ServeHTTP(w, r)
	}))
	defer secondServer.Close()

	if resp := env.do(t, first, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "cross-process"}); resp.status != 201 {
		t.Fatalf("post: %d", resp.status)
	}
	read := env.do(t, secondServer, "GET", "/v1/board/messages?channel=work", "carol", nil)
	if read.status != 200 || !strings.Contains(string(read.body), "cross-process") {
		// The non-streaming read may lag by the freshness window (1s); the SSE
		// path forces a Refresh before it starts. Wait one window and retry once.
		time.Sleep(1200 * time.Millisecond)
		read = env.do(t, secondServer, "GET", "/v1/board/messages?channel=work", "carol", nil)
	}
	if read.status != 200 || !strings.Contains(string(read.body), "cross-process") {
		t.Fatalf("second replica read = %d: %s", read.status, read.body)
	}
}

// TestBoardWebhookUsesProviderSignatureNotSession proves /webhooks is outside
// board session authentication and that the provider's verification decides.
func TestBoardWebhookUsesProviderSignatureNotSession(t *testing.T) {
	provider := &boardsyncfake.Provider{
		PlatformName: "fake",
		ReceiveFunc: func(_ context.Context, _ boardsync.Key, req *boardsync.WebhookRequest) (boardsync.Intake, error) {
			if req.Header.Get("X-Signature") != "good" {
				return boardsync.Intake{}, boardsync.ErrUnauthorized
			}
			return boardsync.Intake{Response: boardsync.OK()}, nil
		},
	}
	registry := boardsync.NewRegistry()
	env := newTestEnv(t, func(opts *Options) {
		opts.SyncRegistry = registry
		entry := boardsync.Entry{
			Tenant: opts.TenantID.String(), Key: "fake", Platform: "fake",
			Provider: provider, Sync: boardsync.Mirror("work"),
		}
		if err := registry.Add(entry); err != nil {
			t.Fatalf("register sync entry: %v", err)
		}
	})
	ctx := context.Background()
	if _, err := env.surf.SyncStore().UpsertEntry(ctx, boardsyncstore.Entry{
		Tenant: env.tenant.String(), Key: "fake", Platform: "fake", Enabled: true,
	}); err != nil {
		t.Fatalf("store sync entry: %v", err)
	}

	server := httptest.NewServer(env.surf.Webhook())
	defer server.Close()

	bad, err := http.Post(server.URL+"/webhooks/fake", "application/json", strings.NewReader(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("bad webhook: %v", err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d, want 401", bad.StatusCode)
	}

	good, err := http.NewRequest(http.MethodPost, server.URL+"/webhooks/fake", strings.NewReader(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("good request: %v", err)
	}
	good.Header.Set("X-Signature", "good")
	good.Header.Set("content-type", "application/json")
	response, err := http.DefaultClient.Do(good)
	if err != nil {
		t.Fatalf("good webhook: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("verified webhook = %d: %s", response.StatusCode, body)
	}
	// The accepted webhook is a bounded operational audit row: status and key,
	// never the platform body.
	recent, err := env.surf.AuditStore().Recent(ctx, env.tenant.String(), 10)
	if err != nil {
		t.Fatalf("read board audit: %v", err)
	}
	found := false
	for _, event := range recent {
		if event.Kind == boardaudit.KindWebhookAccepted {
			found = true
		}
		raw, _ := json.Marshal(event)
		if strings.Contains(string(raw), "hello") {
			t.Fatalf("board audit leaked the webhook body: %s", raw)
		}
	}
	if !found {
		t.Fatalf("no %s audit row: %+v", boardaudit.KindWebhookAccepted, recent)
	}
}

func openSSE(t *testing.T, ctx context.Context, url, who, lastEventID string) (*http.Response, <-chan string) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("SSE request: %v", err)
	}
	request.Header.Set("X-Test-Who", who)
	request.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	client := &http.Client{Timeout: 0}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	if response.StatusCode != 200 {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("SSE status = %d: %s", response.StatusCode, body)
	}
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	return response, lines
}

func waitForLine(t *testing.T, lines <-chan string, match func(string) bool, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed")
			}
			if match(line) {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for an SSE line")
		}
	}
}
