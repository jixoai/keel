package agentcore

// Durable and transport content shapes for input attachments (change
// keel-harness-v2, modality-input lane). The event log stores mediaRef blocks
// and must round-trip them; the provider boundary produces file/audio blocks,
// which the decoder must accept so a replayed transcript never fails to load.

import (
	"encoding/json"
	"testing"
)

// TestMediaReferenceRoundTrips pins the durable shape: every field the fallback
// text renders survives the event log's JSON encoding.
func TestMediaReferenceRoundTrips(t *testing.T) {
	ref := NewMediaRefContent(MediaKindPDF, "blob-1", "report.pdf", "application/pdf", 2048)
	ref.Width, ref.Height = 640, 480
	encoded, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	content, err := decodeContent(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	decoded, ok := content.(MediaRefContent)
	if !ok {
		t.Fatalf("decoded = %T, want MediaRefContent", content)
	}
	if decoded.Kind != MediaKindPDF || decoded.BlobID != "blob-1" || decoded.Name != "report.pdf" ||
		decoded.MimeType != "application/pdf" || decoded.Size != 2048 || decoded.Width != 640 || decoded.Height != 480 {
		t.Fatalf("decoded = %+v, want every reference field preserved", decoded)
	}
}

// TestTransportMediaBlocksDecode pins that the resolver's wire-only blocks are
// part of the closed content vocabulary: a transcript that carries one (a
// hand-built fixture, a future producer) loads instead of erroring on an unknown
// type.
func TestTransportMediaBlocksDecode(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`{"type":"file","fileId":"file-1","mimeType":"application/pdf","name":"a.pdf"}`, ContentTypeFile},
		{`{"type":"audio","data":"UklGRg==","format":"wav","mimeType":"audio/wav"}`, ContentTypeAudio},
	}
	for _, tc := range cases {
		content, err := decodeContent(json.RawMessage(tc.raw))
		if err != nil {
			t.Fatalf("decodeContent(%s): %v", tc.raw, err)
		}
		switch tc.want {
		case ContentTypeFile:
			file, ok := content.(FileContent)
			if !ok || file.FileID != "file-1" || file.MimeType != "application/pdf" {
				t.Fatalf("decoded = %#v, want a FileContent", content)
			}
		case ContentTypeAudio:
			audio, ok := content.(AudioContent)
			if !ok || audio.Format != "wav" || audio.Data != "UklGRg==" {
				t.Fatalf("decoded = %#v, want an AudioContent", content)
			}
		}
	}
}

// TestInputKindForMime pins the classifier the transcript rebuild and the
// dispatcher share: images have their own durable reference, so an image MIME is
// a generic file here, and unknown inputs are files rather than a guess.
func TestInputKindForMime(t *testing.T) {
	cases := []struct {
		mime, name, want string
	}{
		{"application/pdf", "a.pdf", MediaKindPDF},
		{"application/pdf; charset=binary", "", MediaKindPDF},
		{"", "A.PDF", MediaKindPDF},
		{"audio/wav", "a.wav", MediaKindAudio},
		{"audio/mpeg", "", MediaKindAudio},
		{"", "a.mp3", MediaKindAudio},
		{"video/mp4", "clip.mp4", MediaKindVideo},
		{"image/png", "shot.png", MediaKindFile},
		{"text/plain", "notes.log", MediaKindFile},
		{"", "", MediaKindFile},
	}
	for _, tc := range cases {
		if got := InputKindForMime(tc.mime, tc.name); got != tc.want {
			t.Fatalf("InputKindForMime(%q, %q) = %q, want %q", tc.mime, tc.name, got, tc.want)
		}
	}
}
