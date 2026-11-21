// Bot API credentials (change keel-bot-identity-access, A2, L18).
//
// A bot key is `keel_bot_<32 random bytes, base64url>`. It binds to one
// (account, bot) pair and is the only login method for a bot: bots never get a
// browser session. Only SHA-256(plaintext) and a short display prefix are
// stored; the plaintext appears once in the issue response and is never logged,
// echoed or recoverable.
//
// The prefix is a deliberate credential shape: a `Bearer keel_bot_...` or
// `X-API-Key: keel_bot_...` is unambiguously a bot key. A retired
// `keel_tenant_`/`keel_admin_` key is not accepted by this package, which is how
// the old api_keys path stays disconnected.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	// BotKeyScheme prefixes every bot credential.
	BotKeyScheme = "keel_bot_"
	// SetupTokenScheme prefixes a one-time first-super-admin setup token. It is
	// a different shape from a bot key so a log reader can tell them apart.
	SetupTokenScheme = "keel_setup_"
	// SetupTokenBytes is the entropy of a setup token.
	SetupTokenBytes = 32
	// BotKeyBytes is the entropy of a key (256 bits).
	BotKeyBytes = 32
	// BotKeyDisplayChars is how much of the random part a list may show.
	BotKeyDisplayChars = 8
	// BotKeyPrefixLength is the stored display prefix.
	BotKeyPrefixLength = len(BotKeyScheme) + BotKeyDisplayChars
)

// MintedBotKey is a freshly generated credential. Plaintext is returned to the
// issuer exactly once; nothing in this package retains it.
type MintedBotKey struct {
	Plaintext string
	Prefix    string
	Hash      []byte
}

// MintBotKey generates a new bot credential. It fails only when the system
// random source fails; there is no weak fallback.
func MintBotKey() (MintedBotKey, error) {
	buf := make([]byte, BotKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return MintedBotKey{}, fmt.Errorf("auth: generate bot key: %w", err)
	}
	plaintext := BotKeyScheme + base64.RawURLEncoding.EncodeToString(buf)
	return MintedBotKey{
		Plaintext: plaintext,
		Prefix:    plaintext[:BotKeyPrefixLength],
		Hash:      HashBotKey(plaintext),
	}, nil
}

// HashBotKey is the lookup identity of a plaintext bot key.
func HashBotKey(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// HashToken is SHA-256 over an arbitrary bearer value. Setup tokens use the
// same hash: only the digest is stored, never the plaintext.
func HashToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// MintSetupToken generates a one-time first-super-admin token and its stored
// hash. The plaintext is printed once by `keel admin bootstrap`; the database
// only ever sees the digest.
func MintSetupToken() (plaintext string, hash []byte, err error) {
	buf := make([]byte, SetupTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("auth: generate setup token: %w", err)
	}
	plaintext = SetupTokenScheme + base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, HashToken(plaintext), nil
}

// MintTemporaryPassword generates the one-time password `keel admin reset`
// prints. 18 bytes encode to 24 characters, comfortably above the length floor.
func MintTemporaryPassword() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate temporary password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// LooksLikeBotKey reports whether a credential is in this package's format. It
// is a pure string test: a truncated or foreign token must not be handed to a
// database lookup.
func LooksLikeBotKey(credential string) bool {
	credential = strings.TrimSpace(credential)
	if !strings.HasPrefix(credential, BotKeyScheme) {
		return false
	}
	rest := credential[len(BotKeyScheme):]
	if len(rest) < BotKeyDisplayChars {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(decoded) == BotKeyBytes
}
