package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OTLPOptions configures the optional OTLP/HTTP JSON adapter. Endpoint is the
// base URL (for example http://127.0.0.1:4318); the adapter appends the
// standard /v1/traces and /v1/metrics paths. An empty endpoint is rejected:
// the host keeps the in-process memory adapter instead of creating an exporter.
type OTLPOptions struct {
	Endpoint    string
	Headers     map[string]string
	ServiceName string
}

// OTLP is the optional zero-dependency OTLP/HTTP JSON adapter. It records into
// the in-process Memory adapter and exports settled spans/measurements on
// Shutdown, so no exporter goroutine or connection exists while it is idle.
// It intentionally does not pull the OpenTelemetry SDK into the shipping
// binary (the binary-size gate forbids that dependency).
type OTLP struct {
	*Memory
	endpoint string
	headers  map[string]string
	service  string
	client   *http.Client
}

// NewOTLP validates the endpoint and returns an idle adapter. No network
// connection is made until Shutdown.
func NewOTLP(_ context.Context, options OTLPOptions) (*OTLP, error) {
	endpoint := strings.TrimSpace(options.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("telemetry: otlp endpoint is empty")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("telemetry: otlp endpoint must be an absolute http(s) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("telemetry: otlp endpoint scheme must be http or https")
	}
	service := strings.TrimSpace(options.ServiceName)
	if service == "" {
		service = "keel"
	}
	headers := make(map[string]string, len(options.Headers))
	for key, value := range options.Headers {
		headers[key] = value
	}
	return &OTLP{
		Memory:   NewMemory(),
		endpoint: strings.TrimSuffix(endpoint, "/"),
		headers:  headers,
		service:  service,
		client:   &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Shutdown exports the gathered spans and metrics as OTLP/HTTP JSON.
func (o *OTLP) Shutdown(ctx context.Context) error {
	if o == nil {
		return nil
	}
	if err := o.post(ctx, "/v1/traces", o.tracePayload()); err != nil {
		return err
	}
	return o.post(ctx, "/v1/metrics", o.metricPayload())
}

func (o *OTLP) post(ctx context.Context, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range o.headers {
		req.Header.Set(key, value)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telemetry: otlp %s returned %s", path, resp.Status)
	}
	return nil
}

func (o *OTLP) resource() map[string]any {
	return map[string]any{"attributes": otlpAttrs(SpanAttributes{"service.name": o.service})}
}

func (o *OTLP) tracePayload() map[string]any {
	page, _ := o.FindTraces(TraceQuery{Limit: 100000})
	out := make([]any, 0)
	for _, trace := range page.Traces {
		for _, span := range trace.Spans {
			item := map[string]any{
				"traceId":           span.TraceID,
				"spanId":            span.ID,
				"name":              span.Name,
				"kind":              1,
				"startTimeUnixNano": fmt.Sprintf("%d", int64(span.StartedMs*1e6)),
				"endTimeUnixNano":   fmt.Sprintf("%d", int64(span.EndedMs*1e6)),
			}
			if span.ParentID != "" {
				item["parentSpanId"] = span.ParentID
			}
			if attrs := otlpAttrs(span.Attributes); len(attrs) > 0 {
				item["attributes"] = attrs
			}
			if len(span.Events) > 0 {
				events := make([]any, 0, len(span.Events))
				for _, event := range span.Events {
					events = append(events, map[string]any{
						"name":         event.Name,
						"timeUnixNano": fmt.Sprintf("%d", int64((span.StartedMs+event.TimeMs)*1e6)),
						"attributes":   otlpAttrs(event.Attributes),
					})
				}
				item["events"] = events
			}
			status := map[string]any{"code": 1}
			if span.Status.Status == SpanStatusError {
				status["code"] = 2
				if span.Status.Error != nil {
					status["message"] = span.Status.Error.Message
				}
			}
			item["status"] = status
			out = append(out, item)
		}
	}
	return map[string]any{"resourceSpans": []any{map[string]any{
		"resource": o.resource(),
		"scopeSpans": []any{map[string]any{
			"scope": map[string]any{"name": "keel"},
			"spans": out,
		}},
	}}}
}

func (o *OTLP) metricPayload() map[string]any {
	type metricAgg struct {
		kind   MetricKind
		unit   string
		points []any
	}
	order := make([]string, 0)
	byName := map[string]*metricAgg{}
	for _, measurement := range o.Measurements() {
		key := string(measurement.Kind) + "\x00" + measurement.Name
		agg, ok := byName[key]
		if !ok {
			agg = &metricAgg{kind: measurement.Kind, unit: measurement.Unit}
			byName[key] = agg
			order = append(order, key)
		}
		point := map[string]any{
			"timeUnixNano": fmt.Sprintf("%d", time.Now().UnixNano()),
			"asDouble":     measurement.Value,
		}
		if attrs := otlpAttrs(measurement.Attributes); len(attrs) > 0 {
			point["attributes"] = attrs
		}
		if measurement.Kind == MetricKindHistogram {
			point = map[string]any{
				"timeUnixNano":   fmt.Sprintf("%d", time.Now().UnixNano()),
				"count":          "1",
				"sum":            measurement.Value,
				"bucketCounts":   []string{"1"},
				"explicitBounds": []float64{},
			}
			if attrs := otlpAttrs(measurement.Attributes); len(attrs) > 0 {
				point["attributes"] = attrs
			}
		}
		agg.points = append(agg.points, point)
	}
	metrics := make([]any, 0, len(order))
	for _, key := range order {
		agg := byName[key]
		name := strings.SplitN(key, "\x00", 2)[1]
		metric := map[string]any{"name": name}
		if agg.unit != "" {
			metric["unit"] = agg.unit
		}
		if agg.kind == MetricKindHistogram {
			metric["histogram"] = map[string]any{
				"aggregationTemporality": 2,
				"dataPoints":             agg.points,
			}
		} else {
			metric["sum"] = map[string]any{
				"aggregationTemporality": 2,
				"isMonotonic":            agg.kind == MetricKindCounter,
				"dataPoints":             agg.points,
			}
		}
		metrics = append(metrics, metric)
	}
	return map[string]any{"resourceMetrics": []any{map[string]any{
		"resource": o.resource(),
		"scopeMetrics": []any{map[string]any{
			"scope":   map[string]any{"name": "keel"},
			"metrics": metrics,
		}},
	}}}
}

func otlpAttrs(attrs SpanAttributes) []any {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]any, 0, len(attrs))
	for key, value := range attrs {
		var encoded any
		switch v := value.(type) {
		case string:
			encoded = map[string]any{"stringValue": v}
		case bool:
			encoded = map[string]any{"boolValue": v}
		case int:
			encoded = map[string]any{"intValue": fmt.Sprintf("%d", v)}
		case int64:
			encoded = map[string]any{"intValue": fmt.Sprintf("%d", v)}
		case float64:
			encoded = map[string]any{"doubleValue": v}
		default:
			continue
		}
		out = append(out, map[string]any{"key": key, "value": encoded})
	}
	return out
}
