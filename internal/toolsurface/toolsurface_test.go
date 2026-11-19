package toolsurface_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/skills"
	"github.com/jixoai/keel/internal/toolsurface"
)

// fakeMCP is the aggregate backend over an in-memory server catalogue. It never
// touches a transport: the aggregate tool's contract is server/tool dispatch,
// not connection handling.
type fakeMCP struct {
	servers []*mcp.Server
	calls   int
	result  mcp.CallToolResult
	err     error
}

func (f *fakeMCP) Servers() []*mcp.Server { return f.servers }
func (f *fakeMCP) CallTool(_ context.Context, server, tool string, _ json.RawMessage) (mcp.CallToolResult, error) {
	f.calls++
	for _, candidate := range f.servers {
		if candidate.Entry.Name != server {
			continue
		}
		// Mirror the manager: a configured but disconnected server fails before
		// name lookup, exactly like the production CallTool.
		if candidate.Err != nil {
			return mcp.CallToolResult{}, fmt.Errorf("mcp: server %q: %w", server, candidate.Err)
		}
		for _, entry := range candidate.Tools {
			if entry.Name == tool {
				return f.result, f.err
			}
		}
		return mcp.CallToolResult{}, mcp.ErrToolNotFound
	}
	return mcp.CallToolResult{}, mcp.ErrServerNotFound
}

func mcpServer(name string, tools ...mcp.Tool) *mcp.Server {
	return &mcp.Server{Entry: mcp.ServerEntry{Name: name, Config: map[string]any{"description": name + " server"}}, Tools: tools}
}

func builtinRegistry(t *testing.T, backends toolsurface.Backends, discovered ...skills.Skill) map[string]agentcore.AgentTool {
	t.Helper()
	registry := map[string]agentcore.AgentTool{}
	for _, tool := range toolsurface.BuiltinTools(discovered, backends) {
		registry[tool.Name()] = tool
	}
	return registry
}

func runAction(t *testing.T, tool agentcore.AgentTool, args string) agentcore.AgentToolResult {
	t.Helper()
	result, err := tool.Execute(context.Background(), "call-1", json.RawMessage(args), nil)
	if err != nil {
		t.Fatalf("%s execute: %v", tool.Name(), err)
	}
	return result
}

func errorDocument(t *testing.T, result agentcore.AgentToolResult) map[string]any {
	t.Helper()
	document, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v, want map", result.StructuredContent)
	}
	problem, ok := document["error"].(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v, want an error document", document)
	}
	return problem
}

func TestBuiltinSurfaceIsExactlyEleven(t *testing.T) {
	h, _, err := host.New(host.Config{IncludeBuiltins: true})
	if err != nil {
		t.Fatal(err)
	}
	got := h.Tools().Names()
	want := toolsurface.BuiltinNames()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("builtin tool names = %v, want exactly %v", got, want)
	}
	if len(want) != 11 {
		t.Fatalf("builtin surface has %d names, want 11", len(want))
	}
	removed := []string{
		"spawn_subagent", "creation_scaffold", "creation_validate", "creation_install",
		"creation_list", "creation_remove", "skill", "clock", "echo",
	}
	for _, name := range removed {
		if _, ok := h.Tools().Get(name); ok {
			t.Fatalf("removed v1 tool %q is still registered", name)
		}
	}
	for _, name := range got {
		if strings.HasPrefix(name, "mcp__") {
			t.Fatalf("per-server MCP tool name %q is registered", name)
		}
	}
	// Calling a retired name is an unknown-tool failure, not a route to a shim.
	for _, retired := range []string{"spawn_subagent", "mcp__fs__read", "skill"} {
		errs := h.Tools().Validate(retired, json.RawMessage(`{}`))
		if len(errs) == 0 || !strings.Contains(errs[0].Message, "unknown tool") {
			t.Fatalf("retired tool %q validation = %#v, want unknown tool", retired, errs)
		}
	}
}

func TestUnknownActionIsTypedAndSideEffectFree(t *testing.T) {
	registry := builtinRegistry(t, toolsurface.Backends{})
	result := runAction(t, registry[toolsurface.Skills], `{"action":"delete_everything"}`)
	if result.IsError == nil || !*result.IsError {
		t.Fatalf("unknown action result = %#v, want an error", result)
	}
	problem := errorDocument(t, result)
	if problem["code"] != toolsurface.CodeUnknownAction {
		t.Fatalf("code = %v, want %s", problem["code"], toolsurface.CodeUnknownAction)
	}
	available, ok := problem["availableActions"].([]string)
	if !ok || len(available) != 4 {
		t.Fatalf("availableActions = %#v", problem["availableActions"])
	}
}

func TestReservedActionsReturnNotImplemented(t *testing.T) {
	registry := builtinRegistry(t, toolsurface.Backends{})
	cases := []struct {
		tool   string
		args   string
		action string
	}{
		{toolsurface.Bash, `{"action":"run","command":"ls"}`, "run"},
		{toolsurface.Bash, `{"action":"jobs","operation":"list"}`, "jobs"},
		{toolsurface.Approve, `{"action":"confirm","prompt":"x"}`, "confirm"},
		{toolsurface.Notify, `{"action":"push","text":"x"}`, "push"},
		{toolsurface.Read, `{"path":"a.txt"}`, ""},
		{toolsurface.Write, `{"path":"a.txt","content":"x"}`, ""},
		{toolsurface.Edit, `{"path":"a.txt"}`, ""},
	}
	for _, test := range cases {
		result := runAction(t, registry[test.tool], test.args)
		problem := errorDocument(t, result)
		if problem["code"] != toolsurface.CodeNotImplemented {
			t.Fatalf("%s.%s code = %v, want not_implemented", test.tool, test.action, problem["code"])
		}
		if !strings.Contains(problem["message"].(string), test.tool) {
			t.Fatalf("%s message = %v", test.tool, problem["message"])
		}
	}
}

func TestMCPActions(t *testing.T) {
	backend := &fakeMCP{
		servers: []*mcp.Server{mcpServer("docs",
			mcp.Tool{Name: "search", Description: "Search docs", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
			mcp.Tool{Name: "fetch"},
		)},
		result: mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "found"}}},
	}
	registry := builtinRegistry(t, toolsurface.Backends{MCP: backend})
	tool := registry[toolsurface.MCP]

	list := runAction(t, tool, `{"action":"list"}`)
	listText := list.Content[0].(agentcore.TextContent).Text
	for _, want := range []string{`"name":"docs"`, `"name":"search"`, `"name":"fetch"`} {
		if !strings.Contains(listText, want) {
			t.Fatalf("mcp list = %s, want %s", listText, want)
		}
	}

	describe := runAction(t, tool, `{"action":"describe","server":"docs","tool":"search"}`)
	describeText := describe.Content[0].(agentcore.TextContent).Text
	if !strings.Contains(describeText, `"inputSchema":{"properties":{"q":{"type":"string"}},"type":"object"}`) {
		t.Fatalf("mcp describe = %s", describeText)
	}

	call := runAction(t, tool, `{"action":"call","server":"docs","tool":"search","arguments":{"q":"x"}}`)
	if got := agentcore.ContentToText(call.Content); got != "found" {
		t.Fatalf("mcp call content = %q", got)
	}
	if backend.calls != 1 {
		t.Fatalf("mcp call count = %d", backend.calls)
	}

	missing := runAction(t, tool, `{"action":"call","server":"docs","tool":"nope"}`)
	if problem := errorDocument(t, missing); problem["code"] != toolsurface.CodeNotFound {
		t.Fatalf("missing tool = %v", problem)
	}
}

func TestMCPNotFoundIsTyped(t *testing.T) {
	registry := builtinRegistry(t, toolsurface.Backends{MCP: &fakeMCP{}})
	tool := registry[toolsurface.MCP]
	result := runAction(t, tool, `{"action":"describe","server":"absent","tool":"x"}`)
	if problem := errorDocument(t, result); problem["code"] != toolsurface.CodeNotFound {
		t.Fatalf("describe missing server = %v", problem)
	}
	call := runAction(t, tool, `{"action":"call","server":"absent","tool":"x"}`)
	if problem := errorDocument(t, call); problem["code"] != toolsurface.CodeNotFound {
		t.Fatalf("call missing server = %v", problem)
	}
}

// TestMCPDisconnectedServerSharesOneCode pins the rule the review found broken:
// "the server is configured but not connected" is one fact, so describe and
// call answer it with the same typed code (backend_error). A name that does not
// exist stays not_found, because that is a different fact the caller can fix.
func TestMCPDisconnectedServerSharesOneCode(t *testing.T) {
	registry := builtinRegistry(t, toolsurface.Backends{MCP: &fakeMCP{servers: []*mcp.Server{{
		Entry: mcp.ServerEntry{Name: "flaky"},
		Tools: []mcp.Tool{{Name: "search"}},
		Err:   errors.New("connection refused"),
	}}}})
	tool := registry[toolsurface.MCP]
	describe := runAction(t, tool, `{"action":"describe","server":"flaky","tool":"search"}`)
	call := runAction(t, tool, `{"action":"call","server":"flaky","tool":"search"}`)
	describeCode := errorDocument(t, describe)["code"]
	callCode := errorDocument(t, call)["code"]
	if describeCode != toolsurface.CodeBackendFailed || callCode != toolsurface.CodeBackendFailed {
		t.Fatalf("disconnected describe/call codes = %v/%v, want both %s", describeCode, callCode, toolsurface.CodeBackendFailed)
	}
}

func TestSkillsActions(t *testing.T) {
	visible := skills.Skill{Name: "pdf", Description: "Read PDFs", Path: "/skills/pdf/SKILL.md", Body: "Read the PDF body."}
	hidden := skills.Skill{Name: "secret", Description: "hidden", DisableModelInvocation: true}
	registry := builtinRegistry(t, toolsurface.Backends{}, visible, hidden)
	tool := registry[toolsurface.Skills]

	list := runAction(t, tool, `{"action":"ls"}`)
	listText := list.Content[0].(agentcore.TextContent).Text
	if !strings.Contains(listText, `"name":"pdf"`) || strings.Contains(listText, "secret") {
		t.Fatalf("skills ls = %s", listText)
	}
	view := runAction(t, tool, `{"action":"view","name":"pdf"}`)
	if !strings.Contains(agentcore.ContentToText(view.Content), "Read the PDF body.") {
		t.Fatalf("skills view = %#v", view.Content)
	}
	missing := runAction(t, tool, `{"action":"view","name":"secret"}`)
	if problem := errorDocument(t, missing); problem["code"] != toolsurface.CodeNotFound {
		t.Fatalf("hidden skill view = %v", problem)
	}
	for _, action := range []string{"manage", "sources"} {
		result := runAction(t, tool, `{"action":"`+action+`"}`)
		if problem := errorDocument(t, result); problem["code"] != toolsurface.CodeNotImplemented {
			t.Fatalf("skills.%s = %v, want not_implemented", action, problem)
		}
	}
}

type fakeAgents struct {
	scope   agentext.RunScope
	args    json.RawMessage
	result  agentext.SubagentResult
	records []agentext.SubagentRecord
}

func (f *fakeAgents) Spawn(_ context.Context, scope agentext.RunScope, args json.RawMessage) (agentext.SubagentResult, error) {
	f.scope = scope
	f.args = append(json.RawMessage(nil), args...)
	return f.result, nil
}
func (f *fakeAgents) List(uuid.UUID, *uuid.UUID) []agentext.SubagentRecord { return f.records }
func (f *fakeAgents) Get(sessionID uuid.UUID) (agentext.SubagentRecord, bool) {
	for _, record := range f.records {
		if record.SessionID == sessionID.String() {
			return record, true
		}
	}
	return agentext.SubagentRecord{}, false
}

func TestAgentSpawnCarriesTheBinding(t *testing.T) {
	tenant, parent := uuid.New(), uuid.New()
	backend := &fakeAgents{result: agentext.SubagentResult{Status: "done", Summary: "ok", SessionID: uuid.NewString()}}
	registry := builtinRegistry(t, toolsurface.Backends{Agents: backend})
	ctx := agentext.WithRunScope(context.Background(), agentext.RunScope{TenantID: tenant, SessionID: parent, RunID: "run-1"})
	result, err := registry[toolsurface.Agent].Execute(ctx, "call-1",
		json.RawMessage(`{"action":"spawn","task":"review","agentId":"agent_member","idempotencyKey":"k1"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError != nil && *result.IsError {
		t.Fatalf("spawn result = %#v", result)
	}
	if backend.scope.TenantID != tenant || backend.scope.SessionID != parent {
		t.Fatalf("spawn scope = %+v", backend.scope)
	}
	var spawned struct {
		Task           string `json:"task"`
		AgentID        string `json:"agentId"`
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if err := json.Unmarshal(backend.args, &spawned); err != nil {
		t.Fatal(err)
	}
	if spawned.AgentID != "agent_member" {
		t.Fatalf("agentId = %q, want the member binding", spawned.AgentID)
	}
	if spawned.IdempotencyKey != "k1" || spawned.Task != "review" {
		t.Fatalf("spawn request = %+v", spawned)
	}

	// Without the ambient run scope the action is a typed refusal, not a guess.
	outside := runAction(t, registry[toolsurface.Agent], `{"action":"spawn","task":"x"}`)
	if problem := errorDocument(t, outside); problem["code"] != toolsurface.CodeRunScopeMissing {
		t.Fatalf("unscoped spawn = %v", problem)
	}
	for _, action := range []string{"fork", "resume", "cancel"} {
		reserved := runAction(t, registry[toolsurface.Agent], `{"action":"`+action+`"}`)
		if problem := errorDocument(t, reserved); problem["code"] != toolsurface.CodeNotImplemented {
			t.Fatalf("agent.%s = %v", action, problem)
		}
	}
}

func TestAgentListAndStatusStayInTheParent(t *testing.T) {
	tenant, parent := uuid.New(), uuid.New()
	child := uuid.NewString()
	backend := &fakeAgents{records: []agentext.SubagentRecord{{
		SessionID: child, TenantID: tenant.String(), ParentSessionID: parent.String(), Status: "done",
	}}}
	registry := builtinRegistry(t, toolsurface.Backends{Agents: backend})
	ctx := agentext.WithRunScope(context.Background(), agentext.RunScope{TenantID: tenant, SessionID: parent})
	list, err := registry[toolsurface.Agent].Execute(ctx, "call-1", json.RawMessage(`{"action":"list"}`), nil)
	if err != nil || !strings.Contains(agentcore.ContentToText(list.Content), child) {
		t.Fatalf("agent list = %#v err=%v", list, err)
	}
	status, err := registry[toolsurface.Agent].Execute(ctx, "call-2", json.RawMessage(`{"action":"status","sessionId":"`+child+`"}`), nil)
	if err != nil || !strings.Contains(agentcore.ContentToText(status.Content), `"status":"done"`) {
		t.Fatalf("agent status = %#v err=%v", status, err)
	}
	// A child of another parent is invisible.
	otherParent := uuid.New()
	ctxOther := agentext.WithRunScope(context.Background(), agentext.RunScope{TenantID: tenant, SessionID: otherParent})
	missing, err := registry[toolsurface.Agent].Execute(ctxOther, "call-3", json.RawMessage(`{"action":"status","sessionId":"`+child+`"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if problem := errorDocument(t, missing); problem["code"] != toolsurface.CodeNotFound {
		t.Fatalf("foreign child = %v", problem)
	}
}

// TestAgentUnresolvedBackendIsNotWired pins the three-state distinction the
// review found broken: a backend object was injected, but the service it
// delegates to does not exist. That is a deployment wiring gap (not_wired) for
// every action — never backend_error for spawn, and never a misleading empty
// child list for list/status.
func TestAgentUnresolvedBackendIsNotWired(t *testing.T) {
	backend := agentext.AgentBackend{Runner: func() *agentext.SubagentRunner { return nil }}
	registry := builtinRegistry(t, toolsurface.Backends{Agents: backend})
	ctx := agentext.WithRunScope(context.Background(), agentext.RunScope{
		TenantID: uuid.New(), SessionID: uuid.New(), RunID: "run-1",
	})
	for _, args := range []string{
		`{"action":"spawn","task":"x"}`,
		`{"action":"list"}`,
		`{"action":"status","sessionId":"` + uuid.NewString() + `"}`,
	} {
		result, err := registry[toolsurface.Agent].Execute(ctx, "call-1", json.RawMessage(args), nil)
		if err != nil {
			t.Fatalf("%s execute: %v", args, err)
		}
		if problem := errorDocument(t, result); problem["code"] != toolsurface.CodeNotWired {
			t.Fatalf("%s = %v, want %s", args, problem, toolsurface.CodeNotWired)
		}
	}
}

// TestCreationWiringAndDisabledStayDistinct pins the two facts the review
// conflated: a missing manager is not_wired (the deployment never wired the
// backend), while an installed-but-disabled or uninstalled plugin is
// plugin_disabled with wording that matches which one it is.
func TestCreationWiringAndDisabledStayDistinct(t *testing.T) {
	unwired := builtinRegistry(t, toolsurface.Backends{Creation: agentext.CreationBackend{
		Manager: func() *agentext.Manager { return nil },
	}})
	result := runAction(t, unwired[toolsurface.Creation], `{"action":"list"}`)
	if problem := errorDocument(t, result); problem["code"] != toolsurface.CodeNotWired {
		t.Fatalf("nil manager = %v, want %s", problem, toolsurface.CodeNotWired)
	}

	missing := &fakeCreation{err: agentext.ErrPluginNotFound}
	result = runAction(t, builtinRegistry(t, toolsurface.Backends{Creation: missing})[toolsurface.Creation], `{"action":"list"}`)
	problem := errorDocument(t, result)
	if problem["code"] != toolsurface.CodePluginDisabled || !strings.Contains(problem["message"].(string), "not installed") {
		t.Fatalf("uninstalled plugin = %v, want plugin_disabled naming installation", problem)
	}

	disabled := &fakeCreation{err: agentext.ErrPluginState}
	result = runAction(t, builtinRegistry(t, toolsurface.Backends{Creation: disabled})[toolsurface.Creation], `{"action":"list"}`)
	problem = errorDocument(t, result)
	if problem["code"] != toolsurface.CodePluginDisabled || !strings.Contains(problem["message"].(string), "not enabled") {
		t.Fatalf("disabled plugin = %v, want plugin_disabled naming the disabled state", problem)
	}
}

type fakeCreation struct {
	action string
	result agentcore.AgentToolResult
	err    error
}

func (f *fakeCreation) Invoke(_ context.Context, action string, _ json.RawMessage) (agentcore.AgentToolResult, error) {
	f.action = action
	return f.result, f.err
}

func TestCreationRoutesAndMapsDisabledPlugin(t *testing.T) {
	backend := &fakeCreation{result: agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(`{"status":"ok"}`)},
		StructuredContent: map[string]any{"status": "ok"},
	}}
	registry := builtinRegistry(t, toolsurface.Backends{Creation: backend})
	result := runAction(t, registry[toolsurface.Creation], `{"action":"catalog"}`)
	if backend.action != "catalog" || !strings.Contains(agentcore.ContentToText(result.Content), `"status":"ok"`) {
		t.Fatalf("creation catalog = %#v action=%q", result, backend.action)
	}
	backend.err = agentext.ErrPluginState
	disabled := runAction(t, registry[toolsurface.Creation], `{"action":"scaffold","id":"demo"}`)
	if problem := errorDocument(t, disabled); problem["code"] != toolsurface.CodePluginDisabled {
		t.Fatalf("disabled creation = %v", problem)
	}
	if backend.action != "scaffold" {
		t.Fatalf("action after plugin_disabled = %q", backend.action)
	}
}

func TestManifestHashStableAcrossMCPServers(t *testing.T) {
	hash := func(servers []*mcp.Server) string {
		t.Helper()
		h, _, err := host.New(host.Config{
			IncludeBuiltins: true,
			ToolBackends:    toolsurface.Backends{MCP: &fakeMCP{servers: servers}},
		})
		if err != nil {
			t.Fatalf("host: %v", err)
		}
		digest, err := toolsurface.RegistryHash(h.Tools())
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		return digest
	}
	empty := hash(nil)
	many := make([]*mcp.Server, 0, 100)
	for i := 0; i < 100; i++ {
		many = append(many, mcpServer("server-"+uuid.NewString(),
			mcp.Tool{Name: "tool-" + uuid.NewString(), InputSchema: json.RawMessage(`{"type":"object"}`)}))
	}
	if got := hash(many); got != empty {
		t.Fatalf("100 MCP servers changed the manifest hash: %s vs %s", got, empty)
	}
}

func TestApprovedPluginToolsChangeAndRestoreTheManifestHash(t *testing.T) {
	h, _, err := host.New(host.Config{IncludeBuiltins: true})
	if err != nil {
		t.Fatal(err)
	}
	base, err := toolsurface.RegistryHash(h.Tools())
	if err != nil {
		t.Fatal(err)
	}
	// The durable approval is the HTTP gate in front of install/enable
	// (internal/agentapi/plugin_approval.go). Once answered, the plugin runtime's
	// ToolSink is the publication point its provides.tools reaches; patch it onto
	// the host exactly as servicehost.RefreshProductTools does.
	publish := func(tools []agentcore.AgentTool) error {
		return h.RefreshSections(tools, "", nil)
	}
	runtime := agentext.NewRuntime(agentext.RuntimeOptions{Tools: publish})
	t.Cleanup(func() { runtime.Close(context.Background()) })
	plugin := &agentext.Plugin{
		ID: "vendor.panel", Version: "1.0.0",
		Tools: []agentext.ToolSpec{{
			Name:        "vendor_panel_refresh",
			Description: "Refresh the vendor panel.",
			Schema:      json.RawMessage(`{"type":"object"}`),
			Execute: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
				return agentcore.AgentToolResult{}, nil
			},
		}},
	}
	if err := runtime.ApplyPlugins(context.Background(), []*agentext.Plugin{plugin}); err != nil {
		t.Fatalf("enable plugin: %v", err)
	}
	enabled, err := toolsurface.RegistryHash(h.Tools())
	if err != nil {
		t.Fatal(err)
	}
	if enabled == base {
		t.Fatal("an approved plugin's provides.tools did not change the manifest hash")
	}
	if _, ok := h.Tools().Get("vendor_panel_refresh"); !ok {
		t.Fatal("plugin tool is not in the host registry after publication")
	}
	if err := runtime.ApplyPlugins(context.Background(), nil); err != nil {
		t.Fatalf("uninstall plugin: %v", err)
	}
	restored, err := toolsurface.RegistryHash(h.Tools())
	if err != nil {
		t.Fatal(err)
	}
	if restored != base {
		t.Fatalf("manifest hash did not restore after uninstall: %s vs %s", restored, base)
	}
}

func TestCreationBackendNilIsTyped(t *testing.T) {
	registry := builtinRegistry(t, toolsurface.Backends{})
	result := runAction(t, registry[toolsurface.Creation], `{"action":"list"}`)
	if problem := errorDocument(t, result); problem["code"] != toolsurface.CodeNotWired {
		t.Fatalf("nil creation backend = %v", problem)
	}
	if _, err := toolsurface.ManifestHash([]agentcore.AgentTool{brokenSchemaTool{}}); err == nil {
		t.Fatal("a broken schema must fail the manifest hash")
	}
}

func TestTasksBackendNilIsTyped(t *testing.T) {
	registry := builtinRegistry(t, toolsurface.Backends{})
	result := runAction(t, registry[toolsurface.Tasks], `{"action":"create","title":"x"}`)
	if problem := errorDocument(t, result); problem["code"] != toolsurface.CodeNotWired {
		t.Fatalf("nil tasks backend = %v", problem)
	}
}

type brokenSchemaTool struct{}

func (brokenSchemaTool) Name() string            { return "broken" }
func (brokenSchemaTool) Description() string     { return "broken" }
func (brokenSchemaTool) Schema() json.RawMessage { return json.RawMessage(`{`) }
func (brokenSchemaTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (brokenSchemaTool) Execute(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return agentcore.AgentToolResult{}, nil
}
