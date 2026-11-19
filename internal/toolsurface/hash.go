package toolsurface

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
)

// manifestEntry is one tool's model-visible identity: the exact tuple a
// provider request exposes to the model.
type manifestEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// ManifestHash returns a stable hash of the model-visible tool manifest. Tools
// are sorted by name and schemas are canonicalized (decoded and re-encoded), so
// whitespace or registration order do not move the hash. It is the value a run
// snapshot can record to answer "did this attempt see new tools?".
//
// Dynamic capability (MCP servers, skills, tasks) must not change this hash: it
// only moves when the set of tool names/descriptions/schemas changes, which is
// exactly an installed plugin's provides.tools decision.
func ManifestHash(tools []agentcore.AgentTool) (string, error) {
	entries := make([]manifestEntry, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			return "", fmt.Errorf("toolsurface: nil tool in manifest")
		}
		schema := tool.Schema()
		if len(schema) > 0 {
			var value any
			decoder := json.NewDecoder(bytes.NewReader(schema))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				return "", fmt.Errorf("toolsurface: tool %q schema: %w", tool.Name(), err)
			}
			canonical, err := json.Marshal(value)
			if err != nil {
				return "", fmt.Errorf("toolsurface: tool %q schema: %w", tool.Name(), err)
			}
			schema = canonical
		}
		entries = append(entries, manifestEntry{
			Name:        tool.Name(),
			Description: tool.Description(),
			Schema:      schema,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("toolsurface: encode manifest: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// RegistryHash is ManifestHash over every tool in a registry. A nil registry has
// the empty-manifest hash.
func RegistryHash(registry *agenttool.ToolRegistry) (string, error) {
	if registry == nil {
		return ManifestHash(nil)
	}
	return ManifestHash(registry.List())
}
