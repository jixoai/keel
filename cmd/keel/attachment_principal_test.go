package main

// File-route principal isolation (change keel-plugin-attachment, 4.1): the
// preview's download and thumbnail URLs are ordinary blob routes, so they must
// read the authenticated principal's tenant and never the client's X-Tenant-ID.
// The auth middleware strips a forged header; this test pins the whole edge
// (middleware + handler): anonymous/forged requests are 401, a forged header on
// an authenticated request changes nothing, and another tenant's id is a 404.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
)

func fileRouteHandler(t *testing.T, st *store.Store) http.Handler {
	t.Helper()
	log := discardLogger()
	hub := httpapi.NewHub(st, log)
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)
	t.Cleanup(cancel)
	kernelLog, err := kernel.NewPG(context.Background(), st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	committer := kernel.New(kernelLog, kernel.Options{})
	return httpapi.New(httpapi.Config{
		Store:     st,
		Hub:       hub,
		Edge:      committer,
		Events:    committer,
		Blobs:     blob.NewReader(st, 4, 1),
		Uploader:  &blob.Uploader{Store: st, CASDir: t.TempDir()},
		Scheduler: stream.NewScheduler(16),
		Admitter:  stream.NewAdmitter(16, 32),
		Logger:    log,
	})
}

func TestFileRoutesUseThePrincipalTenant(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	tenantA := uuid.New()
	tenantB := uuid.New()
	var st *store.Store
	env := newAuthTestEnvWithProduct(t, authModeRequired, tenantA, func(database *store.Store) http.Handler {
		st = database
		return fileRouteHandler(t, database)
	})
	env.bootstrapAdmin("principal-file-password-1")
	ctx := context.Background()
	if err := st.CreateTenant(ctx, tenantB, "attachment-principal-b"); err != nil {
		t.Fatal(err)
	}
	uploaderA := &blob.Uploader{Store: st, CASDir: t.TempDir()}
	uploaderB := &blob.Uploader{Store: st, CASDir: t.TempDir()}
	metaA, err := uploaderA.Put(ctx, tenantA, "text/plain", bytes.NewReader([]byte("tenant-a-bytes")))
	if err != nil {
		t.Fatal(err)
	}
	metaB, err := uploaderB.Put(ctx, tenantB, "text/plain", bytes.NewReader([]byte("tenant-b-bytes")))
	if err != nil {
		t.Fatal(err)
	}

	get := func(client *http.Client, path string, headers map[string]string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, env.server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, body
	}

	filePath := "/v1/files/" + metaA.BlobID.String()
	// Anonymous, with and without a forged tenant header: 401.
	anonymous := env.newClient()
	if res, _ := get(anonymous, filePath, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous file read = %d, want 401", res.StatusCode)
	}
	if res, _ := get(anonymous, filePath, map[string]string{"X-Tenant-ID": tenantA.String()}); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous forged-header file read = %d, want 401", res.StatusCode)
	}
	// The authenticated principal reads its own blob with no tenant header.
	res, body := get(env.client, filePath, nil)
	if res.StatusCode != http.StatusOK || !bytes.Equal(body, []byte("tenant-a-bytes")) {
		t.Fatalf("principal file read = %d %q", res.StatusCode, body)
	}
	// A forged X-Tenant-ID naming another tenant must not retarget the read.
	res, body = get(env.client, filePath, map[string]string{"X-Tenant-ID": tenantB.String()})
	if res.StatusCode != http.StatusOK || !bytes.Equal(body, []byte("tenant-a-bytes")) {
		t.Fatalf("forged-header read = %d %q, want the principal's bytes", res.StatusCode, body)
	}
	// Another tenant's id is a 404, indistinguishable from an unknown one.
	res, _ = get(env.client, "/v1/files/"+metaB.BlobID.String(), nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant file read = %d, want 404", res.StatusCode)
	}
	// The thumbnail route carries the same tenant predicate.
	if res, _ := get(anonymous, filePath+"/thumbnail", map[string]string{"X-Tenant-ID": tenantA.String()}); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous thumbnail = %d, want 401", res.StatusCode)
	}
	if res, _ := get(env.client, "/v1/files/"+metaB.BlobID.String()+"/thumbnail", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant thumbnail = %d, want 404", res.StatusCode)
	}
}
