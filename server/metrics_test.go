package server_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMetricsEndpoint(t *testing.T) {
	h := newHarness(t)

	// A scrape before any application traffic already exposes the declared
	// families (with their HELP/TYPE headers) and the queue-depth gauge.
	body := scrapeMetrics(t, h)
	require.Contains(t, body, "# TYPE http_requests_total counter")
	require.Contains(t, body, "# TYPE http_request_duration_seconds_sum counter")
	require.Contains(t, body, "# TYPE http_request_duration_seconds_count counter")
	require.Contains(t, body, "# TYPE queue_pending_jobs gauge")
	require.Contains(t, body, "queue_pending_jobs 0")

	// Drive one request through the app chain, then confirm the counter for its
	// route pattern (not the raw path) is reflected on the next scrape.
	resp, err := h.client.Get(h.base + "/")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body = scrapeMetrics(t, h)
	require.Contains(t, body, `http_requests_total{route="GET /{$}",status="200"} 1`)
	require.Contains(t, body, `http_request_duration_seconds_count{route="GET /{$}"} 1`)
	require.Contains(t, body, `http_request_duration_seconds_sum{route="GET /{$}"}`)
}

// scrapeMetrics fetches /metrics and returns its body. The endpoint itself is
// mounted outside the instrumented app mux, so scraping does not perturb the
// HTTP counters it reports.
func scrapeMetrics(t *testing.T, h *harness) string {
	t.Helper()
	resp, err := h.client.Get(h.base + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain"))
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}
