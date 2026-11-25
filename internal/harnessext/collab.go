package harnessext

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

// CollabSubagentLauncher adapts the existing SubagentRunner to the
// mode-agnostic collab.SessionLauncher port (change keel-collab-core).
//
// The adapter is deliberately thin: the Host has already checked tenant,
// bot.invoke, budget, depth/width and concurrency, while SpawnAsync still owns
// the existing child-session mechanics (idempotent child id, parent link,
// ownership inheritance and the turn starter). No behavior of the
// SubagentRunner or the `agent` tool changes: nothing calls the adapter in this
// change, and keel-collab-subagent is the change that maps the runner's depth
// semantics onto the mode limits.
type CollabSubagentLauncher struct {
	// Runner is lazy because the product assembly builds the launcher before
	// the runner exists in some boot orders.
	Runner func() *SubagentRunner
	// ModeID labels the created sessions. Empty defaults to collab.ModeSubagent.
	ModeID string
}

var _ collab.SessionLauncher = CollabSubagentLauncher{}

func (l CollabSubagentLauncher) runner() (*SubagentRunner, error) {
	if l.Runner == nil {
		return nil, fmt.Errorf("%w: no sub-agent runner is configured", ErrNotWired)
	}
	runner := l.Runner()
	if runner == nil {
		return nil, fmt.Errorf("%w: no sub-agent runner is configured", ErrNotWired)
	}
	return runner, nil
}

func (l CollabSubagentLauncher) modeID() string {
	if l.ModeID != "" {
		return l.ModeID
	}
	return collab.ModeSubagent
}

// Launch creates the child through the existing SpawnAsync path. The returned
// SessionRef carries the Host-computed depth and the mode identity; the runner
// keeps owning the child ledger.
func (l CollabSubagentLauncher) Launch(ctx context.Context, req collab.LaunchRequest) (collab.SessionRef, error) {
	runner, err := l.runner()
	if err != nil {
		return collab.SessionRef{}, err
	}
	if req.TenantID == uuid.Nil || req.ParentSessionID == uuid.Nil {
		return collab.SessionRef{}, fmt.Errorf("%w: tenant and parent session are required", collab.ErrInvalidCreation)
	}
	record, err := runner.SpawnAsync(ctx, SpawnRequest{
		TenantID:        req.TenantID,
		ParentSessionID: req.ParentSessionID,
		Task:            req.Task,
		BotID:           req.BotID,
		IdempotencyKey:  req.IdempotencyKey,
		Budget: Budget{
			MaxTurns:  req.Budget.MaxTurns,
			MaxTokens: req.Budget.MaxTokens,
			TimeoutMs: req.Budget.TimeoutMs,
		},
	})
	if err != nil {
		return collab.SessionRef{}, err
	}
	sessionID, err := uuid.Parse(record.SessionID)
	if err != nil {
		return collab.SessionRef{}, fmt.Errorf("harnessext: sub-agent record has an invalid session id: %w", err)
	}
	return collab.SessionRef{
		TenantID:        req.TenantID,
		SessionID:       sessionID,
		ParentSessionID: req.ParentSessionID,
		BotID:           req.BotID,
		ModeID:          l.modeID(),
		WorkItemID:      req.WorkItemID,
		Depth:           req.Depth,
		State:           collab.SessionActive,
	}, nil
}

// CancelSession stops the child's active run. The parent link is recovered
// from the warm registry and, after a restart, from the durable child input
// mark the runner already maintains.
func (l CollabSubagentLauncher) CancelSession(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) error {
	runner, err := l.runner()
	if err != nil {
		return err
	}
	parentID := uuid.Nil
	if record, ok := runner.Get(sessionID); ok {
		if parsed, parseErr := uuid.Parse(record.ParentSessionID); parseErr == nil {
			parentID = parsed
		}
	}
	if parentID == uuid.Nil {
		link, ok, err := runner.durableLink(ctx, tenantID, sessionID)
		if err != nil {
			return err
		}
		if !ok {
			return collab.ErrSessionNotFound
		}
		parentID = link.ParentSessionID
	}
	_, err = runner.Cancel(ctx, CancelRequest{
		TenantID:        tenantID,
		ParentSessionID: parentID,
		SessionID:       sessionID,
		Reason:          reason,
	})
	return err
}
