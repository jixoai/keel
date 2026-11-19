package toolsurface

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
)

// NotifyBackend is the keel.notify plugin's action backend. The aggregate tool
// is owned by the harness and always present; the plugin only supplies the
// implementation, so disabling keel.notify yields plugin_disabled instead of
// removing the tool name.
type NotifyBackend interface {
	Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error)
}

var notifyActions = []string{ActionPush, ActionList, ActionAck}

const notifyToolDescription = "Create and consume durable notifications. " +
	"Actions: push {text, title?, consumers:[{id, kind, label?, online?, ack?, serviceWorker?}], windowMs?, requestId?} " +
	"— kinds are client (live, ack-capable), native (system-notification target), url (single-use consumption link) and " +
	"img (delivery pixel); the push answer carries the generated urls once. list {sessionId?, limit?}; " +
	"ack {notificationId, consumerId}. Each consumer has its own state and any ack consumes the notification. " +
	"The ack window decides the one fallback push: a live ack-capable consumer that acks inside the window suppresses " +
	"it; otherwise exactly one native/in-page push is sent (a web target without service-worker capability degrades to " +
	"an in-page notice and registers the gap; a deployment without a native pusher registers that gap too). " +
	"The keel.notify plugin supplies the backend."

func notifySchema() json.RawMessage {
	return schema(`, "text":{"type":"string"}, "title":{"type":"string"}, ` +
		`"consumers":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"kind":{"type":"string"},` +
		`"label":{"type":"string"},"online":{"type":"boolean"},"ack":{"type":"boolean"},"serviceWorker":{"type":"boolean"}},` +
		`"required":["id","kind"]}}, "windowMs":{"type":"integer","minimum":0}, "requestId":{"type":"string"}, ` +
		`"notificationId":{"type":"string"}, "consumerId":{"type":"string"}, "sessionId":{"type":"string"}, ` +
		`"limit":{"type":"integer","minimum":0}`)
}

func newNotifyTool(backend NotifyBackend) *actionTool {
	return &actionTool{
		name:        Notify,
		description: notifyToolDescription,
		schema:      notifySchema(),
		actions:     notifyActions,
		run: func(ctx context.Context, action string, raw json.RawMessage) (agentcore.AgentToolResult, error) {
			if backend == nil {
				return notWired(Notify, "no notification backend is wired in this deployment")
			}
			result, err := backend.Invoke(ctx, action, raw)
			if err != nil {
				switch {
				case errors.Is(err, agentext.ErrPluginState), errors.Is(err, agentext.ErrPluginNotFound):
					return ErrorResult(CodePluginDisabled, Notify,
						"keel.notify is not enabled; enable the plugin to use notification actions", map[string]any{"action": action})
				default:
					return ErrorResult(CodeBackendFailed, Notify, err.Error(), map[string]any{"action": action})
				}
			}
			return result, nil
		},
	}
}
