package server_test

import (
	"context"
	"errors"
	"io"
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

// Repeated failed password logins from the same client are throttled with a 429
// once the per-IP budget is exhausted, while GET routes and OIDC SSO stay
// unaffected.
func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: &auth.Identity{}}))
	h.seed(t) // user a@b.c / "secret"

	wrong := url.Values{"email": {"a@b.c"}, "password": {"nope"}}

	// The first loginRateLimitMax (5) attempts are processed and rejected as
	// invalid credentials (401).
	for i := range 5 {
		resp, err := h.postForm(t, "/login", wrong)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "attempt %d", i)
	}

	// The next attempt is throttled with 429 and a Retry-After header.
	resp, err := h.postForm(t, "/login", wrong)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.NotEmpty(t, resp.Header.Get("Retry-After"))

	// A correct password would also be throttled now, proving the block is on
	// the IP, not credential correctness.
	resp, err = h.postForm(t, "/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)

	// GET /login (rendering the form) is never throttled.
	resp, _ = h.get(t, "/login")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// OIDC SSO is a separate path and stays available.
	resp, err = h.client.Get(h.base + "/auth/oidc/login")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
}

// A successful login clears the failed-attempt counter so a user who mistypes a
// few times and then succeeds is not throttled on their next session.
func TestLoginRateLimitResetOnSuccess(t *testing.T) {
	h := newHarness(t)
	h.seed(t) // user a@b.c / "secret"

	// Four failures — one under the budget of five.
	for range 4 {
		resp, err := h.postForm(t, "/login", url.Values{"email": {"a@b.c"}, "password": {"nope"}})
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	}

	// A success clears the counter.
	resp, err := h.postForm(t, "/login", url.Values{"email": {"a@b.c"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// The budget is full again: five more failures are all processed (401), none
	// throttled, which would be impossible if the counter had not reset.
	for i := range 5 {
		resp, err := h.postForm(t, "/login", url.Values{"email": {"a@b.c"}, "password": {"nope"}})
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "post-reset attempt %d", i)
	}
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

// TestLoginTimingEqualization guards against account enumeration via login
// latency: an unknown email, a known email with the wrong password, and an
// OIDC-only account (no local password) must all fail identically, and a valid
// login must still succeed. The handler achieves this by always running a bcrypt
// comparison — against a constant dummy hash when the email does not resolve to a
// usable local password — so none of the failure paths returns before the
// (dominant) hashing cost is paid.
func TestLoginTimingEqualization(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	// A local account with a real password.
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "local", Email: "local@user.test",
		DisplayName: "Local", PasswordHash: mustHash(t, "secret"), CreatedAt: time.Now()}))
	// An OIDC-only account: it exists but has no local password to compare.
	require.NoError(t, h.store.CreateUser(ctx, &model.User{ID: "sso", Email: "sso@user.test",
		DisplayName: "SSO", OIDCIssuer: "https://idp.test", OIDCSubject: "sub-1", CreatedAt: time.Now()}))

	// Every failing case returns the identical status and body, so nothing in the
	// response distinguishes "no such account" from "wrong password" or "no local
	// password". Each uses a fresh client to keep well under the rate limiter.
	failCases := []struct {
		name          string
		email, passwd string
	}{
		{"unknown email", "nobody@user.test", "whatever"},
		{"known email, wrong password", "local@user.test", "wrong"},
		{"oidc-only account", "sso@user.test", "anything"},
		// The OIDC-only account must not be loggable in with an empty password
		// either, even though its stored hash is empty.
		{"oidc-only account, empty password", "sso@user.test", ""},
	}

	for _, tc := range failCases {
		client := newClient()
		resp, err := postFormCSRF(t, client, h.base, "/login",
			url.Values{"email": {tc.email}, "password": {tc.passwd}})
		require.NoError(t, err, tc.name)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// Identical status and error message across every failing case: the only
		// per-request differences in the page are the reflected email and CSRF
		// token, both of which are caller-supplied input, not account signals.
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, tc.name)
		require.Contains(t, string(body), "Invalid email or password.", tc.name)
	}

	// A genuine local login still succeeds.
	client := newClient()
	resp, err := postFormCSRF(t, client, h.base, "/login",
		url.Values{"email": {"local@user.test"}, "password": {"secret"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/admin/", resp.Header.Get("Location"))
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

// An account already linked to one identity (subject S1) must NOT have its OIDC
// link overwritten when a DIFFERENT identity (subject S2) signs in asserting the
// same verified email — e.g. a reused/aliased mailbox or a second issuer claiming
// the address. That overwrite would be an account takeover. The sign-in for S2 is
// rejected and the stored (issuer, subject) is left pointing at S1.
func TestOIDCRejectsRelinkToAlreadyLinkedAccount(t *testing.T) {
	// The account is already linked to subject S1.
	s2 := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-S2",
		Email: "shared@user.test", EmailVerified: true, Name: "Attacker"}
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: s2}))
	require.NoError(t, h.store.CreateUser(t.Context(), &model.User{ID: "victim",
		Email: "shared@user.test", DisplayName: "Victim",
		OIDCIssuer: "https://idp.test", OIDCSubject: "sub-S1"}))

	// A second identity (S2) with the same verified email tries to sign in.
	state := h.startOIDC(t)
	resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	// S2 was not linked to any account, and S1 still owns the account.
	_, err = h.store.UserByOIDCSubject(t.Context(), "https://idp.test", "sub-S2")
	require.ErrorIs(t, err, store.ErrNotFound, "the newcomer identity must not be linked")
	stillS1, err := h.store.UserByOIDCSubject(t.Context(), "https://idp.test", "sub-S1")
	require.NoError(t, err)
	require.Equal(t, "victim", stillS1.ID, "the original link must be preserved")
}

// A password-only (unlinked) account links successfully on first OIDC sign-in.
func TestOIDCLinksPasswordOnlyAccount(t *testing.T) {
	id := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-link-new",
		Email: "pwuser@user.test", EmailVerified: true, Name: "PW User"}
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: id}))
	require.NoError(t, h.store.CreateUser(t.Context(), &model.User{ID: "pwonly",
		Email: "pwuser@user.test", DisplayName: "PW User", PasswordHash: mustHash(t, "secret")}))

	state := h.startOIDC(t)
	resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/admin/", resp.Header.Get("Location"))

	linked, err := h.store.UserByOIDCSubject(t.Context(), "https://idp.test", "sub-link-new")
	require.NoError(t, err)
	require.Equal(t, "pwonly", linked.ID)
	require.NotEmpty(t, linked.PasswordHash, "linking keeps the existing password")
}

// A returning user whose subject already matches the stored link logs in via the
// subject lookup — the idempotent path, which must keep working unchanged.
func TestOIDCReturningMatchingSubjectLogsIn(t *testing.T) {
	id := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-returning",
		Email: "returning@user.test", EmailVerified: true, Name: "Returning"}
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: id}))
	require.NoError(t, h.store.CreateUser(t.Context(), &model.User{ID: "returning1",
		Email: "returning@user.test", DisplayName: "Returning",
		OIDCIssuer: "https://idp.test", OIDCSubject: "sub-returning"}))

	state := h.startOIDC(t)
	resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/admin/", resp.Header.Get("Location"))
}

// epochRaceOnUpdateStore wraps a store.Store and, on the link path's
// LinkOIDCIdentity, first bumps the user's session epoch — standing in for a "log
// out everywhere" (or password change) landing between UserByEmail and the link
// write. It is used to assert the link path reloads the user before issuing a
// session, so the cookie carries the CURRENT epoch rather than the stale one
// UserByEmail read.
type epochRaceOnUpdateStore struct {
	store.Store
}

func (s epochRaceOnUpdateStore) LinkOIDCIdentity(ctx context.Context, userID, issuer, subject string) error {
	if err := s.BumpSessionEpoch(ctx, userID); err != nil {
		return err
	}
	return s.Store.LinkOIDCIdentity(ctx, userID, issuer, subject)
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

// Signing up with mixed-case input stores the mailbox in canonical (lower-cased,
// display-name-stripped) form, and a later login with any casing of the same
// mailbox resolves that one account — one mailbox = one account.
func TestSignupCanonicalizesEmail(t *testing.T) {
	h := newHarness(t, server.WithAllowSignup(true))

	// A display name and mixed case are both normalized away on write.
	resp, err := h.postForm(t, "/signup", url.Values{
		"email": {"Alice <Alice@Example.com>"}, "name": {"Alice"},
		"password": {"longenough"}, "password_confirm": {"longenough"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// Stored canonical: bare, lower-cased address.
	u, err := h.store.UserByEmail(t.Context(), "alice@example.com")
	require.NoError(t, err)
	require.Equal(t, "alice@example.com", u.Email)

	// Login with a different casing of the same mailbox resolves the SAME account.
	for _, cred := range []string{"alice@example.com", "ALICE@EXAMPLE.COM", "Alice@Example.com"} {
		client := newClient()
		resp, err := postFormCSRF(t, client, h.base, "/login", url.Values{
			"email": {cred}, "password": {"longenough"}})
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusSeeOther, resp.StatusCode, "login with %q must succeed", cred)
		require.Equal(t, "/admin/", resp.Header.Get("Location"))
	}
}

// A second signup for the same mailbox under a different case is rejected as a
// duplicate: the canonical form collides on the case-insensitive unique index.
func TestSignupRejectsCaseVariantDuplicate(t *testing.T) {
	h := newHarness(t, server.WithAllowSignup(true))

	resp, err := h.postForm(t, "/signup", url.Values{
		"email": {"dup@example.com"}, "password": {"longenough"}, "password_confirm": {"longenough"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	client := newClient()
	resp, err = postFormCSRF(t, client, h.base, "/signup", url.Values{
		"email": {"DUP@Example.com"}, "password": {"longenough"}, "password_confirm": {"longenough"}})
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, string(body), "already registered")

	// Only the original account exists.
	u, err := h.store.UserByEmail(t.Context(), "dup@example.com")
	require.NoError(t, err)
	require.NotEmpty(t, u.ID)
}

// A malformed email is rejected at signup via the bad-request path, before any
// account is created.
func TestSignupRejectsMalformedEmail(t *testing.T) {
	h := newHarness(t, server.WithAllowSignup(true))

	resp, err := h.postForm(t, "/signup", url.Values{
		"email": {"not-an-email"}, "password": {"longenough"}, "password_confirm": {"longenough"}})
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, string(body), "valid email")
}

// OIDC links a pre-existing password account when the provider reports the same
// mailbox in a different case, instead of provisioning a duplicate.
func TestOIDCLinksCaseInsensitiveEmail(t *testing.T) {
	// Provider reports the mailbox with different casing than it was stored under.
	id := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-ci",
		Email: "Existing@User.Test", EmailVerified: true, Name: "Existing"}
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: id}))

	// pre-existing local account stored in canonical (lower-case) form
	require.NoError(t, h.store.CreateUser(t.Context(), &model.User{ID: "local1",
		Email: "existing@user.test", DisplayName: "Existing", PasswordHash: mustHash(t, "secret")}))

	state := h.startOIDC(t)
	resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// the existing account was linked (matched case-insensitively), not duplicated
	linked, err := h.store.UserByOIDCSubject(t.Context(), "https://idp.test", "sub-ci")
	require.NoError(t, err)
	require.Equal(t, "local1", linked.ID)
	require.NotEmpty(t, linked.PasswordHash, "linking keeps the existing password")
	require.Equal(t, "existing@user.test", linked.Email, "linking does not rewrite the stored canonical email")
}

// OIDC just-in-time provisioning stores the provider email in canonical form, so
// a later provider report under a different case resolves the same account.
func TestOIDCProvisionCanonicalizesEmail(t *testing.T) {
	id := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-prov",
		Email: "New.User@Example.COM", EmailVerified: true, Name: "New User"}
	h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: id}))

	state := h.startOIDC(t)
	resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	u, err := h.store.UserByOIDCSubject(t.Context(), "https://idp.test", "sub-prov")
	require.NoError(t, err)
	require.Equal(t, "new.user@example.com", u.Email, "provisioning stores the canonical email")

	// The canonical address is now discoverable via the case-insensitive lookup.
	byEmail, err := h.store.UserByEmail(t.Context(), "new.user@example.com")
	require.NoError(t, err)
	require.Equal(t, u.ID, byEmail.ID)
}

// A malformed email at login (one net/mail rejects) cannot canonicalize and so
// cannot match any account. It must take the ordinary invalid-credentials path:
// the same 401 + "Invalid email or password." as a wrong password, never a 500
// or a different status, and it must not authenticate.
func TestLoginMalformedEmailFailsAsInvalidCredentials(t *testing.T) {
	h := newHarness(t)
	h.seed(t) // a@b.c / "secret"

	// Baseline: a wrong password for a real account renders the 401 error page.
	resp, err := h.postForm(t, "/login", url.Values{"email": {"a@b.c"}, "password": {"nope"}})
	require.NoError(t, err)
	baseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Contains(t, string(baseBody), "Invalid email or password.")

	// A malformed address takes the identical path: 401, same message, no session.
	// (Kept under the per-IP failure budget so the limiter does not turn later
	// attempts into 429s.)
	for _, bad := range []string{"not-an-email", "alice@"} {
		client := newClient()
		resp, err := postFormCSRF(t, client, h.base, "/login",
			url.Values{"email": {bad}, "password": {"whatever"}})
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"malformed email %q must return the standard 401, not a 500 or other status", bad)
		require.Contains(t, string(body), "Invalid email or password.",
			"malformed email must render the same invalid-credentials message as a wrong password")
		for _, c := range resp.Cookies() {
			require.NotEqual(t, "castlet_session", c.Name,
				"malformed login must not issue a session")
		}
	}
}

// When the provider reports a malformed email (net/mail rejects it), it is
// treated as "no usable email": neither a new account is provisioned nor an
// existing account is linked. Both paths must reject with 403 and write nothing.
func TestOIDCMalformedProviderEmailRejected(t *testing.T) {
	t.Run("provision", func(t *testing.T) {
		id := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-bad",
			Email: "not-an-email", EmailVerified: true, Name: "Bad"}
		h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: id}))

		state := h.startOIDC(t)
		resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode)

		// no account provisioned for the malformed identity
		_, err = h.store.UserByOIDCSubject(t.Context(), "https://idp.test", "sub-bad")
		require.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("link", func(t *testing.T) {
		id := &auth.Identity{Issuer: "https://idp.test", Subject: "sub-bad-link",
			Email: "not-an-email", EmailVerified: true, Name: "Bad"}
		h := newHarness(t, server.WithAuthenticator(fakeAuthn{identity: id}))

		// A pre-existing local account exists, but the malformed provider email
		// cannot be used to find (and thus must not link) it.
		require.NoError(t, h.store.CreateUser(t.Context(), &model.User{ID: "local1",
			Email: "existing@user.test", DisplayName: "Existing", PasswordHash: mustHash(t, "secret")}))

		state := h.startOIDC(t)
		resp, err := h.client.Get(h.base + "/auth/oidc/callback?state=" + state + "&code=good")
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode)

		// the existing account was NOT linked to the malformed identity
		_, err = h.store.UserByOIDCSubject(t.Context(), "https://idp.test", "sub-bad-link")
		require.ErrorIs(t, err, store.ErrNotFound)
		unchanged, err := h.store.UserByEmail(t.Context(), "existing@user.test")
		require.NoError(t, err)
		require.Empty(t, unchanged.OIDCSubject, "malformed provider email must not link the existing account")
	})
}
