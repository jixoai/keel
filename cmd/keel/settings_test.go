package main

// End-to-end settings test (change keel-model-settings) over the real HTTP
// surface: the store, the service runtime, the agentapi settings endpoints and
// the runtime edge's per-request selection seam are the production ones; only
// the kernel is the in-memory implementation. Skipped without KEEL_TEST_DSN
// (the write path is durable).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentapi"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/servicehost"
	"github.com/jixoai/keel/internal/store"
)

func settingsTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func doSettingsRequest(t *testing.T, client *http.Client, method, url string, tenant uuid.UUID, body any, out any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if tenant != uuid.Nil {
		req.Header.Set("X-Tenant-ID", tenant.String())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s %s (%d): %v body=%s", method, url, res.StatusCode, err, raw)
		}
	}
	return res.StatusCode
}

const settingsDeploymentDocument = `{
  "providers": [{"id": "gw", "api": {"type": "openai-chat-completions", "baseUrl": "https://gw.example.invalid/v1"},
    "access": {"type": "api-key", "env": "GW_KEY"}, "modelIds": ["deepseek-v4.1-flash", "claude-sonnet-5"]}],
  "selection": {"providerId": "gw", "modelId": "deepseek-v4.1-flash", "reasoningLevel": "high"}
}`

// TestSettingsEndToEndOverHTTP is the settings acceptance gate inside one
// process: write a secret, write a models document that references it, and the
// next run resolves the new identity — the input edge agrees, the stored read
// converges, and a reset returns everything to the deployment document.
func TestSettingsEndToEndOverHTTP(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	log := settingsTestLogger()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "settings-e2e"); err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	box, err := store.NewSecretBox(bytes.Repeat([]byte{0x77}, 32))
	if err != nil {
		t.Fatal(err)
	}

	mem := kernel.NewMemStore()
	// The in-memory kernel is this test's writer of record: it must know the
	// session the edge commits into (the store row alone is not enough).
	mem.CreateSession(tenant, session)
	k := kernel.New(mem, kernel.Options{})
	rt, err := servicehost.Boot(ctx, servicehost.Config{
		Seams:     servicehost.Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: st, Logger: log},
		Tenant:    host.TenantConfig{Models: json.RawMessage(settingsDeploymentDocument)},
		RuntimeID: "settings-e2e",
		LLM: servicehost.LLMOptions{
			// Only the deployment provider's environment variable is "set"; the
			// secret store is the only place a new provider's credential can come
			// from.
			LookupEnv: func(name string) (string, bool) {
				if name == "GW_KEY" {
					return "env-key", true
				}
				return "", false
			},
			LookupSecret: tenantSecretLookup(st, box, tenant, log),
		},
	})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })

	modelSeam := modelRuntimeSeam{rt: rt}
	settingsAPI := agentapi.New(agentapi.Config{
		ModelsLive: func() agentapi.ModelCatalog {
			if rt.Models() == nil {
				return nil
			}
			return modelSeam
		},
		ModelSettingsStore:   st,
		ModelSettingsRuntime: modelSeam,
		SecretStore:          tenantSecretSeam{store: st, box: box},
		Logger:               log,
	})
	api := httpapi.New(httpapi.Config{
		Store:             st,
		Edge:              k,
		Events:            k,
		Product:           settingsAPI,
		ValidateSelection: selectFromCurrentGeneration(rt),
		Logger:            log,
	})
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	client := server.Client()
	call := func(method, path string, body any, out any) int {
		return doSettingsRequest(t, client, method, server.URL+path, tenant, body, out)
	}

	// The deployment document is what is served before any write.
	var settings struct {
		Revision int64           `json:"revision"`
		Source   string          `json:"source"`
		Document json.RawMessage `json:"document"`
		Issues   []any           `json:"issues"`
	}
	if code := call(http.MethodGet, "/v1/settings/models", nil, &settings); code != 200 {
		t.Fatalf("GET models = %d", code)
	}
	if settings.Source != "deployment" || settings.Revision != 0 {
		t.Fatalf("deployment read = %+v", settings)
	}

	// --- secrets: write one; the value never comes back ---
	var secret struct {
		Name      string    `json:"name"`
		UpdatedAt time.Time `json:"updatedAt"`
	}
	if code := call(http.MethodPut, "/v1/settings/secrets/gw_key", map[string]string{"value": "stored-gateway-key"}, &secret); code != 200 {
		t.Fatalf("PUT secret = %d", code)
	}
	if secret.Name != "gw_key" || secret.UpdatedAt.IsZero() {
		t.Fatalf("PUT secret response = %+v", secret)
	}
	var secretsRaw json.RawMessage
	if code := call(http.MethodGet, "/v1/settings/secrets", nil, &secretsRaw); code != 200 {
		t.Fatalf("GET secrets = %d", code)
	}
	if strings.Contains(string(secretsRaw), "stored-gateway-key") {
		t.Fatalf("the secret list leaked the value: %s", secretsRaw)
	}

	// --- models: a document whose provider is credentialed by that secret ---
	storedDocument := map[string]any{
		"providers": []any{map[string]any{
			"id":       "secret-gw",
			"api":      map[string]any{"type": "openai-chat-completions", "baseUrl": "https://secret.example.invalid/v1"},
			"access":   map[string]any{"type": "api-key", "ref": "secret:gw_key"},
			"modelIds": []string{"claude-sonnet-5"},
		}},
		"selection": map[string]any{"providerId": "secret-gw", "modelId": "claude-sonnet-5", "reasoningLevel": "low"},
	}
	var written struct {
		Revision  int64  `json:"revision"`
		Source    string `json:"source"`
		Selection struct {
			ProviderID     string `json:"providerId"`
			ModelID        string `json:"modelId"`
			ReasoningLevel string `json:"reasoningLevel"`
		} `json:"selection"`
		ProvidersExecutable int `json:"providersExecutable"`
		ModelsSelectable    int `json:"modelsSelectable"`
	}
	if code := call(http.MethodPut, "/v1/settings/models", storedDocument, &written); code != 200 {
		t.Fatalf("PUT models = %d", code)
	}
	if written.Source != "stored" || written.Revision != 1 {
		t.Fatalf("PUT summary = %+v", written)
	}
	if written.Selection.ProviderID != "secret-gw" || written.Selection.ModelID != "claude-sonnet-5" ||
		written.Selection.ReasoningLevel != "low" {
		t.Fatalf("PUT selection = %+v", written.Selection)
	}
	if written.ProvidersExecutable != 1 || written.ModelsSelectable != 1 {
		t.Fatalf("PUT counts = %d/%d, want the one secret-credentialed provider",
			written.ProvidersExecutable, written.ModelsSelectable)
	}

	// A new run resolves the new identity — with the credential the secret store
	// holds, not the environment's — without a restart.
	lease, err := rt.Binding().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	active, err := lease.Runner().Models.Resolve(ctx, kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("resolve after write: %v", err)
	}
	if active.ProviderID != "secret-gw" || active.Model != "claude-sonnet-5" || active.APIKey != "stored-gateway-key" {
		t.Fatalf("run identity after write = %s/%s key %q", active.ProviderID, active.Model, active.APIKey)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatal(err)
	}

	// The input edge reads the same generation: the new selection is accepted,
	// the retired one is refused.
	var input map[string]any
	code := call(http.MethodPost, "/v1/sessions/"+session.String()+"/input", map[string]any{
		"text":      "hello",
		"selection": map[string]any{"providerId": "secret-gw", "modelId": "claude-sonnet-5"},
	}, &input)
	if code != 201 {
		t.Fatalf("input with the new selection = %d %v", code, input)
	}
	input = map[string]any{}
	code = call(http.MethodPost, "/v1/sessions/"+session.String()+"/input", map[string]any{
		"text":      "hello again",
		"selection": map[string]any{"providerId": "gw", "modelId": "deepseek-v4.1-flash"},
	}, &input)
	if code != 400 {
		t.Fatalf("input with the retired selection = %d %v, want 400", code, input)
	}

	// The registry projection follows the live runtime; the stored read
	// converges on the same revision.
	var models struct {
		Models []struct {
			ProviderID string `json:"providerId"`
			ModelID    string `json:"modelId"`
		} `json:"models"`
	}
	if code := call(http.MethodGet, "/v1/models", nil, &models); code != 200 {
		t.Fatalf("GET models registry = %d", code)
	}
	if len(models.Models) != 1 || models.Models[0].ProviderID != "secret-gw" {
		t.Fatalf("registry projection = %+v", models.Models)
	}
	settings = struct {
		Revision int64           `json:"revision"`
		Source   string          `json:"source"`
		Document json.RawMessage `json:"document"`
		Issues   []any           `json:"issues"`
	}{}
	if code := call(http.MethodGet, "/v1/settings/models", nil, &settings); code != 200 ||
		settings.Source != "stored" || settings.Revision != 1 {
		t.Fatalf("stored read = %+v (%d)", settings, code)
	}

	// Boot precedence: a process that boots now reads the stored override and
	// serves it instead of the deployment document — the same read `keel serve`
	// performs before assembling its runtime.
	stored, revision, _, found, err := st.GetModelSettings(ctx, tenant)
	if err != nil || !found || revision != 1 {
		t.Fatalf("stored override = (revision %d, found %v, err %v)", revision, found, err)
	}
	rebooted, err := servicehost.Boot(ctx, servicehost.Config{
		Seams:     servicehost.Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: st, Logger: log},
		Tenant:    host.TenantConfig{Models: stored},
		RuntimeID: "settings-e2e-reboot",
		LLM: servicehost.LLMOptions{
			LookupSecret: tenantSecretLookup(st, box, tenant, log),
		},
	})
	if err != nil {
		t.Fatalf("reboot: %v", err)
	}
	if got := rebooted.ProviderName() + "/" + rebooted.Model(); got != "secret-gw/claude-sonnet-5" {
		t.Fatalf("a boot on the stored override served %s, want the stored document's identity", got)
	}
	if err := rebooted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Reset: back to the deployment document, on the edge and in the runtime.
	var reset struct {
		Revision int64  `json:"revision"`
		Source   string `json:"source"`
	}
	if code := call(http.MethodDelete, "/v1/settings/models?expectedRevision=1", nil, &reset); code != 200 {
		t.Fatalf("DELETE models = %d", code)
	}
	if reset.Revision != 2 || reset.Source != "deployment" {
		t.Fatalf("DELETE models response = %+v", reset)
	}
	lease, err = rt.Binding().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire after reset: %v", err)
	}
	active, err = lease.Runner().Models.Resolve(ctx, kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("resolve after reset: %v", err)
	}
	if active.ProviderID != "gw" || active.APIKey != "env-key" {
		t.Fatalf("identity after reset = %s/%s", active.ProviderID, active.Model)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	input = map[string]any{}
	code = call(http.MethodPost, "/v1/sessions/"+session.String()+"/input", map[string]any{
		"text":      "back to default",
		"selection": map[string]any{"providerId": "secret-gw", "modelId": "claude-sonnet-5"},
	}, &input)
	if code != 400 {
		t.Fatalf("input with the cleared selection = %d %v, want 400", code, input)
	}
}
