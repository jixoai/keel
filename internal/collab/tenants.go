package collab

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// ModeRegistrySource resolves the collaboration mode catalogue of one tenant.
// The Host asks for it on every request, so a deployment is not limited to the
// single registry it happened to construct during assembly.
type ModeRegistrySource interface {
	Registry(ctx context.Context, tenantID uuid.UUID) (*ModeRegistry, error)
}

// TenantRegistries is the lazy per-tenant registry cache. The factory owns how a
// tenant's modes are configured; this type only guarantees one registry per
// tenant, tenant identity and a single build under concurrency.
//
// The factory must not call back into Registry (the mutex is held while it
// runs); a deployment factory reads configuration, it does not re-enter the
// cache.
type TenantRegistries struct {
	mu         sync.Mutex
	factory    func(ctx context.Context, tenantID uuid.UUID) (*ModeRegistry, error)
	registries map[uuid.UUID]*ModeRegistry
}

// NewTenantRegistries builds an empty per-tenant cache.
func NewTenantRegistries(factory func(ctx context.Context, tenantID uuid.UUID) (*ModeRegistry, error)) (*TenantRegistries, error) {
	if factory == nil {
		return nil, newError(ErrInvalidRequest, CodeInvalidRequest, "new_registries", "a registry factory is required")
	}
	return &TenantRegistries{factory: factory, registries: make(map[uuid.UUID]*ModeRegistry)}, nil
}

// Registry returns the tenant's registry, building it on first use. A factory
// failure or a factory result that belongs to another tenant is a typed
// unknown-tenant refusal: the Host must not guess a catalogue.
func (t *TenantRegistries) Registry(ctx context.Context, tenantID uuid.UUID) (*ModeRegistry, error) {
	if t == nil || t.factory == nil {
		return nil, newError(ErrInvalidRequest, CodeInvalidRequest, "resolve_registry", "no registry source is wired")
	}
	if tenantID == uuid.Nil {
		return nil, newError(ErrTenantRequired, CodeTenantRequired, "resolve_registry", "tenant is required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if registry, ok := t.registries[tenantID]; ok {
		return registry, nil
	}
	registry, err := t.factory(ctx, tenantID)
	if err != nil {
		return nil, &Error{Kind: ErrUnknownTenant, Code: CodeTenantUnknown, Operation: "resolve_registry",
			TenantID: tenantID, Detail: err.Error()}
	}
	if registry == nil {
		return nil, &Error{Kind: ErrUnknownTenant, Code: CodeTenantUnknown, Operation: "resolve_registry",
			TenantID: tenantID, Detail: "the registry factory returned no registry"}
	}
	if owner := registry.TenantID(); owner != tenantID {
		return nil, &Error{Kind: ErrTenantMismatch, Code: CodeTenantMismatch, Operation: "resolve_registry",
			TenantID: tenantID, Detail: "the registry factory returned another tenant's catalogue"}
	}
	t.registries[tenantID] = registry
	return registry, nil
}

// List projects the tenant's registered modes. It is the read-side companion of
// Host registry resolution.
func (t *TenantRegistries) List(ctx context.Context, tenantID uuid.UUID) ([]ModeView, error) {
	registry, err := t.Registry(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return registry.List(), nil
}
