package agent

// Outbound image resolution (keel-ga §3 provider-files).
//
// The transcript rebuild produces ImageRefContent blocks (durable references);
// the provider wire wants bytes, a URL or a file handle. This file is the one
// place that crosses that boundary, immediately before the request:
//
//	transcript (imageRef) ──▶ resolve ──▶ wire content (image: file_id/url/base64)
//
// Two properties are load-bearing:
//
//  1. Resolution is per attempt, not cached in the transcript. The blob is the
//     durable fact; the wire form depends on the provider, its capabilities and
//     the handle cache state at this moment (a Gemini handle past 48h must be
//     re-uploaded, and a different model in a later turn may want a different
//     form entirely).
//  2. A failure never silently drops the image. The block becomes a text notice
//     naming the reason and the run continues: the surrounding prompt still
//     reaches the model, and the operator sees exactly which attachment was
//     unusable. The alternative — aborting the run — makes one bad attachment
//     take the whole conversation down; the alternative of dropping it silently
//     is the failure mode this change exists to remove.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
)

// ImageResolver resolves durable image references into the wire form one
// provider accepts. provider.ImageResolver implements it; the interface exists so
// the runner depends on the behavior, not on the concrete resolver's
// configuration (and so a test can supply one without building uploader stacks).
type ImageResolver interface {
	Resolve(ctx context.Context, workspace string, target provider.ImageTarget, ref agentcore.ImageRefContent, caps provider.ImageCaps) (provider.ResolvedImage, error)
}

// imageResolution carries the per-attempt inputs of outbound image resolution.
type imageResolution struct {
	resolver  ImageResolver
	workspace string
	target    provider.ImageTarget
	caps      provider.ImageCaps
	// model is the resolved model's capability fact sheet. A model that declares
	// no image input never causes bytes to be uploaded or inlined: the reference
	// becomes the same metadata text an unsupported attachment gets, so the model
	// is told about the image without a native part its declaration denies.
	model *ModelProperties
}

// resolveImages rewrites every ImageRefContent in the transcript into a wire
// image block, in place order, reporting what happened. The returned list is a
// copy: the caller's messages (which mirror the loaded transcript) must not be
// mutated, because a later step re-reads them.
//
// A nil resolver is not an error: the references are then rendered as text
// notices with no network/IO, which is the honest answer for a deployment whose
// image pipeline is not wired (a kernel-only host, or the fake-provider path).
// The runner logs one warn per such attempt.
func (r *Runner) resolveImages(ctx context.Context, msgs agentcore.MessageList, res imageResolution) (agentcore.MessageList, imageResolutionReport) {
	var report imageResolutionReport
	if len(msgs) == 0 {
		return msgs, report
	}
	out := make(agentcore.MessageList, len(msgs))
	copied := false
	ensureCopy := func() {
		if copied {
			return
		}
		copy(out, msgs)
		copied = true
	}
	for i, msg := range msgs {
		content := messageContent(msg)
		if !contentHasImageRef(content) {
			continue
		}
		resolved := make(agentcore.ContentList, 0, len(content))
		for _, block := range content {
			ref, ok := block.(agentcore.ImageRefContent)
			if !ok {
				resolved = append(resolved, block)
				continue
			}
			if res.model != nil && !res.model.ImageInput {
				// The model explicitly denies image input: no upload, no inline
				// bytes, an explicit metadata fallback with the tools hint.
				report.fallbacks = append(report.fallbacks, ref.BlobID)
				ensureCopy()
				resolved = append(resolved, agentcore.NewTextContent(
					imageFallbackText(ref, "the selected model does not declare image input")))
				continue
			}
			wire, err := r.resolveOneImage(ctx, res, ref)
			if err != nil {
				report.failures = append(report.failures, imageFailure{blobID: ref.BlobID, err: err})
				resolved = append(resolved, agentcore.NewTextContent(imageOmittedNotice(ref, err)))
				continue
			}
			if wire.Note != "" {
				report.degraded = append(report.degraded, wire.Note)
			}
			if report.forms == nil {
				report.forms = make(map[string]int, 3)
			}
			report.forms[wire.Form]++
			resolved = append(resolved, wire.Content)
		}
		ensureCopy()
		out[i] = replaceMessageContent(msg, resolved)
	}
	if !copied {
		return msgs, report
	}
	return out, report
}

// resolveOneImage resolves a single reference, applying the runner's policy on
// top of the resolver: a nil resolver (no image pipeline wired) and a missing
// workspace are refusals the report names.
func (r *Runner) resolveOneImage(ctx context.Context, res imageResolution, ref agentcore.ImageRefContent) (provider.ResolvedImage, error) {
	if res.resolver == nil {
		return provider.ResolvedImage{}, fmt.Errorf("this deployment has no image pipeline wired (provider files are not assembled)")
	}
	if strings.TrimSpace(res.workspace) == "" {
		return provider.ResolvedImage{}, fmt.Errorf("no workspace is bound to the attempt, so the blob cannot be read")
	}
	return res.resolver.Resolve(ctx, res.workspace, res.target, ref, res.caps)
}

// imageOmittedNotice renders the text a dropped image becomes. It names the blob
// and the reason so a reader of the transcript can tell an unusable attachment
// from a model that ignored one.
func imageOmittedNotice(ref agentcore.ImageRefContent, err error) string {
	name := ref.Name
	if name == "" {
		name = ref.BlobID
	}
	return fmt.Sprintf("[image %s omitted: %v]", name, err)
}

// imageResolutionReport summarises one attempt's outbound resolution for the
// log line: how many references took each form, which fell back to metadata
// text, which degraded, which failed.
type imageResolutionReport struct {
	forms     map[string]int
	degraded  []string
	failures  []imageFailure
	fallbacks []string
}

type imageFailure struct {
	blobID string
	err    error
}

// empty reports that no image reference was present (no log line needed).
func (rep imageResolutionReport) empty() bool {
	return len(rep.forms) == 0 && len(rep.degraded) == 0 && len(rep.failures) == 0 && len(rep.fallbacks) == 0
}

// imageFallbackText renders the metadata block an image reference becomes when
// the model denies image input. It names the blob and tells the model to inspect
// it with file tools; it carries no bytes.
func imageFallbackText(ref agentcore.ImageRefContent, reason string) string {
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
	stored := ""
	if blobID := strings.TrimSpace(ref.BlobID); blobID != "" {
		stored = fmt.Sprintf(" Stored bytes: blobId=%s.", blobID)
	}
	return fmt.Sprintf("[image attachment %s was not sent with this request: %s.%s "+
		"Use file tools to inspect the attachment if you need its contents.]", facts, reason, stored)
}

// contentHasImageRef reports whether a content list carries a durable reference.
func contentHasImageRef(content agentcore.ContentList) bool {
	for _, block := range content {
		if _, ok := block.(agentcore.ImageRefContent); ok {
			return true
		}
	}
	return false
}

// messageContent returns a message's content list (nil for messages that carry
// none, e.g. a system entry or a compaction checkpoint, which holds a summary
// rather than a content list).
func messageContent(msg agentcore.Message) agentcore.ContentList {
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

// replaceMessageContent returns the message with a replaced content list,
// preserving every other field. An unrecognized message kind is returned
// unchanged: it carries no content list this pass understands, and rebuilding an
// unknown type would silently lose its payload.
func replaceMessageContent(msg agentcore.Message, content agentcore.ContentList) agentcore.Message {
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

// imageWorkspace is the blob scope of the attempt: the explicit field when set,
// otherwise the lease's tenant. The tenant is the right default — blobs are
// tenant-scoped rows — and the explicit field exists so a host can override it
// (a future per-user workspace) without a new seam. Attachments share this scope
// (see attachments.go).
func (r *Runner) imageWorkspace(lease kernel.Lease) string {
	if workspace := strings.TrimSpace(r.ImageWorkspace); workspace != "" {
		return workspace
	}
	return lease.Scope.TenantID.String()
}

// logImageResolution records what the outbound pass did. One line per attempt
// with images: forms in use, degradations (the preferred form was unavailable)
// and failures (the image was replaced by a notice). Both warn level — an image
// the model did not receive is never a debug detail.
func logImageResolution(logger *slog.Logger, lease kernel.Lease, model ActiveModel, report imageResolutionReport) {
	if logger == nil {
		return
	}
	attrs := []any{
		"session_id", lease.Scope.SessionID,
		"provider", model.ProviderID,
		"model", model.Model,
	}
	for _, failure := range report.failures {
		logger.Warn("outbound image omitted", append(attrs, "blob_id", failure.blobID, "err", failure.err)...)
	}
	for _, blobID := range report.fallbacks {
		logger.Info("outbound image sent as metadata text", append(attrs, "blob_id", blobID,
			"reason", "the selected model does not declare image input")...)
	}
	for _, note := range report.degraded {
		logger.Warn("outbound image degraded", append(attrs, "reason", note)...)
	}
	if len(report.forms) > 0 {
		logger.Info("outbound images resolved", append(attrs, "forms", report.forms)...)
	}
}
