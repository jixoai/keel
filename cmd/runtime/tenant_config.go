// The runtime's hosting-side adapters onto the single tenant-configuration
// vocabulary (internal/host). Everything here is a pure mapping: environment
// defaults → host.TenantConfig, the stored tenant document → the quota
// resolver, the loaded mcp.json entries → the document's server list, and the
// boot-time external provider file → the document's external section.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/store"
)

// externalConfigFromFile loads the boot-time external provider declarations.
// The file is a tenant document (the exact host.TenantConfig JSON shape; a
// {"external": {...}} fragment is the common case), so no second configuration
// vocabulary exists. An empty path means "no external providers".
//
// The document is only parsed here; host.TenantConfig.Validate runs during
// loadout resolution, so a bad endpoint or a missing unmetered policy fails
// boot with the same message an administrator would see on the admin API.
func externalConfigFromFile(path string) (host.ExternalConfig, error) {
	if strings.TrimSpace(path) == "" {
		return host.ExternalConfig{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return host.ExternalConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	doc, err := host.ParseTenantConfig(raw)
	if err != nil {
		return host.ExternalConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	return doc.External, nil
}

// tenantDocumentSource is the quota package's DocumentSource over the admin
// API's tenant row. A tenant without a stored document answers "{}" (no
// override), so the hosting default applies; any other read failure surfaces
// and the admission seam fails closed.
type tenantDocumentSource struct {
	store *store.Store
}

func (s tenantDocumentSource) TenantDocument(ctx context.Context, tenantID uuid.UUID) ([]byte, int64, error) {
	if s.store == nil {
		return []byte("{}"), 1, nil
	}
	record, err := s.store.GetTenantConfig(ctx, tenantID)
	if errors.Is(err, store.ErrNoTenant) {
		return []byte("{}"), 1, nil
	}
	if err != nil {
		return nil, 0, err
	}
	return record.Config, record.Revision, nil
}

// runMirror is the narrow read-only view the services may hold. It is a
// concrete wrapper (not the store itself) so no service binding carries a
// *store.Store that could be type-asserted back into a write handle.
type runMirror struct {
	store *store.Store
}

func (m runMirror) GetRun(ctx context.Context, tenantID, sessionID uuid.UUID) (store.Run, error) {
	return m.store.GetRun(ctx, tenantID, sessionID)
}

// promptSections maps the KEEL_SYSTEM_SECTIONS view onto the document's
// sections. The two shapes are structurally identical; only the document type
// is allowed to travel into a loadout.
func promptSections(sections []host.Section) []host.PromptSection {
	if len(sections) == 0 {
		return nil
	}
	out := make([]host.PromptSection, 0, len(sections))
	for _, section := range sections {
		out = append(out, host.PromptSection{Name: section.Name, Content: section.Content})
	}
	return out
}

// mcpServersFromEntries names the configured MCP servers in the tenant
// document. The document is the enablement/metadata source (loadout selection);
// the connection parameters stay with the loaded mcp.json entry, which the mcp
// service consumes directly. Credential-bearing fields (env/headers) never enter
// the document: they would have to be secret references there, while mcp.json
// resolves them itself.
func mcpServersFromEntries(entries []mcp.ServerEntry) map[string]host.MCPServerConfig {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]host.MCPServerConfig, len(entries))
	for _, entry := range entries {
		server := host.MCPServerConfig{
			Command: mcp.ServerField(entry.Config, "command"),
			URL:     mcp.ServerField(entry.Config, "url"),
		}
		if server.Command == "" && server.URL == "" {
			continue
		}
		if timeout, ok := entry.Config["timeout"].(float64); ok {
			server.Timeout = int(timeout)
		}
		out[entry.Name] = server
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// telemetryAdapter maps the legacy KEEL_TELEMETRY values onto the document's
// adapter name. Only the implemented adapter is selected; anything else leaves
// telemetry disabled exactly as before.
func telemetryAdapter(raw string) string {
	if raw == "memory" {
		return "memory"
	}
	return ""
}
