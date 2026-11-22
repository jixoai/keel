package auth

import (
	"errors"
	"strings"
	"testing"
)

// fastParams keep the table tests quick; the default-parameter test below is
// the one that pins the deployment constants.
var fastParams = PasswordParams{Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

func TestDefaultPasswordParamsMatchDesign(t *testing.T) {
	if DefaultPasswordParams.Memory != 64*1024 ||
		DefaultPasswordParams.Iterations != 3 ||
		DefaultPasswordParams.Parallelism != 2 ||
		DefaultPasswordParams.SaltLength != 16 ||
		DefaultPasswordParams.KeyLength != 32 {
		t.Fatalf("default params drifted from design §2.2: %+v", DefaultPasswordParams)
	}
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("hash = %q", hash)
	}
	params, err := ParsePasswordHash(hash)
	if err != nil {
		t.Fatal(err)
	}
	if params != DefaultPasswordParams {
		t.Fatalf("parsed params = %+v", params)
	}
	if NeedsRehash(hash) {
		t.Fatal("a fresh default hash needs no rehash")
	}
	if err := VerifyPassword(hash, "correct horse battery staple"); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestPasswordRoundTripAndMismatch(t *testing.T) {
	hash, err := HashPasswordWithParams("s3cret-passphrase", fastParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword(hash, "s3cret-passphrase"); err != nil {
		t.Fatalf("correct password: %v", err)
	}
	if err := VerifyPassword(hash, "s3cret-passphrasE"); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("wrong password err = %v, want ErrPasswordMismatch", err)
	}
	// Every account gets its own salt: the same password never produces the
	// same stored value.
	other, err := HashPasswordWithParams("s3cret-passphrase", fastParams)
	if err != nil {
		t.Fatal(err)
	}
	if other == hash {
		t.Fatal("two hashes of one password are identical (salt is not random)")
	}
}

func TestPasswordPolicy(t *testing.T) {
	cases := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"empty", "", true},
		{"one below minimum", strings.Repeat("a", MinPasswordChars-1), true},
		{"minimum", strings.Repeat("a", MinPasswordChars), false},
		{"maximum", strings.Repeat("a", MaxPasswordBytes), false},
		{"one above maximum", strings.Repeat("a", MaxPasswordBytes+1), true},
		{"multi-byte below minimum", strings.Repeat("密", MinPasswordChars-1), true},
		{"multi-byte minimum", strings.Repeat("密", MinPasswordChars), false},
		{"multi-byte within byte budget", strings.Repeat("密码", 100), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPasswordPolicy(tc.password)
			if tc.wantErr && !errors.Is(err, ErrPasswordPolicy) {
				t.Fatalf("err = %v, want ErrPasswordPolicy", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestHashPasswordRejectsInvalidInput(t *testing.T) {
	if _, err := HashPassword(""); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("empty hash err = %v", err)
	}
	if _, err := HashPassword(strings.Repeat("x", MaxPasswordBytes+1)); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("long hash err = %v", err)
	}
	if _, err := HashPasswordWithParams("long-enough-password", PasswordParams{
		Memory: 1, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	}); !errors.Is(err, ErrInvalidPasswordHash) {
		t.Fatalf("too-small memory err = %v", err)
	}
	if _, err := HashPasswordWithParams("long-enough-password", PasswordParams{
		Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 4, KeyLength: 32,
	}); !errors.Is(err, ErrInvalidPasswordHash) {
		t.Fatalf("short salt err = %v", err)
	}
}

func TestVerifyMalformedHash(t *testing.T) {
	valid, err := HashPasswordWithParams("long-enough-password", fastParams)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(valid, "$")
	if len(parts) != 6 {
		t.Fatalf("unexpected hash shape %q", valid)
	}
	cases := map[string]string{
		"empty":             "",
		"plain text":        "not-a-hash",
		"wrong algorithm":   strings.Replace(valid, "$argon2id$", "$argon2i$", 1),
		"wrong version":     strings.Replace(valid, "$v=19$", "$v=16$", 1),
		"trailing field":    strings.Replace(valid, parts[3], parts[3]+",x=1", 1),
		"bad salt base64":   strings.Replace(valid, parts[4], "***", 1),
		"truncated key":     strings.Replace(valid, parts[5], parts[5][:8], 1),
		"missing key field": strings.Join(parts[:5], "$"),
		"too many fields":   valid + "$extra",
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			if err := VerifyPassword(encoded, "long-enough-password"); !errors.Is(err, ErrInvalidPasswordHash) {
				t.Fatalf("err = %v, want ErrInvalidPasswordHash", err)
			}
			if !NeedsRehash(encoded) {
				t.Fatal("a malformed hash must need rehash")
			}
		})
	}
	// A hash with an out-of-bounds cost must be refused before Argon2 runs.
	over := strings.Replace(valid, "m=8192", "m=2097152", 1)
	if err := VerifyPassword(over, "long-enough-password"); !errors.Is(err, ErrInvalidPasswordHash) {
		t.Fatalf("over-budget hash err = %v", err)
	}
}

func TestNeedsRehashDetectsParameterDrift(t *testing.T) {
	current, err := HashPasswordWithParams("long-enough-password", fastParams)
	if err != nil {
		t.Fatal(err)
	}
	if !NeedsRehash(current) {
		t.Fatal("fast test params differ from the defaults and must need rehash")
	}
	shaLike := "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5MzI"
	if NeedsRehash(shaLike) {
		t.Fatal("a hash with the default parameters must not need rehash")
	}
	weaker := strings.Replace(shaLike, "m=65536,t=3,p=2", "m=32768,t=3,p=2", 1)
	if !NeedsRehash(weaker) {
		t.Fatal("a weaker hash must need rehash")
	}
}
