package boardintegration

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/auth"

	"github.com/jixoai/keel-board/protocol"
)

func TestBotProfileTokenIsStableAndDistinct(t *testing.T) {
	first := BotProfileToken("researcher")
	if first != BotProfileToken("researcher") {
		t.Fatal("BotProfileToken is not stable")
	}
	if first == BotProfileToken("researcher-2") {
		t.Fatal("two bot ids produced one profile token")
	}
	if !protocol.ValidProfile(first) {
		t.Fatalf("profile token %q is not a valid board profile", first)
	}
	// Renaming the bot must not change the token: the input is the ID only.
	if BotProfileToken("researcher") == BotProfileToken("Renamed Researcher") {
		t.Fatal("bot profile token depends on something other than the id")
	}
}

func TestHumanActorUsesAccountIdentity(t *testing.T) {
	tenant := uuid.New()
	account := uuid.New()
	request := httptest.NewRequest(http.MethodGet, "/v1/board/me", nil)
	request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: tenant, AccountID: account,
		Username: "alice", DisplayName: "Alice",
	}))
	actor, err := (&Identity{}).Actor(request)
	if err != nil {
		t.Fatalf("Actor: %v", err)
	}
	if actor.ID != AccountActorID(account) || actor.Principal != AccountActorID(account) {
		t.Fatalf("actor = %+v", actor)
	}
	if actor.Profile != nil || actor.Kind != protocol.ActorPerson {
		t.Fatalf("a human actor must be a person with no profile: %+v", actor)
	}
	if actor.Handle != "alice" || actor.Name != "Alice" {
		t.Fatalf("human display fields = %q/%q", actor.Handle, actor.Name)
	}
}

func TestActorRequiresEdgePrincipal(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/board/me", nil)
	_, err := (&Identity{}).Actor(request)
	var boardErr *protocol.Error
	if !errors.As(err, &boardErr) || boardErr.Code != protocol.CodeUnauthorized {
		t.Fatalf("Actor without a principal = %v, want unauthorized", err)
	}
}

func TestBotActorWithoutStoreFailsClosed(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/board/me", nil)
	request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{
		Kind: auth.PrincipalBot, TenantID: uuid.New(), AccountID: uuid.New(),
		BotID: "researcher", Username: "owner",
	}))
	_, err := (&Identity{}).Actor(request)
	var boardErr *protocol.Error
	if !errors.As(err, &boardErr) || boardErr.Code != protocol.CodeUnavailable {
		t.Fatalf("bot actor without a store = %v, want unavailable (fail closed)", err)
	}
}

func TestTenantComesFromThePrincipal(t *testing.T) {
	tenant := uuid.New()
	request := httptest.NewRequest(http.MethodGet, "/v1/board/me", nil)
	request.Header.Set("X-Tenant-ID", uuid.NewString())
	request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: tenant, AccountID: uuid.New(), Username: "alice",
	}))
	got, err := (&Identity{}).Tenant(request)
	if err != nil {
		t.Fatalf("Tenant: %v", err)
	}
	if got != tenant.String() {
		t.Fatalf("Tenant = %q, want %q (the header must be ignored)", got, tenant)
	}
}

func TestBoundedKeepsValidUTF8(t *testing.T) {
	value := "a" + string([]rune{'é'}) + "b"
	if got := bounded(value, 2); got != "a" {
		t.Fatalf("bounded(%q, 2) = %q", value, got)
	}
}
