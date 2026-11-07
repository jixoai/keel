package logsafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestHandlerRedactsSensitiveAttributes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(New(slog.NewJSONHandler(&buf, nil)))
	logger.Info("provider call failed",
		"request_id", "req-1",
		"tenant_id", "tenant-1",
		"authorization", "Bearer credential-three",
		"X-API-Key", "credential-one",
		"token", "credential-two",
		"text", "message body with secrets",
		slog.Group("request",
			slog.String("body", `{"text":"hello"}`),
			slog.String("path", "/v1/sessions"),
		),
		"err", "upstream rejected Bearer credential-four",
	)
	out := buf.String()
	for _, secret := range []string{
		"credential-three", "credential-one", "credential-two",
		"message body with secrets", `{"text":"hello"}`, "credential-four",
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{"req-1", "tenant-1", "/v1/sessions", Redacted} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q:\n%s", want, out)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &decoded); err != nil {
		t.Fatalf("log is not JSON: %v\n%s", err, out)
	}
	if decoded["authorization"] != Redacted {
		t.Fatalf("authorization = %v", decoded["authorization"])
	}
}

func TestWithAttrsIsFiltered(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(New(slog.NewJSONHandler(&buf, nil))).With("token", "must-not-appear")
	logger.Info("hello", "request_id", "r1")
	if strings.Contains(buf.String(), "must-not-appear") {
		t.Fatalf("With attrs leaked:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "r1") {
		t.Fatalf("safe attr missing:\n%s", buf.String())
	}
}

func TestBenignTextSurvives(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(New(slog.NewJSONHandler(&buf, nil)))
	logger.Info("run finished", "status", "done", "duration_ms", 12)
	out := buf.String()
	if strings.Contains(out, Redacted) {
		t.Fatalf("benign log redacted: %s", out)
	}
}

type secretStringer struct{}

func (secretStringer) String() string { return "token=credential-stringer" }

func TestHandlerRedactsAnyValues(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(New(slog.NewJSONHandler(&buf, nil)))
	logger.Info("upstream failure",
		"request_id", "req-2",
		slog.Any("err", errors.New("upstream said Bearer credential-one")),
	)
	logger.Info("wrapped failure",
		slog.Any("err", fmt.Errorf("call failed: %w", errors.New("Authorization: Bearer credential-two"))),
	)
	logger.Info("request map",
		slog.Any("request", map[string]any{
			"authorization": "Bearer credential-three",
			"model":         "gpt",
			"nested":        map[string]any{"api_key": "credential-four"},
			"headers":       map[string]string{"X-Api-Key": "credential-five"},
		}),
	)
	logger.Info("payload struct",
		slog.Any("metadata", struct {
			Authorization string
			Public        string
		}{Authorization: "Bearer credential-six", Public: "visible"}),
	)
	logger.Info("stringer", slog.Any("transport", secretStringer{}))
	probe := "probe" + "-" + "value" + "-" + "9"
	jsonBody := `provider body {` + `"api_key"` + `:` + `"` + probe + `"` + `","message":"bad request"}`
	logger.Info("json body", slog.Any("err", errors.New(jsonBody)))
	logger.Info("error slice", slog.Any("errors", []error{
		errors.New("Bearer credential-seven"),
	}))

	out := buf.String()
	for _, secret := range []string{
		"credential-one", "credential-two", "credential-three", "credential-four",
		"credential-five", "credential-six", "credential-seven", "credential-stringer", probe,
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{"req-2", "visible", Redacted} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q:\n%s", want, out)
		}
	}
}

func TestHandlerRedactsNestedGroupValues(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(New(slog.NewJSONHandler(&buf, nil)))
	logger.Info("group",
		slog.Group("request",
			slog.Any("err", errors.New("Bearer credential-eight")),
			slog.String("api_key", "credential-nine"),
			slog.Any("meta", map[string]any{"token": "credential-ten"}),
		),
	)
	out := buf.String()
	for _, secret := range []string{"credential-eight", "credential-nine", "credential-ten"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains %q:\n%s", secret, out)
		}
	}
}

func TestHandlerMarksUninspectableValues(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(New(slog.NewJSONHandler(&buf, nil)))
	logger.Info("uninspectable", slog.Any("bad_map", map[int]string{1: "value"}))
	if !strings.Contains(buf.String(), Unredactable) {
		t.Fatalf("uninspectable value not marked: %s", buf.String())
	}
}

func TestHandlerRedactsProviderTokenShapes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(New(slog.NewJSONHandler(&buf, nil)))
	sk := "s" + "k-" + "proj-" + strings.Repeat("A", 24)
	aiza := "A" + "Iza" + strings.Repeat("B", 28)
	ghp := "g" + "hp_" + strings.Repeat("C", 24)
	xoxb := "x" + "oxb-" + strings.Repeat("1", 16)
	akia := "A" + "KIA" + strings.Repeat("D", 16)
	logger.Info("openai 401", slog.Any("err", errors.New(
		`{"error":{"message":"Incorrect API key provided: `+sk+`","type":"invalid_request_error","code":"invalid_api_key"}}`)))
	logger.Info("gemini", slog.Any("err",
		errors.New("GET https://generativelanguage.googleapis.com/v1beta/models?key="+aiza)))
	logger.Info("provider", slog.Any("err",
		errors.New("provider error: key "+sk+" is invalid")))
	logger.Info("other tokens", slog.Any("err",
		errors.New(ghp+" "+xoxb+" "+akia)))
	logger.Info("benign", "detail", "max_tokens=4096; token budget is fine")

	out := buf.String()
	for _, secret := range []string{sk, aiza, ghp, xoxb, akia} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains provider token %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{"max_tokens=4096", "token budget is fine", "?key=" + Redacted, Redacted} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q:\n%s", want, out)
		}
	}
}

func TestHandlerUsesJSONFieldNamesForStructs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(New(slog.NewJSONHandler(&buf, nil)))
	type payload struct {
		Foo  string `json:"api_key"`
		Bar  string `json:"model"`
		Skip string `json:"-"`
	}
	logger.Info("struct", slog.Any("metadata", payload{
		Foo:  "struct-credential",
		Bar:  "gpt",
		Skip: "must-not-appear",
	}))
	out := buf.String()
	if strings.Contains(out, "struct-credential") || strings.Contains(out, "must-not-appear") {
		t.Fatalf("struct field leaked:\n%s", out)
	}
	if !strings.Contains(out, `"model":"gpt"`) {
		t.Fatalf("non-sensitive field missing:\n%s", out)
	}
}
