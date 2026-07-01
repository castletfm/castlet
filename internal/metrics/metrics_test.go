package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/castletfm/castlet/internal/metrics"
	"github.com/stretchr/testify/require"
)

func TestRegistryExposition(t *testing.T) {
	r := metrics.New()
	r.Register("http_requests_total", metrics.Counter, "Total HTTP requests.")
	r.Register("worker_last_success_timestamp_seconds", metrics.Gauge, "Last success.")

	// Counters accumulate per label set; identical label sets (regardless of the
	// order they are supplied in) map to the same series.
	r.Inc("http_requests_total", "route", "GET /", "status", "200")
	r.Inc("http_requests_total", "status", "200", "route", "GET /")
	r.Inc("http_requests_total", "route", "GET /", "status", "500")
	r.Set("worker_last_success_timestamp_seconds", 1700000000)
	r.GaugeFunc("queue_pending_jobs", "Pending jobs.", func() float64 { return 3 })

	var b strings.Builder
	_, err := r.WriteTo(&b)
	require.NoError(t, err)
	out := b.String()

	require.Contains(t, out, "# HELP http_requests_total Total HTTP requests.")
	require.Contains(t, out, "# TYPE http_requests_total counter")
	// Labels are rendered sorted by key, so the series key is canonical.
	require.Contains(t, out, `http_requests_total{route="GET /",status="200"} 2`)
	require.Contains(t, out, `http_requests_total{route="GET /",status="500"} 1`)
	require.Contains(t, out, "# TYPE worker_last_success_timestamp_seconds gauge")
	require.Contains(t, out, "worker_last_success_timestamp_seconds 1700000000")
	require.Contains(t, out, "# TYPE queue_pending_jobs gauge")
	require.Contains(t, out, "queue_pending_jobs 3")
}

func TestRegistryGaugeSetAndFractional(t *testing.T) {
	r := metrics.New()
	r.Set("temp", 2.5)
	r.Set("temp", 4.5) // Set overwrites rather than accumulates.
	r.Add("bytes_total", 1.5)
	r.Add("bytes_total", 0.25)

	var b strings.Builder
	_, err := r.WriteTo(&b)
	require.NoError(t, err)
	out := b.String()
	require.Contains(t, out, "temp 4.5")
	require.Contains(t, out, "bytes_total 1.75")
}

func TestRegistryLabelEscaping(t *testing.T) {
	r := metrics.New()
	r.Inc("evt_total", "detail", `a"b\c`+"\n")
	var b strings.Builder
	_, err := r.WriteTo(&b)
	require.NoError(t, err)
	require.Contains(t, b.String(), `evt_total{detail="a\"b\\c\n"} 1`)
}

func TestRegistryServeHTTP(t *testing.T) {
	r := metrics.New()
	r.Inc("hits_total")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
	require.Contains(t, rec.Body.String(), "hits_total 1")
}
