package agent

// Outbound attachment dispatch (change keel-harness-v2, modality-input lane).
//
// The transcript stores durable references (ImageRefContent for images,
// MediaRefContent for documents/audio/video/generic files). This file is the
// boundary crossing for every non-image attachment, and it implements the
// Owner's rule (2026-11-23):
//
//  1. If the selected model declares the input modality AND the wire protocol
//     carries it natively, the attachment travels as a native provider part
//     (document/audio; see internal/provider/media.go for the matrix).
//  2. Otherwise the attachment becomes a text block naming the blob id, name,
//     MIME, size and whatever cheap metadata was available, plus an instruction
//     to inspect it with file tools. Never bytes, never base64.
//  3. No attachment is ever dropped silently: every fallback names the reason,
//     and a reference that cannot even be probed still produces that text.
//
// Images keep their dedicated resolver path (internal/agent/images.go), but the
// capability gate is now the same general one: a model that denies a modality
// never causes an upload or a native part.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
)

// MediaResolver resolves durable non-image references: a bounded metadata probe
// (fallback text), a document wire form, and an audio wire form.
// provider.MediaResolver implements it.
type MediaResolver interface {
	Probe(ctx context.Context, workspace string, ref agentcore.MediaRefContent) (provider.MediaFacts, error)
	ResolveFile(ctx context.Context, workspace string, target provider.ImageTarget, ref agentcore.MediaRefContent, caps provider.FileCaps) (provider.ResolvedFile, error)
	ResolveAudio(ctx context.Context, workspace string, target provider.ImageTarget, ref agentcore.MediaRefContent, caps provider.AudioCaps) (provider.ResolvedAudio, error)
}

// noAttachmentPipeline is the reason a reference gets when the deployment wired
// no resolver at all. It is deliberately distinct from a wire refusal: the wire
// never got the chance to carry the attachment, and saying otherwise would blame
// the protocol for a wiring gap.
const noAttachmentPipeline = "this deployment has no attachment pipeline wired"

// attachmentResolution carries the per-attempt inputs of outbound attachment
// dispatch: the resolver, the blob scope, the attempt's target and the model's
// declared capabilities (the one fact that decides native vs fallback).
type attachmentResolution struct {
	resolver  MediaResolver
	workspace string
	target    provider.ImageTarget
	model     *ModelProperties
	caps      provider.MediaCaps
}

// resolveAttachments rewrites every MediaRefContent in the transcript: a native
// wire block when the model and the wire both accept the modality, otherwise a
// metadata text block. Returns a copy (the loaded transcript is re-read by a
// later step and must stay untouched).
func (r *Runner) resolveAttachments(ctx context.Context, msgs agentcore.MessageList, res attachmentResolution) (agentcore.MessageList, attachmentResolutionReport) {
	var report attachmentResolutionReport
	if len(msgs) == 0 {
		return msgs, report
	}
	// A nil resolver is not an error: every reference becomes the metadata
	// fallback with the facts the reference itself carries. The model still learns
	// the attachment exists; the run continues. The degradation is reported only
	// when a reference is actually present (a text-only transcript with no
	// pipeline is not a degraded attempt).
	resolverMissing := res.resolver == nil
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
		if !contentHasMediaRef(content) {
			continue
		}
		if resolverMissing && len(report.degraded) == 0 {
			report.degraded = append(report.degraded, noAttachmentPipeline+"; attachments travel as metadata text")
		}
		resolved := make(agentcore.ContentList, 0, len(content))
		for _, block := range content {
			ref, ok := block.(agentcore.MediaRefContent)
			if !ok {
				resolved = append(resolved, block)
				continue
			}
			native, fallback, err := r.resolveOneAttachment(ctx, res, ref)
			if err != nil {
				report.failures = append(report.failures, attachmentFailure{blobID: ref.BlobID, err: err})
				ensureCopy()
				resolved = append(resolved, agentcore.NewTextContent(r.attachmentFallbackText(ctx, res, ref, err.Error())))
				continue
			}
			if native != nil {
				// A native form can still carry a note ("the handle upload failed,
				// degraded to inline"): the attachment travels, but the operator
				// needs the degradation fact, as the image path already records it.
				if fallback.reason != "" {
					report.degraded = append(report.degraded, fallback.reason)
				}
				if report.forms == nil {
					report.forms = make(map[string]int, 3)
				}
				report.forms[nativeForm(native)]++
				ensureCopy()
				resolved = append(resolved, native)
				continue
			}
			// fallback is the honest path: no bytes, no base64, an explicit reason.
			report.fallbacks = append(report.fallbacks, fallback)
			ensureCopy()
			resolved = append(resolved, agentcore.NewTextContent(r.attachmentFallbackText(ctx, res, ref, fallback.reason)))
		}
		ensureCopy()
		out[i] = replaceMessageContent(msg, resolved)
	}
	if !copied {
		return msgs, report
	}
	return out, report
}

// attachmentFallback names the modality and the reason a native part was not
// produced.
type attachmentFallback struct {
	blobID   string
	kind     string
	modality string
	reason   string
}

// attachmentFailure is a reference the resolver could not even attempt (for
// example an upload error after the modality gate passed).
type attachmentFailure struct {
	blobID string
	err    error
}

// nativeForm labels a native wire block for the log line.
func nativeForm(block agentcore.Content) string {
	switch block.(type) {
	case agentcore.FileContent:
		return "document"
	case agentcore.AudioContent:
		return "audio"
	case agentcore.ImageContent:
		return "image"
	default:
		return "unknown"
	}
}

// resolveOneAttachment applies the dispatch matrix to one reference:
//
//	model denies modality        → fallback (the decisive gate)
//	kind has no native wire form → fallback
//	provider resolver fails      → fallback with the reason
//	otherwise                    → native wire block
//
// It returns exactly one of native/fallback on success; err means the resolver
// itself malfunctioned, which still becomes a fallback text at the call site.
func (r *Runner) resolveOneAttachment(ctx context.Context, res attachmentResolution, ref agentcore.MediaRefContent) (agentcore.Content, attachmentFallback, error) {
	kind := attachmentKind(ref)
	fallback := attachmentFallback{
		blobID: ref.BlobID,
		kind:   kind,
	}
	switch kind {
	case agentcore.MediaKindPDF:
		fallback.modality = "pdf"
		if !res.model.AcceptsModality(kind) {
			fallback.reason = "the selected model does not declare PDF input"
			return nil, fallback, nil
		}
		if res.resolver == nil {
			fallback.reason = noAttachmentPipeline
			return nil, fallback, nil
		}
		if !res.caps.File.SupportsMime("application/pdf") {
			fallback.reason = "this wire protocol cannot carry a PDF document natively"
			return nil, fallback, nil
		}
		file, err := res.resolver.ResolveFile(ctx, res.workspace, res.target, ref, res.caps.File)
		if err != nil {
			fallback.reason = err.Error()
			return nil, fallback, nil
		}
		if file.Note != "" {
			fallback.reason = file.Note
		}
		return file.Content, fallback, nil
	case agentcore.MediaKindAudio:
		fallback.modality = "audio"
		if !res.model.AcceptsModality(kind) {
			fallback.reason = "the selected model does not declare audio input"
			return nil, fallback, nil
		}
		if res.resolver == nil {
			fallback.reason = noAttachmentPipeline
			return nil, fallback, nil
		}
		format := provider.AudioFormatFromMime(ref.MimeType, ref.Name)
		if !res.caps.Audio.SupportsInline || format == "" || !res.caps.Audio.SupportsFormat(format) {
			fallback.reason = audioUnsupportedReason(format, res.caps.Audio)
			return nil, fallback, nil
		}
		audio, err := res.resolver.ResolveAudio(ctx, res.workspace, res.target, ref, res.caps.Audio)
		if err != nil {
			fallback.reason = err.Error()
			return nil, fallback, nil
		}
		return audio.Content, fallback, nil
	case agentcore.MediaKindVideo:
		fallback.modality = "video"
		if !res.model.AcceptsModality(kind) {
			fallback.reason = "the selected model does not declare video input"
			return nil, fallback, nil
		}
		// No wire this package speaks carries video natively. The model may accept
		// it, but the protocol cannot: the truthful answer is the fallback.
		fallback.reason = "no provider protocol implemented here carries video input"
		return nil, fallback, nil
	default:
		fallback.modality = "file"
		fallback.reason = "no native input part exists for this file type"
		return nil, fallback, nil
	}
}

// audioUnsupportedReason explains why audio did not travel natively, naming the
// format the wire accepts when the format is the blocker.
func audioUnsupportedReason(format string, caps provider.AudioCaps) string {
	switch {
	case !caps.SupportsInline:
		return "this wire protocol has no audio input part"
	case format == "":
		return "the audio format is not one this wire accepts (wav or mp3)"
	default:
		return fmt.Sprintf("audio format %q is not accepted by this wire (wav or mp3)", format)
	}
}

// attachmentKind classifies a reference: the producer's Kind when set, otherwise
// the MIME type and name extension (the same classifier the provider uses, so the
// dispatcher and the resolver never disagree).
func attachmentKind(ref agentcore.MediaRefContent) string {
	if kind := strings.TrimSpace(ref.Kind); kind != "" {
		return kind
	}
	return agentcore.InputKindForMime(ref.MimeType, ref.Name)
}

// attachmentFallbackText renders the metadata block a non-native attachment
// becomes. It names the blob (the durable handle), the cheap facts the platform
// knows, and an explicit "unknown" for anything it does not — then tells the
// model to inspect the bytes with file tools. It never carries bytes or
// credentials.
func (r *Runner) attachmentFallbackText(ctx context.Context, res attachmentResolution, ref agentcore.MediaRefContent, reason string) string {
	facts := provider.MediaFacts{
		BlobID: strings.TrimSpace(ref.BlobID),
		Name:   strings.TrimSpace(ref.Name),
		Mime:   strings.TrimSpace(ref.MimeType),
		Size:   ref.Size,
		Width:  ref.Width,
		Height: ref.Height,
	}
	// Metadata is best-effort: a probe failure already produced the reason being
	// rendered, so it must not throw away the reference's own facts.
	if res.resolver != nil && facts.BlobID != "" {
		if probed, err := res.resolver.Probe(ctx, res.workspace, ref); err == nil {
			facts = probed
			if name := strings.TrimSpace(ref.Name); name != "" {
				facts.Name = name
			}
		}
	}

	var b strings.Builder
	b.WriteString("[attachment ")
	b.WriteString(attachmentLabel(facts))
	b.WriteString(" was not sent with this request")
	if msg := strings.TrimSpace(reason); msg != "" {
		b.WriteString(": ")
		b.WriteString(trimReason(msg))
	}
	b.WriteString(".")
	if facts.BlobID != "" {
		b.WriteString(" Stored bytes are referenced by blobId=")
		b.WriteString(facts.BlobID)
		b.WriteString(" (platform file id; the tenant file endpoint GET /v1/files/")
		b.WriteString(facts.BlobID)
		b.WriteString(" streams the bytes).")
	}
	b.WriteString(" Use file tools to inspect the attachment if you need its contents.]")
	return b.String()
}

// attachmentLabel renders the cheap facts: name, MIME, size, dimensions, pages,
// duration — with explicit "unknown" where the information is not available
// (never a guess).
func attachmentLabel(facts provider.MediaFacts) string {
	var parts []string
	name := facts.Name
	if name == "" {
		name = facts.BlobID
	}
	if name == "" {
		name = "unnamed"
	}
	parts = append(parts, name)
	if mime := strings.TrimSpace(facts.Mime); mime != "" {
		parts = append(parts, mime)
	}
	if facts.Size > 0 {
		parts = append(parts, fmt.Sprintf("%d bytes", facts.Size))
	}
	mime := strings.ToLower(strings.TrimSpace(facts.Mime))
	switch agentcore.InputKindForMime(mime, facts.Name) {
	case agentcore.MediaKindPDF:
		if facts.Pages > 0 {
			parts = append(parts, fmt.Sprintf("%d pages", facts.Pages))
		} else {
			parts = append(parts, "pages unknown")
		}
	case agentcore.MediaKindAudio, agentcore.MediaKindVideo:
		if facts.DurationMs > 0 {
			parts = append(parts, fmt.Sprintf("%d ms", facts.DurationMs))
		} else {
			parts = append(parts, "duration unknown")
		}
	}
	if strings.HasPrefix(mime, "image/") {
		if facts.Width > 0 && facts.Height > 0 {
			parts = append(parts, fmt.Sprintf("%dx%d", facts.Width, facts.Height))
		} else {
			parts = append(parts, "dimensions unknown")
		}
	}
	return strings.Join(parts, ", ")
}

// trimReason bounds a reason string so one pathological error cannot blow the
// context budget; the truncation is visible. It cuts on a rune boundary: a
// provider error may carry non-ASCII text, and half a UTF-8 sequence in the
// JSON body would make the whole request unencodable.
func trimReason(reason string) string {
	const maxReason = 400
	if len(reason) <= maxReason {
		return reason
	}
	cut := maxReason
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut] + "…"
}

// attachmentResolutionReport summarises one attempt's dispatch for the log line.
type attachmentResolutionReport struct {
	forms     map[string]int
	fallbacks []attachmentFallback
	degraded  []string
	failures  []attachmentFailure
}

// empty reports that no attachment reference was present.
func (rep attachmentResolutionReport) empty() bool {
	return len(rep.forms) == 0 && len(rep.fallbacks) == 0 && len(rep.degraded) == 0 && len(rep.failures) == 0
}

// contentHasMediaRef reports whether a content list carries a durable non-image
// reference.
func contentHasMediaRef(content agentcore.ContentList) bool {
	for _, block := range content {
		if _, ok := block.(agentcore.MediaRefContent); ok {
			return true
		}
	}
	return false
}

// logAttachmentResolution records what the outbound pass did: native forms in
// use, fallbacks (with the reason) and failures. Every fallback is a warn: an
// attachment the model did not receive natively is never a debug detail.
func logAttachmentResolution(logger *slog.Logger, lease kernel.Lease, model ActiveModel, report attachmentResolutionReport) {
	if logger == nil {
		return
	}
	attrs := []any{
		"session_id", lease.Scope.SessionID,
		"provider", model.ProviderID,
		"model", model.Model,
	}
	for _, failure := range report.failures {
		logger.Warn("outbound attachment failed", append(attrs, "blob_id", failure.blobID, "err", failure.err)...)
	}
	for _, note := range report.degraded {
		logger.Warn("outbound attachment degraded", append(attrs, "reason", note)...)
	}
	for _, fallback := range report.fallbacks {
		logger.Info("outbound attachment sent as metadata text", append(attrs,
			"blob_id", fallback.blobID, "kind", fallback.kind, "reason", fallback.reason)...)
	}
	if len(report.forms) > 0 {
		logger.Info("outbound attachments resolved", append(attrs, "forms", report.forms)...)
	}
}

// mediaCapsFor returns the attempt's media transport declaration, gated by the
// model's declared input modalities: a modality the model denies yields the zero
// declaration, so the dispatcher never uploads or inlines bytes for a request
// the model would refuse. Unknown properties (nil) keep the declaration — the
// runtime only refuses what the configuration explicitly denies.
func mediaCapsFor(model ActiveModel) provider.MediaCaps {
	caps := model.MediaCaps
	if caps.IsEmpty() {
		// The legacy field predates MediaCaps: keep the old image declaration
		// working for a host that sets ImageCaps directly.
		caps.Image = model.ImageCaps
	}
	props := model.Properties
	if props == nil {
		return caps
	}
	if !props.ImageInput {
		caps.Image = provider.ImageCaps{}
	}
	if !props.PDFInput {
		caps.File = provider.FileCaps{}
	}
	if !props.AudioInput {
		caps.Audio = provider.AudioCaps{}
	}
	if !props.VideoInput {
		caps.Video = false
	}
	return caps
}

// imageCapsFor returns the attempt's image transport declaration: the general
// media gate's image slice. It remains the seam the image resolver path reads.
func imageCapsFor(model ActiveModel) provider.ImageCaps {
	return mediaCapsFor(model).Image
}
