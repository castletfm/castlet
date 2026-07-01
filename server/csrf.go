package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"mime"
	"net/http"
)

const (
	// csrfCookieName holds the per-browser CSRF secret. It is a double-submit
	// token: the same value is embedded in every state-changing form (as the
	// csrfField input, or the query string for multipart uploads) and compared
	// against this cookie on unsafe requests. A cross-site POST cannot read the
	// cookie to echo it back, so the tokens will not match.
	csrfCookieName = "castlet_csrf"
	// csrfField is the form field / query parameter carrying the token back.
	csrfField = "csrf_token"
	// csrfHeader lets a fetch()-based caller submit the token without a form.
	// Stored in canonical MIME form; http.Header.Get canonicalizes the caller's
	// "X-CSRF-Token" to this, so browser-sent headers still match.
	csrfHeader = "X-Csrf-Token"
	// csrfTokenBytes is the raw entropy behind a token before base64 encoding.
	csrfTokenBytes = 32
)

// csrfCtxKeyType keys the per-request CSRF token in the request context so
// render can embed it in forms without re-reading the cookie.
type csrfCtxKeyType int

const csrfCtxKey csrfCtxKeyType = 0

// csrfFrom returns the CSRF token stashed in ctx for embedding in forms, or "".
func csrfFrom(ctx context.Context) string {
	t, _ := ctx.Value(csrfCtxKey).(string)
	return t
}

// csrf is the double-submit-cookie CSRF middleware. It runs for every request:
// it makes sure a CSRF cookie exists (minting one when absent) and stashes the
// token in the request context so forms can embed it, then, for unsafe methods,
// requires the request to echo the token back and rejects a mismatch with 403.
//
// The design is deliberately session-independent so it also covers the login
// POST, whose token must exist before the user is authenticated: a GET of the
// login page issues the cookie, and the login form submits it back. Sessions in
// this app are stateless signed cookies with no server-side store to bind a
// token to, so a browser-scoped double-submit token is the natural fit.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.ensureCSRFCookie(w, r)
		r = r.WithContext(context.WithValue(r.Context(), csrfCtxKey, token))
		if !csrfSafeMethod(r.Method) && !verifyCSRF(r, token) {
			s.renderError(w, r, http.StatusForbidden,
				"Invalid or missing security token. Please reload the page and try again.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrfSafeMethod reports whether m is a read-only method that never needs a CSRF
// token (per RFC 7231 these are the safe/idempotent-read methods).
func csrfSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// ensureCSRFCookie returns the request's CSRF token, minting a fresh one and
// queuing its Set-Cookie when the request carries no valid cookie. The cookie is
// HttpOnly (only the server ever needs it, to embed in forms) and scoped to the
// whole site so a single token covers every page.
func (s *Server) ensureCSRFCookie(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookieName); err == nil && validCSRFToken(c.Value) {
		return c.Value
	}
	token := newCSRFToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
	return token
}

// verifyCSRF constant-time compares the token the request echoed back against
// the cookie token. An empty token on either side never matches.
func verifyCSRF(r *http.Request, cookieToken string) bool {
	got := requestCSRFToken(r)
	if got == "" || cookieToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(cookieToken)) == 1
}

// requestCSRFToken extracts the caller-supplied token. It checks the header
// first (for fetch()-based callers), then the query string, and only reads the
// body for a urlencoded form. It never parses a multipart body: media uploads
// are read by their handler under a custom size cap and read deadline, so the
// middleware must not consume the body — those forms carry the token in the
// query string instead (see admin_episode_form.html).
func requestCSRFToken(r *http.Request) string {
	if t := r.Header.Get(csrfHeader); t != "" {
		return t
	}
	if t := r.URL.Query().Get(csrfField); t != "" {
		return t
	}
	if ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil &&
		ct == "application/x-www-form-urlencoded" {
		return r.PostFormValue(csrfField)
	}
	return ""
}

// newCSRFToken returns a fresh random token. A crypto/rand failure is
// catastrophic and unrecoverable, so it panics (the per-request recover turns it
// into a 500 rather than serving a predictable token).
func newCSRFToken() string {
	b := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(b); err != nil {
		panic("server: csrf token generation failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// validCSRFToken reports whether v is a well-formed token this server could have
// issued, so a garbage/forged cookie is replaced rather than trusted.
func validCSRFToken(v string) bool {
	b, err := base64.RawURLEncoding.DecodeString(v)
	return err == nil && len(b) == csrfTokenBytes
}
