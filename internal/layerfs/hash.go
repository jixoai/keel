package layerfs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Hash is a lowercase hex sha256 digest. The zero value is not a valid object
// hash; use ParseHash or one returned by the store.
type Hash string

// HashPrefix is the display form a model sees and a caller may send back as a
// precondition token. Storage always uses the bare hex form.
const HashPrefix = "sha256:"

// ParseHash accepts a bare hex digest or the prefixed display form.
func ParseHash(value string) (Hash, error) {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, HashPrefix)
	trimmed = strings.ToLower(trimmed)
	if len(trimmed) != sha256.Size*2 {
		return "", fmt.Errorf("%w: hash %q must be %d hex characters", ErrInvalid, value, sha256.Size*2)
	}
	if _, err := hex.DecodeString(trimmed); err != nil {
		return "", fmt.Errorf("%w: hash %q is not hex", ErrInvalid, value)
	}
	return Hash(trimmed), nil
}

// String returns the bare hex form (the storage spelling).
func (h Hash) String() string { return string(h) }

// Prefixed returns the display form ("sha256:<hex>").
func (h Hash) Prefixed() string { return HashPrefix + string(h) }

// Valid reports whether h is a well-formed bare hex digest.
func (h Hash) Valid() bool {
	if len(h) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(string(h))
	return err == nil
}

// Bytes returns the raw digest.
func (h Hash) Bytes() ([]byte, error) {
	if !h.Valid() {
		return nil, fmt.Errorf("%w: hash %q", ErrInvalid, string(h))
	}
	return hex.DecodeString(string(h))
}

// HashBytes returns the canonical Hash of raw bytes.
func HashBytes(raw []byte) Hash {
	sum := sha256.Sum256(raw)
	return Hash(hex.EncodeToString(sum[:]))
}
