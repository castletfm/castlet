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
	"github.com/castletfm/castlet/store"
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

func TestOIDCRejectsUnverifiedEmailProvisioning(t *testing.T) {
	id := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-u",
		Email: "unverified@user.test", EmailVerified: false, Name: "Unverified"}
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: id}))

	state := h.startOIDC(t)
	resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	// no account was provisioned for the unverified identity
	_, err = h.store.UserByOIDCSubject(t.Context(), "https://idp.test", "sub-u")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestOIDCAllowedDomains(t *testing.T) {
	// a listed, verified domain is provisioned
	allowed := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-ok",
		Email: "ok@allowed.test", EmailVerified: true, Name: "OK"}
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: allowed}),
		server.WithAllowedDomains([]string{"allowed.test"}))

	state := h.startOIDC(t)
	resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	_, err = h.store.UserByOIDCSubject(t.Context(), "https://idp.test", "sub-ok")
	require.NoError(t, err)

	// a non-listed domain is rejected and not provisioned
	other := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-no",
		Email: "no@other.test", EmailVerified: true, Name: "No"}
	h2 := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: other}),
		server.WithAllowedDomains([]string{"allowed.test"}))

	state = h2.startOIDC(t)
	resp, err = h2.client.Get(h2.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	_, err = h2.store.UserByOIDCSubject(t.Context(), "https://idp.test", "sub-no")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestOIDCAllowedDomainsEnforcedOnLinkedSubject(t *testing.T) {
	// a subject already linked to a local account, but whose current email is
	// in a blocked domain, is rejected when an allowlist is configured.
	blocked := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-linked",
		Email: "user@blocked.test", EmailVerified: true, Name: "Linked"}
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: blocked}),
		server.WithAllowedDomains([]string{"allowed.test"}))
	require.NoError(t, h.store.CreateUser(t.Context(), &model.User{ID: "linked1",
		Email: "user@blocked.test", DisplayName: "Linked",
		OIDCIssuer: "https://idp.test", OIDCSubject: "sub-linked"}))

	state := h.startOIDC(t)
	resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	// a linked subject on an allowed domain still signs in.
	ok := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-linked-ok",
		Email: "user@allowed.test", EmailVerified: true, Name: "Linked OK"}
	h2 := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: ok}),
		server.WithAllowedDomains([]string{"allowed.test"}))
	require.NoError(t, h2.store.CreateUser(t.Context(), &model.User{ID: "linked2",
		Email: "user@allowed.test", DisplayName: "Linked OK",
		OIDCIssuer: "https://idp.test", OIDCSubject: "sub-linked-ok"}))

	state = h2.startOIDC(t)
	resp, err = h2.client.Get(h2.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/admin/", resp.Header.Get("Location"))
}

func TestOIDCAllowedDomainsRejectsUnverifiedEmail(t *testing.T) {
	// a subject-linked identity whose allowed-domain email is unverified must
	// not satisfy the allowlist.
	unverified := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-unverified",
		Email: "user@allowed.test", EmailVerified: false, Name: "Unverified"}
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: unverified}),
		server.WithAllowedDomains([]string{"allowed.test"}))
	require.NoError(t, h.store.CreateUser(t.Context(), &model.User{ID: "unverified1",
		Email: "user@allowed.test", DisplayName: "Unverified",
		OIDCIssuer: "https://idp.test", OIDCSubject: "sub-unverified"}))

	state := h.startOIDC(t)
	resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// TestOIDCCallbackClearsTransientCookies verifies the callback clears the
// transient state/nonce cookies on every exit path — the clearing Set-Cookie
// headers must be written BEFORE the response body/redirect, so they are not
// dropped after headers are flushed. A defer-based clear (running after
// WriteHeader) would silently fail, leaving the cookies alive until expiry.
func TestOIDCCallbackClearsTransientCookies(t *testing.T) {
	id := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-clear",
		Email: "clear@user.test", EmailVerified: true, Name: "Clear"}

	// success path: matching state + good code -> session redirect
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: id}))
	state := h.startOIDC(t)
	resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	requireCookieCleared(t, resp, "castlet_oidc_state")
	requireCookieCleared(t, resp, "castlet_oidc_nonce")

	// failure path: valid state cookie present but a bad code fails the exchange
	h2 := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: id}))
	state = h2.startOIDC(t)
	resp, err = h2.client.Get(h2.base + "/auth/oidc/callback?state=" + state + "&code=bad")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	requireCookieCleared(t, resp, "castlet_oidc_state")
	requireCookieCleared(t, resp, "castlet_oidc_nonce")
}

// requireCookieCleared asserts the response carries a Set-Cookie for name that
// deletes it (empty value and a negative max-age). Note that MaxAge==0 means "no
// Max-Age attribute" (the cookie is NOT expired); only MaxAge<0 deletes it. Go
// renders clearOIDCCookie's MaxAge=-1 as "Max-Age=0", which resp.Cookies() parses
// back to MaxAge=-1, so require MaxAge<0 here.
func requireCookieCleared(t *testing.T, resp *http.Response, name string) {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == name {
			require.Empty(t, c.Value, "cleared cookie %q must have an empty value", name)
			require.Less(t, c.MaxAge, 0, "cleared cookie %q must have max-age<0 to delete it", name)
			return
		}
	}
	t.Fatalf("expected a clearing Set-Cookie for %q, got none", name)
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
