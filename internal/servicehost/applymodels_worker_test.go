package servicehost

// Regression for the settings hot-reload path: a models write must leave the
// worker claiming runs. The first version of ApplyModels passed the caller's
// context (the HTTP request) into the engine's activation, and the worker
// service registers a watchdog that stops claiming as soon as its activation
// context ends — so the PUT that changed the configuration also silently parked
// the worker until the process restarted. Only an end-to-end run could see it.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
)

// waitForRunSettled waits until the session's run row leaves pending/running,
// reporting whether it was ever claimed.
func waitForRunSettled(t *testing.T, mem *kernel.MemStore, tenant, session uuid.UUID, within time.Duration) kernel.RunPin {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		pin, ok, err := mem.ReadRunPin(context.Background(), kernel.Scope{TenantID: tenant, SessionID: session})
		if err == nil && ok {
			switch pin.Status {
			case kernel.RunStatusDone, kernel.RunStatusFailed, "cancelled":
				return pin
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	pin, _, _ := mem.ReadRunPin(context.Background(), kernel.Scope{TenantID: tenant, SessionID: session})
	return pin
}

// TestApplyModelsKeepsTheWorkerClaiming is the regression: after a settings
// write the next input must still be claimed and answered (the model call may
// fail — the document points at a fake endpoint — but a settled run proves the
// worker was alive).
func TestApplyModelsKeepsTheWorkerClaiming(t *testing.T) {
	ctx := context.Background()
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	tenant, session := uuid.New(), uuid.New()
	mem.CreateSession(tenant, session)
	rt, err := Boot(ctx, Config{
		Seams:     Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:    host.TenantConfig{Models: json.RawMessage(modelsDocument)},
		LLM:       testLLM(map[string]string{"TENANT_GW_KEY": "gw-key", "ANTHROPIC_API_KEY": "anthropic-key"}, "flat-key"),
		Options:   loadout.Options{Worker: true, RunConcurrency: 1},
		RuntimeID: "test-runtime",
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })

	submit := func(requestID string) {
		t.Helper()
		if _, err := k.Commit(ctx, kernel.CommitRequest{
			Scope:     kernel.Scope{TenantID: tenant, SessionID: session},
			Kind:      kernel.KindUserMessage,
			RequestID: requestID,
			Payload:   json.RawMessage(`{"text":"hello"}`),
		}); err != nil {
			t.Fatalf("commit input: %v", err)
		}
		mem.EnqueueRun(tenant, session)
	}

	// The deployment document works before the write.
	submit("before")
	first := waitForRunSettled(t, mem, tenant, session, 10*time.Second)
	if first.Status == kernel.RunStatusPending || first.Status == kernel.RunStatusRunning {
		t.Fatalf("the run before the settings write was never claimed: %+v", first)
	}

	// The settings write uses a request-scoped context that ends with the HTTP
	// response — exactly what the API does.
	reqCtx, cancel := context.WithCancel(ctx)
	if _, err := rt.ApplyModels(reqCtx, documentWithSelection(t, "tenant-gw", "deepseek-v4.1-flash", "low")); err != nil {
		t.Fatalf("apply models: %v", err)
	}
	cancel()

	submit("after")
	second := waitForRunSettled(t, mem, tenant, session, 15*time.Second)
	if second.Status == kernel.RunStatusPending || second.Status == kernel.RunStatusRunning {
		t.Fatalf("the worker stopped claiming after the settings write: %+v", second)
	}
}
