package tasklog

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// PgProjection is the PostgreSQL implementation of Projection. It maps the
// store's row types onto the tasklog domain types and translates the store's
// not-found sentinel, so callers only ever branch on tasklog errors.
type PgProjection struct {
	store *store.Store
}

// NewPGProjection wraps the application store.
func NewPGProjection(st *store.Store) *PgProjection {
	return &PgProjection{store: st}
}

func cardFromRow(row store.TaskCardRow) Card {
	return Card{
		TenantID:     row.TenantID,
		SessionID:    row.SessionID,
		TaskID:       row.TaskID,
		ParentTaskID: row.ParentTaskID,
		Title:        row.Title,
		Status:       row.Status,
		Origin:       row.Origin,
		OriginSeq:    row.OriginSeq,
		CreatedSeq:   row.CreatedSeq,
		UpdatedSeq:   row.UpdatedSeq,
		ClosedSeq:    row.ClosedSeq,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
}

func cardToRow(card Card) store.TaskCardRow {
	return store.TaskCardRow{
		TenantID:     card.TenantID,
		SessionID:    card.SessionID,
		TaskID:       card.TaskID,
		ParentTaskID: card.ParentTaskID,
		Title:        card.Title,
		Status:       card.Status,
		Origin:       card.Origin,
		OriginSeq:    card.OriginSeq,
		CreatedSeq:   card.CreatedSeq,
		UpdatedSeq:   card.UpdatedSeq,
		ClosedSeq:    card.ClosedSeq,
	}
}

func eventFromRow(row store.TaskEventRow) Event {
	return Event{
		TaskID:    row.TaskID,
		Kind:      row.Kind,
		Seq:       row.Seq,
		Payload:   row.Payload,
		CreatedAt: row.CreatedAt,
	}
}

// GetCard reads one card of a session.
func (p *PgProjection) GetCard(ctx context.Context, tenantID, sessionID, taskID uuid.UUID) (Card, error) {
	row, err := p.store.GetTaskCardRow(ctx, tenantID, sessionID, taskID)
	if errors.Is(err, store.ErrTaskCardNotFound) {
		return Card{}, ErrTaskNotFound
	}
	if err != nil {
		return Card{}, err
	}
	return cardFromRow(row), nil
}

// GetCardByID reads a card by its tenant-wide id.
func (p *PgProjection) GetCardByID(ctx context.Context, tenantID, taskID uuid.UUID) (Card, error) {
	row, err := p.store.GetTaskCardRowByID(ctx, tenantID, taskID)
	if errors.Is(err, store.ErrTaskCardNotFound) {
		return Card{}, ErrTaskNotFound
	}
	if err != nil {
		return Card{}, err
	}
	return cardFromRow(row), nil
}

// ListCards lists cards with the optional filters.
func (p *PgProjection) ListCards(ctx context.Context, tenantID uuid.UUID, sessionID, parentTaskID *uuid.UUID, status string) ([]Card, error) {
	rows, err := p.store.ListTaskCardRows(ctx, tenantID, sessionID, parentTaskID, status)
	if err != nil {
		return nil, err
	}
	out := make([]Card, 0, len(rows))
	for _, row := range rows {
		out = append(out, cardFromRow(row))
	}
	return out, nil
}

// ListEvents reads one card's projected events in ledger order.
func (p *PgProjection) ListEvents(ctx context.Context, tenantID, sessionID, taskID uuid.UUID) ([]Event, error) {
	rows, err := p.store.ListTaskEventRows(ctx, tenantID, sessionID, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		out = append(out, eventFromRow(row))
	}
	return out, nil
}

// CountEvents reports the projection size (consistency assertions).
func (p *PgProjection) CountEvents(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error) {
	return p.store.CountTaskEvents(ctx, tenantID, sessionID)
}

// UpsertCard advances the current-state row.
func (p *PgProjection) UpsertCard(ctx context.Context, card Card) error {
	return p.store.UpsertTaskCardRow(ctx, cardToRow(card))
}

// InsertEvent materializes one committed event.
func (p *PgProjection) InsertEvent(ctx context.Context, tenantID, sessionID uuid.UUID, event Event) error {
	return p.store.InsertTaskEventRow(ctx, store.TaskEventRow{
		TenantID:  tenantID,
		SessionID: sessionID,
		Seq:       event.Seq,
		TaskID:    event.TaskID,
		Kind:      event.Kind,
		Payload:   event.Payload,
	})
}

// ReplaceSession swaps one session's projection for a rebuilt one.
func (p *PgProjection) ReplaceSession(ctx context.Context, tenantID, sessionID uuid.UUID, cards []Card, events []Event) error {
	rows := make([]store.TaskCardRow, 0, len(cards))
	for _, card := range cards {
		rows = append(rows, cardToRow(card))
	}
	eventRows := make([]store.TaskEventRow, 0, len(events))
	for _, event := range events {
		eventRows = append(eventRows, store.TaskEventRow{
			TenantID:  tenantID,
			SessionID: sessionID,
			Seq:       event.Seq,
			TaskID:    event.TaskID,
			Kind:      event.Kind,
			Payload:   event.Payload,
		})
	}
	return p.store.ReplaceTaskSessionProjection(ctx, tenantID, sessionID, rows, eventRows)
}

var _ Projection = (*PgProjection)(nil)
