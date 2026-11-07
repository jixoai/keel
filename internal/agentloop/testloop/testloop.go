// Package testloop provides a second agentloop.Loop implementation that exists
// to prove the loop slot is genuinely replaceable.
//
// It is deliberately minimal and deterministic: it never calls the model
// provider and never executes tools. It answers the last user message with a
// canned reply (prefix + the user text), emitting the minimal PI event shape
// (agent_start, turn_start, message_start/update/end, turn_end, agent_end) and
// settling with its produced messages.
//
// It MUST NOT enter a production loadout: because it never consults the
// provider, it cannot pass the PI conformance suite that gates every loop
// replacement (see internal/agentloop/README.md). Its purpose is:
//
//   - unit tests of this package and of the loop service interface, and
//   - loadout tests that need to swap the ctx.loop provider and observe the
//     replacement path without a model.
package testloop

import (
	"context"
	"errors"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/servicegraph"
)

// ErrNothingToAnswer mirrors agentloop's transcript guard: the canned loop
// needs a non-system trailing message to answer.
var ErrNothingToAnswer = errors.New("testloop: the transcript has nothing for the model to answer")

// Loop is the canned alternate loop. The zero value answers with the raw user
// text; Prefix decorates the reply.
type Loop struct {
	// Prefix is prepended to the answered text.
	Prefix string
}

// New builds a canned loop whose replies are Prefix + the answered text.
func New(prefix string) *Loop { return &Loop{Prefix: prefix} }

var (
	_ agentloop.Loop        = (*Loop)(nil)
	_ servicegraph.Instance = (*Loop)(nil)
)

// Start implements the servicegraph.Instance lifecycle: the canned loop holds
// no runtime effects, so it acquires nothing (it exists so the loop can be
// installed as a loadout service exactly like pi-loop).
func (l *Loop) Start(context.Context, *servicegraph.EffectScope) error { return nil }

// Stop implements the servicegraph.Instance lifecycle. See Start.
func (l *Loop) Stop(context.Context) error { return nil }

// Run implements agentloop.Loop. It reads the last non-system message, builds
// the canned reply and drives a minimal event sequence on the run stream.
func (l *Loop) Run(ctx context.Context, in agentloop.Input) (*agentloop.RunStream, error) {
	text, ok := lastText(in.Context.Messages)
	if !ok {
		return nil, ErrNothingToAnswer
	}
	runCtx, cancel := context.WithCancel(ctx)
	stream := agentloop.NewRunStream(in.Limits.EventBuffer, cancel)
	reply := agentcore.AssistantMessage{
		RoleField:    agentcore.RoleAssistant,
		Model:        in.Model.ID,
		Provider:     in.Model.Provider,
		Content:      agentcore.ContentList{agentcore.NewTextContent(l.Prefix + text)},
		StopReason:   agentcore.StopReasonEndTurn,
		ErrorMessage: "",
	}
	if runCtx.Err() != nil {
		reply.Content = nil
		reply.StopReason = agentcore.StopReasonAborted
	}
	result := []agentcore.AgentMessage{reply}
	go func() {
		// Emits are best-effort exactly like the PI loop: a cancelled run
		// still settles and sets its result.
		_ = stream.Emit(runCtx, agentcore.AgentStartEvent{SessionID: in.SessionID})
		_ = stream.Emit(runCtx, agentcore.TurnStartEvent{})
		_ = stream.Emit(runCtx, agentcore.MessageStartEvent{Message: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
		_ = stream.Emit(runCtx, agentcore.MessageUpdateEvent{Message: reply})
		_ = stream.Emit(runCtx, agentcore.MessageEndEvent{Message: reply})
		_ = stream.Emit(runCtx, agentcore.TurnEndEvent{Message: reply})
		_ = stream.Emit(runCtx, agentcore.AgentEndEvent{Messages: result})
		stream.Finish(result)
	}()
	return stream, nil
}

// lastText returns the text of the last non-system message (the one the canned
// loop answers): a user message's text, a tool result's text, or a compaction
// summary.
func lastText(msgs agentcore.MessageList) (string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		switch m := msgs[i].(type) {
		case agentcore.UserMessage:
			return agentcore.ContentToText(m.Content), true
		case agentcore.ToolResultMessage:
			return agentcore.ContentToText(m.Content), true
		case agentcore.CompactionMessage:
			return m.Summary, true
		case agentcore.SystemMessage:
			continue
		default:
			return "", false
		}
	}
	return "", false
}
