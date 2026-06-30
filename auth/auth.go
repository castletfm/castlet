// Package auth defines the single-sign-on boundary: a provider-agnostic
// Authenticator and the Identity it returns. The server depends only on this
// package, so it stays decoupled from any particular OIDC/OAuth2 library; the
// default implementation lives in auth/oidc.
package auth

import "context"

// Identity is the verified identity returned by an Authenticator after a
// successful login.
type Identity struct {
	Issuer        string // identity provider issuer URL
	Subject       string // stable, unique subject within the issuer
	Email         string
	EmailVerified bool
	Name          string
}

// Authenticator drives an OIDC-style redirect login. Implementations must be
// safe for concurrent use.
type Authenticator interface {
	// AuthCodeURL returns the provider's authorization URL for the given
	// opaque state and nonce.
	AuthCodeURL(state, nonce string) string
	// Exchange completes the callback: it exchanges code for tokens, verifies
	// the ID token (signature, audience, expiry, and the supplied nonce), and
	// returns the authenticated identity.
	Exchange(ctx context.Context, code, nonce string) (*Identity, error)
}
