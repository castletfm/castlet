package session_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/castletfm/castlet/internal/session"
	"github.com/stretchr/testify/require"
)

func TestIssueAndVerify(t *testing.T) {
	m := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))
	now := time.Now()

	rec := httptest.NewRecorder()
	m.Issue(rec, "user-123", now)
	cookie := rec.Result().Cookies()[0]

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)

	uid, ok := m.UserID(req, now.Add(time.Hour))
	require.True(t, ok)
	require.Equal(t, "user-123", uid)
}

func TestExpired(t *testing.T) {
	m := session.NewManager([]byte("0123456789abcdef0123456789abcdef"), session.WithTTL(time.Minute))
	now := time.Now()
	rec := httptest.NewRecorder()
	m.Issue(rec, "u", now)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(rec.Result().Cookies()[0])

	_, ok := m.UserID(req, now.Add(2*time.Minute))
	require.False(t, ok)
}

func TestTampered(t *testing.T) {
	m := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))
	now := time.Now()
	rec := httptest.NewRecorder()
	m.Issue(rec, "u", now)
	c := rec.Result().Cookies()[0]
	c.Value += "x" // corrupt the signature

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	_, ok := m.UserID(req, now)
	require.False(t, ok)
}

func TestWrongKeyRejected(t *testing.T) {
	now := time.Now()
	rec := httptest.NewRecorder()
	session.NewManager([]byte("0123456789abcdef0123456789abcdef")).Issue(rec, "u", now)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(rec.Result().Cookies()[0])

	other := session.NewManager([]byte("ffffffffffffffffffffffffffffffff"))
	_, ok := other.UserID(req, now)
	require.False(t, ok)
}
