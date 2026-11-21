package auth

import (
	"strings"
	"testing"
)

func TestMintBotKeyFormat(t *testing.T) {
	first, err := MintBotKey()
	if err != nil {
		t.Fatal(err)
	}
	second, err := MintBotKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.Plaintext, BotKeyScheme) {
		t.Fatalf("plaintext = %q", first.Plaintext)
	}
	if len(first.Prefix) != BotKeyPrefixLength || !strings.HasPrefix(first.Plaintext, first.Prefix) {
		t.Fatalf("prefix = %q (len %d)", first.Prefix, len(first.Prefix))
	}
	if len(first.Hash) != 32 {
		t.Fatalf("hash length = %d", len(first.Hash))
	}
	if first.Plaintext == second.Plaintext || string(first.Hash) == string(second.Hash) {
		t.Fatal("two minted keys collide")
	}
	if !LooksLikeBotKey(first.Plaintext) {
		t.Fatal("a minted key does not look like one")
	}
	if got := HashBotKey(first.Plaintext); string(got) != string(first.Hash) {
		t.Fatal("HashBotKey disagrees with the minted hash")
	}
}

func TestLooksLikeBotKeyRejectsForeignShapes(t *testing.T) {
	minted, _ := MintBotKey()
	cases := []string{
		"",
		"keel_bot_",
		"keel_bot_short",
		minted.Plaintext[:len(minted.Plaintext)-1],
		minted.Plaintext + "x",
		"keel_tenant_abcdefgh_" + strings.Repeat("A", 43),
		"keel_admin_abcdefgh_" + strings.Repeat("A", 43),
		"bearer " + minted.Plaintext,
	}
	for _, value := range cases {
		if LooksLikeBotKey(value) {
			t.Fatalf("foreign credential accepted: %q", value)
		}
	}
	if !LooksLikeBotKey("  " + minted.Plaintext + "  ") {
		t.Fatal("surrounding whitespace was not trimmed")
	}
}
