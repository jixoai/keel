package superagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

// Store is the persistence port of the super-agent mode. It embeds the core
// WorkStore so Tickets, Assignments, Attentions and Links reuse the same
// transaction/tenant boundary; the extra methods are the records the core
// vocabulary deliberately does not own.
//
// Every read and write is tenant-scoped. A missing row and a foreign tenant's
// row are the same not-found answer.
type Store interface {
	collab.WorkStore

	GetAttention(ctx context.Context, tenantID, attentionID uuid.UUID) (collab.Attention, error)
	ListOpenAttentions(ctx context.Context, tenantID uuid.UUID) ([]collab.Attention, error)
	ListAssignmentsByStatus(ctx context.Context, tenantID uuid.UUID, statuses ...collab.AssignmentStatus) ([]collab.Assignment, error)

	SourceEventSeen(ctx context.Context, tenantID uuid.UUID, sourceID, eventID string) (bool, error)
	RecordSourceEvent(ctx context.Context, event SeenEvent) error

	GetSourceCursor(ctx context.Context, tenantID uuid.UUID, sourceID string) (string, error)
	SetSourceCursor(ctx context.Context, tenantID uuid.UUID, sourceID, cursor string) error

	CreateSubscription(ctx context.Context, subscription Subscription) (Subscription, error)
	UpdateSubscription(ctx context.Context, subscription Subscription) (Subscription, error)
	ListSubscriptions(ctx context.Context, tenantID, workItemID uuid.UUID) ([]Subscription, error)
	ListThreadSubscriptions(ctx context.Context, tenantID uuid.UUID, thread ThreadRef) ([]Subscription, error)

	// Worker reports are the ticket timeline facts and the durable idempotency
	// ledger of the model-facing `report` tool. A claim is scoped by
	// assignment+session+request id; a duplicate returns created=false.
	GetWorkerReport(ctx context.Context, tenantID, workItemID, assignmentID, sessionID uuid.UUID, requestID string) (WorkerReport, error)
	ClaimWorkerReport(ctx context.Context, report WorkerReport) (WorkerReport, bool, error)
	MarkWorkerReportAudited(ctx context.Context, tenantID, reportID uuid.UUID, at time.Time) (bool, error)
	MarkWorkerReportApplied(ctx context.Context, tenantID, reportID uuid.UUID, at time.Time) (bool, error)

	EnqueueOutbox(ctx context.Context, message OutboxMessage) (OutboxMessage, error)
	ListOutbox(ctx context.Context, tenantID uuid.UUID, status OutboxStatus, limit int) ([]OutboxMessage, error)
	UpdateOutbox(ctx context.Context, message OutboxMessage) (OutboxMessage, error)

	// ListAttentionsByStatus lists the newest attention rows in the named states
	// (all states for an empty list), newest first, bounded by limit and limited
	// to modeID when non-empty. The answer recovery scan uses it to find a
	// recorded answer with no delivery evidence.
	ListAttentionsByStatus(ctx context.Context, tenantID uuid.UUID, modeID string, limit int, statuses ...collab.AttentionStatus) ([]collab.Attention, error)

	// AcquireWriterLease takes the tenant's single-writer guard. Run holds it for
	// the process lifetime; a second orchestrator against the same tenant fails
	// closed instead of polling and delivering in parallel.
	AcquireWriterLease(ctx context.Context, tenantID uuid.UUID) (WriterLease, error)
}

// ErrWriterLeaseHeld is the fail-closed answer of a second writer: another
// process owns this tenant's queue.
var ErrWriterLeaseHeld = errors.New("superagent: another orchestrator owns this tenant's queue")

// WriterLease is the durable single-writer ownership of one tenant's queue. It
// is released on shutdown; a crashed process's lease expires with its
// connection, so recovery does not need a manual unlock step. Healthy MUST
// prove the lease is still held (a live round trip against the owning
// connection): a PostgreSQL restart/failover silently releases the advisory
// lock with the backend, and the queue must stop writing instead of assuming it
// still owns the tenant.
type WriterLease interface {
	Healthy(ctx context.Context) error
	Release(ctx context.Context) error
}

// WriterLeaseStore is the narrow capability Run requires. It is a separate
// interface so a read-only test double can state that it cannot host a queue
// instead of silently running a second writer.
type WriterLeaseStore interface {
	AcquireWriterLease(ctx context.Context, tenantID uuid.UUID) (WriterLease, error)
}

// MemoryStore is the in-process Store used by tests and a deployment that has
// no collaboration persistence yet. Every record is lost at process exit; it is
// explicitly not a persistence decision.
type MemoryStore struct {
	mu          sync.Mutex
	works       *collab.MemoryWorkStore
	now         func() time.Time
	assignments map[uuid.UUID]collab.Assignment
	attentions  map[uuid.UUID]collab.Attention
	events      map[string]SeenEvent
	cursors     map[string]string
	subs        map[uuid.UUID]Subscription
	outbox      map[uuid.UUID]OutboxMessage
	outboxKeys  map[string]uuid.UUID
	reports     map[uuid.UUID]WorkerReport
	reportKeys  map[string]uuid.UUID
	leases      map[string]bool
}

// NewMemoryStore builds an empty store. A nil clock uses time.Now (UTC).
func NewMemoryStore(now func() time.Time) *MemoryStore {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &MemoryStore{
		works:       collab.NewMemoryWorkStore(now),
		now:         now,
		assignments: make(map[uuid.UUID]collab.Assignment),
		attentions:  make(map[uuid.UUID]collab.Attention),
		events:      make(map[string]SeenEvent),
		cursors:     make(map[string]string),
		subs:        make(map[uuid.UUID]Subscription),
		outbox:      make(map[uuid.UUID]OutboxMessage),
		outboxKeys:  make(map[string]uuid.UUID),
		reports:     make(map[uuid.UUID]WorkerReport),
		reportKeys:  make(map[string]uuid.UUID),
		leases:      make(map[string]bool),
	}
}

func (m *MemoryStore) CreateWorkItem(ctx context.Context, item collab.WorkItem) (collab.WorkItem, error) {
	return m.works.CreateWorkItem(ctx, item)
}

func (m *MemoryStore) GetWorkItem(ctx context.Context, tenantID, workItemID uuid.UUID) (collab.WorkItem, error) {
	return m.works.GetWorkItem(ctx, tenantID, workItemID)
}

func (m *MemoryStore) ListWorkItems(ctx context.Context, tenantID uuid.UUID, filter collab.WorkFilter) ([]collab.WorkItem, error) {
	return m.works.ListWorkItems(ctx, tenantID, filter)
}

func (m *MemoryStore) UpdateWorkItem(ctx context.Context, item collab.WorkItem) (collab.WorkItem, error) {
	return m.works.UpdateWorkItem(ctx, item)
}

func (m *MemoryStore) CreateAssignment(ctx context.Context, assignment collab.Assignment) (collab.Assignment, error) {
	stored, err := m.works.CreateAssignment(ctx, assignment)
	if err != nil {
		return collab.Assignment{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.assignments[stored.ID] = cloneAssignment(stored)
	return cloneAssignment(stored), nil
}

func (m *MemoryStore) GetAssignment(ctx context.Context, tenantID, assignmentID uuid.UUID) (collab.Assignment, error) {
	return m.works.GetAssignment(ctx, tenantID, assignmentID)
}

func (m *MemoryStore) ListAssignments(ctx context.Context, tenantID, workItemID uuid.UUID) ([]collab.Assignment, error) {
	return m.works.ListAssignments(ctx, tenantID, workItemID)
}

func (m *MemoryStore) UpdateAssignment(ctx context.Context, assignment collab.Assignment) (collab.Assignment, error) {
	stored, err := m.works.UpdateAssignment(ctx, assignment)
	if err != nil {
		return collab.Assignment{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.assignments[stored.ID] = cloneAssignment(stored)
	return cloneAssignment(stored), nil
}

func (m *MemoryStore) CreateAttention(ctx context.Context, attention collab.Attention) (collab.Attention, error) {
	stored, err := m.works.CreateAttention(ctx, attention)
	if err != nil {
		return collab.Attention{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attentions[stored.ID] = cloneAttention(stored)
	return cloneAttention(stored), nil
}

func (m *MemoryStore) UpdateAttention(ctx context.Context, attention collab.Attention) (collab.Attention, error) {
	stored, err := m.works.UpdateAttention(ctx, attention)
	if err != nil {
		return collab.Attention{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attentions[stored.ID] = cloneAttention(stored)
	return cloneAttention(stored), nil
}

func (m *MemoryStore) ListAttentions(ctx context.Context, tenantID, workItemID uuid.UUID) ([]collab.Attention, error) {
	return m.works.ListAttentions(ctx, tenantID, workItemID)
}

func (m *MemoryStore) GetAttention(_ context.Context, tenantID, attentionID uuid.UUID) (collab.Attention, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	attention, ok := m.attentions[attentionID]
	if !ok || attention.TenantID != tenantID {
		return collab.Attention{}, fmt.Errorf("%w: attention not found", collab.ErrAttentionNotFound)
	}
	return cloneAttention(attention), nil
}

func (m *MemoryStore) ListOpenAttentions(_ context.Context, tenantID uuid.UUID) ([]collab.Attention, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]collab.Attention, 0)
	for _, attention := range m.attentions {
		if attention.TenantID == tenantID && attention.Status == collab.AttentionOpen {
			out = append(out, cloneAttention(attention))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

// ListAttentionsByStatus lists the newest attention rows in the named states
// (all states for an empty list), newest first, bounded by limit and limited to
// modeID when non-empty.
func (m *MemoryStore) ListAttentionsByStatus(_ context.Context, tenantID uuid.UUID, modeID string, limit int, statuses ...collab.AttentionStatus) ([]collab.Attention, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wanted := make(map[collab.AttentionStatus]bool, len(statuses))
	for _, status := range statuses {
		wanted[status] = true
	}
	out := make([]collab.Attention, 0)
	for _, attention := range m.attentions {
		if attention.TenantID != tenantID {
			continue
		}
		if modeID != "" && attention.ModeID != modeID {
			continue
		}
		if len(wanted) > 0 && !wanted[attention.Status] {
			continue
		}
		out = append(out, cloneAttention(attention))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// AcquireWriterLease grants one process-local writer for a tenant. The memory
// store is a test/demo store, so the lease is process-local by construction.
func (m *MemoryStore) AcquireWriterLease(_ context.Context, tenantID uuid.UUID) (WriterLease, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant is required", collab.ErrTenantRequired)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tenantKey(tenantID)
	if m.leases[key] {
		return nil, ErrWriterLeaseHeld
	}
	m.leases[key] = true
	return &memoryWriterLease{store: m, key: key}, nil
}

type memoryWriterLease struct {
	store *MemoryStore
	key   string
}

func (l *memoryWriterLease) Healthy(context.Context) error {
	if l == nil || l.store == nil {
		return ErrWriterLeaseHeld
	}
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	if !l.store.leases[l.key] {
		return ErrWriterLeaseHeld
	}
	return nil
}

func (l *memoryWriterLease) Release(context.Context) error {
	if l == nil || l.store == nil {
		return nil
	}
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	delete(l.store.leases, l.key)
	return nil
}

func (m *MemoryStore) ListAssignmentsByStatus(_ context.Context, tenantID uuid.UUID, statuses ...collab.AssignmentStatus) ([]collab.Assignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wanted := make(map[collab.AssignmentStatus]bool, len(statuses))
	for _, status := range statuses {
		wanted[status] = true
	}
	out := make([]collab.Assignment, 0)
	for _, assignment := range m.assignments {
		if assignment.TenantID != tenantID {
			continue
		}
		if len(wanted) > 0 && !wanted[assignment.Status] {
			continue
		}
		out = append(out, cloneAssignment(assignment))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

func (m *MemoryStore) AddLink(ctx context.Context, link collab.Link) (collab.Link, error) {
	return m.works.AddLink(ctx, link)
}

func (m *MemoryStore) ListLinks(ctx context.Context, tenantID, workItemID uuid.UUID) ([]collab.Link, error) {
	return m.works.ListLinks(ctx, tenantID, workItemID)
}

func (m *MemoryStore) SourceEventSeen(_ context.Context, tenantID uuid.UUID, sourceID, eventID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.events[tenantKey(tenantID)+"|"+sourceID+"|"+eventID]
	return ok, nil
}

func (m *MemoryStore) RecordSourceEvent(_ context.Context, event SeenEvent) error {
	if event.TenantID == uuid.Nil || event.SourceID == "" || event.EventID == "" {
		return fmt.Errorf("%w: tenant, source and event ids are required", collab.ErrTenantRequired)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tenantKey(event.TenantID) + "|" + event.Key()
	if _, ok := m.events[key]; ok {
		return nil
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = m.now()
	}
	m.events[key] = event
	return nil
}

func (m *MemoryStore) GetSourceCursor(_ context.Context, tenantID uuid.UUID, sourceID string) (string, error) {
	if tenantID == uuid.Nil || sourceID == "" {
		return "", fmt.Errorf("%w: tenant and source ids are required", collab.ErrTenantRequired)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursors[tenantKey(tenantID)+"|"+sourceID], nil
}

func (m *MemoryStore) SetSourceCursor(_ context.Context, tenantID uuid.UUID, sourceID, cursor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if tenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant is required", collab.ErrTenantRequired)
	}
	m.cursors[tenantKey(tenantID)+"|"+sourceID] = cursor
	return nil
}

func (m *MemoryStore) CreateSubscription(_ context.Context, subscription Subscription) (Subscription, error) {
	if subscription.TenantID == uuid.Nil {
		return Subscription{}, fmt.Errorf("%w: tenant is required", collab.ErrTenantRequired)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.subs {
		if existing.TenantID == subscription.TenantID && existing.Key() == subscription.Key() {
			return existing, nil
		}
	}
	if subscription.ID == uuid.Nil {
		subscription.ID = uuid.New()
	}
	now := m.now()
	if subscription.CreatedAt.IsZero() {
		subscription.CreatedAt = now
	}
	subscription.UpdatedAt = now
	m.subs[subscription.ID] = subscription
	return subscription, nil
}

func (m *MemoryStore) UpdateSubscription(_ context.Context, subscription Subscription) (Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.subs[subscription.ID]
	if !ok || existing.TenantID != subscription.TenantID {
		return Subscription{}, fmt.Errorf("superagent: subscription not found")
	}
	subscription.UpdatedAt = m.now()
	m.subs[subscription.ID] = subscription
	return subscription, nil
}

func (m *MemoryStore) ListSubscriptions(_ context.Context, tenantID, workItemID uuid.UUID) ([]Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Subscription, 0)
	for _, subscription := range m.subs {
		if subscription.TenantID == tenantID && subscription.WorkItemID == workItemID {
			out = append(out, subscription)
		}
	}
	sortSubscriptions(out)
	return out, nil
}

func (m *MemoryStore) ListThreadSubscriptions(_ context.Context, tenantID uuid.UUID, thread ThreadRef) ([]Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Subscription, 0)
	for _, subscription := range m.subs {
		if subscription.TenantID == tenantID && subscription.Active && subscription.Thread.Key() == thread.Key() {
			out = append(out, subscription)
		}
	}
	sortSubscriptions(out)
	return out, nil
}

func (m *MemoryStore) EnqueueOutbox(_ context.Context, message OutboxMessage) (OutboxMessage, error) {
	if message.TenantID == uuid.Nil {
		return OutboxMessage{}, fmt.Errorf("%w: tenant is required", collab.ErrTenantRequired)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if message.IdempotencyKey != "" {
		key := tenantKey(message.TenantID) + "|" + message.IdempotencyKey
		if existing, ok := m.outboxKeys[key]; ok {
			return m.outbox[existing], nil
		}
	}
	if message.ID == uuid.Nil {
		message.ID = uuid.New()
	}
	now := m.now()
	if message.CreatedAt.IsZero() {
		message.CreatedAt = now
	}
	if message.Status == "" {
		message.Status = OutboxPending
	}
	if message.NextAttemptAt.IsZero() {
		message.NextAttemptAt = now
	}
	message.UpdatedAt = now
	m.outbox[message.ID] = message
	if message.IdempotencyKey != "" {
		m.outboxKeys[tenantKey(message.TenantID)+"|"+message.IdempotencyKey] = message.ID
	}
	return message, nil
}

func (m *MemoryStore) ListOutbox(_ context.Context, tenantID uuid.UUID, status OutboxStatus, limit int) ([]OutboxMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]OutboxMessage, 0)
	for _, message := range m.outbox {
		if message.TenantID != tenantID {
			continue
		}
		if status != "" && message.Status != status {
			continue
		}
		out = append(out, message)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) UpdateOutbox(_ context.Context, message OutboxMessage) (OutboxMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.outbox[message.ID]
	if !ok || existing.TenantID != message.TenantID {
		return OutboxMessage{}, fmt.Errorf("superagent: outbox message not found")
	}
	message.UpdatedAt = m.now()
	m.outbox[message.ID] = message
	return message, nil
}

// GetWorkerReport reads one report by its assignment/session/request-id
// idempotency key. A foreign tenant's row and a missing row are the same
// not-found answer.
func (m *MemoryStore) GetWorkerReport(_ context.Context, tenantID, workItemID, assignmentID, sessionID uuid.UUID, requestID string) (WorkerReport, error) {
	key, ok := workerReportKey(tenantID, workItemID, assignmentID, sessionID, requestID)
	if !ok {
		return WorkerReport{}, ErrReportNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.reportKeys[key]
	if !ok {
		return WorkerReport{}, ErrReportNotFound
	}
	report, ok := m.reports[id]
	if !ok || report.TenantID != tenantID || report.WorkItemID != workItemID {
		return WorkerReport{}, ErrReportNotFound
	}
	return cloneWorkerReport(report), nil
}

// ClaimWorkerReport inserts one report at most once per
// assignment/session/request-id key. The row is the idempotency claim; it is
// inserted before the audit and the state transition.
func (m *MemoryStore) ClaimWorkerReport(_ context.Context, report WorkerReport) (WorkerReport, bool, error) {
	if report.TenantID == uuid.Nil || report.WorkItemID == uuid.Nil || report.AssignmentID == uuid.Nil || report.SessionID == uuid.Nil {
		return WorkerReport{}, false, fmt.Errorf("%w: tenant, work item, assignment and session ids are required", collab.ErrTenantRequired)
	}
	report.RequestID = strings.TrimSpace(report.RequestID)
	if report.RequestID == "" {
		return WorkerReport{}, false, fmt.Errorf("superagent: a worker report claim needs a request id")
	}
	key, ok := workerReportKey(report.TenantID, report.WorkItemID, report.AssignmentID, report.SessionID, report.RequestID)
	if !ok {
		return WorkerReport{}, false, fmt.Errorf("superagent: invalid worker report claim key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.reportKeys[key]; ok {
		return cloneWorkerReport(m.reports[id]), false, nil
	}
	if report.ID == uuid.Nil {
		report.ID = uuid.New()
	}
	if report.CreatedAt.IsZero() {
		report.CreatedAt = m.now()
	}
	m.reports[report.ID] = cloneWorkerReport(report)
	m.reportKeys[key] = report.ID
	return cloneWorkerReport(report), true, nil
}

// MarkWorkerReportAudited transitions an unaudited claim. It returns true when
// this call performed the transition and false when another attempt already
// did; a missing claim is ErrReportNotFound.
func (m *MemoryStore) MarkWorkerReportAudited(_ context.Context, tenantID, reportID uuid.UUID, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	report, ok := m.reports[reportID]
	if !ok || report.TenantID != tenantID {
		return false, ErrReportNotFound
	}
	if report.AuditedAt != nil {
		return false, nil
	}
	stamp := at.UTC()
	report.AuditedAt = &stamp
	m.reports[reportID] = report
	return true, nil
}

// MarkWorkerReportApplied transitions a claim whose state transition landed.
// It returns true when this call performed the transition.
func (m *MemoryStore) MarkWorkerReportApplied(_ context.Context, tenantID, reportID uuid.UUID, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	report, ok := m.reports[reportID]
	if !ok || report.TenantID != tenantID {
		return false, ErrReportNotFound
	}
	if report.AppliedAt != nil {
		return false, nil
	}
	stamp := at.UTC()
	report.AppliedAt = &stamp
	m.reports[reportID] = report
	return true, nil
}

func sortSubscriptions(subs []Subscription) {
	sort.Slice(subs, func(i, j int) bool {
		if !subs[i].CreatedAt.Equal(subs[j].CreatedAt) {
			return subs[i].CreatedAt.Before(subs[j].CreatedAt)
		}
		return subs[i].ID.String() < subs[j].ID.String()
	})
}

func tenantKey(tenantID uuid.UUID) string { return tenantID.String() }

// workerReportKey is the assignment/session-scoped idempotency key. A replaced
// worker's request id lives in its old assignment's key space, so it can never
// collide with the replacement assignment's report.
func workerReportKey(tenantID, workItemID, assignmentID, sessionID uuid.UUID, requestID string) (string, bool) {
	requestID = strings.TrimSpace(requestID)
	if tenantID == uuid.Nil || workItemID == uuid.Nil || assignmentID == uuid.Nil || sessionID == uuid.Nil || requestID == "" {
		return "", false
	}
	return tenantKey(tenantID) + "|" + workItemID.String() + "|" + assignmentID.String() + "|" + sessionID.String() + "|" + requestID, true
}

func cloneAssignment(assignment collab.Assignment) collab.Assignment {
	copied := assignment
	copied.Input = append([]byte(nil), assignment.Input...)
	copied.Result = append([]byte(nil), assignment.Result...)
	return copied
}

func cloneAttention(attention collab.Attention) collab.Attention {
	copied := attention
	copied.Options = append([]collab.AttentionOption(nil), attention.Options...)
	if attention.Answer != nil {
		answer := *attention.Answer
		copied.Answer = &answer
	}
	return copied
}

func cloneWorkerReport(report WorkerReport) WorkerReport {
	copied := report
	copied.Result = append(json.RawMessage(nil), report.Result...)
	return copied
}
