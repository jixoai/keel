package agentext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNotWired reports that a backend exists but the service it delegates to is
// absent in this deployment (a nil getter, or a getter that resolves to nil).
// It is a wiring fact, not a backend call failure: the aggregate tools map it
// to the typed not_wired result, while an actual transport/plugin failure stays
// backend_error (or plugin_disabled for a disabled plugin).
var ErrNotWired = errors.New("agentext: backend is not wired")

// AgentBackend adapts the SubagentRunner to the harness-owned `agent` aggregate
// tool. The getter is lazy because the runner is created after the host
// assembly; at call time it resolves.
type AgentBackend struct {
	Runner func() *SubagentRunner
}

func (b AgentBackend) runner() (*SubagentRunner, error) {
	if b.Runner == nil {
		return nil, fmt.Errorf("%w: no sub-agent runner is configured", ErrNotWired)
	}
	runner := b.Runner()
	if runner == nil {
		return nil, fmt.Errorf("%w: no sub-agent runner is configured", ErrNotWired)
	}
	return runner, nil
}

// Wired reports whether the lazy getter resolves to a runner. It lets the
// aggregate tool answer not_wired for list/status too, instead of reporting
// "no children" for a deployment that simply has no sub-agent runtime.
func (b AgentBackend) Wired() bool {
	_, err := b.runner()
	return err == nil
}

func (b AgentBackend) Spawn(ctx context.Context, scope RunScope, args json.RawMessage) (SubagentResult, error) {
	runner, err := b.runner()
	if err != nil {
		return SubagentResult{}, err
	}
	return SpawnFromTool(WithRunScope(ctx, scope), runner, args)
}

func (b AgentBackend) Fork(ctx context.Context, scope RunScope, args json.RawMessage) (SubagentResult, error) {
	runner, err := b.runner()
	if err != nil {
		return SubagentResult{}, err
	}
	result, _, err := ForkFromTool(WithRunScope(ctx, scope), runner, args)
	return result, err
}

func (b AgentBackend) Resume(ctx context.Context, scope RunScope, args json.RawMessage) (ResumeResult, error) {
	runner, err := b.runner()
	if err != nil {
		return ResumeResult{}, err
	}
	return ResumeFromTool(WithRunScope(ctx, scope), runner, args)
}

func (b AgentBackend) Cancel(ctx context.Context, scope RunScope, args json.RawMessage) (CancelResult, error) {
	runner, err := b.runner()
	if err != nil {
		return CancelResult{}, err
	}
	return CancelFromTool(WithRunScope(ctx, scope), runner, args)
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
