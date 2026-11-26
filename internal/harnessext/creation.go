package harnessext

// Creation mode (change keel-plugin-core, spec: 创造模式（作为内置插件
// `keel.creation`）). It is an official plugin like any other — the manifest lives
// in internal/pluginpkg/builtin/keel.creation.yaml, an operator can disable it,
// and it contributes its own prompt section, tools and (optional) UI.
//
// The implementation premise, stated so it cannot drift: creation mode does not
// call a model from inside a plugin. The MAIN session's agent writes the plugin
// using the scaffold and the standard this plugin contributes — the tools are
// the agent's hands on the filesystem and on the plugin manager:
//
// The model reaches them through the harness-owned `creation` aggregate tool:
//
//	creation.catalog   read-only aggregate of installed plugins and their surface.
//	creation.scaffold  render the template into `<plugins-root>/.creation/<id>`,
//	                   then validate the OUTPUT with the real package validator.
//	creation.validate  field-level problems of a staged package (plus the
//	                   structural and host-prerequisite warnings).
//	creation.install   durable approval → manager Install → manager Enable →
//	                   deep link. The approval is the same primitive the API uses
//	                   (/v1/plugin-approvals answers it), and it is two-phase
//	                   because a human answer does not fit in one tool call.
//	creation.list      what this session generated and where it stands.
//	creation.remove    durable approval → disable → uninstall → delete the staged
//	                   artifact.
//
// Failure discipline: a scaffold renders into a temporary directory and only
// moves into the staging path after the real validator accepts it, so a failed
// generation leaves nothing; a failed install removes the staged package and
// writes audit rows. Nothing here can leave a half-package.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/plugins/templates"
)

// The creation plugin's identity.
const (
	// CreationPluginID is the builtin plugin id (the manifest lives in
	// internal/pluginpkg/builtin/keel.creation.yaml).
	CreationPluginID = "keel.creation"
	// CreationPluginVersion is the bundled version.
	CreationPluginVersion = "1.0.0"
	// CreationStagingDir is where generated packages live before installation.
	// It is hidden, so plugin discovery never mistakes a staged package for an
	// installed one.
	CreationStagingDir = ".creation"
	// creationTemplate is the package template every scaffold renders.
	creationTemplate = templates.WebviewReactSolidglass
	// creationDefaultVersion is the version a scaffold gives a new package.
	creationDefaultVersion = "0.1.0"
	// creationDefaultTool is the tool name a scaffold gives a new package when
	// the caller does not name one.
	creationDefaultTool = "panel_refresh"
)

// Approval vocabulary. The option ids match the lifecycle approval gate
// (internal/harnessapi): one answer vocabulary for every plugin write, so a client
// that can answer one can answer the other.
const (
	creationAllowOption = "allow"
	creationDenyOption  = "deny"
	// creationApprovalNamespace seeds the tenant-scoped approval session, the
	// same namespace /v1/plugin-approvals reads and answers. A question opened
	// inside a turn lives in that turn's session instead, so it also shows up in
	// the session's own decision list.
	creationApprovalNamespace = "keel.plugin.approval.v1"
)

// Internal action handler names. They are namespaced and never published to the
// model: the harness-owned `creation` tool owns the model-facing name, and
// Manager.InvokeToolResult dispatches to these handlers. The v1 creation_*
// names are deleted with no alias.
const (
	CreationToolCatalog  = "keel.creation.catalog"
	CreationToolScaffold = "keel.creation.scaffold"
	CreationToolRead     = "keel.creation.read"
	CreationToolWrite    = "keel.creation.write"
	CreationToolValidate = "keel.creation.validate"
	CreationToolInstall  = "keel.creation.install"
	CreationToolList     = "keel.creation.list"
	CreationToolRemove   = "keel.creation.remove"
)

// CreationOptions wires the creation plugin.
type CreationOptions struct {
	// Manager resolves the plugin lifecycle authority. It is a getter because the
	// host registers builtin factories before the manager exists; by the time a
	// tool executes the getter always resolves.
	Manager func() *Manager
	// TenantID is the deployment's tenant, used for approval and audit
	// attribution when a call carries no ambient run scope (a bridge call from a
	// plugin page). A call inside a turn always uses the run's tenant.
	TenantID uuid.UUID
	// Template overrides the template name (default webview-react-solidglass).
	Template string
	// Now overrides the clock (approval rows, audit stamps; tests).
	Now func() time.Time
	// Logger is the plugin logger (defaults to the PluginEnv logger).
	Logger *slog.Logger
}

// NewCreationPlugin builds the creation plugin. Every tool is sequential: they
// write the filesystem and drive the manager, so a batch must not interleave.
func NewCreationPlugin(opts CreationOptions) *Plugin {
	plugin := &creationPlugin{opts: opts, template: opts.Template}
	if plugin.template == "" {
		plugin.template = creationTemplate
	}
	return &Plugin{
		ID:             CreationPluginID,
		Version:        CreationPluginVersion,
		Provides:       []string{"cap:plugin.creation"},
		PromptSections: []PromptSection{creationPromptSection()},
		Tools:          plugin.tools(),
		Start: func(_ context.Context, env *PluginEnv) error {
			plugin.env = env
			if env != nil && env.Logger != nil {
				env.Logger.Info("creation plugin started", "tools", 5, "template", plugin.template)
			}
			return nil
		},
	}
}

// CreationPromptSectionName is the prompt section the plugin contributes.
const CreationPromptSectionName = "plugin_standard"

// CreationPrompt returns the standard text the plugin contributes to the
// session's system prompt. It is exported so a test (and an operator reading
// /v1/plugins) can assert its content without reaching into the plugin.
func CreationPrompt() string { return creationStandardText }

// creationPromptSection builds the contribution as the contract's section type.
func creationPromptSection() PromptSection {
	return PromptSection{Name: CreationPromptSectionName, Content: creationStandardText}
}

// creationStandardText is the plugin standard: the manifest in full, the three
// kinds, where the template lives and how the flow goes. It names no upstream
// project: the vocabulary is this product's.
const creationStandardText = `Creation mode builds plugins by writing real plugin packages. You are the author;
the creation tool's actions are your hands. Never describe a plugin instead of building one.

A plugin is a directory <plugins-root>/<id>/ containing plugin.yaml:

  id: vendor.name              # ^[a-z][a-z0-9._-]{1,63}$, globally unique
  version: 1.2.3               # semantic version
  kind: in-process|sidecar|webview
  description: one line        # shown in the plugin list
  engines: { keel: ">=0.1.0 <1.0.0" }         # host compatibility gate
  provides:
    tools: [name, ...]         # see "published names" below
    hooks: [event, ...]        # closed set: subagent.*, trigger.*, turn.*, decision.*
    triggers: [id, ...]
    ui: { entry: ui/index.html, title: "..." }   # required for kind webview
  requires: [plugin-or-service-id, ...]      # missing dependency = not loaded
  permissions: { fs: [...], net: [host...], secrets: [NAME...], tools: [name...] }
  entry: { command: [...], args: [...], env: [...] }   # required for kind sidecar
  source: { type: builtin|local|git|npm, ... }

The three kinds:
- in-process: compiled into the host binary. A package of this kind cannot be
  installed at run time; only the host's own plugins are in-process.
- sidecar: an external process speaking stdio newline-delimited JSON-RPC
  (initialize, tools/list, tools/call, close). This is what a new plugin ships
  when it needs code the host does not have. Any language works.
- webview: a UI resource package hosted at /plugins/<id>/...; it may declare an
  optional sidecar backend in entry, which is how a panel gets tools.

The generated package also carries the extension documents:
  contributes: [{slot, form: chunk, entry: ui/settings.js}]   # Native chunk
  extensibility: { doc: EXTENSIBILITY.md, schema: extensibility.json }
The chunk is imported into the host tree (same VM, host theme tokens, web and
native; the TUI only loads chunks bundled with the app). extensibility.json and
EXTENSIBILITY.md describe the same surface and are validated against
plugin.yaml (drift is refused).

Published tool names must be wire safe: the host joins <id>.<local tool> and
replaces every character outside [A-Za-z0-9_-] with '_', so plugin "demo.panel"
with tool "refresh" is published as "demo_panel_refresh". A name that is already
safe is unchanged. An in-process plugin's declared name is its published name.

The template (webview + React panel + host theme tokens + one backend tool +
one Native chunk contribution):
  plugins/templates/webview-react-solidglass/

Workflow, in order:
1. creation {action:"scaffold", id, purpose, title?, tool?, version?} writes a
   complete package to the staging directory and validates it with the real
   validator.
2. Edit the staged package when the purpose needs more than the template:
   creation {action:"read", id} lists it, creation {action:"read", id, file}
   returns one file, and creation {action:"write", id, file, content} replaces
   it. Build the backend (backend/server.mjs), the panel (ui/app.jsx +
   ui/app.js) and the Native chunk (ui/settings.js) you actually need, and keep
   extensibility.json in step with plugin.yaml.
3. creation {action:"validate", id} returns field-level problems. Fix them; do not ignore.
4. creation {action:"install", id} asks for a durable approval and answers
   approval_required with a decisionId and an answerUrl. Have it answered, then
   call creation {action:"install", id, decisionId} again. A refusal changes nothing;
   repeating the same requestId is idempotent.
5. The success document carries deepLink: /plugins/<id>/ and chunkEntry:
   /plugins/<id>/ui/settings.js — those are what the client renders.

creation {action:"catalog"} is the read-only aggregate of installed plugins.
creation {action:"list"} shows the staged artifacts and their state;
creation {action:"remove"} (also approval-gated) uninstalls the plugin and
deletes the staged package.

Rules:
- Never claim an installation that answered approval_required, pending or refused.
- A failure leaves no half-package: the tools clean their staging directory.
- The manifest is the contract: permissions are enforced at dispatch, an
  undeclared tool is refused and audited, and a sidecar only receives the
  environment variable NAMES its manifest declares.
- Plugin UI talks to the host only through keel.plugin.* (state/call/approve/
  subscribe), renders with the host theme tokens, and never receives a secret.`

// creationPlugin is the plugin's own state. Every value it needs at call time is
// resolved from the PluginEnv it was started with (the manager through the
// option getter), so a disable/enable cycle is a clean restart.
type creationPlugin struct {
	opts     CreationOptions
	template string

	env *PluginEnv
}

func (c *creationPlugin) manager() *Manager {
	if c == nil || c.opts.Manager == nil {
		return nil
	}
	return c.opts.Manager()
}

func (c *creationPlugin) logger() *slog.Logger {
	if c.opts.Logger != nil {
		return c.opts.Logger
	}
	if c.env != nil && c.env.Logger != nil {
		return c.env.Logger
	}
	return slog.Default()
}

func (c *creationPlugin) now() time.Time {
	if c.opts.Now != nil {
		return c.opts.Now()
	}
	return time.Now().UTC()
}

func (c *creationPlugin) decisions() *DecisionBroker {
	if c.env == nil {
		return nil
	}
	return c.env.Decisions
}

// --- tools ----------------------------------------------------------------

func (c *creationPlugin) tools() []ToolSpec {
	return []ToolSpec{
		{
			Name:        CreationToolCatalog,
			Description: "Read-only catalog of installed plugins: id, version, state, declared surface and live model-facing tools.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`),
			Sequential: true,
			Internal:   true,
			Execute:    c.catalog,
		},
		{
			Name:        CreationToolScaffold,
			Description: "Render the plugin template into the staging directory (a complete, already validated package) and return its path, hash and published tool names.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "plugin id, ^[a-z][a-z0-9._-]{1,63}$"},
    "purpose": {"type": "string", "description": "one line: what the plugin is for (becomes the manifest description)"},
    "title": {"type": "string", "description": "panel title (defaults to the purpose)"},
    "tool": {"type": "string", "description": "local tool name the backend serves (default panel_refresh)"},
    "version": {"type": "string", "description": "package version (default 0.1.0)"},
    "kind": {"type": "string", "enum": ["webview"], "description": "template kind; the shipped template is a webview package with a sidecar backend"}
  },
  "required": ["id", "purpose"],
  "additionalProperties": false
}`),
			Sequential: true,
			Internal:   true,
			Execute:    c.scaffold,
		},
		{
			Name:        CreationToolRead,
			Description: "Read a staged package: with file, returns that file's UTF-8 text; without file, lists the package files with sizes. The staged package is the creation workspace, so this is how you inspect what scaffold wrote before editing it.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "plugin id of a staged package"},
    "file": {"type": "string", "description": "path relative to the package (default: list the files)"}
  },
  "required": ["id"],
  "additionalProperties": false
}`),
			Sequential: true,
			Internal:   true,
			Execute:    c.read,
		},
		{
			Name:        CreationToolWrite,
			Description: "Write one UTF-8 file inside a staged package. The path is relative to the package; it cannot leave the staging directory. Use it to make the package do what the purpose needs, then call validate.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "plugin id of a staged package"},
    "file": {"type": "string", "description": "path relative to the package, e.g. ui/settings.js"},
    "content": {"type": "string", "description": "the complete new file content"}
  },
  "required": ["id", "file", "content"],
  "additionalProperties": false
}`),
			Sequential: true,
			Internal:   true,
			Execute:    c.write,
		},
		{
			Name:        CreationToolValidate,
			Description: "Validate a staged plugin package with the real package validator (manifest fields, UI entry, engine gate) and the template's structural checks.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "plugin id of a staged package"},
    "path": {"type": "string", "description": "an explicit staged package directory (defaults to the staging path of id)"}
  },
  "required": ["id"],
  "additionalProperties": false
}`),
			Sequential: true,
			Internal:   true,
			Execute:    c.validate,
		},
		{
			Name:        CreationToolInstall,
			Description: "Install and enable a staged plugin package through the plugin manager, which requires a durable approval. Call without decisionId to open the question; call again with the answered decisionId to proceed. Returns the /plugins/<id>/ deep link.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "plugin id of the staged package"},
    "requestId": {"type": "string", "description": "opener idempotency key; repeat it to address the same question"},
    "decisionId": {"type": "string", "description": "the answered approval decision that authorizes this install"}
  },
  "required": ["id"],
  "additionalProperties": false
}`),
			Sequential: true,
			Internal:   true,
			Execute:    c.install,
		},
		{
			Name:        CreationToolList,
			Description: "List the plugin packages this deployment generated (staged artifacts), each with its validation state and whether it is installed.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`),
			Sequential: true,
			Internal:   true,
			Execute:    c.list,
		},
		{
			Name:        CreationToolRemove,
			Description: "Remove a generated plugin: uninstall it if installed, then delete the staged package. Requires a durable approval (two-phase, like install).",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "plugin id to remove"},
    "requestId": {"type": "string", "description": "opener idempotency key; repeat it to address the same question"},
    "decisionId": {"type": "string", "description": "the answered approval decision that authorizes this removal"}
  },
  "required": ["id"],
  "additionalProperties": false
}`),
			Sequential: true,
			Internal:   true,
			Execute:    c.remove,
		},
	}
}

// --- arguments ------------------------------------------------------------

type creationScaffoldArgs struct {
	ID      string `json:"id"`
	Purpose string `json:"purpose"`
	Title   string `json:"title"`
	Tool    string `json:"tool"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

type creationValidateArgs struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type creationReadArgs struct {
	ID   string `json:"id"`
	File string `json:"file"`
}

type creationWriteArgs struct {
	ID      string `json:"id"`
	File    string `json:"file"`
	Content string `json:"content"`
}

type creationApprovalArgs struct {
	ID         string `json:"id"`
	RequestID  string `json:"requestId"`
	DecisionID string `json:"decisionId"`
}

// --- catalog --------------------------------------------------------------

// catalog is the read-only aggregate of the machine's installed plugins. It is
// deliberately a projection of the manager's inventory (identity, lifecycle,
// declared surface, live tools), not a second registry. The extension fields
// (slots, contributions, extensibility) come from the same view the UI syncs
// from, so the catalog cannot drift from what is assembled.
func (c *creationPlugin) catalog(ctx context.Context, _ string, _ json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	manager := c.manager()
	if manager == nil {
		return creationResult(failureDocument("the plugin manager is not wired in this deployment", nil), true), nil
	}
	if failure := c.tenantFailure(ctx, manager); failure != nil {
		return creationResult(*failure, true), nil
	}
	inventory := manager.Snapshot()
	plugins := make([]map[string]any, 0, len(inventory.Plugins))
	for _, plugin := range inventory.Plugins {
		entry := map[string]any{
			"id":          plugin.ID,
			"version":     plugin.Version,
			"kind":        plugin.Kind,
			"state":       string(plugin.State),
			"bundled":     plugin.Bundled,
			"provides":    plugin.Provides,
			"permissions": plugin.Permissions,
		}
		if plugin.Description != "" {
			entry["description"] = plugin.Description
		}
		if len(plugin.Tools) > 0 {
			entry["tools"] = plugin.Tools
		}
		if len(plugin.Requires) > 0 {
			entry["requires"] = plugin.Requires
		}
		if plugin.Layer != "" {
			entry["layer"] = plugin.Layer
		}
		if len(plugin.Slots) > 0 {
			entry["slots"] = plugin.Slots
		}
		if len(plugin.Contributes) > 0 {
			entry["contributes"] = plugin.Contributes
		}
		if plugin.Extensibility != nil {
			entry["extensibility"] = plugin.Extensibility
		}
		plugins = append(plugins, entry)
	}
	document := map[string]any{
		"tenant":     manager.TenantID().String(),
		"plugins":    plugins,
		"enabled":    inventory.Enabled,
		"tools":      inventory.Tools,
		"revision":   inventory.Revision,
		"generation": inventory.Generation,
	}
	return creationResult(document, false), nil
}

// tenantFailure refuses a creation action whose ambient tenant is not the
// manager's. A tool call inside a turn always names a tenant; a bridge-style
// call may not, in which case the manager's tenant is the deployment's one
// tenant. There is no cross-tenant creation path.
func (c *creationPlugin) tenantFailure(ctx context.Context, manager *Manager) *map[string]any {
	tenant, ok := c.tenant(ctx)
	if !ok {
		document := failureDocument("creation mode needs a tenant: call it inside a turn, or configure the deployment tenant", nil)
		return &document
	}
	if tenant != manager.TenantID() {
		document := failureDocument("creation mode belongs to another tenant; this process serves exactly one tenant", nil)
		return &document
	}
	return nil
}

// --- scaffold -------------------------------------------------------------

func (c *creationPlugin) scaffold(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var args creationScaffoldArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return creationResult(failureDocument("invalid arguments: "+err.Error(), nil), true), nil
	}
	manager := c.manager()
	if manager == nil {
		return creationResult(failureDocument("the plugin manager is not wired in this deployment", nil), true), nil
	}
	if failure := c.tenantFailure(ctx, manager); failure != nil {
		return creationResult(*failure, true), nil
	}
	root := manager.Root()
	if root == "" {
		return creationResult(failureDocument("this deployment has no plugins root, so there is nowhere to scaffold a package", nil), true), nil
	}
	id := strings.TrimSpace(args.ID)
	purpose := strings.TrimSpace(args.Purpose)
	if managementID := id; !pluginpkg.ValidatePluginID(managementID) {
		return creationResult(fieldFailure("id", "id must match ^[a-z][a-z0-9._-]{1,63}$"), true), nil
	}
	if purpose == "" {
		return creationResult(fieldFailure("purpose", "purpose is required (one line)"), true), nil
	}
	if len([]rune(purpose)) > 1024 || strings.ContainsAny(purpose, "\r\n") {
		return creationResult(fieldFailure("purpose", "purpose must be one line of at most 1024 characters"), true), nil
	}
	if kind := strings.TrimSpace(args.Kind); kind != "" && kind != string(pluginpkg.KindWebview) {
		return creationResult(fieldFailure("kind", "the shipped template is kind webview; a sidecar package is written by editing the staged package's entry"), true), nil
	}
	if view, installed := pluginViewOf(manager, id); installed {
		return creationResult(failureDocument(fmt.Sprintf("plugin %s is already installed (%s); remove it first or scaffold under another id", id, view.State), nil), true), nil
	}
	tool := strings.TrimSpace(args.Tool)
	if tool == "" {
		tool = creationDefaultTool
	}
	version := strings.TrimSpace(args.Version)
	if version == "" {
		version = creationDefaultVersion
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		title = purpose
	}
	if len([]rune(title)) > 128 {
		title = string([]rune(title)[:128])
	}

	vars := templates.Vars{
		ID:            id,
		Title:         title,
		Description:   purpose,
		Tool:          tool,
		Version:       version,
		PublishedTool: pluginpkg.NamespacedPublishedToolName(id, tool),
	}
	files, err := templates.Render(c.template, vars)
	if err != nil {
		return creationResult(failureDocument(err.Error(), nil), true), nil
	}

	stagingRoot := filepath.Join(root, CreationStagingDir)
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return creationResult(failureDocument("create the staging directory: "+err.Error(), nil), true), nil
	}
	// Render into a temporary sibling first: the package only appears at its
	// staging path once the real validator accepts it, so a failure leaves
	// nothing behind (spec: 生成失败可回滚).
	temporary, err := os.MkdirTemp(stagingRoot, ".tmp-"+id+"-")
	if err != nil {
		return creationResult(failureDocument("create the temporary staging directory: "+err.Error(), nil), true), nil
	}
	cleanup := func() {
		if err := os.RemoveAll(temporary); err != nil {
			c.logger().Warn("creation: remove the temporary package failed", "plugin", id, "dir", temporary, "err", err)
		}
	}
	for _, file := range files {
		target := filepath.Join(temporary, filepath.FromSlash(file.Path))
		if !pluginpkg.IsInside(temporary, target) {
			cleanup()
			return creationResult(failureDocument("template file escapes the package: "+file.Path, nil), true), nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			cleanup()
			return creationResult(failureDocument("create "+file.Path+": "+err.Error(), nil), true), nil
		}
		if err := os.WriteFile(target, file.Data, 0o644); err != nil {
			cleanup()
			return creationResult(failureDocument("write "+file.Path+": "+err.Error(), nil), true), nil
		}
	}
	pkg := pluginpkg.LoadDir(temporary, manager.environment())
	if !pkg.Usable() {
		cleanup()
		fields := problemFields(pkg.Problem)
		document := failureDocument(problemReason(pkg), fields)
		document["status"] = "invalid"
		c.audit(ctx, manager, id, version, "creation.scaffold", store.PluginResultFailed,
			map[string]any{"reason": problemReason(pkg), "fields": fields})
		return creationResult(document, true), nil
	}
	destination := c.stagingDir(root, id)
	if err := os.RemoveAll(destination); err != nil {
		cleanup()
		return creationResult(failureDocument("replace the previous staged package: "+err.Error(), nil), true), nil
	}
	if err := os.Rename(temporary, destination); err != nil {
		cleanup()
		return creationResult(failureDocument("move the package into the staging area: "+err.Error(), nil), true), nil
	}
	final := pluginpkg.LoadDir(destination, manager.environment())
	if !final.Usable() {
		_ = os.RemoveAll(destination)
		return creationResult(failureDocument(problemReason(final), problemFields(final.Problem)), true), nil
	}
	document := map[string]any{
		"status":        "scaffolded",
		"id":            id,
		"version":       final.Version(),
		"dir":           destination,
		"hash":          final.Hash,
		"template":      c.template,
		"title":         title,
		"tool":          tool,
		"publishedTool": pluginpkg.NamespacedPublishedToolName(id, tool),
		"files":         templatePaths(files),
		"next":          "creation validate → edit if needed → creation install",
	}
	c.audit(ctx, manager, id, final.Version(), "creation.scaffold", store.PluginResultOK,
		map[string]any{"dir": destination, "hash": final.Hash, "template": c.template, "files": templatePaths(files)})
	return creationResult(document, false), nil
}

// stagingDir is the staged package path of one id.
func (c *creationPlugin) stagingDir(root, id string) string {
	return filepath.Join(root, CreationStagingDir, id)
}

// --- read / write ---------------------------------------------------------

// creationMaxFileBytes bounds one read or write through the creation tool. A
// generated package is source code and manifests; 512 KiB is far above any
// template file and keeps a mistaken binary write out of the staging tree.
const creationMaxFileBytes = 512 << 10

// creationMaxListFiles bounds the listing a read without a file returns.
const creationMaxListFiles = 200

func (c *creationPlugin) read(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var args creationReadArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return creationResult(failureDocument("invalid arguments: "+err.Error(), nil), true), nil
	}
	manager := c.manager()
	if manager == nil {
		return creationResult(failureDocument("the plugin manager is not wired in this deployment", nil), true), nil
	}
	if failure := c.tenantFailure(ctx, manager); failure != nil {
		return creationResult(*failure, true), nil
	}
	id := strings.TrimSpace(args.ID)
	dir, failure := c.resolveStaged(manager, id, "")
	if failure != nil {
		return creationResult(*failure, true), nil
	}
	name := strings.TrimSpace(args.File)
	if name == "" {
		files := make([]map[string]any, 0, 16)
		walkErr := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if len(files) >= creationMaxListFiles {
				return filepath.SkipAll
			}
			relative, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			files = append(files, map[string]any{
				"file":  filepath.ToSlash(relative),
				"bytes": info.Size(),
			})
			return nil
		})
		if walkErr != nil {
			return creationResult(failureDocument("list the staged package: "+walkErr.Error(), nil), true), nil
		}
		return creationResult(map[string]any{
			"status": "ok",
			"id":     id,
			"dir":    dir,
			"files":  files,
		}, false), nil
	}
	target, err := creationFileTarget(dir, name, false)
	if err != nil {
		return creationResult(fieldFailure("file", err.Error()), true), nil
	}
	if info, statErr := os.Lstat(target); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return creationResult(fieldFailure("file", "refusing to read through a symlink"), true), nil
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return creationResult(failureDocument("read "+name+": "+err.Error(), nil), true), nil
	}
	if len(data) > creationMaxFileBytes {
		return creationResult(fieldFailure("file", fmt.Sprintf("file is %d bytes; the read limit is %d", len(data), creationMaxFileBytes)), true), nil
	}
	return creationResult(map[string]any{
		"status":  "ok",
		"id":      id,
		"file":    filepath.ToSlash(name),
		"content": string(data),
	}, false), nil
}

func (c *creationPlugin) write(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var args creationWriteArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return creationResult(failureDocument("invalid arguments: "+err.Error(), nil), true), nil
	}
	manager := c.manager()
	if manager == nil {
		return creationResult(failureDocument("the plugin manager is not wired in this deployment", nil), true), nil
	}
	if failure := c.tenantFailure(ctx, manager); failure != nil {
		return creationResult(*failure, true), nil
	}
	id := strings.TrimSpace(args.ID)
	dir, failure := c.resolveStaged(manager, id, "")
	if failure != nil {
		return creationResult(*failure, true), nil
	}
	name := strings.TrimSpace(args.File)
	if name == "" {
		return creationResult(fieldFailure("file", "file is required (a path relative to the staged package)"), true), nil
	}
	if len(args.Content) > creationMaxFileBytes {
		return creationResult(fieldFailure("content", fmt.Sprintf("content is %d bytes; the write limit is %d", len(args.Content), creationMaxFileBytes)), true), nil
	}
	target, err := creationFileTarget(dir, name, true)
	if err != nil {
		return creationResult(fieldFailure("file", err.Error()), true), nil
	}
	if info, statErr := os.Lstat(target); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return creationResult(fieldFailure("file", "refusing to write through a symlink"), true), nil
	}
	if err := os.WriteFile(target, []byte(args.Content), 0o644); err != nil {
		return creationResult(failureDocument("write "+name+": "+err.Error(), nil), true), nil
	}
	c.audit(ctx, manager, id, "", "creation.write", store.PluginResultOK,
		map[string]any{"file": filepath.ToSlash(name), "bytes": len(args.Content)})
	return creationResult(map[string]any{
		"status": "written",
		"id":     id,
		"file":   filepath.ToSlash(name),
		"bytes":  len(args.Content),
		"next":   "creation {action:\"validate\", id} re-checks the package before install",
	}, false), nil
}

// creationFileTarget resolves a package-relative file inside the staging
// directory. It refuses absolute paths, traversal and (for writes) parent
// directories that resolve outside the package through a symlink.
func creationFileTarget(dir, name string, createParent bool) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(name)))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path must stay inside the staged package")
	}
	target := filepath.Join(dir, clean)
	if !pluginpkg.IsInside(dir, target) || filepath.Clean(target) == filepath.Clean(dir) {
		return "", errors.New("path must stay inside the staged package")
	}
	parent := filepath.Dir(target)
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve the staged package: %w", err)
	}
	// Check the deepest existing ancestor before creating anything: walking
	// through a symlinked subdirectory must not create directories outside the
	// package first and refuse afterwards.
	ancestor := parent
	for {
		if _, statErr := os.Lstat(ancestor); statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(ancestor)
			if resolveErr != nil {
				return "", fmt.Errorf("resolve the target directory: %w", resolveErr)
			}
			if filepath.Clean(resolved) != filepath.Clean(resolvedDir) && !pluginpkg.IsInside(resolvedDir, resolved) {
				return "", errors.New("the target directory resolves outside the staged package")
			}
			break
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			break
		}
		ancestor = next
	}
	if createParent {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return "", fmt.Errorf("create the parent directory: %w", err)
		}
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve the target directory: %w", err)
	}
	if filepath.Clean(resolvedParent) != filepath.Clean(resolvedDir) && !pluginpkg.IsInside(resolvedDir, resolvedParent) {
		return "", errors.New("the target directory resolves outside the staged package")
	}
	return target, nil
}

// --- validate -------------------------------------------------------------

func (c *creationPlugin) validate(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var args creationValidateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return creationResult(failureDocument("invalid arguments: "+err.Error(), nil), true), nil
	}
	manager := c.manager()
	if manager == nil {
		return creationResult(failureDocument("the plugin manager is not wired in this deployment", nil), true), nil
	}
	if failure := c.tenantFailure(ctx, manager); failure != nil {
		return creationResult(*failure, true), nil
	}
	id := strings.TrimSpace(args.ID)
	dir, failure := c.resolveStaged(manager, id, args.Path)
	if failure != nil {
		detail := map[string]any{"stage": "resolve"}
		if reason, _ := (*failure)["error"].(string); reason != "" {
			detail["reason"] = reason
		}
		c.audit(ctx, manager, id, "", "creation.validate", store.PluginResultFailed, detail)
		return creationResult(*failure, true), nil
	}
	document := c.validationDocument(manager, id, dir)
	ok, _ := document["ok"].(bool)
	result := store.PluginResultFailed
	detail := map[string]any{
		"status": document["status"],
		"hash":   document["hash"],
		"fields": auditFieldPaths(document["problems"]),
	}
	if ok {
		result = store.PluginResultOK
	}
	c.audit(ctx, manager, id, fmt.Sprint(document["version"]), "creation.validate", result, detail)
	return creationResult(document, !ok), nil
}

// auditFieldPaths keeps only the field paths of a validation problem set: the
// audit names where a package failed, never its body.
func auditFieldPaths(value any) []string {
	out := []string{}
	switch fields := value.(type) {
	case []pluginpkg.FieldError:
		for _, field := range fields {
			if field.Field != "" {
				out = append(out, field.Field)
			}
		}
	case []any:
		for _, raw := range fields {
			entry, _ := raw.(map[string]any)
			if field, _ := entry["field"].(string); field != "" {
				out = append(out, field)
			}
		}
	}
	return out
}

// validationDocument runs the real validator plus the structural checks a
// scaffolded package must pass, and reports every problem as data.
func (c *creationPlugin) validationDocument(manager *Manager, id, dir string) map[string]any {
	pkg := pluginpkg.LoadDir(dir, manager.environment())
	document := map[string]any{
		"id":      id,
		"dir":     dir,
		"ok":      false,
		"version": pkg.Version(),
		"hash":    pkg.Hash,
		"status":  "valid",
	}
	var problems []pluginpkg.FieldError
	var warnings []string
	if !pkg.Usable() {
		document["status"] = "invalid"
		document["reason"] = problemReason(pkg)
		problems = append(problems, problemFields(pkg.Problem)...)
		document["fields"] = problems
		if len(problems) == 0 {
			problems = append(problems, pluginpkg.FieldError{Field: "", Message: problemReason(pkg)})
		}
		document["problems"] = problems
		return document
	}
	// Structural self-checks against the manifest (what the host will look for at
	// load and hosting time), and the host prerequisites the backend needs.
	if pkg.Manifest.Provides.UI != nil {
		entry := filepath.Join(dir, filepath.FromSlash(pkg.Manifest.Provides.UI.Entry))
		if info, err := os.Stat(entry); err != nil || info.IsDir() {
			problems = append(problems, pluginpkg.FieldError{Field: "provides.ui.entry", Message: "the UI entry does not exist in the package"})
		}
	}
	for index, command := range pkg.Manifest.Entry.Command {
		if !strings.ContainsRune(command, '/') {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(command))
		if info, err := os.Stat(target); err != nil || info.IsDir() {
			problems = append(problems, pluginpkg.FieldError{
				Field:   fmt.Sprintf("entry.command[%d]", index),
				Message: "the backend file does not exist in the package",
			})
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		warnings = append(warnings, "README.md is missing: the package cannot explain itself to the next reader")
	}
	if len(pkg.Manifest.Entry.Command) > 0 {
		head := pkg.Manifest.Entry.Command[0]
		if !strings.ContainsRune(head, '/') {
			if _, err := exec.LookPath(head); err != nil {
				warnings = append(warnings, fmt.Sprintf("the backend command %q is not on PATH in this host: the plugin will not load", head))
			}
		}
	}
	if len(problems) == 0 {
		document["ok"] = true
	}
	document["problems"] = problems
	document["warnings"] = warnings
	document["publishedTools"] = pkg.Manifest.PublishedTools()
	return document
}

// --- install --------------------------------------------------------------

func (c *creationPlugin) install(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var args creationApprovalArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return creationResult(failureDocument("invalid arguments: "+err.Error(), nil), true), nil
	}
	manager := c.manager()
	if manager == nil {
		return creationResult(failureDocument("the plugin manager is not wired in this deployment", nil), true), nil
	}
	if failure := c.tenantFailure(ctx, manager); failure != nil {
		return creationResult(*failure, true), nil
	}
	id := strings.TrimSpace(args.ID)
	dir, failure := c.resolveStaged(manager, id, "")
	if failure != nil {
		return creationResult(*failure, true), nil
	}
	document := c.validationDocument(manager, id, dir)
	if ok, _ := document["ok"].(bool); !ok {
		document["status"] = "invalid"
		c.audit(ctx, manager, id, fmt.Sprint(document["version"]), "creation.install", store.PluginResultRefused,
			map[string]any{"reason": "the staged package is invalid", "problems": document["problems"]})
		return creationResult(document, true), nil
	}
	pkg := pluginpkg.LoadDir(dir, manager.environment())
	tenant, ok := c.tenant(ctx)
	if !ok {
		return creationResult(failureDocument("creation mode needs a tenant: call it inside a turn, or configure the deployment tenant", nil), true), nil
	}
	// Idempotent replay: the same package (id + version + content hash) is already
	// installed and ENABLED. A repeated call — including a retry with the same
	// requestId — returns the same view and writes no second state. A disabled or
	// otherwise inactive plugin is NOT enabled here: enabling is a lifecycle
	// write that needs its own durable approval.
	if existing, found := pluginViewOf(manager, id); found {
		if existing.ContentHash == pkg.Hash && existing.Version == pkg.Version() {
			if existing.State != LifecycleEnabled {
				c.audit(ctx, manager, id, pkg.Version(), "creation.install", store.PluginResultRefused,
					map[string]any{"reason": "plugin is installed but not enabled", "state": string(existing.State)})
				document := failureDocument(fmt.Sprintf(
					"plugin %s %s is %s; enabling is a separate lifecycle write: call POST /v1/plugins/%s/enable with its own durable approval",
					id, existing.Version, existing.State, id), nil)
				document["status"] = "disabled"
				document["action"] = "enable"
				return creationResult(document, true), nil
			}
			c.audit(ctx, manager, id, pkg.Version(), "creation.install", store.PluginResultOK,
				map[string]any{"idempotent": true, "hash": pkg.Hash, "decisionId": args.DecisionID})
			return creationResult(installationDocument(existing, args.DecisionID, true), false), nil
		}
		c.audit(ctx, manager, id, pkg.Version(), "creation.install", store.PluginResultRefused,
			map[string]any{"reason": "a different package is already installed", "installedHash": existing.ContentHash, "hash": pkg.Hash})
		return creationResult(failureDocument(fmt.Sprintf(
			"plugin %s %s is already installed with a different package; remove it first or change its version", id, existing.Version), nil), true), nil
	}
	approved, approval, err := c.approve(ctx, tenant, store.PluginActionInstall, id,
		"content="+pkg.Hash+":version="+pkg.Version(), installationPrompt(pkg, dir), args)
	if err != nil {
		return creationResult(failureDocument(err.Error(), nil), true), nil
	}
	if !approved {
		if state, _ := approval["status"].(string); state == "refused" {
			c.audit(ctx, manager, id, pkg.Version(), "creation.install", store.PluginResultRefused,
				map[string]any{"reason": approval["reason"], "decisionId": approval["decisionId"]})
		}
		return creationResult(approval, stateIsError(approval)), nil
	}
	audit := AuditContext{TenantID: &tenant,
		Actor: approvalActor("plugin:"+CreationPluginID+"/install", approval)}
	view, err := manager.Install(ctx, InstallRequest{Path: dir, ExpectedHash: pkg.Hash}, audit)
	if err != nil {
		c.cleanupStaged(manager, id)
		c.audit(ctx, manager, id, pkg.Version(), "creation.install", store.PluginResultFailed,
			map[string]any{"reason": err.Error(), "stagingRemoved": true})
		return creationResult(failureDocument(err.Error(), pluginpkg.FieldErrors(err)), true), nil
	}
	if _, err := manager.Enable(ctx, id, audit); err != nil {
		// Enable leaves the previous assembly untouched; finish the roll back so a
		// failed creation never leaves an installed-but-dead package behind.
		_, _ = manager.Uninstall(ctx, id, audit)
		c.cleanupStaged(manager, id)
		c.audit(ctx, manager, id, pkg.Version(), "creation.install", store.PluginResultFailed,
			map[string]any{"reason": err.Error(), "stagingRemoved": true, "rolledBack": true})
		return creationResult(failureDocument(err.Error(), nil), true), nil
	}
	c.audit(ctx, manager, id, pkg.Version(), "creation.install", store.PluginResultOK,
		map[string]any{"hash": pkg.Hash, "decisionId": args.DecisionID, "staging": c.stagingDir(manager.Root(), id)})
	// The enable view is read from the live inventory once more: it is the same
	// projection GET /v1/plugins serves, so the document names the tools and the
	// UI contributions the model/client will actually see.
	refreshed := view
	if current, found := pluginViewOf(manager, id); found {
		refreshed = current
	}
	return creationResult(installationDocument(refreshed, args.DecisionID, false), false), nil
}

// installationDocument is the success body of creation.install. It carries the
// render metadata from the same view GET /v1/plugins serves, so a client does
// not have to guess where the plugin appears.
func installationDocument(view PluginView, decisionID string, idempotent bool) map[string]any {
	document := map[string]any{
		"status":         "enabled",
		"id":             view.ID,
		"version":        view.Version,
		"bundlePath":     view.Path,
		"publishedTools": view.Tools,
		"contributes":    view.Contributes,
		"slots":          view.Slots,
		"decisionId":     decisionID,
	}
	if idempotent {
		document["idempotent"] = true
	}
	if view.Extensibility != nil {
		document["extensibility"] = view.Extensibility
	}
	if view.Provides.UI != nil && strings.TrimSpace(view.Provides.UI.Entry) != "" {
		document["deepLink"] = "/plugins/" + view.ID + "/"
	}
	for _, contribution := range view.Contributes {
		if contribution.Form == "chunk" && contribution.Entry != "" {
			document["chunkEntry"] = "/plugins/" + view.ID + "/" + strings.TrimPrefix(contribution.Entry, "./")
			break
		}
	}
	return document
}

// installationPrompt is what a human approves: identity, provenance, content
// hash, the permissions the plugin requests and the slots it will contribute.
// It carries names only — never a secret value or a package body.
func installationPrompt(pkg pluginpkg.Package, source string) string {
	manifest := pkg.Manifest
	var permissions []string
	if len(manifest.Permissions.FS) > 0 {
		permissions = append(permissions, "fs="+strings.Join(manifest.Permissions.FS, ","))
	}
	if len(manifest.Permissions.Net) > 0 {
		permissions = append(permissions, "net="+strings.Join(manifest.Permissions.Net, ","))
	}
	if len(manifest.Permissions.Secrets) > 0 {
		permissions = append(permissions, "secrets="+strings.Join(manifest.Permissions.Secrets, ","))
	}
	if len(manifest.Permissions.Tools) > 0 {
		permissions = append(permissions, "tools="+strings.Join(manifest.Permissions.Tools, ","))
	}
	if len(permissions) == 0 {
		permissions = append(permissions, "none")
	}
	var contributions []string
	for _, contribution := range manifest.Contributes {
		contributions = append(contributions, contribution.Slot+"/"+string(contribution.Form))
	}
	if len(contributions) == 0 {
		contributions = append(contributions, "none")
	}
	return fmt.Sprintf(
		"Install the generated plugin %s %s?\nSource: %s\nContent hash: %s\nRequested permissions: %s\nContributed slots: %s\nInstalling adopts its code into this deployment and lets it run.",
		pkg.ID(), pkg.Version(), source, pkg.Hash, strings.Join(permissions, " "), strings.Join(contributions, ", "))
}

// --- list -----------------------------------------------------------------

func (c *creationPlugin) list(ctx context.Context, _ string, _ json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	manager := c.manager()
	if manager == nil {
		return creationResult(failureDocument("the plugin manager is not wired in this deployment", nil), true), nil
	}
	if failure := c.tenantFailure(ctx, manager); failure != nil {
		return creationResult(*failure, true), nil
	}
	root := manager.Root()
	if root == "" {
		return creationResult(map[string]any{"status": "ok", "artifacts": []any{}, "note": "this deployment has no plugins root"}, false), nil
	}
	stagingRoot := filepath.Join(root, CreationStagingDir)
	entries, err := os.ReadDir(stagingRoot)
	if err != nil && !os.IsNotExist(err) {
		return creationResult(failureDocument("read the staging directory: "+err.Error(), nil), true), nil
	}
	artifacts := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		id := entry.Name()
		dir := filepath.Join(stagingRoot, id)
		pkg := pluginpkg.LoadDir(dir, manager.environment())
		artifact := map[string]any{
			"id":      id,
			"dir":     dir,
			"version": pkg.Version(),
			"valid":   pkg.Usable(),
			"hash":    pkg.Hash,
		}
		if !pkg.Usable() {
			artifact["problem"] = problemReason(pkg)
			artifact["fields"] = problemFields(pkg.Problem)
		}
		if view, installed := pluginViewOf(manager, id); installed {
			artifact["installed"] = true
			artifact["state"] = view.State
			artifact["tools"] = view.Tools
			artifact["slots"] = view.Slots
			artifact["contributes"] = view.Contributes
			if view.Extensibility != nil {
				artifact["extensibility"] = view.Extensibility
			}
			if view.Provides.UI != nil && strings.TrimSpace(view.Provides.UI.Entry) != "" {
				artifact["deepLink"] = "/plugins/" + id + "/"
			}
			for _, contribution := range view.Contributes {
				if contribution.Form == "chunk" && contribution.Entry != "" {
					artifact["chunkEntry"] = "/plugins/" + id + "/" + strings.TrimPrefix(contribution.Entry, "./")
					break
				}
			}
		} else {
			artifact["installed"] = false
		}
		artifacts = append(artifacts, artifact)
	}
	sort.Slice(artifacts, func(i, j int) bool { return fmt.Sprint(artifacts[i]["id"]) < fmt.Sprint(artifacts[j]["id"]) })
	return creationResult(map[string]any{"status": "ok", "root": stagingRoot, "artifacts": artifacts, "count": len(artifacts)}, false), nil
}

// --- remove ---------------------------------------------------------------

func (c *creationPlugin) remove(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var args creationApprovalArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return creationResult(failureDocument("invalid arguments: "+err.Error(), nil), true), nil
	}
	manager := c.manager()
	if manager == nil {
		return creationResult(failureDocument("the plugin manager is not wired in this deployment", nil), true), nil
	}
	if failure := c.tenantFailure(ctx, manager); failure != nil {
		return creationResult(*failure, true), nil
	}
	id := strings.TrimSpace(args.ID)
	if !pluginpkg.ValidatePluginID(id) {
		return creationResult(fieldFailure("id", "id must match ^[a-z][a-z0-9._-]{1,63}$"), true), nil
	}
	view, installed := pluginViewOf(manager, id)
	staged := ""
	if manager.Root() != "" {
		if dir := c.stagingDir(manager.Root(), id); dir != "" {
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				staged = dir
			}
		}
	}
	if !installed && staged == "" {
		return creationResult(map[string]any{"status": "absent", "id": id, "note": "nothing is installed or staged under this id"}, false), nil
	}
	version := ""
	if installed {
		version = view.Version
	}
	// A bundled plugin is not a creation artifact: refuse before any lifecycle
	// write. Disabling it first would leave the built-in plugin off even though
	// the uninstall could never succeed.
	if installed && view.Bundled {
		c.audit(ctx, manager, id, version, "creation.remove", store.PluginResultRefused,
			map[string]any{"reason": "bundled plugin cannot be removed"})
		return creationResult(failureDocument(
			"plugin "+id+" is bundled with the binary and cannot be removed; disable it through the plugin lifecycle if it must be off", nil), true), nil
	}
	tenant, ok := c.tenant(ctx)
	if !ok {
		return creationResult(failureDocument("creation mode needs a tenant: call it inside a turn, or configure the deployment tenant", nil), true), nil
	}
	binding := "version=" + version
	if installed && view.ContentHash != "" {
		binding += ":content=" + view.ContentHash
	}
	approved, approval, err := c.approve(ctx, tenant, store.PluginActionUninstall, id, binding,
		fmt.Sprintf("Remove the generated plugin %s%s? The plugin is uninstalled (if installed) and its staged package is deleted.", id, versionSuffix(version)),
		args)
	if err != nil {
		return creationResult(failureDocument(err.Error(), nil), true), nil
	}
	if !approved {
		if state, _ := approval["status"].(string); state == "refused" {
			c.audit(ctx, manager, id, version, "creation.remove", store.PluginResultRefused,
				map[string]any{"reason": approval["reason"], "decisionId": approval["decisionId"]})
		}
		return creationResult(approval, stateIsError(approval)), nil
	}
	// The approval was bound to the installed version/hash; an update between
	// the question and the write must not delete the package nobody approved.
	if installed {
		if current, still := pluginViewOf(manager, id); !still ||
			current.Version != version || current.ContentHash != view.ContentHash {
			c.audit(ctx, manager, id, version, "creation.remove", store.PluginResultRefused,
				map[string]any{"reason": "plugin changed after approval"})
			return creationResult(failureDocument(
				"the plugin changed after approval; ask again with the new version/hash", nil), true), nil
		}
	}
	audit := AuditContext{TenantID: &tenant,
		Actor: approvalActor("plugin:"+CreationPluginID+"/remove", approval)}
	uninstalled := false
	if installed {
		if view.State == LifecycleEnabled {
			if _, err := manager.Disable(ctx, id, audit); err != nil {
				c.audit(ctx, manager, id, version, "creation.remove", store.PluginResultFailed,
					map[string]any{"stage": "disable", "reason": err.Error()})
				return creationResult(failureDocument("disable before removal: "+err.Error(), nil), true), nil
			}
		}
		if _, err := manager.Uninstall(ctx, id, audit); err != nil {
			c.audit(ctx, manager, id, version, "creation.remove", store.PluginResultFailed,
				map[string]any{"stage": "uninstall", "reason": err.Error()})
			return creationResult(failureDocument("uninstall: "+err.Error(), nil), true), nil
		}
		uninstalled = true
	}
	stagingRemoved := false
	if staged != "" {
		if err := os.RemoveAll(staged); err != nil {
			c.audit(ctx, manager, id, version, "creation.remove", store.PluginResultFailed,
				map[string]any{"stage": "staging", "reason": err.Error()})
			return creationResult(failureDocument("delete the staged package: "+err.Error(), nil), true), nil
		}
		stagingRemoved = true
	}
	c.audit(ctx, manager, id, version, "creation.remove", store.PluginResultOK,
		map[string]any{"uninstalled": uninstalled, "stagingRemoved": stagingRemoved, "decisionId": args.DecisionID})
	return creationResult(map[string]any{
		"status":         "removed",
		"id":             id,
		"uninstalled":    uninstalled,
		"stagingRemoved": stagingRemoved,
	}, false), nil
}

// --- approval -------------------------------------------------------------

// approvalDocument is the answer while a question is not answered yet (or was
// refused): the client answers at answerUrl, then repeats the call with
// decisionId.
func (c *creationPlugin) approve(ctx context.Context, tenant uuid.UUID, action, id, binding, prompt string, args creationApprovalArgs) (bool, map[string]any, error) {
	broker := c.decisions()
	if broker == nil {
		return false, nil, fmt.Errorf("creation %s requires the durable approval primitive, but decisions are not wired in this deployment", action)
	}
	session := PluginApprovalSessionID(tenant)
	answerInSession := false
	if scope, ok := RunScopeFromContext(ctx); ok && scope.SessionID != uuid.Nil && scope.TenantID == tenant {
		session = scope.SessionID
		answerInSession = true
	}
	requestID := "creation." + action + ":" + id
	if binding = strings.TrimSpace(binding); binding != "" {
		requestID += ":" + binding
	}
	if key := strings.TrimSpace(args.RequestID); key != "" {
		if len(key) > 128 {
			return false, nil, errors.New("requestId is too long")
		}
		requestID += ":" + key
	}
	expected := DecisionIDFor(tenant, session, requestID)
	answerURL := func(decisionID string) string {
		if answerInSession {
			return "/v1/sessions/" + session.String() + "/decisions/" + decisionID
		}
		return PluginApprovalAnswerURL(decisionID)
	}
	given := strings.TrimSpace(args.DecisionID)
	kind := "plugin." + action
	options := []DecisionOption{
		{ID: creationAllowOption, Label: "Approve"},
		{ID: creationDenyOption, Label: "Deny"},
	}
	if given == "" {
		decision, _, err := broker.Open(ctx, OpenDecisionRequest{
			TenantID:  tenant,
			SessionID: session,
			RequestID: requestID,
			Kind:      kind,
			Prompt:    prompt,
			Options:   options,
		})
		if err != nil {
			return false, nil, err
		}
		if !creationApprovalMatches(decision, kind, requestID, prompt, options) {
			return false, nil, errors.New("approval decision does not belong to this request")
		}
		return false, approvalDocument("approval_required", decision, answerURL(decision.DecisionID), id, action), nil
	}
	parsed, err := uuid.Parse(given)
	if err != nil {
		return false, nil, errors.New("decisionId must be a UUID")
	}
	if parsed != expected {
		return false, nil, errors.New("decisionId does not authorize this request: repeat the call with the same requestId it was asked with (or with no requestId at all)")
	}
	decision, found := broker.Get(ctx, tenant, session, expected)
	if !found {
		return false, nil, errors.New("approval decision not found")
	}
	// The id is derived from (tenant, session, requestId), so an id match alone
	// is not enough: a client with session.write can open its own decision under
	// the derived requestId. Only a row that this action opened (kind, prompt and
	// menu all match) may authorize the lifecycle write.
	if !creationApprovalMatches(decision, kind, requestID, prompt, options) {
		return false, nil, errors.New("approval decision does not belong to this request")
	}
	switch {
	case decision.Status == DecisionOpen:
		return false, approvalDocument("pending", decision, answerURL(decision.DecisionID), id, action), nil
	case decision.Status == DecisionAnswered && decision.Answer != nil && decision.Answer.OptionID == creationAllowOption:
		return true, approvalDocument("approved", decision, answerURL(decision.DecisionID), id, action), nil
	default:
		reason := "the approval decision is " + string(decision.Status)
		if decision.Answer != nil && decision.Answer.OptionID != "" {
			reason = "the approver answered " + decision.Answer.OptionID
		}
		document := approvalDocument("refused", decision, answerURL(decision.DecisionID), id, action)
		document["reason"] = reason
		document["refused"] = true
		return false, document, nil
	}
}

// creationApprovalMatches pins a decision row to the exact question this
// lifecycle action asked: same kind, derived requestId, prompt and menu. The
// decision id alone is derived from client-influenced fields, so it cannot be
// the whole authorization fact.
func creationApprovalMatches(decision Decision, kind, requestID, prompt string, options []DecisionOption) bool {
	return decision.Kind == kind &&
		decision.RequestID == requestID &&
		decision.Prompt == prompt &&
		sameDecisionOptions(decision.Options, options)
}

// approvalDocument renders the not-yet-authorized answer.
func approvalDocument(status string, decision Decision, answerURL, id, action string) map[string]any {
	document := map[string]any{
		"status":           status,
		"id":               id,
		"action":           action,
		"approvalRequired": status == "approval_required" || status == "pending",
		"decisionId":       decision.DecisionID,
		"decision":         decision,
		"answerUrl":        answerURL,
		"next":             "answer the question at answerUrl, then repeat the call with the same decisionId",
	}
	if decision.Answer != nil && decision.Answer.AnsweredBy != "" {
		document["answeredBy"] = decision.Answer.AnsweredBy
		document["answeredByKind"] = decision.Answer.AnsweredByKind
	}
	return document
}

// approvalActor is the audit actor of an approved lifecycle write: the recorded
// approver when the decision names one, otherwise the plugin flow itself.
func approvalActor(fallback string, approval map[string]any) string {
	if answeredBy, _ := approval["answeredBy"].(string); answeredBy != "" {
		actor := "approver:" + answeredBy
		if len(actor) > 128 {
			actor = actor[:128]
		}
		return actor
	}
	return fallback
}

// stateIsError marks approval_required/pending as non-errors (the flow is on
// track, the human has not answered yet) and a refusal as a failure.
func stateIsError(document map[string]any) bool {
	status, _ := document["status"].(string)
	return status == "refused"
}

// PluginApprovalAnswerURL is where a tenant-scoped plugin approval is answered.
// It is the same route internal/harnessapi serves (/v1/plugin-approvals), which is
// why the creation tools and the settings page share one answer surface.
func PluginApprovalAnswerURL(decisionID string) string {
	return "/v1/plugin-approvals/" + decisionID
}

// PluginApprovalSessionID is the tenant-scoped decision session plugin approvals
// live in. It is derived, never random: a restarted process resolves the same
// namespace and finds the question it asked before the crash. internal/harnessapi
// derives the same value for the /v1/plugin-approvals routes; keeping the
// derivation here as well is what lets an in-process tool open a question the
// API can answer.
func PluginApprovalSessionID(tenantID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(creationApprovalNamespace+"|"+tenantID.String()))
}

// tenant resolves who the call acts for: the ambient run's tenant, or the
// deployment tenant when the call arrived through the plugin bridge (which has a
// tenant but no session).
func (c *creationPlugin) tenant(ctx context.Context) (uuid.UUID, bool) {
	if scope, ok := RunScopeFromContext(ctx); ok && scope.TenantID != uuid.Nil {
		return scope.TenantID, true
	}
	if c.opts.TenantID != uuid.Nil {
		return c.opts.TenantID, true
	}
	return uuid.Nil, false
}

// --- filesystem helpers ---------------------------------------------------

// resolveStaged resolves a staged package directory: an explicit path must stay
// inside the deployment's staging root (the tool is for generated artifacts, not
// for arbitrary directories), the default is `<root>/.creation/<id>`.
func (c *creationPlugin) resolveStaged(manager *Manager, id, explicit string) (string, *map[string]any) {
	if !pluginpkg.ValidatePluginID(id) {
		document := fieldFailure("id", "id must match ^[a-z][a-z0-9._-]{1,63}$")
		return "", &document
	}
	root := manager.Root()
	if root == "" {
		document := failureDocument("this deployment has no plugins root", nil)
		return "", &document
	}
	stagingRoot := filepath.Join(root, CreationStagingDir)
	dir := strings.TrimSpace(explicit)
	if dir == "" {
		dir = c.stagingDir(root, id)
	} else {
		absolute, err := filepath.Abs(dir)
		if err != nil {
			document := failureDocument("resolve path: "+err.Error(), nil)
			return "", &document
		}
		dir = absolute
		if !pluginpkg.IsInside(stagingRoot, dir) || filepath.Clean(dir) == filepath.Clean(stagingRoot) {
			document := failureDocument("path must stay inside the creation staging directory ("+stagingRoot+")", nil)
			return "", &document
		}
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		document := failureDocument("no staged package at "+dir+": run creation scaffold first", nil)
		return "", &document
	}
	return dir, nil
}

// cleanupStaged removes a staged package after a failed install.
func (c *creationPlugin) cleanupStaged(manager *Manager, id string) {
	root := manager.Root()
	if root == "" {
		return
	}
	if err := os.RemoveAll(c.stagingDir(root, id)); err != nil {
		c.logger().Warn("creation: remove the staged package failed", "plugin", id, "err", err)
	}
}

// audit writes one row for a creation-specific outcome. The manager writes the
// lifecycle rows of an install/enable/uninstall itself; these rows cover what it
// never sees (a scaffold, a validation refusal, a refusal at the approval gate,
// a rollback).
func (c *creationPlugin) audit(ctx context.Context, manager *Manager, id, version, action, result string, detail map[string]any) {
	manager.RecordAudit(ctx, AuditContext{Actor: "plugin:" + CreationPluginID}, id, version, action, result, detail)
}

// --- small helpers --------------------------------------------------------

// pluginViewOf finds an id in the live inventory.
func pluginViewOf(manager *Manager, id string) (PluginView, bool) {
	for _, view := range manager.Snapshot().Plugins {
		if view.ID == id {
			return view, true
		}
	}
	return PluginView{}, false
}

func problemReason(pkg pluginpkg.Package) string {
	if pkg.Problem == nil {
		return "the package is not usable"
	}
	return pkg.Problem.Reason
}

func versionSuffix(version string) string {
	if version == "" {
		return ""
	}
	return " " + version
}

func templatePaths(files []templates.File) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.Path)
	}
	sort.Strings(out)
	return out
}

// failureDocument is the body of a failed creation call.
func failureDocument(reason string, fields []pluginpkg.FieldError) map[string]any {
	document := map[string]any{"status": "failed", "error": reason}
	if len(fields) > 0 {
		document["fields"] = fields
		document["problems"] = fields
	}
	return document
}

// fieldFailure is a single-field argument failure.
func fieldFailure(field, message string) map[string]any {
	return failureDocument(message, []pluginpkg.FieldError{{Field: field, Message: message}})
}

// creationResult builds a tool result from a structured document: the model reads
// the text, a UI reads structuredContent, and both carry the same facts.
func creationResult(document map[string]any, isError bool) agentcore.AgentToolResult {
	raw, err := json.Marshal(document)
	if err != nil {
		raw = []byte(`{"status":"failed","error":"the result document could not be encoded"}`)
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
