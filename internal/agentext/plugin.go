package agentext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/pluginpkg"
)

// Agent plugin contract. A plugin is declarative: it declares what it provides,
// what it requires, the tools it adds to the model, the hooks it subscribes to,
// the triggers it owns and the effects it must clean up. Assembly is ordered and
// unload is the exact mirror image (reverse), so an assembly cycle can never
// leave a listener, a timer or a tool behind.
type PluginState string

const (
	PluginActive  PluginState = "active"
	PluginPending PluginState = "pending"
	PluginFailed  PluginState = "failed"
	PluginStopped PluginState = "stopped"
)

// ToolSpec is one model-facing tool a plugin contributes.
type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
	// Sequential forces the whole tool batch to run serially (tools with side
	// effects on shared state).
	Sequential bool
	Execute    func(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error)
}

// HookSpec is one hook subscription a plugin declares.
type HookSpec struct {
	Event   HookEvent
	Handler HookHandler
	Timeout time.Duration
}

// PromptSection is one named block of text a plugin contributes to the session's
// system prompt while the plugin is loaded, in the same shape the host assembles
// its own sections (a preamble plus named sections). It is how a plugin teaches
// the model a vocabulary — `keel.creation` contributes the plugin standard — and
// it is declarative on purpose: the hook bus is a fire-and-forget observation
// stream (bounded payloads, handlers that cannot return text), so a prompt
// contribution is part of the plugin contract, published through the same sink
// pattern as tools, not smuggled through an event.
type PromptSection struct {
	// Name is the section name (the host renders <name>...</name>).
	Name string `json:"name"`
	// Content is the text. It must not carry a credential; a plugin's prompt is
	// shown to the model.
	Content string `json:"content"`
}

// Effect is a named cleanup a plugin must run on unload, in reverse declaration
// order.
type Effect struct {
	Name string
	Stop func(ctx context.Context) error
}

// PluginEnv is what Start receives: the live product services, never the kernel.
type PluginEnv struct {
	Hooks     *HookBus
	Decisions *DecisionBroker
	Triggers  *TriggerEngine
	Subagents *SubagentRunner
	Partials  *PartialHub
	Logger    *slog.Logger
}

// Plugin is one agent extension.
type Plugin struct {
	ID       string
	Version  string
	Provides []string
	Requires []string
	Tools    []ToolSpec
	Hooks    []HookSpec
	Triggers []TriggerSpec
	Effects  []Effect
	// PromptSections are the system-prompt blocks this plugin contributes while
	// it is active. They are published through RuntimeOptions.PromptSink and
	// appear in the runtime snapshot, so a host can rebuild its prompt on every
	// assembly exactly as it rebuilds its tool registry.
	PromptSections []PromptSection
	// Start runs before the plugin's tools/hooks/triggers are applied; an error
	// marks the plugin FAILED (its dependents stay PENDING).
	Start func(ctx context.Context, env *PluginEnv) error
	// Stop runs after hooks/triggers/effects are removed.
	Stop func(ctx context.Context) error
}

// ToolSink receives the active tool set after every assembly. The hosting
// process publishes it to the model registry (servicehost's product assembly
// point); the runtime never reaches into the host directly.
type ToolSink func(tools []agentcore.AgentTool) error

// PromptSink receives the active prompt sections after every assembly, in
// activation order. It mirrors ToolSink: the host decides how a section reaches
// its system prompt (servicehost rebuilds the host assembly), and the runtime
// stays free of prompt concerns. A nil sink still exposes the sections through
// Snapshot().Prompt.
type PromptSink func(sections []PromptSection) error

// RuntimeOptions configures the plugin runtime.
type RuntimeOptions struct {
	Plugins   []*Plugin
	Hooks     *HookBus
	Triggers  *TriggerEngine
	Decisions *DecisionBroker
	Subagents *SubagentRunner
	Partials  *PartialHub
	Tools     ToolSink
	// Prompt publishes the active prompt sections (nil keeps them snapshot-only).
	Prompt func(sections []PromptSection) error
	Logger *slog.Logger
	Now    func() time.Time
}

// PluginStatus is the observable plugin state (GET /v1/plugins).
type PluginStatus struct {
	ID       string        `json:"id"`
	Version  string        `json:"version,omitempty"`
	State    PluginState   `json:"state"`
	Provides []string      `json:"provides,omitempty"`
	Requires []string      `json:"requires,omitempty"`
	Tools    []string      `json:"tools,omitempty"`
	Hooks    []HookEvent   `json:"hooks,omitempty"`
	Triggers []TriggerSpec `json:"triggers,omitempty"`
	Effects  []string      `json:"effects,omitempty"`
	// Prompt is the system-prompt contribution this plugin makes while it is
	// active (empty for a plugin that contributes none, or is not loaded).
	Prompt      []PromptSection `json:"prompt,omitempty"`
	Diagnostics []string        `json:"diagnostics,omitempty"`
	StartedAt   *time.Time      `json:"startedAt,omitempty"`
	StoppedAt   *time.Time      `json:"stoppedAt,omitempty"`
}

// Snapshot is the assembled extension surface.
type Snapshot struct {
	Generation int64          `json:"generation"`
	Plugins    []PluginStatus `json:"plugins"`
	Hooks      StatsSummary   `json:"hooks"`
	Tools      []string       `json:"tools"`
	// Prompt is the effective prompt contribution of the active plugins, in
	// activation order.
	Prompt []PromptSection `json:"prompt,omitempty"`
}

// Runtime assembles plugins in dependency order and unloads them in reverse.
type Runtime struct {
	hooks      *HookBus
	triggers   *TriggerEngine
	decisions  *DecisionBroker
	subagents  *SubagentRunner
	partials   *PartialHub
	sink       ToolSink
	promptSink func(sections []PromptSection) error
	log        *slog.Logger
	now        func() time.Time

	mu         sync.Mutex
	plugins    []*Plugin
	generation int64
	active     []*activatedPlugin
	statuses   map[string]PluginStatus
	closed     bool
	// lifeCtx is the plugin lifetime: cancelled by Close, so a Start that spawned
	// background work (a ticker, a subscription) stops with the runtime instead of
	// with the activation call.
	lifeCtx    context.Context
	lifeCancel context.CancelFunc
}

type activatedPlugin struct {
	plugin       *Plugin
	unsubs       []func()
	triggerStops []func()
	effectsRun   []bool
	tools        []agentcore.AgentTool
	startedAt    time.Time
}

// NewRuntime builds a plugin runtime. Every service is optional; a plugin that
// needs a missing service fails with a readable diagnostic.
func NewRuntime(opts RuntimeOptions) *Runtime {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	return &Runtime{
		hooks:      opts.Hooks,
		triggers:   opts.Triggers,
		decisions:  opts.Decisions,
		subagents:  opts.Subagents,
		partials:   opts.Partials,
		sink:       opts.Tools,
		promptSink: opts.Prompt,
		log:        opts.Logger,
		now:        opts.Now,
		plugins:    append([]*Plugin(nil), opts.Plugins...),
		statuses:   make(map[string]PluginStatus),
		lifeCtx:    lifeCtx,
		lifeCancel: lifeCancel,
	}
}

// Apply (re)assembles the configured plugins. A previous assembly is unloaded
// in reverse order first; the generation increments exactly once per apply.
func (r *Runtime) Apply(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("agentext: plugin runtime is closed")
	}
	plugins := append([]*Plugin(nil), r.plugins...)
	r.mu.Unlock()
	return r.assemble(ctx, plugins)
}

// ApplyPlugins replaces the plugin set and reassembles.
func (r *Runtime) ApplyPlugins(ctx context.Context, plugins []*Plugin) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("agentext: plugin runtime is closed")
	}
	r.plugins = append([]*Plugin(nil), plugins...)
	r.mu.Unlock()
	return r.assemble(ctx, plugins)
}

func (r *Runtime) assemble(ctx context.Context, plugins []*Plugin) error {
	// Publication normalization happens here, once, for every loader: the model
	// faces one wire grammar, so a tool name carrying '.' or ':' is published
	// with '_' instead. A manifest that would publish an uncallable name is
	// already refused at install (pluginpkg validation); this is the assembly's
	// own guarantee, so a plugin registered from Go cannot bypass it either. An
	// already compliant name is returned verbatim — `example_status` and
	// `spawn_subagent` must never change.
	plugins = publishToolNames(plugins)

	r.unload(ctx)

	r.mu.Lock()
	r.generation++
	generation := r.generation
	r.statuses = make(map[string]PluginStatus, len(plugins))
	for _, plugin := range plugins {
		if plugin == nil || plugin.ID == "" {
			continue
		}
		r.statuses[plugin.ID] = PluginStatus{
			ID:       plugin.ID,
			Version:  plugin.Version,
			State:    PluginPending,
			Provides: append([]string(nil), plugin.Provides...),
			Requires: append([]string(nil), plugin.Requires...),
			Hooks:    hookNames(plugin.Hooks),
			Tools:    toolNames(plugin.Tools),
			Effects:  effectNames(plugin.Effects),
			Triggers: append([]TriggerSpec(nil), plugin.Triggers...),
			Prompt:   append([]PromptSection(nil), plugin.PromptSections...),
		}
	}
	r.mu.Unlock()

	// Duplicate tool names are a fail-closed condition: the model faces one
	// namespace, so two plugins cannot own the same name.
	toolOwners := make(map[string]string)
	for _, plugin := range plugins {
		if plugin == nil {
			continue
		}
		for _, tool := range plugin.Tools {
			if owner, exists := toolOwners[tool.Name]; exists && owner != plugin.ID {
				r.diagnose(plugin.ID, fmt.Sprintf("tool %q is also declared by plugin %q", tool.Name, owner))
			}
			toolOwners[tool.Name] = plugin.ID
		}
	}

	provided := make(map[string]struct{})
	remaining := make([]*Plugin, 0, len(plugins))
	for _, plugin := range plugins {
		if plugin != nil && plugin.ID != "" {
			remaining = append(remaining, plugin)
		}
	}
	activated := make([]*activatedPlugin, 0, len(remaining))
	for {
		progress := false
		next := remaining[:0]
		for _, plugin := range remaining {
			if !requiresSatisfied(plugin.Requires, provided) {
				next = append(next, plugin)
				continue
			}
			// A duplicate tool declaration makes the plugin fail; the check is
			// repeated here so dependency order does not hide it.
			if conflict := r.toolConflict(plugin, activated); conflict != "" {
				r.fail(plugin.ID, conflict)
				progress = true
				continue
			}
			entry, err := r.activate(ctx, plugin)
			if err != nil {
				r.fail(plugin.ID, err.Error())
				r.log.Warn("plugin activation failed", "plugin", plugin.ID, "generation", generation, "err", err)
				progress = true
				continue
			}
			activated = append(activated, entry)
			for _, capability := range plugin.Provides {
				provided[capability] = struct{}{}
			}
			r.markActive(plugin.ID, entry.startedAt)
			progress = true
		}
		remaining = next
		if !progress || len(remaining) == 0 {
			break
		}
	}
	for _, plugin := range remaining {
		var missing []string
		for _, requirement := range plugin.Requires {
			if _, ok := provided[requirement]; !ok {
				missing = append(missing, requirement)
			}
		}
		if len(missing) == 0 {
			missing = []string{"dependency activation did not complete"}
		}
		r.setDiagnostics(plugin.ID, PluginPending, fmt.Sprintf("waiting for: %s", strings.Join(missing, ", ")))
		r.log.Info("plugin pending", "plugin", plugin.ID, "missing", strings.Join(missing, ", "))
	}

	r.mu.Lock()
	r.active = activated
	r.mu.Unlock()

	if err := r.publish(); err != nil {
		return err
	}
	r.log.Info("plugin assembly", "generation", generation, "plugins", len(plugins), "active", len(activated),
		"pending", len(remaining))
	return nil
}

func (r *Runtime) activate(ctx context.Context, plugin *Plugin) (*activatedPlugin, error) {
	env := &PluginEnv{
		Hooks:     r.hooks,
		Decisions: r.decisions,
		Triggers:  r.triggers,
		Subagents: r.subagents,
		Partials:  r.partials,
		Logger:    r.log,
	}
	if plugin.Start != nil {
		// The lifecycle context, not the activation context: activation returns
		// right after Apply and a Start that owns background work must keep
		// running until the runtime closes.
		if err := plugin.Start(r.lifeCtx, env); err != nil {
			return nil, fmt.Errorf("start: %w", err)
		}
	}
	entry := &activatedPlugin{plugin: plugin, startedAt: r.now()}
	// Effects are cleaned in reverse; effectsRun tracks which ones were already
	// executed so a partial unload never double-runs one.
	entry.effectsRun = make([]bool, len(plugin.Effects))

	if r.hooks != nil {
		for _, spec := range plugin.Hooks {
			unsub, err := r.hooks.Subscribe(plugin.ID, HookOptions{
				Event:   spec.Event,
				Handler: spec.Handler,
				Timeout: spec.Timeout,
			})
			if err != nil {
				entry.cleanup(ctx, r.log)
				return nil, fmt.Errorf("subscribe %s: %w", spec.Event, err)
			}
			entry.unsubs = append(entry.unsubs, unsub)
		}
	} else if len(plugin.Hooks) > 0 {
		entry.cleanup(ctx, r.log)
		return nil, errors.New("hook bus is not wired")
	}

	if r.triggers != nil {
		for _, spec := range plugin.Triggers {
			if spec.ID == "" {
				spec.ID = plugin.ID + "." + strings.ToLower(string(spec.Kind)) + "." + fmt.Sprintf("%d", len(entry.triggerStops))
			}
			if spec.Owner == "" {
				spec.Owner = plugin.ID
			}
			if spec.Enabled && (spec.Kind == TriggerInterval || spec.Kind == TriggerCron) && spec.TenantID == "" {
				entry.cleanup(ctx, r.log)
				return nil, fmt.Errorf("trigger %s needs a tenantId", spec.ID)
			}
			stop, err := r.triggers.Register(spec)
			if err != nil {
				entry.cleanup(ctx, r.log)
				return nil, fmt.Errorf("trigger %s: %w", spec.ID, err)
			}
			entry.triggerStops = append(entry.triggerStops, stop)
		}
	} else if len(plugin.Triggers) > 0 {
		entry.cleanup(ctx, r.log)
		return nil, errors.New("trigger engine is not wired")
	}

	for _, tool := range plugin.Tools {
		entry.tools = append(entry.tools, &pluginTool{owner: plugin.ID, spec: tool})
	}
	return entry, nil
}

func (entry *activatedPlugin) cleanup(ctx context.Context, log *slog.Logger) {
	for i := len(entry.triggerStops) - 1; i >= 0; i-- {
		entry.triggerStops[i]()
	}
	entry.triggerStops = nil
	for i := len(entry.unsubs) - 1; i >= 0; i-- {
		entry.unsubs[i]()
	}
	entry.unsubs = nil
	for i := len(entry.plugin.Effects) - 1; i >= 0; i-- {
		if entry.effectsRun[i] {
			continue
		}
		entry.effectsRun[i] = true
		effect := entry.plugin.Effects[i]
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		err := effect.Stop(stopCtx)
		cancel()
		if err != nil {
			log.Warn("plugin effect cleanup failed", "plugin", entry.plugin.ID, "effect", effect.Name, "err", err)
		}
	}
	if entry.plugin.Stop != nil {
		if err := entry.plugin.Stop(context.WithoutCancel(ctx)); err != nil {
			log.Warn("plugin stop failed", "plugin", entry.plugin.ID, "err", err)
		}
	}
}

// unload tears the current assembly down in reverse activation order.
func (r *Runtime) unload(ctx context.Context) {
	r.mu.Lock()
	active := r.active
	r.active = nil
	r.mu.Unlock()
	for i := len(active) - 1; i >= 0; i-- {
		entry := active[i]
		entry.cleanup(ctx, r.log)
		r.mu.Lock()
		status := r.statuses[entry.plugin.ID]
		status.State = PluginStopped
		stopped := r.now()
		status.StoppedAt = &stopped
		if status.StartedAt != nil {
			status.Diagnostics = append(status.Diagnostics, fmt.Sprintf("ran for %s", stopped.Sub(*status.StartedAt).Round(time.Millisecond)))
		}
		r.statuses[entry.plugin.ID] = status
		r.mu.Unlock()
	}
}

// publish publishes the assembled surface: the tool registry and the prompt
// sections, through their own sinks. Both are published on every assembly, so a
// freshly enabled plugin is visible (and a disabled one disappears) without a
// restart — the same rule the tool registry already follows.
func (r *Runtime) publish() error {
	r.mu.Lock()
	tools := make([]agentcore.AgentTool, 0)
	for _, entry := range r.active {
		tools = append(tools, entry.tools...)
	}
	sections := r.activePromptSectionsLocked()
	r.mu.Unlock()
	if r.sink != nil {
		if err := r.sink(tools); err != nil {
			return fmt.Errorf("agentext: publish plugin tools: %w", err)
		}
	}
	if r.promptSink != nil {
		if err := r.promptSink(sections); err != nil {
			return fmt.Errorf("agentext: publish plugin prompt sections: %w", err)
		}
	}
	return nil
}

// activePromptSectionsLocked collects the prompt sections of the active plugins,
// in activation order. The caller holds r.mu.
func (r *Runtime) activePromptSectionsLocked() []PromptSection {
	sections := make([]PromptSection, 0)
	for _, entry := range r.active {
		for _, section := range entry.plugin.PromptSections {
			if strings.TrimSpace(section.Content) == "" {
				continue
			}
			sections = append(sections, section)
		}
	}
	return sections
}

func (r *Runtime) toolConflict(plugin *Plugin, activated []*activatedPlugin) string {
	owned := make(map[string]string)
	for _, entry := range activated {
		for _, tool := range entry.plugin.Tools {
			owned[tool.Name] = entry.plugin.ID
		}
	}
	for _, tool := range plugin.Tools {
		if owner, exists := owned[tool.Name]; exists {
			return fmt.Sprintf("tool %q is already owned by plugin %q", tool.Name, owner)
		}
	}
	return ""
}

func (r *Runtime) markActive(id string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.statuses[id]
	status.State = PluginActive
	status.StartedAt = &at
	r.statuses[id] = status
}

func (r *Runtime) fail(id, diagnostic string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.statuses[id]
	status.State = PluginFailed
	status.Diagnostics = append(status.Diagnostics, diagnostic)
	r.statuses[id] = status
}

func (r *Runtime) diagnose(id, diagnostic string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.statuses[id]
	status.Diagnostics = append(status.Diagnostics, diagnostic)
	r.statuses[id] = status
}

func (r *Runtime) setDiagnostics(id string, state PluginState, diagnostic string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.statuses[id]
	status.State = state
	status.Diagnostics = append(status.Diagnostics, diagnostic)
	r.statuses[id] = status
}

// Snapshot returns the assembled extension surface.
func (r *Runtime) Snapshot() Snapshot {
	r.mu.Lock()
	statuses := make([]PluginStatus, 0, len(r.statuses))
	for _, status := range r.statuses {
		copied := status
		copied.Diagnostics = append([]string(nil), status.Diagnostics...)
		copied.Tools = append([]string(nil), status.Tools...)
		copied.Hooks = append([]HookEvent(nil), status.Hooks...)
		copied.Triggers = append([]TriggerSpec(nil), status.Triggers...)
		copied.Effects = append([]string(nil), status.Effects...)
		copied.Prompt = append([]PromptSection(nil), status.Prompt...)
		statuses = append(statuses, copied)
	}
	generation := r.generation
	tools := make([]string, 0)
	for _, entry := range r.active {
		for _, tool := range entry.tools {
			tools = append(tools, tool.Name())
		}
	}
	prompt := r.activePromptSectionsLocked()
	r.mu.Unlock()
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
	sort.Strings(tools)
	snapshot := Snapshot{Generation: generation, Plugins: statuses, Tools: tools, Prompt: prompt}
	if r.hooks != nil {
		snapshot.Hooks = r.hooks.Summary()
	}
	return snapshot
}

// Generation reports the current assembly generation.
func (r *Runtime) Generation() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation
}

// Close unloads everything and forbids further assembly.
func (r *Runtime) Close(ctx context.Context) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	r.lifeCancel()
	r.unload(ctx)
	if r.sink != nil {
		_ = r.sink(nil)
	}
	if r.promptSink != nil {
		_ = r.promptSink(nil)
	}
}

func requiresSatisfied(requires []string, provided map[string]struct{}) bool {
	for _, requirement := range requires {
		if _, ok := provided[requirement]; !ok {
			return false
		}
	}
	return true
}

// publishToolNames copies the plugin set with every tool name normalized to the
// model-facing wire grammar (pluginpkg.NormalizePublishedToolName). The plugin
// values are copied rather than mutated: the caller's set stays the declarative
// surface, and the assembly owns what it publishes. Compliance is idempotent, so
// applying this twice (Apply → ApplyPlugins) changes nothing.
func publishToolNames(plugins []*Plugin) []*Plugin {
	out := make([]*Plugin, 0, len(plugins))
	for _, plugin := range plugins {
		if plugin == nil || len(plugin.Tools) == 0 {
			out = append(out, plugin)
			continue
		}
		copied := *plugin
		copied.Tools = make([]ToolSpec, len(plugin.Tools))
		for i, tool := range plugin.Tools {
			tool.Name = pluginpkg.NormalizePublishedToolName(tool.Name)
			copied.Tools[i] = tool
		}
		out = append(out, &copied)
	}
	return out
}

func hookNames(specs []HookSpec) []HookEvent {
	out := make([]HookEvent, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.Event)
	}
	return out
}

func toolNames(specs []ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.Name)
	}
	sort.Strings(out)
	return out
}

func effectNames(effects []Effect) []string {
	out := make([]string, 0, len(effects))
	for _, effect := range effects {
		out = append(out, effect.Name)
	}
	return out
}

// pluginTool adapts a ToolSpec to agentcore.AgentTool.
type pluginTool struct {
	owner string
	spec  ToolSpec
}

func (t *pluginTool) Name() string        { return t.spec.Name }
func (t *pluginTool) Description() string { return t.spec.Description }
func (t *pluginTool) Schema() json.RawMessage {
	if len(t.spec.Schema) == 0 {
		return json.RawMessage(`{"type":"object","additionalProperties":true}`)
	}
	return t.spec.Schema
}

func (t *pluginTool) ExecutionMode() agentcore.ToolExecutionMode {
	if t.spec.Sequential {
		return agentcore.ToolExecutionSequential
	}
	return agentcore.ToolExecutionParallel
}

func (t *pluginTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	if t.spec.Execute == nil {
		return errorToolResult(fmt.Sprintf("tool %s is declared by plugin %s but has no executor", t.spec.Name, t.owner)), nil
	}
	return t.spec.Execute(ctx, id, args, onUpdate)
}
