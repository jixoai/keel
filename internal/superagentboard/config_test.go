package superagentboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParseDocumentDefaultsToOff(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		mirror  bool
		bridge  bool
		channel string
	}{
		{name: "absent", raw: `{}`},
		{name: "empty", raw: ``},
		{name: "empty board", raw: `{"board":{}}`},
		{name: "empty superagent", raw: `{"board":{"superagent":{}}}`},
		{name: "mirror only", raw: `{"board":{"superagent":{"mirror":true}}}`, mirror: true},
		{name: "bridge only", raw: `{"board":{"superagent":{"bridge":true}}}`, bridge: true},
		{name: "both", raw: `{"board":{"superagent":{"mirror":true,"bridge":true}}}`, mirror: true, bridge: true},
		{name: "channel", raw: `{"board":{"superagent":{"channel":"work/agents"}}}`, channel: "work/agents"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config, err := ParseDocument(json.RawMessage(testCase.raw))
			if err != nil {
				t.Fatalf("ParseDocument: %v", err)
			}
			if config.Mirror != testCase.mirror || config.Bridge != testCase.bridge {
				t.Fatalf("switches = mirror:%v bridge:%v, want %v/%v",
					config.Mirror, config.Bridge, testCase.mirror, testCase.bridge)
			}
			wantChannel := testCase.channel
			if wantChannel == "" {
				wantChannel = DefaultChannel
			}
			if config.Channel != wantChannel {
				t.Fatalf("channel = %q, want %q", config.Channel, wantChannel)
			}
		})
	}
}

func TestParseDocumentRejectsBadChannel(t *testing.T) {
	if _, err := ParseDocument(json.RawMessage(`{"board":{"superagent":{"channel":"Not A Channel"}}}`)); err == nil {
		t.Fatal("want an invalid channel error")
	}
	if _, err := ParseDocument(json.RawMessage(`{"board":`)); err == nil {
		t.Fatal("want a JSON error")
	}
}

func TestStaticConfigReaderDefaultsChannel(t *testing.T) {
	reader := StaticConfigReader{Config: SuperagentConfig{Mirror: true}}
	config, err := reader.SuperagentConfig(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if config.Channel != DefaultChannel {
		t.Fatalf("channel = %q", config.Channel)
	}
}

func TestTokenSourceFailsClosedWithoutSecret(t *testing.T) {
	if _, err := NewTokenSource("   "); err != ErrSecretMissing {
		t.Fatalf("NewTokenSource blank = %v, want ErrSecretMissing", err)
	}
	if _, err := (*TokenSource)(nil).ClaimToken(uuid.New(), uuid.New(), uuid.New()); err != ErrSecretMissing {
		t.Fatalf("nil ClaimToken = %v, want ErrSecretMissing", err)
	}
}

func TestClaimTokenIsDeterministicAndBounded(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	assignment := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	session := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	tokens, err := NewTokenSource("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	first, err := tokens.ClaimToken(tenant, assignment, session)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tokens.ClaimToken(tenant, assignment, session)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("token is not deterministic: %q vs %q", first, second)
	}
	if len(first) != 43 {
		t.Fatalf("token length = %d, want 43", len(first))
	}
	if _, err := base64.RawURLEncoding.DecodeString(first); err != nil {
		t.Fatalf("token is not base64url: %v", err)
	}
	if strings.Contains(first, "test-secret") {
		t.Fatal("token leaks the secret")
	}
	other, err := tokens.ClaimToken(tenant, assignment, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("a different session produced the same token")
	}
	otherSecret, _ := NewTokenSource("another-secret")
	third, err := otherSecret.ClaimToken(tenant, assignment, session)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("a different secret produced the same token")
	}
}

func TestClaimTokenRequiresIDs(t *testing.T) {
	tokens, _ := NewTokenSource("s")
	if _, err := tokens.ClaimToken(uuid.Nil, uuid.New(), uuid.New()); err == nil {
		t.Fatal("want an error for a nil tenant")
	}
	if _, err := tokens.ClaimToken(uuid.New(), uuid.Nil, uuid.New()); err == nil {
		t.Fatal("want an error for a nil assignment")
	}
	if _, err := tokens.ClaimToken(uuid.New(), uuid.New(), uuid.Nil); err == nil {
		t.Fatal("want an error for a nil session")
	}
}
