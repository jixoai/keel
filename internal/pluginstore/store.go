// Package pluginstore is the plugin platform's storage contract (change
// keel-plugin-split).
//
// The platform owns one tenant-scoped state surface for every plugin:
//
//   - opaque per-plugin state keys,
//   - one opaque JSON configuration document per plugin,
//   - the dependency status the host computed for the last assembly.
//
// The install/version inventory stays with the core store and harnessext
// manager; it has one writer, not a second plugin-platform copy.
//
// Domain data does NOT live here. Only plugins with an independent domain
// (board and superagent in this change) declare a DomainStore; attachment,
// telemetry, commands, UI, MCP and skills keep using platform/core services and
// add no schema of their own.
//
// The contract is dialect-neutral and carries no driver. PostgreSQL is the only
// adapter shipped in this change; a later adapter (for example an embedded
// store) implements the same interface. SQLite is deliberately not added:
// a deployment either has PostgreSQL or it does not persist plugin state.
package pluginstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is the single not-found answer. A foreign tenant's row and a
// missing row are indistinguishable on purpose.
var ErrNotFound = errors.New("pluginstore: not found")

// Dependency states are the host's assembly answer for one plugin.
const (
	DependencyReady   = "ready"
	DependencyPending = "pending"
	DependencyFailed  = "failed"
	DependencyStopped = "stopped"
)

// The install inventory row (`plugins`) is owned by the core store and the
// harnessext plugin manager (store.UpsertPlugin). pluginstore deliberately has
// no install write path: two writers would overwrite each other's version,
// state and timestamps.

// StateEntry is one opaque plugin state key. Values are strings; a plugin that
// needs structure owns its encoding.
type StateEntry struct {
	PluginID  string
	Key       string
	Value     string
	UpdatedAt time.Time
}

// Config is one plugin's opaque configuration document.
type Config struct {
	PluginID  string
	Document  json.RawMessage
	UpdatedAt time.Time
}

// Dependency is the assembly answer persisted for one plugin: what it required
// and whether those requirements were satisfied.
type Dependency struct {
	PluginID string
	// Requires is the manifest's requirement list, in declaration order.
	Requires []string
	// State is one of DependencyReady/Pending/Failed/Stopped.
	State string
	// Reason is the host's diagnostic (missing requirements, start failure).
	Reason    string
	UpdatedAt time.Time
}

// Store is the platform storage port. Every method is tenant-scoped and takes
// the tenant explicitly; there is no deployment-level branch.
type Store interface {
	// Opaque plugin state.
	GetState(ctx context.Context, tenantID uuid.UUID, pluginID, key string) (string, bool, error)
	PutState(ctx context.Context, tenantID uuid.UUID, pluginID, key, value string) error
	DeleteState(ctx context.Context, tenantID uuid.UUID, pluginID, key string) (bool, error)
	ListState(ctx context.Context, tenantID uuid.UUID, pluginID string) ([]StateEntry, error)

	// Opaque plugin configuration.
	GetConfig(ctx context.Context, tenantID uuid.UUID, pluginID string) (Config, bool, error)
	PutConfig(ctx context.Context, tenantID uuid.UUID, pluginID string, document json.RawMessage) (Config, error)

	// Dependency status.
	PutDependency(ctx context.Context, tenantID uuid.UUID, dependency Dependency) (Dependency, error)
	ListDependencies(ctx context.Context, tenantID uuid.UUID) ([]Dependency, error)
}

// DomainStore is the independent-domain storage port. Only board and superagent
// implement it. The host uses it for lifecycle checks (migrate once, prove the
// adapter is ready); each plugin's own typed store (board log/pgadapter,
// superagent_* tables) is built on top.
type DomainStore interface {
	// Domain names the storage owner ("board", "superagent").
	Domain() string
	// Migrate applies the domain's own schema. It is idempotent.
	Migrate(ctx context.Context) error
	// Ready proves the adapter can serve reads and writes.
	Ready(ctx context.Context) error
}

// ValidateDependency checks one dependency row and normalizes its state.
func ValidateDependency(dependency Dependency) error {
	if dependency.PluginID == "" {
		return errors.New("pluginstore: dependency plugin id is required")
	}
	if dependency.State == "" {
		dependency.State = DependencyPending
	}
	switch dependency.State {
	case DependencyReady, DependencyPending, DependencyFailed, DependencyStopped:
	default:
		return errors.New("pluginstore: dependency state " + dependency.State + " is not in the assembly vocabulary")
	}
	return nil
}
