// load mode, chaos phase: run a 10-minute random-failure window while the
// bridge traffic continues, and measure exactly what the G5 bar names:
// reconnects, the backoff cap, catalogue recompute, audit visibility and the
// runtime's own survival.
//
// The fixture exposes deterministic control endpoints (SIGKILL itself, drop
// every listen stream, reject listen attempts). The driver schedules random
// events, observes the runtime's /v1/health and /metrics, and reads the
// fixture's attempt timeline to compute the reconnect gaps. No shell timing is
// involved, so the numbers in the report are the server's own facts.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"time"
)

type chaosResult struct {
	DurationSec           float64 `json:"durationSec"`
	Kills                 int     `json:"kills"`
	Drops                 int     `json:"drops"`
	BackoffProbes         int     `json:"backoffProbes"`
	ReconnectsDelta       float64 `json:"reconnectsDelta"`
	MaxRecoveryMS         int64   `json:"maxRecoveryMs"`
	MaxDropRecoveryMS     int64   `json:"maxDropRecoveryMs"`
	MaxListenAttemptGapMS int64   `json:"maxListenAttemptGapMs"`
	ProbeRecoveryMS       int64   `json:"probeRecoveryMs"`
	HealthFailures        int     `json:"healthFailures"`
	PIDChanges            int     `json:"pidChanges"`
	CatalogBadSamples     int     `json:"catalogBadSamples"`
	CatalogSamples        int     `json:"catalogSamples"`
	CatalogHashChanges    int     `json:"catalogHashChanges"`
	RevisionChanges       int     `json:"revisionChanges"`
	BridgeOK              int64   `json:"bridgeOk"`
	BridgeErrors          int64   `json:"bridgeErrors"`
	BridgeStatus429       int64   `json:"bridgeStatus429"`
	FinalRevision         int64   `json:"finalRevision"`
	FinalApps             int     `json:"finalApps"`
	FinalListeners        int     `json:"finalListeners"`
	AuditSubscriptionRows int     `json:"auditSubscriptionRows"`
}

type chaosEvent struct {
	at   time.Time
	kind string // kill|drop
}

type chaosMonitor struct {
	driver          *driver
	events          []chaosEvent
	healthPID       int
	healthBootID    string
	healthFailures  int
	pidChanges      int
	catalogBad      int
	catalogSamples  int
	hashChanges     int
	revisionChanges int
	maxRecoveryMS   int64
	maxDropRecMS    int64
	attempts        []fixtureAttempt
	seen            map[string]bool
	firstAccepted   map[int64]bool
	lastStatus      fixtureStatus
	haveStatus      bool
	lastHash        string
	lastRevision    int64
}

func (d *driver) phaseChaos(ctx context.Context) error {
	start := time.Now()
	chaosCtx, cancel := context.WithTimeout(ctx, d.config.chaosDuration)
	defer cancel()

	status, err := d.fixtureStatus(ctx)
	if err != nil {
		return fmt.Errorf("fixture status before chaos: %w", err)
	}
	d.result.Chaos.FinalRevision = status.Revision
	startMetrics, err := d.fetchMetrics(ctx)
	if err != nil {
		return err
	}
	reconnectsStart, _ := metricValue(startMetrics, "mcp_listen_reconnects_total")
	health, err := d.health(ctx)
	if err != nil {
		return fmt.Errorf("health before chaos: %w", err)
	}
	snapshot, _, err := d.catalog(ctx)
	if err != nil {
		return err
	}
	monitor := &chaosMonitor{
		driver:        d,
		healthPID:     health.PID,
		healthBootID:  health.BootID,
		seen:          make(map[string]bool),
		firstAccepted: make(map[int64]bool),
		lastHash:      snapshot.SetHash,
		lastRevision:  status.Revision,
	}
	// The initial fixture process predates the host's first connection; its
	// start-to-first-listen span is boot latency, not crash recovery.
	monitor.firstAccepted[status.StartedAtUnixMs] = true
	monitor.recordStatus(status)

	bridgeDone := make(chan bridgeStats, 1)
	go func() {
		bridgeDone <- d.bridgeLoad(chaosCtx, d.config.chaosRate, d.config.chaosDuration, "chaos")
	}()

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	probeAt := d.config.backoffProbeAt
	if probeAt <= 0 || probeAt >= d.config.chaosDuration {
		probeAt = d.config.chaosDuration / 3
	}
	probeTotal := d.config.probeWindow + 45*time.Second
	probeEnd := probeAt + probeTotal
	events := buildChaosEvents(rng, start, d.config.chaosDuration, probeAt, probeEnd,
		d.config.killMin, d.config.killMax, d.config.dropMin, d.config.dropMax)
	nextEvent := 0
	probeDone := false
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-chaosCtx.Done():
			goto finished
		case now := <-ticker.C:
			if !probeDone && now.Sub(start) >= probeAt {
				probeDone = true
				d.runBackoffProbe(chaosCtx, monitor)
			}
			for nextEvent < len(events) && !now.Before(events[nextEvent].at) {
				event := events[nextEvent]
				nextEvent++
				if event.at.Sub(start) >= probeAt && event.at.Sub(start) < probeEnd {
					continue
				}
				switch event.kind {
				case "kill":
					d.chaosKill(chaosCtx, monitor)
				case "drop":
					d.chaosDrop(chaosCtx, monitor)
				}
			}
			d.chaosPoll(chaosCtx, monitor)
		}
	}

finished:
	bridge := <-bridgeDone
	// Wait for the fixture to come back and the catalogue to settle.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		status, err := d.fixtureStatus(ctx)
		if err == nil && status.Mode == "accept" {
			snapshot, _, err := d.catalog(ctx)
			if err == nil && snapshot.Apps == d.config.apps && status.ActiveListeners >= 1 {
				monitor.recordStatus(status)
				monitor.lastHash = snapshot.SetHash
				d.result.Chaos.FinalRevision = status.Revision
				d.result.Chaos.FinalApps = snapshot.Apps
				d.result.Chaos.FinalListeners = status.ActiveListeners
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	endMetrics, err := d.fetchMetrics(ctx)
	if err != nil {
		return err
	}
	reconnectsEnd, _ := metricValue(endMetrics, "mcp_listen_reconnects_total")

	d.result.Chaos.DurationSec = time.Since(start).Seconds()
	d.result.Chaos.Kills = monitor.countKind("kill")
	d.result.Chaos.Drops = monitor.countKind("drop")
	d.result.Chaos.BackoffProbes = monitor.countKind("probe")
	d.result.Chaos.ReconnectsDelta = reconnectsEnd - reconnectsStart
	d.result.Chaos.MaxRecoveryMS = monitor.maxRecoveryMS
	d.result.Chaos.MaxDropRecoveryMS = monitor.maxDropRecMS
	d.result.Chaos.HealthFailures = monitor.healthFailures
	d.result.Chaos.PIDChanges = monitor.pidChanges
	d.result.Chaos.CatalogBadSamples = monitor.catalogBad
	d.result.Chaos.CatalogSamples = monitor.catalogSamples
	d.result.Chaos.CatalogHashChanges = monitor.hashChanges
	d.result.Chaos.RevisionChanges = monitor.revisionChanges
	d.result.Chaos.BridgeOK = bridge.OK
	d.result.Chaos.BridgeErrors = bridge.Errors + bridge.Other
	d.result.Chaos.BridgeStatus429 = bridge.Status429
	d.result.Chaos.AuditSubscriptionRows = d.auditCount(ctx, "subscription.listen", "ok") +
		d.auditCount(ctx, "subscription.listen", "closed") +
		d.auditCount(ctx, "subscription.listen", "error")

	if d.result.Chaos.Kills < 8 {
		d.fail("chaos kills = %d, want >= 8", d.result.Chaos.Kills)
	}
	if d.result.Chaos.Drops < 8 {
		d.fail("chaos listen drops = %d, want >= 8", d.result.Chaos.Drops)
	}
	if d.result.Chaos.BackoffProbes != 1 {
		d.fail("chaos backoff probes = %d, want 1", d.result.Chaos.BackoffProbes)
	}
	if d.result.Chaos.ReconnectsDelta < float64(d.result.Chaos.Kills+d.result.Chaos.Drops) {
		d.fail("listen reconnects = %.0f, want >= kills+drops (%d)", d.result.Chaos.ReconnectsDelta,
			d.result.Chaos.Kills+d.result.Chaos.Drops)
	}
	if d.result.Chaos.MaxRecoveryMS > 35000 {
		d.fail("max reconnect recovery = %dms, want <= 35000ms", d.result.Chaos.MaxRecoveryMS)
	}
	if d.result.Chaos.MaxDropRecoveryMS > 10000 {
		d.fail("max drop recovery = %dms, want <= 10000ms", d.result.Chaos.MaxDropRecoveryMS)
	}
	if d.result.Chaos.MaxListenAttemptGapMS > 35000 {
		d.fail("max listen attempt gap = %dms, want <= 35000ms (the 30s backoff cap)",
			d.result.Chaos.MaxListenAttemptGapMS)
	}
	if d.result.Chaos.HealthFailures > 0 {
		d.fail("the runtime health endpoint failed %d times during chaos", d.result.Chaos.HealthFailures)
	}
	if d.result.Chaos.PIDChanges > 0 {
		d.fail("the runtime process changed during chaos: %d pid/boot changes", d.result.Chaos.PIDChanges)
	}
	if d.result.Chaos.CatalogBadSamples > 0 {
		d.fail("the catalogue was incomplete in %d/%d chaos samples", d.result.Chaos.CatalogBadSamples,
			d.result.Chaos.CatalogSamples)
	}
	if d.result.Chaos.CatalogHashChanges < d.result.Chaos.Kills {
		d.fail("catalogue hash changed %d times, want >= %d kills (each restart republishes new content)",
			d.result.Chaos.CatalogHashChanges, d.result.Chaos.Kills)
	}
	if d.result.Chaos.AuditSubscriptionRows == 0 {
		d.fail("no subscription.listen audit rows are visible after chaos")
	}
	if d.result.Chaos.FinalApps != d.config.apps {
		d.fail("final chaos catalogue has %d apps, want %d", d.result.Chaos.FinalApps, d.config.apps)
	}
	if d.result.Chaos.FinalListeners < 1 {
		d.fail("no listen stream was active at the end of chaos")
	}
	return nil
}

func buildChaosEvents(rng *rand.Rand, start time.Time, duration, probeAt, probeEnd, killMin, killMax, dropMin, dropMax time.Duration) []chaosEvent {
	var events []chaosEvent
	now := time.Duration(0)
	kind := "kill"
	for {
		gap := randomBetween(rng, killMin, killMax)
		if kind == "drop" {
			gap = randomBetween(rng, dropMin, dropMax)
		}
		now += gap
		if now >= duration {
			return events
		}
		if now >= probeAt && now < probeEnd {
			now = probeEnd
			continue
		}
		events = append(events, chaosEvent{at: start.Add(now), kind: kind})
		if kind == "kill" {
			kind = "drop"
		} else {
			kind = "kill"
		}
	}
}

func (m *chaosMonitor) countKind(kind string) int {
	count := 0
	for _, event := range m.events {
		if event.kind == kind {
			count++
		}
	}
	return count
}

// chaosPoll samples health, fixture status and the catalogue. It is the
// steady-state monitor between failure events.
func (d *driver) chaosPoll(ctx context.Context, monitor *chaosMonitor) {
	if err := d.chaosHealth(ctx, monitor); err != nil {
		monitor.healthFailures++
	}
	if status, err := d.fixtureStatus(ctx); err == nil {
		monitor.recordStatus(status)
	}
	if snapshot, _, err := d.catalog(ctx); err == nil {
		monitor.catalogSamples++
		if snapshot.Apps != d.config.apps {
			monitor.catalogBad++
		}
		if snapshot.SetHash != monitor.lastHash {
			monitor.hashChanges++
			monitor.lastHash = snapshot.SetHash
		}
	}
}

func (d *driver) chaosHealth(ctx context.Context, monitor *chaosMonitor) error {
	health, err := d.health(ctx)
	if err != nil {
		return err
	}
	if health.PID != monitor.healthPID || health.BootID != monitor.healthBootID {
		monitor.pidChanges++
		monitor.healthPID = health.PID
		monitor.healthBootID = health.BootID
	}
	return nil
}

func (d *driver) health(ctx context.Context) (healthView, error) {
	var health healthView
	status, _, err := d.getJSON(ctx, "/v1/health", &health)
	if err != nil {
		return health, err
	}
	if status != http.StatusOK || !health.OK {
		return health, fmt.Errorf("health HTTP %d ok=%v", status, health.OK)
	}
	return health, nil
}

func (d *driver) fixtureStatus(ctx context.Context) (fixtureStatus, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var status fixtureStatus
	raw, err := d.control(requestCtx, http.MethodGet, "/control/status")
	if err != nil {
		return status, err
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return status, err
	}
	return status, nil
}

func (m *chaosMonitor) recordStatus(status fixtureStatus) {
	if status.StartedAtUnixMs == 0 {
		return
	}
	m.haveStatus = true
	m.lastStatus = status
	if status.Revision != m.lastRevision {
		m.lastRevision = status.Revision
		m.revisionChanges++
	}
	for _, attempt := range status.ListenAttempts {
		key := fmt.Sprintf("%d|%d|%s", status.StartedAtUnixMs, attempt.UnixMs, attempt.Outcome)
		if m.seen[key] {
			continue
		}
		m.seen[key] = true
		m.attempts = append(m.attempts, attempt)
		if m.firstAccepted == nil {
			m.firstAccepted = make(map[int64]bool)
		}
		if attempt.Outcome == "accepted" && !m.firstAccepted[status.StartedAtUnixMs] {
			m.firstAccepted[status.StartedAtUnixMs] = true
			if recovery := attempt.UnixMs - status.StartedAtUnixMs; recovery > m.maxRecoveryMS {
				m.maxRecoveryMS = recovery
			}
		}
	}
	sort.Slice(m.attempts, func(i, j int) bool { return m.attempts[i].UnixMs < m.attempts[j].UnixMs })
}

func (d *driver) chaosKill(ctx context.Context, monitor *chaosMonitor) {
	before, err := d.fixtureStatus(ctx)
	if err != nil {
		monitor.healthFailures++
		return
	}
	beforeHash := monitor.lastHash
	monitor.events = append(monitor.events, chaosEvent{at: time.Now(), kind: "kill"})
	start := time.Now()
	_, _ = d.control(ctx, http.MethodPost, "/control/crash") // the process may die mid-response

	deadline := time.Now().Add(60 * time.Second)
	var restarted fixtureStatus
	restartedOK := false
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		_ = d.chaosHealth(ctx, monitor)
		status, err := d.fixtureStatus(ctx)
		if err == nil && status.StartedAtUnixMs != before.StartedAtUnixMs {
			restarted = status
			restartedOK = true
			monitor.recordStatus(status)
			break
		}
	}
	if !restartedOK {
		d.fail("the MCP fixture did not restart after SIGKILL")
		return
	}
	// First successful listen in the new process is the host's reconnect.
	var firstListen int64
	for _, attempt := range restarted.ListenAttempts {
		if attempt.Outcome == "accepted" && attempt.UnixMs >= restarted.StartedAtUnixMs {
			if firstListen == 0 || attempt.UnixMs < firstListen {
				firstListen = attempt.UnixMs
			}
		}
	}
	if firstListen > 0 {
		recovery := firstListen - restarted.StartedAtUnixMs
		if recovery > monitor.maxRecoveryMS {
			monitor.maxRecoveryMS = recovery
		}
	}
	// The catalogue must recompute to the restarted revision.
	catalogDeadline := time.Now().Add(60 * time.Second)
	changed := false
	for time.Now().Before(catalogDeadline) {
		snapshot, _, err := d.catalog(ctx)
		if err == nil && snapshot.Apps == d.config.apps && snapshot.SetHash != beforeHash {
			changed = true
			monitor.hashChanges++
			monitor.lastHash = snapshot.SetHash
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !changed {
		d.fail("the catalogue did not recompute after the MCP restart")
	} else if _, view, err := d.catalog(ctx); err == nil && len(view.Apps) > 0 {
		// The hash changed; the document must actually carry the restarted
		// revision, not just a different timestamp.
		status, body, err := d.getJSON(ctx, "/v1/mcp/apps/"+view.Apps[0].AppID+"/view", nil)
		if err != nil || status != http.StatusOK || !bytes.Contains(body, []byte(fmt.Sprintf("revision=%d", restarted.Revision))) {
			d.fail("the recomputed document does not carry revision %d (status=%d err=%v)", restarted.Revision, status, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 90*time.Second {
		d.fail("SIGKILL recovery took %s, want <= 90s", elapsed)
	}
	if status, err := d.fixtureStatus(ctx); err == nil {
		monitor.recordStatus(status)
	}
}

func (d *driver) chaosDrop(ctx context.Context, monitor *chaosMonitor) {
	startMS := time.Now().UnixMilli()
	monitor.events = append(monitor.events, chaosEvent{at: time.Now(), kind: "drop"})
	if _, err := d.control(ctx, http.MethodPost, "/control/drop"); err != nil {
		d.fail("drop listen streams: %v", err)
		return
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		status, err := d.fixtureStatus(ctx)
		if err != nil {
			continue
		}
		monitor.recordStatus(status)
		for _, attempt := range status.ListenAttempts {
			if attempt.Outcome == "accepted" && attempt.UnixMs >= startMS {
				recovery := attempt.UnixMs - startMS
				if recovery > monitor.maxDropRecMS {
					monitor.maxDropRecMS = recovery
				}
				return
			}
		}
	}
	d.fail("the host did not re-establish a listen stream within 15s after a drop")
}

func (d *driver) runBackoffProbe(ctx context.Context, monitor *chaosMonitor) {
	monitor.events = append(monitor.events, chaosEvent{at: time.Now(), kind: "probe"})
	if _, err := d.control(ctx, http.MethodPost, "/control/mode?listen=reject"); err != nil {
		d.fail("backoff probe: reject mode: %v", err)
		return
	}
	if _, err := d.control(ctx, http.MethodPost, "/control/drop"); err != nil {
		d.fail("backoff probe: drop: %v", err)
		return
	}
	startMS := time.Now().UnixMilli()
	probeDeadline := time.Now().Add(d.config.probeWindow)
	seenAttempts := make(map[int64]bool)
	var attempts []fixtureAttempt
	for time.Now().Before(probeDeadline) {
		time.Sleep(500 * time.Millisecond)
		_ = d.chaosHealth(ctx, monitor)
		status, err := d.fixtureStatus(ctx)
		if err != nil {
			continue
		}
		monitor.recordStatus(status)
		for _, attempt := range status.ListenAttempts {
			if attempt.UnixMs >= startMS && !seenAttempts[attempt.UnixMs] {
				seenAttempts[attempt.UnixMs] = true
				attempts = append(attempts, attempt)
			}
		}
	}
	if _, err := d.control(ctx, http.MethodPost, "/control/mode?listen=accept"); err != nil {
		d.fail("backoff probe: accept mode: %v", err)
		return
	}
	acceptMS := time.Now().UnixMilli()
	recoveryDeadline := time.Now().Add(45 * time.Second)
	probeRecovery := int64(0)
	for time.Now().Before(recoveryDeadline) {
		time.Sleep(500 * time.Millisecond)
		_ = d.chaosHealth(ctx, monitor)
		status, err := d.fixtureStatus(ctx)
		if err != nil {
			continue
		}
		monitor.recordStatus(status)
		for _, attempt := range status.ListenAttempts {
			if attempt.Outcome == "accepted" && attempt.UnixMs >= acceptMS {
				probeRecovery = attempt.UnixMs - acceptMS
				break
			}
		}
		if probeRecovery > 0 {
			break
		}
	}
	if probeRecovery == 0 {
		d.fail("the host did not recover after the backoff probe")
	}
	if probeRecovery > 35000 {
		d.fail("backoff probe recovery = %dms, want <= 35000ms", probeRecovery)
	}
	d.result.Chaos.ProbeRecoveryMS = probeRecovery

	// The attempt timeline inside the reject window is the backoff schedule.
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].UnixMs < attempts[j].UnixMs })
	var maxGap int64
	for i := 1; i < len(attempts); i++ {
		gap := attempts[i].UnixMs - attempts[i-1].UnixMs
		if gap > maxGap {
			maxGap = gap
		}
	}
	if maxGap == 0 {
		d.fail("the backoff probe observed fewer than two listen attempts")
	}
	if maxGap > 35000 {
		d.fail("backoff probe observed a %dms listen gap, want <= 35000ms", maxGap)
	}
	d.result.Chaos.MaxListenAttemptGapMS = maxGap
}
