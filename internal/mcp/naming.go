// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/naming.ts (MIT License); implemented independently in Go.
// Contract kept identical; see docs/UPSTREAM.md (PI is the interface baseline).
//
// Package mcp implements the pure-function parts of the Model Context Protocol
// client contract keel shares with pi: tool naming, input-schema normalization,
// tool-result conversion, and output limiting. It performs no I/O and depends
// only on the standard library and internal/agentcore.
//
// Names of MCP tools and of their servers' namespaces as models and code mode
// scripts see them. Applications that name tools the same way rank and group
// them the same way in tool search and code mode.
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf16"
)

// MaxToolNameLength is the provider limit for tool names: 64 characters of
// [A-Za-z0-9_-]. Mirrors MAX_TOOL_NAME_LENGTH in pi (naming.ts:10).
const MaxToolNameLength = 64

// CreateToolName returns the model-facing name of an MCP tool:
// mcp__<server>__<tool>, sanitized and shortened with a hash suffix when too
// long. Like Codex, everything but [A-Za-z0-9_] becomes "_", so the name is
// also the identifier code mode scripts call it by. isTaken reports names used
// by a different MCP tool: sanitizing can map two tools to one name ("a-b" and
// "a_b"), which then get the hash suffix. A nil isTaken means "nothing taken".
//
// The hash covers the unsanitized server and tool names, so distinct tools that
// sanitize to the same string still get distinct names.
func CreateToolName(server, tool string, isTaken func(string) bool) string {
	name := sanitizeToolName("mcp__" + server + "__" + tool)
	if len(name) <= MaxToolNameLength && (isTaken == nil || !isTaken(name)) {
		return name
	}
	sum := sha256.Sum256([]byte(server + "\x00" + tool))
	hash := hex.EncodeToString(sum[:])[:8]
	// JS String.slice clamps past the end, unlike Go slicing: a short but taken
	// name must survive intact before the suffix is appended.
	prefix := MaxToolNameLength - len(hash) - 1
	if prefix > len(name) {
		prefix = len(name)
	}
	return name[:prefix] + "_" + hash
}

// Namespace returns the namespace of a server's tools: mcp__<server> with "-"
// replaced by "_", like the tool names. Server names that differ only in "-"
// and "_" share a namespace, so a configuration must not have both.
func Namespace(server string) string {
	return "mcp__" + strings.ReplaceAll(server, "-", "_")
}

// sanitizeToolName replaces every character outside [A-Za-z0-9_] with "_".
// pi's /[^A-Za-z0-9_]/g works on UTF-16 code units, so an astral-plane rune
// yields two underscores there; iterating UTF-16 here reproduces that exactly.
func sanitizeToolName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, unit := range utf16.Encode([]rune(s)) {
		switch {
		case unit >= 'A' && unit <= 'Z',
			unit >= 'a' && unit <= 'z',
			unit >= '0' && unit <= '9',
			unit == '_':
			b.WriteByte(byte(unit))
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
