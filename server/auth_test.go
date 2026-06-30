package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/castletfm/castlet/auth"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/server"
	"github.com/stretchr/testify/require"
)

func TestSignupDisabledByDefault(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.get(t, "/signup")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, err := h.client.PostForm(h.base+"/signup", url.Values{
		"email": {"x@y.z"}, "password": {"longenough"}, "password_confirm": {"longenough"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestSignupFlow(t *testing.T) {
	h := newHarness(t, server.WithAllowSignup(true))

	resp, body := h.get(t, "/signup")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "Create your account")

	// too-short password is rejected
	resp, err := h.client.PostForm(h.base+"/signup", url.Values{
		"email": {"new@user.test"}, "name": {"New"}, "password": {"short"}, "password_confirm": {"short"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// mismatch is rejected
	resp, err = h.client.PostForm(h.base+"/signup", url.Values{
		"email": {"new@user.test"}, "password": {"longenough1"}, "password_confirm": {"longenough2"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// success creates the account and an active session
	resp, err = h.client.PostForm(h.base+"/signup", url.Values{
		"email": {"new@user.test"}, "name": {"New"}, "password": {"longenough"}, "password_confirm": {"longenough"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/admin/", resp.Header.Get("Location"))

	u, err := h.store.UserByEmail(t.Context(), "new@user.test")
	require.NoError(t, err)
	require.Equal(t, "New", u.DisplayName)
	require.NotEmpty(t, u.PasswordHash)

	// session is live: dashboard reachable
	resp, _ = h.get(t, "/admin/")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// duplicate email is reported
	jar2 := newClient()
	resp, err = jar2.PostForm(h.base+"/signup", url.Values{
		"email": {"new@user.test"}, "password": {"longenough"}, "password_confirm": {"longenough"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// fakeAuthn is a stand-in OIDC authenticator for tests.
type fakeAuthn struct {
	identity *auth.Identity
}

func (f fakeAuthn) AuthCodeURL(state, nonce string) string {
	return "https://idp.test/authorize?state=" + state + "&nonce=" + nonce
}

func (f fakeAuthn) Exchange(ctx context.Context, code, nonce string) (*auth.Identity, error) {
	if code != "good" {
		return nil, errors.New("bad code")
	}
	return f.identity, nil
}

func TestOIDCDisabledByDefault(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.get(t, "/auth/oidc/login")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOIDCFlowProvisionsUser(t *testing.T) {
	id := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-1",
		Email: "sso@user.test", EmailVerified: true, Name: "SSO User"}
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: id}))

	// the login page offers the SSO button when OIDC is enabled
	_, loginBody := h.get(t, "/login")
	require.Contains(t, loginBody, "/auth/oidc/login")

	// start: redirect to the provider, state+nonce cookies set
	resp, err := h.client.Get(h.base + "/auth/oidc/login")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	state := loc.Query().Get("state")
	require.NotEmpty(t, state)
	require.NotEmpty(t, loc.Query().Get("nonce"))

	// callback with the matching state and a good code -> session + redirect
	resp, err = h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/admin/", resp.Header.Get("Location"))

	// a user was provisioned and the session is live
	u, err := h.store.UserByOIDCSubject(t.Context(), "https://idp.test", "sub-1")
	require.NoError(t, err)
	require.Equal(t, "sso@user.test", u.Email)
	require.Empty(t, u.PasswordHash, "OIDC-only account has no password")

	resp, _ = h.get(t, "/admin/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestOIDCCallbackRejectsBadState(t *testing.T) {
	id := &auth.Identity{Issuer: "https://idp.test", Subject: "s", Email: "a@b.c", EmailVerified: true}
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: id}))

	// no prior /login -> no state cookie -> rejected
	resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=forged&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOIDCLinksVerifiedEmail(t *testing.T) {
	id := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-9",
		Email: "existing@user.test", EmailVerified: true, Name: "Existing"}
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: id}))

	// pre-existing local account with the same email
	hash := mustHash(t, "secret")
	require.NoError(t, h.store.CreateUser(t.Context(), &model.User{ID: "local1", Email: "existing@user.test",
		DisplayName: "Existing", PasswordHash: hash}))

	state := h.startOIDC(t)
	resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// the existing account is now linked, not duplicated
	linked, err := h.store.UserByOIDCSubject(t.Context(), "https://idp.test", "sub-9")
	require.NoError(t, err)
	require.Equal(t, "local1", linked.ID)
	require.NotEmpty(t, linked.PasswordHash, "linking keeps the existing password")
}

// startOIDC performs the login step and returns the state value.
func (h *harness) startOIDC(t *testing.T) string {
	t.Helper()
	resp, err := h.client.Get(h.base + "/auth/oidc/login")
	require.NoError(t, err)
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	return loc.Query().Get("state")
}
