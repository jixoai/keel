package agentapi

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// The product API's error body is a place a credential must never appear. Its
// text is composed elsewhere, so the masking happens at this one exit.
func TestWriteErrRedactsCredentials(t *testing.T) {
	cases := []struct{ name, want string; err error }{
		{"bearer", "sk-live-abcdef123456", errors.New("upstream: Authorization: Bearer sk-live-abcdef123456")},
		{"api key", "sk-live-777777", errors.New(`provider: {"api_key":"sk-live-777777"}`)},
		{"query token", "tok-123456", errors.New("GET https://vendor.test/v1?access_token=tok-123456 failed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeErr(rec, 502, tc.err)
			if body := rec.Body.String(); strings.Contains(body, tc.want) {
				t.Fatalf("error body leaked %q: %s", tc.want, body)
			}
		})
	}
}
