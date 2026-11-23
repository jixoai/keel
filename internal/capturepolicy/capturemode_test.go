package capturepolicy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jixoai/keel/internal/capturepolicy"
)

func TestParseAndClosedVocabulary(t *testing.T) {
	for _, mode := range []capturepolicy.CaptureMode{
		capturepolicy.CaptureModePerToolCall, capturepolicy.CaptureModeCheckpoint, capturepolicy.CaptureModeManual,
	} {
		got, err := capturepolicy.ParseCaptureMode(string(mode))
		if err != nil || got != mode {
			t.Fatalf("ParseCaptureMode(%q) = %q/%v", mode, got, err)
		}
		if !got.Valid() {
			t.Fatalf("%q is not Valid", got)
		}
		if got.Effective() != mode {
			t.Fatalf("%q Effective = %q", got, got.Effective())
		}
	}
	if got, err := capturepolicy.ParseCaptureMode(""); err != nil || got != capturepolicy.DefaultCaptureMode {
		t.Fatalf("ParseCaptureMode(empty) = %q/%v, want the default", got, err)
	}
	if got, err := capturepolicy.ParseCaptureMode("  checkpoint  "); err != nil || got != capturepolicy.CaptureModeCheckpoint {
		t.Fatalf("ParseCaptureMode(padded) = %q/%v", got, err)
	}
	if _, err := capturepolicy.ParseCaptureMode("every-now-and-then"); err == nil {
		t.Fatal("ParseCaptureMode accepted an unknown mode")
	} else {
		var typed *capturepolicy.CaptureModeError
		if !errors.As(err, &typed) || typed.Code() != "invalid_capture_mode" {
			t.Fatalf("error = %T %v, want the typed invalid_capture_mode", err, err)
		}
	}
	if capturepolicy.CaptureMode("").Effective() != capturepolicy.DefaultCaptureMode {
		t.Fatal("empty Effective must resolve to the default")
	}
}

func TestTriggerErrorIsTyped(t *testing.T) {
	err := error(&capturepolicy.CaptureTriggerError{Mode: capturepolicy.CaptureModeCheckpoint})
	var typed *capturepolicy.CaptureTriggerError
	if !errors.As(err, &typed) || typed.Code() != "capture_trigger_missing" {
		t.Fatalf("error = %T %v", err, err)
	}
}

func TestContextPinRoundTrip(t *testing.T) {
	if _, ok := capturepolicy.CaptureModeFromContext(context.Background()); ok {
		t.Fatal("an empty context carries a policy")
	}
	ctx := capturepolicy.WithCaptureMode(context.Background(), capturepolicy.CaptureModeManual)
	mode, ok := capturepolicy.CaptureModeFromContext(ctx)
	if !ok || mode != capturepolicy.CaptureModeManual {
		t.Fatalf("pinned mode = %q/%v", mode, ok)
	}
	// Empty pins the default instead of storing an invalid value.
	mode, ok = capturepolicy.CaptureModeFromContext(capturepolicy.WithCaptureMode(context.Background(), ""))
	if !ok || mode != capturepolicy.DefaultCaptureMode {
		t.Fatalf("empty pin = %q/%v, want the default", mode, ok)
	}
}
