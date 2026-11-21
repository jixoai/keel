package store

// Bot registry reads that the provider settings surface depends on (A4.2b3a):
// provider deletion protection now scans bots.default_model, not the retired
// agents/agent_teams documents.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestProvidersReferencingProviderScansBots(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	account, _, profile, _ := identityFixture(t, st)
	tenant := account.TenantID
	model := func(provider string) json.RawMessage {
		raw, _ := json.Marshal(map[string]string{"providerId": provider, "modelId": "m"})
		return raw
	}
	harness := json.RawMessage(`{"tools":["bash"]}`)
	for _, bot := range []struct {
		id       string
		provider string
		archived bool
	}{
		{"bot-a-" + tenant.String()[:8], "prov_ref", false},
		{"bot-b-" + tenant.String()[:8], "prov_ref", true},
		{"bot-c-" + tenant.String()[:8], "prov_other", false},
	} {
		if _, err := st.CreateBot(ctx, BotRecord{
			TenantID: tenant, ID: bot.id, Name: "Bot " + bot.id, ProfileID: profile.ID,
			ModelKind: "text", DefaultModel: model(bot.provider), Harness: harness,
			Archived: bot.archived,
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM bots WHERE tenant_id = $1`, tenant)
	})

	references, err := st.ProvidersReferencingProvider(ctx, tenant, "prov_ref")
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 2 {
		t.Fatalf("references = %+v, want two (an archived bot still counts)", references)
	}
	for _, reference := range references {
		if reference.Kind != "bot" || reference.Field != "defaultModel" || reference.Name == "" {
			t.Fatalf("reference = %+v", reference)
		}
	}
	if other, err := st.ProvidersReferencingProvider(ctx, tenant, "prov_other"); err != nil || len(other) != 1 {
		t.Fatalf("other references = %+v err=%v", other, err)
	}
	if foreign, err := st.ProvidersReferencingProvider(ctx, uuid.New(), "prov_ref"); err != nil || len(foreign) != 0 {
		t.Fatalf("foreign references = %+v err=%v", foreign, err)
	}
}
