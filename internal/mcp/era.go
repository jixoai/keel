// Protocol era support (change feat-mcp-apps-events, P5.4).
//
// The MCP 2026-07-28 revision removed the initialize handshake and the
// protocol session, moved transport metadata into every request body's `_meta`,
// and added `server/discover`. The client must keep speaking the 2025-11-25
// dialect byte-for-byte (the PI conformance baseline) while also being able to
// reach a modern server. The era is the one gate that decides which dialect a
// connection uses:
//
//   - legacy: the pre-P5 client, unchanged (initialize + notifications/
//     initialized + Streamable HTTP session + GET stream + Last-Event-ID).
//   - modern: no handshake, no session, no GET stream; `server/discover` at
//     connect; `_meta`, Mcp-Method/Mcp-Name and MCP-Protocol-Version on every
//     request.
//   - auto: probe `server/discover` in the modern shape first and fall back to
//     the legacy handshake when the server does not understand it (the
//     compatibility algorithm of the 2026-07-28 changelog).
//
// The era only changes wire shape. It never changes the tool surface: the
// catalogue, the aggregate `mcp` tool and the app bindings are identical on
// both eras.

package mcp

import "strings"

// ModernProtocolVersion is the 2026-07-28 revision: the stateless, MRTR and
// subscriptions/listen dialect. LatestProtocolVersion stays 2025-11-25 because
// it is the initialize request's default (the PI baseline).
const ModernProtocolVersion = "2026-07-28"

// ProtocolEra selects one wire dialect.
type ProtocolEra string

const (
	// EraAuto probes the modern dialect and falls back to legacy. It is the
	// deployment default (KEEL_MCP_PROTOCOL_ERA, missing or "auto").
	EraAuto ProtocolEra = "auto"
	// EraLegacy is the 2025-11-25 dialect: initialize, session, GET stream.
	EraLegacy ProtocolEra = "legacy"
	// EraModern is the 2026-07-28 dialect: stateless requests with `_meta`.
	EraModern ProtocolEra = "modern"
)

// ParseProtocolEra validates one configured era. The second result is false
// for an unknown value; callers fail closed to legacy instead of guessing.
func ParseProtocolEra(value string) (ProtocolEra, bool) {
	switch ProtocolEra(strings.TrimSpace(strings.ToLower(value))) {
	case EraAuto:
		return EraAuto, true
	case EraLegacy:
		return EraLegacy, true
	case EraModern:
		return EraModern, true
	}
	return EraLegacy, false
}

// EraFromEnvironment reads KEEL_MCP_PROTOCOL_ERA through getenv. A missing value
// is auto (the deployment default); an unknown value fails closed to legacy,
// the dialect that cannot surprise a legacy server. It never panics: an invalid
// configuration degrades, it does not crash the process.
func EraFromEnvironment(getenv func(string) string) ProtocolEra {
	if getenv == nil {
		return EraAuto
	}
	raw := strings.TrimSpace(getenv("KEEL_MCP_PROTOCOL_ERA"))
	if raw == "" {
		return EraAuto
	}
	era, ok := ParseProtocolEra(raw)
	if !ok {
		return EraLegacy
	}
	return era
}

// EraOfProtocolVersion maps a negotiated version onto the dialect it speaks.
// ISO dates compare lexicographically.
func EraOfProtocolVersion(version string) ProtocolEra {
	if version >= ModernProtocolVersion {
		return EraModern
	}
	return EraLegacy
}

// IsModernProtocolVersion reports whether a version is the stateless dialect.
func IsModernProtocolVersion(version string) bool {
	return version >= ModernProtocolVersion
}

// Modern `_meta` keys (2026-07-28 basic/transports).
const (
	MetaProtocolVersionKey    = "io.modelcontextprotocol/protocolVersion"
	MetaClientCapabilitiesKey = "io.modelcontextprotocol/clientCapabilities"
	MetaClientInfoKey         = "io.modelcontextprotocol/clientInfo"
)

// CodeUnsupportedProtocolVersion is the 2026-07-28 error code a modern server
// uses to reject a protocol version it does not serve. The probe treats it as
// proof that the server speaks the modern dialect (the compatibility algorithm
// then retries with a supported version).
const CodeUnsupportedProtocolVersion = -32022
