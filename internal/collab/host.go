package collab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Audit event kinds written by the guarded host. They are structured
// (kind + result + reason + ids); no audit row contains prompt text or file
// contents.
const (
	AuditInvalidCreation   = "collab.invalid_creation"
	AuditPermissionDenied  = "permission.denied"
	AuditSessionCreating   = "collab.session_creating"
	AuditSessionCreated    = "collab.session_created"
	AuditSessionFailed     = "collab.session_create_failed"
	AuditSessionCompleted  = "collab.session_completed"
	AuditWorkOpened        = "collab.work_opened"
	AuditWorkCreateFailed  = "collab.work_create_failed"
	AuditAssignmentMade    = "collab.assignment_created"
	AuditAssignmentFailed  = "collab.assignment_create_failed"
	AuditAttentionOpened   = "collab.attention_opened"
	AuditAttentionFailed   = "collab.attention_publish_failed"
	AuditAttentionAnswered = "collab.attention_answered"
	AuditAttentionExpired  = "collab.attention_expired"
	AuditWorkCancelled     = "collab.work_cancelled"
	AuditCancelFailed      = "collab.cancel_failed"
)

// Default attention timeouts mirror the decision primitive.
const (
	DefaultAttentionTimeout = 5 * time.Minute
	MaxAttentionTimeout     = 24 * time.Hour
)

// AuditEvent is one structured collaboration audit row. Result is
// "accepted" | "denied" | "failed"; Reason is the stable ErrorCode.
type AuditEvent struct {
	Kind         string    `json:"kind"`
	Result       string    `json:"result"`
	Reason       string    `json:"reason,omitempty"`
	Operation    string    `json:"operation,omitempty"`
	TenantID     uuid.UUID `json:"tenantId"`
	ModeID       string    `json:"modeId,omitempty"`
	WorkItemID   uuid.UUID `json:"workItemId,omitempty"`
	AssignmentID uuid.UUID `json:"assignmentId,omitempty"`
	SessionID    uuid.UUID `json:"sessionId,omitempty"`
	// ParentSessionID names the caller/owner when the event is about a created
	// child session (SessionID then names the child only once it exists).
	ParentSessionID uuid.UUID `json:"parentSessionId,omitempty"`
	BotID           string    `json:"botId,omitempty"`
	Field           string    `json:"field,omitempty"`
	Limit           int64     `json:"limit,omitempty"`
	Actual          int64     `json:"actual,omitempty"`
	Detail          string    `json:"detail,omitempty"`
	At              time.Time `json:"at"`
}

// AuditSink records collaboration audit rows. A sink failure is fail-closed on
// every accepted write: the Host refuses to create work it cannot audit.
type AuditSink interface {
	Record(ctx context.Context, event AuditEvent) error
}

// PermissionChecker decides whether a caller session may invoke a target Bot.
// It is the only permission seam the Host needs; the product adapter maps it
// onto the same access bridge the tool surface uses, so a mode never gets an
// authority the Bot itself does not have.
type PermissionChecker interface {
	// CheckBotInvoke returns nil when allowed. Any error is a denial; the Host
	// does not inspect the error's shape.
	CheckBotInvoke(ctx context.Context, tenantID uuid.UUID, caller SessionRef, targetBotID string) error
}

// OperatorChecker is the independent operator/human seam for manual takeover of
// a disabled mode's existing work. The Host never trusts a caller-supplied
// "human takeover" flag: a product adapter decides from the caller's grants.
// Nil means no operator surface is wired, so disabled-mode assignment is denied.
type OperatorChecker interface {
	CheckOperator(ctx context.Context, tenantID uuid.UUID, caller SessionRef) error
}

// LaunchRequest is one guarded session creation handed to the product-layer
// launcher. The Host has already enforced tenant, permission, budget, depth,
// width and concurrency when the launcher sees it.
type LaunchRequest struct {
	TenantID        uuid.UUID       `json:"tenantId"`
	ModeID          string          `json:"modeId"`
	WorkItemID      uuid.UUID       `json:"workItemId,omitempty"`
	ParentSessionID uuid.UUID       `json:"parentSessionId"`
	BotID           string          `json:"botId"`
	Task            string          `json:"task"`
	Inputs          json.RawMessage `json:"inputs,omitempty"`
	IdempotencyKey  string          `json:"idempotencyKey,omitempty"`
	Budget          Budget          `json:"budget"`
	Depth           int             `json:"depth"`
}

// SessionLauncher is the product-layer adapter that actually creates a
// session. The existing SubagentRunner is the first adapter
// (harnessext.CollabSubagentLauncher); future modes provide their own.
type SessionLauncher interface {
	Launch(ctx context.Context, req LaunchRequest) (SessionRef, error)
	CancelSession(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) error
}

// AttentionPublisher optionally delivers a stored Attention to its route
// (decision broker, notification, external channel). Nil keeps the row
// readable but un-routed; the mode change that needs routing wires it.
type AttentionPublisher interface {
	Publish(ctx context.Context, attention Attention) error
}

// SessionRequest asks the Host for one mode session.
type SessionRequest struct {
	// WorkItemID is optional for a bare delegation. A non-zero id requires an
	// AssignmentID: a WorkItem-scoped session that is not bound to an
	// assignment could escape cancellation and manual takeover.
	WorkItemID uuid.UUID `json:"workItemId,omitempty"`
	// ParentSessionID is the caller/owner session. Every mode-created session
	// has one; root sessions are created by the product, not by a mode.
	ParentSessionID uuid.UUID `json:"parentSessionId"`
	// BotID is the target Bot. Empty inherits the parent session's Bot.
	BotID string `json:"botId,omitempty"`
	// Task is the initial instruction committed with the session.
	Task string `json:"task"`
	// Inputs is the mode-specific structured input (never a permission grant).
	Inputs json.RawMessage `json:"inputs,omitempty"`
	// AssignmentID binds the new session to a WorkItem assignment.
	AssignmentID uuid.UUID `json:"assignmentId,omitempty"`
	// IdempotencyKey makes a retried creation target the same session.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// Budget narrows the effective mode limits. Zero fields inherit.
	Budget Budget `json:"budget,omitempty"`
}

// AssignmentRequest asks the Host to bind a WorkItem to a Bot/Session.
type AssignmentRequest struct {
	// CallerSessionID is the acting session; the target Bot's bot.invoke and
	// WorkItem membership are checked against it. Required.
	CallerSessionID uuid.UUID `json:"callerSessionId"`
	// SessionID optionally binds an already-created worker session.
	SessionID uuid.UUID `json:"sessionId,omitempty"`
	// Title/Input describe the assignment (a ticket comment, a retry reason).
	Title          string          `json:"title,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
}

// AttentionRequest asks for one structured human decision.
type AttentionRequest struct {
	// CallerSessionID is the acting worker/orchestrator session. Required.
	CallerSessionID uuid.UUID `json:"callerSessionId"`
	// AssignmentID optionally ties the question to one assignment.
	AssignmentID uuid.UUID `json:"assignmentId,omitempty"`
	// RequestID makes the open idempotent (decision semantics).
	RequestID string `json:"requestId,omitempty"`
	// Kind defaults to "decision"; clarify/approval are the other built-ins.
	Kind          string            `json:"kind,omitempty"`
	Prompt        string            `json:"prompt"`
	Options       []AttentionOption `json:"options,omitempty"`
	AllowFreeText bool              `json:"allowFreeText"`
	Route         AttentionRoute    `json:"route,omitempty"`
	Channel       string            `json:"channel,omitempty"`
	Timeout       time.Duration     `json:"timeout,omitempty"`
}

// AttentionAnswerRequest answers one open attention.
type AttentionAnswerRequest struct {
	OptionID string `json:"optionId,omitempty"`
	Text     string `json:"text,omitempty"`
	// Actor is the authenticated operator/human identity. The product adapter
	// resolves it from the caller, never from the answer body.
	Actor string `json:"actor,omitempty"`
}

// WorkItemRequest asks the Host to open one WorkItem.
type WorkItemRequest struct {
	// CallerSessionID is the owning/main session. It becomes the bootstrap
	// member for the first Assign/Ask.
	CallerSessionID uuid.UUID       `json:"callerSessionId"`
	Title           string          `json:"title"`
	Input           json.RawMessage `json:"input,omitempty"`
	Source          SourceRef       `json:"source,omitempty"`
	Scope           Scope           `json:"scope,omitempty"`
	Limits          Limits          `json:"limits,omitempty"`
	IdempotencyKey  string          `json:"idempotencyKey,omitempty"`
}

// Host is the single creation gate a mode may use. Creating a session any
// other way (importing kernel/store) is a contract violation.
type Host interface {
	CreateSession(ctx context.Context, tenantID uuid.UUID, modeID string, req SessionRequest) (SessionRef, error)
	Assign(ctx context.Context, tenantID, workItemID uuid.UUID, botID string, req AssignmentRequest) (Assignment, error)
	Ask(ctx context.Context, tenantID, workItemID uuid.UUID, req AttentionRequest) (Attention, error)
	Cancel(ctx context.Context, tenantID, workItemID uuid.UUID, reason string) error
}

// HostOptions configures the guarded Coordinator. Every seam except Operators
// and Attention is required: a half-armed Host must fail at construction.
type HostOptions struct {
	TenantID    uuid.UUID
	Modes       *ModeRegistry
	Work        WorkStore
	Sessions    SessionIndex
	Permissions PermissionChecker
	// Operators is the manual-takeover seam. Nil denies disabled-mode
	// assignment (there is no self-asserted takeover flag).
	Operators OperatorChecker
	Audit     AuditSink
	Launcher  SessionLauncher
	// Attention routes stored attention requests. Nil keeps them readable but
	// un-routed (the read API still works).
	Attention AttentionPublisher
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Coordinator implements Host and the work/attention read-write projections.
// It is the only type in this package that performs guards.
type Coordinator struct {
	tenantID    uuid.UUID
	modes       *ModeRegistry
	work        WorkStore
	sessions    SessionIndex
	permissions PermissionChecker
	operators   OperatorChecker
	audit       AuditSink
	launcher    SessionLauncher
	attention   AttentionPublisher
	now         func() time.Time

	mu sync.Mutex
	// idem maps a session idempotency key to the creation in flight or done.
	idem map[string]*sessionCreation
	// pendingChildren/pendingModes count reserved-but-not-yet-indexed sessions,
	// so the width/concurrency check and reservation are one atomic step.
	pendingChildren map[uuid.UUID]int
	pendingModes    map[string]int
	keyLocks        keyedLocker
}

type sessionCreation struct {
	ref          SessionRef
	reservation  reservation
	indexed      bool
	assignmentID uuid.UUID
	workItemID   uuid.UUID
	complete     bool
}

type reservation struct {
	parentID uuid.UUID
	modeID   string
	active   bool
}

var _ Host = (*Coordinator)(nil)

// NewHost builds the guarded Host. Missing seams are construction errors. The
// mode registry must belong to the same tenant as the Host.
func NewHost(opts HostOptions) (*Coordinator, error) {
	switch {
	case opts.TenantID == uuid.Nil:
		return nil, newError(ErrTenantRequired, CodeTenantRequired, "new_host", "tenant is required")
	case opts.Modes == nil:
		return nil, newError(ErrInvalidRequest, CodeInvalidRequest, "new_host", "mode registry is required")
	case opts.Work == nil:
		return nil, newError(ErrInvalidRequest, CodeInvalidRequest, "new_host", "work store is required")
	case opts.Sessions == nil:
		return nil, newError(ErrInvalidRequest, CodeInvalidRequest, "new_host", "session index is required")
	case opts.Permissions == nil:
		return nil, newError(ErrInvalidRequest, CodeInvalidRequest, "new_host", "permission checker is required")
	case opts.Audit == nil:
		return nil, newError(ErrInvalidRequest, CodeInvalidRequest, "new_host", "audit sink is required")
	case opts.Launcher == nil:
		return nil, newError(ErrInvalidRequest, CodeInvalidRequest, "new_host", "session launcher is required")
	}
	if registryTenant := opts.Modes.TenantID(); registryTenant != opts.TenantID {
		return nil, &Error{Kind: ErrTenantMismatch, Code: CodeTenantMismatch, Operation: "new_host",
			TenantID: opts.TenantID, Detail: "the mode registry belongs to a different tenant"}
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Coordinator{
		tenantID:        opts.TenantID,
		modes:           opts.Modes,
		work:            opts.Work,
		sessions:        opts.Sessions,
		permissions:     opts.Permissions,
		operators:       opts.Operators,
		audit:           opts.Audit,
		launcher:        opts.Launcher,
		attention:       opts.Attention,
		now:             opts.Now,
		idem:            make(map[string]*sessionCreation),
		pendingChildren: make(map[uuid.UUID]int),
		pendingModes:    make(map[string]int),
	}, nil
}

// OpenWorkItem creates one WorkItem under an enabled mode. The caller becomes
// the work item's owner session and the bootstrap member for Assign/Ask.
func (c *Coordinator) OpenWorkItem(ctx context.Context, tenantID uuid.UUID, modeID string, req WorkItemRequest) (WorkItem, error) {
	const operation = "open_work_item"
	modeID = strings.TrimSpace(modeID)
	if err := c.checkTenant(tenantID); err != nil {
		return WorkItem{}, c.reject(ctx, operation, err, modeID)
	}
	descriptor, err := c.modes.RequireEnabled(modeID)
	if err != nil {
		return WorkItem{}, c.reject(ctx, operation, err, modeID)
	}
	title := strings.TrimSpace(req.Title)
	if title == "" && !hasJSON(req.Input) {
		return WorkItem{}, c.reject(ctx, operation, newError(ErrInvalidRequest, CodeInvalidRequest, operation,
			"title or input is required"), modeID)
	}
	if err := req.Limits.Validate(); err != nil {
		return WorkItem{}, c.reject(ctx, operation, &Error{Kind: ErrInvalidRequest, Code: CodeInvalidRequest, Operation: operation, Detail: err.Error()}, modeID)
	}
	if violation := req.Limits.Exceeds(descriptor.Limits); violation != nil {
		refusal := newError(ErrLimitExceeded, CodeLimitExceeded, operation, violation.String())
		refusal.ModeID = modeID
		refusal.Field = violation.Field
		refusal.Limit = violation.Limit
		refusal.Actual = violation.Requested
		return WorkItem{}, c.rejectWith(ctx, refusal)
	}
	caller, err := c.activeSession(ctx, tenantID, req.CallerSessionID, operation)
	if err != nil {
		return WorkItem{}, c.reject(ctx, operation, err, modeID)
	}
	source := req.Source
	if strings.TrimSpace(source.Kind) == "" {
		source.Kind = "manual"
	}
	now := c.now()
	item := WorkItem{
		ID:             workItemID(tenantID, modeID, caller.SessionID, strings.TrimSpace(req.IdempotencyKey)),
		TenantID:       tenantID,
		ModeID:         modeID,
		Title:          title,
		Input:          cloneRaw(req.Input),
		Source:         source,
		Scope:          req.Scope.clone(),
		Limits:         req.Limits.NarrowedBy(descriptor.Limits),
		Status:         WorkItemOpen,
		OwnerSessionID: caller.SessionID,
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	// The accepted audit is the authorization fact and must be writable before
	// the row exists. A sink failure refuses the write instead of creating
	// un-audited work.
	if err := c.record(ctx, AuditEvent{
		Kind: AuditWorkOpened, Result: "accepted", Operation: operation,
		ModeID: modeID, WorkItemID: item.ID, ParentSessionID: caller.SessionID,
	}); err != nil {
		return WorkItem{}, &Error{Kind: ErrAuditUnavailable, Code: CodeAuditUnavailable, Operation: operation,
			ModeID: modeID, WorkItemID: item.ID, SessionID: caller.SessionID, Detail: "the audit row could not be written"}
	}
	stored, err := c.work.CreateWorkItem(ctx, item)
	if err != nil {
		_ = c.record(ctx, AuditEvent{Kind: AuditWorkCreateFailed, Result: "failed", Operation: operation,
			ModeID: modeID, WorkItemID: item.ID, ParentSessionID: caller.SessionID, Detail: err.Error()})
		return WorkItem{}, err
	}
	return stored, nil
}

// WorkItem reads one WorkItem. Reads stay available after a mode is disabled.
func (c *Coordinator) WorkItem(ctx context.Context, tenantID, workItemID uuid.UUID) (WorkItem, error) {
	if err := c.checkTenant(tenantID); err != nil {
		return WorkItem{}, err
	}
	if workItemID == uuid.Nil {
		return WorkItem{}, newError(ErrInvalidRequest, CodeInvalidRequest, "get_work_item", "work item id is required")
	}
	return c.work.GetWorkItem(ctx, tenantID, workItemID)
}

// WorkItems lists the tenant's WorkItems. A disabled mode's items remain
// visible: disabling stops new work, it does not erase facts.
func (c *Coordinator) WorkItems(ctx context.Context, tenantID uuid.UUID, filter WorkFilter) ([]WorkItem, error) {
	if err := c.checkTenant(tenantID); err != nil {
		return nil, err
	}
	return c.work.ListWorkItems(ctx, tenantID, filter)
}

// Assignments lists one WorkItem's assignments. It is a read of existing
// bindings, independent of the mode's enabled state and of any Session's
// lifecycle.
func (c *Coordinator) Assignments(ctx context.Context, tenantID, workItemID uuid.UUID) ([]Assignment, error) {
	if _, err := c.WorkItem(ctx, tenantID, workItemID); err != nil {
		return nil, err
	}
	return c.work.ListAssignments(ctx, tenantID, workItemID)
}

// Attentions lists one WorkItem's attention requests.
func (c *Coordinator) Attentions(ctx context.Context, tenantID, workItemID uuid.UUID) ([]Attention, error) {
	if _, err := c.WorkItem(ctx, tenantID, workItemID); err != nil {
		return nil, err
	}
	return c.work.ListAttentions(ctx, tenantID, workItemID)
}

// Links lists one WorkItem's semantic links.
func (c *Coordinator) Links(ctx context.Context, tenantID, workItemID uuid.UUID) ([]Link, error) {
	if _, err := c.WorkItem(ctx, tenantID, workItemID); err != nil {
		return nil, err
	}
	return c.work.ListLinks(ctx, tenantID, workItemID)
}

// AddLink records one semantic relation. The endpoints are records, not a
// parent/child assumption.
func (c *Coordinator) AddLink(ctx context.Context, tenantID uuid.UUID, link Link) (Link, error) {
	const operation = "add_link"
	if err := c.checkTenant(tenantID); err != nil {
		return Link{}, c.reject(ctx, operation, err, "")
	}
	if !link.Kind.Valid() {
		return Link{}, c.reject(ctx, operation, newError(ErrInvalidRequest, CodeInvalidRequest, operation,
			"link kind "+string(link.Kind)+" is not in the closed vocabulary"), "")
	}
	if err := validateLinkEndpoint(link.From); err != nil {
		return Link{}, c.reject(ctx, operation, err, "")
	}
	if err := validateLinkEndpoint(link.To); err != nil {
		return Link{}, c.reject(ctx, operation, err, "")
	}
	if link.WorkItemID != uuid.Nil {
		if _, err := c.WorkItem(ctx, tenantID, link.WorkItemID); err != nil {
			return Link{}, err
		}
	}
	link.TenantID = tenantID
	if link.ID == uuid.Nil {
		link.ID = uuid.New()
	}
	link.CreatedAt = c.now()
	return c.work.AddLink(ctx, link)
}

// CreateSession is the single controlled creation gate. A repeated
// idempotency key returns the same session, including retries after a partial
// bookkeeping failure, without re-running permission or limit checks.
func (c *Coordinator) CreateSession(ctx context.Context, tenantID uuid.UUID, modeID string, req SessionRequest) (SessionRef, error) {
	const operation = "create_session"
	modeID = strings.TrimSpace(modeID)
	if err := c.checkTenant(tenantID); err != nil {
		return SessionRef{}, c.reject(ctx, operation, err, modeID)
	}
	key := sessionIdempotencyKey(tenantID, modeID, req)
	if key != "" {
		unlock := c.keyLocks.lock(key)
		defer unlock()
		if entry, ok := c.creation(key); ok {
			return c.resumeSession(ctx, tenantID, modeID, entry)
		}
	}

	descriptor, err := c.modes.RequireEnabled(modeID)
	if err != nil {
		return SessionRef{}, c.reject(ctx, operation, err, modeID)
	}
	req.Task = strings.TrimSpace(req.Task)
	if req.Task == "" {
		return SessionRef{}, c.reject(ctx, operation, newError(ErrInvalidCreation, CodeInvalidCreation, operation,
			"task is required"), modeID)
	}
	if err := req.Budget.Validate(); err != nil {
		return SessionRef{}, c.reject(ctx, operation, &Error{Kind: ErrInvalidCreation, Code: CodeInvalidCreation, Operation: operation, Detail: err.Error()}, modeID)
	}

	workItem := WorkItem{}
	if req.WorkItemID != uuid.Nil {
		if req.AssignmentID == uuid.Nil {
			refusal := newError(ErrInvalidCreation, CodeInvalidCreation, operation,
				"a WorkItem-scoped session must bind an assignment")
			refusal.ModeID = modeID
			refusal.WorkItemID = req.WorkItemID
			return SessionRef{}, c.rejectWith(ctx, refusal)
		}
		item, err := c.work.GetWorkItem(ctx, tenantID, req.WorkItemID)
		if err != nil {
			return SessionRef{}, c.reject(ctx, operation, err, modeID)
		}
		if item.ModeID != descriptor.ID {
			refusal := newError(ErrInvalidCreation, CodeInvalidCreation, operation,
				fmt.Sprintf("work item mode %q does not match mode %q", item.ModeID, descriptor.ID))
			refusal.ModeID = modeID
			refusal.WorkItemID = item.ID
			return SessionRef{}, c.rejectWith(ctx, refusal)
		}
		if item.Status.Closed() {
			refusal := newError(ErrWorkItemClosed, CodeWorkItemClosed, operation, "work item "+item.ID.String()+" is closed")
			refusal.ModeID = modeID
			refusal.WorkItemID = item.ID
			return SessionRef{}, c.rejectWith(ctx, refusal)
		}
		workItem = item
	}

	effective := descriptor.Limits.NarrowedBy(workItem.Limits)
	budget, budgetErr := effectiveBudget(req.Budget, effective)
	if budgetErr != nil {
		budgetErr.ModeID = modeID
		budgetErr.WorkItemID = workItem.ID
		return SessionRef{}, c.rejectWith(ctx, budgetErr)
	}

	if req.ParentSessionID == uuid.Nil {
		refusal := newError(ErrInvalidCreation, CodeInvalidCreation, operation,
			"parent session is required")
		refusal.ModeID = modeID
		refusal.WorkItemID = workItem.ID
		return SessionRef{}, c.rejectWith(ctx, refusal)
	}
	parent, err := c.sessions.Session(ctx, tenantID, req.ParentSessionID)
	if err != nil {
		refusal := newError(ErrParentSessionNotFound, CodeParentNotFound, operation, "parent session not found")
		refusal.ModeID = modeID
		refusal.WorkItemID = workItem.ID
		return SessionRef{}, c.rejectWith(ctx, refusal)
	}
	if parent.State != SessionActive {
		refusal := newError(ErrParentSessionInactive, CodeParentInactive, operation, "parent session is not active")
		refusal.ModeID = modeID
		refusal.SessionID = parent.SessionID
		refusal.WorkItemID = workItem.ID
		return SessionRef{}, c.rejectWith(ctx, refusal)
	}
	if workItem.ID != uuid.Nil {
		member, err := c.isWorkMember(ctx, tenantID, workItem, parent)
		if err != nil {
			return SessionRef{}, err
		}
		if !member {
			refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation,
				"the parent session is not a member of the work item")
			refusal.ModeID = modeID
			refusal.SessionID = parent.SessionID
			refusal.WorkItemID = workItem.ID
			return SessionRef{}, c.rejectWith(ctx, refusal)
		}
	}

	depth := parent.Depth + 1
	if !effective.Depth.Allows(depth) {
		refusal := newError(ErrDepthExceeded, CodeDepthExceeded, operation,
			fmt.Sprintf("child depth %d is not allowed by the %s depth policy", depth, effective.Depth.Mode))
		refusal.ModeID = modeID
		refusal.SessionID = parent.SessionID
		refusal.WorkItemID = workItem.ID
		refusal.Field = "depth"
		if effective.Depth.Mode == DepthLimited {
			refusal.Limit = int64(effective.Depth.Max)
		}
		refusal.Actual = int64(depth)
		return SessionRef{}, c.rejectWith(ctx, refusal)
	}

	botID := strings.TrimSpace(req.BotID)
	if botID == "" {
		botID = parent.BotID
	}
	if botID == "" {
		refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation,
			"the session has no Bot identity to delegate to")
		refusal.ModeID = modeID
		refusal.SessionID = parent.SessionID
		refusal.WorkItemID = workItem.ID
		return SessionRef{}, c.rejectWith(ctx, refusal)
	}
	if workItem.ID != uuid.Nil && !scopeAllowsBot(workItem.Scope, botID) {
		refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation,
			"the target Bot is outside the work item's scope")
		refusal.ModeID = modeID
		refusal.SessionID = parent.SessionID
		refusal.WorkItemID = workItem.ID
		refusal.BotID = botID
		return SessionRef{}, c.rejectWith(ctx, refusal)
	}
	if err := c.permissions.CheckBotInvoke(ctx, tenantID, parent, botID); err != nil {
		refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation, "bot.invoke denied")
		refusal.ModeID = modeID
		refusal.SessionID = parent.SessionID
		refusal.WorkItemID = workItem.ID
		refusal.BotID = botID
		return SessionRef{}, c.rejectWith(ctx, refusal)
	}

	assignment := Assignment{}
	if req.AssignmentID != uuid.Nil {
		stored, err := c.work.GetAssignment(ctx, tenantID, req.AssignmentID)
		if err != nil {
			return SessionRef{}, c.reject(ctx, operation, err, modeID)
		}
		if stored.WorkItemID != req.WorkItemID {
			refusal := newError(ErrInvalidCreation, CodeInvalidCreation, operation,
				"assignment does not belong to the work item")
			refusal.ModeID = modeID
			refusal.WorkItemID = req.WorkItemID
			refusal.AssignmentID = stored.ID
			return SessionRef{}, c.rejectWith(ctx, refusal)
		}
		if stored.BotID != "" && stored.BotID != botID {
			refusal := newError(ErrInvalidCreation, CodeInvalidCreation, operation,
				"assignment targets a different Bot")
			refusal.ModeID = modeID
			refusal.WorkItemID = req.WorkItemID
			refusal.AssignmentID = stored.ID
			refusal.BotID = botID
			return SessionRef{}, c.rejectWith(ctx, refusal)
		}
		if stored.SessionID != uuid.Nil {
			refusal := newError(ErrInvalidCreation, CodeInvalidCreation, operation,
				"assignment is already bound to a session")
			refusal.ModeID = modeID
			refusal.WorkItemID = req.WorkItemID
			refusal.AssignmentID = stored.ID
			refusal.SessionID = stored.SessionID
			return SessionRef{}, c.rejectWith(ctx, refusal)
		}
		assignment = stored
	}

	reserved, err := c.reserve(ctx, tenantID, modeID, parent.SessionID, effective)
	if err != nil {
		return SessionRef{}, c.reject(ctx, operation, err, modeID)
	}

	launch := LaunchRequest{
		TenantID:        tenantID,
		ModeID:          descriptor.ID,
		WorkItemID:      req.WorkItemID,
		ParentSessionID: parent.SessionID,
		BotID:           botID,
		Task:            req.Task,
		Inputs:          cloneRaw(req.Inputs),
		IdempotencyKey:  strings.TrimSpace(req.IdempotencyKey),
		Budget:          budget,
		Depth:           depth,
	}
	if err := c.record(ctx, AuditEvent{
		Kind: AuditSessionCreating, Result: "accepted", Operation: operation,
		ModeID: descriptor.ID, WorkItemID: req.WorkItemID, ParentSessionID: parent.SessionID, BotID: botID,
		Actual: int64(depth),
	}); err != nil {
		c.release(&reserved)
		return SessionRef{}, &Error{Kind: ErrAuditUnavailable, Code: CodeAuditUnavailable, Operation: operation,
			ModeID: descriptor.ID, WorkItemID: req.WorkItemID, SessionID: parent.SessionID, BotID: botID,
			Detail: "the audit row could not be written"}
	}

	ref, err := c.launcher.Launch(ctx, launch)
	if err != nil {
		c.release(&reserved)
		_ = c.record(ctx, AuditEvent{
			Kind: AuditSessionFailed, Result: "failed", Operation: operation,
			ModeID: descriptor.ID, WorkItemID: req.WorkItemID, ParentSessionID: parent.SessionID, BotID: botID,
			Detail: err.Error(),
		})
		return SessionRef{}, err
	}
	if ref.TenantID == uuid.Nil {
		ref.TenantID = tenantID
	}
	if ref.TenantID != tenantID || ref.SessionID == uuid.Nil {
		c.release(&reserved)
		_ = c.record(ctx, AuditEvent{
			Kind: AuditSessionFailed, Result: "failed", Operation: operation,
			ModeID: descriptor.ID, WorkItemID: req.WorkItemID, ParentSessionID: parent.SessionID, BotID: botID,
			Detail: "the launcher returned an invalid session reference",
		})
		return SessionRef{}, newError(ErrInvalidCreation, CodeInvalidCreation, operation,
			"the launcher returned an invalid session reference")
	}
	ref.ParentSessionID = parent.SessionID
	ref.BotID = botID
	ref.ModeID = descriptor.ID
	ref.WorkItemID = req.WorkItemID
	ref.Depth = depth
	ref.State = SessionActive
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = c.now()
	}

	entry := &sessionCreation{
		ref:          ref,
		reservation:  reserved,
		assignmentID: assignment.ID,
		workItemID:   req.WorkItemID,
	}
	if key != "" {
		c.storeCreation(key, entry)
	}
	if err := c.finishCreation(ctx, tenantID, entry); err != nil {
		if key == "" {
			// No retry identity: do not leave an untracked active session
			// consuming capacity. Best effort, the failure is still returned.
			_ = c.launcher.CancelSession(ctx, tenantID, ref.SessionID, "collab bookkeeping failure")
			_ = c.sessions.SetState(ctx, tenantID, ref.SessionID, SessionCancelled)
			c.release(&entry.reservation)
		}
		_ = c.record(ctx, AuditEvent{
			Kind: AuditSessionFailed, Result: "failed", Operation: operation,
			ModeID: descriptor.ID, WorkItemID: req.WorkItemID, AssignmentID: assignment.ID,
			SessionID: ref.SessionID, BotID: botID, Detail: err.Error(),
		})
		return SessionRef{}, err
	}
	return ref, nil
}

// resumeSession completes a stored creation. It never launches a second
// session and never re-runs the guards: the session already exists.
func (c *Coordinator) resumeSession(ctx context.Context, tenantID uuid.UUID, modeID string, entry *sessionCreation) (SessionRef, error) {
	if entry.ref.TenantID != tenantID || entry.ref.ModeID != modeID {
		return SessionRef{}, newError(ErrInvalidCreation, CodeInvalidCreation, "create_session",
			"the idempotency key belongs to a different tenant/mode")
	}
	if entry.complete {
		return entry.ref, nil
	}
	if err := c.finishCreation(ctx, tenantID, entry); err != nil {
		return SessionRef{}, err
	}
	return entry.ref, nil
}

// finishCreation performs the bookkeeping after a launch: index the session,
// bind the assignment, activate the work item, then release the reservation.
// Each step is idempotent so a retry after a partial failure resumes.
func (c *Coordinator) finishCreation(ctx context.Context, tenantID uuid.UUID, entry *sessionCreation) error {
	if !entry.indexed {
		err := c.sessions.Put(ctx, entry.ref)
		switch {
		case err == nil:
			entry.indexed = true
		case errors.Is(err, ErrSessionExists):
			existing, getErr := c.sessions.Session(ctx, tenantID, entry.ref.SessionID)
			if getErr != nil || existing.SessionID != entry.ref.SessionID {
				return err
			}
			entry.indexed = true
		default:
			return err
		}
	}
	if entry.assignmentID != uuid.Nil {
		assignment, err := c.work.GetAssignment(ctx, tenantID, entry.assignmentID)
		if err != nil {
			return err
		}
		if assignment.SessionID != uuid.Nil && assignment.SessionID != entry.ref.SessionID {
			return newError(ErrInvalidCreation, CodeInvalidCreation, "create_session",
				"assignment is already bound to another session")
		}
		assignment.SessionID = entry.ref.SessionID
		assignment.Status = AssignmentActive
		assignment.UpdatedAt = c.now()
		if _, err := c.work.UpdateAssignment(ctx, assignment); err != nil {
			return err
		}
	}
	if entry.workItemID != uuid.Nil {
		item, err := c.work.GetWorkItem(ctx, tenantID, entry.workItemID)
		if err != nil {
			return err
		}
		if item.Status == WorkItemOpen {
			item.Status = WorkItemActive
			item.UpdatedAt = c.now()
			if _, err := c.work.UpdateWorkItem(ctx, item); err != nil {
				return err
			}
		}
	}
	c.release(&entry.reservation)
	entry.complete = true
	_ = c.record(ctx, AuditEvent{
		Kind: AuditSessionCreated, Result: "accepted", Operation: "create_session",
		ModeID: entry.ref.ModeID, WorkItemID: entry.workItemID, AssignmentID: entry.assignmentID,
		SessionID: entry.ref.SessionID, ParentSessionID: entry.ref.ParentSessionID,
		BotID: entry.ref.BotID, Actual: int64(entry.ref.Depth),
	})
	return nil
}

// Assign binds a WorkItem to a Bot (and optionally a live Session). The
// assignment is not owned by the Session. Manual takeover of a disabled mode
// requires the independent operator seam; there is no caller-supplied flag.
func (c *Coordinator) Assign(ctx context.Context, tenantID, workItemID uuid.UUID, botID string, req AssignmentRequest) (Assignment, error) {
	const operation = "assign"
	if err := c.checkTenant(tenantID); err != nil {
		return Assignment{}, c.reject(ctx, operation, err, "")
	}
	item, err := c.work.GetWorkItem(ctx, tenantID, workItemID)
	if err != nil {
		return Assignment{}, c.reject(ctx, operation, err, "")
	}
	if _, known := c.modes.Descriptor(item.ModeID); !known {
		refusal := newError(ErrUnknownMode, CodeUnknownMode, operation, "the work item's mode is not registered")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Assignment{}, c.rejectWith(ctx, refusal)
	}
	if item.Status.Closed() {
		refusal := newError(ErrWorkItemClosed, CodeWorkItemClosed, operation, "the work item is closed")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Assignment{}, c.rejectWith(ctx, refusal)
	}
	if req.CallerSessionID == uuid.Nil {
		refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation,
			"an assignment needs an acting session")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Assignment{}, c.rejectWith(ctx, refusal)
	}
	caller, err := c.sessions.Session(ctx, tenantID, req.CallerSessionID)
	if err != nil {
		refusal := newError(ErrSessionNotFound, CodeSessionNotFound, operation, "caller session not found")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Assignment{}, c.rejectWith(ctx, refusal)
	}
	if caller.State != SessionActive {
		refusal := newError(ErrParentSessionInactive, CodeParentInactive, operation, "caller session is not active")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		refusal.SessionID = caller.SessionID
		return Assignment{}, c.rejectWith(ctx, refusal)
	}
	member, err := c.isWorkMember(ctx, tenantID, item, caller)
	if err != nil {
		return Assignment{}, err
	}
	if c.modes.Enabled(item.ModeID) {
		// Normal dispatch is restricted to the work item's sessions.
		if !member {
			refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation,
				"the caller session is not a member of the work item")
			refusal.ModeID = item.ModeID
			refusal.WorkItemID = item.ID
			refusal.SessionID = caller.SessionID
			return Assignment{}, c.rejectWith(ctx, refusal)
		}
	} else {
		// Manual takeover is an operator action; the operator seam may take
		// over even when the operator's own session is not a member yet.
		if c.operators == nil {
			refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation,
				"the mode is disabled and no operator seam is wired")
			refusal.ModeID = item.ModeID
			refusal.WorkItemID = item.ID
			refusal.SessionID = caller.SessionID
			return Assignment{}, c.rejectWith(ctx, refusal)
		}
		if err := c.operators.CheckOperator(ctx, tenantID, caller); err != nil {
			refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation,
				"the mode is disabled; operator permission is required for manual takeover")
			refusal.ModeID = item.ModeID
			refusal.WorkItemID = item.ID
			refusal.SessionID = caller.SessionID
			return Assignment{}, c.rejectWith(ctx, refusal)
		}
	}
	botID = strings.TrimSpace(botID)
	if botID == "" {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "botId is required")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Assignment{}, c.rejectWith(ctx, refusal)
	}
	if !scopeAllowsBot(item.Scope, botID) {
		refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation,
			"the target Bot is outside the work item's scope")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		refusal.SessionID = caller.SessionID
		refusal.BotID = botID
		return Assignment{}, c.rejectWith(ctx, refusal)
	}
	if err := c.permissions.CheckBotInvoke(ctx, tenantID, caller, botID); err != nil {
		refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation, "bot.invoke denied")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		refusal.SessionID = caller.SessionID
		refusal.BotID = botID
		return Assignment{}, c.rejectWith(ctx, refusal)
	}
	status := AssignmentOpen
	if req.SessionID != uuid.Nil {
		worker, err := c.sessions.Session(ctx, tenantID, req.SessionID)
		if err != nil {
			refusal := newError(ErrSessionNotFound, CodeSessionNotFound, operation, "worker session not found")
			refusal.ModeID = item.ModeID
			refusal.WorkItemID = item.ID
			refusal.SessionID = req.SessionID
			return Assignment{}, c.rejectWith(ctx, refusal)
		}
		if worker.State != SessionActive {
			refusal := newError(ErrParentSessionInactive, CodeParentInactive, operation, "worker session is not active")
			refusal.ModeID = item.ModeID
			refusal.WorkItemID = item.ID
			refusal.SessionID = req.SessionID
			return Assignment{}, c.rejectWith(ctx, refusal)
		}
		if worker.WorkItemID != uuid.Nil && worker.WorkItemID != item.ID {
			refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation,
				"worker session belongs to a different work item")
			refusal.ModeID = item.ModeID
			refusal.WorkItemID = item.ID
			refusal.SessionID = req.SessionID
			return Assignment{}, c.rejectWith(ctx, refusal)
		}
		if worker.BotID != "" && worker.BotID != botID {
			refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation,
				"worker session belongs to a different Bot")
			refusal.ModeID = item.ModeID
			refusal.WorkItemID = item.ID
			refusal.SessionID = req.SessionID
			refusal.BotID = botID
			return Assignment{}, c.rejectWith(ctx, refusal)
		}
		status = AssignmentActive
	}
	now := c.now()
	key := strings.TrimSpace(req.IdempotencyKey)
	assignment := Assignment{
		ID:              assignmentID(tenantID, item.ID, key),
		TenantID:        tenantID,
		WorkItemID:      item.ID,
		ModeID:          item.ModeID,
		BotID:           botID,
		SessionID:       req.SessionID,
		CallerSessionID: caller.SessionID,
		Status:          status,
		Title:           strings.TrimSpace(req.Title),
		Input:           cloneRaw(req.Input),
		IdempotencyKey:  key,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := c.record(ctx, AuditEvent{
		Kind: AuditAssignmentMade, Result: "accepted", Operation: operation,
		ModeID: item.ModeID, WorkItemID: item.ID, AssignmentID: assignment.ID,
		ParentSessionID: caller.SessionID, BotID: botID,
	}); err != nil {
		return Assignment{}, &Error{Kind: ErrAuditUnavailable, Code: CodeAuditUnavailable, Operation: operation,
			ModeID: item.ModeID, WorkItemID: item.ID, AssignmentID: assignment.ID,
			SessionID: caller.SessionID, BotID: botID, Detail: "the audit row could not be written"}
	}
	stored, err := c.work.CreateAssignment(ctx, assignment)
	if err != nil {
		_ = c.record(ctx, AuditEvent{
			Kind: AuditAssignmentFailed, Result: "failed", Operation: operation,
			ModeID: item.ModeID, WorkItemID: item.ID, AssignmentID: assignment.ID,
			ParentSessionID: caller.SessionID, BotID: botID, Detail: err.Error(),
		})
		return Assignment{}, err
	}
	if stored.SessionID != uuid.Nil {
		if err := c.sessions.SetWorkItem(ctx, tenantID, stored.SessionID, item.ID); err != nil {
			return Assignment{}, err
		}
	}
	return stored, nil
}

// Ask opens one structured human decision request. It reuses the decision
// primitive's vocabulary (options, free text, deadline) rather than inferring
// a question from free text.
func (c *Coordinator) Ask(ctx context.Context, tenantID, workItemID uuid.UUID, req AttentionRequest) (Attention, error) {
	const operation = "ask"
	if err := c.checkTenant(tenantID); err != nil {
		return Attention{}, c.reject(ctx, operation, err, "")
	}
	item, err := c.work.GetWorkItem(ctx, tenantID, workItemID)
	if err != nil {
		return Attention{}, c.reject(ctx, operation, err, "")
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID != "" {
		// Serialise the same request id and answer an idempotent retry from the
		// stored row without publishing or auditing it a second time.
		unlock := c.keyLocks.lock("attention|" + tenantID.String() + "|" + item.ID.String() + "|" + requestID)
		defer unlock()
		existing, err := c.work.ListAttentions(ctx, tenantID, item.ID)
		if err != nil {
			return Attention{}, err
		}
		for _, attention := range existing {
			if attention.RequestID == requestID {
				return attention, nil
			}
		}
	}
	if item.Status.Closed() {
		refusal := newError(ErrWorkItemClosed, CodeWorkItemClosed, operation, "the work item is closed")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	if req.CallerSessionID == uuid.Nil {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "callerSessionId is required")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	caller, err := c.sessions.Session(ctx, tenantID, req.CallerSessionID)
	if err != nil {
		refusal := newError(ErrSessionNotFound, CodeSessionNotFound, operation, "caller session not found")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	if caller.State != SessionActive {
		refusal := newError(ErrParentSessionInactive, CodeParentInactive, operation, "caller session is not active")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		refusal.SessionID = caller.SessionID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	member, err := c.isWorkMember(ctx, tenantID, item, caller)
	if err != nil {
		return Attention{}, err
	}
	if !member {
		refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation,
			"the caller session is not a member of the work item")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		refusal.SessionID = caller.SessionID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	if req.AssignmentID != uuid.Nil {
		assignment, err := c.work.GetAssignment(ctx, tenantID, req.AssignmentID)
		if err != nil {
			return Attention{}, c.reject(ctx, operation, err, item.ModeID)
		}
		if assignment.WorkItemID != item.ID {
			refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation,
				"assignment does not belong to the work item")
			refusal.ModeID = item.ModeID
			refusal.WorkItemID = item.ID
			refusal.AssignmentID = assignment.ID
			return Attention{}, c.rejectWith(ctx, refusal)
		}
		if caller.SessionID != assignment.SessionID && caller.SessionID != assignment.CallerSessionID {
			refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation,
				"the caller session is not part of the assignment")
			refusal.ModeID = item.ModeID
			refusal.WorkItemID = item.ID
			refusal.AssignmentID = assignment.ID
			refusal.SessionID = caller.SessionID
			return Attention{}, c.rejectWith(ctx, refusal)
		}
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "prompt is required")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	options := make([]AttentionOption, 0, len(req.Options))
	seenOptions := make(map[string]bool, len(req.Options))
	for _, option := range req.Options {
		id := strings.TrimSpace(option.ID)
		if id == "" {
			refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "attention option ids are required")
			refusal.ModeID = item.ModeID
			refusal.WorkItemID = item.ID
			return Attention{}, c.rejectWith(ctx, refusal)
		}
		if seenOptions[id] {
			refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "attention option ids must be unique")
			refusal.ModeID = item.ModeID
			refusal.WorkItemID = item.ID
			return Attention{}, c.rejectWith(ctx, refusal)
		}
		seenOptions[id] = true
		option.ID = id
		option.Label = strings.TrimSpace(option.Label)
		option.Description = strings.TrimSpace(option.Description)
		options = append(options, option)
	}
	if len(options) == 0 && !req.AllowFreeText {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation,
			"an attention needs options or allowFreeText")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	route := req.Route
	if route == "" {
		route = AttentionRouteHuman
	}
	if !route.Valid() {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "attention route is not in the closed vocabulary")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	channel := strings.TrimSpace(req.Channel)
	if route == AttentionRouteChannel && channel == "" {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "a channel route needs a channel id")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	if req.Timeout < 0 {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "timeout must be non-negative")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = DefaultAttentionTimeout
	}
	if timeout > MaxAttentionTimeout {
		timeout = MaxAttentionTimeout
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = AttentionDecision
	}
	now := c.now()
	attention := Attention{
		ID:            attentionID(tenantID, item.ID, requestID),
		TenantID:      tenantID,
		WorkItemID:    item.ID,
		AssignmentID:  req.AssignmentID,
		ModeID:        item.ModeID,
		SessionID:     caller.SessionID,
		RequestID:     requestID,
		Kind:          kind,
		Prompt:        prompt,
		Options:       options,
		AllowFreeText: req.AllowFreeText,
		Route:         route,
		Channel:       channel,
		Status:        AttentionOpen,
		CreatedAt:     now,
		Deadline:      now.Add(timeout),
	}
	if err := c.record(ctx, AuditEvent{
		Kind: AuditAttentionOpened, Result: "accepted", Operation: operation,
		ModeID: item.ModeID, WorkItemID: item.ID, AssignmentID: attention.AssignmentID,
		SessionID: caller.SessionID,
	}); err != nil {
		return Attention{}, &Error{Kind: ErrAuditUnavailable, Code: CodeAuditUnavailable, Operation: operation,
			ModeID: item.ModeID, WorkItemID: item.ID, AssignmentID: attention.AssignmentID,
			SessionID: caller.SessionID, Detail: "the audit row could not be written"}
	}
	stored, err := c.work.CreateAttention(ctx, attention)
	if err != nil {
		_ = c.record(ctx, AuditEvent{
			Kind: AuditAttentionFailed, Result: "failed", Operation: operation,
			ModeID: item.ModeID, WorkItemID: item.ID, AssignmentID: attention.AssignmentID,
			SessionID: caller.SessionID, Detail: err.Error(),
		})
		return Attention{}, err
	}
	if stored.ID != attention.ID {
		return stored, nil
	}
	if c.attention != nil {
		if err := c.attention.Publish(ctx, stored); err != nil {
			_ = c.record(ctx, AuditEvent{
				Kind: AuditAttentionFailed, Result: "failed", Operation: operation,
				ModeID: item.ModeID, WorkItemID: item.ID, AssignmentID: stored.AssignmentID,
				SessionID: stored.SessionID, Detail: err.Error(),
			})
			return Attention{}, &Error{Kind: ErrAttentionPublish, Code: CodeAttentionPublish, Operation: operation,
				ModeID: item.ModeID, WorkItemID: item.ID, AssignmentID: stored.AssignmentID,
				SessionID: stored.SessionID, Detail: "the attention could not be published"}
		}
	}
	return stored, nil
}

// AnswerAttention records an answer with decision semantics: idempotent for the
// same answer, conflicting answers refused, and an overdue question expires.
func (c *Coordinator) AnswerAttention(ctx context.Context, tenantID, workItemID, attentionID uuid.UUID, req AttentionAnswerRequest) (Attention, error) {
	const operation = "answer_attention"
	if err := c.checkTenant(tenantID); err != nil {
		return Attention{}, c.reject(ctx, operation, err, "")
	}
	item, err := c.work.GetWorkItem(ctx, tenantID, workItemID)
	if err != nil {
		return Attention{}, c.reject(ctx, operation, err, "")
	}
	if item.Status.Closed() {
		refusal := newError(ErrWorkItemClosed, CodeWorkItemClosed, operation, "the work item is closed")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	attention, err := c.work.GetAttention(ctx, tenantID, attentionID)
	if err != nil {
		return Attention{}, c.reject(ctx, operation, err, item.ModeID)
	}
	if attention.WorkItemID != item.ID {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "attention does not belong to the work item")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	optionID := strings.TrimSpace(req.OptionID)
	text := strings.TrimSpace(req.Text)
	if attention.Status.Terminal() {
		if attention.Status == AttentionAnswered && sameAnswer(attention.Answer, optionID, text) {
			return attention, nil
		}
		if attention.Status == AttentionAnswered {
			refusal := newError(ErrAttentionConflict, CodeAttentionConflict, operation,
				"attention was already answered differently")
			refusal.ModeID = item.ModeID
			refusal.WorkItemID = item.ID
			return Attention{}, c.rejectWith(ctx, refusal)
		}
		refusal := newError(ErrAttentionClosed, CodeAttentionClosed, operation, "attention is already terminal")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	now := c.now()
	if !attention.Deadline.IsZero() && now.After(attention.Deadline) {
		attention.Status = AttentionTimedOut
		if _, err := c.work.UpdateAttention(ctx, attention); err != nil {
			return Attention{}, err
		}
		_ = c.record(ctx, AuditEvent{Kind: AuditAttentionExpired, Result: "failed", Operation: operation,
			ModeID: item.ModeID, WorkItemID: item.ID, Detail: "attention deadline passed"})
		refusal := newError(ErrAttentionExpired, CodeAttentionExpired, operation, "attention deadline passed")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, refusal
	}
	if optionID == "" && text == "" {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "an answer needs an option or text")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	if optionID != "" && !attentionHasOption(attention, optionID) {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "the option is not part of the attention")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	if text != "" && !attention.AllowFreeText {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "free text is not allowed for this attention")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	if optionID == "" && !attention.AllowFreeText {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "this attention needs an option")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Attention{}, c.rejectWith(ctx, refusal)
	}
	attention.Status = AttentionAnswered
	attention.Answer = &AttentionAnswer{OptionID: optionID, Text: text, Actor: strings.TrimSpace(req.Actor), At: now}
	updated, err := c.work.UpdateAttention(ctx, attention)
	if err != nil {
		return Attention{}, err
	}
	_ = c.record(ctx, AuditEvent{Kind: AuditAttentionAnswered, Result: "accepted", Operation: operation,
		ModeID: item.ModeID, WorkItemID: item.ID, AssignmentID: attention.AssignmentID, SessionID: attention.SessionID})
	return updated, nil
}

// ExpireAttentions marks every overdue open attention on a WorkItem as
// timed_out and returns the count.
func (c *Coordinator) ExpireAttentions(ctx context.Context, tenantID, workItemID uuid.UUID) (int, error) {
	const operation = "expire_attentions"
	if err := c.checkTenant(tenantID); err != nil {
		return 0, c.reject(ctx, operation, err, "")
	}
	item, err := c.work.GetWorkItem(ctx, tenantID, workItemID)
	if err != nil {
		return 0, c.reject(ctx, operation, err, "")
	}
	attentions, err := c.work.ListAttentions(ctx, tenantID, item.ID)
	if err != nil {
		return 0, err
	}
	now := c.now()
	expired := 0
	for _, attention := range attentions {
		if attention.Status.Terminal() || attention.Deadline.IsZero() || now.Before(attention.Deadline) {
			continue
		}
		attention.Status = AttentionTimedOut
		if _, err := c.work.UpdateAttention(ctx, attention); err != nil {
			return expired, err
		}
		expired++
	}
	if expired > 0 {
		_ = c.record(ctx, AuditEvent{Kind: AuditAttentionExpired, Result: "accepted", Operation: operation,
			ModeID: item.ModeID, WorkItemID: item.ID, Actual: int64(expired)})
	}
	return expired, nil
}

// Cancel stops a WorkItem: it cancels every bound session and its whole bare
// delegation subtree, closes assignments and open attention requests, and is
// idempotent. A failed propagation leaves the item in "cancelling".
func (c *Coordinator) Cancel(ctx context.Context, tenantID, workItemID uuid.UUID, reason string) error {
	const operation = "cancel"
	if err := c.checkTenant(tenantID); err != nil {
		return c.reject(ctx, operation, err, "")
	}
	item, err := c.work.GetWorkItem(ctx, tenantID, workItemID)
	if err != nil {
		return c.reject(ctx, operation, err, "")
	}
	if item.Status == WorkItemCancelled {
		return nil
	}
	item.Status = WorkItemCancelling
	item.UpdatedAt = c.now()
	if _, err := c.work.UpdateWorkItem(ctx, item); err != nil {
		return err
	}

	var failures []string
	visited := make(map[uuid.UUID]bool)
	assignments, err := c.work.ListAssignments(ctx, tenantID, item.ID)
	if err != nil {
		return err
	}
	for _, assignment := range assignments {
		if assignment.Status.Terminal() {
			continue
		}
		if assignment.SessionID != uuid.Nil {
			treeFailures := c.cancelSessionTree(ctx, tenantID, assignment.SessionID, reason, visited)
			if len(treeFailures) > 0 {
				failures = append(failures, treeFailures...)
				continue
			}
		}
		assignment.Status = AssignmentCancelled
		assignment.UpdatedAt = c.now()
		if _, err := c.work.UpdateAssignment(ctx, assignment); err != nil {
			failures = append(failures, fmt.Sprintf("assignment %s: %v", assignment.ID, err))
		}
	}
	attentions, err := c.work.ListAttentions(ctx, tenantID, item.ID)
	if err != nil {
		return err
	}
	for _, attention := range attentions {
		if attention.Status.Terminal() {
			continue
		}
		attention.Status = AttentionCancelled
		if _, err := c.work.UpdateAttention(ctx, attention); err != nil {
			failures = append(failures, fmt.Sprintf("attention %s: %v", attention.ID, err))
		}
	}

	if len(failures) > 0 {
		item.UpdatedAt = c.now()
		_, _ = c.work.UpdateWorkItem(ctx, item)
		_ = c.record(ctx, AuditEvent{
			Kind: AuditCancelFailed, Result: "failed", Operation: operation,
			ModeID: item.ModeID, WorkItemID: item.ID,
			Detail: strings.Join(failures, "; "),
		})
		refusal := newError(ErrCancelFailed, CodeCancelFailed, operation, strings.Join(failures, "; "))
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return refusal
	}
	item.Status = WorkItemCancelled
	item.UpdatedAt = c.now()
	if _, err := c.work.UpdateWorkItem(ctx, item); err != nil {
		return err
	}
	_ = c.record(ctx, AuditEvent{
		Kind: AuditWorkCancelled, Result: "accepted", Operation: operation,
		ModeID: item.ModeID, WorkItemID: item.ID,
	})
	return nil
}

// CompleteSession projects a worker session's terminal outcome into the index
// and its assignment, releasing the concurrency/width slot. It is the product
// terminator the runner calls when the child settles.
func (c *Coordinator) CompleteSession(ctx context.Context, tenantID, sessionID uuid.UUID, outcome SessionOutcome, result json.RawMessage, failure string) error {
	const operation = "complete_session"
	if err := c.checkTenant(tenantID); err != nil {
		return c.reject(ctx, operation, err, "")
	}
	state, ok := outcome.State()
	if !ok {
		return c.reject(ctx, operation, newError(ErrInvalidRequest, CodeInvalidRequest, operation,
			"unknown session outcome "+string(outcome)), "")
	}
	ref, err := c.sessions.Session(ctx, tenantID, sessionID)
	if err != nil {
		return c.reject(ctx, operation, err, "")
	}
	if err := c.sessions.SetState(ctx, tenantID, sessionID, state); err != nil {
		return err
	}
	if ref.WorkItemID != uuid.Nil {
		assignments, err := c.work.ListAssignments(ctx, tenantID, ref.WorkItemID)
		if err != nil {
			return err
		}
		for _, assignment := range assignments {
			if assignment.SessionID != sessionID {
				continue
			}
			assignment.Status = assignmentStatusFor(outcome)
			assignment.Result = cloneRaw(result)
			assignment.Error = strings.TrimSpace(failure)
			assignment.UpdatedAt = c.now()
			if _, err := c.work.UpdateAssignment(ctx, assignment); err != nil {
				return err
			}
		}
	}
	_ = c.record(ctx, AuditEvent{Kind: AuditSessionCompleted, Result: "accepted", Operation: operation,
		ModeID: ref.ModeID, WorkItemID: ref.WorkItemID, SessionID: sessionID, BotID: ref.BotID})
	return nil
}

// cancelSessionTree cancels a session and every descendant. It returns one
// failure string per operation that could not be propagated; the work item then
// stays "cancelling" instead of claiming the subtree stopped.
func (c *Coordinator) cancelSessionTree(ctx context.Context, tenantID, sessionID uuid.UUID, reason string, visited map[uuid.UUID]bool) []string {
	if visited[sessionID] {
		return nil
	}
	visited[sessionID] = true
	var failures []string
	ref, err := c.sessions.Session(ctx, tenantID, sessionID)
	switch {
	case errors.Is(err, ErrSessionNotFound):
		// Already gone; still walk children by parent id? The index cannot list
		// children of an absent row, and an absent row means nothing to cancel.
		return nil
	case err != nil:
		return []string{fmt.Sprintf("session %s: %v", sessionID, err)}
	}
	if ref.State == SessionActive {
		if err := c.launcher.CancelSession(ctx, tenantID, sessionID, reason); err != nil {
			failures = append(failures, fmt.Sprintf("session %s: %v", sessionID, err))
		} else if err := c.sessions.SetState(ctx, tenantID, sessionID, SessionCancelled); err != nil &&
			!errors.Is(err, ErrSessionNotFound) {
			failures = append(failures, fmt.Sprintf("session %s: %v", sessionID, err))
		}
	}
	children, err := c.sessions.Children(ctx, tenantID, sessionID)
	if err != nil {
		return append(failures, fmt.Sprintf("children of %s: %v", sessionID, err))
	}
	for _, child := range children {
		failures = append(failures, c.cancelSessionTree(ctx, tenantID, child.SessionID, reason, visited)...)
	}
	return failures
}

func (c *Coordinator) reserve(ctx context.Context, tenantID uuid.UUID, modeID string, parentSessionID uuid.UUID, limits Limits) (reservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limits.MaxChildren > 0 {
		active, err := c.sessions.ActiveChildren(ctx, tenantID, parentSessionID)
		if err != nil {
			return reservation{}, newError(ErrInvalidCreation, CodeInvalidCreation, "reserve", err.Error())
		}
		pending := c.pendingChildren[parentSessionID]
		if active+pending >= limits.MaxChildren {
			refusal := newError(ErrWidthExceeded, CodeWidthExceeded, "create_session",
				fmt.Sprintf("parent already has %d active/reserved children (limit %d)", active+pending, limits.MaxChildren))
			refusal.Field = "maxChildren"
			refusal.Limit = int64(limits.MaxChildren)
			refusal.Actual = int64(active + pending)
			refusal.SessionID = parentSessionID
			return reservation{}, refusal
		}
	}
	if limits.MaxConcurrent > 0 {
		active, err := c.sessions.ActiveInMode(ctx, tenantID, modeID)
		if err != nil {
			return reservation{}, newError(ErrInvalidCreation, CodeInvalidCreation, "reserve", err.Error())
		}
		pending := c.pendingModes[modeID]
		if active+pending >= limits.MaxConcurrent {
			refusal := newError(ErrConcurrencyExceeded, CodeConcurrencyExceeded, "create_session",
				fmt.Sprintf("mode already has %d active/reserved sessions (limit %d)", active+pending, limits.MaxConcurrent))
			refusal.Field = "maxConcurrent"
			refusal.Limit = int64(limits.MaxConcurrent)
			refusal.Actual = int64(active + pending)
			return reservation{}, refusal
		}
	}
	c.pendingChildren[parentSessionID]++
	c.pendingModes[modeID]++
	return reservation{parentID: parentSessionID, modeID: modeID, active: true}, nil
}

func (c *Coordinator) release(res *reservation) {
	if res == nil || !res.active {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingChildren[res.parentID] > 0 {
		c.pendingChildren[res.parentID]--
		if c.pendingChildren[res.parentID] == 0 {
			delete(c.pendingChildren, res.parentID)
		}
	}
	if c.pendingModes[res.modeID] > 0 {
		c.pendingModes[res.modeID]--
		if c.pendingModes[res.modeID] == 0 {
			delete(c.pendingModes, res.modeID)
		}
	}
	res.active = false
}

func (c *Coordinator) creation(key string) (*sessionCreation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.idem[key]
	return entry, ok
}

func (c *Coordinator) storeCreation(key string, entry *sessionCreation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.idem[key] = entry
}

func (c *Coordinator) checkTenant(tenantID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return newError(ErrTenantRequired, CodeTenantRequired, "", "tenant is required")
	}
	if tenantID != c.tenantID {
		return newError(ErrTenantMismatch, CodeTenantMismatch, "", "the tenant does not own this host")
	}
	return nil
}

func (c *Coordinator) activeSession(ctx context.Context, tenantID, sessionID uuid.UUID, operation string) (SessionRef, error) {
	if sessionID == uuid.Nil {
		return SessionRef{}, newError(ErrInvalidRequest, CodeInvalidRequest, operation, "callerSessionId is required")
	}
	ref, err := c.sessions.Session(ctx, tenantID, sessionID)
	if err != nil {
		return SessionRef{}, newError(ErrSessionNotFound, CodeSessionNotFound, operation, "caller session not found")
	}
	if ref.State != SessionActive {
		return SessionRef{}, newError(ErrParentSessionInactive, CodeParentInactive, operation, "caller session is not active")
	}
	return ref, nil
}

// isWorkMember answers whether the caller may act on a WorkItem: the owner
// session, a Host-created session bound to the item, or either side of an
// assignment.
func (c *Coordinator) isWorkMember(ctx context.Context, tenantID uuid.UUID, item WorkItem, caller SessionRef) (bool, error) {
	if caller.SessionID == item.OwnerSessionID || caller.WorkItemID == item.ID {
		return true, nil
	}
	assignments, err := c.work.ListAssignments(ctx, tenantID, item.ID)
	if err != nil {
		return false, err
	}
	for _, assignment := range assignments {
		if assignment.SessionID == caller.SessionID || assignment.CallerSessionID == caller.SessionID {
			return true, nil
		}
	}
	return false, nil
}

func (c *Coordinator) record(ctx context.Context, event AuditEvent) error {
	if event.TenantID == uuid.Nil {
		event.TenantID = c.tenantID
	}
	if event.At.IsZero() {
		event.At = c.now()
	}
	return c.audit.Record(ctx, event)
}

// reject audits a refusal and returns it. A denial audit failure never changes
// the refusal (the operation is already denied).
func (c *Coordinator) reject(ctx context.Context, operation string, err error, modeID string) error {
	var target *Error
	if !errors.As(err, &target) {
		target = newError(ErrInvalidRequest, CodeInvalidRequest, operation, err.Error())
		err = target
	}
	if target.ModeID == "" {
		target.ModeID = modeID
	}
	if target.Operation == "" {
		target.Operation = operation
	}
	kind := AuditInvalidCreation
	if errors.Is(err, ErrPermissionDenied) {
		kind = AuditPermissionDenied
	}
	_ = c.record(ctx, AuditEvent{
		Kind: kind, Result: "denied", Reason: string(target.Code), Operation: target.Operation,
		ModeID: target.ModeID, WorkItemID: target.WorkItemID, AssignmentID: target.AssignmentID,
		SessionID: target.SessionID, BotID: target.BotID, Field: target.Field,
		Limit: target.Limit, Actual: target.Actual, Detail: target.Detail,
	})
	return err
}

// rejectWith is reject for an error that already carries its full context.
func (c *Coordinator) rejectWith(ctx context.Context, err error) error {
	return c.reject(ctx, "", err, "")
}

func hasJSON(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func sessionIdempotencyKey(tenantID uuid.UUID, modeID string, req SessionRequest) string {
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return ""
	}
	return tenantID.String() + "|" + modeID + "|" + req.ParentSessionID.String() + "|" + req.WorkItemID.String() + "|" + key
}

func workItemID(tenantID uuid.UUID, modeID string, callerID uuid.UUID, key string) uuid.UUID {
	if key == "" {
		return uuid.New()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("keel.collab.work.v1|"+tenantID.String()+"|"+modeID+"|"+callerID.String()+"|"+key))
}

func assignmentID(tenantID, workItemID uuid.UUID, key string) uuid.UUID {
	if key == "" {
		return uuid.New()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("keel.collab.assignment.v1|"+tenantID.String()+"|"+workItemID.String()+"|"+key))
}

func attentionID(tenantID, workItemID uuid.UUID, requestID string) uuid.UUID {
	if requestID == "" {
		return uuid.New()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("keel.collab.attention.v1|"+tenantID.String()+"|"+workItemID.String()+"|"+requestID))
}

func scopeAllowsBot(scope Scope, botID string) bool {
	if len(scope.BotIDs) == 0 {
		return true
	}
	for _, allowed := range scope.BotIDs {
		if strings.TrimSpace(allowed) == botID {
			return true
		}
	}
	return false
}

func validateLinkEndpoint(endpoint LinkEndpoint) error {
	switch endpoint.Type {
	case LinkEndpointSession, LinkEndpointWorkItem, LinkEndpointAssignment:
		if _, err := uuid.Parse(strings.TrimSpace(endpoint.ID)); err != nil {
			return newError(ErrInvalidRequest, CodeInvalidRequest, "add_link",
				"link endpoint "+string(endpoint.Type)+" needs a uuid id")
		}
	case LinkEndpointBot, LinkEndpointChannel:
		if strings.TrimSpace(endpoint.ID) == "" {
			return newError(ErrInvalidRequest, CodeInvalidRequest, "add_link",
				"link endpoint "+string(endpoint.Type)+" needs an id")
		}
	default:
		return newError(ErrInvalidRequest, CodeInvalidRequest, "add_link",
			"link endpoint type "+string(endpoint.Type)+" is not in the closed vocabulary")
	}
	return nil
}

func sameAnswer(answer *AttentionAnswer, optionID, text string) bool {
	if answer == nil {
		return false
	}
	return answer.OptionID == optionID && answer.Text == text
}

func attentionHasOption(attention Attention, optionID string) bool {
	for _, option := range attention.Options {
		if option.ID == optionID {
			return true
		}
	}
	return false
}

func assignmentStatusFor(outcome SessionOutcome) AssignmentStatus {
	switch outcome {
	case SessionOutcomeDone:
		return AssignmentDone
	case SessionOutcomeFailed:
		return AssignmentFailed
	default:
		return AssignmentCancelled
	}
}

// keyedLocker serialises same-idempotency-key calls without serialising
// unrelated creations. Entries are reference-counted so the map stays bounded.
type keyedLocker struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedLocker) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*keyedLock)
	}
	entry := k.locks[key]
	if entry == nil {
		entry = &keyedLock{}
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
