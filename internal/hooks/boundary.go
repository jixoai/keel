package hooks

import (
	"context"
	"fmt"
)

// BoundaryBuilder rebuilds the projected boundary context from the draft
// entries proposed so far. Returning an error marks the proposal invalid; later
// handlers still run and may repair it.
type BoundaryBuilder func(entries []BoundaryDraft) (BoundaryContext, error)

// BoundaryResult is the outcome of a turn_end or agent_before_settle dispatch.
// Continue is one-shot: the host makes exactly one next provider request when
// it is true. Valid is false when the final proposal failed validation; Entries
// is then empty and Continue false, matching PI.
type BoundaryResult struct {
	Entries  []BoundaryDraft
	Continue bool
	Context  BoundaryContext
	Valid    bool
}

// EmitTurnEnd runs turn_end handlers as an actionable boundary. Handlers see
// the entries and continuation decision proposed so far; each may replace
// Entries and request one continuation.
func (r *Registry) EmitTurnEnd(ctx context.Context, event TurnEndEvent, build BoundaryBuilder) BoundaryResult {
	if event.Entries == nil {
		event.Entries = []BoundaryDraft{}
	}
	return r.emitBoundary(ctx, &event, &event.BoundaryState, build)
}

// EmitAgentBeforeSettle runs agent_before_settle handlers as the final
// actionable boundary. Only the first continuation can start another
// low-level run; a later continuation request cannot extend it.
func (r *Registry) EmitAgentBeforeSettle(ctx context.Context, event AgentBeforeSettleEvent, build BoundaryBuilder) BoundaryResult {
	if event.Entries == nil {
		event.Entries = []BoundaryDraft{}
	}
	return r.emitBoundary(ctx, &event, &event.BoundaryState, build)
}

func (r *Registry) emitBoundary(ctx context.Context, event Event, state *BoundaryState, build BoundaryBuilder) BoundaryResult {
	valid := true
	if build != nil {
		next, err := build(state.Entries)
		if err != nil {
			valid = false
			r.reportError(HookError{Event: event.EventType(), Err: fmt.Errorf("invalid boundary entries: %w", err)})
		} else {
			state.Context = next
		}
	}

	for _, reg := range r.snapshot(event.EventType()) {
		inv := r.invoke(ctx, reg, event)
		if inv.err != nil {
			r.handleFailure(reg, event, inv.err)
			r.record(reg, event, inv, ActionAllow, "")
		} else if reg.Observer {
			r.record(reg, event, inv, ActionAllow, inv.decision.Reason)
		} else {
			action := inv.decision.Action.Normalized()
			if inv.decision.Entries != nil {
				state.Entries = inv.decision.Entries
				if action == ActionAllow {
					action = ActionModify
				}
			}
			if inv.decision.ContinueOverride != nil {
				state.Continue = *inv.decision.ContinueOverride
			} else if action == ActionContinue {
				state.Continue = true
			}
			// Boundaries have no cancel semantic in PI; a deny decision is
			// recorded but cannot cancel the run.
			r.record(reg, event, inv, action, inv.decision.Reason)
		}

		if build == nil {
			continue
		}
		next, err := build(state.Entries)
		if err != nil {
			valid = false
			r.reportError(HookError{
				Hook:  reg.Name,
				Event: event.EventType(),
				Err:   fmt.Errorf("invalid boundary entries: %w", err),
			})
			continue
		}
		state.Context = next
		valid = true
	}

	if !valid {
		return BoundaryResult{Entries: nil, Continue: false, Context: state.Context, Valid: false}
	}
	return BoundaryResult{
		Entries:  state.Entries,
		Continue: state.Continue,
		Context:  state.Context,
		Valid:    true,
	}
}
