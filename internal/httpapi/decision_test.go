package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// decisionTestKey is the credential the fake provider expects. The tests also
// assert it never appears in a response body.
const decisionTestKey = "decision-test-secret-key"

// decisionTestTenantID is the tenant the local passphrase/header contract
// requires on every /v1 call.
const decisionTestTenantID = "22222222-2222-2222-2222-222222222222"

// decisionCall is one request the fake provider observed.
type decisionCall struct {
	Path string
	Auth string
	Body map[string]any
}

// decisionUpstream is a fake decision provider. It records every call so a test
// can assert the forwarded path, credential and body, while respond decides the
// answer.
type decisionUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []decisionCall
}

func newDecisionUpstream(t *testing.T, respond func(w http.ResponseWriter, body map[string]any)) *decisionUpstream {
	t.Helper()
	upstream := &decisionUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		upstream.mu.Lock()
		upstream.calls = append(upstream.calls, decisionCall{
			Path: r.URL.Path,
			Auth: r.Header.Get("Authorization"),
			Body: body,
		})
		upstream.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		respond(w, body)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *decisionUpstream) snapshot() []decisionCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]decisionCall(nil), u.calls...)
}

// setDecisionEnv isolates the decision surface from the developer's shell: the
// media pair wins, the flat pair is cleared so a stray KEEL_BASE_URL cannot make
// an "unconfigured" test reach a real provider. model "" means "use the default".
func setDecisionEnv(t *testing.T, baseURL, key, model string) {
	t.Helper()
	t.Setenv("KEEL_MEDIA_BASE_URL", baseURL)
	t.Setenv("KEEL_MEDIA_API_KEY", key)
	t.Setenv("KEEL_MEDIA_MODEL_DECISION", "")
	t.Setenv("KEEL_DECISION_MODEL", model)
	t.Setenv("KEEL_BASE_URL", "")
	t.Setenv("KEEL_API_KEY", "")
}

func newDecisionServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(New(Config{}))
	t.Cleanup(server.Close)
	return server
}

// TestDecisionAskNormalizesChoiceAndPredicate pins the public ask shape: the
// choice question reaches the provider as
// {type, predicate, choices:[{value,description}]} and the answer comes back in
// the normalized {answers, model, usage} envelope without the credential.
func TestDecisionAskNormalizesChoiceAndPredicate(t *testing.T) {
	upstream := newDecisionUpstream(t, func(w http.ResponseWriter, body map[string]any) {
		questions, _ := body["questions"].([]any)
		first, _ := questions[0].(map[string]any)
		switch first["type"] {
		case "choice":
			_, _ = io.WriteString(w, `{"answers":[{"type":"choice","choice":"B","confidence":0.21,"probabilities":[0.14,0.21,0.65]}],"model":"bailian-token-plan/decision-model-preview","usage":{"input_tokens":42}}`)
		case "predicate":
			_, _ = io.WriteString(w, `{"answers":[{"type":"predicate","probability":0.45}],"model":"bailian-token-plan/decision-model-preview","usage":{"input_tokens":11}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"unexpected question"}}`)
		}
	})
	setDecisionEnv(t, upstream.server.URL, decisionTestKey, "")
	server := newDecisionServer(t)

	var choice struct {
		Answers []struct {
			Type          string    `json:"type"`
			Choice        string    `json:"choice"`
			Confidence    float64   `json:"confidence"`
			Probabilities []float64 `json:"probabilities"`
		} `json:"answers"`
		Model string         `json:"model"`
		Usage map[string]any `json:"usage"`
	}
	choiceBody := map[string]any{
		"input": "A customer says the payment did not arrive after 24 hours.",
		"questions": []any{
			map[string]any{
				"type":      "choice",
				"predicate": "Which team should handle this?",
				"choices": []any{
					map[string]any{"value": "billing", "description": "Payment, refund and billing issues"},
					map[string]any{"value": "technical", "description": "Product faults and integration issues"},
				},
			},
		},
	}
	if code := doJSON(t, server.Client(), "POST", server.URL+"/v1/decisions/ask", decisionTestTenantID, choiceBody, &choice); code != http.StatusOK {
		t.Fatalf("choice ask = %d, want 200", code)
	}
	if len(choice.Answers) != 1 || choice.Answers[0].Type != "choice" ||
		choice.Answers[0].Choice != "B" || choice.Answers[0].Confidence != 0.21 {
		t.Fatalf("normalized choice answer = %+v", choice.Answers)
	}
	if choice.Model != "bailian-token-plan/decision-model-preview" {
		t.Fatalf("model = %q, want the default decision model", choice.Model)
	}
	if choice.Usage["input_tokens"] != float64(42) {
		t.Fatalf("usage = %+v, want input_tokens 42", choice.Usage)
	}

	var predicate struct {
		Answers []struct {
			Type        string  `json:"type"`
			Probability float64 `json:"probability"`
		} `json:"answers"`
	}
	if code := doJSON(t, server.Client(), "POST", server.URL+"/v1/decisions/ask", decisionTestTenantID, map[string]any{
		"input": "The customer is on the enterprise plan.",
		"questions": []any{
			map[string]any{"type": "predicate", "predicate": "Should on-call be notified immediately?"},
		},
	}, &predicate); code != http.StatusOK {
		t.Fatalf("predicate ask = %d, want 200", code)
	}
	if len(predicate.Answers) != 1 || predicate.Answers[0].Type != "predicate" || predicate.Answers[0].Probability != 0.45 {
		t.Fatalf("normalized predicate answer = %+v", predicate.Answers)
	}

	calls := upstream.snapshot()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(calls))
	}
	for _, call := range calls {
		if call.Path != "/decisions" {
			t.Fatalf("provider path = %q, want /decisions", call.Path)
		}
		if call.Auth != "Bearer "+decisionTestKey {
			t.Fatalf("provider Authorization = %q, want the server-side credential", call.Auth)
		}
		if call.Body["model"] != "bailian-token-plan/decision-model-preview" {
			t.Fatalf("provider model = %#v, want the default decision model", call.Body["model"])
		}
	}
	firstQuestions, _ := calls[0].Body["questions"].([]any)
	firstQuestion, _ := firstQuestions[0].(map[string]any)
	choices, _ := firstQuestion["choices"].([]any)
	if firstQuestion["predicate"] != "Which team should handle this?" || len(choices) != 2 {
		t.Fatalf("forwarded choice question = %#v", firstQuestion)
	}
}

// TestDecisionAskUnconfigured pins the 503 state: without a base URL or key the
// endpoint must say so instead of calling a provider anonymously.
func TestDecisionAskUnconfigured(t *testing.T) {
	setDecisionEnv(t, "", "", "")
	server := newDecisionServer(t)

	var out map[string]any
	code := doJSON(t, server.Client(), "POST", server.URL+"/v1/decisions/ask", decisionTestTenantID, map[string]any{
		"input":     "anything",
		"questions": []any{map[string]any{"type": "predicate", "predicate": "Is this configured?"}},
	}, &out)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured ask = %d, want 503", code)
	}
	if out["error"] == nil {
		t.Fatalf("unconfigured body = %#v, want an error", out)
	}
}

// TestDecisionAskRejectsInvalidQuestion pins local validation: an unsupported
// question type is a 400 before any provider call.
func TestDecisionAskRejectsInvalidQuestion(t *testing.T) {
	upstream := newDecisionUpstream(t, func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"answers":[]}`)
	})
	setDecisionEnv(t, upstream.server.URL, decisionTestKey, "")
	server := newDecisionServer(t)

	var out map[string]any
	code := doJSON(t, server.Client(), "POST", server.URL+"/v1/decisions/ask", decisionTestTenantID, map[string]any{
		"input":     "anything",
		"questions": []any{map[string]any{"type": "ranking", "predicate": "Rank these"}},
	}, &out)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid question type = %d, want 400", code)
	}
	if calls := upstream.snapshot(); len(calls) != 0 {
		t.Fatalf("provider calls = %d, want 0 for a locally invalid question", len(calls))
	}
}

// TestDecisionAskModelAllowlist pins KEEL_DECISION_MODEL: only the configured
// decision model is accepted, the omitted model defaults to it, and an
// unconfigured extra model is refused before the provider is called.
func TestDecisionAskModelAllowlist(t *testing.T) {
	upstream := newDecisionUpstream(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = io.WriteString(w, `{"answers":[{"type":"predicate","probability":0.9}]}`)
	})
	const allowed = "bailian-token-plan/decision-model-preview"
	setDecisionEnv(t, upstream.server.URL, decisionTestKey, allowed)
	server := newDecisionServer(t)

	question := []any{map[string]any{"type": "predicate", "predicate": "Is this allowed?"}}
	var out map[string]any
	if code := doJSON(t, server.Client(), "POST", server.URL+"/v1/decisions/ask", decisionTestTenantID, map[string]any{
		"input":     "anything",
		"questions": question,
		"model":     "other-vendor/other-decision-model",
	}, &out); code != http.StatusBadRequest {
		t.Fatalf("disallowed model = %d, want 400", code)
	}
	if calls := upstream.snapshot(); len(calls) != 0 {
		t.Fatalf("provider calls = %d, want 0 for a disallowed model", len(calls))
	}

	if code := doJSON(t, server.Client(), "POST", server.URL+"/v1/decisions/ask", decisionTestTenantID, map[string]any{
		"input":     "anything",
		"questions": question,
	}, &out); code != http.StatusOK {
		t.Fatalf("default model ask = %d, want 200", code)
	}
	calls := upstream.snapshot()
	if len(calls) != 1 || calls[0].Body["model"] != allowed {
		t.Fatalf("forwarded model = %#v, want %q", calls[0].Body["model"], allowed)
	}
}

// TestDecisionAskScoreForwardsLevels pins the score question: its ordered
// criteria levels are forwarded untouched, and the provider's score answer is
// normalized. The provider owns the level-count rule (the observed gateway
// bound is 1..255).
func TestDecisionAskScoreForwardsLevels(t *testing.T) {
	upstream := newDecisionUpstream(t, func(w http.ResponseWriter, body map[string]any) {
		questions, _ := body["questions"].([]any)
		first, _ := questions[0].(map[string]any)
		levels, _ := first["criteria"].([]any)
		if len(levels) != 3 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"score levels missing"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"answers":[{"type":"score","score":1.43,"confidence":0.8,"legend":{"0":"low","1":"medium","2":"high"},"probabilities":[0.1,0.2,0.7]}],"model":"bailian-token-plan/decision-model-preview"}`)
	})
	setDecisionEnv(t, upstream.server.URL, decisionTestKey, "")
	server := newDecisionServer(t)

	var out struct {
		Answers []struct {
			Type   string            `json:"type"`
			Score  float64           `json:"score"`
			Legend map[string]string `json:"legend"`
		} `json:"answers"`
	}
	if code := doJSON(t, server.Client(), "POST", server.URL+"/v1/decisions/ask", decisionTestTenantID, map[string]any{
		"input": "Rate the severity of this outage.",
		"questions": []any{
			map[string]any{
				"type":      "score",
				"predicate": "How severe is this issue?",
				"criteria":  []any{"Minor issue", "Some features affected", "Core feature unavailable"},
			},
		},
	}, &out); code != http.StatusOK {
		t.Fatalf("score ask = %d, want 200", code)
	}
	if len(out.Answers) != 1 || out.Answers[0].Type != "score" || out.Answers[0].Score != 1.43 || out.Answers[0].Legend["2"] != "high" {
		t.Fatalf("normalized score answer = %+v", out.Answers)
	}
}

// TestDecisionAskDoesNotLeakKey pins the credential boundary: a provider error
// that echoes the key is sanitized by the gateway, and the key never appears in
// the client response.
func TestDecisionAskDoesNotLeakKey(t *testing.T) {
	upstream := newDecisionUpstream(t, func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key `+decisionTestKey+`"}}`)
	})
	setDecisionEnv(t, upstream.server.URL, decisionTestKey, "")
	server := newDecisionServer(t)

	var out map[string]any
	code := doJSON(t, server.Client(), "POST", server.URL+"/v1/decisions/ask", decisionTestTenantID, map[string]any{
		"input":     "anything",
		"questions": []any{map[string]any{"type": "predicate", "predicate": "Is the key safe?"}},
	}, &out)
	if code != http.StatusUnauthorized {
		t.Fatalf("provider error = %d, want 401", code)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal error body: %v", err)
	}
	if strings.Contains(string(raw), decisionTestKey) {
		t.Fatalf("response leaked the credential: %s", raw)
	}
	if message, _ := out["error"].(string); !strings.Contains(message, "[redacted]") {
		t.Fatalf("error = %q, want the credential redacted", message)
	}
}
