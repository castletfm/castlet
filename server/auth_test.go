package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/castletfm/castlet/auth"
	"github.com/castletfm/castlet/blob/localfs"
	"github.com/castletfm/castlet/internal/session"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/queue/dbqueue"
	"github.com/castletfm/castlet/server"
	"github.com/castletfm/castlet/store"
	"github.com/castletfm/castlet/store/sqlite"
	"github.com/stretchr/testify/require"
)

func TestSignupDisabledByDefault(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.get(t, "/signup")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, err := h.postForm(t, "/signup", url.Values{
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
	resp, err := h.postForm(t, "/signup", url.Values{
		"email": {"new@user.test"}, "name": {"New"}, "password": {"short"}, "password_confirm": {"short"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// mismatch is rejected
	resp, err = h.postForm(t, "/signup", url.Values{
		"email": {"new@user.test"}, "password": {"longenough1"}, "password_confirm": {"longenough2"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// success creates the account and an active session
	resp, err = h.postForm(t, "/signup", url.Values{
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
	resp, err = postFormCSRF(t, jar2, h.base, "/signup", url.Values{
		"email": {"new@user.test"}, "password": {"longenough"}, "password_confirm": {"longenough"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// A session issued before the user's epoch is bumped (logout / password change)
// stops validating, even when the raw cookie is replayed by another client — a
// leaked cookie cannot outlive a "log out everywhere".
func TestLogoutRevokesExistingSessions(t *testing.T) {
	h := newHarness(t)
	h.seed(t) // user a@b.c / "secret"

	// Log in and capture the raw session cookie, as a leaked copy would have it.
	resp, err := h.postForm(t, "/login", url.Values{
		"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	var sessionCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "castlet_session" && c.Value != "" {
			sessionCookie = c
		}
	}
	require.NotNil(t, sessionCookie, "login must set a session cookie")

	// The session is live.
	resp, _ = h.get(t, "/admin/")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Log out: the epoch is bumped server-side.
	before, err := h.store.UserByID(t.Context(), "u1")
	require.NoError(t, err)
	resp, err = h.postForm(t, "/logout", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	after, err := h.store.UserByID(t.Context(), "u1")
	require.NoError(t, err)
	require.Equal(t, before.SessionEpoch+1, after.SessionEpoch, "logout must bump the epoch")

	// Replay the captured cookie from a fresh client: the stale epoch is rejected.
	leaked := newClient()
	req, err := http.NewRequest(http.MethodGet, h.base+"/admin/", nil)
	require.NoError(t, err)
	req.AddCookie(sessionCookie)
	resp, err = leaked.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/login", resp.Header.Get("Location"))
}

// failBumpStore wraps a store.Store but forces BumpSessionEpoch to fail, so a
// logout cannot revoke sessions — used to assert logout reports the failure with
// an error response instead of silently redirecting as a successful "log out
// everywhere".
type failBumpStore struct {
	store.Store
}

func (failBumpStore) BumpSessionEpoch(context.Context, string) error {
	return errors.New("bump failed")
}

// When the epoch bump (revocation) fails, logout must NOT report success: a
// failed "log out everywhere" that redirected as if it worked would leave the
// user's other sessions live while telling them they were logged out. It must
// return a 500 instead.
func TestLogoutRevocationFailureReturnsError(t *testing.T) {
	ctx := t.Context()

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))
	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: mustHash(t, "secret"), CreatedAt: time.Now()}))

	blobs, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	sess := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))
	srv, err := server.New(failBumpStore{st}, blobs, dbqueue.New(st), sess,
		server.WithAddr("127.0.0.1:0"), server.WithBaseURL("http://example.test"))
	require.NoError(t, err)
	ctrl, err := srv.Run(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { <-ctrl.Done() })
	base := "http://" + ctrl.Addr()

	client := newClient()
	resp, err := postFormCSRF(t, client, base, "/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// Logout fails to bump the epoch: it must surface a 500, not a success redirect.
	resp, err = postFormCSRF(t, client, base, "/logout", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"logout must fail loudly when revocation fails, not redirect as success")
}

// failUserByIDStore wraps a store.Store but forces UserByID to fail with a
// non-ErrNotFound error, standing in for a store outage. loadUser SUPPRESSES
// this error (leaving the request-context user nil), so it is used to assert
// logout does not silently skip revocation and redirect as success when it
// cannot look the user up.
type failUserByIDStore struct {
	store.Store
}

func (failUserByIDStore) UserByID(context.Context, string) (*model.User, error) {
	return nil, errors.New("store outage")
}

// When logout cannot look up the cookie's user (store outage), it must NOT
// report a successful "log out everywhere": the epoch was never bumped, so other
// sessions are still live. It must fail closed with a 500. This guards the case
// loadUser hides — an error there leaves the context user nil, and a logout that
// keyed the bump off that context user would clear only this cookie and redirect
// as success.
func TestLogoutFailsClosedWhenUserLookupFails(t *testing.T) {
	ctx := t.Context()

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))
	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", PasswordHash: mustHash(t, "secret"), CreatedAt: time.Now()}))

	blobs, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	sess := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))
	srv, err := server.New(failUserByIDStore{st}, blobs, dbqueue.New(st), sess,
		server.WithAddr("127.0.0.1:0"), server.WithBaseURL("http://example.test"))
	require.NoError(t, err)
	ctrl, err := srv.Run(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { <-ctrl.Done() })
	base := "http://" + ctrl.Addr()

	// Login issues a valid signed cookie (login uses UserByEmail, which still works).
	client := newClient()
	resp, err := postFormCSRF(t, client, base, "/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// Logout re-parses the cookie itself and looks the user up; that lookup fails,
	// so revocation cannot be confirmed and logout must return 500, not a redirect.
	resp, err = postFormCSRF(t, client, base, "/logout", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"logout must fail closed when it cannot look up the user to revoke")
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

// epochRaceOnUpdateStore wraps a store.Store and, on the link path's UpdateUser,
// first bumps the user's session epoch — standing in for a "log out everywhere"
// (or password change) landing between UserByEmail and UpdateUser. It is used to
// assert the link path reloads the user before issuing a session, so the cookie
// carries the CURRENT epoch rather than the stale one UserByEmail read.
type epochRaceOnUpdateStore struct {
	store.Store
}

func (s epochRaceOnUpdateStore) UpdateUser(ctx context.Context, u *model.User) error {
	if err := s.BumpSessionEpoch(ctx, u.ID); err != nil {
		return err
	}
	return s.Store.UpdateUser(ctx, u)
}

// The OIDC link path must reload the user after UpdateUser and issue the session
// from the reloaded epoch. If an epoch bump races in between (simulated here), a
// session minted from the pre-update struct would carry a stale epoch and be
// rejected on the very next request. With the reload, the cookie carries the
// bumped epoch and the session is live.
func TestOIDCLinkIssuesSessionWithReloadedEpoch(t *testing.T) {
	ctx := t.Context()

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))
	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "local1", Email: "existing@user.test",
		DisplayName: "Existing", PasswordHash: mustHash(t, "secret"), CreatedAt: time.Now()}))

	blobs, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	sess := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))
	id := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-race",
		Email: "existing@user.test", EmailVerified: true, Name: "Existing"}
	srv, err := server.New(epochRaceOnUpdateStore{st}, blobs, dbqueue.New(st), sess,
		server.WithAddr("127.0.0.1:0"), server.WithBaseURL("http://example.test"),
		server.WithAuthenticator(fakeAuthn{identity: id}))
	require.NoError(t, err)
	ctrl, err := srv.Run(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { <-ctrl.Done() })
	base := "http://" + ctrl.Addr()

	client := newClient()
	// Start the flow to obtain the state cookie + value.
	resp, err := client.Get(base + "/auth/oidc/login")
	require.NoError(t, err)
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	state := loc.Query().Get("state")
	require.NotEmpty(t, state)

	// Complete the callback: the link path updates the user (racing a bump) and
	// then issues the session; the epoch is now 1.
	resp, err = client.Get(base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/admin/", resp.Header.Get("Location"))

	// Confirm the race actually bumped the epoch (so the pre-update struct is stale).
	linked, err := st.UserByID(ctx, "local1")
	require.NoError(t, err)
	require.Equal(t, 1, linked.SessionEpoch, "the racing bump must have advanced the epoch")

	// The issued session must be live: it was minted from the reloaded (epoch 1)
	// user, not the stale (epoch 0) struct. A stale cookie would be rejected and
	// /admin/ would redirect to /login.
	req, err := http.NewRequest(http.MethodGet, base+"/admin/", nil)
	require.NoError(t, err)
	resp, err = client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"link path must issue the session from the reloaded epoch, not a stale one")
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
