// Package notify owns notifications as durable facts (change keel-harness-v2,
// requirement "通知与审批是持久事实").
//
// A notification is a row plus two event kinds: `notification.ack` when any
// consumer consumes it and `notification.push_sent` when the harness falls back
// to one native/in-page push. The three-state truth table lives here:
//
//  1. a live, ack-capable consumer acks inside the window -> no push;
//  2. a live, ack-capable consumer does not ack inside the window -> one push;
//  3. no ack-capable live consumer -> one push.
//
// Every consumer has its own state and any ack consumes the notification. The
// push decision is claimed durably before the pusher runs, so a restart never
// pushes the same notification twice. URL consumption is a bearer capability:
// a 256-bit random token, stored only as a SHA-256 hash, single use, bounded by
// its own expiry. A pixel (`img`) consumer is delivery tracking, not
// consumption, and stays idempotent; once any ack consumes the notification,
// every capability URL (the pixel included) is refused.
package notify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// Notification lifecycle statuses.
const (
	StatusCreated   = "created"
	StatusDelivered = "delivered"
	StatusConsumed  = "consumed"
)

// Consumer kinds. `client` is a live client that can ack; `native` is a system
// notification target; `url` is a single-use consumption link; `img` is a
// delivery pixel.
const (
	ConsumerClient = "client"
	ConsumerNative = "native"
	ConsumerURL    = "url"
	ConsumerImage  = "img"
)

// Answer/ack channels recorded with the durable fact.
const (
	ChannelAPI  = "api"
	ChannelTool = "tool"
	ChannelURL  = "url"
	ChannelNone = "none"
)

// Push result classification. A degraded push is still recorded (once) so a
// restart cannot retry it; the gap names what could not be done instead of
// pretending the notification reached the operating system.
const (
	PushSent     = "sent"
	PushDegraded = "degraded"
)

// Error values. The HTTP layer maps them onto 400/404/410/409.
var (
	ErrNotFound     = errors.New("notify: notification not found")
	ErrInvalid      = errors.New("notify: invalid notification request")
	ErrTokenInvalid = errors.New("notify: the consumption token is invalid")
	ErrTokenExpired = errors.New("notify: the consumption token has expired")
	ErrTokenUsed    = errors.New("notify: the consumption token was already used")
	ErrConsumed     = errors.New("notify: the notification is already consumed")
	ErrNotWired     = errors.New("notify: no durable committer is wired")
)

const (
	notificationNamespace = "keel.notification.v1"
	// Bounds. A notification is a short human-facing notice, not a document.
	maxTitleLength = 200
	maxTextLength  = 4000
	maxConsumers   = 16
	maxConsumerID  = 64
	maxRequestID   = 128
	tokenBytes     = 32
)

// ConsumerSpec is one consumer declared by a push. Ack defaults to true for
// client/native consumers and false for url/img consumers.
type ConsumerSpec struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Label         string `json:"label,omitempty"`
	Online        bool   `json:"online,omitempty"`
	Ack           *bool  `json:"ack,omitempty"`
	ServiceWorker bool   `json:"serviceWorker,omitempty"`
}

// Consumer is the durable state of one declared consumer.
type Consumer struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	Label          string     `json:"label,omitempty"`
	Online         bool       `json:"online"`
	AckCapable     bool       `json:"ackCapable"`
	ServiceWorker  bool       `json:"serviceWorker,omitempty"`
	DeliveredAt    *time.Time `json:"deliveredAt,omitempty"`
	AckedAt        *time.Time `json:"ackedAt,omitempty"`
	AckedBy        string     `json:"ackedBy,omitempty"`
	AckChannel     string     `json:"ackChannel,omitempty"`
	TokenHash      string     `json:"tokenHash,omitempty"`
	TokenExpiresAt *time.Time `json:"tokenExpiresAt,omitempty"`
	TokenUsed      bool       `json:"tokenUsed,omitempty"`

	// token is the raw capability token. It lives only in the process that
	// minted it (the push response carries it) and is never persisted.
	token string
}

// PushRecord is the one fallback push of a notification.
type PushRecord struct {
	Status    string    `json:"status"` // sent | degraded
	Channel   string    `json:"channel"`
	Degraded  bool      `json:"degraded,omitempty"`
	Gap       string    `json:"gap,omitempty"`
	Consumers []string  `json:"consumers,omitempty"`
	At        time.Time `json:"at"`
}

// Notification is the durable row. TokenHash is never serialized to a client:
// Service methods return View, which carries no hashes.
type Notification struct {
	NotificationID  string      `json:"notificationId"`
	TenantID        string      `json:"tenantId"`
	SessionID       string      `json:"sessionId"`
	Title           string      `json:"title,omitempty"`
	Text            string      `json:"text"`
	Status          string      `json:"status"`
	CreatedAt       time.Time   `json:"createdAt"`
	Deadline        time.Time   `json:"deadline"`
	Consumers       []Consumer  `json:"consumers"`
	PushAttemptedAt *time.Time  `json:"pushAttemptedAt,omitempty"`
	Push            *PushRecord `json:"push,omitempty"`
	UpdatedAt       time.Time   `json:"updatedAt"`
}

// ConsumerView is the client-facing consumer state (no token hash).
type ConsumerView struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	Label          string     `json:"label,omitempty"`
	Online         bool       `json:"online"`
	AckCapable     bool       `json:"ackCapable"`
	ServiceWorker  bool       `json:"serviceWorker,omitempty"`
	DeliveredAt    *time.Time `json:"deliveredAt,omitempty"`
	AckedAt        *time.Time `json:"ackedAt,omitempty"`
	AckedBy        string     `json:"ackedBy,omitempty"`
	AckChannel     string     `json:"ackChannel,omitempty"`
	TokenExpiresAt *time.Time `json:"tokenExpiresAt,omitempty"`
	TokenUsed      bool       `json:"tokenUsed,omitempty"`
}

// View is the client-facing notification document.
type View struct {
	NotificationID string         `json:"notificationId"`
	SessionID      string         `json:"sessionId"`
	Title          string         `json:"title,omitempty"`
	Text           string         `json:"text"`
	Status         string         `json:"status"`
	CreatedAt      time.Time      `json:"createdAt"`
	Deadline       time.Time      `json:"deadline"`
	Consumers      []ConsumerView `json:"consumers"`
	Push           *PushRecord    `json:"push,omitempty"`
	// URLs is present only on the push that minted the tokens. The raw tokens
	// are not stored, so a retried push cannot reissue them.
	URLs         map[string]string `json:"urls,omitempty"`
	URLExpiresAt *time.Time        `json:"urlExpiresAt,omitempty"`
	Created      bool              `json:"created,omitempty"`
}

// PushRequest is one agent-initiated notification.
type PushRequest struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	RequestID string
	Title     string
	Text      string
	Consumers []ConsumerSpec
	Window    time.Duration
}

// Committer is the kernel's system-lane writer (satisfied by *kernel.Kernel).
type Committer interface {
	Commit(ctx context.Context, req kernel.CommitRequest) (kernel.Receipt, error)
}

// PushRequestInfo is what a Pusher receives.
type PushRequestInfo struct {
	NotificationID string
	TenantID       string
	SessionID      string
	Title          string
	Text           string
	Channel        string
	URL            string
	Consumers      []ConsumerView
}

// PushResult is what a Pusher reports. A nil Pusher (or a Pusher that could not
// deliver) becomes a degraded record with a named gap, never a fake success.
type PushResult struct {
	Channel  string
	Degraded bool
	Gap      string
}

// Pusher is the native/web push seam. The server has no operating-system
// notification channel of its own; deployments that have one inject it here.
type Pusher interface {
	Push(ctx context.Context, req PushRequestInfo) (PushResult, error)
}

// Options configures the service.
type Options struct {
	Store     Store
	Committer Committer
	Pusher    Pusher
	Logger    *slog.Logger
	Now       func() time.Time
	// DefaultWindow is the ack window before the one fallback push (30s).
	DefaultWindow time.Duration
	MaxWindow     time.Duration
	// URLTTL bounds how long a consumption URL stays valid (24h).
	URLTTL time.Duration
	// BaseURL prefixes generated links (KEEL_PUBLIC_BASE_URL). Empty keeps
	// them relative; a relative link is still a valid same-origin capability.
	BaseURL string
	// Random is the token entropy source (crypto/rand by default; tests may
	// inject a deterministic reader).
	Random io.Reader
}

// Store is the durable notification persistence seam.
type Store interface {
	// Create inserts a row unless the notification id exists. It returns the
	// stored row and whether this call inserted it.
	Create(ctx context.Context, row Notification) (Notification, bool, error)
	Get(ctx context.Context, tenantID, notificationID uuid.UUID) (Notification, error)
	// GetAny resolves a notification by id alone. It is the capability-URL
	// path: possession of the unguessable token, not a tenant header, is the
	// authority, and the row supplies the tenant it belongs to.
	GetAny(ctx context.Context, notificationID uuid.UUID) (Notification, error)
	// List returns a tenant's notifications, optionally filtered by session,
	// newest first.
	List(ctx context.Context, tenantID uuid.UUID, sessionID *uuid.UUID, limit int) ([]Notification, error)
	// ListDue returns notifications whose window passed without a push, plus
	// notifications claimed by a process that died before recording the result.
	ListDue(ctx context.Context, now time.Time, limit int) ([]Notification, error)
	Save(ctx context.Context, row Notification) error
	// ClaimPush marks the notification's single push attempt. The compare-and-
	// set is what makes "push once" survive concurrent sweepers and restarts.
	ClaimPush(ctx context.Context, tenantID, notificationID uuid.UUID, at time.Time) (bool, error)
}

// Service implements the notification primitive.
type Service struct {
	store     Store
	committer Committer
	pusher    Pusher
	log       *slog.Logger
	now       func() time.Time
	window    time.Duration
	maxWindow time.Duration
	urlTTL    time.Duration
	baseURL   string
	random    io.Reader
	locks     keyedMutex

	// tokens keeps the raw capability tokens of this process's notifications
	// so a fallback push can carry a click-through URL. The database stores
	// only the hash; a restarted process simply has no raw token to attach.
	tokensMu sync.Mutex
	tokens   map[string]map[string]string
}

// NewService builds the notification service.
func NewService(opts Options) *Service {
	if opts.Store == nil {
		opts.Store = NewMemoryStore()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.DefaultWindow <= 0 {
		opts.DefaultWindow = 30 * time.Second
	}
	if opts.MaxWindow <= 0 {
		opts.MaxWindow = 24 * time.Hour
	}
	if opts.URLTTL <= 0 {
		opts.URLTTL = 24 * time.Hour
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	return &Service{
		store:     opts.Store,
		committer: opts.Committer,
		pusher:    opts.Pusher,
		log:       opts.Logger,
		now:       opts.Now,
		window:    opts.DefaultWindow,
		maxWindow: opts.MaxWindow,
		urlTTL:    opts.URLTTL,
		baseURL:   strings.TrimRight(opts.BaseURL, "/"),
		random:    opts.Random,
		locks:     keyedMutex{locks: make(map[string]*lockEntry)},
		tokens:    make(map[string]map[string]string),
	}
}

// Push creates (idempotently) one notification and mints its consumption URLs.
func (s *Service) Push(ctx context.Context, req PushRequest) (View, error) {
	if req.TenantID == uuid.Nil || req.SessionID == uuid.Nil {
		return View{}, fmt.Errorf("%w: tenant and session are required", ErrInvalid)
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		return View{}, fmt.Errorf("%w: text is required", ErrInvalid)
	}
	if len(req.Text) > maxTextLength {
		return View{}, fmt.Errorf("%w: text is too long (max %d)", ErrInvalid, maxTextLength)
	}
	req.Title = strings.TrimSpace(req.Title)
	if len(req.Title) > maxTitleLength {
		return View{}, fmt.Errorf("%w: title is too long (max %d)", ErrInvalid, maxTitleLength)
	}
	if len(req.RequestID) > maxRequestID {
		return View{}, fmt.Errorf("%w: requestId is too long", ErrInvalid)
	}
	if len(req.Consumers) == 0 {
		return View{}, fmt.Errorf("%w: at least one consumer is required", ErrInvalid)
	}
	if len(req.Consumers) > maxConsumers {
		return View{}, fmt.Errorf("%w: at most %d consumers are allowed", ErrInvalid, maxConsumers)
	}
	window := req.Window
	if window <= 0 {
		window = s.window
	}
	if window > s.maxWindow {
		window = s.maxWindow
	}
	if window < time.Second {
		return View{}, fmt.Errorf("%w: the ack window must be at least one second", ErrInvalid)
	}
	consumers, err := s.buildConsumers(req.Consumers)
	if err != nil {
		return View{}, err
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		requestID = deriveRequestID(req.Text, req.Title, req.Consumers)
	}
	notificationID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(notificationNamespace+"|"+
		req.TenantID.String()+"|"+req.SessionID.String()+"|"+requestID))
	existing, err := s.store.Get(ctx, req.TenantID, notificationID)
	if err == nil {
		return existing.View(nil, false), nil
	}
	if !errors.Is(err, ErrNotFound) {
		return View{}, err
	}
	now := s.now()
	row := Notification{
		NotificationID: notificationID.String(),
		TenantID:       req.TenantID.String(),
		SessionID:      req.SessionID.String(),
		Title:          req.Title,
		Text:           req.Text,
		Status:         StatusCreated,
		CreatedAt:      now,
		Deadline:       now.Add(window),
		Consumers:      consumers,
		UpdatedAt:      now,
	}
	for i := range row.Consumers {
		if row.Consumers[i].Online && row.Consumers[i].Kind != ConsumerURL && row.Consumers[i].Kind != ConsumerImage {
			at := now
			row.Consumers[i].DeliveredAt = &at
			row.Status = StatusDelivered
		}
	}
	stored, created, err := s.store.Create(ctx, row)
	if err != nil {
		return View{}, err
	}
	if !created {
		// A concurrent push with the same request key won the insert. The raw
		// tokens live only in that writer's memory, so this retry cannot reissue
		// them; it returns the durable fact.
		return stored.View(nil, false), nil
	}
	urls := make(map[string]string)
	rawTokens := make(map[string]string)
	var urlExpiresAt *time.Time
	for i := range consumers {
		if consumers[i].Kind != ConsumerURL && consumers[i].Kind != ConsumerImage {
			continue
		}
		raw := consumers[i].token
		if raw == "" {
			continue
		}
		path := "/open"
		if consumers[i].Kind == ConsumerImage {
			path = "/pixel"
		}
		urls[consumers[i].ID] = s.baseURL + "/v1/notifications/" + notificationID.String() + path + "?token=" + raw
		rawTokens[consumers[i].ID] = raw
		if consumers[i].TokenExpiresAt != nil {
			at := *consumers[i].TokenExpiresAt
			urlExpiresAt = &at
		}
	}
	s.tokensMu.Lock()
	if len(rawTokens) > 0 {
		s.tokens[notificationID.String()] = rawTokens
	}
	s.tokensMu.Unlock()
	return stored.View(urls, true).withURLExpiry(urlExpiresAt), nil
}

// tokenFor returns this process's raw token for a consumer, if it still has
// one. A restarted process does not, so a deferred push carries no click URL.
func (s *Service) tokenFor(notificationID, consumerID string) string {
	s.tokensMu.Lock()
	defer s.tokensMu.Unlock()
	return s.tokens[notificationID][consumerID]
}

func (s *Service) forgetTokens(notificationID string) {
	s.tokensMu.Lock()
	delete(s.tokens, notificationID)
	s.tokensMu.Unlock()
}

// List returns the tenant's notifications, evaluating due pushes first.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, sessionID *uuid.UUID, limit int) ([]View, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	s.sweepBestEffort(ctx)
	rows, err := s.store.List(ctx, tenantID, sessionID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.View(nil, false))
	}
	return out, nil
}

// Get returns one notification of the tenant.
func (s *Service) Get(ctx context.Context, tenantID, notificationID uuid.UUID) (View, error) {
	s.sweepBestEffort(ctx)
	row, err := s.store.Get(ctx, tenantID, notificationID)
	if err != nil {
		return View{}, err
	}
	return row.View(nil, false), nil
}

// Ack records one consumer's consumption idempotently. Any ack consumes the
// notification; the ack event is written before the row changes, so a durable
// fact cannot exist without its row update being retryable.
func (s *Service) Ack(ctx context.Context, tenantID, notificationID uuid.UUID, consumerID, actor, channel string) (View, bool, error) {
	if strings.TrimSpace(consumerID) == "" {
		return View{}, false, fmt.Errorf("%w: consumerId is required", ErrInvalid)
	}
	unlock := s.locks.Lock(notificationID.String())
	defer unlock()
	row, err := s.store.Get(ctx, tenantID, notificationID)
	if err != nil {
		return View{}, false, err
	}
	consumer, found := findConsumer(row.Consumers, consumerID)
	if !found {
		return View{}, false, fmt.Errorf("%w: consumer %q is not declared on this notification", ErrInvalid, consumerID)
	}
	if !consumer.AckCapable {
		return View{}, false, fmt.Errorf("%w: consumer %q did not declare ack capability", ErrInvalid, consumerID)
	}
	if actor == "" {
		actor = "unknown"
	}
	if channel == "" {
		channel = ChannelAPI
	}
	now := s.now()
	if consumer.AckedAt != nil {
		// The durable fact already exists; do not re-commit it with a new
		// timestamp (the kernel's key dedupe is content-sensitive). The
		// notification is consumed, so no raw token may stay in this process.
		s.forgetTokens(row.NotificationID)
		return row.View(nil, false), true, nil
	}
	if err := s.commitAck(ctx, row, consumer, actor, channel, now); err != nil {
		return View{}, false, err
	}
	at := now
	for i := range row.Consumers {
		if row.Consumers[i].ID != consumerID || row.Consumers[i].AckedAt != nil {
			continue
		}
		row.Consumers[i].AckedAt = &at
		row.Consumers[i].AckedBy = actor
		row.Consumers[i].AckChannel = channel
	}
	row.Status = StatusConsumed
	row.UpdatedAt = now
	if err := s.store.Save(ctx, row); err != nil {
		return View{}, false, err
	}
	// Any ack consumes the notification, so every consumer's capability URL is
	// dead from here on: OpenURL/TrackDelivery refuse a consumed row, and the
	// raw tokens (which only this process holds) are dropped now rather than
	// lingering until a push finishes.
	s.forgetTokens(row.NotificationID)
	return row.View(nil, false), false, nil
}

// OpenURL consumes a single-use capability URL. A replayed, expired or wrong
// token is an error distinct from "not found"; the tenant hint (when supplied)
// is defence in depth on top of the unguessable token.
func (s *Service) OpenURL(ctx context.Context, notificationID uuid.UUID, token string, tenantHint uuid.UUID) (View, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return View{}, ErrTokenInvalid
	}
	unlock := s.locks.Lock(notificationID.String())
	defer unlock()
	row, err := s.store.GetAny(ctx, notificationID)
	if err != nil {
		return View{}, err
	}
	if tenantHint != uuid.Nil && row.TenantID != tenantHint.String() {
		return View{}, ErrNotFound
	}
	consumer, found := findTokenConsumer(row.Consumers, token, ConsumerURL)
	if !found {
		return View{}, ErrTokenInvalid
	}
	if consumer.TokenUsed || consumer.AckedAt != nil {
		return View{}, ErrTokenUsed
	}
	// The token is real but another consumer already consumed the notification:
	// any ack consumes it, so this capability is dead too (410, not 404 only
	// after the token proved valid).
	if row.Status == StatusConsumed {
		return View{}, ErrConsumed
	}
	now := s.now()
	if consumer.TokenExpiresAt == nil || !now.Before(*consumer.TokenExpiresAt) {
		return View{}, ErrTokenExpired
	}
	actor := "url-token:" + consumer.ID
	if err := s.commitAck(ctx, row, consumer, actor, ChannelURL, now); err != nil {
		return View{}, err
	}
	at := now
	for i := range row.Consumers {
		if row.Consumers[i].ID != consumer.ID {
			continue
		}
		row.Consumers[i].TokenUsed = true
		row.Consumers[i].AckedAt = &at
		row.Consumers[i].AckedBy = actor
		row.Consumers[i].AckChannel = ChannelURL
	}
	row.Status = StatusConsumed
	row.UpdatedAt = now
	if err := s.store.Save(ctx, row); err != nil {
		return View{}, err
	}
	// This consumption also invalidates every other consumer's URL; the raw
	// tokens live only in this process, so drop them with the row consumed.
	s.forgetTokens(row.NotificationID)
	return row.View(nil, false), nil
}

// TrackDelivery records an `img` pixel fetch. It is idempotent and never
// consumes the notification.
func (s *Service) TrackDelivery(ctx context.Context, notificationID uuid.UUID, token string, tenantHint uuid.UUID) (View, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return View{}, ErrTokenInvalid
	}
	unlock := s.locks.Lock(notificationID.String())
	defer unlock()
	row, err := s.store.GetAny(ctx, notificationID)
	if err != nil {
		return View{}, err
	}
	if tenantHint != uuid.Nil && row.TenantID != tenantHint.String() {
		return View{}, ErrNotFound
	}
	consumer, found := findTokenConsumer(row.Consumers, token, ConsumerImage)
	if !found {
		return View{}, ErrTokenInvalid
	}
	now := s.now()
	if consumer.TokenExpiresAt == nil || !now.Before(*consumer.TokenExpiresAt) {
		return View{}, ErrTokenExpired
	}
	// A pixel on a consumed notification is dead: any ack (including another
	// consumer's) consumes the notification and invalidates every capability.
	if row.Status == StatusConsumed {
		return View{}, ErrConsumed
	}
	if consumer.DeliveredAt != nil {
		return row.View(nil, false), nil
	}
	at := now
	for i := range row.Consumers {
		if row.Consumers[i].ID != consumer.ID {
			continue
		}
		row.Consumers[i].DeliveredAt = &at
	}
	if row.Status == StatusCreated {
		row.Status = StatusDelivered
	}
	row.UpdatedAt = now
	if err := s.store.Save(ctx, row); err != nil {
		return View{}, err
	}
	return row.View(nil, false), nil
}

// Sweep evaluates every due notification once. It is exported so a test (and a
// host without the plugin-managed ticker) can drive the window deterministically.
func (s *Service) Sweep(ctx context.Context) error {
	rows, err := s.store.ListDue(ctx, s.now(), 64)
	if err != nil {
		return err
	}
	var firstErr error
	for _, row := range rows {
		if err := s.evaluate(ctx, row); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Run is the plugin-managed sweep loop. It returns when ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.log.Warn("notify sweep failed", "err", err)
			}
		}
	}
}

func (s *Service) evaluate(ctx context.Context, row Notification) error {
	unlock := s.locks.Lock(row.NotificationID)
	defer unlock()
	current, err := s.store.GetAny(ctx, uuid.MustParse(row.NotificationID))
	if err != nil {
		return err
	}
	if current.Status == StatusConsumed || current.Push != nil {
		return nil
	}
	now := s.now()
	claimed := false
	if current.PushAttemptedAt == nil {
		if now.Before(current.Deadline) {
			return nil
		}
		ok, err := s.store.ClaimPush(ctx, uuid.MustParse(current.TenantID), uuid.MustParse(current.NotificationID), now)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		current.PushAttemptedAt = &now
		claimed = true
	}
	return s.deliverPush(ctx, current, claimed)
}

// deliverPush runs after the durable claim. A claimed row with no record is a
// process that died mid-push: it gets an explicit "unknown" gap instead of a
// second push.
func (s *Service) deliverPush(ctx context.Context, row Notification, claimed bool) error {
	now := s.now()
	record := PushRecord{At: now, Status: PushSent}
	if !claimed && row.Push == nil {
		record.Status = PushDegraded
		record.Degraded = true
		record.Channel = ChannelNone
		record.Gap = "push_result_unknown"
		return s.finishPush(ctx, row, record)
	}
	channel, gap, targets := s.pushPlan(row)
	record.Channel = channel
	record.Consumers = targets
	if gap != "" {
		record.Status = PushDegraded
		record.Degraded = true
		record.Gap = gap
	} else if s.pusher == nil {
		record.Status = PushDegraded
		record.Degraded = true
		record.Gap = "pusher_unavailable"
	} else {
		var url string
		for _, consumer := range row.Consumers {
			if consumer.Kind != ConsumerURL || consumer.TokenHash == "" {
				continue
			}
			url = s.baseURL + "/v1/notifications/" + row.NotificationID + "/open"
			if raw := s.tokenFor(row.NotificationID, consumer.ID); raw != "" {
				url += "?token=" + raw
			}
			break
		}
		result, err := s.pusher.Push(ctx, PushRequestInfo{
			NotificationID: row.NotificationID,
			TenantID:       row.TenantID,
			SessionID:      row.SessionID,
			Title:          row.Title,
			Text:           row.Text,
			Channel:        channel,
			URL:            url,
			Consumers:      consumerViews(row.Consumers),
		})
		if err != nil {
			record.Status = PushDegraded
			record.Degraded = true
			record.Gap = "push_failed"
		} else {
			if result.Channel != "" {
				record.Channel = result.Channel
			}
			if result.Degraded || result.Gap != "" {
				record.Status = PushDegraded
				record.Degraded = true
				record.Gap = result.Gap
				if record.Gap == "" {
					record.Gap = "degraded"
				}
			}
		}
	}
	return s.finishPush(ctx, row, record)
}

func (s *Service) finishPush(ctx context.Context, row Notification, record PushRecord) error {
	payload, err := json.Marshal(map[string]any{
		"notificationId": row.NotificationID,
		"channel":        record.Channel,
		"degraded":       record.Degraded,
		"gap":            record.Gap,
		"status":         record.Status,
		"consumers":      record.Consumers,
		"at":             record.At,
	})
	if err != nil {
		return fmt.Errorf("notify: encode push fact: %w", err)
	}
	if err := s.commit(ctx, kernel.CommitRequest{
		Scope:               kernel.Scope{TenantID: uuid.MustParse(row.TenantID), SessionID: uuid.MustParse(row.SessionID)},
		Kind:                kernel.KindNotificationPushSent,
		System:              true,
		Payload:             payload,
		NotificationPushKey: row.NotificationID,
	}); err != nil {
		return err
	}
	row.Push = &record
	if row.Status == StatusCreated {
		row.Status = StatusDelivered
	}
	row.UpdatedAt = s.now()
	if err := s.store.Save(ctx, row); err != nil {
		return err
	}
	s.forgetTokens(row.NotificationID)
	return nil
}

// pushPlan names the fallback channel and, when the harness cannot honour it,
// the gap that must be registered instead of pretending a push happened.
func (s *Service) pushPlan(row Notification) (channel, gap string, targets []string) {
	hasNative := false
	hasClient := false
	hasServiceWorker := false
	for _, consumer := range row.Consumers {
		switch consumer.Kind {
		case ConsumerNative:
			hasNative = true
			targets = append(targets, consumer.ID)
		case ConsumerClient:
			hasClient = true
			if consumer.ServiceWorker {
				hasServiceWorker = true
			}
			targets = append(targets, consumer.ID)
		}
	}
	switch {
	case hasNative:
		return ConsumerNative, "", targets
	case hasClient && hasServiceWorker:
		return "web", "", targets
	case hasClient:
		// Explicit degradation the spec requires: no service worker means the
		// fallback is an in-page notice, and the gap is registered.
		return "in_page", "web_service_worker_unavailable", targets
	default:
		return ChannelNone, "no_push_target", targets
	}
}

func (s *Service) commitAck(ctx context.Context, row Notification, consumer Consumer, actor, channel string, at time.Time) error {
	// The payload is stable per (notification, consumer): a retry must dedupe
	// to the same fact. The actor/channel/instant live in the durable row.
	payload, err := json.Marshal(map[string]any{
		"notificationId": row.NotificationID,
		"consumerId":     consumer.ID,
		"consumerKind":   consumer.Kind,
	})
	if err != nil {
		return fmt.Errorf("notify: encode ack fact: %w", err)
	}
	return s.commit(ctx, kernel.CommitRequest{
		Scope:              kernel.Scope{TenantID: uuid.MustParse(row.TenantID), SessionID: uuid.MustParse(row.SessionID)},
		Kind:               kernel.KindNotificationAck,
		System:             true,
		Payload:            payload,
		NotificationAckKey: row.NotificationID + ":" + consumer.ID,
	})
}

func (s *Service) commit(ctx context.Context, req kernel.CommitRequest) error {
	if s.committer == nil {
		return ErrNotWired
	}
	if _, err := s.committer.Commit(ctx, req); err != nil {
		return fmt.Errorf("notify: commit %s: %w", req.Kind, err)
	}
	return nil
}

func (s *Service) sweepBestEffort(ctx context.Context) {
	if err := s.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.log.Warn("notify sweep failed", "err", err)
	}
}

func (s *Service) buildConsumers(specs []ConsumerSpec) ([]Consumer, error) {
	out := make([]Consumer, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		id := strings.TrimSpace(spec.ID)
		if id == "" {
			return nil, fmt.Errorf("%w: every consumer needs an id", ErrInvalid)
		}
		if len(id) > maxConsumerID {
			return nil, fmt.Errorf("%w: consumer id %q is too long", ErrInvalid, id)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("%w: duplicate consumer id %q", ErrInvalid, id)
		}
		seen[id] = struct{}{}
		kind := strings.TrimSpace(spec.Kind)
		switch kind {
		case ConsumerClient, ConsumerNative, ConsumerURL, ConsumerImage:
		case "":
			kind = ConsumerClient
		default:
			return nil, fmt.Errorf("%w: unknown consumer kind %q", ErrInvalid, kind)
		}
		ack := kind == ConsumerClient || kind == ConsumerNative
		if spec.Ack != nil {
			ack = *spec.Ack
		}
		online := spec.Online && kind != ConsumerURL && kind != ConsumerImage
		consumer := Consumer{
			ID:            id,
			Kind:          kind,
			Label:         strings.TrimSpace(spec.Label),
			Online:        online,
			AckCapable:    ack,
			ServiceWorker: spec.ServiceWorker && kind == ConsumerClient,
		}
		if kind == ConsumerURL || kind == ConsumerImage {
			raw, hash, err := s.mintToken()
			if err != nil {
				return nil, err
			}
			consumer.token = raw
			consumer.TokenHash = hash
			expires := s.now().Add(s.urlTTL)
			consumer.TokenExpiresAt = &expires
			if kind == ConsumerURL {
				consumer.AckCapable = true
			}
		}
		out = append(out, consumer)
	}
	return out, nil
}

func (s *Service) mintToken() (string, string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := io.ReadFull(s.random, raw); err != nil {
		return "", "", fmt.Errorf("notify: mint consumption token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, hashToken(token), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func findConsumer(consumers []Consumer, id string) (Consumer, bool) {
	for _, consumer := range consumers {
		if consumer.ID == id {
			return consumer, true
		}
	}
	return Consumer{}, false
}

func findTokenConsumer(consumers []Consumer, token, kind string) (Consumer, bool) {
	hash := hashToken(token)
	for _, consumer := range consumers {
		if consumer.Kind != kind || consumer.TokenHash == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(consumer.TokenHash), []byte(hash)) == 1 {
			return consumer, true
		}
	}
	return Consumer{}, false
}

func deriveRequestID(text, title string, consumers []ConsumerSpec) string {
	ids := make([]string, 0, len(consumers))
	for _, consumer := range consumers {
		ids = append(ids, consumer.ID+":"+consumer.Kind)
	}
	sum := sha256.Sum256([]byte(title + "\x00" + text + "\x00" + strings.Join(ids, "\x00")))
	return "auto-" + hex.EncodeToString(sum[:12])
}

func consumerViews(consumers []Consumer) []ConsumerView {
	out := make([]ConsumerView, 0, len(consumers))
	for _, consumer := range consumers {
		out = append(out, ConsumerView{
			ID:             consumer.ID,
			Kind:           consumer.Kind,
			Label:          consumer.Label,
			Online:         consumer.Online,
			AckCapable:     consumer.AckCapable,
			ServiceWorker:  consumer.ServiceWorker,
			DeliveredAt:    consumer.DeliveredAt,
			AckedAt:        consumer.AckedAt,
			AckedBy:        consumer.AckedBy,
			AckChannel:     consumer.AckChannel,
			TokenExpiresAt: consumer.TokenExpiresAt,
			TokenUsed:      consumer.TokenUsed,
		})
	}
	return out
}

// View renders the client-facing document.
func (n Notification) View(urls map[string]string, created bool) View {
	return View{
		NotificationID: n.NotificationID,
		SessionID:      n.SessionID,
		Title:          n.Title,
		Text:           n.Text,
		Status:         n.Status,
		CreatedAt:      n.CreatedAt,
		Deadline:       n.Deadline,
		Consumers:      consumerViews(n.Consumers),
		Push:           n.Push,
		URLs:           urls,
		Created:        created,
	}
}

func (v View) withURLExpiry(at *time.Time) View {
	v.URLExpiresAt = at
	return v
}

// Gateway-specific helpers ------------------------------------------------

// keyedMutex serializes work per notification without one global lock.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	entry := k.locks[key]
	if entry == nil {
		entry = &lockEntry{}
		k.locks[key] = entry
	}
	entry.refs++
	k.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
