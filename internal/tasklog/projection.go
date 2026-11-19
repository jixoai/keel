package tasklog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// replayChunk mirrors kernel's page cap: a full-log replay reads 500 events at
// a time, so a long session never materializes its whole ledger in memory.
const replayChunk = 500

// RebuildSession regenerates one session's read models from its ledger. It is
// idempotent: the projection tables are replaced in one transaction with rows
// derived only from the committed task.* events, so rebuilding twice yields the
// same rows and a projection written around the ledger is erased (spec: 任务事件
// 是账本投影；直接写入会被一致性测试判失败).
func (s *Service) RebuildSession(ctx context.Context, scope kernel.Scope) (Stats, error) {
	if err := s.ready(); err != nil {
		return Stats{}, err
	}
	if scope.IsZero() {
		return Stats{}, fmt.Errorf("%w: tenant and session are required", ErrTaskRequired)
	}
	var ledger []kernel.Event
	var after int64
	for {
		page, err := s.reader.LoadEvents(ctx, scope, after, replayChunk)
		if err != nil {
			return Stats{}, fmt.Errorf("tasklog: rebuild read %s: %w", scope, err)
		}
		if len(page) == 0 {
			break
		}
		ledger = append(ledger, page...)
		after = page[len(page)-1].Seq
		if len(page) < replayChunk {
			break
		}
	}
	cards, events, err := buildProjection(scope, ledger, s.now)
	if err != nil {
		return Stats{}, err
	}
	if err := s.projection.ReplaceSession(ctx, scope.TenantID, scope.SessionID, cards, events); err != nil {
		return Stats{}, err
	}
	return Stats{Events: len(events), Cards: len(cards)}, nil
}

// buildProjection folds the task.* subset of a session ledger into cards and
// projected events. Non-task kinds are ignored; a malformed task payload is an
// error, because silently skipping it would make the projection disagree with
// the ledger forever.
func buildProjection(scope kernel.Scope, ledger []kernel.Event, now func() time.Time) ([]Card, []Event, error) {
	cards := make(map[uuid.UUID]*Card)
	order := make([]uuid.UUID, 0, len(ledger))
	events := make([]Event, 0, len(ledger))
	for _, committed := range ledger {
		if !IsTaskKind(committed.Kind) {
			continue
		}
		payload, err := decodeTaskPayload(string(committed.Kind), committed.Payload)
		if err != nil {
			return nil, nil, fmt.Errorf("tasklog: rebuild %s at seq %d: %w", committed.Kind, committed.Seq, err)
		}
		taskID, _ := uuid.Parse(payload.TaskID)
		event := Event{
			TaskID:    taskID,
			Kind:      string(committed.Kind),
			Seq:       committed.Seq,
			Payload:   append(json.RawMessage(nil), committed.Payload...),
			CreatedAt: now(),
		}
		events = append(events, event)
		switch committed.Kind {
		case kernel.KindTaskCreated:
			if _, seen := cards[taskID]; !seen {
				order = append(order, taskID)
			}
			card := cardFromCreated(scope, event, payload, now())
			cards[taskID] = &card
		case kernel.KindTaskUpdated, kernel.KindTaskClosed:
			if card, ok := cards[taskID]; ok {
				applyCardChange(card, event, payload, now())
			}
		}
	}
	out := make([]Card, 0, len(order))
	for _, id := range order {
		out = append(out, *cards[id])
	}
	return out, events, nil
}
