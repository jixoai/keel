package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// PI boundary test: "chains shared draft proposals and preserves omitted result
// fields" (agent-session-boundaries.test.ts / extensions-runner.test.ts).
func TestBoundaryChainsDraftProposalsAndPreservesOmittedFields(t *testing.T) {
	reg, _ := newTestRegistry(t)
	type observation struct {
		entries   int
		continues bool
		preview   int
	}
	var observations []observation

	reg.On(EventAgentBeforeSettle, func(_ context.Context, event Event) (Decision, error) {
		settle := event.(*AgentBeforeSettleEvent)
		observations = append(observations, observation{
			entries:   len(settle.Entries),
			continues: settle.Continue,
			preview:   len(settle.Context.ContextEntries),
		})
		settle.Entries = append(settle.Entries, BoundaryDraft{Type: BoundaryDraftCustom, CustomType: "first"})
		return Decision{Action: ActionContinue}, nil
	})
	reg.On(EventAgentBeforeSettle, func(_ context.Context, event Event) (Decision, error) {
		settle := event.(*AgentBeforeSettleEvent)
		observations = append(observations, observation{
			entries:   len(settle.Entries),
			continues: settle.Continue,
			preview:   len(settle.Context.ContextEntries),
		})
		return Decision{Entries: []BoundaryDraft{}}, nil
	})

	build := func(entries []BoundaryDraft) (BoundaryContext, error) {
		return BoundaryContext{ContextEntries: make([]json.RawMessage, len(entries))}, nil
	}
	result := reg.EmitAgentBeforeSettle(context.Background(), AgentBeforeSettleEvent{
		BoundaryState: BoundaryState{Outcome: "completed"},
	}, build)

	want := []observation{
		{entries: 0, continues: false, preview: 0},
		{entries: 1, continues: true, preview: 1},
	}
	if len(observations) != len(want) {
		t.Fatalf("observations = %+v", observations)
	}
	for i := range want {
		if observations[i] != want[i] {
			t.Fatalf("observation[%d] = %+v, want %+v", i, observations[i], want[i])
		}
	}
	if len(result.Entries) != 0 {
		t.Fatalf("entries = %+v", result.Entries)
	}
	if !result.Continue {
		t.Fatal("continuation was not preserved")
	}
	if !result.Valid {
		t.Fatal("result should be valid")
	}
}

// PI: invalid boundary previews are reported; later handlers still run and may
// repair the proposal.
func TestBoundaryReportsInvalidPreviewAndLetsLaterHandlersRepair(t *testing.T) {
	reg, rec := newTestRegistry(t)
	secondRan := false
	reg.On(EventAgentBeforeSettle, func(context.Context, Event) (Decision, error) {
		return Decision{Entries: []BoundaryDraft{{
			Type:     BoundaryDraftContextEdit,
			TargetID: "missing",
		}}}, nil
	})
	reg.On(EventAgentBeforeSettle, func(_ context.Context, event Event) (Decision, error) {
		secondRan = true
		if len(event.(*AgentBeforeSettleEvent).Entries) != 1 {
			t.Fatalf("second handler saw %d entries", len(event.(*AgentBeforeSettleEvent).Entries))
		}
		return Decision{Entries: []BoundaryDraft{}}, nil
	})

	result := reg.EmitAgentBeforeSettle(context.Background(), AgentBeforeSettleEvent{
		BoundaryState: BoundaryState{Outcome: "completed"},
	}, func(entries []BoundaryDraft) (BoundaryContext, error) {
		for _, entry := range entries {
			if entry.Type == BoundaryDraftContextEdit {
				return BoundaryContext{}, errors.New("Entry missing not found")
			}
		}
		return BoundaryContext{CanContinue: true}, nil
	})

	if !secondRan {
		t.Fatal("second handler did not run")
	}
	if !result.Valid || len(result.Entries) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(rec.errors()) != 1 || !strings.Contains(rec.errors()[0].Error(), "Entry missing not found") {
		t.Fatalf("errors = %+v", rec.errors())
	}
}

// PI: "keeps shared mutations made before a handler throws".
func TestBoundaryKeepsMutationsMadeBeforeThrow(t *testing.T) {
	reg, rec := newTestRegistry(t)
	reg.On(EventAgentBeforeSettle, func(_ context.Context, event Event) (Decision, error) {
		settle := event.(*AgentBeforeSettleEvent)
		settle.Entries = append(settle.Entries, BoundaryDraft{Type: BoundaryDraftCustom, CustomType: "kept"})
		return Allow(), errors.New("boundary failed")
	})

	result := reg.EmitAgentBeforeSettle(context.Background(), AgentBeforeSettleEvent{
		BoundaryState: BoundaryState{Outcome: "completed"},
	}, func(entries []BoundaryDraft) (BoundaryContext, error) {
		return BoundaryContext{ContextEntries: make([]json.RawMessage, len(entries))}, nil
	})

	if len(result.Entries) != 1 || result.Entries[0].CustomType != "kept" {
		t.Fatalf("entries = %+v", result.Entries)
	}
	if len(rec.errors()) != 1 || !strings.Contains(rec.errors()[0].Error(), "boundary failed") {
		t.Fatalf("errors = %+v", rec.errors())
	}
}

// PI: continue requests exactly one next provider request (one-shot).
func TestBoundaryContinuationIsOneShot(t *testing.T) {
	reg, _ := newTestRegistry(t)
	requested := false
	reg.On(EventTurnEnd, func(context.Context, Event) (Decision, error) {
		if requested {
			return Allow(), nil
		}
		requested = true
		return ContinueBoundary(BoundaryDraft{
			Type:       BoundaryDraftCustomMessage,
			CustomType: "continuation",
			Content:    "continue now",
		}), nil
	})

	turns := 0
	for {
		turns++
		result := reg.EmitTurnEnd(context.Background(), TurnEndEvent{
			BoundaryState: BoundaryState{Outcome: "completed"},
		}, func(entries []BoundaryDraft) (BoundaryContext, error) {
			return BoundaryContext{CanContinue: len(entries) == 0 || entries[0].Type == BoundaryDraftCustomMessage}, nil
		})
		if !result.Valid {
			t.Fatalf("invalid boundary: %+v", result)
		}
		if turns == 1 {
			if len(result.Entries) != 1 || result.Entries[0].CustomType != "continuation" {
				t.Fatalf("entries = %+v", result.Entries)
			}
			if !result.Continue {
				t.Fatal("first turn did not request the continuation")
			}
			continue
		}
		if len(result.Entries) != 0 {
			t.Fatalf("continuation residue on turn %d: %+v", turns, result.Entries)
		}
		if !result.Continue {
			break
		}
		if turns > 3 {
			t.Fatal("continuation repeated forever")
		}
	}
	if turns != 2 {
		t.Fatalf("turns = %d, want 2", turns)
	}
}

// PI: when the final proposal does not validate, entries are discarded and
// continuation is suppressed.
func TestBoundaryInvalidFinalProposalReturnsEmpty(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventTurnEnd, func(context.Context, Event) (Decision, error) {
		return ContinueBoundary(BoundaryDraft{Type: BoundaryDraftContextEdit, TargetID: "missing"}), nil
	})
	result := reg.EmitTurnEnd(context.Background(), TurnEndEvent{
		BoundaryState: BoundaryState{Outcome: "completed"},
	}, func(entries []BoundaryDraft) (BoundaryContext, error) {
		if len(entries) > 0 {
			return BoundaryContext{}, errors.New("missing target")
		}
		return BoundaryContext{CanContinue: true}, nil
	})
	if result.Valid {
		t.Fatal("invalid proposal reported as valid")
	}
	if result.Entries != nil || result.Continue {
		t.Fatalf("result = %+v", result)
	}
}

// PI: agent_before_settle entries include custom messages and the outcome.
func TestAgentBeforeSettleCustomMessageDraft(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventAgentBeforeSettle, func(_ context.Context, event Event) (Decision, error) {
		settle := event.(*AgentBeforeSettleEvent)
		if settle.Outcome != "completed" {
			t.Fatalf("outcome = %q", settle.Outcome)
		}
		return ContinueBoundary(BoundaryDraft{
			Type:       BoundaryDraftCustomMessage,
			CustomType: "review-reminder",
			Content:    "Review the final diff before replying.",
			Display:    false,
		}), nil
	})
	result := reg.EmitAgentBeforeSettle(context.Background(), AgentBeforeSettleEvent{
		BoundaryState: BoundaryState{Outcome: "completed"},
	}, nil)
	if !result.Continue || len(result.Entries) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.Entries[0].CustomType != "review-reminder" || result.Entries[0].Content == "" {
		t.Fatalf("entry = %+v", result.Entries[0])
	}
}
