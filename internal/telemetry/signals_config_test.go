package telemetry

import (
	"strings"
	"testing"
)

func TestForbiddenAttributeKeys(t *testing.T) {
	forbidden := []string{
		"prompt", "message", "tool.args", "tool_result", "file.name", "path",
		"url.query", "secret", "Authorization", "cookie", "attachment.name",
		"api_key", "bearer",
	}
	for _, key := range forbidden {
		if !ForbiddenAttributeKey(key) {
			t.Fatalf("%q must be forbidden", key)
		}
		if AllowedSpanAttribute(key) {
			t.Fatalf("%q must not be allowed", key)
		}
	}
	for _, key := range []string{"tenant", "session", "run", "turn", "tool", "model", "provider",
		"outcome", "status", "kind", "error.kind", "http.status_code", "mcp.server", "plugin", "command"} {
		if ForbiddenAttributeKey(key) {
			t.Fatalf("%q must not be forbidden", key)
		}
		if !AllowedSpanAttribute(key) {
			t.Fatalf("%q must be allowed", key)
		}
	}
	// A prefix match must not swallow a longer, unrelated key at a different
	// segment boundary.
	if ForbiddenAttributeKey("profile") {
		t.Fatal("profile must not match the file/path prefix family")
	}
}

func TestSanitizeSpanAttributes(t *testing.T) {
	long := strings.Repeat("x", MaxAttributeStringRunes+32)
	attributes := SpanAttributes{
		"tenant":        "tenant-a",
		"session":       "session-a",
		"prompt":        "write me a haiku",
		"message":       "hello",
		"tool.args":     "secret",
		"file.name":     "report.pdf",
		"path":          "/tmp/report.pdf",
		"url":           "https://example.test/?token=abc",
		"Authorization": "Bearer abc",
		"command":       "sk-abcdefghijklmnop",
		"tool":          long,
		"provider":      "openai",
		"outcome":       "ok",
		"unknown":       "x",
	}
	clean, dropped := SanitizeSpanAttributes(attributes)
	for _, key := range []string{"prompt", "message", "tool.args", "file.name", "path", "url", "Authorization", "command", "unknown"} {
		if _, exists := clean[key]; exists {
			t.Fatalf("forbidden/unknown key %q survived: %#v", key, clean)
		}
	}
	if clean["tenant"] != "tenant-a" || clean["provider"] != "openai" || clean["outcome"] != "ok" {
		t.Fatalf("allowed attributes = %#v", clean)
	}
	if got := []rune(clean["tool"].(string)); len(got) != MaxAttributeStringRunes {
		t.Fatalf("tool length = %d, want %d", len(got), MaxAttributeStringRunes)
	}
	if dropped == 0 {
		t.Fatal("dropped count is zero")
	}
}

func TestSanitizeMetricLabelsOnlyBoundedEnums(t *testing.T) {
	clean, _ := SanitizeMetricLabels(SpanAttributes{
		"model": "m", "provider": "p", "outcome": "ok", "status": "done",
		"tool": "t", "kind": "k", "result": "r",
		"tenant": "tenant-a", "session": "session-a", "run": "run-1", "trace": "trace-1",
	})
	for _, forbidden := range []string{"tenant", "session", "run", "trace"} {
		if _, exists := clean[forbidden]; exists {
			t.Fatalf("metric label %q must be dropped", forbidden)
		}
	}
	if len(clean) != 7 {
		t.Fatalf("metric labels = %#v", clean)
	}
}

func TestSanitizeAttributesBoundsCountAndArrays(t *testing.T) {
	attributes := SpanAttributes{}
	for index := 0; index < MaxAttributesPerRecord+10; index++ {
		// All keys are allowed and unique: tenant is the only repeated key, so use
		// model as a bounded enum and a numeric suffix; unknown suffixes would be
		// dropped, which would defeat the count test. Use repeated allowed keys
		// with distinct case to stay within the allowlist shape.
		switch index % 7 {
		case 0:
			attributes["tenant"] = index
		case 1:
			attributes["session"] = index
		case 2:
			attributes["run"] = index
		case 3:
			attributes["turn"] = index
		case 4:
			attributes["tool"] = index
		case 5:
			attributes["model"] = index
		case 6:
			attributes["provider"] = index
		}
	}
	clean, _ := SanitizeSpanAttributes(attributes)
	if len(clean) > MaxAttributesPerRecord {
		t.Fatalf("attributes = %d, want <= %d", len(clean), MaxAttributesPerRecord)
	}
	array := make([]string, MaxAttributeArrayItems+5)
	for index := range array {
		array[index] = "v"
	}
	clean, _ = SanitizeSpanAttributes(SpanAttributes{"tool": array})
	items, ok := clean["tool"].([]string)
	if !ok || len(items) != MaxAttributeArrayItems {
		t.Fatalf("array = %#v", clean["tool"])
	}
}

func TestParseConfigDefaultsAndInactive(t *testing.T) {
	effective, err := ParseConfig(Config{Adapter: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if !effective.Inactive() || len(effective.Signals) != 0 || effective.Adapter != "none" {
		t.Fatalf("inactive config = %#v", effective)
	}
	if effective.SampleRatio != 1 || effective.LogRatio != 1 || effective.LogMinLevel != LogLevelInfo {
		t.Fatalf("defaults = %#v", effective)
	}
	memory, err := ParseConfig(Config{Adapter: "memory"})
	if err != nil || !memory.Inactive() || memory.Adapter != "memory" {
		t.Fatalf("memory = %#v, %v", memory, err)
	}
}

func TestParseConfigOTLP(t *testing.T) {
	ratio := 0.25
	effective, err := ParseConfig(Config{
		Adapter:     "otlp",
		Endpoint:    "https://collector.example:4318",
		Headers:     map[string]string{"Authorization": "secret:OTLP_TOKEN"},
		ServiceName: "keel-test",
		SampleRatio: &ratio,
	})
	if err != nil {
		t.Fatal(err)
	}
	if effective.EndpointHost != "collector.example:4318" {
		t.Fatalf("endpoint host = %q", effective.EndpointHost)
	}
	if len(effective.Signals) != 2 || effective.Signals[0] != SignalTraces || effective.Signals[1] != SignalMetrics {
		t.Fatalf("default signals = %v", effective.Signals)
	}
	if effective.SampleRatio != 0.25 || len(effective.HeaderNames) != 1 {
		t.Fatalf("effective = %#v", effective)
	}
	if effective.Headers["Authorization"] != "secret:OTLP_TOKEN" {
		t.Fatal("header reference was not preserved")
	}
	if _, err := effective.ResolveHeaders(func(name string) (string, bool) { return "token-value", name == "OTLP_TOKEN" }); err != nil {
		t.Fatal(err)
	}
	// An explicit empty signal list means "none"; only an omitted (nil) list
	// defaults to traces+metrics.
	empty, err := ParseConfig(Config{Adapter: "otlp", Endpoint: "https://collector.example", Signals: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Signals) != 0 {
		t.Fatalf("explicit empty signals = %v", empty.Signals)
	}
}

func TestParseConfigRefusesInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		config Config
		field  string
	}{
		{"missing endpoint", Config{Adapter: "otlp"}, "endpoint"},
		{"relative endpoint", Config{Adapter: "otlp", Endpoint: "/v1/traces"}, "endpoint"},
		{"plaintext header", Config{Adapter: "otlp", Endpoint: "https://c.example", Headers: map[string]string{"Authorization": "Bearer abc"}}, "headers"},
		{"bad secret ref", Config{Adapter: "otlp", Endpoint: "https://c.example", Headers: map[string]string{"Authorization": "secret:"}}, "headers"},
		{"bad signal", Config{Adapter: "otlp", Endpoint: "https://c.example", Signals: []string{"profiles"}}, "signals[0]"},
		{"ratio high", Config{Adapter: "otlp", Endpoint: "https://c.example", SampleRatio: floatPointer(1.5)}, "sampleRatio"},
		{"ratio negative", Config{Adapter: "otlp", Endpoint: "https://c.example", LogRatio: floatPointer(-0.1)}, "logRatio"},
		{"bad level", Config{Adapter: "otlp", Endpoint: "https://c.example", LogMinLevel: "trace"}, "logMinLevel"},
		{"unknown adapter", Config{Adapter: "prometheus"}, "adapter"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseConfig(test.config)
			if err == nil {
				t.Fatal("config was accepted")
			}
			fields := ConfigErrorFields(err)
			found := false
			for _, field := range fields {
				if field.Field == test.field {
					found = true
				}
			}
			if !found {
				t.Fatalf("fields = %#v, want %q", fields, test.field)
			}
		})
	}
}

func TestSummarizeConfigHasNoHeaderValue(t *testing.T) {
	summary := SummarizeConfig(Config{
		Adapter:  "otlp",
		Endpoint: "https://user:pass@collector.example/v1?token=abc",
		Headers:  map[string]string{"Authorization": "secret:OTLP_TOKEN"},
	})
	if strings.Contains(summary.EndpointHost, "pass") || strings.Contains(summary.EndpointHost, "token") {
		t.Fatalf("endpoint host leaks userinfo/query: %q", summary.EndpointHost)
	}
	if len(summary.HeaderNames) != 1 || summary.HeaderNames[0] != "Authorization" {
		t.Fatalf("header names = %v", summary.HeaderNames)
	}
	if summary.Valid {
		t.Fatal("an endpoint with userinfo/query must be invalid")
	}
	if strings.Contains(summary.Reason, "OTLP_TOKEN") {
		t.Fatal("summary reason mentions a secret name")
	}
}

func floatPointer(value float64) *float64 { return &value }
