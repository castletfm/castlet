package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadSessionKey(t *testing.T) {
	t.Run("too short rejected", func(t *testing.T) {
		t.Setenv("CASTLET_SESSION_KEY", strings.Repeat("x", minSessionKeyLen-1))
		if _, err := Load(nil); err == nil {
			t.Fatal("expected error for short CASTLET_SESSION_KEY, got nil")
		}
	})

	t.Run("minimum length accepted", func(t *testing.T) {
		key := strings.Repeat("x", minSessionKeyLen)
		t.Setenv("CASTLET_SESSION_KEY", key)
		cfg, err := Load(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(cfg.SessionKey) != key {
			t.Fatalf("SessionKey = %q, want %q", cfg.SessionKey, key)
		}
		if cfg.GeneratedKey {
			t.Fatal("GeneratedKey = true, want false for supplied key")
		}
	})

	t.Run("unset generates fallback", func(t *testing.T) {
		t.Setenv("CASTLET_SESSION_KEY", "")
		cfg, err := Load(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.GeneratedKey {
			t.Fatal("GeneratedKey = false, want true when unset")
		}
		if len(cfg.SessionKey) != minSessionKeyLen {
			t.Fatalf("generated key len = %d, want %d", len(cfg.SessionKey), minSessionKeyLen)
		}
	})
}

func TestAllowSignupDefault(t *testing.T) {
	t.Setenv("CASTLET_SESSION_KEY", strings.Repeat("x", minSessionKeyLen))

	t.Run("defaults to disabled", func(t *testing.T) {
		t.Setenv("CASTLET_ALLOW_SIGNUP", "")
		cfg, err := Load(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.AllowSignup {
			t.Fatal("AllowSignup = true, want false by default (closed environments)")
		}
	})

	t.Run("enabled via flag", func(t *testing.T) {
		t.Setenv("CASTLET_ALLOW_SIGNUP", "")
		cfg, err := Load([]string{"--allow-signup"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.AllowSignup {
			t.Fatal("AllowSignup = false, want true when --allow-signup is set")
		}
	})

	t.Run("enabled via env", func(t *testing.T) {
		t.Setenv("CASTLET_ALLOW_SIGNUP", "true")
		cfg, err := Load(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.AllowSignup {
			t.Fatal("AllowSignup = false, want true when CASTLET_ALLOW_SIGNUP=true")
		}
	})
}

// TestOperationalKnobs covers the scalar operational tuning flags: the defaults
// must match the components' built-in behavior, flags and env override, and a
// non-positive value is rejected.
func TestOperationalKnobs(t *testing.T) {
	t.Setenv("CASTLET_SESSION_KEY", strings.Repeat("x", minSessionKeyLen))

	t.Run("defaults preserve current behavior", func(t *testing.T) {
		cfg, err := Load(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ShutdownTimeout != 10*time.Second {
			t.Fatalf("ShutdownTimeout = %v, want 10s", cfg.ShutdownTimeout)
		}
		if cfg.WorkerPollInterval != 5*time.Second {
			t.Fatalf("WorkerPollInterval = %v, want 5s", cfg.WorkerPollInterval)
		}
		if cfg.JobLease != 10*time.Minute {
			t.Fatalf("JobLease = %v, want 10m", cfg.JobLease)
		}
		if cfg.JobMaxAttempts != 5 {
			t.Fatalf("JobMaxAttempts = %d, want 5", cfg.JobMaxAttempts)
		}
		if cfg.MaxUploadBytes != 512<<20 {
			t.Fatalf("MaxUploadBytes = %d, want %d", cfg.MaxUploadBytes, int64(512<<20))
		}
		if cfg.TranscribeTimeoutMin != 5*time.Minute {
			t.Fatalf("TranscribeTimeoutMin = %v, want 5m", cfg.TranscribeTimeoutMin)
		}
	})

	t.Run("overridden via flags", func(t *testing.T) {
		cfg, err := Load([]string{
			"--shutdown-timeout", "20s",
			"--worker-poll-interval", "1s",
			"--job-lease", "30m",
			"--job-max-attempts", "3",
			"--max-upload-bytes", "1048576",
			"--transcribe-timeout-min", "2m",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ShutdownTimeout != 20*time.Second {
			t.Fatalf("ShutdownTimeout = %v, want 20s", cfg.ShutdownTimeout)
		}
		if cfg.WorkerPollInterval != time.Second {
			t.Fatalf("WorkerPollInterval = %v, want 1s", cfg.WorkerPollInterval)
		}
		if cfg.JobLease != 30*time.Minute {
			t.Fatalf("JobLease = %v, want 30m", cfg.JobLease)
		}
		if cfg.JobMaxAttempts != 3 {
			t.Fatalf("JobMaxAttempts = %d, want 3", cfg.JobMaxAttempts)
		}
		if cfg.MaxUploadBytes != 1048576 {
			t.Fatalf("MaxUploadBytes = %d, want 1048576", cfg.MaxUploadBytes)
		}
		if cfg.TranscribeTimeoutMin != 2*time.Minute {
			t.Fatalf("TranscribeTimeoutMin = %v, want 2m", cfg.TranscribeTimeoutMin)
		}
	})

	t.Run("overridden via env", func(t *testing.T) {
		t.Setenv("CASTLET_SHUTDOWN_TIMEOUT", "20s")
		t.Setenv("CASTLET_WORKER_POLL_INTERVAL", "1s")
		t.Setenv("CASTLET_JOB_LEASE", "45m")
		t.Setenv("CASTLET_JOB_MAX_ATTEMPTS", "7")
		t.Setenv("CASTLET_MAX_UPLOAD_BYTES", "2048")
		t.Setenv("CASTLET_TRANSCRIBE_TIMEOUT_MIN", "2m")
		cfg, err := Load(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ShutdownTimeout != 20*time.Second {
			t.Fatalf("ShutdownTimeout = %v, want 20s", cfg.ShutdownTimeout)
		}
		if cfg.WorkerPollInterval != time.Second {
			t.Fatalf("WorkerPollInterval = %v, want 1s", cfg.WorkerPollInterval)
		}
		if cfg.JobLease != 45*time.Minute {
			t.Fatalf("JobLease = %v, want 45m", cfg.JobLease)
		}
		if cfg.JobMaxAttempts != 7 {
			t.Fatalf("JobMaxAttempts = %d, want 7", cfg.JobMaxAttempts)
		}
		if cfg.MaxUploadBytes != 2048 {
			t.Fatalf("MaxUploadBytes = %d, want 2048", cfg.MaxUploadBytes)
		}
		if cfg.TranscribeTimeoutMin != 2*time.Minute {
			t.Fatalf("TranscribeTimeoutMin = %v, want 2m", cfg.TranscribeTimeoutMin)
		}
	})

	t.Run("non-positive values rejected via flag", func(t *testing.T) {
		for _, args := range [][]string{
			{"--shutdown-timeout", "0"},
			{"--worker-poll-interval", "-1s"},
			{"--job-lease", "0"},
			{"--job-max-attempts", "0"},
			{"--max-upload-bytes", "0"},
			{"--transcribe-timeout-min", "-5m"},
		} {
			if _, err := Load(args); err == nil {
				t.Fatalf("expected error for %v, got nil", args)
			}
		}
	})

	// A non-positive env value parses successfully into a zero/negative field,
	// which validation then rejects — exactly like the flag path. (An UNparseable
	// env value is a separate, intentionally lenient case; see the next subtest.)
	t.Run("non-positive values rejected via env", func(t *testing.T) {
		for _, kv := range []struct{ key, val string }{
			{"CASTLET_SHUTDOWN_TIMEOUT", "0"},
			{"CASTLET_WORKER_POLL_INTERVAL", "-1s"},
			{"CASTLET_JOB_LEASE", "0"},
			{"CASTLET_JOB_MAX_ATTEMPTS", "0"},
			{"CASTLET_MAX_UPLOAD_BYTES", "-1"},
			{"CASTLET_TRANSCRIBE_TIMEOUT_MIN", "-5m"},
		} {
			t.Run(kv.key, func(t *testing.T) {
				t.Setenv(kv.key, kv.val)
				if _, err := Load(nil); err == nil {
					t.Fatalf("expected error for %s=%q, got nil", kv.key, kv.val)
				}
			})
		}
	})

	// Unparseable env values fall back to the default and are accepted, matching
	// the existing env-helper behavior (envDuration/envInt/envInt64/envBool all
	// swallow parse errors). This keeps a typo in one env var from failing boot
	// with a surprising message; the default is a safe, positive value.
	t.Run("unparseable env falls back to default", func(t *testing.T) {
		t.Setenv("CASTLET_JOB_MAX_ATTEMPTS", "notanumber")
		t.Setenv("CASTLET_WORKER_POLL_INTERVAL", "notaduration")
		t.Setenv("CASTLET_MAX_UPLOAD_BYTES", "huge")
		cfg, err := Load(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.JobMaxAttempts != 5 {
			t.Fatalf("JobMaxAttempts = %d, want default 5", cfg.JobMaxAttempts)
		}
		if cfg.WorkerPollInterval != 5*time.Second {
			t.Fatalf("WorkerPollInterval = %v, want default 5s", cfg.WorkerPollInterval)
		}
		if cfg.MaxUploadBytes != 512<<20 {
			t.Fatalf("MaxUploadBytes = %d, want default %d", cfg.MaxUploadBytes, int64(512<<20))
		}
	})
}
