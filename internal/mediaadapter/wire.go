package mediaadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// wireClient is the shared JSON transport for the HTTP provider adapters. It
// owns the three properties every family needs: a bounded response body, no
// credential in any error, and no credential-bearing redirect.
type wireClient struct {
	baseURL     string
	apiKey      string
	client      *http.Client
	maxResponse int64
}

func newWireClient(baseURL, apiKey string, client *http.Client, maxResponse int64) (*wireClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, configError("provider", "config", "provider base URL is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, configError("provider", "config", "provider base URL must be an absolute http(s) URL")
	}
	if client == nil {
		client = BoundedHTTPClient(2 * time.Minute)
	}
	if maxResponse <= 0 {
		maxResponse = 8 << 20
	}
	return &wireClient{baseURL: baseURL, apiKey: strings.TrimSpace(apiKey), client: client, maxResponse: maxResponse}, nil
}

// endpoint joins the configured base and a path. A path that is itself an
// absolute URL is considered an explicit full endpoint (a gateway that mounts
// capabilities under a nonstandard root).
func (c *wireClient) endpoint(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", configError("provider", "config", "provider endpoint path is required")
	}
	if parsed, err := url.Parse(path); err == nil && parsed.IsAbs() {
		if parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return "", configError("provider", "config", "provider endpoint must be an absolute http(s) URL")
		}
		return parsed.String(), nil
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.baseURL + path, nil
}

// postJSON sends one JSON body and returns the response bytes. Non-2xx
// responses are typed upstream errors carrying a bounded, scrubbed message.
func (c *wireClient) postJSON(ctx context.Context, family, op, path string, payload any, headers map[string]string) ([]byte, int, error) {
	endpoint, err := c.endpoint(path)
	if err != nil {
		return nil, 0, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, &Error{Family: family, Op: op, Kind: KindRequest, Message: "request is not encodable: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, &Error{Family: family, Op: op, Kind: KindConfig, Message: "provider request could not be built", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, providerError(family, op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponse+1))
	if err != nil {
		return nil, resp.StatusCode, providerError(family, op, err)
	}
	if int64(len(raw)) > c.maxResponse {
		return nil, resp.StatusCode, &Error{
			Family:  family,
			Op:      op,
			Kind:    KindUpstream,
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("provider response exceeds the %d-byte limit", c.maxResponse),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return raw, resp.StatusCode, upstreamError(family, op, resp.StatusCode, upstreamMessage(raw), c.apiKey)
	}
	return raw, resp.StatusCode, nil
}

// upstreamMessage extracts a human-readable error from the common provider
// envelopes. The result is still passed through sanitize.
func upstreamMessage(raw []byte) string {
	var envelope struct {
		Error   json.RawMessage   `json:"error"`
		Message string            `json:"message"`
		Errors  []json.RawMessage `json:"errors"`
		Detail  json.RawMessage   `json:"detail"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return string(raw)
	}
	if envelope.Message != "" {
		return envelope.Message
	}
	if len(envelope.Errors) > 0 {
		parts := make([]string, 0, len(envelope.Errors))
		for _, item := range envelope.Errors {
			parts = append(parts, jsonText(item))
		}
		return strings.Join(parts, "; ")
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return jsonText(envelope.Error)
	}
	if len(envelope.Detail) > 0 && string(envelope.Detail) != "null" {
		return jsonText(envelope.Detail)
	}
	return string(raw)
}

func jsonText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var object struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	}
	if json.Unmarshal(raw, &object) == nil && object.Message != "" {
		return object.Message
	}
	compact := strings.TrimSpace(string(raw))
	if len(compact) > 512 {
		compact = compact[:512] + "…"
	}
	return compact
}
