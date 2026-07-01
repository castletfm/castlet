package server

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// TestDummyPasswordHashIsUsableBcrypt is a white-box guard that the fallback used
// by the login handler is a genuine bcrypt hash the "unknown email / no local
// password" path can always run a comparison against. If it were empty or
// malformed, bcrypt.CompareHashAndPassword would fail cheaply (before the KDF
// work) and reintroduce the timing side channel this fallback exists to close.
func TestDummyPasswordHashIsUsableBcrypt(t *testing.T) {
	require.NotEmpty(t, dummyPasswordHash, "dummy hash must be precomputed")

	// A valid bcrypt hash exposes its cost; a malformed one errors here.
	cost, err := bcrypt.Cost(dummyPasswordHash)
	require.NoError(t, err, "dummy hash must be a valid bcrypt hash")
	require.Equal(t, bcrypt.DefaultCost, cost, "dummy hash must use the same cost as real hashes")

	// It behaves like a real hash: a wrong guess is rejected via the full
	// comparison, so the fallback path pays the bcrypt cost rather than bailing
	// out early.
	require.Error(t, bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte("not-the-password")))
}
