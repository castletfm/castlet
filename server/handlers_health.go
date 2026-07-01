package server

import (
	"io"
	"net/http"
)

// handleHealthz is the liveness probe: it is cheap and always 200 as long as
// the process can accept and serve a request. It performs no dependency checks,
// so a process supervisor can distinguish "up" from "wedged" without penalising
// it for a transient backend outage.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeProbe(w, http.StatusOK, "ok")
}

// handleReadyz is the readiness probe: it returns 200 only when the metadata
// store is reachable and 503 otherwise, so a load balancer routes traffic away
// from an instance that has lost its database.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		s.logger.Warn("readiness check failed", "error", err)
		writeProbe(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeProbe(w, http.StatusOK, "ok")
}

// writeProbe writes a plain-text probe response with the given status and body.
func writeProbe(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body+"\n")
}
