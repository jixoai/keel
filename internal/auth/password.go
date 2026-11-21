// Password hashing for account login (change keel-bot-identity-access, A2).
//
// Passwords are stored as an Argon2id PHC string:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>
//
// The parameters are code constants (design §2.2): memory 64 MiB, iterations 3,
// parallelism 2, a 16-byte random salt per account and a 32-byte key. They are
// raised by a new change, never by reading a value from the database: a stored
// hash's parameters are only used to verify it, and NeedsRehash reports when a
// verified hash should be upgraded to the current constants.
//
// # What this file does not do
//
// It does not decide policy (length, complexity, rotation) and it does not
// authenticate an account: callers treat every verification failure the same
// way (unknown account, wrong password, malformed hash) and must never render
// the reason. VerifyPassword returns distinct sentinels internally so tests can
// assert the distinction; the HTTP layer maps them all to one 401.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Password policy bounds. They are deliberately generous: the point is to keep
// a request from turning into an unbounded Argon2 allocation, not to impose a
// composition rule (the modern guidance is length over character classes).
const (
	// MinPasswordBytes is the shortest accepted password (12 bytes). Setup,
	// login-time change and CLI reset all use it.
	MinPasswordBytes = 12
	// MaxPasswordBytes is the longest accepted password. It bounds the memory a
	// single verification may touch and matches common proxy header limits.
	MaxPasswordBytes = 1024
)

// Hash parameter bounds accepted from a stored PHC string. They are wider than
// the defaults (an upgraded future hash still verifies) but bounded so a
// tampered row cannot make the server allocate arbitrary memory.
const (
	minArgonMemoryKiB = 8 * 1024
	maxArgonMemoryKiB = 1024 * 1024 // 1 GiB
	minArgonTime      = 1
	maxArgonTime      = 32
	minArgonThreads   = 1
	maxArgonThreads   = 64
	minSaltLength     = 8
	maxSaltLength     = 64
	minKeyLength      = 16
	maxKeyLength      = 64
)

// Password hashing sentinels. None of them carries the password or the hash.
var (
	// ErrPasswordMismatch means the password does not match the stored hash.
	ErrPasswordMismatch = errors.New("auth: password does not match")
	// ErrInvalidPasswordHash means the stored value is not a supported PHC
	// string. A caller treats it as a failed login; NeedsRehash treats it as
	// "must be replaced".
	ErrInvalidPasswordHash = errors.New("auth: invalid password hash")
	// ErrPasswordPolicy means a candidate password is outside the accepted
	// length bounds. It is a caller/programming error, not a login outcome.
	ErrPasswordPolicy = errors.New("auth: password does not meet the length policy")
)

// PasswordParams is the tunable half of an Argon2id hash.
type PasswordParams struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultPasswordParams are the deployment constants from design §2.2:
// 64 MiB, 3 passes, 2 lanes, 16-byte salt, 32-byte key.
var DefaultPasswordParams = PasswordParams{
	Memory:      64 * 1024,
	Iterations:  3,
	Parallelism: 2,
	SaltLength:  16,
	KeyLength:   32,
}

func (p PasswordParams) validate() error {
	switch {
	case p.Memory < minArgonMemoryKiB || p.Memory > maxArgonMemoryKiB:
		return fmt.Errorf("%w: memory %d KiB outside [%d, %d]", ErrInvalidPasswordHash, p.Memory, minArgonMemoryKiB, maxArgonMemoryKiB)
	case p.Iterations < minArgonTime || p.Iterations > maxArgonTime:
		return fmt.Errorf("%w: iterations %d outside [%d, %d]", ErrInvalidPasswordHash, p.Iterations, minArgonTime, maxArgonTime)
	case p.Parallelism < minArgonThreads || p.Parallelism > maxArgonThreads:
		return fmt.Errorf("%w: parallelism %d outside [%d, %d]", ErrInvalidPasswordHash, p.Parallelism, minArgonThreads, maxArgonThreads)
	case p.SaltLength < minSaltLength || p.SaltLength > maxSaltLength:
		return fmt.Errorf("%w: salt length %d outside [%d, %d]", ErrInvalidPasswordHash, p.SaltLength, minSaltLength, maxSaltLength)
	case p.KeyLength < minKeyLength || p.KeyLength > maxKeyLength:
		return fmt.Errorf("%w: key length %d outside [%d, %d]", ErrInvalidPasswordHash, p.KeyLength, minKeyLength, maxKeyLength)
	}
	return nil
}

// argon2Slots bounds concurrent Argon2 work per process. Each hash is 64MiB;
// without a bound, N concurrent login/bootstrap requests turn into N×64MiB
// regardless of the per-username failure limiter. This is process-local (a
// multi-replica deployment still needs a shared edge limiter); it exists to
// make a single instance's memory ceiling predictable.
var argon2Slots = make(chan struct{}, 4)

func acquireArgon2Slot() { argon2Slots <- struct{}{} }
func releaseArgon2Slot() { <-argon2Slots }

// HashPassword hashes one password with the current deployment parameters.
// Callers must run it on a password that passed CheckPasswordPolicy; the CLI,
// setup and password-change paths all call that first.
func HashPassword(password string) (string, error) {
	return HashPasswordWithParams(password, DefaultPasswordParams)
}

// HashPasswordWithParams is HashPassword with explicit parameters. Tests use it
// to prove that a hash's own parameters round-trip; production always uses the
// defaults.
func HashPasswordWithParams(password string, params PasswordParams) (string, error) {
	if err := CheckPasswordPolicy(password); err != nil {
		return "", err
	}
	if err := params.validate(); err != nil {
		return "", err
	}
	salt := make([]byte, params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate password salt: %w", err)
	}
	acquireArgon2Slot()
	key := argon2.IDKey([]byte(password), salt, params.Iterations, params.Memory, params.Parallelism, params.KeyLength)
	releaseArgon2Slot()
	return encodePasswordHash(params, salt, key), nil
}

// CheckPasswordPolicy validates a candidate password's length in bytes. It is
// the single policy call site for setup, change and CLI reset; it deliberately
// does not impose character classes.
func CheckPasswordPolicy(password string) error {
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes {
		return fmt.Errorf("%w: must be %d..%d bytes", ErrPasswordPolicy, MinPasswordBytes, MaxPasswordBytes)
	}
	return nil
}

// VerifyPassword checks a password against a stored PHC string. It returns nil
// on a match, ErrPasswordMismatch on a mismatch and ErrInvalidPasswordHash when
// the stored value cannot be parsed. The comparison is constant time; the
// Argon2 work depends only on the stored parameters, never on whether the
// password was close.
func VerifyPassword(encoded, password string) error {
	params, salt, want, err := parsePasswordHash(encoded)
	if err != nil {
		return err
	}
	acquireArgon2Slot()
	got := argon2.IDKey([]byte(password), salt, params.Iterations, params.Memory, params.Parallelism, uint32(len(want)))
	releaseArgon2Slot()
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

// NeedsRehash reports whether a stored hash should be replaced after a
// successful verification: either it is malformed/unsupported, or its
// parameters differ from the current deployment constants. Verification must
// have succeeded first; a malformed value is "needs rehash" so the next login
// can repair it rather than locking the account out forever.
func NeedsRehash(encoded string) bool {
	params, _, _, err := parsePasswordHash(encoded)
	if err != nil {
		return true
	}
	return params != DefaultPasswordParams
}

// ParsePasswordHash exposes the stored parameters (tests and diagnostics).
func ParsePasswordHash(encoded string) (PasswordParams, error) {
	params, _, _, err := parsePasswordHash(encoded)
	return params, err
}

func encodePasswordHash(params PasswordParams, salt, key []byte) string {
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, params.Memory, params.Iterations, params.Parallelism,
		enc.EncodeToString(salt), enc.EncodeToString(key))
}

func parsePasswordHash(encoded string) (PasswordParams, []byte, []byte, error) {
	var zero PasswordParams
	if !strings.HasPrefix(encoded, "$argon2id$") {
		return zero, nil, nil, fmt.Errorf("%w: not an argon2id PHC string", ErrInvalidPasswordHash)
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return zero, nil, nil, fmt.Errorf("%w: malformed PHC fields", ErrInvalidPasswordHash)
	}
	if parts[1] != "argon2id" {
		return zero, nil, nil, fmt.Errorf("%w: algorithm %q", ErrInvalidPasswordHash, parts[1])
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version ||
		parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return zero, nil, nil, fmt.Errorf("%w: unsupported version %q", ErrInvalidPasswordHash, parts[2])
	}
	var params PasswordParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.Memory, &params.Iterations, &params.Parallelism); err != nil {
		return zero, nil, nil, fmt.Errorf("%w: malformed parameters", ErrInvalidPasswordHash)
	}
	// Sscanf ignores trailing input; the parameter field must be exactly the
	// three-tuple this package writes, so no unknown cost parameter can hide
	// behind a valid prefix.
	if parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", params.Memory, params.Iterations, params.Parallelism) {
		return zero, nil, nil, fmt.Errorf("%w: malformed parameters %q", ErrInvalidPasswordHash, parts[3])
	}
	salt, err := decodePHCBase64(parts[4])
	if err != nil {
		return zero, nil, nil, fmt.Errorf("%w: salt: %v", ErrInvalidPasswordHash, err)
	}
	key, err := decodePHCBase64(parts[5])
	if err != nil {
		return zero, nil, nil, fmt.Errorf("%w: key: %v", ErrInvalidPasswordHash, err)
	}
	params.SaltLength = uint32(len(salt))
	params.KeyLength = uint32(len(key))
	if err := params.validate(); err != nil {
		return zero, nil, nil, err
	}
	return params, salt, key, nil
}

// decodePHCBase64 accepts the unpadded encoding this package writes and the
// padded form other Argon2 implementations commonly emit.
func decodePHCBase64(value string) ([]byte, error) {
	if decoded, err := base64.RawStdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.StdEncoding.DecodeString(value)
}
