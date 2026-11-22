// Package approval owns the durable audit trail of the `approve` aggregate
// tool (change keel-harness-v2, requirement "通知与审批是持久事实").
//
// The decision lifecycle itself is the v1 decision primitive
// (internal/harnessext/decisions.go): a durable row with an answer journal, an
// idempotent answer and a persisted deadline. The decision broker knows *what*
// was answered; it does not know on whose authority. This package records that
// second fact — the provenance — as its own durable row:
//
//   - who answered (the X-Keel-Actor credential context, or "unknown" when the
//     client sent none — never invented). X-Keel-Actor is tenant-asserted
//     advisory attribution, not an authorization fact: any holder of the tenant
//     credential can set it, so audit UIs and policy must never treat the
//     recorded actor as a cryptographic identity;
//   - from which channel (the HTTP decisions API, a URL consumption, or the
//     tool noticing an answer that arrived through an older path);
//   - whether the answer stayed inside the declared answer space
//     (`validated: true`) or took the "other" escape (`validated: false`);
//   - the lifecycle instants (opened, delivered, answered/timed_out/cancelled).
//
// The row is a projection for audit; the event-ized truth of the decision is
// still the decision journal. It exists so downstream code can distinguish a
// confirmed option from a free-text escape without re-deriving it, and so an
// operator can answer "who approved this, and when" without correlating logs.
package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Approval modes, matching the `approve` tool's closed action set.
const (
	ModeConfirm = "confirm"
	ModePrompt  = "prompt"
	ModeChoice  = "choice"
)

// Decision kinds the approve plugin opens. The prefix is what lets the HTTP
// decisions route recognize an approval question (and record provenance) while
// other question kinds (clarify, plan) stay untouched.
const (
	KindPrefix       = "approval."
	KindConfirm      = KindPrefix + ModeConfirm
	KindPrompt       = KindPrefix + ModePrompt
	KindChoice       = KindPrefix + ModeChoice
	kindNamespace    = "keel.approval.v1"
	maxActorLength   = 256
	maxContextFields = 32
)

// Lifecycle statuses of one provenance row.
const (
	StatusOpen      = "open"
	StatusAnswered  = "answered"
	StatusTimedOut  = "timed_out"
	StatusCancelled = "cancelled"
)

// Answer channels. They are recorded, never inferred from the actor.
const (
	ChannelHTTP    = "http"
	ChannelURL     = "url"
	ChannelTool    = "tool"
	ChannelTimeout = "timeout"
	ChannelCancel  = "cancel"
	ChannelUnknown = "unknown"
)

// Errors of the provenance store. The HTTP layer maps them onto 404/409/400.
var (
	ErrNotFound = errors.New("approval: provenance not found")
	ErrConflict = errors.New("approval: a different answer is already recorded")
	ErrClosed   = errors.New("approval: the approval is already terminal")
	ErrInvalid  = errors.New("approval: invalid provenance")
)

// Record is the durable provenance of one approval question.
type Record struct {
	TenantID   string `json:"tenantId"`
	SessionID  string `json:"sessionId"`
	DecisionID string `json:"decisionId"`
	Kind       string `json:"kind"`
	Mode       string `json:"mode"`
	Status     string `json:"status"`
	OptionID   string `json:"optionId,omitempty"`
	Text       string `json:"text,omitempty"`
	// Validated is true when the answer stayed inside the declared answer
	// space. The "other" escape is recorded with validated=false and
	// escape=true so downstream code can refuse to treat it as a selection.
	Validated bool `json:"validated"`
	Escape    bool `json:"escape,omitempty"`
	// Actor is the credential context the answer arrived with. An absent actor
	// is recorded verbatim as UnknownActor; it is never fabricated.
	Actor   string `json:"actor"`
	Channel string `json:"channel"`
	// Context is bounded request context (request id, remote address, user
	// agent). It never carries credentials.
	Context     map[string]string `json:"context,omitempty"`
	OpenedAt    *time.Time        `json:"openedAt,omitempty"`
	DeliveredAt *time.Time        `json:"deliveredAt,omitempty"`
	AnsweredAt  *time.Time        `json:"answeredAt,omitempty"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

// UnknownActor is the honest marker of an answer that arrived without an actor
// credential. It is a value, not a missing field: the audit must be able to
// tell "nobody identified themselves" from "the record was never written".
const UnknownActor = "unknown"

// Store persists provenance rows. Two implementations exist:
// MemoryStore (tests) and PostgresStore (production).
type Store interface {
	// Create inserts a row unless the same decision already has one. It
	// returns the stored row and whether this call inserted it.
	Create(ctx context.Context, row Record) (Record, bool, error)
	Get(ctx context.Context, tenantID, sessionID, decisionID uuid.UUID) (Record, error)
	List(ctx context.Context, tenantID, sessionID uuid.UUID) ([]Record, error)
	Save(ctx context.Context, row Record) error
}

// Options configures the service.
type Options struct {
	Store  Store
	Logger *slog.Logger
	Now    func() time.Time
}

// Service records and reads approval provenance.
type Service struct {
	store Store
	log   *slog.Logger
	now   func() time.Time
	locks keyedMutex
}

// NewService builds the provenance service. A nil store keeps the service
// available in memory (degraded: a restart loses provenance, not the decision).
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
	return &Service{store: opts.Store, log: opts.Logger, now: opts.Now, locks: keyedMutex{locks: make(map[string]*lockEntry)}}
}

// Mode derives the approve mode from a decision kind.
func Mode(kind string) string {
	return strings.TrimPrefix(kind, KindPrefix)
}

// Compute decides whether an answer is validated and whether it is the "other"
// escape:
//
//   - an option id that is declared in the question is a validated selection;
//   - free text for a prompt is the declared answer space (validated);
//   - free text for a choice is the "other" escape (not validated);
//   - free text for a confirm is not a declared answer (not validated).
func Compute(mode string, optionIDs []string, optionID, text string) (validated, escape bool) {
	optionID = strings.TrimSpace(optionID)
	text = strings.TrimSpace(text)
	if optionID != "" {
		for _, id := range optionIDs {
			if id == optionID {
				return true, false
			}
		}
		return false, false
	}
	if text == "" {
		return false, false
	}
	switch mode {
	case ModePrompt:
		return true, false
	case ModeChoice:
		return false, true
	default:
		return false, false
	}
}

// Open records that a question was opened and delivered to the client. The
// call is idempotent: an existing row (whatever its state) wins.
func (s *Service) Open(ctx context.Context, row Record) (Record, bool, error) {
	if err := validateIDs(row); err != nil {
		return Record{}, false, err
	}
	now := s.now()
	row.Status = StatusOpen
	row.OpenedAt = timePtr(now)
	row.DeliveredAt = timePtr(now)
	row.UpdatedAt = now
	if row.Actor == "" {
		row.Actor = UnknownActor
	}
	if row.Channel == "" {
		row.Channel = ChannelTool
	}
	row.Context = boundedContext(row.Context)
	row.Actor = boundedActor(row.Actor)
	stored, created, err := s.store.Create(ctx, row)
	if err != nil {
		return Record{}, false, err
	}
	return stored, created, nil
}

// RecordAnswer records a human answer idempotently. The second result reports
// that this exact answer was already recorded.
func (s *Service) RecordAnswer(ctx context.Context, row Record) (Record, bool, error) {
	if err := validateIDs(row); err != nil {
		return Record{}, false, err
	}
	row.Status = StatusAnswered
	if row.Actor == "" {
		row.Actor = UnknownActor
	}
	if row.Channel == "" {
		row.Channel = ChannelUnknown
	}
	row.Context = boundedContext(row.Context)
	row.Actor = boundedActor(row.Actor)
	unlock := s.locks.Lock(row.TenantID + ":" + row.SessionID + ":" + row.DecisionID)
	defer unlock()
	now := s.now()
	row.UpdatedAt = now
	if row.AnsweredAt == nil {
		row.AnsweredAt = timePtr(now)
	}
	if row.DeliveredAt == nil {
		row.DeliveredAt = row.OpenedAt
	}
	existing, created, err := s.store.Create(ctx, row)
	if err != nil {
		return Record{}, false, err
	}
	if !created {
		switch existing.Status {
		case StatusAnswered:
			if existing.OptionID != row.OptionID || existing.Text != row.Text {
				return existing, false, ErrConflict
			}
			// The same answer. An answer that the tool observed first records
			// actor "unknown"; the HTTP answer path (which knows the actor) may
			// arrive a moment later. Upgrade the provenance instead of keeping
			// the weaker record — never downgrade a known actor.
			if existing.Actor != UnknownActor || row.Actor == UnknownActor {
				return existing, true, nil
			}
			if existing.OpenedAt != nil {
				row.OpenedAt = existing.OpenedAt
			}
			if existing.DeliveredAt != nil {
				row.DeliveredAt = existing.DeliveredAt
			}
			if existing.AnsweredAt != nil {
				row.AnsweredAt = existing.AnsweredAt
			}
			row.UpdatedAt = now
			if err := s.store.Save(ctx, row); err != nil {
				return Record{}, false, err
			}
			return row, true, nil
		case StatusTimedOut, StatusCancelled:
			return existing, false, ErrClosed
		}
		// The row exists in the open state: apply this answer.
		if existing.OpenedAt != nil {
			row.OpenedAt = existing.OpenedAt
		}
		if existing.DeliveredAt != nil {
			row.DeliveredAt = existing.DeliveredAt
		}
		if err := s.store.Save(ctx, row); err != nil {
			return Record{}, false, err
		}
	}
	return row, false, nil
}

// RecordOutcome records a terminal non-answer (timed_out or cancelled). It is
// idempotent and refuses to overwrite a human answer.
func (s *Service) RecordOutcome(ctx context.Context, row Record) (Record, bool, error) {
	if row.Status != StatusTimedOut && row.Status != StatusCancelled {
		return Record{}, false, fmt.Errorf("%w: outcome status must be timed_out or cancelled", ErrInvalid)
	}
	if err := validateIDs(row); err != nil {
		return Record{}, false, err
	}
	if row.Actor == "" {
		row.Actor = "system"
	}
	if row.Channel == "" {
		switch row.Status {
		case StatusTimedOut:
			row.Channel = ChannelTimeout
		case StatusCancelled:
			row.Channel = ChannelCancel
		}
	}
	row.Context = boundedContext(row.Context)
	row.Actor = boundedActor(row.Actor)
	unlock := s.locks.Lock(row.TenantID + ":" + row.SessionID + ":" + row.DecisionID)
	defer unlock()
	now := s.now()
	row.UpdatedAt = now
	existing, created, err := s.store.Create(ctx, row)
	if err != nil {
		return Record{}, false, err
	}
	if !created {
		switch existing.Status {
		case row.Status:
			return existing, true, nil
		case StatusAnswered:
			return existing, false, ErrClosed
		}
		if existing.OpenedAt != nil {
			row.OpenedAt = existing.OpenedAt
		}
		if existing.DeliveredAt != nil {
			row.DeliveredAt = existing.DeliveredAt
		}
		if err := s.store.Save(ctx, row); err != nil {
			return Record{}, false, err
		}
	}
	return row, false, nil
}

// Get reads one provenance row; the second result reports existence.
func (s *Service) Get(ctx context.Context, tenantID, sessionID, decisionID uuid.UUID) (Record, bool, error) {
	row, err := s.store.Get(ctx, tenantID, sessionID, decisionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	return row, true, nil
}

// List returns the session's provenance rows, newest first.
func (s *Service) List(ctx context.Context, tenantID, sessionID uuid.UUID) ([]Record, error) {
	return s.store.List(ctx, tenantID, sessionID)
}

// Backfill builds a provenance record for an answer observed through a path
// that did not record one (an older client answering the plain decisions API,
// or a decision answered before this service existed). It is honest about the
// missing context: actor "unknown", channel "unknown".
func Backfill(kind string, optionIDs []string, optionID, text, actor, channel string, openedAt *time.Time, answeredAt time.Time) Record {
	mode := Mode(kind)
	validated, escape := Compute(mode, optionIDs, optionID, text)
	if actor == "" {
		actor = UnknownActor
	}
	if channel == "" {
		channel = ChannelUnknown
	}
	return Record{
		Kind:        kind,
		Mode:        mode,
		Status:      StatusAnswered,
		OptionID:    optionID,
		Text:        text,
		Validated:   validated,
		Escape:      escape,
		Actor:       actor,
		Channel:     channel,
		OpenedAt:    openedAt,
		DeliveredAt: openedAt,
		AnsweredAt:  &answeredAt,
	}
}

// EncodeContext renders a bounded context map for the storage column.
func EncodeContext(context map[string]string) json.RawMessage {
	if len(context) == 0 {
		return nil
	}
	raw, err := json.Marshal(boundedContext(context))
	if err != nil {
		return nil
	}
	return raw
}

// DecodeContext reads the storage column.
func DecodeContext(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func validateIDs(row Record) error {
	if _, err := uuid.Parse(row.TenantID); err != nil {
		return fmt.Errorf("%w: tenantId must be a UUID", ErrInvalid)
	}
	if _, err := uuid.Parse(row.SessionID); err != nil {
		return fmt.Errorf("%w: sessionId must be a UUID", ErrInvalid)
	}
	if _, err := uuid.Parse(row.DecisionID); err != nil {
		return fmt.Errorf("%w: decisionId must be a UUID", ErrInvalid)
	}
	return nil
}

func boundedActor(actor string) string {
	actor = strings.TrimSpace(actor)
	if len(actor) > maxActorLength {
		actor = actor[:maxActorLength]
	}
	return actor
}

// boundedContext caps the audit context so a client cannot balloon a row.
func boundedContext(context map[string]string) map[string]string {
	if len(context) == 0 {
		return nil
	}
	out := make(map[string]string, len(context))
	keys := 0
	for key, value := range context {
		if keys >= maxContextFields {
			break
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if len(value) > 512 {
			value = value[:512]
		}
		out[key] = value
		keys++
	}
	return out
}

func timePtr(t time.Time) *time.Time { return &t }

// keyedMutex serializes transitions of one decision without one global lock.
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
