// Package spill is the keel.spill capability contract: the policy and text
// rendering for "long content leaves the context, a durable reference stays".
//
// The package is deliberately pure. It defines what a spilled item is and how a
// bounded reference is rendered; it does not know where the bytes live. The
// production implementation is internal/spillblob (the tenant blob store), and
// the compaction package consumes the Store interface below. Keeping the
// contract free of storage imports is what lets the agent loop depend on the
// capability without reaching the database (see internal/agentloop/boundary_test.go).
//
// The reference text is a contract, not decoration: a pruned tool result or an
// offloaded image must stay recoverable by the identifier it names. Every
// renderer therefore carries the durable id, the content hash, the byte size
// and an explicit recovery hint, and the preview is bounded by a caller-chosen
// budget.
package spill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// PluginID is the built-in plugin that owns this capability.
const PluginID = "keel.spill"

// Capability identifiers. They are backend capability names (like
// keel.file.read), not model-facing tool names: the model-facing surface stays
// the fixed eleven aggregated tools.
const (
	// CapabilitySpill writes a long payload and returns its durable reference.
	CapabilitySpill = "keel.spill.text"
	// CapabilityLoad reads a spilled payload back by reference.
	CapabilityLoad = "keel.spill.load"
)

// Defaults for the spill policy. They are intentionally conservative: a tool
// result smaller than a few KiB is cheaper to keep than to round-trip through
// storage, and a preview larger than half a KiB defeats the purpose.
const (
	DefaultMinBytes     = 4096
	DefaultPreviewBytes = 512
)

// Scope names the durable owner of a spilled payload. TenantID scopes the blob
// table; SessionID lets a future store attribute the reference without a second
// lookup. Empty values are rejected by implementations, not silently zeroed.
type Scope struct {
	TenantID  string
	SessionID string
}

// Ref is the durable identity of one spilled payload, as it appears in the
// context reference. ID is the store's public handle (a blob id for the blob
// implementation); SHA256 is the lowercase hex digest of the exact bytes;
// Size is their length; MIME names the stored representation.
type Ref struct {
	ID     string
	MIME   string
	SHA256 string
	Size   int64
}

// Store is the storage seam of the capability. Spill writes the exact bytes and
// returns their identity; Load reads them back. Both methods are idempotent for
// the same content and scope (content-addressed stores dedupe; a test double
// must too), because memory hygiene re-runs on every turn over the same
// transcript.
type Store interface {
	Spill(ctx context.Context, scope Scope, mime string, data []byte) (Ref, error)
	Load(ctx context.Context, scope Scope, ref Ref) ([]byte, error)
}

// Policy bounds what is spilled and how much preview stays in context.
type Policy struct {
	// MinBytes is the smallest payload worth spilling. Non-positive disables
	// spilling (the caller keeps the original content; nothing is lost).
	MinBytes int
	// PreviewBytes caps the preview embedded in the reference text. A
	// non-positive value renders no preview.
	PreviewBytes int
}

// DefaultPolicy is the policy the memory-hygiene path applies when the caller
// does not override it.
func DefaultPolicy() Policy {
	return Policy{MinBytes: DefaultMinBytes, PreviewBytes: DefaultPreviewBytes}
}

// ShouldSpill reports whether a payload of size bytes crosses the policy.
func (p Policy) ShouldSpill(size int) bool {
	return p.MinBytes > 0 && size >= p.MinBytes
}

// SpillBytes writes data through store and renders the bounded reference that
// replaces it in the context. The returned Ref is the durable identity a reader
// uses to recover the exact bytes; the returned string is what the model sees.
//
// A nil store is a wiring gap, not a reason to drop data: it returns an error
// and the caller keeps the original content.
func SpillBytes(ctx context.Context, store Store, scope Scope, mime, label string, data []byte, policy Policy) (Ref, string, error) {
	if store == nil {
		return Ref{}, "", fmt.Errorf("spill: no durable store is wired")
	}
	ref, err := store.Spill(ctx, scope, mime, data)
	if err != nil {
		return Ref{}, "", err
	}
	return ref, RenderReference(ref, label, string(data), policy.PreviewBytes), nil
}

// SpillText is SpillBytes for UTF-8 text.
func SpillText(ctx context.Context, store Store, scope Scope, label, text string, policy Policy) (Ref, string, error) {
	return SpillBytes(ctx, store, scope, "text/plain; charset=utf-8", label, []byte(text), policy)
}

// ReferencePrefix marks a bounded reference this package rendered. Callers use
// it to keep the operation idempotent: a message that already carries the
// prefix has already been spilled and must not be spilled twice.
const ReferencePrefix = "[keel.spill]"

// IsReference reports whether text is already a spill reference.
func IsReference(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), ReferencePrefix)
}

// RenderReference renders the bounded text that stands in for a spilled
// payload. It names the durable id, hash and size so recovery is a lookup, not
// a guess, and includes at most previewBytes of the original content.
func RenderReference(ref Ref, label, content string, previewBytes int) string {
	var b strings.Builder
	b.WriteString(ReferencePrefix)
	b.WriteString(" omitted from context: ")
	if label = strings.TrimSpace(label); label != "" {
		b.WriteString(label)
		b.WriteString("; ")
	}
	fmt.Fprintf(&b, "stored %d bytes (mime=%s, sha256=%s, ref=%s)", ref.Size, ref.MIME, ref.SHA256, ref.ID)
	b.WriteString(". The full content is durable and recoverable by ref.")
	if previewBytes > 0 {
		preview := TruncateUTF8(content, previewBytes)
		if preview != "" {
			b.WriteString("\nPreview (first ")
			fmt.Fprintf(&b, "%d bytes):\n", len(preview))
			b.WriteString(preview)
			if len(preview) < len(content) {
				b.WriteString("\n…")
			}
		}
	}
	return b.String()
}

// TruncateUTF8 returns at most maxBytes of s without splitting a rune. It is
// exported because both the spill renderer and the image-offload renderer
// bound their previews with the same rule.
func TruncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		if maxBytes <= 0 {
			return ""
		}
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Digest returns the lowercase hex sha256 of data. It is the same identity the
// blob store records, exposed here so a caller can verify a loaded payload
// without importing the storage implementation.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
