package collab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Audit event kinds written by the guarded host. They are structured
// (kind + result + reason + ids); no audit row contains prompt text or file
// contents.
const (
	AuditInvalidCreation  = "collab.invalid_creation"
	AuditPermissionDenied = "permission.denied"
	AuditSessionCreating  = "collab.session_creating"
	AuditSessionCreated   = "collab.session_created"
	AuditSessionFailed    = "collab.session_create_failed"
	AuditWorkOpened       = "collab.work_opened"
	AuditAssignmentMade   = "collab.assignment_created"
	AuditAttentionOpened  = "collab.attention_opened"
	AuditWorkCancelled    = "collab.work_cancelled"
	AuditCancelFailed     = "collab.cancel_failed"
	AuditAttentionFailed  = "collab.attention_publish_failed"
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

// AuditSink records collaboration audit rows. A sink failure is fail-closed
// for the accepted path: the Host refuses to create work it cannot audit.
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
	// WorkItemID is optional for a bare delegation. An empty id creates a
	// parent-scoped session with no ticket record.
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
	// AssignmentID, when set, binds the new session to an existing Assignment.
	AssignmentID uuid.UUID `json:"assignmentId,omitempty"`
	// IdempotencyKey makes a retried creation target the same session.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// Budget narrows the effective mode limits. Zero fields inherit.
	Budget Budget `json:"budget,omitempty"`
}

// AssignmentRequest asks the Host to bind a WorkItem to a Bot/Session.
type AssignmentRequest struct {
	// CallerSessionID is the acting session; the target Bot's bot.invoke is
	// checked against it. Required.
	CallerSessionID uuid.UUID `json:"callerSessionId"`
	// SessionID optionally binds an already-created worker session.
	SessionID uuid.UUID `json:"sessionId,omitempty"`
	// HumanTakeover allows an assignment on an existing WorkItem whose mode is
	// disabled. It is the manual-takeover path, not a way to create new work.
	HumanTakeover bool `json:"humanTakeover,omitempty"`
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

// WorkItemRequest asks the Host to open one WorkItem.
type WorkItemRequest struct {
	Title          string          `json:"title"`
	Input          json.RawMessage `json:"input,omitempty"`
	Source         SourceRef       `json:"source,omitempty"`
	Scope          Scope           `json:"scope,omitempty"`
	Limits         Limits          `json:"limits,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
}

// Host is the single creation gate a mode may use. Creating a session any
// other way (importing kernel/store) is a contract violation.
type Host interface {
	CreateSession(ctx context.Context, tenantID uuid.UUID, modeID string, req SessionRequest) (SessionRef, error)
	Assign(ctx context.Context, tenantID, workItemID uuid.UUID, botID string, req AssignmentRequest) (Assignment, error)
	Ask(ctx context.Context, tenantID, workItemID uuid.UUID, req AttentionRequest) (Attention, error)
	Cancel(ctx context.Context, tenantID, workItemID uuid.UUID, reason string) error
}

// HostOptions configures the guarded Coordinator. Every seam is required: a
// half-armed Host must fail at construction, not at the first refusal.
type HostOptions struct {
	TenantID    uuid.UUID
	Modes       *ModeRegistry
	Work        WorkStore
	Sessions    SessionIndex
	Permissions PermissionChecker
	Audit       AuditSink
	Launcher    SessionLauncher
	// Attention routes stored attention requests. Nil keeps them readable but
	// un-routed (the read API still works).
	Attention AttentionPublisher
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Coordinator implements Host and the read-side WorkItem projection. It is the
// only type in this package that performs guards.
type Coordinator struct {
	tenantID    uuid.UUID
	modes       *ModeRegistry
	work        WorkStore
	sessions    SessionIndex
	permissions PermissionChecker
	audit       AuditSink
	launcher    SessionLauncher
	attention   AttentionPublisher
	now         func() time.Time
}

var _ Host = (*Coordinator)(nil)

// NewHost builds the guarded Host. Missing seams are construction errors.
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
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Coordinator{
		tenantID:    opts.TenantID,
		modes:       opts.Modes,
		work:        opts.Work,
		sessions:    opts.Sessions,
		permissions: opts.Permissions,
		audit:       opts.Audit,
		launcher:    opts.Launcher,
		attention:   opts.Attention,
		now:         opts.Now,
	}, nil
}

// OpenWorkItem creates one WorkItem under an enabled mode. The request limits
// may only narrow the mode's effective limits.
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
	source := req.Source
	if strings.TrimSpace(source.Kind) == "" {
		source.Kind = "manual"
	}
	now := c.now()
	item := WorkItem{
		ID:             uuid.New(),
		TenantID:       tenantID,
		ModeID:         modeID,
		Title:          title,
		Input:          cloneRaw(req.Input),
		Source:         source,
		Scope:          req.Scope.clone(),
		Limits:         req.Limits.NarrowedBy(descriptor.Limits),
		Status:         WorkItemOpen,
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	stored, err := c.work.CreateWorkItem(ctx, item)
	if err != nil {
		return WorkItem{}, err
	}
	_ = c.record(ctx, AuditEvent{Kind: AuditWorkOpened, Result: "accepted", Operation: operation, ModeID: modeID, WorkItemID: stored.ID})
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
	if strings.TrimSpace(link.From.ID) == "" || strings.TrimSpace(link.To.ID) == "" {
		return Link{}, c.reject(ctx, operation, newError(ErrInvalidRequest, CodeInvalidRequest, operation,
			"link endpoints need an id"), "")
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

// CreateSession is the single controlled creation gate.
func (c *Coordinator) CreateSession(ctx context.Context, tenantID uuid.UUID, modeID string, req SessionRequest) (SessionRef, error) {
	const operation = "create_session"
	modeID = strings.TrimSpace(modeID)
	if err := c.checkTenant(tenantID); err != nil {
		return SessionRef{}, c.reject(ctx, operation, err, modeID)
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
	// A parent this host created for a different WorkItem must not lend its
	// authority to a new item. A root session (created outside the Host) has no
	// WorkItemID and stays usable as the caller for a first delegation.
	if req.WorkItemID != uuid.Nil && parent.WorkItemID != uuid.Nil && parent.WorkItemID != req.WorkItemID {
		refusal := newError(ErrInvalidCreation, CodeInvalidCreation, operation,
			"the parent session belongs to a different work item")
		refusal.ModeID = modeID
		refusal.SessionID = parent.SessionID
		refusal.WorkItemID = req.WorkItemID
		return SessionRef{}, c.rejectWith(ctx, refusal)
	}

	depth := parent.Depth + 1
	if effective.MaxDepth > 0 && depth > effective.MaxDepth {
		refusal := newError(ErrDepthExceeded, CodeDepthExceeded, operation,
			fmt.Sprintf("child depth %d exceeds the mode limit %d", depth, effective.MaxDepth))
		refusal.ModeID = modeID
		refusal.SessionID = parent.SessionID
		refusal.WorkItemID = workItem.ID
		refusal.Field = "maxDepth"
		refusal.Limit = int64(effective.MaxDepth)
		refusal.Actual = int64(depth)
		return SessionRef{}, c.rejectWith(ctx, refusal)
	}
	if effective.MaxChildren > 0 {
		children, err := c.sessions.ActiveChildren(ctx, tenantID, parent.SessionID)
		if err != nil {
			return SessionRef{}, err
		}
		if children >= effective.MaxChildren {
			refusal := newError(ErrWidthExceeded, CodeWidthExceeded, operation,
				fmt.Sprintf("parent already has %d active children (limit %d)", children, effective.MaxChildren))
			refusal.ModeID = modeID
			refusal.SessionID = parent.SessionID
			refusal.WorkItemID = workItem.ID
			refusal.Field = "maxChildren"
			refusal.Limit = int64(effective.MaxChildren)
			refusal.Actual = int64(children)
			return SessionRef{}, c.rejectWith(ctx, refusal)
		}
	}
	if effective.MaxConcurrent > 0 {
		active, err := c.sessions.ActiveInMode(ctx, tenantID, modeID)
		if err != nil {
			return SessionRef{}, err
		}
		if active >= effective.MaxConcurrent {
			refusal := newError(ErrConcurrencyExceeded, CodeConcurrencyExceeded, operation,
				fmt.Sprintf("mode already has %d active sessions (limit %d)", active, effective.MaxConcurrent))
			refusal.ModeID = modeID
			refusal.SessionID = parent.SessionID
			refusal.WorkItemID = workItem.ID
			refusal.Field = "maxConcurrent"
			refusal.Limit = int64(effective.MaxConcurrent)
			refusal.Actual = int64(active)
			return SessionRef{}, c.rejectWith(ctx, refusal)
		}
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
		assignment = stored
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
		return SessionRef{}, &Error{Kind: ErrAuditUnavailable, Code: CodeAuditUnavailable, Operation: operation,
			ModeID: descriptor.ID, WorkItemID: req.WorkItemID, SessionID: parent.SessionID, BotID: botID,
			Detail: "the audit row could not be written"}
	}

	ref, err := c.launcher.Launch(ctx, launch)
	if err != nil {
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
	if err := c.sessions.Put(ctx, ref); err != nil {
		_ = c.record(ctx, AuditEvent{
			Kind: AuditSessionFailed, Result: "failed", Operation: operation,
			ModeID: descriptor.ID, WorkItemID: req.WorkItemID, SessionID: ref.SessionID, BotID: botID,
			Detail: err.Error(),
		})
		return SessionRef{}, err
	}

	if assignment.ID != uuid.Nil {
		assignment.SessionID = ref.SessionID
		assignment.Status = AssignmentActive
		assignment.UpdatedAt = c.now()
		if _, err := c.work.UpdateAssignment(ctx, assignment); err != nil {
			_ = c.record(ctx, AuditEvent{
				Kind: AuditSessionFailed, Result: "failed", Operation: operation,
				ModeID: descriptor.ID, WorkItemID: req.WorkItemID, AssignmentID: assignment.ID,
				SessionID: ref.SessionID, BotID: botID, Detail: err.Error(),
			})
			return SessionRef{}, err
		}
	}
	if workItem.ID != uuid.Nil && workItem.Status == WorkItemOpen {
		workItem.Status = WorkItemActive
		workItem.UpdatedAt = c.now()
		if _, err := c.work.UpdateWorkItem(ctx, workItem); err != nil {
			_ = c.record(ctx, AuditEvent{
				Kind: AuditSessionFailed, Result: "failed", Operation: operation,
				ModeID: descriptor.ID, WorkItemID: workItem.ID, SessionID: ref.SessionID, BotID: botID,
				Detail: err.Error(),
			})
			return SessionRef{}, err
		}
	}
	// The pre-launch row is the authorization fact; this outcome row is
	// best-effort so a sink hiccup cannot hide a session that already exists.
	_ = c.record(ctx, AuditEvent{
		Kind: AuditSessionCreated, Result: "accepted", Operation: operation,
		ModeID: descriptor.ID, WorkItemID: req.WorkItemID, AssignmentID: assignment.ID,
		SessionID: ref.SessionID, BotID: botID, Actual: int64(depth),
	})
	return ref, nil
}

// Assign binds a WorkItem to a Bot (and optionally a live Session). The
// assignment is not owned by the Session: ending or replacing the worker
// session leaves it readable and a new assignment can continue the same item.
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
	if !c.modes.Enabled(item.ModeID) && !req.HumanTakeover {
		refusal := newError(ErrModeDisabled, CodeModeDisabled, operation,
			"the mode is disabled for new assignments; set humanTakeover for manual takeover")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Assignment{}, c.rejectWith(ctx, refusal)
	}
	botID = strings.TrimSpace(botID)
	if botID == "" {
		refusal := newError(ErrInvalidRequest, CodeInvalidRequest, operation, "botId is required")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		return Assignment{}, c.rejectWith(ctx, refusal)
	}
	if req.CallerSessionID == uuid.Nil {
		refusal := newError(ErrPermissionDenied, CodePermissionDenied, operation,
			"an assignment needs an acting session")
		refusal.ModeID = item.ModeID
		refusal.WorkItemID = item.ID
		refusal.BotID = botID
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
	assignment := Assignment{
		ID:              uuid.New(),
		TenantID:        tenantID,
		WorkItemID:      item.ID,
		ModeID:          item.ModeID,
		BotID:           botID,
		SessionID:       req.SessionID,
		CallerSessionID: caller.SessionID,
		Status:          status,
		Title:           strings.TrimSpace(req.Title),
		Input:           cloneRaw(req.Input),
		IdempotencyKey:  strings.TrimSpace(req.IdempotencyKey),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	stored, err := c.work.CreateAssignment(ctx, assignment)
	if err != nil {
		return Assignment{}, err
	}
	_ = c.record(ctx, AuditEvent{
		Kind: AuditAssignmentMade, Result: "accepted", Operation: operation,
		ModeID: item.ModeID, WorkItemID: item.ID, AssignmentID: stored.ID,
		SessionID: stored.SessionID, BotID: botID,
	})
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
		ID:            uuid.New(),
		TenantID:      tenantID,
		WorkItemID:    item.ID,
		AssignmentID:  req.AssignmentID,
		ModeID:        item.ModeID,
		SessionID:     caller.SessionID,
		RequestID:     strings.TrimSpace(req.RequestID),
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
	stored, err := c.work.CreateAttention(ctx, attention)
	if err != nil {
		return Attention{}, err
	}
	if stored.ID != attention.ID {
		// An idempotent retry returned the original row; do not publish twice.
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
	_ = c.record(ctx, AuditEvent{
		Kind: AuditAttentionOpened, Result: "accepted", Operation: operation,
		ModeID: item.ModeID, WorkItemID: item.ID, AssignmentID: stored.AssignmentID,
		SessionID: stored.SessionID,
	})
	return stored, nil
}

// Cancel stops a WorkItem: it cancels every bound session, closes assignments
// and open attention requests, and is idempotent. A failed propagation leaves
// the item in "cancelling" (never silently "cancelled").
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
	assignments, err := c.work.ListAssignments(ctx, tenantID, item.ID)
	if err != nil {
		return err
	}
	for _, assignment := range assignments {
		if assignment.Status.Terminal() {
			continue
		}
		// A failed cancel leaves the assignment active so a retry can find it:
		// the work item stays "cancelling" instead of claiming a session stopped
		// when it did not.
		if assignment.SessionID != uuid.Nil {
			if err := c.launcher.CancelSession(ctx, tenantID, assignment.SessionID, reason); err != nil {
				failures = append(failures, fmt.Sprintf("assignment %s: %v", assignment.ID, err))
				continue
			}
			if err := c.sessions.SetState(ctx, tenantID, assignment.SessionID, SessionCancelled); err != nil &&
				!errors.Is(err, ErrSessionNotFound) {
				failures = append(failures, fmt.Sprintf("session %s: %v", assignment.SessionID, err))
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

func (c *Coordinator) checkTenant(tenantID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return newError(ErrTenantRequired, CodeTenantRequired, "", "tenant is required")
	}
	if tenantID != c.tenantID {
		return newError(ErrTenantMismatch, CodeTenantMismatch, "", "the tenant does not own this host")
	}
	return nil
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
