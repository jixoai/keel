// Keel security checks that PI's client-side flow does not need: server-side
// redaction of request credentials in logged errors.

package oauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpoauth "github.com/jixoai/keel/internal/mcp/oauth"
)

func TestTokenErrorsRedactSecrets(t *testing.T) {
	const secret = "super-secret-client-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A misbehaving server echoes the request credentials in its error.
		body := readBody(r)
		response := map[string]any{
			"error":             "invalid_client",
			"error_description": "rejected " + body,
		}
		raw, _ := json.Marshal(response)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write(raw)
	}))
	defer server.Close()

	_, err := mcpoauth.ExchangeAuthorizationCode(context.Background(), server.URL, mcpoauth.TokenRequestOptions{
		ClientInformation: mcpoauth.ClientInformation{ClientID: "client", ClientSecret: secret},
		HTTPClient:        testHTTPClient,
	}, "the-code", "the-verifier", "https://app.example/callback")
	if err == nil {
		t.Fatal("expected an error")
	}
	var oauthErr *mcpoauth.Error
	if !errors.As(err, &oauthErr) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(oauthErr.ResponseBody, secret) {
		t.Fatalf("error leaks the client secret: %v", err)
	}
	if strings.Contains(err.Error(), "the-verifier") || strings.Contains(oauthErr.ResponseBody, "the-verifier") {
		t.Fatalf("error leaks the code verifier: %v", err)
	}
}
