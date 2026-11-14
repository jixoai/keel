package agentext

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// Built-in example plugin. It is a complete, non-trivial plugin: two tools, a
// hook subscription, a webhook trigger, two effects and a Start/Stop pair. It
// exists so the extension surface is verified end to end without a third-party
// plugin, and so an operator can POST /v1/triggers/keel.example.ping/fire on a
// fresh `keel serve` and watch a real turn start with real audit records.
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
	// useful for a demo scheduler; without both, only the webhook trigger is
	// declared.
	TenantID string
	Interval time.Duration
	// Prompt overrides the webhook turn text.
	Prompt string
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
	triggers := []TriggerSpec{{
		ID:       ExamplePingTriggerID,
		Kind:     TriggerWebhook,
		Prompt:   prompt,
		Enabled:  true,
		Owner:    ExamplePluginID,
		Schedule: "",
	}}
	if opts.Interval > 0 && opts.TenantID != "" {
		triggers = append(triggers, TriggerSpec{
			ID:       ExamplePluginID + ".heartbeat",
			Kind:     TriggerInterval,
			Schedule: opts.Interval.String(),
			Prompt:   "Heartbeat from the example plugin.",
			TenantID: opts.TenantID,
			Enabled:  true,
			Owner:    ExamplePluginID,
		})
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
					"triggers": []string{ExamplePingTriggerID},
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

// NewSubagentPlugin exposes the spawn_subagent primitive to the model. The tool
// needs the ambient run scope in its context; RunControl.BeginRun installs it,
// so a `keel serve` deployment with the worker's run-control seam gets the tool
// for free. Without that seam the tool answers with an actionable error instead
// of guessing a parent session.
func NewSubagentPlugin(runner *SubagentRunner) *Plugin {
	return &Plugin{
		ID:       "keel.subagents",
		Version:  "1.0.0",
		Provides: []string{"cap:subagent"},
		Tools: []ToolSpec{{
			Name:        "spawn_subagent",
			Description: "Spawn a sub-agent in an independent session and return its structured result (status, summary, usage).",
			Schema:      (&SubagentTool{}).Schema(),
			Execute: func(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
				tool := &SubagentTool{Runner: runner}
				return tool.Execute(ctx, id, args, onUpdate)
			},
		}},
	}
}

// ExamplePingPrompt is the default prompt of the example webhook trigger.
func ExamplePingPrompt() string {
	return fmt.Sprintf("Example plugin ping (%s).", ExamplePingTriggerID)
}

// Capability plugin ids (change keel-agent-team-v1): the three management
// surfaces the product ships as declared plugins. Their behavioural surface is
// the CLI tree each manifest declares (`provides.cli`), enforced by the real
// `keel model` / `keel agent` / `keel team` commands — so the Go side stays an
// empty in-process plugin: it carries its registry capability marker and
// contributes nothing to the model's tool face.
const (
	// ModelPluginID is the model-registry plugin (providers, models, families).
	ModelPluginID = "keel.model"
	// AgentPluginID is the agent-registry plugin (the Agent library).
	AgentPluginID = "keel.agent"
	// TeamPluginID is the agent-team plugin (teams, captain, members, the
	// captain's team-CLI switch).
	TeamPluginID = "keel.team"
	// CapabilityPluginVersion is the version all three capability plugins ship.
	CapabilityPluginVersion = "1.0.0"
)

// NewModelPlugin builds the model-registry plugin.
func NewModelPlugin() *Plugin {
	return &Plugin{ID: ModelPluginID, Version: CapabilityPluginVersion, Provides: []string{"cap:model-registry"}}
}

// NewAgentPlugin builds the agent-registry plugin.
func NewAgentPlugin() *Plugin {
	return &Plugin{ID: AgentPluginID, Version: CapabilityPluginVersion, Provides: []string{"cap:agent-registry"}}
}

// NewTeamPlugin builds the agent-team plugin.
func NewTeamPlugin() *Plugin {
	return &Plugin{ID: TeamPluginID, Version: CapabilityPluginVersion, Provides: []string{"cap:team-registry"}}
}
