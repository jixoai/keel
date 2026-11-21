package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TriggerState is the durable catch-up cursor of one scheduled trigger. The
// trigger engine owns the semantics (which window was processed, why); this row
// is only the fact that survives a process restart.
//
// There is deliberately no secret here: webhook HMAC keys live in the trigger
// spec/environment and are never persisted with the cursor.
type TriggerState struct {
	TenantID        uuid.UUID
	TriggerID       string
	LastScheduledAt time.Time
	LastFiredAt     time.Time
	LastStatus      string
}

// LoadTriggerState reads one trigger's cursor. ok is false when the trigger has
// never processed a window (a fresh trigger must not catch up from zero).
func (s *Store) LoadTriggerState(ctx context.Context, tenantID uuid.UUID, triggerID string) (TriggerState, bool, error) {
	if tenantID == uuid.Nil || triggerID == "" {
		return TriggerState{}, false, fmt.Errorf("store: load trigger state: tenant and trigger id are required")
	}
	var state TriggerState
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id, trigger_id, last_scheduled_at, last_fired_at, last_status
		   FROM trigger_state
		  WHERE tenant_id = $1 AND trigger_id = $2`,
		tenantID, triggerID).
		Scan(&state.TenantID, &state.TriggerID, &state.LastScheduledAt, &state.LastFiredAt, &state.LastStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return TriggerState{}, false, nil
	}
	if err != nil {
		return TriggerState{}, false, fmt.Errorf("store: load trigger state: %w", err)
	}
	return state, true, nil
}

// SaveTriggerState upserts one trigger's cursor. It is monotonic in
// last_scheduled_at: a late writer (a slower fire or a replay) never moves the
// cursor backwards, which keeps a second process from re-catching an older
// window.
func (s *Store) SaveTriggerState(ctx context.Context, state TriggerState) error {
	if state.TenantID == uuid.Nil || state.TriggerID == "" || state.LastScheduledAt.IsZero() {
		return fmt.Errorf("store: save trigger state: tenant, trigger id and last scheduled window are required")
	}
	lastFiredAt := state.LastFiredAt
	if lastFiredAt.IsZero() {
		lastFiredAt = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO trigger_state
		     (tenant_id, trigger_id, last_scheduled_at, last_fired_at, last_status)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (tenant_id, trigger_id) DO UPDATE
		    SET last_scheduled_at = GREATEST(trigger_state.last_scheduled_at, EXCLUDED.last_scheduled_at),
		        last_fired_at     = CASE
		                                WHEN EXCLUDED.last_scheduled_at >= trigger_state.last_scheduled_at
		                                THEN EXCLUDED.last_fired_at
		                                ELSE trigger_state.last_fired_at
		                            END,
		        last_status       = CASE
		                                WHEN EXCLUDED.last_scheduled_at >= trigger_state.last_scheduled_at
		                                THEN EXCLUDED.last_status
		                                ELSE trigger_state.last_status
		                            END,
		        updated_at        = now()`,
		state.TenantID, state.TriggerID, state.LastScheduledAt, lastFiredAt, state.LastStatus)
	if err != nil {
		return fmt.Errorf("store: save trigger state: %w", err)
	}
	return nil
}
