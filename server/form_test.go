package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/castletfm/castlet/internal/session"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/store/sqlite"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// trackingBody records whether its underlying reader was ever read, so a test
// can assert a handler rejected a request WITHOUT consuming (and, for a
// multipart body, spooling to disk) it.
type trackingBody struct {
	r    *strings.Reader
	read bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	b.read = true
	return b.r.Read(p)
}

func (b *trackingBody) Close() error { return nil }

// authTestServer builds a minimal Server wired for the login/signup handlers: a
// real store, template renderer, session manager, and login limiter, with
// sign-up enabled.
func authTestServer(t *testing.T) *Server {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))
	renderer, err := newTemplateRenderer()
	require.NoError(t, err)
	return &Server{
		store:        st,
		renderer:     renderer,
		logger:       slog.Default(),
		sessions:     session.NewManager([]byte("0123456789abcdef0123456789abcdef")),
		loginLimiter: newLoginLimiter(loginRateLimitMax, loginRateLimitWindow, loginLimiterMaxEntries),
		allowSignup:  true,
		now:          time.Now,
	}
}

// TestLoginRejectsMultipartWithoutSpooling guards the confirmed high finding: a
// multipart POST /login (token would ride in the query, bypassing the CSRF
// body-parse guard) must be rejected 415 before its body is read, so a huge
// multipart body is never spooled to os.TempDir by FormValue.
func TestLoginRejectsMultipartWithoutSpooling(t *testing.T) {
	s := authTestServer(t)
	body := &trackingBody{r: strings.NewReader(strings.Repeat("x", 1<<20))}
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.Body = body
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	rec := httptest.NewRecorder()

	s.handleLogin(rec, req)

	require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
	require.False(t, body.read, "a multipart login body must be rejected without being read/spooled")
}

// TestLoginRejectsOverCapURLEncoded guards the body cap: a urlencoded login body
// larger than maxSmallFormBytes is rejected 413 rather than read wholesale.
func TestLoginRejectsOverCapURLEncoded(t *testing.T) {
	s := authTestServer(t)
	oversized := "email=" + strings.Repeat("a", int(maxSmallFormBytes)+1024)
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(oversized))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	s.handleLogin(rec, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// TestLoginThrottledBeforeBodyParse guards the ordering fix: a throttled IP is
// rejected 429 BEFORE the body is parsed, so it cannot make the server spool or
// parse a body on a request that will not be processed anyway.
func TestLoginThrottledBeforeBodyParse(t *testing.T) {
	s := authTestServer(t)
	now := time.Now()
	for range loginRateLimitMax {
		s.loginLimiter.fail("192.0.2.1", now)
	}

	body := &trackingBody{r: strings.NewReader("email=a@b.c&password=secret")}
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.Body = body
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.1:54321"
	rec := httptest.NewRecorder()

	s.handleLogin(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
	require.False(t, body.read, "a throttled login must be rejected before the body is parsed")
}

// TestLoginNormalURLEncoded confirms a legitimate urlencoded login still works
// after the cap/constraint: valid credentials yield a session and a 303 to the
// admin dashboard.
func TestLoginNormalURLEncoded(t *testing.T) {
	s := authTestServer(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("secret42"), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, s.store.CreateUser(t.Context(), &model.User{
		ID: "u1", Email: "a@b.c", DisplayName: "A", PasswordHash: string(hash), CreatedAt: time.Now()}))

	form := url.Values{"email": {"a@b.c"}, "password": {"secret42"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	s.handleLogin(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/admin/", rec.Header().Get("Location"))
	require.NotEmpty(t, rec.Result().Cookies(), "a successful login must set a session cookie")
}

// TestLoginBadCredentialsReRenders confirms the invalid-credentials re-render
// path still works through the cap/constraint (a 401 login page, not an error).
func TestLoginBadCredentialsReRenders(t *testing.T) {
	s := authTestServer(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("secret42"), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, s.store.CreateUser(t.Context(), &model.User{
		ID: "u1", Email: "a@b.c", DisplayName: "A", PasswordHash: string(hash), CreatedAt: time.Now()}))

	form := url.Values{"email": {"a@b.c"}, "password": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	s.handleLogin(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "Invalid email or password.")
}

// TestSignupNormalURLEncoded confirms a legitimate urlencoded sign-up still
// works after the cap/constraint.
func TestSignupNormalURLEncoded(t *testing.T) {
	s := authTestServer(t)
	form := url.Values{
		"email":            {"new@b.c"},
		"name":             {"New"},
		"password":         {"password1"},
		"password_confirm": {"password1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	s.handleSignup(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/admin/", rec.Header().Get("Location"))
}

// TestSignupRejectsMultipart confirms sign-up shares the multipart-spool
// protection: a multipart body is rejected 415 without being read.
func TestSignupRejectsMultipart(t *testing.T) {
	s := authTestServer(t)
	body := &trackingBody{r: strings.NewReader(strings.Repeat("x", 1<<20))}
	req := httptest.NewRequest(http.MethodPost, "/signup", nil)
	req.Body = body
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	rec := httptest.NewRecorder()

	s.handleSignup(rec, req)

	require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
	require.False(t, body.read, "a multipart sign-up body must be rejected without being read/spooled")
}
