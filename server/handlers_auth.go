package server

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/castletfm/castlet/internal/email"
	"github.com/castletfm/castlet/internal/idgen"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/store"
	"golang.org/x/crypto/bcrypt"
)

// minPasswordLen is the minimum length for a local account password.
const minPasswordLen = 8

// dummyPasswordHash is a precomputed bcrypt hash of an arbitrary password,
// generated once at package load. When the login email is unknown, or the
// account has no usable local password (an OIDC-only user with an empty
// PasswordHash), the login handler still runs bcrypt against this constant hash
// so the request pays the same ~100ms hashing cost as a real account would.
// Without it the handler would return before any bcrypt work whenever the email
// did not resolve to a password, letting an attacker enumerate which accounts
// exist purely from the ~100x difference in response latency.
var dummyPasswordHash = mustDummyPasswordHash()

func mustDummyPasswordHash() []byte {
	// bcrypt.GenerateFromPassword only errors on an invalid cost or an
	// over-length password, neither of which applies to these constants, so a
	// failure here is a programming error worth failing loudly at startup.
	h, err := bcrypt.GenerateFromPassword([]byte("castlet-timing-equalizer"), bcrypt.DefaultCost)
	if err != nil {
		panic("server: precompute dummy bcrypt hash: " + err.Error())
	}
	return h
}

type loginPage struct {
	Email string
	Error string
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if userFrom(r.Context()) != nil {
		s.redirect(w, r, "/admin/")
		return
	}
	s.render(w, r, http.StatusOK, "login", "Log in", loginPage{})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	rawEmail := r.FormValue("email")
	password := r.FormValue("password")

	// Brute-force speed bump: refuse further attempts from an IP that has
	// already exhausted its failed-attempt budget, before touching the store or
	// hashing a password.
	key := clientIP(r)
	if retryAfter, blocked := s.loginLimiter.blocked(key, s.now()); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(retryAfter)))
		s.render(w, r, http.StatusTooManyRequests, "login", "Log in",
			loginPage{Email: rawEmail, Error: "Too many failed login attempts. Please wait and try again."})
		return
	}

	// Look up by the canonical form so a login with different casing than at
	// signup ("Alice@Example.com" vs "alice@example.com") still finds the account.
	// A malformed address cannot match any account: fall through to the same
	// invalid-credentials response rather than short-circuiting, so the form never
	// reveals whether an address is well-formed or exists.
	lookup := rawEmail
	if canonical, cerr := email.Canonical(rawEmail); cerr == nil {
		lookup = canonical
	}

	user, err := s.store.UserByEmail(r.Context(), lookup)

	// Pick the hash to verify against. For an unknown email or an OIDC-only
	// account (no local password), fall back to the constant dummy hash so the
	// bcrypt comparison below always runs; otherwise the response time would
	// reveal whether the account exists. realAccount records whether this is a
	// genuine local login, so a chance match against the dummy hash can never
	// authenticate.
	hash := dummyPasswordHash
	realAccount := 0
	if err == nil && user.PasswordHash != "" {
		hash = []byte(user.PasswordHash)
		realAccount = 1
	}

	// Always pay the full bcrypt cost regardless of which hash was selected, so
	// response time never reveals whether the account exists. A nil result means
	// the password matched; a plain mismatch has already paid the KDF cost. Any
	// OTHER error means the stored hash was structurally unusable (empty, wrong
	// prefix, bad cost, corrupt salt, …) so bcrypt returned cheaply before the
	// KDF — burn a full comparison against the known-good dummy hash and never
	// treat it as a real login. This closes the whole malformed-stored-hash
	// timing class without trying to pre-validate every hash field.
	match := 0
	switch cmpErr := bcrypt.CompareHashAndPassword(hash, []byte(password)); {
	case cmpErr == nil:
		match = 1
	case errors.Is(cmpErr, bcrypt.ErrMismatchedHashAndPassword):
		// Full KDF paid; wrong password.
	default:
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(password))
		realAccount = 0
	}

	// A login succeeds only for a real account whose password matched. subtle
	// combines the two flags without a short-circuit so the decision itself adds
	// no data-dependent branch on top of the (dominant) hashing cost.
	if subtle.ConstantTimeEq(int32(realAccount&match), 1) != 1 {
		// Same response whether the email is unknown or the password is wrong,
		// so the form does not reveal which accounts exist.
		s.loginLimiter.fail(key, s.now())
		s.render(w, r, http.StatusUnauthorized, "login", "Log in",
			loginPage{Email: rawEmail, Error: "Invalid email or password."})
		return
	}

	// A successful login clears the counter so a legitimate user is never locked
	// out by their own earlier typos.
	s.loginLimiter.reset(key)
	s.sessions.Issue(w, user.ID, user.SessionEpoch, s.now())
	s.redirect(w, r, "/admin/")
}

// retryAfterSeconds rounds a retry-after duration up to whole seconds, with a
// floor of 1, for the Retry-After header.
func retryAfterSeconds(d time.Duration) int {
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		return 1
	}
	return secs
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Logout is "log out everywhere": bump the user's session epoch so every
	// session issued for them (not just this browser's cookie) stops validating;
	// clearing the cookie alone only affects the current client.
	//
	// This is deliberately self-contained and fail-closed. It re-parses the signed
	// session cookie here rather than trusting loadUser's request-context user,
	// because loadUser SUPPRESSES UserByID errors: during a store outage the
	// context user is nil, and a logout that keyed the epoch bump off that would
	// silently skip revocation while redirecting as a successful "logged out
	// everywhere", leaving the user's other sessions live. On any store lookup or
	// bump error we therefore best-effort clear this browser's cookie but return a
	// 500 instead of a success redirect.
	uid, epoch, ok := s.sessions.UserID(r, s.now())
	if !ok {
		// No cookie, or a tampered/expired/forged one: there is no session to
		// revoke, so clearing the cookie and redirecting is a benign success.
		s.sessions.Clear(w)
		s.redirect(w, r, "/")
		return
	}

	u, err := s.store.UserByID(r.Context(), uid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The user no longer exists: nothing to revoke, treat as already
			// logged out.
			s.sessions.Clear(w)
			s.redirect(w, r, "/")
			return
		}
		// Store outage: we cannot confirm the epoch bump, so fail closed rather
		// than claim a successful "log out everywhere".
		s.sessions.Clear(w)
		s.serverError(w, r, err)
		return
	}

	if u.SessionEpoch != epoch {
		// The cookie's epoch is already stale — a prior "log out everywhere" or a
		// password change already revoked it, so there is nothing left to do.
		s.sessions.Clear(w)
		s.redirect(w, r, "/")
		return
	}

	if err := s.store.BumpSessionEpoch(r.Context(), u.ID); err != nil {
		// Revocation failed: other sessions for this user may still be live, so we
		// must not report a successful "log out everywhere". Best-effort clear this
		// browser's cookie, then return a 500 rather than a success redirect.
		s.sessions.Clear(w)
		s.serverError(w, r, err)
		return
	}
	s.sessions.Clear(w)
	s.redirect(w, r, "/")
}

type signupPage struct {
	Email string
	Name  string
	Error string
}

func (s *Server) handleSignupForm(w http.ResponseWriter, r *http.Request) {
	if !s.allowSignup {
		s.renderError(w, r, http.StatusNotFound, "Sign-up is disabled.")
		return
	}
	if userFrom(r.Context()) != nil {
		s.redirect(w, r, "/admin/")
		return
	}
	s.render(w, r, http.StatusOK, "signup", "Sign up", signupPage{})
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	if !s.allowSignup {
		s.renderError(w, r, http.StatusNotFound, "Sign-up is disabled.")
		return
	}

	rawEmail := strings.TrimSpace(r.FormValue("email"))
	name := strings.TrimSpace(r.FormValue("name"))
	password := r.FormValue("password")
	confirm := r.FormValue("password_confirm")

	form := signupPage{Email: rawEmail, Name: name}
	fail := func(msg string) {
		form.Error = msg
		s.render(w, r, http.StatusBadRequest, "signup", "Sign up", form)
	}

	// Canonicalize before any store write so accounts are keyed on the mailbox:
	// a malformed address is rejected here, and the stored value is the bare,
	// lower-cased address so the case-insensitive unique index rejects a later
	// duplicate signup under a different case with ErrConflict.
	canonicalEmail, err := email.Canonical(rawEmail)
	if err != nil {
		fail("Enter a valid email address.")
		return
	}
	if len(password) < minPasswordLen {
		fail("Password must be at least 8 characters.")
		return
	}
	if password != confirm {
		fail("Passwords do not match.")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if name == "" {
		name = canonicalEmail
	}
	user := &model.User{
		ID:           idgen.New(),
		Email:        canonicalEmail,
		DisplayName:  name,
		PasswordHash: string(hash),
		CreatedAt:    s.now(),
	}
	if err := s.store.CreateUser(r.Context(), user); err != nil {
		if errors.Is(err, store.ErrConflict) {
			fail("That email is already registered.")
			return
		}
		s.serverError(w, r, err)
		return
	}

	s.sessions.Issue(w, user.ID, user.SessionEpoch, s.now())
	s.redirect(w, r, "/admin/")
}
