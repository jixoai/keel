// Package metrics is a zero-dependency Prometheus text exposition registry.
//
// The runtime exposes a fixed set of metric families. Cardinality is bounded by
// construction: label names are validated against a whitelist and identifiers
// such as session, run, tenant, request, and stream IDs are rejected. Series
// counts therefore do not grow with the number of sessions or runs.
//
// The exposition format follows the Prometheus text format 0.0.4:
// https://prometheus.io/docs/instrumenting/exposition_formats/
package metrics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Type is a Prometheus metric family type.
type Type string

const (
	// TypeCounter is a monotonically increasing counter.
	TypeCounter Type = "counter"
	// TypeGauge is a value that may go up or down.
	TypeGauge Type = "gauge"
	// TypeHistogram is a cumulative histogram.
	TypeHistogram Type = "histogram"
)

// AllowedLabelNames is the fixed label vocabulary. It intentionally contains no
// identity dimensions: no tenant/session/run/request/stream IDs.
var AllowedLabelNames = map[string]bool{
	"status":  true, // bounded outcome (ok/error/done/failed/...)
	"outcome": true, // alias kept for symmetry with telemetry attributes
	"reason":  true, // bounded cause (drain, timeout, ...)
	"kind":    true, // bounded event/tool class
	"tool":    true, // configured tool name (bounded by the tool loadout)
	"class":   true, // small/medium/large
	"method":  true, // HTTP method
	"route":   true, // matched route pattern (bounded by the mux)
	"code":    true, // HTTP status code class
}

// forbiddenLabelFragments rejects high-cardinality identity labels even when a
// future whitelist entry is added by accident.
var forbiddenLabelFragments = []string{
	"tenant", "session", "run", "request", "stream", "trace", "span",
	"user", "email", "message", "blob", "file", "task", "submission",
}

// ValidateLabelNames reports whether every label name is allowed. It is
// exported so callers and tests can enforce the same contract.
func ValidateLabelNames(names []string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" {
			return fmt.Errorf("metrics: empty label name")
		}
		if !AllowedLabelNames[name] {
			return fmt.Errorf("metrics: label %q is not in the fixed whitelist", name)
		}
		for _, fragment := range forbiddenLabelFragments {
			if strings.Contains(name, fragment) {
				return fmt.Errorf("metrics: label %q is high-cardinality (%q)", name, fragment)
			}
		}
		if seen[name] {
			return fmt.Errorf("metrics: duplicate label %q", name)
		}
		seen[name] = true
	}
	return nil
}

// FamilySnapshot is a detached view of one metric family, used by tests and by
// JSON adapters that need the same fixed-cardinality set.
type FamilySnapshot struct {
	Name    string
	Help    string
	Type    Type
	Labels  []string
	Samples []SampleSnapshot
	Buckets []float64
}

// SampleSnapshot is one detached series.
type SampleSnapshot struct {
	Labels map[string]string
	// Value is the counter/gauge value. For histograms it is unused.
	Value float64
	// BucketCounts is cumulative for histograms (last entry is +Inf).
	BucketCounts []uint64
	Sum          float64
	Count        uint64
}

// Registry holds metric families in registration order.
type Registry struct {
	mu       sync.Mutex
	families map[string]collector
	order    []collector
	onError  func(name string, err error)
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{families: make(map[string]collector)}
}

// SetErrorHandler installs a callback for gauge-function collection errors.
// A failing gauge is skipped for that scrape; the rest of the exposition is
// still served.
func (r *Registry) SetErrorHandler(fn func(name string, err error)) {
	r.mu.Lock()
	r.onError = fn
	r.mu.Unlock()
}

func (r *Registry) collectorError(name string, err error) {
	r.mu.Lock()
	fn := r.onError
	r.mu.Unlock()
	if fn != nil {
		fn(name, err)
	}
}

func (r *Registry) register(name string, c collector) {
	if err := validateMetricName(name); err != nil {
		panic(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.families[name]; exists {
		panic(fmt.Sprintf("metrics: duplicate metric family %q", name))
	}
	r.families[name] = c
	r.order = append(r.order, c)
}

func validateMetricName(name string) error {
	if name == "" {
		return fmt.Errorf("metrics: empty metric name")
	}
	for i, ch := range name {
		valid := ch == '_' || ch == ':' ||
			(ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(i > 0 && ch >= '0' && ch <= '9')
		if !valid {
			return fmt.Errorf("metrics: invalid metric name %q", name)
		}
	}
	return nil
}

// Counter is a monotonically increasing metric.
type Counter struct {
	sample *sample
}

// Inc adds one.
func (c *Counter) Inc() { c.Add(1) }

// Add adds v; negative values are ignored (counters never decrease).
func (c *Counter) Add(v float64) {
	if c == nil || v < 0 || math.IsNaN(v) {
		return
	}
	c.sample.add(v)
}

// Value reads the current value.
func (c *Counter) Value() float64 { return c.sample.value() }

// Gauge is a metric that may increase or decrease.
type Gauge struct {
	sample *sample
}

// Set replaces the value.
func (g *Gauge) Set(v float64) { g.sample.set(v) }

// Add adds v.
func (g *Gauge) Add(v float64) { g.sample.add(v) }

// Inc adds one.
func (g *Gauge) Inc() { g.Add(1) }

// Dec subtracts one.
func (g *Gauge) Dec() { g.Add(-1) }

// Value reads the current value.
func (g *Gauge) Value() float64 { return g.sample.value() }

// CounterVec is a counter with a fixed set of label names allowed.
type CounterVec struct {
	family *family
	labels []string
}

// With returns the counter series for the given label values. The number of
// distinct combinations is the caller's responsibility; the label names are
// fixed by the whitelist.
func (v *CounterVec) With(values ...string) *Counter {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d label values, got %d",
			v.family.name, len(v.labels), len(values)))
	}
	return &Counter{sample: v.family.sampleFor(values)}
}

// Histogram is a fixed-bucket histogram without labels.
type Histogram struct {
	data *histogramData
}

// Observe records one value. NaN and infinities are ignored.
func (h *Histogram) Observe(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	h.data.observe(v)
}

// GaugeFunc is a gauge evaluated at scrape time. It is the seam for values owned
// by another subsystem (database queue depth, connection pool, Go runtime).
type GaugeFunc struct {
	name string
	help string
	fn   func(context.Context) (float64, error)
}

// CounterFunc is a counter evaluated at scrape time. It is used for cumulative
// values owned by another subsystem (for example pgxpool statistics) so the
// exposition type stays "counter" without keeping a shadow counter.
type CounterFunc struct {
	name string
	help string
	fn   func(context.Context) (float64, error)
}

// NewCounter registers a counter.
func (r *Registry) NewCounter(name, help string) *Counter {
	f := newFamily(name, help, TypeCounter, nil)
	r.register(name, f)
	return &Counter{sample: f.sampleFor(nil)}
}

// NewGauge registers a gauge.
func (r *Registry) NewGauge(name, help string) *Gauge {
	f := newFamily(name, help, TypeGauge, nil)
	r.register(name, f)
	return &Gauge{sample: f.sampleFor(nil)}
}

// NewCounterVec registers a counter with fixed label names.
func (r *Registry) NewCounterVec(name, help string, labelNames ...string) *CounterVec {
	if err := ValidateLabelNames(labelNames); err != nil {
		panic(err)
	}
	f := newFamily(name, help, TypeCounter, labelNames)
	r.register(name, f)
	return &CounterVec{family: f, labels: append([]string(nil), labelNames...)}
}

// NewHistogram registers a histogram. Buckets must be strictly increasing;
// the +Inf bucket is implicit.
func (r *Registry) NewHistogram(name, help string, buckets []float64) *Histogram {
	for i := 1; i < len(buckets); i++ {
		if buckets[i] <= buckets[i-1] {
			panic(fmt.Sprintf("metrics: histogram %q buckets must be increasing", name))
		}
	}
	if len(buckets) == 0 {
		panic(fmt.Sprintf("metrics: histogram %q needs at least one bucket", name))
	}
	h := &Histogram{data: newHistogramData(buckets)}
	f := newFamily(name, help, TypeHistogram, nil)
	f.hist = h.data
	r.register(name, f)
	return h
}

// NewGaugeFunc registers a scrape-time gauge.
func (r *Registry) NewGaugeFunc(name, help string, fn func(context.Context) (float64, error)) *GaugeFunc {
	if fn == nil {
		panic("metrics: gauge function is nil")
	}
	g := &GaugeFunc{name: name, help: help, fn: fn}
	r.register(name, gaugeCollector{g})
	return g
}

// NewCounterFunc registers a scrape-time counter.
func (r *Registry) NewCounterFunc(name, help string, fn func(context.Context) (float64, error)) *CounterFunc {
	if fn == nil {
		panic("metrics: counter function is nil")
	}
	c := &CounterFunc{name: name, help: help, fn: fn}
	r.register(name, counterCollector{c})
	return c
}

// Snapshot returns a detached fixed-cardinality view of every family.
func (r *Registry) Snapshot(ctx context.Context) []FamilySnapshot {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	order := append([]collector(nil), r.order...)
	r.mu.Unlock()
	out := make([]FamilySnapshot, 0, len(order))
	for _, c := range order {
		c.snapshot(ctx, &out)
	}
	return out
}

// WriteText writes Prometheus text exposition 0.0.4.
func (r *Registry) WriteText(ctx context.Context, w io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, collectorErrorKey{}, r.collectorError)
	r.mu.Lock()
	order := append([]collector(nil), r.order...)
	r.mu.Unlock()
	bw := bufio.NewWriterSize(w, 16<<10)
	for _, c := range order {
		if err := c.render(ctx, bw); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// collector is the internal rendering/snapshot seam.
type collector interface {
	render(ctx context.Context, w io.Writer) error
	snapshot(ctx context.Context, out *[]FamilySnapshot)
}

// family is a counter/gauge/histogram metric family.
type family struct {
	name       string
	help       string
	typ        Type
	labelNames []string
	hist       *histogramData

	mu      sync.Mutex
	samples []*sample
	index   map[string]*sample
}

func newFamily(name, help string, typ Type, labelNames []string) *family {
	return &family{
		name:       name,
		help:       help,
		typ:        typ,
		labelNames: append([]string(nil), labelNames...),
		index:      make(map[string]*sample),
	}
}

func (f *family) sampleFor(values []string) *sample {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.Join(values, "\xff")
	if s, ok := f.index[key]; ok {
		return s
	}
	s := &sample{labels: append([]string(nil), values...)}
	f.samples = append(f.samples, s)
	f.index[key] = s
	return s
}

func (f *family) render(ctx context.Context, w io.Writer) error {
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.typ); err != nil {
		return err
	}
	if f.typ == TypeHistogram {
		return f.hist.render(w, f.name)
	}
	f.mu.Lock()
	samples := append([]*sample(nil), f.samples...)
	f.mu.Unlock()
	for _, s := range samples {
		if _, err := fmt.Fprintf(w, "%s%s %s\n", f.name, labelText(f.labelNames, s.labels), formatFloat(s.value())); err != nil {
			return err
		}
	}
	return nil
}

func (f *family) snapshot(ctx context.Context, out *[]FamilySnapshot) {
	fs := FamilySnapshot{Name: f.name, Help: f.help, Type: f.typ, Labels: append([]string(nil), f.labelNames...)}
	if f.typ == TypeHistogram {
		counts, sum, count := f.hist.snapshot()
		fs.Buckets = append([]float64(nil), f.hist.bounds...)
		fs.Samples = []SampleSnapshot{{BucketCounts: counts, Sum: sum, Count: count}}
		*out = append(*out, fs)
		return
	}
	f.mu.Lock()
	samples := append([]*sample(nil), f.samples...)
	f.mu.Unlock()
	for _, s := range samples {
		labels := make(map[string]string, len(f.labelNames))
		for i, name := range f.labelNames {
			labels[name] = s.labels[i]
		}
		fs.Samples = append(fs.Samples, SampleSnapshot{Labels: labels, Value: s.value()})
	}
	*out = append(*out, fs)
}

// gaugeCollector renders a scraped gauge function.
type gaugeCollector struct {
	g *GaugeFunc
}

func (c gaugeCollector) render(ctx context.Context, w io.Writer) error {
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", c.g.name, escapeHelp(c.g.help), c.g.name); err != nil {
		return err
	}
	v, err := c.g.fn(ctx)
	if err != nil {
		collectorErrorHook(ctx, c.g.name, err)
		return nil
	}
	_, err = fmt.Fprintf(w, "%s %s\n", c.g.name, formatFloat(v))
	return err
}

func (c gaugeCollector) snapshot(ctx context.Context, out *[]FamilySnapshot) {
	fs := FamilySnapshot{Name: c.g.name, Help: c.g.help, Type: TypeGauge}
	if v, err := c.g.fn(ctx); err == nil {
		fs.Samples = []SampleSnapshot{{Value: v}}
	}
	*out = append(*out, fs)
}

// counterCollector renders a scraped cumulative counter.
type counterCollector struct {
	c *CounterFunc
}

func (c counterCollector) render(ctx context.Context, w io.Writer) error {
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.c.name, escapeHelp(c.c.help), c.c.name); err != nil {
		return err
	}
	v, err := c.c.fn(ctx)
	if err != nil {
		collectorErrorHook(ctx, c.c.name, err)
		return nil
	}
	_, err = fmt.Fprintf(w, "%s %s\n", c.c.name, formatFloat(v))
	return err
}

func (c counterCollector) snapshot(ctx context.Context, out *[]FamilySnapshot) {
	fs := FamilySnapshot{Name: c.c.name, Help: c.c.help, Type: TypeCounter}
	if v, err := c.c.fn(ctx); err == nil {
		fs.Samples = []SampleSnapshot{{Value: v}}
	}
	*out = append(*out, fs)
}

// collectorErrorHook lets WriteText report a failed gauge without coupling the
// collector to the registry. Registry sets it through context.
type collectorErrorKey struct{}

func collectorErrorHook(ctx context.Context, name string, err error) {
	if fn, ok := ctx.Value(collectorErrorKey{}).(func(string, error)); ok && fn != nil {
		fn(name, err)
	}
}

// sample is one atomic series value.
type sample struct {
	labels []string
	bits   atomic.Uint64
}

func (s *sample) add(v float64) {
	for {
		old := s.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if s.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

func (s *sample) set(v float64) {
	s.bits.Store(math.Float64bits(v))
}

func (s *sample) value() float64 {
	return math.Float64frombits(s.bits.Load())
}

// histogramData holds non-cumulative bucket counters.
type histogramData struct {
	bounds []float64
	counts []atomic.Uint64 // len(bounds)+1; the last is +Inf
	sum    atomic.Uint64
	count  atomic.Uint64
}

func newHistogramData(bounds []float64) *histogramData {
	return &histogramData{
		bounds: append([]float64(nil), bounds...),
		counts: make([]atomic.Uint64, len(bounds)+1),
	}
}

func (h *histogramData) observe(v float64) {
	idx := 0
	for idx < len(h.bounds) && v > h.bounds[idx] {
		idx++
	}
	h.counts[idx].Add(1)
	h.count.Add(1)
	for {
		old := h.sum.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if h.sum.CompareAndSwap(old, next) {
			return
		}
	}
}

func (h *histogramData) snapshot() (cumulative []uint64, sum float64, count uint64) {
	cumulative = make([]uint64, len(h.counts))
	var running uint64
	for i := range h.counts {
		running += h.counts[i].Load()
		cumulative[i] = running
	}
	return cumulative, math.Float64frombits(h.sum.Load()), h.count.Load()
}

func (h *histogramData) render(w io.Writer, name string) error {
	cumulative, sum, count := h.snapshot()
	for i, bound := range h.bounds {
		if _, err := fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", name, formatFloat(bound), cumulative[i]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, cumulative[len(cumulative)-1]); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s_sum %s\n%s_count %d\n", name, formatFloat(sum), name, count); err != nil {
		return err
	}
	return nil
}

func labelText(names, values []string) string {
	if len(names) == 0 || len(values) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(name)
		b.WriteString("=\"")
		b.WriteString(escapeLabelValue(values[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return v
}

func escapeHelp(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
