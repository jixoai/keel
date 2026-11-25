package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
)

// OTLPOptions configures the optional OTLP/HTTP adapter. Endpoint is a URL
// such as http://127.0.0.1:4318; an empty endpoint means the caller should not
// construct this adapter at all (the host keeps the in-process memory adapter).
type OTLPOptions struct {
	Endpoint    string
	Headers     map[string]string
	ServiceName string
}

// OTLP is the optional OpenTelemetry adapter. It is constructed only when a
// tenant explicitly selects telemetry.adapter=otlp with an endpoint; the
// default factory never creates an exporter or a background goroutine.
type OTLP struct {
	*otlpContext
	tracer trace.Tracer
	meter  otelmetric.Meter
	tp     *sdktrace.TracerProvider
	mp     *sdkmetric.MeterProvider
	prop   propagation.TextMapPropagator
}

type otlpContext struct {
	owner *OTLP
	ctx   context.Context
}

func (c *otlpContext) Metrics() Metrics         { return &otlpMetrics{owner: c.owner} }
func (c *otlpContext) Propagation() Propagation { return c.owner }
func (c *otlpContext) StartSpan(options SpanOptions, callback func(Span) error) error {
	return c.owner.startSpan(c.ctx, options, callback)
}
func (c *otlpContext) StartManualSpan(options SpanOptions) (Span, func()) {
	return c.owner.startManualSpan(c.ctx, options)
}

// NewOTLP builds OTLP/HTTP trace and metric exporters. Callers must Shutdown
// the returned adapter when the hosting process stops.
func NewOTLP(ctx context.Context, options OTLPOptions) (*OTLP, error) {
	endpoint := strings.TrimSpace(options.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("telemetry: otlp endpoint is empty")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("telemetry: parse otlp endpoint: %w", err)
	}
	host := parsed.Host
	if host == "" {
		host = endpoint
	}
	traceOpts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(host)}
	metricOpts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(host)}
	if len(options.Headers) > 0 {
		traceOpts = append(traceOpts, otlptracehttp.WithHeaders(options.Headers))
		metricOpts = append(metricOpts, otlpmetrichttp.WithHeaders(options.Headers))
	}
	if parsed.Scheme == "http" {
		traceOpts = append(traceOpts, otlptracehttp.WithInsecure())
		metricOpts = append(metricOpts, otlpmetrichttp.WithInsecure())
	}
	if path := strings.TrimSpace(parsed.Path); path != "" && path != "/" {
		traceOpts = append(traceOpts, otlptracehttp.WithURLPath(path))
		metricOpts = append(metricOpts, otlpmetrichttp.WithURLPath(path))
	}
	traceExporter, err := otlptracehttp.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: otlp trace exporter: %w", err)
	}
	metricExporter, err := otlpmetrichttp.New(ctx, metricOpts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: otlp metric exporter: %w", err)
	}
	serviceName := strings.TrimSpace(options.ServiceName)
	if serviceName == "" {
		serviceName = "keel"
	}
	res := resource.NewSchemaless(attribute.String("service.name", serviceName))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)
	adapter := &OTLP{
		tracer: tp.Tracer("keel"),
		meter:  mp.Meter("keel"),
		tp:     tp,
		mp:     mp,
		prop:   propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	}
	adapter.otlpContext = &otlpContext{owner: adapter, ctx: ctx}
	return adapter, nil
}

// Shutdown flushes and stops both providers.
func (o *OTLP) Shutdown(ctx context.Context) error {
	if o == nil {
		return nil
	}
	if err := o.tp.Shutdown(ctx); err != nil {
		return err
	}
	return o.mp.Shutdown(ctx)
}

func (o *OTLP) startSpan(ctx context.Context, options SpanOptions, callback func(Span) error) error {
	spanCtx, span := o.tracer.Start(ctx, options.Name, o.startOptions(options)...)
	defer span.End()
	err := callback(&otlpSpan{owner: o, ctx: spanCtx, span: span})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func (o *OTLP) startManualSpan(ctx context.Context, options SpanOptions) (Span, func()) {
	spanCtx, span := o.tracer.Start(ctx, options.Name, o.startOptions(options)...)
	return &otlpSpan{owner: o, ctx: spanCtx, span: span}, func() { span.End() }
}

func (o *OTLP) startOptions(options SpanOptions) []trace.SpanStartOption {
	opts := []trace.SpanStartOption{trace.WithAttributes(otelAttributes(options.Attributes)...)}
	for _, link := range options.Links {
		if link.Context == nil {
			continue
		}
		carrier, ok := o.inject(link.Context)
		if !ok {
			continue
		}
		linkCtx := o.prop.Extract(context.Background(), headerCarrier(carrier))
		if sc := trace.SpanContextFromContext(linkCtx); sc.IsValid() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc, Attributes: otelAttributes(link.Attributes)}))
		}
	}
	return opts
}

func headerCarrier(carrier Carrier) propagation.HeaderCarrier {
	out := propagation.HeaderCarrier{}
	for key, value := range carrier {
		out[key] = []string{value}
	}
	return out
}

func (o *OTLP) Inject(ctx Context) (Carrier, bool) { return o.inject(ctx) }

func (o *OTLP) inject(ctx Context) (Carrier, bool) {
	if ctx == nil {
		return nil, false
	}
	goCtx := context.Background()
	if span, ok := ctx.(*otlpSpan); ok {
		goCtx = span.ctx
	}
	carrier := propagation.HeaderCarrier{}
	o.prop.Inject(goCtx, carrier)
	if len(carrier) == 0 {
		return nil, false
	}
	out := Carrier{}
	for key, values := range carrier {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out, true
}

func (o *OTLP) Extract(carrier Carrier) Context {
	goCtx := context.Background()
	if len(carrier) > 0 {
		headers := propagation.HeaderCarrier{}
		for key, value := range carrier {
			headers[key] = []string{value}
		}
		goCtx = o.prop.Extract(goCtx, headers)
	}
	return &otlpContext{owner: o, ctx: goCtx}
}

type otlpSpan struct {
	owner *OTLP
	ctx   context.Context
	span  trace.Span
}

func (s *otlpSpan) Metrics() Metrics         { return &otlpMetrics{owner: s.owner} }
func (s *otlpSpan) Propagation() Propagation { return s.owner }
func (s *otlpSpan) StartSpan(options SpanOptions, callback func(Span) error) error {
	return s.owner.startSpan(s.ctx, options, callback)
}
func (s *otlpSpan) StartManualSpan(options SpanOptions) (Span, func()) {
	return s.owner.startManualSpan(s.ctx, options)
}
func (s *otlpSpan) AddLink(link SpanLink) {
	if link.Context == nil {
		return
	}
	carrier, ok := s.owner.inject(link.Context)
	if !ok {
		return
	}
	ctx := s.owner.prop.Extract(context.Background(), headerCarrier(carrier))
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		s.span.AddLink(trace.Link{SpanContext: sc, Attributes: otelAttributes(link.Attributes)})
	}
}
func (s *otlpSpan) AddEvent(name string, attributes SpanAttributes) {
	s.span.AddEvent(name, trace.WithAttributes(otelAttributes(attributes)...))
}
func (s *otlpSpan) SetAttributes(attributes SpanAttributes) {
	s.span.SetAttributes(otelAttributes(attributes)...)
}
func (s *otlpSpan) SetStatus(status SpanStatus) {
	if status.Status == SpanStatusError {
		message := ""
		if status.Error != nil {
			message = status.Error.Message
		}
		s.span.SetStatus(codes.Error, message)
		return
	}
	s.span.SetStatus(codes.Ok, "")
}

type otlpMetrics struct {
	owner *OTLP
}

func (m *otlpMetrics) Counter(name string, options *InstrumentOptions) Counter {
	var opts []otelmetric.Float64CounterOption
	if options != nil {
		if options.Description != "" {
			opts = append(opts, otelmetric.WithDescription(options.Description))
		}
		if options.Unit != "" {
			opts = append(opts, otelmetric.WithUnit(options.Unit))
		}
	}
	inst, _ := m.owner.meter.Float64Counter(name, opts...)
	return otlpCounter{inst: inst}
}

func (m *otlpMetrics) UpDownCounter(name string, options *InstrumentOptions) UpDownCounter {
	var opts []otelmetric.Float64UpDownCounterOption
	if options != nil {
		if options.Description != "" {
			opts = append(opts, otelmetric.WithDescription(options.Description))
		}
		if options.Unit != "" {
			opts = append(opts, otelmetric.WithUnit(options.Unit))
		}
	}
	inst, _ := m.owner.meter.Float64UpDownCounter(name, opts...)
	return otlpUpDownCounter{inst: inst}
}

func (m *otlpMetrics) Histogram(name string, options *HistogramOptions) Histogram {
	var opts []otelmetric.Float64HistogramOption
	if options != nil {
		if options.Description != "" {
			opts = append(opts, otelmetric.WithDescription(options.Description))
		}
		if options.Unit != "" {
			opts = append(opts, otelmetric.WithUnit(options.Unit))
		}
	}
	inst, _ := m.owner.meter.Float64Histogram(name, opts...)
	return otlpHistogram{inst: inst}
}

type otlpCounter struct{ inst otelmetric.Float64Counter }

func (c otlpCounter) Add(value float64, attributes SpanAttributes) {
	if c.inst != nil {
		c.inst.Add(context.Background(), value, otelmetric.WithAttributes(otelAttributes(attributes)...))
	}
}

type otlpUpDownCounter struct {
	inst otelmetric.Float64UpDownCounter
}

func (c otlpUpDownCounter) Add(value float64, attributes SpanAttributes) {
	if c.inst != nil {
		c.inst.Add(context.Background(), value, otelmetric.WithAttributes(otelAttributes(attributes)...))
	}
}

type otlpHistogram struct{ inst otelmetric.Float64Histogram }

func (h otlpHistogram) Record(value float64, attributes SpanAttributes, _ Context) {
	if h.inst != nil {
		h.inst.Record(context.Background(), value, otelmetric.WithAttributes(otelAttributes(attributes)...))
	}
}

func otelAttributes(attributes SpanAttributes) []attribute.KeyValue {
	if len(attributes) == 0 {
		return nil
	}
	out := make([]attribute.KeyValue, 0, len(attributes))
	for key, value := range attributes {
		switch v := value.(type) {
		case string:
			out = append(out, attribute.String(key, v))
		case bool:
			out = append(out, attribute.Bool(key, v))
		case int:
			out = append(out, attribute.Int(key, v))
		case int64:
			out = append(out, attribute.Int64(key, v))
		case float64:
			out = append(out, attribute.Float64(key, v))
		case []string:
			out = append(out, attribute.StringSlice(key, v))
		}
	}
	return out
}
