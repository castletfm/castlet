package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Password-login brute-force speed bump. Castlet targets closed environments
// (intranet/homelab), so this is a small in-memory, per-client-IP fixed-window
// limiter rather than a distributed rate limiter: after loginRateLimitMax failed
// attempts from an IP within loginRateLimitWindow, further attempts are refused
// with 429 until the window elapses. A successful login clears the counter.
const (
	// loginRateLimitMax is the number of failed password-login attempts allowed
	// from one client IP within a single window before further attempts are
	// throttled.
	loginRateLimitMax = 5

	// loginRateLimitWindow is the span over which failed attempts are counted
	// and, once the limit is hit, the span an IP stays throttled.
	loginRateLimitWindow = time.Minute

	// loginLimiterMaxEntries caps how many IPs the limiter tracks at once. If a
	// sweep cannot bring the map back under this cap (e.g. a flood of distinct
	// source IPs), the whole map is dropped so memory can never grow unbounded;
	// the worst case is that a burst of live counters is forgiven at once.
	loginLimiterMaxEntries = 10000
)

// loginBucket is one IP's fixed-window failure counter.
type loginBucket struct {
	count int
	// resetAt is when the current window ends; the counter is stale (and the
	// entry evictable) once now is at or past it.
	resetAt time.Time
}

// loginLimiter is a mutex-guarded, in-memory fixed-window rate limiter keyed by
// client IP. The zero value is not usable; construct it with newLoginLimiter.
type loginLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*loginBucket
	max        int
	window     time.Duration
	maxEntries int
	// lastSweep throttles the opportunistic stale-entry sweep so a busy login
	// path does not walk the whole map on every request.
	lastSweep time.Time
}

func newLoginLimiter(maxFailures int, window time.Duration, maxEntries int) *loginLimiter {
	return &loginLimiter{
		buckets:    make(map[string]*loginBucket),
		max:        maxFailures,
		window:     window,
		maxEntries: maxEntries,
	}
}

// blocked reports whether key is currently throttled and, if so, how long until
// its window elapses. An expired window never blocks.
func (l *loginLimiter) blocked(key string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[key]
	if b == nil || !now.Before(b.resetAt) {
		return 0, false
	}
	if b.count < l.max {
		return 0, false
	}
	return b.resetAt.Sub(now), true
}

// fail records one failed attempt for key, starting a fresh window if the
// previous one has elapsed (or never existed). It also opportunistically evicts
// stale entries so the map cannot grow without bound.
func (l *loginLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	b := l.buckets[key]
	if b == nil || !now.Before(b.resetAt) {
		l.buckets[key] = &loginBucket{count: 1, resetAt: now.Add(l.window)}
		return
	}
	b.count++
}

// reset clears any counter for key, e.g. after a successful login.
func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// sweepLocked removes expired entries. It runs at most once per window unless
// the map has hit the hard cap, in which case it always runs and, if the map is
// still over the cap afterward, drops everything to guarantee bounded memory.
// The caller must hold l.mu.
func (l *loginLimiter) sweepLocked(now time.Time) {
	over := len(l.buckets) >= l.maxEntries
	if !over && now.Sub(l.lastSweep) < l.window {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if !now.Before(b.resetAt) {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= l.maxEntries {
		// Even live windows exceed the cap (a flood of distinct IPs): forgive
		// them all rather than let the map grow unbounded.
		clear(l.buckets)
	}
}

// clientIP extracts the client's IP from RemoteAddr. Castlet has no
// trusted-proxy configuration, so forwarded headers (which a client can forge)
// are deliberately ignored; the transport-level peer address is the only
// trustworthy source. If RemoteAddr has no port, it is used verbatim.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
