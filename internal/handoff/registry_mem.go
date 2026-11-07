package handoff

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemRegistry is the in-memory Registry used by the protocol's tests. It
// reproduces the observable PostgreSQL semantics: upserts keyed by runtime id /
// handoff id, newest-first listings, and ErrUnknownRuntime/ErrUnknownHandoff on
// absent rows.
type MemRegistry struct {
	mu       sync.Mutex
	runtimes map[string]RuntimeRecord
	handoffs map[string]HandoffRecord
	now      func() time.Time
}

var _ Registry = (*MemRegistry)(nil)

// NewMemRegistry returns an empty in-memory registry.
func NewMemRegistry() *MemRegistry {
	return &MemRegistry{
		runtimes: map[string]RuntimeRecord{},
		handoffs: map[string]HandoffRecord{},
		now:      time.Now,
	}
}

// SetClock replaces the registry clock (test hook).
func (m *MemRegistry) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// SaveRuntime upserts one runtime row.
func (m *MemRegistry) SaveRuntime(ctx context.Context, rec RuntimeRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runtimes[rec.RuntimeID] = rec
	return nil
}

// Runtime reads one runtime row.
func (m *MemRegistry) Runtime(ctx context.Context, runtimeID string) (RuntimeRecord, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeRecord{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.runtimes[runtimeID]
	if !ok {
		return RuntimeRecord{}, ErrUnknownRuntime
	}
	return rec, nil
}

// Runtimes lists runtime rows, newest first.
func (m *MemRegistry) Runtimes(ctx context.Context, limit int) ([]RuntimeRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RuntimeRecord, 0, len(m.runtimes))
	for _, rec := range m.runtimes {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].RuntimeID > out[j].RuntimeID
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SaveHandoff inserts one handoff row (the id is the key; re-saving replaces).
func (m *MemRegistry) SaveHandoff(ctx context.Context, rec HandoffRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handoffs[rec.ID] = rec
	return nil
}

// FinishHandoff closes a started handoff row.
func (m *MemRegistry) FinishHandoff(ctx context.Context, id, status string, inFlight int, detail string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.handoffs[id]
	if !ok {
		return ErrUnknownHandoff
	}
	rec.Status = status
	rec.InFlight = inFlight
	rec.Detail = detail
	rec.FinishedAt = at
	m.handoffs[id] = rec
	return nil
}

// Handoff reads one row.
func (m *MemRegistry) Handoff(ctx context.Context, id string) (HandoffRecord, error) {
	if err := ctx.Err(); err != nil {
		return HandoffRecord{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.handoffs[id]
	if !ok {
		return HandoffRecord{}, ErrUnknownHandoff
	}
	return rec, nil
}

// ListHandoffs lists rows matching the query, newest first.
func (m *MemRegistry) ListHandoffs(ctx context.Context, q HandoffQuery) ([]HandoffRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []HandoffRecord
	for _, rec := range m.handoffs {
		if q.TenantID != uuid.Nil && rec.TenantID != q.TenantID {
			continue
		}
		if q.SessionID != uuid.Nil && rec.SessionID != q.SessionID {
			continue
		}
		if q.FromRuntime != "" && rec.FromRuntime != q.FromRuntime {
			continue
		}
		if q.ToRuntime != "" && rec.ToRuntime != q.ToRuntime {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}
