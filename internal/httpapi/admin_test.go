package httpapi

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

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// Administrator tenant API tests. Skipped when KEEL_TEST_DSN is unset, like
// the other httpapi integration tests.

func newAdminTestServer(t *testing.T, adminKey string) (*httptest.Server, *store.Store) {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := store.Open(context.Background(), store.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	srv := New(Config{
		Store:    st,
		AdminKey: adminKey,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return hs, st
}

func doAdminJSON(t *testing.T, hc *http.Client, method, url, token string, body any, out any) int {
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
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return res.StatusCode
}

func TestAdminConfigRequiresBearerKey(t *testing.T) {
	hs, st := newAdminTestServer(t, "operator-secret")
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "admin-auth"); err != nil {
		t.Fatal(err)
	}
	url := hs.URL + "/v1/tenants/" + tenant.String() + "/config"
	hc := hs.Client()
	if code := doAdminJSON(t, hc, http.MethodGet, url, "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", code)
	}
	if code := doAdminJSON(t, hc, http.MethodGet, url, "wrong", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", code)
	}
	if code := doAdminJSON(t, hc, http.MethodPut, url, "", map[string]any{}, nil); code != http.StatusUnauthorized {
		t.Fatalf("put without token = %d, want 401", code)
	}
	if code := doAdminJSON(t, hc, http.MethodPut, url, "wrong", map[string]any{}, nil); code != http.StatusUnauthorized {
		t.Fatalf("put with wrong token = %d, want 401", code)
	}
}

func TestAdminConfigLifecycle(t *testing.T) {
	hs, st := newAdminTestServer(t, "operator-secret")
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "admin-config"); err != nil {
		t.Fatal(err)
	}
	hc := hs.Client()
	url := hs.URL + "/v1/tenants/" + tenant.String() + "/config"

	var written struct {
		Revision int64 `json:"revision"`
	}
	config := map[string]any{
		"agent": map[string]any{"provider": "openai", "model": "gpt-5", "thinking": "high"},
		"mcp": map[string]any{"servers": map[string]any{
			"filesystem": map[string]any{"command": "npx", "args": []string{"-y", "fs"}},
		}},
		"compaction": map[string]any{"enabled": true, "threshold": 0.8, "keepRecentTokens": 20000},
		"telemetry":  map[string]any{"adapter": "memory"},
	}
	if code := doAdminJSON(t, hc, http.MethodPut, url, "operator-secret", config, &written); code != http.StatusOK {
		t.Fatalf("put config = %d", code)
	}
	if written.Revision != 2 {
		t.Fatalf("revision = %d, want 2", written.Revision)
	}

	var read struct {
		Revision int64          `json:"revision"`
		Config   map[string]any `json:"config"`
	}
	if code := doAdminJSON(t, hc, http.MethodGet, url, "operator-secret", nil, &read); code != http.StatusOK {
		t.Fatalf("get config = %d", code)
	}
	if read.Revision != 2 {
		t.Fatalf("read revision = %d, want 2", read.Revision)
	}
	agent, ok := read.Config["agent"].(map[string]any)
	if !ok || agent["model"] != "gpt-5" || agent["thinking"] != "high" {
		t.Fatalf("stored agent = %v", read.Config["agent"])
	}

	// A second write bumps the revision and replaces the document.
	if code := doAdminJSON(t, hc, http.MethodPut, url, "operator-secret",
		map[string]any{"telemetry": map[string]any{"adapter": "memory"}}, &written); code != http.StatusOK {
		t.Fatalf("second put = %d", code)
	}
	if written.Revision != 3 {
		t.Fatalf("second revision = %d, want 3", written.Revision)
	}
	if revision, err := st.GetTenantRevision(context.Background(), tenant); err != nil || revision != 3 {
		t.Fatalf("store revision = %d, err = %v", revision, err)
	}
}

func TestAdminConfigRejectsInvalidDocuments(t *testing.T) {
	hs, st := newAdminTestServer(t, "operator-secret")
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "admin-invalid"); err != nil {
		t.Fatal(err)
	}
	hc := hs.Client()
	url := hs.URL + "/v1/tenants/" + tenant.String() + "/config"
	cases := []struct {
		name string
		body any
	}{
		{"unknown field", map[string]any{"nope": true}},
		{"bad protocol", map[string]any{"agent": map[string]any{"protocol": "grpc"}}},
		{"bad threshold", map[string]any{"compaction": map[string]any{"threshold": 2}}},
		{"mcp both transports", map[string]any{"mcp": map[string]any{"servers": map[string]any{
			"s": map[string]any{"command": "cmd", "url": "https://example/mcp"},
		}}}},
		{"non object", []string{"not", "an", "object"}},
		{"literal api key ref", map[string]any{"agent": map[string]any{"apiKeyRef": "sk-live-not-a-real-key"}}},
		{"literal bearer header", map[string]any{"mcp": map[string]any{"servers": map[string]any{
			"s": map[string]any{"url": "https://example/mcp",
				"headers": map[string]any{"Authorization": "Bearer secret"}},
		}}}},
		{"literal env value", map[string]any{"mcp": map[string]any{"servers": map[string]any{
			"s": map[string]any{"command": "cmd", "env": map[string]any{"TOKEN": "plain-text"}},
		}}}},
		{"bad section name", map[string]any{"agent": map[string]any{
			"sections": []map[string]any{{"name": "Bad Name", "content": "x"}},
		}}},
		{"bad mcp server name", map[string]any{"mcp": map[string]any{"servers": map[string]any{
			"Server": map[string]any{"command": "cmd"},
		}}}},
		{"bad tool allow", map[string]any{"tools": map[string]any{"allow": []string{"bad name"}}}},
		// The provider configuration is boot-time only until a reload path
		// exists; accepting it here would store a promise nothing reads, and a
		// literal secret would ride along unvalidated.
		{"models section", map[string]any{"models": map[string]any{"providers": []any{}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := doAdminJSON(t, hc, http.MethodPut, url, "operator-secret", tc.body, nil); code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", code)
			}
		})
	}
	if code := doAdminJSON(t, hc, http.MethodGet, hs.URL+"/v1/tenants/not-a-uuid/config", "operator-secret", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("bad tenant id = %d, want 400", code)
	}
	if code := doAdminJSON(t, hc, http.MethodGet, hs.URL+"/v1/tenants/"+uuid.NewString()+"/config", "operator-secret", nil, nil); code != http.StatusNotFound {
		t.Fatalf("missing tenant = %d, want 404", code)
	}
	if code := doAdminJSON(t, hc, http.MethodPut, hs.URL+"/v1/tenants/"+uuid.NewString()+"/config",
		"operator-secret", map[string]any{}, nil); code != http.StatusNotFound {
		t.Fatalf("put missing tenant = %d, want 404", code)
	}
}

func TestAdminConfigPreservesExplicitEmptyCollections(t *testing.T) {
	hs, st := newAdminTestServer(t, "operator-secret")
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "admin-empty"); err != nil {
		t.Fatal(err)
	}
	hc := hs.Client()
	url := hs.URL + "/v1/tenants/" + tenant.String() + "/config"
	body := map[string]any{
		"agent":  map[string]any{"sections": []any{}},
		"skills": map[string]any{"roots": []string{}},
		"hooks":  map[string]any{"commands": []any{}},
		"tools":  map[string]any{"allow": []string{}},
	}
	if code := doAdminJSON(t, hc, http.MethodPut, url, "operator-secret", body, nil); code != http.StatusOK {
		t.Fatalf("put empty collections = %d", code)
	}
	var read struct {
		Config map[string]any `json:"config"`
	}
	if code := doAdminJSON(t, hc, http.MethodGet, url, "operator-secret", nil, &read); code != http.StatusOK {
		t.Fatalf("get config = %d", code)
	}
	requireEmptyArray := func(object map[string]any, key, label string) {
		t.Helper()
		value, exists := object[key]
		if !exists {
			t.Fatalf("%s.%s is missing from %v", label, key, object)
		}
		array, ok := value.([]any)
		if !ok {
			t.Fatalf("%s.%s = %#v, want an empty array", label, key, value)
		}
		if len(array) != 0 {
			t.Fatalf("%s.%s = %#v, want an empty array", label, key, array)
		}
	}
	agent, _ := read.Config["agent"].(map[string]any)
	skills, _ := read.Config["skills"].(map[string]any)
	hooks, _ := read.Config["hooks"].(map[string]any)
	tools, _ := read.Config["tools"].(map[string]any)
	requireEmptyArray(agent, "sections", "agent")
	requireEmptyArray(skills, "roots", "skills")
	requireEmptyArray(hooks, "commands", "hooks")
	requireEmptyArray(tools, "allow", "tools")
}

func TestAdminConfigAcceptsSecretReferences(t *testing.T) {
	hs, st := newAdminTestServer(t, "operator-secret")
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "admin-secrets"); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"agent": map[string]any{"apiKeyRef": "env:OPENAI_API_KEY"},
		"mcp": map[string]any{"servers": map[string]any{
			"remote": map[string]any{
				"url":     "https://mcp.example/mcp",
				"headers": map[string]any{"Authorization": "${MCP_TOKEN}"},
				"env":     map[string]any{"MCP_MODE": "secret:MCP_MODE"},
			},
		}},
	}
	if code := doAdminJSON(t, hs.Client(), http.MethodPut,
		hs.URL+"/v1/tenants/"+tenant.String()+"/config", "operator-secret", body, nil); code != http.StatusOK {
		t.Fatalf("put secret references = %d", code)
	}
}

func TestAdminBearerSchemeIsCaseInsensitive(t *testing.T) {
	hs, st := newAdminTestServer(t, "operator-secret")
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "admin-bearer"); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet,
		hs.URL+"/v1/tenants/"+tenant.String()+"/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "bEaReR operator-secret")
	res, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("mixed-case bearer = %d %s", res.StatusCode, raw)
	}
}

func TestAdminConfigBodyTooLarge(t *testing.T) {
	hs, st := newAdminTestServer(t, "operator-secret")
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "admin-toolarge"); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("x", (1<<20)+1024)
	body := map[string]any{"agent": map[string]any{"systemPrompt": huge}}
	code := doAdminJSON(t, hs.Client(), http.MethodPut,
		hs.URL+"/v1/tenants/"+tenant.String()+"/config", "operator-secret", body, nil)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", code)
	}
}

func TestAdminUsageAggregatesLedger(t *testing.T) {
	hs, st := newAdminTestServer(t, "operator-secret")
	ctx := context.Background()
	tenant := uuid.New()
	session := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "admin-usage"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	records := []store.UsageRecord{
		{TenantID: tenant, SessionID: session, RequestID: "req-1", Seq: 1, Provider: "openai", Model: "gpt-5",
			InputTokens: 100, OutputTokens: 40, CacheReadTokens: 30, CacheWriteTokens: 20, ReasoningTokens: 10},
		{TenantID: tenant, SessionID: session, RequestID: "req-2", Seq: 2, Provider: "openai", Model: "gpt-5",
			InputTokens: 50, OutputTokens: 5},
		// One request may have been billed before the ledger append crashed.
		{TenantID: tenant, SessionID: session, RequestID: "req-3", Seq: 3, Provider: "openai", Model: "gpt-5",
			Status: store.UsageUnaccounted},
	}
	for _, record := range records {
		if recorded, err := st.AppendUsage(ctx, record); err != nil || !recorded {
			t.Fatalf("append usage: recorded=%v err=%v", recorded, err)
		}
	}
	// A duplicate request id must not double count.
	if recorded, err := st.AppendUsage(ctx, records[0]); err != nil || recorded {
		t.Fatalf("duplicate append: recorded=%v err=%v", recorded, err)
	}

	var body struct {
		Usage []struct {
			SessionID          string `json:"sessionId"`
			Provider           string `json:"provider"`
			Model              string `json:"model"`
			Entries            int64  `json:"entries"`
			UnaccountedEntries int64  `json:"unaccountedEntries"`
			InputTokens        int64  `json:"inputTokens"`
			OutputTokens       int64  `json:"outputTokens"`
			CacheReadTokens    int64  `json:"cacheReadTokens"`
			CacheWriteTokens   int64  `json:"cacheWriteTokens"`
			ReasoningTokens    int64  `json:"reasoningTokens"`
		} `json:"usage"`
		Totals struct {
			Entries            int64 `json:"entries"`
			UnaccountedEntries int64 `json:"unaccountedEntries"`
			InputTokens        int64 `json:"inputTokens"`
			OutputTokens       int64 `json:"outputTokens"`
			CacheReadTokens    int64 `json:"cacheReadTokens"`
			CacheWriteTokens   int64 `json:"cacheWriteTokens"`
			ReasoningTokens    int64 `json:"reasoningTokens"`
		} `json:"totals"`
	}
	url := hs.URL + "/v1/tenants/" + tenant.String() + "/usage"
	if code := doAdminJSON(t, hs.Client(), http.MethodGet, url, "operator-secret", nil, &body); code != http.StatusOK {
		t.Fatalf("get usage = %d", code)
	}
	if len(body.Usage) != 1 {
		t.Fatalf("usage rows = %+v", body.Usage)
	}
	row := body.Usage[0]
	if row.SessionID != session.String() || row.Provider != "openai" || row.Model != "gpt-5" ||
		row.Entries != 3 || row.UnaccountedEntries != 1 {
		t.Fatalf("row = %+v", row)
	}
	if row.InputTokens != 150 || row.OutputTokens != 45 || row.CacheReadTokens != 30 ||
		row.CacheWriteTokens != 20 || row.ReasoningTokens != 10 {
		t.Fatalf("row totals = %+v", row)
	}
	if body.Totals.Entries != 3 || body.Totals.UnaccountedEntries != 1 ||
		body.Totals.InputTokens != 150 || body.Totals.OutputTokens != 45 {
		t.Fatalf("totals = %+v", body.Totals)
	}
	if code := doAdminJSON(t, hs.Client(), http.MethodGet,
		hs.URL+"/v1/tenants/"+uuid.NewString()+"/usage", "operator-secret", nil, nil); code != http.StatusNotFound {
		t.Fatalf("usage missing tenant = %d, want 404", code)
	}
	if code := doAdminJSON(t, hs.Client(), http.MethodGet, url, "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("usage without token = %d, want 401", code)
	}
}

func TestAdminDisabledWithoutConfiguredKey(t *testing.T) {
	if os.Getenv("KEEL_ADMIN_KEY") != "" {
		t.Skip("KEEL_ADMIN_KEY is set in the environment; cannot exercise the disabled path")
	}
	hs, st := newAdminTestServer(t, "")
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "admin-disabled"); err != nil {
		t.Fatal(err)
	}
	code := doAdminJSON(t, hs.Client(), http.MethodGet,
		hs.URL+"/v1/tenants/"+tenant.String()+"/config", "anything", nil, nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("disabled admin api = %d, want 503", code)
	}
}

// doTenantJSON posts to /v1/tenants with the explicit tenant id header and an
// optional admin bearer token.
func doTenantJSON(t *testing.T, hc *http.Client, url, tenantID, token string, body any) int {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// TestTenantProvisioningRequiresAdminKey pins the security boundary: creating
// a tenant is an operator action. A caller-chosen X-Tenant-ID is the row id,
// never proof of identity.
func TestTenantProvisioningRequiresAdminKey(t *testing.T) {
	ctx := context.Background()
	hs, st := newAdminTestServer(t, "operator-secret")
	tenant := uuid.NewString()
	url := hs.URL + "/v1/tenants"
	body := map[string]string{"name": "provisioned"}
	if code := doTenantJSON(t, hs.Client(), url, tenant, "", body); code != http.StatusUnauthorized {
		t.Fatalf("provision without key = %d, want 401", code)
	}
	if code := doTenantJSON(t, hs.Client(), url, tenant, "wrong", body); code != http.StatusUnauthorized {
		t.Fatalf("provision with wrong key = %d, want 401", code)
	}
	parsed, err := uuid.Parse(tenant)
	if err != nil {
		t.Fatal(err)
	}
	if exists, err := st.TenantExists(ctx, parsed); err != nil || exists {
		t.Fatalf("unauthenticated provisioning created the tenant: exists=%v err=%v", exists, err)
	}
	if code := doTenantJSON(t, hs.Client(), url, tenant, "operator-secret", body); code != http.StatusCreated {
		t.Fatalf("provision with the operator key = %d, want 201", code)
	}
	if exists, err := st.TenantExists(ctx, parsed); err != nil || !exists {
		t.Fatalf("authenticated provisioning did not create the tenant: exists=%v err=%v", exists, err)
	}
}

// TestTenantProvisioningDisabledWithoutConfiguredKey keeps the admin API
// fail-closed: without KEEL_ADMIN_KEY the endpoint is disabled (503), not open.
func TestTenantProvisioningDisabledWithoutConfiguredKey(t *testing.T) {
	if os.Getenv("KEEL_ADMIN_KEY") != "" {
		t.Skip("KEEL_ADMIN_KEY is set in the environment; cannot exercise the disabled path")
	}
	hs, _ := newAdminTestServer(t, "")
	code := doTenantJSON(t, hs.Client(), hs.URL+"/v1/tenants", uuid.NewString(), "anything",
		map[string]string{"name": "disabled"})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("disabled provisioning = %d, want 503", code)
	}
}

// TestAdminKeyRotatesThroughEnvironmentPerRequest is the P1-10 regression: an
// empty Config.AdminKey reads KEEL_ADMIN_KEY on every request, so an operator
// can rotate the key without restarting the process. Both the tenant config API
// and tenant provisioning follow the new key immediately.
func TestAdminKeyRotatesThroughEnvironmentPerRequest(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KEEL_ADMIN_KEY", "first-key")
	hs, st := newAdminTestServer(t, "")
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "admin-rotation"); err != nil {
		t.Fatal(err)
	}
	configURL := hs.URL + "/v1/tenants/" + tenant.String() + "/config"
	if code := doAdminJSON(t, hs.Client(), http.MethodGet, configURL, "first-key", nil, nil); code != http.StatusOK {
		t.Fatalf("first key = %d, want 200", code)
	}

	t.Setenv("KEEL_ADMIN_KEY", "second-key")
	if code := doAdminJSON(t, hs.Client(), http.MethodGet, configURL, "first-key", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("rotated-out key = %d, want 401", code)
	}
	if code := doAdminJSON(t, hs.Client(), http.MethodGet, configURL, "second-key", nil, nil); code != http.StatusOK {
		t.Fatalf("rotated-in key = %d, want 200", code)
	}
	if code := doTenantJSON(t, hs.Client(), hs.URL+"/v1/tenants", uuid.NewString(), "first-key",
		map[string]string{"name": "rotated"}); code != http.StatusUnauthorized {
		t.Fatalf("rotated-out key provisioning = %d, want 401", code)
	}
	if code := doTenantJSON(t, hs.Client(), hs.URL+"/v1/tenants", uuid.NewString(), "second-key",
		map[string]string{"name": "rotated"}); code != http.StatusCreated {
		t.Fatalf("rotated-in key provisioning = %d, want 201", code)
	}
}

// TestConfiguredAdminKeyWinsOverEnvironment pins the priority order: an
// explicit Config.AdminKey (embedders, tests) is authoritative and is not
// shadowed by whatever KEEL_ADMIN_KEY happens to hold.
func TestConfiguredAdminKeyWinsOverEnvironment(t *testing.T) {
	t.Setenv("KEEL_ADMIN_KEY", "env-key")
	hs, st := newAdminTestServer(t, "pinned-key")
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "admin-pinned"); err != nil {
		t.Fatal(err)
	}
	configURL := hs.URL + "/v1/tenants/" + tenant.String() + "/config"
	if code := doAdminJSON(t, hs.Client(), http.MethodGet, configURL, "pinned-key", nil, nil); code != http.StatusOK {
		t.Fatalf("configured key = %d, want 200", code)
	}
	if code := doAdminJSON(t, hs.Client(), http.MethodGet, configURL, "env-key", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("environment key overrode Config.AdminKey: %d, want 401", code)
	}
	if code := doTenantJSON(t, hs.Client(), hs.URL+"/v1/tenants", uuid.NewString(), "pinned-key",
		map[string]string{"name": "pinned"}); code != http.StatusCreated {
		t.Fatalf("configured key provisioning = %d, want 201", code)
	}
}
