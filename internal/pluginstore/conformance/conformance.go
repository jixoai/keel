// Package conformance is the shared test suite every pluginstore adapter must
// pass (change keel-plugin-split: "domain store interface + PG adapter +
// conformance"). It is a normal package (not a _test.go file) so both the
// in-memory adapter and the PostgreSQL adapter run exactly these assertions.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/pluginstore"
)

// Factory builds one isolated adapter. The adapter must be usable immediately:
// Migrate (when the adapter has a schema) is the factory's job.
type Factory func(t *testing.T) pluginstore.Store

// Run executes the full contract suite.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	t.Run("install inventory", func(t *testing.T) { runInstall(t, newStore(t)) })
	t.Run("opaque state", func(t *testing.T) { runState(t, newStore(t)) })
	t.Run("opaque config", func(t *testing.T) { runConfig(t, newStore(t)) })
	t.Run("dependency status", func(t *testing.T) { runDependency(t, newStore(t)) })
	t.Run("tenant isolation", func(t *testing.T) { runTenantIsolation(t, newStore(t)) })
}

func runInstall(t *testing.T, store pluginstore.Store) {
	ctx := context.Background()
	tenant := uuid.New()
	install := pluginstore.Install{
		TenantID: tenant, PluginID: "keel.board", Version: "1.0.0", Kind: "in-process",
		State: pluginstore.StateInstalled, Manifest: json.RawMessage(`{"id":"keel.board"}`),
	}
	created, err := store.PutInstall(ctx, install)
	if err != nil {
		t.Fatalf("put install: %v", err)
	}
	if created.PluginID != install.PluginID || created.State != pluginstore.StateInstalled {
		t.Fatalf("created = %+v", created)
	}
	if created.InstalledAt.IsZero() {
		t.Fatal("installed_at is zero")
	}
	// A state change preserves the installation age.
	enabled := created
	enabled.State = pluginstore.StateEnabled
	updated, err := store.PutInstall(ctx, enabled)
	if err != nil {
		t.Fatalf("update install: %v", err)
	}
	if !updated.InstalledAt.Equal(created.InstalledAt) {
		t.Fatalf("installed_at changed: %v -> %v", created.InstalledAt, updated.InstalledAt)
	}
	if updated.State != pluginstore.StateEnabled {
		t.Fatalf("state = %s", updated.State)
	}
	got, err := store.GetInstall(ctx, tenant, install.PluginID)
	if err != nil {
		t.Fatalf("get install: %v", err)
	}
	if got.State != pluginstore.StateEnabled {
		t.Fatalf("got state = %s", got.State)
	}
	list, err := store.ListInstalls(ctx, tenant)
	if err != nil {
		t.Fatalf("list installs: %v", err)
	}
	if len(list) != 1 || list[0].PluginID != install.PluginID {
		t.Fatalf("list = %+v", list)
	}
	deleted, err := store.DeleteInstall(ctx, tenant, install.PluginID)
	if err != nil || !deleted {
		t.Fatalf("delete install: deleted=%v err=%v", deleted, err)
	}
	if _, err := store.GetInstall(ctx, tenant, install.PluginID); !errors.Is(err, pluginstore.ErrNotFound) {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
	if _, err := store.PutInstall(ctx, pluginstore.Install{PluginID: "x"}); err == nil {
		t.Fatal("install without a tenant was accepted")
	}
}

func runState(t *testing.T, store pluginstore.Store) {
	ctx := context.Background()
	tenant := uuid.New()
	if _, ok, err := store.GetState(ctx, tenant, "keel.board", "missing"); err != nil || ok {
		t.Fatalf("get missing state: ok=%v err=%v", ok, err)
	}
	if err := store.PutState(ctx, tenant, "keel.board", "cursor", "42"); err != nil {
		t.Fatalf("put state: %v", err)
	}
	if err := store.PutState(ctx, tenant, "keel.board", "alpha", "one"); err != nil {
		t.Fatalf("put state: %v", err)
	}
	value, ok, err := store.GetState(ctx, tenant, "keel.board", "cursor")
	if err != nil || !ok || value != "42" {
		t.Fatalf("get state = %q ok=%v err=%v", value, ok, err)
	}
	entries, err := store.ListState(ctx, tenant, "keel.board")
	if err != nil {
		t.Fatalf("list state: %v", err)
	}
	if len(entries) != 2 || entries[0].Key != "alpha" || entries[1].Key != "cursor" {
		t.Fatalf("state entries = %+v", entries)
	}
	deleted, err := store.DeleteState(ctx, tenant, "keel.board", "cursor")
	if err != nil || !deleted {
		t.Fatalf("delete state: deleted=%v err=%v", deleted, err)
	}
	if _, ok, _ := store.GetState(ctx, tenant, "keel.board", "cursor"); ok {
		t.Fatal("state survived delete")
	}
}

func runConfig(t *testing.T, store pluginstore.Store) {
	ctx := context.Background()
	tenant := uuid.New()
	if _, ok, err := store.GetConfig(ctx, tenant, "keel.superagent"); err != nil || ok {
		t.Fatalf("get missing config: ok=%v err=%v", ok, err)
	}
	document := json.RawMessage(`{"mirror":true,"channel":"ops"}`)
	config, err := store.PutConfig(ctx, tenant, "keel.superagent", document)
	if err != nil {
		t.Fatalf("put config: %v", err)
	}
	if !sameJSON(config.Document, document) {
		t.Fatalf("document = %s", config.Document)
	}
	got, ok, err := store.GetConfig(ctx, tenant, "keel.superagent")
	if err != nil || !ok {
		t.Fatalf("get config: ok=%v err=%v", ok, err)
	}
	if !sameJSON(got.Document, document) {
		t.Fatalf("got document = %s", got.Document)
	}
}

func sameJSON(left, right []byte) bool {
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return false
	}
	leftCanonical, _ := json.Marshal(a)
	rightCanonical, _ := json.Marshal(b)
	return string(leftCanonical) == string(rightCanonical)
}

func runDependency(t *testing.T, store pluginstore.Store) {
	ctx := context.Background()
	tenant := uuid.New()
	dependency := pluginstore.Dependency{
		PluginID: "keel.superagent", Requires: []string{"board"},
		State: pluginstore.DependencyPending, Reason: "waiting for: board",
	}
	if _, err := store.PutDependency(ctx, tenant, dependency); err != nil {
		t.Fatalf("put dependency: %v", err)
	}
	list, err := store.ListDependencies(ctx, tenant)
	if err != nil {
		t.Fatalf("list dependencies: %v", err)
	}
	if len(list) != 1 || list[0].PluginID != "keel.superagent" || len(list[0].Requires) != 1 {
		t.Fatalf("dependencies = %+v", list)
	}
	dependency.State = pluginstore.DependencyReady
	dependency.Reason = ""
	if _, err := store.PutDependency(ctx, tenant, dependency); err != nil {
		t.Fatalf("update dependency: %v", err)
	}
	list, err = store.ListDependencies(ctx, tenant)
	if err != nil || len(list) != 1 || list[0].State != pluginstore.DependencyReady {
		t.Fatalf("updated dependencies = %+v err=%v", list, err)
	}
	if _, err := store.PutDependency(ctx, tenant, pluginstore.Dependency{PluginID: "x", State: "bogus"}); err == nil {
		t.Fatal("invalid dependency state was accepted")
	}
}

func runTenantIsolation(t *testing.T, store pluginstore.Store) {
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	if err := store.PutState(ctx, tenantA, "keel.board", "cursor", "A"); err != nil {
		t.Fatalf("put A: %v", err)
	}
	if _, ok, err := store.GetState(ctx, tenantB, "keel.board", "cursor"); err != nil || ok {
		t.Fatalf("tenant B read A's state: ok=%v err=%v", ok, err)
	}
	if _, err := store.PutInstall(ctx, pluginstore.Install{
		TenantID: tenantA, PluginID: "keel.board", Version: "1.0.0", Kind: "in-process",
	}); err != nil {
		t.Fatalf("put install A: %v", err)
	}
	if _, err := store.GetInstall(ctx, tenantB, "keel.board"); !errors.Is(err, pluginstore.ErrNotFound) {
		t.Fatalf("tenant B read A's install: err=%v", err)
	}
	if list, err := store.ListInstalls(ctx, tenantB); err != nil || len(list) != 0 {
		t.Fatalf("tenant B installs = %+v err=%v", list, err)
	}
	if _, err := store.PutConfig(ctx, tenantA, "keel.superagent", json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatalf("put config A: %v", err)
	}
	if _, ok, err := store.GetConfig(ctx, tenantB, "keel.superagent"); err != nil || ok {
		t.Fatalf("tenant B read A's config: ok=%v err=%v", ok, err)
	}
	if _, err := store.PutDependency(ctx, tenantA, pluginstore.Dependency{PluginID: "keel.superagent"}); err != nil {
		t.Fatalf("put dependency A: %v", err)
	}
	if list, err := store.ListDependencies(ctx, tenantB); err != nil || len(list) != 0 {
		t.Fatalf("tenant B dependencies = %+v err=%v", list, err)
	}
}

// RunDomain executes the independent-domain contract: a non-empty owner name,
// an idempotent Migrate and a Ready that reaches the adapter. Only board and
// superagent are expected to call it.
func RunDomain(t *testing.T, newStore func(t *testing.T) pluginstore.DomainStore, want string) {
	t.Helper()
	store := newStore(t)
	if store.Domain() != want {
		t.Fatalf("domain = %q, want %q", store.Domain(), want)
	}
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("repeat migrate: %v", err)
	}
	if err := store.Ready(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
}
