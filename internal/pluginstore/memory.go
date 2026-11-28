package pluginstore

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Memory is the in-process Store used by tests and a host without durable plugin
// persistence. It is explicitly not a persistence decision: every record is lost
// at process exit. It implements the same contract as the PostgreSQL adapter and
// passes the same conformance suite.
type Memory struct {
	mu       sync.Mutex
	now      func() time.Time
	installs map[string]Install
	states   map[string]map[string]StateEntry
	configs  map[string]Config
	deps     map[string]Dependency
}

// NewMemory builds an empty store. A nil clock uses time.Now (UTC).
func NewMemory(now func() time.Time) *Memory {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Memory{
		now:      now,
		installs: make(map[string]Install),
		states:   make(map[string]map[string]StateEntry),
		configs:  make(map[string]Config),
		deps:     make(map[string]Dependency),
	}
}

var _ Store = (*Memory)(nil)

func installKey(tenantID uuid.UUID, pluginID string) string {
	return tenantID.String() + "\x00" + pluginID
}

func stateKey(tenantID uuid.UUID, pluginID string) string {
	return tenantID.String() + "\x00" + pluginID
}

func (m *Memory) PutInstall(_ context.Context, install Install) (Install, error) {
	if err := ValidateInstall(install); err != nil {
		return Install{}, err
	}
	if install.State == "" {
		install.State = StateInstalled
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	key := installKey(install.TenantID, install.PluginID)
	if existing, ok := m.installs[key]; ok {
		if install.InstalledAt.IsZero() {
			install.InstalledAt = existing.InstalledAt
		}
	}
	if install.InstalledAt.IsZero() {
		install.InstalledAt = now
	}
	if install.UpdatedAt.IsZero() {
		install.UpdatedAt = now
	}
	install.Diagnostics = append([]string(nil), install.Diagnostics...)
	install.Manifest = append(json.RawMessage(nil), install.Manifest...)
	m.installs[key] = install
	return cloneInstall(install), nil
}

func (m *Memory) GetInstall(_ context.Context, tenantID uuid.UUID, pluginID string) (Install, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	install, ok := m.installs[installKey(tenantID, pluginID)]
	if !ok {
		return Install{}, ErrNotFound
	}
	return cloneInstall(install), nil
}

func (m *Memory) ListInstalls(_ context.Context, tenantID uuid.UUID) ([]Install, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := tenantID.String() + "\x00"
	out := make([]Install, 0)
	for key, install := range m.installs {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			out = append(out, cloneInstall(install))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PluginID < out[j].PluginID })
	return out, nil
}

func (m *Memory) DeleteInstall(_ context.Context, tenantID uuid.UUID, pluginID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := installKey(tenantID, pluginID)
	if _, ok := m.installs[key]; !ok {
		return false, nil
	}
	delete(m.installs, key)
	return true, nil
}

func (m *Memory) GetState(_ context.Context, tenantID uuid.UUID, pluginID, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, ok := m.states[stateKey(tenantID, pluginID)]
	if !ok {
		return "", false, nil
	}
	entry, ok := entries[key]
	if !ok {
		return "", false, nil
	}
	return entry.Value, true, nil
}

func (m *Memory) PutState(_ context.Context, tenantID uuid.UUID, pluginID, key, value string) error {
	if pluginID == "" || key == "" {
		return errStateKey
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	scoped := stateKey(tenantID, pluginID)
	entries, ok := m.states[scoped]
	if !ok {
		entries = make(map[string]StateEntry)
		m.states[scoped] = entries
	}
	entries[key] = StateEntry{PluginID: pluginID, Key: key, Value: value, UpdatedAt: m.now()}
	return nil
}

func (m *Memory) DeleteState(_ context.Context, tenantID uuid.UUID, pluginID, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scoped := stateKey(tenantID, pluginID)
	entries, ok := m.states[scoped]
	if !ok {
		return false, nil
	}
	if _, ok := entries[key]; !ok {
		return false, nil
	}
	delete(entries, key)
	return true, nil
}

func (m *Memory) ListState(_ context.Context, tenantID uuid.UUID, pluginID string) ([]StateEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := m.states[stateKey(tenantID, pluginID)]
	out := make([]StateEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *Memory) GetConfig(_ context.Context, tenantID uuid.UUID, pluginID string) (Config, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	config, ok := m.configs[stateKey(tenantID, pluginID)]
	if !ok {
		return Config{}, false, nil
	}
	config.Document = append(json.RawMessage(nil), config.Document...)
	return config, true, nil
}

func (m *Memory) PutConfig(_ context.Context, tenantID uuid.UUID, pluginID string, document json.RawMessage) (Config, error) {
	if pluginID == "" {
		return Config{}, errStateKey
	}
	if len(document) == 0 {
		document = json.RawMessage(`{}`)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	config := Config{
		PluginID: pluginID, Document: append(json.RawMessage(nil), document...), UpdatedAt: m.now(),
	}
	m.configs[stateKey(tenantID, pluginID)] = config
	return config, nil
}

func (m *Memory) PutDependency(_ context.Context, tenantID uuid.UUID, dependency Dependency) (Dependency, error) {
	if err := ValidateDependency(dependency); err != nil {
		return Dependency{}, err
	}
	if dependency.State == "" {
		dependency.State = DependencyPending
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if dependency.UpdatedAt.IsZero() {
		dependency.UpdatedAt = m.now()
	}
	dependency.Requires = append([]string(nil), dependency.Requires...)
	m.deps[installKey(tenantID, dependency.PluginID)] = dependency
	return dependency, nil
}

func (m *Memory) ListDependencies(_ context.Context, tenantID uuid.UUID) ([]Dependency, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := tenantID.String() + "\x00"
	out := make([]Dependency, 0)
	for key, dependency := range m.deps {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			dependency.Requires = append([]string(nil), dependency.Requires...)
			out = append(out, dependency)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PluginID < out[j].PluginID })
	return out, nil
}

func cloneInstall(install Install) Install {
	install.Diagnostics = append([]string(nil), install.Diagnostics...)
	install.Manifest = append(json.RawMessage(nil), install.Manifest...)
	return install
}

var errStateKey = errors.New("pluginstore: plugin id and state key are required")
