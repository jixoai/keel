package agentext

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// AgentBackend adapts the SubagentRunner to the harness-owned `agent` aggregate
// tool. The getter is lazy because the runner is created after the host
// assembly; at call time it resolves.
type AgentBackend struct {
	Runner func() *SubagentRunner
}

func (b AgentBackend) runner() (*SubagentRunner, error) {
	if b.Runner == nil {
		return nil, errors.New("agentext: no sub-agent runner is wired")
	}
	runner := b.Runner()
	if runner == nil {
		return nil, errors.New("agentext: no sub-agent runner is wired")
	}
	return runner, nil
}

func (b AgentBackend) Spawn(ctx context.Context, scope RunScope, args json.RawMessage) (SubagentResult, error) {
	runner, err := b.runner()
	if err != nil {
		return SubagentResult{}, err
	}
	return SpawnFromTool(WithRunScope(ctx, scope), runner, args)
}

func (b AgentBackend) List(tenantID uuid.UUID, parent *uuid.UUID) []SubagentRecord {
	runner, err := b.runner()
	if err != nil {
		return nil
	}
	return runner.List(tenantID, parent)
}

func (b AgentBackend) Get(sessionID uuid.UUID) (SubagentRecord, bool) {
	runner, err := b.runner()
	if err != nil {
		return SubagentRecord{}, false
	}
	return runner.Get(sessionID)
}
