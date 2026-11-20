// Shared helpers for the G5 load driver: HTTP, catalogue snapshots, metrics
// parsing and the small statistics the report needs.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 512,
			MaxConnsPerHost:     0,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
	}
}

func newAPIClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
	}
}

func readAllLimited(body io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, limit))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// --- catalogue -----------------------------------------------------------

type appToolView struct {
	Tool       string   `json:"tool"`
	Visibility []string `json:"visibility"`
}

type appSpecView struct {
	AppID       string        `json:"appId"`
	Server      string        `json:"server"`
	Title       string        `json:"title"`
	ResourceURI string        `json:"resourceUri"`
	Hash        string        `json:"hash"`
	Tools       []appToolView `json:"tools"`
}

type catalogView struct {
	Generation  int64           `json:"generation"`
	Apps        []appSpecView   `json:"apps"`
	Diagnostics json.RawMessage `json:"diagnostics"`
	Limits      map[string]int  `json:"limits"`
}

type catalogSnapshot struct {
	Generation     int64          `json:"generation"`
	Apps           int            `json:"apps"`
	AppIDs         []string       `json:"appIds"`
	ResponseBytes  int            `json:"responseBytes"`
	CanonicalBytes int            `json:"canonicalBytes"`
	SetHash        string         `json:"setHash"`
	Hashes         []string       `json:"hashes"`
	Limits         map[string]int `json:"limits,omitempty"`
}

type canonicalApp struct {
	AppID       string          `json:"appId"`
	Server      string          `json:"server"`
	ResourceURI string          `json:"resourceUri"`
	Hash        string          `json:"hash"`
	Tools       []canonicalTool `json:"tools"`
}

type canonicalTool struct {
	Tool       string   `json:"tool"`
	Visibility []string `json:"visibility"`
}

func canonicalCatalog(apps []appSpecView) ([]byte, string) {
	canonical := make([]canonicalApp, 0, len(apps))
	for _, app := range apps {
		tools := make([]canonicalTool, 0, len(app.Tools))
		for _, tool := range app.Tools {
			visibility := append([]string(nil), tool.Visibility...)
			sort.Strings(visibility)
			tools = append(tools, canonicalTool{Tool: tool.Tool, Visibility: visibility})
		}
		sort.Slice(tools, func(i, j int) bool { return tools[i].Tool < tools[j].Tool })
		canonical = append(canonical, canonicalApp{
			AppID:       app.AppID,
			Server:      app.Server,
			ResourceURI: app.ResourceURI,
			Hash:        app.Hash,
			Tools:       tools,
		})
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].AppID < canonical[j].AppID })
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func snapshotCatalog(raw []byte, view catalogView) catalogSnapshot {
	canonical, hash := canonicalCatalog(view.Apps)
	ids := make([]string, 0, len(view.Apps))
	hashes := make([]string, 0, len(view.Apps))
	for _, app := range view.Apps {
		ids = append(ids, app.AppID)
		hashes = append(hashes, app.Hash)
	}
	sort.Strings(ids)
	sort.Strings(hashes)
	return catalogSnapshot{
		Generation:     view.Generation,
		Apps:           len(view.Apps),
		AppIDs:         ids,
		ResponseBytes:  len(raw),
		CanonicalBytes: len(canonical),
		SetHash:        hash,
		Hashes:         hashes,
		Limits:         view.Limits,
	}
}

// --- Prometheus text -----------------------------------------------------

type metricSnapshot struct {
	Values       map[string]float64 `json:"values"`
	Series       []string           `json:"series"`
	FamilyCounts map[string]int     `json:"familyCounts"`
	RawBytes     int                `json:"rawBytes"`
}

var mcpFamilies = []string{
	"mcp_servers",
	"mcp_surface_generation",
	"mcp_app_fetch_total",
	"mcp_app_bridge_frames_total",
	"mcp_listen_reconnects_total",
	"mcp_mrtr_rounds",
}

func familyOf(name string) string {
	switch name {
	case "mcp_servers", "mcp_surface_generation", "mcp_app_fetch_total",
		"mcp_app_bridge_frames_total", "mcp_listen_reconnects_total":
		return name
	}
	if strings.HasPrefix(name, "mcp_mrtr_rounds") {
		return "mcp_mrtr_rounds"
	}
	return ""
}

func parseMetrics(raw []byte) metricSnapshot {
	snapshot := metricSnapshot{
		Values:       make(map[string]float64),
		FamilyCounts: make(map[string]int),
		RawBytes:     len(raw),
	}
	series := make(map[string]bool)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := fields[0]
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		snapshot.Values[key] = value
		name := key
		if index := strings.IndexByte(name, '{'); index >= 0 {
			name = name[:index]
		}
		if family := familyOf(name); family != "" {
			snapshot.FamilyCounts[family]++
			series[key] = true
		}
	}
	for key := range series {
		snapshot.Series = append(snapshot.Series, key)
	}
	sort.Strings(snapshot.Series)
	return snapshot
}

func sameSeries(a, b metricSnapshot) bool {
	if len(a.Series) != len(b.Series) {
		return false
	}
	for i := range a.Series {
		if a.Series[i] != b.Series[i] {
			return false
		}
	}
	return true
}

func metricValue(snapshot metricSnapshot, key string) (float64, bool) {
	value, ok := snapshot.Values[key]
	return value, ok
}

func metricSumPrefix(snapshot metricSnapshot, prefix string) float64 {
	var total float64
	for key, value := range snapshot.Values {
		if strings.HasPrefix(key, prefix) {
			total += value
		}
	}
	return total
}

// --- statistics ----------------------------------------------------------

func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(p*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func writeJSONFile(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

// --- small HTTP helpers --------------------------------------------------

func (d *driver) getJSON(ctx context.Context, path string, out any) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.config.base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	response, err := d.api.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := readAllLimited(response.Body, 32<<20)
	if err != nil {
		return response.StatusCode, nil, err
	}
	if out != nil && response.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, out); err != nil {
			return response.StatusCode, raw, err
		}
	}
	return response.StatusCode, raw, nil
}

func (d *driver) postJSON(ctx context.Context, path string, body any, out any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.config.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := d.api.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := readAllLimited(response.Body, 16<<20)
	if err != nil {
		return response.StatusCode, nil, err
	}
	if out != nil && response.StatusCode >= 200 && response.StatusCode < 300 && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return response.StatusCode, raw, err
		}
	}
	return response.StatusCode, raw, nil
}

func (d *driver) control(ctx context.Context, method, path string) ([]byte, error) {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader("{}")
	}
	request, err := http.NewRequestWithContext(ctx, method, d.config.control+path, body)
	if err != nil {
		return nil, err
	}
	response, err := d.controlClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, _ := readAllLimited(response.Body, 4<<20)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return raw, fmt.Errorf("control %s: HTTP %d: %s", path, response.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

// --- fixture status ------------------------------------------------------

type fixtureAttempt struct {
	At      time.Time `json:"at"`
	UnixMs  int64     `json:"unixMs"`
	Outcome string    `json:"outcome"`
	ID      string    `json:"id"`
}

type fixtureStatus struct {
	StartedAt       time.Time        `json:"startedAt"`
	StartedAtUnixMs int64            `json:"startedAtUnixMs"`
	Revision        int64            `json:"revision"`
	Mode            string           `json:"mode"`
	Apps            int              `json:"apps"`
	DocBytes        int              `json:"docBytes"`
	ActiveListeners int              `json:"activeListeners"`
	ListenAttempts  []fixtureAttempt `json:"listenAttempts"`
	Drops           int64            `json:"drops"`
	Crashes         int64            `json:"crashes"`
	Calls           int64            `json:"calls"`
	Reads           int64            `json:"reads"`
}
