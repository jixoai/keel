package compaction

// Memory hygiene (keel.compaction extension): the tool-result pruner and the
// image offload.
//
// Context compaction summarizes an old *prefix* once the window is nearly full.
// Two cheaper measures keep the context small before that point:
//
//   - tool-result-pruner: a large tool result outside the recent window is
//     written to the durable spill store and replaced by a bounded reference
//     (ref id, content hash, byte size, preview). The original bytes stay
//     recoverable by reference; the model keeps a summary, not the payload.
//   - image-offload: an image reference outside the recent window is replaced
//     by a text reference naming its durable blob id, so the provider is not
//     asked to upload/send bytes no turn is likely to need.
//
// Both operate on the *request view* only. The committed transcript is never
// rewritten: the runner prunes its in-memory rebuild and the loop prunes its
// in-memory context, while every MessageEnd/tool result event already on the
// ledger keeps the original content. This is the same contract compaction
// already follows (the summary stands in for a prefix; the prefix events stay).
//
// Failure semantics follow the change's rule: a spill failure leaves the
// original message untouched and is reported, never silently dropped. A
// reference is emitted only after the store confirmed the write.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/spill"
)

// Default hygiene bounds. They are deliberately conservative: 4KiB is the
// point where a tool result costs more context than the round-trip through
// storage, and a 512-byte preview keeps the model oriented without moving the
// payload back in.
const (
	DefaultHygieneKeepRecentTokens = 20000
	DefaultPruneMinBytes           = spill.DefaultMinBytes
	DefaultPrunePreviewBytes       = spill.DefaultPreviewBytes
)

// ImageReferencePrefix marks the text an offloaded image reference becomes.
// Callers can use it to recognize an already-offloaded block.
const ImageReferencePrefix = "[keel.image-offload]"

// ToolResultPayloadMIME is the stored representation of a pruned tool result:
// the marshalled agentcore.ToolResultMessage, so recovery returns the exact
// message (call id, name, content blocks, error flag, duration, timestamp).
const ToolResultPayloadMIME = "application/vnd.keel.tool-result+json"

// HygieneSettings bounds the two hygiene passes. Zero values disable the
// corresponding pass; the production wiring fills them from
// DefaultHygieneSettings so a transport that forgets the field degrades to
// "no hygiene", never to "prune everything".
type HygieneSettings struct {
	// Enabled is the master switch. The loop additionally requires compaction
	// to be enabled: hygiene is a keel.compaction extension, not a second
	// memory subsystem.
	Enabled bool
	// KeepRecentTokens is the verbatim tail the pruner protects. A tool result
	// whose message starts before this tail is eligible; a non-positive value
	// disables pruning.
	KeepRecentTokens int
	// MinToolResultBytes is the size at which a text-only tool result is worth
	// spilling. Non-positive disables pruning.
	MinToolResultBytes int
	// PreviewBytes caps the preview embedded in a prune reference.
	PreviewBytes int
	// OffloadImages enables the image offload. ImageRecentTokens is its own
	// window (images are far more expensive per block than text); non-positive
	// falls back to KeepRecentTokens.
	OffloadImages     bool
	ImageRecentTokens int
}

// DefaultHygieneSettings returns the production defaults.
func DefaultHygieneSettings() HygieneSettings {
	return HygieneSettings{
		Enabled:            true,
		KeepRecentTokens:   DefaultHygieneKeepRecentTokens,
		MinToolResultBytes: DefaultPruneMinBytes,
		PreviewBytes:       DefaultPrunePreviewBytes,
		OffloadImages:      true,
		ImageRecentTokens:  DefaultHygieneKeepRecentTokens,
	}
}

// PrunedToolResult records one pruner decision.
type PrunedToolResult struct {
	// MessageIndex is the index in the input list.
	MessageIndex int
	ToolCallID   string
	ToolName     string
	Ref          spill.Ref
	// OriginalBytes is the size of the stored ToolResultMessage JSON.
	OriginalBytes int
	// OriginalTokens and ReferenceTokens are the estimate for the message
	// before and after the replacement.
	OriginalTokens  int
	ReferenceTokens int
}

// OffloadedImage records one image reference replaced by text.
type OffloadedImage struct {
	MessageIndex int
	BlobID       string
	MIME         string
	Size         int64
	Name         string
	TokensBefore int
	TokensAfter  int
}

// HygieneFailure records a message the pass could not rewrite. The original
// message was kept verbatim; the failure is observable rather than silent.
type HygieneFailure struct {
	MessageIndex int
	Kind         string
	Reason       string
}

// HygieneReport is the outcome of one hygiene pass.
type HygieneReport struct {
	TokensBefore int
	TokensAfter  int
	Pruned       []PrunedToolResult
	Offloaded    []OffloadedImage
	Failures     []HygieneFailure
}

// Changed reports whether any message was rewritten.
func (r HygieneReport) Changed() bool {
	return len(r.Pruned) > 0 || len(r.Offloaded) > 0
}

// ApplyHygiene runs both passes over the request view and returns the rewritten
// list (the input slice is returned unchanged when nothing qualified). A nil
// store disables the pruner but not the image offload: image references are
// already durable and need no write.
func ApplyHygiene(ctx context.Context, msgs agentcore.MessageList, settings HygieneSettings, store spill.Store, scope spill.Scope) (agentcore.MessageList, HygieneReport) {
	report := HygieneReport{TokensBefore: EstimateContextTokens(msgs).Tokens}
	if !settings.Enabled {
		report.TokensAfter = report.TokensBefore
		return msgs, report
	}
	out := msgs
	if settings.MinToolResultBytes > 0 && settings.KeepRecentTokens > 0 && store != nil {
		out, report.Pruned, report.Failures = pruneToolResults(ctx, out, settings, store, scope)
	}
	if settings.OffloadImages {
		var offloaded []OffloadedImage
		var failures []HygieneFailure
		out, offloaded, failures = offloadImages(out, settings)
		report.Offloaded = offloaded
		report.Failures = append(report.Failures, failures...)
	}
	report.TokensAfter = EstimateContextTokens(out).Tokens
	return out, report
}

// pruneToolResults replaces eligible text-only tool results with a bounded
// reference and returns the rewritten list plus the decisions.
func pruneToolResults(ctx context.Context, msgs agentcore.MessageList, settings HygieneSettings, store spill.Store, scope spill.Scope) (agentcore.MessageList, []PrunedToolResult, []HygieneFailure) {
	start := recentWindowStart(msgs, settings.KeepRecentTokens)
	if start <= 0 {
		return msgs, nil, nil
	}
	previewBytes := settings.PreviewBytes
	if previewBytes <= 0 {
		previewBytes = DefaultPrunePreviewBytes
	}
	var pruned []PrunedToolResult
	var failures []HygieneFailure
	out := msgs
	copied := false
	ensureCopy := func() {
		if copied {
			return
		}
		out = make(agentcore.MessageList, len(msgs))
		copy(out, msgs)
		copied = true
	}
	for i := 0; i < start; i++ {
		tr, ok := msgs[i].(agentcore.ToolResultMessage)
		if !ok {
			continue
		}
		text, ok := toolResultText(tr)
		if !ok || len(text) < settings.MinToolResultBytes || spill.IsReference(text) {
			continue
		}
		payload, err := json.Marshal(tr)
		if err != nil {
			failures = append(failures, HygieneFailure{
				MessageIndex: i,
				Kind:         "tool_result_prune",
				Reason:       fmt.Sprintf("marshal tool result: %v", err),
			})
			continue
		}
		label := fmt.Sprintf("tool result %q (call %s)", tr.ToolName, tr.ToolCallID)
		ref, reference, err := spill.SpillBytes(ctx, store, scope, ToolResultPayloadMIME, label, payload, spill.Policy{
			MinBytes:     settings.MinToolResultBytes,
			PreviewBytes: previewBytes,
		})
		if err != nil {
			failures = append(failures, HygieneFailure{
				MessageIndex: i,
				Kind:         "tool_result_prune",
				Reason:       fmt.Sprintf("spill tool result: %v", err),
			})
			continue
		}
		if ref.ID == "" || ref.SHA256 == "" || ref.Size <= 0 {
			// A store that returns a non-reference cannot be trusted with the
			// payload: keep the original content rather than render a reference
			// that may not resolve.
			failures = append(failures, HygieneFailure{
				MessageIndex: i,
				Kind:         "tool_result_prune",
				Reason:       "spill store returned an incomplete reference",
			})
			continue
		}
		replacement := tr
		replacement.Content = agentcore.ContentList{agentcore.NewTextContent(reference)}
		originalTokens := EstimateTokens(tr)
		referenceTokens := EstimateTokens(replacement)
		ensureCopy()
		out[i] = replacement
		pruned = append(pruned, PrunedToolResult{
			MessageIndex:    i,
			ToolCallID:      tr.ToolCallID,
			ToolName:        tr.ToolName,
			Ref:             ref,
			OriginalBytes:   len(payload),
			OriginalTokens:  originalTokens,
			ReferenceTokens: referenceTokens,
		})
	}
	return out, pruned, failures
}

// offloadImages replaces every image reference outside the recent window with
// a text reference naming its durable blob. A reference without a blob id is
// not recoverable, so it is reported and left in place.
func offloadImages(msgs agentcore.MessageList, settings HygieneSettings) (agentcore.MessageList, []OffloadedImage, []HygieneFailure) {
	window := settings.ImageRecentTokens
	if window <= 0 {
		window = settings.KeepRecentTokens
	}
	if window <= 0 {
		return msgs, nil, nil
	}
	start := recentWindowStart(msgs, window)
	if start <= 0 {
		return msgs, nil, nil
	}
	var offloaded []OffloadedImage
	var failures []HygieneFailure
	out := msgs
	copied := false
	for i := 0; i < start; i++ {
		content := messageContentForHygiene(msgs[i])
		if content == nil {
			continue
		}
		replaced := false
		next := make(agentcore.ContentList, 0, len(content))
		for _, block := range content {
			ref, ok := block.(agentcore.ImageRefContent)
			if !ok {
				next = append(next, block)
				continue
			}
			if strings.TrimSpace(ref.BlobID) == "" {
				failures = append(failures, HygieneFailure{
					MessageIndex: i,
					Kind:         "image_offload",
					Reason:       "image reference carries no blob id; leaving it in place to avoid an unrecoverable drop",
				})
				next = append(next, block)
				continue
			}
			replaced = true
			next = append(next, agentcore.NewTextContent(RenderImageReference(ref)))
			offloaded = append(offloaded, OffloadedImage{
				MessageIndex: i,
				BlobID:       ref.BlobID,
				MIME:         ref.MimeType,
				Size:         ref.Size,
				Name:         ref.Name,
				TokensBefore: EstimateTokens(msgs[i]),
				TokensAfter:  estimateTextTokens(RenderImageReference(ref)),
			})
		}
		if !replaced {
			continue
		}
		if !copied {
			out = make(agentcore.MessageList, len(msgs))
			copy(out, msgs)
			copied = true
		}
		out[i] = replaceContentForHygiene(msgs[i], next)
	}
	return out, offloaded, failures
}

// RenderImageReference renders the bounded text an offloaded image becomes. It
// names the blob id, MIME and byte size — the durable facts — and states
// explicitly that the bytes were not sent, so a reader can tell an offloaded
// image from a missing one.
func RenderImageReference(ref agentcore.ImageRefContent) string {
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		name = ref.BlobID
	}
	facts := name
	if mime := strings.TrimSpace(ref.MimeType); mime != "" {
		facts += ", " + mime
	}
	if ref.Size > 0 {
		facts += fmt.Sprintf(", %d bytes", ref.Size)
	}
	if ref.Width > 0 && ref.Height > 0 {
		facts += fmt.Sprintf(", %dx%d", ref.Width, ref.Height)
	}
	return fmt.Sprintf("%s image %s is outside the recent window; its bytes were not sent with this request. "+
		"It is stored durably as blobId=%s and remains recoverable by that reference.",
		ImageReferencePrefix, facts, ref.BlobID)
}

// recentWindowStart returns the index of the first message inside the retained
// tail: messages before it are eligible for rewriting. The estimate walks from
// the end and stops when adding the next message would exceed keepTokens; a
// non-positive budget means nothing is protected. The newest message is always
// protected, even when it alone exceeds the budget: it is the turn's working
// set.
func recentWindowStart(msgs agentcore.MessageList, keepTokens int) int {
	if keepTokens <= 0 || len(msgs) == 0 {
		return 0
	}
	total := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		total += EstimateTokens(msgs[i])
		if total > keepTokens {
			if i == len(msgs)-1 {
				return i
			}
			return i + 1
		}
	}
	return 0
}

// toolResultText returns the concatenated text of a text-only tool result. A
// result carrying any non-text block is not eligible: spilling only part of a
// message would make the reference describe content it does not own.
func toolResultText(tr agentcore.ToolResultMessage) (string, bool) {
	if len(tr.Content) == 0 {
		return "", false
	}
	var parts []string
	for _, block := range tr.Content {
		text, ok := block.(agentcore.TextContent)
		if !ok {
			return "", false
		}
		parts = append(parts, text.Text)
	}
	return strings.Join(parts, "\n"), true
}

// messageContentForHygiene returns the content list of a message that carries
// one; system and compaction messages carry none.
func messageContentForHygiene(msg agentcore.Message) agentcore.ContentList {
	switch m := msg.(type) {
	case agentcore.UserMessage:
		return m.Content
	case agentcore.AssistantMessage:
		return m.Content
	case agentcore.ToolResultMessage:
		return m.Content
	}
	return nil
}

// replaceContentForHygiene returns the message with a new content list,
// preserving every other field. An unknown message kind is returned unchanged.
func replaceContentForHygiene(msg agentcore.Message, content agentcore.ContentList) agentcore.Message {
	switch m := msg.(type) {
	case agentcore.UserMessage:
		m.Content = content
		return m
	case agentcore.AssistantMessage:
		m.Content = content
		return m
	case agentcore.ToolResultMessage:
		m.Content = content
		return m
	default:
		return msg
	}
}

// estimateTextTokens prices a plain text block for the offload report.
func estimateTextTokens(text string) int {
	return estimateCharsTokens(len(text))
}
