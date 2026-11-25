package servicehost

import (
	"io"
	"log/slog"
	"testing"

	"github.com/jixoai/keel/internal/hooks"
	"github.com/jixoai/keel/internal/host"
)

func TestNewHookRegistryLoadsTenantCommands(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tenant := host.TenantConfig{Hooks: host.HooksConfig{Commands: []host.HookCommand{{
		Event:     hooks.EventToolCall,
		Command:   []string{"true"},
		TimeoutMS: 100,
	}}}}
	registry, err := newHookRegistry(tenant, logger)
	if err != nil {
		t.Fatal(err)
	}
	if !registry.HasHandlers(hooks.EventToolCall) {
		t.Fatal("tenant command hook was not registered")
	}
}

func TestNewHookRegistryRejectsUnknownEvent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tenant := host.TenantConfig{Hooks: host.HooksConfig{Commands: []host.HookCommand{{
		Event:   "not_a_pi_event",
		Command: []string{"true"},
	}}}}
	if _, err := newHookRegistry(tenant, logger); err == nil {
		t.Fatal("unknown hook event was accepted")
	}
}
