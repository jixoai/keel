package servicehost

// Provider probe tests: the one upstream request a `keel model provider test`
// (or `keel model catalog --refresh`) triggers. The tests pin the ordering —
// incomplete configuration and unresolvable credentials answer without a
// request, a reachable provider reports the live catalog, and no credential
// value ever appears in the exported result.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/modelcfg"
)

// probeDocument builds one tenant provider entry. The model id is a builtin
// one so per-model capability rules resolve; the provider itself is the
// tenant's (base URL and credential are what the probe consumes).
func probeDocument(providerID, baseURL, envName string) string {
	document := map[string]any{
		"providers": []any{map[string]any{
			"id":       providerID,
			"name":     providerID,
			"api":      map[string]any{"type": "openai-chat-completions", "baseUrl": baseURL},
			"access":   map[string]any{"type": "api-key", "env": envName},
			"modelIds": []any{"deepseek-v4.1-flash"},
		}},
	}
	raw, _ := json.Marshal(document)
	return string(raw)
}

func buildProbeService(t *testing.T, document string, lookup map[string]string) *modelsService {
	t.Helper()
	service, err := buildModelsService(host.TenantConfig{Models: json.RawMessage(document)}, testLLM(lookup, ""), nil)
	if err != nil {
		t.Fatalf("buildModelsService: %v", err)
	}
	return service
}

// TestProviderProbeReachesUpstream: a provider with a resolvable credential
// gets one authenticated model-list request, and the exported probe carries
// the measured catalog but never the key.
func TestProviderProbeReachesUpstream(t *testing.T) {
	const key = "probe-upstream-key-value"
	var hits int
	var authorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		authorization = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/models" {
			t.Errorf("upstream path = %q, want /v1/models", r.URL.Path)
		}
		writeProbeJSON(w, map[string]any{"data": []any{
			map[string]any{"id": "model-b"}, map[string]any{"id": "model-a"},
		}})
	}))
	defer upstream.Close()

	service := buildProbeService(t, probeDocument("gw", upstream.URL+"/v1", "GW_KEY"), map[string]string{"GW_KEY": key})
	probe, found, err := service.ProbeProvider(context.Background(), "gw")
	if err != nil || !found {
		t.Fatalf("ProbeProvider: found=%v err=%v", found, err)
	}
	if !probe.OK || probe.Status != modelcfg.ProbeStatusReady {
		t.Fatalf("probe = %+v, want ready", probe)
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits)
	}
	if authorization != "Bearer "+key {
		t.Fatal("the probe did not authenticate with the resolved credential")
	}
	if probe.ModelCount != 2 || len(probe.Models) != 2 || probe.Models[0] != "model-a" || probe.Models[1] != "model-b" {
		t.Fatalf("probe models = %+v", probe.Models)
	}
	raw, _ := json.Marshal(probe)
	if strings.Contains(string(raw), key) {
		t.Fatal("the credential value leaked into the probe result")
	}
}

// TestProviderProbeNeedsAuthWithoutRequest: an unresolvable credential is a
// measured needs_auth with a field path, and no request leaves the process.
func TestProviderProbeNeedsAuthWithoutRequest(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer upstream.Close()

	service := buildProbeService(t, probeDocument("gw", upstream.URL+"/v1", "GW_MISSING_KEY"), nil)
	probe, found, err := service.ProbeProvider(context.Background(), "gw")
	if err != nil || !found {
		t.Fatalf("ProbeProvider: found=%v err=%v", found, err)
	}
	if probe.OK || probe.Status != modelcfg.ProbeStatusNeedsAuth {
		t.Fatalf("probe = %+v, want needs_auth", probe)
	}
	if len(probe.Issues) != 1 || probe.Issues[0].Field != "access.env" {
		t.Fatalf("probe issues = %+v", probe.Issues)
	}
	if hits != 0 {
		t.Fatalf("a needs_auth probe sent %d request(s)", hits)
	}
}

// TestProviderProbeUnreachableReportsTheFailure: a non-200 upstream is an
// unreachable probe carrying the status text, not a fabricated success.
func TestProviderProbeUnreachableReportsTheFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no catalog here", http.StatusBadGateway)
	}))
	defer upstream.Close()

	service := buildProbeService(t, probeDocument("gw", upstream.URL+"/v1", "GW_KEY"), map[string]string{"GW_KEY": "k"})
	probe, found, err := service.ProbeProvider(context.Background(), "gw")
	if err != nil || !found {
		t.Fatalf("ProbeProvider: found=%v err=%v", found, err)
	}
	if probe.OK || probe.Status != modelcfg.ProbeStatusUnreachable {
		t.Fatalf("probe = %+v, want unreachable", probe)
	}
	if !strings.Contains(probe.Error, "502") {
		t.Fatalf("probe error = %q, want the upstream status", probe.Error)
	}
}

// TestProviderProbeRejectedCredentialIsNeedsAuth: an endpoint that answers 401
// rejected the credential. That is needs_auth (the fix is the reference), not
// a transport failure, and the result still carries no value.
func TestProviderProbeRejectedCredentialIsNeedsAuth(t *testing.T) {
	const key = "probe-rejected-key-value"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	service := buildProbeService(t, probeDocument("gw", upstream.URL+"/v1", "GW_KEY"), map[string]string{"GW_KEY": key})
	probe, found, err := service.ProbeProvider(context.Background(), "gw")
	if err != nil || !found {
		t.Fatalf("ProbeProvider: found=%v err=%v", found, err)
	}
	if probe.OK || probe.Status != modelcfg.ProbeStatusNeedsAuth {
		t.Fatalf("probe = %+v, want needs_auth", probe)
	}
	if len(probe.Issues) != 1 || probe.Issues[0].Field != "access" {
		t.Fatalf("probe issues = %+v", probe.Issues)
	}
	raw, _ := json.Marshal(probe)
	if strings.Contains(string(raw), key) {
		t.Fatal("the rejected credential leaked into the probe result")
	}
}

// TestProviderProbeIncompleteConfigurationSendsNothing: a provider that is
// not executable on paper is invalid_config with the settings issue, and no
// request is sent.
func TestProviderProbeIncompleteConfigurationSendsNothing(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer upstream.Close()

	// No base URL: the provider cannot execute, so the probe must not guess one.
	document := `{"providers":[{"id":"gw","api":{"type":"openai-chat-completions"},` +
		`"access":{"type":"api-key","env":"GW_KEY"},"modelIds":["deepseek-v4.1-flash"]}]}`
	service := buildProbeService(t, document, map[string]string{"GW_KEY": "k"})
	probe, found, err := service.ProbeProvider(context.Background(), "gw")
	if err != nil || !found {
		t.Fatalf("ProbeProvider: found=%v err=%v", found, err)
	}
	if probe.OK || probe.Status != modelcfg.ProbeStatusInvalidConfig {
		t.Fatalf("probe = %+v, want invalid_config", probe)
	}
	if hits != 0 {
		t.Fatalf("an invalid provider sent %d request(s)", hits)
	}
}

// TestProviderProbeUnknownAndHiddenAreMisses: the id must name a visible
// provider of this resolution; an unknown or hidden id is found=false.
func TestProviderProbeUnknownAndHiddenAreMisses(t *testing.T) {
	document := `{"providers":[
		{"id":"gw","api":{"type":"openai-chat-completions","baseUrl":"https://gw.example.invalid/v1"},
		 "access":{"type":"api-key","env":"GW_KEY"},"modelIds":["deepseek-v4.1-flash"]},
		{"id":"hidden-gw","visibility":"hidden",
		 "api":{"type":"openai-chat-completions","baseUrl":"https://hidden.example.invalid/v1"},
		 "access":{"type":"api-key","env":"HIDDEN_KEY"},"modelIds":["deepseek-v4.1-flash"]}
	]}`
	service := buildProbeService(t, document, map[string]string{"GW_KEY": "k", "HIDDEN_KEY": "k"})
	for _, id := range []string{"nope", "hidden-gw"} {
		if _, found, err := service.ProbeProvider(context.Background(), id); err != nil || found {
			t.Fatalf("ProbeProvider(%q): found=%v err=%v, want a miss", id, found, err)
		}
	}
}

func writeProbeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
