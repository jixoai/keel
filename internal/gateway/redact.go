// Secret redaction for anything the gateway logs.
//
// The external-gateway spec is explicit: credentials are resolved from
// configuration references, logs show reference names or hashes, and complete
// parameters or message bodies are never logged. This file is the small toolkit
// the rest of the package (and its callers) use to keep that promise:
//
//   - Fingerprint replaces a secret with a stable, non-reversible-looking tag;
//   - RedactJSON walks an arbitrary JSON payload and masks the values of
//     sensitive fields (the common case: "log this tool argument object");
//   - RedactText replaces known secret literals wherever they appear;
//   - RedactURL masks userinfo and sensitive query parameters.
//
// A hash is still a derivative of the secret: for a low-entropy password it can
// be brute-forced. Fingerprints exist for correlation, not for safety; the
// safe move is to never log the value at all. The gateway itself never logs
// raw payloads - diagnostics carry sizes and messages, not bytes.

package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// redactedMarker is what a masked value looks like in logs.
const redactedMarker = "redacted:"

// Fingerprint is a stable short digest of a secret, suitable for logs and
// correlation ("the token ending 1a2b").
func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// RedactSecret renders a secret for logs: its fingerprint, never its value.
func RedactSecret(secret string) string {
	return redactedMarker + Fingerprint(secret)
}

// IsSensitiveKey reports whether a field name conventionally holds a secret.
// Matching is on the lowercase name with separators removed: exact names like
// password/token/apiKey/... plus any name containing password, secret, token,
// credential or apikey. Bare "key" is deliberately not matched, so "monkey"
// and "keyboard" survive; over-redaction costs little but noise does too.
func IsSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	if normalized == "" {
		return false
	}
	compact := strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', '.', ' ':
			return -1
		default:
			return r
		}
	}, normalized)
	switch compact {
	case "password", "passwd", "secret", "token", "apikey", "accesskey",
		"secretkey", "privatekey", "credential", "credentials", "authorization",
		"auth", "cookie", "sessionkey", "clientsecret", "refreshtoken",
		"accesstoken", "authtoken":
		return true
	}
	for _, part := range []string{"password", "secret", "token", "credential", "apikey"} {
		if strings.Contains(compact, part) {
			return true
		}
	}
	return false
}

// RedactValue renders one field for logs: sensitive fields become a
// fingerprint, everything else passes through.
func RedactValue(key, value string) string {
	if IsSensitiveKey(key) {
		return RedactSecret(value)
	}
	return value
}

// RedactJSON walks any JSON value and replaces the values of sensitive fields
// with their fingerprint, preserving structure. Non-string values under a
// sensitive key are fingerprinted through their JSON encoding; nulls stay null.
// The input is re-marshaled, so key order and number formatting may change -
// this is for logs, not for round-tripping data.
func RedactJSON(raw []byte) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("gateway: redact JSON: %w", err)
	}
	redacted := redactValue("", value)
	return json.Marshal(redacted)
}

func redactValue(key string, value any) any {
	if key != "" && IsSensitiveKey(key) && value != nil {
		if text, ok := value.(string); ok {
			return RedactSecret(text)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return RedactSecret(fmt.Sprint(value))
		}
		return RedactSecret(string(encoded))
	}
	switch typed := value.(type) {
	case map[string]any:
		for field, item := range typed {
			typed[field] = redactValue(field, item)
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = redactValue("", item)
		}
		return typed
	default:
		return value
	}
}

// RedactText replaces every occurrence of the given secret literals. Empty
// secrets are ignored (otherwise everything would be masked).
func RedactText(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, RedactSecret(secret))
	}
	return text
}

// RedactURL masks the password of a URL's userinfo and the values of sensitive
// query parameters.
func RedactURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if parsed.User != nil {
		username := parsed.User.Username()
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(username, RedactSecret("password"))
		}
	}
	query := parsed.Query()
	changed := false
	for key := range query {
		if !IsSensitiveKey(key) {
			continue
		}
		query.Set(key, RedactSecret(query.Get(key)))
		changed = true
	}
	if changed {
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}
