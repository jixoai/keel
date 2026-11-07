// PKCE S256 (RFC 7636), the only challenge method MCP authorization servers
// may rely on. PI generates 32 random bytes, base64url-encodes them as the
// verifier, and sends the SHA-256 digest as the challenge.

package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// GeneratePKCE returns a fresh verifier and its S256 challenge.
func GeneratePKCE() (verifier string, challenge string, err error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(bytes)
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}
