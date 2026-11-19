package mcp

// The host-registry readiness gate (change feat-mcp-apps-events, P2.8): an app
// whose document has not been fetched, validated and published must not reach a
// client as a mountable surface — no app binding in tool details, no mcp_apps
// line.

import (
	"encoding/json"
	"strings"
	"testing"
)

func catalogServer() *Server {
	tool := Tool{
		Name: "dashboard",
		UI: ToolUI{ResourceURI: "ui://weather/dashboard", Visibility: []string{VisibilityModel, VisibilityApp}},
	}
	return &Server{
		Entry: ServerEntry{Name: "weather"},
		Tools: []Tool{tool},
	}
}

func TestAppReadinessGatesThePublishedCatalogue(t *testing.T) {
	server := catalogServer()
	manager := &Manager{servers: []*Server{server}}
	appID := AppIDFor("weather", "ui://weather/dashboard")

	// No gate (the P1 behaviour, no host registry wired): the declared app is
	// publishable.
	if got := manager.Apps(); len(got) != 1 || got[0].AppID != appID {
		t.Fatalf("ungated Apps() = %+v", got)
	}
	if manager.AppsPromptSection() == "" {
		t.Fatal("ungated AppsPromptSection is empty")
	}

	// The registry says "not published yet" (fetch failed or still in flight):
	// the app disappears from every client-facing projection, but the raw
	// declaration stays available to the registry that will fetch it.
	server.SetAppReadiness(func(id string) bool { return false })
	if got := manager.Apps(); len(got) != 0 {
		t.Fatalf("gated Apps() = %+v, want none", got)
	}
	if got := manager.DeclaredApps(); len(got) != 1 {
		t.Fatalf("DeclaredApps() = %+v, want the declaration", got)
	}
	if section := manager.AppsPromptSection(); section != "" {
		t.Fatalf("gated AppsPromptSection = %q, want empty", section)
	}
	if _, ok := AppBindingFor(server, server.Tools[0]); ok {
		t.Fatal("a gated app still produced a tool binding")
	}
	if binding, ok := manager.AppForTool("weather", "dashboard"); ok {
		t.Fatalf("AppForTool returned a gated binding: %+v", binding)
	}

	// Once the document is published, the same declaration is live again.
	server.SetAppReadiness(func(id string) bool { return id == appID })
	if got := manager.Apps(); len(got) != 1 {
		t.Fatalf("ready Apps() = %+v", got)
	}
	binding, ok := AppBindingFor(server, server.Tools[0])
	if !ok || binding.AppID != appID || binding.ResourceURI != "ui://weather/dashboard" {
		t.Fatalf("binding = %+v, %v", binding, ok)
	}
	if !strings.Contains(manager.AppsPromptSection(), "- weather/dashboard: ui://weather/dashboard") {
		t.Fatalf("apps section = %q", manager.AppsPromptSection())
	}
}

// TestValidateAppResourceMetaPreference pins where the app metadata is read
// from: the content item's `_meta` wins, the result-level object is the
// accepted fallback, and no metadata yields none.
func TestValidateAppResourceMetaPreference(t *testing.T) {
	itemMeta := json.RawMessage(`{"ui":{"prefersBorder":true}}`)
	resultMeta := json.RawMessage(`{"ui":{"prefersBorder":false}}`)
	result := ReadResourceResult{
		Contents: []ResourceContents{{
			URI:      "ui://weather/dashboard",
			MimeType: AppMIMEType,
			Text:     "<html></html>",
			Meta:     itemMeta,
		}},
		Meta: resultMeta,
	}
	resource, err := ValidateAppResource(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(resource.Meta) != string(itemMeta) {
		t.Fatalf("meta = %s, want the content item's %s", resource.Meta, itemMeta)
	}

	result.Contents[0].Meta = nil
	resource, err = ValidateAppResource(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(resource.Meta) != string(resultMeta) {
		t.Fatalf("meta = %s, want the result-level fallback %s", resource.Meta, resultMeta)
	}

	result.Meta = nil
	resource, err = ValidateAppResource(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(resource.Meta) != 0 {
		t.Fatalf("meta = %s, want none", resource.Meta)
	}
}
