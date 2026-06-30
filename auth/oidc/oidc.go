// Package oidc is the default auth.Authenticator: an OpenID Connect client
// built on github.com/coreos/go-oidc. It is provider-agnostic — point it at any
// compliant issuer (Google, Auth0, Keycloak, Okta, …) via discovery.
package oidc

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/castletfm/castlet/auth"
	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config configures New.
type Config struct {
	Issuer       string   // issuer URL; its /.well-known/openid-configuration is fetched
	ClientID     string   // OAuth2 client id
	ClientSecret string   // OAuth2 client secret
	RedirectURL  string   // must match a redirect URI registered with the provider
	Scopes       []string // requested scopes; "openid" is added if missing
}

// Authenticator implements auth.Authenticator against an OIDC provider.
type Authenticator struct {
	verifier *gooidc.IDTokenVerifier
	oauth2   oauth2.Config
}

var _ auth.Authenticator = (*Authenticator)(nil)

// New performs OIDC discovery against cfg.Issuer and returns an Authenticator.
// It requires network access to the issuer at startup.
func New(ctx context.Context, cfg Config) (*Authenticator, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("oidc: client id and secret are required")
	}
	provider, err := gooidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery for %q: %w", cfg.Issuer, err)
	}
	return &Authenticator{
		verifier: provider.Verifier(&gooidc.Config{ClientID: cfg.ClientID}),
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       withOpenID(cfg.Scopes),
		},
	}, nil
}

// AuthCodeURL returns the provider authorization URL carrying state and nonce.
func (a *Authenticator) AuthCodeURL(state, nonce string) string {
	return a.oauth2.AuthCodeURL(state, gooidc.Nonce(nonce))
}

// Exchange swaps an authorization code for tokens, verifies the ID token and
// its nonce, and returns the caller's identity.
func (a *Authenticator) Exchange(ctx context.Context, code, nonce string) (*auth.Identity, error) {
	tok, err := a.oauth2.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("oidc: token exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("oidc: response has no id_token")
	}
	idToken, err := a.verifier.Verify(ctx, rawID)
	if err != nil {
		return nil, fmt.Errorf("oidc: verify id token: %w", err)
	}
	if idToken.Nonce != nonce {
		return nil, errors.New("oidc: id token nonce mismatch")
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("oidc: parse claims: %w", err)
	}
	return &auth.Identity{
		Issuer:        idToken.Issuer,
		Subject:       idToken.Subject,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		Name:          claims.Name,
	}, nil
}

// withOpenID ensures the required openid scope is present.
func withOpenID(scopes []string) []string {
	if len(scopes) == 0 {
		return []string{gooidc.ScopeOpenID, "profile", "email"}
	}
	if slices.Contains(scopes, gooidc.ScopeOpenID) {
		return scopes
	}
	return append([]string{gooidc.ScopeOpenID}, scopes...)
}
