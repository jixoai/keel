package mediaadapter

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/layerfs"
)

func openTestLayerFS(t *testing.T) *layerfs.Store {
	t.Helper()
	store, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func artifactHash(t *testing.T, artifact Artifact) layerfs.Hash {
	t.Helper()
	hash, err := layerfs.ParseHash(artifact.BlobID)
	if err != nil {
		t.Fatalf("blob id %q: %v", artifact.BlobID, err)
	}
	return hash
}

func readLayerFSObject(t *testing.T, store *layerfs.Store, hash layerfs.Hash) []byte {
	t.Helper()
	object, err := store.Open(hash)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = object.Close() }()
	data, err := io.ReadAll(object)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLayerFSStorePersistsEveryMediaFamily(t *testing.T) {
	store := openTestLayerFS(t)
	ctx := context.Background()
	imageBytes := []byte("image-bytes")
	videoBytes := []byte("video-bytes")
	audioBytes := []byte("audio-bytes")

	imageAdapter := &ImageAdapter{
		Provider: &fakeImageProvider{batch: ImageBatch{Assets: []ProviderAsset{{Data: imageBytes, Mime: "image/png", Width: 4, Height: 3}}}},
		Store:    &LayerFSStore{Store: store},
	}
	imageResult, err := imageAdapter.Generate(ctx, "session-image", ImageRequest{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}

	videoAdapter := &VideoAdapter{
		Provider: &fakeVideoProvider{batch: VideoBatch{Assets: []ProviderAsset{{Data: videoBytes, Mime: "video/mp4", DurationMillis: 2000}}}},
		Store:    &LayerFSStore{Store: store},
	}
	videoResult, err := videoAdapter.Generate(ctx, "session-video", VideoRequest{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}

	speechAdapter := &SpeechAdapter{
		Provider: &fakeSpeechProvider{audio: SpeechAudio{Data: audioBytes, Mime: "audio/wav", DurationMillis: 500}},
		Store:    &LayerFSStore{Store: store},
	}
	speechResult, err := speechAdapter.Synthesize(ctx, "session-speech", SynthesizeRequest{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name     string
		artifact Artifact
		want     []byte
		kind     string
	}{
		{name: "image", artifact: imageResult.Images[0], want: imageBytes, kind: ArtifactImage},
		{name: "video", artifact: videoResult.Videos[0], want: videoBytes, kind: ArtifactVideo},
		{name: "audio", artifact: speechResult.Audio, want: audioBytes, kind: ArtifactAudio},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.artifact.Kind != testCase.kind {
				t.Fatalf("kind = %q", testCase.artifact.Kind)
			}
			hash := artifactHash(t, testCase.artifact)
			if got := readLayerFSObject(t, store, hash); string(got) != string(testCase.want) {
				t.Fatalf("stored bytes = %q", got)
			}
			if store.RefCount(hash) != 1 {
				t.Fatalf("ref count = %d, want 1", store.RefCount(hash))
			}
			if testCase.artifact.Size != int64(len(testCase.want)) {
				t.Fatalf("reported size = %d", testCase.artifact.Size)
			}
			t.Logf("artifact kind=%s blobId=%s size=%d refs=%d bytes=%q",
				testCase.artifact.Kind, testCase.artifact.BlobID, testCase.artifact.Size,
				store.RefCount(hash), testCase.want)
		})
	}

	// Results carry references, never the bytes themselves.
	for _, result := range []any{imageResult, videoResult, speechResult} {
		raw, _ := json.Marshal(result)
		for _, forbidden := range []string{"image-bytes", "video-bytes", "audio-bytes", "base64"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("result leaked %q: %s", forbidden, raw)
			}
		}
	}

	// Identical bytes dedupe to one object but keep one reference per session.
	before := store.UsageBytes()
	second, err := imageAdapter.Generate(ctx, "session-image-2", ImageRequest{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if store.UsageBytes() != before {
		t.Fatalf("dedupe stored the object twice: %d -> %d", before, store.UsageBytes())
	}
	hash := artifactHash(t, second.Images[0])
	if store.RefCount(hash) != 2 {
		t.Fatalf("ref count after second session = %d, want 2", store.RefCount(hash))
	}
}

func TestLayerFSStoreRequiresSession(t *testing.T) {
	store := openTestLayerFS(t)
	adapter := &ImageAdapter{
		Provider: &fakeImageProvider{batch: ImageBatch{Assets: []ProviderAsset{{Data: []byte("x")}}}},
		Store:    &LayerFSStore{Store: store},
	}
	_, err := adapter.Generate(context.Background(), "", ImageRequest{Prompt: "x"})
	var typed *Error
	if !asAdapterError(err, &typed) || typed.Kind != KindRequest {
		t.Fatalf("error = %v", err)
	}
}
