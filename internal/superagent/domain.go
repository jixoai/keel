package superagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

// DefaultModeID is the collaboration mode this package implements. The mode is
// a role contract (orchestrator/worker), never a Bot kind.
const DefaultModeID = collab.ModeSuperagent

// Audit event kinds written by the orchestrator. They are structured
// (kind + result + ids); no row contains source bodies or credentials.
const (
	AuditEventSeen       = "collab.superagent.event_seen"
	AuditEventIgnored    = "collab.superagent.event_ignored"
	AuditTicketAccepted  = "collab.superagent.ticket_accepted"
	AuditTicketAttached  = "collab.superagent.event_attached"
	AuditTicketStopped   = "collab.superagent.ticket_stopped"
	AuditTicketResumed   = "collab.superagent.ticket_resumed"
	AuditDispatch        = "collab.superagent.dispatch"
	AuditDispatchFailed  = "collab.superagent.dispatch_failed"
	AuditRecovered       = "collab.superagent.assignment_recovered"
	AuditAttentionAnswer = "collab.superagent.attention_answered"
	AuditAttentionOpen   = "collab.superagent.attention_opened"
	AuditAttentionExpire = "collab.superagent.attention_expired"
	AuditOutboxSent      = "collab.superagent.outbox_sent"
	AuditOutboxFailed    = "collab.superagent.outbox_failed"
	AuditReport          = "collab.superagent.worker_report"
)

// Source kinds used by the built-in adapters. They are open strings: a plugin
// may add Feishu or an internal ticket system without changing this package.
const (
	SourceGitHub  = "github"
	SourceFixture = "fixture"
	SourceManual  = "manual"
)

// ThreadRef identifies one conversation surface inside a Source. Key is the
// identity used for subscriptions and fan-out; Kind is descriptive metadata
// (issue, pull_request, conversation, ...) and must not change the identity.
type ThreadRef struct {
	Source string `json:"source"`
	Kind   string `json:"kind,omitempty"`
	ID     string `json:"id"`
	URL    string `json:"url,omitempty"`
}

// Key is the stable subscription/fan-out key of a thread.
func (t ThreadRef) Key() string {
	return strings.TrimSpace(t.Source) + "|" + strings.TrimSpace(t.ID)
}

// Valid reports whether the thread can be subscribed to.
func (t ThreadRef) Valid() bool {
	return strings.TrimSpace(t.Source) != "" && strings.TrimSpace(t.ID) != ""
}

// Actor is the identity that produced an event. It is recorded in the Ticket
// Mandate, never used as an authorization shortcut: authorization is the
// Policy's decision.
type Actor struct {
	ID      string `json:"id"`
	Login   string `json:"login,omitempty"`
	Display string `json:"display,omitempty"`
	IsBot   bool   `json:"isBot"`
}

// EventKind is the closed vocabulary of source events this package acts on.
// Adapters may emit an unknown kind; the policy decides to ignore it.
type EventKind string

const (
	EventIssueOpened     EventKind = "issue_opened"
	EventIssueUpdated    EventKind = "issue_updated"
	EventIssueComment    EventKind = "issue_comment"
	EventPullRequestOpen EventKind = "pull_request_opened"
	EventPullRequestNote EventKind = "pull_request_comment"
	EventCIStatus        EventKind = "ci_status"
	EventManual          EventKind = "manual"
)

// IsFollowUp reports whether the event is a conversation message that should
// fan-out to subscribed Tickets rather than create unrelated work.
func (k EventKind) IsFollowUp() bool {
	switch k {
	case EventIssueComment, EventPullRequestNote, EventIssueUpdated:
		return true
	default:
		return false
	}
}

// Valid reports whether the kind is in the built-in vocabulary. Unknown kinds
// are not errors: a Source may emit them and a Policy may accept them.
func (k EventKind) Valid() bool {
	switch k {
	case EventIssueOpened, EventIssueUpdated, EventIssueComment,
		EventPullRequestOpen, EventPullRequestNote, EventCIStatus, EventManual:
		return true
	default:
		return false
	}
}

// EventPayload is the canonical JSON document stored in collab.SourceEvent.
// Adapters (GitHub, fixtures) populate this shape so the orchestrator never
// parses a provider's wire format.
type EventPayload struct {
	EventID string    `json:"eventId"`
	Kind    EventKind `json:"kind"`
	Actor   Actor     `json:"actor"`
	Thread  ThreadRef `json:"thread"`
	Title   string    `json:"title,omitempty"`
	Body    string    `json:"body,omitempty"`
	URL     string    `json:"url,omitempty"`
}

// Event is the orchestrator's typed view of one Source event. It is also the
// Trigger record: SourceID names the source, Actor the requester, Thread the
// conversation and IdempotencyKey the trigger key. SeenEvent is its durable
// already-consumed row.
type Event struct {
	SourceID   string
	EventID    string
	Kind       EventKind
	Actor      Actor
	Thread     ThreadRef
	Title      string
	Body       string
	URL        string
	ReceivedAt time.Time
	Raw        json.RawMessage
}

// IdempotencyKey is the event identity used for ticket creation and the seen
// log. It is stable across replays of the same source event.
func (e Event) IdempotencyKey() string {
	return strings.TrimSpace(e.SourceID) + "/" + strings.TrimSpace(e.EventID)
}

// Validate checks the fields an orchestrator needs before it evaluates an
// event. Body is untrusted text and is never interpreted as policy.
func (e Event) Validate() error {
	switch {
	case strings.TrimSpace(e.SourceID) == "":
		return errors.New("superagent: source id is required")
	case strings.TrimSpace(e.EventID) == "":
		return errors.New("superagent: event id is required")
	case strings.TrimSpace(e.Thread.Source) == "" || strings.TrimSpace(e.Thread.ID) == "":
		return errors.New("superagent: event thread is required")
	}
	return nil
}

// ParseSourceEvent decodes the canonical payload of a collab.SourceEvent. An
// empty payload is accepted for a manual event, which carries its body in the
// SourceEvent itself.
func ParseSourceEvent(raw collab.SourceEvent) (Event, error) {
	event := Event{
		SourceID:   strings.TrimSpace(raw.SourceID),
		ReceivedAt: raw.ReceivedAt,
		Raw:        append(json.RawMessage(nil), raw.Payload...),
	}
	if len(raw.Payload) > 0 && strings.TrimSpace(string(raw.Payload)) != "null" {
		var payload EventPayload
		if err := json.Unmarshal(raw.Payload, &payload); err != nil {
			return Event{}, fmt.Errorf("superagent: decode source event: %w", err)
		}
		event.EventID = strings.TrimSpace(payload.EventID)
		event.Kind = payload.Kind
		event.Actor = payload.Actor
		event.Thread = payload.Thread
		event.Title = payload.Title
		event.Body = payload.Body
		event.URL = payload.URL
	}
	if event.EventID == "" {
		event.EventID = strings.TrimSpace(raw.ExternalID)
	}
	if event.Kind == "" {
		event.Kind = EventKind(strings.TrimSpace(raw.Kind))
	}
	if event.Thread.URL == "" {
		event.Thread.URL = event.URL
	}
	if event.Thread.Source == "" {
		event.Thread.Source = event.SourceID
	}
	return event, nil
}

// Action is the orchestrator's evaluation outcome for one event.
type Action string

const (
	ActionAccept Action = "accept"
	ActionAttach Action = "attach"
	ActionStop   Action = "stop"
	ActionIgnore Action = "ignore"
)

// Valid reports whether the action is in the closed vocabulary.
func (a Action) Valid() bool {
	switch a {
	case ActionAccept, ActionAttach, ActionStop, ActionIgnore:
		return true
	default:
		return false
	}
}

// Verdict is a Policy's structured decision. It never carries a permission
// grant: accepting a ticket only records a Mandate; the Worker still runs under
// its own Bot permissions.
type Verdict struct {
	Action Action `json:"action"`
	Reason string `json:"reason,omitempty"`
	// Title overrides the ticket title (empty uses the event title).
	Title string `json:"title,omitempty"`
	// BotID optionally overrides the configured worker Bot for this ticket.
	BotID string `json:"botId,omitempty"`
	// Strength records what the requester is allowed to ask for.
	Strength MandateStrength `json:"strength,omitempty"`
	// PolicyVersion is the policy revision that produced this verdict.
	PolicyVersion string `json:"policyVersion,omitempty"`
}

// Policy evaluates untrusted Source events. Implementations MUST NOT let the
// event body mutate policy, and MUST NOT infer authorization from the text.
type Policy interface {
	Evaluate(ctx context.Context, tenantID uuid.UUID, event Event) (Verdict, error)
}

// MandateStrength is the recorded authorization strength of an accepted ticket.
type MandateStrength string

const (
	MandateObserve MandateStrength = "observe"
	MandateComment MandateStrength = "comment"
	MandateWrite   MandateStrength = "write"
)

// Valid reports whether the strength is in the closed vocabulary.
func (m MandateStrength) Valid() bool {
	switch m {
	case MandateObserve, MandateComment, MandateWrite:
		return true
	default:
		return false
	}
}

// Mandate is the recorded authorization of an accepted Ticket: who asked,
// through which Source/Thread, under which policy revision and with what
// strength. A human stop revokes it; policy changes do not retroactively
// revoke in-flight work (they are recorded and delivered).
type Mandate struct {
	Requester       Actor           `json:"requester"`
	Source          string          `json:"source"`
	Thread          ThreadRef       `json:"thread"`
	Strength        MandateStrength `json:"strength"`
	PolicyVersion   string          `json:"policyVersion,omitempty"`
	AcceptedAt      time.Time       `json:"acceptedAt"`
	AcceptedBy      string          `json:"acceptedBy,omitempty"`
	RevokedAt       *time.Time      `json:"revokedAt,omitempty"`
	RevokedBy       string          `json:"revokedBy,omitempty"`
	RevocationCause string          `json:"revocationCause,omitempty"`
}

// Revoked reports whether the mandate was withdrawn by a human stop.
func (m Mandate) Revoked() bool { return m.RevokedAt != nil }

// ticketDoc is the WorkItem.Input document for super-agent tickets. Keeping it
// in the generic WorkItem store means the core package needs no super-agent
// table and one WorkStore implementation serves every mode.
type ticketDoc struct {
	Mandate   Mandate   `json:"mandate"`
	Thread    ThreadRef `json:"thread"`
	Request   string    `json:"request,omitempty"`
	WorkerBot string    `json:"workerBot,omitempty"`
	Event     *Event    `json:"event,omitempty"`
}

// Ticket is a collab.WorkItem whose work unit is a ticket. It is independent of
// any Session: the Worker Session may end or be replaced while the Ticket and
// its Mandate stay.
type Ticket struct {
	collab.WorkItem
	Mandate   Mandate   `json:"mandate"`
	Thread    ThreadRef `json:"thread"`
	Request   string    `json:"request,omitempty"`
	WorkerBot string    `json:"workerBot,omitempty"`
}

// TicketFromWorkItem projects a stored WorkItem into a Ticket. A malformed
// document is returned as an empty mandate/thread rather than hiding the row:
// the store remains the fact and the caller can repair it.
func TicketFromWorkItem(item collab.WorkItem) Ticket {
	ticket := Ticket{WorkItem: item}
	if len(item.Input) == 0 {
		return ticket
	}
	var doc ticketDoc
	if err := json.Unmarshal(item.Input, &doc); err != nil {
		return ticket
	}
	ticket.Mandate = doc.Mandate
	ticket.Thread = doc.Thread
	ticket.Request = doc.Request
	ticket.WorkerBot = doc.WorkerBot
	return ticket
}

// Record projects the Ticket back onto the generic WorkItem shape, replacing
// Input with the ticket document.
func (t Ticket) Record() collab.WorkItem {
	item := t.WorkItem
	doc := ticketDoc{Mandate: t.Mandate, Thread: t.Thread, Request: t.Request, WorkerBot: t.WorkerBot}
	if raw, err := json.Marshal(doc); err == nil {
		item.Input = raw
	}
	return item
}

// NewTicket builds an open Ticket from an accepted event and a verdict.
func NewTicket(tenantID uuid.UUID, modeID string, event Event, verdict Verdict, workerBot string, now time.Time) Ticket {
	strength := verdict.Strength
	if strength == "" {
		strength = MandateComment
	}
	title := strings.TrimSpace(verdict.Title)
	if title == "" {
		title = strings.TrimSpace(event.Title)
	}
	if title == "" {
		title = event.Kind.String() + " from " + event.Actor.Login
	}
	mandate := Mandate{
		Requester:     event.Actor,
		Source:        event.SourceID,
		Thread:        event.Thread,
		Strength:      strength,
		PolicyVersion: verdict.PolicyVersion,
		AcceptedAt:    now,
	}
	item := collab.WorkItem{
		ID:        uuid.New(),
		TenantID:  tenantID,
		ModeID:    modeID,
		Title:     title,
		Source:    collab.SourceRef{Kind: event.SourceID, ID: event.Thread.ID, URI: event.Thread.URL},
		Scope:     collab.Scope{Resources: []collab.ResourceRef{{Type: threadResourceType(event.Thread), ID: event.Thread.ID}}},
		Status:    collab.WorkItemOpen,
		CreatedAt: now,
		UpdatedAt: now,
		Input:     nil,
	}
	ticket := Ticket{WorkItem: item, Mandate: mandate, Thread: event.Thread, Request: strings.TrimSpace(event.Body), WorkerBot: workerBot}
	ticket.WorkItem.Input = ticket.Record().Input
	return ticket
}

func threadResourceType(thread ThreadRef) string {
	if strings.TrimSpace(thread.Kind) != "" {
		return thread.Kind
	}
	return "thread"
}

// String returns a human-readable event kind.
func (k EventKind) String() string {
	if k == "" {
		return "event"
	}
	return string(k)
}

// Ticket transition helpers. The core WorkItem status vocabulary is reused;
// the super-agent rules decide which legal edges exist.

// CanAcceptFollowUp reports whether a thread follow-up may reopen the ticket.
func (t Ticket) CanAcceptFollowUp() bool {
	if t.Mandate.Revoked() {
		return false
	}
	switch t.Status {
	case collab.WorkItemDone, collab.WorkItemFailed, collab.WorkItemBlocked:
		return true
	default:
		return false
	}
}

// CanDispatch reports whether a Worker may be assigned now.
func (t Ticket) CanDispatch() bool {
	if t.Mandate.Revoked() {
		return false
	}
	switch t.Status {
	case collab.WorkItemOpen, collab.WorkItemActive:
		return true
	default:
		return false
	}
}

// CanStop reports whether a human stop can act on this ticket.
func (t Ticket) CanStop() bool {
	return t.Status != collab.WorkItemCancelled && t.Status != collab.WorkItemCancelling
}

// ValidateTransition checks one ticket status edge. It is intentionally stricter
// than "any status may be overwritten": an invalid edge is an error the caller
// must explain rather than a silent state change.
func ValidateTransition(from, to collab.WorkItemStatus) error {
	if from == to {
		return nil
	}
	allowed := false
	switch from {
	case collab.WorkItemOpen:
		allowed = to == collab.WorkItemActive || to == collab.WorkItemBlocked || to == collab.WorkItemDone ||
			to == collab.WorkItemFailed || to == collab.WorkItemCancelling || to == collab.WorkItemCancelled
	case collab.WorkItemActive:
		allowed = to == collab.WorkItemOpen || to == collab.WorkItemBlocked || to == collab.WorkItemDone ||
			to == collab.WorkItemFailed || to == collab.WorkItemCancelling || to == collab.WorkItemCancelled
	case collab.WorkItemBlocked:
		allowed = to == collab.WorkItemActive || to == collab.WorkItemOpen || to == collab.WorkItemDone ||
			to == collab.WorkItemFailed || to == collab.WorkItemCancelled || to == collab.WorkItemCancelling
	case collab.WorkItemDone, collab.WorkItemFailed:
		allowed = to == collab.WorkItemOpen || to == collab.WorkItemCancelled || to == collab.WorkItemCancelling
	case collab.WorkItemCancelling:
		allowed = to == collab.WorkItemCancelled
	case collab.WorkItemCancelled:
		allowed = false
	}
	if !allowed {
		return fmt.Errorf("superagent: ticket transition %s -> %s is not allowed", from, to)
	}
	return nil
}

// Subscription connects a Ticket to one Thread. It is a context subscription,
// never an authorization: a subscribed thread may deliver input, but it cannot
// grant a new permission or reopen a stopped ticket.
type Subscription struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenantId"`
	WorkItemID uuid.UUID `json:"workItemId"`
	Thread     ThreadRef `json:"thread"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Key is the unique identity of a subscription.
func (s Subscription) Key() string {
	return s.WorkItemID.String() + "|" + s.Thread.Key()
}

// SourceEventSeen is the durable "already consumed" fact for one source event.
// It records the decision too, so a duplicate delivery is visible without
// creating a second Ticket.
type SeenEvent struct {
	TenantID   uuid.UUID       `json:"tenantId"`
	SourceID   string          `json:"sourceId"`
	EventID    string          `json:"eventId"`
	Kind       string          `json:"kind,omitempty"`
	ActorID    string          `json:"actorId,omitempty"`
	ThreadID   string          `json:"threadId,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Decision   string          `json:"decision,omitempty"`
	WorkItemID uuid.UUID       `json:"workItemId,omitempty"`
	ReceivedAt time.Time       `json:"receivedAt"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// Key is the unique identity of a seen event.
func (s SeenEvent) Key() string {
	return strings.TrimSpace(s.SourceID) + "|" + strings.TrimSpace(s.EventID)
}
