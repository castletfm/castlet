package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoginLimiter(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("throttles after max failures", func(t *testing.T) {
		l := newLoginLimiter(3, time.Minute, 100)
		now := base
		for i := range 3 {
			_, blocked := l.blocked("ip", now)
			require.False(t, blocked, "attempt %d must be allowed", i)
			l.fail("ip", now)
		}
		retryAfter, blocked := l.blocked("ip", now)
		require.True(t, blocked, "must be throttled after hitting the limit")
		require.Equal(t, time.Minute, retryAfter)
	})

	t.Run("success resets the counter", func(t *testing.T) {
		l := newLoginLimiter(3, time.Minute, 100)
		now := base
		l.fail("ip", now)
		l.fail("ip", now)
		l.fail("ip", now)
		_, blocked := l.blocked("ip", now)
		require.True(t, blocked)

		l.reset("ip")
		_, blocked = l.blocked("ip", now)
		require.False(t, blocked, "reset must clear the counter")
	})

	t.Run("different keys are independent", func(t *testing.T) {
		// A different window here also exercises the limiter's configurability.
		l := newLoginLimiter(3, 30*time.Second, 100)
		now := base
		for range 3 {
			l.fail("a", now)
		}
		_, blockedA := l.blocked("a", now)
		require.True(t, blockedA)
		_, blockedB := l.blocked("b", now)
		require.False(t, blockedB, "key b must be unaffected by key a")
	})

	t.Run("window expires", func(t *testing.T) {
		l := newLoginLimiter(3, time.Minute, 100)
		now := base
		for range 3 {
			l.fail("ip", now)
		}
		_, blocked := l.blocked("ip", now)
		require.True(t, blocked)

		// Once the window elapses the block lifts and a fresh window starts.
		later := now.Add(time.Minute)
		_, blocked = l.blocked("ip", later)
		require.False(t, blocked, "block must lift after the window elapses")
	})

	t.Run("stale entries are swept", func(t *testing.T) {
		l := newLoginLimiter(3, time.Minute, 100)
		now := base
		l.fail("old", now)
		require.Len(t, l.buckets, 1)

		// A failure a full window later triggers the sweep, evicting the stale
		// "old" entry while recording the new one.
		later := now.Add(2 * time.Minute)
		l.fail("new", later)
		_, ok := l.buckets["old"]
		require.False(t, ok, "stale entry must be evicted")
		require.Len(t, l.buckets, 1)
	})

	t.Run("hard cap bounds memory", func(t *testing.T) {
		l := newLoginLimiter(3, time.Minute, 4)
		now := base
		// All within one window (so none are stale): once the live map reaches
		// the cap it is dropped wholesale rather than growing unbounded.
		l.fail("a", now)
		l.fail("b", now)
		l.fail("c", now)
		l.fail("d", now)
		require.LessOrEqual(t, len(l.buckets), l.maxEntries)
	})
}

func TestClientIP(t *testing.T) {
	t.Run("host:port", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/login", nil)
		r.RemoteAddr = "203.0.113.7:54321"
		require.Equal(t, "203.0.113.7", clientIP(r))
	})
	t.Run("no port", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/login", nil)
		r.RemoteAddr = "203.0.113.7"
		require.Equal(t, "203.0.113.7", clientIP(r))
	})
	t.Run("forwarded header ignored", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/login", nil)
		r.RemoteAddr = "203.0.113.7:1234"
		r.Header.Set("X-Forwarded-For", "10.0.0.1")
		require.Equal(t, "203.0.113.7", clientIP(r))
	})
}
