package toolsurface

import (
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
)

// ManifestHash is the model-visible tool manifest digest. The implementation
// lives in agenttool so the kernel packages (agent) can hash an injected
// surface without importing this product-layer package; the product surface
// keeps this entry point for its own callers and tests.
func ManifestHash(tools []agentcore.AgentTool) (string, error) {
	return agenttool.ManifestHash(tools)
}

// RegistryHash is ManifestHash over every tool in a registry. A nil registry has
// the empty-manifest hash.
func RegistryHash(registry *agenttool.ToolRegistry) (string, error) {
	return agenttool.RegistryHash(registry)
}
