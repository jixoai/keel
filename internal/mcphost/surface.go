// Package mcphost is the host half of MCP Apps (SEP-1865, extension
// io.modelcontextprotocol/ui): it turns the app declarations of the connected
// MCP fleet into a machine-readable catalogue, fetches and validates each app
// document, keeps the documents in a bounded LRU cache, and publishes the
// resulting surface to the serving runtime.
//
// The package owns no HTTP and no frontend. The catalogue and the documents are
// read by internal/agentapi; the prompt half is republished through Sinks by
// the hosting process. mcphost never imports cmd/keel — the reverse is how a
// host wires it.
//
// Identity rules (design §3.1, spec「app 身份、缓存身份与目录」):
//
//   - appId is derived (mcp.AppIDFor: hash of server + NUL + resourceUri), never
//     a server-supplied string, and is the only value used in URLs and cache
//     keys;
//   - a document's identity is its content hash, and its URL carries that hash,
//     so a content change yields a new immutable URL;
//   - the catalogue is the single machine-readable source of the binding: a
//     client never recomputes it.
package mcphost

import (
	"encoding/json"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/mcp"
)

// Section is one dynamic prompt section the MCP half contributes. It mirrors
// host.Section without importing the assembly package: the hosting process
// projects it onto its own section type.
type Section struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// ServerState is one MCP server as the host reports it (GET /v1/mcp/servers).
// It is a projection of mcp.Server, never a second connection authority.
type ServerState struct {
	Name            string `json:"name"`
	Transport       string `json:"transport"`
	ProtocolVersion string `json:"protocolVersion,omitempty"`
	// Era is "legacy" or "modern" for the negotiated protocol version. The
	// era-gated transport behaviour itself is P5.4; this is the read-only
	// projection clients use to understand which dialect the server speaks.
	Era          string          `json:"era"`
	Connected    bool            `json:"connected"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	Error        string          `json:"error,omitempty"`
	// CatalogueError is the last failed tools/list refresh of a connected
	// server (P4.4): the previous tool list stays published, and the failure is
	// visible here (spec「热更的 attempt 边界与失败保面」).
	CatalogueError string   `json:"catalogueError,omitempty"`
	Reconnects     int      `json:"reconnectCount"`
	Tools          int      `json:"tools"`
	Apps           int      `json:"apps"`
	Diagnostics    []string `json:"diagnostics,omitempty"`
}

// ToolRef is one tool that references an app resource.
type ToolRef struct {
	Tool       string   `json:"tool"`
	Visibility []string `json:"visibility"`
}

// CSPMeta is the resource `_meta.ui.csp` block: the only domain lists an app
// may declare. Any other key in the block is ignored (and diagnosed): the host
// constructs every directive, so a server can never introduce a directive.
type CSPMeta struct {
	ConnectDomains  []string `json:"connectDomains,omitempty"`
	ResourceDomains []string `json:"resourceDomains,omitempty"`
	FrameDomains    []string `json:"frameDomains,omitempty"`
	BaseURIDomains  []string `json:"baseUriDomains,omitempty"`
}

// UIResourceMeta is the parsed resource `_meta.ui` block (apps.mdx §UI Resource
// Format). Invalid members are dropped with a diagnostic; the app still
// renders under the strict defaults.
type UIResourceMeta struct {
	CSP           *CSPMeta        `json:"csp,omitempty"`
	Permissions   map[string]bool `json:"permissions,omitempty"`
	Domain        string          `json:"domain,omitempty"`
	PrefersBorder *bool           `json:"prefersBorder,omitempty"`
}

// AppSpec is one renderable app in the catalogue. It is the JSON projection
// clients read (GET /v1/mcp/apps) and the input of the document route.
type AppSpec struct {
	AppID       string `json:"appId"`
	Server      string `json:"server"`
	Title       string `json:"title,omitempty"`
	ResourceURI string `json:"resourceUri"`
	MIMEType    string `json:"mimeType"`
	// Visibility is the union across Tools, for clients that need one value.
	// The exact per-tool visibility is in Tools.
	Visibility []string  `json:"visibility"`
	Tools      []ToolRef `json:"tools"`
	CSP        string    `json:"csp"`
	// CSPDomains is the validated declared-domain block the header was built
	// from (audit/transparency: what the server asked for, after dropping).
	CSPDomains    *CSPMeta        `json:"cspDomains,omitempty"`
	Permissions   map[string]bool `json:"permissions"`
	PrefersBorder *bool           `json:"prefersBorder,omitempty"`
	Hash          string          `json:"hash"`
	FetchedAt     time.Time       `json:"fetchedAt"`
	TTLMs         *int64          `json:"ttlMs,omitempty"`
	// Diagnostics are the non-fatal notes of this app's metadata (dropped CSP
	// domains, unknown permissions, deprecated keys).
	Diagnostics []string `json:"diagnostics,omitempty"`
	// DocumentURL is the immutable, hash-named document URL. It is built here,
	// once, so the client never recomputes the binding.
	DocumentURL string `json:"documentUrl"`
}

// ResourceSpec is one MCP resource the host knows the size and metadata of. In
// P2 the catalogue is built from the app documents the host actually read;
// resources/list prefetch is P3.
type ResourceSpec struct {
	Server   string         `json:"server"`
	URI      string         `json:"uri"`
	Name     string         `json:"name,omitempty"`
	MIMEType string         `json:"mimeType"`
	Size     int64          `json:"size"`
	Meta     UIResourceMeta `json:"meta"`
}

// Surface is the complete declaration the MCP fleet publishes to the host. It
// is republished wholesale on every change (assembly semantics), never mutated
// in place: readers may keep the value they got.
type Surface struct {
	Generation int64
	Servers    []ServerState
	Tools      []agentcore.AgentTool
	Resources  []ResourceSpec
	Apps       []AppSpec
	Prompt     []Section
}

// Sinks are the host's registration points for the published surface. Every
// sink is optional (nil = this deployment does not consume that half).
//
// The registry publishes its own state first and invokes the sinks afterwards,
// so a sink may call Registry.Surface() and read exactly what it is being told
// about. Changed carries the whole republished surface for a host that composes
// one merged republish (MCP half + plugin half).
type Sinks struct {
	Tools     func([]agentcore.AgentTool) error
	Prompt    func([]Section) error
	Apps      func([]AppSpec) error
	Resources func([]ResourceSpec) error
	Changed   func(Surface)
}

// appEntry is the registry's record of one published app.
type appEntry struct {
	spec  AppSpec
	docID string
}

// Document is one validated app document, identified by content hash. Its
// bytes are immutable: a new version is a new Document value.
type Document struct {
	AppID     string
	Server    string
	Hash      string
	MIMEType  string
	CSP       string
	Bytes     []byte
	FetchedAt time.Time
}

// documentKey is the cache identity of one document version: the app identity
// plus the content hash. The hash is what makes the key content-addressed and
// safe to use as an immutable cache identity.
func documentKey(appID, hash string) string { return appID + "\x00" + hash }

// documentURL builds the immutable, hash-named view URL. appID is derived
// (mcpapp_ + hex) and the hash is hex, so no escaping is involved.
func documentURL(appID, hash string) string {
	return "/v1/mcp/apps/" + appID + "/view?v=" + hash
}

// eraOf maps a negotiated protocol version onto the dialect name. ISO dates
// compare lexicographically. The era-gated behaviour is P5.4; until then every
// server the client can negotiate with reports its honest dialect.
func eraOf(protocolVersion string) string {
	if protocolVersion >= "2026-07-28" {
		return "modern"
	}
	return "legacy"
}

// transportName is the configured transport of one server, as the host reports
// it: a url entry is Streamable HTTP, a command entry is stdio.
func transportName(entry mcp.ServerEntry) string {
	if mcp.ServerField(entry.Config, "url") != "" {
		return "streamable-http"
	}
	return "stdio"
}
