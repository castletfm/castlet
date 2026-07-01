// Package config loads Castlet's runtime configuration from command-line flags
// with environment-variable (CASTLET_*) fallbacks.
package config

import (
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// minSessionKeyLen is the minimum length, in bytes, required for an
// operator-supplied CASTLET_SESSION_KEY. It matches the size of the generated
// fallback key and the HMAC-SHA256 block-relevant entropy floor.
const minSessionKeyLen = 32

// Config is the resolved configuration for `castlet serve`.
type Config struct {
	Addr     string // listen address, e.g. ":8080"
	BaseURL  string // absolute site root for feed/enclosure URLs
	DataDir  string // holds the sqlite file and the media/ blob root
	SiteName string // shown in the header and page titles

	SessionKey   []byte // HMAC key for session cookies
	GeneratedKey bool   // true when SessionKey was randomly generated this run

	AllowSignup bool // enable the self-service local sign-up path

	// BlobStoreConfig is the path to a JSON file selecting and configuring the
	// media blob store (its "type" field picks the backend). Empty means the
	// local filesystem under DataDir/media.
	BlobStoreConfig string

	// OIDC single sign-on. When OIDCIssuer is empty, OIDC is disabled.
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRedirectURL  string   // defaults to BaseURL + /auth/oidc/callback
	OIDCScopes       []string // defaults to openid, profile, email

	// OIDCAllowedDomains restricts which email domains may sign in via OIDC
	// (linking or just-in-time provisioning). Empty means no domain restriction.
	OIDCAllowedDomains []string

	Transcriber       string   // "null" (default) or "command"
	TranscribeCommand string   // executable for the command transcriber
	TranscribeArgs    []string // argument template; "{{audio}}" is the audio path

	// Per-job transcription timeout. The effective bound scales with the
	// episode's audio length (factor * duration), clamped to a floor
	// (TranscribeTimeoutMin) and to TranscribeTimeout. TranscribeTimeout is also
	// the fallback cap used when an episode's duration is unknown.
	TranscribeTimeout       time.Duration // max wall-clock per job / unknown-length fallback
	TranscribeTimeoutFactor float64       // multiplier on the audio length
	TranscribeTimeoutMin    time.Duration // floor on the derived per-job timeout

	// Operational tuning knobs. Defaults match the components' built-in defaults,
	// so leaving them unset preserves the out-of-the-box behavior.
	ShutdownTimeout    time.Duration // graceful HTTP shutdown drain bound
	WorkerPollInterval time.Duration // how often the worker polls an idle queue
	JobLease           time.Duration // how long a claimed job stays invisible before reclaim
	JobMaxAttempts     int           // attempts before a job is dead-lettered
	MaxUploadBytes     int64         // cap on an episode audio upload, in bytes

	LogLevel string // debug|info|warn|error
}

// Load parses serve flags from args (typically the arguments after the
// subcommand). Defaults come from CASTLET_* environment variables.
func Load(args []string) (*Config, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfg := &Config{}

	fs.StringVar(&cfg.Addr, "addr", env("CASTLET_ADDR", ":8080"), "listen address")
	fs.StringVar(&cfg.BaseURL, "base-url", env("CASTLET_BASE_URL", "http://localhost:8080"), "absolute site root for feed URLs")
	fs.StringVar(&cfg.DataDir, "data-dir", env("CASTLET_DATA_DIR", "./data"), "directory for the database and media files")
	fs.StringVar(&cfg.SiteName, "site-name", env("CASTLET_SITE_NAME", "Castlet"), "site name shown in the UI")
	fs.StringVar(&cfg.Transcriber, "transcriber", env("CASTLET_TRANSCRIBER", "null"), "transcriber backend: null|command")
	fs.StringVar(&cfg.TranscribeCommand, "transcribe-command", env("CASTLET_TRANSCRIBE_COMMAND", ""), "executable for the command transcriber")
	fs.DurationVar(&cfg.TranscribeTimeout, "transcribe-timeout", envDuration("CASTLET_TRANSCRIBE_TIMEOUT", 2*time.Hour), "max wall-clock per transcription job; also the fallback when audio duration is unknown")
	fs.Float64Var(&cfg.TranscribeTimeoutFactor, "transcribe-timeout-factor", envFloat("CASTLET_TRANSCRIBE_TIMEOUT_FACTOR", 1.5), "multiply audio duration by this to derive the per-job timeout (clamped to --transcribe-timeout-min and --transcribe-timeout)")
	fs.DurationVar(&cfg.TranscribeTimeoutMin, "transcribe-timeout-min", envDuration("CASTLET_TRANSCRIBE_TIMEOUT_MIN", 5*time.Minute), "floor on the per-job transcription timeout (covers model spin-up on short clips)")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", envDuration("CASTLET_SHUTDOWN_TIMEOUT", 10*time.Second), "max time to wait for in-flight requests to drain on shutdown")
	fs.DurationVar(&cfg.WorkerPollInterval, "worker-poll-interval", envDuration("CASTLET_WORKER_POLL_INTERVAL", 5*time.Second), "how often the transcription worker polls the queue when idle")
	fs.DurationVar(&cfg.JobLease, "job-lease", envDuration("CASTLET_JOB_LEASE", 10*time.Minute), "how long a claimed job stays invisible before another worker may reclaim it")
	fs.IntVar(&cfg.JobMaxAttempts, "job-max-attempts", envInt("CASTLET_JOB_MAX_ATTEMPTS", 5), "how many times a job is attempted before it is dead-lettered")
	fs.Int64Var(&cfg.MaxUploadBytes, "max-upload-bytes", envInt64("CASTLET_MAX_UPLOAD_BYTES", 512<<20), "maximum episode audio upload size, in bytes")
	fs.StringVar(&cfg.LogLevel, "log-level", env("CASTLET_LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	fs.BoolVar(&cfg.AllowSignup, "allow-signup", envBool("CASTLET_ALLOW_SIGNUP", false), "enable self-service local sign-up (disabled by default)")
	fs.StringVar(&cfg.BlobStoreConfig, "blob-store-config", env("CASTLET_BLOB_STORE_CONFIG", ""), "path to a JSON file configuring the media blob store (default: local filesystem under data-dir)")
	fs.StringVar(&cfg.OIDCIssuer, "oidc-issuer", env("CASTLET_OIDC_ISSUER", ""), "OIDC issuer URL (enables SSO when set)")
	fs.StringVar(&cfg.OIDCClientID, "oidc-client-id", env("CASTLET_OIDC_CLIENT_ID", ""), "OIDC client id")
	fs.StringVar(&cfg.OIDCClientSecret, "oidc-client-secret", env("CASTLET_OIDC_CLIENT_SECRET", ""), "OIDC client secret")
	fs.StringVar(&cfg.OIDCRedirectURL, "oidc-redirect-url", env("CASTLET_OIDC_REDIRECT_URL", ""), "OIDC redirect URL (default base-url + /auth/oidc/callback)")
	args0 := fs.String("transcribe-args", env("CASTLET_TRANSCRIBE_ARGS", "{{audio}}"), "space-separated argument template for the command transcriber")
	scopes := fs.String("oidc-scopes", env("CASTLET_OIDC_SCOPES", "openid profile email"), "space-separated OIDC scopes")
	domains := fs.String("oidc-allowed-domains", env("CASTLET_OIDC_ALLOWED_DOMAINS", ""), "comma-separated email domains allowed to sign in via OIDC (empty allows any)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg.TranscribeArgs = strings.Fields(*args0)
	cfg.OIDCScopes = strings.Fields(*scopes)
	cfg.OIDCAllowedDomains = parseDomains(*domains)
	if cfg.OIDCRedirectURL == "" {
		cfg.OIDCRedirectURL = strings.TrimRight(cfg.BaseURL, "/") + "/auth/oidc/callback"
	}

	// The operational knobs feed component constructors that misbehave on a
	// non-positive value (e.g. time.NewTicker panics), so reject them here rather
	// than let a bad value surface later.
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"transcribe-timeout-min", cfg.TranscribeTimeoutMin > 0},
		{"shutdown-timeout", cfg.ShutdownTimeout > 0},
		{"worker-poll-interval", cfg.WorkerPollInterval > 0},
		{"job-lease", cfg.JobLease > 0},
		{"max-upload-bytes", cfg.MaxUploadBytes > 0},
	} {
		if !c.ok {
			return nil, fmt.Errorf("--%s must be positive", c.name)
		}
	}
	if cfg.JobMaxAttempts < 1 {
		return nil, fmt.Errorf("--job-max-attempts must be at least 1")
	}

	if key := os.Getenv("CASTLET_SESSION_KEY"); key != "" {
		if len(key) < minSessionKeyLen {
			return nil, fmt.Errorf("CASTLET_SESSION_KEY must be at least %d bytes", minSessionKeyLen)
		}
		cfg.SessionKey = []byte(key)
	} else {
		cfg.SessionKey = randomKey()
		cfg.GeneratedKey = true
	}
	return cfg, nil
}

// parseDomains splits a comma-separated domain list, trimming spaces and
// lower-casing each entry so matching is case-insensitive. Empty entries drop.
func parseDomains(s string) []string {
	var out []string
	for d := range strings.SplitSeq(s, ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool reads a boolean env var; unset or unparseable falls back to def.
func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// envDuration reads a duration env var (e.g. "90m"); unset or unparseable
// falls back to def.
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// envInt reads an int env var; unset or unparseable falls back to def.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// envInt64 reads an int64 env var; unset or unparseable falls back to def.
func envInt64(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// envFloat reads a float env var; unset or unparseable falls back to def.
func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func randomKey() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("config: crypto/rand failed: " + err.Error())
	}
	return b
}
