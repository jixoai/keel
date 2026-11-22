package harnessext

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/notify"
)

type notifyTestPusher struct {
	mu    sync.Mutex
	calls []notify.PushRequestInfo
}

func (p *notifyTestPusher) Push(_ context.Context, req notify.PushRequestInfo) (notify.PushResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	return notify.PushResult{Channel: req.Channel}, nil
}

func (p *notifyTestPusher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

type notifyPluginHarness struct {
	plugin  *Plugin
	service *notify.Service
	pusher  *notifyTestPusher
	tenant  uuid.UUID
	session uuid.UUID
	ctx     context.Context
}

func newNotifyPluginHarness(t *testing.T) *notifyPluginHarness {
	t.Helper()
	tenant := uuid.New()
	session := uuid.New()
	mem := kernel.NewMemStore()
	mem.CreateSession(tenant, session)
	committer := kernel.New(mem, kernel.Options{})
	pusher := &notifyTestPusher{}
	service := notify.NewService(notify.Options{
		Store:         notify.NewMemoryStore(),
		Committer:     committer,
		Pusher:        pusher,
		Logger:        testLogger(),
		DefaultWindow: time.Minute,
	})
	plugin := NewNotifyPlugin(NotifyOptions{Service: service, Logger: testLogger()})
	ctx := WithRunScope(context.Background(), RunScope{TenantID: tenant, SessionID: session, RunID: "run-notify-1"})
	return &notifyPluginHarness{plugin: plugin, service: service, pusher: pusher, tenant: tenant, session: session, ctx: ctx}
}

func (h *notifyPluginHarness) execute(t *testing.T, name, args string) map[string]any {
	t.Helper()
	for i := range h.plugin.Tools {
		if h.plugin.Tools[i].Name != name {
			continue
		}
		result, err := h.plugin.Tools[i].Execute(h.ctx, "call-1", json.RawMessage(args), nil)
		if err != nil {
			t.Fatalf("execute %s: %v", name, err)
		}
		document, ok := result.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("%s structured content = %#v", name, result.StructuredContent)
		}
		return document
	}
	t.Fatalf("no tool %s", name)
	return nil
}

func TestNotifyPushListAck(t *testing.T) {
	h := newNotifyPluginHarness(t)
	doc := h.execute(t, NotifyToolPush, `{"text":"Deploy is waiting","title":"Keel","consumers":[
		{"id":"web","kind":"client","online":true},
		{"id":"mail","kind":"url"}]}`)
	view, ok := doc["notification"].(notify.View)
	if !ok {
		t.Fatalf("notification = %#v", doc["notification"])
	}
	if view.NotificationID == "" || view.Status != notify.StatusDelivered {
		t.Fatalf("view = %+v", view)
	}
	if view.URLs["mail"] == "" {
		t.Fatalf("urls = %+v", view.URLs)
	}

	list := h.execute(t, NotifyToolList, `{"limit":10}`)
	if list["count"] != 1 {
		t.Fatalf("list = %+v", list)
	}

	ack := h.execute(t, NotifyToolAck, `{"notificationId":"`+view.NotificationID+`","consumerId":"web"}`)
	acked, ok := ack["notification"].(notify.View)
	if !ok || acked.Status != notify.StatusConsumed {
		t.Fatalf("ack = %#v", ack)
	}
	if ack["duplicate"] != false {
		t.Fatalf("duplicate = %v", ack["duplicate"])
	}
	again := h.execute(t, NotifyToolAck, `{"notificationId":"`+view.NotificationID+`","consumerId":"web"}`)
	if again["duplicate"] != true {
		t.Fatalf("second ack = %+v", again)
	}
}

func TestNotifyPushIsIdempotentPerRun(t *testing.T) {
	h := newNotifyPluginHarness(t)
	args := `{"text":"same notice","consumers":[{"id":"web","kind":"client","online":true}]}`
	first := h.execute(t, NotifyToolPush, args)
	second := h.execute(t, NotifyToolPush, args)
	firstView := first["notification"].(notify.View)
	secondView := second["notification"].(notify.View)
	if firstView.NotificationID != secondView.NotificationID {
		t.Fatalf("retry minted a second notification: %s vs %s", firstView.NotificationID, secondView.NotificationID)
	}
	if secondView.Created {
		t.Fatal("the retry claims it created the notification")
	}
	different := h.execute(t, NotifyToolPush, `{"text":"another notice","consumers":[{"id":"web","kind":"client","online":true}]}`)
	if different["notification"].(notify.View).NotificationID == firstView.NotificationID {
		t.Fatal("a different notice collapsed into the first")
	}
	list := h.execute(t, NotifyToolList, `{}`)
	if list["count"] != 2 {
		t.Fatalf("count = %v, want 2", list["count"])
	}
}

func TestNotifyPushRequiresRunScope(t *testing.T) {
	h := newNotifyPluginHarness(t)
	for i := range h.plugin.Tools {
		result, err := h.plugin.Tools[i].Execute(context.Background(), "call-1", json.RawMessage(`{"text":"x"}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		document := result.StructuredContent.(map[string]any)
		if document["code"] != "run_scope_missing" {
			t.Fatalf("%s = %+v", h.plugin.Tools[i].Name, document)
		}
	}
}

func TestNotifyPushValidatesArguments(t *testing.T) {
	h := newNotifyPluginHarness(t)
	cases := []struct {
		name string
		args string
	}{
		{"missing text", `{"consumers":[{"id":"web","kind":"client"}]}`},
		{"missing consumers", `{"text":"x"}`},
		{"bad window", `{"text":"x","windowMs":10,"consumers":[{"id":"web","kind":"client"}]}`},
		{"bad kind", `{"text":"x","consumers":[{"id":"web","kind":"telepathy"}]}`},
		{"duplicate consumer", `{"text":"x","consumers":[{"id":"web","kind":"client"},{"id":"web","kind":"url"}]}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			doc := h.execute(t, NotifyToolPush, test.args)
			if doc["status"] != "failed" || doc["code"] != "invalid_arguments" {
				t.Fatalf("document = %+v", doc)
			}
		})
	}
}

func TestNotifyWithoutServiceFailsClosed(t *testing.T) {
	plugin := NewNotifyPlugin(NotifyOptions{})
	for i := range plugin.Tools {
		ctx := WithRunScope(context.Background(), RunScope{TenantID: uuid.New(), SessionID: uuid.New()})
		result, err := plugin.Tools[i].Execute(ctx, "call-1", json.RawMessage(`{"text":"x"}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		document := result.StructuredContent.(map[string]any)
		if document["code"] != "not_wired" {
			t.Fatalf("%s = %+v", plugin.Tools[i].Name, document)
		}
	}
}

func TestNotifyPluginDeclaresOnlyInternalTools(t *testing.T) {
	plugin := NewNotifyPlugin(NotifyOptions{})
	if names := toolNames(plugin.Tools); len(names) != 0 {
		t.Fatalf("notify plugin publishes tools = %v, want none (the harness owns the name)", names)
	}
	for _, tool := range plugin.Tools {
		if !tool.Internal {
			t.Fatalf("%s is not marked internal", tool.Name)
		}
	}
}

// TestNotifyPluginSweeperStops verifies the Start/Stop pair owns the sweep
// goroutine: Start runs it and Stop ends it without a leak.
func TestNotifyPluginSweeperStops(t *testing.T) {
	h := newNotifyPluginHarness(t)
	if err := h.plugin.Start(context.Background(), &PluginEnv{Logger: testLogger()}); err != nil {
		t.Fatal(err)
	}
	view, err := h.service.Push(context.Background(), notify.PushRequest{
		TenantID:  h.tenant,
		SessionID: h.session,
		Text:      "sweep me",
		Consumers: []notify.ConsumerSpec{{ID: "phone", Kind: notify.ConsumerNative, Online: true}},
		Window:    time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.pusher.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if h.pusher.count() != 1 {
		t.Fatalf("pusher calls = %d, want 1", h.pusher.count())
	}
	if err := h.plugin.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if view.NotificationID == "" {
		t.Fatal("empty notification id")
	}
}
