package httpapi

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// A response body is a place credentials must never reach. `writeErr` composes
// its body from an error that someone else built, so it masks credential-shaped
// substrings instead of trusting every future `fmt.Errorf` to be careful.
func TestWriteErrRedactsCredentials(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string // must not appear
	}{
		{"bearer", errors.New("upstream rejected Authorization: Bearer sk-live-abcdef123456"), "sk-live-abcdef123456"},
		{"api key assignment", errors.New(`provider failed: {"api_key":"sk-live-777777"}`), "sk-live-777777"},
		{"query token", errors.New("GET https://vendor.test/v1?access_token=tok-123456 failed"), "tok-123456"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeErr(rec, 502, tc.err)
			body := rec.Body.String()
			if strings.Contains(body, tc.want) {
				t.Fatalf("response body leaked %q: %s", tc.want, body)
			}
			if !strings.Contains(body, "error") {
				t.Fatalf("response body has no error field: %s", body)
			}
		})
	}
}
