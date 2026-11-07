// Redaction tests: the spec's 凭据与审计 scenario - logs show reference names or
// hashes, never plaintext.

package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{
		"password", "Password", "PASSWORD", "passwd", "secret", "client_secret",
		"api_key", "apiKey", "x-api-key", "token", "accessToken", "refresh_token",
		"authorization", "Authorization", "credential", "credentials", "cookie",
		"sessionKey", "private_key", "A6API_KEY",
	}
	for _, key := range sensitive {
		if !IsSensitiveKey(key) {
			t.Errorf("IsSensitiveKey(%q) = false, want true", key)
		}
	}
	benign := []string{"", "key", "monkey", "keyboard", "model", "path", "count", "user", "sessionId"}
	for _, key := range benign {
		if IsSensitiveKey(key) {
			t.Errorf("IsSensitiveKey(%q) = true, want false", key)
		}
	}
}

func TestFingerprintIsStableAndNotPlaintext(t *testing.T) {
	const secret = "sk-live-9f8e7d6c5b4a"
	first := Fingerprint(secret)
	if first != Fingerprint(secret) {
		t.Error("fingerprint is not stable")
	}
	if strings.Contains(first, secret) {
		t.Errorf("fingerprint %q leaks the secret", first)
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+12 {
		t.Errorf("fingerprint = %q, want sha256: plus 12 hex chars", first)
	}
	if Fingerprint("other") == first {
		t.Error("different secrets produced the same fingerprint")
	}
	if !strings.Contains(RedactSecret(secret), Fingerprint(secret)) {
		t.Error("RedactSecret must contain the fingerprint")
	}
}

func TestRedactValue(t *testing.T) {
	if got := RedactValue("api_key", "abc"); strings.Contains(got, "abc") {
		t.Errorf("RedactValue(api_key) = %q, want it masked", got)
	}
	if got := RedactValue("model", "gpt-5"); got != "gpt-5" {
		t.Errorf("RedactValue(model) = %q, want it untouched", got)
	}
}

func TestRedactJSONWalksNestedPayloads(t *testing.T) {
	input := `{
		"password": "hunter2",
		"note": "keep me",
		"nested": {"api_key": "k-123", "list": [{"token": "t-9", "keep": 1}], "empty": null, "meta": {"a": 1}},
		"count": 3,
		"secretObject": {"a": 1}
	}`
	output, err := RedactJSON([]byte(input))
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	text := string(output)
	for _, secret := range []string{"hunter2", "k-123", "t-9"} {
		if strings.Contains(text, secret) {
			t.Errorf("redacted JSON still contains %q: %s", secret, text)
		}
	}
	for _, kept := range []string{"keep me", "count", `"a":1`} {
		if !strings.Contains(text, kept) {
			t.Errorf("redacted JSON dropped %q: %s", kept, text)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("redacted JSON does not parse: %v", err)
	}
	if decoded["password"] == "hunter2" {
		t.Error("password survived redaction")
	}
	if _, ok := decoded["secretObject"].(string); !ok {
		t.Errorf("non-string value under a sensitive key = %T, want a fingerprint string", decoded["secretObject"])
	}
	nested := decoded["nested"].(map[string]any)
	if _, ok := nested["empty"]; !ok {
		t.Error("null under a sensitive key should stay present")
	}
	if nested["empty"] != nil {
		t.Errorf("null became %v", nested["empty"])
	}
	if meta, ok := nested["meta"].(map[string]any); !ok || meta["a"] != float64(1) {
		t.Errorf("benign nested value = %v, want it preserved", nested["meta"])
	}
}

func TestRedactText(t *testing.T) {
	const secret = "sk-live-0123456789"
	line := "provider call failed with key " + secret + " (again: " + secret + ")"
	redacted := RedactText(line, secret, "")
	if strings.Contains(redacted, secret) {
		t.Fatalf("redacted line still contains the secret: %s", redacted)
	}
	if !strings.Contains(redacted, RedactSecret(secret)) {
		t.Errorf("redacted line = %s, want the fingerprint", redacted)
	}
}

func TestRedactURL(t *testing.T) {
	const (
		password = "pass"
		token    = "abc123&region=cn&region=cn"
	)
	raw := "https://user:" + password + "@provider.example/mcp?token=" + token + "&region=cn"
	redacted := RedactURL(raw)
	if strings.Contains(redacted, password) || strings.Contains(redacted, token) {
		t.Fatalf("redacted URL leaks secrets: %s", redacted)
	}
	if !strings.Contains(redacted, "user") {
		t.Errorf("redacted URL = %s, want the username preserved", redacted)
	}
	if !strings.Contains(redacted, "region=cn") {
		t.Errorf("redacted URL = %s, want benign query parameters preserved", redacted)
	}
	if got := RedactURL("http://%zz"); got != "http://%zz" {
		t.Errorf("RedactURL(unparseable) = %q, want the input back", got)
	}
}

// TestDiagnosticsNeverCarryFrameBodies is the log check at the transport
// layer: a bad frame that contains a secret must not put it into diagnostics.
func TestDiagnosticsNeverCarryFrameBodies(t *testing.T) {
	const secret = "sk-live-9f8e7d6c5b4a"
	group := newToolFixture(t)
	group.provider.silence(MethodToolsCall)

	done := make(chan struct{})
	go func() {
		_, _ = group.tool.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"x"}`))
		close(done)
	}()
	request := group.provider.waitForRequest(MethodToolsCall)
	// A malformed frame carrying the secret, then the real response.
	group.provider.writeRaw([]byte(`{"jsonrpc":"2.0","id":1,"result":{"leak":"` + secret + `"` + "\n"))
	group.provider.respond(request.ID, map[string]any{"content": []any{}})
	<-done

	rendered := group.diags.all()
	if strings.Contains(rendered, secret) {
		t.Fatalf("diagnostics leaked the secret:\n%s", rendered)
	}
	if !group.diags.hasKind(DiagMessageInvalid) {
		t.Errorf("diagnostics = %s, want an invalid-message diagnostic", rendered)
	}
}
