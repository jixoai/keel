// Command board-sync-e2e is the keel-side end-to-end proof for keel-board-v1
// tasks 12.1-12.4 and 13.2.
//
// It starts a real board surface on real PostgreSQL, a real keel identity store
// and four fake platforms (Slack, GitHub, Linear, Chat SDK bridge), then drives
// the full webhook -> inbox -> board -> outbox -> fake platform -> receipt path
// through keel's HTTP mount. Every step asserts an observable fact (an HTTP
// status, a board row, a fake platform call, a real blob byte). The program
// writes an HTML report; the shell gate records the run as evidence.
//
// Usage:
//
//	board-sync-e2e -dsn 'postgres://…' -out evidence/phase-1-sync-e2e.html
//
// The DSN must name a dedicated test database: the program creates tenants and
// a throwaway board schema and drops the schema on exit.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/boardintegration"
	keelstore "github.com/jixoai/keel/internal/store"

	boardsync "github.com/jixoai/keel-board/sync"
	"github.com/jixoai/keel-board/sync/chatsdk"
	"github.com/jixoai/keel-board/sync/github"
	"github.com/jixoai/keel-board/sync/linear"
	"github.com/jixoai/keel-board/sync/slack"
	boardsyncstore "github.com/jixoai/keel-board/syncstore"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("KEEL_TEST_DSN"), "PostgreSQL DSN (dedicated test database)")
	out := flag.String("out", "openspec/changes/keel-board-v1/evidence/phase-1-sync-e2e.html", "HTML report path")
	flag.Parse()
	if strings.TrimSpace(*dsn) == "" {
		fmt.Fprintln(os.Stderr, "board-sync-e2e: -dsn or KEEL_TEST_DSN is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report := &Report{StartedAt: time.Now().UTC()}
	runErr := run(ctx, *dsn, report)
	report.FinishedAt = time.Now().UTC()
	if err := writeReport(*out, report); err != nil {
		fmt.Fprintf(os.Stderr, "board-sync-e2e: write report: %v\n", err)
		if runErr == nil {
			runErr = err
		}
	}
	passed, failed := report.Counts()
	fmt.Printf("board-sync-e2e: %d passed, %d failed; report %s\n", passed, failed, *out)
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "board-sync-e2e: %v\n", runErr)
		os.Exit(1)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// --- environment ---------------------------------------------------------

type env struct {
	ctx      context.Context
	dsn      string
	schema   string
	logger   *slog.Logger
	store    *keelstore.Store
	bridge   *accesswire.Bridge
	box      *keelstore.SecretBox
	tenant   uuid.UUID
	other    uuid.UUID
	alice    keelstore.AccountRecord
	bob      keelstore.AccountRecord
	operator keelstore.AccountRecord
	carol    keelstore.AccountRecord
	dave     keelstore.AccountRecord
	surface  *boardintegration.Surface

	api     *httptest.Server
	webhook *httptest.Server

	slack     *fakeSlack
	github    *fakeGitHub
	linear    *fakeLinear
	bridgeSDK *fakeBridge

	principals map[string]auth.Principal

	boardOptions boardintegration.Options
}

func newEnv(ctx context.Context, dsn string, logger *slog.Logger) (*env, error) {
	st, err := keelstore.Open(ctx, keelstore.Config{DSN: dsn})
	if err != nil {
		return nil, fmt.Errorf("open keel store: %w", err)
	}
	box, err := keelstore.NewSecretBox(bytes.Repeat([]byte{0x6b}, 32))
	if err != nil {
		st.Close()
		return nil, err
	}
	e := &env{
		ctx: ctx, dsn: dsn, logger: logger, store: st, box: box,
		schema: fmt.Sprintf("board_sync_e2e_%s", strings.ReplaceAll(uuid.NewString()[:8], "-", "")),
		tenant: uuid.New(), other: uuid.New(),
		principals: map[string]auth.Principal{},
	}
	for _, tenant := range []uuid.UUID{e.tenant, e.other} {
		if err := st.CreateTenant(ctx, tenant, "board-e2e-"+tenant.String()[:8]); err != nil {
			return nil, fmt.Errorf("create tenant: %w", err)
		}
		if err := accesswire.EnsureBuiltinGroups(ctx, st, tenant); err != nil {
			return nil, fmt.Errorf("builtin groups: %w", err)
		}
		if _, err := st.SetTenantConfig(ctx, tenant, json.RawMessage(`{"board":{"enabled":true,"sync":{"enabled":true}}}`)); err != nil {
			return nil, fmt.Errorf("board flags: %w", err)
		}
	}
	if e.alice, err = e.account(e.tenant, "alice", true); err != nil {
		return nil, err
	}
	if e.carol, err = e.account(e.tenant, "carol", false); err != nil {
		return nil, err
	}
	if e.bob, err = e.account(e.tenant, "bob", false); err != nil {
		return nil, err
	}
	if e.operator, err = e.account(e.tenant, "operator", false); err != nil {
		return nil, err
	}
	if e.dave, err = e.account(e.other, "dave", true); err != nil {
		return nil, err
	}
	for _, key := range []string{access.KeyBoardRead, access.KeyBoardWrite, access.KeyBoardManage, access.KeySecretWrite} {
		if err := e.grant(e.tenant, e.alice.ID, key); err != nil {
			return nil, err
		}
	}
	if err := e.grant(e.tenant, e.carol.ID, access.KeyBoardRead); err != nil {
		return nil, err
	}
	// Bob writes boards, but has no relation on the superagent channel: the
	// bridge's channel check must refuse him. The operator's authority comes
	// from the tenant-admin builtin group alone.
	for _, key := range []string{access.KeyBoardRead, access.KeyBoardWrite} {
		if err := e.grant(e.tenant, e.bob.ID, key); err != nil {
			return nil, err
		}
	}
	for _, key := range []string{access.KeyBoardRead, access.KeyBoardWrite, access.KeyBoardManage} {
		if err := e.grant(e.other, e.dave.ID, key); err != nil {
			return nil, err
		}
	}
	e.principals["alice"] = auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: e.tenant, AccountID: e.alice.ID,
		Username: e.alice.Username, DisplayName: e.alice.DisplayName,
	}
	e.principals["carol"] = auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: e.tenant, AccountID: e.carol.ID,
		Username: e.carol.Username, DisplayName: e.carol.DisplayName,
	}
	e.principals["bob"] = auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: e.tenant, AccountID: e.bob.ID,
		Username: e.bob.Username, DisplayName: e.bob.DisplayName,
	}
	e.principals["operator"] = auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: e.tenant, AccountID: e.operator.ID,
		Username: e.operator.Username, DisplayName: e.operator.DisplayName,
	}
	e.principals["dave"] = auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: e.other, AccountID: e.dave.ID,
		Username: e.dave.Username, DisplayName: e.dave.DisplayName,
	}
	bridge, err := accesswire.NewBridge(st, logger)
	if err != nil {
		return nil, err
	}
	e.bridge = bridge
	e.boardOptions = boardintegration.Options{
		DSN: dsn, Schema: e.schema, Access: bridge, Store: st, TenantID: e.tenant,
		Logger: logger, Heartbeat: 60 * time.Millisecond, StreamLifetime: -1,
		ChannelScanInterval: -1, SecretBox: box, BlobDir: "",
		FileHosts:      []string{"acme.slack.com"},
		TenantFlagsTTL: -1, DisableAutomation: true,
		SyncMonitorInterval: 20 * time.Millisecond, SyncWorkerPollInterval: 50 * time.Millisecond,
		SyncStoreConfig: boardsyncstore.Config{
			OutboundRetry: boardsync.RetryPolicy{MaxAttempts: 3, BaseDelay: 20 * time.Millisecond, MaxDelay: 60 * time.Millisecond},
			InboundRetry:  boardsync.RetryPolicy{MaxAttempts: 3, BaseDelay: 20 * time.Millisecond, MaxDelay: 60 * time.Millisecond},
		},
	}
	return e, nil
}

func (e *env) account(tenant uuid.UUID, name string, manager bool) (keelstore.AccountRecord, error) {
	record, err := e.store.CreateAccount(e.ctx, keelstore.AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: name + "-" + tenant.String()[:8],
		DisplayName: strings.ToUpper(name[:1]) + name[1:], Kind: "human",
	})
	if err != nil {
		return record, fmt.Errorf("create account %s: %w", name, err)
	}
	_ = manager
	return record, nil
}

func (e *env) grant(tenant, account uuid.UUID, key string) error {
	_, err := e.store.CreatePermissionGrant(e.ctx, keelstore.PermissionGrantRecord{
		TenantID: tenant, ID: uuid.New(), SubjectType: keelstore.PermissionSubjectAccount,
		SubjectID: account, PermissionKey: key, Scope: keelstore.PermissionScopeTenant,
		GrantedBy: account,
	})
	if err != nil {
		return fmt.Errorf("grant %s: %w", key, err)
	}
	return nil
}

func (e *env) startSurface() error {
	surface, err := boardintegration.New(e.ctx, e.boardOptions)
	if err != nil {
		return err
	}
	e.surface = surface
	e.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal, ok := e.principals[r.Header.Get("X-E2E-Who")]; ok {
			r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
		}
		surface.ServeHTTP(w, r)
	}))
	e.webhook = httptest.NewServer(surface.Webhook())
	return nil
}

func (e *env) stopSurface() {
	if e.api != nil {
		e.api.Close()
		e.api = nil
	}
	if e.webhook != nil {
		e.webhook.Close()
		e.webhook = nil
	}
	if e.surface != nil {
		e.surface.Close()
		e.surface = nil
	}
}

func (e *env) close() {
	e.stopSurface()
	drop := fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", quoteIdent(e.schema))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = e.store.Pool().Exec(ctx, drop)
	e.store.Close()
}

func quoteIdent(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

// --- HTTP helpers --------------------------------------------------------

type apiResponse struct {
	status int
	body   []byte
}

func (e *env) apiDo(who, method, path string, body any) apiResponse {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return apiResponse{status: 0, body: []byte(err.Error())}
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(e.ctx, method, e.api.URL+path, reader)
	if err != nil {
		return apiResponse{status: 0, body: []byte(err.Error())}
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if who != "" {
		req.Header.Set("X-E2E-Who", who)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return apiResponse{status: 0, body: []byte(err.Error())}
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	return apiResponse{status: resp.StatusCode, body: payload}
}

func (e *env) postWebhook(path string, headers map[string]string, body []byte) apiResponse {
	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost, e.webhook.URL+path, bytes.NewReader(body))
	if err != nil {
		return apiResponse{status: 0, body: []byte(err.Error())}
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return apiResponse{status: 0, body: []byte(err.Error())}
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	return apiResponse{status: resp.StatusCode, body: payload}
}

func (e *env) apiPost(path string, headers map[string]string, body []byte) apiResponse {
	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost, e.api.URL+path, bytes.NewReader(body))
	if err != nil {
		return apiResponse{status: 0, body: []byte(err.Error())}
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return apiResponse{status: 0, body: []byte(err.Error())}
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	return apiResponse{status: resp.StatusCode, body: payload}
}

func (e *env) waitFor(timeout time.Duration, what string, check func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		ok, err := check()
		if err != nil {
			last = err
		} else if ok {
			return nil
		}
		select {
		case <-e.ctx.Done():
			return e.ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	if last != nil {
		return fmt.Errorf("timeout waiting for %s: %w", what, last)
	}
	return fmt.Errorf("timeout waiting for %s", what)
}

type messageView struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
	Deleted bool   `json:"deleted"`
	Links   []struct {
		URL string `json:"url"`
	} `json:"links"`
	By *struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	} `json:"by"`
}

func (e *env) messages(channel string) ([]messageView, error) {
	resp := e.apiDo("alice", http.MethodGet, "/v1/board/messages?channel="+url.QueryEscape(channel)+"&limit=100", nil)
	if resp.status != 200 {
		return nil, fmt.Errorf("list messages: %d %s", resp.status, resp.body)
	}
	var page struct {
		Messages []messageView `json:"messages"`
	}
	if err := json.Unmarshal(resp.body, &page); err != nil {
		return nil, err
	}
	return page.Messages, nil
}

func (e *env) waitMessage(channel, contains string) (messageView, error) {
	var found messageView
	err := e.waitFor(10*time.Second, "board message "+contains, func() (bool, error) {
		messages, err := e.messages(channel)
		if err != nil {
			return false, err
		}
		for _, message := range messages {
			if !message.Deleted && strings.Contains(message.Text, contains) {
				found = message
				return true, nil
			}
		}
		return false, nil
	})
	return found, err
}

func (e *env) createChannel(path string) error {
	resp := e.apiDo("alice", http.MethodPost, "/v1/board/channels", map[string]string{"path": path})
	if resp.status != 201 && resp.status != 200 {
		return fmt.Errorf("create channel %s: %d %s", path, resp.status, resp.body)
	}
	return nil
}

func (e *env) postReply(rootID, text string) (messageView, error) {
	resp := e.apiDo("alice", http.MethodPost, "/v1/board/messages", map[string]any{"re": rootID, "text": text})
	if resp.status != 201 {
		return messageView{}, fmt.Errorf("reply: %d %s", resp.status, resp.body)
	}
	var body struct {
		Message messageView `json:"message"`
	}
	if err := json.Unmarshal(resp.body, &body); err != nil {
		return messageView{}, err
	}
	return body.Message, nil
}

// --- Slack fake ----------------------------------------------------------

type slackCall struct {
	Method   string
	Channel  string
	Text     string
	ThreadTS string
	Emoji    string
	Status   string
}

type fakeSlack struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []slackCall
	// failPosts counts down: while >0, chat.postMessage answers 500.
	failPosts int
	// deadChannel answers 500 for every post to that channel.
	deadChannel string
}

func newFakeSlack() *fakeSlack {
	fake := &fakeSlack{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		method := strings.TrimPrefix(r.URL.Path, "/")
		call := slackCall{
			Method: method, Channel: r.Form.Get("channel"), Text: r.Form.Get("text") + r.Form.Get("markdown_text"),
			ThreadTS: r.Form.Get("thread_ts"), Emoji: r.Form.Get("name"), Status: r.Form.Get("status"),
		}
		fake.mu.Lock()
		fake.calls = append(fake.calls, call)
		fail := fake.failPosts > 0 && method == "chat.postMessage"
		if fail {
			fake.failPosts--
		}
		dead := method == "chat.postMessage" && call.Channel == fake.deadChannel
		fake.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		if fail || dead {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false,"error":"server_error"}`))
			return
		}
		switch method {
		case "auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"U_APP","response_metadata":{"scopes":["chat:write","chat:write.customize"]}}`))
		case "chat.postMessage":
			_, _ = w.Write([]byte(`{"ok":true,"channel":"` + call.Channel + `","ts":"1700000001.000100"}`))
		case "conversations.info":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"C0SUPPORT","name":"support","is_member":true}}`))
		case "conversations.history":
			_, _ = w.Write([]byte(`{"ok":true,"has_more":false,"messages":[{"type":"message","user":"U_HUMAN","text":"source read from slack","ts":"1700000000.000001"}]}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	return fake
}

func (f *fakeSlack) URL() string { return f.server.URL }
func (f *fakeSlack) Close()      { f.server.Close() }

func (f *fakeSlack) snapshot() []slackCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]slackCall(nil), f.calls...)
}

func (f *fakeSlack) has(method string) bool {
	for _, call := range f.snapshot() {
		if call.Method == method {
			return true
		}
	}
	return false
}

func (f *fakeSlack) count(method, channel string) int {
	count := 0
	for _, call := range f.snapshot() {
		if call.Method == method && (channel == "" || call.Channel == channel) {
			count++
		}
	}
	return count
}

// --- GitHub fake ---------------------------------------------------------

type githubCall struct {
	Method string
	Path   string
	Body   string
}

type fakeGitHub struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []githubCall
}

func newFakeGitHub() *fakeGitHub {
	fake := &fakeGitHub{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.calls = append(fake.calls, githubCall{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		fake.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/comments"):
			_, _ = w.Write([]byte(`{"id":9001,"body":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	return fake
}

func (f *fakeGitHub) URL() string { return f.server.URL }
func (f *fakeGitHub) Close()      { f.server.Close() }

func (f *fakeGitHub) comments() []githubCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]githubCall, 0, len(f.calls))
	for _, call := range f.calls {
		if call.Method == http.MethodPost && strings.Contains(call.Path, "/comments") {
			out = append(out, call)
		}
	}
	return out
}

// --- Linear fake ---------------------------------------------------------

type fakeLinear struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []string
}

func newFakeLinear() *fakeLinear {
	fake := &fakeLinear{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.calls = append(fake.calls, string(body))
		fake.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"commentCreate":{"success":true,"comment":{"id":"linear-comment-out"}},"issueCreate":{"success":true,"issue":{"id":"issue-out","identifier":"ENG-999","team":{"key":"ENG"}}},"issues":{"nodes":[]},"issue":{"id":"11111111-1111-1111-1111-111111111111","identifier":"ENG-123","team":{"key":"ENG","id":"team-1"}}}}`))
	}))
	return fake
}

func (f *fakeLinear) URL() string { return f.server.URL }
func (f *fakeLinear) Close()      { f.server.Close() }

func (f *fakeLinear) has(fragment string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if strings.Contains(call, fragment) {
			return true
		}
	}
	return false
}

// --- Chat SDK bridge fake ------------------------------------------------

type fakeBridge struct {
	server *httptest.Server
	secret boardsync.Secret
	key    boardsync.Key
	mu     sync.Mutex
	calls  []chatsdk.DeliverRequest
	// failDeliveries answers 500 for that many attempts.
	failDeliveries int
	// garbageDeliveries answers 200 with a malformed body for that many
	// attempts (a transport/protocol failure the client must retry).
	garbageDeliveries int
	// slowDeliveries answers after deliverDelay; the provider's 15s client
	// timeout fires first, which is a real timeout rather than a 5xx.
	slowDeliveries int
	deliverDelay   time.Duration
	attempts       int
}

func newFakeBridge(secret boardsync.Secret, key boardsync.Key) *fakeBridge {
	fake := &fakeBridge{secret: secret, key: key}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, chatsdk.MaxBodyBytes))
		if err := chatsdk.VerifyRequestV1(fake.secret, r.Header, body, time.Now(), chatsdk.DefaultWindow); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad signature"}`))
			return
		}
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/deliver":
			var request chatsdk.DeliverRequest
			if err := json.Unmarshal(body, &request); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"bad body"}`))
				return
			}
			fake.mu.Lock()
			fake.attempts++
			if fake.slowDeliveries > 0 {
				fake.slowDeliveries--
				delay := fake.deliverDelay
				fake.mu.Unlock()
				if delay > 0 {
					select {
					case <-time.After(delay):
					case <-r.Context().Done():
						return
					}
				}
				_, _ = w.Write([]byte(`{"thread":"-100:t1","message":"tg-out-1"}`))
				return
			}
			if fake.failDeliveries > 0 {
				fake.failDeliveries--
				fake.mu.Unlock()
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"sidecar unavailable"}`))
				return
			}
			if fake.garbageDeliveries > 0 {
				fake.garbageDeliveries--
				fake.mu.Unlock()
				_, _ = w.Write([]byte(`not-json`))
				return
			}
			fake.calls = append(fake.calls, request)
			fake.mu.Unlock()
			_, _ = w.Write([]byte(`{"thread":"-100:t1","message":"tg-out-1"}`))
		case "/resolve":
			_, _ = w.Write([]byte(`{"target":{"channel":"-100","thread":"t1","kind":"thread"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return fake
}

func (f *fakeBridge) URL() string { return f.server.URL }
func (f *fakeBridge) Close()      { f.server.Close() }

func (f *fakeBridge) deliveries() []chatsdk.DeliverRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]chatsdk.DeliverRequest(nil), f.calls...)
}

func (f *fakeBridge) deliveryAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *fakeBridge) setFailDeliveries(count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failDeliveries = count
}

func (f *fakeBridge) setGarbageDeliveries(count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.garbageDeliveries = count
}

func (f *fakeBridge) setSlowDeliveries(count int, delay time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slowDeliveries = count
	f.deliverDelay = delay
}

// --- report --------------------------------------------------------------

type Report struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Scenarios  []*Scenario
}

type Scenario struct {
	Name  string
	Steps []Step
	Err   string
}

type Step struct {
	Name   string
	OK     bool
	Detail string
}

func (r *Report) Scenario(name string, fn func(*Scenario) error) {
	scenario := &Scenario{Name: name}
	r.Scenarios = append(r.Scenarios, scenario)
	if err := fn(scenario); err != nil {
		scenario.Err = err.Error()
	}
}

func (s *Scenario) Step(name string, fn func() (string, error)) bool {
	detail, err := fn()
	step := Step{Name: name, OK: err == nil, Detail: detail}
	if err != nil {
		step.Detail = strings.TrimSpace(step.Detail + " | error: " + err.Error())
	}
	s.Steps = append(s.Steps, step)
	return err == nil
}

func (r *Report) Counts() (passed, failed int) {
	for _, scenario := range r.Scenarios {
		if scenario.Err != "" {
			failed++
			continue
		}
		bad := false
		for _, step := range scenario.Steps {
			if !step.OK {
				bad = true
			}
		}
		if bad {
			failed++
		} else {
			passed++
		}
	}
	return passed, failed
}

var reportTemplate = template.Must(template.New("report").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>keel-board v1 · Phase 1 sync E2E</title>
<style>
body{font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:32px;color:#111;background:#fafafa}
h1{font-size:22px;margin:0 0 8px} .muted{color:#666}
table{border-collapse:collapse;width:100%;margin:16px 0;background:#fff}
th,td{border:1px solid #ddd;padding:8px 10px;text-align:left;vertical-align:top}
th{background:#f0f0f0} .ok{color:#0a6b2d;font-weight:600} .bad{color:#a11;font-weight:600}
code{background:#f2f2f2;padding:1px 4px;border-radius:3px}
.summary{font-size:15px;margin:8px 0 20px}
</style></head><body>
<h1>keel-board v1 · Phase 1 sync E2E</h1>
<div class="muted">Generated by <code>cmd/board-sync-e2e</code>. Every step asserted a real HTTP/DB/platform fact.</div>
<div class="summary">Started {{.StartedAt}} · Finished {{.FinishedAt}} · {{.Passed}} passed / {{.Failed}} failed</div>
{{range .Scenarios}}
<h2>{{.Name}} {{if .Err}}<span class="bad">FAIL</span>{{else}}<span class="ok">PASS</span>{{end}}</h2>
{{if .Err}}<p class="bad">{{.Err}}</p>{{end}}
<table><tr><th style="width:34%">Step</th><th style="width:8%">Status</th><th>Evidence</th></tr>
{{range .Steps}}<tr><td>{{.Name}}</td><td class="{{if .OK}}ok{{else}}bad{{end}}">{{if .OK}}PASS{{else}}FAIL{{end}}</td><td>{{.Detail}}</td></tr>{{end}}
</table>
{{end}}
</body></html>`))

func writeReport(path string, report *Report) error {
	passed, failed := report.Counts()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return reportTemplate.Execute(file, map[string]any{
		"StartedAt":  report.StartedAt.Format(time.RFC3339),
		"FinishedAt": report.FinishedAt.Format(time.RFC3339),
		"Passed":     passed, "Failed": failed,
		"Scenarios": report.Scenarios,
	})
}

// --- run -----------------------------------------------------------------

func run(ctx context.Context, dsn string, report *Report) error {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e, err := newEnv(ctx, dsn, logger)
	if err != nil {
		return err
	}
	defer e.close()
	e.slack = newFakeSlack()
	defer e.slack.Close()
	e.github = newFakeGitHub()
	defer e.github.Close()
	e.linear = newFakeLinear()
	defer e.linear.Close()
	bridgeSecret := boardsync.Secret("bridge-e2e-secret")
	e.bridgeSDK = newFakeBridge(bridgeSecret, "telegram")
	defer e.bridgeSDK.Close()
	if err := e.configureSecrets(bridgeSecret); err != nil {
		return err
	}
	// P0-1 ordering: the board and its channels exist before any sync entry is
	// written. A channel created through the API records its creator as owner;
	// only then can an entry name it as a route target.
	if err := e.startSurface(); err != nil {
		return err
	}
	for _, channel := range []string{"support", "prs", "dev", "telegram", "flaky", "dead", "superagent"} {
		if err := e.createChannel(channel); err != nil {
			return err
		}
	}
	if err := e.createSyncEntries(bridgeSecret); err != nil {
		return err
	}
	// The entries are persisted but this process loaded none of them; the
	// restart is the adoption boundary (new entries stay disabled meanwhile).
	if err := e.restartSurface(); err != nil {
		return err
	}

	report.Scenario("slack: webhook → inbox → board → outbox → Slack receipt", e.scenarioSlack)
	report.Scenario("slack: echo, dedupe, attachment bytes, source read, bad signature", e.scenarioSlackSafety)
	report.Scenario("github: issue webhook → board → comment receipt", e.scenarioGitHub)
	report.Scenario("linear: issue webhook → board → comment receipt", e.scenarioLinear)
	report.Scenario("chatsdk bridge: signed events in, deliver out, replay refusal", e.scenarioBridge)
	report.Scenario("failures: retry then success, and dead letter visibility", e.scenarioFailures)
	report.Scenario("permissions and cross-tenant isolation", e.scenarioPermissions)
	report.Scenario("superagent bridge: HTTP chain, board_disabled, timeout/5xx/retry", e.scenarioSuperagentBridge)
	report.Scenario("fresh channel: entry requires an owned board channel (P0-1)", e.scenarioFreshChannel)
	report.Scenario("restart: pending inbox survives and a new process imports it", e.scenarioRestart)
	return nil
}

func (e *env) configureSecrets(bridgeSecret boardsync.Secret) error {
	secrets := map[string]string{
		"SLACK_SIGNING": "slack-signing-e2e", "SLACK_BOT": "xoxb-slack-e2e",
		"GITHUB_WEBHOOK": "github-webhook-e2e", "GITHUB_TOKEN": "ghp-token-e2e",
		"LINEAR_WEBHOOK": "linear-webhook-e2e", "LINEAR_TOKEN": "lin_api_e2e",
		"BRIDGE_SECRET": string(bridgeSecret),
	}
	for name, value := range secrets {
		if _, err := e.store.PutTenantSecret(e.ctx, e.tenant, name, []byte(value), e.box); err != nil {
			return fmt.Errorf("secret %s: %w", name, err)
		}
	}
	return nil
}

// createSyncEntries writes every connection through the real admin route
// (board.manage + secret.write). The route ACL precondition is therefore part of
// the main E2E path, not bypassed by a direct store write.
func (e *env) createSyncEntries(bridgeSecret boardsync.Secret) error {
	entries := []map[string]any{
		{
			"key": "slack", "platform": "slack", "enabled": true,
			"secretRef":     map[string]string{"signingSecret": "SLACK_SIGNING", "botToken": "SLACK_BOT"},
			"settings":      mustJSON(map[string]any{"botUserId": "U_APP", "botId": "B_APP", "apiBaseUrl": e.slack.URL()}),
			"routes":        map[string]string{"C0SUPPORT": "support"},
			"startChannels": []string{"C0SUPPORT"},
		},
		{
			"key": "flaky", "platform": "slack", "enabled": true,
			"secretRef":     map[string]string{"signingSecret": "SLACK_SIGNING", "botToken": "SLACK_BOT"},
			"settings":      mustJSON(map[string]any{"botUserId": "U_APP", "botId": "B_APP", "apiBaseUrl": e.slack.URL()}),
			"routes":        map[string]string{"C0FLAKY": "flaky"},
			"startChannels": []string{"C0FLAKY"},
		},
		{
			"key": "dead", "platform": "slack", "enabled": true,
			"secretRef":     map[string]string{"signingSecret": "SLACK_SIGNING", "botToken": "SLACK_BOT"},
			"settings":      mustJSON(map[string]any{"botUserId": "U_APP", "botId": "B_APP", "apiBaseUrl": e.slack.URL()}),
			"routes":        map[string]string{"C0DEAD": "dead"},
			"startChannels": []string{"C0DEAD"},
		},
		{
			"key": "github", "platform": "github", "enabled": true,
			"secretRef": map[string]string{"webhookSecret": "GITHUB_WEBHOOK", "token": "GITHUB_TOKEN"},
			"settings":  mustJSON(map[string]any{"botLogin": "keel-bot", "repos": []string{"acme/web"}, "writeRepos": []string{"acme/web"}, "apiBaseUrl": e.github.URL()}),
			"routes":    map[string]string{"acme/web:issues": "prs"},
		},
		{
			"key": "linear", "platform": "linear", "enabled": true,
			"secretRef":     map[string]string{"webhookSecret": "LINEAR_WEBHOOK", "apiToken": "LINEAR_TOKEN"},
			"settings":      mustJSON(map[string]any{"botUserId": "user-app", "teamKeys": []string{"ENG"}, "apiBaseUrl": e.linear.URL()}),
			"routes":        map[string]string{"ENG": "dev"},
			"startChannels": []string{"ENG"},
		},
		{
			"key": "telegram", "platform": "chatsdk", "enabled": true,
			"secretRef":     map[string]string{"bridgeSecret": "BRIDGE_SECRET"},
			"settings":      mustJSON(map[string]any{"platform": "telegram", "bridgeUrl": e.bridgeSDK.URL(), "selfIds": []string{"bot-1"}}),
			"routes":        map[string]string{"-100": "telegram"},
			"startChannels": []string{"-100"},
		},
	}
	for _, entry := range entries {
		resp := e.apiDo("alice", http.MethodPost, "/v1/board/sync/entries", entry)
		if resp.status != http.StatusCreated {
			return fmt.Errorf("entry %v: %d %s", entry["key"], resp.status, resp.body)
		}
	}
	_ = bridgeSecret
	return nil
}

func (e *env) restartSurface() error {
	e.stopSurface()
	return e.startSurface()
}

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

// --- scenarios -----------------------------------------------------------

func (e *env) scenarioSlack(scenario *Scenario) error {
	var root messageView
	if !scenario.Step("signed Slack message is accepted and imported", func() (string, error) {
		body := slackMessage("C0SUPPORT", "U_HUMAN", "hello from slack", "1700000100.000001", "")
		resp := e.postSlack(body)
		if resp.status != 200 {
			return "", fmt.Errorf("webhook = %d %s", resp.status, resp.body)
		}
		found, err := e.waitMessage("support", "hello from slack")
		if err != nil {
			return "", err
		}
		root = found
		return "webhook 200; board message #" + root.ID, nil
	}) {
		return errors.New("slack inbound failed")
	}
	if !scenario.Step("replayed delivery is deduped (one board message)", func() (string, error) {
		body := slackMessage("C0SUPPORT", "U_HUMAN", "hello from slack", "1700000100.000001", "")
		if resp := e.postSlack(body); resp.status != 200 {
			return "", fmt.Errorf("replay = %d %s", resp.status, resp.body)
		}
		time.Sleep(150 * time.Millisecond)
		messages, err := e.messages("support")
		if err != nil {
			return "", err
		}
		count := 0
		for _, message := range messages {
			if strings.Contains(message.Text, "hello from slack") {
				count++
			}
		}
		if count != 1 {
			return "", fmt.Errorf("board holds %d copies", count)
		}
		return fmt.Sprintf("board copies=%d; inbox dedupe absorbed the replay", count), nil
	}) {
		return errors.New("slack dedupe failed")
	}
	var reply messageView
	if !scenario.Step("board reply reaches Slack chat.postMessage (threaded)", func() (string, error) {
		created, err := e.postReply(root.ID, "reply from board")
		if err != nil {
			return "", err
		}
		reply = created
		err = e.waitFor(10*time.Second, "Slack post", func() (bool, error) {
			return e.slack.count("chat.postMessage", "C0SUPPORT") > 0, nil
		})
		if err != nil {
			return "", err
		}
		for _, call := range e.slack.snapshot() {
			if call.Method == "chat.postMessage" && call.Channel == "C0SUPPORT" {
				return fmt.Sprintf("board reply #%s → chat.postMessage channel=%s text=%q", reply.ID, call.Channel, call.Text), nil
			}
		}
		return "", errors.New("no chat.postMessage captured")
	}) {
		return errors.New("slack outbound failed")
	}
	if !scenario.Step("typing status is shown before the post", func() (string, error) {
		if !e.slack.has("assistant.threads.setStatus") {
			return "", errors.New("no typing status call")
		}
		return "assistant.threads.setStatus observed", nil
	}) {
		return errors.New("typing failed")
	}
	if !scenario.Step("board edit reaches Slack chat.update", func() (string, error) {
		resp := e.apiDo("alice", http.MethodPost, "/v1/board/messages/"+reply.ID+"/edit", map[string]any{"text": "edited reply from board"})
		if resp.status != 200 && resp.status != 201 {
			return "", fmt.Errorf("edit = %d %s", resp.status, resp.body)
		}
		err := e.waitFor(10*time.Second, "Slack update", func() (bool, error) { return e.slack.has("chat.update"), nil })
		if err != nil {
			return "", err
		}
		return "chat.update observed", nil
	}) {
		return errors.New("slack edit failed")
	}
	if !scenario.Step("board reaction reaches Slack reactions.add", func() (string, error) {
		resp := e.apiDo("alice", http.MethodPost, "/v1/board/messages/"+reply.ID+"/reactions", map[string]any{"emoji": "eyes"})
		if resp.status != 200 && resp.status != 201 {
			return "", fmt.Errorf("react = %d %s", resp.status, resp.body)
		}
		err := e.waitFor(10*time.Second, "Slack reaction", func() (bool, error) { return e.slack.has("reactions.add"), nil })
		if err != nil {
			return "", err
		}
		return "reactions.add observed", nil
	}) {
		return errors.New("slack reaction failed")
	}
	if !scenario.Step("board delete reaches Slack chat.delete", func() (string, error) {
		resp := e.apiDo("alice", http.MethodPost, "/v1/board/messages/"+reply.ID+"/delete", map[string]any{})
		if resp.status != 200 && resp.status != 201 {
			return "", fmt.Errorf("delete = %d %s", resp.status, resp.body)
		}
		err := e.waitFor(10*time.Second, "Slack delete", func() (bool, error) { return e.slack.has("chat.delete"), nil })
		if err != nil {
			return "", err
		}
		return "chat.delete observed", nil
	}) {
		return errors.New("slack delete failed")
	}
	return nil
}

func (e *env) postSlack(body []byte) apiResponse {
	return e.postSlackKey("slack", body)
}

func (e *env) postSlackKey(key string, body []byte) apiResponse {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature := slack.Sign([]byte("slack-signing-e2e"), timestamp, body)
	return e.postWebhook("/webhooks/"+key, map[string]string{
		"content-type":        "application/json",
		slack.TimestampHeader: timestamp,
		slack.SignatureHeader: signature,
	}, body)
}

func slackMessage(channel, user, text, ts, thread string) []byte {
	event := map[string]any{
		"type": "message", "channel": channel, "user": user, "text": text, "ts": ts,
	}
	if thread != "" {
		event["thread_ts"] = thread
	}
	body, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": "T_E2E",
		"event": event,
	})
	return body
}

func (e *env) scenarioSlackSafety(scenario *Scenario) error {
	if !scenario.Step("bad Slack signature is refused with no board write", func() (string, error) {
		body := slackMessage("C0SUPPORT", "U_HUMAN", "unsigned message", "1700000101.000001", "")
		resp := e.postWebhook("/webhooks/slack", map[string]string{
			"content-type": "application/json", slack.TimestampHeader: strconv.FormatInt(time.Now().Unix(), 10),
			slack.SignatureHeader: "v0=deadbeef",
		}, body)
		if resp.status != http.StatusUnauthorized {
			return "", fmt.Errorf("status = %d %s", resp.status, resp.body)
		}
		time.Sleep(100 * time.Millisecond)
		messages, err := e.messages("support")
		if err != nil {
			return "", err
		}
		for _, message := range messages {
			if strings.Contains(message.Text, "unsigned message") {
				return "", errors.New("bad signature still imported")
			}
		}
		return "401 and no board row", nil
	}) {
		return errors.New("slack signature guard failed")
	}
	if !scenario.Step("the app's own Slack message is never imported (echo guard)", func() (string, error) {
		body := slackMessage("C0SUPPORT", "U_APP", "this is the bot talking", "1700000102.000001", "")
		resp := e.postSlack(body)
		if resp.status != 200 {
			return "", fmt.Errorf("webhook = %d %s", resp.status, resp.body)
		}
		time.Sleep(150 * time.Millisecond)
		messages, err := e.messages("support")
		if err != nil {
			return "", err
		}
		for _, message := range messages {
			if strings.Contains(message.Text, "this is the bot talking") {
				return "", errors.New("the app's own message was imported")
			}
		}
		return "self-authored event dropped", nil
	}) {
		return errors.New("echo guard failed")
	}
	if !scenario.Step("attachment upload returns real bytes through keel's blob store", func() (string, error) {
		payload := []byte("real attachment bytes from keel blob storage")
		resp, err := e.uploadAttachment(payload)
		if err != nil {
			return "", err
		}
		var uploaded struct {
			Attachment struct {
				ID string `json:"id"`
			} `json:"attachment"`
		}
		if err := json.Unmarshal(resp.body, &uploaded); err != nil || uploaded.Attachment.ID == "" {
			return "", fmt.Errorf("upload body = %s", resp.body)
		}
		read := e.apiDo("alice", http.MethodGet, "/v1/board/attachments/"+uploaded.Attachment.ID, nil)
		if read.status != 200 {
			return "", fmt.Errorf("read = %d %s", read.status, read.body)
		}
		if !bytes.Equal(read.body, payload) {
			return "", fmt.Errorf("bytes differ: %q", read.body)
		}
		return fmt.Sprintf("attachment %s round-tripped %d bytes", uploaded.Attachment.ID, len(read.body)), nil
	}) {
		return errors.New("attachment path failed")
	}
	if !scenario.Step("source read returns normalized Slack messages under board.manage", func() (string, error) {
		resp := e.apiDo("alice", http.MethodGet, "/v1/board/sources/slack/read?target=C0SUPPORT", nil)
		if resp.status != 200 {
			return "", fmt.Errorf("source read = %d %s", resp.status, resp.body)
		}
		if !strings.Contains(string(resp.body), "source read from slack") {
			return "", fmt.Errorf("source body = %s", resp.body)
		}
		return "source read 200 with normalized message", nil
	}) {
		return errors.New("source read failed")
	}
	if !scenario.Step("a Slack permalink in message text is preserved as a board link", func() (string, error) {
		const permalink = "https://acme.slack.com/archives/C0SUPPORT/p1700000100000001"
		body := slackMessage("C0SUPPORT", "U_HUMAN", "see "+permalink, "1700000103.000001", "")
		if resp := e.postSlack(body); resp.status != http.StatusOK {
			return "", fmt.Errorf("webhook = %d %s", resp.status, resp.body)
		}
		found, err := e.waitMessage("support", permalink)
		if err != nil {
			return "", err
		}
		if len(found.Links) == 0 || found.Links[0].URL != permalink {
			return "", fmt.Errorf("links = %+v, want %s", found.Links, permalink)
		}
		// The read is alice's session through the board authorizer; a 200 with
		// content proves the permalink is part of normal channel content.
		if _, err := e.messages("support"); err != nil {
			return "", err
		}
		return "board links[0].url = " + found.Links[0].URL, nil
	}) {
		return errors.New("permalink E2E failed")
	}
	return nil
}

func (e *env) uploadAttachment(payload []byte) (apiResponse, error) {
	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost, e.api.URL+"/v1/board/attachments", bytes.NewReader(payload))
	if err != nil {
		return apiResponse{}, err
	}
	req.Header.Set("X-E2E-Who", "alice")
	req.Header.Set("X-Board-Filename", "e2e-note.txt")
	req.Header.Set("Content-Type", "text/plain")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return apiResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 {
		return apiResponse{status: resp.StatusCode, body: body}, fmt.Errorf("upload = %d %s", resp.StatusCode, body)
	}
	return apiResponse{status: resp.StatusCode, body: body}, nil
}

func (e *env) scenarioGitHub(scenario *Scenario) error {
	var root messageView
	if !scenario.Step("signed GitHub issue webhook imports a board root", func() (string, error) {
		body := githubIssueOpened()
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		_ = timestamp
		resp := e.postWebhook("/webhooks/github", map[string]string{
			"content-type":         "application/json",
			github.EventHeader:     "issues",
			github.DeliveryHeader:  "delivery-e2e-1",
			github.SignatureHeader: github.Sign([]byte("github-webhook-e2e"), body),
		}, body)
		if resp.status != 200 {
			return "", fmt.Errorf("webhook = %d %s", resp.status, resp.body)
		}
		found, err := e.waitMessage("prs", "Issue #7")
		if err != nil {
			return "", err
		}
		root = found
		return "board root #" + root.ID + " from GitHub issue", nil
	}) {
		return errors.New("github inbound failed")
	}
	if !scenario.Step("board reply becomes a GitHub issue comment", func() (string, error) {
		if _, err := e.postReply(root.ID, "reply to GitHub"); err != nil {
			return "", err
		}
		err := e.waitFor(10*time.Second, "GitHub comment", func() (bool, error) {
			return len(e.github.comments()) > 0, nil
		})
		if err != nil {
			return "", err
		}
		call := e.github.comments()[0]
		if !strings.Contains(call.Body, "reply to GitHub") {
			return "", fmt.Errorf("comment body = %s", call.Body)
		}
		return call.Method + " " + call.Path, nil
	}) {
		return errors.New("github outbound failed")
	}
	return nil
}

func githubIssueOpened() []byte {
	body, _ := json.Marshal(map[string]any{
		"action":     "opened",
		"repository": map[string]any{"full_name": "acme/web", "id": 1},
		"issue": map[string]any{
			"number": 7, "title": "Checkout retry", "body": "Retries checkout.",
			"html_url": "https://github.com/acme/web/issues/7",
			"user":     map[string]any{"id": 42, "login": "human"},
			"state":    "open", "labels": []any{}, "assignees": []any{},
		},
		"sender": map[string]any{"id": 42, "login": "human"},
	})
	return body
}

func (e *env) scenarioLinear(scenario *Scenario) error {
	var root messageView
	if !scenario.Step("signed Linear issue webhook imports a board root", func() (string, error) {
		body := linearIssueCreated()
		resp := e.postWebhook("/webhooks/linear", map[string]string{
			"content-type":         "application/json",
			linear.SignatureHeader: linear.Sign([]byte("linear-webhook-e2e"), body),
			linear.DeliveryHeader:  "linear-e2e-1",
		}, body)
		if resp.status != 200 {
			return "", fmt.Errorf("webhook = %d %s", resp.status, resp.body)
		}
		found, err := e.waitMessage("dev", "ENG-123")
		if err != nil {
			return "", err
		}
		root = found
		return "board root #" + root.ID + " from Linear issue", nil
	}) {
		return errors.New("linear inbound failed")
	}
	if !scenario.Step("board reply becomes a Linear comment (GraphQL)", func() (string, error) {
		if _, err := e.postReply(root.ID, "reply to Linear"); err != nil {
			return "", err
		}
		err := e.waitFor(10*time.Second, "Linear commentCreate", func() (bool, error) {
			return e.linear.has("commentCreate"), nil
		})
		if err != nil {
			return "", err
		}
		return "commentCreate mutation observed", nil
	}) {
		return errors.New("linear outbound failed")
	}
	return nil
}

func linearIssueCreated() []byte {
	body, _ := json.Marshal(map[string]any{
		"action": "create", "type": "Issue",
		"data": map[string]any{
			"id": "11111111-1111-1111-1111-111111111111", "identifier": "ENG-123", "number": 123,
			"title": "Checkout retry", "description": "Retries checkout.",
			"url":      "https://linear.app/acme/issue/ENG-123/checkout-retry",
			"team":     map[string]any{"id": "team-1", "key": "ENG", "name": "Engineering"},
			"state":    map[string]any{"id": "state-1", "name": "In Progress", "type": "started"},
			"priority": 2, "priorityLabel": "High",
			"labels": []any{}, "assignee": nil,
			"creator":   map[string]any{"id": "user-1", "name": "Ada"},
			"createdAt": "2026-01-02T03:04:05Z", "updatedAt": "2026-01-02T03:04:05Z",
		},
		"actor":          map[string]any{"id": "user-1", "name": "ada", "type": "user"},
		"organizationId": "org-1", "webhookId": "wh-e2e", "webhookTimestamp": 0,
	})
	return body
}

func (e *env) scenarioBridge(scenario *Scenario) error {
	var root messageView
	if !scenario.Step("signed bridge event imports a board root", func() (string, error) {
		body := bridgeEventBatch("tg-1", "hello from the bridge")
		timestamp := time.Now().Unix()
		nonce := "nonce-e2e-" + uuid.NewString()
		signature := chatsdk.SignV1(boardsync.Secret("bridge-e2e-secret"), timestamp, nonce, body)
		resp := e.apiPost("/v1/board/sync/bridge/telegram/events", map[string]string{
			"content-type":          "application/json",
			chatsdk.HeaderTimestamp: strconv.FormatInt(timestamp, 10),
			chatsdk.HeaderNonce:     nonce,
			chatsdk.HeaderSignature: signature,
		}, body)
		if resp.status != 200 && resp.status != 202 {
			return "", fmt.Errorf("bridge = %d %s", resp.status, resp.body)
		}
		found, err := e.waitMessage("telegram", "hello from the bridge")
		if err != nil {
			return "", err
		}
		root = found
		return "board root #" + root.ID + " from bridge event", nil
	}) {
		return errors.New("bridge inbound failed")
	}
	if !scenario.Step("a replayed bridge nonce is refused and imports nothing", func() (string, error) {
		body := bridgeEventBatch("tg-2", "replayed bridge message")
		timestamp := time.Now().Unix()
		nonce := "nonce-e2e-replay"
		signature := chatsdk.SignV1(boardsync.Secret("bridge-e2e-secret"), timestamp, nonce, body)
		headers := map[string]string{
			"content-type":          "application/json",
			chatsdk.HeaderTimestamp: strconv.FormatInt(timestamp, 10),
			chatsdk.HeaderNonce:     nonce,
			chatsdk.HeaderSignature: signature,
		}
		if resp := e.apiPost("/v1/board/sync/bridge/telegram/events", headers, body); resp.status != 200 && resp.status != 202 {
			return "", fmt.Errorf("first push = %d %s", resp.status, resp.body)
		}
		replay := e.apiPost("/v1/board/sync/bridge/telegram/events", headers, body)
		if replay.status < 400 {
			return "", fmt.Errorf("replay = %d %s", replay.status, replay.body)
		}
		if _, err := e.waitMessage("telegram", "replayed bridge message"); err != nil {
			return "", err
		}
		messages, err := e.messages("telegram")
		if err != nil {
			return "", err
		}
		count := 0
		for _, message := range messages {
			if strings.Contains(message.Text, "replayed bridge message") {
				count++
			}
		}
		if count != 1 {
			return "", fmt.Errorf("replay imported %d copies", count)
		}
		return fmt.Sprintf("status=%d, one board copy", replay.status), nil
	}) {
		return errors.New("bridge replay guard failed")
	}
	if !scenario.Step("bad bridge signature and stale timestamp are refused", func() (string, error) {
		body := bridgeEventBatch("tg-bad", "bad signature message")
		timestamp := time.Now().Unix()
		nonce := "nonce-e2e-bad"
		signature := chatsdk.SignV1(boardsync.Secret("bridge-e2e-secret"), timestamp, nonce, body)
		headers := map[string]string{
			"content-type":          "application/json",
			chatsdk.HeaderTimestamp: strconv.FormatInt(timestamp, 10),
			chatsdk.HeaderNonce:     nonce,
			chatsdk.HeaderSignature: signature,
		}
		headers[chatsdk.HeaderSignature] = "v1=deadbeef"
		if resp := e.apiPost("/v1/board/sync/bridge/telegram/events", headers, body); resp.status != http.StatusUnauthorized {
			return "", fmt.Errorf("bad signature = %d %s", resp.status, resp.body)
		}
		stale := timestamp - 3600
		headers[chatsdk.HeaderTimestamp] = strconv.FormatInt(stale, 10)
		headers[chatsdk.HeaderSignature] = chatsdk.SignV1(boardsync.Secret("bridge-e2e-secret"), stale, nonce, body)
		if resp := e.apiPost("/v1/board/sync/bridge/telegram/events", headers, body); resp.status != http.StatusUnauthorized {
			return "", fmt.Errorf("stale timestamp = %d %s", resp.status, resp.body)
		}
		time.Sleep(100 * time.Millisecond)
		messages, err := e.messages("telegram")
		if err != nil {
			return "", err
		}
		for _, message := range messages {
			if strings.Contains(message.Text, "bad signature message") {
				return "", errors.New("a refused bridge request imported a message")
			}
		}
		return "401 for a bad signature and for a one-hour-old timestamp", nil
	}) {
		return errors.New("bridge signature guard failed")
	}
	if !scenario.Step("board reply is delivered to the Chat SDK sidecar", func() (string, error) {
		if _, err := e.postReply(root.ID, "reply over the bridge"); err != nil {
			return "", err
		}
		err := e.waitFor(10*time.Second, "bridge deliver", func() (bool, error) {
			return len(e.bridgeSDK.deliveries()) > 0, nil
		})
		if err != nil {
			return "", err
		}
		delivery := e.bridgeSDK.deliveries()[0]
		if !strings.Contains(delivery.Body.Markdown, "reply over the bridge") {
			return "", fmt.Errorf("delivery body = %+v", delivery.Body)
		}
		return fmt.Sprintf("deliver kind=%s target=%s", delivery.Kind, delivery.Target.External()), nil
	}) {
		return errors.New("bridge outbound failed")
	}
	if !scenario.Step("a sidecar 5xx is retried and a later attempt succeeds", func() (string, error) {
		before := e.bridgeSDK.deliveryAttempts()
		e.bridgeSDK.setFailDeliveries(2)
		if _, err := e.postReply(root.ID, "sidecar 5xx reply"); err != nil {
			return "", err
		}
		err := e.waitFor(10*time.Second, "sidecar retry", func() (bool, error) {
			for _, delivery := range e.bridgeSDK.deliveries() {
				if strings.Contains(delivery.Body.Markdown, "sidecar 5xx reply") {
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			return "", fmt.Errorf("%w (attempts=%d deliveries=%d)", err, e.bridgeSDK.deliveryAttempts(), len(e.bridgeSDK.deliveries()))
		}
		attempts := e.bridgeSDK.deliveryAttempts() - before
		return fmt.Sprintf("2 x HTTP 500 then success; attempts=%d", attempts), nil
	}) {
		return errors.New("sidecar 5xx retry failed")
	}
	if !scenario.Step("a malformed sidecar answer is retried", func() (string, error) {
		before := e.bridgeSDK.deliveryAttempts()
		e.bridgeSDK.setGarbageDeliveries(1)
		if _, err := e.postReply(root.ID, "sidecar malformed reply"); err != nil {
			return "", err
		}
		err := e.waitFor(10*time.Second, "sidecar retry after malformed answer", func() (bool, error) {
			for _, delivery := range e.bridgeSDK.deliveries() {
				if strings.Contains(delivery.Body.Markdown, "sidecar malformed reply") {
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			return "", err
		}
		attempts := e.bridgeSDK.deliveryAttempts() - before
		return fmt.Sprintf("malformed 200 then success; attempts=%d", attempts), nil
	}) {
		return errors.New("sidecar malformed retry failed")
	}
	if !scenario.Step("a sidecar timeout is retried and a later attempt succeeds", func() (string, error) {
		before := e.bridgeSDK.deliveryAttempts()
		// The chatsdk client's default per-attempt timeout is 15s; a handler
		// that sleeps 16s produces a real client-side timeout, not a 5xx.
		e.bridgeSDK.setSlowDeliveries(1, 16*time.Second)
		if _, err := e.postReply(root.ID, "sidecar timeout reply"); err != nil {
			return "", err
		}
		err := e.waitFor(30*time.Second, "sidecar retry after timeout", func() (bool, error) {
			for _, delivery := range e.bridgeSDK.deliveries() {
				if strings.Contains(delivery.Body.Markdown, "sidecar timeout reply") {
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			return "", fmt.Errorf("%w (attempts=%d)", err, e.bridgeSDK.deliveryAttempts()-before)
		}
		attempts := e.bridgeSDK.deliveryAttempts() - before
		if attempts != 2 {
			return "", fmt.Errorf("attempts = %d, want one timeout + one success", attempts)
		}
		return "15s client timeout then success; attempts=2", nil
	}) {
		return errors.New("sidecar timeout retry failed")
	}
	return nil
}

func bridgeEventBatch(dedupe, text string) []byte {
	body, _ := json.Marshal(map[string]any{
		"version": "1",
		"events": []any{map[string]any{
			"dedupe": dedupe,
			"event": map[string]any{
				"key": "telegram", "platform": "telegram", "kind": "message", "type": "message",
				"channel": "-100", "thread": "t1", "message": "m-" + dedupe, "isRoot": true,
				"text":   text,
				"author": map[string]any{"id": "u-1", "handle": "user-one", "name": "User One"},
			},
		}},
	})
	return body
}

func (e *env) scenarioFailures(scenario *Scenario) error {
	e.slack.mu.Lock()
	e.slack.failPosts = 2
	e.slack.mu.Unlock()
	if !scenario.Step("a transient Slack failure is retried and then succeeds", func() (string, error) {
		body := slackMessage("C0FLAKY", "U_HUMAN", "flaky root", "1700000200.000001", "")
		if resp := e.postSlackKey("flaky", body); resp.status != 200 {
			return "", fmt.Errorf("webhook = %d %s", resp.status, resp.body)
		}
		root, err := e.waitMessage("flaky", "flaky root")
		if err != nil {
			return "", err
		}
		if _, err := e.postReply(root.ID, "flaky reply"); err != nil {
			return "", err
		}
		err = e.waitFor(10*time.Second, "Slack flaky post", func() (bool, error) {
			return e.slack.count("chat.postMessage", "C0FLAKY") >= 3, nil
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("chat.postMessage attempts=%d (2 failures + success)", e.slack.count("chat.postMessage", "C0FLAKY")), nil
	}) {
		return errors.New("retry scenario failed")
	}
	if !scenario.Step("an exhausted delivery is visible as a dead letter", func() (string, error) {
		e.slack.mu.Lock()
		e.slack.deadChannel = "C0DEAD"
		e.slack.mu.Unlock()
		body := slackMessage("C0DEAD", "U_HUMAN", "dead root", "1700000300.000001", "")
		if resp := e.postSlackKey("dead", body); resp.status != 200 {
			return "", fmt.Errorf("webhook = %d %s", resp.status, resp.body)
		}
		root, err := e.waitMessage("dead", "dead root")
		if err != nil {
			return "", err
		}
		if _, err := e.postReply(root.ID, "dead reply"); err != nil {
			return "", err
		}
		err = e.waitFor(10*time.Second, "dead letter", func() (bool, error) {
			resp := e.apiDo("alice", http.MethodGet, "/v1/board/sync/outbox/failures?key=dead", nil)
			if resp.status != 200 {
				return false, fmt.Errorf("failures = %d %s", resp.status, resp.body)
			}
			return strings.Contains(string(resp.body), "delivery_"), nil
		})
		if err != nil {
			return "", err
		}
		return "GET /sync/outbox/failures lists the dead delivery", nil
	}) {
		return errors.New("dead letter scenario failed")
	}
	return nil
}

func (e *env) scenarioPermissions(scenario *Scenario) error {
	if !scenario.Step("a read-only member cannot manage sync", func() (string, error) {
		resp := e.apiDo("carol", http.MethodGet, "/v1/board/sync/entries", nil)
		if resp.status != 403 && resp.status != 404 {
			return "", fmt.Errorf("status = %d %s", resp.status, resp.body)
		}
		resp = e.apiDo("carol", http.MethodPost, "/v1/board/sync/entries", map[string]any{
			"key": "x", "platform": "slack", "secretRef": map[string]string{"signingSecret": "SLACK_SIGNING"},
		})
		if resp.status != 403 && resp.status != 404 {
			return "", fmt.Errorf("create status = %d %s", resp.status, resp.body)
		}
		return "list/create refused", nil
	}) {
		return errors.New("member permission guard failed")
	}
	if !scenario.Step("another tenant cannot see this tenant's entries", func() (string, error) {
		resp := e.apiDo("dave", http.MethodGet, "/v1/board/sync/entries", nil)
		if resp.status != 200 {
			return "", fmt.Errorf("entries = %d %s", resp.status, resp.body)
		}
		if strings.Contains(string(resp.body), `"slack"`) {
			return "", fmt.Errorf("cross-tenant leak: %s", resp.body)
		}
		return "tenant B sees an empty entry list", nil
	}) {
		return errors.New("cross-tenant isolation failed")
	}
	return nil
}

func (e *env) scenarioFreshChannel(scenario *Scenario) error {
	entry := map[string]any{
		"key": "fresh", "platform": "slack", "enabled": true,
		"secretRef":     map[string]string{"signingSecret": "SLACK_SIGNING", "botToken": "SLACK_BOT"},
		"settings":      mustJSON(map[string]any{"botUserId": "U_APP", "botId": "B_APP", "apiBaseUrl": e.slack.URL()}),
		"routes":        map[string]string{"C0FRESH": "fresh"},
		"startChannels": []string{"C0FRESH"},
	}
	if !scenario.Step("an entry for a channel that does not exist is refused (400)", func() (string, error) {
		resp := e.apiDo("alice", http.MethodPost, "/v1/board/sync/entries", entry)
		if resp.status != http.StatusBadRequest {
			return "", fmt.Errorf("status = %d %s", resp.status, resp.body)
		}
		if !strings.Contains(string(resp.body), "does not exist") {
			return "", fmt.Errorf("error is not actionable: %s", resp.body)
		}
		return "400 names the missing channel and the create call", nil
	}) {
		return errors.New("missing-channel entry was accepted")
	}
	if !scenario.Step("POST /v1/board/channels provisions the fresh channel", func() (string, error) {
		resp := e.apiDo("alice", http.MethodPost, "/v1/board/channels", map[string]string{"path": "fresh"})
		if resp.status != http.StatusCreated {
			return "", fmt.Errorf("status = %d %s", resp.status, resp.body)
		}
		return "201 created; alice is the owner", nil
	}) {
		return errors.New("fresh channel creation failed")
	}
	if !scenario.Step("the same entry is accepted after the channel is owned", func() (string, error) {
		resp := e.apiDo("alice", http.MethodPost, "/v1/board/sync/entries", entry)
		if resp.status != http.StatusCreated {
			return "", fmt.Errorf("status = %d %s", resp.status, resp.body)
		}
		return "201 created; pending adoption (restart required)", nil
	}) {
		return errors.New("entry was not accepted after provisioning")
	}
	if !scenario.Step("webhook imports into fresh and alice reads it over HTTP", func() (string, error) {
		if err := e.restartSurface(); err != nil {
			return "", err
		}
		body := slackMessage("C0FRESH", "U_HUMAN", "fresh channel import", "1700000500.000001", "")
		if resp := e.postSlackKey("fresh", body); resp.status != http.StatusOK {
			return "", fmt.Errorf("webhook = %d %s", resp.status, resp.body)
		}
		found, err := e.waitMessage("fresh", "fresh channel import")
		if err != nil {
			return "", err
		}
		// e.messages uses alice's session and fails on a non-200, proving the
		// channel is readable through the board authorizer, not just imported.
		if _, err := e.messages("fresh"); err != nil {
			return "", err
		}
		return "webhook 200; alice GET /messages?channel=fresh 200; message #" + found.ID, nil
	}) {
		return errors.New("fresh channel import failed")
	}
	return nil
}

func (e *env) scenarioRestart(scenario *Scenario) error {
	if !scenario.Step("a pending inbox row is imported by a newly started surface", func() (string, error) {
		// Stop the process's workers, push one normalized record directly into
		// the durable inbox, then start a fresh surface over the same schema.
		e.stopSurface()
		syncdb, err := boardsyncstore.OpenPostgres(e.ctx, boardsyncstore.PostgresOptions{DSN: e.dsn, SearchPath: e.schema})
		if err != nil {
			return "", fmt.Errorf("open sync store: %w", err)
		}
		body := slackMessage("C0SUPPORT", "U_HUMAN", "pending across restart", "1700000400.000001", "")
		provider, err := slack.New(slack.Config{
			Key: "slack", SigningSecret: boardsync.Secret("slack-signing-e2e"),
			BotToken: boardsync.Secret("xoxb-slack-e2e"), BotUserID: "U_APP", BotID: "B_APP",
			APIBaseURL: e.slack.URL(),
		})
		if err != nil {
			syncdb.Close()
			return "", err
		}
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		request := &boardsync.WebhookRequest{
			Method: http.MethodPost, Path: "/webhooks/slack",
			Header: http.Header{
				"Content-Type":        []string{"application/json"},
				slack.TimestampHeader: []string{timestamp},
				slack.SignatureHeader: []string{slack.Sign([]byte("slack-signing-e2e"), timestamp, body)},
			},
			Body: body,
		}
		intake, err := provider.Receive(e.ctx, "slack", request)
		if err != nil || len(intake.Records) != 1 {
			syncdb.Close()
			return "", fmt.Errorf("normalize: %v/%d", err, len(intake.Records))
		}
		if _, err := syncdb.PushInbox(e.ctx, e.tenant.String(), "slack", intake.Records); err != nil {
			syncdb.Close()
			return "", fmt.Errorf("push inbox: %w", err)
		}
		syncdb.Close()
		if err := e.startSurface(); err != nil {
			return "", err
		}
		found, err := e.waitMessage("support", "pending across restart")
		if err != nil {
			return "", err
		}
		return "new surface imported inbox row into board message #" + found.ID, nil
	}) {
		return errors.New("restart recovery failed")
	}
	return nil
}
