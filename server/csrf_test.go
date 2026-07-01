package server_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/castletfm/castlet/model"
	"github.com/stretchr/testify/require"
)

// TestCSRFLoginRequiresToken verifies the login POST is CSRF-protected: a POST
// with no token (or a wrong one) is rejected with 403 before authentication,
// while the same credentials with a valid token succeed. This is the pre-auth
// case — the token is issued on a GET and echoed back by the form.
func TestCSRFLoginRequiresToken(t *testing.T) {
	h := newHarness(t)
	h.seed(t) // user a@b.c / "secret"

	// No token at all: rejected.
	resp, err := h.client.PostForm(h.base+"/login", url.Values{
		"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a login POST without a CSRF token must be rejected")

	// A wrong token (even with a valid cookie in the jar): rejected.
	_ = csrfToken(t, h.client, h.base) // ensure the cookie is set in the jar
	resp, err = h.client.PostForm(h.base+"/login", url.Values{
		"email": {"a@b.c"}, "password": {"secret"}, "csrf_token": {"not-the-real-token"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a login POST with a mismatched CSRF token must be rejected")

	// The correct token: succeeds.
	resp, err = h.postForm(t, "/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/admin/", resp.Header.Get("Location"))
}

// TestCSRFAdminPostRequiresToken verifies an authenticated state-changing POST
// (channel create) is rejected without a token and accepted with one, so a valid
// session alone is not enough to make a change.
func TestCSRFAdminPostRequiresToken(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.store.CreateUser(t.Context(), &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: mustHash(t, "secret"), CreatedAt: time.Now()}))

	// Authenticate.
	resp, err := h.postForm(t, "/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// Logged in, but no CSRF token on the POST: rejected.
	resp, err = h.client.PostForm(h.base+"/admin/channels", url.Values{"title": {"Nope"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a session cookie alone must not authorize a state-changing POST")

	channels, err := h.store.ListChannelsByUser(t.Context(), "u1")
	require.NoError(t, err)
	require.Empty(t, channels, "the rejected POST must not have created a channel")

	// With a valid token: accepted.
	resp, err = h.postForm(t, "/admin/channels", url.Values{"title": {"Yes"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	channels, err = h.store.ListChannelsByUser(t.Context(), "u1")
	require.NoError(t, err)
	require.Len(t, channels, 1)
	require.Equal(t, "Yes", channels[0].Title)
}

// TestCSRFTokenMismatchBetweenClients verifies the token is bound to the caller's
// cookie: a token minted for one browser cannot be replayed by another, since the
// other browser's cookie carries a different secret.
func TestCSRFTokenMismatchBetweenClients(t *testing.T) {
	h := newHarness(t)
	h.seed(t)

	resp, err := h.postForm(t, "/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// A different client (different CSRF cookie) hands us a token; replaying it on
	// h.client, whose cookie differs, must not validate.
	other := newClient()
	stolen := csrfToken(t, other, h.base)
	resp, err = h.client.PostForm(h.base+"/admin/channels",
		url.Values{"title": {"X"}, "csrf_token": {stolen}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a token minted for another client's cookie must not be accepted")
}

// TestCSRFCookieIssuedOnGet verifies a GET issues an HttpOnly CSRF cookie and the
// rendered form embeds the matching token, so a browser can submit it back.
func TestCSRFCookieIssuedOnGet(t *testing.T) {
	h := newHarness(t)

	resp, body := h.get(t, "/login")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var csrf *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "castlet_csrf" {
			csrf = c
		}
	}
	require.NotNil(t, csrf, "GET /login must set a CSRF cookie")
	require.True(t, csrf.HttpOnly, "the CSRF cookie must be HttpOnly")
	require.NotEmpty(t, csrf.Value)
	require.True(t, strings.Contains(body, csrf.Value),
		"the login form must embed the CSRF token so the browser can echo it back")
}

// TestCSRFTokenEmbeddedInAdminForms verifies the admin episode list — which
// embeds the token via $.CSRFToken inside a range (reorder/publish/delete forms)
// — renders it, so the per-item forms carry a valid token.
func TestCSRFTokenEmbeddedInAdminForms(t *testing.T) {
	h := newHarness(t)
	h.seed(t) // user a@b.c, channel c1, episode e1

	resp, err := h.postForm(t, "/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	token := csrfToken(t, h.client, h.base)
	resp, body := h.get(t, "/admin/channels/c1/episodes")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, `name="csrf_token" value="`+token+`"`,
		"per-episode forms in the range must embed the CSRF token")
}

// TestCSRFSafeMethodsUnaffected verifies read-only requests never require a token
// and still succeed (GET is the common case exercised throughout, asserted here
// explicitly for the guard).
func TestCSRFSafeMethodsUnaffected(t *testing.T) {
	h := newHarness(t)
	h.seed(t)

	resp, _ := h.get(t, "/")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, _ = h.get(t, "/c/c1/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
