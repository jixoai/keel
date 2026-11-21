package agent

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/sessionfork"
	"github.com/jixoai/keel/internal/store"
)

// ForkPrefixReader resolves the parent ledger prefix a forked session inherits.
// *store.Store implements it; a deployment without the fork columns leaves the
// runner's field nil and every session runs on its own log alone.
type ForkPrefixReader interface {
	SessionFork(ctx context.Context, tenantID, sessionID uuid.UUID) (sessionfork.Fact, bool, error)
}

// forkPrefixMessages loads the parent's committed events up to the fork origin
// and decodes them as the child's transcript prefix. The prefix is a read-time
// union, never a copy: the parent log remains the only storage of those facts.
//
// The prefix messages carry negative synthetic sequence numbers. They are
// history, not child inputs: inputSeqs only records seq > 0 (the child's own
// user messages), and every fact decision (owedTurn, unreadInputs, generation
// identity, cursor) still reads the child's log alone.
func (r *Runner) forkPrefixMessages(ctx context.Context, scope kernel.Scope) (agentcore.MessageList, []int64, error) {
	if r.ForkPrefixes == nil {
		return nil, nil, nil
	}
	fact, found, err := r.ForkPrefixes.SessionFork(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: resolve fork prefix: %w", err)
	}
	if !found || fact.OriginSeq <= 0 {
		return nil, nil, nil
	}
	parent := kernel.Scope{TenantID: scope.TenantID, SessionID: fact.ParentSessionID}
	var events []store.Event
	var after int64
	for after < fact.OriginSeq {
		page, err := r.Events.LoadEvents(ctx, parent, after, 500)
		if err != nil {
			return nil, nil, fmt.Errorf("agent: load fork prefix: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, ev := range page {
			if ev.Seq > fact.OriginSeq {
				break
			}
			events = append(events, store.Event{Seq: ev.Seq, Kind: string(ev.Kind), Payload: ev.Payload})
		}
		after = page[len(page)-1].Seq
		if len(page) < 500 {
			break
		}
	}
	if len(events) == 0 {
		return nil, nil, nil
	}
	msgs, _ := DecodeMessages(events, func(seq int64, kind string, decodeErr error) {
		r.Logger.Warn("skipping unreadable fork prefix event", "session_id", scope.SessionID,
			"parent_session_id", fact.ParentSessionID, "seq", seq, "kind", kind, "err", decodeErr)
	})
	seqs := make([]int64, len(msgs))
	for i := range seqs {
		// Negative, unique and never mistaken for a committed child input.
		seqs[i] = -int64(i + 1)
	}
	return msgs, seqs, nil
}
