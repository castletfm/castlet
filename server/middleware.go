package server

import (
	"context"
	"net/http"

	"github.com/castletfm/castlet/model"
)

type ctxKey int

const userCtxKey ctxKey = iota

// loadUser resolves the session cookie to a user and stores it in the request
// context for downstream handlers. It never blocks the request: an absent or
// invalid session simply leaves the context user nil.
func (s *Server) loadUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uid, ok := s.sessions.UserID(r, s.now()); ok {
			if u, err := s.store.UserByID(r.Context(), uid); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userCtxKey, u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// userFrom returns the authenticated user in ctx, or nil.
func userFrom(ctx context.Context) *model.User {
	u, _ := ctx.Value(userCtxKey).(*model.User)
	return u
}

// requireAuth guards admin handlers, redirecting unauthenticated requests to
// the login page. It relies on loadUser having already run.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r.Context()) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// logRequests logs one line per request after it completes.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status)
	})
}

// statusWriter captures the response status code for logging.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}
