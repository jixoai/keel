package tasklog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// Projection is the read-model store the service drives. *PgProjection is the
// production implementation; tests may substitute another one.
type Projection interface {
	GetCard(ctx context.Context, tenantID, sessionID, taskID uuid.UUID) (Card, error)
	GetCardByID(ctx context.Context, tenantID, taskID uuid.UUID) (Card, error)
	ListCards(ctx context.Context, tenantID uuid.UUID, sessionID, parentTaskID *uuid.UUID, status string) ([]Card, error)
	ListEvents(ctx context.Context, tenantID, sessionID, taskID uuid.UUID) ([]Event, error)
	CountEvents(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error)
	UpsertCard(ctx context.Context, card Card) error
	InsertEvent(ctx context.Context, tenantID, sessionID uuid.UUID, event Event) error
	ReplaceSession(ctx context.Context, tenantID, sessionID uuid.UUID, cards []Card, events []Event) error
}

// API is the task-log surface the tools surface and the keel.tasks plugin
// depend on. Keeping it an interface lets the aggregate-tool gate and the
// plugin be tested without a database.
type API interface {
	EnsureCard(ctx context.Context, scope kernel.Scope) (Card, error)
	Resolve(ctx context.Context, scope kernel.Scope, taskID string) (Card, error)
	Create(ctx context.Context, scope kernel.Scope, in CreateInput, callID string) (Card, error)
	Update(ctx context.Context, scope kernel.Scope, taskID uuid.UUID, in UpdateInput, callID string) (Card, error)
	Attach(ctx context.Context, scope kernel.Scope, taskID uuid.UUID, path, hash, callID string) (Event, error)
	Detach(ctx context.Context, scope kernel.Scope, taskID uuid.UUID, path, hash, callID string) (Event, error)
	Close(ctx context.Context, scope kernel.Scope, taskID uuid.UUID, reason, callID string) (Card, error)
	Note(ctx context.Context, scope kernel.Scope, taskID uuid.UUID, text, callID string) (Event, error)
	List(ctx context.Context, scope kernel.Scope, filter ListFilter) ([]Card, error)
	Show(ctx context.Context, scope kernel.Scope, taskID uuid.UUID) (View, error)
	RebuildSession(ctx context.Context, scope kernel.Scope) (Stats, error)
	ForkCard(ctx context.Context, origin kernel.Scope, originTaskID uuid.UUID, originSeq int64, target kernel.Scope, title, callID string) (Card, error)
	LinkSubagent(ctx context.Context, parent kernel.Scope, childSessionID uuid.UUID) (Card, error)
}

var _ API = (*Service)(nil)

// Options wires the service. Committer and Reader are the kernel seams; the
// service never sees a table handle.
type Options struct {
	Committer  kernel.Committer
	Reader     kernel.Reader
	Projection Projection
	// Now overrides the clock (tests).
	Now func() time.Time
	// NewID mints card ids (tests use it to make ids predictable).
	NewID func() uuid.UUID
}

// Service is the task-log facade. It is safe for concurrent use: every write
// goes through the kernel's per-session serialization, and projection upserts
// are guarded by ledger sequence.
type Service struct {
	committer  kernel.Committer
	reader     kernel.Reader
	projection Projection
	now        func() time.Time
	newID      func() uuid.UUID
}

// New builds the service.
func New(opts Options) *Service {
	service := &Service{
		committer:  opts.Committer,
		reader:     opts.Reader,
		projection: opts.Projection,
		now:        opts.Now,
		newID:      opts.NewID,
	}
	if service.now == nil {
		service.now = func() time.Time { return time.Now().UTC() }
	}
	if service.newID == nil {
		service.newID = uuid.New
	}
	return service
}

func (s *Service) ready() error {
	if s.committer == nil || s.reader == nil || s.projection == nil {
		return ErrNoBackend
	}
	return nil
}

// EnsureCard returns the session's initial card, creating it (through the
// ledger, system lane) when it does not exist yet. The id is derived from the
// session, so concurrent callers converge on one card and one ledger key.
func (s *Service) EnsureCard(ctx context.Context, scope kernel.Scope) (Card, error) {
	if err := s.ready(); err != nil {
		return Card{}, err
	}
	if scope.IsZero() {
		return Card{}, fmt.Errorf("%w: tenant and session are required", ErrTaskRequired)
	}
	taskID := InitialTaskID(scope.TenantID, scope.SessionID)
	card, err := s.projection.GetCard(ctx, scope.TenantID, scope.SessionID, taskID)
	if err == nil {
		return card, nil
	}
	if !errors.Is(err, ErrTaskNotFound) {
		return Card{}, err
	}
	title := s.firstInputTitle(ctx, scope)
	payload := taskEventPayload{Title: title, Status: StatusOpen, Origin: OriginSession}
	event, _, err := s.commit(ctx, scope, kernel.KindTaskCreated, taskID, payload,
		scope.SessionID.String()+":init")
	if err != nil {
		if errors.Is(err, kernel.ErrKeyConflict) {
			// Another writer created the card with a different title; the card
			// exists either way, so read it instead of fighting the ledger.
			if existing, readErr := s.projection.GetCard(ctx, scope.TenantID, scope.SessionID, taskID); readErr == nil {
				return existing, nil
			}
		}
		return Card{}, fmt.Errorf("tasklog: create initial card: %w", err)
	}
	return s.project(ctx, scope, event, payload)
}

// Resolve maps a caller-supplied task id onto a card of this session. An empty
// id means "the session's active card" and is auto-attributed; an id that does
// not resolve is a typed not-found (after one projection repair attempt).
func (s *Service) Resolve(ctx context.Context, scope kernel.Scope, taskID string) (Card, error) {
	if strings.TrimSpace(taskID) == "" {
		return s.EnsureCard(ctx, scope)
	}
	id, err := uuid.Parse(strings.TrimSpace(taskID))
	if err != nil {
		return Card{}, fmt.Errorf("%w: %q", ErrInvalidTaskID, taskID)
	}
	return s.requireCard(ctx, scope, id)
}

// Create appends task.created for a new card in the caller's session.
func (s *Service) Create(ctx context.Context, scope kernel.Scope, in CreateInput, callID string) (Card, error) {
	if err := s.ready(); err != nil {
		return Card{}, err
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return Card{}, fmt.Errorf("%w: create needs a title", ErrInvalidEvent)
	}
	status := in.Status
	if status == "" {
		status = StatusOpen
	}
	if !ValidStatus(status) {
		return Card{}, fmt.Errorf("%w: %q", ErrInvalidStatus, status)
	}
	taskID := s.newID()
	if in.ParentTaskID != nil {
		if err := s.validateParent(ctx, scope.TenantID, taskID, *in.ParentTaskID); err != nil {
			return Card{}, err
		}
	}
	payload := taskEventPayload{Title: title, Status: status, Origin: OriginTool}
	if in.ParentTaskID != nil {
		payload.ParentTaskID = in.ParentTaskID.String()
	}
	event, _, err := s.commit(ctx, scope, kernel.KindTaskCreated, taskID, payload,
		fmt.Sprintf("%s:created:%s", taskID, callSuffix(callID)))
	if err != nil {
		return Card{}, err
	}
	return s.project(ctx, scope, event, payload)
}

// Update appends task.updated with the changed fields.
func (s *Service) Update(ctx context.Context, scope kernel.Scope, taskID uuid.UUID, in UpdateInput, callID string) (Card, error) {
	if err := s.ready(); err != nil {
		return Card{}, err
	}
	card, err := s.requireCard(ctx, scope, taskID)
	if err != nil {
		return Card{}, err
	}
	payload := taskEventPayload{}
	if in.Title != nil {
		title := strings.TrimSpace(*in.Title)
		if title == "" {
			return Card{}, fmt.Errorf("%w: a title cannot be empty", ErrInvalidEvent)
		}
		payload.Title = title
	}
	if in.Status != nil {
		status := strings.TrimSpace(*in.Status)
		if !ValidStatus(status) {
			return Card{}, fmt.Errorf("%w: %q", ErrInvalidStatus, status)
		}
		payload.Status = status
	}
	if in.ParentTaskID != nil {
		if err := s.validateParent(ctx, scope.TenantID, card.TaskID, *in.ParentTaskID); err != nil {
			return Card{}, err
		}
		payload.ParentTaskID = in.ParentTaskID.String()
	}
	if payload.Title == "" && payload.Status == "" && payload.ParentTaskID == "" {
		return Card{}, fmt.Errorf("%w: update carries no change", ErrInvalidEvent)
	}
	event, _, err := s.commit(ctx, scope, kernel.KindTaskUpdated, taskID, payload,
		fmt.Sprintf("%s:updated:%s", taskID, callSuffix(callID)))
	if err != nil {
		return Card{}, err
	}
	return s.project(ctx, scope, event, payload)
}

// Attach records a (path, hash) reference on the card.
func (s *Service) Attach(ctx context.Context, scope kernel.Scope, taskID uuid.UUID, path, hash, callID string) (Event, error) {
	if err := s.ready(); err != nil {
		return Event{}, err
	}
	path = strings.TrimSpace(path)
	hash = strings.TrimSpace(hash)
	if path == "" || hash == "" {
		return Event{}, fmt.Errorf("%w: attach needs path and hash", ErrInvalidEvent)
	}
	if _, err := s.requireCard(ctx, scope, taskID); err != nil {
		return Event{}, err
	}
	payload := taskEventPayload{Path: path, Hash: hash}
	event, _, err := s.commit(ctx, scope, kernel.KindTaskAttached, taskID, payload,
		fmt.Sprintf("%s:attached:%s", taskID, callSuffix(callID)))
	if err != nil {
		return Event{}, err
	}
	_, err = s.project(ctx, scope, event, payload)
	return event, err
}

// Detach unbinds a path (or one hash of it) with task.detached.
func (s *Service) Detach(ctx context.Context, scope kernel.Scope, taskID uuid.UUID, path, hash, callID string) (Event, error) {
	if err := s.ready(); err != nil {
		return Event{}, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return Event{}, fmt.Errorf("%w: detach needs a path", ErrInvalidEvent)
	}
	if _, err := s.requireCard(ctx, scope, taskID); err != nil {
		return Event{}, err
	}
	payload := taskEventPayload{Path: path, Hash: strings.TrimSpace(hash)}
	event, _, err := s.commit(ctx, scope, kernel.KindTaskDetached, taskID, payload,
		fmt.Sprintf("%s:detached:%s", taskID, callSuffix(callID)))
	if err != nil {
		return Event{}, err
	}
	_, err = s.project(ctx, scope, event, payload)
	return event, err
}

// Close appends task.closed. Closing an already closed card is a no-op: the
// history already carries the fact.
func (s *Service) Close(ctx context.Context, scope kernel.Scope, taskID uuid.UUID, reason, callID string) (Card, error) {
	if err := s.ready(); err != nil {
		return Card{}, err
	}
	card, err := s.requireCard(ctx, scope, taskID)
	if err != nil {
		return Card{}, err
	}
	if card.Status == StatusClosed {
		return card, nil
	}
	payload := taskEventPayload{Reason: strings.TrimSpace(reason)}
	event, _, err := s.commit(ctx, scope, kernel.KindTaskClosed, taskID, payload,
		fmt.Sprintf("%s:closed:%s", taskID, callSuffix(callID)))
	if err != nil {
		return Card{}, err
	}
	return s.project(ctx, scope, event, payload)
}

// Note appends a free-form note to the card's history.
func (s *Service) Note(ctx context.Context, scope kernel.Scope, taskID uuid.UUID, text, callID string) (Event, error) {
	if err := s.ready(); err != nil {
		return Event{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Event{}, fmt.Errorf("%w: note needs text", ErrInvalidEvent)
	}
	if _, err := s.requireCard(ctx, scope, taskID); err != nil {
		return Event{}, err
	}
	payload := taskEventPayload{Text: text}
	event, _, err := s.commit(ctx, scope, kernel.KindTaskNote, taskID, payload,
		fmt.Sprintf("%s:note:%s", taskID, callSuffix(callID)))
	if err != nil {
		return Event{}, err
	}
	_, err = s.project(ctx, scope, event, payload)
	return event, err
}

// List reads cards from the projection. The default session is the caller's;
// another session of the same tenant may be named to walk the tree.
func (s *Service) List(ctx context.Context, scope kernel.Scope, filter ListFilter) ([]Card, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if filter.Status != "" && !ValidStatus(filter.Status) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, filter.Status)
	}
	sessionID := scope.SessionID
	if filter.SessionID != nil {
		sessionID = *filter.SessionID
	}
	return s.projection.ListCards(ctx, scope.TenantID, &sessionID, filter.ParentTaskID, filter.Status)
}

// Show renders one card with its history, children and attachments.
func (s *Service) Show(ctx context.Context, scope kernel.Scope, taskID uuid.UUID) (View, error) {
	if err := s.ready(); err != nil {
		return View{}, err
	}
	card, err := s.projection.GetCardByID(ctx, scope.TenantID, taskID)
	if err != nil {
		return View{}, err
	}
	events, err := s.projection.ListEvents(ctx, scope.TenantID, card.SessionID, taskID)
	if err != nil {
		return View{}, err
	}
	children, err := s.projection.ListCards(ctx, scope.TenantID, nil, &taskID, "")
	if err != nil {
		return View{}, err
	}
	return View{Card: card, Events: events, Children: children, Attachments: attachmentsFromEvents(events)}, nil
}

// ForkCard creates a new card in target (a new session) whose parentTaskId
// points at the origin card. The file-view prefix of a fork belongs to
// layerfs; this method records only the memory relation. It is the task-log
// half of agent.fork, which itself stays reserved until layerfs lands.
func (s *Service) ForkCard(ctx context.Context, origin kernel.Scope, originTaskID uuid.UUID, originSeq int64, target kernel.Scope, title, callID string) (Card, error) {
	if err := s.ready(); err != nil {
		return Card{}, err
	}
	if origin.TenantID != target.TenantID {
		// A fork never crosses tenants; answer exactly like a missing card so
		// the refusal leaks nothing about the other tenant.
		return Card{}, fmt.Errorf("%w: %s", ErrTaskNotFound, originTaskID)
	}
	if originSeq < 0 {
		return Card{}, fmt.Errorf("%w: fork originSeq must be non-negative", ErrInvalidEvent)
	}
	originCard, err := s.requireCard(ctx, origin, originTaskID)
	if err != nil {
		return Card{}, err
	}
	if strings.TrimSpace(title) == "" {
		title = originCard.Title
	}
	newID := s.newID()
	payload := taskEventPayload{
		Title:        title,
		Status:       StatusOpen,
		ParentTaskID: originCard.TaskID.String(),
		Origin:       OriginFork,
		OriginSeq:    originSeq,
	}
	event, _, err := s.commit(ctx, target, kernel.KindTaskCreated, newID, payload,
		fmt.Sprintf("%s:created:fork:%s", newID, callSuffix(callID)))
	if err != nil {
		return Card{}, err
	}
	return s.project(ctx, target, event, payload)
}

// LinkSubagent gives a sub-agent session its own card and points it at the
// parent's active card. It is idempotent: the child's initial card id is
// derived from its session, and the parent link is appended at most once.
func (s *Service) LinkSubagent(ctx context.Context, parent kernel.Scope, childSessionID uuid.UUID) (Card, error) {
	if err := s.ready(); err != nil {
		return Card{}, err
	}
	if parent.TenantID == uuid.Nil || parent.SessionID == uuid.Nil || childSessionID == uuid.Nil {
		return Card{}, fmt.Errorf("%w: parent session and child session are required", ErrTaskRequired)
	}
	parentCard, err := s.activeCard(ctx, parent)
	if err != nil {
		return Card{}, err
	}
	child := kernel.Scope{TenantID: parent.TenantID, SessionID: childSessionID}
	childTaskID := InitialTaskID(child.TenantID, child.SessionID)
	card, err := s.projection.GetCard(ctx, child.TenantID, child.SessionID, childTaskID)
	if err == nil {
		if card.ParentTaskID != nil && *card.ParentTaskID == parentCard.TaskID {
			return card, nil
		}
		return s.setParent(ctx, child, card, parentCard.TaskID, fmt.Sprintf("%s:updated:subagent:%s", childTaskID, parent.SessionID))
	}
	if !errors.Is(err, ErrTaskNotFound) {
		return Card{}, err
	}
	title := s.firstInputTitle(ctx, child)
	if title == "Session" {
		title = "Subagent"
	}
	payload := taskEventPayload{
		Title:        title,
		Status:       StatusOpen,
		ParentTaskID: parentCard.TaskID.String(),
		Origin:       OriginSubagent,
	}
	event, _, err := s.commit(ctx, child, kernel.KindTaskCreated, childTaskID, payload,
		child.SessionID.String()+":init")
	if err != nil {
		if errors.Is(err, kernel.ErrKeyConflict) {
			// The child's first tool call created its initial card first. Link
			// it instead of failing the spawn.
			if existing, readErr := s.projection.GetCard(ctx, child.TenantID, child.SessionID, childTaskID); readErr == nil {
				return s.setParent(ctx, child, existing, parentCard.TaskID, fmt.Sprintf("%s:updated:subagent:%s", childTaskID, parent.SessionID))
			}
		}
		return Card{}, fmt.Errorf("tasklog: create sub-agent card: %w", err)
	}
	return s.project(ctx, child, event, payload)
}

// setParent appends the parent link as task.updated (the card may already exist
// because the child's first tool call auto-created it).
func (s *Service) setParent(ctx context.Context, scope kernel.Scope, card Card, parentID uuid.UUID, key string) (Card, error) {
	if card.ParentTaskID != nil && *card.ParentTaskID == parentID {
		return card, nil
	}
	if err := s.validateParent(ctx, scope.TenantID, card.TaskID, parentID); err != nil {
		return Card{}, err
	}
	payload := taskEventPayload{ParentTaskID: parentID.String()}
	event, _, err := s.commit(ctx, scope, kernel.KindTaskUpdated, card.TaskID, payload, key)
	if err != nil {
		return Card{}, err
	}
	return s.project(ctx, scope, event, payload)
}

// activeCard returns the parent session's most recently touched non-closed card,
// falling back to the initial card.
func (s *Service) activeCard(ctx context.Context, scope kernel.Scope) (Card, error) {
	cards, err := s.projection.ListCards(ctx, scope.TenantID, &scope.SessionID, nil, "")
	if err != nil {
		return Card{}, err
	}
	for _, card := range cards {
		if card.Status != StatusClosed {
			return card, nil
		}
	}
	if len(cards) > 0 {
		return cards[0], nil
	}
	return s.EnsureCard(ctx, scope)
}

// requireCard resolves a card inside the session, repairing the projection once
// from the ledger before answering not-found.
func (s *Service) requireCard(ctx context.Context, scope kernel.Scope, taskID uuid.UUID) (Card, error) {
	card, err := s.projection.GetCard(ctx, scope.TenantID, scope.SessionID, taskID)
	if err == nil {
		return card, nil
	}
	if !errors.Is(err, ErrTaskNotFound) {
		return Card{}, err
	}
	if _, rebuildErr := s.RebuildSession(ctx, scope); rebuildErr == nil {
		card, err = s.projection.GetCard(ctx, scope.TenantID, scope.SessionID, taskID)
		if err == nil {
			return card, nil
		}
	}
	return Card{}, fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
}

// validateParent refuses a missing parent and any link that would close a
// cycle. The walk is bounded: a corrupt chain answers ErrTaskCycle instead of
// looping.
func (s *Service) validateParent(ctx context.Context, tenantID, childID, parentID uuid.UUID) error {
	if childID == parentID {
		return fmt.Errorf("%w: a card cannot be its own parent", ErrTaskCycle)
	}
	card, err := s.projection.GetCardByID(ctx, tenantID, parentID)
	if errors.Is(err, ErrTaskNotFound) {
		return fmt.Errorf("%w: parent %s", ErrTaskNotFound, parentID)
	}
	if err != nil {
		return err
	}
	seen := map[uuid.UUID]bool{parentID: true}
	for depth := 0; depth < 64 && card.ParentTaskID != nil; depth++ {
		if *card.ParentTaskID == childID {
			return fmt.Errorf("%w: %s is an ancestor of the proposed parent", ErrTaskCycle, childID)
		}
		if seen[*card.ParentTaskID] {
			return fmt.Errorf("%w: the existing parent chain is cyclic", ErrTaskCycle)
		}
		seen[*card.ParentTaskID] = true
		next, err := s.projection.GetCardByID(ctx, tenantID, *card.ParentTaskID)
		if err != nil {
			// A dangling ancestor cannot be walked; the link itself is still
			// safe to make.
			return nil
		}
		card = next
	}
	return nil
}

// commit appends one task event through the kernel's system lane and returns
// its materialized form. Task kinds are dual-lane by contract: the shard may be
// written before any run exists (initial card) and outside a run (panel edits).
func (s *Service) commit(ctx context.Context, scope kernel.Scope, kind kernel.EventKind, taskID uuid.UUID, payload taskEventPayload, eventKey string) (Event, kernel.Receipt, error) {
	payload.TaskID = taskID.String()
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, kernel.Receipt{}, fmt.Errorf("tasklog: encode %s: %w", kind, err)
	}
	receipt, err := s.committer.Commit(ctx, kernel.CommitRequest{
		Scope:        scope,
		Kind:         kind,
		System:       true,
		TaskEventKey: eventKey,
		Payload:      raw,
	})
	if err != nil {
		return Event{}, kernel.Receipt{}, err
	}
	event := Event{TaskID: taskID, Kind: string(kind), Seq: receipt.Seq, Payload: raw, CreatedAt: s.now()}
	return event, receipt, nil
}

// project materializes a just-committed event into the read models and returns
// the card as the projection now sees it.
func (s *Service) project(ctx context.Context, scope kernel.Scope, event Event, payload taskEventPayload) (Card, error) {
	if err := s.projection.InsertEvent(ctx, scope.TenantID, scope.SessionID, event); err != nil {
		return Card{}, err
	}
	switch event.Kind {
	case string(kernel.KindTaskCreated):
		card := cardFromCreated(scope, event, payload, s.now())
		if err := s.projection.UpsertCard(ctx, card); err != nil {
			return Card{}, err
		}
		return card, nil
	case string(kernel.KindTaskUpdated), string(kernel.KindTaskClosed):
		card, err := s.projection.GetCard(ctx, scope.TenantID, scope.SessionID, event.TaskID)
		if errors.Is(err, ErrTaskNotFound) {
			// The event references a card the projection has not seen yet; the
			// next rebuild recreates it. The event row is already recorded.
			return Card{}, nil
		}
		if err != nil {
			return Card{}, err
		}
		applyCardChange(&card, event, payload, s.now())
		if err := s.projection.UpsertCard(ctx, card); err != nil {
			return Card{}, err
		}
		return card, nil
	default:
		// attached / detached / note do not change the card row.
		card, err := s.projection.GetCard(ctx, scope.TenantID, scope.SessionID, event.TaskID)
		if errors.Is(err, ErrTaskNotFound) {
			return Card{}, nil
		}
		return card, err
	}
}

// firstInputTitle is the initial card's title: the first user input, trimmed to
// a readable length. It reads only the session's ledger, so it stays stable
// when two processes create the card concurrently.
func (s *Service) firstInputTitle(ctx context.Context, scope kernel.Scope) string {
	events, err := s.reader.LoadEvents(ctx, scope, 0, 200)
	if err != nil {
		return "Session"
	}
	for _, event := range events {
		if event.Kind != kernel.KindUserMessage {
			continue
		}
		text := strings.TrimSpace(kernel.EventText(event))
		if text == "" {
			continue
		}
		text = strings.Join(strings.Fields(text), " ")
		if utf8.RuneCountInString(text) > 80 {
			runes := []rune(text)
			text = string(runes[:80]) + "…"
		}
		return text
	}
	return "Session"
}

// callSuffix turns a tool-call id into a stable idempotency suffix. An absent
// id gets a fresh UUID: a manual call is a new fact, a retried tool call is not.
func callSuffix(callID string) string {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return uuid.NewString()
	}
	return callID
}

func cardFromCreated(scope kernel.Scope, event Event, payload taskEventPayload, now time.Time) Card {
	card := Card{
		TenantID:   scope.TenantID,
		SessionID:  scope.SessionID,
		TaskID:     event.TaskID,
		Title:      payload.Title,
		Status:     payload.Status,
		Origin:     payload.Origin,
		OriginSeq:  payload.OriginSeq,
		CreatedSeq: event.Seq,
		UpdatedSeq: event.Seq,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if card.Status == "" {
		card.Status = StatusOpen
	}
	if payload.ParentTaskID != "" {
		if parent, err := uuid.Parse(payload.ParentTaskID); err == nil {
			card.ParentTaskID = &parent
		}
	}
	return card
}

func applyCardChange(card *Card, event Event, payload taskEventPayload, now time.Time) {
	switch event.Kind {
	case string(kernel.KindTaskClosed):
		card.Status = StatusClosed
		card.ClosedSeq = event.Seq
	default:
		if payload.Title != "" {
			card.Title = payload.Title
		}
		if payload.Status != "" {
			card.Status = payload.Status
		}
	}
	if payload.ParentTaskID != "" {
		if parent, err := uuid.Parse(payload.ParentTaskID); err == nil {
			card.ParentTaskID = &parent
		}
	}
	card.UpdatedSeq = event.Seq
	card.UpdatedAt = now
}

// attachmentsFromEvents folds attach/detach events into the live reference set.
// A detached event with a hash unbinds that exact reference; without one it
// unbinds the path.
func attachmentsFromEvents(events []Event) []Attachment {
	live := map[string]string{}
	for _, event := range events {
		payload, err := decodeTaskPayload(event.Kind, event.Payload)
		if err != nil {
			continue
		}
		switch event.Kind {
		case string(kernel.KindTaskAttached):
			live[payload.Path] = payload.Hash
		case string(kernel.KindTaskDetached):
			if payload.Hash == "" || live[payload.Path] == payload.Hash {
				delete(live, payload.Path)
			}
		}
	}
	out := make([]Attachment, 0, len(live))
	for path, hash := range live {
		out = append(out, Attachment{Path: path, Hash: hash})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
