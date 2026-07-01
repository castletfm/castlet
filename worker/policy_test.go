package worker

import (
	"testing"
	"time"
)

// TestJobTimeoutPolicy documents the length-scaled timeout: factor * duration,
// clamped to [Min, Max], with an unknown duration falling back to Max.
func TestJobTimeoutPolicy(t *testing.T) {
	p := JobTimeoutPolicy{Factor: 1.5, Min: 5 * time.Minute, Max: 2 * time.Hour}.withDefaults()

	cases := []struct {
		name        string
		durationSec int
		want        time.Duration
	}{
		{"unknown falls back to max", 0, 2 * time.Hour},
		{"negative falls back to max", -10, 2 * time.Hour},
		{"short clip hits the floor", 60, 5 * time.Minute},             // 1.5*60s=90s < 5m
		{"scales with length", 30 * 60, 45 * time.Minute},              // 1.5*1800s
		{"long episode is capped at max", 10 * 60 * 60, 2 * time.Hour}, // 1.5*10h > 2h
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.timeout(tc.durationSec); got != tc.want {
				t.Fatalf("timeout(%d) = %v, want %v", tc.durationSec, got, tc.want)
			}
		})
	}
}

// TestJobTimeoutPolicyMaxWins verifies Max is a hard ceiling even when it is
// below Min: the floor is applied first, then clamped down to Max, so a small
// configured Max is honored for short known-duration episodes rather than being
// overridden by the (larger) Min default.
func TestJobTimeoutPolicyMaxWins(t *testing.T) {
	p := JobTimeoutPolicy{Factor: 1.5, Min: 5 * time.Minute, Max: time.Minute}.withDefaults()
	if got := p.timeout(60); got != time.Minute { // floor would be 5m, but Max caps it
		t.Fatalf("timeout(60) = %v, want %v", got, time.Minute)
	}
}

// TestJobTimeoutPolicyDefaults verifies zero fields normalize to safe defaults,
// so callers can set only some fields (as the app wiring does for Min).
func TestJobTimeoutPolicyDefaults(t *testing.T) {
	p := JobTimeoutPolicy{}.withDefaults()
	if p.Factor != defaultTimeoutFactor || p.Min != defaultTimeoutMin || p.Max != defaultTimeoutMax {
		t.Fatalf("defaults = %+v, want factor=%v min=%v max=%v", p, defaultTimeoutFactor, defaultTimeoutMin, defaultTimeoutMax)
	}
}

// TestJobTimeoutPolicyMinDefaulting nails down the contract the app wiring relies
// on for TranscribeTimeoutMin (the one knob carried inside the policy struct
// rather than gated by >0 in app): a non-positive Min normalizes to the 5m
// default, while a positive Min is preserved verbatim.
func TestJobTimeoutPolicyMinDefaulting(t *testing.T) {
	cases := []struct {
		name string
		min  time.Duration
		want time.Duration
	}{
		{"zero defaults to 5m", 0, defaultTimeoutMin},
		{"negative defaults to 5m", -1 * time.Minute, defaultTimeoutMin},
		{"positive preserved", 3 * time.Minute, 3 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (JobTimeoutPolicy{Min: tc.min}).withDefaults().Min; got != tc.want {
				t.Fatalf("withDefaults().Min = %v, want %v", got, tc.want)
			}
		})
	}
}
