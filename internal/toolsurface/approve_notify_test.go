package toolsurface_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/toolsurface"
)

type fakeApproveBackend struct {
	action string
	calls  int
	result agentcore.AgentToolResult
	err    error
}

func (f *fakeApproveBackend) Invoke(_ context.Context, action string, _ json.RawMessage) (agentcore.AgentToolResult, error) {
	f.action = action
	f.calls++
	return f.result, f.err
}

type fakeNotifyBackend struct {
	action string
	calls  int
	result agentcore.AgentToolResult
	err    error
}

func (f *fakeNotifyBackend) Invoke(_ context.Context, action string, _ json.RawMessage) (agentcore.AgentToolResult, error) {
	f.action = action
	f.calls++
	return f.result, f.err
}

func successResult() agentcore.AgentToolResult {
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(`{"ok":true}`)},
		StructuredContent: map[string]any{"ok": true},
	}
}

func TestApproveToolRoutesToItsBackend(t *testing.T) {
	backend := &fakeApproveBackend{result: successResult()}
	registry := builtinRegistry(t, toolsurface.Backends{Approve: backend})
	result := runAction(t, registry[toolsurface.Approve], `{"action":"confirm","prompt":"x"}`)
	if backend.calls != 1 || backend.action != "confirm" {
		t.Fatalf("backend calls = %d action = %q", backend.calls, backend.action)
	}
	if result.StructuredContent.(map[string]any)["ok"] != true {
		t.Fatalf("result = %#v", result)
	}
}

func TestNotifyToolRoutesToItsBackend(t *testing.T) {
	backend := &fakeNotifyBackend{result: successResult()}
	registry := builtinRegistry(t, toolsurface.Backends{Notify: backend})
	for _, action := range []string{"push", "list", "ack"} {
		runAction(t, registry[toolsurface.Notify], `{"action":"`+action+`"}`)
	}
	if backend.calls != 3 || backend.action != "ack" {
		t.Fatalf("backend calls = %d action = %q", backend.calls, backend.action)
	}
}

func TestApproveAndNotifyDisabledPluginsAreTyped(t *testing.T) {
	disabled := agentext.ErrPluginState
	registry := builtinRegistry(t, toolsurface.Backends{
		Approve: &fakeApproveBackend{err: disabled},
		Notify:  &fakeNotifyBackend{err: disabled},
	})
	for _, test := range []struct {
		tool string
		args string
	}{
		{toolsurface.Approve, `{"action":"confirm","prompt":"x"}`},
		{toolsurface.Notify, `{"action":"push","text":"x"}`},
	} {
		result := runAction(t, registry[test.tool], test.args)
		problem := errorDocument(t, result)
		if problem["code"] != toolsurface.CodePluginDisabled {
			t.Fatalf("%s code = %v, want plugin_disabled", test.tool, problem["code"])
		}
	}
}

func TestApproveAndNotifyWithoutBackendsAreNotWired(t *testing.T) {
	registry := builtinRegistry(t, toolsurface.Backends{})
	for _, test := range []struct {
		tool string
		args string
	}{
		{toolsurface.Approve, `{"action":"confirm","prompt":"x"}`},
		{toolsurface.Notify, `{"action":"push","text":"x"}`},
	} {
		result := runAction(t, registry[test.tool], test.args)
		problem := errorDocument(t, result)
		if problem["code"] != toolsurface.CodeNotWired {
			t.Fatalf("%s code = %v, want not_wired", test.tool, problem["code"])
		}
	}
}
