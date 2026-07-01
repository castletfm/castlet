package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockIdP is a hermetic OpenID Connect provider backed by httptest. It serves a
// discovery document, a JWKS built from an in-test RSA key, and a token
// endpoint that hands back whatever ID token the current test has staged. This
// lets the tests exercise the real verification path in Authenticator.Exchange
// (github.com/coreos/go-oidc/v3) with no external network.
type mockIdP struct {
	// issuerURL is the immutable base URL of the mock server. It is set once
	// before the server can serve any request, so handler goroutines can read it
	// without synchronization.
	issuerURL string
	key       *rsa.PrivateKey
	kid       string

	// mu guards idToken, which subtests assign from the test goroutine while the
	// /token handler reads it from the httptest server goroutine.
	mu sync.RWMutex
	// idToken is the raw JWT the token endpoint returns for the next Exchange.
	idToken string
}

func newMockIdP(t *testing.T) *mockIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	idp := &mockIdP{key: key, kid: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                idp.issuer(),
			"authorization_endpoint":                idp.issuer() + "/authorize",
			"token_endpoint":                        idp.issuer() + "/token",
			"jwks_uri":                              idp.issuer() + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, idp.jwks())
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"access_token": "dummy-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idp.getIDToken(),
		})
	})

	server := httptest.NewServer(mux)
	idp.issuerURL = server.URL
	t.Cleanup(server.Close)
	return idp
}

func (idp *mockIdP) issuer() string { return idp.issuerURL }

// setIDToken stages the JWT the /token endpoint will return next, guarding the
// write against the concurrent read in the token handler.
func (idp *mockIdP) setIDToken(token string) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.idToken = token
}

// getIDToken returns the staged JWT under the read lock.
func (idp *mockIdP) getIDToken() string {
	idp.mu.RLock()
	defer idp.mu.RUnlock()
	return idp.idToken
}

// jwks returns the JSON Web Key Set exposing the public half of idp.key.
func (idp *mockIdP) jwks() map[string]any {
	pub := idp.key.Public().(*rsa.PublicKey)
	eBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(eBuf, uint64(pub.E))
	// Trim leading zero bytes from the exponent.
	i := 0
	for i < len(eBuf)-1 && eBuf[i] == 0 {
		i++
	}
	return map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"alg": "RS256",
			"use": "sig",
			"kid": idp.kid,
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(eBuf[i:]),
		}},
	}
}

// signToken builds and RS256-signs a JWT with the given claims using signKey
// and advertises signKid in the header. Passing idp.key/idp.kid yields a token
// the JWKS will validate; passing a foreign key simulates a bad signature.
func signToken(t *testing.T, signKey *rsa.PrivateKey, signKid string, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": signKid}
	seg := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt segment: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := seg(header) + "." + seg(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, signKey, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

const testClientID = "castlet-test-client"

// baseClaims returns a valid, unexpired claim set for the given issuer/nonce.
func baseClaims(issuer, nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":            issuer,
		"sub":            "subject-123",
		"aud":            testClientID,
		"exp":            now.Add(time.Hour).Unix(),
		"iat":            now.Unix(),
		"nonce":          nonce,
		"email":          "alice@example.com",
		"email_verified": true,
		"name":           "Alice Example",
	}
}

func newAuthenticator(t *testing.T, idp *mockIdP) *Authenticator {
	t.Helper()
	a, err := New(context.Background(), Config{
		Issuer:       idp.issuer(),
		ClientID:     testClientID,
		ClientSecret: "test-secret",
		RedirectURL:  "http://localhost/callback",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestExchange(t *testing.T) {
	const nonce = "nonce-abc"

	t.Run("accepts a valid id token", func(t *testing.T) {
		idp := newMockIdP(t)
		a := newAuthenticator(t, idp)
		idp.setIDToken(signToken(t, idp.key, idp.kid, baseClaims(idp.issuer(), nonce)))

		id, err := a.Exchange(context.Background(), "code", nonce)
		if err != nil {
			t.Fatalf("Exchange: unexpected error: %v", err)
		}
		if id.Issuer != idp.issuer() {
			t.Errorf("Issuer = %q, want %q", id.Issuer, idp.issuer())
		}
		if id.Subject != "subject-123" {
			t.Errorf("Subject = %q, want subject-123", id.Subject)
		}
		if id.Email != "alice@example.com" {
			t.Errorf("Email = %q, want alice@example.com", id.Email)
		}
		if !id.EmailVerified {
			t.Errorf("EmailVerified = false, want true")
		}
		if id.Name != "Alice Example" {
			t.Errorf("Name = %q, want Alice Example", id.Name)
		}
	})

	t.Run("rejects a wrong issuer", func(t *testing.T) {
		idp := newMockIdP(t)
		a := newAuthenticator(t, idp)
		claims := baseClaims("https://evil.example.com", nonce)
		idp.setIDToken(signToken(t, idp.key, idp.kid, claims))

		if _, err := a.Exchange(context.Background(), "code", nonce); err == nil {
			t.Fatal("Exchange accepted a token with a mismatched issuer")
		}
	})

	t.Run("rejects a wrong audience", func(t *testing.T) {
		idp := newMockIdP(t)
		a := newAuthenticator(t, idp)
		claims := baseClaims(idp.issuer(), nonce)
		claims["aud"] = "some-other-client"
		idp.setIDToken(signToken(t, idp.key, idp.kid, claims))

		if _, err := a.Exchange(context.Background(), "code", nonce); err == nil {
			t.Fatal("Exchange accepted a token minted for a different audience")
		}
	})

	t.Run("rejects a wrong-key signature", func(t *testing.T) {
		idp := newMockIdP(t)
		a := newAuthenticator(t, idp)
		attackerKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate attacker key: %v", err)
		}
		// Signed with a key that is NOT in the JWKS but reuses the trusted kid.
		idp.setIDToken(signToken(t, attackerKey, idp.kid, baseClaims(idp.issuer(), nonce)))

		if _, err := a.Exchange(context.Background(), "code", nonce); err == nil {
			t.Fatal("Exchange accepted a token signed with an untrusted key")
		}
	})

	t.Run("rejects a tampered signature", func(t *testing.T) {
		idp := newMockIdP(t)
		a := newAuthenticator(t, idp)
		tok := signToken(t, idp.key, idp.kid, baseClaims(idp.issuer(), nonce))
		// Corrupt a byte in the decoded signature so verification fails
		// deterministically. Flipping only the final base64url character can be a
		// no-op: for an RSA-2048 signature its low bits are unused, so the decoded
		// signature bytes may be unchanged and the token still verifies.
		dot := strings.LastIndexByte(tok, '.')
		sig, err := base64.RawURLEncoding.DecodeString(tok[dot+1:])
		if err != nil {
			t.Fatalf("decode signature: %v", err)
		}
		sig[0] ^= 0xff
		idp.setIDToken(tok[:dot+1] + base64.RawURLEncoding.EncodeToString(sig))

		if _, err := a.Exchange(context.Background(), "code", nonce); err == nil {
			t.Fatal("Exchange accepted a token with a tampered signature")
		}
	})

	t.Run("rejects an expired token", func(t *testing.T) {
		idp := newMockIdP(t)
		a := newAuthenticator(t, idp)
		claims := baseClaims(idp.issuer(), nonce)
		claims["exp"] = time.Now().Add(-time.Hour).Unix()
		claims["iat"] = time.Now().Add(-2 * time.Hour).Unix()
		idp.setIDToken(signToken(t, idp.key, idp.kid, claims))

		if _, err := a.Exchange(context.Background(), "code", nonce); err == nil {
			t.Fatal("Exchange accepted an expired token")
		}
	})

	t.Run("rejects a nonce mismatch", func(t *testing.T) {
		idp := newMockIdP(t)
		a := newAuthenticator(t, idp)
		idp.setIDToken(signToken(t, idp.key, idp.kid, baseClaims(idp.issuer(), "the-real-nonce")))

		if _, err := a.Exchange(context.Background(), "code", "a-different-nonce"); err == nil {
			t.Fatal("Exchange accepted a token whose nonce did not match")
		}
	})

	// email_verified is not a rejection criterion in oidc.go: the claim is
	// verified as part of the signed token and passed through on Identity, and
	// the server layer (resolveOIDCUser) decides policy. Assert the pass-through
	// so a regression that drops or forces the flag is caught.
	t.Run("passes through unverified email", func(t *testing.T) {
		idp := newMockIdP(t)
		a := newAuthenticator(t, idp)
		claims := baseClaims(idp.issuer(), nonce)
		claims["email_verified"] = false
		idp.setIDToken(signToken(t, idp.key, idp.kid, claims))

		id, err := a.Exchange(context.Background(), "code", nonce)
		if err != nil {
			t.Fatalf("Exchange: unexpected error: %v", err)
		}
		if id.EmailVerified {
			t.Errorf("EmailVerified = true, want false (claim must be preserved)")
		}
	})
}
