package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/loop"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// Runner executes one claimed run: rebuild → (fill missing tool results) →
// loop → append events.
type Runner struct {
	Store        *store.Store
	Stream       provider.StreamFn
	ProviderName string
	Model        string
	APIKey       string
	Tools        *agenttool.ToolRegistry
	MaxTurns     int
	Logger       *slog.Logger
}

// NewRunner wires the production defaults: clock + echo tools and a turn cap.
func NewRunner(st *store.Store, stream provider.StreamFn, providerName, model, apiKey string, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	reg := agenttool.NewToolRegistry()
	if err := reg.Register(ClockTool{}); err != nil {
		logger.Error("register clock tool", "err", err)
	}
	if err := reg.Register(EchoTool{}); err != nil {
		logger.Error("register echo tool", "err", err)
	}
	return &Runner{
		Store:        st,
		Stream:       stream,
		ProviderName: providerName,
		Model:        model,
		APIKey:       apiKey,
		Tools:        reg,
		MaxTurns:     8,
		Logger:       logger,
	}
}

// Run executes the session once. The returned cursor is the last event sequence
// the run consumed or produced; the caller uses it to decide whether more input
// arrived while the run was executing.
func (r *Runner) Run(ctx context.Context, run store.Run) (int64, error) {
	events, cursor, err := r.loadEvents(ctx, run.TenantID, run.SessionID)
	if err != nil {
		return cursor, err
	}
	msgs, err := MessagesFromEvents(events)
	if err != nil {
		return cursor, err
	}
	if len(msgs) == 0 {
		return cursor, nil
	}
	// Nothing is owed to the model when the last committed message is a final
	// assistant answer. An assistant message with unresolved tool calls is NOT
	// final: fall through so the missing results are produced first.
	if last, ok := msgs[len(msgs)-1].(agentcore.AssistantMessage); ok && len(last.ToolCalls()) == 0 {
		return cursor, nil
	}

	// A previous process may have died after committing an assistant tool call
	// but before committing its results. Execute the missing calls now (at least
	// once) so the model sees a complete turn.
	msgs, cursor, err = r.fillMissingToolResults(ctx, run, msgs, cursor)
	if err != nil {
		return cursor, err
	}

	turns := 0
	cfg := loop.RunConfig{
		LoopConfig: loop.LoopConfig{
			Model:        r.Model,
			Provider:     r.ProviderName,
			APIKey:       r.APIKey,
			Stream:       r.Stream,
			ThinkingLevel: agentcore.ThinkingLow,
			// Compaction stays off until the checkpoint projection is wired to
			// the event log; the loop supports it via OnCompaction.
		},
		Batch: agenttool.BatchConfig{
			ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: r.Tools},
		},
		EventBuffer: 32,
		SessionID:   run.SessionID.String(),
		ShouldStopAfterTurn: func(ctx context.Context, agentCtx *agentcore.AgentContext) bool {
			turns++
			return r.MaxTurns > 0 && turns >= r.MaxTurns
		},
		OnCompaction: func(ctx context.Context, summary string, firstKeptIndex, summarizedCount int) {
			seq, err := r.appendEvent(ctx, run.TenantID, run.SessionID, store.EventCompaction, map[string]any{
				"summary":      summary,
				"firstKept":    firstKeptIndex,
				"summarized":   summarizedCount,
				"timestamp":    time.Now().UnixMilli(),
			})
			if err != nil {
				r.Logger.Warn("persist compaction", "err", err)
				return
			}
			cursor = seq
		},
	}
	agentCtx := &agentcore.AgentContext{
		Messages: msgs,
		Tools:    r.Tools.List(),
	}
	stream := loop.StartRun(ctx, agentCtx, cfg)
	for ev := range stream.Events() {
		if ctx.Err() != nil {
			return cursor, ctx.Err()
		}
		switch e := ev.(type) {
		case agentcore.MessageEndEvent:
			seq, err := r.appendEvent(ctx, run.TenantID, run.SessionID, store.EventAssistantMessage, e.Message)
			if err != nil {
				return cursor, err
			}
			cursor = seq
		case agentcore.ToolExecutionEndEvent:
			msg := agentcore.ToolResultMessage{
				RoleField:  agentcore.RoleToolResult,
				ToolCallID: e.ToolCallID,
				ToolName:   e.ToolName,
				Content:    e.Result.Content,
				Details:    e.Result.Details,
				IsError:    e.IsError,
			}
			seq, err := r.appendEvent(ctx, run.TenantID, run.SessionID, store.EventToolResult, msg)
			if err != nil {
				return cursor, err
			}
			cursor = seq
		}
	}
	if _, err := stream.Result(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return cursor, err
	}
	return cursor, nil
}

// fillMissingToolResults executes tool calls that a crashed process left
// unresolved. The call is at-least-once; tools are expected to be idempotent or
// to carry their own idempotency keys.
func (r *Runner) fillMissingToolResults(ctx context.Context, run store.Run, msgs agentcore.MessageList, cursor int64) (agentcore.MessageList, int64, error) {
	lastAssistant := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if _, ok := msgs[i].(agentcore.AssistantMessage); ok {
			lastAssistant = i
			break
		}
	}
	if lastAssistant < 0 {
		return msgs, cursor, nil
	}
	assistant, _ := msgs[lastAssistant].(agentcore.AssistantMessage)
	calls := assistant.ToolCalls()
	if len(calls) == 0 {
		return msgs, cursor, nil
	}
	done := map[string]bool{}
	for _, m := range msgs[lastAssistant+1:] {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			done[tr.ToolCallID] = true
		}
	}
	var missing []agentcore.AgentToolCall
	for _, c := range calls {
		if !done[c.ID] {
			missing = append(missing, agentcore.AgentToolCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
		}
	}
	if len(missing) == 0 {
		return msgs, cursor, nil
	}
	r.Logger.Warn("resuming unresolved tool calls", "session", run.SessionID, "count", len(missing))
	results, _ := agenttool.ExecuteToolCalls(ctx, agenttool.BatchConfig{
		ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: r.Tools},
	}, missing, nil)
	for _, tr := range results {
		msgs = append(msgs, tr)
		seq, err := r.appendEvent(ctx, run.TenantID, run.SessionID, store.EventToolResult, tr)
		if err != nil {
			return msgs, cursor, err
		}
		cursor = seq
	}
	return msgs, cursor, nil
}

func (r *Runner) loadEvents(ctx context.Context, tenantID, sessionID uuid.UUID) ([]store.Event, int64, error) {
	var all []store.Event
	var after int64
	for {
		page, err := r.Store.LoadEvents(ctx, tenantID, sessionID, after, 500)
		if err != nil {
			return nil, after, err
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		after = page[len(page)-1].Seq
		if len(page) < 500 {
			break
		}
	}
	return all, after, nil
}

func (r *Runner) appendEvent(ctx context.Context, tenantID, sessionID uuid.UUID, kind string, payload any) (int64, error) {
	seq, err := r.Store.AppendEvent(ctx, tenantID, sessionID, kind, payload)
	if err != nil {
		r.Logger.Error("append event", "kind", kind, "err", err)
		return 0, err
	}
	return seq, nil
}

// MarshalAssistant is a small helper for callers that persist a message outside
// the loop (tests, migrations).
func MarshalAssistant(m agentcore.AssistantMessage) (json.RawMessage, error) {
	return json.Marshal(m)
}
