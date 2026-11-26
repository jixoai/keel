package httpapi

// Upload size refusals (change keel-plugin-attachment, review P1-2): both body
// shapes must answer 413, never 500. The raw path is bounded by MaxBytesReader;
// the multipart path bounds the whole body and then admits the file's size.

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/quota"
)

func TestUploadOverMaxUploadIs413(t *testing.T) {
	hs, _ := newTestServer(t, func(config *Config) { config.MaxUpload = 16 })
	tenant := uuid.NewString()
	if code := createTestTenant(t, hs.Client(), hs.URL, tenant, "upload-limit"); code != http.StatusCreated {
		t.Fatalf("create tenant: %d", code)
	}
	post := func(body []byte, contentType string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, hs.URL+"/v1/files", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Tenant-ID", tenant)
		req.Header.Set("Content-Type", contentType)
		res, err := hs.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(res.Body)
		return res.StatusCode, raw.Bytes()
	}

	if code, body := post(bytes.Repeat([]byte("x"), 17), "application/octet-stream"); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("raw oversized upload = %d, want 413 (%s)", code, body)
	}

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, err := writer.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("y"), 32)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if code, body := post(form.Bytes(), writer.FormDataContentType()); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("multipart oversized upload = %d, want 413 (%s)", code, body)
	}

	// A fitting upload still succeeds on both shapes (the bound is not the only
	// answer).
	if code, body := post([]byte("small"), "text/plain"); code != http.StatusCreated {
		t.Fatalf("raw fitting upload = %d, want 201 (%s)", code, body)
	}
}

func multipartBody(t *testing.T, filename string, content []byte) ([]byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes(), writer.FormDataContentType()
}

func TestMultipartFileAtTheLimitFits(t *testing.T) {
	hs, _ := newTestServer(t, func(config *Config) { config.MaxUpload = 16 })
	tenant := uuid.NewString()
	if code := createTestTenant(t, hs.Client(), hs.URL, tenant, "upload-envelope"); code != http.StatusCreated {
		t.Fatalf("create tenant: %d", code)
	}
	body, contentType := multipartBody(t, "exact.txt", bytes.Repeat([]byte("x"), 16))
	req, err := http.NewRequest(http.MethodPost, hs.URL+"/v1/files", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", contentType)
	res, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("file at the limit = %d (%s), want 201", res.StatusCode, raw)
	}
}

func TestMalformedMultipartIs400(t *testing.T) {
	hs, _ := newTestServer(t)
	tenant := uuid.NewString()
	if code := createTestTenant(t, hs.Client(), hs.URL, tenant, "upload-malformed"); code != http.StatusCreated {
		t.Fatalf("create tenant: %d", code)
	}
	req, err := http.NewRequest(http.MethodPost, hs.URL+"/v1/files", bytes.NewReader([]byte("not a multipart body")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=missing")
	res, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("malformed multipart = %d (%s), want 400", res.StatusCode, raw)
	}
}

func TestRawChunkedUploadRequiresLength(t *testing.T) {
	hs, _ := newTestServer(t)
	tenant := uuid.NewString()
	if code := createTestTenant(t, hs.Client(), hs.URL, tenant, "upload-chunked"); code != http.StatusCreated {
		t.Fatalf("create tenant: %d", code)
	}
	req, err := http.NewRequest(http.MethodPost, hs.URL+"/v1/files", io.NopCloser(bytes.NewReader([]byte("small"))))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1 // force a chunked body
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", "text/plain")
	res, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusLengthRequired {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("chunked raw upload = %d (%s), want 411", res.StatusCode, raw)
	}
}

func TestRawUploadQuotaIs413(t *testing.T) {
	src := quota.QuotaSourceFunc(func(context.Context, uuid.UUID) (quota.TenantQuota, error) {
		return quota.TenantQuota{StorageBytes: 5}, nil
	})
	edge := quota.NewEdge(src, func(context.Context, uuid.UUID) (int64, error) { return 0, nil })
	hs, _ := newTestServer(t, func(config *Config) { config.Quota = edge })
	tenant := uuid.NewString()
	if code := createTestTenant(t, hs.Client(), hs.URL, tenant, "upload-quota"); code != http.StatusCreated {
		t.Fatalf("create tenant: %d", code)
	}
	body := bytes.Repeat([]byte("q"), 6)
	req, err := http.NewRequest(http.MethodPost, hs.URL+"/v1/files", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", "text/plain")
	res, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("raw upload over quota = %d (%s), want 413", res.StatusCode, raw)
	}
}

func TestUploaderMaxBytesSharesTheServerLimit(t *testing.T) {
	uploader := &blob.Uploader{}
	server := New(Config{Uploader: uploader, MaxUpload: 1234})
	if uploader.MaxBytes != 1234 {
		t.Fatalf("uploader MaxBytes = %d, want Config.MaxUpload 1234", uploader.MaxBytes)
	}
	if server.cfg.MaxUpload != 1234 {
		t.Fatalf("server MaxUpload = %d", server.cfg.MaxUpload)
	}
}
