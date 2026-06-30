// Package config loads Castlet's runtime configuration from command-line flags
// with environment-variable (CASTLET_*) fallbacks.
package config

import (
	"crypto/rand"
	"flag"
	"os"
	"strings"
)

// Config is the resolved configuration for `castlet serve`.
type Config struct {
	Addr     string // listen address, e.g. ":8080"
	BaseURL  string // absolute site root for feed/enclosure URLs
	DataDir  string // holds the sqlite file and the media/ blob root
	SiteName string // shown in the header and page titles

	SessionKey   []byte // HMAC key for session cookies
	GeneratedKey bool   // true when SessionKey was randomly generated this run

	Transcriber       string   // "null" (default) or "command"
	TranscribeCommand string   // executable for the command transcriber
	TranscribeArgs    []string // argument template; "{{audio}}" is the audio path

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
	fs.StringVar(&cfg.LogLevel, "log-level", env("CASTLET_LOG_LEVEL", "info"), "log level: debug|info|warn|error")
	args0 := fs.String("transcribe-args", env("CASTLET_TRANSCRIBE_ARGS", "{{audio}}"), "space-separated argument template for the command transcriber")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg.TranscribeArgs = strings.Fields(*args0)

	if key := os.Getenv("CASTLET_SESSION_KEY"); key != "" {
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

func randomKey() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("config: crypto/rand failed: " + err.Error())
	}
	return b
}
