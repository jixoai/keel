package kernel

import (
	"context"
	"encoding/json"
	"fmt"
)

// Defaults mirror internal/store: a page is at most 1000 events, default 200.
const (
	defaultEventPage = 200
	maxEventPage     = 1000
	// scanChunk is the page size used by full-log projections.
	scanChunk = 500
)

// LoadEvents returns up to limit events with seq > afterSeq, in order. It is
// the read seam a session projection uses; it carries the tenant boundary, so
// a foreign or missing session answers ErrNotFound.
func (k *Kernel) LoadEvents(ctx context.Context, scope Scope, afterSeq int64, limit int) ([]Event, error) {
	if scope.IsZero() {
		return nil, k.reject(ReasonInvalidRequest, OpRead, scope, "", OwnerToken{}, "tenant and session are required", ErrInvalidRequest)
	}
	if limit <= 0 || limit > maxEventPage {
		limit = defaultEventPage
	}
	events, err := k.log.LoadEvents(ctx, scope.TenantID, scope.SessionID, afterSeq, limit)
	if err != nil {
		return nil, k.classifyStoreError(OpRead, CommitRequest{Scope: scope}, err)
	}
	return events, nil
}

// UnresolvedToolCalls scans the committed log and reports every assistant tool
// call that has no committed tool.result yet, oldest first. It is the recovery
// projection: a process that died after committing a call but before its
// result re-executes exactly these calls (at-least-once), and once a result is
// committed the call disappears from this list permanently (exactly-once in
// the log, because results are deduplicated by toolCallId).
func (k *Kernel) UnresolvedToolCalls(ctx context.Context, scope Scope) ([]ToolCallRef, error) {
	if scope.IsZero() {
		return nil, k.reject(ReasonInvalidRequest, OpRead, scope, "", OwnerToken{}, "tenant and session are required", ErrInvalidRequest)
	}
	type callState struct {
		ref      ToolCallRef
		resolved bool
	}
	var order []*callState
	byID := map[string]*callState{}

	var after int64
	for {
		events, err := k.log.LoadEvents(ctx, scope.TenantID, scope.SessionID, after, scanChunk)
		if err != nil {
			return nil, k.classifyStoreError(OpRead, CommitRequest{Scope: scope}, err)
		}
		for _, ev := range events {
			switch ev.Kind {
			case KindAssistantMessage:
				calls, err := toolCallBlocks(ev.Payload)
				if err != nil {
					continue
				}
				for _, c := range calls {
					if c.ID == "" {
						continue
					}
					if _, ok := byID[c.ID]; ok {
						continue
					}
					st := &callState{ref: ToolCallRef{ToolCallID: c.ID, ToolName: c.Name, Seq: ev.Seq}}
					byID[c.ID] = st
					order = append(order, st)
				}
			case KindToolResult:
				var p struct {
					ToolCallID string `json:"toolCallId"`
				}
				if err := json.Unmarshal(ev.Payload, &p); err != nil || p.ToolCallID == "" {
					continue
				}
				if st, ok := byID[p.ToolCallID]; ok {
					st.resolved = true
				}
			}
		}
		if len(events) == 0 {
			break
		}
		after = events[len(events)-1].Seq
		if len(events) < scanChunk {
			break
		}
	}
	var out []ToolCallRef
	for _, st := range order {
		if !st.resolved {
			out = append(out, st.ref)
		}
	}
	return out, nil
}

// EventText returns the text of a user.message payload, for callers that only
// need the human-facing part of the fact.
func EventText(ev Event) string {
	if ev.Kind != KindUserMessage {
		return ""
	}
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return ""
	}
	return p.Text
}

// String renders a tool call reference for diagnostics.
func (r ToolCallRef) String() string {
	return fmt.Sprintf("%s(%s)@%d", r.ToolCallID, r.ToolName, r.Seq)
}
