package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func catalogueServer(name string, tools ...Tool) *Server {
	return &Server{
		Entry: ServerEntry{Name: name, Config: map[string]any{"description": name + " server"}},
		Tools: tools,
	}
}

func uiTool(name, uri string, visibility ...string) Tool {
	tool := Tool{Name: name}
	if uri != "" {
		tool.UI.ResourceURI = uri
	}
	if visibility != nil {
		tool.UI.Visibility = visibility
	}
	return tool
}

func TestModelToolsFilterVisibility(t *testing.T) {
	hidden := Tool{Name: "hidden", UI: ToolUI{Visibility: []string{}}}
	server := catalogueServer("docs",
		uiTool("search", "ui://docs/search"),
		uiTool("refresh", "ui://docs/panel", VisibilityApp),
		hidden,
		Tool{Name: "plain"},
	)
	model := server.ModelTools()
	names := make([]string, 0, len(model))
	for _, tool := range model {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); got != "search,plain" {
		t.Fatalf("model tools = %s, want search,plain", got)
	}
	app := server.AppTools()
	appNames := make([]string, 0, len(app))
	for _, tool := range app {
		appNames = append(appNames, tool.Name)
	}
	if got := strings.Join(appNames, ","); got != "search,refresh,plain" {
		t.Fatalf("app tools = %s, want search,refresh,plain (visibility, not resource, decides)", got)
	}
	if _, ok := server.ModelTool("refresh"); ok {
		t.Fatal("an app-only tool is reachable through ModelTool")
	}
	if tool, ok := server.Tool("refresh"); !ok || tool.VisibleToModel() {
		t.Fatal("the raw catalogue lost the app-only tool or misreported its visibility")
	}
}

func TestAppsCatalogueDeduplicatesOneResource(t *testing.T) {
	// Two tools referencing the same resource, one of them declared through the
	// deprecated flat key: exactly one app with two tool references.
	first := uiTool("render", "ui://weather/dashboard")
	second := Tool{Name: "refresh"}
	second.UI, _ = parseToolUI(json.RawMessage(`{"ui/resourceUri": "ui://weather/dashboard"}`))
	manager := &Manager{servers: []*Server{
		catalogueServer("weather", first, second),
		catalogueServer("other", uiTool("map", "ui://weather/dashboard")),
	}}

	apps := manager.Apps()
	if len(apps) != 2 {
		t.Fatalf("apps = %+v, want one per (server, resource)", apps)
	}
	byServer := map[string]App{}
	for _, app := range apps {
		byServer[app.Server] = app
		if app.AppID != AppIDFor(app.Server, app.ResourceURI) {
			t.Errorf("appId = %q, want the derived id", app.AppID)
		}
	}
	weather := byServer["weather"]
	if len(weather.Tools) != 2 {
		t.Fatalf("weather tools = %+v, want both referencing tools", weather.Tools)
	}
	if weather.Tools[0].Tool != "refresh" || weather.Tools[1].Tool != "render" {
		t.Errorf("weather tools = %+v, want the deterministic order refresh,render", weather.Tools)
	}
	if got := strings.Join(byServer["other"].Tools[0].Visibility, ","); got != "model,app" {
		t.Errorf("flat-key visibility = %s, want the default", got)
	}
}

func TestAppsCatalogueSkipsNonAppTools(t *testing.T) {
	manager := &Manager{servers: []*Server{catalogueServer("docs",
		uiTool("search", ""), // no resource
		uiTool("model_only", "ui://docs/panel", VisibilityModel), // resource, model only
		uiTool("app_only", "ui://docs/panel", VisibilityApp),     // resource, app only
	)}}
	apps := manager.Apps()
	if len(apps) != 1 {
		t.Fatalf("apps = %+v, want one", apps)
	}
	if len(apps[0].Tools) != 1 || apps[0].Tools[0].Tool != "app_only" {
		t.Fatalf("app tools = %+v, want only app_only", apps[0].Tools)
	}
	binding, ok := manager.AppForTool("docs", "app_only")
	if !ok || binding.AppID != apps[0].AppID || binding.ResourceURI != "ui://docs/panel" {
		t.Fatalf("AppForTool = %+v ok=%v", binding, ok)
	}
	if _, ok := manager.AppForTool("docs", "model_only"); ok {
		t.Fatal("a model-only tool produced an app binding")
	}
	if _, ok := manager.AppForTool("docs", "missing"); ok {
		t.Fatal("a missing tool produced an app binding")
	}
}

func TestAppsCatalogueSkipsDisconnectedServers(t *testing.T) {
	broken := catalogueServer("broken", uiTool("render", "ui://broken/panel"))
	broken.Err = errors.New("connection refused")
	manager := &Manager{servers: []*Server{broken}}
	if apps := manager.Apps(); len(apps) != 0 {
		t.Fatalf("apps = %+v, want none for a disconnected server", apps)
	}
}

func TestAppsPromptSection(t *testing.T) {
	manager := &Manager{servers: []*Server{catalogueServer("weather",
		uiTool("render", "ui://weather/dashboard"),
		uiTool("refresh", "ui://weather/dashboard", VisibilityApp),
		uiTool("plain", ""),
	)}}
	section := manager.AppsPromptSection()
	want := "- weather/refresh: ui://weather/dashboard\n- weather/render: ui://weather/dashboard"
	if section != want {
		t.Fatalf("AppsPromptSection = %q, want %q", section, want)
	}
	if strings.Contains(section, "plain") {
		t.Fatalf("AppsPromptSection leaked a tool without an app: %q", section)
	}
}

func TestAppsPromptSectionEmptyWithoutApps(t *testing.T) {
	if got := (&Manager{servers: []*Server{catalogueServer("docs", Tool{Name: "search"})}}).AppsPromptSection(); got != "" {
		t.Fatalf("AppsPromptSection = %q, want empty", got)
	}
	if got := (*Manager)(nil).AppsPromptSection(); got != "" {
		t.Fatalf("nil manager section = %q, want empty", got)
	}
}
