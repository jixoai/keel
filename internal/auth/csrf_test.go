package auth

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func testSecret() []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = byte(i + 1)
	}
	return out
}

func TestSessionCookieRoundTrip(t *testing.T) {
	cookie, err := NewSessionCookie(testSecret())
	if err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.New()
	value := cookie.Sign(sessionID)
	if !strings.HasPrefix(value, "v1.") {
		t.Fatalf("cookie = %q", value)
	}
	got, err := cookie.Verify(value)
	if err != nil || got != sessionID {
		t.Fatalf("verify = %s err=%v", got, err)
	}

	// The retired format (base64 payload + "." + signature, no version) fails.
	if _, err := cookie.Verify("AAAA.BBBB"); err != ErrBadCookie {
		t.Fatalf("old format err = %v", err)
	}
	// Tampering with the id or signature fails.
	parts := strings.Split(value, ".")
	if _, err := cookie.Verify("v1." + parts[1] + ".AAAA"); err != ErrBadCookie {
		t.Fatalf("bad signature err = %v", err)
	}
	tamperedPayload := "B" + parts[1][1:]
	if parts[1][0] == 'B' {
		tamperedPayload = "A" + parts[1][1:]
	}
	if _, err := cookie.Verify("v1." + tamperedPayload + "." + parts[2]); err != ErrBadCookie {
		t.Fatalf("tampered payload err = %v", err)
	}
	other, _ := NewSessionCookie([]byte(strings.Repeat("x", 32)))
	if _, err := other.Verify(value); err != ErrBadCookie {
		t.Fatalf("foreign key err = %v", err)
	}
	if _, err := NewSessionCookie([]byte("short")); err == nil {
		t.Fatal("short secret accepted")
	}
}

func TestCSRFTokenBoundToSubject(t *testing.T) {
	csrf, err := NewCSRF(testSecret())
	if err != nil {
		t.Fatal(err)
	}
	token, err := csrf.Issue("session-1")
	if err != nil {
		t.Fatal(err)
	}
	other, err := csrf.Issue("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if token == other {
		t.Fatal("two tokens are identical")
	}
	if !csrf.Verify("session-1", token) {
		t.Fatal("token did not verify for its subject")
	}
	if csrf.Verify("session-2", token) {
		t.Fatal("token verified for another subject")
	}
	if csrf.Verify("session-1", token+"x") {
		t.Fatal("tampered token verified")
	}
	if csrf.Verify("session-1", "") {
		t.Fatal("empty token verified")
	}
	if !Equal(token, token) || Equal(token, other) {
		t.Fatal("constant-time equality is wrong")
	}
	if Equal("a", "ab") {
		t.Fatal("different lengths compared equal")
	}
}

func TestCSRFAndCookieKeysAreIndependent(t *testing.T) {
	csrf, _ := NewCSRF(testSecret())
	cookie, _ := NewSessionCookie(testSecret())
	sessionID := uuid.New()
	cookieValue := cookie.Sign(sessionID)
	if csrf.Verify(sessionID.String(), cookieValue) {
		t.Fatal("a session cookie verified as a csrf token")
	}
	token, _ := csrf.Issue(sessionID.String())
	if _, err := cookie.Verify(token); err == nil {
		t.Fatal("a csrf token verified as a session cookie")
	}
}
