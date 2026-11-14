package provider

// Vendor file uploaders: the three Files APIs this deployment implements
// (keel-ga §3 order: Gemini → OpenAI Responses → Anthropic).
//
// Each uploader is plain HTTP against the vendor's documented multipart/JSON
// upload endpoint. They are separate types rather than one parameterized client
// because the three protocols disagree on every detail that matters:
//
//	           upload endpoint                     handle returned    expiry
//	Gemini     POST {base}/upload/v1beta/files     file.uri (string) 48h (server)
//	Anthropic  POST {base}/v1/files (beta header)  id                 永不过期
//	OpenAI     POST {base}/files                   id                 永不过期
//
// A vendor whose upload fails (401, 429, network) surfaces the error and the
// resolver degrades; nothing here retries, because an upload is on the request
// path and a retry loop would multiply the run's latency.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// geminiFileTTL is the Gemini Files API retention window (48h, server-enforced).
// The cache stores it so a handle past the window is re-uploaded lazily; the
// number is the vendor's documented constant, not a local policy.
const geminiFileTTL = 48 * time.Hour

// anthropicFilesBeta is the beta header the Anthropic Files API requires.
const anthropicFilesBeta = "files-api-2025-04-14"

// httpUploader is the shared bones of an HTTP file uploader: base URL, auth
// header, HTTP client.
type httpUploader struct {
	baseURL string
	client  *http.Client
}

func (u *httpUploader) http() *http.Client {
	if u.client != nil {
		return u.client
	}
	return http.DefaultClient
}

func (u *httpUploader) endpoint(path string) string {
	return strings.TrimRight(u.baseURL, "/") + path
}

// multipartUpload streams one blob as a multipart/form-data body. The body is
// piped, not buffered: a 2GB original crosses the network without ever being
// resident, which is the whole reason the upload path takes a stream.
func multipartUpload(ctx context.Context, client *http.Client, method, target string, headers map[string]string, field, filename, contentType string, open func() (io.ReadCloser, error)) (*http.Response, error) {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		part, err := createFilePart(writer, field, filename, contentType)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		src, err := open()
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		defer src.Close()
		if _, err := io.Copy(part, src); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := pw.CloseWithError(writer.Close()); err != nil {
			return
		}
	}()
	req, err := http.NewRequestWithContext(ctx, method, target, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, err
	}
	return resp, nil
}

// createFilePart opens a multipart file part with an explicit content type (the
// stdlib's CreateFormFile always labels the part application/octet-stream, and
// the Files APIs key off the real image mime).
func createFilePart(writer *multipart.Writer, field, filename, contentType string) (io.Writer, error) {
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filename))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header.Set("Content-Type", contentType)
	return writer.CreatePart(header)
}

// uploadFilename picks a filename for the multipart part: the caller's display
// name when it looks like a bare name, otherwise a synthesized one from the mime.
func uploadFilename(name, mime string, blob BlobInfo) string {
	base := strings.TrimSpace(name)
	if base != "" {
		if i := strings.LastIndexAny(base, `/\`); i >= 0 {
			base = base[i+1:]
		}
		if base != "" && len(base) <= 120 {
			return base
		}
	}
	if blob.BlobID != "" {
		return "image-" + blob.BlobID + extensionForMime(mime)
	}
	return "image" + extensionForMime(mime)
}

func extensionForMime(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/heic":
		return ".heic"
	case "application/pdf":
		return ".pdf"
	default:
		return ".bin"
	}
}

// readJSONBody decodes a bounded JSON response body.
func readJSONBody(resp *http.Response) (map[string]any, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

// statusError turns a non-2xx response into an error naming the status and a
// bounded slice of the body (never a credential: the body is the vendor's).
func statusError(providerName string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	resp.Body.Close()
	detail := strings.TrimSpace(string(raw))
	if len(detail) > 300 {
		detail = detail[:300] + "…"
	}
	if detail == "" {
		return fmt.Errorf("%s files: %s", providerName, resp.Status)
	}
	return fmt.Errorf("%s files: %s: %s", providerName, resp.Status, detail)
}

// ---------------------------------------------------------------------------
// Gemini Files API
// ---------------------------------------------------------------------------

// GeminiFileUploader uploads to Google's Files API (generativelanguage) and
// returns file.uri, which the Gemini generateContent wire takes as
// file_data.file_uri. Handles expire after 48h; the cache re-uploads lazily.
//
// Auth: x-goog-api-key, matching the OpenAI-compatible usage of the same key.
type GeminiFileUploader struct {
	httpUploader
	APIKey string
	// UploadBaseURL overrides the upload endpoint root (default derives from
	// BaseURL: the Files API lives at /upload/v1beta on generativelanguage).
	UploadBaseURL string
}

// NewGeminiFileUploader builds the Gemini uploader. baseURL is the model API
// root (…/v1beta); the upload root is derived from it unless overridden.
func NewGeminiFileUploader(baseURL, apiKey string, client *http.Client) *GeminiFileUploader {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	return &GeminiFileUploader{httpUploader: httpUploader{baseURL: baseURL, client: client}, APIKey: apiKey}
}

func (u *GeminiFileUploader) Provider() string { return "google" }

func (u *GeminiFileUploader) uploadBase() string {
	if u.UploadBaseURL != "" {
		return u.UploadBaseURL
	}
	// {root}/v1beta → {root}/upload/v1beta
	raw := strings.TrimRight(u.baseURL, "/")
	if idx := strings.LastIndex(raw, "/v1beta"); idx >= 0 {
		return raw[:idx] + "/upload/v1beta"
	}
	return raw + "/upload/v1beta"
}

// Upload performs the two-step Files API handshake: start an upload session with
// the file's metadata, then POST the bytes to the returned upload URL.
func (u *GeminiFileUploader) Upload(ctx context.Context, req FileUploadRequest) (FileRef, error) {
	if strings.TrimSpace(u.APIKey) == "" {
		return FileRef{}, fmt.Errorf("google files: missing API key")
	}
	name := uploadFilename(req.Name, req.Blob.Mime, req.Blob)
	metadata, err := json.Marshal(map[string]any{
		"file": map[string]any{"display_name": name},
	})
	if err != nil {
		return FileRef{}, err
	}
	startURL := u.uploadBase() + "/files"
	startReq, err := http.NewRequestWithContext(ctx, http.MethodPost, startURL, bytes.NewReader(metadata))
	if err != nil {
		return FileRef{}, err
	}
	startReq.Header.Set("Content-Type", "application/json")
	startReq.Header.Set("x-goog-api-key", u.APIKey)
	startReq.Header.Set("X-Goog-Upload-Protocol", "resumable")
	startReq.Header.Set("X-Goog-Upload-Command", "start")
	startReq.Header.Set("X-Goog-Upload-Header-Content-Length", fmt.Sprintf("%d", req.Blob.Size))
	mime := req.Blob.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}
	startReq.Header.Set("X-Goog-Upload-Header-Content-Type", mime)
	startResp, err := u.http().Do(startReq)
	if err != nil {
		return FileRef{}, err
	}
	if startResp.StatusCode < 200 || startResp.StatusCode > 299 {
		return FileRef{}, statusError("google", startResp)
	}
	uploadURL := startResp.Header.Get("X-Goog-Upload-URL")
	startResp.Body.Close()
	if uploadURL == "" {
		return FileRef{}, fmt.Errorf("google files: upload session returned no X-Goog-Upload-URL")
	}
	// Local copy: the upload goroutine reads it after this frame returns.
	open := req.Open
	uploadResp, err := multipartUploadFree(ctx, u.http(), http.MethodPost, uploadURL, map[string]string{
		"Content-Type":          mime,
		"x-goog-api-key":        u.APIKey,
		"X-Goog-Upload-Offset":  "0",
		"X-Goog-Upload-Command": "upload, finalize",
	}, open)
	if err != nil {
		return FileRef{}, err
	}
	if uploadResp.StatusCode < 200 || uploadResp.StatusCode > 299 {
		return FileRef{}, statusError("google", uploadResp)
	}
	body, err := readJSONBody(uploadResp)
	if err != nil {
		return FileRef{}, fmt.Errorf("google files: %w", err)
	}
	file, _ := body["file"].(map[string]any)
	uri, _ := file["uri"].(string)
	if uri == "" {
		return FileRef{}, fmt.Errorf("google files: response carries no file.uri")
	}
	return FileRef{FileID: uri, URI: uri, ExpiresAt: time.Now().Add(geminiFileTTL)}, nil
}

// multipartUploadFree posts the raw bytes of a stream (not multipart) — Gemini's
// upload session takes the file body directly on the finalize call.
func multipartUploadFree(ctx context.Context, client *http.Client, method, target string, headers map[string]string, open func(context.Context) (io.ReadCloser, error)) (*http.Response, error) {
	pr, pw := io.Pipe()
	go func() {
		src, err := open(ctx)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		defer src.Close()
		_, copyErr := io.Copy(pw, src)
		_ = pw.CloseWithError(copyErr)
	}()
	req, err := http.NewRequestWithContext(ctx, method, target, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, err
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// OpenAI Files API (Responses input_image.file_id)
// ---------------------------------------------------------------------------

// OpenAIFileUploader uploads with purpose "user_data" and returns the file id the
// Responses API takes as input_image.file_id.
type OpenAIFileUploader struct {
	httpUploader
	APIKey string
}

// NewOpenAIFileUploader builds the OpenAI uploader. baseURL is the API root
// (e.g. https://api.openai.com/v1).
func NewOpenAIFileUploader(baseURL, apiKey string, client *http.Client) *OpenAIFileUploader {
	return &OpenAIFileUploader{httpUploader: httpUploader{baseURL: baseURL, client: client}, APIKey: apiKey}
}

func (u *OpenAIFileUploader) Provider() string { return "openai" }

func (u *OpenAIFileUploader) Upload(ctx context.Context, req FileUploadRequest) (FileRef, error) {
	if strings.TrimSpace(u.APIKey) == "" {
		return FileRef{}, fmt.Errorf("openai files: missing API key")
	}
	open := req.Open
	resp, err := multipartUpload(ctx, u.http(), http.MethodPost, u.endpoint("/files"),
		map[string]string{"Authorization": "Bearer " + u.APIKey},
		"file", uploadFilename(req.Name, req.Blob.Mime, req.Blob), req.Blob.Mime,
		func() (io.ReadCloser, error) { return open(ctx) })
	if err != nil {
		return FileRef{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return FileRef{}, statusError("openai", resp)
	}
	body, err := readJSONBody(resp)
	if err != nil {
		return FileRef{}, fmt.Errorf("openai files: %w", err)
	}
	id, _ := body["id"].(string)
	if id == "" {
		return FileRef{}, fmt.Errorf("openai files: response carries no id")
	}
	return FileRef{FileID: id}, nil
}

// ---------------------------------------------------------------------------
// Anthropic Files API
// ---------------------------------------------------------------------------

// AnthropicFileUploader uploads through Anthropic's Files API (beta) and returns
// the file id the Messages API takes as image source {type:"file", file_id:…}.
type AnthropicFileUploader struct {
	httpUploader
	APIKey string
	// Version is the anthropic-version header (defaults to the driver constant).
	Version string
}

// NewAnthropicFileUploader builds the Anthropic uploader. baseURL is the API root
// (e.g. https://api.anthropic.com/v1).
func NewAnthropicFileUploader(baseURL, apiKey string, client *http.Client) *AnthropicFileUploader {
	return &AnthropicFileUploader{httpUploader: httpUploader{baseURL: baseURL, client: client}, APIKey: apiKey}
}

func (u *AnthropicFileUploader) Provider() string { return "anthropic" }

func (u *AnthropicFileUploader) Upload(ctx context.Context, req FileUploadRequest) (FileRef, error) {
	if strings.TrimSpace(u.APIKey) == "" {
		return FileRef{}, fmt.Errorf("anthropic files: missing API key")
	}
	version := u.Version
	if version == "" {
		version = anthropicAPIVersion
	}
	open := req.Open
	resp, err := multipartUpload(ctx, u.http(), http.MethodPost, u.endpoint("/files"),
		map[string]string{"x-api-key": u.APIKey, "anthropic-version": version, "anthropic-beta": anthropicFilesBeta},
		"file", uploadFilename(req.Name, req.Blob.Mime, req.Blob), req.Blob.Mime,
		func() (io.ReadCloser, error) { return open(ctx) })
	if err != nil {
		return FileRef{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return FileRef{}, statusError("anthropic", resp)
	}
	body, err := readJSONBody(resp)
	if err != nil {
		return FileRef{}, fmt.Errorf("anthropic files: %w", err)
	}
	id, _ := body["id"].(string)
	if id == "" {
		return FileRef{}, fmt.Errorf("anthropic files: response carries no id")
	}
	return FileRef{FileID: id}, nil
}

// ---------------------------------------------------------------------------
// URL signer for the url form
// ---------------------------------------------------------------------------

// PathURLSigner mints URLs under a deployment's public base URL. The platform's
// file endpoint is GET /v1/files/{id}, which streams the blob; a deployment that
// fronts Keel with a public origin sets Base to that origin so a provider can
// fetch the image.
//
// The URL is *not* signed (the file endpoint authorizes by tenant header, which a
// provider cannot send). It is therefore only usable by a deployment that
// exposes the file path publicly; that is why the resolver treats "no signer
// configured" as "no url form" rather than minting an unusable URL.
type PathURLSigner struct {
	Base string
	// Query carries optional extra parameters (e.g. a deployment's access token).
	// It is never a credential for this Keel deployment: it authenticates the
	// public fronting layer only.
	Query url.Values
}

// ImageURL builds {Base}/v1/files/{blobID}.
func (s *PathURLSigner) ImageURL(_ context.Context, _ string, blob BlobInfo, _ time.Duration) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(s.Base), "/")
	if base == "" {
		return "", fmt.Errorf("provider: no public base URL configured for image URLs")
	}
	if strings.TrimSpace(blob.BlobID) == "" {
		return "", fmt.Errorf("provider: blob has no id")
	}
	target := base + "/v1/files/" + url.PathEscape(blob.BlobID)
	if len(s.Query) > 0 {
		target += "?" + s.Query.Encode()
	}
	return target, nil
}
