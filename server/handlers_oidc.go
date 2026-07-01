package server

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/castletfm/castlet/auth"
	"github.com/castletfm/castlet/internal/idgen"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/store"
)

const (
	oidcStateCookie = "castlet_oidc_state"
	oidcNonceCookie = "castlet_oidc_nonce"
	oidcCookiePath  = "/auth/oidc"
)

// handleOIDCLogin starts the OIDC flow: it mints a random state and nonce,
// stashes them in short-lived cookies (the state defends against CSRF on the
// callback, the nonce binds the ID token to this request), and redirects to
// the provider.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.authn == nil {
		s.renderError(w, r, http.StatusNotFound, "Single sign-on is not enabled.")
		return
	}
	state := idgen.New()
	nonce := idgen.New()
	s.setOIDCCookie(w, oidcStateCookie, state)
	s.setOIDCCookie(w, oidcNonceCookie, nonce)
	http.Redirect(w, r, s.authn.AuthCodeURL(state, nonce), http.StatusSeeOther)
}

// handleOIDCCallback completes the flow: it checks the returned state against
// the cookie, exchanges the code (which verifies the ID token and nonce), then
// resolves the identity to a user and starts a session.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.authn == nil {
		s.renderError(w, r, http.StatusNotFound, "Single sign-on is not enabled.")
		return
	}
	// Clear the transient state/nonce cookies up front, before any response is
	// written. The values were carried on the request, so reading them below is
	// unaffected; queuing the clearing Set-Cookie headers now guarantees they
	// survive to the client on every exit path — a deferred clear would run
	// after WriteHeader and be silently dropped.
	s.clearOIDCCookie(w, oidcStateCookie)
	s.clearOIDCCookie(w, oidcNonceCookie)

	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		s.logger.Warn("oidc provider returned error", "error", errMsg, "description", r.URL.Query().Get("error_description"))
		s.renderError(w, r, http.StatusUnauthorized, "Single sign-on was cancelled or failed.")
		return
	}

	if !validOIDCState(r) {
		s.renderError(w, r, http.StatusBadRequest, "Invalid single sign-on state. Please try again.")
		return
	}
	nonce, err := r.Cookie(oidcNonceCookie)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Single sign-on session expired. Please try again.")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.renderError(w, r, http.StatusBadRequest, "Missing authorization code.")
		return
	}

	identity, err := s.authn.Exchange(r.Context(), code, nonce.Value)
	if err != nil {
		s.logger.Warn("oidc exchange failed", "error", err)
		s.renderError(w, r, http.StatusUnauthorized, "Could not verify your single sign-on identity.")
		return
	}

	user, err := s.resolveOIDCUser(r, identity)
	if err != nil {
		s.logger.Warn("oidc user resolution failed", "error", err)
		s.renderError(w, r, http.StatusForbidden, err.Error())
		return
	}

	s.sessions.Issue(w, user.ID, s.now())
	s.redirect(w, r, "/admin/")
}

// resolveOIDCUser maps a verified identity to a local user: by (issuer,
// subject) first; otherwise it links a pre-existing account that owns the same
// verified email; otherwise it provisions a new account.
func (s *Server) resolveOIDCUser(r *http.Request, id *auth.Identity) (*model.User, error) {
	ctx := r.Context()

	// When an email-domain allowlist is configured, enforce it on every
	// sign-in path, including accounts already linked by subject, so removing
	// a domain from the allowlist blocks its previously linked users too.
	if len(s.allowedDomains) > 0 {
		if id.Email == "" {
			return nil, errors.New("Your identity provider did not share an email address.")
		}
		if !id.EmailVerified {
			return nil, errors.New("Your identity provider did not verify your email address.")
		}
		if !s.emailDomainAllowed(id.Email) {
			return nil, errors.New("Your email domain is not permitted to sign in.")
		}
	}

	user, err := s.store.UserByOIDCSubject(ctx, id.Issuer, id.Subject)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	if id.Email == "" {
		return nil, errors.New("Your identity provider did not share an email address.")
	}

	// Link an existing local account, but only when the provider vouches for
	// the email, so a password account cannot be hijacked via an unverified
	// claim.
	existing, err := s.store.UserByEmail(ctx, id.Email)
	if err == nil {
		if !id.EmailVerified {
			return nil, errors.New("An account with this email exists but the provider did not verify the address.")
		}
		existing.OIDCIssuer = id.Issuer
		existing.OIDCSubject = id.Subject
		if err := s.store.UpdateUser(ctx, existing); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	// Provision a new account (just-in-time), but only for a verified email so
	// a broad/multi-tenant issuer cannot self-provision arbitrary identities.
	if !id.EmailVerified {
		return nil, errors.New("Your identity provider did not verify your email address.")
	}
	name := id.Name
	if name == "" {
		name = id.Email
	}
	user = &model.User{
		ID:          idgen.New(),
		Email:       id.Email,
		DisplayName: name,
		OIDCIssuer:  id.Issuer,
		OIDCSubject: id.Subject,
		CreatedAt:   s.now(),
	}
	if err := s.store.CreateUser(ctx, user); err != nil {
		return nil, err
	}
	return user, nil
}

// emailDomainAllowed reports whether email's domain is in the configured
// allowlist. With no allowlist, every domain is permitted.
func (s *Server) emailDomainAllowed(email string) bool {
	if len(s.allowedDomains) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	return slices.Contains(s.allowedDomains, strings.ToLower(email[at+1:]))
}

func validOIDCState(r *http.Request) bool {
	c, err := r.Cookie(oidcStateCookie)
	if err != nil {
		return false
	}
	got := r.URL.Query().Get("state")
	return got != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(got)) == 1
}

func (s *Server) setOIDCCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     oidcCookiePath,
		MaxAge:   int((10 * time.Minute).Seconds()),
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearOIDCCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     oidcCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
}

// cookieSecure marks auth cookies Secure when the site is served over HTTPS.
func (s *Server) cookieSecure() bool { return strings.HasPrefix(s.baseURL, "https://") }
