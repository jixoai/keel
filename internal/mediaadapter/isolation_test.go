package mediaadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProviderPanicIsIsolated(t *testing.T) {
	ctx := context.Background()

	t.Run("image", func(t *testing.T) {
		broken := &ImageAdapter{
			Provider: &fakeImageProvider{panicValue: "image backend exploded"},
			Store:    &recordingStore{},
		}
		_, err := broken.Generate(ctx, "s", ImageRequest{Prompt: "x"})
		var typed *Error
		if !asAdapterError(err, &typed) {
			t.Fatalf("panic escaped as %v", err)
		}
		if typed.Kind != KindProviderUnavailable || !typed.Retryable {
			t.Fatalf("error = %+v", typed)
		}
		if !strings.Contains(typed.Message, "image backend exploded") {
			t.Fatalf("panic reason lost: %q", typed.Message)
		}
		t.Logf("panicking image provider -> %v", typed)

		healthy := &ImageAdapter{
			Provider: &fakeImageProvider{batch: ImageBatch{Assets: []ProviderAsset{{Data: []byte("ok")}}}},
			Store:    &recordingStore{},
		}
		result, err := healthy.Generate(ctx, "s", ImageRequest{Prompt: "x"})
		if err != nil || len(result.Images) != 1 {
			t.Fatalf("healthy provider after panic: result=%+v err=%v", result, err)
		}
		t.Logf("healthy image provider after panic -> image=%+v", result.Images[0])
	})

	t.Run("video", func(t *testing.T) {
		broken := &VideoAdapter{
			Provider: &fakeVideoProvider{panicValue: errors.New("video backend exploded")},
			Store:    &recordingStore{},
		}
		_, err := broken.Generate(ctx, "s", VideoRequest{Prompt: "x"})
		var typed *Error
		if !asAdapterError(err, &typed) || typed.Kind != KindProviderUnavailable {
			t.Fatalf("error = %v", err)
		}
		healthy := &VideoAdapter{
			Provider: &fakeVideoProvider{batch: VideoBatch{Assets: []ProviderAsset{{Data: []byte("ok")}}}},
			Store:    &recordingStore{},
		}
		if _, err := healthy.Generate(ctx, "s", VideoRequest{Prompt: "x"}); err != nil {
			t.Fatalf("healthy video provider: %v", err)
		}
	})

	t.Run("speech", func(t *testing.T) {
		broken := &SpeechAdapter{
			Provider: &fakeSpeechProvider{panicValue: "speech backend exploded"},
			Store:    &recordingStore{},
		}
		_, err := broken.Synthesize(ctx, "s", SynthesizeRequest{Text: "x"})
		var typed *Error
		if !asAdapterError(err, &typed) || typed.Kind != KindProviderUnavailable {
			t.Fatalf("error = %v", err)
		}
		healthy := &SpeechAdapter{
			Provider: &fakeSpeechProvider{audio: SpeechAudio{Data: []byte("wav"), Mime: "audio/wav"}},
			Store:    &recordingStore{},
		}
		if _, err := healthy.Synthesize(ctx, "s", SynthesizeRequest{Text: "x"}); err != nil {
			t.Fatalf("healthy speech provider: %v", err)
		}
	})
}

func TestDecisionProviderFailureDoesNotPoisonAnotherClient(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"exploded"}}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"model":"jev-1.14","answers":%s}`, decisionAnswersFixture)
	}))
	defer good.Close()

	badClient, err := NewDecisionClient(DecisionConfig{Adapter: DecisionAdapterTypeSafe, BaseURL: bad.URL, APIKey: "k", Model: "jev-1.14"})
	if err != nil {
		t.Fatal(err)
	}
	goodClient, err := NewDecisionClient(DecisionConfig{Adapter: DecisionAdapterTypeSafe, BaseURL: good.URL, APIKey: "k", Model: "jev-1.14"})
	if err != nil {
		t.Fatal(err)
	}
	request := DecisionRequest{State: json.RawMessage(`{"ticket":"x"}`), Questions: decisionQuestions()}
	if _, err := badClient.Decide(context.Background(), request); !errors.Is(err, ErrDecisionUnavailable) {
		t.Fatalf("bad client error = %v", err)
	}
	outcome, err := goodClient.Decide(context.Background(), request)
	if err != nil || len(outcome.Answers) != 3 {
		t.Fatalf("good client after failure: outcome=%+v err=%v", outcome, err)
	}
}

func TestDecisionTimeoutFollowsPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		fmt.Fprintf(w, `{"answers":%s}`, decisionAnswersFixture)
	}))
	defer server.Close()
	client, err := NewDecisionClient(DecisionConfig{
		Adapter: DecisionAdapterTypeSafe, BaseURL: server.URL, APIKey: "k",
		Model: "jev-1.14", Client: &http.Client{Timeout: 20 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := DecisionRequest{State: json.RawMessage(`{"ticket":"x"}`), Questions: decisionQuestions()}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Decide(ctx, request); !errors.Is(err, ErrDecisionUnavailable) {
		t.Fatalf("timeout must be decision unavailable, got %v", err)
	}
	request.Policy = DecisionPolicy{Mode: DecisionModeShadow}
	outcome, err := client.Decide(ctx, request)
	if err != nil {
		t.Fatalf("shadow timeout returned an error: %v", err)
	}
	if outcome == nil || !outcome.Shadow || outcome.Failure == "" || len(outcome.Answers) != 0 {
		t.Fatalf("shadow timeout outcome = %+v", outcome)
	}
}
