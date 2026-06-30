package slug_test

import (
	"testing"

	"github.com/castletfm/castlet/internal/slug"
	"github.com/stretchr/testify/require"
)

func TestMake(t *testing.T) {
	cases := map[string]string{
		"Hello, World!":         "hello-world",
		"  spaced  out  ":       "spaced-out",
		"Episode #1: The Pilot": "episode-1-the-pilot",
		"already-a-slug":        "already-a-slug",
		"under_score.dot/slash": "under-score-dot-slash",
		"café crème":            "caf-crme",
		"!!!":                   "",
		"":                      "",
	}
	for in, want := range cases {
		require.Equalf(t, want, slug.Make(in), "input %q", in)
	}
}
