package harnessext

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

// CollabSubagentLauncher adapts the existing SubagentRunner to the
// mode-agnostic collab.SessionLauncher port (change keel-collab-core).
//
// The adapter is deliberately thin: the Host has already checked tenant,
// bot.invoke, budget, depth, width and concurrency, while spawnChild still owns
// the existing child-session mechanics (idempotent child id, parent link,
// durable depth fact, ownership inheritance and the turn starter). SpawnAsync is
// the guarded entry that calls the Host; the Host calls Launch, which calls
// spawnChild directly, so a spawn is never re-entered through the gate.
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

// Launch creates the child through spawnChild. The returned SessionRef carries
// the Host-computed depth and the mode identity; the runner keeps owning the
// child ledger.
func (l CollabSubagentLauncher) Launch(ctx context.Context, req collab.LaunchRequest) (collab.SessionRef, error) {
	runner, err := l.runner()
	if err != nil {
		return collab.SessionRef{}, err
	}
	if req.TenantID == uuid.Nil || req.ParentSessionID == uuid.Nil {
		return collab.SessionRef{}, fmt.Errorf("%w: tenant and parent session are required", collab.ErrInvalidCreation)
	}
	var input subagentLaunchInput
	if raw := bytes.TrimSpace(req.Inputs); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		if err := json.Unmarshal(raw, &input); err != nil {
			return collab.SessionRef{}, fmt.Errorf("%w: decode sub-agent input: %v", ErrInvalidSpawn, err)
		}
	}
	record, err := runner.spawnChild(ctx, SpawnRequest{
		TenantID:        req.TenantID,
		ParentSessionID: req.ParentSessionID,
		Task:            req.Task,
		Agent:           input.Agent,
		BotID:           req.BotID,
		Selection:       input.Selection,
		IdempotencyKey:  req.IdempotencyKey,
		Budget: Budget{
			MaxTurns:  req.Budget.MaxTurns,
			MaxTokens: req.Budget.MaxTokens,
			TimeoutMs: req.Budget.TimeoutMs,
		},
		MaxDepth:          input.MaxDepth,
		MaxChildren:       input.MaxChildren,
		Depth:             req.Depth,
		EffectiveMaxDepth: input.EffectiveMaxDepth,
		selectionChecked:  input.SelectionChecked,
		ModeID:            l.modeID(),
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
		Depth:           record.Depth,
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
