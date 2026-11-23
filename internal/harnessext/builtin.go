package harnessext

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/testkit"
)

// Built-in example plugin. It is a complete, non-trivial plugin: two tools, a
// hook subscription, a webhook trigger, two effects and a Start/Stop pair. It
// exists so the extension surface is verified end to end without a third-party
// plugin, and so an operator can POST /v1/triggers/keel.example.ping/fire on a
// fresh `keel serve` and watch a real turn start with real audit records.
//
// The webhook trigger accepts an unsigned manual fire. Signed deliveries need
// KEEL_EXAMPLE_WEBHOOK_SECRET (WebhookSecretEnv); without it the trigger is
// fail-closed for webhook calls, exactly like every other unconfigured webhook
// trigger.
const (
	ExamplePluginID      = "keel.example"
	ExamplePluginVersion = "1.0.0"
	// ExamplePingTriggerID is the webhook trigger the example plugin declares.
	ExamplePingTriggerID = "keel.example.ping"
	exampleCapability    = "cap:example.status"
)

// ExamplePluginOptions configures the example plugin.
type ExamplePluginOptions struct {
	// TenantID + Interval (both set) add an enabled interval trigger, which is
	// useful for a demo scheduler.
	TenantID string
	Interval time.Duration
	// BotID is the Bot the example triggers fire as. Every trigger must have a
	// Bot owner, so the plugin declares no trigger while this is empty: the
	// alternative is a plugin that fails every activation (or, worse, an
	// unowned session the runtime refuses). `keel serve --example-bot-id`
	// (KEEL_EXAMPLE_BOT_ID) supplies it after the operator creates the Bot.
	BotID string
	// Prompt overrides the webhook turn text.
	Prompt string
	// WebhookSecret configures the ping trigger's HMAC key directly. Tests use
	// it; production sets WebhookSecretEnv instead so no secret is compiled in.
	WebhookSecret string
	// WebhookSecretEnv names the environment variable holding the HMAC key.
	// Unset/empty leaves the trigger fail-closed for signed deliveries; the
	// manual fire route still works.
	WebhookSecretEnv string
}

// NewExamplePlugin builds the built-in example plugin.
func NewExamplePlugin(opts ExamplePluginOptions) *Plugin {
	audit := &exampleAudit{}
	var (
		mu      sync.Mutex
		ticker  *time.Ticker
		stop    chan struct{}
		started time.Time
		ticks   int64
	)
	prompt := opts.Prompt
	if prompt == "" {
		prompt = "Example plugin ping: confirm the extension surface is live."
	}
	// The example trigger is bound to the serve tenant when one is configured,
	// so its Bot is validated at registration and the trigger is visible in
	// that tenant's GET /v1/triggers. A zero tenant leaves it tenantless (the
	// fire resolves the handle inside the caller's tenant).
	triggerTenant := ""
	if parsed, err := uuid.Parse(opts.TenantID); err == nil && parsed != uuid.Nil {
		triggerTenant = parsed.String()
	}
	triggers := make([]TriggerSpec, 0, 2)
	if botID := strings.TrimSpace(opts.BotID); botID != "" {
		triggers = append(triggers, TriggerSpec{
			ID:               ExamplePingTriggerID,
			Kind:             TriggerWebhook,
			Prompt:           prompt,
			BotID:            botID,
			TenantID:         triggerTenant,
			Enabled:          true,
			Owner:            ExamplePluginID,
			Schedule:         "",
			WebhookSecret:    opts.WebhookSecret,
			WebhookSecretEnv: opts.WebhookSecretEnv,
		})
		if opts.Interval > 0 && opts.TenantID != "" {
			triggers = append(triggers, TriggerSpec{
				ID:       ExamplePluginID + ".heartbeat",
				Kind:     TriggerInterval,
				Schedule: opts.Interval.String(),
				Prompt:   "Heartbeat from the example plugin.",
				BotID:    botID,
				TenantID: opts.TenantID,
				Enabled:  true,
				Owner:    ExamplePluginID,
			})
		}
	}
	triggerIDs := make([]string, 0, len(triggers))
	for _, trigger := range triggers {
		triggerIDs = append(triggerIDs, trigger.ID)
	}

	return &Plugin{
		ID:       ExamplePluginID,
		Version:  ExamplePluginVersion,
		Provides: []string{exampleCapability},
		Hooks: []HookSpec{
			{Event: HookTriggerAfter, Handler: func(_ context.Context, payload HookPayload) error {
				audit.add("trigger.after", payload)
				return nil
			}},
			{Event: HookDecisionAnswered, Handler: func(_ context.Context, payload HookPayload) error {
				audit.add("decision.answered", payload)
				return nil
			}},
			{Event: HookSubagentFinished, Handler: func(_ context.Context, payload HookPayload) error {
				audit.add("subagent.finished", payload)
				return nil
			}},
		},
		Triggers: triggers,
		Tools: []ToolSpec{{
			Name:        "example_status",
			Description: "Report the example plugin's uptime, tick count, hook events seen and trigger ids.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Execute: func(_ context.Context, _ string, _ json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
				mu.Lock()
				uptime := int64(0)
				if !started.IsZero() {
					uptime = time.Since(started).Milliseconds()
				}
				currentTicks := ticks
				mu.Unlock()
				body := map[string]any{
					"plugin":   ExamplePluginID,
					"version":  ExamplePluginVersion,
					"uptimeMs": uptime,
					"ticks":    currentTicks,
					"hooks":    audit.counts(),
					"triggers": triggerIDs,
				}
				raw, _ := json.Marshal(body)
				return agentcore.AgentToolResult{
					Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
					StructuredContent: body,
				}, nil
			},
		}, {
			Name:        "example_audit_tail",
			Description: "Return the most recent hook payloads the example plugin observed (ids and statuses only).",
			Schema:      json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":64}},"additionalProperties":false}`),
			Execute: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
				var input struct {
					Limit int `json:"limit"`
				}
				_ = json.Unmarshal(args, &input)
				if input.Limit <= 0 {
					input.Limit = 10
				}
				entries := audit.tail(input.Limit)
				raw, _ := json.Marshal(map[string]any{"events": entries})
				return agentcore.AgentToolResult{
					Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
					StructuredContent: map[string]any{"events": entries},
				}, nil
			},
		}},
		Effects: []Effect{
			{Name: "example-audit-seal", Stop: func(context.Context) error {
				audit.seal()
				return nil
			}},
			{Name: "example-clock", Stop: func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				if ticker != nil {
					ticker.Stop()
					ticker = nil
				}
				if stop != nil {
					select {
					case <-stop:
					default:
						close(stop)
					}
					stop = nil
				}
				return nil
			}},
		},
		Start: func(ctx context.Context, env *PluginEnv) error {
			mu.Lock()
			started = time.Now()
			ticker = time.NewTicker(time.Second)
			stop = make(chan struct{})
			tickCh := ticker.C
			stopCh := stop
			mu.Unlock()
			go func() {
				for {
					select {
					case <-stopCh:
						return
					case <-ctx.Done():
						return
					case <-tickCh:
						mu.Lock()
						ticks++
						mu.Unlock()
					}
				}
			}()
			if env != nil && env.Logger != nil {
				env.Logger.Info("example plugin started", "plugin", ExamplePluginID, "tools", 2, "hooks", 3, "triggers", len(triggers))
			}
			return nil
		},
		Stop: func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			started = time.Time{}
			return nil
		},
	}
}

// exampleAudit is the example plugin's bounded in-memory view of the hooks it
// observed. Hook payloads carry identifiers only, so this is safe to expose.
type exampleAudit struct {
	mu      sync.Mutex
	entries []HookPayload
	sealed  bool
	countsM map[string]int
}

const exampleAuditLimit = 64

func (a *exampleAudit) add(event string, payload HookPayload) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sealed {
		return
	}
	if a.countsM == nil {
		a.countsM = make(map[string]int)
	}
	a.countsM[event]++
	a.entries = append(a.entries, payload)
	if len(a.entries) > exampleAuditLimit {
		a.entries = a.entries[len(a.entries)-exampleAuditLimit:]
	}
}

func (a *exampleAudit) counts() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.countsM))
	for key, value := range a.countsM {
		out[key] = value
	}
	return out
}

func (a *exampleAudit) tail(limit int) []HookPayload {
	a.mu.Lock()
	defer a.mu.Unlock()
	if limit > len(a.entries) {
		limit = len(a.entries)
	}
	out := make([]HookPayload, 0, limit)
	for _, entry := range a.entries[len(a.entries)-limit:] {
		out = append(out, entry)
	}
	return out
}

func (a *exampleAudit) seal() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sealed = true
}

// NewTestkitPlugin builds the disabled-by-default test plugin. clock/echo used
// to be builtin tools; they now enter the model surface only when this plugin is
// installed/enabled (and, for a bound run, granted to the agent). E2E uses that
// path to prove grant filtering still applies to plugin-provided tools.
func NewTestkitPlugin() *Plugin {
	return &Plugin{
		ID:       testkit.PluginID,
		Version:  "1.0.0",
		Provides: []string{"cap:testkit"},
		Tools: []ToolSpec{
			{
				Name:        "clock",
				Description: "Return the current time as an ISO 8601 string.",
				Schema:      (&testkit.ClockTool{}).Schema(),
				Execute: func(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
					return (&testkit.ClockTool{}).Execute(ctx, id, args, onUpdate)
				},
			},
			{
				Name:        "echo",
				Description: "Echo the given text back.",
				Schema:      (&testkit.EchoTool{}).Schema(),
				Execute: func(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
					return (&testkit.EchoTool{}).Execute(ctx, id, args, onUpdate)
				},
			},
		},
	}
}

// ExamplePingPrompt is the default prompt of the example webhook trigger.
func ExamplePingPrompt() string {
	return fmt.Sprintf("Example plugin ping (%s).", ExamplePingTriggerID)
}

// Capability plugin ids (change keel-bot-identity-access, A4.2b3a): the three
// management surfaces the product ships as declared plugins. Their behavioural
// surface is the CLI tree each manifest declares (`provides.cli`), enforced by
// the real `keel model` / `keel bot` / `keel team-bot` commands — so the Go side
// stays an empty in-process plugin: it carries its registry capability marker
// and contributes nothing to the model's tool face.
const (
	// ModelPluginID is the model-registry plugin (providers, models, families).
	ModelPluginID = "keel.model"
	// BotPluginID is the Bot-registry plugin (the Bot library).
	BotPluginID = "keel.bot"
	// TeamBotPluginID is the TeamBot plugin (mission, roster, captain).
	TeamBotPluginID = "keel.team-bot"
	// SpillPluginID is the keel.spill capability plugin: long content leaves the
	// request view and a durable reference stays (backend: internal/spill).
	SpillPluginID = "keel.spill"
	// CapabilityPluginVersion is the version all three capability plugins ship.
	CapabilityPluginVersion = "1.0.0"
)

// NewModelPlugin builds the model-registry plugin.
func NewModelPlugin() *Plugin {
	return &Plugin{ID: ModelPluginID, Version: CapabilityPluginVersion, Provides: []string{"cap:model-registry"}}
}

// NewBotPlugin builds the Bot-registry plugin.
func NewBotPlugin() *Plugin {
	return &Plugin{ID: BotPluginID, Version: CapabilityPluginVersion, Provides: []string{"cap:bot-registry"}}
}

// NewTeamBotPlugin builds the TeamBot plugin.
func NewTeamBotPlugin() *Plugin {
	return &Plugin{ID: TeamBotPluginID, Version: CapabilityPluginVersion, Provides: []string{"cap:team-bot-registry"}}
}

// NewSpillPlugin builds the keel.spill capability plugin. Its behavior lives in
// internal/spill + internal/compaction (the pruner), wired by the runner; the
// plugin entity carries the registry marker and the capability names so the
// plugin list, the audit surface and the builtin manifest describe the same
// thing. It contributes nothing to the model's tool face, which stays the fixed
// eleven aggregated tools.
func NewSpillPlugin() *Plugin {
	return &Plugin{ID: SpillPluginID, Version: CapabilityPluginVersion, Provides: []string{"cap:ctx-spill"}}
}
