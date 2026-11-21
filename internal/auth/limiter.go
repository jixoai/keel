// Login throttling (change keel-bot-identity-access, A2).
//
// Two dimensions are tracked independently: the attempted username (normalized
// case) and the caller's IP. A login is refused while either dimension is
// locked. The IP is always the transport peer (RemoteAddr); X-Forwarded-For is
// never consulted, so a client cannot reset its bucket by spoofing a header.
//
// The delay is exponential after a small free-attempt allowance and capped at
// 15 minutes. Nothing sleeps inside a request: a locked attempt gets 429 with
// Retry-After and returns immediately, which also keeps a lockout from becoming
// a cheap way to hold server goroutines.
package auth

import (
	"strings"
	"sync"
	"time"
)

const (
	// limiterFreeAttempts is how many consecutive failures are answered without
	// a lock. The next failure starts the exponential ladder.
	limiterFreeAttempts = 3
	// limiterBaseDelay is the first lock duration (after free attempts).
	limiterBaseDelay = 1 * time.Second
	// limiterMaxDelay is the cap (design §2.2: at most 15 minutes).
	limiterMaxDelay = 15 * time.Minute
	// limiterFailureWindow forgets a dimension that has been idle this long.
	limiterFailureWindow = 1 * time.Hour
	// limiterSweepAt bounds the map when many distinct usernames/IPs fail.
	limiterSweepAt = 4096
)

// Limiter is the in-process username+IP failure tracker. It is process-local by
// design (the alpha deployment is a single writer); a multi-replica deployment
// would need the same table-backed contract and is a separate change.
type Limiter struct {
	mu      sync.Mutex
	entries map[string]*limitEntry
	now     func() time.Time
	ops     int
}

type limitEntry struct {
	failures    int
	lastFailure time.Time
	lockedUntil time.Time
}

// NewLimiter builds a limiter whose clock defaults to time.Now. Tests inject a
// clock through SetClock.
func NewLimiter() *Limiter {
	return &Limiter{entries: make(map[string]*limitEntry), now: func() time.Time { return time.Now().UTC() }}
}

// SetClock replaces the clock (tests only). It must be called before concurrent
// use.
func (l *Limiter) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// Check reports how long the caller must wait before this username/IP pair may
// attempt a login again. Zero means "allowed now".
func (l *Limiter) Check(username, ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	var wait time.Duration
	for _, key := range []string{usernameKey(username), ipKey(ip)} {
		if key == "" {
			continue
		}
		entry := l.entries[key]
		if entry == nil {
			continue
		}
		if now.Sub(entry.lastFailure) > limiterFailureWindow && !now.Before(entry.lockedUntil) {
			delete(l.entries, key)
			continue
		}
		if remaining := entry.lockedUntil.Sub(now); remaining > wait {
			wait = remaining
		}
	}
	return wait
}

// Failure records one failed attempt and returns the lock that now applies
// (zero when still inside the free-attempt allowance).
func (l *Limiter) Failure(username, ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.ops++
	l.sweepLocked(now)
	var longest time.Duration
	for _, key := range []string{usernameKey(username), ipKey(ip)} {
		if key == "" {
			continue
		}
		entry := l.entries[key]
		if entry == nil {
			entry = &limitEntry{}
			l.entries[key] = entry
		}
		if now.Sub(entry.lastFailure) > limiterFailureWindow {
			entry.failures = 0
		}
		entry.failures++
		entry.lastFailure = now
		if entry.failures >= limiterFreeAttempts {
			entry.lockedUntil = now.Add(limiterDelay(entry.failures))
		}
		if remaining := entry.lockedUntil.Sub(now); remaining > longest {
			longest = remaining
		}
	}
	return longest
}

// Success clears the username dimension and decrements the IP dimension by one:
// a legitimate login from a shared address should not leave that NAT locked
// forever, but it must not wipe an attack that is still in progress.
func (l *Limiter) Success(username, ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if key := usernameKey(username); key != "" {
		delete(l.entries, key)
	}
	if key := ipKey(ip); key != "" {
		entry := l.entries[key]
		if entry != nil {
			if entry.failures > 0 {
				entry.failures--
			}
			if entry.failures < limiterFreeAttempts {
				entry.lockedUntil = time.Time{}
			}
			if entry.failures == 0 {
				delete(l.entries, key)
			}
		}
	}
}

// limiterDelay is the exponential ladder: 1s, 2s, 4s, ... capped at 15m.
func limiterDelay(failures int) time.Duration {
	if failures <= limiterFreeAttempts {
		return 0
	}
	steps := failures - limiterFreeAttempts - 1
	if steps >= 20 { // 1s << 20 already exceeds the cap
		return limiterMaxDelay
	}
	delay := limiterBaseDelay << uint(steps)
	if delay > limiterMaxDelay {
		return limiterMaxDelay
	}
	return delay
}

func (l *Limiter) sweepLocked(now time.Time) {
	if len(l.entries) < limiterSweepAt && l.ops%1024 != 0 {
		return
	}
	for key, entry := range l.entries {
		if now.Sub(entry.lastFailure) > limiterFailureWindow && !now.Before(entry.lockedUntil) {
			delete(l.entries, key)
		}
	}
}

func usernameKey(username string) string {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return ""
	}
	return "u:" + username
}

func ipKey(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	return "i:" + ip
}
