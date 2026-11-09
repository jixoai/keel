package servicehost

// The settings write path (change keel-model-settings): a models document that
// passes ValidateModelsDocument must serve new runs after ApplyModels, keep the
// in-flight generation untouched, and go back to the deployment document on a
// reset; a rejected reload must keep the previous generation serving.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/store"
)

// documentWithSelection rewrites the shared test document's default selection.
func documentWithSelection(t *testing.T, providerID, modelID, level string) json.RawMessage {
	t.Helper()
	selection := `{"providerId": "` + providerID + `", "modelId": "` + modelID + `"`
	if level != "" {
		selection += `, "reasoningLevel": "` + level + `"`
	}
	selection += `}`
	document := strings.Replace(modelsDocument,
		`"selection": {"providerId": "tenant-gw", "modelId": "deepseek-v4.1-flash", "reasoningLevel": "high"}`,
		`"selection": `+selection, 1)
	if !strings.Contains(document, `"providerId": "`+providerID+`"`) {
		t.Fatalf("selection rewrite failed for %s/%s", providerID, modelID)
	}
	return json.RawMessage(document)
}

// TestModelsDocumentWritePathServesNewRuns is the process-level R3 gate: a
// validated document applies to the next run without a restart, an in-flight
// run keeps the generation it pinned, and a reset returns to the deployment
// document.
func TestModelsDocumentWritePathServesNewRuns(t *testing.T) {
	ctx := context.Background()
	rt := bootWithLLM(t,
		host.TenantConfig{Models: json.RawMessage(modelsDocument)},
		testLLM(map[string]string{"TENANT_GW_KEY": "gw-key", "ANTHROPIC_API_KEY": "anthropic-key"}, "flat-key"))
	t.Cleanup(func() { rt.Close(context.Background()) })

	bootRevision := rt.Snapshot().Revision
	// The deployment document serves tenant-gw/deepseek-v4.1-flash at high.
	if got := rt.ProviderName() + "/" + rt.Model(); got != "tenant-gw/deepseek-v4.1-flash" {
		t.Fatalf("deployment identity = %s", got)
	}
	inflight, err := rt.Binding().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire in-flight: %v", err)
	}
	defer inflight.Release(ctx)
	oldActive, err := inflight.Runner().Models.Resolve(ctx, kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("in-flight resolve: %v", err)
	}
	if oldActive.ProviderID != "tenant-gw" || oldActive.APIKey != "gw-key" || oldActive.ThinkingLevel != "high" {
		t.Fatalf("in-flight identity = %s/%s %q", oldActive.ProviderID, oldActive.Model, oldActive.ThinkingLevel)
	}

	// The candidate document is validated through the same construction the
	// generation runs; the summary reports what it would serve.
	next := documentWithSelection(t, "anthropic", "claude-sonnet-4-5", "low")
	candidate, issues, err := rt.ValidateModelsDocument(next)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("a valid document must not report issues: %+v", issues)
	}
	if candidate.Selection.ProviderID != "anthropic" || candidate.Selection.ModelID != "claude-sonnet-4-5" ||
		candidate.Selection.ReasoningLevel != "low" {
		t.Fatalf("candidate selection = %+v", candidate.Selection)
	}
	// The summary counts what this process can actually serve: at least the
	// document's two credentialed providers (hidden-gw is not part of the
	// resolution at all).
	if candidate.ProvidersExecutable < 2 || candidate.ModelsSelectable < 2 {
		t.Fatalf("candidate summary = %d providers / %d models, want at least the document's two",
			candidate.ProvidersExecutable, candidate.ModelsSelectable)
	}

	snap, err := rt.ApplyModels(ctx, next)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if snap.Revision != bootRevision+1 {
		t.Fatalf("apply revision = %d, want the boot revision + 1 (%d)", snap.Revision, bootRevision+1)
	}
	if got := rt.ProviderName() + "/" + rt.Model(); got != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("identity after apply = %s, want the new document's", got)
	}
	// The candidate summary describes exactly the generation that got applied.
	applied := rt.Models().(*modelsService)
	if candidate.ProvidersExecutable != applied.ProvidersExecutable() ||
		candidate.ModelsSelectable != applied.ModelsSelectable() {
		t.Fatalf("candidate summary (%d/%d) differs from the applied generation (%d/%d)",
			candidate.ProvidersExecutable, candidate.ModelsSelectable,
			applied.ProvidersExecutable(), applied.ModelsSelectable())
	}

	// A new run resolves the new identity, with the new credential.
	fresh, err := rt.Binding().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire after apply: %v", err)
	}
	defer fresh.Release(ctx)
	newActive, err := fresh.Runner().Models.Resolve(ctx, kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("new resolve: %v", err)
	}
	if newActive.ProviderID != "anthropic" || newActive.Model != "claude-sonnet-4-5" {
		t.Fatalf("new identity = %s/%s", newActive.ProviderID, newActive.Model)
	}
	if newActive.APIKey != "anthropic-key" || newActive.ThinkingLevel != "low" || newActive.Stream == nil {
		t.Fatalf("new attempt composition = key %q level %q stream %v",
			newActive.APIKey, newActive.ThinkingLevel, newActive.Stream != nil)
	}

	// The in-flight run keeps its pinned generation: same model, same key.
	pinned, err := inflight.Runner().Models.Resolve(ctx, kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("pinned resolve: %v", err)
	}
	if pinned.Model != oldActive.Model || pinned.APIKey != oldActive.APIKey {
		t.Fatalf("the in-flight generation changed under the run: %s/%s", pinned.ProviderID, pinned.Model)
	}

	// Resetting returns to the deployment document.
	reset, err := rt.ApplyModels(ctx, nil)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if reset.Revision != bootRevision+2 {
		t.Fatalf("reset revision = %d, want the boot revision + 2 (%d)", reset.Revision, bootRevision+2)
	}
	afterReset, err := rt.Binding().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire after reset: %v", err)
	}
	defer afterReset.Release(ctx)
	resetActive, err := afterReset.Runner().Models.Resolve(ctx, kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("reset resolve: %v", err)
	}
	if resetActive.ProviderID != "tenant-gw" || resetActive.APIKey != "gw-key" {
		t.Fatalf("reset identity = %s/%s, want the deployment document's", resetActive.ProviderID, resetActive.Model)
	}
	if rt.DeploymentModels() == nil {
		t.Fatal("the deployment document must stay readable after a reset")
	}
}

// TestModelsDocumentValidationRejections covers the write-path gate: unknown
// identities, unexecutable providers and a document that clears the default
// selection all come back as field-path issues, and nothing is applied.
func TestModelsDocumentValidationRejections(t *testing.T) {
	ctx := context.Background()
	rt := bootWithLLM(t,
		host.TenantConfig{Models: json.RawMessage(modelsDocument)},
		testLLM(map[string]string{"TENANT_GW_KEY": "gw-key"}, "flat-key"))
	t.Cleanup(func() { rt.Close(context.Background()) })
	before := rt.Snapshot()

	cases := []struct {
		name      string
		document  string
		wantField string
	}{
		{
			name:      "unknown model",
			document:  string(documentWithSelection(t, "tenant-gw", "no-such-model", "high")),
			wantField: "selection.modelId",
		},
		{
			name:      "unknown provider",
			document:  `{"selection":{"providerId":"nope","modelId":"deepseek-v4.1-flash"}}`,
			wantField: "selection.providerId",
		},
		{
			name:      "no default selection",
			document:  `{"providers":[{"id":"tenant-gw","modelIds":["deepseek-v4.1-flash"]}]}`,
			wantField: "selection.modelId",
		},
		{
			name: "incomplete provider",
			document: `{"providers":[{"id":"broken","api":{"type":"openai-chat-completions"},
				"access":{"type":"api-key","env":"BROKEN_KEY"},"modelIds":["m1"]}],
				"selection":{"providerId":"broken","modelId":"m1"}}`,
			wantField: "selection",
		},
		{
			name:      "unsupported api type in the patch",
			document:  `{"providers":[{"id":"broken","api":{"type":"carrier-pigeon"},"modelIds":["m1"]}]}`,
			wantField: "document",
		},
		{
			name:      "unknown reasoning level",
			document:  string(documentWithSelection(t, "tenant-gw", "deepseek-v4.1-flash", "maximum")),
			wantField: "selection.reasoningLevel",
		},
		{
			name:      "document shape",
			document:  `{"providers":"not-an-array"}`,
			wantField: "document",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, issues, err := rt.ValidateModelsDocument(json.RawMessage(tc.document))
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			if len(issues) == 0 {
				t.Fatal("the document must be refused with issues")
			}
			if !strings.HasPrefix(issues[0].Field, tc.wantField) {
				t.Fatalf("first issue field = %q, want prefix %q (issues %+v)", issues[0].Field, tc.wantField, issues)
			}
			for _, issue := range issues {
				if strings.Contains(issue.Message, "gw-key") || strings.Contains(issue.Message, "flat-key") {
					t.Fatalf("an issue leaked a credential value: %+v", issue)
				}
			}
		})
	}
	// No rejected validation may have touched the runtime.
	if after := rt.Snapshot(); after.Revision != before.Revision || after.Generation != before.Generation {
		t.Fatalf("a rejected validation changed the generation: before %d/%d after %d/%d",
			before.Revision, before.Generation, after.Revision, after.Generation)
	}
	if rt.Models() == nil {
		t.Fatal("the serving generation must stay intact")
	}
	active, err := rt.Models().Resolve(ctx, kernel.Scope{}, nil)
	if err != nil || active.ProviderID != "tenant-gw" {
		t.Fatalf("the deployment identity must keep resolving: %+v (%v)", active, err)
	}
}

// TestModelsDocumentValidationReportsCredentialFieldPaths: a credential the
// process cannot resolve is reported where the operator edits it
// (providers[id].access.*), not as a generic document failure. A variable that
// is set but empty never falls back to the process key, so this is a real
// refusal even when a flat key exists.
func TestModelsDocumentValidationReportsCredentialFieldPaths(t *testing.T) {
	llm := testLLM(map[string]string{"TENANT_GW_KEY": "gw-key", "EMPTY_KEY": ""}, "flat-key")
	rt := bootWithLLM(t, host.TenantConfig{Models: json.RawMessage(modelsDocument)}, llm)
	t.Cleanup(func() { rt.Close(context.Background()) })

	document := json.RawMessage(`{"providers":[{"id":"nokey",
		"api":{"type":"openai-chat-completions","baseUrl":"https://nokey.example.invalid/v1"},
		"access":{"type":"api-key","env":"EMPTY_KEY"},"modelIds":["m1"]}],
		"selection":{"providerId":"nokey","modelId":"m1"}}`)
	_, issues, err := rt.ValidateModelsDocument(document)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(issues) == 0 {
		t.Fatal("an unresolvable credential must be refused")
	}
	if issues[0].Field != "providers[nokey].access.env" {
		t.Fatalf("credential issue field = %q, want providers[nokey].access.env (issues %+v)", issues[0].Field, issues)
	}
	if !strings.Contains(issues[0].Message, "EMPTY_KEY") {
		t.Fatalf("the issue must name the variable: %+v", issues[0])
	}
}

// TestRejectedReloadKeepsTheServingGeneration is R1's "failure does not adopt"
// gate: a reload whose new generation cannot activate is rejected AND the
// runtime that was serving keeps serving (the engine had already adopted the
// broken graph, so the rejection must roll back).
func TestRejectedReloadKeepsTheServingGeneration(t *testing.T) {
	ctx := context.Background()
	rt := bootWithLLM(t,
		host.TenantConfig{Models: json.RawMessage(modelsDocument)},
		testLLM(map[string]string{"TENANT_GW_KEY": "gw-key"}, "flat-key"))
	t.Cleanup(func() { rt.Close(context.Background()) })
	before := rt.Snapshot()

	broken := host.TenantConfig{Models: json.RawMessage(`{"providers":[{"id":"broken","api":{"type":"carrier-pigeon"},
		"access":{"type":"api-key","env":"BROKEN_KEY"},"modelIds":["m1"]}],
		"selection":{"providerId":"broken","modelId":"m1"}}`)}
	if _, err := rt.Reload(ctx, broken, loadout.Options{Revision: 9}); err == nil {
		t.Fatal("a document that cannot activate must be rejected")
	}
	if rt.Models() == nil {
		t.Fatal("the previous generation must keep serving after a rejected reload")
	}
	active, err := rt.Models().Resolve(ctx, kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("resolve after rejection: %v", err)
	}
	if active.ProviderID != "tenant-gw" || active.APIKey != "gw-key" {
		t.Fatalf("identity after rejection = %s/%s", active.ProviderID, active.Model)
	}
	if got := rt.ProviderName() + "/" + rt.Model(); got != "tenant-gw/deepseek-v4.1-flash" {
		t.Fatalf("advertised identity after rejection = %s", got)
	}
	if after := rt.Snapshot(); after.Revision != before.Revision {
		t.Fatalf("a rejected reload must not move the revision: %d -> %d", before.Revision, after.Revision)
	}
	fresh, err := rt.Binding().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire after rejection: %v", err)
	}
	defer fresh.Release(ctx)
	if fresh.Runner().Models == nil {
		t.Fatal("a new run must still resolve a model service")
	}
	resolved, err := fresh.Runner().Models.Resolve(ctx, kernel.Scope{}, nil)
	if err != nil || resolved.ProviderID != "tenant-gw" {
		t.Fatalf("a new run after rejection = %+v (%v)", resolved, err)
	}
}

// TestStoredSecretResolvesAnAttempt is the K3/K4 integration gate: with the
// real store and a real SecretBox, `access.ref: secret:NAME` resolves the
// attempt's credential from the tenant secret store, and without one the same
// document fails closed naming the secret — never falling back to the
// environment that happens to hold the name.
func TestStoredSecretResolvesAnAttempt(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	box, err := store.NewSecretBox(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	tenant := uuid.New()
	const plaintext = "stored-gateway-key"
	if _, err := st.PutTenantSecret(ctx, tenant, "gw_key", []byte(plaintext), box); err != nil {
		t.Fatalf("put secret: %v", err)
	}

	document := json.RawMessage(`{"providers":[{"id":"store-gw",
		"api":{"type":"openai-chat-completions","baseUrl":"https://gw.example.invalid/v1"},
		"access":{"type":"api-key","ref":"secret:gw_key"},"modelIds":["m1"]}],
		"selection":{"providerId":"store-gw","modelId":"m1"}}`)

	// The hosting process's lookup closure: decrypt the tenant's secret.
	lookup := func(name string) (string, bool) {
		value, found, err := st.GetTenantSecret(ctx, tenant, name, box)
		if err != nil || !found || len(value) == 0 {
			return "", false
		}
		return string(value), true
	}
	llm := testLLM(map[string]string{"gw_key": "environment-decoy"}, "flat-key")
	llm.LookupSecret = lookup
	rt := bootWithLLM(t, host.TenantConfig{Models: document}, llm)
	t.Cleanup(func() { rt.Close(context.Background()) })

	active, err := rt.Models().Resolve(ctx, kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if active.APIKey != plaintext {
		t.Fatalf("APIKey = %q, want the stored secret (the environment value must not be used)", active.APIKey)
	}

	// Without the secret store the same document is refused, naming the secret
	// and never leaking the value or the environment decoy. (The runtime below
	// boots on the shared document, which needs no secret, and is then asked to
	// validate the secret-bearing one.)
	noStore := bootWithLLM(t, host.TenantConfig{Models: json.RawMessage(modelsDocument)},
		testLLM(map[string]string{"gw_key": "environment-decoy"}, "flat-key"))
	t.Cleanup(func() { noStore.Close(context.Background()) })
	if noStore.Models() == nil {
		t.Fatal("the deployment generation could not be assembled")
	}
	_, rejected, err := noStore.ValidateModelsDocument(document)
	if err != nil {
		t.Fatalf("validate without a secret store: %v", err)
	}
	if len(rejected) == 0 {
		t.Fatal("a secret: reference without a secret store must be refused")
	}
	rendered, _ := json.Marshal(rejected)
	if !strings.Contains(string(rendered), "gw_key") || !strings.Contains(string(rendered), "KEEL_SECRET_KEY") {
		t.Fatalf("the refusal must name the secret and the missing store: %s", rendered)
	}
	if strings.Contains(string(rendered), plaintext) || strings.Contains(string(rendered), "environment-decoy") {
		t.Fatalf("the refusal leaked a credential: %s", rendered)
	}
}

// TestBootFallsBackToTheDeploymentDocumentWhenTheOverrideCannotBeServed pins
// the anti-brick rule: a stored override whose credentials no longer resolve
// (a deleted secret, an unset environment variable) must not take the
// deployment down on the next boot. The deployment document serves, the boot
// says exactly which override it refused, and the settings read can name it so
// the operator fixes or deletes the row.
func TestBootFallsBackToTheDeploymentDocumentWhenTheOverrideCannotBeServed(t *testing.T) {
	deployment := json.RawMessage(`{
		"providers":[{"id":"dep-gw","api":{"type":"openai-chat-completions","baseUrl":"https://dep.example.invalid/v1"},
			"access":{"type":"api-key","env":"DEP_KEY"},"modelIds":["deepseek-v4.1-flash"]}],
		"selection":{"providerId":"dep-gw","modelId":"deepseek-v4.1-flash"}}`)
	// The stored override references a tenant secret this process cannot
	// resolve: LookupSecret is nil, which is exactly the "no secret store /
	// nothing stored" state that used to fail boot.
	override := json.RawMessage(`{
		"providers":[{"id":"stored-gw","api":{"type":"openai-chat-completions","baseUrl":"https://stored.example.invalid/v1"},
			"access":{"type":"api-key","ref":"secret:GONE_KEY"},"modelIds":["deepseek-v4.1-flash"]}],
		"selection":{"providerId":"stored-gw","modelId":"deepseek-v4.1-flash"}}`)

	st := kernel.NewMemStore()
	k := kernel.New(st, kernel.Options{})
	rt, err := Boot(context.Background(), Config{
		Seams:            Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:           host.TenantConfig{Models: override},
		LLM:              testLLM(map[string]string{"DEP_KEY": "env-value"}, ""),
		DeploymentModels: deployment,
		OverrideRevision: 9,
		RuntimeID:        "test-runtime",
	})
	if err != nil {
		t.Fatalf("a stored override that cannot be served must fall back, not fail boot: %v", err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })

	rejected, ok := rt.RejectedModelsOverride()
	if !ok {
		t.Fatal("the fallback must be reported")
	}
	if rejected.Revision != 9 {
		t.Fatalf("rejected revision = %d, want 9", rejected.Revision)
	}
	if !strings.Contains(rejected.Reason, "GONE_KEY") {
		t.Fatalf("the reason must name the credential that vanished: %q", rejected.Reason)
	}
	// What is serving is the deployment document, not the refused override.
	service := rt.Models()
	if service == nil {
		t.Fatal("the deployment document must serve after the fallback")
	}
	if got := service.DefaultSelection(); got.ProviderID != "dep-gw" {
		t.Fatalf("serving provider = %q, want the deployment one (dep-gw)", got.ProviderID)
	}
}

// TestBootKeepsFailingWhenTheDeploymentDocumentItselfIsBroken pins the other
// half: the fallback is only for the stored override — a deployment document
// this process cannot serve is still fatal, because it is the deployment's own
// file.
func TestBootKeepsFailingWhenTheDeploymentDocumentItselfIsBroken(t *testing.T) {
	broken := json.RawMessage(`{"providers":[{"id":"x","api":{"type":"carrier-pigeon"}}]}`)
	st := kernel.NewMemStore()
	k := kernel.New(st, kernel.Options{})
	rt, err := Boot(context.Background(), Config{
		Seams:            Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:           host.TenantConfig{Models: broken},
		DeploymentModels: broken,
		RuntimeID:        "test-runtime",
	})
	if err == nil {
		rt.Close(context.Background())
		t.Fatal("a broken deployment document must still fail boot")
	}
	if !strings.Contains(err.Error(), "api.type") {
		t.Fatalf("boot error must carry the resolver's diagnostic: %v", err)
	}
}
