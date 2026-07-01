package email_test

import (
	"testing"

	"github.com/castletfm/castlet/internal/email"
	"github.com/stretchr/testify/require"
)

func TestCanonical(t *testing.T) {
	t.Run("valid addresses are lower-cased and stripped", func(t *testing.T) {
		cases := map[string]string{
			"alice@example.com":      "alice@example.com",
			"Alice@Example.com":      "alice@example.com",
			"ALICE@EXAMPLE.COM":      "alice@example.com",
			"  Alice@Example.Com  ":  "alice@example.com", // surrounding whitespace
			"Bob <Bob@Example.com>":  "bob@example.com",   // display name stripped
			"MixedLocal@Example.COM": "mixedlocal@example.com",
		}
		for in, want := range cases {
			got, err := email.Canonical(in)
			require.NoError(t, err, "input %q", in)
			require.Equal(t, want, got, "input %q", in)
		}
	})

	t.Run("case-only variants share one canonical form", func(t *testing.T) {
		a, err := email.Canonical("Alice@Example.com")
		require.NoError(t, err)
		b, err := email.Canonical("alice@example.com")
		require.NoError(t, err)
		require.Equal(t, a, b)
	})

	t.Run("malformed input is rejected", func(t *testing.T) {
		for _, in := range []string{"", "   ", "not-an-email", "@example.com", "alice@", "a b@example.com"} {
			_, err := email.Canonical(in)
			require.Error(t, err, "input %q should be rejected", in)
		}
	})
}
