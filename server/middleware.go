package server

import (
	"context"
	"errors"
	"net/http"
	"runtime/debug"

	"github.com/castletfm/castlet/model"
)

type ctxKey int

const userCtxKey ctxKey = iota

// loadUser resolves the session cookie to a user and stores it in the request
// context for downstream handlers. It never blocks the request: an absent or
// invalid session simply leaves the context user nil.
func (s *Server) loadUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uid, epoch, ok := s.sessions.UserID(r, s.now()); ok {
			// The cookie's epoch must still match the user's current epoch;
			// bumping it (logout / password change) invalidates older sessions.
			if u, err := s.store.UserByID(r.Context(), uid); err == nil && u.SessionEpoch == epoch {
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

// recoverPanic converts a panic in any downstream handler into a logged 500
// response instead of letting it propagate and crash the process. It is the
// per-request safety net only: the process-level "let it crash" policy for
// other goroutines is intentionally left intact. http.ErrAbortHandler is
// re-panicked per the net/http convention so the server can abort the response.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// Re-panic ErrAbortHandler per the net/http convention. Detect it
			// via errors.Is behind an error type-assert: a panic value can be
			// uncomparable (e.g. a map or slice), and a bare == would itself
			// panic, defeating the recovery.
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			s.logger.Error("panic recovered", "method", r.Method, "path", r.URL.Path,
				"panic", rec, "stack", string(debug.Stack()))
			// If the handler already committed a status or body, the response is
			// past the point of no return: the status is on the wire and a 500
			// page would just be appended to a partial response. Abort the
			// connection (net/http closes it on ErrAbortHandler) rather than
			// write a bogus error page. logRequests installs the statusWriter, so
			// in the real chain w is always one; when it is not (nothing could
			// have been recorded) fall through to the normal 500.
			if sw, ok := w.(*statusWriter); ok && sw.wrote {
				panic(http.ErrAbortHandler)
			}
			s.renderError(w, r, http.StatusInternalServerError, "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets baseline security response headers on every route. It
// runs before the handler, so per-route headers set later (e.g. the media
// endpoint's Content-Disposition) are untouched; setting nosniff again there is
// idempotent.
//
// No Content-Security-Policy is set on purpose: the app is server-rendered and
// relies on small inline <script> blocks for progressive enhancement (see
// web/templates/episode.html and admin_episodes.html), and /media 302-redirects
// to presigned URLs on arbitrary object-store origins. A meaningful CSP would
// have to allow 'unsafe-inline' for scripts and whitelist those origins, which
// buys little; omitting it keeps the existing pages working.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// logRequests logs one line per request after it completes.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status)
	})
}

// statusWriter captures the response status code for logging and records
// whether anything (a header or body byte) has been committed, so recoverPanic
// can tell an intact response apart from one that can no longer be replaced.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	wrote       bool
}

func (w *statusWriter) WriteHeader(code int) {
	w.wrote = true
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Unwrap exposes the wrapped ResponseWriter so http.ResponseController can
// traverse the chain to the underlying connection. Without it, optional
// capabilities like SetReadDeadline (used by the upload handler) would resolve
// to http.ErrNotSupported and silently no-op.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// underlying follows the Unwrap chain to the innermost ResponseWriter, i.e.
// net/http's real *response. It exists for http.MaxBytesReader, which — unlike
// http.ResponseController — does NOT follow Unwrap: it only fires net/http's
// internal oversized-body hook (which flags the connection to close and skip
// draining the rest of the body) when handed the concrete *response. Passing it
// a wrapping statusWriter would silently defeat that hook, so the upload handler
// unwraps first.
func underlying(w http.ResponseWriter) http.ResponseWriter {
	for {
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return w
		}
		w = u.Unwrap()
	}
}
