package app

import (
	"testing"
	"time"

	"github.com/castletfm/castlet/config"
	"github.com/stretchr/testify/require"
)

// TestTuningOptionsZeroConfigUsesComponentDefaults guards against a regression
// where a partially/manually built config.Config (e.g. the user-create path in
// cmd/castlet, which leaves the operational knobs zero) would inject ZERO values
// into the queue/worker/server and clobber their built-in defaults (a zero poll
// interval panics time.NewTicker, a zero max-attempts dead-letters instantly, a
// zero max-upload rejects every upload, etc.). A zero field must forward NO
// option, so each component falls back to its own default.
func TestTuningOptionsZeroConfigUsesComponentDefaults(t *testing.T) {
	zero := &config.Config{}
	require.Empty(t, queueOptions(zero), "zero config must forward no queue options")
	require.Empty(t, workerTuningOptions(zero), "zero config must forward no worker options")
	require.Empty(t, serverTuningOptions(zero), "zero config must forward no server options")
}

// TestTuningOptionsForwardsPositiveValues confirms that explicitly configured
// (positive) knobs are forwarded as options.
func TestTuningOptionsForwardsPositiveValues(t *testing.T) {
	cfg := &config.Config{
		JobLease:           10 * time.Minute,
		JobMaxAttempts:     5,
		WorkerPollInterval: 5 * time.Second,
		MaxUploadBytes:     512 << 20,
		ShutdownTimeout:    10 * time.Second,
	}
	require.Len(t, queueOptions(cfg), 2, "lease and max-attempts should both forward")
	require.Len(t, workerTuningOptions(cfg), 1, "poll interval should forward")
	require.Len(t, serverTuningOptions(cfg), 2, "max-upload and shutdown timeout should both forward")
}
