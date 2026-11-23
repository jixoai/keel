package agent

// The run-path capture policy tests (change keel-harness-v2, W3 capture policy
// override). They drive the real worker attempt hook with the in-memory kernel:
// no Postgres is needed because the policy is resolved before the settlement
// and the checkpoint hook is a seam this test observes directly.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/layerfs"
)

// captureProbeTool is the attempt's tool call: it records the capture mode the
// runner pinned on the tool-execution context. The fake provider calls "echo"
// for a prompt containing "echo".
type captureProbeTool struct {
	modes []layerfs.CaptureMode
}

func (t *captureProbeTool) Name() string            { return "echo" }
func (t *captureProbeTool) Description() string     { return "capture mode probe" }
func (t *captureProbeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *captureProbeTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

func (t *captureProbeTool) Execute(ctx context.Context, _ string, _ json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	mode, _ := layerfs.CaptureModeFromContext(ctx)
	t.modes = append(t.modes, mode)
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent("probed")},
	}, nil
}

type captureHarness struct {
	mem    *kernel.MemStore
	kernel *kernel.Kernel
	runner *Runner
	worker *Worker
	probe  *captureProbeTool
	scope  kernel.Scope
}

func newCaptureHarness(t *testing.T) *captureHarness {
	t.Helper()
	mem := kernel.NewMemStore()
	tenant, session := uuid.New(), uuid.New()
	mem.CreateSession(tenant, session)
	mem.EnqueueRun(tenant, session)
	kern := kernel.New(mem, kernel.Options{})
	probe := &captureProbeTool{}
	registry := agenttool.NewToolRegistry()
	if err := registry.Register(probe); err != nil {
		t.Fatalf("register probe: %v", err)
	}
	runner := NewRunner(kern, kern, FakeStream(), "fake", "fake", "", discardLog())
	runner.Tools = registry
	runner.ToolAccess = AllowAllToolAccess{}
	worker := NewWorker(kern, nil, runner, "capture-policy-test", 1, discardLog())
	worker.Committer = kern
	return &captureHarness{
		mem: mem, kernel: kern, runner: runner, worker: worker, probe: probe,
		scope: kernel.Scope{TenantID: tenant, SessionID: session},
	}
}

// runAttempt commits one input and executes exactly one worker attempt.
func (h *captureHarness) runAttempt(t *testing.T) {
	t.Helper()
	commitInput(t, h.kernel, h.scope, "req-"+uuid.NewString(), "echo capture", "", nil)
	lease, err := h.kernel.ClaimScope(context.Background(), h.scope, "capture-policy-test", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	h.worker.execute(context.Background(), sem, lease)
}

// TestCheckpointPolicyPinsAndFiresOnce proves the attempt boundary: the mode is
// resolved once, the tool sees the pinned value even after the source rotates
// mid-attempt, and the run-completion hook fires exactly once.
func TestCheckpointPolicyPinsAndFiresOnce(t *testing.T) {
	h := newCaptureHarness(t)
	current := layerfs.CaptureModeCheckpoint
	var hookCalls int
	var hookModes []layerfs.CaptureMode
	h.runner.CapturePolicy = func(context.Context, kernel.Scope) (RunCapture, error) {
		mode := current
		if mode == layerfs.CaptureModeCheckpoint {
			// Rotation lands after this attempt pinned its policy. The in-flight
			// attempt must keep checkpoint; the next attempt reads manual.
			current = layerfs.CaptureModeManual
			return RunCapture{
				Mode: mode,
				Checkpoint: func(ctx context.Context, _ kernel.Scope, callID string) error {
					if callID == "" {
						t.Error("checkpoint hook called without an idempotency key")
					}
					hookCalls++
					pinned, _ := layerfs.CaptureModeFromContext(ctx)
					hookModes = append(hookModes, pinned)
					return nil
				},
			}, nil
		}
		// Manual must still declare the explicit surface (the worker validates
		// that a trigger exists even though it never calls it at run close).
		return RunCapture{Mode: mode, Checkpoint: func(context.Context, kernel.Scope, string) error {
			hookCalls++
			return nil
		}}, nil
	}

	h.runAttempt(t)
	if hookCalls != 1 {
		t.Fatalf("checkpoint hook calls = %d, want exactly 1", hookCalls)
	}
	if len(hookModes) != 1 || hookModes[0] != layerfs.CaptureModeCheckpoint {
		t.Fatalf("checkpoint hook modes = %v, want the pinned checkpoint", hookModes)
	}
	if len(h.probe.modes) != 1 || h.probe.modes[0] != layerfs.CaptureModeCheckpoint {
		t.Fatalf("tool modes = %v, want the pinned checkpoint (the rotation must not reach this run)", h.probe.modes)
	}

	// The next attempt reads the rotated policy and does not checkpoint.
	h.runAttempt(t)
	if hookCalls != 1 {
		t.Fatalf("checkpoint hook calls after rotation = %d, want 1", hookCalls)
	}
	if len(h.probe.modes) != 2 || h.probe.modes[1] != layerfs.CaptureModeManual {
		t.Fatalf("second attempt modes = %v, want the rotated manual policy", h.probe.modes)
	}
}

// TestPerToolCallPolicyDoesNotCheckpointAtRunClose pins the default: no
// run-completion hook is invoked.
func TestPerToolCallPolicyDoesNotCheckpointAtRunClose(t *testing.T) {
	h := newCaptureHarness(t)
	hookCalls := 0
	h.runner.CapturePolicy = func(context.Context, kernel.Scope) (RunCapture, error) {
		return RunCapture{
			Mode: layerfs.CaptureModePerToolCall,
			Checkpoint: func(context.Context, kernel.Scope, string) error {
				hookCalls++
				return nil
			},
		}, nil
	}
	h.runAttempt(t)
	if hookCalls != 0 {
		t.Fatalf("checkpoint hook calls = %d, want 0 for per-tool-call", hookCalls)
	}
	if len(h.probe.modes) != 1 || h.probe.modes[0] != layerfs.CaptureModePerToolCall {
		t.Fatalf("tool modes = %v, want per-tool-call", h.probe.modes)
	}
}

// TestManualPolicyDoesNotCheckpointAtRunClose pins that manual has no automatic
// trigger at all; the explicit surface is the only capture path.
func TestManualPolicyDoesNotCheckpointAtRunClose(t *testing.T) {
	h := newCaptureHarness(t)
	hookCalls := 0
	h.runner.CapturePolicy = func(context.Context, kernel.Scope) (RunCapture, error) {
		return RunCapture{
			Mode: layerfs.CaptureModeManual,
			Checkpoint: func(context.Context, kernel.Scope, string) error {
				hookCalls++
				return nil
			},
		}, nil
	}
	h.runAttempt(t)
	if hookCalls != 0 {
		t.Fatalf("checkpoint hook calls = %d, want 0 for manual", hookCalls)
	}
	if len(h.probe.modes) != 1 || h.probe.modes[0] != layerfs.CaptureModeManual {
		t.Fatalf("tool modes = %v, want manual", h.probe.modes)
	}
}

// TestCapturePolicyWithoutTriggerFailsClosed pins the typed refusal: selecting
// checkpoint/manual without the matching trigger never silently swallows
// capture.
func TestCapturePolicyWithoutTriggerFailsClosed(t *testing.T) {
	h := newCaptureHarness(t)
	ctx := context.Background()

	for _, mode := range []layerfs.CaptureMode{layerfs.CaptureModeCheckpoint, layerfs.CaptureModeManual} {
		h.runner.CapturePolicy = func(context.Context, kernel.Scope) (RunCapture, error) {
			return RunCapture{Mode: mode}, nil
		}
		_, err := h.runner.ResolveCapturePolicy(ctx, h.scope)
		var triggerErr *layerfs.CaptureTriggerError
		if !errors.As(err, &triggerErr) {
			t.Fatalf("mode %s without a trigger: err = %v, want *layerfs.CaptureTriggerError", mode, err)
		}
		if triggerErr.Mode != mode || triggerErr.Code() != "capture_trigger_missing" {
			t.Fatalf("mode %s trigger error = %+v", mode, triggerErr)
		}
	}

	h.runner.CapturePolicy = func(context.Context, kernel.Scope) (RunCapture, error) {
		return RunCapture{Mode: layerfs.CaptureMode("sometimes")}, nil
	}
	_, err := h.runner.ResolveCapturePolicy(ctx, h.scope)
	var modeErr *layerfs.CaptureModeError
	if !errors.As(err, &modeErr) {
		t.Fatalf("unknown mode: err = %v, want *layerfs.CaptureModeError", err)
	}

	// A runner without the seam is the documented default, not a refusal.
	h.runner.CapturePolicy = nil
	capture, err := h.runner.ResolveCapturePolicy(ctx, h.scope)
	if err != nil {
		t.Fatalf("nil policy: %v", err)
	}
	if capture.Mode != layerfs.CaptureModePerToolCall {
		t.Fatalf("nil policy mode = %q, want per-tool-call", capture.Mode)
	}
}
