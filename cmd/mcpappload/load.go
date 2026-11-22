// load mode: the G5 driver. See the package comment in main.go.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type loadConfig struct {
	base             string
	pprof            string
	control          string
	out              string
	evidence         string
	sessions         int
	firstBatch       int
	sseCap           int
	apps             int
	runSlots         int
	bridgeRate       int
	bridgeDuration   time.Duration
	chaosDuration    time.Duration
	chaosRate        int
	cacheGenerations int
	killMin          time.Duration
	killMax          time.Duration
	dropMin          time.Duration
	dropMax          time.Duration
	backoffProbeAt   time.Duration
	probeWindow      time.Duration
	runTimeout       time.Duration
	providerID       string
	fastModel        string
	slowModel        string
}

type loadResult struct {
	StartedAt  string          `json:"startedAt"`
	FinishedAt string          `json:"finishedAt"`
	Config     resultConfig    `json:"config"`
	Sessions   int             `json:"sessions"`
	SSEStreams int             `json:"sseStreams"`
	Apps       int             `json:"apps"`
	Runs       runsResult      `json:"runs"`
	Catalog    catalogResult   `json:"catalog"`
	Heap       heapResult      `json:"heap"`
	Cache      cacheResult     `json:"cache"`
	Bridge     bridgeStats     `json:"bridge"`
	PreLimit   preLimitResult  `json:"preLimit"`
	OverLimit  overLimitResult `json:"overLimit"`
	Metrics    metricsResult   `json:"metrics"`
	Chaos      chaosResult     `json:"chaos"`
	Final      finalResult     `json:"final"`
	Failures   []string        `json:"failures"`
	OK         bool            `json:"ok"`
}

type resultConfig struct {
	Sessions         int    `json:"sessions"`
	FirstBatch       int    `json:"firstBatch"`
	SSECap           int    `json:"sseCap"`
	RunSlots         int    `json:"runSlots"`
	BridgeRate       int    `json:"bridgeRate"`
	BridgeDuration   string `json:"bridgeDuration"`
	ChaosDuration    string `json:"chaosDuration"`
	CacheGenerations int    `json:"cacheGenerations"`
}

type runsResult struct {
	Slots          int   `json:"slots"`
	MaxActive      int   `json:"maxActive"`
	MaxPending     int   `json:"maxPending"`
	InputsSent     int   `json:"inputsSent"`
	InputErrors    int   `json:"inputErrors"`
	Started        int64 `json:"started"`
	FinishedDone   int64 `json:"finishedDone"`
	FinishedFailed int64 `json:"finishedFailed"`
}

type catalogResult struct {
	At100        catalogSnapshot `json:"at100"`
	At1000       catalogSnapshot `json:"at1000"`
	AtRuns       catalogSnapshot `json:"atRuns"`
	Identical    bool            `json:"identical"`
	DriftSamples int             `json:"driftSamples"`
}

type heapResult struct {
	HeapAllocAt100MB  float64  `json:"heapAllocAt100MB"`
	HeapAllocAt1000MB float64  `json:"heapAllocAt1000MB"`
	DeltaMB           float64  `json:"deltaMB"`
	Profiles          []string `json:"profiles"`
	ProfileErrors     []string `json:"profileErrors"`
}

type cacheResult struct {
	Generations  int    `json:"generations"`
	FetchOkDelta int64  `json:"fetchOkDelta"`
	FinalHash    string `json:"finalHash"`
	Profile      string `json:"profile"`
}

type bridgeStats struct {
	Rate        int     `json:"rate"`
	DurationSec float64 `json:"durationSec"`
	Requests    int64   `json:"requests"`
	OK          int64   `json:"ok"`
	Status429   int64   `json:"status429"`
	Other       int64   `json:"other"`
	Errors      int64   `json:"errors"`
	P50MS       float64 `json:"p50Ms"`
	P95MS       float64 `json:"p95Ms"`
	P99MS       float64 `json:"p99Ms"`
	MaxMS       float64 `json:"maxMs"`
}

type preLimitResult struct {
	BridgeRateRefusals  int `json:"bridgeRateRefusals"`
	ConcurrencyRefusals int `json:"concurrencyRefusals"`
}

type overLimitResult struct {
	RateRequests         int `json:"rateRequests"`
	Rate429              int `json:"rate429"`
	RateOther            int `json:"rateOther"`
	ConcurrencyRequests  int `json:"concurrencyRequests"`
	Concurrency429       int `json:"concurrency429"`
	ConcurrencyOK        int `json:"concurrencyOK"`
	RateAuditRows        int `json:"rateAuditRows"`
	ConcurrencyAuditRows int `json:"concurrencyAuditRows"`
}

type metricsResult struct {
	SeriesAt100      int            `json:"seriesAt100"`
	SeriesAt1000     int            `json:"seriesAt1000"`
	SeriesStable     bool           `json:"seriesStable"`
	FamilyCounts     map[string]int `json:"familyCounts"`
	FinalHeapAllocMB float64        `json:"finalHeapAllocMB"`
	RunsActiveMax    int            `json:"runsActiveMax"`
}

type finalResult struct {
	Catalog           catalogSnapshot `json:"catalog"`
	Metrics           metricsResult   `json:"metrics"`
	AuditOK           int             `json:"auditOk"`
	AuditClosed       int             `json:"auditClosed"`
	AuditError        int             `json:"auditError"`
	ServerDiagnostics int             `json:"serverDiagnostics"`
}

type driver struct {
	config loadConfig
	// csrf is the X-Keel-CSRF value for writes. Even loopback local-only auth
	// requires the double-submit token (the cookie pair is issued by
	// GET /v1/auth/session).
	csrf          string
	csrfCookie    string
	api           *http.Client
	sseClient     *http.Client
	controlClient *http.Client
	result        loadResult
	sessions      []string
	apps          []appSpecView
	sseOpened     int
	sseOpenErrors int
	sseErrors     atomic.Int64
	mu            sync.Mutex
	failures      []string
}

func runLoad(args []string) (retErr error) {
	config, err := parseLoadFlags(args)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.evidence, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(),
		config.bridgeDuration+config.chaosDuration+10*time.Minute)
	defer cancel()

	d := &driver{
		config:        config,
		api:           newAPIClient(),
		sseClient:     newHTTPClient(),
		controlClient: newAPIClient(),
	}
	d.result = loadResult{
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Config: resultConfig{
			Sessions:         config.sessions,
			FirstBatch:       config.firstBatch,
			SSECap:           config.sseCap,
			RunSlots:         config.runSlots,
			BridgeRate:       config.bridgeRate,
			BridgeDuration:   config.bridgeDuration.String(),
			ChaosDuration:    config.chaosDuration.String(),
			CacheGenerations: config.cacheGenerations,
		},
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			d.fail("panic: %v", recovered)
			retErr = fmt.Errorf("load driver panic: %v", recovered)
		}
		d.finish(&retErr)
	}()

	log.Printf("phase boot: base=%s pprof=%s control=%s evidence=%s", d.config.base, d.config.pprof, d.config.control, d.config.evidence)
	if err := d.waitBoot(ctx); err != nil {
		d.fail("boot: %v", err)
		return err
	}
	if err := d.refreshCSRF(ctx); err != nil {
		d.fail("csrf: %v", err)
		return err
	}
	log.Printf("boot ok: sessions target=%d apps=%d", d.config.sessions, d.config.apps)
	if err := d.phaseScale(ctx); err != nil {
		d.fail("scale: %v", err)
		return err
	}
	log.Printf("scale snapshot ok: sessions=%d sse=%d catalogue-identical=%t", d.result.Sessions, d.result.SSEStreams, d.result.Catalog.Identical)
	if err := d.phaseLoad(ctx); err != nil {
		d.fail("load: %v", err)
		return err
	}
	log.Printf("load ok: maxActive=%d finished=%d bridgeRequests=%d", d.result.Runs.MaxActive, d.result.Runs.FinishedDone, d.result.Bridge.Requests)
	if err := d.phaseCache(ctx); err != nil {
		d.fail("cache: %v", err)
		return err
	}
	log.Printf("cache ok: generations=%d fetchOkDelta=%d", d.result.Cache.Generations, d.result.Cache.FetchOkDelta)
	if err := d.phaseOverLimit(ctx); err != nil {
		d.fail("over-limit: %v", err)
		return err
	}
	log.Printf("over-limit ok: rate429=%d concurrency429=%d", d.result.OverLimit.Rate429, d.result.OverLimit.Concurrency429)
	if config.chaosDuration > 0 {
		log.Printf("phase chaos: duration=%s", config.chaosDuration)
		if err := d.phaseChaos(ctx); err != nil {
			d.fail("chaos: %v", err)
			return err
		}
		log.Printf("chaos ok: kills=%d drops=%d reconnects=%.0f maxRecoveryMs=%d maxGapMs=%d", d.result.Chaos.Kills, d.result.Chaos.Drops, d.result.Chaos.ReconnectsDelta, d.result.Chaos.MaxRecoveryMS, d.result.Chaos.MaxListenAttemptGapMS)
	}
	if err := d.phaseFinal(ctx); err != nil {
		d.fail("final: %v", err)
		return err
	}
	return nil
}

func parseLoadFlags(args []string) (loadConfig, error) {
	var config loadConfig
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.StringVar(&config.base, "base", "http://127.0.0.1:13750", "keel serve base URL")
	fs.StringVar(&config.pprof, "pprof", "http://127.0.0.1:13751", "pprof base URL (keelperf build seam)")
	fs.StringVar(&config.control, "control", "http://127.0.0.1:13689", "fake MCP control base URL")
	fs.StringVar(&config.out, "out", "", "result JSON path")
	fs.StringVar(&config.evidence, "evidence", "", "evidence directory for heap profiles")
	fs.IntVar(&config.sessions, "sessions", 1000, "sessions to create")
	fs.IntVar(&config.firstBatch, "first-batch", 100, "sessions in the first (100) snapshot")
	fs.IntVar(&config.sseCap, "sse-cap", 256, "SSE streams to hold (keel serve control-stream cap)")
	fs.IntVar(&config.apps, "apps", 20, "expected app count")
	fs.IntVar(&config.runSlots, "run-slots", 64, "configured run concurrency")
	fs.IntVar(&config.bridgeRate, "bridge-rate", 200, "normal bridge frames per second")
	fs.DurationVar(&config.bridgeDuration, "bridge-duration", 60*time.Second, "normal bridge duration")
	fs.DurationVar(&config.chaosDuration, "chaos-duration", 10*time.Minute, "chaos duration (0 disables)")
	fs.IntVar(&config.chaosRate, "chaos-rate", 20, "bridge frames per second during chaos")
	fs.IntVar(&config.cacheGenerations, "cache-generations", 10, "document generations for the cache-bound probe")
	fs.DurationVar(&config.killMin, "kill-min", 12*time.Second, "minimum interval between SIGKILL events")
	fs.DurationVar(&config.killMax, "kill-max", 24*time.Second, "maximum interval between SIGKILL events")
	fs.DurationVar(&config.dropMin, "drop-min", 10*time.Second, "minimum interval between listen drops")
	fs.DurationVar(&config.dropMax, "drop-max", 20*time.Second, "maximum interval between listen drops")
	fs.DurationVar(&config.backoffProbeAt, "backoff-probe-at", 2*time.Minute, "time into chaos when the backoff probe starts")
	fs.DurationVar(&config.probeWindow, "probe-window", 70*time.Second, "how long listen attempts are rejected")
	fs.DurationVar(&config.runTimeout, "run-timeout", 5*time.Minute, "how long to wait for runs to settle")
	fs.StringVar(&config.providerID, "provider", "perf", "provider id in the models document")
	fs.StringVar(&config.fastModel, "fast-model", "perf-fast", "fast model id")
	fs.StringVar(&config.slowModel, "slow-model", "perf-slow", "slow model id")
	if err := fs.Parse(args); err != nil {
		return config, err
	}
	if config.out == "" {
		config.out = filepath.Join(config.evidence, "perf.json")
	}
	if config.sessions <= 0 || config.firstBatch <= 0 || config.firstBatch > config.sessions {
		return config, fmt.Errorf("-sessions and -first-batch are invalid")
	}
	if config.sseCap <= 0 || config.sseCap > config.sessions {
		config.sseCap = config.sessions
	}
	return config, nil
}

func (d *driver) fail(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	d.mu.Lock()
	d.failures = append(d.failures, message)
	d.mu.Unlock()
}

func (d *driver) failureList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.failures...)
}

func (d *driver) finish(retErr *error) {
	d.result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	d.result.Failures = d.failureList()
	d.result.OK = len(d.result.Failures) == 0
	if err := writeJSONFile(d.config.out, d.result); err != nil && *retErr == nil {
		*retErr = err
	}
	if *retErr == nil && !d.result.OK {
		*retErr = fmt.Errorf("%d acceptance failure(s): %s", len(d.result.Failures), strings.Join(d.result.Failures, "; "))
	}
}

// --- boot ----------------------------------------------------------------

type healthView struct {
	OK     bool   `json:"ok"`
	PID    int    `json:"pid"`
	BootID string `json:"bootId"`
}

func (d *driver) waitBoot(ctx context.Context) error {
	deadline := time.Now().Add(3 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		var health healthView
		status, _, err := d.getJSON(ctx, "/v1/health", &health)
		if err == nil && status == http.StatusOK && health.OK {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("health status=%d err=%v", status, err)
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr != nil {
		return lastErr
	}
	for time.Now().Before(deadline) {
		snapshot, view, err := d.catalog(ctx)
		if err == nil && snapshot.Apps == d.config.apps && view.Generation > 0 {
			d.apps = view.Apps
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("catalogue has %d apps (want %d), generation %d", snapshot.Apps, d.config.apps, view.Generation)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("the app catalogue did not publish: %v", lastErr)
}

func (d *driver) catalog(ctx context.Context) (catalogSnapshot, catalogView, error) {
	var view catalogView
	status, raw, err := d.getJSON(ctx, "/v1/mcp/apps", &view)
	if err != nil {
		return catalogSnapshot{}, view, err
	}
	if status != http.StatusOK {
		return catalogSnapshot{}, view, fmt.Errorf("GET /v1/mcp/apps: HTTP %d", status)
	}
	return snapshotCatalog(raw, view), view, nil
}

func (d *driver) fetchMetrics(ctx context.Context) (metricSnapshot, error) {
	var raw []byte
	status, body, err := d.getJSON(ctx, "/metrics", nil)
	raw = body
	if err != nil {
		return metricSnapshot{}, err
	}
	if status != http.StatusOK {
		return metricSnapshot{}, fmt.Errorf("GET /metrics: HTTP %d", status)
	}
	return parseMetrics(raw), nil
}

func (d *driver) fetchHeap(ctx context.Context, name string) error {
	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, d.config.pprof+"/debug/pprof/heap?gc=1", nil)
	if err != nil {
		return err
	}
	response, err := d.api.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("pprof heap: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	path := filepath.Join(d.config.evidence, name+".pb.gz")
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, response.Body); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	d.result.Heap.Profiles = append(d.result.Heap.Profiles, path)
	return nil
}

// --- scale ---------------------------------------------------------------

func (d *driver) phaseScale(ctx context.Context) error {
	if err := d.createSessions(ctx, d.config.firstBatch); err != nil {
		return fmt.Errorf("create the first %d sessions: %w", d.config.firstBatch, err)
	}
	d.openSSE(ctx, minInt(d.config.firstBatch, d.config.sseCap))
	time.Sleep(2 * time.Second)
	snapshot100, _, err := d.catalog(ctx)
	if err != nil {
		return err
	}
	metrics100, err := d.fetchMetrics(ctx)
	if err != nil {
		return err
	}
	if err := d.fetchHeap(ctx, "heap-100"); err != nil {
		d.result.Heap.ProfileErrors = append(d.result.Heap.ProfileErrors, "heap-100: "+err.Error())
	}
	heap100, _ := metricValue(metrics100, "go_memstats_heap_alloc_bytes")

	remaining := d.config.sessions - len(d.sessions)
	if remaining > 0 {
		if err := d.createSessions(ctx, remaining); err != nil {
			return fmt.Errorf("create the remaining %d sessions: %w", remaining, err)
		}
	}
	d.openSSE(ctx, minInt(d.config.sessions, d.config.sseCap))
	time.Sleep(2 * time.Second)
	snapshot1000, _, err := d.catalog(ctx)
	if err != nil {
		return err
	}
	metrics1000, err := d.fetchMetrics(ctx)
	if err != nil {
		return err
	}
	if err := d.fetchHeap(ctx, "heap-1000"); err != nil {
		d.result.Heap.ProfileErrors = append(d.result.Heap.ProfileErrors, "heap-1000: "+err.Error())
	}
	heap1000, _ := metricValue(metrics1000, "go_memstats_heap_alloc_bytes")

	d.result.Sessions = len(d.sessions)
	d.result.SSEStreams = d.sseOpened
	d.result.Apps = snapshot100.Apps
	d.result.Catalog.At100 = snapshot100
	d.result.Catalog.At1000 = snapshot1000
	d.result.Catalog.Identical = snapshot100.SetHash == snapshot1000.SetHash &&
		snapshot100.CanonicalBytes == snapshot1000.CanonicalBytes &&
		snapshot100.Apps == snapshot1000.Apps
	d.result.Heap.HeapAllocAt100MB = heap100 / 1e6
	d.result.Heap.HeapAllocAt1000MB = heap1000 / 1e6
	d.result.Heap.DeltaMB = (heap1000 - heap100) / 1e6
	d.result.Metrics.SeriesAt100 = len(metrics100.Series)
	d.result.Metrics.SeriesAt1000 = len(metrics1000.Series)
	d.result.Metrics.SeriesStable = sameSeries(metrics100, metrics1000)
	d.result.Metrics.FamilyCounts = metrics1000.FamilyCounts

	if len(d.sessions) != d.config.sessions {
		d.fail("sessions = %d, want %d", len(d.sessions), d.config.sessions)
	}
	if d.sseOpenErrors > 0 {
		d.fail("SSE open failures below the %d-stream cap: %d", d.config.sseCap, d.sseOpenErrors)
	}
	if !d.result.Catalog.Identical {
		d.fail("the app catalogue changed between %d and %d sessions (canonical %d vs %d bytes)",
			d.config.firstBatch, d.config.sessions, snapshot100.CanonicalBytes, snapshot1000.CanonicalBytes)
	}
	if !d.result.Metrics.SeriesStable {
		d.fail("the mcp_* series set changed between %d and %d sessions", d.config.firstBatch, d.config.sessions)
	}
	return nil
}

func (d *driver) createSessions(ctx context.Context, count int) error {
	type result struct {
		session string
		err     error
	}
	results := make(chan result, count)
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			var out struct {
				SessionID string `json:"sessionId"`
			}
			status, raw, err := d.postJSON(ctx, "/v1/sessions", map[string]any{}, &out)
			if err != nil {
				results <- result{err: err}
				return
			}
			if status != http.StatusCreated || out.SessionID == "" {
				results <- result{err: fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(string(raw)))}
				return
			}
			results <- result{session: out.SessionID}
		}()
	}
	wg.Wait()
	close(results)
	for item := range results {
		if item.err != nil {
			return item.err
		}
		d.sessions = append(d.sessions, item.session)
	}
	return nil
}

func (d *driver) openSSE(ctx context.Context, target int) {
	for d.sseOpened < target && d.sseOpened < len(d.sessions) {
		session := d.sessions[d.sseOpened]
		request, err := http.NewRequestWithContext(ctx, http.MethodGet,
			d.config.base+"/v1/sessions/"+session+"/events/stream", nil)
		if err != nil {
			d.sseOpenErrors++
			return
		}
		response, err := d.sseClient.Do(request)
		if err != nil {
			d.sseOpenErrors++
			return
		}
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 256))
			response.Body.Close()
			d.sseOpenErrors++
			d.fail("SSE stream %d: HTTP %d: %s", d.sseOpened+1, response.StatusCode, strings.TrimSpace(string(body)))
			return
		}
		d.sseOpened++
		go func(body io.ReadCloser) {
			defer body.Close()
			scanner := bufio.NewScanner(body)
			scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
			for scanner.Scan() {
			}
			if ctx.Err() == nil {
				d.sseErrors.Add(1)
			}
		}(response.Body)
	}
}

// --- load ----------------------------------------------------------------

func (d *driver) phaseLoad(ctx context.Context) error {
	if err := d.phaseRuns(ctx); err != nil {
		return err
	}
	return d.phaseBridge(ctx)
}

// phaseRuns creates one run per session and measures the configured
// concurrency. The bridge load is deliberately sequenced after this phase: a
// 1000-run queue plus 200 bridge frames/s would measure database contention,
// not the bridge contract. The sessions stay connected throughout both phases.
func (d *driver) phaseRuns(ctx context.Context) error {
	if len(d.apps) == 0 {
		return errors.New("no app catalogue")
	}
	slowCount := minInt(len(d.sessions), maxInt(d.config.runSlots*5, 320))
	var inputErr error
	var inputs atomic.Int64
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	requestID := 0
	for i := 0; i < slowCount; i++ {
		wg.Add(1)
		sem <- struct{}{}
		requestID++
		go func(index int, rid int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := d.sendInput(ctx, d.sessions[index], d.config.slowModel, fmt.Sprintf("perf-slow-%d", rid)); err != nil {
				inputErr = err
				return
			}
			inputs.Add(1)
		}(i, requestID)
	}
	wg.Wait()
	if inputErr != nil {
		d.fail("slow input: %v", inputErr)
	}
	for i := slowCount; i < len(d.sessions); i++ {
		wg.Add(1)
		sem <- struct{}{}
		requestID++
		go func(index int, rid int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := d.sendInput(ctx, d.sessions[index], d.config.fastModel, fmt.Sprintf("perf-fast-%d", rid)); err != nil {
				d.mu.Lock()
				d.result.Runs.InputErrors++
				d.mu.Unlock()
				return
			}
			inputs.Add(1)
		}(i, requestID)
	}
	wg.Wait()

	d.result.Runs.Slots = d.config.runSlots
	d.result.Runs.InputsSent = int(inputs.Load())
	if d.result.Runs.InputErrors > 0 {
		d.fail("run inputs failed: %d", d.result.Runs.InputErrors)
	}

	deadline := time.Now().Add(d.config.runTimeout)
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	var lastCatalogCheck time.Time
	capturedAtRuns := false
	done := false
	for !done && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			done = true
		case <-poll.C:
			metrics, err := d.fetchMetrics(ctx)
			if err != nil {
				continue
			}
			activeValue, _ := metricValue(metrics, "keel_runs_active")
			pendingValue, _ := metricValue(metrics, "keel_runs_pending")
			startedValue, _ := metricValue(metrics, "keel_runs_started_total")
			doneValue, _ := metricValue(metrics, `keel_runs_finished_total{status="done"}`)
			failedValue, _ := metricValue(metrics, `keel_runs_finished_total{status="failed"}`)
			active := int(activeValue)
			pending := int(pendingValue)
			started := int64(startedValue)
			finished := int64(doneValue)
			failed := int64(failedValue)
			if active > d.result.Runs.MaxActive {
				d.result.Runs.MaxActive = active
			}
			if pending > d.result.Runs.MaxPending {
				d.result.Runs.MaxPending = pending
			}
			d.result.Runs.Started = started
			d.result.Runs.FinishedDone = finished
			d.result.Runs.FinishedFailed = failed
			if !capturedAtRuns && active >= d.config.runSlots {
				capturedAtRuns = true
				if snapshot, _, err := d.catalog(ctx); err == nil {
					d.result.Catalog.AtRuns = snapshot
					if snapshot.SetHash != d.result.Catalog.At100.SetHash {
						d.fail("the app catalogue changed under the 64-run load")
					}
				}
				if err := d.fetchHeap(ctx, "heap-runs"); err != nil {
					d.result.Heap.ProfileErrors = append(d.result.Heap.ProfileErrors, "heap-runs: "+err.Error())
				}
			}
			if time.Since(lastCatalogCheck) > 2*time.Second {
				lastCatalogCheck = time.Now()
				if snapshot, _, err := d.catalog(ctx); err == nil && snapshot.Apps != d.config.apps {
					d.result.Catalog.DriftSamples++
				}
			}
			if finished >= int64(d.config.sessions) && active == 0 {
				done = true
			}
		}
	}
	if d.result.Runs.MaxActive != d.config.runSlots {
		d.fail("max concurrent runs = %d, want exactly the configured %d", d.result.Runs.MaxActive, d.config.runSlots)
	}
	if d.result.Runs.FinishedDone < int64(d.config.sessions) {
		d.fail("runs finished = %d, want %d in %s", d.result.Runs.FinishedDone, d.config.sessions, d.config.runTimeout)
	}
	if d.result.Runs.FinishedFailed > 0 {
		d.fail("runs failed = %d, want 0", d.result.Runs.FinishedFailed)
	}
	if d.result.Catalog.DriftSamples > 0 {
		d.fail("the app catalogue drifted during the run load: %d samples", d.result.Catalog.DriftSamples)
	}
	if d.result.Catalog.AtRuns.Generation == 0 {
		d.fail("the 64-run catalogue snapshot was not captured")
	}
	return nil
}

// phaseBridge saturates the HTTP proxy surface at the documented rate while
// the 1000 sessions stay connected (their runs have settled).
func (d *driver) phaseBridge(ctx context.Context) error {
	if len(d.apps) == 0 {
		return errors.New("no app catalogue")
	}
	bridgeCtx, bridgeCancel := context.WithTimeout(ctx, d.config.bridgeDuration)
	defer bridgeCancel()
	stats := d.bridgeLoad(bridgeCtx, d.config.bridgeRate, d.config.bridgeDuration, "normal")
	d.result.Bridge = stats
	if stats.DurationSec < 0.9*d.config.bridgeDuration.Seconds() {
		d.fail("bridge load ran %.1fs, want >= %.1fs", stats.DurationSec, 0.9*d.config.bridgeDuration.Seconds())
	}
	if stats.Requests < int64(0.9*float64(d.config.bridgeRate)*d.config.bridgeDuration.Seconds()) {
		d.fail("bridge requests = %d, want ~%d/s for %s", stats.Requests, d.config.bridgeRate, d.config.bridgeDuration)
	}
	if stats.Status429 > 0 {
		d.fail("normal bridge load saw %d 429s; backpressure must only appear when deliberately exceeded", stats.Status429)
	}
	if stats.Errors > 0 || stats.Other > 0 {
		d.fail("normal bridge load errors = %d, other statuses = %d", stats.Errors, stats.Other)
	}
	time.Sleep(1500 * time.Millisecond) // let sampled refusal audit rows flush
	d.result.PreLimit = d.refusalAudit(ctx)
	if d.result.PreLimit.BridgeRateRefusals > 0 || d.result.PreLimit.ConcurrencyRefusals > 0 {
		d.fail("normal load produced refusal audit rows: rate=%d concurrency=%d",
			d.result.PreLimit.BridgeRateRefusals, d.result.PreLimit.ConcurrencyRefusals)
	}
	return nil
}

func (d *driver) sendInput(ctx context.Context, session, model, requestID string) error {
	body := map[string]any{
		"text":      "G5 perf run",
		"requestId": requestID,
		"selection": map[string]any{"providerId": d.config.providerID, "modelId": model},
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	status, raw, err := d.postJSON(requestCtx, "/v1/sessions/"+session+"/input", body, nil)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(string(raw)))
	}
	return nil
}

// bridgeLoad saturates the HTTP proxy surface at rate frames/s for duration.
// The rate is a token ticker; the workers are a small pool. A frame is one
// tools/call to one app, rotating across the published catalogue.
func (d *driver) bridgeLoad(ctx context.Context, rate int, duration time.Duration, label string) bridgeStats {
	stats := bridgeStats{Rate: rate}
	if rate <= 0 || duration <= 0 {
		return stats
	}
	start := time.Now()
	jobs := make(chan int, rate)
	var dropped atomic.Int64
	schedulerDone := make(chan struct{})
	interval := time.Second / time.Duration(rate)
	if interval <= 0 {
		interval = time.Microsecond
	}
	go func() {
		defer close(schedulerDone)
		defer close(jobs)
		// A deadline loop instead of time.Ticker: Ticker coalesces missed
		// ticks under scheduling jitter, which silently lowers the achieved
		// rate (a 60 s run lost ~5% in the first G5 pass). The loop catches up
		// to the wall-clock budget, and a long pause skips the missed budget
		// rather than emitting a burst.
		next := time.Now()
		sequence := 0
		for {
			now := time.Now()
			if now.Before(next) {
				timer := time.NewTimer(next.Sub(now))
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			sequence++
			select {
			case jobs <- sequence:
			default:
				dropped.Add(1)
			}
			next = next.Add(interval)
			if now.Sub(next) > time.Second {
				next = now.Add(interval)
			}
		}
	}()

	var (
		mu         sync.Mutex
		latencies  []time.Duration
		status499  int64
		requests   int64
		ok         int64
		status429  int64
		other      int64
		errorsSeen int64
	)
	workers := 32
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		var perWorkerIndex uint64
		go func(worker int) {
			defer wg.Done()
			for sequence := range jobs {
				app := d.apps[int((perWorkerIndex+uint64(sequence))%uint64(len(d.apps)))]
				perWorkerIndex++
				tool := appToolName(app)
				body := map[string]any{"name": tool, "arguments": map[string]any{"i": sequence}}
				// In-flight frames finish after the duration ticker stops; a
				// teardown cancellation is not a bridge error.
				requestCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				requestStart := time.Now()
				status, _, err := d.postJSON(requestCtx, "/v1/mcp/apps/"+app.AppID+"/tools/call", body, nil)
				cancel()
				elapsed := time.Since(requestStart)
				mu.Lock()
				requests++
				if err != nil {
					errorsSeen++
				}
				switch {
				case status == http.StatusOK:
					ok++
				case status == http.StatusTooManyRequests:
					status429++
				case status == 499:
					status499++
				default:
					other++
				}
				latencies = append(latencies, elapsed)
				mu.Unlock()
			}
			_ = worker
		}(worker)
	}
	<-schedulerDone
	wg.Wait()
	stats.DurationSec = time.Since(start).Seconds()
	mu.Lock()
	stats.Requests = requests
	stats.OK = ok
	stats.Status429 = status429
	stats.Other = other + status499
	stats.Errors = errorsSeen
	stats.P50MS = ms(percentile(latencies, 0.50))
	stats.P95MS = ms(percentile(latencies, 0.95))
	stats.P99MS = ms(percentile(latencies, 0.99))
	stats.MaxMS = ms(percentile(latencies, 1.0))
	mu.Unlock()
	if dropped.Load() > 0 {
		d.fail("bridge %s scheduler dropped %d ticks: workers could not keep up", label, dropped.Load())
	}
	return stats
}

func appToolName(app appSpecView) string {
	for _, tool := range app.Tools {
		for _, visibility := range tool.Visibility {
			if visibility == "app" {
				return tool.Tool
			}
		}
	}
	if len(app.Tools) > 0 {
		return app.Tools[0].Tool
	}
	return ""
}

// --- cache generations ---------------------------------------------------

func (d *driver) phaseCache(ctx context.Context) error {
	if d.config.cacheGenerations <= 0 {
		return nil
	}
	before, err := d.fetchMetrics(ctx)
	if err != nil {
		return err
	}
	previous, _, err := d.catalog(ctx)
	if err != nil {
		return err
	}
	for generation := 0; generation < d.config.cacheGenerations; generation++ {
		if _, err := d.control(ctx, http.MethodPost, "/control/bump"); err != nil {
			return fmt.Errorf("bump revision %d: %w", generation+1, err)
		}
		deadline := time.Now().Add(45 * time.Second)
		advanced := false
		for time.Now().Before(deadline) {
			snapshot, _, err := d.catalog(ctx)
			if err == nil && snapshot.Apps == d.config.apps &&
				snapshot.Generation > previous.Generation && snapshot.SetHash != previous.SetHash {
				previous = snapshot
				advanced = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !advanced {
			d.fail("catalogue did not advance after document generation %d", generation+1)
			break
		}
	}
	after, err := d.fetchMetrics(ctx)
	if err != nil {
		return err
	}
	if err := d.fetchHeap(ctx, "heap-generations"); err != nil {
		d.result.Heap.ProfileErrors = append(d.result.Heap.ProfileErrors, "heap-generations: "+err.Error())
	}
	d.result.Cache.Generations = d.config.cacheGenerations
	d.result.Cache.FinalHash = previous.SetHash
	beforeFetch, _ := metricValue(before, `mcp_app_fetch_total{result="ok"}`)
	afterFetch, _ := metricValue(after, `mcp_app_fetch_total{result="ok"}`)
	d.result.Cache.FetchOkDelta = int64(afterFetch - beforeFetch)
	if d.result.Cache.FetchOkDelta < int64(d.config.cacheGenerations*d.config.apps) {
		d.fail("document fetch ok delta = %d, want >= %d", d.result.Cache.FetchOkDelta, d.config.cacheGenerations*d.config.apps)
	}
	for _, profile := range d.result.Heap.Profiles {
		if strings.HasSuffix(profile, "heap-generations.pb.gz") {
			d.result.Cache.Profile = profile
		}
	}
	return nil
}

// --- over-limit ----------------------------------------------------------

func (d *driver) phaseOverLimit(ctx context.Context) error {
	if len(d.apps) < 2 {
		return errors.New("need at least two apps for the over-limit probes")
	}
	rateApp := d.apps[0]
	concurrencyApp := d.apps[1]
	rateTool := appToolName(rateApp)
	concurrencyTool := appToolName(concurrencyApp)

	// Sequential-ish burst: two workers stay under the per-app concurrency
	// bound (4) so every 429 is a rate refusal; 500 frames in well under a
	// second exceed the 200-frame bucket.
	type outcome struct {
		status int
	}
	var mu sync.Mutex
	counts := map[int]int{}
	var wg sync.WaitGroup
	jobs := make(chan int)
	for worker := 0; worker < 2; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sequence := range jobs {
				body := map[string]any{"name": rateTool, "arguments": map[string]any{"i": sequence}}
				requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				status, _, _ := d.postJSON(requestCtx, "/v1/mcp/apps/"+rateApp.AppID+"/tools/call", body, nil)
				cancel()
				mu.Lock()
				counts[status]++
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < 500; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	d.result.OverLimit.RateRequests = 500
	d.result.OverLimit.Rate429 = counts[http.StatusTooManyRequests]
	d.result.OverLimit.RateOther = 500 - counts[http.StatusOK] - counts[http.StatusTooManyRequests]
	if d.result.OverLimit.Rate429 == 0 {
		d.fail("a 500-frame burst on one app produced no 429 rate refusal")
	}
	time.Sleep(1500 * time.Millisecond)
	d.result.OverLimit.RateAuditRows = d.auditCount(ctx, "bridge.rate", "refused")
	if d.result.OverLimit.RateAuditRows == 0 {
		d.fail("the rate refusal did not reach the audit trail")
	}

	// Let the bucket refill, then open five *slow* calls: four fit the
	// per-app concurrency bound and the fifth must be a concurrency refusal.
	time.Sleep(2 * time.Second)
	start := make(chan struct{})
	statuses := make(chan int, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			body := map[string]any{"name": concurrencyTool, "arguments": map[string]any{"delayMs": 3000}}
			requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			status, _, _ := d.postJSON(requestCtx, "/v1/mcp/apps/"+concurrencyApp.AppID+"/tools/call", body, nil)
			cancel()
			statuses <- status
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)
	d.result.OverLimit.ConcurrencyRequests = 5
	for status := range statuses {
		switch status {
		case http.StatusOK:
			d.result.OverLimit.ConcurrencyOK++
		case http.StatusTooManyRequests:
			d.result.OverLimit.Concurrency429++
		}
	}
	if d.result.OverLimit.Concurrency429 == 0 {
		d.fail("five concurrent slow calls produced no 429 concurrency refusal")
	}
	if d.result.OverLimit.ConcurrencyOK != 4 {
		d.fail("concurrent slow calls admitted = %d, want the per-app bound 4", d.result.OverLimit.ConcurrencyOK)
	}
	time.Sleep(1500 * time.Millisecond)
	d.result.OverLimit.ConcurrencyAuditRows = d.auditCount(ctx, "bridge.concurrency", "refused")
	if d.result.OverLimit.ConcurrencyAuditRows == 0 {
		d.fail("the concurrency refusal did not reach the audit trail")
	}
	return nil
}

// --- final ---------------------------------------------------------------

func (d *driver) phaseFinal(ctx context.Context) error {
	snapshot, _, err := d.catalog(ctx)
	if err != nil {
		return err
	}
	d.result.Final.Catalog = snapshot
	if snapshot.Apps != d.config.apps {
		d.fail("final catalogue has %d apps, want %d", snapshot.Apps, d.config.apps)
	}
	metrics, err := d.fetchMetrics(ctx)
	if err != nil {
		return err
	}
	connected, _ := metricValue(metrics, `mcp_servers{state="connected"}`)
	if connected < 1 {
		d.fail("mcp_servers{state=connected} = %v, want >= 1", connected)
	}
	heapAlloc, _ := metricValue(metrics, "go_memstats_heap_alloc_bytes")
	d.result.Final.Metrics = metricsResult{
		SeriesAt100:      len(metrics.Series),
		SeriesAt1000:     len(metrics.Series),
		SeriesStable:     true,
		FamilyCounts:     metrics.FamilyCounts,
		FinalHeapAllocMB: heapAlloc / 1e6,
		RunsActiveMax:    d.result.Runs.MaxActive,
	}
	d.result.Metrics.FamilyCounts = metrics.FamilyCounts
	for _, family := range mcpFamilies {
		if metrics.FamilyCounts[family] == 0 {
			d.fail("metric family %s is absent from /metrics", family)
		}
	}
	var servers struct {
		Diagnostics []json.RawMessage `json:"diagnostics"`
		Servers     []struct {
			Diagnostics []string `json:"diagnostics"`
		} `json:"servers"`
	}
	if status, raw, err := d.getJSON(ctx, "/v1/mcp/servers", &servers); err != nil || status != http.StatusOK {
		d.fail("GET /v1/mcp/servers: status=%d err=%v body=%s", status, err, strings.TrimSpace(string(raw)))
	} else {
		d.result.Final.ServerDiagnostics = len(servers.Diagnostics)
		for _, server := range servers.Servers {
			d.result.Final.ServerDiagnostics += len(server.Diagnostics)
		}
	}
	d.result.Final.AuditOK = d.auditCount(ctx, "subscription.listen", "ok")
	d.result.Final.AuditClosed = d.auditCount(ctx, "subscription.listen", "closed")
	d.result.Final.AuditError = d.auditCount(ctx, "subscription.listen", "error")
	return nil
}

// --- audit ---------------------------------------------------------------

type auditEntry struct {
	Action string `json:"action"`
	Result string `json:"result"`
	Detail string `json:"detail"`
}

func (d *driver) auditCount(ctx context.Context, action, result string) int {
	var body struct {
		Entries []auditEntry `json:"entries"`
	}
	status, raw, err := d.getJSON(ctx, "/v1/mcp/apps/audit?action="+action+"&limit=200", &body)
	if err != nil || status != http.StatusOK {
		d.fail("audit query %s: status=%d err=%v body=%s", action, status, err, strings.TrimSpace(string(raw)))
		return 0
	}
	count := 0
	for _, entry := range body.Entries {
		if entry.Result == result {
			count++
		}
	}
	return count
}

func (d *driver) refusalAudit(ctx context.Context) preLimitResult {
	return preLimitResult{
		BridgeRateRefusals:  d.auditCount(ctx, "bridge.rate", "refused"),
		ConcurrencyRefusals: d.auditCount(ctx, "bridge.concurrency", "refused"),
	}
}

// --- helpers -------------------------------------------------------------

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func randomBetween(rng *rand.Rand, min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rng.Int63n(int64(max-min)))
}
