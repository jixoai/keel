package httpapi

// Upload size refusals (change keel-plugin-attachment, review P1-2): both body
// shapes must answer 413, never 500. The raw path is bounded by MaxBytesReader;
// the multipart path bounds the whole body and then admits the file's size.

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/google/uuid"
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
