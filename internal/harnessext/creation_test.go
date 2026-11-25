package harnessext

// Creation mode tests (change keel-plugin-core, spec: 创造模式（作为内置插件
// `keel.creation`）).
//
// The chain under test is the real one: the tools are invoked through the
// manager's dispatch point (the same one the model and the plugin bridge use),
// the scaffold renders the real embedded template, the validator is
// pluginpkg.LoadDir (not a looser test double), install/enable go through the
// manager, and every approval is a real decision row answered through the
// DecisionBroker. The negative controls are here by name:
//
//	TestCreationScaffoldRefusesInvalidInput       生成失败不留半包
//	TestCreationInstallRefusesWithoutApproval     拒绝则不生效（且审计 refused）
//	TestCreationPromptSectionIsTheStandard        提示词片段零上游署名、与 spec 一致

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/pluginpkg/builtin"
)

// creationHarness owns the pieces a creation test drives.
type creationHarness struct {
	harness   *managerHarness
	decisions *DecisionBroker
	tenant    uuid.UUID
}

func newCreationHarness(t *testing.T) *creationHarness {
	t.Helper()
	return newCreationHarnessWith(t, true)
}

// newCreationHarnessWith builds the harness; withoutDecisions is the fail-closed
// control (a deployment that cannot ask a durable question).
func newCreationHarnessWith(t *testing.T, withDecisions bool) *creationHarness {
	t.Helper()
	root := t.TempDir()
	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	hooks := NewHookBus(HookBusOptions{Logger: testLogger(), Queue: 64, Workers: 1})
	triggers := NewTriggerEngine(TriggerEngineOptions{Starter: &countingStarter{}, Logger: testLogger()})
	var decisions *DecisionBroker
	if withDecisions {
		decisions = NewDecisionBroker(DecisionBrokerOptions{Logger: testLogger(), Now: func() time.Time {
			return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
		}})
	}
	recorder := &toolRecorder{}
	clock := &testClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	runtime := NewRuntime(RuntimeOptions{
		Hooks:     hooks,
		Triggers:  triggers,
		Decisions: decisions,
		Tools:     recorder.sink,
		Logger:    testLogger(),
		Now:       clock.tick,
	})
	manifest, ok := builtin.Manifest(CreationPluginID)
	if !ok {
		t.Fatalf("no embedded manifest for %s", CreationPluginID)
	}
	if err := manifest.Validate(pluginpkg.Environment{KeelVersion: testKeelVersion(), KnownHookEvent: ValidHookEvent}); err != nil {
		t.Fatalf("the creation manifest does not validate: %v", err)
	}
	var manager *Manager
	creation := Builtin{
		Manifest: manifest,
		Factory: func() *Plugin {
			return NewCreationPlugin(CreationOptions{
				Manager:  func() *Manager { return manager },
				TenantID: tenant,
				Now:      clock.tick,
			})
		},
	}
	loader := &fakeLoader{perVersion: map[string]error{}}
	persistence := newFakePluginStore()
	built, err := NewPluginManager(ManagerOptions{
		TenantID:    tenant,
		Root:        root,
		Runtime:     runtime,
		Persistence: persistence,
		Builtins:    []Builtin{creation},
		Loaders:     map[pluginpkg.Kind]PluginLoader{pluginpkg.KindWebview: loader},
		KeelVersion: testKeelVersion(),
		Logger:      testLogger(),
		Now:         clock.tick,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager = built
	if err := manager.Boot(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	harness := &managerHarness{manager: manager, runtime: runtime, store: persistence, loader: loader, root: root,
		hooks: hooks, tools: recorder, clock: clock}
	t.Cleanup(func() {
		runtime.Close(context.Background())
		triggers.Close()
		if decisions != nil {
			decisions.Close()
		}
		hooks.Close()
	})
	return &creationHarness{harness: harness, decisions: decisions, tenant: tenant}
}

// call invokes one creation tool through the manager's dispatch point and
// returns the structured document plus the tool's isError flag.
func (c *creationHarness) call(t *testing.T, tool string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	return c.callCtx(t, context.Background(), tool, args)
}

// callCtx is call with an explicit ambient run scope (cross-tenant controls).
func (c *creationHarness) callCtx(t *testing.T, ctx context.Context, tool string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.harness.manager.InvokePluginTool(ctx, CreationPluginID, tool, raw)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var envelope struct {
		StructuredContent map[string]any `json:"structuredContent"`
		IsError           bool           `json:"isError"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil {
		t.Fatalf("%s result = %s: %v", tool, result, err)
	}
	if envelope.StructuredContent == nil {
		t.Fatalf("%s returned no structured document: %s", tool, result)
	}
	return envelope.StructuredContent, envelope.IsError
}

func (c *creationHarness) answer(t *testing.T, decisionID, option string) {
	t.Helper()
	parsed, err := uuid.Parse(decisionID)
	if err != nil {
		t.Fatalf("decision id %q: %v", decisionID, err)
	}
	if _, _, err := c.decisions.Answer(context.Background(), AnswerDecisionRequest{
		TenantID:   c.tenant,
		SessionID:  PluginApprovalSessionID(c.tenant),
		DecisionID: parsed,
		OptionID:   option,
	}); err != nil {
		t.Fatalf("answer %s: %v", decisionID, err)
	}
}

func (c *creationHarness) status(t *testing.T, tool string, document map[string]any) string {
	t.Helper()
	status, _ := document["status"].(string)
	if status == "" {
		t.Fatalf("%s document has no status: %+v", tool, document)
	}
	return status
}

// TestCreationScaffoldValidateInstallListRemove drives the whole loop.
func TestCreationCatalogListsInstalledPlugins(t *testing.T) {
	c := newCreationHarness(t)
	if _, err := c.harness.manager.Enable(context.Background(), CreationPluginID, AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("enable creation: %v", err)
	}
	document, isError := c.call(t, CreationToolCatalog, map[string]any{})
	if isError {
		t.Fatalf("catalog failed: %+v", document)
	}
	plugins, ok := document["plugins"].([]any)
	if !ok || len(plugins) == 0 {
		t.Fatalf("catalog plugins = %#v", document["plugins"])
	}
	found := false
	for _, raw := range plugins {
		plugin, _ := raw.(map[string]any)
		if plugin["id"] == CreationPluginID {
			found = true
			if plugin["state"] != "enabled" {
				t.Fatalf("creation state = %v", plugin["state"])
			}
		}
	}
	if !found {
		t.Fatalf("catalog does not list the creation plugin: %+v", plugins)
	}
	if _, ok := document["tools"].([]any); !ok {
		t.Fatalf("catalog tools = %#v", document["tools"])
	}
}

func TestCreationScaffoldValidateInstallListRemove(t *testing.T) {
	c := newCreationHarness(t)
	ctx := context.Background()
	if _, err := c.harness.manager.Enable(ctx, CreationPluginID, AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("enable creation: %v", err)
	}

	// 1. Scaffold: a complete package appears at the staging path, named and
	// validated by the real validator.
	document, isError := c.call(t, CreationToolScaffold, map[string]any{
		"id":      "demo.panel",
		"purpose": "show the session token usage",
		"title":   "Token Panel",
		"tool":    "refresh",
	})
	if isError {
		t.Fatalf("scaffold failed: %+v", document)
	}
	if got := c.status(t, CreationToolScaffold, document); got != "scaffolded" {
		t.Fatalf("scaffold status = %s (%+v)", got, document)
	}
	dir, _ := document["dir"].(string)
	wantDir := filepath.Join(c.harness.root, CreationStagingDir, "demo.panel")
	if filepath.Clean(dir) != filepath.Clean(wantDir) {
		t.Fatalf("scaffold dir = %q, want %q", dir, wantDir)
	}
	if published, _ := document["publishedTool"].(string); published != "demo_panel_refresh" {
		t.Fatalf("publishedTool = %q", published)
	}
	scaffoldHash, _ := document["hash"].(string)
	if scaffoldHash == "" {
		t.Fatal("scaffold returned no content hash")
	}
	for _, name := range []string{"plugin.yaml", "ui/index.html", "ui/app.js", "ui/settings.js",
		"extensibility.json", "EXTENSIBILITY.md", "backend/server.mjs", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Fatalf("scaffolded package is missing %s: %v", name, err)
		}
	}

	// 2. Validate: the real validator, no problems.
	document, isError = c.call(t, CreationToolValidate, map[string]any{"id": "demo.panel"})
	if isError {
		t.Fatalf("validate failed: %+v", document)
	}
	if ok, _ := document["ok"].(bool); !ok {
		t.Fatalf("validate ok = false: %+v", document)
	}
	if fields, _ := document["problems"].([]any); len(fields) != 0 {
		t.Fatalf("validate problems = %+v", fields)
	}

	// 3. Install without a decision opens a durable question.
	document, isError = c.call(t, CreationToolInstall, map[string]any{"id": "demo.panel"})
	if isError {
		t.Fatalf("opening the approval reported an error: %+v", document)
	}
	if got := c.status(t, CreationToolInstall, document); got != "approval_required" {
		t.Fatalf("install status = %s (%+v)", got, document)
	}
	decisionID, _ := document["decisionId"].(string)
	answerURL, _ := document["answerUrl"].(string)
	if decisionID == "" || !strings.HasPrefix(answerURL, "/v1/plugin-approvals/") {
		t.Fatalf("approval document = %+v", document)
	}
	// The approver sees identity, provenance, hash, permissions and contributions.
	decision, _ := document["decision"].(map[string]any)
	promptText, _ := decision["prompt"].(string)
	for _, want := range []string{"demo.panel", scaffoldHash, "tools=refresh", "keel.ui.settings-section/chunk", "Source:"} {
		if !strings.Contains(promptText, want) {
			t.Fatalf("approval prompt does not mention %q:\n%s", want, promptText)
		}
	}

	// 4. Refusal: nothing is installed, and the refusal is audited.
	c.answer(t, decisionID, creationDenyOption)
	document, isError = c.call(t, CreationToolInstall, map[string]any{"id": "demo.panel", "decisionId": decisionID})
	if !isError {
		t.Fatalf("a refused approval did not fail the call: %+v", document)
	}
	if got := c.status(t, CreationToolInstall, document); got != "refused" {
		t.Fatalf("install after refusal status = %s", got)
	}
	if _, installed := pluginViewOf(c.harness.manager, "demo.panel"); installed {
		t.Fatal("the plugin was installed despite a refused approval")
	}
	if !containsAction(c.harness.store.auditActions(t), "creation.install:refused") {
		t.Fatalf("the refusal was not audited: %v", c.harness.store.auditActions(t))
	}

	// 5. A second question (new requestId), approved, installs and enables.
	document, _ = c.call(t, CreationToolInstall, map[string]any{"id": "demo.panel", "requestId": "retry-1"})
	decisionID, _ = document["decisionId"].(string)
	if decisionID == "" {
		t.Fatalf("second question = %+v", document)
	}
	c.answer(t, decisionID, creationAllowOption)
	document, isError = c.call(t, CreationToolInstall, map[string]any{"id": "demo.panel", "requestId": "retry-1", "decisionId": decisionID})
	if isError {
		t.Fatalf("approved install failed: %+v", document)
	}
	if got := c.status(t, CreationToolInstall, document); got != "enabled" {
		t.Fatalf("install status = %s (%+v)", got, document)
	}
	if link, _ := document["deepLink"].(string); link != "/plugins/demo.panel/" {
		t.Fatalf("deepLink = %q", link)
	}
	view, installed := pluginViewOf(c.harness.manager, "demo.panel")
	if !installed || view.State != LifecycleEnabled {
		t.Fatalf("inventory = %+v (installed=%v)", view, installed)
	}
	// The Native chunk is a fact of the same view the client syncs from.
	if len(view.Contributes) != 1 || view.Contributes[0].Form != "chunk" ||
		view.Contributes[0].Entry != "ui/settings.js" || view.Contributes[0].Slot != "keel.ui.settings-section" {
		t.Fatalf("installed contributes = %+v", view.Contributes)
	}
	if view.Extensibility == nil || !view.Extensibility.Usable {
		t.Fatalf("extensibility = %+v", view.Extensibility)
	}
	if chunk, _ := document["chunkEntry"].(string); chunk != "/plugins/demo.panel/ui/settings.js" {
		t.Fatalf("install chunkEntry = %q (%+v)", chunk, document)
	}
	// Replaying the same install request is idempotent: same state, no second
	// row, no second install audit fact for a new package.
	document, isError = c.call(t, CreationToolInstall, map[string]any{
		"id": "demo.panel", "requestId": "retry-1", "decisionId": decisionID,
	})
	if isError || document["idempotent"] != true {
		t.Fatalf("repeated install = %+v (isError=%v)", document, isError)
	}
	if row := c.harness.store.row(t, c.tenant, "demo.panel"); row.Version != view.Version {
		t.Fatalf("repeated install changed the row: %+v", row)
	}

	// 6. List reports the artifact as installed.
	document, _ = c.call(t, CreationToolList, map[string]any{})
	artifacts, _ := document["artifacts"].([]any)
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %+v", artifacts)
	}
	artifact, _ := artifacts[0].(map[string]any)
	if artifact["id"] != "demo.panel" || artifact["installed"] != true || artifact["valid"] != true {
		t.Fatalf("artifact = %+v", artifact)
	}
	if chunk, _ := artifact["chunkEntry"].(string); chunk != "/plugins/demo.panel/ui/settings.js" {
		t.Fatalf("list chunkEntry = %q (%+v)", chunk, artifact)
	}
	if contributes, _ := artifact["contributes"].([]any); len(contributes) != 1 {
		t.Fatalf("list contributes = %+v", artifact["contributes"])
	}

	// 7. Remove: approval, then uninstall + delete the staged package.
	document, _ = c.call(t, CreationToolRemove, map[string]any{"id": "demo.panel"})
	if got := c.status(t, CreationToolRemove, document); got != "approval_required" {
		t.Fatalf("remove status = %s (%+v)", got, document)
	}
	decisionID, _ = document["decisionId"].(string)
	c.answer(t, decisionID, creationAllowOption)
	document, isError = c.call(t, CreationToolRemove, map[string]any{"id": "demo.panel", "decisionId": decisionID})
	if isError {
		t.Fatalf("approved remove failed: %+v", document)
	}
	if got := c.status(t, CreationToolRemove, document); got != "removed" {
		t.Fatalf("remove status = %s (%+v)", got, document)
	}
	if _, installed := pluginViewOf(c.harness.manager, "demo.panel"); installed {
		t.Fatal("the plugin is still in the inventory after removal")
	}
	if row, found, err := c.harness.store.GetPlugin(context.Background(), c.tenant, "demo.panel"); err != nil {
		t.Fatalf("read removed plugin row: %v", err)
	} else if found {
		t.Fatalf("the plugin row survived removal: %+v", row)
	}
	for _, listed := range c.harness.manager.Snapshot().Plugins {
		if listed.ID == "demo.panel" {
			t.Fatalf("the removed plugin is still in the snapshot: %+v", listed)
		}
	}
	// Zero residue on the assembled surface: the published tool and the chunk
	// contribution are gone with the plugin.
	for _, tool := range c.harness.manager.Snapshot().Tools {
		if strings.HasPrefix(tool, "demo_panel_") {
			t.Fatalf("the removed plugin's tool is still published: %s", tool)
		}
	}
	if _, err := os.Stat(wantDir); !os.IsNotExist(err) {
		t.Fatalf("the staged package still exists: %v", err)
	}
	for _, want := range []string{"creation.scaffold:ok", "install:ok", "uninstall:ok", "creation.remove:ok"} {
		if !containsAction(c.harness.store.auditActions(t), want) {
			t.Fatalf("audit is missing %q: %v", want, c.harness.store.auditActions(t))
		}
	}
}

// TestCreationScaffoldRefusesInvalidInput is the "生成失败不留半包" control: an
// id that is not a plugin id, and a package whose published tool name cannot
// satisfy the wire grammar, both fail without leaving a staged directory.
func TestCreationScaffoldRefusesInvalidInput(t *testing.T) {
	c := newCreationHarness(t)
	if _, err := c.harness.manager.Enable(context.Background(), CreationPluginID, AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("enable creation: %v", err)
	}

	document, isError := c.call(t, CreationToolScaffold, map[string]any{"id": "Not An Id", "purpose": "x"})
	if !isError || c.status(t, CreationToolScaffold, document) != "failed" {
		t.Fatalf("an invalid id was scaffolded: %+v", document)
	}
	if _, err := os.Stat(filepath.Join(c.harness.root, CreationStagingDir, "Not An Id")); !os.IsNotExist(err) {
		t.Fatalf("an invalid id left a directory behind: %v", err)
	}

	// A 60-character id + a 7-character tool publishes 68 characters, which the
	// real validator refuses; the scaffold must report the field error and leave
	// nothing on disk.
	longID := "a" + strings.Repeat("b", 59)
	document, isError = c.call(t, CreationToolScaffold, map[string]any{
		"id": longID, "purpose": "too long a published name", "tool": "toolong",
	})
	if !isError || c.status(t, CreationToolScaffold, document) != "invalid" {
		t.Fatalf("an unpublishable name was scaffolded: %+v", document)
	}
	if _, err := os.Stat(filepath.Join(c.harness.root, CreationStagingDir, longID)); !os.IsNotExist(err) {
		t.Fatalf("a failed scaffold left a package behind: %v", err)
	}
	if !containsAction(c.harness.store.auditActions(t), "creation.scaffold:failed") {
		t.Fatalf("the failed generation was not audited: %v", c.harness.store.auditActions(t))
	}
}

// TestCreationInstallRequiresTheApprovalPrimitive is the fail-closed control: a
// deployment without decisions cannot install through creation mode.
func TestCreationInstallRequiresTheApprovalPrimitive(t *testing.T) {
	c := newCreationHarnessWith(t, false)
	ctx := context.Background()
	if _, err := c.harness.manager.Enable(ctx, CreationPluginID, AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("enable creation: %v", err)
	}
	if _, isError := c.call(t, CreationToolScaffold, map[string]any{"id": "demo.panel", "purpose": "p"}); isError {
		t.Fatal("scaffold failed")
	}
	document, isError := c.call(t, CreationToolInstall, map[string]any{"id": "demo.panel"})
	if !isError || !strings.Contains(strings.ToLower(asString(document["error"])), "approval") {
		t.Fatalf("install without decisions = %+v", document)
	}
	if _, installed := pluginViewOf(c.harness.manager, "demo.panel"); installed {
		t.Fatal("the plugin was installed without the approval primitive")
	}
}

// TestCreationRefusesAnotherTenant is the cross-tenant control: a tool call
// whose ambient run belongs to another tenant cannot read, scaffold, install or
// remove, and the manager refuses a lifecycle write attributed to that tenant.
func TestCreationRefusesAnotherTenant(t *testing.T) {
	c := newCreationHarness(t)
	ctx := context.Background()
	if _, err := c.harness.manager.Enable(ctx, CreationPluginID, AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("enable creation: %v", err)
	}
	other := uuid.New()
	foreign := WithRunScope(ctx, RunScope{TenantID: other, SessionID: uuid.New(), RunID: "run-foreign"})

	for _, tool := range []string{CreationToolCatalog, CreationToolScaffold, CreationToolValidate, CreationToolList} {
		args := map[string]any{"id": "demo.panel", "purpose": "cross-tenant"}
		document, isError := c.callCtx(t, foreign, tool, args)
		if !isError {
			t.Fatalf("%s served another tenant: %+v", tool, document)
		}
		if !strings.Contains(asString(document["error"]), "another tenant") {
			t.Fatalf("%s error = %+v", tool, document)
		}
	}
	if _, err := os.Stat(filepath.Join(c.harness.root, CreationStagingDir, "demo.panel")); !os.IsNotExist(err) {
		t.Fatalf("a cross-tenant scaffold wrote a package: %v", err)
	}

	// The manager itself refuses a write attributed to the other tenant.
	if _, err := c.harness.manager.Install(ctx, InstallRequest{Path: c.harness.root},
		AuditContext{TenantID: &other, Actor: "foreign"}); !errors.Is(err, ErrPluginTenant) {
		t.Fatalf("cross-tenant install = %v", err)
	}
	if _, err := c.harness.manager.Enable(ctx, CreationPluginID,
		AuditContext{TenantID: &other, Actor: "foreign"}); !errors.Is(err, ErrPluginTenant) {
		t.Fatalf("cross-tenant enable = %v", err)
	}
}

// TestCreationValidateReportsFieldErrors covers the fail-closed validator on a
// tampered staged package: the call reports the field path, does not install,
// and leaves the package (and the inventory) untouched.
func TestCreationValidateReportsFieldErrors(t *testing.T) {
	c := newCreationHarness(t)
	ctx := context.Background()
	if _, err := c.harness.manager.Enable(ctx, CreationPluginID, AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("enable creation: %v", err)
	}
	dir := filepath.Join(c.harness.root, CreationStagingDir, "demo.broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Missing version and a non-namespaced slot are field-level errors.
	manifest := `id: demo.broken
kind: webview
description: broken
contributes:
  - slot: route
    form: chunk
    entry: ui/panel.js
`
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	document, isError := c.call(t, CreationToolValidate, map[string]any{"id": "demo.broken"})
	if !isError || document["ok"] == true {
		t.Fatalf("invalid package validated: %+v", document)
	}
	fields, _ := document["fields"].([]any)
	if len(fields) == 0 {
		t.Fatalf("no field-level errors: %+v", document)
	}
	document, isError = c.call(t, CreationToolInstall, map[string]any{"id": "demo.broken"})
	if !isError || c.status(t, CreationToolInstall, document) != "invalid" {
		t.Fatalf("invalid package install = %+v", document)
	}
	if _, installed := pluginViewOf(c.harness.manager, "demo.broken"); installed {
		t.Fatal("an invalid package was installed")
	}
}

// TestCreationPromptSectionIsTheStandard pins the prompt contribution: it is the
// plugin standard (manifest fields, three kinds, template path), it appears only
// while the plugin is active, and it carries no upstream attribution.
func TestCreationPromptSectionIsTheStandard(t *testing.T) {
	c := newCreationHarness(t)
	ctx := context.Background()
	if prompt := c.harness.runtime.Snapshot().Prompt; len(prompt) != 0 {
		t.Fatalf("the creation plugin contributed a prompt while disabled: %+v", prompt)
	}
	if _, err := c.harness.manager.Enable(ctx, CreationPluginID, AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("enable creation: %v", err)
	}
	prompt := c.harness.runtime.Snapshot().Prompt
	if len(prompt) != 1 || prompt[0].Name != CreationPromptSectionName {
		t.Fatalf("prompt = %+v", prompt)
	}
	// The inventory projection carries the same section (that is what a host and
	// /v1/plugins read).
	inventory := c.harness.manager.Snapshot()
	if len(inventory.Prompt) != 1 || inventory.Prompt[0].Name != CreationPromptSectionName {
		t.Fatalf("inventory prompt = %+v", inventory.Prompt)
	}
	view, _ := pluginViewOf(c.harness.manager, CreationPluginID)
	if len(view.Prompt) != 1 || view.Prompt[0].Name != CreationPromptSectionName {
		t.Fatalf("plugin view prompt = %+v", view.Prompt)
	}
	text := prompt[0].Content
	for _, want := range []string{
		"plugin.yaml", "id:", "version:", "kind:", "provides:", "permissions:", "entry:",
		"in-process", "sidecar", "webview",
		"plugins/templates/webview-react-solidglass/",
		`creation {action:"scaffold"`, `creation {action:"validate"`, `creation {action:"install"`,
		`creation {action:"catalog"}`, `creation {action:"list"}`, `creation {action:"remove"}`,
		"approval_required", "deepLink",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the standard does not mention %q", want)
		}
	}
	// Zero upstream attribution: the text names this product's vocabulary only.
	lower := strings.ToLower(text)
	for _, forbidden := range []string{"hermes", "nous", "deepseek", "dsh"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("the standard carries the upstream attribution %q", forbidden)
		}
	}
	// Disabling withdraws it.
	if _, err := c.harness.manager.Disable(ctx, CreationPluginID, AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("disable creation: %v", err)
	}
	if prompt := c.harness.runtime.Snapshot().Prompt; len(prompt) != 0 {
		t.Fatalf("the disabled plugin still contributes a prompt: %+v", prompt)
	}
}

// TestWebviewBackendLoadsThroughTheSidecarLoader covers the loader fallback: a
// webview package that declares `entry.command` has a sidecar backend by
// contract, and a host that registers only the sidecar loader must still be able
// to enable it.
func TestWebviewBackendLoadsThroughTheSidecarLoader(t *testing.T) {
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "refresh")
	root := t.TempDir()
	harness := newSidecarHarness(t, root, nil)
	harness.manager.loaders = map[pluginpkg.Kind]PluginLoader{
		pluginpkg.KindSidecar: newTestSidecarLoader(t, SidecarOptions{}),
	}
	source := writePluginPackage(t, t.TempDir(), map[string]string{
		"plugin.yaml": `id: demo.webpanel
version: 1.0.0
kind: webview
description: "a panel with a backend"
provides:
  tools:
    - refresh
  ui:
    entry: ui/index.html
    title: Panel
permissions:
  tools:
    - refresh
entry:
  command: [` + sidecarHelperBinary(t) + `, -test.run=^TestSidecarHelperProcess$]
  env:
    - KEEL_TEST_SIDECAR
    - KEEL_TEST_SIDECAR_TOOLS
`,
		"ui/index.html": "<!doctype html><title>panel</title>",
	})
	ctx := context.Background()
	if _, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	view, err := harness.manager.Enable(ctx, "demo.webpanel", AuditContext{Actor: "test"})
	if err != nil {
		t.Fatalf("enable a webview backend without a webview loader: %v", err)
	}
	if view.State != LifecycleEnabled {
		t.Fatalf("view = %+v", view)
	}
	if !contains(harness.tools.list(), "demo_webpanel_refresh") {
		t.Fatalf("published tools = %v, want demo_webpanel_refresh", harness.tools.list())
	}
	if _, err := harness.manager.Disable(ctx, "demo.webpanel", AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("disable: %v", err)
	}
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}
