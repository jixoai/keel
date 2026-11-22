//go:build !windows

package oar

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/externalgw"
	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
)

// TestExternalGatewayProjectsOARRecords is the bridge proof: the OAR sidecar is
// configured as a normal host.ExternalConfig backend, the existing externalgw
// opens/sends through the gateway protocol, and each oar.record becomes an
// external.event with the record payload intact. No externalgw code changed.
func TestExternalGatewayProjectsOARRecords(t *testing.T) {
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	tenant, sessionID := uuid.New(), uuid.New()
	mem.CreateSession(tenant, sessionID)
	scope := kernel.Scope{TenantID: tenant, SessionID: sessionID}

	doc := host.ExternalConfig{
		Backends: map[string]host.ExternalProviderConfig{
			"oar": {
				Command: sidecarBin,
				Args:    []string{"sidecar", "-runtime=fake", "-usage"},
			},
		},
		Quota: host.ExternalQuotaConfig{Unmetered: host.ExternalUnmeteredAllow},
	}
	service, err := externalgw.NewAgentService(doc, externalgw.Options{
		Committer:      k,
		Sessions:       k,
		LookupEnv:      os.LookupEnv,
		Environ:        os.Environ(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Stderr:         io.Discard,
		Restart:        gateway.RestartPolicy{InitialDelay: 20 * time.Millisecond, MaxDelay: 200 * time.Millisecond, MaxRestarts: 3},
		ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewAgentService: %v", err)
	}
	if err := service.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := service.Stop(ctx); err != nil {
			t.Logf("Stop: %v", err)
		}
	})
	readyCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	err = service.WaitReady(readyCtx)
	cancel()
	if err != nil {
		t.Fatalf("backend did not become ready: %v (%+v)", err, service.Backends())
	}

	workdir := t.TempDir()
	config, err := json.Marshal(map[string]any{
		"attribution": map[string]string{"sessionId": sessionID.String(), "taskId": "task-card-ext"},
		"workdir":     workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.Open(context.Background(), scope, "oar", gateway.SessionOpenRequest{
		Reference: "oar-ext-1",
		Config:    config,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if view.StateOwner != kernel.StateOwnerExternal || view.Status != kernel.ExternalStatusActive || view.Metering != kernel.MeteringExternal {
		t.Fatalf("session view = %+v", view)
	}
	if _, err := service.Send(context.Background(), scope, json.RawMessage(`{"text":"external bridge probe"}`), "req-oar-1"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	payload := waitForOAREvent(t, k, scope)
	if payload["backend"] != "oar" || payload["stateOwner"] != kernel.StateOwnerExternal || payload["direction"] != "receive" {
		t.Fatalf("external.event provenance = %+v", payload)
	}
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("external.event data = %T (%+v)", payload["data"], payload["data"])
	}
	if data["schema"] != "keel.oar.record/1" || data["agentPath"] == "" {
		t.Fatalf("OAR record payload = %+v", data)
	}
	attribution, _ := data["attribution"].(map[string]any)
	if attribution["sessionId"] != sessionID.String() || attribution["taskId"] != "task-card-ext" {
		t.Fatalf("OAR attribution = %+v", attribution)
	}

	marks, err := k.ExternalSession(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if marks.StateOwner != kernel.StateOwnerExternal || marks.Backend != "oar" {
		t.Fatalf("external session marks = %+v", marks.ExternalMarks)
	}
}

func waitForOAREvent(t *testing.T, k *kernel.Kernel, scope kernel.Scope) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, err := k.LoadEvents(context.Background(), scope, 0, 500)
		if err == nil {
			for _, event := range events {
				if event.Kind != kernel.KindExternalEvent {
					continue
				}
				var payload map[string]any
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					continue
				}
				if payload["type"] == EventTypeRecord {
					return payload
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no oar.record external.event within 10s")
	return nil
}
