package httpapi

// Assembly matrix (W0): the optional pieces of the production assembly are
// combined here so that no combination panics on a nil seam or silently opens
// an endpoint. Each feature also has its own focused test; this test is about
// the *cross-product* (admin key x metrics exporter x drain x OAuth config),
// which is what cmd/runtime actually assembles from the environment.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// textExporter satisfies MetricsExporter without pulling the metrics registry
// into this package's test.
type textExporter struct{ body string }

func (e textExporter) WriteText(_ context.Context, w io.Writer) error {
	_, err := io.WriteString(w, e.body)
	return err
}

func TestAssemblyMatrix(t *testing.T) {
	cases := []struct {
		name       string
		adminKey   string
		metrics    bool
		drain      bool
		wantAdmin  int // POST /v1/tenants without a bearer token
		wantMetric int // GET /metrics
		wantSignin int // POST /v1/mcp/servers/x/signin (OAuth unconfigured)
	}{
		{"everything off", "", false, false, http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{"admin only", "matrix-key", false, false, http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{"metrics only", "", true, false, http.StatusServiceUnavailable, http.StatusOK, http.StatusServiceUnavailable},
		{"drain only", "", false, true, http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{"admin+metrics", "matrix-key", true, false, http.StatusUnauthorized, http.StatusOK, http.StatusServiceUnavailable},
		{"admin+drain", "matrix-key", false, true, http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{"metrics+drain", "", true, true, http.StatusServiceUnavailable, http.StatusOK, http.StatusServiceUnavailable},
		{"all on", "matrix-key", true, true, http.StatusUnauthorized, http.StatusOK, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// The admin API falls back to the environment at request time, so a
			// developer shell with KEEL_ADMIN_KEY set would turn the "off"
			// cases into "on" ones.
			t.Setenv("KEEL_ADMIN_KEY", "")
			var drainer *GracefulDrainer
			hs, _ := newTestServer(t, func(c *Config) {
				c.AdminKey = tc.adminKey
				if tc.metrics {
					c.MetricsExporter = textExporter{body: "keel_matrix 1\n"}
				}
				if tc.drain {
					drainer = NewDrainer(1000, 1000)
					c.Drain = drainer
				}
			})
			hc := hs.Client()
			tenant := uuid.NewString()

			code := doJSON(t, hc, "POST", hs.URL+"/v1/tenants", tenant, map[string]string{"name": "m"}, nil)
			if code != tc.wantAdmin {
				t.Fatalf("POST /v1/tenants without a token = %d, want %d", code, tc.wantAdmin)
			}
			if tc.adminKey != "" {
				// The same request with the token succeeds: the 401 above is
				// authentication, not a broken route.
				req, _ := http.NewRequest("POST", hs.URL+"/v1/tenants",
					strings.NewReader(`{"name":"m"}`))
				req.Header.Set("X-Tenant-ID", tenant)
				req.Header.Set("Authorization", "Bearer "+tc.adminKey)
				req.Header.Set("Content-Type", "application/json")
				res, err := hc.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				if res.StatusCode != http.StatusCreated {
					t.Fatalf("POST /v1/tenants with the admin token = %d, want 201", res.StatusCode)
				}
			}

			res, err := hc.Get(hs.URL + "/metrics")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != tc.wantMetric {
				t.Fatalf("GET /metrics = %d, want %d", res.StatusCode, tc.wantMetric)
			}

			// The tenant header is what lets the request reach the OAuth config
			// check (without it the handler answers 401 before looking at the
			// configuration).
			signinReq, _ := http.NewRequest("POST", hs.URL+"/v1/mcp/servers/demo/signin",
				strings.NewReader(`{}`))
			signinReq.Header.Set("X-Tenant-ID", tenant)
			signinReq.Header.Set("Content-Type", "application/json")
			res, err = hc.Do(signinReq)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != tc.wantSignin {
				t.Fatalf("POST signin with OAuth unconfigured = %d, want %d", res.StatusCode, tc.wantSignin)
			}

			var sess struct {
				SessionID string `json:"sessionId"`
			}
			if drainer != nil {
				// Drain flips new API writes to 503 while the probes stay up.
				drainer.Begin()
				res, err = hc.Get(hs.URL + "/v1/health")
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				if res.StatusCode != http.StatusOK {
					t.Fatalf("GET /v1/health while draining = %d, want 200", res.StatusCode)
				}
				if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant, map[string]string{}, &sess); code != http.StatusServiceUnavailable {
					t.Fatalf("POST /v1/sessions while draining = %d, want 503", code)
				}
			} else {
				// No drainer: the same write is not refused.
				if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant, map[string]string{}, &sess); code != http.StatusCreated {
					t.Fatalf("POST /v1/sessions without a drainer = %d, want 201", code)
				}
			}
		})
	}
}
