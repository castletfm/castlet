// Package session implements stateless, HMAC-signed cookie sessions. The
// signed value carries the user id, an expiry, and the user's session epoch, so
// no server-side session storage is required for the default install. The epoch
// is a per-user revocation counter kept in the store: a request's session is
// accepted only while the epoch signed into the cookie still matches the user's
// current epoch, so bumping it (on logout or a password change) invalidates
// every session issued before the bump — a "log out everywhere" that a purely
// stateless cookie cannot provide. A deployment that needs full server-side
// sessions or SSO can replace this behind the same Manager surface used by the
// server package.
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// errInvalid covers every malformed/forged/expired cookie; callers only care
// that there is no valid session.
var errInvalid = errors.New("session: invalid")

// Manager issues and verifies signed session cookies.
type Manager struct {
	key        []byte
	cookieName string
	ttl        time.Duration
	secure     bool
}

// Option configures a Manager.
type Option func(*Manager)

// WithCookieName overrides the cookie name (default "castlet_session").
func WithCookieName(name string) Option { return func(m *Manager) { m.cookieName = name } }

// WithTTL overrides the session lifetime (default 720h).
func WithTTL(d time.Duration) Option { return func(m *Manager) { m.ttl = d } }

// WithSecure marks the cookie Secure (send over HTTPS only). Enable in
// production behind TLS.
func WithSecure(secure bool) Option { return func(m *Manager) { m.secure = secure } }

// NewManager returns a Manager that signs cookies with key. key should be at
// least 32 bytes of high-entropy secret.
func NewManager(key []byte, options ...Option) *Manager {
	m := &Manager{
		key:        key,
		cookieName: "castlet_session",
		ttl:        720 * time.Hour,
	}
	for _, o := range options {
		o(m)
	}
	return m
}

// Issue writes a signed session cookie identifying userID at the given session
// epoch. The epoch is the user's current revocation counter (model.User's
// SessionEpoch); UserID later returns it so the caller can reject a cookie whose
// epoch is stale.
func (m *Manager) Issue(w http.ResponseWriter, userID string, epoch int, now time.Time) {
	exp := now.Add(m.ttl)
	value := m.sign(userID, epoch, exp)
	http.SetCookie(w, &http.Cookie{
		Name:     m.cookieName,
		Value:    value,
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(m.ttl.Seconds()),
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Clear deletes the session cookie.
func (m *Manager) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// UserID returns the authenticated user id and the session epoch signed into
// the cookie, or ("", 0, false) if there is no valid, unexpired session. The
// caller must still confirm the epoch matches the user's current epoch to honor
// revocation.
func (m *Manager) UserID(r *http.Request, now time.Time) (string, int, bool) {
	c, err := r.Cookie(m.cookieName)
	if err != nil {
		return "", 0, false
	}
	uid, epoch, err := m.verify(c.Value, now)
	if err != nil {
		return "", 0, false
	}
	return uid, epoch, true
}

// signed value layout:
// base64url(userID) "." base64url(expiryUnix) "." base64url(epoch) "." base64url(mac)
func (m *Manager) sign(userID string, epoch int, exp time.Time) string {
	payload := b64(userID) + "." + b64(strconv.FormatInt(exp.Unix(), 10)) +
		"." + b64(strconv.Itoa(epoch))
	mac := m.mac(payload)
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac)
}

func (m *Manager) verify(value string, now time.Time) (string, int, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 {
		return "", 0, errInvalid
	}
	payload := parts[0] + "." + parts[1] + "." + parts[2]
	gotMAC, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return "", 0, errInvalid
	}
	if subtle.ConstantTimeCompare(gotMAC, m.mac(payload)) != 1 {
		return "", 0, errInvalid
	}
	expRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", 0, errInvalid
	}
	expUnix, err := strconv.ParseInt(string(expRaw), 10, 64)
	if err != nil {
		return "", 0, errInvalid
	}
	if now.Unix() >= expUnix {
		return "", 0, errInvalid
	}
	epochRaw, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", 0, errInvalid
	}
	epoch, err := strconv.Atoi(string(epochRaw))
	if err != nil {
		return "", 0, errInvalid
	}
	uidRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", 0, errInvalid
	}
	return string(uidRaw), epoch, nil
}

func (m *Manager) mac(payload string) []byte {
	h := hmac.New(sha256.New, m.key)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
