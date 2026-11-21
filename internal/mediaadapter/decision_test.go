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
)

func decisionQuestions() map[string]DecisionQuestion {
	return map[string]DecisionQuestion{
		"bug": {
			Type:         QuestionNoul,
			Instructions: json.RawMessage(`"Is the customer reporting a software defect?"`),
			Criteria:     json.RawMessage(`{"true":"a defect","false":"not a defect"}`),
		},
		"owner": {
			Type:         QuestionChoice,
			Instructions: json.RawMessage(`"Which team should own this?"`),
			Criteria:     json.RawMessage(`{"payments":"checkout and billing","frontend":"web UI"}`),
		},
		"urgency": {
			Type:         QuestionScore,
			Instructions: json.RawMessage(`"How urgent is this?"`),
			Criteria:     json.RawMessage(`["low","medium","high"]`),
		},
	}
}

const decisionAnswersFixture = `{
  "bug": {"type":"noul","noul":0.91},
  "owner": {"type":"choice","choice":"payments","probabilities":{"payments":0.7,"frontend":0.3},"confidence":0.8},
  "urgency": {"type":"score","score":1.4,"legend":{"0":"low","1":"medium","2":"high"},"confidence":0.6}
}`

func TestDecisionAdaptersShareSystemOneWire(t *testing.T) {
	cases := []struct {
		adapter  string
		model    string
		wantPath string
		envelope bool
	}{
		{adapter: DecisionAdapterTypeSafe, model: "jev-1.14", wantPath: "/v1/systemone"},
		{adapter: DecisionAdapterOpenRouter, model: "typesafe/jev-1.14", wantPath: "/api/alpha/decisions"},
		{adapter: DecisionAdapterOpenJev, model: "openjev", wantPath: "/v1/systemone"},
		{adapter: DecisionAdapterCloudflare, model: "@cf/cloudflare/clef", wantPath: "/ai/run/@cf/cloudflare/clef", envelope: true},
	}
	for _, tc := range cases {
		t.Run(tc.adapter, func(t *testing.T) {
			var posted map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.wantPath {
					t.Errorf("path = %q, want %q", r.URL.Path, tc.wantPath)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer decision-key" {
					t.Errorf("authorization = %q", got)
				}
				if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
					t.Errorf("decode: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.envelope {
					fmt.Fprintf(w, `{"result":{"id":"req-1","model":"clef-flash","answers":%s,"usage":{"input_tokens":11,"output_tokens":2,"cost":0.5}},"success":true,"errors":[],"messages":[]}`, decisionAnswersFixture)
					return
				}
				fmt.Fprintf(w, `{"id":"req-1","model":"jev-1.14","answers":%s,"usage":{"input_tokens":11,"output_tokens":2,"cost":0.5}}`, decisionAnswersFixture)
			}))
			defer server.Close()

			client, err := NewDecisionClient(DecisionConfig{
				Adapter: tc.adapter,
				BaseURL: server.URL,
				APIKey:  "decision-key",
				Model:   tc.model,
			})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := client.Decide(context.Background(), DecisionRequest{
				State:     json.RawMessage(`{"ticket":"checkout page is blank"}`),
				Questions: decisionQuestions(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Mode != DecisionModeEnforce || outcome.Shadow {
				t.Fatalf("mode = %q shadow=%v", outcome.Mode, outcome.Shadow)
			}
			if len(outcome.Answers) != 3 {
				t.Fatalf("answers = %+v", outcome.Answers)
			}
			if strings.Contains(string(outcome.Answers["bug"]), "confidence") {
				t.Fatalf("adapter added confidence to a noul answer: %s", outcome.Answers["bug"])
			}
			if outcome.Usage.InputTokens != 11 || outcome.Usage.OutputTokens != 2 || outcome.Usage.Questions != 3 {
				t.Fatalf("usage = %+v", outcome.Usage)
			}
			if outcome.RequestID != "req-1" {
				t.Fatalf("request id = %q", outcome.RequestID)
			}

			// Request shape: state + questions; the model is a body field only for
			// the adapters that route by body (Cloudflare routes by path).
			body, _ := json.Marshal(posted)
			t.Logf("wire POST %s body=%s", tc.wantPath, body)
			t.Logf("answers=%s usage=%+v mode=%s", mustJSON(t, outcome.Answers), outcome.Usage, outcome.Mode)
			for _, want := range []string{`"state":{"ticket":"checkout page is blank"}`, `"questions"`, `"bug"`, `"owner"`, `"urgency"`, `"criteria"`, `"instructions"`} {
				if !strings.Contains(string(body), want) {
					t.Fatalf("wire body missing %s: %s", want, body)
				}
			}
			if tc.envelope {
				if strings.Contains(string(body), `"model"`) {
					t.Fatalf("cloudflare body must not carry a model field: %s", body)
				}
			} else if !strings.Contains(string(body), `"model":"`+tc.model+`"`) {
				t.Fatalf("wire body missing model: %s", body)
			}

			raw, _ := json.Marshal(outcome)
			for _, forbidden := range []string{"cost", "0.5", "decision-key", server.URL} {
				if strings.Contains(string(raw), forbidden) {
					t.Fatalf("outcome leaked %q: %s", forbidden, raw)
				}
			}
		})
	}
}

func TestDecisionPreflightCapabilities(t *testing.T) {
	newClient := func(adapter, model string) *DecisionClient {
		t.Helper()
		client, err := NewDecisionClient(DecisionConfig{
			Adapter: adapter, BaseURL: "https://decision.example", Model: model,
			AllowAnonymous: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}

	t.Run("box rejected by clef", func(t *testing.T) {
		client := newClient(DecisionAdapterCloudflare, "@cf/cloudflare/clef")
		fields := client.Preflight(json.RawMessage(`{"image_url":"data:image/png;base64,AAAA"}`), map[string]DecisionQuestion{
			"target": {Type: QuestionBox, Instructions: json.RawMessage(`"find the button"`)},
		})
		if !hasField(fields, "questions.target.type", "box") {
			t.Fatalf("fields = %+v", fields)
		}
	})

	t.Run("remote image rejected", func(t *testing.T) {
		client := newClient(DecisionAdapterOpenRouter, "typesafe/jev-1.14")
		fields := client.Preflight(json.RawMessage(`{"image_url":"https://example.com/a.png"}`), decisionQuestions())
		if !hasField(fields, "state", "remote URLs") {
			t.Fatalf("fields = %+v", fields)
		}
	})

	t.Run("data image accepted", func(t *testing.T) {
		client := newClient(DecisionAdapterOpenRouter, "typesafe/jev-1.14")
		fields := client.Preflight(json.RawMessage(`{"image_url":"data:image/png;base64,AAAA"}`), decisionQuestions())
		if len(fields) != 0 {
			t.Fatalf("fields = %+v", fields)
		}
	})

	t.Run("empty choice criteria", func(t *testing.T) {
		client := newClient(DecisionAdapterTypeSafe, "jev-1.14")
		fields := client.Preflight(json.RawMessage(`{"text":"x"}`), map[string]DecisionQuestion{
			"owner": {Type: QuestionChoice, Instructions: json.RawMessage(`"who?"`), Criteria: json.RawMessage(`{}`)},
		})
		if !hasField(fields, "questions.owner.criteria", "non-empty") {
			t.Fatalf("fields = %+v", fields)
		}
	})

	t.Run("score needs two levels", func(t *testing.T) {
		client := newClient(DecisionAdapterTypeSafe, "jev-1.14")
		fields := client.Preflight(json.RawMessage(`{"text":"x"}`), map[string]DecisionQuestion{
			"level": {Type: QuestionScore, Instructions: json.RawMessage(`"how?"`), Criteria: json.RawMessage(`["only"]`)},
		})
		if !hasField(fields, "questions.level.criteria", "at least two") {
			t.Fatalf("fields = %+v", fields)
		}
	})

	t.Run("unknown question type", func(t *testing.T) {
		client := newClient(DecisionAdapterTypeSafe, "jev-1.14")
		fields := client.Preflight(json.RawMessage(`{"text":"x"}`), map[string]DecisionQuestion{
			"weird": {Type: "ranking", Instructions: json.RawMessage(`"rank"`)},
		})
		if !hasField(fields, "questions.weird.type", "ranking") {
			t.Fatalf("fields = %+v", fields)
		}
	})

	t.Run("question count limit", func(t *testing.T) {
		client := newClient(DecisionAdapterCloudflare, "@cf/cloudflare/clef")
		questions := map[string]DecisionQuestion{}
		for i := 0; i < 65; i++ {
			questions[fmt.Sprintf("q%d", i)] = DecisionQuestion{
				Type: QuestionNoul, Instructions: json.RawMessage(`"is it?"`),
			}
		}
		fields := client.Preflight(json.RawMessage(`{"text":"x"}`), questions)
		if !hasField(fields, "questions", "at most 64") {
			t.Fatalf("fields = %+v", fields)
		}
	})

	t.Run("box requires an image", func(t *testing.T) {
		client := newClient(DecisionAdapterTypeSafe, "jev-1.14")
		fields := client.Preflight(json.RawMessage(`{"text":"x"}`), map[string]DecisionQuestion{
			"target": {Type: QuestionBox, Instructions: json.RawMessage(`"find the button"`)},
		})
		if !hasField(fields, "questions.target", "require an image") {
			t.Fatalf("fields = %+v", fields)
		}
	})
}

func hasField(fields []FieldError, field, contains string) bool {
	for _, fieldError := range fields {
		if fieldError.Field == field && strings.Contains(fieldError.Message, contains) {
			return true
		}
	}
	return false
}

func TestDecisionFailClosedAndShadow(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":{"message":"upstream maintenance"}}`)
	}))
	defer failing.Close()
	client, err := NewDecisionClient(DecisionConfig{
		Adapter: DecisionAdapterTypeSafe, BaseURL: failing.URL, APIKey: "k", Model: "jev-1.14",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := DecisionRequest{State: json.RawMessage(`{"ticket":"x"}`), Questions: decisionQuestions()}

	// Enforce (the default) must block and must not fabricate an answer.
	outcome, err := client.Decide(context.Background(), request)
	if outcome != nil {
		t.Fatalf("enforce returned an outcome: %+v", outcome)
	}
	if !errors.Is(err, ErrDecisionUnavailable) {
		t.Fatalf("error = %v, want decision unavailable", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "noul:0") || strings.Contains(strings.ToLower(err.Error()), `"choice":"unknown"`) {
		t.Fatalf("error fabricated an answer: %v", err)
	}

	// Shadow records the failure and returns no answer without failing the call.
	request.Policy = DecisionPolicy{Mode: DecisionModeShadow, FailPolicy: DecisionFailOpen}
	outcome, err = client.Decide(context.Background(), request)
	if err != nil {
		t.Fatalf("shadow error = %v", err)
	}
	if !outcome.Shadow || outcome.Failure == "" || len(outcome.Answers) != 0 || outcome.Mode != DecisionModeShadow {
		t.Fatalf("shadow outcome = %+v", outcome)
	}

	// fail_open outside shadow is a binding error, not a provider behavior.
	request.Policy = DecisionPolicy{Mode: DecisionModeEnforce, FailPolicy: DecisionFailOpen}
	_, err = client.Decide(context.Background(), request)
	var typed *Error
	if !asAdapterError(err, &typed) || typed.Kind != KindRequest || !hasField(typed.Fields, "failPolicy", "shadow") {
		t.Fatalf("error = %+v", err)
	}
}

func TestDecisionShadowSuccessCarriesAnswersButStaysShadow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"req-2","model":"jev-1.14","answers":%s,"usage":{"input_tokens":4}}`, decisionAnswersFixture)
	}))
	defer server.Close()
	client, err := NewDecisionClient(DecisionConfig{
		Adapter: DecisionAdapterTypeSafe, BaseURL: server.URL, APIKey: "k", Model: "jev-1.14",
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := client.Decide(context.Background(), DecisionRequest{
		State:     json.RawMessage(`{"ticket":"x"}`),
		Questions: decisionQuestions(),
		Policy:    DecisionPolicy{Mode: DecisionModeShadow},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Shadow || outcome.Mode != DecisionModeShadow || len(outcome.Answers) != 3 {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestDecisionMalformedAnswersNeverBecomeDefaults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Missing owner and urgency; noul has no probability.
		fmt.Fprint(w, `{"model":"jev-1.14","answers":{"bug":{"type":"noul"}}}`)
	}))
	defer server.Close()
	client, err := NewDecisionClient(DecisionConfig{
		Adapter: DecisionAdapterTypeSafe, BaseURL: server.URL, APIKey: "k", Model: "jev-1.14",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := DecisionRequest{State: json.RawMessage(`{"ticket":"x"}`), Questions: decisionQuestions()}
	outcome, err := client.Decide(context.Background(), request)
	if outcome != nil || !errors.Is(err, ErrDecisionUnavailable) {
		t.Fatalf("outcome=%+v error=%v", outcome, err)
	}
	request.Policy = DecisionPolicy{Mode: DecisionModeShadow}
	outcome, err = client.Decide(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Answers) != 0 || !outcome.Shadow || outcome.Failure == "" {
		t.Fatalf("shadow outcome = %+v", outcome)
	}
}

func TestDecisionCloudflareEnvelopeFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"result":null,"success":false,"errors":[{"message":"no image supplied"}],"messages":[]}`)
	}))
	defer server.Close()
	client, err := NewDecisionClient(DecisionConfig{
		Adapter: DecisionAdapterCloudflare, BaseURL: server.URL, APIKey: "k", Model: "@cf/cloudflare/clef",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Decide(context.Background(), DecisionRequest{
		State:     json.RawMessage(`{"text":"x"}`),
		Questions: decisionQuestions(),
	})
	if !errors.Is(err, ErrDecisionUnavailable) {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "no image supplied") {
		t.Fatalf("error lost the clef message: %v", err)
	}
}

func TestDecisionCapabilitiesAreHonest(t *testing.T) {
	clef, err := DecisionCapabilities(DecisionAdapterCloudflare, "@cf/cloudflare/clef")
	if err != nil {
		t.Fatal(err)
	}
	if clef.Protocol != SystemOneProtocol || clef.ProtocolVersion != SystemOneProtocolVersion {
		t.Fatalf("protocol = %s %s", clef.Protocol, clef.ProtocolVersion)
	}
	if containsString(clef.QuestionTypes, QuestionBox) {
		t.Fatal("clef must not advertise box")
	}
	if clef.ContextWindow != 65536 {
		t.Fatalf("context window = %d", clef.ContextWindow)
	}
	if clef.NoulConfidence {
		t.Fatal("noul answers have no confidence and the descriptor must say so")
	}
	if clef.Streaming || clef.Rationale || clef.CrossQuestionVisibility {
		t.Fatalf("clef declared a capability it does not have: %+v", clef)
	}
	if !clef.BatchedQuestions || !clef.AdvisoryOnly || !clef.NativeDecision {
		t.Fatalf("clef missing required declaration: %+v", clef)
	}
	jev, err := DecisionCapabilities(DecisionAdapterTypeSafe, "jev-1.14")
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(jev.QuestionTypes, QuestionBox) {
		t.Fatal("typesafe must advertise box")
	}
	if _, err := DecisionCapabilities("mystery", "m"); err == nil {
		t.Fatal("unknown adapter must be rejected")
	}
}

func TestDecisionAnswerContractRejectsProviderViolations(t *testing.T) {
	base := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(decisionAnswersFixture), &base); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		id     string
		answer string
		field  string
	}{
		{name: "choice not declared", id: "owner", answer: `{"type":"choice","choice":"marketing"}`, field: "answers.owner.choice"},
		{name: "score outside levels", id: "urgency", answer: `{"type":"score","score":9.9}`, field: "answers.urgency.score"},
		{name: "noul confidence", id: "bug", answer: `{"type":"noul","noul":0.9,"confidence":0.8}`, field: "answers.bug.confidence"},
		{name: "noul rationale", id: "bug", answer: `{"type":"noul","noul":0.9,"rationale":"because"}`, field: "answers.bug.rationale"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answers := make(map[string]json.RawMessage, len(base))
			for key, value := range base {
				answers[key] = value
			}
			answers[tc.id] = json.RawMessage(tc.answer)
			payload, _ := json.Marshal(map[string]any{"model": "jev-1.14", "answers": answers})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(payload)
			}))
			defer server.Close()
			client, err := NewDecisionClient(DecisionConfig{
				Adapter: DecisionAdapterTypeSafe, BaseURL: server.URL, APIKey: "k", Model: "jev-1.14",
			})
			if err != nil {
				t.Fatal(err)
			}
			request := DecisionRequest{State: json.RawMessage(`{"ticket":"x"}`), Questions: decisionQuestions()}
			outcome, err := client.Decide(context.Background(), request)
			if outcome != nil || !errors.Is(err, ErrDecisionUnavailable) {
				t.Fatalf("outcome=%+v error=%v", outcome, err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error %q does not name %s", err, tc.field)
			}
			request.Policy = DecisionPolicy{Mode: DecisionModeShadow}
			outcome, err = client.Decide(context.Background(), request)
			if err != nil || outcome == nil || len(outcome.Answers) != 0 || !outcome.Shadow {
				t.Fatalf("shadow outcome=%+v err=%v", outcome, err)
			}
		})
	}
}

func TestDecisionBoxSendsTheTypeSafeDangerZoneHeader(t *testing.T) {
	var header string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("TypeSafe-Danger-Zone-Images")
		fmt.Fprint(w, `{"model":"jev-1.14","answers":{"target":{"type":"box","box":{"top":0.1,"bottom":0.4,"left":0.2,"right":0.6},"confidence":0.9}}}`)
	}))
	defer server.Close()
	client, err := NewDecisionClient(DecisionConfig{
		Adapter: DecisionAdapterTypeSafe, BaseURL: server.URL, APIKey: "k", Model: "jev-1.14",
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := client.Decide(context.Background(), DecisionRequest{
		State: json.RawMessage(`{"image_url":"data:image/png;base64,AAAA"}`),
		Questions: map[string]DecisionQuestion{
			"target": {Type: QuestionBox, Instructions: json.RawMessage(`"find the button in the image"`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if header != "true" {
		t.Fatalf("danger-zone header = %q", header)
	}
	if len(outcome.Answers["target"]) == 0 {
		t.Fatalf("answers = %+v", outcome.Answers)
	}
}
