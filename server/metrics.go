package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/castletfm/castlet/internal/metrics"
)

// Metric names exposed at /metrics. HTTP names follow Prometheus conventions;
// the duration is exposed as the standard _sum/_count pair (an implicit
// summary) so rate(sum)/rate(count) yields the average request latency.
const (
	metricHTTPRequests    = "http_requests_total"
	metricHTTPDurationSum = "http_request_duration_seconds_sum"
	metricHTTPDurationCnt = "http_request_duration_seconds_count"
	metricQueuePending    = "queue_pending_jobs"
)

// registerMetrics declares the server-owned metric families (so their HELP/TYPE
// headers are populated even before the first request) and wires the queue-depth
// gauge, whose value is read from the store at scrape time.
func (s *Server) registerMetrics() {
	s.metrics.Register(metricHTTPRequests, metrics.Counter, "Total HTTP requests by route pattern and status.")
	s.metrics.Register(metricHTTPDurationSum, metrics.Counter, "Sum of HTTP request durations in seconds by route pattern.")
	s.metrics.Register(metricHTTPDurationCnt, metrics.Counter, "Count of HTTP requests observed for the duration sum, by route pattern.")
	s.metrics.GaugeFunc(metricQueuePending, "Jobs currently pending in the queue.", func() float64 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		n, err := s.store.CountPendingJobs(ctx)
		if err != nil {
			s.logger.Warn("metrics: count pending jobs", "error", err)
			return 0
		}
		return float64(n)
	})
}

// handleMetrics serves the current metrics snapshot in Prometheus text format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.metrics.ServeHTTP(w, r)
}

// instrumentHTTP records request count and duration per matched route pattern.
// It resolves the pattern via the app mux (r.URL.Path alone carries opaque ids,
// which would explode label cardinality), so the label is the low-cardinality
// route template, e.g. "GET /e/{id}/{$}".
func (s *Server) instrumentHTTP(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		elapsed := time.Since(start).Seconds()

		route := routePattern(mux, r)
		s.metrics.Inc(metricHTTPRequests, "route", route, "status", strconv.Itoa(sw.status))
		s.metrics.Add(metricHTTPDurationSum, elapsed, "route", route)
		s.metrics.Inc(metricHTTPDurationCnt, "route", route)
	})
}

// routePattern returns the mux pattern that matches r, or "other" when nothing
// does (so an unmatched flood cannot create unbounded label series).
func routePattern(mux *http.ServeMux, r *http.Request) string {
	if _, pattern := mux.Handler(r); pattern != "" {
		return pattern
	}
	return "other"
}
