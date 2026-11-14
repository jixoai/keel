package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/demo"
)

// TestDemoAssetsServedWhenEnabled pins the image turn's hosted asset: every tile
// the demo can return is fetchable, carries the PNG magic, and is cacheable.
func TestDemoAssetsServedWhenEnabled(t *testing.T) {
	clearMediaEnv(t)
	t.Setenv(demo.Env, "1")
	hs := newMediaServer(t, nil)

	for _, name := range []string{demo.AssetSquare, demo.AssetPortrait, demo.AssetLandscape} {
		req, err := http.NewRequest(http.MethodGet, hs.URL+demo.AssetURL(name), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Tenant-ID", uuid.NewString())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d, want 200", name, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != "image/png" {
			t.Fatalf("%s content-type = %q, want image/png", name, got)
		}
		if len(raw) < 8 || string(raw[1:4]) != "PNG" {
			head := raw
			if len(head) > 8 {
				head = head[:8]
			}
			t.Fatalf("%s is not a PNG: % x", name, head)
		}
		if !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
			t.Fatalf("%s cache-control = %q, want immutable", name, resp.Header.Get("Cache-Control"))
		}
	}

	// A name outside the closed set is a 404, never a path escape attempt.
	for _, path := range []string{"/v1/demo/assets/../etc/passwd", "/v1/demo/assets/nope.png", "/v1/demo/assets/"} {
		req, err := http.NewRequest(http.MethodGet, hs.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Tenant-ID", uuid.NewString())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", path, resp.StatusCode)
		}
	}
}

// TestDemoAssetsAbsentWhenDisabled pins "not enabled has zero impact": the asset
// path is not published, so a disabled deployment cannot serve a demo image.
func TestDemoAssetsAbsentWhenDisabled(t *testing.T) {
	clearMediaEnv(t)
	t.Setenv(demo.Env, "")
	hs := newMediaServer(t, nil)

	req, err := http.NewRequest(http.MethodGet, hs.URL+demo.AssetURL(demo.AssetSquare), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled asset = %d, want 404", resp.StatusCode)
	}
}

// TestDemoImageTurnReturnsHostedAsset pins the end-to-end image shape: a generate
// request against the demo provider answers a server-hosted asset URL and names
// demo-image.
func TestDemoImageTurnReturnsHostedAsset(t *testing.T) {
	clearMediaEnv(t)
	t.Setenv(demo.Env, "1")
	hs := newMediaServer(t, nil)
	tenant := uuid.NewString()

	status, body := doMedia(t, hs, http.MethodPost, "/v1/media/images/generate", map[string]any{
		"prompt": "a calm abstract tile",
		"size":   "1024x1536",
	}, tenant)
	if status != http.StatusOK {
		t.Fatalf("generate = %d, %s", status, body)
	}
	// The URL may carry an `?i=<index>` marker distinguishing the members of one
	// generation batch; the path is what must name the portrait tile.
	if !strings.Contains(body, demo.AssetURL(demo.AssetPortrait)) {
		t.Fatalf("body = %s, want the portrait asset URL", body)
	}
	if !strings.Contains(body, `"model":"demo-image"`) {
		t.Fatalf("body = %s, want model demo-image", body)
	}

	// The returned URL is fetchable on the same server — with its `?i=` marker, which
	// the asset handler must ignore (it addresses one member of a batch, not a
	// different resource).
	resp, err := http.Get(hs.URL + demo.AssetURL(demo.AssetPortrait) + "?i=0")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asset fetch = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("asset content-type = %q, want image/png", got)
	}
	if len(raw) < 8 || string(raw[1:4]) != "PNG" {
		t.Fatalf("asset is not a PNG: % x", raw)
	}

	// A square request gets the square tile; a landscape request the landscape one.
	for size, want := range map[string]string{
		"1024x1024": demo.AssetSquare,
		"1536x1024": demo.AssetLandscape,
	} {
		_, body := doMedia(t, hs, http.MethodPost, "/v1/media/images/generate", map[string]any{
			"prompt": "x",
			"size":   size,
		}, tenant)
		if !strings.Contains(body, demo.AssetURL(want)) {
			t.Fatalf("size %s body = %s, want %s", size, body, demo.AssetURL(want))
		}
	}
}

// TestDemoDecisionAnswersAreDeterministic pins the decision shapes: choice names
// one of the offered options, predicate reports a probability, score reports a
// level with the declared legend — and two identical requests answer identically.
func TestDemoDecisionAnswersAreDeterministic(t *testing.T) {
	clearMediaEnv(t)
	t.Setenv(demo.Env, "1")
	hs := newMediaServer(t, nil)
	tenant := uuid.NewString()
	request := map[string]any{
		"input": "A customer says the payment did not arrive after 24 hours.",
		"questions": []any{
			map[string]any{
				"type":      "choice",
				"predicate": "Which team should handle this?",
				"choices": []any{
					map[string]any{"value": "billing"},
					map[string]any{"value": "technical"},
				},
			},
			map[string]any{"type": "predicate", "predicate": "Should on-call be notified?"},
			map[string]any{
				"type":      "score",
				"predicate": "How severe is this?",
				"criteria":  []any{"low", "medium", "high"},
			},
		},
	}
	status, first := doMedia(t, hs, http.MethodPost, "/v1/media/decision", request, tenant)
	if status != http.StatusOK {
		t.Fatalf("decision = %d, %s", status, first)
	}
	_, second := doMedia(t, hs, http.MethodPost, "/v1/media/decision", request, tenant)
	if first != second {
		t.Fatalf("the demo decision is not deterministic:\n%s\n%s", first, second)
	}
	var out struct {
		Answers []struct {
			Type          string            `json:"type"`
			Choice        string            `json:"choice"`
			Probability   float64           `json:"probability"`
			Score         float64           `json:"score"`
			Legend        map[string]string `json:"legend"`
			Probabilities []float64         `json:"probabilities"`
		} `json:"answers"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(first), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, first)
	}
	if out.Model != demo.ModelDecision {
		t.Fatalf("model = %q, want %q", out.Model, demo.ModelDecision)
	}
	if len(out.Answers) != 3 {
		t.Fatalf("answers = %d, want 3", len(out.Answers))
	}
	if out.Answers[0].Type != "choice" || out.Answers[0].Choice != "billing" ||
		len(out.Answers[0].Probabilities) != 2 {
		t.Fatalf("choice answer = %+v", out.Answers[0])
	}
	if out.Answers[1].Type != "predicate" || out.Answers[1].Probability != 0.5 {
		t.Fatalf("predicate answer = %+v", out.Answers[1])
	}
	if out.Answers[2].Type != "score" || out.Answers[2].Legend["2"] != "high" {
		t.Fatalf("score answer = %+v", out.Answers[2])
	}
}

// TestDemoMediaRequiresTheSwitch pins "zero impact when off": with the vendor pair
// cleared and the demo switch off, every media route keeps its 503 and the
// capabilities report stays false.
func TestDemoMediaRequiresTheSwitch(t *testing.T) {
	clearMediaEnv(t)
	t.Setenv(demo.Env, "0")
	hs := newMediaServer(t, nil)
	tenant := uuid.NewString()

	for _, tc := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/media/images/generate", map[string]any{"prompt": "x"}},
		{"/v1/media/decision", map[string]any{
			"input":     "x",
			"questions": []any{map[string]any{"type": "predicate", "predicate": "p"}},
		}},
	} {
		if status, body := doMedia(t, hs, http.MethodPost, tc.path, tc.body, tenant); status != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d (%s), want 503", tc.path, status, body)
		}
	}

	status, body := doMedia(t, hs, http.MethodGet, "/v1/media/capabilities", nil, tenant)
	if status != http.StatusOK {
		t.Fatalf("capabilities = %d", status)
	}
	if !strings.Contains(body, `"configured":false`) || !strings.Contains(body, `"tts":false`) {
		t.Fatalf("capabilities = %s, want an unconfigured report", body)
	}
}

// TestDemoModelSelectableWithVendorConfigured pins the integration gap that broke
// a review deployment: /v1/models lists the demo models whenever the switch is on,
// so they are selectable. A deployment that ALSO holds vendor media credentials
// used to reject that selection with "model \"demo-image\" is not allowed" — a
// model the client could pick but never run.
//
// The rule: an explicit demo-* model is served by the demo provider (the switch is
// on), an unnamed request keeps going to the vendor.
func TestDemoModelSelectableWithVendorConfigured(t *testing.T) {
	fake, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	t.Setenv(demo.Env, "1")
	hs := newMediaServer(t, nil)
	tenant := uuid.NewString()

	// 1. Explicit demo-image with the vendor configured: 200 + hosted assets.
	status, body := doMedia(t, hs, http.MethodPost, "/v1/media/images/generate", map[string]any{
		"prompt": "雨夜霓虹街道",
		"n":      2,
		"size":   "1024x1024",
		"model":  demo.ModelImage,
	}, tenant)
	if status != http.StatusOK {
		t.Fatalf("demo-image generate = %d (%s), want 200", status, body)
	}
	var generated struct {
		Assets []struct {
			URL   string `json:"url"`
			Model string `json:"model"`
		} `json:"assets"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &generated); err != nil {
		t.Fatalf("decode generate: %v (%s)", err, body)
	}
	if generated.Model != demo.ModelImage {
		t.Fatalf("generate model = %q, want %q", generated.Model, demo.ModelImage)
	}
	if len(generated.Assets) != 2 {
		t.Fatalf("assets = %d, want 2", len(generated.Assets))
	}
	for i, asset := range generated.Assets {
		// n assets are n distinct URLs: `?i=0` and `?i=1` (the bytes are the same
		// tile, but a gallery keeps two separate entries).
		want := "/v1/demo/assets/square.png?i=" + strconv.Itoa(i)
		if asset.URL != want {
			t.Fatalf("asset %d URL = %q, want %q", i, asset.URL, want)
		}
		if asset.Model != demo.ModelImage {
			t.Fatalf("asset model = %q, want %q", asset.Model, demo.ModelImage)
		}
	}
	if fake.callCount("/images/generations") != 0 {
		t.Fatal("an explicit demo model must not reach the vendor")
	}

	// 2. Unnamed request with vendor credentials: unchanged vendor behavior.
	status, body = doMedia(t, hs, http.MethodPost, "/v1/media/images/generate", map[string]any{
		"prompt": "a lighthouse at dusk",
	}, tenant)
	if status != http.StatusOK {
		t.Fatalf("unnamed generate = %d (%s), want 200", status, body)
	}
	if !strings.Contains(body, "cdn.example.test/img-1.png") {
		t.Fatalf("unnamed generate = %s, want the vendor asset", body)
	}
	if got := fake.callCount("/images/generations"); got != 1 {
		t.Fatalf("vendor calls = %d, want exactly the unnamed request", got)
	}

	// 3. demo-decision on the ask surface with the vendor configured.
	status, body = doMedia(t, hs, http.MethodPost, "/v1/decisions/ask", map[string]any{
		"input": "A customer says the payment did not arrive after 24 hours.",
		"questions": []any{
			map[string]any{"type": "choice", "predicate": "Which team?", "choices": []any{
				map[string]any{"value": "billing"}, map[string]any{"value": "technical"},
			}},
			map[string]any{"type": "predicate", "predicate": "Notify on-call?"},
			map[string]any{"type": "score", "predicate": "Severity?", "criteria": []any{"low", "high"}},
		},
		"model": demo.ModelDecision,
	}, tenant)
	if status != http.StatusOK {
		t.Fatalf("demo-decision ask = %d (%s), want 200", status, body)
	}
	var decided struct {
		Answers []struct {
			Type        string            `json:"type"`
			Choice      string            `json:"choice"`
			Probability float64           `json:"probability"`
			Legend      map[string]string `json:"legend"`
			Prob        []float64         `json:"probabilities"`
		} `json:"answers"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &decided); err != nil {
		t.Fatalf("decode decision: %v (%s)", err, body)
	}
	if decided.Model != demo.ModelDecision {
		t.Fatalf("decision model = %q, want %q", decided.Model, demo.ModelDecision)
	}
	if len(decided.Answers) != 3 {
		t.Fatalf("answers = %d, want 3", len(decided.Answers))
	}
	if decided.Answers[0].Type != "choice" || decided.Answers[0].Choice != "billing" ||
		len(decided.Answers[0].Prob) != 2 {
		t.Fatalf("choice answer = %+v", decided.Answers[0])
	}
	if decided.Answers[1].Type != "predicate" || decided.Answers[1].Probability != 0.5 {
		t.Fatalf("predicate answer = %+v", decided.Answers[1])
	}
	if decided.Answers[2].Type != "score" || decided.Answers[2].Legend["1"] != "high" {
		t.Fatalf("score answer = %+v", decided.Answers[2])
	}

	// 4. The same ask without a model keeps going to the vendor.
	status, body = doMedia(t, hs, http.MethodPost, "/v1/decisions/ask", map[string]any{
		"input":     "x",
		"questions": []any{map[string]any{"type": "predicate", "predicate": "p"}},
	}, tenant)
	if status != http.StatusOK {
		t.Fatalf("unnamed ask = %d (%s), want 200", status, body)
	}
	if !strings.Contains(body, `"answer":"yes"`) {
		t.Fatalf("unnamed ask = %s, want the vendor answer", body)
	}
	if got := fake.callCount("/decisions"); got != 1 {
		t.Fatalf("vendor decision calls = %d, want exactly the unnamed request", got)
	}
}

// TestDemoModelRefusedWhenSwitchOff pins the boundary of the fix: with the switch
// off, naming a demo model is not served by the demo provider. With vendor
// credentials present the vendor allowlist answers its 400; without them the
// endpoint keeps its historical 503.
func TestDemoModelRefusedWhenSwitchOff(t *testing.T) {
	fake, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	t.Setenv(demo.Env, "")
	hs := newMediaServer(t, nil)
	tenant := uuid.NewString()

	status, body := doMedia(t, hs, http.MethodPost, "/v1/media/images/generate", map[string]any{
		"prompt": "x",
		"model":  demo.ModelImage,
	}, tenant)
	if status != http.StatusBadRequest {
		t.Fatalf("demo-image with the switch off = %d (%s), want 400", status, body)
	}
	if !strings.Contains(body, "is not allowed") {
		t.Fatalf("body = %s, want the allowlist rejection", body)
	}
	if fake.callCount("/images/generations") != 0 {
		t.Fatal("a rejected model must not reach the vendor")
	}

	clearMediaEnv(t)
	hsNoVendor := newMediaServer(t, nil)
	status, body = doMedia(t, hsNoVendor, http.MethodPost, "/v1/media/images/generate", map[string]any{
		"prompt": "x",
		"model":  demo.ModelImage,
	}, tenant)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("demo-image with no vendor and the switch off = %d (%s), want 503", status, body)
	}
}
