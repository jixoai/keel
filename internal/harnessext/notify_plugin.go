package harnessext

// Notification plugin (`keel.notify`, change keel-harness-v2, W2.5).
//
// The harness owns the model-facing `notify` aggregate tool; this plugin is its
// action backend. The durable notification, its consumers, the ack facts and
// the one fallback push live in internal/notify; this file only parses the
// model's arguments, binds them to the ambient run (tenant + session) and
// renders the result. The plugin's Start/Stop own the sweep loop, so disabling
// keel.notify also disables the fallback push timer.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/notify"
)

// Notification plugin identity and action handler names.
const (
	NotifyPluginID      = "keel.notify"
	NotifyPluginVersion = "1.0.0"

	NotifyToolPush = "keel.notify.push"
	NotifyToolList = "keel.notify.list"
	NotifyToolAck  = "keel.notify.ack"

	notifyDefaultSweep = time.Second
	notifyMinWindowMs  = int64(1_000)
	notifyMaxWindowMs  = int64(24 * 60 * 60 * 1_000)
)

// NotifyOptions wires the plugin.
type NotifyOptions struct {
	Service       *notify.Service
	SweepInterval time.Duration
	Logger        *slog.Logger
}

// NewNotifyPlugin builds the builtin notification plugin.
func NewNotifyPlugin(opts NotifyOptions) *Plugin {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = notifyDefaultSweep
	}
	handler := &notifyPlugin{opts: opts}
	var stop func()
	return &Plugin{
		ID:      NotifyPluginID,
		Version: NotifyPluginVersion,
		Tools: []ToolSpec{
			{
				Name:        NotifyToolPush,
				Description: "Create a durable notification for the declared consumers.",
				Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
				Internal:    true,
				Execute:     handler.push,
			},
			{
				Name:        NotifyToolList,
				Description: "List this session's notifications.",
				Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
				Internal:    true,
				Execute:     handler.list,
			},
			{
				Name:        NotifyToolAck,
				Description: "Acknowledge one consumer of a notification.",
				Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
				Internal:    true,
				Execute:     handler.ack,
			},
		},
		Start: func(_ context.Context, _ *PluginEnv) error {
			if handler.opts.Service == nil {
				return nil
			}
			if stop != nil {
				stop()
			}
			runCtx, cancel := context.WithCancel(context.Background())
			stop = cancel
			go handler.opts.Service.Run(runCtx, handler.opts.SweepInterval)
			return nil
		},
		Stop: func(context.Context) error {
			if stop != nil {
				stop()
				stop = nil
			}
			return nil
		},
	}
}

type notifyPlugin struct {
	opts NotifyOptions
}

type notifyPushArgs struct {
	Text      string                `json:"text"`
	Title     string                `json:"title"`
	Consumers []notify.ConsumerSpec `json:"consumers"`
	WindowMs  int64                 `json:"windowMs"`
	RequestID string                `json:"requestId"`
}

func (p *notifyPlugin) push(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := RunScopeFromContext(ctx)
	if !ok || scope.TenantID == uuid.Nil || scope.SessionID == uuid.Nil {
		return notifyFailure("run_scope_missing", "notify actions need the ambient run scope", nil), nil
	}
	if p.opts.Service == nil {
		return notifyFailure("not_wired", "no notification service is wired in this deployment", nil), nil
	}
	var args notifyPushArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return notifyFailure("invalid_arguments", "arguments are not valid JSON: "+err.Error(), nil), nil
	}
	if strings.TrimSpace(args.Text) == "" {
		return notifyFailure("invalid_arguments", "text is required", map[string]any{"field": "text"}), nil
	}
	if args.WindowMs != 0 && (args.WindowMs < notifyMinWindowMs || args.WindowMs > notifyMaxWindowMs) {
		return notifyFailure("invalid_arguments",
			fmt.Sprintf("windowMs must be between %d and %d", notifyMinWindowMs, notifyMaxWindowMs),
			map[string]any{"field": "windowMs"}), nil
	}
	requestID := strings.TrimSpace(args.RequestID)
	if requestID == "" {
		requestID = notifyRequestID(scope, args)
	}
	view, err := p.opts.Service.Push(ctx, notify.PushRequest{
		TenantID:  scope.TenantID,
		SessionID: scope.SessionID,
		RequestID: requestID,
		Title:     args.Title,
		Text:      args.Text,
		Consumers: args.Consumers,
		Window:    time.Duration(args.WindowMs) * time.Millisecond,
	})
	if err != nil {
		return notifyError(err), nil
	}
	return notifyResult(view), nil
}

func (p *notifyPlugin) list(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := RunScopeFromContext(ctx)
	if !ok || scope.TenantID == uuid.Nil {
		return notifyFailure("run_scope_missing", "notify actions need the ambient run scope", nil), nil
	}
	if p.opts.Service == nil {
		return notifyFailure("not_wired", "no notification service is wired in this deployment", nil), nil
	}
	var args struct {
		SessionID string `json:"sessionId"`
		Limit     int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return notifyFailure("invalid_arguments", "arguments are not valid JSON: "+err.Error(), nil), nil
	}
	sessionID := scope.SessionID
	if strings.TrimSpace(args.SessionID) != "" {
		parsed, err := uuid.Parse(args.SessionID)
		if err != nil {
			return notifyFailure("invalid_arguments", "sessionId must be a UUID", map[string]any{"field": "sessionId"}), nil
		}
		sessionID = parsed
	}
	views, err := p.opts.Service.List(ctx, scope.TenantID, &sessionID, args.Limit)
	if err != nil {
		return notifyError(err), nil
	}
	document := map[string]any{"count": len(views), "notifications": views}
	return notifyDocument(document, false), nil
}

func (p *notifyPlugin) ack(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := RunScopeFromContext(ctx)
	if !ok || scope.TenantID == uuid.Nil {
		return notifyFailure("run_scope_missing", "notify actions need the ambient run scope", nil), nil
	}
	if p.opts.Service == nil {
		return notifyFailure("not_wired", "no notification service is wired in this deployment", nil), nil
	}
	var args struct {
		NotificationID string `json:"notificationId"`
		ConsumerID     string `json:"consumerId"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return notifyFailure("invalid_arguments", "arguments are not valid JSON: "+err.Error(), nil), nil
	}
	notificationID, err := uuid.Parse(strings.TrimSpace(args.NotificationID))
	if err != nil {
		return notifyFailure("invalid_arguments", "notificationId must be a UUID", map[string]any{"field": "notificationId"}), nil
	}
	view, duplicate, err := p.opts.Service.Ack(ctx, scope.TenantID, notificationID, args.ConsumerID,
		"agent:"+scope.RunID, notify.ChannelTool)
	if err != nil {
		return notifyError(err), nil
	}
	document := map[string]any{"notification": view, "duplicate": duplicate}
	return notifyDocument(document, false), nil
}

// notifyRequestID makes a retried push idempotent without collapsing two
// deliberate pushes of the same text in one run: the run id scopes the retry,
// the content distinguishes separate notices.
func notifyRequestID(scope RunScope, args notifyPushArgs) string {
	ids := make([]string, 0, len(args.Consumers))
	for _, consumer := range args.Consumers {
		ids = append(ids, consumer.ID+":"+consumer.Kind)
	}
	sum := sha256.Sum256([]byte(scope.RunID + "\x00" + args.Title + "\x00" + args.Text + "\x00" + strings.Join(ids, "\x00")))
	return "run:" + scope.RunID + ":" + hex.EncodeToString(sum[:12])
}

func notifyResult(view notify.View) agentcore.AgentToolResult {
	return notifyDocument(map[string]any{"notification": view}, false)
}

func notifyDocument(document map[string]any, isError bool) agentcore.AgentToolResult {
	raw, err := json.Marshal(document)
	if err != nil {
		raw = []byte(`{"status":"failed","code":"backend_error","message":"the result document could not be encoded"}`)
	}
	result := agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: document,
	}
	if isError {
		failure := true
		result.IsError = &failure
	}
	return result
}

func notifyFailure(code, message string, extra map[string]any) agentcore.AgentToolResult {
	document := map[string]any{
		"status":  "failed",
		"code":    code,
		"message": strings.TrimSpace(message),
	}
	for key, value := range extra {
		document[key] = value
	}
	return notifyDocument(document, true)
}

func notifyError(err error) agentcore.AgentToolResult {
	switch {
	case errors.Is(err, notify.ErrInvalid):
		return notifyFailure("invalid_arguments", err.Error(), nil)
	case errors.Is(err, notify.ErrNotFound):
		return notifyFailure("not_found", err.Error(), nil)
	case errors.Is(err, notify.ErrTokenInvalid):
		return notifyFailure("token_invalid", err.Error(), nil)
	case errors.Is(err, notify.ErrTokenExpired):
		return notifyFailure("token_expired", err.Error(), nil)
	case errors.Is(err, notify.ErrTokenUsed):
		return notifyFailure("token_used", err.Error(), nil)
	case errors.Is(err, notify.ErrConsumed):
		return notifyFailure("consumed", err.Error(), nil)
	case errors.Is(err, notify.ErrNotWired):
		return notifyFailure("not_wired", err.Error(), nil)
	default:
		return notifyFailure("backend_error", err.Error(), nil)
	}
}

// NotifyBackend adapts the plugin's internal action handlers to the
// harness-owned `notify` aggregate tool.
type NotifyBackend struct {
	Manager func() *Manager
}

var notifyBackendActions = map[string]string{
	"push": NotifyToolPush,
	"list": NotifyToolList,
	"ack":  NotifyToolAck,
}

// Invoke runs one notify action through the plugin manager. A disabled plugin
// returns ErrPluginState/ErrPluginNotFound, which the tool maps to
// plugin_disabled.
func (b NotifyBackend) Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	tool, ok := notifyBackendActions[action]
	if !ok {
		return agentcore.AgentToolResult{}, fmt.Errorf("harnessext: unknown notify action %q", action)
	}
	manager := (*Manager)(nil)
	if b.Manager != nil {
		manager = b.Manager()
	}
	if manager == nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("harnessext: %w: the plugin manager is not wired", ErrPluginState)
	}
	return manager.InvokeToolResult(ctx, NotifyPluginID, tool, args)
}
