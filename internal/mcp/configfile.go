// Behaviour ported from @earendil-works/pi coding-agent,
// src/extensions/mcp/config.ts (MIT License); implemented independently in Go.
// Contract kept identical; see docs/UPSTREAM.md.
//
// Servers are read from a global mcp.json and, for trusted projects, from a
// project mcp.json. Both use the shared `mcpServers` shape. Project entries
// replace global entries with the same name; an entry without command, url, or
// type only overrides enabled/exposure of the global server.

package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ServerEntry is one loaded server configuration.
type ServerEntry struct {
	Name   string
	Config map[string]any
	// Source is the config file that defined the entry.
	Source string
	// Scope is "global" or "project".
	Scope string
	// Override is the project file when it only overrides a global entry.
	Override string
}

// LoadedConfig is the merged MCP configuration.
type LoadedConfig struct {
	Servers []ServerEntry
	Errors  []string
}

// overrideKeys are the fields a project-only entry may set on a global server.
var overrideKeys = map[string]bool{"enabled": true, "exposure": true, "toolExposure": true}

// LoadConfigFiles reads the global mcp.json and, when projectPath is not
// empty, the project file. Missing files are not errors; invalid entries are
// collected in Errors and skipped.
func LoadConfigFiles(globalPath, projectPath string) LoadedConfig {
	state := configState{servers: map[string]*ServerEntry{}}
	readConfigFile(globalPath, "global", &state)
	if projectPath != "" {
		readConfigFile(projectPath, "project", &state)
	}
	loaded := LoadedConfig{Errors: state.errors}
	for _, entry := range state.servers {
		loaded.Servers = append(loaded.Servers, *entry)
	}
	return loaded
}

// LoadConfig loads the host defaults: <agentDir>/mcp.json plus
// <cwd>/.keel/mcp.json when the project is trusted.
func LoadConfig(agentDir, cwd string, projectTrusted bool) LoadedConfig {
	projectPath := ""
	if projectTrusted {
		projectPath = filepath.Join(cwd, ".keel", "mcp.json")
	}
	return LoadConfigFiles(filepath.Join(agentDir, "mcp.json"), projectPath)
}

type configState struct {
	servers map[string]*ServerEntry
	errors  []string
}

func readConfigFile(path, scope string, state *configState) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		state.errors = append(state.errors, fmt.Sprintf("%s: %v", path, err))
		return
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil || parsed == nil {
		state.errors = append(state.errors, fmt.Sprintf("%s: expected an object with an \"mcpServers\" object", path))
		return
	}
	rawServers, hasServers := parsed["mcpServers"]
	if !hasServers {
		return
	}
	servers, ok := rawServers.(map[string]any)
	if !ok {
		state.errors = append(state.errors, fmt.Sprintf("%s: expected an object with an \"mcpServers\" object", path))
		return
	}
	for name, value := range servers {
		if scope == "project" {
			if record, ok := value.(map[string]any); ok && IsMcpServerOverride(record) {
				base := state.servers[name]
				var extra []string
				for key := range record {
					if !overrideKeys[key] {
						extra = append(extra, key)
					}
				}
				if base == nil {
					state.errors = append(state.errors, fmt.Sprintf("%s: server %q needs \"command\" or \"url\", or a global server to override", path, name))
					continue
				}
				if len(extra) > 0 {
					state.errors = append(state.errors, fmt.Sprintf("%s: server %q: an override can only set enabled, exposure, toolExposure", path, name))
					continue
				}
				merged := make(map[string]any, len(base.Config)+len(record))
				for key, entry := range base.Config {
					merged[key] = entry
				}
				for key, entry := range record {
					merged[key] = entry
				}
				encoded, _ := json.Marshal(merged)
				validated := ValidateMcpServerConfig(name, encoded, McpServerConfigOptions{})
				if validated.Kind == "invalid" {
					state.errors = append(state.errors, fmt.Sprintf("%s: %s", path, validated.Error))
					continue
				}
				copied := *base
				copied.Config = merged
				copied.Override = path
				state.servers[name] = &copied
				continue
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			state.errors = append(state.errors, fmt.Sprintf("%s: server %q: %v", path, name, err))
			continue
		}
		validated := ValidateMcpServerConfig(name, encoded, McpServerConfigOptions{})
		if validated.Kind == "invalid" {
			state.errors = append(state.errors, fmt.Sprintf("%s: %s", path, validated.Error))
			continue
		}
		clash := ""
		for other := range state.servers {
			if other != name && Namespace(other) == Namespace(name) {
				clash = other
				break
			}
		}
		if clash != "" {
			state.errors = append(state.errors, fmt.Sprintf("%s: server %q conflicts with %q", path, name, clash))
			continue
		}
		state.servers[name] = &ServerEntry{Name: name, Config: validated.Config, Source: path, Scope: scope}
	}
}

// ServerField reads a string field from a server config.
func ServerField(config map[string]any, name string) string {
	value, _ := config[name].(string)
	return value
}

// ServerBool reads a bool field, returning def when absent.
func ServerBool(config map[string]any, name string, def bool) bool {
	value, ok := config[name].(bool)
	if !ok {
		return def
	}
	return value
}
