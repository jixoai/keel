package notify

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

var (
	notifyTenant  = uuid.MustParse("aaaaaaaa-1111-1111-1111-111111111111")
	notifySession = uuid.MustParse("bbbbbbbb-2222-2222-2222-222222222222")
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type recordingPusher struct {
	calls  []PushRequestInfo
	result PushResult
	err    error
}

func (p *recordingPusher) Push(_ context.Context, req PushRequestInfo) (PushResult, error) {
	p.calls = append(p.calls, req)
	if p.err != nil {
		return PushResult{}, p.err
	}
	return p.result, nil
}

type notifyHarness struct {
	service *Service
	store   *MemoryStore
	clock   *testClock
	kernel  *kernel.Kernel
	pusher  *recordingPusher
	scope   kernel.Scope
}

func newNotifyHarness(t *testing.T, opts Options) *notifyHarness {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	mem := kernel.NewMemStore()
	scope := kernel.Scope{TenantID: notifyTenant, SessionID: notifySession}
	mem.CreateSession(scope.TenantID, scope.SessionID)
	k := kernel.New(mem, kernel.Options{})
	memory := NewMemoryStore()
	if opts.Store == nil {
		opts.Store = memory
	}
	opts.Now = clock.Now
	if opts.DefaultWindow == 0 {
		opts.DefaultWindow = 10 * time.Second
	}
	if opts.URLTTL == 0 {
		opts.URLTTL = time.Hour
	}
	if opts.Committer == nil {
		opts.Committer = k
	}
	pusher := &recordingPusher{}
	if opts.Pusher == nil {
		opts.Pusher = pusher
	}
	service := NewService(opts)
	return &notifyHarness{service: service, store: memory, clock: clock, kernel: k, pusher: pusher, scope: scope}
}

func (h *notifyHarness) push(t *testing.T, consumers ...ConsumerSpec) View {
	t.Helper()
	view, err := h.service.Push(context.Background(), PushRequest{
		TenantID:  h.scope.TenantID,
		SessionID: h.scope.SessionID,
		Text:      "a task needs you",
		Consumers: consumers,
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	return view
}

func (h *notifyHarness) events(t *testing.T, kind kernel.EventKind) []kernel.Event {
	t.Helper()
	events, err := h.kernel.LoadEvents(context.Background(), h.scope, 0, 200)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var out []kernel.Event
	for _, event := range events {
		if event.Kind == kind {
			out = append(out, event)
		}
	}
	return out
}

func clientConsumer(id string, online bool) ConsumerSpec {
	ack := true
	return ConsumerSpec{ID: id, Kind: ConsumerClient, Online: online, Ack: &ack}
}

func TestAckInsideWindowSuppressesPush(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	view := h.push(t, clientConsumer("web", true))
	if view.Status != StatusDelivered {
		t.Fatalf("status = %s, want delivered (the live consumer received it)", view.Status)
	}
	// The live consumer acks inside the window: the ack lands and no push is
	// ever attempted, even after the window passes.
	acked, duplicate, err := h.service.Ack(context.Background(), h.scope.TenantID,
		uuid.MustParse(view.NotificationID), "web", "owner@example.test", ChannelAPI)
	if err != nil || duplicate {
		t.Fatalf("ack: duplicate=%v err=%v", duplicate, err)
	}
	if acked.Status != StatusConsumed {
		t.Fatalf("acked status = %s, want consumed", acked.Status)
	}
	h.clock.Advance(11 * time.Second)
	if err := h.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.pusher.calls) != 0 {
		t.Fatalf("pusher calls after ack = %d, want 0", len(h.pusher.calls))
	}
	if got := len(h.events(t, kernel.KindNotificationAck)); got != 1 {
		t.Fatalf("ack events = %d, want 1", got)
	}
	if got := len(h.events(t, kernel.KindNotificationPushSent)); got != 0 {
		t.Fatalf("push events = %d, want 0", got)
	}
	// A duplicate ack is one durable fact, not two.
	if _, duplicate, err := h.service.Ack(context.Background(), h.scope.TenantID,
		uuid.MustParse(view.NotificationID), "web", "owner@example.test", ChannelAPI); err != nil || !duplicate {
		t.Fatalf("duplicate ack = duplicate=%v err=%v", duplicate, err)
	}
	if got := len(h.events(t, kernel.KindNotificationAck)); got != 1 {
		t.Fatalf("ack events after duplicate = %d, want 1", got)
	}
}

func TestUnackedWebConsumerDegradesToInPageOnce(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	view := h.push(t, clientConsumer("web", true))
	h.clock.Advance(11 * time.Second)
	if err := h.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	// No service worker: the explicit degradation is an in-page notice with a
	// registered gap, not a fake system push.
	got, err := h.service.Get(context.Background(), h.scope.TenantID, uuid.MustParse(view.NotificationID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Push == nil || !got.Push.Degraded || got.Push.Channel != "in_page" {
		t.Fatalf("push = %+v", got.Push)
	}
	if got.Push.Gap != "web_service_worker_unavailable" {
		t.Fatalf("gap = %q", got.Push.Gap)
	}
	if got.Status != StatusDelivered {
		t.Fatalf("status = %s", got.Status)
	}
	// A second sweep must not push again.
	h.clock.Advance(time.Minute)
	if err := h.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(h.events(t, kernel.KindNotificationPushSent)); got != 1 {
		t.Fatalf("push events = %d, want 1", got)
	}
	// Late ack still works after the fallback push.
	acked, _, err := h.service.Ack(context.Background(), h.scope.TenantID,
		uuid.MustParse(view.NotificationID), "web", "", ChannelAPI)
	if err != nil || acked.Status != StatusConsumed {
		t.Fatalf("late ack = %+v err=%v", acked, err)
	}
}

func TestNativeConsumerUsesThePusherOnce(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	h.pusher.result = PushResult{Channel: ConsumerNative}
	view := h.push(t, ConsumerSpec{ID: "phone", Kind: ConsumerNative, Online: true})
	h.clock.Advance(11 * time.Second)
	if err := h.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.pusher.calls) != 1 {
		t.Fatalf("pusher calls = %d, want 1", len(h.pusher.calls))
	}
	if h.pusher.calls[0].Channel != ConsumerNative || h.pusher.calls[0].NotificationID != view.NotificationID {
		t.Fatalf("push request = %+v", h.pusher.calls[0])
	}
	got, err := h.service.Get(context.Background(), h.scope.TenantID, uuid.MustParse(view.NotificationID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Push == nil || got.Push.Degraded || got.Push.Status != PushSent {
		t.Fatalf("push = %+v", got.Push)
	}
	if got := len(h.events(t, kernel.KindNotificationPushSent)); got != 1 {
		t.Fatalf("push events = %d, want 1", got)
	}
	h.clock.Advance(time.Hour)
	if err := h.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.pusher.calls) != 1 {
		t.Fatalf("pusher calls after second sweep = %d, want 1", len(h.pusher.calls))
	}
}

func TestNoPushTargetIsRecordedNotRetried(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	view := h.push(t, ConsumerSpec{ID: "link", Kind: ConsumerURL})
	if view.URLs["link"] == "" {
		t.Fatal("the url consumer got no consumption URL")
	}
	h.clock.Advance(11 * time.Second)
	if err := h.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := h.service.Get(context.Background(), h.scope.TenantID, uuid.MustParse(view.NotificationID))
	if got.Push == nil || got.Push.Gap != "no_push_target" || !got.Push.Degraded {
		t.Fatalf("push = %+v", got.Push)
	}
	if got := len(h.events(t, kernel.KindNotificationPushSent)); got != 1 {
		t.Fatalf("push events = %d, want 1", got)
	}
}

func TestMultipleConsumersAnyAckConsumes(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	view := h.push(t, clientConsumer("web", true), clientConsumer("tablet", true))
	id := uuid.MustParse(view.NotificationID)
	if _, _, err := h.service.Ack(context.Background(), h.scope.TenantID, id, "web", "owner@example.test", ChannelAPI); err != nil {
		t.Fatal(err)
	}
	second, duplicate, err := h.service.Ack(context.Background(), h.scope.TenantID, id, "tablet", "", ChannelAPI)
	if err != nil || duplicate {
		t.Fatalf("second consumer ack = duplicate=%v err=%v", duplicate, err)
	}
	if second.Status != StatusConsumed {
		t.Fatalf("status = %s", second.Status)
	}
	if got := len(h.events(t, kernel.KindNotificationAck)); got != 2 {
		t.Fatalf("ack events = %d, want 2 (one per consumer)", got)
	}
	h.clock.Advance(11 * time.Second)
	if err := h.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(h.events(t, kernel.KindNotificationPushSent)); got != 0 {
		t.Fatalf("push events = %d, want 0 after consumption", got)
	}
}

func TestURLConsumptionIsSingleUse(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	view := h.push(t, ConsumerSpec{ID: "mail", Kind: ConsumerURL})
	raw := view.URLs["mail"]
	token := tokenFromURL(t, raw)
	id := uuid.MustParse(view.NotificationID)

	consumed, err := h.service.OpenURL(context.Background(), id, token, uuid.Nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if consumed.Status != StatusConsumed {
		t.Fatalf("status = %s", consumed.Status)
	}
	if _, err := h.service.OpenURL(context.Background(), id, token, uuid.Nil); !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("replay = %v, want ErrTokenUsed", err)
	}
	if _, err := h.service.OpenURL(context.Background(), id, "not-the-token", uuid.Nil); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("wrong token = %v, want ErrTokenInvalid", err)
	}
	if _, err := h.service.OpenURL(context.Background(), id, token, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign tenant hint = %v, want ErrNotFound", err)
	}
	if got := len(h.events(t, kernel.KindNotificationAck)); got != 1 {
		t.Fatalf("ack events = %d, want 1", got)
	}
}

// Any successful ack consumes the notification, so it must kill every
// consumer's capability URL, not only the one that answered, and drop the raw
// tokens this process still holds.
func TestAnyAckInvalidatesEveryCapabilityToken(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	ctx := context.Background()
	view := h.push(t,
		clientConsumer("web", true),
		ConsumerSpec{ID: "mail", Kind: ConsumerURL},
		ConsumerSpec{ID: "pixel", Kind: ConsumerImage},
	)
	id := uuid.MustParse(view.NotificationID)
	mailToken := tokenFromURL(t, view.URLs["mail"])
	pixelToken := tokenFromURL(t, view.URLs["pixel"])
	if h.service.tokenFor(view.NotificationID, "mail") == "" || h.service.tokenFor(view.NotificationID, "pixel") == "" {
		t.Fatal("the push did not keep the raw tokens in this process")
	}

	acked, duplicate, err := h.service.Ack(ctx, h.scope.TenantID, id, "web", "owner@example.test", ChannelAPI)
	if err != nil || duplicate {
		t.Fatalf("ack = duplicate=%v err=%v", duplicate, err)
	}
	if acked.Status != StatusConsumed {
		t.Fatalf("status = %s", acked.Status)
	}
	for _, consumer := range []string{"mail", "pixel"} {
		if token := h.service.tokenFor(view.NotificationID, consumer); token != "" {
			t.Fatalf("raw token of %q survived the ack", consumer)
		}
	}
	// The other consumers' URLs were minted against this notification; a
	// consumed notification invalidates them (410, never 404 for a real token).
	if _, err := h.service.OpenURL(ctx, id, mailToken, uuid.Nil); !errors.Is(err, ErrConsumed) {
		t.Fatalf("open with a non-acking token = %v, want ErrConsumed", err)
	}
	if _, err := h.service.TrackDelivery(ctx, id, pixelToken, uuid.Nil); !errors.Is(err, ErrConsumed) {
		t.Fatalf("pixel with a non-acking token = %v, want ErrConsumed", err)
	}
	// The ack itself stays idempotent.
	again, duplicate, err := h.service.Ack(ctx, h.scope.TenantID, id, "web", "owner@example.test", ChannelAPI)
	if err != nil || !duplicate || again.Status != StatusConsumed {
		t.Fatalf("duplicate ack = duplicate=%v status=%s err=%v", duplicate, again.Status, err)
	}
	if got := len(h.events(t, kernel.KindNotificationAck)); got != 1 {
		t.Fatalf("ack events = %d, want 1 (the repeat is not a second fact)", got)
	}
}

// A URL consumption is an ack too: opening one link closes every other link.
func TestURLConsumptionInvalidatesOtherTokens(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	ctx := context.Background()
	view := h.push(t,
		ConsumerSpec{ID: "mail", Kind: ConsumerURL},
		ConsumerSpec{ID: "backup", Kind: ConsumerURL},
		ConsumerSpec{ID: "pixel", Kind: ConsumerImage},
	)
	id := uuid.MustParse(view.NotificationID)
	backupToken := tokenFromURL(t, view.URLs["backup"])
	pixelToken := tokenFromURL(t, view.URLs["pixel"])
	if _, err := h.service.OpenURL(ctx, id, tokenFromURL(t, view.URLs["mail"]), uuid.Nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := h.service.OpenURL(ctx, id, backupToken, uuid.Nil); !errors.Is(err, ErrConsumed) {
		t.Fatalf("second link = %v, want ErrConsumed", err)
	}
	if _, err := h.service.TrackDelivery(ctx, id, pixelToken, uuid.Nil); !errors.Is(err, ErrConsumed) {
		t.Fatalf("pixel = %v, want ErrConsumed", err)
	}
	for _, consumer := range []string{"mail", "backup", "pixel"} {
		if token := h.service.tokenFor(view.NotificationID, consumer); token != "" {
			t.Fatalf("raw token of %q survived the consumption", consumer)
		}
	}
	if got := len(h.events(t, kernel.KindNotificationAck)); got != 1 {
		t.Fatalf("ack events = %d, want 1", got)
	}
}

func TestURLExpiry(t *testing.T) {
	h := newNotifyHarness(t, Options{URLTTL: 30 * time.Minute})
	view := h.push(t, ConsumerSpec{ID: "mail", Kind: ConsumerURL})
	token := tokenFromURL(t, view.URLs["mail"])
	h.clock.Advance(31 * time.Minute)
	if _, err := h.service.OpenURL(context.Background(), uuid.MustParse(view.NotificationID), token, uuid.Nil); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired token = %v, want ErrTokenExpired", err)
	}
}

func TestPixelDeliveryIsIdempotent(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	view := h.push(t, ConsumerSpec{ID: "pixel", Kind: ConsumerImage})
	token := tokenFromURL(t, view.URLs["pixel"])
	id := uuid.MustParse(view.NotificationID)
	first, err := h.service.TrackDelivery(context.Background(), id, token, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != StatusDelivered {
		t.Fatalf("status = %s", first.Status)
	}
	second, err := h.service.TrackDelivery(context.Background(), id, token, uuid.Nil)
	if err != nil || second.Status != StatusDelivered {
		t.Fatalf("second pixel = %+v err=%v", second, err)
	}
	if first.Consumers[0].DeliveredAt == nil || second.Consumers[0].AckedAt != nil {
		t.Fatalf("pixel state = %+v", second.Consumers[0])
	}
}

func TestAckRequiresDeclaredCapability(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	view := h.push(t, ConsumerSpec{ID: "pixel", Kind: ConsumerImage})
	if _, _, err := h.service.Ack(context.Background(), h.scope.TenantID,
		uuid.MustParse(view.NotificationID), "pixel", "", ChannelAPI); !errors.Is(err, ErrInvalid) {
		t.Fatalf("img ack = %v, want ErrInvalid", err)
	}
	declined := false
	view = h.push(t, ConsumerSpec{ID: "quiet", Kind: ConsumerClient, Online: true, Ack: &declined})
	if _, _, err := h.service.Ack(context.Background(), h.scope.TenantID,
		uuid.MustParse(view.NotificationID), "quiet", "", ChannelAPI); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ack:false consumer = %v, want ErrInvalid", err)
	}
}

func TestPushIsIdempotentByRequestID(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	request := PushRequest{
		TenantID:  h.scope.TenantID,
		SessionID: h.scope.SessionID,
		RequestID: "same-request",
		Text:      "once",
		Consumers: []ConsumerSpec{{ID: "web", Kind: ConsumerClient, Online: true, Ack: boolPtr(true)}},
	}
	first, err := h.service.Push(context.Background(), request)
	if err != nil || !first.Created {
		t.Fatalf("first push = created %v err %v", first.Created, err)
	}
	second, err := h.service.Push(context.Background(), request)
	if err != nil || second.Created || second.NotificationID != first.NotificationID {
		t.Fatalf("second push = %+v err=%v", second, err)
	}
	rows, err := h.service.List(context.Background(), h.scope.TenantID, &h.scope.SessionID, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = %d err=%v", len(rows), err)
	}
}

func TestRestartDoesNotRepush(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	view := h.push(t, ConsumerSpec{ID: "phone", Kind: ConsumerNative, Online: true})
	h.clock.Advance(11 * time.Second)
	if err := h.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.pusher.calls) != 1 {
		t.Fatalf("pusher calls = %d, want 1", len(h.pusher.calls))
	}
	// A successor process shares the durable store but not process memory.
	successor := NewService(Options{
		Store:     h.store,
		Committer: h.kernel,
		Pusher:    h.pusher,
		Now:       h.clock.Now,
	})
	h.clock.Advance(time.Hour)
	if err := successor.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.pusher.calls) != 1 {
		t.Fatalf("pusher calls after restart = %d, want 1", len(h.pusher.calls))
	}
	got, _ := successor.Get(context.Background(), h.scope.TenantID, uuid.MustParse(view.NotificationID))
	if got.Status != StatusDelivered {
		t.Fatalf("status after restart = %s", got.Status)
	}
}

func TestCrashBetweenClaimAndRecordIsHonest(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	view := h.push(t, ConsumerSpec{ID: "phone", Kind: ConsumerNative, Online: true})
	h.clock.Advance(11 * time.Second)
	// Simulate a process that claimed the push and died before recording it.
	if claimed, err := h.store.ClaimPush(context.Background(), h.scope.TenantID,
		uuid.MustParse(view.NotificationID), h.clock.Now()); err != nil || !claimed {
		t.Fatalf("claim = %v err=%v", claimed, err)
	}
	successor := NewService(Options{
		Store:     h.store,
		Committer: h.kernel,
		Pusher:    h.pusher,
		Now:       h.clock.Now,
	})
	if err := successor.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.pusher.calls) != 0 {
		t.Fatalf("pusher calls = %d, want 0 (never push twice)", len(h.pusher.calls))
	}
	got, _ := successor.Get(context.Background(), h.scope.TenantID, uuid.MustParse(view.NotificationID))
	if got.Push == nil || got.Push.Gap != "push_result_unknown" || !got.Push.Degraded {
		t.Fatalf("push = %+v", got.Push)
	}
	if got := len(h.events(t, kernel.KindNotificationPushSent)); got != 1 {
		t.Fatalf("push events = %d, want 1", got)
	}
}

func TestNotificationTenantIsolation(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	own := h.push(t, clientConsumer("web", true))
	otherTenant := uuid.New()
	otherSession := uuid.New()
	mem := kernel.NewMemStore()
	mem.CreateSession(otherTenant, otherSession)
	otherKernel := kernel.New(mem, kernel.Options{})
	otherService := NewService(Options{
		Store:     h.store,
		Committer: otherKernel,
		Pusher:    h.pusher,
		Now:       h.clock.Now,
	})
	foreign, err := otherService.Push(context.Background(), PushRequest{
		TenantID:  otherTenant,
		SessionID: otherSession,
		Text:      "foreign",
		Consumers: []ConsumerSpec{clientConsumer("web", true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := h.service.List(context.Background(), h.scope.TenantID, nil, 10)
	if err != nil || len(rows) != 1 || rows[0].NotificationID != own.NotificationID {
		t.Fatalf("own list = %+v err=%v", rows, err)
	}
	if _, err := h.service.Get(context.Background(), h.scope.TenantID, uuid.MustParse(foreign.NotificationID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign get = %v, want ErrNotFound", err)
	}
	if _, _, err := h.service.Ack(context.Background(), h.scope.TenantID, uuid.MustParse(foreign.NotificationID), "web", "", ChannelAPI); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign ack = %v, want ErrNotFound", err)
	}
}

func TestSweepWaitsForTheWindow(t *testing.T) {
	h := newNotifyHarness(t, Options{})
	h.push(t, ConsumerSpec{ID: "phone", Kind: ConsumerNative, Online: true})
	h.clock.Advance(9 * time.Second)
	if err := h.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.pusher.calls) != 0 {
		t.Fatalf("pusher calls before deadline = %d, want 0", len(h.pusher.calls))
	}
}

func tokenFromURL(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	token := parsed.Query().Get("token")
	if token == "" {
		t.Fatalf("url %q carries no token", raw)
	}
	if strings.Contains(raw, " ") {
		t.Fatalf("url %q carries whitespace", raw)
	}
	return token
}

func boolPtr(value bool) *bool { return &value }
