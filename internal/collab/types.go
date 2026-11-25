package collab

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Built-in mode ids. A mode is a plugin contract, not a Bot kind: adding a
// mode never adds a Bot kind and never changes the bots table.
const (
	ModeSubagent   = "subagent"
	ModeSuperagent = "superagent"
)

// Topology is the shape of the collaboration graph a mode declares.
type Topology string

const (
	TopologyTree      Topology = "tree"
	TopologyWorkQueue Topology = "work-queue"
	TopologyPipeline  Topology = "pipeline"
	TopologyPeer      Topology = "peer"
)

// Valid reports whether the topology is in the closed vocabulary.
func (t Topology) Valid() bool {
	switch t {
	case TopologyTree, TopologyWorkQueue, TopologyPipeline, TopologyPeer:
		return true
	default:
		return false
	}
}

// WorkUnitKind is what a mode works on. delegation is a parent-to-child task;
// ticket is an independently tracked work item; job is a bounded execution.
type WorkUnitKind string

const (
	WorkUnitDelegation WorkUnitKind = "delegation"
	WorkUnitTicket     WorkUnitKind = "ticket"
	WorkUnitJob        WorkUnitKind = "job"
)

// Valid reports whether the work unit is in the closed vocabulary.
func (w WorkUnitKind) Valid() bool {
	switch w {
	case WorkUnitDelegation, WorkUnitTicket, WorkUnitJob:
		return true
	default:
		return false
	}
}

// Capability is an optional mode surface the deployment can advertise.
type Capability string

const (
	CapabilityAttention Capability = "attention"
	CapabilitySource    Capability = "source"
	CapabilityChannel   Capability = "channel"
)

// Valid reports whether the capability is in the closed vocabulary.
func (c Capability) Valid() bool {
	switch c {
	case CapabilityAttention, CapabilitySource, CapabilityChannel:
		return true
	default:
		return false
	}
}

// RoleSpec is one collaboration position a mode needs, expressed as a role
// (main, sub, worker, orchestrator, reviewer), never as a Bot kind. Max 0 means
// unbounded.
type RoleSpec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Min         int    `json:"min"`
	Max         int    `json:"max,omitempty"`
}

// UIEntry is the read-only discovery hint for a mode's UI surface. It is a
// label and a route, not an active UI registration.
type UIEntry struct {
	Label string `json:"label,omitempty"`
	Route string `json:"route,omitempty"`
}

// ModeDescriptor is what a mode plugin declares at assembly time. Every field
// the spec requires is here: id, roles, topology, work unit, inputs/outputs,
// capabilities, limits and the UI entry.
type ModeDescriptor struct {
	ID           string       `json:"id"`
	Title        string       `json:"title,omitempty"`
	Description  string       `json:"description,omitempty"`
	Roles        []RoleSpec   `json:"roles"`
	Topology     Topology     `json:"topology"`
	WorkUnit     WorkUnitKind `json:"workUnit"`
	Inputs       []string     `json:"inputs,omitempty"`
	Outputs      []string     `json:"outputs,omitempty"`
	Capabilities []Capability `json:"capabilities,omitempty"`
	Limits       Limits       `json:"limits"`
	UI           UIEntry      `json:"ui,omitempty"`
}

// Validate checks the descriptor's closed vocabularies and required fields.
func (d ModeDescriptor) Validate() error {
	id := strings.TrimSpace(d.ID)
	if id == "" {
		return invalidMode("id is required")
	}
	if len(id) > 64 {
		return invalidMode("id must be at most 64 characters")
	}
	for _, r := range id {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_'
		if !valid {
			return invalidMode("id must match [a-z0-9._-]+")
		}
	}
	if !d.Topology.Valid() {
		return invalidMode("topology " + string(d.Topology) + " is not in the closed vocabulary")
	}
	if !d.WorkUnit.Valid() {
		return invalidMode("workUnit " + string(d.WorkUnit) + " is not in the closed vocabulary")
	}
	if len(d.Roles) == 0 {
		return invalidMode("at least one role is required")
	}
	seenRoles := make(map[string]bool, len(d.Roles))
	for _, role := range d.Roles {
		name := strings.TrimSpace(role.Name)
		if name == "" {
			return invalidMode("role name is required")
		}
		if seenRoles[name] {
			return invalidMode("role " + name + " is declared twice")
		}
		seenRoles[name] = true
		if role.Min < 0 || role.Max < 0 {
			return invalidMode("role " + name + " has a negative bound")
		}
		if role.Max > 0 && role.Max < role.Min {
			return invalidMode("role " + name + " has max < min")
		}
	}
	seenCapabilities := make(map[Capability]bool, len(d.Capabilities))
	for _, capability := range d.Capabilities {
		if !capability.Valid() {
			return invalidMode("capability " + string(capability) + " is not in the closed vocabulary")
		}
		if seenCapabilities[capability] {
			return invalidMode("capability " + string(capability) + " is declared twice")
		}
		seenCapabilities[capability] = true
	}
	if !d.Limits.Depth.IsSet() {
		return invalidMode("an explicit depth policy is required")
	}
	if err := d.Limits.Validate(); err != nil {
		return &Error{Kind: ErrInvalidMode, Code: CodeInvalidMode, Operation: "register_mode", Detail: err.Error()}
	}
	return nil
}

// Clone returns a deep copy so a registry caller can never mutate shared
// slices through a returned descriptor.
func (d ModeDescriptor) Clone() ModeDescriptor {
	copied := d
	copied.Roles = append([]RoleSpec(nil), d.Roles...)
	copied.Inputs = append([]string(nil), d.Inputs...)
	copied.Outputs = append([]string(nil), d.Outputs...)
	copied.Capabilities = append([]Capability(nil), d.Capabilities...)
	return copied
}

// ModeView is the read-only projection of one registered mode. Limits are the
// effective limits after the tenant ceiling was applied, and TenantID is the
// owning tenant (the registry is tenant-scoped).
type ModeView struct {
	TenantID     uuid.UUID    `json:"tenantId"`
	ID           string       `json:"id"`
	Title        string       `json:"title,omitempty"`
	Description  string       `json:"description,omitempty"`
	Roles        []RoleSpec   `json:"roles"`
	Topology     Topology     `json:"topology"`
	WorkUnit     WorkUnitKind `json:"workUnit"`
	Inputs       []string     `json:"inputs,omitempty"`
	Outputs      []string     `json:"outputs,omitempty"`
	Capabilities []Capability `json:"capabilities,omitempty"`
	Limits       Limits       `json:"limits"`
	UI           UIEntry      `json:"ui,omitempty"`
	Enabled      bool         `json:"enabled"`
}

// View projects the descriptor with the owning tenant and the registry's
// enabled state.
func (d ModeDescriptor) View(tenantID uuid.UUID, enabled bool) ModeView {
	return ModeView{
		TenantID:     tenantID,
		ID:           d.ID,
		Title:        d.Title,
		Description:  d.Description,
		Roles:        append([]RoleSpec(nil), d.Roles...),
		Topology:     d.Topology,
		WorkUnit:     d.WorkUnit,
		Inputs:       append([]string(nil), d.Inputs...),
		Outputs:      append([]string(nil), d.Outputs...),
		Capabilities: append([]Capability(nil), d.Capabilities...),
		Limits:       d.Limits,
		UI:           d.UI,
		Enabled:      enabled,
	}
}

// SourceRef identifies where a WorkItem came from. Kind is an open string
// (manual, trigger, github, feishu, …) because external sources are plugins.
type SourceRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	URI  string `json:"uri,omitempty"`
}

// ResourceRef names one external or internal resource a WorkItem may touch. It
// is an allow-list entry, never a permission grant: the Bot's ordinary
// permissions still apply.
type ResourceRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Scope narrows what a WorkItem may reach. An empty Scope adds no restriction
// beyond the ordinary Bot permissions; a non-empty BotIDs list restricts which
// Bots the work may invoke.
type Scope struct {
	BotIDs    []string      `json:"botIds,omitempty"`
	Resources []ResourceRef `json:"resources,omitempty"`
}

func (s Scope) clone() Scope {
	copied := s
	copied.BotIDs = append([]string(nil), s.BotIDs...)
	copied.Resources = append([]ResourceRef(nil), s.Resources...)
	return copied
}

// WorkItemStatus is the durable lifecycle of a work unit.
type WorkItemStatus string

const (
	WorkItemOpen       WorkItemStatus = "open"
	WorkItemActive     WorkItemStatus = "active"
	WorkItemBlocked    WorkItemStatus = "blocked"
	WorkItemDone       WorkItemStatus = "done"
	WorkItemFailed     WorkItemStatus = "failed"
	WorkItemCancelling WorkItemStatus = "cancelling"
	WorkItemCancelled  WorkItemStatus = "cancelled"
)

// Closed reports whether no new assignment may start on the work item. A
// cancelling item is closed to new work even while propagation is retried.
func (s WorkItemStatus) Closed() bool {
	switch s {
	case WorkItemDone, WorkItemFailed, WorkItemCancelling, WorkItemCancelled:
		return true
	default:
		return false
	}
}

// WorkItem is one persistable unit of collaboration. It is independent of any
// Session: a worker session may end or be replaced while the WorkItem and its
// Assignments stay.
type WorkItem struct {
	ID       uuid.UUID       `json:"id"`
	TenantID uuid.UUID       `json:"tenantId"`
	ModeID   string          `json:"modeId"`
	Title    string          `json:"title"`
	Input    json.RawMessage `json:"input,omitempty"`
	Source   SourceRef       `json:"source"`
	Scope    Scope           `json:"scope,omitempty"`
	Limits   Limits          `json:"limits"`
	Status   WorkItemStatus  `json:"status"`
	// OwnerSessionID is the session that opened the work item (the main or
	// orchestrator). It is the bootstrap member for Assign/Ask; a work item
	// without an owner would have no session able to start its first
	// assignment.
	OwnerSessionID uuid.UUID `json:"ownerSessionId,omitempty"`
	IdempotencyKey string    `json:"idempotencyKey,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// AssignmentStatus is the lifecycle of one WorkItem → Bot/Session binding. It
// is independent of the bound Session's lifecycle.
type AssignmentStatus string

const (
	AssignmentOpen      AssignmentStatus = "open"
	AssignmentActive    AssignmentStatus = "active"
	AssignmentDone      AssignmentStatus = "done"
	AssignmentFailed    AssignmentStatus = "failed"
	AssignmentCancelled AssignmentStatus = "cancelled"
)

// Terminal reports whether the assignment is final and must not be retried.
func (s AssignmentStatus) Terminal() bool {
	switch s {
	case AssignmentDone, AssignmentFailed, AssignmentCancelled:
		return true
	default:
		return false
	}
}

// Assignment binds a WorkItem to a Bot and, once dispatched, a Session. A
// replacement worker gets a new Assignment; the previous one keeps its binding
// and result for audit.
type Assignment struct {
	ID              uuid.UUID        `json:"id"`
	TenantID        uuid.UUID        `json:"tenantId"`
	WorkItemID      uuid.UUID        `json:"workItemId"`
	ModeID          string           `json:"modeId"`
	BotID           string           `json:"botId"`
	SessionID       uuid.UUID        `json:"sessionId,omitempty"`
	CallerSessionID uuid.UUID        `json:"callerSessionId,omitempty"`
	Status          AssignmentStatus `json:"status"`
	Title           string           `json:"title,omitempty"`
	Input           json.RawMessage  `json:"input,omitempty"`
	Result          json.RawMessage  `json:"result,omitempty"`
	Error           string           `json:"error,omitempty"`
	IdempotencyKey  string           `json:"idempotencyKey,omitempty"`
	CreatedAt       time.Time        `json:"createdAt"`
	UpdatedAt       time.Time        `json:"updatedAt"`
}

// Attention kinds. They reuse the decision primitive's vocabulary rather than
// inventing a second approval semantic.
const (
	AttentionDecision = "decision"
	AttentionClarify  = "clarify"
	AttentionApproval = "approval"
)

// AttentionRoute is where a human decision request is delivered.
type AttentionRoute string

const (
	AttentionRouteSession AttentionRoute = "session"
	AttentionRouteHuman   AttentionRoute = "human"
	AttentionRouteChannel AttentionRoute = "channel"
)

// Valid reports whether the route is in the closed vocabulary.
func (r AttentionRoute) Valid() bool {
	switch r {
	case AttentionRouteSession, AttentionRouteHuman, AttentionRouteChannel:
		return true
	default:
		return false
	}
}

// AttentionStatus mirrors the decision primitive's terminal states.
type AttentionStatus string

const (
	AttentionOpen      AttentionStatus = "open"
	AttentionAnswered  AttentionStatus = "answered"
	AttentionTimedOut  AttentionStatus = "timed_out"
	AttentionCancelled AttentionStatus = "cancelled"
)

// Terminal reports whether the attention is final.
func (s AttentionStatus) Terminal() bool { return s != AttentionOpen }

// AttentionOption is one selectable answer.
type AttentionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// AttentionAnswer is the recorded decision: an option, free text, or both when
// the request allowed free text.
type AttentionAnswer struct {
	OptionID string `json:"optionId,omitempty"`
	Text     string `json:"text,omitempty"`
	// Actor is the authenticated human/operator who answered. It is recorded
	// with the answer so provenance does not depend on an audit row alone.
	Actor string    `json:"actor,omitempty"`
	At    time.Time `json:"at"`
}

// Attention is a structured human decision request attached to a WorkItem.
// The question, options, routing and deadline are fields; nothing is inferred
// from a free-text summary.
type Attention struct {
	ID            uuid.UUID         `json:"id"`
	TenantID      uuid.UUID         `json:"tenantId"`
	WorkItemID    uuid.UUID         `json:"workItemId"`
	AssignmentID  uuid.UUID         `json:"assignmentId,omitempty"`
	ModeID        string            `json:"modeId"`
	SessionID     uuid.UUID         `json:"sessionId,omitempty"`
	RequestID     string            `json:"requestId,omitempty"`
	Kind          string            `json:"kind"`
	Prompt        string            `json:"prompt"`
	Options       []AttentionOption `json:"options,omitempty"`
	AllowFreeText bool              `json:"allowFreeText"`
	Route         AttentionRoute    `json:"route"`
	Channel       string            `json:"channel,omitempty"`
	Status        AttentionStatus   `json:"status"`
	Answer        *AttentionAnswer  `json:"answer,omitempty"`
	CreatedAt     time.Time         `json:"createdAt"`
	Deadline      time.Time         `json:"deadline"`
}

func (a Attention) clone() Attention {
	copied := a
	copied.Options = append([]AttentionOption(nil), a.Options...)
	if a.Answer != nil {
		answer := *a.Answer
		copied.Answer = &answer
	}
	return copied
}

// LinkKind is the semantic relation between two endpoints. A link is not
// necessarily a parent/child edge.
type LinkKind string

const (
	LinkDelegation   LinkKind = "delegation"
	LinkAssignment   LinkKind = "assignment"
	LinkSubscription LinkKind = "subscription"
	LinkReview       LinkKind = "review"
)

// Valid reports whether the link kind is in the closed vocabulary.
func (k LinkKind) Valid() bool {
	switch k {
	case LinkDelegation, LinkAssignment, LinkSubscription, LinkReview:
		return true
	default:
		return false
	}
}

// LinkEndpointType is the endpoint namespace of a link.
type LinkEndpointType string

const (
	LinkEndpointSession    LinkEndpointType = "session"
	LinkEndpointWorkItem   LinkEndpointType = "work_item"
	LinkEndpointAssignment LinkEndpointType = "assignment"
	LinkEndpointBot        LinkEndpointType = "bot"
	LinkEndpointChannel    LinkEndpointType = "channel"
)

// LinkEndpoint names one end of a link.
type LinkEndpoint struct {
	Type LinkEndpointType `json:"type"`
	ID   string           `json:"id"`
}

// Link is a semantic relation between two endpoints, scoped to one tenant and
// optionally to one WorkItem.
type Link struct {
	ID         uuid.UUID    `json:"id"`
	TenantID   uuid.UUID    `json:"tenantId"`
	WorkItemID uuid.UUID    `json:"workItemId,omitempty"`
	Kind       LinkKind     `json:"kind"`
	From       LinkEndpoint `json:"from"`
	To         LinkEndpoint `json:"to"`
	CreatedAt  time.Time    `json:"createdAt"`
}

// SourceEvent is one external event read from a Source.
type SourceEvent struct {
	SourceID   string          `json:"sourceId"`
	Kind       string          `json:"kind"`
	ExternalID string          `json:"externalId,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	ReceivedAt time.Time       `json:"receivedAt"`
}

// SourcePage is one bounded read from a Source.
type SourcePage struct {
	Events     []SourceEvent `json:"events"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// Source is the read boundary for external events (GitHub, Feishu, a ticket
// system, …). A mode polls or subscribes through it; it has no session access.
type Source interface {
	Poll(ctx context.Context, tenantID uuid.UUID, cursor string, limit int) (SourcePage, error)
}

// ChannelMessage is one outbound message to a Channel.
type ChannelMessage struct {
	WorkItemID  uuid.UUID       `json:"workItemId,omitempty"`
	AttentionID uuid.UUID       `json:"attentionId,omitempty"`
	Text        string          `json:"text"`
	Data        json.RawMessage `json:"data,omitempty"`
	ThreadID    string          `json:"threadId,omitempty"`
}

// Channel is the write boundary for an external conversation surface.
type Channel interface {
	Send(ctx context.Context, tenantID uuid.UUID, channelID string, message ChannelMessage) error
}

// cloneRaw copies a JSON document so a stored record cannot be mutated through
// a caller's slice.
func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
