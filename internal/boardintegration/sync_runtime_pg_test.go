package boardintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/access"
	keelstore "github.com/jixoai/keel/internal/store"

	boardsync "github.com/jixoai/keel-board/sync"
	"github.com/jixoai/keel-board/sync/slack"
)

const (
	testSlackSigning = "slack-signing-runtime-test"
	testSlackToken   = "xoxb-slack-runtime-test"
)

// addSlackEntry writes one boot-visible Slack entry and its secrets.
func (e *testEnv) addSlackEntry(t *testing.T, enabled bool) {
	e.addSlackEntryWithAPI(t, enabled, "http://127.0.0.1:1/api")
}

func (e *testEnv) addSlackEntryWithAPI(t *testing.T, enabled bool, apiBase string) {
	t.Helper()
	// P0-1: the entry's route must name a provisioned channel at boot. Tests
	// seed the same event/owner the HTTP create would produce; the E2E proves
	// the real POST /v1/board/channels path.
	e.seedChannel("support", e.alice)
	e.grant(e.tenant, e.alice.record.ID, access.KeySecretWrite)
	ctx := context.Background()
	if _, err := e.st.PutTenantSecret(ctx, e.tenant, "SLACK_SIGNING", []byte(testSlackSigning), e.box); err != nil {
		t.Fatalf("put signing secret: %v", err)
	}
	if _, err := e.st.PutTenantSecret(ctx, e.tenant, "SLACK_BOT", []byte(testSlackToken), e.box); err != nil {
		t.Fatalf("put bot secret: %v", err)
	}
	record := keelstore.BoardSyncEntryRecord{
		TenantID: e.tenant, Key: "slack", Platform: "slack", Enabled: enabled,
		SecretRef:     map[string]string{"signingSecret": "SLACK_SIGNING", "botToken": "SLACK_BOT"},
		Settings:      mustRawJSON(map[string]any{"botUserId": "U_APP", "botId": "B_APP", "apiBaseUrl": apiBase}),
		Routes:        map[string]string{"C0SUPPORT": "support"},
		StartChannels: []string{"C0SUPPORT"},
	}
	if _, err := e.st.CreateBoardSyncEntry(ctx, record); err != nil {
		t.Fatalf("create sync entry: %v", err)
	}
}

// pushSlackInbox normalizes one signed Slack webhook and pushes its records
// directly into the durable inbox. This isolates the worker/recovery path from
// the HTTP webhook path (which has its own tests).
func mustRawJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func slackMessageBody(channel, user, text, ts string) []byte {
	body, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": "T_RUNTIME",
		"event": map[string]any{
			"type": "message", "channel": channel, "user": user, "text": text, "ts": ts,
		},
	})
	return body
}

func (e *testEnv) pushSlackInbox(t *testing.T, text, ts string) {
	t.Helper()
	provider, err := slack.New(slack.Config{
		Key: "slack", SigningSecret: boardsync.Secret(testSlackSigning),
		BotToken: boardsync.Secret(testSlackToken), BotUserID: "U_APP", BotID: "B_APP",
		APIBaseURL: "http://127.0.0.1:1/api",
	})
	if err != nil {
		t.Fatalf("slack provider: %v", err)
	}
	body := slackMessageBody("C0SUPPORT", "U_HUMAN", text, ts)
	signedAt := strconv.FormatInt(time.Now().Unix(), 10)
	request := &boardsync.WebhookRequest{
		Method: http.MethodPost, Path: "/webhooks/slack",
		Header: http.Header{
			"Content-Type":        []string{"application/json"},
			slack.TimestampHeader: []string{signedAt},
			slack.SignatureHeader: []string{slack.Sign([]byte(testSlackSigning), signedAt, body)},
		},
		Body: body,
	}
	intake, err := provider.Receive(context.Background(), "slack", request)
	if err != nil || len(intake.Records) != 1 {
		t.Fatalf("normalize slack webhook: %v/%d", err, len(intake.Records))
	}
	if _, err := e.surf.SyncStore().PushInbox(context.Background(), e.tenant.String(), "slack", intake.Records); err != nil {
		t.Fatalf("push inbox: %v", err)
	}
}

func (e *testEnv) postSignedSlack(t *testing.T, server *httptest.Server, body []byte) response {
	t.Helper()
	signedAt := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/webhooks/slack", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new webhook request: %v", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set(slack.TimestampHeader, signedAt)
	req.Header.Set(slack.SignatureHeader, slack.Sign([]byte(testSlackSigning), signedAt, body))
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("signed webhook: %v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, body: payload}
}

func (e *testEnv) setBoardFlags(t *testing.T, doc string) {
	t.Helper()
	if _, err := e.st.SetTenantConfig(context.Background(), e.tenant, json.RawMessage(doc)); err != nil {
		t.Fatalf("set board flags: %v", err)
	}
}

type syncMessageView struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
	Deleted bool   `json:"deleted"`
}

func (e *testEnv) syncMessages(t *testing.T, server *httptest.Server, channel string) []syncMessageView {
	t.Helper()
	resp := e.do(t, server, http.MethodGet, "/v1/board/messages?channel="+channel+"&limit=100", "alice", nil)
	if resp.status != 200 {
		t.Fatalf("list messages: %d %s", resp.status, resp.body)
	}
	var page struct {
		Messages []syncMessageView `json:"messages"`
	}
	if err := json.Unmarshal(resp.body, &page); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	return page.Messages
}

func (e *testEnv) waitSyncMessage(t *testing.T, server *httptest.Server, channel, contains string) (syncMessageView, error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, message := range e.syncMessages(t, server, channel) {
			if !message.Deleted && strings.Contains(message.Text, contains) {
				return message, nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return syncMessageView{}, fmt.Errorf("timeout waiting for board message %q", contains)
}

func (e *testEnv) assertNoBoardMessage(t *testing.T, server *httptest.Server, channel, contains string, wait time.Duration) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		for _, message := range e.syncMessages(t, server, channel) {
			if !message.Deleted && strings.Contains(message.Text, contains) {
				t.Fatalf("message %q appeared while sync should be paused: %+v", contains, message)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSyncDisableEnableKeepsWorkersAlive is the P0-1 regression: an enable
// PATCH must start workers on the process lifetime context, not the request
// context (which is canceled as soon as the response is written).
func TestSyncDisableEnableKeepsWorkersAlive(t *testing.T) {
	env := newTestEnvBefore(t, nil, func(e *testEnv) {
		e.addSlackEntry(t, true)
	})
	server := env.server()
	env.ensureChannel(t, server, "support")
	if resp := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": false}); resp.status != 200 {
		t.Fatalf("disable: %d %s", resp.status, resp.body)
	}
	resp := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": true})
	var result SyncEntryResult
	env.decode(t, resp, &result)
	if result.RestartRequired {
		t.Fatalf("enable of a boot-loaded entry asked for a restart: %+v", result)
	}
	env.pushSlackInbox(t, "imported after re-enable", "1700000500.000001")
	if _, err := env.waitSyncMessage(t, server, "support", "imported after re-enable"); err != nil {
		t.Fatalf("worker did not survive disable/enable: %v", err)
	}
}

// TestSyncTenantFlagsPauseAndResumeWorkers covers the flag monitor: a closed
// sync switch stops the durable lease loop; reopening it resumes without a
// process restart.
func TestSyncTenantFlagsPauseAndResumeWorkers(t *testing.T) {
	env := newTestEnvBefore(t, nil, func(e *testEnv) {
		e.addSlackEntry(t, true)
	})
	server := env.server()
	env.ensureChannel(t, server, "support")
	env.setBoardFlags(t, `{"board":{"enabled":true,"sync":{"enabled":false}}}`)
	time.Sleep(150 * time.Millisecond) // let the monitor pause the workers
	env.pushSlackInbox(t, "while sync is off", "1700000600.000001")
	// More than one worker poll cycle: this fails if both the monitor stop and
	// the durable lease gate are removed (the old 300ms window did not).
	env.assertNoBoardMessage(t, server, "support", "while sync is off", 500*time.Millisecond)

	env.setBoardFlags(t, `{"board":{"enabled":true,"sync":{"enabled":true}}}`)
	time.Sleep(150 * time.Millisecond)
	env.pushSlackInbox(t, "after sync is back", "1700000601.000001")
	if _, err := env.waitSyncMessage(t, server, "support", "after sync is back"); err != nil {
		t.Fatalf("worker did not resume: %v", err)
	}
}

// TestSyncCredentialRotationPausesWorkers covers P1-4: a same-name secret
// rewrite is observed by fingerprint comparison and the old provider stops
// consuming until a restart rebuilds it.
func TestSyncCredentialRotationPausesWorkers(t *testing.T) {
	env := newTestEnvBefore(t, nil, func(e *testEnv) {
		e.addSlackEntry(t, true)
	})
	server := env.server()
	env.ensureChannel(t, server, "support")
	if _, err := env.st.PutTenantSecret(context.Background(), env.tenant, "SLACK_SIGNING", []byte("rotated-signing-value"), env.box); err != nil {
		t.Fatalf("rotate secret: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	rotated := false
	for time.Now().Before(deadline) {
		entry, found, err := env.surf.SyncStore().Entry(context.Background(), env.tenant.String(), "slack")
		if err != nil {
			t.Fatalf("read sync entry: %v", err)
		}
		if found && !entry.Enabled {
			rotated = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !rotated {
		t.Fatal("entry was not disabled after credential rotation")
	}
	statusResp := env.do(t, server, http.MethodGet, "/v1/board/sync/credentials", "alice", nil)
	if statusResp.status != 200 || !strings.Contains(string(statusResp.body), `"restartRequired":true`) {
		t.Fatalf("rotated entry status = %d %s", statusResp.status, statusResp.body)
	}
	webhook := httptest.NewServer(env.surf.Webhook())
	defer webhook.Close()
	webhookBody := slackMessageBody("C0SUPPORT", "U_HUMAN", "webhook after rotation", "1700000701.000001")
	if resp := env.postSignedSlack(t, webhook, webhookBody); resp.status != http.StatusServiceUnavailable {
		t.Fatalf("webhook through a rotated entry = %d %s", resp.status, resp.body)
	}
	env.pushSlackInbox(t, "while credential is rotated", "1700000700.000001")
	env.assertNoBoardMessage(t, server, "support", "while credential is rotated", 400*time.Millisecond)
}

// TestSyncCredentialDeletionPausesWorkers covers the missing half of P1-4:
// deleting a referenced secret stops the old provider before the next lease.
func TestSyncCredentialDeletionPausesWorkers(t *testing.T) {
	env := newTestEnvBefore(t, nil, func(e *testEnv) {
		e.addSlackEntry(t, true)
	})
	server := env.server()
	env.ensureChannel(t, server, "support")
	if _, err := env.st.DeleteTenantSecret(context.Background(), env.tenant, "SLACK_SIGNING"); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		entry, found, err := env.surf.SyncStore().Entry(context.Background(), env.tenant.String(), "slack")
		if err != nil {
			t.Fatalf("read sync entry: %v", err)
		}
		if found && !entry.Enabled {
			stopped = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("entry was not disabled after credential deletion")
	}
	env.pushSlackInbox(t, "while credential is missing", "1700000710.000001")
	env.assertNoBoardMessage(t, server, "support", "while credential is missing", 300*time.Millisecond)
}

// TestSyncConcurrentRotationAndEnable is the P1-5 regression: a structural
// change racing an enable must never leave the old provider live. The manager
// serializes entry mutations; when the final document differs from the boot
// snapshot the board sync row must remain disabled.
func TestSyncConcurrentRotationAndEnable(t *testing.T) {
	env := newTestEnvBefore(t, nil, func(e *testEnv) {
		e.addSlackEntry(t, true)
	})
	server := env.server()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{
				"secretRef": map[string]string{"signingSecret": "SLACK_SIGNING_V2", "botToken": "SLACK_BOT"},
			})
		}(i)
		go func(i int) {
			defer wg.Done()
			env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": i%2 == 0})
		}(i)
	}
	wg.Wait()
	record, found, err := env.st.GetBoardSyncEntry(context.Background(), env.tenant, "slack")
	if err != nil || !found {
		t.Fatalf("final entry = %+v/%v/%v", record, found, err)
	}
	entry, found, err := env.surf.SyncStore().Entry(context.Background(), env.tenant.String(), "slack")
	if err != nil || !found {
		t.Fatalf("final sync entry = %+v/%v/%v", entry, found, err)
	}
	if record.SecretRef["signingSecret"] == "SLACK_SIGNING_V2" && entry.Enabled {
		t.Fatalf("rotated entry was left enabled: config=%+v board=%+v", record, entry)
	}
}

// TestSyncDisabledEntryBlocksAttachAndSource covers P1-3: the board's own
// attach/source routes still hold the boot registry entry, so keel must reject
// them before the old provider is reachable.
func TestSyncDisabledEntryBlocksAttachAndSource(t *testing.T) {
	env := newTestEnvBefore(t, nil, func(e *testEnv) {
		e.addSlackEntry(t, true)
	})
	server := env.server()
	env.ensureChannel(t, server, "support")
	posted := env.do(t, server, http.MethodPost, "/v1/board/messages", "alice", map[string]any{"channel": "support", "text": "attach target"})
	var body struct {
		Message syncMessageView `json:"message"`
	}
	env.decode(t, posted, &body)
	if resp := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": false}); resp.status != 200 {
		t.Fatalf("disable: %d %s", resp.status, resp.body)
	}
	attach := env.do(t, server, http.MethodPost, "/v1/board/sync/threads", "alice", map[string]any{
		"key": "slack", "thread": body.Message.ID, "target": "C0SUPPORT",
	})
	if attach.status != http.StatusNotFound {
		t.Fatalf("attach through a disabled entry = %d %s", attach.status, attach.body)
	}
	source := env.do(t, server, http.MethodGet, "/v1/board/sources/slack/read?target=C0SUPPORT", "alice", nil)
	if source.status != http.StatusNotFound {
		t.Fatalf("source read through a disabled entry = %d %s", source.status, source.body)
	}
	webhook := httptest.NewServer(env.surf.Webhook())
	defer webhook.Close()
	webhookBody := slackMessageBody("C0SUPPORT", "U_HUMAN", "webhook after disable", "1700001100.000001")
	if resp := env.postSignedSlack(t, webhook, webhookBody); resp.status != http.StatusNotFound {
		t.Fatalf("webhook through a disabled entry = %d %s", resp.status, resp.body)
	}
}

// TestSyncBootDisabledEnableRequiresRestart proves a connection disabled at
// boot is not silently revived by the old provider: enabling asks for a
// restart and does not consume inbox rows.
func TestSyncBootDisabledEnableRequiresRestart(t *testing.T) {
	env := newTestEnvBefore(t, nil, func(e *testEnv) {
		e.addSlackEntry(t, false)
	})
	server := env.server()
	env.ensureChannel(t, server, "support")
	resp := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": true})
	var result SyncEntryResult
	env.decode(t, resp, &result)
	if !result.RestartRequired {
		t.Fatalf("enable of a boot-disabled entry = %+v, want restartRequired", result)
	}
	env.pushSlackInbox(t, "must wait for restart", "1700000800.000001")
	env.assertNoBoardMessage(t, server, "support", "must wait for restart", 300*time.Millisecond)
}

// TestSyncDualInstanceDisableIsDurable covers P1-2: a disable made through one
// replica is observed by another replica's worker lease gate before it can
// import an already-pushed inbox row.
func TestSyncDualInstanceDisableIsDurable(t *testing.T) {
	env := newTestEnvBefore(t, nil, func(e *testEnv) {
		e.addSlackEntry(t, true)
	})
	server := env.server()
	env.ensureChannel(t, server, "support")
	second, err := New(context.Background(), env.options)
	if err != nil {
		t.Fatalf("second surface: %v", err)
	}
	t.Cleanup(second.Close)

	if resp := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": false}); resp.status != 200 {
		t.Fatalf("disable through replica one: %d %s", resp.status, resp.body)
	}
	// Push after the disable; both replicas must see the durable row.
	env.pushSlackInbox(t, "no replica may import", "1700000900.000001")
	time.Sleep(200 * time.Millisecond)
	env.assertNoBoardMessage(t, server, "support", "no replica may import", 400*time.Millisecond)
}

// TestSyncDisabledEntryBlocksRootStart proves the board-event subscription's
// root-start path is gated too: a new board root in a mapped channel must not
// call the platform after the entry is disabled.
func TestSyncDisabledEntryBlocksRootStart(t *testing.T) {
	var requests atomic.Int64
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"U_APP","response_metadata":{"scopes":["chat:write","chat:write.customize"]}}`))
		case "/chat.postMessage":
			_, _ = w.Write([]byte(`{"ok":true,"channel":"C0SUPPORT","ts":"1700000001.000100"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer fake.Close()
	env := newTestEnvBefore(t, nil, func(e *testEnv) {
		e.addSlackEntryWithAPI(t, true, fake.URL)
	})
	server := env.server()
	env.ensureChannel(t, server, "support")
	if resp := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": false}); resp.status != 200 {
		t.Fatalf("disable: %d %s", resp.status, resp.body)
	}
	if resp := env.do(t, server, http.MethodPost, "/v1/board/messages", "alice", map[string]any{"channel": "support", "text": "root after disable"}); resp.status != 201 {
		t.Fatalf("post root: %d %s", resp.status, resp.body)
	}
	time.Sleep(500 * time.Millisecond)
	if got := requests.Load(); got != 0 {
		t.Fatalf("disabled entry reached the platform %d time(s)", got)
	}
}

// TestSyncEntryRejectsUnlistedProviderHost covers the runtime endpoint trust
// boundary: a custom base URL must be loopback, the official endpoint, or an
// explicitly allowlisted HTTPS host.
func TestSyncEntryRejectsUnlistedProviderHost(t *testing.T) {
	env := newTestEnv(t, nil)
	env.grant(env.tenant, env.alice.record.ID, access.KeySecretWrite)
	server := env.server()
	unlisted := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", map[string]any{
		"key": "slack", "platform": "slack",
		"secretRef": map[string]string{"signingSecret": "SLACK_SIGNING"},
		"settings":  map[string]any{"botUserId": "U_APP", "botId": "B_APP", "apiBaseUrl": "https://evil.example/api"},
	})
	if unlisted.status != http.StatusBadRequest {
		t.Fatalf("unlisted provider host = %d %s", unlisted.status, unlisted.body)
	}
	official := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", map[string]any{
		"key": "slack", "platform": "slack",
		"secretRef": map[string]string{"signingSecret": "SLACK_SIGNING"},
		"settings":  map[string]any{"botUserId": "U_APP", "botId": "B_APP", "apiBaseUrl": "https://slack.com/api"},
	})
	if official.status != http.StatusCreated {
		t.Fatalf("official provider host = %d %s", official.status, official.body)
	}
}

// TestSyncDisabledEntryBlocksOutbound proves the outbound worker's durable
// lease gate: a reply queued after the entry is disabled is never posted to the
// platform by any replica.
func TestSyncDisabledEntryBlocksOutbound(t *testing.T) {
	var requests atomic.Int64
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"U_APP","response_metadata":{"scopes":["chat:write","chat:write.customize"]}}`))
		case "/chat.postMessage":
			_, _ = w.Write([]byte(`{"ok":true,"channel":"C0SUPPORT","ts":"1700000001.000100"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer fake.Close()
	env := newTestEnvBefore(t, nil, func(e *testEnv) {
		e.addSlackEntryWithAPI(t, true, fake.URL)
	})
	server := env.server()
	env.ensureChannel(t, server, "support")
	env.pushSlackInbox(t, "root before disable", "1700001000.000001")
	root, err := env.waitSyncMessage(t, server, "support", "root before disable")
	if err != nil {
		t.Fatal(err)
	}
	if resp := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": false}); resp.status != 200 {
		t.Fatalf("disable: %d %s", resp.status, resp.body)
	}
	if resp := env.do(t, server, http.MethodPost, "/v1/board/messages", "alice", map[string]any{"re": root.ID, "text": "reply must not post"}); resp.status != 201 {
		t.Fatalf("reply: %d %s", resp.status, resp.body)
	}
	time.Sleep(500 * time.Millisecond)
	if got := requests.Load(); got != 0 {
		t.Fatalf("disabled entry sent %d platform request(s)", got)
	}
}

// TestSyncLogsCarryNoSecretValue captures the logger while entries rotate and
// status is read; no credential value may appear.
func TestSyncLogsCarryNoSecretValue(t *testing.T) {
	var logs bytes.Buffer
	env := newTestEnv(t, func(options *Options) {
		options.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	env.grant(env.tenant, env.alice.record.ID, access.KeySecretWrite)
	server := env.server()
	const value = "log-secret-value-never-shown"
	if _, err := env.st.PutTenantSecret(context.Background(), env.tenant, "SLACK_SIGNING", []byte(value), env.box); err != nil {
		t.Fatalf("put secret: %v", err)
	}
	created := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", map[string]any{
		"key": "slack", "platform": "slack",
		"secretRef": map[string]string{"signingSecret": "SLACK_SIGNING"},
		"settings":  map[string]any{"botUserId": "U_APP", "botId": "B_APP"},
	})
	if created.status != 201 {
		t.Fatalf("create: %d %s", created.status, created.body)
	}
	env.do(t, server, http.MethodGet, "/v1/board/sync/credentials", "alice", nil)
	if strings.Contains(logs.String(), value) {
		t.Fatalf("logs leaked a secret: %s", logs.String())
	}
}

// TestSyncInboundWorkerGateBlocksLeases isolates the durable worker gate from
// the monitor: the board sync row is disabled directly while the process's
// worker keeps running, and the worker must not lease/import across several
// poll cycles.
func TestSyncInboundWorkerGateBlocksLeases(t *testing.T) {
	env := newTestEnvBefore(t, func(options *Options) {
		options.SyncMonitorInterval = time.Hour
	}, func(e *testEnv) {
		e.addSlackEntry(t, true)
	})
	server := env.server()
	env.ensureChannel(t, server, "support")
	if _, err := env.surf.SyncStore().SetEntryEnabled(context.Background(), env.tenant.String(), "slack", false); err != nil {
		t.Fatalf("disable board sync row: %v", err)
	}
	env.pushSlackInbox(t, "blocked by the worker lease gate", "1700003000.000001")
	env.assertNoBoardMessage(t, server, "support", "blocked by the worker lease gate", 500*time.Millisecond)
}

// TestSyncOutboundWorkerGateBlocksLeases is the outbound half: a reply queued
// while the durable row is disabled is never posted, even though the worker is
// still polling.
func TestSyncOutboundWorkerGateBlocksLeases(t *testing.T) {
	var requests atomic.Int64
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"U_APP","response_metadata":{"scopes":["chat:write","chat:write.customize"]}}`))
		case "/chat.postMessage":
			_, _ = w.Write([]byte(`{"ok":true,"channel":"C0SUPPORT","ts":"1700000001.000100"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer fake.Close()
	env := newTestEnvBefore(t, func(options *Options) {
		options.SyncMonitorInterval = time.Hour
	}, func(e *testEnv) {
		e.addSlackEntryWithAPI(t, true, fake.URL)
	})
	server := env.server()
	env.ensureChannel(t, server, "support")
	env.pushSlackInbox(t, "root before the gate closes", "1700003001.000001")
	root, err := env.waitSyncMessage(t, server, "support", "root before the gate closes")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.surf.SyncStore().SetEntryEnabled(context.Background(), env.tenant.String(), "slack", false); err != nil {
		t.Fatalf("disable board sync row: %v", err)
	}
	requests.Store(0)
	if resp := env.do(t, server, http.MethodPost, "/v1/board/messages", "alice", map[string]any{"re": root.ID, "text": "must not post"}); resp.status != 201 {
		t.Fatalf("reply: %d %s", resp.status, resp.body)
	}
	time.Sleep(500 * time.Millisecond)
	if got := requests.Load(); got != 0 {
		t.Fatalf("disabled outbound worker made %d platform requests", got)
	}
}
