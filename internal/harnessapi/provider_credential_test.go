package harnessapi

// The provider editor's direct credential entry (change keel-model-catalog A):
// PUT /v1/settings/providers/{id}/credential stores the pasted value in the
// encrypted tenant secret store under a stable name and answers only the
// reference plus a masked preview. The models document is never written by this
// route, so a literal credential can never enter model configuration.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// TestProviderCredentialDirectEntry pins the happy path: the value lands under
// provider_<id>, the answer is the reference and the masked preview, and the
// response body carries neither the value nor any other guessable fact.
func TestProviderCredentialDirectEntry(t *testing.T) {
	secrets := newFakeSecretStore()
	server := settingsServer(t, func(cfg *Config) { cfg.SecretStore = secrets })
	client := server.Client()
	tenant := uuid.New()

	const value = "sk-live-0123456789-abcdefghijklmnop"
	var response providerCredentialResponse
	var raw json.RawMessage
	code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/providers/s2api/credential", tenant,
		map[string]string{"value": value}, &raw)
	if code != http.StatusOK {
		t.Fatalf("put credential = %d %s", code, raw)
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.ProviderID != "s2api" || response.SecretName != "provider_s2api" || response.SecretRef != "secret:provider_s2api" {
		t.Fatalf("answer = %+v", response)
	}
	wantPreview := store.SecretPreview([]byte(value))
	if response.Preview != wantPreview {
		t.Fatalf("preview = %q, want %q", response.Preview, wantPreview)
	}
	if response.UpdatedAt.IsZero() {
		t.Fatal("the answer must carry the write timestamp")
	}
	// The value is in the store exactly once, under the derived name.
	if stored := secrets.values[tenant]["provider_s2api"]; string(stored) != value {
		t.Fatal("the value was not stored under its stable name")
	}
	// No response byte may carry the value, a length, a hash or a bare prefix.
	if strings.Contains(string(raw), value) {
		t.Fatalf("the response leaked the value: %s", raw)
	}
	for _, field := range []string{`"length"`, `"hash"`, `"prefix"`, `"suffix"`, `"value"`} {
		if strings.Contains(string(raw), field) {
			t.Fatalf("the response carries %s: %s", field, raw)
		}
	}

	// The secrets list shows the same masked preview, not the value.
	var list secretsResponse
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/secrets", tenant, nil, &list); code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	if len(list.Secrets) != 1 || list.Secrets[0].Name != "provider_s2api" {
		t.Fatalf("list = %+v", list.Secrets)
	}
	if list.Secrets[0].Preview != wantPreview {
		t.Fatalf("list preview = %q, want %q", list.Secrets[0].Preview, wantPreview)
	}
}

// TestProviderCredentialShortValueMask: a credential at or below the boundary
// gets the fixed mask (no prefix/suffix), and the route says so in its preview.
func TestProviderCredentialShortValueMask(t *testing.T) {
	secrets := newFakeSecretStore()
	server := settingsServer(t, func(cfg *Config) { cfg.SecretStore = secrets })
	client := server.Client()
	tenant := uuid.New()

	const value = "shortkey"
	var response providerCredentialResponse
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/providers/gw/credential", tenant,
		map[string]string{"value": value}, &response); code != http.StatusOK {
		t.Fatalf("put = %d", code)
	}
	if response.Preview != store.MaskedSecretPreview {
		t.Fatalf("short value preview = %q, want the fixed mask", response.Preview)
	}
	if strings.Contains(response.Preview, "short") {
		t.Fatalf("a short credential leaked a prefix: %q", response.Preview)
	}
}

// TestProviderCredentialRefusals: a blank value (the "must not erase" rule),
// the tenant gate, an unwired store, a bad provider id, and the name grammar
// are all refused with the right status and without a value echo.
func TestProviderCredentialRefusals(t *testing.T) {
	secrets := newFakeSecretStore()
	server := settingsServer(t, func(cfg *Config) { cfg.SecretStore = secrets })
	client := server.Client()
	tenant := uuid.New()

	// A blank field never erases a stored credential: the route refuses it, and
	// the stored value stays.
	const value = "sk-live-existing-credential-xx"
	var written providerCredentialResponse
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/providers/s2api/credential", tenant,
		map[string]string{"value": value}, &written); code != http.StatusOK {
		t.Fatalf("seed put = %d", code)
	}
	var body map[string]any
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/providers/s2api/credential", tenant,
		map[string]string{"value": "  "}, &body); code != http.StatusBadRequest {
		t.Fatalf("blank value = %d %v, want 400", code, body)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "never erases") {
		t.Fatalf("blank refusal = %v, want it to state the no-erase rule", body)
	}
	if got := secrets.values[tenant]["provider_s2api"]; string(got) != value {
		t.Fatal("a blank write changed the stored credential")
	}

	// A missing tenant is a 401 before the store is reached.
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/providers/s2api/credential", uuid.Nil,
		map[string]string{"value": "v"}, &body); code != http.StatusUnauthorized {
		t.Fatalf("no tenant = %d, want 401", code)
	}
	// Unknown body keys are refused (decodeBody's strict decoder).
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/providers/s2api/credential", tenant,
		map[string]string{"value": "v", "apiKey": "x"}, &body); code != http.StatusBadRequest {
		t.Fatalf("unknown key = %d, want 400", code)
	}
	// A provider id outside the direct-credential route's own grammar is
	// refused before a secret name is derived.
	if code := doSettingsJSON(t, client, http.MethodPut,
		server.URL+"/v1/settings/providers/"+url.PathEscape("bad id")+"/credential", tenant,
		map[string]string{"value": "v"}, &body); code != http.StatusBadRequest {
		t.Fatalf("bad provider id = %d, want 400", code)
	}
	// An invalid provider id maps to the path's id segment.
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/providers/a%20b/credential", tenant,
		map[string]string{"value": "v"}, nil); code != http.StatusBadRequest {
		t.Fatalf("space in provider id = %d, want 400", code)
	}

	// A deployment without a secret store answers 503 with the operator reason.
	bare := settingsServer(t, nil)
	if code := doSettingsJSON(t, bare.Client(), http.MethodPut, bare.URL+"/v1/settings/providers/s2api/credential", tenant,
		map[string]string{"value": "v"}, &body); code != http.StatusServiceUnavailable {
		t.Fatalf("unwired store = %d, want 503", code)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "KEEL_SECRET_KEY") {
		t.Fatalf("503 must explain the missing configuration: %v", body)
	}
}

// TestProviderCredentialNameIsStableAndGrammatical: a provider id with
// characters the secret-name grammar refuses is sanitized with a digest suffix,
// a re-paste overwrites the same row, and the derived name is one a models
// document can reference.
func TestProviderCredentialNameIsStableAndGrammatical(t *testing.T) {
	secrets := newFakeSecretStore()
	server := settingsServer(t, func(cfg *Config) { cfg.SecretStore = secrets })
	client := server.Client()
	tenant := uuid.New()

	const wantName = "provider_local_gw_b75ac66d"
	var first providerCredentialResponse
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/providers/local-gw/credential", tenant,
		map[string]string{"value": "first-credential-value-0123"}, &first); code != http.StatusOK {
		t.Fatalf("put = %d", code)
	}
	if first.SecretName != wantName {
		t.Fatalf("derived name = %q, want %s", first.SecretName, wantName)
	}
	var second providerCredentialResponse
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/providers/local-gw/credential", tenant,
		map[string]string{"value": "second-credential-value-4567"}, &second); code != http.StatusOK {
		t.Fatalf("re-paste = %d", code)
	}
	if second.SecretName != first.SecretName {
		t.Fatalf("a re-paste changed the secret name: %q -> %q", first.SecretName, second.SecretName)
	}
	if rows := len(secrets.values[tenant]); rows != 1 {
		t.Fatalf("a re-paste accumulated %d rows, want the one stable row", rows)
	}
	if got := secrets.values[tenant][wantName]; string(got) != "second-credential-value-4567" {
		t.Fatal("the re-paste did not overwrite the value")
	}
	// The derived name is legal for the reference grammar (the write route
	// enforces exactly the grammar a document can reference).
	if !secretNamePattern.MatchString(first.SecretName) {
		t.Fatalf("derived name %q is not referencable", first.SecretName)
	}
}

// TestProviderCredentialNamesDoNotCollideAfterSanitization: `a.b` and `a-b`
// sanitize to the same fragment, so each gets its own digest suffix; a re-paste
// of one rotates only that row. An id the grammar already accepts keeps the
// bare name.
func TestProviderCredentialNamesDoNotCollideAfterSanitization(t *testing.T) {
	secrets := newFakeSecretStore()
	server := settingsServer(t, func(cfg *Config) { cfg.SecretStore = secrets })
	client := server.Client()
	tenant := uuid.New()

	put := func(id, value string) providerCredentialResponse {
		t.Helper()
		var response providerCredentialResponse
		if code := doSettingsJSON(t, client, http.MethodPut,
			server.URL+"/v1/settings/providers/"+id+"/credential", tenant,
			map[string]string{"value": value}, &response); code != http.StatusOK {
			t.Fatalf("put %q = %d", id, code)
		}
		return response
	}
	dotted := put("a.b", "credential-for-dotted-0123")
	dashed := put("a-b", "credential-for-dashed-4567")
	if dotted.SecretName != "provider_a_b_108bf50c" {
		t.Fatalf("a.b name = %q", dotted.SecretName)
	}
	if dashed.SecretName != "provider_a_b_2a89df63" {
		t.Fatalf("a-b name = %q", dashed.SecretName)
	}
	if dotted.SecretName == dashed.SecretName {
		t.Fatalf("sanitization collided two providers onto %q", dotted.SecretName)
	}
	if rows := len(secrets.values[tenant]); rows != 2 {
		t.Fatalf("stored %d rows, want one per provider", rows)
	}
	// Rotating one credential leaves the other provider's value alone.
	rotated := put("a.b", "credential-for-dotted-rotated")
	if rotated.SecretName != dotted.SecretName {
		t.Fatalf("a re-paste changed the name: %q -> %q", dotted.SecretName, rotated.SecretName)
	}
	if got := secrets.values[tenant][dashed.SecretName]; string(got) != "credential-for-dashed-4567" {
		t.Fatal("rotating a.b changed a-b's credential")
	}

	plain := put("s2api", "credential-for-plain-7890")
	if plain.SecretName != "provider_s2api" {
		t.Fatalf("an unsanitized id changed its name: %q", plain.SecretName)
	}
	// Every derived name stays inside the reference grammar's bound, including
	// an id at the route grammar's 128-character maximum (truncated + digested).
	longID := "p." + strings.Repeat("x", 126)
	long := put(longID, "credential-for-long-id-0123")
	if len(long.SecretName) > maxSecretNameLength {
		t.Fatalf("long id name %q is %d chars, want <= %d", long.SecretName, len(long.SecretName), maxSecretNameLength)
	}
	if !secretNamePattern.MatchString(long.SecretName) {
		t.Fatalf("long id name %q is not referencable", long.SecretName)
	}
	if long.SecretName != providerCredentialSecretName(longID) {
		t.Fatalf("long id name = %q, derivation = %q", long.SecretName, providerCredentialSecretName(longID))
	}
}

// TestProviderCredentialNeverReachesTheDocumentOrLogs pins the two remaining
// leak surfaces of the direct-entry path: the stored models document is not
// written by the route, and neither a success nor a strict-body refusal puts the
// value in the server's log.
func TestProviderCredentialNeverReachesTheDocumentOrLogs(t *testing.T) {
	const value = "sk-live-0123456789-should-never-be-logged"
	var logs bytes.Buffer
	secrets := newFakeSecretStore()
	models := newFakeModelSettingsStore()
	tenant := uuid.New()
	document := json.RawMessage(`{"schemaVersion":1,"providers":[` +
		`{"id":"s2api","api":{"type":"openai-chat-completions","baseUrl":"https://api.example/v1"},` +
		`"access":{"type":"api-key","ref":"secret:provider_s2api"},"modelIds":["deepseek-v4.1-flash"]}]}`)
	if _, err := models.SetModelSettings(context.Background(), tenant, 0, document); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	server := settingsServer(t, func(cfg *Config) {
		cfg.SecretStore = secrets
		cfg.ModelSettingsStore = models
		cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})
	client := server.Client()
	// The happy path.
	if code := doSettingsJSON(t, client, http.MethodPut,
		server.URL+"/v1/settings/providers/s2api/credential", tenant,
		map[string]string{"value": value}, nil); code != http.StatusOK {
		t.Fatalf("put = %d, want 200", code)
	}
	// A refused body still carries the value into decodeBody; its error must not
	// echo it, and nothing may be logged.
	var refusal map[string]any
	if code := doSettingsJSON(t, client, http.MethodPut,
		server.URL+"/v1/settings/providers/s2api/credential", tenant,
		map[string]string{"value": value, "apiKey": "extra"}, &refusal); code != http.StatusBadRequest {
		t.Fatalf("strict-body refusal = %d, want 400", code)
	}
	if message, _ := refusal["error"].(string); strings.Contains(message, value) {
		t.Fatalf("the refusal echoed the value: %q", message)
	}
	if strings.Contains(logs.String(), value) {
		t.Fatalf("the value reached the server log: %s", logs.String())
	}
	// The models document is untouched (the client puts the reference in it
	// through PUT /v1/settings/models, never this route).
	stored, _, _, found, err := models.GetModelSettings(context.Background(), tenant)
	if err != nil || !found {
		t.Fatalf("read document: found=%v err=%v", found, err)
	}
	if string(stored) != string(document) {
		t.Fatalf("the credential route changed the models document:\ngot  %s\nwant %s", stored, document)
	}
}
