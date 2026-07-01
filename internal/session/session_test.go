package session_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/castletfm/castlet/internal/session"
	"github.com/stretchr/testify/require"
)

func TestIssueAndVerify(t *testing.T) {
	m := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))
	now := time.Now()

	rec := httptest.NewRecorder()
	m.Issue(rec, "user-123", 7, now)
	cookie := rec.Result().Cookies()[0]

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)

	uid, epoch, ok := m.UserID(req, now.Add(time.Hour))
	require.True(t, ok)
	require.Equal(t, "user-123", uid)
	require.Equal(t, 7, epoch)
}

func TestExpired(t *testing.T) {
	m := session.NewManager([]byte("0123456789abcdef0123456789abcdef"), session.WithTTL(time.Minute))
	now := time.Now()
	rec := httptest.NewRecorder()
	m.Issue(rec, "u", 0, now)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(rec.Result().Cookies()[0])

	_, _, ok := m.UserID(req, now.Add(2*time.Minute))
	require.False(t, ok)
}

func TestTampered(t *testing.T) {
	m := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))
	now := time.Now()
	rec := httptest.NewRecorder()
	m.Issue(rec, "u", 0, now)
	c := rec.Result().Cookies()[0]
	c.Value += "x" // corrupt the signature

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	_, _, ok := m.UserID(req, now)
	require.False(t, ok)
}

func TestWrongKeyRejected(t *testing.T) {
	now := time.Now()
	rec := httptest.NewRecorder()
	session.NewManager([]byte("0123456789abcdef0123456789abcdef")).Issue(rec, "u", 0, now)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(rec.Result().Cookies()[0])

	other := session.NewManager([]byte("ffffffffffffffffffffffffffffffff"))
	_, _, ok := other.UserID(req, now)
	require.False(t, ok)
}

// The epoch is signed into the cookie and returned verbatim; the server rejects
// a stale epoch by comparing it against the user's current epoch. Changing the
// epoch value must also invalidate the signature (it is covered by the MAC).
func TestEpochIsSignedAndReturned(t *testing.T) {
	m := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))
	now := time.Now()

	rec := httptest.NewRecorder()
	m.Issue(rec, "u", 3, now)
	c := rec.Result().Cookies()[0]

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	uid, epoch, ok := m.UserID(req, now)
	require.True(t, ok)
	require.Equal(t, "u", uid)
	require.Equal(t, 3, epoch)

	// Tampering with the epoch segment breaks the MAC, so the cookie is rejected.
	parts := strings.Split(c.Value, ".")
	require.Len(t, parts, 4)
	parts[2] = base64.RawURLEncoding.EncodeToString([]byte("999"))
	forged := &http.Cookie{Name: c.Name, Value: strings.Join(parts, ".")}
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(forged)
	_, _, ok = m.UserID(req2, now)
	require.False(t, ok)
}
