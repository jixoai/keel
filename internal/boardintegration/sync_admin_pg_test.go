package boardintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/access"

	boardsync "github.com/jixoai/keel-board/sync"
)

// TestSyncEntryAdminRoutes covers task 10.6: POST/PATCH entry documents need
// board.manage + secret.write, accept secret NAMES only, write both keel's
// config row and the board sync store row, report credential status without a
// value, and fail closed (disabled/restart) on a structural change.
func TestSyncEntryAdminRoutes(t *testing.T) {
	env := newTestEnv(t, nil)
	server := env.server()
	ctx := context.Background()
	const secretValue = "xoxb-SUPER-SECRET-SHOULD-NEVER-APPEAR"

	body := map[string]any{
		"key":      "slack",
		"platform": "slack",
		"secretRef": map[string]string{
			"signingSecret": "SLACK_SIGNING",
			"botToken":      "SLACK_BOT_TOKEN",
		},
		"settings": map[string]any{
			"botUserId":  "U_APP",
			"botId":      "B_APP",
			"apiBaseUrl": "http://127.0.0.1:1/api",
		},
		"routes":        map[string]string{"C_SUPPORT": "support"},
		"startChannels": []string{"C_SUPPORT"},
		"enabled":       true,
	}

	// Board.manage without secret.write is refused.
	denied := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", body)
	if denied.status != 403 || !containsCode(denied.body, "forbidden") {
		t.Fatalf("manage without secret.write = %d %s", denied.status, denied.body)
	}
	// A reader/writer without board.manage is refused too.
	if resp := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "bob", body); resp.status != 403 {
		t.Fatalf("bob entry create = %d %s", resp.status, resp.body)
	}
	// Alice now holds both keys.
	env.grant(env.tenant, env.alice.record.ID, access.KeySecretWrite)

	// A plaintext credential field is rejected by the strict decoder before
	// anything is stored.
	leaky := map[string]any{"key": "slack2", "platform": "slack", "secret": secretValue}
	if resp := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", leaky); resp.status != 400 {
		t.Fatalf("plaintext secret field = %d %s", resp.status, resp.body)
	}
	// A value where a secret NAME belongs is rejected by the name grammar.
	alias := map[string]any{
		"key": "slack2", "platform": "slack",
		"secretRef": map[string]string{"signingSecret": secretValue},
	}
	if resp := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", alias); resp.status != 400 {
		t.Fatalf("secret value as ref = %d %s", resp.status, resp.body)
	}

	created := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", body)
	var result SyncEntryResult
	env.decode(t, created, &result)
	if !result.RestartRequired || !result.PendingRestart {
		t.Fatalf("create result = %+v", result)
	}
	if result.Entry.Credential != CredentialMissing {
		t.Fatalf("create credential = %q, want missing", result.Entry.Credential)
	}
	if strings.Contains(string(created.body), secretValue) {
		t.Fatalf("create response leaked a secret: %s", created.body)
	}

	// Both stores carry the entry. The board store is the one the board's own
	// GET entries/status routes read.
	config, found, err := env.st.GetBoardSyncEntry(ctx, env.tenant, "slack")
	if err != nil || !found {
		t.Fatalf("keel entry = %+v/%v/%v", config, found, err)
	}
	if config.Platform != "slack" || config.SecretRef["signingSecret"] != "SLACK_SIGNING" {
		t.Fatalf("stored entry = %+v", config)
	}
	boardEntry, found, err := env.surf.SyncStore().Entry(ctx, env.tenant.String(), boardsync.Key("slack"))
	if err != nil || !found || !boardEntry.Enabled {
		t.Fatalf("board sync entry = %+v/%v/%v", boardEntry, found, err)
	}
	if resp := env.do(t, server, http.MethodPost, "/v1/board/sync/entries", "alice", body); resp.status != 409 {
		t.Fatalf("duplicate create = %d %s", resp.status, resp.body)
	}
	// A PATCH names the deployment tenant; another tenant's manager gets 404
	// and never reaches this tenant's row.
	if resp := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "dave", map[string]any{"enabled": false}); resp.status != 404 {
		t.Fatalf("cross-tenant PATCH = %d %s", resp.status, resp.body)
	}

	// Credential status: missing -> configured -> invalid, never the value.
	if resp := env.do(t, server, http.MethodGet, "/v1/board/sync/credentials", "alice", nil); len(resp.body) == 0 || !strings.Contains(string(resp.body), `"credential":"missing"`) {
		t.Fatalf("missing credential status = %d %s", resp.status, resp.body)
	}
	if _, err := env.st.PutTenantSecret(ctx, env.tenant, "SLACK_SIGNING", []byte("signing-secret-value"), env.box); err != nil {
		t.Fatalf("put signing secret: %v", err)
	}
	if _, err := env.st.PutTenantSecret(ctx, env.tenant, "SLACK_BOT_TOKEN", []byte(secretValue), env.box); err != nil {
		t.Fatalf("put bot token: %v", err)
	}
	statusResp := env.do(t, server, http.MethodGet, "/v1/board/sync/credentials", "alice", nil)
	if !strings.Contains(string(statusResp.body), `"credential":"configured"`) {
		t.Fatalf("configured credential status = %d %s", statusResp.status, statusResp.body)
	}
	if strings.Contains(string(statusResp.body), secretValue) || strings.Contains(string(statusResp.body), "signing-secret-value") {
		t.Fatalf("credential status leaked a value: %s", statusResp.body)
	}
	// Corrupt the ciphertext: the status must be invalid, not configured.
	if _, err := env.st.Pool().Exec(ctx,
		`UPDATE tenant_secrets SET ciphertext = '\x00'::bytea WHERE tenant_id = $1 AND name = 'SLACK_SIGNING'`,
		env.tenant); err != nil {
		t.Fatalf("corrupt secret: %v", err)
	}
	if resp := env.do(t, server, http.MethodGet, "/v1/board/sync/credentials", "alice", nil); !strings.Contains(string(resp.body), `"credential":"invalid"`) {
		t.Fatalf("invalid credential status = %d %s", resp.status, resp.body)
	}

	// Metric exposes the status, not the value.
	var exposition bytes.Buffer
	if err := env.surf.Metrics().WritePrometheus(&exposition); err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if !strings.Contains(exposition.String(), `board_sync_credential_status{key="slack",status="invalid"} 1`) {
		t.Fatalf("credential metric missing:\n%s", exposition.String())
	}
	if strings.Contains(exposition.String(), secretValue) {
		t.Fatal("metrics leaked a secret")
	}

	// Disable is immediate at the board sync row (webhook stops accepting).
	disabled := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": false})
	env.decode(t, disabled, &result)
	if result.Entry.Enabled || result.RestartRequired {
		t.Fatalf("disable result = %+v", result)
	}
	if boardEntry, _, _ = env.surf.SyncStore().Entry(ctx, env.tenant.String(), boardsync.Key("slack")); boardEntry.Enabled {
		t.Fatal("disable left the board sync entry enabled")
	}
	// Enable of an entry this process never loaded asks for a restart and stays
	// disabled until then, so an old/absent provider cannot half-serve.
	enabled := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": true})
	env.decode(t, enabled, &result)
	if !result.RestartRequired {
		t.Fatalf("enable result = %+v, want restartRequired", result)
	}
	if boardEntry, _, _ = env.surf.SyncStore().Entry(ctx, env.tenant.String(), boardsync.Key("slack")); boardEntry.Enabled {
		t.Fatal("unloaded enable should stay disabled until restart")
	}

	// A structural change (rotation) is persisted, restart-required, and fails
	// closed at the board sync row.
	rotated := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{
		"secretRef": map[string]string{"signingSecret": "SLACK_SIGNING_V2", "botToken": "SLACK_BOT_TOKEN"},
	})
	env.decode(t, rotated, &result)
	if !result.RestartRequired || result.Entry.Credential != CredentialMissing {
		t.Fatalf("rotation result = %+v", result)
	}
	if boardEntry, _, _ = env.surf.SyncStore().Entry(ctx, env.tenant.String(), boardsync.Key("slack")); boardEntry.Enabled {
		t.Fatal("rotation left the old provider's entry enabled")
	}
	// Re-enabling a pending rotation must not fall back to the old provider.
	reenable := env.do(t, server, http.MethodPatch, "/v1/board/sync/entries/slack", "alice", map[string]any{"enabled": true})
	env.decode(t, reenable, &result)
	if !result.RestartRequired {
		t.Fatalf("re-enable after rotation = %+v, want restartRequired", result)
	}
	if boardEntry, _, _ = env.surf.SyncStore().Entry(ctx, env.tenant.String(), boardsync.Key("slack")); boardEntry.Enabled {
		t.Fatal("re-enable after rotation revived the old provider")
	}
	if _, err := env.st.PutTenantSecret(ctx, env.tenant, "SLACK_SIGNING_V2", []byte("new-signing"), env.box); err != nil {
		t.Fatalf("put rotated signing secret: %v", err)
	}
	// Audit rows name the entry/status only; no value and no ciphertext.
	events, err := env.surf.AuditStore().Recent(ctx, env.tenant.String(), 20)
	if err != nil {
		t.Fatalf("audit recent: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("entry lifecycle wrote no audit row")
	}
	for _, event := range events {
		detail, _ := json.Marshal(event.Detail)
		if strings.Contains(string(detail), secretValue) || strings.Contains(string(detail), "new-signing") {
			t.Fatalf("audit detail leaked a credential: %s", detail)
		}
	}
}
