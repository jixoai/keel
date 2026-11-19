// MCP Apps catalogue: the internal binding table between tools and the UI
// resources they declare. This is NOT a model-facing tool surface: v2 exposes
// MCP only through the aggregate `mcp` tool (docs/PI-PARITY.md §4.2), and the
// tools listed here are reached by apps through the host bridge (P3), never by
// hardcoded mcp__<server>__<tool> names.

package mcp

import (
	"fmt"
	"sort"
	"strings"
)

// AppTool is one tool reference inside the app catalogue.
type AppTool struct {
	Server     string   `json:"server"`
	Tool       string   `json:"tool"`
	Visibility []string `json:"visibility"`
}

// App is one deduplicated UI resource: every tool that declares the same
// (server, resourceUri) is listed under the one appId.
type App struct {
	AppID       string    `json:"appId"`
	Server      string    `json:"server"`
	ResourceURI string    `json:"resourceUri"`
	Tools       []AppTool `json:"tools"`
}

// ModelTools returns the tools whose visibility includes "model": the only
// tools the aggregate `mcp` tool may list, describe or call.
func (s *Server) ModelTools() []Tool {
	if s == nil {
		return nil
	}
	out := make([]Tool, 0, len(s.Tools))
	for _, tool := range s.Tools {
		if tool.VisibleToModel() {
			out = append(out, tool)
		}
	}
	return out
}

// AppTools returns the tools whose visibility includes "app". An app-only tool
// (visibility ["app"]) lives here and nowhere on the model surface.
func (s *Server) AppTools() []Tool {
	if s == nil {
		return nil
	}
	out := make([]Tool, 0, len(s.Tools))
	for _, tool := range s.Tools {
		if tool.VisibleToApp() {
			out = append(out, tool)
		}
	}
	return out
}

// ModelTool returns one model-visible tool by its server-side name.
func (s *Server) ModelTool(name string) (Tool, bool) {
	if s == nil {
		return Tool{}, false
	}
	for _, tool := range s.Tools {
		if tool.Name == name && tool.VisibleToModel() {
			return tool, true
		}
	}
	return Tool{}, false
}

// Tool returns one tool by name, regardless of visibility.
func (s *Server) Tool(name string) (Tool, bool) {
	if s == nil {
		return Tool{}, false
	}
	for _, tool := range s.Tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return Tool{}, false
}

// Apps returns the deduplicated app catalogue of every connected server. An app
// is keyed by appId = AppIDFor(server, resourceUri); a resource referenced by
// several tools (or declared through both the structured and the deprecated
// flat key) appears exactly once. The order is deterministic: apps by appId,
// tools by (server, tool).
func (m *Manager) Apps() []App {
	if m == nil {
		return nil
	}
	byID := make(map[string]*App)
	for _, server := range m.Servers() {
		if server.Err != nil {
			continue
		}
		for _, tool := range server.Tools {
			uri := tool.AppResourceURI()
			if uri == "" || !tool.VisibleToApp() {
				continue
			}
			appID := AppIDFor(server.Entry.Name, uri)
			app := byID[appID]
			if app == nil {
				app = &App{AppID: appID, Server: server.Entry.Name, ResourceURI: uri}
				byID[appID] = app
			}
			app.Tools = append(app.Tools, AppTool{
				Server:     server.Entry.Name,
				Tool:       tool.Name,
				Visibility: tool.VisibilityList(),
			})
		}
	}
	apps := make([]App, 0, len(byID))
	for _, app := range byID {
		sort.Slice(app.Tools, func(i, j int) bool {
			if app.Tools[i].Server != app.Tools[j].Server {
				return app.Tools[i].Server < app.Tools[j].Server
			}
			return app.Tools[i].Tool < app.Tools[j].Tool
		})
		apps = append(apps, *app)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].AppID < apps[j].AppID })
	return apps
}

// AppForTool returns the app binding of one tool: its derived appId, resource
// URI and effective visibility. A tool without a UI resource has no binding.
func (m *Manager) AppForTool(server, tool string) (MCPAppBinding, bool) {
	if m == nil {
		return MCPAppBinding{}, false
	}
	for _, candidate := range m.Servers() {
		if candidate.Entry.Name != server || candidate.Err != nil {
			continue
		}
		entry, ok := candidate.Tool(tool)
		if !ok {
			return MCPAppBinding{}, false
		}
		return AppBindingFor(candidate, entry)
	}
	return MCPAppBinding{}, false
}

// AppBindingFor returns the app binding of one tool of one server. A tool that
// declares no UI resource (or whose declaration was unusable), or that is not
// visible to apps, has none.
func AppBindingFor(server *Server, tool Tool) (MCPAppBinding, bool) {
	if server == nil || tool.AppResourceURI() == "" || !tool.VisibleToApp() {
		return MCPAppBinding{}, false
	}
	return MCPAppBinding{
		AppID:       AppIDFor(server.Entry.Name, tool.AppResourceURI()),
		ResourceURI: tool.AppResourceURI(),
		Visibility:  tool.VisibilityList(),
	}, true
}

// AppsPromptSection renders the mcp_apps section: one line per app-visible tool
// that declares an interactive UI resource, so the model knows which results
// have a rendered surface. Empty when no server declares one.
func (m *Manager) AppsPromptSection() string {
	apps := m.Apps()
	if len(apps) == 0 {
		return ""
	}
	var lines []string
	for _, app := range apps {
		for _, tool := range app.Tools {
			lines = append(lines, fmt.Sprintf("- %s/%s: %s", tool.Server, tool.Tool, app.ResourceURI))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}
