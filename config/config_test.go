package config

import (
	"strings"
	"testing"
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
