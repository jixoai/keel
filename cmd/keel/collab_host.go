// Production assembly of the guarded collaboration Host (change
// keel-collab-subagent). The core contract lives in internal/collab; this file
// supplies the four product seams it requires:
//
//   - SessionIndex: durable session state + the spawn link (ledger first, the
//     sessions.subagent_* projection as the queryable fallback);
//   - PermissionChecker: bot.invoke on the target Bot, evaluated as the caller
//     session's owning account/Bot (never as an inherited super-principal);
//   - AuditSink: append-only auth_events rows (ids, codes and numbers only);
//   - SessionLauncher: harnessext.CollabSubagentLauncher over the existing
//     SubagentRunner.
//
// The runner reaches the Host through spawnAsync; the Host reaches the runner
// through the launcher. Neither side constructs a child session without the
// other, which is what makes the depth/width/permission checks unbypassable.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// newCollabHost builds the deployment's single guarded Host and installs it on
// the sub-agent runner. It must be called after the mode registry and the
// runner exist; the launcher resolves the runner lazily, so the cycle is closed
// here.
func newCollabHost(
	tenantID uuid.UUID,
	modes *collab.ModeRegistry,
	st *store.Store,
	events kernel.Reader,
	accessBridge *accesswire.Bridge,
	subagents *harnessext.SubagentRunner,
	log *slog.Logger,
) (*collab.Coordinator, error) {
	if log == nil {
		log = slog.Default()
	}
	host, err := collab.NewHost(collab.HostOptions{
		TenantID: tenantID,
		Modes:    modes,
		// The sub-agent mode creates bare delegations, not WorkItems. The
		// in-memory store keeps Host creation correct without inventing a
		// persistence decision the super-agent change owns.
		Work:        collab.NewMemoryWorkStore(nil),
		Sessions:    collabSessionIndex{store: st, events: events},
		Permissions: collabPermissionChecker{store: st, access: accessBridge},
		Audit:       collabAuditSink{store: st, log: log},
		Launcher: harnessext.CollabSubagentLauncher{
			Runner: func() *harnessext.SubagentRunner { return subagents },
			ModeID: collab.ModeSubagent,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("collaboration host: %w", err)
	}
	if err := subagents.SetHost(host); err != nil {
		return nil, fmt.Errorf("collaboration host: %w", err)
	}
	return host, nil
}

// collabSessionIndex is the durable collab.SessionIndex. Depth and parent come
// from the child's first user.message (DurableSubagentLink); the sessions
// projection is the fallback and the source of active-child counts.
type collabSessionIndex struct {
	store  *store.Store
	events kernel.Reader
}

var _ collab.SessionIndex = collabSessionIndex{}

func (i collabSessionIndex) Session(ctx context.Context, tenantID, sessionID uuid.UUID) (collab.SessionRef, error) {
	info, err := i.store.GetSessionInfo(ctx, tenantID, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNoSession) {
			return collab.SessionRef{}, collab.ErrSessionNotFound
		}
		return collab.SessionRef{}, err
	}
	ref := collab.SessionRef{
		TenantID:  tenantID,
		SessionID: sessionID,
		BotID:     info.BotID,
		State:     collab.SessionActive,
	}
	if info.Archived {
		ref.State = collab.SessionDone
	}
	switch info.State {
	case "cancelled":
		ref.State = collab.SessionCancelled
	case "done":
		ref.State = collab.SessionDone
	}
	// The ledger is the restart truth; a link there that lacks a depth is a
	// fail-closed error (DurableSubagentLink), not a guessed root.
	if i.events != nil {
		link, ok, err := harnessext.DurableSubagentLink(ctx, i.events, tenantID, sessionID)
		if err != nil {
			return collab.SessionRef{}, err
		}
		if ok {
			ref.ParentSessionID = link.ParentSessionID
			ref.Depth = link.Depth
			ref.ModeID = link.ModeID
			return ref, nil
		}
	}
	// Projection fallback: a spawn that crashed after the link write but before
	// the first input still has a truthful parent/depth; treating it as a root
	// would let it spawn past the cap.
	link, ok, err := i.store.SubagentLink(ctx, tenantID, sessionID)
	if err != nil {
		return collab.SessionRef{}, err
	}
	if ok {
		ref.ParentSessionID = link.ParentSessionID
		ref.Depth = link.Depth
		ref.ModeID = link.ModeID
	}
	return ref, nil
}

// Put is a no-op: the launcher writes the durable ledger link and the sessions
// projection before the child's first input, so putting the ref again would be
// a second (weaker) source of the same fact.
func (i collabSessionIndex) Put(context.Context, collab.SessionRef) error { return nil }

// SetWorkItem exists for the generic Host contract. A sub-agent delegation has
// no WorkItem; refusing a non-nil binding is honest rather than pretending the
// session belongs to one. The super-agent mode supplies its own index.
func (i collabSessionIndex) SetWorkItem(_ context.Context, _, _, workItemID uuid.UUID) error {
	if workItemID == uuid.Nil {
		return nil
	}
	return fmt.Errorf("%w: sub-agent sessions do not use work items", collab.ErrInvalidRequest)
}

func (i collabSessionIndex) SetState(ctx context.Context, tenantID, sessionID uuid.UUID, state collab.SessionState) error {
	return i.store.SetSessionState(ctx, tenantID, sessionID, string(state))
}

// Children walks the durable spawn projection. The cancel path resolves each
// child's live state with Session before acting, so the projection only needs
// identity, parent, mode and depth here.
func (i collabSessionIndex) Children(ctx context.Context, tenantID, parentSessionID uuid.UUID) ([]collab.SessionRef, error) {
	rows, err := i.store.SubagentChildren(ctx, tenantID, parentSessionID)
	if err != nil {
		return nil, err
	}
	out := make([]collab.SessionRef, 0, len(rows))
	for _, row := range rows {
		out = append(out, collab.SessionRef{
			TenantID:        row.TenantID,
			SessionID:       row.SessionID,
			ParentSessionID: row.ParentSessionID,
			ModeID:          row.ModeID,
			Depth:           row.Depth,
		})
	}
	return out, nil
}

func (i collabSessionIndex) ActiveChildren(ctx context.Context, tenantID, parentSessionID uuid.UUID) (int, error) {
	return i.store.CountActiveSubagentChildren(ctx, tenantID, parentSessionID)
}

func (i collabSessionIndex) ActiveInMode(ctx context.Context, tenantID uuid.UUID, modeID string) (int, error) {
	return i.store.CountActiveSubagentSessions(ctx, tenantID, modeID)
}

// collabSubagentLinks adapts the sessions projection to the runner's link
// seam. The store stays below harnessext (it does not import it); this adapter
// lives in the assembly layer that already knows both vocabularies.
type collabSubagentLinks struct {
	store *store.Store
}

var _ harnessext.SubagentLinkStore = collabSubagentLinks{}

func (l collabSubagentLinks) RecordSubagentLink(ctx context.Context, link harnessext.SubagentLink) error {
	var budget []byte
	if link.Budget != nil {
		encoded, err := json.Marshal(link.Budget)
		if err != nil {
			return fmt.Errorf("collab: encode sub-agent budget: %w", err)
		}
		budget = encoded
	}
	return l.store.RecordSubagentLink(ctx, store.SubagentLinkRow{
		TenantID:        link.TenantID,
		SessionID:       link.SessionID,
		ParentSessionID: link.ParentSessionID,
		ModeID:          link.ModeID,
		Depth:           link.Depth,
		MaxDepth:        link.MaxDepth,
		MaxChildren:     link.MaxChildren,
		Budget:          budget,
	})
}

func (l collabSubagentLinks) SubagentLink(ctx context.Context, tenantID, childID uuid.UUID) (harnessext.SubagentLink, bool, error) {
	row, ok, err := l.store.SubagentLink(ctx, tenantID, childID)
	if err != nil || !ok {
		return harnessext.SubagentLink{}, ok, err
	}
	return subagentLinkFromRow(row), true, nil
}

func (l collabSubagentLinks) SubagentChildren(ctx context.Context, tenantID, parentID uuid.UUID) ([]harnessext.SubagentLink, error) {
	rows, err := l.store.SubagentChildren(ctx, tenantID, parentID)
	if err != nil {
		return nil, err
	}
	out := make([]harnessext.SubagentLink, 0, len(rows))
	for _, row := range rows {
		out = append(out, subagentLinkFromRow(row))
	}
	return out, nil
}

// SetSubagentState persists a cancelled/done lifecycle mark on the session row.
func (l collabSubagentLinks) SetSubagentState(ctx context.Context, tenantID, childID uuid.UUID, state string) error {
	return l.store.SetSessionState(ctx, tenantID, childID, state)
}

// DeleteSubagentLink clears a ghost link whose input never committed.
func (l collabSubagentLinks) DeleteSubagentLink(ctx context.Context, tenantID, childID uuid.UUID) error {
	return l.store.DeleteSubagentLink(ctx, tenantID, childID)
}

// ActiveSubagentChildren counts live direct children for the runner's defensive
// width check.
func (l collabSubagentLinks) ActiveSubagentChildren(ctx context.Context, tenantID, parentID uuid.UUID) (int, error) {
	return l.store.CountActiveSubagentChildren(ctx, tenantID, parentID)
}

func subagentLinkFromRow(row store.SubagentLinkRow) harnessext.SubagentLink {
	link := harnessext.SubagentLink{
		TenantID: row.TenantID, SessionID: row.SessionID, ParentSessionID: row.ParentSessionID,
		ModeID: row.ModeID, Depth: row.Depth, MaxDepth: row.MaxDepth, MaxChildren: row.MaxChildren,
		RunStatus: row.RunStatus,
	}
	if len(row.Budget) > 0 {
		var budget harnessext.Budget
		if json.Unmarshal(row.Budget, &budget) == nil {
			link.Budget = &budget
		}
	}
	return link
}

// ListSubagentTree implements harnessapi.SubagentLinkReader: the durable
// delegation tree under one root, for the post-restart read.
func (l collabSubagentLinks) ListSubagentTree(ctx context.Context, tenantID, rootSessionID uuid.UUID) ([]harnessext.SubagentLink, error) {
	rows, err := l.store.SubagentTree(ctx, tenantID, rootSessionID)
	if err != nil {
		return nil, err
	}
	out := make([]harnessext.SubagentLink, 0, len(rows))
	for _, row := range rows {
		link := harnessext.SubagentLink{
			TenantID: tenantID, SessionID: row.SessionID, ParentSessionID: row.ParentSessionID,
			ModeID: row.ModeID, Depth: row.Depth, MaxDepth: row.MaxDepth, MaxChildren: row.MaxChildren,
			RunStatus: row.RunStatus,
		}
		if len(row.Budget) > 0 {
			var budget harnessext.Budget
			if json.Unmarshal(row.Budget, &budget) == nil {
				link.Budget = &budget
			}
		}
		out = append(out, link)
	}
	return out, nil
}

// collabAuditSink appends collaboration audit facts to auth_events. Detail never
// carries a task body, tool output or a credential: ids, codes and budget
// numbers only.
type collabAuditSink struct {
	store *store.Store
	log   *slog.Logger
}

var _ collab.AuditSink = collabAuditSink{}

func (a collabAuditSink) Record(ctx context.Context, event collab.AuditEvent) error {
	if a.store == nil {
		return errors.New("collab: audit store is not wired")
	}
	result := store.AuthResultOK
	switch event.Result {
	case "denied":
		result = store.AuthResultDenied
	case "failed":
		result = store.AuthResultFailed
	}
	detail := map[string]any{"result": event.Result}
	add := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			detail[key] = value
		}
	}
	add("operation", event.Operation)
	add("mode_id", event.ModeID)
	add("reason", event.Reason)
	add("bot_id", event.BotID)
	add("field", event.Field)
	add("detail", event.Detail)
	if event.WorkItemID != uuid.Nil {
		detail["work_item_id"] = event.WorkItemID.String()
	}
	if event.AssignmentID != uuid.Nil {
		detail["assignment_id"] = event.AssignmentID.String()
	}
	if event.SessionID != uuid.Nil {
		detail["session_id"] = event.SessionID.String()
	}
	if event.ParentSessionID != uuid.Nil {
		detail["parent_session_id"] = event.ParentSessionID.String()
	}
	if event.Limit != 0 {
		detail["limit"] = event.Limit
	}
	if event.Actual != 0 {
		detail["actual"] = event.Actual
	}
	if _, err := a.store.RecordAuthEvent(ctx, store.AuthEvent{
		TenantID: event.TenantID,
		Kind:     event.Kind,
		Result:   result,
		Detail:   detail,
	}); err != nil {
		a.log.Warn("collaboration audit write failed", "kind", event.Kind, "operation", event.Operation, "err", err)
		return err
	}
	return nil
}

// collabPermissionChecker maps the Host's bot.invoke check onto the same access
// bridge the HTTP and tool edges use. The caller principal is the session's
// owning account/Bot: a delegation never lends the parent's *actor* more
// authority than the session itself holds.
type collabPermissionChecker struct {
	store  *store.Store
	access *accesswire.Bridge
}

var _ collab.PermissionChecker = collabPermissionChecker{}

func (c collabPermissionChecker) CheckBotInvoke(ctx context.Context, tenantID uuid.UUID, caller collab.SessionRef, targetBotID string) error {
	targetBotID = strings.TrimSpace(targetBotID)
	if targetBotID == "" {
		return errors.New("collab: bot.invoke target is required")
	}
	if c.access == nil {
		return errors.New("collab: the identity surface is not wired in this deployment")
	}
	ownership, err := c.store.GetSessionOwnership(ctx, tenantID, caller.SessionID)
	if err != nil {
		return err
	}
	// A spawn naming no target runs the parent's own Bot. The existing tool and
	// HTTP edges deliberately require no extra capability for that case (it is
	// not a cross-Bot delegation), so the Host's check keeps the same rule:
	// bot.invoke is required only for a *different* target Bot.
	if ownership.BotID != "" && ownership.BotID == targetBotID {
		return nil
	}
	accountID := ownership.AccountID
	if accountID == uuid.Nil {
		// An unowned session (pre-identity data) may still name an owner through
		// the relation tuple. Without any account principal, fail closed.
		owner, found, ownerErr := c.store.SessionOwnerAccount(ctx, tenantID, caller.SessionID)
		if ownerErr != nil {
			return ownerErr
		}
		if !found {
			return errors.New("collab: the caller session has no account principal")
		}
		accountID = owner
	}
	principal := access.Principal{
		TenantID:  tenantID,
		AccountID: accountID,
		BotID:     ownership.BotID,
	}
	// A Bot's effective authority is Account grants ∩ its own Harness. The
	// super-agent orchestrator's bot.invoke can be a harness permission rather
	// than an account grant; loading the same harness set the runs use keeps the
	// Host's check and the runner's defensive check aligned with the product
	// policy.
	if ownership.BotID != "" {
		permissions, permErr := c.access.BotHarnessPermissions(ctx, tenantID, ownership.BotID, accountID)
		if permErr != nil {
			return permErr
		}
		principal.HarnessPermissions = permissions
	}
	decision, err := c.access.Authorize(ctx, principal, access.KeyBotInvoke, access.Resource{
		TenantID: tenantID,
		Type:     "bot",
		ID:       targetBotID,
	})
	if err != nil {
		return err
	}
	if decision.Denied() {
		return fmt.Errorf("collab: bot.invoke on %q is denied", targetBotID)
	}
	return nil
}
