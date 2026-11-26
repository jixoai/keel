package agent

// Outbound image resolution at the runner boundary (keel-ga §3 provider-files).
//
// These tests pin the two properties the change owes the rest of the system:
//
//  1. The event log carries references only: a committed user.message with an
//     image attachment rebuilds into agentcore.ImageRefContent, never bytes.
//  2. A reference always reaches the provider as *something* the model can see,
//     or as an explicit text notice naming why not — never silently.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// TestUserMessageAttachmentsDecodeAsDurableReferences pins the durable shape:
// every attachment becomes a reference block (imageRef for images, mediaRef for
// everything else) and nothing else enters the content list; an attachment with
// no blob id becomes an explicit notice.
func TestUserMessageAttachmentsDecodeAsDurableReferences(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"text": "what is in this shot?",
		"attachments": []map[string]any{
			{"fileId": "blob-image-1", "name": "shot.png", "mime": "image/png", "size": 1234, "width": 800, "height": 600},
			{"fileId": "blob-doc-1", "name": "notes.txt", "mime": "text/plain", "size": 10},
			{"name": "no-id.png", "mime": "image/png"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := MessagesFromEvents([]store.Event{
		{Seq: 1, Kind: store.EventUserMessage, Payload: payload},
	}, nil)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	user, ok := msgs[0].(agentcore.UserMessage)
	if !ok {
		t.Fatalf("message = %T, want UserMessage", msgs[0])
	}
	if len(user.Content) != 4 {
		t.Fatalf("content = %#v, want text, imageRef, mediaRef and the no-blob notice", user.Content)
	}
	if _, ok := user.Content[0].(agentcore.TextContent); !ok {
		t.Fatalf("first block = %T, want the text block first", user.Content[0])
	}
	ref, ok := user.Content[1].(agentcore.ImageRefContent)
	if !ok {
		t.Fatalf("second block = %T, want ImageRefContent", user.Content[1])
	}
	if ref.BlobID != "blob-image-1" || ref.MimeType != "image/png" || ref.Width != 800 || ref.Height != 600 || ref.Size != 1234 {
		t.Fatalf("reference = %+v, want the attachment's blob metadata", ref)
	}
	doc, ok := user.Content[2].(agentcore.MediaRefContent)
	if !ok {
		t.Fatalf("third block = %T, want MediaRefContent for the non-image attachment", user.Content[2])
	}
	if doc.BlobID != "blob-doc-1" || doc.Name != "notes.txt" || doc.MimeType != "text/plain" || doc.Size != 10 {
		t.Fatalf("media reference = %+v, want the attachment's blob metadata", doc)
	}
	notice, ok := user.Content[3].(agentcore.TextContent)
	if !ok || !strings.Contains(notice.Text, "no fileId") {
		t.Fatalf("fourth block = %#v, want an explicit no-blob notice", user.Content[3])
	}
	// The byte-carrying forms must never appear: that is the whole point.
	for _, block := range user.Content {
		switch block.(type) {
		case agentcore.ImageContent, agentcore.FileContent, agentcore.AudioContent:
			t.Fatalf("the durable transcript must not carry bytes: %+v", block)
		}
	}
}

// TestNonImageAttachmentsBecomeDurableReferences pins the Owner rule (2026-11-23):
// a non-image attachment is never silently dropped. It enters the transcript as a
// durable mediaRef (no bytes), which the request boundary turns into a native part
// or a metadata text block.
func TestNonImageAttachmentsBecomeDurableReferences(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"text":        "read the log",
		"attachments": []map[string]any{{"fileId": "blob-doc", "name": "a.log", "mime": "text/plain"}},
	})
	msgs := MessagesFromEvents([]store.Event{{Seq: 1, Kind: store.EventUserMessage, Payload: payload}}, nil)
	user := msgs[0].(agentcore.UserMessage)
	if len(user.Content) != 2 {
		t.Fatalf("content = %#v, want the text block plus one media reference", user.Content)
	}
	ref, ok := user.Content[1].(agentcore.MediaRefContent)
	if !ok {
		t.Fatalf("second block = %T, want MediaRefContent", user.Content[1])
	}
	if ref.BlobID != "blob-doc" || ref.Kind != agentcore.MediaKindFile {
		t.Fatalf("reference = %+v, want a generic file reference", ref)
	}
}

// fakeImageResolver records what the runner asked for and answers with a fixed
// form, so the boundary crossing is observable without a blob store.
type fakeImageResolver struct {
	calls   int
	lastRef agentcore.ImageRefContent
	caps    provider.ImageCaps
	form    string
	err     error
}

func (f *fakeImageResolver) Resolve(_ context.Context, workspace string, target provider.ImageTarget, ref agentcore.ImageRefContent, caps provider.ImageCaps) (provider.ResolvedImage, error) {
	f.calls++
	f.lastRef = ref
	f.caps = caps
	if f.err != nil {
		return provider.ResolvedImage{}, f.err
	}
	content := agentcore.ImageContent{Type: agentcore.ContentTypeImage, MimeType: ref.MimeType}
	switch f.form {
	case provider.ImageFormURL:
		content.URL = "https://keel.example/v1/files/" + ref.BlobID
	default:
		f.form = provider.ImageFormBase64
		content.Data = "QUJD"
	}
	return provider.ResolvedImage{Content: content, Form: f.form}, nil
}

// userImageTranscript writes one committed user.message carrying an image
// attachment and returns the runner's view of the log.
func userImageTranscript(t *testing.T) []store.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"text":        "look",
		"requestId":   "req-image",
		"attachments": []map[string]any{{"fileId": uuid.NewString(), "name": "shot.png", "mime": "image/png"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return []store.Event{{Seq: 1, Kind: store.EventUserMessage, Payload: payload}}
}

// TestResolveImagesReplacesReferencesWithTheChosenForm pins the happy path: the
// resolver is consulted with the attempt's provider and capabilities, and the
// transcript view the loop receives carries the wire block.
func TestResolveImagesReplacesReferencesWithTheChosenForm(t *testing.T) {
	events := userImageTranscript(t)
	msgs := MessagesFromEvents(events, nil)
	resolver := &fakeImageResolver{form: provider.ImageFormURL}
	runner := &Runner{Logger: discardLog()}

	resolved, report := runner.resolveImages(context.Background(), msgs, imageResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		target:    provider.ImageTarget{Provider: "anthropic", BaseURL: "https://api.anthropic.com/v1"},
		caps:      provider.ImageCaps{SupportsFileID: true, SupportsImageURL: true},
	})
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
	if resolver.lastRef.BlobID == "" {
		t.Fatal("the resolver must receive the reference's blob id")
	}
	if resolver.caps.SupportsFileID != true {
		t.Fatalf("caps forwarded = %+v, want the attempt's declaration", resolver.caps)
	}
	if len(report.failures) != 0 {
		t.Fatalf("report failures = %+v, want none", report.failures)
	}
	if report.forms[provider.ImageFormURL] != 1 {
		t.Fatalf("report forms = %+v, want one url", report.forms)
	}
	user := resolved[0].(agentcore.UserMessage)
	image, ok := user.Content[1].(agentcore.ImageContent)
	if !ok {
		t.Fatalf("second block = %T, want the wire image", user.Content[1])
	}
	if image.URL == "" || image.Data != "" {
		t.Fatalf("wire image = %+v, want the url form", image)
	}
	// The original transcript view must be untouched: a later step re-reads it.
	original := msgs[0].(agentcore.UserMessage)
	if _, ok := original.Content[1].(agentcore.ImageRefContent); !ok {
		t.Fatalf("the loaded transcript was mutated: %#v", original.Content[1])
	}
}

// TestResolveImagesWithNoResolverDegradesToANotice pins the unwired deployment:
// a reference becomes a text notice naming the reason, the run continues, and the
// report records the failure (which the runner logs at warn).
func TestResolveImagesWithNoResolverDegradesToANotice(t *testing.T) {
	msgs := MessagesFromEvents(userImageTranscript(t), nil)
	runner := &Runner{Logger: discardLog()}

	resolved, report := runner.resolveImages(context.Background(), msgs, imageResolution{
		workspace: "tenant-1",
		target:    provider.ImageTarget{Provider: "openai"},
	})
	if len(report.failures) != 1 {
		t.Fatalf("report failures = %+v, want one", report.failures)
	}
	if !strings.Contains(report.failures[0].err.Error(), "no image pipeline") {
		t.Fatalf("failure = %v, want it to name the missing pipeline", report.failures[0].err)
	}
	user := resolved[0].(agentcore.UserMessage)
	if len(user.Content) != 2 {
		t.Fatalf("content = %#v, want text plus the notice", user.Content)
	}
	notice, ok := user.Content[1].(agentcore.TextContent)
	if !ok {
		t.Fatalf("second block = %T, want a text notice", user.Content[1])
	}
	if !strings.Contains(notice.Text, "image") || !strings.Contains(notice.Text, "omitted") {
		t.Fatalf("notice = %q, want it to name the dropped image", notice.Text)
	}
	// The text prompt still reaches the model: the run must not lose the turn.
	if text := ContentText(user.Content); !strings.Contains(text, "look") {
		t.Fatalf("the user's text was lost: %q", text)
	}
}

// TestResolveImagesRecordsAResolverFailure pins the failure path with a wired
// resolver: the error is reported and the notice names it, so an operator can
// tell a missing blob from a missing pipeline.
func TestResolveImagesRecordsAResolverFailure(t *testing.T) {
	msgs := MessagesFromEvents(userImageTranscript(t), nil)
	resolver := &fakeImageResolver{err: provider.ErrImageTooLarge}
	runner := &Runner{Logger: discardLog()}

	resolved, report := runner.resolveImages(context.Background(), msgs, imageResolution{
		resolver:  resolver,
		workspace: "tenant-1",
		target:    provider.ImageTarget{Provider: "custom"},
	})
	if len(report.failures) != 1 {
		t.Fatalf("report failures = %+v, want one", report.failures)
	}
	notice := resolved[0].(agentcore.UserMessage).Content[1].(agentcore.TextContent)
	if !strings.Contains(notice.Text, "too large") && !strings.Contains(notice.Text, "exceeds") {
		t.Fatalf("notice = %q, want the resolver's reason", notice.Text)
	}
}

// TestResolveImagesLeavesTextOnlyTranscriptsAlone pins the no-op path: a
// transcript without references is returned unchanged (no copy, no resolver call).
func TestResolveImagesLeavesTextOnlyTranscriptsAlone(t *testing.T) {
	msgs := MessagesFromEvents([]store.Event{{
		Seq:     1,
		Kind:    store.EventUserMessage,
		Payload: json.RawMessage(`{"text":"hello"}`),
	}}, nil)
	resolver := &fakeImageResolver{}
	runner := &Runner{Logger: discardLog()}
	resolved, report := runner.resolveImages(context.Background(), msgs, imageResolution{resolver: resolver})
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolver.calls)
	}
	if !report.empty() {
		t.Fatalf("report = %+v, want empty", report)
	}
	if len(resolved) != 1 {
		t.Fatalf("messages = %d, want 1", len(resolved))
	}
}

// TestImageCapsForHonoursTheModelDeclaration pins the capability gate: a model
// that declares no image input yields the zero declaration, so a resolver never
// uploads bytes for a request the driver would refuse.
func TestImageCapsForHonoursTheModelDeclaration(t *testing.T) {
	withImages := ActiveModel{
		Properties: &ModelProperties{ImageInput: true},
		ImageCaps:  provider.ImageCaps{SupportsFileID: true, MaxImageBytes: 1 << 20},
	}
	if caps := imageCapsFor(withImages); !caps.SupportsFileID {
		t.Fatalf("caps = %+v, want the declaration", caps)
	}
	withoutImages := ActiveModel{
		Properties: &ModelProperties{ImageInput: false},
		ImageCaps:  provider.ImageCaps{SupportsFileID: true},
	}
	if caps := imageCapsFor(withoutImages); caps.SupportsFileID {
		t.Fatalf("caps = %+v, want nothing (the model declares no image input)", caps)
	}
	unknown := ActiveModel{ImageCaps: provider.ImageCaps{SupportsImageURL: true}}
	if caps := imageCapsFor(unknown); !caps.SupportsImageURL {
		t.Fatalf("caps = %+v, want the declaration even when Properties is nil", caps)
	}
}

// TestImageFallbackWithoutBlobIDDoesNotClaimStoredBytes pins the honest
// reference rule for the image path too: a reference with no blob id cannot
// claim the platform stores bytes for it.
func TestImageFallbackWithoutBlobIDDoesNotClaimStoredBytes(t *testing.T) {
	text := imageFallbackText(agentcore.ImageRefContent{
		Type: agentcore.ContentTypeImageRef, Name: "shot.png", MimeType: "image/png",
	}, "the selected model does not declare image input")
	if strings.Contains(text, "Stored bytes") {
		t.Fatalf("fallback = %q, want no stored-bytes claim without a blobId", text)
	}
	withID := imageFallbackText(agentcore.ImageRefContent{
		Type: agentcore.ContentTypeImageRef, BlobID: "blob-1", Name: "shot.png",
	}, "reason")
	if !strings.Contains(withID, "blobId=blob-1") {
		t.Fatalf("fallback = %q, want the blob reference when one exists", withID)
	}
}

// TestImageWorkspaceDefaultsToTheTenant pins the blob scope: the tenant is the
// default, and an explicit override wins.
func TestImageWorkspaceDefaultsToTheTenant(t *testing.T) {
	tenant := uuid.New()
	lease := kernel.Lease{Scope: kernel.Scope{TenantID: tenant}}
	if got := (&Runner{}).imageWorkspace(lease); got != tenant.String() {
		t.Fatalf("workspace = %q, want the tenant id", got)
	}
	if got := (&Runner{ImageWorkspace: "custom"}).imageWorkspace(lease); got != "custom" {
		t.Fatalf("workspace = %q, want the override", got)
	}
}

// TestImageDegradationLogCarriesOnlySafeFacts is the image half of the
// attachment log contract: blob id, MIME and size reach the log; the display
// name and bytes do not.
func TestImageDegradationLogCarriesOnlySafeFacts(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	report := imageResolutionReport{
		fallbacks: []imageFallback{{blobID: "blob-img", mime: "image/png", size: 4096}},
	}
	logImageResolution(logger, kernel.Lease{Scope: kernel.Scope{SessionID: uuid.New()}},
		ActiveModel{ProviderID: "provider-1", Model: "model-1"}, report)
	line := buffer.String()
	for _, want := range []string{"blob-img", "image/png", "4096", "does not declare image input"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line lacks %q: %s", want, line)
		}
	}
	for _, forbidden := range []string{"screenshot.png", "iVBORw0KGgo"} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("log line leaked %q: %s", forbidden, line)
		}
	}
}
