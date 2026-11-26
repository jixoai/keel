package provider

// Modality dispatch wire tests (change keel-harness-v2, modality-input lane).
//
// Every assertion here reads the *encoded request body* — what the vendor
// actually receives — per the Owner rule: a native part exists only where the
// upstream accepts it, and everything else is a text block naming the reason.
// The matrix under test is the one documented in media.go and
// docs/MODEL-INPUT-MODALITIES.md.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// fileRequest builds a CompletionRequest carrying one document block.
func fileRequest(model string, file agentcore.FileContent) CompletionRequest {
	return CompletionRequest{
		Model: model,
		Context: LlmContext{
			Messages: agentcore.MessageList{agentcore.UserMessage{
				RoleField: agentcore.RoleUser,
				Content:   agentcore.ContentList{agentcore.NewTextContent("read this"), file},
			}},
		},
		Config: StreamConfig{APIKey: "test-key"},
	}
}

// audioRequest builds a CompletionRequest carrying one audio block.
func audioRequest(model string, audio agentcore.AudioContent) CompletionRequest {
	return CompletionRequest{
		Model: model,
		Context: LlmContext{
			Messages: agentcore.MessageList{agentcore.UserMessage{
				RoleField: agentcore.RoleUser,
				Content:   agentcore.ContentList{agentcore.NewTextContent("transcribe"), audio},
			}},
		},
		Config: StreamConfig{APIKey: "test-key"},
	}
}

// firstUserParts pulls the content parts of the first user message out of an
// OpenAI chat body.
func firstUserParts(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message["role"] != "user" {
			continue
		}
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(parts))
		for _, part := range parts {
			block, _ := part.(map[string]any)
			out = append(out, block)
		}
		return out
	}
	t.Fatalf("no user content parts in %#v", body["messages"])
	return nil
}

// TestOpenAIChatWireCarriesNativePDF pins the chat wire's document shape: a
// `file` part with a data-URL file_data and a filename (the chat wire accepts
// PDFs only, and a bare base64 string is rejected upstream).
func TestOpenAIChatWireCarriesNativePDF(t *testing.T) {
	raw, err := encodeOpenAIRequest(fileRequest("gpt-test", agentcore.FileContent{
		Type:     agentcore.ContentTypeFile,
		Data:     "JVBERi0xLjQK",
		MimeType: "application/pdf",
		Name:     "report.pdf",
	}))
	if err != nil {
		t.Fatalf("encodeOpenAIRequest: %v", err)
	}
	body := decodeBody(t, raw)
	var filePart map[string]any
	for _, part := range firstUserParts(t, body) {
		if part["type"] == "file" {
			filePart, _ = part["file"].(map[string]any)
		}
	}
	if filePart == nil {
		t.Fatalf("body parts = %#v, want a file part", firstUserParts(t, body))
	}
	if filePart["filename"] != "report.pdf" {
		t.Fatalf("filename = %#v, want the display name", filePart["filename"])
	}
	if filePart["file_data"] != "data:application/pdf;base64,JVBERi0xLjQK" {
		t.Fatalf("file_data = %#v, want a data URL with the PDF MIME", filePart["file_data"])
	}
	if _, ok := filePart["file_id"]; ok {
		t.Fatalf("file part = %#v, want no file_id alongside file_data", filePart)
	}
}

// TestOpenAIChatWireCarriesNativePDFByFileID pins the handle form: the chat wire
// takes a file_id for PDFs, and no file_data travels with it.
func TestOpenAIChatWireCarriesNativePDFByFileID(t *testing.T) {
	raw, err := encodeOpenAIRequest(fileRequest("gpt-test", agentcore.FileContent{
		Type:     agentcore.ContentTypeFile,
		FileID:   "file-abc123",
		MimeType: "application/pdf",
		Name:     "report.pdf",
	}))
	if err != nil {
		t.Fatalf("encodeOpenAIRequest: %v", err)
	}
	body := decodeBody(t, raw)
	var filePart map[string]any
	for _, part := range firstUserParts(t, body) {
		if part["type"] == "file" {
			filePart, _ = part["file"].(map[string]any)
		}
	}
	if filePart["file_id"] != "file-abc123" {
		t.Fatalf("file part = %#v, want the provider handle", filePart)
	}
	if _, ok := filePart["file_data"]; ok {
		t.Fatalf("file part = %#v, want no inline bytes alongside file_id", filePart)
	}
}

// TestOpenAIChatWireCarriesNativeImage pins the pre-existing image path after the
// general dispatch refactor: the image_url part still arrives with the data URI.
func TestOpenAIChatWireCarriesNativeImage(t *testing.T) {
	raw, err := encodeOpenAIRequest(imageRequest("gpt-test", agentcore.ImageContent{
		Type: agentcore.ContentTypeImage, Data: "QUJD", MimeType: "image/png",
	}))
	if err != nil {
		t.Fatalf("encodeOpenAIRequest: %v", err)
	}
	body := decodeBody(t, raw)
	target := findChatImageURL(t, body)
	if target != "data:image/png;base64,QUJD" {
		t.Fatalf("image_url = %q, want the data URI", target)
	}
	if got := ContentTextOfBody(t, body); !strings.Contains(got, "read this") && !strings.Contains(got, "look") {
		t.Fatalf("body text = %q, want the surrounding prompt preserved", got)
	}
}

// ContentTextOfBody concatenates text parts of the first user message (test
// helper; the prompt must never be lost when a media part is added).
func ContentTextOfBody(t *testing.T, body map[string]any) string {
	t.Helper()
	var parts []string
	for _, part := range firstUserParts(t, body) {
		if part["type"] == "text" {
			if text, ok := part["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, " ")
}

// TestOpenAIChatWireCarriesNativeAudio pins the input_audio shape: base64 data
// plus the wire format token, no data URI.
func TestOpenAIChatWireCarriesNativeAudio(t *testing.T) {
	raw, err := encodeOpenAIRequest(audioRequest("gpt-audio", agentcore.AudioContent{
		Type: agentcore.ContentTypeAudio, Data: "UklGRg==", Format: "wav", MimeType: "audio/wav",
	}))
	if err != nil {
		t.Fatalf("encodeOpenAIRequest: %v", err)
	}
	body := decodeBody(t, raw)
	var audioPart map[string]any
	for _, part := range firstUserParts(t, body) {
		if part["type"] == "input_audio" {
			audioPart, _ = part["input_audio"].(map[string]any)
		}
	}
	if audioPart == nil {
		t.Fatalf("body parts = %#v, want an input_audio part", firstUserParts(t, body))
	}
	if audioPart["data"] != "UklGRg==" || audioPart["format"] != "wav" {
		t.Fatalf("input_audio = %#v, want inline data and format wav", audioPart)
	}
}

// TestOpenAIChatRefusesADocumentTheWireCannotCarry pins the no-fake-part rule:
// a non-PDF document or an unsupported form becomes a text notice in the body.
func TestOpenAIChatRefusesADocumentTheWireCannotCarry(t *testing.T) {
	for _, file := range []agentcore.FileContent{
		{Type: agentcore.ContentTypeFile, Data: "QUJD", MimeType: "text/markdown", Name: "notes.md"},
		{Type: agentcore.ContentTypeFile, URL: "https://keel.example/f.pdf", MimeType: "application/pdf", Name: "f.pdf"},
	} {
		raw, err := encodeOpenAIRequest(fileRequest("gpt-test", file))
		if err != nil {
			t.Fatalf("encodeOpenAIRequest: %v", err)
		}
		body := decodeBody(t, raw)
		parts := firstUserParts(t, body)
		last := parts[len(parts)-1]
		if last["type"] != "text" || !strings.Contains(fmt.Sprint(last["text"]), "document omitted") {
			t.Fatalf("part = %#v, want an explicit document-omitted notice", last)
		}
	}
}

// TestResponsesWireCarriesNativePDF pins the Responses input_file shapes for
// file_id, inline (data URL + filename) and file_url.
func TestResponsesWireCarriesNativePDF(t *testing.T) {
	cases := []struct {
		name   string
		file   agentcore.FileContent
		field  string
		expect string
	}{
		{
			name:   "file id",
			file:   agentcore.FileContent{Type: agentcore.ContentTypeFile, FileID: "file_resp_1", MimeType: "application/pdf"},
			field:  "file_id",
			expect: "file_resp_1",
		},
		{
			name: "inline",
			file: agentcore.FileContent{Type: agentcore.ContentTypeFile, Data: "JVBERi0xLjQK",
				MimeType: "application/pdf", Name: "report.pdf"},
			field:  "file_data",
			expect: "data:application/pdf;base64,JVBERi0xLjQK",
		},
		{
			name:   "file url",
			file:   agentcore.FileContent{Type: agentcore.ContentTypeFile, URL: "https://keel.example/v1/files/x", MimeType: "application/pdf"},
			field:  "file_url",
			expect: "https://keel.example/v1/files/x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := fileRequest("gpt-test", tc.file)
			parts, ok := mediaInputParts(req.Context.Messages[0])
			if !ok {
				t.Fatal("the message carries a document; mediaInputParts must say so")
			}
			raw, err := json.Marshal(parts)
			if err != nil {
				t.Fatalf("marshal parts: %v", err)
			}
			var decoded []map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal parts: %v", err)
			}
			var filePart map[string]any
			for _, part := range decoded {
				if part["type"] == "input_file" {
					filePart = part
				}
			}
			if filePart == nil {
				t.Fatalf("parts = %#v, want an input_file part", decoded)
			}
			if filePart[tc.field] != tc.expect {
				t.Fatalf("%s = %#v, want %q (part %#v)", tc.field, filePart[tc.field], tc.expect, filePart)
			}
			if tc.field == "file_data" && filePart["filename"] != "report.pdf" {
				t.Fatalf("filename = %#v, want the inline document's name", filePart["filename"])
			}
		})
	}
}

// TestResponsesWireNeverSendsInputAudio pins the honest gap: audio on the
// Responses wire is a text notice naming the attachment, not an input_audio part
// the upstream rejects.
func TestResponsesWireNeverSendsInputAudio(t *testing.T) {
	req := audioRequest("gpt-test", agentcore.AudioContent{
		Type: agentcore.ContentTypeAudio, Data: "UklGRg==", Format: "wav", MimeType: "audio/wav", Name: "memo.wav",
	})
	parts, ok := mediaInputParts(req.Context.Messages[0])
	if !ok {
		t.Fatal("the message carries audio; mediaInputParts must say so")
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		t.Fatalf("marshal parts: %v", err)
	}
	if strings.Contains(string(raw), `"type":"input_audio"`) {
		t.Fatalf("parts = %s, want no input_audio part on the Responses wire", raw)
	}
	if !strings.Contains(string(raw), "audio omitted") || !strings.Contains(string(raw), "memo.wav") {
		t.Fatalf("parts = %s, want an explicit audio notice naming the attachment", raw)
	}
}

// TestAnthropicWireCarriesNativePDF pins the Messages document block for the
// forms the driver implements: base64 and url (a file_id source needs a beta
// header this driver does not send, so the resolver never produces one here).
func TestAnthropicWireCarriesNativePDF(t *testing.T) {
	t.Run("base64", func(t *testing.T) {
		body := anthropicWireBody(t, fileRequest("claude-sonnet-4-5", agentcore.FileContent{
			Type: agentcore.ContentTypeFile, Data: "JVBERi0xLjQK", MimeType: "application/pdf", Name: "report.pdf",
		}))
		source := decodeAnthropicDocument(t, body)
		if source["type"] != "base64" || source["media_type"] != "application/pdf" || source["data"] != "JVBERi0xLjQK" {
			t.Fatalf("source = %#v, want a base64 PDF source", source)
		}
	})
	t.Run("url", func(t *testing.T) {
		body := anthropicWireBody(t, fileRequest("claude-sonnet-4-5", agentcore.FileContent{
			Type: agentcore.ContentTypeFile, URL: "https://keel.example/v1/files/p", MimeType: "application/pdf",
		}))
		source := decodeAnthropicDocument(t, body)
		if source["type"] != "url" || source["url"] != "https://keel.example/v1/files/p" {
			t.Fatalf("source = %#v, want a url document source", source)
		}
	})
}

// TestAnthropicWireNeverSendsAudio pins the Messages API's honest gap.
func TestAnthropicWireNeverSendsAudio(t *testing.T) {
	body := anthropicWireBody(t, audioRequest("claude-sonnet-4-5", agentcore.AudioContent{
		Type: agentcore.ContentTypeAudio, Data: "UklGRg==", Format: "wav", MimeType: "audio/wav", Name: "memo.wav",
	}))
	messages, _ := body["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	blocks, _ := first["content"].([]any)
	var notice map[string]any
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if strings.Contains(fmt.Sprint(block["text"]), "audio omitted") {
			notice = block
		}
	}
	if notice == nil || notice["type"] != "text" {
		t.Fatalf("blocks = %#v, want an explicit audio notice", blocks)
	}
}

// decodeAnthropicDocument pulls the first document block's source out of a body.
func decodeAnthropicDocument(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	messages, _ := body["messages"].([]any)
	if len(messages) == 0 {
		t.Fatalf("body messages = %#v", body["messages"])
	}
	first, _ := messages[0].(map[string]any)
	blocks, ok := first["content"].([]any)
	if !ok {
		t.Fatalf("message content = %#v, want a block list", first["content"])
	}
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block["type"] == "document" {
			source, _ := block["source"].(map[string]any)
			return source
		}
	}
	t.Fatalf("no document block in %#v", blocks)
	return nil
}

// TestNativePDFReachesAMockProviderEndToEnd drives the real chat driver against a
// mock vendor and asserts the body it received: the resolver's inline document
// survives the encoder, the transport and the auth header.
func TestNativePDFReachesAMockProviderEndToEnd(t *testing.T) {
	var seen map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openAISSE("ok"))
	}))
	defer server.Close()
	driver := NewOpenAICompatibleProvider(server.URL, nil)
	stream, err := driver.StreamCompletion(context.Background(), fileRequest("gpt-test", agentcore.FileContent{
		Type: agentcore.ContentTypeFile, Data: "JVBERi0xLjQK", MimeType: "application/pdf", Name: "report.pdf",
	}))
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	for range stream.Events() {
	}
	if seen == nil {
		t.Fatal("the mock server saw no body")
	}
	var filePart map[string]any
	for _, part := range firstUserParts(t, seen) {
		if part["type"] == "file" {
			filePart, _ = part["file"].(map[string]any)
		}
	}
	if filePart == nil || filePart["file_data"] != "data:application/pdf;base64,JVBERi0xLjQK" {
		t.Fatalf("file part = %#v, want the inline PDF on the wire", filePart)
	}
}

// TestMediaKindAndAudioFormatClassification pins the MIME/extension classifier
// both the transcript rebuild and the resolver use.
func TestMediaKindAndAudioFormatClassification(t *testing.T) {
	cases := []struct {
		mime, name, want string
	}{
		{"application/pdf", "a.pdf", agentcore.MediaKindPDF},
		{"", "b.PDF", agentcore.MediaKindPDF},
		{"audio/wav", "memo.wav", agentcore.MediaKindAudio},
		{"", "memo.mp3", agentcore.MediaKindAudio},
		{"video/mp4", "clip.mp4", agentcore.MediaKindVideo},
		{"text/plain", "notes.log", agentcore.MediaKindFile},
		{"text/plain", "fake.pdf", agentcore.MediaKindFile},
		{"application/zip", "fake.mp3", agentcore.MediaKindFile},
		{"application/zip", "", agentcore.MediaKindFile},
	}
	for _, tc := range cases {
		if got := MediaKindFromMime(tc.mime, tc.name); got != tc.want {
			t.Fatalf("MediaKindFromMime(%q, %q) = %q, want %q", tc.mime, tc.name, got, tc.want)
		}
		if got := agentcore.InputKindForMime(tc.mime, tc.name); got != tc.want {
			t.Fatalf("agentcore.InputKindForMime(%q, %q) = %q, want %q (the two classifiers must agree)", tc.mime, tc.name, got, tc.want)
		}
	}
	for mime, want := range map[string]string{"audio/wav": "wav", "audio/mpeg": "mp3", "audio/x-wav": "wav", "audio/mp4": ""} {
		if got := AudioFormatFromMime(mime, ""); got != want {
			t.Fatalf("AudioFormatFromMime(%q) = %q, want %q", mime, got, want)
		}
	}
}

// TestMediaCapsMatrix pins the declared protocol matrix (the docs table is
// generated from this function).
func TestMediaCapsMatrix(t *testing.T) {
	chat := MediaCapsForAPI("openai-chat-completions")
	if !chat.File.SupportsMime("application/pdf") || !chat.File.SupportsInline || !chat.File.SupportsFileID {
		t.Fatalf("chat file caps = %+v, want PDF inline and file_id", chat.File)
	}
	if chat.File.SupportsFileURL {
		t.Fatalf("chat file caps = %+v, want no url form (the chat wire rejects one)", chat.File)
	}
	if !chat.Audio.SupportsFormat("wav") || !chat.Audio.SupportsFormat("mp3") {
		t.Fatalf("chat audio caps = %+v, want wav and mp3", chat.Audio)
	}
	if chat.Video {
		t.Fatal("chat declares native video; no adapter implements one")
	}

	responses := MediaCapsForAPI("openai-responses")
	if !responses.File.SupportsFileURL || !responses.File.SupportsFileID || !responses.File.SupportsInline {
		t.Fatalf("responses file caps = %+v, want all three forms", responses.File)
	}
	if responses.Audio.SupportsInline {
		t.Fatal("responses declares native audio; the upstream rejects input_audio today")
	}

	anthropic := MediaCapsForAPI("anthropic-messages")
	if anthropic.File.SupportsFileID {
		t.Fatalf("anthropic file caps = %+v, want no file_id (the driver sends no Files beta header)", anthropic.File)
	}
	if !anthropic.File.SupportsFileURL || !anthropic.File.SupportsInline {
		t.Fatalf("anthropic file caps = %+v, want url and inline", anthropic.File)
	}
	if anthropic.Image.SupportsFileID {
		t.Fatalf("anthropic image caps = %+v, want no file_id (the driver sends no Files beta header)", anthropic.Image)
	}
	if anthropic.Audio.SupportsInline || anthropic.Video {
		t.Fatalf("anthropic media caps = %+v, want no audio and no video", anthropic)
	}

	unknown := MediaCapsForAPI("something-else")
	if !unknown.IsEmpty() {
		t.Fatalf("unknown protocol = %+v, want the empty declaration (everything degrades)", unknown)
	}
}

// TestResolveFileForms pins the resolver's degradation order and refusals.
func TestResolveFileForms(t *testing.T) {
	blobs := newMemBlobs()
	blobs.put("blob-pdf", "application/pdf", []byte("%PDF-1.4 payload"))
	resolver := &MediaResolver{ImageResolver: ImageResolver{Blobs: blobs, Files: NewFileCache(8)}}

	got, err := resolver.ResolveFile(context.Background(), "ws",
		ImageTarget{Provider: "openai"},
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-pdf", Name: "report.pdf"},
		FileCaps{SupportsInline: true, Mimes: []string{"application/pdf"}})
	if err != nil {
		t.Fatalf("ResolveFile: %v", err)
	}
	if got.Form != FileFormInline || got.Content.Data == "" {
		t.Fatalf("resolved = %+v, want the inline form", got)
	}
	wantData := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 payload"))
	if got.Content.Data != wantData {
		t.Fatalf("data = %q, want the blob's base64", got.Content.Data)
	}
	if got.Content.Name != "report.pdf" || got.Content.MimeType != "application/pdf" {
		t.Fatalf("content = %+v, want the filename and PDF MIME", got.Content)
	}

	// A wire that declares no form refuses with a reason (the caller falls back).
	_, err = resolver.ResolveFile(context.Background(), "ws",
		ImageTarget{Provider: "custom"},
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-pdf"},
		FileCaps{})
	if err == nil || !strings.Contains(err.Error(), "cannot travel natively") {
		t.Fatalf("err = %v, want a native-form refusal", err)
	}

	// A wire that does not carry the MIME refuses even when it has an inline form.
	_, err = resolver.ResolveFile(context.Background(), "ws",
		ImageTarget{Provider: "custom"},
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-pdf"},
		FileCaps{SupportsInline: true, Mimes: []string{"application/docx"}})
	if err == nil || !strings.Contains(err.Error(), "not carried natively") {
		t.Fatalf("err = %v, want a MIME refusal", err)
	}
}

// TestResolveFileMissingBlobFailsClosed pins the missing-bytes path: the resolver
// refuses with a reason naming the blob, and no base64 is invented.
func TestResolveFileMissingBlobFailsClosed(t *testing.T) {
	resolver := &MediaResolver{ImageResolver: ImageResolver{Blobs: newMemBlobs()}}
	_, err := resolver.ResolveFile(context.Background(), "ws",
		ImageTarget{Provider: "openai"},
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-missing"},
		FileCaps{SupportsInline: true})
	if err == nil || !strings.Contains(err.Error(), "no such blob") {
		t.Fatalf("err = %v, want the missing blob named", err)
	}
}

// TestResolveFileWithoutBlobIDFailsClosed pins the no-blob-id path.
func TestResolveFileWithoutBlobIDFailsClosed(t *testing.T) {
	resolver := &MediaResolver{ImageResolver: ImageResolver{Blobs: newMemBlobs()}}
	_, err := resolver.ResolveFile(context.Background(), "ws",
		ImageTarget{Provider: "openai"},
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef},
		FileCaps{SupportsInline: true})
	if err == nil || !strings.Contains(err.Error(), "no blobId") {
		t.Fatalf("err = %v, want a no-blobId refusal", err)
	}
}

// TestResolveAudioInlineOnly pins the audio resolver: inline base64, wav/mp3 only,
// and a refusal for a wire without an audio part.
func TestResolveAudioInlineOnly(t *testing.T) {
	blobs := newMemBlobs()
	blobs.put("blob-wav", "audio/wav", []byte("RIFFxxxxWAVE"))
	resolver := &MediaResolver{ImageResolver: ImageResolver{Blobs: blobs}}
	got, err := resolver.ResolveAudio(context.Background(), "ws",
		ImageTarget{Provider: "openai"},
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-wav", Name: "memo.wav"},
		AudioCaps{SupportsInline: true, Formats: []string{"wav", "mp3"}})
	if err != nil {
		t.Fatalf("ResolveAudio: %v", err)
	}
	if got.Content.Format != "wav" || got.Content.Data == "" {
		t.Fatalf("resolved = %+v, want inline wav", got)
	}
	// A fmt the wire does not accept is refused rather than sent.
	blobs.put("blob-flac", "audio/flac", []byte("fLaCxxxx"))
	_, err = resolver.ResolveAudio(context.Background(), "ws",
		ImageTarget{Provider: "openai"},
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-flac", Name: "a.flac"},
		AudioCaps{SupportsInline: true, Formats: []string{"wav", "mp3"}})
	if err == nil || !strings.Contains(err.Error(), "not carried natively") {
		t.Fatalf("err = %v, want a format refusal", err)
	}
	// A wire with no audio part is refused.
	_, err = resolver.ResolveAudio(context.Background(), "ws",
		ImageTarget{Provider: "anthropic"},
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-wav"},
		AudioCaps{})
	if err == nil || !strings.Contains(err.Error(), "no audio input part") {
		t.Fatalf("err = %v, want a wire refusal", err)
	}
}

// TestResolveFilePrefersFileHandleWhenDeclared pins the degradation order: a wire
// that declares a file handle produces it before touching inline bytes.
func TestResolveFilePrefersFileHandleWhenDeclared(t *testing.T) {
	blobs := newMemBlobs()
	blobs.put("blob-pdf", "application/pdf", []byte("%PDF-1.4"))
	resolver := &MediaResolver{ImageResolver: ImageResolver{
		Blobs:            blobs,
		Files:            NewFileCache(8),
		uploaderOverride: &fakeUploader{name: "openai", id: "file-uploaded"},
	}}
	got, err := resolver.ResolveFile(context.Background(), "ws",
		ImageTarget{Provider: "openai", APIKey: "key"},
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-pdf", Name: "report.pdf"},
		FileCaps{SupportsFileID: true, SupportsInline: true, Mimes: []string{"application/pdf"}})
	if err != nil {
		t.Fatalf("ResolveFile: %v", err)
	}
	if got.Form != FileFormFileID || got.Content.FileID != "file-uploaded" {
		t.Fatalf("resolved = %+v, want the uploaded handle", got)
	}
	if blobs.encodeCount() != 0 {
		t.Fatal("the inline path ran even though a handle was available")
	}
}

// TestProbeReadsOnlyBoundedMetadata pins the Owner's metadata constraint: the
// probe reads a bounded header, the dimensions come from stdlib decoders, and an
// unsupported format reports unknown rather than guessing.
func TestProbeReadsOnlyBoundedMetadata(t *testing.T) {
	blobs := newMemBlobs()
	// A minimal 1x1 PNG: the stdlib decoder reads it from the header alone.
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde")
	blobs.put("blob-png", "image/png", png)
	resolver := &MediaResolver{ImageResolver: ImageResolver{Blobs: blobs}}
	facts, err := resolver.Probe(context.Background(), "ws",
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-png", Name: "one.png"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if facts.Size != int64(len(png)) || facts.Mime != "image/png" {
		t.Fatalf("facts = %+v, want the blob's size and MIME", facts)
	}
	if facts.Width == 0 || facts.Height == 0 {
		t.Fatalf("facts = %+v, want the stdlib-decoded dimensions", facts)
	}

	// A PDF's page count is deliberately unknown (no parser dependency).
	blobs.put("blob-pdf", "application/pdf", []byte("%PDF-1.4\n%%EOF\n"))
	facts, err = resolver.Probe(context.Background(), "ws",
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-pdf", Name: "doc.pdf"})
	if err != nil {
		t.Fatalf("Probe pdf: %v", err)
	}
	if facts.Pages != 0 {
		t.Fatalf("facts = %+v, want pages unknown (no PDF parser dependency)", facts)
	}

	// A WAV duration comes from the header's byte rate.
	wav := wavHeader(8000, 16, 1, 16000) // 1s of mono 8kHz 16-bit PCM
	blobs.put("blob-wav", "audio/wav", wav)
	facts, err = resolver.Probe(context.Background(), "ws",
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-wav", Name: "memo.wav"})
	if err != nil {
		t.Fatalf("Probe wav: %v", err)
	}
	if facts.DurationMs != 1000 {
		t.Fatalf("duration = %d ms, want 1000", facts.DurationMs)
	}

	// A streaming WAV writes 0xFFFFFFFF as a size placeholder; that is not a
	// duration, so the probe must say unknown rather than fabricate one.
	streaming := wavHeader(8000, 16, 1, 0xFFFFFFFF)
	blobs.put("blob-wav-stream", "audio/wav", streaming)
	facts, err = resolver.Probe(context.Background(), "ws",
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-wav-stream", Name: "live.wav"})
	if err != nil {
		t.Fatalf("Probe streaming wav: %v", err)
	}
	if facts.DurationMs != 0 {
		t.Fatalf("duration = %d ms, want unknown for a placeholder data size", facts.DurationMs)
	}
}

// wavHeader builds a minimal PCM WAV header with a declared data size.
func wavHeader(sampleRate uint32, bits uint16, channels uint16, dataBytes uint32) []byte {
	byteRate := sampleRate * uint32(channels) * uint32(bits/8)
	buf := make([]byte, 44)
	copy(buf[0:4], "RIFF")
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	putU32(buf[16:20], 16)
	putU16(buf[20:22], 1) // PCM
	putU16(buf[22:24], channels)
	putU32(buf[24:28], sampleRate)
	putU32(buf[28:32], byteRate)
	putU16(buf[32:34], channels*bits/8)
	putU16(buf[34:36], bits)
	copy(buf[36:40], "data")
	putU32(buf[40:44], dataBytes)
	return buf
}

func putU16(dst []byte, v uint16) {
	dst[0] = byte(v)
	dst[1] = byte(v >> 8)
}

func putU32(dst []byte, v uint32) {
	dst[0] = byte(v)
	dst[1] = byte(v >> 8)
	dst[2] = byte(v >> 16)
	dst[3] = byte(v >> 24)
}

// TestAnthropicWireRefusesDocumentFormsItCannotCarry pins the honest gap in the
// Messages encoder: a document file_id needs the Files API beta header this
// driver does not send, and a non-PDF MIME has no verified native shape here, so
// both become explicit text notices instead of a part the upstream would reject.
func TestAnthropicWireRefusesDocumentFormsItCannotCarry(t *testing.T) {
	cases := []struct {
		name string
		file agentcore.FileContent
	}{
		{
			name: "file id needs the beta header",
			file: agentcore.FileContent{Type: agentcore.ContentTypeFile, FileID: "file_abc", MimeType: "application/pdf", Name: "a.pdf"},
		},
		{
			name: "non-pdf mime",
			file: agentcore.FileContent{Type: agentcore.ContentTypeFile, Data: "QUJD", MimeType: "text/markdown", Name: "notes.md"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := anthropicWireBody(t, fileRequest("claude-sonnet-4-5", tc.file))
			messages, _ := body["messages"].([]any)
			first, _ := messages[0].(map[string]any)
			blocks, _ := first["content"].([]any)
			if len(blocks) < 2 {
				t.Fatalf("blocks = %#v, want the prompt plus an omission notice", blocks)
			}
			for _, raw := range blocks {
				block, _ := raw.(map[string]any)
				if block["type"] == "document" {
					t.Fatalf("blocks = %#v, want no document block the wire cannot carry", blocks)
				}
			}
			last, _ := blocks[len(blocks)-1].(map[string]any)
			if last["type"] != "text" || !strings.Contains(fmt.Sprint(last["text"]), "document omitted") {
				t.Fatalf("last block = %#v, want an explicit document-omitted notice", last)
			}
		})
	}
}

// TestOpenAIChatAudioRefusalNotices pins the two audio shapes the chat wire
// cannot accept: a block with no bytes and a format outside wav/mp3. Neither may
// be dropped silently.
func TestOpenAIChatAudioRefusalNotices(t *testing.T) {
	for _, audio := range []agentcore.AudioContent{
		{Type: agentcore.ContentTypeAudio, Format: "wav", MimeType: "audio/wav", Name: "empty.wav"},
		{Type: agentcore.ContentTypeAudio, Data: "ZkxhQw==", Format: "flac", MimeType: "audio/flac", Name: "a.flac"},
	} {
		raw, err := encodeOpenAIRequest(audioRequest("gpt-audio", audio))
		if err != nil {
			t.Fatalf("encodeOpenAIRequest: %v", err)
		}
		parts := firstUserParts(t, decodeBody(t, raw))
		last := parts[len(parts)-1]
		if last["type"] != "text" || !strings.Contains(fmt.Sprint(last["text"]), "audio omitted") {
			t.Fatalf("part = %#v, want an explicit audio-omitted notice for %s", last, audio.Name)
		}
	}
}

// TestOpenAIDocumentRefusesAmbiguousSources pins the resolver contract at the
// encoder: exactly one document source must be set, and neither zero nor two
// sources may become a silently empty part.
func TestOpenAIDocumentRefusesAmbiguousSources(t *testing.T) {
	for _, file := range []agentcore.FileContent{
		{Type: agentcore.ContentTypeFile, MimeType: "application/pdf", Name: "a.pdf"},
		{Type: agentcore.ContentTypeFile, Data: "QUJD", FileID: "file_abc", MimeType: "application/pdf", Name: "a.pdf"},
	} {
		raw, err := encodeOpenAIRequest(fileRequest("gpt-test", file))
		if err != nil {
			t.Fatalf("encodeOpenAIRequest: %v", err)
		}
		parts := firstUserParts(t, decodeBody(t, raw))
		last := parts[len(parts)-1]
		if last["type"] != "text" || !strings.Contains(fmt.Sprint(last["text"]), "document omitted") {
			t.Fatalf("part = %#v, want an explicit document-omitted notice", last)
		}
	}
}

// countingBlobs wraps memBlobs to prove the probe reads a bounded header and
// opens the blob exactly once.
type countingBlobs struct {
	*memBlobs
	opens     int
	readBytes int64
}

func (c *countingBlobs) Open(ctx context.Context, workspace, blobID string) (io.ReadCloser, error) {
	rc, err := c.memBlobs.Open(ctx, workspace, blobID)
	if err != nil {
		return nil, err
	}
	c.opens++
	return &countingReadCloser{rc: rc, blobs: c}, nil
}

type countingReadCloser struct {
	rc    io.ReadCloser
	blobs *countingBlobs
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	r.blobs.readBytes += int64(n)
	return n, err
}

func (r *countingReadCloser) Close() error { return r.rc.Close() }

// TestProbeReadsOnlyABoundedHeader pins the Owner's metadata constraint at the
// byte level: a large attachment costs at most mediaHeaderBytes of reads, one
// open, and the reader is closed (the probe never materialises the blob).
func TestProbeReadsOnlyABoundedHeader(t *testing.T) {
	blobs := newMemBlobs()
	blobs.put("blob-big", "application/pdf", bytes.Repeat([]byte("x"), mediaHeaderBytes*3))
	counting := &countingBlobs{memBlobs: blobs}
	resolver := &MediaResolver{ImageResolver: ImageResolver{Blobs: counting}}

	facts, err := resolver.Probe(context.Background(), "ws",
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-big", Name: "big.pdf"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if counting.opens != 1 {
		t.Fatalf("opens = %d, want exactly one bounded open", counting.opens)
	}
	if counting.readBytes != int64(mediaHeaderBytes) {
		t.Fatalf("read %d bytes, want the %d-byte header bound", counting.readBytes, mediaHeaderBytes)
	}
	if !facts.HeaderRead {
		t.Fatalf("facts = %+v, want HeaderRead for a successful bounded read", facts)
	}
	if facts.Pages != 0 {
		t.Fatalf("facts = %+v, want pages unknown (no PDF parser)", facts)
	}
}

// TestProbeUsesClientDimensionsWithoutOpeningTheBlob pins the cheap path: facts
// the client already supplied cost no I/O.
func TestProbeUsesClientDimensionsWithoutOpeningTheBlob(t *testing.T) {
	blobs := newMemBlobs()
	blobs.put("blob-img", "image/png", []byte("\x89PNG\r\n\x1a\n"))
	counting := &countingBlobs{memBlobs: blobs}
	resolver := &MediaResolver{ImageResolver: ImageResolver{Blobs: counting}}

	facts, err := resolver.Probe(context.Background(), "ws",
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-img", Name: "shot.png", Width: 640, Height: 480})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if facts.Width != 640 || facts.Height != 480 {
		t.Fatalf("facts = %+v, want the client dimensions", facts)
	}
	if counting.opens != 0 {
		t.Fatalf("opens = %d, want 0 when the client supplied the dimensions", counting.opens)
	}
}

// TestProbeLeavesVideoAndUnsupportedImagesUnknown pins the no-guess rule: a video
// and an image format without a stdlib decoder report unknown, not a guess.
func TestProbeLeavesVideoAndUnsupportedImagesUnknown(t *testing.T) {
	blobs := newMemBlobs()
	blobs.put("blob-mp4", "video/mp4", append([]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), bytes.Repeat([]byte{0}, 64)...))
	blobs.put("blob-webp", "image/webp", append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{0}, 32)...))
	resolver := &MediaResolver{ImageResolver: ImageResolver{Blobs: blobs}}

	video, err := resolver.Probe(context.Background(), "ws",
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-mp4", Name: "clip.mp4"})
	if err != nil {
		t.Fatalf("Probe video: %v", err)
	}
	if video.Width != 0 || video.Height != 0 || video.DurationMs != 0 || video.Pages != 0 {
		t.Fatalf("video facts = %+v, want every unsupported field unknown", video)
	}
	if !video.HeaderRead {
		t.Fatalf("video facts = %+v, want the bounded read recorded", video)
	}

	webp, err := resolver.Probe(context.Background(), "ws",
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-webp", Name: "pic.webp"})
	if err != nil {
		t.Fatalf("Probe webp: %v", err)
	}
	if webp.Width != 0 || webp.Height != 0 {
		t.Fatalf("webp facts = %+v, want dimensions unknown (no x/image dependency)", webp)
	}
}

// TestProbeCompressedWavStaysUnknown pins the WAV parser's honesty: a compressed
// (non-PCM/float) WAV has no byte-rate duration, so the probe reports unknown
// rather than a wrong number.
func TestProbeCompressedWavStaysUnknown(t *testing.T) {
	blobs := newMemBlobs()
	wav := wavHeader(8000, 4, 1, 16000)
	putU16(wav[20:22], 2) // ADPCM, not PCM (1) or float (3)
	blobs.put("blob-wav", "audio/wav", wav)
	resolver := &MediaResolver{ImageResolver: ImageResolver{Blobs: blobs}}
	facts, err := resolver.Probe(context.Background(), "ws",
		agentcore.MediaRefContent{Type: agentcore.ContentTypeMediaRef, BlobID: "blob-wav", Name: "memo.wav"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if facts.DurationMs != 0 {
		t.Fatalf("duration = %d ms, want unknown for a compressed WAV", facts.DurationMs)
	}
}
