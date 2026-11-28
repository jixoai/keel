package collab

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// WorkStore is the persistence port for WorkItem/Assignment/Attention/Link.
// The core package deliberately does not decide a schema: the super-agent
// change can back it with dedicated tables, the sub-agent change with the
// existing session/task-card facts, and tests with MemoryWorkStore.
//
// All reads and writes are tenant-scoped. A missing row and a foreign tenant's
// row are the same not-found answer.
type WorkStore interface {
	CreateWorkItem(ctx context.Context, item WorkItem) (WorkItem, error)
	GetWorkItem(ctx context.Context, tenantID, workItemID uuid.UUID) (WorkItem, error)
	ListWorkItems(ctx context.Context, tenantID uuid.UUID, filter WorkFilter) ([]WorkItem, error)
	UpdateWorkItem(ctx context.Context, item WorkItem) (WorkItem, error)

	CreateAssignment(ctx context.Context, assignment Assignment) (Assignment, error)
	GetAssignment(ctx context.Context, tenantID, assignmentID uuid.UUID) (Assignment, error)
	ListAssignments(ctx context.Context, tenantID, workItemID uuid.UUID) ([]Assignment, error)
	// ListAssignmentsBySession returns every assignment of one session across
	// the tenant's WorkItems. The binding guards use it so a session cannot be
	// rebound to a second WorkItem through an index that never recorded its
	// first binding (manual takeover, restart, or a foreign writer).
	ListAssignmentsBySession(ctx context.Context, tenantID, sessionID uuid.UUID) ([]Assignment, error)
	UpdateAssignment(ctx context.Context, assignment Assignment) (Assignment, error)

	CreateAttention(ctx context.Context, attention Attention) (Attention, error)
	GetAttention(ctx context.Context, tenantID, attentionID uuid.UUID) (Attention, error)
	UpdateAttention(ctx context.Context, attention Attention) (Attention, error)
	ListAttentions(ctx context.Context, tenantID, workItemID uuid.UUID) ([]Attention, error)

	// AddLink records one natural relation at most once. A repeated relation
	// returns the existing row (idempotent); an explicit id that names a
	// different relation is a conflict.
	AddLink(ctx context.Context, link Link) (Link, error)
	GetLink(ctx context.Context, tenantID, linkID uuid.UUID) (Link, error)
	// ListLinks returns the relations hosted by the WorkItem and the relations
	// whose either endpoint is that WorkItem, so a cross-WorkItem relation is
	// readable from both sides without duplicating the row.
	ListLinks(ctx context.Context, tenantID, workItemID uuid.UUID) ([]Link, error)
}

// WorkOrder is the stable ordering of a work-item list. The zero value keeps
// the historical oldest-created-first order (the dispatch queue's fairness
// rule); a board asks for most-recently-updated-first explicitly.
type WorkOrder string

const (
	// WorkOrderCreatedAsc is the default: oldest created first, id ascending
	// as the stable tie-break.
	WorkOrderCreatedAsc WorkOrder = "created_asc"
	// WorkOrderUpdatedDesc is the board order: most recently updated first,
	// id ascending as the stable tie-break.
	WorkOrderUpdatedDesc WorkOrder = "updated_desc"
)

// Valid reports whether the order is in the closed vocabulary. The empty
// value is the default created-asc order.
func (o WorkOrder) Valid() bool {
	switch o {
	case "", WorkOrderCreatedAsc, WorkOrderUpdatedDesc:
		return true
	default:
		return false
	}
}

// WorkFilter narrows a work-item list. Zero values mean "no filter"; a zero
// Order keeps the default created-asc order.
type WorkFilter struct {
	ModeID string
	Status WorkItemStatus
	Order  WorkOrder
}

// SessionState is the index's view of one mode-created session. It exists so
// depth/width/concurrency guards can count live sessions without reading the
// kernel ledger.
type SessionState string

const (
	SessionActive    SessionState = "active"
	SessionDone      SessionState = "done"
	SessionFailed    SessionState = "failed"
	SessionCancelled SessionState = "cancelled"
)

// SessionOutcome is the caller-reported terminal outcome of a Host-created
// session. It releases the session's concurrency/width slot.
type SessionOutcome string

const (
	SessionOutcomeDone      SessionOutcome = "done"
	SessionOutcomeFailed    SessionOutcome = "failed"
	SessionOutcomeCancelled SessionOutcome = "cancelled"
)

// State maps the outcome onto the index state.
func (o SessionOutcome) State() (SessionState, bool) {
	switch o {
	case SessionOutcomeDone:
		return SessionDone, true
	case SessionOutcomeFailed:
		return SessionFailed, true
	case SessionOutcomeCancelled:
		return SessionCancelled, true
	default:
		return "", false
	}
}

// SessionRef is the Host's tracked identity of one created session. Depth is
// the collaboration depth: a top-level session is 0, a Host-created child is
// parent.Depth+1.
type SessionRef struct {
	TenantID        uuid.UUID    `json:"tenantId"`
	SessionID       uuid.UUID    `json:"sessionId"`
	ParentSessionID uuid.UUID    `json:"parentSessionId,omitempty"`
	BotID           string       `json:"botId,omitempty"`
	ModeID          string       `json:"modeId,omitempty"`
	WorkItemID      uuid.UUID    `json:"workItemId,omitempty"`
	Depth           int          `json:"depth"`
	State           SessionState `json:"state"`
	CreatedAt       time.Time    `json:"createdAt"`
}

// SessionIndex tracks sessions the Host created so the guards can count depth,
// width and concurrency. A production implementation may project the same
// facts from store rows; the in-memory version is the skeleton/test one.
type SessionIndex interface {
	Session(ctx context.Context, tenantID, sessionID uuid.UUID) (SessionRef, error)
	Put(ctx context.Context, ref SessionRef) error
	SetState(ctx context.Context, tenantID, sessionID uuid.UUID, state SessionState) error
	// SetWorkItem binds an existing session to a WorkItem (manual takeover of a
	// pre-existing worker session). A session already bound elsewhere must be
	// refused by the caller.
	SetWorkItem(ctx context.Context, tenantID, sessionID, workItemID uuid.UUID) error
	// Children returns the direct children of a session in any state; the
	// Cancel path walks the tree so a bare delegation is not orphaned.
	Children(ctx context.Context, tenantID, parentSessionID uuid.UUID) ([]SessionRef, error)
	ActiveChildren(ctx context.Context, tenantID, parentSessionID uuid.UUID) (int, error)
	ActiveInMode(ctx context.Context, tenantID uuid.UUID, modeID string) (int, error)
}

// MemoryWorkStore is the in-process WorkStore used by tests and by a
// deployment that has no collaboration persistence yet. It is explicitly not a
// persistence decision: every record is lost at process exit.
type MemoryWorkStore struct {
	mu          sync.Mutex
	now         func() time.Time
	works       map[uuid.UUID]WorkItem
	assignments map[uuid.UUID]Assignment
	attentions  map[uuid.UUID]Attention
	links       map[uuid.UUID]Link
	workKeys    map[string]uuid.UUID
	assignKeys  map[string]uuid.UUID
	attnKeys    map[string]uuid.UUID
}

// NewMemoryWorkStore builds an empty store. A nil clock uses time.Now (UTC).
func NewMemoryWorkStore(now func() time.Time) *MemoryWorkStore {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &MemoryWorkStore{
		now:         now,
		works:       make(map[uuid.UUID]WorkItem),
		assignments: make(map[uuid.UUID]Assignment),
		attentions:  make(map[uuid.UUID]Attention),
		links:       make(map[uuid.UUID]Link),
		workKeys:    make(map[string]uuid.UUID),
		assignKeys:  make(map[string]uuid.UUID),
		attnKeys:    make(map[string]uuid.UUID),
	}
}

func (m *MemoryWorkStore) CreateWorkItem(_ context.Context, item WorkItem) (WorkItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if item.TenantID == uuid.Nil {
		return WorkItem{}, newError(ErrTenantRequired, CodeTenantRequired, "create_work_item", "tenant is required")
	}
	if item.IdempotencyKey != "" {
		key := item.TenantID.String() + "|" + item.ModeID + "|" + item.IdempotencyKey
		if existing, ok := m.workKeys[key]; ok {
			return cloneWorkItem(m.works[existing]), nil
		}
		item.CreatedAt = m.now()
		item.UpdatedAt = item.CreatedAt
		m.works[item.ID] = item
		m.workKeys[key] = item.ID
		return cloneWorkItem(item), nil
	}
	item.CreatedAt = m.now()
	item.UpdatedAt = item.CreatedAt
	m.works[item.ID] = item
	return cloneWorkItem(item), nil
}

func (m *MemoryWorkStore) GetWorkItem(_ context.Context, tenantID, workItemID uuid.UUID) (WorkItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.works[workItemID]
	if !ok || item.TenantID != tenantID {
		return WorkItem{}, newError(ErrWorkItemNotFound, CodeWorkItemNotFound, "get_work_item", "work item not found")
	}
	return cloneWorkItem(item), nil
}

func (m *MemoryWorkStore) ListWorkItems(_ context.Context, tenantID uuid.UUID, filter WorkFilter) ([]WorkItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]WorkItem, 0)
	for _, item := range m.works {
		if item.TenantID != tenantID {
			continue
		}
		if filter.ModeID != "" && item.ModeID != filter.ModeID {
			continue
		}
		if filter.Status != "" && item.Status != filter.Status {
			continue
		}
		out = append(out, cloneWorkItem(item))
	}
	if !filter.Order.Valid() {
		return nil, newError(ErrInvalidRequest, CodeInvalidRequest, "list_work_items",
			"work item order "+string(filter.Order)+" is not in the closed vocabulary")
	}
	switch filter.Order {
	case WorkOrderUpdatedDesc:
		sort.Slice(out, func(i, j int) bool {
			if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
				return out[i].UpdatedAt.After(out[j].UpdatedAt)
			}
			return out[i].ID.String() < out[j].ID.String()
		})
	default:
		sort.Slice(out, func(i, j int) bool {
			if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
				return out[i].CreatedAt.Before(out[j].CreatedAt)
			}
			return out[i].ID.String() < out[j].ID.String()
		})
	}
	return out, nil
}

func (m *MemoryWorkStore) UpdateWorkItem(_ context.Context, item WorkItem) (WorkItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.works[item.ID]
	if !ok || existing.TenantID != item.TenantID {
		return WorkItem{}, newError(ErrWorkItemNotFound, CodeWorkItemNotFound, "update_work_item", "work item not found")
	}
	item.UpdatedAt = m.now()
	m.works[item.ID] = cloneWorkItem(item)
	return cloneWorkItem(item), nil
}

func (m *MemoryWorkStore) CreateAssignment(_ context.Context, assignment Assignment) (Assignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if assignment.TenantID == uuid.Nil {
		return Assignment{}, newError(ErrTenantRequired, CodeTenantRequired, "create_assignment", "tenant is required")
	}
	if assignment.IdempotencyKey != "" {
		key := assignment.TenantID.String() + "|" + assignment.WorkItemID.String() + "|" + assignment.IdempotencyKey
		if existing, ok := m.assignKeys[key]; ok {
			return cloneAssignment(m.assignments[existing]), nil
		}
		assignment.CreatedAt = m.now()
		assignment.UpdatedAt = assignment.CreatedAt
		m.assignments[assignment.ID] = assignment
		m.assignKeys[key] = assignment.ID
		return cloneAssignment(assignment), nil
	}
	assignment.CreatedAt = m.now()
	assignment.UpdatedAt = assignment.CreatedAt
	m.assignments[assignment.ID] = assignment
	return cloneAssignment(assignment), nil
}

func (m *MemoryWorkStore) GetAssignment(_ context.Context, tenantID, assignmentID uuid.UUID) (Assignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	assignment, ok := m.assignments[assignmentID]
	if !ok || assignment.TenantID != tenantID {
		return Assignment{}, newError(ErrAssignmentNotFound, CodeAssignmentNotFound, "get_assignment", "assignment not found")
	}
	return cloneAssignment(assignment), nil
}

func (m *MemoryWorkStore) ListAssignments(_ context.Context, tenantID, workItemID uuid.UUID) ([]Assignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listAssignmentsLocked(tenantID, workItemID), nil
}

// ListAssignmentsBySession lists one session's assignments across every
// WorkItem of the tenant. The binding guard uses it so a session index that
// never recorded a WorkItem cannot hide an existing binding.
func (m *MemoryWorkStore) ListAssignmentsBySession(_ context.Context, tenantID, sessionID uuid.UUID) ([]Assignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Assignment, 0)
	for _, assignment := range m.assignments {
		if assignment.TenantID != tenantID || assignment.SessionID != sessionID {
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

func (m *MemoryWorkStore) listAssignmentsLocked(tenantID, workItemID uuid.UUID) []Assignment {
	out := make([]Assignment, 0)
	for _, assignment := range m.assignments {
		if assignment.TenantID != tenantID || assignment.WorkItemID != workItemID {
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
	return out
}

func (m *MemoryWorkStore) UpdateAssignment(_ context.Context, assignment Assignment) (Assignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.assignments[assignment.ID]
	if !ok || existing.TenantID != assignment.TenantID {
		return Assignment{}, newError(ErrAssignmentNotFound, CodeAssignmentNotFound, "update_assignment", "assignment not found")
	}
	assignment.UpdatedAt = m.now()
	m.assignments[assignment.ID] = cloneAssignment(assignment)
	return cloneAssignment(assignment), nil
}

func (m *MemoryWorkStore) CreateAttention(_ context.Context, attention Attention) (Attention, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if attention.TenantID == uuid.Nil {
		return Attention{}, newError(ErrTenantRequired, CodeTenantRequired, "create_attention", "tenant is required")
	}
	if attention.RequestID != "" {
		key := attention.TenantID.String() + "|" + attention.WorkItemID.String() + "|" + attention.RequestID
		if existing, ok := m.attnKeys[key]; ok {
			return cloneAttention(m.attentions[existing]), nil
		}
		attention.CreatedAt = m.now()
		m.attentions[attention.ID] = attention
		m.attnKeys[key] = attention.ID
		return cloneAttention(attention), nil
	}
	attention.CreatedAt = m.now()
	m.attentions[attention.ID] = attention
	return cloneAttention(attention), nil
}

func (m *MemoryWorkStore) GetAttention(_ context.Context, tenantID, attentionID uuid.UUID) (Attention, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	attention, ok := m.attentions[attentionID]
	if !ok || attention.TenantID != tenantID {
		return Attention{}, newError(ErrAttentionNotFound, CodeAttentionNotFound, "get_attention", "attention not found")
	}
	return cloneAttention(attention), nil
}

func (m *MemoryWorkStore) UpdateAttention(_ context.Context, attention Attention) (Attention, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.attentions[attention.ID]
	if !ok || existing.TenantID != attention.TenantID {
		return Attention{}, newError(ErrAttentionNotFound, CodeAttentionNotFound, "update_attention", "attention not found")
	}
	m.attentions[attention.ID] = cloneAttention(attention)
	return cloneAttention(attention), nil
}

func (m *MemoryWorkStore) ListAttentions(_ context.Context, tenantID, workItemID uuid.UUID) ([]Attention, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Attention, 0)
	for _, attention := range m.attentions {
		if attention.TenantID != tenantID || attention.WorkItemID != workItemID {
			continue
		}
		out = append(out, cloneAttention(attention))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

func (m *MemoryWorkStore) AddLink(_ context.Context, link Link) (Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if link.TenantID == uuid.Nil || link.ID == uuid.Nil {
		return Link{}, newError(ErrTenantRequired, CodeTenantRequired, "add_link", "tenant and link ids are required")
	}
	for _, existing := range m.links {
		if existing.TenantID != link.TenantID {
			continue
		}
		if sameLinkRelation(existing, link) {
			// The relation already exists (possibly under another explicit id):
			// AddLink is idempotent and never creates a second row.
			return existing, nil
		}
		if existing.ID == link.ID {
			return Link{}, newError(ErrLinkConflict, CodeLinkConflict, "add_link",
				"link id names a different relation")
		}
	}
	link.CreatedAt = m.now()
	m.links[link.ID] = link
	return link, nil
}

func (m *MemoryWorkStore) GetLink(_ context.Context, tenantID, linkID uuid.UUID) (Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	link, ok := m.links[linkID]
	if !ok || link.TenantID != tenantID {
		return Link{}, newError(ErrLinkNotFound, CodeLinkNotFound, "get_link", "link not found")
	}
	return link, nil
}

func (m *MemoryWorkStore) ListLinks(_ context.Context, tenantID, workItemID uuid.UUID) ([]Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Link, 0)
	for _, link := range m.links {
		if link.TenantID != tenantID {
			continue
		}
		if link.WorkItemID != workItemID && !linkHasWorkItemEndpoint(link, workItemID) {
			continue
		}
		out = append(out, link)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

// sameLinkRelation compares the natural identity of two links: tenant, host
// WorkItem scope, kind and both directed endpoints. CreatedAt and an explicit
// id are not part of the relation.
func sameLinkRelation(a, b Link) bool {
	return a.TenantID == b.TenantID &&
		a.WorkItemID == b.WorkItemID &&
		a.Kind == b.Kind &&
		a.From == b.From &&
		a.To == b.To
}

// linkHasWorkItemEndpoint reports whether one endpoint names the WorkItem.
func linkHasWorkItemEndpoint(link Link, workItemID uuid.UUID) bool {
	id := workItemID.String()
	return link.From.Type == LinkEndpointWorkItem && link.From.ID == id ||
		link.To.Type == LinkEndpointWorkItem && link.To.ID == id
}

// MemorySessionIndex is the in-process SessionIndex used by tests and by the
// skeleton Host. Production mode changes replace it with a durable projection.
type MemorySessionIndex struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]SessionRef
}

// NewMemorySessionIndex builds an empty index.
func NewMemorySessionIndex() *MemorySessionIndex {
	return &MemorySessionIndex{sessions: make(map[uuid.UUID]SessionRef)}
}

func (m *MemorySessionIndex) Session(_ context.Context, tenantID, sessionID uuid.UUID) (SessionRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref, ok := m.sessions[sessionID]
	if !ok || ref.TenantID != tenantID {
		return SessionRef{}, newError(ErrSessionNotFound, CodeSessionNotFound, "session", "session not found")
	}
	return ref, nil
}

func (m *MemorySessionIndex) Put(_ context.Context, ref SessionRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ref.TenantID == uuid.Nil || ref.SessionID == uuid.Nil {
		return newError(ErrInvalidRequest, CodeInvalidRequest, "put_session", "tenant and session are required")
	}
	if _, exists := m.sessions[ref.SessionID]; exists {
		return newError(ErrSessionExists, CodeSessionExists, "put_session", "session is already tracked")
	}
	m.sessions[ref.SessionID] = ref
	return nil
}

func (m *MemorySessionIndex) SetState(_ context.Context, tenantID, sessionID uuid.UUID, state SessionState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref, ok := m.sessions[sessionID]
	if !ok || ref.TenantID != tenantID {
		return newError(ErrSessionNotFound, CodeSessionNotFound, "set_session_state", "session not found")
	}
	ref.State = state
	m.sessions[sessionID] = ref
	return nil
}

func (m *MemorySessionIndex) SetWorkItem(_ context.Context, tenantID, sessionID, workItemID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref, ok := m.sessions[sessionID]
	if !ok || ref.TenantID != tenantID {
		return newError(ErrSessionNotFound, CodeSessionNotFound, "set_session_work_item", "session not found")
	}
	ref.WorkItemID = workItemID
	m.sessions[sessionID] = ref
	return nil
}

func (m *MemorySessionIndex) Children(_ context.Context, tenantID, parentSessionID uuid.UUID) ([]SessionRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionRef, 0)
	for _, ref := range m.sessions {
		if ref.TenantID == tenantID && ref.ParentSessionID == parentSessionID {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].SessionID.String() < out[j].SessionID.String()
	})
	return out, nil
}

func (m *MemorySessionIndex) ActiveChildren(_ context.Context, tenantID, parentSessionID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, ref := range m.sessions {
		if ref.TenantID == tenantID && ref.ParentSessionID == parentSessionID && ref.State == SessionActive {
			count++
		}
	}
	return count, nil
}

func (m *MemorySessionIndex) ActiveInMode(_ context.Context, tenantID uuid.UUID, modeID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, ref := range m.sessions {
		if ref.TenantID == tenantID && ref.ModeID == modeID && ref.State == SessionActive {
			count++
		}
	}
	return count, nil
}

func cloneWorkItem(item WorkItem) WorkItem {
	copied := item
	copied.Input = cloneRaw(item.Input)
	copied.Scope = item.Scope.clone()
	return copied
}

func cloneAssignment(assignment Assignment) Assignment {
	copied := assignment
	copied.Input = cloneRaw(assignment.Input)
	copied.Result = cloneRaw(assignment.Result)
	return copied
}

func cloneAttention(attention Attention) Attention { return attention.clone() }
