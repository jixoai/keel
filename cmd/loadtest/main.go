// Command loadtest drives the runtime at the target profile: N sessions that
// each hold an SSE stream, post periodic text events, and download images on a
// schedule with a capped client bandwidth (so the server sees realistic slow
// consumers rather than a loopback burst).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	base       string
	tenant     string
	sessions   int
	duration   time.Duration
	textEvery  time.Duration
	imageEvery time.Duration
	imageSize  int
	images     int
	clientRate int // bytes/sec per download stream, 0 = unlimited
	out        string
}

func main() {
	var c config
	flag.StringVar(&c.base, "base", "http://127.0.0.1:13740", "runtime base URL")
	flag.StringVar(&c.tenant, "tenant", "", "tenant UUID (created when empty)")
	flag.IntVar(&c.sessions, "sessions", 1000, "concurrent sessions")
	flag.DurationVar(&c.duration, "duration", 60*time.Second, "test duration")
	flag.DurationVar(&c.textEvery, "text-every", 10*time.Second, "text event period per session")
	flag.DurationVar(&c.imageEvery, "image-every", 60*time.Second, "image download period per session")
	flag.IntVar(&c.imageSize, "image-size", 1<<20, "seeded image size in bytes")
	flag.IntVar(&c.images, "images", 20, "distinct seeded images")
	flag.IntVar(&c.clientRate, "client-rate", 50<<10, "per-stream client read rate (bytes/sec, 0=unlimited)")
	flag.StringVar(&c.out, "out", "", "write JSON results here")
	flag.Parse()

	client := &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConns:        2000,
			MaxIdleConnsPerHost: 2000,
			MaxConnsPerHost:     0,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.duration+2*time.Minute)
	defer cancel()

	tenant := c.tenant
	if tenant == "" {
		var err error
		tenant, err = createTenant(ctx, client, c.base)
		if err != nil {
			log.Fatalf("create tenant: %v", err)
		}
	}
	log.Printf("tenant %s", tenant)

	sessions, err := createSessions(ctx, client, c.base, tenant, c.sessions)
	if err != nil {
		log.Fatalf("create sessions: %v", err)
	}
	images, err := seedImages(ctx, client, c.base, tenant, c.images, c.imageSize)
	if err != nil {
		log.Fatalf("seed images: %v", err)
	}
	log.Printf("seeded %d sessions, %d images of %d bytes", len(sessions), len(images), c.imageSize)

	runCtx, stop := context.WithTimeout(ctx, c.duration)
	defer stop()

	var (
		texts    atomic.Int64
		imageOK  atomic.Int64
		imageErr atomic.Int64
		sseErr   atomic.Int64
		bytesIn  atomic.Int64
		statuses sync.Map // string -> *atomic.Int64
		errTexts sync.Map // string -> *atomic.Int64
	)
	count := func(k int) {
		v, _ := statuses.LoadOrStore(k, &atomic.Int64{})
		v.(*atomic.Int64).Add(1)
	}
	countErr := func(err error) {
		if err == nil {
			return
		}
		msg := err.Error()
		if i := strings.Index(msg, "eve"); i > 0 && len(msg) > 120 {
			msg = msg[i:]
		}
		if len(msg) > 160 {
			msg = msg[len(msg)-160:]
		}
		v, _ := errTexts.LoadOrStore(msg, &atomic.Int64{})
		v.(*atomic.Int64).Add(1)
	}
	var latMu sync.Mutex
	var latencies []time.Duration

	start := time.Now()
	var wg sync.WaitGroup
	for i, sid := range sessions {
		wg.Add(1)
		go func(i int, sid string) {
			defer wg.Done()
			worker(runCtx, client, c, tenant, sid, images, i, &texts, &imageOK, &imageErr, &sseErr, &bytesIn, &latMu, &latencies, count, countErr)
		}(i, sid)
	}

	// RSS sampler: on macOS/Linux read the server's resident set if the pid is
	// available via the metrics endpoint's pid field.
	rssSamples := make(chan int64, 256)
	sampleStop := make(chan struct{})
	go sampleRSS(sampleStop, rssSamples, c.base)

	wg.Wait()
	duration := time.Since(start)
	close(sampleStop)
	close(rssSamples)

	var rss []int64
	for v := range rssSamples {
		rss = append(rss, v)
	}
	sort.Slice(rss, func(i, j int) bool { return rss[i] < rss[j] })

	latMu.Lock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50, p95, p99 := percentile(latencies, 0.50), percentile(latencies, 0.95), percentile(latencies, 0.99)
	latMu.Unlock()

	errCounts := map[string]int64{}
	errTexts.Range(func(k, v any) bool {
		errCounts[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	statusCounts := map[int]int64{}
	statuses.Range(func(k, v any) bool {
		statusCounts[k.(int)] = v.(*atomic.Int64).Load()
		return true
	})
	res := map[string]any{
		"statusCounts":      statusCounts,
		"errorCounts":       errCounts,
		"tenant":            tenant,
		"sessions":          len(sessions),
		"durationSec":       duration.Seconds(),
		"texts":             texts.Load(),
		"imagesOK":          imageOK.Load(),
		"imagesErr":         imageErr.Load(),
		"sseErrors":         sseErr.Load(),
		"bytesIn":           bytesIn.Load(),
		"throughputMBs":     float64(bytesIn.Load()) / duration.Seconds() / 1e6,
		"imageLatencyP50MS": p50.Seconds() * 1000,
		"imageLatencyP95MS": p95.Seconds() * 1000,
		"imageLatencyP99MS": p99.Seconds() * 1000,
		"rssMinMB":          mb(rssMin(rss)),
		"rssP50MB":          mb(rssPercentile(rss, 0.50)),
		"rssMaxMB":          mb(rssMax(rss)),
	}
	raw, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(raw))
	if c.out != "" {
		_ = os.WriteFile(c.out, raw, 0o644)
	}
}

func worker(ctx context.Context, client *http.Client, c config, tenant, sid string, images []string, i int,
	texts, imageOK, imageErr, sseErr, bytesIn *atomic.Int64, latMu *sync.Mutex, latencies *[]time.Duration, count func(int), countErr func(error)) {
	rng := rand.New(rand.NewSource(int64(i)*7919 + 1))
	go textLoop(ctx, client, c, tenant, sid, rng, texts)
	go imageLoop(ctx, client, c, tenant, sid, images, rng, imageOK, imageErr, bytesIn, latMu, latencies, count)
	sseLoop(ctx, client, c, tenant, sid, sseErr, count, countErr, time.Now().Add(c.duration-time.Second))
}

func textLoop(ctx context.Context, client *http.Client, c config, tenant, sid string, rng *rand.Rand, texts *atomic.Int64) {
	t := time.NewTimer(jitter(c.textEvery, rng))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			body := fmt.Sprintf(`{"text":"tick %d","requestId":"%s-%d"}`, time.Now().UnixNano(), sid, rng.Int63())
			req, _ := http.NewRequestWithContext(ctx, "POST", c.base+"/v1/sessions/"+sid+"/input", strings.NewReader(body))
			req.Header.Set("X-Tenant-ID", tenant)
			req.Header.Set("Content-Type", "application/json")
			if res, err := client.Do(req); err == nil {
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode < 300 {
					texts.Add(1)
				}
			}
			t.Reset(jitter(c.textEvery, rng))
		}
	}
}

func imageLoop(ctx context.Context, client *http.Client, c config, tenant, sid string, images []string, rng *rand.Rand,
	ok, fail, bytesIn *atomic.Int64, latMu *sync.Mutex, latencies *[]time.Duration, count func(int)) {
	// First fetch lands in the first second; the period then spreads across
	// sessions so image traffic is steady rather than a synchronized wave.
	t := time.NewTimer(time.Second + time.Duration(rng.Int63n(int64(2*time.Second))))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			id := images[rng.Intn(len(images))]
			start := time.Now()
			var n int64
			var err error
			// 429/503 is expected backpressure: honour Retry-After and try a few
			// times instead of treating it as a permanent failure.
			for attempt := 0; attempt < 6; attempt++ {
				req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/v1/files/"+id+"?format=base64", nil)
				req.Header.Set("X-Tenant-ID", tenant)
				res, derr := client.Do(req)
				if derr != nil {
					err = derr
					break
				}
				if res.StatusCode == 429 || res.StatusCode == 503 {
					count(-res.StatusCode)
					io.Copy(io.Discard, res.Body)
					res.Body.Close()
					err = fmt.Errorf("status %d", res.StatusCode)
					sleep(ctx, 300*time.Millisecond+time.Duration(rng.Int63n(int64(500*time.Millisecond))))
					continue
				}
				if res.StatusCode != 200 && res.StatusCode != 206 {
					count(-res.StatusCode)
					io.Copy(io.Discard, res.Body)
					res.Body.Close()
					err = fmt.Errorf("status %d", res.StatusCode)
					break
				}
				n, err = copyThrottled(io.Discard, res.Body, c.clientRate)
				res.Body.Close()
				break
			}
			if err != nil {
				if ctx.Err() == nil {
					fail.Add(1)
				}
			} else {
				ok.Add(1)
				bytesIn.Add(n)
				latMu.Lock()
				*latencies = append(*latencies, time.Since(start))
				latMu.Unlock()
			}
			t.Reset(jitter(c.imageEvery, rng))
		}
	}
}

func sseLoop(ctx context.Context, client *http.Client, c config, tenant, sid string, errs *atomic.Int64, count func(int), countErr func(error), quietAfter time.Time) {
	lastID := ""
	backoff := 200 * time.Millisecond
	for ctx.Err() == nil {
		url := c.base + "/v1/sessions/" + sid + "/events/stream"
		if lastID != "" {
			url += "?after=" + lastID
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.Header.Set("X-Tenant-ID", tenant)
		res, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			countErr(err)
			errs.Add(1)
			sleep(ctx, backoff)
			backoff = minDur(backoff*2, 5*time.Second)
			continue
		}
		if res.StatusCode != 200 {
			count(res.StatusCode)
			errs.Add(1)
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			sleep(ctx, backoff)
			backoff = minDur(backoff*2, 5*time.Second)
			continue
		}
		backoff = 200 * time.Millisecond
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "id: ") {
				lastID = strings.TrimPrefix(line, "id: ")
			}
		}
		res.Body.Close()
		if ctx.Err() == nil {
			// Churn in the final second is the test tearing down, not a server
			// fault; only earlier closes are counted.
			if time.Now().Before(quietAfter) {
				count(0)
				errs.Add(1)
			}
			sleep(ctx, backoff)
		}
	}
}

func copyThrottled(dst io.Writer, src io.Reader, rate int) (int64, error) {
	if rate <= 0 {
		return io.Copy(dst, src)
	}
	buf := make([]byte, 32<<10)
	var total int64
	start := time.Now()
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			want := time.Duration(float64(total) / float64(rate) * float64(time.Second))
			if sleep := want - time.Since(start); sleep > 0 {
				time.Sleep(sleep)
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

func createTenant(ctx context.Context, client *http.Client, base string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/tenants", bytes.NewReader([]byte(`{"name":"loadtest"}`)))
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		TenantID string `json:"tenantId"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.TenantID, nil
}

func createSessions(ctx context.Context, client *http.Client, base, tenant string, n int) ([]string, error) {
	out := make([]string, n)
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/sessions", bytes.NewReader([]byte(`{}`)))
			req.Header.Set("X-Tenant-ID", tenant)
			req.Header.Set("Content-Type", "application/json")
			res, err := client.Do(req)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			defer res.Body.Close()
			var body struct {
				SessionID string `json:"sessionId"`
			}
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			out[i] = body.SessionID
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func seedImages(ctx context.Context, client *http.Client, base, tenant string, n, size int) ([]string, error) {
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		data := make([]byte, size)
		for j := range data {
			data[j] = byte((j*31 + i*7) % 251)
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/files", bytes.NewReader(data))
		req.Header.Set("X-Tenant-ID", tenant)
		req.Header.Set("Content-Type", "image/png")
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		var body struct {
			FileID string `json:"fileId"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			res.Body.Close()
			return nil, err
		}
		res.Body.Close()
		ids = append(ids, body.FileID)
	}
	return ids, nil
}

func sampleRSS(stop <-chan struct{}, out chan<- int64, base string) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			pid, err := serverPID(base)
			if err != nil {
				continue
			}
			if rss := readRSS(pid); rss > 0 {
				select {
				case out <- rss:
				default:
				}
			}
		}
	}
}

func serverPID(base string) (int, error) {
	res, err := http.Get(base + "/v1/health")
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	var body struct {
		PID int `json:"pid"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return 0, err
	}
	return body.PID, nil
}

func readRSS(pid int) int64 {
	// Portable-enough: ps works on darwin and linux; RSS in KB.
	out, err := exec.Command("ps", "-o", "rss=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return 0
	}
	var kb int64
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &kb)
	return kb * 1024
}

func jitter(base time.Duration, rng *rand.Rand) time.Duration {
	if base <= 0 {
		return time.Second
	}
	return base/2 + time.Duration(rng.Int63n(int64(base)))
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func rssMin(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	return v[0]
}

func rssMax(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	return v[len(v)-1]
}

func rssPercentile(v []int64, p float64) int64 {
	if len(v) == 0 {
		return 0
	}
	return v[int(float64(len(v)-1)*p)]
}

func mb(v int64) float64 { return float64(v) / 1e6 }
