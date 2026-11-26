package agent

// Outbound attachment dispatch (change keel-harness-v2, modality-input lane).
//
// The Owner rule these tests pin (2026-11-23): if the model declares the input
// modality and the wire carries it, the attachment travels natively; otherwise
// it travels as a text block naming the blob and the cheap facts, and the model
// is told to use file tools. No attachment is ever dropped silently.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// fakeMediaResolver records dispatch calls and answers with the form requested.
type fakeMediaResolver struct {
	probes     int
	probeErr   error
	probeFacts *provider.MediaFacts

	fileCalls   int
	lastFileRef agentcore.MediaRefContent
	fileCaps    provider.FileCaps
	fileErr     error
	fileForm    string
	fileNote    string

	audioCalls   int
	lastAudioRef agentcore.MediaRefContent
	audioErr     error
}

func (f *fakeMediaResolver) Probe(_ context.Context, _ string, ref agentcore.MediaRefContent) (provider.MediaFacts, error) {
	f.probes++
	if f.probeErr != nil {
		return provider.MediaFacts{}, f.probeErr
	}
	if f.probeFacts != nil {
		return *f.probeFacts, nil
	}
	return provider.MediaFacts{BlobID: ref.BlobID, Name: ref.Name, Mime: ref.MimeType, Size: ref.Size, Width: ref.Width, Height: ref.Height}, nil
}

func (f *fakeMediaResolver) ResolveFile(_ context.Context, _ string, _ provider.ImageTarget, ref agentcore.MediaRefContent, caps provider.FileCaps) (provider.ResolvedFile, error) {
	f.fileCalls++
	f.lastFileRef = ref
	f.fileCaps = caps
	if f.fileErr != nil {
		return provider.ResolvedFile{}, f.fileErr
	}
	form := f.fileForm
	if form == "" {
		form = provider.FileFormInline
	}
	content := agentcore.FileContent{Type: agentcore.ContentTypeFile, MimeType: ref.MimeType, Name: ref.Name}
	switch form {
	case provider.FileFormFileID:
		content.FileID = "file-abc"
	case provider.FileFormURL:
		content.URL = "https://keel.example/v1/files/" + ref.BlobID
	default:
		form = provider.FileFormInline
		content.Data = "JVBERi0xLjQK"
	}
	return provider.ResolvedFile{Content: content, Form: form, Note: f.fileNote}, nil
}

func (f *fakeMediaResolver) ResolveAudio(_ context.Context, _ string, _ provider.ImageTarget, ref agentcore.MediaRefContent, caps provider.AudioCaps) (provider.ResolvedAudio, error) {
	f.audioCalls++
	f.lastAudioRef = ref
	if f.audioErr != nil {
		return provider.ResolvedAudio{}, f.audioErr
	}
	return provider.ResolvedAudio{
		Content: agentcore.AudioContent{Type: agentcore.ContentTypeAudio, Data: "UklGRg==", MimeType: ref.MimeType, Format: "wav", Name: ref.Name},
		Form:    provider.FileFormInline,
	}, nil
}

// mediaTranscript writes one committed user.message carrying the given
// attachment entries and returns the runner's view of the log.
func mediaTranscript(t *testing.T, attachments ...map[string]any) agentcore.MessageList {
	t.Helper()
	payload := mustJSON(map[string]any{"text": "look at this", "attachments": attachments})
	return MessagesFromEvents([]store.Event{{Seq: 1, Kind: store.EventUserMessage, Payload: payload}}, nil)
}

// TestAttachmentPDFNativePath pins the native branch: a model that declares PDF
// input and a wire that carries PDFs produces a native document block, and the
// resolver receives the model's file declaration.
func TestAttachmentPDFNativePath(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-pdf-1", "name": "report.pdf", "mime": "application/pdf", "size": 2048,
	})
	resolver := &fakeMediaResolver{fileForm: provider.FileFormInline}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{PDFInput: true},
		caps:      provider.MediaCapsForAPI("openai-chat-completions"),
	})
	if resolver.fileCalls != 1 {
		t.Fatalf("ResolveFile calls = %d, want 1", resolver.fileCalls)
	}
	if !resolver.fileCaps.SupportsMime("application/pdf") {
		t.Fatalf("file caps forwarded = %+v, want PDF support", resolver.fileCaps)
	}
	if report.forms["document"] != 1 {
		t.Fatalf("report forms = %+v, want one native document", report.forms)
	}
	user := resolved[0].(agentcore.UserMessage)
	doc, ok := user.Content[1].(agentcore.FileContent)
	if !ok {
		t.Fatalf("second block = %T, want FileContent", user.Content[1])
	}
	if doc.Data == "" || doc.MimeType != "application/pdf" || doc.Name != "report.pdf" {
		t.Fatalf("document block = %+v, want inline PDF bytes with name and MIME", doc)
	}
	// The loaded transcript must stay untouched (a later step re-reads it).
	original := msgs[0].(agentcore.UserMessage)
	if _, ok := original.Content[1].(agentcore.MediaRefContent); !ok {
		t.Fatalf("the loaded transcript was mutated: %#v", original.Content[1])
	}
}

// TestAttachmentPDFModelDeniesInput pins the decisive gate: a model that does
// not declare PDF input never causes a resolver call, and the attachment becomes
// an explicit metadata text block naming the blob.
func TestAttachmentPDFModelDeniesInput(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-pdf-2", "name": "report.pdf", "mime": "application/pdf", "size": 2048,
	})
	resolver := &fakeMediaResolver{}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{PDFInput: false, TextInput: true},
		caps:      provider.MediaCapsForAPI("openai-chat-completions"),
	})
	if resolver.fileCalls != 0 {
		t.Fatalf("ResolveFile calls = %d, want 0 (the model denies PDF input)", resolver.fileCalls)
	}
	if len(report.fallbacks) != 1 || !strings.Contains(report.fallbacks[0].reason, "does not declare PDF") {
		t.Fatalf("report = %+v, want one fallback naming the model declaration", report)
	}
	text := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.TextContent).Text
	for _, want := range []string{"report.pdf", "application/pdf", "2048 bytes", "blob-pdf-2", "file tools"} {
		if !strings.Contains(text, want) {
			t.Fatalf("fallback text = %q, want it to contain %q", text, want)
		}
	}
	// No bytes may leak into the fallback.
	if strings.Contains(text, "JVBERi0") {
		t.Fatalf("fallback text carries bytes: %q", text)
	}
}

// TestAttachmentPDFWireCannotCarry pins the wire gate: a model that declares PDF
// input on a protocol without a document part still degrades (no fake part).
func TestAttachmentPDFWireCannotCarry(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-pdf-3", "name": "report.pdf", "mime": "application/pdf",
	})
	resolver := &fakeMediaResolver{}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{PDFInput: true},
		caps:      provider.MediaCaps{}, // an undeclared wire
	})
	if resolver.fileCalls != 0 {
		t.Fatalf("ResolveFile calls = %d, want 0 (the wire declares no document part)", resolver.fileCalls)
	}
	if len(report.fallbacks) != 1 || !strings.Contains(report.fallbacks[0].reason, "wire protocol cannot carry a PDF") {
		t.Fatalf("report = %+v, want a wire-level fallback", report)
	}
	text := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.TextContent).Text
	for _, want := range []string{"blob-pdf-3", "application/pdf", "wire protocol cannot carry a PDF"} {
		if !strings.Contains(text, want) {
			t.Fatalf("fallback = %q, want %q", text, want)
		}
	}
}

// TestAttachmentAudioNativeOnChatWire pins the one native audio path: a model
// that declares audio input on the chat wire gets an inline audio block.
func TestAttachmentAudioNativeOnChatWire(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-audio-1", "name": "memo.wav", "mime": "audio/wav", "size": 4096,
	})
	resolver := &fakeMediaResolver{}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{AudioInput: true},
		caps:      provider.MediaCapsForAPI("openai-chat-completions"),
	})
	if resolver.audioCalls != 1 {
		t.Fatalf("ResolveAudio calls = %d, want 1", resolver.audioCalls)
	}
	if report.forms["audio"] != 1 {
		t.Fatalf("report forms = %+v, want one native audio", report.forms)
	}
	audio, ok := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.AudioContent)
	if !ok {
		t.Fatalf("second block = %T, want AudioContent", resolved[0].(agentcore.UserMessage).Content[1])
	}
	if audio.Data != "UklGRg==" || audio.Format != "wav" {
		t.Fatalf("audio block = %+v, want inline wav", audio)
	}
}

// TestAttachmentAudioFallbackWhereUnsupported pins the truthful degradation: the
// Responses wire has no accepted audio part, so even a model that declares audio
// input receives the metadata text (it must never receive a part the upstream
// rejects).
func TestAttachmentAudioFallbackWhereUnsupported(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-audio-2", "name": "memo.mp3", "mime": "audio/mpeg",
	})
	resolver := &fakeMediaResolver{}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{AudioInput: true},
		caps:      provider.MediaCapsForAPI("openai-responses"),
	})
	if resolver.audioCalls != 0 {
		t.Fatalf("ResolveAudio calls = %d, want 0 (the Responses wire cannot carry audio)", resolver.audioCalls)
	}
	if len(report.fallbacks) != 1 || !strings.Contains(report.fallbacks[0].reason, "no audio input part") {
		t.Fatalf("report = %+v, want the wire-level audio fallback", report)
	}
	text := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.TextContent).Text
	for _, want := range []string{"blob-audio-2", "audio/mpeg", "no audio input part"} {
		if !strings.Contains(text, want) {
			t.Fatalf("fallback = %q, want %q", text, want)
		}
	}
}

// TestAttachmentVideoAlwaysFallsBack pins the video row of the matrix: no wire
// this package speaks carries video, and the fallback says so.
func TestAttachmentVideoAlwaysFallsBack(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-video-1", "name": "clip.mp4", "mime": "video/mp4",
	})
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  &fakeMediaResolver{},
		workspace: "tenant-1",
		model:     &ModelProperties{VideoInput: true},
		caps:      provider.MediaCapsForAPI("openai-responses"),
	})
	if len(report.fallbacks) != 1 || !strings.Contains(report.fallbacks[0].reason, "no provider protocol") {
		t.Fatalf("report = %+v, want one video fallback", report)
	}
	text := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.TextContent).Text
	for _, want := range []string{"blob-video-1", "clip.mp4", "video/mp4", "no provider protocol"} {
		if !strings.Contains(text, want) {
			t.Fatalf("fallback = %q, want %q", text, want)
		}
	}
}

// TestAttachmentResolverFailureLeavesText pins the failure path: an upload or
// read error becomes metadata text naming the reason, never a dropped turn.
func TestAttachmentResolverFailureLeavesText(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-pdf-4", "name": "a.pdf", "mime": "application/pdf",
	})
	resolver := &fakeMediaResolver{fileErr: fmt.Errorf("vendor upload failed (401)")}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{PDFInput: true},
		caps:      provider.MediaCapsForAPI("openai-responses"),
	})
	if len(report.fallbacks) != 1 || !strings.Contains(report.fallbacks[0].reason, "401") {
		t.Fatalf("report = %+v, want the vendor failure reason", report)
	}
	text := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.TextContent).Text
	if !strings.Contains(text, "401") {
		t.Fatalf("fallback = %q, want the resolver's reason", text)
	}
	if !strings.Contains(ContentText(resolved[0].(agentcore.UserMessage).Content), "look at this") {
		t.Fatal("the user's prompt was lost")
	}
}

// TestAttachmentProbeFailureKeepsReferenceFacts pins the metadata honesty: when a
// probe fails, the fallback still names the reference's own facts (the reference
// is never lost because the probe was).
func TestAttachmentProbeFailureKeepsReferenceFacts(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-doc-9", "name": "notes.log", "mime": "text/plain", "size": 77,
	})
	resolver := &fakeMediaResolver{probeErr: fmt.Errorf("blob read failed")}
	runner := &Runner{Logger: discardLog()}
	resolved, _ := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{TextInput: true},
		caps:      provider.MediaCapsForAPI("openai-chat-completions"),
	})
	text := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.TextContent).Text
	for _, want := range []string{"notes.log", "text/plain", "77 bytes", "blob-doc-9"} {
		if !strings.Contains(text, want) {
			t.Fatalf("fallback = %q, want it to contain %q", text, want)
		}
	}
}

// TestAttachmentWithoutBlobIdIsNotDropped pins the decode-side guarantee: an
// attachment with no fileId becomes an explicit notice at rebuild time (there is
// no reference to dispatch later).
func TestAttachmentWithoutBlobIdIsNotDropped(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{"name": "orphan.pdf", "mime": "application/pdf"})
	user := msgs[0].(agentcore.UserMessage)
	if len(user.Content) != 2 {
		t.Fatalf("content = %#v, want the text plus the notice", user.Content)
	}
	notice, ok := user.Content[1].(agentcore.TextContent)
	if !ok || !strings.Contains(notice.Text, "no fileId") || !strings.Contains(notice.Text, "orphan.pdf") {
		t.Fatalf("notice = %#v, want an explicit no-blobId notice naming the attachment", user.Content[1])
	}
}

// TestAttachmentNoResolverStillNamesTheBlob pins the unwired deployment: without
// a resolver, every reference becomes the metadata fallback with the facts the
// reference carries, and the report says why.
func TestAttachmentNoResolverStillNamesTheBlob(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-pdf-5", "name": "x.pdf", "mime": "application/pdf", "size": 10,
	})
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		workspace: "tenant-1",
		model:     &ModelProperties{PDFInput: true},
		caps:      provider.MediaCapsForAPI("openai-chat-completions"),
	})
	if len(report.degraded) != 1 || !strings.Contains(report.degraded[0], "no attachment pipeline") {
		t.Fatalf("report = %+v, want the unwired degradation", report)
	}
	text := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.TextContent).Text
	for _, want := range []string{"blob-pdf-5", "application/pdf", "10 bytes", "no attachment pipeline", "file tools"} {
		if !strings.Contains(text, want) {
			t.Fatalf("fallback = %q, want %q", text, want)
		}
	}
}

// TestAttachmentTextOnlyModelReceivesOnlyMetadata pins the end-to-end shape for a
// text-only model: no bytes cross any resolver, and every attachment is present
// as text.
func TestAttachmentTextOnlyModelReceivesOnlyMetadata(t *testing.T) {
	msgs := mediaTranscript(t,
		map[string]any{"fileId": "blob-pdf-6", "name": "d.pdf", "mime": "application/pdf"},
		map[string]any{"fileId": "blob-audio-6", "name": "a.wav", "mime": "audio/wav"},
	)
	resolver := &fakeMediaResolver{}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{TextInput: true},
		caps:      provider.MediaCapsForAPI("openai-chat-completions"),
	})
	if resolver.fileCalls != 0 || resolver.audioCalls != 0 {
		t.Fatalf("resolver calls file=%d audio=%d, want none for a text-only model", resolver.fileCalls, resolver.audioCalls)
	}
	if len(report.fallbacks) != 2 {
		t.Fatalf("report = %+v, want two fallbacks", report)
	}
	user := resolved[0].(agentcore.UserMessage)
	if len(user.Content) != 3 {
		t.Fatalf("content = %#v, want the text plus one fallback per attachment", user.Content)
	}
	for i := 1; i < len(user.Content); i++ {
		if _, ok := user.Content[i].(agentcore.TextContent); !ok {
			t.Fatalf("block %d = %T, want metadata text", i, user.Content[i])
		}
	}
}

// TestMediaCapsForGatesEveryModality pins the general capability gate: a model
// declaration zeroes exactly the modality slices it denies and leaves the rest.
func TestMediaCapsForGatesEveryModality(t *testing.T) {
	base := ActiveModel{
		MediaCaps: provider.MediaCapsForAPI("openai-chat-completions"),
		ImageCaps: provider.ImageCaps{SupportsImageURL: true},
	}
	base.Properties = nil
	if got := mediaCapsFor(base); !got.Audio.SupportsInline || !got.File.SupportsInline || !got.Image.SupportsImageURL {
		t.Fatalf("unknown properties = %+v, want the full wire declaration", got)
	}

	denied := base
	denied.Properties = &ModelProperties{TextInput: true}
	if got := mediaCapsFor(denied); got.Audio.SupportsInline || got.File.SupportsInline || got.Image.SupportsImageURL {
		t.Fatalf("text-only model = %+v, want every modality slice zeroed", got)
	}

	partial := base
	partial.Properties = &ModelProperties{TextInput: true, ImageInput: true, PDFInput: true}
	got := mediaCapsFor(partial)
	if !got.Image.SupportsImageURL || !got.File.SupportsInline {
		t.Fatalf("partial = %+v, want image and PDF slices kept", got)
	}
	if got.Audio.SupportsInline {
		t.Fatalf("partial = %+v, want the audio slice zeroed", got)
	}
}

// TestMediaCapsForKeepsTheLegacyImageField pins backward compatibility with a
// host that sets only ActiveModel.ImageCaps (the pre-MediaCaps seam).
func TestMediaCapsForKeepsTheLegacyImageField(t *testing.T) {
	model := ActiveModel{
		ImageCaps:  provider.ImageCaps{SupportsFileID: true, MaxImageBytes: 1 << 20},
		Properties: &ModelProperties{TextInput: true, ImageInput: true},
	}
	if got := mediaCapsFor(model).Image; !got.SupportsFileID {
		t.Fatalf("image caps = %+v, want the legacy declaration preserved", got)
	}
}

// TestImageReferenceOnATextOnlyModelDegrades pins the image half of the general
// gate: no upload, no bytes, an explicit metadata block.
func TestImageReferenceOnATextOnlyModelDegrades(t *testing.T) {
	msgs := MessagesFromEvents(userImageTranscript(t), nil)
	resolver := &fakeImageResolver{form: provider.ImageFormURL}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveImages(context.Background(), msgs, imageResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{TextInput: true},
		caps:      provider.ImageCaps{SupportsImageURL: true},
	})
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0 (the model denies image input)", resolver.calls)
	}
	if len(report.fallbacks) != 1 {
		t.Fatalf("report = %+v, want one image fallback", report)
	}
	text := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.TextContent).Text
	if !strings.Contains(text, "does not declare image input") || !strings.Contains(text, "file tools") {
		t.Fatalf("fallback = %q, want the reason and the tools hint", text)
	}
}

// TestAttachmentNoResolverBlamesThePipelineNotTheWire pins the honest reason for
// an unwired deployment: the wire may well carry the document, so the fallback
// must not claim the protocol refused it.
func TestAttachmentNoResolverBlamesThePipelineNotTheWire(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-pdf-nr", "name": "a.pdf", "mime": "application/pdf",
	})
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		workspace: "tenant-1",
		model:     &ModelProperties{PDFInput: true},
		caps:      provider.MediaCapsForAPI("openai-chat-completions"),
	})
	if len(report.fallbacks) != 1 {
		t.Fatalf("report = %+v, want one fallback", report)
	}
	reason := report.fallbacks[0].reason
	if !strings.Contains(reason, "no attachment pipeline") {
		t.Fatalf("reason = %q, want the wiring gap named", reason)
	}
	if strings.Contains(reason, "wire protocol") {
		t.Fatalf("reason = %q, must not blame a wire that declares PDF support", reason)
	}
	text := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.TextContent).Text
	if !strings.Contains(text, "blob-pdf-nr") || !strings.Contains(text, "no attachment pipeline") {
		t.Fatalf("fallback = %q, want the blob and the wiring reason", text)
	}
}

// TestAttachmentAudioResolverFailureFallsBack pins the audio failure path: an
// upload or read error becomes the metadata text, and no native part is faked.
func TestAttachmentAudioResolverFailureFallsBack(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-audio-f", "name": "memo.wav", "mime": "audio/wav",
	})
	resolver := &fakeMediaResolver{audioErr: fmt.Errorf("provider: audio blob read failed")}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{AudioInput: true},
		caps:      provider.MediaCapsForAPI("openai-chat-completions"),
	})
	if len(report.fallbacks) != 1 || !strings.Contains(report.fallbacks[0].reason, "blob read failed") {
		t.Fatalf("report = %+v, want the resolver failure reason", report)
	}
	user := resolved[0].(agentcore.UserMessage)
	if _, ok := user.Content[1].(agentcore.AudioContent); ok {
		t.Fatal("a failed resolver must not produce a native audio block")
	}
	text := user.Content[1].(agentcore.TextContent).Text
	if !strings.Contains(text, "memo.wav") || !strings.Contains(text, "blob-audio-f") {
		t.Fatalf("fallback = %q, want the attachment's facts", text)
	}
}

// TestAttachmentWithoutBlobIdFallsBackHonestly pins the reference with no blob
// id that reaches dispatch anyway (a hand-built or future producer): the fallback
// names the reason, does not probe, and never claims stored bytes exist.
func TestAttachmentWithoutBlobIdFallsBackHonestly(t *testing.T) {
	msgs := agentcore.MessageList{agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content: agentcore.ContentList{
			agentcore.NewTextContent("see file"),
			agentcore.NewMediaRefContent(agentcore.MediaKindPDF, "", "orphan.pdf", "application/pdf", 0),
		},
	}}
	resolver := &fakeMediaResolver{fileErr: fmt.Errorf("provider: attachment carries no blobId")}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{PDFInput: true},
		caps:      provider.MediaCapsForAPI("openai-chat-completions"),
	})
	if len(report.fallbacks) != 1 || !strings.Contains(report.fallbacks[0].reason, "no blobId") {
		t.Fatalf("report = %+v, want the missing-blobId reason", report)
	}
	if resolver.probes != 0 {
		t.Fatalf("probe calls = %d, want 0 (there is no blob to probe)", resolver.probes)
	}
	text := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.TextContent).Text
	if !strings.Contains(text, "orphan.pdf") || !strings.Contains(text, "file tools") {
		t.Fatalf("fallback = %q, want the name and the tools hint", text)
	}
	if strings.Contains(text, "blobId=") {
		t.Fatalf("fallback = %q, must not claim stored bytes for a reference with no blob id", text)
	}
	if !strings.Contains(ContentText(resolved[0].(agentcore.UserMessage).Content), "see file") {
		t.Fatal("the user's prompt was lost")
	}
}

// TestAttachmentMixedNativeAndFallbackPreservesOrder pins per-attachment
// dispatch inside one message: the native document and the fallback audio keep
// the client's order, and the report counts each form.
func TestAttachmentMixedNativeAndFallbackPreservesOrder(t *testing.T) {
	msgs := mediaTranscript(t,
		map[string]any{"fileId": "blob-pdf-mix", "name": "a.pdf", "mime": "application/pdf"},
		map[string]any{"fileId": "blob-audio-mix", "name": "b.wav", "mime": "audio/wav"},
	)
	resolver := &fakeMediaResolver{}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{PDFInput: true}, // audio not declared
		caps:      provider.MediaCapsForAPI("openai-chat-completions"),
	})
	if report.forms["document"] != 1 {
		t.Fatalf("report = %+v, want one native document", report)
	}
	if len(report.fallbacks) != 1 || report.fallbacks[0].kind != agentcore.MediaKindAudio {
		t.Fatalf("report = %+v, want the audio fallback only", report)
	}
	user := resolved[0].(agentcore.UserMessage)
	if len(user.Content) != 3 {
		t.Fatalf("content = %#v, want text, document, fallback", user.Content)
	}
	if _, ok := user.Content[1].(agentcore.FileContent); !ok {
		t.Fatalf("block 1 = %T, want the native document in attachment order", user.Content[1])
	}
	text, ok := user.Content[2].(agentcore.TextContent)
	if !ok || !strings.Contains(text.Text, "b.wav") {
		t.Fatalf("block 2 = %#v, want the audio fallback", user.Content[2])
	}
	// The loaded transcript (re-read by a later step) must stay untouched.
	original := msgs[0].(agentcore.UserMessage)
	if _, ok := original.Content[1].(agentcore.MediaRefContent); !ok {
		t.Fatalf("loaded transcript mutated at block 1: %#v", original.Content[1])
	}
	if _, ok := original.Content[2].(agentcore.MediaRefContent); !ok {
		t.Fatalf("loaded transcript mutated at block 2: %#v", original.Content[2])
	}
}

// TestAttachmentVideoModelDeniesInput pins the model gate ahead of the video
// row: a model that denies video input gets the model reason, not the wire one.
func TestAttachmentVideoModelDeniesInput(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-video-d", "name": "clip.mp4", "mime": "video/mp4",
	})
	runner := &Runner{Logger: discardLog()}
	_, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  &fakeMediaResolver{},
		workspace: "tenant-1",
		model:     &ModelProperties{TextInput: true},
		caps:      provider.MediaCapsForAPI("openai-responses"),
	})
	if len(report.fallbacks) != 1 || !strings.Contains(report.fallbacks[0].reason, "does not declare video") {
		t.Fatalf("report = %+v, want the model's video denial", report)
	}
}

// TestAttachmentWithoutMimeButImageExtensionIsAnImage pins the classifier's
// extension fallback: a client that omitted the MIME must not turn a real image
// into a generic document (the blob store's MIME remains the authority the image
// resolver reads).
func TestAttachmentWithoutMimeButImageExtensionIsAnImage(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{"fileId": "blob-shot", "name": "shot.PNG"})
	user := msgs[0].(agentcore.UserMessage)
	ref, ok := user.Content[1].(agentcore.ImageRefContent)
	if !ok {
		t.Fatalf("block 1 = %T, want an image reference", user.Content[1])
	}
	if ref.BlobID != "blob-shot" || ref.Name != "shot.PNG" {
		t.Fatalf("reference = %+v, want the attachment's blob and name", ref)
	}
}

// TestAttachmentNativeFormWithANoteIsReportedDegraded pins the degradation fact:
// when the resolver had to fall back to a cheaper native form (for example the
// file-handle upload failed and it inlined instead), the attachment still travels
// natively and the report says why the first form was unavailable.
func TestAttachmentNativeFormWithANoteIsReportedDegraded(t *testing.T) {
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-pdf-note", "name": "a.pdf", "mime": "application/pdf",
	})
	resolver := &fakeMediaResolver{
		fileForm: provider.FileFormInline,
		fileNote: "provider file upload failed (401); degraded to inline",
	}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		model:     &ModelProperties{PDFInput: true},
		caps:      provider.MediaCapsForAPI("openai-chat-completions"),
	})
	if report.forms["document"] != 1 || len(report.fallbacks) != 0 {
		t.Fatalf("report = %+v, want one native document and no fallback", report)
	}
	if len(report.degraded) != 1 || !strings.Contains(report.degraded[0], "401") {
		t.Fatalf("report = %+v, want the degraded form's reason", report)
	}
	if _, ok := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.FileContent); !ok {
		t.Fatalf("block = %T, want the native inline document", resolved[0].(agentcore.UserMessage).Content[1])
	}
}

// TestAttachmentFallbackStatesUnknowns pins the label's honesty: a fact the cheap
// probe cannot know is written "unknown", never guessed from a byte count.
func TestAttachmentFallbackStatesUnknowns(t *testing.T) {
	cases := []struct {
		name  string
		facts provider.MediaFacts
		want  string
	}{
		{
			name:  "pdf pages",
			facts: provider.MediaFacts{Name: "a.pdf", Mime: "application/pdf", Size: 10},
			want:  "pages unknown",
		},
		{
			name:  "video duration",
			facts: provider.MediaFacts{Name: "a.mp4", Mime: "video/mp4", Size: 10},
			want:  "duration unknown",
		},
		{
			name:  "image dimensions",
			facts: provider.MediaFacts{Name: "a.webp", Mime: "image/webp", Size: 10},
			want:  "dimensions unknown",
		},
		{
			name:  "known pdf pages",
			facts: provider.MediaFacts{Name: "a.pdf", Mime: "application/pdf", Pages: 3},
			want:  "3 pages",
		},
		{
			name:  "known wav duration",
			facts: provider.MediaFacts{Name: "a.wav", Mime: "audio/wav", DurationMs: 1500},
			want:  "1500 ms",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := attachmentLabel(tc.facts); !strings.Contains(got, tc.want) {
				t.Fatalf("label = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTrimReasonCutsOnARuneBoundary pins that a pathological provider error
// cannot produce invalid UTF-8 in the request body.
func TestTrimReasonCutsOnARuneBoundary(t *testing.T) {
	reason := strings.Repeat("a", 399) + "é" + strings.Repeat("b", 20)
	got := trimReason(reason)
	if !utf8.ValidString(got) {
		t.Fatalf("trimReason produced invalid UTF-8: %q", got)
	}
	if len(got) > 402 { // 400-byte cut plus the ellipsis rune
		t.Fatalf("trimReason = %d bytes, want it bounded", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("trimReason = %q, want a visible truncation marker", got)
	}
}

// TestAttachmentDegradationLogCarriesOnlySafeFacts pins the log contract on a
// real dispatch: the degradation line names the blob id, MIME, size and reason,
// never the attachment's display name, the prompt text or any bytes.
func TestAttachmentDegradationLogCarriesOnlySafeFacts(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	msgs := mediaTranscript(t, map[string]any{
		"fileId": "blob-1", "name": "secret.pdf", "mime": "application/pdf", "size": 2048,
	})
	runner := &Runner{Logger: logger}
	_, report := runner.resolveAttachments(context.Background(), msgs, attachmentResolution{
		resolver:  &fakeMediaResolver{},
		workspace: "tenant-1",
		model:     &ModelProperties{TextInput: true},
		caps:      provider.MediaCapsForAPI("openai-chat-completions"),
	})
	report.failures = append(report.failures, attachmentFailure{
		blobID: "blob-2", mime: "image/png", size: 10, err: errors.New("blob missing"),
	})
	lease := kernel.Lease{Scope: kernel.Scope{SessionID: uuid.New()}}
	logAttachmentResolution(logger, lease, ActiveModel{ProviderID: "provider-1", Model: "model-1"}, report)
	line := buffer.String()
	for _, want := range []string{"blob-1", "application/pdf", "2048", "does not declare PDF input", "blob-2"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line lacks %q: %s", want, line)
		}
	}
	for _, forbidden := range []string{"secret.pdf", "look at this", "base64", "JVBERi0"} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("log line leaked %q: %s", forbidden, line)
		}
	}
}

// TestAttachmentDisplayNameCannotWidenTheModality pins the authority rule: a
// non-empty MIME decides the durable kind and the image path, so a text/plain
// object named `shot.png` stays a document, and an image/png object named
// `notes.txt` stays an image.
func TestAttachmentDisplayNameCannotWidenTheModality(t *testing.T) {
	text := mediaTranscript(t, map[string]any{
		"fileId": "blob-text-png", "name": "shot.png", "mime": "text/plain",
	})
	block := text[0].(agentcore.UserMessage).Content[1]
	if _, ok := block.(agentcore.MediaRefContent); !ok {
		t.Fatalf("text/plain named .png decoded as %T, want MediaRefContent", block)
	}

	image := mediaTranscript(t, map[string]any{
		"fileId": "blob-image-txt", "name": "notes.txt", "mime": "image/png",
	})
	imageBlock := image[0].(agentcore.UserMessage).Content[1]
	if _, ok := imageBlock.(agentcore.ImageRefContent); !ok {
		t.Fatalf("image/png named .txt decoded as %T, want ImageRefContent", imageBlock)
	}
}
