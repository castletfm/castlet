package dbqueue

// This test targets the unexported defaultBackoff directly, so it lives in the
// internal test package rather than dbqueue_test.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDefaultBackoffSaturates verifies the retry backoff is always a positive
// duration bounded by the 1h cap, even for very large attempt counts where the
// naive time.Minute << attempt would overflow int64 and collapse to a
// zero/negative delay. It also pins the small-attempt schedule so the fix does
// not change existing behavior.
func TestDefaultBackoffSaturates(t *testing.T) {
	const maxBackoff = time.Hour

	// Small attempts match the original exponential schedule (2^attempt minutes,
	// capped at 1h). attempt 6 (64m) is the first value that meets the cap.
	for attempt, want := range map[int]time.Duration{
		0: 1 * time.Minute,
		1: 2 * time.Minute,
		2: 4 * time.Minute,
		3: 8 * time.Minute,
		4: 16 * time.Minute,
		5: 32 * time.Minute,
		6: maxBackoff,
		7: maxBackoff,
	} {
		require.Equalf(t, want, defaultBackoff(attempt), "backoff(%d)", attempt)
	}

	// Across the whole range (well past the int64-overflow point) the backoff is
	// always in (0, maxBackoff], and monotonically non-decreasing up to the cap.
	var prev time.Duration
	for attempt := range 1001 {
		d := defaultBackoff(attempt)
		require.Greaterf(t, d, time.Duration(0), "backoff(%d) must be positive", attempt)
		require.LessOrEqualf(t, d, maxBackoff, "backoff(%d) must not exceed cap", attempt)
		require.GreaterOrEqualf(t, d, prev, "backoff(%d) must be monotonic up to the cap", attempt)
		prev = d
	}
}
