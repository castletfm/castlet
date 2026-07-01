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

	Transcriber       string   // "null" (default) or "command"
	TranscribeCommand string   // executable for the command transcriber
	TranscribeArgs    []string // argument template; "{{audio}}" is the audio path

	// Per-job transcription timeout. The effective bound scales with the
	// episode's audio length (factor * duration), clamped to a floor and to
	// TranscribeTimeout. TranscribeTimeout is also the fallback cap used when an
	// episode's duration is unknown.
	TranscribeTimeout       time.Duration // max wall-clock per job / unknown-length fallback
	TranscribeTimeoutFactor float64       // multiplier on the audio length

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
	fs.Float64Var(&cfg.TranscribeTimeoutFactor, "transcribe-timeout-factor", envFloat("CASTLET_TRANSCRIBE_TIMEOUT_FACTOR", 1.5), "multiply audio duration by this to derive the per-job timeout (clamped to a floor and --transcribe-timeout)")
	fs.StringVar(&cfg.LogLevel, "log-level", env("CASTLET_LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	fs.BoolVar(&cfg.AllowSignup, "allow-signup", envBool("CASTLET_ALLOW_SIGNUP", true), "enable self-service local sign-up")
	fs.StringVar(&cfg.BlobStoreConfig, "blob-store-config", env("CASTLET_BLOB_STORE_CONFIG", ""), "path to a JSON file configuring the media blob store (default: local filesystem under data-dir)")
	fs.StringVar(&cfg.OIDCIssuer, "oidc-issuer", env("CASTLET_OIDC_ISSUER", ""), "OIDC issuer URL (enables SSO when set)")
	fs.StringVar(&cfg.OIDCClientID, "oidc-client-id", env("CASTLET_OIDC_CLIENT_ID", ""), "OIDC client id")
	fs.StringVar(&cfg.OIDCClientSecret, "oidc-client-secret", env("CASTLET_OIDC_CLIENT_SECRET", ""), "OIDC client secret")
	fs.StringVar(&cfg.OIDCRedirectURL, "oidc-redirect-url", env("CASTLET_OIDC_REDIRECT_URL", ""), "OIDC redirect URL (default base-url + /auth/oidc/callback)")
	args0 := fs.String("transcribe-args", env("CASTLET_TRANSCRIBE_ARGS", "{{audio}}"), "space-separated argument template for the command transcriber")
	scopes := fs.String("oidc-scopes", env("CASTLET_OIDC_SCOPES", "openid profile email"), "space-separated OIDC scopes")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg.TranscribeArgs = strings.Fields(*args0)
	cfg.OIDCScopes = strings.Fields(*scopes)
	if cfg.OIDCRedirectURL == "" {
		cfg.OIDCRedirectURL = strings.TrimRight(cfg.BaseURL, "/") + "/auth/oidc/callback"
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
