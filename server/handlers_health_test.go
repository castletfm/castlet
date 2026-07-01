package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHealthz checks the liveness probe: it is unauthenticated and always 200,
// and still carries the baseline security headers.
func TestHealthz(t *testing.T) {
	h := newHarness(t)
	resp, body := h.get(t, "/healthz")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "ok")
	requireSecurityHeaders(t, resp)
}

// TestReadyz checks the readiness probe: 200 while the store is reachable, and
// 503 once it is not (here, by closing the underlying database).
func TestReadyz(t *testing.T) {
	h := newHarness(t)

	resp, _ := h.get(t, "/readyz")
	require.Equal(t, http.StatusOK, resp.StatusCode, "a healthy store is ready")
	requireSecurityHeaders(t, resp)

	require.NoError(t, h.store.Close())

	resp, _ = h.get(t, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
		"an unreachable store must report not-ready")
}

// TestPathCleanupRedirectHeaders checks that a redirect the root mux generates
// itself for a non-canonical path (here //healthz, cleaned to /healthz) still
// carries the baseline security headers, since securityHeaders wraps the mux.
func TestPathCleanupRedirectHeaders(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.get(t, "//healthz")
	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	require.Equal(t, "/healthz", resp.Header.Get("Location"))
	requireSecurityHeaders(t, resp)
}
