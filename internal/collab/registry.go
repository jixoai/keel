package collab

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// ModeRegistry is the tenant-scoped, assembly-time mode catalogue. Registration
// is the only way a mode becomes usable; the read-only projection is what a UI
// discovers. The registry belongs to exactly one tenant, so every descriptor's
// effective configuration carries that tenant even though a mode plugin's
// descriptor is tenant-agnostic and reusable.
//
// A mode is disabled, not forgotten: disabling stops new WorkItems/sessions
// but keeps the descriptor visible and every existing WorkItem/Assignment
// readable (see Coordinator read methods). Unknown ids are never silently
// created.
type ModeRegistry struct {
	mu       sync.RWMutex
	tenantID uuid.UUID
	ceiling  Limits
	modes    map[string]modeEntry
}

type modeEntry struct {
	descriptor ModeDescriptor
	enabled    bool
}

// NewModeRegistry builds an empty registry for one tenant under that tenant's
// ceiling. The ceiling is the hard upper bound for every registered mode; a
// descriptor declaring more is narrowed rather than trusted.
func NewModeRegistry(tenantID uuid.UUID, ceiling Limits) (*ModeRegistry, error) {
	if tenantID == uuid.Nil {
		return nil, newError(ErrTenantRequired, CodeTenantRequired, "new_registry", "tenant is required")
	}
	if err := ceiling.Validate(); err != nil {
		return nil, &Error{Kind: ErrInvalidMode, Code: CodeInvalidMode, Operation: "new_registry", Detail: "tenant ceiling: " + err.Error()}
	}
	return &ModeRegistry{tenantID: tenantID, ceiling: ceiling, modes: make(map[string]modeEntry)}, nil
}

// TenantID returns the tenant this catalogue belongs to.
func (r *ModeRegistry) TenantID() uuid.UUID {
	if r == nil {
		return uuid.Nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tenantID
}

// Ceiling returns the tenant-level hard cap.
func (r *ModeRegistry) Ceiling() Limits {
	if r == nil {
		return Limits{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ceiling
}

// Register adds one mode descriptor. The descriptor is validated, its limits
// are narrowed by the tenant ceiling, and it starts enabled. A duplicate id is
// a startup error; it never overwrites the first registration.
func (r *ModeRegistry) Register(descriptor ModeDescriptor) error {
	if r == nil {
		return newError(ErrInvalidMode, CodeInvalidMode, "register_mode", "registry is nil")
	}
	if err := descriptor.Validate(); err != nil {
		return err
	}
	descriptor = descriptor.Clone()
	descriptor.Limits = descriptor.Limits.NarrowedBy(r.ceiling)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.modes[descriptor.ID]; exists {
		return &Error{
			Kind:      ErrDuplicateMode,
			Code:      CodeDuplicateMode,
			Operation: "register_mode",
			ModeID:    descriptor.ID,
			Detail:    "mode " + descriptor.ID + " is already registered",
		}
	}
	r.modes[descriptor.ID] = modeEntry{descriptor: descriptor, enabled: true}
	return nil
}

// SetEnabled turns a registered mode on or off. It is the one admin mutation;
// an unknown id is an error, never an implicit registration.
func (r *ModeRegistry) SetEnabled(modeID string, enabled bool) error {
	if r == nil {
		return newError(ErrUnknownMode, CodeUnknownMode, "set_mode_enabled", "registry is nil")
	}
	modeID = strings.TrimSpace(modeID)
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.modes[modeID]
	if !ok {
		return &Error{Kind: ErrUnknownMode, Code: CodeUnknownMode, Operation: "set_mode_enabled", ModeID: modeID,
			Detail: fmt.Sprintf("mode %q is not registered", modeID)}
	}
	entry.enabled = enabled
	r.modes[modeID] = entry
	return nil
}

// Enabled reports whether a registered mode accepts new work. An unknown mode
// is false.
func (r *ModeRegistry) Enabled(modeID string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.modes[modeID]
	return ok && entry.enabled
}

// Descriptor returns a registered descriptor regardless of its enabled state.
// The second result is false for an unknown id.
func (r *ModeRegistry) Descriptor(modeID string) (ModeDescriptor, bool) {
	if r == nil {
		return ModeDescriptor{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.modes[modeID]
	if !ok {
		return ModeDescriptor{}, false
	}
	return entry.descriptor.Clone(), true
}

// RequireEnabled resolves a mode for new work. It returns ErrUnknownMode or
// ErrModeDisabled as a typed error; the descriptor itself is returned for the
// disabled case so the caller can report the mode's identity.
func (r *ModeRegistry) RequireEnabled(modeID string) (ModeDescriptor, error) {
	if r == nil {
		return ModeDescriptor{}, newError(ErrUnknownMode, CodeUnknownMode, "require_mode", "registry is nil")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.modes[modeID]
	if !ok {
		return ModeDescriptor{}, &Error{Kind: ErrUnknownMode, Code: CodeUnknownMode, Operation: "require_mode", ModeID: modeID,
			Detail: fmt.Sprintf("mode %q is not registered", modeID)}
	}
	if !entry.enabled {
		return entry.descriptor.Clone(), &Error{Kind: ErrModeDisabled, Code: CodeModeDisabled, Operation: "require_mode", ModeID: modeID,
			Detail: fmt.Sprintf("mode %q is disabled for new work", modeID)}
	}
	return entry.descriptor.Clone(), nil
}

// Get returns the read-only view of one mode (enabled state included).
func (r *ModeRegistry) Get(modeID string) (ModeView, bool) {
	if r == nil {
		return ModeView{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.modes[modeID]
	if !ok {
		return ModeView{}, false
	}
	return entry.descriptor.View(r.tenantID, entry.enabled), true
}

// List returns every registered mode sorted by id. Disabled modes stay in the
// list: existing work must remain discoverable after a disable.
func (r *ModeRegistry) List() []ModeView {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModeView, 0, len(r.modes))
	for _, entry := range r.modes {
		out = append(out, entry.descriptor.View(r.tenantID, entry.enabled))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
